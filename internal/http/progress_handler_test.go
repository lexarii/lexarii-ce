// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// progressStoreFake records what it was asked and answers as configured.
type progressStoreFake struct {
	saved     domain.ReadingProgress
	saveCalls int
	// stored is what SaveProgress answers with; zero means echo the input.
	stored  *domain.ReadingProgress
	saveErr error

	read    domain.ReadingProgress
	readErr error
	asked   [2]uuid.UUID // user, book

	continueUser  uuid.UUID
	continueLimit int
	items         []postgres.ContinueItem
	continueErr   error
}

func (f *progressStoreFake) SaveProgress(_ context.Context, p domain.ReadingProgress) (domain.ReadingProgress, error) {
	f.saved = p
	f.saveCalls++
	if f.saveErr != nil {
		return domain.ReadingProgress{}, f.saveErr
	}
	if f.stored != nil {
		return *f.stored, nil
	}
	return p, nil
}

func (f *progressStoreFake) ReadingProgress(_ context.Context, userID, bookFileID uuid.UUID) (domain.ReadingProgress, error) {
	f.asked = [2]uuid.UUID{userID, bookFileID}
	return f.read, f.readErr
}

func (f *progressStoreFake) ContinueReading(_ context.Context, userID uuid.UUID, limit int) ([]postgres.ContinueItem, error) {
	f.continueUser, f.continueLimit = userID, limit
	return f.items, f.continueErr
}

func progressRouter(f *progressStoreFake) *Router {
	return &Router{Progress: &ProgressHandler{Store: f}}
}

func TestProgressNeedsSomebodySignedIn(t *testing.T) {
	fileID := uuid.NewString()
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/v1/books/" + fileID + "/progress", ""},
		{http.MethodPut, "/api/v1/books/" + fileID + "/progress", `{"locator":"pdf:2:0"}`},
		{http.MethodGet, "/api/v1/continue", ""},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			f := &progressStoreFake{}
			rec := serveAsViewer(t, progressRouter(f), uuid.Nil, tc.method, tc.target, tc.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if f.saveCalls != 0 || f.asked != [2]uuid.UUID{} || f.continueUser != uuid.Nil {
				t.Error("an anonymous request reached the store")
			}
		})
	}
}

func TestReadingProgressOfABookFile(t *testing.T) {
	user, fileID := uuid.New(), uuid.New()
	target := "/api/v1/books/" + fileID.String() + "/progress"

	t.Run("a book never opened is not started, not missing", func(t *testing.T) {
		f := &progressStoreFake{readErr: fmt.Errorf("reading: %w", store.ErrNotFound)}
		rec := serveAsViewer(t, progressRouter(f), user, http.MethodGet, target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		out := decodeInto[dto.ReadingProgress](t, rec)
		if out.Started || out.BookFileID != fileID.String() || out.Locator != "" {
			t.Errorf("progress = %+v, want an unstarted book", out)
		}
		if f.asked != [2]uuid.UUID{user, fileID} {
			t.Errorf("asked for %v", f.asked)
		}
	})

	t.Run("a position with the furthest point reached", func(t *testing.T) {
		pct, far := float32(0.25), float32(0.6)
		at := time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC)
		f := &progressStoreFake{read: domain.ReadingProgress{
			BookFileID: fileID, Position: domain.NewEPUBLocator(3, "section[1]/p[2]", 7), Percent: &pct,
			Furthest: domain.NewEPUBLocator(9, "section[1]/p[1]", 0), FurthestPercent: &far, LastReadAt: at,
		}}
		rec := serveAsViewer(t, progressRouter(f), user, http.MethodGet, target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		out := decodeInto[dto.ReadingProgress](t, rec)
		if !out.Started || out.Locator != "epub:3/section[1]/p[2]:7" || out.FurthestLocator != "epub:9/section[1]/p[1]:0" ||
			out.Percent == nil || *out.Percent != pct || out.FurthestPercent == nil || *out.FurthestPercent != far ||
			out.LastReadAt == nil || !out.LastReadAt.Equal(at) {
			t.Errorf("progress = %+v", out)
		}
	})

	t.Run("a malformed book id", func(t *testing.T) {
		rec := serveAsViewer(t, progressRouter(&progressStoreFake{}), user, http.MethodGet, "/api/v1/books/x/progress", "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("a store failure", func(t *testing.T) {
		rec := serveAsViewer(t, progressRouter(&progressStoreFake{readErr: errors.New("boom")}), user, http.MethodGet, target, "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestSavingProgress(t *testing.T) {
	user, fileID := uuid.New(), uuid.New()
	target := "/api/v1/books/" + fileID.String() + "/progress"

	t.Run("the stored position is what comes back", func(t *testing.T) {
		// Another device read further; the response carries the server's
		// furthest, not the position just sent.
		far := domain.NewPDFLocator(40, 0)
		f := &progressStoreFake{}
		f.stored = &domain.ReadingProgress{BookFileID: fileID, Position: domain.NewPDFLocator(12, 3), Furthest: far}
		rec := serveAsViewer(t, progressRouter(f), user, http.MethodPut, target, `{"locator":"pdf:12:3","percent":0.1}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
		}
		if f.saved.UserID != user || f.saved.BookFileID != fileID || f.saved.Position.String() != "pdf:12:3" ||
			f.saved.Percent == nil || *f.saved.Percent != 0.1 {
			t.Errorf("the store received %+v", f.saved)
		}
		if out := decodeInto[dto.ReadingProgress](t, rec); out.FurthestLocator != "pdf:40:0" || out.Locator != "pdf:12:3" {
			t.Errorf("response = %+v", out)
		}
	})

	for _, tc := range []struct {
		name, target, body string
	}{
		{"a malformed book id", "/api/v1/books/x/progress", `{"locator":"pdf:1:0"}`},
		{"a body that is not JSON", target, `locator`},
		{"a percentage instead of a locator", target, `{"locator":"0.42"}`},
		{"a page that is not one-based", target, `{"locator":"pdf:0:0"}`},
		{"a body over the limit", target, `{"locator":"pdf:1:0","x":"` + strings.Repeat("y", 9<<10) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &progressStoreFake{}
			rec := serveAsViewer(t, progressRouter(f), user, http.MethodPut, tc.target, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
			}
			if f.saveCalls != 0 {
				t.Error("an invalid position reached the store")
			}
		})
	}

	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"a locator the store rejects", fmt.Errorf("saving: %w", domain.ErrInvalidLocator), http.StatusBadRequest},
		{"a file the reader cannot open", fmt.Errorf("saving: %w", store.ErrNotFound), http.StatusNotFound},
		{"a store failure", errors.New("boom"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &progressStoreFake{saveErr: tc.err}
			rec := serveAsViewer(t, progressRouter(f), user, http.MethodPut, target, `{"locator":"pdf:1:0"}`)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
}

func TestContinueReading(t *testing.T) {
	user := uuid.New()
	pct := float32(0.5)
	at := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	item := postgres.ContinueItem{WorkID: uuid.New(), Title: "Concrete Mathematics", BookFileID: uuid.New(),
		Format: domain.FormatPDF, Percent: &pct, LastReadAt: at}

	f := &progressStoreFake{items: []postgres.ContinueItem{item}}
	rec := serveAsViewer(t, progressRouter(f), user, http.MethodGet, "/api/v1/continue", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if f.continueUser != user || f.continueLimit != 12 {
		t.Errorf("asked for %s with limit %d", f.continueUser, f.continueLimit)
	}
	out := decodeInto[dto.ContinueList](t, rec)
	if len(out.Items) != 1 {
		t.Fatalf("items = %+v", out.Items)
	}
	got := out.Items[0]
	if got.WorkID != item.WorkID.String() || got.BookFileID != item.BookFileID.String() || got.Title != item.Title ||
		got.Format != "pdf" || got.Percent == nil || *got.Percent != pct || !got.LastReadAt.Equal(at) {
		t.Errorf("item = %+v", got)
	}

	rec = serveAsViewer(t, progressRouter(&progressStoreFake{}), user, http.MethodGet, "/api/v1/continue", "")
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("nothing to continue should be an empty list: %s", rec.Body)
	}

	rec = serveAsViewer(t, progressRouter(&progressStoreFake{continueErr: errors.New("boom")}), user, http.MethodGet, "/api/v1/continue", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure: status = %d, want 500", rec.Code)
	}
}
