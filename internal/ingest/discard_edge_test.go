// SPDX-License-Identifier: AGPL-3.0-or-later

package ingest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Removal is repeated to finish an interrupted one, so a copy already gone is
// not an error - but it is not reported as removed either.
func TestDiscardRemoveOfAMissingCopyIsQuiet(t *testing.T) {
	uploads := t.TempDir()
	d := Discard{Uploads: uploads}

	removed, err := d.Remove(filepath.Join(uploads, "books", "ab", "gone.epub"))
	if err != nil || removed {
		t.Errorf("Remove = %v, %v; want false, nil", removed, err)
	}
}

// A failure that is not "already gone" is reported, so the caller does not
// record a removal that did not happen.
func TestDiscardRemoveReportsARealFailure(t *testing.T) {
	uploads := t.TempDir()
	dir := filepath.Join(uploads, "books", "ab")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inside.epub"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A non-empty directory cannot be removed with Remove.
	removed, err := Discard{Uploads: uploads}.Remove(dir)
	if err == nil || removed {
		t.Errorf("Remove(non-empty directory) = %v, %v; want an error", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "inside.epub")); err != nil {
		t.Errorf("the directory's contents were touched: %v", err)
	}

	covers := t.TempDir()
	if err := os.MkdirAll(filepath.Join(covers, "stuck", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (Discard{Covers: covers}).RemoveCover("stuck"); err == nil {
		t.Error("RemoveCover of something it cannot delete reported success")
	}
	if err := (Discard{Covers: covers}).RemoveCover("never-existed.jpg"); err != nil {
		t.Errorf("RemoveCover of a missing cover: %v", err)
	}
}

// Only a lowercase SHA-256 may become part of a path.
func TestDiscardCopiesRefusesAnythingButAHash(t *testing.T) {
	d := Discard{Uploads: t.TempDir(), Archive: t.TempDir()}
	for _, hash := range []string{
		"",
		strings.Repeat("a", 63),
		strings.Repeat("A", 64),
		strings.Repeat("g", 64),
		strings.Repeat("a", 62) + "/.",
		"../" + strings.Repeat("a", 61),
	} {
		if got := d.Copies(hash); got != nil {
			t.Errorf("Copies(%q) = %v, want nothing", hash, got)
		}
	}
}
