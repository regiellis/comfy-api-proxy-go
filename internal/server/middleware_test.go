package server

import (
	"encoding/json"
	"github.com/regiellis/comfy-api-proxy-go/internal/httpx"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/regiellis/comfy-api-proxy-go/internal/config"
)

const hosted = "https://app.example.com"

type call struct {
	method, path string
	headers      map[string]string
}

func serve(t *testing.T, h http.Handler, c call) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(c.method, c.path, nil)
	req.Host = "127.0.0.1:8189"
	for k, v := range c.headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env httpx.Envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body %q is not an error envelope: %v", rec.Body.String(), err)
	}
	return env.Error.Code
}

// reached is a terminal handler that records it ran.
func reached(ran *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*ran = true
		w.WriteHeader(http.StatusOK)
	})
}

func headerSet(h http.Header, key string) map[string]bool {
	set := map[string]bool{}
	for part := range strings.SplitSeq(h.Get(key), ",") {
		if p := strings.ToLower(strings.TrimSpace(part)); p != "" {
			set[p] = true
		}
	}
	return set
}

func assertCORS(t *testing.T, h http.Header) {
	t.Helper()
	if got := h.Get("Access-Control-Allow-Origin"); got != hosted {
		t.Fatalf("Access-Control-Allow-Origin = %q", got)
	}
	for _, want := range []string{"authorization", "content-type", "idempotency-key"} {
		if !headerSet(h, "Access-Control-Allow-Headers")[want] {
			t.Errorf("allow-headers missing %s", want)
		}
	}
	for _, want := range []string{"retry-after", "content-range", "accept-ranges"} {
		if !headerSet(h, "Access-Control-Expose-Headers")[want] {
			t.Errorf("expose-headers missing %s", want)
		}
	}
	if h.Get("Access-Control-Allow-Credentials") != "true" {
		t.Error("allow-credentials not true")
	}
	if !headerSet(h, "Vary")["origin"] {
		t.Error("Vary lacks Origin")
	}
}

func TestOriginGuard(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no origin is allowed", nil, 200},
		{"matching origin and host", map[string]string{"Origin": "http://127.0.0.1:8189"}, 200},
		{"mismatched origin on loopback host", map[string]string{"Origin": "http://evil.example:1234"}, 403},
		{"cross-site sec-fetch without origin", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"cross-site sec-fetch with matching origin", map[string]string{"Origin": "http://127.0.0.1:8189", "Sec-Fetch-Site": "cross-site"}, 403},
		{"same-origin sec-fetch", map[string]string{"Sec-Fetch-Site": "same-origin"}, 200},
		{"port omitted on origin side", map[string]string{"Host": "localhost:8189", "Origin": "http://localhost"}, 200},
		{"port omitted on host side", map[string]string{"Host": "127.0.0.1", "Origin": "http://127.0.0.1:3000"}, 200},
		{"ports differ on both sides", map[string]string{"Origin": "http://127.0.0.1:3000"}, 403},
		{"case-insensitive hosts", map[string]string{"Host": "LOCALHOST:8189", "Origin": "http://localhost:8189"}, 200},
		{"non-loopback host is exempt", map[string]string{"Host": "192.168.1.5:8189", "Origin": "http://elsewhere.example"}, 200},
		{"ipv6 loopback match", map[string]string{"Host": "[::1]:8189", "Origin": "http://[::1]:8189"}, 200},
		{"ipv6 origin without port fails closed", map[string]string{"Host": "[::1]:8189", "Origin": "http://[::1]"}, 403},
		{"userinfo in origin fails", map[string]string{"Origin": "http://user@127.0.0.1:8189"}, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ran bool
			rec := serve(t, originGuard(nil, reached(&ran)), call{"GET", "/api/v2/jobs", tc.headers})
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == 403 {
				if ran {
					t.Fatal("handler ran on a rejected request")
				}
				if code := errorCode(t, rec); code != "forbidden_origin" {
					t.Fatalf("code = %q", code)
				}
			}
		})
	}
}

func TestOriginGuardOptionsWithoutAllowlist(t *testing.T) {
	var ran bool
	rec := serve(t, originGuard(nil, reached(&ran)), call{"OPTIONS", "/api/v2/jobs", nil})
	if rec.Code != 200 || ran {
		t.Fatalf("status = %d, ran = %v; want 200 without reaching the handler", rec.Code, ran)
	}
}

func TestCORSAllowlist(t *testing.T) {
	h := newTestHandler(&config.Config{ComfyUI: "http://x", CORSOrigins: []string{hosted}})
	crossSite := map[string]string{"Origin": hosted, "Sec-Fetch-Site": "cross-site"}

	rec := serve(t, h, call{"OPTIONS", "/api/v2/jobs/11111111-1111-1111-1111-111111111111", crossSite})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("preflight body = %q", rec.Body.String())
	}
	assertCORS(t, rec.Header())
	if !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Error("allow-methods lacks POST")
	}

	rec = serve(t, h, call{"GET", "/api/v2/health", crossSite})
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d", rec.Code)
	}
	assertCORS(t, rec.Header())

	for _, method := range []string{"GET", "OPTIONS"} {
		rec = serve(t, h, call{method, "/api/v2/health", map[string]string{"Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"}})
		if rec.Code != http.StatusForbidden || errorCode(t, rec) != "forbidden_origin" {
			t.Fatalf("%s from evil origin: %d %s", method, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("%s from evil origin got ACAO", method)
		}
	}
}

func TestDefaultBlocksHostedOrigin(t *testing.T) {
	h := newTestHandler(&config.Config{ComfyUI: "http://x"})
	rec := serve(t, h, call{"GET", "/api/v2/health", map[string]string{"Origin": hosted, "Sec-Fetch-Site": "cross-site"}})
	if rec.Code != http.StatusForbidden || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("status = %d, ACAO = %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestAddVaryMerges(t *testing.T) {
	h := http.Header{}
	h.Set("Vary", "Accept-Encoding")
	addVary(h, "Origin")
	addVary(h, "origin")
	if got := h.Values("Vary"); len(got) != 2 || got[1] != "Origin" {
		t.Fatalf("Vary = %v", got)
	}
}

func TestBearerAuth(t *testing.T) {
	rejected := []map[string]string{
		nil,
		{"Authorization": "secret"},
		{"Authorization": "Basic secret"},
		{"Authorization": "Bearer"},
		{"Authorization": "Bearer "},
		{"Authorization": "Bearer nope"},
		{"Authorization": "Bearer secret2"},
		{"Authorization": "Bearer  secret"},
	}
	for _, hdrs := range rejected {
		var ran bool
		rec := serve(t, bearerAuth("secret", reached(&ran)), call{"GET", "/api/v2/jobs/x", hdrs})
		if rec.Code != http.StatusUnauthorized || ran {
			t.Errorf("%v: status %d, ran %v; want 401 without reaching the handler", hdrs, rec.Code, ran)
			continue
		}
		if code := errorCode(t, rec); code != "unauthorized" {
			t.Errorf("%v: code %q", hdrs, code)
		}
	}

	allowed := []call{
		{"GET", "/api/v2/jobs/x", map[string]string{"Authorization": "Bearer secret"}},
		{"GET", "/api/v2/jobs/x", map[string]string{"Authorization": "bearer secret"}},
		{"GET", "/api/v2/health", nil},
		{"POST", "/api/v2/health", nil},
		{"GET", "/health", nil},
		{"OPTIONS", "/api/v2/jobs/x", nil},
	}
	for _, c := range allowed {
		var ran bool
		serve(t, bearerAuth("secret", reached(&ran)), c)
		if !ran {
			t.Errorf("%s %s %v: handler did not run", c.method, c.path, c.headers)
		}
	}
}

// A path that dodges the /api/v2/ prefix check must never reach a v2 handler
// unauthenticated. The mux answers it with a redirect to the clean path,
// which then goes through the gate like any other request.
func TestAuthBypassViaUncleanPath(t *testing.T) {
	h := newTestHandler(&config.Config{ComfyUI: "http://x", Token: "secret"})
	for _, path := range []string{"//api/v2/health/../jobs", "/x/../api/v2/jobs", "/api/v2/health/../jobs"} {
		rec := serve(t, h, call{"GET", path, nil})
		if rec.Code == http.StatusOK {
			t.Errorf("%s: got 200 without a token", path)
		}
		redirect := rec.Code >= 300 && rec.Code < 400
		if !redirect && rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want a redirect or 401", path, rec.Code)
		}
	}
}

func TestPreflightThenTokenRequired(t *testing.T) {
	h := newTestHandler(&config.Config{ComfyUI: "http://x", Token: "secret", CORSOrigins: []string{hosted}})
	crossSite := map[string]string{"Origin": hosted, "Sec-Fetch-Site": "cross-site"}
	path := "/api/v2/jobs/11111111-1111-1111-1111-111111111111"

	rec := serve(t, h, call{"OPTIONS", path, crossSite})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", rec.Code)
	}

	rec = serve(t, h, call{"GET", path, crossSite})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", rec.Code)
	}
	assertCORS(t, rec.Header())
}
