// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

func pushedHighlight(id, bookFileID uuid.UUID, note string, at time.Time) PushedMark {
	return PushedMark{
		ID:         id,
		BookFileID: bookFileID,
		Type:       domain.AnnotationHighlight,
		Range: domain.Range{
			Start: domain.Locator{Format: domain.FormatEPUB, SpineIndex: 3, ElementPath: "p[1]", CharOffset: 0},
			End:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 3, ElementPath: "p[1]", CharOffset: 40},
		},
		SelectedText: "a sentence worth keeping",
		Note:         note,
		UpdatedAt:    at,
	}
}

// A mark made with no signal has an id before any server sees it, and that id
// is what the whole protocol rests on.
func TestAMarkMadeOfflineArrivesWithItsOwnID(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	id := uuid.New()
	out, err := p.ApplyMarks(t.Context(), userID,
		[]PushedMark{pushedHighlight(id, bookFileID, "made on a train", time.Now())})
	if err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}
	if len(out) != 1 || out[0].Status != MarkApplied {
		t.Fatalf("outcomes = %+v, want one applied", out)
	}

	marks, err := p.Annotations(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 || marks[0].ID != id {
		t.Fatalf("stored %+v, want the client's own id %s", marks, id)
	}
}

// The connection that drops is the one carrying the response, so the same
// push arrives twice. Twice must mean once.
func TestPushingTheSameChangeTwiceChangesNothing(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	mark := pushedHighlight(uuid.New(), bookFileID, "once", time.Now())
	for i := 0; i < 3; i++ {
		if _, err := p.ApplyMarks(t.Context(), userID, []PushedMark{mark}); err != nil {
			t.Fatalf("ApplyMarks %d: %v", i, err)
		}
	}

	marks, err := p.Annotations(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 {
		t.Fatalf("three identical pushes made %d marks, want 1", len(marks))
	}
}

// Last write wins BY THE EDITOR'S CLOCK. A phone handing over a week-old edit
// must not undo what a laptop did yesterday.
func TestAnOlderEditDoesNotOverwriteANewerOne(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	id := uuid.New()

	recent := time.Now()
	if _, err := p.ApplyMarks(t.Context(), userID,
		[]PushedMark{pushedHighlight(id, bookFileID, "edited yesterday", recent)}); err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}

	stale := pushedHighlight(id, bookFileID, "edited last week", recent.Add(-7*24*time.Hour))
	out, err := p.ApplyMarks(t.Context(), userID, []PushedMark{stale})
	if err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}
	if out[0].Status != MarkStale {
		t.Errorf("status = %q, want %q", out[0].Status, MarkStale)
	}

	marks, _ := p.Annotations(t.Context(), userID, bookFileID)
	if len(marks) != 1 || marks[0].Note != "edited yesterday" {
		t.Fatalf("note = %q, want the newer edit to have survived", marks[0].Note)
	}
}

// A deleted mark that comes back from the dead is the failure people notice.
func TestADeletedMarkIsNotResurrectedByAnOfflineEdit(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	id := uuid.New()

	if _, err := p.ApplyMarks(t.Context(), userID,
		[]PushedMark{pushedHighlight(id, bookFileID, "doomed", time.Now())}); err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}
	if err := p.DeleteAnnotation(t.Context(), userID, id); err != nil {
		t.Fatalf("DeleteAnnotation: %v", err)
	}

	// A phone that never heard about the deletion pushes its copy back.
	if _, err := p.ApplyMarks(t.Context(), userID,
		[]PushedMark{pushedHighlight(id, bookFileID, "still here", time.Now())}); err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}

	marks, _ := p.Annotations(t.Context(), userID, bookFileID)
	if len(marks) != 0 {
		t.Fatalf("the deleted mark came back: %+v", marks)
	}
}

// Deleting in the browser has to reach the phone, which can only happen if
// the ordinary delete leaves a record.
func TestDeletingAMarkIsReportedToAClientThatWasAway(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	id := uuid.New()

	if _, err := p.ApplyMarks(t.Context(), userID,
		[]PushedMark{pushedHighlight(id, bookFileID, "gone soon", time.Now())}); err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}
	before := time.Now()
	if err := p.DeleteAnnotation(t.Context(), userID, id); err != nil {
		t.Fatalf("DeleteAnnotation: %v", err)
	}

	changes, err := p.Changes(t.Context(), userID, before, "", 100)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes.Deleted) != 1 || changes.Deleted[0].AnnotationID != id {
		t.Fatalf("deletions = %+v, want the one that happened", changes.Deleted)
	}
	if changes.Until.IsZero() {
		t.Error("no server clock came back, so a client has no next cursor")
	}
}

// A pull is scoped to the moment asked for, or a phone re-downloads a
// library's worth of marks every time it syncs.
func TestAPullOnlyCarriesWhatChanged(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	if _, err := p.ApplyMarks(t.Context(), userID,
		[]PushedMark{pushedHighlight(uuid.New(), bookFileID, "old news", time.Now())}); err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}

	// Far enough ahead to clear the overlap window, which exists so a
	// committing transaction is never missed.
	after := time.Now().Add(time.Minute)
	changes, err := p.Changes(t.Context(), userID, after, "", 100)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes.Annotations) != 0 {
		t.Fatalf("a pull for the future carried %d marks", len(changes.Annotations))
	}
}

// Two facts, two rules. Where you are takes the newest write; how far you got
// only advances - which is what stops a laptop opening chapter one from
// erasing an evening's reading on a phone.
func TestAnOldPositionDoesNotRewindTheFurthestRead(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	// Percent is a fraction, not a number out of a hundred.
	ahead := float32(0.62)
	now := time.Now()
	if err := p.MergePositions(t.Context(), userID, []PushedPosition{{
		BookFileID: bookFileID,
		Position:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 9, ElementPath: "p[4]"},
		Percent:    &ahead,
		ReadAt:     now,
	}}); err != nil {
		t.Fatalf("MergePositions: %v", err)
	}

	// A laptop that was offline, reporting an older look at chapter one.
	behind := float32(0.04)
	if err := p.MergePositions(t.Context(), userID, []PushedPosition{{
		BookFileID: bookFileID,
		Position:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1, ElementPath: "p[1]"},
		Percent:    &behind,
		ReadAt:     now.Add(-time.Hour),
	}}); err != nil {
		t.Fatalf("MergePositions: %v", err)
	}

	got, err := p.ReadingProgress(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("ReadingProgress: %v", err)
	}
	if got.Furthest.SpineIndex != 9 {
		t.Errorf("furthest = spine %d, want 9: an old device rewound how far they got", got.Furthest.SpineIndex)
	}
	if got.Position.SpineIndex != 9 {
		t.Errorf("position = spine %d, want 9: an older reading overwrote a newer one", got.Position.SpineIndex)
	}
}

// Merging offline positions goes through the same Go-side comparison
// SaveProgress does (furthestFor), not a second one written in SQL - the bug
// that let "p[10]" sort before "p[9]" as text and stop "how far they got"
// from advancing across a double-digit paragraph.
func TestMergedPositionsAdvanceFurthestAcrossDoubleDigitParagraphs(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	now := time.Now()

	if err := p.MergePositions(t.Context(), userID, []PushedPosition{{
		BookFileID: bookFileID,
		Position:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 7, ElementPath: "p[9]", CharOffset: 300},
		ReadAt:     now,
	}}); err != nil {
		t.Fatalf("MergePositions: %v", err)
	}
	if err := p.MergePositions(t.Context(), userID, []PushedPosition{{
		BookFileID: bookFileID,
		Position:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 7, ElementPath: "p[10]", CharOffset: 5},
		ReadAt:     now.Add(time.Minute),
	}}); err != nil {
		t.Fatalf("MergePositions: %v", err)
	}

	got, err := p.ReadingProgress(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("ReadingProgress: %v", err)
	}
	if got.Furthest.ElementPath != "p[10]" {
		t.Errorf("furthest = %q, want p[10]: paging from p[9] to p[10] did not advance it", got.Furthest.ElementPath)
	}
}

// And the newest position does move, backwards included, because turning back
// a chapter is reading.
func TestANewerPositionMovesEvenBackwards(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	now := time.Now()

	for _, step := range []struct {
		spine int
		at    time.Time
	}{{9, now.Add(-time.Hour)}, {2, now}} {
		if err := p.MergePositions(t.Context(), userID, []PushedPosition{{
			BookFileID: bookFileID,
			Position:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: step.spine, ElementPath: "p[1]"},
			ReadAt:     step.at,
		}}); err != nil {
			t.Fatalf("MergePositions: %v", err)
		}
	}

	got, _ := p.ReadingProgress(t.Context(), userID, bookFileID)
	if got.Position.SpineIndex != 2 {
		t.Errorf("position = spine %d, want 2: turning back a chapter is reading", got.Position.SpineIndex)
	}
	if got.Furthest.SpineIndex != 9 {
		t.Errorf("furthest = spine %d, want 9", got.Furthest.SpineIndex)
	}
}

// A book somebody cannot open is not a book they can push marks into, and one
// refusal must not discard the rest of what a phone carried back.
func TestARefusedBookDoesNotDiscardTheOtherChanges(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	out, err := p.ApplyMarks(t.Context(), userID, []PushedMark{
		pushedHighlight(uuid.New(), uuid.New(), "somebody else's book", time.Now()),
		pushedHighlight(uuid.New(), bookFileID, "mine", time.Now()),
	})
	if err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("outcomes = %+v, want two", out)
	}
	if out[0].Status != MarkRefused {
		t.Errorf("first status = %q, want %q", out[0].Status, MarkRefused)
	}
	if out[1].Status != MarkApplied {
		t.Errorf("second status = %q, want %q: one refusal discarded the rest", out[1].Status, MarkApplied)
	}
}

// The bug this exists to prevent: a mark edited offline on Monday and pushed on
// Friday was stored with Friday's timestamp, because a trigger stamped now() on
// every update regardless of what the statement said.
//
// Two things broke. Last-write-wins compared ARRIVAL instead of authorship, so
// a newer edit made elsewhere lost to an older one that arrived later and was
// told it was stale. And a pull asks for changes after a moment, so a mark
// written with its true Monday timestamp was already older than every other
// device's cursor and was never sent to any of them.
func TestAnOfflineEditKeepsTheTimeItWasMade(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	id := uuid.New()

	// Created a week ago, pushed now.
	madeAt := time.Now().Add(-7 * 24 * time.Hour).Truncate(time.Millisecond)
	if _, err := p.ApplyMarks(t.Context(), userID,
		[]PushedMark{pushedHighlight(id, bookFileID, "made a week ago", madeAt)}); err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}

	// Edited two days ago, also pushed now: this is the UPDATE path, which is
	// where the trigger used to overwrite the timestamp.
	editedAt := time.Now().Add(-2 * 24 * time.Hour).Truncate(time.Millisecond)
	if _, err := p.ApplyMarks(t.Context(), userID,
		[]PushedMark{pushedHighlight(id, bookFileID, "edited two days ago", editedAt)}); err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}

	// Two timestamps, two questions. edited_at is when the person made the
	// change and is what last-write-wins compares; updated_at is when this
	// server heard about it and is what a pull cursor reads. One column cannot
	// answer both, which is the whole of migration 28.
	var madeAtStored, heardAt time.Time
	if err := p.QueryRow(t.Context(),
		`SELECT edited_at, updated_at FROM annotations WHERE id = $1`, id,
	).Scan(&madeAtStored, &heardAt); err != nil {
		t.Fatalf("reading the mark: %v", err)
	}
	if delta := madeAtStored.Sub(editedAt); delta > time.Second || delta < -time.Second {
		t.Fatalf("edited_at = %s, want the moment the edit was MADE (%s)", madeAtStored, editedAt)
	}
	if time.Since(heardAt) > time.Minute {
		t.Fatalf("updated_at = %s, want roughly now: a device that syncs after this "+
			"push must be told about it, and it asks by that column", heardAt)
	}

	// And an edit made BEFORE that one must still lose, which is the whole
	// point of keeping the real time.
	older := time.Now().Add(-5 * 24 * time.Hour)
	out, err := p.ApplyMarks(t.Context(), userID,
		[]PushedMark{pushedHighlight(id, bookFileID, "made five days ago", older)})
	if err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}
	if out[0].Status != MarkStale {
		t.Errorf("status = %q, want %q: an older edit must not win", out[0].Status, MarkStale)
	}

	var note string
	if err := p.QueryRow(t.Context(),
		`SELECT note FROM annotations WHERE id = $1`, id).Scan(&note); err != nil {
		t.Fatalf("reading the note: %v", err)
	}
	if note != "edited two days ago" {
		t.Errorf("note = %q, want the newest edit to have survived", note)
	}
}

// An ordinary edit made here is still stamped by the server, which is what the
// trigger is for everywhere else.
func TestAnEditMadeHereIsStampedNow(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	mark, err := p.CreateAnnotation(t.Context(), domain.Annotation{
		UserID:     userID,
		BookFileID: bookFileID,
		Type:       domain.AnnotationHighlight,
		Range: domain.Range{
			Start: domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1, ElementPath: "p[1]"},
			End:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 1, ElementPath: "p[1]", CharOffset: 10},
		},
		// A highlight without its text is refused, and rightly: the snapshot is
		// what survives the anchor going stale.
		SelectedText: "ten chars.",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	updated, err := p.UpdateAnnotation(t.Context(), userID, mark.ID, "a note", "")
	if err != nil {
		t.Fatalf("UpdateAnnotation: %v", err)
	}
	if time.Since(updated.UpdatedAt) > time.Minute {
		t.Errorf("updated_at = %s, want roughly now", updated.UpdatedAt)
	}
}

// A week-old edit pushed today must still be delivered to every other device.
//
// This is the failure migration 27 introduced while fixing a different one.
// Honouring the client's clock made last-write-wins correct and the pull
// cursor wrong: a mark made on Monday and pushed on Friday was stamped Monday,
// which is older than every other device's cursor, so none of them were ever
// told about it. Not a delay - never, short of a full resync.
func TestAWeekOldEditStillReachesTheOtherDevices(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	// Another device synced three days ago and holds that as its cursor.
	cursor := time.Now().Add(-3 * 24 * time.Hour)

	// A phone pushes a highlight it made a week ago - before that cursor.
	madeAt := time.Now().Add(-7 * 24 * time.Hour)
	id := uuid.New()
	if _, err := p.ApplyMarks(t.Context(), userID,
		[]PushedMark{pushedHighlight(id, bookFileID, "made on a train", madeAt)}); err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}

	changes, err := p.Changes(t.Context(), userID, cursor, "", 100)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	var delivered bool
	for _, a := range changes.Annotations {
		if a.ID == id {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("a mark pushed today was not delivered to a device whose cursor is three days old; " +
			"it would never be delivered to it at all")
	}

	// And a deletion made a week ago and pushed today, likewise.
	before := time.Now()
	if _, err := p.ApplyMarks(t.Context(), userID, []PushedMark{{
		ID: id, BookFileID: bookFileID, Deleted: true, UpdatedAt: madeAt,
	}}); err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}

	changes, err = p.Changes(t.Context(), userID, before, "", 100)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes.Deleted) != 1 || changes.Deleted[0].AnnotationID != id {
		t.Fatalf("deletions = %+v, want the one just pushed", changes.Deleted)
	}
}

// One malformed mark must not discard the rest of the push.
//
// ApplyMarks documents refusals as per-item, and then returned an error on any
// constraint violation - which the handler turns into a 500. The client
// re-sends the same batch, hits the same row again, and forty good changes
// from a week offline never land at all.
func TestOneBadMarkDoesNotDiscardTheBatch(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	bad := pushedHighlight(uuid.New(), bookFileID, "", time.Now())
	bad.SelectedText = "" // a highlight with no text is refused by the schema

	good := pushedHighlight(uuid.New(), bookFileID, "worth keeping", time.Now())

	out, err := p.ApplyMarks(t.Context(), userID, []PushedMark{bad, good})
	if err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("outcomes = %+v, want one per mark", out)
	}
	if out[0].Status != MarkRefused {
		t.Errorf("the malformed mark reported %q, want %q", out[0].Status, MarkRefused)
	}
	if out[1].Status != MarkApplied {
		t.Errorf("the good mark reported %q; one bad row discarded the batch", out[1].Status)
	}

	marks, err := p.Annotations(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 {
		t.Errorf("stored %d marks, want the good one", len(marks))
	}
}

// A highlight running from the 9th paragraph to the 10th runs forwards, but
// "p[10]" used to sort before "p[9]" as a string, so both Range.Validate and
// the whole push failed - and the one bad-looking mark took a genuinely good
// one down with it, because the old error was not recognised as refusable and
// bubbled up as a request-level failure rather than a per-mark one.
func TestAPushedHighlightCanSpanDoubleDigitParagraphs(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	mark := PushedMark{
		ID:         uuid.New(),
		BookFileID: bookFileID,
		Type:       domain.AnnotationHighlight,
		Range: domain.Range{
			Start: domain.Locator{Format: domain.FormatEPUB, SpineIndex: 0, ElementPath: "p[9]", CharOffset: 0},
			End:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 0, ElementPath: "p[10]", CharOffset: 5},
		},
		SelectedText: "runs from the ninth paragraph into the tenth",
		UpdatedAt:    time.Now(),
	}

	out, err := p.ApplyMarks(t.Context(), userID, []PushedMark{mark})
	if err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}
	if len(out) != 1 || out[0].Status != MarkApplied {
		t.Fatalf("outcomes = %+v, want one applied", out)
	}

	marks, err := p.Annotations(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 {
		t.Fatalf("stored %d marks, want the one that spans p[9] to p[10]", len(marks))
	}
}

// A push carrying one mark with a genuinely backwards range alongside a good
// one must store the good one and report the bad one refused - not 500 the
// whole request, and not silently drop the good mark either.
func TestAGenuinelyBackwardsMarkIsRefusedNotFatal(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	backwards := PushedMark{
		ID:         uuid.New(),
		BookFileID: bookFileID,
		Type:       domain.AnnotationHighlight,
		Range: domain.Range{
			// Same tag both ends, so the order is decidable - and it runs the
			// wrong way.
			Start: domain.Locator{Format: domain.FormatEPUB, SpineIndex: 0, ElementPath: "p[10]", CharOffset: 5},
			End:   domain.Locator{Format: domain.FormatEPUB, SpineIndex: 0, ElementPath: "p[9]", CharOffset: 0},
		},
		SelectedText: "this one really does run backwards",
		UpdatedAt:    time.Now(),
	}
	good := pushedHighlight(uuid.New(), bookFileID, "worth keeping", time.Now())

	out, err := p.ApplyMarks(t.Context(), userID, []PushedMark{backwards, good})
	if err != nil {
		t.Fatalf("ApplyMarks: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("outcomes = %+v, want one per mark", out)
	}
	if out[0].Status != MarkRefused {
		t.Errorf("backwards mark status = %q, want %q", out[0].Status, MarkRefused)
	}
	if out[1].Status != MarkApplied {
		t.Errorf("good mark status = %q, want %q: a backwards mark discarded the batch", out[1].Status, MarkApplied)
	}

	marks, err := p.Annotations(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 {
		t.Errorf("stored %d marks, want only the good one", len(marks))
	}
}

// A mark's id is client-generated and travels on the wire in plain sight - the
// per-book list hands it out to everyone a highlight is shared with. Someone
// who can merely READ a shared highlight must not be able to push a
// "deletion" of its id and erase it for its actual owner, and must not be
// able to plant a tombstone that then blocks the owner's own future edits of
// their own mark.
// A deletion of somebody else's mark is a no-op reported as "applied" -
// exactly like deleting an id that never existed at all - rather than a
// distinguishable "refused". Refusing it distinguishably would make this
// endpoint an oracle: probe an id you once saw in a shared list and learn,
// from the status alone, whether it is still somebody's mark. A deletion has
// no content to lose track of, so there is nothing wrong with pretending it
// worked.
func TestPushingADeletionOfSomeoneElsesMarkIsANoOp(t *testing.T) {
	p := testPool(t)
	owner, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	stranger := secondUser(t, p)

	// The owner shares the book, and their annotations on it, with the
	// stranger - which is how a real attacker would learn the mark's id.
	if err := p.ShareBook(t.Context(), bookFileID, owner, stranger, false, true); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	id := uuid.New()
	if _, err := p.ApplyMarks(t.Context(), owner,
		[]PushedMark{pushedHighlight(id, bookFileID, "the owner's own highlight", time.Now())}); err != nil {
		t.Fatalf("ApplyMarks (owner creates): %v", err)
	}

	// The stranger pushes a deletion of the owner's mark id.
	out, err := p.ApplyMarks(t.Context(), stranger, []PushedMark{
		{ID: id, BookFileID: bookFileID, Deleted: true, UpdatedAt: time.Now()},
	})
	if err != nil {
		t.Fatalf("ApplyMarks (stranger deletes): %v", err)
	}
	if len(out) != 1 || out[0].Status != MarkApplied {
		t.Fatalf("stranger's deletion = %+v, want applied (a silent no-op, indistinguishable from deleting nothing)", out)
	}

	// The mark survives, unaffected.
	marks, err := p.Annotations(t.Context(), owner, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 || marks[0].ID != id {
		t.Fatalf("the owner's mark did not survive a stranger's deletion push: %+v", marks)
	}

	// And the owner's own later edit of it still applies - the stranger's
	// no-op must not have left any trace that could freeze it.
	out, err = p.ApplyMarks(t.Context(), owner,
		[]PushedMark{pushedHighlight(id, bookFileID, "edited afterwards", time.Now().Add(time.Minute))})
	if err != nil {
		t.Fatalf("ApplyMarks (owner edits): %v", err)
	}
	if len(out) != 1 || out[0].Status != MarkApplied {
		t.Fatalf("the owner's own edit = %+v, want applied", out)
	}

	marks, err = p.Annotations(t.Context(), owner, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	if len(marks) != 1 || marks[0].Note != "edited afterwards" {
		t.Fatalf("the owner's edit did not stick: %+v", marks)
	}
}

// The hijack this exists to prevent: once A deletes a mark, its id is gone
// from the live table - and a first version of the ownership check only
// looked there, so it answered "nobody owns this id" and let B, who once saw
// it in a shared list, push an EDIT of that same id and become its new owner.
// A's own devices would then find the id owned by a stranger and have their
// own later, legitimate pushes of it refused. A tombstone is as much a claim
// on an id as a live row.
func TestPushingAnEditOfADeletedMarksIdIsRefused(t *testing.T) {
	p := testPool(t)
	owner, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	stranger := secondUser(t, p)

	if err := p.ShareBook(t.Context(), bookFileID, owner, stranger, false, true); err != nil {
		t.Fatalf("ShareBook: %v", err)
	}

	id := uuid.New()
	if _, err := p.ApplyMarks(t.Context(), owner,
		[]PushedMark{pushedHighlight(id, bookFileID, "seen, then deleted", time.Now())}); err != nil {
		t.Fatalf("ApplyMarks (owner creates): %v", err)
	}
	if _, err := p.ApplyMarks(t.Context(), owner, []PushedMark{
		{ID: id, BookFileID: bookFileID, Deleted: true, UpdatedAt: time.Now()},
	}); err != nil {
		t.Fatalf("ApplyMarks (owner deletes): %v", err)
	}

	// The stranger, who saw the id before it was deleted, tries to claim it.
	out, err := p.ApplyMarks(t.Context(), stranger,
		[]PushedMark{pushedHighlight(id, bookFileID, "the stranger's attempt to steal it", time.Now())})
	if err != nil {
		t.Fatalf("ApplyMarks (stranger edits): %v", err)
	}
	if len(out) != 1 || out[0].Status != MarkRefused {
		t.Fatalf("stranger's edit of a deleted id = %+v, want refused", out)
	}

	// Nothing was created under the stranger's ownership.
	if marks, err := p.Annotations(t.Context(), stranger, bookFileID); err != nil {
		t.Fatalf("Annotations: %v", err)
	} else if len(marks) != 0 {
		t.Fatalf("the stranger's hijack attempt created %+v", marks)
	}

	// And the owner's own devices are not locked out of the id they deleted.
	out, err = p.ApplyMarks(t.Context(), owner,
		[]PushedMark{pushedHighlight(id, bookFileID, "the owner, offline, never heard of the deletion", time.Now())})
	if err != nil {
		t.Fatalf("ApplyMarks (owner re-pushes): %v", err)
	}
	// The owner's own tombstone still applies: their own device resurrecting
	// a mark they themselves deleted is the documented "not resurrected"
	// case, reported stale, and is not what this test is about - the point is
	// only that it is REFUSED to nobody but the actual owner.
	if len(out) != 1 || out[0].Status == MarkRefused {
		t.Fatalf("the owner's own push of their own deleted id = %+v, want anything but refused", out)
	}
}

// A database from before deletions checked ownership at all could hold a
// tombstone naming the WRONG person for a mark that is still live and
// correctly owned by somebody else: applyDeletion's old, unchecked version
// scoped its DELETE by user_id - so it left a live mark it did not own
// untouched - but the tombstone INSERT that followed it was not scoped at
// all, and wrote under the wrong name regardless. annotationOwner must
// prefer the live row's real owner over that corrupt tombstone, or the
// actual owner's own later edits of their own mark would be refused by data
// that reflects a bug, not a fact.
func TestOwnershipPrefersTheLiveRowOverAForeignTombstone(t *testing.T) {
	p := testPool(t)
	owner, bookFileID := annotationFixture(t, p, domain.FormatEPUB)
	stranger := secondUser(t, p)

	id := uuid.New()
	if _, err := p.ApplyMarks(t.Context(), owner,
		[]PushedMark{pushedHighlight(id, bookFileID, "the real owner's mark", time.Now())}); err != nil {
		t.Fatalf("ApplyMarks (owner creates): %v", err)
	}

	// Seeded directly, bypassing every write path this package actually
	// offers: none of them can produce this state any more, which is exactly
	// why it has to be reproduced by hand to prove the read side is not
	// fooled by it regardless.
	if _, err := p.Exec(t.Context(), `
		INSERT INTO annotation_tombstones (annotation_id, user_id, book_file_id)
		VALUES ($1, $2, $3)`, id, stranger, bookFileID); err != nil {
		t.Fatalf("seeding a corrupt tombstone: %v", err)
	}

	out, err := p.ApplyMarks(t.Context(), owner,
		[]PushedMark{pushedHighlight(id, bookFileID, "the owner's own edit", time.Now().Add(time.Minute))})
	if err != nil {
		t.Fatalf("ApplyMarks (owner edits): %v", err)
	}
	if len(out) != 1 || out[0].Status != MarkApplied {
		t.Fatalf("the real owner's own edit = %+v, want applied: a foreign tombstone must not shadow "+
			"the live row's real owner", out)
	}
}

// forceAnnotationTimestamps overwrites updated_at directly, bypassing the
// trigger that otherwise stamps it with now() on every UPDATE. Real bursts
// that share one instant - a bulk import inside one transaction - are not
// practical to reproduce by racing wall-clock writes in a test, and sleeping
// to spread them out would make the test slow and flaky: this gets the same shape
// deterministically.
func forceAnnotationTimestamps(t *testing.T, p *Pool, userID uuid.UUID, at time.Time) {
	t.Helper()
	ctx := t.Context()
	if _, err := p.Exec(ctx, `ALTER TABLE annotations DISABLE TRIGGER annotations_set_updated_at`); err != nil {
		t.Fatalf("disabling the updated_at trigger: %v", err)
	}
	defer func() {
		if _, err := p.Exec(ctx, `ALTER TABLE annotations ENABLE TRIGGER annotations_set_updated_at`); err != nil {
			t.Fatalf("re-enabling the updated_at trigger: %v", err)
		}
	}()
	if _, err := p.Exec(ctx, `UPDATE annotations SET updated_at = $1 WHERE user_id = $2`, at, userID); err != nil {
		t.Fatalf("forcing timestamps: %v", err)
	}
}

// pageThroughChanges drains a sync pull to completion, following Cursor
// rather than re-deriving `since`, and fails the test rather than looping
// forever if More never clears.
func pageThroughChanges(t *testing.T, p *Pool, userID uuid.UUID, since time.Time, limit int) []domain.Annotation {
	t.Helper()
	var all []domain.Annotation
	cursor := ""
	for page := 0; ; page++ {
		if page > 50 {
			t.Fatalf("more than 50 pages paging through changes; paging is not making progress")
		}
		changes, err := p.Changes(t.Context(), userID, since, cursor, limit)
		if err != nil {
			t.Fatalf("Changes (page %d): %v", page, err)
		}
		all = append(all, changes.Annotations...)
		if !changes.More {
			if changes.Cursor != "" {
				t.Errorf("More is false but Cursor = %q, want empty", changes.Cursor)
			}
			return all
		}
		if changes.Cursor == "" {
			t.Fatalf("More is true but Cursor is empty; the client has no way to ask for the next page")
		}
		if changes.Cursor == cursor {
			t.Fatalf("page %d returned the same cursor it was given; paging cannot advance", page)
		}
		cursor = changes.Cursor
	}
}

// The bug: a client that saw More and asked again with the SAME `since` got
// the same first page back forever, because nothing about the request had
// moved. A bulk import can write hundreds of rows sharing one exact
// updated_at - one INSERT statement, one transaction, one now() - and a
// timestamp-only resume point has no way to ask for "the rest of this
// instant". The cursor's id tiebreaker is what makes that answerable.
func TestSyncPullPagesThroughASharedTimestampWithNoGapsOrRepeats(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	const total = 23
	ids := make(map[uuid.UUID]bool, total)
	for i := 0; i < total; i++ {
		id := uuid.New()
		if _, err := p.ApplyMarks(t.Context(), userID,
			[]PushedMark{pushedHighlight(id, bookFileID, "from one big import", time.Now())}); err != nil {
			t.Fatalf("ApplyMarks %d: %v", i, err)
		}
		ids[id] = true
	}

	// Collapse every row onto the exact same instant, the way one
	// transaction's worth of imported marks would arrive.
	sharedInstant := time.Now().Truncate(time.Microsecond)
	forceAnnotationTimestamps(t, p, userID, sharedInstant)

	// A limit well under the batch size, so this cannot finish in one page.
	all := pageThroughChanges(t, p, userID, sharedInstant.Add(-time.Minute), 5)

	if len(all) != total {
		t.Fatalf("paging returned %d annotations, want %d", len(all), total)
	}
	seen := make(map[uuid.UUID]bool, total)
	for _, a := range all {
		if seen[a.ID] {
			t.Errorf("annotation %s was returned more than once", a.ID)
		}
		seen[a.ID] = true
		if !ids[a.ID] {
			t.Errorf("annotation %s was not one that was pushed", a.ID)
		}
	}
}

// The bug's other half: the server always re-reads a couple of seconds before
// whatever `since` it is given, so a client working around the first bug by
// advancing `since` to its last row's own timestamp would silently walk back
// into rows it had already seen - or, if enough of them shared that instant,
// never get past it at all. Paging with a real cursor does not re-derive
// `since` between pages, so the overlap window - which only ever applies once,
// at the very start of a round - cannot reopen mid-round and trap it.
func TestSyncPullCursorIsNotTrappedByTheOverlapWindow(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	// Two clusters a second and a half apart - inside the 2-second overlap
	// window a fresh round's `from` is computed from - each large enough on
	// its own to need more than one page.
	early := time.Now().Truncate(time.Microsecond)
	late := early.Add(1500 * time.Millisecond)

	ids := make(map[uuid.UUID]bool, 16)
	push := func(n int, at time.Time) {
		for i := 0; i < n; i++ {
			id := uuid.New()
			if _, err := p.ApplyMarks(t.Context(), userID,
				[]PushedMark{pushedHighlight(id, bookFileID, "clustered", time.Now())}); err != nil {
				t.Fatalf("ApplyMarks: %v", err)
			}
			ids[id] = true
		}
	}
	push(8, early)
	push(8, late)

	if _, err := p.Exec(t.Context(),
		`ALTER TABLE annotations DISABLE TRIGGER annotations_set_updated_at`); err != nil {
		t.Fatalf("disabling the updated_at trigger: %v", err)
	}
	// Stamp the two clusters individually - forceAnnotationTimestamps sets one
	// instant for the whole user, which would collapse both clusters into one.
	all, err := p.Annotations(t.Context(), userID, bookFileID)
	if err != nil {
		t.Fatalf("Annotations: %v", err)
	}
	for i, a := range all {
		at := early
		if i%2 == 0 {
			at = late
		}
		if _, err := p.Exec(t.Context(),
			`UPDATE annotations SET updated_at = $1 WHERE id = $2`, at, a.ID); err != nil {
			t.Fatalf("stamping annotation %s: %v", a.ID, err)
		}
	}
	if _, err := p.Exec(t.Context(),
		`ALTER TABLE annotations ENABLE TRIGGER annotations_set_updated_at`); err != nil {
		t.Fatalf("re-enabling the updated_at trigger: %v", err)
	}

	got := pageThroughChanges(t, p, userID, early.Add(-time.Minute), 3)
	if len(got) != len(ids) {
		t.Fatalf("paging returned %d annotations, want %d", len(got), len(ids))
	}
	seen := make(map[uuid.UUID]bool, len(ids))
	for _, a := range got {
		if seen[a.ID] {
			t.Errorf("annotation %s was returned more than once", a.ID)
		}
		seen[a.ID] = true
	}
}

// The bug: a stream can reach its own tip on page one while More stays true
// for another stream, and every later page re-read now() as its own Until. A
// row stamped just before page one's last row, but not yet committed when
// page one ran, would then sit behind a keyset bound built from a LATER
// page's own, later now() - invisible to this round, and gone for good if the
// next round's `since` (the last page's Until) starts after it too. Carrying
// the FIRST page's Until through every later page of the same round, and
// only THEN letting the client store it, keeps that window open for the
// whole round rather than closing it early.
func TestSyncPullUntilIsFixedForTheWholeRound(t *testing.T) {
	p := testPool(t)
	userID, bookFileID := annotationFixture(t, p, domain.FormatEPUB)

	for i := 0; i < 7; i++ {
		if _, err := p.ApplyMarks(t.Context(), userID,
			[]PushedMark{pushedHighlight(uuid.New(), bookFileID, "one of several", time.Now())}); err != nil {
			t.Fatalf("ApplyMarks %d: %v", i, err)
		}
	}

	cursor := ""
	var firstUntil time.Time
	pages := 0
	for {
		pages++
		if pages > 20 {
			t.Fatalf("more than 20 pages; paging is not making progress")
		}
		changes, err := p.Changes(t.Context(), userID, time.Now().Add(-time.Minute), cursor, 2)
		if err != nil {
			t.Fatalf("Changes: %v", err)
		}
		if pages == 1 {
			firstUntil = changes.Until
			if firstUntil.IsZero() {
				t.Fatal("the first page returned a zero Until")
			}
		} else if !changes.Until.Equal(firstUntil) {
			t.Errorf("page %d Until = %s, want the first page's %s", pages, changes.Until, firstUntil)
		}
		if !changes.More {
			break
		}
		cursor = changes.Cursor
	}
	if pages < 2 {
		t.Fatalf("this round fit in one page; the test needs at least two to say anything")
	}
}
