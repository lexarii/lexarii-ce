// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// skipUnlessPermissionsBite skips where chmod cannot make a file unreadable:
// root reads everything, and Windows has no mode bits to speak of.
func skipUnlessPermissionsBite(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not restrict reads on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
}

// lockForTest removes every permission from path and restores them when the
// test ends, so TempDir cleanup can still remove it.
func lockForTest(t *testing.T, path string, restore os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, restore) })
}

func byRelPath(found []ScannedFile) map[string]ScannedFile {
	out := make(map[string]ScannedFile, len(found))
	for _, f := range found {
		out[filepath.ToSlash(f.RelPath)] = f
	}
	return out
}

// A permissions problem on one folder must not cost the rest of the library.
func TestScanSkipsAnUnreadableDirectory(t *testing.T) {
	skipUnlessPermissionsBite(t)

	root := libraryFixture(t, map[string]string{
		"a.epub":        "first",
		"locked/b.epub": "hidden behind permissions",
		"z/c.pdf":       "third",
	})
	lockForTest(t, filepath.Join(root, "locked"), 0o755)

	found, stats := collect(t, &Scanner{Root: root})

	got := byRelPath(found)
	if len(got) != 2 || got["a.epub"].ContentHash == "" || got["z/c.pdf"].ContentHash == "" {
		t.Fatalf("found %v, want the two readable books hashed", found)
	}
	if stats.Errors != 1 {
		t.Errorf("Errors = %d, want 1 for the locked directory", stats.Errors)
	}
	if stats.BooksFound != 2 {
		t.Errorf("BooksFound = %d, want 2", stats.BooksFound)
	}
}

// A book that cannot be opened is reported against that book - visible in
// the UI with its reason - and the scan goes on.
func TestScanReportsAnUnreadableFile(t *testing.T) {
	skipUnlessPermissionsBite(t)

	root := libraryFixture(t, map[string]string{
		"a.epub":      "readable",
		"secret.epub": "not readable",
	})
	lockForTest(t, filepath.Join(root, "secret.epub"), 0o644)

	found, stats := collect(t, &Scanner{Root: root})

	got := byRelPath(found)
	bad, ok := got["secret.epub"]
	if !ok {
		t.Fatalf("the unreadable file was not reported at all: %v", found)
	}
	if !errors.Is(bad.Err, fs.ErrPermission) {
		t.Errorf("Err = %v, want a permission error", bad.Err)
	}
	if bad.ContentHash != "" {
		t.Error("a file that could not be read carries a hash")
	}
	if bad.SizeBytes != int64(len("not readable")) {
		t.Errorf("SizeBytes = %d; the stat succeeded and should be recorded", bad.SizeBytes)
	}
	if got["a.epub"].Err != nil || got["a.epub"].ContentHash == "" {
		t.Errorf("the readable file was affected: %+v", got["a.epub"])
	}
	if stats.Errors != 1 || stats.BooksFound != 1 {
		t.Errorf("stats = %+v, want 1 error and 1 book", stats)
	}
}

// A 4 GiB "epub" is something other than a book, and hashing it would hold up
// the scan for nothing. A sparse file makes the size real without the bytes.
func TestScanRefusesAFileOverTheSizeCap(t *testing.T) {
	root := libraryFixture(t, map[string]string{"normal.epub": "content"})
	huge := filepath.Join(root, "huge.pdf")
	f, err := os.Create(huge)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	if err := f.Truncate(MaxFileSize + 1); err != nil {
		_ = f.Close()
		t.Skipf("this filesystem cannot make a sparse file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	found, stats := collect(t, &Scanner{Root: root})

	got := byRelPath(found)
	big := got["huge.pdf"]
	if big.Err == nil {
		t.Fatal("an over-sized file was accepted")
	}
	if big.ContentHash != "" {
		t.Error("an over-sized file was hashed")
	}
	if big.SizeBytes != MaxFileSize+1 {
		t.Errorf("SizeBytes = %d, want the real size recorded", big.SizeBytes)
	}
	if stats.BytesHashed != int64(len("content")) {
		t.Errorf("BytesHashed = %d, want only the normal file", stats.BytesHashed)
	}
}

// A file deleted between the directory listing and its stat - a sync client
// tidying up mid-scan - is an error against that file, not a failed scan.
func TestScanSurvivesAFileVanishingMidWalk(t *testing.T) {
	root := libraryFixture(t, map[string]string{
		"a.epub": "first",
		"b.epub": "about to vanish",
		"c.epub": "third",
	})

	var found []ScannedFile
	_, err := (&Scanner{Root: root}).Scan(t.Context(), func(f ScannedFile) error {
		found = append(found, f)
		if filepath.Base(f.Path) == "a.epub" {
			// The walk has already listed the directory; this is the race.
			if err := os.Remove(filepath.Join(root, "b.epub")); err != nil {
				t.Fatalf("removing: %v", err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	got := byRelPath(found)
	if len(got) != 3 {
		t.Fatalf("found %d files, want all three reported", len(got))
	}
	if !errors.Is(got["b.epub"].Err, fs.ErrNotExist) {
		t.Errorf("vanished file Err = %v, want ErrNotExist", got["b.epub"].Err)
	}
	if got["c.epub"].Err != nil || got["c.epub"].ContentHash == "" {
		t.Errorf("the file after the vanished one was affected: %+v", got["c.epub"])
	}
}

// With following enabled, a link that stays inside the library is read
// through to its target - and is recognised as the same book by its hash.
func TestScanFollowsASymlinkInsideTheLibrary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}

	root := libraryFixture(t, map[string]string{"shelf/real.epub": "the same bytes"})
	if err := os.Symlink(filepath.Join(root, "shelf", "real.epub"), filepath.Join(root, "alias.epub")); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "gone.epub"), filepath.Join(root, "dangling.epub")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	found, stats := collect(t, &Scanner{Root: root, FollowSymlinks: true})

	if len(found) != 2 {
		t.Fatalf("found %d files, want the real file and the alias: %+v", len(found), found)
	}
	if found[0].ContentHash == "" || found[0].ContentHash != found[1].ContentHash {
		t.Errorf("hashes %q and %q: the alias should be identified as the same book",
			found[0].ContentHash, found[1].ContentHash)
	}
	// The link is resolved, so what is read is the file inside the library.
	for _, f := range found {
		if filepath.Base(f.Path) != "real.epub" {
			t.Errorf("Path = %s, want the resolved target", f.Path)
		}
	}
	if stats.SymlinksSeen != 2 {
		t.Errorf("SymlinksSeen = %d, want 2", stats.SymlinksSeen)
	}
	if stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 for the dangling link", stats.Skipped)
	}
}

// Hidden and tool directories are metadata, not shelves.
func TestScanSkipsMetadataDirectories(t *testing.T) {
	root := libraryFixture(t, map[string]string{
		"keep/a.epub":         "a",
		".git/objects/x.epub": "x",
		".hidden/y.epub":      "y",
		"__MACOSX/z.epub":     "z",
		".stversions/w.epub":  "w",
		"node_modules/v.epub": "v",
	})

	found, _ := collect(t, &Scanner{Root: root})
	if len(found) != 1 || filepath.ToSlash(found[0].RelPath) != "keep/a.epub" {
		t.Errorf("found %v, want only keep/a.epub", found)
	}
}

// Content hash is identity: the same bytes under any name are the same book,
// and the hash is plain SHA-256 so it can be checked with any tool.
func TestHashFile(t *testing.T) {
	dir := t.TempDir()
	one := filepath.Join(dir, "one.epub")
	two := filepath.Join(dir, "renamed copy.pdf")
	for _, p := range []string{one, two} {
		if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const sha256OfABC = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	for _, p := range []string{one, two} {
		got, err := HashFile(t.Context(), p)
		if err != nil {
			t.Fatalf("HashFile(%s): %v", p, err)
		}
		if got != sha256OfABC {
			t.Errorf("HashFile(%s) = %s, want %s", filepath.Base(p), got, sha256OfABC)
		}
	}

	t.Run("missing file", func(t *testing.T) {
		if _, err := HashFile(t.Context(), filepath.Join(dir, "absent.epub")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("error = %v, want ErrNotExist", err)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := HashFile(ctx, one); !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	})
}
