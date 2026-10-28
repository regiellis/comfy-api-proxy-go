package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fastPolicy retries quickly for tests.
var fastPolicy = RetryPolicy{
	MaxElapsed: 2 * time.Second, InitialBackoff: time.Millisecond, BackoffFactor: 2, MaxBackoff: 5 * time.Millisecond,
	RetryCollectable: true, CollectMaxElapsed: 2 * time.Second,
}

func routerClient(t *testing.T, s *stub, p RetryPolicy, opts ...Option) *Client {
	t.Helper()
	fastRetry(t, time.Second)
	return s.client(t, append([]Option{WithRouterBaseURL(s.srv.URL), WithRetry(p), WithAPIKey("tok")}, opts...)...)
}

const runPath = "/v2/models/acme/painter"

func TestParseModelAndRequestIDs(t *testing.T) {
	if p, m, err := ParseModelID("acme/painter"); err != nil || p != "acme" || m != "painter" {
		t.Fatalf("got %s %s %v", p, m, err)
	}
	for _, bad := range []string{"", "acme", "acme/", "/painter", "a/b/c", "a/b/c/d", "../x", "acme/.."} {
		if _, _, err := ParseModelID(bad); err == nil {
			t.Errorf("model id %q accepted", bad)
		}
	}
	for _, bad := range []string{"", "a/b", ".", "..", strings.Repeat("r", 257), "a\nb"} {
		if err := ParseRequestID(bad); err == nil {
			t.Errorf("request id %q accepted", bad)
		}
	}
	if err := ParseRequestID("req_123"); err != nil {
		t.Fatal(err)
	}
}

func TestRunJSONAndDisclosures(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.routes["POST "+runPath] = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Idempotency-Key") == "" {
			t.Errorf("headers: %v", r.Header)
		}
		if r.URL.Query().Get("model_provider") != "other" || r.URL.Query().Get("strict_mode") != "true" {
			t.Errorf("query: %s", r.URL.RawQuery)
		}
		w.Header().Set("Idempotent-Replayed", "true")
		w.Header().Set("X-Comfy-Router-Dropped-Params", `["seed: unsupported"]`)
		w.Header().Set("X-Comfy-Router-Fallback-Provider", "backup")
		w.Header().Set("X-Comfy-Credits-Used", "0.00")
		writeJSON(t, w, 200, map[string]any{"images": []any{"https://x/1.png"}})
	}
	strict := true
	r, err := routerClient(t, s, fastPolicy).Models.RunDetailed(context.Background(), "acme/painter",
		map[string]any{"prompt": "a cube"}, RunOptions{ModelProvider: "other", StrictMode: &strict})
	if err != nil {
		t.Fatal(err)
	}
	if r.Output.JSON["images"] == nil || !r.Replayed || r.DroppedParams[0] != "seed: unsupported" ||
		*r.ServingProvider != "backup" || *r.CreditsUsed != "0.00" {
		t.Fatalf("result = %+v", r)
	}
	var body map[string]any
	if err := json.Unmarshal(s.recorded("POST", runPath)[0].Body, &body); err != nil || body["prompt"] != "a cube" {
		t.Fatalf("body = %v %v", body, err)
	}
}

func TestRunBinaryAndOddDisclosures(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.routes["POST "+runPath] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("X-Comfy-Router-Dropped-Params", "not json")
		w.Header().Set("X-Comfy-Credits-Used", "NaN")
		if _, err := w.Write([]byte("\x89PNG")); err != nil {
			t.Error(err)
		}
	}
	r, err := routerClient(t, s, fastPolicy).Models.RunDetailed(context.Background(), "acme/painter", nil, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Output.Binary == nil || string(r.Output.Binary.Content) != "\x89PNG" || r.Output.Binary.ContentType != "image/png" {
		t.Fatalf("binary = %+v", r.Output)
	}
	if r.DroppedParams[0] != "not json" || r.CreditsUsed != nil || r.Replayed {
		t.Fatalf("disclosures = %+v", r)
	}
}

func failingRun(t *testing.T, s *stub, replies []func(http.ResponseWriter)) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	keys := map[string]bool{}
	s.routes["POST "+runPath] = func(w http.ResponseWriter, r *http.Request) {
		keys[r.Header.Get("Idempotency-Key")] = true
		if len(keys) != 1 {
			t.Errorf("retry used a new idempotency key: %v", keys)
		}
		i := int(n.Add(1)) - 1
		if i < len(replies) {
			replies[i](w)
			return
		}
		writeJSON(t, w, 200, map[string]any{"ok": true})
	}
	return &n
}

func reply(t *testing.T, status int, bucket, retryAfter string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		if bucket != "" {
			w.Header().Set("X-Comfy-Error-Type", bucket)
		}
		writeJSON(t, w, status, map[string]any{"detail": bucket})
	}
}

func TestRunRetryRules(t *testing.T) {
	isolateEnv(t)
	inFlight := fastPolicy
	inFlight.RetryPossiblyInFlight = true
	cases := []struct {
		name    string
		policy  RetryPolicy
		replies []func(http.ResponseWriter)
		retried bool
	}{
		{"429 with Retry-After", fastPolicy, []func(http.ResponseWriter){reply(t, 429, "rate_limited", "1")}, true},
		{"429 without Retry-After surfaces", fastPolicy, []func(http.ResponseWriter){reply(t, 429, "rate_limited", "")}, false},
		{"503 is not retried by default (billed)", fastPolicy, []func(http.ResponseWriter){reply(t, 503, "service_unavailable", "")}, false},
		{"503 retried when in-flight retries are allowed", inFlight, []func(http.ResponseWriter){reply(t, 503, "service_unavailable", "")}, true},
		{"409 still running is collected", fastPolicy, []func(http.ResponseWriter){reply(t, 409, "concurrency_limit_exceeded", "1")}, true},
		{"504 deadline is collected", fastPolicy, []func(http.ResponseWriter){reply(t, 504, "deadline_exceeded", "1")}, true},
		{"409 without Retry-After surfaces", fastPolicy, []func(http.ResponseWriter){reply(t, 409, "concurrency_limit_exceeded", "")}, false},
		{"NoRetry makes one attempt", NoRetry, []func(http.ResponseWriter){reply(t, 429, "rate_limited", "1")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStub(t)
			n := failingRun(t, s, tc.replies)
			_, err := routerClient(t, s, tc.policy).Models.Run(context.Background(), "acme/painter", nil, RunOptions{IdempotencyKey: "k1"})
			if tc.retried && (err != nil || n.Load() != 2) {
				t.Fatalf("want a retry: err=%v attempts=%d", err, n.Load())
			}
			if !tc.retried {
				var api *APIError
				if err == nil || n.Load() != 1 || !errors.As(err, &api) || api.IdempotencyKey != "k1" {
					t.Fatalf("want one attempt and a keyed error: err=%v attempts=%d", err, n.Load())
				}
			}
		})
	}
}

func TestRunResendRefused(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	inFlight := fastPolicy
	inFlight.RetryPossiblyInFlight = true
	failingRun(t, s, []func(http.ResponseWriter){
		reply(t, 502, "provider_error", ""),
		func(w http.ResponseWriter) { writeJSON(t, w, 422, envelope("idempotency_key_reuse", "reused")) },
	})
	_, err := routerClient(t, s, inFlight).Models.Run(context.Background(), "acme/painter", nil, RunOptions{})
	var api *APIError
	if !errors.As(err, &api) || !api.ResendRefused || !errors.Is(err, ErrProviderError) || api.IdempotencyKey == "" {
		t.Fatalf("want the earlier provider error with ResendRefused: %v", err)
	}
}

func TestRouterErrorBuckets(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.routes["POST "+runPath] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Comfy-Error-Type", "content_policy_violation")
		w.Header().Set("X-Comfy-Request-Id", "rq-9")
		writeJSON(t, w, 400, map[string]any{"detail": []any{map[string]any{"loc": []any{"body", "prompt"}, "msg": "refused", "type": "policy"}}})
	}
	_, err := routerClient(t, s, fastPolicy).Models.Run(context.Background(), "acme/painter", nil, RunOptions{})
	var api *APIError
	if !errors.Is(err, ErrContentPolicyViolation) || !errors.Is(err, ErrRouter) || !errors.As(err, &api) {
		t.Fatalf("got %v", err)
	}
	if api.RequestID != "rq-9" || len(api.Validation) != 1 || api.Validation[0].Location() != "body.prompt" || api.Message != "body.prompt: refused" {
		t.Fatalf("error = %+v", api)
	}
	already := &APIError{Code: "already_completed", ErrorType: "already_completed"}
	if !errors.Is(already, ErrAlreadyCompleted) || !errors.Is(already, ErrCancelRefused) || !errors.Is(already, ErrRouter) {
		t.Fatal("already_completed should be a cancel refusal and a router error")
	}
	if errors.Is(&APIError{Code: "not_found"}, ErrRouter) {
		t.Fatal("a plain v2 not_found is not a router error")
	}
}

func queueRoutes(t *testing.T, s *stub, statuses []map[string]any, result func(http.ResponseWriter)) {
	t.Helper()
	var polls atomic.Int32
	s.routes["POST "+runPath+"/requests"] = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 202, map[string]any{"request_id": "req_1", "status": "IN_QUEUE"})
	}
	s.routes["GET "+runPath+"/requests/req_1/status"] = func(w http.ResponseWriter, _ *http.Request) {
		i := int(polls.Add(1)) - 1
		writeJSON(t, w, 200, statuses[min(i, len(statuses)-1)])
	}
	s.routes["GET "+runPath+"/requests/req_1"] = func(w http.ResponseWriter, _ *http.Request) { result(w) }
	s.routes["PUT "+runPath+"/requests/req_1/cancel"] = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 202, map[string]any{"status": "CANCELLATION_REQUESTED"})
	}
}

func TestSubmitUpdatesGet(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	queueRoutes(t, s, []map[string]any{
		{"status": "IN_QUEUE", "queue_position": 2},
		{"status": "IN_QUEUE", "queue_position": 2},
		{"status": "IN_PROGRESS"},
		{"status": "COMPLETED"},
	}, func(w http.ResponseWriter) { writeJSON(t, w, 200, map[string]any{"video": "https://x/v.mp4"}) })
	c := routerClient(t, s, fastPolicy)
	h, err := c.Models.Submit(context.Background(), "acme/painter", map[string]any{"prompt": "x"}, RunOptions{})
	if err != nil || h.RequestID() != "req_1" {
		t.Fatalf("submit = %v, %v", h, err)
	}
	var seen []string
	out, err := c.Models.Subscribe(context.Background(), "acme/painter", nil, RunOptions{}, func(u QueueUpdate) { seen = append(seen, u.Status) })
	if err != nil || out.JSON["video"] != "https://x/v.mp4" {
		t.Fatalf("subscribe = %v, %v", out, err)
	}
	if strings.Join(seen, ",") != "IN_QUEUE,IN_PROGRESS,COMPLETED" {
		t.Fatalf("updates = %v (a repeated state must not be yielded twice)", seen)
	}
	u, err := h.Cancel(context.Background())
	if err != nil || u.Status != "CANCELLATION_REQUESTED" {
		t.Fatalf("cancel = %+v, %v", u, err)
	}
}

func TestCompletionWithErrorTypeIsTyped(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	queueRoutes(t, s, []map[string]any{{"status": "COMPLETED", "error_type": "provider_timeout", "detail": "took too long"}},
		func(w http.ResponseWriter) { t.Error("a failed completion must not fetch a result") })
	h, err := routerClient(t, s, fastPolicy).Models.Handle("acme/painter", "req_1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Get(context.Background())
	if !errors.Is(err, ErrProviderTimeout) || !strings.Contains(err.Error(), "took too long") {
		t.Fatalf("got %v", err)
	}
}

func TestSubscribeCancelsWhenContextEnds(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	queueRoutes(t, s, []map[string]any{{"status": "IN_PROGRESS"}}, func(http.ResponseWriter) {})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := routerClient(t, s, fastPolicy).Models.Subscribe(ctx, "acme/painter", nil, RunOptions{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if len(s.recorded("PUT", runPath+"/requests/req_1/cancel")) != 1 {
		t.Fatal("an abandoned subscribe left the generation running")
	}
}

func TestCatalogWalkAndValidation(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.routes["GET /v2/models"] = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") == "" {
			writeJSON(t, w, 200, map[string]any{"data": []any{
				map[string]any{"id": "acme/painter", "provider": "acme", "model": "painter", "billing": map[string]any{"charges_on_policy_rejection": true}},
			}, "has_more": true, "next_cursor": "c2", "limit": 1})
			return
		}
		writeJSON(t, w, 200, map[string]any{"data": []any{map[string]any{"id": "beta/x", "provider": "beta", "model": "x"}}, "has_more": false})
	}
	var ids []string
	for m, err := range routerClient(t, s, fastPolicy).Models.List(context.Background(), ListModelsOptions{Limit: 1}) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
		if m.ID == "acme/painter" && m.Billing["charges_on_policy_rejection"] != true {
			t.Fatalf("billing = %v", m.Billing)
		}
	}
	if strings.Join(ids, ",") != "acme/painter,beta/x" {
		t.Fatalf("ids = %v", ids)
	}

	for name, page := range map[string]map[string]any{
		"mismatched id":       {"data": []any{map[string]any{"id": "a/b", "provider": "a", "model": "c"}}, "has_more": false},
		"missing has_more":    {"data": []any{}},
		"has_more, no cursor": {"data": []any{}, "has_more": true},
		"looping cursor":      {"data": []any{}, "has_more": true, "next_cursor": "loop"},
	} {
		t.Run(name, func(t *testing.T) {
			s := newStub(t)
			s.routes["GET /v2/models"] = func(w http.ResponseWriter, _ *http.Request) { writeJSON(t, w, 200, page) }
			var last error
			for _, err := range routerClient(t, s, fastPolicy).Models.List(context.Background(), ListModelsOptions{Cursor: "loop"}) {
				if err != nil {
					last = err
				}
			}
			if !errors.Is(last, ErrInvalidResponse) {
				t.Fatalf("got %v", last)
			}
		})
	}
}

func TestSchemaETag(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.routes["GET "+runPath+"/openapi.json"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		writeJSON(t, w, 200, map[string]any{"openapi": "3.1.0"})
	}
	m := routerClient(t, s, fastPolicy).Models
	first, err := m.Schema(context.Background(), "acme/painter", "")
	if err != nil || first.Unchanged || first.Document["openapi"] != "3.1.0" || first.ETag != `"v1"` {
		t.Fatalf("first = %+v, %v", first, err)
	}
	again, err := m.Schema(context.Background(), "acme/painter", first.ETag)
	if err != nil || !again.Unchanged || again.Document != nil || again.ETag != `"v1"` {
		t.Fatalf("again = %+v, %v", again, err)
	}
}

func TestReadsRetryInFlightFailures(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	var n atomic.Int32
	s.routes["GET /v2/models"] = func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			writeJSON(t, w, 503, map[string]any{"detail": "blip"})
			return
		}
		writeJSON(t, w, 200, map[string]any{"data": []any{}, "has_more": false})
	}
	if _, err := routerClient(t, s, fastPolicy).Models.Page(context.Background(), ListModelsOptions{}); err != nil || n.Load() != 2 {
		t.Fatalf("a catalog read was not retried after a 503: %v (%d attempts)", err, n.Load())
	}
}

func TestRouterKeyScoping(t *testing.T) {
	isolateEnv(t)
	c, err := New(WithBaseURL("http://127.0.0.1:8189"), WithRouterBaseURL("https://api.comfy.org"), WithAPIKey("tok"))
	if err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]bool{
		"https://api.comfy.org/v2/models":      true,
		"http://127.0.0.1:8189/api/v2/jobs":    true,
		"http://api.comfy.org/v2/models":       false,
		"https://evil.api.comfy.org/v2/models": false,
		"https://storage.example.invalid/x":    false,
	} {
		if got := c.t.credentialed(target); got != want {
			t.Errorf("credentialed(%s) = %v, want %v", target, got, want)
		}
	}
	if c.Models.BaseURL() != "https://api.comfy.org" {
		t.Fatalf("router base = %s", c.Models.BaseURL())
	}
}

func TestRetryPolicyRules(t *testing.T) {
	if err := (RetryPolicy{InitialBackoff: time.Second, BackoffFactor: 2, MaxBackoff: time.Millisecond}).Validate(); err == nil {
		t.Error("MaxBackoff below InitialBackoff accepted")
	}
	if err := DefaultRetry.Validate(); err != nil {
		t.Fatal(err)
	}
	p := DefaultRetry
	p.Jitter = false
	for attempt, want := range map[int]time.Duration{1: 500 * time.Millisecond, 2: time.Second, 3: 2 * time.Second, 10: 15 * time.Second} {
		if got := p.backoff(attempt); got != want {
			t.Errorf("backoff(%d) = %s, want %s", attempt, got, want)
		}
	}
	dial := &TransportError{Err: &net.OpError{Op: "dial", Err: errors.New("refused")}}
	read := &TransportError{Err: io.ErrUnexpectedEOF}
	if !DefaultRetry.shouldRetry(dial) || mayHaveClaimedKey(dial) {
		t.Error("a refused connection never reached the server: retry it, and it claimed nothing")
	}
	if DefaultRetry.shouldRetry(read) || !mayHaveClaimedKey(read) {
		t.Error("a dropped read may have run: no retry by default, and it may have claimed the key")
	}
}
