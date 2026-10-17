// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
)

func rerankServer(t *testing.T, reply string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rerank" {
			t.Errorf("posted to %q, want /rerank", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		if _, ok := body["query"]; !ok {
			t.Error("request has no query")
		}
		if _, ok := body["documents"]; !ok {
			t.Error("request has no documents")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)

	return New(config.ModelConfig{
		BaseURL: srv.URL, Model: "a-reranker",
		MaxConcurrency: 1, Timeout: 5 * time.Second,
	})
}

// The shape llama.cpp, Jina and Cohere all return.
func TestRerankReadsScores(t *testing.T) {
	c := rerankServer(t, `{"results":[{"index":0,"relevance_score":-3.2},{"index":1,"relevance_score":7.9}]}`)

	got, err := c.Rerank(context.Background(), "a question", []string{"one", "two"})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(got) != 2 || got[1].Index != 1 || got[1].Score != 7.9 {
		t.Fatalf("got %+v", got)
	}
}

// A cross-encoder converted to GGUF without its classification head loads,
// serves, and scores every passage the same - around 1e-28. Nothing errors, and
// the ranking it produces is noise. Noise silently replacing a working ranking
// is the worst outcome available, so it is refused.
func TestRerankRefusesScoresThatCannotOrderAnything(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply string
	}{
		{
			"a headless conversion",
			`{"results":[{"index":0,"relevance_score":1.1353239058953662e-28},
			             {"index":1,"relevance_score":4.5e-23}]}`,
		},
		{
			"every score identical",
			`{"results":[{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.5}]}`,
		},
		{
			"all zero",
			`{"results":[{"index":0,"relevance_score":0},{"index":1,"relevance_score":0}]}`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := rerankServer(t, c.reply)
			_, err := client.Rerank(context.Background(), "a question", []string{"one", "two"})
			if !errors.Is(err, ErrDegenerateScores) {
				t.Fatalf("err = %v, want ErrDegenerateScores", err)
			}
		})
	}
}

// One document cannot be out of order, and a server that says so is not broken.
func TestRerankAcceptsASingleDocument(t *testing.T) {
	c := rerankServer(t, `{"results":[{"index":0,"relevance_score":0}]}`)

	got, err := c.Rerank(context.Background(), "a question", []string{"only one"})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
}

// An index the caller never sent is a different program's bug, and acting on it
// would reorder passages by a number that means nothing here.
func TestRerankRefusesAnIndexItNeverSent(t *testing.T) {
	c := rerankServer(t, `{"results":[{"index":0,"relevance_score":1},{"index":9,"relevance_score":2}]}`)

	if _, err := c.Rerank(context.Background(), "a question", []string{"one", "two"}); err == nil {
		t.Fatal("accepted a score for a document that was not sent")
	}
}

// Voyage returns the same objects under `data` rather than `results`. Without
// this the reply decodes cleanly, yields nothing, and the stage silently does
// nothing at all - which is the worst way for a configuration to be wrong.
func TestRerankAcceptsVoyagesSpelling(t *testing.T) {
	c := rerankServer(t, `{"data":[{"index":0,"relevance_score":0.2},{"index":1,"relevance_score":0.9}],"model":"rerank-2.5"}`)

	got, err := c.Rerank(context.Background(), "a question", []string{"one", "two"})
	if err != nil {
		t.Fatalf("Rerank: %v", err)
	}
	if len(got) != 2 || got[1].Score != 0.9 {
		t.Fatalf("got %+v", got)
	}
}
