// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// Subject is one tag and how much of this person's library carries it.
type Subject struct {
	Name  string
	Slug  string
	Books int
}

// Subjects lists what a library is about, commonest first.
//
// Counted over the books this viewer may actually read, like everything else
// here. An unscoped version would say "42 books on Kubernetes" to somebody who
// was lent two, which both misreports their library and describes somebody
// else's.
//
// The long tail is not collapsed, filtered, or merged. Measured on a real
// library: 136 of 218 subjects name exactly one book, and reading them makes
// the case - bayesian statistics, ansible, device drivers, econometrics,
// cheminformatics. Each is a true and specific fact about a book, and the
// entry that asked for a controlled vocabulary was solving for a tag list
// nobody could see. Ordering by book count puts the shape of the library first
// and leaves the tail reachable, which is what was actually missing.
func (p *Pool) Subjects(ctx context.Context, viewer uuid.UUID) ([]Subject, error) {
	if viewer == uuid.Nil {
		return nil, fmt.Errorf("%w: subjects need a viewer", store.ErrInvalidQuery)
	}

	rows, err := p.Query(ctx, `
		SELECT t.name, t.slug, count(DISTINCT wt.work_id) AS books
		FROM tags t
		JOIN work_tags wt ON wt.tag_id = t.id
		JOIN readable_works rw ON rw.work_id = wt.work_id AND rw.user_id = $1
		GROUP BY t.id, t.name, t.slug
		ORDER BY count(DISTINCT wt.work_id) DESC, t.name`, viewer)
	if err != nil {
		return nil, fmt.Errorf("reading subjects: %w", mapError(err))
	}
	defer rows.Close()

	var out []Subject
	for rows.Next() {
		var s Subject
		if err := rows.Scan(&s.Name, &s.Slug, &s.Books); err != nil {
			return nil, fmt.Errorf("scanning subject: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
