// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

var errCitationsDown = errors.New("citation store unavailable")

// citationShelf pages a shelf of works the way the store does - a page at a
// time, each naming the next - and answers each work's detail, or fails it.
type citationShelf struct {
	mu       sync.Mutex
	pageSize int
	works    []uuid.UUID
	missing  map[uuid.UUID]bool
	workErr  error
	pageErr  error
	queries  []postgres.LibraryQuery
	detailed int
}

func (s *citationShelf) Library(_ context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries = append(s.queries, q)
	if s.pageErr != nil {
		return postgres.LibraryPage{}, s.pageErr
	}
	start := 0
	if q.Cursor != "" {
		start, _ = strconv.Atoi(q.Cursor)
	}
	end := min(start+s.pageSize, len(s.works))
	page := postgres.LibraryPage{}
	for _, id := range s.works[start:end] {
		page.Items = append(page.Items, postgres.LibraryItem{WorkID: id})
	}
	if end < len(s.works) {
		page.NextCursor = strconv.Itoa(end)
	}
	return page, nil
}

func (s *citationShelf) Work(_ context.Context, id, _ uuid.UUID) (postgres.WorkDetail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detailed++
	if s.workErr != nil {
		return postgres.WorkDetail{}, s.workErr
	}
	if s.missing[id] {
		return postgres.WorkDetail{}, store.ErrNotFound
	}
	return postgres.WorkDetail{Work: domain.Work{ID: id, Title: "Work " + id.String()[:8],
		Authors: []domain.WorkAuthor{{Author: domain.Author{Name: "Author, An"}}}}}, nil
}

func citationWorks(n int) []uuid.UUID {
	out := make([]uuid.UUID, n)
	for i := range out {
		out[i] = uuid.New()
	}
	return out
}

func citationGet(h *CitationHandler, target string, signedIn bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if signedIn {
		req = asUser(req)
	}
	rec := httptest.NewRecorder()
	(&Router{Health: &HealthHandler{}, Citations: h}).Handler().ServeHTTP(rec, req)
	return rec
}

// A bibliography follows the shelf's pages to the end - the store clamps a
// page, and a 400-book collection exported as its first 200 looks complete
// and is not.
func TestABibliographyFollowsEveryPage(t *testing.T) {
	s := &citationShelf{pageSize: 200, works: citationWorks(450)}
	collection := uuid.New()
	rec := citationGet(&CitationHandler{Store: s}, "/api/v1/citations?collection="+collection.String()+"&q=x&tag=y", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := strings.Count(rec.Body.String(), "@book{"); got != 450 {
		t.Errorf("%d entries, want all 450", got)
	}
	if len(s.queries) != 3 || s.queries[1].Cursor != "200" || s.queries[2].Cursor != "400" {
		t.Errorf("queries = %d, want three pages chained by cursor", len(s.queries))
	}
	q := s.queries[0]
	if q.CollectionID == nil || *q.CollectionID != collection || q.Search != "x" || q.Tag != "y" {
		t.Errorf("query = %+v, want the collection and filters passed on", q)
	}
}

// And it stops at the bound, however long the shelf.
func TestABibliographyIsBounded(t *testing.T) {
	s := &citationShelf{pageSize: 200, works: citationWorks(maxBibliography + 150)}
	rec := citationGet(&CitationHandler{Store: s}, "/api/v1/citations", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.Count(rec.Body.String(), "@book{"); got != maxBibliography {
		t.Errorf("%d entries, want %d", got, maxBibliography)
	}
	if s.detailed != maxBibliography {
		t.Errorf("read %d works, want no more than the bound", s.detailed)
	}
}

// A work that disappears between the two reads is left out; the rest of the
// export stands.
func TestABibliographySkipsAVanishedWork(t *testing.T) {
	works := citationWorks(3)
	s := &citationShelf{pageSize: 200, works: works, missing: map[uuid.UUID]bool{works[1]: true}}
	rec := citationGet(&CitationHandler{Store: s}, "/api/v1/citations?format=ris&download=1", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.Count(rec.Body.String(), "TY  - BOOK"); got != 2 {
		t.Errorf("%d entries, want the two that remain:\n%s", got, rec.Body)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="lexarii.ris"` {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

func TestCitationRefusals(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct {
		name     string
		target   string
		s        *citationShelf
		signedIn bool
		status   int
	}{
		{"a work, signed out", "/api/v1/works/" + id + "/citation", &citationShelf{}, false, http.StatusUnauthorized},
		{"a work with a bad id", "/api/v1/works/nope/citation", &citationShelf{}, true, http.StatusBadRequest},
		{"a work you cannot see", "/api/v1/works/" + id + "/citation",
			&citationShelf{missing: map[uuid.UUID]bool{uuid.MustParse(id): true}}, true, http.StatusNotFound},
		{"a work whose read fails", "/api/v1/works/" + id + "/citation", &citationShelf{workErr: errCitationsDown}, true,
			http.StatusInternalServerError},
		{"a shelf, signed out", "/api/v1/citations", &citationShelf{}, false, http.StatusUnauthorized},
		{"a shelf in an unknown format", "/api/v1/citations?format=docx", &citationShelf{}, true, http.StatusBadRequest},
		{"a shelf of a bad collection", "/api/v1/citations?collection=nope", &citationShelf{}, true, http.StatusBadRequest},
		{"a shelf whose page fails", "/api/v1/citations", &citationShelf{pageErr: errCitationsDown}, true,
			http.StatusInternalServerError},
		{"a shelf whose work read fails", "/api/v1/citations",
			&citationShelf{pageSize: 10, works: citationWorks(2), workErr: errCitationsDown}, true, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := citationGet(&CitationHandler{Store: tc.s}, tc.target, tc.signedIn)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
		})
	}
}

// A citation key made of nothing a filename can carry still downloads under a
// name.
func TestACitationKeyWithNothingSafeInItStillHasAName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"knuth1968art", "knuth1968art"},
		{`"; rm -rf /`, "rm-rf"},
		{"日本語", "citation"},
		{"", "citation"},
	} {
		if got := safeFilename(tc.in); got != tc.want {
			t.Errorf("safeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
