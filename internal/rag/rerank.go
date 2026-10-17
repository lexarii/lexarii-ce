// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
)

// Reranking is the only stage that reads a candidate in light of the actual
// question. Retrieval matches a question against a passage without ever
// considering whether the passage ANSWERS it: a table of contents listing
// "Eigenvalues and Eigenvectors" matches a question about eigenvalues on every
// signal an index has, and answers nothing.
//
// This is a CROSS-ENCODER's job - a model that reads the question and one
// passage together and scores the pair - and not the chat model's. That was
// measured rather than assumed. Asking gemma4-12b to reorder twenty passages
// cost 0.081 of work Recall@10 and 0.167 of multi-book coverage, bought 0.010
// of passage MRR, and took retrieval from 1.6 to 6.8 seconds a question. A
// decoder asked to rank is doing a task it was not trained for, and no prompt
// fixes that.
//
// llama.cpp serves cross-encoders: `--reranking --pooling rank`, and the
// request shape is the one Jina and Cohere use. bge-reranker-v2-m3 is 636MB at
// Q8_0 and scores twenty full-length passages in 280-360ms on a 12GB RTX 3060
// that is already serving a 12B chat model.
//
// Every latency in these comments came from that card. The recall and MRR
// figures are properties of the models and will hold anywhere; the
// milliseconds are one consumer GPU serving three models at once and should be
// read as a shape rather than a number.

// RerankPool is how many candidates to retrieve for the reranker to choose
// from.
//
// Reranking can only reorder what it is handed, so the pool has to be deeper
// than the answer needs or a passage at rank 25 can never be rescued. Twenty is
// deep enough to catch those and cheap for a cross-encoder, which scores each
// pair independently and in one batch.
const RerankPool = 20

// rerankExcerpt is how much of each candidate the reranker sees.
//
// The whole passage, near enough: chunks are capped at 2,000 characters and
// average 1,378 on a real library, so this reaches all of nearly all of them.
//
// It was 700 while a CHAT model did this job, because twenty passages had to
// fit in one prompt alongside the question - which meant the reranker judged
// roughly half of each passage. A cross-encoder scores one pair at a time and
// has no such pressure, so the clip was a handicap inherited from an approach
// that no longer exists.
//
// Measured against bge-reranker-v2-m3, raising it from 700 to 2,048 is worth
// 0.032 of passage Recall@10, 0.106 of passage MRR and 0.063 of work MRR, for
// 250ms. Reranking half a passage was close to not reranking at all: at 700 it
// moved passage Recall by 0.031 and made both MRRs slightly WORSE than leaving
// the order alone. The bound stays because a reranker's
// context is finite - bge-reranker-v2-m3 takes 8,192 tokens, others take 512 -
// and a passage silently truncated by the server is scored on its first half
// without saying so.
const rerankExcerpt = 2048

// Rerank orders candidates by how well they answer the question.
//
// It never returns an error: a reranker that fails must leave the fused order
// alone rather than take the answer down with it. Reranking is optional here as
// the model is everywhere else, and the fused order is a serviceable answer.
//
// A reranker whose scores cannot discriminate is treated as a failure, not as
// an opinion - see llm.ErrDegenerateScores, which is the shape a GGUF converted
// without its classification head produces.
func Rerank(ctx context.Context, reranker *llm.Client, question string,
	candidates []domain.RetrievedChunk, keep int) []domain.RetrievedChunk {

	if reranker == nil || len(candidates) <= 1 || keep <= 0 {
		return truncate(candidates, keep)
	}

	documents := make([]string, 0, len(candidates))
	for _, c := range candidates {
		documents = append(documents, clip(c.Chunk.Content, rerankExcerpt))
	}

	scored, err := reranker.Rerank(ctx, question, documents)
	if err != nil {
		slog.Warn("reranking failed; keeping the retrieved order", "error", err)
		return truncate(candidates, keep)
	}

	// Highest score first. Sorted here rather than trusted from the server,
	// because the endpoint is only specified to return scores - some
	// implementations sort, some do not, and one that does not would otherwise
	// silently reverse the point of the stage.
	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })

	ranked := make([]int, 0, len(scored))
	for _, r := range scored {
		ranked = append(ranked, r.Index+1)
	}

	ordered, ok := apply(candidates, ranked, keep)
	if !ok {
		return truncate(candidates, keep)
	}
	return ordered
}

// apply turns a ranking into passages, and reports whether it was usable.
//
// Written against a list of one-based positions because that is what the stage
// produced when a model was asked to rank in prose. A cross-encoder returns
// indices it was given, so the invented-number case cannot arise - but the
// bounds check stays, because a scoring service is still somebody else\'s
// program and this is the boundary with it.
func apply(candidates []domain.RetrievedChunk, ranked []int, keep int) ([]domain.RetrievedChunk, bool) {
	if len(ranked) == 0 {
		return nil, true
	}

	seen := make(map[int]bool, len(ranked))
	out := make([]domain.RetrievedChunk, 0, keep)
	for _, n := range ranked {
		// One-based, from the prompt. Anything outside the list was invented.
		if n < 1 || n > len(candidates) || seen[n] {
			continue
		}
		seen[n] = true

		chunk := candidates[n-1]
		// Position in the reranked order, highest first, on the same scale as
		// the fused score so the two can be read side by side.
		chunk.RerankScore = domain.RRFScore(len(out)+1, domain.DefaultRRFK)
		out = append(out, chunk)
		if len(out) == keep {
			break
		}
	}
	if len(out) == 0 {
		return nil, false
	}

	// The reranker orders; retrieval decides membership.
	//
	// Letting it drop passages outright was measured and cost 9 points of
	// Recall@10 for 2 of work MRR. A passage the model did not list is not
	// evidence that the passage is useless - the model was asked to pick the
	// best few and it did, from twenty near-identical candidates it read at
	// seven hundred characters each. Everything it did not rank keeps its fused
	// order and follows behind, so a bad judgement costs position rather than
	// the answer.
	for i, c := range candidates {
		if len(out) == keep {
			break
		}
		if seen[i+1] {
			continue
		}
		c.RerankScore = 0
		out = append(out, c)
	}
	return out, true
}

func truncate(passages []domain.RetrievedChunk, keep int) []domain.RetrievedChunk {
	if keep > 0 && len(passages) > keep {
		return passages[:keep]
	}
	return passages
}

// clip shortens a passage to roughly n bytes, at a rune and word boundary.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	if idx := strings.LastIndex(s[:cut], " "); idx > n/2 {
		cut = idx
	}
	return strings.TrimSpace(s[:cut]) + "…"
}
