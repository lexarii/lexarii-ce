// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// A library of 168 books where every card is the same typographic rectangle is
// a library nobody recognises anything in. Covers fix that, and for a
// PDF-shaped library the cover is the first page.
//
// Rendering a PDF page needs a rasteriser. Pure Go has none worth using, and
// the real ones need cgo - which CGO_ENABLED=0 rules out, and which is why the
// model link is HTTP rather than bindings. So the page is rendered where a
// rasteriser already exists and is already open: pdf.js, in the reader's own
// browser. It downscales the result and sends it here, once per book.
//
// That makes this an endpoint that accepts an image from a client, so it is
// treated as untrusted input: the bytes are sniffed rather than believed, the
// size is bounded, and the stored name is generated rather than taken from
// anything the client said.

// maxCoverBytes bounds an uploaded cover.
//
// A downscaled page at around 400px wide is tens of kilobytes. One megabyte is
// far more than that and far less than anything worth worrying about.
const maxCoverBytes = 1 << 20

// CoverStore is what the cover handlers need.
type CoverStore interface {
	SetCover(ctx context.Context, bookFileID, viewer uuid.UUID, path string) (string, error)
	Cover(ctx context.Context, bookFileID, viewer uuid.UUID) (string, error)
	FilesNeedingCovers(ctx context.Context, viewer uuid.UUID, limit int) ([]postgres.CoverTarget, error)
	CountFilesNeedingCovers(ctx context.Context, viewer uuid.UUID) (int, error)
}

// CoverHandler stores and serves book covers.
type CoverHandler struct {
	Store CoverStore
	// Dir is where cover images are written: under the DATA directory, never
	// the library directory, which is read-only and holds somebody's books.
	Dir string
}

// Pending serves GET /api/v1/covers/pending.
//
// The files this person can see that still have no cover. The client works
// through them in the background and uploads what it renders.
func (h *CoverHandler) Pending(w http.ResponseWriter, r *http.Request) {
	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	targets, err := h.Store.FilesNeedingCovers(r.Context(), viewer, 24)
	if err != nil {
		respondError(w, err)
		return
	}

	remaining, err := h.Store.CountFilesNeedingCovers(r.Context(), viewer)
	if err != nil {
		respondError(w, err)
		return
	}

	out := dto.CoverTargets{
		Items:     make([]dto.CoverTarget, 0, len(targets)),
		Remaining: remaining,
	}
	for _, t := range targets {
		out.Items = append(out.Items, dto.CoverTarget{
			BookFileID: t.BookFileID.String(),
			Format:     t.Format,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// Put serves PUT /api/v1/books/{id}/cover.
func (h *CoverHandler) Put(w http.ResponseWriter, r *http.Request) {
	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCoverBytes))
	if err != nil {
		respondError(w, Invalid("the cover is too large or could not be read", err))
		return
	}

	// The content type is sniffed from the bytes rather than trusted from the
	// header: this endpoint writes a file to disk that is later served back,
	// and a client claiming image/webp while sending something else is exactly
	// the case that matters.
	kind := imageKind(body)
	if kind == "" {
		respondError(w, Invalid("a cover must be a PNG, JPEG, or WebP image", nil))
		return
	}

	if err := os.MkdirAll(h.Dir, 0o755); err != nil {
		respondError(w, err)
		return
	}

	// A generated name, not the book's id: a cover replaced later must not be
	// served from a browser cache under the same URL, and nothing about the
	// stored name should come from the client.
	name, err := coverName(kind)
	if err != nil {
		respondError(w, err)
		return
	}
	full := filepath.Join(h.Dir, name)

	// Written to a temporary file and renamed, so a request that dies halfway
	// leaves no half-written image to be served as a cover.
	tmp, err := os.CreateTemp(h.Dir, ".cover-*")
	if err != nil {
		respondError(w, err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		respondError(w, err)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		respondError(w, err)
		return
	}
	if err := os.Rename(tmpName, full); err != nil {
		_ = os.Remove(tmpName)
		respondError(w, err)
		return
	}

	previous, err := h.Store.SetCover(r.Context(), id, viewer, name)
	if err != nil {
		_ = os.Remove(full)
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("book file"))
			return
		}
		respondError(w, err)
		return
	}

	// The cover this one replaced. Nothing references it any more, and a
	// re-rendered library would otherwise leave one orphan per book per pass.
	if previous != "" {
		if base := filepath.Base(previous); base == previous {
			_ = os.Remove(filepath.Join(h.Dir, base))
		}
	}
	respondJSON(w, http.StatusNoContent, nil)
}

// Get serves GET /api/v1/books/{id}/cover.
func (h *CoverHandler) Get(w http.ResponseWriter, r *http.Request) {
	viewer, _ := UserFrom(r.Context())
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}

	name, err := h.Store.Cover(r.Context(), id, viewer)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("cover"))
			return
		}
		respondError(w, err)
		return
	}

	// The stored name was generated by coverName and never came from a client,
	// but joining a database value into a path deserves the check anyway: one
	// bad row should not become an arbitrary file read.
	clean := filepath.Base(name)
	if clean != name || clean == "." || clean == ".." {
		respondError(w, NotFound("cover"))
		return
	}

	f, err := os.Open(filepath.Join(h.Dir, clean))
	if err != nil {
		respondError(w, NotFound("cover"))
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		respondError(w, err)
		return
	}

	w.Header().Set("Content-Type", contentTypeFor(clean))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Private and long-lived: the name changes when the cover does, and a
	// shared book must not sit in a proxy cache where the next person is served
	// it without an access check.
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeContent(w, r, clean, info.ModTime(), f)
}

// imageKind reports the image format of some bytes, or "" if it is not an image
// this accepts. The magic numbers are checked rather than the Content-Type,
// which a client controls.
func imageKind(b []byte) string {
	switch {
	case len(b) > 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "png"
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpg"
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "webp"
	default:
		return ""
	}
}

func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".webp"):
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

func coverName(kind string) (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]) + "." + kind, nil
}
