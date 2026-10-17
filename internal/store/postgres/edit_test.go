// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"testing"

	"github.com/google/uuid"
)

func str(s string) *string       { return &s }
func intp(n int) *int            { return &n }
func strs(v ...string) *[]string { return &v }

// editFixture puts one badly-described book in a library, the way they
// actually arrive: a title taken off the wrong line, and no author at all.
func editFixture(t *testing.T, p *Pool) (userID, workID uuid.UUID, hash string) {
	t.Helper()
	cleanLibrary(t, p)

	userID, err := p.EnsureDefaultUser(t.Context())
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}

	hash = hashOf("e")
	in := sampleInput(hash, "/library/bad.epub", "Second Edition", "")
	in.OwnerID = userID
	in.Result.Metadata.Authors = nil
	// No provenance: nothing has declared anything about this book, which is
	// the state the badly-described ones in a real library are actually in.
	in.Result.Metadata.Provenance = nil
	book, err := p.UpsertBookFile(t.Context(), in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	return userID, book.WorkID, hash
}

// The correction itself.
func TestABookCanBeCorrectedByHand(t *testing.T) {
	p := testPool(t)
	userID, workID, _ := editFixture(t, p)

	detail, err := p.UpdateWork(t.Context(), workID, userID, WorkEdit{
		Title:     str("Structure and Interpretation of Computer Programs"),
		Authors:   strs("Harold Abelson", "Gerald Jay Sussman"),
		Publisher: str("MIT Press"),
		Year:      intp(1985),
	})
	if err != nil {
		t.Fatalf("UpdateWork: %v", err)
	}

	if detail.Work.Title != "Structure and Interpretation of Computer Programs" {
		t.Errorf("title = %q", detail.Work.Title)
	}
	if len(detail.Work.Authors) != 2 {
		t.Fatalf("authors = %+v, want two", detail.Work.Authors)
	}
	if detail.Work.Authors[0].Author.Name != "Harold Abelson" {
		t.Errorf("first author = %q, want the order it was given in",
			detail.Work.Authors[0].Author.Name)
	}
	if len(detail.Work.Editions) == 0 || detail.Work.Editions[0].Publisher != "MIT Press" {
		t.Errorf("editions = %+v, want the publisher saved", detail.Work.Editions)
	}
}

// The whole reason this exists. Fixing a title and finding it reverted by the
// next scan is what people give up over.
func TestACorrectionSurvivesARescan(t *testing.T) {
	p := testPool(t)
	userID, workID, hash := editFixture(t, p)

	if _, err := p.UpdateWork(t.Context(), workID, userID, WorkEdit{
		Title:   str("A Real Title"),
		Authors: strs("A Real Author"),
	}); err != nil {
		t.Fatalf("UpdateWork: %v", err)
	}

	// The scanner finds the same file again and offers the same bad metadata.
	in := sampleInput(hash, "/library/bad.epub", "Second Edition", "Www.Ebook-Dl.Com")
	in.OwnerID = userID
	if _, err := p.UpsertBookFile(t.Context(), in); err != nil {
		t.Fatalf("re-ingesting: %v", err)
	}

	detail, err := p.Work(t.Context(), workID, userID)
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if detail.Work.Title != "A Real Title" {
		t.Errorf("title = %q, want the correction to have survived", detail.Work.Title)
	}
	for _, a := range detail.Work.Authors {
		if a.Author.Name == "Www.Ebook-Dl.Com" {
			t.Errorf("the scan put %q back after it was removed", a.Author.Name)
		}
	}
}

// Clearing is a real instruction, and the only way to remove what a scanner
// invented.
func TestAnEmptyFieldClearsAndAMissingOneDoesNot(t *testing.T) {
	p := testPool(t)
	userID, workID, _ := editFixture(t, p)

	if _, err := p.UpdateWork(t.Context(), workID, userID, WorkEdit{
		Title:    str("Kept"),
		Subtitle: str("An Invented Subtitle"),
	}); err != nil {
		t.Fatalf("UpdateWork: %v", err)
	}

	// Mentioning only the subtitle, and mentioning it as empty.
	detail, err := p.UpdateWork(t.Context(), workID, userID, WorkEdit{Subtitle: str("")})
	if err != nil {
		t.Fatalf("UpdateWork: %v", err)
	}
	if detail.Work.Subtitle != "" {
		t.Errorf("subtitle = %q, want it cleared", detail.Work.Subtitle)
	}
	if detail.Work.Title != "Kept" {
		t.Errorf("title = %q, want the field nobody mentioned left alone", detail.Work.Title)
	}
}

// An edit is recorded as the user's, which is the mechanism the rescan test
// depends on. Worth asserting directly so a change to the ledger fails here
// rather than silently somewhere else.
func TestAnEditIsRecordedAsTheUsers(t *testing.T) {
	p := testPool(t)
	userID, workID, _ := editFixture(t, p)

	if _, err := p.UpdateWork(t.Context(), workID, userID, WorkEdit{Title: str("Mine")}); err != nil {
		t.Fatalf("UpdateWork: %v", err)
	}

	var source string
	if err := p.QueryRow(t.Context(), `
		SELECT source FROM enrichment_records
		WHERE work_id = $1 AND field = 'title'
		ORDER BY accepted_at DESC LIMIT 1`, workID).Scan(&source); err != nil {
		t.Fatalf("reading the ledger: %v", err)
	}
	if source != "user" {
		t.Errorf("source = %q, want \"user\"", source)
	}
}

// Somebody the book was shared with may read it and mark it up. Renaming it
// for everybody is a different act.
func TestSomebodyElsesBookCannotBeRenamed(t *testing.T) {
	p := testPool(t)
	_, workID, _ := editFixture(t, p)

	if _, err := p.UpdateWork(t.Context(), workID, uuid.New(), WorkEdit{Title: str("Mine now")}); err == nil {
		t.Fatal("a stranger renamed somebody else's book")
	}
}

// A book with no title is not a book anybody can find again.
func TestATitleCannotBeRemoved(t *testing.T) {
	p := testPool(t)
	userID, workID, _ := editFixture(t, p)

	if _, err := p.UpdateWork(t.Context(), workID, userID, WorkEdit{Title: str("   ")}); err == nil {
		t.Fatal("the title was allowed to be blanked")
	}
}
