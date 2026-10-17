// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A name that is all extension cannot be shortened without losing the part
// that says what it is, so it is refused rather than mangled.
func TestSafeFilenameRefusesANameThatIsAllExtension(t *testing.T) {
	name := "a." + strings.Repeat("x", 300)
	if got, err := SafeFilename(name); !errors.Is(err, ErrBadFilename) {
		t.Errorf("SafeFilename(%d-byte extension) = %q, %v; want ErrBadFilename", len(name), got, err)
	}
}

// When the managed directory cannot be written, the upload fails and leaves
// nothing behind - least of all a partial file a later scan would mistake for
// a book.
func TestStoreFailsCleanlyWhenStorageIsBlocked(t *testing.T) {
	t.Run("the upload root is a file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "uploads")
		if err := os.WriteFile(root, []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &UploadStore{Root: root}
		if got, err := s.Store(t.Context(), "a.epub", strings.NewReader("bytes")); err == nil {
			t.Fatalf("Store succeeded at %q", got.Path)
		}
	})

	t.Run("the content-addressed directory is blocked", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "books"), []byte("in the way"), 0o644); err != nil {
			t.Fatal(err)
		}
		s := &UploadStore{Root: root}
		got, err := s.Store(t.Context(), "a.epub", strings.NewReader("bytes"))
		if err == nil {
			t.Fatalf("Store succeeded at %q", got.Path)
		}
		if _, statErr := os.Stat(s.PathFor(uploadStorageHash("bytes"), "epub")); statErr == nil {
			t.Error("a stored file exists after a failed store")
		}
		leftovers, readErr := os.ReadDir(filepath.Join(root, "incoming"))
		if readErr != nil {
			t.Fatalf("reading incoming: %v", readErr)
		}
		if len(leftovers) != 0 {
			t.Errorf("a failed store left %d temporary files behind", len(leftovers))
		}
	})
}

func uploadStorageHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
