// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

var errAccountsDown = errors.New("account store unavailable")

// accountsFakeStore is the account store without a database. It records who
// asked for what, and moves archived files through the keeper it is handed
// exactly as the real one does.
type accountsFakeStore struct {
	mu sync.Mutex

	archived  []postgres.ArchivedFile
	listed    []postgres.ArchivedFile
	deleteErr error
	listErr   error
	claimErr  error
	// moveErr fails the move of this one file.
	moveErr  map[uuid.UUID]error
	deletes  [][2]uuid.UUID // actor, target
	claims   [][3]uuid.UUID // admin, file, new owner
	listedBy []uuid.UUID
}

func (s *accountsFakeStore) DeleteAccount(_ context.Context, actor, target uuid.UUID) ([]postgres.ArchivedFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, [2]uuid.UUID{actor, target})
	return s.archived, s.deleteErr
}

func (s *accountsFakeStore) MoveArchived(_ context.Context, id uuid.UUID, from string,
	move func(string) (string, error)) (string, error) {
	if err := s.moveErr[id]; err != nil {
		return "", err
	}
	return move(from)
}

func (s *accountsFakeStore) ArchivedBooks(_ context.Context, admin uuid.UUID) ([]postgres.ArchivedFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listedBy = append(s.listedBy, admin)
	return s.listed, s.listErr
}

func (s *accountsFakeStore) ClaimArchived(_ context.Context, admin, file, owner uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims = append(s.claims, [3]uuid.UUID{admin, file, owner})
	return s.claimErr
}

func (s *accountsFakeStore) ReleaseHolding(context.Context, uuid.UUID, uuid.UUID) ([]postgres.ArchivedFile, error) {
	return nil, s.deleteErr
}

// accountsKeeper keeps files that Lexarii stored and leaves the rest where
// they are, as ingest.Archive does with the library directory.
type accountsKeeper struct{ kept []string }

func (k *accountsKeeper) Keep(path string) (string, error) {
	if strings.HasPrefix(path, "/library/") {
		return "", nil
	}
	k.kept = append(k.kept, path)
	return "/archive" + path, nil
}

func accountsRouter(s *accountsFakeStore, keeper BookKeeper) *Router {
	return &Router{
		Health:   &HealthHandler{},
		Sharing:  &SharingHandler{Store: &fakeSharing{}},
		Accounts: &AccountHandler{Store: s, Archive: keeper, Logger: quietLogger()},
	}
}

func accountsServe(t *testing.T, rt *Router, user uuid.UUID, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if user != uuid.Nil {
		req = asUserID(req, user)
	}
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

// Deleting an account reports how many of its books were archived and how
// many of those Lexarii could move; a file it could not move is still
// archived, and the deletion still happened.
func TestDeletingAnAccountArchivesItsFiles(t *testing.T) {
	stored, scanned, stuck := uuid.New(), uuid.New(), uuid.New()
	s := &accountsFakeStore{
		archived: []postgres.ArchivedFile{
			{BookFileID: stored, Path: "/data/uploads/ab/x.epub"},
			{BookFileID: scanned, Path: "/library/kept/y.pdf"},
			{BookFileID: stuck, Path: "/data/uploads/cd/z.epub"},
		},
		moveErr: map[uuid.UUID]error{stuck: errAccountsDown},
	}
	keeper := &accountsKeeper{}
	admin, target := uuid.New(), uuid.New()

	rec := accountsServe(t, accountsRouter(s, keeper), admin, http.MethodDelete, "/api/v1/users/"+target.String(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var out dto.AccountDeleted
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.BooksArchived != 3 || out.FilesMoved != 1 {
		t.Errorf("result = %+v, want 3 archived and only the stored file moved", out)
	}
	if len(keeper.kept) != 1 || keeper.kept[0] != "/data/uploads/ab/x.epub" {
		t.Errorf("kept = %v, want the library directory left alone", keeper.kept)
	}
	if s.deletes[0] != [2]uuid.UUID{admin, target} {
		t.Errorf("delete = %v, want the signed-in actor and the path's target", s.deletes[0])
	}
}

// With nowhere to archive to, nothing is moved and the deletion still stands.
func TestDeletingAnAccountWithNoArchive(t *testing.T) {
	s := &accountsFakeStore{archived: []postgres.ArchivedFile{{BookFileID: uuid.New(), Path: "/data/x.epub"}}}
	rt := accountsRouter(s, nil)
	rt.Accounts.Logger = nil // the default logger is used when none is given

	rec := accountsServe(t, rt, uuid.New(), http.MethodDelete, "/api/v1/users/"+uuid.NewString(), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out dto.AccountDeleted
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.BooksArchived != 1 || out.FilesMoved != 0 {
		t.Errorf("result = %+v", out)
	}
}

func TestAccountRefusals(t *testing.T) {
	file := uuid.NewString()
	for _, tc := range []struct {
		name         string
		method, path string
		body         string
		signedIn     bool
		setup        func(*accountsFakeStore)
		status       int
		code         string
	}{
		{"delete signed out", http.MethodDelete, "/api/v1/users/" + uuid.NewString(), "", false, nil,
			http.StatusUnauthorized, "not_signed_in"},
		{"delete a bad id", http.MethodDelete, "/api/v1/users/nope", "", true, nil,
			http.StatusBadRequest, string(CodeInvalid)},
		{"delete somebody else as a reader", http.MethodDelete, "/api/v1/users/" + uuid.NewString(), "", true,
			func(s *accountsFakeStore) { s.deleteErr = fmt.Errorf("deleting: %w", store.ErrForbidden) },
			http.StatusForbidden, "not_an_admin"},
		{"delete the last administrator", http.MethodDelete, "/api/v1/users/" + uuid.NewString(), "", true,
			func(s *accountsFakeStore) {
				s.deleteErr = fmt.Errorf("%w: the only administrator", store.ErrInvalidQuery)
			},
			http.StatusBadRequest, string(CodeInvalid)},
		{"delete nobody", http.MethodDelete, "/api/v1/users/" + uuid.NewString(), "", true,
			func(s *accountsFakeStore) { s.deleteErr = store.ErrNotFound },
			http.StatusNotFound, string(CodeNotFound)},
		{"delete when the store fails", http.MethodDelete, "/api/v1/users/" + uuid.NewString(), "", true,
			func(s *accountsFakeStore) { s.deleteErr = errAccountsDown },
			http.StatusInternalServerError, string(CodeInternal)},
		{"archive signed out", http.MethodGet, "/api/v1/archive", "", false, nil,
			http.StatusUnauthorized, "not_signed_in"},
		{"archive as a reader", http.MethodGet, "/api/v1/archive", "", true,
			func(s *accountsFakeStore) { s.listErr = store.ErrForbidden },
			http.StatusForbidden, "not_an_admin"},
		{"archive when the store fails", http.MethodGet, "/api/v1/archive", "", true,
			func(s *accountsFakeStore) { s.listErr = errAccountsDown },
			http.StatusInternalServerError, string(CodeInternal)},
		{"claim signed out", http.MethodPost, "/api/v1/archive/" + file + "/claim", "", false, nil,
			http.StatusUnauthorized, "not_signed_in"},
		{"claim a bad id", http.MethodPost, "/api/v1/archive/nope/claim", "", true, nil,
			http.StatusBadRequest, string(CodeInvalid)},
		{"claim with malformed JSON", http.MethodPost, "/api/v1/archive/" + file + "/claim", `{"ownerId":`, true, nil,
			http.StatusBadRequest, string(CodeInvalid)},
		{"claim for a bad owner id", http.MethodPost, "/api/v1/archive/" + file + "/claim", `{"ownerId":"nope"}`, true, nil,
			http.StatusBadRequest, string(CodeInvalid)},
		{"claim as a reader", http.MethodPost, "/api/v1/archive/" + file + "/claim", "", true,
			func(s *accountsFakeStore) { s.claimErr = store.ErrForbidden },
			http.StatusForbidden, "not_an_admin"},
		{"claim a book that is not archived", http.MethodPost, "/api/v1/archive/" + file + "/claim", "", true,
			func(s *accountsFakeStore) { s.claimErr = store.ErrNotFound },
			http.StatusNotFound, string(CodeNotFound)},
		{"release a bad id", http.MethodDelete, "/api/v1/books/nope/holding", "", true, nil,
			http.StatusBadRequest, string(CodeInvalid)},
		{"release when the store fails", http.MethodDelete, "/api/v1/books/" + file + "/holding", "", true,
			func(s *accountsFakeStore) { s.deleteErr = errAccountsDown },
			http.StatusInternalServerError, string(CodeInternal)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &accountsFakeStore{}
			if tc.setup != nil {
				tc.setup(s)
			}
			user := uuid.Nil
			if tc.signedIn {
				user = uuid.New()
			}
			rec := accountsServe(t, accountsRouter(s, &accountsKeeper{}), user, tc.method, tc.path, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if got := authErrorCode(t, rec); got != tc.code {
				t.Errorf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestTheArchiveListsWhatWasLeftBehind(t *testing.T) {
	kept, left := uuid.New(), uuid.New()
	when := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	s := &accountsFakeStore{listed: []postgres.ArchivedFile{
		{BookFileID: kept, Title: "Moved", Format: "epub", ArchivedOwner: "gone", ArchivedAt: when, ArchivedPath: "/archive/x"},
		{BookFileID: left, Title: "Stayed", Format: "pdf", ArchivedOwner: "gone", ArchivedAt: when},
	}}
	admin := uuid.New()
	rec := accountsServe(t, accountsRouter(s, nil), admin, http.MethodGet, "/api/v1/archive", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out dto.ArchivedBooks
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Items) != 2 || !out.Items[0].Kept || out.Items[1].Kept || out.Items[0].LeftBy != "gone" ||
		out.Items[0].BookFileID != kept.String() || !out.Items[0].ArchivedAt.Equal(when) {
		t.Errorf("items = %+v, want both, with which one was moved", out.Items)
	}
	if s.listedBy[0] != admin {
		t.Errorf("listed for %v, want the signed-in person, so the store can check them", s.listedBy[0])
	}
}

// A claim with no body is "give it to me"; a named owner gets it instead.
func TestClaimingAnArchivedFile(t *testing.T) {
	file, other := uuid.New(), uuid.New()
	for _, tc := range []struct {
		name string
		body string
		want func(admin uuid.UUID) uuid.UUID
	}{
		{"no body", "", func(admin uuid.UUID) uuid.UUID { return admin }},
		{"an empty owner", `{}`, func(admin uuid.UUID) uuid.UUID { return admin }},
		{"somebody named", `{"ownerId":"` + other.String() + `"}`, func(uuid.UUID) uuid.UUID { return other }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &accountsFakeStore{}
			admin := uuid.New()
			rec := accountsServe(t, accountsRouter(s, nil), admin, http.MethodPost,
				"/api/v1/archive/"+file.String()+"/claim", tc.body)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			if got := s.claims[0]; got != [3]uuid.UUID{admin, file, tc.want(admin)} {
				t.Errorf("claim = %v, want admin %v file %v owner %v", got, admin, file, tc.want(admin))
			}
		})
	}
}

// Against the real store: what the documentation promises for deleting an
// account and reclaiming its books, including the two not-founds that once
// came back as 500s.
func TestAccountsAgainstTheDatabase(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	p := e2ePool(t, url)
	admin, reader, other := e2eUser(t, p, true), e2eUser(t, p, false), e2eUser(t, p, false)
	rt := &Router{
		Health:   &HealthHandler{},
		Sharing:  &SharingHandler{Store: p},
		Accounts: &AccountHandler{Store: p, Logger: quietLogger()},
	}

	for _, tc := range []struct {
		name         string
		as           uuid.UUID
		method, path string
		body         string
		status       int
	}{
		{"a reader may not delete somebody else", reader, http.MethodDelete, "/api/v1/users/" + other.String(), "", http.StatusForbidden},
		{"an administrator deleting nobody", admin, http.MethodDelete, "/api/v1/users/" + uuid.NewString(), "", http.StatusNotFound},
		{"a reader may not see the archive", reader, http.MethodGet, "/api/v1/archive", "", http.StatusForbidden},
		{"a reader may not claim", reader, http.MethodPost, "/api/v1/archive/" + uuid.NewString() + "/claim", "", http.StatusForbidden},
		{"claiming a book that is not archived", admin, http.MethodPost, "/api/v1/archive/" + uuid.NewString() + "/claim", "", http.StatusNotFound},
		{"releasing a book you do not hold", reader, http.MethodDelete, "/api/v1/books/" + uuid.NewString() + "/holding", "", http.StatusNotFound},
		{"the archive, empty", admin, http.MethodGet, "/api/v1/archive", "", http.StatusOK},
		{"anybody may delete themselves", other, http.MethodDelete, "/api/v1/users/" + other.String(), "", http.StatusOK},
		{"and then they are gone", admin, http.MethodDelete, "/api/v1/users/" + other.String(), "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := accountsServe(t, rt, tc.as, tc.method, tc.path, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
		})
	}

	var exists bool
	if err := p.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, reader).Scan(&exists); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if !exists {
		t.Error("a refused deletion deleted the account anyway")
	}
}
