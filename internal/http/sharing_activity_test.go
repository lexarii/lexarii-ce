// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

var errSharingDown = errors.New("sharing store unavailable")

// sharingActivitySpy is fakeSharing that also remembers the window and the
// owner the activity was read for.
type sharingActivitySpy struct {
	*fakeSharing
	owner uuid.UUID
	since time.Time
	limit int
}

func (s *sharingActivitySpy) SharingActivity(ctx context.Context, owner uuid.UUID, since time.Time, limit int) ([]postgres.ShareActivity, error) {
	s.owner, s.since, s.limit = owner, since, limit
	return s.fakeSharing.SharingActivity(ctx, owner, since, limit)
}

// What happened lately to the books somebody lent: a fortnight of it, theirs
// only, with the rows worth interrupting them for counted.
func TestSharingActivity(t *testing.T) {
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	file, work, reader := uuid.New(), uuid.New(), uuid.New()
	spy := &sharingActivitySpy{fakeSharing: &fakeSharing{activity: []postgres.ShareActivity{
		{BookFileID: file, WorkID: work, Title: "Dune", UserID: reader, Username: "bob", Day: day,
			Kind: postgres.AccessFile, Views: 1, Notable: true},
		{BookFileID: file, WorkID: work, Title: "Dune", UserID: reader, Username: "bob", Day: day,
			Kind: postgres.AccessPage, Views: 12, Units: 9, Of: 400},
	}}}
	h := &SharingHandler{Store: spy}
	owner := uuid.New()

	before := time.Now()
	rec := httptest.NewRecorder()
	h.Activity(rec, asUserID(httptest.NewRequest(http.MethodGet, "/api/v1/shares/activity", nil), owner))
	after := time.Now()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	if spy.owner != owner || spy.limit <= 0 {
		t.Errorf("read for %v limit %d, want the signed-in owner and a bound", spy.owner, spy.limit)
	}
	if spy.since.Before(before.Add(-activityWindow)) || spy.since.After(after.Add(-activityWindow)) {
		t.Errorf("since = %v, want a fortnight before the request", spy.since)
	}

	var out dto.SharingActivity
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Notable != 1 || len(out.Items) != 2 || !out.Since.Equal(spy.since) {
		t.Errorf("activity = %+v, want two items, one notable, and the window the store was asked for", out)
	}
	first := out.Items[0]
	if first.WorkTitle != "Dune" || first.Day != "2026-10-14" || first.Kind != "file" || !first.Notable ||
		first.WorkID != work.String() || first.Username != "bob" {
		t.Errorf("first = %+v", first)
	}
	if second := out.Items[1]; second.Units != 9 || second.Of != 400 || second.Views != 12 {
		t.Errorf("second = %+v", second)
	}
}

func TestTheAccessLogSaysWhoReadHowMuch(t *testing.T) {
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	reader := uuid.New()
	h := &SharingHandler{Store: &fakeSharing{access: []postgres.AccessRecord{
		{UserID: reader, Username: "bob", Day: day, Kind: postgres.AccessChapter, Views: 5, Units: 3, Of: 20},
	}}}
	rec := httptest.NewRecorder()
	h.AccessLog(rec, shareRequest(t, http.MethodGet, "/x", "", uuid.New(), uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out dto.AccessLog
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %+v", out.Items)
	}
	if got := out.Items[0]; got.UserID != reader.String() || got.Day != "2026-10-01" || got.Kind != "chapter" ||
		got.Units != 3 || got.Of != 20 {
		t.Errorf("record = %+v", got)
	}
}

func TestTheSharesOfAFile(t *testing.T) {
	reader := uuid.New()
	h := &SharingHandler{Store: &fakeSharing{shares: []postgres.Share{
		{UserID: reader, Username: "bob", CanDownload: true, Annotations: false},
	}}}
	rec := httptest.NewRecorder()
	h.List(rec, shareRequest(t, http.MethodGet, "/x", "", uuid.New(), uuid.Nil))
	var out dto.ShareList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].UserID != reader.String() || !out.Items[0].CanDownload || out.Items[0].Annotations {
		t.Errorf("shares = %+v", out.Items)
	}
}

// Who somebody shares their own notes on a book with.
func TestNoteSharesListTheirRecipients(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	h := &SharingHandler{Store: &fakeSharing{noteShares: []postgres.Person{{ID: a, Username: "ada"}, {ID: b, Username: "bo"}}}}
	rec := httptest.NewRecorder()
	h.NoteShares(rec, shareRequest(t, http.MethodGet, "/x", "", uuid.New(), uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out dto.NoteShareList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Items) != 2 || out.Items[0] != a.String() || out.Items[1] != b.String() {
		t.Errorf("items = %v", out.Items)
	}
	// Named too, for a client that has no list of people to look them up in.
	if len(out.People) != 2 || out.People[0].ID != a.String() || out.People[0].Username != "ada" ||
		out.People[1].Username != "bo" {
		t.Errorf("people = %+v", out.People)
	}

	rec = httptest.NewRecorder()
	(&SharingHandler{Store: &fakeSharing{}}).NoteShares(rec, shareRequest(t, http.MethodGet, "/x", "", uuid.New(), uuid.Nil))
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("nobody is not an empty array: %s", rec.Body)
	}
}

// Not yours and not there are the same 404, on every route; anything else
// the store says is a 500 that does not repeat it.
func TestSharingErrorsAcrossEveryRoute(t *testing.T) {
	file, person := uuid.New(), uuid.New()
	routes := func(h *SharingHandler) []struct {
		name string
		call http.HandlerFunc
		req  *http.Request
	} {
		return []struct {
			name string
			call http.HandlerFunc
			req  *http.Request
		}{
			{"people", h.People, shareRequest(t, http.MethodGet, "/x", "", file, uuid.Nil)},
			{"list", h.List, shareRequest(t, http.MethodGet, "/x", "", file, uuid.Nil)},
			{"put", h.Put, shareRequest(t, http.MethodPut, "/x", `{}`, file, person)},
			{"delete", h.Delete, shareRequest(t, http.MethodDelete, "/x", "", file, person)},
			{"access log", h.AccessLog, shareRequest(t, http.MethodGet, "/x", "", file, uuid.Nil)},
			{"activity", h.Activity, shareRequest(t, http.MethodGet, "/x", "", file, uuid.Nil)},
			{"note shares", h.NoteShares, shareRequest(t, http.MethodGet, "/x", "", file, uuid.Nil)},
			{"share notes", h.PutNoteShare, shareRequest(t, http.MethodPut, "/x", "", file, person)},
			{"unshare notes", h.DeleteNoteShare, shareRequest(t, http.MethodDelete, "/x", "", file, person)},
		}
	}

	for _, r := range routes(&SharingHandler{Store: &fakeSharing{err: errSharingDown}}) {
		t.Run(r.name+" store down", func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.call(rec, r.req)
			if rec.Code != http.StatusInternalServerError || authErrorCode(t, rec) != string(CodeInternal) {
				t.Fatalf("status = %d body = %s, want 500", rec.Code, rec.Body)
			}
		})
	}
	for _, r := range routes(&SharingHandler{Store: &fakeSharing{err: store.ErrNotFound}}) {
		if r.name == "people" {
			continue // a list of accounts has no book to be missing
		}
		t.Run(r.name+" not yours", func(t *testing.T) {
			rec := httptest.NewRecorder()
			r.call(rec, r.req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (never 403)", rec.Code)
			}
		})
	}
	t.Run("activity signed out", func(t *testing.T) {
		rec := httptest.NewRecorder()
		(&SharingHandler{Store: &fakeSharing{}}).Activity(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

func TestSharingRefusesMalformedRequests(t *testing.T) {
	fake := &fakeSharing{}
	h := &SharingHandler{Store: fake}
	for _, tc := range []struct {
		name         string
		call         http.HandlerFunc
		file, person string
		body         string
	}{
		{"a share with a malformed body", h.Put, uuid.NewString(), uuid.NewString(), `{"canDownload":`},
		{"unsharing with a bad person", h.Delete, uuid.NewString(), "nope", ""},
		{"note shares of a bad book", h.NoteShares, "nope", "", ""},
		{"sharing notes on a bad book", h.PutNoteShare, "nope", uuid.NewString(), ""},
		{"sharing notes with a bad person", h.PutNoteShare, uuid.NewString(), "nope", ""},
		{"unsharing notes from a bad person", h.DeleteNoteShare, uuid.NewString(), "nope", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body == "" {
				req = httptest.NewRequest(http.MethodPut, "/x", nil)
			} else {
				req = httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(tc.body))
			}
			req.SetPathValue("id", tc.file)
			req.SetPathValue("userId", tc.person)
			rec := httptest.NewRecorder()
			tc.call(rec, asUser(req))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
			}
		})
	}
	if fake.sharedWith != uuid.Nil || fake.unshared != uuid.Nil || fake.notesSharedWith != uuid.Nil ||
		fake.notesUnsharedFrom != uuid.Nil {
		t.Error("a malformed request reached the store")
	}
}

// Unsharing names the person whose access ends.
func TestUnsharingAFile(t *testing.T) {
	fake := &fakeSharing{}
	person := uuid.New()
	rec := httptest.NewRecorder()
	(&SharingHandler{Store: fake}).Delete(rec, shareRequest(t, http.MethodDelete, "/x", "", uuid.New(), person))
	if rec.Code != http.StatusNoContent || fake.unshared != person {
		t.Fatalf("status = %d unshared = %v, want 204 and %v", rec.Code, fake.unshared, person)
	}
}
