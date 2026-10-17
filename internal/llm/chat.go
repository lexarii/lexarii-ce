// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message is one turn in a conversation.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// System, User, and Assistant build messages without stringly-typed roles at
// every call site.
func System(content string) Message    { return Message{Role: "system", Content: content} }
func User(content string) Message      { return Message{Role: "user", Content: content} }
func Assistant(content string) Message { return Message{Role: "assistant", Content: content} }

// Request is one completion.
type Request struct {
	Messages []Message
	// Temperature defaults to 0. Enrichment is extraction, not writing: the same
	// book should produce the same tags today and next week, or the provenance
	// ledger records a decision nobody can reproduce.
	Temperature float32
	MaxTokens   int
	// Schema, when set, constrains the reply to JSON matching it.
	Schema *Schema
	// Stop sequences, passed through to the server.
	Stop []string
	// Priority says whether someone is waiting on this. Background work yields
	// the model to interactive requests.
	Priority Priority
	// ReasoningEffort controls how much a reasoning model thinks before it
	// answers. Empty leaves the server's default alone; "none" asks it not to.
	//
	// It matters far more than it sounds. Measured against gemma4-12b: the same
	// request took 6 completion tokens with reasoning off, and used its entire
	// 4,000-token budget on thinking - returning NO answer at all - with it on.
	// For extraction, where the prompt already says exactly what is wanted,
	// thinking buys nothing and costs a minute a book.
	//
	// Servers that do not know the field ignore it, which is why the token
	// budget still has to be generous enough to survive being ignored.
	ReasoningEffort string
}

// Schema is a JSON schema the reply must satisfy.
type Schema struct {
	// Name is required by some servers and ignored by others.
	Name string
	// Definition is the JSON Schema itself, as a Go value that marshals to one.
	Definition any
	// Strict asks the server to refuse anything that does not match. Supported
	// by llama.cpp and by OpenAI; harmless where it is not.
	Strict bool
}

// ErrEmptyReply reports a response with no content, which some servers return
// instead of an error when a request is rejected.
var ErrEmptyReply = errors.New("the model returned nothing")

// ErrBadJSON reports a reply that was not the JSON that was asked for.
var ErrBadJSON = errors.New("the model did not return valid JSON")

// ReasoningOff asks a model not to think before answering.
//
// The right setting for extraction: reading a title off a page is not a
// question that benefits from deliberation.
const ReasoningOff = "none"

// ErrTokenBudget reports a reply cut off before the model reached its answer.
//
// Its own error because the fix is a setting, not a retry: raise MaxTokens, or
// use a model that thinks less. Retrying spends the same budget the same way.
var ErrTokenBudget = errors.New("the model ran out of tokens before answering")

// reasoningHint names the cause when a reasoning model is the reason.
func reasoningHint(r Reply) string {
	if r.Reasoned {
		return " (it spent the budget reasoning; raise MaxTokens)"
	}
	return ""
}

// chatRequest is the wire format. Only the fields Lexarii actually sets are
// present, so a server's own defaults are never silently overridden.
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature float32   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Stop        []string  `json:"stop,omitempty"`
	// ResponseFormat carries the schema. llama.cpp, vLLM, and OpenAI all accept
	// this shape; servers that do not simply ignore it, which is why the reply
	// is validated in Go regardless.
	ResponseFormat  *responseFormat `json:"response_format,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	// StreamOptions asks for the usage totals, which a streaming response
	// otherwise omits entirely: without it every streamed answer records zero
	// tokens, and the numbers that explain a slow answer are simply absent.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type responseFormat struct {
	Type       string      `json:"type"`
	JSONSchema *jsonSchema `json:"json_schema,omitempty"`
}

type jsonSchema struct {
	Name   string `json:"name"`
	Schema any    `json:"schema"`
	Strict bool   `json:"strict,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
			// ReasoningContent is where a reasoning model puts its thinking.
			//
			// Read only to tell two failures apart: a model that answered with
			// nothing, and one that spent its whole token budget thinking and
			// never reached the answer. Those need opposite responses and look
			// identical without this field.
			ReasoningContent string `json:"reasoning_content"`
			// Reasoning is the same thing under OpenRouter's name for it.
			Reasoning string `json:"reasoning"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
		// Error is how OpenRouter reports a failure that happened after it
		// had committed to a 200.
		Error *wireError `json:"error"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *wireError `json:"error"`
}

// Reply is a completion and what it cost.
type Reply struct {
	Content      string
	FinishReason string
	// Reasoned is true when the model returned thinking alongside its answer.
	// Not stored anywhere: it is how a budget problem is told from a refusal.
	Reasoned         bool
	PromptTokens     int
	CompletionTokens int
}

// Truncated reports whether the model stopped because it hit the token limit
// rather than because it finished.
//
// Worth checking before parsing: a truncated JSON object is invalid JSON, and
// the useful error is "the answer did not fit", not "the model cannot count
// braces".
func (r Reply) Truncated() bool { return r.FinishReason == "length" }

// Complete sends one chat completion.
//
// A nil client returns ErrNotConfigured, which callers must treat as "this
// feature is switched off" rather than as a failure.
func (c *Client) Complete(ctx context.Context, req Request) (Reply, error) {
	if c == nil {
		return Reply{}, ErrNotConfigured
	}

	body := chatRequest{
		Model:           c.model,
		Messages:        req.Messages,
		Temperature:     req.Temperature,
		MaxTokens:       req.MaxTokens,
		Stop:            req.Stop,
		ReasoningEffort: c.effort(req.ReasoningEffort),
	}
	if req.Schema != nil {
		name := req.Schema.Name
		if name == "" {
			name = "response"
		}
		body.ResponseFormat = &responseFormat{
			Type: "json_schema",
			JSONSchema: &jsonSchema{
				Name:   name,
				Schema: req.Schema.Definition,
				Strict: req.Schema.Strict,
			},
		}
	}

	encoded, err := c.encode(body)
	if err != nil {
		return Reply{}, err
	}

	reply, err := c.post(ctx, "/chat/completions", encoded, req.Priority)
	if err != nil {
		return Reply{}, err
	}
	return reply, nil
}

// post sends one request, with the gate held for its duration.
func (c *Client) post(ctx context.Context, path string, body []byte, priority Priority) (Reply, error) {
	release, err := c.acquireAs(ctx, priority)
	if err != nil {
		return Reply{}, err
	}
	defer release()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return Reply{}, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorise(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return Reply{}, transportError(c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Bounded: a server answering with a gigabyte must not become this process's
	// memory problem.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Reply{}, interruptedError("reading response", err)
	}

	if resp.StatusCode != http.StatusOK {
		return Reply{}, newHTTPError(resp, raw)
	}

	var decoded chatResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Reply{}, fmt.Errorf("decoding response: %w", err)
	}
	if decoded.Error != nil {
		return Reply{}, decoded.Error.asError()
	}
	if len(decoded.Choices) == 0 {
		return Reply{}, ErrEmptyReply
	}
	if decoded.Choices[0].Error != nil {
		return Reply{}, decoded.Choices[0].Error.asError()
	}

	return Reply{
		Content:      decoded.Choices[0].Message.Content,
		FinishReason: decoded.Choices[0].FinishReason,
		Reasoned: decoded.Choices[0].Message.ReasoningContent != "" ||
			decoded.Choices[0].Message.Reasoning != "",
		PromptTokens:     decoded.Usage.PromptTokens,
		CompletionTokens: decoded.Usage.CompletionTokens,
	}, nil
}

// maxResponseBytes bounds a single reply.
const maxResponseBytes = 8 << 20

// retryable reports whether an error is worth another attempt.
func retryable(err error) bool {
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.Retryable()
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return providerErr.Retryable()
	}
	// A transport error is a network that might come back; a cancelled context
	// is a decision that will not.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, ErrNotConfigured) {
		return false
	}
	return true
}

// JSON sends a request and decodes the reply into out.
//
// The reply is validated in Go whatever the server claimed to enforce. A
// schema-constrained endpoint is a request, not a guarantee: llama.cpp honours
// it, some servers ignore it silently, and a model told to emit JSON will still
// occasionally wrap it in a code fence or add a sentence of commentary. One
// repair attempt is made, showing the model its own output and the error.
func (c *Client) JSON(ctx context.Context, req Request, out any) error {
	if c == nil {
		return ErrNotConfigured
	}

	reply, err := c.retry(ctx, req)
	if err != nil {
		return err
	}

	content := extractJSON(reply.Content)
	if content != "" {
		if err := json.Unmarshal([]byte(content), out); err == nil {
			return nil
		}
	}

	// The repair turn. The model sees what it said and what was wrong with it,
	// which is far more likely to work than asking the same question again.
	because := "it was not valid JSON"
	if reply.Truncated() {
		// Asking a model to repair an answer that was cut off produces another
		// answer that is cut off. This is a budget problem, not a format one.
		return fmt.Errorf("%w: the reply was cut off at the %d-token limit%s",
			ErrTokenBudget, req.MaxTokens, reasoningHint(reply))
	}
	if content == "" {
		because = "no JSON object could be found in it"
	}

	repair := req
	repair.Messages = append(append([]Message{}, req.Messages...),
		Assistant(truncate(reply.Content, 2000)),
		User("That reply could not be used, because "+because+
			". Reply again with the JSON object only: no explanation, no code fence, nothing before or after it."),
	)

	retried, err := c.retry(ctx, repair)
	if err != nil {
		return err
	}
	repaired := extractJSON(retried.Content)
	if repaired == "" {
		return fmt.Errorf("%w: %s", ErrBadJSON, truncate(retried.Content, 200))
	}
	if err := json.Unmarshal([]byte(repaired), out); err != nil {
		return fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	return nil
}

// retry sends a request, backing off on failures that might pass.
func (c *Client) retry(ctx context.Context, req Request) (Reply, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			// Doubling from a quarter second. The model server is one machine
			// doing one thing at a time; hammering it makes the queue longer,
			// not shorter.
			wait := backoff(attempt, lastErr, req.Priority)
			if wait < 0 {
				// The server asked for longer than is worth holding a worker
				// for. Hand the error up and let the job layer requeue it.
				return Reply{}, fmt.Errorf("after %d attempts: %w", attempt, lastErr)
			}
			select {
			case <-ctx.Done():
				return Reply{}, ctx.Err()
			case <-time.After(wait):
			}
		}

		reply, err := c.Complete(ctx, req)
		if err == nil {
			if strings.TrimSpace(reply.Content) == "" {
				// A reasoning model that ran out of budget while thinking
				// returns no answer and stops for "length". Retrying that is
				// pointless at temperature 0 - the same prompt spends the same
				// budget the same way - and on a library it is the same wasted
				// minute per book, every book.
				if reply.Truncated() {
					return Reply{}, fmt.Errorf(
						"%w: the model used its entire %d-token budget before answering%s",
						ErrTokenBudget, req.MaxTokens, reasoningHint(reply))
				}
				lastErr = ErrEmptyReply
				continue
			}
			return reply, nil
		}
		lastErr = err
		if !retryable(err) {
			return Reply{}, err
		}
	}
	return Reply{}, fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// maxAttempts bounds retries. Three is enough to ride out a restart and few
// enough that a genuinely broken request fails while someone is still watching.
const maxAttempts = 3

// extractJSON pulls the JSON value out of a reply.
//
// Models wrap JSON in code fences, prefix it with "Here is the JSON:", and
// append a closing remark, whatever the prompt says. The text around it is not
// an error worth failing on when the object itself is right there.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	// A fenced block, with or without a language tag.
	if fence := strings.Index(s, "```"); fence >= 0 {
		rest := s[fence+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			// Drop the language tag on the fence line, if any.
			if tag := strings.TrimSpace(rest[:nl]); tag == "" || isWord(tag) {
				rest = rest[nl+1:]
			}
		}
		if end := strings.Index(rest, "```"); end >= 0 {
			rest = rest[:end]
		}
		s = strings.TrimSpace(rest)
	}

	// The outermost balanced object or array. Scanning rather than taking the
	// first and last brace, so a trailing "}" in prose does not swallow it.
	for i, r := range s {
		if r == '{' || r == '[' {
			if end := matchBracket(s, i); end > i {
				return s[i : end+1]
			}
		}
	}
	return ""
}

// matchBracket returns the index closing the bracket at start, or -1.
//
// String-aware, because a brace inside a JSON string value is not structure -
// and book metadata is full of them.
func matchBracket(s string, start int) int {
	open := s[start]
	closing := byte('}')
	if open == '[' {
		closing = ']'
	}

	depth := 0
	inString := false
	escaped := false

	for i := start; i < len(s); i++ {
		ch := s[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inString:
			escaped = true
		case ch == '"':
			inString = !inString
		case inString:
			// Nothing inside a string is structure.
		case ch == open:
			depth++
		case ch == closing:
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// isWord reports whether s is a bare identifier, which is what a code-fence
// language tag looks like.
func isWord(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_':
		default:
			return false
		}
	}
	return s != ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
