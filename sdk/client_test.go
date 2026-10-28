package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func isolateEnv(t *testing.T) {
	t.Helper()
	t.Setenv(BaseURLEnv, "")
	t.Setenv(APIKeyEnv, "")
}

func TestKeyResolution(t *testing.T) {
	isolateEnv(t)
	if _, err := New(); !errors.Is(err, ErrMissingAPIKey) || !strings.Contains(err.Error(), APIKeyEnv) {
		t.Fatalf("cloud without a key: %v", err)
	}
	for _, spelling := range []string{"https://cloud.comfy.org:443/", "HTTPS://CLOUD.comfy.org"} {
		if _, err := New(WithBaseURL(spelling)); !errors.Is(err, ErrMissingAPIKey) {
			t.Errorf("%s still needs a key: %v", spelling, err)
		}
	}
	for _, keyless := range []string{"http://127.0.0.1:8189", "https://cloud.comfy.org/self-hosted"} {
		c, err := New(WithBaseURL(keyless))
		if err != nil || c.Authenticated() {
			t.Errorf("%s should be keyless: %v", keyless, err)
		}
	}

	t.Setenv(APIKeyEnv, "  env-key \n")
	c, err := New()
	if err != nil || c.t.apiKey != "env-key" {
		t.Fatalf("env key not stripped and used: %v", err)
	}
	c, err = New(WithAPIKey("explicit"))
	if err != nil || c.t.apiKey != "explicit" {
		t.Fatal("explicit key does not win over the environment")
	}
	c, err = New(WithAPIKey("   "))
	if err != nil || c.t.apiKey != "env-key" {
		t.Fatal("a blank explicit key should fall through to the environment")
	}
}

func TestBaseURLFromEnvAndValidation(t *testing.T) {
	isolateEnv(t)
	t.Setenv(BaseURLEnv, "http://127.0.0.1:8189/")
	c, err := New()
	if err != nil || c.BaseURL() != "http://127.0.0.1:8189" {
		t.Fatalf("env base url: %v %q", err, c.BaseURL())
	}
	for _, bad := range []string{"ftp://x", "http://h?q=1", "http://h#f", "http://h:99999", "not a url"} {
		if _, err := New(WithBaseURL(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestKeyNeverRendered(t *testing.T) {
	isolateEnv(t)
	c, err := New(WithBaseURL("http://user:pw@127.0.0.1:8189"), WithAPIKey("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	s := c.String()
	if strings.Contains(s, "s3cret") || strings.Contains(s, "pw") || !strings.Contains(s, "***@127.0.0.1:8189") || !strings.Contains(s, "authenticated=true") {
		t.Fatalf("String() = %s", s)
	}
}

func TestBearerOnlyToOwnOrigin(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	c := s.client(t, WithAPIKey("tok"))
	if _, err := c.GetJob(context.Background(), "job-1"); err != nil {
		t.Fatal(err)
	}
	if got := s.recorded("GET", "/api/v2/jobs/job-1")[0].Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("own origin Authorization = %q", got)
	}
	req, err := c.t.request(context.Background(), "GET", "https://elsewhere.example/x", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("bearer sent to a third host")
	}
}

func submitBody(t *testing.T, s *stub) map[string]any {
	t.Helper()
	reqs := s.recorded("POST", "/api/v2/jobs")
	var body map[string]any
	if err := json.Unmarshal(reqs[len(reqs)-1].Body, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func simpleWorkflow() *Workflow {
	return NewWorkflow(map[string]any{"1": map[string]any{"class_type": "Noop", "inputs": map[string]any{}}})
}

func TestSubmitBodyAndKey(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	c := s.client(t)
	ctx := context.Background()

	if _, err := c.Submit(ctx, simpleWorkflow(), SubmitOptions{}); err != nil {
		t.Fatal(err)
	}
	body := submitBody(t, s)
	if _, ok := body["extra_data"]; ok {
		t.Fatal("extra_data sent without a partner key")
	}
	if _, ok := body["metadata"]; ok {
		t.Fatal("metadata sent without labels")
	}
	if key := s.recorded("POST", "/api/v2/jobs")[0].Header.Get("Idempotency-Key"); len(key) != 36 {
		t.Fatalf("no minted idempotency key: %q", key)
	}

	if _, err := c.Submit(ctx, simpleWorkflow(), SubmitOptions{PartnerAPIKey: "comfyui-x", Metadata: map[string]string{"run": "7"}}); err != nil {
		t.Fatal(err)
	}
	body = submitBody(t, s)
	extra, _ := body["extra_data"].(map[string]any)
	if extra["api_key_comfy_org"] != "comfyui-x" {
		t.Fatalf("extra_data = %v", body["extra_data"])
	}
	if md, _ := body["metadata"].(map[string]any); md["run"] != "7" {
		t.Fatalf("metadata = %v", body["metadata"])
	}
}

func TestSubmitReusedKeyIsTyped(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	c := s.client(t)
	ctx := context.Background()
	opts := SubmitOptions{IdempotencyKey: "my-key"}
	if _, err := c.Submit(ctx, simpleWorkflow(), opts); err != nil {
		t.Fatal(err)
	}
	_, err := c.Submit(ctx, simpleWorkflow(), opts)
	var api *APIError
	if !errors.Is(err, ErrIdempotencyKeyReuse) || !errors.As(err, &api) || api.IdempotencyKey != "my-key" {
		t.Fatalf("reuse: %v", err)
	}
}

func TestSubmitRefusesBadKeysLocally(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	c := s.client(t)
	for _, k := range []string{strings.Repeat("k", 256), "a\r\nb", "naïve"} {
		if _, err := c.Submit(context.Background(), simpleWorkflow(), SubmitOptions{IdempotencyKey: k}); err == nil {
			t.Errorf("%q accepted", k)
		}
	}
	if n := len(s.recorded("POST", "/api/v2/jobs")); n != 0 {
		t.Fatalf("%d requests sent for refused keys", n)
	}
}

func TestSubmitRejectsUIFormatLocally(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	wf := NewWorkflow(map[string]any{"nodes": []any{}, "links": []any{}, "last_node_id": 1})
	_, err := s.client(t).Submit(context.Background(), wf, SubmitOptions{})
	if !errors.Is(err, ErrWorkflowFormatUI) || !errors.Is(err, ErrInvalidWorkflow) {
		t.Fatalf("got %v", err)
	}
	if len(s.requests) != 0 {
		t.Fatal("a UI-format workflow reached the server")
	}
}

func TestSubmitRetries(t *testing.T) {
	isolateEnv(t)
	cases := []struct {
		name    string
		errs    []stubError
		retried bool
	}{
		{"queue_full with Retry-After", []stubError{{429, "queue_full", "1"}}, true},
		{"queue_full without Retry-After uses the default", []stubError{{429, "queue_full", ""}}, true},
		{"another 429 code with Retry-After", []stubError{{429, "deployment_not_ready", "1"}}, true},
		{"another 429 code without Retry-After surfaces", []stubError{{429, "deployment_not_ready", ""}}, false},
		{"Retry-After 0 still waits the minimum", []stubError{{429, "queue_full", "0"}}, true},
		{"negative Retry-After does not storm", []stubError{{429, "queue_full", "-5"}}, true},
		{"a 422 is never retried", []stubError{{422, "invalid_workflow", "1"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastRetry(t, time.Second)
			s := newStub(t)
			s.submitErrors = tc.errs
			_, err := s.client(t).Submit(context.Background(), simpleWorkflow(), SubmitOptions{})
			if tc.retried && err != nil {
				t.Fatalf("not retried: %v", err)
			}
			if !tc.retried && err == nil {
				t.Fatal("retried when it should have surfaced")
			}
		})
	}
}

func TestSubmitGivesUpWhenBudgetSpent(t *testing.T) {
	isolateEnv(t)
	fastRetry(t, 50*time.Millisecond)
	s := newStub(t)
	for i := 0; i < 100; i++ {
		s.submitErrors = append(s.submitErrors, stubError{429, "queue_full", "1000"})
	}
	start := time.Now()
	_, err := s.client(t).Submit(context.Background(), simpleWorkflow(), SubmitOptions{IdempotencyKey: "budget-key"})
	var api *APIError
	if !errors.Is(err, ErrQueueFull) || !errors.As(err, &api) || api.IdempotencyKey != "budget-key" {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("a huge Retry-After was not clamped to the remaining budget")
	}
}

func TestTransportFailureCarriesKey(t *testing.T) {
	isolateEnv(t)
	c, err := New(WithBaseURL("http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Submit(context.Background(), simpleWorkflow(), SubmitOptions{IdempotencyKey: "net-key"})
	var te *TransportError
	if !errors.As(err, &te) || te.IdempotencyKey != "net-key" {
		t.Fatalf("got %v", err)
	}
}

func TestRunPollsToTerminal(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.jobStatus = []string{StatusQueued, StatusRunning, StatusSucceeded}
	job, err := s.client(t).Run(context.Background(), simpleWorkflow(), SubmitOptions{})
	if err != nil || job.Status() != StatusSucceeded || len(job.Outputs()) != 1 {
		t.Fatalf("run = %v, %v", job, err)
	}
	if o := job.GetOutputs("9"); len(o) != 1 || *o[0].JobID() != "job-1" || len(job.GetOutputs("nope")) != 0 {
		t.Fatalf("outputs = %v", o)
	}
}

func TestRunReportsFailure(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.jobStatus = []string{StatusFailed}
	_, err := s.client(t).Run(context.Background(), simpleWorkflow(), SubmitOptions{})
	var jf *JobFailedError
	if !errors.As(err, &jf) || jf.Err == nil || jf.Err.Message != "boom" || !errors.Is(err, ErrJobFailed) {
		t.Fatalf("got %v", err)
	}
}

func TestWaitHonoursContext(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.jobStatus = []string{StatusRunning}
	c := s.client(t)
	job, err := c.Submit(context.Background(), simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if err := job.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestCancelAndGetJob(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	c := s.client(t)
	ctx := context.Background()
	job, err := c.Submit(ctx, simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := job.Cancel(ctx); err != nil || job.Status() != StatusCanceling {
		t.Fatalf("cancel = %v, %s", err, job.Status())
	}
	if len(s.recorded("POST", "/api/v2/jobs/job-1/cancel")) != 1 {
		t.Fatal("cancel did not follow the job's cancel link")
	}
	if _, err := c.GetJob(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing job: %v", err)
	}
}

func TestListJobsPagesAndFilters(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	pages := []string{
		`{"jobs":[{"id":"a","status":"succeeded","metadata":{"run":"7"}},{"id":"b","status":"failed","metadata":"proxy-string"}],"next_cursor":"c1"}`,
		`{"jobs":[{"id":"c","status":"queued","metadata":{"run":"7","x":1}}],"next_cursor":""}`,
	}
	served := 0
	s.routes["GET /api/v2/jobs"] = func(w http.ResponseWriter, r *http.Request) {
		if served > 0 && r.URL.Query().Get("cursor") != "c1" {
			t.Errorf("second page without the cursor: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(pages[min(served, len(pages)-1)])); err != nil {
			t.Error(err)
		}
		served++
	}
	var ids []string
	for js, err := range s.client(t).ListJobs(context.Background(), ListOptions{Metadata: map[string]string{"run": "7"}}) {
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, js.ID)
	}
	if strings.Join(ids, ",") != "a,c" {
		t.Fatalf("ids = %v", ids)
	}
	if q := s.recorded("GET", "/api/v2/jobs"); len(q) != 2 {
		t.Fatalf("%d pages fetched", len(q))
	}
}

func TestListJobsRefusesBadPages(t *testing.T) {
	isolateEnv(t)
	for name, page := range map[string]string{
		"looping cursor":    `{"jobs":[],"next_cursor":"same"}`,
		"non-string cursor": `{"jobs":[],"next_cursor":0}`,
		"jobs not an array": `{"jobs":{}}`,
		"item missing id":   `{"jobs":[{"status":"queued"}]}`,
		"non-string status": `{"jobs":[{"id":"a","status":3}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newStub(t)
			s.routes["GET /api/v2/jobs"] = func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, err := w.Write([]byte(page)); err != nil {
					t.Error(err)
				}
			}
			var last error
			for _, err := range s.client(t).ListJobs(context.Background(), ListOptions{}) {
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
