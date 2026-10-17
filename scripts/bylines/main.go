// SPDX-License-Identifier: AGPL-3.0-or-later

// Command bylines finds books with no author and reads one off the title page.
//
//	make bylines                  # report only, asks the model nothing
//	make bylines ARGS=-apply      # read the byline and save it
//
// Twenty-nine books in the library this was written against have no author at
// all. They are unattributed on the shelf, cannot be found by author, and
// export as citations with nobody's name on them.
//
// The model has been reading those bylines all along. The classify prompt has
// asked for `authors` since v1, the reply carries them, and the result was
// dropped on the floor between the worker and the store - so every one of
// those books was described by a model that had the answer in front of it.
// That is fixed; this is how the books already enriched catch up, since the
// enrichment queue selects on prompt version and every one of them has already
// been seen by this prompt.
//
// Only books with NO author are touched. A model's reading of a byline is a
// weaker claim than the file's own metadata and must never compete with it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/extract"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/llm/prompts"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// sampleChars is how much of the front matter the model is shown. The same
// budget the enrichment worker uses, so this reads what that would have read.
const sampleChars = 12000

func main() {
	apply := flag.Bool("apply", false, "ask the model to read the byline, and save what it finds")
	limit := flag.Int("n", 100, "how many books")
	flag.Parse()

	if err := run(*apply, *limit); err != nil {
		fmt.Fprintln(os.Stderr, "bylines:", err)
		os.Exit(1)
	}
}

func run(apply bool, limit int) error {
	ctx := context.Background()

	database := os.Getenv("DATABASE_URL")
	if database == "" {
		return errors.New("DATABASE_URL is not set; run `make bylines` rather than `go run`")
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("reading the configuration: %w", err)
	}

	pool, err := postgres.Open(ctx, database)
	if err != nil {
		return fmt.Errorf("connecting to the database: %w", err)
	}
	defer pool.Close()

	books, err := unattributed(ctx, pool, limit)
	if err != nil {
		return err
	}
	if len(books) == 0 {
		fmt.Println("every book has an author")
		return nil
	}

	if !apply {
		fmt.Printf("%d books have no author:\n\n", len(books))
		for _, b := range books {
			fmt.Printf("  %s\n", truncate(b.Title, 72))
		}
		fmt.Printf("\nNothing was asked of anything. `make bylines ARGS=-apply` reads the\n")
		fmt.Printf("title page of each and saves the byline it finds.\n")
		return nil
	}

	if cfg.LLM.BaseURL == "" {
		return errors.New("no model configured; set LEXARII_LLM_BASE_URL")
	}
	client := llm.New(cfg.LLM)
	fmt.Printf("reading the title pages of %d books\n\n", len(books))

	var named, silent, failed int
	for _, b := range books {
		subject, err := pool.EnrichmentSubject(ctx, b.WorkID)
		if err != nil {
			failed++
			fmt.Printf("  %-60s %v\n", truncate(b.Title, 60), err)
			continue
		}

		sample, err := extract.Sample(ctx, subject.Path, subject.Format, sampleChars)
		if err != nil {
			failed++
			fmt.Printf("  %-60s could not be read: %v\n", truncate(b.Title, 60), err)
			continue
		}

		asked, cancel := context.WithTimeout(ctx, 5*time.Minute)
		result, err := llm.Classify(asked, client, prompts.ClassifyInput{
			Title:    subject.Title,
			Authors:  subject.Authors,
			Filename: subject.Filename,
			Language: subject.Language,
			Sample:   sample,
		})
		cancel()
		if err != nil {
			failed++
			fmt.Printf("  %-60s %v\n", truncate(b.Title, 60), err)
			continue
		}
		if len(result.Authors) == 0 {
			silent++
			fmt.Printf("  %-60s no byline on the page\n", truncate(b.Title, 60))
			continue
		}

		// The same save path the worker uses, so the same rules apply: the
		// byline lands only because the book has none, and a name that is
		// nowhere in the sample is recorded as a suggestion rather than
		// applied.
		outcome, err := pool.SaveEnrichment(ctx, postgres.EnrichmentInput{
			WorkID:     b.WorkID,
			Title:      result.Title,
			Subtitle:   result.Subtitle,
			Authors:    result.Authors,
			Edition:    result.Edition,
			Summary:    result.Summary,
			Series:     result.Series,
			Subjects:   result.Subjects,
			Audience:   result.Audience,
			Model:      result.Model,
			PromptID:   result.PromptID,
			Confidence: result.Confidence,
			Ungrounded: map[string]bool{
				"authors": !llm.Grounded(sample, strings.Join(result.Authors, " ")),
				"title":   result.Title != "" && !llm.Grounded(sample, result.Title),
			},
		})
		if err != nil {
			failed++
			fmt.Printf("  %-60s %v\n", truncate(b.Title, 60), err)
			continue
		}

		if applied(outcome.Applied, "authors") {
			named++
			fmt.Printf("  %-60s %s\n", truncate(b.Title, 60), strings.Join(result.Authors, ", "))
			continue
		}
		silent++
		fmt.Printf("  %-60s %s (suggested, not on the page)\n",
			truncate(b.Title, 60), strings.Join(result.Authors, ", "))
	}

	fmt.Printf("\n%d books given an author, %d left without one, %d failed\n", named, silent, failed)
	return nil
}

func applied(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

type unattributedWork struct {
	WorkID uuid.UUID
	Title  string
}

// unattributed lists books with no author row at all.
func unattributed(ctx context.Context, pool *postgres.Pool, limit int) ([]unattributedWork, error) {
	rows, err := pool.Query(ctx, `
		SELECT w.id, w.title
		FROM works w
		WHERE NOT EXISTS (SELECT 1 FROM work_authors wa WHERE wa.work_id = w.id)
		  AND EXISTS (
		      SELECT 1 FROM editions e
		      JOIN book_files bf ON bf.edition_id = e.id
		      WHERE e.work_id = w.id AND bf.missing_since IS NULL)
		ORDER BY w.title
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("finding books with no author: %w", err)
	}
	defer rows.Close()

	var out []unattributedWork
	for rows.Next() {
		var w unattributedWork
		if err := rows.Scan(&w.WorkID, &w.Title); err != nil {
			return nil, fmt.Errorf("scanning: %w", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
