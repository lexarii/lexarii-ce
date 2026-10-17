// SPDX-License-Identifier: AGPL-3.0-or-later

// Command authors finds author rows that are one person written two ways, and
// optionally merges them.
//
//	go run ./scripts/authors            # report only, changes nothing
//	go run ./scripts/authors -merge     # apply what it is confident about
//	go run ./scripts/authors -prune     # detach names that are not people
//
// Reporting is the default because merging is destructive and unrecoverable:
// once two rows are one, the fact that they were ever separate is gone. A
// duplicate author is a cosmetic problem; merging a married pair of co-authors
// into one person destroys two bibliographies. The rule that decides is in
// internal/ingest.SameAuthor, and the reasoning for it is there too.
//
// Candidates come from the trigram index, which is only a way of avoiding an
// n-squared scan. Similarity NEVER decides - on the real library it gets three
// of the five closest pairs wrong.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

func main() {
	var (
		merge     = flag.Bool("merge", false, "apply the merges instead of only reporting them")
		prune     = flag.Bool("prune", false, "detach author rows that are not people")
		threshold = flag.Float64("threshold", 0.35, "trigram similarity below which pairs are not even considered")
	)
	flag.Parse()

	if err := run(*merge, *prune, *threshold); err != nil {
		fmt.Fprintln(os.Stderr, "authors:", err)
		os.Exit(1)
	}
}

func run(merge, prune bool, threshold float64) error {
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

	// Reported before anything is applied, so the two flags can be used
	// independently and so a dry run shows both halves.
	out, err := pool.NormaliseAuthors(ctx, threshold, false)
	if err != nil {
		return err
	}

	if len(out.Pruned) == 0 {
		fmt.Println("no author rows look like machine accounts")
	} else {
		fmt.Printf("%d author rows are not people:\n", len(out.Pruned))
		for _, im := range out.Pruned {
			fmt.Printf("  %-28s on %d book%s\n", im.Name, im.Books, plural(im.Books))
		}
		if !prune {
			fmt.Println("  re-run with -prune to detach them; the books keep their real authors")
		}
	}
	fmt.Println()

	fmt.Printf("%d candidate pairs above %.2f similarity\n\n",
		len(out.Merged)+len(out.KeptSame), threshold)

	if len(out.KeptSame) > 0 {
		fmt.Println("kept apart:")
		for _, c := range out.KeptSame {
			fmt.Printf("  %-24s %-24s  %.2f  %s\n",
				c.KeepName, c.DropName, c.Similarity, reason(c))
		}
		fmt.Println()
	}

	if len(out.Merged) == 0 {
		fmt.Println("nothing to merge")
	} else {
		fmt.Println("one person:")
		for _, c := range out.Merged {
			fmt.Printf("  %-24s %-24s  %.2f  keeping %q\n",
				c.KeepName, c.DropName, c.Similarity, c.KeepName)
		}
		if !merge {
			fmt.Printf("\n%d merges available; re-run with -merge to apply them\n", len(out.Merged))
		}
	}

	if !merge && !prune {
		return nil
	}

	// The worker runs exactly this, on a timer. Running it here is for a
	// library that predates the rules, or for somebody who wants to watch it
	// happen rather than find it in a log.
	applied, err := pool.NormaliseAuthors(ctx, threshold, true)
	if err != nil {
		return err
	}
	fmt.Printf("\napplied: %d detached, %d merged\n", len(applied.Pruned), len(applied.Merged))
	return nil
}

func reason(c postgres.AuthorPair) string {
	if ingest.DifferentByCoauthorship(c.Shared) {
		return fmt.Sprintf("co-author the same %d work(s)", c.Shared)
	}
	return "names are not compatible"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
