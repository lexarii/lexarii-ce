// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// The last holder lets a book go, its file is moved into the archive, and
// somebody uploads the same bytes - in every order those can interleave, with
// real files on disk. Whatever the order, the uploader ends with a readable
// book whose recorded path exists, and no copy is left in the archive with
// nothing pointing at it.
func TestTheArchiveAndAReuploadAgreeOnDisk(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	for _, tc := range []struct {
		name string
		// run performs the release, the move and the re-upload in some order.
		run func(t *testing.T, e *archiveRig)
		// archiveEmpty is whether nothing should be left in the archive.
		archiveEmpty bool
	}{
		{"the upload lands before the move", func(t *testing.T, e *archiveRig) {
			archived := e.release()
			e.uploadAs(e.bob)
			e.handler.keepArchived(t.Context(), archived)
		}, true},
		{"the move lands before the upload", func(t *testing.T, e *archiveRig) {
			archived := e.release()
			e.handler.keepArchived(t.Context(), archived)
			e.uploadAs(e.bob)
		}, true},
		{"the upload is stored before the move and held after it", func(t *testing.T, e *archiveRig) {
			archived := e.release()
			// The upload store finds the bytes already there and writes
			// nothing...
			stored, err := e.files.Store(t.Context(), "again.epub", stringsReader(e.book))
			if err != nil || !stored.Duplicate {
				t.Fatalf("Store = %+v, %v; want a duplicate", stored, err)
			}
			// ...then the archive moves them away...
			e.handler.keepArchived(t.Context(), archived)
			// ...and only then does the holding and its job land, pointing at
			// where the bytes were.
			if _, err := e.pool.HoldUpload(t.Context(), postgres.HoldInput{
				ContentHash: stored.ContentHash, UserID: e.bob, OriginalFilename: "again.epub",
			}); err != nil {
				t.Fatalf("HoldUpload: %v", err)
			}
			target := worker.TargetForUpload(stored.ContentHash, e.bob)
			if _, _, err := e.pool.Enqueue(t.Context(), postgres.EnqueueOptions{
				Type: domain.JobExtract, TargetID: &target, RequestedBy: e.bob,
				Payload: worker.ExtractPayload{
					Path: stored.Path, ContentHash: stored.ContentHash, Format: stored.Format,
					SizeBytes: stored.SizeBytes, OriginalFilename: "again.epub",
					Uploaded: true, OwnerID: e.bob,
				},
			}); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newArchiveRig(t, url)
			e.uploadAs(e.alice)
			drainQueue(t, e.pool, e.ing)

			tc.run(t, e)
			drainQueue(t, e.pool, e.ing)
			e.assertReadableBy(e.bob)
			if tc.archiveEmpty {
				e.assertArchiveEmpty()
			}
		})
	}
}

// A book whose extraction failed because its bytes were missing is not stuck:
// uploading it again is a retry, not a "duplicate".
func TestAFailedBookIsRetriedByUploadingItAgain(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	e := newArchiveRig(t, url)
	e.uploadAs(e.bob)
	drainQueue(t, e.pool, e.ing)

	// The bytes go, and an extraction records the file as failed.
	var path string
	if err := e.pool.QueryRow(t.Context(), `SELECT path FROM book_files`).Scan(&path); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing: %v", err)
	}
	if _, err := e.pool.Exec(t.Context(), `UPDATE book_files SET status = 'failed'`); err != nil {
		t.Fatalf("failing: %v", err)
	}

	out := e.uploadAs(e.bob)
	if out.Files[0].Status != dto.UploadStored || out.Files[0].JobID == "" {
		t.Fatalf("re-upload of a failed book = %+v, want stored and queued", out.Files[0])
	}
	drainQueue(t, e.pool, e.ing)
	e.assertReadableBy(e.bob)
}

type archiveRig struct {
	t          *testing.T
	pool       *postgres.Pool
	ing        *worker.Ingest
	files      *ingest.UploadStore
	handler    *AccountHandler
	rt         *Router
	alice, bob uuid.UUID
	book       string
	archiveDir string
	fileID     uuid.UUID
}

func newArchiveRig(t *testing.T, url string) *archiveRig {
	p := e2ePool(t, url)
	uploads, archive := t.TempDir(), t.TempDir()
	files := &ingest.UploadStore{Root: uploads}
	handler := &AccountHandler{
		Store:   p,
		Archive: ingest.Archive{Root: archive, Managed: uploads},
		Logger:  quietLogger(),
	}
	return &archiveRig{
		t:    t,
		pool: p,
		ing: &worker.Ingest{Store: p, CoverDir: t.TempDir(), ArchiveDir: archive,
			Logger: quietLogger()},
		files:   files,
		handler: handler,
		rt: &Router{Health: &HealthHandler{}, Uploads: &UploadHandler{
			Store: files, Queue: p, Accounts: p, Log: quietLogger(), AnyoneMayUpload: true,
		}},
		alice:      e2eUser(t, p, false),
		bob:        e2eUser(t, p, false),
		book:       minimalEPUB(t, "A Book That Was Let Go"),
		archiveDir: archive,
	}
}

func (e *archiveRig) uploadAs(who uuid.UUID) dto.UploadResult {
	e.t.Helper()
	_, out := uploadAs(e.t, e.rt, who, [][2]string{{"book.epub", e.book}})
	return out
}

// release is alice, the only holder, taking the book off her shelf.
func (e *archiveRig) release() []postgres.ArchivedFile {
	e.t.Helper()
	if err := e.pool.QueryRow(e.t.Context(), `SELECT id FROM book_files`).Scan(&e.fileID); err != nil {
		e.t.Fatalf("reading the file: %v", err)
	}
	archived, err := e.pool.ReleaseHolding(e.t.Context(), e.fileID, e.alice)
	if err != nil || len(archived) != 1 {
		e.t.Fatalf("ReleaseHolding = %+v, %v; want the book archived", archived, err)
	}
	return archived
}

func (e *archiveRig) assertReadableBy(who uuid.UUID) {
	e.t.Helper()
	var status, path string
	var archived bool
	if err := e.pool.QueryRow(e.t.Context(),
		`SELECT status, path, archived_at IS NOT NULL FROM book_files`).Scan(&status, &path, &archived); err != nil {
		e.t.Fatalf("reading: %v", err)
	}
	if status != string(domain.IngestReady) || archived {
		e.t.Fatalf("status %q, archived %v; want a ready book on the shelf", status, archived)
	}
	if _, err := os.Stat(path); err != nil {
		e.t.Fatalf("the recorded path %s does not exist: %v", path, err)
	}
	page, err := e.pool.Library(e.t.Context(), postgres.LibraryQuery{ViewerID: who})
	if err != nil || len(page.Items) != 1 {
		e.t.Fatalf("the uploader's shelf = %+v, %v", page.Items, err)
	}
}

func (e *archiveRig) assertArchiveEmpty() {
	e.t.Helper()
	left, err := os.ReadDir(e.archiveDir)
	if err != nil {
		e.t.Fatalf("reading the archive: %v", err)
	}
	if len(left) != 0 {
		e.t.Errorf("%d unreferenced copies left in the archive", len(left))
	}
}

func stringsReader(s string) io.Reader { return strings.NewReader(s) }
