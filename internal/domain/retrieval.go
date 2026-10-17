// SPDX-License-Identifier: AGPL-3.0-or-later

package domain

import (
	"time"

	"github.com/google/uuid"
)

// QueryIntent is what the router decides a question is before anything is
// retrieved. Routing is cheap and saves more tokens than any other stage: a
// metadata question needs SQL and no retrieval at all.
type QueryIntent string

const (
	// IntentFactualLookup answers from passages, with citations.
	IntentFactualLookup QueryIntent = "factual-lookup"
	// IntentFindBook returns titles and does not synthesise an answer.
	IntentFindBook QueryIntent = "find-book"
	// IntentSummariseBook is scoped to one BookFile and retrieves structurally.
	IntentSummariseBook QueryIntent = "summarise-book"
	// IntentBroadTopic is too wide to answer: return a reading list, offer to narrow.
	IntentBroadTopic QueryIntent = "broad-topic"
	// IntentMetadata is answered from SQL. Retrieve nothing, embed nothing.
	IntentMetadata QueryIntent = "metadata"
)

// NeedsRetrieval reports whether this intent requires searching passages.
func (q QueryIntent) NeedsRetrieval() bool {
	switch q {
	case IntentFactualLookup, IntentSummariseBook:
		return true
	default:
		return false
	}
}

// Chunk is a retrieval unit: a passage with stable locators. A chunk the reader
// cannot navigate to is worthless, so the range is part of the definition rather
// than metadata alongside it.
type Chunk struct {
	ID         uuid.UUID `json:"id"`
	BookFileID uuid.UUID `json:"bookFileId"`
	// Seq is position in reading order within the file. Neighbour expansion is
	// Seq-1 and Seq+1.
	Seq          int    `json:"seq"`
	Range        Range  `json:"range"`
	ChapterTitle string `json:"chapterTitle,omitempty"`
	Content      string `json:"content"`
	TokenCount   int    `json:"tokenCount"`
	// StrategyVersion identifies the chunking strategy that produced this row.
	// Changing strategy means re-chunking, and old rows must be distinguishable.
	StrategyVersion int       `json:"strategyVersion"`
	CreatedAt       time.Time `json:"createdAt"`
}

// Embedding is a vector for a Chunk under a specific model.
type Embedding struct {
	ID      uuid.UUID `json:"id"`
	ChunkID uuid.UUID `json:"chunkId"`
	// ModelID is essential: swapping embedding models invalidates existing
	// vectors, and without it there is no way to tell which rows need
	// recomputing. Mixed-model vector search returns nonsense, not an error.
	ModelID    string    `json:"modelId"`
	Dimensions int       `json:"dimensions"`
	Vector     []float32 `json:"-"` // normalised on write
	CreatedAt  time.Time `json:"createdAt"`
}

// RetrievedChunk is a Chunk with the context an answer needs to cite it.
type RetrievedChunk struct {
	Chunk Chunk `json:"chunk"`

	WorkID     uuid.UUID `json:"workId"`
	WorkTitle  string    `json:"workTitle"`
	AuthorName string    `json:"authorName,omitempty"`

	// DenseRank and SparseRank are 1-based positions in each retrieval arm, or 0
	// when that arm did not return the chunk. Reciprocal Rank Fusion needs both.
	DenseRank  int `json:"denseRank,omitempty"`
	SparseRank int `json:"sparseRank,omitempty"`

	FusedScore  float64 `json:"fusedScore"`
	RerankScore float64 `json:"rerankScore,omitempty"`
}

// RRFScore is the Reciprocal Rank Fusion contribution of a single arm at the
// given 1-based rank. A rank of 0 means the arm did not return the chunk and
// contributes nothing.
//
// k = 60 is the value from the original RRF paper and is a sensible default:
// it damps the influence of the top few ranks enough that one arm cannot
// dominate the fusion on its own.
func RRFScore(rank int, k float64) float64 {
	if rank <= 0 {
		return 0
	}
	return 1 / (k + float64(rank))
}

// DefaultRRFK is the standard Reciprocal Rank Fusion constant.
const DefaultRRFK = 60.0

// Fuse computes the fused score from both arms' ranks.
func (r *RetrievedChunk) Fuse(k float64) {
	r.FusedScore = RRFScore(r.DenseRank, k) + RRFScore(r.SparseRank, k)
}
