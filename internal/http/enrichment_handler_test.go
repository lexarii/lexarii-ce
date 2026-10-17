// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
	"scm.poniatowski.dev/adam/lexarii/internal/worker"
)

var errEnrichDown = errors.New("enrichment store unavailable")

// enrichStoreFake answers what needs describing or indexing, and records what
// it was asked.
type enrichStoreFake struct {
	works       []uuid.UUID
	targets     []postgres.ChunkTarget
	suggestions []postgres.Suggestion
	stats       postgres.RetrievalStats

	worksErr       error
	targetsErr     error
	suggestionsErr error
	statsErr       error
	resolveErr     error

	worksLimit   int
	promptID     string
	chunkVersion int
	statsModel   string
	resolved     []uuid.UUID
	accepted     []bool
}

func (s *enrichStoreFake) WorksNeedingEnrichment(_ context.Context, promptID string, limit int) ([]uuid.UUID, error) {
	s.promptID, s.worksLimit = promptID, limit
	return s.works, s.worksErr
}
func (s *enrichStoreFake) PendingSuggestions(context.Context, int) ([]postgres.Suggestion, error) {
	return s.suggestions, s.suggestionsErr
}
func (s *enrichStoreFake) ResolveSuggestion(_ context.Context, id uuid.UUID, accept bool) error {
	s.resolved = append(s.resolved, id)
	s.accepted = append(s.accepted, accept)
	return s.resolveErr
}
func (s *enrichStoreFake) BookFilesNeedingChunks(_ context.Context, version, _ int) ([]postgres.ChunkTarget, error) {
	s.chunkVersion = version
	return s.targets, s.targetsErr
}
func (s *enrichStoreFake) RetrievalStats(_ context.Context, model string) (postgres.RetrievalStats, error) {
	s.statsModel = model
	return s.stats, s.statsErr
}

// enrichQueue is a job queue that deduplicates per target, as the real one
// does, so "queued" and "eligible" can differ.
type enrichQueue struct {
	mu      sync.Mutex
	seen    map[uuid.UUID]bool
	jobs    []postgres.EnqueueOptions
	failErr error
}

func (q *enrichQueue) Enqueue(_ context.Context, opts postgres.EnqueueOptions) (uuid.UUID, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failErr != nil {
		return uuid.Nil, false, q.failErr
	}
	q.jobs = append(q.jobs, opts)
	if q.seen == nil {
		q.seen = map[uuid.UUID]bool{}
	}
	created := !q.seen[*opts.TargetID]
	q.seen[*opts.TargetID] = true
	return uuid.New(), created, nil
}
func (q *enrichQueue) GetJobFor(context.Context, uuid.UUID, uuid.UUID) (postgres.Job, error) {
	return postgres.Job{}, store.ErrNotFound
}
func (q *enrichQueue) Cancel(context.Context, uuid.UUID) error { return nil }

func enrichServe(t *testing.T, h *EnrichmentHandler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	(&Router{Health: &HealthHandler{}, Enrichment: h}).Handler().ServeHTTP(rec, asUser(req))
	return rec
}

// Starting enrichment queues one job per work still to be described, under
// the current prompt, attributed to nobody in particular; a work already
// queued counts as eligible but not as newly queued.
func TestStartingEnrichment(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	s := &enrichStoreFake{works: []uuid.UUID{a, b}}
	q := &enrichQueue{seen: map[uuid.UUID]bool{b: true}}
	h := &EnrichmentHandler{Store: s, Queue: q, ModelConfigured: true, Accounts: anAdmin}

	rec := enrichServe(t, h, http.MethodPost, "/api/v1/enrich", `{"limit":5}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var out dto.EnrichmentStarted
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Queued != 1 || out.Eligible != 2 || out.PromptID != worker.EnrichPromptID() || !out.ModelConfigured {
		t.Errorf("result = %+v", out)
	}
	if s.worksLimit != 5 || s.promptID != worker.EnrichPromptID() {
		t.Errorf("asked for %d works under %q", s.worksLimit, s.promptID)
	}
	for _, j := range q.jobs {
		if j.Type != domain.JobEnrich || j.RequestedBy != uuid.Nil {
			t.Errorf("job = %+v, want an enrichment for nobody in particular", j)
		}
	}
}

// The batch is bounded whatever is asked for, and the body is optional.
func TestTheEnrichmentBatchIsBounded(t *testing.T) {
	for _, body := range []string{"", `{}`, `{"limit":-1}`, `{"limit":100000}`, `not json`} {
		t.Run(body, func(t *testing.T) {
			s := &enrichStoreFake{}
			rec := enrichServe(t, &EnrichmentHandler{Store: s, Queue: &enrichQueue{}, Accounts: anAdmin},
				http.MethodPost, "/api/v1/enrich", body)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d", rec.Code)
			}
			if s.worksLimit != maxEnrichBatch {
				t.Errorf("limit = %d, want %d", s.worksLimit, maxEnrichBatch)
			}
		})
	}
}

// With no model configured the request is still accepted, and says so, so
// the client can tell "nothing happened" from "switch your model on".
func TestEnrichmentWithNoModelSaysSo(t *testing.T) {
	rec := enrichServe(t, &EnrichmentHandler{Store: &enrichStoreFake{}, Queue: &enrichQueue{}, Accounts: anAdmin},
		http.MethodPost, "/api/v1/enrich", "")
	var out dto.EnrichmentStarted
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.ModelConfigured {
		t.Error("modelConfigured = true with no model")
	}
}

func TestStartingIndexing(t *testing.T) {
	f1, f2 := uuid.New(), uuid.New()
	s := &enrichStoreFake{
		targets: []postgres.ChunkTarget{{BookFileID: f1}, {BookFileID: f2}},
		stats:   postgres.RetrievalStats{Files: 10, ChunkedFiles: 4, Chunks: 900, EmbeddedChunks: 300},
	}
	q := &enrichQueue{}
	h := &EnrichmentHandler{Store: s, Queue: q, EmbedModel: "nomic-embed", Accounts: anAdmin}

	rec := enrichServe(t, h, http.MethodPost, "/api/v1/index", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var out dto.IndexingStarted
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	want := dto.IndexingStarted{Queued: 2, Eligible: 2, StrategyVersion: rag.StrategyVersion,
		Files: 10, IndexedFiles: 4, Chunks: 900, EmbeddedChunks: 300, ModelConfigured: true}
	if out != want {
		t.Errorf("result = %+v, want %+v", out, want)
	}
	if s.chunkVersion != rag.StrategyVersion || s.statsModel != "nomic-embed" {
		t.Errorf("asked under strategy %d for model %q", s.chunkVersion, s.statsModel)
	}
	if len(q.jobs) != 2 || q.jobs[0].Type != domain.JobChunk || *q.jobs[0].TargetID != f1 {
		t.Errorf("jobs = %+v, want a chunk job per file", q.jobs)
	}

	// Chunking needs no model; the reply says only that embedding has none.
	rec = enrichServe(t, &EnrichmentHandler{Store: &enrichStoreFake{}, Queue: &enrichQueue{}, Accounts: anAdmin},
		http.MethodPost, "/api/v1/index", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusAccepted || out.ModelConfigured {
		t.Errorf("no embedding model: status %d, %+v", rec.Code, out)
	}
}

func TestEnrichmentStoreAndQueueFailures(t *testing.T) {
	work, file := uuid.New(), uuid.New()
	for _, tc := range []struct {
		name   string
		target string
		store  *enrichStoreFake
		queue  *enrichQueue
	}{
		{"finding works", "/api/v1/enrich", &enrichStoreFake{worksErr: errEnrichDown}, &enrichQueue{}},
		{"queueing a work", "/api/v1/enrich", &enrichStoreFake{works: []uuid.UUID{work}}, &enrichQueue{failErr: errEnrichDown}},
		{"finding files", "/api/v1/index", &enrichStoreFake{targetsErr: errEnrichDown}, &enrichQueue{}},
		{"queueing a file", "/api/v1/index", &enrichStoreFake{targets: []postgres.ChunkTarget{{BookFileID: file}}},
			&enrichQueue{failErr: errEnrichDown}},
		{"counting", "/api/v1/index", &enrichStoreFake{statsErr: errEnrichDown}, &enrichQueue{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := enrichServe(t, &EnrichmentHandler{Store: tc.store, Queue: tc.queue, Accounts: anAdmin},
				http.MethodPost, tc.target, "")
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
		})
	}
}

// Suggestions carry the whole library's titles, so they are an
// administrator's to read - refused to a reader before anything is read.
func TestSuggestionsAreForAdministrators(t *testing.T) {
	s := &enrichStoreFake{suggestions: []postgres.Suggestion{{ID: uuid.New(), WorkTitle: "Somebody's book"}}}
	rec := enrichServe(t, &EnrichmentHandler{Store: s, Queue: &enrichQueue{}, Accounts: aReader},
		http.MethodGet, "/api/v1/suggestions", "")
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "Somebody") {
		t.Fatalf("status = %d body = %s, want 403 and nothing of the library", rec.Code, rec.Body)
	}

	anon := httptest.NewRecorder()
	(&EnrichmentHandler{Store: s, Accounts: anAdmin}).Suggestions(anon, httptest.NewRequest(http.MethodGet, "/x", nil))
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("signed out: status = %d, want 401", anon.Code)
	}
}

func TestListingSuggestions(t *testing.T) {
	conf := 0.82
	id, work := uuid.New(), uuid.New()
	when := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	s := &enrichStoreFake{suggestions: []postgres.Suggestion{{
		ID: id, WorkID: work, WorkTitle: "Algebra", Field: "title", Value: []byte(`"Algebra Essentials"`),
		Current: "Essentials", Model: "gemma", PromptID: "classify/7", Confidence: &conf, GeneratedAt: when,
	}}}
	rec := enrichServe(t, &EnrichmentHandler{Store: s, Queue: &enrichQueue{}, Accounts: anAdmin},
		http.MethodGet, "/api/v1/suggestions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out dto.SuggestionList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items = %+v", out.Items)
	}
	got := out.Items[0]
	if got.ID != id.String() || got.WorkID != work.String() || string(got.Value) != `"Algebra Essentials"` ||
		got.Current != "Essentials" || got.Model != "gemma" || got.Confidence == nil || *got.Confidence != conf {
		t.Errorf("suggestion = %+v, want the proposal beside what it would replace, with its provenance", got)
	}

	failing := enrichServe(t, &EnrichmentHandler{Store: &enrichStoreFake{suggestionsErr: errEnrichDown}, Accounts: anAdmin},
		http.MethodGet, "/api/v1/suggestions", "")
	if failing.Code != http.StatusInternalServerError {
		t.Errorf("store failure: status = %d, want 500", failing.Code)
	}
}

func TestResolvingASuggestion(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		path   string
		accept bool
	}{
		{"/api/v1/suggestions/" + id.String() + "/accept", true},
		{"/api/v1/suggestions/" + id.String() + "/reject", false},
	} {
		s := &enrichStoreFake{}
		rec := enrichServe(t, &EnrichmentHandler{Store: s, Queue: &enrichQueue{}, Accounts: anAdmin}, http.MethodPost, tc.path, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: status = %d", tc.path, rec.Code)
		}
		if len(s.resolved) != 1 || s.resolved[0] != id || s.accepted[0] != tc.accept {
			t.Errorf("%s: resolved %v accept %v", tc.path, s.resolved, s.accepted)
		}
	}

	for _, tc := range []struct {
		name   string
		path   string
		err    error
		status int
	}{
		{"a bad id", "/api/v1/suggestions/nope/accept", nil, http.StatusBadRequest},
		{"already decided, or never there", "/api/v1/suggestions/" + id.String() + "/accept", store.ErrNotFound, http.StatusNotFound},
		{"the store fails", "/api/v1/suggestions/" + id.String() + "/reject", errEnrichDown, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := enrichServe(t, &EnrichmentHandler{Store: &enrichStoreFake{resolveErr: tc.err}, Accounts: anAdmin},
				http.MethodPost, tc.path, "")
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
		})
	}
}
