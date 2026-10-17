// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// ExternalMetadata is what a catalogue said about one work.
//
// Only the fields a catalogue is good at. A title and an author are what the
// lookup was keyed ON, so taking them back from the answer would let a
// near-match rename somebody's book.
type ExternalMetadata struct {
	WorkID    uuid.UUID
	Publisher string
	Year      int
	ISBN13    string
	ISBN10    string
	PageCount int
	// Authors is used ONLY when the work has none at all.
	//
	// A catalogue naming a different translator or dropping a second author is
	// a worse answer than the file's own, so it never competes with one. But
	// 70 of the 168 books in the library this was measured against have no
	// author whatsoever - they are unattributed on the shelf, unfindable by
	// author, and they export as citations with nobody's name on them - and
	// for those a catalogue's answer is strictly better than silence.
	Authors []string
	// Source identifies the record that answered, so a value can be traced
	// back to the catalogue entry it came from.
	Source string
}

// ExternalOutcome says what a lookup actually changed.
type ExternalOutcome struct {
	Filled []string
	// Kept is what the catalogue also had and the library already knew. Not an
	// error and worth reporting: it is the evidence that the match was right.
	Kept []string
}

// NeedsExternalMetadata lists works missing the fields a catalogue can fill.
//
// A citation needs a publisher and a year, and most books here have neither:
// an EPUB's OPF usually carries them, a PDF almost never does.
func (p *Pool) NeedsExternalMetadata(ctx context.Context, viewer uuid.UUID, limit int) ([]WorkLookup, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	rows, err := p.Query(ctx, `
		SELECT DISTINCT ON (w.id) w.id, w.title, e.isbn13, e.isbn10
		FROM works w
		JOIN editions e ON e.work_id = w.id
		JOIN book_files bf ON bf.edition_id = e.id
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = $1
		WHERE bf.missing_since IS NULL
		  AND w.title <> $2
		  AND (e.publisher IS NULL OR e.publisher = ''
		       OR e.published_on IS NULL
		       OR (e.isbn13 IS NULL OR e.isbn13 = '')
		       -- A book with nobody's name on it. Unfindable by author, and
		       -- it exports as a citation with no author, which is the state
		       -- 70 of 168 books in a real library are in.
		       OR NOT EXISTS (SELECT 1 FROM work_authors wa WHERE wa.work_id = w.id))
		ORDER BY w.id
		LIMIT $3`, viewer, PlaceholderTitle, limit)
	if err != nil {
		return nil, fmt.Errorf("finding works needing metadata: %w", mapError(err))
	}
	defer rows.Close()

	var out []WorkLookup
	for rows.Next() {
		var w WorkLookup
		var isbn13, isbn10 *string
		if err := rows.Scan(&w.WorkID, &w.Title, &isbn13, &isbn10); err != nil {
			return nil, fmt.Errorf("scanning work: %w", err)
		}
		w.ISBN13, w.ISBN10 = deref(isbn13), deref(isbn10)
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finding works needing metadata: %w", mapError(err))
	}

	// The authors, for the title search to check its match against.
	for i := range out {
		names, err := p.workAuthorNames(ctx, out[i].WorkID)
		if err != nil {
			return nil, err
		}
		out[i].Authors = names
	}
	return out, nil
}

// WorkLookup is a work as a catalogue query sees it.
type WorkLookup struct {
	WorkID  uuid.UUID
	Title   string
	Authors []string
	ISBN13  string
	ISBN10  string
}

func (p *Pool) workAuthorNames(ctx context.Context, workID uuid.UUID) ([]string, error) {
	rows, err := p.Query(ctx, `
		SELECT a.name FROM work_authors wa
		JOIN authors a ON a.id = wa.author_id
		WHERE wa.work_id = $1
		ORDER BY wa.position`, workID)
	if err != nil {
		return nil, fmt.Errorf("reading authors: %w", mapError(err))
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scanning author: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ApplyExternalMetadata fills EMPTY edition fields from a catalogue.
//
// Empty only. A value the file itself carried is what the publisher wrote, and
// a catalogue is a third party describing the same book from somewhere else -
// often a different printing. Overwriting here would quietly replace the
// publisher's own year with a reprint's, which is the kind of wrong that never
// gets noticed.
//
// Every value written is recorded as SourceExternal, so the work detail can say
// where it came from and a person can reject it.
func (p *Pool) ApplyExternalMetadata(ctx context.Context, in ExternalMetadata) (ExternalOutcome, error) {
	var out ExternalOutcome

	tx, err := p.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("applying catalogue metadata: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// COALESCE-and-NULLIF per column: fill when empty, leave alone otherwise,
	// in one statement so two lookups cannot interleave into half an update.
	// The RETURNING tells us which ones actually changed.
	var publisherWas, isbn13Was, isbn10Was *string
	var yearWas *int
	var pagesWas *int
	err = tx.QueryRow(ctx, `
		WITH before AS (
			SELECT id, publisher, isbn13, isbn10,
			       EXTRACT(YEAR FROM published_on)::int AS year, page_count
			FROM editions WHERE work_id = $1
			ORDER BY created_at
			LIMIT 1
		)
		UPDATE editions e SET
			publisher    = COALESCE(NULLIF(e.publisher, ''), NULLIF($2, '')),
			isbn13       = COALESCE(NULLIF(e.isbn13, ''), NULLIF($3, '')),
			isbn10       = COALESCE(NULLIF(e.isbn10, ''), NULLIF($4, '')),
			published_on = COALESCE(e.published_on,
			                        CASE WHEN $5::int > 0
			                             THEN make_date($5::int, 1, 1) END),
			page_count   = COALESCE(e.page_count, NULLIF($6::int, 0)),
			updated_at   = now()
		FROM before b
		WHERE e.id = b.id
		RETURNING b.publisher, b.isbn13, b.isbn10, b.year, b.page_count`,
		in.WorkID, in.Publisher, in.ISBN13, in.ISBN10, in.Year, in.PageCount,
	).Scan(&publisherWas, &isbn13Was, &isbn10Was, &yearWas, &pagesWas)
	if err != nil {
		return out, fmt.Errorf("updating the edition: %w", mapError(err))
	}

	// What was empty before and is offered now has been filled; what was
	// already there was kept.
	type field struct {
		name    string
		wasSet  bool
		offered bool
	}
	for _, f := range []field{
		{"publisher", deref(publisherWas) != "", in.Publisher != ""},
		{"isbn13", deref(isbn13Was) != "", in.ISBN13 != ""},
		{"isbn10", deref(isbn10Was) != "", in.ISBN10 != ""},
		{"publishedOn", yearWas != nil, in.Year > 0},
		{"pageCount", pagesWas != nil, in.PageCount > 0},
	} {
		switch {
		case !f.offered:
		case f.wasSet:
			out.Kept = append(out.Kept, f.name)
		default:
			out.Filled = append(out.Filled, f.name)
		}
	}

	// Authors, when the book has none. Checked inside the transaction, so two
	// lookups running at once cannot both decide the work was empty.
	if len(in.Authors) > 0 {
		var has bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM work_authors WHERE work_id = $1)`,
			in.WorkID).Scan(&has); err != nil {
			return out, fmt.Errorf("checking for authors: %w", mapError(err))
		}
		switch {
		case has:
			out.Kept = append(out.Kept, "authors")
		default:
			contributors := make([]ingest.Contributor, 0, len(in.Authors))
			for _, name := range in.Authors {
				if strings.TrimSpace(name) == "" {
					continue
				}
				contributors = append(contributors, ingest.Contributor{
					Name: strings.TrimSpace(name),
					Role: domain.RoleAuthor,
				})
			}
			if len(contributors) > 0 {
				if err := linkAuthors(ctx, tx, in.WorkID, contributors); err != nil {
					return out, err
				}
				out.Filled = append(out.Filled, "authors")
			}
		}
	}

	// The ledger stores the VALUE, as every other record does, with the
	// catalogue entry that supplied it in model_id. Storing the source URL as
	// the value would make the work detail show a catalogue key where a person
	// expects to see a publisher.
	values := map[string]string{
		"publisher":   in.Publisher,
		"isbn13":      in.ISBN13,
		"isbn10":      in.ISBN10,
		"publishedOn": year(in.Year),
		"pageCount":   count(in.PageCount),
		"authors":     strings.Join(in.Authors, "; "),
	}
	for _, name := range out.Filled {
		if err := recordExternal(ctx, tx, in.WorkID, name, values[name], in.Source); err != nil {
			return out, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("applying catalogue metadata: %w", mapError(err))
	}
	return out, nil
}

func year(y int) string {
	if y <= 0 {
		return ""
	}
	return strconv.Itoa(y)
}

func count(n int) string {
	if n <= 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// recordExternal writes the provenance row for a filled field.
//
// Accepted at the moment it is written, because it was applied: the ledger
// records what happened, and a row that says "suggested" for a value already
// on the work would be a lie the review queue then offers to a person.
func recordExternal(ctx context.Context, tx pgx.Tx, workID uuid.UUID, field, value, source string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO enrichment_records (work_id, field, value, source, model_id, accepted_at)
		VALUES ($1, $2, to_jsonb($3::text), $4, NULLIF($5, ''), now())`,
		workID, field, value, domain.SourceExternal, source)
	if err != nil {
		return fmt.Errorf("recording where %q came from: %w", field, mapError(err))
	}
	return nil
}
