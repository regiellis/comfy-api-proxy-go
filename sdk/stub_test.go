package sdk

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stub is an in-process stand-in for a Comfy API v2 deployment, the Go
// counterpart of the Python SDK's conftest server. Tests set fields to shape
// its answers and read the recorded requests back.
type stub struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	requests []recorded
	// jobStatus is what GET /jobs/{id} reports; it advances through the
	// slice one poll at a time and then holds the last entry.
	jobStatus []string
	polls     int
	// submitErrors are returned, in order, before a submit succeeds.
	submitErrors []stubError
	usedKeys     map[string]bool
	hashKnown    bool
	events       []string // raw SSE frames for /events, one per connection
	eventsConns  int
	routes       map[string]http.HandlerFunc
}

type recorded struct {
	Method, Path string
	Header       http.Header
	Body         []byte
}

type stubError struct {
	status     int
	code       string
	retryAfter string
}

func newStub(t *testing.T) *stub {
	t.Helper()
	s := &stub{t: t, jobStatus: []string{StatusSucceeded}, usedKeys: map[string]bool{}, routes: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stub) client(t *testing.T, opts ...Option) *Client {
	t.Helper()
	c, err := New(append([]Option{WithBaseURL(s.srv.URL)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (s *stub) recorded(method, path string) []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recorded
	for _, r := range s.requests {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Error(err)
	}
}

func envelope(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message, "details": nil}}
}

func (s *stub) job(id, status string) map[string]any {
	base := s.srv.URL
	return map[string]any{
		"id": id, "status": status,
		"created_at": "2026-10-17T10:00:00.000000+00:00", "expires_at": "2026-10-18T10:00:00.000000+00:00",
		"started_at": nil, "completed_at": nil, "queue_position": nil, "progress": nil, "error": nil,
		"outputs": []any{map[string]any{
			"node_id": "9", "name": "out.png", "type": "image", "content_type": "image/png", "size_bytes": 4,
			"id": "out-1", "hash": nil, "url": base + "/api/v2/assets/out-1/content",
			"url_expires_at": "2026-10-18T10:00:00.000000+00:00", "job_id": id,
		}},
		"urls": map[string]any{
			"self":   base + "/api/v2/jobs/" + id,
			"events": base + "/api/v2/jobs/" + id + "/events",
			"cancel": base + "/api/v2/jobs/" + id + "/cancel",
		},
	}
}

func (s *stub) asset(id string, createdNew bool) map[string]any {
	return map[string]any{
		"id": id, "hash": "blake3:abc", "size_bytes": 4, "content_type": "image/png", "file_path": "photo.png",
		"created_new": createdNew, "created_at": "2026-10-17T10:00:00.000000+00:00",
		"url": s.srv.URL + "/api/v2/assets/" + id + "/content", "url_expires_at": "2026-10-18T10:00:00.000000+00:00",
		"expires_at": "2026-11-01T00:00:00.000000+00:00",
	}
}

func (s *stub) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Error(err)
	}
	s.mu.Lock()
	s.requests = append(s.requests, recorded{r.Method, r.URL.Path, r.Header.Clone(), body})
	route := s.routes[r.Method+" "+r.URL.Path]
	s.mu.Unlock()
	if route != nil {
		route(w, r)
		return
	}

	p := strings.TrimPrefix(r.URL.Path, "/api/v2")
	switch {
	case r.Method == http.MethodPost && p == "/jobs":
		s.mu.Lock()
		if len(s.submitErrors) > 0 {
			e := s.submitErrors[0]
			s.submitErrors = s.submitErrors[1:]
			s.mu.Unlock()
			if e.retryAfter != "" {
				w.Header().Set("Retry-After", e.retryAfter)
			}
			writeJSON(s.t, w, e.status, envelope(e.code, e.code))
			return
		}
		key := r.Header.Get("Idempotency-Key")
		reused := s.usedKeys[key]
		s.usedKeys[key] = true
		s.mu.Unlock()
		if reused {
			writeJSON(s.t, w, 422, envelope("idempotency_key_reuse", "key reused"))
			return
		}
		writeJSON(s.t, w, 201, s.job("job-1", StatusQueued))
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/events"):
		s.mu.Lock()
		frame := ""
		if s.eventsConns < len(s.events) {
			frame = s.events[s.eventsConns]
		}
		s.eventsConns++
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, frame); err != nil {
			s.t.Error(err)
		}
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/jobs/"):
		id := strings.TrimPrefix(p, "/jobs/")
		if id == "missing" {
			writeJSON(s.t, w, 404, envelope("not_found", "No job missing."))
			return
		}
		s.mu.Lock()
		status := s.jobStatus[min(s.polls, len(s.jobStatus)-1)]
		s.polls++
		s.mu.Unlock()
		job := s.job(id, status)
		if status == StatusFailed {
			job["error"] = map[string]any{"code": "node_execution_error", "message": "boom", "node_id": "3", "class_type": "KSampler", "traceback": nil}
		}
		writeJSON(s.t, w, 200, job)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/cancel"):
		writeJSON(s.t, w, 200, s.job("job-1", StatusCanceling))
	case r.Method == http.MethodHead && strings.HasPrefix(p, "/assets/by-hash/"):
		s.mu.Lock()
		known := s.hashKnown
		s.mu.Unlock()
		if known {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(404)
		}
	case r.Method == http.MethodPost && p == "/assets/from-hash":
		writeJSON(s.t, w, 200, s.asset("asset-dedup", false))
	case r.Method == http.MethodPost && p == "/assets":
		writeJSON(s.t, w, 201, s.asset("asset-new", true))
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/assets/"):
		w.WriteHeader(204)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/content"):
		data := []byte("\x89PNG")
		if rng := r.Header.Get("Range"); rng != "" {
			parts := strings.SplitN(strings.TrimPrefix(rng, "bytes="), "-", 2)
			a, errA := strconv.Atoi(parts[0])
			b, errB := strconv.Atoi(parts[len(parts)-1])
			if errA != nil || errB != nil || len(parts) != 2 {
				s.t.Errorf("bad Range %q", rng)
				return
			}
			w.Header().Set("Content-Range", "bytes "+parts[0]+"-"+parts[1]+"/4")
			w.WriteHeader(206)
			if _, err := w.Write(data[a : b+1]); err != nil {
				s.t.Error(err)
			}
			return
		}
		w.Header().Set("Content-Type", "image/png")
		if _, err := w.Write(data); err != nil {
			s.t.Error(err)
		}
	default:
		writeJSON(s.t, w, 404, envelope("not_found", "no route "+r.Method+" "+r.URL.Path))
	}
}

// fastRetry shrinks the retry budget and the minimum retry delay for a test,
// scaling seconds of Retry-After down to milliseconds.
func fastRetry(t *testing.T, budget time.Duration) {
	t.Helper()
	oldBudget, oldMin, oldUnit := retryBudget, minRetryDelay, retryAfterUnit
	retryBudget, minRetryDelay, retryAfterUnit = budget, time.Millisecond, time.Millisecond
	t.Cleanup(func() { retryBudget, minRetryDelay, retryAfterUnit = oldBudget, oldMin, oldUnit })
}
