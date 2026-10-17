// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

func epubPosition(spine int, path string, offset int) domain.Locator {
	return domain.Locator{
		Format:      domain.FormatEPUB,
		SpineIndex:  spine,
		ElementPath: path,
		CharOffset:  offset,
	}
}

func save(t *testing.T, p *Pool, userID, bookFileID uuid.UUID, pos domain.Locator, percent float32) domain.ReadingProgress {
	t.Helper()
	out, err := p.SaveProgress(t.Context(), domain.ReadingProgress{
		UserID:     userID,
		BookFileID: bookFileID,
		Position:   pos,
		Percent:    &percent,
	})
	if err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	return out
}

// The bug this split exists for.
//
// Under the old furthest-wins rule, opening the contents, jumping back to an
// earlier chapter, and closing the book reopened it at the furthest chapter,
// because the backward save was discarded. Turning back a page is reading, and
// reading is what gets recorded.
func TestSaveProgressRecordsMovingBackwards(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(69, "section[1]/p[1]", 0), 0.66)
	back := save(t, p, userID, bookFileID, epubPosition(11, "section[1]/p[2]", 40), 0.10)

	if back.Position.SpineIndex != 11 {
		t.Errorf("current spine = %d, want 11: a backward move was discarded", back.Position.SpineIndex)
	}

	// And it survives a read, which is the path the reader actually reopens on.
	read, err := p.ReadingProgress(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("ReadingProgress: %v", err)
	}
	if read.Position.String() != "epub:11/section[1]/p[2]:40" {
		t.Errorf("reopened at %q, want epub:11/section[1]/p[2]:40", read.Position)
	}
}

// The property the old rule was protecting, which must survive the split: a
// device that read ahead is not rewound by one that saves an earlier position.
func TestFurthestNeverMovesBackwards(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(69, "section[1]/p[1]", 0), 0.66)
	back := save(t, p, userID, bookFileID, epubPosition(11, "section[1]/p[2]", 40), 0.10)

	if back.Furthest.SpineIndex != 69 {
		t.Errorf("furthest spine = %d, want 69: reading ahead was lost", back.Furthest.SpineIndex)
	}
	if back.FurthestPercent == nil || *back.FurthestPercent < 0.6 {
		t.Errorf("furthest percent = %v, want the deeper 0.66", back.FurthestPercent)
	}
	// The current percent tracks the current position, not the furthest one.
	if back.Percent == nil || *back.Percent > 0.2 {
		t.Errorf("current percent = %v, want the shallower 0.10", back.Percent)
	}
}

func TestFurthestAdvancesWithReading(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(5, "section[1]/p[1]", 0), 0.05)
	on := save(t, p, userID, bookFileID, epubPosition(40, "section[1]/p[3]", 12), 0.40)

	if on.Furthest.SpineIndex != 40 || on.Position.SpineIndex != 40 {
		t.Errorf("reading on gave current %d and furthest %d, want both 40",
			on.Position.SpineIndex, on.Furthest.SpineIndex)
	}
}

// Within one chapter the offset decides, so turning back a page is recorded
// just as turning back a chapter is.
func TestBackwardsWithinAChapterIsRecorded(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(7, "section[1]/p[9]", 900), 0.30)
	back := save(t, p, userID, bookFileID, epubPosition(7, "section[1]/p[2]", 10), 0.28)

	if back.Position.ElementPath != "section[1]/p[2]" || back.Position.CharOffset != 10 {
		t.Errorf("current = %q, want the earlier paragraph", back.Position)
	}
	if back.Furthest.ElementPath != "section[1]/p[9]" {
		t.Errorf("furthest = %q, want the deeper paragraph kept", back.Furthest)
	}
}

// A first save sets both, or the furthest columns would start empty and the
// shelf would show nothing until the second page turn.
func TestFirstSaveSetsBoth(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	first := save(t, p, userID, bookFileID, epubPosition(3, "section[1]/p[1]", 0), 0.03)
	if first.Furthest.String() != first.Position.String() {
		t.Errorf("furthest = %q, current = %q, want them equal on a first save",
			first.Furthest, first.Position)
	}
	if first.FurthestPercent == nil {
		t.Error("the furthest percent was not set on a first save")
	}
}

// major carries the page for PDFs and the spine index for EPUB, in both the
// current and the furthest columns. Getting it wrong on one but not the other
// would send a PDF reader back to page 0.
func TestProgressMajorIsFormatSpecific(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatPDF)

	save(t, p, userID, bookFileID, domain.Locator{Format: domain.FormatPDF, Page: 300, CharOffset: 0}, 0.75)
	back := save(t, p, userID, bookFileID, domain.Locator{Format: domain.FormatPDF, Page: 42, CharOffset: 0}, 0.10)

	if back.Position.Page != 42 || back.Position.SpineIndex != 0 {
		t.Errorf("current page = %d, spine = %d, want 42 and 0", back.Position.Page, back.Position.SpineIndex)
	}
	if back.Furthest.Page != 300 || back.Furthest.SpineIndex != 0 {
		t.Errorf("furthest page = %d, spine = %d, want 300 and 0", back.Furthest.Page, back.Furthest.SpineIndex)
	}

	read, err := p.ReadingProgress(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("ReadingProgress: %v", err)
	}
	if read.Position.String() != "pdf:42:0" || read.Furthest.String() != "pdf:300:0" {
		t.Errorf("read back current %q and furthest %q, want pdf:42:0 and pdf:300:0",
			read.Position, read.Furthest)
	}
}

func TestReadingProgressIsPerUser(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(9, "section[1]/p[1]", 0), 0.09)

	if _, err := p.ReadingProgress(t.Context(), uuid.New(), bookFileID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a stranger read someone else's position: %v", err)
	}
}

func TestReadingProgressNotFoundForAnUnopenedBook(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	_ = bookFileID
	if _, err := p.ReadingProgress(t.Context(), userID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unopened book returned %v, want ErrNotFound", err)
	}
}

// The shelf answers "how far through this am I?", so it shows the furthest
// position. Opening the book still resumes wherever the reader actually was.
func TestContinueReadingShowsFurthest(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(69, "section[1]/p[1]", 0), 0.66)
	save(t, p, userID, bookFileID, epubPosition(11, "section[1]/p[2]", 40), 0.10)

	items, err := p.ContinueReading(t.Context(), userID, 10)
	if err != nil {
		t.Fatalf("ContinueReading: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Percent == nil || *items[0].Percent < 0.6 {
		t.Errorf("shelf percent = %v, want the furthest 0.66", items[0].Percent)
	}
	if items[0].BookFileID != bookFileID {
		t.Error("the entry did not carry its book file")
	}
}

// The bug: SQL compared (major, path, offset) as a row, which orders path as
// TEXT - "p[10]" sorts before "p[9]" - so turning the page from the 9th
// paragraph to the 10th did not advance "how far they got" at all, and SQL
// and Go were free to disagree about which of two positions was further.
func TestFurthestAdvancesAcrossDoubleDigitParagraphs(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(7, "section[1]/p[9]", 300), 0.30)
	on := save(t, p, userID, bookFileID, epubPosition(7, "section[1]/p[10]", 5), 0.31)

	if on.Furthest.ElementPath != "section[1]/p[10]" {
		t.Errorf("furthest = %q, want section[1]/p[10]: turning the page from p[9] to p[10] did not advance it",
			on.Furthest.ElementPath)
	}
	if on.FurthestPercent == nil || *on.FurthestPercent < 0.305 {
		t.Errorf("furthest percent = %v, want the later 0.31", on.FurthestPercent)
	}
}

// And a genuine step back, within the same paragraph, must still not move it.
func TestFurthestDoesNotAdvanceOnAGenuineStepBack(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(7, "section[1]/p[10]", 300), 0.31)
	back := save(t, p, userID, bookFileID, epubPosition(7, "section[1]/p[9]", 5), 0.30)

	if back.Furthest.ElementPath != "section[1]/p[10]" {
		t.Errorf("furthest = %q, want section[1]/p[10] kept: a real step back must not advance it",
			back.Furthest.ElementPath)
	}
	if back.FurthestPercent == nil || *back.FurthestPercent < 0.305 {
		t.Errorf("furthest percent = %v, want the earlier 0.31 kept", back.FurthestPercent)
	}
}

// The bug: a chapter's own heading is saved first, every time a reader turns
// a page into it - before any paragraph under it. Structurally, a heading
// and the paragraphs nested inside it are undecided against each other (see
// CompareDecided), so with no fallback but "keep the stored value", furthest
// froze at the heading the moment it was saved and never moved again until
// the reader crossed into the NEXT spine item. On a single-spine EPUB that
// means the shelf percentage sits near the chapter's own opening percent for
// the entire rest of the book.
//
// This reproduces the reviewer's own probe: spine 4 settles reading at
// p[30], the reader turns the page into spine 5 and its own heading is saved
// at 21%, and reading then proceeds through p[1]..p[41] up to 30% - which
// must actually show as 30%, not stay frozen at the heading's own 21%.
func TestFurthestAdvancesFromAChapterHeadingIntoItsParagraphs(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(4, "section[1]/p[30]", 0), 0.20)
	save(t, p, userID, bookFileID, epubPosition(5, "section[1]/h2[1]", 0), 0.21)

	var last domain.ReadingProgress
	for i := 1; i <= 41; i++ {
		percent := 0.21 + float32(i)*(0.30-0.21)/41
		last = save(t, p, userID, bookFileID, epubPosition(5, fmt.Sprintf("section[1]/p[%d]", i), 0), percent)
	}

	if last.Furthest.ElementPath == "section[1]/h2[1]" {
		t.Fatal("furthest is still the chapter heading: it never advanced past the first save of the chapter")
	}
	if last.FurthestPercent == nil || *last.FurthestPercent < 0.29 {
		t.Errorf("furthest percent = %v, want it to have caught up to roughly 0.30", last.FurthestPercent)
	}
}

// A genuine step back is still recognisable by percent alone, with no
// structural signal available at all.
func TestFurthestByPercentDoesNotAdvanceOnAStepBack(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(5, "section[1]/p[9]", 0), 0.40)
	back := save(t, p, userID, bookFileID, epubPosition(5, "section[1]/h2[1]", 0), 0.10)

	if back.Furthest.ElementPath != "section[1]/p[9]" {
		t.Errorf("furthest = %q, want section[1]/p[9] kept: a real step back must not advance it by percent either",
			back.Furthest.ElementPath)
	}
	if back.FurthestPercent == nil || *back.FurthestPercent < 0.35 {
		t.Errorf("furthest percent = %v, want the earlier 0.40 kept", back.FurthestPercent)
	}
}

// With no percent to fall back on either, the only honest choice left is to
// leave the stored furthest exactly alone.
func TestFurthestKeepsStoredWhenNeitherSaveHasAPercent(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(5, "section[1]/p[9]", 0), 0.40)

	out, err := p.SaveProgress(t.Context(), domain.ReadingProgress{
		UserID:     userID,
		BookFileID: bookFileID,
		Position:   epubPosition(5, "section[1]/h2[1]", 0),
		// No percent at all.
	})
	if err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	if out.Furthest.ElementPath != "section[1]/p[9]" {
		t.Errorf("furthest = %q, want section[1]/p[9] kept when neither save carries a percent",
			out.Furthest.ElementPath)
	}
	if out.FurthestPercent == nil || *out.FurthestPercent < 0.35 {
		t.Errorf("furthest percent = %v, want the earlier 0.40 kept", out.FurthestPercent)
	}
}

// A spine change is still decided by the spine index itself, percent or not
// - structure answers first, and does not need percent's help here.
func TestFurthestSpineChangeStillDecidedStructurally(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	save(t, p, userID, bookFileID, epubPosition(5, "section[1]/p[9]", 0), 0.90)
	on := save(t, p, userID, bookFileID, epubPosition(6, "section[1]/h2[1]", 0), 0.10)

	if on.Furthest.SpineIndex != 6 {
		t.Errorf("furthest spine = %d, want 6: a later spine item is further regardless of percent",
			on.Furthest.SpineIndex)
	}
}

// The bug: furthestFor's SELECT ... FOR UPDATE locks a ROW, and a row that
// does not exist yet cannot be locked. Two FIRST-ever saves for the same book
// racing each other both found no row, both decided their own position was
// the furthest with nothing yet to compare against, and both proceeded to
// INSERT ... ON CONFLICT DO UPDATE - whichever committed second overwrote
// furthest unconditionally, even when it was the one that was actually
// behind. lockReadingProgress closes the gap with an advisory lock keyed on
// the pair, taken before either save reads anything.
//
// Races are not reliably reproduced by one attempt, so this runs the same
// race many times over independent books - the reviewer's own probe found it
// in 8 of 40 runs.
func TestConcurrentFirstSavesDoNotRewindFurthest(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	userID, err := p.EnsureDefaultUser(ctx)
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}

	const rounds = 40
	for i := 0; i < rounds; i++ {
		// A valid-looking SHA-256, unique per round: 64 lowercase hex
		// characters, as the CHECK constraint requires - hashOf's own
		// repeat-the-seed trick only produces hex when the seed is a single
		// hex digit, which "race0", "race1", ... is not.
		hash := fmt.Sprintf("%064x", i+1)
		in := sampleInput(hash, fmt.Sprintf("/library/race-%d.epub", i), "Race", "Author")
		in.OwnerID = userID
		book, err := p.UpsertBookFile(ctx, in)
		if err != nil {
			t.Fatalf("round %d: UpsertBookFile: %v", i, err)
		}

		ahead := epubPosition(9, "section[1]/p[1]", 0)
		behind := epubPosition(1, "section[1]/p[1]", 0)

		start := make(chan struct{})
		done := make(chan error, 2)
		save := func(pos domain.Locator) {
			<-start
			_, err := p.SaveProgress(ctx, domain.ReadingProgress{
				UserID: userID, BookFileID: book.BookFileID, Position: pos,
			})
			done <- err
		}
		go save(ahead)
		go save(behind)
		close(start)
		for j := 0; j < 2; j++ {
			if err := <-done; err != nil {
				t.Fatalf("round %d: SaveProgress: %v", i, err)
			}
		}

		got, err := p.ReadingProgress(ctx, userID, book.BookFileID)
		if err != nil {
			t.Fatalf("round %d: ReadingProgress: %v", i, err)
		}
		if got.Furthest.SpineIndex != 9 {
			t.Fatalf("round %d: furthest spine = %d, want 9: a concurrent first save moved it backwards",
				i, got.Furthest.SpineIndex)
		}
	}
}

func TestSaveProgressRejectsAnInvalidPosition(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	_, err := p.SaveProgress(t.Context(), domain.ReadingProgress{
		UserID:     userID,
		BookFileID: bookFileID,
		Position:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: -1},
	})
	if err == nil {
		t.Fatal("a negative spine index was accepted")
	}
}

// A position is a fact about reading a file, so somebody who cannot open the
// file has none to save - and the refusal is the one an absent file gets, so
// it does not say which of the two it was.
func TestSaveProgressRefusesAFileTheReaderCannotOpen(t *testing.T) {
	p := testPool(t)
	_, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	stranger := secondUser(t, p)

	for name, fileID := range map[string]uuid.UUID{
		"somebody else's file":       bookFileID,
		"a file that does not exist": uuid.New(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.SaveProgress(t.Context(), domain.ReadingProgress{
				UserID:     stranger,
				BookFileID: fileID,
				Position:   epubPosition(1, "section[1]/p[1]", 0),
			})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("SaveProgress = %v, want ErrNotFound", err)
			}
			var n int
			if err := p.QueryRow(t.Context(),
				`SELECT count(*) FROM reading_progress WHERE user_id = $1`, stranger).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%d positions stored for somebody who cannot open the file", n)
			}
		})
	}
}
