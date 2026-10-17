// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"os"
	"strings"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
)

// TestStreamAgainstARealServer runs against the configured chat model, and
// skips without one.
//
//	set -a; . ./.env; set +a; go test ./internal/llm -run RealServer -v
//
// A fake proves the parser handles the SSE framing the spec allows. Only a real
// server proves that THIS one streams at all, sends the [DONE] sentinel, and
// delivers the answer in more than one piece - which is the entire point, and
// is not something llama.cpp's behaviour can be assumed from the OpenAI docs.
func TestStreamAgainstARealServer(t *testing.T) {
	base := os.Getenv("LEXARII_LLM_BASE_URL")
	if base == "" {
		t.Skip("no LEXARII_LLM_BASE_URL; skipping the live streaming test")
	}

	client := New(config.ModelConfig{
		BaseURL:        base,
		Model:          os.Getenv("LEXARII_LLM_MODEL"),
		APIKey:         os.Getenv("LEXARII_LLM_API_KEY"),
		MaxConcurrency: 1,
		Timeout:        120 * time.Second,
	})

	var deltas []string
	var first, last time.Duration
	start := time.Now()

	reply, err := client.Stream(t.Context(), Request{
		Messages: []Message{
			User("Count from one to ten in words, one per line. No preamble."),
		},
		ReasoningEffort: ReasoningOff,
		MaxTokens:       200,
	}, func(s string) {
		if len(deltas) == 0 {
			first = time.Since(start)
		}
		last = time.Since(start)
		deltas = append(deltas, s)
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if len(deltas) < 2 {
		t.Fatalf("got %d deltas; the server is not streaming, it is sending one chunk", len(deltas))
	}
	if reply.Content != strings.Join(deltas, "") {
		t.Fatal("the assembled reply does not match the deltas it was built from")
	}
	if !strings.Contains(strings.ToLower(reply.Content), "seven") {
		t.Fatalf("the model did not answer the question: %q", reply.Content)
	}

	// The measurement that justifies the feature: text has to start arriving
	// well before the answer is finished, or streaming buys nothing.
	t.Logf("%d deltas, first at %v, last at %v, %d completion tokens",
		len(deltas), first.Round(time.Millisecond), last.Round(time.Millisecond),
		reply.CompletionTokens)
	if first >= last {
		t.Fatal("every delta arrived at once")
	}
}
