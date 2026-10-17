// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// The share picker lists everybody else, by name, and never the person asking.
func TestPeopleListsEverybodyElseByName(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	me := testOwner(t, p)
	friend := secondUser(t, p)
	admin := anotherAdmin(t, p)

	people, err := p.People(t.Context(), me)
	if err != nil {
		t.Fatalf("People: %v", err)
	}
	byID := map[uuid.UUID]Person{}
	var names []string
	for _, person := range people {
		byID[person.ID] = person
		names = append(names, strings.ToLower(person.Username))
	}
	if _, ok := byID[me]; ok {
		t.Error("the person asking was offered to themselves")
	}
	if f, ok := byID[friend]; !ok || f.IsAdmin {
		t.Errorf("friend = %+v, %v; want listed, not an administrator", f, ok)
	}
	if a, ok := byID[admin]; !ok || !a.IsAdmin {
		t.Errorf("admin = %+v, %v; want listed as an administrator", a, ok)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("people are not in name order: %v", names)
	}
}

// Only a holder may withdraw a grant, and only their own; anybody else is
// told there is no such grant, and the grant stands.
func TestUnshareBookIsTheGrantorsAlone(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	stranger := secondUser(t, p)
	fileID := oneBook(t, p, owner)
	ctx := t.Context()

	if err := p.ShareBook(ctx, fileID, owner, friend, false, true); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	for name, unshare := range map[string]func() error{
		"a stranger":                  func() error { return p.UnshareBook(ctx, fileID, stranger, friend) },
		"the borrower themselves":     func() error { return p.UnshareBook(ctx, fileID, friend, friend) },
		"the owner, a grant not made": func() error { return p.UnshareBook(ctx, fileID, owner, stranger) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := unshare(); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("UnshareBook: %v, want store.ErrNotFound", err)
			}
		})
	}
	if !canRead(t, p, fileID, friend) {
		t.Fatal("a refused unshare still withdrew the grant")
	}

	if err := p.UnshareBook(ctx, fileID, owner, friend); err != nil {
		t.Fatalf("UnshareBook: %v", err)
	}
	if canRead(t, p, fileID, friend) {
		t.Error("the borrower can still read after the grant was withdrawn")
	}
	if n := rowCount(t, p, `SELECT count(*) FROM annotation_access WHERE book_file_id = $1 AND viewer_id = $2`, fileID, friend); n != 0 {
		t.Error("the owner's notes stayed shared after the book was withdrawn")
	}
}

// Reading with nobody signed in is not recorded against anybody.
func TestRecordAccessWithNoViewerRecordsNothing(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	fileID := oneBook(t, p, owner)

	if err := p.RecordAccess(t.Context(), fileID, uuid.Nil, AccessChapter, 1); err != nil {
		t.Fatalf("RecordAccess: %v", err)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM access_log WHERE book_file_id = $1`, fileID); n != 0 {
		t.Errorf("%d access rows for nobody", n)
	}
}

func TestSharingActivityBoundsItsQuestion(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	fileID := oneBook(t, p, owner)
	ctx := t.Context()

	if _, err := p.SharingActivity(ctx, uuid.Nil, time.Now().Add(-time.Hour), 10); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SharingActivity(nobody): %v, want store.ErrNotFound", err)
	}

	if err := p.ShareBook(ctx, fileID, owner, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	if err := p.RecordAccess(ctx, fileID, friend, AccessChapter, 2); err != nil {
		t.Fatalf("RecordAccess: %v", err)
	}
	// A limit out of range falls back to the default page rather than
	// returning nothing or everything.
	activity, err := p.SharingActivity(ctx, owner, time.Now().Add(-time.Hour), -1)
	if err != nil {
		t.Fatalf("SharingActivity: %v", err)
	}
	if len(activity) != 1 || activity[0].UserID != friend || activity[0].Units != 1 || activity[0].Of != 3 {
		t.Errorf("activity = %+v, want one row: the friend, one chapter of three", activity)
	}
	// And the window is honoured.
	later, err := p.SharingActivity(ctx, owner, time.Now().Add(time.Hour), 10)
	if err != nil {
		t.Fatalf("SharingActivity: %v", err)
	}
	if len(later) != 0 {
		t.Errorf("activity from before the window: %+v", later)
	}
}

func TestShareAnnotationsRules(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	stranger := secondUser(t, p)
	fileID := oneBook(t, p, owner)
	ctx := t.Context()

	t.Run("not with yourself", func(t *testing.T) {
		if err := p.ShareAnnotations(ctx, fileID, owner, owner); !errors.Is(err, store.ErrInvalidQuery) {
			t.Errorf("ShareAnnotations: %v, want store.ErrInvalidQuery", err)
		}
	})

	t.Run("not on a book you cannot read", func(t *testing.T) {
		if err := p.ShareAnnotations(ctx, fileID, stranger, friend); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ShareAnnotations: %v, want store.ErrNotFound", err)
		}
		if n := rowCount(t, p, `SELECT count(*) FROM annotation_access WHERE owner_id = $1`, stranger); n != 0 {
			t.Error("a share was recorded for somebody who cannot read the book")
		}
	})

	t.Run("sharing twice is not an error, and withdrawing ends it", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			if err := p.ShareAnnotations(ctx, fileID, owner, friend); err != nil {
				t.Fatalf("ShareAnnotations %d: %v", i, err)
			}
		}
		shares, err := p.AnnotationShares(ctx, fileID, owner)
		if err != nil {
			t.Fatalf("AnnotationShares: %v", err)
		}
		if len(shares) != 1 || shares[0].ID != friend || shares[0].Username == "" {
			t.Fatalf("shares = %v, want only the friend", shares)
		}

		if err := p.UnshareAnnotations(ctx, fileID, owner, friend); err != nil {
			t.Fatalf("UnshareAnnotations: %v", err)
		}
		shares, err = p.AnnotationShares(ctx, fileID, owner)
		if err != nil {
			t.Fatalf("AnnotationShares: %v", err)
		}
		if len(shares) != 0 {
			t.Errorf("shares after withdrawing = %v", shares)
		}
		// Withdrawing what is not there is not an error either.
		if err := p.UnshareAnnotations(ctx, fileID, owner, friend); err != nil {
			t.Errorf("UnshareAnnotations again: %v", err)
		}
	})
}
