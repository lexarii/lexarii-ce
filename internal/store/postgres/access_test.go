// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// secondUser makes an account that owns nothing.
func secondUser(t *testing.T, p *Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := p.QueryRow(t.Context(), `
		INSERT INTO users (username, is_admin) VALUES ($1, false) RETURNING id`,
		"stranger-"+uuid.NewString()[:8]).Scan(&id)
	if err != nil {
		t.Fatalf("creating a second user: %v", err)
	}
	return id
}

// Books belong to people. Everything that reads them has to agree about that,
// and the failure being tested is not a tidy 403 - it is someone else's library
// appearing in a listing, a count, or a cited passage.
func TestBooksAreInvisibleToOtherPeople(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := seedLibrary(t, p, sampleBooks)
	stranger := secondUser(t, p)

	t.Run("the owner sees their library", func(t *testing.T) {
		page, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner})
		if err != nil {
			t.Fatalf("Library: %v", err)
		}
		if len(page.Items) != len(sampleBooks) {
			t.Fatalf("owner sees %d books, want %d", len(page.Items), len(sampleBooks))
		}
	})

	t.Run("a stranger sees nothing", func(t *testing.T) {
		page, err := p.Library(t.Context(), LibraryQuery{ViewerID: stranger})
		if err != nil {
			t.Fatalf("Library: %v", err)
		}
		if len(page.Items) != 0 {
			t.Fatalf("stranger sees %d books: %v", len(page.Items), titlesOf(page))
		}
	})

	t.Run("counts do not leak either", func(t *testing.T) {
		// "168 books" on an account granted two says plainly that there are 166
		// more, which is the same leak in a smaller package.
		s, err := p.Stats(t.Context(), stranger)
		if err != nil {
			t.Fatalf("Stats: %v", err)
		}
		if s.Works != 0 || s.Files != 0 || s.Authors != 0 {
			t.Fatalf("stranger's stats describe someone else's shelf: %+v", s)
		}
	})

	t.Run("a search without a viewer is refused, not unfiltered", func(t *testing.T) {
		_, err := p.Search(t.Context(), SearchQuery{Text: "algorithms"})
		if err == nil {
			t.Fatal("a search with no viewer was allowed; it would have searched everything")
		}
	})

	t.Run("a listing without a viewer is refused", func(t *testing.T) {
		if _, err := p.Library(t.Context(), LibraryQuery{}); err == nil {
			t.Fatal("a listing with no viewer was allowed")
		}
	})
}

// A grant makes exactly one book visible, and says nothing about the rest.
func TestGrantingOneBook(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := seedLibrary(t, p, sampleBooks)
	friend := secondUser(t, p)

	var fileID uuid.UUID
	var title string
	err := p.QueryRow(t.Context(), `
		SELECT bf.id, w.title FROM book_files bf
		JOIN editions e ON e.id = bf.edition_id
		JOIN works w ON w.id = e.work_id
		ORDER BY w.title LIMIT 1`).Scan(&fileID, &title)
	if err != nil {
		t.Fatalf("finding a book: %v", err)
	}

	if _, err := p.Exec(t.Context(), `
		INSERT INTO book_access (book_file_id, user_id, granted_by, can_download)
		VALUES ($1, $2, $3, false)`, fileID, friend, owner); err != nil {
		t.Fatalf("granting: %v", err)
	}

	page, err := p.Library(t.Context(), LibraryQuery{ViewerID: friend})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("friend sees %d books, want exactly the one granted: %v",
			len(page.Items), titlesOf(page))
	}
	if page.Items[0].Title != title {
		t.Fatalf("friend sees %q, want %q", page.Items[0].Title, title)
	}

	// Reading is not downloading. The grant said so explicitly.
	allowed, err := p.CanDownload(t.Context(), fileID, friend)
	if err != nil {
		t.Fatalf("CanDownload: %v", err)
	}
	if allowed {
		t.Fatal("a read-only grant allowed downloading")
	}

	// The owner may always take their own file.
	allowed, err = p.CanDownload(t.Context(), fileID, owner)
	if err != nil {
		t.Fatalf("CanDownload: %v", err)
	}
	if !allowed {
		t.Fatal("the owner was refused their own book")
	}

	// And the file itself opens for the grantee but not for a stranger.
	if _, err := p.ReadableBookFile(t.Context(), fileID, friend); err != nil {
		t.Fatalf("grantee cannot open the book they were given: %v", err)
	}
	stranger := secondUser(t, p)
	if _, err := p.ReadableBookFile(t.Context(), fileID, stranger); !isNotFound(err) {
		t.Fatalf("a stranger opened the file: err = %v", err)
	}

	// Asking whether a stranger may download it is "no", not an error: with
	// no readable row the aggregate is NULL, which once failed the scan.
	allowed, err = p.CanDownload(t.Context(), fileID, stranger)
	if err != nil || allowed {
		t.Fatalf("CanDownload(stranger) = %v, %v; want false, nil", allowed, err)
	}
}

// A work is visible when at least one of its files is, so two people can hold
// different formats of the same book and each see it through their own copy.
func TestWorkVisibilityFollowsItsFiles(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)
	other := secondUser(t, p)

	mine := sampleInput(fmt.Sprintf("%064x", 1), "/library/shared.epub", "Shared Title", "An Author")
	mine.OwnerID = owner
	out, err := p.UpsertBookFile(ctx, mine)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	if _, err := p.Work(ctx, out.WorkID, owner); err != nil {
		t.Fatalf("owner cannot see their own work: %v", err)
	}
	if _, err := p.Work(ctx, out.WorkID, other); !isNotFound(err) {
		// Not found rather than forbidden: someone who may not see a book
		// should not learn that it exists.
		t.Fatalf("a stranger read the work detail: err = %v", err)
	}
}

// A grant must not outlive the account that made it.
//
// Deleting an owner archives their books and ends their holdings. The view's
// second branch once never looked at book_files, so a grant made before that
// survived with nobody able to revoke it - UnshareBook needs an owner to act
// as - and the borrower kept reading and downloading a book the library says
// belongs to no one.
func TestAGrantDiesWithTheOwnersAccount(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	owner := testOwner(t, p)
	borrower := secondUser(t, p)

	in := sampleInput(hashOf("a"), "/library/lent.epub", "A Lent Book", "An Author")
	in.OwnerID = owner
	book, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if err := p.ShareBook(ctx, book.BookFileID, owner, borrower, true, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	var canRead bool
	if err := p.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM readable_book_files
		                WHERE book_file_id = $1 AND user_id = $2)`,
		book.BookFileID, borrower).Scan(&canRead); err != nil {
		t.Fatalf("checking access: %v", err)
	}
	if !canRead {
		t.Fatal("the borrower cannot read a book that was lent to them")
	}

	if _, err := p.DeleteAccount(ctx, owner, owner); err != nil {
		// Deleting the last administrator is refused; make somebody else one.
		if _, uerr := p.Exec(ctx, `UPDATE users SET is_admin = true WHERE id = $1`, borrower); uerr != nil {
			t.Fatalf("promoting: %v", uerr)
		}
		if _, err := p.DeleteAccount(ctx, owner, owner); err != nil {
			t.Fatalf("DeleteAccount: %v", err)
		}
	}

	if err := p.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM readable_book_files
		                WHERE book_file_id = $1 AND user_id = $2)`,
		book.BookFileID, borrower).Scan(&canRead); err != nil {
		t.Fatalf("checking access: %v", err)
	}
	if canRead {
		t.Error("the borrower still reads a book whose owner deleted their account, " +
			"and nobody is left who could revoke it")
	}
}

// One row per reader per file, whatever route they came by.
//
// The view was a UNION of two branches that disagreed about can_download, so
// somebody holding both an ownership and a grant appeared twice - and fifteen
// queries join this as though it were unique, so every one of them doubled.
func TestTheAccessViewNeverReturnsSomebodyTwice(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	owner := testOwner(t, p)
	other := secondUser(t, p)

	in := sampleInput(hashOf("b"), "/library/claimed.epub", "A Claimed Book", "An Author")
	in.OwnerID = owner
	book, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if err := p.ShareBook(ctx, book.BookFileID, owner, other, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	// The borrower comes to hold the book as well - they upload the same
	// file - without their grant being cleared. Holding and grant are now two
	// routes to one reader, which is exactly the shape that doubled.
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: in.ContentHash, UserID: other}); err != nil {
		t.Fatalf("holding the book too: %v", err)
	}

	var rows int
	var canDownload bool
	if err := p.QueryRow(ctx, `
		SELECT count(*), bool_or(can_download) FROM readable_book_files
		 WHERE book_file_id = $1 AND user_id = $2`,
		book.BookFileID, other).Scan(&rows, &canDownload); err != nil {
		t.Fatalf("reading the view: %v", err)
	}
	if rows != 1 {
		t.Errorf("the view returns %d rows for one reader and one file; every join doubles", rows)
	}
	if !canDownload {
		t.Error("owning the book should allow downloading it, whatever an older grant said")
	}
}
