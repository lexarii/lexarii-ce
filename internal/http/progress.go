// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// ProgressStore reads and writes reading positions.
type ProgressStore interface {
	SaveProgress(ctx context.Context, progress domain.ReadingProgress) (domain.ReadingProgress, error)
	ReadingProgress(ctx context.Context, userID, bookFileID uuid.UUID) (domain.ReadingProgress, error)
	ContinueReading(ctx context.Context, userID uuid.UUID, limit int) ([]postgres.ContinueItem, error)
}

// ProgressHandler serves reading positions.
type ProgressHandler struct {
	Store ProgressStore
}

// userOrUnauthorised resolves the signed-in user, or writes a 401.
//
// This replaced a single hardcoded UserID field. Keeping that as one field
// rather than threading a fixed id through every handler is what made adding
// real authentication a small change rather than an audit.
func userOrUnauthorised(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, ok := UserFrom(r.Context())
	if !ok {
		respondError(w, &httpError{
			status:  http.StatusUnauthorized,
			code:    "not_signed_in",
			message: "sign in to continue",
		})
		return uuid.Nil, false
	}
	return id, true
}

// Get serves GET /api/v1/books/{id}/progress.
func (h *ProgressHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}

	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	progress, err := h.Store.ReadingProgress(r.Context(), userID, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Not an error: the book has simply never been opened. Returning 404
			// would make the reader treat a fresh book as a failure.
			respondJSON(w, http.StatusOK, dto.ReadingProgress{BookFileID: id.String(), Started: false})
			return
		}
		respondError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, progressDTO(progress))
}

// Put serves PUT /api/v1/books/{id}/progress.
func (h *ProgressHandler) Put(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}

	var body dto.SaveProgress
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must be a reading position", err))
		return
	}

	locator, err := domain.ParseLocator(body.Locator)
	if err != nil {
		respondError(w, Invalid("locator is not valid: "+err.Error(), err))
		return
	}

	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	saved, err := h.Store.SaveProgress(r.Context(), domain.ReadingProgress{
		UserID:     userID,
		BookFileID: id,
		Position:   locator,
		Percent:    body.Percent,
	})
	if err != nil {
		if errors.Is(err, domain.ErrInvalidLocator) {
			respondError(w, Invalid(err.Error(), err))
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("book file"))
			return
		}
		respondError(w, err)
		return
	}

	// The response carries the STORED position, which may be further along than
	// the one just sent: another device may have read ahead. The client adopts
	// what comes back rather than assuming its own write won.
	respondJSON(w, http.StatusOK, progressDTO(saved))
}

// Continue serves GET /api/v1/continue.
func (h *ProgressHandler) Continue(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	items, err := h.Store.ContinueReading(r.Context(), userID, 12)
	if err != nil {
		respondError(w, err)
		return
	}

	out := make([]dto.ContinueItem, 0, len(items))
	for _, it := range items {
		out = append(out, dto.ContinueItem{
			WorkID:     it.WorkID.String(),
			Title:      it.Title,
			BookFileID: it.BookFileID.String(),
			Format:     string(it.Format),
			Percent:    it.Percent,
			LastReadAt: it.LastReadAt,
		})
	}
	respondJSON(w, http.StatusOK, dto.ContinueList{Items: out})
}

func progressDTO(p domain.ReadingProgress) dto.ReadingProgress {
	return dto.ReadingProgress{
		BookFileID:      p.BookFileID.String(),
		Started:         true,
		Locator:         p.Position.String(),
		Percent:         p.Percent,
		FurthestLocator: p.Furthest.String(),
		FurthestPercent: p.FurthestPercent,
		LastReadAt:      &p.LastReadAt,
	}
}
