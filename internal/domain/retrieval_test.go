// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"math"
	"sort"
	"testing"
)

func TestRRFScore(t *testing.T) {
	// A missing arm contributes nothing; it must not be treated as rank 1.
	if got := RRFScore(0, DefaultRRFK); got != 0 {
		t.Fatalf("RRFScore(0) = %v, want 0", got)
	}
	if got := RRFScore(-3, DefaultRRFK); got != 0 {
		t.Fatalf("RRFScore(-3) = %v, want 0", got)
	}

	// Better rank, higher score, and strictly monotonic.
	prev := math.Inf(1)
	for rank := 1; rank <= 50; rank++ {
		got := RRFScore(rank, DefaultRRFK)
		if got >= prev {
			t.Fatalf("rank %d scored %v, not below previous %v", rank, got, prev)
		}
		prev = got
	}

	if want := 1.0 / 61.0; math.Abs(RRFScore(1, DefaultRRFK)-want) > 1e-12 {
		t.Fatalf("RRFScore(1) = %v, want %v", RRFScore(1, DefaultRRFK), want)
	}
}

// The reason hybrid retrieval exists: a chunk found by both arms should beat a
// chunk that only one arm ranked first. Dense-only retrieval fails on proper
// nouns and symbols, which a technical library is full of.
func TestFusionPrefersAgreementAcrossArms(t *testing.T) {
	both := RetrievedChunk{DenseRank: 3, SparseRank: 2}
	denseOnly := RetrievedChunk{DenseRank: 1}
	sparseOnly := RetrievedChunk{SparseRank: 1}

	for _, c := range []*RetrievedChunk{&both, &denseOnly, &sparseOnly} {
		c.Fuse(DefaultRRFK)
	}

	if both.FusedScore <= denseOnly.FusedScore {
		t.Fatalf("chunk found by both arms (%v) did not beat dense-only rank 1 (%v)",
			both.FusedScore, denseOnly.FusedScore)
	}
	if both.FusedScore <= sparseOnly.FusedScore {
		t.Fatalf("chunk found by both arms (%v) did not beat sparse-only rank 1 (%v)",
			both.FusedScore, sparseOnly.FusedScore)
	}
}

func TestFusionOrdering(t *testing.T) {
	chunks := []RetrievedChunk{
		{WorkTitle: "dense 10 only", DenseRank: 10},
		{WorkTitle: "both near top", DenseRank: 2, SparseRank: 3},
		{WorkTitle: "sparse 1 only", SparseRank: 1},
	}
	for i := range chunks {
		chunks[i].Fuse(DefaultRRFK)
	}
	sort.Slice(chunks, func(i, j int) bool { return chunks[i].FusedScore > chunks[j].FusedScore })

	if chunks[0].WorkTitle != "both near top" {
		t.Fatalf("top result = %q, want %q", chunks[0].WorkTitle, "both near top")
	}
	if chunks[len(chunks)-1].WorkTitle != "dense 10 only" {
		t.Fatalf("last result = %q, want %q", chunks[len(chunks)-1].WorkTitle, "dense 10 only")
	}
}

func TestIntentNeedsRetrieval(t *testing.T) {
	tests := map[QueryIntent]bool{
		IntentFactualLookup: true,
		IntentSummariseBook: true,
		// "How many unread books do I have" is SQL. Retrieving for it burns the
		// local model's context on a question that never needed it.
		IntentMetadata: false,
		// find-book answers from work-level metadata, not passages.
		IntentFindBook: false,
		// broad-topic returns a reading list and offers to narrow.
		IntentBroadTopic: false,
	}
	for intent, want := range tests {
		t.Run(string(intent), func(t *testing.T) {
			if got := intent.NeedsRetrieval(); got != want {
				t.Fatalf("%s.NeedsRetrieval() = %v, want %v", intent, got, want)
			}
		})
	}
}
