// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
	"scm.poniatowski.dev/adam/lexarii/internal/rag"
)

// providerSecret is in every refusal the fake model server sends, so a test
// can tell whether the provider's own reply reached the person asking.
const providerSecret = "unsupported parameter top_k for account 7f3a"

// failingModel answers the router at once (with a refusal, so routing falls
// back to searching) and the answer itself as the test says.
type failingModel struct {
	status     int
	retryAfter string
	// hang holds the answer until the client gives up, as a model server
	// that has stopped responding does.
	hang    bool
	release chan struct{}
}

func (m *failingModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if bytes.Contains(raw, []byte(`"response_format"`)) {
		http.Error(w, `{"error":{"message":"no router here"}}`, http.StatusBadRequest)
		return
	}
	if m.hang {
		select {
		case <-r.Context().Done():
		case <-m.release:
		}
		return
	}
	if m.retryAfter != "" {
		w.Header().Set("Retry-After", m.retryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(m.status)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":%q}}`, m.status, providerSecret)
}

func failingAnswerer(t *testing.T, m *failingModel) *rag.Answerer {
	t.Helper()
	m.release = make(chan struct{})
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(m.release) })
	passage := retrievedPassage("Frankenstein", "Chapter 5", "I rushed out of the room.", 0, 1)
	return &rag.Answerer{
		Store: &askSearcherFake{found: []domain.RetrievedChunk{passage}},
		// Short, so the hanging server is given up on quickly; the routing
		// and the refusals all answer at once.
		Chat: llm.New(config.ModelConfig{BaseURL: srv.URL, Model: "test-model", Timeout: 300 * time.Millisecond}),
	}
}

// captureLog records what is logged while a test runs.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

var modelFailureCases = []struct {
	name       string
	model      failingModel
	status     int
	code       ErrorCode
	message    string
	retryAfter string
}{
	{"a 429 with Retry-After", failingModel{status: http.StatusTooManyRequests, retryAfter: "17"},
		http.StatusServiceUnavailable, CodeLLMOffline, msgModelBusy, "17"},
	{"a 429 asking for one second", failingModel{status: http.StatusTooManyRequests, retryAfter: "1"},
		http.StatusServiceUnavailable, CodeLLMOffline, msgModelBusy, "1"},
	{"a 503 with no Retry-After", failingModel{status: http.StatusServiceUnavailable},
		http.StatusServiceUnavailable, CodeLLMOffline, msgModelBusy, "60"},
	{"a 500", failingModel{status: http.StatusInternalServerError},
		http.StatusServiceUnavailable, CodeLLMOffline, msgModelBusy, "60"},
	{"a timeout", failingModel{hang: true},
		http.StatusServiceUnavailable, CodeLLMOffline, msgModelBusy, "60"},
	{"a 400", failingModel{status: http.StatusBadRequest},
		http.StatusBadGateway, CodeModelError, msgModelRefused, ""},
	{"a 404 for a model it does not have", failingModel{status: http.StatusNotFound},
		http.StatusBadGateway, CodeModelError, msgModelRefused, ""},
}

// A model server that is busy, slow or refusing is not Lexarii failing: the
// asker is told which, in the house error shape, and never in the provider's
// words.
func TestAskReportsModelFailuresForWhatTheyAre(t *testing.T) {
	for _, tc := range modelFailureCases {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureLog(t)
			model := tc.model
			rt := &Router{Ask: &AskHandler{Answerer: failingAnswerer(t, &model)}}

			rec := serveAsViewer(t, rt, uuid.New(), http.MethodPost, "/api/v1/ask", `{"question":"why does Victor flee?"}`)
			assertModelFailure(t, rec, tc.status, tc.code, tc.message, tc.retryAfter)

			if tc.code == CodeModelError && !strings.Contains(logged.String(), providerSecret) {
				t.Errorf("the log does not name the provider's message:\n%s", logged)
			}
		})
	}
}

// The same for a conversation, which keeps the question either way.
func TestAConversationReportsModelFailuresForWhatTheyAre(t *testing.T) {
	for _, tc := range modelFailureCases {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			threads := newChatThreadStore()
			h := &ChatHandler{Store: threads, Answerer: failingAnswerer(t, &model)}

			rec := httptest.NewRecorder()
			h.Post(rec, chatRequest(http.MethodPost, "/api/v1/conversations",
				`{"question":"why does Victor flee?"}`, uuid.New(), ""))
			assertModelFailure(t, rec, tc.status, tc.code, tc.message, tc.retryAfter)

			if len(threads.appended) != 1 || threads.appended[0].Role != "user" {
				t.Errorf("appended = %+v, want the question kept", threads.appended)
			}
		})
	}
}

// A stream has already sent its status when the model fails, so the error
// event carries the same code and message, and the wait in its details.
func TestAStreamReportsModelFailuresForWhatTheyAre(t *testing.T) {
	for _, tc := range modelFailureCases {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.model
			h := &ChatHandler{Store: newChatThreadStore(), Answerer: failingAnswerer(t, &model)}

			rec := httptest.NewRecorder()
			h.Stream(rec, chatRequest(http.MethodPost, "/api/v1/conversations/stream",
				`{"question":"why does Victor flee?"}`, uuid.New(), ""))

			got := events(t, rec.Body.String())
			last := got[len(got)-1]
			if last[0] != "error" {
				t.Fatalf("last event = %v, want error", last)
			}
			if strings.Contains(last[1], providerSecret) {
				t.Errorf("the error event carries the provider's reply: %s", last[1])
			}
			var e dto.ErrorDetail
			if err := json.Unmarshal([]byte(last[1]), &e); err != nil {
				t.Fatalf("error event: %v", err)
			}
			if e.Code != string(tc.code) || e.Message != tc.message {
				t.Errorf("error event = %+v, want %s %q", e, tc.code, tc.message)
			}
			wantWait := tc.retryAfter
			gotWait := ""
			if v, ok := e.Details["retryAfterSeconds"]; ok {
				raw, _ := json.Marshal(v)
				gotWait = string(raw)
			}
			if gotWait != wantWait {
				t.Errorf("retryAfterSeconds = %q, want %q", gotWait, wantWait)
			}
		})
	}
}

// With no model configured at all nothing here applies: the answer is the
// passages, with a 200, as it always was.
func TestNoModelIsNotAModelFailure(t *testing.T) {
	passage := retrievedPassage("Frankenstein", "Chapter 5", "I rushed out of the room.", 0, 1)
	rt := &Router{Ask: &AskHandler{Answerer: &rag.Answerer{
		Store: &askSearcherFake{found: []domain.RetrievedChunk{passage}}}}}
	rec := serveAsViewer(t, rt, uuid.New(), http.MethodPost, "/api/v1/ask", `{"question":"why does Victor flee?"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func assertModelFailure(t *testing.T, rec *httptest.ResponseRecorder, status int, code ErrorCode, message, retryAfter string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, status, rec.Body)
	}
	if strings.Contains(rec.Body.String(), providerSecret) {
		t.Errorf("the provider's reply reached the client: %s", rec.Body)
	}
	var e dto.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not the house error shape: %v; body = %s", err, rec.Body)
	}
	if e.Error.Code != string(code) || e.Error.Message != message {
		t.Errorf("error = %+v, want %s %q", e.Error, code, message)
	}
	if got := rec.Header().Get("Retry-After"); got != retryAfter {
		t.Errorf("Retry-After = %q, want %q", got, retryAfter)
	}
}

// unusableModel answers the router with a refusal and the answer with
// something that is not an answer.
type unusableModel struct {
	// stream is what a streamed answer sends, event by event.
	stream []string
}

func (m *unusableModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	if bytes.Contains(raw, []byte(`"response_format"`)) {
		http.Error(w, `{"error":{"message":"no router here"}}`, http.StatusBadRequest)
		return
	}
	if !bytes.Contains(raw, []byte(`"stream":true`)) {
		// No choices at all.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range m.stream {
		_, _ = io.WriteString(w, "data: "+e+"\n\n")
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func unusableAnswerer(t *testing.T, m http.Handler, baseURL string) *rag.Answerer {
	t.Helper()
	if baseURL == "" {
		srv := httptest.NewServer(m)
		t.Cleanup(srv.Close)
		baseURL = srv.URL
	}
	passage := retrievedPassage("Frankenstein", "Chapter 5", "I rushed out of the room.", 0, 1)
	return &rag.Answerer{
		Store: &askSearcherFake{found: []domain.RetrievedChunk{passage}},
		Chat:  llm.New(config.ModelConfig{BaseURL: baseURL, Model: "test-model", Timeout: 2 * time.Second}),
	}
}

// A model that answers with nothing usable, or a model server that is
// configured wrongly, is a 502 that says what happened - not "busy", which
// would send the person back to get the same result in a minute.
func TestUnusableAnswersAndMisconfigurationAreModelErrors(t *testing.T) {
	empty := []string{`{"choices":[{"delta":{},"finish_reason":"stop"}]}`}
	budget := []string{
		`{"choices":[{"delta":{"reasoning_content":"thinking about it"}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":4000}}`,
	}

	t.Run("an answer with no choices, asked once", func(t *testing.T) {
		rt := &Router{Ask: &AskHandler{Answerer: unusableAnswerer(t, &unusableModel{}, "")}}
		rec := serveAsViewer(t, rt, uuid.New(), http.MethodPost, "/api/v1/ask", `{"question":"why?"}`)
		assertModelFailure(t, rec, http.StatusBadGateway, CodeModelError, msgModelEmpty, "")
	})
	t.Run("a model server URL that is not http", func(t *testing.T) {
		rt := &Router{Ask: &AskHandler{Answerer: unusableAnswerer(t, nil, "ftp://models.invalid/v1")}}
		rec := serveAsViewer(t, rt, uuid.New(), http.MethodPost, "/api/v1/ask", `{"question":"why?"}`)
		assertModelFailure(t, rec, http.StatusBadGateway, CodeModelError, msgModelRefused, "")
	})

	for _, tc := range []struct {
		name    string
		stream  []string
		message string
	}{
		{"a streamed answer that is empty", empty, msgModelEmpty},
		{"a streamed answer that ran out of tokens thinking", budget, msgModelTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &ChatHandler{Store: newChatThreadStore(),
				Answerer: unusableAnswerer(t, &unusableModel{stream: tc.stream}, "")}
			rec := httptest.NewRecorder()
			h.Stream(rec, chatRequest(http.MethodPost, "/api/v1/conversations/stream",
				`{"question":"why?"}`, uuid.New(), ""))

			got := events(t, rec.Body.String())
			last := got[len(got)-1]
			var e dto.ErrorDetail
			if last[0] != "error" || json.Unmarshal([]byte(last[1]), &e) != nil {
				t.Fatalf("last event = %v, want an error", last)
			}
			if e.Code != string(CodeModelError) || e.Message != tc.message || e.Details != nil {
				t.Errorf("error event = %+v, want model_error %q and no wait", e, tc.message)
			}
		})
	}
}
