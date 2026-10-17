// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// syncShelfForStreams gives the fixture's user n more files of their own, so positions
// can be paged.
func syncShelfForStreams(t *testing.T, p *Pool, userID uuid.UUID, n int) []uuid.UUID {
	t.Helper()
	var out []uuid.UUID
	for i := 0; i < n; i++ {
		in := owned(sampleInput(fmt.Sprintf("%063x%d", 0xabc, i), fmt.Sprintf("/library/sync-%d.epub", i),
			fmt.Sprintf("Synced Work %d", i), "An Author"), userID)
		res, err := p.UpsertBookFile(t.Context(), in)
		if err != nil {
			t.Fatalf("UpsertBookFile: %v", err)
		}
		out = append(out, res.BookFileID)
	}
	return out
}

// A cursor can only have come from this server; one that does not decode is
// the client's mistake, reported as an invalid query rather than a fault.
func TestChangesRefusesAMangledCursor(t *testing.T) {
	p := testPool(t)
	userID, _ := annotationFixture(t, p, domain.FormatEPUB)

	for name, cursor := range map[string]string{
		"not base64":     "!!!not-a-cursor!!!",
		"not the format": base64.RawURLEncoding.EncodeToString([]byte("[1,2,3]")),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p.Changes(t.Context(), userID, time.Time{}, cursor, 10)
			if !errors.Is(err, store.ErrInvalidQuery) {
				t.Fatalf("Changes: %v, want store.ErrInvalidQuery", err)
			}
		})
	}
}

// Reading positions are their own stream, paged independently of marks.
func TestChangesPagesThroughReadingPositions(t *testing.T) {
	p := testPool(t)
	userID, first := annotationFixture(t, p, domain.FormatEPUB)
	files := append([]uuid.UUID{first}, syncShelfForStreams(t, p, userID, 2)...)
	since := time.Now().Add(-time.Minute)

	for i, id := range files {
		percent := float32(i+1) / 10
		if err := p.MergePositions(t.Context(), userID, []PushedPosition{{
			BookFileID: id,
			Position:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: i + 2, ElementPath: "p[3]", CharOffset: 7},
			Percent:    &percent,
			ReadAt:     time.Now(),
		}}); err != nil {
			t.Fatalf("MergePositions: %v", err)
		}
	}

	page, err := p.Changes(t.Context(), userID, since, "", 2)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(page.Progress) != 2 || !page.More || page.Cursor == "" {
		t.Fatalf("first page: %d positions, More=%v, cursor %q; want 2, true, a cursor",
			len(page.Progress), page.More, page.Cursor)
	}
	rest, err := p.Changes(t.Context(), userID, since, page.Cursor, 2)
	if err != nil {
		t.Fatalf("Changes (second page): %v", err)
	}
	if len(rest.Progress) != 1 || rest.More || rest.Cursor != "" {
		t.Fatalf("second page: %d positions, More=%v, cursor %q; want 1, false, none",
			len(rest.Progress), rest.More, rest.Cursor)
	}
	if !rest.Until.Equal(page.Until) {
		t.Errorf("Until moved within a round: %v then %v", page.Until, rest.Until)
	}

	seen := map[uuid.UUID]domain.ReadingProgress{}
	for _, rp := range append(page.Progress, rest.Progress...) {
		if _, dup := seen[rp.BookFileID]; dup {
			t.Errorf("position for %s delivered twice", rp.BookFileID)
		}
		seen[rp.BookFileID] = rp
	}
	for i, id := range files {
		rp, ok := seen[id]
		if !ok {
			t.Errorf("position for file %d never delivered", i)
			continue
		}
		if rp.UserID != userID || rp.Position.Format != domain.FormatEPUB ||
			rp.Position.SpineIndex != i+2 || rp.Position.ElementPath != "p[3]" || rp.Position.CharOffset != 7 {
			t.Errorf("file %d position = %+v", i, rp.Position)
		}
		if rp.Furthest.SpineIndex != i+2 || rp.Percent == nil || *rp.Percent != float32(i+1)/10 {
			t.Errorf("file %d furthest = %+v, percent %v", i, rp.Furthest, rp.Percent)
		}
	}

	// A limit out of range is the default page, which holds all three.
	all, err := p.Changes(t.Context(), userID, since, "", 0)
	if err != nil {
		t.Fatalf("Changes (default limit): %v", err)
	}
	if len(all.Progress) != 3 || all.More {
		t.Errorf("default limit: %d positions, More=%v; want 3 and no more", len(all.Progress), all.More)
	}
}

// Deletions page the same way, so a device away for a big clear-out still
// hears about every one of them.
func TestChangesPagesThroughDeletions(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	since := time.Now().Add(-time.Minute)

	deleted := map[uuid.UUID]bool{}
	for i := 0; i < 3; i++ {
		id := uuid.New()
		out, err := p.ApplyMarks(t.Context(), userID, []PushedMark{{ID: id, BookFileID: bookFileID, Deleted: true}})
		if err != nil {
			t.Fatalf("ApplyMarks: %v", err)
		}
		if len(out) != 1 || out[0].Status != MarkApplied {
			t.Fatalf("deletion outcome = %+v", out)
		}
		deleted[id] = true
	}

	page, err := p.Changes(t.Context(), userID, since, "", 2)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(page.Deleted) != 2 || !page.More {
		t.Fatalf("first page: %d deletions, More=%v; want 2 and more", len(page.Deleted), page.More)
	}
	rest, err := p.Changes(t.Context(), userID, since, page.Cursor, 2)
	if err != nil {
		t.Fatalf("Changes (second page): %v", err)
	}
	if len(rest.Deleted) != 1 || rest.More {
		t.Fatalf("second page: %d deletions, More=%v; want 1 and no more", len(rest.Deleted), rest.More)
	}
	for _, d := range append(page.Deleted, rest.Deleted...) {
		if !deleted[d.AnnotationID] || d.BookFileID != bookFileID {
			t.Errorf("unexpected tombstone %+v", d)
		}
		delete(deleted, d.AnnotationID)
	}
	if len(deleted) != 0 {
		t.Errorf("%d deletions never delivered", len(deleted))
	}
}

// A client that sends no clock of its own gets the server's.
func TestAnUntimedPushIsStampedNow(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	id := uuid.New()
	out, err := p.ApplyMarks(t.Context(), userID, []PushedMark{pushedHighlight(id, bookFileID, "untimed", time.Time{})})
	if err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}
	if len(out) != 1 || out[0].Status != MarkApplied {
		t.Fatalf("outcome = %+v", out)
	}
	changes, err := p.Changes(t.Context(), userID, time.Now().Add(-time.Minute), "", 10)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes.Annotations) != 1 || time.Since(changes.Annotations[0].EditedAt) > time.Minute {
		t.Fatalf("annotations = %+v, want one edited just now", changes.Annotations)
	}

	if err := p.MergePositions(t.Context(), userID, []PushedPosition{{
		BookFileID: bookFileID,
		Position:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1, ElementPath: "p[1]"},
	}}); err != nil {
		t.Fatalf("MergePositions: %v", err)
	}
	got, err := p.ReadingProgress(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("ReadingProgress: %v", err)
	}
	if time.Since(got.LastReadAt) > time.Minute {
		t.Errorf("last read %v, want now", got.LastReadAt)
	}
}

func TestMergePositionsRefusesAnUnanchoredPosition(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	err := p.MergePositions(t.Context(), userID, []PushedPosition{{
		BookFileID: bookFileID,
		Position:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1},
		ReadAt:     time.Now(),
	}})
	if !errors.Is(err, domain.ErrInvalidLocator) {
		t.Fatalf("MergePositions: %v, want domain.ErrInvalidLocator", err)
	}
	if _, err := p.ReadingProgress(t.Context(), userID, bookFileID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused position was stored: %v", err)
	}
}

// A book unshared while the phone was away is skipped, not an error, and the
// rest of the batch still lands.
func TestMergePositionsSkipsAFileThatIsNoLongerReadable(t *testing.T) {
	p := testPool(t)
	owner, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	stranger := secondUser(t, p)
	theirs := syncShelfForStreams(t, p, stranger, 1)[0]
	_ = owner

	err := p.MergePositions(t.Context(), stranger, []PushedPosition{
		{BookFileID: bookFileID, Position: domain.Locator{Format: domain.FormatEPUB, SpineIndex: 4, ElementPath: "p[1]"}, ReadAt: time.Now()},
		{BookFileID: theirs, Position: domain.Locator{Format: domain.FormatEPUB, SpineIndex: 2, ElementPath: "p[1]"}, ReadAt: time.Now()},
	})
	if err != nil {
		t.Fatalf("MergePositions: %v", err)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM reading_progress WHERE user_id = $1 AND book_file_id = $2`, stranger, bookFileID); n != 0 {
		t.Error("a position was stored on a book the person cannot open")
	}
	got, err := p.ReadingProgress(t.Context(), stranger, theirs)
	if err != nil {
		t.Fatalf("ReadingProgress on their own book: %v", err)
	}
	if got.Position.SpineIndex != 2 {
		t.Errorf("their own position = %+v", got.Position)
	}
}

// A deletion of a book the person cannot open is refused per item, like an
// edit.
func TestADeletionOnAnUnreadableFileIsRefused(t *testing.T) {
	p := testPool(t)
	_, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	stranger := secondUser(t, p)

	id := uuid.New()
	out, err := p.ApplyMarks(t.Context(), stranger, []PushedMark{{ID: id, BookFileID: bookFileID, Deleted: true}})
	if err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}
	if len(out) != 1 || out[0].Status != MarkRefused {
		t.Fatalf("outcome = %+v, want refused", out)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM annotation_tombstones WHERE annotation_id = $1`, id); n != 0 {
		t.Error("a refused deletion still recorded a tombstone")
	}
}

// What the row did wrong is refused and the push goes on; what the server did
// wrong stops it.
func TestIsRefusableSeparatesTheRowsFaultFromTheServers(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"invalid query", fmt.Errorf("wrapped: %w", store.ErrInvalidQuery), true},
		{"unanchored locator", fmt.Errorf("wrapped: %w", domain.ErrInvalidLocator), true},
		{"somebody else's mark", errNotYourMark, true},
		{"check violation", &pgconn.PgError{Code: "23514"}, true},
		{"foreign key violation", fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "23503"}), true},
		{"serialisation failure", &pgconn.PgError{Code: "40001"}, false},
		{"connection lost", errors.New("connection reset"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRefusable(tc.err); got != tc.want {
				t.Errorf("isRefusable = %v, want %v", got, tc.want)
			}
		})
	}
}
