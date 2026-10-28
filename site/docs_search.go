package main

import (
	"html"
	"html/template"
	"sort"
	"strings"
	"unicode"
)

// Search is built once at startup from the Markdown: one entry for each page
// (its title and lede) and one for each heading's section. Scoring is fixed
// and deterministic: heading and title hits outrank body hits, and ties keep
// reading order.
const (
	maxQueryRunes   = 80
	maxQueryTerms   = 6
	maxResults      = 12
	snippetBefore   = 60
	snippetAfter    = 140
	headingHit      = 30
	pageTitleHit    = 4
	bodyHit         = 2
	bodyHitCap      = 5
	phraseHeading   = 20
	phraseBody      = 6
	exactHeadingHit = 10
)

type searchEntry struct {
	page                          *docPage
	anchor                        string
	heading, text                 []rune // original text
	lowHeading, lowText, lowTitle []rune
}

type searchResult struct {
	Page    string
	Heading template.HTML
	URL     string
	Snippet template.HTML
	score   int
	rank    int
}

func buildSearchIndex(pages []*docPage) []searchEntry {
	var index []searchEntry
	add := func(page *docPage, anchor, heading, text string) {
		text = strings.Join(strings.Fields(text), " ")
		index = append(index, searchEntry{
			page: page, anchor: anchor,
			heading: []rune(heading), text: []rune(text),
			lowHeading: fold(heading), lowText: fold(text), lowTitle: fold(page.Title),
		})
	}
	for _, page := range pages {
		// The page entry comes first, so a title hit links to the top of the page.
		intro := page.Description
		for _, s := range page.sections {
			if s.Heading.Level == 0 {
				intro += " " + s.Text
			}
		}
		add(page, "", page.Title, intro)
		for _, s := range page.sections {
			if s.Heading.Level > 0 {
				add(page, s.Heading.ID, s.Heading.Text, s.Text)
			}
		}
	}
	return index
}

// boundQuery trims the query and caps its length in characters.
func boundQuery(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if r := []rune(q); len(r) > maxQueryRunes {
		q = strings.TrimSpace(string(r[:maxQueryRunes]))
	}
	return q
}

func queryTerms(q string) [][]rune {
	var terms [][]rune
	seen := map[string]bool{}
	for _, field := range strings.Fields(q) {
		term := strings.TrimFunc(field, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		key := string(fold(term))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		terms = append(terms, fold(term))
		if len(terms) == maxQueryTerms {
			break
		}
	}
	return terms
}

// search returns the best entries for q. Every term must appear in the
// entry's heading, text, or page title.
func (d *docSet) search(q string) []searchResult {
	terms := queryTerms(q)
	if len(terms) == 0 {
		return nil
	}
	phrase := fold(strings.Join(strings.Fields(q), " "))
	var results []searchResult
	for i, e := range d.index {
		score := 0
		matched := true
		for _, t := range terms {
			inHeading := contains(e.lowHeading, t)
			inTitle := contains(e.lowTitle, t)
			hits := count(e.lowText, t, bodyHitCap)
			if !inHeading && !inTitle && hits == 0 {
				matched = false
				break
			}
			if inHeading {
				score += headingHit
			}
			if inTitle && e.anchor != "" {
				score += pageTitleHit
			}
			score += hits * bodyHit
		}
		if !matched {
			continue
		}
		if len(terms) > 1 {
			if contains(e.lowHeading, phrase) {
				score += phraseHeading
			}
			if contains(e.lowText, phrase) {
				score += phraseBody
			}
		}
		if string(e.lowHeading) == string(phrase) {
			score += exactHeadingHit
		}
		url := e.page.Path
		if e.anchor != "" {
			url += "#" + e.anchor
		}
		results = append(results, searchResult{
			Page:    e.page.Title,
			Heading: highlight(e.heading, e.lowHeading, terms),
			URL:     url,
			Snippet: snippet(e, terms),
			score:   score,
			rank:    i,
		})
	}
	sort.SliceStable(results, func(a, b int) bool {
		if results[a].score != results[b].score {
			return results[a].score > results[b].score
		}
		return results[a].rank < results[b].rank
	})
	if len(results) > maxResults {
		results = results[:maxResults]
	}
	return results
}

// snippet returns escaped text around the first body hit, with every term
// highlighted. With no body hit it returns the start of the section.
func snippet(e searchEntry, terms [][]rune) template.HTML {
	first := -1
	for _, t := range terms {
		if at := index(e.lowText, t, 0); at >= 0 && (first < 0 || at < first) {
			first = at
		}
	}
	start, end := 0, len(e.text)
	if first < 0 {
		first = 0
	}
	if first > snippetBefore {
		start = first - snippetBefore
		for start < first && !unicode.IsSpace(e.text[start]) {
			start++
		}
	}
	if end > first+snippetAfter {
		end = first + snippetAfter
		for end > first && !unicode.IsSpace(e.text[end-1]) {
			end--
		}
	}
	out := highlight(e.text[start:end], e.lowText[start:end], terms)
	if start > 0 {
		out = "…" + out
	}
	if end < len(e.text) {
		out += "…"
	}
	return out
}

// highlight escapes text and wraps each term match in <mark>. low is the
// case-folded text, rune for rune.
func highlight(text, low []rune, terms [][]rune) template.HTML {
	var b strings.Builder
	plain := 0
	for i := 0; i < len(low); {
		n := 0
		for _, t := range terms {
			if len(t) > n && hasPrefixAt(low, t, i) {
				n = len(t)
			}
		}
		if n == 0 {
			i++
			continue
		}
		b.WriteString(html.EscapeString(string(text[plain:i])))
		b.WriteString("<mark>" + html.EscapeString(string(text[i:i+n])) + "</mark>")
		i += n
		plain = i
	}
	b.WriteString(html.EscapeString(string(text[plain:])))
	return template.HTML(b.String()) // every piece of text above is escaped
}

// fold lowercases rune by rune so folded and original text share indexes.
func fold(s string) []rune {
	r := []rune(s)
	for i, c := range r {
		r[i] = unicode.ToLower(c)
	}
	return r
}

func hasPrefixAt(s, t []rune, at int) bool {
	if at+len(t) > len(s) {
		return false
	}
	for i, c := range t {
		if s[at+i] != c {
			return false
		}
	}
	return true
}

func index(s, t []rune, from int) int {
	for i := from; i+len(t) <= len(s); i++ {
		if hasPrefixAt(s, t, i) {
			return i
		}
	}
	return -1
}

func contains(s, t []rune) bool { return len(t) > 0 && index(s, t, 0) >= 0 }

func count(s, t []rune, limit int) int {
	n := 0
	for at := index(s, t, 0); at >= 0 && n < limit; at = index(s, t, at+len(t)) {
		n++
	}
	return n
}
