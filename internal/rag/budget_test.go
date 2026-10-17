// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"strings"
	"testing"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm/prompts"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// chunksOf builds n passages of the given character length, which is how the
// real ones arrive: this library's median chunk is 1,514 characters.
func chunksOf(n, chars int) []domain.RetrievedChunk {
	out := make([]domain.RetrievedChunk, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, domain.RetrievedChunk{
			Chunk:     domain.Chunk{Content: strings.Repeat("a ", chars/2)},
			WorkTitle: "A Book",
		})
	}
	return out
}

// The case that made this necessary: ten passages at the median length, the
// default 8,192 window, and 4,000 tokens reserved for the answer. Before the
// budget existed this prompt went out at about 4,300 tokens of passages and
// the server dropped the front of it.
func TestTheMedianQuestionIsTrimmedToFitAnEightKWindow(t *testing.T) {
	a := &Answerer{ContextTokens: 8192}

	kept, noteBudget := a.fitPassages(chunksOf(10, 1514), nil)

	if len(kept) == 10 {
		t.Fatal("ten median passages were kept; they do not fit 8192 - 4000 tokens")
	}
	if len(kept) == 0 {
		t.Fatal("every passage was dropped, which answers the question with nothing")
	}
	cost, err := promptTokens(kept, "")
	if err != nil {
		t.Fatalf("measuring: %v", err)
	}
	if cost > a.budget() {
		t.Errorf("kept %d passages costing %d tokens, over the budget of %d",
			len(kept), cost, a.budget())
	}
	if noteBudget < 0 {
		t.Errorf("note budget = %d, want what is left rather than a debt", noteBudget)
	}
	t.Logf("kept %d of 10, %d tokens of %d, %d left for notes",
		len(kept), cost, a.budget(), noteBudget)
}

// A big window changes nothing. The trim is for the local models this is
// built for, not a tax on everyone.
func TestARoomyWindowKeepsEveryPassage(t *testing.T) {
	a := &Answerer{ContextTokens: 128000}

	kept, _ := a.fitPassages(chunksOf(10, 1514), nil)

	if len(kept) != 10 {
		t.Errorf("kept %d of 10 passages against a 128k window", len(kept))
	}
}

// Zero is "unbounded", which is what a Lexarii with no window configured
// has always done and must keep doing.
func TestAnUnsetWindowDoesNotTrim(t *testing.T) {
	a := &Answerer{}

	kept, budget := a.fitPassages(chunksOf(10, 2000), nil)

	if len(kept) != 10 {
		t.Errorf("kept %d of 10 with no window configured", len(kept))
	}
	if budget != 0 {
		t.Errorf("budget = %d, want 0 meaning unbounded", budget)
	}
}

// The history is part of the prompt, so it has to come out of the same
// budget. A long thread that quietly bought itself room by evicting nothing
// would be the same overflow in a different place.
func TestHistoryCostsPassages(t *testing.T) {
	a := &Answerer{ContextTokens: 8192}
	passages := chunksOf(10, 1514)

	alone, _ := a.fitPassages(passages, nil)
	withHistory, _ := a.fitPassages(passages, History{
		{Role: "user", Text: strings.Repeat("what about the second one ", 200)},
		{Role: "assistant", Text: strings.Repeat("the second one says ", 200)},
	})

	if len(withHistory) >= len(alone) {
		t.Errorf("kept %d passages with a long history and %d without; history is free",
			len(withHistory), len(alone))
	}
}

// One passage always survives. "Nothing in your library covers this" is a
// real answer with a real meaning, and it must not be what a narrow window
// produces.
func TestOnePassageSurvivesAWindowTooSmallForIt(t *testing.T) {
	a := &Answerer{ContextTokens: maxAnswerTokens + 64}

	kept, _ := a.fitPassages(chunksOf(10, 2000), nil)

	if len(kept) != 1 {
		t.Errorf("kept %d passages, want the single best one", len(kept))
	}
}

// A window with no room for an answer is a misconfiguration, not a reason to
// send one passage and hope.
func TestAWindowSmallerThanTheAnswerIsTreatedAsUnset(t *testing.T) {
	a := &Answerer{ContextTokens: maxAnswerTokens}

	if got := a.budget(); got != 0 {
		t.Errorf("budget = %d, want 0", got)
	}
}

// The notes take what is left and no more. Before the bound, a note carrying
// a highlighted chapter could push the passages out of the window on its own.
func TestNotesAreDroppedWhenThereIsNoRoomLeft(t *testing.T) {
	notes := []postgres.FoundNote{
		{Title: "A Book", Annotation: domain.Annotation{
			SelectedText: strings.Repeat("quoted ", 400), Note: "first",
		}},
		{Title: "A Book", Annotation: domain.Annotation{
			SelectedText: strings.Repeat("quoted ", 400), Note: "second",
		}},
		{Title: "A Book", Annotation: domain.Annotation{
			SelectedText: strings.Repeat("quoted ", 400), Note: "third",
		}},
	}

	if kept := fitNotes(notes, 10); len(kept) != 0 {
		t.Errorf("kept %d notes against a 10 token budget", len(kept))
	}
	if kept := fitNotes(notes, 100000); len(kept) != 3 {
		t.Errorf("kept %d notes against a budget that fits them all", len(kept))
	}
	// Unbounded, as when no window is configured.
	if kept := fitNotes(notes, 0); len(kept) != 3 {
		t.Errorf("kept %d of 3 notes with no budget set", len(kept))
	}
}

// The measurement has to be of the prompt, not of the chunks: the scaffold
// and the per passage headers are a real share of a small window.
func TestTheScaffoldIsCounted(t *testing.T) {
	empty, err := promptTokens(nil, "")
	if err != nil {
		t.Fatalf("measuring: %v", err)
	}
	if empty < 100 {
		t.Errorf("an empty prompt measured %d tokens; the scaffold is not being counted", empty)
	}

	one, err := promptTokens(chunksOf(1, 1514), "")
	if err != nil {
		t.Fatalf("measuring: %v", err)
	}
	if one <= empty+EstimateTokens(strings.Repeat("a ", 757)) {
		t.Errorf("one passage added %d tokens, less than the passage itself", one-empty)
	}
	t.Logf("scaffold %d tokens, one median passage brings it to %d", empty, one)
}

// Trimming must not renumber anything. The passages keep their order and the
// notes are numbered after however many survived, so [4] means the same thing
// to the model and to the reader.
func TestTrimmingKeepsTheBestPassagesInOrder(t *testing.T) {
	a := &Answerer{ContextTokens: 8192}
	passages := chunksOf(10, 1514)
	for i := range passages {
		passages[i].WorkTitle = string(rune('A' + i))
	}

	kept, _ := a.fitPassages(passages, nil)

	for i, p := range kept {
		if p.WorkTitle != string(rune('A'+i)) {
			t.Fatalf("passage %d is %q, want the %d-th retrieved", i, p.WorkTitle, i)
		}
	}

	numbered := toPromptNotes([]postgres.FoundNote{{Title: "A Book"}}, len(kept))
	if numbered[0].Number != len(kept)+1 {
		t.Errorf("the first note is [%d], want [%d] - one past the passages the model saw",
			numbered[0].Number, len(kept)+1)
	}
}

// promptTokens is only a measurement if it measures the prompt that is
// actually sent. This pins it to the same builder generate uses.
func TestTheMeasurementUsesTheRealPrompt(t *testing.T) {
	passages := chunksOf(3, 800)
	tmpl, err := prompts.Answer(prompts.AnswerInput{Passages: toPromptPassages(passages)})
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	want := EstimateTokens(tmpl.System) + EstimateTokens(tmpl.User)

	got, err := promptTokens(passages, "")
	if err != nil {
		t.Fatalf("measuring: %v", err)
	}
	if got != want {
		t.Errorf("promptTokens = %d, the rendered prompt is %d", got, want)
	}
}
