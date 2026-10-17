// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// An edit that asks for nothing changes nothing, and still only answers
// somebody who may see the work.
func TestAnEmptyEditIsARead(t *testing.T) {
	p := testPool(t)
	userID, workID, _ := editFixture(t, p)

	detail, err := p.UpdateWork(t.Context(), workID, userID, WorkEdit{})
	if err != nil {
		t.Fatalf("UpdateWork: %v", err)
	}
	if detail.Work.ID != workID || detail.Work.Title != "Second Edition" {
		t.Errorf("an empty edit returned %+v", detail.Work)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM enrichment_records WHERE work_id = $1 AND source = 'user'`, workID); n != 0 {
		t.Errorf("an empty edit recorded %d corrections", n)
	}

	if _, err := p.UpdateWork(t.Context(), workID, secondUser(t, p), WorkEdit{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a stranger's empty edit: %v, want store.ErrNotFound", err)
	}
}

func TestEditingTheSeriesNumberAndTheYear(t *testing.T) {
	p := testPool(t)
	userID, workID, _ := editFixture(t, p)
	ctx := t.Context()

	index := 3.5
	detail, err := p.UpdateWork(ctx, workID, userID, WorkEdit{
		Series:      str("Foundations"),
		SeriesIndex: &index,
		Year:        intp(1999),
		Authors:     strs("  ", "Edsger Dijkstra", ""),
	})
	if err != nil {
		t.Fatalf("UpdateWork: %v", err)
	}
	if detail.Work.Series != "Foundations" || detail.Work.SeriesIndex == nil || *detail.Work.SeriesIndex != 3.5 {
		t.Errorf("series = %q #%v", detail.Work.Series, detail.Work.SeriesIndex)
	}
	if len(detail.Work.Authors) != 1 || detail.Work.Authors[0].Author.Name != "Edsger Dijkstra" {
		t.Errorf("authors = %+v, want only the named one", detail.Work.Authors)
	}
	if len(detail.Work.Editions) == 0 || detail.Work.Editions[0].PublishedOn == nil ||
		detail.Work.Editions[0].PublishedOn.Year() != 1999 {
		t.Fatalf("editions = %+v, want published in 1999", detail.Work.Editions)
	}

	t.Run("a year that no book was published in is refused, with the rest of the edit", func(t *testing.T) {
		for _, year := range []int{1200, 2500} {
			_, err := p.UpdateWork(ctx, workID, userID, WorkEdit{Title: str("Changed With It"), Year: intp(year)})
			if !errors.Is(err, store.ErrInvalidQuery) {
				t.Errorf("year %d: %v, want store.ErrInvalidQuery", year, err)
			}
		}
		d, err := p.Work(ctx, workID, userID)
		if err != nil {
			t.Fatalf("Work: %v", err)
		}
		if d.Work.Title != "Second Edition" || d.Work.Editions[0].PublishedOn.Year() != 1999 {
			t.Errorf("a refused edit changed the work: %q, %v", d.Work.Title, d.Work.Editions[0].PublishedOn)
		}
	})

	t.Run("year zero clears it, and that is recorded as the reader's", func(t *testing.T) {
		d, err := p.UpdateWork(ctx, workID, userID, WorkEdit{Year: intp(0)})
		if err != nil {
			t.Fatalf("UpdateWork: %v", err)
		}
		if d.Work.Editions[0].PublishedOn != nil {
			t.Errorf("published on = %v, want cleared", d.Work.Editions[0].PublishedOn)
		}
		if n := rowCount(t, p, `
			SELECT count(*) FROM enrichment_records
			 WHERE work_id = $1 AND field = 'publishedOn' AND source = 'user' AND value = '""'::jsonb`, workID); n != 1 {
			t.Errorf("the cleared year was recorded %d times, want once", n)
		}
	})
}

func TestCountAuthors(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	authorOf(t, p, owner, 0x91, "/library/one.epub", "One", "Alan Turing")
	authorOf(t, p, owner, 0x92, "/library/two.epub", "Two", "Alonzo Church")
	authorOf(t, p, owner, 0x93, "/library/three.epub", "Three", "Alan Turing")

	n, err := p.CountAuthors(t.Context())
	if err != nil {
		t.Fatalf("CountAuthors: %v", err)
	}
	if n != 2 {
		t.Errorf("CountAuthors = %d, want 2 people", n)
	}
}

// The spelling that carries more of the person survives a merge.
func TestFullerName(t *testing.T) {
	for _, tc := range []struct{ a, b, want string }{
		{"M. Tabatabaian", "Mehrzad Tabatabaian", "Mehrzad Tabatabaian"},
		{"Mehrzad Tabatabaian", "M. Tabatabaian", "Mehrzad Tabatabaian"},
		{"Ada Lovelace", "Ada King Lovelace", "Ada King Lovelace"},
		{"Ada King Lovelace", "Ada Lovelace", "Ada King Lovelace"},
	} {
		if got := fullerName(tc.a, tc.b); got != tc.want {
			t.Errorf("fullerName(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestHoldUploadNeedsSomebody(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	hash := hashOf("f")
	if _, err := p.HoldUpload(t.Context(), HoldInput{ContentHash: hash}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("HoldUpload(nobody): %v, want store.ErrNotFound", err)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM holding_claims WHERE content_hash = $1`, hash); n != 0 {
		t.Errorf("%d claims for nobody", n)
	}
}

// A copy that could not be removed is reported, so whoever asked can say so.
func TestRemoveIfUnreferencedReportsAFailedRemoval(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	boom := errors.New("permission denied")
	err := p.RemoveIfUnreferenced(t.Context(), hashOf("f"), "/data/archive/orphan.epub", func(string) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("RemoveIfUnreferenced: %v, want the removal's error", err)
	}
}

func TestContinueReadingDefaultsItsPage(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	save(t, p, userID, bookFileID, epubPosition(2, "p[1]", 0), 0.1)

	for _, limit := range []int{0, 500} {
		items, err := p.ContinueReading(t.Context(), userID, limit)
		if err != nil {
			t.Fatalf("ContinueReading(%d): %v", limit, err)
		}
		if len(items) != 1 || items[0].BookFileID != bookFileID {
			t.Errorf("ContinueReading(%d) = %+v", limit, items)
		}
	}
	if items, err := p.ContinueReading(t.Context(), uuid.New(), 0); err != nil || len(items) != 0 {
		t.Errorf("a stranger's list = %+v, %v", items, err)
	}
}
