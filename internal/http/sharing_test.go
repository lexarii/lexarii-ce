// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// fakeSharing records what the handlers asked the store to do.
//
// The store's own rules - only the owner may grant, a grantee cannot re-share -
// are tested against a real database in internal/store/postgres. What is tested
// here is the layer above: that the HTTP surface passes the right arguments,
// keeps "not yours" indistinguishable from "does not exist", and refuses
// nonsense before it reaches SQL.
type fakeSharing struct {
	people []postgres.Person
	shares []postgres.Share
	access []postgres.AccessRecord

	err error

	sharedWith  uuid.UUID
	canDownload bool
	annotations bool
	unshared    uuid.UUID
	activity    []postgres.ShareActivity

	noteShares        []postgres.Person
	notesSharedWith   uuid.UUID
	notesUnsharedFrom uuid.UUID

	// named is every account PersonNamed knows, by exact lower-case name.
	named     map[string]postgres.Person
	askedName string
	// lookups counts PersonNamed calls; mu guards it and askedName, for
	// tests that ask concurrently.
	mu      sync.Mutex
	lookups int
	// notEntitled makes MayLend and MayShareNotes answer not-found: the
	// caller neither holds nor can read the book.
	notEntitled bool
}

func (f *fakeSharing) MayLend(context.Context, uuid.UUID, uuid.UUID) error {
	if f.notEntitled {
		return store.ErrNotFound
	}
	return nil
}

func (f *fakeSharing) MayShareNotes(context.Context, uuid.UUID, uuid.UUID) error {
	if f.notEntitled {
		return store.ErrNotFound
	}
	return nil
}

func (f *fakeSharing) PersonNamed(_ context.Context, username string) (postgres.Person, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	f.askedName = username
	if person, ok := f.named[strings.ToLower(strings.TrimSpace(username))]; ok {
		return person, nil
	}
	return postgres.Person{}, store.ErrNotFound
}

func (f *fakeSharing) People(context.Context, uuid.UUID) ([]postgres.Person, error) {
	return f.people, f.err
}

func (f *fakeSharing) BookShares(context.Context, uuid.UUID, uuid.UUID) ([]postgres.Share, error) {
	return f.shares, f.err
}

func (f *fakeSharing) ShareBook(_ context.Context, _, _, viewer uuid.UUID,
	canDownload, annotations bool) error {
	f.sharedWith, f.canDownload, f.annotations = viewer, canDownload, annotations
	return f.err
}

func (f *fakeSharing) UnshareBook(_ context.Context, _, _, viewer uuid.UUID) error {
	f.unshared = viewer
	return f.err
}

func (f *fakeSharing) BookAccessLog(context.Context, uuid.UUID, uuid.UUID) ([]postgres.AccessRecord, error) {
	return f.access, f.err
}

func (f *fakeSharing) SharingActivity(context.Context, uuid.UUID, time.Time, int) ([]postgres.ShareActivity, error) {
	return f.activity, f.err
}

func (f *fakeSharing) ShareAnnotations(_ context.Context, _, _, viewer uuid.UUID) error {
	f.notesSharedWith = viewer
	return f.err
}

func (f *fakeSharing) UnshareAnnotations(_ context.Context, _, _, viewer uuid.UUID) error {
	f.notesUnsharedFrom = viewer
	return f.err
}

func (f *fakeSharing) AnnotationShares(context.Context, uuid.UUID, uuid.UUID) ([]postgres.Person, error) {
	return f.noteShares, f.err
}

func shareRequest(t *testing.T, method, target, body string, book, person uuid.UUID) *http.Request {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.SetPathValue("id", book.String())
	if person != uuid.Nil {
		r.SetPathValue("userId", person.String())
	}
	return asUser(r)
}

func TestSharingRequiresSignIn(t *testing.T) {
	h := &SharingHandler{Store: &fakeSharing{}}
	book := uuid.New()

	for _, c := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{"people", h.People, httptest.NewRequest(http.MethodGet, "/api/v1/people", nil)},
		{"list", h.List, httptest.NewRequest(http.MethodGet, "/x", nil)},
		{"put", h.Put, httptest.NewRequest(http.MethodPut, "/x", strings.NewReader("{}"))},
		{"delete", h.Delete, httptest.NewRequest(http.MethodDelete, "/x", nil)},
		{"access log", h.AccessLog, httptest.NewRequest(http.MethodGet, "/x", nil)},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.req.SetPathValue("id", book.String())
			rec := httptest.NewRecorder()
			c.call(rec, c.req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// A book that is not yours must be indistinguishable from one that does not
// exist. "You may not share this" confirms it is there.
func TestSharingHidesBooksThatAreNotYours(t *testing.T) {
	h := &SharingHandler{Store: &fakeSharing{err: store.ErrNotFound}}
	book, person := uuid.New(), uuid.New()

	cases := []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
		req  *http.Request
	}{
		{"list", h.List, shareRequest(t, http.MethodGet, "/x", "", book, uuid.Nil)},
		{"put", h.Put, shareRequest(t, http.MethodPut, "/x", `{"canDownload":false}`, book, person)},
		{"delete", h.Delete, shareRequest(t, http.MethodDelete, "/x", "", book, person)},
		{"access log", h.AccessLog, shareRequest(t, http.MethodGet, "/x", "", book, uuid.Nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.call(rec, c.req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (never 403)", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "forbidden") {
				t.Fatalf("the refusal admits the book exists: %s", rec.Body)
			}
		})
	}
}

// The permissions in the body are what reaches the store, and both default to
// false: a share that quietly allowed downloading would be the surprise.
func TestShareRequestCarriesThePermissions(t *testing.T) {
	book, person := uuid.New(), uuid.New()

	tests := []struct {
		body        string
		download    bool
		annotations bool
	}{
		{`{}`, false, false},
		{`{"canDownload":true}`, true, false},
		{`{"annotations":true}`, false, true},
		{`{"canDownload":true,"annotations":true}`, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			fake := &fakeSharing{}
			h := &SharingHandler{Store: fake}
			rec := httptest.NewRecorder()
			h.Put(rec, shareRequest(t, http.MethodPut, "/x", tt.body, book, person))

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body)
			}
			if fake.sharedWith != person {
				t.Fatalf("shared with %v, want %v", fake.sharedWith, person)
			}
			if fake.canDownload != tt.download || fake.annotations != tt.annotations {
				t.Fatalf("permissions = download:%v annotations:%v, want %v/%v",
					fake.canDownload, fake.annotations, tt.download, tt.annotations)
			}
		})
	}
}

func TestSharingRejectsMalformedIDs(t *testing.T) {
	h := &SharingHandler{Store: &fakeSharing{}}

	r := asUser(httptest.NewRequest(http.MethodPut, "/x", strings.NewReader("{}")))
	r.SetPathValue("id", "not-a-uuid")
	r.SetPathValue("userId", uuid.NewString())
	rec := httptest.NewRecorder()
	h.Put(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad book id gave %d, want 400", rec.Code)
	}

	r = asUser(httptest.NewRequest(http.MethodPut, "/x", strings.NewReader("{}")))
	r.SetPathValue("id", uuid.NewString())
	r.SetPathValue("userId", "not-a-uuid")
	rec = httptest.NewRecorder()
	h.Put(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a bad person id gave %d, want 400", rec.Code)
	}
}

// The share picker hands out usernames and nothing else.
func TestPeopleExposesOnlyNames(t *testing.T) {
	fake := &fakeSharing{people: []postgres.Person{
		{ID: uuid.New(), Username: "someone", IsAdmin: false},
	}}
	h := &SharingHandler{Store: fake}

	rec := httptest.NewRecorder()
	h.People(rec, asUser(httptest.NewRequest(http.MethodGet, "/api/v1/people", nil)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	items, _ := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v", items)
	}
	person, _ := items[0].(map[string]any)
	for _, leaked := range []string{"email", "passwordHash", "createdAt"} {
		if _, present := person[leaked]; present {
			t.Fatalf("the share picker exposes %q: %v", leaked, person)
		}
	}
	if person["username"] != "someone" {
		t.Fatalf("username missing: %v", person)
	}
}

// An empty list is an empty array, never null: a client should not have to
// special-case "nobody has read this yet".
func TestEmptyListsAreArrays(t *testing.T) {
	h := &SharingHandler{Store: &fakeSharing{}}
	book := uuid.New()

	for name, call := range map[string]func(http.ResponseWriter, *http.Request){
		"shares":     h.List,
		"access log": h.AccessLog,
		"people":     h.People,
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			call(rec, shareRequest(t, http.MethodGet, "/x", "", book, uuid.Nil))
			if !strings.Contains(rec.Body.String(), `"items":[]`) {
				t.Fatalf("empty list is not an array: %s", rec.Body)
			}
		})
	}
}

// Sharing your own notes is not the book owner's decision, so these routes do
// not go through the ownership check the book shares use.
func TestNoteSharesAreScopedToWhoeverWroteThem(t *testing.T) {
	book, person := uuid.New(), uuid.New()
	fake := &fakeSharing{}
	h := &SharingHandler{Store: fake}

	rec := httptest.NewRecorder()
	h.PutNoteShare(rec, shareRequest(t, http.MethodPut, "/x", "", book, person))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body)
	}
	if fake.notesSharedWith != person {
		t.Fatalf("shared with %v, want %v", fake.notesSharedWith, person)
	}

	rec = httptest.NewRecorder()
	h.DeleteNoteShare(rec, shareRequest(t, http.MethodDelete, "/x", "", book, person))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if fake.notesUnsharedFrom != person {
		t.Fatalf("unshared from %v, want %v", fake.notesUnsharedFrom, person)
	}
}

// A book nobody may read is indistinguishable from one that does not exist,
// here as everywhere else.
func TestNoteSharingHidesBooksYouCannotRead(t *testing.T) {
	h := &SharingHandler{Store: &fakeSharing{err: store.ErrNotFound}}
	book, person := uuid.New(), uuid.New()

	rec := httptest.NewRecorder()
	h.PutNoteShare(rec, shareRequest(t, http.MethodPut, "/x", "", book, person))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "forbidden") {
		t.Fatalf("the refusal admits the book exists: %s", rec.Body)
	}
}

func TestNoteSharesRequireSignIn(t *testing.T) {
	h := &SharingHandler{Store: &fakeSharing{}}
	for _, c := range []struct {
		name string
		call func(http.ResponseWriter, *http.Request)
	}{
		{"list", h.NoteShares},
		{"put", h.PutNoteShare},
		{"delete", h.DeleteNoteShare},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/x", nil)
			r.SetPathValue("id", uuid.NewString())
			r.SetPathValue("userId", uuid.NewString())
			rec := httptest.NewRecorder()
			c.call(rec, r)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}
