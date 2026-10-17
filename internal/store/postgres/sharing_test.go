// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

func oneBook(t *testing.T, p *Pool, owner uuid.UUID) uuid.UUID {
	t.Helper()
	in := owned(sampleInput(fmt.Sprintf("%064x", 1), "/library/shared.epub",
		"A Shared Book", "An Author"), owner)
	out, err := p.UpsertBookFile(t.Context(), in)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	return out.BookFileID
}

func TestSharingABook(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	book := oneBook(t, p, owner)
	ctx := t.Context()

	if err := p.ShareBook(ctx, book, owner, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	shares, err := p.BookShares(ctx, book, owner)
	if err != nil {
		t.Fatalf("BookShares: %v", err)
	}
	if len(shares) != 1 || shares[0].UserID != friend {
		t.Fatalf("shares = %+v", shares)
	}
	if shares[0].CanDownload || shares[0].Annotations {
		t.Fatalf("a bare share granted more than reading: %+v", shares[0])
	}

	// Sharing again is a change, not a failure: the UI toggles it.
	if err := p.ShareBook(ctx, book, owner, friend, true, true); err != nil {
		t.Fatalf("re-sharing: %v", err)
	}
	shares, _ = p.BookShares(ctx, book, owner)
	if !shares[0].CanDownload || !shares[0].Annotations {
		t.Fatalf("re-sharing did not update the permissions: %+v", shares[0])
	}

	if err := p.UnshareBook(ctx, book, owner, friend); err != nil {
		t.Fatalf("UnshareBook: %v", err)
	}
	if _, err := p.ReadableBookFile(ctx, book, friend); !isNotFound(err) {
		t.Fatalf("access survived being revoked: %v", err)
	}
}

// A username names one account however it is capitalised, as it does at
// sign-in, and a name nobody has is not found.
func TestPersonNamed(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	friend := secondUser(t, p)
	var name string
	if err := p.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, friend).Scan(&name); err != nil {
		t.Fatalf("reading the name: %v", err)
	}

	for _, asked := range []string{name, strings.ToUpper(name), "  " + name + " "} {
		got, err := p.PersonNamed(ctx, asked)
		if err != nil || got.ID != friend || got.Username != name {
			t.Errorf("PersonNamed(%q) = %+v, %v; want %s as %q", asked, got, err, friend, name)
		}
	}
	if _, err := p.PersonNamed(ctx, "nobody-"+uuid.NewString()); !isNotFound(err) {
		t.Errorf("an unknown name: %v, want not found", err)
	}
	if _, err := p.PersonNamed(ctx, ""); !isNotFound(err) {
		t.Errorf("no name: %v, want not found", err)
	}
}

// MayLend and MayShareNotes answer exactly what ShareBook and
// ShareAnnotations would: a holder may lend, a borrower may only share their
// notes, a stranger neither, and an archived file nobody.
func TestMayLendAndMayShareNotes(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner, borrower, stranger := testOwner(t, p), secondUser(t, p), secondUser(t, p)
	book := oneBook(t, p, owner)
	if err := p.ShareBook(ctx, book, owner, borrower, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	for _, tc := range []struct {
		name          string
		who           uuid.UUID
		book          uuid.UUID
		lend, comment bool
	}{
		{"the holder", owner, book, true, true},
		{"a borrower", borrower, book, false, true},
		{"a stranger", stranger, book, false, false},
		{"no such book", owner, uuid.New(), false, false},
	} {
		if err := p.MayLend(ctx, tc.book, tc.who); (err == nil) != tc.lend || (err != nil && !isNotFound(err)) {
			t.Errorf("%s: MayLend = %v, want allowed %v (else not found)", tc.name, err, tc.lend)
		}
		if err := p.MayShareNotes(ctx, tc.book, tc.who); (err == nil) != tc.comment || (err != nil && !isNotFound(err)) {
			t.Errorf("%s: MayShareNotes = %v, want allowed %v (else not found)", tc.name, err, tc.comment)
		}
	}

	if _, err := p.Exec(ctx, `UPDATE book_files SET archived_at = now() WHERE id = $1`, book); err != nil {
		t.Fatalf("archiving: %v", err)
	}
	if err := p.MayLend(ctx, book, owner); !isNotFound(err) {
		t.Errorf("an archived book: MayLend = %v, want not found, as ShareBook refuses it", err)
	}
}

// Only the owner may share, and the check is in the statement that writes -
// not in a handler that a second caller could skip.
func TestOnlyTheOwnerMayShare(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	stranger := secondUser(t, p)
	book := oneBook(t, p, owner)
	ctx := t.Context()

	// A stranger cannot give away someone else's book...
	if err := p.ShareBook(ctx, book, stranger, friend, false, false); !isNotFound(err) {
		t.Fatalf("a stranger shared a book they do not own: err = %v", err)
	}
	// ...and not even after being given it themselves.
	if err := p.ShareBook(ctx, book, owner, stranger, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	if err := p.ShareBook(ctx, book, stranger, friend, false, false); !isNotFound(err) {
		t.Fatalf("a grantee re-shared a book that is not theirs: err = %v", err)
	}
	if _, err := p.BookShares(ctx, book, stranger); !isNotFound(err) {
		t.Fatalf("a grantee read the owner's share list: err = %v", err)
	}
}

func TestSharingWithYourselfIsRefused(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	book := oneBook(t, p, owner)

	if err := p.ShareBook(t.Context(), book, owner, owner, false, false); err == nil {
		t.Fatal("a book was shared with its own owner")
	}
}

// Annotations follow the book: shared when the owner says so, and withdrawn
// the moment the book is. Marginalia visible for a book someone can no longer
// open would outlive the decision to stop sharing.
func TestSharedAnnotationsAreVisibleAndAttributed(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	book := oneBook(t, p, owner)
	ctx := t.Context()

	mark := epubMark(owner, book, 3, "/body/p[2]", 0, 20)
	if _, err := p.CreateAnnotation(ctx, mark); err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	// Shared the book, but not the marks.
	if err := p.ShareBook(ctx, book, owner, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	seen, err := p.Annotations(ctx, friend, book)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("marginalia leaked with a book-only share: %+v", seen)
	}

	// Now the marks too.
	if err := p.ShareBook(ctx, book, owner, friend, false, true); err != nil {
		t.Fatalf("sharing annotations: %v", err)
	}
	seen, err = p.Annotations(ctx, friend, book)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("shared annotation not visible: %+v", seen)
	}
	if !seen[0].Shared || seen[0].Author == "" {
		t.Fatalf("a shared mark must say whose it is: %+v", seen[0])
	}

	// The owner's own view is unchanged and unattributed.
	mine, _ := p.Annotations(ctx, owner, book)
	if len(mine) != 1 || mine[0].Shared {
		t.Fatalf("the owner's own mark was reported as shared: %+v", mine)
	}

	// Revoking the book takes the marks with it.
	if err := p.UnshareBook(ctx, book, owner, friend); err != nil {
		t.Fatalf("UnshareBook: %v", err)
	}
	seen, _ = p.Annotations(ctx, friend, book)
	if len(seen) != 0 {
		t.Fatalf("marginalia survived the book being unshared: %+v", seen)
	}
}

// The audit records other people's reading and not your own. That distinction
// is the difference between an audit trail and surveillance, and it lives in
// SQL so no handler can get it wrong.
func TestTheAuditRecordsOtherPeopleOnly(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	book := oneBook(t, p, owner)
	ctx := t.Context()
	// Lent, as anybody reading somebody else's book has been: the log records
	// who lent it, and the owner sees the reading done through their grants.
	if err := p.ShareBook(ctx, book, owner, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	// The owner reads their own book, repeatedly.
	for n := 1; n <= 5; n++ {
		if err := p.RecordAccess(ctx, book, owner, AccessPage, n); err != nil {
			t.Fatalf("RecordAccess: %v", err)
		}
	}
	log, err := p.BookAccessLog(ctx, book, owner)
	if err != nil {
		t.Fatalf("BookAccessLog: %v", err)
	}
	if len(log) != 0 {
		t.Fatalf("the owner's own reading was logged: %+v", log)
	}

	// Somebody else reads it.
	for n := 1; n <= 3; n++ {
		if err := p.RecordAccess(ctx, book, friend, AccessPage, n); err != nil {
			t.Fatalf("RecordAccess: %v", err)
		}
	}
	// And re-reads a page, which must count as a view and not a new unit.
	if err := p.RecordAccess(ctx, book, friend, AccessPage, 2); err != nil {
		t.Fatalf("RecordAccess: %v", err)
	}

	log, err = p.BookAccessLog(ctx, book, owner)
	if err != nil {
		t.Fatalf("BookAccessLog: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("want one record, got %+v", log)
	}
	if log[0].Views != 4 {
		t.Errorf("views = %d, want 4", log[0].Views)
	}
	// Three DISTINCT pages, even though there were four requests. This is the
	// number that answers "how much of the book did they take".
	if log[0].Units != 3 {
		t.Errorf("distinct units = %d, want 3", log[0].Units)
	}
	if log[0].Username == "" {
		t.Error("a record with no name attached attributes nothing")
	}
}

// Only the owner may read the log: it says what other people have been reading.
func TestTheAuditIsOwnerOnly(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	book := oneBook(t, p, owner)

	if _, err := p.BookAccessLog(t.Context(), book, friend); !isNotFound(err) {
		t.Fatalf("someone read the access log for a book that is not theirs: %v", err)
	}
}

// The shape of a bulk extraction: every page, in one sitting.
func TestBulkExtractionIsVisible(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	book := oneBook(t, p, owner)
	ctx := t.Context()

	if _, err := p.Exec(ctx, `UPDATE book_files SET unit_count = 40 WHERE id = $1`, book); err != nil {
		t.Fatalf("setting a page count: %v", err)
	}
	if err := p.ShareBook(ctx, book, owner, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	for n := 1; n <= 40; n++ {
		if err := p.RecordAccess(ctx, book, friend, AccessPage, n); err != nil {
			t.Fatalf("RecordAccess: %v", err)
		}
	}

	log, err := p.BookAccessLog(ctx, book, owner)
	if err != nil {
		t.Fatalf("BookAccessLog: %v", err)
	}
	if len(log) != 1 {
		t.Fatalf("want one record, got %d", len(log))
	}
	// The whole book, which is exactly what an owner would want to see.
	if log[0].Units != 40 || log[0].Of != 40 {
		t.Fatalf("units = %d of %d, want 40 of 40", log[0].Units, log[0].Of)
	}
}

// Losing access to a book takes it off every list, not just the shelf.
//
// Reading position and notes outlive the access that allowed them, so a book
// lent and taken back would otherwise keep its title in the borrower's
// "continue reading" and recent-notes lists - a small leak that survives the
// decision to stop sharing, and the kind that is only ever found by looking.
func TestRevokedAccessLeavesNoTraces(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	book := oneBook(t, p, owner)
	ctx := t.Context()

	if err := p.ShareBook(ctx, book, owner, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	// The borrower reads a little and makes a note.
	if _, err := p.SaveProgress(ctx, domain.ReadingProgress{
		UserID:     friend,
		BookFileID: book,
		Position:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 2, ElementPath: "/body/p[1]"},
	}); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	if _, err := p.CreateAnnotation(ctx, epubMark(friend, book, 2, "/body/p[1]", 0, 10)); err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	items, err := p.ContinueReading(ctx, friend, 10)
	if err != nil {
		t.Fatalf("ContinueReading: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("a borrowed book should be on the borrower's list: %+v", items)
	}

	// The owner takes it back.
	if err := p.UnshareBook(ctx, book, owner, friend); err != nil {
		t.Fatalf("UnshareBook: %v", err)
	}

	items, err = p.ContinueReading(ctx, friend, 10)
	if err != nil {
		t.Fatalf("ContinueReading: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("a revoked book is still on the continue list: %+v", items)
	}

	recentPage, err := p.RecentAnnotations(ctx, friend, 10, "")
	if err != nil {
		t.Fatalf("RecentAnnotations: %v", err)
	}
	if recent := recentPage.Items; len(recent) != 0 {
		t.Fatalf("a revoked book is still in the notes list: %+v", recent)
	}

	// And nothing new can be written against it.
	if _, err := p.CreateAnnotation(ctx, epubMark(friend, book, 3, "/body/p[2]", 0, 5)); !isNotFound(err) {
		t.Fatalf("a note was made on a book that is no longer readable: %v", err)
	}
}

// Replacing a cover reports the one it displaced, so the caller can delete the
// file. Every cover gets a fresh name, so without this each re-render would
// leave its predecessor on disk forever.
func TestSetCoverReportsWhatItReplaced(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	stranger := secondUser(t, p)
	book := oneBook(t, p, owner)
	ctx := t.Context()

	// The first cover replaces nothing.
	previous, err := p.SetCover(ctx, book, owner, "first.webp")
	if err != nil {
		t.Fatalf("SetCover: %v", err)
	}
	if previous != "" {
		t.Fatalf("the first cover replaced %q", previous)
	}

	// The second reports the first.
	previous, err = p.SetCover(ctx, book, owner, "second.webp")
	if err != nil {
		t.Fatalf("SetCover: %v", err)
	}
	if previous != "first.webp" {
		t.Fatalf("replaced = %q, want first.webp", previous)
	}

	// Storing the same name again is not a replacement.
	previous, err = p.SetCover(ctx, book, owner, "second.webp")
	if err != nil {
		t.Fatalf("SetCover: %v", err)
	}
	if previous != "" {
		t.Fatalf("re-storing the same name reported %q", previous)
	}

	stored, err := p.Cover(ctx, book, owner)
	if err != nil || stored != "second.webp" {
		t.Fatalf("Cover = %q, %v", stored, err)
	}

	// Somebody who cannot read the book cannot set or see its cover.
	if _, err := p.SetCover(ctx, book, stranger, "theirs.webp"); !isNotFound(err) {
		t.Fatalf("a stranger set a cover: %v", err)
	}
	if _, err := p.Cover(ctx, book, stranger); !isNotFound(err) {
		t.Fatalf("a stranger read a cover: %v", err)
	}
}

// A borrower is told about the file they were lent, and nothing else.
//
// Both halves of this were wrong at once. The library card listed every format
// the WORK was held in, so a book lent as an EPUB advertised "EPUB MOBI PDF";
// and the work page listed every file of the work with its id, size, hash and
// full filesystem path - four copies the borrower had no access to, and the
// owner's home directory along with them. The viewer was already a parameter of
// both queries. Neither used it.
func TestABorrowerSeesOnlyTheFileTheyWereLent(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	ctx := t.Context()

	// One work held as two files, which is what the leak needed. They are
	// matched into one work by their shared filename STEM - the same rule that
	// puts a real library's algebraessentials.epub and .pdf on one card, since
	// PDFs essentially never carry an ISBN. The stem has to be at least
	// MinStemMatchLength, which is why these filenames are not "a.epub".
	lent := owned(sampleInput(fmt.Sprintf("%064x", 21), "/library/twowaystoholdabook.epub",
		"Two Ways To Hold A Book", "An Author"), owner)
	kept := owned(sampleInput(fmt.Sprintf("%064x", 22), "/library/twowaystoholdabook.pdf",
		"Two Ways To Hold A Book", "An Author"), owner)
	kept.Format = domain.FormatPDF
	kept.Result.Format = domain.FormatPDF

	lentOut, err := p.UpsertBookFile(ctx, lent)
	if err != nil {
		t.Fatalf("seeding the lent file: %v", err)
	}
	if _, err := p.UpsertBookFile(ctx, kept); err != nil {
		t.Fatalf("seeding the kept file: %v", err)
	}

	if err := p.ShareBook(ctx, lentOut.BookFileID, owner, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	page, err := p.Library(ctx, LibraryQuery{ViewerID: friend})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("borrower sees %d works, want 1", len(page.Items))
	}
	item := page.Items[0]
	if len(item.Formats) != 1 || item.Formats[0] != domain.FormatEPUB {
		t.Errorf("card lists formats the borrower cannot open: %v", item.Formats)
	}
	if item.LentBy == "" {
		t.Error("a borrowed book does not say who lent it")
	}
	if item.CanDownload {
		t.Error("a book shared to read reports itself as downloadable")
	}

	detail, err := p.Work(ctx, item.WorkID, friend)
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(detail.Files) != 1 {
		t.Fatalf("borrower sees %d files, want 1: %+v", len(detail.Files), detail.Files)
	}
	if detail.Files[0].ID != lentOut.BookFileID {
		t.Errorf("the wrong file is visible: %v", detail.Files[0].ID)
	}
	if detail.Files[0].Path != "" {
		t.Errorf("a borrower was given the owner's path on disk: %q", detail.Files[0].Path)
	}

	// The owner still sees everything, including where the files are.
	ownerView, err := p.Work(ctx, item.WorkID, owner)
	if err != nil {
		t.Fatalf("Work as owner: %v", err)
	}
	if len(ownerView.Files) != 2 {
		t.Fatalf("owner sees %d files, want 2", len(ownerView.Files))
	}
	for _, f := range ownerView.Files {
		if f.Path == "" {
			t.Error("the owner cannot see where their own file is")
		}
	}
}

// The ownership filter answers "what did somebody lend me" without hiding what
// the viewer owns from the unfiltered shelf.
func TestTheShelfCanBeNarrowedToBorrowedBooks(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	ctx := t.Context()

	theirs := owned(sampleInput(fmt.Sprintf("%064x", 31), "/library/theirs.epub",
		"A Book They Own", "An Author"), friend)
	if _, err := p.UpsertBookFile(ctx, theirs); err != nil {
		t.Fatalf("seeding their own book: %v", err)
	}
	borrowed := oneBook(t, p, owner)
	if err := p.ShareBook(ctx, borrowed, owner, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	for _, c := range []struct {
		owned Ownership
		want  int
	}{
		{OwnedAny, 2},
		{OwnedMine, 1},
		{OwnedShared, 1},
	} {
		page, err := p.Library(ctx, LibraryQuery{ViewerID: friend, Owned: c.owned})
		if err != nil {
			t.Fatalf("Library(%q): %v", c.owned, err)
		}
		if len(page.Items) != c.want {
			t.Errorf("owned=%q returned %d works, want %d", c.owned, len(page.Items), c.want)
		}
	}
}

// The log is meant to warn, and a warning that fires on ordinary reading is one
// people learn to close without reading. So: bulk reading is notable, an
// evening's reading is not, and a download is notable whatever its size.
func TestSharingActivityMarksBulkReadingOnly(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	ctx := t.Context()

	// sampleInput seeds a three-item spine, so two chapters is most of it and
	// one is not.
	bulk := oneBook(t, p, owner)
	casual := owned(sampleInput(fmt.Sprintf("%064x", 51), "/library/casualreading.epub",
		"Read At Leisure", "An Author"), owner)
	casualOut, err := p.UpsertBookFile(ctx, casual)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	for _, b := range []uuid.UUID{bulk, casualOut.BookFileID} {
		if err := p.ShareBook(ctx, b, owner, friend, true, false); err != nil {
			t.Fatalf("ShareBook: %v", err)
		}
	}

	for _, unit := range []int{1, 2, 3} {
		if err := p.RecordAccess(ctx, bulk, friend, AccessChapter, unit); err != nil {
			t.Fatalf("RecordAccess: %v", err)
		}
	}
	if err := p.RecordAccess(ctx, casualOut.BookFileID, friend, AccessChapter, 1); err != nil {
		t.Fatalf("RecordAccess: %v", err)
	}

	activity, err := p.SharingActivity(ctx, owner, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("SharingActivity: %v", err)
	}
	if len(activity) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(activity), activity)
	}

	byBook := map[uuid.UUID]ShareActivity{}
	for _, a := range activity {
		byBook[a.BookFileID] = a
	}
	if !byBook[bulk].Notable {
		t.Errorf("reading a whole book in a day was not notable: %+v", byBook[bulk])
	}
	if byBook[casualOut.BookFileID].Notable {
		t.Errorf("one chapter of three was treated as bulk: %+v", byBook[casualOut.BookFileID])
	}

	// A download is notable however small the book: the file has left.
	if err := p.RecordAccess(ctx, casualOut.BookFileID, friend, AccessFile, 0); err != nil {
		t.Fatalf("RecordAccess(file): %v", err)
	}
	activity, err = p.SharingActivity(ctx, owner, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("SharingActivity: %v", err)
	}
	var sawDownload bool
	for _, a := range activity {
		if a.Kind == AccessFile {
			sawDownload = true
			if !a.Notable {
				t.Errorf("a whole-file download was not notable: %+v", a)
			}
		}
	}
	if !sawDownload {
		t.Error("the download was not recorded at all")
	}
}

// Activity is scoped to the books you own, in the statement rather than in a
// handler - the same rule as every other access decision here.
func TestSharingActivityIsOwnerScoped(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	ctx := t.Context()

	book := oneBook(t, p, owner)
	if err := p.ShareBook(ctx, book, owner, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	if err := p.RecordAccess(ctx, book, friend, AccessChapter, 1); err != nil {
		t.Fatalf("RecordAccess: %v", err)
	}

	// The borrower asking about their own lending sees nothing: they lent
	// nothing, and what they READ is not theirs to audit.
	activity, err := p.SharingActivity(ctx, friend, time.Now().Add(-time.Hour), 50)
	if err != nil {
		t.Fatalf("SharingActivity: %v", err)
	}
	if len(activity) != 0 {
		t.Fatalf("a borrower was shown %d rows about themselves: %+v", len(activity), activity)
	}
}

// A borrower can answer in the margin and share what they wrote.
//
// The store has always keyed annotation_access on whoever WROTE the marks, and
// nothing could reach it except the owner's share panel - so a reader could be
// lent a book and had no way to share a note back.
func TestABorrowerCanShareTheirOwnNotes(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	reader := secondUser(t, p)
	third := secondUser(t, p)
	ctx := t.Context()

	book := oneBook(t, p, owner)
	for _, who := range []uuid.UUID{reader, third} {
		if err := p.ShareBook(ctx, book, owner, who, false, false); err != nil {
			t.Fatalf("ShareBook: %v", err)
		}
	}

	mark := epubMark(reader, book, 1, "p[1]", 0, 20)
	mark.Note = "no it isn't"
	saved, err := p.CreateAnnotation(ctx, mark)
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	// Before sharing, the note is the reader's alone - not even the book's
	// owner sees it.
	seen, err := p.Annotations(ctx, owner, book)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("the owner saw a borrower's private note: %+v", seen)
	}

	if err := p.ShareAnnotations(ctx, book, reader, owner); err != nil {
		t.Fatalf("ShareAnnotations: %v", err)
	}
	seen, err = p.Annotations(ctx, owner, book)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(seen) != 1 || seen[0].ID != saved.ID {
		t.Fatalf("the shared note did not reach the owner: %+v", seen)
	}
	if !seen[0].Shared || seen[0].Author == "" {
		t.Errorf("a shared note arrived unattributed: %+v", seen[0])
	}

	// Their own shares, and nobody else's business.
	shares, err := p.AnnotationShares(ctx, book, reader)
	if err != nil {
		t.Fatalf("AnnotationShares: %v", err)
	}
	if len(shares) != 1 || shares[0].ID != owner {
		t.Fatalf("shares = %v, want [%v]", shares, owner)
	}

	// A third person who can read the book still cannot see notes that were
	// not shared with them.
	seen, err = p.Annotations(ctx, third, book)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("a note shared with one person was visible to another: %+v", seen)
	}

	if err := p.UnshareAnnotations(ctx, book, reader, owner); err != nil {
		t.Fatalf("UnshareAnnotations: %v", err)
	}
	seen, _ = p.Annotations(ctx, owner, book)
	if len(seen) != 0 {
		t.Fatalf("the note survived being unshared: %+v", seen)
	}
}

// Notes are only ever shown on a book the reader may open, and that is checked
// when they are READ rather than only when they are shared.
//
// The case is a third party's share. UnshareBook removes the annotation_access
// rows the owner granted, which is the only kind that used to exist - but a
// borrower sharing their notes with somebody else writes a row the owner's
// revocation does not touch. Without a check at the point of reading, revoking
// the book would leave those marks visible, and a highlight carries the text it
// highlighted: the book would leak one mark at a time.
func TestNotesAreInvisibleWithoutAccessToTheBook(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	reader := secondUser(t, p)
	friendOfReader := secondUser(t, p)
	ctx := t.Context()

	book := oneBook(t, p, owner)
	for _, who := range []uuid.UUID{reader, friendOfReader} {
		if err := p.ShareBook(ctx, book, owner, who, false, false); err != nil {
			t.Fatalf("ShareBook: %v", err)
		}
	}

	mark := epubMark(reader, book, 0, "p[1]", 0, 24)
	mark.SelectedText = "the whole of chapter one"
	if _, err := p.CreateAnnotation(ctx, mark); err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	// The borrower shares their notes with a friend, which is a row the owner
	// did not create and does not know about.
	if err := p.ShareAnnotations(ctx, book, reader, friendOfReader); err != nil {
		t.Fatalf("ShareAnnotations: %v", err)
	}

	seen, _ := p.Annotations(ctx, friendOfReader, book)
	if len(seen) != 1 {
		t.Fatalf("the shared note did not arrive: %+v", seen)
	}

	// The owner withdraws the BOOK from the friend. Their share of the
	// borrower's notes is untouched by that, and must stop working anyway.
	if err := p.UnshareBook(ctx, book, owner, friendOfReader); err != nil {
		t.Fatalf("UnshareBook: %v", err)
	}

	var stillShared bool
	if err := p.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM annotation_access
		                WHERE book_file_id = $1 AND owner_id = $2 AND viewer_id = $3)`,
		book, reader, friendOfReader).Scan(&stillShared); err != nil {
		t.Fatalf("reading annotation_access: %v", err)
	}
	if !stillShared {
		t.Fatal("the premise no longer holds: revoking the book removed a third party's note share, so this test proves nothing")
	}

	seen, err := p.Annotations(ctx, friendOfReader, book)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(seen) != 0 {
		t.Fatalf("notes outlived access to the book they annotate: %+v", seen)
	}
}

// The cover queue is handed out 24 at a time, so the number of books still
// missing one cannot be counted from a batch. The count is separate, and it is
// scoped to what the person asking can actually see: a shelf does not report
// work on somebody else's books.
func TestCountingFilesThatStillNeedCovers(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	stranger := secondUser(t, p)
	ctx := t.Context()

	var books []uuid.UUID
	for i := range 3 {
		in := owned(sampleInput(fmt.Sprintf("%064x", 100+i),
			fmt.Sprintf("/library/needs-cover-%d.pdf", i), fmt.Sprintf("Book %d", i), "An Author"), owner)
		// Only PDFs need one rendered: an EPUB carries its own cover image.
		in.Format = domain.FormatPDF
		in.Result.Format = domain.FormatPDF
		out, err := p.UpsertBookFile(ctx, in)
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}
		books = append(books, out.BookFileID)
	}

	count := func(who uuid.UUID) int {
		t.Helper()
		n, err := p.CountFilesNeedingCovers(ctx, who)
		if err != nil {
			t.Fatalf("CountFilesNeedingCovers: %v", err)
		}
		return n
	}

	if got := count(owner); got != 3 {
		t.Fatalf("owner sees %d books needing covers, want 3", got)
	}
	// Nobody else can see them, so nobody else has work to do.
	if got := count(stranger); got != 0 {
		t.Fatalf("a stranger sees %d books needing covers, want 0", got)
	}

	if _, err := p.SetCover(ctx, books[0], owner, "one.webp"); err != nil {
		t.Fatalf("SetCover: %v", err)
	}
	if got := count(owner); got != 2 {
		t.Fatalf("after one cover the owner sees %d, want 2", got)
	}
}
