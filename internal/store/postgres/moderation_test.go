// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// anotherAdmin is a second administrator, so one can be deleted by the other.
func anotherAdmin(t *testing.T, p *Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := p.QueryRow(t.Context(), `
		INSERT INTO users (username, is_admin) VALUES ($1, true) RETURNING id`,
		"admin-"+uuid.NewString()[:8]).Scan(&id); err != nil {
		t.Fatalf("creating an administrator: %v", err)
	}
	return id
}

// rowCount runs a count(*) query, for assertions.
func rowCount(t *testing.T, p *Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(t.Context(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("counting (%s): %v", sql, err)
	}
	return n
}

// chunkIt gives a file a passage with a vector, as indexing would.
func chunkIt(t *testing.T, p *Pool, bookFileID uuid.UUID) {
	t.Helper()
	ctx := t.Context()
	if _, err := p.SaveChunks(ctx, bookFileID, []ChunkInput{{
		Start:      domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1},
		End:        domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1, CharOffset: 40},
		Content:    "A passage nobody here may keep.",
		TokenCount: 7,
	}}, 1); err != nil {
		t.Fatalf("SaveChunks: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO embeddings (chunk_id, model_id, dimensions, vector)
		SELECT id, 'test-model', 768, array_fill(0.1::real, ARRAY[768])::vector
		  FROM chunks WHERE book_file_id = $1`, bookFileID); err != nil {
		t.Fatalf("embedding: %v", err)
	}
}

func sortedIDs(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	sort.Strings(out)
	return out
}

// A file several people hold, lend, annotate and are still uploading, removed
// by an administrator: it leaves every shelf, every grant and every share of
// notes on it end, its passages and vectors go, and its bytes are blocked. What
// each person wrote on it stays.
func TestRemovingAFileForEveryone(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin := testOwner(t, p)
	alice, bob, carol, dave, eve := secondUser(t, p), secondUser(t, p), secondUser(t, p), secondUser(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb1)

	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "alice.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	id := out.BookFileID
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: bob, OriginalFilename: "bob.epub"}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	// Carol's upload of the same bytes is still waiting for its extraction,
	// and Dave let the book go earlier.
	if _, err := p.Exec(ctx, `INSERT INTO holding_claims (content_hash, user_id, original_filename)
		VALUES ($1, $2, 'carol.epub')`, hash, carol); err != nil {
		t.Fatalf("claiming: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO released_holdings (user_id, content_hash) VALUES ($1, $2)`,
		dave, hash); err != nil {
		t.Fatalf("releasing: %v", err)
	}
	// Alice lends it to Eve, and shares her notes with Bob.
	if err := p.ShareBook(ctx, id, alice, eve, true, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	if err := p.ShareAnnotations(ctx, id, alice, bob); err != nil {
		t.Fatalf("ShareAnnotations: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO annotations (user_id, book_file_id, type, start_major, end_major, note)
		VALUES ($1, $2, 'note', 1, 1, 'what I thought of it')`, alice, id); err != nil {
		t.Fatalf("annotating: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO reading_progress (user_id, book_file_id, major, furthest_major) VALUES ($1, $2, 1, 1)`, bob, id); err != nil {
		t.Fatalf("reading: %v", err)
	}
	chunkIt(t, p, id)
	// Work still queued for it: chunking the file, and Carol's extraction.
	carolsTarget := uuid.New()
	for _, job := range []EnqueueOptions{
		{Type: domain.JobChunk, TargetID: &id, Payload: map[string]string{"bookFileId": id.String()}},
		{Type: domain.JobExtract, TargetID: &carolsTarget, Payload: map[string]string{"contentHash": hash}},
	} {
		if _, _, err := p.Enqueue(ctx, job); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	removedFor, err := p.RemoveBookFileForEveryone(ctx, id, admin, "  not ours to keep  ", RemovalOptions{})
	if err != nil {
		t.Fatalf("RemoveBookFileForEveryone: %v", err)
	}
	if got, want := sortedIDs(removedFor), sortedIDs([]uuid.UUID{alice, bob, carol}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("removed for %v, want the holders and the claimant %v", got, want)
	}

	if h := holders(t, p, id); len(h) != 0 {
		t.Errorf("holders after removal: %v", h)
	}
	for _, who := range []uuid.UUID{admin, alice, bob, carol, dave, eve} {
		if canRead(t, p, id, who) {
			t.Errorf("%s can still read a removed file", who)
		}
	}
	for what, n := range map[string]int{
		"grants":         rowCount(t, p, `SELECT count(*) FROM book_access WHERE book_file_id = $1`, id),
		"note shares":    rowCount(t, p, `SELECT count(*) FROM annotation_access WHERE book_file_id = $1`, id),
		"claims":         rowCount(t, p, `SELECT count(*) FROM holding_claims WHERE content_hash = $1`, hash),
		"releases":       rowCount(t, p, `SELECT count(*) FROM released_holdings WHERE content_hash = $1`, hash),
		"passages":       rowCount(t, p, `SELECT count(*) FROM chunks WHERE book_file_id = $1`, id),
		"vectors":        rowCount(t, p, `SELECT count(*) FROM embeddings`),
		"queued work":    rowCount(t, p, `SELECT count(*) FROM jobs WHERE status <> 'cancelled' AND target_id IN ($1, $2)`, id, carolsTarget),
		"live records":   rowCount(t, p, `SELECT count(*) FROM book_files WHERE id = $1 AND (archived_at IS NULL OR missing_since IS NULL)`, id),
		"blocks missing": 1 - rowCount(t, p, `SELECT count(*) FROM blocked_content WHERE content_hash = $1 AND reason = 'not ours to keep' AND blocked_by = $2`, hash, admin),
	} {
		if n != 0 {
			t.Errorf("%s left after removal: %d", what, n)
		}
	}
	// What people wrote is theirs, and stays.
	if n := rowCount(t, p, `SELECT count(*) FROM annotations WHERE book_file_id = $1`, id); n != 1 {
		t.Errorf("annotations after removal = %d, want Alice's note kept", n)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM reading_progress WHERE book_file_id = $1`, id); n != 1 {
		t.Errorf("reading positions after removal = %d, want Bob's kept", n)
	}

	// Not in the archive, and not to be handed on from it, while blocked.
	archived, err := p.ArchivedBooks(ctx, admin)
	if err != nil {
		t.Fatalf("ArchivedBooks: %v", err)
	}
	if len(archived) != 0 {
		t.Errorf("the archive lists a removed file: %+v", archived)
	}
	if err := p.ClaimArchived(ctx, admin, id, admin); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ClaimArchived of a removed file = %v, want not found", err)
	}

	// Removing it again changes nothing and keeps the first reason.
	again, err := p.RemoveBookFileForEveryone(ctx, id, admin, "a second reason", RemovalOptions{})
	if err != nil || len(again) != 0 {
		t.Errorf("removing again = %v, %v; want nobody, no error", again, err)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM blocked_content WHERE reason = 'not ours to keep'`); n != 1 {
		t.Errorf("the first reason was not kept")
	}
}

func TestRemovingIsForAdministratorsWithAReason(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, alice := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb2)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for _, tc := range []struct {
		name   string
		file   uuid.UUID
		actor  uuid.UUID
		reason string
		want   error
	}{
		{"a holder who is not an administrator", out.BookFileID, alice, "mine", store.ErrForbidden},
		{"no reason", out.BookFileID, admin, "   ", store.ErrInvalidQuery},
		{"a reason too long", out.BookFileID, admin, strings.Repeat("x", RemovalReasonMax+1), store.ErrInvalidQuery},
		{"no such file", uuid.New(), admin, "gone", store.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.RemoveBookFileForEveryone(ctx, tc.file, tc.actor, tc.reason, RemovalOptions{}); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if len(holders(t, p, out.BookFileID)) != 1 || rowCount(t, p, `SELECT count(*) FROM blocked_content`) != 0 {
		t.Error("a refused removal changed something")
	}
}

// Every way the same bytes could come back is refused while they are
// blocked: an upload, the extraction it queues, a scan finding them in the
// library directory, a chunking pass, the archive's move. None of them says
// anything about who else has the file.
func TestBlockedBytesDoNotComeBack(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, bob := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb3)
	scanned := sampleInput(hash, "/library/kept-by-its-owner.epub", "A Scanned Book", "An Author")
	scanned.OwnerID = admin
	out, err := p.UpsertBookFile(ctx, scanned)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "not to be kept", RemovalOptions{}); err != nil {
		t.Fatalf("removing: %v", err)
	}

	if blocked, err := p.IsBlocked(ctx, hash); err != nil || !blocked {
		t.Errorf("IsBlocked = %v, %v", blocked, err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: bob}); !errors.Is(err, store.ErrBlocked) {
		t.Errorf("HoldUpload = %v, want blocked", err)
	}
	if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "b.epub", bob)); !errors.Is(err, store.ErrBlocked) {
		t.Errorf("an upload's extraction = %v, want blocked", err)
	}
	if _, err := p.UpsertBookFile(ctx, scanned); !errors.Is(err, store.ErrBlocked) {
		t.Errorf("a scan's extraction = %v, want blocked", err)
	}
	if err := p.TouchSeen(ctx, hash, "/library/moved.epub", admin); !errors.Is(err, store.ErrBlocked) {
		t.Errorf("TouchSeen = %v, want blocked", err)
	}
	if n, err := p.SaveChunks(ctx, out.BookFileID, []ChunkInput{{
		Start: domain.Locator{Format: domain.FormatEPUB}, End: domain.Locator{Format: domain.FormatEPUB},
		Content: "back again", TokenCount: 2,
	}}, 1); err != nil || n != 0 {
		t.Errorf("SaveChunks = %d, %v; want nothing saved", n, err)
	}
	moved := false
	if to, err := p.MoveArchived(ctx, out.BookFileID, scanned.Path, func(string) (string, error) {
		moved = true
		return "/archive/x.epub", nil
	}); err != nil || to != "" || moved {
		t.Errorf("MoveArchived = %q, %v, moved %v; want nothing moved", to, err, moved)
	}

	if h := holders(t, p, out.BookFileID); len(h) != 0 {
		t.Errorf("holders = %v, want none", h)
	}
	for what, n := range map[string]int{
		"claims":   rowCount(t, p, `SELECT count(*) FROM holding_claims`),
		"passages": rowCount(t, p, `SELECT count(*) FROM chunks`),
		"present":  rowCount(t, p, `SELECT count(*) FROM book_files WHERE missing_since IS NULL`),
		"moved":    rowCount(t, p, `SELECT count(*) FROM book_files WHERE path <> $1`, scanned.Path),
	} {
		if n != 0 {
			t.Errorf("%s: %d, want 0", what, n)
		}
	}
}

// Lifting a block brings nothing back by itself. An upload's bytes were
// deleted, so its record stays out of the archive - there is nothing to hand
// on - and only the same bytes added again bring the book, and its marks,
// back.
func TestUnblocking(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, alice := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb4)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO annotations (user_id, book_file_id, type, start_major, end_major, note)
		VALUES ($1, $2, 'note', 1, 1, 'kept')`, alice, out.BookFileID); err != nil {
		t.Fatalf("annotating: %v", err)
	}
	if _, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "for now", RemovalOptions{}); err != nil {
		t.Fatalf("removing: %v", err)
	}

	if err := p.Unblock(ctx, alice, hash); !errors.Is(err, store.ErrForbidden) {
		t.Errorf("Unblock by a reader = %v, want forbidden", err)
	}
	if err := p.Unblock(ctx, admin, hash); err != nil {
		t.Fatalf("Unblock: %v", err)
	}
	if err := p.Unblock(ctx, admin, hash); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Unblock again = %v, want not found", err)
	}
	if canRead(t, p, out.BookFileID, alice) {
		t.Error("lifting the block put the book back on a shelf by itself")
	}
	archived, err := p.ArchivedBooks(ctx, admin)
	if err != nil || len(archived) != 0 {
		t.Errorf("the archive after unblocking = %+v, %v; want an upload with no bytes left out", archived, err)
	}
	if err := p.ClaimArchived(ctx, admin, out.BookFileID, admin); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("ClaimArchived of an upload with no bytes = %v, want not found", err)
	}

	// Alice adds the same bytes again: her book, and her note, are back. Its
	// copy was deleted with the removal, so it needs extracting again.
	hold, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: alice, OriginalFilename: "a.epub"})
	if err != nil || !hold.Missing {
		t.Fatalf("HoldUpload = %+v, %v; want held and its copy missing", hold, err)
	}
	if !canRead(t, p, out.BookFileID, alice) {
		t.Error("adding the bytes again did not bring the book back")
	}
	notes, err := p.Annotations(ctx, alice, out.BookFileID)
	if err != nil || len(notes) != 1 {
		t.Errorf("Alice's notes = %d, %v; want hers back", len(notes), err)
	}
}

// The copies of blocked bytes are handed over for deletion under the file's
// lock, and only while they are still blocked; the record then forgets the
// cover and the archive copy.
func TestDiscardingBlockedCopies(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, alice := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb5)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE book_files SET archived_path = '/archive/a.epub', cover_path = 'c.jpg'
		WHERE id = $1`, out.BookFileID); err != nil {
		t.Fatalf("setting: %v", err)
	}

	var given []StoredCopies
	record := func(c StoredCopies) error {
		given = append(given, c)
		return nil
	}
	if err := p.DiscardBlockedCopies(ctx, hash, record); err != nil || len(given) != 0 {
		t.Fatalf("discarding unblocked bytes = %v, handed %+v; want nothing handed over", err, given)
	}
	if _, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "gone", RemovalOptions{}); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if err := p.DiscardBlockedCopies(ctx, hash, record); err != nil {
		t.Fatalf("DiscardBlockedCopies: %v", err)
	}
	if len(given) != 1 || len(given[0].Paths) != 2 || given[0].Paths[1] != "/archive/a.epub" || given[0].Cover != "c.jpg" {
		t.Fatalf("handed %+v; want the record's path, its archive copy and its cover", given)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM book_files WHERE cover_path IS NOT NULL OR archived_path IS NOT NULL`); n != 0 {
		t.Errorf("the record still names a deleted cover or archive copy")
	}

	// A failed deletion changes nothing on record, so trying again sees it all.
	if _, err := p.Exec(ctx, `UPDATE book_files SET cover_path = 'c2.jpg' WHERE id = $1`, out.BookFileID); err != nil {
		t.Fatalf("setting: %v", err)
	}
	if err := p.DiscardBlockedCopies(ctx, hash, func(StoredCopies) error { return errors.New("disk") }); err == nil {
		t.Error("a failed deletion was not reported")
	}
	if n := rowCount(t, p, `SELECT count(*) FROM book_files WHERE cover_path = 'c2.jpg'`); n != 1 {
		t.Error("a failed deletion forgot the cover")
	}

	// Bytes with no record at all: the caller's own paths are all there is.
	other := fmt.Sprintf("%064x", 0xb6)
	if _, err := p.Exec(ctx, `INSERT INTO blocked_content (content_hash, reason) VALUES ($1, 'r')`, other); err != nil {
		t.Fatalf("blocking: %v", err)
	}
	given = nil
	if err := p.DiscardBlockedCopies(ctx, other, record); err != nil || len(given) != 1 || len(given[0].Paths) != 0 {
		t.Errorf("discarding bytes with no record = %v, %+v", err, given)
	}
}

func TestTheBlocklist(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner, admin, alice := testOwner(t, p), anotherAdmin(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xb7)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "the reason", RemovalOptions{}); err != nil {
		t.Fatalf("removing: %v", err)
	}

	if _, err := p.BlockedFiles(ctx, alice); !errors.Is(err, store.ErrForbidden) {
		t.Errorf("BlockedFiles by a reader = %v, want forbidden", err)
	}
	list, err := p.BlockedFiles(ctx, owner)
	if err != nil || len(list) != 1 {
		t.Fatalf("BlockedFiles = %+v, %v", list, err)
	}
	if got := list[0]; got.ContentHash != hash || got.Reason != "the reason" || got.Title == "" || got.BlockedBy == "" {
		t.Errorf("entry = %+v", got)
	}

	// The block outlives the administrator who made it.
	if _, err := p.DeleteAccount(ctx, owner, admin); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	list, err = p.BlockedFiles(ctx, owner)
	if err != nil || len(list) != 1 || list[0].BlockedBy != "" {
		t.Errorf("after the administrator left: %+v, %v", list, err)
	}
}

// Removal and deleting the account of the administrator removing it,
// interleaved exactly: the removal holds the file's lock when the deletion
// takes the account and asks for that lock. The removal must already hold the
// account (key-share) by then, or recording the block - a row whose foreign
// key needs the account - waits on the deletion while the deletion waits on
// it.
func TestRemovingAndDeletingTheAdministratorTakeLocksInOneOrder(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner, admin := testOwner(t, p), anotherAdmin(t, p)
	hash := fmt.Sprintf("%064x", 0xc1)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", admin))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	raceDeletion(t, p, owner, admin, &afterRemovalLock, func() error {
		_, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "racing", RemovalOptions{})
		return err
	})
	if n := rowCount(t, p, `SELECT count(*) FROM blocked_content WHERE content_hash = $1 AND blocked_by IS NULL`, hash); n != 1 {
		t.Error("the block did not outlive its administrator")
	}
}

// Removal and deleting a holder's account: the removal holds the file's lock
// while the deletion, holding the account, waits for it. Nothing the removal
// does needs that account.
func TestRemovingAndDeletingAHolderTakeLocksInOneOrder(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, x := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xc2)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "x.epub", x))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO released_holdings (user_id, content_hash) VALUES ($1, $2)`,
		x, fmt.Sprintf("%064x", 0xc3)); err != nil {
		t.Fatalf("releasing: %v", err)
	}

	raceDeletion(t, p, admin, x, &afterRemovalLock, func() error {
		_, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "racing", RemovalOptions{})
		return err
	})
	if n := rowCount(t, p, `SELECT count(*) FROM book_files WHERE archived_owner IS NOT NULL`); n != 0 {
		t.Error("the deletion archived a file the removal had already taken")
	}
}

// race parks first at hook, starts second, waits until second is blocked on
// a lock, lets first go on, and returns both results.
func race(t *testing.T, p *Pool, hook *func(), first, second func() error, secondWaitsOn ...string) (error, error) {
	t.Helper()
	reached, proceed := make(chan struct{}), make(chan struct{})
	*hook = func() {
		*hook = nil
		close(reached)
		<-proceed
	}
	t.Cleanup(func() { *hook = nil })

	a := make(chan error, 1)
	go func() { a <- first() }()
	<-reached
	b := make(chan error, 1)
	go func() { b <- second() }()
	waitForLockWait(t, p, secondWaitsOn...)
	close(proceed)
	return <-a, <-b
}

// Removal racing somebody letting the same book go, and somebody uploading
// it, in both orders. Whichever goes first, the end is the same: nobody holds
// it, nothing remembers a release or waits as a claim, and the archive does
// not move a copy the removal is deleting.
func TestRemovalRacesAReleaseAndAnUpload(t *testing.T) {
	const fileLock = "book_file_holding:"
	type rig struct {
		p     *Pool
		admin uuid.UUID
		x, y  uuid.UUID
		hash  string
		id    uuid.UUID
	}
	setup := func(t *testing.T, seed int) *rig {
		p := testPool(t)
		cleanLibrary(t, p)
		r := &rig{p: p, admin: testOwner(t, p), x: secondUser(t, p), y: secondUser(t, p),
			hash: fmt.Sprintf("%064x", seed)}
		out, err := p.UpsertBookFile(t.Context(), uploadInput(r.hash, "x.epub", r.x))
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}
		r.id = out.BookFileID
		return r
	}
	remove := func(t *testing.T, r *rig) func() error {
		return func() error {
			_, err := r.p.RemoveBookFileForEveryone(t.Context(), r.id, r.admin, "racing", RemovalOptions{})
			return err
		}
	}
	settled := func(t *testing.T, r *rig) {
		t.Helper()
		for what, n := range map[string]int{
			"holdings": rowCount(t, r.p, `SELECT count(*) FROM book_file_holders`),
			"claims":   rowCount(t, r.p, `SELECT count(*) FROM holding_claims`),
			"releases": rowCount(t, r.p, `SELECT count(*) FROM released_holdings`),
		} {
			if n != 0 {
				t.Errorf("%s left: %d", what, n)
			}
		}
		moved := false
		if _, err := r.p.MoveArchived(t.Context(), r.id, uploadInput(r.hash, "", r.x).Path, func(string) (string, error) {
			moved = true
			return "/archive/moved.epub", nil
		}); err != nil || moved {
			t.Errorf("the archive moved a removed file: %v", err)
		}
	}

	t.Run("a release holding the lock, then the removal", func(t *testing.T) {
		r := setup(t, 0xd1)
		release, removal := race(t, r.p, &afterReleaseArchive,
			func() error { _, err := r.p.ReleaseHolding(t.Context(), r.id, r.x); return err },
			remove(t, r), fileLock)
		if release != nil || removal != nil {
			t.Fatalf("release %v, removal %v", release, removal)
		}
		settled(t, r)
	})
	t.Run("the removal holding the lock, then a release", func(t *testing.T) {
		r := setup(t, 0xd2)
		removal, release := race(t, r.p, &afterRemovalLock, remove(t, r),
			func() error { _, err := r.p.ReleaseHolding(t.Context(), r.id, r.x); return err }, fileLock)
		if removal != nil || !errors.Is(release, store.ErrNotFound) {
			t.Fatalf("removal %v, release %v; want the release to find nothing to let go", removal, release)
		}
		settled(t, r)
	})
	t.Run("an upload holding the lock, then the removal", func(t *testing.T) {
		r := setup(t, 0xd3)
		upload, removal := race(t, r.p, &deliberateHoldStep,
			func() error {
				_, err := r.p.HoldUpload(t.Context(), HoldInput{ContentHash: r.hash, UserID: r.y})
				return err
			},
			remove(t, r), fileLock)
		if upload != nil || removal != nil {
			t.Fatalf("upload %v, removal %v", upload, removal)
		}
		settled(t, r)
	})
	t.Run("the removal holding the lock, then an upload", func(t *testing.T) {
		r := setup(t, 0xd4)
		removal, upload := race(t, r.p, &afterRemovalLock, remove(t, r),
			func() error {
				_, err := r.p.HoldUpload(t.Context(), HoldInput{ContentHash: r.hash, UserID: r.y})
				return err
			}, fileLock)
		if removal != nil || !errors.Is(upload, store.ErrBlocked) {
			t.Fatalf("removal %v, upload %v; want the upload refused", removal, upload)
		}
		settled(t, r)
	})
}

// A chunking pass that read the bytes just before a removal: whichever
// commits first, no passage of a removed file survives.
func TestRemovalRacesAChunkingPass(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, x := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xd5)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "x.epub", x))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// The chunking pass holds the file's row (shared) when the removal, holding
	// the file's lock, asks for it.
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM book_files WHERE id = $1 FOR SHARE`, out.BookFileID); err != nil {
		t.Fatalf("locking: %v", err)
	}
	removal := make(chan error, 1)
	go func() {
		_, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "racing", RemovalOptions{})
		removal <- err
	}()
	waitForLockWait(t, p, "FROM book_files WHERE id = $1 FOR UPDATE")
	if _, err := tx.Exec(ctx, `
		INSERT INTO chunks (book_file_id, seq, start_major, end_major, content, token_count, strategy_version)
		VALUES ($1, 0, 0, 0, 'a passage', 2, 1)`, out.BookFileID); err != nil {
		t.Fatalf("chunking: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := <-removal; err != nil {
		t.Fatalf("removal: %v", err)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM chunks`); n != 0 {
		t.Errorf("%d passages of a removed file survived", n)
	}
	// And afterwards, a pass finds the bytes blocked and saves nothing.
	if n, err := p.SaveChunks(ctx, out.BookFileID, []ChunkInput{{
		Start: domain.Locator{Format: domain.FormatEPUB}, End: domain.Locator{Format: domain.FormatEPUB},
		Content: "late", TokenCount: 1,
	}}, 1); err != nil || n != 0 || rowCount(t, p, `SELECT count(*) FROM chunks`) != 0 {
		t.Errorf("a late chunking pass saved %d, %v", n, err)
	}
}

// A file in the library directory was never deleted. After the block is
// lifted, the next scan that finds it marks it present, and the archive
// offers it again; the scan itself puts it on nobody's shelf.
func TestUnblockingAScannedFile(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin := testOwner(t, p)
	hash := fmt.Sprintf("%064x", 0xb8)
	scanned := sampleInput(hash, "/library/kept.epub", "Kept By Its Owner", "An Author")
	scanned.OwnerID = admin
	out, err := p.UpsertBookFile(ctx, scanned)
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "for now", RemovalOptions{}); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if err := p.Unblock(ctx, admin, hash); err != nil {
		t.Fatalf("Unblock: %v", err)
	}
	if err := p.TouchSeen(ctx, hash, scanned.Path, admin); err != nil {
		t.Fatalf("TouchSeen: %v", err)
	}
	if canRead(t, p, out.BookFileID, admin) {
		t.Error("a scan put an archived book back on a shelf by itself")
	}
	archived, err := p.ArchivedBooks(ctx, admin)
	if err != nil || len(archived) != 1 {
		t.Fatalf("the archive = %+v, %v; want the scanned record offered again", archived, err)
	}
	if err := p.ClaimArchived(ctx, admin, out.BookFileID, admin); err != nil || !canRead(t, p, out.BookFileID, admin) {
		t.Errorf("ClaimArchived = %v; want it on the shelf", err)
	}
}

// Purging quotes erases the file's words where they live on outside it - the
// text a highlight quotes, the excerpt a chat citation keeps - and keeps the
// marks, notes, citations and their places. Without it they stay.
func TestPurgingQuotes(t *testing.T) {
	for _, purge := range []bool{false, true} {
		t.Run(fmt.Sprintf("purge %v", purge), func(t *testing.T) {
			p := testPool(t)
			cleanLibrary(t, p)
			ctx := t.Context()
			admin, alice := testOwner(t, p), secondUser(t, p)
			hash := fmt.Sprintf("%064x", 0xb9)
			out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
			if err != nil {
				t.Fatalf("seeding: %v", err)
			}
			id := out.BookFileID
			if _, err := p.Exec(ctx, `
				INSERT INTO annotations (user_id, book_file_id, type, start_major, end_major, selected_text, note)
				VALUES ($1, $2, 'highlight', 1, 1, 'the quoted sentence', 'my thought')`, alice, id); err != nil {
				t.Fatalf("highlighting: %v", err)
			}
			chunkIt(t, p, id)
			var conv, msg uuid.UUID
			if err := p.QueryRow(ctx, `INSERT INTO conversations (user_id) VALUES ($1) RETURNING id`, alice).Scan(&conv); err != nil {
				t.Fatalf("conversation: %v", err)
			}
			if err := p.QueryRow(ctx, `INSERT INTO messages (conversation_id, seq, role, content)
				VALUES ($1, 0, 'assistant', 'an answer') RETURNING id`, conv).Scan(&msg); err != nil {
				t.Fatalf("message: %v", err)
			}
			if _, err := p.Exec(ctx, `
				INSERT INTO citations (message_id, chunk_id, book_file_id, start_major, excerpt)
				SELECT $1, c.id, c.book_file_id, c.start_major, left(c.content, 400) FROM chunks c WHERE c.book_file_id = $2`,
				msg, id); err != nil {
				t.Fatalf("citing: %v", err)
			}

			if _, err := p.RemoveBookFileForEveryone(ctx, id, admin, "gone", RemovalOptions{PurgeQuotes: purge}); err != nil {
				t.Fatalf("removing: %v", err)
			}

			var quote, note string
			if err := p.QueryRow(ctx, `SELECT selected_text, note FROM annotations WHERE book_file_id = $1`, id).Scan(&quote, &note); err != nil {
				t.Fatalf("reading the highlight: %v", err)
			}
			var excerpt *string
			var start int
			if err := p.QueryRow(ctx, `SELECT excerpt, start_major FROM citations WHERE book_file_id = $1`, id).Scan(&excerpt, &start); err != nil {
				t.Fatalf("reading the citation: %v", err)
			}
			if note != "my thought" || start != 1 {
				t.Errorf("note %q, citation place %d; want both kept", note, start)
			}
			if purge && (quote != "" || excerpt != nil) {
				t.Errorf("quote %q, excerpt %v; want both erased", quote, excerpt)
			}
			if !purge && (quote != "the quoted sentence" || excerpt == nil || *excerpt == "") {
				t.Errorf("quote %q, excerpt %v; want both kept", quote, excerpt)
			}
		})
	}
}

// A removal touches people who hold nothing of the file - somebody who let it
// go but whose extraction of it is still queued, say - and their deletion
// cascades into the same rows in its own order. The removal holds their
// account first, so the deletion waits there, holding nothing.
func TestRemovingAndDeletingSomebodyWhoLetItGoTakeLocksInOneOrder(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, alice, x := testOwner(t, p), secondUser(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xc4)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO released_holdings (user_id, content_hash) VALUES ($1, $2)`, x, hash); err != nil {
		t.Fatalf("releasing: %v", err)
	}
	target := uuid.New()
	if _, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobExtract, TargetID: &target,
		Payload: map[string]string{"contentHash": hash}, RequestedBy: x}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	raceDeletion(t, p, admin, x, &afterRemovalLock, func() error {
		_, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "racing", RemovalOptions{})
		return err
	})
}

// Somebody new coming to have something of the file while a removal locks
// accounts - lent it, here - starts the removal again, with them included.
func TestARemovalStartsAgainForSomebodyNew(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, alice, bob := testOwner(t, p), secondUser(t, p), secondUser(t, p)
	out, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", 0xc5), "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	tries := 0
	afterRemovalLock = func() {
		tries++
		if tries == 1 {
			if err := p.ShareBook(ctx, out.BookFileID, alice, bob, false, false); err != nil {
				t.Errorf("ShareBook: %v", err)
			}
		}
	}
	t.Cleanup(func() { afterRemovalLock = nil })

	if _, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "lent meanwhile", RemovalOptions{}); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if tries != 2 {
		t.Errorf("the removal ran %d times, want twice", tries)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM book_access WHERE book_file_id = $1`, out.BookFileID); n != 0 {
		t.Errorf("%d grants survived", n)
	}
}

// Copies remain until a deletion has succeeded, and a failed one says so.
func TestTheBlocklistSaysWhetherCopiesRemain(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, alice := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xc6)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "gone", RemovalOptions{}); err != nil {
		t.Fatalf("removing: %v", err)
	}
	remain := func() bool {
		t.Helper()
		list, err := p.BlockedFiles(ctx, admin)
		if err != nil || len(list) != 1 {
			t.Fatalf("BlockedFiles = %+v, %v", list, err)
		}
		return list[0].CopiesRemain
	}
	pending := func() []string {
		t.Helper()
		h, err := p.BlocksWithCopies(ctx)
		if err != nil {
			t.Fatalf("BlocksWithCopies: %v", err)
		}
		return h
	}
	if !remain() || len(pending()) != 1 {
		t.Error("copies were not reported as remaining before any deletion")
	}
	if err := p.DiscardBlockedCopies(ctx, hash, func(StoredCopies) error { return nil }); err != nil {
		t.Fatalf("DiscardBlockedCopies: %v", err)
	}
	if remain() || len(pending()) != 0 {
		t.Error("copies reported as remaining after a deletion succeeded")
	}
	// Removing again asks for the clean-up again, and until it has run the
	// blocklist - and the next startup - say copies may remain.
	if _, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "again", RemovalOptions{}); err != nil {
		t.Fatalf("removing again: %v", err)
	}
	if !remain() || len(pending()) != 1 {
		t.Error("removing again did not ask for the clean-up again")
	}
	if err := p.DiscardBlockedCopies(ctx, hash, func(StoredCopies) error { return nil }); err != nil {
		t.Fatalf("DiscardBlockedCopies: %v", err)
	}
	if err := p.DiscardBlockedUpload(ctx, hash, func() error { return errors.New("disk") }); err == nil {
		t.Error("a failed deletion of a refused upload was not reported")
	}
	if !remain() {
		t.Error("a refused upload that could not be deleted was not reported as remaining")
	}
	if err := p.DiscardBlockedCopies(ctx, hash, func(StoredCopies) error { return errors.New("disk") }); err == nil || !remain() {
		t.Errorf("a failed deletion = %v; want reported, and copies remaining", err)
	}
}

// Somebody new appearing on every try is not waited out for ever: the
// removal stops, changing nothing, and says to try again.
func TestARemovalThatKeepsMeetingSomebodyNewStops(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, alice := testOwner(t, p), secondUser(t, p)
	out, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", 0xc7), "a.epub", alice))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	afterRemovalLock = func() {
		if err := p.ShareBook(ctx, out.BookFileID, alice, secondUser(t, p), false, false); err != nil {
			t.Errorf("ShareBook: %v", err)
		}
	}
	t.Cleanup(func() { afterRemovalLock = nil })

	if _, err := p.RemoveBookFileForEveryone(ctx, out.BookFileID, admin, "busy", RemovalOptions{}); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("err = %v, want busy", err)
	}
	if rowCount(t, p, `SELECT count(*) FROM blocked_content`) != 0 || len(holders(t, p, out.BookFileID)) != 1 {
		t.Error("a removal that stopped changed something")
	}
}
