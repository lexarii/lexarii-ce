// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// Mention is one book naming another.
type Mention struct {
	WorkID     uuid.UUID
	BookFileID uuid.UUID
	Title      string
	Authors    []string
	// Locator is where in that book the name appears, so the link opens at the
	// page rather than at the cover.
	Locator domain.Locator
	Excerpt string
	// Places is how many passages of that book name this one. A bibliography
	// entry is one; a book that argues with another names it twenty times.
	Places int
}

// maxMentionExcerpt bounds the quoted surroundings of a mention.
const maxMentionExcerpt = 240

// Mentions finds the books in this library that name a given one.
//
// A backlink in a library of books is not a hyperlink somebody typed. It is a
// bibliography entry, an index line, a "further reading" list: one book naming
// another in its own text. Nobody writes those down, so they have to be found,
// and the whole index is already here for retrieval.
//
// The test is the title as a PHRASE plus the author's surname in the same
// passage. Measured on a real library of 168 books, the title alone is not
// nearly enough: "Linear Algebra" as a phrase appears in 23 other books and
// "Reinforcement Learning" in 11, none of them citing anything - a title that
// is also a subject matches every book about the subject. Adding the surname
// took the whole library from a list dominated by generic phrases to 44 pairs,
// of which the ones checked by hand were genuine: `Sierra, Kathy (Head First
// Java), 4` is an index line in another book, which is exactly the thing.
//
// Two guards follow from the same measurement. A one-word title is skipped,
// because a phrase query for "Refactoring" matches every discussion of
// refactoring. And a surname under four letters is skipped, because it is
// matched inside the text rather than against a token list and a three-letter
// name is a syllable.
func (p *Pool) Mentions(ctx context.Context, workID, viewer uuid.UUID, limit int) ([]Mention, error) {
	if viewer == uuid.Nil {
		return nil, errNotFound
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	// The subject of the search, read separately so the guards can be applied
	// in Go where they can be explained, and so a book that cannot qualify
	// costs one cheap query rather than a scan.
	var title, author string
	err := p.QueryRow(ctx, `
		SELECT w.title,
		       COALESCE((SELECT a.name FROM work_authors wa
		                 JOIN authors a ON a.id = wa.author_id
		                 WHERE wa.work_id = w.id ORDER BY wa.position LIMIT 1), '')
		FROM works w
		WHERE w.id = $1
		  AND EXISTS (
		      SELECT 1 FROM book_files bf
		      JOIN editions e ON e.id = bf.edition_id
		      JOIN readable_book_files rbf
		        ON rbf.book_file_id = bf.id AND rbf.user_id = $2
		      WHERE e.work_id = w.id)`, workID, viewer).Scan(&title, &author)
	if err != nil {
		return nil, mapError(err)
	}

	phrase := strings.TrimSpace(title)
	surname := surnameOf(author)
	if len(strings.Fields(phrase)) < 2 || len([]rune(surname)) < 4 {
		return nil, nil
	}

	// One row per mentioning WORK, not per passage: a book that names this one
	// in its index and again in its bibliography is one backlink with two
	// places, not two backlinks. The excerpt is the first passage, which for a
	// bibliography is the entry itself.
	rows, err := p.Query(ctx, `
		WITH hits AS (
		    SELECT e.work_id, c.book_file_id, c.content, c.seq,
		           c.start_major, c.start_path, c.start_offset
		    FROM chunks c
		    JOIN book_files bf ON bf.id = c.book_file_id
		    JOIN editions e ON e.id = bf.edition_id
		    JOIN readable_book_files rbf
		      ON rbf.book_file_id = bf.id AND rbf.user_id = $3
		    WHERE e.work_id <> $1
		      AND c.tsv @@ phraseto_tsquery('english', $2)
		      -- The surname against the text itself rather than the tsvector:
		      -- stemming turns "Powers" into "power" and would match the word.
		      -- \m and \M are word boundaries, so "Li" does not match "library".
		      AND c.content ~* ('\m' || $4 || '\M')
		), ranked AS (
		    SELECT *, count(*) OVER (PARTITION BY work_id) AS places,
		           row_number() OVER (PARTITION BY work_id ORDER BY seq) AS n
		    FROM hits
		)
		SELECT r.work_id, r.book_file_id, bf.format, w.title, r.content, r.places,
		       r.start_major, r.start_path, r.start_offset,
		       COALESCE((SELECT array_agg(a.name ORDER BY wa.position)
		                 FROM work_authors wa JOIN authors a ON a.id = wa.author_id
		                 WHERE wa.work_id = r.work_id), '{}')
		FROM ranked r
		JOIN works w ON w.id = r.work_id
		JOIN book_files bf ON bf.id = r.book_file_id
		WHERE r.n = 1
		ORDER BY r.places DESC, w.title
		LIMIT $5`, workID, phrase, viewer, regexpQuote(surname), limit)
	if err != nil {
		return nil, fmt.Errorf("finding mentions: %w", mapError(err))
	}
	defer rows.Close()

	out := []Mention{}
	for rows.Next() {
		var m Mention
		var content string
		var format domain.Format
		var major int
		if err := rows.Scan(&m.WorkID, &m.BookFileID, &format, &m.Title, &content, &m.Places,
			&major, &m.Locator.ElementPath, &m.Locator.CharOffset, &m.Authors); err != nil {
			return nil, fmt.Errorf("scanning mention: %w", err)
		}
		// start_major is a spine index or a page depending on the format, the
		// same column serving both because a locator is one thing with two
		// spellings.
		m.Locator.Format = format
		if format == domain.FormatPDF {
			m.Locator.Page = major
		} else {
			m.Locator.SpineIndex = major
		}
		m.Excerpt = around(content, phrase, maxMentionExcerpt)
		out = append(out, m)
	}
	return out, rows.Err()
}

// surnameOf takes the last name out of a name written either way round.
func surnameOf(name string) string {
	name = strings.TrimSpace(name)
	if before, _, found := strings.Cut(name, ","); found {
		return strings.TrimSpace(before)
	}
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return ""
	}
	return fields[len(fields)-1]
}

// regexpQuote escapes a name for use inside a regular expression.
//
// The name comes out of a book's own metadata, which is untrusted input: a
// title or an author holding a bracket would otherwise be a syntax error at
// best, and at worst a pattern that scans the whole index.
func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\^$.|?*+()[]{}`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// around quotes the text surrounding the first mention of a phrase.
//
// The passage holding a bibliography entry is mostly other entries, so showing
// its start shows somebody else's citation. This finds the name.
func around(content, phrase string, width int) string {
	content = strings.Join(strings.Fields(content), " ")
	if len(content) <= width {
		return content
	}

	at := indexFold(content, phrase)
	if at < 0 {
		at = 0
	}
	start := at - width/3
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(content) {
		end = len(content)
		start = end - width
	}

	// Never cut a rune in half, and prefer a word boundary on each side.
	for start > 0 && content[start]&0xC0 == 0x80 {
		start--
	}
	for end < len(content) && content[end]&0xC0 == 0x80 {
		end++
	}
	if start > 0 {
		if i := strings.IndexByte(content[start:end], ' '); i >= 0 && i < width/4 {
			start += i + 1
		}
	}
	if end < len(content) {
		if i := strings.LastIndexByte(content[start:end], ' '); i > 0 && end-start-i < width/4 {
			end = start + i
		}
	}

	out := strings.TrimSpace(content[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(content) {
		out += "…"
	}
	return out
}

// indexFold finds a phrase ignoring case, without allocating a lowered copy of
// a whole passage for every mention.
func indexFold(haystack, needle string) int {
	return strings.Index(strings.ToLower(haystack), strings.ToLower(needle))
}
