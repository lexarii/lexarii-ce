// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

func writeArchiveFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func skipArchiveTestUnlessPermissionsBite(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not restrict writes on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits")
	}
}

// The library is never moved even when it sits inside the managed directory.
// The configuration refuses that layout; Keep must not depend on it.
func TestArchiveLeavesALibraryNestedInManagedStorage(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "data")
	library := filepath.Join(managed, "library")
	book := filepath.Join(library, "theirs.epub")
	writeArchiveFixture(t, book, "somebody's own file")

	a := ingest.Archive{Root: filepath.Join(root, "archive"), Managed: managed, Library: library}
	at, err := a.Keep(book)
	if err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if at != "" {
		t.Errorf("a library file was archived to %q", at)
	}
	if _, err := os.Stat(book); err != nil {
		t.Errorf("the library file is gone: %v", err)
	}
}

// With nothing configured as managed, or no path at all, nothing is ours to
// move.
func TestArchiveWithNothingManagedMovesNothing(t *testing.T) {
	root := t.TempDir()
	uploads := filepath.Join(root, "uploads")
	book := filepath.Join(uploads, "x.epub")
	writeArchiveFixture(t, book, "x")

	tests := []struct {
		name string
		a    ingest.Archive
		path string
	}{
		{"no managed directory", ingest.Archive{Root: filepath.Join(root, "archive")}, book},
		{"empty path", ingest.Archive{Root: filepath.Join(root, "archive"), Managed: uploads}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, err := tt.a.Keep(tt.path)
			if err != nil || at != "" {
				t.Errorf("Keep = %q, %v; want nothing moved", at, err)
			}
			if _, err := os.Stat(book); err != nil {
				t.Errorf("the file was moved: %v", err)
			}
		})
	}
}

// An archive directory that cannot be created fails the archiving and leaves
// the upload where it was: losing the last copy is the one unacceptable
// outcome.
func TestArchiveFailsWithoutLosingTheFileWhenTheRootCannotBeMade(t *testing.T) {
	root := t.TempDir()
	uploads := filepath.Join(root, "uploads")
	book := filepath.Join(uploads, "abc.epub")
	writeArchiveFixture(t, book, "the only copy")
	blocker := filepath.Join(root, "archive")
	writeArchiveFixture(t, blocker, "a file where the directory should be")

	a := ingest.Archive{Root: blocker, Managed: uploads}
	at, err := a.Keep(book)
	if err == nil {
		t.Fatalf("Keep succeeded into %q with no archive directory", at)
	}
	if got, err := os.ReadFile(book); err != nil || string(got) != "the only copy" {
		t.Errorf("the upload was disturbed: %q, %v", got, err)
	}
}

// When the move itself is refused, the file is copied instead. If the
// original then cannot be removed, the copy still stands and is reported:
// untidy, but nothing is lost.
func TestArchiveCopiesWhenTheMoveIsRefused(t *testing.T) {
	skipArchiveTestUnlessPermissionsBite(t)

	root := t.TempDir()
	uploads := filepath.Join(root, "uploads")
	book := filepath.Join(uploads, "abc.epub")
	writeArchiveFixture(t, book, "the bytes")
	archive := filepath.Join(root, "archive")

	// A directory nobody may remove entries from: rename fails, and so does
	// deleting the original after the copy.
	if err := os.Chmod(uploads, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(uploads, 0o755) })

	a := ingest.Archive{Root: archive, Managed: uploads}
	at, err := a.Keep(book)
	if err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if at != filepath.Join(archive, "abc.epub") {
		t.Errorf("archived to %q, want %q", at, filepath.Join(archive, "abc.epub"))
	}
	if got, err := os.ReadFile(at); err != nil || string(got) != "the bytes" {
		t.Errorf("the archived copy reads %q, %v", got, err)
	}
	if _, err := os.Stat(book); err != nil {
		t.Errorf("the original could not be removed and should still be there: %v", err)
	}
}

// If neither moving nor copying works, Keep says so and the upload stays.
func TestArchiveReportsAFailedCopy(t *testing.T) {
	skipArchiveTestUnlessPermissionsBite(t)

	root := t.TempDir()
	uploads := filepath.Join(root, "uploads")
	book := filepath.Join(uploads, "abc.epub")
	writeArchiveFixture(t, book, "the bytes")
	archive := filepath.Join(root, "archive")
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(archive, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(archive, 0o755) })

	a := ingest.Archive{Root: archive, Managed: uploads}
	if at, err := a.Keep(book); err == nil {
		t.Fatalf("Keep reported success at %q into a read-only archive", at)
	}
	if got, err := os.ReadFile(book); err != nil || string(got) != "the bytes" {
		t.Errorf("the upload was disturbed: %q, %v", got, err)
	}
}
