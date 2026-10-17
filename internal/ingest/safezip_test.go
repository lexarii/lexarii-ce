// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"archive/zip"
	"bytes"
	"errors"
	"path"
	"strings"
	"testing"
)

func TestSanitizeEntryPathRejectsTraversal(t *testing.T) {
	attacks := []struct {
		name  string
		entry string
	}{
		{"parent escape", "../evil.txt"},
		{"nested parent escape", "OEBPS/../../evil.txt"},
		{"deep escape", "../../../../etc/passwd"},
		{"bare parent", ".."},
		{"absolute", "/etc/passwd"},
		{"backslash escape", `..\..\evil.txt`},
		{"mixed separators", `OEBPS\..\..\evil.txt`},
		{"drive letter", "C:/Windows/system32"},
		{"unc path", `\\server\share\evil`},
		{"nul byte", "OEBPS/ev\x00il.txt"},
		{"empty", ""},
		{"current dir only", "."},
		{"trailing traversal", "OEBPS/.."},
	}

	for _, tt := range attacks {
		t.Run(tt.name, func(t *testing.T) {
			got, err := sanitizeEntryPath(tt.entry)
			if err == nil {
				t.Fatalf("sanitizeEntryPath(%q) = %q, want error", tt.entry, got)
			}
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("error = %v, want ErrUnsafePath", err)
			}
		})
	}
}

func TestSanitizeEntryPathAcceptsRealNames(t *testing.T) {
	tests := []struct {
		entry string
		want  string
	}{
		{"META-INF/container.xml", "META-INF/container.xml"},
		{"OEBPS/content.opf", "OEBPS/content.opf"},
		{"OEBPS/./text/ch01.xhtml", "OEBPS/text/ch01.xhtml"},
		{"OEBPS/text/../images/cover.jpg", "OEBPS/images/cover.jpg"},
		{"mimetype", "mimetype"},
		{`OEBPS\text\ch01.xhtml`, "OEBPS/text/ch01.xhtml"},
		{"Ünïcødé/chäpter.xhtml", "Ünïcødé/chäpter.xhtml"},
	}

	for _, tt := range tests {
		t.Run(tt.entry, func(t *testing.T) {
			got, err := sanitizeEntryPath(tt.entry)
			if err != nil {
				t.Fatalf("sanitizeEntryPath(%q) returned error: %v", tt.entry, err)
			}
			if got != tt.want {
				t.Fatalf("sanitizeEntryPath(%q) = %q, want %q", tt.entry, got, tt.want)
			}
		})
	}
}

// The invariant that matters: whatever comes out must stay inside the archive
// root. Entry names come from files downloaded off the internet, so the sanitiser
// is fuzzed rather than only sampled.
func FuzzSanitizeEntryPath(f *testing.F) {
	seeds := []string{
		"META-INF/container.xml", "../evil", "..", "/abs", `..\win`,
		"a/./b", "a/../../b", "", ".", "C:/x", "a\x00b", "OEBPS/ch1.xhtml",
		"////", "a//b", "....//", "..././..",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, name string) {
		got, err := sanitizeEntryPath(name)
		if err != nil {
			return // rejection is always a safe outcome
		}

		if path.IsAbs(got) {
			t.Fatalf("sanitizeEntryPath(%q) = %q, which is absolute", name, got)
		}
		if got == ".." || strings.HasPrefix(got, "../") {
			t.Fatalf("sanitizeEntryPath(%q) = %q, which escapes the root", name, got)
		}
		if strings.ContainsRune(got, 0) {
			t.Fatalf("sanitizeEntryPath(%q) = %q, which contains a NUL", name, got)
		}
		if got == "" || got == "." {
			t.Fatalf("sanitizeEntryPath(%q) = %q, which is not a usable entry", name, got)
		}
		// Joining the result onto a root must not leave that root.
		if joined := path.Join("/root", got); !strings.HasPrefix(joined, "/root/") {
			t.Fatalf("sanitizeEntryPath(%q) = %q, which escapes when joined: %q", name, got, joined)
		}
		// Sanitising an already-sanitised path must be a no-op, or callers that
		// normalise twice get different answers.
		again, err := sanitizeEntryPath(got)
		if err != nil || again != got {
			t.Fatalf("not idempotent: %q -> %q -> (%q, %v)", name, got, again, err)
		}
	})
}

// buildZip writes an in-memory archive. Entry names are written verbatim,
// including hostile ones, which is the point.
func buildZip(t *testing.T, entries map[string]string) ([]byte, int64) {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range entries {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatalf("creating entry %q: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("writing entry %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return buf.Bytes(), int64(buf.Len())
}

func TestOpenSafeZipSkipsUnsafeEntriesButKeepsGoodOnes(t *testing.T) {
	data, size := buildZip(t, map[string]string{
		"META-INF/container.xml": "<container/>",
		"OEBPS/content.opf":      "<package/>",
		"../../../etc/passwd":    "root:x:0:0",
		"/absolute":              "nope",
	})

	z, skipped, err := OpenSafeZip(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("OpenSafeZip: %v", err)
	}
	if skipped != 2 {
		t.Fatalf("skipped = %d, want 2", skipped)
	}

	if !z.Has("META-INF/container.xml") || !z.Has("OEBPS/content.opf") {
		t.Fatalf("legitimate entries missing; have %v", z.Names())
	}

	// The traversing entry must be unreachable by any spelling, not merely
	// undocumented.
	for _, spelling := range []string{"../../../etc/passwd", "/absolute", "etc/passwd"} {
		if z.Has(spelling) {
			t.Errorf("unsafe entry reachable as %q", spelling)
		}
		if _, err := z.ReadFile(spelling); err == nil {
			t.Errorf("ReadFile(%q) succeeded on an unsafe entry", spelling)
		}
	}
}

func TestReadFileReturnsContent(t *testing.T) {
	data, size := buildZip(t, map[string]string{"OEBPS/ch1.xhtml": "<p>Hello</p>"})

	z, _, err := OpenSafeZip(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("OpenSafeZip: %v", err)
	}

	got, err := z.ReadFile("OEBPS/ch1.xhtml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "<p>Hello</p>" {
		t.Fatalf("ReadFile = %q, want %q", got, "<p>Hello</p>")
	}

	if _, err := z.ReadFile("OEBPS/missing.xhtml"); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("missing entry error = %v, want ErrEntryNotFound", err)
	}
}

// A highly compressible entry must be stopped by the ratio check before it is
// ever decompressed, not after it has filled memory.
func TestZipBombIsRejected(t *testing.T) {
	bomb := strings.Repeat("A", 8<<20) // 8 MiB of one byte: compresses ~1000:1
	data, size := buildZip(t, map[string]string{
		"OEBPS/bomb.xhtml": bomb,
		"OEBPS/ok.xhtml":   "<p>fine</p>",
	})

	z, skipped, err := OpenSafeZip(bytes.NewReader(data), size)
	if err != nil {
		t.Fatalf("OpenSafeZip: %v", err)
	}

	// Either the ratio check skipped it at open time, or the read must fail.
	// Both are acceptable; silently returning 8 MiB is not.
	if skipped == 0 {
		if _, err := z.ReadFile("OEBPS/bomb.xhtml"); err == nil {
			t.Fatal("bomb entry read without error")
		}
	}
	if !z.Has("OEBPS/ok.xhtml") {
		t.Fatal("the legitimate entry was dropped along with the bomb")
	}
}

// Two entries with the same name is a known trick for showing one file to a
// validator and another to a reader.
func TestDuplicateEntryNamesKeepTheFirst(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, content := range []string{"first", "second"} {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: "OEBPS/dup.xhtml", Method: zip.Deflate})
		if err != nil {
			t.Fatalf("creating entry: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("writing entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}

	z, skipped, err := OpenSafeZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("OpenSafeZip: %v", err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}

	got, err := z.ReadFile("OEBPS/dup.xhtml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("ReadFile = %q, want the first entry %q", got, "first")
	}
}

func TestResolveRelative(t *testing.T) {
	tests := []struct {
		name    string
		base    string
		href    string
		want    string
		wantErr bool
	}{
		{"sibling", "OEBPS/content.opf", "ch01.xhtml", "OEBPS/ch01.xhtml", false},
		{"subdirectory", "OEBPS/content.opf", "text/ch01.xhtml", "OEBPS/text/ch01.xhtml", false},
		{"up one level", "OEBPS/text/nav.xhtml", "../images/cover.jpg", "OEBPS/images/cover.jpg", false},
		{"root-level base", "content.opf", "ch01.xhtml", "ch01.xhtml", false},
		{"fragment stripped", "OEBPS/nav.xhtml", "ch01.xhtml#section-2", "OEBPS/ch01.xhtml", false},
		{"escapes root", "OEBPS/content.opf", "../../etc/passwd", "", true},
		{"remote reference", "OEBPS/content.opf", "https://example.com/x.xhtml", "", true},
		{"protocol-relative url is remote", "OEBPS/x.opf", "//evil.com/x.xhtml", "", true},
		{"empty href", "OEBPS/content.opf", "", "", true},
		{"fragment only", "OEBPS/content.opf", "#top", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveRelative(tt.base, tt.href)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveRelative(%q, %q) = %q, want error", tt.base, tt.href, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveRelative(%q, %q) returned error: %v", tt.base, tt.href, err)
			}
			if got != tt.want {
				t.Fatalf("ResolveRelative(%q, %q) = %q, want %q", tt.base, tt.href, got, tt.want)
			}
		})
	}
}
