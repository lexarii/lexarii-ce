// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
)

// RerankResult is one document's score, keyed back to the input order.
type RerankResult struct {
	Index int     `json:"index"`
	Score float64 `json:"relevance_score"`
}

// ErrDegenerateScores reports a reranker that answered without discriminating.
//
// The failure this catches is specific and common. A cross-encoder converted to
// GGUF without its classification head - `cls.output.weight` missing, which
// several published conversions are - loads happily, serves happily, and
// returns scores like 1.1e-28 for every document. Nothing errors. The ranking
// it produces is noise, and noise silently replacing a working ranking is the
// worst possible outcome, so it is refused instead.
var ErrDegenerateScores = fmt.Errorf("the reranker scored every passage the same; " +
	"this usually means a GGUF conversion without its classification head")

// rerankRequest is the wire format llama.cpp, vLLM, Jina and Cohere all accept.
type rerankRequest struct {
	Model     string   `json:"model,omitempty"`
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

// rerankResponse covers both spellings in use.
//
// llama.cpp, Cohere and Jina return `results`; Voyage returns `data` with the
// same objects inside it. One extra field is cheaper than telling somebody
// their reranker is unsupported when it works perfectly and is spelled
// differently.
type rerankResponse struct {
	Results []RerankResult `json:"results"`
	Data    []RerankResult `json:"data"`
}

// scores returns whichever field the server filled in.
func (r rerankResponse) scores() []RerankResult {
	if len(r.Results) > 0 {
		return r.Results
	}
	return r.Data
}

// Rerank scores documents against a query with a cross-encoder.
//
// A cross-encoder reads the question and one passage TOGETHER and scores how
// well they match. That is a different model from the one that writes answers,
// and a different job: measured on this library, asking the generative model to
// reorder passages cost 0.081 of Recall@10 and five seconds a question. It is
// not a better prompt away from working - a decoder asked to rank is doing a
// task it was not trained for.
//
// Served by llama.cpp with `--reranking --embedding --pooling rank`, and by
// every hosted reranker with the same request shape.
func (c *Client) Rerank(ctx context.Context, query string, documents []string) ([]RerankResult, error) {
	if c == nil {
		return nil, ErrNotConfigured
	}
	if len(documents) == 0 {
		return nil, nil
	}

	body, err := json.Marshal(rerankRequest{
		Model:     c.model,
		Query:     query,
		Documents: documents,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding rerank request: %w", err)
	}

	// Interactive: somebody is waiting for the answer this reorders.
	raw, err := c.postRaw(ctx, "/rerank", body, Interactive)
	if err != nil {
		return nil, err
	}

	var out rerankResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding rerank reply: %w", err)
	}
	scores := out.scores()
	if len(scores) == 0 {
		return nil, fmt.Errorf("the reranker returned no scores")
	}

	for _, r := range scores {
		if r.Index < 0 || r.Index >= len(documents) {
			return nil, fmt.Errorf("the reranker scored document %d of %d", r.Index, len(documents))
		}
	}
	if degenerate(scores) {
		return nil, ErrDegenerateScores
	}
	return scores, nil
}

// degenerate reports scores that cannot order anything.
//
// Two shapes, both seen in the wild: every score identical, and every score so
// close to zero that the differences are float noise. A single document is not
// degenerate - there is nothing to discriminate between.
func degenerate(results []RerankResult) bool {
	if len(results) < 2 {
		return false
	}

	lowest, highest := results[0].Score, results[0].Score
	var largest float64
	for _, r := range results {
		lowest = math.Min(lowest, r.Score)
		highest = math.Max(highest, r.Score)
		largest = math.Max(largest, math.Abs(r.Score))
	}

	// Every score the same.
	if highest == lowest {
		return true
	}
	// Every score indistinguishable from zero. 1e-12 is far below any scale a
	// working reranker produces - theirs are around -10 to 10 as logits, or 0
	// to 1 once squashed - and far above the 1e-28 a headless conversion emits.
	return largest < 1e-12
}
