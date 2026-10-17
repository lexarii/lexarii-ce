// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// SyncStore is what reconciling a client that was offline needs.
type SyncStore interface {
	Changes(ctx context.Context, userID uuid.UUID, since time.Time, cursor string, limit int) (postgres.Changes, error)
	ApplyMarks(ctx context.Context, userID uuid.UUID, marks []postgres.PushedMark) ([]postgres.MarkOutcome, error)
	MergePositions(ctx context.Context, userID uuid.UUID, positions []postgres.PushedPosition) error
}

// SyncHandler reconciles a client that has been reading offline.
type SyncHandler struct {
	Store SyncStore
}

// maxPushBytes and maxPushedItems bound one push.
//
// A week on a train is tens of marks, not thousands. The ceiling is well above
// anything a person produces and well below anything that would hold a
// connection open while it was applied.
const (
	maxPushBytes  = 2 << 20
	maxPushedMark = 500
)

// Pull serves GET /api/v1/sync.
//
// Everything about this person's own reading that changed after `since`, plus
// the server's clock to use as the next `since`. A client with no `since` at
// all is asking for everything, which is what a fresh install wants.
//
// When More comes back true, the response also carries Cursor: send that back
// as `cursor` on the next call to get the NEXT page. `since` is not what
// advances a page - it never did, resending it just re-reads the same window
// - `cursor` is what a client with more than one page waiting must use.
func (h *SyncHandler) Pull(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	var since time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			respondError(w, Invalid("since must be an RFC3339 timestamp", err))
			return
		}
		since = parsed
	}

	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))

	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			respondError(w, Invalid("limit must be a number", err))
			return
		}
		limit = parsed
	}

	changes, err := h.Store.Changes(r.Context(), userID, since, cursor, limit)
	if err != nil {
		// A cursor that was tampered with, or sent for a different account, is
		// the client's mistake - not a server fault - the same way a corrupt
		// library pagination cursor already is.
		if errors.Is(err, store.ErrInvalidQuery) {
			respondError(w, Invalid(err.Error(), err))
			return
		}
		respondError(w, err)
		return
	}

	out := dto.SyncPull{
		Until:       changes.Until,
		More:        changes.More,
		Cursor:      changes.Cursor,
		Annotations: make([]dto.Annotation, 0, len(changes.Annotations)),
		Deleted:     make([]dto.DeletedMark, 0, len(changes.Deleted)),
		Positions:   make([]dto.SyncPosition, 0, len(changes.Progress)),
	}
	for _, a := range changes.Annotations {
		out.Annotations = append(out.Annotations, annotationDTO(a))
	}
	for _, t := range changes.Deleted {
		out.Deleted = append(out.Deleted, dto.DeletedMark{
			ID:         t.AnnotationID.String(),
			BookFileID: t.BookFileID.String(),
			DeletedAt:  t.DeletedAt,
		})
	}
	for _, p := range changes.Progress {
		out.Positions = append(out.Positions, syncPositionDTO(p))
	}
	respondJSON(w, http.StatusOK, out)
}

// Push serves POST /api/v1/sync.
//
// What the client did while it was away. Every item carries its own id and its
// own timestamp, which is what makes this safe to repeat: a push whose
// response was lost is sent again and lands on the same rows with the same
// result.
func (h *SyncHandler) Push(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	var body dto.SyncPush
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPushBytes)).Decode(&body); err != nil {
		respondError(w, Invalid("body must be a list of changes", err))
		return
	}
	if len(body.Annotations) > maxPushedMark || len(body.Positions) > maxPushedMark {
		respondError(w, Invalid("that is more changes than one push may carry", nil))
		return
	}

	// A mark that fails to parse - a malformed locator, an unknown type - is
	// refused right here rather than sent to the store, but it still gets a
	// result. One mark shaped wrong must not cost the other thirty-nine their
	// place in the response, which is what returning 400 for the whole body
	// used to do: the client would retry the identical batch, hit the same
	// malformed mark, and never get the good ones stored at all.
	//
	// results is indexed like body.Annotations so a mark sent to the store
	// can be slotted back into its original position once ApplyMarks answers;
	// marks/indices carry only the ones that made it that far.
	results := make([]dto.MarkResult, len(body.Annotations))
	marks := make([]postgres.PushedMark, 0, len(body.Annotations))
	indices := make([]int, 0, len(body.Annotations))
	for i, m := range body.Annotations {
		mark, err := pushedMark(m)
		if err != nil {
			results[i] = dto.MarkResult{ID: m.ID, Status: postgres.MarkRefused}
			continue
		}
		marks = append(marks, mark)
		indices = append(indices, i)
	}

	positions := make([]postgres.PushedPosition, 0, len(body.Positions))
	for _, p := range body.Positions {
		position, err := pushedPosition(p)
		if err != nil {
			// Positions carry no per-item outcome on the wire - a reading
			// position is not something a client resends and checks the fate
			// of the way a mark is. A malformed one is dropped rather than
			// failing the marks in the same push, and it is logged because it
			// says a client is sending something it should not be.
			slog.Warn("skipping a malformed pushed position", "error", err)
			continue
		}
		positions = append(positions, position)
	}

	// ApplyMarks itself never fails the batch for one bad mark - see its own
	// documentation - so an error here is a server- or connection-level
	// failure, not a client mistake, and 500 is the right answer for it.
	outcomes, err := h.Store.ApplyMarks(r.Context(), userID, marks)
	if err != nil {
		respondError(w, err)
		return
	}
	for i, o := range outcomes {
		results[indices[i]] = dto.MarkResult{ID: o.ID.String(), Status: o.Status}
	}

	if err := h.Store.MergePositions(r.Context(), userID, positions); err != nil {
		respondError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, dto.SyncPushResult{Marks: results})
}

// pushedMark turns one wire change into a store change.
//
// The id is the CLIENT's, because a mark made with no signal has to have an
// identity before any server sees it. A malformed one is refused rather than
// replaced with a new one: silently renaming somebody's offline highlight
// would make the next push create a second copy of it.
func pushedMark(m dto.PushedMark) (postgres.PushedMark, error) {
	id, err := uuid.Parse(m.ID)
	if err != nil {
		return postgres.PushedMark{}, errBadPush("every change needs an id its client generated")
	}
	bookFileID, err := uuid.Parse(m.BookFileID)
	if err != nil {
		return postgres.PushedMark{}, errBadPush("a change has no book")
	}

	out := postgres.PushedMark{
		ID:         id,
		BookFileID: bookFileID,
		Deleted:    m.Deleted,
		UpdatedAt:  m.UpdatedAt,
	}
	// A deletion carries nothing else. Insisting on a locator for one would
	// stop a client deleting a mark whose anchor it no longer holds.
	if m.Deleted {
		return out, nil
	}

	start, err := domain.ParseLocator(m.Start)
	if err != nil {
		return postgres.PushedMark{}, errBadPush("start locator is not valid: " + err.Error())
	}
	end, err := domain.ParseLocator(m.End)
	if err != nil {
		return postgres.PushedMark{}, errBadPush("end locator is not valid: " + err.Error())
	}

	kind := domain.AnnotationType(strings.TrimSpace(m.Type))
	switch kind {
	case domain.AnnotationHighlight, domain.AnnotationNote, domain.AnnotationBookmark:
	case "":
		kind = domain.AnnotationHighlight
	default:
		return postgres.PushedMark{}, errBadPush("type must be highlight, note, or bookmark")
	}

	out.Type = kind
	out.Range = domain.Range{Start: start, End: end}
	out.SelectedText = m.SelectedText
	out.Note = m.Note
	out.Colour = m.Colour
	return out, nil
}

func pushedPosition(p dto.PushedPosition) (postgres.PushedPosition, error) {
	bookFileID, err := uuid.Parse(p.BookFileID)
	if err != nil {
		return postgres.PushedPosition{}, errBadPush("a position has no book")
	}
	locator, err := domain.ParseLocator(p.Position)
	if err != nil {
		return postgres.PushedPosition{}, errBadPush("position is not a locator: " + err.Error())
	}
	out := postgres.PushedPosition{
		BookFileID: bookFileID,
		Position:   locator,
		ReadAt:     p.ReadAt,
	}
	if p.Percent != nil {
		percent := float32(*p.Percent)
		out.Percent = &percent
	}
	return out, nil
}

type badPush string

func (e badPush) Error() string { return string(e) }

func errBadPush(message string) error { return badPush(message) }

func syncPositionDTO(p domain.ReadingProgress) dto.SyncPosition {
	out := dto.SyncPosition{
		BookFileID: p.BookFileID.String(),
		Position:   p.Position.String(),
		Furthest:   p.Furthest.String(),
		ReadAt:     p.LastReadAt,
		UpdatedAt:  p.UpdatedAt,
	}
	if p.Percent != nil {
		percent := float64(*p.Percent)
		out.Percent = &percent
	}
	if p.FurthestPercent != nil {
		percent := float64(*p.FurthestPercent)
		out.FurthestPercent = &percent
	}
	return out
}
