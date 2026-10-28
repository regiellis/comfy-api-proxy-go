package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/regiellis/hyperchi/hyperchi"
)

const testOrigin = "https://comfy-proxy.regiellis.com"

func testDocs(t *testing.T) *docSet {
	t.Helper()
	docs, err := loadDocs(docsFiles)
	if err != nil {
		t.Fatal(err)
	}
	return docs
}

// siteApp builds the whole site without the Host policy, so requests from
// httptest (Host example.com) reach the routes.
func siteApp(t *testing.T, origin string) *hyperchi.HyperChi {
	t.Helper()
	app, err := newApp(origin, nil)
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

func get(t *testing.T, app http.Handler, target string, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Header.Set("Accept", "text/html")
	if htmx {
		r.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	app.ServeHTTP(w, r)
	return w
}

func TestEveryDocsPageRenders(t *testing.T) {
	docs := testDocs(t)
	files, err := fs.Glob(docsFiles, "docs/*.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs.pages) != len(files) || len(files) == 0 {
		t.Fatalf("got %d pages from %d files", len(docs.pages), len(files))
	}
	app := siteApp(t, testOrigin)
	for i, page := range docs.pages {
		t.Run(page.Slug, func(t *testing.T) {
			w := get(t, app, page.Path, false)
			body := w.Body.String()
			if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
				t.Fatalf("status %d, type %s", w.Code, w.Header().Get("Content-Type"))
			}
			title := strings.NewReplacer("&", "&amp;", "'", "&#39;").Replace(page.Title)
			for _, want := range []string{
				"<title>" + title + " | comfy-api-proxy-go</title>",
				`<meta name="description" content="`,
				`<link rel="canonical" href="` + testOrigin + page.Path + `">`,
				`<meta property="og:image" content="` + testOrigin + `/static/img/og.png">`,
				"<h1>" + title + "</h1>",
				`<a class="skip-link" href="#main">`, `<main id="main"`,
				`<a href="` + page.Path + `" aria-current="page">`,
				`<link rel="alternate" type="text/markdown" href="` + page.MarkdownPath + `">`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q", want)
				}
			}
			for _, h := range page.TOC {
				if !strings.Contains(body, `<a href="#`+h.ID+`">`) {
					t.Errorf("on this page misses #%s", h.ID)
				}
			}
			for _, other := range docs.pages {
				if !strings.Contains(body, `<a href="`+other.Path+`"`) {
					t.Errorf("sidebar does not link %s", other.Path)
				}
			}
			// Prev and next follow reading order, from the front matter.
			if i > 0 && !strings.Contains(body, `href="`+docs.pages[i-1].Path+`" rel="prev"`) {
				t.Errorf("no previous link to %s", docs.pages[i-1].Path)
			}
			if i < len(docs.pages)-1 && !strings.Contains(body, `href="`+docs.pages[i+1].Path+`" rel="next"`) {
				t.Errorf("no next link to %s", docs.pages[i+1].Path)
			}
			for _, banned := range []string{"\u2014", "\u2013", `href="#"`, "raw HTML omitted", ` style="`, "<style"} {
				if strings.Contains(body, banned) {
					t.Errorf("page contains %q", banned)
				}
			}
			if m := relativeMD.FindString(body); m != "" {
				t.Errorf("relative Markdown link left unrewritten: %s", m)
			}
			if pre, buttons := strings.Count(body, "<pre>"), strings.Count(body, `class="copy-code"`); pre != buttons {
				t.Errorf("%d code blocks, %d copy buttons", pre, buttons)
			}
		})
	}
	// Reading order follows the sidebar sections.
	var order []string
	for _, p := range docs.pages {
		order = append(order, p.Slug)
	}
	if order[0] != "introduction" || order[len(order)-1] != "troubleshooting" {
		t.Errorf("reading order %v", order)
	}
}

var (
	relativeMD = regexp.MustCompile(`href="[a-z0-9-]+\.md`)
	hrefAttr   = regexp.MustCompile(`href="(/[^"#]*)?(#[^"]*)?"`)
	idAttr     = regexp.MustCompile(`id="([^"]+)"`)
)

// TestEveryInternalLinkResolves crawls every page on the site and follows
// each same-site link: the target must answer 200, and a fragment must name
// an id on the target page.
func TestEveryInternalLinkResolves(t *testing.T) {
	docs := testDocs(t)
	app := siteApp(t, "")
	pages := map[string]string{} // path -> body
	fetch := func(p string) (string, int) {
		if body, ok := pages[p]; ok {
			return body, http.StatusOK
		}
		w := get(t, app, p, false)
		if w.Code == http.StatusOK {
			pages[p] = w.Body.String()
		}
		return w.Body.String(), w.Code
	}
	start := []string{"/", "/docs/search?q=job"}
	for _, p := range docs.pages {
		start = append(start, p.Path)
	}
	checked := 0
	for _, from := range start {
		body, code := fetch(from)
		if code != http.StatusOK {
			t.Fatalf("%s: %d", from, code)
		}
		for _, m := range hrefAttr.FindAllStringSubmatch(body, -1) {
			target, fragment := m[1], strings.TrimPrefix(m[2], "#")
			if target == "" {
				target = strings.SplitN(from, "?", 2)[0]
			}
			if strings.HasPrefix(target, "//") {
				continue // protocol-relative: not this site
			}
			targetBody, code := fetch(target)
			if code != http.StatusOK {
				t.Errorf("%s links %s: %d", from, target, code)
				continue
			}
			if fragment != "" && !strings.Contains(targetBody, `id="`+fragment+`"`) {
				t.Errorf("%s links %s#%s: no such id", from, target, fragment)
			}
			checked++
		}
	}
	if checked < 200 {
		t.Errorf("only %d links checked; the crawl is not reaching the pages", checked)
	}
	// Every id on a docs page is unique, so a fragment has one target.
	for _, p := range docs.pages {
		seen := map[string]bool{}
		for _, m := range idAttr.FindAllStringSubmatch(pages[p.Path], -1) {
			if seen[m[1]] {
				t.Errorf("%s: id %q used twice", p.Path, m[1])
			}
			seen[m[1]] = true
		}
	}
}

func TestRelativeMarkdownLinksBecomeSiteURLs(t *testing.T) {
	body := get(t, siteApp(t, ""), "/docs/quickstart", false).Body.String()
	for _, want := range []string{`href="/docs/api#submit"`, `href="/docs/events"`, `href="/docs/assets#output-ids"`} {
		if !strings.Contains(body, want) {
			t.Errorf("quickstart lacks %s", want)
		}
	}
	md := get(t, siteApp(t, ""), "/docs/quickstart.md", false).Body.String()
	if !strings.Contains(md, "(api.md#submit)") {
		t.Error("the Markdown source no longer carries its relative links")
	}
}

func TestDocsIndexRedirects(t *testing.T) {
	w := get(t, siteApp(t, ""), "/docs", false)
	if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "/docs/introduction" {
		t.Fatalf("/docs: %d to %q", w.Code, w.Header().Get("Location"))
	}
}

func TestLandingPage(t *testing.T) {
	w := get(t, siteApp(t, testOrigin), "/", false)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	for _, want := range []string{
		`<link rel="canonical" href="` + testOrigin + `/">`,
		"go install github.com/regiellis/comfy-api-proxy-go/cmd/comfy-api-proxy@latest",
		`href="https://github.com/regiellis/comfy-api-proxy-go"`,
		"356k req/s", "233 MB", // the results table, rendered from benchmarks.md
		`<a href="/docs/introduction">Introduction</a>`,
		`<a class="brand" href="/" aria-label="comfy-api-proxy-go home" aria-current="page">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("landing page missing %q", want)
		}
	}
	// The Open Graph image and favicon are real files, not just meta tags.
	for path, kind := range map[string]string{"/static/img/og.png": "image/png", "/static/img/favicon.svg": "image/svg+xml"} {
		if w := get(t, siteApp(t, ""), path, false); w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), kind) {
			t.Errorf("%s: %d %s", path, w.Code, w.Header().Get("Content-Type"))
		}
	}
	// Every number the landing page states in prose comes from the docs.
	docs := testDocs(t)
	sources := docs.bySlug["introduction"].Markdown + docs.bySlug["benchmarks"].Markdown
	for _, claim := range []string{"6 to 12 times", "2003 MB/s", "815 MB/s", "82 of 82", "p99 397 ms against 348 ms", "2026-10-28", "amd64, 24 threads", "Medians of 3 runs"} {
		if !strings.Contains(body, claim) && !strings.Contains(body, strings.ReplaceAll(claim, "'", "&#39;")) {
			t.Errorf("landing page lost the claim %q", claim)
		}
		if !strings.Contains(sources, claim) {
			t.Errorf("landing page claim %q is not in the docs", claim)
		}
	}
	for _, banned := range []string{"\u2014", "\u2013", `href="#"`, ` style="`, "<style"} {
		if strings.Contains(body, banned) {
			t.Errorf("landing page contains %q", banned)
		}
	}
}

func TestLLMSIndexListsEveryPage(t *testing.T) {
	docs := testDocs(t)
	w := get(t, siteApp(t, testOrigin), "/llms.txt", false)
	body := w.Body.String()
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("status %d, type %s", w.Code, w.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(body, "# comfy-api-proxy-go\n\n> ") || !strings.Contains(body, "\n## Docs\n\n") || !strings.Contains(body, "\n## Optional\n\n") {
		t.Fatalf("not llms.txt shaped:\n%s", body)
	}
	if strings.Count(body, "\n# ") != 0 || strings.Count(body, "\n## ") != 2 {
		t.Error("llms.txt has headings beyond the H1 and two H2 sections")
	}
	for _, page := range docs.pages {
		link := "- [" + page.Title + "](" + testOrigin + "/docs/" + page.Slug + ".md): "
		if !strings.Contains(body, link) {
			t.Errorf("llms.txt missing %s", link)
		}
	}
	if !strings.Contains(body, "("+testOrigin+"/llms-full.txt)") {
		t.Error("llms.txt does not link llms-full.txt")
	}
	dev := get(t, siteApp(t, ""), "/llms.txt", false).Body.String()
	if !strings.Contains(dev, "- [API reference](/docs/api.md): ") {
		t.Error("development llms.txt is not relative")
	}
}

func TestLLMSFullContainsEveryPage(t *testing.T) {
	docs := testDocs(t)
	body := get(t, siteApp(t, testOrigin), "/llms-full.txt", false).Body.String()
	last := -1
	for _, page := range docs.pages {
		at := strings.Index(body, page.Markdown)
		if at < 0 {
			t.Errorf("llms-full.txt missing %s", page.Slug)
			continue
		}
		if at < last {
			t.Errorf("%s is out of reading order", page.Slug)
		}
		last = at
	}
	if !strings.Contains(body, "Source: "+testOrigin+"/docs/api") {
		t.Error("llms-full.txt lacks page sources")
	}
}

func TestMarkdownSourceRoutes(t *testing.T) {
	app := siteApp(t, testOrigin)
	w := get(t, app, "/docs/api.md", false)
	body := w.Body.String()
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/markdown; charset=utf-8" {
		t.Fatalf("status %d, type %s", w.Code, w.Header().Get("Content-Type"))
	}
	if !strings.HasPrefix(body, "# API reference\n\nEvery route the proxy serves") {
		t.Errorf("api.md starts %q", body[:min(len(body), 80)])
	}
	if strings.Contains(body, "title:") || strings.Contains(body, "<h2") {
		t.Error("api.md leaks front matter or HTML")
	}
	intro := get(t, app, "/docs/introduction.md", false).Body.String()
	if index := get(t, app, "/docs/index.md", false).Body.String(); index != intro {
		t.Error("/docs/index.md does not serve the introduction")
	}
	if get(t, app, "/docs/missing.md", false).Code != http.StatusNotFound {
		t.Error("unknown Markdown page did not 404")
	}
}

func TestUnknownPagesAre404(t *testing.T) {
	app := siteApp(t, "")
	for _, p := range []string{"/docs/missing", "/nope", "/docs/introduction/extra", "/static/none.css"} {
		w := get(t, app, p, false)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, w.Code)
			continue
		}
		body := w.Body.String()
		for _, want := range []string{"<title>No page at this address | comfy-api-proxy-go</title>", `<meta name="robots" content="noindex">`, `href="/docs/introduction"`} {
			if p != "/static/none.css" && !strings.Contains(body, want) {
				t.Errorf("%s: 404 page lacks %q", p, want)
			}
		}
	}
}

func TestDocsSearch(t *testing.T) {
	app := siteApp(t, testOrigin)
	frag := get(t, app, "/docs/search?q=token", true)
	body := frag.Body.String()
	if frag.Code != 200 || strings.Contains(body, "<html") {
		t.Fatalf("status %d, fragment %q", frag.Code, body)
	}
	if !strings.Contains(body, `href="/docs/remote-access#`) || !strings.Contains(body, "<mark>token</mark>") {
		t.Errorf("search for token misses remote access or the highlight:\n%s", body)
	}
	top := get(t, app, "/docs/search?q=Event+stream", true).Body.String()
	first := strings.Index(top, `<li><a href="`)
	if first < 0 || !strings.HasPrefix(top[first:], `<li><a href="/docs/events">`) {
		t.Errorf("the page title is not the top result:\n%s", top)
	}
	page := get(t, app, "/docs/search?q=token", false).Body.String()
	if !strings.Contains(page, "<html") || !strings.Contains(page, `value="token"`) {
		t.Error("full-page search lacks the layout or keeps no query")
	}
	hostile := get(t, app, "/docs/search?q=%3Cscript%3Ealert(1)%3C/script%3E", true).Body.String()
	if strings.Contains(hostile, "<script>alert") || !strings.Contains(hostile, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("hostile query not escaped: %s", hostile)
	}
	if empty := get(t, app, "/docs/search?q=", true); empty.Code != 200 || empty.Body.Len() != 0 {
		t.Errorf("empty htmx query: %d %q", empty.Code, empty.Body.String())
	}
	if hint := get(t, app, "/docs/search", false).Body.String(); !strings.Contains(hint, `class="search-hint"`) {
		t.Error("empty full-page search has no hint")
	}
	if none := get(t, app, "/docs/search?q=zzqxv", true).Body.String(); !strings.Contains(none, `class="search-empty"`) || !strings.Contains(none, "zzqxv") {
		t.Error("no-result state missing or does not name the query")
	}
	if got := []rune(boundQuery(strings.Repeat("é", 500))); len(got) != maxQueryRunes {
		t.Errorf("bounded query has %d characters", len(got))
	}
}

func TestSitemapAndRobots(t *testing.T) {
	docs := testDocs(t)
	app := siteApp(t, testOrigin)
	w := get(t, app, "/sitemap.xml", false)
	body := w.Body.String()
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/xml") {
		t.Fatalf("status %d, type %s", w.Code, w.Header().Get("Content-Type"))
	}
	if !strings.Contains(body, "<loc>"+testOrigin+"/</loc>") {
		t.Error("sitemap lacks the landing page")
	}
	for _, p := range docs.pages {
		if !strings.Contains(body, "<loc>"+testOrigin+p.Path+"</loc>") {
			t.Errorf("sitemap lacks %s", p.Path)
		}
	}
	robots := get(t, app, "/robots.txt", false).Body.String()
	if !strings.Contains(robots, "Sitemap: "+testOrigin+"/sitemap.xml") {
		t.Errorf("robots.txt: %q", robots)
	}
}

func TestLoadDocsRejectsBrokenFiles(t *testing.T) {
	good := "---\ntitle: Introduction\nsection: Start\norder: 10\ndescription: d\n---\n\n## X {#x}\n\nSee [x](#x).\n"
	page := func(src string) fstest.MapFS { return fstest.MapFS{"docs/introduction.md": {Data: []byte(src)}} }
	for name, files := range map[string]fstest.MapFS{
		"no title":         page(strings.Replace(good, "title: Introduction\n", "", 1)),
		"bad order":        page(strings.Replace(good, "order: 10", "order: ten", 1)),
		"unknown section":  page(strings.Replace(good, "section: Start", "section: Misc", 1)),
		"broken next":      page(strings.Replace(good, "order: 10", "order: 10\nnext: nowhere", 1)),
		"broken page link": page(good + "\n[a](nowhere.md)\n"),
		"broken anchor":    page(good + "\n[a](introduction.md#nowhere)\n"),
		"broken same-page": page(good + "\n[a](#nowhere)\n"),
		"duplicate id":     page(good + "\n## Y {#x}\n"),
		"no introduction":  {"docs/other.md": {Data: []byte(good)}},
		"dup order": {
			"docs/introduction.md": {Data: []byte(good)},
			"docs/b.md":            {Data: []byte(good)},
		},
		"empty": {},
	} {
		if _, err := loadDocs(files); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := loadDocs(page(good)); err != nil {
		t.Fatal(err)
	}
}
