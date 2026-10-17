// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"slices"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
)

// retrievalChunk is a stored passage as a test reads it back.
type retrievalChunk struct {
	seq      int
	content  string
	chapter  *string
	version  int
	startMaj int
}

func storedChunksOf(t *testing.T, p *Pool, bookFileID uuid.UUID) []retrievalChunk {
	t.Helper()
	rows, err := p.Query(t.Context(), `
		SELECT seq, content, chapter_title, strategy_version, start_major
		FROM chunks WHERE book_file_id = $1 ORDER BY seq`, bookFileID)
	if err != nil {
		t.Fatalf("reading chunks: %v", err)
	}
	defer rows.Close()
	var out []retrievalChunk
	for rows.Next() {
		var c retrievalChunk
		if err := rows.Scan(&c.seq, &c.content, &c.chapter, &c.version, &c.startMaj); err != nil {
			t.Fatalf("scanning chunk: %v", err)
		}
		out = append(out, c)
	}
	return out
}

func retrievalPassage(seq int, text string, tokens int) ChunkInput {
	at := domain.Locator{Format: domain.FormatEPUB, SpineIndex: seq + 1, ElementPath: "body/p[1]"}
	end := at
	end.CharOffset = len(text)
	return ChunkInput{Seq: seq, Start: at, End: end, Content: text, TokenCount: tokens}
}

// Re-chunking replaces: a new strategy must not leave the old passages behind
// to be retrieved beside the new ones, nor their vectors.
func TestSaveChunksReplacesAFilesPassages(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	bookFile := oneBook(t, p, testOwner(t, p))

	first := []ChunkInput{
		retrievalPassage(0, "The first version of the opening.", 7),
		retrievalPassage(1, "The first version of the close.", 7),
	}
	first[0].ChapterTitle = "Opening"
	n, err := p.SaveChunks(ctx, bookFile, first, 1)
	if err != nil || n != 2 {
		t.Fatalf("SaveChunks = %d, %v; want 2", n, err)
	}
	var oldID uuid.UUID
	if err := p.QueryRow(ctx, `SELECT id FROM chunks WHERE book_file_id = $1 AND seq = 0`, bookFile).Scan(&oldID); err != nil {
		t.Fatalf("finding a chunk: %v", err)
	}
	if _, err := p.SaveEmbeddings(ctx, searchModel, searchTestDims,
		[]EmbeddingInput{{ChunkID: oldID, Vector: searchBasisVector(5)}}); err != nil {
		t.Fatalf("SaveEmbeddings: %v", err)
	}

	second := []ChunkInput{
		retrievalPassage(0, "The second version, all in one.", 6),
		// Neither of these can be stored, and neither may fail the book.
		retrievalPassage(1, "   \n\t ", 4),
		retrievalPassage(2, "A passage with no tokens counted.", 0),
	}
	second[0].ChapterTitle = "   "
	n, err = p.SaveChunks(ctx, bookFile, second, 2)
	if err != nil {
		t.Fatalf("SaveChunks (again): %v", err)
	}
	if n != 1 {
		t.Fatalf("SaveChunks reported %d written, want 1 (the blank and the empty skipped)", n)
	}

	got := storedChunksOf(t, p, bookFile)
	if len(got) != 1 {
		t.Fatalf("%d chunks stored after re-chunking, want 1: %+v", len(got), got)
	}
	if got[0].content != "The second version, all in one." || got[0].version != 2 || got[0].startMaj != 1 {
		t.Errorf("stored chunk = %+v", got[0])
	}
	if got[0].chapter != nil {
		t.Errorf("a blank chapter title was stored as %q, want NULL", *got[0].chapter)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM embeddings WHERE chunk_id = $1`, oldID); n != 0 {
		t.Errorf("%d embeddings of a deleted passage survived", n)
	}

	// An empty set clears the file.
	if n, err := p.SaveChunks(ctx, bookFile, nil, 3); err != nil || n != 0 {
		t.Fatalf("SaveChunks (empty) = %d, %v", n, err)
	}
	if got := storedChunksOf(t, p, bookFile); len(got) != 0 {
		t.Errorf("%d chunks left after saving none", len(got))
	}
}

// Bytes an administrator removed for everybody are not indexed again by a
// chunking pass that read them first.
func TestSaveChunksWritesNothingForBlockedBytes(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	bookFile := oneBook(t, p, testOwner(t, p))

	if _, err := p.Exec(ctx, `
		INSERT INTO blocked_content (content_hash, reason)
		SELECT content_hash, 'test' FROM book_files WHERE id = $1`, bookFile); err != nil {
		t.Fatalf("blocking: %v", err)
	}
	n, err := p.SaveChunks(ctx, bookFile, []ChunkInput{retrievalPassage(0, "Words.", 1)}, 1)
	if err != nil {
		t.Fatalf("SaveChunks: %v", err)
	}
	if n != 0 {
		t.Errorf("SaveChunks wrote %d passages of blocked bytes", n)
	}
	if got := storedChunksOf(t, p, bookFile); len(got) != 0 {
		t.Errorf("%d passages of blocked bytes stored", len(got))
	}
}

func TestEnqueueIfNeedsChunksRequiresATarget(t *testing.T) {
	p := testPool(t)
	added, err := p.EnqueueIfNeedsChunks(t.Context(), EnqueueOptions{Type: domain.JobChunk}, 1)
	if err == nil || added {
		t.Fatalf("enqueued without a target: added %v, err %v", added, err)
	}
}

// What needs embedding is keyed on the model: switching models makes every
// passage pending again rather than silently mixing two vector spaces.
func TestChunksNeedingEmbeddingIsPerModel(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	bookFile := oneBook(t, p, testOwner(t, p))

	if _, err := p.SaveChunks(ctx, bookFile, []ChunkInput{
		retrievalPassage(0, "First passage.", 2),
		retrievalPassage(1, "Second passage.", 2),
		retrievalPassage(2, "Third passage.", 2),
	}, 1); err != nil {
		t.Fatalf("SaveChunks: %v", err)
	}

	pending, err := p.ChunksNeedingEmbedding(ctx, searchModel, 0)
	if err != nil {
		t.Fatalf("ChunksNeedingEmbedding: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("%d pending, want 3", len(pending))
	}
	// In reading order, so a long book is embedded front to back.
	wantText := []string{"First passage.", "Second passage.", "Third passage."}
	for i, c := range pending {
		if c.Content != wantText[i] {
			t.Errorf("pending[%d] = %q, want %q", i, c.Content, wantText[i])
		}
	}

	if limited, err := p.ChunksNeedingEmbedding(ctx, searchModel, 2); err != nil || len(limited) != 2 {
		t.Fatalf("limit 2 returned %d (err %v)", len(limited), err)
	}

	if n, err := p.SaveEmbeddings(ctx, searchModel, searchTestDims, []EmbeddingInput{
		{ChunkID: pending[0].ID, Vector: searchBasisVector(0)},
		{ChunkID: pending[1].ID, Vector: searchBasisVector(1)},
	}); err != nil || n != 2 {
		t.Fatalf("SaveEmbeddings = %d, %v", n, err)
	}

	left, err := p.ChunksNeedingEmbedding(ctx, searchModel, 10)
	if err != nil {
		t.Fatalf("ChunksNeedingEmbedding: %v", err)
	}
	if len(left) != 1 || left[0].ID != pending[2].ID {
		t.Fatalf("after embedding two, pending = %+v, want only the third", left)
	}

	other, err := p.ChunksNeedingEmbedding(ctx, searchOtherModel, 10)
	if err != nil {
		t.Fatalf("ChunksNeedingEmbedding (other model): %v", err)
	}
	if len(other) != 3 {
		t.Fatalf("another model sees %d pending, want all 3", len(other))
	}

	stats, err := p.RetrievalStats(ctx, searchModel)
	if err != nil {
		t.Fatalf("RetrievalStats: %v", err)
	}
	if stats != (RetrievalStats{Files: 1, ChunkedFiles: 1, Chunks: 3, EmbeddedChunks: 2}) {
		t.Errorf("stats = %+v", stats)
	}
	stats, err = p.RetrievalStats(ctx, searchOtherModel)
	if err != nil {
		t.Fatalf("RetrievalStats: %v", err)
	}
	if stats.EmbeddedChunks != 0 {
		t.Errorf("another model reports %d embedded", stats.EmbeddedChunks)
	}
}

func TestSaveEmbeddings(t *testing.T) {
	p := testPool(t)
	cleanLibrary(t, p)
	ctx := t.Context()
	bookFile := oneBook(t, p, testOwner(t, p))
	if _, err := p.SaveChunks(ctx, bookFile, []ChunkInput{
		retrievalPassage(0, "One.", 1), retrievalPassage(1, "Two.", 1),
	}, 1); err != nil {
		t.Fatalf("SaveChunks: %v", err)
	}
	pending, err := p.ChunksNeedingEmbedding(ctx, searchModel, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("ChunksNeedingEmbedding = %d, %v", len(pending), err)
	}
	a, b := pending[0].ID, pending[1].ID

	t.Run("nothing to save is not an error", func(t *testing.T) {
		if n, err := p.SaveEmbeddings(ctx, searchModel, searchTestDims, nil); err != nil || n != 0 {
			t.Fatalf("SaveEmbeddings(nil) = %d, %v", n, err)
		}
	})

	t.Run("a vector of the wrong width writes nothing at all", func(t *testing.T) {
		n, err := p.SaveEmbeddings(ctx, searchModel, searchTestDims, []EmbeddingInput{
			{ChunkID: a, Vector: searchBasisVector(0)},
			{ChunkID: b, Vector: []float32{1, 2, 3}},
		})
		if err == nil || n != 0 {
			t.Fatalf("SaveEmbeddings = %d, %v; want an error", n, err)
		}
		if got := rowCount(t, p, `SELECT count(*) FROM embeddings WHERE chunk_id = ANY($1)`,
			[]uuid.UUID{a, b}); got != 0 {
			t.Fatalf("%d vectors written from a rejected batch", got)
		}
	})

	t.Run("a width the column does not hold is refused by the database", func(t *testing.T) {
		n, err := p.SaveEmbeddings(ctx, searchModel, 3, []EmbeddingInput{{ChunkID: a, Vector: []float32{1, 0, 0}}})
		if err == nil || n != 0 {
			t.Fatalf("SaveEmbeddings (3 wide) = %d, %v; want an error", n, err)
		}
	})

	t.Run("saving again replaces rather than conflicts", func(t *testing.T) {
		for _, axis := range []int{0, 7} {
			n, err := p.SaveEmbeddings(ctx, searchModel, searchTestDims,
				[]EmbeddingInput{{ChunkID: a, Vector: searchBasisVector(axis)}})
			if err != nil || n != 1 {
				t.Fatalf("SaveEmbeddings = %d, %v", n, err)
			}
		}
		if got := rowCount(t, p, `SELECT count(*) FROM embeddings WHERE chunk_id = $1 AND model_id = $2`,
			a, searchModel); got != 1 {
			t.Fatalf("%d vectors for one chunk under one model, want 1", got)
		}
		// The second write is the one kept.
		var axis7 float64
		if err := p.QueryRow(ctx, `
			SELECT (vector::real[])[8] FROM embeddings WHERE chunk_id = $1 AND model_id = $2`,
			a, searchModel).Scan(&axis7); err != nil {
			t.Fatalf("reading the vector: %v", err)
		}
		if axis7 != 1 {
			t.Errorf("stored vector has %v on axis 7, want the replacement's 1", axis7)
		}
	})

	t.Run("another model's vector sits beside it", func(t *testing.T) {
		if _, err := p.SaveEmbeddings(ctx, searchOtherModel, searchTestDims,
			[]EmbeddingInput{{ChunkID: a, Vector: searchBasisVector(1)}}); err != nil {
			t.Fatalf("SaveEmbeddings: %v", err)
		}
		var models []string
		rows, err := p.Query(ctx, `SELECT model_id FROM embeddings WHERE chunk_id = $1 ORDER BY model_id`, a)
		if err != nil {
			t.Fatalf("reading models: %v", err)
		}
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				t.Fatalf("scanning: %v", err)
			}
			models = append(models, m)
		}
		rows.Close()
		if want := []string{searchOtherModel, searchModel}; !slices.Equal(models, want) {
			t.Fatalf("models = %v, want %v", models, want)
		}
	})
}

func TestVectorLiteralIsPgvectorsTextForm(t *testing.T) {
	for _, tc := range []struct {
		in   []float32
		want string
	}{
		{nil, "[]"},
		{[]float32{1}, "[1]"},
		{[]float32{0.5, -2, 0.1}, "[0.5,-2,0.1]"},
		{[]float32{1e-8, 3e10}, "[1e-08,3e+10]"},
	} {
		if got := vectorLiteral(tc.in); got != tc.want {
			t.Errorf("vectorLiteral(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
