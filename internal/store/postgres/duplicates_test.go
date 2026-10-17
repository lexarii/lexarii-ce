// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// twoCopies puts the same book in a library twice, the way it actually
// happens: two files whose NAMES differ, so ingest never matched them.
func twoCopies(t *testing.T, p *Pool, title, author string) (userID, first, second uuid.UUID) {
	t.Helper()
	cleanLibrary(t, p)

	userID, err := p.EnsureDefaultUser(t.Context())
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}

	one := sampleInput(hashOf("a"), "/library/a-book.epub", title, author)
	one.OwnerID = userID
	a, err := p.UpsertBookFile(t.Context(), one)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	two := sampleInput(hashOf("b"), "/library/quite-another-name.pdf", title, author)
	two.OwnerID = userID
	two.Format = domain.FormatPDF
	two.Result.Format = domain.FormatPDF
	b, err := p.UpsertBookFile(t.Context(), two)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	if a.WorkID == b.WorkID {
		t.Fatalf("the two files landed on one work; this fixture is not testing anything")
	}
	return userID, a.WorkID, b.WorkID
}

func TestOneBookUnderTwoFilenamesIsFound(t *testing.T) {
	p := testPool(t)
	userID, first, second := twoCopies(t, p, "The Go Programming Language", "Alan Donovan")

	groups, err := p.Duplicates(t.Context(), userID)
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %d, want 1: %+v", len(groups), groups)
	}
	if len(groups[0].Works) != 2 {
		t.Fatalf("works = %+v, want both copies", groups[0].Works)
	}

	found := map[uuid.UUID]bool{}
	for _, w := range groups[0].Works {
		found[w.WorkID] = true
	}
	if !found[first] || !found[second] {
		t.Errorf("group holds %+v, want %s and %s", groups[0].Works, first, second)
	}
	if groups[0].Reason != ReasonTitleAuthor {
		t.Errorf("reason = %q, want %q", groups[0].Reason, ReasonTitleAuthor)
	}
}

// Two books that share a title and nothing else are two books. "Refactoring"
// is a title several people have used.
func TestTwoBooksWithOneTitleAreNotDuplicates(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)

	userID, err := p.EnsureDefaultUser(t.Context())
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}
	for seed, author := range map[string]string{"a": "Martin Fowler", "b": "Somebody Else"} {
		in := sampleInput(hashOf(seed), "/library/refactoring-"+seed+".epub", "Refactoring", author)
		in.OwnerID = userID
		if _, err := p.UpsertBookFile(t.Context(), in); err != nil {
			t.Fatalf("UpsertBookFile: %v", err)
		}
	}

	groups, err := p.Duplicates(t.Context(), userID)
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("two different books by different authors were called duplicates: %+v", groups)
	}
}

// The whole risk of merging: somebody's highlights. They key on the FILE, so
// moving a file carries them - but that has to be true and not assumed.
func TestMergingKeepsEveryHighlight(t *testing.T) {
	p := testPool(t)
	userID, keep, fold := twoCopies(t, p, "The Go Programming Language", "Alan Donovan")

	// A mark on the copy that is about to disappear.
	var doomedFile uuid.UUID
	if err := p.QueryRow(t.Context(), `
		SELECT bf.id FROM book_files bf JOIN editions e ON e.id = bf.edition_id
		WHERE e.work_id = $1`, fold).Scan(&doomedFile); err != nil {
		t.Fatalf("finding the file: %v", err)
	}
	mark, err := p.CreateAnnotation(t.Context(), domain.Annotation{
		UserID:     userID,
		BookFileID: doomedFile,
		Type:       domain.AnnotationHighlight,
		Range: domain.Range{
			Start: domain.Locator{Format: domain.FormatPDF, Page: 4},
			End:   domain.Locator{Format: domain.FormatPDF, Page: 4, CharOffset: 40},
		},
		SelectedText: "worth keeping",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	moved, err := p.MergeWorks(t.Context(), userID, keep, []uuid.UUID{fold})
	if err != nil {
		t.Fatalf("MergeWorks: %v", err)
	}
	if moved != 1 {
		t.Errorf("moved %d editions, want 1", moved)
	}

	// The mark is still there, and still on its file.
	marks, err := p.Annotations(t.Context(), userID, doomedFile)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 || marks[0].ID != mark.ID {
		t.Fatalf("the highlight did not survive the merge: %+v", marks)
	}

	// And the kept book now holds both files.
	detail, err := p.Work(t.Context(), keep, userID)
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(detail.Files) != 2 {
		t.Errorf("the kept book has %d files, want both copies", len(detail.Files))
	}
	if _, err := p.Work(t.Context(), fold, userID); err == nil {
		t.Error("the merged-away book is still there")
	}
}

// The common case for merging is one copy described and one bare, so the
// description has to survive from whichever side has it.
func TestMergingTakesTheAuthorsTheKeptCopyLacks(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)

	userID, err := p.EnsureDefaultUser(t.Context())
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}

	bare := sampleInput(hashOf("a"), "/library/bare.epub", "Some Real Book", "")
	bare.OwnerID = userID
	bare.Result.Metadata.Authors = nil
	keptWork, err := p.UpsertBookFile(t.Context(), bare)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	described := sampleInput(hashOf("b"), "/library/described.pdf", "Some Real Book", "A Real Author")
	described.OwnerID = userID
	described.Format = domain.FormatPDF
	described.Result.Format = domain.FormatPDF
	other, err := p.UpsertBookFile(t.Context(), described)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	if _, err := p.MergeWorks(t.Context(), userID, keptWork.WorkID, []uuid.UUID{other.WorkID}); err != nil {
		t.Fatalf("MergeWorks: %v", err)
	}

	detail, err := p.Work(t.Context(), keptWork.WorkID, userID)
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(detail.Work.Authors) != 1 || detail.Work.Authors[0].Author.Name != "A Real Author" {
		t.Errorf("authors = %+v, want the one the other copy had", detail.Work.Authors)
	}
}

// Merging is destructive, and a borrowed book is not somebody's to fold into
// anything.
func TestSomebodyElsesBooksCannotBeMerged(t *testing.T) {
	p := testPool(t)
	_, keep, fold := twoCopies(t, p, "The Go Programming Language", "Alan Donovan")

	if _, err := p.MergeWorks(t.Context(), uuid.New(), keep, []uuid.UUID{fold}); err == nil {
		t.Fatal("a stranger merged two books they do not own")
	}
}

func TestABookCannotBeMergedIntoItself(t *testing.T) {
	p := testPool(t)
	userID, keep, _ := twoCopies(t, p, "The Go Programming Language", "Alan Donovan")

	if _, err := p.MergeWorks(t.Context(), userID, keep, []uuid.UUID{keep}); err == nil {
		t.Fatal("a book was merged into itself")
	}
}
