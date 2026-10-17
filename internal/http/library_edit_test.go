// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
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

var errLibraryDown = errors.New("library store unavailable")

// libraryServe sends one request through the router, signed in or not, with a
// body when there is one.
func libraryServe(t *testing.T, fake *fakeLibrary, method, target, body string, signedIn bool) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if signedIn {
		req = asUser(req)
	}
	rec := httptest.NewRecorder()
	rt := &Router{Health: &HealthHandler{}, Library: &LibraryHandler{Store: fake}}
	rt.Handler().ServeHTTP(rec, req)
	return rec
}

// Every library route answers somebody who is not signed in with 401, before
// anything is read.
func TestLibraryRoutesNeedASignedInUser(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, "/api/v1/library", ""},
		{http.MethodGet, "/api/v1/stats", ""},
		{http.MethodGet, "/api/v1/subjects", ""},
		{http.MethodGet, "/api/v1/works/" + id, ""},
		{http.MethodGet, "/api/v1/works/" + id + "/mentions", ""},
		{http.MethodPatch, "/api/v1/works/" + id, `{"title":"x"}`},
		{http.MethodGet, "/api/v1/duplicates", ""},
		{http.MethodPost, "/api/v1/works/" + id + "/merge", `{"merge":[]}`},
	} {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			fake := &fakeLibrary{}
			rec := libraryServe(t, fake, tc.method, tc.target, tc.body, false)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			if len(fake.callLog) != 0 {
				t.Errorf("an anonymous request reached the store: %v", fake.callLog)
			}
		})
	}
}

// The ownership filter: "mine" and "shared" are passed on, and a value the
// server does not know is refused rather than silently showing everything.
func TestTheOwnedFilter(t *testing.T) {
	for _, tc := range []struct {
		query  string
		status int
		want   postgres.Ownership
	}{
		{"", http.StatusOK, postgres.OwnedAny},
		{"?owned=mine", http.StatusOK, postgres.OwnedMine},
		{"?owned=shared", http.StatusOK, postgres.OwnedShared},
		{"?owned=everyone", http.StatusBadRequest, ""},
	} {
		t.Run(tc.query, func(t *testing.T) {
			fake := &fakeLibrary{}
			rec := libraryServe(t, fake, http.MethodGet, "/api/v1/library"+tc.query, "", true)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if tc.status == http.StatusOK && fake.lastQ.Owned != tc.want {
				t.Errorf("Owned = %q, want %q", fake.lastQ.Owned, tc.want)
			}
			if tc.status != http.StatusOK && len(fake.callLog) != 0 {
				t.Error("an unknown filter still reached the store")
			}
		})
	}
}

// The cursor a page hands out is the one the next request carries.
func TestTheLibraryCursorRoundTrips(t *testing.T) {
	fake := &fakeLibrary{page: postgres.LibraryPage{
		Items:      []postgres.LibraryItem{{WorkID: uuid.New(), Title: "First"}},
		NextCursor: "opaque-cursor/2",
	}}
	rec := libraryServe(t, fake, http.MethodGet, "/api/v1/library?limit=1", "", true)
	var page dto.LibraryPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if page.NextCursor != "opaque-cursor/2" {
		t.Fatalf("nextCursor = %q", page.NextCursor)
	}

	libraryServe(t, fake, http.MethodGet, "/api/v1/library?limit=1&cursor="+
		strings.ReplaceAll(page.NextCursor, "/", "%2F"), "", true)
	if fake.lastQ.Cursor != "opaque-cursor/2" || fake.lastQ.Limit != 1 {
		t.Errorf("second request carried cursor %q limit %d", fake.lastQ.Cursor, fake.lastQ.Limit)
	}
}

// A shelf card links to the file's cover, and counts pages for a PDF and
// sections for an EPUB.
func TestAShelfCardNamesItsCoverAndUnits(t *testing.T) {
	file := uuid.New()
	fake := &fakeLibrary{page: postgres.LibraryPage{Items: []postgres.LibraryItem{
		{WorkID: uuid.New(), Title: "With cover", BookFileID: file, CoverPath: "covers/x.jpg",
			Formats: []domain.Format{domain.FormatEPUB}},
		{WorkID: uuid.New(), Title: "No cover", Formats: []domain.Format{domain.FormatCBZ}},
	}}}
	rec := libraryServe(t, fake, http.MethodGet, "/api/v1/library", "", true)
	var page dto.LibraryPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if got := page.Items[0].CoverURL; got != "/api/v1/books/"+file.String()+"/cover" {
		t.Errorf("coverUrl = %q, want the file's cover route", got)
	}
	if page.Items[1].CoverURL != "" {
		t.Errorf("a book with no cover has coverUrl %q", page.Items[1].CoverURL)
	}
	if page.Items[0].UnitType != "section" || page.Items[1].UnitType != "" {
		t.Errorf("unit types = %q, %q; want section and nothing", page.Items[0].UnitType, page.Items[1].UnitType)
	}
}

func TestSubjects(t *testing.T) {
	fake := &fakeLibrary{subjects: []postgres.Subject{
		{Name: "Mathematics", Slug: "mathematics", Books: 48},
		{Name: "Distributed systems", Slug: "distributed-systems", Books: 3},
	}}
	rec := libraryServe(t, fake, http.MethodGet, "/api/v1/subjects", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out dto.SubjectList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Items) != 2 || out.Items[0] != (dto.Subject{Name: "Mathematics", Slug: "mathematics", Books: 48}) {
		t.Errorf("items = %+v, want both, commonest first as the store gave them", out.Items)
	}

	empty := libraryServe(t, &fakeLibrary{}, http.MethodGet, "/api/v1/subjects", "", true)
	if !strings.Contains(empty.Body.String(), `"items":[]`) {
		t.Errorf("no subjects is not an empty array: %s", empty.Body)
	}
}

// Store failures are 500s with nothing of the cause in them; a work the
// viewer cannot see is 404, never 403.
func TestLibraryStoreErrors(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct {
		method, target, body string
		err                  error
		status               int
	}{
		{http.MethodGet, "/api/v1/subjects", "", errLibraryDown, http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/stats", "", errLibraryDown, http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/duplicates", "", errLibraryDown, http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/works/" + id, "", errLibraryDown, http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/works/" + id + "/mentions", "", errLibraryDown, http.StatusInternalServerError},
		{http.MethodGet, "/api/v1/works/" + id + "/mentions", "", store.ErrNotFound, http.StatusNotFound},
		{http.MethodPatch, "/api/v1/works/" + id, `{"title":"x"}`, errLibraryDown, http.StatusInternalServerError},
		{http.MethodPatch, "/api/v1/works/" + id, `{"title":"x"}`, store.ErrNotFound, http.StatusNotFound},
		{http.MethodPost, "/api/v1/works/" + id + "/merge", `{"merge":[]}`, errLibraryDown, http.StatusInternalServerError},
		{http.MethodPost, "/api/v1/works/" + id + "/merge", `{"merge":[]}`, store.ErrNotFound, http.StatusNotFound},
	} {
		t.Run(fmt.Sprintf("%s %s %v", tc.method, tc.target, tc.err), func(t *testing.T) {
			rec := libraryServe(t, &fakeLibrary{err: tc.err}, tc.method, tc.target, tc.body, true)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "unavailable") {
				t.Errorf("the cause leaked: %s", rec.Body)
			}
		})
	}
}

// Bad ids and bad bodies are the client's mistake.
func TestLibraryRefusesMalformedRequests(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct{ name, method, target, body string }{
		{"mentions of a bad id", http.MethodGet, "/api/v1/works/nope/mentions", ""},
		{"edit a bad id", http.MethodPatch, "/api/v1/works/nope", `{"title":"x"}`},
		{"edit with malformed JSON", http.MethodPatch, "/api/v1/works/" + id, `{"title":`},
		{"merge into a bad id", http.MethodPost, "/api/v1/works/nope/merge", `{"merge":[]}`},
		{"merge malformed JSON", http.MethodPost, "/api/v1/works/" + id + "/merge", `[`},
		{"merge a bad id in", http.MethodPost, "/api/v1/works/" + id + "/merge", `{"merge":["` + uuid.NewString() + `","nope"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeLibrary{}
			rec := libraryServe(t, fake, tc.method, tc.target, tc.body, true)
			if rec.Code != http.StatusBadRequest || authErrorCode(t, rec) != string(CodeInvalid) {
				t.Fatalf("status = %d body = %s, want 400 invalid_request", rec.Code, rec.Body)
			}
			if len(fake.callLog) != 0 {
				t.Errorf("a malformed request reached the store: %v", fake.callLog)
			}
		})
	}

	t.Run("a correction the store refuses", func(t *testing.T) {
		fake := &fakeLibrary{err: fmt.Errorf("%w: an ISBN-13 has thirteen digits", store.ErrInvalidQuery)}
		rec := libraryServe(t, fake, http.MethodPatch, "/api/v1/works/"+id, `{"isbn13":"123"}`, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("a merge the store refuses", func(t *testing.T) {
		fake := &fakeLibrary{err: fmt.Errorf("%w: a work cannot be merged into itself", store.ErrInvalidQuery)}
		rec := libraryServe(t, fake, http.MethodPost, "/api/v1/works/"+id+"/merge", `{"merge":["`+id+`"]}`, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})
}

// A correction says exactly what was sent: a field left out is untouched, a
// field sent empty is cleared, and the author list is replaced whole.
func TestACorrectionReachesTheStoreAsSent(t *testing.T) {
	workID := uuid.New()
	fake := &fakeLibrary{detail: postgres.WorkDetail{Work: domain.Work{ID: workID, Title: "Corrected"}}}

	rec := libraryServe(t, fake, http.MethodPatch, "/api/v1/works/"+workID.String(),
		`{"title":"Corrected","subtitle":"","authors":["Kernighan, Brian","Pike, Rob"],"year":1999,"seriesIndex":2}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if fake.lastID != workID {
		t.Errorf("edited %v, want %v", fake.lastID, workID)
	}
	e := fake.edit
	if e.Title == nil || *e.Title != "Corrected" {
		t.Errorf("title = %v", e.Title)
	}
	if e.Subtitle == nil || *e.Subtitle != "" {
		t.Errorf("subtitle = %v, want present and empty: a clear", e.Subtitle)
	}
	if e.Series != nil || e.Description != nil || e.ISBN13 != nil {
		t.Error("fields left out of the body were not left alone")
	}
	if e.Authors == nil || len(*e.Authors) != 2 || (*e.Authors)[1] != "Pike, Rob" {
		t.Errorf("authors = %v, want both in order", e.Authors)
	}
	if e.Year == nil || *e.Year != 1999 || e.SeriesIndex == nil || *e.SeriesIndex != 2 {
		t.Errorf("year %v seriesIndex %v", e.Year, e.SeriesIndex)
	}

	var out dto.WorkDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.ID != workID.String() || out.Title != "Corrected" {
		t.Errorf("reply = %+v, want the work as it now is", out)
	}
}

func TestDuplicates(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	fake := &fakeLibrary{dupes: []postgres.DuplicateGroup{{
		Reason: "same title and author",
		Works: []postgres.DuplicateWork{
			{WorkID: a, Title: "SICP", Authors: []string{"Abelson"}, Formats: []string{"pdf"}, Files: 1, Annotations: 12, HasProgress: true},
			{WorkID: b, Title: "SICP", Files: 1},
		},
	}}}
	rec := libraryServe(t, fake, http.MethodGet, "/api/v1/duplicates", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out dto.DuplicateList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].Reason != "same title and author" || len(out.Items[0].Works) != 2 {
		t.Fatalf("items = %+v", out.Items)
	}
	first := out.Items[0].Works[0]
	if first.WorkID != a.String() || first.Annotations != 12 || !first.HasProgress || first.Formats[0] != "pdf" {
		t.Errorf("first = %+v, want what says which copy was read", first)
	}

	empty := libraryServe(t, &fakeLibrary{}, http.MethodGet, "/api/v1/duplicates", "", true)
	if !strings.Contains(empty.Body.String(), `"items":[]`) {
		t.Errorf("no duplicates is not an empty array: %s", empty.Body)
	}
}

// Merging keeps the work in the path and moves the others into it.
func TestMergingWorks(t *testing.T) {
	keep, other1, other2 := uuid.New(), uuid.New(), uuid.New()
	fake := &fakeLibrary{}
	rec := libraryServe(t, fake, http.MethodPost, "/api/v1/works/"+keep.String()+"/merge",
		`{"merge":["`+other1.String()+`","`+other2.String()+`"]}`, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if fake.lastID != keep || len(fake.merged) != 2 || fake.merged[0] != other1 || fake.merged[1] != other2 {
		t.Errorf("kept %v merged %v", fake.lastID, fake.merged)
	}
	var out dto.MergeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Kept != keep.String() || out.FilesMoved != 2 {
		t.Errorf("result = %+v", out)
	}
}

// A work's page shows its editions, its tags with where each came from, and,
// per file, the name this viewer gave it and whether it is theirs to remove.
func TestTheWorkPageCarriesEditionsTagsAndPerViewerFileFacts(t *testing.T) {
	published := time.Date(1978, 2, 22, 0, 0, 0, 0, time.UTC)
	pages := 272
	epub, pdf := uuid.New(), uuid.New()
	fake := &fakeLibrary{detail: postgres.WorkDetail{
		Work: domain.Work{ID: uuid.New(), Title: "The C Programming Language",
			Editions: []domain.Edition{{ID: uuid.New(), ISBN13: "9780131101630", Publisher: "Prentice Hall",
				PublishedOn: &published, PageCount: &pages, Language: "en"}}},
		Files: []domain.BookFile{
			{ID: epub, Format: domain.FormatEPUB},
			{ID: pdf, Format: domain.FormatPDF},
		},
		// Shorter than Files on purpose: a missing entry is "no", never a
		// panic.
		Filenames:    []string{"k&r.epub"},
		Held:         []bool{true},
		Downloadable: []bool{true},
		Tags:         []domain.Tag{{ID: uuid.New(), Name: "C", Slug: "c", IsSubject: true, Source: domain.SourceLLM}},
	}}
	rec := libraryServe(t, fake, http.MethodGet, "/api/v1/works/"+uuid.NewString(), "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var out dto.WorkDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Editions) != 1 || out.Editions[0].ISBN13 != "9780131101630" || out.Editions[0].PageCount == nil ||
		*out.Editions[0].PageCount != 272 {
		t.Errorf("editions = %+v", out.Editions)
	}
	if len(out.Tags) != 1 || !out.Tags[0].IsSubject || out.Tags[0].Source != string(domain.SourceLLM) {
		t.Errorf("tags = %+v, want the subject marked as the model's", out.Tags)
	}
	if len(out.Files) != 2 {
		t.Fatalf("files = %+v", out.Files)
	}
	if f := out.Files[0]; f.Filename != "k&r.epub" || !f.Held || !f.CanDownload || f.UnitType != "section" {
		t.Errorf("epub = %+v", f)
	}
	if f := out.Files[1]; f.Filename != "" || f.Held || f.CanDownload || f.UnitType != "page" {
		t.Errorf("pdf = %+v, want nothing claimed for a file with no per-viewer facts", f)
	}
}

func TestStatsCountsByFormat(t *testing.T) {
	fake := &fakeLibrary{stats: postgres.LibraryStats{Works: 3, Files: 4,
		ByFormat: map[domain.Format]int{domain.FormatEPUB: 3, domain.FormatPDF: 1}}}
	rec := libraryServe(t, fake, http.MethodGet, "/api/v1/stats", "", true)
	var out dto.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.ByFormat["epub"] != 3 || out.ByFormat["pdf"] != 1 || out.Works != 3 {
		t.Errorf("stats = %+v", out)
	}
}
