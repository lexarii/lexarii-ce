// SPDX-License-Identifier: AGPL-3.0-or-later

package worker_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

func sha256File(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	return sha256Hex(data)
}

// A file removed for everyone stays where it is in the library directory -
// which is read-only - and a rescan passes over it: no holding, no record
// brought back, no extraction queued. Nor does a copy of the same bytes under
// another name get a record of its own.
func TestAScanPassesOverBlockedFiles(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	admin, err := p.EnsureDefaultUser(ctx)
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}
	root := t.TempDir()
	removed := filepath.Join(root, "removed.epub")
	writeEPUB(t, removed, "Not To Be Kept", "An Author")
	writeEPUB(t, filepath.Join(root, "kept.epub"), "Kept", "An Author")
	before, err := os.ReadFile(removed)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}

	ing := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger()}
	scan := func() {
		t.Helper()
		if _, _, err := p.Enqueue(ctx, postgres.EnqueueOptions{
			Type: domain.JobScan, Payload: worker.ScanPayload{OwnerID: admin}, RequestedBy: admin,
		}); err != nil {
			t.Fatalf("enqueue scan: %v", err)
		}
		drain(t, p, ing)
	}
	scan()

	var id uuid.UUID
	if err := p.QueryRow(ctx, `SELECT id FROM book_files WHERE path = $1`, removed).Scan(&id); err != nil {
		t.Fatalf("finding the file: %v", err)
	}
	if _, err := p.RemoveBookFileForEveryone(ctx, id, admin, "not to be kept", postgres.RemovalOptions{}); err != nil {
		t.Fatalf("removing: %v", err)
	}
	// The same bytes under another name, put there after the removal.
	if err := os.WriteFile(filepath.Join(root, "again.epub"), before, 0o644); err != nil {
		t.Fatalf("copying: %v", err)
	}
	if _, err := p.Exec(ctx, `DELETE FROM jobs`); err != nil {
		t.Fatalf("clearing jobs: %v", err)
	}
	scan()

	after, err := os.ReadFile(removed)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("the library file was touched: %v", err)
	}
	if n := countRows(t, p, `SELECT count(*) FROM book_file_holders WHERE book_file_id = $1`, id); n != 0 {
		t.Errorf("a rescan made %d holders of a removed file", n)
	}
	if n := countRows(t, p, `SELECT count(*) FROM book_files WHERE id = $1 AND missing_since IS NULL`, id); n != 0 {
		t.Error("a rescan marked a removed file present again")
	}
	if n := countRows(t, p, `SELECT count(*) FROM book_files`); n != 2 {
		t.Errorf("%d records after the rescan, want the two there were", n)
	}
	if n := countRows(t, p, `SELECT count(*) FROM jobs WHERE type = 'extract'`); n != 0 {
		t.Errorf("a rescan queued %d extractions of blocked bytes", n)
	}
}

// An upload's extraction queued before its bytes were removed for everyone is
// dropped when it runs: nothing recorded, nobody holding, and the upload's own
// copy deleted from the upload directory. The job succeeds - retrying would
// find the same block.
func TestAnExtractionOfBlockedBytesIsDropped(t *testing.T) {
	r := newRaceRig(t)
	r.ing.UploadDir = r.uploads
	r.enqueue(t)
	// The removal: the bytes are blocked and the uploader's claim is gone.
	if _, err := r.pool.Exec(t.Context(), `
		INSERT INTO blocked_content (content_hash, reason) VALUES ($1, 'not to be kept')`,
		r.stored.ContentHash); err != nil {
		t.Fatalf("blocking: %v", err)
	}
	if _, err := r.pool.Exec(t.Context(), `DELETE FROM holding_claims`); err != nil {
		t.Fatalf("dropping the claim: %v", err)
	}

	r.runOne(t)

	if n := countRows(t, r.pool, `SELECT count(*) FROM book_files`); n != 0 {
		t.Errorf("an extraction of blocked bytes recorded %d files", n)
	}
	if _, err := os.Stat(r.stored.Path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the blocked upload is still on disk: %v", err)
	}
}

// The same, for a file a scan found: dropped, and the file in the library
// directory left exactly where it is.
func TestAScannedExtractionOfBlockedBytesLeavesTheFile(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	root := t.TempDir()
	path := filepath.Join(root, "book.epub")
	writeEPUB(t, path, "Scanned", "An Author")
	hash := sha256File(t, path)
	if _, err := p.Exec(ctx, `INSERT INTO blocked_content (content_hash, reason) VALUES ($1, 'r')`, hash); err != nil {
		t.Fatalf("blocking: %v", err)
	}
	target := worker.TargetForHash(hash)
	if _, _, err := p.Enqueue(ctx, postgres.EnqueueOptions{
		Type: domain.JobExtract, TargetID: &target,
		Payload: worker.ExtractPayload{Path: path, ContentHash: hash, Format: domain.FormatEPUB, SizeBytes: 1},
	}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// The upload directory set to the library's own, which the configuration
	// refuses: even then, only an upload's copy is ever deleted.
	ing := &worker.Ingest{Store: p, LibraryDir: root, UploadDir: root, Logger: quietTestLogger()}
	drain(t, p, ing)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("the library file is gone: %v", err)
	}
	if n := countRows(t, p, `SELECT count(*) FROM book_files`); n != 0 {
		t.Errorf("%d records of blocked bytes", n)
	}
	if n := countRows(t, p, `SELECT count(*) FROM jobs WHERE status <> 'succeeded'`); n != 0 {
		t.Errorf("%d jobs did not succeed", n)
	}
}
