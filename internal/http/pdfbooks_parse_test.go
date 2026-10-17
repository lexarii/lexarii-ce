// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"

	pdfingest "scm.poniatowski.dev/adam/lexarii/internal/ingest/pdf"
)

// A book is parsed once and every later page turn reuses the parse.
func TestTheCacheParsesAFileOnce(t *testing.T) {
	c := newBookCache(0)
	if c.budget != DefaultBudget {
		t.Errorf("a zero budget became %d, want the default %d", c.budget, DefaultBudget)
	}

	id, path := uuid.New(), writeReaderPDF(t, 3)
	first, err := c.Book(id, path)
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	if first.Pages() != 3 {
		t.Errorf("pages = %d, want 3", first.Pages())
	}
	again, err := c.Book(id, path)
	if err != nil {
		t.Fatalf("Book again: %v", err)
	}
	if again != first {
		t.Error("the second ask parsed the book again")
	}
	if c.order.Len() != 1 || c.held != estimate(path) {
		t.Errorf("held %d books and %d bytes, want 1 book and %d bytes", c.order.Len(), c.held, estimate(path))
	}
}

// Two people opening the same title at once share one parse.
func TestConcurrentReadersShareOneParse(t *testing.T) {
	c := newBookCache(0)
	id, path := uuid.New(), writeReaderPDF(t, 2)

	const readers = 8
	got := make([]*pdfingest.Book, readers)
	errs := make([]error, readers)
	var wg sync.WaitGroup
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = c.Book(id, path)
		}()
	}
	wg.Wait()

	for i := range readers {
		if errs[i] != nil {
			t.Fatalf("reader %d: %v", i, errs[i])
		}
		if got[i] != got[0] {
			t.Fatalf("reader %d was given a different parse", i)
		}
	}
	if c.order.Len() != 1 {
		t.Errorf("held %d entries for one book", c.order.Len())
	}
}

// The same book file at a new path is a different file on disk, and must not
// be served from the old parse.
func TestAMovedFileIsParsedAgain(t *testing.T) {
	c := newBookCache(0)
	id := uuid.New()
	before, err := c.Book(id, writeReaderPDF(t, 2))
	if err != nil {
		t.Fatalf("Book: %v", err)
	}
	newPath := writeReaderPDF(t, 4)
	after, err := c.Book(id, newPath)
	if err != nil {
		t.Fatalf("Book after the move: %v", err)
	}
	if after == before || after.Pages() != 4 {
		t.Fatalf("served %d pages from the stale parse, want the new file's 4", after.Pages())
	}
	if c.order.Len() != 1 || c.held != estimate(newPath) {
		t.Errorf("held %d entries and %d bytes; the stale parse was not released", c.order.Len(), c.held)
	}
}

// A failed parse is not remembered: the next ask tries again rather than
// inheriting a failure for as long as the entry would have survived.
func TestAFailedParseIsNotCached(t *testing.T) {
	c := newBookCache(0)
	id := uuid.New()
	path := filepath.Join(t.TempDir(), "half-written.pdf")
	if err := os.WriteFile(path, []byte("not yet a PDF"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if _, err := c.Book(id, path); err == nil {
		t.Fatal("a file that is not a PDF parsed")
	}
	if c.order.Len() != 0 || len(c.entries) != 0 || c.held != 0 {
		t.Fatalf("a failed parse was kept: %d entries, %d bytes held", c.order.Len(), c.held)
	}

	if err := os.WriteFile(path, readerPDFBytes(t, 1), 0o600); err != nil {
		t.Fatalf("finishing the file: %v", err)
	}
	parsed, err := c.Book(id, path)
	if err != nil {
		t.Fatalf("the retry inherited the failure: %v", err)
	}
	if parsed.Pages() != 1 {
		t.Errorf("pages = %d, want 1", parsed.Pages())
	}
}

// A file the cache cannot size is assumed to be large rather than free.
func TestAnUnstattableFileIsEstimatedGenerously(t *testing.T) {
	if got := estimate(filepath.Join(t.TempDir(), "absent.pdf")); got != minimumCost {
		t.Errorf("estimate = %d, want the minimum cost %d", got, minimumCost)
	}
	empty := filepath.Join(t.TempDir(), "empty.pdf")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if got := estimate(empty); got != minimumCost {
		t.Errorf("estimate of an empty file = %d, want the minimum cost %d", got, minimumCost)
	}
}
