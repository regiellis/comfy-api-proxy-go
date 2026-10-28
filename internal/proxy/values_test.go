package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/regiellis/comfy-api-proxy-go/internal/comfy"
)

// valueComfy serves the history a real ComfyUI 0.43.0 wrote for a
// PrimitiveString into PreviewAny and SaveText (checked 2026-10-28): raw
// strings under "text", and SaveText's file under "files". HEADs of /view are
// counted, and the file answers with its size.
func valueComfy(t *testing.T, jobID string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var heads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /history/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{}`
		if r.PathValue("id") == jobID {
			body = `{"` + jobID + `":{"outputs":{` +
				`"2":{"text":["probe22 hello","{\"score\": 0.93}"]},` +
				`"3":{"text":["probe22 hello"],"files":[{"filename":"probe22_00001.txt","subfolder":"comfy-api-proxy-test","type":"output"}]},` +
				`"4":{"text":[42]}},` +
				`"status":{"status_str":"success","completed":true,"messages":[]}}}`
		}
		if _, err := w.Write([]byte(body)); err != nil {
			return // the client left
		}
	})
	mux.HandleFunc("GET /view", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
		}
		w.Header().Set("Content-Length", "13")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &heads
}

const valueJob = "3e6ef66f-a573-4035-b3ca-b9a54cb2217d"

func valueProxy(t *testing.T) (*Proxy, *atomic.Int32) {
	t.Helper()
	srv, heads := valueComfy(t, valueJob)
	p := testProxy(t)
	p.comfy = comfy.New(srv.URL, "")
	return p, heads
}

func TestValueOutputsAreListed(t *testing.T) {
	p, heads := valueProxy(t)
	st, err := p.statusOf(context.Background(), valueJob, "http://proxy")
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		node, name, typ, ct string
		size                int64
	}
	var outs []got
	for _, o := range st.Outputs {
		outs = append(outs, got{o.NodeID, o.Name, o.Type, o.ContentType, o.SizeBytes})
		if !strings.HasPrefix(o.URL, "http://proxy/api/v2/assets/") || o.Hash != nil {
			t.Errorf("%s: url %q hash %v", o.Name, o.URL, o.Hash)
		}
	}
	want := []got{
		{"2", "2_text_0.txt", "text", "text/plain", int64(len("probe22 hello"))},
		{"2", "2_text_1.txt", "text", "text/plain", int64(len(`{"score": 0.93}`))},
		{"3", "3_text_0.txt", "text", "text/plain", int64(len("probe22 hello"))},
		{"3", "probe22_00001.txt", "file", "text/plain", 13},
	}
	if len(outs) != len(want) {
		t.Fatalf("outputs = %+v, want %+v (a non-string value is skipped)", outs, want)
	}
	for i := range want {
		if outs[i] != want[i] {
			t.Errorf("output %d = %+v, want %+v", i, outs[i], want[i])
		}
	}
	// Values carry their size already; only the file needs ComfyUI's /view.
	if n := heads.Load(); n != 1 {
		t.Errorf("HEAD /view sent %d times, want 1 (the file only)", n)
	}
}

func TestValueOutputContent(t *testing.T) {
	p, _ := valueProxy(t)
	st, err := p.statusOf(context.Background(), valueJob, "http://proxy")
	if err != nil {
		t.Fatal(err)
	}
	id := st.Outputs[1].ID

	get := func(method, id, rng string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/v2/assets/"+id+"/content", nil)
		req.SetPathValue("id", id)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		rec := httptest.NewRecorder()
		p.getAssetContent(rec, req)
		return rec
	}

	rec := get(http.MethodGet, id, "")
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || string(body) != `{"score": 0.93}` {
		t.Fatalf("content = %d %q", rec.Code, body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q: a value must never be served as HTML or script", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
	if rec := get(http.MethodGet, id, "bytes=0-1"); rec.Code != http.StatusPartialContent || rec.Body.String() != `{"` {
		t.Errorf("range = %d %q", rec.Code, rec.Body.String())
	}

	// A value id whose index is past the list reads as unavailable.
	gone := p.valueAssetID(valueJob, "2", "text", 9)
	if rec := get(http.MethodGet, gone, ""); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "output_unavailable") {
		t.Errorf("missing value = %d %s", rec.Code, rec.Body.String())
	}

	// GET /assets/{id} reports the same size and type.
	req := httptest.NewRequest(http.MethodGet, "/api/v2/assets/"+id, nil)
	req.SetPathValue("id", id)
	meta := httptest.NewRecorder()
	p.getAsset(meta, req)
	var asset struct {
		SizeBytes   int64  `json:"size_bytes"`
		ContentType string `json:"content_type"`
		JobID       string `json:"job_id"`
	}
	if err := json.NewDecoder(meta.Body).Decode(&asset); err != nil {
		t.Fatal(err)
	}
	if asset.SizeBytes != int64(len(`{"score": 0.93}`)) || asset.ContentType != "text/plain" || asset.JobID != valueJob {
		t.Errorf("asset = %+v", asset)
	}
}

func TestValueAssetIDsAreSignedAndDistinct(t *testing.T) {
	p := testProxy(t)
	id := p.valueAssetID(valueJob, "2", "text", 1)
	ref, ok := p.decodeValueAssetID(id)
	if !ok || ref.job != valueJob || ref.node != "2" || ref.key != "text" || ref.index != 1 {
		t.Fatalf("round trip = %+v %v", ref, ok)
	}
	// Neither kind of id decodes as the other.
	if _, ok := p.decodeOutputAssetID(id); ok {
		t.Error("a value id decoded as a file id")
	}
	if _, ok := p.decodeValueAssetID(p.outputAssetID(valueJob, "a.png", "", "output")); ok {
		t.Error("a file id decoded as a value id")
	}
	// A changed payload or a key from another proxy does not verify.
	payload, tag, _ := strings.Cut(id, ".")
	if _, ok := p.decodeValueAssetID(payload + "x." + tag); ok {
		t.Error("a tampered payload verified")
	}
	other := testProxy(t)
	other.secret = []byte("another-secret")
	if _, ok := other.decodeValueAssetID(id); ok {
		t.Error("an id verified under another secret")
	}
}
