// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// SearchQuery is one retrieval.
type SearchQuery struct {
	// Text is the question as asked, used for the keyword arm.
	Text string
	// Vector is the question's embedding, used for the meaning arm. Nil runs
	// keyword search alone, which is what happens with no embedding model.
	Vector []float32
	// ModelID scopes the vectors searched. Vectors from a different model are
	// not comparable and must never be mixed into one search.
	ModelID string

	// ViewerID is whose library is being searched, and it is REQUIRED.
	//
	// Books belong to people. A search without a viewer would return passages
	// from books the asker cannot open, which is the worst kind of leak: the
	// content arrives quoted inside an answer, with a citation, looking
	// authoritative. A zero viewer is refused rather than treated as "no
	// filter" - the failure has to be an error, never an accident.
	ViewerID uuid.UUID

	// WorkIDs narrows the search to particular books. Empty searches the whole
	// library. This is where routing takes effect: "a maths question, so look
	// at the maths books" is expressed here rather than in the ranking.
	WorkIDs []uuid.UUID
	// BookFileID narrows to a single file, for a conversation pinned to a book.
	BookFileID *uuid.UUID

	// Limit is how many passages to return. Zero uses DefaultSearchLimit.
	Limit int
	// Candidates is how deep each arm searches before fusion. Zero uses
	// DefaultCandidates.
	Candidates int
	// RRFK damps the influence of the top ranks. Zero uses domain.DefaultRRFK.
	RRFK float64
}

// Search sizing.
//
// Each arm returns Candidates rows and fusion picks Limit from their union, so
// Candidates must be comfortably larger than Limit or fusion has nothing to
// choose between: a passage ranked 30th by meaning and 2nd by keyword is
// exactly the kind of result hybrid search exists to surface, and it is only
// visible if both arms were asked for at least 30.
const (
	DefaultSearchLimit = 12
	DefaultCandidates  = 60

	// efSearchHeadroom multiplies the candidate count to get the HNSW search
	// depth. Two is the smallest that reliably returns the full candidate list
	// once a filter has thrown some of it away.
	efSearchHeadroom = 2
	// maxEFSearch bounds that depth. Deeper is slower for diminishing recall,
	// and this runs on a home server.
	maxEFSearch = 400

	// fallbackPool is how many candidates' worth of passages with only some
	// of the question's words are ranked by cover density. See Search.
	fallbackPool = 4
	// fallbackWords is how many of the question's words the fallback
	// searches for, rarest first.
	fallbackWords = 16
)

// efSearchFor is how deep the HNSW scan goes for a given candidate count.
func efSearchFor(candidates int) int {
	depth := candidates * efSearchHeadroom
	if depth > maxEFSearch {
		depth = maxEFSearch
	}
	// Below the index's own default the parameter would make recall worse
	// than not setting it at all.
	if depth < 40 {
		depth = 40
	}
	return depth
}

// Search finds passages by meaning and by keyword, and fuses the two.
//
// Both arms run in one statement. Fusing in the database rather than in Go is
// not an optimisation: each arm's rank depends on the whole candidate set, so
// splitting the query would mean transferring both sets in full to compute
// something Postgres can compute while it already has them.
//
// With no vector the keyword arm is the whole search, and it falls back to
// any of the question's words when requiring every word finds too little; see
// the any_words step. That fallback is the one part of the statement whose
// cost depends on the question, so the statement runs under
// searchStatementTimeout, and a search that hits it is run again without the
// fallback rather than failing the question.
func (p *Pool) Search(ctx context.Context, q SearchQuery) ([]domain.RetrievedChunk, error) {
	// With a vector the meaning arm finds what has only some of the words, and
	// the fallback's passages would compete with it at full fusion weight: a
	// passage sharing one common word with the question outranked the best
	// passage by meaning. So hybrid search is exactly what it was.
	fallback := len(q.Vector) == 0
	out, err := p.search(ctx, q, fallback)
	if fallback && isStatementTimeout(err) && ctx.Err() == nil {
		slog.Warn("keyword search timed out with the any-word fallback; searching without it",
			"timeout", searchStatementTimeout, "error", err)
		return p.search(ctx, q, false)
	}
	return out, err
}

// searchStatementTimeout bounds one search statement. A backstop, not a
// budget: the slowest question measured takes well under a second.
var searchStatementTimeout = 5 * time.Second

// isStatementTimeout reports Postgres cancelling a statement for running past
// statement_timeout.
func isStatementTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == codeQueryCanceled
}

// slowFallbackForTest makes the fallback take a second, so a test can watch the
// timeout and the retry without a library big enough to be slow.
var slowFallbackForTest = false

func (p *Pool) search(ctx context.Context, q SearchQuery, fallback bool) ([]domain.RetrievedChunk, error) {
	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = DefaultSearchLimit
	}
	candidates := q.Candidates
	if candidates <= 0 || candidates > 1000 {
		candidates = DefaultCandidates
	}
	if candidates < limit {
		candidates = limit
	}
	k := q.RRFK
	if k <= 0 {
		k = domain.DefaultRRFK
	}

	text := strings.TrimSpace(q.Text)
	if text == "" && len(q.Vector) == 0 {
		return nil, fmt.Errorf("%w: a search needs a question", store.ErrInvalidQuery)
	}
	if q.ViewerID == uuid.Nil {
		return nil, fmt.Errorf("%w: a search needs a viewer", store.ErrInvalidQuery)
	}

	// Filters apply inside BOTH arms rather than to the fused result. Applied
	// afterwards, a narrow search would first fill its candidate lists with the
	// whole library and then discard nearly all of them, leaving a handful of
	// passages from the books actually being asked about.
	var filters strings.Builder
	args := []any{}

	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	vectorArg := "NULL"
	if len(q.Vector) > 0 {
		vectorArg = arg(vectorLiteral(q.Vector)) + "::vector"
	}
	modelArg := arg(q.ModelID)
	textArg := arg(text)
	viewerArg := arg(q.ViewerID)

	if len(q.WorkIDs) > 0 {
		filters.WriteString(" AND ed.work_id = ANY(" + arg(q.WorkIDs) + ")")
	}
	if q.BookFileID != nil {
		filters.WriteString(" AND c.book_file_id = " + arg(*q.BookFileID))
	}

	candidatesArg := arg(candidates)
	poolArg := arg(candidates * fallbackPool)
	maxWordsArg := arg(fallbackWords)
	// Built into the statement rather than passed, so that without the
	// fallback its step is planned as never running at all.
	fallbackGate := "false"
	if fallback {
		fallbackGate = "true"
		if slowFallbackForTest {
			fallbackGate = "(SELECT pg_sleep(1)) IS NOT NULL"
		}
	}
	kArg := arg(k)
	limitArg := arg(limit)

	// The joins are needed in both arms because the filters reference them, and
	// again at the end for the citation's title.
	// The access join is part of the FROM clause rather than a WHERE condition
	// so that it cannot be forgotten by a later edit to the filters, and so
	// that a passage from a book this person cannot open is not merely
	// deprioritised but absent.
	joins := `
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = c.book_file_id AND rbf.user_id = ` + viewerArg + `
		JOIN book_files bf ON bf.id = c.book_file_id AND bf.missing_since IS NULL
		JOIN editions ed ON ed.id = bf.edition_id`

	sql := `
		WITH dense AS (
			SELECT c.id, row_number() OVER (ORDER BY e.vector <=> ` + vectorArg + `) AS rank
			FROM chunks c
			JOIN embeddings e ON e.chunk_id = c.id AND e.model_id = ` + modelArg + `
			` + joins + `
			WHERE ` + vectorArg + ` IS NOT NULL` + filters.String() + `
			ORDER BY e.vector <=> ` + vectorArg + `
			LIMIT ` + candidatesArg + `
		),
		every_word AS (
			SELECT c.id, ts_rank_cd(c.tsv, query) AS score
			FROM chunks c
			` + joins + `,
			     websearch_to_tsquery('english', ` + textArg + `) AS query
			WHERE c.tsv @@ query` + filters.String() + `
			ORDER BY score DESC, c.id
			LIMIT ` + candidatesArg + `
		),
		-- The question's words, any of them rather than all, for when
		-- requiring every word finds too little.
		--
		-- At most fallbackWords of them, and the rarest that are in the
		-- library at all. A word in no passage cannot match, and costs one
		-- index probe to rule out. Among the rest, the planner's own
		-- statistics on the column say which lexemes are common; a word they
		-- do not list counts as rare, in the order asked. Unbounded, a
		-- 400-word question took 11.6 seconds on 100,000 passages, and the
		-- rare words are the ones that say what a question is about.
		--
		-- From to_tsvector, so Postgres does the parsing, stemming and stop
		-- word removal, and quoted lexeme by lexeme into a tsquery. Known
		-- skew: a hyphenated word is three lexemes here (well-known, well,
		-- known) and so counts three times towards matched.
		stats AS (
			SELECT most_common_elems::text::text[] AS elems, most_common_elem_freqs AS freqs
			FROM pg_stats
			WHERE schemaname = current_schema() AND tablename = 'chunks' AND attname = 'tsv'
		),
		lexemes AS (
			SELECT q.lexeme, q.positions[1] AS position,
			       ('''' || replace(replace(q.lexeme, '\', '\\'), '''', '''''') || '''')::tsquery AS term
			FROM unnest(to_tsvector('english', ` + textArg + `)) AS q
		),
		picked AS (
			SELECT l.lexeme, l.term
			FROM lexemes l
			WHERE ` + fallbackGate + `
			  AND EXISTS (SELECT 1 FROM chunks c WHERE c.tsv @@ l.term)
			ORDER BY COALESCE((SELECT s.freqs[array_position(s.elems, l.lexeme)] FROM stats s), 0),
			         l.position
			LIMIT ` + maxWordsArg + `
		),
		words AS (
			SELECT string_agg(term::text, ' | ')::tsquery AS any_word,
			       array_agg(lexeme) AS each_word,
			       -- A query that excludes a word has said what it does not
			       -- want, and matching any word would bring it back.
			       (SELECT strpos(websearch_to_tsquery('english', ` + textArg + `)::text, '!') > 0) AS excludes
			FROM picked
		),
		-- Every passage with any of the words, most of the words first. Only
		-- this pool is ranked by cover density, in the next step: ranking
		-- every match that way is what made a common word slow.
		any_words AS (
			SELECT c.id, c.tsv,
			       -- How many of the question's words the passage has: what
			       -- deleting them takes out of its vector.
			       length(c.tsv) - length(ts_delete(c.tsv, w.each_word)) AS matched
			FROM chunks c
			` + joins + `,
			     words w
			WHERE ` + fallbackGate + `
			  AND c.tsv @@ w.any_word
			  -- Only when requiring every word came back short.
			  AND (SELECT count(*) FROM every_word) < ` + limitArg + `
			  AND NOT w.excludes
			  AND c.id NOT IN (SELECT id FROM every_word)` + filters.String() + `
			ORDER BY matched DESC, c.id
			LIMIT ` + poolArg + `
		),
		some_words AS (
			SELECT a.id, a.matched, ts_rank_cd(a.tsv, w.any_word) AS score
			FROM any_words a, words w
			ORDER BY a.matched DESC, score DESC, a.id
			LIMIT ` + candidatesArg + `
		),
		sparse AS (
			SELECT id, row_number() OVER (ORDER BY tier, place) AS rank
			FROM (
				SELECT id, 0 AS tier, row_number() OVER (ORDER BY score DESC, id) AS place
				FROM every_word
				UNION ALL
				SELECT id, 1 AS tier, row_number() OVER (ORDER BY matched DESC, score DESC, id) AS place
				FROM some_words
			) AS tiers
			ORDER BY rank
			LIMIT ` + candidatesArg + `
		),
		fused AS (
			SELECT COALESCE(d.id, s.id) AS id,
			       COALESCE(d.rank, 0) AS dense_rank,
			       COALESCE(s.rank, 0) AS sparse_rank,
			       CASE WHEN d.rank IS NULL THEN 0 ELSE 1.0 / (` + kArg + ` + d.rank) END +
			       CASE WHEN s.rank IS NULL THEN 0 ELSE 1.0 / (` + kArg + ` + s.rank) END AS score
			FROM dense d
			FULL OUTER JOIN sparse s ON s.id = d.id
		)
		SELECT c.id, c.book_file_id, c.seq,
		       c.start_major, c.start_path, c.start_offset,
		       c.end_major, c.end_path, c.end_offset,
		       COALESCE(c.chapter_title, ''), c.content, c.token_count,
		       bf.format, ed.work_id, w.title,
		       f.dense_rank, f.sparse_rank, f.score
		FROM fused f
		JOIN chunks c ON c.id = f.id
		JOIN book_files bf ON bf.id = c.book_file_id
		JOIN editions ed ON ed.id = bf.edition_id
		JOIN works w ON w.id = ed.work_id
		ORDER BY f.score DESC, c.book_file_id, c.seq
		LIMIT ` + limitArg

	// The HNSW index stops at ef_search candidates, whose default is 40 - so
	// asking for 60 quietly got 40, and the reasoning behind DefaultCandidates
	// ("a passage ranked 30th by meaning and 2nd by keyword is only visible if
	// both arms were asked for at least 30") was half untrue in practice.
	// Measured on this library: LIMIT 60 returned 40 rows from the index scan,
	// and 60 once the depth was raised.
	//
	// Comfortably above the candidate count rather than equal to it, because
	// every filter - a shared library, a pinned book, a subject - discards rows
	// AFTER the index scan, so the shortfall compounds exactly when the search
	// is most constrained. SET LOCAL, so it dies with the transaction rather
	// than following the connection back into the pool.
	tx, err := p.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`SET LOCAL hnsw.ef_search = `+strconv.Itoa(efSearchFor(candidates))); err != nil {
		return nil, fmt.Errorf("setting the search depth: %w", mapError(err))
	}
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = `+
		strconv.FormatInt(searchStatementTimeout.Milliseconds(), 10)); err != nil {
		return nil, fmt.Errorf("setting the search timeout: %w", mapError(err))
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("searching: %w", mapError(err))
	}
	defer rows.Close()

	var out []domain.RetrievedChunk
	for rows.Next() {
		var r domain.RetrievedChunk
		var format domain.Format
		var startMajor, endMajor int

		if err := rows.Scan(&r.Chunk.ID, &r.Chunk.BookFileID, &r.Chunk.Seq,
			&startMajor, &r.Chunk.Range.Start.ElementPath, &r.Chunk.Range.Start.CharOffset,
			&endMajor, &r.Chunk.Range.End.ElementPath, &r.Chunk.Range.End.CharOffset,
			&r.Chunk.ChapterTitle, &r.Chunk.Content, &r.Chunk.TokenCount,
			&format, &r.WorkID, &r.WorkTitle,
			&r.DenseRank, &r.SparseRank, &r.FusedScore); err != nil {
			return nil, fmt.Errorf("scanning search result: %w", err)
		}

		applyChunkFormat(&r.Chunk, format, startMajor, endMajor)
		out = append(out, r)
	}
	return out, rows.Err()
}

// applyChunkFormat restores what the stored `major` columns meant, which the
// format alone decides: a page for PDF, a spine index for EPUB.
func applyChunkFormat(c *domain.Chunk, format domain.Format, startMajor, endMajor int) {
	c.Range.Start.Format = format
	c.Range.End.Format = format
	if format == domain.FormatPDF {
		c.Range.Start.Page = startMajor
		c.Range.End.Page = endMajor
		return
	}
	c.Range.Start.SpineIndex = startMajor
	c.Range.End.SpineIndex = endMajor
}

// WorksMatchingSubjects finds books tagged with any of the given subjects.
//
// This is what routing uses: a question about eigenvalues should look at the
// mathematics books first, and "which books are about mathematics" is a fact
// the library already holds from ingest and enrichment.
func (p *Pool) WorksMatchingSubjects(ctx context.Context, subjects []string, limit int) ([]uuid.UUID, error) {
	if len(subjects) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	slugs := make([]string, 0, len(subjects))
	for _, s := range subjects {
		if slug := TagSlug(s); slug != "" {
			slugs = append(slugs, slug)
		}
	}
	if len(slugs) == 0 {
		return nil, nil
	}

	rows, err := p.Query(ctx, `
		SELECT w.id
		FROM works w
		JOIN work_tags wt ON wt.work_id = w.id
		JOIN tags t ON t.id = wt.tag_id
		WHERE t.slug = ANY($1)
		GROUP BY w.id
		-- Books matching more of the subjects first: a question about "linear
		-- algebra" and "proofs" is better answered by a book about both.
		ORDER BY count(DISTINCT t.slug) DESC, w.title
		LIMIT $2`, slugs, limit)
	if err != nil {
		return nil, fmt.Errorf("finding works by subject: %w", mapError(err))
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning work id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LibraryFacts is what the library knows about itself.
//
// Assembled for answering questions about the collection rather than about its
// contents - "how many books do I have", "what am I reading", "what is most of
// this about". Those need SQL and no retrieval at all, which is the cheapest
// thing routing does.
type LibraryFacts struct {
	Works      int
	Files      int
	Authors    int
	Formats    map[domain.Format]int
	NeedsOCR   int
	Missing    int
	Untitled   int
	Searchable int

	TopSubjects []SubjectCount
	TopAuthors  []AuthorCount
	Recent      []string
	InProgress  []string
}

// SubjectCount is one subject and how many books carry it.
type SubjectCount struct {
	Name  string
	Books int
}

// AuthorCount is one author and how many of their books are on the shelf.
type AuthorCount struct {
	Name  string
	Books int
}

// LibraryFacts gathers the collection's own statistics.
func (p *Pool) LibraryFacts(ctx context.Context, userID uuid.UUID) (LibraryFacts, error) {
	stats, err := p.Stats(ctx, userID)
	if err != nil {
		return LibraryFacts{}, err
	}

	facts := LibraryFacts{
		Works:    stats.Works,
		Files:    stats.Files,
		Authors:  stats.Authors,
		Formats:  stats.ByFormat,
		NeedsOCR: stats.NeedsOCR,
		Missing:  stats.Missing,
	}

	if err := p.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM works
		         WHERE title = $1 AND id IN (SELECT work_id FROM readable_works WHERE user_id = $2)),
		       (SELECT count(DISTINCT c.book_file_id) FROM chunks c
		         JOIN readable_book_files rbf
		           ON rbf.book_file_id = c.book_file_id AND rbf.user_id = $2)`,
		PlaceholderTitle, userID,
	).Scan(&facts.Untitled, &facts.Searchable); err != nil {
		return LibraryFacts{}, fmt.Errorf("reading library facts: %w", mapError(err))
	}

	subjects, err := p.Query(ctx, `
		SELECT t.name, count(DISTINCT wt.work_id) AS books
		FROM tags t
		JOIN work_tags wt ON wt.tag_id = t.id
		WHERE wt.work_id IN (SELECT work_id FROM readable_works WHERE user_id = $1)
		GROUP BY t.name
		ORDER BY books DESC, t.name
		LIMIT 15`, userID)
	if err != nil {
		return LibraryFacts{}, fmt.Errorf("reading subjects: %w", mapError(err))
	}
	for subjects.Next() {
		var s SubjectCount
		if err := subjects.Scan(&s.Name, &s.Books); err != nil {
			subjects.Close()
			return LibraryFacts{}, fmt.Errorf("scanning subject: %w", err)
		}
		facts.TopSubjects = append(facts.TopSubjects, s)
	}
	subjects.Close()
	if err := subjects.Err(); err != nil {
		return LibraryFacts{}, fmt.Errorf("reading subjects: %w", mapError(err))
	}

	authors, err := p.Query(ctx, `
		SELECT a.name, count(DISTINCT wa.work_id) AS books
		FROM authors a
		JOIN work_authors wa ON wa.author_id = a.id
		WHERE wa.role = 'author'
		  AND wa.work_id IN (SELECT work_id FROM readable_works WHERE user_id = $1)
		GROUP BY a.id, a.name
		ORDER BY books DESC, a.sort_name
		LIMIT 15`, userID)
	if err != nil {
		return LibraryFacts{}, fmt.Errorf("reading authors: %w", mapError(err))
	}
	for authors.Next() {
		var a AuthorCount
		if err := authors.Scan(&a.Name, &a.Books); err != nil {
			authors.Close()
			return LibraryFacts{}, fmt.Errorf("scanning author: %w", err)
		}
		facts.TopAuthors = append(facts.TopAuthors, a)
	}
	authors.Close()
	if err := authors.Err(); err != nil {
		return LibraryFacts{}, fmt.Errorf("reading authors: %w", mapError(err))
	}

	facts.Recent, err = p.titleList(ctx, `
		SELECT w.title FROM works w
		JOIN editions e ON e.work_id = w.id
		JOIN book_files bf ON bf.edition_id = e.id
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = $1
		WHERE bf.missing_since IS NULL
		GROUP BY w.id, w.title
		ORDER BY max(bf.created_at) DESC
		LIMIT 10`, userID)
	if err != nil {
		return LibraryFacts{}, err
	}

	facts.InProgress, err = p.titleList(ctx, `
		SELECT w.title FROM reading_progress rp
		JOIN book_files bf ON bf.id = rp.book_file_id
		JOIN editions e ON e.id = bf.edition_id
		JOIN works w ON w.id = e.work_id
		WHERE rp.user_id = $1 AND bf.missing_since IS NULL
		ORDER BY rp.last_read_at DESC
		LIMIT 5`, userID)
	if err != nil {
		return LibraryFacts{}, err
	}

	return facts, nil
}

func (p *Pool) titleList(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := p.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("reading titles: %w", mapError(err))
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			return nil, fmt.Errorf("scanning title: %w", err)
		}
		out = append(out, title)
	}
	return out, rows.Err()
}

// SubjectMatch is a tag the library really has, how many books carry it, and
// how much the match is worth.
type SubjectMatch struct {
	Slug  string
	Books int
	// Relevance is how informative the words that matched are.
	//
	// Matching "agile" says a great deal, because one tag in this library
	// contains it. Matching "development" says almost nothing, because a
	// dozen do. Without this the loose matching was worse than the exact
	// matching it replaced: a one-book tag called "android-development"
	// matched "software development" on its second word and, being small,
	// outranked everything.
	Relevance float64
}

// stopTokens are words that match half the shelf and narrow nothing.
var stopTokens = map[string]bool{
	"and": true, "the": true, "for": true, "with": true, "using": true,
	"introduction": true, "guide": true, "book": true, "books": true,
}

// MatchingSubjects finds the tags that a set of requested subjects reaches.
//
// Exactly, when the library happens to use the same words; otherwise by any
// whole word they share. The router asked for "agile methodology" and this
// library files that book under "agile software development" - no exact slug,
// no books, and a reading list about agile with no agile book in it. Neither
// name is wrong; they are two people describing the same shelf.
//
// Whole words, not substrings: "java" must not match "javascript", and
// matching on fragments is how "art" starts matching "particle".
func (p *Pool) MatchingSubjects(ctx context.Context, subjects []string) ([]SubjectMatch, error) {
	tokens := make([]string, 0, len(subjects)*3)
	seen := map[string]bool{}
	for _, subject := range subjects {
		for _, token := range strings.Split(TagSlug(subject), "-") {
			// Three letters is the floor: "ai" and "go" are real subjects, but
			// they are also fragments of a hundred words, and this matches
			// whole slug segments rather than substrings so they are safe.
			if token == "" || seen[token] || stopTokens[token] {
				continue
			}
			seen[token] = true
			tokens = append(tokens, token)
		}
	}
	if len(tokens) == 0 {
		return nil, nil
	}

	rows, err := p.Query(ctx, `
		WITH wanted AS (SELECT unnest($1::text[]) AS token),
		-- How many tags contain each word, which is how much matching it is
		-- worth: a word in one tag names a subject, a word in fifteen names a
		-- category.
		freq AS (
			SELECT w.token,
			       count(*) FILTER (
			           WHERE ('-' || t.slug || '-') LIKE ('%-' || w.token || '-%')
			       ) AS tags
			FROM wanted w CROSS JOIN tags t
			GROUP BY w.token
		),
		-- Relevance is summed over the WORDS, and the books are counted
		-- separately. Doing both in one join multiplied the relevance by the
		-- number of books, so the largest shelf always looked the most
		-- relevant - which is precisely backwards.
		matched AS (
			SELECT t.id, t.slug, sum(1.0 / ln(exp(1) + f.tags)) AS relevance
			FROM tags t
			JOIN wanted w ON ('-' || t.slug || '-') LIKE ('%-' || w.token || '-%')
			JOIN freq f ON f.token = w.token
			GROUP BY t.id, t.slug
		)
		SELECT m.slug, count(DISTINCT wt.work_id) AS books, m.relevance,
		       -- How many of the asked-for words this library has a tag for at
		       -- all. Relevance cannot answer that: a word found in exactly one
		       -- tag scores the HIGHEST possible relevance, because rarity is
		       -- what relevance measures.
		       (SELECT count(*) FROM freq WHERE freq.tags > 0) AS covered
		FROM matched m
		JOIN work_tags wt ON wt.tag_id = m.id
		GROUP BY m.slug, m.relevance
		ORDER BY m.relevance DESC, books, m.slug
		LIMIT 20`, tokens)
	if err != nil {
		return nil, fmt.Errorf("matching subjects: %w", mapError(err))
	}
	defer rows.Close()

	var out []SubjectMatch
	var covered int
	for rows.Next() {
		var m SubjectMatch
		if err := rows.Scan(&m.Slug, &m.Books, &m.Relevance, &covered); err != nil {
			return nil, fmt.Errorf("scanning subject match: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("matching subjects: %w", mapError(err))
	}

	// A subject this library simply does not have.
	//
	// "Medieval European history" asked for five words and this library has a
	// tag for one of them - "history", in "mathematical-history", one book
	// about the history of mathematics. That one book came back as a reading
	// list, which is the answer a person would least expect and least forgive.
	//
	// Relevance cannot catch it, and in fact points the wrong way: a word
	// found in exactly ONE tag scores the highest relevance there is, because
	// rarity is the whole of what relevance measures. The cut below is
	// relative to the best match, so a weak best match survives by being the
	// only one. What separates the two cases is how much of the QUESTION the
	// library could answer at all - "agile" matches a shelf about agile, while
	// medieval history matches one stray word out of five.
	if covered*2 < len(tokens) {
		return nil, nil
	}

	// Anything far weaker than the best match is dropped rather than ranked
	// last. A question about agile that finds an agile shelf does not want
	// seven books that merely share the word "development" with it - a reading
	// list of eight where seven are padding reads as a broken answer, which is
	// how this was noticed. Ordered by relevance already, so this is a cut
	// rather than a search.
	if len(out) > 0 {
		floor := out[0].Relevance / 2
		for i, m := range out {
			if m.Relevance < floor {
				return out[:i], nil
			}
		}
	}
	return out, nil
}
