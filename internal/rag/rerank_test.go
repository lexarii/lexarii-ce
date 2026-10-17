// SPDX-License-Identifier: AGPL-3.0-or-later

package rag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
	"scm.poniatowski.dev/adam/lexarii/internal/domain"
	"scm.poniatowski.dev/adam/lexarii/internal/llm"
)

func candidates(n int) []domain.RetrievedChunk {
	out := make([]domain.RetrievedChunk, n)
	for i := range out {
		out[i] = domain.RetrievedChunk{
			WorkID:     uuid.New(),
			WorkTitle:  string(rune('A' + i)),
			FusedScore: float64(n - i),
		}
	}
	return out
}

func titles(got []domain.RetrievedChunk) string {
	s := ""
	for _, c := range got {
		s += c.WorkTitle
	}
	return s
}

func TestApplyRanking(t *testing.T) {
	tests := []struct {
		name   string
		ranked []int
		keep   int
		want   string
		wantOK bool
	}{
		{"reorders", []int{3, 1, 2}, 10, "CABDE", true},
		{"keeps only what was asked for", []int{5, 4, 3, 2, 1}, 2, "ED", true},

		// What the model did not rank follows in fused order rather than being
		// discarded: letting it drop passages cost 9 points of Recall@10 for 2
		// of MRR, so a bad judgement costs position, not the answer.
		{"unranked passages follow behind", []int{2}, 10, "BACDE", true},
		{"unranked passages respect the limit", []int{5}, 3, "EAB", true},

		// A model that returns nothing has said the library does not cover the
		// question. That is a real answer, not a failure to rank.
		{"an empty ranking is honoured", []int{}, 10, "", true},

		// Invented and repeated numbers are discarded, not trusted.
		{"ignores out-of-range numbers", []int{99, 2, 0, -1}, 10, "BACDE", true},
		{"ignores repeats", []int{2, 2, 1}, 10, "BACDE", true},

		// Nothing usable at all is a failure, and the caller falls back to the
		// fused order rather than showing an empty result.
		{"all invented is a failure", []int{99, 100}, 10, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := apply(candidates(5), tt.ranked, tt.keep)
			if ok != tt.wantOK {
				t.Fatalf("apply(%v) ok = %v, want %v", tt.ranked, ok, tt.wantOK)
			}
			if titles(got) != tt.want {
				t.Fatalf("apply(%v) = %q, want %q", tt.ranked, titles(got), tt.want)
			}
		})
	}
}

func TestApplyScoresInRankOrder(t *testing.T) {
	got, ok := apply(candidates(5), []int{4, 2, 5}, 10)
	if !ok || len(got) != 5 {
		t.Fatalf("apply returned %d passages, ok=%v", len(got), ok)
	}
	// The three the model ranked score in descending order.
	for i := 1; i < 3; i++ {
		if got[i-1].RerankScore <= got[i].RerankScore {
			t.Fatalf("rerank scores not descending: %v then %v",
				got[i-1].RerankScore, got[i].RerankScore)
		}
	}
	// The two it did not rank carry no rerank score, which is how a reranked
	// passage is told from one that merely survived.
	for _, c := range got[3:] {
		if c.RerankScore != 0 {
			t.Fatalf("unranked passage carries a rerank score: %v", c.RerankScore)
		}
	}
}

// A nil model must leave the fused order alone rather than empty the answer.
func TestRerankWithoutAModelKeepsFusedOrder(t *testing.T) {
	in := candidates(5)
	got := Rerank(t.Context(), nil, "anything", in, 3)
	if titles(got) != "ABC" {
		t.Fatalf("Rerank without a model = %q, want %q", titles(got), "ABC")
	}
}

func TestClipStaysOnRuneBoundaries(t *testing.T) {
	s := clip("héllo wörld this is a much longer passage than the limit", 20)
	for _, r := range s {
		if r == '�' {
			t.Fatalf("clip split a rune: %q", s)
		}
	}
	if len(s) > 24 {
		t.Fatalf("clip returned %d bytes: %q", len(s), s)
	}
}

// The whole stage against a stand-in cross-encoder: nothing here serves one, so
// this is the only place the wiring is exercised end to end.
func TestRerankOrdersByTheCrossEncoderScore(t *testing.T) {
	// Scores deliberately disagree with the retrieved order, since a reranker
	// that agreed with retrieval would prove nothing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding: %v", err)
		}
		if body.Query != "a question" {
			t.Errorf("query = %q", body.Query)
		}
		if len(body.Documents) != 4 {
			t.Errorf("sent %d documents, want 4", len(body.Documents))
		}
		// Reverse the retrieved order.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[
			{"index":0,"relevance_score":-5},
			{"index":1,"relevance_score":-1},
			{"index":2,"relevance_score":3},
			{"index":3,"relevance_score":8}]}`))
	}))
	defer srv.Close()

	client := llm.New(config.ModelConfig{
		BaseURL: srv.URL, Model: "a-reranker", MaxConcurrency: 1, Timeout: 5 * time.Second,
	})

	got := Rerank(context.Background(), client, "a question", candidates(4), 4)
	if titles(got) != "DCBA" {
		t.Fatalf("order = %q, want DCBA - the scores reversed the retrieved order", titles(got))
	}
}

// A reranker that fails must leave the answer alone rather than take it down.
// That includes the degenerate-score case, where the scores exist and mean
// nothing: keeping retrieval's order is strictly better than ordering by noise.
func TestRerankFallsBackToTheRetrievedOrder(t *testing.T) {
	for _, c := range []struct {
		name    string
		status  int
		payload string
	}{
		{"the server is down", http.StatusInternalServerError, `{}`},
		{"scores that cannot order", http.StatusOK,
			`{"results":[{"index":0,"relevance_score":1e-28},{"index":1,"relevance_score":2e-28},
			             {"index":2,"relevance_score":1e-28},{"index":3,"relevance_score":3e-28}]}`},
		{"nonsense in the reply", http.StatusOK, `not json at all`},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.payload))
			}))
			defer srv.Close()

			client := llm.New(config.ModelConfig{
				BaseURL: srv.URL, Model: "a-reranker", MaxConcurrency: 1, Timeout: 5 * time.Second,
			})

			got := Rerank(context.Background(), client, "a question", candidates(4), 4)
			if titles(got) != "ABCD" {
				t.Fatalf("order = %q, want the retrieved order ABCD", titles(got))
			}
		})
	}
}

// No reranker configured is the default, and must cost nothing.
func TestRerankWithoutARerankerKeepsTheOrder(t *testing.T) {
	got := Rerank(context.Background(), nil, "a question", candidates(4), 3)
	if titles(got) != "ABC" {
		t.Fatalf("order = %q, want the first three retrieved", titles(got))
	}
}

// A comparison must come back with both of its sides, which is the whole point
// of splitting it. Round-robin rather than by score, because the scores come
// from different queries and comparing them means nothing.
func TestInterleaveTakesFromEveryArm(t *testing.T) {
	arm := func(ids ...string) []domain.RetrievedChunk {
		out := make([]domain.RetrievedChunk, 0, len(ids))
		for _, id := range ids {
			out = append(out, domain.RetrievedChunk{
				Chunk: domain.Chunk{ID: uuid.NewSHA1(uuid.Nil, []byte(id)), Content: id},
			})
		}
		return out
	}
	names := func(chunks []domain.RetrievedChunk) []string {
		out := make([]string, 0, len(chunks))
		for _, c := range chunks {
			out = append(out, c.Chunk.Content)
		}
		return out
	}

	got := interleave([][]domain.RetrievedChunk{arm("go1", "go2", "go3"), arm("java1", "java2")}, 4)
	want := []string{"go1", "java1", "go2", "java2"}
	if !reflect.DeepEqual(names(got), want) {
		t.Errorf("interleave = %v, want %v", names(got), want)
	}

	// A short arm stops contributing and the other fills the rest, rather than
	// the answer coming back short.
	got = interleave([][]domain.RetrievedChunk{arm("go1", "go2", "go3"), arm("java1")}, 4)
	want = []string{"go1", "java1", "go2", "go3"}
	if !reflect.DeepEqual(names(got), want) {
		t.Errorf("with one short arm, interleave = %v, want %v", names(got), want)
	}

	// Both arms can find the same passage. It is one passage.
	got = interleave([][]domain.RetrievedChunk{arm("shared", "go2"), arm("shared", "java2")}, 4)
	want = []string{"shared", "go2", "java2"}
	if !reflect.DeepEqual(names(got), want) {
		t.Errorf("with a shared passage, interleave = %v, want %v", names(got), want)
	}
}
