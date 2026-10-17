// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

func TestSafeFilenameRejectsHostileNames(t *testing.T) {
	bad := []string{
		"", "   ", ".", "..", "/", `\`,
		"/etc/passwd/", // nothing left after stripping directories
		"\x00",
		"\x01\x02\x03",
	}
	for _, name := range bad {
		t.Run(name, func(t *testing.T) {
			if got, err := SafeFilename(name); err == nil {
				t.Fatalf("SafeFilename(%q) = %q, want an error", name, got)
			} else if !errors.Is(err, ErrBadFilename) {
				t.Fatalf("error = %v, want ErrBadFilename", err)
			}
		})
	}
}

// Storage is content-addressed, so a filename never becomes a path - but it is
// still attacker-controlled text that reaches the database and the UI.
func TestSafeFilenameStripsDirectoryComponents(t *testing.T) {
	tests := map[string]string{
		"book.epub":                      "book.epub",
		"/etc/passwd":                    "passwd",
		"../../../etc/shadow":            "shadow",
		`C:\Windows\System32\config.sys`: "config.sys",
		"subdir/book.epub":               "book.epub",
		`folder\book.epub`:               "book.epub",
		"Gödel, Escher, Bach.epub":       "Gödel, Escher, Bach.epub",
		"book\x01name.epub":              "bookname.epub",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			got, err := SafeFilename(in)
			if err != nil {
				t.Fatalf("SafeFilename(%q) returned error: %v", in, err)
			}
			if got != want {
				t.Fatalf("SafeFilename(%q) = %q, want %q", in, got, want)
			}
			if strings.ContainsAny(got, `/\`) {
				t.Fatalf("SafeFilename(%q) = %q still contains a separator", in, got)
			}
		})
	}
}

// The extension decides the format, so truncation must not eat it.
func TestSafeFilenameKeepsExtensionWhenTruncating(t *testing.T) {
	long := strings.Repeat("a", 400) + ".epub"
	got, err := SafeFilename(long)
	if err != nil {
		t.Fatalf("SafeFilename: %v", err)
	}
	if len(got) > 255 {
		t.Errorf("length = %d, want <= 255", len(got))
	}
	if !strings.HasSuffix(got, ".epub") {
		t.Fatalf("truncation lost the extension: %q", got)
	}
	if _, ok := FormatForPath(got); !ok {
		t.Error("the truncated name no longer identifies a format")
	}
}

func newStore(t *testing.T) *UploadStore {
	t.Helper()
	return &UploadStore{Root: t.TempDir()}
}

func TestStoreWritesFileAndReportsHash(t *testing.T) {
	s := newStore(t)
	content := []byte("a small but valid-enough book payload")

	got, err := s.Store(t.Context(), "algebraessentials.epub", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Store: %v", err)
	}

	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])
	if got.ContentHash != want {
		t.Errorf("ContentHash = %q, want %q", got.ContentHash, want)
	}
	if got.Format != domain.FormatEPUB {
		t.Errorf("Format = %q", got.Format)
	}
	if got.SizeBytes != int64(len(content)) {
		t.Errorf("SizeBytes = %d, want %d", got.SizeBytes, len(content))
	}
	if got.Duplicate {
		t.Error("a first upload was reported as a duplicate")
	}
	// The original name is kept: same-book merging derives its stem from it, and
	// content-addressed storage has thrown the name away.
	if got.OriginalFilename != "algebraessentials.epub" {
		t.Errorf("OriginalFilename = %q", got.OriginalFilename)
	}

	stored, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("reading the stored file: %v", err)
	}
	if !bytes.Equal(stored, content) {
		t.Error("the stored bytes differ from what was uploaded")
	}
}

// The property the whole recovery story rests on: re-uploading a batch after a
// failure must be cheap and must not duplicate anything.
func TestStoreDeduplicatesByContent(t *testing.T) {
	s := newStore(t)
	content := []byte("identical bytes")

	first, err := s.Store(t.Context(), "book.epub", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if first.Duplicate {
		t.Fatal("the first upload was reported as a duplicate")
	}

	// Same bytes, different name - as happens when a user re-uploads from a
	// differently-organised folder.
	second, err := s.Store(t.Context(), "renamed-copy.epub", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !second.Duplicate {
		t.Error("re-uploading identical content was not recognised as a duplicate")
	}
	if second.ContentHash != first.ContentHash || second.Path != first.Path {
		t.Error("the duplicate resolved to a different location")
	}

	// And nothing extra was written.
	var files int
	_ = filepath.Walk(filepath.Join(s.Root, "books"), func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			files++
		}
		return nil
	})
	if files != 1 {
		t.Errorf("%d files stored after uploading the same content twice, want 1", files)
	}
}

func TestStoreDistinguishesDifferentContent(t *testing.T) {
	s := newStore(t)

	a, err := s.Store(t.Context(), "one.epub", strings.NewReader("first book"))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	b, err := s.Store(t.Context(), "two.epub", strings.NewReader("second book"))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if a.ContentHash == b.ContentHash || a.Path == b.Path {
		t.Fatal("different content collided")
	}
}

func TestStoreRejectsBadUploads(t *testing.T) {
	s := &UploadStore{Root: t.TempDir(), MaxFileSize: 64}

	tests := []struct {
		name     string
		filename string
		body     string
		wantErr  error
	}{
		{"empty file", "book.epub", "", ErrEmptyUpload},
		{"over the size limit", "book.epub", strings.Repeat("x", 200), ErrUploadTooLarge},
		{"not a book", "notes.txt", "content", ErrUnsupportedUpload},
		{"no extension", "book", "content", ErrUnsupportedUpload},
		{"hostile name", "../../etc/passwd", "content", ErrUnsupportedUpload},
		{"empty name", "", "content", ErrBadFilename},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.Store(t.Context(), tt.filename, strings.NewReader(tt.body))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// An interrupted bulk upload is the case this is designed around, so a failed
// upload must leave nothing a later scan could mistake for a real book.
func TestFailedUploadLeavesNoPartialFile(t *testing.T) {
	s := &UploadStore{Root: t.TempDir(), MaxFileSize: 32}

	if _, err := s.Store(t.Context(), "toobig.epub", strings.NewReader(strings.Repeat("x", 500))); err == nil {
		t.Fatal("an oversized upload was accepted")
	}

	var leftovers []string
	_ = filepath.Walk(s.Root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if len(leftovers) != 0 {
		t.Fatalf("a failed upload left files behind: %v", leftovers)
	}
}

// Cancelling mid-upload must also leave nothing behind.
func TestCancelledUploadLeavesNoPartialFile(t *testing.T) {
	s := newStore(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := s.Store(ctx, "book.epub", strings.NewReader("some content")); err == nil {
		t.Fatal("a cancelled upload was accepted")
	}

	var leftovers []string
	_ = filepath.Walk(s.Root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			leftovers = append(leftovers, path)
		}
		return nil
	})
	if len(leftovers) != 0 {
		t.Fatalf("a cancelled upload left files behind: %v", leftovers)
	}
}

// Every format the scanner recognises must be uploadable, including the ones
// with no parser yet - they are stored and labelled unsupported, not refused.
func TestStoreAcceptsEveryKnownFormat(t *testing.T) {
	s := newStore(t)

	for name, want := range map[string]domain.Format{
		"a.epub": domain.FormatEPUB,
		"b.pdf":  domain.FormatPDF,
		"c.mobi": domain.FormatMOBI,
		"d.azw3": domain.FormatAZW3,
		"e.cbz":  domain.FormatCBZ,
		"f.cbr":  domain.FormatCBR,
		"g.EPUB": domain.FormatEPUB,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := s.Store(t.Context(), name, strings.NewReader("content for "+name))
			if err != nil {
				t.Fatalf("Store(%q): %v", name, err)
			}
			if got.Format != want {
				t.Fatalf("Format = %q, want %q", got.Format, want)
			}
		})
	}
}

// Storage must never land outside the managed directory, whatever the client
// calls the file.
func TestStoredPathStaysInsideTheManagedDirectory(t *testing.T) {
	s := newStore(t)

	for _, name := range []string{
		"book.epub", "../../escape.epub", "/absolute/book.epub", `..\..\win.epub`,
	} {
		got, err := s.Store(t.Context(), name, strings.NewReader("content"))
		if err != nil {
			continue // rejection is a safe outcome
		}
		abs, err := filepath.Abs(got.Path)
		if err != nil {
			t.Fatalf("resolving %q: %v", got.Path, err)
		}
		root, err := filepath.Abs(s.Root)
		if err != nil {
			t.Fatalf("resolving root: %v", err)
		}
		if !strings.HasPrefix(abs, root+string(filepath.Separator)) {
			t.Fatalf("upload %q stored at %q, outside %q", name, abs, root)
		}
	}
}
