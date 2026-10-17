// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// authorIDNamedForQuery reads an author's id, for filtering by it.
func authorIDNamedForQuery(t *testing.T, p *Pool, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := p.QueryRow(t.Context(), `SELECT id FROM authors WHERE name = $1`, name).Scan(&id); err != nil {
		t.Fatalf("finding author %q: %v", name, err)
	}
	return id
}

func TestLibraryFiltersByAuthor(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := seedLibrary(t, p, sampleBooks)
	knuth := authorIDNamedForQuery(t, p, "Donald Knuth")

	page, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, AuthorID: &knuth})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	var want []string
	for _, b := range sampleBooks {
		if b.author == "Donald Knuth" {
			want = append(want, b.title)
		}
	}
	got := titlesOf(page)
	sort.Strings(got)
	sort.Strings(want)
	if len(want) == 0 || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Knuth's shelf = %v, want %v", got, want)
	}

	// The filter narrows what the viewer may see; it never widens it.
	stranger := secondUser(t, p)
	page, err = p.Library(t.Context(), LibraryQuery{ViewerID: stranger, AuthorID: &knuth})
	if err != nil {
		t.Fatalf("Library: %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("a stranger filtering by author saw %v", titlesOf(page))
	}
}

func TestLibraryFiltersByIngestStatus(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	ctx := t.Context()

	ready := owned(sampleInput(fmt.Sprintf("%064x", 0xa1), "/library/ready.epub", "Ready Work", "An Author"), owner)
	broken := owned(sampleInput(fmt.Sprintf("%064x", 0xa2), "/library/broken.epub", "Broken Work", "An Author"), owner)
	broken.Status = domain.IngestFailed
	broken.IngestError = "the archive is truncated"
	for _, in := range []IngestInput{ready, broken} {
		if _, err := p.UpsertBookFile(ctx, in); err != nil {
			t.Fatalf("UpsertBookFile: %v", err)
		}
	}

	for _, tc := range []struct {
		status domain.IngestStatus
		want   string
	}{
		{domain.IngestReady, "Ready Work"},
		{domain.IngestFailed, "Broken Work"},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			page, err := p.Library(ctx, LibraryQuery{ViewerID: owner, Status: tc.status})
			if err != nil {
				t.Fatalf("Library: %v", err)
			}
			if got := titlesOf(page); len(got) != 1 || got[0] != tc.want {
				t.Errorf("status %s lists %v, want [%s]", tc.status, got, tc.want)
			}
		})
	}
}

// The series cursor carries the volume number zero-padded, so volume 2 comes
// before volume 10 and paging one at a time neither skips nor repeats.
func TestLibrarySeriesSortPagesInVolumeOrder(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	ctx := t.Context()

	for i, volume := range []float64{10, 2, 1} {
		v := volume
		in := owned(sampleInput(fmt.Sprintf("%064x", 0xb0+i), fmt.Sprintf("/library/vol-%v.epub", v),
			fmt.Sprintf("Volume %v", v), "An Author"), owner)
		in.Result.Metadata.Series = "The Long Series"
		in.Result.Metadata.SeriesIndex = &v
		if _, err := p.UpsertBookFile(ctx, in); err != nil {
			t.Fatalf("UpsertBookFile: %v", err)
		}
	}

	var order []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("paging by series did not terminate")
		}
		page, err := p.Library(ctx, LibraryQuery{ViewerID: owner, Sort: SortSeriesAsc, Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatalf("Library: %v", err)
		}
		order = append(order, titlesOf(page)...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	want := []string{"Volume 1", "Volume 2", "Volume 10"}
	if fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("series order = %v, want %v", order, want)
	}
}

// A cursor can only have come from this server. One that does not parse is the
// client's mistake, reported as an invalid query.
func TestLibraryRefusesCorruptCursors(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

	for _, tc := range []struct {
		name   string
		sort   LibrarySort
		cursor string
	}{
		{"id is not a uuid", SortTitleAsc, enc("Some Title\x00not-a-uuid")},
		{"added time is not a time", SortAddedDesc, enc("yesterday\x00" + uuid.NewString())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Library(t.Context(), LibraryQuery{ViewerID: owner, Sort: tc.sort, Cursor: tc.cursor})
			if !errors.Is(err, store.ErrInvalidQuery) {
				t.Fatalf("Library: %v, want store.ErrInvalidQuery", err)
			}
		})
	}
}

func TestStatsNeedsAViewer(t *testing.T) {
	p := testPool(t)
	if _, err := p.Stats(t.Context(), uuid.Nil); !errors.Is(err, store.ErrInvalidQuery) {
		t.Fatalf("Stats(nobody): %v, want store.ErrInvalidQuery", err)
	}
}

// Not being allowed to see a book reads exactly like it not existing, on every
// path that serves one.
func TestReadingPathsReportInvisibleFilesAsNotFound(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	fileID := oneBook(t, p, owner)
	stranger := secondUser(t, p)
	ctx := t.Context()

	detail, err := p.ReadableBookWithWork(ctx, fileID, owner)
	if err != nil {
		t.Fatalf("ReadableBookWithWork (owner): %v", err)
	}
	if detail.File.ID != fileID || detail.Title != "A Shared Book" || detail.WorkID == uuid.Nil ||
		detail.File.Format != domain.FormatEPUB || detail.File.Path != "/library/shared.epub" {
		t.Errorf("ReadableBookWithWork = %+v", detail)
	}

	for _, tc := range []struct {
		name   string
		id     uuid.UUID
		viewer uuid.UUID
	}{
		{"a stranger", fileID, stranger},
		{"nobody signed in", fileID, uuid.Nil},
		{"a file that does not exist", uuid.New(), owner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.ReadableBookWithWork(ctx, tc.id, tc.viewer); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("ReadableBookWithWork: %v, want store.ErrNotFound", err)
			}
			if _, err := p.ReadableBookFile(ctx, tc.id, tc.viewer); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("ReadableBookFile: %v, want store.ErrNotFound", err)
			}
			if _, err := p.Work(ctx, detail.WorkID, tc.viewer); tc.id == fileID && !errors.Is(err, store.ErrNotFound) {
				t.Errorf("Work: %v, want store.ErrNotFound", err)
			}
		})
	}

	t.Run("nobody signed in may download nothing", func(t *testing.T) {
		ok, err := p.CanDownload(ctx, fileID, uuid.Nil)
		if err != nil || ok {
			t.Errorf("CanDownload(nobody) = %v, %v; want false, nil", ok, err)
		}
	})
}

// Background work reads a file with no viewer at all: it acts as the
// installation, and must reach a book whose holder is not signed in.
func TestBookFileHasNoViewer(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	fileID := oneBook(t, p, owner)

	f, err := p.BookFile(t.Context(), fileID)
	if err != nil {
		t.Fatalf("BookFile: %v", err)
	}
	if f.ID != fileID || f.ContentHash != fmt.Sprintf("%064x", 1) || f.Status != domain.IngestReady ||
		f.UnitCount == nil || *f.UnitCount != 3 {
		t.Errorf("BookFile = %+v", f)
	}
	if _, err := p.BookFile(t.Context(), uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("BookFile(unknown) = %v, want store.ErrNotFound", err)
	}
}

// A rescan skips what it already has by asking for the status and path of a
// content hash.
func TestBookFileStatusAndPathByHash(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	ctx := t.Context()

	in := owned(sampleInput(fmt.Sprintf("%064x", 0xc1), "/library/needs-ocr.pdf", "Scanned", "An Author"), owner)
	in.Format, in.Result.Format = domain.FormatPDF, domain.FormatPDF
	in.Status = domain.IngestNeedsOCR
	if _, err := p.UpsertBookFile(ctx, in); err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	status, err := p.BookFileStatus(ctx, in.ContentHash)
	if err != nil || status != domain.IngestNeedsOCR {
		t.Errorf("BookFileStatus = %q, %v; want needs_ocr", status, err)
	}
	path, err := p.BookFilePath(ctx, in.ContentHash)
	if err != nil || path != "/library/needs-ocr.pdf" {
		t.Errorf("BookFilePath = %q, %v", path, err)
	}

	unknown := fmt.Sprintf("%064x", 0xdead)
	if _, err := p.BookFileStatus(ctx, unknown); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("BookFileStatus(unknown) = %v, want store.ErrNotFound", err)
	}
	if _, err := p.BookFilePath(ctx, unknown); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("BookFilePath(unknown) = %v, want store.ErrNotFound", err)
	}
}

// Blank names and subjects that slug to nothing are dropped, and subjects that
// differ only in case are one tag.
func TestIngestDropsBlankAuthorsAndSubjects(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	ctx := t.Context()

	in := owned(sampleInput(fmt.Sprintf("%064x", 0xd1), "/library/messy.epub", "Messy Metadata", "Ada Lovelace"), owner)
	in.Result.Metadata.Authors = []ingest.Contributor{
		{Name: "   ", Role: domain.RoleAuthor},
		{Name: "Ada Lovelace"},
	}
	in.Result.Metadata.Subjects = []string{"", "!!!", "Mathematics", "mathematics", "  "}
	out, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}

	d, err := p.Work(ctx, out.WorkID, owner)
	if err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(d.Work.Authors) != 1 || d.Work.Authors[0].Author.Name != "Ada Lovelace" || d.Work.Authors[0].Role != domain.RoleAuthor {
		t.Errorf("authors = %+v, want only Ada Lovelace as author", d.Work.Authors)
	}
	if len(d.Tags) != 1 || d.Tags[0].Slug != "mathematics" {
		t.Errorf("tags = %+v, want one mathematics tag", d.Tags)
	}
}

// A newly extracted cover displaces the old one, and the caller is told which
// file to delete.
func TestReingestReportsTheReplacedCover(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	owner := testOwner(t, p)
	ctx := t.Context()

	in := owned(sampleInput(fmt.Sprintf("%064x", 0xe1), "/library/covered.epub", "Covered", "An Author"), owner)
	in.CoverPath = "covers/old.jpg"
	if out, err := p.UpsertBookFile(ctx, in); err != nil || out.ReplacedCover != "" {
		t.Fatalf("first ingest = %+v, %v; want no replaced cover", out, err)
	}

	same, err := p.UpsertBookFile(ctx, in)
	if err != nil || same.ReplacedCover != "" {
		t.Fatalf("re-ingest with the same cover = %+v, %v; want nothing replaced", same, err)
	}

	in.CoverPath = "covers/new.jpg"
	out, err := p.UpsertBookFile(ctx, in)
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	if out.ReplacedCover != "covers/old.jpg" {
		t.Errorf("ReplacedCover = %q, want the old cover", out.ReplacedCover)
	}
}
