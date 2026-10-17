// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/epub"
)

// writeExtractEPUB writes an in-memory EPUB to a file in a fresh directory
// and returns its path. Entries are written in sorted order so the archive
// is the same bytes every run.
func writeExtractEPUB(t *testing.T, entries map[string]string) string {
	t.Helper()

	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatalf("creating %q: %v", name, err)
		}
		if _, err := w.Write([]byte(entries[name])); err != nil {
			t.Fatalf("writing %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return writeExtractFile(t, "book.epub", buf.Bytes())
}

func writeExtractFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o444); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

const extractContainer = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`

// extractOPF has five spine items: a chapter, its continuation, a
// non-linear notes page, a document with no text, and one whose file is
// missing from the archive.
const extractOPF = `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>Extraction Fixture</dc:title>
    <dc:identifier id="id">urn:uuid:00000000-0000-0000-0000-000000000001</dc:identifier>
  </metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="ch1" href="text/ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch1b" href="text/ch1b.xhtml" media-type="application/xhtml+xml"/>
    <item id="notes" href="text/notes.xhtml" media-type="application/xhtml+xml"/>
    <item id="blank" href="text/blank.xhtml" media-type="application/xhtml+xml"/>
    <item id="lost" href="text/lost.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch2" href="text/ch2.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="ch1"/>
    <itemref idref="ch1b"/>
    <itemref idref="notes" linear="no"/>
    <itemref idref="blank"/>
    <itemref idref="lost"/>
    <itemref idref="ch2"/>
  </spine>
</package>`

const extractNav = `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops">
  <body><nav epub:type="toc"><ol>
    <li><a href="text/ch1.xhtml">One: The Beginning</a>
      <ol><li><a href="text/ch1.xhtml#s2">A Sub-heading</a></li></ol></li>
    <li><a href="text/ch2.xhtml">Two: The Middle</a></li>
  </ol></nav></body>
</html>`

func extractFixture() map[string]string {
	return map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": extractContainer,
		"OEBPS/content.opf":      extractOPF,
		"OEBPS/nav.xhtml":        extractNav,
		"OEBPS/text/ch1.xhtml":   `<html><body><p>First paragraph.</p><p>Second paragraph.</p></body></html>`,
		"OEBPS/text/ch1b.xhtml":  `<html><body><p>Chapter one continues.</p></body></html>`,
		"OEBPS/text/notes.xhtml": `<html><body><p>An advert for other titles.</p></body></html>`,
		"OEBPS/text/blank.xhtml": `<html><body><img src="x.png"/></body></html>`,
		"OEBPS/text/ch2.xhtml":   `<html><body><p>Middle matter.</p></body></html>`,
	}
}

// An EPUB becomes one unit per readable spine item, indexed by its spine
// position so locators line up with the reader, each carrying the chapter it
// belongs to.
func TestUnitsFromAnEPUB(t *testing.T) {
	path := writeExtractEPUB(t, extractFixture())

	units, err := Units(t.Context(), path, domain.FormatEPUB)
	if err != nil {
		t.Fatalf("Units: %v", err)
	}

	type summary struct {
		Index int
		Title string
		Texts []string
	}
	var got []summary
	for _, u := range units {
		s := summary{Index: u.Index, Title: u.Title}
		for _, b := range u.Blocks {
			if b.Path == "" {
				t.Errorf("unit %d has a block with no element path; the reader cannot navigate to it", u.Index)
			}
			s.Texts = append(s.Texts, b.Text)
		}
		got = append(got, s)
	}

	want := []summary{
		{0, "One: The Beginning", []string{"First paragraph.", "Second paragraph."}},
		// The continuation has no heading of its own and inherits the chapter.
		{1, "One: The Beginning", []string{"Chapter one continues."}},
		// Spine 2 is non-linear, 3 has no text, 4 is missing from the
		// archive: none is a unit, and none renumbers what follows.
		{5, "Two: The Middle", []string{"Middle matter."}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("units =\n%+v\nwant\n%+v", got, want)
	}
}

// Extraction is deterministic: chunk ids downstream depend on it.
func TestUnitsAreDeterministic(t *testing.T) {
	path := writeExtractEPUB(t, extractFixture())
	first, err := Units(t.Context(), path, domain.FormatEPUB)
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	second, err := Units(t.Context(), path, domain.FormatEPUB)
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Error("two reads of the same file produced different units")
	}
}

// The library is read-only: extraction must not change the file.
func TestUnitsLeaveTheFileUntouched(t *testing.T) {
	path := writeExtractEPUB(t, extractFixture())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Units(t.Context(), path, domain.FormatEPUB); err != nil {
		t.Fatalf("Units: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	infoAfter, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Error("extraction modified the book file")
	}
}

// A book with nothing to index says so with ErrNoText, which is not a
// failure to retry.
func TestUnitsReportsNoText(t *testing.T) {
	t.Run("an EPUB whose only text is non-linear", func(t *testing.T) {
		entries := extractFixture()
		entries["OEBPS/content.opf"] = strings.Replace(extractOPF, `<spine>
    <itemref idref="ch1"/>
    <itemref idref="ch1b"/>`, `<spine>`, 1)
		entries["OEBPS/content.opf"] = strings.Replace(entries["OEBPS/content.opf"],
			`<itemref idref="ch2"/>`, ``, 1)
		path := writeExtractEPUB(t, entries)

		if _, err := Units(t.Context(), path, domain.FormatEPUB); !errors.Is(err, ErrNoText) {
			t.Errorf("error = %v, want ErrNoText", err)
		}
	})

	t.Run("a comic", func(t *testing.T) {
		path := writeExtractFile(t, "comic.cbz", []byte("PK pictures"))
		if _, err := Units(t.Context(), path, domain.FormatCBZ); !errors.Is(err, ErrNoText) {
			t.Errorf("error = %v, want ErrNoText", err)
		}
	})

	t.Run("a scanned PDF", func(t *testing.T) {
		path := writeExtractFile(t, "scan.pdf", extractPDF(t, []string{".", ".", "."}))
		if _, err := Units(t.Context(), path, domain.FormatPDF); !errors.Is(err, ErrNoText) {
			t.Errorf("error = %v, want ErrNoText", err)
		}
	})
}

func TestUnitsReportsUnreadableFiles(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		_, err := Units(t.Context(), filepath.Join(t.TempDir(), "absent.epub"), domain.FormatEPUB)
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("error = %v, want ErrNotExist", err)
		}
	})

	t.Run("not an EPUB", func(t *testing.T) {
		path := writeExtractFile(t, "fake.epub", []byte("this is plain text, not a zip"))
		_, err := Units(t.Context(), path, domain.FormatEPUB)
		if !errors.Is(err, epub.ErrNotEPUB) {
			t.Errorf("error = %v, want ErrNotEPUB", err)
		}
	})

	t.Run("not a PDF", func(t *testing.T) {
		path := writeExtractFile(t, "fake.pdf", []byte("this is plain text, not a pdf"))
		_, err := Units(t.Context(), path, domain.FormatPDF)
		if err == nil || errors.Is(err, ErrNoText) {
			t.Errorf("error = %v, want a parse failure rather than an empty book", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		path := writeExtractEPUB(t, extractFixture())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := Units(ctx, path, domain.FormatEPUB); !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	})
}

// Ebooks are untrusted. An external entity in a package document or a
// chapter must never pull a file from the host into the text that gets
// indexed and quoted back.
func TestUnitsNeverResolveExternalEntities(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.txt")
	const marker = "HOST-SECRET-7f3a91"
	if err := os.WriteFile(secret, []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	doctype := fmt.Sprintf(`<!DOCTYPE x [<!ENTITY xxe SYSTEM "file://%s">]>`, secret)

	t.Run("in the package document", func(t *testing.T) {
		entries := extractFixture()
		entries["OEBPS/content.opf"] = strings.Replace(extractOPF,
			`<?xml version="1.0" encoding="UTF-8"?>`, `<?xml version="1.0" encoding="UTF-8"?>`+doctype, 1)
		entries["OEBPS/content.opf"] = strings.Replace(entries["OEBPS/content.opf"],
			"Extraction Fixture", "&xxe;", 1)
		path := writeExtractEPUB(t, entries)

		units, err := Units(t.Context(), path, domain.FormatEPUB)
		assertNoLeak(t, units, marker)
		if err == nil {
			return
		}
		// Failing loudly against this one file is also acceptable.
		if !errors.Is(err, epub.ErrNotEPUB) {
			t.Errorf("error = %v, want ErrNotEPUB if it fails at all", err)
		}
	})

	t.Run("in a chapter", func(t *testing.T) {
		entries := extractFixture()
		entries["OEBPS/text/ch1.xhtml"] = `<?xml version="1.0"?>` + doctype +
			`<html><body><p>Before &xxe; after.</p></body></html>`
		path := writeExtractEPUB(t, entries)

		units, err := Units(t.Context(), path, domain.FormatEPUB)
		if err != nil {
			t.Fatalf("Units: %v", err)
		}
		assertNoLeak(t, units, marker)
	})
}

func assertNoLeak(t *testing.T, units []Unit, marker string) {
	t.Helper()
	for _, u := range units {
		if strings.Contains(u.Title, marker) {
			t.Fatalf("unit %d's title carries a host file's contents", u.Index)
		}
		for _, b := range u.Blocks {
			if strings.Contains(b.Text, marker) {
				t.Fatalf("unit %d carries a host file's contents: %q", u.Index, b.Text)
			}
		}
	}
}

// A PDF is one unit per page with text, numbered from ONE, which is what a
// PDF locator means and what the reader's page control shows.
func TestUnitsFromAPDF(t *testing.T) {
	long := strings.Repeat("Real words on a real page of a real document. ", 6)
	path := writeExtractFile(t, "doc.pdf", extractPDF(t, []string{long + "one", "", long + "three"}))

	units, err := Units(t.Context(), path, domain.FormatPDF)
	if err != nil {
		t.Fatalf("Units: %v", err)
	}
	if len(units) != 2 {
		t.Fatalf("units = %d, want the two pages with text", len(units))
	}
	if units[0].Index != 1 || units[1].Index != 3 {
		t.Errorf("indices = %d, %d; want pages 1 and 3", units[0].Index, units[1].Index)
	}
	for _, u := range units {
		if len(u.Blocks) != 1 || u.Blocks[0].Path != "" || !strings.Contains(u.Blocks[0].Text, "Real words") {
			t.Errorf("page %d blocks = %+v, want one pathless block of the page text", u.Index, u.Blocks)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(units[1].Blocks[0].Text), "three") {
		t.Errorf("page 3 text = %q", units[1].Blocks[0].Text)
	}
}

// titlesByUnit flattens a nested, partial table of contents. The first entry
// for a unit wins, and an entry pointing outside the spine is skipped rather
// than guessed at.
func TestTitlesByUnit(t *testing.T) {
	toc := []ingest.TOCEntry{
		{Title: "  Part One  ", SpineIndex: 0, Children: []ingest.TOCEntry{
			{Title: "Chapter 1", SpineIndex: 0},
			{Title: "Chapter 2", SpineIndex: 3, Children: []ingest.TOCEntry{
				{Title: "Section 2.1", SpineIndex: 4},
			}},
		}},
		{Title: "Outside the spine", SpineIndex: -1},
		{Title: "   ", SpineIndex: 7},
		{Title: "Appendix", SpineIndex: 9},
	}

	got := titlesByUnit(toc)
	want := map[int]string{0: "Part One", 3: "Chapter 2", 4: "Section 2.1", 9: "Appendix"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("titlesByUnit = %v, want %v", got, want)
	}
}

// A unit with no heading of its own belongs to the nearest one before it;
// a unit before any heading has none.
func TestTitleAtFillsForward(t *testing.T) {
	titles := map[int]string{2: "Chapter 1", 5: "Chapter 2"}
	tests := []struct {
		unit int
		want string
	}{
		{0, ""}, {1, ""}, {2, "Chapter 1"}, {3, "Chapter 1"}, {4, "Chapter 1"},
		{5, "Chapter 2"}, {40, "Chapter 2"},
	}
	for _, tt := range tests {
		if got := titleAt(titles, tt.unit); got != tt.want {
			t.Errorf("titleAt(%d) = %q, want %q", tt.unit, got, tt.want)
		}
	}
	if got := titleAt(nil, 3); got != "" {
		t.Errorf("titleAt with no TOC = %q", got)
	}
}

// extractPDF builds a minimal valid PDF with one page per string, each drawn
// as a single line of Helvetica. An empty string makes a page with no text.
func extractPDF(t *testing.T, pages []string) []byte {
	t.Helper()

	firstPage := 3
	fontObj := firstPage + len(pages)*2
	kids := make([]string, 0, len(pages))
	for i := range pages {
		kids = append(kids, fmt.Sprintf("%d 0 R", firstPage+i*2))
	}
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)),
	}
	for i, text := range pages {
		content := ""
		if text != "" {
			escaped := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`).Replace(text)
			content = fmt.Sprintf("BT\n/F1 12 Tf\n1 0 0 1 72 720 Tm\n(%s) Tj\nET\n", escaped)
		}
		objects = append(objects,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] "+
				"/Resources << /Font << /F1 %d 0 R >> >> /Contents %d 0 R >>", fontObj, firstPage+i*2+1),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		)
	}
	objects = append(objects, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>")

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, body := range objects {
		offsets[i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, body)
	}
	xref := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return buf.Bytes()
}
