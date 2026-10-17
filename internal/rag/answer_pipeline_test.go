// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/llm/prompts"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

var errPipelineStore = errors.New("the store is down")

// The whole path of a factual question: routed, searched as the asker, the
// passages and the asker's own notes numbered in one sequence, answered, and
// the citations checked against what was actually offered.
func TestAskAnswersFromPassagesAndNotesInOneCitationSequence(t *testing.T) {
	model := newFakeModel(t)
	// [4] is the note, [17] was never offered and must not survive.
	model.answer = "  Eigenvalues scale eigenvectors [1]. You wrote as much [4]. See [2][1] and [17].  "

	user := uuid.New()
	pinned := uuid.New()
	store := &pipelineStore{
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return pipelinePassages("Linear Algebra Done Right", 3), nil
		},
		notes: []postgres.FoundNote{{
			Title:      "Linear Algebra Done Right",
			Annotation: domain.Annotation{SelectedText: "Av = λv", Note: "the key definition"},
		}},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	got, err := a.Ask(t.Context(), "  what is an eigenvalue?  ", Scope{UserID: user, BookFileID: &pinned})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	if got.Question != "what is an eigenvalue?" {
		t.Errorf("Question = %q, want it trimmed", got.Question)
	}
	if got.Route.Intent != domain.IntentFactualLookup {
		t.Errorf("Intent = %q", got.Route.Intent)
	}
	if got.Text != "Eigenvalues scale eigenvectors [1]. You wrote as much [4]. See [2][1] and [17]." {
		t.Errorf("Text = %q", got.Text)
	}
	if want := []int{1, 4, 2}; !reflect.DeepEqual(got.Cited, want) {
		t.Errorf("Cited = %v, want %v: in order of first use, notes included, inventions dropped", got.Cited, want)
	}
	if len(got.Passages) != 3 || len(got.Notes) != 1 {
		t.Errorf("returned %d passages and %d notes, want 3 and 1", len(got.Passages), len(got.Notes))
	}
	if got.Model != "fake-chat" || got.PromptID != "answer/"+prompts.AnswerVersion {
		t.Errorf("Model = %q, PromptID = %q", got.Model, got.PromptID)
	}
	if got.PromptTokens != 321 || got.CompletionTokens != 45 {
		t.Errorf("tokens = %d/%d, want what the server reported", got.PromptTokens, got.CompletionTokens)
	}

	// The search ran as the asker, pinned to the book in scope, for the
	// question as asked.
	if len(store.searches) != 1 {
		t.Fatalf("%d searches, want 1", len(store.searches))
	}
	q := store.searches[0]
	if q.ViewerID != user || q.BookFileID != &pinned || q.Text != "what is an eigenvalue?" || q.Limit != DefaultPassages {
		t.Errorf("search = %+v", q)
	}
	if q.Vector != nil {
		t.Error("a vector was sent with no embedding model configured")
	}

	// The prompt carried the passages as [1]-[3] with their sources, and the
	// note as [4] - one sequence, so [4] means the same thing to the model
	// and to the reader.
	answers := model.callsOf("answer")
	if len(answers) != 1 {
		t.Fatalf("%d answer requests, want 1", len(answers))
	}
	prompt := answers[0].user
	for _, want := range []string{
		"[1] from Linear Algebra Done Right, Chapter 1",
		"[3] from Linear Algebra Done Right, Chapter 3",
		"[4] your note on Linear Algebra Done Right",
		"beside: “Av = λv”",
		"what is an eigenvalue?",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the answer prompt lacks %q", want)
		}
	}
	if strings.Contains(prompt, "[5]") {
		t.Error("the prompt numbers something past the passages and the note")
	}
	if answers[0].stream {
		t.Error("Ask streamed; only AskStreaming should")
	}
	if answers[0].maxToken != maxAnswerTokens {
		t.Errorf("max_tokens = %d, want %d", answers[0].maxToken, maxAnswerTokens)
	}
}

// Grounding: nothing found is a real answer, said plainly - and the model is
// never asked to write one from no evidence.
func TestAskSaysNothingInYourLibraryWhenNothingIsFound(t *testing.T) {
	model := newFakeModel(t)
	model.answer = "a confident answer from nowhere [1]"
	store := &pipelineStore{}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	got, err := a.Ask(t.Context(), "what does my library say about falconry?", Scope{UserID: uuid.New()})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Text != NothingFound {
		t.Errorf("Text = %q, want NothingFound", got.Text)
	}
	if len(got.Cited) != 0 || len(got.Passages) != 0 {
		t.Errorf("cited %v from %d passages; there is nothing to cite", got.Cited, len(got.Passages))
	}
	if n := len(model.callsOf("answer")); n != 0 {
		t.Errorf("the model was asked to answer %d times with no passages", n)
	}
	if len(store.noteSearches) != 0 {
		t.Error("notes were searched for a question with no evidence")
	}
}

// With no model at all the library still answers: the question is searched
// as asked and the passages are the result. Search with citations.
func TestAskWithNoModelReturnsPassagesWithoutAnAnswer(t *testing.T) {
	store := &pipelineStore{
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return pipelinePassages("SICP", 2), nil
		},
		notes: []postgres.FoundNote{{Title: "SICP"}},
	}
	a := &Answerer{Store: store}

	got, err := a.Ask(t.Context(), "what is a closure?", Scope{UserID: uuid.New()})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Route.Intent != domain.IntentFactualLookup {
		t.Errorf("Intent = %q, want the fallback factual lookup", got.Route.Intent)
	}
	if got.Text != "" || got.Cited != nil || got.Model != "" {
		t.Errorf("an answer appeared with no model: %+v", got)
	}
	if len(got.Passages) != 2 {
		t.Errorf("Passages = %d, want 2", len(got.Passages))
	}
	if len(store.noteSearches) != 0 {
		t.Error("notes were looked up for an answer that will not be written")
	}
}

// A router that fails must not stop the library answering: the fallback is a
// plain search of everything, cited.
func TestAskFallsBackWhenRoutingFails(t *testing.T) {
	model := newFakeModel(t)
	model.routeStatus = http.StatusBadRequest
	model.answer = "From the book [1]."
	store := &pipelineStore{
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return pipelinePassages("SICP", 1), nil
		},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	got, err := a.Ask(t.Context(), "what is a closure?", Scope{})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got.Route.Intent != domain.IntentFactualLookup {
		t.Errorf("Intent = %q, want the fallback", got.Route.Intent)
	}
	if got.Text != "From the book [1]." || !reflect.DeepEqual(got.Cited, []int{1}) {
		t.Errorf("Text = %q, Cited = %v", got.Text, got.Cited)
	}
}

// A model that refuses to write the answer is an error the caller sees - but
// the passages already found are kept on the result, because they were
// already shown.
func TestAskReportsAFailedGeneration(t *testing.T) {
	model := newFakeModel(t)
	model.answerStatus = http.StatusBadRequest
	store := &pipelineStore{
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return pipelinePassages("SICP", 2), nil
		},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	got, err := a.Ask(t.Context(), "what is a closure?", Scope{})
	var httpErr *llm.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want the model's 400", err)
	}
	if len(got.Passages) != 2 {
		t.Errorf("Passages = %d, want the two already found", len(got.Passages))
	}
	if got.Text != "" {
		t.Errorf("Text = %q after a failed generation", got.Text)
	}
}

// A failed search is a failed question, not an empty library.
func TestAskReportsAFailedSearch(t *testing.T) {
	store := &pipelineStore{
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return nil, errPipelineStore
		},
	}
	a := &Answerer{Store: store}

	got, err := a.Ask(t.Context(), "anything", Scope{})
	if !errors.Is(err, errPipelineStore) {
		t.Fatalf("error = %v, want the store's", err)
	}
	if got.Text == NothingFound {
		t.Error("a broken search was reported as an empty library")
	}
}

func TestAskRefusesAnEmptyQuestion(t *testing.T) {
	a := &Answerer{Store: &pipelineStore{}}
	for _, q := range []string{"", "   \n\t"} {
		if _, err := a.Ask(t.Context(), q, Scope{}); err == nil {
			t.Errorf("Ask(%q) succeeded", q)
		}
		if _, _, err := a.Retrieve(t.Context(), q, Scope{}); err == nil {
			t.Errorf("Retrieve(%q) succeeded", q)
		}
	}
}

// The notes never cost the answer: a failed note search answers from the
// books alone, and a question with no asker looks for no notes at all.
func TestNotesAreNeverFatal(t *testing.T) {
	model := newFakeModel(t)
	model.answer = "Answered [1]."
	passages := func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
		return pipelinePassages("SICP", 1), nil
	}

	t.Run("note search fails", func(t *testing.T) {
		store := &pipelineStore{search: passages, notesErr: errPipelineStore}
		a := &Answerer{Store: store, Chat: model.client("fake-chat")}
		got, err := a.Ask(t.Context(), "q", Scope{UserID: uuid.New()})
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if got.Text != "Answered [1]." || len(got.Notes) != 0 {
			t.Errorf("Text = %q, Notes = %d", got.Text, len(got.Notes))
		}
	})

	t.Run("no asker", func(t *testing.T) {
		store := &pipelineStore{search: passages, notes: []postgres.FoundNote{{Title: "x"}}}
		a := &Answerer{Store: store, Chat: model.client("fake-chat")}
		got, err := a.Ask(t.Context(), "q", Scope{})
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if len(store.noteSearches) != 0 || len(got.Notes) != 0 {
			t.Errorf("notes were searched with no user: %v", store.noteSearches)
		}
	})
}

// Streaming delivers the passages first, before a word of the answer, then
// the text piece by piece - and returns the same complete Answer as Ask.
func TestAskStreamingDeliversPassagesBeforeText(t *testing.T) {
	model := newFakeModel(t)
	model.answerPieces = []string{"Closures ", "capture ", "their environment [1]."}
	store := &pipelineStore{
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return pipelinePassages("SICP", 2), nil
		},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	var events []string
	var shown []domain.RetrievedChunk
	got, err := a.AskStreaming(t.Context(), "what is a closure?", Scope{}, nil,
		func(p []domain.RetrievedChunk) {
			events = append(events, "passages")
			shown = p
		},
		func(d string) { events = append(events, "delta:"+d) },
	)
	if err != nil {
		t.Fatalf("AskStreaming: %v", err)
	}

	want := []string{"passages", "delta:Closures ", "delta:capture ", "delta:their environment [1]."}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %q, want %q", events, want)
	}
	if len(shown) != 2 {
		t.Errorf("onPassages received %d passages, want 2", len(shown))
	}
	if got.Text != "Closures capture their environment [1]." {
		t.Errorf("Text = %q, want the deltas assembled", got.Text)
	}
	if !reflect.DeepEqual(got.Cited, []int{1}) {
		t.Errorf("Cited = %v", got.Cited)
	}
	if got.PromptTokens != 654 || got.CompletionTokens != 32 {
		t.Errorf("tokens = %d/%d, want the stream's usage", got.PromptTokens, got.CompletionTokens)
	}
	if calls := model.callsOf("answer"); len(calls) != 1 || !calls[0].stream {
		t.Errorf("answer calls = %+v, want one streamed", calls)
	}
}

// With no passages there is nothing to show early and nothing to stream.
func TestAskStreamingWithNothingFound(t *testing.T) {
	model := newFakeModel(t)
	a := &Answerer{Store: &pipelineStore{}, Chat: model.client("fake-chat")}

	passagesCalled, deltas := false, 0
	got, err := a.AskStreaming(t.Context(), "q", Scope{}, nil,
		func([]domain.RetrievedChunk) { passagesCalled = true },
		func(string) { deltas++ },
	)
	if err != nil {
		t.Fatalf("AskStreaming: %v", err)
	}
	if got.Text != NothingFound || passagesCalled || deltas != 0 {
		t.Errorf("Text = %q, onPassages called = %v, deltas = %d", got.Text, passagesCalled, deltas)
	}
}

// A caller with no use for deltas gets the ordinary, unstreamed answer.
func TestAskStreamingWithoutADeltaHandlerDoesNotStream(t *testing.T) {
	model := newFakeModel(t)
	model.answer = "Whole [1]."
	store := &pipelineStore{
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return pipelinePassages("SICP", 1), nil
		},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}

	got, err := a.AskStreaming(t.Context(), "q", Scope{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("AskStreaming: %v", err)
	}
	if got.Text != "Whole [1]." {
		t.Errorf("Text = %q", got.Text)
	}
	if calls := model.callsOf("answer"); len(calls) != 1 || calls[0].stream {
		t.Errorf("answer calls = %+v, want one unstreamed", calls)
	}
}

// A follow-up is routed WITH the thread and answered with it in the prompt:
// "what about the second one" means nothing on its own.
func TestAFollowUpCarriesTheConversation(t *testing.T) {
	model := newFakeModel(t)
	model.answer = "The second one [1]."
	store := &pipelineStore{
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return pipelinePassages("SICP", 1), nil
		},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat")}
	history := History{
		{Role: "user", Text: "name two evaluation strategies"},
		{Role: "assistant", Text: "applicative order and normal order"},
	}

	if _, err := a.AskWithHistory(t.Context(), "what about the second one?", Scope{}, history); err != nil {
		t.Fatalf("AskWithHistory: %v", err)
	}

	routes := model.callsOf("route")
	if len(routes) != 1 {
		t.Fatalf("%d route calls", len(routes))
	}
	for _, want := range []string{"They asked: name two evaluation strategies", "You answered: applicative order",
		"They asked: what about the second one?"} {
		if !strings.Contains(routes[0].user, want) {
			t.Errorf("the router was not shown %q", want)
		}
	}
	answers := model.callsOf("answer")
	if len(answers) != 1 || !strings.Contains(answers[0].user, "Earlier in this conversation") ||
		!strings.Contains(answers[0].user, "applicative order and normal order") {
		t.Error("the answer prompt does not carry the conversation")
	}
	// The search is for the question as asked, not the whole thread.
	if store.searches[0].Text != "what about the second one?" {
		t.Errorf("searched for %q", store.searches[0].Text)
	}
}

// The passages shown are the passages the model received: trimmed to the
// window before anybody sees them, and the notes take only what is left.
func TestAskTrimsPassagesToTheWindowBeforeShowingThem(t *testing.T) {
	model := newFakeModel(t)
	model.answer = "Trimmed [1]."
	store := &pipelineStore{
		search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
			return chunksOf(10, 1514), nil
		},
		notes: []postgres.FoundNote{{
			Title:      "A Book",
			Annotation: domain.Annotation{SelectedText: strings.Repeat("quoted ", 2000), Note: "mine"},
		}},
	}
	a := &Answerer{Store: store, Chat: model.client("fake-chat"), ContextTokens: 8192}

	var shown int
	got, err := a.AskStreaming(t.Context(), "q", Scope{UserID: uuid.New()}, nil,
		func(p []domain.RetrievedChunk) { shown = len(p) }, func(string) {})
	if err != nil {
		t.Fatalf("AskStreaming: %v", err)
	}
	if len(got.Passages) >= 10 || len(got.Passages) == 0 {
		t.Fatalf("kept %d of 10 passages against an 8k window", len(got.Passages))
	}
	if shown != len(got.Passages) {
		t.Errorf("showed %d passages and answered from %d", shown, len(got.Passages))
	}
	prompt := model.callsOf("answer")[0].user
	if strings.Contains(prompt, "[10]") {
		t.Error("a trimmed passage reached the prompt")
	}
	cost, err := promptTokens(got.Passages, "")
	if err != nil {
		t.Fatal(err)
	}
	if left := a.budget() - cost; left < 0 {
		t.Errorf("kept passages overflow the budget by %d", -left)
	}
}

func TestMaxPassagesBoundsTheSearch(t *testing.T) {
	store := &pipelineStore{}
	a := &Answerer{Store: store, MaxPassages: 4}
	if _, err := a.Ask(t.Context(), "q", Scope{}); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if store.searches[0].Limit != 4 {
		t.Errorf("Limit = %d, want 4", store.searches[0].Limit)
	}
}

// Retrieve is the measured path: routing then the search the route implies,
// with no generation.
func TestRetrieve(t *testing.T) {
	t.Run("a factual question retrieves", func(t *testing.T) {
		model := newFakeModel(t)
		store := &pipelineStore{
			search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
				return pipelinePassages("SICP", 3), nil
			},
		}
		a := &Answerer{Store: store, Chat: model.client("fake-chat")}
		route, passages, err := a.Retrieve(t.Context(), "  what is a closure? ", Scope{})
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		if route.Intent != domain.IntentFactualLookup || len(passages) != 3 {
			t.Errorf("route %q, %d passages", route.Intent, len(passages))
		}
		if store.searches[0].Text != "what is a closure?" {
			t.Errorf("searched %q", store.searches[0].Text)
		}
		if len(model.callsOf("answer")) != 0 {
			t.Error("Retrieve generated an answer")
		}
	})

	t.Run("a question routed away from retrieval has no passages", func(t *testing.T) {
		model := newFakeModel(t)
		model.route = routeJSON(domain.IntentMetadata, nil, nil, "")
		store := &pipelineStore{}
		a := &Answerer{Store: store, Chat: model.client("fake-chat")}
		route, passages, err := a.Retrieve(t.Context(), "how many books do I have?", Scope{})
		if err != nil {
			t.Fatalf("Retrieve: %v", err)
		}
		if route.Intent != domain.IntentMetadata || passages != nil || len(store.searches) != 0 {
			t.Errorf("route %q, passages %v, searches %d", route.Intent, passages, len(store.searches))
		}
	})

	t.Run("no model falls back to searching", func(t *testing.T) {
		store := &pipelineStore{}
		a := &Answerer{Store: store}
		route, _, err := a.Retrieve(t.Context(), "q", Scope{})
		if err != nil || route.Intent != domain.IntentFactualLookup || len(store.searches) != 1 {
			t.Errorf("route %q, err %v, searches %d", route.Intent, err, len(store.searches))
		}
	})
}

// An answer cited in another notation is stored and returned in [n], with its
// citations read from it, whether it was streamed or not.
func TestAskNormalisesAnotherCitationNotation(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "streamed"}[streaming], func(t *testing.T) {
			model := newFakeModel(t)
			model.answer = "Closures capture 【2†L10-L20】 their environment 【1】【2】."
			model.answerPieces = []string{"Closures capture 【", "2†L10-L20】 their environment 【1】", "【2】."}
			store := &pipelineStore{
				search: func(postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
					return pipelinePassages("SICP", 2), nil
				},
			}
			a := &Answerer{Store: store, Chat: model.client("fake-chat")}

			var deltas strings.Builder
			var got Answer
			var err error
			if streaming {
				got, err = a.AskStreaming(t.Context(), "what is a closure?", Scope{}, nil, nil,
					func(d string) { deltas.WriteString(d) })
			} else {
				got, err = a.Ask(t.Context(), "what is a closure?", Scope{})
			}
			if err != nil {
				t.Fatalf("ask: %v", err)
			}
			const want = "Closures capture [2] their environment [1][2]."
			if got.Text != want {
				t.Errorf("Text = %q, want %q", got.Text, want)
			}
			if streaming && deltas.String() != want {
				t.Errorf("streamed %q, want %q", deltas.String(), want)
			}
			if !reflect.DeepEqual(got.Cited, []int{2, 1}) {
				t.Errorf("Cited = %v, want [2 1]", got.Cited)
			}
		})
	}
}
