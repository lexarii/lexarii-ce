// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// LibraryReader is the slice of the store the library handlers need.
//
// Declared by the consumer so the handlers are testable with a small fake and
// do not depend on the whole store surface.
type LibraryReader interface {
	Library(ctx context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error)
	Work(ctx context.Context, id, viewer uuid.UUID) (postgres.WorkDetail, error)
	Stats(ctx context.Context, viewer uuid.UUID) (postgres.LibraryStats, error)
	Subjects(ctx context.Context, viewer uuid.UUID) ([]postgres.Subject, error)
	Mentions(ctx context.Context, workID, viewer uuid.UUID, limit int) ([]postgres.Mention, error)
	UpdateWork(ctx context.Context, workID, userID uuid.UUID, edit postgres.WorkEdit) (postgres.WorkDetail, error)
	Duplicates(ctx context.Context, viewer uuid.UUID) ([]postgres.DuplicateGroup, error)
	MergeWorks(ctx context.Context, userID, keep uuid.UUID, merge []uuid.UUID) (int, error)
}

// LibraryHandler serves the library grid, book detail, and stats.
type LibraryHandler struct {
	Store LibraryReader
	// Accounts decides who is an administrator, which is who is shown where
	// a file sits on the server's disk. Nil means nobody is.
	Accounts AdminStore
}

// workDetailFor is a work's page as this viewer may see it.
//
// A file's server path is for administrators: it is how they find a book on
// disk, and to everybody else it is the server's directory layout - mount
// points, other people's folder names - which on a larger installation is
// none of their business. The store already sends it only to whoever scanned
// the file; this narrows that to whoever is an administrator NOW, so taking
// somebody's rights away also takes the paths away. A failed lookup hides the
// paths rather than failing the page.
func (h *LibraryHandler) workDetailFor(ctx context.Context, d postgres.WorkDetail, viewer uuid.UUID) dto.WorkDetail {
	out := workDetailDTO(d)
	admin := false
	if h.Accounts != nil {
		if u, err := h.Accounts.User(ctx, viewer); err == nil {
			admin = u.IsAdmin
		}
	}
	if !admin {
		for i := range out.Files {
			out.Files[i].Path = ""
		}
	}
	return out
}

// List serves GET /api/v1/library.
func (h *LibraryHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	query := postgres.LibraryQuery{
		ViewerID:       viewer,
		Search:         strings.TrimSpace(q.Get("search")),
		Tag:            strings.TrimSpace(q.Get("tag")),
		Cursor:         q.Get("cursor"),
		Sort:           postgres.LibrarySort(q.Get("sort")),
		IncludeMissing: q.Get("includeMissing") == "true",
	}

	// `owned=mine` and `owned=shared`. An unrecognised value is refused rather
	// than ignored: a filter that silently does nothing shows somebody the whole
	// shelf while they believe they are looking at a part of it.
	switch owned := q.Get("owned"); owned {
	case "":
		query.Owned = postgres.OwnedAny
	case string(postgres.OwnedMine), string(postgres.OwnedShared):
		query.Owned = postgres.Ownership(owned)
	default:
		respondError(w, Invalid(`owned must be "mine" or "shared"`, nil))
		return
	}

	if s := q.Get("status"); s != "" {
		query.Status = domain.IngestStatus(s)
	}
	if a := q.Get("authorId"); a != "" {
		id, err := uuid.Parse(a)
		if err != nil {
			respondError(w, Invalid("authorId must be a UUID", err))
			return
		}
		query.AuthorID = &id
	}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil {
			respondError(w, Invalid("limit must be an integer", err))
			return
		}
		query.Limit = n
	}

	page, err := h.Store.Library(r.Context(), query)
	if err != nil {
		// An unknown sort or a malformed cursor is the client's mistake, not a
		// server fault, and must not be reported as a 500.
		if errors.Is(err, store.ErrInvalidQuery) {
			respondError(w, Invalid(err.Error(), err))
			return
		}
		respondError(w, err)
		return
	}

	out := dto.LibraryPage{
		Items:      make([]dto.LibraryItem, 0, len(page.Items)),
		NextCursor: page.NextCursor,
	}
	for _, it := range page.Items {
		out.Items = append(out.Items, libraryItemDTO(it))
	}
	respondJSON(w, http.StatusOK, out)
}

// Subjects serves GET /api/v1/subjects.
//
// What this library is about, commonest first. Scoped to what the viewer may
// read, like every other listing: an unscoped count would describe somebody
// else's shelf.
func (h *LibraryHandler) Subjects(w http.ResponseWriter, r *http.Request) {
	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	subjects, err := h.Store.Subjects(r.Context(), viewer)
	if err != nil {
		respondError(w, err)
		return
	}

	out := dto.SubjectList{Items: make([]dto.Subject, 0, len(subjects))}
	for _, s := range subjects {
		out.Items = append(out.Items, dto.Subject{Name: s.Name, Slug: s.Slug, Books: s.Books})
	}
	respondJSON(w, http.StatusOK, out)
}

// Detail serves GET /api/v1/works/{id}.
func (h *LibraryHandler) Detail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("work id must be a UUID", err))
		return
	}

	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	d, err := h.Store.Work(r.Context(), id, viewer)
	if err != nil {
		if isNotFound(err) {
			respondError(w, NotFound("work"))
			return
		}
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, h.workDetailFor(r.Context(), d, viewer))
}

// Edit serves PATCH /api/v1/works/{id}.
//
// A person correcting their own library. Seventy of the 168 books in the
// library this was built against have no author at all, three have a title
// taken off the wrong line of a title page, and until now there was no way to
// fix any of it by hand - only an LLM suggestion to accept or reject.
//
// A field left out of the body is left alone. A field present and empty is
// CLEARED, which is a real instruction: a subtitle read off a cover as "Second
// Edition" needs removing, not keeping.
func (h *LibraryHandler) Edit(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("work id must be a UUID", err))
		return
	}
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	var body dto.UpdateWork
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEditBytes)).Decode(&body); err != nil {
		respondError(w, Invalid("body must be a book correction", err))
		return
	}

	detail, err := h.Store.UpdateWork(r.Context(), id, userID, postgres.WorkEdit{
		Title:       body.Title,
		Subtitle:    body.Subtitle,
		Series:      body.Series,
		SeriesIndex: body.SeriesIndex,
		Language:    body.Language,
		Description: body.Description,
		Authors:     body.Authors,
		Publisher:   body.Publisher,
		Year:        body.Year,
		ISBN13:      body.ISBN13,
		ISBN10:      body.ISBN10,
	})
	if err != nil {
		if isNotFound(err) {
			respondError(w, NotFound("work"))
			return
		}
		if errors.Is(err, store.ErrInvalidQuery) {
			respondError(w, Invalid(err.Error(), err))
			return
		}
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, h.workDetailFor(r.Context(), detail, userID))
}

// maxEditBytes bounds a correction. A description is the long field, and a
// book's description is paragraphs rather than chapters.
const maxEditBytes = 64 << 10

// Duplicates serves GET /api/v1/duplicates.
//
// Books arrive as separate works when their FILENAMES differ: ingest matches a
// new file to an existing book by filename stem, so one book downloaded twice
// under two names is two books. Invisible until somebody scrolls past both.
func (h *LibraryHandler) Duplicates(w http.ResponseWriter, r *http.Request) {
	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	groups, err := h.Store.Duplicates(r.Context(), viewer)
	if err != nil {
		respondError(w, err)
		return
	}

	out := dto.DuplicateList{Items: make([]dto.DuplicateGroup, 0, len(groups))}
	for _, g := range groups {
		group := dto.DuplicateGroup{
			Reason: g.Reason,
			Works:  make([]dto.DuplicateWork, 0, len(g.Works)),
		}
		for _, wk := range g.Works {
			group.Works = append(group.Works, dto.DuplicateWork{
				WorkID:      wk.WorkID.String(),
				Title:       wk.Title,
				Authors:     wk.Authors,
				Formats:     wk.Formats,
				Files:       wk.Files,
				Annotations: wk.Annotations,
				HasProgress: wk.HasProgress,
			})
		}
		out.Items = append(out.Items, group)
	}
	respondJSON(w, http.StatusOK, out)
}

// Merge serves POST /api/v1/works/{id}/merge.
//
// The id in the path is the book being KEPT. Nothing is merged automatically:
// two printings, a translation and a second volume with a shared name are all
// real and distinct, and a program that welds them together on its own loses
// a library in a way no undo recovers.
func (h *LibraryHandler) Merge(w http.ResponseWriter, r *http.Request) {
	keep, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("work id must be a UUID", err))
		return
	}
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	var body dto.MergeWorks
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must list the books to merge in", err))
		return
	}

	merge := make([]uuid.UUID, 0, len(body.Merge))
	for _, raw := range body.Merge {
		id, err := uuid.Parse(raw)
		if err != nil {
			respondError(w, Invalid("every book to merge needs a UUID", err))
			return
		}
		merge = append(merge, id)
	}

	moved, err := h.Store.MergeWorks(r.Context(), userID, keep, merge)
	if err != nil {
		if isNotFound(err) {
			respondError(w, NotFound("work"))
			return
		}
		if errors.Is(err, store.ErrInvalidQuery) {
			respondError(w, Invalid(err.Error(), err))
			return
		}
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, dto.MergeResult{Kept: keep.String(), FilesMoved: moved})
}

// Mentions serves GET /api/v1/works/{id}/mentions.
//
// Separate from the detail request rather than folded into it: most books have
// no mentions, the search costs a full-text query per book, and a page that is
// already rendered does not need to wait on a list that sits below the fold.
func (h *LibraryHandler) Mentions(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("work id must be a UUID", err))
		return
	}
	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	found, err := h.Store.Mentions(r.Context(), id, viewer, maxMentions)
	if err != nil {
		if isNotFound(err) {
			respondError(w, NotFound("work"))
			return
		}
		respondError(w, err)
		return
	}

	out := dto.MentionList{Items: make([]dto.Mention, 0, len(found))}
	for _, m := range found {
		out.Items = append(out.Items, dto.Mention{
			WorkID:     m.WorkID.String(),
			BookFileID: m.BookFileID.String(),
			Title:      m.Title,
			Authors:    m.Authors,
			Locator:    m.Locator.String(),
			Excerpt:    m.Excerpt,
			Places:     m.Places,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// maxMentions bounds the list. On a real library the most any book has is one
// naming it fifteen times, which is one entry.
const maxMentions = 20

// Stats serves GET /api/v1/stats.
func (h *LibraryHandler) Stats(w http.ResponseWriter, r *http.Request) {
	viewer, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	s, err := h.Store.Stats(r.Context(), viewer)
	if err != nil {
		respondError(w, err)
		return
	}

	byFormat := make(map[string]int, len(s.ByFormat))
	for f, n := range s.ByFormat {
		byFormat[string(f)] = n
	}

	respondJSON(w, http.StatusOK, dto.Stats{
		Works:     s.Works,
		Files:     s.Files,
		Authors:   s.Authors,
		Tags:      s.Tags,
		Pages:     s.Pages,
		Missing:   s.Missing,
		NeedsOCR:  s.NeedsOCR,
		Failed:    s.Failed,
		ByFormat:  byFormat,
		QueueBusy: s.QueueBusy,
	})
}

func libraryItemDTO(it postgres.LibraryItem) dto.LibraryItem {
	formats := make([]string, 0, len(it.Formats))
	for _, f := range it.Formats {
		formats = append(formats, string(f))
	}

	out := dto.LibraryItem{
		WorkID:      it.WorkID.String(),
		Title:       it.Title,
		Subtitle:    it.Subtitle,
		Authors:     it.Authors,
		Series:      it.Series,
		SeriesIndex: it.SeriesIndex,
		Formats:     formats,
		Tags:        it.TagNames,
		UnitCount:   it.PageCount,
		UnitType:    unitTypeFor(it.Formats),
		Status:      string(it.Status),
		Missing:     it.Missing,
		BookFileID:  it.BookFileID.String(),
		AddedAt:     it.AddedAt,
	}
	if out.Authors == nil {
		// An empty array rather than null: the client should not have to
		// null-check a list it always iterates.
		out.Authors = []string{}
	}
	out.Progress = it.Progress
	out.LentBy = it.LentBy
	out.CanDownload = it.CanDownload
	if it.CoverPath != "" {
		// The FILE, not the work: a cover is rendered from a particular file's
		// first page, and the route that took a work id was never registered -
		// so this pointed at a 404 for as long as the field existed.
		out.CoverURL = "/api/v1/books/" + it.BookFileID.String() + "/cover"
	}
	return out
}

func workDetailDTO(d postgres.WorkDetail) dto.WorkDetail {
	out := dto.WorkDetail{
		ID:          d.Work.ID.String(),
		Title:       d.Work.Title,
		Subtitle:    d.Work.Subtitle,
		SortTitle:   d.Work.SortTitle,
		Series:      d.Work.Series,
		SeriesIndex: d.Work.SeriesIndex,
		Language:    d.Work.Language,
		Description: d.Work.Description,
		Summary:     d.Work.Summary,
		Warnings:    d.Warnings,
		Authors:     make([]dto.Author, 0, len(d.Work.Authors)),
		Files:       make([]dto.BookFile, 0, len(d.Files)),
		CreatedAt:   d.Work.CreatedAt,
		UpdatedAt:   d.Work.UpdatedAt,
	}

	for _, e := range d.Work.Editions {
		out.Editions = append(out.Editions, dto.Edition{
			ID:          e.ID.String(),
			ISBN13:      e.ISBN13,
			ISBN10:      e.ISBN10,
			Publisher:   e.Publisher,
			PublishedOn: e.PublishedOn,
			PageCount:   e.PageCount,
			Language:    e.Language,
		})
	}
	for _, wa := range d.Work.Authors {
		out.Authors = append(out.Authors, dto.Author{
			ID:       wa.Author.ID.String(),
			Name:     wa.Author.Name,
			SortName: wa.Author.SortName,
			Role:     string(wa.Role),
		})
	}
	for i, f := range d.Files {
		canDownload := i < len(d.Downloadable) && d.Downloadable[i]
		var filename string
		if i < len(d.Filenames) {
			filename = d.Filenames[i]
		}
		held := i < len(d.Held) && d.Held[i]
		out.Files = append(out.Files, dto.BookFile{
			Filename:    filename,
			Held:        held,
			CanDownload: canDownload,
			ID:          f.ID.String(),
			Format:      string(f.Format),
			SizeBytes:   f.SizeBytes,
			Status:      string(f.Status),
			IngestError: f.IngestError,
			UnitCount:   f.UnitCount,
			UnitType:    unitTypeFor([]domain.Format{f.Format}),
			ExtractedAt: f.ExtractedAt,
			Path:        f.Path,
		})
	}
	for _, t := range d.Tags {
		out.Tags = append(out.Tags, dto.Tag{
			ID:        t.ID.String(),
			Name:      t.Name,
			Slug:      t.Slug,
			IsSubject: t.IsSubject,
			Source:    string(t.Source),
		})
	}
	if len(d.Provenance) > 0 {
		out.Provenance = make(map[string]string, len(d.Provenance))
		for field, source := range d.Provenance {
			out.Provenance[field] = string(source)
		}
	}
	return out
}

// unitTypeFor names what a unit count counts, so the UI never labels spine
// items "pages".
func unitTypeFor(formats []domain.Format) string {
	for _, f := range formats {
		switch f {
		case domain.FormatPDF:
			return "page"
		case domain.FormatEPUB:
			return "section"
		}
	}
	return ""
}

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}
