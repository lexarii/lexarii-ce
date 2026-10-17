// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// rawZipEntry is one entry written with headers the test chooses, so an
// archive can lie about its sizes the way a hostile one would.
type rawZipEntry struct {
	name         string
	method       uint16
	payload      []byte // written verbatim, already compressed
	compressed   uint64
	uncompressed uint64
}

// buildLyingZip writes entries through CreateRaw, which trusts the declared
// sizes and checksum instead of computing them.
func buildLyingZip(t *testing.T, entries []rawZipEntry) ([]byte, int64) {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.CreateRaw(&zip.FileHeader{
			Name:               e.name,
			Method:             e.method,
			CompressedSize64:   e.compressed,
			UncompressedSize64: e.uncompressed,
		})
		if err != nil {
			t.Fatalf("creating %q: %v", e.name, err)
		}
		if _, err := w.Write(e.payload); err != nil {
			t.Fatalf("writing %q: %v", e.name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes(), int64(buf.Len())
}

func deflated(t *testing.T, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		t.Fatalf("flate: %v", err)
	}
	if _, err := fw.Write(plain); err != nil {
		t.Fatalf("flate write: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("flate close: %v", err)
	}
	return buf.Bytes()
}

// A file that is not an archive at all, or one cut off mid-download, fails
// that one file with an error rather than panicking or yielding entries.
func TestOpenSafeZipRejectsMalformedArchives(t *testing.T) {
	good, _ := buildZip(t, map[string]string{"OEBPS/ch1.xhtml": "<p>hello</p>"})

	tests := []struct {
		name string
		data []byte
	}{
		{"not a zip", []byte("%PDF-1.4 this is not an archive at all")},
		{"empty", nil},
		{"truncated central directory", good[:len(good)-10]},
		{"only the first half", good[:len(good)/2]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			z, _, err := OpenSafeZip(bytes.NewReader(tt.data), int64(len(tt.data)))
			if err == nil {
				t.Fatalf("OpenSafeZip accepted a malformed archive with entries %v", z.Names())
			}
			if !errors.Is(err, zip.ErrFormat) {
				t.Errorf("error = %v, want one wrapping zip.ErrFormat", err)
			}
		})
	}
}

// The entry-count cap is checked before any entry is looked at, so a million
// tiny entries cost one header read rather than a million map inserts.
func TestOpenSafeZipCapsTheEntryCount(t *testing.T) {
	build := func(n int) ([]byte, int64) {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for i := 0; i < n; i++ {
			if _, err := zw.CreateHeader(&zip.FileHeader{Name: "e/" + strconv.Itoa(i), Method: zip.Store}); err != nil {
				t.Fatalf("creating entry %d: %v", i, err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("closing zip: %v", err)
		}
		return buf.Bytes(), int64(buf.Len())
	}

	data, size := build(MaxEntries + 1)
	if _, _, err := OpenSafeZip(bytes.NewReader(data), size); !errors.Is(err, ErrTooManyEntries) {
		t.Fatalf("%d entries: error = %v, want ErrTooManyEntries", MaxEntries+1, err)
	}

	data, size = build(MaxEntries)
	z, skipped, err := OpenSafeZip(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("exactly %d entries: %v", MaxEntries, err)
	}
	if skipped != 0 || len(z.Names()) != MaxEntries {
		t.Errorf("exactly at the cap: %d names, %d skipped; want %d and 0", len(z.Names()), skipped, MaxEntries)
	}
}

// Directory entries carry no content: they are neither addressable nor
// counted as hostile.
func TestOpenSafeZipIgnoresDirectoryEntries(t *testing.T) {
	data, size := buildZip(t, map[string]string{
		"OEBPS/":          "",
		"OEBPS/text/":     "",
		"OEBPS/ch1.xhtml": "<p>one</p>",
	})

	z, skipped, err := OpenSafeZip(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("OpenSafeZip: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0: a directory is not an attack", skipped)
	}
	if got := z.Names(); !reflect.DeepEqual(got, []string{"OEBPS/ch1.xhtml"}) {
		t.Errorf("Names = %v, want only the file", got)
	}
	if z.Has("OEBPS") || z.Has("OEBPS/") {
		t.Error("a directory entry is addressable")
	}
}

// Names hands out a copy: a caller sorting or trimming it must not reorder
// the archive for everyone else.
func TestNamesIsACopy(t *testing.T) {
	data, size := buildZip(t, map[string]string{"a.xhtml": "a", "b.xhtml": "b"})
	z, _, err := OpenSafeZip(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("OpenSafeZip: %v", err)
	}
	names := z.Names()
	names[0] = "../../etc/passwd"
	for _, n := range z.Names() {
		if strings.Contains(n, "..") {
			t.Fatalf("mutating the returned slice changed the archive: %v", z.Names())
		}
	}
}

// The declared sizes are checked at open, so an entry claiming to be huge, or
// claiming an expansion only a bomb achieves, is never decompressed at all.
func TestOpenSafeZipSkipsEntriesWhoseHeadersDeclareABomb(t *testing.T) {
	small := deflated(t, []byte("<p>fine</p>"))
	data, size := buildLyingZip(t, []rawZipEntry{
		{name: "OEBPS/huge.xhtml", method: zip.Deflate, payload: small,
			compressed: uint64(len(small)), uncompressed: MaxEntrySize + 1},
		{name: "OEBPS/ratio.xhtml", method: zip.Deflate, payload: small,
			compressed: 10, uncompressed: 10 * (MaxCompressionRatio + 1)},
		{name: "OEBPS/ok.xhtml", method: zip.Deflate, payload: small,
			compressed: uint64(len(small)), uncompressed: uint64(len("<p>fine</p>"))},
	})

	z, skipped, err := OpenSafeZip(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("OpenSafeZip: %v", err)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2", skipped)
	}
	for _, name := range []string{"OEBPS/huge.xhtml", "OEBPS/ratio.xhtml"} {
		if z.Has(name) {
			t.Errorf("%s is reachable", name)
		}
		if _, err := z.ReadFile(name); !errors.Is(err, ErrEntryNotFound) {
			t.Errorf("ReadFile(%s) error = %v, want ErrEntryNotFound", name, err)
		}
	}
	if !z.Has("OEBPS/ok.xhtml") {
		t.Error("the honest entry was dropped with the bombs")
	}
}

// A header that understates the size must not let the real data through, and
// must not truncate it into plausible-looking wrong text either: the read
// fails.
func TestReadFileRefusesAnEntryLargerThanItsHeaderSays(t *testing.T) {
	plain := bytes.Repeat([]byte("lorem ipsum "), 4096)
	payload := deflated(t, plain)
	data, size := buildLyingZip(t, []rawZipEntry{
		{name: "OEBPS/liar.xhtml", method: zip.Deflate, payload: payload,
			compressed: uint64(len(payload)), uncompressed: 64},
	})

	z, skipped, err := OpenSafeZip(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("OpenSafeZip: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d; the lie is only detectable on read", skipped)
	}
	got, err := z.ReadFile("OEBPS/liar.xhtml")
	if err == nil {
		t.Fatalf("ReadFile returned %d bytes from an entry that lied about its size", len(got))
	}
	if !errors.Is(err, zip.ErrFormat) {
		t.Errorf("error = %v, want zip.ErrFormat", err)
	}
	if got != nil {
		t.Errorf("a failed read returned %d bytes", len(got))
	}
}

// An entry compressed with a method nobody implements is an error on that
// entry, not on the archive.
func TestReadFileReportsAnUnsupportedCompressionMethod(t *testing.T) {
	data, size := buildLyingZip(t, []rawZipEntry{
		{name: "OEBPS/odd.xhtml", method: 99, payload: []byte("xxxx"), compressed: 4, uncompressed: 4},
	})
	z, _, err := OpenSafeZip(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("OpenSafeZip: %v", err)
	}
	if _, err := z.ReadFile("OEBPS/odd.xhtml"); !errors.Is(err, zip.ErrAlgorithm) {
		t.Errorf("error = %v, want zip.ErrAlgorithm", err)
	}
}

// Lookups go through the same sanitiser as the archive did, so a traversing
// name is refused before it reaches the map.
func TestLookupsRejectUnsafeNames(t *testing.T) {
	data, size := buildZip(t, map[string]string{"OEBPS/ch1.xhtml": "<p>one</p>"})
	z, _, err := OpenSafeZip(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("OpenSafeZip: %v", err)
	}

	for _, name := range []string{"../OEBPS/ch1.xhtml", "/OEBPS/ch1.xhtml", "", `..\..\x`} {
		if z.Has(name) {
			t.Errorf("Has(%q) = true", name)
		}
		if _, err := z.ReadFile(name); !errors.Is(err, ErrUnsafePath) {
			t.Errorf("ReadFile(%q) error = %v, want ErrUnsafePath", name, err)
		}
	}

	// A harmless alternative spelling of a real entry is the same entry.
	for _, name := range []string{"OEBPS/./ch1.xhtml", `OEBPS\ch1.xhtml`, "OEBPS/text/../ch1.xhtml"} {
		got, err := z.ReadFile(name)
		if err != nil || string(got) != "<p>one</p>" {
			t.Errorf("ReadFile(%q) = %q, %v; want the entry", name, got, err)
		}
	}
}

// The cumulative cap is what stops a thousand modest entries from adding up
// to a bomb. Reading a real gigabyte in a unit test is not reasonable, so the
// running total is set directly to where a long archive would have left it.
func TestReadFileEnforcesTheCumulativeCap(t *testing.T) {
	data, size := buildZip(t, map[string]string{"OEBPS/ch1.xhtml": "0123456789"})

	t.Run("already at the cap", func(t *testing.T) {
		z, _, err := OpenSafeZip(bytes.NewReader(data), size)
		if err != nil {
			t.Fatalf("OpenSafeZip: %v", err)
		}
		z.readSoFar = MaxTotalSize
		if _, err := z.ReadFile("OEBPS/ch1.xhtml"); !errors.Is(err, ErrArchiveTooLarge) {
			t.Errorf("error = %v, want ErrArchiveTooLarge", err)
		}
	})

	t.Run("this entry crosses the cap", func(t *testing.T) {
		z, _, err := OpenSafeZip(bytes.NewReader(data), size)
		if err != nil {
			t.Fatalf("OpenSafeZip: %v", err)
		}
		z.readSoFar = MaxTotalSize - 5
		got, err := z.ReadFile("OEBPS/ch1.xhtml")
		if !errors.Is(err, ErrArchiveTooLarge) {
			t.Errorf("error = %v, want ErrArchiveTooLarge", err)
		}
		if got != nil {
			t.Errorf("returned %q past the cap", got)
		}
		if z.readSoFar != MaxTotalSize-5 {
			t.Errorf("a refused read was counted: readSoFar = %d", z.readSoFar)
		}
	})

	t.Run("reads add up", func(t *testing.T) {
		z, _, err := OpenSafeZip(bytes.NewReader(data), size)
		if err != nil {
			t.Fatalf("OpenSafeZip: %v", err)
		}
		for i := 0; i < 3; i++ {
			if _, err := z.ReadFile("OEBPS/ch1.xhtml"); err != nil {
				t.Fatalf("read %d: %v", i, err)
			}
		}
		if z.readSoFar != 30 {
			t.Errorf("readSoFar = %d after three 10-byte reads, want 30", z.readSoFar)
		}
	})
}

// SanitizePath is the root-relative entry point (container.xml's full-path)
// and must hold the same line as the archive itself.
func TestSanitizePath(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"OEBPS/content.opf", "OEBPS/content.opf", false},
		{"./OEBPS//content.opf", "OEBPS/content.opf", false},
		{`OEBPS\content.opf`, "OEBPS/content.opf", false},
		{"../content.opf", "", true},
		{"/OEBPS/content.opf", "", true},
		{`\OEBPS\content.opf`, "", true},
		{"x/../C:evil", "", true},
		{"", "", true},
	}
	for _, tt := range tests {
		got, err := SanitizePath(tt.in)
		if tt.wantErr {
			if !errors.Is(err, ErrUnsafePath) {
				t.Errorf("SanitizePath(%q) = %q, %v; want ErrUnsafePath", tt.in, got, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("SanitizePath(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}
