// SPDX-License-Identifier: AGPL-3.0-or-later

// Command titles clears title suggestions a person could never accept.
//
//	go run ./scripts/titles            # report only, changes nothing
//	go run ./scripts/titles -reject    # mark them rejected
//
// Enrichment records what a model answered even when it does not apply it, so
// a title that is wrong waits in the review queue for somebody to dismiss. Most
// of those are worth a look - a model reading a subtitle off the title page is
// a real suggestion, even when the answer is no. Some are not: gemma4-12b falls
// into a repetition loop under a JSON schema and produces "Numerical Methods in
// Engineering and Science: C, C++, and MATLAB(R).(R).(R).(R)." There is nothing
// there to decide.
//
// Rejecting rather than deleting, because the ledger is the record of what the
// model said and that is what makes a bad prompt visible later. The review
// queue simply stops showing them.
//
// The rule is internal/ingest.PlausibleTitle - the same function the store
// consults when it decides whether to apply a title - so this cannot drift from
// what enrichment does today. Suggestions made after that rule existed are
// already refused at the point they are made; this is for the ones recorded
// before it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

type suggestion struct {
	ID    uuid.UUID
	Work  string
	Title string
}

func main() {
	reject := flag.Bool("reject", false, "mark the unusable suggestions rejected instead of only reporting them")
	flag.Parse()

	if err := run(*reject); err != nil {
		fmt.Fprintln(os.Stderr, "titles:", err)
		os.Exit(1)
	}
}

func run(reject bool) error {
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
		SELECT e.id, w.title, e.value
		FROM enrichment_records e
		JOIN works w ON w.id = e.work_id
		WHERE e.field = 'title'
		  AND e.source = 'llm'
		  AND e.accepted_at IS NULL
		  AND e.rejected_at IS NULL
		ORDER BY w.title`)
	if err != nil {
		return fmt.Errorf("reading suggestions: %w", err)
	}

	var pending, unusable []suggestion
	for rows.Next() {
		var s suggestion
		var raw []byte
		if err := rows.Scan(&s.ID, &s.Work, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("scanning suggestion: %w", err)
		}
		// Stored as a JSON value, so a title is a quoted string rather than the
		// bare text.
		if err := json.Unmarshal(raw, &s.Title); err != nil {
			s.Title = string(raw)
		}
		pending = append(pending, s)
		if !ingest.PlausibleTitle(s.Title) {
			unusable = append(unusable, s)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading suggestions: %w", err)
	}

	fmt.Printf("%d title suggestions waiting, %d of them unusable\n\n", len(pending), len(unusable))
	if len(unusable) == 0 {
		fmt.Println("nothing to reject")
		return nil
	}

	for _, s := range unusable {
		fmt.Printf("  %-44.44s  %.90s\n", s.Work, s.Title)
	}

	if !reject {
		fmt.Printf("\n%d suggestions to reject; re-run with -reject to apply\n", len(unusable))
		return nil
	}

	ids := make([]uuid.UUID, 0, len(unusable))
	for _, s := range unusable {
		ids = append(ids, s.ID)
	}
	tag, err := pool.Exec(ctx, `
		UPDATE enrichment_records SET rejected_at = now()
		WHERE id = ANY($1) AND accepted_at IS NULL AND rejected_at IS NULL`, ids)
	if err != nil {
		return fmt.Errorf("rejecting: %w", err)
	}

	fmt.Printf("\nrejected %d suggestions\n", tag.RowsAffected())
	return nil
}
