// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func collect(t *testing.T, body string) (Reply, []string, error) {
	t.Helper()
	var deltas []string
	reply, err := readStream(t.Context(), strings.NewReader(body), func(s string) {
		deltas = append(deltas, s)
	})
	return reply, deltas, err
}

func frame(content string) string {
	return `data: {"choices":[{"delta":{"content":"` + content + `"}}]}` + "\n\n"
}

func TestStreamAssemblesDeltas(t *testing.T) {
	body := frame("An ") + frame("eigenvalue ") + frame("is") +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":40,"completion_tokens":3}}` + "\n\n" +
		"data: [DONE]\n\n"

	reply, deltas, err := collect(t, body)
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}
	if reply.Content != "An eigenvalue is" {
		t.Fatalf("content = %q", reply.Content)
	}
	if len(deltas) != 3 {
		t.Fatalf("got %d deltas, want 3: %q", len(deltas), deltas)
	}
	if reply.FinishReason != "stop" {
		t.Fatalf("finish reason = %q", reply.FinishReason)
	}
	if reply.CompletionTokens != 3 || reply.PromptTokens != 40 {
		t.Fatalf("usage not read: %+v", reply)
	}
}

// Keep-alive comments and blank lines are part of SSE and must not become text.
func TestStreamIgnoresKeepAlives(t *testing.T) {
	body := ": keep-alive\n\n" + frame("hello") + "\n\n: ping\n\n" + "data: [DONE]\n\n"
	reply, _, err := collect(t, body)
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}
	if reply.Content != "hello" {
		t.Fatalf("content = %q, want %q", reply.Content, "hello")
	}
}

// One bad frame must not discard an answer that is otherwise arriving.
func TestStreamSurvivesOneMalformedFrame(t *testing.T) {
	body := frame("good ") + "data: {not json at all\n\n" + frame("news") + "data: [DONE]\n\n"
	reply, _, err := collect(t, body)
	if err != nil {
		t.Fatalf("readStream: %v", err)
	}
	if reply.Content != "good news" {
		t.Fatalf("content = %q", reply.Content)
	}
}

func TestStreamReportsModelError(t *testing.T) {
	body := frame("partial") + `data: {"error":{"message":"context overflow"}}` + "\n\n"
	reply, _, err := collect(t, body)
	if err == nil || !strings.Contains(err.Error(), "context overflow") {
		t.Fatalf("err = %v, want the model's message", err)
	}
	// What arrived before the failure is still returned, so a caller can decide
	// whether a partial answer is worth keeping.
	if reply.Content != "partial" {
		t.Fatalf("partial content lost: %q", reply.Content)
	}
}

// A reasoning model that never reached an answer is a budget failure, not an
// empty reply: retrying it would hit the same wall.
func TestStreamDistinguishesThinkingFromSilence(t *testing.T) {
	thinking := `data: {"choices":[{"delta":{"reasoning_content":"hmm"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	_, _, err := collect(t, thinking)
	if !errors.Is(err, ErrTokenBudget) {
		t.Fatalf("err = %v, want ErrTokenBudget", err)
	}

	_, _, err = collect(t, "data: [DONE]\n\n")
	if !errors.Is(err, ErrEmptyReply) {
		t.Fatalf("err = %v, want ErrEmptyReply", err)
	}
}

func TestStreamStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := readStream(ctx, strings.NewReader(frame("x")+"data: [DONE]\n\n"), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
