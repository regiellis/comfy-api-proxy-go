// Command site serves the comfy-api-proxy-go landing page and documentation.
// The documentation is the repository's docs/*.md, embedded at build time.
package main

import (
	"embed"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/regiellis/hyperchi/hyperchi"
	"github.com/regiellis/hyperchi/hyperchi/config"
	"github.com/regiellis/hyperchi/modules/markdown"
)

// Embedding keeps the binary self-contained: no template or asset directory
// at runtime, and no directory listings or symlink escapes.
//
//go:embed static
var publicAssets embed.FS

//go:embed templates/*.tmpl
var templateFiles embed.FS

// releaseVersion is stamped into the binary for deployment verification.
var releaseVersion = "development"

const (
	repoURL = "https://github.com/regiellis/comfy-api-proxy-go"
	// devPortBase is where a development run starts looking for a free port.
	devPortBase = 8090
	devPortScan = 100
)

// settings is the server's environment, read once at startup.
type settings struct {
	host, port, publicOrigin string
	production               bool
}

// readSettings reads HOST, PORT, PUBLIC_ORIGIN, and ENVIRONMENT. Production
// needs PORT and PUBLIC_ORIGIN; development binds 0.0.0.0 and, without PORT,
// takes the first free port from 8090 up.
func readSettings(getenv func(string) string, free func(host string, port int) bool) (settings, error) {
	s := settings{
		host:         getenv("HOST"),
		port:         getenv("PORT"),
		publicOrigin: getenv("PUBLIC_ORIGIN"),
	}
	switch env := getenv("ENVIRONMENT"); env {
	case "production":
		s.production = true
	case "", "development":
	default:
		return s, fmt.Errorf("ENVIRONMENT must be development or production, not %q", env)
	}
	if s.host == "" {
		s.host = "0.0.0.0"
	}
	if s.production {
		if s.port == "" {
			return s, errors.New("PORT is required when ENVIRONMENT=production")
		}
		if s.publicOrigin == "" {
			return s, errors.New("PUBLIC_ORIGIN is required when ENVIRONMENT=production")
		}
	}
	if s.publicOrigin != "" {
		if err := validOrigin(s.publicOrigin); err != nil {
			return s, err
		}
	}
	if s.port == "" {
		for p := devPortBase; p < devPortBase+devPortScan; p++ {
			if free(s.host, p) {
				s.port = strconv.Itoa(p)
				break
			}
		}
		if s.port == "" {
			return s, fmt.Errorf("no free port from %d to %d; set PORT", devPortBase, devPortBase+devPortScan-1)
		}
	}
	return s, nil
}

// portFree reports whether host:port can be bound right now.
func portFree(host string, port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	// The probe listener is closed at once; a failed close leaves the port in
	// doubt, so it counts as taken.
	return ln.Close() == nil
}

func main() {
	s, err := readSettings(os.Getenv, portFree)
	if err != nil {
		log.Fatal(err)
	}
	policy, err := newSitePolicy(s.host, s.port, s.publicOrigin)
	if err != nil {
		log.Fatal(err)
	}
	// The framework always runs with production behavior, even in a dev run.
	// Some subsystems read ENVIRONMENT or GO_ENV instead of Config, so set both.
	for _, key := range []string{"ENVIRONMENT", "GO_ENV"} {
		if err := os.Setenv(key, "production"); err != nil {
			log.Fatal(err)
		}
	}
	app, err := newApp(s.publicOrigin, policy)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("comfy-api-proxy-go site %s", releaseVersion)
	// HyperChi logs every local URL it listens on (bridges skipped). In
	// production only PUBLIC_ORIGIN and loopback get past the Host check.
	if s.publicOrigin != "" {
		log.Printf("public origin %s", s.publicOrigin)
	}
	if err := app.Serve(net.JoinHostPort(s.host, s.port)); err != nil {
		log.Fatal(err)
	}
}

// newApp builds the application: middleware, templates, static files, and
// every route. publicOrigin is the validated PUBLIC_ORIGIN, or "" in
// development. A nil policy leaves the Host check out, for tests that exercise
// the content alone.
func newApp(publicOrigin string, policy *sitePolicy) (*hyperchi.HyperChi, error) {
	cfg := config.Production()
	cfg.Database.Name = ":memory:"
	app := hyperchi.NewWithConfig(cfg)
	// Middleware goes on before any route; chi refuses it afterwards.
	if policy != nil {
		app.Use(policy.middleware(app))
	}
	app.Use(app.Middleware.Recovery())
	if err := setupRoutes(app, publicOrigin); err != nil {
		return nil, errors.Join(err, app.CloseDB())
	}
	return app, nil
}

func setupRoutes(app *hyperchi.HyperChi, publicOrigin string) error {
	// The embedded static files. Pages link them with {{asset "/static/..."}},
	// which adds a content version (?v=) that StaticFS answers with a one-year
	// immutable Cache-Control, so a changed file gets a new URL.
	static, err := fs.Sub(publicAssets, "static")
	if err != nil {
		return err
	}
	if err := app.StaticFS("/static/*", static, hyperchi.StaticOptions{}); err != nil {
		return err
	}
	if err := app.LoadTemplatesFS(templateFiles, "templates/*.tmpl"); err != nil {
		return err
	}
	app.SetGlobal("repo", repoURL)
	app.SetGlobal("ogImage", absoluteURL(publicOrigin, "/static/img/og.png"))
	app.OnError(siteError)

	docs, err := loadDocs(docsFiles)
	if err != nil {
		return err
	}
	bench, err := docs.benchTable(markdown.New())
	if err != nil {
		return err
	}

	healthz := func(c *hyperchi.Context) error {
		return c.SetHeader("Cache-Control", "no-store").Text("ok\n")
	}
	app.Get("/healthz", healthz)
	app.Head("/healthz", healthz)

	app.Get("/", func(c *hyperchi.Context) error {
		return c.Page("index.tmpl", hyperchi.H{
			"title":       "comfy-api-proxy-go | The Comfy API v2 for your own ComfyUI",
			"description": docs.bySlug[firstSlug].Description,
			"canonical":   canonical(publicOrigin, "/"),
			"current":     "home", "topic": "",
			"sections": docs.sections,
			"bench":    bench,
		})
	})
	setupDocs(app, docs, publicOrigin)

	sitemap, err := sitemapXML(publicOrigin, docs)
	if err != nil {
		return err
	}
	app.Get("/sitemap.xml", func(c *hyperchi.Context) error {
		return c.Blob("application/xml; charset=utf-8", sitemap)
	})
	robots := "User-agent: *\nAllow: /\n"
	if publicOrigin != "" {
		robots += "\nSitemap: " + publicOrigin + "/sitemap.xml\n"
	}
	app.Get("/robots.txt", func(c *hyperchi.Context) error { return c.Text(robots) })
	return nil
}

type sitemapURL struct {
	Loc string `xml:"loc"`
}

type urlSet struct {
	XMLName xml.Name     `xml:"urlset"`
	XMLNS   string       `xml:"xmlns,attr"`
	URLs    []sitemapURL `xml:"url"`
}

// sitemapXML lists the landing page and every docs page. Sitemaps need
// absolute URLs, so a dev run without PUBLIC_ORIGIN lists paths only.
func sitemapXML(origin string, docs *docSet) ([]byte, error) {
	set := urlSet{XMLNS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
	set.URLs = append(set.URLs, sitemapURL{Loc: absoluteURL(origin, "/")})
	for _, p := range docs.pages {
		set.URLs = append(set.URLs, sitemapURL{Loc: absoluteURL(origin, p.Path)})
	}
	body, err := xml.MarshalIndent(set, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), append(body, '\n')...), nil
}

// siteError renders the errors a browser sees, router misses and site policy
// refusals included, as a page in the site's design. htmx and JSON clients
// keep the framework default, so an htmx target never receives a whole page.
func siteError(c *hyperchi.Context, err error) {
	accept := c.Header("Accept")
	if c.IsHTMX() || (strings.Contains(accept, "application/json") && !strings.Contains(accept, "text/html")) {
		c.App().DefaultError(c, err)
		return
	}
	code, message := http.StatusInternalServerError, "Something went wrong on the server. Try again in a moment."
	var he *hyperchi.HTTPError
	if errors.As(err, &he) {
		code = he.Code
		if code < http.StatusInternalServerError {
			message = he.Message
		}
	}
	if code >= http.StatusInternalServerError {
		log.Printf("site: %s %s: %v", c.Method(), c.Path(), err)
	}
	heading := http.StatusText(code)
	if code == http.StatusNotFound {
		heading, message = "No page at this address", "The link may be old, or the address mistyped. The documentation starts at the introduction, and search covers every page."
	}
	if rerr := c.Status(code).Page("error-page.tmpl", hyperchi.H{
		"title":       heading + " | comfy-api-proxy-go",
		"description": message,
		"status":      code,
		"current":     "error", "topic": "",
		"heading": heading,
		"message": message,
	}); rerr != nil {
		log.Printf("site: error page: %v", rerr)
		c.App().DefaultError(c, err)
	}
}
