// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

type fakeCitations struct {
	detail postgres.WorkDetail
	items  []postgres.LibraryItem
	err    error
}

func (f *fakeCitations) Work(context.Context, uuid.UUID, uuid.UUID) (postgres.WorkDetail, error) {
	return f.detail, f.err
}

func (f *fakeCitations) Library(context.Context, postgres.LibraryQuery) (postgres.LibraryPage, error) {
	return postgres.LibraryPage{Items: f.items}, f.err
}

func citedWork() postgres.WorkDetail {
	when := time.Date(1968, time.January, 1, 0, 0, 0, 0, time.UTC)
	return postgres.WorkDetail{Work: domain.Work{
		Title:   "The Art of Computer Programming",
		Authors: []domain.WorkAuthor{{Author: domain.Author{Name: "Knuth, Donald E."}}},
		Editions: []domain.Edition{{
			Publisher: "Addison-Wesley", ISBN13: "9780201896831", PublishedOn: &when,
		}},
	}}
}

func getCitation(t *testing.T, h *CitationHandler, query string) *httptest.ResponseRecorder {
	t.Helper()
	r := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/works/x/citation"+query, nil))
	r.SetPathValue("id", uuid.New().String())
	w := httptest.NewRecorder()
	h.Work(w, r)
	return w
}

func TestCitationDefaultsToBibTeX(t *testing.T) {
	h := &CitationHandler{Store: &fakeCitations{detail: citedWork()}}
	w := getCitation(t, h, "")

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(w.Body.String(), "@book{knuth1968art,") {
		t.Errorf("body = %q", w.Body.String())
	}
	// text/plain so a browser shows it. Copying one entry is the common case.
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q", ct)
	}
	// Not a download unless asked.
	if cd := w.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("unrequested download: %q", cd)
	}
}

func TestCitationFormats(t *testing.T) {
	h := &CitationHandler{Store: &fakeCitations{detail: citedWork()}}

	ris := getCitation(t, h, "?format=ris")
	if !strings.Contains(ris.Body.String(), "TY  - BOOK") {
		t.Errorf("ris body = %q", ris.Body.String())
	}
	if ct := ris.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/x-research-info-systems") {
		t.Errorf("ris content type = %q", ct)
	}

	csl := getCitation(t, h, "?format=csl-json")
	if !strings.Contains(csl.Body.String(), `"type": "book"`) {
		t.Errorf("csl body = %q", csl.Body.String())
	}
	if ct := csl.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("csl content type = %q", ct)
	}
}

func TestCitationRejectsAnUnknownFormat(t *testing.T) {
	h := &CitationHandler{Store: &fakeCitations{detail: citedWork()}}
	if w := getCitation(t, h, "?format=endnote"); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// The citation key comes from a book's own title and author, which is somebody
// else's text. A quotation mark or a newline in a Content-Disposition is a
// header injection, not a filename.
func TestCitationDownloadFilenameIsSafe(t *testing.T) {
	detail := citedWork()
	detail.Work.Title = `A "Quoted" Title`
	detail.Work.Authors = []domain.WorkAuthor{{Author: domain.Author{Name: "../../etc/passwd"}}}

	h := &CitationHandler{Store: &fakeCitations{detail: detail}}
	w := getCitation(t, h, "?download=1")

	cd := w.Header().Get("Content-Disposition")
	if strings.ContainsAny(cd, "\"\n\r/") != strings.Contains(cd, `"`) {
		t.Fatalf("suspicious header: %q", cd)
	}
	if strings.Contains(cd, "..") || strings.Contains(cd, "/") {
		t.Errorf("path characters survived into %q", cd)
	}
	if !strings.HasSuffix(cd, `.bib"`) {
		t.Errorf("no extension in %q", cd)
	}
}

// Without a public URL the field is left out rather than filled in with an
// address that means the reader's own machine.
func TestCitationOmitsTheURLWhenThereIsNoPublicOne(t *testing.T) {
	h := &CitationHandler{Store: &fakeCitations{detail: citedWork()}}
	if body := getCitation(t, h, "").Body.String(); strings.Contains(body, "url") {
		t.Errorf("a url appeared with no PublicURL set:\n%s", body)
	}

	h.BaseURL = "https://books.example/"
	body := getCitation(t, h, "").Body.String()
	if !strings.Contains(body, "url        = {https://books.example/works/") {
		t.Errorf("no url with PublicURL set:\n%s", body)
	}
}

func TestBibliographyExportsAShelf(t *testing.T) {
	store := &fakeCitations{
		detail: citedWork(),
		items:  []postgres.LibraryItem{{WorkID: uuid.New()}, {WorkID: uuid.New()}},
	}
	h := &CitationHandler{Store: store}

	r := asUser(httptest.NewRequest(http.MethodGet, "/api/v1/citations?tag=algorithms", nil))
	w := httptest.NewRecorder()
	h.Bibliography(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	// Two books, one author, one year: the keys must be disambiguated or
	// BibTeX silently drops one of them.
	body := w.Body.String()
	if !strings.Contains(body, "@book{knuth1968arta,") || !strings.Contains(body, "@book{knuth1968artb,") {
		t.Errorf("keys not disambiguated across the shelf:\n%s", body)
	}
}
