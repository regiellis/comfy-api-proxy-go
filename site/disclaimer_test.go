package main

import (
	"html"
	"net/http"
	"strings"
	"testing"
)

// The site echoes Comfy Org's palette, so every page must say plainly that
// it is independent: a notice above the header and the full line in the
// footer, on the landing page, every docs page, search, and error pages.
const (
	footerDisclaimer = "comfy-api-proxy-go is an independent, community Go port of Comfy-Org's comfy-api-proxy. It is not an official ComfyUI or Comfy Org project and is not affiliated with or endorsed by Comfy Org. Comfy and ComfyUI are trademarks of Comfy Org."
	noticeLine       = "Independent project. Not affiliated with or endorsed by Comfy Org."
	heroLine         = "It is not an official ComfyUI or Comfy Org project and is not affiliated with or endorsed by Comfy Org."
)

func TestDisclaimerOnEveryPage(t *testing.T) {
	docs := testDocs(t)
	app := siteApp(t, testOrigin)
	paths := []string{"/", "/docs/search", "/docs/search?q=token", "/nope"}
	for _, p := range docs.pages {
		paths = append(paths, p.Path)
	}
	for _, p := range paths {
		w := get(t, app, p, false)
		body := html.UnescapeString(w.Body.String())
		if !strings.Contains(body, `<p class="footer-disclaimer">`+footerDisclaimer+`</p>`) {
			t.Errorf("%s: footer disclaimer missing", p)
		}
		if !strings.Contains(body, `<p class="notice-bar" role="note">`+noticeLine+`</p>`) {
			t.Errorf("%s: top notice missing", p)
		}
		// The notice sits above the header, so it is the first thing read.
		if n, h := strings.Index(body, `class="notice-bar"`), strings.Index(body, `<header`); n < 0 || h < 0 || n > h {
			t.Errorf("%s: notice is not above the header", p)
		}
	}
	landing := html.UnescapeString(get(t, app, "/", false).Body.String())
	hero, end := strings.Index(landing, `class="hero"`), strings.Index(landing, `class="install"`)
	if hero < 0 || end < hero || !strings.Contains(landing[hero:end], heroLine) {
		t.Error("landing hero lacks the independence statement")
	}
}

func TestFontsAndLicensesAreServed(t *testing.T) {
	app := siteApp(t, "")
	for _, p := range []string{
		"/static/fonts/inter/InterVariable.woff2",
		"/static/fonts/inter/Inter-Italic.woff2",
		"/static/fonts/archivo/ArchivoCondensed-SemiBold.woff2",
		"/static/fonts/archivo/ArchivoCondensed-Bold.woff2",
	} {
		w := get(t, app, p, false)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "font/woff2" || !strings.HasPrefix(w.Body.String(), "wOF2") {
			t.Errorf("%s: %d %s", p, w.Code, w.Header().Get("Content-Type"))
		}
	}
	for _, p := range []string{"/static/fonts/inter/OFL.txt", "/static/fonts/archivo/OFL.txt"} {
		if w := get(t, app, p, false); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "SIL Open Font License, Version 1.1") {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	page := get(t, app, "/", false).Body.String()
	if !strings.Contains(page, `<link rel="preload" href="/static/fonts/inter/InterVariable.woff2" as="font" type="font/woff2" crossorigin>`) {
		t.Error("the main face is not preloaded")
	}
}
