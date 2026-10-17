// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// Sharing is deliberately narrow: a holder names the people who may read one of
// their books, and separately decides whether those people also get their
// marginalia. There are no links, no public books, and no sharing with someone
// who does not already have an account here.
//
// "Owner" below means a HOLDER: anybody who added the file. A file can have
// several, and each lends it independently. A holder sees and revokes the
// grants they made and no others - another holder's lending is that person's
// business, and listing it would say who they lend books to.
//
// Two rules are enforced in SQL rather than trusted to a handler, because a
// permission check that lives only in one caller is a permission check that the
// second caller forgets:
//
//   - Only a HOLDER of a file may grant it. The INSERT reads the holding from
//     book_file_holders rather than taking it as an argument, so a request
//     cannot claim to be granting on someone else's behalf - and the foreign
//     key from a grant to the holding that made it says the same thing again.
//   - Annotations may only be shared with someone who can already READ the book.
//     Marginalia without the text it marks is both useless and a way of leaking
//     what a book says one highlight at a time.

// Share is one person's access to one book.
type Share struct {
	UserID   uuid.UUID
	Username string
	// CanDownload allows taking the original file, as opposed to reading it.
	CanDownload bool
	// Annotations is true when the owner's marks travel with the book.
	Annotations bool
}

// Person is an account a book can be shared with.
type Person struct {
	ID       uuid.UUID
	Username string
	IsAdmin  bool
}

// People lists the accounts on this installation, for a share picker.
//
// Usernames only. Sharing needs a name to point at, and nothing here needs an
// email address or the shape of anyone's library.
func (p *Pool) People(ctx context.Context, excluding uuid.UUID) ([]Person, error) {
	rows, err := p.Query(ctx, `
		SELECT id, username, is_admin FROM users
		WHERE id <> $1 ORDER BY lower(username)`, excluding)
	if err != nil {
		return nil, fmt.Errorf("reading people: %w", mapError(err))
	}
	defer rows.Close()

	var out []Person
	for rows.Next() {
		var person Person
		if err := rows.Scan(&person.ID, &person.Username, &person.IsAdmin); err != nil {
			return nil, fmt.Errorf("scanning person: %w", err)
		}
		out = append(out, person)
	}
	return out, rows.Err()
}

// PersonNamed finds the account a username names, matched without regard to
// case as usernames are unique without regard to it.
//
// For lending by name on an installation that does not list its people. A
// name that matches nobody is store.ErrNotFound; the handler says so plainly,
// since lending to a name nobody has must not look like success (see
// SharingHandler.PostByUsername for what that does and does not reveal).
func (p *Pool) PersonNamed(ctx context.Context, username string) (Person, error) {
	var person Person
	err := p.QueryRow(ctx, `
		SELECT id, username, is_admin FROM users WHERE lower(username) = lower($1)`,
		strings.TrimSpace(username)).Scan(&person.ID, &person.Username, &person.IsAdmin)
	if err != nil {
		return Person{}, mapError(err)
	}
	return person, nil
}

// BookShares lists who this holder has shared a book with. Only a holder may
// ask, and sees only their own grants.
func (p *Pool) BookShares(ctx context.Context, bookFileID, owner uuid.UUID) ([]Share, error) {
	if err := p.assertOwner(ctx, bookFileID, owner); err != nil {
		return nil, err
	}

	rows, err := p.Query(ctx, `
		SELECT ba.user_id, u.username, ba.can_download,
		       EXISTS (SELECT 1 FROM annotation_access aa
		                WHERE aa.book_file_id = ba.book_file_id
		                  AND aa.owner_id = $2 AND aa.viewer_id = ba.user_id)
		FROM book_access ba
		JOIN users u ON u.id = ba.user_id
		WHERE ba.book_file_id = $1 AND ba.granted_by = $2
		ORDER BY lower(u.username)`, bookFileID, owner)
	if err != nil {
		return nil, fmt.Errorf("reading shares: %w", mapError(err))
	}
	defer rows.Close()

	var out []Share
	for rows.Next() {
		var s Share
		if err := rows.Scan(&s.UserID, &s.Username, &s.CanDownload, &s.Annotations); err != nil {
			return nil, fmt.Errorf("scanning share: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ShareBook grants someone read access to a book, and optionally the owner's
// annotations on it.
//
// Idempotent: sharing again updates the permissions rather than failing, which
// is what a toggle in a UI needs.
func (p *Pool) ShareBook(ctx context.Context, bookFileID, owner, viewer uuid.UUID,
	canDownload, annotations bool) error {

	// Only the self-share is refused. Refusing a viewer because they ALSO hold
	// the file would tell this holder that they do; the grant is harmless,
	// since a holder reads it anyway.
	if owner == viewer {
		return fmt.Errorf("%w: a book cannot be shared with its owner", store.ErrInvalidQuery)
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("sharing: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The holding is read from the table, not taken on trust from the caller.
	tag, err := tx.Exec(ctx, `
		INSERT INTO book_access (book_file_id, user_id, granted_by, can_download)
		SELECT h.book_file_id, $3, h.user_id, $4
		FROM book_file_holders h
		JOIN book_files bf ON bf.id = h.book_file_id
		WHERE h.book_file_id = $1 AND h.user_id = $2 AND bf.archived_at IS NULL
		ON CONFLICT (book_file_id, user_id, granted_by)
		DO UPDATE SET can_download = EXCLUDED.can_download`,
		bookFileID, owner, viewer, canDownload)
	if err != nil {
		return fmt.Errorf("sharing: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		// Either the file does not exist or it is not this person's to share.
		// Both are "not found" to someone who should not learn the difference.
		return errNotFound
	}

	if annotations {
		// The book_access row above is this statement's guarantee that the
		// viewer can read the book, which is the condition for sharing marks on
		// it: highlights without their text leak the text one mark at a time.
		if _, err := tx.Exec(ctx, `
			INSERT INTO annotation_access (book_file_id, owner_id, viewer_id)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, bookFileID, owner, viewer); err != nil {
			return fmt.Errorf("sharing annotations: %w", mapError(err))
		}
	} else if _, err := tx.Exec(ctx, `
		DELETE FROM annotation_access
		WHERE book_file_id = $1 AND owner_id = $2 AND viewer_id = $3`,
		bookFileID, owner, viewer); err != nil {
		return fmt.Errorf("unsharing annotations: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("sharing: %w", mapError(err))
	}
	return nil
}

// UnshareBook withdraws the access this holder gave someone.
//
// Only this holder's grant. If another holder lends them the same file, that is
// a separate decision this one cannot see and does not undo.
//
// The annotation share goes with it. Leaving marginalia visible for a book the
// person can no longer open would be a leak that outlived the decision to stop
// sharing, and is never what revoking meant.
func (p *Pool) UnshareBook(ctx context.Context, bookFileID, owner, viewer uuid.UUID) error {
	if err := p.assertOwner(ctx, bookFileID, owner); err != nil {
		return err
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("unsharing: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		DELETE FROM annotation_access
		WHERE book_file_id = $1 AND owner_id = $2 AND viewer_id = $3`,
		bookFileID, owner, viewer); err != nil {
		return fmt.Errorf("unsharing annotations: %w", mapError(err))
	}
	tag, err := tx.Exec(ctx, `
		DELETE FROM book_access
		WHERE book_file_id = $1 AND user_id = $2 AND granted_by = $3`,
		bookFileID, viewer, owner)
	if err != nil {
		return fmt.Errorf("unsharing: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("unsharing: %w", mapError(err))
	}
	return nil
}

// assertOwner reports not-found unless this person holds the file.
//
// An archived file has no holders, so it fails here too.
func (p *Pool) assertOwner(ctx context.Context, bookFileID, owner uuid.UUID) error {
	var exists bool
	err := p.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM book_file_holders WHERE book_file_id = $1 AND user_id = $2)`,
		bookFileID, owner).Scan(&exists)
	if err != nil {
		return fmt.Errorf("checking ownership: %w", mapError(err))
	}
	if !exists {
		return errNotFound
	}
	return nil
}

// MayLend reports not-found unless this person may lend the file now: they
// hold it and it is not archived - the condition ShareBook writes under.
//
// For asking before anything else is looked up, so a request about a file
// that is not theirs learns nothing else either (lending by username: see
// SharingHandler.PostByUsername). ShareBook still checks for itself, in the
// statement that writes: this answer can be stale by then.
func (p *Pool) MayLend(ctx context.Context, bookFileID, owner uuid.UUID) error {
	var ok bool
	if err := p.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM book_file_holders h
		                 JOIN book_files bf ON bf.id = h.book_file_id
		                WHERE h.book_file_id = $1 AND h.user_id = $2 AND bf.archived_at IS NULL)`,
		bookFileID, owner).Scan(&ok); err != nil {
		return fmt.Errorf("checking a holding: %w", mapError(err))
	}
	if !ok {
		return errNotFound
	}
	return nil
}

// MayShareNotes reports not-found unless this person can read the file - the
// condition ShareAnnotations writes under - for the same purpose as MayLend.
func (p *Pool) MayShareNotes(ctx context.Context, bookFileID, author uuid.UUID) error {
	var ok bool
	if err := p.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM readable_book_files
		                WHERE book_file_id = $1 AND user_id = $2)`,
		bookFileID, author).Scan(&ok); err != nil {
		return fmt.Errorf("checking access: %w", mapError(err))
	}
	if !ok {
		return errNotFound
	}
	return nil
}

// AccessKind is what was served.
type AccessKind string

const (
	// AccessPage is one page of a PDF.
	AccessPage AccessKind = "page"
	// AccessChapter is one spine item of an EPUB.
	AccessChapter AccessKind = "chapter"
	// AccessFile is the whole book, handed over.
	AccessFile AccessKind = "file"
)

// RecordAccess notes that somebody read part of a book that is not theirs.
//
// Reading your OWN library is not recorded - and a book is yours when you hold
// it, whoever else does - and the check is in this statement rather than in the
// caller: an audit trail over a person's own reading is not an audit trail, and
// a rule that lives in one handler is a rule the next handler forgets. A book
// with no holder is not recorded either - there is nobody the record would be
// for.
//
// The row remembers WHO lent it: every holder whose grant let this person in.
// Each holder sees the reading done through their own grants, and that record
// survives the grant being revoked, which is when it is most wanted.
//
// Never returns an error worth failing a request over. Auditing is a side
// effect of reading, and a reader that stops working because the log is
// unavailable has turned a record-keeping problem into an outage.
func (p *Pool) RecordAccess(ctx context.Context, bookFileID, viewer uuid.UUID,
	kind AccessKind, unit int) error {

	if viewer == uuid.Nil {
		return nil
	}

	// units accumulates the DISTINCT pages or chapters seen, which is what
	// makes "how much of this book did they take" answerable. A whole-file
	// download has no unit and records none.
	_, err := p.Exec(ctx, `
		INSERT INTO access_log (book_file_id, user_id, kind, views, units, lent_by)
		SELECT bf.id, $2, $3, 1,
		       CASE WHEN $4::int > 0 THEN ARRAY[$4::int] ELSE '{}'::int[] END,
		       ARRAY(SELECT ba.granted_by FROM book_access ba
		              WHERE ba.book_file_id = bf.id AND ba.user_id = $2
		              ORDER BY ba.granted_by)
		FROM book_files bf
		WHERE bf.id = $1
		  AND EXISTS (SELECT 1 FROM book_file_holders h WHERE h.book_file_id = bf.id)
		  AND NOT EXISTS (SELECT 1 FROM book_file_holders h
		                   WHERE h.book_file_id = bf.id AND h.user_id = $2)
		ON CONFLICT (book_file_id, user_id, day, kind) DO UPDATE
		SET views   = access_log.views + 1,
		    last_at = now(),
		    units   = CASE
		                WHEN $4::int <= 0 OR $4::int = ANY(access_log.units)
		                THEN access_log.units
		                ELSE access_log.units || $4::int
		              END,
		    lent_by = ARRAY(SELECT DISTINCT g
		                      FROM unnest(access_log.lent_by || EXCLUDED.lent_by) AS g
		                     ORDER BY g)`,
		bookFileID, viewer, string(kind), unit)
	if err != nil {
		return fmt.Errorf("recording access: %w", mapError(err))
	}
	return nil
}

// AccessRecord is one person's reading of one book on one day.
type AccessRecord struct {
	UserID   uuid.UUID
	Username string
	Day      time.Time
	Kind     AccessKind
	// Views is how many requests; Units is how many DISTINCT pages or chapters
	// were seen. The gap between them is re-reading.
	Views int
	Units int
	// Of is how many units the book has, so "42 of 558" can be said rather
	// than "42".
	Of      int
	FirstAt time.Time
	LastAt  time.Time
}

// BookAccessLog reports who has read a book through this holder's grants.
// Only a holder may ask, and learns nothing about another holder's lending.
func (p *Pool) BookAccessLog(ctx context.Context, bookFileID, owner uuid.UUID) ([]AccessRecord, error) {
	if err := p.assertOwner(ctx, bookFileID, owner); err != nil {
		return nil, err
	}

	rows, err := p.Query(ctx, `
		SELECT al.user_id, COALESCE(u.username, 'a deleted account'),
		       al.day, al.kind, al.views, cardinality(al.units),
		       COALESCE(bf.unit_count, 0), al.first_at, al.last_at
		FROM access_log al
		JOIN book_files bf ON bf.id = al.book_file_id
		LEFT JOIN users u ON u.id = al.user_id
		WHERE al.book_file_id = $1 AND $2 = ANY(al.lent_by)
		ORDER BY al.last_at DESC`, bookFileID, owner)
	if err != nil {
		return nil, fmt.Errorf("reading the access log: %w", mapError(err))
	}
	defer rows.Close()

	var out []AccessRecord
	for rows.Next() {
		var r AccessRecord
		var userID *uuid.UUID
		var units *int
		if err := rows.Scan(&userID, &r.Username, &r.Day, &r.Kind, &r.Views,
			&units, &r.Of, &r.FirstAt, &r.LastAt); err != nil {
			return nil, fmt.Errorf("scanning access record: %w", err)
		}
		if userID != nil {
			r.UserID = *userID
		}
		if units != nil {
			r.Units = *units
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// NotableShareOfBook is how much of a book somebody has to read in one day
// before it is worth telling the owner.
//
// Half. Not a number anybody can defend precisely, but the shape of the thing
// is clear: a reader working through a chapter a night never approaches it, and
// somebody pulling a book to keep passes it within minutes. Set lower it would
// fire on an ordinary evening's reading, and a warning that fires on ordinary
// behaviour is one people learn to close without reading.
const NotableShareOfBook = 0.5

// ShareActivity is one person's reading of one of the owner's books on one day.
type ShareActivity struct {
	BookFileID uuid.UUID
	// WorkID is what a link points at: the book's page, where the per-book log
	// and the share controls already live.
	WorkID   uuid.UUID
	Title    string
	UserID   uuid.UUID
	Username string
	Day      time.Time
	Kind     AccessKind
	Views    int
	Units    int
	// Of is the book's total units, or 0 when it has no count recorded.
	Of int
	// Notable marks the rows worth interrupting somebody for: most of a book in
	// a day, or a whole-file download. A download is notable whatever its size,
	// because the file left the installation.
	Notable bool
	FirstAt time.Time
	LastAt  time.Time
}

// SharingActivity reports recent reading of every book this person has lent.
//
// BookAccessLog answers "who read THIS book" and needs somebody to already
// suspect the book. This is the other question - "is anything happening to my
// books" - which nothing could answer, so the log recorded faithfully and told
// nobody.
func (p *Pool) SharingActivity(ctx context.Context, owner uuid.UUID, since time.Time, limit int) ([]ShareActivity, error) {
	if owner == uuid.Nil {
		return nil, errNotFound
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	rows, err := p.Query(ctx, `
		SELECT al.book_file_id, w.id, w.title,
		       al.user_id, COALESCE(u.username, 'a deleted account'),
		       al.day, al.kind, al.views, cardinality(al.units),
		       COALESCE(bf.unit_count, 0), al.first_at, al.last_at
		FROM access_log al
		JOIN book_files bf ON bf.id = al.book_file_id
		JOIN editions e ON e.id = bf.edition_id
		JOIN works w ON w.id = e.work_id
		LEFT JOIN users u ON u.id = al.user_id
		-- Holder-scoped in the statement rather than in a handler, like every
		-- other access decision here: books this person holds, read through
		-- grants this person made.
		JOIN book_file_holders h ON h.book_file_id = al.book_file_id AND h.user_id = $1
		WHERE $1 = ANY(al.lent_by) AND al.last_at >= $2
		ORDER BY al.last_at DESC
		LIMIT $3`, owner, since, limit)
	if err != nil {
		return nil, fmt.Errorf("reading sharing activity: %w", mapError(err))
	}
	defer rows.Close()

	var out []ShareActivity
	for rows.Next() {
		var a ShareActivity
		var userID *uuid.UUID
		var units *int
		if err := rows.Scan(&a.BookFileID, &a.WorkID, &a.Title, &userID, &a.Username,
			&a.Day, &a.Kind, &a.Views, &units, &a.Of, &a.FirstAt, &a.LastAt); err != nil {
			return nil, fmt.Errorf("scanning sharing activity: %w", err)
		}
		if userID != nil {
			a.UserID = *userID
		}
		if units != nil {
			a.Units = *units
		}
		// A book with no unit count recorded cannot be measured against itself,
		// so only a download counts as notable there. Guessing would produce a
		// warning nobody could check.
		a.Notable = a.Kind == AccessFile ||
			(a.Of > 0 && float64(a.Units) >= float64(a.Of)*NotableShareOfBook)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ShareAnnotations shares one person's marks on a book with somebody else.
//
// Separate from ShareBook, which only the owner may call. A borrower's notes on
// a borrowed book are theirs: the store has always keyed annotation_access on
// whoever wrote them, and nothing could reach that except the owner's own share
// panel - so a reader could be handed a book and had no way to answer in it.
//
// The SHARER has to be able to read the book, which is checked here. The
// recipient deliberately is NOT: refusing, or even reporting, a recipient who
// lacks access would tell a borrower who else the owner lent the book to. The
// share is recorded as an intention, and Annotations enforces book access at
// the point of reading - so a recipient without the book sees nothing, and one
// who is given it later sees the notes then.
func (p *Pool) ShareAnnotations(ctx context.Context, bookFileID, author, viewer uuid.UUID) error {
	if author == viewer {
		return fmt.Errorf("%w: notes cannot be shared with their author", store.ErrInvalidQuery)
	}

	tag, err := p.Exec(ctx, `
		INSERT INTO annotation_access (book_file_id, owner_id, viewer_id)
		SELECT $1, $2, $3
		WHERE EXISTS (SELECT 1 FROM readable_book_files rbf
		               WHERE rbf.book_file_id = $1 AND rbf.user_id = $2)
		ON CONFLICT DO NOTHING`, bookFileID, author, viewer)
	if err != nil {
		return fmt.Errorf("sharing notes: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		// Either the sharer cannot read the book - in which case it is not
		// theirs to annotate, let alone share - or the row already existed.
		// Distinguishing them costs a second query and tells the caller
		// nothing they can act on, so a repeat share is simply not an error.
		var exists bool
		if err := p.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM annotation_access
			                WHERE book_file_id = $1 AND owner_id = $2 AND viewer_id = $3)`,
			bookFileID, author, viewer).Scan(&exists); err != nil {
			return fmt.Errorf("sharing notes: %w", mapError(err))
		}
		if !exists {
			return errNotFound
		}
	}
	return nil
}

// UnshareAnnotations withdraws a share of one person's marks.
func (p *Pool) UnshareAnnotations(ctx context.Context, bookFileID, author, viewer uuid.UUID) error {
	if _, err := p.Exec(ctx, `
		DELETE FROM annotation_access
		WHERE book_file_id = $1 AND owner_id = $2 AND viewer_id = $3`,
		bookFileID, author, viewer); err != nil {
		return fmt.Errorf("unsharing notes: %w", mapError(err))
	}
	return nil
}

// AnnotationShares lists who this person shares their marks on a book with.
//
// Their OWN shares only. Who else has shared notes on the same book is not
// theirs to see, and would answer a question about the owner's other readers.
//
// With their usernames: on an installation that does not list its people the
// client has no other way to name somebody it shares with.
func (p *Pool) AnnotationShares(ctx context.Context, bookFileID, author uuid.UUID) ([]Person, error) {
	rows, err := p.Query(ctx, `
		SELECT aa.viewer_id, u.username, u.is_admin
		FROM annotation_access aa
		JOIN users u ON u.id = aa.viewer_id
		WHERE aa.book_file_id = $1 AND aa.owner_id = $2
		ORDER BY aa.created_at`, bookFileID, author)
	if err != nil {
		return nil, fmt.Errorf("reading note shares: %w", mapError(err))
	}
	defer rows.Close()

	var out []Person
	for rows.Next() {
		var person Person
		if err := rows.Scan(&person.ID, &person.Username, &person.IsAdmin); err != nil {
			return nil, fmt.Errorf("scanning note share: %w", err)
		}
		out = append(out, person)
	}
	return out, rows.Err()
}
