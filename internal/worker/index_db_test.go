// SPDX-License-Identifier: AGPL-3.0-or-later

package worker_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

// libraryVectorWidth is what the embeddings column is fixed at.
const libraryVectorWidth = 768

// embeddingServerForDB answers /v1/embeddings with library-width vectors and
// counts the requests it was sent.
func embeddingServerForDB(t *testing.T, model string) (*llm.Client, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			v := make([]float32, libraryVectorWidth)
			v[i%libraryVectorWidth] = 1
			data[i] = map[string]any{"index": i, "embedding": v}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)

	return llm.New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: model, MaxConcurrency: 1, Timeout: 5 * time.Second,
	}), &requests
}

// Chunking then embedding against the real store: every passage gets one
// vector recorded under the model that made it, and running the embed job
// again neither asks the model anything nor writes a second vector.
func TestEmbedAgainstTheStoreRecordsTheModelAndIsIdempotent(t *testing.T) {
	p := testPool(t)
	root := t.TempDir()
	writeEPUB(t, filepath.Join(root, "one.epub"), "One", "An Author")

	ing := &worker.Ingest{Store: p, LibraryDir: root, Logger: quietTestLogger(), Indexing: p}
	if _, _, err := p.Enqueue(t.Context(), postgres.EnqueueOptions{Type: domain.JobScan}); err != nil {
		t.Fatalf("enqueue scan: %v", err)
	}
	drainWith(t, p, indexingHandlers(p, ing))

	chunks := countRows(t, p, `SELECT count(*) FROM chunks`)
	if chunks == 0 {
		t.Fatal("nothing was chunked")
	}
	if n := countRows(t, p, `SELECT count(*) FROM jobs WHERE type = 'embed'`); n != 1 {
		t.Errorf("embed jobs = %d, want exactly one however many files were chunked", n)
	}

	const model = "fake-embed-768"
	client, requests := embeddingServerForDB(t, model)
	index := &worker.Index{Store: p, Client: client, Dimensions: libraryVectorWidth, Logger: quietTestLogger()}

	if err := index.Embed(t.Context(), postgres.Job{}, func(float32, string) {}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if n := countRows(t, p, `SELECT count(*) FROM embeddings WHERE model_id = $1 AND dimensions = $2`,
		model, libraryVectorWidth); n != chunks {
		t.Fatalf("vectors under %s = %d, want one per passage (%d)", model, n, chunks)
	}
	if n := countRows(t, p, `SELECT count(*) FROM embeddings WHERE model_id <> $1`, model); n != 0 {
		t.Errorf("%d vectors were recorded under another model", n)
	}

	asked := requests.Load()
	if err := index.Embed(t.Context(), postgres.Job{}, func(float32, string) {}); err != nil {
		t.Fatalf("second Embed: %v", err)
	}
	if again := requests.Load(); again != asked {
		t.Errorf("the rerun sent %d more requests to the model, want none", again-asked)
	}
	if n := countRows(t, p, `SELECT count(*) FROM embeddings`); n != chunks {
		t.Errorf("vectors after the rerun = %d, want still %d", n, chunks)
	}
}
