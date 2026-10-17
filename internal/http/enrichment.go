// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// EnrichmentStore is what the enrichment handlers need.
type EnrichmentStore interface {
	WorksNeedingEnrichment(ctx context.Context, promptID string, limit int) ([]uuid.UUID, error)
	PendingSuggestions(ctx context.Context, limit int) ([]postgres.Suggestion, error)
	ResolveSuggestion(ctx context.Context, id uuid.UUID, accept bool) error
	BookFilesNeedingChunks(ctx context.Context, strategyVersion, limit int) ([]postgres.ChunkTarget, error)
	RetrievalStats(ctx context.Context, modelID string) (postgres.RetrievalStats, error)
}

// EnrichmentHandler queues enrichment and serves what it produced.
type EnrichmentHandler struct {
	Store EnrichmentStore
	Queue JobQueue
	// ModelConfigured reports whether a model is set up at all, so the client
	// can say "switch one on" rather than "nothing happened".
	ModelConfigured bool
	// EmbedModel is the embedding model's id, which is what embeddings are
	// counted against: a library embedded by one model and queried by another
	// returns nonsense, so the count is per model rather than global.
	EmbedModel string
	// Accounts is the admin check. Enrichment spends the household's model on
	// the shared library, and accepting a suggestion rewrites a book's metadata
	// for everyone.
	Accounts AdminStore
}

// maxEnrichBatch bounds one request.
//
// Enrichment is one model call per book on a machine in someone's house.
// Queueing five thousand at once would be honest but useless: the queue is
// there to be topped up, and a bounded batch keeps the answer to "how far along
// is this?" meaningful.
const maxEnrichBatch = 200

// Start serves POST /api/v1/enrich.
//
// Queues enrichment for books no model has described yet. Safe to call
// repeatedly: the queue deduplicates per work, and works already enriched with
// the current prompt are not selected.
func (h *EnrichmentHandler) Start(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminOrForbidden(w, r, h.Accounts); !ok {
		return
	}
	var body dto.StartEnrichment
	if r.Body != nil {
		// A body is optional: the common case is "enrich whatever needs it".
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body)
	}

	limit := body.Limit
	if limit <= 0 || limit > maxEnrichBatch {
		limit = maxEnrichBatch
	}

	promptID := worker.EnrichPromptID()
	works, err := h.Store.WorksNeedingEnrichment(r.Context(), promptID, limit)
	if err != nil {
		respondError(w, err)
		return
	}

	queued := 0
	for _, workID := range works {
		// The installation's own sweep: nobody in particular asked.
		_, created, err := h.Queue.Enqueue(r.Context(), worker.EnrichJob(workID, uuid.Nil))
		if err != nil {
			respondError(w, err)
			return
		}
		if created {
			queued++
		}
	}

	respondJSON(w, http.StatusAccepted, dto.EnrichmentStarted{
		Queued:          queued,
		Eligible:        len(works),
		PromptID:        promptID,
		ModelConfigured: h.ModelConfigured,
	})
}

// StartIndexing serves POST /api/v1/index.
//
// Queues chunking for books that have none. Chunking needs no model, so this
// works with every machine but the database switched off; embedding follows on
// its own once there is something to embed.
func (h *EnrichmentHandler) StartIndexing(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminOrForbidden(w, r, h.Accounts); !ok {
		return
	}
	targets, err := h.Store.BookFilesNeedingChunks(r.Context(), rag.StrategyVersion, maxEnrichBatch)
	if err != nil {
		respondError(w, err)
		return
	}

	queued := 0
	for _, target := range targets {
		// Ahead of enrichment: see worker.ChunkJob.
		_, created, err := h.Queue.Enqueue(r.Context(), worker.ChunkJob(target.BookFileID))
		if err != nil {
			respondError(w, err)
			return
		}
		if created {
			queued++
		}
	}

	stats, err := h.Store.RetrievalStats(r.Context(), h.EmbedModel)
	if err != nil {
		respondError(w, err)
		return
	}

	respondJSON(w, http.StatusAccepted, dto.IndexingStarted{
		Queued:          queued,
		Eligible:        len(targets),
		StrategyVersion: rag.StrategyVersion,
		Files:           stats.Files,
		IndexedFiles:    stats.ChunkedFiles,
		Chunks:          stats.Chunks,
		EmbeddedChunks:  stats.EmbeddedChunks,
		ModelConfigured: h.EmbedModel != "",
	})
}

// Suggestions serves GET /api/v1/suggestions.
//
// Administrators only, like every other endpoint on this handler. It was the
// one that forgot, and the omission was not cosmetic: the reply carries the
// TITLE and the model-written summary of every book on the installation, and
// PendingSuggestions has no viewer to scope by - so any signed-in account,
// including one that owns no books at all, could read a catalogue of
// everybody else's library.
func (h *EnrichmentHandler) Suggestions(w http.ResponseWriter, r *http.Request) {
	if _, ok := adminOrForbidden(w, r, h.Accounts); !ok {
		return
	}

	items, err := h.Store.PendingSuggestions(r.Context(), 100)
	if err != nil {
		respondError(w, err)
		return
	}

	out := dto.SuggestionList{Items: make([]dto.Suggestion, 0, len(items))}
	for _, s := range items {
		out.Items = append(out.Items, dto.Suggestion{
			ID:          s.ID.String(),
			WorkID:      s.WorkID.String(),
			WorkTitle:   s.WorkTitle,
			Field:       s.Field,
			Value:       json.RawMessage(s.Value),
			Current:     s.Current,
			Model:       s.Model,
			PromptID:    s.PromptID,
			Confidence:  s.Confidence,
			GeneratedAt: s.GeneratedAt,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// Resolve serves POST /api/v1/suggestions/{id}/accept and .../reject.
//
// Accepting is the only path by which a model's value replaces one the book
// itself declared, and it exists because a person asked for it.
func (h *EnrichmentHandler) Resolve(accept bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := adminOrForbidden(w, r, h.Accounts); !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			respondError(w, Invalid("suggestion id must be a UUID", err))
			return
		}

		if err := h.Store.ResolveSuggestion(r.Context(), id, accept); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Already decided, or never existed. Both are "there is nothing
				// here to act on".
				respondError(w, NotFound("suggestion"))
				return
			}
			respondError(w, err)
			return
		}
		respondJSON(w, http.StatusNoContent, nil)
	}
}
