package sdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"

	"github.com/regiellis/comfy-api-proxy-go/sdk"
)

// fakeAPI is a minimal in-process Comfy API v2 deployment for the examples:
// one job, one output, single-use idempotency keys, and metadata stored as
// the client sent it.
type fakeAPI struct {
	mu       sync.Mutex
	keys     map[string]bool
	metadata json.RawMessage
}

func newFakeAPI() (*fakeAPI, *httptest.Server) {
	f := &fakeAPI{keys: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/jobs", f.submit)
	mux.HandleFunc("GET /api/v2/jobs", f.list)
	mux.HandleFunc("GET /api/v2/jobs/{id}", f.get)
	mux.HandleFunc("GET /api/v2/jobs/{id}/events", f.events)
	mux.HandleFunc("GET /api/v2/assets/{id}/content", f.content)
	return f, httptest.NewServer(mux)
}

func (f *fakeAPI) job(status string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{
		"id": "job-1", "status": status, "created_at": "2026-10-27T10:00:00Z",
		"metadata": f.metadata,
		"outputs": []any{map[string]any{
			"node_id": "9", "name": "out.png", "type": "image", "content_type": "image/png",
			"size_bytes": 8, "id": "out-1",
		}},
	}
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		return // the client left
	}
}

func replyError(w http.ResponseWriter, status int, code, message string) {
	reply(w, status, map[string]any{"error": map[string]any{"code": code, "message": message, "details": nil}})
}

func (f *fakeAPI) submit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		replyError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	key := r.Header.Get("Idempotency-Key")
	f.mu.Lock()
	reused := f.keys[key]
	f.keys[key] = true
	if !reused {
		f.metadata = body.Metadata
	}
	f.mu.Unlock()
	if reused {
		replyError(w, http.StatusUnprocessableEntity, "idempotency_key_reuse", "This Idempotency-Key has already been used.")
		return
	}
	reply(w, http.StatusCreated, f.job(sdk.StatusQueued))
}

func (f *fakeAPI) list(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, map[string]any{"jobs": []any{f.job(sdk.StatusSucceeded)}})
}

func (f *fakeAPI) get(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != "job-1" {
		replyError(w, http.StatusNotFound, "not_found", "No job "+r.PathValue("id")+".")
		return
	}
	reply(w, http.StatusOK, f.job(sdk.StatusSucceeded))
}

func (f *fakeAPI) events(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	frames := "event: status\ndata: {\"status\":\"running\"}\n\n" +
		"event: progress\ndata: {\"value\":0.5,\"nodes_done\":1,\"nodes_total\":2}\n\n" +
		"event: status\ndata: {\"status\":\"succeeded\"}\n\n"
	if _, err := io.WriteString(w, frames); err != nil {
		return // the client left
	}
}

func (f *fakeAPI) content(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	if _, err := io.WriteString(w, "PNG data"); err != nil {
		return // the client left
	}
}

// demoWorkflow is a two-node API-format graph.
func demoWorkflow() *sdk.Workflow {
	return sdk.NewWorkflow(map[string]any{
		"3": map[string]any{"class_type": "KSampler", "inputs": map[string]any{"seed": 42, "steps": 1}},
		"9": map[string]any{"class_type": "SaveImage", "inputs": map[string]any{"images": []any{"3", 0}}},
	})
}

func ExampleNew() {
	_, srv := newFakeAPI()
	defer srv.Close()

	// Against a local comfy-api-proxy you would pass "http://127.0.0.1:8189".
	client, err := sdk.New(
		sdk.WithBaseURL(srv.URL),
		sdk.WithAPIKey("example-token"),
		sdk.WithClientInfo("my-app/1.0"),
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("authenticated:", client.Authenticated())
	// Output:
	// authenticated: true
}

func ExampleWorkflowFromJSON() {
	// A seed above 2^53 would lose precision as a float64. WorkflowFromJSON
	// keeps every number exact, so it reaches the server unchanged.
	wf, err := sdk.WorkflowFromJSON([]byte(`{"3": {"class_type": "KSampler", "inputs": {"seed": 18446744073709551615}}}`))
	if err != nil {
		log.Fatal(err)
	}
	wf.SetInput("3", "steps", 20)
	out, err := json.Marshal(wf.Graph)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(out))
	// Output:
	// {"3":{"class_type":"KSampler","inputs":{"seed":18446744073709551615,"steps":20}}}
}

func ExampleClient_Submit() {
	_, srv := newFakeAPI()
	defer srv.Close()
	client, err := sdk.New(sdk.WithBaseURL(srv.URL))
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	job, err := client.Submit(ctx, demoWorkflow(), sdk.SubmitOptions{IdempotencyKey: "order-1234"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("submitted:", job.ID(), job.Status())

	// Keys are single-use. A second submit under the same key is refused,
	// and the error carries the key so the caller can look the job up.
	_, err = client.Submit(ctx, demoWorkflow(), sdk.SubmitOptions{IdempotencyKey: "order-1234"})
	var apiErr *sdk.APIError
	if errors.Is(err, sdk.ErrIdempotencyKeyReuse) && errors.As(err, &apiErr) {
		fmt.Println("refused:", apiErr.HTTPStatus, apiErr.Code, apiErr.IdempotencyKey)
	}

	if err := job.Wait(ctx); err != nil {
		log.Fatal(err)
	}
	fmt.Println("finished:", job.Status())
	// Output:
	// submitted: job-1 queued
	// refused: 422 idempotency_key_reuse order-1234
	// finished: succeeded
}

func ExampleClient_Run() {
	_, srv := newFakeAPI()
	defer srv.Close()
	client, err := sdk.New(sdk.WithBaseURL(srv.URL))
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	job, err := client.Run(ctx, demoWorkflow(), sdk.SubmitOptions{})
	if err != nil {
		log.Fatal(err)
	}
	for _, o := range job.GetOutputs("9") {
		data, err := o.Bytes(ctx, nil)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s %s %q\n", o.Name(), o.ContentType(), data)
	}
	// Output:
	// out.png image/png "PNG data"
}

func ExampleWithProxyMetadata() {
	fake, srv := newFakeAPI()
	defer srv.Close()
	client, err := sdk.New(sdk.WithBaseURL(srv.URL), sdk.WithProxyMetadata())
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	job, err := client.Submit(ctx, demoWorkflow(), sdk.SubmitOptions{
		Metadata: map[string]string{"project": "demo", "shot": "12"},
	})
	if err != nil {
		log.Fatal(err)
	}
	fake.mu.Lock()
	fmt.Println("sent:", string(fake.metadata))
	fake.mu.Unlock()
	fmt.Println("labels:", job.Metadata())

	for s, err := range client.ListJobs(ctx, sdk.ListOptions{Metadata: map[string]string{"project": "demo"}}) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("listed:", s.ID, s.Status)
	}
	// Output:
	// sent: "{\"project\":\"demo\",\"shot\":\"12\"}"
	// labels: map[project:demo shot:12]
	// listed: job-1 succeeded
}

func ExampleJob_Events() {
	_, srv := newFakeAPI()
	defer srv.Close()
	client, err := sdk.New(sdk.WithBaseURL(srv.URL))
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	job, err := client.Submit(ctx, demoWorkflow(), sdk.SubmitOptions{})
	if err != nil {
		log.Fatal(err)
	}
	for ev, err := range job.Events(ctx) {
		if err != nil {
			log.Fatal(err)
		}
		switch e := ev.(type) {
		case *sdk.StatusChange:
			fmt.Println("status", e.Status)
		case *sdk.Progress:
			fmt.Printf("progress %.0f%%\n", e.Value*100)
		}
	}
	// Output:
	// status running
	// progress 50%
	// status succeeded
}

func ExampleAPIError() {
	_, srv := newFakeAPI()
	defer srv.Close()
	client, err := sdk.New(sdk.WithBaseURL(srv.URL))
	if err != nil {
		log.Fatal(err)
	}

	_, err = client.GetJob(context.Background(), "no-such-job")
	if errors.Is(err, sdk.ErrNotFound) {
		fmt.Println("not found")
	}
	var apiErr *sdk.APIError
	if errors.As(err, &apiErr) {
		fmt.Println(apiErr.HTTPStatus, apiErr.Code, apiErr.Message)
	}
	// Output:
	// not found
	// 404 not_found No job no-such-job.
}
