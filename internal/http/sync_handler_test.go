// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// syncStoreFake records what reached the store, so a test can tell what the
// handler refused itself from what it passed on.
type syncStoreFake struct {
	changes    postgres.Changes
	changesErr error
	since      time.Time
	cursor     string
	limit      int
	pullUser   uuid.UUID

	marks     []postgres.PushedMark
	applyErr  error
	outcome   func(m postgres.PushedMark) string
	positions []postgres.PushedPosition
	mergeErr  error
	merged    bool
}

func (f *syncStoreFake) Changes(_ context.Context, userID uuid.UUID, since time.Time, cursor string, limit int) (postgres.Changes, error) {
	f.pullUser, f.since, f.cursor, f.limit = userID, since, cursor, limit
	return f.changes, f.changesErr
}

func (f *syncStoreFake) ApplyMarks(_ context.Context, _ uuid.UUID, marks []postgres.PushedMark) ([]postgres.MarkOutcome, error) {
	f.marks = marks
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	out := make([]postgres.MarkOutcome, 0, len(marks))
	for _, m := range marks {
		status := postgres.MarkApplied
		if f.outcome != nil {
			status = f.outcome(m)
		}
		out = append(out, postgres.MarkOutcome{ID: m.ID, Status: status})
	}
	return out, nil
}

func (f *syncStoreFake) MergePositions(_ context.Context, _ uuid.UUID, positions []postgres.PushedPosition) error {
	f.merged = true
	f.positions = positions
	return f.mergeErr
}

func syncRouter(f *syncStoreFake) *Router { return &Router{Sync: &SyncHandler{Store: f}} }

func TestSyncNeedsSomebodySignedIn(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		f := &syncStoreFake{}
		rec := serveAsViewer(t, syncRouter(f), uuid.Nil, method, "/api/v1/sync", `{}`)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", method, rec.Code)
		}
		if f.pullUser != uuid.Nil || f.merged {
			t.Errorf("%s: an anonymous request reached the store", method)
		}
	}
}

func TestSyncPullPassesTheWindowThrough(t *testing.T) {
	user := uuid.New()
	since := time.Date(2026, 4, 1, 9, 30, 0, 123456789, time.UTC)

	for _, tc := range []struct {
		name, query string
		since       time.Time
		cursor      string
		limit       int
	}{
		{"a fresh install asks for everything", "", time.Time{}, "", 0},
		{"since, with nanoseconds kept", "?since=" + url.QueryEscape(since.Format(time.RFC3339Nano)), since, "", 0},
		{"a cursor and a limit, trimmed", "?cursor=%20opaque-cursor%20&limit=%2025%20", time.Time{}, "opaque-cursor", 25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &syncStoreFake{}
			rec := serveAsViewer(t, syncRouter(f), user, http.MethodGet, "/api/v1/sync"+tc.query, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
			}
			if f.pullUser != user || !f.since.Equal(tc.since) || f.cursor != tc.cursor || f.limit != tc.limit {
				t.Errorf("store asked (%s, %v, %q, %d), want (%s, %v, %q, %d)",
					f.pullUser, f.since, f.cursor, f.limit, user, tc.since, tc.cursor, tc.limit)
			}
		})
	}

	for _, tc := range []struct {
		name, query string
		err         error
		status      int
	}{
		{"since that is not a timestamp", "?since=yesterday", nil, http.StatusBadRequest},
		{"a limit that is not a number", "?limit=lots", nil, http.StatusBadRequest},
		{"a tampered cursor", "?cursor=bogus", fmt.Errorf("%w: cursor is not valid", store.ErrInvalidQuery), http.StatusBadRequest},
		{"a store failure", "", errors.New("boom"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &syncStoreFake{changesErr: tc.err}
			rec := serveAsViewer(t, syncRouter(f), user, http.MethodGet, "/api/v1/sync"+tc.query, "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
		})
	}
}

func TestSyncPullRendersEveryKindOfChange(t *testing.T) {
	user, fileID := uuid.New(), uuid.New()
	until := time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC)
	pct, far := float32(0.5), float32(0.75)
	mark := domain.Annotation{ID: uuid.New(), BookFileID: fileID, Type: domain.AnnotationHighlight,
		Range: domain.Range{Start: domain.NewPDFLocator(3, 0), End: domain.NewPDFLocator(3, 8)}}
	tomb := postgres.Tombstone{AnnotationID: uuid.New(), BookFileID: fileID, DeletedAt: until.Add(-time.Minute)}
	withPercent := domain.ReadingProgress{BookFileID: fileID, Position: domain.NewPDFLocator(7, 0), Percent: &pct,
		Furthest: domain.NewPDFLocator(9, 0), FurthestPercent: &far, LastReadAt: until.Add(-time.Hour), UpdatedAt: until}
	withoutPercent := domain.ReadingProgress{BookFileID: uuid.New(), Position: domain.NewPDFLocator(1, 0),
		Furthest: domain.NewPDFLocator(1, 0)}

	f := &syncStoreFake{changes: postgres.Changes{
		Annotations: []domain.Annotation{mark}, Deleted: []postgres.Tombstone{tomb},
		Progress: []domain.ReadingProgress{withPercent, withoutPercent},
		Until:    until, More: true, Cursor: "next-page",
	}}
	rec := serveAsViewer(t, syncRouter(f), user, http.MethodGet, "/api/v1/sync", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	out := decodeInto[dto.SyncPull](t, rec)
	if !out.Until.Equal(until) || !out.More || out.Cursor != "next-page" {
		t.Errorf("until/more/cursor = %v/%v/%q", out.Until, out.More, out.Cursor)
	}
	if len(out.Annotations) != 1 || out.Annotations[0].ID != mark.ID.String() || out.Annotations[0].Start != "pdf:3:0" {
		t.Errorf("annotations = %+v", out.Annotations)
	}
	if len(out.Deleted) != 1 || out.Deleted[0].ID != tomb.AnnotationID.String() ||
		out.Deleted[0].BookFileID != fileID.String() || !out.Deleted[0].DeletedAt.Equal(tomb.DeletedAt) {
		t.Errorf("deleted = %+v", out.Deleted)
	}
	if len(out.Positions) != 2 {
		t.Fatalf("positions = %+v", out.Positions)
	}
	p := out.Positions[0]
	if p.Position != "pdf:7:0" || p.Furthest != "pdf:9:0" || p.Percent == nil || *p.Percent != 0.5 ||
		p.FurthestPercent == nil || *p.FurthestPercent != 0.75 || !p.UpdatedAt.Equal(until) {
		t.Errorf("position = %+v", p)
	}
	if q := out.Positions[1]; q.Percent != nil || q.FurthestPercent != nil {
		t.Errorf("a position with no percentage gained one: %+v", q)
	}

	t.Run("nothing changed is empty lists, not nulls", func(t *testing.T) {
		rec := serveAsViewer(t, syncRouter(&syncStoreFake{}), user, http.MethodGet, "/api/v1/sync", "")
		for _, key := range []string{`"annotations":[]`, `"deleted":[]`, `"positions":[]`} {
			if !strings.Contains(rec.Body.String(), key) {
				t.Errorf("missing %s in %s", key, rec.Body)
			}
		}
	})
}

// One malformed mark is refused on its own and keeps its place in the
// response; the rest of the push goes through.
func TestSyncPushRefusesPerItem(t *testing.T) {
	user, fileID := uuid.New(), uuid.New()
	at := time.Date(2026, 4, 3, 7, 0, 0, 0, time.UTC)
	good := func(id uuid.UUID) dto.PushedMark {
		return dto.PushedMark{ID: id.String(), BookFileID: fileID.String(), Start: "pdf:2:0", End: "pdf:2:9", UpdatedAt: at}
	}
	applied, stale, deletion := uuid.New(), uuid.New(), uuid.New()

	marks := []dto.PushedMark{
		good(applied),
		{ID: "client-made-nonsense", BookFileID: fileID.String(), Start: "pdf:1:0", End: "pdf:1:1"},
		{ID: uuid.NewString(), BookFileID: "no-book", Start: "pdf:1:0", End: "pdf:1:1"},
		{ID: uuid.NewString(), BookFileID: fileID.String(), Start: "37%", End: "pdf:1:1"},
		{ID: uuid.NewString(), BookFileID: fileID.String(), Start: "pdf:1:0", End: "pdf:-1:1"},
		{ID: uuid.NewString(), BookFileID: fileID.String(), Start: "pdf:1:0", End: "pdf:1:1", Type: "doodle"},
		// A deletion needs no anchor: the client may no longer hold one.
		{ID: deletion.String(), BookFileID: fileID.String(), Deleted: true, UpdatedAt: at},
		func() dto.PushedMark { m := good(stale); m.Type = "bookmark"; m.Note = "n"; m.Colour = "red"; return m }(),
	}
	half := 0.5
	positions := []dto.PushedPosition{
		{BookFileID: fileID.String(), Position: "pdf:4:0", Percent: &half, ReadAt: at},
		{BookFileID: "no-book", Position: "pdf:4:0"},
		{BookFileID: fileID.String(), Position: "half way"},
		{BookFileID: fileID.String(), Position: "epub:0/body[1]:0"},
	}
	body, err := json.Marshal(dto.SyncPush{Annotations: marks, Positions: positions})
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	f := &syncStoreFake{outcome: func(m postgres.PushedMark) string {
		if m.ID == stale {
			return postgres.MarkStale
		}
		return postgres.MarkApplied
	}}
	rec := serveAsViewer(t, syncRouter(f), user, http.MethodPost, "/api/v1/sync", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}

	out := decodeInto[dto.SyncPushResult](t, rec)
	want := []dto.MarkResult{
		{ID: applied.String(), Status: postgres.MarkApplied},
		{ID: marks[1].ID, Status: postgres.MarkRefused},
		{ID: marks[2].ID, Status: postgres.MarkRefused},
		{ID: marks[3].ID, Status: postgres.MarkRefused},
		{ID: marks[4].ID, Status: postgres.MarkRefused},
		{ID: marks[5].ID, Status: postgres.MarkRefused},
		{ID: deletion.String(), Status: postgres.MarkApplied},
		{ID: stale.String(), Status: postgres.MarkStale},
	}
	if len(out.Marks) != len(want) {
		t.Fatalf("results = %+v, want %d in push order", out.Marks, len(want))
	}
	for i := range want {
		if out.Marks[i] != want[i] {
			t.Errorf("result %d = %+v, want %+v", i, out.Marks[i], want[i])
		}
	}

	// Only the well-formed marks reached the store, typed and anchored.
	if len(f.marks) != 3 {
		t.Fatalf("the store received %d marks, want 3", len(f.marks))
	}
	if m := f.marks[0]; m.ID != applied || m.Type != domain.AnnotationHighlight || m.Range.Start.String() != "pdf:2:0" || !m.UpdatedAt.Equal(at) {
		t.Errorf("first mark = %+v; an untyped mark is a highlight", m)
	}
	if m := f.marks[1]; m.ID != deletion || !m.Deleted || m.Range.Start.Format != "" {
		t.Errorf("deletion = %+v", m)
	}
	if m := f.marks[2]; m.Type != domain.AnnotationBookmark || m.Note != "n" || m.Colour != "red" {
		t.Errorf("last mark = %+v", m)
	}

	// Malformed positions are dropped; good ones keep their percentage.
	if len(f.positions) != 2 {
		t.Fatalf("the store received %d positions, want 2: %+v", len(f.positions), f.positions)
	}
	if p := f.positions[0]; p.BookFileID != fileID || p.Position.String() != "pdf:4:0" || p.Percent == nil || *p.Percent != 0.5 || !p.ReadAt.Equal(at) {
		t.Errorf("position = %+v", p)
	}
	if p := f.positions[1]; p.Percent != nil || p.Position.String() != "epub:0/body[1]:0" {
		t.Errorf("second position = %+v", p)
	}
}

func TestSyncPushRefusesWholeBodies(t *testing.T) {
	user := uuid.New()
	tooMany := dto.SyncPush{Annotations: make([]dto.PushedMark, maxPushedMark+1)}
	manyBody, err := json.Marshal(tooMany)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	tooManyPositions := dto.SyncPush{Positions: make([]dto.PushedPosition, maxPushedMark+1)}
	positionsBody, err := json.Marshal(tooManyPositions)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}

	for _, tc := range []struct {
		name, body string
		f          *syncStoreFake
		status     int
	}{
		{"a body that is not JSON", `{"annotations":`, &syncStoreFake{}, http.StatusBadRequest},
		{"more marks than one push carries", string(manyBody), &syncStoreFake{}, http.StatusBadRequest},
		{"more positions than one push carries", string(positionsBody), &syncStoreFake{}, http.StatusBadRequest},
		{"applying fails", `{}`, &syncStoreFake{applyErr: errors.New("boom")}, http.StatusInternalServerError},
		{"merging positions fails", `{}`, &syncStoreFake{mergeErr: errors.New("boom")}, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveAsViewer(t, syncRouter(tc.f), user, http.MethodPost, "/api/v1/sync", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if tc.status == http.StatusBadRequest && (tc.f.marks != nil || tc.f.merged) {
				t.Error("a refused push reached the store")
			}
		})
	}

	t.Run("an empty push answers an empty list", func(t *testing.T) {
		f := &syncStoreFake{}
		rec := serveAsViewer(t, syncRouter(f), user, http.MethodPost, "/api/v1/sync", `{}`)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"marks":[]`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})
}
