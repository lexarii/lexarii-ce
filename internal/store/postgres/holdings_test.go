// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// holders lists who holds a file, for assertions.
func holders(t *testing.T, p *Pool, bookFileID uuid.UUID) map[uuid.UUID]string {
	t.Helper()
	rows, err := p.Query(t.Context(), `
		SELECT user_id, original_filename FROM book_file_holders WHERE book_file_id = $1`, bookFileID)
	if err != nil {
		t.Fatalf("reading holders: %v", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			t.Fatalf("scanning holder: %v", err)
		}
		out[id] = name
	}
	return out
}

// canRead reports what readable_book_files says, which is what every serving
// path joins.
func canRead(t *testing.T, p *Pool, bookFileID, user uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := p.QueryRow(t.Context(), `
		SELECT EXISTS (SELECT 1 FROM readable_book_files
		                WHERE book_file_id = $1 AND user_id = $2)`, bookFileID, user).Scan(&ok); err != nil {
		t.Fatalf("checking access: %v", err)
	}
	return ok
}

// uploadInput is what the extract job writes for one person's upload.
func uploadInput(hash, filename string, owner uuid.UUID) IngestInput {
	in := sampleInput(hash, "/data/uploads/books/"+hash[:2]+"/"+hash+".epub", "A Book Two People Have", "An Author")
	in.OriginalFilename = filename
	in.Uploaded = true
	in.OwnerID = owner
	return in
}

// Two people adding the same bytes each hold the file, whatever order they
// come in and whether or not the first one's extraction has finished. And
// "already held" is only ever about the person asking.
func TestTwoPeopleHoldOneFile(t *testing.T) {
	type step struct {
		// who uploads; zero means "the extraction runs now".
		who      string
		filename string
		// wantAlready is what HoldUpload must say to them.
		wantAlready bool
	}
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"same person twice, settled", []step{
			{"alice", "mine.epub", false}, {"", "", false}, {"alice", "again.epub", true},
		}},
		{"same person twice, still extracting", []step{
			{"alice", "mine.epub", false}, {"alice", "again.epub", true}, {"", "", false},
		}},
		{"alice then bob, settled", []step{
			{"alice", "alice.epub", false}, {"", "", false}, {"bob", "bob.epub", false},
		}},
		{"bob then alice, settled", []step{
			{"bob", "bob.epub", false}, {"", "", false}, {"alice", "alice.epub", false},
		}},
		{"alice then bob, still extracting", []step{
			{"alice", "alice.epub", false}, {"bob", "bob.epub", false}, {"", "", false},
		}},
		{"bob then alice, still extracting", []step{
			{"bob", "bob.epub", false}, {"alice", "alice.epub", false}, {"", "", false},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPool(t)
			cleanLibrary(t, p)
			ctx := t.Context()
			people := map[string]uuid.UUID{"alice": testOwner(t, p), "bob": secondUser(t, p)}
			hash := fmt.Sprintf("%064x", 0xb0)

			// The first uploader's job is the one that extracts the file.
			first := people[tc.steps[0].who]
			firstName := tc.steps[0].filename
			names := map[uuid.UUID]string{}
			var fileID uuid.UUID

			for _, s := range tc.steps {
				if s.who == "" {
					out, err := p.UpsertBookFile(ctx, uploadInput(hash, firstName, first))
					if err != nil {
						t.Fatalf("extracting: %v", err)
					}
					fileID = out.BookFileID
					continue
				}
				who := people[s.who]
				got, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: who, OriginalFilename: s.filename})
				if err != nil {
					t.Fatalf("HoldUpload(%s): %v", s.who, err)
				}
				if got.AlreadyHeld != s.wantAlready {
					t.Errorf("%s: AlreadyHeld = %v, want %v", s.who, got.AlreadyHeld, s.wantAlready)
				}
				if _, seen := names[who]; !seen {
					names[who] = s.filename
				}
			}

			got := holders(t, p, fileID)
			if len(got) != len(names) {
				t.Fatalf("holders = %v, want %v", got, names)
			}
			for who, name := range names {
				// Each person's own name for the file, never the other's,
				// and never renamed by their own second upload.
				if got[who] != name {
					t.Errorf("holder %s filename = %q, want %q", who, got[who], name)
				}
				if !canRead(t, p, fileID, who) {
					t.Errorf("%s cannot read a book they added", who)
				}
			}

			var claims int
			if err := p.QueryRow(ctx, `SELECT count(*) FROM holding_claims`).Scan(&claims); err != nil {
				t.Fatalf("counting claims: %v", err)
			}
			if claims != 0 {
				t.Errorf("%d claims left undrained after the extraction", claims)
			}
		})
	}
}

// The file keeps the name it first arrived with, for merging by stem. A later
// upload under another name renames nobody's book.
func TestALaterUploadRenamesNothing(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice, bob := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb1)

	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "the-real-title.epub", alice))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: bob, OriginalFilename: "bobs-copy.epub"}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "bobs-copy.epub", bob)); err != nil {
		t.Fatalf("second: %v", err)
	}

	var fileName string
	if err := p.QueryRow(ctx, `SELECT original_filename FROM book_files WHERE id = $1`,
		out.BookFileID).Scan(&fileName); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if fileName != "the-real-title.epub" {
		t.Errorf("the file's own name became %q", fileName)
	}

	for who, want := range map[uuid.UUID]string{alice: "the-real-title.epub", bob: "bobs-copy.epub"} {
		d, err := p.Work(ctx, out.WorkID, who)
		if err != nil {
			t.Fatalf("Work: %v", err)
		}
		if len(d.Filenames) != 1 || d.Filenames[0] != want {
			t.Errorf("work detail shows %v, want %q", d.Filenames, want)
		}
		if len(d.Held) != 1 || !d.Held[0] {
			t.Errorf("a holder's work detail says held = %v", d.Held)
		}
	}
}

// An upload of a book that sits in the library directory does not move the
// file's path into Lexarii's storage, and a scan of a book Lexarii stores does
// not move it into the library directory. Only a file that has gone missing
// adopts the copy that exists.
func TestAPathBelongsToTheFile(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice, bob := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb2)

	scanned := owned(sampleInput(hash, "/library/shelf/book.epub", "Scanned", "An Author"), alice)
	out, err := p.UpsertBookFile(ctx, scanned)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	path := func() string {
		var path string
		if err := p.QueryRow(ctx, `SELECT path FROM book_files WHERE id = $1`, out.BookFileID).Scan(&path); err != nil {
			t.Fatalf("reading path: %v", err)
		}
		return path
	}

	if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "upload.epub", bob)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := path(); got != "/library/shelf/book.epub" {
		t.Fatalf("an upload moved a scanned file to %q", got)
	}

	// Bob holds it now, but where Alice keeps her books is not his business:
	// he is shown the name he gave it, and not her directory.
	for _, tc := range []struct {
		who      uuid.UUID
		path     string
		filename string
	}{
		{alice, "/library/shelf/book.epub", ""},
		{bob, "", "upload.epub"},
	} {
		d, err := p.Work(ctx, out.WorkID, tc.who)
		if err != nil {
			t.Fatalf("Work: %v", err)
		}
		if len(d.Files) != 1 || d.Files[0].Path != tc.path || d.Filenames[0] != tc.filename {
			t.Errorf("work detail for %s: path %q, filename %v; want %q, %q",
				tc.who, d.Files[0].Path, d.Filenames, tc.path, tc.filename)
		}
	}

	if _, err := p.Exec(ctx, `UPDATE book_files SET missing_since = now() WHERE id = $1`, out.BookFileID); err != nil {
		t.Fatalf("marking missing: %v", err)
	}
	if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "upload.epub", bob)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if got := path(); got == "/library/shelf/book.epub" {
		t.Fatal("a missing file did not adopt the uploaded copy that exists")
	}

	// Now Lexarii stores it; finding the same bytes in the library directory
	// does not make it a scanned file again.
	if err := p.TouchSeen(ctx, hash, "/library/elsewhere/book.epub", alice); err != nil {
		t.Fatalf("TouchSeen: %v", err)
	}
	if got := path(); got == "/library/elsewhere/book.epub" {
		t.Fatal("a scan moved a file Lexarii stores into the library directory")
	}
}

// A scan makes whoever ran it a holder of what it finds, even a book somebody
// else added first - but it never claims an archived book.
func TestAScanHoldsWhatItFinds(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice := testOwner(t, p)
	bob := secondUser(t, p)
	if _, err := p.Exec(ctx, `UPDATE users SET is_admin = true WHERE id = $1`, bob); err != nil {
		t.Fatalf("promoting: %v", err)
	}

	live := fmt.Sprintf("%064x", 0xb3)
	out, err := p.UpsertBookFile(ctx, owned(sampleInput(live, "/library/a.epub", "Live", "A"), alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := p.TouchSeen(ctx, live, "/library/a.epub", bob); err != nil {
		t.Fatalf("TouchSeen: %v", err)
	}
	// Bob's library directory has it, so it is on his shelf too - whoever
	// added it first, and without the difference saying who.
	if got := holders(t, p, out.BookFileID); len(got) != 2 {
		t.Errorf("holders after bob's scan = %v, want alice and bob", got)
	}

	gone := secondUser(t, p)
	archived := fmt.Sprintf("%064x", 0xb4)
	aout, err := p.UpsertBookFile(ctx, owned(sampleInput(archived, "/library/b.epub", "Archived", "B"), gone))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.DeleteAccount(ctx, alice, gone); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if err := p.TouchSeen(ctx, archived, "/library/b.epub", bob); err != nil {
		t.Fatalf("TouchSeen: %v", err)
	}
	if got := holders(t, p, aout.BookFileID); len(got) != 0 {
		t.Errorf("a rescan claimed an archived book: holders = %v", got)
	}
	if waiting, _ := p.ArchivedBooks(ctx, alice); len(waiting) != 1 {
		t.Errorf("archived = %+v, want the book still waiting", waiting)
	}
}

// Somebody uploading a book whose only holder deleted their account gets it,
// like anybody else uploading it: the reply would otherwise be a book that
// vanishes the moment it is added.
func TestUploadingAnArchivedBookBringsItBack(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin := testOwner(t, p)
	leaver, newcomer := secondUser(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb5)

	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "left.epub", leaver))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.DeleteAccount(ctx, admin, leaver); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}

	got, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: newcomer, OriginalFilename: "found.epub"})
	if err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if got.AlreadyHeld {
		t.Error("a newcomer was told they already had the book")
	}
	if !canRead(t, p, out.BookFileID, newcomer) {
		t.Fatal("the newcomer cannot read the book they just uploaded")
	}
	if waiting, _ := p.ArchivedBooks(ctx, admin); len(waiting) != 0 {
		t.Errorf("the book is still waiting in the archive: %+v", waiting)
	}
}

// Each holder lends independently and sees only their own lending.
func TestGrantsBelongToTheHolderWhoMadeThem(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice, bob, carol := testOwner(t, p), secondUser(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb6)

	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	book := out.BookFileID
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: bob}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}

	if err := p.ShareBook(ctx, book, alice, carol, false, false); err != nil {
		t.Fatalf("alice lends: %v", err)
	}

	// Bob sees none of it and cannot undo it.
	shares, err := p.BookShares(ctx, book, bob)
	if err != nil {
		t.Fatalf("BookShares: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("bob sees alice's lending: %+v", shares)
	}
	if err := p.UnshareBook(ctx, book, bob, carol); !isNotFound(err) {
		t.Fatalf("bob revoked alice's grant: %v", err)
	}
	if !canRead(t, p, book, carol) {
		t.Fatal("carol lost the book alice lent her")
	}

	// Bob lends it too, with the download alice withheld.
	if err := p.ShareBook(ctx, book, bob, carol, true, false); err != nil {
		t.Fatalf("bob lends: %v", err)
	}
	if shares, _ := p.BookShares(ctx, book, alice); len(shares) != 1 || shares[0].CanDownload {
		t.Errorf("alice's view of her own grant changed: %+v", shares)
	}

	// Carol reads through both grants; each lender sees it.
	if err := p.RecordAccess(ctx, book, carol, AccessPage, 1); err != nil {
		t.Fatalf("RecordAccess: %v", err)
	}
	// Alice withdraws. Carol still reads, through Bob.
	if err := p.UnshareBook(ctx, book, alice, carol); err != nil {
		t.Fatalf("alice withdraws: %v", err)
	}
	if !canRead(t, p, book, carol) {
		t.Fatal("alice's withdrawal undid bob's grant")
	}
	for _, lender := range []uuid.UUID{alice, bob} {
		log, err := p.BookAccessLog(ctx, book, lender)
		if err != nil {
			t.Fatalf("BookAccessLog: %v", err)
		}
		// Alice's record outlives her grant: that is when it is wanted.
		if len(log) != 1 || log[0].UserID != carol {
			t.Errorf("lender %s sees %+v, want carol's reading", lender, log)
		}
	}

	// Dave reads only through Alice: Bob learns nothing of it.
	dave := secondUser(t, p)
	if err := p.ShareBook(ctx, book, alice, dave, false, false); err != nil {
		t.Fatalf("alice lends to dave: %v", err)
	}
	if err := p.RecordAccess(ctx, book, dave, AccessPage, 1); err != nil {
		t.Fatalf("RecordAccess: %v", err)
	}
	log, err := p.BookAccessLog(ctx, book, bob)
	if err != nil {
		t.Fatalf("BookAccessLog: %v", err)
	}
	for _, r := range log {
		if r.UserID == dave {
			t.Error("bob's access log shows reading done through alice's grant")
		}
	}
	activity, err := p.SharingActivity(ctx, bob, time.Now().AddDate(-1, 0, 0), 50)
	if err != nil {
		t.Fatalf("SharingActivity: %v", err)
	}
	for _, a := range activity {
		if a.UserID == dave {
			t.Error("bob's activity feed shows reading done through alice's grant")
		}
	}

	// The shelf names who lent it to the reader, not some other holder.
	page, err := p.Library(ctx, LibraryQuery{ViewerID: dave})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].LentBy == "" {
		t.Fatalf("dave's shelf = %+v", page.Items)
	}
	var aliceName string
	if err := p.QueryRow(ctx, `SELECT username FROM users WHERE id = $1`, alice).Scan(&aliceName); err != nil {
		t.Fatalf("reading name: %v", err)
	}
	if page.Items[0].LentBy != aliceName {
		t.Errorf("lent by %q, want %q", page.Items[0].LentBy, aliceName)
	}
	// A borrower does not hold it, so it is not theirs to take off a shelf.
	if d, err := p.Work(ctx, out.WorkID, dave); err != nil || len(d.Held) != 1 || d.Held[0] {
		t.Errorf("a borrower's work detail: held = %v, err %v", d.Held, err)
	}
}

// A holder's own reading is never logged, whoever else holds the file.
func TestAHolderIsNeverAudited(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice, bob := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb7)

	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: bob}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	// Even holding a grant as well, from before they added it themselves.
	if err := p.ShareBook(ctx, out.BookFileID, alice, bob, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	for _, who := range []uuid.UUID{alice, bob} {
		if err := p.RecordAccess(ctx, out.BookFileID, who, AccessFile, 0); err != nil {
			t.Fatalf("RecordAccess: %v", err)
		}
	}
	var rows int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM access_log WHERE book_file_id = $1`,
		out.BookFileID).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d access_log rows for holders reading their own book", rows)
	}
}

// Deleting one holder leaves the book with the other: not archived, not moved,
// still lent by whoever else lends it. The deleted holder's own lending ends.
// Deleting the last holder archives it, as before.
func TestDeletingAHolder(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin := testOwner(t, p)
	alice, bob, carol, dave := secondUser(t, p), secondUser(t, p), secondUser(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb8)

	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	book := out.BookFileID
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: bob}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if err := p.ShareBook(ctx, book, alice, carol, false, false); err != nil {
		t.Fatalf("alice lends: %v", err)
	}
	if err := p.ShareBook(ctx, book, bob, dave, false, false); err != nil {
		t.Fatalf("bob lends: %v", err)
	}

	archived, err := p.DeleteAccount(ctx, admin, alice)
	if err != nil {
		t.Fatalf("DeleteAccount(alice): %v", err)
	}
	if len(archived) != 0 {
		t.Fatalf("a book bob still holds was archived (and its file would be moved): %+v", archived)
	}
	if !canRead(t, p, book, bob) {
		t.Fatal("bob lost a book he holds because alice left")
	}
	if canRead(t, p, book, carol) {
		t.Error("carol still reads through the grant of somebody who has left")
	}
	if !canRead(t, p, book, dave) {
		t.Error("dave lost the book bob lends him because alice left")
	}
	if waiting, _ := p.ArchivedBooks(ctx, admin); len(waiting) != 0 {
		t.Errorf("archive = %+v, want nothing", waiting)
	}

	archived, err = p.DeleteAccount(ctx, admin, bob)
	if err != nil {
		t.Fatalf("DeleteAccount(bob): %v", err)
	}
	if len(archived) != 1 || archived[0].BookFileID != book {
		t.Fatalf("the last holder's deletion archived %+v, want the book", archived)
	}
	if canRead(t, p, book, dave) {
		t.Error("an archived book is still readable through a grant")
	}

	// Claimed, the claimant holds it.
	if err := p.ClaimArchived(ctx, admin, book, carol); err != nil {
		t.Fatalf("ClaimArchived: %v", err)
	}
	if got := holders(t, p, book); len(got) != 1 {
		t.Fatalf("holders after the claim = %v, want carol", got)
	} else if _, ok := got[carol]; !ok {
		t.Fatalf("holders after the claim = %v, want carol", got)
	}
	if canRead(t, p, book, dave) {
		t.Error("a grant from a deleted holder came back with the claim")
	}
	if err := p.ClaimArchived(ctx, admin, book, dave); !isNotFound(err) {
		t.Errorf("claiming a book that is no longer archived = %v, want not found", err)
	}
}

// work_editors is who may edit a shared work: anybody holding a file of it,
// and nobody it is merely lent to.
func TestWorkEditorsAreItsHolders(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice, bob, carol := testOwner(t, p), secondUser(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb9)

	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: bob}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if err := p.ShareBook(ctx, out.BookFileID, alice, carol, true, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	for _, tc := range []struct {
		name string
		who  uuid.UUID
		may  bool
	}{
		{"the first holder", alice, true},
		{"a second holder", bob, true},
		{"a borrower", carol, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			title := "Renamed by " + tc.name
			_, err := p.UpdateWork(ctx, out.WorkID, tc.who, WorkEdit{Title: &title})
			if tc.may && err != nil {
				t.Fatalf("UpdateWork: %v", err)
			}
			if !tc.may && !isNotFound(err) {
				t.Fatalf("UpdateWork = %v, want not found", err)
			}
		})
	}
}

// A job is visible to who it was queued for and to administrators.
func TestAJobIsVisibleToWhoAskedAndAdministrators(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, alice, bob := testOwner(t, p), secondUser(t, p), secondUser(t, p)

	target := uuid.New()
	id, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobExtract, TargetID: &target, RequestedBy: alice})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	unowned, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobEmbed})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	t.Cleanup(func() { _, _ = p.Exec(ctx, `DELETE FROM jobs WHERE id = ANY($1)`, []uuid.UUID{id, unowned}) })

	for _, tc := range []struct {
		name   string
		job    uuid.UUID
		viewer uuid.UUID
		sees   bool
	}{
		{"the person it was queued for", id, alice, true},
		{"somebody else", id, bob, false},
		{"an administrator", id, admin, true},
		{"nobody", id, uuid.Nil, false},
		{"the installation's own work, a reader", unowned, alice, false},
		{"the installation's own work, an administrator", unowned, admin, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.GetJobFor(ctx, tc.job, tc.viewer)
			if tc.sees && err != nil {
				t.Fatalf("GetJobFor: %v", err)
			}
			if !tc.sees && !isNotFound(err) {
				t.Fatalf("GetJobFor = %v, want not found", err)
			}
		})
	}
}

// What a person's shelf takes up is a sum over their holdings, each holder
// counted for the whole file.
func TestHeldStorage(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice, bob := testOwner(t, p), secondUser(t, p)

	shared := fmt.Sprintf("%064x", 0xba)
	if _, err := p.UpsertBookFile(ctx, uploadInput(shared, "s.epub", alice)); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: shared, UserID: bob}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	own := fmt.Sprintf("%064x", 0xbb)
	in := uploadInput(own, "o.epub", alice)
	in.SizeBytes = 1000
	if _, err := p.UpsertBookFile(ctx, in); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for _, tc := range []struct {
		who   uuid.UUID
		files int
		bytes int64
	}{
		{alice, 2, 4096 + 1000},
		{bob, 1, 4096},
	} {
		got, err := p.HeldStorage(ctx, tc.who)
		if err != nil {
			t.Fatalf("HeldStorage: %v", err)
		}
		if got.Files != tc.files || got.Bytes != tc.bytes {
			t.Errorf("HeldStorage = %+v, want %d files, %d bytes", got, tc.files, tc.bytes)
		}
	}
}

// An upload joins only an edition its uploader already holds a file of.
// Matching across the installation would let an uploader attach a file to
// somebody else's book by its ISBN or filename - and
// learn the book is here, and become one of its editors. A scan, which is an
// administrator reading their own directory, matches as before.
func TestAnUploadJoinsOnlyItsUploadersBooks(t *testing.T) {
	const isbn = "9780131103627"
	withISBN := func(in IngestInput, format domain.Format) IngestInput {
		in.Format = format
		in.Result.Format = format
		in.Result.Metadata.ISBN13 = isbn
		return in
	}

	for _, tc := range []struct {
		name string
		// second builds the second file from its owner and whether it is the
		// first file's owner.
		sameOwner bool
		uploaded  bool
		byStem    bool
		wantJoin  bool
	}{
		{"somebody else's ISBN, uploaded", false, true, false, false},
		{"somebody else's filename, uploaded", false, true, true, false},
		{"their own ISBN, uploaded", true, true, false, true},
		{"their own filename, uploaded", true, true, true, true},
		// A scan is scoped to the scanning administrator's own books too.
		{"somebody else's ISBN, scanned", false, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPool(t)
			cleanLibrary(t, p)
			ctx := t.Context()
			alice, mallory := testOwner(t, p), secondUser(t, p)

			first := withISBN(uploadInput(fmt.Sprintf("%064x", 0xc1), "the-c-programming-language.epub", alice), domain.FormatEPUB)
			out1, err := p.UpsertBookFile(ctx, first)
			if err != nil {
				t.Fatalf("first: %v", err)
			}

			owner := mallory
			if tc.sameOwner {
				owner = alice
			}
			second := uploadInput(fmt.Sprintf("%064x", 0xc2), "something-else-entirely.pdf", owner)
			second.Format, second.Result.Format = domain.FormatPDF, domain.FormatPDF
			second.Result.Metadata.Title = "A Different Title"
			if tc.byStem {
				second.OriginalFilename = "the-c-programming-language.pdf"
			} else {
				second.Result.Metadata.ISBN13 = isbn
			}
			if !tc.uploaded {
				second.Uploaded = false
				second.OriginalFilename = ""
				second.Path = "/library/scan.pdf"
			}
			out2, err := p.UpsertBookFile(ctx, second)
			if err != nil {
				t.Fatalf("second: %v", err)
			}

			if joined := out2.WorkID == out1.WorkID; joined != tc.wantJoin {
				t.Fatalf("joined the first book = %v, want %v", joined, tc.wantJoin)
			}
			if !tc.wantJoin {
				// Nothing of the first book reaches the second person.
				d, err := p.Work(ctx, out2.WorkID, owner)
				if err != nil {
					t.Fatalf("Work: %v", err)
				}
				if d.Work.Title != "A Different Title" {
					t.Errorf("the uploader's book took the title %q", d.Work.Title)
				}
				title := "Renamed"
				if _, err := p.UpdateWork(ctx, out1.WorkID, owner, WorkEdit{Title: &title}); !isNotFound(err) {
					t.Errorf("the uploader can edit the other book: %v", err)
				}
			}
		})
	}
}

// The archive move happens after the archive commits, under the file's lock and
// only while the book is still archived and still where it was. An upload in
// between brings the book back, and then it is not moved at all - moved, it
// would no longer be where that upload expects it.
func TestTheArchiveMoveWaitsForTheFile(t *testing.T) {
	for _, tc := range []struct {
		name        string
		broughtBack bool
		wantMoved   bool
	}{
		{"still archived", false, true},
		{"brought back by an upload first", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPool(t)
			cleanLibrary(t, p)
			ctx := t.Context()
			admin := testOwner(t, p)
			leaver, newcomer := secondUser(t, p), secondUser(t, p)
			hash := fmt.Sprintf("%064x", 0xc3)

			out, err := p.UpsertBookFile(ctx, uploadInput(hash, "left.epub", leaver))
			if err != nil {
				t.Fatalf("seeding: %v", err)
			}
			archived, err := p.DeleteAccount(ctx, admin, leaver)
			if err != nil || len(archived) != 1 {
				t.Fatalf("DeleteAccount = %+v, %v", archived, err)
			}
			from := archived[0].Path

			if tc.broughtBack {
				if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: newcomer}); err != nil {
					t.Fatalf("HoldUpload: %v", err)
				}
			}

			called := false
			to, err := p.MoveArchived(ctx, out.BookFileID, from, func(path string) (string, error) {
				called = true
				return "/data/archive/left.epub", nil
			})
			if err != nil {
				t.Fatalf("MoveArchived: %v", err)
			}
			if called != tc.wantMoved || (to != "") != tc.wantMoved {
				t.Fatalf("moved = %v (to %q), want %v", called, to, tc.wantMoved)
			}

			var path string
			var archivedPath *string
			if err := p.QueryRow(ctx, `SELECT path, archived_path FROM book_files WHERE id = $1`,
				out.BookFileID).Scan(&path, &archivedPath); err != nil {
				t.Fatalf("reading: %v", err)
			}
			if tc.wantMoved {
				if path != "/data/archive/left.epub" || archivedPath == nil || *archivedPath != path {
					t.Errorf("path %q, archived_path %v; want both the archive", path, archivedPath)
				}
				return
			}
			if path != from || archivedPath != nil {
				t.Errorf("a book brought back was moved: path %q, archived_path %v", path, archivedPath)
			}
		})
	}
}

// An uploader whose recorded copy has gone missing is told so - the upload is
// needed - rather than that they already have it.
func TestHoldUploadReportsAMissingFile(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice := testOwner(t, p)
	hash := fmt.Sprintf("%064x", 0xc4)

	out, err := p.UpsertBookFile(ctx, owned(sampleInput(hash, "/library/gone.epub", "Gone", "A"), alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE book_files SET missing_since = now() WHERE id = $1`, out.BookFileID); err != nil {
		t.Fatalf("marking missing: %v", err)
	}
	got, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: alice})
	if err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if !got.AlreadyHeld || !got.Missing {
		t.Errorf("HoldUpload = %+v, want already held and missing", got)
	}
}

// Taking a book off your shelf ends your holding and nothing else of anybody's.
// When somebody else holds it too it stays with them, lent as they lend it;
// when you were the last it is archived exactly as account deletion would.
// Your own notes and position are kept either way.
func TestReleasingAHolding(t *testing.T) {
	for _, tc := range []struct {
		name         string
		bobHoldsToo  bool
		wantArchived bool
	}{
		{"another holder remains", true, false},
		{"the last holder", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPool(t)
			cleanLibrary(t, p)
			ctx := t.Context()
			admin := testOwner(t, p)
			alice, bob, carol, dave := secondUser(t, p), secondUser(t, p), secondUser(t, p), secondUser(t, p)
			hash := fmt.Sprintf("%064x", 0xd1)

			out, err := p.UpsertBookFile(ctx, uploadInput(hash, "mine.epub", alice))
			if err != nil {
				t.Fatalf("seeding: %v", err)
			}
			book := out.BookFileID
			if tc.bobHoldsToo {
				if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: bob}); err != nil {
					t.Fatalf("HoldUpload: %v", err)
				}
				if err := p.ShareBook(ctx, book, bob, dave, false, false); err != nil {
					t.Fatalf("bob lends: %v", err)
				}
			}
			// Alice lends it, shares her notes with the borrower, and reads.
			if err := p.ShareBook(ctx, book, alice, carol, false, true); err != nil {
				t.Fatalf("alice lends: %v", err)
			}
			mark, err := p.CreateAnnotation(ctx, epubMark(alice, book, 1, "section[1]/p[1]", 0, 10))
			if err != nil {
				t.Fatalf("CreateAnnotation: %v", err)
			}
			save(t, p, alice, book, epubPosition(1, "section[1]/p[1]", 0), 0.3)
			before, err := p.HeldStorage(ctx, alice)
			if err != nil || before.Files != 1 {
				t.Fatalf("HeldStorage before = %+v, %v", before, err)
			}

			archived, err := p.ReleaseHolding(ctx, book, alice)
			if err != nil {
				t.Fatalf("ReleaseHolding: %v", err)
			}
			if got := len(archived) == 1; got != tc.wantArchived {
				t.Fatalf("archived = %+v, want archived %v", archived, tc.wantArchived)
			}

			if canRead(t, p, book, alice) {
				t.Error("alice still reads a book she took off her shelf")
			}
			if canRead(t, p, book, carol) {
				t.Error("carol still reads through the grant of somebody who let the book go")
			}
			var noteShares int
			if err := p.QueryRow(ctx, `SELECT count(*) FROM annotation_access WHERE owner_id = $1`,
				alice).Scan(&noteShares); err != nil {
				t.Fatalf("counting note shares: %v", err)
			}
			if noteShares != 0 {
				t.Error("alice's notes on a book she let go are still shared")
			}
			after, err := p.HeldStorage(ctx, alice)
			if err != nil || after.Files != 0 || after.Bytes != 0 {
				t.Errorf("HeldStorage after = %+v, %v; want nothing", after, err)
			}

			// What she wrote is hers and is kept.
			var marks, positions int
			if err := p.QueryRow(ctx, `SELECT count(*) FROM annotations WHERE id = $1`, mark.ID).Scan(&marks); err != nil {
				t.Fatalf("counting marks: %v", err)
			}
			if err := p.QueryRow(ctx, `SELECT count(*) FROM reading_progress WHERE user_id = $1 AND book_file_id = $2`,
				alice, book).Scan(&positions); err != nil {
				t.Fatalf("counting positions: %v", err)
			}
			if marks != 1 || positions != 1 {
				t.Errorf("marks = %d, positions = %d; want both kept", marks, positions)
			}

			waiting, err := p.ArchivedBooks(ctx, admin)
			if err != nil {
				t.Fatalf("ArchivedBooks: %v", err)
			}
			if tc.wantArchived {
				if len(waiting) != 1 || waiting[0].BookFileID != book || waiting[0].ArchivedOwner == "" {
					t.Errorf("archive = %+v, want the book under alice's name", waiting)
				}
				return
			}
			if len(waiting) != 0 {
				t.Errorf("a book bob holds was archived: %+v", waiting)
			}
			if !canRead(t, p, book, bob) || !canRead(t, p, book, dave) {
				t.Error("alice letting go changed bob's book or his lending")
			}
		})
	}
}

// Releasing is only ever of your own holding, and anything else is "not
// found": a borrower, somebody who never had it, a file that does not exist,
// and the same release twice.
func TestReleasingWhatIsNotYours(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice, carol, stranger := testOwner(t, p), secondUser(t, p), secondUser(t, p)

	out, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", 0xd2), "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if err := p.ShareBook(ctx, out.BookFileID, alice, carol, true, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	for _, tc := range []struct {
		name string
		file uuid.UUID
		who  uuid.UUID
	}{
		{"a borrower", out.BookFileID, carol},
		{"a stranger", out.BookFileID, stranger},
		{"a file that does not exist", uuid.New(), alice},
		{"nobody", out.BookFileID, uuid.Nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.ReleaseHolding(ctx, tc.file, tc.who); !isNotFound(err) {
				t.Fatalf("ReleaseHolding = %v, want not found", err)
			}
		})
	}
	if !canRead(t, p, out.BookFileID, alice) || !canRead(t, p, out.BookFileID, carol) {
		t.Fatal("a refused release changed something")
	}

	if _, err := p.ReleaseHolding(ctx, out.BookFileID, alice); err != nil {
		t.Fatalf("ReleaseHolding: %v", err)
	}
	if _, err := p.ReleaseHolding(ctx, out.BookFileID, alice); !isNotFound(err) {
		t.Fatalf("releasing twice = %v, want not found", err)
	}
}

// The last holder letting go while somebody uploads the same bytes. Whichever
// commits first, the outcome is one of the two consistent ones: archived with
// nobody holding it, or held by the uploader and not archived - never a book
// somebody holds that is archived and invisible to them, nor one nobody holds
// that is not in the archive.
func TestReleasingRacesAnUpload(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	alice := testOwner(t, p)
	bob := secondUser(t, p)

	for i := range 20 {
		cleanLibrary(t, p)
		hash := fmt.Sprintf("%064x", 0xd300+i)
		out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}

		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			_, err := p.ReleaseHolding(ctx, out.BookFileID, alice)
			errs <- err
		}()
		go func() {
			<-start
			_, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: bob})
			errs <- err
		}()
		close(start)
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
		}

		var archived bool
		if err := p.QueryRow(ctx, `SELECT archived_at IS NOT NULL FROM book_files WHERE id = $1`,
			out.BookFileID).Scan(&archived); err != nil {
			t.Fatalf("reading: %v", err)
		}
		got := holders(t, p, out.BookFileID)
		if _, ok := got[alice]; ok {
			t.Fatalf("round %d: alice still holds a book she let go", i)
		}
		_, bobHolds := got[bob]
		if !bobHolds {
			t.Fatalf("round %d: bob's upload was lost (holders %v, archived %v)", i, got, archived)
		}
		if archived {
			t.Fatalf("round %d: bob holds a book that is archived and invisible to him", i)
		}
	}
}

// Review item 1. Holding one copy of a book is not holding the book: somebody
// lent an EPUB who uploads the same bytes may not retitle the work, or merge
// it - with a PDF of it nobody lent them - into a work of their own.
func TestHoldingOneCopyDoesNotMakeTheBookYours(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice, bob := testOwner(t, p), secondUser(t, p)

	epub := uploadInput(fmt.Sprintf("%064x", 0xe1), "a-book-with-two-files.epub", alice)
	out, err := p.UpsertBookFile(ctx, epub)
	if err != nil {
		t.Fatalf("epub: %v", err)
	}
	pdf := uploadInput(fmt.Sprintf("%064x", 0xe2), "a-book-with-two-files.pdf", alice)
	pdf.Format, pdf.Result.Format = domain.FormatPDF, domain.FormatPDF
	pout, err := p.UpsertBookFile(ctx, pdf)
	if err != nil {
		t.Fatalf("pdf: %v", err)
	}
	if pout.WorkID != out.WorkID {
		t.Fatal("alice's two formats did not make one work")
	}
	alicesWork := out.WorkID

	// Lent the EPUB, Bob downloads it and uploads it back: he holds it.
	if err := p.ShareBook(ctx, out.BookFileID, alice, bob, true, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: epub.ContentHash, UserID: bob}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	bobs, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", 0xe3), "bobs-own.epub", bob))
	if err != nil {
		t.Fatalf("bob's own: %v", err)
	}

	if _, err := p.MergeWorks(ctx, bob, bobs.WorkID, []uuid.UUID{alicesWork}); !isNotFound(err) {
		t.Fatalf("bob merged alice's book into his: %v", err)
	}
	// Nor is it offered to him as a duplicate of his own, same title or not.
	groups, err := p.Duplicates(ctx, bob)
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	for _, g := range groups {
		for _, w := range g.Works {
			if w.WorkID == alicesWork {
				t.Fatal("alice's book is offered to bob to merge")
			}
		}
	}
	title := "Bob Title"
	if _, err := p.UpdateWork(ctx, alicesWork, bob, WorkEdit{Title: &title}); !isNotFound(err) {
		t.Fatalf("bob retitled alice's book: %v", err)
	}
	d, err := p.Work(ctx, alicesWork, alice)
	if err != nil || d.Work.Title == "Bob Title" || len(d.Files) != 2 {
		t.Fatalf("alice's book changed: %+v, %v", d.Work.Title, err)
	}

	// Alice, who holds every file, still may.
	mine := "Alice's Title"
	if _, err := p.UpdateWork(ctx, alicesWork, alice, WorkEdit{Title: &mine}); err != nil {
		t.Fatalf("alice cannot edit her own book: %v", err)
	}
	// And two people holding exactly the same single file share the book.
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: fmt.Sprintf("%064x", 0xe3), UserID: alice}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	both := "Shared Title"
	if _, err := p.UpdateWork(ctx, bobs.WorkID, alice, WorkEdit{Title: &both}); err != nil {
		t.Errorf("a co-holder of every file cannot edit: %v", err)
	}
}

// Review item 2. Whether anybody holds a book with a given ISBN is nobody
// else's business: uploading or editing to that ISBN succeeds either way, and
// the uploader's edition is their own.
func TestAnISBNSaysNothingAboutWhoHasIt(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice, mallory := testOwner(t, p), secondUser(t, p)
	const isbn = "9780131103627"

	first := uploadInput(fmt.Sprintf("%064x", 0xe4), "alice.epub", alice)
	first.Result.Metadata.ISBN13 = isbn
	if _, err := p.UpsertBookFile(ctx, first); err != nil {
		t.Fatalf("alice: %v", err)
	}

	second := uploadInput(fmt.Sprintf("%064x", 0xe5), "mallory.epub", mallory)
	second.Result.Metadata.ISBN13 = isbn
	out, err := p.UpsertBookFile(ctx, second)
	if err != nil {
		t.Fatalf("mallory: %v", err)
	}
	d, err := p.Work(ctx, out.WorkID, mallory)
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(d.Work.Editions) != 1 || d.Work.Editions[0].ISBN13 != isbn {
		t.Errorf("mallory's edition = %+v, want her own carrying the ISBN", d.Work.Editions)
	}

	// Editing a book of her own to that ISBN is not refused either.
	third := uploadInput(fmt.Sprintf("%064x", 0xe6), "another.epub", mallory)
	third.Result.Metadata.Title = "Another"
	tout, err := p.UpsertBookFile(ctx, third)
	if err != nil {
		t.Fatalf("third: %v", err)
	}
	isbnEdit := isbn
	if _, err := p.UpdateWork(ctx, tout.WorkID, mallory, WorkEdit{ISBN13: &isbnEdit}); err != nil {
		t.Errorf("setting an ISBN somebody else's book carries was refused: %v", err)
	}
}

// Review item 3. Account deletion and an extraction turning that person's
// claim into a holding, at the same moment, must not deadlock.
func TestDeletingAnAccountWhileItsUploadIsExtracted(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	admin := testOwner(t, p)

	// Many people waiting on the same bytes, so the drain spends a while
	// between locking the claims and taking each claimant's account - the
	// window in which the deletion, taken the other way round, deadlocked.
	var crowd []uuid.UUID
	for range 60 {
		crowd = append(crowd, secondUser(t, p))
	}
	for i := range 40 {
		cleanLibrary(t, p)
		leaver, other := secondUser(t, p), secondUser(t, p)
		hash := fmt.Sprintf("%064x", 0xe700+i)
		for _, who := range append(append([]uuid.UUID{other}, crowd...), leaver) {
			if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: who}); err != nil {
				t.Fatalf("HoldUpload: %v", err)
			}
		}

		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			_, err := p.DeleteAccount(ctx, admin, leaver)
			errs <- err
		}()
		go func() {
			<-start
			_, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", other))
			errs <- err
		}()
		close(start)
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatalf("round %d: %v", i, err)
			}
		}
	}
}

// Review item 4b. The person an upload was for deleted their account before it
// was extracted: the file is archived, not left on nobody's shelf.
func TestAnUploadWhoseUploaderLeftIsArchived(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, leaver := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xe8)

	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: leaver}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if _, err := p.DeleteAccount(ctx, admin, leaver); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "left.epub", leaver))
	if err != nil {
		t.Fatalf("extracting: %v", err)
	}
	waiting, err := p.ArchivedBooks(ctx, admin)
	if err != nil {
		t.Fatalf("ArchivedBooks: %v", err)
	}
	if len(waiting) != 1 || waiting[0].BookFileID != out.BookFileID {
		t.Fatalf("archive = %+v, want the orphaned upload", waiting)
	}
}

// Review item 4c. A scan an administrator started before their account was
// deleted still queues its work; nobody in particular asked for it any more.
func TestAJobForSomebodyWhoLeftIsStillQueued(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	admin, leaver := testOwner(t, p), secondUser(t, p)
	if _, err := p.DeleteAccount(ctx, admin, leaver); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	target := uuid.New()
	id, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobExtract, TargetID: &target, RequestedBy: leaver})
	if err != nil {
		t.Fatalf("Enqueue for a deleted requester: %v", err)
	}
	t.Cleanup(func() { _, _ = p.Exec(ctx, `DELETE FROM jobs WHERE id = $1`, id) })
	if _, err := p.GetJobFor(ctx, id, admin); err != nil {
		t.Errorf("an administrator cannot see it: %v", err)
	}
}

// Review item 5. An administrator who took a book off their shelf does not get
// it back from their next scan - whether or not somebody else still holds it.
func TestAScanDoesNotUndoLettingGo(t *testing.T) {
	for _, tc := range []struct {
		name     string
		coHolder bool
	}{
		{"another holder remains", true},
		{"they were the last", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPool(t)
			cleanLibrary(t, p)
			ctx := t.Context()
			admin, bob := testOwner(t, p), secondUser(t, p)
			hash := fmt.Sprintf("%064x", 0xe9)

			out, err := p.UpsertBookFile(ctx, owned(sampleInput(hash, "/library/x.epub", "X", "A"), admin))
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if tc.coHolder {
				if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: bob}); err != nil {
					t.Fatalf("HoldUpload: %v", err)
				}
			}
			if _, err := p.ReleaseHolding(ctx, out.BookFileID, admin); err != nil {
				t.Fatalf("ReleaseHolding: %v", err)
			}
			if err := p.TouchSeen(ctx, hash, "/library/x.epub", admin); err != nil {
				t.Fatalf("TouchSeen: %v", err)
			}
			if _, ok := holders(t, p, out.BookFileID)[admin]; ok {
				t.Error("a rescan put back a book its administrator let go")
			}
		})
	}
}

// Review nit. An uploader's name for a scanned file does not become the
// file's own name, which merging, the enrichment prompt and the archive read.
func TestAnUploadDoesNotNameAScannedFile(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, bob := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xea)

	out, err := p.UpsertBookFile(ctx, owned(sampleInput(hash, "/library/x.epub", "X", "A"), admin))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "bobs-name.epub", bob)); err != nil {
		t.Fatalf("upload: %v", err)
	}
	var name string
	if err := p.QueryRow(ctx, `SELECT original_filename FROM book_files WHERE id = $1`,
		out.BookFileID).Scan(&name); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if name != "" {
		t.Errorf("the scanned file took the uploader's name %q", name)
	}
}

// Taking a book off your shelf while your own upload of it is still being
// extracted: the extraction runs on - the file may be somebody else's too - but
// finishing does not put the book back on your shelf.
func TestLettingGoOutlivesYourOwnExtraction(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice, bob := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xeb)

	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: bob}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if _, err := p.ReleaseHolding(ctx, out.BookFileID, alice); err != nil {
		t.Fatalf("ReleaseHolding: %v", err)
	}
	// Her queued extraction of it finishes now.
	if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice)); err != nil {
		t.Fatalf("the extraction finishing: %v", err)
	}
	got := holders(t, p, out.BookFileID)
	if _, ok := got[alice]; ok {
		t.Error("an extraction queued before alice let the book go put it back")
	}
	if _, ok := got[bob]; !ok {
		t.Error("bob lost the book")
	}

	// Uploading it again is changing her mind.
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: alice}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if _, ok := holders(t, p, out.BookFileID)[alice]; !ok {
		t.Error("alice's deliberate re-upload did not bring the book back")
	}
}

// A scan respects a release - and only a deliberate act forgets one. An
// administrator who let a book go does not get it back from their next scan;
// one who uploads it again, or is handed it, does - and then a scan holds it
// for them again as it should.
func TestAScanRespectsARelease(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, bob := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xec)

	// Bob uploaded it first; the administrator's directory has it too.
	if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "b.epub", bob)); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	var id uuid.UUID
	if err := p.QueryRow(ctx, `SELECT id FROM book_files WHERE content_hash = $1`, hash).Scan(&id); err != nil {
		t.Fatalf("reading: %v", err)
	}
	scan := func() {
		t.Helper()
		if err := p.TouchSeen(ctx, hash, "/library/x.epub", admin); err != nil {
			t.Fatalf("TouchSeen: %v", err)
		}
	}
	holds := func() bool { _, ok := holders(t, p, id)[admin]; return ok }

	scan()
	if !holds() {
		t.Fatal("a scan did not put the administrator's own file on their shelf")
	}
	if _, err := p.ReleaseHolding(ctx, id, admin); err != nil {
		t.Fatalf("ReleaseHolding: %v", err)
	}
	scan()
	if holds() {
		t.Fatal("a rescan undid letting the book go")
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: admin}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if !holds() {
		t.Fatal("uploading it again did not bring it back")
	}
	if _, err := p.ReleaseHolding(ctx, id, admin); err != nil {
		t.Fatalf("ReleaseHolding: %v", err)
	}
	scan()
	if holds() {
		t.Fatal("a rescan undid letting the book go a second time")
	}
}

// Review item 3. A scanned PDF does not join an uploader's EPUB of the same
// book, by ISBN or by filename: the result would be a work nobody holds every
// file of, which nobody may edit or merge.
func TestAScanJoinsOnlyTheScannersOwnBooks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		byISBN bool
	}{
		{"by ISBN", true},
		{"by filename", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPool(t)
			cleanLibrary(t, p)
			ctx := t.Context()
			admin, x := testOwner(t, p), secondUser(t, p)
			const isbn = "9780441013593"

			epub := uploadInput(fmt.Sprintf("%064x", 0xed), "dune-by-frank-herbert.epub", x)
			pdf := owned(sampleInput(fmt.Sprintf("%064x", 0xee), "/library/scanned-copy.pdf", "Dune", "Frank Herbert"), admin)
			pdf.Format, pdf.Result.Format = domain.FormatPDF, domain.FormatPDF
			if tc.byISBN {
				epub.Result.Metadata.ISBN13, pdf.Result.Metadata.ISBN13 = isbn, isbn
			} else {
				pdf.Path = "/library/dune-by-frank-herbert.pdf"
			}
			eout, err := p.UpsertBookFile(ctx, epub)
			if err != nil {
				t.Fatalf("upload: %v", err)
			}
			pout, err := p.UpsertBookFile(ctx, pdf)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if pout.WorkID == eout.WorkID {
				t.Fatal("the scan joined somebody else's book")
			}
			for who, work := range map[uuid.UUID]uuid.UUID{x: eout.WorkID, admin: pout.WorkID} {
				title := "Edited"
				if _, err := p.UpdateWork(ctx, work, who, WorkEdit{Title: &title}); err != nil {
					t.Errorf("a book's only holder cannot edit it: %v", err)
				}
			}
		})
	}
}

// waitForLockWait polls until a session running one of the given statements
// is waiting on a lock, so a test can drive one interleaving step by step
// instead of hoping for it. Matching the statement keeps it from being
// satisfied by an unrelated wait - another test package queueing for the
// shared test lock, say.
func waitForLockWait(t *testing.T, p *Pool, statements ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := p.QueryRow(t.Context(), `
			SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'
			   AND pid <> pg_backend_pid()
			   AND query LIKE ANY ($1)`, likeAny(statements)).Scan(&waiting); err != nil {
			t.Fatalf("reading lock waits: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("nothing running %q ever waited on a lock", statements)
}

func likeAny(fragments []string) []string {
	out := make([]string, len(fragments))
	for i, f := range fragments {
		out[i] = "%" + f + "%"
	}
	return out
}

// What DeleteAccount's statements contain: its account lock, its archive
// step's file locks, and its delete that cascades.
var accountDeletion = []string{"FROM users WHERE id = $1 FOR UPDATE", "book_file_holding:", "DELETE FROM users WHERE id"}

// The exact interleaving that deadlocked: an ingest has made its owner a
// holder (a key-share lock on their account) and is about to drain the claims
// on the same bytes - one of them that same person's - when their account is
// deleted. Every path must take the account row before the claims; with
// deletion taking the claims first, the two waited on each other.
func TestAccountDeletionAndADrainTakeLocksInOneOrder(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, x := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xf1)
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: x}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}

	reached, proceed := make(chan struct{}), make(chan struct{})
	betweenHoldingAndClaims = func() {
		close(reached)
		<-proceed
	}
	t.Cleanup(func() { betweenHoldingAndClaims = nil })

	ingest := make(chan error, 1)
	go func() {
		_, err := p.UpsertBookFile(ctx, uploadInput(hash, "x.epub", x))
		ingest <- err
	}()
	<-reached
	betweenHoldingAndClaims = nil

	deletion := make(chan error, 1)
	go func() {
		_, err := p.DeleteAccount(ctx, admin, x)
		deletion <- err
	}()
	// The deletion is now blocked on the account the ingest holds.
	waitForLockWait(t, p, accountDeletion...)
	close(proceed)

	if err := <-ingest; err != nil {
		t.Errorf("ingest: %v", err)
	}
	if err := <-deletion; err != nil {
		t.Errorf("account deletion: %v", err)
	}
}

// raceDeletion runs step in a goroutine until it reaches a hook, deletes the
// account in another while the first is parked, waits for the deletion to be
// blocked on a lock, lets the first go on, and reports both results. hook is
// the package's hook variable for that step.
func raceDeletion(t *testing.T, p *Pool, admin, x uuid.UUID, hook *func(), step func() error) {
	t.Helper()
	reached, proceed := make(chan struct{}), make(chan struct{})
	*hook = func() {
		*hook = nil
		close(reached)
		<-proceed
	}
	t.Cleanup(func() { *hook = nil })

	first := make(chan error, 1)
	go func() { first <- step() }()
	<-reached

	deletion := make(chan error, 1)
	go func() {
		_, err := p.DeleteAccount(t.Context(), admin, x)
		deletion <- err
	}()
	waitForLockWait(t, p, accountDeletion...)
	close(proceed)

	if err := <-first; err != nil {
		t.Errorf("the racing step: %v", err)
	}
	if err := <-deletion; err != nil {
		t.Errorf("account deletion: %v", err)
	}
}

// Letting a book go and deleting the same account, interleaved exactly: the
// release holds the file's lock when the deletion takes the account and asks
// for that lock. The release must already hold the account (key-share) by
// then, or remembering the release - a row whose foreign key needs the
// account - waits on the deletion while the deletion waits on it.
func TestReleasingAndDeletingTheAccountTakeLocksInOneOrder(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, x, y := testOwner(t, p), secondUser(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xf2)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "x.epub", x))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: y}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}

	raceDeletion(t, p, admin, x, &afterReleaseArchive, func() error {
		_, err := p.ReleaseHolding(ctx, out.BookFileID, x)
		return err
	})
}

// Uploading again - which forgets an earlier release - while the account is
// deleted, for a file that exists (a holding) and for bytes not extracted yet
// (a claim). The upload must take the account (key-share, by its new row's
// foreign key) before it touches the release row; the deletion's cascade
// reaches the release row only after taking the account.
func TestForgettingAReleaseAndDeletingTheAccountTakeLocksInOneOrder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fileExists bool
	}{
		{"a holding", true},
		{"a claim", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPool(t)
			cleanLibrary(t, p)
			ctx := t.Context()
			admin, x := testOwner(t, p), secondUser(t, p)
			hash := fmt.Sprintf("%064x", 0xf3)
			if tc.fileExists {
				if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", admin)); err != nil {
					t.Fatalf("seeding: %v", err)
				}
			}
			// X let these bytes go earlier.
			if _, err := p.Exec(ctx, `INSERT INTO released_holdings (user_id, content_hash) VALUES ($1, $2)`,
				x, hash); err != nil {
				t.Fatalf("remembering a release: %v", err)
			}

			raceDeletion(t, p, admin, x, &deliberateHoldStep, func() error {
				_, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: x})
				return err
			})
		})
	}
}

// The zero hold mode is not a mode. Defaulting to one would have made a
// forgotten argument the most permissive holding there is.
func TestTheZeroHoldModeIsRefused(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice := testOwner(t, p)
	out, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", 0xf4), "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // the test never commits
	var zero holdMode
	if _, err := addHolder(ctx, tx, out.BookFileID, secondUser(t, p), "", zero); err == nil {
		t.Fatal("addHolder accepted the zero hold mode")
	}
}

// A replaced copy is removed only when nothing points at it any more, checked
// under the file's lock: the record's path, or the archive's.
func TestRemovingAReplacedCopy(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	alice := testOwner(t, p)
	hash := fmt.Sprintf("%064x", 0xf5)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	var current string
	if err := p.QueryRow(ctx, `SELECT path FROM book_files WHERE id = $1`, out.BookFileID).Scan(&current); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE book_files SET archived_path = '/archive/a.epub' WHERE id = $1`,
		out.BookFileID); err != nil {
		t.Fatalf("setting: %v", err)
	}

	for _, tc := range []struct {
		path    string
		removed bool
	}{
		{current, false},
		{"/archive/a.epub", false},
		{"/archive/an-old-copy.epub", true},
	} {
		var removed []string
		if err := p.RemoveIfUnreferenced(ctx, hash, tc.path, func(path string) error {
			removed = append(removed, path)
			return nil
		}); err != nil {
			t.Fatalf("RemoveIfUnreferenced(%s): %v", tc.path, err)
		}
		if (len(removed) == 1) != tc.removed {
			t.Errorf("%s: removed = %v, want %v", tc.path, removed, tc.removed)
		}
	}
}
