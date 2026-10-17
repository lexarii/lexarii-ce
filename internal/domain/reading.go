// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"time"

	"github.com/google/uuid"
)

// AnnotationType distinguishes the marks a user can leave in a book.
type AnnotationType string

const (
	AnnotationHighlight AnnotationType = "highlight"
	AnnotationNote      AnnotationType = "note"
	AnnotationBookmark  AnnotationType = "bookmark"
)

// Annotation is a user mark anchored to a locator range.
type Annotation struct {
	ID         uuid.UUID      `json:"id"`
	UserID     uuid.UUID      `json:"userId"`
	BookFileID uuid.UUID      `json:"bookFileId"`
	Type       AnnotationType `json:"type"`
	Range      Range          `json:"range"`
	// SelectedText is the text as it read when the annotation was made. If the
	// anchor fails to resolve after a re-ingest, the annotation is shown with
	// this text in an orphaned state. A user's note is never silently dropped.
	SelectedText string     `json:"selectedText,omitempty"`
	Note         string     `json:"note,omitempty"`
	Colour       string     `json:"colour,omitempty"`
	OrphanedAt   *time.Time `json:"orphanedAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	// UpdatedAt is when THIS SERVER wrote the row - bumped by an edit, a sync
	// push, or re-ingest orphaning it. It is what a sync pull's cursor reads,
	// and it is not what decides which of two edits wins.
	UpdatedAt time.Time `json:"updatedAt"`
	// EditedAt is when the WINNING edit was made, by whichever device made
	// it. It is what last-write-wins actually compares (see ApplyMarks), and
	// it is what a client merging a pulled mark against an unsent local edit
	// of its own needs to read - UpdatedAt only says when this server last
	// heard about the row, not when the change it holds was made.
	EditedAt time.Time `json:"editedAt"`

	// Author is who made the mark, and Shared says it was somebody else. A
	// shared highlight must be visibly theirs: an unattributed one read months
	// later becomes a thing you think you wrote.
	Author string `json:"author,omitempty"`
	Shared bool   `json:"shared,omitempty"`
}

// Orphaned reports whether the anchor no longer resolves in the current text.
func (a Annotation) Orphaned() bool { return a.OrphanedAt != nil }

// ReadingProgress is where a user is in a BookFile, and how far they have got.
//
// These are deliberately two values. Where someone IS moves in both directions,
// because turning back a chapter is reading. How far they HAVE GOT only ever
// advances, so a device that read ahead is never rewound by one that did not.
// Collapsing them into a single number is what made deliberate backward
// navigation impossible to save.
type ReadingProgress struct {
	UserID     uuid.UUID `json:"userId"`
	BookFileID uuid.UUID `json:"bookFileId"`
	// Position is the truth, and is what the reader reopens at. Percent is a
	// rendering of a position and must never be used to restore one - it does
	// not survive a font-size change.
	Position Locator  `json:"position"`
	Percent  *float32 `json:"percent,omitempty"`
	// Furthest is the deepest position reached. It is what "62% read" means.
	Furthest        Locator   `json:"furthest"`
	FurthestPercent *float32  `json:"furthestPercent,omitempty"`
	LastReadAt      time.Time `json:"lastReadAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// FurtherPosition decides which of two positions represents more progress
// into the book. It is the one implementation of that question - the store's
// own save path (SaveProgress, MergePositions) and this package's own
// Furthest both call it, rather than each keeping their own idea of what
// "further" means, which is how they came to disagree in the first place.
//
// Structure decides first, via Locator.CompareDecided - the two positions are
// in the same spine item, or one spine index is simply later than the other.
// When the structure itself cannot say - different tag names, or one path a
// strict prefix of the other, see CompareDecided - the two PERCENTS decide
// instead, when both are available and actually differ.
//
// Percent, not recency, is the right fallback here, even though it is
// otherwise "display only" and never used to restore a position: a heading is
// saved before any paragraph under it is read, in EVERY session, so "chose
// whichever arrived more recently" is systematically wrong in the one case
// that matters most - the reader has just turned a page, saved the chapter's
// heading first, and every paragraph read after it compares undecided against
// that heading for as long as the reader stays in that chapter. A save near
// the end of one spine item and a save near the start of the next also
// compare undecided by structure alone (different, unrelated element paths),
// and percent is exactly the number that tells them apart correctly without
// needing to know anything about either document's internal shape.
//
// decided is false only when structure cannot tell AND at least one percent
// is missing or they are exactly equal - a genuine tie carries no more
// information than an absence does. Callers need their own answer for that
// case; see furthestFor and Furthest for what each considers next.
func FurtherPosition(a Locator, aPercent *float32, b Locator, bPercent *float32) (bIsFurther, decided bool) {
	if cmp, ok := a.CompareDecided(b); ok {
		return cmp < 0, true
	}
	if aPercent != nil && bPercent != nil && *aPercent != *bPercent {
		return *bPercent > *aPercent, true
	}
	return false, false
}

// Furthest returns whichever progress reached further into the book.
//
// Used when merging two records that were written without seeing each other -
// an offline client reconciling on reconnect.
//
// When FurtherPosition itself has no answer - neither structure nor percent
// can tell the two positions apart - the more recently written progress wins
// instead, because at that point recency actually is the best remaining
// signal for which of two things a person did more recently, rather than the
// systematically-wrong-at-the-worst-moment heuristic it would be as a FIRST
// resort.
func Furthest(a, b ReadingProgress) ReadingProgress {
	if bIsFurther, decided := FurtherPosition(a.Position, a.Percent, b.Position, b.Percent); decided {
		if bIsFurther {
			return b
		}
		return a
	}
	if b.LastReadAt.After(a.LastReadAt) {
		return b
	}
	return a
}
