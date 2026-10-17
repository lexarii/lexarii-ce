// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest_test

import (
	"os"
	"path/filepath"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
)

// The first invariant: the library directory is never written to. That includes
// not tidying it up when an unrelated account is deleted - the file there is
// somebody's own copy of their own book, and Lexarii moving it would take it
// away from them.
func TestArchiveNeverTouchesTheLibraryDirectory(t *testing.T) {
	root := t.TempDir()
	library := filepath.Join(root, "library")
	uploads := filepath.Join(root, "data", "uploads")
	archive := filepath.Join(root, "data", "archive")

	if err := os.MkdirAll(library, 0o755); err != nil {
		t.Fatal(err)
	}
	book := filepath.Join(library, "somebodys-own.epub")
	if err := os.WriteFile(book, []byte("a book"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := ingest.Archive{Root: archive, Managed: uploads}
	at, err := a.Keep(book)
	if err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if at != "" {
		t.Errorf("a library file reports having been moved to %q", at)
	}
	if _, err := os.Stat(book); err != nil {
		t.Errorf("the library file was moved or removed: %v", err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Error("an archive directory was created for a file that was never ours")
	}
}

// An upload has no other home: Lexarii wrote it, and if it is not kept here
// it is not kept anywhere.
func TestArchiveKeepsUploads(t *testing.T) {
	root := t.TempDir()
	uploads := filepath.Join(root, "uploads", "books", "ab")
	archive := filepath.Join(root, "archive")
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	book := filepath.Join(uploads, "abcdef.epub")
	if err := os.WriteFile(book, []byte("an uploaded book"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := ingest.Archive{Root: archive, Managed: filepath.Join(root, "uploads")}
	at, err := a.Keep(book)
	if err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if at == "" {
		t.Fatal("an uploaded book was not archived")
	}
	kept, err := os.ReadFile(at)
	if err != nil {
		t.Fatalf("the archived file is not readable: %v", err)
	}
	if string(kept) != "an uploaded book" {
		t.Errorf("the archived file says %q", kept)
	}
	if _, err := os.Stat(book); !os.IsNotExist(err) {
		t.Error("the upload is still in the live directory as well")
	}
}

// Two people uploading the same book must not end with one archive file.
func TestArchiveDoesNotOverwriteAnEarlierBook(t *testing.T) {
	root := t.TempDir()
	uploads := filepath.Join(root, "uploads")
	archive := filepath.Join(root, "archive")
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	a := ingest.Archive{Root: archive, Managed: uploads}

	paths := make([]string, 0, 2)
	for _, content := range []string{"first", "second"} {
		book := filepath.Join(uploads, "same-name.epub")
		if err := os.WriteFile(book, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		at, err := a.Keep(book)
		if err != nil {
			t.Fatalf("Keep: %v", err)
		}
		paths = append(paths, at)
	}

	if paths[0] == paths[1] {
		t.Fatalf("both books were archived to %q", paths[0])
	}
	for i, want := range []string{"first", "second"} {
		got, err := os.ReadFile(paths[i])
		if err != nil {
			t.Fatalf("reading %s: %v", paths[i], err)
		}
		if string(got) != want {
			t.Errorf("archive %d holds %q, want %q", i, got, want)
		}
	}
}

// A directory that merely starts with the managed path is not the managed path.
func TestArchiveDoesNotClaimANeighbouringDirectory(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "uploads")
	neighbour := filepath.Join(root, "uploads-elsewhere")
	if err := os.MkdirAll(neighbour, 0o755); err != nil {
		t.Fatal(err)
	}
	book := filepath.Join(neighbour, "book.epub")
	if err := os.WriteFile(book, []byte("not ours"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := ingest.Archive{Root: filepath.Join(root, "archive"), Managed: managed}
	at, err := a.Keep(book)
	if err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if at != "" {
		t.Errorf("a neighbouring directory was treated as managed storage: %q", at)
	}
	if _, err := os.Stat(book); err != nil {
		t.Errorf("the neighbour's file was moved: %v", err)
	}
}
