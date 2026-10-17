// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"fmt"
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// paragraph builds a paragraph of roughly the requested token count.
func paragraph(tokens int, word string) string {
	// Four characters per token, and a word plus its space is five.
	return strings.TrimSpace(strings.Repeat(word+" ", tokens*4/(len(word)+1)+1))
}

// epubUnits turns paragraph-separated text into units of blocks, the way the
// extractor does.
func testUnits(texts ...string) []Unit {
	units := make([]Unit, len(texts))
	for i, t := range texts {
		var blocks []Block
		for j, para := range strings.Split(t, "\n\n") {
			blocks = append(blocks, Block{
				Path: fmt.Sprintf("section[1]/p[%d]", j+1),
				Text: para,
			})
		}
		units[i] = Unit{Index: i, Blocks: blocks, Title: "Chapter " + string(rune('A'+i))}
	}
	return units
}

func TestSplitProducesChunksAroundTheTarget(t *testing.T) {
	body := strings.Join([]string{
		paragraph(200, "alpha"),
		paragraph(200, "beta"),
		paragraph(200, "gamma"),
		paragraph(200, "delta"),
	}, "\n\n")

	chunks := Split(testUnits(body), Options{Format: domain.FormatEPUB})
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks from ~800 tokens with a 450 target", len(chunks))
	}

	for i, c := range chunks {
		if c.TokenCount > DefaultMaxTokens {
			t.Errorf("chunk %d is %d tokens, above the %d ceiling", i, c.TokenCount, DefaultMaxTokens)
		}
		if strings.TrimSpace(c.Content) == "" {
			t.Errorf("chunk %d is empty", i)
		}
		if c.Seq != i {
			t.Errorf("chunk %d has seq %d", i, c.Seq)
		}
	}
}

// The ceiling is set by the embedding server: anything longer is silently
// truncated when embedded, so the chunk would be stored complete and embedded
// partial - and retrieval would match on text the vector never saw.
func TestSplitNeverExceedsTheCeiling(t *testing.T) {
	// One enormous paragraph with no blank lines, as a flattened table or a
	// code listing arrives.
	huge := paragraph(5000, "word")

	chunks := Split(testUnits(huge), Options{Format: domain.FormatEPUB})
	t.Logf("%d chunks from one oversized paragraph", len(chunks))
	if len(chunks) == 0 {
		t.Fatal("an oversized paragraph produced no chunks")
	}
	for i, c := range chunks {
		if c.TokenCount > DefaultMaxTokens {
			t.Errorf("chunk %d is %d tokens, above the %d ceiling", i, c.TokenCount, DefaultMaxTokens)
		}
	}
}

// A passage that starts mid-sentence reads as nonsense when it is quoted back
// as a citation.
func TestSplitBreaksOversizedTextOnSentences(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		b.WriteString("This is a complete sentence about vector spaces and linear maps. ")
	}

	chunks := Split(testUnits(b.String()), Options{Format: domain.FormatEPUB})
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks", len(chunks))
	}

	for i, c := range chunks {
		text := strings.TrimSpace(c.Content)
		if !strings.HasPrefix(text, "This is") {
			t.Errorf("chunk %d starts mid-sentence: %q", i, text[:min(40, len(text))])
		}
	}
}

// Every chunk must be openable in the reader, or a citation is a quotation with
// no source.
func TestChunksCarryLocators(t *testing.T) {
	chunks := Split(testUnits(
		paragraph(300, "alpha"),
		paragraph(300, "beta"),
	), Options{Format: domain.FormatEPUB})

	for i, c := range chunks {
		if err := c.Start.Validate(); err != nil {
			t.Errorf("chunk %d has an invalid start locator %q: %v", i, c.Start, err)
		}
		if err := c.End.Validate(); err != nil {
			t.Errorf("chunk %d has an invalid end locator %q: %v", i, c.End, err)
		}
		if c.End.Compare(c.Start) < 0 {
			t.Errorf("chunk %d ends before it starts: %q .. %q", i, c.Start, c.End)
		}
	}
}

// major means the page for a PDF and the spine index for EPUB, and a chunk
// stored under the wrong one opens the reader in the wrong place.
func TestLocatorsUseTheFormatsOwnPosition(t *testing.T) {
	units := []Unit{{
		Index:  7,
		Blocks: []Block{{Path: "section[1]/p[1]", Text: paragraph(100, "word")}},
	}}

	epub := Split(units, Options{Format: domain.FormatEPUB})
	if epub[0].Start.SpineIndex != 7 || epub[0].Start.Page != 0 {
		t.Errorf("EPUB locator = %q, want spine 7", epub[0].Start)
	}
	if epub[0].Start.ElementPath != "section[1]/p[1]" {
		t.Errorf("EPUB locator lost its element path: %q", epub[0].Start)
	}

	pdf := Split(units, Options{Format: domain.FormatPDF})
	if pdf[0].Start.Page != 7 || pdf[0].Start.SpineIndex != 0 {
		t.Errorf("PDF locator = %q, want page 7", pdf[0].Start)
	}
}

// A PDF page is far smaller than a useful passage, so chunks have to be allowed
// to span units or every citation is half a sentence.
func TestChunksMaySpanUnits(t *testing.T) {
	// Twenty small pages, as a PDF produces.
	var units []Unit
	for i := 1; i <= 20; i++ { // pages are one-based
		units = append(units, Unit{
			Index:  i,
			Blocks: []Block{{Text: paragraph(60, "page")}},
		})
	}

	chunks := Split(units, Options{Format: domain.FormatPDF})
	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}

	spanning := 0
	for _, c := range chunks {
		if c.End.Page != c.Start.Page {
			spanning++
		}
	}
	if spanning == 0 {
		t.Error("no chunk spanned more than one page; every citation would be a fragment")
	}
}

// A passage that straddles a boundary must still be retrievable whole from one
// side of it.
func TestOverlapRepeatsTheTail(t *testing.T) {
	marker := "THE DISTINCTIVE SENTENCE ABOUT EIGENVALUES."
	body := strings.Join([]string{
		paragraph(400, "alpha"),
		marker,
		paragraph(400, "beta"),
	}, "\n\n")

	chunks := Split(testUnits(body), Options{Format: domain.FormatEPUB, OverlapTokens: 40})

	appearances := 0
	for _, c := range chunks {
		if strings.Contains(c.Content, marker) {
			appearances++
		}
	}
	if appearances < 2 {
		t.Errorf("the boundary sentence appears in %d chunks, want at least 2", appearances)
	}
}

// Overlap must not let one paragraph propagate through a whole book.
func TestOverlapDoesNotCascade(t *testing.T) {
	marker := "CARRIED."
	body := marker + "\n\n" + strings.Join([]string{
		paragraph(400, "alpha"), paragraph(400, "beta"),
		paragraph(400, "gamma"), paragraph(400, "delta"),
	}, "\n\n")

	chunks := Split(testUnits(body), Options{Format: domain.FormatEPUB})

	appearances := 0
	for _, c := range chunks {
		if strings.Contains(c.Content, marker) {
			appearances++
		}
	}
	if appearances > 2 {
		t.Errorf("the first paragraph appears in %d chunks; overlap is cascading", appearances)
	}
}

func TestOverlapCanBeDisabled(t *testing.T) {
	body := strings.Join([]string{
		paragraph(400, "alpha"), paragraph(400, "beta"), paragraph(400, "gamma"),
	}, "\n\n")

	chunks := Split(testUnits(body), Options{Format: domain.FormatEPUB, OverlapTokens: -1})

	seen := map[string]bool{}
	for _, c := range chunks {
		for _, para := range strings.Split(c.Content, "\n\n") {
			if seen[para] {
				t.Error("a paragraph appeared twice with overlap disabled")
			}
			seen[para] = true
		}
	}
}

// The last chunk must not be a copy of the previous one's tail.
func TestNoTrailingDuplicateChunk(t *testing.T) {
	body := strings.Join([]string{paragraph(400, "alpha"), paragraph(200, "beta")}, "\n\n")

	chunks := Split(testUnits(body), Options{Format: domain.FormatEPUB})
	if len(chunks) < 2 {
		return
	}

	last := chunks[len(chunks)-1]
	previous := chunks[len(chunks)-2]
	if strings.Contains(previous.Content, last.Content) {
		t.Error("the final chunk is entirely contained in the one before it")
	}
}

func TestSplitIgnoresEmptyInput(t *testing.T) {
	if got := Split(nil, Options{Format: domain.FormatEPUB}); len(got) != 0 {
		t.Errorf("got %d chunks from no units", len(got))
	}
	if got := Split(testUnits("", "   \n\n  "), Options{Format: domain.FormatEPUB}); len(got) != 0 {
		t.Errorf("got %d chunks from blank units", len(got))
	}
}

func TestChunksCarryTheChapterTitle(t *testing.T) {
	chunks := Split([]Unit{{
		Index: 3, Title: "Eigenvalues",
		Blocks: []Block{{Path: "section[1]/p[1]", Text: paragraph(200, "word")}},
	}}, Options{Format: domain.FormatEPUB})

	if len(chunks) == 0 || chunks[0].ChapterTitle != "Eigenvalues" {
		t.Errorf("chapter title = %q", chunks[0].ChapterTitle)
	}
}

// Offsets are measured against the unit's original text, so a locator is not
// off by the leading whitespace of every paragraph.
func TestOffsetsPointAtTheRealPosition(t *testing.T) {
	marker := "FINDME"
	// Leading whitespace on the block, as sanitised HTML routinely carries.
	blockText := "\n   " + marker + " and the rest of the paragraph."

	chunks := Split([]Unit{{
		Index:  0,
		Blocks: []Block{{Path: "section[1]/p[1]", Text: blockText}},
	}}, Options{Format: domain.FormatEPUB})

	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	// The offset is into the BLOCK, which is what an annotation anchor means
	// too, so a citation resolves the same way a highlight does.
	if got := blockText[chunks[0].Start.CharOffset:]; !strings.HasPrefix(got, marker) {
		t.Errorf("start offset %d points at %q, want it at %q",
			chunks[0].Start.CharOffset, got[:min(20, len(got))], marker)
	}
}

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Error("an empty string is not zero tokens")
	}
	// Never zero for real text: a zero would violate the database's own check.
	if EstimateTokens("a") < 1 {
		t.Error("a short string estimated as zero tokens")
	}
	if n := EstimateTokens(strings.Repeat("x", 400)); n < 50 || n > 200 {
		t.Errorf("400 characters estimated as %d tokens", n)
	}
}

// Extracted book text contains NUL bytes - PDF text layers and sanitised EPUB
// markup both produce them - and Postgres text cannot hold one at all. A single
// NUL anywhere in a 900-page book failed the whole file.
func TestScrubRemovesUnstorableCharacters(t *testing.T) {
	text := "Before\x00after\x07and\ufeffmore."

	chunks := Split([]Unit{{
		Index:  0,
		Blocks: []Block{{Path: "p[1]", Text: text}},
	}}, Options{Format: domain.FormatEPUB})

	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	for _, r := range chunks[0].Content {
		if r == 0 || r == '\a' || r == '\ufeff' {
			t.Errorf("unstorable character %q survived into %q", r, chunks[0].Content)
		}
	}
	// Words either side must not run together, or the text is changed rather
	// than cleaned.
	if !strings.Contains(chunks[0].Content, "Before after") {
		t.Errorf("content = %q, want the words kept apart", chunks[0].Content)
	}
}

func TestScrubKeepsOrdinaryText(t *testing.T) {
	text := "A normal paragraph.\nWith a line break\tand a tab."
	if got := scrub(text); got != text {
		t.Errorf("scrub changed ordinary text: %q", got)
	}
}

// The character ceiling is the only real guard, because the token estimate
// cannot be trusted to agree with the model's tokeniser. It has to hold
// exactly, including for the overlap carried into each chunk.
func TestCharacterCeilingHoldsExactly(t *testing.T) {
	// Paragraphs of assorted sizes, so chunk boundaries fall in many places.
	var paras []string
	for i := 1; i <= 60; i++ {
		paras = append(paras, paragraph(40+i*7, "word"))
	}
	body := strings.Join(paras, "\n\n")

	for _, limit := range []int{500, 1000, 2000} {
		chunks := Split(testUnits(body), Options{
			Format: domain.FormatEPUB, MaxCharacters: limit,
		})
		if len(chunks) == 0 {
			t.Fatalf("limit %d produced no chunks", limit)
		}
		for i, c := range chunks {
			if len(c.Content) > limit {
				t.Errorf("limit %d: chunk %d is %d characters", limit, i, len(c.Content))
			}
		}
	}
}

// A chapter is not one unit, so the heading has to carry forward.
//
// Nothing set Unit.Title before this, and chapter_title was NULL on all 81,230
// chunks of a real library: passage labels degraded to the bare book title, so
// ten passages from one book reached the model and the reader as ten identical
// labels. Verified against a real EPUB afterwards - 100 of its 103 spine items
// resolve to a heading, the three that do not being front matter.
func TestATableOfContentsTitlesTheUnitsUnderIt(t *testing.T) {
	toc := []ingest.TOCEntry{
		{Title: "Preface", SpineIndex: 3},
		{Title: "1. Beginnings", SpineIndex: 4, Children: []ingest.TOCEntry{
			// A sub-heading pointing at the same place must not displace the
			// chapter it sits under.
			{Title: "1.1 A Digression", SpineIndex: 4},
		}},
		{Title: "2. Endings", SpineIndex: 9},
		// Real books contain these, and they are not an error.
		{Title: "Nowhere", SpineIndex: -1},
		{Title: "   ", SpineIndex: 11},
	}

	titles := titlesByUnit(toc)

	for unit, want := range map[int]string{
		2:  "",              // before anything is titled
		3:  "Preface",       // its own
		4:  "1. Beginnings", // the chapter, not its sub-heading
		6:  "1. Beginnings", // carried forward through the chapter
		9:  "2. Endings",    // its own again
		40: "2. Endings",    // and onwards to the end of the book
		11: "2. Endings",    // a blank title is not a title
	} {
		if got := titleAt(titles, unit); got != want {
			t.Errorf("unit %d is titled %q, want %q", unit, got, want)
		}
	}
}
