// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// A guessed id, or somebody else's, edits nothing and reads as not found.
func TestUpdateAnnotationOfAnotherOrUnknownMark(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	stranger := secondUser(t, p)
	ctx := t.Context()

	created, err := p.CreateAnnotation(ctx, epubMark(userID, bookFileID, 1, "p[1]", 0, 5))
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	if _, err := p.UpdateAnnotation(ctx, stranger, created.ID, "defaced", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a stranger's edit: %v, want store.ErrNotFound", err)
	}
	if _, err := p.UpdateAnnotation(ctx, userID, uuid.New(), "nothing", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("editing an unknown mark: %v, want store.ErrNotFound", err)
	}
	marks, err := p.Annotations(ctx, userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 || marks[0].Note != "" {
		t.Errorf("marks = %+v, want the original untouched", marks)
	}
}

// Without a usable limit the notes view gets its default page, not nothing.
func TestRecentAnnotationsDefaultsItsPage(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	ctx := t.Context()
	for i := 0; i < 3; i++ {
		if _, err := p.CreateAnnotation(ctx, epubMark(userID, bookFileID, i+1, "p[1]", 0, 5)); err != nil {
			t.Fatalf("CreateAnnotation: %v", err)
		}
	}
	for _, limit := range []int{0, 1000} {
		page, err := p.RecentAnnotations(ctx, userID, limit, "")
		if err != nil {
			t.Fatalf("RecentAnnotations(%d): %v", limit, err)
		}
		if items := page.Items; len(items) != 3 || page.NextCursor != "" {
			t.Errorf("RecentAnnotations(%d) = %d items, want all 3", limit, len(items))
		}
	}
}

func TestSearchNotesBounds(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatPDF)
	ctx := t.Context()

	t.Run("needs a reader", func(t *testing.T) {
		if _, err := p.SearchNotes(ctx, uuid.Nil, "anything", 3); !errors.Is(err, store.ErrInvalidQuery) {
			t.Errorf("SearchNotes(nobody): %v, want store.ErrInvalidQuery", err)
		}
	})

	t.Run("a blank question finds nothing", func(t *testing.T) {
		found, err := p.SearchNotes(ctx, userID, "   ", 3)
		if err != nil || found != nil {
			t.Errorf("SearchNotes(blank) = %v, %v", found, err)
		}
	})

	// Four notes on a PDF, all matching; an unusable limit gives the default
	// three, each anchored by page.
	for i := 0; i < 4; i++ {
		m := pdfMark(userID, bookFileID, i+2, 0)
		created, err := p.CreateAnnotation(ctx, m)
		if err != nil {
			t.Fatalf("CreateAnnotation: %v", err)
		}
		if _, err := p.UpdateAnnotation(ctx, userID, created.ID,
			fmt.Sprintf("Monads compose effects, remark %d.", i), ""); err != nil {
			t.Fatalf("UpdateAnnotation: %v", err)
		}
	}
	found, err := p.SearchNotes(ctx, userID, "how do monads compose effects?", 0)
	if err != nil {
		t.Fatalf("SearchNotes: %v", err)
	}
	if len(found) != 3 {
		t.Fatalf("found %d notes, want the default 3", len(found))
	}
	for _, n := range found {
		start := n.Annotation.Range.Start
		if start.Format != domain.FormatPDF || start.Page < 2 || start.SpineIndex != 0 {
			t.Errorf("a PDF note was anchored as %+v", start)
		}
	}
}

func TestImportMarksNeedsAReader(t *testing.T) {
	p := testPool(t)
	_, bookFileID := annotationFixture(t, p, domain.FormatPDF)
	if _, err := p.ImportMarks(t.Context(), uuid.Nil, bookFileID, nil); !errors.Is(err, store.ErrInvalidQuery) {
		t.Fatalf("ImportMarks(nobody): %v, want store.ErrInvalidQuery", err)
	}
}
