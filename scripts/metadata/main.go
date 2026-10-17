// SPDX-License-Identifier: AGPL-3.0-or-later

// Command metadata fills in publishers, years and ISBNs from a catalogue.
//
//	make metadata                 # report only, sends nothing
//	make metadata ARGS=-apply     # look books up and fill empty fields
//
// Most books here have no year and no publisher: an EPUB's OPF usually carries
// them and a PDF almost never does, which is why an exported citation comes out
// as a title and an author and nothing else.
//
// **This is the one thing in Lexarii that talks to a stranger.** Running it
// with -apply sends a title and an author, or an ISBN, to whatever
// LEXARII_METADATA_BASE_URL names - for each book it looks up. That is a
// description of somebody's shelf leaving their machine, so it is off unless
// configured, it reports without sending anything by default, and it is a
// command a person runs rather than something that happens quietly in a worker.
//
// Only EMPTY fields are filled. What the file itself carried is what the
// publisher wrote; a catalogue is a third party describing possibly a different
// printing.
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
	"scm.poniatowski.dev/adam/lexarii/internal/metadata"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// pause between lookups.
//
// OpenLibrary asks for about one request a second from anonymous clients and
// rate-limits harder when ignored. A library of 168 books is three minutes at
// this rate, which is the correct speed for a thing that asks somebody else's
// server for 168 favours.
const pause = time.Second

func main() {
	apply := flag.Bool("apply", false, "look books up and fill empty fields, instead of only reporting what is missing")
	limit := flag.Int("n", 50, "how many books")
	flag.Parse()

	if err := run(*apply, *limit); err != nil {
		fmt.Fprintln(os.Stderr, "metadata:", err)
		os.Exit(1)
	}
}

func run(apply bool, limit int) error {
	ctx := context.Background()

	database := os.Getenv("DATABASE_URL")
	if database == "" {
		return errors.New("DATABASE_URL is not set; run `make metadata` rather than `go run`")
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

	// The person who owns the most books, NOT EnsureDefaultUser - that one
	// creates the account it cannot find, so a maintenance command run against
	// a real library quietly added an admin user to it. A command that reports
	// must not write anything at all.
	owner, err := libraryOwner(ctx, pool)
	if err != nil {
		return err
	}

	books, err := pool.NeedsExternalMetadata(ctx, owner, limit)
	if err != nil {
		return err
	}
	if len(books) == 0 {
		fmt.Println("every book has a publisher, a year and an ISBN")
		return nil
	}

	if !apply {
		fmt.Printf("%d books are missing a publisher, a year, or an ISBN:\n\n", len(books))
		for _, b := range books {
			fmt.Printf("  %-60s %s\n", truncate(b.Title, 60), known(b))
		}
		fmt.Printf("\nNothing was sent anywhere. `make metadata ARGS=-apply` looks these up at\n")
		if cfg.MetadataBaseURL == "" {
			fmt.Printf("a catalogue - set LEXARII_METADATA_BASE_URL first, e.g. https://openlibrary.org\n")
		} else {
			fmt.Printf("%s, one request per book.\n", cfg.MetadataBaseURL)
		}
		return nil
	}

	client := metadata.New(cfg.MetadataBaseURL, cfg.MetadataUserAgent, cfg.MetadataTimeout)
	if client == nil {
		return errors.New("LEXARII_METADATA_BASE_URL is not set, so there is no catalogue to ask")
	}
	fmt.Printf("looking up %d books at %s, one per second\n\n", len(books), cfg.MetadataBaseURL)

	var filled, missed, failed int
	for i, b := range books {
		if i > 0 {
			time.Sleep(pause)
		}

		found, err := look(ctx, client, b)
		switch {
		case errors.Is(err, metadata.ErrNotFound):
			missed++
			fmt.Printf("  %-50s not in the catalogue\n", truncate(b.Title, 50))
			continue
		case err != nil:
			failed++
			fmt.Printf("  %-50s %v\n", truncate(b.Title, 50), err)
			continue
		}

		outcome, err := pool.ApplyExternalMetadata(ctx, postgres.ExternalMetadata{
			WorkID:    b.WorkID,
			Publisher: found.Publisher,
			Year:      found.Year,
			ISBN13:    found.ISBN13,
			ISBN10:    found.ISBN10,
			PageCount: found.PageCount,
			// Only used when the book has no author at all, which in this
			// library is 70 of 168 - unattributed on the shelf and exported as
			// citations with nobody's name on them.
			Authors: found.Authors,
			Source:  found.Source,
		})
		if err != nil {
			failed++
			fmt.Printf("  %-50s %v\n", truncate(b.Title, 50), err)
			continue
		}

		if len(outcome.Filled) == 0 {
			fmt.Printf("  %-50s nothing to add\n", truncate(b.Title, 50))
			continue
		}
		filled++
		fmt.Printf("  %-50s + %s\n", truncate(b.Title, 50), strings.Join(outcome.Filled, ", "))
	}

	fmt.Printf("\n%d books filled in, %d not in the catalogue, %d failed\n", filled, missed, failed)
	return nil
}

// libraryOwner is whoever holds the most books here.
//
// Read-only, and it fails rather than inventing somebody: a library with no
// books has nothing for this command to do anyway.
func libraryOwner(ctx context.Context, pool *postgres.Pool) (uuid.UUID, error) {
	var owner uuid.UUID
	err := pool.QueryRow(ctx, `
		SELECT user_id FROM book_file_holders
		GROUP BY user_id
		ORDER BY count(*) DESC, user_id
		LIMIT 1`).Scan(&owner)
	if err != nil {
		return uuid.Nil, fmt.Errorf("finding who owns this library: %w", err)
	}
	return owner, nil
}

// look asks by ISBN when there is one and by title otherwise.
//
// An ISBN names one printing; a title is a guess. Preferring the exact question
// is most of what keeps a wrong year out of somebody's library.
func look(ctx context.Context, client *metadata.Client, b postgres.WorkLookup) (metadata.Book, error) {
	if isbn := firstNonEmpty(b.ISBN13, b.ISBN10); isbn != "" {
		found, err := client.ByISBN(ctx, isbn)
		if err == nil || !errors.Is(err, metadata.ErrNotFound) {
			return found, err
		}
		// An ISBN the catalogue does not have is worth a title search: the
		// number may be a reprint's, or simply absent from their data.
	}
	return client.ByTitle(ctx, b.Title, b.Authors)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// known says what the library already has, so the report shows what a lookup
// would actually be for.
func known(b postgres.WorkLookup) string {
	if isbn := firstNonEmpty(b.ISBN13, b.ISBN10); isbn != "" {
		return "ISBN " + isbn
	}
	if len(b.Authors) > 0 {
		return "by " + b.Authors[0]
	}
	return "title only"
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}
