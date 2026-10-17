// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// SharingStore is what the sharing handlers need.
type SharingStore interface {
	People(ctx context.Context, excluding uuid.UUID) ([]postgres.Person, error)
	BookShares(ctx context.Context, bookFileID, owner uuid.UUID) ([]postgres.Share, error)
	ShareBook(ctx context.Context, bookFileID, owner, viewer uuid.UUID, canDownload, annotations bool) error
	UnshareBook(ctx context.Context, bookFileID, owner, viewer uuid.UUID) error
	BookAccessLog(ctx context.Context, bookFileID, owner uuid.UUID) ([]postgres.AccessRecord, error)
	SharingActivity(ctx context.Context, owner uuid.UUID, since time.Time, limit int) ([]postgres.ShareActivity, error)
	ShareAnnotations(ctx context.Context, bookFileID, author, viewer uuid.UUID) error
	UnshareAnnotations(ctx context.Context, bookFileID, author, viewer uuid.UUID) error
	AnnotationShares(ctx context.Context, bookFileID, author uuid.UUID) ([]postgres.Person, error)
	PersonNamed(ctx context.Context, username string) (postgres.Person, error)
	MayLend(ctx context.Context, bookFileID, owner uuid.UUID) error
	MayShareNotes(ctx context.Context, bookFileID, author uuid.UUID) error
}

// SharingHandler lets an owner say who may read their books.
//
// Every one of these is owner-only, and the store decides that rather than the
// handler: ownership is read from the row inside the statement that writes, so
// a request cannot claim to be sharing on someone else's behalf. A book that is
// not yours reports not-found, because "you may not share this" would confirm
// it exists.
type SharingHandler struct {
	Store SharingStore
	// Unlisted keeps the installation's people from being listed to everybody
	// (LEXARII_PEOPLE_DIRECTORY=none): GET /people answers an empty list to
	// anybody but an administrator, and lending names the recipient by
	// username instead. False lists everybody, as it always did.
	Unlisted bool
	// Accounts decides who is an administrator when people are unlisted.
	// Nil means nobody is.
	Accounts AdminStore
	// UnknownNames limits how many names that match nobody one person may
	// try when sharing by username, so somebody holding a book cannot run
	// through a list of guesses (auth.NewThrottle: 8 in 15 minutes, then 15
	// minutes refused). Nil does not limit.
	UnknownNames *auth.Throttle
}

// codeUserNotFound is a username that names no account.
const codeUserNotFound ErrorCode = "user_not_found"

// codeTooManyUnknownNames is a person who has named too many accounts that
// do not exist, lately.
const codeTooManyUnknownNames ErrorCode = "too_many_unknown_names"

// People serves GET /api/v1/people.
//
// The accounts a book can be shared with. Usernames and nothing else: a share
// picker needs a name to point at, and no part of it needs anyone's email
// address or the shape of their library.
//
// With people unlisted, only an administrator gets the list. Everybody else
// gets an empty one - 200, not 404, so a client that does not know the
// setting shows "nobody to share with" rather than an error - and names a
// recipient by username (POST /books/{id}/shares).
func (h *SharingHandler) People(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	if h.Unlisted {
		admin := false
		if h.Accounts != nil {
			user, err := h.Accounts.User(r.Context(), userID)
			if err != nil {
				respondError(w, err)
				return
			}
			admin = user.IsAdmin
		}
		if !admin {
			respondJSON(w, http.StatusOK, dto.PersonList{Items: []dto.Person{}})
			return
		}
	}

	people, err := h.Store.People(r.Context(), userID)
	if err != nil {
		respondError(w, err)
		return
	}

	out := dto.PersonList{Items: make([]dto.Person, 0, len(people))}
	for _, p := range people {
		out.Items = append(out.Items, dto.Person{
			ID:       p.ID.String(),
			Username: p.Username,
			IsAdmin:  p.IsAdmin,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// List serves GET /api/v1/books/{id}/shares.
func (h *SharingHandler) List(w http.ResponseWriter, r *http.Request) {
	owner, id, ok := h.target(w, r)
	if !ok {
		return
	}

	shares, err := h.Store.BookShares(r.Context(), id, owner)
	if err != nil {
		respondShareError(w, err)
		return
	}

	out := dto.ShareList{Items: make([]dto.Share, 0, len(shares))}
	for _, s := range shares {
		out.Items = append(out.Items, dto.Share{
			UserID:      s.UserID.String(),
			Username:    s.Username,
			CanDownload: s.CanDownload,
			Annotations: s.Annotations,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// Put serves PUT /api/v1/books/{id}/shares/{userId}.
//
// Idempotent, so the UI can treat it as "make it be this" rather than having to
// know whether a share already exists.
func (h *SharingHandler) Put(w http.ResponseWriter, r *http.Request) {
	owner, id, ok := h.target(w, r)
	if !ok {
		return
	}
	viewer, ok := personParam(w, r)
	if !ok {
		return
	}

	var body dto.ShareRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must describe the share", err))
		return
	}

	if err := h.Store.ShareBook(r.Context(), id, owner, viewer,
		body.CanDownload, body.Annotations); err != nil {
		respondShareError(w, err)
		return
	}
	respondJSON(w, http.StatusNoContent, nil)
}

// PostByUsername serves POST /api/v1/books/{id}/shares: lending a book to
// somebody named by their username, for a person who is not shown a list of
// everybody. The same grant as Put, and as idempotent; the reply is the
// share, with the account's id for changing or ending it.
//
// An unknown name is 404 user_not_found, distinct from the book's own 404.
// That says whether a username exists - which sign-in deliberately does not -
// and is the price of lending by name: a reply that hid it would leave
// somebody believing they had lent a book to a name nobody has. Usernames are
// how people here name each other, not credentials. Two things keep it from
// being a directory by other means. The book is checked FIRST: one that is
// not the caller's to lend is the plain not-found it always was, whatever
// name came with it, and the name is never looked up - so only somebody
// holding a book can ask at all. And names that match nobody are counted per
// person (UnknownNames): past the limit the answer is 429, for any name,
// until the window passes.
func (h *SharingHandler) PostByUsername(w http.ResponseWriter, r *http.Request) {
	owner, id, ok := h.target(w, r)
	if !ok {
		return
	}

	var body dto.ShareByUsername
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must name the person and describe the share", err))
		return
	}
	if err := h.Store.MayLend(r.Context(), id, owner); err != nil {
		respondShareError(w, err)
		return
	}
	person, ok := h.named(w, r, owner, body.Username)
	if !ok {
		return
	}

	if err := h.Store.ShareBook(r.Context(), id, owner, person.ID,
		body.CanDownload, body.Annotations); err != nil {
		respondShareError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, dto.Share{
		UserID:      person.ID.String(),
		Username:    person.Username,
		CanDownload: body.CanDownload,
		Annotations: body.Annotations,
	})
}

// named resolves a username to an account for this caller, answering 400 for
// no name, 404 user_not_found for a name nobody has, and 429 once the caller
// has named too many of those lately. The caller must already have been
// found entitled to share the book: this is the only step that says anything
// about who has an account.
func (h *SharingHandler) named(w http.ResponseWriter, r *http.Request, caller uuid.UUID, username string) (postgres.Person, bool) {
	if strings.TrimSpace(username) == "" {
		respondError(w, Invalid("username is required", nil))
		return postgres.Person{}, false
	}
	// Per person, not per address: the caller is signed in, and it is their
	// guesses being counted. Every lookup is counted as a miss BEFORE it is
	// made, in one step with the check (Reserve), and handed back if the
	// name exists: checked and counted separately, any number of concurrent
	// requests would pass the check before the first miss was counted. A
	// caller past the limit learns nothing about any name, known or not.
	key := "unknown-name:" + caller.String()
	if allowed, wait := h.UnknownNames.Reserve(key); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		respondError(w, &httpError{
			status:  http.StatusTooManyRequests,
			code:    codeTooManyUnknownNames,
			message: "too many names that match nobody; try again shortly",
		})
		return postgres.Person{}, false
	}
	person, err := h.Store.PersonNamed(r.Context(), username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// A miss: the reservation stands.
			respondError(w, &httpError{
				status:  http.StatusNotFound,
				code:    codeUserNotFound,
				message: "there is no account with that username",
			})
			return postgres.Person{}, false
		}
		// Not an answer about the name either way.
		h.UnknownNames.Refund(key)
		respondError(w, err)
		return postgres.Person{}, false
	}
	// A hit is handed back, and only its own count: an existing name tried
	// between guesses must not reset the misses before it.
	h.UnknownNames.Refund(key)
	return person, true
}

// Delete serves DELETE /api/v1/books/{id}/shares/{userId}.
func (h *SharingHandler) Delete(w http.ResponseWriter, r *http.Request) {
	owner, id, ok := h.target(w, r)
	if !ok {
		return
	}
	viewer, ok := personParam(w, r)
	if !ok {
		return
	}

	if err := h.Store.UnshareBook(r.Context(), id, owner, viewer); err != nil {
		respondShareError(w, err)
		return
	}
	respondJSON(w, http.StatusNoContent, nil)
}

// AccessLog serves GET /api/v1/books/{id}/access.
//
// Who has read this book, and how much of it. Owner only.
//
// `book_access` records the permission; this records the fact, which is the
// half that makes a leak attributable rather than merely discouraged. Reading
// your own books is not recorded at all, so this is always about other people.
func (h *SharingHandler) AccessLog(w http.ResponseWriter, r *http.Request) {
	owner, id, ok := h.target(w, r)
	if !ok {
		return
	}

	records, err := h.Store.BookAccessLog(r.Context(), id, owner)
	if err != nil {
		respondShareError(w, err)
		return
	}

	out := dto.AccessLog{Items: make([]dto.AccessRecord, 0, len(records))}
	for _, rec := range records {
		out.Items = append(out.Items, dto.AccessRecord{
			UserID:   rec.UserID.String(),
			Username: rec.Username,
			Day:      rec.Day.Format("2006-01-02"),
			Kind:     string(rec.Kind),
			Views:    rec.Views,
			Units:    rec.Units,
			Of:       rec.Of,
			FirstAt:  rec.FirstAt,
			LastAt:   rec.LastAt,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// activityWindow is how far back the activity endpoint looks by default.
//
// A fortnight. Long enough that somebody who opens the app weekly still sees
// what happened, short enough that the list stays about NOW rather than
// becoming a second copy of the whole log - which is what the per-book view is
// for.
const activityWindow = 14 * 24 * time.Hour

// Activity serves GET /api/v1/shares/activity.
//
// What has happened lately to the books this person has lent. The per-book log
// answers "who read THIS book" and needs somebody to already suspect the book;
// this is the question nothing could answer, which is why the log recorded
// faithfully and warned nobody.
func (h *SharingHandler) Activity(w http.ResponseWriter, r *http.Request) {
	owner, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	since := time.Now().Add(-activityWindow)
	records, err := h.Store.SharingActivity(r.Context(), owner, since, 50)
	if err != nil {
		respondShareError(w, err)
		return
	}

	out := dto.SharingActivity{
		Items: make([]dto.ShareActivity, 0, len(records)),
		Since: since,
	}
	for _, rec := range records {
		if rec.Notable {
			out.Notable++
		}
		out.Items = append(out.Items, dto.ShareActivity{
			BookFileID: rec.BookFileID.String(),
			WorkID:     rec.WorkID.String(),
			WorkTitle:  rec.Title,
			UserID:     rec.UserID.String(),
			Username:   rec.Username,
			Day:        rec.Day.Format("2006-01-02"),
			Kind:       string(rec.Kind),
			Views:      rec.Views,
			Units:      rec.Units,
			Of:         rec.Of,
			Notable:    rec.Notable,
			FirstAt:    rec.FirstAt,
			LastAt:     rec.LastAt,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// NoteShares serves GET /api/v1/books/{id}/annotations/shares.
//
// Who this person shares their own marks on this book with. Not who else has
// shared notes on it: that would answer a question about the owner's other
// readers, which is not the asker's business.
func (h *SharingHandler) NoteShares(w http.ResponseWriter, r *http.Request) {
	author, book, ok := h.bookFor(w, r)
	if !ok {
		return
	}

	people, err := h.Store.AnnotationShares(r.Context(), book, author)
	if err != nil {
		respondShareError(w, err)
		return
	}

	out := dto.NoteShareList{
		Items:  make([]string, 0, len(people)),
		People: make([]dto.Recipient, 0, len(people)),
	}
	for _, p := range people {
		out.Items = append(out.Items, p.ID.String())
		out.People = append(out.People, dto.Recipient{ID: p.ID.String(), Username: p.Username})
	}
	respondJSON(w, http.StatusOK, out)
}

// PostNoteShareByUsername serves POST /api/v1/books/{id}/annotations/shares:
// sharing your own marks with somebody named by their username. The reply
// names who they were shared with. The same order and the same limit as
// PostByUsername.
func (h *SharingHandler) PostNoteShareByUsername(w http.ResponseWriter, r *http.Request) {
	author, book, ok := h.bookFor(w, r)
	if !ok {
		return
	}

	var body dto.NoteShareByUsername
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must name the person", err))
		return
	}
	// The book first, as for lending: a book the caller cannot read is the
	// plain not-found, and no name is looked up for it.
	if err := h.Store.MayShareNotes(r.Context(), book, author); err != nil {
		respondShareError(w, err)
		return
	}
	person, ok := h.named(w, r, author, body.Username)
	if !ok {
		return
	}

	if err := h.Store.ShareAnnotations(r.Context(), book, author, person.ID); err != nil {
		respondShareError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, dto.Recipient{ID: person.ID.String(), Username: person.Username})
}

// PutNoteShare serves PUT /api/v1/books/{id}/annotations/shares/{userId}.
//
// Sharing YOUR notes, which needs no permission from the book's owner: a
// borrower's marginalia is theirs. The store checks that the sharer can read
// the book; the recipient is deliberately not checked, because refusing on
// their behalf would say whether they have the book.
func (h *SharingHandler) PutNoteShare(w http.ResponseWriter, r *http.Request) {
	author, book, person, ok := h.notePeople(w, r)
	if !ok {
		return
	}
	if err := h.Store.ShareAnnotations(r.Context(), book, author, person); err != nil {
		respondShareError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteNoteShare serves DELETE /api/v1/books/{id}/annotations/shares/{userId}.
func (h *SharingHandler) DeleteNoteShare(w http.ResponseWriter, r *http.Request) {
	author, book, person, ok := h.notePeople(w, r)
	if !ok {
		return
	}
	if err := h.Store.UnshareAnnotations(r.Context(), book, author, person); err != nil {
		respondShareError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// bookFor reads the signed-in person and the book in the path.
//
// Unlike target it asserts nothing about ownership: these routes are for
// whoever wrote the notes, who is often not the owner.
func (h *SharingHandler) bookFor(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	author, ok := userOrUnauthorised(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	book, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return uuid.Nil, uuid.Nil, false
	}
	return author, book, true
}

func (h *SharingHandler) notePeople(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, uuid.UUID, bool) {
	author, book, ok := h.bookFor(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	person, err := uuid.Parse(r.PathValue("userId"))
	if err != nil {
		respondError(w, Invalid("user id must be a UUID", err))
		return uuid.Nil, uuid.Nil, uuid.Nil, false
	}
	return author, book, person, true
}

// target reads the signed-in owner and the book file being shared.
func (h *SharingHandler) target(w http.ResponseWriter, r *http.Request) (uuid.UUID, uuid.UUID, bool) {
	owner, ok := userOrUnauthorised(w, r)
	if !ok {
		return uuid.Nil, uuid.Nil, false
	}
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		respondError(w, Invalid("book file id must be a UUID", err))
		return uuid.Nil, uuid.Nil, false
	}
	return owner, id, true
}

func personParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("userId"))
	if err != nil {
		respondError(w, Invalid("user id must be a UUID", err))
		return uuid.Nil, false
	}
	return id, true
}

// respondShareError keeps "not yours" and "does not exist" indistinguishable.
func respondShareError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		respondError(w, NotFound("book file"))
		return
	}
	// Sharing with yourself, which the store refuses. The person's mistake,
	// not a server fault.
	if errors.Is(err, store.ErrInvalidQuery) {
		respondError(w, Invalid("a book or notes cannot be shared with yourself", err))
		return
	}
	respondError(w, err)
}
