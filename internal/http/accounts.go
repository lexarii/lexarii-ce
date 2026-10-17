// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// AccountStore is what deleting an account and reclaiming its books needs.
type AccountStore interface {
	DeleteAccount(ctx context.Context, actor, target uuid.UUID) ([]postgres.ArchivedFile, error)
	MoveArchived(ctx context.Context, bookFileID uuid.UUID, from string,
		move func(path string) (string, error)) (string, error)
	ArchivedBooks(ctx context.Context, admin uuid.UUID) ([]postgres.ArchivedFile, error)
	ClaimArchived(ctx context.Context, admin, bookFileID, newOwner uuid.UUID) error
	ReleaseHolding(ctx context.Context, bookFileID, user uuid.UUID) ([]postgres.ArchivedFile, error)
}

// ErasureStore is what erasing a person's books needs, instead of archiving
// them.
type ErasureStore interface {
	DeleteAccountErasing(ctx context.Context, actor, target uuid.UUID) (postgres.Erasure, error)
	EraseSoleHoldings(ctx context.Context, admin, target uuid.UUID) (postgres.Erasure, error)
	DiscardPending(ctx context.Context, hashes []string,
		discard func(postgres.StoredCopies) error) (postgres.DiscardResult, error)
	StillOwed(ctx context.Context, hashes []string) (int, error)
	DeleteEmptyEditions(ctx context.Context, editions []uuid.UUID) error
}

// BookKeeper moves a file into the archive, when it is Lexarii's to move.
type BookKeeper interface {
	Keep(path string) (string, error)
}

// AccountHandler serves account deletion and the archive left behind.
type AccountHandler struct {
	Store   AccountStore
	Archive BookKeeper
	Logger  *slog.Logger

	// Eraser erases books instead of archiving them: on account deletion when
	// EraseOnDelete is set, and whenever an administrator asks
	// (POST /admin/users/{id}/erase-files). Nil makes both unavailable.
	Eraser ErasureStore
	// Discard deletes the copies Lexarii stored, and nothing in the library
	// directory: ingest.Discard.
	Discard CopyDiscarder
	// EraseOnDelete erases the books only a deleted person held rather than
	// archiving them (LEXARII_DELETED_ACCOUNT_FILES=erase).
	EraseOnDelete bool
}

func (h *AccountHandler) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// Delete serves DELETE /api/v1/users/{id}.
//
// An administrator may delete anybody; anybody may delete themselves. What goes
// is the person: their notes, positions, conversations and collections, by
// cascade, immediately. What stays is the books they owned, archived against
// their name so an administrator can hand them on and so they are still there
// if the person comes back.
//
// Books they had BORROWED need nothing: their access cascades away and the book
// is simply its owner's again.
func (h *AccountHandler) Delete(w http.ResponseWriter, r *http.Request) {
	actor, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	target, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("user id must be a UUID", err))
		return
	}

	if h.EraseOnDelete && h.Eraser != nil {
		h.deleteErasing(w, r, actor, target)
		return
	}

	archived, err := h.Store.DeleteAccount(r.Context(), actor, target)
	if err != nil {
		respondDeleteError(w, err)
		return
	}

	// The account is already gone. Moving files is best-effort from here: a
	// file that cannot be moved keeps its old path, which is still true, and
	// the book is still archived. Failing the request would report that
	// deletion did not happen, which would be a lie.
	moved := h.keepArchived(r.Context(), archived)

	h.log().Info("account deleted",
		"actor", actor, "target", target,
		"booksArchived", len(archived), "filesMoved", moved)

	respondJSON(w, http.StatusOK, dto.AccountDeleted{
		BooksArchived: len(archived),
		FilesMoved:    moved,
	})
}

// respondDeleteError answers a refused account deletion.
func respondDeleteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrForbidden):
		respondError(w, &httpError{
			status:  http.StatusForbidden,
			code:    "not_an_admin",
			message: "only an administrator can delete somebody else's account",
		})
	case errors.Is(err, store.ErrInvalidQuery):
		respondError(w, Invalid(err.Error(), err))
	case errors.Is(err, store.ErrNotFound):
		respondError(w, NotFound("user"))
	default:
		respondError(w, err)
	}
}

// deleteErasing deletes an account and erases, rather than archives, the
// books only that person held.
//
// The records go with the account, in one transaction that also records what
// it owes the disk. The stored copies are deleted straight after, under each
// file's lock (payErasure); whatever cannot be deleted yet - the disk
// refused, or somebody's upload of the same bytes is waiting - stays owed and
// is finished later (FinishErasures), and the reply counts it. The editions
// and works the erased files leave empty are tidied last, best effort.
func (h *AccountHandler) deleteErasing(w http.ResponseWriter, r *http.Request, actor, target uuid.UUID) {
	ctx := context.WithoutCancel(r.Context())
	erasure, err := h.Eraser.DeleteAccountErasing(ctx, actor, target)
	if err != nil {
		respondDeleteError(w, err)
		return
	}
	deleted, pending := h.payErasure(ctx, erasure)

	h.log().Info("account deleted, books erased",
		"actor", actor, "target", target, "booksErased", len(erasure.Erased),
		"copiesDeleted", deleted, "copiesPending", pending, "booksKept", erasure.Kept)
	respondJSON(w, http.StatusOK, dto.AccountDeleted{
		BooksArchived: erasure.Kept,
		FilesErased:   len(erasure.Erased),
		CopiesPending: pending,
	})
}

// EraseFiles serves POST /api/v1/admin/users/{id}/erase-files: erase, now,
// the books only this person holds, without deleting their account.
// Administrators only.
//
// For a retention rule that reaches a person's books before it reaches the
// person. Books they share with another holder stay; so does one somebody
// else's upload of the same bytes is still waiting for.
func (h *AccountHandler) EraseFiles(w http.ResponseWriter, r *http.Request) {
	admin, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	target, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("user id must be a UUID", err))
		return
	}
	if h.Eraser == nil {
		respondError(w, NotFound("erasing"))
		return
	}

	ctx := context.WithoutCancel(r.Context())
	erasure, err := h.Eraser.EraseSoleHoldings(ctx, admin, target)
	// Batches before a failure stay erased, and what they owe is paid.
	deleted, pending := h.payErasure(ctx, erasure)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrForbidden):
			respondError(w, &httpError{
				status:  http.StatusForbidden,
				code:    "not_an_admin",
				message: "only an administrator can erase somebody's books",
			})
		case errors.Is(err, store.ErrNotFound):
			respondError(w, NotFound("user"))
		default:
			h.log().Error("erasing books failed part-way; what was erased stays erased, "+
				"and asking again finishes it", "target", target,
				"booksErased", len(erasure.Erased), "error", err)
			respondError(w, err)
		}
		return
	}

	h.log().Info("books erased", "admin", admin, "target", target, "booksErased", len(erasure.Erased),
		"copiesDeleted", deleted, "copiesPending", pending, "booksKept", erasure.Kept,
		"uploadsWithdrawn", erasure.Withdrawn)
	respondJSON(w, http.StatusOK, dto.FilesErased{
		FilesErased:   len(erasure.Erased),
		CopiesDeleted: deleted,
		CopiesPending: pending,
		Kept:          erasure.Kept,
		Withdrawn:     erasure.Withdrawn,
	})
}

// payErasure deletes the stored copies a committed erasure owes and tidies
// the editions it left empty. It reports how many copies it deleted, and how
// many of the owed hashes are still owed - waiting for an upload of the same
// bytes, or refused by the disk - which FinishErasures settles later.
func (h *AccountHandler) payErasure(ctx context.Context, erasure postgres.Erasure) (deleted, pending int) {
	if len(erasure.Owed) > 0 {
		if h.Discard != nil {
			if _, err := h.Eraser.DiscardPending(ctx, erasure.Owed, h.discardCounting(&deleted)); err != nil {
				h.log().Warn("deleting erased books' copies failed; they stay owed and are tried again",
					"error", err)
			}
		}
		// What is actually still owed, counted rather than worked out: a
		// debt another pass paid meanwhile is paid.
		n, err := h.Eraser.StillOwed(ctx, erasure.Owed)
		if err != nil {
			h.log().Warn("counting what erased books still owe failed", "error", err)
			n = len(erasure.Owed)
		}
		pending = n
	}
	if err := h.Eraser.DeleteEmptyEditions(ctx, erasure.Editions); err != nil {
		h.log().Warn("deleting the editions erased books left empty failed", "error", err)
	}
	return deleted, pending
}

// discardCounting deletes stored copies, counting them. The archive directory
// is listed once for the whole pass rather than once per file.
func (h *AccountHandler) discardCounting(count *int) func(postgres.StoredCopies) error {
	deleteCopies := StoredCopyDeleter(h.Discard)
	return func(c postgres.StoredCopies) error {
		n, err := deleteCopies(c)
		*count += n
		return err
	}
}

// FinishErasures pays what erasures still owe the disk - copies that could
// not be deleted when they were erased, a server stopped in between, an
// upload of the same bytes since abandoned, an archive record expired in
// erase mode - once now and then every interval until ctx ends.
func (h *AccountHandler) FinishErasures(ctx context.Context, interval time.Duration) {
	if h.Eraser == nil || h.Discard == nil {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	// Each debt that has waited a day for an upload is mentioned once, not
	// every hour.
	reported := map[string]bool{}
	for {
		deleted := 0
		result, err := h.Eraser.DiscardPending(ctx, nil, h.discardCounting(&deleted))
		switch {
		case err != nil && ctx.Err() == nil:
			h.log().Warn("paying what erasures owe the disk failed", "error", err)
		case result.Paid+result.Failed > 0:
			h.log().Info("paid what erasures owed the disk", "settled", result.Paid,
				"copiesDeleted", deleted, "waiting", result.Waiting, "failed", result.Failed)
		}
		for _, hash := range result.Stale {
			if !reported[hash] {
				reported[hash] = true
				h.log().Warn("an erased book's stored copy has waited over a day for an upload of the same bytes",
					"contentHash", hash)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// keepArchived moves the files that were just archived into the archive, when
// they are Lexarii's to move, and reports how many it moved.
//
// The rows are already committed. Moving files is best-effort from here: a
// file that cannot be moved keeps its old path, which is still true, and the
// book is still archived. Failing the request would report that the deletion
// did not happen, which would be a lie. Each move happens under the file's
// lock and only if it is still archived (MoveArchived), so a book somebody
// uploads again meanwhile is left where their upload expects it. A file that
// is not Lexarii's to move - it lives in the library directory, which is
// read-only and somebody else's - is left alone by Keep.
func (h *AccountHandler) keepArchived(ctx context.Context, archived []postgres.ArchivedFile) int {
	moved := 0
	for _, file := range archived {
		if h.Archive == nil {
			break
		}
		to, err := h.Store.MoveArchived(ctx, file.BookFileID, file.Path, h.Archive.Keep)
		if err != nil {
			h.log().Warn("could not archive a book",
				"bookFileId", file.BookFileID, "path", file.Path, "error", err)
			continue
		}
		if to != "" {
			moved++
		}
	}
	return moved
}

// ReleaseHolding serves DELETE /api/v1/books/{id}/holding: take a book off your
// own shelf.
//
// The caller stops holding that file. Nobody else's holding changes, and
// nobody can end anybody else's: there is no administrator override here. When
// they were its last holder the book is archived and, if Lexarii stores it,
// moved, exactly as when an account is deleted.
//
// 204 either way. Whether anybody else still holds the file is not the
// caller's to learn, and "archived" or "kept" in the reply would tell them.
// Not holding it is 404, the same as a file that does not exist.
func (h *AccountHandler) ReleaseHolding(w http.ResponseWriter, r *http.Request) {
	user, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}

	archived, err := h.Store.ReleaseHolding(r.Context(), id, user)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("book"))
			return
		}
		respondError(w, err)
		return
	}

	moved := h.keepArchived(r.Context(), archived)
	h.log().Info("book taken off a shelf",
		"user", user, "bookFileId", id, "archived", len(archived) > 0, "fileMoved", moved > 0)

	respondJSON(w, http.StatusNoContent, nil)
}

// Archived serves GET /api/v1/archive.
func (h *AccountHandler) Archived(w http.ResponseWriter, r *http.Request) {
	admin, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	books, err := h.Store.ArchivedBooks(r.Context(), admin)
	if err != nil {
		respondArchiveError(w, err)
		return
	}

	out := dto.ArchivedBooks{Items: make([]dto.ArchivedBook, 0, len(books))}
	for _, b := range books {
		out.Items = append(out.Items, dto.ArchivedBook{
			BookFileID: b.BookFileID.String(),
			Title:      b.Title,
			Format:     b.Format,
			LeftBy:     b.ArchivedOwner,
			ArchivedAt: b.ArchivedAt,
			// Whether the bytes were moved into Lexarii's archive, or left in
			// a library directory that is not ours to touch.
			Kept: b.ArchivedPath != "",
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// Claim serves POST /api/v1/archive/{id}/claim.
func (h *AccountHandler) Claim(w http.ResponseWriter, r *http.Request) {
	admin, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}
	bookFileID, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return
	}

	// An absent body is fine: it means "give it to me", which is the common
	// case and should not require a client to send `{}`.
	var body dto.ClaimRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
			respondError(w, Invalid("body must name an owner", err))
			return
		}
	}

	// An empty owner means "mine", which is what an administrator tidying up
	// after somebody almost always wants.
	newOwner := admin
	if body.OwnerID != "" {
		newOwner, err = uuid.Parse(body.OwnerID)
		if err != nil {
			respondError(w, Invalid("ownerId must be a UUID", err))
			return
		}
	}

	if err := h.Store.ClaimArchived(r.Context(), admin, bookFileID, newOwner); err != nil {
		respondArchiveError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// respondArchiveError keeps "you are not an administrator" distinct from "there
// is no such book", because neither is a secret from a signed-in account.
func respondArchiveError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrForbidden) {
		respondError(w, &httpError{
			status:  http.StatusForbidden,
			code:    "not_an_admin",
			message: "only an administrator can see or claim archived books",
		})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		respondError(w, NotFound("archived book"))
		return
	}
	respondError(w, err)
}
