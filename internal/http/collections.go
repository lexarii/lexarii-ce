// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// CollectionStore is what this handler needs.
type CollectionStore interface {
	Collections(ctx context.Context, userID uuid.UUID) ([]postgres.CollectionSummary, error)
	Collection(ctx context.Context, userID, id uuid.UUID) (postgres.CollectionSummary, error)
	CreateCollection(ctx context.Context, userID uuid.UUID, name, description string, parent *uuid.UUID) (domain.Collection, error)
	UpdateCollection(ctx context.Context, userID, id uuid.UUID, name, description string) (domain.Collection, error)
	DeleteCollection(ctx context.Context, userID, id uuid.UUID) error
	AddToCollection(ctx context.Context, userID, collectionID, workID uuid.UUID) error
	RemoveFromCollection(ctx context.Context, userID, collectionID, workID uuid.UUID) error
	Library(ctx context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error)
}

// CollectionHandler serves a reader's own groupings of their books.
type CollectionHandler struct {
	Store CollectionStore
}

// List serves GET /api/v1/collections.
func (h *CollectionHandler) List(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	items, err := h.Store.Collections(r.Context(), userID)
	if err != nil {
		respondError(w, err)
		return
	}

	out := dto.CollectionList{Items: make([]dto.Collection, 0, len(items))}
	for _, item := range items {
		out.Items = append(out.Items, collectionDTO(item))
	}
	respondJSON(w, http.StatusOK, out)
}

// Detail serves GET /api/v1/collections/{id}, with the books in it.
func (h *CollectionHandler) Detail(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.collectionRequest(w, r)
	if !ok {
		return
	}

	summary, err := h.Store.Collection(r.Context(), userID, id)
	if err != nil {
		respondCollectionError(w, err)
		return
	}
	page, err := h.Store.Library(r.Context(), postgres.LibraryQuery{
		ViewerID:     userID,
		CollectionID: &id,
		Limit:        maxCollectionBooks,
	})
	if err != nil {
		respondError(w, err)
		return
	}

	out := dto.CollectionDetail{Collection: collectionDTO(summary)}
	out.Items = make([]dto.LibraryItem, 0, len(page.Items))
	for _, item := range page.Items {
		out.Items = append(out.Items, libraryItemDTO(item))
	}
	respondJSON(w, http.StatusOK, out)
}

// maxCollectionBooks bounds a collection listing. A hand-curated group of
// books is tens, not thousands; a cursor here would be scaffolding for a case
// that does not arise.
const maxCollectionBooks = 500

// Create serves POST /api/v1/collections.
func (h *CollectionHandler) Create(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	var body dto.CollectionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("a collection needs a name", err))
		return
	}

	var parent *uuid.UUID
	if body.ParentID != "" {
		id, err := uuid.Parse(body.ParentID)
		if err != nil {
			respondError(w, Invalid("parentId must be a UUID", err))
			return
		}
		parent = &id
	}

	created, err := h.Store.CreateCollection(r.Context(), userID, body.Name, body.Description, parent)
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, collectionDTO(postgres.CollectionSummary{Collection: created}))
}

// Update serves PATCH /api/v1/collections/{id}.
func (h *CollectionHandler) Update(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.collectionRequest(w, r)
	if !ok {
		return
	}

	var body dto.CollectionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("a collection needs a name", err))
		return
	}

	updated, err := h.Store.UpdateCollection(r.Context(), userID, id, body.Name, body.Description)
	if err != nil {
		respondCollectionError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, collectionDTO(postgres.CollectionSummary{Collection: updated}))
}

// Delete serves DELETE /api/v1/collections/{id}. The books are untouched.
func (h *CollectionHandler) Delete(w http.ResponseWriter, r *http.Request) {
	userID, id, ok := h.collectionRequest(w, r)
	if !ok {
		return
	}
	if err := h.Store.DeleteCollection(r.Context(), userID, id); err != nil {
		respondCollectionError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Add serves PUT /api/v1/collections/{id}/works/{workId}.
func (h *CollectionHandler) Add(w http.ResponseWriter, r *http.Request) {
	userID, id, workID, ok := h.membershipRequest(w, r)
	if !ok {
		return
	}
	if err := h.Store.AddToCollection(r.Context(), userID, id, workID); err != nil {
		respondCollectionError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Remove serves DELETE /api/v1/collections/{id}/works/{workId}.
func (h *CollectionHandler) Remove(w http.ResponseWriter, r *http.Request) {
	userID, id, workID, ok := h.membershipRequest(w, r)
	if !ok {
		return
	}
	if err := h.Store.RemoveFromCollection(r.Context(), userID, id, workID); err != nil {
		respondCollectionError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *CollectionHandler) collectionRequest(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("collection id must be a UUID", err))
		return uuid.Nil, uuid.Nil, false
	}
	return userID, id, true
}

func (h *CollectionHandler) membershipRequest(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, uuid.UUID, bool) {
	userID, id, ok := h.collectionRequest(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	workID, err := uuid.Parse(r.PathValue("workId"))
	if err != nil {
		respondError(w, Invalid("work id must be a UUID", err))
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	return userID, id, workID, true
}

// respondCollectionError keeps "no such collection" and "not your collection"
// as the same answer, which they should be: whether somebody else's collection
// exists is not a question this application answers.
func respondCollectionError(w http.ResponseWriter, err error) {
	if isNotFound(err) {
		respondError(w, NotFound("collection"))
		return
	}
	respondError(w, err)
}

func collectionDTO(s postgres.CollectionSummary) dto.Collection {
	out := dto.Collection{
		ID:          s.Collection.ID.String(),
		Name:        s.Collection.Name,
		Description: s.Collection.Description,
		Books:       s.Books,
		CreatedAt:   s.Collection.CreatedAt,
		UpdatedAt:   s.Collection.UpdatedAt,
	}
	if s.Collection.ParentID != nil {
		out.ParentID = s.Collection.ParentID.String()
	}
	return out
}
