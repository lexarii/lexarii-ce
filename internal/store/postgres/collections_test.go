// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

func TestCollectionsAreOwnedAndCounted(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	stranger := secondUser(t, p)
	book := oneBook(t, p, owner)
	ctx := t.Context()

	made, err := p.CreateCollection(ctx, owner, "  To read  ", "the pile", nil)
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if made.Name != "To read" {
		t.Errorf("name = %q, want it trimmed", made.Name)
	}

	workID := workOf(t, p, book)
	if err := p.AddToCollection(ctx, owner, made.ID, workID); err != nil {
		t.Fatalf("AddToCollection: %v", err)
	}
	// Adding twice is success, not an error: the book is in the collection
	// either way, which is what the caller asked for.
	if err := p.AddToCollection(ctx, owner, made.ID, workID); err != nil {
		t.Fatalf("adding the same book twice: %v", err)
	}

	list, err := p.Collections(ctx, owner)
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	if len(list) != 1 || list[0].Books != 1 {
		t.Fatalf("got %d collections, first holding %d books", len(list), bookCount(list))
	}

	// A stranger sees none of it, and cannot add to it.
	if other, err := p.Collections(ctx, stranger); err != nil || len(other) != 0 {
		t.Errorf("a stranger saw %d collections (err %v)", len(other), err)
	}
	if err := p.AddToCollection(ctx, stranger, made.ID, workID); !isNotFound(err) {
		t.Errorf("a stranger added a book to somebody else's collection: %v", err)
	}
	if err := p.DeleteCollection(ctx, stranger, made.ID); !isNotFound(err) {
		t.Errorf("a stranger deleted somebody else's collection: %v", err)
	}
}

// A collection is an arrangement of a shelf, not a box that owns what is in
// it. Deleting "to read" must never be a way to lose a book.
func TestDeletingACollectionKeepsTheBooks(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	book := oneBook(t, p, owner)
	ctx := t.Context()

	made, err := p.CreateCollection(ctx, owner, "To read", "", nil)
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	workID := workOf(t, p, book)
	if err := p.AddToCollection(ctx, owner, made.ID, workID); err != nil {
		t.Fatalf("AddToCollection: %v", err)
	}
	if err := p.DeleteCollection(ctx, owner, made.ID); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}

	page, err := p.Library(ctx, LibraryQuery{ViewerID: owner, Limit: 10})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 1 {
		t.Errorf("the shelf holds %d books after a collection was deleted, want 1", len(page.Items))
	}
}

// The listing filter has to check whose collection it is, or it becomes a way
// of asking whether a book is in a stranger's "to read".
func TestTheLibraryFilterChecksWhoseCollectionItIs(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	stranger := secondUser(t, p)
	book := oneBook(t, p, owner)
	ctx := t.Context()

	made, err := p.CreateCollection(ctx, owner, "Private", "", nil)
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if err := p.AddToCollection(ctx, owner, made.ID, workOf(t, p, book)); err != nil {
		t.Fatalf("AddToCollection: %v", err)
	}

	page, err := p.Library(ctx, LibraryQuery{ViewerID: owner, CollectionID: &made.ID, Limit: 10})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("the owner sees %d books in their own collection", len(page.Items))
	}

	page, err = p.Library(ctx, LibraryQuery{ViewerID: stranger, CollectionID: &made.ID, Limit: 10})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("a stranger read %d books out of somebody else's collection", len(page.Items))
	}
}

func bookCount(list []CollectionSummary) int {
	if len(list) == 0 {
		return 0
	}
	return list[0].Books
}

// workOf finds the work a seeded book file belongs to.
func workOf(t *testing.T, p *Pool, bookFileID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := p.QueryRow(t.Context(), `
		SELECT e.work_id FROM book_files bf
		JOIN editions e ON e.id = bf.edition_id
		WHERE bf.id = $1`, bookFileID).Scan(&id); err != nil {
		t.Fatalf("finding the work: %v", err)
	}
	return id
}

func TestCollectionsNeedAnOwnerAndAName(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	ctx := t.Context()

	if _, err := p.Collections(ctx, uuid.Nil); !errors.Is(err, store.ErrInvalidQuery) {
		t.Errorf("listing with no owner: err = %v, want ErrInvalidQuery", err)
	}
	for name, tc := range map[string]struct {
		user  uuid.UUID
		named string
	}{
		"no owner":   {uuid.Nil, "Maths"},
		"blank name": {owner, "  \t "},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := p.CreateCollection(ctx, tc.user, tc.named, "", nil); !errors.Is(err, store.ErrInvalidQuery) {
				t.Fatalf("err = %v, want ErrInvalidQuery", err)
			}
		})
	}
	if n := rowCount(t, p, `SELECT count(*) FROM collections`); n != 0 {
		t.Errorf("%d collections created by refused requests", n)
	}
}

// Nesting only ever happens inside one person's own shelf.
func TestANestedCollectionsParentMustBeTheOwners(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	stranger := secondUser(t, p)
	ctx := t.Context()

	parent, err := p.CreateCollection(ctx, owner, "Mathematics", "  ", nil)
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if parent.Description != "" || parent.ParentID != nil {
		t.Errorf("parent = %+v", parent)
	}

	child, err := p.CreateCollection(ctx, owner, "Algebra", " groups and rings ", &parent.ID)
	if err != nil {
		t.Fatalf("CreateCollection (child): %v", err)
	}
	if child.ParentID == nil || *child.ParentID != parent.ID || child.Description != "groups and rings" {
		t.Errorf("child = %+v", child)
	}

	for name, parentID := range map[string]uuid.UUID{
		"somebody else's collection":  parent.ID,
		"a collection that is absent": uuid.New(),
	} {
		t.Run(name, func(t *testing.T) {
			id := parentID
			if _, err := p.CreateCollection(ctx, stranger, "Mine", "", &id); !errors.Is(err, store.ErrInvalidQuery) {
				t.Fatalf("err = %v, want ErrInvalidQuery", err)
			}
		})
	}
	if list, _ := p.Collections(ctx, stranger); len(list) != 0 {
		t.Errorf("the stranger has %d collections after being refused", len(list))
	}
}

func TestCollectionsAreListedByPositionThenName(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	ctx := t.Context()

	for _, name := range []string{"gamma", "Alpha", "beta"} {
		if _, err := p.CreateCollection(ctx, owner, name, "", nil); err != nil {
			t.Fatalf("CreateCollection: %v", err)
		}
	}
	list, err := p.Collections(ctx, owner)
	if err != nil {
		t.Fatalf("Collections: %v", err)
	}
	var names []string
	for _, c := range list {
		names = append(names, c.Collection.Name)
		if c.Books != 0 {
			t.Errorf("%s holds %d books, want 0", c.Collection.Name, c.Books)
		}
	}
	if want := []string{"Alpha", "beta", "gamma"}; !slices.Equal(names, want) {
		t.Fatalf("order = %v, want %v (case-insensitive)", names, want)
	}
}

func TestUpdatingACollection(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	stranger := secondUser(t, p)
	ctx := t.Context()

	made, err := p.CreateCollection(ctx, owner, "To read", "the pile", nil)
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}

	renamed, err := p.UpdateCollection(ctx, owner, made.ID, "  Reading next ", "   ")
	if err != nil {
		t.Fatalf("UpdateCollection: %v", err)
	}
	if renamed.ID != made.ID || renamed.Name != "Reading next" || renamed.Description != "" {
		t.Errorf("renamed = %+v", renamed)
	}
	if renamed.UpdatedAt.Before(made.UpdatedAt) {
		t.Errorf("updated_at went backwards: %v -> %v", made.UpdatedAt, renamed.UpdatedAt)
	}

	if _, err := p.UpdateCollection(ctx, owner, made.ID, " ", "x"); !errors.Is(err, store.ErrInvalidQuery) {
		t.Errorf("blank rename: err = %v, want ErrInvalidQuery", err)
	}
	if _, err := p.UpdateCollection(ctx, stranger, made.ID, "Stolen", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a stranger renaming: err = %v, want ErrNotFound", err)
	}
	if _, err := p.UpdateCollection(ctx, owner, uuid.New(), "Nothing", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("renaming an absent collection: err = %v, want ErrNotFound", err)
	}

	got, err := p.Collection(ctx, owner, made.ID)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	if got.Collection.Name != "Reading next" {
		t.Errorf("after refused renames the name is %q", got.Collection.Name)
	}
}

func TestDeletingAnAbsentCollectionIsNotFound(t *testing.T) {
	p := testPool(t)
	owner := testOwner(t, p)
	if err := p.DeleteCollection(t.Context(), owner, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// Filing a book needs both halves to be the asker's: their collection, and a
// book they can open. Otherwise filing becomes a way to probe what exists.
func TestFilingNeedsAWorkTheOwnerCanRead(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	stranger := secondUser(t, p)
	ctx := t.Context()

	mine, err := p.CreateCollection(ctx, owner, "Mine", "", nil)
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	theirs := workOf(t, p, oneBook(t, p, stranger))

	if err := p.AddToCollection(ctx, owner, mine.ID, theirs); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("filing somebody else's book: err = %v, want ErrNotFound", err)
	}
	if err := p.AddToCollection(ctx, owner, mine.ID, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("filing an absent book: err = %v, want ErrNotFound", err)
	}
	if err := p.AddToCollection(ctx, owner, uuid.New(), theirs); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("filing into an absent collection: err = %v, want ErrNotFound", err)
	}
	got, err := p.Collection(ctx, owner, mine.ID)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	if got.Books != 0 {
		t.Errorf("refused filings left %d books in the collection", got.Books)
	}
}

func TestReadingAndEmptyingACollection(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	ctx := t.Context()

	bookFile := oneBook(t, p, owner)
	work := workOf(t, p, bookFile)
	if err := p.ShareBook(ctx, bookFile, owner, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	toRead, err := p.CreateCollection(ctx, owner, "To read", "", nil)
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	maths, err := p.CreateCollection(ctx, owner, "Maths", "", nil)
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if _, err := p.CreateCollection(ctx, owner, "Empty", "", nil); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	// The friend files the lent book in a collection of their own.
	borrowed, err := p.CreateCollection(ctx, friend, "Borrowed", "", nil)
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	for _, f := range []struct{ user, collection uuid.UUID }{
		{owner, toRead.ID}, {owner, maths.ID}, {friend, borrowed.ID},
	} {
		if err := p.AddToCollection(ctx, f.user, f.collection, work); err != nil {
			t.Fatalf("AddToCollection: %v", err)
		}
	}

	got, err := p.Collection(ctx, owner, toRead.ID)
	if err != nil {
		t.Fatalf("Collection: %v", err)
	}
	if got.Collection.Name != "To read" || got.Books != 1 || got.Collection.UserID != owner {
		t.Errorf("collection = %+v", got)
	}
	if _, err := p.Collection(ctx, friend, toRead.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a friend reading the owner's collection: err = %v, want ErrNotFound", err)
	}

	// Each person sees only where THEY filed it.
	holding, err := p.CollectionsHolding(ctx, owner, work)
	if err != nil {
		t.Fatalf("CollectionsHolding: %v", err)
	}
	if !searchSameIDs(holding, []uuid.UUID{toRead.ID, maths.ID}) {
		t.Errorf("owner's collections holding the book = %v", holding)
	}
	holding, err = p.CollectionsHolding(ctx, friend, work)
	if err != nil {
		t.Fatalf("CollectionsHolding (friend): %v", err)
	}
	if !slices.Equal(holding, []uuid.UUID{borrowed.ID}) {
		t.Errorf("friend's collections holding the book = %v", holding)
	}

	// Somebody else cannot take it out of the owner's collection.
	if err := p.RemoveFromCollection(ctx, friend, toRead.ID, work); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a friend emptying the owner's collection: err = %v, want ErrNotFound", err)
	}
	if err := p.RemoveFromCollection(ctx, owner, toRead.ID, work); err != nil {
		t.Fatalf("RemoveFromCollection: %v", err)
	}
	if err := p.RemoveFromCollection(ctx, owner, toRead.ID, work); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("removing twice: err = %v, want ErrNotFound", err)
	}
	holding, err = p.CollectionsHolding(ctx, owner, work)
	if err != nil {
		t.Fatalf("CollectionsHolding: %v", err)
	}
	if !slices.Equal(holding, []uuid.UUID{maths.ID}) {
		t.Errorf("after removal the book is in %v, want only Maths", holding)
	}
	// Taking a book out of a collection leaves the book on the shelf.
	if _, err := p.ReadableBookFile(ctx, bookFile, owner); err != nil {
		t.Errorf("the book went with the collection entry: %v", err)
	}
}
