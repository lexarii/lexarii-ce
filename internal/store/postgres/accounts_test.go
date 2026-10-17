// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// A borrower's account going needs no special handling: their access ends and
// the book is the owner's again, which the schema already did by cascade. The
// test exists because that is the half of the policy nobody would think to
// check, and a change to the foreign keys could silently take it away.
func TestDeletingABorrowerLeavesTheOwnerWhole(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	borrower := secondUser(t, p)
	ctx := t.Context()

	book := oneBook(t, p, owner)
	if err := p.ShareBook(ctx, book, owner, borrower, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	archived, err := p.DeleteAccount(ctx, owner, borrower)
	if err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if len(archived) != 0 {
		t.Fatalf("a borrower's deletion archived %d books", len(archived))
	}

	// The owner still has it, unarchived and readable.
	page, err := p.Library(ctx, LibraryQuery{ViewerID: owner})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("the owner has %d books, want 1", len(page.Items))
	}
	if _, err := p.ReadableBookFile(ctx, book, owner); err != nil {
		t.Fatalf("the owner cannot read their own book: %v", err)
	}
}

// The owner's own side: the books are archived against their name rather than
// orphaned, and are visible to nobody until somebody claims them.
func TestDeletingAnOwnerArchivesTheirBooks(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	admin := testOwner(t, p)
	leaver := secondUser(t, p)
	ctx := t.Context()

	in := owned(sampleInput(fmt.Sprintf("%064x", 61), "/library/theleaversbook.epub",
		"A Book Somebody Left", "An Author"), leaver)
	out, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	archived, err := p.DeleteAccount(ctx, admin, leaver)
	if err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if len(archived) != 1 || archived[0].BookFileID != out.BookFileID {
		t.Fatalf("archived = %+v", archived)
	}
	if archived[0].ArchivedOwner == "" {
		t.Error("the archive does not remember who owned it")
	}
	if archived[0].Path == "" {
		t.Error("the archive does not say where the file is")
	}

	// Nobody sees it, including the administrator, because it is not theirs.
	page, err := p.Library(ctx, LibraryQuery{ViewerID: admin})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("an archived book is on somebody's shelf: %+v", page.Items)
	}

	// It is waiting, and an administrator can see what is waiting.
	waiting, err := p.ArchivedBooks(ctx, admin)
	if err != nil {
		t.Fatalf("ArchivedBooks: %v", err)
	}
	if len(waiting) != 1 || waiting[0].Title != "A Book Somebody Left" {
		t.Fatalf("waiting = %+v", waiting)
	}

	// Claimed, it becomes an ordinary book again.
	if err := p.ClaimArchived(ctx, admin, out.BookFileID, admin); err != nil {
		t.Fatalf("ClaimArchived: %v", err)
	}
	page, err = p.Library(ctx, LibraryQuery{ViewerID: admin})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("a claimed book did not arrive: %+v", page.Items)
	}
	waiting, _ = p.ArchivedBooks(ctx, admin)
	if len(waiting) != 0 {
		t.Fatalf("a claimed book is still listed as archived: %+v", waiting)
	}
}

// Deleting an account deletes the person. The books outlive it; the notes,
// positions and conversations do not, and that is not deferred by six months.
func TestDeletingAnAccountTakesEverythingPersonalWithIt(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	admin := testOwner(t, p)
	leaver := secondUser(t, p)
	ctx := t.Context()

	book := oneBook(t, p, admin)
	if err := p.ShareBook(ctx, book, admin, leaver, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	if _, err := p.CreateAnnotation(ctx, epubMark(leaver, book, 0, "p[1]", 0, 9)); err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	percent := float32(0.4)
	if _, err := p.SaveProgress(ctx, domain.ReadingProgress{
		UserID:     leaver,
		BookFileID: book,
		Position:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 0, ElementPath: "p[1]"},
		Percent:    &percent,
	}); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}

	if _, err := p.DeleteAccount(ctx, admin, leaver); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	var marks, progress, access int
	if err := p.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM annotations WHERE user_id = $1),
		       (SELECT count(*) FROM reading_progress WHERE user_id = $1),
		       (SELECT count(*) FROM book_access WHERE user_id = $1)`,
		leaver).Scan(&marks, &progress, &access); err != nil {
		t.Fatalf("counting what is left: %v", err)
	}
	if marks != 0 || progress != 0 || access != 0 {
		t.Errorf("personal data survived the account: marks=%d progress=%d access=%d",
			marks, progress, access)
	}
}

// The record of what a deleted account READ is kept deliberately - it is the
// owner's audit trail, not the reader's data - and it stops naming them.
func TestTheAuditOutlivesTheAccountWithoutNamingIt(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	reader := secondUser(t, p)
	ctx := t.Context()

	book := oneBook(t, p, owner)
	if err := p.ShareBook(ctx, book, owner, reader, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	if err := p.RecordAccess(ctx, book, reader, AccessChapter, 1); err != nil {
		t.Fatalf("RecordAccess: %v", err)
	}

	if _, err := p.DeleteAccount(ctx, owner, reader); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	log, err := p.BookAccessLog(ctx, book, owner)
	if err != nil {
		t.Fatalf("BookAccessLog: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("the audit lost its row: %+v", log)
	}
	if log[0].UserID != uuid.Nil {
		t.Errorf("the audit still points at the deleted account: %v", log[0].UserID)
	}
	if log[0].Username == "" {
		t.Error("the audit row has nothing to show in place of a name")
	}
}

// After the retention period the ROW goes and the file does not.
func TestArchivedBooksExpireIntoJustFiles(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	admin := testOwner(t, p)
	leaver := secondUser(t, p)
	ctx := t.Context()

	in := owned(sampleInput(fmt.Sprintf("%064x", 62), "/library/longforgotten.epub",
		"Long Forgotten", "An Author"), leaver)
	out, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.DeleteAccount(ctx, admin, leaver); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	// Nothing expires before its time.
	gone, err := p.ExpireArchived(ctx, time.Now().Add(-domain.DefaultArchiveRetention), false)
	if err != nil {
		t.Fatalf("ExpireArchived: %v", err)
	}
	if len(gone) != 0 {
		t.Fatalf("a book archived today expired: %v", gone)
	}

	// Backdate it past the window.
	if _, err := p.Exec(ctx, `
		UPDATE book_files SET archived_at = now() - interval '200 days' WHERE id = $1`,
		out.BookFileID); err != nil {
		t.Fatalf("backdating: %v", err)
	}

	gone, err = p.ExpireArchived(ctx, time.Now().Add(-domain.DefaultArchiveRetention), false)
	if err != nil {
		t.Fatalf("ExpireArchived: %v", err)
	}
	if len(gone) != 1 || gone[0] != "/library/longforgotten.epub" {
		t.Fatalf("expired = %v, want the file's path so somebody can find it", gone)
	}

	var rows int
	if err := p.QueryRow(ctx,
		`SELECT count(*) FROM book_files WHERE id = $1`, out.BookFileID).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 0 {
		t.Error("the expired row is still in the database")
	}
}

func TestOnlyAnAdministratorMayDeleteSomebodyElse(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	_ = testOwner(t, p)
	one := secondUser(t, p)
	two := secondUser(t, p)
	ctx := t.Context()

	if _, err := p.DeleteAccount(ctx, one, two); !errors.Is(err, store.ErrForbidden) {
		t.Fatalf("a reader deleted another account: %v", err)
	}

	// Deleting yourself needs nobody's permission.
	if _, err := p.DeleteAccount(ctx, one, one); err != nil {
		t.Fatalf("somebody could not delete their own account: %v", err)
	}
}

// An installation with no administrator is one where nothing can be added,
// scanned, or handed on - including the archived books above.
func TestTheLastAdministratorCannotBeDeleted(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	admin := testOwner(t, p)
	ctx := t.Context()

	// Make this the last administrator, rather than assuming it.
	//
	// cleanLibrary clears books, not people, and the test database is shared
	// and never truncated - so an administrator seeded by some other test in
	// some earlier run is still there, and deleting this one is then perfectly
	// legal. The test passed for as long as nothing else had happened to
	// create one, which is not the same as passing.
	if _, err := p.Exec(ctx,
		`UPDATE users SET is_admin = false WHERE id <> $1`, admin); err != nil {
		t.Fatalf("clearing other administrators: %v", err)
	}

	if _, err := p.DeleteAccount(ctx, admin, admin); !errors.Is(err, store.ErrInvalidQuery) {
		t.Fatalf("the only administrator deleted themselves: %v", err)
	}
}
