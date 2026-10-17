// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

type fakeCovers struct {
	path string
	err  error
	// previous is what SetCover reports the new cover replaced.
	previous string
	// remaining is how many books the store says still have no cover.
	remaining int

	stored string
}

func (f *fakeCovers) SetCover(_ context.Context, _, _ uuid.UUID, path string) (string, error) {
	f.stored = path
	return f.previous, f.err
}

func (f *fakeCovers) Cover(context.Context, uuid.UUID, uuid.UUID) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.path, nil
}

func (f *fakeCovers) FilesNeedingCovers(context.Context, uuid.UUID, int) ([]postgres.CoverTarget, error) {
	return nil, f.err
}

func (f *fakeCovers) CountFilesNeedingCovers(context.Context, uuid.UUID) (int, error) {
	return f.remaining, f.err
}

// A one-pixel PNG, and the smallest thing that is honestly a JPEG or a WebP.
var (
	pngBytes  = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	jpegBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 32)...)
	webpBytes = append([]byte("RIFF\x00\x00\x00\x00WEBP"), make([]byte, 32)...)
)

func putCover(t *testing.T, h *CoverHandler, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	id := uuid.New()
	r := asUser(httptest.NewRequest(http.MethodPut, "/x", bytes.NewReader(body)))
	r.SetPathValue("id", id.String())
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.Put(rec, r)
	return rec
}

// The bytes decide what this is, not the header. This endpoint writes a
// client-supplied file to disk and serves it back later, so a request claiming
// image/webp while sending something else is the case that matters.
func TestCoverUploadSniffsTheBytes(t *testing.T) {
	dir := t.TempDir()

	for _, tt := range []struct {
		name        string
		body        []byte
		contentType string
		want        int
	}{
		{"png", pngBytes, "image/png", http.StatusNoContent},
		{"jpeg", jpegBytes, "image/jpeg", http.StatusNoContent},
		{"webp", webpBytes, "image/webp", http.StatusNoContent},

		// Each of these claims to be an image and is not.
		{"html claiming to be png", []byte("<html><script>alert(1)</script>"), "image/png", http.StatusBadRequest},
		{"svg claiming to be png", []byte("<svg xmlns='http://www.w3.org/2000/svg'>"), "image/png", http.StatusBadRequest},
		{"empty", nil, "image/webp", http.StatusBadRequest},
		{"text", []byte("not an image at all"), "image/jpeg", http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := &CoverHandler{Store: &fakeCovers{}, Dir: dir}
			rec := putCover(t, h, tt.body, tt.contentType)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// The stored name is generated here and never comes from the client.
func TestCoverNameIsGeneratedNotGiven(t *testing.T) {
	dir := t.TempDir()
	fake := &fakeCovers{}
	h := &CoverHandler{Store: fake, Dir: dir}

	if rec := putCover(t, h, pngBytes, "image/png"); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.HasSuffix(fake.stored, ".png") || len(fake.stored) < 20 {
		t.Fatalf("stored name looks guessable or wrong: %q", fake.stored)
	}
	if strings.ContainsAny(fake.stored, "/\\") {
		t.Fatalf("stored name contains a path separator: %q", fake.stored)
	}

	// A second upload gets a different name, so a replaced cover is never
	// served from a browser cache under the old URL.
	first := fake.stored
	if rec := putCover(t, h, pngBytes, "image/png"); rec.Code != http.StatusNoContent {
		t.Fatalf("second upload: %d", rec.Code)
	}
	if fake.stored == first {
		t.Fatal("two covers were given the same name")
	}
}

// A failed database write must not leave the image behind: an orphan file that
// nothing references is never cleaned up by anything.
func TestCoverFileIsRemovedWhenTheRecordFails(t *testing.T) {
	dir := t.TempDir()
	h := &CoverHandler{Store: &fakeCovers{err: store.ErrNotFound}, Dir: dir}

	if rec := putCover(t, h, pngBytes, "image/png"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the cover directory: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("an orphaned cover was left on disk: %v", names)
	}
}

// A stored path is a bare filename. Even though this one is generated, joining
// a database value into a path deserves the check: one bad row must not become
// an arbitrary file read.
func TestCoverPathTraversalIsRefused(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "..", "secret.txt")
	if err := os.WriteFile(secret, []byte("not a cover"), 0o600); err != nil {
		t.Fatalf("writing the decoy: %v", err)
	}

	h := &CoverHandler{Store: &fakeCovers{path: "../secret.txt"}, Dir: dir}
	r := asUser(httptest.NewRequest(http.MethodGet, "/x", nil))
	r.SetPathValue("id", uuid.NewString())
	rec := httptest.NewRecorder()
	h.Get(rec, r)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "not a cover") {
		t.Fatal("a file outside the cover directory was served")
	}
}

func TestCoverUploadRequiresSignIn(t *testing.T) {
	h := &CoverHandler{Store: &fakeCovers{}, Dir: t.TempDir()}
	r := httptest.NewRequest(http.MethodPut, "/x", bytes.NewReader(pngBytes))
	r.SetPathValue("id", uuid.NewString())
	rec := httptest.NewRecorder()
	h.Put(rec, r)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// A replaced cover takes its predecessor with it. Every cover gets a fresh
// generated name, so without this a library re-rendered once a year would leave
// one orphan per book per pass, and nothing would ever clean them up.
func TestReplacingACoverRemovesTheOldFile(t *testing.T) {
	dir := t.TempDir()
	old := "0123456789abcdef0123456789abcdef.webp"
	if err := os.WriteFile(filepath.Join(dir, old), []byte("an old cover"), 0o600); err != nil {
		t.Fatalf("writing the old cover: %v", err)
	}

	h := &CoverHandler{Store: &fakeCovers{previous: old}, Dir: dir}
	if rec := putCover(t, h, pngBytes, "image/png"); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	if _, err := os.Stat(filepath.Join(dir, old)); !os.IsNotExist(err) {
		t.Fatalf("the replaced cover is still on disk: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the cover directory: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%d files left, want just the new cover", len(entries))
	}
}

// A previous name that is not a bare filename is left alone rather than used to
// delete something outside the cover directory.
func TestReplacingDoesNotDeleteOutsideTheCoverDirectory(t *testing.T) {
	dir := t.TempDir()
	decoy := filepath.Join(dir, "..", "keep-me.txt")
	if err := os.WriteFile(decoy, []byte("not a cover"), 0o600); err != nil {
		t.Fatalf("writing the decoy: %v", err)
	}

	h := &CoverHandler{Store: &fakeCovers{previous: "../keep-me.txt"}, Dir: dir}
	if rec := putCover(t, h, pngBytes, "image/png"); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}

	if _, err := os.Stat(decoy); err != nil {
		t.Fatalf("a file outside the cover directory was deleted: %v", err)
	}
}
