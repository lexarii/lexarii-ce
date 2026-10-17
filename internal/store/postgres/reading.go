// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// DefaultUsername is the account Lexarii uses when no authentication is
// configured.
const DefaultUsername = "lexarii"

// EnsureDefaultUser returns the single-user account, creating it if needed.
//
// Idempotent: safe to call on every startup.
//
// It creates a PERSON, not a credential. Since identities moved to their own
// table, a row here carries no way to sign in, and AdoptOrCreateLocalUser is
// what attaches one. That separation is the point: an account with no usable
// credential is why HasAnyUser asks whether anyone can actually log in rather
// than whether any user row exists.
func (p *Pool) EnsureDefaultUser(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	err := p.QueryRow(ctx, `
		INSERT INTO users (username, is_admin)
		VALUES ($1, true)
		ON CONFLICT (lower(username)) DO UPDATE SET username = users.username
		RETURNING id`, DefaultUsername,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("ensuring the default user: %w", mapError(err))
	}
	return id, nil
}

// SaveProgress records where a user has read to.
//
// Two facts are stored, because they are two different facts.
//
// The CURRENT position takes the most recent write. It is where the reader is,
// and a reader who turns back to an earlier chapter has moved there - refusing
// to record that is refusing to record reading. Under the old furthest-wins
// rule every backward navigation was silently discarded, so jumping back via
// the contents and closing the book reopened it where you had last got to,
// never where you actually were.
//
// The FURTHEST position only ever advances. That is what protects the case the
// old rule existed for: a phone that read ahead is not rewound by a laptop that
// saves an earlier position, and "62% read" on a shelf keeps meaning how far
// through the book someone has got.
func (p *Pool) SaveProgress(ctx context.Context, progress domain.ReadingProgress) (domain.ReadingProgress, error) {
	if err := progress.Position.Validate(); err != nil {
		return domain.ReadingProgress{}, fmt.Errorf("saving progress: %w", err)
	}

	// A position on a file the reader cannot open is refused exactly as if the
	// file did not exist, as a mark on one is.
	readable, err := p.canOpen(ctx, progress.BookFileID, progress.UserID)
	if err != nil {
		return domain.ReadingProgress{}, fmt.Errorf("saving progress: %w", err)
	}
	if !readable {
		return domain.ReadingProgress{}, fmt.Errorf("saving progress: %w", errNotFound)
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return domain.ReadingProgress{}, fmt.Errorf("saving progress: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockReadingProgress(ctx, tx, progress.UserID, progress.BookFileID); err != nil {
		return domain.ReadingProgress{}, fmt.Errorf("saving progress: %w", err)
	}

	furthest, furthestPercentIn, err := furthestFor(ctx, tx,
		progress.UserID, progress.BookFileID, progress.Position, progress.Percent)
	if err != nil {
		return domain.ReadingProgress{}, fmt.Errorf("saving progress: %w", err)
	}

	var out domain.ReadingProgress
	var percent, furthestPercent *float32
	var major, furthestMajor int

	err = tx.QueryRow(ctx, `
		INSERT INTO reading_progress
			(user_id, book_file_id, major, path, "offset", percent,
			 furthest_major, furthest_path, furthest_offset, furthest_percent, last_read_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, now())
		ON CONFLICT (user_id, book_file_id) DO UPDATE SET
			-- Where they are: the newest write, backwards included.
			major = EXCLUDED.major,
			path = EXCLUDED.path,
			"offset" = EXCLUDED."offset",
			percent = EXCLUDED.percent,

			-- How far they got: decided in Go, above, by furthestFor - not
			-- here. Comparing (major, path, offset) as a SQL row tuple orders
			-- path as TEXT, the same mistake migration 00015 already undid for
			-- annotations: "p[10]" sorts before "p[9]", so a reader who turned
			-- the page from the 9th paragraph to the 10th had "how far they
			-- got" silently NOT advance, and SQL and Go were free to disagree
			-- about which of two positions was further, because each carried
			-- its own separate idea of path order.
			furthest_major = $7,
			furthest_path = $8,
			furthest_offset = $9,
			furthest_percent = $10,
			last_read_at = now()
		RETURNING user_id, book_file_id, major, path, "offset", percent,
		          furthest_major, furthest_path, furthest_offset, furthest_percent,
		          last_read_at, updated_at`,
		progress.UserID, progress.BookFileID,
		progress.Position.SpineIndex+progress.Position.Page, // one of the two is zero
		progress.Position.ElementPath, progress.Position.CharOffset, progress.Percent,
		furthest.SpineIndex+furthest.Page, furthest.ElementPath, furthest.CharOffset, furthestPercentIn,
	).Scan(&out.UserID, &out.BookFileID, &major, &out.Position.ElementPath,
		&out.Position.CharOffset, &percent,
		&furthestMajor, &out.Furthest.ElementPath, &out.Furthest.CharOffset, &furthestPercent,
		&out.LastReadAt, &out.UpdatedAt)
	if err != nil {
		return domain.ReadingProgress{}, fmt.Errorf("saving progress: %w", mapError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ReadingProgress{}, fmt.Errorf("saving progress: %w", mapError(err))
	}

	out.Percent = percent
	out.FurthestPercent = furthestPercent
	applyPositionFormat(&out, progress.Position.Format, major, furthestMajor)
	return out, nil
}

// lockReadingProgress serialises concurrent saves for one (user, book) pair
// before either one reads or writes it, for the rest of the caller's
// transaction.
//
// furthestFor's own SELECT ... FOR UPDATE is not enough on its own: it locks
// a ROW, and a row that does not exist yet cannot be locked. Two FIRST-ever
// saves for the same book, racing, both find no row, both treat their own
// position as furthest with nothing to compare against, and both proceed to
// INSERT ... ON CONFLICT DO UPDATE - whichever commits second overwrites
// furthest unconditionally with its own value, even if it is the one that is
// actually behind. An advisory lock has no row to be missing: it is keyed on
// the pair itself, taken out before the SELECT even runs, and held for the
// transaction, so the second save to arrive simply waits for the first to
// commit and then finds a real row to compare against.
//
// hashtext folds each uuid down to the int4 pg_advisory_xact_lock's two-key
// form wants. A collision between two different (user, book) pairs would only
// ever cost an unrelated pair a moment of unnecessary serialisation, never a
// wrong answer - the actual exclusion is still keyed correctly by the
// transaction that holds it.
func lockReadingProgress(ctx context.Context, tx pgx.Tx, userID, bookFileID uuid.UUID) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1::text), hashtext($2::text))`,
		userID, bookFileID); err != nil {
		return fmt.Errorf("locking reading progress: %w", mapError(err))
	}
	return nil
}

// furthestFor decides what FURTHEST position and percent an incoming save
// should record, using domain.FurtherPosition - the one comparison this and
// domain.Furthest both defer to - rather than a second, independent one
// written in SQL, which is what let the database and Go disagree about path
// order in the first place (see SaveProgress).
//
// When FurtherPosition itself cannot decide - true ties only, since it
// already falls back to percent once structure runs out - the existing
// furthest is kept rather than guessed at: "how far they got" is a claim this
// function must never advance on a hunch, only ever leave alone. There is no
// recency signal to fall back on further, the way domain.Furthest has one:
// this is ONE save being merged against the one row already on file, not two
// independent records with their own timestamps to compare.
//
// Locks the existing row, if any, for the rest of the caller's transaction,
// so two concurrent saves for the same book cannot both read the same stored
// furthest and both believe they are the one moving it forward.
func furthestFor(ctx context.Context, tx pgx.Tx, userID, bookFileID uuid.UUID,
	incoming domain.Locator, incomingPercent *float32) (domain.Locator, *float32, error) {

	var major, offset int
	var path string
	var percent *float32
	err := tx.QueryRow(ctx, `
		SELECT furthest_major, furthest_path, furthest_offset, furthest_percent
		FROM reading_progress
		WHERE user_id = $1 AND book_file_id = $2
		FOR UPDATE`, userID, bookFileID,
	).Scan(&major, &path, &offset, &percent)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing stored yet for this book: the incoming position IS the
		// furthest, by definition, with nothing to compare it against.
		return incoming, incomingPercent, nil
	}
	if err != nil {
		return domain.Locator{}, nil, fmt.Errorf("locking the stored furthest position: %w", mapError(err))
	}

	existing := domain.Locator{Format: incoming.Format, ElementPath: path, CharOffset: offset}
	if incoming.Format == domain.FormatPDF {
		existing.Page = major
	} else {
		existing.SpineIndex = major
	}

	if incomingIsFurther, decided := domain.FurtherPosition(existing, percent, incoming, incomingPercent); decided && incomingIsFurther {
		return incoming, incomingPercent, nil
	}
	return existing, percent, nil
}

// applyPositionFormat restores what the stored `major` columns meant, since
// they carry the spine index for EPUB and the page for PDF and scan back
// identically either way.
func applyPositionFormat(out *domain.ReadingProgress, format domain.Format, major, furthestMajor int) {
	out.Position.Format = format
	out.Furthest.Format = format
	if format == domain.FormatPDF {
		out.Position.Page = major
		out.Furthest.Page = furthestMajor
		return
	}
	out.Position.SpineIndex = major
	out.Furthest.SpineIndex = furthestMajor
}

// ReadingProgress reads where a user has read to, or ErrNotFound if they have
// not opened this file.
//
// Named for the thing it reads rather than just "Progress": the job queue
// already has a Progress method, and a reading position and a job's percentage
// are entirely different concepts that should not share a name.
func (p *Pool) ReadingProgress(ctx context.Context, userID, bookFileID uuid.UUID) (domain.ReadingProgress, error) {
	var out domain.ReadingProgress
	var percent, furthestPercent *float32
	var major, furthestMajor int
	var format domain.Format

	err := p.QueryRow(ctx, `
		SELECT rp.user_id, rp.book_file_id, rp.major, rp.path, rp."offset", rp.percent,
		       rp.furthest_major, rp.furthest_path, rp.furthest_offset, rp.furthest_percent,
		       rp.last_read_at, rp.updated_at, bf.format
		FROM reading_progress rp
		JOIN book_files bf ON bf.id = rp.book_file_id
		WHERE rp.user_id = $1 AND rp.book_file_id = $2`, userID, bookFileID,
	).Scan(&out.UserID, &out.BookFileID, &major, &out.Position.ElementPath,
		&out.Position.CharOffset, &percent,
		&furthestMajor, &out.Furthest.ElementPath, &out.Furthest.CharOffset, &furthestPercent,
		&out.LastReadAt, &out.UpdatedAt, &format)
	if err != nil {
		return domain.ReadingProgress{}, mapError(err)
	}

	out.Percent = percent
	out.FurthestPercent = furthestPercent
	applyPositionFormat(&out, format, major, furthestMajor)
	return out, nil
}

// ContinueReading lists the books a user has open, most recently read first.
//
// Percent here is the FURTHEST position, because a shelf answers "how far
// through this am I?". Opening the book resumes at the current position, which
// may be earlier if the reader turned back.
func (p *Pool) ContinueReading(ctx context.Context, userID uuid.UUID, limit int) ([]ContinueItem, error) {
	if limit <= 0 || limit > 50 {
		limit = 12
	}

	rows, err := p.Query(ctx, `
		SELECT w.id, w.title, bf.id, bf.format, rp.furthest_percent, rp.last_read_at
		FROM reading_progress rp
		JOIN book_files bf ON bf.id = rp.book_file_id
		-- Books someone can still open. Reading position outlives access: a
		-- book lent and then taken back would otherwise keep its title on the
		-- borrower's shelf, which is a small leak that survives the decision to
		-- stop sharing.
		JOIN readable_book_files rbf
		  ON rbf.book_file_id = bf.id AND rbf.user_id = rp.user_id
		JOIN editions e ON e.id = bf.edition_id
		JOIN works w ON w.id = e.work_id
		WHERE rp.user_id = $1 AND bf.missing_since IS NULL
		ORDER BY rp.last_read_at DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("reading continue list: %w", mapError(err))
	}
	defer rows.Close()

	var out []ContinueItem
	for rows.Next() {
		var it ContinueItem
		if err := rows.Scan(&it.WorkID, &it.Title, &it.BookFileID, &it.Format,
			&it.Percent, &it.LastReadAt); err != nil {
			return nil, fmt.Errorf("scanning continue item: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ContinueItem is a book the user has started.
type ContinueItem struct {
	WorkID     uuid.UUID
	Title      string
	BookFileID uuid.UUID
	Format     domain.Format
	Percent    *float32
	LastReadAt time.Time
}
