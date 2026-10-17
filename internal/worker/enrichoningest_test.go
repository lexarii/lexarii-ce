// SPDX-License-Identifier: AGPL-3.0-or-later

package worker_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// enrichRecorder stands in for the model: it records who each enrich job was
// for and describes nothing.
type enrichRecorder struct {
	mu     sync.Mutex
	byWork map[uuid.UUID]uuid.UUID // work -> requested by
}

func (r *enrichRecorder) handlers(ing *worker.Ingest) map[domain.JobType]worker.Handler {
	r.byWork = map[uuid.UUID]uuid.UUID{}
	h := ing.Handlers()
	h[domain.JobEnrich] = func(_ context.Context, job postgres.Job, _ worker.ProgressFunc) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.byWork[*job.TargetID] = job.RequestedBy
		return nil
	}
	return h
}

// An upload's extraction queues the description of its book, for the
// uploader, who may follow it; a scan's, for whoever ran it - or nobody, for a
// scan nobody in particular asked for. A file that could not be read queues
// nothing.
func TestEnrichOnIngestQueuesForWhoeverAddedTheBook(t *testing.T) {
	t.Run("an upload", func(t *testing.T) {
		r := newRaceRig(t)
		r.ing.Enriching = r.pool
		r.enqueue(t)
		rec := &enrichRecorder{}
		drainWith(t, r.pool, rec.handlers(r.ing))

		var work uuid.UUID
		if err := r.pool.QueryRow(t.Context(), `
			SELECT e.work_id FROM book_files bf JOIN editions e ON e.id = bf.edition_id`).Scan(&work); err != nil {
			t.Fatalf("reading: %v", err)
		}
		if who, ok := rec.byWork[work]; !ok || who != r.holder {
			t.Errorf("enrichment ran for %v (queued: %v); want once, for the uploader %v", who, ok, r.holder)
		}
	})

	t.Run("a scan nobody asked for", func(t *testing.T) {
		p := testPool(t)
		root := t.TempDir()
		writeEPUB(t, filepath.Join(root, "one.epub"), "One", "An Author")
		writeEPUB(t, filepath.Join(root, "two.epub"), "Two", "Another Author")
		if err := os.WriteFile(filepath.Join(root, "broken.epub"), []byte("not a zip"), 0o644); err != nil {
			t.Fatalf("writing: %v", err)
		}
		ing := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger(), Enriching: p}
		if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
			t.Fatalf("enqueue scan: %v", err)
		}
		rec := &enrichRecorder{}
		drainWith(t, p, rec.handlers(ing))

		if len(rec.byWork) != 2 {
			t.Errorf("enriched %d works, want the two readable ones", len(rec.byWork))
		}
		for work, who := range rec.byWork {
			if who != uuid.Nil {
				t.Errorf("work %s enriched for %s, want nobody in particular", work, who)
			}
		}
		if n := countRows(t, p, `SELECT count(*) FROM jobs WHERE type = 'enrich'`); n != 2 {
			t.Errorf("enrich jobs = %d, want one per readable work", n)
		}
	})
}

// Off - the default, and what the server wires with no chat model configured
// - extraction queues no enrichment at all; and bytes removed for everyone
// never get as far as asking.
func TestEnrichOnIngestIsOptInAndNeverForBlockedBytes(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		p := testPool(t)
		root := t.TempDir()
		writeEPUB(t, filepath.Join(root, "one.epub"), "One", "An Author")
		ing := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger()}
		if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
			t.Fatalf("enqueue scan: %v", err)
		}
		drain(t, p, ing)
		if n := countRows(t, p, `SELECT count(*) FROM jobs WHERE type = 'enrich'`); n != 0 {
			t.Errorf("enrich jobs = %d with enrichment on ingest off", n)
		}
	})

	t.Run("blocked", func(t *testing.T) {
		r := newRaceRig(t)
		r.ing.Enriching = r.pool
		r.ing.UploadDir = r.uploads
		r.enqueue(t)
		if _, err := r.pool.Exec(t.Context(), `
			INSERT INTO blocked_content (content_hash, reason) VALUES ($1, 'r')`, r.stored.ContentHash); err != nil {
			t.Fatalf("blocking: %v", err)
		}
		r.runOne(t)
		if n := countRows(t, r.pool, `SELECT count(*) FROM jobs WHERE type = 'enrich'`); n != 0 {
			t.Errorf("enrich jobs = %d for blocked bytes", n)
		}
	})
}
