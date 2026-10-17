// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Archive is where books go when the account that owned them is deleted.
//
// The books are kept because somebody may come back for them, and because a
// book is a file rather than a fact about a person: deleting an account has to
// take the account's data with it, and none of that is the book.
type Archive struct {
	// Root is the archive directory, inside Lexarii's own data directory.
	Root string
	// Managed is the directory Lexarii stores uploads in. ONLY files under
	// this are moved.
	Managed string
	// Library is the read-only library directory. Nothing inside it is ever
	// moved, even if it were also inside Managed - the configuration refuses
	// that, and this does not depend on it.
	Library string
}

// Keep moves a file into the archive, and reports where it ended up.
//
// A file outside Lexarii's own storage is left exactly where it is, and the
// returned path is empty. That is the first invariant in this project: the
// library directory belongs to whoever pointed Lexarii at it, and is never
// written to - which includes not tidying it up on somebody's behalf. Such a
// book is archived in the sense that matters, by the record saying so; the
// bytes were never ours to move.
//
// It is also the right behaviour rather than merely the permitted one. A book
// scanned out of a library directory is already kept, by the person who keeps
// that directory, and moving it would take their file away from them at the
// moment an unrelated account was deleted.
func (a Archive) Keep(path string) (string, error) {
	if !a.manages(path) {
		return "", nil
	}

	if err := os.MkdirAll(a.Root, 0o755); err != nil {
		return "", fmt.Errorf("preparing the archive: %w", err)
	}

	// The name is kept, and collisions are not overwritten: two people can have
	// uploaded files with the same content-addressed name, and the archive is
	// the last copy of both.
	target := filepath.Join(a.Root, filepath.Base(path))
	target = a.free(target)

	if err := os.Rename(path, target); err != nil {
		// A rename across filesystems fails, and the data directory can easily
		// be a different mount from the uploads directory on a NAS.
		if copied, cerr := copyFile(path, target); cerr != nil {
			return "", fmt.Errorf("archiving %s: %w", filepath.Base(path), cerr)
		} else if copied {
			if err := os.Remove(path); err != nil {
				// The copy is there, which is what matters; the original being
				// left behind is untidy rather than lossy.
				return target, nil
			}
		}
	}
	return target, nil
}

// manages reports whether a path is Lexarii's own storage to move.
func (a Archive) manages(path string) bool {
	if a.Managed == "" || path == "" || inside(a.Library, path) {
		return false
	}
	managed, err := filepath.Abs(a.Managed)
	if err != nil {
		return false
	}
	full, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	// Separator-terminated, so /data/uploads does not appear to manage
	// /data/uploads-elsewhere.
	return strings.HasPrefix(full, managed+string(os.PathSeparator))
}

// free returns a path that is not taken, by suffixing a counter.
func (a Archive) free(target string) string {
	if _, err := os.Stat(target); os.IsNotExist(err) {
		return target
	}
	ext := filepath.Ext(target)
	stem := strings.TrimSuffix(target, ext)
	for n := 2; n < 1000; n++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, n, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return target
}

func copyFile(from, to string) (bool, error) {
	src, err := os.Open(from)
	if err != nil {
		return false, err
	}
	defer func() { _ = src.Close() }()

	dst, err := os.Create(to)
	if err != nil {
		return false, err
	}
	defer func() { _ = dst.Close() }()

	if _, err := dst.ReadFrom(src); err != nil {
		return false, err
	}
	if err := dst.Sync(); err != nil {
		return false, err
	}
	return true, nil
}
