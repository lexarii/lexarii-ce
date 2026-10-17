// SPDX-License-Identifier: AGPL-3.0-or-later

package pdf

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// TestExtractPageFromARealBook runs against a book on disk, and skips without
// one.
//
//	LEXARII_TEST_PDF=/path/to/book.pdf go test ./internal/ingest/pdf -run RealBook -v
//
// A fixture proves the call compiles. Only a real technical book - scanned
// figures, embedded fonts, a cross-reference stream - proves that one page can
// be lifted out of it at a speed a reader can wait for.
func TestExtractPageFromARealBook(t *testing.T) {
	path := os.Getenv("LEXARII_TEST_PDF")
	if path == "" {
		t.Skip("no LEXARII_TEST_PDF; skipping the real-book page test")
	}

	start := time.Now()
	page, err := ExtractPage(path, 42)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ExtractPage: %v", err)
	}

	if !bytes.HasPrefix(page, []byte("%PDF-")) {
		t.Fatalf("result is not a PDF: % x", page[:min(8, len(page))])
	}

	whole, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The point of the exercise: the recipient gets a page, not the book.
	if int64(len(page)) >= whole.Size() {
		t.Fatalf("one page is %d bytes against a %d byte book", len(page), whole.Size())
	}

	t.Logf("page 42: %d bytes from a %d byte book (%.1f%%), in %v",
		len(page), whole.Size(),
		100*float64(len(page))/float64(whole.Size()), elapsed.Round(time.Millisecond))
}

func TestExtractPageRejectsNonsense(t *testing.T) {
	if _, err := ExtractPage("/nonexistent.pdf", 1); err == nil {
		t.Fatal("a missing file was accepted")
	}
	if _, err := ExtractPage("/nonexistent.pdf", 0); err == nil {
		t.Fatal("page zero was accepted; PDF pages are 1-based")
	}
}

// The measurement that decides whether a shared PDF is readable at all: the
// first page pays for parsing, every page after it must not.
func TestOpenBookServesPagesQuickly(t *testing.T) {
	path := os.Getenv("LEXARII_TEST_PDF")
	if path == "" {
		t.Skip("no LEXARII_TEST_PDF; skipping the real-book page test")
	}

	start := time.Now()
	book, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	parse := time.Since(start)

	if book.Pages() < 2 {
		t.Fatalf("book reports %d pages", book.Pages())
	}

	// Several pages in a row, as a reader turning them would ask.
	var slowest time.Duration
	for _, n := range []int{42, 43, 44, 1, book.Pages()} {
		start = time.Now()
		page, err := book.Page(n)
		took := time.Since(start)
		if err != nil {
			t.Fatalf("page %d: %v", n, err)
		}
		if !bytes.HasPrefix(page, []byte("%PDF-")) {
			t.Fatalf("page %d is not a PDF", n)
		}
		if took > slowest {
			slowest = took
		}
	}

	t.Logf("parsed in %v, then %d pages, slowest %v",
		parse.Round(time.Millisecond), 5, slowest.Round(time.Millisecond))

	// A page turn a person waits on. Parsing is allowed to be slow; this is not.
	if slowest > 2*time.Second {
		t.Fatalf("a page turn took %v, which is too slow to read with", slowest)
	}
}

func TestPageRejectsOutOfRange(t *testing.T) {
	path := os.Getenv("LEXARII_TEST_PDF")
	if path == "" {
		t.Skip("no LEXARII_TEST_PDF")
	}
	book, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := book.Page(0); err == nil {
		t.Error("page zero was accepted")
	}
	if _, err := book.Page(book.Pages() + 1); err == nil {
		t.Error("a page past the end was accepted")
	}
}
