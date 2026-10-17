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
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// MaxFileSize caps the size of a file the scanner will consider. Well past any
// real book; a 4 GiB "epub" is something other than a book.
const MaxFileSize = 4 << 30

// ErrOutsideLibrary reports a path that resolves outside the library directory.
var ErrOutsideLibrary = errors.New("path escapes the library directory")

// ScannedFile is one file found during a scan.
type ScannedFile struct {
	// Path is absolute.
	Path string
	// RelPath is relative to the library root and is what the user recognises.
	RelPath string
	// ContentHash is the SHA-256 of the file bytes, and is file identity. The
	// same hash is the same book: a moved or renamed file is recognised and
	// updated rather than duplicated.
	ContentHash string
	Format      domain.Format
	SizeBytes   int64
	ModTime     int64

	// Err records why this file could not be scanned. A file with an error is
	// still reported, so the UI can show it rather than silently omitting it.
	Err error
}

// ScanStats summarises a scan.
type ScanStats struct {
	FilesSeen    int
	BooksFound   int
	Skipped      int
	Errors       int
	BytesHashed  int64
	DirsScanned  int
	SymlinksSeen int
}

// Scanner walks a library directory and identifies book files.
//
// The library directory is READ-ONLY. The scanner opens files for reading and
// never creates, modifies, renames, or deletes anything under it. That promise
// is what makes a user willing to point this at a twenty-year collection.
type Scanner struct {
	// Root is the library directory. It is resolved once, and every file found
	// is verified to live underneath it.
	Root string

	// FollowSymlinks is false by default. A symlink in a library directory can
	// point anywhere on the filesystem, including outside the library, and
	// following them by default would quietly break the read-only boundary and
	// invite loops.
	FollowSymlinks bool

	Logger *slog.Logger
}

// bookExtensions maps a lowercase file extension to its format.
//
// Formats with no parser yet are listed deliberately. Skipping them silently
// means a user with MOBI files sees no trace of them at all - not in the
// library, not as an error - and has no way to tell whether Lexarii noticed
// them. They are ingested as `unsupported` instead: visible on the shelf,
// honestly labelled, and already attached to the right book when a readable
// format of it exists.
var bookExtensions = map[string]domain.Format{
	".epub": domain.FormatEPUB,
	".pdf":  domain.FormatPDF,
	".mobi": domain.FormatMOBI,
	".azw3": domain.FormatAZW3,
	".cbz":  domain.FormatCBZ,
	".cbr":  domain.FormatCBR,
}

// FormatForPath reports the book format implied by a path's extension.
func FormatForPath(path string) (domain.Format, bool) {
	f, ok := bookExtensions[strings.ToLower(filepath.Ext(path))]
	return f, ok
}

// Scan walks the library directory, emitting one ScannedFile per book found.
//
// Errors against individual files are reported through the callback and never
// abort the walk: one unreadable file in a 5000-book library must cost that one
// file. Only a failure to read the root itself is fatal.
//
// The callback runs synchronously. Returning an error from it stops the scan.
func (s *Scanner) Scan(ctx context.Context, emit func(ScannedFile) error) (ScanStats, error) {
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}

	root, err := filepath.Abs(s.Root)
	if err != nil {
		return ScanStats{}, fmt.Errorf("resolving library directory %q: %w", s.Root, err)
	}
	// Resolve symlinks in the root itself so the containment check below
	// compares like with like.
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}

	info, err := os.Stat(root)
	if err != nil {
		return ScanStats{}, fmt.Errorf("reading library directory %q: %w", root, err)
	}
	if !info.IsDir() {
		return ScanStats{}, fmt.Errorf("library directory %q is not a directory", root)
	}

	var stats ScanStats

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// An unreadable directory is reported and skipped, not fatal: a
			// permissions problem on one folder must not cost the whole library.
			stats.Errors++
			log.Warn("skipping unreadable path", "path", path, "error", err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			stats.DirsScanned++
			if shouldSkipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}

		if d.Type()&fs.ModeSymlink != 0 {
			stats.SymlinksSeen++
			if !s.FollowSymlinks {
				stats.Skipped++
				return nil
			}
			// Even when following is enabled, the target must stay inside the
			// library. Otherwise a symlink is a way to make Lexarii read
			// arbitrary files on the host.
			target, err := filepath.EvalSymlinks(path)
			if err != nil || !within(root, target) {
				stats.Skipped++
				log.Warn("skipping symlink pointing outside the library", "path", path)
				return nil
			}
			path = target
		}

		stats.FilesSeen++

		format, ok := FormatForPath(path)
		if !ok {
			stats.Skipped++
			return nil
		}
		if !within(root, path) {
			stats.Skipped++
			log.Warn("skipping path outside the library", "path", path)
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}

		file := ScannedFile{Path: path, RelPath: rel, Format: format}

		fi, err := d.Info()
		if err != nil {
			file.Err = fmt.Errorf("stat: %w", err)
			stats.Errors++
			return emit(file)
		}
		file.SizeBytes = fi.Size()
		file.ModTime = fi.ModTime().Unix()

		switch {
		case fi.Size() == 0:
			file.Err = errors.New("file is empty")
			stats.Errors++
			return emit(file)
		case fi.Size() > MaxFileSize:
			file.Err = fmt.Errorf("file is %d bytes, over the %d byte limit", fi.Size(), int64(MaxFileSize))
			stats.Errors++
			return emit(file)
		}

		hash, err := HashFile(ctx, path)
		if err != nil {
			file.Err = fmt.Errorf("hashing: %w", err)
			stats.Errors++
			return emit(file)
		}
		file.ContentHash = hash
		stats.BytesHashed += fi.Size()
		stats.BooksFound++

		return emit(file)
	})

	if walkErr != nil {
		return stats, walkErr
	}
	return stats, nil
}

// HashFile computes the SHA-256 of a file's contents.
//
// Streamed, never buffered: a 900 MiB PDF must not become 900 MiB of resident
// memory, and a library scan hashes thousands of files in sequence.
func HashFile(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, &ctxReader{ctx: ctx, r: f}); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ctxReader makes a long read cancellable, so stopping a scan does not have to
// wait for a multi-gigabyte file to finish hashing.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// within reports whether path is inside root.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// shouldSkipDir reports directories that never contain books worth scanning.
func shouldSkipDir(name string) bool {
	switch name {
	case ".git", ".svn", "node_modules", "__MACOSX", ".Trash", "$RECYCLE.BIN",
		".stfolder", ".stversions": // Syncthing metadata
		return true
	}
	// Hidden directories are conventionally metadata, not content. "." itself
	// reaches this function as the root and must not be skipped.
	return len(name) > 1 && strings.HasPrefix(name, ".")
}
