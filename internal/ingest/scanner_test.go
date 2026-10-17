// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// libraryFixture builds a library directory and returns its path.
func libraryFixture(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating %q: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %q: %v", full, err)
		}
	}
	return root
}

// collect runs a scan and returns the results sorted by relative path.
func collect(t *testing.T, s *Scanner) ([]ScannedFile, ScanStats) {
	t.Helper()

	var found []ScannedFile
	stats, err := s.Scan(t.Context(), func(f ScannedFile) error {
		found = append(found, f)
		return nil
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	sort.Slice(found, func(i, j int) bool { return found[i].RelPath < found[j].RelPath })
	return found, stats
}

func TestScanFindsBooksAndIgnoresEverythingElse(t *testing.T) {
	root := libraryFixture(t, map[string]string{
		"Knuth/taocp.epub":                  "epub bytes",
		"Knuth/taocp.pdf":                   "pdf bytes",
		"Knuth/cover.jpg":                   "not a book",
		"Knuth/notes.txt":                   "not a book",
		"Fiction/Le Guin/dispossessed.EPUB": "case insensitive extension",
		"readme.md":                         "not a book",
		".hidden/secret.epub":               "hidden directory",
		"node_modules/pkg/x.epub":           "junk directory",
	})

	found, stats := collect(t, &Scanner{Root: root})

	var rels []string
	for _, f := range found {
		rels = append(rels, filepath.ToSlash(f.RelPath))
	}
	want := []string{
		"Fiction/Le Guin/dispossessed.EPUB",
		"Knuth/taocp.epub",
		"Knuth/taocp.pdf",
	}
	if len(rels) != len(want) {
		t.Fatalf("found %v, want %v", rels, want)
	}
	for i := range want {
		if rels[i] != want[i] {
			t.Errorf("found[%d] = %q, want %q", i, rels[i], want[i])
		}
	}

	if stats.BooksFound != 3 {
		t.Errorf("BooksFound = %d, want 3", stats.BooksFound)
	}
	if stats.Errors != 0 {
		t.Errorf("Errors = %d, want 0", stats.Errors)
	}

	for _, f := range found {
		if f.ContentHash == "" {
			t.Errorf("%s has no content hash", f.RelPath)
		}
		if len(f.ContentHash) != 64 {
			t.Errorf("%s hash %q is not a SHA-256 hex digest", f.RelPath, f.ContentHash)
		}
	}
}

func TestScanDetectsFormat(t *testing.T) {
	root := libraryFixture(t, map[string]string{
		"a.epub": "x",
		"b.pdf":  "y",
		"c.EPUB": "z",
	})

	found, _ := collect(t, &Scanner{Root: root})

	want := map[string]domain.Format{
		"a.epub": domain.FormatEPUB,
		"b.pdf":  domain.FormatPDF,
		"c.EPUB": domain.FormatEPUB,
	}
	for _, f := range found {
		if got := want[f.RelPath]; got != f.Format {
			t.Errorf("%s: Format = %q, want %q", f.RelPath, f.Format, got)
		}
	}
}

// Content hash is file identity. The same bytes under a different name must
// produce the same hash, or every rename duplicates a book.
func TestHashIsContentNotPath(t *testing.T) {
	root := libraryFixture(t, map[string]string{
		"original.epub":          "identical bytes",
		"renamed/elsewhere.epub": "identical bytes",
		"different.epub":         "other bytes",
	})

	found, _ := collect(t, &Scanner{Root: root})

	byRel := make(map[string]ScannedFile, len(found))
	for _, f := range found {
		byRel[filepath.ToSlash(f.RelPath)] = f
	}

	a := byRel["original.epub"]
	b := byRel["renamed/elsewhere.epub"]
	c := byRel["different.epub"]

	if a.ContentHash == "" || b.ContentHash == "" {
		t.Fatalf("missing hashes: %+v %+v", a, b)
	}
	if a.ContentHash != b.ContentHash {
		t.Errorf("identical content hashed differently:\n  %s -> %s\n  %s -> %s",
			a.RelPath, a.ContentHash, b.RelPath, b.ContentHash)
	}
	if a.ContentHash == c.ContentHash {
		t.Error("different content produced the same hash")
	}
}

// One unreadable file in a large library must cost that one file.
func TestScanIsolatesPerFileErrors(t *testing.T) {
	root := libraryFixture(t, map[string]string{
		"good-one.epub": "content",
		"empty.epub":    "",
		"good-two.epub": "content two",
	})

	found, stats := collect(t, &Scanner{Root: root})

	if len(found) != 3 {
		t.Fatalf("found %d files, want 3 - the bad file must still be reported", len(found))
	}
	if stats.BooksFound != 2 {
		t.Errorf("BooksFound = %d, want 2", stats.BooksFound)
	}
	if stats.Errors != 1 {
		t.Errorf("Errors = %d, want 1", stats.Errors)
	}

	for _, f := range found {
		switch filepath.ToSlash(f.RelPath) {
		case "empty.epub":
			if f.Err == nil {
				t.Error("empty file reported no error")
			}
			if f.ContentHash != "" {
				t.Error("a failed file must not carry a hash")
			}
		default:
			if f.Err != nil {
				t.Errorf("%s: unexpected error %v", f.RelPath, f.Err)
			}
			if f.ContentHash == "" {
				t.Errorf("%s: missing hash", f.RelPath)
			}
		}
	}
}

// A symlink in a library directory can point anywhere on the filesystem.
// Following them by default would quietly break the read-only boundary.
func TestScanSkipsSymlinksByDefault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}

	outside := t.TempDir()
	secret := filepath.Join(outside, "outside.epub")
	if err := os.WriteFile(secret, []byte("content outside the library"), 0o644); err != nil {
		t.Fatalf("writing outside file: %v", err)
	}

	root := libraryFixture(t, map[string]string{"real.epub": "inside content"})
	if err := os.Symlink(secret, filepath.Join(root, "link.epub")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	found, stats := collect(t, &Scanner{Root: root})

	if len(found) != 1 || filepath.ToSlash(found[0].RelPath) != "real.epub" {
		t.Fatalf("found %+v, want only real.epub", found)
	}
	if stats.SymlinksSeen != 1 {
		t.Errorf("SymlinksSeen = %d, want 1", stats.SymlinksSeen)
	}
}

// Even with following enabled, a symlink out of the library must not be read:
// otherwise it is a way to make Lexarii read arbitrary host files.
func TestScanRefusesSymlinksOutOfLibraryEvenWhenFollowing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}

	outside := t.TempDir()
	secret := filepath.Join(outside, "outside.epub")
	if err := os.WriteFile(secret, []byte("content outside the library"), 0o644); err != nil {
		t.Fatalf("writing outside file: %v", err)
	}

	root := libraryFixture(t, map[string]string{"real.epub": "inside content"})
	if err := os.Symlink(secret, filepath.Join(root, "escape.epub")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	found, _ := collect(t, &Scanner{Root: root, FollowSymlinks: true})

	for _, f := range found {
		if f.Path == secret {
			t.Fatal("scanner read a file outside the library through a symlink")
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d files, want 1", len(found))
	}
}

// The promise that makes people willing to point this at a 20-year collection.
func TestScanNeverWritesToTheLibrary(t *testing.T) {
	root := libraryFixture(t, map[string]string{
		"a.epub":        "content a",
		"sub/b.pdf":     "content b",
		"sub/notes.txt": "content c",
	})

	before := snapshot(t, root)

	if _, err := (&Scanner{Root: root}).Scan(t.Context(), func(ScannedFile) error { return nil }); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	after := snapshot(t, root)
	if len(before) != len(after) {
		t.Fatalf("file count changed: %d -> %d", len(before), len(after))
	}
	for path, want := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("%s disappeared during the scan", path)
			continue
		}
		if got != want {
			t.Errorf("%s changed during the scan: %q -> %q", path, want, got)
		}
	}
}

// snapshot records every path under root with its contents.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()

	out := make(map[string]string)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting: %v", err)
	}
	return out
}

func TestScanIsCancellable(t *testing.T) {
	files := make(map[string]string, 200)
	for i := 0; i < 200; i++ {
		files[filepath.Join("books", string(rune('a'+i%26))+string(rune('a'+i/26))+".epub")] = "content"
	}
	root := libraryFixture(t, files)

	ctx, cancel := context.WithCancel(t.Context())
	count := 0
	_, err := (&Scanner{Root: root}).Scan(ctx, func(ScannedFile) error {
		count++
		if count == 5 {
			cancel()
		}
		return nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Scan error = %v, want context.Canceled", err)
	}
	if count > 20 {
		t.Errorf("scan emitted %d files after cancellation at 5; it should stop promptly", count)
	}
}

func TestScanRejectsBadRoot(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		s := &Scanner{Root: filepath.Join(t.TempDir(), "does-not-exist")}
		if _, err := s.Scan(t.Context(), func(ScannedFile) error { return nil }); err == nil {
			t.Fatal("Scan accepted a missing library directory")
		}
	})

	t.Run("root is a file", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing: %v", err)
		}
		s := &Scanner{Root: file}
		if _, err := s.Scan(t.Context(), func(ScannedFile) error { return nil }); err == nil {
			t.Fatal("Scan accepted a file as the library directory")
		}
	})
}

func TestEmitErrorStopsScan(t *testing.T) {
	root := libraryFixture(t, map[string]string{
		"a.epub": "1", "b.epub": "2", "c.epub": "3", "d.epub": "4",
	})

	sentinel := errors.New("stop here")
	seen := 0
	_, err := (&Scanner{Root: root}).Scan(t.Context(), func(ScannedFile) error {
		seen++
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("Scan error = %v, want the callback's error", err)
	}
	if seen != 1 {
		t.Errorf("callback ran %d times, want 1", seen)
	}
}

func TestFormatForPath(t *testing.T) {
	tests := map[string]struct {
		format domain.Format
		ok     bool
	}{
		"book.epub":        {domain.FormatEPUB, true},
		"BOOK.EPUB":        {domain.FormatEPUB, true},
		"paper.pdf":        {domain.FormatPDF, true},
		"a/b/c.PdF":        {domain.FormatPDF, true},
		"notes.txt":        {"", false},
		"noextension":      {"", false},
		"archive.epub.zip": {"", false},
		".epub":            {domain.FormatEPUB, true},
	}
	for path, want := range tests {
		t.Run(path, func(t *testing.T) {
			got, ok := FormatForPath(path)
			if ok != want.ok || got != want.format {
				t.Fatalf("FormatForPath(%q) = (%q, %v), want (%q, %v)", path, got, ok, want.format, want.ok)
			}
		})
	}
}

func TestWithin(t *testing.T) {
	root := filepath.FromSlash("/library")
	tests := map[string]bool{
		filepath.FromSlash("/library/book.epub"):        true,
		filepath.FromSlash("/library/sub/book.epub"):    true,
		filepath.FromSlash("/library"):                  true,
		filepath.FromSlash("/etc/passwd"):               false,
		filepath.FromSlash("/librarysibling/book.epub"): false,
		filepath.FromSlash("/library/../etc/passwd"):    false,
	}
	for path, want := range tests {
		t.Run(path, func(t *testing.T) {
			if got := within(root, path); got != want {
				t.Fatalf("within(%q, %q) = %v, want %v", root, path, got, want)
			}
		})
	}
}

// A format with no parser must still be seen. Silently ignoring a file the user
// owns gives them no way to tell whether Lexarii noticed it.
func TestScanSeesFormatsWithoutParsers(t *testing.T) {
	root := libraryFixture(t, map[string]string{
		"book.epub":  "epub",
		"book.pdf":   "pdf",
		"book.mobi":  "mobi",
		"book.azw3":  "azw3",
		"comic.cbz":  "cbz",
		"comic.cbr":  "cbr",
		"notes.txt":  "not a book",
		"cover.jpeg": "not a book",
	})

	found, stats := collect(t, &Scanner{Root: root})

	byFormat := make(map[domain.Format]int)
	for _, f := range found {
		byFormat[f.Format]++
	}
	for _, format := range []domain.Format{
		domain.FormatEPUB, domain.FormatPDF, domain.FormatMOBI,
		domain.FormatAZW3, domain.FormatCBZ, domain.FormatCBR,
	} {
		if byFormat[format] != 1 {
			t.Errorf("format %q: found %d files, want 1", format, byFormat[format])
		}
	}
	if stats.BooksFound != 6 {
		t.Errorf("BooksFound = %d, want 6", stats.BooksFound)
	}
	// Genuinely irrelevant files are still ignored.
	if stats.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 (the txt and the jpeg)", stats.Skipped)
	}
}
