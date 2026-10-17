// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// ArchivedFile is a book whose owner's account was deleted.
type ArchivedFile struct {
	BookFileID uuid.UUID
	// Path is where the file is now. The caller decides whether that path is
	// Lexarii's to move: books scanned from the library directory are the
	// operator's own files and are never touched.
	Path          string
	Format        string
	ArchivedOwner string
	ArchivedAt    time.Time
	// ArchivedPath is where the file was moved to, or empty when it stayed.
	ArchivedPath string
	Title        string
}

// DeleteAccount removes a person and archives the books only they held.
//
// Everything personal goes with the account, by cascade: annotations, reading
// position, conversations, collections. That is what deleting an account has to
// mean, and it is not deferred - the six-month window in this file is about
// BOOKS, which are files rather than facts about a person.
//
// Books they were LENT need no handling at all. book_access cascades, so their
// access simply ends and the book is the owner's again, which is what somebody
// would expect and what the schema already did.
//
// Books they HELD lose a holder. One that somebody else also holds is simply
// theirs now: it is not archived, not moved, and nobody else's shelf changes.
// One that only they held is archived rather than orphaned: marked with the
// name that held it so an administrator can hand it on, and invisible in the
// meantime because readable_book_files requires a holder. Those files are
// returned so the caller can move the ones Lexarii stores; this function does
// not touch the filesystem, because a transaction that has already committed
// cannot un-move a file.
//
// The books they LENT stop being lent by them: a grant belongs to the holding
// that made it, and goes with it by cascade. A grant another holder made for
// the same file is that holder's, and stays.
func (p *Pool) DeleteAccount(ctx context.Context, actor, target uuid.UUID) ([]ArchivedFile, error) {
	if target == uuid.Nil {
		return nil, errNotFound
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("deleting account: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	username, err := lockAccountForDeletion(ctx, tx, actor, target)
	if err != nil {
		return nil, err
	}

	// Marked BEFORE the delete, because the name is about to stop existing and
	// the holdings are about to go by cascade.
	archived, err := archiveSoleHoldings(ctx, tx, target, username, uuid.Nil)
	if err != nil {
		return nil, err
	}

	// What their finished jobs say about them - the names of the files they
	// uploaded - before the cascade sets the requester to null and nothing
	// can find those jobs again.
	if err := scrubFinishedJobs(ctx, tx, target); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, target); err != nil {
		return nil, fmt.Errorf("deleting account: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("deleting account: %w", mapError(err))
	}
	return archived, nil
}

// lockAccountForDeletion checks that actor may delete target and that target
// is not the last administrator, and takes the account row FOR UPDATE: the
// prologue every way of deleting an account shares. It returns the username,
// which is about to stop existing.
func lockAccountForDeletion(ctx context.Context, tx pgx.Tx, actor, target uuid.UUID) (string, error) {
	// An administrator may delete anyone; anyone may delete themselves. Read
	// from the row rather than trusted from the caller, like every other
	// permission here.
	if actor != target {
		var isAdmin bool
		if err := tx.QueryRow(ctx,
			`SELECT is_admin FROM users WHERE id = $1`, actor).Scan(&isAdmin); err != nil {
			return "", mapError(err)
		}
		if !isAdmin {
			return "", fmt.Errorf("%w: only an administrator may delete another account",
				store.ErrForbidden)
		}
	}

	// The last administrator cannot be deleted.
	//
	// Not a courtesy: administration is how books are added, scanned, enriched
	// and reclaimed, so an installation with no administrator is one where the
	// archived books below can never be handed to anybody.
	var admins int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM users WHERE is_admin AND id <> $1`, target).Scan(&admins); err != nil {
		return "", mapError(err)
	}
	var targetIsAdmin bool
	var username string
	if err := tx.QueryRow(ctx,
		`SELECT is_admin, username FROM users WHERE id = $1`, target).Scan(&targetIsAdmin, &username); err != nil {
		return "", mapError(err)
	}
	if targetIsAdmin && admins == 0 {
		return "", fmt.Errorf("%w: this is the only administrator", store.ErrInvalidQuery)
	}

	// The account row, before anything else of theirs - the order every path
	// follows (see "Lock order" in holdings.go). A holding being added for
	// this person right now - an upload of theirs bringing an archived book
	// back, or an ingest making them a holder - holds a key-share lock on it,
	// so this waits for that to commit and then sees the holding; one that
	// starts after this blocks until the account is gone and fails. Without it
	// the holding could land after the archive decision and be removed by the
	// cascade, leaving a book nobody holds and nobody can claim. Their waiting
	// claims go by cascade, after this lock, as a drain takes them too.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, target); err != nil {
		return "", fmt.Errorf("locking the account: %w", mapError(err))
	}

	return username, nil
}

// archiveSoleHoldings archives the files only this person holds, as they stop
// holding them, and returns them so the caller can move the ones Lexarii
// stores. only limits it to one file; uuid.Nil means every file they hold.
//
// The one place that decides "the last holder is going", for account deletion
// and for somebody letting a single book go, so the two cannot come to
// disagree. It takes the per-file lock every change of holder takes, in a fixed
// order so two callers cannot deadlock: without it an upload of one of these
// files could add a holder between "nobody else holds this" and the archive,
// and the caller would move a file out from under somebody's shelf.
//
// The holdings themselves are the caller's to remove, in the same transaction.
func archiveSoleHoldings(ctx context.Context, tx pgx.Tx, user uuid.UUID, label string, only uuid.UUID) ([]ArchivedFile, error) {
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended('book_file_holding:' || bf.content_hash, 0))
		  FROM (SELECT bf.content_hash
		          FROM book_file_holders h
		          JOIN book_files bf ON bf.id = h.book_file_id
		         WHERE h.user_id = $1 AND ($2::uuid IS NULL OR bf.id = $2)
		         ORDER BY bf.content_hash) bf`, user, nullableUUID(only)); err != nil {
		return nil, fmt.Errorf("locking books: %w", mapError(err))
	}

	// Only the files nobody else holds: the rest are still somebody's books,
	// and are neither archived nor moved.
	rows, err := tx.Query(ctx, `
		UPDATE book_files bf
		SET archived_at = now(), archived_owner = $2
		WHERE ($3::uuid IS NULL OR bf.id = $3)
		  AND EXISTS (SELECT 1 FROM book_file_holders h
		               WHERE h.book_file_id = bf.id AND h.user_id = $1)
		  AND NOT EXISTS (SELECT 1 FROM book_file_holders h
		                   WHERE h.book_file_id = bf.id AND h.user_id <> $1)
		RETURNING bf.id, bf.path, bf.format::text`, user, label, nullableUUID(only))
	if err != nil {
		return nil, fmt.Errorf("archiving books: %w", mapError(err))
	}
	defer rows.Close()
	var archived []ArchivedFile
	for rows.Next() {
		var f ArchivedFile
		if err := rows.Scan(&f.BookFileID, &f.Path, &f.Format); err != nil {
			return nil, fmt.Errorf("scanning archived book: %w", err)
		}
		f.ArchivedOwner = label
		archived = append(archived, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("archiving books: %w", mapError(err))
	}
	return archived, nil
}

// ReleaseHolding takes one book off somebody's shelf: they stop holding the
// file.
//
// The same rule as deleting their account, for one file. When somebody else
// holds it too it simply loses a holder - nothing is archived or moved, and
// what the other holder lends stays lent. When they were the last holder it is
// archived against their name exactly as account deletion would, and returned
// so the caller can move it if Lexarii stores it.
//
// Their grants on it end with the holding, by cascade. So does sharing their
// notes on it with others, unless they can still read the book through
// somebody else's grant: notes nobody could reach from the book any more
// should not outlive the decision to let it go. The notes and the reading
// position THEMSELVES are kept - nothing here deletes what a person wrote, it
// is the same as when a lent book is withdrawn, and adding the same file again
// brings them back.
//
// Somebody who does not hold the file - including somebody it is merely lent
// to - is told it was not found, never forbidden.
func (p *Pool) ReleaseHolding(ctx context.Context, bookFileID, user uuid.UUID) ([]ArchivedFile, error) {
	if user == uuid.Nil || bookFileID == uuid.Nil {
		return nil, errNotFound
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("letting a book go: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The caller's account first, key-share: ReleaseHolding enters the lock
	// order at the account (see "Lock order" in holdings.go), as account
	// deletion does. Remembering the release below writes a row whose foreign
	// key needs that account; taken there instead, after the file's lock, it
	// waits on a deletion of the account that is itself waiting for the file.
	var hash, username string
	if err := tx.QueryRow(ctx, `
		SELECT bf.content_hash, u.username
		  FROM book_file_holders h
		  JOIN book_files bf ON bf.id = h.book_file_id
		  JOIN users u ON u.id = h.user_id
		 WHERE h.book_file_id = $1 AND h.user_id = $2
		   FOR KEY SHARE OF u`, bookFileID, user).Scan(&hash, &username); err != nil {
		return nil, mapError(err)
	}

	archived, err := archiveSoleHoldings(ctx, tx, user, username, bookFileID)
	if err != nil {
		return nil, err
	}
	if afterReleaseArchive != nil {
		afterReleaseArchive()
	}

	// The rest in lock order: the release row, the claims, the holding.
	//
	// Remembered, so that neither a scan nor their own upload's extraction
	// still in the queue hands the book straight back. The extraction itself
	// runs on: the file may be somebody else's too, and it may be the copy
	// that brings their missing book back. Only its holding for this person is
	// dropped. Uploading it again, or being handed it, forgets this.
	if _, err := tx.Exec(ctx, `
		INSERT INTO released_holdings (user_id, content_hash) VALUES ($1, $2)
		ON CONFLICT (user_id, content_hash) DO UPDATE SET released_at = now()`, user, hash); err != nil {
		return nil, fmt.Errorf("letting a book go: %w", mapError(err))
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM holding_claims WHERE content_hash = $1 AND user_id = $2`, hash, user); err != nil {
		return nil, fmt.Errorf("letting a book go: %w", mapError(err))
	}
	// Under the file's lock, so this is the answer that counts: a release
	// racing another release of the same holding finds nothing here and rolls
	// back, release row and all.
	tag, err := tx.Exec(ctx, `
		DELETE FROM book_file_holders WHERE book_file_id = $1 AND user_id = $2`, bookFileID, user)
	if err != nil {
		return nil, fmt.Errorf("letting a book go: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM annotation_access aa
		 WHERE aa.book_file_id = $1 AND aa.owner_id = $2
		   AND NOT EXISTS (SELECT 1 FROM readable_book_files rbf
		                    WHERE rbf.book_file_id = $1 AND rbf.user_id = $2)`,
		bookFileID, user); err != nil {
		return nil, fmt.Errorf("unsharing notes: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("letting a book go: %w", mapError(err))
	}
	return archived, nil
}

// MoveArchived moves an archived file into the archive, under the file's lock,
// and records where it went.
//
// The move is not part of the transaction that archived the file - that one
// has committed, and a rename cannot be rolled back with it - but it happens
// under the same per-file lock every change of holder takes, after checking
// again that the file is still archived and still where it was. Somebody
// uploading the same bytes in between brings the book back, and then it is not
// moved at all: moved anyway, the row would point at a file that was no longer
// where their upload put it. Their upload waits for the move instead of racing
// it.
//
// move is the filesystem's half, passed in so this package stays about SQL: it
// returns the new path, or "" when the file is not Lexarii's to move. The
// returned path is empty when nothing moved.
func (p *Pool) MoveArchived(ctx context.Context, bookFileID uuid.UUID, from string,
	move func(path string) (string, error)) (string, error) {

	tx, err := p.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("moving an archived book: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var hash string
	if err := tx.QueryRow(ctx, `SELECT content_hash FROM book_files WHERE id = $1`,
		bookFileID).Scan(&hash); err != nil {
		if isNotFound(mapError(err)) {
			return "", nil
		}
		return "", fmt.Errorf("moving an archived book: %w", mapError(err))
	}
	if err := lockContentHash(ctx, tx, hash); err != nil {
		return "", err
	}

	// Not a file removed for everyone meanwhile: its copy is being deleted,
	// and moved first it would be left behind in the archive.
	var still bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM book_files bf
		                WHERE bf.id = $1 AND bf.archived_at IS NOT NULL AND bf.path = $2
		                  AND NOT EXISTS (SELECT 1 FROM blocked_content b
		                                   WHERE b.content_hash = bf.content_hash))`,
		bookFileID, from).Scan(&still); err != nil {
		return "", fmt.Errorf("moving an archived book: %w", mapError(err))
	}
	if !still {
		return "", nil
	}

	to, err := move(from)
	if err != nil || to == "" {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE book_files SET path = $2, archived_path = $2 WHERE id = $1`,
		bookFileID, to); err != nil {
		return "", fmt.Errorf("recording the archive path: %w", mapError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		// The file has moved and the row does not say so. Saying where it is
		// lets whoever reads the log put it right.
		return "", fmt.Errorf("recording that %s moved to %s: %w", from, to, mapError(err))
	}
	return to, nil
}

// ArchivedBooks lists what is waiting to be claimed. Administrators only.
func (p *Pool) ArchivedBooks(ctx context.Context, admin uuid.UUID) ([]ArchivedFile, error) {
	if err := p.assertAdmin(ctx, admin); err != nil {
		return nil, err
	}

	rows, err := p.Query(ctx, `
		SELECT bf.id, bf.path, bf.format::text, COALESCE(bf.archived_owner, ''),
		       bf.archived_at, COALESCE(bf.archived_path, ''), w.title
		FROM book_files bf
		JOIN editions e ON e.id = bf.edition_id
		JOIN works w ON w.id = e.work_id
		WHERE bf.archived_at IS NOT NULL
		  -- A removed file is on the blocklist instead, until it is lifted.
		  AND NOT EXISTS (SELECT 1 FROM blocked_content b WHERE b.content_hash = bf.content_hash)
		  -- Nor an upload whose bytes are gone - a removal deleted them - since
		  -- there is nothing to hand on: only adding the same bytes again
		  -- brings that book back.
		  AND NOT (bf.origin = 'uploaded' AND bf.missing_since IS NOT NULL)
		ORDER BY bf.archived_at DESC, w.title`)
	if err != nil {
		return nil, fmt.Errorf("reading archived books: %w", mapError(err))
	}
	defer rows.Close()

	var out []ArchivedFile
	for rows.Next() {
		var f ArchivedFile
		if err := rows.Scan(&f.BookFileID, &f.Path, &f.Format, &f.ArchivedOwner,
			&f.ArchivedAt, &f.ArchivedPath, &f.Title); err != nil {
			return nil, fmt.Errorf("scanning archived book: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ClaimArchived gives an archived book a new holder.
//
// The book returns to an ordinary life: a holder, a place in a library, and no
// archive record. Whoever left it behind is not recorded on the row any more,
// because the row is no longer about them. Nothing they lent comes back: those
// grants were theirs and went with their account.
func (p *Pool) ClaimArchived(ctx context.Context, admin, bookFileID, newOwner uuid.UUID) error {
	if err := p.assertAdmin(ctx, admin); err != nil {
		return err
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("claiming archived book: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var hash, filename string
	err = tx.QueryRow(ctx, `
		SELECT bf.content_hash, bf.original_filename
		  FROM book_files bf
		 WHERE bf.id = $1 AND bf.archived_at IS NOT NULL
		   AND EXISTS (SELECT 1 FROM users WHERE id = $2)`, bookFileID, newOwner).Scan(&hash, &filename)
	if err != nil {
		return mapError(err)
	}
	if err := lockContentHash(ctx, tx, hash); err != nil {
		return err
	}
	// Again under the lock: an upload may have brought it back meanwhile, in
	// which case it is somebody's book and no longer this function's to hand
	// out.
	// Nor is a removed file, whose record is archived only so its holders'
	// marks outlive it: it is not in the archive while it is blocked, nor
	// after, if it was an upload whose bytes the removal deleted - handed on,
	// it would be a book with nothing to open.
	var stillArchived bool
	if err := tx.QueryRow(ctx, `
		SELECT bf.archived_at IS NOT NULL
		       AND NOT EXISTS (SELECT 1 FROM blocked_content b WHERE b.content_hash = bf.content_hash)
		       AND NOT (bf.origin = 'uploaded' AND bf.missing_since IS NOT NULL)
		  FROM book_files bf WHERE bf.id = $1`, bookFileID,
	).Scan(&stillArchived); err != nil {
		return fmt.Errorf("claiming archived book: %w", mapError(err))
	}
	if !stillArchived {
		return errNotFound
	}
	if _, err := addHolder(ctx, tx, bookFileID, newOwner, filename, holdDeliberate); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("claiming archived book: %w", mapError(err))
	}
	return nil
}

// ExpireArchived deletes the records of books archived longer ago than this.
//
// The ROW goes; the file does not. That is the whole point of the archive: a
// catalogue entry naming somebody who deleted their account is a record about a
// person and should not be kept forever, while the book itself is a file on a
// disk that costs nothing to leave alone and cannot be got back if deleted.
//
// The paths are returned so that whatever calls this can say where the files
// are - after the row is gone, nothing in the database knows.
//
// erase is for an installation that erases what departed people leave
// (LEXARII_DELETED_ACCOUNT_FILES=erase): there the file goes too. The
// expired records' stored copies and covers are owed the disk in the same
// statement (pending_discards), and DiscardPending deletes them - never one
// in the library directory.
func (p *Pool) ExpireArchived(ctx context.Context, olderThan time.Time, erase bool) ([]string, error) {
	rows, err := p.Query(ctx, `
		WITH gone AS (
			DELETE FROM book_files
			WHERE archived_at IS NOT NULL AND archived_at < $1
			RETURNING content_hash, path, archived_path, cover_path
		), owed AS (
			INSERT INTO pending_discards (content_hash, paths, covers)
			SELECT content_hash,
			       array_remove(ARRAY[path, NULLIF(archived_path, '')], NULL),
			       array_remove(ARRAY[NULLIF(cover_path, '')], NULL)
			  FROM gone WHERE $2
			 ORDER BY content_hash
			ON CONFLICT (content_hash) DO UPDATE
			   SET paths  = ARRAY(SELECT DISTINCT unnest(pending_discards.paths || EXCLUDED.paths)),
			       covers = ARRAY(SELECT DISTINCT unnest(pending_discards.covers || EXCLUDED.covers))
		)
		SELECT COALESCE(NULLIF(archived_path, ''), path) FROM gone`, olderThan, erase)
	if err != nil {
		return nil, fmt.Errorf("expiring archived books: %w", mapError(err))
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scanning expired book: %w", err)
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

// assertAdmin refuses anybody who is not an administrator.
func (p *Pool) assertAdmin(ctx context.Context, id uuid.UUID) error {
	var isAdmin bool
	if err := p.QueryRow(ctx, `SELECT is_admin FROM users WHERE id = $1`, id).Scan(&isAdmin); err != nil {
		return mapError(err)
	}
	if !isAdmin {
		return fmt.Errorf("%w: administrators only", store.ErrForbidden)
	}
	return nil
}
