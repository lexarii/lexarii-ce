// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
)

// fakeServer is an OpenAI-compatible server that replies with whatever the test
// tells it to.
//
// A fake rather than a live model: the behaviour being tested is what happens
// when a model returns a code fence, prose, half an object, or a 503 - all of
// which real models do, and none of which can be produced on demand from a real
// one.
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []chatRequest
	replies  []func(w http.ResponseWriter, r *http.Request)
	calls    atomic.Int64
}

func newFakeServer(t *testing.T, replies ...func(w http.ResponseWriter, r *http.Request)) *fakeServer {
	t.Helper()
	f := &fakeServer{replies: replies}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(f.calls.Add(1)) - 1

		body, _ := io.ReadAll(r.Body)
		var decoded chatRequest
		_ = json.Unmarshal(body, &decoded)
		f.mu.Lock()
		f.requests = append(f.requests, decoded)
		f.mu.Unlock()

		if n < len(f.replies) {
			f.replies[n](w, r)
			return
		}
		// Past the scripted replies, keep answering with the last one, so a test
		// that retries does not get a confusing 404.
		if len(f.replies) > 0 {
			f.replies[len(f.replies)-1](w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeServer) client(t *testing.T) *Client {
	t.Helper()
	return New(config.ModelConfig{
		BaseURL:        f.URL + "/v1",
		Model:          "test-model",
		MaxConcurrency: 2,
		Timeout:        5 * time.Second,
	})
}

func (f *fakeServer) lastRequest(t *testing.T) chatRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("the server received no requests")
	}
	return f.requests[len(f.requests)-1]
}

// says replies with content, as a well-formed completion.
func says(content string) func(http.ResponseWriter, *http.Request) {
	return saysWithReason(content, "stop")
}

func saysWithReason(content, reason string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": content},
				"finish_reason": reason,
			}},
			"usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 20},
		})
	}
}

func status(code int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

type subjects struct {
	Subjects []string `json:"subjects"`
	Level    string   `json:"level"`
}

func TestCompleteSendsTheConfiguredModel(t *testing.T) {
	server := newFakeServer(t, says("a reply"))
	client := server.client(t)

	reply, err := client.Complete(context.Background(), Request{
		Messages:  []Message{System("be brief"), User("hello")},
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if reply.Content != "a reply" {
		t.Errorf("content = %q", reply.Content)
	}
	if reply.PromptTokens != 100 || reply.CompletionTokens != 20 {
		t.Errorf("usage not carried through: %+v", reply)
	}

	sent := server.lastRequest(t)
	if sent.Model != "test-model" {
		t.Errorf("model = %q", sent.Model)
	}
	if len(sent.Messages) != 2 || sent.Messages[0].Role != "system" {
		t.Errorf("messages = %+v", sent.Messages)
	}
	if sent.MaxTokens != 64 {
		t.Errorf("max_tokens = %d", sent.MaxTokens)
	}
}

// Enrichment is extraction, not writing: the same book must produce the same
// answer twice, or the provenance ledger records a decision nobody can check.
func TestCompleteDefaultsToDeterministicTemperature(t *testing.T) {
	server := newFakeServer(t, says("ok"))
	if _, err := server.client(t).Complete(context.Background(), Request{
		Messages: []Message{User("hello")},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if temp := server.lastRequest(t).Temperature; temp != 0 {
		t.Errorf("temperature = %v, want 0", temp)
	}
}

func TestCompleteSendsTheSchema(t *testing.T) {
	server := newFakeServer(t, says(`{"subjects":["algebra"],"level":"undergraduate"}`))

	var out subjects
	err := server.client(t).JSON(context.Background(), Request{
		Messages: []Message{User("classify")},
		Schema: &Schema{
			Name:   "classification",
			Strict: true,
			Definition: map[string]any{
				"type":     "object",
				"required": []string{"subjects", "level"},
			},
		},
	}, &out)
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}

	sent := server.lastRequest(t)
	if sent.ResponseFormat == nil || sent.ResponseFormat.Type != "json_schema" {
		t.Fatalf("response_format = %+v", sent.ResponseFormat)
	}
	if sent.ResponseFormat.JSONSchema.Name != "classification" || !sent.ResponseFormat.JSONSchema.Strict {
		t.Errorf("schema not sent faithfully: %+v", sent.ResponseFormat.JSONSchema)
	}
	if len(out.Subjects) != 1 || out.Subjects[0] != "algebra" {
		t.Errorf("decoded = %+v", out)
	}
}

// What models actually do, whatever the prompt says.
func TestJSONSurvivesModelHabits(t *testing.T) {
	tests := map[string]string{
		"bare object":      `{"subjects":["maths"],"level":"school"}`,
		"code fence":       "```json\n{\"subjects\":[\"maths\"],\"level\":\"school\"}\n```",
		"unlabelled fence": "```\n{\"subjects\":[\"maths\"],\"level\":\"school\"}\n```",
		"preamble":         "Here is the JSON you asked for:\n{\"subjects\":[\"maths\"],\"level\":\"school\"}",
		"closing remark":   "{\"subjects\":[\"maths\"],\"level\":\"school\"}\nLet me know if you need anything else!",
		"both":             "Sure!\n```json\n{\"subjects\":[\"maths\"],\"level\":\"school\"}\n```\nHope that helps.",
		"leading newline":  "\n\n  {\"subjects\":[\"maths\"],\"level\":\"school\"}  \n",
	}

	for name, reply := range tests {
		t.Run(name, func(t *testing.T) {
			server := newFakeServer(t, says(reply))

			var out subjects
			if err := server.client(t).JSON(context.Background(), Request{
				Messages: []Message{User("classify")},
			}, &out); err != nil {
				t.Fatalf("JSON: %v", err)
			}
			if len(out.Subjects) != 1 || out.Subjects[0] != "maths" || out.Level != "school" {
				t.Errorf("decoded = %+v", out)
			}
			if server.calls.Load() != 1 {
				t.Errorf("took %d calls; none of these should need a repair turn", server.calls.Load())
			}
		})
	}
}

// A brace inside a string value is not structure. Book metadata is full of
// punctuation, and scanning for the last "}" would cut the object short.
func TestJSONHandlesBracesInsideStrings(t *testing.T) {
	server := newFakeServer(t, says(`{"subjects":["a } brace","an \"escaped\" quote"],"level":"odd"}`))

	var out subjects
	if err := server.client(t).JSON(context.Background(), Request{
		Messages: []Message{User("classify")},
	}, &out); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if len(out.Subjects) != 2 || out.Subjects[0] != "a } brace" {
		t.Errorf("decoded = %+v", out)
	}
}

// The repair turn: the model is shown its own output and what was wrong.
func TestJSONRepairsABadReply(t *testing.T) {
	server := newFakeServer(t,
		says("I'm not sure I can answer that."),
		says(`{"subjects":["repaired"],"level":"school"}`),
	)

	var out subjects
	if err := server.client(t).JSON(context.Background(), Request{
		Messages: []Message{User("classify")},
	}, &out); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if len(out.Subjects) != 1 || out.Subjects[0] != "repaired" {
		t.Errorf("decoded = %+v", out)
	}

	// The repair turn must carry the original conversation plus what went wrong,
	// or it is just the same question asked twice.
	sent := server.lastRequest(t)
	if len(sent.Messages) != 3 {
		t.Fatalf("repair sent %d messages, want the original plus the failed reply and the correction", len(sent.Messages))
	}
	if sent.Messages[1].Role != "assistant" || !strings.Contains(sent.Messages[1].Content, "not sure") {
		t.Errorf("the model was not shown its own reply: %+v", sent.Messages[1])
	}
	if !strings.Contains(sent.Messages[2].Content, "JSON object only") {
		t.Errorf("the correction did not say what to do: %q", sent.Messages[2].Content)
	}
}

func TestJSONGivesUpAfterOneRepair(t *testing.T) {
	server := newFakeServer(t, says("still not JSON"))

	var out subjects
	err := server.client(t).JSON(context.Background(), Request{Messages: []Message{User("classify")}}, &out)
	if !errors.Is(err, ErrBadJSON) {
		t.Fatalf("error = %v, want ErrBadJSON", err)
	}
	// Two turns, not an unbounded argument with the model.
	if calls := server.calls.Load(); calls != 2 {
		t.Errorf("made %d calls, want 2", calls)
	}
}

// A truncated reply is a budget problem. Asking the model to repair an answer
// that was cut off produces another answer that is cut off.
func TestJSONDoesNotRepairATruncatedReply(t *testing.T) {
	server := newFakeServer(t, saysWithReason(`{"subjects":["algeb`, "length"))

	var out subjects
	err := server.client(t).JSON(context.Background(), Request{Messages: []Message{User("classify")}}, &out)
	if !errors.Is(err, ErrTokenBudget) {
		t.Fatalf("error = %v, want ErrTokenBudget", err)
	}
	if !strings.Contains(err.Error(), "cut off") {
		t.Errorf("error = %q, want it to name the real cause", err)
	}
	if calls := server.calls.Load(); calls != 1 {
		t.Errorf("made %d calls; a truncated reply must not be argued with", calls)
	}
}

// A reasoning model puts its thinking in a separate field and fills `content`
// only once it is done. Given too small a budget it spends the lot thinking and
// returns an EMPTY answer that stopped for "length".
//
// Found against gemma4-12b, which does exactly this. Retrying it is the worst
// possible response: at temperature 0 the same prompt spends the same budget
// the same way, so it is three wasted minutes per book across a whole library.
func TestReasoningModelOutOfBudgetIsNotRetried(t *testing.T) {
	server := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role":              "assistant",
					"content":           "",
					"reasoning_content": "Let me think about what this book is about. It appears to be...",
				},
				"finish_reason": "length",
			}},
		})
	})

	var out subjects
	err := server.client(t).JSON(context.Background(),
		Request{Messages: []Message{User("classify")}, MaxTokens: 800}, &out)

	if !errors.Is(err, ErrTokenBudget) {
		t.Fatalf("error = %v, want ErrTokenBudget", err)
	}
	if calls := server.calls.Load(); calls != 1 {
		t.Errorf("made %d calls, want 1: retrying spends the same budget the same way", calls)
	}
	// The message has to name the cause, or the fix - raise the budget - is
	// indistinguishable from "the model is broken".
	if !strings.Contains(err.Error(), "reasoning") {
		t.Errorf("error = %q, want it to say the budget went on reasoning", err)
	}
	if !strings.Contains(err.Error(), "800") {
		t.Errorf("error = %q, want it to name the budget that was too small", err)
	}
}

// An empty answer that stopped normally is a different thing: the model had
// room and said nothing, which a retry can legitimately fix.
func TestEmptyButNotTruncatedReplyIsRetried(t *testing.T) {
	server := newFakeServer(t,
		says(""),
		says(`{"subjects":["second time"],"level":"school"}`),
	)

	var out subjects
	if err := server.client(t).JSON(context.Background(),
		Request{Messages: []Message{User("classify")}}, &out); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if len(out.Subjects) != 1 || out.Subjects[0] != "second time" {
		t.Errorf("decoded = %+v", out)
	}
}

func TestReplyReportsReasoning(t *testing.T) {
	server := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"role": "assistant", "content": "ok", "reasoning_content": "thinking",
				},
				"finish_reason": "stop",
			}},
		})
	})

	reply, err := server.client(t).Complete(context.Background(), Request{Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// The answer is the answer; the thinking is not part of it and is never
	// stored, shown, or embedded.
	if reply.Content != "ok" {
		t.Errorf("content = %q, want the answer without the thinking", reply.Content)
	}
	if !reply.Reasoned {
		t.Error("a reply carrying reasoning did not report it")
	}
}

// A busy server is worth waiting for.
func TestRetriesOnServerBusy(t *testing.T) {
	server := newFakeServer(t,
		status(http.StatusServiceUnavailable, "loading model"),
		status(http.StatusTooManyRequests, "slow down"),
		says(`{"subjects":["eventually"],"level":"school"}`),
	)

	var out subjects
	if err := server.client(t).JSON(context.Background(), Request{Messages: []Message{User("classify")}}, &out); err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if out.Subjects[0] != "eventually" {
		t.Errorf("decoded = %+v", out)
	}
}

// A 400 will be a 400 every time. Retrying it only wastes the user's GPU.
func TestDoesNotRetryARejectedRequest(t *testing.T) {
	server := newFakeServer(t, status(http.StatusBadRequest, `{"error":{"message":"context too long"}}`))

	var out subjects
	err := server.client(t).JSON(context.Background(), Request{Messages: []Message{User("classify")}}, &out)
	if err == nil {
		t.Fatal("a rejected request was reported as success")
	}
	if calls := server.calls.Load(); calls != 1 {
		t.Errorf("made %d calls, want 1", calls)
	}

	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest {
		t.Fatalf("error = %v, want an HTTPError carrying the status", err)
	}
	// The server's own explanation is the useful part and must survive.
	if !strings.Contains(err.Error(), "context too long") {
		t.Errorf("error = %q, want the server's message kept", err)
	}
}

func TestGivesUpAfterBoundedRetries(t *testing.T) {
	server := newFakeServer(t, status(http.StatusInternalServerError, "broken"))

	_, err := server.client(t).retry(context.Background(), Request{Messages: []Message{User("hello")}})
	if err == nil {
		t.Fatal("a permanently failing server was reported as success")
	}
	if calls := server.calls.Load(); calls != maxAttempts {
		t.Errorf("made %d calls, want %d", calls, maxAttempts)
	}
}

// The gate is what keeps a home GPU usable while a library enriches.
func TestConcurrencyIsBounded(t *testing.T) {
	var inFlight, peak atomic.Int64

	server := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		now := inFlight.Add(1)
		for {
			old := peak.Load()
			if now <= old || peak.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		inFlight.Add(-1)
		says("ok")(w, nil)
	})

	client := New(config.ModelConfig{
		BaseURL:        server.URL + "/v1",
		Model:          "test-model",
		MaxConcurrency: 2,
		Timeout:        5 * time.Second,
	})

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.Complete(context.Background(), Request{Messages: []Message{User("hello")}})
		}()
	}
	wg.Wait()

	if peak.Load() > 2 {
		t.Errorf("%d requests were in flight at once, want at most 2", peak.Load())
	}
	if peak.Load() < 2 {
		t.Errorf("peak concurrency was %d; the gate is serialising more than asked", peak.Load())
	}
}

// Shutdown must not wait for a queue of model requests.
func TestGateReleasesOnCancellation(t *testing.T) {
	release := make(chan struct{})
	server := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		<-release
		says("ok")(w, nil)
	})
	t.Cleanup(func() { close(release) })

	client := New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "m", MaxConcurrency: 1, Timeout: 5 * time.Second,
	})

	// Occupy the only slot.
	go func() { _, _ = client.Complete(context.Background(), Request{Messages: []Message{User("first")}}) }()
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := client.Complete(ctx, Request{Messages: []Message{User("second")}})
	if err == nil {
		t.Fatal("a request waiting on a full gate succeeded with no slot free")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %v for a cancelled request", elapsed)
	}
}

// The invariant the whole project rests on: with no model configured, nothing
// here is an error worth failing a library over.
func TestNilClientIsSwitchedOffNotBroken(t *testing.T) {
	var client *Client

	if _, err := client.Complete(context.Background(), Request{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Complete = %v, want ErrNotConfigured", err)
	}
	if err := client.JSON(context.Background(), Request{}, &subjects{}); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("JSON = %v, want ErrNotConfigured", err)
	}
	if err := client.Ping(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Ping = %v, want ErrNotConfigured", err)
	}
	if client.Model() != "" {
		t.Error("a nil client claimed to have a model")
	}
	if retryable(ErrNotConfigured) {
		t.Error("an unconfigured model would be retried forever")
	}
}

// An unreachable server is the ordinary state of a box in someone's house.
func TestUnreachableServerIsReportedClearly(t *testing.T) {
	client := New(config.ModelConfig{
		// Reserved for documentation, so nothing can be listening.
		BaseURL: "http://192.0.2.1:9/v1", Model: "m", MaxConcurrency: 1, Timeout: 200 * time.Millisecond,
	})

	_, err := client.Complete(context.Background(), Request{Messages: []Message{User("hello")}})
	if err == nil {
		t.Fatal("an unreachable server was reported as success")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error = %q, want it to say the model could not be reached", err)
	}
}

func TestEmptyReplyIsAnError(t *testing.T) {
	server := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	})

	_, err := server.client(t).Complete(context.Background(), Request{Messages: []Message{User("hello")}})
	if !errors.Is(err, ErrEmptyReply) {
		t.Fatalf("error = %v, want ErrEmptyReply", err)
	}
}

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"text before {\"a\":1} text after", `{"a":1}`},
		{`[1,2,3]`, `[1,2,3]`},
		{`{"a":{"b":[1,2]}}`, `{"a":{"b":[1,2]}}`},
		{`{"a":"}"}`, `{"a":"}"}`},
		{`{"a":"\""}`, `{"a":"\""}`},
		{"no json here", ""},
		{"", ""},
		{`{"unclosed":`, ""},
	}

	for _, tt := range tests {
		if got := extractJSON(tt.in); got != tt.want {
			t.Errorf("extractJSON(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestHTTPErrorRetryable(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusBadRequest:          false,
		http.StatusUnauthorized:        false,
		http.StatusNotFound:            false,
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusServiceUnavailable:  true,
		http.StatusBadGateway:          true,
	} {
		err := &HTTPError{Status: status}
		if got := err.Retryable(); got != want {
			t.Errorf("status %d retryable = %v, want %v", status, got, want)
		}
	}
}

func TestTruncatedReply(t *testing.T) {
	if !(Reply{FinishReason: "length"}).Truncated() {
		t.Error("a length-limited reply did not report itself truncated")
	}
	if (Reply{FinishReason: "stop"}).Truncated() {
		t.Error("a finished reply reported itself truncated")
	}
}

// A request must not be able to hang a worker forever.
func TestRequestHonoursContext(t *testing.T) {
	block := make(chan struct{})
	server := newFakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		<-block
		says("ok")(w, nil)
	})
	t.Cleanup(func() { close(block) })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := server.client(t).Complete(ctx, Request{Messages: []Message{User("hello")}})
	if err == nil {
		t.Fatal("a hanging request returned success")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v to honour a 150ms context", elapsed)
	}
}

func TestTruncateHelper(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate kept = %q", got)
	}
	if got := truncate(strings.Repeat("x", 20), 5); got != "xxxxx…" {
		t.Errorf("truncate cut = %q", got)
	}
}

func ExampleClient_JSON() {
	// Illustrative only: the client is nil, so this is the switched-off path.
	var client *Client
	var out subjects
	err := client.JSON(context.Background(), Request{
		Messages: []Message{User("classify this book")},
	}, &out)
	fmt.Println(err)
	// Output: not configured
}
