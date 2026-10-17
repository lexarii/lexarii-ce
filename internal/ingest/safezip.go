// SPDX-License-Identifier: AGPL-3.0-or-later

// Package ingest turns files on disk into structured book records.
//
// Everything here treats its input as hostile. Ebooks are zip archives
// downloaded from the internet, and a malformed or malicious one must fail
// loudly against one file rather than take down a scan or fill a disk.
package ingest

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// Archive safety limits. Generous enough for real books - a large illustrated
// EPUB is a few hundred megabytes and a few thousand entries - and tight enough
// that a decompression bomb hits a ceiling instead of the disk.
const (
	// MaxEntries caps the number of files in one archive.
	MaxEntries = 10_000
	// MaxEntrySize caps a single decompressed entry.
	MaxEntrySize = 128 << 20 // 128 MiB
	// MaxTotalSize caps the decompressed size of everything read from one archive.
	MaxTotalSize = 1 << 30 // 1 GiB
	// MaxCompressionRatio rejects entries whose declared expansion is absurd.
	// A 1000:1 ratio is a bomb; real text compresses at roughly 3:1 to 10:1.
	MaxCompressionRatio = 1000
)

var (
	// ErrUnsafePath reports a zip entry that escapes the archive root.
	ErrUnsafePath = errors.New("unsafe archive path")
	// ErrTooManyEntries reports an archive with more entries than MaxEntries.
	ErrTooManyEntries = errors.New("too many archive entries")
	// ErrEntryTooLarge reports an entry exceeding MaxEntrySize.
	ErrEntryTooLarge = errors.New("archive entry too large")
	// ErrArchiveTooLarge reports cumulative reads exceeding MaxTotalSize.
	ErrArchiveTooLarge = errors.New("archive too large")
	// ErrSuspiciousRatio reports an entry with a decompression ratio consistent
	// with a zip bomb.
	ErrSuspiciousRatio = errors.New("suspicious compression ratio")
	// ErrEntryNotFound reports a missing archive entry.
	ErrEntryNotFound = errors.New("archive entry not found")
)

// sanitizeEntryPath normalises a zip entry name and rejects anything that
// escapes the archive root.
//
// Zip entry names are attacker-controlled. The classic attack is "zip slip":
// an entry named ../../etc/cron.d/x that writes outside the extraction
// directory. Lexarii never extracts to disk, but entry names are also used to
// resolve OPF-relative hrefs, so a traversing name must be rejected at the
// boundary rather than trusted anywhere downstream.
//
// The returned path is always slash-separated, relative, and free of "." and
// ".." segments.
func sanitizeEntryPath(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%w: empty name", ErrUnsafePath)
	}

	// Zip stores forward slashes, but archives written on Windows sometimes use
	// backslashes. Normalise before inspecting, or a "..\.." slips through.
	normalised := strings.ReplaceAll(name, `\`, "/")

	if strings.ContainsRune(normalised, 0) {
		return "", fmt.Errorf("%w: NUL byte in %q", ErrUnsafePath, name)
	}
	// An absolute path escapes the root just as surely as "..".
	if strings.HasPrefix(normalised, "/") {
		return "", fmt.Errorf("%w: absolute path %q", ErrUnsafePath, name)
	}

	cleaned := path.Clean(normalised)

	// Every remaining check runs on the CLEANED value, not on the input.
	// Clean can create a dangerous path out of an innocuous-looking one:
	// "./C:evil" cleans to "C:evil", and "x/../C:evil" does too. Checking the
	// input alone lets those through and, worse, makes the function
	// non-idempotent - re-sanitising its own output would reject it. A fuzzer
	// found exactly that.
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: %q escapes the archive root", ErrUnsafePath, name)
	}
	if cleaned == "." {
		return "", fmt.Errorf("%w: %q resolves to the root itself", ErrUnsafePath, name)
	}
	if strings.HasPrefix(cleaned, "/") {
		return "", fmt.Errorf("%w: %q resolves to an absolute path", ErrUnsafePath, name)
	}
	// Windows drive letters and UNC paths, checked per segment: only a leading
	// segment can carry a drive, but a cleaned path may have promoted a later
	// segment to the front.
	if first, _, _ := strings.Cut(cleaned, "/"); strings.Contains(first, ":") {
		return "", fmt.Errorf("%w: drive-qualified path %q", ErrUnsafePath, name)
	}
	return cleaned, nil
}

// SanitizePath normalises an archive path that is already relative to the
// archive root, rejecting anything that escapes it.
//
// Use this for paths the format specifies as root-relative - notably the
// container.xml rootfile full-path, which is relative to the archive root and
// NOT to the directory containing container.xml. Use ResolveRelative instead
// for hrefs that are relative to the document containing them.
func SanitizePath(name string) (string, error) {
	return sanitizeEntryPath(name)
}

// SafeZip is a zip archive with traversal, size, and count protections applied.
// It never writes to disk.
type SafeZip struct {
	r *zip.Reader
	// entries maps sanitised paths to archive entries. Names that fail
	// sanitisation are absent, so a traversing entry is unreachable rather than
	// merely discouraged.
	entries map[string]*zip.File
	// order preserves archive order for callers that need to scan rather than
	// look up by name.
	order []string

	readSoFar int64
}

// OpenSafeZip validates an archive's entry names and limits, and returns a
// reader over the entries that passed.
//
// Unsafe entry names are skipped rather than fatal: real EPUBs occasionally
// carry junk like "__MACOSX/" siblings, and rejecting a whole book over one bad
// name would fail more often on sloppiness than on attacks. The count of
// skipped entries is returned so the caller can record it.
func OpenSafeZip(r io.ReaderAt, size int64) (*SafeZip, int, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, 0, fmt.Errorf("reading zip: %w", err)
	}

	if len(zr.File) > MaxEntries {
		return nil, 0, fmt.Errorf("%w: %d entries (limit %d)", ErrTooManyEntries, len(zr.File), MaxEntries)
	}

	sz := &SafeZip{
		r:       zr,
		entries: make(map[string]*zip.File, len(zr.File)),
		order:   make([]string, 0, len(zr.File)),
	}

	skipped := 0
	for _, f := range zr.File {
		clean, err := sanitizeEntryPath(f.Name)
		if err != nil {
			skipped++
			continue
		}
		// Directory entries carry no content and are not addressable.
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		if f.UncompressedSize64 > MaxEntrySize {
			skipped++
			continue
		}
		// A tiny compressed entry declaring a huge expansion is the signature of
		// a bomb. Checked here so it never reaches a read.
		if f.CompressedSize64 > 0 && f.UncompressedSize64/f.CompressedSize64 > MaxCompressionRatio {
			skipped++
			continue
		}
		if _, exists := sz.entries[clean]; exists {
			// Duplicate names: keep the first. A second entry shadowing the
			// first is a known trick for showing one file to a validator and a
			// different one to a reader.
			skipped++
			continue
		}
		sz.entries[clean] = f
		sz.order = append(sz.order, clean)
	}

	return sz, skipped, nil
}

// Names returns the sanitised entry paths in archive order.
func (z *SafeZip) Names() []string {
	out := make([]string, len(z.order))
	copy(out, z.order)
	return out
}

// Has reports whether an entry exists under the given sanitised path.
func (z *SafeZip) Has(name string) bool {
	clean, err := sanitizeEntryPath(name)
	if err != nil {
		return false
	}
	_, ok := z.entries[clean]
	return ok
}

// ReadFile returns the decompressed contents of one entry, enforcing the
// per-entry and cumulative size limits.
//
// The limit is enforced during the read, not from the header: the declared
// UncompressedSize64 is attacker-controlled and may lie.
func (z *SafeZip) ReadFile(name string) ([]byte, error) {
	clean, err := sanitizeEntryPath(name)
	if err != nil {
		return nil, err
	}

	f, ok := z.entries[clean]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrEntryNotFound, name)
	}

	remaining := MaxTotalSize - z.readSoFar
	if remaining <= 0 {
		return nil, fmt.Errorf("%w: already read %d bytes", ErrArchiveTooLarge, z.readSoFar)
	}
	limit := int64(MaxEntrySize)
	if remaining < limit {
		limit = remaining
	}

	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", clean, err)
	}
	defer func() { _ = rc.Close() }()

	// Read one byte past the limit so an over-long entry is detected rather than
	// silently truncated. Truncated book content is worse than a rejected file:
	// it produces plausible-looking wrong text.
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", clean, err)
	}
	if int64(len(data)) > limit {
		if limit == remaining {
			return nil, fmt.Errorf("%w: exceeded %d bytes total", ErrArchiveTooLarge, MaxTotalSize)
		}
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrEntryTooLarge, clean, MaxEntrySize)
	}

	z.readSoFar += int64(len(data))
	return data, nil
}

// ResolveRelative resolves an href relative to the directory containing base,
// the way an OPF manifest or an EPUB nav document addresses its siblings.
// Traversal out of the archive root is rejected.
func ResolveRelative(base, href string) (string, error) {
	// Fragment identifiers address a position inside the target, not a file.
	if i := strings.IndexByte(href, '#'); i >= 0 {
		href = href[:i]
	}
	if href == "" {
		return "", fmt.Errorf("%w: empty href", ErrUnsafePath)
	}
	// Absolute URLs are not archive entries; a remote reference in a book is
	// something to ignore, not to fetch. "//host/path" is protocol-relative and
	// is just as remote as "https://host/path", despite looking like a path.
	if strings.Contains(href, "://") || strings.HasPrefix(href, "//") {
		return "", fmt.Errorf("%w: remote reference %q", ErrUnsafePath, href)
	}

	dir := path.Dir(base)
	if dir == "." {
		dir = ""
	}
	return sanitizeEntryPath(path.Join(dir, href))
}
