// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// A holding is one person's claim to one BookFile: they added it, so it is on
// their shelf and they may read, download and lend it. A file can have several
// holders - two people in one household adding the same EPUB each hold it -
// while the file itself, and everything derived from it, stays one row.
//
// Four invariants, kept here and nowhere else:
//
//   - A holding is recorded whenever somebody adds a file, whether the file was
//     new to the installation, already on the shelf, or still being extracted
//     for somebody else. Before an extraction has created the row, the holding
//     waits as a claim on the content hash and the ingest turns it into one.
//   - A file has holders, or it is archived - never neither. A file that loses
//     or never gets a holder is archived; one that gains a holder stops being
//     archived. A scan never claims an archived book - that is an
//     administrator's decision - and nothing but a deliberate act gives back a
//     book somebody let go: a release is remembered in released_holdings, a
//     scan or an extraction already queued respects it, and only uploading the
//     file again or being handed it (holdDeliberate) forgets it.
//   - Blocked bytes have no holder, no claim and no release, and get none: an
//     administrator removed them for everybody (moderation.go), and every path
//     that would add a holding, a claim or a record for them checks
//     blocked_content under the file's lock first and refuses
//     (refuseBlocked). Only lifting the block ends that.
//   - Everything that changes who holds a file takes the same per-hash lock
//     first, so an upload cannot slip between an ingest reading the claims and
//     committing, nor between an account deletion deciding a file has no other
//     holder and archiving it, nor between a removal blocking the bytes and
//     taking the file off every shelf.
//
// Lock order. Every path that changes holdings takes its locks in this order,
// and this is the one place it is written down:
//
//  1. the per-hash advisory lock (lockContentHash), for each hash in content
//     hash order when there are several;
//  2. the book_files row;
//  3. users rows - key-share, taken by the foreign key of a new holding,
//     claim, release or block row, and explicitly where noted; FOR UPDATE
//     only by account deletion;
//  4. blocked_content rows - written only under the file's lock, by a removal
//     (insert) and by lifting a block (delete), and by account deletion's
//     cascade (blocked_by set null); read, unlocked, by every arrival;
//  5. released_holdings rows;
//  6. holding_claims rows;
//  7. book_file_holders rows (and book_access, by cascade from them);
//  8. the book_files row again, to archive or unarchive it;
//  9. pending_discards rows - what an erasure owes the disk - written last,
//     in content-hash order, by an erasure (under the bytes' locks for the
//     files it erased and the uploads EraseSoleHoldings withdrew; an erasing
//     deletion owes its pending uploads without them) and by an expiry in
//     erase mode; paid by DiscardPending, which takes the bytes' lock (1)
//     and then the row. A writer of the row never waits on that lock, so
//     DiscardPending waiting on the row while holding the lock closes no
//     cycle.
//
// Audit, statement by statement (numbers are the steps above; "4 read" is a
// plain read of the blocklist, which takes no row lock):
//
//	path                      locks, in the order taken
//	DeleteAccount             3 account FOR UPDATE; 1 every hash they hold, sorted;
//	                          2/8 the files only they hold (archive); (their
//	                          finished jobs scrubbed); cascade from the account,
//	                          in the order of its foreign keys' triggers: 7
//	                          holdings, 6 claims, (their jobs, set null), 5
//	                          releases, 4 their blocks (set null)
//	DeleteAccountErasing      3 account FOR UPDATE; 1 every hash they hold, sorted;
//	                          (their pending upload extractions, cancelled); 2/8
//	                          the files only they hold that somebody awaits
//	                          (archive, nameless); 2 FOR UPDATE the other files
//	                          only they hold (queued jobs cancelled), 8 deleted -
//	                          cascading 7 holdings and grants; 9 what that owes;
//	                          then their finished jobs scrubbed and the account's
//	                          cascade, as DeleteAccount. No file is deleted
//	                          before the commit
//	EraseSoleHoldings         per batch of EraseBatch hashes: 3 administrator read,
//	                          person key-share; 1 those hashes - held or claimed -
//	                          sorted; 6 (read) their claims; 2 FOR UPDATE, 5
//	                          release rows, 6 their claims deleted, (queued jobs
//	                          cancelled), 8 deleted - cascading 7; 9 what that
//	                          owes. Only the hashes locked at 1 are erased: with
//	                          just a key-share on the account a holding can still
//	                          arrive meanwhile, and it is left alone. Then, in a
//	                          transaction of its own, their finished jobs
//	                          scrubbed: finished rows, which only a deletion's
//	                          cascade and the retention sweep also write
//	DiscardPending            per owed hash: 1; 9 FOR UPDATE; reads (records,
//	                          claims, blocks, jobs); the files deleted; 9 deleted
//	                          or marked failed
//	DeleteEmptyEditions       editions, then works, FOR UPDATE SKIP LOCKED, then
//	                          checked and deleted, after the erasure committed: no
//	                          step here, and it waits on nothing
//	ReleaseHolding            3 account key-share; 1; 2/8 the file (archive, if last);
//	                          5 release row; 6 claims; 7 holding; then their note shares
//	RemoveBookFileForEveryone 3 administrator key-share; 3 every account it will
//	                          touch, key-share, sorted; 1; 2 FOR UPDATE; 4 block
//	                          row; 5 releases; 6 claims; 7 holdings (and grants);
//	                          then note shares, quotes if purged, passages (and
//	                          citations' links), queued jobs; 8 archive
//	DiscardBlockedUpload      1; 4 read, and on failure 4 update
//	Unblock                   1; 4 delete
//	DiscardBlockedCopies      1; 4 read; 2 (read); 8 forget the cover and archive
//	                          copy; 4 update (copies gone, or not)
//	HoldUpload, file exists   1; 4 read; 2 (read); 7 new holding, its key takes 3;
//	                          5 forget the release; 8 unarchive; then the upload's
//	                          stored copy settled (files only)
//	HoldUpload, no file yet   1; 4 read; 6 new claim, its key takes 3; 5 forget the
//	                          release; then the stored copy settled (files only)
//	UpsertBookFile            1; 4 read; 2 insert/update; 5 read, 7 owner's holding,
//	                          its key takes 3; 3 claimants, explicitly; 6 claims;
//	                          5 read; 7 their holdings (not past a release);
//	                          (an uploader left off the shelf is scrubbed from
//	                          their own running job); 8 unarchive, or archive if
//	                          nobody holds it
//	TouchSeen                 1; 4 read; 2; 7 scanner's holding, its key takes 3
//	ClaimArchived             1; 2 and 4 (read); 7 holding, its key takes 3; 5 forget
//	                          the release; 8 unarchive
//	MoveArchived              1; 2 and 4 (read)
//	RemoveIfUnreferenced      1; 2 (read)
//	SaveChunks                2 FOR SHARE, no advisory lock; 4 read; then passages.
//	                          It waits on nothing but the row, and a removal takes
//	                          the row before the passages, so the two never wait
//	                          on each other the other way round
//	ExpireArchived            2 delete, no advisory lock: an archived file has no
//	                          holdings or grants left to cascade, and claims,
//	                          releases and blocks are keyed on bytes, not on the row;
//	                          in erase mode 9 what that owes
//
// Two kinds of step out of order, and why each is safe:
//
//   - "7, its key takes 3": a new holding or claim row is contended by nobody -
//     its key is only ever inserted under the same advisory lock - so what
//     matters is that the account lock its foreign key takes comes before any
//     step 5 or 6. When the insert conflicts and takes no foreign-key lock
//     (the holding or claim already exists), the release row that follows is
//     still safe: for a holding, an account deletion of that person first
//     needs this file's advisory lock and so cannot be holding the release row
//     by cascade; for a claim, the person holds nothing of these bytes and the
//     deletion never waits on this transaction at all.
//   - Entering at 3 (DeleteAccount, DeleteAccountErasing, EraseSoleHoldings,
//     ReleaseHolding, RemoveBookFileForEveryone):
//     they take only advisory locks of files somebody already holds or the
//     administrator is removing, and a path holding one of those waits on an
//     account only to add a NEW row for that person - which for a file they
//     hold conflicts and takes no lock. A key-share at 3 conflicts with nothing
//     but a deletion's FOR UPDATE, which then waits at step 3 holding nothing.
//     EraseSoleHoldings also takes the locks of bytes the person only claims,
//     where a drain holding one waits on the person's account explicitly; but
//     that wait is a key-share against the erasure's key-share, which never
//     conflict - and a key-share does not queue behind a deletion's FOR UPDATE
//     waiting at 3, so a deletion arriving in between closes no cycle either
//     (TestErasingAClaimRacesADrainAndADeletion).
//     A removal changes other people's rows - releases, claims, holdings and
//     the grants under them, note shares, queued jobs, citations - and takes
//     each of those people's accounts first, read before the file's lock and
//     checked again under it (a try that finds somebody new starts again).
//     So a deletion of any of them either finished before, or waits at 3.
//
// Account deletion's cascade runs through steps 7, 6, 5, 4 backwards, but
// only through that person's own rows, and never in a cycle: every other path
// that touches a row of theirs for a file either holds that file's advisory
// lock - which the deletion takes first for every file the person holds, and
// a person has at most one of a holding, a claim and a release per file - or
// holds the person's account (ReleaseHolding, a removal, an ingest's
// claimants), which the deletion is waiting for at 3.
//
// A block row (4) is contended only by a deletion's cascade, and never in a
// cycle: the deletion of the administrator making one waits at 3 for the
// removal to commit, and a block being lifted holds only the file's lock,
// which a deletion takes only for files the person holds - which blocked bytes
// never are - so whichever of the two reaches the row first simply finishes.
//
// What breaks it, each pinned by a deterministic test that drives the exact
// interleaving: deleting an account's claims before its account
// (TestAccountDeletionAndADrainTakeLocksInOneOrder), remembering a release
// after the file's lock without the account first
// (TestReleasingAndDeletingTheAccountTakeLocksInOneOrder), forgetting a
// release before the holding or claim that takes the account
// (TestForgettingAReleaseAndDeletingTheAccountTakeLocksInOneOrder), and
// recording a block after the file's lock without the administrator's account
// first (TestRemovingAndDeletingTheAdministratorTakeLocksInOneOrder), and
// removing a file somebody let go while their account is deleted, without
// their account first (TestRemovingAndDeletingSomebodyWhoLetItGoTakeLocksInOneOrder:
// the removal holds their release and wants their queued job, the cascade
// the other way round); the first three, and erasing somebody's books while
// their account is deleted, are run again against an erasing deletion
// (TestErasingDeletionTakesLocksInTheDocumentedOrder). An erasing deletion
// takes no advisory lock for bytes the person only claims: a drain holding
// that lock waits on their account, which the deletion holds FOR UPDATE - so
// those bytes are only owed, and paid after the commit under a fresh lock
// (DiscardPending). EraseSoleHoldings, holding only a key-share, may and does.
// Adding an
// unconditional account lock to addHolder would break it too: a path holding a
// file's advisory lock would wait on an account whose deletion waits for that
// lock.

// lockContentHash serialises everything that changes who holds one file, for
// the rest of the transaction.
//
// Keyed on the content hash rather than a row because the row may not exist
// yet: the race that matters is an upload recording a claim while the ingest
// that would drain it is creating the file. A lock on a row that does not exist
// is no lock at all.
func lockContentHash(ctx context.Context, tx pgx.Tx, hash string) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('book_file_holding:' || $1, 0))`,
		hash); err != nil {
		return fmt.Errorf("locking %s: %w", hash, mapError(err))
	}
	return nil
}

// holdMode is how a holding comes about, which decides what it may do.
type holdMode int

const (
	// holdDeliberate is a person adding the file themselves - uploading it,
	// or an administrator handing them an archived book. It may bring an
	// archived file back: they demonstrably have it, and a book that vanished
	// from their shelf the moment they added it would be both wrong and a
	// tell. It forgets an earlier release: adding it again is changing your
	// mind.
	holdDeliberate holdMode = iota + 1
	// holdByUploadJob is the extraction an upload queued, finishing later. It
	// may bring an archived file back too - the upload's own holding may not
	// have been recorded - but not past a release made since: somebody who
	// let the book go while it was still being extracted does not get it
	// back when the extraction ends.
	holdByUploadJob
	// holdByScan is a scan finding the file in the library directory. The
	// administrator who scanned it holds it, whoever else does, unless they
	// let it go; an archived file is never claimed by a rescan.
	holdByScan
)

// addHolder records that somebody holds a file, and reports whether they
// already did. A holding for an account that no longer exists is skipped, not
// an error: a job can outlive the person who queued it.
func addHolder(ctx context.Context, tx pgx.Tx, bookFileID, user uuid.UUID, filename string, mode holdMode) (bool, error) {
	if user == uuid.Nil {
		return false, nil
	}
	switch mode {
	case holdDeliberate, holdByUploadJob, holdByScan:
	default:
		return false, fmt.Errorf("recording a holding: invalid hold mode %d", mode)
	}
	// The holding first: its foreign key takes the account (key-share) before
	// the release row is touched, the lock order every path keeps. A
	// deliberate holding ignores the release, which it is about to forget.
	tag, err := tx.Exec(ctx, `
		INSERT INTO book_file_holders (book_file_id, user_id, original_filename)
		SELECT bf.id, u.id, $3
		  FROM book_files bf, users u
		 WHERE bf.id = $1 AND u.id = $2
		   AND (NOT $4 OR bf.archived_at IS NULL)
		   AND ($5 OR NOT EXISTS (SELECT 1 FROM released_holdings r
		                           WHERE r.user_id = u.id AND r.content_hash = bf.content_hash))
		ON CONFLICT (book_file_id, user_id) DO NOTHING`,
		bookFileID, user, filename, mode == holdByScan, mode == holdDeliberate)
	if err != nil {
		return false, fmt.Errorf("recording a holding: %w", mapError(err))
	}
	if mode == holdDeliberate {
		if deliberateHoldStep != nil {
			deliberateHoldStep()
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM released_holdings r
			 USING book_files bf
			 WHERE bf.id = $1 AND r.content_hash = bf.content_hash AND r.user_id = $2`,
			bookFileID, user); err != nil {
			return false, fmt.Errorf("forgetting a release: %w", mapError(err))
		}
	}
	if tag.RowsAffected() == 0 {
		var held bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM book_file_holders
			                WHERE book_file_id = $1 AND user_id = $2)`,
			bookFileID, user).Scan(&held); err != nil {
			return false, fmt.Errorf("checking a holding: %w", mapError(err))
		}
		return held, nil
	}
	return false, unarchive(ctx, tx, bookFileID)
}

// archiveIfUnheld archives a file that has ended up with no holder, so that
// nothing is ever on nobody's shelf and out of every administrator's sight.
// The name is unknown - whoever it was for is gone - so none is recorded.
func archiveIfUnheld(ctx context.Context, tx pgx.Tx, bookFileID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		UPDATE book_files bf SET archived_at = now()
		WHERE bf.id = $1 AND bf.archived_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM book_file_holders h WHERE h.book_file_id = bf.id)`,
		bookFileID); err != nil {
		return fmt.Errorf("archiving an unheld file: %w", mapError(err))
	}
	return nil
}

// unarchive returns a file that has gained a holder to ordinary life.
func unarchive(ctx context.Context, tx pgx.Tx, bookFileID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		UPDATE book_files
		SET archived_at = NULL, archived_owner = NULL,
		    -- path already says where the file is. Left set, this would
		    -- describe an archive copy that is no longer the file's, and be
		    -- reported as such if the book were ever archived again.
		    archived_path = NULL
		WHERE id = $1 AND archived_at IS NOT NULL`, bookFileID); err != nil {
		return fmt.Errorf("unarchiving: %w", mapError(err))
	}
	return nil
}

// drainClaims turns every claim waiting on this file's hash into a holding.
//
// Claims are only ever left by uploads, so they bring an archived file back
// exactly as an upload of a settled file would - and, like the extraction an
// upload queued (holdByUploadJob), not past a release: a claim whose person
// has let these bytes go since is deleted and gives nothing. Making a claim
// forgets the release (HoldUpload), and erasing withdraws the claim, so the
// two meet only if something writes a release without withdrawing the
// claim; this is where that is settled either way, and the rule is the same
// as for the extraction's own holder.
func drainClaims(ctx context.Context, tx pgx.Tx, bookFileID uuid.UUID, hash string) error {
	// The claimants' accounts BEFORE their claims (steps 3 and 6 of the lock
	// order above), as account deletion takes them: the account row, then its
	// claims by cascade. Taken the other way round - claim rows locked by the
	// DELETE below, then the holding's foreign key waiting on an account being
	// deleted - the two deadlock.
	if _, err := tx.Exec(ctx, `
		SELECT 1 FROM users u
		 WHERE u.id IN (SELECT c.user_id FROM holding_claims c WHERE c.content_hash = $1)
		   FOR KEY SHARE`, hash); err != nil {
		return fmt.Errorf("locking claimants: %w", mapError(err))
	}
	tag, err := tx.Exec(ctx, `
		WITH taken AS (
		    DELETE FROM holding_claims WHERE content_hash = $2
		    RETURNING user_id, original_filename, created_at
		)
		INSERT INTO book_file_holders (book_file_id, user_id, original_filename, created_at)
		SELECT $1, t.user_id, t.original_filename, t.created_at FROM taken t
		 WHERE NOT EXISTS (SELECT 1 FROM released_holdings r
		                    WHERE r.user_id = t.user_id AND r.content_hash = $2)
		ON CONFLICT (book_file_id, user_id) DO NOTHING`, bookFileID, hash)
	if err != nil {
		return fmt.Errorf("turning claims into holdings: %w", mapError(err))
	}
	if tag.RowsAffected() > 0 {
		return unarchive(ctx, tx, bookFileID)
	}
	return nil
}

// recordHoldings is the holding half of an ingest: whoever this ingest is for,
// and everybody who uploaded the same bytes while it was waiting.
func recordHoldings(ctx context.Context, tx pgx.Tx, bookFileID uuid.UUID, in IngestInput) error {
	mode := holdByScan
	if in.Uploaded {
		mode = holdByUploadJob
	}
	if _, err := addHolder(ctx, tx, bookFileID, in.OwnerID, in.OriginalFilename, mode); err != nil {
		return err
	}
	if betweenHoldingAndClaims != nil {
		betweenHoldingAndClaims()
	}
	if err := drainClaims(ctx, tx, bookFileID, in.ContentHash); err != nil {
		return err
	}
	return forgetUnshelvedUploader(ctx, tx, bookFileID, in)
}

// forgetUnshelvedUploader takes the uploader out of their own running upload
// extraction when it has just finished without putting the book on their
// shelf - they let the bytes go or had them erased meanwhile, or their
// account has gone. That job is about to be finished, and a finished job is
// not a record of anybody (scrubFinishedJobs): this one would otherwise keep
// the file's name and whom it was for, since the scrub that ran when they
// left, or were erased, skipped it while it was still running.
//
// Matched by its bytes and its uploader, which is the one live job per
// upload (worker.TargetForUpload); called with nothing running - a test,
// say - it changes nothing. Its row is otherwise written only by its own
// worker and by single-statement updates (cancel, reclaim) that hold
// nothing else, so taking it here, last, waits on nobody holding a lock this
// transaction has. A retry after this sees no owner and no name, and holds
// nothing for anybody - which is what it should do.
func forgetUnshelvedUploader(ctx context.Context, tx pgx.Tx, bookFileID uuid.UUID, in IngestInput) error {
	if !in.Uploaded || in.OwnerID == uuid.Nil {
		return nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE jobs
		   SET payload = payload - 'originalFilename' - 'ownerId',
		       requested_by = NULL
		 WHERE status = 'running' AND type = 'extract'
		   AND (payload ->> 'uploaded')::boolean IS TRUE
		   AND payload ->> 'contentHash' = $2
		   AND payload ->> 'ownerId' = $3::uuid::text
		   AND NOT EXISTS (SELECT 1 FROM book_file_holders
		                    WHERE book_file_id = $1 AND user_id = $3::uuid)`,
		bookFileID, in.ContentHash, in.OwnerID); err != nil {
		return fmt.Errorf("forgetting an uploader: %w", mapError(err))
	}
	return nil
}

// betweenHoldingAndClaims lets a test stop an ingest after it has taken the
// owner's account lock and before it takes the claims, to drive one exact
// interleaving with account deletion. Always nil outside tests.
var betweenHoldingAndClaims func()

// deliberateHoldStep stops a deliberate holding - an upload, or a claim on
// bytes not extracted yet - between its two statements, the release row and
// the holding or claim, for the same purpose. Always nil outside tests.
var deliberateHoldStep func()

// afterReleaseArchive stops ReleaseHolding once it holds the file's lock and
// before it writes anything else, for the same purpose. Always nil outside
// tests.
var afterReleaseArchive func()

// HoldInput is somebody adding a file they have just uploaded.
type HoldInput struct {
	ContentHash      string
	UserID           uuid.UUID
	OriginalFilename string
	// Settle, when set, runs once the holding or claim is written and before
	// it commits, under the bytes' lock: the upload's chance to make sure its
	// stored copy is in place (ingest.UploadStore.Settle). Nothing deleting
	// copies of these bytes can run between the two, and an error rolls the
	// holding back. Not called for blocked bytes. If the commit itself fails
	// after Settle has put a copy in place, that copy is left with no record
	// - the same stray an upload whose recording failed always left - and is
	// the one thing here nothing goes back for.
	Settle func() error
}

// HoldOutcome is what the upload path needs to answer the person.
type HoldOutcome struct {
	// AlreadyHeld is true when THIS person already held the file, or had
	// already uploaded it and it is still waiting to be extracted. It says
	// nothing about anybody else, and nothing derived from it may.
	AlreadyHeld bool
	// Status is the file's ingest status, or empty when no extraction has
	// created it yet.
	Status domain.IngestStatus
	// Missing is true when the file is on record but its copy has gone - a
	// scanned book whose directory was tidied away. The upload just stored is
	// then the copy that exists, and needs an extraction to become the
	// file's, even for somebody who already holds it.
	Missing bool
}

// HoldUpload records that somebody has uploaded a file.
//
// It does not care whether anybody else has the same bytes, and neither may
// its caller: the file goes on this person's shelf either way. When it is
// already on the installation the holding is written now; when it is not, or
// only an extraction queued for somebody else knows about it, a claim waits on
// the hash and that extraction - or this person's own - turns it into a
// holding.
func (p *Pool) HoldUpload(ctx context.Context, in HoldInput) (HoldOutcome, error) {
	var out HoldOutcome
	if in.UserID == uuid.Nil {
		return out, fmt.Errorf("holding an upload: %w", errNotFound)
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("holding an upload: %w", mapError(err))
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	if err := lockContentHash(ctx, tx, in.ContentHash); err != nil {
		return out, err
	}
	// Blocked bytes are refused before anything is written: no holding, no
	// claim, and no word on whether anybody else has them.
	if err := refuseBlocked(ctx, tx, in.ContentHash); err != nil {
		return out, err
	}

	var fileID uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT id, status, missing_since IS NOT NULL FROM book_files WHERE content_hash = $1`, in.ContentHash,
	).Scan(&fileID, &out.Status, &out.Missing)
	switch {
	case err == nil:
		out.AlreadyHeld, err = addHolder(ctx, tx, fileID, in.UserID, in.OriginalFilename, holdDeliberate)
		if err != nil {
			return out, err
		}
	case isNotFound(mapError(err)):
		out.Status = ""
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM holding_claims
			                WHERE content_hash = $1 AND user_id = $2)`,
			in.ContentHash, in.UserID).Scan(&out.AlreadyHeld); err != nil {
			return out, fmt.Errorf("checking a claim: %w", mapError(err))
		}
		// The claim first, for the same reason as a holding: its foreign key
		// takes the account before the release row is touched. Then the
		// release is forgotten - uploading it again is changing one's mind.
		if _, err := tx.Exec(ctx, `
			INSERT INTO holding_claims (content_hash, user_id, original_filename)
			VALUES ($1, $2, $3)
			ON CONFLICT (content_hash, user_id) DO NOTHING`,
			in.ContentHash, in.UserID, in.OriginalFilename); err != nil {
			return out, fmt.Errorf("recording a claim: %w", mapError(err))
		}
		if deliberateHoldStep != nil {
			deliberateHoldStep()
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM released_holdings WHERE content_hash = $1 AND user_id = $2`,
			in.ContentHash, in.UserID); err != nil {
			return out, fmt.Errorf("forgetting a release: %w", mapError(err))
		}
	default:
		return out, fmt.Errorf("looking up content hash: %w", mapError(err))
	}

	if in.Settle != nil {
		if err := in.Settle(); err != nil {
			return out, fmt.Errorf("holding an upload: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("holding an upload: %w", mapError(err))
	}
	return out, nil
}

// RemoveIfUnreferenced removes a copy of a file's bytes that its record no
// longer points at, under the file's lock and after checking that again.
//
// For an archive copy a fresh upload replaced. The check and the removal sit
// under the same lock the archive's own moves take, so a copy the archive has
// just made the record's home again is never the one removed. remove is the
// filesystem's half, passed in so this package stays about SQL.
func (p *Pool) RemoveIfUnreferenced(ctx context.Context, contentHash, path string, remove func(string) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("removing a replaced copy: %w", mapError(err))
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	if err := lockContentHash(ctx, tx, contentHash); err != nil {
		return err
	}
	// Every path a record of these bytes - or any record at all naming this
	// exact string - points at, compared as files rather than as strings: the
	// same copy written down under a differently spelled data directory is
	// still the record's copy, and must not be removed as a stray.
	rows, err := tx.Query(ctx, `
		SELECT p FROM book_files bf, unnest(ARRAY[bf.path, bf.archived_path]) AS p
		 WHERE (bf.content_hash = $1 OR bf.path = $2 OR bf.archived_path = $2)
		   AND p IS NOT NULL AND p <> ''`, contentHash, path)
	if err != nil {
		return fmt.Errorf("removing a replaced copy: %w", mapError(err))
	}
	referenced, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("removing a replaced copy: %w", mapError(err))
	}
	for _, r := range referenced {
		if ingest.SamePath(r, path) {
			return nil
		}
	}
	if err := remove(path); err != nil {
		return fmt.Errorf("removing a replaced copy: %w", err)
	}
	return tx.Commit(ctx)
}

// HeldStorage is what one person's shelf takes up on disk.
type HeldStorage struct {
	Files int
	Bytes int64
}

// HeldStorage reports how many files a person holds and their total size.
//
// Each holder is counted for the whole file. That is the answer to "how much
// have I added", which is the question a person asks; the installation's own
// disk use is the sum over distinct files and is smaller whenever two people
// hold the same one.
func (p *Pool) HeldStorage(ctx context.Context, user uuid.UUID) (HeldStorage, error) {
	var out HeldStorage
	if err := p.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(bf.size_bytes), 0)
		  FROM book_file_holders h
		  JOIN book_files bf ON bf.id = h.book_file_id
		 WHERE h.user_id = $1`, user).Scan(&out.Files, &out.Bytes); err != nil {
		return out, fmt.Errorf("measuring a shelf: %w", mapError(err))
	}
	return out, nil
}
