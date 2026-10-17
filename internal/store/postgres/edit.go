// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// WorkEdit is a correction somebody made by hand.
//
// Every field is a POINTER, and nil is not the same as empty. Nil means the
// edit did not mention this field and it keeps whatever it has; a pointer to
// "" means clear it, which is a real instruction - a book whose subtitle was
// read out of a title page as "Second Edition" needs it gone, not kept.
type WorkEdit struct {
	Title       *string
	Subtitle    *string
	Series      *string
	SeriesIndex *float64
	Language    *string
	Description *string
	// Authors replaces the list entirely, in order. An empty list removes
	// them, which is the honest answer for the 70 books in this library that
	// arrived with an author invented from a filename.
	Authors *[]string

	Publisher *string
	// Year is the year of publication, stored as the first of January because
	// that is what the column holds and a book's precise publication date is
	// not a thing anybody types.
	Year   *int
	ISBN13 *string
	ISBN10 *string
}

// Empty reports an edit that asks for nothing.
func (e WorkEdit) Empty() bool {
	return e.Title == nil && e.Subtitle == nil && e.Series == nil && e.SeriesIndex == nil &&
		e.Language == nil && e.Description == nil && e.Authors == nil &&
		e.Publisher == nil && e.Year == nil && e.ISBN13 == nil && e.ISBN10 == nil
}

// UpdateWork applies a hand correction and records who made it.
//
// The point of this is not the UPDATE statement, which is ordinary. It is the
// provenance: every field touched here is written to the ledger as `user`, and
// backfillMetadata already refuses to write over anything the ledger has a
// record of. So a corrected title survives the next scan, the next file added
// to the same work, and the next run of the enricher.
//
// That is the complaint this answers. Fixing a title in a library manager and
// finding it reverted after a rescan is the thing people give up over, and it
// happens because the correction and the import write to the same column with
// nothing to say which of them knows better. Here the ledger says.
//
// Only somebody who holds every file of this work may edit it, as
// work_editors defines. A person the book was shared with can read it and mark it up;
// renaming it for everybody is a different act. Their own notes are theirs,
// the library's facts are not.
func (p *Pool) UpdateWork(ctx context.Context, workID, userID uuid.UUID, edit WorkEdit) (WorkDetail, error) {
	if edit.Empty() {
		return p.Work(ctx, workID, userID)
	}

	// work_editors is the one definition of who may, shared with merging.
	var owns bool
	if err := p.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM work_editors we
		    WHERE we.work_id = $1 AND we.user_id = $2)`, workID, userID).Scan(&owns); err != nil {
		return WorkDetail{}, fmt.Errorf("checking who owns this book: %w", mapError(err))
	}
	if !owns {
		// The same answer as a work that does not exist, for somebody who can
		// see it but may not change it: saying "not yours" to a reader who was
		// shared a book tells them nothing they can act on.
		return WorkDetail{}, errNotFound
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return WorkDetail{}, fmt.Errorf("editing a book: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	touched := map[string]any{}

	if edit.Title != nil {
		title := strings.TrimSpace(*edit.Title)
		if title == "" {
			return WorkDetail{}, fmt.Errorf("%w: a book needs a title", store.ErrInvalidQuery)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE works SET title = $2, sort_title = $3, updated_at = now() WHERE id = $1`,
			workID, title, SortTitle(title)); err != nil {
			return WorkDetail{}, fmt.Errorf("saving the title: %w", mapError(err))
		}
		touched["title"] = title
	}

	for column, value := range map[string]*string{
		"subtitle":    edit.Subtitle,
		"series":      edit.Series,
		"language":    edit.Language,
		"description": edit.Description,
	} {
		if value == nil {
			continue
		}
		text := strings.TrimSpace(*value)
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`UPDATE works SET %s = NULLIF($2, ''), updated_at = now() WHERE id = $1`, column),
			workID, text); err != nil {
			return WorkDetail{}, fmt.Errorf("saving the %s: %w", column, mapError(err))
		}
		touched[column] = text
	}

	if edit.SeriesIndex != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE works SET series_index = $2, updated_at = now() WHERE id = $1`,
			workID, *edit.SeriesIndex); err != nil {
			return WorkDetail{}, fmt.Errorf("saving the series number: %w", mapError(err))
		}
	}

	if edit.Authors != nil {
		if err := replaceAuthors(ctx, tx, workID, *edit.Authors); err != nil {
			return WorkDetail{}, err
		}
		touched["authors"] = *edit.Authors
	}

	if err := editEditions(ctx, tx, workID, edit, touched); err != nil {
		return WorkDetail{}, err
	}

	// The ledger, which is what makes the edit stick.
	for _, field := range provenanceFields {
		value, ok := touched[field]
		if !ok {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO enrichment_records (work_id, field, value, source, accepted_at)
			VALUES ($1, $2, $3, 'user', now())`,
			workID, field, jsonValue(value)); err != nil {
			return WorkDetail{}, fmt.Errorf("recording who corrected %q: %w", field, mapError(err))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return WorkDetail{}, fmt.Errorf("saving the edit: %w", mapError(err))
	}
	return p.Work(ctx, workID, userID)
}

// editEditions writes the publisher, year and ISBNs onto every edition of the
// work.
//
// Every edition, because the screen that offers this shows one book with one
// publisher and one year. A work that genuinely has two printings with
// different publishers is a thing this model supports and this form does not
// pretend to; when that screen exists it will address an edition by id.
func editEditions(ctx context.Context, tx pgx.Tx, workID uuid.UUID, edit WorkEdit, touched map[string]any) error {
	for column, value := range map[string]*string{
		"publisher": edit.Publisher,
		"isbn13":    edit.ISBN13,
		"isbn10":    edit.ISBN10,
	} {
		if value == nil {
			continue
		}
		text := strings.TrimSpace(*value)
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`UPDATE editions SET %s = NULLIF($2, ''), updated_at = now() WHERE work_id = $1`, column),
			workID, text); err != nil {
			return fmt.Errorf("saving the %s: %w", column, mapError(err))
		}
		touched[column] = text
	}

	if edit.Year != nil {
		year := *edit.Year
		if year == 0 {
			if _, err := tx.Exec(ctx,
				`UPDATE editions SET published_on = NULL, updated_at = now() WHERE work_id = $1`,
				workID); err != nil {
				return fmt.Errorf("clearing the year: %w", mapError(err))
			}
			touched["publishedOn"] = ""
			return nil
		}
		if year < 1400 || year > 2200 {
			return fmt.Errorf("%w: %d is not a year a book was published in", store.ErrInvalidQuery, year)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE editions SET published_on = make_date($2, 1, 1), updated_at = now()
			WHERE work_id = $1`, workID, year); err != nil {
			return fmt.Errorf("saving the year: %w", mapError(err))
		}
		touched["publishedOn"] = fmt.Sprintf("%d", year)
	}
	return nil
}

// replaceAuthors sets a work's authors to exactly this list, in this order.
//
// Replace rather than add, because the case that matters is a wrong one: a
// book credited to "Www.Ebook-Dl.Com" because that was in the file needs that
// name GONE, and an additive link could never remove it.
//
// The author rows themselves are left behind. An author with no books is
// harmless, and deleting one would race with every other work that has just
// linked to it.
func replaceAuthors(ctx context.Context, tx pgx.Tx, workID uuid.UUID, names []string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM work_authors WHERE work_id = $1`, workID); err != nil {
		return fmt.Errorf("clearing the authors: %w", mapError(err))
	}

	contributors := make([]ingest.Contributor, 0, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			continue
		}
		contributors = append(contributors, ingest.Contributor{
			Name: strings.TrimSpace(name),
			Role: domain.RoleAuthor,
		})
	}
	return linkAuthors(ctx, tx, workID, contributors)
}
