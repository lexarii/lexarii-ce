// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// fakeShelf is the account store for taking a book off a shelf: it releases
// what it is told to, and records what it was asked.
type fakeShelf struct {
	archived []postgres.ArchivedFile
	err      error
	released []uuid.UUID
	callers  []uuid.UUID
	recorded map[uuid.UUID]string
}

func (f *fakeShelf) DeleteAccount(context.Context, uuid.UUID, uuid.UUID) ([]postgres.ArchivedFile, error) {
	return nil, nil
}
func (f *fakeShelf) MoveArchived(_ context.Context, id uuid.UUID, from string,
	move func(string) (string, error)) (string, error) {
	to, err := move(from)
	if err != nil || to == "" {
		return "", err
	}
	if f.recorded == nil {
		f.recorded = map[uuid.UUID]string{}
	}
	f.recorded[id] = to
	return to, nil
}
func (f *fakeShelf) ArchivedBooks(context.Context, uuid.UUID) ([]postgres.ArchivedFile, error) {
	return nil, nil
}
func (f *fakeShelf) ClaimArchived(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error { return nil }
func (f *fakeShelf) ReleaseHolding(_ context.Context, file, user uuid.UUID) ([]postgres.ArchivedFile, error) {
	f.released = append(f.released, file)
	f.callers = append(f.callers, user)
	return f.archived, f.err
}

// fakeKeeper moves every file it is given into an archive directory.
type fakeKeeper struct{ kept []string }

func (k *fakeKeeper) Keep(path string) (string, error) {
	k.kept = append(k.kept, path)
	return "/archive/" + path, nil
}

// DELETE /books/{id}/holding: always the caller's own holding, 204 whether or
// not the book was archived - which would say whether anybody else holds it -
// and 404, never 403, for anything the caller does not hold.
func TestReleasingAHoldingOverHTTP(t *testing.T) {
	file := uuid.MustParse("f11e0000-0000-4000-8000-000000000001")
	caller := uuid.MustParse("ca11e400-0000-4000-8000-000000000002")

	for _, tc := range []struct {
		name     string
		path     string
		signedIn bool
		store    *fakeShelf
		want     int
		moved    int
	}{
		{"another holder remains", "/api/v1/books/" + file.String() + "/holding", true,
			&fakeShelf{}, http.StatusNoContent, 0},
		{"the last holder, a stored file", "/api/v1/books/" + file.String() + "/holding", true,
			&fakeShelf{archived: []postgres.ArchivedFile{{BookFileID: file, Path: "uploads/ab/x.epub"}}},
			http.StatusNoContent, 1},
		{"not theirs", "/api/v1/books/" + file.String() + "/holding", true,
			&fakeShelf{err: store.ErrNotFound}, http.StatusNotFound, 0},
		{"not a uuid", "/api/v1/books/not-a-uuid/holding", true,
			&fakeShelf{}, http.StatusBadRequest, 0},
		{"not signed in", "/api/v1/books/" + file.String() + "/holding", false,
			&fakeShelf{}, http.StatusUnauthorized, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keeper := &fakeKeeper{}
			rt := &Router{
				Health:   &HealthHandler{},
				Sharing:  &SharingHandler{},
				Accounts: &AccountHandler{Store: tc.store, Archive: keeper, Logger: quietLogger()},
			}
			req := httptest.NewRequest(http.MethodDelete, tc.path, nil)
			if tc.signedIn {
				req = asUserID(req, caller)
			}
			rec := httptest.NewRecorder()
			rt.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.want, rec.Body)
			}
			if tc.want == http.StatusNoContent && rec.Body.Len() != 0 {
				t.Errorf("a release said something: %s", rec.Body)
			}
			if len(keeper.kept) != tc.moved || len(tc.store.recorded) != tc.moved {
				t.Errorf("moved %d, recorded %d; want %d", len(keeper.kept), len(tc.store.recorded), tc.moved)
			}
			if tc.want == http.StatusBadRequest || tc.want == http.StatusUnauthorized {
				if len(tc.store.released) != 0 {
					t.Error("the store was asked anyway")
				}
				return
			}
			// Only ever on the caller's own behalf.
			if len(tc.store.callers) != 1 || tc.store.callers[0] != caller || tc.store.released[0] != file {
				t.Errorf("released %v for %v, want %s for %s", tc.store.released, tc.store.callers, file, caller)
			}
		})
	}
}
