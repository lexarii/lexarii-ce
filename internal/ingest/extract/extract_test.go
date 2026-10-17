// SPDX-License-Identifier: AGPL-3.0-or-later

package extract_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest/extract"
)

// writeEPUB writes a small but genuinely valid EPUB to disk.
func writeEPUB(t *testing.T, path, title, author string) {
	t.Helper()

	opf := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:title>%s</dc:title>
    <dc:creator>%s</dc:creator>
    <dc:language>en</dc:language>
  </metadata>
  <manifest>
    <item id="c1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
    <item id="c2" href="ch2.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="c1"/><itemref idref="c2"/></spine>
</package>`, title, author)

	entries := map[string]string{
		"mimetype": "application/epub+zip",
		"META-INF/container.xml": `<?xml version="1.0"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`,
		"OEBPS/content.opf": opf,
		"OEBPS/ch1.xhtml":   "<html><body><p>Chapter one.</p></body></html>",
		"OEBPS/ch2.xhtml":   "<html><body><p>Chapter two.</p></body></html>",
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// Deterministic entry order so the fixture bytes are stable.
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
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

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// The whole M1 path: a directory of real files in, structured books out.
func TestScanThenExtract(t *testing.T) {
	root := t.TempDir()

	writeEPUB(t, filepath.Join(root, "Knuth", "taocp.epub"),
		"The Art of Computer Programming", "Donald Knuth")
	writeEPUB(t, filepath.Join(root, "Fiction", "dispossessed.epub"),
		"The Dispossessed", "Ursula K. Le Guin")

	// Junk that must be ignored or reported, never ingested as a book.
	if err := os.WriteFile(filepath.Join(root, "Knuth", "cover.jpg"), []byte("not a book"), 0o644); err != nil {
		t.Fatalf("writing jpg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "broken.epub"), []byte("not a zip at all"), 0o644); err != nil {
		t.Fatalf("writing broken epub: %v", err)
	}

	scanner := &ingest.Scanner{Root: root}

	type outcome struct {
		rel    string
		title  string
		status domain.IngestStatus
		units  int
	}
	var got []outcome

	stats, err := scanner.Scan(t.Context(), func(f ingest.ScannedFile) error {
		if f.Err != nil {
			got = append(got, outcome{rel: filepath.ToSlash(f.RelPath), status: domain.IngestFailed})
			return nil
		}
		res, perr := extract.File(t.Context(), f.Path, f.Format)
		status := extract.StatusFor(res, perr)
		o := outcome{rel: filepath.ToSlash(f.RelPath), status: status}
		if perr == nil {
			o.title = res.Metadata.Title
			o.units = len(res.Spine)
		}
		got = append(got, o)
		return nil
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if stats.BooksFound != 3 {
		t.Errorf("BooksFound = %d, want 3 (two good epubs and one broken one)", stats.BooksFound)
	}

	sort.Slice(got, func(i, j int) bool { return got[i].rel < got[j].rel })

	want := []outcome{
		{rel: "Fiction/dispossessed.epub", title: "The Dispossessed", status: domain.IngestReady, units: 2},
		{rel: "Knuth/taocp.epub", title: "The Art of Computer Programming", status: domain.IngestReady, units: 2},
		{rel: "broken.epub", status: domain.IngestFailed},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d outcomes, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("outcome[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// One malformed file must not stop the others being ingested.
func TestBrokenFileDoesNotStopTheScan(t *testing.T) {
	root := t.TempDir()

	writeEPUB(t, filepath.Join(root, "good.epub"), "Good Book", "An Author")
	if err := os.WriteFile(filepath.Join(root, "bad.epub"), []byte("garbage"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}

	succeeded, failed := 0, 0
	_, err := (&ingest.Scanner{Root: root}).Scan(t.Context(), func(f ingest.ScannedFile) error {
		if f.Err != nil {
			failed++
			return nil
		}
		if _, perr := extract.File(t.Context(), f.Path, f.Format); perr != nil {
			failed++
			return nil
		}
		succeeded++
		return nil
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if succeeded != 1 {
		t.Errorf("succeeded = %d, want 1", succeeded)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
}

func TestUnsupportedFormatIsReportedNotGuessed(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "comic.cbz")
	if err := os.WriteFile(path, []byte("PK\x03\x04 not really"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}

	res, err := extract.File(t.Context(), path, domain.FormatCBZ)
	if !errors.Is(err, extract.ErrUnsupportedFormat) {
		t.Fatalf("error = %v, want ErrUnsupportedFormat", err)
	}
	if status := extract.StatusFor(res, err); status != domain.IngestUnsupported {
		t.Errorf("StatusFor = %q, want unsupported", status)
	}
}

func TestStatusFor(t *testing.T) {
	tests := []struct {
		name string
		res  *ingest.Result
		err  error
		want domain.IngestStatus
	}{
		{"clean parse", &ingest.Result{}, nil, domain.IngestReady},
		{"scanned pdf", &ingest.Result{NeedsOCR: true}, nil, domain.IngestNeedsOCR},
		{"parse failure", nil, errors.New("boom"), domain.IngestFailed},
		{"unsupported", nil, extract.ErrUnsupportedFormat, domain.IngestUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extract.StatusFor(tt.res, tt.err); got != tt.want {
				t.Fatalf("StatusFor = %q, want %q", got, tt.want)
			}
		})
	}
}

// Re-running a scan over an unchanged library must produce byte-identical
// hashes, or every scan would look like a library full of new books.
func TestRescanIsStable(t *testing.T) {
	root := t.TempDir()
	writeEPUB(t, filepath.Join(root, "book.epub"), "Stable", "Author")

	hashes := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		_, err := (&ingest.Scanner{Root: root}).Scan(t.Context(), func(f ingest.ScannedFile) error {
			hashes = append(hashes, f.ContentHash)
			return nil
		})
		if err != nil {
			t.Fatalf("scan %d: %v", i, err)
		}
	}

	for i, h := range hashes {
		if h != hashes[0] {
			t.Fatalf("scan %d produced hash %q, want %q", i, h, hashes[0])
		}
	}
}

// A file moved or renamed within the library keeps its hash, which is how
// re-ingest recognises it instead of creating a duplicate book.
func TestMovedFileKeepsItsIdentity(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "Inbox", "book.epub")
	writeEPUB(t, original, "Moved Book", "Author")

	var before string
	if _, err := (&ingest.Scanner{Root: root}).Scan(t.Context(), func(f ingest.ScannedFile) error {
		before = f.ContentHash
		return nil
	}); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	moved := filepath.Join(root, "Sorted", "Author", "renamed.epub")
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Rename(original, moved); err != nil {
		t.Fatalf("rename: %v", err)
	}

	var after string
	var afterPath string
	if _, err := (&ingest.Scanner{Root: root}).Scan(t.Context(), func(f ingest.ScannedFile) error {
		after = f.ContentHash
		afterPath = filepath.ToSlash(f.RelPath)
		return nil
	}); err != nil {
		t.Fatalf("second scan: %v", err)
	}

	if before == "" || after != before {
		t.Fatalf("hash changed across a move: %q -> %q", before, after)
	}
	if afterPath != "Sorted/Author/renamed.epub" {
		t.Errorf("path = %q, want the new location", afterPath)
	}
}

// An unreadable format is reported as unsupported, not as a failure: there is
// nothing wrong with the file, Lexarii simply cannot read it yet.
func TestUnsupportedFormatsAreDistinguishedFromFailures(t *testing.T) {
	root := t.TempDir()

	// MOBI and AZW3 used to be here. They are read now, so a .mobi holding
	// seven bytes of nonsense is a FAILED file rather than an unsupported one -
	// which is the honest answer and a different one: nothing is wrong with a
	// comic, while something is wrong with that.
	for name, format := range map[string]domain.Format{
		"comic.cbz": domain.FormatCBZ,
		"comic.cbr": domain.FormatCBR,
	} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
		res, err := extract.File(t.Context(), path, format)
		if !errors.Is(err, extract.ErrUnsupportedFormat) {
			t.Errorf("%s: error = %v, want ErrUnsupportedFormat", name, err)
		}
		if got := extract.StatusFor(res, err); got != domain.IngestUnsupported {
			t.Errorf("%s: status = %q, want unsupported", name, got)
		}
	}
}

// A Kindle book that is not one is a failure, and says which kind.
//
// The distinction matters to somebody looking at a shelf: "not supported yet"
// is a promise about Lexarii, and "could not be read" is a statement about
// the file.
func TestARuinedKindleBookFailsRatherThanBeingUnsupported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "book.mobi")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}

	res, err := extract.File(t.Context(), path, domain.FormatMOBI)
	if err == nil {
		t.Fatal("seven bytes of nonsense parsed as a book")
	}
	if errors.Is(err, extract.ErrUnsupportedFormat) {
		t.Error("reported as unsupported; MOBI is supported, this file is broken")
	}
	if got := extract.StatusFor(res, err); got != domain.IngestFailed {
		t.Errorf("status = %q, want failed", got)
	}
}
