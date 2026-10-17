// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// AuthorPair is two author rows that might be one person.
type AuthorPair struct {
	KeepID, DropID     uuid.UUID
	KeepName, DropName string
	Similarity         float64
	// Shared is how many works they are credited on together, which settles the
	// question before the names are considered: two people who co-wrote a book
	// are not one person however alike their names look.
	Shared int
}

// AuthorImposter is an author row that is not a person.
type AuthorImposter struct {
	ID    uuid.UUID
	Name  string
	Books int
}

// AuthorNormalisation reports what a normalisation pass found or did.
type AuthorNormalisation struct {
	Merged   []AuthorPair
	KeptSame []AuthorPair
	Pruned   []AuthorImposter
	// Applied says whether the changes were written or only reported.
	Applied bool
}

// AuthorCandidates finds pairs worth judging, and the ones that are one person.
//
// Similarity NEVER decides - it is only a way of avoiding an n-squared scan,
// and on the real library it gets three of the five closest pairs wrong. The
// decision is ingest.SameAuthor, with co-authorship as a veto.
// MaxSweepAuthors is where the unattended sweep stops.
//
// The comparison is every author against every other, and `similarity()` as a
// function call cannot use the trigram index - the plan is a nested loop with
// the similarity as a filter, which was verified with EXPLAIN rather than
// assumed. Measured on the real catalogue: 228 authors, 26,000 pairs, 55ms.
// That is nothing. It is also quadratic, so 6,000 authors is 18 million pairs
// and tens of seconds, every fifteen minutes, forever, on a machine that is
// also serving reads.
//
// 2,000 authors is roughly two million pairs and a few seconds. Above that the
// sweep declines and says so, and `make authors` still runs the whole thing
// for somebody who asked for it and is watching.
const MaxSweepAuthors = 2000

// CountAuthors is how many author rows exist.
func (p *Pool) CountAuthors(ctx context.Context) (int, error) {
	var n int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM authors`).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting authors: %w", mapError(err))
	}
	return n, nil
}

func (p *Pool) AuthorCandidates(ctx context.Context, threshold float64) (same, apart []AuthorPair, err error) {
	rows, err := p.Query(ctx, `
		SELECT a.id, a.name, b.id, b.name,
		       similarity(a.name, b.name),
		       (SELECT count(*) FROM work_authors wa
		         JOIN work_authors wb ON wb.work_id = wa.work_id
		        WHERE wa.author_id = a.id AND wb.author_id = b.id)
		FROM authors a
		JOIN authors b ON a.id < b.id
		WHERE similarity(a.name, b.name) > $1
		ORDER BY similarity(a.name, b.name) DESC`, threshold)
	if err != nil {
		return nil, nil, fmt.Errorf("finding author candidates: %w", mapError(err))
	}
	defer rows.Close()

	for rows.Next() {
		var (
			aID, bID     uuid.UUID
			aName, bName string
			sim          float64
			shared       int
		)
		if err := rows.Scan(&aID, &aName, &bID, &bName, &sim, &shared); err != nil {
			return nil, nil, fmt.Errorf("scanning author candidate: %w", err)
		}

		pair := AuthorPair{Similarity: sim, Shared: shared}
		// The fuller spelling survives: "Mehrzad Tabatabaian" is recoverable to
		// "M. Tabatabaian" and not the other way round.
		if fullerName(aName, bName) == aName {
			pair.KeepID, pair.KeepName, pair.DropID, pair.DropName = aID, aName, bID, bName
		} else {
			pair.KeepID, pair.KeepName, pair.DropID, pair.DropName = bID, bName, aID, aName
		}

		if ingest.DifferentByCoauthorship(shared) ||
			!ingest.SameAuthor(ingest.ParseName(aName), ingest.ParseName(bName)) {
			apart = append(apart, pair)
			continue
		}
		same = append(same, pair)
	}
	return same, apart, rows.Err()
}

// fullerName picks the spelling that carries more of the person's name.
func fullerName(a, b string) string {
	x, y := ingest.ParseName(a), ingest.ParseName(b)
	if len(x.Forenames) > 0 && len(y.Forenames) > 0 &&
		len(x.Forenames[0]) != len(y.Forenames[0]) {
		if len(x.Forenames[0]) > len(y.Forenames[0]) {
			return a
		}
		return b
	}
	if len(a) >= len(b) {
		return a
	}
	return b
}

// MergeAuthors moves every credit onto the surviving row and deletes the other.
//
// The losing spelling is kept as an alias, so a search for it still finds the
// person and the merge is not silently lossy.
func (p *Pool) MergeAuthors(ctx context.Context, pair AuthorPair) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("merging authors: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO work_authors (work_id, author_id, role, position)
		SELECT work_id, $1, role, position FROM work_authors WHERE author_id = $2
		ON CONFLICT (work_id, author_id, role) DO NOTHING`, pair.KeepID, pair.DropID); err != nil {
		return fmt.Errorf("moving credits: %w", mapError(err))
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM work_authors WHERE author_id = $1`, pair.DropID); err != nil {
		return fmt.Errorf("clearing old credits: %w", mapError(err))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE authors SET aliases = (
			SELECT array_agg(DISTINCT x) FROM unnest(aliases || $2::text[]) AS x
		) WHERE id = $1`, pair.KeepID, []string{pair.KeepName, pair.DropName}); err != nil {
		return fmt.Errorf("recording alias: %w", mapError(err))
	}
	if _, err := tx.Exec(ctx, `DELETE FROM authors WHERE id = $1`, pair.DropID); err != nil {
		return fmt.Errorf("deleting merged author: %w", mapError(err))
	}

	return tx.Commit(ctx)
}

// AuthorImposters lists author rows that ingest would no longer accept.
//
// The check is ingest.PlausibleAuthor, the same one the scanner applies to a
// new file, so the catalogue cannot drift from what is let in today. Rows
// written before a rule was tightened are exactly what this finds: "radha", a
// Unix account that reached the /Author field of five unrelated books.
func (p *Pool) AuthorImposters(ctx context.Context) ([]AuthorImposter, error) {
	rows, err := p.Query(ctx, `
		SELECT a.id, a.name, count(DISTINCT wa.work_id)
		FROM authors a
		LEFT JOIN work_authors wa ON wa.author_id = a.id
		GROUP BY a.id, a.name
		ORDER BY count(DISTINCT wa.work_id) DESC, a.name`)
	if err != nil {
		return nil, fmt.Errorf("reading authors: %w", mapError(err))
	}
	defer rows.Close()

	var out []AuthorImposter
	for rows.Next() {
		var im AuthorImposter
		if err := rows.Scan(&im.ID, &im.Name, &im.Books); err != nil {
			return nil, fmt.Errorf("scanning author: %w", err)
		}
		if !ingest.PlausibleAuthor(im.Name) {
			out = append(out, im)
		}
	}
	return out, rows.Err()
}

// PruneAuthor detaches an author row that is not a person.
//
// The books keep every real author they have and lose the one that was never
// somebody. A book left with no author at all is the honest outcome, and the
// same one as if the file had declared nothing - which also lets enrichment
// fill the field, since a model may fill an empty field and may not overrule a
// publisher's.
func (p *Pool) PruneAuthor(ctx context.Context, id uuid.UUID) error {
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pruning author: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `DELETE FROM work_authors WHERE author_id = $1`, id); err != nil {
		return fmt.Errorf("detaching author: %w", mapError(err))
	}
	if _, err := tx.Exec(ctx, `DELETE FROM authors WHERE id = $1`, id); err != nil {
		return fmt.Errorf("deleting author: %w", mapError(err))
	}
	return tx.Commit(ctx)
}

// NormaliseAuthors prunes rows that are not people and merges the ones that are
// the same person.
//
// Both rules are deterministic and neither consults a model: PlausibleAuthor
// decides what is a person, SameAuthor decides who is the same one, and
// co-authorship vetoes a merge outright. That is what makes this safe to run
// unattended - the same inputs give the same answer every time, and the answer
// does not depend on which order the library was scanned in.
//
// `apply` false reports without writing, which is what the script does by
// default and what makes the change reviewable before it happens.
func (p *Pool) NormaliseAuthors(ctx context.Context, threshold float64, apply bool) (AuthorNormalisation, error) {
	out := AuthorNormalisation{Applied: apply}

	imposters, err := p.AuthorImposters(ctx)
	if err != nil {
		return out, err
	}
	out.Pruned = imposters

	same, apart, err := p.AuthorCandidates(ctx, threshold)
	if err != nil {
		return out, err
	}
	out.Merged, out.KeptSame = same, apart

	if !apply {
		return out, nil
	}

	// Pruning first: a name that is not a person should never be considered for
	// a merge, and doing it in this order means it cannot be.
	for _, im := range imposters {
		if err := p.PruneAuthor(ctx, im.ID); err != nil {
			return out, err
		}
	}
	for _, pair := range same {
		if err := p.MergeAuthors(ctx, pair); err != nil {
			return out, err
		}
	}
	return out, nil
}
