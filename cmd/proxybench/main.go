// Command proxybench measures the Go proxy against the Python reference on
// this machine: throughput and latency per endpoint, startup time, and
// memory, with both proxies in front of the same upstream.
//
// The upstream is a Go stand-in for ComfyUI by default (-upstream stub), so
// the numbers are the proxies' own cost rather than the Python fake's. With
// -upstream ref it is the reference's demo/fake_comfyui.py, which is how the
// README's first numbers were taken.
//
//	task bench                                   # the defaults below
//	task bench -- -runs 5 -duration 10s -out .cache/bench/report.md
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type options struct {
	goBin      string
	refBin     string
	refPython  string
	refFake    string
	upstream   string
	proxyCPUs  string
	runs       int
	duration   time.Duration
	uploadMB   int
	uploads    int
	uploadConc int
	out        string
	targets    string
}

func main() {
	var o options
	flag.StringVar(&o.goBin, "go", "bin/comfy-api-proxy", "Go proxy binary")
	flag.StringVar(&o.refBin, "ref", ".cache/ref-venv/bin/comfy-api-proxy", "reference proxy entry point")
	flag.StringVar(&o.refPython, "ref-python", ".cache/ref-venv/bin/python", "Python for the reference fake ComfyUI")
	flag.StringVar(&o.refFake, "ref-fake", "proxy-ref-ts/demo/fake_comfyui.py", "the reference fake ComfyUI")
	flag.StringVar(&o.upstream, "upstream", "stub", "upstream ComfyUI: stub (Go, in process) or ref (the Python fake)")
	flag.StringVar(&o.proxyCPUs, "proxy-cpus", "0-7", "CPU list the proxies are pinned to with taskset (empty: no pinning)")
	flag.IntVar(&o.runs, "runs", 3, "runs per scenario; the median is reported")
	flag.DurationVar(&o.duration, "duration", 5*time.Second, "length of each timed run")
	flag.IntVar(&o.uploadMB, "upload-mb", 64, "size of each upload")
	flag.IntVar(&o.uploads, "uploads", 16, "uploads per upload run")
	flag.IntVar(&o.uploadConc, "upload-concurrency", 4, "uploads in flight at once")
	flag.StringVar(&o.out, "out", "", "also write the Markdown report here")
	flag.StringVar(&o.targets, "targets", "go,go-1cpu,ref", "targets to measure, comma separated")
	flag.Parse()

	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "proxybench:", err)
		os.Exit(1)
	}
}

// target is one proxy configuration under test.
type target struct {
	name string
	bin  string
	env  []string
}

// result is one scenario on one target, medians across runs.
type result struct {
	Target   string  `json:"target"`
	Scenario string  `json:"scenario"`
	Conc     int     `json:"concurrency"`
	RPS      float64 `json:"rps"`
	MBps     float64 `json:"mb_per_s,omitempty"`
	P50      float64 `json:"p50_ms"`
	P99      float64 `json:"p99_ms"`
	Errors   int64   `json:"errors"`
	PeakRSS  int64   `json:"peak_rss_kb"`
	EndRSS   int64   `json:"end_rss_kb"`
}

// startup is a target's cold start and idle memory.
type startup struct {
	Target  string  `json:"target"`
	ReadyMS float64 `json:"ready_ms"`
	IdleRSS int64   `json:"idle_rss_kb"`
}

func run(o options) error {
	if runtime.GOOS != "linux" {
		return errors.New("memory figures read /proc, so proxybench runs on Linux only")
	}
	upURL, stopUp, err := startUpstream(o)
	if err != nil {
		return err
	}
	defer stopUp()

	all := map[string]target{
		"go":      {name: "go", bin: o.goBin},
		"go-1cpu": {name: "go-1cpu", bin: o.goBin, env: []string{"GOMAXPROCS=1"}},
		"ref":     {name: "ref", bin: o.refBin},
	}
	var (
		results  []result
		startups []startup
	)
	for name := range strings.SplitSeq(o.targets, ",") {
		t, ok := all[strings.TrimSpace(name)]
		if !ok {
			return fmt.Errorf("unknown target %q (want go, go-1cpu, ref)", name)
		}
		fmt.Fprintf(os.Stderr, "== %s\n", t.name)
		rs, st, err := measure(o, t, upURL)
		if err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
		results = append(results, rs...)
		startups = append(startups, st)
	}

	report := render(o, results, startups)
	fmt.Print(report)
	if o.out != "" {
		if err := os.MkdirAll(filepath.Dir(o.out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(o.out, []byte(report), 0o644); err != nil {
			return err
		}
		raw, err := json.MarshalIndent(map[string]any{"results": results, "startup": startups}, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(strings.TrimSuffix(o.out, filepath.Ext(o.out))+".json", raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// measure runs every scenario on t. Each run gets a fresh proxy process, so a
// run's peak memory is its own and not what earlier runs left behind (a
// submit run stores every job it creates).
func measure(o options, t target, upURL string) ([]result, startup, error) {
	st := startup{Target: t.name}
	var readies []float64
	idle := func(p *proxy) error {
		readies = append(readies, p.ready.Seconds()*1000)
		if st.IdleRSS != 0 {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
		var err error
		st.IdleRSS, err = p.rss("VmRSS")
		return err
	}

	type scenario struct {
		name string
		conc int
		// run measures one fresh proxy at base.
		run func(base string, i int) (result, error)
	}
	load := func(conc int, req func(base string) func(*http.Client, int64) (int64, error)) func(string, int) (result, error) {
		return func(base string, _ int) (result, error) {
			return timed(conc, o.duration, req(base)), nil
		}
	}
	scenarios := []scenario{
		{"health", 32, load(32, func(base string) func(*http.Client, int64) (int64, error) {
			return func(c *http.Client, _ int64) (int64, error) {
				return 0, expect(c, http.MethodGet, base+"/api/v2/health", nil, "", http.StatusOK)
			}
		})},
		{"job read", 16, func(base string, _ int) (result, error) {
			id, err := finishedJob(base)
			if err != nil {
				return result{}, err
			}
			return timed(16, o.duration, func(c *http.Client, _ int64) (int64, error) {
				return 0, expect(c, http.MethodGet, base+"/api/v2/jobs/"+id, nil, "", http.StatusOK)
			}), nil
		}},
		{"submit", 16, load(16, func(base string) func(*http.Client, int64) (int64, error) {
			return func(c *http.Client, _ int64) (int64, error) {
				return 0, expect(c, http.MethodPost, base+"/api/v2/jobs", bytes.NewReader(workflowBody), "application/json", http.StatusOK, http.StatusCreated, http.StatusAccepted)
			}
		})},
	}
	// The reference fake reads uploads with aiohttp's request.post(), whose
	// 1 MB body limit fails any real upload with a 500 through either proxy.
	if o.upstream == "stub" {
		scenarios = append(scenarios, scenario{fmt.Sprintf("upload %d MB", o.uploadMB), o.uploadConc, func(base string, i int) (result, error) {
			return uploads(base, o, i)
		}})
	}

	var out []result
	for _, s := range scenarios {
		rs := make([]result, 0, o.runs)
		for i := range o.runs {
			p, err := startProxy(o, t, upURL)
			if err != nil {
				return nil, st, err
			}
			r, err := func() (result, error) {
				defer p.stop()
				if err := idle(p); err != nil {
					return result{}, err
				}
				r, err := s.run(p.base, i)
				if err != nil {
					return r, err
				}
				if r.PeakRSS, err = p.rss("VmHWM"); err != nil {
					return r, err
				}
				r.EndRSS, err = p.rss("VmRSS")
				return r, err
			}()
			if err != nil {
				return nil, st, fmt.Errorf("%s: %w", s.name, err)
			}
			rs = append(rs, r)
		}
		m := median(rs)
		m.Target, m.Scenario, m.Conc = t.name, s.name, s.conc
		if m.MBps > 0 {
			fmt.Fprintf(os.Stderr, "   %-12s %8.0f MB/s   p50 %7.2f ms  peak RSS %4d MB  errors %d\n", s.name, m.MBps, m.P50, m.PeakRSS/1024, m.Errors)
		} else {
			fmt.Fprintf(os.Stderr, "   %-12s %8.0f req/s  p50 %7.2f ms  p99 %6.2f ms  peak RSS %4d MB  errors %d\n", s.name, m.RPS, m.P50, m.P99, m.PeakRSS/1024, m.Errors)
		}
		out = append(out, m)
	}
	slices.Sort(readies)
	st.ReadyMS = readies[len(readies)/2]
	return out, st, nil
}

var workflowBody = []byte(`{"workflow":{"3":{"class_type":"KSampler","inputs":{"seed":42,"steps":1}},"9":{"class_type":"SaveImage","inputs":{"images":["3",0]}}}}`)

func client(conc int) *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        conc,
			MaxIdleConnsPerHost: conc,
			MaxConnsPerHost:     conc,
			DisableCompression:  true,
		},
	}
}

// expect sends one request, drains the body so the connection is reused, and
// fails on any status not listed.
func expect(c *http.Client, method, url string, body io.Reader, contentType string, ok ...int) error {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(io.Discard, resp.Body)
	closeErr := resp.Body.Close()
	if !slices.Contains(ok, resp.StatusCode) {
		return fmt.Errorf("%s %s: status %d", method, url, resp.StatusCode)
	}
	return errors.Join(copyErr, closeErr)
}

// timed runs conc closed-loop workers for d and returns requests per second
// and latency percentiles. Errors are counted, not fatal, and reported.
func timed(conc int, d time.Duration, req func(*http.Client, int64) (int64, error)) result {
	c := client(conc)
	defer c.CloseIdleConnections()
	var (
		errs  atomic.Int64
		seq   atomic.Int64
		mu    sync.Mutex
		lats  []time.Duration
		wg    sync.WaitGroup
		start = time.Now()
		end   = start.Add(d)
	)
	for range conc {
		wg.Go(func() {
			local := make([]time.Duration, 0, 1<<16)
			for time.Now().Before(end) {
				t0 := time.Now()
				if _, err := req(c, seq.Add(1)); err != nil {
					errs.Add(1)
					continue
				}
				local = append(local, time.Since(t0))
			}
			mu.Lock()
			lats = append(lats, local...)
			mu.Unlock()
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	slices.Sort(lats)
	return result{
		RPS:    float64(len(lats)) / elapsed.Seconds(),
		P50:    pct(lats, 0.50),
		P99:    pct(lats, 0.99),
		Errors: errs.Load(),
	}
}

func pct(sorted []time.Duration, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return float64(sorted[int(q*float64(len(sorted)-1))].Microseconds()) / 1000
}

// uploads posts o.uploads files of o.uploadMB each, o.uploadConc at a time.
// Each file's first bytes differ, so no upload is answered from the hash index.
func uploads(base string, o options, runIdx int) (result, error) {
	size := o.uploadMB << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	c := client(o.uploadConc)
	defer c.CloseIdleConnections()
	var (
		errs  atomic.Int64
		first sync.Once
		next  atomic.Int64
		mu    sync.Mutex
		lats  []time.Duration
		wg    sync.WaitGroup
		start = time.Now()
	)
	for range o.uploadConc {
		wg.Go(func() {
			for {
				n := next.Add(1)
				if n > int64(o.uploads) {
					return
				}
				t0 := time.Now()
				if err := upload(c, base, payload, fmt.Sprintf("bench-%d-%d-%d", time.Now().UnixNano(), runIdx, n)); err != nil {
					first.Do(func() { fmt.Fprintln(os.Stderr, "   upload:", err) })
					errs.Add(1)
					continue
				}
				mu.Lock()
				lats = append(lats, time.Since(t0))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	slices.Sort(lats)
	return result{
		RPS:    float64(len(lats)) / elapsed.Seconds(),
		MBps:   float64(len(lats)*o.uploadMB) / elapsed.Seconds(),
		P50:    pct(lats, 0.50),
		P99:    pct(lats, 0.99),
		Errors: errs.Load(),
	}, nil
}

// upload streams one multipart body through a pipe, so the generator never
// holds a second copy of the payload.
func upload(c *http.Client, base string, payload []byte, name string) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		pw.CloseWithError(writeUpload(mw, payload, name))
	}()
	return expect(c, http.MethodPost, base+"/api/v2/assets", pr, mw.FormDataContentType(), http.StatusOK, http.StatusCreated)
}

func writeUpload(mw *multipart.Writer, payload []byte, name string) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s.png"`, name))
	h.Set("Content-Type", "image/png")
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := part.Write([]byte(name)); err != nil {
		return err
	}
	if _, err := part.Write(payload[len(name):]); err != nil {
		return err
	}
	if err := mw.WriteField("file_path", "input/"+name+".png"); err != nil {
		return err
	}
	if err := mw.WriteField("content_type", "image/png"); err != nil {
		return err
	}
	return mw.Close()
}

// finishedJob submits one job and waits until it reads as succeeded, so the
// job read scenario measures a terminal job.
func finishedJob(base string) (string, error) {
	c := client(1)
	defer c.CloseIdleConnections()
	resp, err := c.Post(base+"/api/v2/jobs", "application/json", bytes.NewReader(workflowBody))
	if err != nil {
		return "", err
	}
	var job struct {
		ID string `json:"id"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&job)
	if err := errors.Join(decodeErr, resp.Body.Close()); err != nil {
		return "", fmt.Errorf("submit: %w", err)
	}
	if job.ID == "" {
		return "", fmt.Errorf("submit: status %d and no job id", resp.StatusCode)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := c.Get(base + "/api/v2/jobs/" + job.ID)
		if err != nil {
			return "", err
		}
		var st struct {
			Status string `json:"status"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&st)
		if err := errors.Join(decodeErr, resp.Body.Close()); err != nil {
			return "", err
		}
		if st.Status == "succeeded" || st.Status == "completed" {
			return job.ID, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return "", fmt.Errorf("job %s did not finish in 10 s", job.ID)
}

func median(rs []result) result {
	pick := func(f func(result) float64) float64 {
		v := make([]float64, len(rs))
		for i, r := range rs {
			v[i] = f(r)
		}
		slices.Sort(v)
		return v[len(v)/2]
	}
	var errs, peak, end int64
	for _, r := range rs {
		errs += r.Errors
		peak = max(peak, r.PeakRSS)
		end = max(end, r.EndRSS)
	}
	return result{
		RPS:     pick(func(r result) float64 { return r.RPS }),
		MBps:    pick(func(r result) float64 { return r.MBps }),
		P50:     pick(func(r result) float64 { return r.P50 }),
		P99:     pick(func(r result) float64 { return r.P99 }),
		Errors:  errs,
		PeakRSS: peak,
		EndRSS:  end,
	}
}

type proxy struct {
	cmd   *exec.Cmd
	base  string
	ready time.Duration
}

func startProxy(o options, t target, upURL string) (*proxy, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	args := []string{"--comfyui", upURL, "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--max-upload-mb", strconv.Itoa(o.uploadMB * 2)}
	name, argv := t.bin, args
	if o.proxyCPUs != "" {
		name, argv = "taskset", append([]string{"-c", o.proxyCPUs, t.bin}, args...)
	}
	cmd := exec.Command(name, argv...)
	cmd.Env = append(os.Environ(), t.env...)
	if err := os.MkdirAll(".cache/bench", 0o755); err != nil {
		return nil, err
	}
	logf, err := os.CreateTemp(".cache/bench", t.name+"-*.log")
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, logf.Close())
	}
	p := &proxy{cmd: cmd, base: fmt.Sprintf("http://127.0.0.1:%d", port)}
	if err := waitHealthy(p.base, 15*time.Second); err != nil {
		p.stop()
		return nil, fmt.Errorf("%w (log: %s)", err, logf.Name())
	}
	p.ready = time.Since(start)
	return p, logf.Close()
}

func (p *proxy) stop() {
	if p.cmd.Process == nil {
		return
	}
	// Kill is the fallback when the interrupt fails; either way Wait reaps it.
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		fmt.Fprintln(os.Stderr, "   interrupt:", err)
	}
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		if kerr := p.cmd.Process.Kill(); kerr != nil {
			fmt.Fprintln(os.Stderr, "   kill:", kerr)
		}
		err = <-done
	}
	// An exit status after the signal is expected; anything else is worth seeing.
	if _, exited := errors.AsType[*exec.ExitError](err); err != nil && !exited {
		fmt.Fprintln(os.Stderr, "   wait:", err)
	}
}

// rss reads a memory field of the proxy process in kB from /proc.
func (p *proxy) rss(field string) (int64, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", p.cmd.Process.Pid))
	if err != nil {
		return 0, err
	}
	defer closeQuietly(f)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), field+":"); ok {
			return strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(v), " kB"), 10, 64)
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("no %s in /proc/%d/status", field, p.cmd.Process.Pid)
}

// closeQuietly closes a read-only file, where a close error loses nothing.
func closeQuietly(f *os.File) {
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "   close:", err)
	}
}

func waitHealthy(base string, within time.Duration) error {
	c := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := c.Get(base + "/api/v2/health")
		if err == nil {
			closeErr := resp.Body.Close()
			if resp.StatusCode == http.StatusOK && closeErr == nil {
				return nil
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("%s not healthy within %s", base, within)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	return port, l.Close()
}

// startUpstream starts the ComfyUI the proxies talk to.
func startUpstream(o options) (string, func(), error) {
	switch o.upstream {
	case "stub":
		l, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return "", nil, err
		}
		srv := &http.Server{Handler: stubComfy(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintln(os.Stderr, "stub upstream:", err)
			}
		}()
		stop := func() {
			if err := srv.Shutdown(context.Background()); err != nil {
				fmt.Fprintln(os.Stderr, "stub upstream:", err)
			}
		}
		return "http://" + l.Addr().String(), stop, nil
	case "ref":
		port, err := freePort()
		if err != nil {
			return "", nil, err
		}
		cmd := exec.Command(o.refPython, o.refFake, "--port", strconv.Itoa(port))
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Start(); err != nil {
			return "", nil, err
		}
		stop := func() {
			if err := cmd.Process.Kill(); err != nil {
				fmt.Fprintln(os.Stderr, "fake upstream:", err)
			}
			if err := cmd.Wait(); err != nil {
				fmt.Fprintln(os.Stderr, "fake upstream exited:", err)
			}
		}
		url := fmt.Sprintf("http://127.0.0.1:%d", port)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
			if err == nil {
				return url, stop, conn.Close()
			}
			time.Sleep(20 * time.Millisecond)
		}
		stop()
		return "", nil, errors.New("the reference fake ComfyUI did not start")
	default:
		return "", nil, fmt.Errorf("-upstream %q: want stub or ref", o.upstream)
	}
}

// stubComfy answers the native endpoints the proxies call, with no work
// behind them: every job finishes as soon as it is queued.
func stubComfy() http.Handler {
	var (
		mu   sync.RWMutex
		jobs = map[string]bool{}
	)
	history := []byte(`{"outputs":{"9":{"images":[{"filename":"out.png","subfolder":"","type":"output"}]}},"status":{"status_str":"success","completed":true,"messages":[["execution_cached",{"nodes":[]}]]}}`)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /prompt", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			PromptID string `json:"prompt_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PromptID == "" {
			http.Error(w, `{"error":{"type":"invalid"},"node_errors":{}}`, http.StatusBadRequest)
			return
		}
		mu.Lock()
		jobs[body.PromptID] = true
		mu.Unlock()
		writeJSON(w, fmt.Appendf(nil, `{"prompt_id":%q,"number":1,"node_errors":{}}`, body.PromptID))
	})
	mux.HandleFunc("GET /history/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		mu.RLock()
		ok := jobs[id]
		mu.RUnlock()
		if !ok {
			writeJSON(w, []byte(`{}`))
			return
		}
		writeJSON(w, fmt.Appendf(nil, `{%q:%s}`, id, history))
	})
	mux.HandleFunc("GET /queue", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []byte(`{"queue_running":[],"queue_pending":[]}`))
	})
	// ComfyUI answers HEAD /view with the file's length; the proxy sizes
	// outputs this way once per output.
	mux.HandleFunc("GET /view", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "69")
		if r.Method == http.MethodHead {
			return
		}
		// A failed write means the proxy hung up; the stub has nothing to recover.
		if _, err := w.Write(make([]byte, 69)); err != nil {
			return
		}
	})
	mux.HandleFunc("POST /upload/image", func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, []byte(`{"name":"upload.png","subfolder":"","type":"input"}`))
	})
	mux.HandleFunc("POST /api/jobs/{id}/cancel", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []byte(`{"cancelled":false}`))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	// A failed write means the proxy hung up; the stub has nothing to recover.
	if _, err := w.Write(b); err != nil {
		return
	}
}

func render(o options, results []result, startups []startup) string {
	var b strings.Builder
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	fmt.Fprintf(&b, "# Proxy benchmark\n\n")
	fmt.Fprintf(&b, "%s on %s, %s/%s. Upstream: %s. Proxies pinned to CPUs %s; the load generator%s on the %d CPUs it was given. ",
		time.Now().Format("2006-01-02 15:04"), host, runtime.GOOS, runtime.GOARCH, o.upstream, o.proxyCPUs, map[bool]string{true: " and the stub upstream run", false: " runs"}[o.upstream == "stub"], runtime.NumCPU())
	fmt.Fprintf(&b, "Median of %d runs of %s each, every run on a fresh proxy process; peak RSS is the highest of the runs. ", o.runs, o.duration)
	if o.upstream == "stub" {
		fmt.Fprintf(&b, "Uploads: %d files of %d MB, %d at a time, per run. ", o.uploads, o.uploadMB, o.uploadConc)
	} else {
		b.WriteString("No upload runs: the reference fake caps request bodies at 1 MB. ")
	}
	fmt.Fprintf(&b, "`go-1cpu` is the Go proxy with GOMAXPROCS=1, ")
	fmt.Fprintf(&b, "a per-core comparison with the single-threaded reference.\n\n")
	b.WriteString("| Scenario | Target | Concurrency | req/s | MB/s | p50 ms | p99 ms | Errors | Peak RSS MB | RSS after MB |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	order := []string{}
	for _, r := range results {
		if !slices.Contains(order, r.Scenario) {
			order = append(order, r.Scenario)
		}
	}
	for _, s := range order {
		for _, r := range results {
			if r.Scenario != s {
				continue
			}
			mbps := "-"
			if r.MBps > 0 {
				mbps = fmt.Sprintf("%.0f", r.MBps)
			}
			fmt.Fprintf(&b, "| %s | %s | %d | %.0f | %s | %.2f | %.2f | %d | %.0f | %.0f |\n",
				r.Scenario, r.Target, r.Conc, r.RPS, mbps, r.P50, r.P99, r.Errors, float64(r.PeakRSS)/1024, float64(r.EndRSS)/1024)
		}
	}
	b.WriteString("\n| Target | Ready after start, ms (median) | Idle RSS MB |\n|---|---:|---:|\n")
	for _, s := range startups {
		fmt.Fprintf(&b, "| %s | %.0f | %.0f |\n", s.Target, s.ReadyMS, float64(s.IdleRSS)/1024)
	}
	return b.String()
}
