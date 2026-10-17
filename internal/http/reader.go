// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/epub"
	pdfingest "scm.poniatowski.dev/adam/lexarii/internal/ingest/pdf"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"sync"
)

// BookFileReader looks up the file backing a reading session.
//
// Every lookup carries the viewer. Books belong to people, and the reader is
// where their content is actually served - a missing check here is not a
// listing that shows too much but the book itself, handed over.
type BookFileReader interface {
	ReadableBookFile(ctx context.Context, id, viewer uuid.UUID) (domain.BookFile, error)
	ReadableBookWithWork(ctx context.Context, id, viewer uuid.UUID) (postgres.ReadableBook, error)
	CanDownload(ctx context.Context, id, viewer uuid.UUID) (bool, error)
	RecordAccess(ctx context.Context, bookFileID, viewer uuid.UUID, kind postgres.AccessKind, unit int) error
}

// ReaderHandler serves book content to the reading surface.
//
// Content is read from the book file on disk on demand rather than copied into
// the database. A book is already stored once; storing it again would double
// the disk cost of every library and put the user's text somewhere they did not
// put it. Opening an EPUB is fast because only the zip directory is read.
type ReaderHandler struct {
	Store BookFileReader

	// PDFCacheBytes is how much memory parsed PDFs may hold. Zero uses the
	// default. See DefaultBudget for what it costs and why it is bytes rather
	// than a number of books.
	PDFCacheBytes int64

	// books holds parsed PDFs so that a book shared for reading can be served
	// a page at a time without re-parsing it on every page turn.
	books     *bookCache
	booksOnce sync.Once
}

// audit records that somebody read part of a book that is not theirs.
//
// Deliberately nothing is done with the error beyond logging it. Auditing is a
// side effect of reading: a reader that fails because the log is unavailable
// has turned a record-keeping problem into an outage, and the person trying to
// read a book they were legitimately given would be the one to suffer for it.
//
// context.WithoutCancel because the record should survive the reader closing
// the tab. A page that was sent was read.
func (h *ReaderHandler) audit(r *http.Request, id, viewer uuid.UUID, kind postgres.AccessKind, unit int) {
	if viewer == uuid.Nil {
		return
	}
	if err := h.Store.RecordAccess(context.WithoutCancel(r.Context()), id, viewer, kind, unit); err != nil {
		slog.Warn("recording access failed", "book", id, "kind", kind, "error", err)
	}
}

func (h *ReaderHandler) cache() *bookCache {
	h.booksOnce.Do(func() { h.books = newBookCache(h.PDFCacheBytes) })
	return h.books
}

// Page serves GET /api/v1/books/{id}/page/{n}.
//
// One page of a PDF, as a standalone one-page PDF document.
//
// This exists so that a book shared for READING does not have to be handed over
// as a file. The whole-file endpoint requires the download permission; this one
// does not, because a page is what reading needs.
//
// It is friction rather than protection, and worth being honest about: several
// hundred requests is a script, and anything on screen can be photographed.
// What it buys is that taking the book stops being one click and starts being
// visible in the request log.
func (h *ReaderHandler) Page(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}
	number, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		respondError(w, Invalid("page number must be a number", err))
		return
	}

	viewer, _ := UserFrom(r.Context())
	file, err := h.Store.ReadableBookFile(r.Context(), id, viewer)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("book file"))
			return
		}
		respondError(w, err)
		return
	}
	if file.Format != domain.FormatPDF {
		respondError(w, Invalid("only a PDF is read by page", nil))
		return
	}

	book, err := h.cache().Book(file.ID, file.Path)
	if err != nil {
		respondError(w, &httpError{
			status:  http.StatusGone,
			code:    "file_unreadable",
			message: "this file could not be opened",
			cause:   err,
		})
		return
	}

	page, err := book.Page(number)
	if err != nil {
		respondError(w, Invalid(fmt.Sprintf("no page %d in this book", number), err))
		return
	}

	// A prefetched page counts as traffic but not as a page taken.
	//
	// The reader fetches one page either side of where somebody is, so that
	// flipping is smooth. Counting those as pages read would make the audit
	// over-report: someone who looked at page 10 would be recorded as having
	// seen 9, 10 and 11. `views` still counts every request, so a script
	// pulling the whole book is just as visible - more so, in fact, since a
	// run of hundreds of views against almost no pages read is a shape no
	// reader produces.
	unit := number
	if r.URL.Query().Get("prefetch") == "1" {
		unit = 0
	}
	h.audit(r, id, viewer, postgres.AccessPage, unit)

	w.Header().Set("Content-Type", "application/pdf")
	// Private: a shared book must not be held in a proxy cache where the next
	// reader could be served it without an access check.
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

// Manifest serves GET /api/v1/books/{id}/manifest.
func (h *ReaderHandler) Manifest(w http.ResponseWriter, r *http.Request) {
	// A PDF has no spine to reflow, but it does have a contents list, and a
	// reader that cannot jump to a chapter in a 900-page reference book is
	// missing the thing such a book is mostly used for.
	if file, ok := h.pdfFile(w, r); ok {
		h.pdfManifest(w, file)
		return
	}

	file, reader, closer, err := h.open(r)
	if err != nil {
		respondError(w, err)
		return
	}
	defer closer()

	spine := reader.Spine()
	out := dto.ReaderManifest{
		BookFileID: file.ID.String(),
		Format:     string(file.Format),
		Spine:      make([]dto.SpineItem, 0, len(spine)),
		TOC:        make([]dto.TOCEntry, 0),
	}
	for _, item := range spine {
		out.Spine = append(out.Spine, dto.SpineItem{
			Index:  item.Index,
			ID:     item.ID,
			Linear: item.Linear,
		})
	}
	out.TOC = tocDTO(reader.TOC())

	respondJSON(w, http.StatusOK, out)
}

// Content serves GET /api/v1/books/{id}/spine/{index}.
//
// The response is sanitised HTML fragment, not a document: the reader inserts
// it into its own page, under its own typography.
func (h *ReaderHandler) Content(w http.ResponseWriter, r *http.Request) {
	file, reader, closer, err := h.open(r)
	if err != nil {
		respondError(w, err)
		return
	}
	defer closer()

	index, convErr := strconv.Atoi(r.PathValue("index"))
	if convErr != nil {
		respondError(w, Invalid("spine index must be an integer", convErr))
		return
	}

	base := "/api/v1/books/" + file.ID.String() + "/assets/"
	html, err := reader.SpineItem(index, func(archivePath string) string {
		// Each path segment is escaped separately so slashes survive as
		// separators and everything else is inert.
		parts := strings.Split(archivePath, "/")
		for i, p := range parts {
			parts[i] = url.PathEscape(p)
		}
		return base + strings.Join(parts, "/")
	})
	if err != nil {
		respondError(w, NotFound("spine item"))
		return
	}

	viewer, _ := UserFrom(r.Context())
	h.audit(r, file.ID, viewer, postgres.AccessChapter, index+1)

	respondJSON(w, http.StatusOK, dto.SpineContent{
		Index: index,
		HTML:  html,
	})
}

// Asset serves GET /api/v1/books/{id}/assets/{path...}.
func (h *ReaderHandler) Asset(w http.ResponseWriter, r *http.Request) {
	_, reader, closer, err := h.open(r)
	if err != nil {
		respondError(w, err)
		return
	}
	defer closer()

	archivePath, perr := epub.AssetPathFromURL(r.PathValue("path"))
	if perr != nil {
		respondError(w, Invalid("bad asset path", perr))
		return
	}

	data, mediaType, aerr := reader.Asset(archivePath)
	if aerr != nil {
		respondError(w, NotFound("asset"))
		return
	}

	w.Header().Set("Content-Type", mediaType)
	// Archive contents cannot change without the file's hash changing, and the
	// hash is part of the book's identity, so this is safe to cache hard.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	// An image from a book is never a document. Without this a crafted SVG could
	// be navigated to directly and would run as one.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// open resolves the book file named in the request and opens it for reading.
// The returned closer MUST be called by the caller, not deferred here: the zip
// reader reads entries lazily from the open file, so closing it before the
// caller has read anything makes every request fail.
func (h *ReaderHandler) open(r *http.Request) (domain.BookFile, *epub.Reader, func(), error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return domain.BookFile{}, nil, nil, Invalid("book file id must be a UUID", err)
	}

	viewer, _ := UserFrom(r.Context())
	file, err := h.Store.ReadableBookFile(r.Context(), id, viewer)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return domain.BookFile{}, nil, nil, NotFound("book file")
		}
		return domain.BookFile{}, nil, nil, err
	}

	if file.Format != domain.FormatEPUB {
		// Only EPUB is reflowed here. A PDF has fixed pages and is rendered by
		// the client from the raw file instead, so this path is genuinely not
		// applicable rather than merely unimplemented.
		return domain.BookFile{}, nil, nil, &httpError{
			status:  http.StatusNotImplemented,
			code:    "format_not_reflowable",
			message: string(file.Format) + " has fixed pages and is not reflowed; fetch the file itself instead",
		}
	}

	f, err := os.Open(file.Path) // read-only: the library directory is never written to
	if err != nil {
		if os.IsNotExist(err) {
			return domain.BookFile{}, nil, nil, &httpError{
				status:  http.StatusGone,
				code:    "file_missing",
				message: "this file is no longer on disk",
			}
		}
		return domain.BookFile{}, nil, nil, err
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return domain.BookFile{}, nil, nil, err
	}

	reader, err := epub.Open(f, info.Size())
	if err != nil {
		_ = f.Close()
		return domain.BookFile{}, nil, nil, &httpError{
			status:  http.StatusUnprocessableEntity,
			code:    "unreadable_file",
			message: "this file could not be opened for reading",
			cause:   err,
		}
	}
	// Re-opened per request rather than cached, so a deleted or replaced file is
	// never served from a stale handle.
	return file, reader, func() { _ = f.Close() }, nil
}

// pdfFile resolves the request to a PDF, or reports that it is not one.
//
// A failure to resolve is not reported here: the ordinary path runs next and
// produces the same error with its own message, so nothing is swallowed.
func (h *ReaderHandler) pdfFile(w http.ResponseWriter, r *http.Request) (domain.BookFile, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return domain.BookFile{}, false
	}
	viewer, _ := UserFrom(r.Context())
	file, err := h.Store.ReadableBookFile(r.Context(), id, viewer)
	if err != nil || file.Format != domain.FormatPDF {
		return domain.BookFile{}, false
	}
	return file, true
}

// pdfManifest serves the contents of a PDF.
//
// Spine is empty and stays empty: a PDF's pages are not spine items, and the
// client already knows how many there are from the file it is rendering.
// Inventing spine entries would be a second, disagreeing source of truth.
func (h *ReaderHandler) pdfManifest(w http.ResponseWriter, file domain.BookFile) {
	out := dto.ReaderManifest{
		BookFileID: file.ID.String(),
		Format:     string(file.Format),
		Spine:      make([]dto.SpineItem, 0),
		TOC:        make([]dto.TOCEntry, 0),
	}

	f, err := os.Open(file.Path) // read-only: the library directory is never written to
	if err != nil {
		// A missing file is not worth failing the manifest over - the client
		// fetches the document itself next and will report it properly.
		respondJSON(w, http.StatusOK, out)
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		respondJSON(w, http.StatusOK, out)
		return
	}

	entries, err := pdfingest.Outline(f, info.Size())
	if err != nil {
		// An unreadable outline means no contents list, not a broken book.
		respondJSON(w, http.StatusOK, out)
		return
	}
	out.TOC = tocDTO(entries)
	respondJSON(w, http.StatusOK, out)
}

func tocDTO(entries []ingest.TOCEntry) []dto.TOCEntry {
	out := make([]dto.TOCEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, dto.TOCEntry{
			Title:      e.Title,
			SpineIndex: e.SpineIndex,
			Fragment:   e.Fragment,
			Children:   tocDTO(e.Children),
		})
	}
	return out
}

// File serves GET /api/v1/books/{id}/file.
//
// The raw bytes, with range support, so a PDF viewer can fetch only the pages
// it is showing. A 158 MB reference book must not be downloaded in full to read
// page one, and http.ServeContent handles conditional and partial requests
// correctly, which is not worth reimplementing.
//
// This is the one endpoint that hands over the original file, so it needs the
// download permission and not merely read access. An owner always has it; a
// grantee has it only when the grant says so.
//
// Be clear about what withholding it achieves. It stops the file being SENT,
// which for an EPUB means a reader receives sanitised chapter HTML and would
// have to scrape and reassemble a hundred requests to reconstruct the book. It
// does not stop a determined person keeping what they can see - screenshots and
// OCR exist, and no arrangement of this code changes that. What it buys is
// effort and a record, not prevention.
func (h *ReaderHandler) File(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}

	viewer, _ := UserFrom(r.Context())
	file, err := h.Store.ReadableBookFile(r.Context(), id, viewer)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("book file"))
			return
		}
		respondError(w, err)
		return
	}

	allowed, err := h.Store.CanDownload(r.Context(), id, viewer)
	if err != nil {
		respondError(w, err)
		return
	}
	if !allowed {
		respondError(w, &httpError{
			status: http.StatusForbidden,
			code:   "download_not_permitted",
			message: "this book was shared with you to read, not to download; " +
				"open it in the reader instead",
		})
		return
	}

	f, err := os.Open(file.Path) // read-only
	if err != nil {
		respondError(w, &httpError{
			status:  http.StatusGone,
			code:    "file_missing",
			message: "this file is no longer on disk",
		})
		return
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		respondError(w, err)
		return
	}

	w.Header().Set("Content-Type", mediaTypeFor(file.Format))
	// The file cannot change without its content hash changing, and the hash is
	// the book's identity, so this is safe to cache hard.
	w.Header().Set("Cache-Control", "private, max-age=86400")
	// Recorded once per request, and only for a book that is not the reader's
	// own. A PDF viewer issues dozens of range requests for one reading, so
	// views on a file are a count of requests and not of downloads; the log
	// says `file` and the number is read as traffic rather than copies.
	if r.Header.Get("Range") == "" {
		h.audit(r, file.ID, viewer, postgres.AccessFile, 0)
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A book is data, never a document to be navigated to directly.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")

	// ServeContent negotiates ranges and conditional requests from the modtime
	// and size.
	http.ServeContent(w, r, file.ContentHash, info.ModTime(), f)
}

// mediaTypeFor maps a book format to its wire media type.
func mediaTypeFor(format domain.Format) string {
	switch format {
	case domain.FormatPDF:
		return "application/pdf"
	case domain.FormatEPUB:
		return "application/epub+zip"
	default:
		return "application/octet-stream"
	}
}

// Info serves GET /api/v1/books/{id}/info.
//
// Format and size, without opening the archive. The reader calls this first to
// decide which surface to show: a reflowable EPUB and a fixed-page PDF are
// different readers, and asking for an EPUB manifest to find that out would
// mean a wasted request and a misleading error.
func (h *ReaderHandler) Info(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}

	viewer, _ := UserFrom(r.Context())
	book, err := h.Store.ReadableBookWithWork(r.Context(), id, viewer)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("book file"))
			return
		}
		respondError(w, err)
		return
	}
	file := book.File

	download, err := h.Store.CanDownload(r.Context(), id, viewer)
	if err != nil {
		respondError(w, err)
		return
	}

	respondJSON(w, http.StatusOK, dto.BookInfo{
		CanDownload: download,
		BookFileID:  file.ID.String(),
		Title:       book.Title,
		WorkID:      book.WorkID.String(),
		Format:      string(file.Format),
		SizeBytes:   file.SizeBytes,
		Status:      string(file.Status),
		UnitCount:   file.UnitCount,
		Reflowable:  file.Format == domain.FormatEPUB,
	})
}
