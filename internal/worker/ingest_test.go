// SPDX-License-Identifier: AGPL-3.0-or-later

package worker_test

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

func testPool(t *testing.T) *postgres.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := postgres.Open(t.Context(), url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)
	lockTestDB(t, pool)

	// Each test starts from an empty library and queue.
	for _, stmt := range []string{"DELETE FROM jobs", "DELETE FROM holding_claims", "DELETE FROM released_holdings", "DELETE FROM blocked_content",
		"DELETE FROM works", "DELETE FROM authors", "DELETE FROM tags"} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("cleaning (%s): %v", stmt, err)
		}
	}
	return pool
}

func writeEPUB(t *testing.T, path, title, author string) {
	t.Helper()

	opf := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>%s</dc:title>
    <dc:creator>%s</dc:creator>
    <dc:language>en</dc:language>
    <dc:subject>Computer science</dc:subject>
  </metadata>
  <manifest>
    <item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="c2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="c1"/><itemref idref="c2"/></spine>
</package>`, title, author)

	entries := map[string]string{
		"mimetype": "application/epub+zip",
		"META-INF/container.xml": `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`,
		"OEBPS/content.opf": opf,
		"OEBPS/ch1.xhtml":   "<html><body><p>Chapter one.</p></body></html>",
		"OEBPS/ch2.xhtml":   "<html><body><p>Chapter two.</p></body></html>",
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatalf("creating %q: %v", name, err)
		}
		if _, err := w.Write([]byte(entries[name])); err != nil {
			t.Fatalf("writing %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}
}

// drain runs the worker pool until the queue is empty.
func drain(t *testing.T, p *postgres.Pool, ing *worker.Ingest) {
	t.Helper()
	drainWith(t, p, ing.Handlers())
}

// drainWith runs the worker pool with the given handlers until the queue is
// empty.
func drainWith(t *testing.T, p *postgres.Pool, handlers map[domain.JobType]worker.Handler) {
	t.Helper()

	pool := worker.New(p, handlers, worker.Config{
		Workers:         3,
		PollInterval:    5 * time.Millisecond,
		ReclaimInterval: time.Hour,
		Logger:          quietTestLogger(),
	})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	deadline := time.After(15 * time.Second)
	idle := 0
	for {
		depth, err := p.QueueDepth(t.Context())
		if err != nil {
			t.Fatalf("QueueDepth: %v", err)
		}
		total := 0
		for _, n := range depth {
			total += n
		}
		if total == 0 {
			// Require a couple of consecutive empty reads so a job in flight
			// between claim and completion is not mistaken for an empty queue.
			idle++
			if idle >= 3 {
				break
			}
		} else {
			idle = 0
		}
		select {
		case <-deadline:
			t.Fatalf("timed out with %d jobs still queued", total)
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("pool: %v", err)
	}
}

// The whole M1 pipeline: files on disk, through the queue, into the database.
func TestScanAndExtractEndToEnd(t *testing.T) {
	p := testPool(t)
	root := t.TempDir()

	writeEPUB(t, filepath.Join(root, "Knuth", "taocp.epub"), "The Art of Computer Programming", "Donald Knuth")
	writeEPUB(t, filepath.Join(root, "Fiction", "dispossessed.epub"), "The Dispossessed", "Ursula K. Le Guin")
	if err := os.WriteFile(filepath.Join(root, "broken.epub"), []byte("not a zip"), 0o644); err != nil {
		t.Fatalf("writing broken file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "cover.jpg"), []byte("ignored"), 0o644); err != nil {
		t.Fatalf("writing jpg: %v", err)
	}

	ing := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger()}

	if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
		t.Fatalf("enqueue scan: %v", err)
	}
	drain(t, p, ing)

	var works, files int
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM works`).Scan(&works); err != nil {
		t.Fatalf("counting works: %v", err)
	}
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM book_files`).Scan(&files); err != nil {
		t.Fatalf("counting files: %v", err)
	}
	if works != 3 || files != 3 {
		t.Fatalf("works = %d, book_files = %d; want 3 each (two good books and one broken one)", works, files)
	}

	// The good books landed with their real metadata.
	var title, author string
	err := p.QueryRow(t.Context(), `
		SELECT w.title, a.name
		FROM works w
		JOIN work_authors wa ON wa.work_id = w.id
		JOIN authors a ON a.id = wa.author_id
		WHERE w.title = 'The Dispossessed'`).Scan(&title, &author)
	if err != nil {
		t.Fatalf("reading the parsed book: %v", err)
	}
	if author != "Ursula K. Le Guin" {
		t.Errorf("author = %q", author)
	}

	// The broken one is recorded with its error rather than silently omitted.
	var status, ingestErr string
	err = p.QueryRow(t.Context(), `
		SELECT status, COALESCE(ingest_error, '') FROM book_files WHERE path LIKE '%broken.epub'`,
	).Scan(&status, &ingestErr)
	if err != nil {
		t.Fatalf("reading the broken file: %v", err)
	}
	if status != string(domain.IngestFailed) {
		t.Errorf("status = %q, want failed", status)
	}
	if ingestErr == "" {
		t.Error("no error was recorded for the broken file")
	}

	// A file that is not a book is not a book.
	var jpgRows int
	if err := p.QueryRow(t.Context(),
		`SELECT count(*) FROM book_files WHERE path LIKE '%cover.jpg'`).Scan(&jpgRows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if jpgRows != 0 {
		t.Error("a jpg was ingested as a book")
	}
}

// A rescan of an unchanged library must be nearly free: no new rows, and no
// extraction work queued at all.
func TestRescanQueuesNothingForUnchangedFiles(t *testing.T) {
	p := testPool(t)
	root := t.TempDir()
	writeEPUB(t, filepath.Join(root, "book.epub"), "Unchanged", "Author")

	ing := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger()}

	if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drain(t, p, ing)

	var extractsAfterFirst int
	if err := p.QueryRow(t.Context(),
		`SELECT count(*) FROM jobs WHERE type = 'extract'`).Scan(&extractsAfterFirst); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if extractsAfterFirst != 1 {
		t.Fatalf("first scan queued %d extract jobs, want 1", extractsAfterFirst)
	}

	// Second scan over the same, unchanged library.
	if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
		t.Fatalf("enqueue rescan: %v", err)
	}
	drain(t, p, ing)

	var extractsAfterSecond, works int
	if err := p.QueryRow(t.Context(),
		`SELECT count(*) FROM jobs WHERE type = 'extract'`).Scan(&extractsAfterSecond); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM works`).Scan(&works); err != nil {
		t.Fatalf("counting works: %v", err)
	}

	if extractsAfterSecond != extractsAfterFirst {
		t.Errorf("rescan queued %d more extract jobs; an unchanged library should queue none",
			extractsAfterSecond-extractsAfterFirst)
	}
	if works != 1 {
		t.Errorf("works = %d after a rescan, want 1", works)
	}
}

// Deleting a file marks it missing. It is never removed, because that would
// take the user's annotations and reading position with it.
func TestDeletedFileIsMarkedMissingNotRemoved(t *testing.T) {
	p := testPool(t)
	root := t.TempDir()

	keep := filepath.Join(root, "keep.epub")
	remove := filepath.Join(root, "remove.epub")
	writeEPUB(t, keep, "Kept", "Author")
	writeEPUB(t, remove, "Removed", "Author")

	ing := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger()}

	if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drain(t, p, ing)

	if err := os.Remove(remove); err != nil {
		t.Fatalf("removing: %v", err)
	}
	// MarkMissing compares against the scan's start time, so the rows must be
	// older than the next scan begins.
	time.Sleep(10 * time.Millisecond)

	if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
		t.Fatalf("enqueue rescan: %v", err)
	}
	drain(t, p, ing)

	var total int
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM book_files`).Scan(&total); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if total != 2 {
		t.Fatalf("book_files = %d, want 2 - a deleted file is flagged, never removed", total)
	}

	var missingPath string
	if err := p.QueryRow(t.Context(),
		`SELECT path FROM book_files WHERE missing_since IS NOT NULL`).Scan(&missingPath); err != nil {
		t.Fatalf("reading missing file: %v", err)
	}
	if missingPath != remove {
		t.Errorf("missing file = %q, want %q", missingPath, remove)
	}

	var stillPresent int
	if err := p.QueryRow(t.Context(),
		`SELECT count(*) FROM book_files WHERE path = $1 AND missing_since IS NULL`, keep,
	).Scan(&stillPresent); err != nil {
		t.Fatalf("checking the kept file: %v", err)
	}
	if stillPresent != 1 {
		t.Error("the file that is still there was marked missing")
	}
}

// Reorganising the library directory must not look like a library of new books.
func TestMovedFileIsNotDuplicated(t *testing.T) {
	p := testPool(t)
	root := t.TempDir()

	original := filepath.Join(root, "Inbox", "book.epub")
	writeEPUB(t, original, "Moved Book", "Author")

	ing := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger()}

	if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	drain(t, p, ing)

	moved := filepath.Join(root, "Sorted", "Author", "renamed.epub")
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Rename(original, moved); err != nil {
		t.Fatalf("rename: %v", err)
	}

	if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
		t.Fatalf("enqueue rescan: %v", err)
	}
	drain(t, p, ing)

	var files, works int
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM book_files`).Scan(&files); err != nil {
		t.Fatalf("counting files: %v", err)
	}
	if err := p.QueryRow(t.Context(), `SELECT count(*) FROM works`).Scan(&works); err != nil {
		t.Fatalf("counting works: %v", err)
	}
	if files != 1 || works != 1 {
		t.Fatalf("book_files = %d, works = %d after a move; want 1 each", files, works)
	}

	var path string
	var missing *time.Time
	if err := p.QueryRow(t.Context(), `SELECT path, missing_since FROM book_files`).Scan(&path, &missing); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if path != moved {
		t.Errorf("path = %q, want the new location", path)
	}
	if missing != nil {
		t.Error("a moved file was marked missing")
	}
}

// Test packages that touch the database run concurrently by default, and each
// one truncates shared tables in setup. A session-level advisory lock serialises
// them without relying on anyone remembering `go test -p 1`.
const testDBLockKey = 0x5348454c464d4b01

// lockTestDB holds the shared test lock for the duration of one test. The lock
// is session-scoped, so it is taken on one dedicated connection.
func lockTestDB(t *testing.T, pool *postgres.Pool) {
	t.Helper()

	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquiring a connection for the test lock: %v", err)
	}
	if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_lock($1)", int64(testDBLockKey)); err != nil {
		conn.Release()
		t.Fatalf("taking the test lock: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(testDBLockKey)); err != nil {
			t.Logf("releasing the test lock: %v", err)
		}
		conn.Release()
	})
}
