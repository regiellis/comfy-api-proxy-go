package comfy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// ComfyUI traffic carries the partner API key, so an HTTP_PROXY in the
// environment must never route it elsewhere.
func TestClientIgnoresEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{}`)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "")
	if c.HTTP.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("transport consults the environment proxy")
	}
	raw, ok, err := c.GetJSON(t.Context(), "/history/x")
	if err != nil || !ok || string(raw) != "{}" {
		t.Fatalf("GetJSON = %s, %v, %v", raw, ok, err)
	}
}

// A ComfyUI behind an authenticating gateway gets the configured token on
// HTTP requests and on the WebSocket handshake, which shares the client.
func TestClientSendsTheUpstreamToken(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{}`)); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "s3cret")
	if _, _, err := c.GetJSON(t.Context(), "/history/x"); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("the caller's request was modified")
	}
	want := []string{"/history/x Bearer s3cret", "/ws Bearer s3cret"}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Fatalf("upstream saw %q, want %q", seen, want)
	}

	plain := New(srv.URL, "")
	seen = nil
	if _, _, err := plain.GetJSON(t.Context(), "/history/x"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "/history/x " {
		t.Fatalf("with no token the upstream saw %q", seen)
	}
}
