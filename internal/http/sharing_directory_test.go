// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// With people unlisted, the list of accounts goes to administrators only.
// Everybody else gets an empty list - not an error, so a client that does not
// know the setting says "nobody to share with" rather than failing.
func TestPeopleAreListedOnlyToAdministratorsWhenUnlisted(t *testing.T) {
	everybody := []postgres.Person{{ID: uuid.New(), Username: "ada"}, {ID: uuid.New(), Username: "bo"}}
	for _, tc := range []struct {
		name     string
		unlisted bool
		accounts AdminStore
		want     int
		status   int
	}{
		{"listed, to anybody", false, aReader, 2, http.StatusOK},
		{"unlisted, to an administrator", true, anAdmin, 2, http.StatusOK},
		{"unlisted, to anybody else", true, aReader, 0, http.StatusOK},
		{"unlisted, with no account check", true, nil, 0, http.StatusOK},
		{"unlisted, when the account check fails", true, jobAdminLookupFails{}, 0, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &SharingHandler{Store: &fakeSharing{people: everybody}, Unlisted: tc.unlisted, Accounts: tc.accounts}
			rec := httptest.NewRecorder()
			h.People(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/v1/people", nil)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if tc.status != http.StatusOK {
				return
			}
			var out dto.PersonList
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if out.Items == nil || len(out.Items) != tc.want {
				t.Errorf("items = %+v, want %d (and an array, never null)", out.Items, tc.want)
			}
		})
	}
}

// Lending by username: resolved on the server, granted exactly as by id, and
// answered with the share so the client can change or end it by id later.
func TestLendingByUsername(t *testing.T) {
	ada := postgres.Person{ID: uuid.New(), Username: "Ada"}
	fake := &fakeSharing{named: map[string]postgres.Person{"ada": ada}}
	h := &SharingHandler{Store: fake, Unlisted: true, Accounts: aReader}

	rec := httptest.NewRecorder()
	h.PostByUsername(rec, shareRequest(t, http.MethodPost, "/x",
		`{"username":" ADA ","canDownload":true,"annotations":false}`, uuid.New(), uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if fake.sharedWith != ada.ID || !fake.canDownload || fake.annotations {
		t.Errorf("shared with %s (download %v, notes %v), want Ada with download only",
			fake.sharedWith, fake.canDownload, fake.annotations)
	}
	var got dto.Share
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.UserID != ada.ID.String() || got.Username != "Ada" || !got.CanDownload {
		t.Errorf("share = %+v, want Ada's, as she spells it", got)
	}
}

// A name nobody has is 404 user_not_found - usernames are not secret here -
// and never reaches the grant. A book that is not the caller's is the same
// plain not-found it always was, whoever it was to be lent to.
func TestLendingByUsernameRefusals(t *testing.T) {
	ada := postgres.Person{ID: uuid.New(), Username: "ada"}
	for _, tc := range []struct {
		name   string
		body   string
		err    error
		status int
		code   string
	}{
		{"nobody by that name", `{"username":"nobody"}`, nil, http.StatusNotFound, string(codeUserNotFound)},
		{"no name at all", `{"username":"  "}`, nil, http.StatusBadRequest, string(CodeInvalid)},
		{"not a body", `{`, nil, http.StatusBadRequest, string(CodeInvalid)},
		{"not the caller's book", `{"username":"ada"}`, store.ErrNotFound, http.StatusNotFound, string(CodeNotFound)},
		{"the caller themselves", `{"username":"ada"}`, store.ErrInvalidQuery, http.StatusBadRequest, string(CodeInvalid)},
		{"the store is down", `{"username":"ada"}`, errors.New("down"), http.StatusInternalServerError, string(CodeInternal)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSharing{named: map[string]postgres.Person{"ada": ada}, err: tc.err}
			h := &SharingHandler{Store: fake}
			for _, call := range []struct {
				route string
				serve http.HandlerFunc
			}{{"lend", h.PostByUsername}, {"share notes", h.PostNoteShareByUsername}} {
				rec := httptest.NewRecorder()
				call.serve(rec, shareRequest(t, http.MethodPost, "/x", tc.body, uuid.New(), uuid.Nil))
				if rec.Code != tc.status || authErrorCode(t, rec) != tc.code {
					t.Errorf("%s: status = %d body = %s, want %d %s", call.route, rec.Code, rec.Body, tc.status, tc.code)
				}
			}
			if tc.code == string(codeUserNotFound) && (fake.sharedWith != uuid.Nil || fake.notesSharedWith != uuid.Nil) {
				t.Error("an unknown name reached the grant")
			}
		})
	}
}

// Without a book to share, a username is never looked up: the answer is the
// book's plain not-found whether the name exists or not, so a request naming
// any id at all is no way to learn who has an account.
func TestSharingByUsernameChecksTheBookFirst(t *testing.T) {
	ada := postgres.Person{ID: uuid.New(), Username: "ada"}
	for _, name := range []string{"ada", "nobody"} {
		fake := &fakeSharing{named: map[string]postgres.Person{"ada": ada}, notEntitled: true}
		h := &SharingHandler{Store: fake, Unlisted: true, UnknownNames: auth.NewThrottle()}
		for _, call := range []struct {
			route string
			serve http.HandlerFunc
		}{{"lend", h.PostByUsername}, {"share notes", h.PostNoteShareByUsername}} {
			rec := httptest.NewRecorder()
			call.serve(rec, shareRequest(t, http.MethodPost, "/x", `{"username":"`+name+`"}`, uuid.New(), uuid.Nil))
			if rec.Code != http.StatusNotFound || authErrorCode(t, rec) != string(CodeNotFound) {
				t.Errorf("%s naming %q with no book: status = %d body = %s, want the book's 404",
					call.route, name, rec.Code, rec.Body)
			}
		}
		if fake.askedName != "" {
			t.Errorf("naming %q with no book looked the name up", name)
		}
		if fake.sharedWith != uuid.Nil || fake.notesSharedWith != uuid.Nil {
			t.Errorf("naming %q with no book reached a grant", name)
		}
	}
}

// Names that match nobody are counted per person, across both routes. Past
// the limit every name is refused with 429 - a real one too, so the refusal
// itself says nothing - without being looked up; a hit in between resets
// nothing; and somebody else is not held up by it.
func TestUnknownNamesAreLimited(t *testing.T) {
	ada := postgres.Person{ID: uuid.New(), Username: "ada"}
	fake := &fakeSharing{named: map[string]postgres.Person{"ada": ada}}
	limit := auth.NewThrottle()
	limit.MaxAttempts = 3
	h := &SharingHandler{Store: fake, Unlisted: true, UnknownNames: limit}
	guesser, other := uuid.New(), uuid.New()

	ask := func(serve http.HandlerFunc, who uuid.UUID, name string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		r := shareRequest(t, http.MethodPost, "/x", `{"username":"`+name+`"}`, uuid.New(), uuid.Nil)
		serve(rec, asUserID(r, who))
		return rec
	}
	for i, step := range []struct {
		serve  http.HandlerFunc
		name   string
		status int
	}{
		{h.PostByUsername, "nobody1", http.StatusNotFound},
		{h.PostByUsername, "ada", http.StatusOK},
		{h.PostNoteShareByUsername, "nobody2", http.StatusNotFound},
		{h.PostByUsername, "ada", http.StatusOK},
		{h.PostByUsername, "nobody3", http.StatusNotFound},
	} {
		if rec := ask(step.serve, guesser, step.name); rec.Code != step.status {
			t.Fatalf("step %d naming %q: status = %d, want %d; body = %s", i, step.name, rec.Code, step.status, rec.Body)
		}
	}

	for _, serve := range []http.HandlerFunc{h.PostByUsername, h.PostNoteShareByUsername} {
		for _, name := range []string{"ada", "nobody4"} {
			fake.askedName = ""
			rec := ask(serve, guesser, name)
			if rec.Code != http.StatusTooManyRequests || authErrorCode(t, rec) != string(codeTooManyUnknownNames) {
				t.Errorf("past the limit, naming %q: status = %d body = %s, want 429", name, rec.Code, rec.Body)
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Error("a 429 without Retry-After")
			}
			if fake.askedName != "" {
				t.Errorf("past the limit, %q was still looked up", name)
			}
		}
	}

	if rec := ask(h.PostByUsername, other, "ada"); rec.Code != http.StatusOK {
		t.Errorf("somebody else: status = %d, want 200; body = %s", rec.Code, rec.Body)
	}
}

// Concurrent guesses cannot outrun the limit: counted before each lookup, in
// one step with the check, so of a hundred unknown names asked at once
// exactly the limit are looked up and the rest are refused.
func TestUnknownNamesAreLimitedUnderConcurrency(t *testing.T) {
	fake := &fakeSharing{named: map[string]postgres.Person{}}
	limit := auth.NewThrottle()
	limit.MaxAttempts = 8
	h := &SharingHandler{Store: fake, Unlisted: true, UnknownNames: limit}
	guesser := uuid.New()

	const asks = 100
	codes := make(chan int, asks)
	var wg sync.WaitGroup
	for i := range asks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			serve := h.PostByUsername
			if i%2 == 1 {
				serve = h.PostNoteShareByUsername
			}
			rec := httptest.NewRecorder()
			r := shareRequest(t, http.MethodPost, "/x", fmt.Sprintf(`{"username":"nobody%d"}`, i), uuid.New(), uuid.Nil)
			serve(rec, asUserID(r, guesser))
			codes <- rec.Code
		}()
	}
	wg.Wait()
	close(codes)

	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if fake.lookups != limit.MaxAttempts || counts[http.StatusNotFound] != limit.MaxAttempts ||
		counts[http.StatusTooManyRequests] != asks-limit.MaxAttempts {
		t.Errorf("looked up %d, answered %v; want exactly %d looked up and answered 404, the rest 429",
			fake.lookups, counts, limit.MaxAttempts)
	}
}

// Sharing your notes by username, for a borrower who has no list either.
func TestSharingNotesByUsername(t *testing.T) {
	bo := postgres.Person{ID: uuid.New(), Username: "bo"}
	fake := &fakeSharing{named: map[string]postgres.Person{"bo": bo}}
	h := &SharingHandler{Store: fake, Unlisted: true}

	rec := httptest.NewRecorder()
	h.PostNoteShareByUsername(rec, shareRequest(t, http.MethodPost, "/x", `{"username":"bo"}`, uuid.New(), uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body)
	}
	if fake.notesSharedWith != bo.ID {
		t.Errorf("notes shared with %s, want bo", fake.notesSharedWith)
	}
	var got dto.Recipient
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.ID != bo.ID.String() || got.Username != "bo" {
		t.Errorf("recipient = %+v", got)
	}
}

// The client learns which picker to show from /auth/me.
func TestMeSaysWhetherPeopleAreListed(t *testing.T) {
	for _, tc := range []struct {
		unlisted, admin, want bool
	}{
		{false, false, true},
		{false, true, true},
		{true, true, true},
		{true, false, false},
	} {
		f := newAuthFixture(t)
		f.h.PeopleUnlisted = tc.unlisted
		f.store.admin = tc.admin
		rec := httptest.NewRecorder()
		f.h.Me(rec, asUserID(httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil), f.store.userID))
		var u dto.User
		if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		if u.CanListPeople != tc.want {
			t.Errorf("unlisted=%v admin=%v: canListPeople = %v, want %v", tc.unlisted, tc.admin, u.CanListPeople, tc.want)
		}
	}
}
