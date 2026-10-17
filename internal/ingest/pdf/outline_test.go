// SPDX-License-Identifier: AGPL-3.0-or-later

package pdf

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// outlinePDF builds a document with pages and an outline.
//
// Each entry is described by its title and a destination written as raw PDF,
// so a test can say exactly what shape of destination it is exercising -
// which is the whole difficulty: the spec allows several, and real files use
// all of them.
type outlineItem struct {
	title string
	dest  string // raw PDF for /Dest or /A, e.g. "/Dest [4 0 R /XYZ null null null]"
	kids  []outlineItem
}

func newOutlinePDF(t *testing.T, pageCount int, items []outlineItem, extraObjects ...string) []byte {
	t.Helper()

	// Object layout: 1 catalog, 2 pages tree, 3..(2+pageCount) pages, then the
	// outline root, then one object per outline entry, then any extras.
	firstPage := 3
	kids := make([]string, 0, pageCount)
	for i := 0; i < pageCount; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", firstPage+i))
	}

	objects := []string{
		"", // catalog, filled in below once object numbers are known
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pageCount),
	}
	for i := 0; i < pageCount; i++ {
		// Each page carries a distinct /Annots marker, so no two page
		// dictionaries are byte-identical and each is individually identifiable.
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Rotate 0 /UserUnit %d >>", i+1))
	}

	outlineRoot := firstPage + pageCount
	next := outlineRoot + 1

	// Entries are laid out breadth-first so parents can name their children.
	type placed struct {
		item   outlineItem
		obj    int
		parent int
	}
	var queue []placed
	var flat []placed

	var enqueue func(items []outlineItem, parent int)
	enqueue = func(items []outlineItem, parent int) {
		var assigned []placed
		for _, item := range items {
			p := placed{item: item, obj: next, parent: parent}
			next++
			assigned = append(assigned, p)
		}
		queue = append(queue, assigned...)
		flat = append(flat, assigned...)
		for _, p := range assigned {
			if len(p.item.kids) > 0 {
				enqueue(p.item.kids, p.obj)
			}
		}
	}
	enqueue(items, outlineRoot)

	// The outline root names its first and last top-level children.
	firstTop, lastTop := 0, 0
	for _, p := range flat {
		if p.parent == outlineRoot {
			if firstTop == 0 {
				firstTop = p.obj
			}
			lastTop = p.obj
		}
	}

	outlineObjects := make([]string, 0, len(flat)+1)
	if firstTop != 0 {
		outlineObjects = append(outlineObjects,
			fmt.Sprintf("<< /Type /Outlines /First %d 0 R /Last %d 0 R /Count %d >>",
				firstTop, lastTop, len(flat)))
	} else {
		outlineObjects = append(outlineObjects, "<< /Type /Outlines /Count 0 >>")
	}

	// Siblings need /Next, children need /First and /Last.
	for i, p := range flat {
		var b strings.Builder
		fmt.Fprintf(&b, "<< /Title (%s) /Parent %d 0 R", p.item.title, p.parent)

		var prev, nxt int
		for j, q := range flat {
			if q.parent != p.parent {
				continue
			}
			if j < i {
				prev = q.obj
			}
			if j > i && nxt == 0 {
				nxt = q.obj
			}
		}
		if prev != 0 {
			fmt.Fprintf(&b, " /Prev %d 0 R", prev)
		}
		if nxt != 0 {
			fmt.Fprintf(&b, " /Next %d 0 R", nxt)
		}

		var childFirst, childLast int
		for _, q := range flat {
			if q.parent == p.obj {
				if childFirst == 0 {
					childFirst = q.obj
				}
				childLast = q.obj
			}
		}
		if childFirst != 0 {
			fmt.Fprintf(&b, " /First %d 0 R /Last %d 0 R /Count %d", childFirst, childLast, len(p.item.kids))
		}
		if p.item.dest != "" {
			fmt.Fprintf(&b, " %s", p.item.dest)
		}
		b.WriteString(" >>")
		outlineObjects = append(outlineObjects, b.String())
	}

	objects = append(objects, outlineObjects...)
	objects = append(objects, extraObjects...)

	// The catalog can only be written once the extras have their numbers.
	catalog := fmt.Sprintf("<< /Type /Catalog /Pages 2 0 R /Outlines %d 0 R", outlineRoot)
	for _, extra := range extraObjects {
		// An extra that declares itself a names dictionary is linked in.
		if strings.Contains(extra, "/Dests") && strings.HasPrefix(extra, "<< /Dests") {
			catalog += fmt.Sprintf(" /Names %d 0 R", indexOf(objects, extra)+1)
		}
	}
	catalog += " >>"
	objects[0] = catalog

	return buildPDF(t, objects, 1)
}

func indexOf(objects []string, want string) int {
	for i, o := range objects {
		if o == want {
			return i
		}
	}
	return -1
}

// pageObj returns the object number of a 1-based page in newOutlinePDF's layout.
func pageObj(page int) int { return 2 + page }

func outlineOf(t *testing.T, doc []byte) []ingest.TOCEntry {
	t.Helper()
	entries, err := Outline(bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}
	return entries
}

func find(entries []ingest.TOCEntry, title string) (ingest.TOCEntry, bool) {
	for _, e := range entries {
		if e.Title == title {
			return e, true
		}
		if found, ok := find(e.Children, title); ok {
			return found, true
		}
	}
	return ingest.TOCEntry{}, false
}

// The ordinary case: /Dest naming a page object directly.
func TestOutlineResolvesExplicitDestinations(t *testing.T) {
	doc := newOutlinePDF(t, 6, []outlineItem{
		{title: "Chapter One", dest: fmt.Sprintf("/Dest [%d 0 R /XYZ null null null]", pageObj(2))},
		{title: "Chapter Two", dest: fmt.Sprintf("/Dest [%d 0 R /Fit]", pageObj(5))},
	})

	entries := outlineOf(t, doc)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].SpineIndex != 2 {
		t.Errorf("Chapter One resolved to page %d, want 2", entries[0].SpineIndex)
	}
	if entries[1].SpineIndex != 5 {
		t.Errorf("Chapter Two resolved to page %d, want 5", entries[1].SpineIndex)
	}
}

// The other half of the spec: a /GoTo action rather than a /Dest.
func TestOutlineResolvesGoToActions(t *testing.T) {
	doc := newOutlinePDF(t, 4, []outlineItem{
		{title: "Via Action", dest: fmt.Sprintf("/A << /S /GoTo /D [%d 0 R /Fit] >>", pageObj(3))},
	})

	entry, ok := find(outlineOf(t, doc), "Via Action")
	if !ok {
		t.Fatal("the entry is missing")
	}
	if entry.SpineIndex != 3 {
		t.Errorf("resolved to page %d, want 3", entry.SpineIndex)
	}
}

// An action that does not name a page in this document must resolve to nothing
// rather than to some page.
func TestOutlineIgnoresNonGoToActions(t *testing.T) {
	doc := newOutlinePDF(t, 3, []outlineItem{
		{title: "A Web Link", dest: "/A << /S /URI /URI (https://example.com) >>"},
		{title: "A Launch", dest: "/A << /S /Launch /F (payload.exe) >>"},
	})

	for _, e := range outlineOf(t, doc) {
		if e.SpineIndex != -1 {
			t.Errorf("%q resolved to page %d, want -1", e.Title, e.SpineIndex)
		}
	}
}

// Named destinations, in the /Names name tree.
func TestOutlineResolvesNamedDestinations(t *testing.T) {
	// A names dictionary whose /Dests is a name tree leaf.
	names := fmt.Sprintf(
		"<< /Dests << /Names [(chap.intro) [%d 0 R /Fit] (chap.middle) [%d 0 R /Fit]] "+
			"/Limits [(chap.intro) (chap.middle)] >> >>",
		pageObj(2), pageObj(4))

	doc := newOutlinePDF(t, 5, []outlineItem{
		{title: "Intro", dest: "/Dest /chap.intro"},
		{title: "Middle", dest: "/Dest (chap.middle)"},
		{title: "Missing", dest: "/Dest /chap.nowhere"},
	}, names)

	entries := outlineOf(t, doc)
	intro, _ := find(entries, "Intro")
	middle, _ := find(entries, "Middle")
	missing, _ := find(entries, "Missing")

	if intro.SpineIndex != 2 {
		t.Errorf("a /Name destination resolved to %d, want 2", intro.SpineIndex)
	}
	// A destination named by STRING rather than by name is equally legal, and
	// files in the wild use both.
	if middle.SpineIndex != 4 {
		t.Errorf("a (string) destination resolved to %d, want 4", middle.SpineIndex)
	}
	if missing.SpineIndex != -1 {
		t.Errorf("an unknown name resolved to %d, want -1", missing.SpineIndex)
	}
}

// /Limits is sometimes simply wrong. The descent must not be the only route, or
// entries silently vanish from the contents of a real book.
func TestOutlineSurvivesLyingLimits(t *testing.T) {
	names := fmt.Sprintf(
		"<< /Dests << /Names [(zzz.target) [%d 0 R /Fit]] /Limits [(aaa) (bbb)] >> >>",
		pageObj(3))

	doc := newOutlinePDF(t, 4, []outlineItem{
		{title: "Hidden By Limits", dest: "/Dest /zzz.target"},
	}, names)

	entry, ok := find(outlineOf(t, doc), "Hidden By Limits")
	if !ok {
		t.Fatal("the entry is missing")
	}
	if entry.SpineIndex != 3 {
		t.Errorf("resolved to page %d, want 3: a wrong /Limits lost the entry", entry.SpineIndex)
	}
}

// The older /Dests dictionary, which some producers still emit.
func TestOutlineResolvesLegacyDestsDictionary(t *testing.T) {
	names := fmt.Sprintf("<< /Dests << /Names [(old.style) [%d 0 R /Fit]] >> >>", pageObj(2))

	doc := newOutlinePDF(t, 3, []outlineItem{
		{title: "Old Style", dest: "/Dest /old.style"},
	}, names)

	entry, _ := find(outlineOf(t, doc), "Old Style")
	if entry.SpineIndex != 2 {
		t.Errorf("resolved to page %d, want 2", entry.SpineIndex)
	}
}

func TestOutlineKeepsNesting(t *testing.T) {
	doc := newOutlinePDF(t, 8, []outlineItem{
		{
			title: "Part One",
			dest:  fmt.Sprintf("/Dest [%d 0 R /Fit]", pageObj(1)),
			kids: []outlineItem{
				{title: "Section A", dest: fmt.Sprintf("/Dest [%d 0 R /Fit]", pageObj(2))},
				{title: "Section B", dest: fmt.Sprintf("/Dest [%d 0 R /Fit]", pageObj(4))},
			},
		},
		{title: "Part Two", dest: fmt.Sprintf("/Dest [%d 0 R /Fit]", pageObj(6))},
	})

	entries := outlineOf(t, doc)
	if len(entries) != 2 {
		t.Fatalf("got %d top-level entries, want 2", len(entries))
	}
	if len(entries[0].Children) != 2 {
		t.Fatalf("Part One has %d children, want 2", len(entries[0].Children))
	}
	if entries[0].Children[1].Title != "Section B" || entries[0].Children[1].SpineIndex != 4 {
		t.Errorf("Section B = %q at page %d, want it at 4",
			entries[0].Children[1].Title, entries[0].Children[1].SpineIndex)
	}
	if entries[1].SpineIndex != 6 {
		t.Errorf("Part Two resolved to page %d, want 6", entries[1].SpineIndex)
	}
}

// A destination outside the document is not a page in it.
func TestOutlineRejectsOutOfRangePages(t *testing.T) {
	doc := newOutlinePDF(t, 3, []outlineItem{
		// A bare integer destination, 0-based, as remote go-to destinations use.
		{title: "In Range", dest: "/Dest [1 /Fit]"},
		{title: "Beyond The End", dest: "/Dest [99 /Fit]"},
	})

	entries := outlineOf(t, doc)
	inRange, _ := find(entries, "In Range")
	beyond, _ := find(entries, "Beyond The End")

	if inRange.SpineIndex != 2 {
		t.Errorf("a 0-based index of 1 resolved to page %d, want 2", inRange.SpineIndex)
	}
	if beyond.SpineIndex != -1 {
		t.Errorf("a page beyond the document resolved to %d, want -1", beyond.SpineIndex)
	}
}

// An entry with no destination at all is still part of the book's structure and
// is kept, unfollowable.
func TestOutlineKeepsEntriesWithNoDestination(t *testing.T) {
	doc := newOutlinePDF(t, 3, []outlineItem{
		{title: "Just A Heading"},
	})

	entry, ok := find(outlineOf(t, doc), "Just A Heading")
	if !ok {
		t.Fatal("an entry with no destination was dropped from the contents")
	}
	if entry.SpineIndex != -1 {
		t.Errorf("resolved to page %d, want -1", entry.SpineIndex)
	}
}

// A document with no outline is the ordinary case for a scan, not an error.
func TestOutlineOfADocumentWithoutOne(t *testing.T) {
	doc := newScannedPDF(t, 3)

	entries, err := Outline(bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries from a document with no outline", len(entries))
	}
}

// A file we did not write may point an entry back at its own ancestor. The walk
// must end.
func TestOutlineSurvivesACycle(t *testing.T) {
	// Hand-built, because the fixture builder only produces well-formed trees:
	// object 5 is an outline entry whose /Next points at itself.
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R /Outlines 4 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		"<< /Type /Outlines /First 5 0 R /Last 5 0 R /Count 1 >>",
		"<< /Title (Loops Forever) /Parent 4 0 R /Next 5 0 R /Dest [3 0 R /Fit] >>",
	}
	doc := buildPDF(t, objects, 1)

	done := make(chan []ingest.TOCEntry, 1)
	go func() {
		entries, _ := Outline(bytes.NewReader(doc), int64(len(doc)))
		done <- entries
	}()

	select {
	case entries := <-done:
		if len(entries) != 1 {
			t.Errorf("got %d entries from a self-referencing chain, want 1", len(entries))
		}
	case <-t.Context().Done():
		t.Fatal("the outline walk did not terminate")
	}
}

// Two pages that are byte-identical cannot be told apart, and guessing would
// put a contents entry on the wrong page.
func TestOutlineRefusesAmbiguousPages(t *testing.T) {
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R /Outlines 5 0 R >>",
		"<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		"<< /Type /Outlines /First 6 0 R /Last 6 0 R /Count 1 >>",
		"<< /Title (Which Page) /Parent 5 0 R /Dest [4 0 R /Fit] >>",
	}
	doc := buildPDF(t, objects, 1)

	entries, err := Outline(bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		t.Fatalf("Outline: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].SpineIndex != -1 {
		t.Errorf("an indistinguishable page resolved to %d; a wrong page is worse than none",
			entries[0].SpineIndex)
	}
}

// TestOutlineAgainstRealLibrary measures outline resolution across a directory
// of real PDFs, and is skipped unless one is named.
//
// Synthetic fixtures prove the code handles each destination shape the spec
// allows; only real files show which shapes actually occur, and in what
// proportion. This is how the named-destination path was found to matter at
// all - and how a five-second open on a 1,156-page book was found, which no
// fixture would ever have been large enough to reveal.
//
//	LEXARII_PDF_DIR=~/books go test ./internal/ingest/pdf/ -run RealLibrary -v
func TestOutlineAgainstRealLibrary(t *testing.T) {
	dir := os.Getenv("LEXARII_PDF_DIR")
	if dir == "" {
		t.Skip("LEXARII_PDF_DIR not set")
	}

	paths, err := filepath.Glob(filepath.Join(dir, "*.pdf"))
	if err != nil || len(paths) == 0 {
		t.Skipf("no PDFs in %s", dir)
	}
	sort.Strings(paths)

	var withOutline, none, slowest int
	var totalEntries, totalResolved int
	var slowestFile string
	var slowestTime time.Duration

	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			continue
		}

		start := time.Now()
		entries, err := Outline(f, info.Size())
		elapsed := time.Since(start)
		_ = f.Close()
		if err != nil {
			t.Logf("unreadable: %s: %v", filepath.Base(path), err)
			continue
		}

		if elapsed > slowestTime {
			slowestTime, slowestFile = elapsed, filepath.Base(path)
		}
		if elapsed > time.Second {
			slowest++
			t.Logf("slow: %6.2fs  %s", elapsed.Seconds(), filepath.Base(path))
		}

		total, resolved := countEntries(entries), countEntries(entries)-countUnresolved(entries)
		if total == 0 {
			none++
			continue
		}
		withOutline++
		totalEntries += total
		totalResolved += resolved

		if resolved < total {
			t.Logf("partial: %d/%d  %s", resolved, total, filepath.Base(path))
		}
	}

	t.Logf("%d files: %d with an outline, %d without", len(paths), withOutline, none)
	t.Logf("entries: %d of %d resolved", totalResolved, totalEntries)
	t.Logf("slowest: %s at %v (%d over a second)", slowestFile, slowestTime.Round(time.Millisecond), slowest)

	// A file with an outline whose entries resolve to nothing is the state this
	// work replaced, and would mean a contents list that cannot be used.
	if totalEntries > 0 && totalResolved == 0 {
		t.Error("no outline entry in the whole library resolved to a page")
	}
}
