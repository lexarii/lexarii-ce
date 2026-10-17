// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// One file at a time, by the same rule as the sweep: queued while it has no
// chunks under this strategy, queued again under a new one, never for a scan
// with no text or a file that does not exist - and never twice while a job for
// it is still live.
func TestEnqueueIfNeedsChunksAgreesWithTheSweep(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()

	book, err := p.UpsertBookFile(ctx, sampleInput(hashOf("d"), "/library/waiting.epub", "Waiting", "An Author"))
	if err != nil {
		t.Fatalf("UpsertBookFile: %v", err)
	}
	scan := sampleInput(hashOf("e"), "/library/scan.pdf", "A Scan", "An Author")
	scan.Format, scan.Status = domain.FormatPDF, domain.IngestNeedsOCR
	scanned, err := p.UpsertBookFile(ctx, scan)
	if err != nil {
		t.Fatalf("UpsertBookFile (scan): %v", err)
	}
	missing := uuid.New()

	clearJobs := func() {
		t.Helper()
		if _, err := p.Exec(context.Background(), `DELETE FROM jobs WHERE target_id = ANY($1)`,
			[]uuid.UUID{book.BookFileID, scanned.BookFileID, missing}); err != nil {
			t.Fatalf("clearing jobs: %v", err)
		}
	}
	clearJobs()
	t.Cleanup(clearJobs)

	enqueue := func(id uuid.UUID, version int) bool {
		t.Helper()
		target := id
		queued, err := p.EnqueueIfNeedsChunks(ctx, EnqueueOptions{
			Type: domain.JobChunk, TargetID: &target, Payload: map[string]string{"bookFileId": id.String()},
		}, version)
		if err != nil {
			t.Fatalf("EnqueueIfNeedsChunks: %v", err)
		}
		return queued
	}
	check := func(id uuid.UUID, version int, want bool) {
		t.Helper()
		swept, err := p.BookFilesNeedingChunks(ctx, version, 1000)
		if err != nil {
			t.Fatalf("BookFilesNeedingChunks: %v", err)
		}
		inSweep := false
		for _, target := range swept {
			inSweep = inSweep || target.BookFileID == id
		}
		if got := enqueue(id, version); got != want || inSweep != want {
			t.Errorf("%s under v%d: queued %v, in the sweep %v; want %v", id, version, got, inSweep, want)
		}
	}

	check(scanned.BookFileID, 1, false)
	check(missing, 1, false)
	check(book.BookFileID, 1, true)
	if enqueue(book.BookFileID, 1) {
		t.Error("a second chunk job was queued while the first was still waiting")
	}

	clearJobs()
	if _, err := p.SaveChunks(ctx, book.BookFileID, []ChunkInput{{
		Start:      domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1, ElementPath: "p[1]"},
		End:        domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1, ElementPath: "p[1]", CharOffset: 20},
		Content:    "A passage worth finding.",
		TokenCount: 5,
	}}, 1); err != nil {
		t.Fatalf("SaveChunks: %v", err)
	}
	check(book.BookFileID, 1, false)
	check(book.BookFileID, 2, true)

	var jobs int
	if err := p.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE target_id = $1`, book.BookFileID).Scan(&jobs); err != nil {
		t.Fatalf("counting jobs: %v", err)
	}
	if jobs != 1 {
		t.Errorf("jobs for the file = %d, want the one queued under v2", jobs)
	}
}
