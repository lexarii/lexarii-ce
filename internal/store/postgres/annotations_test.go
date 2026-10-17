// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// annotationFixture gives a test a user and a book file to hang marks off.
func annotationFixture(t *testing.T, p *Pool, format domain.Format) (uuid.UUID, uuid.UUID) {
	t.Helper()
	cleanLibrary(t, p)
	ctx := t.Context()

	userID, err := p.EnsureDefaultUser(ctx)
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}

	in := sampleInput(hashOf("d"), "/library/annotated.epub", "Annotated", "An Author")
	in.Format = format
	in.Result.Format = format
	// Books belong to people, and a mark cannot be made on one nobody can open.
	in.OwnerID = userID
	book, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	return userID, book.BookFileID
}

func epubMark(userID, bookFileID uuid.UUID, spine int, path string, from, to int) domain.Annotation {
	return domain.Annotation{
		UserID:     userID,
		BookFileID: bookFileID,
		Type:       domain.AnnotationHighlight,
		Range: domain.Range{
			Start: domain.Locator{Format: domain.FormatEPUB, SpineIndex: spine, ElementPath: path, CharOffset: from},
			End:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: spine, ElementPath: path, CharOffset: to},
		},
		SelectedText: "a passage",
	}
}

func pdfMark(userID, bookFileID uuid.UUID, page, occurrence int) domain.Annotation {
	return domain.Annotation{
		UserID:     userID,
		BookFileID: bookFileID,
		Type:       domain.AnnotationHighlight,
		Range: domain.Range{
			Start: domain.Locator{Format: domain.FormatPDF, Page: page, CharOffset: occurrence},
			End:   domain.Locator{Format: domain.FormatPDF, Page: page, CharOffset: occurrence},
		},
		SelectedText: "a passage",
	}
}

func TestCreateAnnotationRoundTripsTheLocator(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	in := epubMark(userID, bookFileID, 11, "section[1]/p[2]", 10, 80)
	out, err := p.CreateAnnotation(t.Context(), in)
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	if out.Range.Start.String() != in.Range.Start.String() {
		t.Errorf("start = %q, want %q", out.Range.Start, in.Range.Start)
	}
	if out.Range.End.String() != in.Range.End.String() {
		t.Errorf("end = %q, want %q", out.Range.End, in.Range.End)
	}
	if out.Orphaned() {
		t.Error("a new mark was born orphaned")
	}
}

// The stored `major` column is the spine index for EPUB and the page for PDF,
// and it scans back into SpineIndex whatever it was. Getting this wrong puts a
// PDF highlight on page 0 of every document, so it is asserted directly.
func TestAnnotationMajorIsFormatSpecific(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatPDF)

	in := domain.Annotation{
		UserID:     userID,
		BookFileID: bookFileID,
		Type:       domain.AnnotationHighlight,
		Range: domain.Range{
			Start: domain.Locator{Format: domain.FormatPDF, Page: 42, CharOffset: 5},
			End:   domain.Locator{Format: domain.FormatPDF, Page: 42, CharOffset: 60},
		},
		SelectedText: "from a scanned page",
	}
	created, err := p.CreateAnnotation(t.Context(), in)
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	if created.Range.Start.Page != 42 || created.Range.Start.SpineIndex != 0 {
		t.Fatalf("page = %d, spineIndex = %d, want 42 and 0",
			created.Range.Start.Page, created.Range.Start.SpineIndex)
	}

	// And again when read back, which is a different code path.
	marks, err := p.Annotations(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 {
		t.Fatalf("got %d marks, want 1", len(marks))
	}
	if marks[0].Range.Start.Page != 42 {
		t.Errorf("read back page = %d, want 42", marks[0].Range.Start.Page)
	}
	if got := marks[0].Range.Start.String(); got != "pdf:42:5" {
		t.Errorf("locator = %q, want pdf:42:5", got)
	}
}

func TestAnnotationsAreInReadingOrder(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()

	// Inserted out of order on purpose.
	for _, m := range []domain.Annotation{
		epubMark(userID, bookFileID, 11, "section[1]/p[2]", 10, 20),
		epubMark(userID, bookFileID, 2, "section[1]/p[9]", 0, 5),
		epubMark(userID, bookFileID, 11, "section[1]/p[2]", 90, 99),
		epubMark(userID, bookFileID, 2, "section[1]/p[1]", 0, 5),
	} {
		if _, err := p.CreateAnnotation(ctx, m); err != nil {
			t.Fatalf("CreateAnnotation: %v", err)
		}
	}

	marks, err := p.Annotations(ctx, userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}

	var got []string
	for _, m := range marks {
		got = append(got, m.Range.Start.String())
	}
	want := []string{
		"epub:2/section[1]/p[1]:0",
		"epub:2/section[1]/p[9]:0",
		"epub:11/section[1]/p[2]:10",
		"epub:11/section[1]/p[2]:90",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d marks, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			// Spine 11 sorting before spine 2 is the string-ordering bug the
			// separate major/path/offset columns exist to prevent.
			t.Fatalf("order[%d] = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// The anchor is deliberately not editable: moving a highlight to different text
// would detach the note from the words it was written about.
func TestUpdateAnnotationLeavesTheAnchorAlone(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()

	created, err := p.CreateAnnotation(ctx, epubMark(userID, bookFileID, 3, "section[1]/p[1]", 0, 10))
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	updated, err := p.UpdateAnnotation(ctx, userID, created.ID, "a thought", "amber")
	if err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}
	if updated.Note != "a thought" || updated.Colour != "amber" {
		t.Errorf("note = %q, colour = %q", updated.Note, updated.Colour)
	}
	if updated.Range.Start.String() != created.Range.Start.String() {
		t.Errorf("the anchor moved: %q -> %q", created.Range.Start, updated.Range.Start)
	}

	// An empty colour keeps the one already stored rather than clearing it.
	kept, err := p.UpdateAnnotation(ctx, userID, created.ID, "", "")
	if err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}
	if kept.Colour != "amber" {
		t.Errorf("colour = %q, want it kept as amber", kept.Colour)
	}
	if kept.Note != "" {
		t.Errorf("note = %q, want it cleared", kept.Note)
	}
}

// A guessed id must not delete someone else's note.
func TestDeleteAnnotationIsScopedToTheOwner(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()

	created, err := p.CreateAnnotation(ctx, epubMark(userID, bookFileID, 1, "section[1]/p[1]", 0, 10))
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	if err := p.DeleteAnnotation(ctx, uuid.New(), created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a stranger's delete returned %v, want ErrNotFound", err)
	}
	marks, err := p.Annotations(ctx, userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 {
		t.Fatalf("the mark was deleted by someone who did not own it")
	}

	if err := p.DeleteAnnotation(ctx, userID, created.ID); err != nil {
		t.Fatalf("the owner's delete: %v", err)
	}
	if err := p.DeleteAnnotation(ctx, userID, created.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting twice returned %v, want ErrNotFound", err)
	}
}

// Orphaning must be reversible. A transient render failure that flagged a mark
// permanently would be indistinguishable from losing it.
func TestOrphanFlagRoundTrips(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()

	created, err := p.CreateAnnotation(ctx, epubMark(userID, bookFileID, 1, "section[1]/p[1]", 0, 10))
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	count, err := p.MarkAnnotationsOrphaned(ctx, userID, []uuid.UUID{created.ID})
	if err != nil {
		t.Fatalf("MarkAnnotationsOrphaned: %v", err)
	}
	if count != 1 {
		t.Fatalf("flagged %d marks, want 1", count)
	}
	// Flagging again changes nothing, so a reader that reports the same
	// unresolved anchor on every page turn does not keep rewriting the row.
	if again, err := p.MarkAnnotationsOrphaned(ctx, userID, []uuid.UUID{created.ID}); err != nil || again != 0 {
		t.Fatalf("re-flagging: count = %d, err = %v, want 0 and no error", again, err)
	}

	marks, err := p.Annotations(ctx, userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if !marks[0].Orphaned() {
		t.Fatal("the mark was not reported as orphaned")
	}
	// The text snapshot is what makes an orphan survivable.
	if marks[0].SelectedText == "" {
		t.Error("an orphan lost the text it was made against")
	}

	if err := p.ClearAnnotationOrphan(ctx, userID, []uuid.UUID{created.ID}); err != nil {
		t.Fatalf("ClearAnnotationOrphan: %v", err)
	}
	marks, err = p.Annotations(ctx, userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if marks[0].Orphaned() {
		t.Error("the flag did not clear when the anchor resolved again")
	}
}

// Another user's marks must never be flagged or cleared by id alone.
func TestOrphanFlagIsScopedToTheOwner(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()

	created, err := p.CreateAnnotation(ctx, epubMark(userID, bookFileID, 1, "section[1]/p[1]", 0, 10))
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	stranger := uuid.New()
	if count, err := p.MarkAnnotationsOrphaned(ctx, stranger, []uuid.UUID{created.ID}); err != nil || count != 0 {
		t.Fatalf("a stranger flagged %d marks (err %v), want 0", count, err)
	}

	if _, err := p.MarkAnnotationsOrphaned(ctx, userID, []uuid.UUID{created.ID}); err != nil {
		t.Fatalf("MarkAnnotationsOrphaned: %v", err)
	}
	if err := p.ClearAnnotationOrphan(ctx, stranger, []uuid.UUID{created.ID}); err != nil {
		t.Fatalf("ClearAnnotationOrphan: %v", err)
	}
	marks, err := p.Annotations(ctx, userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if !marks[0].Orphaned() {
		t.Error("a stranger cleared someone else's orphan flag")
	}
}

// An empty list is a no-op, not an UPDATE with no WHERE.
func TestOrphanCallsIgnoreEmptyLists(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()

	created, err := p.CreateAnnotation(ctx, epubMark(userID, bookFileID, 1, "section[1]/p[1]", 0, 10))
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	if count, err := p.MarkAnnotationsOrphaned(ctx, userID, nil); err != nil || count != 0 {
		t.Fatalf("empty flag: count = %d, err = %v", count, err)
	}
	if err := p.ClearAnnotationOrphan(ctx, userID, nil); err != nil {
		t.Fatalf("empty clear: %v", err)
	}
	marks, err := p.Annotations(ctx, userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 || marks[0].ID != created.ID || marks[0].Orphaned() {
		t.Error("an empty list changed a mark")
	}
}

func TestRecentAnnotationsCarryTheBook(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()

	if _, err := p.CreateAnnotation(ctx, epubMark(userID, bookFileID, 1, "section[1]/p[1]", 0, 10)); err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	page, err := p.RecentAnnotations(ctx, userID, 50, "")
	if err != nil {
		t.Fatalf("RecentAnnotations: %v", err)
	}
	items := page.Items
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Title != "Annotated" {
		t.Errorf("title = %q, want Annotated", items[0].Title)
	}
	if items[0].BookFileID != bookFileID {
		t.Error("the mark did not carry its book file")
	}
	if items[0].WorkID == uuid.Nil {
		t.Error("the mark did not carry its work")
	}

	// The ANNOTATION's own fields, not just the row around it. Every one of
	// these came back zeroed, so a client reading the annotation - which is
	// what the notes view does - could not tell which book it was from, when
	// it was last touched, or where it ended.
	mark := items[0].Annotation
	if mark.BookFileID != bookFileID {
		t.Errorf("annotation.bookFileId = %v, want %v", mark.BookFileID, bookFileID)
	}
	if mark.UserID != userID {
		t.Errorf("annotation.userId = %v, want %v", mark.UserID, userID)
	}
	if mark.UpdatedAt.IsZero() {
		t.Error("annotation.updatedAt is the zero time")
	}
	// Never edited, so edited when it was made - not year one, which the
	// notes view once sent because this list did not read the column.
	if mark.EditedAt.IsZero() || !mark.EditedAt.Equal(mark.CreatedAt) {
		t.Errorf("annotation.editedAt = %v, want its creation time %v", mark.EditedAt, mark.CreatedAt)
	}
	if mark.Range.End.ElementPath == "" {
		t.Errorf("annotation end = %+v, want the range it was created with", mark.Range.End)
	}

	// A stranger sees nothing.
	if other, err := p.RecentAnnotations(ctx, uuid.New(), 50, ""); err != nil || len(other.Items) != 0 {
		t.Errorf("a stranger saw %d marks (err %v)", len(other.Items), err)
	}
}

// The notes view pages through everything, newest first, without repeating
// or skipping a mark - including marks made in the same instant, which only
// the id tells apart - and a note made while somebody pages does not shift
// the pages they have not reached.
func TestRecentAnnotationsPageThroughEverything(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()

	const total = 7
	for i := 0; i < total; i++ {
		if _, err := p.CreateAnnotation(ctx, epubMark(userID, bookFileID, i+1, "p[1]", 0, 5)); err != nil {
			t.Fatalf("CreateAnnotation: %v", err)
		}
	}
	// Three of them share one instant, so the order there is the id's.
	if _, err := p.Exec(ctx, `
		UPDATE annotations SET created_at = '2026-01-01T00:00:00Z'
		WHERE id IN (SELECT id FROM annotations WHERE user_id = $1 ORDER BY id LIMIT 3)`, userID); err != nil {
		t.Fatalf("aligning creation times: %v", err)
	}

	seen := map[uuid.UUID]bool{}
	var order []domain.Annotation
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatal("paging did not end")
		}
		page, err := p.RecentAnnotations(ctx, userID, 2, cursor)
		if err != nil {
			t.Fatalf("RecentAnnotations: %v", err)
		}
		if len(page.Items) > 2 {
			t.Fatalf("page of %d, want at most 2", len(page.Items))
		}
		for _, it := range page.Items {
			if seen[it.Annotation.ID] {
				t.Fatalf("mark %s appeared twice", it.Annotation.ID)
			}
			seen[it.Annotation.ID] = true
			order = append(order, it.Annotation)
		}
		if pages == 0 {
			// Made after the first page was read: newer than everything,
			// so it belongs before it, not on a later page.
			if _, err := p.CreateAnnotation(ctx, epubMark(userID, bookFileID, 99, "p[1]", 0, 5)); err != nil {
				t.Fatalf("CreateAnnotation: %v", err)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(order) != total {
		t.Fatalf("paged through %d marks, want %d", len(order), total)
	}
	for i := 1; i < len(order); i++ {
		a, b := order[i-1], order[i]
		if a.CreatedAt.Before(b.CreatedAt) ||
			(a.CreatedAt.Equal(b.CreatedAt) && a.ID.String() < b.ID.String()) {
			t.Errorf("mark %d (%v %s) came before %d (%v %s)", i-1, a.CreatedAt, a.ID, i, b.CreatedAt, b.ID)
		}
	}

	if _, err := p.RecentAnnotations(ctx, userID, 2, "not a cursor!"); !errors.Is(err, store.ErrInvalidQuery) {
		t.Errorf("a malformed cursor: %v, want store.ErrInvalidQuery", err)
	}
}

// A range that runs backwards is rejected before it reaches the database,
// because a highlight that ends before it starts paints nothing and silently
// disappears from the reader.
func TestCreateAnnotationRejectsABackwardsRange(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	backwards := epubMark(userID, bookFileID, 4, "section[1]/p[1]", 90, 10)
	if _, err := p.CreateAnnotation(t.Context(), backwards); err == nil {
		t.Fatal("a range ending before it starts was accepted")
	}
}

// A reader marking a passage does not know yet whether they are about to write
// anything, so both readers create a highlight and the note promotes it. The
// schema will not accept a note with no body, which is what made the Note
// button fail with a 500 in both readers.
func TestWritingANoteMakesItANote(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	book := oneBook(t, p, owner)
	ctx := t.Context()

	mark, err := p.CreateAnnotation(ctx, epubMark(owner, book, 3, "/body/p[2]", 0, 5))
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	if mark.Type != domain.AnnotationHighlight {
		t.Fatalf("created as %q, want a highlight", mark.Type)
	}

	noted, err := p.UpdateAnnotation(ctx, owner, mark.ID, "worth remembering", "")
	if err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}
	if noted.Type != domain.AnnotationNote {
		t.Errorf("type = %q after a note was written, want note", noted.Type)
	}

	// And clearing it goes back, rather than leaving a note with no body -
	// which the schema forbids outright.
	plain, err := p.UpdateAnnotation(ctx, owner, mark.ID, "", "")
	if err != nil {
		t.Fatalf("clearing the note: %v", err)
	}
	if plain.Type != domain.AnnotationHighlight {
		t.Errorf("type = %q after the note was cleared, want highlight", plain.Type)
	}
}

// A note is the most personal thing here, and a shared book does not share the
// margins. Only the person who wrote one ever finds it.
func TestSearchNotesFindsOnlyYourOwn(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	stranger := secondUser(t, p)
	ctx := t.Context()

	mine := epubMark(userID, bookFileID, 1, "section[1]/p[1]", 0, 10)
	mine.SelectedText = "the continuous delivery build pipeline"
	created, err := p.CreateAnnotation(ctx, mine)
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	if _, err := p.UpdateAnnotation(ctx, userID, created.ID,
		"Codify architectural rules in CI, not in a weekly review.", ""); err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}

	found, err := p.SearchNotes(ctx, userID, "should architectural rules be enforced in CI?", 3)
	if err != nil {
		t.Fatalf("SearchNotes: %v", err)
	}
	if len(found) != 1 || found[0].Annotation.ID != created.ID {
		t.Fatalf("got %d notes, want the one that was written", len(found))
	}
	if found[0].Title == "" || found[0].BookFileID != bookFileID {
		t.Errorf("the note did not carry its book: %+v", found[0])
	}

	// Somebody else's library, somebody else's margins.
	if other, err := p.SearchNotes(ctx, stranger, "architectural rules in CI", 3); err != nil {
		t.Fatalf("SearchNotes: %v", err)
	} else if len(other) != 0 {
		t.Errorf("a stranger found %d of this person's notes", len(other))
	}
}

// The question's words are ORed, because a note is a sentence and a question
// rarely uses all of its words. websearch_to_tsquery ANDs them, so "is fuzz
// testing worth doing?" failed against a note about fuzz testing on the word
// "worth" alone.
func TestSearchNotesDoesNotNeedEveryWord(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()

	mark := epubMark(userID, bookFileID, 2, "section[1]/p[2]", 0, 10)
	mark.SelectedText = "Fuzz testing is now an essential part"
	created, err := p.CreateAnnotation(ctx, mark)
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	if _, err := p.UpdateAnnotation(ctx, userID, created.ID,
		"Disagree - it is a last resort, not a toolbox staple.", ""); err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}

	found, err := p.SearchNotes(ctx, userID, "is fuzz testing worth doing?", 3)
	if err != nil {
		t.Fatalf("SearchNotes: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("got %d notes, want the one beside the fuzz testing passage", len(found))
	}

	// And the floor keeps an unrelated question from dragging it in, which is
	// what ORing the words would otherwise do.
	unrelated, err := p.SearchNotes(ctx, userID, "what is a monad in Haskell?", 3)
	if err != nil {
		t.Fatalf("SearchNotes: %v", err)
	}
	if len(unrelated) != 0 {
		t.Errorf("an unrelated question found %d notes", len(unrelated))
	}
}

// A highlight with no note is the book's own words, which the passage search
// covers far better than this does.
func TestSearchNotesIgnoresABareHighlight(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()

	mark := epubMark(userID, bookFileID, 3, "section[1]/p[3]", 0, 10)
	mark.SelectedText = "architectural rules enforced in the build pipeline"
	if _, err := p.CreateAnnotation(ctx, mark); err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	found, err := p.SearchNotes(ctx, userID, "architectural rules in the build pipeline", 3)
	if err != nil {
		t.Fatalf("SearchNotes: %v", err)
	}
	if len(found) != 0 {
		t.Errorf("a highlight with no note was returned as one: %+v", found)
	}
}

// A word spelled wrong in the question should still find the note, because
// the person looking for it is the person who wrote it and they are going
// from memory.
func TestSearchNotesForgivesAMisspelling(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()

	mark := epubMark(userID, bookFileID, 4, "section[1]/p[4]", 0, 10)
	mark.SelectedText = "the continuous delivery build pipeline"
	created, err := p.CreateAnnotation(ctx, mark)
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	if _, err := p.UpdateAnnotation(ctx, userID, created.ID,
		"Codify architectural rules in CI, not in a weekly review.", ""); err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}

	// "architectual" is not a word, so no amount of stemming reaches it.
	found, err := p.SearchNotes(ctx, userID, "should architectual rules be enforced in CI?", 3)
	if err != nil {
		t.Fatalf("SearchNotes: %v", err)
	}
	if len(found) != 1 || found[0].Annotation.ID != created.ID {
		t.Fatalf("got %d notes for a misspelled question, want the one that was written", len(found))
	}

	// The fallback must not turn into a net that catches everything: an
	// unrelated question still finds nothing at all.
	unrelated, err := p.SearchNotes(ctx, userID, "what is a monad in Haskell?", 3)
	if err != nil {
		t.Fatalf("SearchNotes: %v", err)
	}
	if len(unrelated) != 0 {
		t.Errorf("an unrelated question found %d notes through the fuzzy fallback", len(unrelated))
	}
}

// Importing twice must not double the marks, and must never touch the ones
// somebody made here.
func TestImportingMarksReplacesOnlyTheImportedOnes(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatPDF)
	ctx := t.Context()

	mine, err := p.CreateAnnotation(ctx, pdfMark(userID, bookFileID, 4, 0))
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	marks := []ImportedMark{
		{Page: 61, Occurrence: 0, Text: "(2.2.3)"},
		{Page: 61, Occurrence: 1, Text: "(2.2.3)"},
		// A rectangle over a picture: no words, nothing written on it.
		{Page: 62, Text: ""},
		// A sticky note quotes nothing and is still worth keeping.
		{Page: 63, Note: "come back to this"},
	}
	stored, err := p.ImportMarks(ctx, userID, bookFileID, marks)
	if err != nil {
		t.Fatalf("ImportMarks: %v", err)
	}
	if stored != 3 {
		t.Errorf("stored %d marks, want 3 - the empty one carries nothing", stored)
	}

	// Again, with fewer: the second import replaces the first rather than
	// adding to it.
	if _, err := p.ImportMarks(ctx, userID, bookFileID, marks[:1]); err != nil {
		t.Fatalf("second ImportMarks: %v", err)
	}

	all, err := p.Annotations(ctx, userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d marks after re-importing, want 1 imported and 1 made here", len(all))
	}

	var kept bool
	for _, a := range all {
		if a.ID == mine.ID {
			kept = true
		}
	}
	if !kept {
		t.Error("the mark made in the reader was lost to an import")
	}
}

// An import is a way to write rows, so it has to check the book is one this
// person can open.
func TestImportingIntoSomebodyElsesBookIsRefused(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatPDF)
	stranger := secondUser(t, p)
	ctx := t.Context()

	if _, err := p.ImportMarks(ctx, stranger, bookFileID, []ImportedMark{{Page: 1, Text: "x"}}); !isNotFound(err) {
		t.Errorf("a stranger imported marks into a book they cannot read: %v", err)
	}
	if _, err := p.ImportMarks(ctx, userID, bookFileID, []ImportedMark{{Page: 1, Text: "x"}}); err != nil {
		t.Errorf("the owner could not import: %v", err)
	}
}

// An import's rows are stamped with clock_timestamp() as the last statement
// before commit, not at each row's own INSERT - clock_timestamp() there would
// still be wrong for every row but the last, since none of them are visible
// to another transaction until this one commits regardless of what value they
// carry. This checks the end result: every imported row's updated_at reads
// close to now, whether the import held five marks or one.
func TestImportedMarksAreStampedAtCommitTime(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatPDF)
	ctx := t.Context()

	if _, err := p.ImportMarks(ctx, userID, bookFileID, []ImportedMark{
		{Page: 1, Text: "(1.1)"},
		{Page: 2, Text: "(1.2)"},
		{Page: 3, Text: "(1.3)"},
	}); err != nil {
		t.Fatalf("ImportMarks: %v", err)
	}

	marks, err := p.Annotations(ctx, userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 3 {
		t.Fatalf("stored %d marks, want 3", len(marks))
	}
	for _, m := range marks {
		if time.Since(m.UpdatedAt) > time.Minute {
			t.Errorf("mark %s updated_at = %s, want roughly now", m.ID, m.UpdatedAt)
		}
	}
}

// The tombstones an import writes for the marks it replaces are scoped by id,
// not by "this book, this user" - which would also match a tombstone the
// reader left by deleting their OWN note, entirely unrelated to any import,
// and re-stamping it would misreport when that deletion actually happened.
func TestImportMarksDoesNotRestampAnUnrelatedTombstone(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatPDF)
	ctx := t.Context()

	note, err := p.CreateAnnotation(ctx, domain.Annotation{
		UserID:     userID,
		BookFileID: bookFileID,
		Type:       domain.AnnotationNote,
		Range: domain.Range{
			Start: domain.NewPDFLocator(1, 0),
			End:   domain.NewPDFLocator(1, 10),
		},
		Note: "a note, not from any import",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	if err := p.DeleteAnnotation(ctx, userID, note.ID); err != nil {
		t.Fatalf("DeleteAnnotation: %v", err)
	}

	var before time.Time
	if err := p.QueryRow(ctx,
		`SELECT deleted_at FROM annotation_tombstones WHERE annotation_id = $1`, note.ID,
	).Scan(&before); err != nil {
		t.Fatalf("reading the tombstone: %v", err)
	}

	// An import into the same book, nothing to do with that note.
	if _, err := p.ImportMarks(ctx, userID, bookFileID, []ImportedMark{{Page: 1, Text: "(1.1)"}}); err != nil {
		t.Fatalf("ImportMarks: %v", err)
	}

	var after time.Time
	if err := p.QueryRow(ctx,
		`SELECT deleted_at FROM annotation_tombstones WHERE annotation_id = $1`, note.ID,
	).Scan(&after); err != nil {
		t.Fatalf("reading the tombstone: %v", err)
	}
	if !after.Equal(before) {
		t.Errorf("an unrelated tombstone's deleted_at changed from %s to %s during an import that never touched it",
			before, after)
	}
}
