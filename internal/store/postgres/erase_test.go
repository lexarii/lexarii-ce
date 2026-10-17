// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// discardLog records what an erasure asked to have deleted from disk.
type discardLog struct {
	calls []StoredCopies
	err   error
}

func (d *discardLog) discard(c StoredCopies) error {
	d.calls = append(d.calls, c)
	return d.err
}

func (d *discardLog) hashes() []string {
	out := make([]string, 0, len(d.calls))
	for _, c := range d.calls {
		out = append(out, c.ContentHash)
	}
	return out
}

func hasKey(m map[uuid.UUID]string, k uuid.UUID) bool {
	_, ok := m[k]
	return ok
}

func owedHashes(t *testing.T, p *Pool) []string {
	t.Helper()
	rows, err := p.Query(t.Context(), `SELECT content_hash FROM pending_discards ORDER BY content_hash`)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	return out
}

func fileExists(t *testing.T, p *Pool, id uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := p.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM book_files WHERE id = $1)`, id).Scan(&ok); err != nil {
		t.Fatalf("reading: %v", err)
	}
	return ok
}

func userExists(t *testing.T, p *Pool, id uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := p.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, id).Scan(&ok); err != nil {
		t.Fatalf("reading: %v", err)
	}
	return ok
}

// Deleting an account with erasing on: a book only they held goes, with what
// was derived from it and what was written on it; a book somebody else holds
// just loses a holder; a book somebody else's upload is still waiting for is
// kept, archived with no name.
func TestDeletingAnAccountErasesWhatOnlyTheyHeld(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, leaver, friend, waiter := testOwner(t, p), secondUser(t, p), secondUser(t, p), secondUser(t, p)

	soleHash, sharedHash, awaitedHash := fmt.Sprintf("%064x", 0xe1), fmt.Sprintf("%064x", 0xe2), fmt.Sprintf("%064x", 0xe3)
	sole, err := p.UpsertBookFile(ctx, uploadInput(soleHash, "sole.epub", leaver))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	shared, err := p.UpsertBookFile(ctx, uploadInput(sharedHash, "shared.epub", leaver))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: sharedHash, UserID: friend}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	awaited, err := p.UpsertBookFile(ctx, uploadInput(awaitedHash, "awaited.epub", leaver))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	// Somebody else's upload of the same bytes, not extracted yet: a claim.
	if _, err := p.Exec(ctx, `INSERT INTO holding_claims (content_hash, user_id) VALUES ($1, $2)`,
		awaitedHash, waiter); err != nil {
		t.Fatalf("claiming: %v", err)
	}

	// What hangs off the sole book: passages, a cover, a borrower's note,
	// a queued chunking.
	if _, err := p.SaveChunks(ctx, sole.BookFileID, []ChunkInput{{
		Seq: 0, Start: domain.NewEPUBLocator(0, "p[1]", 0), End: domain.NewEPUBLocator(0, "p[1]", 5),
		Content: "words", TokenCount: 1,
	}}, 1); err != nil {
		t.Fatalf("SaveChunks: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE book_files SET cover_path = 'sole-cover.jpg' WHERE id = $1`, sole.BookFileID); err != nil {
		t.Fatalf("cover: %v", err)
	}
	if err := p.ShareBook(ctx, sole.BookFileID, leaver, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	if _, err := p.CreateAnnotation(ctx, epubMark(friend, sole.BookFileID, 0, "p[1]", 0, 5)); err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	target := sole.BookFileID
	chunkJob, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobChunk, TargetID: &target})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var log discardLog
	erasure, err := p.DeleteAccountErasing(ctx, admin, leaver)
	if err != nil {
		t.Fatalf("DeleteAccountErasing: %v", err)
	}
	// Nothing was deleted from disk inside the transaction; what it owes is
	// recorded, and paid now.
	if len(log.calls) != 0 {
		t.Fatal("copies were deleted before the erasure committed")
	}
	if got := owedHashes(t, p); len(got) != 1 || got[0] != soleHash {
		t.Fatalf("owed = %v, want only the sole book's bytes", got)
	}
	if res, err := p.DiscardPending(ctx, erasure.Owed, log.discard); err != nil || res.Paid != 1 {
		t.Fatalf("DiscardPending = %+v, %v; want it paid", res, err)
	}
	if got := owedHashes(t, p); len(got) != 0 {
		t.Errorf("still owed after paying: %v", got)
	}

	if userExists(t, p, leaver) {
		t.Error("the account is still there")
	}
	if len(erasure.Erased) != 1 || erasure.Erased[0] != sole.BookFileID || erasure.Kept != 1 {
		t.Errorf("erasure = %+v, want the sole book erased and the awaited one kept", erasure)
	}

	// The sole book: record, passages, the borrower's note and access, gone.
	if fileExists(t, p, sole.BookFileID) {
		t.Error("the book only they held is still recorded")
	}
	var leftovers int
	if err := p.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM chunks WHERE book_file_id = $1)
		     + (SELECT count(*) FROM annotations WHERE book_file_id = $1)
		     + (SELECT count(*) FROM book_access WHERE book_file_id = $1)`, sole.BookFileID).Scan(&leftovers); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if leftovers != 0 {
		t.Errorf("%d rows derived from the erased book remain", leftovers)
	}
	var status string
	if err := p.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, chunkJob).Scan(&status); err != nil {
		t.Fatalf("reading the job: %v", err)
	}
	if status != "cancelled" {
		t.Errorf("chunking queued for the erased book is %s, want cancelled", status)
	}
	// Its stored copies were handed over for deletion: the record's path and
	// its cover, once, and nothing of the other books.
	if got := log.hashes(); len(got) != 1 || got[0] != soleHash {
		t.Fatalf("discarded %v, want only the sole book's bytes", got)
	}
	if c := log.calls[0]; len(c.Paths) != 1 || c.Paths[0] == "" || len(c.Covers) != 1 || c.Covers[0] != "sole-cover.jpg" {
		t.Errorf("copies = %+v, want its path and its cover", c)
	}

	// The shared book: the friend's, untouched.
	if got := holders(t, p, shared.BookFileID); len(got) != 1 || !hasKey(got, friend) {
		t.Errorf("holders of the shared book = %v, want only the friend", got)
	}
	if !canRead(t, p, shared.BookFileID, friend) {
		t.Error("the friend lost a book they hold")
	}

	// The awaited book: kept, archived, and with no name recorded.
	var archived bool
	var owner *string
	if err := p.QueryRow(ctx, `SELECT archived_at IS NOT NULL, archived_owner FROM book_files WHERE id = $1`,
		awaited.BookFileID).Scan(&archived, &owner); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !archived || owner != nil {
		t.Errorf("awaited book: archived %v, owner %v; want archived with no name", archived, owner)
	}
	// And the waiting upload still makes it theirs.
	if _, err := p.UpsertBookFile(ctx, uploadInput(awaitedHash, "mine.epub", waiter)); err != nil {
		t.Fatalf("the waiting extraction: %v", err)
	}
	if !canRead(t, p, awaited.BookFileID, waiter) {
		t.Error("the waiting upload did not reach its uploader's shelf")
	}

	// The editions it left empty go once the erasure has committed.
	if err := p.DeleteEmptyEditions(ctx, erasure.Editions); err != nil {
		t.Fatalf("DeleteEmptyEditions: %v", err)
	}
	var editions int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM editions WHERE id = ANY($1)`, erasure.Editions).Scan(&editions); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if editions != 0 {
		t.Errorf("%d editions with no file left survived", editions)
	}
}

// A disk that refuses does not undo an erasure, and does not lose track: the
// copies stay owed, the failure is recorded against them, and the next pass
// pays them.
func TestOwedCopiesSurviveAFailedDisk(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, leaver := testOwner(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xe4)
	out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", leaver))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.DeleteAccountErasing(ctx, admin, leaver); err != nil {
		t.Fatalf("DeleteAccountErasing: %v", err)
	}
	if userExists(t, p, leaver) || fileExists(t, p, out.BookFileID) {
		t.Fatal("the erasure did not happen")
	}

	disk := errors.New("disk refused")
	failing := discardLog{err: disk}
	res, err := p.DiscardPending(ctx, nil, failing.discard)
	if err != nil || res.Failed != 1 || res.Paid != 0 {
		t.Fatalf("DiscardPending on a failing disk = %+v, %v", res, err)
	}
	var attempts int
	var lastError string
	if err := p.QueryRow(ctx, `SELECT attempts, last_error FROM pending_discards WHERE content_hash = $1`,
		hash).Scan(&attempts, &lastError); err != nil {
		t.Fatalf("the debt was lost: %v", err)
	}
	if attempts != 1 || lastError == "" {
		t.Errorf("attempts %d, error %q; want the failure recorded", attempts, lastError)
	}

	var ok discardLog
	if res, err := p.DiscardPending(ctx, nil, ok.discard); err != nil || res.Paid != 1 || len(ok.calls) != 1 {
		t.Fatalf("the next pass = %+v, %v (calls %d); want it paid", res, err, len(ok.calls))
	}
	if got := owedHashes(t, p); len(got) != 0 {
		t.Errorf("still owed: %v", got)
	}
}

// An expired archive record owes its stored copies only on an installation
// that erases; otherwise the file is kept, as it always was.
func TestExpiringArchivesInEraseMode(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	for _, erase := range []bool{false, true} {
		cleanLibrary(t, p)
		admin, leaver := testOwner(t, p), secondUser(t, p)
		hash := fmt.Sprintf("%064x", 0xef)
		out, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", leaver))
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}
		if _, err := p.Exec(ctx, `UPDATE book_files SET cover_path = 'c.jpg' WHERE id = $1`, out.BookFileID); err != nil {
			t.Fatalf("cover: %v", err)
		}
		if _, err := p.DeleteAccount(ctx, admin, leaver); err != nil {
			t.Fatalf("DeleteAccount: %v", err)
		}
		if _, err := p.Exec(ctx, `UPDATE book_files SET archived_at = now() - interval '1 year' WHERE id = $1`,
			out.BookFileID); err != nil {
			t.Fatalf("ageing: %v", err)
		}
		paths, err := p.ExpireArchived(ctx, time.Now().Add(-time.Hour), erase)
		if err != nil || len(paths) != 1 {
			t.Fatalf("ExpireArchived(erase=%v) = %v, %v", erase, paths, err)
		}
		var paths2, covers []string
		err = p.QueryRow(ctx, `SELECT paths, covers FROM pending_discards WHERE content_hash = $1`, hash).Scan(&paths2, &covers)
		switch {
		case !erase && !isNotFound(mapError(err)):
			t.Errorf("archive mode owes the disk: %v, %v", paths2, err)
		case erase && (err != nil || len(paths2) != 1 || len(covers) != 1 || covers[0] != "c.jpg"):
			t.Errorf("erase mode owes %v %v (%v), want the path and the cover", paths2, covers, err)
		}
	}
}

// The same rules as deleting an account: only an administrator may delete
// somebody else, and not the last administrator.
func TestDeletingErasingKeepsTheRules(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, a, b := testOwner(t, p), secondUser(t, p), secondUser(t, p)

	if _, err := p.DeleteAccountErasing(ctx, a, b); !errors.Is(err, store.ErrForbidden) {
		t.Errorf("a reader deleting somebody else: %v, want ErrForbidden", err)
	}
	if _, err := p.DeleteAccountErasing(ctx, admin, uuid.Nil); !isNotFound(err) {
		t.Errorf("nobody: %v, want not found", err)
	}
	if _, err := p.DeleteAccountErasing(ctx, admin, uuid.New()); !isNotFound(err) {
		t.Errorf("an unknown account: %v, want not found", err)
	}
	var admins int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM users WHERE is_admin`).Scan(&admins); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if admins == 1 {
		if _, err := p.DeleteAccountErasing(ctx, admin, admin); !errors.Is(err, store.ErrInvalidQuery) {
			t.Errorf("the last administrator: %v, want ErrInvalidQuery", err)
		}
	}
	if _, err := p.DeleteAccountErasing(ctx, a, a); err != nil {
		t.Errorf("deleting yourself: %v", err)
	}
}

// Their own upload still waiting to be extracted: the extraction is
// cancelled and the bytes are owed, and paid only while nothing else wants
// them.
func TestErasingAnAccountWithAnUploadStillWaiting(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, leaver, other := testOwner(t, p), secondUser(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xe5)
	// Jobs outlive cleanLibrary; one left by an earlier run would want these
	// bytes.
	if _, err := p.Exec(ctx, `DELETE FROM jobs WHERE payload ->> 'contentHash' = $1`, hash); err != nil {
		t.Fatalf("clearing jobs: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: leaver}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	target := uuid.New()
	job, _, err := p.Enqueue(ctx, EnqueueOptions{
		Type: domain.JobExtract, TargetID: &target, RequestedBy: leaver,
		Payload: map[string]any{"contentHash": hash, "uploaded": true},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// A scan they queued is not theirs to take with them: its files are in
	// the library directory.
	scanTarget := uuid.New()
	scanJob, _, err := p.Enqueue(ctx, EnqueueOptions{
		Type: domain.JobExtract, TargetID: &scanTarget, RequestedBy: leaver,
		Payload: map[string]any{"contentHash": fmt.Sprintf("%064x", 0xec)},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	erasure, err := p.DeleteAccountErasing(ctx, admin, leaver)
	if err != nil {
		t.Fatalf("DeleteAccountErasing: %v", err)
	}
	if len(erasure.Owed) != 1 || erasure.Owed[0] != hash {
		t.Fatalf("owed = %v, want the upload that was waiting", erasure.Owed)
	}
	var status string
	if err := p.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, job).Scan(&status); err != nil {
		t.Fatalf("reading the job: %v", err)
	}
	if status != "cancelled" {
		t.Errorf("their extraction is %s, want cancelled", status)
	}
	if err := p.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, scanJob).Scan(&status); err != nil {
		t.Fatalf("reading the job: %v", err)
	}
	if status != "pending" {
		t.Errorf("a scan's extraction they queued is %s, want still pending", status)
	}
	if _, err := p.Exec(ctx, `UPDATE jobs SET status = 'cancelled' WHERE id = $1`, scanJob); err != nil {
		t.Fatalf("tidying: %v", err)
	}

	// Somebody else uploads the same bytes before the debt is paid: their
	// claim keeps the stored copy, and the debt waits.
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: other}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	var log discardLog
	if res, err := p.DiscardPending(ctx, nil, log.discard); err != nil || res.Waiting != 1 || len(log.calls) != 0 {
		t.Fatalf("with a claim = %+v, %v (calls %d); want it waiting", res, err, len(log.calls))
	}
	// Their extraction makes the bytes a record's: they are its holder's
	// now, and the debt is settled without touching them.
	if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "o.epub", other)); err != nil {
		t.Fatalf("their extraction: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE pending_discards SET covers = '{gone.jpg}' WHERE content_hash = $1`, hash); err != nil {
		t.Fatalf("owing a cover: %v", err)
	}
	if res, err := p.DiscardPending(ctx, nil, log.discard); err != nil || res.Paid != 1 || len(log.calls) != 1 {
		t.Fatalf("with a record = %+v, %v (calls %d); want settled", res, err, len(log.calls))
	}
	if c := log.calls[0]; c.ContentHash != "" || len(c.Paths) != 0 || len(c.Covers) != 1 {
		t.Errorf("with a record, discarded %+v; want only the erased record's cover", c)
	}
	log.calls = nil

	// Owed, unwanted bytes are deleted; blocked ones are left to the
	// blocklist, but their owed covers go.
	free, blocked := fmt.Sprintf("%064x", 0xf0), fmt.Sprintf("%064x", 0xf1)
	if _, err := p.Exec(ctx, `INSERT INTO pending_discards (content_hash, covers) VALUES ($1, '{x.jpg}'), ($2, '{b.jpg}')`,
		free, blocked); err != nil {
		t.Fatalf("owing: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO blocked_content (content_hash, reason, blocked_by) VALUES ($1, 'test', $2)`,
		blocked, admin); err != nil {
		t.Fatalf("blocking: %v", err)
	}
	if res, err := p.DiscardPending(ctx, nil, log.discard); err != nil || res.Paid != 2 {
		t.Fatalf("DiscardPending = %+v, %v", res, err)
	}
	var bytesGone, coversGone []string
	for _, c := range log.calls {
		if c.ContentHash != "" {
			bytesGone = append(bytesGone, c.ContentHash)
		}
		coversGone = append(coversGone, c.Covers...)
	}
	if len(bytesGone) != 1 || bytesGone[0] != free {
		t.Errorf("deleted the bytes of %v, want only the unwanted ones", bytesGone)
	}
	if len(coversGone) != 2 {
		t.Errorf("covers deleted = %v, want both owed covers", coversGone)
	}
}

// A claim wants its bytes while its upload's extraction is on its way - or
// for a moment after the claim is recorded, before the extraction is queued.
// A claim whose extraction failed for good, or was never queued, wants
// nothing, and does not hold a debt forever. While a debt waits, what no
// upload reads - covers, the archive's copies - goes; and a debt waiting
// over a day is reported.
func TestADebtWaitsOnlyForAnUploadOnItsWay(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	uploader := secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xf2)
	if _, err := p.Exec(ctx, `DELETE FROM jobs WHERE payload ->> 'contentHash' = $1`, hash); err != nil {
		t.Fatalf("clearing jobs: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: uploader}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if _, err := p.Exec(ctx, `
		INSERT INTO pending_discards (content_hash, paths, covers, queued_at)
		VALUES ($1, '{/data/archive/x.epub,/data/uploads/x.epub}', '{c.jpg}', now() - interval '2 days')`, hash); err != nil {
		t.Fatalf("owing: %v", err)
	}

	// A fresh claim: waiting, the archive copy and the cover go now, and the
	// day-old debt is reported.
	var log discardLog
	res, err := p.DiscardPending(ctx, nil, log.discard)
	if err != nil || res.Waiting != 1 || len(res.Stale) != 1 || res.Stale[0] != hash {
		t.Fatalf("fresh claim = %+v, %v; want waiting and reported stale", res, err)
	}
	if len(log.calls) != 1 || !log.calls[0].SkipUploads || len(log.calls[0].Covers) != 1 {
		t.Fatalf("while waiting, discarded %+v; want everything but the upload directory", log.calls)
	}
	log.calls = nil
	if res, err := p.DiscardPending(ctx, nil, log.discard); err != nil || res.Waiting != 1 || len(log.calls) != 1 ||
		len(log.calls[0].Covers) != 0 {
		t.Fatalf("again = %+v, %v (%+v); want the cover not deleted twice", res, err, log.calls)
	}
	log.calls = nil

	// The claim is old and no extraction is queued for it: it wants nothing.
	if _, err := p.Exec(ctx, `UPDATE holding_claims SET created_at = now() - interval '2 hours' WHERE content_hash = $1`,
		hash); err != nil {
		t.Fatalf("ageing: %v", err)
	}
	// ...unless its extraction is queued.
	target := uuid.New()
	job, _, err := p.Enqueue(ctx, EnqueueOptions{Type: domain.JobExtract, TargetID: &target, RequestedBy: uploader,
		Payload: map[string]any{"contentHash": hash, "uploaded": true}})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if res, err := p.DiscardPending(ctx, nil, log.discard); err != nil || res.Waiting != 1 {
		t.Fatalf("with its extraction queued = %+v, %v; want waiting", res, err)
	}
	if _, err := p.Exec(ctx, `UPDATE jobs SET status = 'failed' WHERE id = $1`, job); err != nil {
		t.Fatalf("failing the job: %v", err)
	}
	log.calls = nil
	if res, err := p.DiscardPending(ctx, nil, log.discard); err != nil || res.Paid != 1 || len(log.calls) != 1 ||
		log.calls[0].SkipUploads {
		t.Fatalf("with its extraction failed for good = %+v, %v (%+v); want it paid in full", res, err, log.calls)
	}
}

// Erasing somebody's books without deleting them: administrators only; books
// they share stay; the release is remembered.
func TestErasingSomebodysBooksNow(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, reader, friend := testOwner(t, p), secondUser(t, p), secondUser(t, p)
	soleHash, sharedHash := fmt.Sprintf("%064x", 0xe6), fmt.Sprintf("%064x", 0xe7)
	sole, err := p.UpsertBookFile(ctx, uploadInput(soleHash, "s.epub", reader))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	shared, err := p.UpsertBookFile(ctx, uploadInput(sharedHash, "t.epub", reader))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: sharedHash, UserID: friend}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if _, err := p.CreateAnnotation(ctx, epubMark(reader, sole.BookFileID, 0, "p[1]", 0, 5)); err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	if _, err := p.EraseSoleHoldings(ctx, friend, reader); !errors.Is(err, store.ErrForbidden) {
		t.Errorf("a reader erasing somebody: %v, want ErrForbidden", err)
	}
	if _, err := p.EraseSoleHoldings(ctx, admin, uuid.New()); !isNotFound(err) {
		t.Errorf("an unknown account: %v, want not found", err)
	}
	if !fileExists(t, p, sole.BookFileID) {
		t.Fatal("a refused erasure erased")
	}

	erasure, err := p.EraseSoleHoldings(ctx, admin, reader)
	if err != nil {
		t.Fatalf("EraseSoleHoldings: %v", err)
	}
	if len(erasure.Erased) != 1 || erasure.Erased[0] != sole.BookFileID ||
		len(erasure.Owed) != 1 || erasure.Owed[0] != soleHash {
		t.Errorf("erasure = %+v, want the sole book", erasure)
	}
	if !userExists(t, p, reader) {
		t.Error("erasing books deleted the account")
	}
	if fileExists(t, p, sole.BookFileID) {
		t.Error("the sole book is still recorded")
	}
	if !canRead(t, p, shared.BookFileID, reader) || !canRead(t, p, shared.BookFileID, friend) {
		t.Error("a book somebody else holds too was touched")
	}
	var released bool
	if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM released_holdings WHERE user_id = $1 AND content_hash = $2)`,
		reader, soleHash).Scan(&released); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !released {
		t.Error("the erased book's release was not remembered, so a scan would hand it back")
	}

	// Again: nothing left to erase, and no error.
	again, err := p.EraseSoleHoldings(ctx, admin, reader)
	if err != nil || len(again.Erased) != 0 {
		t.Errorf("erasing again = %+v, %v; want nothing", again, err)
	}
}

// A large shelf is erased in batches, a transaction each, so no transaction
// holds more advisory locks than a batch: every sole-held file still goes.
func TestErasingALargeShelfGoesInBatches(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, reader, friend := testOwner(t, p), secondUser(t, p), secondUser(t, p)
	eraseBatchSize = 2
	t.Cleanup(func() { eraseBatchSize = EraseBatch })

	var sole []uuid.UUID
	for i := range 5 {
		out, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", 0xa00+2*i), fmt.Sprintf("%d.epub", i), reader))
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}
		sole = append(sole, out.BookFileID)
	}
	// A shared one in the middle of the order, which stays.
	sharedHash := fmt.Sprintf("%064x", 0xa05)
	shared, err := p.UpsertBookFile(ctx, uploadInput(sharedHash, "s.epub", reader))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: sharedHash, UserID: friend}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}

	erasure, err := p.EraseSoleHoldings(ctx, admin, reader)
	if err != nil {
		t.Fatalf("EraseSoleHoldings: %v", err)
	}
	if len(erasure.Erased) != len(sole) {
		t.Errorf("erased %d, want the %d only they held", len(erasure.Erased), len(sole))
	}
	for _, id := range sole {
		if fileExists(t, p, id) {
			t.Errorf("%s survived", id)
		}
	}
	if !canRead(t, p, shared.BookFileID, friend) || !canRead(t, p, shared.BookFileID, reader) {
		t.Error("the shared book was touched")
	}
}

// An erasure and an upload of the same bytes. The erasure commits its
// records and owes the copies; paying the debt takes the bytes' lock, as
// recording an upload does. An upload recorded first keeps the copies - the
// debt waits for its extraction. An upload that stored its copy before the
// debt was paid, and is recorded after, puts its own copy back in place
// while it is recorded, under the same lock (Settle). And an upload recorded
// before the erasure makes its uploader a holder, and nothing is erased.
func TestAnErasureAndAnUploadOfTheSameBytes(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	admin := testOwner(t, p)
	content := []byte("the same bytes, uploaded twice")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	type fixture struct {
		uploads          ingest.UploadStore
		discard          ingest.Discard
		leaver, uploader uuid.UUID
		fileID           uuid.UUID
	}
	setup := func(t *testing.T, libraryCopy bool) fixture {
		t.Helper()
		cleanLibrary(t, p)
		dir := t.TempDir()
		f := fixture{
			uploads: ingest.UploadStore{Root: filepath.Join(dir, "uploads")},
			leaver:  secondUser(t, p), uploader: secondUser(t, p),
		}
		f.discard = ingest.Discard{Uploads: f.uploads.Root, Archive: filepath.Join(dir, "archive"),
			Covers: filepath.Join(dir, "covers"), Library: filepath.Join(dir, "library")}
		in := uploadInput(hash, "theirs.epub", f.leaver)
		if libraryCopy {
			// Scanned from the library directory: nothing stored of it.
			in.Path = filepath.Join(f.discard.Library, "scanned.epub")
			in.Uploaded = false
		} else {
			stored, err := f.uploads.Store(ctx, "theirs.epub", bytes.NewReader(content))
			if err != nil {
				t.Fatalf("Store: %v", err)
			}
			if err := f.uploads.Settle(stored, true); err != nil {
				t.Fatalf("Settle: %v", err)
			}
			in.Path = stored.Path
		}
		out, err := p.UpsertBookFile(ctx, in)
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}
		f.fileID = out.BookFileID
		return f
	}
	deleteCopies := func(d ingest.Discard) func(StoredCopies) error {
		return func(c StoredCopies) error {
			paths := append([]string{}, c.Paths...)
			if c.ContentHash != "" {
				paths = append(paths, d.Copies(c.ContentHash)...)
			}
			for _, path := range paths {
				if c.SkipUploads && d.IsUpload(path) {
					continue
				}
				if _, err := d.Remove(path); err != nil {
					return err
				}
			}
			return nil
		}
	}
	// hold records the upload as the upload handler does: settling its copy
	// under the lock, as it is recorded.
	hold := func(ctx context.Context, f fixture, mine ingest.StoredFile) error {
		_, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: f.uploader,
			Settle: func() error { return f.uploads.Settle(mine, true) }})
		return err
	}
	readable := func(t *testing.T, path string) {
		t.Helper()
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("the upload's bytes are %q, %v", got, err)
		}
	}

	for _, tc := range []struct {
		name        string
		libraryCopy bool
	}{
		{"a duplicate of the leaver's stored copy", false},
		{"a new copy, the leaver's in the library directory", true},
	} {
		t.Run("the debt paid while the upload waits: "+tc.name, func(t *testing.T) {
			f := setup(t, tc.libraryCopy)
			mine, err := f.uploads.Store(ctx, "mine.epub", bytes.NewReader(content))
			if err != nil || mine.Duplicate == tc.libraryCopy {
				t.Fatalf("Store = %+v, %v", mine, err)
			}
			erasure, err := p.DeleteAccountErasing(ctx, admin, f.leaver)
			if err != nil || len(erasure.Owed) != 1 {
				t.Fatalf("erasing = %+v, %v", erasure, err)
			}

			// The debt is being paid, holding the bytes' lock, when the upload
			// is recorded: it waits.
			reached, proceed := make(chan struct{}), make(chan struct{})
			afterDiscardLock = func() {
				afterDiscardLock = nil
				close(reached)
				<-proceed
			}
			t.Cleanup(func() { afterDiscardLock = nil })
			paying := make(chan DiscardResult, 1)
			go func() {
				res, err := p.DiscardPending(context.Background(), erasure.Owed, deleteCopies(f.discard))
				if err != nil {
					t.Errorf("DiscardPending: %v", err)
				}
				paying <- res
			}()
			<-reached
			holding := make(chan error, 1)
			go func() { holding <- hold(context.Background(), f, mine) }()
			waitForLockWait(t, p, "book_file_holding:")
			close(proceed)

			if res := <-paying; res.Paid != 1 {
				t.Fatalf("paid = %+v, want the debt paid", res)
			}
			if err := <-holding; err != nil {
				t.Fatalf("recording the upload: %v", err)
			}
			// The stored copy went with the debt; the upload's own copy was
			// put back as it was recorded, so its extraction has bytes to read.
			readable(t, mine.Path)
			var claimed bool
			if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM holding_claims WHERE content_hash = $1 AND user_id = $2)`,
				hash, f.uploader).Scan(&claimed); err != nil || !claimed {
				t.Fatalf("the upload was not recorded: %v", err)
			}
		})
	}

	t.Run("the upload recorded before the debt is paid", func(t *testing.T) {
		f := setup(t, false)
		if _, err := p.DeleteAccountErasing(ctx, admin, f.leaver); err != nil {
			t.Fatalf("erasing: %v", err)
		}
		mine, err := f.uploads.Store(ctx, "mine.epub", bytes.NewReader(content))
		if err != nil {
			t.Fatalf("Store: %v", err)
		}
		if err := hold(ctx, f, mine); err != nil {
			t.Fatalf("recording: %v", err)
		}
		res, err := p.DiscardPending(ctx, nil, deleteCopies(f.discard))
		if err != nil || res.Waiting != 1 {
			t.Fatalf("DiscardPending = %+v, %v; want the debt waiting for the upload", res, err)
		}
		readable(t, mine.Path)
	})

	t.Run("the upload recorded before the erasure", func(t *testing.T) {
		f := setup(t, false)
		mine, err := f.uploads.Store(ctx, "mine.epub", bytes.NewReader(content))
		if err != nil {
			t.Fatalf("Store: %v", err)
		}
		if err := hold(ctx, f, mine); err != nil {
			t.Fatalf("recording: %v", err)
		}
		erasure, err := p.DeleteAccountErasing(ctx, admin, f.leaver)
		if err != nil {
			t.Fatalf("erasing: %v", err)
		}
		if len(erasure.Erased) != 0 || len(erasure.Owed) != 0 {
			t.Errorf("erasure = %+v; the uploader holds that book too", erasure)
		}
		if !canRead(t, p, f.fileID, f.uploader) {
			t.Error("the uploader lost the book")
		}
		readable(t, mine.Path)
	})
}

// Erasing somebody's books now holds only their account's key-share, so a
// holding of theirs can commit after the erasure has taken its file locks -
// an extraction of their upload finishing. That file was not locked, and is
// not erased: erasing it would change who holds it without its lock.
func TestErasingNowLeavesAHoldingThatArrivedAfterItsLocks(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, reader := testOwner(t, p), secondUser(t, p)
	early, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", 0xed), "early.epub", reader))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	reached, proceed := make(chan struct{}), make(chan struct{})
	afterEraseLock = func() {
		afterEraseLock = nil
		close(reached)
		<-proceed
	}
	t.Cleanup(func() { afterEraseLock = nil })

	erasing := make(chan Erasure, 1)
	failed := make(chan error, 1)
	go func() {
		out, err := p.EraseSoleHoldings(context.Background(), admin, reader)
		failed <- err
		erasing <- out
	}()
	<-reached
	late, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", 0xee), "late.epub", reader))
	if err != nil {
		t.Fatalf("a holding arriving meanwhile: %v", err)
	}
	close(proceed)
	if err := <-failed; err != nil {
		t.Fatalf("EraseSoleHoldings: %v", err)
	}
	out := <-erasing

	if len(out.Erased) != 1 || out.Erased[0] != early.BookFileID {
		t.Errorf("erased %v, want only the book held when the erasure locked", out.Erased)
	}
	if !fileExists(t, p, late.BookFileID) || !canRead(t, p, late.BookFileID, reader) {
		t.Error("a book that arrived after the erasure's locks was erased")
	}
}

// The account-deletion races the archive path is pinned against, run with
// erasing: an ingest draining claims, a release, an upload forgetting a
// release. Each holds a file's lock or the account when the deletion starts;
// none may deadlock.
func TestErasingDeletionTakesLocksInTheDocumentedOrder(t *testing.T) {
	race := func(t *testing.T, p *Pool, admin, x uuid.UUID, hook *func(), step func() error) {
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
			_, err := p.DeleteAccountErasing(context.Background(), admin, x)
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

	t.Run("a drain", func(t *testing.T) {
		p := testPool(t)
		cleanLibrary(t, p)
		ctx := t.Context()
		admin, x := testOwner(t, p), secondUser(t, p)
		hash := fmt.Sprintf("%064x", 0xe8)
		if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: x}); err != nil {
			t.Fatalf("HoldUpload: %v", err)
		}
		race(t, p, admin, x, &betweenHoldingAndClaims, func() error {
			_, err := p.UpsertBookFile(ctx, uploadInput(hash, "x.epub", x))
			return err
		})
	})

	t.Run("a release", func(t *testing.T) {
		p := testPool(t)
		cleanLibrary(t, p)
		ctx := t.Context()
		admin, x, y := testOwner(t, p), secondUser(t, p), secondUser(t, p)
		hash := fmt.Sprintf("%064x", 0xe9)
		out, err := p.UpsertBookFile(ctx, uploadInput(hash, "x.epub", x))
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}
		if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: y}); err != nil {
			t.Fatalf("HoldUpload: %v", err)
		}
		race(t, p, admin, x, &afterReleaseArchive, func() error {
			_, err := p.ReleaseHolding(ctx, out.BookFileID, x)
			return err
		})
	})

	t.Run("forgetting a release", func(t *testing.T) {
		p := testPool(t)
		cleanLibrary(t, p)
		ctx := t.Context()
		admin, x := testOwner(t, p), secondUser(t, p)
		hash := fmt.Sprintf("%064x", 0xea)
		if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "a.epub", admin)); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		if _, err := p.Exec(ctx, `INSERT INTO released_holdings (user_id, content_hash) VALUES ($1, $2)`,
			x, hash); err != nil {
			t.Fatalf("remembering a release: %v", err)
		}
		race(t, p, admin, x, &deliberateHoldStep, func() error {
			_, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: x})
			return err
		})
	})

	// Erasing somebody's books now, while their account is deleted: the
	// erasure holds their account (key-share) and their files' locks; the
	// deletion waits at the account.
	t.Run("erasing books now", func(t *testing.T) {
		p := testPool(t)
		cleanLibrary(t, p)
		ctx := t.Context()
		admin, x := testOwner(t, p), secondUser(t, p)
		if _, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", 0xeb), "x.epub", x)); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		race(t, p, admin, x, &afterEraseLock, func() error {
			_, err := p.EraseSoleHoldings(ctx, admin, x)
			return err
		})
	})
}

// An edition is deleted only when it is empty once it is locked. A file being
// added to it at that moment either holds it - and the edition is skipped -
// or waits for the deletion and fails, loudly, rather than being cascaded
// away; a file added before is seen.
func TestEmptyEditionsAreCheckedAfterTheyAreLocked(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	insertFile := func(ctx context.Context, edition uuid.UUID, hash string) error {
		_, err := p.Exec(ctx, `
			INSERT INTO book_files (edition_id, path, content_hash, format, size_bytes)
			VALUES ($1, '/data/uploads/x.epub', $2, 'epub', 1)`, edition, hash)
		return err
	}
	// An edition whose only file has gone, as an erasure leaves it.
	emptyEdition := func(t *testing.T, seed int) (edition, work uuid.UUID) {
		t.Helper()
		cleanLibrary(t, p)
		out, err := p.UpsertBookFile(ctx, uploadInput(fmt.Sprintf("%064x", seed), "e.epub", testOwner(t, p)))
		if err != nil {
			t.Fatalf("seeding: %v", err)
		}
		if err := p.QueryRow(ctx, `
			DELETE FROM book_files bf USING editions e
			 WHERE bf.id = $1 AND e.id = bf.edition_id
			RETURNING e.id, e.work_id`, out.BookFileID).Scan(&edition, &work); err != nil {
			t.Fatalf("emptying: %v", err)
		}
		return edition, work
	}
	exists := func(table string, id uuid.UUID) bool {
		var ok bool
		if err := p.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+` WHERE id = $1)`, id).Scan(&ok); err != nil {
			t.Fatalf("reading: %v", err)
		}
		return ok
	}

	t.Run("a file being added holds the edition", func(t *testing.T) {
		edition, work := emptyEdition(t, 0xf10)
		tx, err := p.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // committed below
		hash := fmt.Sprintf("%064x", 0xf11)
		if _, err := tx.Exec(ctx, `
			INSERT INTO book_files (edition_id, path, content_hash, format, size_bytes)
			VALUES ($1, '/data/uploads/x.epub', $2, 'epub', 1)`, edition, hash); err != nil {
			t.Fatalf("adding: %v", err)
		}
		if err := p.DeleteEmptyEditions(ctx, []uuid.UUID{edition}); err != nil {
			t.Fatalf("DeleteEmptyEditions: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("the addition: %v", err)
		}
		if !exists("editions", edition) || !exists("works", work) {
			t.Error("an edition being added to was deleted")
		}
		var files int
		if err := p.QueryRow(ctx, `SELECT count(*) FROM book_files WHERE content_hash = $1`, hash).Scan(&files); err != nil || files != 1 {
			t.Errorf("the added file: %d rows, %v", files, err)
		}
	})

	t.Run("a file added once the edition is locked fails rather than vanishing", func(t *testing.T) {
		edition, work := emptyEdition(t, 0xf12)
		hash := fmt.Sprintf("%064x", 0xf13)
		reached, proceed := make(chan struct{}), make(chan struct{})
		betweenEmptyCheckAndDelete = func() {
			betweenEmptyCheckAndDelete = nil
			close(reached)
			<-proceed
		}
		t.Cleanup(func() { betweenEmptyCheckAndDelete = nil })

		deleting := make(chan error, 1)
		go func() { deleting <- p.DeleteEmptyEditions(context.Background(), []uuid.UUID{edition}) }()
		<-reached
		adding := make(chan error, 1)
		go func() { adding <- insertFile(context.Background(), edition, hash) }()
		waitForLockWait(t, p, "INSERT INTO book_files")
		close(proceed)

		if err := <-deleting; err != nil {
			t.Fatalf("DeleteEmptyEditions: %v", err)
		}
		if err := <-adding; err == nil {
			t.Fatal("a file was added to an edition that was deleted under it")
		}
		if exists("editions", edition) || exists("works", work) {
			t.Error("the empty edition and its work were kept")
		}
	})

	t.Run("a file added before is seen", func(t *testing.T) {
		edition, _ := emptyEdition(t, 0xf14)
		if err := insertFile(ctx, edition, fmt.Sprintf("%064x", 0xf15)); err != nil {
			t.Fatalf("adding: %v", err)
		}
		if err := p.DeleteEmptyEditions(ctx, []uuid.UUID{edition}); err != nil {
			t.Fatalf("DeleteEmptyEditions: %v", err)
		}
		if !exists("editions", edition) {
			t.Error("an edition with a file was deleted")
		}
	})
}

// deleteStoredCopies deletes as the server does (internal/http
// deleteCopiesIndexed): the owed paths and every stored copy of the hash,
// sparing the upload directory when asked, and never the library directory.
func deleteStoredCopies(d ingest.Discard) func(StoredCopies) error {
	return func(c StoredCopies) error {
		paths := append([]string{}, c.Paths...)
		if c.ContentHash != "" {
			paths = append(paths, d.Copies(c.ContentHash)...)
		}
		for _, path := range paths {
			if c.SkipUploads && d.IsUpload(path) {
				continue
			}
			if _, err := d.Remove(path); err != nil {
				return err
			}
		}
		return nil
	}
}

// Found in review. A record of owed bytes exists again - a scanned book whose
// file is missing - and somebody uploads the same bytes: the upload is
// recorded as a holding, and its extraction is queued a moment later, in a
// separate statement. A pass over the debts in between must not take the
// upload's only copy. The bytes are the record's holder's now; nothing of
// them is the erasure's to delete.
func TestAReturningRecordsBytesAreNotTheErasures(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	root := t.TempDir()
	d := ingest.Discard{Uploads: filepath.Join(root, "uploads"), Archive: filepath.Join(root, "archive"),
		Covers: filepath.Join(root, "covers"), Library: filepath.Join(root, "library")}
	content := []byte("bytes coming back while a debt is owed")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	admin, uploader := testOwner(t, p), secondUser(t, p)
	in := uploadInput(hash, "x.epub", admin)
	in.Path = filepath.Join(d.Library, "x.epub")
	in.Uploaded = false
	if _, err := p.UpsertBookFile(ctx, in); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE book_files SET missing_since = now() WHERE content_hash = $1`, hash); err != nil {
		t.Fatalf("missing: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO pending_discards (content_hash) VALUES ($1)`, hash); err != nil {
		t.Fatalf("owing: %v", err)
	}

	uploads := &ingest.UploadStore{Root: d.Uploads}
	stored, err := uploads.Store(ctx, "again.epub", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: uploader, OriginalFilename: "again.epub",
		Settle: func() error { return uploads.Settle(stored, true) }}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	// The extraction is not queued yet.
	res, err := p.DiscardPending(ctx, nil, deleteStoredCopies(d))
	if err != nil || res.Paid != 1 {
		t.Fatalf("DiscardPending = %+v, %v; want the debt settled", res, err)
	}
	if _, err := os.Stat(stored.Path); err != nil {
		t.Errorf("the upload's only copy was deleted before its extraction was queued: %v", err)
	}
}

// Found in review. The data directory written down one way when a record was
// made and configured another way now: the record's copy is still its copy,
// and is not deleted as a stray.
func TestADifferentlySpelledDataDirectoryDeletesNothing(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	root := t.TempDir()
	content := []byte("bytes under a directory with two names")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	admin := testOwner(t, p)

	then := &ingest.UploadStore{Root: filepath.Join(root, "x", "..", "uploads") + "/"}
	stored, err := then.Store(ctx, "a.epub", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := then.Settle(stored, true); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	in := uploadInput(hash, "a.epub", admin)
	in.Path = stored.Path
	if _, err := p.UpsertBookFile(ctx, in); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO pending_discards (content_hash) VALUES ($1)`, hash); err != nil {
		t.Fatalf("owing: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(cwd, filepath.Join(root, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	now := ingest.Discard{Uploads: rel, Archive: filepath.Join(root, "archive")}
	if res, err := p.DiscardPending(ctx, nil, deleteStoredCopies(now)); err != nil || res.Paid != 1 {
		t.Fatalf("DiscardPending = %+v, %v", res, err)
	}
	if _, err := os.Stat(stored.Path); err != nil {
		t.Errorf("a live record's only copy was deleted: %v", err)
	}

	// The same for a replaced copy: a path naming the record's own file is
	// not a stray, however it is spelled.
	other := filepath.Join(cwd, rel, "books", hash[:2], hash+".epub")
	removed := false
	if err := p.RemoveIfUnreferenced(ctx, hash, other, func(string) error { removed = true; return nil }); err != nil {
		t.Fatalf("RemoveIfUnreferenced: %v", err)
	}
	if removed {
		t.Errorf("%s was removed as unreferenced; the record points at it as %s", other, stored.Path)
	}
}

func TestUniqueStrings(t *testing.T) {
	for _, tc := range []struct{ in, want []string }{
		{nil, nil},
		{[]string{"b", "a", "b", "c", "a"}, []string{"a", "b", "c"}},
		{[]string{"x"}, []string{"x"}},
	} {
		got := uniqueStrings(tc.in)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("uniqueStrings(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// holds reports whether somebody holds a file of these bytes.
func holds(t *testing.T, p *Pool, user uuid.UUID, hash string) bool {
	t.Helper()
	var ok bool
	if err := p.QueryRow(t.Context(), `
		SELECT EXISTS (SELECT 1 FROM book_file_holders h JOIN book_files bf ON bf.id = h.book_file_id
		                WHERE h.user_id = $1 AND bf.content_hash = $2)`, user, hash).Scan(&ok); err != nil {
		t.Fatalf("reading: %v", err)
	}
	return ok
}

// Erasing somebody's books now withdraws their uploads not extracted yet:
// the claim goes, a queued extraction is cancelled, the bytes are owed and
// the release remembered - so a later extraction of the same bytes, theirs
// already running or somebody else's, does not give them the book.
func TestErasingNowWithdrawsUploadsOnTheirWay(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, reader, other := testOwner(t, p), secondUser(t, p), secondUser(t, p)
	queued, running := fmt.Sprintf("%064x", 0xd1), fmt.Sprintf("%064x", 0xd2)
	if _, err := p.Exec(ctx, `DELETE FROM jobs WHERE payload ->> 'contentHash' = ANY($1)`,
		[]string{queued, running}); err != nil {
		t.Fatalf("clearing jobs: %v", err)
	}
	for _, hash := range []string{queued, running} {
		if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: reader, OriginalFilename: "mine.epub"}); err != nil {
			t.Fatalf("HoldUpload: %v", err)
		}
	}
	target := uuid.New()
	job, _, err := p.Enqueue(ctx, EnqueueOptions{
		Type: domain.JobExtract, TargetID: &target, RequestedBy: reader,
		Payload: map[string]any{"contentHash": queued, "uploaded": true},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	erasure, err := p.EraseSoleHoldings(ctx, admin, reader)
	if err != nil {
		t.Fatalf("EraseSoleHoldings: %v", err)
	}
	if erasure.Withdrawn != 2 || len(erasure.Erased) != 0 {
		t.Errorf("erasure = %+v, want both uploads withdrawn and no file erased", erasure)
	}
	if got := owedHashes(t, p); len(got) != 2 || got[0] != queued || got[1] != running {
		t.Errorf("owed %v, want both uploads' bytes", got)
	}
	var claims, releases int
	if err := p.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM holding_claims WHERE user_id = $1),
		       (SELECT count(*) FROM released_holdings WHERE user_id = $1)`, reader).Scan(&claims, &releases); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if claims != 0 || releases != 2 {
		t.Errorf("claims %d, releases %d after the erasure; want none and two", claims, releases)
	}
	var status string
	if err := p.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, job).Scan(&status); err != nil {
		t.Fatalf("reading the job: %v", err)
	}
	if status != "cancelled" {
		t.Errorf("their queued extraction is %s, want cancelled", status)
	}

	// Somebody else uploads the queued bytes, and their extraction runs: it
	// is theirs, and nobody else's.
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: queued, UserID: other}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if _, err := p.UpsertBookFile(ctx, uploadInput(queued, "theirs.epub", other)); err != nil {
		t.Fatalf("their extraction: %v", err)
	}
	if !holds(t, p, other, queued) || holds(t, p, reader, queued) {
		t.Error("another upload's extraction gave the erased person the book back")
	}
	// Their own extraction, already running when the erasure came, finishes:
	// the record is nobody's, archived with no name.
	out, err := p.UpsertBookFile(ctx, uploadInput(running, "mine.epub", reader))
	if err != nil {
		t.Fatalf("their extraction: %v", err)
	}
	if holds(t, p, reader, running) {
		t.Error("their own running extraction gave the erased person the book back")
	}
	var archived bool
	if err := p.QueryRow(ctx, `SELECT archived_at IS NOT NULL AND archived_owner IS NULL FROM book_files WHERE id = $1`,
		out.BookFileID).Scan(&archived); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !archived {
		t.Error("a record nobody holds is not archived with no name")
	}

	// Uploading it again is changing their mind, as ever.
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: running, UserID: reader}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if !holds(t, p, reader, running) {
		t.Error("uploading the bytes again did not bring the book back")
	}
}

// A claim whose person has let the bytes go since gives nothing when the
// file arrives, as the extraction's own holder would not: drained, not
// turned into a holding.
func TestADrainRespectsARelease(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	x, y := secondUser(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xd3)
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: x}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO released_holdings (user_id, content_hash) VALUES ($1, $2)`, x, hash); err != nil {
		t.Fatalf("remembering a release: %v", err)
	}
	if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "y.epub", y)); err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if holds(t, p, x, hash) || !holds(t, p, y, hash) {
		t.Error("a released claim was turned into a holding")
	}
	var claims int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM holding_claims WHERE content_hash = $1`, hash).Scan(&claims); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if claims != 0 {
		t.Errorf("%d claims left after the drain, want none", claims)
	}
}

// Erasing now takes the lock of bytes the person only claims, holding just a
// key-share on their account. A drain of those bytes holds their lock and
// then wants the person's account, key-share; a deletion of the account
// arriving in between waits FOR UPDATE behind the erasure. The drain's
// key-share must not queue behind the deletion, or the three wait in a
// circle: driven here exactly, and all three finish.
func TestErasingAClaimRacesADrainAndADeletion(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	admin, x, y := testOwner(t, p), secondUser(t, p), secondUser(t, p)
	hash := fmt.Sprintf("%064x", 0xd4)
	if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: x}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}

	reached, proceed := make(chan struct{}), make(chan struct{})
	var release sync.Once
	letGo := func() { release.Do(func() { close(proceed) }) }
	betweenHoldingAndClaims = func() {
		betweenHoldingAndClaims = nil
		close(reached)
		<-proceed
	}
	// A failure below must not leave the drain parked holding its lock.
	t.Cleanup(func() {
		betweenHoldingAndClaims = nil
		letGo()
	})

	// Somebody else's extraction of the same bytes, holding their lock and
	// about to drain x's claim.
	drain := make(chan error, 1)
	go func() {
		_, err := p.UpsertBookFile(context.Background(), uploadInput(hash, "y.epub", y))
		drain <- err
	}()
	<-reached

	erasing := make(chan error, 1)
	go func() {
		_, err := p.EraseSoleHoldings(context.Background(), admin, x)
		erasing <- err
	}()
	waitForLockWait(t, p, "'book_file_holding:' || t.h")

	deletion := make(chan error, 1)
	go func() {
		_, err := p.DeleteAccount(context.Background(), admin, x)
		deletion <- err
	}()
	waitForLockWait(t, p, "FROM users WHERE id = $1 FOR UPDATE")
	letGo()

	if err := <-drain; err != nil {
		t.Errorf("the drain: %v", err)
	}
	if err := <-erasing; err != nil {
		t.Errorf("erasing: %v", err)
	}
	if err := <-deletion; err != nil {
		t.Errorf("deleting the account: %v", err)
	}
	if userExists(t, p, x) || !holds(t, p, y, hash) {
		t.Error("after the race: the account is still there, or the other holder lost the book")
	}
}

// An upload's extraction that finishes without putting the book on its
// uploader's shelf - they had it erased, or left, while it ran - forgets
// them from its own job: no file name, no owner, no requester, as the scrub
// would have done had it finished first. One that does shelve it keeps
// what it says, for the person following it.
func TestARunningUploadForgetsAnUploaderItDoesNotShelve(t *testing.T) {
	for _, tc := range []struct {
		name    string
		before  func(p *Pool, admin, x uuid.UUID) error
		forgets bool
	}{
		{"shelved as ever", func(*Pool, uuid.UUID, uuid.UUID) error { return nil }, false},
		{"erased while it ran", func(p *Pool, admin, x uuid.UUID) error {
			_, err := p.EraseSoleHoldings(context.Background(), admin, x)
			return err
		}, true},
		{"their account deleted while it ran", func(p *Pool, admin, x uuid.UUID) error {
			_, err := p.DeleteAccount(context.Background(), admin, x)
			return err
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPool(t)
			cleanLibrary(t, p)
			ctx := t.Context()
			admin, x := testOwner(t, p), secondUser(t, p)
			hash := fmt.Sprintf("%064x", 0xd5)
			if _, err := p.Exec(ctx, `DELETE FROM jobs WHERE payload ->> 'contentHash' = $1`, hash); err != nil {
				t.Fatalf("clearing jobs: %v", err)
			}
			if _, err := p.HoldUpload(ctx, HoldInput{ContentHash: hash, UserID: x, OriginalFilename: "my diary.epub"}); err != nil {
				t.Fatalf("HoldUpload: %v", err)
			}
			job := insertJob(t, p, "running", 0, &x, map[string]any{"contentHash": hash, "uploaded": true,
				"originalFilename": "my diary.epub", "ownerId": x.String()})
			t.Cleanup(func() { _, _ = p.Exec(context.Background(), `DELETE FROM jobs WHERE id = $1`, job) })

			if err := tc.before(p, admin, x); err != nil {
				t.Fatalf("meanwhile: %v", err)
			}
			if _, err := p.UpsertBookFile(ctx, uploadInput(hash, "my diary.epub", x)); err != nil {
				t.Fatalf("the extraction: %v", err)
			}

			payload, by, ok := jobRow(t, p, job)
			if !ok {
				t.Fatal("the running job is gone")
			}
			_, named := payload["originalFilename"]
			_, owned := payload["ownerId"]
			if forgot := !named && !owned && by == nil; forgot != tc.forgets {
				t.Errorf("job says %v, asked by %v; want forgotten = %v", payload, by, tc.forgets)
			}
			if payload["contentHash"] != hash {
				t.Errorf("the job lost what it is about: %v", payload)
			}
		})
	}
}
