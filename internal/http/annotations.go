// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// AnnotationStore is what the annotation handlers need.
type AnnotationStore interface {
	CreateAnnotation(ctx context.Context, a domain.Annotation) (domain.Annotation, error)
	Annotations(ctx context.Context, userID, bookFileID uuid.UUID) ([]domain.Annotation, error)
	UpdateAnnotation(ctx context.Context, userID, id uuid.UUID, note, colour string) (domain.Annotation, error)
	DeleteAnnotation(ctx context.Context, userID, id uuid.UUID) error
	MarkAnnotationsOrphaned(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) (int64, error)
	ClearAnnotationOrphan(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) error
	RecentAnnotations(ctx context.Context, userID uuid.UUID, limit int, cursor string) (postgres.RecentAnnotationsPage, error)
	ImportMarks(ctx context.Context, userID, bookFileID uuid.UUID, marks []postgres.ImportedMark) (int, error)
}

// AnnotationHandler serves highlights, notes, and bookmarks.
type AnnotationHandler struct {
	Store AnnotationStore
}

// List serves GET /api/v1/books/{id}/annotations.
func (h *AnnotationHandler) List(w http.ResponseWriter, r *http.Request) {
	bookFileID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	marks, err := h.Store.Annotations(r.Context(), userID, bookFileID)
	if err != nil {
		respondError(w, err)
		return
	}

	out := dto.AnnotationList{Items: make([]dto.Annotation, 0, len(marks))}
	for _, a := range marks {
		out.Items = append(out.Items, annotationDTO(a))
	}
	respondJSON(w, http.StatusOK, out)
}

// Create serves POST /api/v1/books/{id}/annotations.
func (h *AnnotationHandler) Create(w http.ResponseWriter, r *http.Request) {
	bookFileID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	var body dto.CreateAnnotation
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must be an annotation", err))
		return
	}

	start, err := domain.ParseLocator(body.Start)
	if err != nil {
		respondError(w, Invalid("start locator is not valid: "+err.Error(), err))
		return
	}
	end, err := domain.ParseLocator(body.End)
	if err != nil {
		respondError(w, Invalid("end locator is not valid: "+err.Error(), err))
		return
	}

	kind := domain.AnnotationType(strings.TrimSpace(body.Type))
	switch kind {
	case domain.AnnotationHighlight, domain.AnnotationNote, domain.AnnotationBookmark:
	case "":
		kind = domain.AnnotationHighlight
	default:
		respondError(w, Invalid("type must be highlight, note, or bookmark", nil))
		return
	}

	created, err := h.Store.CreateAnnotation(r.Context(), domain.Annotation{
		UserID:     userID,
		BookFileID: bookFileID,
		Type:       kind,
		Range:      domain.Range{Start: start, End: end},
		// The snapshot is what makes an orphan survivable: when the anchor stops
		// resolving, this is still shown to the user.
		SelectedText: body.SelectedText,
		Note:         body.Note,
		Colour:       body.Colour,
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

	respondJSON(w, http.StatusCreated, annotationDTO(created))
}

// Update serves PATCH /api/v1/annotations/{id}.
func (h *AnnotationHandler) Update(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("annotation id must be a UUID", err))
		return
	}
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	var body dto.UpdateAnnotation
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must be an annotation update", err))
		return
	}

	updated, err := h.Store.UpdateAnnotation(r.Context(), userID, id, body.Note, body.Colour)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("annotation"))
			return
		}
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, annotationDTO(updated))
}

// Delete serves DELETE /api/v1/annotations/{id}.
func (h *AnnotationHandler) Delete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("annotation id must be a UUID", err))
		return
	}
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	if err := h.Store.DeleteAnnotation(r.Context(), userID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("annotation"))
			return
		}
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusNoContent, nil)
}

// Orphan serves POST /api/v1/books/{id}/annotations/orphaned.
//
// The reader reports which anchors it could not resolve. The server does not
// discover this on its own, because only the client knows what the rendered
// document actually contains.
func (h *AnnotationHandler) Orphan(w http.ResponseWriter, r *http.Request) {
	if _, err := uuid.Parse(r.PathValue("id")); err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	var body dto.OrphanAnnotations
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must list annotation ids", err))
		return
	}

	ids := parseIDs(body.IDs)
	resolved := parseIDs(body.Resolved)

	// Clearing comes first. A mark can only be in one state, and doing it in
	// this order means a client that mistakenly sends the same id in both lists
	// ends up with it flagged rather than silently cleared.
	if err := h.Store.ClearAnnotationOrphan(r.Context(), userID, resolved); err != nil {
		respondError(w, err)
		return
	}

	count, err := h.Store.MarkAnnotationsOrphaned(r.Context(), userID, ids)
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, dto.OrphanResult{Orphaned: int(count)})
}

// Import serves POST /api/v1/books/{id}/annotations/import.
//
// The marks come from the CLIENT, because a PDF's highlights are rectangles
// and turning a rectangle back into words needs the geometry of the rendered
// text. The browser has that and the server does not: the extractor the server
// uses reports every word at position zero on the very books that carry
// annotations, which is how this design was settled. The same reason cover
// rendering happens in the browser.
func (h *AnnotationHandler) Import(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	bookFileID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book id must be a UUID", err))
		return
	}

	var body dto.ImportMarks
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImportBytes)).Decode(&body); err != nil {
		respondError(w, Invalid("an import is a list of marks", err))
		return
	}
	if len(body.Marks) > maxImportedMarks {
		respondError(w, Invalid("that is more marks than one book can hold", nil))
		return
	}

	marks := make([]postgres.ImportedMark, 0, len(body.Marks))
	for _, m := range body.Marks {
		marks = append(marks, postgres.ImportedMark{
			Page:       m.Page,
			Occurrence: m.Occurrence,
			Text:       m.Text,
			Note:       m.Note,
		})
	}

	stored, err := h.Store.ImportMarks(r.Context(), userID, bookFileID, marks)
	if err != nil {
		if isNotFound(err) {
			respondError(w, NotFound("book"))
			return
		}
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, dto.ImportResult{Imported: stored, Offered: len(body.Marks)})
}

// One book in a real library carries 1,051 marks, so the ceiling is well above
// what anybody has and well below what would hurt.
const (
	maxImportedMarks = 5000
	maxImportBytes   = 4 << 20
)

// Recent serves GET /api/v1/annotations, a notes view across the library,
// newest first, a page at a time (?limit=&cursor=; 50 by default, at most 200).
func (h *AnnotationHandler) Recent(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	limit := 0
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil {
			respondError(w, Invalid("limit must be an integer", err))
			return
		}
		limit = n
	}

	page, err := h.Store.RecentAnnotations(r.Context(), userID, limit, q.Get("cursor"))
	if err != nil {
		if errors.Is(err, store.ErrInvalidQuery) {
			respondError(w, Invalid(err.Error(), err))
			return
		}
		respondError(w, err)
		return
	}

	out := dto.RecentAnnotationList{
		Items:      make([]dto.RecentAnnotation, 0, len(page.Items)),
		NextCursor: page.NextCursor,
	}
	for _, item := range page.Items {
		out.Items = append(out.Items, dto.RecentAnnotation{
			Annotation: annotationDTO(item.Annotation),
			BookFileID: item.BookFileID.String(),
			WorkID:     item.WorkID.String(),
			Title:      item.Title,
			Format:     string(item.Format),
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// parseIDs keeps the ids that are well-formed and drops the rest, because one
// malformed id in a batch report should not discard the whole report.
func parseIDs(raw []string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(raw))
	for _, s := range raw {
		if id, err := uuid.Parse(s); err == nil {
			out = append(out, id)
		}
	}
	return out
}

func annotationDTO(a domain.Annotation) dto.Annotation {
	out := dto.Annotation{
		ID:           a.ID.String(),
		BookFileID:   a.BookFileID.String(),
		Type:         string(a.Type),
		Start:        a.Range.Start.String(),
		End:          a.Range.End.String(),
		SelectedText: a.SelectedText,
		Note:         a.Note,
		Colour:       a.Colour,
		Orphaned:     a.Orphaned(),
		Shared:       a.Shared,
		CreatedAt:    a.CreatedAt,
		UpdatedAt:    a.UpdatedAt,
	}
	if !a.EditedAt.IsZero() {
		edited := a.EditedAt
		out.EditedAt = &edited
	}
	if a.Shared {
		// Only for somebody else's mark. Naming the author of your own notes
		// back to you is noise.
		out.Author = a.Author
	}
	return out
}
