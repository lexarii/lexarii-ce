// SPDX-License-Identifier: AGPL-3.0-or-later

package epub

import (
	"archive/zip"
	"bytes"
	"errors"
	"reflect"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// buildEPUB assembles an in-memory EPUB. Entry names are written verbatim so
// hostile ones can be tested.
func buildEPUB(t *testing.T, entries map[string]string) ([]byte, int64) {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatalf("creating %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("writing %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes(), int64(buf.Len())
}

const container = `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`

// epub3OPF exercises EPUB 3 refinements: a subtitle declared via title-type,
// a file-as refinement, properties-based nav and cover, and a collection.
const epub3OPF = `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="pub-id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title id="t1">Structure and Interpretation of Computer Programs</dc:title>
    <dc:title id="t2">A Gentle Introduction</dc:title>
    <meta refines="#t2" property="title-type">subtitle</meta>
    <dc:creator id="c1">Harold Abelson</dc:creator>
    <meta refines="#c1" property="file-as">Abelson, Harold</meta>
    <dc:creator id="c2">Gerald Jay Sussman</dc:creator>
    <meta refines="#c2" property="role">edt</meta>
    <dc:language>en-GB</dc:language>
    <dc:publisher>MIT Press</dc:publisher>
    <dc:date>1996-07-25</dc:date>
    <dc:description>A classic text.</dc:description>
    <dc:subject>Computer science</dc:subject>
    <dc:subject>Lisp</dc:subject>
    <dc:identifier id="pub-id">urn:isbn:978-0-262-51087-5</dc:identifier>
    <meta property="belongs-to-collection">MIT Electrical Engineering</meta>
  </metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="cover-img" href="images/cover.png" media-type="image/png" properties="cover-image"/>
    <item id="ch1" href="text/ch01.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch2" href="text/ch02.xhtml" media-type="application/xhtml+xml"/>
    <item id="notes" href="text/notes.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="ch1"/>
    <itemref idref="ch2"/>
    <itemref idref="notes" linear="no"/>
  </spine>
</package>`

// The nav lists chapter 2 before chapter 1 on purpose: the TOC is navigation,
// and it must not be mistaken for reading order.
const navDocXML = `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops">
  <body>
    <nav epub:type="toc">
      <ol>
        <li><a href="text/ch02.xhtml">Building Abstractions</a>
          <ol><li><a href="text/ch02.xhtml#sec-2-1">Data Abstraction</a></li></ol>
        </li>
        <li><a href="text/ch01.xhtml">The Elements of Programming</a></li>
      </ol>
    </nav>
    <nav epub:type="landmarks">
      <ol><li><a href="text/ch01.xhtml">Start</a></li></ol>
    </nav>
  </body>
</html>`

func newEPUB3(t *testing.T) ([]byte, int64) {
	t.Helper()
	return buildEPUB(t, map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": container,
		"OEBPS/content.opf":      epub3OPF,
		"OEBPS/nav.xhtml":        navDocXML,
		"OEBPS/images/cover.png": "PNG-BYTES",
		"OEBPS/text/ch01.xhtml":  "<html><body><p>One</p></body></html>",
		"OEBPS/text/ch02.xhtml":  "<html><body><p>Two</p></body></html>",
		"OEBPS/text/notes.xhtml": "<html><body><p>Notes</p></body></html>",
	})
}

func TestParseEPUB3Metadata(t *testing.T) {
	data, size := newEPUB3(t)

	res, err := Parse(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	m := res.Metadata
	if m.Title != "Structure and Interpretation of Computer Programs" {
		t.Errorf("Title = %q", m.Title)
	}
	if m.Subtitle != "A Gentle Introduction" {
		t.Errorf("Subtitle = %q, want the title-type=subtitle entry", m.Subtitle)
	}
	if m.Language != "en-GB" {
		t.Errorf("Language = %q", m.Language)
	}
	if m.Publisher != "MIT Press" {
		t.Errorf("Publisher = %q", m.Publisher)
	}
	if m.PublishedOn != "1996-07-25" {
		t.Errorf("PublishedOn = %q; dates are kept as claimed", m.PublishedOn)
	}
	if m.ISBN13 != "9780262510875" {
		t.Errorf("ISBN13 = %q, want the normalised urn:isbn value", m.ISBN13)
	}
	if m.Series != "MIT Electrical Engineering" {
		t.Errorf("Series = %q", m.Series)
	}
	if !reflect.DeepEqual(m.Subjects, []string{"Computer science", "Lisp"}) {
		t.Errorf("Subjects = %v", m.Subjects)
	}

	if len(m.Authors) != 2 {
		t.Fatalf("got %d authors, want 2", len(m.Authors))
	}
	if m.Authors[0].Name != "Harold Abelson" || m.Authors[0].SortName != "Abelson, Harold" {
		t.Errorf("author[0] = %+v, want the file-as refinement applied", m.Authors[0])
	}
	if m.Authors[0].Role != domain.RoleAuthor {
		t.Errorf("author[0].Role = %q, want author", m.Authors[0].Role)
	}
	if m.Authors[1].Role != domain.RoleEditor {
		t.Errorf("author[1].Role = %q, want editor from the edt refinement", m.Authors[1].Role)
	}

	// Every field found must record that it came from the file itself, or
	// enrichment cannot know what it is safe to overwrite.
	for _, field := range []string{"title", "subtitle", "authors", "language", "publisher", "isbn13"} {
		if got := m.Provenance[field]; got != domain.SourceEmbedded {
			t.Errorf("Provenance[%q] = %q, want embedded", field, got)
		}
	}
}

func TestParseEPUB3SpineIsReadingOrder(t *testing.T) {
	data, size := newEPUB3(t)

	res, err := Parse(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := []string{"OEBPS/text/ch01.xhtml", "OEBPS/text/ch02.xhtml", "OEBPS/text/notes.xhtml"}
	if len(res.Spine) != len(want) {
		t.Fatalf("got %d spine items, want %d", len(res.Spine), len(want))
	}
	for i, path := range want {
		if res.Spine[i].Path != path {
			t.Errorf("spine[%d].Path = %q, want %q", i, res.Spine[i].Path, path)
		}
		if res.Spine[i].Index != i {
			t.Errorf("spine[%d].Index = %d, want %d", i, res.Spine[i].Index, i)
		}
	}

	// linear="no" content keeps its slot: dropping it would renumber every
	// locator after it.
	if res.Spine[2].Linear {
		t.Error("notes.xhtml has linear=no and should not be marked linear")
	}
	if !res.Spine[0].Linear || !res.Spine[1].Linear {
		t.Error("chapters without a linear attribute should default to linear")
	}
}

func TestParseEPUB3TOCIsNotSpineOrder(t *testing.T) {
	data, size := newEPUB3(t)

	res, err := Parse(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(res.TOC) != 2 {
		t.Fatalf("got %d TOC entries, want 2; entries = %+v", len(res.TOC), res.TOC)
	}

	// The nav lists chapter 2 first. The TOC must preserve that, and must not be
	// silently reordered to match the spine.
	if res.TOC[0].Title != "Building Abstractions" {
		t.Errorf("TOC[0].Title = %q, want the nav document's first entry", res.TOC[0].Title)
	}
	if res.TOC[0].SpineIndex != 1 {
		t.Errorf("TOC[0].SpineIndex = %d, want 1", res.TOC[0].SpineIndex)
	}
	if res.TOC[1].SpineIndex != 0 {
		t.Errorf("TOC[1].SpineIndex = %d, want 0", res.TOC[1].SpineIndex)
	}

	if len(res.TOC[0].Children) != 1 {
		t.Fatalf("got %d children, want 1", len(res.TOC[0].Children))
	}
	child := res.TOC[0].Children[0]
	if child.Fragment != "sec-2-1" {
		t.Errorf("child.Fragment = %q, want sec-2-1", child.Fragment)
	}
	if child.Target != "OEBPS/text/ch02.xhtml" {
		t.Errorf("child.Target = %q, want the fragment stripped", child.Target)
	}

	// The landmarks nav is not the table of contents.
	for _, e := range res.TOC {
		if e.Title == "Start" {
			t.Error("landmarks nav leaked into the TOC")
		}
	}
}

func TestParseEPUB3Cover(t *testing.T) {
	data, size := newEPUB3(t)

	res, err := Parse(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Cover == nil {
		t.Fatal("no cover extracted")
	}
	if res.Cover.Path != "OEBPS/images/cover.png" {
		t.Errorf("Cover.Path = %q", res.Cover.Path)
	}
	if string(res.Cover.Data) != "PNG-BYTES" {
		t.Errorf("Cover.Data = %q", res.Cover.Data)
	}
}

// ---------------------------------------------------------------------------
// EPUB 2
// ---------------------------------------------------------------------------

const epub2OPF = `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="2.0" unique-identifier="bookid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:opf="http://www.idpf.org/2007/opf">
    <dc:title>The C Programming Language</dc:title>
    <dc:creator opf:file-as="Ritchie, Dennis" opf:role="aut">Dennis Ritchie</dc:creator>
    <dc:language>en</dc:language>
    <dc:identifier id="bookid" opf:scheme="ISBN">0-13-110362-8</dc:identifier>
    <meta name="cover" content="cover-image"/>
    <meta name="calibre:series" content="Prentice Hall Software"/>
    <meta name="calibre:series_index" content="2.5"/>
  </metadata>
  <manifest>
    <item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml"/>
    <item id="cover-image" href="cover.jpg" media-type="image/jpeg"/>
    <item id="c1" href="ch1.html" media-type="application/xhtml+xml"/>
    <item id="c2" href="ch2.html" media-type="application/xhtml+xml"/>
  </manifest>
  <spine toc="ncx">
    <itemref idref="c1"/>
    <itemref idref="c2"/>
  </spine>
</package>`

// playOrder deliberately disagrees with document order.
const ncxDocument = `<?xml version="1.0" encoding="UTF-8"?>
<ncx xmlns="http://www.daisy.org/z3986/2005/ncx/" version="2005-1">
  <navMap>
    <navPoint id="np2" playOrder="2">
      <navLabel><text>Types, Operators and Expressions</text></navLabel>
      <content src="ch2.html"/>
    </navPoint>
    <navPoint id="np1" playOrder="1">
      <navLabel><text>A Tutorial Introduction</text></navLabel>
      <content src="ch1.html"/>
    </navPoint>
  </navMap>
</ncx>`

func TestParseEPUB2(t *testing.T) {
	data, size := buildEPUB(t, map[string]string{
		"META-INF/container.xml": container,
		"OEBPS/content.opf":      epub2OPF,
		"OEBPS/toc.ncx":          ncxDocument,
		"OEBPS/cover.jpg":        "JPEG-BYTES",
		"OEBPS/ch1.html":         "<html><body><p>One</p></body></html>",
		"OEBPS/ch2.html":         "<html><body><p>Two</p></body></html>",
	})

	res, err := Parse(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if res.Metadata.Title != "The C Programming Language" {
		t.Errorf("Title = %q", res.Metadata.Title)
	}
	if res.Metadata.ISBN10 != "0131103628" {
		t.Errorf("ISBN10 = %q, want the hyphens stripped", res.Metadata.ISBN10)
	}
	if res.Metadata.Series != "Prentice Hall Software" {
		t.Errorf("Series = %q, want the calibre convention honoured", res.Metadata.Series)
	}
	if res.Metadata.SeriesIndex == nil || *res.Metadata.SeriesIndex != 2.5 {
		t.Errorf("SeriesIndex = %v, want 2.5", res.Metadata.SeriesIndex)
	}
	if len(res.Metadata.Authors) != 1 || res.Metadata.Authors[0].SortName != "Ritchie, Dennis" {
		t.Errorf("Authors = %+v, want opf:file-as honoured", res.Metadata.Authors)
	}

	if res.Cover == nil || string(res.Cover.Data) != "JPEG-BYTES" {
		t.Errorf("Cover = %+v, want the meta name=cover target", res.Cover)
	}

	// playOrder wins over document order.
	if len(res.TOC) != 2 {
		t.Fatalf("got %d TOC entries, want 2", len(res.TOC))
	}
	if res.TOC[0].Title != "A Tutorial Introduction" {
		t.Errorf("TOC[0] = %q, want playOrder 1 first", res.TOC[0].Title)
	}
	if res.TOC[0].SpineIndex != 0 || res.TOC[1].SpineIndex != 1 {
		t.Errorf("TOC spine indices = %d, %d", res.TOC[0].SpineIndex, res.TOC[1].SpineIndex)
	}
}

// ---------------------------------------------------------------------------
// Malformed and hostile input
// ---------------------------------------------------------------------------

func TestParseRejectsMalformed(t *testing.T) {
	tests := []struct {
		name    string
		entries map[string]string
	}{
		{
			name:    "not a zip at all",
			entries: nil,
		},
		{
			name:    "missing container.xml",
			entries: map[string]string{"OEBPS/content.opf": epub3OPF},
		},
		{
			name: "container names a missing OPF",
			entries: map[string]string{
				"META-INF/container.xml": container,
			},
		},
		{
			name: "OPF is not XML",
			entries: map[string]string{
				"META-INF/container.xml": container,
				"OEBPS/content.opf":      "this is not xml at all <<<",
			},
		},
		{
			name: "empty spine",
			entries: map[string]string{
				"META-INF/container.xml": container,
				"OEBPS/content.opf": `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Empty</dc:title></metadata>
  <manifest><item id="a" href="a.xhtml" media-type="application/xhtml+xml"/></manifest>
  <spine></spine>
</package>`,
			},
		},
		{
			name: "spine references only unknown ids",
			entries: map[string]string{
				"META-INF/container.xml": container,
				"OEBPS/content.opf": `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Dangling</dc:title></metadata>
  <manifest><item id="a" href="a.xhtml" media-type="application/xhtml+xml"/></manifest>
  <spine><itemref idref="nope"/></spine>
</package>`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var data []byte
			var size int64
			if tt.entries == nil {
				data = []byte("definitely not a zip file")
				size = int64(len(data))
			} else {
				data, size = buildEPUB(t, tt.entries)
			}

			_, err := Parse(bytes.NewReader(data), size)
			if err == nil {
				t.Fatal("Parse accepted a malformed file")
			}
			if !errors.Is(err, ErrNotEPUB) {
				t.Fatalf("error = %v, want ErrNotEPUB", err)
			}
		})
	}
}

// A container.xml pointing outside the archive must not be followed.
func TestParseRejectsTraversingRootfile(t *testing.T) {
	data, size := buildEPUB(t, map[string]string{
		"META-INF/container.xml": `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="../../../etc/passwd" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>`,
		"OEBPS/content.opf": epub3OPF,
	})

	if _, err := Parse(bytes.NewReader(data), size); !errors.Is(err, ErrNotEPUB) {
		t.Fatalf("error = %v, want ErrNotEPUB", err)
	}
}

// A book missing its TOC or cover is still a book.
func TestParseSurvivesMissingTOCAndCover(t *testing.T) {
	data, size := buildEPUB(t, map[string]string{
		"META-INF/container.xml": container,
		"OEBPS/content.opf": `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Bare</dc:title></metadata>
  <manifest><item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/></manifest>
  <spine><itemref idref="c1"/></spine>
</package>`,
		"OEBPS/ch1.xhtml": "<html><body><p>Only chapter</p></body></html>",
	})

	res, err := Parse(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Spine) != 1 {
		t.Fatalf("got %d spine items, want 1", len(res.Spine))
	}
	if res.Cover != nil {
		t.Errorf("Cover = %+v, want nil", res.Cover)
	}
	if len(res.Warnings) == 0 {
		t.Error("missing TOC should produce a warning")
	}
}

// A file with no declared title gets an empty title and a warning, never a
// value invented from the filename: a guess recorded as embedded metadata would
// outrank the truth forever.
func TestParseDoesNotInventATitle(t *testing.T) {
	data, size := buildEPUB(t, map[string]string{
		"META-INF/container.xml": container,
		"OEBPS/content.opf": `<?xml version="1.0"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/"></metadata>
  <manifest><item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/></manifest>
  <spine><itemref idref="c1"/></spine>
</package>`,
		"OEBPS/ch1.xhtml": "<html><body><p>x</p></body></html>",
	})

	res, err := Parse(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Metadata.Title != "" {
		t.Errorf("Title = %q, want empty", res.Metadata.Title)
	}
	if _, recorded := res.Metadata.Provenance["title"]; recorded {
		t.Error("an absent title must not be given provenance")
	}
}

// Chunk ids, highlight anchors, and citations all assume the same bytes always
// produce the same spine indices and paths.
func TestParseIsDeterministic(t *testing.T) {
	data, size := newEPUB3(t)

	first, err := Parse(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	for i := 0; i < 20; i++ {
		next, err := Parse(bytes.NewReader(data), size)
		if err != nil {
			t.Fatalf("Parse %d: %v", i, err)
		}
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("parse %d differs from the first:\nfirst = %+v\nnext  = %+v", i, first, next)
		}
	}
}

func TestNormaliseISBN(t *testing.T) {
	tests := map[string]string{
		"urn:isbn:978-0-262-51087-5": "9780262510875",
		"ISBN 0-13-110362-8":         "0131103628",
		"9780262510875":              "9780262510875",
		"013110362X":                 "013110362X",
		"":                           "",
		"not-an-isbn":                "",
		"12345":                      "",
		"01X1103628":                 "", // X is only legal as the final character
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := normaliseISBN(in); got != want {
				t.Fatalf("normaliseISBN(%q) = %q, want %q", in, got, want)
			}
		})
	}
}
