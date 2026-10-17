// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
)

// These tests stand in for OpenRouter with httptest servers that reproduce the
// behaviour its documentation describes (https://openrouter.ai/docs/api-reference/
// overview, /streaming, /errors-and-debugging, /limits). They prove the client
// copes with the documented shapes; only the live test proves OpenRouter still
// sends them.

func hostedClient(t *testing.T, srv *httptest.Server, mutate func(*config.ModelConfig)) *Client {
	t.Helper()
	cfg := config.ModelConfig{
		BaseURL:        srv.URL + "/api/v1",
		Model:          "vendor/model",
		APIKey:         "sk-or-test",
		MaxConcurrency: 2,
		Timeout:        5 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c := New(cfg)
	if c == nil {
		t.Fatal("client not built")
	}
	return c
}

func chatOK(content string) string {
	return `{"id":"gen-1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":` +
		mustJSON(content) + `}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestExtraBodyIsMergedAndOwnedFieldsWin(t *testing.T) {
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(chatOK("hi")))
	}))
	defer srv.Close()

	extra, err := config.ParseExtraBody(`{"provider":{"require_parameters":true,"data_collection":"deny"},"plugins":[{"id":"response-healing"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	// Smuggled in past config's validation, as a bug or a future edit might: the
	// client must still refuse to let it replace what it owns.
	extra["model"] = json.RawMessage(`"evil/model"`)
	extra["max_tokens"] = json.RawMessage(`999999`)

	c := hostedClient(t, srv, func(cfg *config.ModelConfig) { cfg.ExtraBody = extra })
	if _, err := c.Complete(t.Context(), Request{Messages: []Message{User("q")}, MaxTokens: 50}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if string(got["model"]) != `"vendor/model"` {
		t.Fatalf("model = %s; configuration replaced a field the client owns", got["model"])
	}
	if string(got["max_tokens"]) != `50` {
		t.Fatalf("max_tokens = %s", got["max_tokens"])
	}
	var provider struct {
		RequireParameters bool   `json:"require_parameters"`
		DataCollection    string `json:"data_collection"`
	}
	if err := json.Unmarshal(got["provider"], &provider); err != nil || !provider.RequireParameters || provider.DataCollection != "deny" {
		t.Fatalf("provider = %s, %v", got["provider"], err)
	}
	if _, ok := got["plugins"]; !ok {
		t.Fatal("plugins was not merged")
	}
}

func TestExtraBodyAlsoReachesStreams(t *testing.T) {
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frame("ok")+"data: [DONE]\n\n")
	}))
	defer srv.Close()

	extra, _ := config.ParseExtraBody(`{"provider":{"zdr":true}}`)
	c := hostedClient(t, srv, func(cfg *config.ModelConfig) { cfg.ExtraBody = extra })
	if _, err := c.Stream(t.Context(), Request{Messages: []Message{User("q")}}, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if string(got["provider"]) != `{"zdr":true}` || string(got["stream"]) != `true` {
		t.Fatalf("body = %v", got)
	}
}

func TestNoExtraBodyLeavesTheRequestAlone(t *testing.T) {
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(chatOK("hi")))
	}))
	defer srv.Close()

	c := hostedClient(t, srv, nil)
	if _, err := c.Complete(t.Context(), Request{Messages: []Message{User("q")}}); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"provider", "plugins", "reasoning", "reasoning_effort", "response_format"} {
		if _, ok := got[key]; ok {
			t.Errorf("%q present in a request that asked for nothing like it", key)
		}
	}
}

func TestExtraHeadersAreSentAndAuthorizationIsNotTheirs(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(chatOK("hi")))
	}))
	defer srv.Close()

	c := hostedClient(t, srv, func(cfg *config.ModelConfig) {
		cfg.ExtraHeaders = map[string]string{
			"HTTP-Referer": "https://books.example",
			"X-Title":      "Lexarii",
			// Cannot get here through config; the client must still not honour it.
			"Authorization": "Bearer stolen",
			"Content-Type":  "text/plain",
		}
	})
	if _, err := c.Complete(t.Context(), Request{Messages: []Message{User("q")}}); err != nil {
		t.Fatal(err)
	}
	if got.Get("HTTP-Referer") != "https://books.example" || got.Get("X-Title") != "Lexarii" {
		t.Fatalf("headers = %v", got)
	}
	if got.Get("Authorization") != "Bearer sk-or-test" {
		t.Fatalf("Authorization = %q", got.Get("Authorization"))
	}
	if got.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q", got.Get("Content-Type"))
	}
}

// What a schema request looks like on the wire, which is what OpenRouter's
// structured-outputs documentation specifies: response_format.type json_schema,
// with name, strict and schema inside json_schema.
func TestStructuredOutputRequestShape(t *testing.T) {
	var got struct {
		ResponseFormat struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Name   string         `json:"name"`
				Strict bool           `json:"strict"`
				Schema map[string]any `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
		Provider map[string]any `json:"provider"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(chatOK(`{"intent":"find_book"}`)))
	}))
	defer srv.Close()

	extra, _ := config.ParseExtraBody(`{"provider":{"require_parameters":true}}`)
	c := hostedClient(t, srv, func(cfg *config.ModelConfig) { cfg.ExtraBody = extra })

	var out struct{ Intent string }
	err := c.JSON(t.Context(), Request{Messages: []Message{User("q")}, Schema: routeSchema}, &out)
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if got.ResponseFormat.Type != "json_schema" || got.ResponseFormat.JSONSchema.Name != "query_route" ||
		!got.ResponseFormat.JSONSchema.Strict || got.ResponseFormat.JSONSchema.Schema["type"] != "object" {
		t.Fatalf("response_format = %+v", got.ResponseFormat)
	}
	if got.Provider["require_parameters"] != true {
		t.Fatalf("provider = %v", got.Provider)
	}
}

func TestReasoningEffortOverride(t *testing.T) {
	cases := []struct {
		name     string
		override string
		asked    string
		want     string // "" means the field must be absent
	}{
		{"unset keeps what was asked", "", ReasoningOff, "none"},
		{"default drops the field", config.ReasoningDefault, ReasoningOff, ""},
		{"a value replaces it", "low", ReasoningOff, "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]json.RawMessage
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &got)
				_, _ = w.Write([]byte(chatOK("hi")))
			}))
			defer srv.Close()
			c := hostedClient(t, srv, func(cfg *config.ModelConfig) { cfg.ReasoningEffort = tc.override })
			if _, err := c.Complete(t.Context(), Request{Messages: []Message{User("q")}, ReasoningEffort: tc.asked}); err != nil {
				t.Fatal(err)
			}
			raw, present := got["reasoning_effort"]
			if tc.want == "" && present {
				t.Fatalf("reasoning_effort = %s, want absent", raw)
			}
			if tc.want != "" && string(raw) != `"`+tc.want+`"` {
				t.Fatalf("reasoning_effort = %s, want %q", raw, tc.want)
			}
		})
	}
}

// OpenRouter names the thinking "reasoning", not "reasoning_content". A model
// that spends its whole budget on it must still be recognised as that, because
// the fix (raise the budget) is not the fix for an empty reply (retry).
func TestOpenRouterReasoningFieldIsRecognised(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","reasoning":"Let me think about this..."}}]}`)
	}))
	defer srv.Close()

	c := hostedClient(t, srv, nil)
	_, err := c.retry(t.Context(), Request{Messages: []Message{User("q")}, MaxTokens: 100})
	if !errors.Is(err, ErrTokenBudget) || !strings.Contains(err.Error(), "reasoning") {
		t.Fatalf("err = %v, want ErrTokenBudget naming reasoning", err)
	}
}

func TestChoiceErrorInA200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"error","message":{"content":""},"error":{"code":502,"message":"Provider returned error"}}]}`)
	}))
	defer srv.Close()

	_, err := hostedClient(t, srv, nil).Complete(t.Context(), Request{Messages: []Message{User("q")}})
	var perr *ProviderError
	if !errors.As(err, &perr) || perr.Code != "502" || !perr.Retryable() {
		t.Fatalf("err = %v", err)
	}
}

// --- streaming ---

func TestStreamSkipsOpenRouterProcessingComments(t *testing.T) {
	body := ": OPENROUTER PROCESSING\n\n: OPENROUTER PROCESSING\n\n" +
		`data: {"id":"gen-1","provider":"x","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}` + "\n\n" +
		": OPENROUTER PROCESSING\n\n" +
		`data: {"id":"gen-1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}` + "\n\n" +
		`data: {"id":"gen-1","choices":[{"index":0,"delta":{"content":""},"finish_reason":"stop","native_finish_reason":"stop"}]}` + "\n\n" +
		// The documented usage frame: a choice with an empty delta repeating the
		// finish reason, plus usage. It is accounting, not a second answer.
		`data: {"id":"gen-1","choices":[{"index":0,"delta":{"content":"","role":"assistant"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"cost":0.0001}}` + "\n\n" +
		"data: [DONE]\n\n"

	reply, deltas, err := collect(t, body)
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}
	if reply.Content != "Hello" || len(deltas) != 2 {
		t.Fatalf("content %q, deltas %q", reply.Content, deltas)
	}
	if reply.PromptTokens != 11 || reply.CompletionTokens != 2 || reply.FinishReason != "stop" {
		t.Fatalf("reply = %+v", reply)
	}
}

func TestStreamSurfacesAMidStreamError(t *testing.T) {
	// The documented shape: HTTP 200 already sent, error at the top level with a
	// STRING code, and a choice finishing with "error".
	body := frame("The answer is") +
		`data: {"id":"cmpl-abc123","object":"chat.completion.chunk","created":1234567890,"model":"openai/gpt-4o","provider":"openai","error":{"code":"server_error","message":"Provider disconnected unexpectedly"},"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}` + "\n\n"

	reply, deltas, err := collect(t, body)
	var perr *ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want a ProviderError", err)
	}
	if perr.Code != "server_error" || !strings.Contains(err.Error(), "Provider disconnected unexpectedly") {
		t.Fatalf("err = %v", err)
	}
	if !perr.Retryable() {
		t.Error("a server_error should be worth retrying")
	}
	// What was already delivered stands.
	if reply.Content != "The answer is" || len(deltas) != 1 {
		t.Fatalf("partial answer lost: %q %q", reply.Content, deltas)
	}
}

// The error can be the first and only event; a 200 carrying it is a failure and
// must not be reported as an empty-but-successful answer.
func TestStreamErrorAsTheOnlyEvent(t *testing.T) {
	body := ": OPENROUTER PROCESSING\n\n" +
		`data: {"error":{"code":429,"message":"Rate limit exceeded"},"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}` + "\n\n"
	_, _, err := collect(t, body)
	var perr *ProviderError
	if !errors.As(err, &perr) || perr.Code != "429" || !perr.Retryable() {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamFinishReasonErrorWithoutAnObject(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}` + "\n\n"
	_, _, err := collect(t, body)
	var perr *ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want a ProviderError", err)
	}
}

func TestStreamRecognisesOpenRouterReasoningDeltas(t *testing.T) {
	body := `data: {"choices":[{"delta":{"reasoning":"hmm"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"","reasoning":"still hmm"},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	reply, deltas, err := collect(t, body)
	if !errors.Is(err, ErrTokenBudget) || !reply.Reasoned || len(deltas) != 0 {
		t.Fatalf("reply %+v deltas %q err %v", reply, deltas, err)
	}
}

func TestStreamNon200BeforeCommitIsAnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"code":402,"message":"Insufficient credits. Add more using https://openrouter.ai/settings/credits"}}`)
	}))
	defer srv.Close()

	_, err := hostedClient(t, srv, nil).Stream(t.Context(), Request{Messages: []Message{User("q")}}, nil)
	if !errors.Is(err, ErrInsufficientCredits) {
		t.Fatalf("err = %v", err)
	}
}

// --- errors and retry policy ---

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		header     map[string]string
		body       string
		retryable  bool
		sentinel   error
		wantInText string
	}{
		{
			name: "401 bad key", status: 401,
			body:       `{"error":{"code":401,"message":"No auth credentials found"}}`,
			sentinel:   ErrUnauthorized,
			wantInText: "No auth credentials found",
		},
		{
			name: "402 out of credits", status: 402,
			body:       `{"error":{"code":402,"message":"Insufficient credits","metadata":{"limit_source":"openrouter_credits"}}}`,
			sentinel:   ErrInsufficientCredits,
			wantInText: "Insufficient credits",
		},
		{
			name: "402 in-flight budget is transient", status: 402,
			header:    map[string]string{"Retry-After": "2"},
			body:      `{"error":{"code":402,"message":"would exceed in-flight","metadata":{"limit_source":"openrouter_in_flight_budget"}}}`,
			retryable: true, sentinel: ErrInsufficientCredits,
		},
		{
			name: "429 with retry-after", status: 429,
			header:    map[string]string{"Retry-After": "7"},
			body:      `{"error":{"code":429,"message":"Rate limited"}}`,
			retryable: true,
		},
		{name: "400 is final", status: 400, body: `{"error":{"code":400,"message":"Invalid model specified"}}`},
		{name: "403 is final", status: 403, body: `{"error":{"code":403,"message":"flagged"}}`},
		{name: "502 provider error", status: 502, body: `{"error":{"code":502,"message":"upstream"}}`, retryable: true},
		{name: "503 no providers", status: 503, body: `{"error":{"code":503,"message":"no available providers"}}`, retryable: true},
		{name: "408 timeout", status: 408, body: `{"error":{"code":408,"message":"timed out"}}`, retryable: true},
		{name: "a non-JSON body still works", status: 500, body: `upstream exploded`, retryable: true, wantInText: "upstream exploded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			_, err := hostedClient(t, srv, nil).Complete(t.Context(), Request{Messages: []Message{User("q")}})
			var httpErr *HTTPError
			if !errors.As(err, &httpErr) || httpErr.Status != tc.status {
				t.Fatalf("err = %v", err)
			}
			if httpErr.Retryable() != tc.retryable || retryable(err) != tc.retryable {
				t.Errorf("retryable = %v, want %v", httpErr.Retryable(), tc.retryable)
			}
			if tc.sentinel != nil && !errors.Is(err, tc.sentinel) {
				t.Errorf("errors.Is(%v) = false", tc.sentinel)
			}
			if tc.wantInText != "" && !strings.Contains(err.Error(), tc.wantInText) {
				t.Errorf("%q missing from %q", tc.wantInText, err.Error())
			}
			// A message extracted from the envelope reads as prose, not as JSON.
			if httpErr.Message != "" && strings.Contains(err.Error(), `{"error"`) {
				t.Errorf("error text is raw JSON: %q", err.Error())
			}
		})
	}
}

func TestRetryAfterIsHonouredAndNotHammered(t *testing.T) {
	var calls atomic.Int32
	var first, second time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			first = time.Now()
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":429,"message":"slow down"}}`)
		default:
			second = time.Now()
			_, _ = io.WriteString(w, chatOK("fine"))
		}
	}))
	defer srv.Close()

	reply, err := hostedClient(t, srv, nil).retry(t.Context(), Request{Messages: []Message{User("q")}})
	if err != nil || reply.Content != "fine" {
		t.Fatalf("reply %+v err %v", reply, err)
	}
	if gap := second.Sub(first); gap < 900*time.Millisecond {
		t.Fatalf("retried after %v, Retry-After was 1s", gap)
	}
}

func TestLongRetryAfterIsHandedBackNotSlept(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":429,"message":"daily free-model limit"}}`)
	}))
	defer srv.Close()

	start := time.Now()
	_, err := hostedClient(t, srv, nil).retry(t.Context(), Request{Messages: []Message{User("q")}})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.RetryAfter != 10*time.Minute {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 || time.Since(start) > 2*time.Second {
		t.Fatalf("%d calls in %v; a ten-minute Retry-After must not be slept on", calls.Load(), time.Since(start))
	}
	if !retryable(err) {
		t.Error("the caller should still be told this is transient, so the job is requeued")
	}
}

func TestA402WithoutRetryAfterIsNeverRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"code":402,"message":"Insufficient credits"}}`)
	}))
	defer srv.Close()

	_, err := hostedClient(t, srv, nil).retry(t.Context(), Request{Messages: []Message{User("q")}})
	if !errors.Is(err, ErrInsufficientCredits) || calls.Load() != 1 {
		t.Fatalf("calls %d err %v", calls.Load(), err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Duration{
		"":     0,
		"30":   30 * time.Second,
		" 5 ":  5 * time.Second,
		"-3":   0,
		"soon": 0,
		"0":    0,
		// The day cap, and either side of it.
		"86400":  24 * time.Hour,
		"86401":  24 * time.Hour,
		"604800": 24 * time.Hour,
		// These wrapped when multiplied into nanoseconds: the first to a
		// NEGATIVE duration, the second to 290ms.
		"9223372037":  24 * time.Hour,
		"18446744074": 24 * time.Hour,
		// Not an int64 at all: unreadable, and so not believed.
		"99999999999999999999999999":                      0,
		now.Add(1000 * time.Hour).Format(http.TimeFormat): 24 * time.Hour,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(-time.Minute).Format(http.TimeFormat):     0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBackoffIsJitteredAndBounded(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 50 {
		d := backoff(1, errors.New("x"), Background)
		if d < 187*time.Millisecond || d > 313*time.Millisecond {
			t.Fatalf("first backoff %v outside 250ms +-25%%", d)
		}
		seen[d] = true
	}
	if len(seen) < 5 {
		t.Fatalf("only %d distinct values in 50 draws; backoff is not jittered", len(seen))
	}
}

// --- health ---

// OpenRouter serves /models to anyone, so a 200 there says nothing about the
// key. A key it rejects on /key must make Ping fail.
func TestPingFailsOnABadKeyWhereModelsIsPublic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/models":
			_, _ = io.WriteString(w, `{"data":[]}`)
		case "/api/v1/key":
			if r.Header.Get("Authorization") != "Bearer good" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"Missing Authentication header","code":401}}`)
				return
			}
			_, _ = io.WriteString(w, `{"data":{"label":"x"}}`)
		}
	}))
	defer srv.Close()

	bad := hostedClient(t, srv, func(cfg *config.ModelConfig) { cfg.APIKey = "bad" })
	if err := bad.Ping(t.Context()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Ping with a bad key = %v, want ErrUnauthorized", err)
	}
	good := hostedClient(t, srv, func(cfg *config.ModelConfig) { cfg.APIKey = "good" })
	if err := good.Ping(t.Context()); err != nil {
		t.Fatalf("Ping with a good key = %v", err)
	}
}

// llama.cpp has no /key route. That must not read as unhealthy, and with no key
// configured nothing extra is asked at all.
func TestPingToleratesServersWithoutAKeyEndpoint(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/api/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()

	if err := hostedClient(t, srv, nil).Ping(t.Context()); err != nil {
		t.Fatalf("Ping = %v", err)
	}
	paths = nil
	if err := hostedClient(t, srv, func(cfg *config.ModelConfig) { cfg.APIKey = "" }).Ping(t.Context()); err != nil {
		t.Fatalf("Ping = %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("paths = %v; an unauthenticated server needs one request", paths)
	}
}

// A server that rejects the key on /models itself (llama-server --api-key does,
// on its protected routes) is not healthy, even though something is listening:
// it would refuse every request Lexarii sends it. Before, any status under 500
// counted as "listening" and this reported healthy.
func TestPingFailsWhenModelsRejectsTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	if err := hostedClient(t, srv, nil).Ping(t.Context()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Ping = %v", err)
	}
}

func TestExtraHeadersReachThePingToo(t *testing.T) {
	var title string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		title = r.Header.Get("X-Title")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	c := hostedClient(t, srv, func(cfg *config.ModelConfig) {
		cfg.APIKey = ""
		cfg.ExtraHeaders = map[string]string{"X-Title": "Lexarii"}
	})
	if err := c.Ping(context.Background()); err != nil || title != "Lexarii" {
		t.Fatalf("err %v title %q", err, title)
	}
}
