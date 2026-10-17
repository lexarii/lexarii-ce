// SPDX-License-Identifier: AGPL-3.0-or-later

package worker_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// indexingHandlers runs ingest and chunking for real. Embedding is a no-op
// here: it needs a model, and what is under test is that chunking is asked
// for, not what the model makes of it.
func indexingHandlers(p *postgres.Pool, ing *worker.Ingest) map[domain.JobType]worker.Handler {
	index := &worker.Index{Store: p, Logger: quietTestLogger()}
	handlers := ing.Handlers()
	handlers[domain.JobChunk] = index.Chunk
	handlers[domain.JobEmbed] = func(context.Context, postgres.Job, worker.ProgressFunc) error { return nil }
	return handlers
}

func countRows(t *testing.T, p *postgres.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// With indexing on ingest, a scanned library is searchable when the queue
// drains: every readable file chunked, once, and nothing queued for the file
// that could not be read.
func TestIndexOnIngestChunksWhatWasScanned(t *testing.T) {
	p := testPool(t)
	root := t.TempDir()
	writeEPUB(t, filepath.Join(root, "one.epub"), "One", "An Author")
	writeEPUB(t, filepath.Join(root, "two.epub"), "Two", "Another Author")
	if err := os.WriteFile(filepath.Join(root, "broken.epub"), []byte("not a zip"), 0o644); err != nil {
		t.Fatalf("writing broken file: %v", err)
	}

	ing := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger(), Indexing: p}
	if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
		t.Fatalf("enqueue scan: %v", err)
	}
	drainWith(t, p, indexingHandlers(p, ing))

	if n := countRows(t, p, `SELECT count(*) FROM jobs WHERE type = 'chunk'`); n != 2 {
		t.Errorf("chunk jobs = %d, want one for each readable file", n)
	}
	if n := countRows(t, p, `SELECT count(DISTINCT book_file_id) FROM chunks`); n != 2 {
		t.Errorf("files with passages = %d, want both readable files searchable", n)
	}
	if n := countRows(t, p, `
		SELECT count(*) FROM chunks c JOIN book_files bf ON bf.id = c.book_file_id
		WHERE bf.path LIKE '%broken.epub'`); n != 0 {
		t.Errorf("the unreadable file has %d passages", n)
	}

	// Extracting the same files again - a forced rescan, an upload of a book
	// already on the shelf - leaves their passages and embeddings alone.
	before := countRows(t, p, `SELECT count(*) FROM jobs WHERE type = 'chunk'`)
	enqueueExtract(t, p, filepath.Join(root, "one.epub"))
	drainWith(t, p, indexingHandlers(p, ing))
	if after := countRows(t, p, `SELECT count(*) FROM jobs WHERE type = 'chunk'`); after != before {
		t.Errorf("re-extracting a chunked file queued %d more chunk jobs, want none", after-before)
	}
}

// An extraction re-run after it stored the file but before it queued the
// chunking - the crash between the two - still queues it, because what is
// asked is whether the file is waiting, not whether it is new.
func TestIndexOnIngestSurvivesAnInterruptedExtraction(t *testing.T) {
	p := testPool(t)
	root := t.TempDir()
	path := filepath.Join(root, "one.epub")
	writeEPUB(t, path, "One", "An Author")

	// The first run stores the file and never gets as far as queuing.
	stored := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger()}
	enqueueExtract(t, p, path)
	drainWith(t, p, indexingHandlers(p, stored))
	if n := countRows(t, p, `SELECT count(*) FROM jobs WHERE type = 'chunk'`); n != 0 {
		t.Fatalf("chunk jobs = %d with indexing on ingest off, want none", n)
	}

	resumed := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger(), Indexing: p}
	enqueueExtract(t, p, path)
	drainWith(t, p, indexingHandlers(p, resumed))
	if n := countRows(t, p, `SELECT count(DISTINCT book_file_id) FROM chunks`); n != 1 {
		t.Errorf("files with passages = %d after the re-run, want the stored file chunked", n)
	}
}

// Off, which is the default, nothing is chunked until somebody asks.
func TestWithoutIndexOnIngestNothingIsChunked(t *testing.T) {
	p := testPool(t)
	root := t.TempDir()
	writeEPUB(t, filepath.Join(root, "one.epub"), "One", "An Author")

	ing := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger()}
	if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
		t.Fatalf("enqueue scan: %v", err)
	}
	drainWith(t, p, indexingHandlers(p, ing))

	if n := countRows(t, p, `SELECT count(*) FROM jobs WHERE type IN ('chunk', 'embed')`); n != 0 {
		t.Errorf("indexing jobs = %d with indexing on ingest off, want none", n)
	}
}

// enqueueExtract queues the extraction of one file directly, as an upload or
// a scan would after hashing it.
func enqueueExtract(t *testing.T, p *postgres.Pool, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	hash := sha256Hex(raw)
	if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{
		Type:     domain.JobExtract,
		TargetID: ptr(worker.TargetForHash(hash)),
		Payload: worker.ExtractPayload{
			Path: path, ContentHash: hash, Format: domain.FormatEPUB, SizeBytes: int64(len(raw)),
		},
	}); err != nil {
		t.Fatalf("enqueue extract: %v", err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ptr(id uuid.UUID) *uuid.UUID { return &id }
