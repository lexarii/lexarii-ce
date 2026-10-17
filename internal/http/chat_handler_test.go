// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

var errChatStoreDown = errors.New("chat store unavailable")

// chatThreadStore keeps conversations per user, the way the real store does:
// asking for somebody else's is the same not-found as asking for nobody's.
type chatThreadStore struct {
	mu        sync.Mutex
	threads   map[uuid.UUID]postgres.Conversation
	appended  []postgres.Message
	deleted   []uuid.UUID
	listLimit int

	listErr   error
	createErr error
	getErr    error
	deleteErr error
	// appendErr fails the append numbered failAppend (1-based).
	appendErr  error
	failAppend int
}

func newChatThreadStore() *chatThreadStore {
	return &chatThreadStore{threads: map[uuid.UUID]postgres.Conversation{}}
}

func (s *chatThreadStore) add(c postgres.Conversation) postgres.Conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	s.threads[c.ID] = c
	return c
}

func (s *chatThreadStore) CreateConversation(_ context.Context, userID uuid.UUID, title string, bookFileID *uuid.UUID) (postgres.Conversation, error) {
	if s.createErr != nil {
		return postgres.Conversation{}, s.createErr
	}
	return s.add(postgres.Conversation{UserID: userID, Title: title, BookFileID: bookFileID,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}), nil
}

func (s *chatThreadStore) Conversation(_ context.Context, userID, id uuid.UUID) (postgres.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getErr != nil {
		return postgres.Conversation{}, s.getErr
	}
	c, ok := s.threads[id]
	if !ok || c.UserID != userID {
		return postgres.Conversation{}, fmt.Errorf("conversation %s: %w", id, store.ErrNotFound)
	}
	return c, nil
}

func (s *chatThreadStore) Conversations(_ context.Context, userID uuid.UUID, limit int) ([]postgres.Conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listLimit = limit
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []postgres.Conversation
	for _, c := range s.threads {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *chatThreadStore) AppendMessage(_ context.Context, conversationID uuid.UUID, m postgres.Message) (postgres.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAppend == len(s.appended)+1 {
		return postgres.Message{}, s.appendErr
	}
	m.ID = uuid.New()
	m.Seq = len(s.appended)
	s.appended = append(s.appended, m)
	return m, nil
}

func (s *chatThreadStore) DeleteConversation(_ context.Context, userID, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	c, ok := s.threads[id]
	if !ok || c.UserID != userID {
		return store.ErrNotFound
	}
	delete(s.threads, id)
	s.deleted = append(s.deleted, id)
	return nil
}

func (s *chatThreadStore) SetConversationTitle(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}

// chatSearchFake is retrieval without a database: it returns the passages it
// was given and remembers what it was asked.
type chatSearchFake struct {
	mu       sync.Mutex
	passages []domain.RetrievedChunk
	queries  []postgres.SearchQuery
	err      error
}

func (f *chatSearchFake) Search(_ context.Context, q postgres.SearchQuery) ([]domain.RetrievedChunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries = append(f.queries, q)
	return f.passages, f.err
}
func (f *chatSearchFake) WorksMatchingSubjects(context.Context, []string, int) ([]uuid.UUID, error) {
	return nil, nil
}
func (f *chatSearchFake) Library(context.Context, postgres.LibraryQuery) (postgres.LibraryPage, error) {
	return postgres.LibraryPage{}, nil
}
func (f *chatSearchFake) SearchNotes(context.Context, uuid.UUID, string, int) ([]postgres.FoundNote, error) {
	return nil, nil
}
func (f *chatSearchFake) MatchingSubjects(context.Context, []string) ([]postgres.SubjectMatch, error) {
	return nil, nil
}
func (f *chatSearchFake) LibraryFacts(context.Context, uuid.UUID) (postgres.LibraryFacts, error) {
	return postgres.LibraryFacts{}, nil
}

// chatPassages are two passages from two books, so a citation can be told
// apart from a passage that was merely shown.
func chatPassages() []domain.RetrievedChunk {
	return []domain.RetrievedChunk{
		{
			Chunk: domain.Chunk{ID: uuid.New(), BookFileID: uuid.New(), ChapterTitle: "Eigenvalues",
				Content: "An eigenvector is not rotated by the map.",
				Range:   domain.Range{Start: domain.NewPDFLocator(12, 0), End: domain.NewPDFLocator(12, 40)}},
			WorkID: uuid.New(), WorkTitle: "Linear Algebra Done Right", FusedScore: 0.9,
		},
		{
			Chunk: domain.Chunk{ID: uuid.New(), BookFileID: uuid.New(), ChapterTitle: "Spectra",
				Content: "The spectrum is the set of eigenvalues.",
				Range:   domain.Range{Start: domain.NewEPUBLocator(3, "/4/2", 0), End: domain.NewEPUBLocator(3, "/4/2", 39)}},
			WorkID: uuid.New(), WorkTitle: "Matrix Analysis", FusedScore: 0.5,
		},
	}
}

// chatModel is an OpenAI-compatible model server on loopback. The router's
// structured request is refused, which the answerer must survive by falling
// back to searching; the answer itself is streamed in the given pieces, or
// written whole for a request that does not stream.
type chatModel struct {
	deltas []string
	// failAnswer refuses the answer itself, as a model that is down does.
	failAnswer bool
	// breakMidStream sends an error event after the first piece.
	breakMidStream bool

	mu       sync.Mutex
	answered int
}

func (m *chatModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Stream         bool            `json:"stream"`
		ResponseFormat json.RawMessage `json:"response_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(body.ResponseFormat) > 0 {
		http.Error(w, `{"error":{"message":"no router here"}}`, http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	m.answered++
	m.mu.Unlock()
	if m.failAnswer {
		http.Error(w, `{"error":{"message":"model unavailable"}}`, http.StatusBadRequest)
		return
	}
	if !body.Stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":7}}`,
			strings.Join(m.deltas, ""))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for i, d := range m.deltas {
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", d)
		if m.breakMidStream && i == 0 {
			_, _ = fmt.Fprint(w, "data: {\"error\":{\"message\":\"the model fell over\"}}\n\n")
			return
		}
	}
	_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7}}\n\n")
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func chatAnswererWithModel(t *testing.T, search *chatSearchFake, model *chatModel) *rag.Answerer {
	t.Helper()
	srv := httptest.NewServer(model)
	t.Cleanup(srv.Close)
	return &rag.Answerer{
		Store: search,
		Chat:  llm.New(config.ModelConfig{BaseURL: srv.URL, Model: "test-model", Timeout: 10 * time.Second}),
	}
}

func chatRequest(method, target, body string, user uuid.UUID, conversation string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if conversation != "" {
		req.SetPathValue("id", conversation)
	}
	if user == uuid.Nil {
		return req
	}
	return asUserID(req, user)
}

// The whole streaming conversation, as the reader sees it: the thread first,
// the passages before the first word, the words as they come, then the stored
// message with what it cited.
func TestAStreamedAnswerIsFramedInOrder(t *testing.T) {
	threads := newChatThreadStore()
	search := &chatSearchFake{passages: chatPassages()}
	model := &chatModel{deltas: []string{"An eigenvector ", "keeps its direction [1]."}}
	h := &ChatHandler{Store: threads, Answerer: chatAnswererWithModel(t, search, model)}
	user := uuid.New()

	rec := httptest.NewRecorder()
	h.Stream(rec, chatRequest(http.MethodPost, "/api/v1/conversations/stream",
		`{"question":"what is an eigenvector?"}`, user, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	for header, want := range map[string]string{
		"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if !strings.HasSuffix(rec.Body.String(), "\n\n") {
		t.Error("the stream does not end on an event boundary")
	}

	got := events(t, rec.Body.String())
	var names []string
	for _, e := range got {
		names = append(names, e[0])
	}
	want := []string{"conversation", "passages", "delta", "delta", "done"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", names, want)
	}

	var passages []dto.Source
	if err := json.Unmarshal([]byte(got[1][1]), &passages); err != nil {
		t.Fatalf("passages event: %v", err)
	}
	if len(passages) != 2 || passages[0].Title != "Linear Algebra Done Right" || passages[0].Locator != "pdf:12:0" {
		t.Errorf("passages = %+v, want both, with locators", passages)
	}
	for _, p := range passages {
		if p.Cited {
			t.Error("a passage was marked cited before the model had written anything")
		}
	}

	var text strings.Builder
	for _, e := range got[2:4] {
		var d dto.AnswerDelta
		if err := json.Unmarshal([]byte(e[1]), &d); err != nil {
			t.Fatalf("delta: %v", err)
		}
		text.WriteString(d.Text)
	}
	if text.String() != "An eigenvector keeps its direction [1]." {
		t.Errorf("deltas = %q", text.String())
	}

	var done dto.Answer
	if err := json.Unmarshal([]byte(got[4][1]), &done); err != nil {
		t.Fatalf("done: %v", err)
	}
	if done.Answer != "An eigenvector keeps its direction [1]." || done.MessageID == "" {
		t.Errorf("done = %+v, want the whole answer and the stored message", done)
	}

	// Stored: the question, then the answer citing only what it cited.
	if len(threads.appended) != 2 {
		t.Fatalf("appended %d messages, want 2", len(threads.appended))
	}
	answer := threads.appended[1]
	if answer.Role != "assistant" || answer.ModelID != "test-model" || answer.Unsupported {
		t.Errorf("stored answer = %+v", answer)
	}
	if len(answer.Citations) != 1 || answer.Citations[0].Rank != 1 ||
		answer.Citations[0].ChunkID != search.passages[0].Chunk.ID {
		t.Errorf("citations = %+v, want exactly the first passage, as [1]", answer.Citations)
	}
	if done.ConversationID == "" || threads.threads[uuid.MustParse(done.ConversationID)].UserID != user {
		t.Errorf("conversation %q is not the asker's", done.ConversationID)
	}
}

// Invariant 3: with no model configured the conversation still works - the
// question is stored, the passages are found and shown, and the stream ends.
func TestAStreamWithNoModelIsSearchWithCitations(t *testing.T) {
	threads := newChatThreadStore()
	search := &chatSearchFake{passages: chatPassages()}
	h := &ChatHandler{Store: threads, Answerer: &rag.Answerer{Store: search}}

	rec := httptest.NewRecorder()
	h.Stream(rec, chatRequest(http.MethodPost, "/api/v1/conversations/stream",
		`{"question":"what is an eigenvector?"}`, uuid.New(), ""))

	got := events(t, rec.Body.String())
	var names []string
	for _, e := range got {
		names = append(names, e[0])
	}
	if strings.Join(names, ",") != "conversation,passages,done" {
		t.Fatalf("events = %v, want conversation, passages, done", names)
	}
	var done dto.Answer
	if err := json.Unmarshal([]byte(got[2][1]), &done); err != nil {
		t.Fatalf("done: %v", err)
	}
	if done.Answer != "" || len(done.Sources) != 2 {
		t.Errorf("done = %+v, want no written answer and both sources", done)
	}
	// Nothing was cited, because nothing was written - and an answer with
	// passages is not unsupported.
	if stored := threads.appended[1]; len(stored.Citations) != 0 || stored.Unsupported {
		t.Errorf("stored = %+v", stored)
	}
}

// A model that is configured but failing ends the stream with an error event,
// after keeping the question.
func TestAStreamWhoseModelFailsEndsWithAnError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model *chatModel
		want  []string
	}{
		{"before the first word", &chatModel{failAnswer: true}, []string{"conversation", "passages", "error"}},
		{"partway through", &chatModel{deltas: []string{"An eigen", "vector"}, breakMidStream: true},
			[]string{"conversation", "passages", "delta", "error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			threads := newChatThreadStore()
			h := &ChatHandler{Store: threads,
				Answerer: chatAnswererWithModel(t, &chatSearchFake{passages: chatPassages()}, tc.model)}

			rec := httptest.NewRecorder()
			h.Stream(rec, chatRequest(http.MethodPost, "/x", `{"question":"what is an eigenvector?"}`, uuid.New(), ""))

			got := events(t, rec.Body.String())
			var names []string
			for _, e := range got {
				names = append(names, e[0])
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("events = %v, want %v", names, tc.want)
			}
			var e map[string]string
			if err := json.Unmarshal([]byte(got[len(got)-1][1]), &e); err != nil || e["message"] == "" {
				t.Errorf("error event = %s, want a message", got[len(got)-1][1])
			}
			if len(threads.appended) != 1 || threads.appended[0].Role != "user" {
				t.Errorf("appended = %+v, want the question kept and no answer", threads.appended)
			}
		})
	}
}

// The answer is written, but the thread cannot hold it: the stream says so
// rather than ending as though it had been saved.
func TestAStreamWhoseAnswerCannotBeStoredSaysSo(t *testing.T) {
	threads := newChatThreadStore()
	threads.failAppend, threads.appendErr = 2, errChatStoreDown
	h := &ChatHandler{Store: threads, Answerer: &rag.Answerer{Store: &chatSearchFake{passages: chatPassages()}}}

	rec := httptest.NewRecorder()
	h.Stream(rec, chatRequest(http.MethodPost, "/x", `{"question":"what is an eigenvector?"}`, uuid.New(), ""))

	got := events(t, rec.Body.String())
	if last := got[len(got)-1]; last[0] != "error" {
		t.Fatalf("last event = %v, want error", last)
	}
	for _, e := range got {
		if e[0] == "done" {
			t.Fatal("a done event was sent for an answer that was not stored")
		}
	}
}

// A follow-up goes to the thread it names, is searched within the book the
// thread is pinned to, and is answered with the thread's history.
func TestAFollowUpStaysInItsThread(t *testing.T) {
	threads := newChatThreadStore()
	user, pinned := uuid.New(), uuid.New()
	thread := threads.add(postgres.Conversation{UserID: user, Title: "Eigenvectors", BookFileID: &pinned,
		Messages: []postgres.Message{
			{Role: "user", Text: "what is an eigenvector?"},
			{Role: "assistant", Text: "One the map does not rotate [1]."},
		}})
	search := &chatSearchFake{passages: chatPassages()}
	h := &ChatHandler{Store: threads, Answerer: &rag.Answerer{Store: search}}

	for _, tc := range []struct {
		name string
		call http.HandlerFunc
	}{
		{"post", h.Post},
		{"stream", h.Stream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(search.queries)
			rec := httptest.NewRecorder()
			// The body names a different book; the thread's pin wins.
			tc.call(rec, chatRequest(http.MethodPost, "/x",
				`{"question":"and an eigenvalue?","bookFileId":"`+uuid.NewString()+`"}`, user, thread.ID.String()))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			if len(search.queries) != before+1 {
				t.Fatalf("searched %d times, want once", len(search.queries)-before)
			}
			q := search.queries[len(search.queries)-1]
			if q.BookFileID == nil || *q.BookFileID != pinned {
				t.Errorf("searched book %v, want the thread's pinned book %v", q.BookFileID, pinned)
			}
			if q.ViewerID != user {
				t.Errorf("searched as %v, want the asker", q.ViewerID)
			}
			if len(threads.threads) != 1 {
				t.Errorf("%d threads, want the follow-up kept in the existing one", len(threads.threads))
			}
		})
	}
}

// Another person's conversation is not there, whatever is asked of it. Never
// 403: that would confirm it exists.
func TestAnotherPersonsConversationIsNotFound(t *testing.T) {
	threads := newChatThreadStore()
	owner, stranger := uuid.New(), uuid.New()
	thread := threads.add(postgres.Conversation{UserID: owner, Title: "private"})
	h := &ChatHandler{Store: threads, Answerer: &rag.Answerer{Store: &chatSearchFake{}}}

	for _, tc := range []struct {
		name   string
		call   http.HandlerFunc
		method string
		body   string
	}{
		{"read", h.Get, http.MethodGet, ""},
		{"delete", h.Delete, http.MethodDelete, ""},
		{"ask", h.Post, http.MethodPost, `{"question":"what did they ask?"}`},
		{"stream", h.Stream, http.MethodPost, `{"question":"what did they ask?"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.call(rec, chatRequest(tc.method, "/x", tc.body, stranger, thread.ID.String()))
			if rec.Code != http.StatusNotFound || authErrorCode(t, rec) != string(CodeNotFound) {
				t.Fatalf("status = %d body = %s, want 404 not_found", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "private") {
				t.Error("the refusal leaks the thread's title")
			}
		})
	}
	if _, still := threads.threads[thread.ID]; !still || len(threads.appended) != 0 {
		t.Error("a stranger's request changed somebody else's thread")
	}
}

func TestChatRefusals(t *testing.T) {
	h := &ChatHandler{Store: newChatThreadStore(), Answerer: &rag.Answerer{Store: &chatSearchFake{}}}

	for _, tc := range []struct {
		name   string
		call   http.HandlerFunc
		method string
		body   string
		id     string
		user   bool
		status int
	}{
		{"list signed out", h.List, http.MethodGet, "", "", false, http.StatusUnauthorized},
		{"read signed out", h.Get, http.MethodGet, "", uuid.NewString(), false, http.StatusUnauthorized},
		{"delete signed out", h.Delete, http.MethodDelete, "", uuid.NewString(), false, http.StatusUnauthorized},
		{"ask signed out", h.Post, http.MethodPost, `{"question":"x"}`, "", false, http.StatusUnauthorized},
		{"read a bad id", h.Get, http.MethodGet, "", "not-a-uuid", true, http.StatusBadRequest},
		{"delete a bad id", h.Delete, http.MethodDelete, "", "not-a-uuid", true, http.StatusBadRequest},
		{"ask into a bad id", h.Post, http.MethodPost, `{"question":"x"}`, "not-a-uuid", true, http.StatusBadRequest},
		{"stream into a bad id", h.Stream, http.MethodPost, `{"question":"x"}`, "not-a-uuid", true, http.StatusBadRequest},
		{"ask malformed JSON", h.Post, http.MethodPost, `{"question":`, "", true, http.StatusBadRequest},
		{"ask nothing", h.Post, http.MethodPost, `{"question":"  "}`, "", true, http.StatusBadRequest},
		{"ask about a bad book id", h.Post, http.MethodPost, `{"question":"x","bookFileId":"nope"}`, "", true, http.StatusBadRequest},
		{"read a thread that does not exist", h.Get, http.MethodGet, "", uuid.NewString(), true, http.StatusNotFound},
		{"delete a thread that does not exist", h.Delete, http.MethodDelete, "", uuid.NewString(), true, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			user := uuid.Nil
			if tc.user {
				user = uuid.New()
			}
			rec := httptest.NewRecorder()
			tc.call(rec, chatRequest(tc.method, "/x", tc.body, user, tc.id))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
				t.Error("a refused request opened a stream")
			}
		})
	}
}

// Failures of the thread store before anything is streamed are ordinary
// errors with a status code, and say nothing about the cause.
func TestChatStoreFailuresAreInternalErrors(t *testing.T) {
	owner := uuid.New()
	for _, tc := range []struct {
		name   string
		setup  func(*chatThreadStore, *ChatHandler) (http.HandlerFunc, string, string)
		method string
	}{
		{"list", func(s *chatThreadStore, h *ChatHandler) (http.HandlerFunc, string, string) {
			s.listErr = errChatStoreDown
			return h.List, "", ""
		}, http.MethodGet},
		{"read", func(s *chatThreadStore, h *ChatHandler) (http.HandlerFunc, string, string) {
			s.getErr = errChatStoreDown
			return h.Get, "", uuid.NewString()
		}, http.MethodGet},
		{"delete", func(s *chatThreadStore, h *ChatHandler) (http.HandlerFunc, string, string) {
			s.deleteErr = errChatStoreDown
			return h.Delete, "", uuid.NewString()
		}, http.MethodDelete},
		{"ask: finding the thread", func(s *chatThreadStore, h *ChatHandler) (http.HandlerFunc, string, string) {
			s.getErr = errChatStoreDown
			return h.Post, `{"question":"x"}`, uuid.NewString()
		}, http.MethodPost},
		{"ask: starting a thread", func(s *chatThreadStore, h *ChatHandler) (http.HandlerFunc, string, string) {
			s.createErr = errChatStoreDown
			return h.Post, `{"question":"x"}`, ""
		}, http.MethodPost},
		{"ask: storing the question", func(s *chatThreadStore, h *ChatHandler) (http.HandlerFunc, string, string) {
			s.failAppend, s.appendErr = 1, errChatStoreDown
			return h.Post, `{"question":"x"}`, ""
		}, http.MethodPost},
		{"ask: storing the answer", func(s *chatThreadStore, h *ChatHandler) (http.HandlerFunc, string, string) {
			s.failAppend, s.appendErr = 2, errChatStoreDown
			return h.Post, `{"question":"x"}`, ""
		}, http.MethodPost},
		{"stream: starting a thread", func(s *chatThreadStore, h *ChatHandler) (http.HandlerFunc, string, string) {
			s.createErr = errChatStoreDown
			return h.Stream, `{"question":"x"}`, ""
		}, http.MethodPost},
		{"stream: storing the question", func(s *chatThreadStore, h *ChatHandler) (http.HandlerFunc, string, string) {
			s.failAppend, s.appendErr = 1, errChatStoreDown
			return h.Stream, `{"question":"x"}`, ""
		}, http.MethodPost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			threads := newChatThreadStore()
			h := &ChatHandler{Store: threads, Answerer: &rag.Answerer{Store: &chatSearchFake{passages: chatPassages()}}}
			call, body, id := tc.setup(threads, h)
			rec := httptest.NewRecorder()
			call(rec, chatRequest(tc.method, "/x", body, owner, id))
			if rec.Code != http.StatusInternalServerError || authErrorCode(t, rec) != string(CodeInternal) {
				t.Fatalf("status = %d body = %s, want 500 internal_error", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), errChatStoreDown.Error()) {
				t.Error("the internal error leaked to the client")
			}
		})
	}
}

// Asking without streaming: the answer, the stored message, and the thread
// named after the question.
func TestAskingInANewThread(t *testing.T) {
	threads := newChatThreadStore()
	search := &chatSearchFake{passages: chatPassages()}
	model := &chatModel{deltas: []string{"See [2]."}}
	h := &ChatHandler{Store: threads, Answerer: chatAnswererWithModel(t, search, model)}
	user, pinnedFile := uuid.New(), uuid.New()

	rec := httptest.NewRecorder()
	h.Post(rec, chatRequest(http.MethodPost, "/api/v1/conversations",
		`{"question":"  what is   the spectrum?  ","bookFileId":"`+pinnedFile.String()+`"}`, user, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	var out dto.Answer
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Answer != "See [2]." || out.ConversationID == "" || out.MessageID == "" {
		t.Errorf("answer = %+v", out)
	}
	if len(out.Sources) != 2 || out.Sources[0].Cited || !out.Sources[1].Cited {
		t.Errorf("sources = %+v, want only [2] marked cited", out.Sources)
	}
	thread := threads.threads[uuid.MustParse(out.ConversationID)]
	if thread.Title != "what is the spectrum?" || thread.BookFileID == nil || *thread.BookFileID != pinnedFile {
		t.Errorf("thread = %+v, want it titled by the question and pinned to the book", thread)
	}
	if stored := threads.appended[1]; len(stored.Citations) != 1 || stored.Citations[0].Rank != 2 ||
		stored.Citations[0].Position != 0 {
		t.Errorf("citations = %+v, want [2] stored at position 0", stored.Citations)
	}
	if model.answered != 1 {
		t.Errorf("the model was asked to answer %d times, want once", model.answered)
	}
}

// A model that is configured and failing loses the answer, not the question.
func TestAskingWhenTheModelFailsKeepsTheQuestion(t *testing.T) {
	threads := newChatThreadStore()
	h := &ChatHandler{Store: threads,
		Answerer: chatAnswererWithModel(t, &chatSearchFake{passages: chatPassages()}, &chatModel{failAnswer: true})}

	rec := httptest.NewRecorder()
	h.Post(rec, chatRequest(http.MethodPost, "/x", `{"question":"what is the spectrum?"}`, uuid.New(), ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(threads.appended) != 1 || threads.appended[0].Text != "what is the spectrum?" {
		t.Errorf("appended = %+v, want the question kept", threads.appended)
	}
}

// An answer built from nothing is recorded as unsupported, which is how
// confabulation is caught later.
func TestAnAnswerFromNoPassagesIsRecordedUnsupported(t *testing.T) {
	threads := newChatThreadStore()
	h := &ChatHandler{Store: threads, Answerer: &rag.Answerer{Store: &chatSearchFake{}}}

	rec := httptest.NewRecorder()
	h.Post(rec, chatRequest(http.MethodPost, "/x", `{"question":"what is a monad?"}`, uuid.New(), ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var out dto.Answer
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Answer != rag.NothingFound {
		t.Errorf("answer = %q, want it said plainly that nothing covers this", out.Answer)
	}
	if !threads.appended[1].Unsupported {
		t.Error("an answer from no passages was not recorded as unsupported")
	}
}

func TestListingConversations(t *testing.T) {
	threads := newChatThreadStore()
	mine, theirs := uuid.New(), uuid.New()
	threads.add(postgres.Conversation{UserID: mine, Title: "mine", UpdatedAt: time.Now()})
	threads.add(postgres.Conversation{UserID: theirs, Title: "theirs"})
	h := &ChatHandler{Store: threads}

	rec := httptest.NewRecorder()
	h.List(rec, chatRequest(http.MethodGet, "/api/v1/conversations", "", mine, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out dto.ConversationList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Items) != 1 || out.Items[0].Title != "mine" {
		t.Errorf("items = %+v, want only my own", out.Items)
	}
	if threads.listLimit <= 0 {
		t.Errorf("listed with limit %d, want a bound", threads.listLimit)
	}

	rec = httptest.NewRecorder()
	h.List(rec, chatRequest(http.MethodGet, "/api/v1/conversations", "", uuid.New(), ""))
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("an empty list is not an array: %s", rec.Body)
	}
}

// Reopening a thread shows every turn, and each citation keeps the number the
// answer used for it.
func TestReadingAConversationBack(t *testing.T) {
	threads := newChatThreadStore()
	user, pinnedFile := uuid.New(), uuid.New()
	work, file := uuid.New(), uuid.New()
	thread := threads.add(postgres.Conversation{UserID: user, Title: "Spectra", BookFileID: &pinnedFile,
		Messages: []postgres.Message{
			{ID: uuid.New(), Seq: 0, Role: "user", Text: "what is the spectrum?"},
			{ID: uuid.New(), Seq: 1, Role: "assistant", Text: "See [2] and [5].", Intent: domain.IntentFactualLookup,
				ModelID: "test-model", Citations: []postgres.Citation{
					{Position: 0, Rank: 2, WorkID: work, BookFileID: file, WorkTitle: "Matrix Analysis",
						Chapter: "Spectra", Locator: "pdf:3:0", Excerpt: "The   spectrum\nis the set."},
					{Position: 1, Rank: 5, WorkTitle: "Another"},
					// A row stored before ranks were: its position stands in.
					{Position: 2, WorkTitle: "Older"},
				}},
		}})
	h := &ChatHandler{Store: threads}

	rec := httptest.NewRecorder()
	h.Get(rec, chatRequest(http.MethodGet, "/x", "", user, thread.ID.String()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var out dto.Conversation
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.ID != thread.ID.String() || out.BookFileID != pinnedFile.String() || len(out.Messages) != 2 {
		t.Fatalf("conversation = %+v", out)
	}
	if out.Messages[0].Citations == nil || len(out.Messages[0].Citations) != 0 {
		t.Errorf("a question's citations = %v, want an empty array", out.Messages[0].Citations)
	}
	cites := out.Messages[1].Citations
	if len(cites) != 3 {
		t.Fatalf("citations = %+v", cites)
	}
	numbers := []int{cites[0].Number, cites[1].Number, cites[2].Number}
	if numbers[0] != 2 || numbers[1] != 5 || numbers[2] != 3 {
		t.Errorf("numbers = %v, want [2 5 3]: the rank, or the position for an old row", numbers)
	}
	if cites[0].WorkID != work.String() || cites[0].BookFile != file.String() || cites[0].Locator != "pdf:3:0" ||
		cites[0].Excerpt != "The spectrum is the set." || !cites[0].Cited {
		t.Errorf("first citation = %+v", cites[0])
	}
	if out.Messages[1].Intent != string(domain.IntentFactualLookup) || out.Messages[1].Model != "test-model" {
		t.Errorf("assistant turn = %+v", out.Messages[1])
	}
}

func TestDeletingMyConversation(t *testing.T) {
	threads := newChatThreadStore()
	user := uuid.New()
	thread := threads.add(postgres.Conversation{UserID: user})
	h := &ChatHandler{Store: threads}

	rec := httptest.NewRecorder()
	h.Delete(rec, chatRequest(http.MethodDelete, "/x", "", user, thread.ID.String()))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(threads.deleted) != 1 || threads.deleted[0] != thread.ID {
		t.Errorf("deleted = %v", threads.deleted)
	}
}

// A thread is named after the question that started it, cut at a word near
// eighty characters and never inside a character.
func TestThreadTitles(t *testing.T) {
	long := strings.Repeat("eigen ", 20)
	if got := titleFor(long); len(got) > 80+len("…") || !strings.HasSuffix(got, "…") ||
		strings.HasSuffix(strings.TrimSuffix(got, "…"), " ") {
		t.Errorf("titleFor(long) = %q", got)
	}
	// One long word of two-byte characters, offset by one byte so the
	// eightieth byte falls inside a character: no space to cut at, so the cut
	// steps back to a character boundary.
	runes := "a" + strings.Repeat("é", 60)
	got := titleFor(runes)
	if !strings.HasSuffix(got, "…") || !utf8.ValidString(got) || len(got) > 80+len("…") {
		t.Errorf("titleFor(a+é×60) = %q, want a clean cut", got)
	}
	if got := titleFor("short\n\tquestion"); got != "short question" {
		t.Errorf("titleFor = %q", got)
	}
}

// The ownership rule, against the real store: one person's thread is
// invisible to another, through every route that takes its id.
func TestConversationOwnershipAgainstTheDatabase(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	p := e2ePool(t, url)
	alice, bob := e2eUser(t, p, false), e2eUser(t, p, false)
	rt := &Router{Health: &HealthHandler{}, Chat: &ChatHandler{Store: p, Answerer: &rag.Answerer{Store: p}}}

	do := func(user uuid.UUID, method, target, body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, target, nil)
		} else {
			req = httptest.NewRequest(method, target, strings.NewReader(body))
		}
		rec := httptest.NewRecorder()
		rt.Handler().ServeHTTP(rec, asUserID(req, user))
		return rec
	}

	// Alice asks with no model and an empty shelf: still an answer, still a
	// thread.
	rec := do(alice, http.MethodPost, "/api/v1/conversations", `{"question":"what is an eigenvector?"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's question: status = %d, body = %s", rec.Code, rec.Body)
	}
	var asked dto.Answer
	if err := json.Unmarshal(rec.Body.Bytes(), &asked); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	thread := "/api/v1/conversations/" + asked.ConversationID

	for _, tc := range []struct{ method, target, body string }{
		{http.MethodGet, thread, ""},
		{http.MethodPost, thread + "/messages", `{"question":"and what did she ask?"}`},
		{http.MethodPost, thread + "/messages/stream", `{"question":"and what did she ask?"}`},
		{http.MethodDelete, thread, ""},
	} {
		if rec := do(bob, tc.method, tc.target, tc.body); rec.Code != http.StatusNotFound {
			t.Errorf("bob %s %s = %d, want 404", tc.method, tc.target, rec.Code)
		}
	}
	if rec := do(bob, http.MethodGet, "/api/v1/conversations", ""); strings.Contains(rec.Body.String(), asked.ConversationID) {
		t.Error("alice's thread is in bob's list")
	}

	rec = do(alice, http.MethodGet, thread, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("alice reading her own thread: %d", rec.Code)
	}
	var mine dto.Conversation
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(mine.Messages) != 2 || mine.Messages[0].Content != "what is an eigenvector?" {
		t.Errorf("alice's thread = %+v, want her question and its answer, and nothing of bob's", mine.Messages)
	}

	if rec := do(alice, http.MethodDelete, thread, ""); rec.Code != http.StatusNoContent {
		t.Errorf("alice deleting her thread = %d, want 204", rec.Code)
	}
	if rec := do(alice, http.MethodGet, thread, ""); rec.Code != http.StatusNotFound {
		t.Errorf("a deleted thread = %d, want 404", rec.Code)
	}
}
