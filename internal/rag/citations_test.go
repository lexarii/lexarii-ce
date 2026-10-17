// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"reflect"
	"strings"
	"testing"
)

func TestCitedNumbers(t *testing.T) {
	tests := []struct {
		name      string
		answer    string
		available int
		want      []int
	}{
		{"one", "A monoid is a set with an associative operation [1].", 5, []int{1}},
		{"several", "It is defined here [1] and used there [3].", 5, []int{1, 3}},
		{"adjacent", "Both books agree [1][3].", 5, []int{1, 3}},
		{"in order of first appearance", "See [3], then [1], then [3] again.", 5, []int{3, 1}},
		{"two digits", "As set out in [12].", 20, []int{12}},
		{"none", "The passages I found do not answer this.", 5, nil},

		// A model citing [17] when it was given ten passages has made something
		// up. A citation that points at nothing is worse than none, because the
		// reader would follow it.
		{"out of range", "As shown in [17].", 10, nil},
		{"zero", "As shown in [0].", 10, nil},
		{"mixed", "Real [2] and invented [99].", 10, []int{2}},

		// Books are full of brackets of their own.
		{"not a citation", "The array is indexed [i] and the note says [sic].", 5, nil},
		{"no passages offered", "Something [1].", 0, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CitedNumbers(tt.answer, tt.available); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CitedNumbers(%q, %d) = %v, want %v", tt.answer, tt.available, got, tt.want)
			}
		})
	}
}

// A model inventing citations is a sign the answer is drifting from its
// sources, which is exactly what this design exists to make visible.
func TestInventedCitations(t *testing.T) {
	got := InventedCitations("Real [2], invented [99], invented again [99], and [12].", 10)
	want := []int{99, 12}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("InventedCitations = %v, want %v", got, want)
	}

	if got := InventedCitations("All real [1][2].", 5); got != nil {
		t.Errorf("InventedCitations on a clean answer = %v", got)
	}
}

// Silence is not an answer. A personal library is finite, and "you have nothing
// on this" is a real answer to a real question.
func TestNothingFoundIsSaidPlainly(t *testing.T) {
	if NothingFound == "" {
		t.Fatal("the empty-result answer is empty")
	}
	for _, word := range []string{"library", "nothing"} {
		if !strings.Contains(strings.ToLower(NothingFound), word) {
			t.Errorf("the empty-result answer does not mention %q: %q", word, NothingFound)
		}
	}
	// It must not read as an error. A person seeing it should understand the
	// library was searched and came up empty.
	for _, word := range []string{"error", "failed", "sorry"} {
		if strings.Contains(strings.ToLower(NothingFound), word) {
			t.Errorf("the empty-result answer reads as a failure: %q", NothingFound)
		}
	}
}
