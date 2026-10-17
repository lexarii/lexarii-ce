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

// collectionStoreFake records each call as "method user collection work" and
// answers with what the test set.
type collectionStoreFake struct {
	err        error
	libraryErr error
	summaries  []postgres.CollectionSummary
	summary    postgres.CollectionSummary
	library    postgres.LibraryPage
	query      postgres.LibraryQuery
	created    struct {
		name, description string
		parent            *uuid.UUID
	}
	calls []string
}

func (f *collectionStoreFake) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *collectionStoreFake) Collections(_ context.Context, userID uuid.UUID) ([]postgres.CollectionSummary, error) {
	f.record("list %s", userID)
	return f.summaries, f.err
}

func (f *collectionStoreFake) Collection(_ context.Context, userID, id uuid.UUID) (postgres.CollectionSummary, error) {
	f.record("get %s %s", userID, id)
	return f.summary, f.err
}

func (f *collectionStoreFake) CreateCollection(_ context.Context, userID uuid.UUID, name, description string, parent *uuid.UUID) (domain.Collection, error) {
	f.record("create %s", userID)
	f.created.name, f.created.description, f.created.parent = name, description, parent
	if f.err != nil {
		return domain.Collection{}, f.err
	}
	return domain.Collection{ID: uuid.New(), UserID: userID, Name: name, Description: description, ParentID: parent}, nil
}

func (f *collectionStoreFake) UpdateCollection(_ context.Context, userID, id uuid.UUID, name, description string) (domain.Collection, error) {
	f.record("update %s %s %s/%s", userID, id, name, description)
	if f.err != nil {
		return domain.Collection{}, f.err
	}
	return domain.Collection{ID: id, UserID: userID, Name: name, Description: description}, nil
}

func (f *collectionStoreFake) DeleteCollection(_ context.Context, userID, id uuid.UUID) error {
	f.record("delete %s %s", userID, id)
	return f.err
}

func (f *collectionStoreFake) AddToCollection(_ context.Context, userID, collectionID, workID uuid.UUID) error {
	f.record("add %s %s %s", userID, collectionID, workID)
	return f.err
}

func (f *collectionStoreFake) RemoveFromCollection(_ context.Context, userID, collectionID, workID uuid.UUID) error {
	f.record("remove %s %s %s", userID, collectionID, workID)
	return f.err
}

func (f *collectionStoreFake) Library(_ context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error) {
	f.query = q
	return f.library, f.libraryErr
}

func collectionRouter(f *collectionStoreFake) *Router {
	return &Router{Collections: &CollectionHandler{Store: f}}
}

func TestCollectionsNeedSomebodySignedIn(t *testing.T) {
	c, w := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/v1/collections", ""},
		{http.MethodPost, "/api/v1/collections", `{"name":"To read"}`},
		{http.MethodGet, "/api/v1/collections/" + c, ""},
		{http.MethodPatch, "/api/v1/collections/" + c, `{"name":"x"}`},
		{http.MethodDelete, "/api/v1/collections/" + c, ""},
		{http.MethodPut, "/api/v1/collections/" + c + "/works/" + w, ""},
		{http.MethodDelete, "/api/v1/collections/" + c + "/works/" + w, ""},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			f := &collectionStoreFake{}
			rec := serveAsViewer(t, collectionRouter(f), uuid.Nil, tc.method, tc.target, tc.body)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if len(f.calls) != 0 {
				t.Errorf("an anonymous request reached the store: %v", f.calls)
			}
		})
	}
}

func TestCollectionRequestsAreValidated(t *testing.T) {
	user, c := uuid.New(), uuid.NewString()
	for _, tc := range []struct{ name, method, target, body string }{
		{"detail: a malformed id", http.MethodGet, "/api/v1/collections/nope", ""},
		{"create: a body that is not JSON", http.MethodPost, "/api/v1/collections", `{"name":`},
		{"create: a parent that is not a UUID", http.MethodPost, "/api/v1/collections", `{"name":"x","parentId":"root"}`},
		{"create: a body over the limit", http.MethodPost, "/api/v1/collections", `{"name":"` + strings.Repeat("x", 9<<10) + `"}`},
		{"update: a malformed id", http.MethodPatch, "/api/v1/collections/nope", `{"name":"x"}`},
		{"update: a body that is not JSON", http.MethodPatch, "/api/v1/collections/" + c, `name`},
		{"delete: a malformed id", http.MethodDelete, "/api/v1/collections/nope", ""},
		{"add: a malformed work id", http.MethodPut, "/api/v1/collections/" + c + "/works/nope", ""},
		{"add: a malformed collection id", http.MethodPut, "/api/v1/collections/nope/works/" + uuid.NewString(), ""},
		{"remove: a malformed work id", http.MethodDelete, "/api/v1/collections/" + c + "/works/nope", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &collectionStoreFake{}
			rec := serveAsViewer(t, collectionRouter(f), user, tc.method, tc.target, tc.body)
			if rec.Code != http.StatusBadRequest || wireCodeOf(t, rec) != string(CodeInvalid) {
				t.Fatalf("status = %d, body = %s; want 400 invalid_request", rec.Code, rec.Body)
			}
			if len(f.calls) != 0 {
				t.Errorf("an invalid request reached the store: %v", f.calls)
			}
		})
	}
}

func TestCollectionsHappyPaths(t *testing.T) {
	user, c, work, parent := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	t.Run("list", func(t *testing.T) {
		f := &collectionStoreFake{summaries: []postgres.CollectionSummary{
			{Collection: domain.Collection{ID: c, Name: "Maths", Description: "proofs", CreatedAt: created, UpdatedAt: created}, Books: 4},
			{Collection: domain.Collection{ID: uuid.New(), Name: "Algebra", ParentID: &c}, Books: 1},
		}}
		rec := serveAsViewer(t, collectionRouter(f), user, http.MethodGet, "/api/v1/collections", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		out := decodeInto[dto.CollectionList](t, rec)
		if len(out.Items) != 2 {
			t.Fatalf("items = %+v", out.Items)
		}
		if got := out.Items[0]; got.ID != c.String() || got.Name != "Maths" || got.Description != "proofs" ||
			got.Books != 4 || got.ParentID != "" || !got.CreatedAt.Equal(created) {
			t.Errorf("first = %+v", got)
		}
		if got := out.Items[1]; got.ParentID != c.String() {
			t.Errorf("a nested collection lost its parent: %+v", got)
		}
		if f.calls[0] != "list "+user.String() {
			t.Errorf("calls = %v", f.calls)
		}
	})

	t.Run("an empty shelf of collections is an empty list", func(t *testing.T) {
		rec := serveAsViewer(t, collectionRouter(&collectionStoreFake{}), user, http.MethodGet, "/api/v1/collections", "")
		if !strings.Contains(rec.Body.String(), `"items":[]`) {
			t.Errorf("body = %s", rec.Body)
		}
	})

	t.Run("detail lists the books, read as the viewer", func(t *testing.T) {
		f := &collectionStoreFake{
			summary: postgres.CollectionSummary{Collection: domain.Collection{ID: c, Name: "Maths"}, Books: 1},
			library: postgres.LibraryPage{Items: []postgres.LibraryItem{{WorkID: work, Title: "Proofs from THE BOOK",
				Formats: []domain.Format{domain.FormatPDF}, BookFileID: uuid.New()}}},
		}
		rec := serveAsViewer(t, collectionRouter(f), user, http.MethodGet, "/api/v1/collections/"+c.String(), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
		}
		if f.query.ViewerID != user || f.query.CollectionID == nil || *f.query.CollectionID != c || f.query.Limit != maxCollectionBooks {
			t.Errorf("library asked %+v", f.query)
		}
		out := decodeInto[dto.CollectionDetail](t, rec)
		if out.ID != c.String() || out.Name != "Maths" || len(out.Items) != 1 || out.Items[0].WorkID != work.String() {
			t.Errorf("detail = %+v", out)
		}
	})

	t.Run("create, nested", func(t *testing.T) {
		f := &collectionStoreFake{}
		rec := serveAsViewer(t, collectionRouter(f), user, http.MethodPost, "/api/v1/collections",
			`{"name":"Linear algebra","description":"the good ones","parentId":"`+parent.String()+`"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
		}
		if f.created.name != "Linear algebra" || f.created.description != "the good ones" ||
			f.created.parent == nil || *f.created.parent != parent {
			t.Errorf("the store received %+v", f.created)
		}
		if out := decodeInto[dto.Collection](t, rec); out.Name != "Linear algebra" || out.ParentID != parent.String() || out.ID == "" {
			t.Errorf("response = %+v", out)
		}
	})

	t.Run("create, at the top", func(t *testing.T) {
		f := &collectionStoreFake{}
		rec := serveAsViewer(t, collectionRouter(f), user, http.MethodPost, "/api/v1/collections", `{"name":"To read"}`)
		if rec.Code != http.StatusCreated || f.created.parent != nil {
			t.Fatalf("status = %d, parent = %v", rec.Code, f.created.parent)
		}
	})

	t.Run("update", func(t *testing.T) {
		f := &collectionStoreFake{}
		rec := serveAsViewer(t, collectionRouter(f), user, http.MethodPatch, "/api/v1/collections/"+c.String(),
			`{"name":"Maths","description":"renamed"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if want := fmt.Sprintf("update %s %s Maths/renamed", user, c); f.calls[0] != want {
			t.Errorf("calls = %v, want %q", f.calls, want)
		}
		if out := decodeInto[dto.Collection](t, rec); out.Description != "renamed" {
			t.Errorf("response = %+v", out)
		}
	})

	for _, tc := range []struct {
		name, method, target, call string
	}{
		{"delete", http.MethodDelete, "/api/v1/collections/" + c.String(), fmt.Sprintf("delete %s %s", user, c)},
		{"add a work", http.MethodPut, "/api/v1/collections/" + c.String() + "/works/" + work.String(), fmt.Sprintf("add %s %s %s", user, c, work)},
		{"remove a work", http.MethodDelete, "/api/v1/collections/" + c.String() + "/works/" + work.String(), fmt.Sprintf("remove %s %s %s", user, c, work)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &collectionStoreFake{}
			rec := serveAsViewer(t, collectionRouter(f), user, tc.method, tc.target, "")
			if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("status = %d, body = %q; want an empty 204", rec.Code, rec.Body)
			}
			if len(f.calls) != 1 || f.calls[0] != tc.call {
				t.Errorf("calls = %v, want %q", f.calls, tc.call)
			}
		})
	}
}

// "No such collection" and "somebody else's collection" are the same answer.
func TestCollectionErrors(t *testing.T) {
	user, c, work := uuid.New(), uuid.NewString(), uuid.NewString()
	requests := []struct{ method, target, body string }{
		{http.MethodGet, "/api/v1/collections/" + c, ""},
		{http.MethodPatch, "/api/v1/collections/" + c, `{"name":"x"}`},
		{http.MethodDelete, "/api/v1/collections/" + c, ""},
		{http.MethodPut, "/api/v1/collections/" + c + "/works/" + work, ""},
		{http.MethodDelete, "/api/v1/collections/" + c + "/works/" + work, ""},
	}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"not found", fmt.Errorf("collection: %w", store.ErrNotFound), http.StatusNotFound, string(CodeNotFound)},
		{"a store failure", errors.New("boom"), http.StatusInternalServerError, string(CodeInternal)},
	} {
		for _, r := range requests {
			t.Run(tc.name+" "+r.method+" "+r.target, func(t *testing.T) {
				f := &collectionStoreFake{err: tc.err}
				rec := serveAsViewer(t, collectionRouter(f), user, r.method, r.target, r.body)
				if rec.Code != tc.status || wireCodeOf(t, rec) != tc.code {
					t.Fatalf("status = %d, body = %s; want %d %s", rec.Code, rec.Body, tc.status, tc.code)
				}
			})
		}
	}

	t.Run("list fails", func(t *testing.T) {
		rec := serveAsViewer(t, collectionRouter(&collectionStoreFake{err: errors.New("boom")}), user, http.MethodGet, "/api/v1/collections", "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("create fails", func(t *testing.T) {
		rec := serveAsViewer(t, collectionRouter(&collectionStoreFake{err: errors.New("boom")}), user, http.MethodPost, "/api/v1/collections", `{"name":"x"}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
	})
	t.Run("the books in it cannot be read", func(t *testing.T) {
		f := &collectionStoreFake{libraryErr: errors.New("boom")}
		rec := serveAsViewer(t, collectionRouter(f), user, http.MethodGet, "/api/v1/collections/"+c, "")
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}
