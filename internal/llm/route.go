// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"fmt"
	"strings"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm/prompts"
)

// Route is what the router decided about a question.
type Route struct {
	Intent   domain.QueryIntent `json:"intent"`
	Subjects []string           `json:"subjects"`
	// Searches is one search per side of a comparison, empty for the ordinary
	// question about a single thing.
	//
	// A comparison retrieved as one query returns the side it matched best and
	// nothing from the other: multi-book coverage on the eval set sat at 0.500,
	// and reranking could not move it, because a reranker reorders what was
	// found and cannot promote a book that never appeared.
	Searches   []string `json:"searches"`
	Book       string   `json:"book"`
	Confidence float64  `json:"confidence"`

	PromptID string `json:"-"`
	Model    string `json:"-"`
}

// Query returns the single text to search with.
//
// The question as asked. There used to be a "search" field here - the model's
// rewrite of the question, stripped of "can you tell me" and with abbreviations
// expanded - and v2 removes it, because the first time it ever actually ran it
// was measurably worse: passage Recall@10 0.719 down to 0.656 and passage MRR
// 0.525 to 0.485, on the same 37 questions and with everything else unchanged.
//
// It had never run before. A rewrite loses the exact words, and exact words are
// most of what a technical question is made of.
func (r Route) Query(question string) string {
	return question
}

// Queries returns one search per arm: several for a comparison, one otherwise.
//
// Only a genuine split counts. A single "search" in the list is the ordinary
// case wearing a different hat, and a list with blanks in it is a model that
// filled the field because the field was there.
func (r Route) Queries(question string) []string {
	out := make([]string, 0, len(r.Searches))
	seen := map[string]bool{}
	for _, s := range r.Searches {
		s = strings.TrimSpace(s)
		// The same side twice is one arm's worth of work done twice.
		if s == "" || seen[strings.ToLower(s)] {
			continue
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	if len(out) < 2 {
		return []string{r.Query(question)}
	}
	return out
}

var routeSchema = &Schema{
	Name:   "query_route",
	Strict: true,
	Definition: map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		// All of them, which is about the model ANSWERING them rather than
		// about the answers mattering - "" and [] are fine values here.
		//
		// With only "intent" required, gemma4-12b returned only intent. Not
		// once in four probe questions did it fill in anything else, which
		// means every other field on this struct had been dead since it was
		// written - the rewrite included, which is how its regression went
		// unnoticed. An optional property in a JSON schema is one llama.cpp's
		// grammar permits the model to skip, and a model asked for the minimum
		// gives the minimum.
		"required": []string{"book", "confidence", "intent", "searches", "subjects"},
		"properties": map[string]any{
			"intent": map[string]any{
				"type": "string",
				"enum": []string{
					string(domain.IntentFactualLookup),
					string(domain.IntentFindBook),
					string(domain.IntentSummariseBook),
					string(domain.IntentBroadTopic),
					string(domain.IntentMetadata),
				},
			},
			"subjects": map[string]any{
				"type":     "array",
				"maxItems": maxRouteSubjects,
				"items":    map[string]any{"type": "string"},
			},
			"searches": map[string]any{
				"type":     "array",
				"maxItems": maxRouteSearches,
				"items":    map[string]any{"type": "string"},
			},
			"book":       map[string]any{"type": "string"},
			"confidence": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
		},
	},
}

const (
	maxRouteSubjects = 4
	// Three sides is already an unusual question, and each one costs an
	// embedding and a search.
	maxRouteSearches = 3
	// The reply is five short fields. Generous enough for a model that ignores
	// the request not to reason, and never reached by one that does not.
	maxRouteTokens = 2000
)

// Routing is what decides where to look before anything is retrieved.
//
// It is the cheapest stage and saves the most: a question about the collection
// needs SQL and no retrieval at all, a question too broad to answer should
// return a reading list rather than a confident synthesis of six random
// passages, and a question with a subject can be answered from the twenty books
// about that subject rather than from all five thousand.
func RouteQuestion(ctx context.Context, client *Client, in prompts.RouteInput) (Route, error) {
	if client == nil {
		return Route{}, ErrNotConfigured
	}
	if strings.TrimSpace(in.Question) == "" {
		return Route{}, fmt.Errorf("routing: no question")
	}

	tmpl, err := prompts.Route(in)
	if err != nil {
		return Route{}, err
	}

	var out Route
	err = client.JSON(ctx, Request{
		Messages: []Message{System(tmpl.System), User(tmpl.User)},
		Schema:   routeSchema,
		// Deciding what kind of question this is does not benefit from
		// deliberation, and the person is waiting.
		ReasoningEffort: ReasoningOff,
		MaxTokens:       maxRouteTokens,
	}, &out)
	if err != nil {
		return Route{}, fmt.Errorf("routing: %w", err)
	}

	out.PromptID = tmpl.ID()
	out.Model = client.Model()
	return out.normalised(), nil
}

func (r Route) normalised() Route {
	out := r

	switch r.Intent {
	case domain.IntentFactualLookup, domain.IntentFindBook,
		domain.IntentSummariseBook, domain.IntentBroadTopic, domain.IntentMetadata:
	default:
		// An intent outside the list is the model inventing one. Falling back to
		// a factual lookup is the safe direction: it retrieves and cites, which
		// is the behaviour that shows its working.
		out.Intent = domain.IntentFactualLookup
	}

	out.Subjects = cleanList(r.Subjects, maxRouteSubjects, maxSubjectLength, strings.ToLower)
	out.Searches = cleanList(r.Searches, maxRouteSearches, 500, tidy)
	out.Book = tidy(clean(r.Book, maxTitleLength))

	if out.Confidence < 0 {
		out.Confidence = 0
	}
	if out.Confidence > 1 {
		out.Confidence = 1
	}
	return out
}

// FallbackRoute is what to do with no model, or when routing fails.
//
// A factual lookup over the whole library: it is the most useful thing that can
// be done without knowing what was asked, and it retrieves and cites, so a
// wrong guess is visible rather than confidently hidden.
func FallbackRoute(question string) Route {
	// No arms: Queries falls back to the question as asked, which is what a
	// fallback should search for.
	return Route{Intent: domain.IntentFactualLookup}
}
