// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// A note must not be able to push the passages out of the prompt.
//
// Passages are bounded when they are chunked; notes were bounded nowhere. The
// annotation endpoint takes 64KB and the reader stores the selected text with
// the mark, so highlighting a chapter and writing one line about it put
// sixteen thousand tokens of quoted book ahead of the question - and on a
// local model with an 8k window every retrieved passage falls out of the
// context while the answer still claims to be grounded.
func TestANoteCannotFillTheContextWindow(t *testing.T) {
	huge := strings.Repeat("the whole of chapter four ", 4000) // ~100KB

	notes := toPromptNotes([]postgres.FoundNote{{
		Title: "A Book",
		Annotation: domain.Annotation{
			SelectedText: huge,
			Note:         huge,
		},
	}}, 10)

	if len(notes) != 1 {
		t.Fatalf("notes = %d, want 1", len(notes))
	}
	if n := len([]rune(notes[0].Quote)); n > maxNoteQuote+1 {
		t.Errorf("quote is %d runes, past the %d bound", n, maxNoteQuote)
	}
	if n := len([]rune(notes[0].Text)); n > maxNoteText+1 {
		t.Errorf("note is %d runes, past the %d bound", n, maxNoteText)
	}
	if notes[0].Number != 11 {
		t.Errorf("number = %d, want the note numbered after the passages", notes[0].Number)
	}

	// A note of ordinary length is untouched.
	short := toPromptNotes([]postgres.FoundNote{{
		Title:      "A Book",
		Annotation: domain.Annotation{SelectedText: "a sentence", Note: "worth remembering"},
	}}, 0)
	if short[0].Quote != "a sentence" || short[0].Text != "worth remembering" {
		t.Errorf("a short note was altered: %+v", short[0])
	}
}
