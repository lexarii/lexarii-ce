// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// SetCover records where a file's cover image was written, and returns the name
// of the one it replaced.
//
// The previous name is returned rather than discarded so the caller can delete
// the file. Every cover gets a fresh generated name - which is what stops a
// replacement being served from a browser cache under the old URL - so without
// this every re-render would leave its predecessor on disk forever.
//
// Scoped to somebody who can READ the book rather than to its owner. A cover is
// derived from the first page, and anyone who may open the book has already
// seen that page; making it owner-only would mean a shared book stayed blank
// for the person it was shared with, which is the opposite of the point.
func (p *Pool) SetCover(ctx context.Context, bookFileID, viewer uuid.UUID, path string) (string, error) {
	// A CTE rather than a subquery inside RETURNING: reading the old value of a
	// row the same statement is writing depends on snapshot rules subtle enough
	// that the next person to read it would have to go and check. `old` is
	// plainly evaluated before the update, and `updated` plainly says whether
	// the write was allowed.
	var previous *string
	var updated int
	err := p.QueryRow(ctx, `
		WITH old AS (
			SELECT cover_path FROM book_files WHERE id = $1
		), updated AS (
			UPDATE book_files SET cover_path = $3
			WHERE id = $1
			  AND EXISTS (SELECT 1 FROM readable_book_files
			               WHERE book_file_id = $1 AND user_id = $2)
			RETURNING 1
		)
		SELECT (SELECT cover_path FROM old), (SELECT count(*) FROM updated)`,
		bookFileID, viewer, path).Scan(&previous, &updated)
	if err != nil {
		return "", fmt.Errorf("recording cover: %w", mapError(err))
	}
	if updated == 0 {
		return "", errNotFound
	}
	if previous == nil || *previous == path {
		return "", nil
	}
	return *previous, nil
}

// Cover returns where a file's cover is stored, for somebody allowed to see it.
func (p *Pool) Cover(ctx context.Context, bookFileID, viewer uuid.UUID) (string, error) {
	var path *string
	err := p.QueryRow(ctx, `
		SELECT bf.cover_path FROM book_files bf
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = $2
		WHERE bf.id = $1`, bookFileID, viewer).Scan(&path)
	if err != nil {
		return "", mapError(err)
	}
	if path == nil || *path == "" {
		return "", errNotFound
	}
	return *path, nil
}

// FilesNeedingCovers lists readable files that have no cover yet.
//
// The client asks for this and renders them itself, because the first page of a
// PDF can only be rasterised where a rasteriser exists. Bounded, and ordered by
// the library's own sort so that the covers which appear first are the ones a
// person is most likely to be looking at.
func (p *Pool) FilesNeedingCovers(ctx context.Context, viewer uuid.UUID, limit int) ([]CoverTarget, error) {
	if limit <= 0 || limit > 200 {
		limit = 24
	}

	rows, err := p.Query(ctx, `
		SELECT bf.id, bf.format
		FROM book_files bf
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = $1
		JOIN editions e ON e.id = bf.edition_id
		JOIN works w ON w.id = e.work_id
		WHERE bf.cover_path IS NULL
		  AND bf.missing_since IS NULL
		  AND bf.status = 'ready'
		  AND bf.format = 'pdf'
		ORDER BY w.sort_title
		LIMIT $2`, viewer, limit)
	if err != nil {
		return nil, fmt.Errorf("finding files without covers: %w", mapError(err))
	}
	defer rows.Close()

	var out []CoverTarget
	for rows.Next() {
		var t CoverTarget
		if err := rows.Scan(&t.BookFileID, &t.Format); err != nil {
			return nil, fmt.Errorf("scanning cover target: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountFilesNeedingCovers is how many are left in total, not in this batch.
//
// The batch is 24 and a library is hundreds, so a client that counts what it
// was handed can only ever say "24 left" - over and over, as it works through
// them. That is worse than silence: it looks stuck. The whole number is what
// somebody needs to decide whether to leave the tab open.
func (p *Pool) CountFilesNeedingCovers(ctx context.Context, viewer uuid.UUID) (int, error) {
	var n int
	err := p.QueryRow(ctx, `
		SELECT count(*)
		FROM book_files bf
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = $1
		WHERE bf.cover_path IS NULL
		  AND bf.missing_since IS NULL
		  AND bf.status = 'ready'
		  AND bf.format = 'pdf'`, viewer).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting files without covers: %w", mapError(err))
	}
	return n, nil
}

// CoverTarget is a file that still needs a cover rendered.
type CoverTarget struct {
	BookFileID uuid.UUID
	Format     string
}
