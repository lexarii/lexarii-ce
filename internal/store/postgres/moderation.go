// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// An administrator can take a file off the installation for everybody, and
// keep it off: something this library must not keep. It is the one change to a
// holding that is not the holder's own, and it is an administrator's because
// it is about the installation rather than about any one shelf.
//
// Removing is letting go, for every holder at once, plus two things letting
// go does not do. The bytes are BLOCKED (blocked_content, by content hash), so
// no upload, scan, claim or queued extraction brings them back; and what was
// derived from them - passages, their vectors, the cover - is purged, along
// with every copy Lexarii itself stored. A copy in the library directory is
// never touched: that directory is read-only, and the block is what keeps a
// scan from finding it again.
//
// What stays is what letting go keeps: each person's own highlights, notes and
// reading position. They stay on the file's record, which is archived, marked
// missing and hidden from the archive while the block lasts. The record
// expires with the archive like any other, and if the block is lifted and the
// same bytes are added again, the marks come back with them. Deleting what
// somebody wrote is a separate act with its own controls, and an
// administrator's decision about a file is not that act.

// RemovalReasonMax bounds the reason an administrator records, in
// characters. Long enough for a sentence or a reference; not a document.
// RemovalReasonMaxBytes bounds it in bytes too, generously, so text in any
// script fits in the characters allowed.
const (
	RemovalReasonMax      = 1000
	RemovalReasonMaxBytes = 4000
)

// RemovalOptions are the choices an administrator makes when removing a file.
type RemovalOptions struct {
	// PurgeQuotes also erases the file's words that live on outside it: the
	// text each highlight on it quotes, and the excerpt each chat citation of
	// it keeps. The marks and citations themselves stay - their places in the
	// book, and every note - so nobody loses what they wrote. Off, those
	// quotations stay where they are, visible only to whoever made them.
	PurgeQuotes bool
}

// removalAttempts bounds how often a removal starts again because somebody
// new came to have something of the file while it was locking accounts.
const removalAttempts = 5

// errPeopleChanged starts a removal again: see removeOnce.
var errPeopleChanged = errors.New("the people a removal touches changed while it locked them")

// afterRemovalLock stops RemoveBookFileForEveryone once it holds the file's
// lock and before it writes anything, so a test can drive one interleaving
// exactly. Always nil outside tests.
var afterRemovalLock func()

// refuseBlocked fails with store.ErrBlocked when these bytes are blocked.
// Called under the file's lock by every path that would add a holding, a
// claim or a record for them, so a removal and an arrival cannot interleave.
func refuseBlocked(ctx context.Context, tx pgx.Tx, hash string) error {
	var blocked bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM blocked_content WHERE content_hash = $1)`, hash).Scan(&blocked); err != nil {
		return fmt.Errorf("checking the blocklist: %w", mapError(err))
	}
	if blocked {
		return fmt.Errorf("%s: %w", hash, store.ErrBlocked)
	}
	return nil
}

// IsBlocked reports whether these bytes are blocked. A plain read, for a scan
// to skip a blocked file before it costs an extraction; everything that writes
// checks again under the file's lock.
func (p *Pool) IsBlocked(ctx context.Context, contentHash string) (bool, error) {
	var blocked bool
	if err := p.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM blocked_content WHERE content_hash = $1)`, contentHash).Scan(&blocked); err != nil {
		return false, fmt.Errorf("checking the blocklist: %w", mapError(err))
	}
	return blocked, nil
}

// RemoveBookFileForEveryone takes a file off every shelf on the installation
// and blocks its bytes from coming back. It returns who held it - holders, and
// people whose upload of it was still waiting to be extracted - so the caller
// can tell them. Administrators only.
//
// In one transaction, under the file's lock and in the documented lock order
// (see "Lock order" in holdings.go): the block is recorded; every release
// marker, claim and holding for these bytes goes, and with the holdings every
// grant made from them; every share of somebody's notes on the file ends; the
// passages and their vectors are deleted, and extraction or chunking still
// queued for the file is cancelled; with opts.PurgeQuotes, the quoted text of
// highlights and chat citations is erased; and the record is archived and
// marked missing, so no sweep - indexing, enrichment, covers - picks it up
// again. Highlights, notes and reading positions stay on the record.
//
// It does not touch the filesystem. DiscardBlockedCopies does that once this
// has committed, because a transaction cannot un-delete a file.
//
// Removing a file that is already blocked removes whatever has crept back
// since - nothing should have - and keeps the original reason.
func (p *Pool) RemoveBookFileForEveryone(ctx context.Context, bookFileID, adminID uuid.UUID, reason string, opts RemovalOptions) ([]uuid.UUID, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, fmt.Errorf("%w: a reason is required", store.ErrInvalidQuery)
	}
	if utf8.RuneCountInString(reason) > RemovalReasonMax || len(reason) > RemovalReasonMaxBytes {
		return nil, fmt.Errorf("%w: the reason is longer than %d characters", store.ErrInvalidQuery, RemovalReasonMax)
	}
	if bookFileID == uuid.Nil || adminID == uuid.Nil {
		return nil, errNotFound
	}
	for attempt := 1; ; attempt++ {
		removedFor, err := p.removeOnce(ctx, bookFileID, adminID, reason, opts)
		if errors.Is(err, errPeopleChanged) {
			if attempt < removalAttempts {
				continue
			}
			return nil, fmt.Errorf("removing a book file: %w: %w", store.ErrBusy, err)
		}
		return removedFor, err
	}
}

// affectedPeople is everybody with a row a removal touches: who holds, has
// claimed or let go of the file, who lends or is lent it, whose notes on it are
// shared or shared with, whose queued work is for it, whose conversations cite
// it, and - when quotes are purged - whose marks are on it. $1 is the file, $2
// its hash, $3 whether quotes are purged.
const affectedPeople = `
	SELECT DISTINCT id FROM (
		SELECT user_id AS id FROM book_file_holders WHERE book_file_id = $1
		UNION SELECT user_id FROM holding_claims WHERE content_hash = $2
		UNION SELECT user_id FROM released_holdings WHERE content_hash = $2
		UNION SELECT user_id FROM book_access WHERE book_file_id = $1
		UNION SELECT granted_by FROM book_access WHERE book_file_id = $1
		UNION SELECT owner_id FROM annotation_access WHERE book_file_id = $1
		UNION SELECT viewer_id FROM annotation_access WHERE book_file_id = $1
		UNION SELECT requested_by FROM jobs
		       WHERE status = 'pending'
		         AND (target_id = $1 OR (type = 'extract' AND payload ->> 'contentHash' = $2))
		UNION SELECT cv.user_id FROM citations ct
		        JOIN messages m ON m.id = ct.message_id
		        JOIN conversations cv ON cv.id = m.conversation_id
		       WHERE ct.book_file_id = $1
		          OR ct.chunk_id IN (SELECT c.id FROM chunks c WHERE c.book_file_id = $1)
		UNION SELECT user_id FROM annotations WHERE $3 AND book_file_id = $1
	) people WHERE id IS NOT NULL`

// removeOnce is one try at a removal.
//
// A removal changes rows belonging to other people - their holdings, claims,
// releases, grants, queued jobs, citations - without holding the file in
// their name, and a deletion of one of those accounts cascades into the same
// rows in its own order. So it takes every such person's account first,
// key-share, as ReleaseHolding takes its caller's: a deletion of any of them
// either finishes before the removal starts, or waits at the account (step 3)
// holding nothing. The set is read before the file's lock, since an account
// lock taken after it could wait on a deletion that waits for the file; and
// read again under it. If somebody new appeared in between - a grant, a
// share, a chat citation, none of which take the file's lock - this try
// rolls back and the removal starts again with them included.
func (p *Pool) removeOnce(ctx context.Context, bookFileID, adminID uuid.UUID, reason string, opts RemovalOptions) ([]uuid.UUID, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("removing a book file: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The administrator's account first, key-share: this enters the lock
	// order at the account. The block row below has a foreign key to it;
	// taken there instead, after the file's lock, it would wait on a deletion
	// of this account that is itself waiting for the file. Read from the row
	// rather than trusted from the caller, like every other permission here.
	var isAdmin bool
	if err := tx.QueryRow(ctx, `
		SELECT is_admin FROM users WHERE id = $1 FOR KEY SHARE`, adminID).Scan(&isAdmin); err != nil {
		return nil, mapError(err)
	}
	if !isAdmin {
		return nil, fmt.Errorf("%w: administrators only", store.ErrForbidden)
	}

	var hash string
	if err := tx.QueryRow(ctx, `SELECT content_hash FROM book_files WHERE id = $1`,
		bookFileID).Scan(&hash); err != nil {
		return nil, mapError(err)
	}
	locked, err := idSet(ctx, tx, affectedPeople, bookFileID, hash, opts.PurgeQuotes)
	if err != nil {
		return nil, fmt.Errorf("finding who a removal touches: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(locked))
	for id := range locked {
		ids = append(ids, id)
	}
	if _, err := tx.Exec(ctx, `
		SELECT 1 FROM users WHERE id = ANY($1) ORDER BY id FOR KEY SHARE`, ids); err != nil {
		return nil, fmt.Errorf("locking the accounts a removal touches: %w", mapError(err))
	}

	if err := lockContentHash(ctx, tx, hash); err != nil {
		return nil, err
	}
	if afterRemovalLock != nil {
		afterRemovalLock()
	}
	// The row, under the lock, before anything that hangs off it. An
	// extraction or a chunking pass holding it finishes first, and then finds
	// the block.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM book_files WHERE id = $1 FOR UPDATE`, bookFileID); err != nil {
		return nil, fmt.Errorf("removing a book file: %w", mapError(err))
	}
	now, err := idSet(ctx, tx, affectedPeople, bookFileID, hash, opts.PurgeQuotes)
	if err != nil {
		return nil, fmt.Errorf("finding who a removal touches: %w", err)
	}
	for id := range now {
		if !locked[id] {
			return nil, errPeopleChanged
		}
	}

	// Removing again keeps the first reason, and says copies may remain: it
	// is how somebody asks for the clean-up to run again, and if the server
	// stops before it has, startup finishes it.
	if _, err := tx.Exec(ctx, `
		INSERT INTO blocked_content (content_hash, reason, blocked_by) VALUES ($1, $2, $3)
		ON CONFLICT (content_hash) DO UPDATE SET copies_discarded_at = NULL`, hash, reason, adminID); err != nil {
		return nil, fmt.Errorf("blocking %s: %w", hash, mapError(err))
	}

	// The rest in lock order: releases, claims, holdings.
	if _, err := tx.Exec(ctx, `DELETE FROM released_holdings WHERE content_hash = $1`, hash); err != nil {
		return nil, fmt.Errorf("forgetting releases: %w", mapError(err))
	}
	seen := map[uuid.UUID]bool{}
	var removedFor []uuid.UUID
	collect := func(sql string, arg any) error {
		rows, err := tx.Query(ctx, sql, arg)
		if err != nil {
			return mapError(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			if !seen[id] {
				seen[id] = true
				removedFor = append(removedFor, id)
			}
		}
		return rows.Err()
	}
	if err := collect(`DELETE FROM holding_claims WHERE content_hash = $1 RETURNING user_id`, hash); err != nil {
		return nil, fmt.Errorf("dropping claims: %w", err)
	}
	// Every grant goes with the holding that made it, by cascade.
	if err := collect(`DELETE FROM book_file_holders WHERE book_file_id = $1 RETURNING user_id`, bookFileID); err != nil {
		return nil, fmt.Errorf("removing holdings: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM annotation_access WHERE book_file_id = $1`, bookFileID); err != nil {
		return nil, fmt.Errorf("unsharing notes: %w", mapError(err))
	}
	if opts.PurgeQuotes {
		if err := purgeQuotes(ctx, tx, bookFileID); err != nil {
			return nil, err
		}
	}
	// The passages, and their vectors by cascade. A citation keeps the
	// excerpt it quoted, unless quotes were purged above, and loses its link.
	if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE book_file_id = $1`, bookFileID); err != nil {
		return nil, fmt.Errorf("deleting passages: %w", mapError(err))
	}
	// Work still waiting in the queue for this file has nothing left to do:
	// extracting its bytes, or chunking it. (Embedding is one job for the
	// whole backlog and finds no passages of this file to embed.) A job
	// already running finds the block when it writes.
	if _, err := tx.Exec(ctx, `
		UPDATE jobs SET status = 'cancelled', finished_at = now()
		 WHERE status = 'pending'
		   AND (target_id = $1 OR (type = 'extract' AND payload ->> 'contentHash' = $2))`,
		bookFileID, hash); err != nil {
		return nil, fmt.Errorf("cancelling queued work: %w", mapError(err))
	}
	// Archived, since nobody holds it, and missing, since as far as this
	// installation is concerned its bytes are gone: every sweep that would
	// index, enrich or draw a cover for it skips a missing file. The cover
	// itself is deleted with the stored copies, after commit.
	if _, err := tx.Exec(ctx, `
		UPDATE book_files
		   SET archived_at = COALESCE(archived_at, now()),
		       missing_since = COALESCE(missing_since, now())
		 WHERE id = $1`, bookFileID); err != nil {
		return nil, fmt.Errorf("archiving a removed file: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("removing a book file: %w", mapError(err))
	}
	return removedFor, nil
}

// purgeQuotes erases the file's words where they live on outside it: the
// text each highlight on it quotes, and the excerpt each chat citation of it
// keeps. Locators, notes, colours and the citations themselves stay.
//
// A highlight's edited time moves on, so a device that still has the old
// quote cannot win it back with an older edit, and its updated time moves by
// the trigger, so the next sync carries the blank to every device.
func purgeQuotes(ctx context.Context, tx pgx.Tx, bookFileID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		UPDATE annotations SET selected_text = '', edited_at = now()
		 WHERE book_file_id = $1 AND selected_text IS NOT NULL AND selected_text <> ''`,
		bookFileID); err != nil {
		return fmt.Errorf("erasing highlight quotes: %w", mapError(err))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE citations SET excerpt = NULL
		 WHERE book_file_id = $1 AND excerpt IS NOT NULL`, bookFileID); err != nil {
		return fmt.Errorf("erasing citation excerpts: %w", mapError(err))
	}
	return nil
}

// idSet runs a query of ids and returns them as a set.
func idSet(ctx context.Context, tx pgx.Tx, sql string, args ...any) (map[uuid.UUID]bool, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// StoredCopies is what may be on disk of a blocked file: the paths its record
// knows, and the stored name of its cover. Whoever discards them decides which
// are Lexarii's to delete; a path in the library directory never is.
type StoredCopies struct {
	// ContentHash names the bytes, so the discarder can find every copy
	// Lexarii stored of them.
	ContentHash string
	Paths       []string
	// Cover is relative to the cover directory, or empty.
	Cover string
	// Covers are more stored covers, by name: an erased record's, owed.
	Covers []string
	// SkipUploads leaves every copy in the upload directory alone, for bytes
	// an upload is on its way to read: only the archive's copies and the
	// covers go.
	SkipUploads bool
}

// DiscardBlockedCopies deletes what Lexarii stored of blocked bytes, under the
// file's lock and only while they are still blocked.
//
// The lock is the one every arrival takes, and each of them checks the block
// under it, so nothing can be adding these bytes while discard runs; and a
// block lifted meanwhile - after which somebody may have uploaded the file
// again, to the same content-addressed path - is noticed, and nothing is
// deleted. discard is the filesystem's half, passed in so this package stays
// about SQL; the paths it is given are the record's, and the caller adds the
// ones only it knows (a fresh upload's). The record is then told the cover
// and the archive copy are gone.
//
// Safe to repeat, which is how a removal whose clean-up failed is finished.
//
// The files are deleted inside the transaction, under the file's lock, on
// purpose. Outside it - read the copies, commit, delete - a block lifted in
// the gap and an upload of the same bytes would put a fresh copy at the very
// content-addressed path about to be deleted, and somebody's new upload would
// vanish. What it costs is a transaction open across a few unlinks. And if
// the commit fails after the files are gone, nothing is lost or wrong: the
// record still says copies remain, the next attempt finds them already gone,
// which counts as done, and marks them so.
func (p *Pool) DiscardBlockedCopies(ctx context.Context, contentHash string, discard func(StoredCopies) error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("discarding blocked copies: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockContentHash(ctx, tx, contentHash); err != nil {
		return err
	}
	var blocked bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM blocked_content WHERE content_hash = $1)`, contentHash).Scan(&blocked); err != nil {
		return fmt.Errorf("checking the blocklist: %w", mapError(err))
	}
	if !blocked {
		// Not blocked (any more): nothing here is ours to delete.
		return nil
	}

	copies := StoredCopies{ContentHash: contentHash}
	var path string
	var archived, cover *string
	err = tx.QueryRow(ctx, `
		SELECT path, archived_path, cover_path FROM book_files WHERE content_hash = $1`,
		contentHash).Scan(&path, &archived, &cover)
	switch {
	case err == nil:
		copies.Paths = append(copies.Paths, path)
		if a := deref(archived); a != "" && a != path {
			copies.Paths = append(copies.Paths, a)
		}
		copies.Cover = deref(cover)
	case isNotFound(mapError(err)):
		// Blocked bytes with no record: an upload refused, or a record that
		// has expired. Only the caller's own paths are left.
	default:
		return fmt.Errorf("discarding blocked copies: %w", mapError(err))
	}

	if derr := discard(copies); derr != nil {
		// Some may be gone and some not. The record keeps naming them all -
		// deleting one already gone is harmless - and the block says copies
		// remain, so the blocklist offers to finish and startup tries again.
		if err := markCopies(ctx, tx, contentHash, false); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("discarding blocked copies: %w", mapError(err))
		}
		return fmt.Errorf("discarding blocked copies: %w", derr)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE book_files SET cover_path = NULL, archived_path = NULL
		 WHERE content_hash = $1 AND (cover_path IS NOT NULL OR archived_path IS NOT NULL)`,
		contentHash); err != nil {
		return fmt.Errorf("discarding blocked copies: %w", mapError(err))
	}
	if err := markCopies(ctx, tx, contentHash, true); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// markCopies records whether every copy Lexarii stored of blocked bytes has
// been deleted.
func markCopies(ctx context.Context, tx pgx.Tx, contentHash string, gone bool) error {
	if _, err := tx.Exec(ctx, `
		UPDATE blocked_content
		   SET copies_discarded_at = CASE WHEN $2 THEN now() END
		 WHERE content_hash = $1`, contentHash, gone); err != nil {
		return fmt.Errorf("recording the copies of blocked bytes: %w", mapError(err))
	}
	return nil
}

// DiscardBlockedUpload deletes one copy of blocked bytes that its caller has
// just stored - an upload refused, or an extraction dropped - under the
// file's lock and only while the bytes are still blocked. When it cannot,
// the block says copies remain, so the blocklist offers to finish and
// startup tries again; when it can, nothing else changes.
func (p *Pool) DiscardBlockedUpload(ctx context.Context, contentHash string, remove func() error) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("discarding a blocked upload: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockContentHash(ctx, tx, contentHash); err != nil {
		return err
	}
	if err := refuseBlocked(ctx, tx, contentHash); err == nil {
		return nil // lifted meanwhile: the copy may be somebody's upload now
	} else if !errors.Is(err, store.ErrBlocked) {
		return err
	}
	if rerr := remove(); rerr != nil {
		if err := markCopies(ctx, tx, contentHash, false); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("discarding a blocked upload: %w", mapError(err))
		}
		return fmt.Errorf("discarding a blocked upload: %w", rerr)
	}
	return tx.Commit(ctx)
}

// BlocksWithCopies lists the blocked bytes some copy of which may still be on
// disk: a deletion that failed, or never ran because the server stopped
// first. Startup finishes them.
func (p *Pool) BlocksWithCopies(ctx context.Context) ([]string, error) {
	rows, err := p.Query(ctx, `
		SELECT content_hash FROM blocked_content WHERE copies_discarded_at IS NULL ORDER BY blocked_at`)
	if err != nil {
		return nil, fmt.Errorf("finding blocked copies: %w", mapError(err))
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// BlockedFile is one entry on the blocklist.
type BlockedFile struct {
	ContentHash string
	Reason      string
	BlockedAt   time.Time
	// BlockedBy is the administrator's name, or empty once their account is
	// gone.
	BlockedBy string
	// Title is the title of the record the bytes had, while it lasts - it
	// expires with the archive - so an administrator can tell entries apart.
	Title string
	// CopiesRemain is true until every copy Lexarii stored has been deleted.
	CopiesRemain bool
}

// BlockedFiles lists the blocklist, newest first. Administrators only.
func (p *Pool) BlockedFiles(ctx context.Context, admin uuid.UUID) ([]BlockedFile, error) {
	if err := p.assertAdmin(ctx, admin); err != nil {
		return nil, err
	}
	rows, err := p.Query(ctx, `
		SELECT b.content_hash, b.reason, b.blocked_at, COALESCE(u.username, ''),
		       COALESCE(w.title, ''), b.copies_discarded_at IS NULL
		  FROM blocked_content b
		  LEFT JOIN users u ON u.id = b.blocked_by
		  LEFT JOIN book_files bf ON bf.content_hash = b.content_hash
		  LEFT JOIN editions e ON e.id = bf.edition_id
		  LEFT JOIN works w ON w.id = e.work_id
		 ORDER BY b.blocked_at DESC, b.content_hash`)
	if err != nil {
		return nil, fmt.Errorf("reading the blocklist: %w", mapError(err))
	}
	defer rows.Close()

	var out []BlockedFile
	for rows.Next() {
		var f BlockedFile
		if err := rows.Scan(&f.ContentHash, &f.Reason, &f.BlockedAt, &f.BlockedBy, &f.Title, &f.CopiesRemain); err != nil {
			return nil, fmt.Errorf("scanning the blocklist: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Unblock lifts a block. Administrators only.
//
// Nothing comes back by itself; the file's record stays archived and missing.
// For an upload, whose bytes the removal deleted, only adding the same bytes
// again brings the book back - with every mark on it - and until then the
// record stays out of the archive, since there is nothing to hand on. For a
// file in the library directory, which was never deleted, the next scan that
// finds it marks it present, and the archive offers it to an administrator
// again; a scan never puts an archived book on a shelf by itself.
func (p *Pool) Unblock(ctx context.Context, admin uuid.UUID, contentHash string) error {
	if err := p.assertAdmin(ctx, admin); err != nil {
		return err
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("unblocking: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Under the file's lock, as the block was made: an arrival checking the
	// block sees it either lifted or not, never half-way.
	if err := lockContentHash(ctx, tx, contentHash); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM blocked_content WHERE content_hash = $1`, contentHash)
	if err != nil {
		return fmt.Errorf("unblocking: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return tx.Commit(ctx)
}
