// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// LibraryItem is one row of the library grid: a Work with just enough joined
// detail to render a card, and nothing more.
type LibraryItem struct {
	WorkID      uuid.UUID
	Title       string
	Subtitle    string
	SortTitle   string
	Series      string
	SeriesIndex *float64
	Authors     []string
	CoverPath   string
	Formats     []domain.Format
	// Status is the best status among the work's files: a work with one readable
	// file and one broken one is readable.
	Status     domain.IngestStatus
	PageCount  *int
	Missing    bool
	AddedAt    time.Time
	TagNames   []string
	BookFileID uuid.UUID
	// Progress is how far this viewer has read, 0 to 1, or nil if they have
	// not opened it. The FURTHEST point rather than the current one: a card
	// should say how much of the book has been read, not where the reader
	// happens to be after flicking back to check something.
	Progress *float32
	// LentBy names whoever lent this book to the viewer, when the viewer does
	// not hold it themselves.
	//
	// Empty for your own books, which is nearly all of them: a shelf that
	// labelled every card with its owner would be noise. Borrowed books are the
	// exception and need saying, because what you may do with one is different
	// - it can be withdrawn, and it may not be downloadable.
	LentBy string
	// CanDownload is false for a book shared to be READ rather than kept.
	CanDownload bool
}

// LibraryQuery filters and pages the library.
type LibraryQuery struct {
	// ViewerID is whose shelf this is, and it is REQUIRED.
	//
	// Books belong to people: a listing without a viewer would show every book
	// on the installation to whoever asked. A zero viewer is refused rather
	// than treated as "everything", because the failure must be an error and
	// never an accident.
	ViewerID uuid.UUID

	// Search matches title and author. Empty matches everything.
	Search string
	// Tag filters by tag slug.
	Tag string
	// CollectionID filters to one of the viewer's own collections.
	CollectionID *uuid.UUID
	// AuthorID filters to one author.
	AuthorID *uuid.UUID
	// Status filters by ingest status.
	Status domain.IngestStatus
	// IncludeMissing includes files whose drive is unmounted or which were
	// deleted. Off by default: the shelf should show what is actually readable.
	IncludeMissing bool
	// Owned narrows the shelf by who owns the books on it.
	//
	// The default is everything the viewer may read, because that is what a
	// shelf is. The other two exist because borrowed books answer a different
	// question - "what did somebody lend me, and is it still there" - and
	// because somebody about to share a book wants to see their own.
	Owned Ownership

	// ByTitleMatch orders a Search by how well each title matches it, best
	// first: the title IS the search (TitleKey on both sides), then it
	// contains the search as whole words, then everything else Search
	// matched; Sort only breaks ties. For resolving a name to a book, where
	// "It" must not be cut from a page of twenty titles that merely contain
	// the letters. Not a sort somebody can ask for over HTTP, and a single
	// page: a Cursor is refused.
	ByTitleMatch bool

	Sort   LibrarySort
	Limit  int
	Cursor string
}

// titleKeySQL is TitleKey in SQL, for the title of the work aliased w. The two
// must agree, or a title Go grades as exact sorts after titles it does not.
const titleKeySQL = `regexp_replace(btrim(regexp_replace(lower(w.title), '[^[:alnum:]'']+', ' ', 'g')),
	'^(the|a|an) ', '')`

// TitleKey is a title reduced to what identifies it: lower case, words of
// letters, digits and apostrophes, single spaces, without a leading article.
// "The Adventures of Sherlock Holmes!" and "adventures of sherlock holmes"
// have the same key.
func TitleKey(title string) string {
	words := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
	if len(words) > 1 && (words[0] == "the" || words[0] == "a" || words[0] == "an") {
		words = words[1:]
	}
	return strings.Join(words, " ")
}

// likeEscaper makes text match itself in LIKE and ILIKE, whose default escape
// character is the backslash.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Ownership selects whose books a listing covers.
type Ownership string

const (
	// OwnedAny is every book the viewer may read, theirs and borrowed.
	OwnedAny Ownership = ""
	// OwnedMine is the books the viewer owns.
	OwnedMine Ownership = "mine"
	// OwnedShared is the books other people have shared with the viewer.
	OwnedShared Ownership = "shared"
)

// LibrarySort names an ordering.
type LibrarySort string

const (
	SortTitleAsc   LibrarySort = "title"
	SortAddedDesc  LibrarySort = "added"
	SortAuthorAsc  LibrarySort = "author"
	SortSeriesAsc  LibrarySort = "series"
	defaultPageLen             = 48
	maxPageLen                 = 200
)

// LibraryPage is one page of results.
type LibraryPage struct {
	Items      []LibraryItem
	NextCursor string
	// No Total. There was one, and nothing ever wrote it or read it - so a
	// reader seeing it on a paginated result assumed a count query existed
	// somewhere, and went looking for it. Cursor pagination deliberately has
	// no total: counting the whole shelf to show a page of it is the cost the
	// cursor exists to avoid.
}

// Library returns a page of the library.
//
// Pagination is cursor-based. Offset pagination silently skips or repeats rows
// when a scan inserts books while the user is scrolling, which is exactly what
// happens on first run.
func (p *Pool) Library(ctx context.Context, q LibraryQuery) (LibraryPage, error) {
	if q.ViewerID == uuid.Nil {
		return LibraryPage{}, fmt.Errorf("%w: a library listing needs a viewer", store.ErrInvalidQuery)
	}

	limit := q.Limit
	if limit <= 0 {
		limit = defaultPageLen
	}
	if limit > maxPageLen {
		limit = maxPageLen
	}

	where := []string{"TRUE"}
	// The viewer is argument one, appended before anything else so its
	// placeholder number is fixed no matter which filters are used.
	args := []any{q.ViewerID}
	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if s := strings.TrimSpace(q.Search); s != "" {
		// Title, series, and author, plus a normalised-filename fallback.
		//
		// The fallback matters more than it looks: a book with no embedded title
		// has nothing to match on, and that is a large minority of a real
		// library. Comparing bare alphanumerics finds "linear algebra" inside
		// "appliedlinearalgebraandoptimizationusingmatlab.pdf", which is
		// otherwise completely unsearchable.
		// Escaped: a search is for the text typed, and "100%" or a name the
		// router wrote with an underscore is not a pattern.
		args = append(args, likeEscaper.Replace(s), SearchKey(s))
		term, key := len(args)-1, len(args)
		where = append(where, fmt.Sprintf(`(w.title ILIKE '%%' || $%[1]d || '%%'
		      OR w.series ILIKE '%%' || $%[1]d || '%%'
		      OR EXISTS (SELECT 1 FROM work_authors wa2
		                 JOIN authors a2 ON a2.id = wa2.author_id
		                 WHERE wa2.work_id = w.id AND a2.name ILIKE '%%' || $%[1]d || '%%')
		      OR ($%[2]d <> '' AND EXISTS (
		            SELECT 1 FROM book_files bf3 JOIN editions e3 ON e3.id = bf3.edition_id
		            WHERE e3.work_id = w.id AND bf3.search_key LIKE '%%' || $%[2]d || '%%')))`,
			term, key))
	}
	if t := strings.TrimSpace(q.Tag); t != "" {
		add(`EXISTS (SELECT 1 FROM work_tags wt JOIN tags tg ON tg.id = wt.tag_id
		             WHERE wt.work_id = w.id AND tg.slug = $%d)`, t)
	}
	if q.CollectionID != nil {
		// The collection has to be the VIEWER's. Without that join this would
		// filter by anybody's collection, which is a way of asking whether a
		// particular book is in a stranger's "to read".
		args = append(args, *q.CollectionID, q.ViewerID)
		where = append(where, fmt.Sprintf(
			`EXISTS (SELECT 1 FROM collection_works cw
			         JOIN collections c ON c.id = cw.collection_id
			         WHERE cw.work_id = w.id AND cw.collection_id = $%d AND c.user_id = $%d)`,
			len(args)-1, len(args)))
	}
	if q.AuthorID != nil {
		add(`EXISTS (SELECT 1 FROM work_authors wa3 WHERE wa3.work_id = w.id AND wa3.author_id = $%d)`, *q.AuthorID)
	}
	orderBy, cursorClause, cursorArgs, err := librarySortClauses(q.Sort, q.Cursor, len(args))
	if err != nil {
		return LibraryPage{}, err
	}
	byMatch := q.ByTitleMatch && strings.TrimSpace(q.Search) != ""
	if byMatch {
		if q.Cursor != "" {
			return LibraryPage{}, fmt.Errorf("%w: a listing by title match is one page", store.ErrInvalidQuery)
		}
		// TitleKey keeps only letters, digits and apostrophes, so the key
		// has nothing LIKE would read as a pattern.
		args = append(args, TitleKey(q.Search))
		orderBy = fmt.Sprintf(`CASE WHEN %[1]s = $%[2]d THEN 0
		                           WHEN ' ' || %[1]s || ' ' LIKE '%% ' || $%[2]d || ' %%' THEN 1
		                           ELSE 2 END, %[3]s`, titleKeySQL, len(args), orderBy)
	}
	if cursorClause != "" {
		where = append(where, cursorClause)
		args = append(args, cursorArgs...)
	}

	// One row per work, with a single representative file chosen by a LATERAL
	// join. A work with an EPUB and a PDF is one card, not two.
	//
	// DISTINCT ON would also collapse the rows, but it requires the ORDER BY to
	// begin with the same bare expressions, which rules out the DESC and NULLS
	// LAST modifiers the sort options need.
	fileWhere := `e.work_id = w.id
		      AND EXISTS (SELECT 1 FROM readable_book_files rbf
		                   WHERE rbf.book_file_id = bf.id AND rbf.user_id = $1)`
	if !q.IncludeMissing {
		fileWhere += " AND bf.missing_since IS NULL"
	}
	if q.Status != "" {
		args = append(args, q.Status)
		fileWhere += fmt.Sprintf(" AND bf.status = $%d", len(args))
	}
	// Inside the LATERAL join rather than the outer WHERE. The join picks ONE
	// file to represent the work, so an ownership test applied outside it would
	// choose a file first and then discard the whole work when that file
	// happened to be the wrong one - hiding a book the viewer does own because
	// a borrowed copy of it sorted first.
	switch q.Owned {
	case OwnedMine:
		fileWhere += ` AND EXISTS (SELECT 1 FROM book_file_holders h
		                           WHERE h.book_file_id = bf.id AND h.user_id = $1)`
	case OwnedShared:
		fileWhere += ` AND NOT EXISTS (SELECT 1 FROM book_file_holders h
		                               WHERE h.book_file_id = bf.id AND h.user_id = $1)`
	}

	query := fmt.Sprintf(`
		SELECT w.id, w.title, COALESCE(w.subtitle, ''), w.sort_title,
		       COALESCE(w.series, ''), w.series_index,
		       COALESCE(f.cover_path, ''),
		       f.id, f.status, f.unit_count, f.missing, f.created_at,
		       (SELECT rp.furthest_percent FROM reading_progress rp
		         WHERE rp.book_file_id = f.id AND rp.user_id = $1),
		       -- Named only when the viewer does not hold it, and then as
		       -- whoever lent it to THEM. A shelf of your own books does not
		       -- need every card to say so, and a file somebody else also
		       -- holds is not evidence that they lent it to anybody.
		       COALESCE((SELECT u.username FROM book_access ba
		                  JOIN users u ON u.id = ba.granted_by
		                  WHERE ba.book_file_id = f.id AND ba.user_id = $1
		                    AND NOT f.held
		                  ORDER BY ba.created_at, lower(u.username)
		                  LIMIT 1), '') AS lent_by,
		       COALESCE((SELECT bool_or(rbf.can_download) FROM readable_book_files rbf
		                  WHERE rbf.book_file_id = f.id AND rbf.user_id = $1), false),
		       COALESCE((SELECT array_agg(a.name ORDER BY wa.position)
		                 FROM work_authors wa JOIN authors a ON a.id = wa.author_id
		                 WHERE wa.work_id = w.id), '{}') AS authors,
		       -- Only the formats this viewer may actually open.
		       --
		       -- Unfiltered, a card for a book shared as an EPUB advertised
		       -- "EPUB MOBI PDF", telling the borrower about copies they have
		       -- no access to. A listing must not describe files outside the
		       -- viewer's reach, even in passing.
		       COALESCE((SELECT array_agg(DISTINCT bf2.format::text)
		                 FROM book_files bf2 JOIN editions e2 ON e2.id = bf2.edition_id
		                 JOIN readable_book_files rbf2
		                   ON rbf2.book_file_id = bf2.id AND rbf2.user_id = $1
		                 WHERE e2.work_id = w.id), '{}') AS formats,
		       COALESCE((SELECT array_agg(tg.name ORDER BY tg.name)
		                 FROM work_tags wt JOIN tags tg ON tg.id = wt.tag_id
		                 WHERE wt.work_id = w.id), '{}') AS tags
		FROM works w
		JOIN LATERAL (
		    SELECT bf.id, bf.status, bf.unit_count, bf.created_at, bf.format,
		           EXISTS (SELECT 1 FROM book_file_holders h
		                    WHERE h.book_file_id = bf.id AND h.user_id = $1) AS held,
		           bf.missing_since IS NOT NULL AS missing,
		           -- The file's own cover first: it is rendered from this
		           -- file's first page. The edition's is the older column,
		           -- kept as a fallback for anything that sets it.
		           COALESCE(bf.cover_path, e.cover_path, '') AS cover_path
		    FROM book_files bf JOIN editions e ON e.id = bf.edition_id
		    WHERE %[1]s
		    -- The card must represent the book at its BEST, not its oldest. A
		    -- book held as a readable EPUB and an unsupported MOBI is a readable
		    -- book, and picking by age would show it as unreadable with no page
		    -- count purely because the MOBI was ingested first.
		    ORDER BY CASE bf.status
		                 WHEN 'ready'       THEN 0
		                 WHEN 'needs_ocr'   THEN 1
		                 WHEN 'extracting'  THEN 2
		                 WHEN 'pending'     THEN 3
		                 WHEN 'unsupported' THEN 4
		                 ELSE 5
		             END,
		             bf.created_at
		    LIMIT 1
		) f ON TRUE
		WHERE %[2]s
		ORDER BY %[3]s
		LIMIT %[4]d`,
		fileWhere, strings.Join(where, " AND "), orderBy, limit+1)

	rows, err := p.Query(ctx, query, args...)
	if err != nil {
		return LibraryPage{}, fmt.Errorf("querying library: %w", mapError(err))
	}
	defer rows.Close()

	var page LibraryPage
	for rows.Next() {
		var it LibraryItem
		var formats []string
		if err := rows.Scan(
			&it.WorkID, &it.Title, &it.Subtitle, &it.SortTitle,
			&it.Series, &it.SeriesIndex, &it.CoverPath,
			&it.BookFileID, &it.Status, &it.PageCount, &it.Missing,
			&it.AddedAt, &it.Progress, &it.LentBy, &it.CanDownload,
			&it.Authors, &formats, &it.TagNames,
		); err != nil {
			return LibraryPage{}, fmt.Errorf("scanning library row: %w", err)
		}
		for _, f := range formats {
			it.Formats = append(it.Formats, domain.Format(f))
		}
		page.Items = append(page.Items, it)
	}
	if err := rows.Err(); err != nil {
		return LibraryPage{}, fmt.Errorf("reading library rows: %w", mapError(err))
	}

	// One extra row was fetched to detect a further page without a second query.
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[len(page.Items)-1]
		if !byMatch {
			page.NextCursor = encodeCursor(q.Sort, last)
		}
	}

	return page, nil
}

// librarySortClauses builds the ORDER BY and the keyset predicate for a cursor.
//
// Every ordering ends in w.id so it is total: without a unique tiebreaker two
// works with the same title would page inconsistently and could repeat or skip.
func librarySortClauses(sort LibrarySort, cursor string, argOffset int) (orderBy, clause string, args []any, err error) {
	switch sort {
	case "", SortTitleAsc:
		orderBy = "w.sort_title, w.id"
	case SortAddedDesc:
		orderBy = "f.created_at DESC, w.id"
	// The ORDER BY and the keyset below must be the SAME expression, or paging
	// skips and repeats books. They were not: the ordering put NULLs last while
	// the cursor coalesced them to the empty string, which sorts first, and the
	// series ordering included series_index while the cursor dropped it
	// entirely. A shelf sorted by series or by author lost books between pages.
	case SortAuthorAsc:
		orderBy = authorSortKey + ", w.id"
	case SortSeriesAsc:
		orderBy = seriesSortKey + ", w.id"
	default:
		return "", "", nil, fmt.Errorf("%w: unknown sort %q", store.ErrInvalidQuery, sort)
	}

	if cursor == "" {
		return orderBy, "", nil, nil
	}

	key, id, err := decodeCursor(cursor)
	if err != nil {
		return "", "", nil, err
	}

	switch sort {
	case "", SortTitleAsc:
		clause = fmt.Sprintf("(w.sort_title, w.id) > ($%d, $%d)", argOffset+1, argOffset+2)
		args = []any{key, id}
	case SortAddedDesc:
		ts, terr := time.Parse(time.RFC3339Nano, key)
		if terr != nil {
			return "", "", nil, fmt.Errorf("%w: cursor timestamp: %v", store.ErrInvalidQuery, terr)
		}
		clause = fmt.Sprintf("(f.created_at, w.id) < ($%d, $%d)", argOffset+1, argOffset+2)
		args = []any{ts, id}
	case SortAuthorAsc:
		clause = fmt.Sprintf("(%s, w.id) > ($%d, $%d)", authorSortKey, argOffset+1, argOffset+2)
		args = []any{key, id}
	case SortSeriesAsc:
		clause = fmt.Sprintf("(%s, w.id) > ($%d, $%d)", seriesSortKey, argOffset+1, argOffset+2)
		args = []any{key, id}
	}
	return orderBy, clause, args, nil
}

// encodeCursor packs the sort key and id of the last row on a page.
func encodeCursor(sort LibrarySort, last LibraryItem) string {
	var key string
	switch sort {
	case "", SortTitleAsc:
		key = last.SortTitle
	case SortAddedDesc:
		key = last.AddedAt.Format(time.RFC3339Nano)
	case SortAuthorAsc:
		if len(last.Authors) > 0 {
			key = SortName(last.Authors[0])
		}
	case SortSeriesAsc:
		key = seriesKeyOf(last.Series, last.SeriesIndex)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(key + "\x00" + last.WorkID.String()))
}

func decodeCursor(cursor string) (key string, id uuid.UUID, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("%w: cursor is not base64: %v", store.ErrInvalidQuery, err)
	}
	key, idStr, ok := strings.Cut(string(raw), "\x00")
	if !ok {
		return "", uuid.Nil, fmt.Errorf("%w: cursor has no id", store.ErrInvalidQuery)
	}
	id, err = uuid.Parse(idStr)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("%w: cursor id is not a UUID: %v", store.ErrInvalidQuery, err)
	}
	return key, id, nil
}

// LibraryStats summarises the whole library for the shelf header.
type LibraryStats struct {
	Works     int
	Files     int
	Authors   int
	Tags      int
	Pages     int
	Missing   int
	NeedsOCR  int
	Failed    int
	ByFormat  map[domain.Format]int
	QueueBusy int
}

// Stats counts what this person can see.
//
// Counting the whole installation would be a quiet leak: "168 books" on an
// account that has been granted two says plainly that there are 166 more, and
// the author and tag totals describe books the person cannot open.
func (p *Pool) Stats(ctx context.Context, viewer uuid.UUID) (LibraryStats, error) {
	s := LibraryStats{ByFormat: make(map[domain.Format]int)}
	if viewer == uuid.Nil {
		return s, fmt.Errorf("%w: stats need a viewer", store.ErrInvalidQuery)
	}

	// One CTE of visible files, so every count below is over the same set and
	// none of them can drift out of agreement with the others.
	err := p.QueryRow(ctx, `
		WITH mine AS (
			SELECT bf.*, e.work_id
			FROM book_files bf
			JOIN editions e ON e.id = bf.edition_id
			JOIN readable_book_files rbf
			  ON rbf.book_file_id = bf.id AND rbf.user_id = $1
		)
		SELECT (SELECT count(DISTINCT work_id) FROM mine),
		       (SELECT count(*) FROM mine),
		       (SELECT count(DISTINCT wa.author_id) FROM work_authors wa
		         WHERE wa.work_id IN (SELECT work_id FROM mine)),
		       (SELECT count(DISTINCT wt.tag_id) FROM work_tags wt
		         WHERE wt.work_id IN (SELECT work_id FROM mine)),
		       (SELECT COALESCE(sum(unit_count), 0) FROM mine),
		       (SELECT count(*) FROM mine WHERE missing_since IS NOT NULL),
		       (SELECT count(*) FROM mine WHERE status = 'needs_ocr'),
		       (SELECT count(*) FROM mine WHERE status = 'failed'),
		       (SELECT count(*) FROM jobs WHERE status IN ('pending','running'))`, viewer,
	).Scan(&s.Works, &s.Files, &s.Authors, &s.Tags, &s.Pages, &s.Missing, &s.NeedsOCR, &s.Failed, &s.QueueBusy)
	if err != nil {
		return s, fmt.Errorf("reading stats: %w", mapError(err))
	}

	rows, err := p.Query(ctx, `
		SELECT bf.format, count(*)
		FROM book_files bf
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = $1
		GROUP BY bf.format`, viewer)
	if err != nil {
		return s, fmt.Errorf("reading format counts: %w", mapError(err))
	}
	defer rows.Close()
	for rows.Next() {
		var f domain.Format
		var n int
		if err := rows.Scan(&f, &n); err != nil {
			return s, fmt.Errorf("scanning format count: %w", err)
		}
		s.ByFormat[f] = n
	}
	return s, rows.Err()
}

// WorkDetail is everything the book detail page shows.
type WorkDetail struct {
	Work  domain.Work
	Files []domain.BookFile
	// Downloadable runs alongside Files: whether this viewer may have that
	// file itself, rather than only read it here. Beside the slice rather than
	// on domain.BookFile because it is a fact about a PERSON and a file, not
	// about the file.
	Downloadable []bool
	// Filenames runs alongside Files too: the name THIS viewer gave each file,
	// empty for a file they do not hold or added without one. Per person
	// because two people can hold one file under different names.
	Filenames []string
	// Held runs alongside Files: whether this viewer holds each file, which
	// is what lets them take it off their shelf. A lent file is not theirs to
	// remove.
	Held     []bool
	Tags     []domain.Tag
	Warnings []string
	// Provenance maps each metadata field to where its value came from, so the
	// UI can mark what the model guessed and let the user reject it.
	Provenance map[string]domain.MetadataSource
}

// Work reads one work with its editions, files, authors, and provenance.
func (p *Pool) Work(ctx context.Context, id, viewer uuid.UUID) (WorkDetail, error) {
	if viewer == uuid.Nil {
		return WorkDetail{}, errNotFound
	}

	var d WorkDetail
	d.Provenance = make(map[string]domain.MetadataSource)

	// A work is visible when at least one of its files is. Someone who was
	// granted the EPUB sees the book; someone granted nothing gets the same
	// answer as for a work that does not exist, which is the right answer to
	// give a person who should not learn that it does.
	var subtitle, series, language, description, summary *string
	err := p.QueryRow(ctx, `
		SELECT w.id, w.title, w.subtitle, w.sort_title, w.series, w.series_index,
		       w.language, w.description, w.summary, w.created_at, w.updated_at
		FROM works w
		WHERE w.id = $1
		  AND EXISTS (
		      SELECT 1 FROM book_files bf
		      JOIN editions e ON e.id = bf.edition_id
		      JOIN readable_book_files rbf
		        ON rbf.book_file_id = bf.id AND rbf.user_id = $2
		      WHERE e.work_id = w.id)`, id, viewer,
	).Scan(&d.Work.ID, &d.Work.Title, &subtitle, &d.Work.SortTitle, &series,
		&d.Work.SeriesIndex, &language, &description, &summary,
		&d.Work.CreatedAt, &d.Work.UpdatedAt)
	if err != nil {
		return d, mapError(err)
	}
	d.Work.Subtitle = deref(subtitle)
	d.Work.Series = deref(series)
	d.Work.Language = deref(language)
	d.Work.Description = deref(description)
	d.Work.Summary = deref(summary)

	// The editions, which carry the publisher, the year and the ISBNs.
	//
	// Written at ingest and, until citations existed, never read back by
	// anything - so an exported citation came out as a title and an author
	// with the rest of the record sitting in the database unused.
	editionRows, err := p.Query(ctx, `
		SELECT id, work_id, COALESCE(isbn13, ''), COALESCE(isbn10, ''),
		       COALESCE(publisher, ''), published_on, page_count,
		       COALESCE(language, ''), created_at, updated_at
		FROM editions WHERE work_id = $1 ORDER BY created_at`, id)
	if err != nil {
		return d, fmt.Errorf("reading editions: %w", mapError(err))
	}
	for editionRows.Next() {
		var e domain.Edition
		if err := editionRows.Scan(&e.ID, &e.WorkID, &e.ISBN13, &e.ISBN10,
			&e.Publisher, &e.PublishedOn, &e.PageCount, &e.Language,
			&e.CreatedAt, &e.UpdatedAt); err != nil {
			editionRows.Close()
			return d, fmt.Errorf("scanning edition: %w", err)
		}
		d.Work.Editions = append(d.Work.Editions, e)
	}
	editionRows.Close()
	if err := editionRows.Err(); err != nil {
		return d, fmt.Errorf("reading editions: %w", mapError(err))
	}

	authorRows, err := p.Query(ctx, `
		SELECT a.id, a.name, a.sort_name, wa.role, wa.position
		FROM work_authors wa JOIN authors a ON a.id = wa.author_id
		WHERE wa.work_id = $1 ORDER BY wa.position`, id)
	if err != nil {
		return d, fmt.Errorf("reading authors: %w", mapError(err))
	}
	for authorRows.Next() {
		var wa domain.WorkAuthor
		if err := authorRows.Scan(&wa.Author.ID, &wa.Author.Name, &wa.Author.SortName,
			&wa.Role, &wa.Position); err != nil {
			authorRows.Close()
			return d, fmt.Errorf("scanning author: %w", err)
		}
		d.Work.Authors = append(d.Work.Authors, wa)
	}
	authorRows.Close()
	if err := authorRows.Err(); err != nil {
		return d, fmt.Errorf("reading authors: %w", mapError(err))
	}

	fileRows, err := p.Query(ctx, `
		SELECT bf.id, bf.edition_id, bf.path, bf.content_hash, bf.format,
		       bf.size_bytes, bf.status, COALESCE(bf.ingest_error, ''),
		       bf.unit_count, bf.extracted_at, bf.ingest_warnings,
		       bf.missing_since IS NOT NULL, bf.created_at, bf.updated_at,
		       h.user_id IS NOT NULL AS owned,
		       -- The name THIS person gave the file. Somebody else holding the
		       -- same bytes under another name is not this person's concern.
		       COALESCE(h.original_filename, '') AS filename,
		       -- Whether this person may have the FILE, as opposed to being
		       -- allowed to read it here. A book can be lent to be read
		       -- without being handed over.
		       COALESCE(rbf.can_download, false) AS can_download
		FROM book_files bf
		JOIN editions e ON e.id = bf.edition_id
		-- Only the files this person may open.
		--
		-- Without this the page listed every file of the work to anybody who
		-- could see the work at all: somebody granted one EPUB was handed the
		-- id, size, hash and full filesystem PATH of four other copies,
		-- including the owner's home directory. The viewer was already a
		-- parameter of this function; this query simply never used it.
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = $2
		LEFT JOIN book_file_holders h
		  ON h.book_file_id = bf.id AND h.user_id = $2
		WHERE e.work_id = $1 ORDER BY bf.created_at`, id, viewer)
	if err != nil {
		return d, fmt.Errorf("reading files: %w", mapError(err))
	}
	for fileRows.Next() {
		var f domain.BookFile
		var warnings []string
		var missing bool
		var owned bool
		var filename string
		var canDownload bool
		if err := fileRows.Scan(&f.ID, &f.EditionID, &f.Path, &f.ContentHash, &f.Format,
			&f.SizeBytes, &f.Status, &f.IngestError, &f.UnitCount, &f.ExtractedAt,
			&warnings, &missing, &f.CreatedAt, &f.UpdatedAt, &owned, &filename, &canDownload); err != nil {
			fileRows.Close()
			return d, fmt.Errorf("scanning file: %w", err)
		}
		if !owned || filename != "" {
			// Where a book sits on disk is the business of whoever put it
			// there. It is shown so they can find it again; to a borrower it
			// is somebody else's directory layout, and their home directory
			// name, in exchange for being lent a book.
			//
			// The same goes for a holder who UPLOADED it: the file may be one
			// an administrator scanned out of their own directory first. They
			// are shown the name they gave it instead, which is also the only
			// useful name an upload has - its own path is a content hash.
			f.Path = ""
		}
		d.Files = append(d.Files, f)
		d.Downloadable = append(d.Downloadable, canDownload)
		d.Filenames = append(d.Filenames, filename)
		d.Held = append(d.Held, owned)
		d.Warnings = append(d.Warnings, warnings...)
	}
	fileRows.Close()
	if err := fileRows.Err(); err != nil {
		return d, fmt.Errorf("reading files: %w", mapError(err))
	}

	tagRows, err := p.Query(ctx, `
		SELECT tg.id, tg.name, tg.slug, tg.is_subject, wt.source, wt.confidence
		FROM work_tags wt JOIN tags tg ON tg.id = wt.tag_id
		WHERE wt.work_id = $1 ORDER BY tg.name`, id)
	if err != nil {
		return d, fmt.Errorf("reading tags: %w", mapError(err))
	}
	for tagRows.Next() {
		var t domain.Tag
		if err := tagRows.Scan(&t.ID, &t.Name, &t.Slug, &t.IsSubject, &t.Source, &t.Confidence); err != nil {
			tagRows.Close()
			return d, fmt.Errorf("scanning tag: %w", err)
		}
		d.Tags = append(d.Tags, t)
	}
	tagRows.Close()
	if err := tagRows.Err(); err != nil {
		return d, fmt.Errorf("reading tags: %w", mapError(err))
	}

	provRows, err := p.Query(ctx, `
		SELECT DISTINCT ON (field) field, source
		FROM enrichment_records
		WHERE work_id = $1 AND rejected_at IS NULL
		ORDER BY field, generated_at DESC`, id)
	if err != nil {
		return d, fmt.Errorf("reading provenance: %w", mapError(err))
	}
	defer provRows.Close()
	for provRows.Next() {
		var field string
		var source domain.MetadataSource
		if err := provRows.Scan(&field, &source); err != nil {
			return d, fmt.Errorf("scanning provenance: %w", err)
		}
		d.Provenance[field] = source
	}
	return d, provRows.Err()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ReadableBook is a file plus the little about its work that the reader needs:
// enough to name the book on screen and link back to it.
type ReadableBook struct {
	File   domain.BookFile
	WorkID uuid.UUID
	Title  string
}

// ReadableBookWithWork reads a file and its work's title, for somebody allowed
// to open it.
//
// The reader had no way to name the book it was showing - the chrome said "95
// of 104" and nothing else - because nothing on this path carried a title.
func (p *Pool) ReadableBookWithWork(ctx context.Context, id, viewer uuid.UUID) (ReadableBook, error) {
	if viewer == uuid.Nil {
		return ReadableBook{}, errNotFound
	}

	var out ReadableBook
	f := &out.File
	err := p.QueryRow(ctx, `
		SELECT bf.id, bf.edition_id, bf.path, bf.content_hash, bf.format,
		       bf.size_bytes, bf.status, COALESCE(bf.ingest_error, ''),
		       bf.unit_count, bf.extracted_at, bf.created_at, bf.updated_at,
		       w.id, w.title
		FROM book_files bf
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = $2
		JOIN editions e ON e.id = bf.edition_id
		JOIN works w ON w.id = e.work_id
		WHERE bf.id = $1`, id, viewer,
	).Scan(&f.ID, &f.EditionID, &f.Path, &f.ContentHash, &f.Format, &f.SizeBytes,
		&f.Status, &f.IngestError, &f.UnitCount, &f.ExtractedAt, &f.CreatedAt, &f.UpdatedAt,
		&out.WorkID, &out.Title)
	if err != nil {
		return ReadableBook{}, mapError(err)
	}
	return out, nil
}

// ReadableBookFile reads a file only if this person may open it.
//
// A file this person cannot open reports ErrNotFound rather than a refusal.
// "You may not see this" and "this does not exist" are the same answer to
// someone who should not know the difference, and distinguishing them turns
// the endpoint into a way of discovering what other people own.
func (p *Pool) ReadableBookFile(ctx context.Context, id, viewer uuid.UUID) (domain.BookFile, error) {
	if viewer == uuid.Nil {
		return domain.BookFile{}, errNotFound
	}

	var f domain.BookFile
	err := p.QueryRow(ctx, `
		SELECT bf.id, bf.edition_id, bf.path, bf.content_hash, bf.format,
		       bf.size_bytes, bf.status, COALESCE(bf.ingest_error, ''),
		       bf.unit_count, bf.extracted_at, bf.created_at, bf.updated_at
		FROM book_files bf
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = $2
		WHERE bf.id = $1`, id, viewer,
	).Scan(&f.ID, &f.EditionID, &f.Path, &f.ContentHash, &f.Format, &f.SizeBytes,
		&f.Status, &f.IngestError, &f.UnitCount, &f.ExtractedAt, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return domain.BookFile{}, mapError(err)
	}
	return f, nil
}

// CanDownload reports whether this person may take a copy of the file, as
// opposed to reading it in the browser.
//
// An owner always can. A grantee can only if the grant says so, which it does
// not by default.
func (p *Pool) CanDownload(ctx context.Context, id, viewer uuid.UUID) (bool, error) {
	if viewer == uuid.Nil {
		return false, nil
	}
	// bool_or over no rows is NULL, not false: somebody who cannot read the
	// file at all must get "no", not a scan error the handler turns into a
	// 500.
	var allowed bool
	err := p.QueryRow(ctx, `
		SELECT COALESCE(bool_or(can_download), false) FROM readable_book_files
		WHERE book_file_id = $1 AND user_id = $2`, id, viewer).Scan(&allowed)
	if err != nil {
		return false, mapError(err)
	}
	return allowed, nil
}

// BookFile reads one file by id, with NO viewer, and must keep none.
//
// Background workers chunk, embed and enrich every file on the installation,
// and they act as the installation rather than as anybody. Giving this a viewer
// would either grant some user's identity to a job queue or stop indexing books
// whose owner is not signed in. ReadableBookFile is the one to reach for from a
// handler.
func (p *Pool) BookFile(ctx context.Context, id uuid.UUID) (domain.BookFile, error) {
	var f domain.BookFile
	err := p.QueryRow(ctx, `
		SELECT id, edition_id, path, content_hash, format, size_bytes, status,
		       COALESCE(ingest_error, ''), unit_count, extracted_at, created_at, updated_at
		FROM book_files WHERE id = $1`, id,
	).Scan(&f.ID, &f.EditionID, &f.Path, &f.ContentHash, &f.Format, &f.SizeBytes,
		&f.Status, &f.IngestError, &f.UnitCount, &f.ExtractedAt, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return domain.BookFile{}, mapError(err)
	}
	return f, nil
}

// The sort keys, written once because the ORDER BY and the keyset comparison
// have to be the same expression down to the NULL handling. When they differ,
// paging a shelf skips books and shows others twice, silently.
//
// Books with no series or no author sort FIRST rather than last, which is the
// price of the two halves agreeing: COALESCE is the only form that can be
// written identically in a comparison and in an ordering.
const (
	authorSortKey = `COALESCE((SELECT a.sort_name FROM work_authors wa JOIN authors a ON a.id = wa.author_id
	                 WHERE wa.work_id = w.id ORDER BY wa.position LIMIT 1), '')`

	// The series number is part of the key, zero-padded so that it compares as
	// a number does: book 2 before book 10, which "2" > "10" as text does not.
	seriesSortKey = `(COALESCE(w.series, '') || E'\x1f' ||
	                  to_char(COALESCE(w.series_index, 0), 'FM000000000.00'))`
)

// seriesKeyOf reproduces seriesSortKey in Go, for the cursor.
//
// It has to match the SQL exactly, which is why the SQL uses to_char with an
// explicit format rather than a cast: COALESCE(series_index, 0) on a
// numeric(8,2) column renders the zero branch as "0" and loses the scale, so
// the two halves disagreed and paging never terminated. to_char pins the width
// and %012.2f is its equivalent here - both give "000000002.00".
func seriesKeyOf(series string, index *float64) string {
	value := 0.0
	if index != nil {
		value = *index
	}
	return fmt.Sprintf("%s\x1f%012.2f", series, value)
}
