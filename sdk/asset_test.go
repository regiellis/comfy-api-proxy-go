package sdk

import (
	"bytes"
	"context"
	"errors"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAssetDedupSkipsUpload(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.hashKnown = true
	a := s.client(t).AssetFromBytes([]byte("same bytes"), "photo.png", "")
	id, err := a.Commit(context.Background())
	if err != nil || id != "asset-dedup" || *a.CreatedNew() {
		t.Fatalf("commit = %q, %v", id, err)
	}
	if len(s.recorded("POST", "/api/v2/assets")) != 0 {
		t.Fatal("bytes the server already holds were uploaded again")
	}
	want := hashBytes([]byte("same bytes"))
	if got := s.recorded("HEAD", "/api/v2/assets/by-hash/"+want); len(got) != 1 {
		t.Fatalf("no HEAD by hash %s", want)
	}
}

func TestAssetUploadStreamsMultipart(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	path := filepath.Join(t.TempDir(), "photo.png")
	data := bytes.Repeat([]byte("p"), 300_000)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	a := s.client(t).AssetFromFile(path)
	id, err := a.Commit(context.Background())
	if err != nil || id != "asset-new" || !*a.CreatedNew() || a.ExpiresAt() == nil || a.JobID() != nil {
		t.Fatalf("commit = %q, %v", id, err)
	}
	if again, err := a.Commit(context.Background()); err != nil || again != id {
		t.Fatal("second commit was not a no-op")
	}

	reqs := s.recorded("POST", "/api/v2/assets")
	if len(reqs) != 1 {
		t.Fatalf("%d uploads", len(reqs))
	}
	r := reqs[0]
	if r.Header.Get("Idempotency-Key") == "" {
		t.Fatal("upload without an idempotency key")
	}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(r.Body), params["boundary"])
	fields := map[string]string{}
	var file []byte
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(p); err != nil {
			t.Fatal(err)
		}
		if p.FormName() == "file" {
			file = buf.Bytes()
			if p.FileName() != "photo.png" || p.Header.Get("Content-Type") != "image/png" {
				t.Fatalf("file part header %v", p.Header)
			}
			continue
		}
		fields[p.FormName()] = buf.String()
	}
	if !bytes.Equal(file, data) {
		t.Fatalf("file part %d bytes, want %d", len(file), len(data))
	}
	if fields["file_path"] != "photo.png" || fields["content_type"] != "image/png" || fields["expected_hash"] != hashBytes(data) {
		t.Fatalf("fields = %v", fields)
	}
}

func TestAssetHashMismatchSurfacesWithoutRetry(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	s.routes["POST /api/v2/assets"] = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 409, envelope("hash_mismatch", "bytes do not match"))
	}
	_, err := s.client(t).AssetFromBytes([]byte("x"), "x.bin", "").Commit(context.Background())
	if !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("got %v", err)
	}
	if n := len(s.recorded("POST", "/api/v2/assets")); n != 1 {
		t.Fatalf("hash mismatch retried: %d uploads", n)
	}
}

func TestSubmitSubstitutesAssetReferences(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	c := s.client(t)
	a := c.AssetFromBytes([]byte("img"), "in.png", "image/png")
	wf := simpleWorkflow()
	wf.SetInput("10", "image", a)
	wf.SetInput("11", "images", []any{a, "plain"})
	if _, err := c.Submit(context.Background(), wf, SubmitOptions{}); err != nil {
		t.Fatal(err)
	}
	body := submitBody(t, s)
	graph := body["workflow"].(map[string]any)
	ref := graph["10"].(map[string]any)["inputs"].(map[string]any)["image"].(map[string]any)
	info := ref["info"].(map[string]any)
	if ref["__type"] != "core/ASSET" || info["id"] != "asset-new" || info["file_path"] != "in.png" {
		t.Fatalf("reference = %v", ref)
	}
	list := graph["11"].(map[string]any)["inputs"].(map[string]any)["images"].([]any)
	if list[0].(map[string]any)["__type"] != "core/ASSET" || list[1] != "plain" {
		t.Fatalf("nested list = %v", list)
	}
	if n := len(s.recorded("POST", "/api/v2/assets")); n != 1 {
		t.Fatalf("one handle used twice was uploaded %d times", n)
	}
	if _, isAsset := wf.Graph["10"].(map[string]any)["inputs"].(map[string]any)["image"].(*Asset); !isAsset {
		t.Fatal("submit rewrote the caller's workflow in place")
	}
}

func TestAssetDelete(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	c := s.client(t)
	ctx := context.Background()
	a := c.AssetFromBytes([]byte("x"), "x.bin", "")
	if err := a.Delete(ctx); err == nil {
		t.Fatal("deleting an uncommitted asset succeeded")
	}
	if _, err := a.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx); err != nil || a.ID() != "" {
		t.Fatalf("delete = %v, id %q", err, a.ID())
	}
	if err := c.DeleteAsset(ctx, "asset-2"); err != nil {
		t.Fatal(err)
	}
	if len(s.recorded("DELETE", "/api/v2/assets/asset-new")) != 1 || len(s.recorded("DELETE", "/api/v2/assets/asset-2")) != 1 {
		t.Fatal("deletes did not reach the server")
	}
}

const signedURL = "https://storage.example.invalid/b/o?X-Goog-Date=20260710T180000Z&X-Goog-Expires=3600&X-Goog-Signature=abc"

func TestParseExpiry(t *testing.T) {
	got, ok := parseExpiry(signedURL)
	if !ok || !got.Equal(time.Date(2026, 7, 10, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("expiry = %v %v", got, ok)
	}
	for _, bad := range []string{
		"https://example.invalid/assets/x/content",
		"https://example.invalid?X-Goog-Date=not-a-date&X-Goog-Expires=60",
		"https://example.invalid?X-Goog-Date=20260710T180000Z&X-Goog-Expires=x",
		"https://example.invalid?X-Goog-Date=20260710T180000Z&X-Goog-Expires=60abc",
		"https://example.invalid?X-Goog-Date=20260710T180000Z&X-Goog-Expires=999999999999999999",
	} {
		if _, ok := parseExpiry(bad); ok {
			t.Errorf("%s parsed", bad)
		}
	}
}

func TestDownloadURL(t *testing.T) {
	isolateEnv(t)
	t.Run("cloud redirect returns the signed url without contacting it", func(t *testing.T) {
		s := newStub(t)
		s.routes["GET /api/v2/assets/out-1/content"] = func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, signedURL, http.StatusFound)
		}
		job, err := s.client(t, WithAPIKey("tok")).Run(context.Background(), simpleWorkflow(), SubmitOptions{})
		if err != nil {
			t.Fatal(err)
		}
		d, err := job.Outputs()[0].DownloadURL(context.Background())
		if err != nil || d.URL != signedURL || d.ExpiresAt == nil {
			t.Fatalf("download url = %+v, %v", d, err)
		}
	})
	t.Run("self-hosted returns the content endpoint", func(t *testing.T) {
		s := newStub(t)
		c := s.client(t)
		a := c.AssetFromBytes([]byte("x"), "x.png", "")
		d, err := a.DownloadURL(context.Background())
		if err != nil || d.URL != s.srv.URL+"/api/v2/assets/asset-new/content" || d.ExpiresAt != nil {
			t.Fatalf("download url = %+v, %v", d, err)
		}
	})
}

func TestOutputDownloads(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	ctx := context.Background()
	job, err := s.client(t).Run(ctx, simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := job.Outputs()[0]
	all, err := out.Bytes(ctx, nil)
	if err != nil || string(all) != "\x89PNG" {
		t.Fatalf("bytes = %q %v", all, err)
	}
	part, err := out.Bytes(ctx, &Range{Start: 1, End: 2})
	if err != nil || string(part) != "PN" {
		t.Fatalf("range = %q %v", part, err)
	}
	dest := filepath.Join(t.TempDir(), "out.png")
	if err := out.ToFile(ctx, dest, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dest); err != nil || string(got) != "\x89PNG" {
		t.Fatalf("file = %q %v", got, err)
	}
	var w strings.Builder
	if n, err := out.ToWriter(ctx, &w, nil); err != nil || n != 4 {
		t.Fatalf("writer = %d %v", n, err)
	}
}

func TestOutputOnDeletedAssetIsTyped(t *testing.T) {
	isolateEnv(t)
	s := newStub(t)
	ctx := context.Background()
	job, err := s.client(t).Run(ctx, simpleWorkflow(), SubmitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s.routes["GET /api/v2/assets/out-1/content"] = func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, 404, envelope("not_found", "gone"))
	}
	dest := filepath.Join(t.TempDir(), "out.png")
	if err := job.Outputs()[0].ToFile(ctx, dest, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a failed download left a file behind")
	}
}
