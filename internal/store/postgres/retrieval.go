// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// ChunkInput is one passage ready to store.
type ChunkInput struct {
	Seq          int
	Start        domain.Locator
	End          domain.Locator
	ChapterTitle string
	Content      string
	TokenCount   int
}

// SaveChunks replaces a file's chunks with a new set.
//
// Replace rather than append, because chunking is versioned and re-chunking a
// book with a new strategy must not leave the old passages behind to be
// retrieved alongside the new ones. Deleting cascades to their embeddings,
// which is correct: an embedding of text that no longer exists can only ever
// retrieve a passage nobody can open.
func (p *Pool) SaveChunks(ctx context.Context, bookFileID uuid.UUID, chunks []ChunkInput, strategyVersion int) (int, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("saving chunks: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The file's row, shared, before its passages: a removal for everyone
	// takes it exclusively before deleting them, so a chunking pass that read
	// the bytes just before the removal either commits first and has its
	// passages deleted, or waits and then finds the bytes blocked.
	// Two statements, so the check reads after the wait: one statement's
	// snapshot is taken before it waits, and would miss the block.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM book_files WHERE id = $1 FOR SHARE`, bookFileID); err != nil {
		return 0, fmt.Errorf("saving chunks: %w", mapError(err))
	}
	var blocked bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM book_files bf
		                 JOIN blocked_content b ON b.content_hash = bf.content_hash
		                WHERE bf.id = $1)`, bookFileID).Scan(&blocked); err != nil {
		return 0, fmt.Errorf("saving chunks: %w", mapError(err))
	}
	if blocked {
		return 0, nil
	}

	if _, err := tx.Exec(ctx, `DELETE FROM chunks WHERE book_file_id = $1`, bookFileID); err != nil {
		return 0, fmt.Errorf("clearing old chunks: %w", mapError(err))
	}

	rows := make([][]any, 0, len(chunks))
	for _, c := range chunks {
		if strings.TrimSpace(c.Content) == "" || c.TokenCount <= 0 {
			// The table forbids both, and a silently skipped chunk is better
			// than a failed book.
			continue
		}
		rows = append(rows, []any{
			bookFileID, c.Seq,
			majorOf(c.Start), c.Start.ElementPath, c.Start.CharOffset,
			majorOf(c.End), c.End.ElementPath, c.End.CharOffset,
			nullableText(c.ChapterTitle), c.Content, c.TokenCount, strategyVersion,
		})
	}

	if len(rows) > 0 {
		// CopyFrom rather than a loop of inserts: a large book is thousands of
		// chunks, and the round trips dominate everything else.
		_, err := tx.CopyFrom(ctx,
			pgx.Identifier{"chunks"},
			[]string{"book_file_id", "seq", "start_major", "start_path", "start_offset",
				"end_major", "end_path", "end_offset", "chapter_title", "content",
				"token_count", "strategy_version"},
			pgx.CopyFromRows(rows))
		if err != nil {
			return 0, fmt.Errorf("writing chunks: %w", mapError(err))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("saving chunks: %w", mapError(err))
	}
	return len(rows), nil
}

// BookFilesNeedingChunks lists readable files with no chunks at the current
// strategy version.
func (p *Pool) BookFilesNeedingChunks(ctx context.Context, strategyVersion, limit int) ([]ChunkTarget, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}

	rows, err := p.Query(ctx, `
		SELECT bf.id, bf.path, bf.format
		FROM book_files bf
		WHERE `+needsChunks("$1")+`
		ORDER BY bf.created_at
		LIMIT $2`, strategyVersion, limit)
	if err != nil {
		return nil, fmt.Errorf("finding files to chunk: %w", mapError(err))
	}
	defer rows.Close()

	var out []ChunkTarget
	for rows.Next() {
		var t ChunkTarget
		if err := rows.Scan(&t.BookFileID, &t.Path, &t.Format); err != nil {
			return nil, fmt.Errorf("scanning chunk target: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// needsChunks is the one definition of a file waiting to be chunked, over
// book_files as bf, with the strategy version in the placeholder given. Shared
// by the sweep and the single-file enqueue, so the two cannot drift apart.
func needsChunks(version string) string {
	return `bf.missing_since IS NULL
		  AND bf.format IN ('epub', 'pdf')
		  -- A scan with no text layer has nothing to chunk, and trying again on
		  -- every sweep would be a permanent queue of work that cannot succeed.
		  AND bf.status <> 'needs_ocr'
		  AND NOT EXISTS (
			SELECT 1 FROM chunks c
			WHERE c.book_file_id = bf.id AND c.strategy_version = ` + version + `
		  )`
}

// EnqueueIfNeedsChunks queues opts - the chunk job for the file named by
// opts.TargetID - only if that file is waiting to be chunked under
// strategyVersion, by the same rule BookFilesNeedingChunks sweeps with. It
// reports whether a job was added.
//
// One statement, so the check and the insert see the same state: there is no
// gap between them for a chunking to finish in and be repeated. A live job
// for the same file already in the queue is left alone, as Enqueue does.
func (p *Pool) EnqueueIfNeedsChunks(ctx context.Context, opts EnqueueOptions, strategyVersion int) (bool, error) {
	if opts.TargetID == nil {
		return false, fmt.Errorf("enqueuing %s: the target file is required", opts.Type)
	}
	payload, maxAttempts, runAfter, err := jobValues(opts)
	if err != nil {
		return false, err
	}

	tag, err := p.Exec(ctx, `
		INSERT INTO jobs (type, target_id, payload, priority, max_attempts, run_after)
		SELECT $1, bf.id, $3, $4, $5, $6
		FROM book_files bf
		WHERE bf.id = $2 AND `+needsChunks("$7")+`
		ON CONFLICT (type, target_id)
			WHERE status IN ('pending', 'running') AND target_id IS NOT NULL
			DO NOTHING`,
		opts.Type, *opts.TargetID, payload, priorityOf(opts), maxAttempts, runAfter, strategyVersion)
	if err != nil {
		return false, fmt.Errorf("enqueuing %s for %s: %w", opts.Type, *opts.TargetID, mapError(err))
	}
	return tag.RowsAffected() == 1, nil
}

// ChunkTarget is a file waiting to be chunked.
type ChunkTarget struct {
	BookFileID uuid.UUID
	Path       string
	Format     domain.Format
}

// PendingChunk is a stored passage with no embedding yet.
type PendingChunk struct {
	ID      uuid.UUID
	Content string
}

// ChunksNeedingEmbedding lists chunks with no vector from this model.
//
// Keyed on the model: a library embedded by one model and queried by another
// returns nonsense, so changing models means re-embedding, and this is what
// makes that visible rather than silent.
func (p *Pool) ChunksNeedingEmbedding(ctx context.Context, modelID string, limit int) ([]PendingChunk, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}

	rows, err := p.Query(ctx, `
		SELECT c.id, c.content
		FROM chunks c
		WHERE NOT EXISTS (
			SELECT 1 FROM embeddings e
			WHERE e.chunk_id = c.id AND e.model_id = $1
		)
		ORDER BY c.book_file_id, c.seq
		LIMIT $2`, modelID, limit)
	if err != nil {
		return nil, fmt.Errorf("finding chunks to embed: %w", mapError(err))
	}
	defer rows.Close()

	var out []PendingChunk
	for rows.Next() {
		var c PendingChunk
		if err := rows.Scan(&c.ID, &c.Content); err != nil {
			return nil, fmt.Errorf("scanning pending chunk: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// EmbeddingInput is one vector to store.
type EmbeddingInput struct {
	ChunkID uuid.UUID
	Vector  []float32
}

// SaveEmbeddings stores vectors for chunks.
func (p *Pool) SaveEmbeddings(ctx context.Context, modelID string, dimensions int, items []EmbeddingInput) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}

	ids := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		if len(item.Vector) != dimensions {
			return 0, fmt.Errorf("chunk %s has a %d-dimensional vector, but this library stores %d",
				item.ChunkID, len(item.Vector), dimensions)
		}
		ids = append(ids, item.ChunkID)
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("saving embeddings: %w", mapError(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// A chunk may already have a vector from a previous run that was
	// interrupted after writing but before the job was marked done. Jobs are
	// idempotent by rule, so this deletes first rather than conflicting.
	if _, err := tx.Exec(ctx,
		`DELETE FROM embeddings WHERE model_id = $1 AND chunk_id = ANY($2)`, modelID, ids); err != nil {
		return 0, fmt.Errorf("clearing previous embeddings: %w", mapError(err))
	}

	// A multi-row INSERT rather than CopyFrom.
	//
	// CopyFrom sends values in the BINARY protocol, and pgx has no codec for
	// pgvector's type - so the text literal went down the wire as binary, whose
	// first bytes pgvector reads as a dimension count. It reported "vector
	// cannot have more than 16000 dimensions", which is what a length prefix
	// looks like when it is really the characters "[0".
	//
	// An explicit ::vector cast on a text parameter avoids the whole problem
	// without a dependency for one column type.
	const columns = 4
	values := make([]string, 0, len(items))
	args := make([]any, 0, len(items)*columns)

	for n, item := range items {
		base := n * columns
		values = append(values, fmt.Sprintf("($%d, $%d, $%d, $%d::vector)",
			base+1, base+2, base+3, base+4))
		args = append(args, item.ChunkID, modelID, dimensions, vectorLiteral(item.Vector))
	}

	sql := `INSERT INTO embeddings (chunk_id, model_id, dimensions, vector) VALUES ` +
		strings.Join(values, ", ")
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return 0, fmt.Errorf("writing embeddings: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("saving embeddings: %w", mapError(err))
	}
	return len(items), nil
}

// vectorLiteral renders a vector in pgvector's text form.
//
// Text rather than a binary codec because the alternative is another
// dependency for one type. pgvector parses "[1,2,3]" on input, and a chunk's
// worth of floats is a few kilobytes - next to the cost of producing the
// embedding in the first place, the encoding is not worth a library.
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 12)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

func nullableText(s string) *string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return &s
}

// RetrievalStats reports how much of the library is searchable.
type RetrievalStats struct {
	Files          int
	ChunkedFiles   int
	Chunks         int
	EmbeddedChunks int
}

// RetrievalStats counts what has been indexed, for the UI and for diagnosis.
func (p *Pool) RetrievalStats(ctx context.Context, modelID string) (RetrievalStats, error) {
	var s RetrievalStats
	err := p.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM book_files
			  WHERE missing_since IS NULL AND format IN ('epub', 'pdf') AND status <> 'needs_ocr'),
			(SELECT count(DISTINCT book_file_id) FROM chunks),
			(SELECT count(*) FROM chunks),
			(SELECT count(*) FROM embeddings WHERE model_id = $1)`, modelID,
	).Scan(&s.Files, &s.ChunkedFiles, &s.Chunks, &s.EmbeddedChunks)
	if err != nil {
		return RetrievalStats{}, fmt.Errorf("reading retrieval stats: %w", mapError(err))
	}
	return s, nil
}
