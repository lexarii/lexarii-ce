// SPDX-License-Identifier: AGPL-3.0-or-later

// Command tags collapses subject tags that name the same subject twice.
//
//	go run ./scripts/tags            # report only, changes nothing
//	go run ./scripts/tags -merge     # apply
//
// Reporting is the default for the same reason as scripts/authors: merging is
// destructive, and a tag merge that is wrong removes a distinction the reader
// was using. The rule is internal/ingest.RedundantSubject, which is structural
// rather than statistical - see the reasoning there for why this is not done
// with embedding similarity.
//
// This deliberately does NOT try to shrink the long tail of tags that name a
// single book. Most of those are correct: a library really does have exactly
// one book on cheminformatics. Collapsing them needs a controlled vocabulary,
// which is a decision about what the library is for, not a tidy-up.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

type tag struct {
	ID    uuid.UUID
	Name  string
	Books int
}

type merge struct {
	From, Into tag
}

func main() {
	apply := flag.Bool("merge", false, "apply the merges instead of only reporting them")
	flag.Parse()

	if err := run(*apply); err != nil {
		fmt.Fprintln(os.Stderr, "tags:", err)
		os.Exit(1)
	}
}

func run(apply bool) error {
	ctx := context.Background()
	// An unset DATABASE_URL otherwise fails as a Unix socket error naming a
	// path nobody chose - `dial unix /tmp/.s.PGSQL.5432` - which reads as "the
	// database is down" rather than "this needs the environment". The Makefile
	// loads .env; a bare `go run` does not.
	database := os.Getenv("DATABASE_URL")
	if database == "" {
		return errors.New("DATABASE_URL is not set; run `set -a; . ./.env; set +a` first, " +
			"or pass it inline")
	}

	pool, err := postgres.Open(ctx, database)
	if err != nil {
		return fmt.Errorf("connecting to the database: %w", err)
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `
		SELECT t.id, t.name, count(DISTINCT wt.work_id)
		FROM tags t LEFT JOIN work_tags wt ON wt.tag_id = t.id
		GROUP BY t.id, t.name
		ORDER BY t.name`)
	if err != nil {
		return fmt.Errorf("reading tags: %w", err)
	}
	var all []tag
	for rows.Next() {
		var t tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Books); err != nil {
			rows.Close()
			return fmt.Errorf("scanning tag: %w", err)
		}
		all = append(all, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading tags: %w", err)
	}

	var merges []merge
	for _, specific := range all {
		for _, general := range all {
			if specific.ID == general.ID {
				continue
			}
			if ingest.RedundantSubject(specific.Name, general.Name) {
				merges = append(merges, merge{From: specific, Into: general})
			}
		}
	}
	sort.Slice(merges, func(i, j int) bool { return merges[i].From.Name < merges[j].From.Name })

	singles := 0
	for _, t := range all {
		if t.Books <= 1 {
			singles++
		}
	}

	fmt.Printf("%d tags, %d of them naming a single book\n\n", len(all), singles)
	if len(merges) == 0 {
		fmt.Println("nothing to merge")
		return nil
	}

	fmt.Println("same subject written twice:")
	for _, m := range merges {
		fmt.Printf("  %-26s (%d)  ->  %-20s (%d)\n",
			m.From.Name, m.From.Books, m.Into.Name, m.Into.Books)
	}

	if !apply {
		fmt.Printf("\n%d merges available; re-run with -merge to apply them\n", len(merges))
		return nil
	}

	for _, m := range merges {
		if err := applyMerge(ctx, pool, m); err != nil {
			return err
		}
	}
	fmt.Printf("\nmerged %d tags\n", len(merges))
	return nil
}

func applyMerge(ctx context.Context, pool *postgres.Pool, m merge) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("merging %q: %w", m.From.Name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Books tagged with the redundant name keep the general one. A book already
	// carrying both simply keeps the one it has.
	if _, err := tx.Exec(ctx, `
		INSERT INTO work_tags (work_id, tag_id, source, confidence)
		SELECT work_id, $1, source, confidence FROM work_tags WHERE tag_id = $2
		ON CONFLICT (work_id, tag_id) DO NOTHING`, m.Into.ID, m.From.ID); err != nil {
		return fmt.Errorf("moving %q: %w", m.From.Name, err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM work_tags WHERE tag_id = $1`, m.From.ID); err != nil {
		return fmt.Errorf("clearing %q: %w", m.From.Name, err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tags WHERE id = $1`, m.From.ID); err != nil {
		return fmt.Errorf("deleting %q: %w", m.From.Name, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("merging %q: %w", m.From.Name, err)
	}
	return nil
}
