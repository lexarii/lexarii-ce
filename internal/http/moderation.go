// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// ModerationStore is what removing a file for everybody, and the blocklist
// that keeps it removed, need.
type ModerationStore interface {
	BookFile(ctx context.Context, id uuid.UUID) (domain.BookFile, error)
	RemoveBookFileForEveryone(ctx context.Context, bookFileID, adminID uuid.UUID, reason string,
		opts postgres.RemovalOptions) ([]uuid.UUID, error)
	DiscardBlockedCopies(ctx context.Context, contentHash string, discard func(postgres.StoredCopies) error) error
	BlocksWithCopies(ctx context.Context) ([]string, error)
	IsBlocked(ctx context.Context, contentHash string) (bool, error)
	BlockedFiles(ctx context.Context, admin uuid.UUID) ([]postgres.BlockedFile, error)
	Unblock(ctx context.Context, admin uuid.UUID, contentHash string) error
}

// CopyDiscarder deletes the copies of a file Lexarii itself stored, and
// nothing else: ingest.Discard.
type CopyDiscarder interface {
	Copies(hash string) []string
	Remove(path string) (bool, error)
	RemoveCover(name string) error
}

// DeleteStoredCopies deletes what Lexarii stored of one file's bytes, by the
// rules every deletion of stored copies here follows - a removal for
// everybody, and paying what an erasure owes the disk - so code that wraps
// either can delete exactly what they would, and nothing else:
//
//   - every path in c.Paths, and every copy d finds of c.ContentHash, each
//     once;
//   - with c.SkipUploads, none of them in the upload directory - bytes an
//     upload is on its way to read - and none at all when d cannot tell
//     which those are (it is not an ingest.Discard);
//   - c.Cover and c.Covers, from the cover directory.
//
// d decides what is Lexarii's to delete: ingest.Discard never deletes
// anything in the library directory, whatever it is asked. It reports how
// many copies it deleted. A copy already gone is not an error; every copy is
// tried, and the failures are returned together.
//
// For many files in one pass, StoredCopyDeleter does the same and lists the
// archive once.
func DeleteStoredCopies(d CopyDiscarder, c postgres.StoredCopies) (int, error) {
	return deleteCopiesIndexed(d, c, nil)
}

// StoredCopyDeleter is DeleteStoredCopies for a pass over many files: d's
// archive is listed once, now, rather than once per file - which is how
// paying erasures' debts uses it, as the discard function DiscardPending
// takes. The listing is not refreshed, so one deleter serves one pass.
func StoredCopyDeleter(d CopyDiscarder) func(postgres.StoredCopies) (int, error) {
	index := archiveIndexOf(d)
	return func(c postgres.StoredCopies) (int, error) {
		return deleteCopiesIndexed(d, c, index)
	}
}

// archiveIndexer is a discarder that can list its archive once for many
// files: ingest.Discard.
type archiveIndexer interface {
	ArchiveIndex() map[string][]string
	CopiesIn(hash string, index map[string][]string) []string
}

// uploadTeller is a discarder that knows its upload directory:
// ingest.Discard.
type uploadTeller interface {
	IsUpload(path string) bool
}

// archiveIndexOf lists the archive once, when the discarder can.
func archiveIndexOf(d CopyDiscarder) map[string][]string {
	if ix, ok := d.(archiveIndexer); ok {
		return ix.ArchiveIndex()
	}
	return nil
}

// deleteCopiesIndexed is DeleteStoredCopies with the archive already listed
// (nil: list it for this file).
func deleteCopiesIndexed(d CopyDiscarder, c postgres.StoredCopies, index map[string][]string) (int, error) {
	paths := append([]string{}, c.Paths...)
	if c.ContentHash != "" {
		if ix, ok := d.(archiveIndexer); ok && index != nil {
			paths = append(paths, ix.CopiesIn(c.ContentHash, index)...)
		} else {
			paths = append(paths, d.Copies(c.ContentHash)...)
		}
	}
	var errs []error
	removed := 0
	seen := map[string]bool{}
	uploads, canTell := d.(uploadTeller)
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		// Bytes an upload is on its way to read: only what lies outside the
		// upload directory goes, and with no way to tell, nothing does.
		if c.SkipUploads && (!canTell || uploads.IsUpload(path)) {
			continue
		}
		ok, err := d.Remove(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ok {
			removed++
		}
	}
	for _, cover := range append([]string{c.Cover}, c.Covers...) {
		if err := d.RemoveCover(cover); err != nil {
			errs = append(errs, err)
		}
	}
	return removed, errors.Join(errs...)
}

// ModerationHandler serves an administrator taking a file off the
// installation for everybody, and the blocklist that keeps it off.
type ModerationHandler struct {
	Store    ModerationStore
	Accounts AdminStore
	Discard  CopyDiscarder
	Logger   *slog.Logger
	// Notify, when set, is told who a removed file was taken from, so an
	// installation can let them know. Nil tells nobody; the people
	// themselves find it gone from their shelf.
	Notify func(ctx context.Context, bookFileID uuid.UUID, people []uuid.UUID)
}

func (h *ModerationHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// Remove serves POST /api/v1/admin/books/{id}/remove: take a file off every
// shelf and block its bytes.
//
// The database half commits first; deleting the copies Lexarii stored follows,
// under the file's lock, and is best-effort - the book is already gone from
// every shelf and the bytes are already refused, which is what was asked. It
// runs to the end even if the person who asked has gone: a closed tab must
// not leave copies behind. A copy that could not be deleted is logged, the
// blocklist shows copies remain and offers to finish, and startup tries
// again. A file in the library directory is never deleted.
func (h *ModerationHandler) Remove(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminOrForbidden(w, r, h.Accounts)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}
	var body dto.RemoveForEveryoneRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must give a reason", err))
		return
	}

	file, err := h.Store.BookFile(r.Context(), id)
	if err != nil {
		respondModerationError(w, err)
		return
	}
	people, err := h.Store.RemoveBookFileForEveryone(r.Context(), id, admin, body.Reason,
		postgres.RemovalOptions{PurgeQuotes: body.PurgeQuotes})
	if err != nil {
		respondModerationError(w, err)
		return
	}

	discarded, _ := h.discard(r.Context(), file.ContentHash)
	h.log().Info("book file removed for everyone",
		"admin", admin, "bookFileId", id, "contentHash", file.ContentHash,
		"removedFor", len(people), "copiesDeleted", discarded)
	if h.Notify != nil {
		h.Notify(r.Context(), id, people)
	}

	respondJSON(w, http.StatusOK, dto.RemovedForEveryone{RemovedFor: len(people)})
}

// discardTimeout bounds deleting one removed file's copies: a handful of
// files, and a slow disk should not hold a request open for ever.
const discardTimeout = 2 * time.Minute

// discard deletes what Lexarii stored of blocked bytes - the record's own
// copies, any upload or archive copy named by the content hash, and the cover
// - and reports how many copies went. Detached from the request: once a
// removal has committed, its clean-up finishes whether or not anybody is still
// waiting for the reply.
func (h *ModerationHandler) discard(ctx context.Context, hash string) (int, error) {
	if h.Discard == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), discardTimeout)
	defer cancel()
	removed := 0
	err := h.Store.DiscardBlockedCopies(ctx, hash, func(c postgres.StoredCopies) error {
		n, err := DeleteStoredCopies(h.Discard, c)
		removed += n
		return err
	})
	if err != nil {
		h.log().Warn("deleting a removed file's copies failed; the blocklist offers to finish",
			"contentHash", hash, "error", err)
	}
	return removed, err
}

// FinishRemovals deletes the stored copies of every blocked file that may
// still have some: a deletion that failed, or that never ran because the
// server stopped between a removal committing and its clean-up. Run once at
// startup. Not on every scan: a scan is about the library directory, which
// these copies are never in, and runs only when an administrator asks.
func (h *ModerationHandler) FinishRemovals(ctx context.Context) {
	hashes, err := h.Store.BlocksWithCopies(ctx)
	if err != nil {
		h.log().Warn("finding removed files with copies left failed", "error", err)
		return
	}
	for _, hash := range hashes {
		if ctx.Err() != nil {
			return
		}
		if n, err := h.discard(ctx, hash); err == nil {
			h.log().Info("finished deleting a removed file's copies", "contentHash", hash, "copiesDeleted", n)
		}
	}
}

// DiscardCopies serves POST /api/v1/admin/blocked/{hash}/discard: finish deleting
// the copies Lexarii stored of a removed file, when the first attempt did not.
func (h *ModerationHandler) DiscardCopies(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminOrForbidden(w, r, h.Accounts); !ok {
		return
	}
	hash := r.PathValue("hash")
	if !contentHashShape.MatchString(hash) {
		respondError(w, Invalid("hash must be a SHA-256 in lowercase hex", nil))
		return
	}
	blocked, err := h.Store.IsBlocked(r.Context(), hash)
	if err != nil {
		respondError(w, err)
		return
	}
	if !blocked {
		respondError(w, NotFound("block"))
		return
	}
	_, derr := h.discard(r.Context(), hash)
	respondJSON(w, http.StatusOK, dto.DiscardedCopies{CopiesRemain: derr != nil})
}

// Blocked serves GET /api/v1/admin/blocked.
func (h *ModerationHandler) Blocked(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminOrForbidden(w, r, h.Accounts)
	if !ok {
		return
	}
	files, err := h.Store.BlockedFiles(r.Context(), admin)
	if err != nil {
		respondModerationError(w, err)
		return
	}
	out := dto.BlockedFiles{Items: make([]dto.BlockedFile, 0, len(files))}
	for _, f := range files {
		out.Items = append(out.Items, dto.BlockedFile{
			ContentHash:  f.ContentHash,
			Reason:       f.Reason,
			BlockedAt:    f.BlockedAt,
			BlockedBy:    f.BlockedBy,
			Title:        f.Title,
			CopiesRemain: f.CopiesRemain,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// Unblock serves DELETE /api/v1/admin/blocked/{hash}.
func (h *ModerationHandler) Unblock(w http.ResponseWriter, r *http.Request) {
	admin, ok := adminOrForbidden(w, r, h.Accounts)
	if !ok {
		return
	}
	hash := r.PathValue("hash")
	if !contentHashShape.MatchString(hash) {
		respondError(w, Invalid("hash must be a SHA-256 in lowercase hex", nil))
		return
	}
	if err := h.Store.Unblock(r.Context(), admin, hash); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("block"))
			return
		}
		respondModerationError(w, err)
		return
	}
	h.log().Info("block lifted", "admin", admin, "contentHash", hash)
	w.WriteHeader(http.StatusNoContent)
}

// contentHashShape is the shape every content hash has, as the database
// checks it.
var contentHashShape = regexp.MustCompile(`^[0-9a-f]{64}$`)

func respondModerationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		respondError(w, &httpError{
			status:  http.StatusForbidden,
			code:    "not_an_admin",
			message: "only an administrator can remove a book for everyone",
		})
	case errors.Is(err, store.ErrBusy):
		// Somebody kept coming to have something of the file while it was
		// being removed. Nothing was changed.
		respondError(w, &httpError{
			status:  http.StatusServiceUnavailable,
			code:    "try_again",
			message: "the book changed while it was being removed; nothing was changed, so try again",
		})
	case errors.Is(err, store.ErrInvalidQuery):
		respondError(w, Invalid(err.Error(), err))
	case errors.Is(err, store.ErrNotFound):
		respondError(w, NotFound("book"))
	default:
		respondError(w, err)
	}
}
