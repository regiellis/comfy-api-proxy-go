package proxy

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/regiellis/comfy-api-proxy-go/internal/comfy"
)

// BenchmarkUploadInput measures one input upload through the whole handler:
// multipart parsing, spooling while hashing, and forwarding to a ComfyUI
// stand-in that discards the bytes. Run with -cpu 1 for the per-core figure
// task bench reports, and -cpuprofile to see where the time goes.
func BenchmarkUploadInput(b *testing.B) {
	const size = 64 << 20
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"name":"bench.png","subfolder":"","type":"input"}`)); err != nil {
			return // the proxy hung up
		}
	}))
	b.Cleanup(upstream.Close)

	p := testProxy(b)
	p.comfy = comfy.New(upstream.URL, "")
	p.maxUpload = 2 * size

	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i * 31)
	}
	b.SetBytes(size)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		// A distinct first word per run, so no upload is a dedup hit.
		copy(payload, strconv.Itoa(i)+"-unique")
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		part, err := mw.CreateFormFile("file", "bench.png")
		if err != nil {
			b.Fatal(err)
		}
		if _, err := part.Write(payload); err != nil {
			b.Fatal(err)
		}
		for k, v := range map[string]string{"file_path": "input/bench-" + strconv.Itoa(i) + ".png", "content_type": "image/png"} {
			if err := mw.WriteField(k, v); err != nil {
				b.Fatal(err)
			}
		}
		if err := mw.Close(); err != nil {
			b.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v2/assets", &body)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		rec := httptest.NewRecorder()
		p.uploadAsset(rec, req)
		if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			b.Fatalf("upload = %d %s", rec.Code, rec.Body.String())
		}
	}
}
