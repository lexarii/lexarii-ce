// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// pipelineStore is a Searcher whose answers the test sets and whose calls it
// records.
type pipelineStore struct {
	mu sync.Mutex

	// search answers a search; nil returns nothing.
	search func(q postgres.SearchQuery) ([]domain.RetrievedChunk, error)

	notes    []postgres.FoundNote
	notesErr error

	facts    postgres.LibraryFacts
	factsErr error

	matches    []postgres.SubjectMatch
	matchesErr error
	// shelf answers a Library query by tag, or by search when Tag is empty.
	shelf      map[string][]postgres.LibraryItem
	libraryErr error

	searches     []postgres.SearchQuery
	noteSearches []string
	libraryCalls []postgres.LibraryQuery
	factCalls    int
}

func (s *pipelineStore) Search(_ context.Context, q postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
	s.mu.Lock()
	s.searches = append(s.searches, q)
	s.mu.Unlock()
	if s.search == nil {
		return nil, nil
	}
	return s.search(q)
}

func (s *pipelineStore) WorksMatchingSubjects(context.Context, []string, int) ([]uuid.UUID, error) {
	return nil, nil
}

func (s *pipelineStore) Library(_ context.Context, q postgres.LibraryQuery) (postgres.LibraryPage, error) {
	s.mu.Lock()
	s.libraryCalls = append(s.libraryCalls, q)
	s.mu.Unlock()
	if s.libraryErr != nil {
		return postgres.LibraryPage{}, s.libraryErr
	}
	key := q.Tag
	if key == "" {
		key = "search:" + q.Search
	}
	items := s.shelf[key]
	if q.Limit > 0 && len(items) > q.Limit {
		items = items[:q.Limit]
	}
	return postgres.LibraryPage{Items: items}, nil
}

func (s *pipelineStore) SearchNotes(_ context.Context, _ uuid.UUID, text string, _ int) ([]postgres.FoundNote, error) {
	s.mu.Lock()
	s.noteSearches = append(s.noteSearches, text)
	s.mu.Unlock()
	return s.notes, s.notesErr
}

func (s *pipelineStore) MatchingSubjects(context.Context, []string) ([]postgres.SubjectMatch, error) {
	return s.matches, s.matchesErr
}

func (s *pipelineStore) LibraryFacts(context.Context, uuid.UUID) (postgres.LibraryFacts, error) {
	s.mu.Lock()
	s.factCalls++
	s.mu.Unlock()
	return s.facts, s.factsErr
}

// pipelinePassages builds n distinct passages from one book.
func pipelinePassages(prefix string, n int) []domain.RetrievedChunk {
	out := make([]domain.RetrievedChunk, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, domain.RetrievedChunk{
			Chunk: domain.Chunk{
				ID:           uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprintf("%s-%d", prefix, i))),
				ChapterTitle: fmt.Sprintf("Chapter %d", i+1),
				Content:      fmt.Sprintf("%s passage %d says something specific.", prefix, i+1),
			},
			WorkTitle:  prefix,
			FusedScore: float64(n - i),
		})
	}
	return out
}

// chatCall is one request the fake model received on /chat/completions.
type chatCall struct {
	kind     string // "route", "answer" or "metadata"
	stream   bool
	system   string
	user     string
	maxToken int
}

// fakeModel is an OpenAI-compatible server on loopback. It answers routing,
// answering and library-fact requests, embeddings and reranking, each as the
// test configures, and records what it was sent.
type fakeModel struct {
	srv *httptest.Server

	mu sync.Mutex

	// route is the JSON the router replies with; routeStatus, when set, is
	// returned instead.
	route       string
	routeStatus int

	// answer is the written answer, sent whole or in answerPieces when
	// streaming.
	answer       string
	answerPieces []string
	answerStatus int

	metadata       string
	metadataStatus int

	embedDims   int
	embedStatus int

	// rerank maps a document's text to its score.
	rerank func(doc string) float64

	calls       []chatCall
	embedInputs []string
	rerankDocs  [][]string
}

func newFakeModel(t *testing.T) *fakeModel {
	t.Helper()
	m := &fakeModel{route: routeJSON(domain.IntentFactualLookup, nil, nil, "")}
	m.srv = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(m.srv.Close)
	return m
}

// client builds a real llm.Client pointed at the fake.
func (m *fakeModel) client(model string) *llm.Client {
	return llm.New(config.ModelConfig{BaseURL: m.srv.URL, Model: model, Timeout: 10 * time.Second})
}

func routeJSON(intent domain.QueryIntent, subjects, searches []string, book string) string {
	if subjects == nil {
		subjects = []string{}
	}
	if searches == nil {
		searches = []string{}
	}
	raw, _ := json.Marshal(map[string]any{
		"intent": intent, "subjects": subjects, "searches": searches, "book": book, "confidence": 0.9,
	})
	return string(raw)
}

func (m *fakeModel) callsOf(kind string) []chatCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []chatCall
	for _, c := range m.calls {
		if c.kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func (m *fakeModel) serve(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch r.URL.Path {
	case "/chat/completions":
		m.serveChat(w, raw)
	case "/embeddings":
		m.serveEmbed(w, raw)
	case "/rerank":
		m.serveRerank(w, raw)
	default:
		http.NotFound(w, r)
	}
}

func (m *fakeModel) serveChat(w http.ResponseWriter, raw []byte) {
	var req struct {
		Messages       []llm.Message `json:"messages"`
		Stream         bool          `json:"stream"`
		MaxTokens      int           `json:"max_tokens"`
		ResponseFormat *struct {
			JSONSchema *struct {
				Name string `json:"name"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	call := chatCall{stream: req.Stream, maxToken: req.MaxTokens}
	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			call.system = msg.Content
		case "user":
			call.user = msg.Content
		}
	}
	switch {
	case req.ResponseFormat != nil && req.ResponseFormat.JSONSchema != nil &&
		req.ResponseFormat.JSONSchema.Name == "query_route":
		call.kind = "route"
	case strings.Contains(call.user, "The passages:"):
		call.kind = "answer"
	default:
		call.kind = "metadata"
	}

	m.mu.Lock()
	m.calls = append(m.calls, call)
	var content string
	status := 0
	switch call.kind {
	case "route":
		content, status = m.route, m.routeStatus
	case "answer":
		content, status = m.answer, m.answerStatus
	case "metadata":
		content, status = m.metadata, m.metadataStatus
	}
	pieces := m.answerPieces
	m.mu.Unlock()
	if status != 0 {
		http.Error(w, `{"error":{"message":"refused by the test"}}`, status)
		return
	}

	if call.stream {
		if len(pieces) == 0 {
			pieces = []string{content}
		}
		streamFakeAnswer(w, pieces)
		return
	}
	writeFakeJSON(w, map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 321, "completion_tokens": 45},
	})
}

func streamFakeAnswer(w http.ResponseWriter, pieces []string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, p := range pieces {
		frame, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": p}}},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
	}
	final, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}},
	})
	_, _ = fmt.Fprintf(w, "data: %s\n\n", final)
	usage, _ := json.Marshal(map[string]any{
		"choices": []any{},
		"usage":   map[string]any{"prompt_tokens": 654, "completion_tokens": 32},
	})
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", usage)
}

func (m *fakeModel) serveEmbed(w http.ResponseWriter, raw []byte) {
	var req struct {
		Input []string `json:"input"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.embedInputs = append(m.embedInputs, req.Input...)
	status, dims := m.embedStatus, m.embedDims
	m.mu.Unlock()
	if status != 0 {
		http.Error(w, "embedding refused by the test", status)
		return
	}
	data := make([]any, 0, len(req.Input))
	for i := range req.Input {
		vec := make([]float32, dims)
		vec[0] = 1
		data = append(data, map[string]any{"index": i, "embedding": vec})
	}
	writeFakeJSON(w, map[string]any{"data": data, "model": "fake-embed"})
}

func (m *fakeModel) serveRerank(w http.ResponseWriter, raw []byte) {
	var req struct {
		Documents []string `json:"documents"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.rerankDocs = append(m.rerankDocs, req.Documents)
	score := m.rerank
	m.mu.Unlock()
	results := make([]any, 0, len(req.Documents))
	for i, d := range req.Documents {
		results = append(results, map[string]any{"index": i, "relevance_score": score(d)})
	}
	writeFakeJSON(w, map[string]any{"results": results})
}

func writeFakeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
