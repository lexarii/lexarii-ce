// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
)

// TestOpenRouterLive runs against OpenRouter, and skips without a key.
//
//	OPENROUTER_API_KEY=sk-or-... OPENROUTER_MODEL=vendor/model \
//	  go test ./internal/llm -run OpenRouterLive -v
//
// The httptest servers in provider_test.go reproduce what the documentation
// says OpenRouter sends. Only this shows it still does. It spends a few cents
// at most, and is never run by default.
func TestOpenRouterLive(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("no OPENROUTER_API_KEY; skipping the live OpenRouter test")
	}
	model := os.Getenv("OPENROUTER_MODEL")
	if model == "" {
		t.Skip("no OPENROUTER_MODEL; name a model that supports structured outputs")
	}

	extra, err := config.ParseExtraBody(`{"provider":{"require_parameters":true,"data_collection":"deny"}}`)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.ModelConfig{
		BaseURL:         "https://openrouter.ai/api/v1",
		Model:           model,
		APIKey:          key,
		MaxConcurrency:  1,
		Timeout:         90 * time.Second,
		ExtraBody:       extra,
		ExtraHeaders:    map[string]string{"X-Title": "Lexarii (test)"},
		ReasoningEffort: os.Getenv("OPENROUTER_REASONING_EFFORT"),
	}
	client := New(cfg)

	t.Run("ping accepts the key", func(t *testing.T) {
		if err := client.Ping(t.Context()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	})

	t.Run("ping rejects a bad key", func(t *testing.T) {
		bad := cfg
		bad.APIKey = "sk-or-v1-not-a-real-key"
		if err := New(bad).Ping(t.Context()); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Ping with a bad key = %v", err)
		}
	})

	t.Run("structured output", func(t *testing.T) {
		var out struct {
			Intent string `json:"intent"`
		}
		err := client.JSON(t.Context(), Request{
			Messages:  []Message{User("Classify this question: who wrote Middlemarch?")},
			MaxTokens: 400,
			Schema:    routeSchema,
		}, &out)
		if err != nil {
			t.Fatalf("JSON: %v", err)
		}
		if out.Intent == "" {
			t.Fatal("no intent in the reply")
		}
	})

	t.Run("stream", func(t *testing.T) {
		var pieces int
		reply, err := client.Stream(t.Context(), Request{
			Messages:  []Message{User("Say hello in five words.")},
			MaxTokens: 200,
		}, func(string) { pieces++ })
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		if strings.TrimSpace(reply.Content) == "" || pieces == 0 {
			t.Fatalf("reply %+v in %d pieces", reply, pieces)
		}
		if reply.PromptTokens == 0 {
			t.Error("no usage in the stream")
		}
	})
}
