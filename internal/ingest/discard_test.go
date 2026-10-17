// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Discard deletes copies in Lexarii's own upload directory and archive, and
// nothing anywhere else - above all nothing in the library directory, however
// the path is spelled.
func TestDiscardOnlyDeletesWhatLexariiStored(t *testing.T) {
	uploads, archive, covers, library := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	d := Discard{Uploads: uploads, Archive: archive, Covers: covers}
	write := func(path string) string {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing: %v", err)
		}
		return path
	}

	for _, tc := range []struct {
		name    string
		path    string
		removed bool
	}{
		{"an upload", write(filepath.Join(uploads, "books", "ab", "ab.epub")), true},
		{"an archive copy", write(filepath.Join(archive, "old.epub")), true},
		{"a library file", write(filepath.Join(library, "mine.epub")), false},
		{"a library file reached through the upload directory",
			filepath.Join(uploads, "..", filepath.Base(library), "mine.epub"), false},
		{"the upload directory itself", uploads, false},
		{"a cover by path", write(filepath.Join(covers, "c.jpg")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			removed, err := d.Remove(tc.path)
			if err != nil || removed != tc.removed {
				t.Fatalf("Remove = %v, %v; want %v", removed, err, tc.removed)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(library, "mine.epub")); err != nil {
		t.Fatalf("the library file is gone: %v", err)
	}

	if err := d.RemoveCover("../" + filepath.Base(library) + "/mine.epub"); err != nil {
		t.Fatalf("RemoveCover: %v", err)
	}
	if _, err := os.Stat(filepath.Join(library, "mine.epub")); err != nil {
		t.Fatalf("a cover name reached the library: %v", err)
	}
	if err := d.RemoveCover("c.jpg"); err != nil {
		t.Fatalf("RemoveCover: %v", err)
	}
	if _, err := os.Stat(filepath.Join(covers, "c.jpg")); !os.IsNotExist(err) {
		t.Errorf("the cover survived: %v", err)
	}
}

func TestDiscardCopiesAreContentAddressed(t *testing.T) {
	d := Discard{Uploads: "/data/uploads"}
	hash := strings.Repeat("0a", 32)
	copies := d.Copies(hash)
	if len(copies) != len(bookExtensions) {
		t.Fatalf("copies = %v, want one per format", copies)
	}
	for _, c := range copies {
		if !strings.HasPrefix(c, "/data/uploads/books/0a/"+hash+".") {
			t.Errorf("copy %s is not where an upload would be", c)
		}
	}
	if got := d.Copies("../../etc/passwd"); got != nil {
		t.Errorf("a malformed hash became paths: %v", got)
	}
}

// The archive keeps an upload's content-addressed name and suffixes a counter
// on a collision. Every one of those names is these bytes; nothing else is.
func TestDiscardCopiesIncludeArchivedCollisions(t *testing.T) {
	archive := t.TempDir()
	hash := strings.Repeat("0b", 32)
	other := strings.Repeat("0c", 32)
	for _, name := range []string{hash + ".epub", hash + "-2.epub", hash + "-13.pdf",
		hash + "-x.epub", hash + ".txt", other + ".epub", hash + "-.epub"} {
		if err := os.WriteFile(filepath.Join(archive, name), []byte("x"), 0o644); err != nil {
			t.Fatalf("writing: %v", err)
		}
	}
	d := Discard{Archive: archive}
	got := d.Copies(hash)
	want := []string{
		filepath.Join(archive, hash+"-13.pdf"),
		filepath.Join(archive, hash+"-2.epub"),
		filepath.Join(archive, hash+".epub"),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Copies = %v, want %v", got, want)
	}
}

// Even a configuration the server refuses - the library inside the upload
// directory - never gets a library file deleted or archived.
func TestTheLibraryIsNeverDeletedFromOrArchivedEvenWhenNested(t *testing.T) {
	uploads := t.TempDir()
	library := filepath.Join(uploads, "library")
	book := filepath.Join(library, "mine.epub")
	if err := os.MkdirAll(library, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(book, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if removed, err := (Discard{Uploads: uploads, Library: library}).Remove(book); err != nil || removed {
		t.Errorf("Remove = %v, %v; want the library file left", removed, err)
	}
	if to, err := (Archive{Root: t.TempDir(), Managed: uploads, Library: library}).Keep(book); err != nil || to != "" {
		t.Errorf("Keep = %q, %v; want the library file left", to, err)
	}
	if _, err := os.Stat(book); err != nil {
		t.Fatalf("the library file is gone: %v", err)
	}
}
