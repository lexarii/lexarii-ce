// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// Tombstone records that a mark was deleted, for a client that was not there.
type Tombstone struct {
	AnnotationID uuid.UUID
	BookFileID   uuid.UUID
	DeletedAt    time.Time
}

// Changes is everything that happened to one person's reading since a moment.
type Changes struct {
	Annotations []domain.Annotation
	Deleted     []Tombstone
	Progress    []domain.ReadingProgress
	// Until is the server's clock at the moment of THIS read. Once More is
	// false - the round is finished - the client stores this as the next
	// `since`. Never its own clock: two clocks that disagree by a minute
	// would silently lose or repeat a minute of edits.
	Until time.Time
	// More says a limit was reached somewhere and another page is waiting.
	// A client that stops here and syncs again from Until, rather than
	// paging with Cursor first, would skip everything it did not receive.
	More bool
	// Cursor resumes exactly where this page left off, and is set whenever
	// More is true. Opaque; a client only ever sends back what it was given.
	Cursor string
}

// syncOverlap is how far before the requested moment a pull actually starts a
// FRESH round - one with no cursor of its own yet.
//
// A row's updated_at is set when the statement runs and becomes visible when
// its transaction commits, which is later. A client that asked for everything
// after 12:00:00 and was answered at 12:00:00 would never see a row stamped
// 11:59:59.9 that committed at 12:00:00.1. The window resends a couple of
// seconds of changes instead, which costs nothing because every apply is
// keyed by id and idempotent by construction.
//
// This only has to cover that gap once, at the start of a round: once a page
// has actually read a row, that row was by definition already committed and
// visible, and every later page of the SAME round can simply keep moving
// forward past it with an ordinary keyset comparison. A client resuming with
// Cursor does not go through this at all.
const syncOverlap = 2 * time.Second

// cursorPosition is one stream's place in a sync cursor: the timestamp and id
// of the last row it sent, which is also the keyset bound for the next page.
type cursorPosition struct {
	At time.Time `json:"at"`
	ID string    `json:"id,omitempty"`
}

// uuidBound gives the keyset comparison a concrete pair to compare against. An
// empty or malformed id reads as uuid.Nil, the minimum possible value, which
// is exactly what a fresh page wants: "at or after this moment, from the very
// first row onward".
func (c cursorPosition) uuidBound() (time.Time, uuid.UUID) {
	id, _ := uuid.Parse(c.ID)
	return c.At, id
}

// syncCursor is where a paused pull left off in each of the three streams a
// page merges. Each advances independently - an annotation, a tombstone, and
// a reading position share nothing but the user, and forcing one shared
// position across all three would mean a burst of highlights blocks paging
// through positions that have nothing to do with them.
//
// Until is the server clock reading from the FIRST page of the round, carried
// forward unchanged on every later page rather than re-read each time. A
// stream can reach its own tip on page one while another still has more
// waiting, and a row stamped just before page one's own cursor position but
// not yet committed when page one ran would then be invisible to a keyset
// bound built from page one - and skipped for good, because a later page's
// own now() is already past it and the round's Until would say so too. Page
// one's own now() cannot have that problem, by the same reasoning syncOverlap
// exists for: the round's `since` already opened a window behind it.
// Re-delivering a row across rounds if this is ever too conservative costs
// nothing - every apply is idempotent - but skipping one costs a client a
// piece of their own history for good.
type syncCursor struct {
	Annotation cursorPosition `json:"a"`
	Tombstone  cursorPosition `json:"t"`
	Position   cursorPosition `json:"p"`
	Until      time.Time      `json:"u"`
}

// encodeSyncCursor packs a cursor for the wire. Opaque by convention, the same
// way the library listing's cursor is: a client is never meant to parse it,
// only to send back exactly what it was given.
func encodeSyncCursor(c syncCursor) string {
	raw, err := json.Marshal(c)
	if err != nil {
		// c is built entirely from this package's own concrete types - times,
		// strings - which always marshal.
		panic(fmt.Sprintf("encoding a sync cursor: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// decodeSyncCursor is the other end. A malformed cursor is the client's
// mistake - it can only have come from mangling what this server gave it -
// not a server fault, so it is reported through store.ErrInvalidQuery the
// same way a corrupt library pagination cursor is.
func decodeSyncCursor(s string) (syncCursor, error) {
	if s == "" {
		return syncCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return syncCursor{}, fmt.Errorf("%w: cursor is not base64: %v", store.ErrInvalidQuery, err)
	}
	var c syncCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return syncCursor{}, fmt.Errorf("%w: cursor is not valid: %v", store.ErrInvalidQuery, err)
	}
	return c, nil
}

// Changes reads what a client needs to catch up.
//
// Scoped to the user's OWN marks and positions, not to everything they can
// read. Somebody else's shared highlights are visible in the reader and are
// not this person's to carry offline and edit.
//
// cursor resumes a round in progress and, when given, takes priority over
// since entirely. Leave it empty to start a fresh round at since - which
// remains meaningful on its own for a client that never adopts paging, as
// long as what changed fits in one page.
func (p *Pool) Changes(ctx context.Context, userID uuid.UUID, since time.Time, cursor string, limit int) (Changes, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}

	at, err := decodeSyncCursor(cursor)
	if err != nil {
		return Changes{}, err
	}

	var out Changes
	if cursor == "" {
		// A fresh round: read the server clock once, now, and this round's
		// Until for every page of it - see syncCursor's doc for why a later
		// page must not read its own.
		if err := p.QueryRow(ctx, `SELECT now()`).Scan(&out.Until); err != nil {
			return out, fmt.Errorf("reading the server clock: %w", mapError(err))
		}
		from := since.Add(-syncOverlap)
		at = syncCursor{
			Annotation: cursorPosition{At: from},
			Tombstone:  cursorPosition{At: from},
			Position:   cursorPosition{At: from},
			Until:      out.Until,
		}
	} else {
		out.Until = at.Until
	}
	next := at

	annotationAt, annotationID := at.Annotation.uuidBound()
	rows, err := p.Query(ctx, `
		SELECT a.id, a.book_file_id, a.type,
		       a.start_major, a.start_path, a.start_offset,
		       a.end_major, a.end_path, a.end_offset,
		       a.selected_text, a.note, a.colour, a.orphaned_at,
		       a.created_at, a.updated_at, a.edited_at, bf.format
		FROM annotations a
		JOIN book_files bf ON bf.id = a.book_file_id
		WHERE a.user_id = $1 AND (a.updated_at, a.id) > ($2, $3)
		ORDER BY a.updated_at, a.id
		LIMIT $4`, userID, annotationAt, annotationID, limit)
	if err != nil {
		return out, fmt.Errorf("reading changed annotations: %w", mapError(err))
	}
	out.Annotations, err = scanSyncAnnotations(rows, userID)
	if err != nil {
		return out, err
	}
	if n := len(out.Annotations); n > 0 {
		last := out.Annotations[n-1]
		next.Annotation = cursorPosition{At: last.UpdatedAt, ID: last.ID.String()}
		if n == limit {
			out.More = true
		}
	}

	tombstoneAt, tombstoneID := at.Tombstone.uuidBound()
	tombstones, err := p.Query(ctx, `
		SELECT annotation_id, book_file_id, deleted_at
		FROM annotation_tombstones
		WHERE user_id = $1 AND (deleted_at, annotation_id) > ($2, $3)
		ORDER BY deleted_at, annotation_id
		LIMIT $4`, userID, tombstoneAt, tombstoneID, limit)
	if err != nil {
		return out, fmt.Errorf("reading deletions: %w", mapError(err))
	}
	defer tombstones.Close()
	for tombstones.Next() {
		var t Tombstone
		if err := tombstones.Scan(&t.AnnotationID, &t.BookFileID, &t.DeletedAt); err != nil {
			return out, fmt.Errorf("scanning a deletion: %w", err)
		}
		out.Deleted = append(out.Deleted, t)
	}
	if err := tombstones.Err(); err != nil {
		return out, fmt.Errorf("reading deletions: %w", mapError(err))
	}
	if n := len(out.Deleted); n > 0 {
		last := out.Deleted[n-1]
		next.Tombstone = cursorPosition{At: last.DeletedAt, ID: last.AnnotationID.String()}
		if n == limit {
			out.More = true
		}
	}

	positionAt, positionID := at.Position.uuidBound()
	progress, err := p.Query(ctx, `
		SELECT rp.book_file_id, rp.major, rp.path, rp."offset", rp.percent,
		       rp.furthest_major, rp.furthest_path, rp.furthest_offset, rp.furthest_percent,
		       rp.last_read_at, rp.updated_at, bf.format
		FROM reading_progress rp
		JOIN book_files bf ON bf.id = rp.book_file_id
		WHERE rp.user_id = $1 AND (rp.updated_at, rp.book_file_id) > ($2, $3)
		ORDER BY rp.updated_at, rp.book_file_id
		LIMIT $4`, userID, positionAt, positionID, limit)
	if err != nil {
		return out, fmt.Errorf("reading changed positions: %w", mapError(err))
	}
	defer progress.Close()
	for progress.Next() {
		var rp domain.ReadingProgress
		var format domain.Format
		var major, furthestMajor int
		var percent, furthestPercent *float32
		if err := progress.Scan(&rp.BookFileID, &major, &rp.Position.ElementPath,
			&rp.Position.CharOffset, &percent, &furthestMajor, &rp.Furthest.ElementPath,
			&rp.Furthest.CharOffset, &furthestPercent, &rp.LastReadAt, &rp.UpdatedAt,
			&format); err != nil {
			return out, fmt.Errorf("scanning a position: %w", err)
		}
		rp.UserID = userID
		rp.Percent = percent
		rp.FurthestPercent = furthestPercent
		applyPositionFormat(&rp, format, major, furthestMajor)
		out.Progress = append(out.Progress, rp)
	}
	if err := progress.Err(); err != nil {
		return out, fmt.Errorf("reading changed positions: %w", mapError(err))
	}
	if n := len(out.Progress); n > 0 {
		last := out.Progress[n-1]
		next.Position = cursorPosition{At: last.UpdatedAt, ID: last.BookFileID.String()}
		if n == limit {
			out.More = true
		}
	}

	if out.More {
		out.Cursor = encodeSyncCursor(next)
	}
	return out, nil
}

func scanSyncAnnotations(rows pgx.Rows, userID uuid.UUID) ([]domain.Annotation, error) {
	defer rows.Close()

	var out []domain.Annotation
	for rows.Next() {
		var a domain.Annotation
		var selected, note, colour *string
		var format domain.Format
		if err := rows.Scan(&a.ID, &a.BookFileID, &a.Type,
			&a.Range.Start.SpineIndex, &a.Range.Start.ElementPath, &a.Range.Start.CharOffset,
			&a.Range.End.SpineIndex, &a.Range.End.ElementPath, &a.Range.End.CharOffset,
			&selected, &note, &colour, &a.OrphanedAt,
			&a.CreatedAt, &a.UpdatedAt, &a.EditedAt, &format); err != nil {
			return nil, fmt.Errorf("scanning a changed annotation: %w", err)
		}
		a.UserID = userID
		a.SelectedText = deref(selected)
		a.Note = deref(note)
		a.Colour = deref(colour)
		applyFormat(&a, format)
		out = append(out, a)
	}
	return out, rows.Err()
}

// PushedMark is one change a client made while it was away.
type PushedMark struct {
	// ID is generated by the CLIENT. A mark made on a plane has to have an
	// identity before any server has seen it, and that identity is what makes
	// pushing the same change twice harmless.
	ID                         uuid.UUID
	BookFileID                 uuid.UUID
	Type                       domain.AnnotationType
	Range                      domain.Range
	SelectedText, Note, Colour string
	// UpdatedAt is when the CLIENT made the change, and decides which version
	// wins. Deleted says the change was a deletion.
	UpdatedAt time.Time
	Deleted   bool
}

// MarkOutcome says what became of one pushed change.
type MarkOutcome struct {
	ID uuid.UUID
	// Status is "applied", "stale" - the server held a newer version, which
	// the client will receive in the next pull - or "refused" for a book this
	// person cannot open.
	Status string
}

const (
	MarkApplied = "applied"
	MarkStale   = "stale"
	MarkRefused = "refused"
)

// ApplyMarks writes a client's offline changes.
//
// Last write wins, by the client's own clock, per mark. That is the rule that
// is defensible to a person: the version you edited most recently is the one
// that survives, on whichever device you edited it. A mark is small, edited by
// one person, and rarely in two places at once, so the merge nobody would
// understand is not worth the machinery it would take.
//
// A DELETION always wins over an edit of the same age. Deleting is the
// deliberate act, and a mark that comes back from the dead is the failure
// people notice and distrust; an edit lost to a delete looks like the delete
// they asked for.
//
// Each mark is its own statement rather than the batch being one transaction.
// A single refused book must not discard the other forty changes a phone
// carried back, and every write here is idempotent, so a half-applied push is
// completed by pushing again.
func (p *Pool) ApplyMarks(ctx context.Context, userID uuid.UUID, marks []PushedMark) ([]MarkOutcome, error) {
	out := make([]MarkOutcome, 0, len(marks))

	for _, m := range marks {
		readable, err := p.canOpen(ctx, m.BookFileID, userID)
		if err != nil {
			return out, err
		}
		if !readable {
			out = append(out, MarkOutcome{ID: m.ID, Status: MarkRefused})
			continue
		}

		if m.Deleted {
			applied, err := p.applyDeletion(ctx, userID, m)
			if err != nil {
				// Same rule as an edit: a deletion of a mark this user does not
				// own is this ROW's problem, not the connection's, and must not
				// take the rest of the push down with it.
				if isRefusable(err) {
					out = append(out, MarkOutcome{ID: m.ID, Status: MarkRefused})
					continue
				}
				return out, err
			}
			out = append(out, MarkOutcome{ID: m.ID, Status: statusOf(applied)})
			continue
		}

		applied, err := p.applyEdit(ctx, userID, m)
		if err != nil {
			// One bad mark is refused; the rest of the push still lands.
			//
			// This function's own documentation says refusals are per item and
			// that a single bad book "must not discard the other forty changes
			// a phone carried back" - and then any constraint violation
			// returned an error, which the handler turns into a 500. The client
			// re-sends the same batch, hits the same row, and forty good
			// changes never land at all. A client old enough or broken enough
			// to send a highlight with no text is exactly the client that
			// cannot be fixed from here.
			if isRefusable(err) {
				out = append(out, MarkOutcome{ID: m.ID, Status: MarkRefused})
				continue
			}
			return out, err
		}
		out = append(out, MarkOutcome{ID: m.ID, Status: statusOf(applied)})
	}
	return out, nil
}

func statusOf(applied bool) string {
	if applied {
		return MarkApplied
	}
	return MarkStale
}

// errNotYourMark reports an EDIT of an id somebody else owns - live, or
// tombstoned.
//
// A mark's id is client-generated and travels on the wire in plain sight - the
// per-book list hands it out to everyone a highlight is shared with, and a
// deleted mark's id was visible there before the deletion. An edit is refused
// distinguishably, unlike a deletion (see applyDeletion): an edit carries
// content the client would otherwise wrongly believe was saved, and hiding
// that failure is worse than what refusing it can leak.
var errNotYourMark = errors.New("not this user's mark")

// applyDeletion removes the mark and records that it was removed.
//
// The tombstone is written even when there was no row to delete: a client that
// deleted a mark the server had never received still has to be able to stop
// another of its owner's devices from resurrecting it. That leniency only
// holds for an id nobody has claimed - one already owned by somebody else,
// live or tombstoned, is left alone: see the comment on the ownership check
// below for why that is reported as success rather than refused.
func (p *Pool) applyDeletion(ctx context.Context, userID uuid.UUID, m PushedMark) (bool, error) {
	owner, exists, err := p.annotationOwner(ctx, m.ID)
	if err != nil {
		return false, err
	}
	if exists && owner != userID {
		// Somebody else's id - reported exactly like deleting one that never
		// existed at all, deliberately, rather than as a distinguishable
		// refusal. The only practical way to learn an id you do not own is a
		// shared list, so this cannot be probed at random, but it must still
		// not become an oracle for "is this still somebody's mark": a
		// deletion has nothing to lose by pretending it worked - there is
		// nothing here for this client to retry - and the end state, "this id
		// owns nothing of mine", is exactly what a delete of an unknown id
		// already promises. Contrast applyEdit, which refuses this
		// distinguishably: an edit has content the client would otherwise
		// wrongly believe was saved, and that dishonesty is worse than the
		// leak.
		return true, nil
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("starting a deletion: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`DELETE FROM annotations WHERE id = $1 AND user_id = $2`, m.ID, userID); err != nil {
		return false, fmt.Errorf("deleting a mark: %w", mapError(err))
	}

	// This server's clock, not the client's, and not the application's either
	// - the DATABASE's now(), the same clock Changes reads for Until. Two
	// different processes' clocks can disagree by more than the window that
	// makes them interchangeable elsewhere in this file, and a tombstone
	// stamped from the wrong one is exactly the kind of few-hundred-millisecond
	// skew that syncOverlap exists to paper over for everything else - no
	// reason to reintroduce it here.
	//
	// deleted_at is read by exactly one query - "what deletions have I not
	// been told about" - so it has to be monotonic in the order this server
	// learned things. Stamping it with the moment the person pressed delete,
	// which may be a week ago, files the tombstone behind every other device's
	// cursor and none of them are ever told. There is no last-write-wins
	// comparison on a deletion to want the other clock for: a deletion beats
	// an edit outright.
	if _, err := tx.Exec(ctx, `
		INSERT INTO annotation_tombstones (annotation_id, user_id, book_file_id, deleted_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (annotation_id) DO NOTHING`,
		m.ID, userID, m.BookFileID); err != nil {
		return false, fmt.Errorf("recording a deletion: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("saving a deletion: %w", mapError(err))
	}
	return true, nil
}

// annotationOwner reports who owns an annotation id, if anyone has created one
// yet. ok is false when nothing by this id exists anywhere, live or deleted:
// an offline device deleting or editing a mark the server has never heard of,
// which is not a conflict with anybody.
//
// Checks the tombstone table as well as the live one, not only the live one.
// Once A deletes X, the live row is gone and a first version of this method
// that only looked there answered "nobody owns X" - which let B, who once saw
// X in a shared list, push an EDIT of id X and become ITS new owner outright,
// because nothing on the id itself said otherwise. A's own devices would then
// find X owned by a stranger and have their own, legitimate, later pushes of
// it refused. A tombstone is exactly as much a claim on the id as a live row
// is - it says who deleted it - and an id that is somebody's business to have
// deleted is not up for grabs just because nothing survives to point at.
//
// The live row wins when both exist, deliberately, rather than an unordered
// UNION ALL ... LIMIT 1 that could return either. Every write path in this
// package keeps a live row and its own tombstone mutually exclusive - a
// delete removes one and writes the other in the same transaction - but a
// database that ran before this method checked ownership at all could hold a
// tombstone belonging to somebody who was never actually allowed to delete
// the row: applyDeletion's old, unchecked version scoped its DELETE by
// user_id, so it left somebody else's live mark untouched, but the INSERT
// that followed it was not scoped at all, and wrote a tombstone under the
// wrong person's name regardless. For a mark that is still there, the row
// that is actually there is the one telling the truth.
func (p *Pool) annotationOwner(ctx context.Context, id uuid.UUID) (owner uuid.UUID, ok bool, err error) {
	var found *uuid.UUID
	err = p.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT user_id FROM annotations WHERE id = $1),
			(SELECT user_id FROM annotation_tombstones WHERE annotation_id = $1)
		)`, id).Scan(&found)
	if err != nil {
		return uuid.UUID{}, false, fmt.Errorf("checking a mark's owner: %w", mapError(err))
	}
	if found == nil {
		return uuid.UUID{}, false, nil
	}
	return *found, true, nil
}

// applyEdit inserts or updates one mark, unless the server holds a newer one.
//
// A mark that has a tombstone THIS USER recorded is not resurrected - the
// client will learn of the deletion on its next pull and drop it. Scoped to
// user_id for the same reason applyDeletion refuses an id it does not own:
// annotation_tombstones is inherently a per-user record of "what I deleted",
// and reading it without that scope let one person's tombstone silently
// freeze a completely different person's own edits of their own mark.
func (p *Pool) applyEdit(ctx context.Context, userID uuid.UUID, m PushedMark) (bool, error) {
	if err := m.Range.Validate(); err != nil {
		return false, fmt.Errorf("a pushed mark is not anchored: %w", err)
	}
	owner, exists, err := p.annotationOwner(ctx, m.ID)
	if err != nil {
		return false, err
	}
	if exists && owner != userID {
		return false, errNotYourMark
	}

	updatedAt := m.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}

	tag, err := p.Exec(ctx, `
		INSERT INTO annotations
			(id, user_id, book_file_id, type,
			 start_major, start_path, start_offset,
			 end_major, end_path, end_offset,
			 selected_text, note, colour, edited_at)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
		       NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, ''), $14
		WHERE NOT EXISTS (
		    SELECT 1 FROM annotation_tombstones
		     WHERE annotation_id = $1 AND user_id = $2)
		ON CONFLICT (id) DO UPDATE SET
			type = EXCLUDED.type,
			start_major = EXCLUDED.start_major,
			start_path = EXCLUDED.start_path,
			start_offset = EXCLUDED.start_offset,
			end_major = EXCLUDED.end_major,
			end_path = EXCLUDED.end_path,
			end_offset = EXCLUDED.end_offset,
			selected_text = EXCLUDED.selected_text,
			note = EXCLUDED.note,
			colour = EXCLUDED.colour,
			-- edited_at is the person's clock and decides who wins;
			-- updated_at is this server's and is left to the trigger, because
			-- it is what a pull cursor reads and must be monotonic in the
			-- order this server learned things.
			edited_at = EXCLUDED.edited_at
		-- The guard that makes this last-write-wins rather than
		-- last-to-arrive-wins. A phone syncing a week-old edit must not undo
		-- what a laptop did yesterday - and the comparison is on when each was
		-- MADE, not when either arrived.
		WHERE annotations.user_id = $2 AND annotations.edited_at < EXCLUDED.edited_at`,
		m.ID, userID, m.BookFileID, m.Type,
		majorOf(m.Range.Start), m.Range.Start.ElementPath, m.Range.Start.CharOffset,
		majorOf(m.Range.End), m.Range.End.ElementPath, m.Range.End.CharOffset,
		m.SelectedText, m.Note, m.Colour, updatedAt)
	if err != nil {
		return false, fmt.Errorf("applying a mark: %w", mapError(err))
	}
	return tag.RowsAffected() > 0, nil
}

// PushedPosition is where a client says a book was left.
type PushedPosition struct {
	BookFileID uuid.UUID
	Position   domain.Locator
	Percent    *float32
	// ReadAt is when the client was at this position. It decides whether this
	// is news or history.
	ReadAt time.Time
}

// MergePositions reconciles reading positions from a client that was away.
//
// Two rules, because a position is two facts, and this is the same merge the
// online path already does with one difference that matters offline.
//
// WHERE the reader is takes the most recent write BY THE READER'S CLOCK, not
// by arrival. The online path can use now() because the request arrives as the
// page turns; a phone handing over a week of reading cannot, or the oldest
// thing in its queue would land as the newest thing that happened and a book
// would reopen where it was a week ago.
//
// HOW FAR they got only ever advances, from either device. That is what keeps
// a laptop that opened chapter one from undoing an evening's reading on a
// phone, and it needs no clock at all.
func (p *Pool) MergePositions(ctx context.Context, userID uuid.UUID, positions []PushedPosition) error {
	for _, in := range positions {
		if err := in.Position.Validate(); err != nil {
			return fmt.Errorf("a pushed position is not a locator: %w", err)
		}
		readable, err := p.canOpen(ctx, in.BookFileID, userID)
		if err != nil {
			return err
		}
		if !readable {
			// Silently skipped rather than refused: a book that was unshared
			// while the phone was away is not the phone's mistake, and there
			// is nothing for it to do about it.
			continue
		}

		readAt := in.ReadAt
		if readAt.IsZero() {
			readAt = time.Now()
		}
		major := in.Position.SpineIndex + in.Position.Page

		if err := p.mergeOnePosition(ctx, userID, in.BookFileID, major, in.Position, in.Percent, readAt); err != nil {
			return err
		}
	}
	return nil
}

// mergeOnePosition is one position's worth of MergePositions, in its own
// short transaction: long enough to lock the existing furthest position,
// decide - in Go, via Locator.CompareDecided, not a second comparison written
// in SQL - whether the incoming one moves it forward, and write both facts
// together.
func (p *Pool) mergeOnePosition(ctx context.Context, userID, bookFileID uuid.UUID,
	major int, position domain.Locator, percent *float32, readAt time.Time) error {

	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("merging a position: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// See lockReadingProgress: a first-ever save for this book has no row for
	// furthestFor's own FOR UPDATE to lock, and two of them racing could
	// otherwise both decide they are the furthest and let whichever commits
	// second win regardless of which position actually was.
	if err := lockReadingProgress(ctx, tx, userID, bookFileID); err != nil {
		return fmt.Errorf("merging a position: %w", err)
	}

	furthest, furthestPercent, err := furthestFor(ctx, tx, userID, bookFileID, position, percent)
	if err != nil {
		return fmt.Errorf("merging a position: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO reading_progress
			(user_id, book_file_id, major, path, "offset", percent,
			 furthest_major, furthest_path, furthest_offset, furthest_percent, last_read_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (user_id, book_file_id) DO UPDATE SET
			-- Where they are: only if this is newer than what is stored.
			major = CASE WHEN EXCLUDED.last_read_at > reading_progress.last_read_at
			             THEN EXCLUDED.major ELSE reading_progress.major END,
			path = CASE WHEN EXCLUDED.last_read_at > reading_progress.last_read_at
			            THEN EXCLUDED.path ELSE reading_progress.path END,
			"offset" = CASE WHEN EXCLUDED.last_read_at > reading_progress.last_read_at
			                THEN EXCLUDED."offset" ELSE reading_progress."offset" END,
			percent = CASE WHEN EXCLUDED.last_read_at > reading_progress.last_read_at
			               THEN EXCLUDED.percent ELSE reading_progress.percent END,

			-- How far they got: decided in Go, above, by furthestFor - see
			-- reading.go for why comparing (major, path, offset) as a SQL row
			-- tuple, which orders path as TEXT, is the same "p[10]" before
			-- "p[9]" mistake migration 00015 already undid for annotations,
			-- and the reason it could silently disagree with Go about which
			-- of two positions was actually further.
			furthest_major = EXCLUDED.furthest_major,
			furthest_path = EXCLUDED.furthest_path,
			furthest_offset = EXCLUDED.furthest_offset,
			furthest_percent = EXCLUDED.furthest_percent,
			last_read_at = GREATEST(EXCLUDED.last_read_at, reading_progress.last_read_at)`,
		userID, bookFileID, major, position.ElementPath, position.CharOffset, percent,
		furthest.SpineIndex+furthest.Page, furthest.ElementPath, furthest.CharOffset, furthestPercent,
		readAt); err != nil {
		return fmt.Errorf("merging a position: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("merging a position: %w", mapError(err))
	}
	return nil
}

// isRefusable reports a failure caused by the mark rather than by the server.
//
// A constraint violation means this row is wrong; a dropped connection means
// the next row will fail too. The first is refused and the push continues, the
// second stops it.
func isRefusable(err error) bool {
	if errors.Is(err, store.ErrInvalidQuery) {
		return true
	}
	// A range that runs backwards - checked in Go, before the statement that
	// would otherwise fail the same way in Postgres - is exactly as much this
	// row's fault as a CHECK violation is. Missing this sent the whole push
	// down the "unexpected error" path below and back as a 500, discarding
	// every other change the push carried alongside it.
	if errors.Is(err, domain.ErrInvalidLocator) {
		return true
	}
	// A push touching an id somebody else already owns - a mark, or a
	// deletion of one - is the client's mistake, not the server's.
	if errors.Is(err, errNotYourMark) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// Class 23: integrity constraint violation - check, not-null, foreign
		// key, uniqueness. All of them say the same thing about this row.
		return strings.HasPrefix(pgErr.Code, "23")
	}
	return false
}
