// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"sort"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

func TestDuplicatesNeedAViewer(t *testing.T) {
	p := testPool(t)
	if _, err := p.Duplicates(t.Context(), uuid.Nil); !errors.Is(err, store.ErrInvalidQuery) {
		t.Fatalf("Duplicates(nobody): %v, want store.ErrInvalidQuery", err)
	}
}

// Overlapping pairs are one decision: three copies of a book are one group,
// not three, and an unrelated pair stays its own.
func TestGroupPairsJoinsOverlaps(t *testing.T) {
	a, b, c, d, e := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	groups := groupPairs([]duplicatePair{{a, b}, {d, e}, {b, c}, {c, a}})

	var sizes []int
	member := map[uuid.UUID]int{}
	for i, g := range groups {
		sizes = append(sizes, len(g))
		for _, id := range g {
			if _, dup := member[id]; dup {
				t.Errorf("%s is in two groups", id)
			}
			member[id] = i
		}
	}
	sort.Ints(sizes)
	if len(sizes) != 2 || sizes[0] != 2 || sizes[1] != 3 {
		t.Fatalf("group sizes = %v, want [2 3]", sizes)
	}
	if member[a] != member[b] || member[b] != member[c] || member[d] != member[e] || member[a] == member[d] {
		t.Errorf("groups = %v", groups)
	}
}

func TestMergingNothingChangesNothing(t *testing.T) {
	p := testPool(t)
	userID, first, second := twoCopies(t, p, "Structure and Interpretation", "Harold Abelson")

	n, err := p.MergeWorks(t.Context(), userID, first, nil)
	if err != nil || n != 0 {
		t.Fatalf("MergeWorks(nothing) = %d, %v; want 0, nil", n, err)
	}
	if rowCount(t, p, `SELECT count(*) FROM works WHERE id = ANY($1)`, []uuid.UUID{first, second}) != 2 {
		t.Error("merging nothing removed a work")
	}
}

// A catalogue entry offers every format the viewer may download, and nothing
// they may only read or may not see at all.
func TestFilesForWorksOffersOnlyDownloadableFiles(t *testing.T) {
	p := testPool(t)
	owner, keep, other := twoCopies(t, p, "The C Programming Language", "Brian Kernighan")
	ctx := t.Context()
	if _, err := p.MergeWorks(ctx, owner, keep, []uuid.UUID{other}); err != nil {
		t.Fatalf("MergeWorks: %v", err)
	}

	files, err := p.FilesForWorks(ctx, owner, []uuid.UUID{keep})
	if err != nil {
		t.Fatalf("FilesForWorks: %v", err)
	}
	got := files[keep]
	if len(got) != 2 || got[0].Format != "epub" || got[1].Format != "pdf" || got[0].Size != 4096 {
		t.Fatalf("owner's files = %+v, want the epub and the pdf", got)
	}
	epub, pdf := got[0].ID, got[1].ID

	reader, downloader, stranger := secondUser(t, p), secondUser(t, p), secondUser(t, p)
	if err := p.ShareBook(ctx, epub, owner, reader, false, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}
	if err := p.ShareBook(ctx, pdf, owner, downloader, true, false); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	for _, tc := range []struct {
		name   string
		viewer uuid.UUID
		want   []uuid.UUID
	}{
		{"lent to read only", reader, nil},
		{"lent to download", downloader, []uuid.UUID{pdf}},
		{"a stranger", stranger, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := p.FilesForWorks(ctx, tc.viewer, []uuid.UUID{keep})
			if err != nil {
				t.Fatalf("FilesForWorks: %v", err)
			}
			var ids []uuid.UUID
			for _, f := range files[keep] {
				ids = append(ids, f.ID)
			}
			if len(ids) != len(tc.want) || (len(ids) == 1 && ids[0] != tc.want[0]) {
				t.Errorf("files = %v, want %v", ids, tc.want)
			}
		})
	}

	t.Run("no works asked about", func(t *testing.T) {
		files, err := p.FilesForWorks(ctx, owner, nil)
		if err != nil || len(files) != 0 {
			t.Errorf("FilesForWorks(none) = %v, %v", files, err)
		}
	})
}
