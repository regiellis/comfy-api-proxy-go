package main

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/regiellis/hyperchi/hyperchi"
)

// contentSecurityPolicy allows only this origin's own scripts, styles,
// images, and requests. htmx runs without eval because the site uses no hx-on
// attributes, so script-src carries no 'unsafe-eval'.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// sitePolicy answers only the hosts it knows and sets the security headers on
// every response. The site is read-only: GET and HEAD are the only methods.
type sitePolicy struct {
	origins map[string]string // exact Host -> canonical browser origin
}

// Origins returns every origin the policy answers, sorted. A bind to 0.0.0.0
// listens on each interface, but only these hosts get past the Host check.
func (p *sitePolicy) Origins() []string {
	origins := make([]string, 0, len(p.origins))
	for _, o := range p.origins {
		origins = append(origins, o)
	}
	slices.Sort(origins)
	return origins
}

// interfaceAddrs lists this machine's addresses; tests replace it.
var interfaceAddrs = net.InterfaceAddrs

// machineHosts returns bind itself, or for an unspecified bind (0.0.0.0, ::)
// every interface address except link-local ones, which a browser cannot use
// without a zone.
func machineHosts(bind net.IP) ([]string, error) {
	if !bind.IsUnspecified() {
		return []string{bind.String()}, nil
	}
	addrs, err := interfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("list this machine's addresses: %w", err)
	}
	var hosts []string
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() || ipNet.IP.IsLinkLocalUnicast() {
			continue
		}
		hosts = append(hosts, ipNet.IP.String())
	}
	return hosts, nil
}

// validOrigin reports whether publicOrigin is an HTTPS origin with no path,
// query, fragment, or credentials.
func validOrigin(publicOrigin string) error {
	u, err := url.Parse(publicOrigin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("PUBLIC_ORIGIN must be an HTTPS origin without a path, e.g. https://comfy-proxy.regiellis.com")
	}
	return nil
}

func newSitePolicy(host, port, publicOrigin string) (*sitePolicy, error) {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("PORT must be 1 through 65535")
	}
	p := &sitePolicy{origins: make(map[string]string)}
	ip := net.ParseIP(host)
	if host != "localhost" && ip == nil {
		return nil, fmt.Errorf("HOST must be localhost or an IP address")
	}
	for _, local := range []string{"localhost", "127.0.0.1", "::1"} {
		authority := net.JoinHostPort(local, port)
		p.origins[authority] = "http://" + authority
	}
	// Without PUBLIC_ORIGIN this is a dev run: answer plain HTTP on the address
	// it binds, or on every address of this machine for 0.0.0.0, so a client on
	// the LAN or the tailnet can open it. Production sets PUBLIC_ORIGIN and
	// answers only that origin and loopback (the container health check).
	if publicOrigin == "" && host != "localhost" && !ip.IsLoopback() {
		hosts, err := machineHosts(ip)
		if err != nil {
			return nil, err
		}
		for _, h := range hosts {
			authority := net.JoinHostPort(h, port)
			p.origins[authority] = "http://" + authority
		}
	}
	if publicOrigin != "" {
		if err := validOrigin(publicOrigin); err != nil {
			return nil, err
		}
		u, err := url.Parse(publicOrigin)
		if err != nil {
			return nil, err
		}
		p.origins[u.Host] = publicOrigin
	}
	return p, nil
}

// middleware enforces the policy in front of app. Every refusal renders
// through app's error pipeline, so a browser sees the site's error page.
func (p *sitePolicy) middleware(app *hyperchi.HyperChi) func(http.Handler) http.Handler {
	deny := func(w http.ResponseWriter, r *http.Request, code int, msg string) {
		app.RespondStatus(w, r, code, hyperchi.NewHTTPError(code, msg))
	}
	return func(next http.Handler) http.Handler {
		return p.handler(next, deny)
	}
}

func (p *sitePolicy) handler(next http.Handler, deny func(http.ResponseWriter, *http.Request, int, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Release", releaseVersion)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		origin, allowed := p.origins[r.Host]
		if !allowed {
			deny(w, r, http.StatusMisdirectedRequest, "Unrecognized host")
			return
		}
		if strings.HasPrefix(origin, "https://") {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if len(r.URL.RawQuery) > 2048 {
			deny(w, r, http.StatusRequestURITooLong, "Query too large")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			deny(w, r, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && o != origin {
			deny(w, r, http.StatusForbidden, "Cross-origin request denied")
			return
		}
		// No debug metadata or framework development endpoints are public.
		if strings.HasPrefix(r.URL.Path, "/__hyperchi/") || r.URL.Path == "/htmx/extensions" {
			deny(w, r, http.StatusNotFound, "Not found")
			return
		}
		next.ServeHTTP(w, r)
	})
}
