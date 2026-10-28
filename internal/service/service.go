// Package service runs the proxy in the background: start spawns a detached
// copy of this binary, records its PID and address, and waits for it to bind;
// stop and status find it again through that record.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const (
	startTimeout = 15 * time.Second
	stopTimeout  = 5 * time.Second
)

// Record is what start leaves behind in proxy.json.
type Record struct {
	PID int `json:"pid"`
	// Exe is the binary start launched. A PID counts as ours only while it
	// still runs that binary, so a recycled PID is never signalled.
	Exe  string `json:"exe,omitempty"`
	Host string `json:"host"`
	Port int    `json:"port"`
	URL  string `json:"url"`
}

// Dir is where the record and the background log live:
// COMFY_API_PROXY_STATE_DIR (the reference's override), else
// $XDG_STATE_HOME/comfy-api-proxy, else ~/.local/state/comfy-api-proxy.
func Dir() (string, error) {
	if d := os.Getenv("COMFY_API_PROXY_STATE_DIR"); d != "" {
		return d, nil
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "comfy-api-proxy"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "comfy-api-proxy"), nil
}

func paths() (record, log string, err error) {
	dir, err := Dir()
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("create %s: %w", dir, err)
	}
	return filepath.Join(dir, "proxy.json"), filepath.Join(dir, "proxy.log"), nil
}

// LogPath is the background process's log file.
func LogPath() (string, error) {
	_, log, err := paths()
	return log, err
}

// Read returns the saved record. A missing, corrupt, or oddly shaped record
// reads as nil: status and stop treat it as "not running" instead of failing.
func Read() (*Record, error) {
	path, _, err := paths()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var rec Record
	if json.Unmarshal(raw, &rec) != nil || rec.PID <= 0 {
		return nil, nil
	}
	return &rec, nil
}

func clearRecord() error {
	path, _, err := paths()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// Running returns the live record, or nil when nothing of ours is running.
func Running() (*Record, error) {
	rec, err := Read()
	if err != nil || rec == nil {
		return nil, err
	}
	if !isOurs(rec) {
		return nil, nil
	}
	return rec, nil
}

// ConnectHost turns a bind address into one a client can dial.
func ConnectHost(ip net.IP) net.IP {
	if ip.IsUnspecified() {
		if ip.To4() != nil {
			return net.IPv4(127, 0, 0, 1)
		}
		return net.IPv6loopback
	}
	return ip
}

// URL is the base URL for a bind address.
func URL(ip net.IP, port int) string {
	return "http://" + net.JoinHostPort(ConnectHost(ip).String(), strconv.Itoa(port))
}

func portOpen(ip net.IP, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ConnectHost(ip).String(), strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	// The probe already succeeded; a failed close of it changes nothing.
	if cerr := conn.Close(); cerr != nil {
		return true
	}
	return true
}

// AlreadyRunningError means start found a live proxy of ours.
type AlreadyRunningError struct{ Rec Record }

func (e *AlreadyRunningError) Error() string {
	return fmt.Sprintf("comfy-api-proxy is already running at %s (pid %d)", e.Rec.URL, e.Rec.PID)
}

// PortBusyError means start found the port held by something else.
type PortBusyError struct{ URL string }

func (e *PortBusyError) Error() string {
	return fmt.Sprintf("comfy-api-proxy cannot start: %s is already in use.\n"+
		"Stop the other process or choose a different port (--port, COMFY_PROXY_PORT, or port in the config file).", e.URL)
}

// Start launches this binary with "run" plus args, detached from the
// terminal, and waits for it to listen on ip:port.
func Start(args []string, host string, ip net.IP, port int) (Record, string, error) {
	if rec, err := Running(); err != nil {
		return Record{}, "", err
	} else if rec != nil {
		return Record{}, "", &AlreadyRunningError{*rec}
	}
	url := URL(ip, port)
	// An occupied port would make the readiness check below pass while the
	// child fails to bind and exits, so refuse up front.
	if portOpen(ip, port, 500*time.Millisecond) {
		return Record{}, "", &PortBusyError{URL: url}
	}

	recordPath, logPath, err := paths()
	if err != nil {
		return Record{}, "", err
	}
	exe, err := os.Executable()
	if err != nil {
		return Record{}, "", fmt.Errorf("find own executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Record{}, "", fmt.Errorf("open log %s: %w", logPath, err)
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return Record{}, "", errors.Join(fmt.Errorf("open %s: %w", os.DevNull, err), log.Close())
	}
	cmd := exec.Command(exe, append([]string{"run"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, log, log
	cmd.SysProcAttr = detached()
	startErr := cmd.Start()
	if err := errors.Join(startErr, log.Close(), devnull.Close()); err != nil {
		return Record{}, "", fmt.Errorf("start background proxy: %w", err)
	}

	// The child's own log says why it exited; the status goes in the message.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(startTimeout)
	for !portOpen(ip, port, time.Second) {
		select {
		case exitErr := <-exited:
			reason := "it exited"
			if exitErr != nil {
				reason = exitErr.Error()
			}
			return Record{}, logPath, fmt.Errorf("comfy-api-proxy failed to start (%s)\nThe log at %s says why", reason, logPath)
		default:
		}
		if time.Now().After(deadline) {
			return Record{}, logPath, errors.Join(
				fmt.Errorf("comfy-api-proxy did not start listening within %s; see %s", startTimeout, logPath),
				terminate(cmd.Process.Pid))
		}
		time.Sleep(200 * time.Millisecond)
	}

	rec := Record{PID: cmd.Process.Pid, Exe: exe, Host: host, Port: port, URL: url}
	raw, err := json.Marshal(rec)
	if err != nil {
		return Record{}, logPath, fmt.Errorf("encode record: %w", err)
	}
	if err := os.WriteFile(recordPath, raw, 0o600); err != nil {
		return Record{}, logPath, errors.Join(fmt.Errorf("write %s: %w", recordPath, err), terminate(rec.PID))
	}
	return rec, logPath, nil
}

// Stop ends the recorded proxy. It reports the PID it stopped, or 0 when
// nothing of ours was running, which is not an error.
func Stop() (int, error) {
	rec, err := Read()
	if err != nil {
		return 0, err
	}
	if rec == nil || !isOurs(rec) {
		return 0, clearRecord()
	}
	if err := terminate(rec.PID); err != nil {
		return 0, err
	}
	return rec.PID, clearRecord()
}

// isOurs reports whether rec.PID is alive and still running the binary
// start launched (sameBinary says how each platform checks). When the
// binary cannot be read it falls back to liveness, which is no worse than
// tracking the PID alone.
func isOurs(rec *Record) bool {
	if !alive(rec.PID) {
		return false
	}
	want := rec.Exe
	if want == "" {
		self, err := os.Executable()
		if err != nil {
			return true
		}
		want = self
	}
	if resolved, err := filepath.EvalSymlinks(want); err == nil {
		want = resolved
	}
	return sameBinary(rec.PID, want)
}

// Health is what GET /api/v2/health reported.
type Health struct {
	Status   string `json:"status"`
	Upstream string `json:"upstream"`
}

// Probe asks a running proxy for its health.
func Probe(ctx context.Context, baseURL string) (Health, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v2/health", nil)
	if err != nil {
		return Health{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Health{}, err
	}
	var h Health
	decodeErr := json.NewDecoder(resp.Body).Decode(&h)
	if err := errors.Join(decodeErr, resp.Body.Close()); err != nil {
		return Health{}, fmt.Errorf("read health: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return h, fmt.Errorf("health returned %d", resp.StatusCode)
	}
	return h, nil
}
