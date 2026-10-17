// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/citation"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// CitationStore is the reading this handler needs.
type CitationStore interface {
	Work(ctx context.Context, id, viewer uuid.UUID) (postgres.WorkDetail, error)
	Library(ctx context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error)
}

// CitationHandler exports books as citations.
type CitationHandler struct {
	Store CitationStore
	// BaseURL is how this installation is reached, used for the url field of a
	// citation. Empty leaves the field out, which is the honest thing to do:
	// a citation naming http://localhost:8080 helps nobody it is sent to.
	BaseURL string
}

// maxBibliography bounds an export. A whole shelf is a reasonable thing to
// want; a whole library of tens of thousands in one response is not.
const maxBibliography = 500

// Work serves GET /api/v1/works/{id}/citation.
func (h *CitationHandler) Work(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("work id must be a UUID", err))
		return
	}
	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	format, ok := citationFormat(w, r)
	if !ok {
		return
	}

	detail, err := h.Store.Work(r.Context(), id, viewer)
	if err != nil {
		if isNotFound(err) {
			respondError(w, NotFound("work"))
			return
		}
		respondError(w, err)
		return
	}

	entry := citation.FromWork(detail.Work, h.workURL(id))
	out, err := citation.Render([]citation.Entry{entry}, format)
	if err != nil {
		respondError(w, err)
		return
	}
	writeCitations(w, r, format, out, citation.Key(entry))
}

// Bibliography serves GET /api/v1/citations, exporting a whole shelf.
//
// It takes the library filters rather than a list of ids, so "everything
// tagged distributed-systems" is one request and stays one request when the
// shelf grows. What the viewer cannot see is not in it, because it goes
// through the same query the shelf does.
func (h *CitationHandler) Bibliography(w http.ResponseWriter, r *http.Request) {
	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	format, ok := citationFormat(w, r)
	if !ok {
		return
	}

	query := postgres.LibraryQuery{
		ViewerID: viewer,
		Search:   strings.TrimSpace(r.URL.Query().Get("q")),
		Tag:      strings.TrimSpace(r.URL.Query().Get("tag")),
		Limit:    maxBibliography,
	}
	// A collection is the reason most people want a bibliography: the books
	// behind one chapter, one essay, one reading group.
	if raw := strings.TrimSpace(r.URL.Query().Get("collection")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			respondError(w, Invalid("collection must be a UUID", err))
			return
		}
		query.CollectionID = &id
	}

	// Pages, because the store clamps a page to 200 however much is asked for.
	// The handler asked for 500 and took what came: a 400-book collection
	// exported as 200 entries with nothing saying so, which is a bibliography
	// that is wrong in the way that matters - it looks complete.
	var items []postgres.LibraryItem
	for len(items) < maxBibliography {
		page, err := h.Store.Library(r.Context(), query)
		if err != nil {
			respondError(w, err)
			return
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" || len(page.Items) == 0 {
			break
		}
		query.Cursor = page.NextCursor
	}
	if len(items) > maxBibliography {
		items = items[:maxBibliography]
	}
	page := postgres.LibraryPage{Items: items}

	// One round trip per work, because a citation needs the editions and
	// authors a shelf card does not carry. Bounded by maxBibliography above.
	entries := make([]citation.Entry, 0, len(page.Items))
	for _, item := range page.Items {
		detail, err := h.Store.Work(r.Context(), item.WorkID, viewer)
		if err != nil {
			// A book that vanished between the two queries is not a reason to
			// fail the export of the other four hundred.
			if isNotFound(err) {
				continue
			}
			respondError(w, err)
			return
		}
		entries = append(entries, citation.FromWork(detail.Work, h.workURL(item.WorkID)))
	}

	out, err := citation.Render(entries, format)
	if err != nil {
		respondError(w, err)
		return
	}
	writeCitations(w, r, format, out, "lexarii")
}

func (h *CitationHandler) workURL(id uuid.UUID) string {
	if h.BaseURL == "" {
		return ""
	}
	return strings.TrimRight(h.BaseURL, "/") + "/works/" + id.String()
}

func citationFormat(w http.ResponseWriter, r *http.Request) (citation.Format, bool) {
	format, err := citation.Parse(r.URL.Query().Get("format"))
	if err != nil {
		respondError(w, Invalid(err.Error(), err))
		return "", false
	}
	return format, true
}

// writeCitations sends the rendered text, offering it as a download when asked.
func writeCitations(w http.ResponseWriter, r *http.Request, format citation.Format, body, name string) {
	w.Header().Set("Content-Type", format.ContentType())
	// ?download=1 for the button that saves a .bib file; without it the
	// response is shown, which is what copying one entry wants.
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s.%s"`, safeFilename(name), format.Extension()))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// safeFilename keeps a citation key usable as a filename.
//
// The key is derived from a book's title and author, which are somebody else's
// text: a quotation mark or a path separator in a Content-Disposition is a
// header injection rather than a filename.
func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "citation"
	}
	return b.String()
}
