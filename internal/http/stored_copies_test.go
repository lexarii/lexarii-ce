// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/ingest"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// storedCopiesFixture is a data directory with one file's bytes stored the
// ways Lexarii stores them - an upload, two archive copies (one after a name
// collision), a cover - and a copy of the same bytes in the library
// directory, which is never Lexarii's to delete.
type storedCopiesFixture struct {
	d                         ingest.Discard
	hash                      string
	upload, archive, archive2 string
	cover, library            string
}

func newStoredCopiesFixture(t *testing.T) storedCopiesFixture {
	t.Helper()
	root := t.TempDir()
	d := ingest.Discard{
		Uploads: filepath.Join(root, "uploads"),
		Archive: filepath.Join(root, "archive"),
		Covers:  filepath.Join(root, "covers"),
		Library: filepath.Join(root, "library"),
	}
	hash := strings.Repeat("ab", 32)
	uploads := &ingest.UploadStore{Root: d.Uploads}
	f := storedCopiesFixture{
		d:        d,
		hash:     hash,
		upload:   uploads.PathFor(hash, domain.FormatEPUB),
		archive:  filepath.Join(d.Archive, hash+".epub"),
		archive2: filepath.Join(d.Archive, hash+"-1.epub"),
		cover:    filepath.Join(d.Covers, "c.jpg"),
		library:  filepath.Join(d.Library, "mine.epub"),
	}
	for _, path := range []string{f.upload, f.archive, f.archive2, f.cover, f.library} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing: %v", err)
		}
	}
	return f
}

// left names what is still on disk: "upload", or the base name of the rest.
func (f storedCopiesFixture) left(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, path := range []string{f.upload, f.archive, f.archive2, f.cover, f.library} {
		if _, err := os.Stat(path); err == nil {
			name := filepath.Base(path)
			if path == f.upload {
				name = "upload"
			}
			out[name] = true
		}
	}
	return out
}

// The exported seam deletes what an erasure's payment and a removal delete,
// by the same rules, one file at a time or in a pass: every stored copy and
// the cover, never the library directory's copy - even when a record names
// it - and with SkipUploads nothing in the upload directory.
func TestDeletingStoredCopies(t *testing.T) {
	for _, tc := range []struct {
		name        string
		skipUploads bool
		removed     int
		left        []string
	}{
		{"everything Lexarii stored", false, 3, []string{"mine.epub"}},
		{"sparing an upload on its way", true, 2, []string{"mine.epub", "upload"}},
	} {
		for _, way := range []struct {
			name   string
			delete func(ingest.Discard, postgres.StoredCopies) (int, error)
		}{
			{"one file", func(d ingest.Discard, c postgres.StoredCopies) (int, error) {
				return DeleteStoredCopies(d, c)
			}},
			{"a pass", func(d ingest.Discard, c postgres.StoredCopies) (int, error) {
				return StoredCopyDeleter(d)(c)
			}},
		} {
			t.Run(tc.name+", "+way.name, func(t *testing.T) {
				f := newStoredCopiesFixture(t)
				n, err := way.delete(f.d, postgres.StoredCopies{
					ContentHash: f.hash,
					// The record's paths: the archive copy again (deleted
					// once) and a library-directory copy (never deleted).
					Paths:       []string{f.archive, f.library},
					Covers:      []string{"c.jpg"},
					SkipUploads: tc.skipUploads,
				})
				if err != nil || n != tc.removed {
					t.Errorf("deleted %d (%v), want %d", n, err, tc.removed)
				}
				left := f.left(t)
				if len(left) != len(tc.left) {
					t.Errorf("left %v, want only %v", left, tc.left)
				}
				for _, name := range tc.left {
					if !left[name] {
						t.Errorf("%s was deleted; left %v", name, left)
					}
				}
			})
		}
	}
}

// A pass lists the archive once, when it is made: a copy that arrives in the
// archive afterwards is not one it finds by hash. One deleter per pass.
func TestAStoredCopyPassListsTheArchiveOnce(t *testing.T) {
	f := newStoredCopiesFixture(t)
	if err := os.Remove(f.archive2); err != nil {
		t.Fatalf("removing: %v", err)
	}
	deleteCopies := StoredCopyDeleter(f.d)
	if err := os.WriteFile(f.archive2, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if _, err := deleteCopies(postgres.StoredCopies{ContentHash: f.hash}); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if left := f.left(t); !left[filepath.Base(f.archive2)] || left[filepath.Base(f.archive)] {
		t.Errorf("left %v; want the copy listed at the start gone and the later one kept", left)
	}
	if _, err := DeleteStoredCopies(f.d, postgres.StoredCopies{ContentHash: f.hash}); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if left := f.left(t); left[filepath.Base(f.archive2)] {
		t.Errorf("one file at a time lists the archive afresh, and left %v", left)
	}
}
