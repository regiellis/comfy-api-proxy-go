package server

import (
	"crypto/subtle"
	"github.com/regiellis/comfy-api-proxy-go/internal/httpx"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// The chain, outermost first: CORS, then the origin guard, then bearer auth,
// then the router. Every request passes through all of it, unrouted paths
// included, as in the reference.

const (
	corsAllowMethods  = "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders  = "Authorization, Content-Type, Idempotency-Key"
	corsExposeHeaders = "Retry-After, Content-Range, Accept-Ranges"
	corsMaxAge        = "600"
)

func originSet(origins []string) map[string]bool {
	set := make(map[string]bool, len(origins))
	for _, o := range origins {
		set[o] = true
	}
	return set
}

// cors answers preflight and attaches CORS headers for allowlisted origins.
// Headers go on before the inner handler runs, so a 401 or 403 further in
// still carries them and the browser can read the error body. Origins are
// matched exactly against the normalized allowlist, never with a wildcard.
func cors(allowed map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" || !allowed[origin] {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		addVary(h, "Origin")
		h.Set("Access-Control-Allow-Methods", corsAllowMethods)
		h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
		h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
		h.Set("Access-Control-Allow-Credentials", "true")
		h.Set("Access-Control-Max-Age", corsMaxAge)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func addVary(h http.Header, value string) {
	for _, line := range h.Values("Vary") {
		for part := range strings.SplitSeq(line, ",") {
			if strings.EqualFold(strings.TrimSpace(part), value) {
				return
			}
		}
	}
	h.Add("Vary", value)
}

// originGuard is ComfyUI core's create_origin_only_middleware, ported via the
// reference. It stops drive-by CSRF from pages the user has open, which can
// script requests at a loopback server freely:
//
//  1. Sec-Fetch-Site: cross-site is rejected outright. The browser sets it and
//     page script cannot forge it.
//  2. When Host names a loopback address and Origin is present, their hosts
//     must match. A port is compared only when both sides carry one. Non-
//     loopback Hosts are exempt because a reverse proxy may rewrite Host.
//
// No Origin at all (curl, SDKs) passes, since browsers always send one on the
// cross-site requests this exists to stop. Allowlisted origins skip both
// checks.
func originGuard(allowed map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && allowed[origin] {
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			httpx.WriteError(w, http.StatusForbidden, "forbidden_origin",
				"Cross-site request blocked (Sec-Fetch-Site: cross-site).", nil)
			return
		}

		if r.Host != "" && origin != "" && originMismatch(r, origin) {
			httpx.WriteError(w, http.StatusForbidden, "forbidden_origin",
				"Origin '"+origin+"' does not match request host '"+r.Host+"'.", nil)
			return
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originMismatch reports whether a loopback Host disagrees with Origin. It
// follows the reference's string comparison exactly, including comparing the
// Origin authority as written, so an IPv6 origin without a port
// ("http://[::1]") does not match "[::1]:8189". That quirk fails closed.
// Unparseable values also fail closed.
func originMismatch(r *http.Request, origin string) bool {
	host, err := url.Parse("//" + strings.ToLower(r.Host))
	if err != nil {
		return true
	}
	if !isLoopbackHost(r, host.Hostname()) {
		return false
	}
	o, err := url.Parse(origin)
	if err != nil {
		return true
	}

	originDomain := strings.ToLower(o.Host)
	if o.User != nil {
		originDomain = strings.ToLower(o.User.String()) + "@" + originDomain
	}
	hostDomain := strings.ToLower(r.Host)
	if o.Port() == "" {
		hostDomain = host.Hostname()
	}
	if host.Port() == "" {
		originDomain = strings.ToLower(o.Hostname())
	}
	return hostDomain != "" && originDomain != "" && hostDomain != originDomain
}

// isLoopbackHost treats a literal loopback IP as loopback, and a name as
// loopback when every address it resolves to is.
func isLoopbackHost(r *http.Request, name string) bool {
	if name == "" {
		return false
	}
	if ip := net.ParseIP(name); ip != nil {
		return ip.IsLoopback()
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(r.Context(), name)
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if !a.IP.IsLoopback() {
			return false
		}
	}
	return true
}

// bearerAuth gates /api/v2/* behind a static token, compared in constant
// time. Health stays open so a supervisor can probe without the secret, and
// OPTIONS passes because a CORS preflight never carries Authorization.
func bearerAuth(token string, next http.Handler) http.Handler {
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/health" || !strings.HasPrefix(r.URL.Path, "/api/v2/") || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		scheme, presented, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "bearer") || presented == "" {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Missing or malformed Authorization header.", nil)
			return
		}
		if subtle.ConstantTimeCompare([]byte(presented), want) != 1 {
			httpx.WriteError(w, http.StatusUnauthorized, "unauthorized", "Invalid token.", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}
