// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"strings"
	"testing"
)

func TestHistoryRendersBothSides(t *testing.T) {
	h := History{
		{Role: "user", Text: "what is an eigenvalue?"},
		{Role: "assistant", Text: "A value λ such that Av = λv."},
	}

	got := h.Render()
	if !strings.Contains(got, "They asked: what is an eigenvalue?") {
		t.Errorf("the question is missing: %q", got)
	}
	if !strings.Contains(got, "You answered: A value") {
		t.Errorf("the answer is missing: %q", got)
	}
}

func TestEmptyHistoryRendersNothing(t *testing.T) {
	if got := History(nil).Render(); got != "" {
		t.Errorf("empty history rendered %q", got)
	}
}

// The most recent exchange is the one a follow-up refers to, so it must be the
// last thing dropped - trimming from the front would keep the least relevant
// part of the thread.
func TestHistoryKeepsTheMostRecentTurns(t *testing.T) {
	var h History
	for i := 0; i < 20; i++ {
		h = append(h, Turn{Role: "user", Text: "question " + string(rune('a'+i))})
	}

	got := h.Render()
	if !strings.Contains(got, "question t") {
		t.Errorf("the most recent turn was dropped: %q", got)
	}
	if strings.Contains(got, "question a") {
		t.Errorf("an old turn survived trimming: %q", got)
	}
	if lines := strings.Count(got, "They asked"); lines > maxHistoryTurns {
		t.Errorf("kept %d turns, want at most %d", lines, maxHistoryTurns)
	}
}

// One turn can be a long answer, so the character bound matters as much as the
// turn count: the passages are what the answer must come from, and history must
// not push them out of the context window.
func TestHistoryIsBoundedByLength(t *testing.T) {
	long := strings.Repeat("a very long answer indeed. ", 400)
	h := History{
		{Role: "assistant", Text: long},
		{Role: "assistant", Text: long},
		{Role: "user", Text: "the latest question"},
	}

	got := h.Render()
	if len(got) > maxHistoryChars*2 {
		t.Errorf("rendered %d characters, want near %d", len(got), maxHistoryChars)
	}
	if !strings.Contains(got, "the latest question") {
		t.Error("the most recent turn was dropped in favour of older, longer ones")
	}
}

// A reading list is the whole of a find-book turn's answer. Rendered as
// "You answered:" with nothing after it, the follow-up "who are the villains in
// the book?" had no book to mean.
func TestHistoryRendersAReadingList(t *testing.T) {
	h := History{
		{Role: "user", Text: "what other sherlock books are there in my library?"},
		{Role: "assistant", Listed: []string{WorkLabel("The Adventures of Sherlock Holmes", []string{"Arthur Conan Doyle"})}},
		{Role: "user", Text: "who are the villians in the book?"},
		{Role: "assistant", Text: "Moriarty [1].", Cited: []string{"The Adventures of Sherlock Holmes"}},
	}

	want := "They asked: what other sherlock books are there in my library?\n\n" +
		"You listed: The Adventures of Sherlock Holmes — Arthur Conan Doyle\n\n" +
		"They asked: who are the villians in the book?\n\n" +
		"You answered: Moriarty [1].\n" +
		"You cited: The Adventures of Sherlock Holmes"
	if got := h.Render(); got != want {
		t.Errorf("rendered\n%s\nwant\n%s", got, want)
	}
}

// An assistant turn with nothing in it says nothing, rather than a dangling
// "You answered:" that a router reads as an answer.
func TestHistorySkipsAnEmptyAnswer(t *testing.T) {
	h := History{{Role: "user", Text: "anything?"}, {Role: "assistant"}}
	if got := h.Render(); got != "They asked: anything?" {
		t.Errorf("rendered %q", got)
	}
}

// One turn names at most maxHistoryWorks books, and they count against the
// same character bound as the text.
func TestHistoryBoundsTheBooksATurnNames(t *testing.T) {
	var listed []string
	for i := 0; i < 30; i++ {
		listed = append(listed, WorkLabel("Book "+string(rune('A'+i)), []string{"Someone"}))
	}
	got := History{{Role: "assistant", Listed: listed}}.Render()
	if n := strings.Count(got, " — Someone"); n != maxHistoryWorks {
		t.Errorf("named %d books, want %d: %q", n, maxHistoryWorks, got)
	}

	// A long list on an older turn is dropped before the latest question is.
	long := make([]string, maxHistoryWorks)
	for i := range long {
		long[i] = strings.Repeat("A Very Long Title ", 30)
	}
	got = History{
		{Role: "assistant", Listed: long},
		{Role: "user", Text: "the latest question"},
	}.Render()
	if strings.Contains(got, "You listed") || !strings.Contains(got, "the latest question") {
		t.Errorf("the list was not counted against the bound: %d chars", len(got))
	}
}

func TestWorkLabel(t *testing.T) {
	for _, tt := range []struct {
		title   string
		authors []string
		want    string
	}{
		{"Dracula", []string{"Bram Stoker"}, "Dracula — Bram Stoker"},
		{"Good Omens", []string{"Terry Pratchett", "Neil Gaiman"}, "Good Omens — Terry Pratchett, Neil Gaiman"},
		{" Untitled ", nil, "Untitled"},
	} {
		if got := WorkLabel(tt.title, tt.authors); got != tt.want {
			t.Errorf("WorkLabel(%q, %q) = %q, want %q", tt.title, tt.authors, got, tt.want)
		}
	}
}
