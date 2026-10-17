// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// Erasing is the other answer to "what happens to the books only this person
// held". The default keeps them: archived against the leaver's name, moved
// into the archive if Lexarii stores them, for an administrator to hand on
// (accounts.go). An installation that must not keep what a person added after
// they are gone - a larger one with a data-retention rule, say - erases them
// instead (LEXARII_DELETED_ACCOUNT_FILES=erase).
//
// Erasing a file deletes its record, and with it everything derived from it -
// passages and their vectors, the access log, the grants - and then the cover
// and the copies Lexarii itself stored, in its upload directory or its
// archive. A copy in the library directory is never touched: that directory
// is read-only, and the next scan that finds the file sees it as new (and a
// scan never puts a book on anybody's shelf whose holder let it go).
// Highlights, notes and reading positions on the file go with the record,
// other people's included: they are what an archived record would have taken
// with it when it expired, and there is no longer a book for them to be on.
//
// A file somebody else also holds simply loses a holder, as it always did. So,
// in effect, does one somebody else is still waiting to hold: an upload of the
// same bytes whose extraction has not run yet leaves a claim on them, and the
// stored copy is theirs as much as the leaver's. That file is archived with no
// name and kept; their extraction makes it theirs, and if it never does the
// record expires with the archive, and its copy with it.
//
// The records go in one transaction; the files cannot, because a rollback
// cannot bring a deleted file back. So the transaction records what it owes
// the disk - pending_discards, a row per content hash - and the copies are
// deleted afterwards by DiscardPending: straight after the erasure, at
// startup, and periodically, each under the bytes' lock and only while
// nothing has come to want them. A crash or a failed disk delays the
// deletion; it never loses track of it.

// Erasure is what erasing a person's books did.
type Erasure struct {
	// Erased are the files whose records were deleted. Their stored copies
	// are owed (Owed) until DiscardPending deletes them.
	Erased []uuid.UUID
	// Kept counts files only this person held that somebody else is still
	// waiting to hold (an upload not yet extracted): kept, not erased.
	Kept int
	// Withdrawn counts this person's own uploads that had not been extracted
	// yet - their claims on the bytes - withdrawn by an erasure that keeps
	// the account (EraseSoleHoldings): the claim gone, its extraction
	// cancelled if still queued, the release remembered.
	Withdrawn int
	// Owed are the content hashes whose stored copies are now owed the disk:
	// the erased files', and - when the account went - the person's own
	// uploads that were still waiting to be extracted.
	Owed []string
	// Editions are the editions the erased files belonged to, for
	// DeleteEmptyEditions once the erasure has committed.
	Editions []uuid.UUID
}

func (e *Erasure) add(o Erasure) {
	e.Erased = append(e.Erased, o.Erased...)
	e.Kept += o.Kept
	e.Withdrawn += o.Withdrawn
	e.Owed = append(e.Owed, o.Owed...)
	e.Editions = append(e.Editions, o.Editions...)
}

// EraseBatch bounds how many files one transaction of EraseSoleHoldings
// erases. Every one is an advisory lock held to the end of the transaction,
// and the lock table is shared and fixed in size: on a default server a few
// tens of thousands in one transaction exhaust it.
const EraseBatch = 1000

// eraseBatchSize is EraseBatch, smaller in a test.
var eraseBatchSize = EraseBatch

// afterEraseLock stops an erasure once it holds the files' locks and before
// it writes anything, so a test can drive one interleaving exactly. Always nil
// outside tests.
var afterEraseLock func()

// DeleteAccountErasing deletes a person as DeleteAccount does, but erases the
// books only they held instead of archiving them, and owes the disk their
// stored copies (see DiscardPending).
//
// The same permission and the same last-administrator rule as DeleteAccount,
// and the same lock order (see "Lock order" in holdings.go): the account
// first, then every file they hold. Like DeleteAccount it is one transaction,
// so it takes one advisory lock per file the person holds, shared or not: a
// shelf of tens of thousands of files needs a larger max_locks_per_transaction,
// or erasing their books first with EraseSoleHoldings, which goes in batches
// and leaves the deletion only the files they share.
func (p *Pool) DeleteAccountErasing(ctx context.Context, actor, target uuid.UUID) (Erasure, error) {
	if target == uuid.Nil {
		return Erasure{}, errNotFound
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return Erasure{}, fmt.Errorf("deleting account: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := lockAccountForDeletion(ctx, tx, actor, target); err != nil {
		return Erasure{}, err
	}

	held, err := heldHashes(ctx, tx, target, "", 0)
	if err != nil {
		return Erasure{}, err
	}
	out, err := eraseSoleHoldings(ctx, tx, target, held, true)
	if err != nil {
		return Erasure{}, err
	}
	// Their finished jobs, the uploads just cancelled among them, as
	// DeleteAccount does.
	if err := scrubFinishedJobs(ctx, tx, target); err != nil {
		return Erasure{}, err
	}

	if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, target); err != nil {
		return Erasure{}, fmt.Errorf("deleting account: %w", mapError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return Erasure{}, fmt.Errorf("deleting account: %w", mapError(err))
	}
	return out, nil
}

// EraseSoleHoldings erases the books only this person holds, now, without
// deleting their account, and owes the disk their stored copies (see
// DiscardPending). Administrators only.
//
// For an installation whose retention rule reaches a person's books before it
// reaches the person. Exactly what erasing on account deletion does to those
// files, and nothing more: books they share with another holder stay on both
// shelves, and one somebody else is waiting to hold is left alone. Their own
// notes and reading positions on the erased files go with the files. The
// release is remembered, as when somebody lets a book go, so a scan of a
// library directory that still has the file does not hand it straight back.
//
// Books still being extracted for them are withdrawn too, as deleting the
// account withdraws them: their claim on the bytes goes, a queued extraction
// of their upload is cancelled and its stored copy owed, and the release is
// remembered - so neither somebody else's extraction of the same bytes nor
// their own already running puts the book on their shelf afterwards. One
// already running still finishes; if nobody else holds the bytes its record
// is archived with no name, and expires with the archive.
//
// Their finished jobs are scrubbed of what they say about them - the names
// of the files they uploaded, and that they asked (scrubFinishedJobs) - as
// deleting the account does; a finished job is never run again.
//
// In batches of EraseBatch files, a transaction each, in content-hash order:
// a person with a very large shelf would otherwise hold more advisory locks
// than the server has room for. Each batch stands on its own, so a failure
// part-way leaves the batches before it erased; erasing again finishes. The
// jobs are scrubbed last, once every batch has cancelled what it cancels.
func (p *Pool) EraseSoleHoldings(ctx context.Context, admin, target uuid.UUID) (Erasure, error) {
	if target == uuid.Nil {
		return Erasure{}, errNotFound
	}
	var out Erasure
	after := ""
	for {
		batch, last, err := p.eraseBatch(ctx, admin, target, after)
		if err != nil {
			return out, err
		}
		out.add(batch)
		if last == "" {
			break
		}
		after = last
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("erasing books: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := scrubFinishedJobs(ctx, tx, target); err != nil {
		return out, err
	}
	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("erasing books: %w", mapError(err))
	}
	return out, nil
}

// eraseBatch is one transaction of EraseSoleHoldings: the next EraseBatch
// hashes the person holds after the given one. It reports the last hash it
// considered, or "" when there were no more.
func (p *Pool) eraseBatch(ctx context.Context, admin, target uuid.UUID, after string) (Erasure, string, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return Erasure{}, "", fmt.Errorf("erasing books: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var isAdmin bool
	if err := tx.QueryRow(ctx, `SELECT is_admin FROM users WHERE id = $1`, admin).Scan(&isAdmin); err != nil {
		return Erasure{}, "", mapError(err)
	}
	if !isAdmin {
		return Erasure{}, "", fmt.Errorf("%w: administrators only", store.ErrForbidden)
	}
	// The person's account first, key-share, as ReleaseHolding takes its
	// caller's: this enters the lock order at the account, and the release
	// rows written below have a foreign key to it. A deletion of the account
	// either finished before - and this is not found - or waits here.
	if err := tx.QueryRow(ctx, `SELECT 1 FROM users WHERE id = $1 FOR KEY SHARE`, target).Scan(new(int)); err != nil {
		return Erasure{}, "", mapError(err)
	}

	held, err := erasableHashes(ctx, tx, target, after, eraseBatchSize)
	if err != nil {
		return Erasure{}, "", err
	}
	if len(held) == 0 {
		return Erasure{}, "", nil
	}
	out, err := eraseSoleHoldings(ctx, tx, target, held, false)
	if err != nil {
		return Erasure{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return Erasure{}, "", fmt.Errorf("erasing books: %w", mapError(err))
	}
	last := held[len(held)-1]
	if len(held) < eraseBatchSize {
		last = ""
	}
	return out, last, nil
}

// heldHashes lists, in order, the content hashes of the files a person holds
// after the given one, at most limit of them (0: all).
func heldHashes(ctx context.Context, tx pgx.Tx, user uuid.UUID, after string, limit int) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT bf.content_hash
		  FROM book_file_holders h
		  JOIN book_files bf ON bf.id = h.book_file_id
		 WHERE h.user_id = $1 AND bf.content_hash > $2
		 ORDER BY bf.content_hash
		 LIMIT NULLIF($3, 0)`, user, after, limit)
	if err != nil {
		return nil, fmt.Errorf("finding books: %w", mapError(err))
	}
	hashes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("finding books: %w", mapError(err))
	}
	return hashes, nil
}

// erasableHashes lists, in order, the content hashes after the given one -
// at most limit of them (0: all) - that a person holds a file of or has a
// claim on: everything EraseSoleHoldings may take from them.
func erasableHashes(ctx context.Context, tx pgx.Tx, user uuid.UUID, after string, limit int) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT h FROM (
		    SELECT bf.content_hash AS h
		      FROM book_file_holders fh
		      JOIN book_files bf ON bf.id = fh.book_file_id
		     WHERE fh.user_id = $1
		    UNION
		    SELECT content_hash FROM holding_claims WHERE user_id = $1
		) t
		 WHERE h > $2
		 ORDER BY h
		 LIMIT NULLIF($3, 0)`, user, after, limit)
	if err != nil {
		return nil, fmt.Errorf("finding books: %w", mapError(err))
	}
	hashes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("finding books: %w", mapError(err))
	}
	return hashes, nil
}

// erasedFile is one record about to be erased.
type erasedFile struct {
	id        uuid.UUID
	hash      string
	path      string
	archived  *string
	cover     *string
	editionID uuid.UUID
}

// eraseSoleHoldings erases, of the files with these content hashes, the ones
// only this person holds and nobody else is waiting to hold, and records what
// that owes the disk. leaving says the account is being deleted in the same
// transaction: then a file somebody else is waiting for is archived with no
// name, since its holder is going, and the person's own uploads still
// waiting to be extracted are cancelled and owed too; otherwise such a file is
// left as it is, every erased file's release is remembered, and the person's
// claims among the hashes - their uploads not extracted yet - are withdrawn:
// the claim deleted, a queued extraction cancelled, the release remembered,
// the stored copy owed.
//
// Locks, in the documented order: the given hashes (1, sorted) - the only
// files erased, so a holding committed after the caller listed them is left
// alone rather than erased without its lock; the files' rows (2), deleted
// (8), which takes their holdings and grants (7) by cascade; release rows
// (5) and then the person's claims (6) only when not leaving - after the
// account's key-share the caller took at 3, as ReleaseHolding writes its
// own; and last the pending_discards rows (9), which nothing holds while
// waiting on anything else.
func eraseSoleHoldings(ctx context.Context, tx pgx.Tx, user uuid.UUID, hashes []string, leaving bool) (Erasure, error) {
	if _, err := tx.Exec(ctx, `
		SELECT pg_advisory_xact_lock(hashtextextended('book_file_holding:' || t.h, 0))
		  FROM (SELECT h FROM unnest($1::text[]) AS h ORDER BY h) t`, hashes); err != nil {
		return Erasure{}, fmt.Errorf("locking books: %w", mapError(err))
	}
	if afterEraseLock != nil {
		afterEraseLock()
	}

	var out Erasure
	var pendingUploads, claimed []string
	debts := map[string]StoredCopies{}
	if !leaving {
		// Their uploads not extracted yet, among these bytes. Read under the
		// hashes' locks, which every writer of a claim holds - but account
		// deletion's cascade, which waits for this transaction at the account.
		rows, err := tx.Query(ctx, `
			SELECT content_hash FROM holding_claims
			 WHERE user_id = $1 AND content_hash = ANY($2::text[])
			 ORDER BY content_hash`, user, hashes)
		if err != nil {
			return Erasure{}, fmt.Errorf("finding uploads on their way: %w", mapError(err))
		}
		if claimed, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return Erasure{}, fmt.Errorf("finding uploads on their way: %w", mapError(err))
		}
	}
	if leaving {
		// Their own uploads still waiting to be extracted. The claims go by
		// cascade with the account; the extractions would otherwise run on
		// and leave a nameless archived copy of what they added. Only
		// pending uploads of theirs - not a scan they queued, whose files are
		// in the library directory - and only after the files' locks, which
		// every other path takes before touching a job. A running extraction
		// finishes, its record is archived with no name because nobody holds
		// it, and it expires with the archive like any other.
		rows, err := tx.Query(ctx, `
			UPDATE jobs SET status = 'cancelled', finished_at = now()
			 WHERE status = 'pending' AND type = 'extract' AND requested_by = $1
			   AND (payload ->> 'uploaded')::boolean IS TRUE
			   AND payload ->> 'contentHash' IN (SELECT content_hash FROM holding_claims WHERE user_id = $1)
			RETURNING payload ->> 'contentHash'`, user)
		if err != nil {
			return Erasure{}, fmt.Errorf("cancelling pending uploads: %w", mapError(err))
		}
		if pendingUploads, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return Erasure{}, fmt.Errorf("cancelling pending uploads: %w", mapError(err))
		}

		// Only this person holds them, but somebody else's upload of the
		// same bytes is waiting to be extracted: the stored copy is theirs
		// too. Archived, nameless, until that extraction makes them its
		// uploader's.
		tag, err := tx.Exec(ctx, `
			UPDATE book_files bf
			   SET archived_at = now(), archived_owner = NULL
			 WHERE bf.content_hash = ANY($2::text[])
			   AND EXISTS (SELECT 1 FROM book_file_holders h
			                WHERE h.book_file_id = bf.id AND h.user_id = $1)
			   AND NOT EXISTS (SELECT 1 FROM book_file_holders h
			                    WHERE h.book_file_id = bf.id AND h.user_id <> $1)
			   AND EXISTS (SELECT 1 FROM holding_claims c
			                WHERE c.content_hash = bf.content_hash AND c.user_id <> $1)`, user, hashes)
		if err != nil {
			return Erasure{}, fmt.Errorf("archiving awaited books: %w", mapError(err))
		}
		out.Kept = int(tag.RowsAffected())
	} else if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM book_files bf
		 WHERE bf.content_hash = ANY($2::text[])
		   AND EXISTS (SELECT 1 FROM book_file_holders h
		                WHERE h.book_file_id = bf.id AND h.user_id = $1)
		   AND NOT EXISTS (SELECT 1 FROM book_file_holders h
		                    WHERE h.book_file_id = bf.id AND h.user_id <> $1)
		   AND EXISTS (SELECT 1 FROM holding_claims c
		                WHERE c.content_hash = bf.content_hash AND c.user_id <> $1)`,
		user, hashes).Scan(&out.Kept); err != nil {
		return Erasure{}, fmt.Errorf("counting awaited books: %w", mapError(err))
	}

	rows, err := tx.Query(ctx, `
		SELECT bf.id, bf.content_hash, bf.path, bf.archived_path, bf.cover_path, bf.edition_id
		  FROM book_files bf
		 WHERE bf.content_hash = ANY($2::text[])
		   AND EXISTS (SELECT 1 FROM book_file_holders h
		                WHERE h.book_file_id = bf.id AND h.user_id = $1)
		   AND NOT EXISTS (SELECT 1 FROM book_file_holders h
		                    WHERE h.book_file_id = bf.id AND h.user_id <> $1)
		   AND NOT EXISTS (SELECT 1 FROM holding_claims c
		                    WHERE c.content_hash = bf.content_hash AND c.user_id <> $1)
		 ORDER BY bf.content_hash
		   FOR UPDATE OF bf`, user, hashes)
	if err != nil {
		return Erasure{}, fmt.Errorf("finding books to erase: %w", mapError(err))
	}
	files, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (erasedFile, error) {
		var f erasedFile
		err := row.Scan(&f.id, &f.hash, &f.path, &f.archived, &f.cover, &f.editionID)
		return f, err
	})
	if err != nil {
		return Erasure{}, fmt.Errorf("finding books to erase: %w", mapError(err))
	}

	ids := make([]uuid.UUID, 0, len(files))
	erasedHashes := make([]string, 0, len(files))
	editions := map[uuid.UUID]bool{}
	for _, f := range files {
		ids = append(ids, f.id)
		erasedHashes = append(erasedHashes, f.hash)
		if !editions[f.editionID] {
			editions[f.editionID] = true
			out.Editions = append(out.Editions, f.editionID)
		}
	}
	// Every hash taken from them, once each: the files erased, and the
	// uploads withdrawn. Those should never share a hash - a claim waits
	// only on bytes with no record - but nothing here depends on it.
	taken := uniqueStrings(append(append([]string{}, erasedHashes...), claimed...))

	if !leaving && len(taken) > 0 {
		// Remembered, as a release is: a scan of a library directory that
		// still has one of these does not put it back on their shelf, and
		// neither does an extraction - theirs, already running, or somebody
		// else's of the same bytes draining claims.
		if _, err := tx.Exec(ctx, `
			INSERT INTO released_holdings (user_id, content_hash)
			SELECT $1, unnest($2::text[])
			ON CONFLICT (user_id, content_hash) DO UPDATE SET released_at = now()`,
			user, taken); err != nil {
			return Erasure{}, fmt.Errorf("remembering erased books: %w", mapError(err))
		}
	}
	if len(claimed) > 0 {
		// After the releases (5 before 6), as ReleaseHolding does.
		tag, err := tx.Exec(ctx, `
			DELETE FROM holding_claims WHERE user_id = $1 AND content_hash = ANY($2::text[])`,
			user, claimed)
		if err != nil {
			return Erasure{}, fmt.Errorf("withdrawing uploads on their way: %w", mapError(err))
		}
		out.Withdrawn = int(tag.RowsAffected())
	}

	if len(taken) > 0 {
		// Work still waiting for these files has nothing left to do:
		// chunking them, or extracting an upload of theirs - again, or for
		// the first time when it was withdrawn. A scan's extraction of the
		// same bytes is left alone - that file is in a library directory,
		// and finding it there again is what a scan is for - and so is
		// anybody else's upload of them. Their works' enrichment is left to
		// find nothing, since the work may have other files.
		if _, err := tx.Exec(ctx, `
			UPDATE jobs SET status = 'cancelled', finished_at = now()
			 WHERE status = 'pending'
			   AND (target_id = ANY($1)
			        OR (type = 'extract' AND requested_by = $3
			            AND (payload ->> 'uploaded')::boolean IS TRUE
			            AND payload ->> 'contentHash' = ANY($2::text[])))`,
			ids, taken, user); err != nil {
			return Erasure{}, fmt.Errorf("cancelling queued work: %w", mapError(err))
		}
	}

	if len(files) > 0 {
		// The records, and by cascade everything hanging off them: holdings
		// and the grants made from them, passages and vectors, marks and
		// positions, the access log. A chat citation keeps its excerpt and
		// loses its link.
		if _, err := tx.Exec(ctx, `DELETE FROM book_files WHERE id = ANY($1)`, ids); err != nil {
			return Erasure{}, fmt.Errorf("erasing books: %w", mapError(err))
		}

		for _, f := range files {
			paths := []string{f.path}
			if a := deref(f.archived); a != "" && a != f.path {
				paths = append(paths, a)
			}
			var covers []string
			if c := deref(f.cover); c != "" {
				covers = append(covers, c)
			}
			debts[f.hash] = StoredCopies{ContentHash: f.hash, Paths: paths, Covers: covers}
			out.Erased = append(out.Erased, f.id)
		}
	}
	// The pending uploads, and the withdrawn ones, owed by hash alone: their
	// copies are wherever Lexarii stores uploads of those bytes. A withdrawn
	// upload still being extracted, or one somebody else made of the same
	// bytes, keeps its copy until it is done with it (DiscardPending).
	for _, hash := range append(pendingUploads, claimed...) {
		if _, ok := debts[hash]; !ok {
			debts[hash] = StoredCopies{ContentHash: hash}
		}
	}
	// Owed in content-hash order, like every other set of rows here, so two
	// erasures owing some of the same hashes - the pending upload of one is
	// a file of the other - take those rows in one order.
	for hash := range debts {
		out.Owed = append(out.Owed, hash)
	}
	sort.Strings(out.Owed)
	for _, hash := range out.Owed {
		if err := owe(ctx, tx, hash, debts[hash].Paths, debts[hash].Covers); err != nil {
			return Erasure{}, err
		}
	}
	return out, nil
}

// uniqueStrings returns the distinct strings of in, sorted.
func uniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string{}, in...)
	sort.Strings(out)
	n := 1
	for _, s := range out[1:] {
		if s != out[n-1] {
			out[n] = s
			n++
		}
	}
	return out[:n]
}

// owe records that the stored copies of these bytes are owed the disk, adding
// to whatever is owed already.
func owe(ctx context.Context, tx pgx.Tx, hash string, paths, covers []string) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO pending_discards (content_hash, paths, covers)
		VALUES ($1, COALESCE($2::text[], '{}'), COALESCE($3::text[], '{}'))
		ON CONFLICT (content_hash) DO UPDATE
		   SET paths  = ARRAY(SELECT DISTINCT unnest(pending_discards.paths || EXCLUDED.paths)),
		       covers = ARRAY(SELECT DISTINCT unnest(pending_discards.covers || EXCLUDED.covers))`,
		hash, paths, covers); err != nil {
		return fmt.Errorf("recording what an erasure owes: %w", mapError(err))
	}
	return nil
}

// DiscardResult is what a pass over the owed copies did.
type DiscardResult struct {
	// Paid counts hashes settled: their copies deleted, or found to belong
	// to a record again, whose holder they are now.
	Paid int
	// Waiting counts hashes left for later because an upload of the bytes is
	// on its way: its extraction is queued or running, or it was recorded
	// moments ago and its extraction is about to be queued.
	Waiting int
	// Failed counts hashes whose deletion failed; they are tried again.
	Failed int
	// Stale are the waiting hashes owed for longer than StaleDebt, worth a
	// word in the log.
	Stale []string
}

// StaleDebt is how long a debt may wait before a pass reports it as stale.
const StaleDebt = 24 * time.Hour

// claimGrace is how long a fresh claim counts as wanting its bytes with no
// extraction queued for it: an upload records its claim and then queues the
// extraction, a moment later, in a separate statement. After that, a claim
// with no extraction queued or running - it failed for good, or was never
// queued - wants nothing.
const claimGrace = time.Hour

// DiscardPending pays what erasures owe the disk: for each owed content
// hash - these, or every one owed when hashes is nil, oldest first - it
// deletes the stored copies through discard, under the bytes' lock, while
// nothing wants them.
//
// Each hash in a transaction of its own, under the lock every arrival of
// those bytes takes:
//
//   - A record of the bytes exists again - somebody added them back, or a
//     scan found them. They are that record's holder's now, and there is
//     nothing of the leaver's left to erase but the erased records' covers,
//     which are named per record and are deleted; the bytes are not touched,
//     and the debt is settled. Deciding which copies a live record still
//     needs is not a question this answers by comparing paths.
//   - An upload of the bytes is on its way - its extraction queued or
//     running, or its claim recorded within claimGrace and the extraction
//     not queued yet. The upload will read the stored copy, so the debt
//     waits; meanwhile the owed covers, and the owed copies outside the
//     upload directory (the archive's), which no upload reads, are deleted.
//   - Blocked bytes are the blocklist's to clean up; the owed covers go and
//     the debt is let go.
//   - Otherwise every stored copy goes, with the covers.
//
// An upload that stored its copy before a debt is paid, and is recorded
// after, puts its own copy back as it is recorded (UploadStore.Settle under
// HoldUpload's lock). The files are deleted inside the transaction, under the
// lock, as DiscardBlockedCopies does; a failure is recorded against the hash
// and tried again next time.
func (p *Pool) DiscardPending(ctx context.Context, hashes []string,
	discard func(StoredCopies) error) (DiscardResult, error) {

	if hashes == nil {
		rows, err := p.Query(ctx, `SELECT content_hash FROM pending_discards ORDER BY queued_at, content_hash`)
		if err != nil {
			return DiscardResult{}, fmt.Errorf("finding owed copies: %w", mapError(err))
		}
		if hashes, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return DiscardResult{}, fmt.Errorf("finding owed copies: %w", mapError(err))
		}
	}
	var out DiscardResult
	for _, hash := range hashes {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		state, stale, err := p.discardOne(ctx, hash, discard)
		if err != nil && state != discardFailed {
			return out, err
		}
		switch state {
		case discardPaid:
			out.Paid++
		case discardWaiting:
			out.Waiting++
			if stale {
				out.Stale = append(out.Stale, hash)
			}
		case discardFailed:
			out.Failed++
		}
	}
	return out, nil
}

// StillOwed counts, of these content hashes, the ones whose stored copies are
// still owed the disk.
func (p *Pool) StillOwed(ctx context.Context, hashes []string) (int, error) {
	var n int
	if err := p.QueryRow(ctx, `
		SELECT count(*) FROM pending_discards WHERE content_hash = ANY($1::text[])`, hashes).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting owed copies: %w", mapError(err))
	}
	return n, nil
}

// afterDiscardLock stops DiscardPending once it holds a hash's lock, so a
// test can race an upload against it exactly. Always nil outside tests.
var afterDiscardLock func()

type discardState int

const (
	discardNothing discardState = iota
	discardPaid
	discardWaiting
	discardFailed
)

func (p *Pool) discardOne(ctx context.Context, hash string, discard func(StoredCopies) error) (discardState, bool, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return discardNothing, false, fmt.Errorf("paying owed copies: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockContentHash(ctx, tx, hash); err != nil {
		return discardNothing, false, err
	}
	if afterDiscardLock != nil {
		afterDiscardLock()
	}
	var owed StoredCopies
	var stale bool
	err = tx.QueryRow(ctx, `
		SELECT paths, covers, queued_at < now() - make_interval(secs => $2)
		  FROM pending_discards WHERE content_hash = $1 FOR UPDATE`, hash, StaleDebt.Seconds(),
	).Scan(&owed.Paths, &owed.Covers, &stale)
	if isNotFound(mapError(err)) {
		return discardNothing, false, nil // paid meanwhile
	}
	if err != nil {
		return discardNothing, false, fmt.Errorf("paying owed copies: %w", mapError(err))
	}
	owed.ContentHash = hash

	var recorded, blocked, wanted bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM book_files WHERE content_hash = $1),
		       EXISTS (SELECT 1 FROM blocked_content WHERE content_hash = $1),
		       EXISTS (SELECT 1 FROM jobs
		                WHERE status IN ('pending', 'running') AND type = 'extract'
		                  AND (payload ->> 'uploaded')::boolean IS TRUE
		                  AND payload ->> 'contentHash' = $1)
		    OR EXISTS (SELECT 1 FROM holding_claims
		                WHERE content_hash = $1 AND created_at > now() - make_interval(secs => $2))`,
		hash, claimGrace.Seconds()).Scan(&recorded, &blocked, &wanted); err != nil {
		return discardNothing, false, fmt.Errorf("paying owed copies: %w", mapError(err))
	}

	settle := func() (discardState, bool, error) {
		if _, err := tx.Exec(ctx, `DELETE FROM pending_discards WHERE content_hash = $1`, hash); err != nil {
			return discardNothing, false, fmt.Errorf("paying owed copies: %w", mapError(err))
		}
		if err := tx.Commit(ctx); err != nil {
			return discardNothing, false, fmt.Errorf("paying owed copies: %w", mapError(err))
		}
		return discardPaid, false, nil
	}
	fail := func(derr error) (discardState, bool, error) {
		if _, err := tx.Exec(ctx, `
			UPDATE pending_discards SET attempts = attempts + 1, last_error = left($2, 1000)
			 WHERE content_hash = $1`, hash, derr.Error()); err != nil {
			return discardNothing, false, fmt.Errorf("paying owed copies: %w", mapError(err))
		}
		if err := tx.Commit(ctx); err != nil {
			return discardNothing, false, fmt.Errorf("paying owed copies: %w", mapError(err))
		}
		return discardFailed, false, fmt.Errorf("deleting the stored copies of %s: %w", hash, derr)
	}
	coversOnly := StoredCopies{Covers: owed.Covers}

	switch {
	case recorded, blocked:
		if len(owed.Covers) > 0 {
			if err := discard(coversOnly); err != nil {
				return fail(err)
			}
		}
		return settle()
	case wanted:
		// What no upload reads goes now; the rest waits. The covers, and
		// the owed copies outside the upload directory, are forgotten once
		// gone so they are not deleted twice.
		partial := StoredCopies{ContentHash: hash, Paths: owed.Paths, Covers: owed.Covers, SkipUploads: true}
		if len(owed.Covers) > 0 || len(owed.Paths) > 0 {
			if err := discard(partial); err != nil {
				return fail(err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE pending_discards SET covers = '{}' WHERE content_hash = $1`, hash); err != nil {
				return discardNothing, false, fmt.Errorf("paying owed copies: %w", mapError(err))
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return discardNothing, false, fmt.Errorf("paying owed copies: %w", mapError(err))
		}
		return discardWaiting, stale, nil
	}
	if err := discard(owed); err != nil {
		return fail(err)
	}
	return settle()
}

// DeleteEmptyEditions deletes, of these editions, the ones no file belongs to
// any more, and then the works left with no edition: what an erasure leaves
// behind once it has committed. Best effort and separate from the erasure, so
// it never waits on anything: an edition or work somebody is changing right
// now is skipped, and stays as an invisible record nobody can open - which is
// what an expired archive record has always left.
func (p *Pool) DeleteEmptyEditions(ctx context.Context, editions []uuid.UUID) error {
	if len(editions) == 0 {
		return nil
	}
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("deleting empty editions: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Locked first, and only then checked for emptiness, in a statement of
	// its own. Checked and locked in one statement, the check reads the
	// statement's snapshot: a file added to the edition and committed between
	// that snapshot and the lock is not seen, and the cascade from editions
	// takes it with it. Locked first, a file being added now holds the
	// edition (its foreign key's key-share), so the lock skips it; one added
	// once the lock is held waits, and fails when the edition is gone rather
	// than vanishing; one committed before is seen by the check.
	rows, err := tx.Query(ctx, `
		SELECT id FROM editions WHERE id = ANY($1) ORDER BY id FOR UPDATE SKIP LOCKED`, editions)
	if err != nil {
		return fmt.Errorf("deleting empty editions: %w", mapError(err))
	}
	lockedEditions, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("deleting empty editions: %w", mapError(err))
	}
	if betweenEmptyCheckAndDelete != nil {
		betweenEmptyCheckAndDelete()
	}
	rows, err = tx.Query(ctx, `
		DELETE FROM editions e
		 WHERE e.id = ANY($1)
		   AND NOT EXISTS (SELECT 1 FROM book_files bf WHERE bf.edition_id = e.id)
		RETURNING e.work_id`, lockedEditions)
	if err != nil {
		return fmt.Errorf("deleting empty editions: %w", mapError(err))
	}
	works, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return fmt.Errorf("deleting empty editions: %w", mapError(err))
	}
	if len(works) > 0 {
		// The same for the works: an edition moved into one by a merge, or
		// added to it, holds it while that happens.
		rows, err := tx.Query(ctx, `
			SELECT id FROM works WHERE id = ANY($1) ORDER BY id FOR UPDATE SKIP LOCKED`, works)
		if err != nil {
			return fmt.Errorf("deleting empty works: %w", mapError(err))
		}
		lockedWorks, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return fmt.Errorf("deleting empty works: %w", mapError(err))
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM works w
			 WHERE w.id = ANY($1)
			   AND NOT EXISTS (SELECT 1 FROM editions e WHERE e.work_id = w.id)`, lockedWorks); err != nil {
			return fmt.Errorf("deleting empty works: %w", mapError(err))
		}
	}
	return tx.Commit(ctx)
}

// betweenEmptyCheckAndDelete stops DeleteEmptyEditions once it holds the
// editions and before it deletes any, so a test can add a file to one
// exactly then. Always nil outside tests.
var betweenEmptyCheckAndDelete func()
