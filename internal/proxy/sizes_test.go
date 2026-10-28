package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/regiellis/comfy-api-proxy-go/internal/comfy"
)

// sizedComfy serves one finished job whose output is size bytes long, and
// counts the HEAD requests /view receives.
func sizedComfy(t *testing.T, jobID string, size int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var heads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /history/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("id") != jobID {
			// The client left if this fails; the test reads what it got.
			if _, err := w.Write([]byte(`{}`)); err != nil {
				return
			}
			return
		}
		body := `{"` + jobID + `":{"outputs":{"9":{"images":[{"filename":"out.png","subfolder":"","type":"output"}]}},` +
			`"status":{"status_str":"success","completed":true,"messages":[]}}}`
		if _, err := w.Write([]byte(body)); err != nil {
			return
		}
	})
	mux.HandleFunc("GET /view", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("filename") != "out.png" || r.URL.Query().Get("type") != "output" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodHead {
			heads.Add(1)
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", strconv.Itoa(size))
		if r.Method == http.MethodHead {
			return
		}
		if _, err := w.Write(make([]byte, size)); err != nil {
			return
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &heads
}

func TestOutputsReportTheirSize(t *testing.T) {
	const jobID = "3f2b8c1e-6d4a-4f0e-9b7c-2a1d5e8f9c00"
	srv, heads := sizedComfy(t, jobID, 1553)
	p := testProxy(t)
	p.comfy = comfy.New(srv.URL, "")

	st, err := p.statusOf(context.Background(), jobID, "http://proxy")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Outputs) != 1 || st.Outputs[0].SizeBytes != 1553 {
		t.Fatalf("outputs = %+v, want one output of 1553 bytes", st.Outputs)
	}

	// A polling client must not cost ComfyUI a request per output per poll.
	for range 3 {
		if _, err := p.statusOf(context.Background(), jobID, "http://proxy"); err != nil {
			t.Fatal(err)
		}
	}
	if n := heads.Load(); n != 1 {
		t.Fatalf("ComfyUI saw %d HEAD requests for one output, want 1", n)
	}

	// The asset endpoint reports the same size as the job.
	req := httptest.NewRequest(http.MethodGet, "/api/v2/assets/"+st.Outputs[0].ID, nil)
	req.SetPathValue("id", st.Outputs[0].ID)
	rec := httptest.NewRecorder()
	p.getAsset(rec, req)
	var asset struct {
		SizeBytes int64 `json:"size_bytes"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&asset); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || asset.SizeBytes != 1553 {
		t.Fatalf("GET /assets/{id} = %d, size %d; want 200 and 1553", rec.Code, asset.SizeBytes)
	}
}

func TestOutputSizeUnknownWhenComfyUIRefuses(t *testing.T) {
	const jobID = "3f2b8c1e-6d4a-4f0e-9b7c-2a1d5e8f9c01"
	srv, _ := sizedComfy(t, jobID, 10)
	p := testProxy(t)
	p.comfy = comfy.New(srv.URL, "")
	// An output /view does not know reads as size 0 and does not fail the read.
	outs := []output{{Name: "gone.png", ref: ComfyRef{Filename: "gone.png", Type: "output"}, ID: "x"}}
	p.fillSizes(context.Background(), outs)
	if outs[0].SizeBytes != 0 {
		t.Fatalf("size = %d, want 0 for an output ComfyUI does not have", outs[0].SizeBytes)
	}
}
