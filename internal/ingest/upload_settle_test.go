// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// filesUnder lists every regular file below a directory.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// A duplicate upload keeps its own bytes aside until it is recorded, because
// the stored copy it matched may be erased with the record it belonged to
// before then. Settled, the copy aside fills in for a stored copy that went,
// and is removed otherwise; refused, it is only removed. Either way nothing
// is left behind.
func TestSettlingADuplicateUpload(t *testing.T) {
	content := []byte("identical bytes")

	for _, tc := range []struct {
		name      string
		erased    bool // the stored copy went before the upload was recorded
		keep      bool
		wantFiles int
	}{
		{"recorded, the stored copy still there", false, true, 1},
		{"recorded, the stored copy erased meanwhile", true, true, 1},
		{"refused", false, false, 1},
		{"refused, the stored copy gone", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			first, err := s.Store(t.Context(), "book.epub", bytes.NewReader(content))
			if err != nil {
				t.Fatalf("first: %v", err)
			}
			if err := s.Settle(first, true); err != nil {
				t.Fatalf("settling the first: %v", err)
			}
			second, err := s.Store(t.Context(), "again.epub", bytes.NewReader(content))
			if err != nil || !second.Duplicate {
				t.Fatalf("second = %+v, %v; want a duplicate", second, err)
			}
			if tc.erased {
				if err := os.Remove(first.Path); err != nil {
					t.Fatalf("erasing: %v", err)
				}
			}

			if err := s.Settle(second, tc.keep); err != nil {
				t.Fatalf("Settle: %v", err)
			}
			if got := filesUnder(t, s.Root); len(got) != tc.wantFiles {
				t.Fatalf("files after settling = %v, want %d", got, tc.wantFiles)
			}
			if tc.keep {
				got, err := os.ReadFile(second.Path)
				if err != nil || !bytes.Equal(got, content) {
					t.Errorf("stored copy = %q, %v; want the upload's bytes", got, err)
				}
			}
			// Settling twice is harmless.
			if err := s.Settle(second, tc.keep); err != nil {
				t.Errorf("Settle again: %v", err)
			}
		})
	}

	// A new copy is kept aside too: an erasure of a record of the same
	// bytes - one whose own copy is in the library directory - deletes every
	// stored copy it finds, this one included, before the upload is recorded.
	t.Run("a new upload erased before it was recorded", func(t *testing.T) {
		s := newStore(t)
		first, err := s.Store(t.Context(), "book.epub", bytes.NewReader(content))
		if err != nil || first.Duplicate {
			t.Fatalf("Store = %+v, %v", first, err)
		}
		if err := os.Remove(first.Path); err != nil {
			t.Fatalf("erasing: %v", err)
		}
		if err := s.Settle(first, true); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		got, err := os.ReadFile(first.Path)
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("stored copy = %q, %v; want it back", got, err)
		}
		if files := filesUnder(t, s.Root); len(files) != 1 {
			t.Errorf("files = %v, want just the stored copy", files)
		}
	})

	t.Run("a new upload settled untouched", func(t *testing.T) {
		s := newStore(t)
		first, err := s.Store(t.Context(), "book.epub", bytes.NewReader(content))
		if err != nil {
			t.Fatalf("Store: %v", err)
		}
		if err := s.Settle(first, true); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if files := filesUnder(t, s.Root); len(files) != 1 || files[0] != first.Path {
			t.Errorf("files = %v, want just the stored copy", files)
		}
	})

	// Where hard links do not work the copy aside is a copy, and does the
	// same job.
	t.Run("a filesystem without hard links", func(t *testing.T) {
		linkFile = func(string, string) error { return os.ErrPermission }
		t.Cleanup(func() { linkFile = os.Link })
		s := newStore(t)
		first, err := s.Store(t.Context(), "book.epub", bytes.NewReader(content))
		if err != nil || first.Duplicate {
			t.Fatalf("Store = %+v, %v", first, err)
		}
		if files := filesUnder(t, s.Root); len(files) != 2 {
			t.Fatalf("files = %v, want the stored copy and one aside", files)
		}
		if err := os.Remove(first.Path); err != nil {
			t.Fatalf("erasing: %v", err)
		}
		if err := s.Settle(first, true); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		got, err := os.ReadFile(first.Path)
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("stored copy = %q, %v; want it back", got, err)
		}
		if files := filesUnder(t, s.Root); len(files) != 1 {
			t.Errorf("files = %v, want just the stored copy", files)
		}
	})
}

// With nothing kept aside, a stored copy gone before the upload is recorded
// fails the recording rather than leaving a book with nothing to open.
func TestSettlingWithNothingAside(t *testing.T) {
	linkFile = func(string, string) error { return os.ErrPermission }
	t.Cleanup(func() { linkFile = os.Link })
	s := newStore(t)
	f, err := s.Store(t.Context(), "book.epub", bytes.NewReader([]byte("some bytes")))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	_ = os.Remove(f.spare)
	f.spare = "" // as when the copy aside could not be made
	if err := s.Settle(f, true); err != nil {
		t.Errorf("Settle with the stored copy in place: %v", err)
	}
	if err := os.Remove(f.Path); err != nil {
		t.Fatal(err)
	}
	if err := s.Settle(f, true); err == nil {
		t.Error("Settle accepted an upload whose only copy is gone")
	}
	if err := s.Settle(f, false); err != nil {
		t.Errorf("settling a refused upload: %v", err)
	}
}

func TestSamePath(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.epub")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{
		file,
		filepath.Join(dir, "sub", "..", "a.epub"),
		filepath.Join(link, "a.epub"),
	} {
		if !SamePath(file, other) {
			t.Errorf("SamePath(%q, %q) = false", file, other)
		}
	}
	if SamePath(file, filepath.Join(dir, "b.epub")) || SamePath(file, "") {
		t.Error("different files were the same")
	}
}
