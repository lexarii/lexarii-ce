// SPDX-License-Identifier: AGPL-3.0-or-later

package pdf

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

func TestParseExtractsTextPerPage(t *testing.T) {
	data := newTextPDF(t, [][]string{
		{"Chapter One", "A loop invariant is an assertion that holds before and after every iteration."},
		{"Chapter Two", "Quicksort partitions the array around a pivot element chosen from the subarray."},
	}, "")

	res, err := Parse(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(res.Spine) != 2 {
		t.Fatalf("got %d units, want 2", len(res.Spine))
	}
	if len(res.UnitText) != 2 {
		t.Fatalf("got %d unit texts, want 2", len(res.UnitText))
	}
	if res.Format != domain.FormatPDF {
		t.Errorf("Format = %q", res.Format)
	}

	if !strings.Contains(res.UnitText[0], "loop invariant") {
		t.Errorf("page 1 text = %q, want it to contain the first page's sentence", res.UnitText[0])
	}
	if !strings.Contains(res.UnitText[1], "Quicksort") {
		t.Errorf("page 2 text = %q, want it to contain the second page's sentence", res.UnitText[1])
	}
	// Pages must not bleed into one another, or every locator past page 1 is wrong.
	if strings.Contains(res.UnitText[0], "Quicksort") {
		t.Error("page 1 text contains page 2 content")
	}

	if res.NeedsOCR {
		t.Error("a document with a real text layer was flagged as a scan")
	}
}

// PDF pages are one-based while EPUB spine indices are zero-based. Getting this
// wrong shifts every locator in the book by one.
func TestMajorForIsOneBasedForPDF(t *testing.T) {
	data := newTextPDF(t, [][]string{
		{"Page one text that is long enough to count as a real text layer here."},
		{"Page two text that is long enough to count as a real text layer here."},
		{"Page three text that is long enough to count as a real text layer too."},
	}, "")

	res, err := Parse(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	for unit := range res.Spine {
		if got, want := res.MajorFor(unit), unit+1; got != want {
			t.Errorf("MajorFor(%d) = %d, want %d", unit, got, want)
		}
	}

	// And the resulting locator must be valid, which requires a one-based page.
	loc := domain.NewPDFLocator(res.MajorFor(0), 0)
	if err := loc.Validate(); err != nil {
		t.Fatalf("locator for the first unit is invalid: %v", err)
	}
}

// The failure mode worth designing against: a scan ingested as an empty book
// looks fine, searches find nothing, and the user cannot tell why.
func TestScannedPDFIsFlaggedNotIngestedEmpty(t *testing.T) {
	data := newScannedPDF(t, 12)

	res, err := Parse(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if !res.NeedsOCR {
		t.Fatal("a page-image PDF was not flagged as needing OCR")
	}
	// It is still a library citizen: the pages are known, so it can be shelved,
	// opened, and OCRed later.
	if len(res.Spine) != 12 {
		t.Errorf("got %d units, want 12 - the pages must still be recorded", len(res.Spine))
	}
	if len(res.Warnings) == 0 {
		t.Error("a scan should produce a warning explaining why it is not searchable")
	}
}

func TestLooksScanned(t *testing.T) {
	tests := []struct {
		name       string
		totalChars int
		pageCount  int
		want       bool
	}{
		{"real book", 400_000, 300, false},
		{"pure scan", 0, 300, true},
		{"scan with watermarks", 300, 300, true},
		{"short but dense document", 4000, 2, false},
		{"short and empty", 10, 2, true},
		{"single dense page", 3000, 1, false},
		{"no pages", 0, 0, false},
		// The absolute floor applies only to single-page documents.
		{"just above the total floor", MinTotalChars + 1, 1, false},
		{"just below the total floor", MinTotalChars - 1, 1, true},
		// A genuinely short multi-page document is not a scan: the per-page
		// average is what decides, not the total.
		{"short real document", 150, 2, false},
		{"empty pages are always a scan", 0, 2, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksScanned(tt.totalChars, tt.pageCount); got != tt.want {
				t.Fatalf("looksScanned(%d, %d) = %v, want %v",
					tt.totalChars, tt.pageCount, got, tt.want)
			}
		})
	}
}

func TestCountMeaningfulIgnoresWhitespace(t *testing.T) {
	// A page of blank lines must not read as a text layer.
	if got := countMeaningful("\n\n   \t\n  \n"); got != 0 {
		t.Fatalf("countMeaningful(whitespace) = %d, want 0", got)
	}
	if got := countMeaningful("ab c\nd"); got != 4 {
		t.Fatalf("countMeaningful = %d, want 4", got)
	}
}

func TestParseReadsDocumentInfo(t *testing.T) {
	info := "<< /Title (The Art of Computer Programming) /Author (Donald Knuth) " +
		"/Subject (A survey of algorithms) /Keywords (algorithms, sorting; searching) >>"
	data := newTextPDF(t, [][]string{
		{"Volume one covers fundamental algorithms and their analysis in detail."},
	}, info)

	res, err := Parse(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	m := res.Metadata
	if m.Title != "The Art of Computer Programming" {
		t.Errorf("Title = %q", m.Title)
	}
	if len(m.Authors) != 1 || m.Authors[0].Name != "Donald Knuth" {
		t.Errorf("Authors = %+v", m.Authors)
	}
	if m.Description != "A survey of algorithms" {
		t.Errorf("Description = %q", m.Description)
	}
	if !reflect.DeepEqual(m.Subjects, []string{"algorithms", "sorting", "searching"}) {
		t.Errorf("Subjects = %v, want keywords split on both commas and semicolons", m.Subjects)
	}
	for _, field := range []string{"title", "authors", "description", "subjects"} {
		if m.Provenance[field] != domain.SourceEmbedded {
			t.Errorf("Provenance[%q] = %q, want embedded", field, m.Provenance[field])
		}
	}
}

// PDF Title is routinely the LaTeX job name or the source filename. An absent
// one must stay absent rather than become a guess with embedded provenance.
func TestParseDoesNotInventMetadata(t *testing.T) {
	data := newTextPDF(t, [][]string{
		{"A document with a text layer but no document information dictionary."},
	}, "")

	res, err := Parse(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Metadata.Title != "" {
		t.Errorf("Title = %q, want empty", res.Metadata.Title)
	}
	if _, recorded := res.Metadata.Provenance["title"]; recorded {
		t.Error("an absent title must not be given provenance")
	}
}

func TestParseRejectsNonPDF(t *testing.T) {
	tests := map[string][]byte{
		"empty":         {},
		"plain text":    []byte("this is not a pdf at all"),
		"header only":   []byte("%PDF-1.4\n"),
		"truncated":     []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog"),
		"zip not a pdf": {'P', 'K', 3, 4, 0, 0, 0, 0},
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(bytes.NewReader(data), int64(len(data)))
			if err == nil {
				t.Fatal("Parse accepted a non-PDF")
			}
			if !errors.Is(err, ErrNotPDF) && !errors.Is(err, ErrEncrypted) {
				t.Fatalf("error = %v, want ErrNotPDF or ErrEncrypted", err)
			}
		})
	}
}

// The parser indexes into structures derived from file bytes and panics on some
// malformed input. A bad file must fail against itself, never take down the
// worker running a library scan.
func TestParseSurvivesHostileInput(t *testing.T) {
	valid := newTextPDF(t, [][]string{
		{"A perfectly ordinary page of text that should extract without trouble."},
	}, "")

	// Corrupt the file at many offsets and confirm each attempt either parses or
	// returns an error - but never panics out of Parse.
	for i := 0; i < len(valid); i += 7 {
		corrupted := make([]byte, len(valid))
		copy(corrupted, valid)
		corrupted[i] ^= 0xFF

		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("Parse panicked on input corrupted at byte %d: %v", i, v)
				}
			}()
			_, _ = Parse(bytes.NewReader(corrupted), int64(len(corrupted)))
		}()
	}

	// Truncation at every prefix length is the other common corruption.
	for n := 0; n < len(valid); n += 11 {
		func() {
			defer func() {
				if v := recover(); v != nil {
					t.Fatalf("Parse panicked on input truncated to %d bytes: %v", n, v)
				}
			}()
			_, _ = Parse(bytes.NewReader(valid[:n]), int64(n))
		}()
	}
}

// Chunk ids, highlight anchors, and citations assume identical bytes always
// produce identical page text.
func TestParseIsDeterministic(t *testing.T) {
	data := newTextPDF(t, [][]string{
		{"Chapter One", "The first page has several lines of text on it for the parser."},
		{"Chapter Two", "The second page also has a reasonable quantity of readable text."},
	}, "<< /Title (Determinism) /Author (Test) >>")

	first, err := Parse(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	for i := 0; i < 10; i++ {
		next, err := Parse(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("Parse %d: %v", i, err)
		}
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("parse %d differs from the first", i)
		}
	}
}

// PDF text strings come in two encodings, and the second one is not optional.
//
// A string that begins with the bytes FE FF is UTF-16BE, and every PDF whose
// metadata contains a character outside Latin-1 - an accent, a dash, a CJK
// title - is written that way by the tools that produce them. Read byte by
// byte instead, and the title arrives with the byte-order mark on the front
// and a NUL between every letter.
func TestParseDecodesUTF16Metadata(t *testing.T) {
	// "Æther: Ingeniørens Håndbog" and "Kjærstad, Ø." as a PDF UTF-16BE
	// string: the BOM, then two bytes per character, escaped for a literal.
	title := utf16PDFString("Æther: Ingeniørens Håndbog")
	author := utf16PDFString("Jan Kjærstad")

	info := "<< /Title " + title + " /Author " + author + " >>"
	data := newTextPDF(t, [][]string{
		{"A document whose information dictionary is written in UTF-16."},
	}, info)

	res, err := Parse(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if res.Metadata.Title != "Æther: Ingeniørens Håndbog" {
		t.Errorf("Title = %q, want the decoded text", res.Metadata.Title)
	}
	if len(res.Metadata.Authors) != 1 || res.Metadata.Authors[0].Name != "Jan Kjærstad" {
		t.Errorf("Authors = %+v, want the decoded name", res.Metadata.Authors)
	}
}

// utf16PDFString renders s as a PDF literal string in UTF-16BE with a BOM,
// which is how every producer writes metadata that Latin-1 cannot hold.
func utf16PDFString(s string) string {
	var b strings.Builder
	b.WriteString("(")
	write := func(c byte) {
		// ( ) and \ are special inside a literal string, and a lone byte that
		// happens to equal one of them would end the string early.
		switch c {
		case '(', ')', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "\\%03o", c)
		}
	}
	write(0xFE)
	write(0xFF)
	for _, r := range utf16.Encode([]rune(s)) {
		write(byte(r >> 8))
		write(byte(r))
	}
	b.WriteString(")")
	return b.String()
}
