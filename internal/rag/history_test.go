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
