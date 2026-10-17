// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// Discard deletes what Lexarii itself stored of a file an administrator has
// removed from the installation: copies in its upload directory or its
// archive, and the file's cover. Nothing else, ever.
//
// A path anywhere else - above all in the library directory, which is
// read-only and somebody else's - is left exactly where it is, and reported
// as not removed. That is the first invariant in this project, and it holds
// for removal as it does for archiving: the blocklist, not deletion, is what
// keeps such a file off every shelf.
type Discard struct {
	// Uploads is Lexarii's own upload directory.
	Uploads string
	// Archive is where the books of departed holders are kept.
	Archive string
	// Covers is the cover directory. Covers are stored by name.
	Covers string
	// Library is the read-only library directory. Nothing inside it is ever
	// deleted, even if it were also inside Uploads or Archive - the
	// configuration refuses that, and this does not depend on it.
	Library string
}

// Copies lists where Lexarii may hold these bytes under their own name: the
// upload directory, in every format an upload can arrive as - storage is
// content-addressed, so a copy uploaded but not yet extracted, or refused, is
// at one of these - and the archive, which keeps an upload's name and suffixes
// a counter on a collision (Archive.free). Every such name is the hash of its
// own bytes, so each of these is a copy of exactly these bytes and nothing
// else; a file scanned from the library directory is never moved into the
// archive, so none of these is ever one.
func (d Discard) Copies(hash string) []string {
	if !validHash(hash) {
		return nil
	}
	return d.CopiesIn(hash, d.ArchiveIndex())
}

// ArchiveIndex lists the archive once: every upload's copy in it, by the hash
// its name carries. For deleting the copies of many files, where listing the
// archive for each would read the same directory over and over.
func (d Discard) ArchiveIndex() map[string][]string {
	index := map[string][]string{}
	if d.Archive == "" {
		return index
	}
	entries, err := os.ReadDir(d.Archive)
	if err != nil {
		return index
	}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || len(name) < 64 {
			continue
		}
		if hash := name[:64]; validHash(hash) && archivedCopyOf(hash, name) {
			index[hash] = append(index[hash], filepath.Join(d.Archive, name))
		}
	}
	return index
}

// CopiesIn is Copies with the archive already listed by ArchiveIndex.
func (d Discard) CopiesIn(hash string, index map[string][]string) []string {
	if !validHash(hash) {
		return nil
	}
	var out []string
	if d.Uploads != "" {
		store := UploadStore{Root: d.Uploads}
		seen := map[domain.Format]bool{}
		for _, f := range bookExtensions {
			if !seen[f] {
				seen[f] = true
				out = append(out, store.PathFor(hash, f))
			}
		}
	}
	out = append(out, index[hash]...)
	sort.Strings(out)
	return out
}

// archivedCopyOf reports whether an archive file name is an upload of these
// bytes: "<hash>.<format>", or "<hash>-<n>.<format>" after a collision.
func archivedCopyOf(hash, name string) bool {
	ext := filepath.Ext(name)
	if _, ok := bookExtensions[ext]; !ok {
		return false
	}
	stem := strings.TrimSuffix(name, ext)
	if stem == hash {
		return true
	}
	n, ok := strings.CutPrefix(stem, hash+"-")
	if !ok || n == "" {
		return false
	}
	for _, c := range n {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Remove deletes one copy, if it is Lexarii's to delete, and reports whether
// it deleted anything. A copy already gone is not an error - removal is
// repeated to finish an interrupted one - but is not reported as removed.
func (d Discard) Remove(path string) (bool, error) {
	if inside(d.Library, path) || path == d.Library {
		return false, nil
	}
	if !inside(d.Uploads, path) && !inside(d.Archive, path) {
		return false, nil
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// RemoveCover deletes a stored cover by name. A name that is anything but a
// plain file name is refused rather than resolved.
func (d Discard) RemoveCover(name string) error {
	if d.Covers == "" || name == "" || filepath.Base(name) != name || name == "." || name == ".." ||
		inside(d.Library, filepath.Join(d.Covers, name)) {
		return nil
	}
	if err := os.Remove(filepath.Join(d.Covers, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// SamePath reports whether two paths name the same file, however each is
// spelled: compared as cleaned absolute paths, and, when both exist, by the
// file itself (os.SameFile), which also sees through symbolic links and a
// directory reached by two names. Anything deciding that a copy is NOT some
// record's copy - and so may be deleted - must ask this, never compare
// strings: a data directory configured as "./data" one day and "/srv/data"
// the next names the same files differently.
func SamePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil && errB == nil && absA == absB {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// IsUpload reports whether a path lies in Lexarii's upload directory.
func (d Discard) IsUpload(path string) bool {
	return inside(d.Uploads, path)
}

// inside reports whether path lies strictly inside dir. An empty dir contains
// nothing, so a missing setting can never widen what may be removed.
func inside(dir, path string) bool {
	if dir == "" || path == "" {
		return false
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	full, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, full)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// validHash reports a SHA-256 in lowercase hex, the only shape a content hash
// has. Anything else must not become part of a path.
func validHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, c := range hash {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
