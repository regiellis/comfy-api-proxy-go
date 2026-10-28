package sdk

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func TestProxyMetadataRoundTrip(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	want := map[string]string{"project": "demo", "caption": "雪 \"quoted\"", "empty": ""}
	var stored string
	s.routes["POST /api/v2/jobs"] = func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Metadata string `json:"metadata"`
		}
		if err := json.Unmarshal(s.recorded("POST", "/api/v2/jobs")[0].Body, &body); err != nil {
			http.Error(w, "metadata must be a string", http.StatusUnprocessableEntity)
			return
		}
		stored = body.Metadata
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(map[string]any{"id": "job-1", "status": "queued", "metadata": stored}); err != nil {
			t.Error(err)
		}
	}
	s.routes["GET /api/v2/jobs/job-1"] = func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{"id": "job-1", "status": "succeeded", "metadata": stored}); err != nil {
			t.Error(err)
		}
	}
	s.routes["GET /api/v2/jobs"] = func(w http.ResponseWriter, r *http.Request) {
		for key := range r.URL.Query() {
			if key != "limit" {
				t.Errorf("unexpected proxy query: %s", key)
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"jobs": []any{
			map[string]any{"id": "job-1", "status": "succeeded", "metadata": stored},
			map[string]any{"id": "unlabeled", "status": "succeeded", "metadata": "opaque"},
		}}); err != nil {
			t.Error(err)
		}
	}
	c := s.client(t, WithProxyMetadata())
	job, err := c.Submit(t.Context(), simpleWorkflow(), SubmitOptions{Metadata: want})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(job.Metadata(), want) {
		t.Fatalf("submitted labels = %v", job.Metadata())
	}
	job, err = c.GetJob(t.Context(), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(job.Metadata(), want) {
		t.Fatalf("fetched labels = %v", job.Metadata())
	}
	count := 0
	for item, err := range c.ListJobs(t.Context(), ListOptions{Metadata: map[string]string{"empty": ""}, Limit: 10}) {
		if err != nil {
			t.Fatal(err)
		}
		count++
		if item.ID != "job-1" || !reflect.DeepEqual(item.Metadata, want) {
			t.Fatalf("summary = %+v", item)
		}
	}
	if count != 1 {
		t.Fatalf("got %d jobs, want 1", count)
	}
}

func TestMetadataDecoding(t *testing.T) {
	for _, raw := range []string{`null`, `"opaque"`, `"{broken"`, `"[]"`, `42`, `[]`} {
		if got := labels(json.RawMessage(raw)); len(got) != 0 {
			t.Errorf("labels(%s) = %v", raw, got)
		}
	}
}
