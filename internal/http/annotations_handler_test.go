// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// serveAsViewer sends a request through a router, signed in as viewer (or
// anonymous when viewer is uuid.Nil).
func serveAsViewer(t *testing.T, rt *Router, viewer uuid.UUID, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if viewer != uuid.Nil {
		req = asUserID(req, viewer)
	}
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeInto decodes a JSON response body, failing the test if it is not one.
func decodeInto[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %T: %v; body = %s", out, err, rec.Body)
	}
	return out
}

// annotationStoreFake records what each call received and answers with what
// the test set.
type annotationStoreFake struct {
	err error

	created domain.Annotation
	listed  [2]uuid.UUID // user, book
	list    []domain.Annotation
	updated struct {
		user, id     uuid.UUID
		note, colour string
	}
	deleted      [2]uuid.UUID // user, id
	orphaned     []uuid.UUID
	cleared      []uuid.UUID
	calls        []string
	orphanErr    error
	orphanN      int64
	recentUser   uuid.UUID
	recent       []postgres.AnnotationWithBook
	recentLimit  int
	recentCursor string
	recentNext   string
	imported     []postgres.ImportedMark
	importUser   uuid.UUID
	importFile   uuid.UUID
}

func (f *annotationStoreFake) CreateAnnotation(_ context.Context, a domain.Annotation) (domain.Annotation, error) {
	f.created = a
	if f.err != nil {
		return domain.Annotation{}, f.err
	}
	a.ID = uuid.New()
	a.CreatedAt = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	a.UpdatedAt = a.CreatedAt
	return a, nil
}

func (f *annotationStoreFake) Annotations(_ context.Context, userID, bookFileID uuid.UUID) ([]domain.Annotation, error) {
	f.listed = [2]uuid.UUID{userID, bookFileID}
	return f.list, f.err
}

func (f *annotationStoreFake) UpdateAnnotation(_ context.Context, userID, id uuid.UUID, note, colour string) (domain.Annotation, error) {
	f.updated.user, f.updated.id, f.updated.note, f.updated.colour = userID, id, note, colour
	if f.err != nil {
		return domain.Annotation{}, f.err
	}
	return domain.Annotation{ID: id, UserID: userID, Type: domain.AnnotationNote, Note: note, Colour: colour,
		Range: domain.Range{Start: domain.NewEPUBLocator(1, "p[1]", 0), End: domain.NewEPUBLocator(1, "p[1]", 4)}}, nil
}

func (f *annotationStoreFake) DeleteAnnotation(_ context.Context, userID, id uuid.UUID) error {
	f.deleted = [2]uuid.UUID{userID, id}
	return f.err
}

func (f *annotationStoreFake) MarkAnnotationsOrphaned(_ context.Context, _ uuid.UUID, ids []uuid.UUID) (int64, error) {
	f.calls = append(f.calls, "mark")
	f.orphaned = ids
	return f.orphanN, f.orphanErr
}

func (f *annotationStoreFake) ClearAnnotationOrphan(_ context.Context, _ uuid.UUID, ids []uuid.UUID) error {
	f.calls = append(f.calls, "clear")
	f.cleared = ids
	return f.err
}

func (f *annotationStoreFake) RecentAnnotations(_ context.Context, userID uuid.UUID, limit int, cursor string) (postgres.RecentAnnotationsPage, error) {
	f.recentUser, f.recentLimit, f.recentCursor = userID, limit, cursor
	if cursor == "bad" {
		return postgres.RecentAnnotationsPage{}, fmt.Errorf("%w: cursor", store.ErrInvalidQuery)
	}
	return postgres.RecentAnnotationsPage{Items: f.recent, NextCursor: f.recentNext}, f.err
}

func (f *annotationStoreFake) ImportMarks(_ context.Context, userID, bookFileID uuid.UUID, marks []postgres.ImportedMark) (int, error) {
	f.importUser, f.importFile, f.imported = userID, bookFileID, marks
	if f.err != nil {
		return 0, f.err
	}
	return len(marks) - 1, nil
}

func annotationRouter(f *annotationStoreFake) *Router {
	return &Router{Annotations: &AnnotationHandler{Store: f}}
}

// Everything about annotations is somebody's own: with nobody signed in there
// is nobody to ask the store about, and it is not asked.
func TestAnnotationsNeedSomebodySignedIn(t *testing.T) {
	fileID, mark := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/v1/books/" + fileID + "/annotations", ""},
		{http.MethodPost, "/api/v1/books/" + fileID + "/annotations", `{"start":"pdf:1:0","end":"pdf:1:4"}`},
		{http.MethodPost, "/api/v1/books/" + fileID + "/annotations/orphaned", `{"ids":[]}`},
		{http.MethodPost, "/api/v1/books/" + fileID + "/annotations/import", `{"marks":[]}`},
		{http.MethodPatch, "/api/v1/annotations/" + mark, `{"note":"x"}`},
		{http.MethodDelete, "/api/v1/annotations/" + mark, ""},
		{http.MethodGet, "/api/v1/annotations", ""},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			f := &annotationStoreFake{err: errors.New("the store must not be reached")}
			rec := serveAsViewer(t, annotationRouter(f), uuid.Nil, tc.method, tc.target, tc.body)
			if rec.Code != http.StatusUnauthorized || wireCodeOf(t, rec) != "not_signed_in" {
				t.Fatalf("status = %d, body = %s; want 401 not_signed_in", rec.Code, rec.Body)
			}
		})
	}
}

func TestAnnotationRequestsAreValidated(t *testing.T) {
	user, fileID := uuid.New(), uuid.NewString()
	for _, tc := range []struct {
		name, method, target, body string
	}{
		{"list: a malformed book id", http.MethodGet, "/api/v1/books/nope/annotations", ""},
		{"create: a malformed book id", http.MethodPost, "/api/v1/books/nope/annotations", `{"start":"pdf:1:0","end":"pdf:1:4"}`},
		{"create: a body that is not JSON", http.MethodPost, "/api/v1/books/" + fileID + "/annotations", `{"start":`},
		{"create: a start that is not a locator", http.MethodPost, "/api/v1/books/" + fileID + "/annotations", `{"start":"42%","end":"pdf:1:4"}`},
		{"create: an end that is not a locator", http.MethodPost, "/api/v1/books/" + fileID + "/annotations", `{"start":"pdf:1:0","end":"pdf:0:4"}`},
		{"create: an unknown type", http.MethodPost, "/api/v1/books/" + fileID + "/annotations", `{"type":"scribble","start":"pdf:1:0","end":"pdf:1:4"}`},
		{"create: a body over the limit", http.MethodPost, "/api/v1/books/" + fileID + "/annotations",
			`{"start":"pdf:1:0","end":"pdf:1:4","note":"` + strings.Repeat("x", 70<<10) + `"}`},
		{"update: a malformed id", http.MethodPatch, "/api/v1/annotations/nope", `{"note":"x"}`},
		{"update: a body that is not JSON", http.MethodPatch, "/api/v1/annotations/" + uuid.NewString(), `note`},
		{"delete: a malformed id", http.MethodDelete, "/api/v1/annotations/nope", ""},
		{"orphan: a malformed book id", http.MethodPost, "/api/v1/books/nope/annotations/orphaned", `{"ids":[]}`},
		{"orphan: a body that is not JSON", http.MethodPost, "/api/v1/books/" + fileID + "/annotations/orphaned", `[`},
		{"import: a malformed book id", http.MethodPost, "/api/v1/books/nope/annotations/import", `{"marks":[]}`},
		{"import: a body that is not JSON", http.MethodPost, "/api/v1/books/" + fileID + "/annotations/import", `{"marks":`},
		{"import: more marks than a book holds", http.MethodPost, "/api/v1/books/" + fileID + "/annotations/import",
			`{"marks":[` + strings.TrimSuffix(strings.Repeat(`{"page":1},`, maxImportedMarks+1), ",") + `]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &annotationStoreFake{err: errors.New("the store must not be reached")}
			rec := serveAsViewer(t, annotationRouter(f), user, tc.method, tc.target, tc.body)
			if rec.Code != http.StatusBadRequest || wireCodeOf(t, rec) != string(CodeInvalid) {
				t.Fatalf("status = %d, body = %s; want 400 invalid_request", rec.Code, rec.Body)
			}
			if f.created.UserID != uuid.Nil || f.imported != nil || f.calls != nil {
				t.Error("an invalid request reached the store")
			}
		})
	}
}

func TestCreatingAnAnnotation(t *testing.T) {
	user, fileID := uuid.New(), uuid.New()
	target := "/api/v1/books/" + fileID.String() + "/annotations"

	t.Run("stored as the signed-in person's, with the snapshot", func(t *testing.T) {
		f := &annotationStoreFake{}
		rec := serveAsViewer(t, annotationRouter(f), user, http.MethodPost, target,
			`{"type":" note ","start":"epub:2/section[1]/p[3]:5","end":"epub:2/section[1]/p[3]:40",
			  "selectedText":"the words","note":"mine","colour":"yellow"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
		}
		got := f.created
		if got.UserID != user || got.BookFileID != fileID || got.Type != domain.AnnotationNote ||
			got.SelectedText != "the words" || got.Note != "mine" || got.Colour != "yellow" {
			t.Errorf("the store received %+v", got)
		}
		if got.Range.Start.String() != "epub:2/section[1]/p[3]:5" || got.Range.End.String() != "epub:2/section[1]/p[3]:40" {
			t.Errorf("range = %s .. %s", got.Range.Start, got.Range.End)
		}
		out := decodeInto[dto.Annotation](t, rec)
		if out.ID == "" || out.BookFileID != fileID.String() || out.Type != "note" ||
			out.Start != "epub:2/section[1]/p[3]:5" || out.SelectedText != "the words" || out.Orphaned || out.Author != "" {
			t.Errorf("response = %+v", out)
		}
	})

	t.Run("an unnamed type is a highlight", func(t *testing.T) {
		f := &annotationStoreFake{}
		rec := serveAsViewer(t, annotationRouter(f), user, http.MethodPost, target, `{"start":"pdf:3:0","end":"pdf:3:9"}`)
		if rec.Code != http.StatusCreated || f.created.Type != domain.AnnotationHighlight {
			t.Fatalf("status = %d, type = %q; want 201 highlight", rec.Code, f.created.Type)
		}
	})

	t.Run("a range the store finds invalid is the client's mistake", func(t *testing.T) {
		f := &annotationStoreFake{err: fmt.Errorf("creating annotation: %w", domain.ErrInvalidLocator)}
		rec := serveAsViewer(t, annotationRouter(f), user, http.MethodPost, target, `{"start":"pdf:3:9","end":"epub:1/p[1]:0"}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("a book the person cannot open is not found", func(t *testing.T) {
		f := &annotationStoreFake{err: store.ErrNotFound}
		rec := serveAsViewer(t, annotationRouter(f), user, http.MethodPost, target, `{"start":"pdf:1:0","end":"pdf:1:4"}`)
		if rec.Code != http.StatusNotFound || wireCodeOf(t, rec) != string(CodeNotFound) {
			t.Fatalf("status = %d, body = %s; want 404 not_found, never a server error or a 403", rec.Code, rec.Body)
		}
	})

	t.Run("a store failure is a server fault", func(t *testing.T) {
		f := &annotationStoreFake{err: errors.New("disk full")}
		rec := serveAsViewer(t, annotationRouter(f), user, http.MethodPost, target, `{"start":"pdf:1:0","end":"pdf:1:4"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestListingAnnotations(t *testing.T) {
	user, fileID := uuid.New(), uuid.New()
	orphanedAt := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	own := domain.Annotation{ID: uuid.New(), BookFileID: fileID, Type: domain.AnnotationHighlight,
		Range:  domain.Range{Start: domain.NewPDFLocator(4, 0), End: domain.NewPDFLocator(4, 12)},
		Author: "me, who should not be named back to myself", OrphanedAt: &orphanedAt}
	shared := domain.Annotation{ID: uuid.New(), BookFileID: fileID, Type: domain.AnnotationNote,
		Range:  domain.Range{Start: domain.NewPDFLocator(5, 0), End: domain.NewPDFLocator(5, 3)},
		Author: "Ada", Shared: true, Note: "see chapter 9"}

	f := &annotationStoreFake{list: []domain.Annotation{own, shared}}
	rec := serveAsViewer(t, annotationRouter(f), user, http.MethodGet, "/api/v1/books/"+fileID.String()+"/annotations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if f.listed != [2]uuid.UUID{user, fileID} {
		t.Errorf("the store was asked for %v", f.listed)
	}
	out := decodeInto[dto.AnnotationList](t, rec)
	if len(out.Items) != 2 {
		t.Fatalf("items = %+v", out.Items)
	}
	if !out.Items[0].Orphaned || out.Items[0].Author != "" || out.Items[0].Start != "pdf:4:0" {
		t.Errorf("own mark = %+v; an orphan must say so and must not name its own author", out.Items[0])
	}
	if !out.Items[1].Shared || out.Items[1].Author != "Ada" || out.Items[1].Note != "see chapter 9" {
		t.Errorf("shared mark = %+v; somebody else's mark names its author", out.Items[1])
	}

	t.Run("an empty book is an empty list, not null", func(t *testing.T) {
		rec := serveAsViewer(t, annotationRouter(&annotationStoreFake{}), user, http.MethodGet,
			"/api/v1/books/"+fileID.String()+"/annotations", "")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	})

	t.Run("a store failure is a server fault", func(t *testing.T) {
		rec := serveAsViewer(t, annotationRouter(&annotationStoreFake{err: errors.New("boom")}), user,
			http.MethodGet, "/api/v1/books/"+fileID.String()+"/annotations", "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestUpdatingAndDeletingAnAnnotation(t *testing.T) {
	user, mark := uuid.New(), uuid.New()
	target := "/api/v1/annotations/" + mark.String()

	t.Run("update", func(t *testing.T) {
		f := &annotationStoreFake{}
		rec := serveAsViewer(t, annotationRouter(f), user, http.MethodPatch, target, `{"note":"rethought","colour":"blue"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
		}
		if f.updated.user != user || f.updated.id != mark || f.updated.note != "rethought" || f.updated.colour != "blue" {
			t.Errorf("the store received %+v", f.updated)
		}
		if out := decodeInto[dto.Annotation](t, rec); out.ID != mark.String() || out.Note != "rethought" {
			t.Errorf("response = %+v", out)
		}
	})

	t.Run("delete", func(t *testing.T) {
		f := &annotationStoreFake{}
		rec := serveAsViewer(t, annotationRouter(f), user, http.MethodDelete, target, "")
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("status = %d, body = %q; want an empty 204", rec.Code, rec.Body)
		}
		if f.deleted != [2]uuid.UUID{user, mark} {
			t.Errorf("the store was asked to delete %v", f.deleted)
		}
	})

	// Somebody else's mark - shared with them or not - is matched on its
	// author, so it is "not found" rather than "forbidden".
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"not theirs", store.ErrNotFound, http.StatusNotFound},
		{"a store failure", errors.New("boom"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &annotationStoreFake{err: tc.err}
			if rec := serveAsViewer(t, annotationRouter(f), user, http.MethodPatch, target, `{"note":"x"}`); rec.Code != tc.status {
				t.Errorf("update: status = %d, want %d", rec.Code, tc.status)
			}
			if rec := serveAsViewer(t, annotationRouter(f), user, http.MethodDelete, target, ""); rec.Code != tc.status {
				t.Errorf("delete: status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
}

// The reader reports both directions, and clearing comes first so an id sent
// in both lists ends up flagged rather than silently cleared.
func TestReportingOrphanedAnnotations(t *testing.T) {
	user, fileID := uuid.New(), uuid.New()
	target := "/api/v1/books/" + fileID.String() + "/annotations/orphaned"
	lost, found := uuid.New(), uuid.New()

	f := &annotationStoreFake{orphanN: 1}
	rec := serveAsViewer(t, annotationRouter(f), user, http.MethodPost, target,
		`{"ids":["`+lost.String()+`","not-an-id"],"resolved":["`+found.String()+`","also bad"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if strings.Join(f.calls, ",") != "clear,mark" {
		t.Errorf("calls = %v, want clear before mark", f.calls)
	}
	if len(f.orphaned) != 1 || f.orphaned[0] != lost || len(f.cleared) != 1 || f.cleared[0] != found {
		t.Errorf("orphaned %v, cleared %v; malformed ids should be dropped and the rest kept", f.orphaned, f.cleared)
	}
	if out := decodeInto[dto.OrphanResult](t, rec); out.Orphaned != 1 {
		t.Errorf("orphaned = %d, want 1", out.Orphaned)
	}

	for _, tc := range []struct {
		name string
		f    *annotationStoreFake
	}{
		{"clearing fails", &annotationStoreFake{err: errors.New("boom")}},
		{"marking fails", &annotationStoreFake{orphanErr: errors.New("boom")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveAsViewer(t, annotationRouter(tc.f), user, http.MethodPost, target, `{"ids":[]}`)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
		})
	}
}

func TestImportingAPDFsOwnMarks(t *testing.T) {
	user, fileID := uuid.New(), uuid.New()
	target := "/api/v1/books/" + fileID.String() + "/annotations/import"
	body := `{"marks":[{"page":3,"occurrence":1,"text":"lemma","note":"check"},{"page":9,"text":"proof"}]}`

	f := &annotationStoreFake{}
	rec := serveAsViewer(t, annotationRouter(f), user, http.MethodPost, target, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	want := []postgres.ImportedMark{{Page: 3, Occurrence: 1, Text: "lemma", Note: "check"}, {Page: 9, Text: "proof"}}
	if f.importUser != user || f.importFile != fileID || len(f.imported) != 2 || f.imported[0] != want[0] || f.imported[1] != want[1] {
		t.Errorf("the store received %v for %s/%s", f.imported, f.importUser, f.importFile)
	}
	if out := decodeInto[dto.ImportResult](t, rec); out.Imported != 1 || out.Offered != 2 {
		t.Errorf("result = %+v, want 1 of 2 imported", out)
	}

	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"a book the person cannot open", fmt.Errorf("importing: %w", store.ErrNotFound), http.StatusNotFound},
		{"a store failure", errors.New("boom"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := serveAsViewer(t, annotationRouter(&annotationStoreFake{err: tc.err}), user, http.MethodPost, target, body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
}

func TestRecentAnnotationsAcrossTheLibrary(t *testing.T) {
	user := uuid.New()
	item := postgres.AnnotationWithBook{
		Annotation: domain.Annotation{ID: uuid.New(), Type: domain.AnnotationHighlight,
			Range: domain.Range{Start: domain.NewPDFLocator(2, 0), End: domain.NewPDFLocator(2, 5)}},
		BookFileID: uuid.New(), WorkID: uuid.New(), Title: "Linear Algebra Done Right", Format: domain.FormatPDF,
	}
	f := &annotationStoreFake{recent: []postgres.AnnotationWithBook{item}}
	rec := serveAsViewer(t, annotationRouter(f), user, http.MethodGet, "/api/v1/annotations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if f.recentUser != user {
		t.Errorf("asked for %s's notes, want the signed-in person's", f.recentUser)
	}
	out := decodeInto[dto.RecentAnnotationList](t, rec)
	if len(out.Items) != 1 {
		t.Fatalf("items = %+v", out.Items)
	}
	got := out.Items[0]
	if got.Annotation.ID != item.Annotation.ID.String() || got.BookFileID != item.BookFileID.String() ||
		got.WorkID != item.WorkID.String() || got.Title != item.Title || got.Format != "pdf" {
		t.Errorf("item = %+v", got)
	}

	rec = serveAsViewer(t, annotationRouter(&annotationStoreFake{err: errors.New("boom")}), user, http.MethodGet, "/api/v1/annotations", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure: status = %d, want 500", rec.Code)
	}
}

// The notes view is paged by the wire convention: ?limit=&cursor= in,
// {items, nextCursor} out, and a malformed request is the client's mistake.
func TestRecentAnnotationsArePaged(t *testing.T) {
	user := uuid.New()
	f := &annotationStoreFake{recentNext: "next-page"}
	rec := serveAsViewer(t, annotationRouter(f), user, http.MethodGet, "/api/v1/annotations?limit=20&cursor=abc", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if f.recentLimit != 20 || f.recentCursor != "abc" {
		t.Errorf("store asked for limit %d cursor %q, want 20 and abc", f.recentLimit, f.recentCursor)
	}
	if out := decodeInto[dto.RecentAnnotationList](t, rec); out.NextCursor != "next-page" {
		t.Errorf("nextCursor = %q, want the store's", out.NextCursor)
	}

	for _, query := range []string{"?limit=lots", "?cursor=bad"} {
		rec := serveAsViewer(t, annotationRouter(&annotationStoreFake{}), user, http.MethodGet, "/api/v1/annotations"+query, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", query, rec.Code)
		}
	}
}

// A mark with no known edit time leaves editedAt out rather than sending year
// one, which a client comparing edits reads as the oldest edit there is.
func TestEditedAtIsOmittedWhenUnknown(t *testing.T) {
	user := uuid.New()
	made := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	unknown := postgres.AnnotationWithBook{
		Annotation: domain.Annotation{ID: uuid.New(), Type: domain.AnnotationHighlight, CreatedAt: made, UpdatedAt: made,
			Range: domain.Range{Start: domain.NewPDFLocator(2, 0), End: domain.NewPDFLocator(2, 5)}},
		BookFileID: uuid.New(), WorkID: uuid.New(), Format: domain.FormatPDF,
	}
	known := unknown
	known.Annotation.ID = uuid.New()
	known.Annotation.EditedAt = made

	f := &annotationStoreFake{recent: []postgres.AnnotationWithBook{unknown, known}}
	rec := serveAsViewer(t, annotationRouter(f), user, http.MethodGet, "/api/v1/annotations", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "0001-01-01") {
		t.Errorf("the zero time went out on the wire: %s", rec.Body)
	}
	out := decodeInto[dto.RecentAnnotationList](t, rec)
	if out.Items[0].Annotation.EditedAt != nil {
		t.Errorf("unknown editedAt = %v, want absent", out.Items[0].Annotation.EditedAt)
	}
	if got := out.Items[1].Annotation.EditedAt; got == nil || !got.Equal(made) {
		t.Errorf("known editedAt = %v, want %v", got, made)
	}
}
