// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// coverPDF ingests a ready PDF with no cover, held by owner.
func coverPDF(t *testing.T, p *Pool, owner uuid.UUID, seed int, title string) uuid.UUID {
	t.Helper()
	in := owned(sampleInput(fmt.Sprintf("%064x", seed), fmt.Sprintf("/library/cover-%d.pdf", seed),
		title, "Some Author"), owner)
	in.Format, in.Result.Format = domain.FormatPDF, domain.FormatPDF
	out, err := p.UpsertBookFile(t.Context(), in)
	if err != nil {
		t.Fatalf("seeding %q: %v", title, err)
	}
	return out.BookFileID
}

func TestSettingACoverReturnsTheOneItReplaced(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)
	friend := secondUser(t, p)
	stranger := secondUser(t, p)
	bookFile := coverPDF(t, p, owner, 601, "A Scanned Book")

	if _, err := p.Cover(ctx, bookFile, owner); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a book with no cover: err = %v, want ErrNotFound", err)
	}

	prev, err := p.SetCover(ctx, bookFile, owner, "first.jpg")
	if err != nil || prev != "" {
		t.Fatalf("SetCover (first) = %q, %v; want nothing replaced", prev, err)
	}
	if got, err := p.Cover(ctx, bookFile, owner); err != nil || got != "first.jpg" {
		t.Fatalf("Cover = %q, %v", got, err)
	}

	// Writing the same name again replaced nothing that needs deleting.
	if prev, err := p.SetCover(ctx, bookFile, owner, "first.jpg"); err != nil || prev != "" {
		t.Fatalf("SetCover (same) = %q, %v; want nothing to delete", prev, err)
	}

	// Somebody who may read it - lent it - may draw its cover.
	if err := p.ShareBook(ctx, bookFile, owner, friend, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	prev, err = p.SetCover(ctx, bookFile, friend, "second.jpg")
	if err != nil || prev != "first.jpg" {
		t.Fatalf("SetCover (by the friend) = %q, %v; want first.jpg handed back to delete", prev, err)
	}
	if got, err := p.Cover(ctx, bookFile, friend); err != nil || got != "second.jpg" {
		t.Fatalf("Cover (friend) = %q, %v", got, err)
	}

	// A stranger can neither replace it nor learn it exists.
	if _, err := p.SetCover(ctx, bookFile, stranger, "vandal.jpg"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a stranger setting the cover: err = %v, want ErrNotFound", err)
	}
	if _, err := p.Cover(ctx, bookFile, stranger); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a stranger reading the cover: err = %v, want ErrNotFound", err)
	}
	if got, _ := p.Cover(ctx, bookFile, owner); got != "second.jpg" {
		t.Errorf("after a refused write the cover is %q", got)
	}

	if _, err := p.SetCover(ctx, uuid.New(), owner, "x.jpg"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a cover for an absent file: err = %v, want ErrNotFound", err)
	}
}

// Only ready, present PDFs the viewer can read need a cover drawn by the
// client, and the count is the whole number rather than the batch.
func TestFilesNeedingCovers(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	owner := testOwner(t, p)
	stranger := secondUser(t, p)

	zebra := coverPDF(t, p, owner, 611, "Zebra Patterns")
	apple := coverPDF(t, p, owner, 612, "Apple Orchards")
	mango := coverPDF(t, p, owner, 613, "Mango Groves")

	covered := coverPDF(t, p, owner, 614, "Already Drawn")
	if _, err := p.SetCover(ctx, covered, owner, "drawn.jpg"); err != nil {
		t.Fatalf("SetCover: %v", err)
	}
	gone := coverPDF(t, p, owner, 615, "Unmounted")
	if _, err := p.Exec(ctx, `UPDATE book_files SET missing_since = now() WHERE id = $1`, gone); err != nil {
		t.Fatalf("marking missing: %v", err)
	}
	scan := coverPDF(t, p, owner, 616, "A Failed Extraction")
	if _, err := p.Exec(ctx, `UPDATE book_files SET status = 'failed' WHERE id = $1`, scan); err != nil {
		t.Fatalf("marking failed: %v", err)
	}
	// An EPUB carries its cover in the archive; the client is never asked.
	oneBook(t, p, owner)
	// The stranger's PDF is theirs to draw.
	theirs := coverPDF(t, p, stranger, 617, "Somebody Else's Scan")

	got, err := p.FilesNeedingCovers(ctx, owner, 0)
	if err != nil {
		t.Fatalf("FilesNeedingCovers: %v", err)
	}
	var ids []uuid.UUID
	for _, c := range got {
		ids = append(ids, c.BookFileID)
		if c.Format != string(domain.FormatPDF) {
			t.Errorf("target %s has format %q", c.BookFileID, c.Format)
		}
	}
	// In the shelf's own order, so the visible covers appear first.
	if want := []uuid.UUID{apple, mango, zebra}; !slices.Equal(ids, want) {
		t.Fatalf("targets = %v, want %v", ids, want)
	}

	if batch, err := p.FilesNeedingCovers(ctx, owner, 2); err != nil || len(batch) != 2 {
		t.Fatalf("a batch of 2 returned %d (err %v)", len(batch), err)
	}
	if n, err := p.CountFilesNeedingCovers(ctx, owner); err != nil || n != 3 {
		t.Fatalf("CountFilesNeedingCovers = %d, %v; want the whole 3, not the batch", n, err)
	}

	theirTargets, err := p.FilesNeedingCovers(ctx, stranger, 50)
	if err != nil {
		t.Fatalf("FilesNeedingCovers (stranger): %v", err)
	}
	if len(theirTargets) != 1 || theirTargets[0].BookFileID != theirs {
		t.Fatalf("stranger's targets = %+v, want only their own", theirTargets)
	}
	if n, err := p.CountFilesNeedingCovers(ctx, stranger); err != nil || n != 1 {
		t.Fatalf("CountFilesNeedingCovers (stranger) = %d, %v", n, err)
	}
}
