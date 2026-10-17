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

// fakeLibrary is a hand-written LibraryReader. The interface is three methods;
// a fake keeps the tests readable and needs no mocking framework.
type fakeLibrary struct {
	page     postgres.LibraryPage
	detail   postgres.WorkDetail
	stats    postgres.LibraryStats
	subjects []postgres.Subject
	mentions []postgres.Mention
	edit     postgres.WorkEdit
	dupes    []postgres.DuplicateGroup
	merged   []uuid.UUID
	err      error
	lastQ    postgres.LibraryQuery
	lastID   uuid.UUID
	callLog  []string
}

func (f *fakeLibrary) Library(_ context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error) {
	f.lastQ = q
	f.callLog = append(f.callLog, "Library")
	return f.page, f.err
}

func (f *fakeLibrary) Work(_ context.Context, id, _ uuid.UUID) (postgres.WorkDetail, error) {
	f.lastID = id
	f.callLog = append(f.callLog, "Work")
	return f.detail, f.err
}

func (f *fakeLibrary) Subjects(context.Context, uuid.UUID) ([]postgres.Subject, error) {
	return f.subjects, f.err
}

func (f *fakeLibrary) Mentions(_ context.Context, id, _ uuid.UUID, _ int) ([]postgres.Mention, error) {
	f.lastID = id
	f.callLog = append(f.callLog, "Mentions")
	return f.mentions, f.err
}

func (f *fakeLibrary) UpdateWork(_ context.Context, id, _ uuid.UUID, edit postgres.WorkEdit) (postgres.WorkDetail, error) {
	f.lastID = id
	f.edit = edit
	f.callLog = append(f.callLog, "UpdateWork")
	return f.detail, f.err
}

func (f *fakeLibrary) Duplicates(context.Context, uuid.UUID) ([]postgres.DuplicateGroup, error) {
	f.callLog = append(f.callLog, "Duplicates")
	return f.dupes, f.err
}

func (f *fakeLibrary) MergeWorks(_ context.Context, _, keep uuid.UUID, merge []uuid.UUID) (int, error) {
	f.lastID = keep
	f.merged = merge
	f.callLog = append(f.callLog, "MergeWorks")
	return len(merge), f.err
}

func (f *fakeLibrary) Stats(context.Context, uuid.UUID) (postgres.LibraryStats, error) {
	f.callLog = append(f.callLog, "Stats")
	return f.stats, f.err
}

// serve drives the router as a signed-in user. Endpoints that change the
// shared library then check whether that user is an admin, which the handler's
// Accounts field decides.
func serve(t *testing.T, rt *Router, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, asUser(httptest.NewRequest(method, target, nil)))
	return rec
}

func TestLibraryListReturnsItems(t *testing.T) {
	workID, fileID := uuid.New(), uuid.New()
	pages := 412
	fake := &fakeLibrary{page: postgres.LibraryPage{
		Items: []postgres.LibraryItem{{
			WorkID: workID, Title: "The Art of Computer Programming",
			SortTitle: "Art of Computer Programming, The",
			Authors:   []string{"Donald Knuth"},
			Formats:   []domain.Format{domain.FormatPDF},
			Status:    domain.IngestReady, PageCount: &pages,
			BookFileID: fileID, AddedAt: time.Now(),
		}},
		NextCursor: "next-page-cursor",
	}}

	rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: fake}}
	rec := serve(t, rt, http.MethodGet, "/api/v1/library")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	var got dto.LibraryPage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v; body = %s", err, rec.Body)
	}
	if len(got.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(got.Items))
	}

	item := got.Items[0]
	if item.WorkID != workID.String() {
		t.Errorf("workId = %q", item.WorkID)
	}
	if item.Title != "The Art of Computer Programming" {
		t.Errorf("title = %q", item.Title)
	}
	if got.NextCursor != "next-page-cursor" {
		t.Errorf("nextCursor = %q", got.NextCursor)
	}
	// A cover URL is only offered when there is a cover to serve.
	if item.CoverURL != "" {
		t.Errorf("coverUrl = %q, want empty when no cover was extracted", item.CoverURL)
	}
}

// The client iterates authors unconditionally; a null would force a null check
// on every render.
func TestLibraryListEmitsEmptyArraysNotNull(t *testing.T) {
	fake := &fakeLibrary{page: postgres.LibraryPage{
		Items: []postgres.LibraryItem{{WorkID: uuid.New(), Title: "No Authors"}},
	}}
	rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: fake}}
	rec := serve(t, rt, http.MethodGet, "/api/v1/library")

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	items := raw["items"].([]any)
	first := items[0].(map[string]any)

	if first["authors"] == nil {
		t.Error("authors is null; it must be an empty array")
	}
	if first["formats"] == nil {
		t.Error("formats is null; it must be an empty array")
	}
}

func TestLibraryListPassesQueryParameters(t *testing.T) {
	authorID := uuid.New()
	fake := &fakeLibrary{}
	rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: fake}}

	target := fmt.Sprintf(
		"/api/v1/library?search=knuth&tag=maths&sort=added&limit=12&cursor=abc&includeMissing=true&status=ready&authorId=%s",
		authorID)
	if rec := serve(t, rt, http.MethodGet, target); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	q := fake.lastQ
	if q.Search != "knuth" {
		t.Errorf("Search = %q", q.Search)
	}
	if q.Tag != "maths" {
		t.Errorf("Tag = %q", q.Tag)
	}
	if q.Sort != postgres.SortAddedDesc {
		t.Errorf("Sort = %q", q.Sort)
	}
	if q.Limit != 12 {
		t.Errorf("Limit = %d", q.Limit)
	}
	if q.Cursor != "abc" {
		t.Errorf("Cursor = %q", q.Cursor)
	}
	if !q.IncludeMissing {
		t.Error("IncludeMissing = false")
	}
	if q.Status != domain.IngestReady {
		t.Errorf("Status = %q", q.Status)
	}
	if q.AuthorID == nil || *q.AuthorID != authorID {
		t.Errorf("AuthorID = %v", q.AuthorID)
	}
}

// A bad sort or cursor is the client's mistake. Reporting it as 500 would send
// the user hunting for a server fault that does not exist.
func TestLibraryListMapsBadQueryTo400(t *testing.T) {
	tests := map[string]struct {
		target string
		err    error
		want   int
	}{
		"unknown sort":    {"/api/v1/library?sort=nonsense", fmt.Errorf("%w: unknown sort", store.ErrInvalidQuery), http.StatusBadRequest},
		"bad cursor":      {"/api/v1/library?cursor=!!!", fmt.Errorf("%w: cursor", store.ErrInvalidQuery), http.StatusBadRequest},
		"bad limit":       {"/api/v1/library?limit=lots", nil, http.StatusBadRequest},
		"bad author id":   {"/api/v1/library?authorId=not-a-uuid", nil, http.StatusBadRequest},
		"genuine failure": {"/api/v1/library", errors.New("connection refused"), http.StatusInternalServerError},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeLibrary{err: tt.err}
			rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: fake}}
			rec := serve(t, rt, http.MethodGet, tt.target)

			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.want, rec.Body)
			}

			var body dto.Error
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding error body: %v", err)
			}
			if body.Error.Code == "" {
				t.Error("error has no machine-readable code")
			}
		})
	}
}

// Internal detail must not leak in an error message.
func TestUnexpectedErrorsDoNotLeakDetail(t *testing.T) {
	fake := &fakeLibrary{err: errors.New("pq: password authentication failed for user \"lexarii\"")}
	rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: fake}}
	rec := serve(t, rt, http.MethodGet, "/api/v1/library")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "password") {
		t.Fatalf("the response leaked internal detail: %s", body)
	}
}

func TestWorkDetail(t *testing.T) {
	workID, authorID, fileID := uuid.New(), uuid.New(), uuid.New()
	fake := &fakeLibrary{detail: postgres.WorkDetail{
		Work: domain.Work{
			ID: workID, Title: "Concrete Mathematics", SortTitle: "Concrete Mathematics",
			Authors: []domain.WorkAuthor{{
				Author: domain.Author{ID: authorID, Name: "Donald Knuth", SortName: "Knuth, Donald"},
				Role:   domain.RoleAuthor,
			}},
		},
		Files: []domain.BookFile{{
			ID: fileID, Format: domain.FormatPDF, SizeBytes: 12345,
			Status: domain.IngestReady, Path: "/library/cm.pdf",
		}},
		Warnings:   []string{`ignoring implausible embedded title "book.indb"`},
		Provenance: map[string]domain.MetadataSource{"title": domain.SourceEmbedded},
	}}

	rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: fake}}
	rec := serve(t, rt, http.MethodGet, "/api/v1/works/"+workID.String())

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if fake.lastID != workID {
		t.Errorf("store was asked for %s, want %s", fake.lastID, workID)
	}

	var got dto.WorkDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Title != "Concrete Mathematics" {
		t.Errorf("title = %q", got.Title)
	}
	if len(got.Authors) != 1 || got.Authors[0].SortName != "Knuth, Donald" {
		t.Errorf("authors = %+v", got.Authors)
	}
	if len(got.Files) != 1 || got.Files[0].Format != "pdf" {
		t.Errorf("files = %+v", got.Files)
	}
	// The UI must be able to explain where a value came from, and to surface
	// what the parser chose to ignore.
	if got.Provenance["title"] != "embedded" {
		t.Errorf("provenance = %v", got.Provenance)
	}
	if len(got.Warnings) != 1 {
		t.Errorf("warnings = %v", got.Warnings)
	}
}

// Where a file sits on the server's disk is for administrators. Everybody
// else - a reader who scanned it before losing their rights, a borrower, or
// anybody when the admin check cannot be made - gets no path at all.
func TestWorkDetailShowsThePathOnlyToAdministrators(t *testing.T) {
	workID := uuid.New()
	detail := postgres.WorkDetail{
		Work: domain.Work{ID: workID, Title: "Concrete Mathematics"},
		Files: []domain.BookFile{{
			ID: uuid.New(), Format: domain.FormatPDF, Status: domain.IngestReady, Path: "/srv/library/alice/cm.pdf",
		}},
	}
	for _, tc := range []struct {
		name     string
		accounts AdminStore
		want     string
	}{
		{"an administrator", anAdmin, "/srv/library/alice/cm.pdf"},
		{"anybody else", aReader, ""},
		{"no account check", nil, ""},
		{"a failed account check", jobAdminLookupFails{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeLibrary{detail: detail}
			rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: fake, Accounts: tc.accounts}}
			rec := serve(t, rt, http.MethodGet, "/api/v1/works/"+workID.String())
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			var got dto.WorkDetail
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if len(got.Files) != 1 || got.Files[0].Path != tc.want {
				t.Errorf("files = %+v, want path %q", got.Files, tc.want)
			}
			if tc.want == "" && strings.Contains(rec.Body.String(), "/srv/library") {
				t.Errorf("the path went out on the wire: %s", rec.Body)
			}
		})
	}
}

// A mention reaches the page with the locator intact, because a backlink that
// opens the cover of a nine-hundred-page reference is a gesture, not a
// reference.
func TestMentionsCarryTheLocator(t *testing.T) {
	workID, citing, fileID := uuid.New(), uuid.New(), uuid.New()
	fake := &fakeLibrary{mentions: []postgres.Mention{{
		WorkID:     citing,
		BookFileID: fileID,
		Title:      "bash Cookbook",
		Authors:    []string{"Carl Albing"},
		Locator:    domain.Locator{Format: domain.FormatPDF, Page: 212, CharOffset: 40},
		Excerpt:    "Learning the bash Shell, 3rd Edition, by Cameron Newham",
		Places:     15,
	}}}

	rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: fake}}
	rec := serve(t, rt, http.MethodGet, "/api/v1/works/"+workID.String()+"/mentions")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if fake.lastID != workID {
		t.Errorf("store was asked for %s, want %s", fake.lastID, workID)
	}

	var out dto.MentionList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("got %d mentions, want 1", len(out.Items))
	}
	if out.Items[0].Locator != "pdf:212:40" {
		t.Errorf("locator = %q, want pdf:212:40", out.Items[0].Locator)
	}
	if out.Items[0].Places != 15 {
		t.Errorf("places = %d, want 15", out.Items[0].Places)
	}
}

func TestWorkDetailNotFound(t *testing.T) {
	fake := &fakeLibrary{err: store.ErrNotFound}
	rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: fake}}

	rec := serve(t, rt, http.MethodGet, "/api/v1/works/"+uuid.New().String())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	var body dto.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Error.Code != string(CodeNotFound) {
		t.Errorf("code = %q, want %q", body.Error.Code, CodeNotFound)
	}
}

func TestWorkDetailRejectsBadID(t *testing.T) {
	rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: &fakeLibrary{}}}
	rec := serve(t, rt, http.MethodGet, "/api/v1/works/not-a-uuid")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestStatsEndpoint(t *testing.T) {
	fake := &fakeLibrary{stats: postgres.LibraryStats{
		Works: 167, Files: 167, Authors: 102, Pages: 72006,
		ByFormat: map[domain.Format]int{domain.FormatPDF: 167},
	}}
	rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: fake}}
	rec := serve(t, rt, http.MethodGet, "/api/v1/stats")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got dto.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got.Works != 167 || got.Pages != 72006 {
		t.Errorf("stats = %+v", got)
	}
	if got.ByFormat["pdf"] != 167 {
		t.Errorf("byFormat = %v", got.ByFormat)
	}
}

// A panicking handler must not take the server down with it.
func TestPanicIsContained(t *testing.T) {
	rt := &Router{
		Health:  &HealthHandler{},
		Library: &LibraryHandler{Store: panickingLibrary{}},
	}
	rec := serve(t, rt, http.MethodGet, "/api/v1/library")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Error("the panic value leaked to the client")
	}
}

type panickingLibrary struct{}

func (panickingLibrary) Library(context.Context, postgres.LibraryQuery) (postgres.LibraryPage, error) {
	panic("boom")
}
func (panickingLibrary) Work(context.Context, uuid.UUID, uuid.UUID) (postgres.WorkDetail, error) {
	panic("boom")
}
func (panickingLibrary) Subjects(context.Context, uuid.UUID) ([]postgres.Subject, error) {
	panic("should not be called")
}

func (panickingLibrary) Mentions(context.Context, uuid.UUID, uuid.UUID, int) ([]postgres.Mention, error) {
	panic("should not be called")
}

func (panickingLibrary) UpdateWork(context.Context, uuid.UUID, uuid.UUID, postgres.WorkEdit) (postgres.WorkDetail, error) {
	panic("should not be called")
}

func (panickingLibrary) Duplicates(context.Context, uuid.UUID) ([]postgres.DuplicateGroup, error) {
	panic("should not be called")
}

func (panickingLibrary) MergeWorks(context.Context, uuid.UUID, uuid.UUID, []uuid.UUID) (int, error) {
	panic("should not be called")
}

func (panickingLibrary) Stats(context.Context, uuid.UUID) (postgres.LibraryStats, error) {
	panic("boom")
}
