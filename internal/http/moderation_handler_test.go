// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

var errModerationDown = errors.New("moderation store unavailable")

// moderationHash is a well-formed content hash.
var moderationHash = strings.Repeat("ab", 32)

// moderationStoreFake is the moderation store without a database. Its
// discard hands the copies it was given to the handler's callback, as the
// real one does under the file's lock.
type moderationStoreFake struct {
	mu sync.Mutex

	file      domain.BookFile
	removedOf []uuid.UUID
	copies    postgres.StoredCopies
	withCopy  []string
	blocked   map[string]bool
	files     []postgres.BlockedFile

	fileErr      error
	removeErr    error
	discardErr   error
	withCopyErr  error
	isBlockedErr error
	listErr      error
	unblockErr   error

	removals  []string // reason
	purges    []bool
	discarded []string
	unblocked []string
}

func (s *moderationStoreFake) BookFile(context.Context, uuid.UUID) (domain.BookFile, error) {
	return s.file, s.fileErr
}
func (s *moderationStoreFake) RemoveBookFileForEveryone(_ context.Context, _, _ uuid.UUID, reason string,
	opts postgres.RemovalOptions) ([]uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removals = append(s.removals, reason)
	s.purges = append(s.purges, opts.PurgeQuotes)
	return s.removedOf, s.removeErr
}
func (s *moderationStoreFake) DiscardBlockedCopies(_ context.Context, hash string, discard func(postgres.StoredCopies) error) error {
	s.mu.Lock()
	s.discarded = append(s.discarded, hash)
	s.mu.Unlock()
	if s.discardErr != nil {
		return s.discardErr
	}
	return discard(s.copies)
}
func (s *moderationStoreFake) BlocksWithCopies(context.Context) ([]string, error) {
	return s.withCopy, s.withCopyErr
}
func (s *moderationStoreFake) IsBlocked(_ context.Context, hash string) (bool, error) {
	return s.blocked[hash], s.isBlockedErr
}
func (s *moderationStoreFake) BlockedFiles(context.Context, uuid.UUID) ([]postgres.BlockedFile, error) {
	return s.files, s.listErr
}
func (s *moderationStoreFake) Unblock(_ context.Context, _ uuid.UUID, hash string) error {
	s.unblocked = append(s.unblocked, hash)
	return s.unblockErr
}

// moderationDiscarder deletes what it is told to, and fails on the paths a
// test names.
type moderationDiscarder struct {
	mu        sync.Mutex
	extra     []string
	missing   map[string]bool
	failPath  map[string]bool
	failCover bool
	removed   []string
	covers    []string
}

func (d *moderationDiscarder) Copies(string) []string { return d.extra }
func (d *moderationDiscarder) Remove(path string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failPath[path] {
		return false, fmt.Errorf("removing %s: permission denied", path)
	}
	if d.missing[path] {
		return false, nil
	}
	d.removed = append(d.removed, path)
	return true, nil
}
func (d *moderationDiscarder) RemoveCover(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.covers = append(d.covers, name)
	if d.failCover {
		return errors.New("cover in use")
	}
	return nil
}

func moderationServe(t *testing.T, h *ModerationHandler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	(&Router{Health: &HealthHandler{}, Moderation: h}).Handler().ServeHTTP(rec, asUser(req))
	return rec
}

// Removing a file for everybody: the database half first, then every copy
// Lexarii stored - each once, however many places name it - and the cover;
// the people it was taken from are passed on to be told.
func TestRemovingAFileForEveryone(t *testing.T) {
	file := uuid.New()
	alice, bob := uuid.New(), uuid.New()
	s := &moderationStoreFake{
		file:      domain.BookFile{ID: file, ContentHash: moderationHash},
		removedOf: []uuid.UUID{alice, bob},
		copies:    postgres.StoredCopies{ContentHash: moderationHash, Paths: []string{"/up/a.epub", "/arch/a.epub"}, Cover: "c.jpg"},
	}
	d := &moderationDiscarder{extra: []string{"/up/a.epub", "/up/gone.epub"}, missing: map[string]bool{"/up/gone.epub": true}}
	var told []uuid.UUID
	var toldAbout uuid.UUID
	h := &ModerationHandler{Store: s, Accounts: anAdmin, Discard: d, Logger: quietLogger(),
		Notify: func(_ context.Context, id uuid.UUID, people []uuid.UUID) { toldAbout, told = id, people }}

	rec := moderationServe(t, h, http.MethodPost, "/api/v1/admin/books/"+file.String()+"/remove",
		`{"reason":"not ours to keep","purgeQuotes":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var out dto.RemovedForEveryone
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.RemovedFor != 2 {
		t.Errorf("removedFor = %d, want 2", out.RemovedFor)
	}
	if s.removals[0] != "not ours to keep" || !s.purges[0] {
		t.Errorf("removal = %q purge %v, want the reason and the purge passed on", s.removals[0], s.purges[0])
	}
	sort.Strings(d.removed)
	if strings.Join(d.removed, ",") != "/arch/a.epub,/up/a.epub" {
		t.Errorf("removed = %v, want each stored copy once", d.removed)
	}
	if len(d.covers) != 1 || d.covers[0] != "c.jpg" {
		t.Errorf("covers = %v", d.covers)
	}
	if toldAbout != file || len(told) != 2 {
		t.Errorf("told %v about %v, want both holders about the file", told, toldAbout)
	}
}

// Deleting a copy is best-effort once the removal has committed: a copy that
// will not go is reported in the log, and the request still succeeds.
func TestARemovalWhoseCopiesWillNotGoStillSucceeds(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    *moderationStoreFake
		d    *moderationDiscarder
	}{
		{"a copy refuses", &moderationStoreFake{copies: postgres.StoredCopies{Paths: []string{"/up/a.epub"}}},
			&moderationDiscarder{failPath: map[string]bool{"/up/a.epub": true}}},
		{"the cover refuses", &moderationStoreFake{}, &moderationDiscarder{failCover: true}},
		{"the store refuses", &moderationStoreFake{discardErr: errModerationDown}, &moderationDiscarder{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.s.file = domain.BookFile{ContentHash: moderationHash}
			h := &ModerationHandler{Store: tc.s, Accounts: anAdmin, Discard: tc.d}
			rec := moderationServe(t, h, http.MethodPost, "/api/v1/admin/books/"+uuid.NewString()+"/remove", `{"reason":"r"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: the removal itself happened", rec.Code)
			}
		})
	}
}

func TestRemovalRefusals(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct {
		name   string
		path   string
		body   string
		s      *moderationStoreFake
		status int
		code   string
	}{
		{"a bad id", "/api/v1/admin/books/nope/remove", `{"reason":"r"}`, &moderationStoreFake{},
			http.StatusBadRequest, string(CodeInvalid)},
		{"a malformed body", "/api/v1/admin/books/" + id + "/remove", `{"reason":`, &moderationStoreFake{},
			http.StatusBadRequest, string(CodeInvalid)},
		{"no such file", "/api/v1/admin/books/" + id + "/remove", `{"reason":"r"}`,
			&moderationStoreFake{fileErr: store.ErrNotFound}, http.StatusNotFound, string(CodeNotFound)},
		{"reading the file fails", "/api/v1/admin/books/" + id + "/remove", `{"reason":"r"}`,
			&moderationStoreFake{fileErr: errModerationDown}, http.StatusInternalServerError, string(CodeInternal)},
		{"no longer an administrator", "/api/v1/admin/books/" + id + "/remove", `{"reason":"r"}`,
			&moderationStoreFake{removeErr: fmt.Errorf("removing: %w", store.ErrForbidden)}, http.StatusForbidden, "not_an_admin"},
		{"the file kept changing", "/api/v1/admin/books/" + id + "/remove", `{"reason":"r"}`,
			&moderationStoreFake{removeErr: store.ErrBusy}, http.StatusServiceUnavailable, "try_again"},
		{"no reason given", "/api/v1/admin/books/" + id + "/remove", `{"reason":""}`,
			&moderationStoreFake{removeErr: fmt.Errorf("%w: a reason is required", store.ErrInvalidQuery)},
			http.StatusBadRequest, string(CodeInvalid)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := moderationServe(t, &ModerationHandler{Store: tc.s, Accounts: anAdmin}, http.MethodPost, tc.path, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if got := authErrorCode(t, rec); got != tc.code {
				t.Errorf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

// Finishing a removal whose copies were left behind: refused to a reader,
// a 404 for bytes that are not blocked, and otherwise a report of whether any
// copy remains.
func TestFinishingADiscard(t *testing.T) {
	path := "/api/v1/admin/blocked/" + moderationHash + "/discard"

	t.Run("a reader", func(t *testing.T) {
		rec := moderationServe(t, &ModerationHandler{Store: &moderationStoreFake{}, Accounts: aReader}, http.MethodPost, path, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	for _, tc := range []struct {
		name   string
		path   string
		s      *moderationStoreFake
		d      *moderationDiscarder
		status int
		remain bool
	}{
		{"not a hash", "/api/v1/admin/blocked/NOT-A-HASH/discard", &moderationStoreFake{}, nil, http.StatusBadRequest, false},
		{"not blocked", path, &moderationStoreFake{}, nil, http.StatusNotFound, false},
		{"cannot tell", path, &moderationStoreFake{isBlockedErr: errModerationDown}, nil, http.StatusInternalServerError, false},
		{"every copy goes", path, &moderationStoreFake{blocked: map[string]bool{moderationHash: true},
			copies: postgres.StoredCopies{Paths: []string{"/up/a.epub"}}}, &moderationDiscarder{}, http.StatusOK, false},
		{"a copy remains", path, &moderationStoreFake{blocked: map[string]bool{moderationHash: true},
			copies: postgres.StoredCopies{Paths: []string{"/up/a.epub"}}},
			&moderationDiscarder{failPath: map[string]bool{"/up/a.epub": true}}, http.StatusOK, true},
		{"nothing to delete with", path, &moderationStoreFake{blocked: map[string]bool{moderationHash: true}}, nil, http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &ModerationHandler{Store: tc.s, Accounts: anAdmin, Logger: quietLogger()}
			if tc.d != nil {
				h.Discard = tc.d
			}
			rec := moderationServe(t, h, http.MethodPost, tc.path, "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if tc.status != http.StatusOK {
				return
			}
			var out dto.DiscardedCopies
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if out.CopiesRemain != tc.remain {
				t.Errorf("copiesRemain = %v, want %v", out.CopiesRemain, tc.remain)
			}
		})
	}
}

// At startup, every blocked file with copies left is finished; one whose
// copies still will not go is left for the blocklist to offer.
func TestFinishingRemovalsAtStartup(t *testing.T) {
	other := strings.Repeat("cd", 32)
	s := &moderationStoreFake{withCopy: []string{moderationHash, other},
		copies: postgres.StoredCopies{Paths: []string{"/up/x.epub"}}}
	d := &moderationDiscarder{}
	h := &ModerationHandler{Store: s, Discard: d}

	h.FinishRemovals(t.Context())
	if strings.Join(s.discarded, ",") != moderationHash+","+other {
		t.Errorf("discarded = %v, want every hash with copies, in order", s.discarded)
	}

	t.Run("the list cannot be read", func(t *testing.T) {
		s := &moderationStoreFake{withCopyErr: errModerationDown}
		(&ModerationHandler{Store: s, Discard: &moderationDiscarder{}, Logger: quietLogger()}).FinishRemovals(t.Context())
		if len(s.discarded) != 0 {
			t.Errorf("discarded %v with no list", s.discarded)
		}
	})

	t.Run("shutdown stops it", func(t *testing.T) {
		s := &moderationStoreFake{withCopy: []string{moderationHash, other}}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		(&ModerationHandler{Store: s, Discard: &moderationDiscarder{}, Logger: quietLogger()}).FinishRemovals(ctx)
		if len(s.discarded) != 0 {
			t.Errorf("discarded %v after shutdown began", s.discarded)
		}
	})

	t.Run("a copy that will not go", func(t *testing.T) {
		s := &moderationStoreFake{withCopy: []string{moderationHash}, discardErr: errModerationDown}
		(&ModerationHandler{Store: s, Discard: &moderationDiscarder{}, Logger: quietLogger()}).FinishRemovals(t.Context())
		if len(s.discarded) != 1 {
			t.Errorf("discarded = %v, want it attempted", s.discarded)
		}
	})
}

func TestTheBlocklist(t *testing.T) {
	when := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	s := &moderationStoreFake{files: []postgres.BlockedFile{
		{ContentHash: moderationHash, Reason: "r", BlockedAt: when, BlockedBy: "adam", Title: "T", CopiesRemain: true},
	}}
	rec := moderationServe(t, &ModerationHandler{Store: s, Accounts: anAdmin}, http.MethodGet, "/api/v1/admin/blocked", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out dto.BlockedFiles
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].ContentHash != moderationHash || !out.Items[0].CopiesRemain ||
		out.Items[0].BlockedBy != "adam" || !out.Items[0].BlockedAt.Equal(when) {
		t.Errorf("items = %+v", out.Items)
	}

	failing := moderationServe(t, &ModerationHandler{Store: &moderationStoreFake{listErr: errModerationDown}, Accounts: anAdmin},
		http.MethodGet, "/api/v1/admin/blocked", "")
	if failing.Code != http.StatusInternalServerError {
		t.Errorf("store failure: status = %d, want 500", failing.Code)
	}
}

func TestLiftingABlock(t *testing.T) {
	s := &moderationStoreFake{}
	rec := moderationServe(t, &ModerationHandler{Store: s, Accounts: anAdmin, Logger: quietLogger()},
		http.MethodDelete, "/api/v1/admin/blocked/"+moderationHash, "")
	if rec.Code != http.StatusNoContent || len(s.unblocked) != 1 || s.unblocked[0] != moderationHash {
		t.Fatalf("status = %d unblocked = %v", rec.Code, s.unblocked)
	}

	for _, tc := range []struct {
		name   string
		hash   string
		err    error
		status int
	}{
		{"not a hash", "abc", nil, http.StatusBadRequest},
		{"upper-case hex is not the stored shape", strings.ToUpper(moderationHash), nil, http.StatusBadRequest},
		{"not blocked", moderationHash, store.ErrNotFound, http.StatusNotFound},
		{"the store fails", moderationHash, errModerationDown, http.StatusInternalServerError},
		{"no longer an administrator", moderationHash, store.ErrForbidden, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := moderationServe(t, &ModerationHandler{Store: &moderationStoreFake{unblockErr: tc.err}, Accounts: anAdmin},
				http.MethodDelete, "/api/v1/admin/blocked/"+tc.hash, "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
}
