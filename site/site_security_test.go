package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// policyApp is the whole site behind the Host policy, as main serves it.
func policyApp(t *testing.T, host, port, origin string) http.Handler {
	t.Helper()
	policy, err := newSitePolicy(host, port, origin)
	if err != nil {
		t.Fatal(err)
	}
	app, err := newApp(origin, policy)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.CloseDB(); err != nil {
			t.Error(err)
		}
	})
	return app
}

func request(t *testing.T, h http.Handler, method, target, host string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Host = host
	r.Header.Set("Accept", "text/html")
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := policyApp(t, "127.0.0.1", "8090", testOrigin)
	for _, target := range []string{"/", "/docs/api", "/docs/api.md", "/llms.txt", "/docs/search?q=token", "/healthz", "/nope", "/static/css/site.css"} {
		w := request(t, h, http.MethodGet, target, "comfy-proxy.regiellis.com", nil)
		want := map[string]string{
			"Content-Security-Policy":   contentSecurityPolicy,
			"X-Content-Type-Options":    "nosniff",
			"X-Frame-Options":           "DENY",
			"Referrer-Policy":           "strict-origin-when-cross-origin",
			"Strict-Transport-Security": "max-age=31536000",
			"Permissions-Policy":        "camera=(), microphone=(), geolocation=()",
		}
		for k, v := range want {
			if got := w.Header().Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", target, k, got, v)
			}
		}
	}
	csp := contentSecurityPolicy
	for _, banned := range []string{"unsafe-eval", "unsafe-inline", "*"} {
		if strings.Contains(csp, banned) {
			t.Errorf("CSP allows %s", banned)
		}
	}
	// Plain-HTTP loopback (the container health check) gets no HSTS.
	if w := request(t, h, http.MethodGet, "/healthz", "127.0.0.1:8090", nil); w.Code != 200 || w.Header().Get("Strict-Transport-Security") != "" {
		t.Errorf("loopback healthz: %d, HSTS %q", w.Code, w.Header().Get("Strict-Transport-Security"))
	}
}

func TestSiteBoundary(t *testing.T) {
	h := policyApp(t, "127.0.0.1", "8090", "")
	for _, tt := range []struct {
		name, method, path, host, origin string
		status                           int
	}{
		{"local page", "GET", "/", "localhost:8090", "", 200},
		{"head", "HEAD", "/healthz", "localhost:8090", "", 200},
		{"rebound host", "GET", "/", "evil.example:8090", "", 421},
		{"post", "POST", "/docs/search", "localhost:8090", "", 405},
		{"delete", "DELETE", "/", "localhost:8090", "", 405},
		{"foreign origin", "GET", "/docs/search?q=x", "localhost:8090", "https://evil.example", 403},
		{"other scheme", "GET", "/", "localhost:8090", "https://localhost:8090", 403},
		{"debug endpoint", "GET", "/__hyperchi/hot-reload", "localhost:8090", "", 404},
		{"huge query", "GET", "/docs/search?q=" + strings.Repeat("a", 2049), "localhost:8090", "", 414},
	} {
		t.Run(tt.name, func(t *testing.T) {
			header := map[string]string{}
			if tt.origin != "" {
				header["Origin"] = tt.origin
			}
			w := request(t, h, tt.method, tt.path, tt.host, header)
			if w.Code != tt.status {
				t.Fatalf("status %d want %d", w.Code, tt.status)
			}
			if w.Header().Get("Content-Security-Policy") == "" {
				t.Fatal("missing CSP")
			}
		})
	}
}

// fakeMachine makes interfaceAddrs return a LAN, a tailnet, a loopback, and a
// link-local address for the length of the test.
func fakeMachine(t *testing.T) {
	t.Helper()
	prev := interfaceAddrs
	interfaceAddrs = func() ([]net.Addr, error) {
		var addrs []net.Addr
		for _, cidr := range []string{"192.168.4.59/24", "100.77.49.6/32", "127.0.0.1/8", "fe80::1/64"} {
			ip, ipNet, err := net.ParseCIDR(cidr)
			if err != nil {
				return nil, err
			}
			addrs = append(addrs, &net.IPNet{IP: ip, Mask: ipNet.Mask})
		}
		return addrs, nil
	}
	t.Cleanup(func() { interfaceAddrs = prev })
}

// A dev run bound to 0.0.0.0 answers on every address of the machine, so a
// client reaches it by LAN or tailnet IP. Production sets PUBLIC_ORIGIN and
// answers only that origin and loopback, whatever the machine's addresses.
func TestDevBindAnswersThisMachineOnly(t *testing.T) {
	fakeMachine(t)
	dev := policyApp(t, "0.0.0.0", "8090", "")
	prod := policyApp(t, "0.0.0.0", "8090", testOrigin)
	for _, tc := range []struct {
		host      string
		dev, prod int
	}{
		{"192.168.4.59:8090", 200, 421},
		{"100.77.49.6:8090", 200, 421},
		{"localhost:8090", 200, 200},
		{"comfy-proxy.regiellis.com", 421, 200},
		{"evil.example:8090", 421, 421},
		{"[fe80::1]:8090", 421, 421},
	} {
		if got := request(t, dev, "GET", "/healthz", tc.host, nil).Code; got != tc.dev {
			t.Errorf("dev %s: %d want %d", tc.host, got, tc.dev)
		}
		if got := request(t, prod, "GET", "/healthz", tc.host, nil).Code; got != tc.prod {
			t.Errorf("prod %s: %d want %d", tc.host, got, tc.prod)
		}
	}
	policy, err := newSitePolicy("0.0.0.0", "8090", testOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if origins := policy.Origins(); !slices.Contains(origins, testOrigin) || slices.Contains(origins, "http://192.168.4.59:8090") {
		t.Errorf("production origins %v", origins)
	}
}

func TestPolicyRejectsBadSettings(t *testing.T) {
	for _, origin := range []string{"http://comfy-proxy.regiellis.com", "https://comfy-proxy.regiellis.com/docs", "https://user@comfy-proxy.regiellis.com", "https://"} {
		if _, err := newSitePolicy("0.0.0.0", "8090", origin); err == nil {
			t.Errorf("accepted origin %s", origin)
		}
	}
	for _, port := range []string{"", "0", "65536", "http"} {
		if _, err := newSitePolicy("0.0.0.0", port, ""); err == nil {
			t.Errorf("accepted port %q", port)
		}
	}
	if _, err := newSitePolicy("example.com", "8090", ""); err == nil {
		t.Error("accepted a host name")
	}
}

func TestErrorPagesForBrowsersAndFragmentsForHTMX(t *testing.T) {
	h := policyApp(t, "127.0.0.1", "8090", "")
	page := request(t, h, "GET", "/", "evil.example:8090", nil)
	if !strings.Contains(page.Body.String(), "<h1>Misdirected Request</h1>") {
		t.Errorf("browser refusal is not the site's error page:\n%s", page.Body.String())
	}
	frag := request(t, h, "GET", "/nope", "localhost:8090", map[string]string{"HX-Request": "true"})
	if frag.Code != 404 || strings.Contains(frag.Body.String(), "<html") {
		t.Errorf("htmx 404 is a whole page: %d", frag.Code)
	}
}

func TestReadSettings(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	free := func(_ string, port int) bool { return port > 8091 } // 8090 and 8091 are busy

	dev, err := readSettings(env(nil), free)
	if err != nil || dev.host != "0.0.0.0" || dev.port != "8092" || dev.production {
		t.Errorf("dev defaults: %+v, %v", dev, err)
	}
	fixed, err := readSettings(env(map[string]string{"PORT": "9000", "HOST": "127.0.0.1"}), free)
	if err != nil || fixed.port != "9000" || fixed.host != "127.0.0.1" {
		t.Errorf("explicit dev settings: %+v, %v", fixed, err)
	}
	prod, err := readSettings(env(map[string]string{"ENVIRONMENT": "production", "PORT": "8080", "PUBLIC_ORIGIN": testOrigin}), free)
	if err != nil || !prod.production || prod.port != "8080" || prod.publicOrigin != testOrigin {
		t.Errorf("production settings: %+v, %v", prod, err)
	}
	for name, vars := range map[string]map[string]string{
		"production without PORT":          {"ENVIRONMENT": "production", "PUBLIC_ORIGIN": testOrigin},
		"production without PUBLIC_ORIGIN": {"ENVIRONMENT": "production", "PORT": "8080"},
		"unknown environment":              {"ENVIRONMENT": "staging"},
		"plain HTTP origin":                {"PUBLIC_ORIGIN": "http://comfy-proxy.regiellis.com"},
	} {
		if _, err := readSettings(env(vars), free); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := readSettings(env(nil), func(string, int) bool { return false }); err == nil {
		t.Error("no free port: accepted")
	}
}
