// SPDX-License-Identifier: AGPL-3.0-or-later

package worker_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// An extraction retried by the book's only holder, with something happening to
// the file in the gap between parsing it and storing the result - forced open
// with a hook rather than hoped for, on real files. Each case pins one guard
// in the store: removing either alone fails its case.
//
//   - frozen: an ingest never re-points an archived upload; only the archive
//     moves those.
//   - PathExists: an ingest never points a record at a path that is gone.
func TestAnExtractionFinishingLateKeepsTheBook(t *testing.T) {
	for _, tc := range []struct {
		name string
		// gap runs between parse and store, and returns the path the record
		// must still point at afterwards.
		gap func(t *testing.T, r *raceRig) string
		// archived is whether the book should end up archived.
		archived bool
	}{
		{
			// Both guards: the holder lets go and the archive moves the file,
			// so the extraction's path is gone and the record archived.
			name: "let go, and the archive moved it",
			gap: func(t *testing.T, r *raceRig) string {
				return r.releaseAndMove(t)
			},
			archived: true,
		},
		{
			// frozen alone: the bytes are back at the extraction's path - a
			// fresh copy written there - but the record is archived and points
			// at the archive's copy. Re-pointing it would make the archive
			// describe a file it does not hold.
			name: "let go, moved, and the old path filled again",
			gap: func(t *testing.T, r *raceRig) string {
				moved := r.releaseAndMove(t)
				copyFile(t, moved, r.stored.Path)
				return moved
			},
			archived: true,
		},
		{
			// PathExists alone: nothing is archived, but the bytes have left
			// the extraction's path and the record already follows them.
			// Pointing it back at the empty path would lose the book, and
			// take the copy it does point at for one that was replaced.
			name: "not let go, but the file moved and its old path is empty",
			gap: func(t *testing.T, r *raceRig) string {
				elsewhere := filepath.Join(r.archiveDir, "kept-elsewhere.epub")
				if err := os.Rename(r.stored.Path, elsewhere); err != nil {
					t.Fatalf("moving: %v", err)
				}
				if _, err := r.pool.Exec(t.Context(), `UPDATE book_files SET path = $2 WHERE id = $1`,
					r.id, elsewhere); err != nil {
					t.Fatalf("recording the move: %v", err)
				}
				return elsewhere
			},
			archived: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRaceRig(t)
			r.enqueue(t)
			r.runOne(t)
			if err := r.pool.QueryRow(t.Context(), `SELECT id FROM book_files`).Scan(&r.id); err != nil {
				t.Fatalf("reading: %v", err)
			}
			// It failed once; the holder uploads it again, queueing a retry.
			if _, err := r.pool.Exec(t.Context(), `UPDATE book_files SET status = 'failed'`); err != nil {
				t.Fatalf("failing: %v", err)
			}
			r.enqueue(t)

			var want string
			worker.SetAfterParse(r.ing, func() {
				worker.SetAfterParse(r.ing, nil)
				want = tc.gap(t, r)
			})
			r.runOne(t)

			var path string
			var archived bool
			if err := r.pool.QueryRow(t.Context(),
				`SELECT path, archived_at IS NOT NULL FROM book_files WHERE id = $1`,
				r.id).Scan(&path, &archived); err != nil {
				t.Fatalf("reading: %v", err)
			}
			if path != want {
				t.Errorf("path = %q, want %q", path, want)
			}
			if _, err := os.Stat(want); err != nil {
				t.Errorf("the copy the book's record needs is gone: %v", err)
			}
			if archived != tc.archived {
				t.Errorf("archived = %v, want %v", archived, tc.archived)
			}
		})
	}
}

type raceRig struct {
	pool       *postgres.Pool
	ing        *worker.Ingest
	uploads    string
	archiveDir string
	stored     ingest.StoredFile
	holder     uuid.UUID
	id         uuid.UUID
}

func newRaceRig(t *testing.T) *raceRig {
	p := testPool(t)
	r := &raceRig{pool: p, uploads: t.TempDir(), archiveDir: t.TempDir()}
	if err := p.QueryRow(t.Context(), `INSERT INTO users (username) VALUES ($1) RETURNING id`,
		"x-"+uuid.NewString()[:8]).Scan(&r.holder); err != nil {
		t.Fatalf("creating a user: %v", err)
	}
	src := filepath.Join(t.TempDir(), "book.epub")
	writeEPUB(t, src, "The Only Copy", "An Author")
	f, err := os.Open(src)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() { _ = f.Close() }()
	r.stored, err = (&ingest.UploadStore{Root: r.uploads}).Store(t.Context(), "book.epub", f)
	if err != nil {
		t.Fatalf("storing: %v", err)
	}
	if _, err := p.HoldUpload(t.Context(), postgres.HoldInput{ContentHash: r.stored.ContentHash, UserID: r.holder}); err != nil {
		t.Fatalf("HoldUpload: %v", err)
	}
	r.ing = &worker.Ingest{Store: p, CoverDir: t.TempDir(), ArchiveDir: r.archiveDir, Logger: quietTestLogger()}
	return r
}

// enqueue queues the extraction an upload of the file queues.
func (r *raceRig) enqueue(t *testing.T) {
	t.Helper()
	target := worker.TargetForUpload(r.stored.ContentHash, r.holder)
	if _, _, err := r.pool.Enqueue(t.Context(), postgres.EnqueueOptions{
		Type: domain.JobExtract, TargetID: &target, RequestedBy: r.holder,
		Payload: worker.ExtractPayload{
			Path: r.stored.Path, ContentHash: r.stored.ContentHash, Format: r.stored.Format,
			SizeBytes: r.stored.SizeBytes, OriginalFilename: "book.epub", Uploaded: true, OwnerID: r.holder,
		},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
}

func (r *raceRig) runOne(t *testing.T) {
	t.Helper()
	job, err := r.pool.Claim(t.Context(), "test")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := r.ing.Extract(t.Context(), job, func(float32, string) {}); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if err := r.pool.Succeed(t.Context(), job.ID, "test"); err != nil {
		t.Fatalf("Succeed: %v", err)
	}
}

// releaseAndMove is the only holder letting the book go and the archive moving
// its file, and returns where it went.
func (r *raceRig) releaseAndMove(t *testing.T) string {
	t.Helper()
	archived, err := r.pool.ReleaseHolding(t.Context(), r.id, r.holder)
	if err != nil || len(archived) != 1 {
		t.Fatalf("ReleaseHolding = %+v, %v", archived, err)
	}
	archive := ingest.Archive{Root: r.archiveDir, Managed: r.uploads}
	moved, err := r.pool.MoveArchived(t.Context(), r.id, archived[0].Path, archive.Keep)
	if err != nil || moved == "" {
		t.Fatalf("MoveArchived = %q, %v", moved, err)
	}
	return moved
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(to)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copying: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
}
