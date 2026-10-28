package main

import (
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/regiellis/hyperchi/hyperchi"
	"github.com/regiellis/hyperchi/modules/markdown"
)

// The documentation is the repository's docs/*.md, copied into site/docs/ by
// `task site:sync` and by the Dockerfile, then embedded so the binary needs no
// content directory at runtime. Each file's front matter holds title, section,
// order (within its section), description, and next (every page but the last).
//
//go:embed docs/*.md
var docsFiles embed.FS

// docSections is the sidebar's group order. Every page names one in its
// front matter; pages within a group follow their order.
var docSections = []string{"Start", "Run", "API", "SDK", "Reference"}

// firstSlug is the page /docs sends a reader to.
const firstSlug = "introduction"

type docSection struct {
	Name  string
	Pages []*docPage
}

// docPage is one rendered documentation page.
type docPage struct {
	Slug, Title, Section, Path, MarkdownPath string
	Description                              string // plain text, for meta tags and llms.txt
	Order                                    int
	Prev, Next                               *docPage
	Body                                     template.HTML
	Markdown                                 string             // the page as served at MarkdownPath
	TOC                                      []markdown.Heading // the page's ## headings, for "On this page"
	ids                                      map[string]bool    // every heading id on the page
	links                                    []docLink          // links to other pages, checked at load
	sections                                 []markdown.Section
}

// docLink is a relative link from one page's Markdown to another page.
type docLink struct{ slug, anchor string }

type docSet struct {
	pages    []*docPage // reading order
	sections []docSection
	bySlug   map[string]*docPage
	index    []searchEntry
}

// pageLink matches the href goldmark renders for a relative Markdown link,
// such as href="api.md#submit". Slugs are lower-case words joined by hyphens.
var pageLink = regexp.MustCompile(`href="([a-z0-9-]+)\.md(#[A-Za-z0-9_-]+)?"`)

// cellAlign matches the inline alignment goldmark writes on table cells. The
// CSP forbids inline styles, so it becomes a class the stylesheet answers.
var cellAlign = regexp.MustCompile(` style="text-align:(left|right|center)"`)

// sameLink matches a link to a heading on the same page, such as href="#jobs".
var sameLink = regexp.MustCompile(`href="#([A-Za-z0-9_-]+)"`)

// loadDocs parses every docs/*.md file once at startup. A missing field, a
// broken next link, or a link to a page or anchor that does not exist is an
// error, so a bad edit fails the tests and the server's boot rather than a
// visitor's request.
func loadDocs(files fs.FS) (*docSet, error) {
	names, err := fs.Glob(files, "docs/*.md")
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("docs: no Markdown files embedded; run task site:sync")
	}
	md := markdown.New()
	set := &docSet{bySlug: make(map[string]*docPage, len(names))}
	orders := map[string]string{} // "section/order" -> slug
	next := map[*docPage]string{}
	for _, name := range names {
		src, err := fs.ReadFile(files, name)
		if err != nil {
			return nil, err
		}
		slug := strings.TrimSuffix(path.Base(name), ".md")
		page, nextSlug, err := parseDocPage(md, slug, src)
		if err != nil {
			return nil, fmt.Errorf("docs/%s.md: %w", slug, err)
		}
		key := page.Section + "/" + strconv.Itoa(page.Order)
		if other, dup := orders[key]; dup {
			return nil, fmt.Errorf("docs/%s.md: order %d in %s already used by %s", slug, page.Order, page.Section, other)
		}
		orders[key] = slug
		set.pages = append(set.pages, page)
		set.bySlug[slug] = page
		next[page] = nextSlug
	}
	// Reading order: sections in sidebar order, then order within a section.
	sort.Slice(set.pages, func(i, j int) bool {
		a, b := set.pages[i], set.pages[j]
		if a.Section != b.Section {
			return slices.Index(docSections, a.Section) < slices.Index(docSections, b.Section)
		}
		return a.Order < b.Order
	})
	if _, ok := set.bySlug[firstSlug]; !ok {
		return nil, fmt.Errorf("docs: no %s.md", firstSlug)
	}
	for page, slug := range next {
		if slug == "" {
			continue
		}
		target, ok := set.bySlug[slug]
		if !ok {
			return nil, fmt.Errorf("docs/%s.md: next page %q does not exist", page.Slug, slug)
		}
		page.Next = target
		if target.Prev != nil {
			return nil, fmt.Errorf("docs/%s.md: %s is already next after %s", page.Slug, slug, target.Prev.Slug)
		}
		target.Prev = page
	}
	for _, page := range set.pages {
		for _, l := range page.links {
			target, ok := set.bySlug[l.slug]
			if l.slug == "" {
				target, ok = page, true
			}
			if !ok {
				return nil, fmt.Errorf("docs/%s.md: link to %s.md, which does not exist", page.Slug, l.slug)
			}
			if l.anchor != "" && !target.ids[l.anchor] {
				return nil, fmt.Errorf("docs/%s.md: link to %s#%s, which has no such heading", page.Slug, target.Slug, l.anchor)
			}
		}
	}
	for _, name := range docSections {
		section := docSection{Name: name}
		for _, page := range set.pages {
			if page.Section == name {
				section.Pages = append(section.Pages, page)
			}
		}
		if len(section.Pages) > 0 {
			set.sections = append(set.sections, section)
		}
	}
	set.index = buildSearchIndex(set.pages)
	return set, nil
}

func parseDocPage(md *markdown.Renderer, slug string, src []byte) (*docPage, string, error) {
	doc, err := md.Parse(src)
	if err != nil {
		return nil, "", err
	}
	page := &docPage{
		Slug:         slug,
		Title:        doc.Meta["title"],
		Section:      doc.Meta["section"],
		Description:  doc.Meta["description"],
		Path:         "/docs/" + slug,
		MarkdownPath: "/docs/" + slug + ".md",
		ids:          map[string]bool{},
	}
	for _, field := range []string{"title", "section", "description", "order"} {
		if doc.Meta[field] == "" {
			return nil, "", fmt.Errorf("front matter needs %q", field)
		}
	}
	if !slices.Contains(docSections, page.Section) {
		return nil, "", fmt.Errorf("section %q is not one of %v", page.Section, docSections)
	}
	if page.Order, err = strconv.Atoi(doc.Meta["order"]); err != nil {
		return nil, "", fmt.Errorf("order: %w", err)
	}
	page.sections = doc.Sections
	for _, h := range doc.Headings {
		if h.ID == "" {
			continue
		}
		if page.ids[h.ID] {
			return nil, "", fmt.Errorf("heading id %q is used twice", h.ID)
		}
		page.ids[h.ID] = true
		if h.Level == 2 {
			page.TOC = append(page.TOC, h)
		}
	}
	body := string(doc.HTML)
	for _, m := range pageLink.FindAllStringSubmatch(body, -1) {
		page.links = append(page.links, docLink{slug: m[1], anchor: strings.TrimPrefix(m[2], "#")})
	}
	for _, m := range sameLink.FindAllStringSubmatch(body, -1) {
		page.links = append(page.links, docLink{anchor: m[1]})
	}
	// Relative links between pages become site URLs. The Markdown endpoints
	// keep them as written: from /docs/api.md, "events.md" already resolves to
	// /docs/events.md.
	body = pageLink.ReplaceAllString(body, `href="/docs/$1$2"`)
	body = cellAlign.ReplaceAllString(body, ` class="align-$1"`)
	page.Body = withCopyButtons(body)
	page.Markdown = "# " + page.Title + "\n\n" + page.Description + "\n\n" + strings.TrimSpace(doc.Raw) + "\n"
	return page, doc.Meta["next"], nil
}

// withCopyButtons gives every code block a copy button. Code text inside the
// HTML is escaped, so a literal "<pre>" only appears as real markup.
func withCopyButtons(body string) template.HTML {
	body = strings.ReplaceAll(body, "<pre>",
		`<div class="code-block"><button type="button" class="copy-code" data-copy-code aria-label="Copy code">Copy</button><pre>`)
	body = strings.ReplaceAll(body, "</pre>", "</pre></div>")
	return template.HTML(body) // goldmark safe-mode output
}

// benchTable returns the first Markdown table in the benchmarks page's
// Results section rendered as HTML, so the landing page quotes the measured
// numbers rather than a copy of them.
func (d *docSet) benchTable(md *markdown.Renderer) (template.HTML, error) {
	page, ok := d.bySlug["benchmarks"]
	if !ok {
		return "", fmt.Errorf("docs: no benchmarks.md")
	}
	_, after, ok := strings.Cut(page.Markdown, "{#results}")
	if !ok {
		return "", fmt.Errorf("docs/benchmarks.md: no {#results} section")
	}
	var rows []string
	for line := range strings.SplitSeq(after, "\n") {
		if strings.HasPrefix(line, "|") {
			rows = append(rows, line)
		} else if len(rows) > 0 {
			break
		}
	}
	if len(rows) < 3 {
		return "", fmt.Errorf("docs/benchmarks.md: no results table")
	}
	doc, err := md.Parse([]byte(strings.Join(rows, "\n") + "\n"))
	if err != nil {
		return "", err
	}
	return template.HTML(cellAlign.ReplaceAllString(string(doc.HTML), ` class="align-$1"`)), nil // goldmark safe-mode output
}

// absoluteURL joins the configured public origin and a path. With no public
// origin (local development) it returns the path unchanged.
func absoluteURL(origin, p string) string {
	return origin + p
}

const llmsSummary = "comfy-api-proxy is one static Go binary that serves the Comfy API v2 (/api/v2/*) in front of a self-hosted ComfyUI, so code written for Comfy Cloud runs against a local GPU unchanged. The repository also ships a Go SDK for the same API and for Comfy Router."

// llmsIndex renders /llms.txt in the llmstxt.org format: an H1, a blockquote
// summary, notes without headings, then H2 sections of Markdown links.
func (d *docSet) llmsIndex(origin string) string {
	var b strings.Builder
	b.WriteString("# comfy-api-proxy-go\n\n> " + llmsSummary + "\n\n")
	b.WriteString("Install the proxy with `go install github.com/regiellis/comfy-api-proxy-go/cmd/comfy-api-proxy@latest` and the SDK with `go get github.com/regiellis/comfy-api-proxy-go/sdk`. ")
	b.WriteString("Each link below is the Markdown source of one documentation page, in reading order.\n\n## Docs\n\n")
	for _, p := range d.pages {
		fmt.Fprintf(&b, "- [%s](%s): %s\n", p.Title, absoluteURL(origin, p.MarkdownPath), p.Description)
	}
	b.WriteString("\n## Optional\n\n")
	fmt.Fprintf(&b, "- [All documentation in one file](%s): every page above, concatenated in reading order.\n", absoluteURL(origin, "/llms-full.txt"))
	b.WriteString("- [Go SDK reference](https://pkg.go.dev/github.com/regiellis/comfy-api-proxy-go/sdk): generated reference for every exported identifier.\n")
	b.WriteString("- [Source code](https://github.com/regiellis/comfy-api-proxy-go): the proxy, the SDK, and this site.\n")
	return b.String()
}

// llmsFull renders /llms-full.txt: every page's Markdown in reading order.
func (d *docSet) llmsFull(origin string) string {
	var b strings.Builder
	b.WriteString("# comfy-api-proxy-go documentation\n\n> " + llmsSummary + "\n\n")
	for _, p := range d.pages {
		fmt.Fprintf(&b, "---\n\nSource: %s\n\n%s\n", absoluteURL(origin, p.Path), p.Markdown)
	}
	return b.String()
}

// setupDocs registers the documentation pages, their Markdown sources,
// llms.txt, llms-full.txt, and search. publicOrigin is the validated
// PUBLIC_ORIGIN, or "" for relative links in local development.
func setupDocs(app *hyperchi.HyperChi, docs *docSet, publicOrigin string) {
	app.RedirectRoute("/docs", docs.bySlug[firstSlug].Path, 308)
	for _, page := range docs.pages {
		app.Get(page.Path, func(c *hyperchi.Context) error {
			return c.Page("docs.tmpl", hyperchi.H{
				"title":       page.Title + " | comfy-api-proxy-go",
				"description": page.Description,
				"canonical":   canonical(publicOrigin, page.Path),
				"origin":      publicOrigin,
				"current":     "docs", "topic": page.Slug,
				"page": page, "sections": docs.sections,
			})
		})
		app.Get(page.MarkdownPath, func(c *hyperchi.Context) error {
			return c.Blob("text/markdown; charset=utf-8", []byte(page.Markdown))
		})
		if page.Slug == firstSlug {
			// llmstxt.org maps a path without a file name to index.md.
			app.Get("/docs/index.md", func(c *hyperchi.Context) error {
				return c.Blob("text/markdown; charset=utf-8", []byte(page.Markdown))
			})
		}
	}
	index := docs.llmsIndex(publicOrigin)
	full := docs.llmsFull(publicOrigin)
	app.Get("/llms.txt", func(c *hyperchi.Context) error { return c.Text(index) })
	app.Get("/llms-full.txt", func(c *hyperchi.Context) error { return c.Text(full) })

	app.Get("/docs/search", func(c *hyperchi.Context) error {
		query := boundQuery(c.Query("q"))
		data := hyperchi.H{"query": query, "results": docs.search(query), "sections": docs.sections}
		if c.IsHTMX() && !c.HX().Boosted() {
			if query == "" {
				return c.HTML("") // an empty dropdown collapses
			}
			return c.Fragment("docs-search-results", data)
		}
		data["title"] = "Search the documentation | comfy-api-proxy-go"
		data["description"] = "Search every page of the comfy-api-proxy-go documentation."
		data["origin"] = publicOrigin
		data["current"], data["topic"] = "docs", "search"
		return c.SetHeader("X-Robots-Tag", "noindex").Page("docs.tmpl", data)
	})
}

// canonical is the page's absolute URL, or "" in local development, where
// the template leaves the canonical link out.
func canonical(origin, p string) string {
	if origin == "" {
		return ""
	}
	return origin + p
}
