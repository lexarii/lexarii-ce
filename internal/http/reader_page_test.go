// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// fakeBooks serves one file, with a settable download permission.
type fakeBooks struct {
	file     domain.BookFile
	download bool
	// audited records what was logged, so a test can assert that reading a
	// shared book leaves a trace.
	audited []string
}

func (f *fakeBooks) ReadableBookFile(_ context.Context, id, _ uuid.UUID) (domain.BookFile, error) {
	if id != f.file.ID {
		return domain.BookFile{}, store.ErrNotFound
	}
	return f.file, nil
}

func (f *fakeBooks) ReadableBookWithWork(_ context.Context, id, _ uuid.UUID) (postgres.ReadableBook, error) {
	if id != f.file.ID {
		return postgres.ReadableBook{}, store.ErrNotFound
	}
	return postgres.ReadableBook{File: f.file, WorkID: uuid.New(), Title: "A Book"}, nil
}

func (f *fakeBooks) CanDownload(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return f.download, nil
}

func (f *fakeBooks) RecordAccess(_ context.Context, _, _ uuid.UUID,
	kind postgres.AccessKind, unit int) error {
	f.audited = append(f.audited, fmt.Sprintf("%s:%d", kind, unit))
	return nil
}

// A book shared to be READ must be readable, and must not be handed over.
//
// These two are one test because they are one decision: refusing the file is
// only defensible if the pages are served, and serving the pages is only
// worth anything if the file is refused.
func TestAReadOnlyShareGetsPagesAndNotTheFile(t *testing.T) {
	path := os.Getenv("LEXARII_TEST_PDF")
	if path == "" {
		t.Skip("no LEXARII_TEST_PDF; skipping the page-serving test")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("LEXARII_TEST_PDF is not readable: %v", err)
	}

	id := uuid.New()
	books := &fakeBooks{
		file:     domain.BookFile{ID: id, Path: path, Format: domain.FormatPDF, Status: domain.IngestReady},
		download: false,
	}
	h := &ReaderHandler{Store: books}

	t.Run("the file is refused", func(t *testing.T) {
		req := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/books/"+id.String()+"/file", nil))
		req.SetPathValue("id", id.String())
		rec := httptest.NewRecorder()
		h.File(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if !bytes.Contains(rec.Body.Bytes(), []byte("download_not_permitted")) {
			t.Fatalf("the refusal should say why: %s", rec.Body)
		}
	})

	t.Run("a page is served", func(t *testing.T) {
		req := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/books/"+id.String()+"/page/42", nil))
		req.SetPathValue("id", id.String())
		req.SetPathValue("n", "42")
		rec := httptest.NewRecorder()
		h.Page(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
			t.Fatalf("content type = %q", ct)
		}
		if !bytes.HasPrefix(rec.Body.Bytes(), []byte("%PDF-")) {
			t.Fatal("the response is not a PDF")
		}

		// The point of the exercise: a page, not the book.
		whole, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if int64(rec.Body.Len()) >= whole.Size()/10 {
			t.Fatalf("one page is %d bytes against a %d byte book; that is not a page",
				rec.Body.Len(), whole.Size())
		}
		t.Logf("page 42 served as %d bytes from a %d byte book", rec.Body.Len(), whole.Size())

		// A page served from somebody else's book leaves a trace. That is the
		// whole point of serving it this way rather than handing over the file.
		if len(books.audited) == 0 || books.audited[len(books.audited)-1] != "page:42" {
			t.Fatalf("reading was not recorded: %v", books.audited)
		}
	})

	t.Run("a page outside the book is refused", func(t *testing.T) {
		req := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/books/"+id.String()+"/page/999999", nil))
		req.SetPathValue("id", id.String())
		req.SetPathValue("n", "999999")
		rec := httptest.NewRecorder()
		h.Page(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

// With the download permission the whole file is served as before, so nothing
// about owning a book changed.
func TestDownloadPermissionStillServesTheFile(t *testing.T) {
	path := os.Getenv("LEXARII_TEST_PDF")
	if path == "" {
		t.Skip("no LEXARII_TEST_PDF")
	}

	id := uuid.New()
	h := &ReaderHandler{Store: &fakeBooks{
		file:     domain.BookFile{ID: id, Path: path, Format: domain.FormatPDF, Status: domain.IngestReady},
		download: true,
	}}

	req := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/books/"+id.String()+"/file", nil))
	req.SetPathValue("id", id.String())
	rec := httptest.NewRecorder()
	h.File(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body)
	}
}
