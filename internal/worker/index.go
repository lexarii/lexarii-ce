// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// IndexStore is the slice of the store the indexing handlers need.
type IndexStore interface {
	BookFile(ctx context.Context, id uuid.UUID) (domain.BookFile, error)
	SaveChunks(ctx context.Context, bookFileID uuid.UUID, chunks []postgres.ChunkInput, strategyVersion int) (int, error)
	ChunksNeedingEmbedding(ctx context.Context, modelID string, limit int) ([]postgres.PendingChunk, error)
	SaveEmbeddings(ctx context.Context, modelID string, dimensions int, items []postgres.EmbeddingInput) (int, error)
	Enqueue(ctx context.Context, opts postgres.EnqueueOptions) (uuid.UUID, bool, error)
}

// ChunkPayload names the file to split.
type ChunkPayload struct {
	BookFileID string `json:"bookFileId"`
}

// ChunkJob is the job that chunks one file.
//
// Every place that asks for chunking asks with this, so the jobs are the same
// job: targeted at the file, which is one content hash, so the queue keeps at
// most one waiting per file however many places ask for it.
func ChunkJob(bookFileID uuid.UUID) postgres.EnqueueOptions {
	return postgres.EnqueueOptions{
		Type:     domain.JobChunk,
		TargetID: &bookFileID,
		Payload:  ChunkPayload{BookFileID: bookFileID.String()},
		// Its place in the queue is its type's: after extraction, ahead of
		// embedding and enrichment (domain.JobType.Priority).
	}
}

// EmbedPayload bounds one embedding pass.
//
// Deliberately not per-book: embedding is a queue of small requests to one
// server, and batching across books keeps it busy rather than paying a round
// trip per chapter.
type EmbedPayload struct {
	Limit int `json:"limit,omitempty"`
}

// Index turns books into something searchable.
//
// Chunking and embedding are separate jobs because only one of them needs the
// model. A library can be fully chunked with the GPU machine switched off, and
// then embedded later - and if the embedding model changes, the chunks are
// still good.
type Index struct {
	Store  IndexStore
	Client *llm.Client
	Logger *slog.Logger

	// Dimensions is the width the vector column is fixed at.
	Dimensions int
	// EmbedBatch bounds one embed job. Defaults to DefaultEmbedBatch.
	EmbedBatch int
}

// embedJobTarget deduplicates embed jobs.
//
// An embed job means "there is embedding work to do", and one is as good as
// twenty: it drains the queue and re-queues itself while anything remains. But
// the queue only deduplicates jobs that name a target, so without one, chunking
// 159 books enqueued 159 identical embed jobs - 158 of which would find nothing
// left and exit.
//
// A fixed id is the target: there is exactly one embedding backlog, so it is a
// genuine single thing to name rather than a trick to get deduplication.
var embedJobTarget = uuid.MustParse("e3b0c442-98fc-1c14-9afb-f4c8996fb924")

// DefaultEmbedBatch is how many chunks one embed job handles.
//
// Bounded so the job reports progress and can be interrupted: a library of a
// hundred thousand chunks should not be one job that either finishes or does
// not.
const DefaultEmbedBatch = 200

// Handlers returns the job handlers this indexer provides.
func (i *Index) Handlers() map[domain.JobType]Handler {
	return map[domain.JobType]Handler{
		domain.JobChunk: i.Chunk,
		domain.JobEmbed: i.Embed,
	}
}

// Chunk splits one book into passages.
func (i *Index) Chunk(ctx context.Context, job postgres.Job, report ProgressFunc) error {
	var payload ChunkPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("chunk payload: %w", err)
	}
	bookFileID, err := uuid.Parse(payload.BookFileID)
	if err != nil {
		return fmt.Errorf("chunk payload: book file id is not a UUID: %w", err)
	}

	report(0.1, "reading the book")

	file, err := i.Store.BookFile(ctx, bookFileID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			i.logger().Info("chunk: the file is gone", "bookFileId", bookFileID)
			return nil
		}
		return fmt.Errorf("loading book file %s: %w", bookFileID, err)
	}

	units, err := rag.Units(ctx, file.Path, file.Format)
	if err != nil {
		if errors.Is(err, rag.ErrNoText) {
			// A scan with no OCR, or a comic. Perfectly good to read, nothing to
			// search, and not a failure.
			i.logger().Info("chunk: nothing to index",
				"bookFileId", bookFileID, "format", file.Format, "reason", err)
			return nil
		}
		return fmt.Errorf("reading %s: %w", file.Path, err)
	}

	report(0.5, "splitting into passages")

	chunks := rag.Split(units, rag.Options{Format: file.Format})
	if len(chunks) == 0 {
		i.logger().Info("chunk: the book produced no passages", "bookFileId", bookFileID)
		return nil
	}

	stored := make([]postgres.ChunkInput, 0, len(chunks))
	for _, c := range chunks {
		stored = append(stored, postgres.ChunkInput{
			Seq:          c.Seq,
			Start:        c.Start,
			End:          c.End,
			ChapterTitle: c.ChapterTitle,
			Content:      c.Content,
			TokenCount:   c.TokenCount,
		})
	}

	report(0.8, "storing passages")

	written, err := i.Store.SaveChunks(ctx, bookFileID, stored, rag.StrategyVersion)
	if err != nil {
		return fmt.Errorf("saving chunks for %s: %w", bookFileID, err)
	}

	i.logger().Info("chunked",
		"bookFileId", bookFileID, "units", len(units), "chunks", written,
		"strategy", rag.StrategyVersion)

	// Embedding follows, but only as a nudge: the embed job is not per-book, so
	// a queued one already covers this work.
	if err := i.queueEmbedding(ctx); err != nil {
		// Not worth failing a good chunking over; the next sweep enqueues it.
		i.logger().Warn("could not queue embedding", "error", err)
	}

	report(1, "done")
	return nil
}

// Embed turns stored passages into vectors.
//
// One job drains the whole backlog, in batches, rather than one job per batch.
//
// Per-batch jobs were tried and cannot work: the job holds the sentinel target
// that deduplicates embedding work, so the successor it enqueues collides with
// the job enqueueing it and is silently dropped. Draining here is also simply
// more honest - "embed what is outstanding" is one piece of work, it reports
// progress as it goes, and because the backlog is a query rather than a list,
// an interrupted run resumes exactly where it stopped.
func (i *Index) Embed(ctx context.Context, job postgres.Job, report ProgressFunc) error {
	if i.Client == nil {
		return fmt.Errorf("%w: no embedding model is configured", ErrSkipped)
	}

	var payload EmbedPayload
	if len(job.Payload) > 0 {
		_ = json.Unmarshal(job.Payload, &payload)
	}
	limit := payload.Limit
	if limit <= 0 {
		limit = i.batch()
	}

	total := 0
	// Passages the model refused during this run.
	//
	// A refused passage is never stored, so the backlog query returns it again
	// on the next pass - which would loop for ever. Remembering them here ends
	// the run cleanly; a later run will try them once more, which is right,
	// because the reason may have been the server's configuration rather than
	// the text.
	refusedThisRun := make(map[uuid.UUID]bool)

	for batch := 0; ; batch++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		found, err := i.Store.ChunksNeedingEmbedding(ctx, i.Client.Model(), limit)
		if err != nil {
			return fmt.Errorf("finding chunks to embed: %w", err)
		}

		pending := found[:0:0]
		for _, c := range found {
			if !refusedThisRun[c.ID] {
				pending = append(pending, c)
			}
		}
		if len(pending) == 0 {
			// Either the backlog is empty, or everything left in it was
			// refused. Both mean this run has nothing more to do.
			break
		}

		texts := make([]string, len(pending))
		for n, c := range pending {
			texts[n] = c.Content
		}

		// The figure does not climb towards 1, because how much is left is not
		// known without counting the whole backlog on every batch. It reports
		// that work is happening, which is what keeps the job from looking
		// abandoned.
		report(0.5, fmt.Sprintf("embedded %d passages", total))

		vectors, refused, err := i.Client.EmbedTolerant(ctx, texts, i.Dimensions)
		if err != nil {
			var mismatch *llm.ErrDimensionMismatch
			if errors.As(err, &mismatch) {
				// A configuration problem, not a transient one: waiting will not
				// make the model return different-sized vectors.
				return fmt.Errorf("embedding: %w", err)
			}
			if transient(err) {
				// Whatever was stored stays stored: the backlog is a query, so
				// the next run continues rather than starting again.
				i.logger().Info("embedding paused", "embedded", total, "reason", err)
				return fmt.Errorf("%w: %w", ErrSkipped, err)
			}
			return fmt.Errorf("embedding %d passages: %w", len(texts), err)
		}

		for _, n := range refused {
			// Named individually, because a passage that can never be embedded
			// is a real gap in what the library can answer from.
			refusedThisRun[pending[n].ID] = true
			i.logger().Warn("passage could not be embedded",
				"chunkId", pending[n].ID, "chars", len(pending[n].Content))
		}

		items := make([]postgres.EmbeddingInput, 0, len(vectors))
		for n, v := range vectors {
			if v == nil {
				continue // refused, and already reported
			}
			items = append(items, postgres.EmbeddingInput{ChunkID: pending[n].ID, Vector: v})
		}

		if len(items) == 0 {
			// Every passage in this batch was refused. They are remembered, so
			// the next pass moves on rather than asking again.
			continue
		}

		written, err := i.Store.SaveEmbeddings(ctx, i.Client.Model(), i.Dimensions, items)
		if err != nil {
			return fmt.Errorf("saving embeddings: %w", err)
		}
		total += written
	}

	if total > 0 || len(refusedThisRun) > 0 {
		i.logger().Info("embedded",
			"passages", total, "refused", len(refusedThisRun), "model", i.Client.Model())
	}
	report(1, "done")
	return nil
}

// queueEmbedding asks for an embedding pass, at most one at a time.
func (i *Index) queueEmbedding(ctx context.Context) error {
	target := embedJobTarget
	_, _, err := i.Store.Enqueue(ctx, postgres.EnqueueOptions{
		Type:     domain.JobEmbed,
		TargetID: &target,
		Payload:  EmbedPayload{},
		// After chunking and ahead of enrichment (domain.JobType.Priority):
		// what the library owes its user first is discovery, then being
		// searchable, then being described. A batch of embeddings takes
		// seconds; queued behind a hundred and fifty enrichments at a minute
		// each, search would not work until tomorrow.
	})
	return err
}

func (i *Index) batch() int {
	if i.EmbedBatch > 0 {
		return i.EmbedBatch
	}
	return DefaultEmbedBatch
}

func (i *Index) logger() *slog.Logger {
	if i.Logger != nil {
		return i.Logger
	}
	return slog.Default()
}
