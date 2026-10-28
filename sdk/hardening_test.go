package sdk

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A redirect that keeps the hostname but changes port must not carry the
// token: Go's own rule compares hostnames only.
func TestBearerDroppedOnRedirectToAnotherPort(t *testing.T) {
	isolateEnv(t)
	var leaked string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		writeJSON(t, w, 200, map[string]any{"id": "j", "status": "succeeded", "outputs": []any{}})
	}))
	t.Cleanup(other.Close)
	s := newStub(t)
	s.routes["GET /api/v2/jobs/j"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/api/v2/jobs/j", http.StatusFound)
	}
	if _, err := s.client(t, WithAPIKey("tok")).GetJob(context.Background(), "j"); err != nil {
		t.Fatal(err)
	}
	if leaked != "" {
		t.Fatalf("token followed a redirect to another port on the same host: %q", leaked)
	}
}

func TestRedactUserinfo(t *testing.T) {
	cases := map[string]string{
		"https://user:p@ss@h.example/x":   "https://***@h.example/x",
		"https://user:s%65cret@h.example": "https://***@h.example",
		"http://u:pw@127.0.0.1:8189":      "http://***@127.0.0.1:8189",
		"http://127.0.0.1:8189/a@b":       "http://127.0.0.1:8189/a@b",
		"http://h.example?x=a@b":          "http://h.example?x=a@b",
	}
	for in, want := range cases {
		if got := redactUserinfo(in); got != want {
			t.Errorf("redactUserinfo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServerLinksKeepBaseUserinfo(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	var auth string
	s.routes["GET /api/v2/jobs/job-1"] = func(w http.ResponseWriter, r *http.Request) {
		user, pass, _ := r.BasicAuth()
		auth = user + ":" + pass
		writeJSON(t, w, 200, s.job("job-1", StatusSucceeded))
	}
	u, err := url.Parse(s.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(WithBaseURL("http://bob:pw@" + u.Host))
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{t: c.t, w: wireJob{ID: "job-1", URLs: jobURLs{Self: "/api/v2/jobs/job-1"}}}
	if err := job.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if auth != "bob:pw" {
		t.Fatalf("server link lost basic auth: %q", auth)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A slow consumer between reads is not an idle server.
func TestIdleClockRunsOnlyInsideRead(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	c := s.client(t, WithTimeout(100*time.Millisecond))
	job, err := c.Run(context.Background(), simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	slow := writerFunc(func(p []byte) (int, error) {
		time.Sleep(250 * time.Millisecond)
		return len(p), nil
	})
	if _, err := job.Outputs()[0].ToWriter(context.Background(), slow, nil); err != nil {
		t.Fatalf("a slow writer tripped the idle timeout: %v", err)
	}
}

// A server that stops sending is an ErrIdleTimeout, not a caller cancel.
func TestIdleStallIsNamed(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	s.routes["GET /api/v2/assets/out-1/content"] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		if _, err := w.Write([]byte("par")); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		<-release
	}
	c := s.client(t, WithTimeout(100*time.Millisecond))
	job, err := c.Run(context.Background(), simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = job.Outputs()[0].Bytes(context.Background(), nil)
	if !errors.Is(err, ErrIdleTimeout) || errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestWriteFailureIsNotATransportError(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	job, err := s.client(t).Run(context.Background(), simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	full := errors.New("no space left on device")
	_, err = job.Outputs()[0].ToWriter(context.Background(), writerFunc(func([]byte) (int, error) { return 0, full }), nil)
	var te *TransportError
	if !errors.Is(err, full) || errors.As(err, &te) {
		t.Fatalf("got %v", err)
	}
}

func TestTypedAssetCollectionsAreSubstituted(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	c := s.client(t)
	a, b := c.AssetFromBytes([]byte("a"), "a.png", ""), c.AssetFromBytes([]byte("b"), "b.png", "")
	wf := simpleWorkflow()
	wf.SetInput("5", "images", []*Asset{a, b})
	wf.SetInput("6", "named", map[string]*Asset{"x": a})
	if _, err := c.Submit(context.Background(), wf, SubmitOptions{}); err != nil {
		t.Fatal(err)
	}
	graph := submitBody(t, s)["workflow"].(map[string]any)
	list := graph["5"].(map[string]any)["inputs"].(map[string]any)["images"].([]any)
	named := graph["6"].(map[string]any)["inputs"].(map[string]any)["named"].(map[string]any)
	for _, ref := range []any{list[0], list[1], named["x"]} {
		if m, ok := ref.(map[string]any); !ok || m["__type"] != "core/ASSET" {
			t.Fatalf("not substituted: %v", ref)
		}
	}
}

func TestMalformedOutputEventIsAnError(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.events = []string{frames(`event: output` + "\n" + `data: {"node_id": 5}`)}
	job, err := s.client(t).Submit(context.Background(), simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for _, err := range job.Events(context.Background()) {
		got = err
	}
	if !errors.Is(got, ErrInvalidResponse) {
		t.Fatalf("got %v", got)
	}
}

func TestSSELineEndings(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader("a\rb\r\nc\nd\r"))
	sc.Split(scanSSELines)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if strings.Join(lines, "|") != "a|b|c|d" {
		t.Fatalf("lines = %q", lines)
	}
}

func TestHugeRetryAfterSaturates(t *testing.T) {
	for _, raw := range []string{"99999999999", "999999999999999999999999"} {
		ra := retryAfter(raw)
		if ra == nil || *ra <= 0 {
			t.Fatalf("Retry-After %s = %v", raw, ra)
		}
		delay, ok := retryDelay(&APIError{HTTPStatus: 429, RetryAfter: ra}, time.Now().Add(time.Minute))
		if !ok || delay < 59*time.Second {
			t.Fatalf("Retry-After %s waited %s, want the rest of the budget", raw, delay)
		}
	}
}

func TestUploadRefusesHeaderInjection(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	_, err := s.client(t).AssetFromBytes([]byte("x"), "a\r\nContent-Type: text/html", "").Commit(context.Background())
	if err == nil || len(s.recorded("POST", "/api/v2/assets")) != 0 {
		t.Fatalf("injected filename uploaded: %v", err)
	}
}

func TestAssetFromURLErrorHidesSecrets(t *testing.T) {
	isolateEnv(t)
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	t.Cleanup(gone.Close)
	u, err := url.Parse(gone.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := newStub(t)
	_, err = s.client(t).AssetFromURL(context.Background(), "http://user:pw@"+u.Host+"/img.png?X-Goog-Signature=abc")
	if err == nil || strings.Contains(err.Error(), "pw") || strings.Contains(err.Error(), "Signature") {
		t.Fatalf("got %v", err)
	}
}

func TestCommitWaitHonoursContext(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	a := s.client(t).AssetFromBytes([]byte("x"), "x.bin", "")
	if err := a.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := a.Commit(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a waiting commit ignored its context: %v", err)
	}
	if a.ID() != "" {
		t.Fatal("state changed while the commit waited")
	}
	a.release()
}
