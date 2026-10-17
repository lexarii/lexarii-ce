// SPDX-License-Identifier: AGPL-3.0-or-later

// Command classifyeval runs two classifier prompts over the same books.
//
//	go run ./scripts/classifyeval -a v2 -b v3 -n 20
//
// A prompt change is a claim about what a model will do with a real book, and
// the only honest way to check one is to run the old text and the new against
// the same pages and read both answers. Changing a prompt because the new
// wording sounds firmer is how a library ends up with 105 suggestions nobody
// can accept.
//
// It asks for the books whose titles the previous prompt got wrong - the ones
// with a title suggestion still waiting - because those are the cases a new
// version is meant to fix. -all takes whatever the library has instead.
//
// Nothing is written. This reads the library and talks to the model; the
// results go to the terminal for a person to read.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/extract"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/llm/prompts"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// sampleChars matches the enrichment worker's default. Measuring against a
// different amount of book than production sends would measure nothing.
const sampleChars = 16000

type book struct {
	Title    string
	Path     string
	Format   domain.Format
	Language string
	Pending  string // the title suggestion waiting for review, if any
}

func main() {
	a := flag.String("a", "v2", "prompt version to run first")
	b := flag.String("b", prompts.ClassifyVersion, "prompt version to run second")
	n := flag.Int("n", 12, "how many books")
	all := flag.Bool("all", false, "any book, rather than only ones with a title suggestion waiting")
	like := flag.String("like", "", "only books whose path contains this")
	known := flag.Bool("known", false, "tell the model the title already recorded, as the enrichment worker does")
	flag.Parse()

	if err := run(*a, *b, *n, *all, *like, *known); err != nil {
		fmt.Fprintln(os.Stderr, "classifyeval:", err)
		os.Exit(1)
	}
}

func run(versionA, versionB string, n int, all bool, like string, known bool) error {
	ctx := context.Background()

	// The Makefile loads .env; a bare `go run` does not, and an unset
	// DATABASE_URL otherwise fails as a Unix socket error naming a path nobody
	// chose.
	database := os.Getenv("DATABASE_URL")
	if database == "" {
		return errors.New("DATABASE_URL is not set; run `set -a; . ./.env; set +a` first")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("reading the configuration: %w", err)
	}
	client := llm.New(cfg.LLM)
	if client == nil {
		return errors.New("no model configured; set LEXARII_LLM_BASE_URL")
	}

	pool, err := postgres.Open(ctx, database)
	if err != nil {
		return fmt.Errorf("connecting to the database: %w", err)
	}
	defer pool.Close()

	books, err := pick(ctx, pool, n, all, like)
	if err != nil {
		return err
	}
	if len(books) == 0 {
		return errors.New("no books to measure")
	}

	fmt.Printf("%d books, %s against %s, model %s\n\n", len(books), versionA, versionB, client.Model())

	var scoreA, scoreB tally
	for i, bk := range books {
		sample, err := extract.Sample(ctx, bk.Path, bk.Format, sampleChars)
		if err != nil {
			fmt.Printf("%2d. %s\n    sample: %v\n\n", i+1, bk.Title, err)
			continue
		}

		in := prompts.ClassifyInput{
			// Deliberately NOT passing the recorded title by default: this
			// measures what the model reads off the page, and handing it the
			// answer first measures nothing. -known reproduces what the
			// enrichment worker actually sends, which is not the same prompt.
			Filename: bk.Path,
			Language: bk.Language,
			Sample:   sample,
		}
		if known {
			in.Title = bk.Title
		}

		fmt.Printf("%2d. %s\n", i+1, bk.Title)
		if bk.Pending != "" {
			fmt.Printf("    waiting: %q\n", bk.Pending)
		}
		resA := ask(ctx, client, versionA, in)
		resB := ask(ctx, client, versionB, in)
		report(versionA, resA, &scoreA)
		report(versionB, resB, &scoreB)
		fmt.Println()
	}

	fmt.Println("clean titles - no edition, no subtitle, no byline, no publisher:")
	scoreA.print(versionA)
	scoreB.print(versionB)
	return nil
}

type result struct {
	c    llm.Classification
	err  error
	took time.Duration
}

func ask(ctx context.Context, client *llm.Client, version string, in prompts.ClassifyInput) result {
	start := time.Now()
	c, err := llm.ClassifyAt(ctx, client, version, in)
	return result{c: c, err: err, took: time.Since(start)}
}

func report(version string, r result, t *tally) {
	if r.err != nil {
		fmt.Printf("    %-3s %v\n", version, r.err)
		t.failed++
		return
	}
	extra := ""
	if r.c.Subtitle != "" {
		extra += fmt.Sprintf("  subtitle=%q", r.c.Subtitle)
	}
	if r.c.Edition != "" {
		extra += fmt.Sprintf("  edition=%q", r.c.Edition)
	}
	fmt.Printf("    %-3s %-6s %q%s\n", version, r.took.Round(100*time.Millisecond), r.c.Title, extra)

	t.total++
	if why := dirty(r.c.Title); why != "" {
		t.dirty = append(t.dirty, fmt.Sprintf("%s (%s)", r.c.Title, why))
		return
	}
	t.clean++
}

type tally struct {
	total  int
	clean  int
	failed int
	dirty  []string
}

func (t tally) print(version string) {
	if t.total == 0 {
		fmt.Printf("  %-3s no answers\n", version)
		return
	}
	fmt.Printf("  %-3s %d of %d (%.0f%%)", version, t.clean, t.total, 100*float64(t.clean)/float64(t.total))
	if t.failed > 0 {
		fmt.Printf(", %d failed", t.failed)
	}
	fmt.Println()
	for _, d := range t.dirty {
		fmt.Printf("      %s\n", d)
	}
}

// dirty names what a title still carries that belongs in another field.
//
// The same categories the 105 waiting suggestions were counted with, so the
// before and after numbers mean the same thing.
var (
	bylineish  = regexp.MustCompile(`(?i)\bby\b|\bedited by\b|,\s*ph\.?d`)
	editionish = regexp.MustCompile(`(?i)\bedition\b|\b\d+(st|nd|rd|th)\s+ed\b`)
	pubish     = regexp.MustCompile(`(?i)\bisbn\b|\bpress\b|\bpublish`)
)

func dirty(title string) string {
	switch {
	case title == "":
		return "empty"
	case bylineish.MatchString(title):
		return "byline"
	case editionish.MatchString(title):
		return "edition"
	case pubish.MatchString(title):
		return "publisher"
	case strings.Contains(title, ":"), strings.Contains(title, " - "):
		return "subtitle"
	case strings.Contains(title, ","):
		return "comma"
	}
	return ""
}

func pick(ctx context.Context, pool *postgres.Pool, n int, all bool, like string) ([]book, error) {
	// One file per work, and the readable formats only: a CBZ has no text to
	// sample and would measure the sampler rather than the prompt.
	query := `
		SELECT DISTINCT ON (w.id)
		       w.title, bf.path, bf.format, COALESCE(w.language, ''),
		       COALESCE((SELECT e.value #>> '{}' FROM enrichment_records e
		                 WHERE e.work_id = w.id AND e.field = 'title' AND e.source = 'llm'
		                   AND e.accepted_at IS NULL AND e.rejected_at IS NULL
		                 LIMIT 1), '')
		FROM works w
		JOIN editions ed ON ed.work_id = w.id
		JOIN book_files bf ON bf.edition_id = ed.id
		WHERE bf.missing_since IS NULL
		  AND bf.format IN ('epub', 'pdf')
		  AND bf.status <> 'needs_ocr'`
	if !all {
		query += `
		  AND EXISTS (SELECT 1 FROM enrichment_records e
		              WHERE e.work_id = w.id AND e.field = 'title' AND e.source = 'llm'
		                AND e.accepted_at IS NULL AND e.rejected_at IS NULL)`
	}
	args := []any{n}
	if like != "" {
		args = append(args, like)
		query += `
		  AND bf.path ILIKE '%' || $2 || '%'`
	}
	query += `
		ORDER BY w.id, bf.format
		LIMIT $1`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("reading books: %w", err)
	}
	defer rows.Close()

	var out []book
	for rows.Next() {
		var bk book
		if err := rows.Scan(&bk.Title, &bk.Path, &bk.Format, &bk.Language, &bk.Pending); err != nil {
			return nil, fmt.Errorf("reading a book: %w", err)
		}
		out = append(out, bk)
	}
	return out, rows.Err()
}
