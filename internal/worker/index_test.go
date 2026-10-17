// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// fakeIndexStore is an in-memory IndexStore: one file, the passages saved for
// it, and the vectors saved for those passages, keyed by model.
type fakeIndexStore struct {
	mu sync.Mutex

	file    domain.BookFile
	fileErr error

	savedChunks   []postgres.ChunkInput
	savedStrategy int
	saveChunksErr error

	// pending is the backlog ChunksNeedingEmbedding answers from, in order.
	pending     []postgres.PendingChunk
	pendingErr  error
	askedLimits []int
	askedModels []string

	embedded      map[string]map[uuid.UUID][]float32 // model -> chunk -> vector
	savedDims     []int
	saveEmbedErr  error
	enqueued      []postgres.EnqueueOptions
	enqueueErr    error
	saveEmbedHook func()
}

func newFakeIndexStore() *fakeIndexStore {
	return &fakeIndexStore{embedded: map[string]map[uuid.UUID][]float32{}}
}

func (s *fakeIndexStore) BookFile(_ context.Context, id uuid.UUID) (domain.BookFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileErr != nil {
		return domain.BookFile{}, s.fileErr
	}
	if s.file.ID != id {
		return domain.BookFile{}, store.ErrNotFound
	}
	return s.file, nil
}

func (s *fakeIndexStore) SaveChunks(_ context.Context, _ uuid.UUID, chunks []postgres.ChunkInput, strategyVersion int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveChunksErr != nil {
		return 0, s.saveChunksErr
	}
	s.savedChunks = chunks
	s.savedStrategy = strategyVersion
	return len(chunks), nil
}

func (s *fakeIndexStore) ChunksNeedingEmbedding(_ context.Context, modelID string, limit int) ([]postgres.PendingChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.askedLimits = append(s.askedLimits, limit)
	s.askedModels = append(s.askedModels, modelID)
	if s.pendingErr != nil {
		return nil, s.pendingErr
	}
	var out []postgres.PendingChunk
	for _, c := range s.pending {
		if _, done := s.embedded[modelID][c.ID]; done {
			continue
		}
		out = append(out, c)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeIndexStore) SaveEmbeddings(_ context.Context, modelID string, dimensions int, items []postgres.EmbeddingInput) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveEmbedHook != nil {
		s.saveEmbedHook()
	}
	if s.saveEmbedErr != nil {
		return 0, s.saveEmbedErr
	}
	if s.embedded[modelID] == nil {
		s.embedded[modelID] = map[uuid.UUID][]float32{}
	}
	for _, item := range items {
		s.embedded[modelID][item.ChunkID] = item.Vector
	}
	s.savedDims = append(s.savedDims, dimensions)
	return len(items), nil
}

func (s *fakeIndexStore) Enqueue(_ context.Context, opts postgres.EnqueueOptions) (uuid.UUID, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enqueueErr != nil {
		return uuid.Nil, false, s.enqueueErr
	}
	s.enqueued = append(s.enqueued, opts)
	return uuid.New(), true, nil
}

func (s *fakeIndexStore) vectorsFor(model string) map[uuid.UUID][]float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID][]float32{}
	for id, v := range s.embedded[model] {
		out[id] = v
	}
	return out
}

// fakeEmbedder is an OpenAI-compatible /embeddings endpoint. It answers every
// input with a vector of the configured width, and refuses (400) any request
// containing an input marked as unembeddable, the way llama.cpp refuses an
// input larger than its batch.
type fakeEmbedder struct {
	server *httptest.Server

	mu         sync.Mutex
	dimensions int
	requests   int
	inputs     []string
	models     []string
}

const fakeEmbedderRefuse = "UNEMBEDDABLE"

func newFakeEmbedder(t *testing.T, dimensions int) *fakeEmbedder {
	t.Helper()
	e := &fakeEmbedder{dimensions: dimensions}
	e.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		e.mu.Lock()
		e.requests++
		e.models = append(e.models, req.Model)
		e.inputs = append(e.inputs, req.Input...)
		dims := e.dimensions
		e.mu.Unlock()

		for _, in := range req.Input {
			if strings.Contains(in, fakeEmbedderRefuse) {
				http.Error(w, `{"error":{"message":"input is too large"}}`, http.StatusBadRequest)
				return
			}
		}

		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]item, len(req.Input))
		for i := range req.Input {
			v := make([]float32, dims)
			v[i%dims] = 1
			data[i] = item{Index: i, Embedding: v}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": req.Model})
	}))
	t.Cleanup(e.server.Close)
	return e
}

func (e *fakeEmbedder) client(model string) *llm.Client {
	return llm.New(config.ModelConfig{
		BaseURL: e.server.URL + "/v1", Model: model,
		MaxConcurrency: 1, Timeout: 5 * time.Second,
	})
}

func (e *fakeEmbedder) requestCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests
}

// progressLog records every progress report a handler makes.
type progressLog struct {
	mu        sync.Mutex
	fractions []float32
}

func (p *progressLog) report(fraction float32, _ string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fractions = append(p.fractions, fraction)
}

func (p *progressLog) last() float32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.fractions) == 0 {
		return -1
	}
	return p.fractions[len(p.fractions)-1]
}

func chunkJobFor(t *testing.T, id uuid.UUID) postgres.Job {
	t.Helper()
	payload, err := json.Marshal(ChunkPayload{BookFileID: id.String()})
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	return postgres.Job{ID: uuid.New(), Type: domain.JobChunk, Payload: payload}
}

func TestIndexHandlersCoverChunkAndEmbed(t *testing.T) {
	handlers := (&Index{}).Handlers()
	for _, typ := range []domain.JobType{domain.JobChunk, domain.JobEmbed} {
		if handlers[typ] == nil {
			t.Errorf("no handler for %s", typ)
		}
	}
	if len(handlers) != 2 {
		t.Errorf("handlers = %d, want exactly chunk and embed", len(handlers))
	}
}

func TestChunkJobIsKeyedOnTheFile(t *testing.T) {
	id := uuid.New()
	opts := ChunkJob(id)
	if opts.Type != domain.JobChunk {
		t.Errorf("type = %s, want chunk", opts.Type)
	}
	if opts.TargetID == nil || *opts.TargetID != id {
		t.Errorf("target = %v, want the file %s", opts.TargetID, id)
	}
	if p, ok := opts.Payload.(ChunkPayload); !ok || p.BookFileID != id.String() {
		t.Errorf("payload = %#v, want the file id", opts.Payload)
	}
}

func TestChunkSplitsAFileAndAsksForEmbedding(t *testing.T) {
	path := writeEPUB(t, "<h1>Chapter One</h1><p>The quick brown fox jumps over the lazy dog. "+
		"A second sentence gives the splitter something to work with.</p>")
	s := newFakeIndexStore()
	s.file = domain.BookFile{ID: uuid.New(), Path: path, Format: domain.FormatEPUB}
	idx := &Index{Store: s, Logger: quietLogger()}
	progress := &progressLog{}

	if err := idx.Chunk(t.Context(), chunkJobFor(t, s.file.ID), progress.report); err != nil {
		t.Fatalf("Chunk: %v", err)
	}

	if len(s.savedChunks) == 0 {
		t.Fatal("no passages were stored")
	}
	if s.savedStrategy != rag.StrategyVersion {
		t.Errorf("strategy = %d, want %d", s.savedStrategy, rag.StrategyVersion)
	}
	var all strings.Builder
	for n, c := range s.savedChunks {
		if c.Seq != n {
			t.Errorf("chunk %d has seq %d, want passages in order", n, c.Seq)
		}
		all.WriteString(c.Content)
	}
	if !strings.Contains(all.String(), "quick brown fox") {
		t.Errorf("stored passages do not carry the text: %q", all.String())
	}

	// Embedding is nudged with the one shared target, so it deduplicates.
	if len(s.enqueued) != 1 {
		t.Fatalf("enqueued %d jobs, want one embed job", len(s.enqueued))
	}
	if got := s.enqueued[0]; got.Type != domain.JobEmbed || got.TargetID == nil || *got.TargetID != embedJobTarget {
		t.Errorf("enqueued %+v, want an embed job on the shared target", got)
	}
	if progress.last() != 1 {
		t.Errorf("last progress = %v, want 1", progress.last())
	}
}

func TestChunkStillSucceedsWhenEmbeddingCannotBeQueued(t *testing.T) {
	path := writeEPUB(t, "<p>Some text worth searching for later on.</p>")
	s := newFakeIndexStore()
	s.file = domain.BookFile{ID: uuid.New(), Path: path, Format: domain.FormatEPUB}
	s.enqueueErr = errors.New("queue unavailable")
	idx := &Index{Store: s, Logger: quietLogger()}

	if err := idx.Chunk(t.Context(), chunkJobFor(t, s.file.ID), noProgress); err != nil {
		t.Fatalf("Chunk = %v, want success: the next sweep queues embedding", err)
	}
	if len(s.savedChunks) == 0 {
		t.Error("the passages were not kept")
	}
}

func TestChunkEndsQuietlyWhenThereIsNothingToDo(t *testing.T) {
	tests := map[string]func(t *testing.T, s *fakeIndexStore) uuid.UUID{
		"the file is gone": func(_ *testing.T, s *fakeIndexStore) uuid.UUID {
			return uuid.New() // not the store's file
		},
		"a comic has no text": func(t *testing.T, s *fakeIndexStore) uuid.UUID {
			s.file = domain.BookFile{ID: uuid.New(), Path: writeEPUB(t, "<p>x</p>"), Format: domain.FormatCBZ}
			return s.file.ID
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			s := newFakeIndexStore()
			id := setup(t, s)
			idx := &Index{Store: s, Logger: quietLogger()}

			if err := idx.Chunk(t.Context(), chunkJobFor(t, id), noProgress); err != nil {
				t.Fatalf("Chunk = %v, want nil", err)
			}
			if len(s.savedChunks) != 0 || len(s.enqueued) != 0 {
				t.Errorf("stored %d passages and queued %d jobs, want nothing", len(s.savedChunks), len(s.enqueued))
			}
		})
	}
}

func TestChunkFailures(t *testing.T) {
	storeDown := errors.New("database unavailable")
	saveFailed := errors.New("disk full")

	tests := []struct {
		name    string
		setup   func(t *testing.T, s *fakeIndexStore) postgres.Job
		wantErr error // nil means "any error"
	}{
		{
			name: "payload is not JSON",
			setup: func(_ *testing.T, _ *fakeIndexStore) postgres.Job {
				return postgres.Job{ID: uuid.New(), Type: domain.JobChunk, Payload: []byte("{")}
			},
		},
		{
			name: "file id is not a UUID",
			setup: func(_ *testing.T, _ *fakeIndexStore) postgres.Job {
				return postgres.Job{ID: uuid.New(), Type: domain.JobChunk, Payload: []byte(`{"bookFileId":"nope"}`)}
			},
		},
		{
			name: "the store cannot load the file",
			setup: func(t *testing.T, s *fakeIndexStore) postgres.Job {
				s.fileErr = storeDown
				return chunkJobFor(t, uuid.New())
			},
			wantErr: storeDown,
		},
		{
			name: "the file has gone from disk",
			setup: func(t *testing.T, s *fakeIndexStore) postgres.Job {
				s.file = domain.BookFile{ID: uuid.New(), Path: filepath.Join(t.TempDir(), "gone.epub"), Format: domain.FormatEPUB}
				return chunkJobFor(t, s.file.ID)
			},
			wantErr: fs.ErrNotExist,
		},
		{
			name: "the passages cannot be saved",
			setup: func(t *testing.T, s *fakeIndexStore) postgres.Job {
				s.file = domain.BookFile{ID: uuid.New(), Path: writeEPUB(t, "<p>Text to keep.</p>"), Format: domain.FormatEPUB}
				s.saveChunksErr = saveFailed
				return chunkJobFor(t, s.file.ID)
			},
			wantErr: saveFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newFakeIndexStore()
			j := tc.setup(t, s)
			idx := &Index{Store: s, Logger: quietLogger()}

			err := idx.Chunk(t.Context(), j, noProgress)
			if err == nil {
				t.Fatal("Chunk succeeded, want an error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
			if errors.Is(err, ErrSkipped) {
				t.Error("a chunking failure was reported as a skip; chunking never needs the model")
			}
			if len(s.enqueued) != 0 {
				t.Error("embedding was queued for a file that was not chunked")
			}
		})
	}
}

func pendingChunks(texts ...string) []postgres.PendingChunk {
	out := make([]postgres.PendingChunk, len(texts))
	for i, text := range texts {
		out[i] = postgres.PendingChunk{ID: uuid.New(), Content: text}
	}
	return out
}

func TestEmbedStoresVectorsUnderTheModel(t *testing.T) {
	const dims = 8
	emb := newFakeEmbedder(t, dims)
	s := newFakeIndexStore()
	s.pending = pendingChunks("first passage", "second passage", "third passage")
	idx := &Index{Store: s, Client: emb.client("embed-model-a"), Dimensions: dims, EmbedBatch: 2, Logger: quietLogger()}
	progress := &progressLog{}

	if err := idx.Embed(t.Context(), postgres.Job{ID: uuid.New(), Type: domain.JobEmbed}, progress.report); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	got := s.vectorsFor("embed-model-a")
	if len(got) != 3 {
		t.Fatalf("embedded %d passages, want all 3", len(got))
	}
	for _, c := range s.pending {
		if len(got[c.ID]) != dims {
			t.Errorf("passage %q has a %d-wide vector, want %d", c.Content, len(got[c.ID]), dims)
		}
	}
	for _, d := range s.savedDims {
		if d != dims {
			t.Errorf("saved with dimensions %d, want %d", d, dims)
		}
	}
	for _, m := range s.askedModels {
		if m != "embed-model-a" {
			t.Errorf("backlog asked for model %q: vectors from different models must never mix", m)
		}
	}
	// The backlog is drained in batches of EmbedBatch.
	for _, l := range s.askedLimits {
		if l != 2 {
			t.Errorf("backlog asked with limit %d, want EmbedBatch=2", l)
		}
	}
	if progress.last() != 1 {
		t.Errorf("last progress = %v, want 1", progress.last())
	}

	// Run again: nothing is outstanding for this model, so the model is not
	// asked anything and nothing changes.
	before := emb.requestCount()
	if err := idx.Embed(t.Context(), postgres.Job{ID: uuid.New(), Type: domain.JobEmbed}, noProgress); err != nil {
		t.Fatalf("second Embed: %v", err)
	}
	if after := emb.requestCount(); after != before {
		t.Errorf("a rerun sent %d more requests, want none", after-before)
	}

	// A different model owes the whole backlog again.
	idx.Client = emb.client("embed-model-b")
	if err := idx.Embed(t.Context(), postgres.Job{ID: uuid.New(), Type: domain.JobEmbed}, noProgress); err != nil {
		t.Fatalf("Embed under a new model: %v", err)
	}
	if n := len(s.vectorsFor("embed-model-b")); n != 3 {
		t.Errorf("new model embedded %d passages, want 3", n)
	}
}

func TestEmbedBatchSize(t *testing.T) {
	tests := []struct {
		name      string
		payload   []byte
		batch     int
		wantLimit int
	}{
		{name: "default", wantLimit: DefaultEmbedBatch},
		{name: "configured", batch: 7, wantLimit: 7},
		{name: "the job's own limit wins", payload: []byte(`{"limit":3}`), batch: 7, wantLimit: 3},
		{name: "an unreadable payload is ignored", payload: []byte(`{`), batch: 5, wantLimit: 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			emb := newFakeEmbedder(t, 4)
			s := newFakeIndexStore()
			idx := &Index{Store: s, Client: emb.client("m"), Dimensions: 4, EmbedBatch: tc.batch, Logger: quietLogger()}

			if err := idx.Embed(t.Context(), postgres.Job{Payload: tc.payload}, noProgress); err != nil {
				t.Fatalf("Embed: %v", err)
			}
			if len(s.askedLimits) == 0 || s.askedLimits[0] != tc.wantLimit {
				t.Errorf("asked limits %v, want %d", s.askedLimits, tc.wantLimit)
			}
		})
	}
}

// A passage the model refuses is never stored and never asked about twice in
// one run; the rest of the batch is stored all the same.
func TestEmbedSetsAsideARefusedPassage(t *testing.T) {
	emb := newFakeEmbedder(t, 4)
	s := newFakeIndexStore()
	s.pending = pendingChunks("fine one", "this one is "+fakeEmbedderRefuse, "fine two")
	idx := &Index{Store: s, Client: emb.client("m"), Dimensions: 4, Logger: quietLogger()}

	if err := idx.Embed(t.Context(), postgres.Job{}, noProgress); err != nil {
		t.Fatalf("Embed = %v, want success: one bad passage must not fail the backlog", err)
	}

	got := s.vectorsFor("m")
	if len(got) != 2 {
		t.Fatalf("embedded %d passages, want the 2 the model accepted", len(got))
	}
	if _, ok := got[s.pending[1].ID]; ok {
		t.Error("the refused passage was stored")
	}
	refusedAsks := 0
	for _, in := range emb.inputs {
		if strings.Contains(in, fakeEmbedderRefuse) {
			refusedAsks++
		}
	}
	// Once in the batch, once on its own to find out which input it was.
	if refusedAsks != 2 {
		t.Errorf("the refused passage was sent %d times, want 2", refusedAsks)
	}
}

// When every passage in a batch is refused the run ends rather than looping
// over the same refused passages for ever.
func TestEmbedEndsWhenAWholeBatchIsRefused(t *testing.T) {
	emb := newFakeEmbedder(t, 4)
	s := newFakeIndexStore()
	s.pending = pendingChunks(fakeEmbedderRefuse + " a")
	idx := &Index{Store: s, Client: emb.client("m"), Dimensions: 4, EmbedBatch: 1, Logger: quietLogger()}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := idx.Embed(ctx, postgres.Job{}, noProgress); err != nil {
		t.Fatalf("Embed = %v, want a clean end", err)
	}
	if n := len(s.vectorsFor("m")); n != 0 {
		t.Errorf("stored %d vectors for refused passages", n)
	}
	if emb.requestCount() != 1 {
		t.Errorf("the model was asked %d times, want once", emb.requestCount())
	}
}

// An overloaded embedding server is a pause, not a list of bad passages.
// Taken for bad input, every passage was "refused" and the job succeeded
// having stored nothing.
func TestEmbedSkipsWhileTheServerIsBusy(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, `{"error":{"message":"busy"}}`, status)
			}))
			t.Cleanup(server.Close)

			s := newFakeIndexStore()
			s.pending = pendingChunks("one", "two", "three")
			idx := &Index{
				Store: s, Dimensions: 4, Logger: quietLogger(),
				Client: llm.New(config.ModelConfig{
					BaseURL: server.URL + "/v1", Model: "m", MaxConcurrency: 1, Timeout: 5 * time.Second,
				}),
			}
			if err := idx.Embed(t.Context(), postgres.Job{}, noProgress); !errors.Is(err, ErrSkipped) {
				t.Fatalf("Embed = %v, want a skip", err)
			}
		})
	}
}

func TestEmbedFailures(t *testing.T) {
	storeDown := errors.New("database unavailable")

	t.Run("no model configured is a skip", func(t *testing.T) {
		idx := &Index{Store: newFakeIndexStore(), Logger: quietLogger()}
		if err := idx.Embed(t.Context(), postgres.Job{}, noProgress); !errors.Is(err, ErrSkipped) {
			t.Fatalf("error = %v, want ErrSkipped (invariant 3: the model is optional)", err)
		}
	})

	t.Run("an unreachable model is a skip and keeps what was stored", func(t *testing.T) {
		emb := newFakeEmbedder(t, 4)
		client := emb.client("m")
		emb.server.Close()

		s := newFakeIndexStore()
		s.pending = pendingChunks("a passage")
		idx := &Index{Store: s, Client: client, Dimensions: 4, Logger: quietLogger()}

		if err := idx.Embed(t.Context(), postgres.Job{}, noProgress); !errors.Is(err, ErrSkipped) {
			t.Fatalf("error = %v, want ErrSkipped", err)
		}
	})

	t.Run("a model of the wrong width fails rather than waits", func(t *testing.T) {
		emb := newFakeEmbedder(t, 6)
		s := newFakeIndexStore()
		s.pending = pendingChunks("a passage")
		idx := &Index{Store: s, Client: emb.client("m"), Dimensions: 4, Logger: quietLogger()}

		err := idx.Embed(t.Context(), postgres.Job{}, noProgress)
		var mismatch *llm.ErrDimensionMismatch
		if !errors.As(err, &mismatch) {
			t.Fatalf("error = %v, want ErrDimensionMismatch", err)
		}
		if errors.Is(err, ErrSkipped) {
			t.Error("a dimension mismatch was a skip; waiting will not change the model's width")
		}
		if mismatch.Got != 6 || mismatch.Want != 4 {
			t.Errorf("mismatch = %+v, want got 6 want 4", mismatch)
		}
		if n := len(s.vectorsFor("m")); n != 0 {
			t.Errorf("stored %d vectors of the wrong width", n)
		}
	})

	t.Run("the backlog cannot be read", func(t *testing.T) {
		emb := newFakeEmbedder(t, 4)
		s := newFakeIndexStore()
		s.pendingErr = storeDown
		idx := &Index{Store: s, Client: emb.client("m"), Dimensions: 4, Logger: quietLogger()}

		if err := idx.Embed(t.Context(), postgres.Job{}, noProgress); !errors.Is(err, storeDown) {
			t.Fatalf("error = %v, want the store's error", err)
		}
	})

	t.Run("vectors cannot be saved", func(t *testing.T) {
		emb := newFakeEmbedder(t, 4)
		s := newFakeIndexStore()
		s.pending = pendingChunks("a passage")
		s.saveEmbedErr = storeDown
		idx := &Index{Store: s, Client: emb.client("m"), Dimensions: 4, Logger: quietLogger()}

		if err := idx.Embed(t.Context(), postgres.Job{}, noProgress); !errors.Is(err, storeDown) {
			t.Fatalf("error = %v, want the store's error", err)
		}
	})

	t.Run("cancellation stops between batches", func(t *testing.T) {
		emb := newFakeEmbedder(t, 4)
		s := newFakeIndexStore()
		s.pending = pendingChunks("one", "two", "three")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		s.saveEmbedHook = cancel // the first batch is stored, then shutdown arrives
		idx := &Index{Store: s, Client: emb.client("m"), Dimensions: 4, EmbedBatch: 1, Logger: quietLogger()}

		if err := idx.Embed(ctx, postgres.Job{}, noProgress); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if n := len(s.vectorsFor("m")); n != 1 {
			t.Errorf("stored %d vectors, want the one batch finished before cancellation", n)
		}
	})
}
