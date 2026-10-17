// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// The whole upload path against a real database and the real worker: two
// people adding the same book, in either order, before or after the first
// one's extraction has finished. The unit tests above check each decision; this
// checks that the decisions add up - that nobody's book is lost between the
// upload, the claim and the job, and that nothing in any reply or any job says
// who else has the book.
func TestTwoPeopleUploadTheSameBook(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}

	for _, tc := range []struct {
		name string
		// aliceFirst decides the order; settled is whether the first upload
		// has been extracted before the second arrives.
		aliceFirst, settled bool
	}{
		{"alice then bob, settled", true, true},
		{"bob then alice, settled", false, true},
		{"alice then bob, still extracting", true, false},
		{"bob then alice, still extracting", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := e2ePool(t, url)
			ctx := t.Context()
			alice, bob := e2eUser(t, p, false), e2eUser(t, p, false)
			first, second := alice, bob
			if !tc.aliceFirst {
				first, second = bob, alice
			}

			rt := &Router{
				Health: &HealthHandler{},
				Uploads: &UploadHandler{
					Store: &ingest.UploadStore{Root: t.TempDir()},
					Queue: p, Accounts: p, Log: quietLogger(),
					AnyoneMayUpload: true,
				},
				Jobs:     &JobHandler{Queue: p, Accounts: p},
				Library:  &LibraryHandler{Store: p},
				Sharing:  &SharingHandler{Store: p},
				Accounts: &AccountHandler{Store: p, Logger: quietLogger()},
			}
			ing := &worker.Ingest{Store: p, CoverDir: t.TempDir(), Logger: quietLogger()}
			book := minimalEPUB(t, "A Book Two People Bought")

			_, r1 := uploadAs(t, rt, first, [][2]string{{"first-name.epub", book}})
			if tc.settled {
				drainQueue(t, p, ing)
			}
			_, r2 := uploadAs(t, rt, second, [][2]string{{"second-name.epub", book}})
			drainQueue(t, p, ing)

			a, b := r1.Files[0], r2.Files[0]
			if a.Status != dto.UploadStored || b.Status != dto.UploadStored {
				t.Fatalf("statuses = %q, %q; want both stored", a.Status, b.Status)
			}
			// The second reply says nothing the first did not: no note, a job
			// of its own, the uploader's own filename.
			if b.Note != "" || b.Error != "" {
				t.Errorf("the second upload was told something: note %q, error %q", b.Note, b.Error)
			}
			if a.JobID == "" || b.JobID == "" || a.JobID == b.JobID {
				t.Fatalf("job ids %q and %q; want one each", a.JobID, b.JobID)
			}
			if b.Filename != "second-name.epub" || b.ContentHash != a.ContentHash ||
				b.Format != a.Format || b.SizeBytes != a.SizeBytes {
				t.Errorf("replies differ beyond the filename and job: %+v vs %+v", a, b)
			}

			// Each job is its uploader's to follow, and nobody else's.
			for _, c := range []struct {
				job, viewer string
				who         uuid.UUID
				want        int
			}{
				{"first", "first", first, http.StatusOK},
				{"first", "second", second, http.StatusNotFound},
				{"second", "second", second, http.StatusOK},
				{"second", "first", first, http.StatusNotFound},
			} {
				id := a.JobID
				if c.job == "second" {
					id = b.JobID
				}
				rec := httptest.NewRecorder()
				rt.Handler().ServeHTTP(rec, asUserID(httptest.NewRequest(http.MethodGet, "/api/v1/jobs/"+id, nil), c.who))
				if rec.Code != c.want {
					t.Errorf("the %s person reading the %s job: status %d, want %d",
						c.viewer, c.job, rec.Code, c.want)
				}
			}

			// Both have the book, each under their own name.
			for who, name := range map[uuid.UUID]string{first: "first-name.epub", second: "second-name.epub"} {
				page, err := p.Library(ctx, postgres.LibraryQuery{ViewerID: who, Owned: postgres.OwnedMine})
				if err != nil {
					t.Fatalf("Library: %v", err)
				}
				if len(page.Items) != 1 {
					t.Fatalf("a holder's shelf has %d books, want 1", len(page.Items))
				}
				if page.Items[0].LentBy != "" {
					t.Errorf("a holder's own book is marked lent by %q", page.Items[0].LentBy)
				}
				d, err := p.Work(ctx, page.Items[0].WorkID, who)
				if err != nil {
					t.Fatalf("Work: %v", err)
				}
				if len(d.Filenames) != 1 || d.Filenames[0] != name {
					t.Errorf("filenames = %v, want %q", d.Filenames, name)
				}
			}

			// One file, however many people hold it.
			var files int
			if err := p.QueryRow(ctx, `SELECT count(*) FROM book_files`).Scan(&files); err != nil {
				t.Fatalf("counting: %v", err)
			}
			if files != 1 {
				t.Errorf("%d book files for one set of bytes", files)
			}

			// Coming back with it again is a duplicate - for the person who
			// already has it, which tells them only about their own shelf.
			_, r3 := uploadAs(t, rt, second, [][2]string{{"again.epub", book}})
			if r3.Files[0].Status != dto.UploadDuplicate || r3.Files[0].JobID != "" {
				t.Errorf("a repeat upload = %+v, want a duplicate with no job", r3.Files[0])
			}

			// Each takes it off their own shelf in turn. The first leaves the
			// book with the other; the second leaves it in the archive. Both
			// replies are the same empty 204, so neither learns which it was.
			var fileID string
			if err := p.QueryRow(ctx, `SELECT id::text FROM book_files`).Scan(&fileID); err != nil {
				t.Fatalf("reading the file: %v", err)
			}
			release := func(who uuid.UUID) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				rt.Handler().ServeHTTP(rec, asUserID(httptest.NewRequest(http.MethodDelete,
					"/api/v1/books/"+fileID+"/holding", nil), who))
				return rec
			}
			if rec := release(first); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("first release: %d %s", rec.Code, rec.Body)
			}
			if page, _ := p.Library(ctx, postgres.LibraryQuery{ViewerID: second}); len(page.Items) != 1 {
				t.Fatal("the other holder lost the book when the first let it go")
			}
			if rec := release(first); rec.Code != http.StatusNotFound {
				t.Errorf("releasing again: %d, want 404", rec.Code)
			}
			if rec := release(second); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("second release: %d %s", rec.Code, rec.Body)
			}
			var archived bool
			if err := p.QueryRow(ctx, `SELECT archived_at IS NOT NULL FROM book_files`).Scan(&archived); err != nil {
				t.Fatalf("reading: %v", err)
			}
			if !archived {
				t.Error("the last holder let the book go and it was not archived")
			}
		})
	}
}

func e2ePool(t *testing.T, url string) *postgres.Pool {
	t.Helper()
	pool, err := postgres.Open(t.Context(), url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(pool.Close)

	// The same session lock every database test package takes, so this does
	// not truncate tables under a test running in another package.
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquiring the test lock: %v", err)
	}
	const testDBLockKey = 0x5348454c464d4b01
	if _, err := conn.Exec(context.Background(), "SELECT pg_advisory_lock($1)", int64(testDBLockKey)); err != nil {
		conn.Release()
		t.Fatalf("taking the test lock: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", int64(testDBLockKey))
		conn.Release()
	})

	for _, stmt := range []string{
		"DELETE FROM jobs", "DELETE FROM holding_claims", "DELETE FROM released_holdings", "DELETE FROM blocked_content", "DELETE FROM works",
		"DELETE FROM authors", "DELETE FROM tags",
	} {
		if _, err := pool.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("cleaning (%s): %v", stmt, err)
		}
	}
	return pool
}

func e2eUser(t *testing.T, p *postgres.Pool, admin bool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := p.QueryRow(t.Context(),
		`INSERT INTO users (username, is_admin) VALUES ($1, $2) RETURNING id`,
		"holder-"+uuid.NewString()[:8], admin).Scan(&id); err != nil {
		t.Fatalf("creating a user: %v", err)
	}
	return id
}

// drainQueue runs the worker until nothing is pending or running.
func drainQueue(t *testing.T, p *postgres.Pool, ing *worker.Ingest) {
	t.Helper()
	pool := worker.New(p, ing.Handlers(), worker.Config{
		Workers: 2, PollInterval: 5 * time.Millisecond, ReclaimInterval: time.Hour,
		Logger: quietLogger(),
	})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- pool.Run(ctx) }()

	idle := 0
	for idle < 3 {
		depth, err := p.QueueDepth(t.Context())
		if err != nil {
			t.Fatalf("QueueDepth: %v", err)
		}
		total := 0
		for _, n := range depth {
			total += n
		}
		if total == 0 {
			idle++
		} else {
			idle = 0
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out with %d jobs queued", total)
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("worker: %v", err)
	}
}

// minimalEPUB is a valid two-chapter EPUB, as a string for a multipart part.
func minimalEPUB(t *testing.T, title string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range []struct{ name, body string }{
		{"mimetype", "application/epub+zip"},
		{"META-INF/container.xml", `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`},
		{"OEBPS/content.opf", `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>` + title + `</dc:title><dc:creator>An Author</dc:creator><dc:language>en</dc:language>
  </metadata>
  <manifest>
    <item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="c2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="c1"/><itemref idref="c2"/></spine>
</package>`},
		{"OEBPS/ch1.xhtml", "<html><body><p>Chapter one.</p></body></html>"},
		{"OEBPS/ch2.xhtml", "<html><body><p>Chapter two.</p></body></html>"},
	} {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatalf("creating %s: %v", e.name, err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatalf("writing %s: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	return buf.String()
}
