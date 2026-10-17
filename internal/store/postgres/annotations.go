// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// CreateAnnotation stores a mark.
//
// Refused on a book the person cannot open. Nothing is leaked by allowing it -
// the text is theirs - but a row pointing at somebody else's book is a claim
// this database should not hold, and it is one fewer thing to reason about the
// next time the access rules change.
func (p *Pool) CreateAnnotation(ctx context.Context, a domain.Annotation) (domain.Annotation, error) {
	readable, err := p.canOpen(ctx, a.BookFileID, a.UserID)
	if err != nil {
		return domain.Annotation{}, err
	}
	if !readable {
		return domain.Annotation{}, errNotFound
	}

	if err := a.Range.Validate(); err != nil {
		return domain.Annotation{}, fmt.Errorf("creating annotation: %w", err)
	}

	var out domain.Annotation
	var selected, note, colour *string
	err = p.QueryRow(ctx, `
		INSERT INTO annotations
			(user_id, book_file_id, type,
			 start_major, start_path, start_offset,
			 end_major, end_path, end_offset,
			 selected_text, note, colour)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''))
		RETURNING id, user_id, book_file_id, type,
		          start_major, start_path, start_offset,
		          end_major, end_path, end_offset,
		          selected_text, note, colour, orphaned_at, created_at, updated_at, edited_at`,
		a.UserID, a.BookFileID, a.Type,
		majorOf(a.Range.Start), a.Range.Start.ElementPath, a.Range.Start.CharOffset,
		majorOf(a.Range.End), a.Range.End.ElementPath, a.Range.End.CharOffset,
		a.SelectedText, a.Note, a.Colour,
	).Scan(&out.ID, &out.UserID, &out.BookFileID, &out.Type,
		&out.Range.Start.SpineIndex, &out.Range.Start.ElementPath, &out.Range.Start.CharOffset,
		&out.Range.End.SpineIndex, &out.Range.End.ElementPath, &out.Range.End.CharOffset,
		&selected, &note, &colour, &out.OrphanedAt, &out.CreatedAt, &out.UpdatedAt, &out.EditedAt)
	if err != nil {
		return domain.Annotation{}, fmt.Errorf("creating annotation: %w", mapError(err))
	}

	out.SelectedText = deref(selected)
	out.Note = deref(note)
	out.Colour = deref(colour)
	applyFormat(&out, a.Range.Start.Format)
	return out, nil
}

// Annotations lists the marks this person can see in one file, in reading
// order: their own, plus anyone who has shared theirs on this book.
//
// Someone else's highlights are readable and never editable - Update and Delete
// still match on a.user_id - so a shared book cannot become a shared
// scribbling surface.
func (p *Pool) Annotations(ctx context.Context, userID, bookFileID uuid.UUID) ([]domain.Annotation, error) {
	rows, err := p.Query(ctx, `
		SELECT a.id, a.user_id, a.book_file_id, a.type,
		       a.start_major, a.start_path, a.start_offset,
		       a.end_major, a.end_path, a.end_offset,
		       a.selected_text, a.note, a.colour, a.orphaned_at, a.created_at, a.updated_at, a.edited_at,
		       bf.format, u.username, a.user_id <> $1 AS shared
		FROM annotations a
		JOIN book_files bf ON bf.id = a.book_file_id
		JOIN users u ON u.id = a.user_id
		-- Marks are only ever shown on a book this person may open.
		--
		-- Until now that held by construction rather than by rule: the only way
		-- to get an annotation_access row was ShareBook, which granted the book
		-- in the same transaction. Notes shared by somebody who is not the
		-- owner break that construction, and a highlight carries its selected
		-- text - so an unenforced share would hand over the book one mark at a
		-- time. Checked here, at the point of reading, because access can be
		-- withdrawn after a share was made.
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = a.book_file_id AND rbf.user_id = $1
		WHERE a.book_file_id = $2
		  AND (a.user_id = $1
		       OR EXISTS (SELECT 1 FROM annotation_access aa
		                   WHERE aa.book_file_id = a.book_file_id
		                     AND aa.owner_id = a.user_id
		                     AND aa.viewer_id = $1))
		ORDER BY a.start_major, a.start_path, a.start_offset`, userID, bookFileID)
	if err != nil {
		return nil, fmt.Errorf("reading annotations: %w", mapError(err))
	}
	defer rows.Close()

	var out []domain.Annotation
	for rows.Next() {
		var a domain.Annotation
		var selected, note, colour *string
		var format domain.Format
		if err := rows.Scan(&a.ID, &a.UserID, &a.BookFileID, &a.Type,
			&a.Range.Start.SpineIndex, &a.Range.Start.ElementPath, &a.Range.Start.CharOffset,
			&a.Range.End.SpineIndex, &a.Range.End.ElementPath, &a.Range.End.CharOffset,
			&selected, &note, &colour, &a.OrphanedAt, &a.CreatedAt, &a.UpdatedAt, &a.EditedAt, &format,
			&a.Author, &a.Shared); err != nil {
			return nil, fmt.Errorf("scanning annotation: %w", err)
		}
		a.SelectedText = deref(selected)
		a.Note = deref(note)
		a.Colour = deref(colour)
		applyFormat(&a, format)
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAnnotation changes a mark's note or colour.
//
// The anchor is deliberately NOT editable. Moving a highlight to different text
// is creating a different highlight, and allowing it in place would silently
// detach the note from the words it was written about.
func (p *Pool) UpdateAnnotation(ctx context.Context, userID, id uuid.UUID, note, colour string) (domain.Annotation, error) {
	var out domain.Annotation
	var selected, storedNote, storedColour *string
	var format domain.Format

	// Writing a note PROMOTES a highlight; clearing it demotes it back.
	//
	// The type and the body cannot disagree - the schema says a note has one
	// and a highlight has selected text - and a reader marking a passage does
	// not know yet whether they are about to write anything. So both readers
	// create a highlight and this decides what it became, which is the only
	// place that knows.
	err := p.QueryRow(ctx, `
		UPDATE annotations a
		SET note = NULLIF($3, ''),
		    colour = COALESCE(NULLIF($4, ''), colour),
		    type = CASE
		             WHEN NULLIF($3, '') IS NOT NULL THEN 'note'::annotation_type
		             WHEN a.selected_text IS NOT NULL THEN 'highlight'::annotation_type
		             ELSE a.type
		           END,
		    -- An edit made here, through the reader rather than through sync,
		    -- was made right now - the same reasoning migration 28 gives for
		    -- why a freshly created row's edited_at defaults to now(). Left
		    -- alone, edited_at would keep whatever it was stamped with at
		    -- creation (or by an earlier sync push), and a later offline edit
		    -- pushed afterwards - even one made BEFORE this one - could read as
		    -- newer than an edit that actually happened after it and overwrite
		    -- it, because last-write-wins had nothing current to compare against.
		    edited_at = now()
		FROM book_files bf
		WHERE a.id = $2 AND a.user_id = $1 AND bf.id = a.book_file_id
		RETURNING a.id, a.user_id, a.book_file_id, a.type,
		          a.start_major, a.start_path, a.start_offset,
		          a.end_major, a.end_path, a.end_offset,
		          a.selected_text, a.note, a.colour, a.orphaned_at,
		          a.created_at, a.updated_at, a.edited_at, bf.format`,
		userID, id, note, colour,
	).Scan(&out.ID, &out.UserID, &out.BookFileID, &out.Type,
		&out.Range.Start.SpineIndex, &out.Range.Start.ElementPath, &out.Range.Start.CharOffset,
		&out.Range.End.SpineIndex, &out.Range.End.ElementPath, &out.Range.End.CharOffset,
		&selected, &storedNote, &storedColour, &out.OrphanedAt, &out.CreatedAt, &out.UpdatedAt, &out.EditedAt, &format)
	if err != nil {
		return domain.Annotation{}, mapError(err)
	}

	out.SelectedText = deref(selected)
	out.Note = deref(storedNote)
	out.Colour = deref(storedColour)
	applyFormat(&out, format)
	return out, nil
}

// DeleteAnnotation removes a mark.
//
// Scoped to the owner, so a guessed id cannot delete someone else's note.
func (p *Pool) DeleteAnnotation(ctx context.Context, userID, id uuid.UUID) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("deleting annotation: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var bookFileID uuid.UUID
	err = tx.QueryRow(ctx,
		`DELETE FROM annotations WHERE id = $1 AND user_id = $2 RETURNING book_file_id`,
		id, userID).Scan(&bookFileID)
	if err != nil {
		return mapError(err)
	}

	// The deletion is recorded, in the same transaction as the deletion.
	//
	// A device that was offline when this happened has no other way to find
	// out: the row is gone, and a missing row is indistinguishable from one it
	// has not been told about yet, so without this the mark comes back the
	// next time that device syncs. Writing it here rather than only on the
	// sync path means deleting a highlight in the browser reaches the phone,
	// which is the direction that actually happens.
	if _, err := tx.Exec(ctx, `
		INSERT INTO annotation_tombstones (annotation_id, user_id, book_file_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (annotation_id) DO UPDATE SET deleted_at = now()`,
		id, userID, bookFileID); err != nil {
		return fmt.Errorf("recording a deletion: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("deleting annotation: %w", mapError(err))
	}
	return nil
}

// MarkAnnotationsOrphaned flags marks whose anchors no longer resolve.
//
// Flagged, never deleted. A note the user wrote is their work, and the selected
// text snapshot means an orphan can still be shown and read even when the
// passage it pointed at has moved or gone. Silently dropping them would be the
// one unrecoverable outcome.
func (p *Pool) MarkAnnotationsOrphaned(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := p.Exec(ctx, `
		UPDATE annotations SET orphaned_at = now()
		WHERE user_id = $1 AND id = ANY($2) AND orphaned_at IS NULL`, userID, ids)
	if err != nil {
		return 0, fmt.Errorf("marking annotations orphaned: %w", mapError(err))
	}
	return tag.RowsAffected(), nil
}

// ClearAnnotationOrphan un-flags a mark whose anchor resolves again, which
// happens when a file is restored or re-ingested deterministically.
func (p *Pool) ClearAnnotationOrphan(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := p.Exec(ctx, `
		UPDATE annotations SET orphaned_at = NULL
		WHERE user_id = $1 AND id = ANY($2) AND orphaned_at IS NOT NULL`, userID, ids); err != nil {
		return fmt.Errorf("clearing orphan flags: %w", mapError(err))
	}
	return nil
}

// RecentAnnotationsPage is one page of the notes view.
type RecentAnnotationsPage struct {
	Items []AnnotationWithBook
	// NextCursor continues from the last item; empty on the last page.
	NextCursor string
}

// RecentAnnotations lists a user's marks across the whole library, newest
// first, a page at a time, for a notes view.
//
// The page is keyed on (created_at, id) rather than an offset, so a note made
// while somebody is paging neither repeats a row nor skips one. A malformed
// cursor is store.ErrInvalidQuery.
func (p *Pool) RecentAnnotations(ctx context.Context, userID uuid.UUID, limit int, cursor string) (RecentAnnotationsPage, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	args := []any{userID, limit + 1}
	after := ""
	if cursor != "" {
		at, id, err := decodeAnnotationCursor(cursor)
		if err != nil {
			return RecentAnnotationsPage{}, err
		}
		after = "AND (a.created_at, a.id) < ($3, $4)"
		args = append(args, at, id)
	}

	rows, err := p.Query(ctx, `
		SELECT a.id, a.type, a.selected_text, a.note, a.colour, a.orphaned_at,
		       a.created_at, a.updated_at, a.edited_at,
		       a.start_major, a.start_path, a.start_offset,
		       a.end_major, a.end_path, a.end_offset,
		       bf.id, bf.format, w.id, w.title
		FROM annotations a
		JOIN book_files bf ON bf.id = a.book_file_id
		-- Only books this person can still open. A note outlives the access
		-- that allowed it, and listing it with its title would keep a borrowed
		-- book on their shelf after it was taken back.
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = a.user_id
		JOIN editions e ON e.id = bf.edition_id
		JOIN works w ON w.id = e.work_id
		WHERE a.user_id = $1 `+after+`
		ORDER BY a.created_at DESC, a.id DESC
		LIMIT $2`, args...)
	if err != nil {
		return RecentAnnotationsPage{}, fmt.Errorf("reading recent annotations: %w", mapError(err))
	}
	defer rows.Close()

	var page RecentAnnotationsPage
	for rows.Next() {
		var item AnnotationWithBook
		var selected, note, colour *string
		if err := rows.Scan(&item.Annotation.ID, &item.Annotation.Type, &selected, &note, &colour,
			&item.Annotation.OrphanedAt, &item.Annotation.CreatedAt, &item.Annotation.UpdatedAt,
			&item.Annotation.EditedAt,
			&item.Annotation.Range.Start.SpineIndex, &item.Annotation.Range.Start.ElementPath,
			&item.Annotation.Range.Start.CharOffset,
			&item.Annotation.Range.End.SpineIndex, &item.Annotation.Range.End.ElementPath,
			&item.Annotation.Range.End.CharOffset,
			&item.BookFileID, &item.Format, &item.WorkID, &item.Title); err != nil {
			return RecentAnnotationsPage{}, fmt.Errorf("scanning recent annotation: %w", err)
		}
		item.Annotation.SelectedText = deref(selected)
		item.Annotation.Note = deref(note)
		item.Annotation.Colour = deref(colour)
		// The mark's own book, which this list never filled in: every row
		// carried the zero UUID, and a client reading the annotation rather
		// than the row around it had no way to know which book it was from.
		item.Annotation.BookFileID = item.BookFileID
		item.Annotation.UserID = userID
		applyFormat(&item.Annotation, item.Format)
		page.Items = append(page.Items, item)
	}
	if err := rows.Err(); err != nil {
		return RecentAnnotationsPage{}, fmt.Errorf("reading recent annotations: %w", mapError(err))
	}

	// One extra row was fetched to tell whether another page follows.
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1].Annotation
		page.NextCursor = encodeAnnotationCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// encodeAnnotationCursor packs the sort key of the last mark on a page. The
// time keeps its full precision: Postgres stores microseconds, and a cursor
// rounded to the second would skip every mark made in the same second.
func encodeAnnotationCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(at.UTC().Format(time.RFC3339Nano) + "\x00" + id.String()))
}

func decodeAnnotationCursor(cursor string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor is not base64: %v", store.ErrInvalidQuery, err)
	}
	when, idStr, ok := strings.Cut(string(raw), "\x00")
	if !ok {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor has no id", store.ErrInvalidQuery)
	}
	at, err := time.Parse(time.RFC3339Nano, when)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor time: %v", store.ErrInvalidQuery, err)
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor id is not a UUID: %v", store.ErrInvalidQuery, err)
	}
	return at, id, nil
}

// AnnotationWithBook is a mark alongside the book it belongs to.
type AnnotationWithBook struct {
	Annotation domain.Annotation
	BookFileID uuid.UUID
	WorkID     uuid.UUID
	Title      string
	Format     domain.Format
}

// majorOf returns the stored `major` for a locator: the spine index for EPUB,
// the page for PDF. Exactly one of the two is ever set.
func majorOf(l domain.Locator) int {
	if l.Format == domain.FormatPDF {
		return l.Page
	}
	return l.SpineIndex
}

// applyFormat restores the format-specific meaning of the stored `major`, which
// scans into SpineIndex regardless of what it actually was.
func applyFormat(a *domain.Annotation, format domain.Format) {
	a.Range.Start.Format = format
	a.Range.End.Format = format
	if format != domain.FormatPDF {
		return
	}
	a.Range.Start.Page, a.Range.Start.SpineIndex = a.Range.Start.SpineIndex, 0
	a.Range.End.Page, a.Range.End.SpineIndex = a.Range.End.SpineIndex, 0
}

// canOpen reports whether this person may read this book file.
func (p *Pool) canOpen(ctx context.Context, bookFileID, userID uuid.UUID) (bool, error) {
	var ok bool
	err := p.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM readable_book_files
		                WHERE book_file_id = $1 AND user_id = $2)`,
		bookFileID, userID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("checking access: %w", mapError(err))
	}
	return ok, nil
}

// minNoteRank is how strong a note's match has to be to reach an answer.
//
// The search ORs the question's words, so without a floor a note sharing one
// common word with a question would be offered as though it bore on it. 0.1 is
// below anything that looks like a real match and above the noise: measured on
// a real library, a question about fuzz testing scores 0.8 against the note
// beside that passage and 0 against the others.
const minNoteRank = 0.1

// minNoteSimilarity is how close a misspelling has to be.
//
// 0.4, from measuring the real thing: the right note scores 0.489 against a
// question with a word misspelled, and the worst unrelated pairing scores
// 0.231. The margin is not large, which is part of why this only runs when
// the keyword search has already found nothing - and why a note that slips
// through is labelled as a note and cited as one rather than quoted as a book.
const minNoteSimilarity = 0.4

// FoundNote is one of the reader's own notes, matched against a question.
type FoundNote struct {
	Annotation domain.Annotation
	BookFileID uuid.UUID
	WorkID     uuid.UUID
	Title      string
	Format     domain.Format
	Rank       float64
}

// SearchNotes finds a reader's own marks that bear on a question.
//
// Their OWN, and only their own. A note is the most personal thing in this
// application - it is what somebody thought, not what a book said - and a
// shared book does not share the margins. The filter is user_id and nothing
// else, deliberately not the readable-books view that governs passages: a note
// survives the access that allowed it, and its author should still find it.
//
// Keyword search rather than vectors. Notes are short and are usually looked
// for by a word somebody knows they wrote; embedding them would mean a second
// pipeline, a second model dependency and a second thing to re-run when the
// model changes, to find twenty things among eighty thousand passages.
func (p *Pool) SearchNotes(ctx context.Context, userID uuid.UUID, text string, limit int) ([]FoundNote, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: a note search needs a reader", store.ErrInvalidQuery)
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 20 {
		limit = 3
	}

	// ANY of the question's words, not all of them.
	//
	// websearch_to_tsquery ANDs its terms, which is right for a passage and
	// wrong for a note: a note is a sentence, and "is fuzz testing worth
	// doing?" asks for four words where the note beside that passage contains
	// three. The whole match failed on "worth". Swapping the operator inside
	// the parsed query keeps Postgres's own parsing and escaping - the string
	// never comes from the question directly.
	//
	// Loose matching needs a floor, and the floor does the work: on a real
	// library the question above scores 0.8 against the note beside the fuzz
	// testing passage and 0.0 against every other note anybody has written.
	rows, err := p.Query(ctx, `
		WITH q AS (
			SELECT replace(
			         websearch_to_tsquery('english', $2)::text, '&', '|'
			       )::tsquery AS query
		)
		SELECT a.id, a.type, a.selected_text, a.note,
		       a.start_major, a.start_path, a.start_offset,
		       a.created_at, bf.id, bf.format, w.id, w.title,
		       ts_rank_cd(a.tsv, q.query) AS rank
		FROM annotations a
		JOIN book_files bf ON bf.id = a.book_file_id AND bf.missing_since IS NULL
		JOIN editions e ON e.id = bf.edition_id
		JOIN works w ON w.id = e.work_id
		CROSS JOIN q
		WHERE a.user_id = $1
		  AND a.tsv @@ q.query
		  -- A bare highlight with no note is the book's words, which the
		  -- passage search already covers far better than this does.
		  AND length(TRIM(COALESCE(a.note, ''))) > 0
		  AND ts_rank_cd(a.tsv, q.query) >= $4
		ORDER BY ts_rank_cd(a.tsv, q.query) DESC, a.created_at DESC
		LIMIT $3`, userID, text, limit, minNoteRank)
	if err != nil {
		return nil, fmt.Errorf("searching notes: %w", mapError(err))
	}
	defer rows.Close()

	out, err := scanNotes(rows, userID)
	if err != nil {
		return nil, err
	}
	if len(out) > 0 {
		return out, nil
	}

	// Nothing matched a word. Try again allowing the words to be misspelled.
	//
	// Only as a fallback, because it costs a scan of this person's notes -
	// measured at roughly six microseconds each, so 6ms for somebody with a
	// thousand of them and 30ms for a heavy annotator with five thousand,
	// against a question that already takes around 2.7 seconds end to end. A
	// trigram index makes it slower rather than faster here: the index is over
	// everybody's notes and the user filter has already reduced the work to a
	// few hundred rows.
	//
	// Measured thresholds, on notes from a real library: the right note scores
	// 0.489 against a question with a word misspelled, and the worst unrelated
	// pairing scores 0.231.
	rows, err = p.Query(ctx, `
		SELECT a.id, a.type, a.selected_text, a.note,
		       a.start_major, a.start_path, a.start_offset,
		       a.created_at, bf.id, bf.format, w.id, w.title,
		       word_similarity($2, COALESCE(a.note, '') || ' ' || COALESCE(a.selected_text, '')) AS rank
		FROM annotations a
		JOIN book_files bf ON bf.id = a.book_file_id AND bf.missing_since IS NULL
		JOIN editions e ON e.id = bf.edition_id
		JOIN works w ON w.id = e.work_id
		WHERE a.user_id = $1
		  AND length(TRIM(COALESCE(a.note, ''))) > 0
		  AND word_similarity($2, COALESCE(a.note, '') || ' ' || COALESCE(a.selected_text, '')) >= $4
		ORDER BY rank DESC, a.created_at DESC
		LIMIT $3`, userID, text, limit, minNoteSimilarity)
	if err != nil {
		return nil, fmt.Errorf("searching notes for a near miss: %w", mapError(err))
	}
	defer rows.Close()
	return scanNotes(rows, userID)
}

// scanNotes reads either of the note searches, which return the same columns.
func scanNotes(rows pgx.Rows, userID uuid.UUID) ([]FoundNote, error) {
	var out []FoundNote
	for rows.Next() {
		var n FoundNote
		var selected, note *string
		var startMajor int
		if err := rows.Scan(&n.Annotation.ID, &n.Annotation.Type, &selected, &note,
			&startMajor, &n.Annotation.Range.Start.ElementPath, &n.Annotation.Range.Start.CharOffset,
			&n.Annotation.CreatedAt, &n.BookFileID, &n.Format, &n.WorkID, &n.Title,
			&n.Rank); err != nil {
			return nil, fmt.Errorf("scanning note: %w", err)
		}
		n.Annotation.SelectedText = deref(selected)
		n.Annotation.Note = deref(note)
		n.Annotation.BookFileID = n.BookFileID
		n.Annotation.UserID = userID
		applyFormat(&n.Annotation, n.Format)
		n.Annotation.Range.Start.Format = n.Format
		if n.Format == domain.FormatPDF {
			n.Annotation.Range.Start.Page = startMajor
		} else {
			n.Annotation.Range.Start.SpineIndex = startMajor
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ImportedMark is one highlight read out of a book file.
type ImportedMark struct {
	// Page is the physical page it was found on, 1-based.
	Page int
	// Occurrence disambiguates a phrase appearing more than once on a page,
	// the same way a mark made in the reader does.
	Occurrence int
	Text       string
	// Note is whatever the other reader stored with it, which is usually
	// nothing: of 1,051 underlines in one real book, not one carried a word.
	Note string
}

// ImportMarks replaces the marks previously imported from a book's own file.
//
// Replace rather than merge, because there is no stable identity to merge on:
// a PDF's annotations have no ids that survive an edit, so the only honest
// answer to "import again" is "whatever the file says now". Marks made in the
// reader are never touched - they are a different origin, and losing somebody's
// own note to a re-import would be unforgivable.
//
// Returns how many were stored.
func (p *Pool) ImportMarks(ctx context.Context, userID, bookFileID uuid.UUID, marks []ImportedMark) (int, error) {
	if userID == uuid.Nil {
		return 0, fmt.Errorf("%w: an import needs a reader", store.ErrInvalidQuery)
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("importing marks: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The book has to be one this person can actually open, or an import would
	// be a way to write rows about books they cannot see.
	var readable bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM readable_book_files
		    WHERE book_file_id = $1 AND user_id = $2)`, bookFileID, userID).Scan(&readable); err != nil {
		return 0, fmt.Errorf("checking the book: %w", mapError(err))
	}
	if !readable {
		return 0, errNotFound
	}

	// Re-importing gives every mark a new id, so the ones it replaces have to
	// be recorded as gone or a device that was offline keeps the old set
	// forever and shows the book's highlights twice. RETURNING captures
	// exactly which tombstones this import just wrote, rather than every
	// tombstone on this book - which could also include one the reader wrote
	// themselves, by deleting their own note, entirely unrelated to this
	// import and not this transaction's to re-stamp.
	clearedRows, err := tx.Query(ctx, `
		WITH gone AS (
		    DELETE FROM annotations
		    WHERE book_file_id = $1 AND user_id = $2 AND origin = 'pdf'
		    RETURNING id, user_id, book_file_id
		)
		INSERT INTO annotation_tombstones (annotation_id, user_id, book_file_id)
		SELECT id, user_id, book_file_id FROM gone
		ON CONFLICT (annotation_id) DO UPDATE SET deleted_at = now()
		RETURNING annotation_id`,
		bookFileID, userID)
	if err != nil {
		return 0, fmt.Errorf("clearing the last import: %w", mapError(err))
	}
	var clearedIDs []uuid.UUID
	for clearedRows.Next() {
		var id uuid.UUID
		if err := clearedRows.Scan(&id); err != nil {
			clearedRows.Close()
			return 0, fmt.Errorf("clearing the last import: %w", err)
		}
		clearedIDs = append(clearedIDs, id)
	}
	clearedRows.Close()
	if err := clearedRows.Err(); err != nil {
		return 0, fmt.Errorf("clearing the last import: %w", mapError(err))
	}

	stored := 0
	for _, m := range marks {
		text := strings.TrimSpace(m.Text)
		note := strings.TrimSpace(m.Note)
		if m.Page < 1 || (text == "" && note == "") {
			// A rectangle over a picture, with nothing written on it. A
			// highlight with no words cannot be found again or shown; a sticky
			// note with words is worth keeping even though it quotes nothing.
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO annotations
			    (user_id, book_file_id, type, origin,
			     start_major, start_path, start_offset,
			     end_major, end_path, end_offset, selected_text, note)
			VALUES ($1, $2, $3, 'pdf', $4, '', $5, $4, '', $5, $6, NULLIF($7, ''))`,
			userID, bookFileID, markType(note), m.Page, m.Occurrence,
			nullIfEmpty(text), note); err != nil {
			return 0, fmt.Errorf("storing an imported mark: %w", mapError(err))
		}
		stored++
	}

	// Stamped LAST, right before commit, with clock_timestamp() rather than
	// left at whatever now() gave each row as it was written.
	//
	// now() is fixed for the whole transaction - the moment it BEGAN - and a
	// row is not visible to any other transaction until this one COMMITs. A
	// first attempt at this fix stamped clock_timestamp() at each row's own
	// INSERT instead, which is wrong for every row except the last: that
	// value still could not become true until COMMIT, arbitrarily later than
	// when it was captured for an import with many rows left to go after it.
	// Capturing it here, in the last statements before COMMIT, keeps the gap
	// between "the timestamp says" and "the row is actually visible" down to
	// however long COMMIT itself takes - milliseconds - regardless of how
	// long the loop above took to get here. A sync pull's keyset cursor only
	// tolerates a gap of syncOverlap, a couple of seconds; a sizeable import
	// can easily run longer than that.
	if stored > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE annotations SET updated_at = clock_timestamp()
			WHERE book_file_id = $1 AND user_id = $2 AND origin = 'pdf'`,
			bookFileID, userID); err != nil {
			return 0, fmt.Errorf("stamping the import: %w", mapError(err))
		}
	}
	if len(clearedIDs) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE annotation_tombstones SET deleted_at = clock_timestamp()
			WHERE annotation_id = ANY($1)`,
			clearedIDs); err != nil {
			return 0, fmt.Errorf("stamping the cleared import: %w", mapError(err))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("importing marks: %w", mapError(err))
	}
	return stored, nil
}

// nullIfEmpty keeps an empty quotation out of the column, because the schema
// distinguishes "no text" from "the empty string" and a sticky note quotes
// nothing at all.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// markType is what an imported mark is: a note when the other reader stored
// words with it, a highlight otherwise. The schema requires a note to have a
// body, which is the same rule the reader follows.
func markType(note string) domain.AnnotationType {
	if strings.TrimSpace(note) != "" {
		return domain.AnnotationNote
	}
	return domain.AnnotationHighlight
}
