// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// CollectionSummary is a collection with the number of books in it.
type CollectionSummary struct {
	Collection domain.Collection
	Books      int
}

// Collections lists the collections a person has made.
//
// Their own only. A collection is a private arrangement of a shelf - "to
// read", "the chapter I am writing" - and it is not something a borrowed book
// drags along with it.
func (p *Pool) Collections(ctx context.Context, userID uuid.UUID) ([]CollectionSummary, error) {
	if userID == uuid.Nil {
		return nil, fmt.Errorf("%w: a collection list needs an owner", store.ErrInvalidQuery)
	}

	rows, err := p.Query(ctx, `
		SELECT c.id, c.user_id, c.name, COALESCE(c.description, ''),
		       c.parent_collection_id, c.position, c.created_at, c.updated_at,
		       count(cw.work_id)
		FROM collections c
		LEFT JOIN collection_works cw ON cw.collection_id = c.id
		WHERE c.user_id = $1
		GROUP BY c.id
		ORDER BY c.position, lower(c.name)`, userID)
	if err != nil {
		return nil, fmt.Errorf("reading collections: %w", mapError(err))
	}
	defer rows.Close()

	var out []CollectionSummary
	for rows.Next() {
		var s CollectionSummary
		if err := rows.Scan(&s.Collection.ID, &s.Collection.UserID, &s.Collection.Name,
			&s.Collection.Description, &s.Collection.ParentID, &s.Collection.Position,
			&s.Collection.CreatedAt, &s.Collection.UpdatedAt, &s.Books); err != nil {
			return nil, fmt.Errorf("scanning collection: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CreateCollection makes a new one.
func (p *Pool) CreateCollection(ctx context.Context, userID uuid.UUID, name, description string, parent *uuid.UUID) (domain.Collection, error) {
	name = strings.TrimSpace(name)
	if userID == uuid.Nil || name == "" {
		return domain.Collection{}, fmt.Errorf("%w: a collection needs an owner and a name", store.ErrInvalidQuery)
	}

	// A parent belonging to somebody else would let one person nest their
	// shelf inside another's, which is not a thing this application has.
	if parent != nil {
		var ok bool
		if err := p.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM collections WHERE id = $1 AND user_id = $2)`,
			*parent, userID).Scan(&ok); err != nil {
			return domain.Collection{}, fmt.Errorf("checking the parent collection: %w", mapError(err))
		}
		if !ok {
			return domain.Collection{}, fmt.Errorf("%w: no such parent collection", store.ErrInvalidQuery)
		}
	}

	var c domain.Collection
	err := p.QueryRow(ctx, `
		INSERT INTO collections (user_id, name, description, parent_collection_id)
		VALUES ($1, $2, NULLIF($3, ''), $4)
		RETURNING id, user_id, name, COALESCE(description, ''),
		          parent_collection_id, position, created_at, updated_at`,
		userID, name, strings.TrimSpace(description), parent,
	).Scan(&c.ID, &c.UserID, &c.Name, &c.Description, &c.ParentID, &c.Position,
		&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return domain.Collection{}, fmt.Errorf("creating the collection: %w", mapError(err))
	}
	return c, nil
}

// UpdateCollection renames one, or changes what it says about itself.
func (p *Pool) UpdateCollection(ctx context.Context, userID, id uuid.UUID, name, description string) (domain.Collection, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return domain.Collection{}, fmt.Errorf("%w: a collection needs a name", store.ErrInvalidQuery)
	}

	var c domain.Collection
	err := p.QueryRow(ctx, `
		UPDATE collections
		SET name = $3, description = NULLIF($4, ''), updated_at = now()
		WHERE id = $2 AND user_id = $1
		RETURNING id, user_id, name, COALESCE(description, ''),
		          parent_collection_id, position, created_at, updated_at`,
		userID, id, name, strings.TrimSpace(description),
	).Scan(&c.ID, &c.UserID, &c.Name, &c.Description, &c.ParentID, &c.Position,
		&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return domain.Collection{}, fmt.Errorf("renaming the collection: %w", mapError(err))
	}
	return c, nil
}

// DeleteCollection removes a collection and nothing else.
//
// The books stay. A collection is an arrangement of a shelf, not a container
// that owns what is in it, and deleting "to read" must never be a way to lose
// a book.
func (p *Pool) DeleteCollection(ctx context.Context, userID, id uuid.UUID) error {
	tag, err := p.Exec(ctx, `DELETE FROM collections WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return fmt.Errorf("deleting the collection: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// AddToCollection puts a book in a collection.
//
// Both have to be the asker's: their collection, and a book they can actually
// read. Adding a book somebody cannot open would let them discover what exists
// on the installation by trying.
func (p *Pool) AddToCollection(ctx context.Context, userID, collectionID, workID uuid.UUID) error {
	tag, err := p.Exec(ctx, `
		INSERT INTO collection_works (collection_id, work_id)
		SELECT $2, $3
		WHERE EXISTS (SELECT 1 FROM collections WHERE id = $2 AND user_id = $1)
		  AND EXISTS (
		      SELECT 1 FROM book_files bf
		      JOIN readable_book_files rbf
		        ON rbf.book_file_id = bf.id AND rbf.user_id = $1
		      JOIN editions e ON e.id = bf.edition_id
		      WHERE e.work_id = $3)
		ON CONFLICT (collection_id, work_id) DO NOTHING`,
		userID, collectionID, workID)
	if err != nil {
		return fmt.Errorf("adding to the collection: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		// Either it was already there, or one of the two is not theirs. Tell
		// them apart, because "already in it" is success and the other is not.
		var present bool
		if err := p.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM collection_works cw
			    JOIN collections c ON c.id = cw.collection_id AND c.user_id = $1
			    WHERE cw.collection_id = $2 AND cw.work_id = $3)`,
			userID, collectionID, workID).Scan(&present); err != nil {
			return fmt.Errorf("adding to the collection: %w", mapError(err))
		}
		if !present {
			return errNotFound
		}
	}
	return nil
}

// RemoveFromCollection takes a book out, leaving the book alone.
func (p *Pool) RemoveFromCollection(ctx context.Context, userID, collectionID, workID uuid.UUID) error {
	tag, err := p.Exec(ctx, `
		DELETE FROM collection_works cw
		USING collections c
		WHERE c.id = cw.collection_id AND c.user_id = $1
		  AND cw.collection_id = $2 AND cw.work_id = $3`,
		userID, collectionID, workID)
	if err != nil {
		return fmt.Errorf("removing from the collection: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// Collection reads one, for the person who owns it.
func (p *Pool) Collection(ctx context.Context, userID, id uuid.UUID) (CollectionSummary, error) {
	var s CollectionSummary
	err := p.QueryRow(ctx, `
		SELECT c.id, c.user_id, c.name, COALESCE(c.description, ''),
		       c.parent_collection_id, c.position, c.created_at, c.updated_at,
		       (SELECT count(*) FROM collection_works cw WHERE cw.collection_id = c.id)
		FROM collections c
		WHERE c.id = $2 AND c.user_id = $1`, userID, id,
	).Scan(&s.Collection.ID, &s.Collection.UserID, &s.Collection.Name,
		&s.Collection.Description, &s.Collection.ParentID, &s.Collection.Position,
		&s.Collection.CreatedAt, &s.Collection.UpdatedAt, &s.Books)
	if err != nil {
		return CollectionSummary{}, mapError(err)
	}
	return s, nil
}

// CollectionsHolding says which of a person's collections a book is in, so a
// book's page can show where it has been filed.
func (p *Pool) CollectionsHolding(ctx context.Context, userID, workID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := p.Query(ctx, `
		SELECT cw.collection_id
		FROM collection_works cw
		JOIN collections c ON c.id = cw.collection_id AND c.user_id = $1
		WHERE cw.work_id = $2`, userID, workID)
	if err != nil {
		return nil, fmt.Errorf("reading a book's collections: %w", mapError(err))
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning a collection id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
