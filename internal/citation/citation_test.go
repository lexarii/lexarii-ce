// SPDX-License-Identifier: AGPL-3.0-or-later

package citation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

func published(year int) *time.Time {
	t := time.Date(year, time.January, 1, 0, 0, 0, 0, time.UTC)
	return &t
}

func pages(n int) *int { return &n }

func work() domain.Work {
	return domain.Work{
		ID:    uuid.New(),
		Title: "The Art of Computer Programming",
		Authors: []domain.WorkAuthor{
			{Author: domain.Author{Name: "Knuth, Donald E."}},
		},
		Editions: []domain.Edition{{
			Publisher:   "Addison-Wesley",
			ISBN13:      "9780201896831",
			PublishedOn: published(1968),
			PageCount:   pages(650),
			Language:    "en",
		}},
	}
}

func TestBibTeXEntry(t *testing.T) {
	entry := FromWork(work(), "https://books.example/works/1")
	out, err := Render([]Entry{entry}, FormatBibTeX)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	// The key convention every BibTeX user already has in their head, and the
	// article dropped so half a library does not key as "the".
	if !strings.HasPrefix(out, "@book{knuth1968art,") {
		t.Errorf("first line = %q", strings.SplitN(out, "\n", 2)[0])
	}
	for _, want := range []string{
		"title      = {The Art of Computer Programming}",
		"author     = {Knuth, Donald E.}",
		"publisher  = {Addison-Wesley}",
		"year       = {1968}",
		"isbn       = {9780201896831}",
		"pagetotal  = {650}",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// A field the library does not know is left out, not filled in. A citation
// with a guessed year gets copied into a bibliography and read as a fact.
func TestBibTeXOmitsWhatIsNotKnown(t *testing.T) {
	out, err := Render([]Entry{FromWork(domain.Work{Title: "Untitled Notes"}, "")}, FormatBibTeX)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, unwanted := range []string{"publisher", "year", "isbn", "author", "url"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("%q appears for a work that has none:\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "@book{anonuntitled,") {
		t.Errorf("key for an author-less work = %q", out)
	}
}

// Book metadata goes into somebody's LaTeX build. An ampersand or an
// underscore in a title breaks the run with an error pointing at the
// bibliography rather than at the book - and this library really does contain
// "sed & awk" and "C# 8.0 in a Nutshell".
func TestBibTeXEscapesTeXSpecials(t *testing.T) {
	w := domain.Work{Title: "sed & awk", Subtitle: "100% of snake_case $and more"}
	out, err := Render([]Entry{FromWork(w, "")}, FormatBibTeX)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := `title      = {sed \& awk: 100\% of snake\_case \$and more}`
	if !strings.Contains(out, want) {
		t.Errorf("escaping wrong:\n%s", out)
	}
}

// Two books by one author in one year produce one key, and a .bib file with a
// duplicate key is one BibTeX silently drops half of.
func TestDuplicateKeysAreSuffixed(t *testing.T) {
	a := domain.Work{Title: "Volume One", Authors: []domain.WorkAuthor{{Author: domain.Author{Name: "Knuth, Donald E."}}}}
	b := domain.Work{Title: "Volume Two", Authors: []domain.WorkAuthor{{Author: domain.Author{Name: "Knuth, Donald E."}}}}
	// Same author, no year, and a first significant word that differs - but
	// "volume" for both, so the keys collide.
	out, err := Render([]Entry{FromWork(a, ""), FromWork(b, "")}, FormatBibTeX)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "@book{knuthvolumea,") || !strings.Contains(out, "@book{knuthvolumeb,") {
		t.Errorf("keys not disambiguated:\n%s", out)
	}
}

func TestRISRecord(t *testing.T) {
	out, err := Render([]Entry{FromWork(work(), "")}, FormatRIS)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{
		"TY  - BOOK\n",
		"AU  - Knuth, Donald E.\n",
		"TI  - The Art of Computer Programming\n",
		"PB  - Addison-Wesley\n",
		"PY  - 1968\n",
		"SN  - 9780201896831\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// Importers require the end-of-record marker.
	if !strings.Contains(out, "ER  - \n") {
		t.Errorf("no end of record:\n%s", out)
	}
}

// RIS has no subtitle tag, so the subtitle joins the title - in ONE TI. Two TI
// lines give an importer two titles for one book and let it pick.
func TestRISHasOneTitlePerRecord(t *testing.T) {
	w := domain.Work{Title: "Programming Rust", Subtitle: "Fast, Safe Systems Development"}
	out, err := Render([]Entry{FromWork(w, "")}, FormatRIS)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if n := strings.Count(out, "TI  - "); n != 1 {
		t.Errorf("%d TI lines, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "TI  - Programming Rust: Fast, Safe Systems Development\n") {
		t.Errorf("subtitle not joined to the title:\n%s", out)
	}
}

// A newline inside a value would be read as the start of the next tag, and the
// record would be mangled on import rather than rejected.
func TestRISFlattensNewlines(t *testing.T) {
	w := domain.Work{Title: "A Title\nSplit Over Lines"}
	out, err := Render([]Entry{FromWork(w, "")}, FormatRIS)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(out, "TI  - A Title Split Over Lines\n") {
		t.Errorf("newline not flattened:\n%s", out)
	}
}

func TestCSLJSON(t *testing.T) {
	out, err := Render([]Entry{FromWork(work(), "https://books.example/works/1")}, FormatCSLJSON)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	var items []map[string]any
	if err := json.Unmarshal([]byte(out), &items); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items", len(items))
	}
	item := items[0]
	if item["type"] != "book" || item["id"] != "knuth1968art" {
		t.Errorf("type/id = %v/%v", item["type"], item["id"])
	}
	// CSL dates are date-parts, not strings: [[1968]].
	issued, ok := item["issued"].(map[string]any)
	if !ok {
		t.Fatalf("issued = %v", item["issued"])
	}
	parts, ok := issued["date-parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("date-parts = %v", issued["date-parts"])
	}
	first, ok := parts[0].([]any)
	if !ok || len(first) != 1 || first[0].(float64) != 1968 {
		t.Errorf("date-parts[0] = %v, want [1968]", parts[0])
	}
}

// A name is split only where the split is unambiguous. Guessing turns
// "Ursula K. Le Guin" into a family name of "Guin" in somebody's bibliography.
func TestCSLNamesAreSplitOnlyWhenTheySayHow(t *testing.T) {
	if got := splitName("Knuth, Donald E."); got.Family != "Knuth" || got.Given != "Donald E." {
		t.Errorf("comma form = %+v", got)
	}
	if got := splitName("Ursula K. Le Guin"); got.Literal != "Ursula K. Le Guin" || got.Family != "" {
		t.Errorf("unpunctuated name = %+v, want it left whole", got)
	}
	if got := splitName("O'Reilly Media"); got.Literal != "O'Reilly Media" {
		t.Errorf("organisation = %+v, want it left whole", got)
	}
}

// The library holds several printings of one book; a citation names one, and
// it has to be the same one every time or two exports of one shelf disagree.
func TestTheCitedEditionIsTheEarliest(t *testing.T) {
	w := work()
	w.Editions = []domain.Edition{
		{Publisher: "Reprint", PublishedOn: published(1997)},
		{Publisher: "First", PublishedOn: published(1968)},
	}
	if got := FromWork(w, "").Publisher; got != "First" {
		t.Errorf("publisher = %q, want the earliest printing", got)
	}

	// With no dates at all it is still deterministic rather than arbitrary.
	w.Editions = []domain.Edition{{Publisher: "One"}, {Publisher: "Two"}}
	if got := FromWork(w, "").Publisher; got != "One" {
		t.Errorf("publisher = %q", got)
	}
}

func TestParseFormat(t *testing.T) {
	for in, want := range map[string]Format{
		"": FormatBibTeX, "bibtex": FormatBibTeX, "BIB": FormatBibTeX,
		"ris": FormatRIS, "csl-json": FormatCSLJSON, "csl": FormatCSLJSON, " json ": FormatCSLJSON,
	} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Parse("endnote"); err == nil {
		t.Error("an unknown format was accepted")
	}
}
