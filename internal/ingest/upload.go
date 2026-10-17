// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// Upload errors. Each names a condition the caller should report per file
// rather than treating as a failure of the whole batch.
var (
	// ErrEmptyUpload reports a zero-byte upload.
	ErrEmptyUpload = errors.New("uploaded file is empty")
	// ErrUploadTooLarge reports an upload over the size limit.
	ErrUploadTooLarge = errors.New("uploaded file is too large")
	// ErrUnsupportedUpload reports a file extension Lexarii does not accept.
	ErrUnsupportedUpload = errors.New("unsupported file type")
	// ErrBadFilename reports a filename that cannot be used safely.
	ErrBadFilename = errors.New("unusable filename")
)

// StoredFile is the result of accepting one upload.
type StoredFile struct {
	// Path is where the file now lives, inside the managed directory.
	Path string
	// OriginalFilename is the name the client supplied, sanitised. It is kept
	// because same-book merging derives its stem from the filename, and
	// content-addressed storage throws the name away.
	OriginalFilename string
	ContentHash      string
	Format           domain.Format
	SizeBytes        int64
	// Duplicate is true when a file with this hash was already stored, in which
	// case nothing new was written to Path. Either way the bytes received are
	// kept aside until Settle.
	Duplicate bool

	// spare is this upload's own copy of its bytes - a second link to a new
	// copy, or the bytes of a duplicate - kept until the upload is recorded
	// (Settle).
	spare string
}

// UploadStore writes uploaded books into a content-addressed directory that
// Lexarii owns.
//
// This is NOT the user's library directory. That one is read-only and is never
// written to; this is Lexarii's own managed storage. Keeping the two separate
// is what lets uploads exist without weakening the read-only promise.
type UploadStore struct {
	// Root is the managed directory. Created on demand.
	Root string
	// MaxFileSize caps one upload. Zero means MaxUploadSize.
	MaxFileSize int64
}

// MaxUploadSize is the default per-file cap. Comfortably past any real book:
// the largest file in a 3.3 GB technical library was 158 MB.
const MaxUploadSize = 2 << 30 // 2 GiB

// Store streams one upload to disk, deduplicating by content.
//
// The body is streamed and hashed at the same time, never buffered: a browser
// uploading a 158 MB PDF must not become 158 MB of server memory, and a bulk
// upload may run dozens of those.
//
// Writing goes to a temporary file first and is renamed into place only once the
// whole body has arrived. An interrupted upload therefore leaves no partial file
// that a later scan would mistake for a real book - which matters precisely
// because an interrupted bulk upload is the case this is designed around.
func (s *UploadStore) Store(ctx context.Context, filename string, body io.Reader) (StoredFile, error) {
	name, err := SafeFilename(filename)
	if err != nil {
		return StoredFile{}, err
	}

	format, ok := FormatForPath(name)
	if !ok {
		return StoredFile{}, fmt.Errorf("%w: %s", ErrUnsupportedUpload, filepath.Ext(name))
	}

	limit := s.MaxFileSize
	if limit <= 0 {
		limit = MaxUploadSize
	}

	tmpDir := filepath.Join(s.Root, "incoming")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return StoredFile{}, fmt.Errorf("preparing upload directory: %w", err)
	}

	tmp, err := os.CreateTemp(tmpDir, "upload-*")
	if err != nil {
		return StoredFile{}, fmt.Errorf("creating temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	keepTmp := false
	// Removing the temp file is safe after a successful rename: the path no
	// longer exists and Remove simply reports that.
	defer func() {
		_ = tmp.Close()
		if !keepTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	// One byte past the limit, so an over-long upload is detected rather than
	// silently truncated into a corrupt book.
	written, err := io.Copy(io.MultiWriter(tmp, hasher),
		io.LimitReader(&ctxReader{ctx: ctx, r: body}, limit+1))
	if err != nil {
		return StoredFile{}, fmt.Errorf("receiving %s: %w", name, err)
	}
	if written == 0 {
		return StoredFile{}, fmt.Errorf("%w: %s", ErrEmptyUpload, name)
	}
	if written > limit {
		return StoredFile{}, fmt.Errorf("%w: %s exceeds %d bytes", ErrUploadTooLarge, name, limit)
	}
	if err := tmp.Sync(); err != nil {
		return StoredFile{}, fmt.Errorf("flushing %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return StoredFile{}, fmt.Errorf("closing %s: %w", name, err)
	}

	hash := hex.EncodeToString(hasher.Sum(nil))
	final := s.PathFor(hash, format)

	out := StoredFile{
		Path:             final,
		OriginalFilename: name,
		ContentHash:      hash,
		Format:           format,
		SizeBytes:        written,
	}

	// Content-addressed: if the bytes are already here, the upload was a repeat.
	// This is what makes re-uploading a whole batch after a failure cheap and
	// safe rather than a source of duplicates.
	//
	// This upload's own copy is kept aside, though, until it is recorded
	// (Settle): the stored one may belong to a record that is being erased
	// right now, and until this upload's claim exists nothing stops that
	// erasure deleting it.
	if _, err := os.Stat(final); err == nil {
		out.Duplicate = true
		out.spare = tmpPath
		keepTmp = true
		return out, nil
	}

	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return StoredFile{}, fmt.Errorf("preparing storage for %s: %w", name, err)
	}
	// A new copy is kept aside too, as a second link to the same bytes: an
	// erasure of a record of these bytes - one scanned from the library
	// directory, say, with no copy here - deletes every stored copy of them
	// it finds, and until this upload is recorded nothing tells it this one
	// is wanted. A filesystem without hard links gets a copy instead: twice
	// the disk for a moment, rather than an upload that can lose its bytes.
	if err := linkFile(tmpPath, final); err == nil {
		out.spare = tmpPath
		keepTmp = true
		return out, nil
	}
	spare := tmpPath + ".spare"
	if _, err := copyFile(tmpPath, spare); err == nil {
		out.spare = spare
	} else {
		// No room for a second copy: stored as every upload was before,
		// with nothing aside.
		_ = os.Remove(spare)
	}
	if err := os.Rename(tmpPath, final); err != nil {
		if out.spare != "" {
			_ = os.Remove(out.spare)
		}
		return StoredFile{}, fmt.Errorf("storing %s: %w", name, err)
	}

	return out, nil
}

// linkFile is os.Link, replaceable in a test of a filesystem without hard
// links.
var linkFile = os.Link

// Settle finishes an upload once it has been recorded - a holding or a claim
// on its bytes written, under the file's lock - or refused.
//
// Store kept this upload's own copy aside - a second link to a new copy, or
// the received bytes of a duplicate - because until the upload was recorded
// nothing stopped the stored copy being deleted: by an erasure of a record of
// the same bytes (LEXARII_DELETED_ACCOUNT_FILES=erase, or an administrator
// erasing somebody's books), which deletes every stored copy of them that
// nobody holds or is waiting for.
// Once recorded, anything deleting the bytes sees this upload's claim and
// leaves them. So: kept, the copy aside replaces a stored copy that has gone
// and is removed otherwise; not kept - the bytes were refused - it is only
// removed. Safe to call for any StoredFile.
func (s *UploadStore) Settle(f StoredFile, keep bool) error {
	if f.spare == "" {
		if !keep || f.Path == "" {
			return nil
		}
		// Nothing was kept aside - there was no room for a copy - so the
		// stored copy is all there is. Gone, the upload is not recorded:
		// the person is told it failed, rather than holding a book with
		// nothing to open.
		if _, err := os.Stat(f.Path); err != nil {
			return fmt.Errorf("the stored copy of %s is gone and none was kept aside: %w", f.ContentHash, err)
		}
		return nil
	}
	if keep {
		if _, err := os.Stat(f.Path); errors.Is(err, fs.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
				return fmt.Errorf("restoring %s: %w", f.ContentHash, err)
			}
			if err := os.Rename(f.spare, f.Path); err != nil {
				return fmt.Errorf("restoring %s: %w", f.ContentHash, err)
			}
			return nil
		}
	}
	if err := os.Remove(f.spare); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing a spare copy of %s: %w", f.ContentHash, err)
	}
	return nil
}

// PathFor returns the content-addressed location of a hash.
//
// The two-character prefix directory keeps any single directory from holding
// tens of thousands of entries, which some filesystems handle poorly.
func (s *UploadStore) PathFor(hash string, format domain.Format) string {
	return filepath.Join(s.Root, "books", hash[:2], hash+"."+string(format))
}

// SafeFilename sanitises a client-supplied filename.
//
// The name is used only as a label - storage is content-addressed, so this
// never becomes a path - but it is still attacker-controlled text that ends up
// in the database, in the UI, and in the stem used for same-book merging. Any
// directory component is stripped rather than cleaned, because an upload has no
// legitimate reason to carry one.
func SafeFilename(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("%w: empty", ErrBadFilename)
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("%w: contains a NUL byte", ErrBadFilename)
	}

	// Strip every directory component, on either separator: browsers on Windows
	// send backslash-separated paths for directory uploads.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)

	// After stripping, "..", "." and hidden-file names carry no useful meaning.
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%w: %q has no filename part", ErrBadFilename, raw)
	}

	// Control characters would corrupt log lines and UI text.
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" {
		return "", fmt.Errorf("%w: %q is entirely control characters", ErrBadFilename, raw)
	}

	const maxNameLen = 255
	if len(name) > maxNameLen {
		// Trim the middle rather than the end, so the extension survives - the
		// extension is what determines the format.
		ext := filepath.Ext(name)
		keep := maxNameLen - len(ext)
		if keep < 1 {
			return "", fmt.Errorf("%w: %q is too long", ErrBadFilename, raw)
		}
		name = name[:keep] + ext
	}

	return name, nil
}
