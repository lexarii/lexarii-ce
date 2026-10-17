// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// DuplicateGroup is a set of works that look like the same book.
type DuplicateGroup struct {
	// Reason says WHY these are grouped, because the answer changes how much
	// somebody should trust it. Merging is destructive enough that a person
	// should be told what the machine noticed.
	Reason string
	Works  []DuplicateWork
}

// DuplicateWork is one candidate, with enough to tell it from its twin.
type DuplicateWork struct {
	WorkID  uuid.UUID
	Title   string
	Authors []string
	Formats []string
	Files   int
	// Annotations and Progress say what would be lost by picking the wrong
	// one to keep. Nothing is lost by a merge - marks follow their file - but
	// somebody deciding deserves to see which copy they have actually read.
	Annotations int
	HasProgress bool
}

const (
	// ReasonISBN is two works whose editions carry the same ISBN. An ISBN
	// names one printing, so this is as close to certain as this gets.
	ReasonISBN = "the same ISBN"
	// ReasonTitleAuthor is the same title and at least one author in common.
	ReasonTitleAuthor = "the same title and author"
	// ReasonTitleOnly is the same title where neither has an author at all.
	//
	// Weaker, and offered anyway: a library where two in five books arrived
	// without an author is exactly where duplicates hide, and a person reading
	// two identical titles can tell in a second what no rule can.
	ReasonTitleOnly = "the same title, and neither has an author"
)

// Duplicates finds works that look like the same book.
//
// Books arrive as separate works when their FILENAMES differ: ingest matches a
// new file to an existing book by filename stem, so "97 Things.epub" and
// "97-things-every-java-programmer.pdf" become two books that are one book.
// That is the whole cause, and it is invisible until somebody scrolls past
// both.
//
// Reported, never merged automatically. Two printings of the same title, a
// translation, a second volume with a shared name - these are real and
// distinct, and a program that quietly welds them together loses somebody's
// library in a way no undo recovers.
func (p *Pool) Duplicates(ctx context.Context, viewer uuid.UUID) ([]DuplicateGroup, error) {
	if viewer == uuid.Nil {
		return nil, fmt.Errorf("%w: duplicates need a viewer", store.ErrInvalidQuery)
	}

	rows, err := p.Query(ctx, `
		-- Only works this person may merge (work_editors - the rule MergeWorks
		-- applies). A duplicate they cannot see is not theirs to merge, and
		-- neither is one they can only read: offering it would be offering a
		-- button that refuses.
		WITH visible AS (
		    SELECT DISTINCT e.work_id
		    FROM book_files bf
		    JOIN editions e ON e.id = bf.edition_id
		    JOIN work_editors we
		      ON we.work_id = e.work_id AND we.user_id = $1
		    WHERE bf.missing_since IS NULL
		), described AS (
		    SELECT w.id,
		           -- Titles compared with punctuation and case removed, which
		           -- is how a person compares them.
		           regexp_replace(lower(w.title), '[^a-z0-9]', '', 'g') AS title_key,
		           COALESCE((SELECT array_agg(lower(a.name) ORDER BY lower(a.name))
		                     FROM work_authors wa JOIN authors a ON a.id = wa.author_id
		                     WHERE wa.work_id = w.id), '{}') AS author_keys,
		           COALESCE((SELECT array_agg(DISTINCT COALESCE(NULLIF(e.isbn13, ''), e.isbn10))
		                     FROM editions e
		                     WHERE e.work_id = w.id
		                       AND COALESCE(NULLIF(e.isbn13, ''), e.isbn10) IS NOT NULL), '{}') AS isbns
		    FROM works w
		    JOIN visible v ON v.work_id = w.id
		    WHERE w.title <> $2
		)
		SELECT a.id, b.id,
		       CASE
		           WHEN a.isbns && b.isbns THEN $3
		           WHEN a.author_keys && b.author_keys THEN $4
		           ELSE $5
		       END AS reason
		FROM described a
		JOIN described b ON b.id > a.id
		WHERE a.title_key <> ''
		  AND (
		      -- Same printing, whatever either is called.
		      a.isbns && b.isbns
		      -- Or the same title, with an author in common or no authors at all.
		      OR (a.title_key = b.title_key
		          AND (a.author_keys && b.author_keys
		               OR (cardinality(a.author_keys) = 0 AND cardinality(b.author_keys) = 0)))
		  )
		ORDER BY reason, a.id`, viewer, PlaceholderTitle,
		ReasonISBN, ReasonTitleAuthor, ReasonTitleOnly)
	if err != nil {
		return nil, fmt.Errorf("looking for duplicates: %w", mapError(err))
	}
	defer rows.Close()

	// Pairs are gathered into groups, because three copies of one book is one
	// decision rather than three.
	var pairs []duplicatePair
	reasons := map[uuid.UUID]string{}
	for rows.Next() {
		var pr duplicatePair
		var reason string
		if err := rows.Scan(&pr.a, &pr.b, &reason); err != nil {
			return nil, fmt.Errorf("scanning a duplicate: %w", err)
		}
		pairs = append(pairs, pr)
		reasons[pr.a] = reason
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("looking for duplicates: %w", mapError(err))
	}
	if len(pairs) == 0 {
		return nil, nil
	}

	groups := groupPairs(pairs)
	out := make([]DuplicateGroup, 0, len(groups))
	for _, ids := range groups {
		works, err := p.duplicateWorks(ctx, viewer, ids)
		if err != nil {
			return nil, err
		}
		if len(works) < 2 {
			continue
		}
		out = append(out, DuplicateGroup{Reason: reasons[ids[0]], Works: works})
	}
	return out, nil
}

// duplicatePair is two works that look like each other.
type duplicatePair struct{ a, b uuid.UUID }

// groupPairs joins overlapping pairs into groups, so three copies of one book
// are one decision rather than three.
func groupPairs(pairs []duplicatePair) [][]uuid.UUID {
	parent := map[uuid.UUID]uuid.UUID{}
	var find func(uuid.UUID) uuid.UUID
	find = func(id uuid.UUID) uuid.UUID {
		p, ok := parent[id]
		if !ok || p == id {
			parent[id] = id
			return id
		}
		root := find(p)
		parent[id] = root
		return root
	}

	for _, pr := range pairs {
		ra, rb := find(pr.a), find(pr.b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	byRoot := map[uuid.UUID][]uuid.UUID{}
	seen := map[uuid.UUID]bool{}
	for _, pr := range pairs {
		for _, id := range []uuid.UUID{pr.a, pr.b} {
			if seen[id] {
				continue
			}
			seen[id] = true
			root := find(id)
			byRoot[root] = append(byRoot[root], id)
		}
	}

	out := make([][]uuid.UUID, 0, len(byRoot))
	for _, ids := range byRoot {
		out = append(out, ids)
	}
	return out
}

// duplicateWorks describes each candidate well enough to choose between them.
func (p *Pool) duplicateWorks(ctx context.Context, viewer uuid.UUID, ids []uuid.UUID) ([]DuplicateWork, error) {
	rows, err := p.Query(ctx, `
		SELECT w.id, w.title,
		       COALESCE((SELECT array_agg(a.name ORDER BY wa.position)
		                 FROM work_authors wa JOIN authors a ON a.id = wa.author_id
		                 WHERE wa.work_id = w.id), '{}'),
		       COALESCE((SELECT array_agg(DISTINCT bf.format::text)
		                 FROM book_files bf JOIN editions e ON e.id = bf.edition_id
		                 WHERE e.work_id = w.id), '{}'),
		       (SELECT count(*) FROM book_files bf JOIN editions e ON e.id = bf.edition_id
		        WHERE e.work_id = w.id),
		       (SELECT count(*) FROM annotations an
		        JOIN book_files bf ON bf.id = an.book_file_id
		        JOIN editions e ON e.id = bf.edition_id
		        WHERE e.work_id = w.id AND an.user_id = $2),
		       EXISTS (SELECT 1 FROM reading_progress rp
		               JOIN book_files bf ON bf.id = rp.book_file_id
		               JOIN editions e ON e.id = bf.edition_id
		               WHERE e.work_id = w.id AND rp.user_id = $2)
		FROM works w
		WHERE w.id = ANY($1)
		ORDER BY w.created_at`, ids, viewer)
	if err != nil {
		return nil, fmt.Errorf("describing duplicates: %w", mapError(err))
	}
	defer rows.Close()

	var out []DuplicateWork
	for rows.Next() {
		var d DuplicateWork
		if err := rows.Scan(&d.WorkID, &d.Title, &d.Authors, &d.Formats,
			&d.Files, &d.Annotations, &d.HasProgress); err != nil {
			return nil, fmt.Errorf("scanning a duplicate: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// MergeWorks folds one or more works into another.
//
// The files move, and everything hanging off a file moves with them without
// being touched: annotations, reading positions, chunks and embeddings all key
// on book_file_id, so a merge cannot lose a highlight or a bookmark. Only what
// keys on WORK has to be carried over by hand - tags, collection membership,
// and the provenance ledger.
//
// The kept work's own details win. Somebody chose which copy to keep, and
// overwriting their choice with the other copy's metadata would make the
// decision meaningless.
//
// Only works the person may edit (work_editors), on both sides. Merging is destructive
// and a borrowed book is not theirs to fold into anything.
func (p *Pool) MergeWorks(ctx context.Context, userID, keep uuid.UUID, merge []uuid.UUID) (int, error) {
	if len(merge) == 0 {
		return 0, nil
	}
	for _, id := range merge {
		if id == keep {
			return 0, fmt.Errorf("%w: a book cannot be merged into itself", store.ErrInvalidQuery)
		}
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("merging: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, id := range append([]uuid.UUID{keep}, merge...) {
		// work_editors is the one definition of who may, shared with editing.
		var owns bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM work_editors we
			    WHERE we.work_id = $1 AND we.user_id = $2)`, id, userID).Scan(&owns); err != nil {
			return 0, fmt.Errorf("checking ownership: %w", mapError(err))
		}
		if !owns {
			return 0, errNotFound
		}
	}

	// Tags and collections first, while the works still exist. ON CONFLICT
	// because the same tag on both copies is the normal case.
	if _, err := tx.Exec(ctx, `
		INSERT INTO work_tags (work_id, tag_id, source)
		SELECT $1, tag_id, source FROM work_tags WHERE work_id = ANY($2)
		ON CONFLICT DO NOTHING`, keep, merge); err != nil {
		return 0, fmt.Errorf("carrying tags over: %w", mapError(err))
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO collection_works (collection_id, work_id, added_at)
		SELECT collection_id, $1, added_at FROM collection_works WHERE work_id = ANY($2)
		ON CONFLICT DO NOTHING`, keep, merge); err != nil {
		return 0, fmt.Errorf("carrying collections over: %w", mapError(err))
	}

	// An author the other copy had and this one does not. The common case is
	// the reason this whole feature exists: one copy described properly and
	// one with nothing at all.
	if _, err := tx.Exec(ctx, `
		INSERT INTO work_authors (work_id, author_id, role, position)
		SELECT $1, wa.author_id, wa.role,
		       (SELECT COALESCE(max(position), -1) FROM work_authors WHERE work_id = $1)
		           + row_number() OVER (ORDER BY wa.position)
		FROM work_authors wa
		WHERE wa.work_id = ANY($2)
		  AND NOT EXISTS (SELECT 1 FROM work_authors k
		                  WHERE k.work_id = $1 AND k.author_id = wa.author_id)
		ON CONFLICT DO NOTHING`, keep, merge); err != nil {
		return 0, fmt.Errorf("carrying authors over: %w", mapError(err))
	}

	// Then the files, which is the merge itself.
	tag, err := tx.Exec(ctx,
		`UPDATE editions SET work_id = $1, updated_at = now() WHERE work_id = ANY($2)`,
		keep, merge)
	if err != nil {
		return 0, fmt.Errorf("moving the files over: %w", mapError(err))
	}

	// The emptied works. Their editions are gone, so nothing cascades to a
	// file: what is deleted here is a title and a description.
	if _, err := tx.Exec(ctx, `DELETE FROM works WHERE id = ANY($1)`, merge); err != nil {
		return 0, fmt.Errorf("removing the merged books: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("saving the merge: %w", mapError(err))
	}
	return int(tag.RowsAffected()), nil
}

// CatalogueFile is one downloadable file of a work.
type CatalogueFile struct {
	ID     uuid.UUID
	Format string
	Size   int64
}

// FilesForWorks lists the readable files of several works at once.
//
// One query for a whole page rather than one per book. The shelf's own query
// deliberately carries a single best file per work, which is right for a card
// showing one cover and one Read button - but a catalogue entry should offer
// every format, because the format this app prefers to render is not the one
// somebody wants on their e-reader.
func (p *Pool) FilesForWorks(ctx context.Context, viewer uuid.UUID, workIDs []uuid.UUID) (map[uuid.UUID][]CatalogueFile, error) {
	if len(workIDs) == 0 {
		return nil, nil
	}

	rows, err := p.Query(ctx, `
		SELECT e.work_id, bf.id, bf.format::text, bf.size_bytes
		FROM book_files bf
		JOIN editions e ON e.id = bf.edition_id
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = $2
		WHERE e.work_id = ANY($1)
		  AND bf.missing_since IS NULL
		  AND rbf.can_download
		ORDER BY e.work_id, bf.format`, workIDs, viewer)
	if err != nil {
		return nil, fmt.Errorf("listing a work's files: %w", mapError(err))
	}
	defer rows.Close()

	out := map[uuid.UUID][]CatalogueFile{}
	for rows.Next() {
		var workID uuid.UUID
		var f CatalogueFile
		if err := rows.Scan(&workID, &f.ID, &f.Format, &f.Size); err != nil {
			return nil, fmt.Errorf("scanning a file: %w", err)
		}
		out[workID] = append(out[workID], f)
	}
	return out, rows.Err()
}
