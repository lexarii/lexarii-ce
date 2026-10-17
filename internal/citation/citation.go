// SPDX-License-Identifier: AGPL-3.0-or-later

// Package citation renders a book as a citation someone can paste elsewhere.
//
// Three formats, because the three tools people actually use do not agree:
// BibTeX for LaTeX, RIS for Zotero, EndNote and Mendeley, and CSL-JSON for
// Pandoc and anything modern. All three are plain text and none needs a
// dependency.
//
// Everything here is rendered from what the library already knows. Nothing is
// looked up, invented, or filled in with a plausible default: a citation with a
// guessed year is worse than a citation with no year, because the guess will be
// copied into somebody's bibliography and read as a fact.
package citation

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// Format is a citation syntax.
type Format string

const (
	// FormatBibTeX is what LaTeX reads.
	FormatBibTeX Format = "bibtex"
	// FormatRIS is what Zotero, Mendeley and EndNote import.
	FormatRIS Format = "ris"
	// FormatCSLJSON is what Pandoc and most modern tools read.
	FormatCSLJSON Format = "csl-json"
)

// Parse maps a request's format parameter, defaulting to BibTeX.
func Parse(s string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(FormatBibTeX), "bib":
		return FormatBibTeX, nil
	case string(FormatRIS):
		return FormatRIS, nil
	case string(FormatCSLJSON), "csl", "json":
		return FormatCSLJSON, nil
	}
	return "", fmt.Errorf("unknown citation format %q: use bibtex, ris, or csl-json", s)
}

// ContentType is what to serve this format as.
func (f Format) ContentType() string {
	switch f {
	case FormatCSLJSON:
		return "application/json; charset=utf-8"
	case FormatRIS:
		return "application/x-research-info-systems; charset=utf-8"
	default:
		// text/plain rather than application/x-bibtex, so a browser shows it
		// instead of downloading it. Copying one entry is the common case.
		return "text/plain; charset=utf-8"
	}
}

// Extension is the file extension for a download of this format.
func (f Format) Extension() string {
	switch f {
	case FormatCSLJSON:
		return "json"
	case FormatRIS:
		return "ris"
	default:
		return "bib"
	}
}

// Entry is one citable thing: a Work, with the edition details to cite it by.
//
// Built from a Work rather than being one, because a Work can have several
// editions and a citation names exactly one. Which one is the caller's
// decision.
type Entry struct {
	Key       string
	Title     string
	Subtitle  string
	Authors   []string
	Publisher string
	Year      string
	ISBN      string
	Series    string
	Edition   string
	Pages     int
	Language  string
	// URL is where this copy lives in this library, for a citation somebody
	// wants to follow back. Optional.
	URL string
}

// FromWork builds an entry from a work and, when it has one, its first edition.
//
// The first edition by publication date, falling back to the first recorded:
// a library that holds two printings of the same book should cite the same one
// every time, and "whichever row came back first" is not that.
func FromWork(w domain.Work, url string) Entry {
	e := Entry{
		Title:    strings.TrimSpace(w.Title),
		Subtitle: strings.TrimSpace(w.Subtitle),
		Series:   strings.TrimSpace(w.Series),
		Language: strings.TrimSpace(w.Language),
		URL:      url,
	}
	for _, a := range w.Authors {
		if name := strings.TrimSpace(a.Author.Name); name != "" {
			e.Authors = append(e.Authors, name)
		}
	}

	if ed, ok := citedEdition(w.Editions); ok {
		e.Publisher = strings.TrimSpace(ed.Publisher)
		e.ISBN = strings.TrimSpace(ed.ISBN13)
		if e.ISBN == "" {
			e.ISBN = strings.TrimSpace(ed.ISBN10)
		}
		if ed.PublishedOn != nil {
			e.Year = fmt.Sprintf("%04d", ed.PublishedOn.Year())
		}
		if ed.PageCount != nil {
			e.Pages = *ed.PageCount
		}
		if e.Language == "" {
			e.Language = strings.TrimSpace(ed.Language)
		}
	}

	e.Key = Key(e)
	return e
}

// citedEdition picks which printing a citation names.
func citedEdition(editions []domain.Edition) (domain.Edition, bool) {
	if len(editions) == 0 {
		return domain.Edition{}, false
	}
	best := editions[0]
	for _, ed := range editions[1:] {
		switch {
		case best.PublishedOn == nil && ed.PublishedOn != nil:
			best = ed
		case best.PublishedOn != nil && ed.PublishedOn != nil && ed.PublishedOn.Before(*best.PublishedOn):
			best = ed
		}
	}
	return best, true
}

// Key is the citation key: surname, year, first significant title word.
//
// "knuth1968art". The convention every BibTeX user already has in their head,
// and stable for the same book across exports - a key that changed between two
// exports would silently break every \cite in a document.
func Key(e Entry) string {
	var b strings.Builder
	if len(e.Authors) > 0 {
		b.WriteString(asciiFold(surname(e.Authors[0])))
	} else {
		b.WriteString("anon")
	}
	b.WriteString(e.Year)
	if word := firstSignificantWord(e.Title); word != "" {
		b.WriteString(asciiFold(word))
	}

	key := b.String()
	if key == "" {
		return "untitled"
	}
	return key
}

// surname takes the family name out of a name written either way round.
func surname(name string) string {
	name = strings.TrimSpace(name)
	// "Knuth, Donald E." - already surname first.
	if before, _, found := strings.Cut(name, ","); found {
		return strings.TrimSpace(before)
	}
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// stopWords are skipped when choosing the title word for a key, so half a
// library does not have keys beginning "the".
var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "on": true, "in": true,
	"to": true, "for": true, "and": true, "with": true,
}

func firstSignificantWord(title string) string {
	for _, field := range strings.Fields(strings.ToLower(title)) {
		word := strings.TrimFunc(field, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
		if word == "" || stopWords[word] {
			continue
		}
		return word
	}
	return ""
}

// asciiFold reduces a string to letters and digits a citation key may contain.
//
// Accents are dropped rather than transliterated. A key is an identifier, not a
// name: "Gödel" keys as "gdel", which is ugly and is stable, and the name
// itself is spelled correctly in the entry two lines below.
func asciiFold(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Render writes one or more entries in the given format.
//
// Keys are made unique across the set before anything is written. Two books by
// the same author in the same year produce the same key, and a bibliography
// with a duplicate key is one BibTeX silently drops half of.
func Render(entries []Entry, format Format) (string, error) {
	entries = uniqueKeys(entries)
	switch format {
	case FormatBibTeX:
		return renderBibTeX(entries), nil
	case FormatRIS:
		return renderRIS(entries), nil
	case FormatCSLJSON:
		return renderCSLJSON(entries)
	}
	return "", fmt.Errorf("unknown citation format %q", format)
}

// uniqueKeys suffixes duplicates a, b, c - the BibTeX convention.
func uniqueKeys(entries []Entry) []Entry {
	out := make([]Entry, len(entries))
	copy(out, entries)

	byKey := map[string][]int{}
	for i, e := range out {
		byKey[e.Key] = append(byKey[e.Key], i)
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		idx := byKey[k]
		if len(idx) < 2 {
			continue
		}
		for n, i := range idx {
			out[i].Key = k + string(rune('a'+n%26))
		}
	}
	return out
}
