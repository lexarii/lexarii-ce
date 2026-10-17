// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// coverStoreFake answers each cover question separately, and records who
// asked.
type coverStoreFake struct {
	name     string
	coverErr error
	setErr   error

	targets    []postgres.CoverTarget
	targetsErr error
	remaining  int
	countErr   error

	viewers []uuid.UUID
	limit   int
}

func (f *coverStoreFake) SetCover(_ context.Context, _, viewer uuid.UUID, _ string) (string, error) {
	f.viewers = append(f.viewers, viewer)
	return "", f.setErr
}

func (f *coverStoreFake) Cover(_ context.Context, _, viewer uuid.UUID) (string, error) {
	f.viewers = append(f.viewers, viewer)
	return f.name, f.coverErr
}

func (f *coverStoreFake) FilesNeedingCovers(_ context.Context, viewer uuid.UUID, limit int) ([]postgres.CoverTarget, error) {
	f.viewers = append(f.viewers, viewer)
	f.limit = limit
	return f.targets, f.targetsErr
}

func (f *coverStoreFake) CountFilesNeedingCovers(_ context.Context, viewer uuid.UUID) (int, error) {
	f.viewers = append(f.viewers, viewer)
	return f.remaining, f.countErr
}

func coverRouter(f *coverStoreFake, dir string) *Router {
	return &Router{Covers: &CoverHandler{Store: f, Dir: dir}}
}

func TestPendingCovers(t *testing.T) {
	user := uuid.New()
	target := postgres.CoverTarget{BookFileID: uuid.New(), Format: "pdf"}

	t.Run("the next batch and how many remain, for this viewer", func(t *testing.T) {
		f := &coverStoreFake{targets: []postgres.CoverTarget{target}, remaining: 40}
		rec := serveAsViewer(t, coverRouter(f, t.TempDir()), user, http.MethodGet, "/api/v1/covers/pending", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		out := decodeInto[dto.CoverTargets](t, rec)
		if out.Remaining != 40 || len(out.Items) != 1 || out.Items[0].BookFileID != target.BookFileID.String() || out.Items[0].Format != "pdf" {
			t.Errorf("pending = %+v", out)
		}
		if f.limit != 24 {
			t.Errorf("batch size = %d, want 24", f.limit)
		}
		for _, v := range f.viewers {
			if v != user {
				t.Fatalf("the store was asked as %s, want the viewer", v)
			}
		}
	})

	for _, tc := range []struct {
		name   string
		f      *coverStoreFake
		viewer uuid.UUID
		status int
	}{
		{"nobody signed in", &coverStoreFake{}, uuid.Nil, http.StatusUnauthorized},
		{"listing fails", &coverStoreFake{targetsErr: errors.New("boom")}, user, http.StatusInternalServerError},
		{"counting fails", &coverStoreFake{countErr: errors.New("boom")}, user, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveAsViewer(t, coverRouter(tc.f, t.TempDir()), tc.viewer, http.MethodGet, "/api/v1/covers/pending", "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
}

func TestServingACover(t *testing.T) {
	user := uuid.New()
	dir := t.TempDir()
	for name, body := range map[string][]byte{
		"0123456789abcdef0123456789abcdef.png":  pngBytes,
		"0123456789abcdef0123456789abcdef.webp": webpBytes,
		"0123456789abcdef0123456789abcdef.jpg":  jpegBytes,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	for _, tc := range []struct {
		name, contentType string
		body              []byte
	}{
		{"0123456789abcdef0123456789abcdef.png", "image/png", pngBytes},
		{"0123456789abcdef0123456789abcdef.webp", "image/webp", webpBytes},
		{"0123456789abcdef0123456789abcdef.jpg", "image/jpeg", jpegBytes},
	} {
		t.Run(tc.contentType, func(t *testing.T) {
			f := &coverStoreFake{name: tc.name}
			rec := serveAsViewer(t, coverRouter(f, dir), user, http.MethodGet, "/api/v1/books/"+uuid.NewString()+"/cover", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
			}
			if !bytes.Equal(rec.Body.Bytes(), tc.body) {
				t.Error("the cover bytes were not served as stored")
			}
			for header, value := range map[string]string{
				"Content-Type":           tc.contentType,
				"X-Content-Type-Options": "nosniff",
				"Cache-Control":          "private, max-age=604800",
			} {
				if got := rec.Header().Get(header); got != value {
					t.Errorf("%s = %q, want %q", header, got, value)
				}
			}
			if len(f.viewers) != 1 || f.viewers[0] != user {
				t.Errorf("the store was asked as %v, want the viewer", f.viewers)
			}
		})
	}

	for _, tc := range []struct {
		name   string
		target string
		f      *coverStoreFake
		status int
	}{
		{"a malformed id", "/api/v1/books/cover-me/cover", &coverStoreFake{}, http.StatusBadRequest},
		{"a book with no cover, or not this viewer's", "", &coverStoreFake{coverErr: store.ErrNotFound}, http.StatusNotFound},
		{"a recorded cover gone from disk", "", &coverStoreFake{name: "ffffffffffffffffffffffffffffffff.png"}, http.StatusNotFound},
		{"a recorded name that is a directory reference", "", &coverStoreFake{name: ".."}, http.StatusNotFound},
		{"a store failure", "", &coverStoreFake{coverErr: errors.New("boom")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.target
			if target == "" {
				target = "/api/v1/books/" + uuid.NewString() + "/cover"
			}
			rec := serveAsViewer(t, coverRouter(tc.f, dir), user, http.MethodGet, target, "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
		})
	}
}

func TestStoringACoverFailsCleanly(t *testing.T) {
	user := uuid.New()
	put := func(t *testing.T, h *CoverHandler, id string, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		req := asUserID(httptest.NewRequest(http.MethodPut, "/api/v1/books/"+id+"/cover", bytes.NewReader(body)), user)
		rec := httptest.NewRecorder()
		(&Router{Covers: h}).Handler().ServeHTTP(rec, req)
		return rec
	}

	t.Run("a malformed id", func(t *testing.T) {
		rec := put(t, &CoverHandler{Store: &coverStoreFake{}, Dir: t.TempDir()}, "nope", pngBytes)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("an image over the size limit", func(t *testing.T) {
		dir := t.TempDir()
		f := &coverStoreFake{}
		big := append(append([]byte{}, pngBytes...), make([]byte, maxCoverBytes)...)
		rec := put(t, &CoverHandler{Store: f, Dir: dir}, uuid.NewString(), big)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 || len(f.viewers) != 0 {
			t.Errorf("an oversized cover was written (%d files) or recorded (%d calls)", len(entries), len(f.viewers))
		}
	})

	t.Run("a cover directory that cannot be made", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "a-file")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatalf("writing: %v", err)
		}
		rec := put(t, &CoverHandler{Store: &coverStoreFake{}, Dir: filepath.Join(blocker, "covers")}, uuid.NewString(), pngBytes)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("a cover directory that cannot be written", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("permissions do not bind root")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		f := &coverStoreFake{}
		rec := put(t, &CoverHandler{Store: f, Dir: dir}, uuid.NewString(), pngBytes)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if len(f.viewers) != 0 {
			t.Error("a cover that was never written was recorded")
		}
	})

	t.Run("a store failure leaves nothing on disk", func(t *testing.T) {
		dir := t.TempDir()
		rec := put(t, &CoverHandler{Store: &coverStoreFake{setErr: errors.New("boom")}, Dir: dir}, uuid.NewString(), pngBytes)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("%d files left behind by a failed record", len(entries))
		}
	})
}
