// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// IngestInput is everything needed to persist one scanned and parsed file.
type IngestInput struct {
	ContentHash string
	Path        string
	Format      domain.Format
	SizeBytes   int64
	Status      domain.IngestStatus
	IngestError string
	Warnings    []string

	// OriginalFilename is set for uploaded files. Storage is content-addressed,
	// so the path carries no name; without this, same-book merging - which
	// derives its stem from the filename - would stop working for uploads.
	//
	// It is the name OwnerID gave the file, and is recorded on their holding.
	// The file keeps the name it FIRST arrived with, for merging; a later
	// upload of the same bytes never renames somebody else's book.
	OriginalFilename string
	// Uploaded marks a file in Lexarii's own managed directory rather than the
	// user's read-only library directory.
	Uploaded bool

	// OwnerID is who added this file, and becomes one of its holders. Books
	// belong to people: a file nobody holds is visible to nobody, which is the
	// safe direction to fail but not a useful outcome, so every path that
	// ingests a file has to say who it is for. Anybody else who uploaded the
	// same bytes meanwhile left a claim, and is made a holder by the same
	// ingest.
	OwnerID uuid.UUID
	// PathExists reports whether a path exists, checked under the file's lock
	// before the record is pointed at it. Nil means "assume it does", which is
	// what a caller with no filesystem - a test - wants.
	PathExists func(path string) bool

	// CoverPath is the stored filename of a cover extracted during parsing,
	// relative to the cover directory. Set for an EPUB, whose cover is a file
	// in the archive; empty for a PDF, whose cover is a page and has to be
	// rendered by a client that owns a rasteriser.
	CoverPath string

	// Result is nil when parsing failed. The file is still recorded, so the UI
	// can show it with its error rather than silently omitting it.
	Result *ingest.Result
}

// IngestOutcome reports what an upsert did.
type IngestOutcome struct {
	BookFileID uuid.UUID
	WorkID     uuid.UUID
	EditionID  uuid.UUID

	// Created is true when this file had not been seen before.
	Created bool
	// Moved is true when a known file appeared at a new path.
	Moved bool
	// PreviousPath is set when Moved is true.
	PreviousPath string
	// ReplacedCover is the stored name of a cover this ingest displaced, so the
	// caller can delete the file. Covers get a fresh name each time, so without
	// this a re-extract leaves its predecessor on disk forever.
	ReplacedCover string
}

// UpsertBookFile persists one scanned file, idempotently.
//
// Identity is the content hash, not the path. Re-running a scan over an
// unchanged library updates timestamps and nothing else; a file that has been
// moved or renamed is recognised and its path corrected rather than duplicated.
//
// Everything happens in one transaction: a partially written book - an edition
// with no file, a work with no edition - is worse than no book at all.
func (p *Pool) UpsertBookFile(ctx context.Context, in IngestInput) (IngestOutcome, error) {
	var out IngestOutcome

	tx, err := p.Begin(ctx)
	if err != nil {
		return out, fmt.Errorf("begin: %w", mapError(err))
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	// Before the lookup: an upload landing now must either see the row this
	// creates or leave a claim this transaction then drains, never neither.
	if err := lockContentHash(ctx, tx, in.ContentHash); err != nil {
		return out, err
	}
	// An extraction of bytes an administrator removed - queued before the
	// removal, or by a scan that raced it - records nothing at all.
	if err := refuseBlocked(ctx, tx, in.ContentHash); err != nil {
		return out, err
	}

	existing, err := findBookFileByHash(ctx, tx, in.ContentHash)
	switch {
	case err == nil:
		out, err = updateExisting(ctx, tx, existing, in)
		if err != nil {
			return out, err
		}
	case isNotFound(err):
		out, err = insertNewBook(ctx, tx, in)
		if err != nil {
			return out, err
		}
	default:
		return out, fmt.Errorf("looking up content hash: %w", err)
	}

	if err := recordHoldings(ctx, tx, out.BookFileID, in); err != nil {
		return out, err
	}
	// A file nobody holds is archived, never left on no shelf at all: the
	// person it was for may have deleted their account while it waited to be
	// extracted. Archived, an administrator can see it and hand it on.
	if err := archiveIfUnheld(ctx, tx, out.BookFileID); err != nil {
		return out, err
	}

	if err := tx.Commit(ctx); err != nil {
		return out, fmt.Errorf("commit: %w", mapError(err))
	}
	return out, nil
}

type existingFile struct {
	id        uuid.UUID
	editionID uuid.UUID
	workID    uuid.UUID
	path      string
	coverPath string
	origin    string
	missing   bool
	archived  bool
}

func findBookFileByHash(ctx context.Context, tx pgx.Tx, hash string) (existingFile, error) {
	var f existingFile
	var cover *string
	err := tx.QueryRow(ctx, `
		SELECT bf.id, bf.edition_id, e.work_id, bf.path, bf.cover_path,
		       bf.origin::text, bf.missing_since IS NOT NULL, bf.archived_at IS NOT NULL
		FROM book_files bf
		JOIN editions e ON e.id = bf.edition_id
		WHERE bf.content_hash = $1`, hash,
	).Scan(&f.id, &f.editionID, &f.workID, &f.path, &cover, &f.origin, &f.missing, &f.archived)
	f.coverPath = deref(cover)
	return f, mapError(err)
}

// updateExisting refreshes a file already known by content hash.
//
// Embedded metadata is never REWRITTEN here - the user may have corrected a
// title, and re-running a scan must not silently undo that - but empty fields
// are filled, which is a different thing and is what backfillMetadata already
// guarantees: it writes only where there is nothing, and treats the provenance
// ledger as the record of what counts as something.
//
// Filling them matters the day a format starts working. A Kindle book shelved
// before MOBI could be read has no title and no author; the rescan now parses
// it properly and would, without this, have thrown the publisher's own title
// away and left the book called "Untitled" forever - which is precisely the
// outcome the change that made it re-extractable was meant to fix.
func updateExisting(ctx context.Context, tx pgx.Tx, f existingFile, in IngestInput) (IngestOutcome, error) {
	out := IngestOutcome{
		BookFileID: f.id,
		EditionID:  f.editionID,
		WorkID:     f.workID,
	}
	// Where the file lives is the file's, not the latest ingest's. A scan that
	// finds a moved file updates the path, and so does an upload re-landing in
	// Lexarii's own storage. Bytes arriving by the OTHER route - somebody
	// uploading a book that also sits in the library directory - leave the
	// path alone, unless the file it points at has gone missing, when the new
	// copy is the one that exists.
	//
	// Never onto a path that does not exist - checked here, under the file's
	// lock, because the archive may have moved the bytes after this ingest
	// read them - and never an upload's path onto an archived file. Only
	// MoveArchived moves an archived file Lexarii stores; an extraction that
	// finishes after its holder let the book go would otherwise point the
	// record back at where the bytes were, and report the archive's copy as
	// replaced, to be removed. A scanned file is never moved by the archive,
	// so its record follows it round the library directory as ever.
	path, origin := f.path, f.origin
	exists := in.PathExists == nil || in.PathExists(in.Path)
	frozen := f.archived && in.Uploaded
	if !frozen && exists && (f.origin == originOf(in) || f.missing) {
		path, origin = in.Path, originOf(in)
	}
	if f.path != path {
		out.Moved = true
		out.PreviousPath = f.path
	}
	// A newly extracted cover displaces whatever was there. The caller deletes
	// the file: nothing references it once this statement commits.
	if in.CoverPath != "" && f.coverPath != "" && f.coverPath != in.CoverPath {
		out.ReplacedCover = f.coverPath
	}

	unitCount := unitCountOf(in.Result)
	_, err := tx.Exec(ctx, `
		UPDATE book_files
		SET path = $2,
		    origin = $10::file_origin,
		    status = $3,
		    ingest_error = NULLIF($4, ''),
		    ingest_warnings = $5,
		    unit_count = COALESCE($6, unit_count),
		    size_bytes = $7,
		    extracted_at = CASE WHEN $3 = 'ready'::ingest_status THEN now() ELSE extracted_at END,
		    last_seen_at = now(),
		    missing_since = NULL,
		    -- The name the file first arrived with is kept. Each holder's own
		    -- name is on their holding; this one only fills a blank, for
		    -- merging by stem - and only for a file Lexarii stores, whose path
		    -- is a hash. A scanned file's stem comes from its own path, and an
		    -- uploader's name for it must not become the name background work
		    -- and the archive know it by.
		    original_filename = CASE WHEN original_filename = '' AND $10::file_origin = 'uploaded'
		                             THEN $8 ELSE original_filename END,
		    -- Only when one was extracted this time. A re-ingest that finds no
		    -- cover must not erase the one already there, and for a PDF the
		    -- cover was rendered by a client and never comes through here.
		    cover_path = COALESCE(NULLIF($9, ''), cover_path)
		WHERE id = $1`,
		f.id, path, in.Status, in.IngestError, warningsOf(in), unitCount, in.SizeBytes,
		in.OriginalFilename, in.CoverPath, origin,
	)
	if err != nil {
		return out, fmt.Errorf("updating book file: %w", mapError(err))
	}

	// Only what the file itself declared, and only into blanks.
	if in.Result != nil {
		if err := backfillMetadata(ctx, tx, f.workID, f.editionID, in.Result.Metadata); err != nil {
			return out, err
		}
	}
	return out, nil
}

// insertNewBook creates the Work, Edition, BookFile, and Authors for a file
// never seen before.
func insertNewBook(ctx context.Context, tx pgx.Tx, in IngestInput) (IngestOutcome, error) {
	var out IngestOutcome
	out.Created = true

	meta := metadataOf(in)

	// An edition may already exist for this ISBN: a second format of a book
	// already in the library is the same Work and the same Edition, with a new
	// file attached.
	//
	// Matching on anything looser than an ISBN is a guess. Two books with the
	// same title and author are routinely different works, and wrongly merging
	// them is far harder for a user to undo than leaving them separate. Merging
	// stays an explicit action.
	//
	// A file only joins an edition the person adding it already holds a file
	// of - an upload's uploader, a scan's administrator. Matching across the
	// whole installation would let somebody attach a file to another person's
	// book by giving it that book's ISBN or filename, seeing it appear on
	// their shelf - which says somebody here has it - and it would leave a
	// work whose files no single person holds, which nobody may then edit or
	// merge (work_editors). ISBNs are not unique (migration 35), so separate
	// editions of one book are fine. With nobody named - a scan nobody asked
	// for - it matches as it always has.
	scope := in.OwnerID
	editionID, workID, found, err := findEditionByISBN(ctx, tx, meta.ISBN13, scope)
	if err != nil {
		return out, err
	}

	// Failing that, an identical filename stem in a different format. PDFs
	// essentially never carry an ISBN, so without this a book owned as both PDF
	// and EPUB appears on the shelf twice.
	if !found {
		stemSource := in.Path
		if in.OriginalFilename != "" {
			// Uploaded files are stored content-addressed, so the path is a
			// hash. The name the user gave is the only usable stem.
			stemSource = in.OriginalFilename
		}
		editionID, workID, found, err = findEditionByStem(ctx, tx, stemSource, in.Format, scope)
		if err != nil {
			return out, err
		}
	}

	if !found {
		workID, err = insertWork(ctx, tx, meta)
		if err != nil {
			return out, err
		}
		editionID, err = insertEdition(ctx, tx, workID, meta)
		if err != nil {
			return out, err
		}
		if err := linkAuthors(ctx, tx, workID, meta.Authors); err != nil {
			return out, err
		}
		if err := linkSubjects(ctx, tx, workID, meta.Subjects); err != nil {
			return out, err
		}
		if err := recordProvenance(ctx, tx, workID, meta); err != nil {
			return out, err
		}
	}

	if found {
		// This file joined an existing edition. If it carries metadata the
		// edition lacks, fill the gaps.
		//
		// Without this the result depends on which worker happened to finish
		// first: a metadata-less PDF ingested before its EPUB twin would leave
		// the book permanently titled "Untitled" with no ISBN and no publisher,
		// even though the EPUB was sitting right there carrying all three.
		if err := backfillMetadata(ctx, tx, workID, editionID, meta); err != nil {
			return out, err
		}
	}

	out.WorkID = workID
	out.EditionID = editionID

	err = tx.QueryRow(ctx, `
		INSERT INTO book_files
			(edition_id, path, content_hash, format, size_bytes, status,
			 ingest_error, ingest_warnings, unit_count, extracted_at,
			 original_filename, origin, cover_path)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9,
		        CASE WHEN $6 = 'ready'::ingest_status THEN now() ELSE NULL END,
		        $10, $11, NULLIF($12, ''))
		RETURNING id`,
		editionID, in.Path, in.ContentHash, in.Format, in.SizeBytes,
		in.Status, in.IngestError, warningsOf(in), unitCountOf(in.Result),
		in.OriginalFilename, originOf(in), in.CoverPath,
	).Scan(&out.BookFileID)
	if err != nil {
		return out, fmt.Errorf("inserting book file: %w", mapError(err))
	}

	return out, nil
}

// backfillMetadata fills fields the existing work and edition are missing.
//
// It only ever writes where there is nothing already. A field that any file has
// declared, or that the user has corrected, is left alone - the provenance
// ledger is the test for "declared", so a value that was genuinely present is
// never displaced by a later file.
func backfillMetadata(ctx context.Context, tx pgx.Tx, workID, editionID uuid.UUID, m ingest.Metadata) error {
	declared, err := declaredFields(ctx, tx, workID)
	if err != nil {
		return err
	}

	// Work-level text fields. COALESCE/NULLIF keeps a populated column as it is;
	// the provenance check keeps a deliberately-blanked one blank.
	if title := strings.TrimSpace(m.Title); title != "" && !declared["title"] {
		if _, err := tx.Exec(ctx,
			`UPDATE works SET title = $2, sort_title = $3 WHERE id = $1`,
			workID, title, SortTitle(title)); err != nil {
			return fmt.Errorf("backfilling title: %w", mapError(err))
		}
	}
	for field, value := range map[string]string{
		"subtitle":    m.Subtitle,
		"series":      m.Series,
		"language":    m.Language,
		"description": m.Description,
	} {
		if strings.TrimSpace(value) == "" || declared[field] {
			continue
		}
		column := map[string]string{
			"subtitle": "subtitle", "series": "series",
			"language": "language", "description": "description",
		}[field]
		if _, err := tx.Exec(ctx,
			fmt.Sprintf(`UPDATE works SET %s = $2 WHERE %s IS NULL AND id = $1`, column, column),
			workID, value); err != nil {
			return fmt.Errorf("backfilling %s: %w", field, mapError(err))
		}
	}

	// Edition-level identifiers. An ISBN is the strongest identity signal there
	// is, and a PDF-first ingest would otherwise discard the EPUB's.
	if _, err := tx.Exec(ctx, `
		UPDATE editions SET
			isbn13 = COALESCE(isbn13, NULLIF($2, '')),
			isbn10 = COALESCE(isbn10, NULLIF($3, '')),
			publisher = COALESCE(publisher, NULLIF($4, '')),
			language = COALESCE(language, NULLIF($5, ''))
		WHERE id = $1`,
		editionID, m.ISBN13, m.ISBN10, m.Publisher, m.Language); err != nil {
		return fmt.Errorf("backfilling edition: %w", mapError(err))
	}

	// Authors and subjects are additive: linking is already idempotent, and a
	// work with no authors gaining them is strictly better.
	if !declared["authors"] {
		if err := linkAuthors(ctx, tx, workID, m.Authors); err != nil {
			return err
		}
	}
	if err := linkSubjects(ctx, tx, workID, m.Subjects); err != nil {
		return err
	}

	// Record provenance for whatever was just filled, so the next file in turn
	// leaves it alone.
	filtered := m
	filtered.Provenance = make(map[string]domain.MetadataSource, len(m.Provenance))
	for field, source := range m.Provenance {
		if !declared[field] {
			filtered.Provenance[field] = source
		}
	}
	return recordProvenance(ctx, tx, workID, filtered)
}

// declaredFields reports which metadata fields already have a recorded source,
// meaning some file genuinely declared them or the user set them.
func declaredFields(ctx context.Context, tx pgx.Tx, workID uuid.UUID) (map[string]bool, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT field FROM enrichment_records WHERE work_id = $1 AND rejected_at IS NULL`, workID)
	if err != nil {
		return nil, fmt.Errorf("reading declared fields: %w", mapError(err))
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var field string
		if err := rows.Scan(&field); err != nil {
			return nil, fmt.Errorf("scanning declared field: %w", err)
		}
		out[field] = true
	}
	return out, rows.Err()
}

// findEditionByISBN finds an edition carrying an ISBN. With a scope, only one
// that person holds a file of is a candidate, and an edition that is not
// theirs is not found - indistinguishably from one that does not exist. ISBNs
// are not unique (migration 35): two people's editions of one book may both
// carry it, so the earliest candidate wins.
func findEditionByISBN(ctx context.Context, tx pgx.Tx, isbn13 string, scope uuid.UUID) (editionID, workID uuid.UUID, found bool, err error) {
	if isbn13 == "" {
		return uuid.Nil, uuid.Nil, false, nil
	}
	err = tx.QueryRow(ctx, `
		SELECT e.id, e.work_id
		FROM editions e
		WHERE e.isbn13 = $1
		  AND ($2::uuid IS NULL OR EXISTS (
		          SELECT 1 FROM book_files bf
		          JOIN book_file_holders h ON h.book_file_id = bf.id
		          WHERE bf.edition_id = e.id AND h.user_id = $2))
		ORDER BY e.created_at, e.id
		LIMIT 1`, isbn13, nullableUUID(scope),
	).Scan(&editionID, &workID)
	switch {
	case err == nil:
		return editionID, workID, true, nil
	case isNotFound(mapError(err)):
		return uuid.Nil, uuid.Nil, false, nil
	default:
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("looking up isbn: %w", mapError(err))
	}
}

// MinStemMatchLength is the shortest filename stem trusted for a merge.
//
// Short stems collide by accident: two folders each holding "notes.pdf" and
// "notes.epub" are almost certainly unrelated documents. Real book filenames
// are long - the median in the corpus this was measured against is 29
// characters - so the threshold costs nothing and removes the whole class of
// accidental merges.
const MinStemMatchLength = 12

// findEditionByStem finds an edition backed by a file with the same filename
// stem in a different format.
//
// Different format is required, not incidental. Two files with the same name
// AND the same format are two distinct things - a duplicate download, two
// printings, two unrelated papers - and merging them would be wrong.
//
// With a scope, only a file that person holds is a candidate, for the reasons
// given in insertNewBook.
func findEditionByStem(ctx context.Context, tx pgx.Tx, path string, format domain.Format, scope uuid.UUID) (editionID, workID uuid.UUID, found bool, err error) {
	stem := StemKey(path)
	if len(stem) < MinStemMatchLength {
		return uuid.Nil, uuid.Nil, false, nil
	}

	err = tx.QueryRow(ctx, `
		SELECT bf.edition_id, e.work_id
		FROM book_files bf
		JOIN editions e ON e.id = bf.edition_id
		WHERE bf.stem_key = $1 AND bf.format <> $2
		  AND ($3::uuid IS NULL OR EXISTS (
		          SELECT 1 FROM book_file_holders h
		          WHERE h.book_file_id = bf.id AND h.user_id = $3))
		ORDER BY bf.created_at
		LIMIT 1`, stem, format, nullableUUID(scope),
	).Scan(&editionID, &workID)
	switch {
	case err == nil:
		return editionID, workID, true, nil
	case isNotFound(mapError(err)):
		return uuid.Nil, uuid.Nil, false, nil
	default:
		return uuid.Nil, uuid.Nil, false, fmt.Errorf("looking up filename stem: %w", mapError(err))
	}
}

// PlaceholderTitle marks a work whose file declared no title.
//
// Exported because it is not a title: enrichment has to be able to tell it from
// one, or a model would treat "Untitled" as a publisher fact and refuse to
// replace it - leaving two in five books in a real library permanently untitled.
const PlaceholderTitle = "Untitled"

func insertWork(ctx context.Context, tx pgx.Tx, m ingest.Metadata) (uuid.UUID, error) {
	title := strings.TrimSpace(m.Title)
	if title == "" {
		// The parser refuses to invent a title and so does the store. A
		// placeholder is visibly a placeholder, and enrichment can replace it;
		// a filename guess recorded as fact would outrank the truth forever.
		title = PlaceholderTitle
	}

	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO works (title, subtitle, sort_title, series, series_index, language, description)
		VALUES ($1, NULLIF($2, ''), $3, NULLIF($4, ''), $5, NULLIF($6, ''), NULLIF($7, ''))
		RETURNING id`,
		title, m.Subtitle, SortTitle(title), m.Series, m.SeriesIndex, m.Language, m.Description,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("inserting work: %w", mapError(err))
	}
	return id, nil
}

func insertEdition(ctx context.Context, tx pgx.Tx, workID uuid.UUID, m ingest.Metadata) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO editions (work_id, isbn13, isbn10, publisher, language)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''))
		RETURNING id`,
		workID, m.ISBN13, m.ISBN10, m.Publisher, m.Language,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("inserting edition: %w", mapError(err))
	}
	return id, nil
}

// linkAuthors attaches contributors to a work, reusing existing author rows.
//
// Authors are matched case-insensitively on name, which is what the unique
// index enforces. Anything cleverer - initials, transliteration, "Le Guin" vs
// "LeGuin" - is normalisation work for enrichment, not something to guess at
// during ingest.
func linkAuthors(ctx context.Context, tx pgx.Tx, workID uuid.UUID, authors []ingest.Contributor) error {
	for position, a := range authors {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			continue
		}
		sortName := strings.TrimSpace(a.SortName)
		if sortName == "" {
			sortName = SortName(name)
		}

		var authorID uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO authors (name, sort_name)
			VALUES ($1, $2)
			ON CONFLICT (lower(name)) DO UPDATE
			  SET sort_name = CASE
			        WHEN authors.sort_name = '' THEN EXCLUDED.sort_name
			        ELSE authors.sort_name
			      END
			RETURNING id`, name, sortName,
		).Scan(&authorID)
		if err != nil {
			return fmt.Errorf("upserting author %q: %w", name, mapError(err))
		}

		role := a.Role
		if role == "" {
			role = domain.RoleAuthor
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO work_authors (work_id, author_id, role, position)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (work_id, author_id, role) DO NOTHING`,
			workID, authorID, string(role), position,
		)
		if err != nil {
			return fmt.Errorf("linking author %q: %w", name, mapError(err))
		}
	}
	return nil
}

// linkSubjects attaches the file's declared subjects as tags.
//
// Tags are deduplicated on a normalised slug. Without that a 5000-book library
// grows 5000 near-duplicates: "ml", "ML", "machine-learning", "Machine Learning".
func linkSubjects(ctx context.Context, tx pgx.Tx, workID uuid.UUID, subjects []string) error {
	seen := make(map[string]struct{}, len(subjects))
	for _, raw := range subjects {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		slug := TagSlug(name)
		if slug == "" {
			continue
		}
		if _, dup := seen[slug]; dup {
			continue
		}
		seen[slug] = struct{}{}

		var tagID uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO tags (name, slug)
			VALUES ($1, $2)
			ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug
			RETURNING id`, name, slug,
		).Scan(&tagID)
		if err != nil {
			return fmt.Errorf("upserting tag %q: %w", name, mapError(err))
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO work_tags (work_id, tag_id, source)
			VALUES ($1, $2, 'embedded')
			ON CONFLICT (work_id, tag_id) DO NOTHING`, workID, tagID)
		if err != nil {
			return fmt.Errorf("linking tag %q: %w", name, mapError(err))
		}
	}
	return nil
}

// recordProvenance writes one accepted record per field the file itself
// declared, so the UI can always answer "where did this come from?" - and so
// enrichment can tell which fields are publisher facts it must not overwrite.
func recordProvenance(ctx context.Context, tx pgx.Tx, workID uuid.UUID, m ingest.Metadata) error {
	if len(m.Provenance) == 0 {
		return nil
	}

	values := map[string]any{
		"title":       m.Title,
		"subtitle":    m.Subtitle,
		"language":    m.Language,
		"publisher":   m.Publisher,
		"publishedOn": m.PublishedOn,
		"isbn13":      m.ISBN13,
		"isbn10":      m.ISBN10,
		"description": m.Description,
		"series":      m.Series,
		"subjects":    m.Subjects,
		"authors":     contributorNames(m.Authors),
	}

	// Deterministic order so a re-ingest produces identical rows.
	for _, field := range provenanceFields {
		source, ok := m.Provenance[field]
		if !ok {
			continue
		}
		value, ok := values[field]
		if !ok {
			continue
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO enrichment_records (work_id, field, value, source, accepted_at)
			VALUES ($1, $2, $3, $4, now())`,
			workID, field, jsonValue(value), string(source),
		)
		if err != nil {
			return fmt.Errorf("recording provenance for %q: %w", field, mapError(err))
		}
	}
	return nil
}

// provenanceFields fixes the iteration order of provenance writes.
var provenanceFields = []string{
	"title", "subtitle", "authors", "language", "publisher",
	"publishedOn", "isbn13", "isbn10", "description", "series", "subjects",
}

// MarkMissing flags every book file not seen since the given time.
//
// Files are marked, never deleted. An unmounted drive must not destroy reading
// positions and annotations, and removal stays an explicit user action.
func (p *Pool) MarkMissing(ctx context.Context, before time.Time) (int64, error) {
	tag, err := p.Exec(ctx, `
		UPDATE book_files
		SET missing_since = now()
		WHERE last_seen_at < $1 AND missing_since IS NULL
		  -- Scanned files only. An UPLOADED book lives in the data directory,
		  -- which the scanner is forbidden from walking - the configuration
		  -- refuses to let the upload directory sit inside the library - so it
		  -- is never seen by a scan and its last_seen_at never moves.
		  --
		  -- Without this line every scan declared every uploaded book missing.
		  -- They vanish from the shelf, from Continue Reading, from search,
		  -- from retrieval and from enrichment, and nothing ever clears the
		  -- flag again, because only a scan or a re-ingest clears it and
		  -- neither will ever touch the file.
		  AND origin = 'scanned'`, before)
	if err != nil {
		return 0, fmt.Errorf("marking missing files: %w", mapError(err))
	}
	return tag.RowsAffected(), nil
}

// TouchSeen records that a file is still present, at the given path, without
// re-parsing it, and that whoever scanned it holds it.
//
// The path matters: a file that has been moved or renamed is recognised by its
// hash and skipped as unchanged, so this is the only place its new location is
// recorded. Without it the database would keep pointing at a path that no
// longer exists and opening the book would fail. A file Lexarii stores itself
// keeps its own path: the same bytes also sitting in the library directory do
// not make the upload a scanned file.
//
// The scanning administrator becomes a holder of the file (addHolder's rule
// for scans), whoever else holds it: their library directory is theirs. Not of
// a file they took off their shelf - a release is remembered until they add it
// again deliberately - and not of an archived file, which is claimed on
// purpose or not at all.
func (p *Pool) TouchSeen(ctx context.Context, contentHash, path string, holder uuid.UUID) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("touching %s: %w", contentHash, mapError(err))
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	if err := lockContentHash(ctx, tx, contentHash); err != nil {
		return err
	}
	// A removed file stays missing and unheld, wherever its bytes still sit.
	if err := refuseBlocked(ctx, tx, contentHash); err != nil {
		return err
	}

	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		UPDATE book_files
		SET last_seen_at = now(), missing_since = NULL,
		    path = CASE WHEN origin = 'scanned' THEN $2 ELSE path END
		WHERE content_hash = $1
		RETURNING id`, contentHash, path).Scan(&id)
	if err != nil {
		return fmt.Errorf("touching %s: %w", contentHash, mapError(err))
	}
	if _, err := addHolder(ctx, tx, id, holder, "", holdByScan); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("touching %s: %w", contentHash, mapError(err))
	}
	return nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// originOf reports where a file lives, which decides whether Lexarii may ever
// delete it: uploaded files are in its own managed directory, scanned files are
// in the user's read-only library.
func originOf(in IngestInput) string {
	if in.Uploaded {
		return "uploaded"
	}
	return "scanned"
}

func metadataOf(in IngestInput) ingest.Metadata {
	if in.Result == nil {
		return ingest.Metadata{}
	}
	return in.Result.Metadata
}

func unitCountOf(res *ingest.Result) *int {
	if res == nil || len(res.Spine) == 0 {
		return nil
	}
	n := len(res.Spine)
	return &n
}

func warningsOf(in IngestInput) []string {
	warnings := in.Warnings
	if in.Result != nil {
		warnings = append(append([]string{}, warnings...), in.Result.Warnings...)
	}
	if warnings == nil {
		// A NOT NULL text[] column needs an empty array, not NULL.
		return []string{}
	}
	return warnings
}

func contributorNames(authors []ingest.Contributor) []string {
	out := make([]string, 0, len(authors))
	for _, a := range authors {
		if name := strings.TrimSpace(a.Name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// BookFileStatus reports the ingest status of a file by content hash.
//
// The scan handler uses this to skip files that are already ingested and
// unchanged, which is what makes a rescan of a large library nearly free.
func (p *Pool) BookFileStatus(ctx context.Context, contentHash string) (domain.IngestStatus, error) {
	var status domain.IngestStatus
	err := p.QueryRow(ctx,
		`SELECT status FROM book_files WHERE content_hash = $1`, contentHash,
	).Scan(&status)
	if err != nil {
		return "", mapError(err)
	}
	return status, nil
}

// BookFilePath reports where the file with this content hash is recorded as
// living now, or ErrNotFound when no file has it.
func (p *Pool) BookFilePath(ctx context.Context, contentHash string) (string, error) {
	var path string
	if err := p.QueryRow(ctx, `SELECT path FROM book_files WHERE content_hash = $1`,
		contentHash).Scan(&path); err != nil {
		return "", mapError(err)
	}
	return path, nil
}

// nullableUUID keeps a zero id out of a column where it would be a foreign key
// violation rather than a value.
func nullableUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}
