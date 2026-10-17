// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"errors"
	"testing"
)

func TestLocatorRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		loc  Locator
		want string
	}{
		{"epub start", NewEPUBLocator(0, "body[1]/p[1]", 0), "epub:0/body[1]/p[1]:0"},
		{"epub deep", NewEPUBLocator(12, "body[1]/div[3]/section[2]/p[9]", 481), "epub:12/body[1]/div[3]/section[2]/p[9]:481"},
		{"pdf first page", NewPDFLocator(1, 0), "pdf:1:0"},
		{"pdf deep", NewPDFLocator(874, 2048), "pdf:874:2048"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.loc.String(); got != tt.want {
				t.Fatalf("String() = %q, want %q", got, tt.want)
			}
			got, err := ParseLocator(tt.want)
			if err != nil {
				t.Fatalf("ParseLocator(%q) returned error: %v", tt.want, err)
			}
			if got != tt.loc {
				t.Fatalf("ParseLocator(%q) = %+v, want %+v", tt.want, got, tt.loc)
			}
		})
	}
}

func TestParseLocatorRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"epub",
		"epub:0",
		"epub:0/body[1]",
		"epub:x/body[1]:0",
		"epub:0/body[1]:x",
		"pdf:1",
		"pdf:x:0",
		"pdf:0:0",  // pages are one-based
		"pdf:1:-5", // negative offset
		"mobi:1:0",
		"epub:-1/body[1]:0",
	}
	for _, s := range bad {
		t.Run(s, func(t *testing.T) {
			if _, err := ParseLocator(s); !errors.Is(err, ErrInvalidLocator) {
				t.Fatalf("ParseLocator(%q) error = %v, want ErrInvalidLocator", s, err)
			}
		})
	}
}

func TestLocatorCompare(t *testing.T) {
	tests := []struct {
		name string
		a, b Locator
		want int
	}{
		{"equal epub", NewEPUBLocator(3, "p[1]", 10), NewEPUBLocator(3, "p[1]", 10), 0},
		{"earlier spine wins", NewEPUBLocator(2, "p[9]", 999), NewEPUBLocator(3, "p[1]", 0), -1},
		{"same spine, offset decides", NewEPUBLocator(3, "p[1]", 10), NewEPUBLocator(3, "p[1]", 40), -1},
		{"pdf page ordering", NewPDFLocator(9, 0), NewPDFLocator(10, 0), -1},
		{"pdf same page, offset", NewPDFLocator(9, 500), NewPDFLocator(9, 100), 1},
		{"mixed formats are incomparable", NewEPUBLocator(1, "p[1]", 0), NewPDFLocator(1, 0), 0},

		// The bug: p[10] sorts before p[9] as a string, which made a highlight
		// running forwards from the 9th paragraph to the 10th read as backwards.
		{"p[9] precedes p[10] though the strings sort the other way",
			NewEPUBLocator(0, "p[9]", 0), NewEPUBLocator(0, "p[10]", 0), -1},
		{"p[10] follows p[9], reversed", NewEPUBLocator(0, "p[10]", 0), NewEPUBLocator(0, "p[9]", 0), 1},
		{"double digits both ways", NewEPUBLocator(0, "body[1]/p[19]", 0), NewEPUBLocator(0, "body[1]/p[100]", 0), -1},
		{"same tag, same index, offset decides",
			NewEPUBLocator(0, "p[9]", 5), NewEPUBLocator(0, "p[9]", 1), 1},
		{"different tags at the same depth are undecided",
			NewEPUBLocator(0, "div[2]/p[9]", 0), NewEPUBLocator(0, "div[2]/span[1]", 0), 0},
		// The path itself is undecidable, so Compare falls back to the char
		// offset - the same weak tiebreaker it already used for any two
		// locators whose paths could not be ordered, before this fix.
		{"a prefix of the other path is undecided, offset decides",
			NewEPUBLocator(0, "div[2]", 50), NewEPUBLocator(0, "div[2]/p[1]", 50), 0},
		{"a segment with no bracketed index still compares by name",
			NewEPUBLocator(0, "body/p[2]", 0), NewEPUBLocator(0, "body/p[3]", 0), -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Compare(tt.b); got != tt.want {
				t.Fatalf("Compare() = %d, want %d", got, tt.want)
			}
		})
	}
}

// Reading-position conflict resolution takes the furthest position; it is the
// only reason Compare exists, so it gets its own test.
func TestFurthestPositionWins(t *testing.T) {
	phone := NewEPUBLocator(4, "body[1]/p[12]", 300)
	laptop := NewEPUBLocator(7, "body[1]/p[2]", 0)

	furthest := phone
	if laptop.Compare(furthest) > 0 {
		furthest = laptop
	}
	if furthest != laptop {
		t.Fatalf("furthest = %s, want %s", furthest, laptop)
	}
}

func TestRangeValidate(t *testing.T) {
	valid := Range{Start: NewEPUBLocator(1, "p[1]", 0), End: NewEPUBLocator(1, "p[4]", 20)}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid range rejected: %v", err)
	}

	reversed := Range{Start: NewPDFLocator(10, 0), End: NewPDFLocator(2, 0)}
	if err := reversed.Validate(); !errors.Is(err, ErrInvalidLocator) {
		t.Fatalf("reversed range error = %v, want ErrInvalidLocator", err)
	}

	mixed := Range{Start: NewEPUBLocator(1, "p[1]", 0), End: NewPDFLocator(1, 0)}
	if err := mixed.Validate(); !errors.Is(err, ErrInvalidLocator) {
		t.Fatalf("mixed-format range error = %v, want ErrInvalidLocator", err)
	}
}

// The reported bug: a highlight from the 9th paragraph to the 10th runs
// forwards, but "p[10]" sorts before "p[9]" as a string, so the old
// string-compare Validate rejected it as backwards.
func TestRangeValidateAcceptsDoubleDigitSiblingOrder(t *testing.T) {
	forwards := Range{Start: NewEPUBLocator(0, "p[9]", 0), End: NewEPUBLocator(0, "p[10]", 5)}
	if err := forwards.Validate(); err != nil {
		t.Fatalf("p[9] to p[10] rejected as backwards: %v", err)
	}

	// And a range that is genuinely backwards between two siblings of the same
	// tag - where the index makes the order decidable - must still be refused.
	backwards := Range{Start: NewEPUBLocator(0, "p[10]", 5), End: NewEPUBLocator(0, "p[9]", 0)}
	if err := backwards.Validate(); !errors.Is(err, ErrInvalidLocator) {
		t.Fatalf("p[10] to p[9] error = %v, want ErrInvalidLocator (it really does run backwards)", err)
	}
}

// The second bug: a compareElementPath that reports 0 for "undecided" is not
// the same thing as reporting 0 for "equal", and Compare's own fallback to
// the char offset conflated the two. A range whose paths are undecided but
// whose raw offsets happen to look backwards was refused - stricter than the
// database CHECK in migration 00015, which accepts any range whose start and
// end paths are not textually identical, offsets be damned. Every case here
// has start offset > end offset, specifically so an implementation that
// silently falls back to comparing them cannot pass by accident - which is
// exactly what hid this the first time.
func TestRangeValidateAcceptsUndecidedForwardRangesRegardlessOfOffset(t *testing.T) {
	tests := []struct {
		name       string
		start, end Locator
	}{
		// Migration 00015's own motivating example.
		{"a paragraph into an anchor's nested paragraph",
			NewEPUBLocator(0, "body[1]/p[74]", 200), NewEPUBLocator(0, "body[1]/a[29]/p[4]", 10)},
		{"a paragraph into a blockquote's nested paragraph",
			NewEPUBLocator(0, "body[1]/p[3]", 120), NewEPUBLocator(0, "body[1]/blockquote[1]/p[1]", 15)},
		{"a paragraph into a heading",
			NewEPUBLocator(0, "body[1]/p[3]", 120), NewEPUBLocator(0, "body[1]/h2[1]", 15)},
		{"a div's own text into its nested paragraph",
			NewEPUBLocator(0, "body[1]/div[1]", 50), NewEPUBLocator(0, "body[1]/div[1]/p[1]", 3)},
		// The regression: accepted by the old plain string.Compare (that
		// implementation had an opinion, even if an arbitrary one, on every
		// pair of paths), refused by an implementation that decides p[9]/
		// p[10] correctly but then folds every OTHER undecided case into
		// "equal, offset decides" instead of leaving it undecided.
		{"a blockquote's nested paragraph into an unrelated top-level paragraph",
			NewEPUBLocator(0, "blockquote[1]/p[1]", 80), NewEPUBLocator(0, "p[5]", 10)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := Range{Start: tt.start, End: tt.end}
			if err := r.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want accepted (undecided path order, not backwards)", err)
			}
		})
	}
}

func TestCompareDecided(t *testing.T) {
	tests := []struct {
		name        string
		a, b        Locator
		wantCmp     int
		wantDecided bool
	}{
		{"same tag, numeric index decides", NewEPUBLocator(0, "p[9]", 0), NewEPUBLocator(0, "p[10]", 0), -1, true},
		{"identical paths, offset decides", NewEPUBLocator(0, "p[1]", 5), NewEPUBLocator(0, "p[1]", 9), -1, true},
		{"different tags, undecided", NewEPUBLocator(0, "p[3]", 999), NewEPUBLocator(0, "h2[1]", 0), 0, false},
		{"a prefix of the other, undecided", NewEPUBLocator(0, "div[1]", 999), NewEPUBLocator(0, "div[1]/p[1]", 0), 0, false},
		{"different spine index always decides", NewEPUBLocator(2, "p[9]", 999), NewEPUBLocator(3, "p[1]", 0), -1, true},
		{"different formats never decide", NewEPUBLocator(1, "p[1]", 0), NewPDFLocator(1, 0), 0, false},
		{"pdf pages decide by page", NewPDFLocator(9, 999), NewPDFLocator(10, 0), -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmp, decided := tt.a.CompareDecided(tt.b)
			if decided != tt.wantDecided {
				t.Fatalf("decided = %v, want %v", decided, tt.wantDecided)
			}
			if decided && cmp != tt.wantCmp {
				t.Fatalf("cmp = %d, want %d", cmp, tt.wantCmp)
			}
		})
	}
}

func TestOnlyTextFormatsAreLocatable(t *testing.T) {
	locatable := []Format{FormatEPUB, FormatPDF}
	notLocatable := []Format{FormatCBZ, FormatCBR, FormatMOBI, FormatAZW3, Format("")}

	for _, f := range locatable {
		if !f.Locatable() {
			t.Errorf("%q should be locatable", f)
		}
	}
	for _, f := range notLocatable {
		if f.Locatable() {
			t.Errorf("%q should not be locatable", f)
		}
		// And a locator must not be mintable for it: a chunk or highlight
		// anchored to a comic book has nothing to anchor to.
		l := Locator{Format: f, CharOffset: 0}
		if err := l.Validate(); !errors.Is(err, ErrInvalidLocator) {
			t.Errorf("Locator{Format: %q}.Validate() = %v, want ErrInvalidLocator", f, err)
		}
	}
}

func TestRangeContains(t *testing.T) {
	at := func(spine int, path string, offset int) Locator {
		return Locator{Format: FormatEPUB, SpineIndex: spine, ElementPath: path, CharOffset: offset}
	}
	span := Range{Start: at(3, "/body/p[2]", 10), End: at(3, "/body/p[5]", 40)}

	tests := []struct {
		name string
		loc  Locator
		want bool
	}{
		{"start is inside", at(3, "/body/p[2]", 10), true},
		{"middle is inside", at(3, "/body/p[3]", 0), true},
		{"before the start offset", at(3, "/body/p[2]", 9), false},
		{"end is exclusive", at(3, "/body/p[5]", 40), false},
		{"just before the end", at(3, "/body/p[5]", 39), true},
		{"a later spine item", at(4, "/body/p[1]", 0), false},
		{"an earlier spine item", at(2, "/body/p[9]", 0), false},

		// Compare reports 0 across formats, which would read as "equal" and let
		// a PDF locator fall inside an EPUB chunk. It must not.
		{"a PDF locator is never inside an EPUB range",
			Locator{Format: FormatPDF, Page: 3, CharOffset: 20}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := span.Contains(tt.loc); got != tt.want {
				t.Fatalf("Contains(%s) = %v, want %v", tt.loc, got, tt.want)
			}
		})
	}
}

// A chunk very often starts at a heading and runs through the paragraphs
// nested under it - exactly the shape CompareDecided cannot structurally
// order. Contains uses Compare, not CompareDecided, specifically so this
// still counts as contained: eval's Recall@10/MRR scoring is its only
// caller today, and undercounting a chunk that plainly does hold the
// expected passage would read as a retrieval regression that never happened.
func TestRangeContainsAcrossAHeadingIntoItsNestedParagraphs(t *testing.T) {
	chunk := Range{
		Start: Locator{Format: FormatEPUB, SpineIndex: 2, ElementPath: "body[1]/h2[1]", CharOffset: 0},
		End:   Locator{Format: FormatEPUB, SpineIndex: 3, ElementPath: "body[1]/h2[1]", CharOffset: 0},
	}
	deep := Locator{Format: FormatEPUB, SpineIndex: 2, ElementPath: "body[1]/p[9]", CharOffset: 40}
	if !chunk.Contains(deep) {
		t.Fatalf("Contains(%s) = false, want true: a paragraph nested under the chunk's own heading", deep)
	}
}
