// SPDX-License-Identifier: AGPL-3.0-or-later

package citation

import (
	"encoding/json"
	"fmt"
	"strings"
)

// renderBibTeX writes @book entries.
//
// Braced values rather than quoted: braces nest, and a title containing a
// quotation mark is otherwise a syntax error in somebody's .bib file.
func renderBibTeX(entries []Entry) string {
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "@book{%s,\n", e.Key)

		fields := [][2]string{
			{"title", bibTitle(e)},
			{"author", strings.Join(e.Authors, " and ")},
			{"publisher", e.Publisher},
			{"year", e.Year},
			{"series", e.Series},
			{"edition", e.Edition},
			{"isbn", e.ISBN},
			{"language", e.Language},
			{"url", e.URL},
		}
		if e.Pages > 0 {
			fields = append(fields, [2]string{"pagetotal", fmt.Sprint(e.Pages)})
		}

		for _, f := range fields {
			if strings.TrimSpace(f[1]) == "" {
				continue
			}
			fmt.Fprintf(&b, "  %-10s = {%s},\n", f[0], escapeBibTeX(f[1]))
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// bibTitle joins a title and its subtitle the way a bibliography expects.
func bibTitle(e Entry) string {
	if e.Subtitle == "" {
		return e.Title
	}
	return e.Title + ": " + e.Subtitle
}

// bibTeXSpecials are the characters TeX reads as markup rather than as text.
var bibTeXSpecials = strings.NewReplacer(
	`\`, `\textbackslash{}`,
	`{`, `\{`,
	`}`, `\}`,
	`$`, `\$`,
	`&`, `\&`,
	`%`, `\%`,
	`#`, `\#`,
	`_`, `\_`,
	`~`, `\textasciitilde{}`,
	`^`, `\textasciicircum{}`,
)

// escapeBibTeX makes a value safe to paste into a .bib file.
//
// This is book metadata going into somebody's build: a title containing an
// ampersand or an underscore - "C# 8.0", "sed & awk", "snake_case" - breaks a
// LaTeX run with an error pointing at the bibliography rather than at the book.
func escapeBibTeX(s string) string {
	return bibTeXSpecials.Replace(s)
}

// renderRIS writes RIS records, which is what Zotero and EndNote import.
//
// Tag, two spaces, a hyphen, a space, value. The format is rigid about that
// and importers reject a record whose tags are spaced differently. ER is the
// end of record and is required.
func renderRIS(entries []Entry) string {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString("TY  - BOOK\n")
		for _, author := range e.Authors {
			writeRIS(&b, "AU", author)
		}
		// One TI. RIS has no tag for a subtitle - T2 is the series - so it
		// goes on the end of the title, where a reader expects it. Writing
		// both a bare TI and a subtitled one gave importers two titles for one
		// book, and which of them survives is the importer's choice.
		writeRIS(&b, "TI", bibTitle(e))
		writeRIS(&b, "PB", e.Publisher)
		writeRIS(&b, "PY", e.Year)
		writeRIS(&b, "T2", e.Series)
		writeRIS(&b, "ET", e.Edition)
		writeRIS(&b, "SN", e.ISBN)
		writeRIS(&b, "LA", e.Language)
		writeRIS(&b, "UR", e.URL)
		if e.Pages > 0 {
			writeRIS(&b, "SP", fmt.Sprint(e.Pages))
		}
		b.WriteString("ER  - \n\n")
	}
	return b.String()
}

func writeRIS(b *strings.Builder, tag, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	// A newline inside a value would be read as the start of a new tag, and
	// the record would be silently mangled on import.
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\r", " "), "\n", " ")
	fmt.Fprintf(b, "%s  - %s\n", tag, value)
}

// cslEntry is one CSL-JSON item.
type cslEntry struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Author    []cslName `json:"author,omitempty"`
	Publisher string    `json:"publisher,omitempty"`
	Issued    *cslDate  `json:"issued,omitempty"`
	ISBN      string    `json:"ISBN,omitempty"`
	Series    string    `json:"collection-title,omitempty"`
	Edition   string    `json:"edition,omitempty"`
	Pages     string    `json:"number-of-pages,omitempty"`
	Language  string    `json:"language,omitempty"`
	URL       string    `json:"URL,omitempty"`
}

// cslName is a name split the way CSL wants it.
type cslName struct {
	Family string `json:"family,omitempty"`
	Given  string `json:"given,omitempty"`
	// Literal is for a name that is not a person - an organisation, or one
	// that cannot be split without guessing.
	Literal string `json:"literal,omitempty"`
}

// cslDate is CSL's date-parts shape: [[year]] for a book with only a year.
type cslDate struct {
	DateParts [][]int `json:"date-parts"`
}

func renderCSLJSON(entries []Entry) (string, error) {
	out := make([]cslEntry, 0, len(entries))
	for _, e := range entries {
		item := cslEntry{
			ID:        e.Key,
			Type:      "book",
			Title:     bibTitle(e),
			Publisher: e.Publisher,
			ISBN:      e.ISBN,
			Series:    e.Series,
			Edition:   e.Edition,
			Language:  e.Language,
			URL:       e.URL,
		}
		for _, a := range e.Authors {
			item.Author = append(item.Author, cslName(splitName(a)))
		}
		if year := yearOf(e.Year); year > 0 {
			item.Issued = &cslDate{DateParts: [][]int{{year}}}
		}
		if e.Pages > 0 {
			item.Pages = fmt.Sprint(e.Pages)
		}
		out = append(out, item)
	}

	encoded, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding CSL-JSON: %w", err)
	}
	return string(encoded) + "\n", nil
}

func yearOf(s string) int {
	var year int
	if _, err := fmt.Sscanf(s, "%d", &year); err != nil {
		return 0
	}
	return year
}

// splitName divides a name into family and given.
//
// Only where the split is unambiguous: "Knuth, Donald E." is written surname
// first and says so with its comma. Anything else goes in `literal`, which CSL
// has for exactly this - a name a tool cannot parse is rendered as written
// rather than guessed at. Guessing here turns "Ursula K. Le Guin" into a family
// name of "Guin" in somebody's bibliography.
func splitName(name string) cslName {
	name = strings.TrimSpace(name)
	if before, after, found := strings.Cut(name, ","); found {
		family := strings.TrimSpace(before)
		given := strings.TrimSpace(after)
		if family != "" && given != "" {
			return cslName{Family: family, Given: given}
		}
	}
	return cslName{Literal: name}
}
