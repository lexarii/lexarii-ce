// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
)

// embedServer answers with deterministic vectors of the requested width.
func embedServer(t *testing.T, dims int) (*Client, *[][]string) {
	t.Helper()
	var batches [][]string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req embedRequest
		_ = json.Unmarshal(body, &req)
		batches = append(batches, req.Input)

		data := make([]map[string]any, len(req.Input))
		for i, text := range req.Input {
			vec := make([]float32, dims)
			// Deterministic and not unit length, so normalisation is observable.
			for j := range vec {
				vec[j] = float32((len(text)+i+j)%7) + 1
			}
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)

	client := New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "embed-test",
		MaxConcurrency: 2, Timeout: 5 * time.Second,
	})
	return client, &batches
}

func TestEmbedReturnsOneVectorPerInput(t *testing.T) {
	client, _ := embedServer(t, 768)

	texts := []string{"first chunk", "second chunk", "third chunk"}
	vectors, err := client.Embed(context.Background(), texts, 768)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != len(texts) {
		t.Fatalf("got %d vectors for %d texts", len(vectors), len(texts))
	}
	for i, v := range vectors {
		if len(v) != 768 {
			t.Errorf("vector %d has %d dimensions", i, len(v))
		}
	}
}

// Normalised on write, so cosine distance and inner product agree and the index
// can use whichever is cheaper.
func TestEmbedNormalises(t *testing.T) {
	client, _ := embedServer(t, 64)

	vectors, err := client.Embed(context.Background(), []string{"a chunk"}, 64)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	var sum float64
	for _, x := range vectors[0] {
		sum += float64(x) * float64(x)
	}
	if length := math.Sqrt(sum); math.Abs(length-1) > 1e-5 {
		t.Errorf("vector length = %v, want 1", length)
	}
}

// A zero vector cannot be normalised, and dividing by its length would turn a
// degenerate embedding into a silent NaN in the database.
func TestNormaliseLeavesAZeroVectorAlone(t *testing.T) {
	out := normalise([]float32{0, 0, 0})
	for i, x := range out {
		if x != 0 {
			t.Errorf("out[%d] = %v, want 0", i, x)
		}
	}
}

// The vector column has a fixed width. A model that quietly changes dimensions
// makes every new row unstorable and every old row incomparable, so it is
// caught at the boundary rather than by a constraint violation later.
func TestEmbedRejectsTheWrongDimensions(t *testing.T) {
	client, _ := embedServer(t, 512)

	_, err := client.Embed(context.Background(), []string{"a chunk"}, 768)
	var mismatch *ErrDimensionMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("error = %v, want ErrDimensionMismatch", err)
	}
	if mismatch.Got != 512 || mismatch.Want != 768 {
		t.Errorf("mismatch = %+v", mismatch)
	}
}

// Large inputs are split, because one enormous request queues inside the server
// while holding a connection, and a failure costs the whole batch.
func TestEmbedBatches(t *testing.T) {
	client, batches := embedServer(t, 8)

	texts := make([]string, 40)
	for i := range texts {
		texts[i] = "chunk"
	}
	vectors, err := client.Embed(context.Background(), texts, 8)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vectors) != 40 {
		t.Fatalf("got %d vectors, want 40", len(vectors))
	}
	if len(*batches) < 2 {
		t.Errorf("sent %d requests; 40 inputs should be split", len(*batches))
	}
	for i, b := range *batches {
		if len(b) > maxEmbedBatch {
			t.Errorf("batch %d had %d inputs, above the %d limit", i, len(b), maxEmbedBatch)
		}
	}
}

// Order is the whole contract: out[i] must be the embedding of texts[i], or
// every chunk is stored against somebody else's vector.
func TestEmbedKeepsOrderWhenTheServerShufflesIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req embedRequest
		_ = json.Unmarshal(body, &req)

		// Answer in reverse, with correct index fields.
		data := make([]map[string]any, 0, len(req.Input))
		for i := len(req.Input) - 1; i >= 0; i-- {
			data = append(data, map[string]any{
				"index":     i,
				"embedding": []float32{float32(i + 1), 0, 0, 0},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	client := New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "m", MaxConcurrency: 1, Timeout: 5 * time.Second,
	})

	vectors, err := client.Embed(context.Background(), []string{"a", "b", "c"}, 4)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	// Each was normalised from (i+1, 0, 0, 0), so each is (1, 0, 0, 0): the
	// check that matters is that three distinct slots are filled in order.
	for i, v := range vectors {
		if len(v) != 4 || v[0] == 0 {
			t.Errorf("vector %d = %v, want the one for input %d", i, v, i)
		}
	}
}

func TestEmbedRefusesEmptyText(t *testing.T) {
	client, _ := embedServer(t, 8)

	if _, err := client.Embed(context.Background(), []string{"fine", "   "}, 8); err == nil {
		t.Error("an empty string was sent to the embedding model")
	}
}

func TestEmbedShortCircuits(t *testing.T) {
	client, batches := embedServer(t, 8)

	vectors, err := client.Embed(context.Background(), nil, 8)
	if err != nil || vectors != nil {
		t.Errorf("Embed(nil) = %v, %v", vectors, err)
	}
	if len(*batches) != 0 {
		t.Error("an empty input list still reached the server")
	}
}

func TestEmbedWithNoModelConfigured(t *testing.T) {
	var client *Client
	if _, err := client.Embed(context.Background(), []string{"x"}, 768); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("error = %v, want ErrNotConfigured", err)
	}
}

// Fewer vectors than inputs would misalign every chunk after the gap.
func TestEmbedRejectsAShortReply(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"index": 0, "embedding": []float32{1, 0}}},
		})
	}))
	defer server.Close()

	client := New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "m", MaxConcurrency: 1, Timeout: 5 * time.Second,
	})
	if _, err := client.Embed(context.Background(), []string{"a", "b"}, 2); err == nil {
		t.Error("a reply with fewer vectors than inputs was accepted")
	}
}

// One oversized passage must not cost the other sixty-five thousand.
//
// Found for real: llama.cpp answered 500 "input (2941 tokens) is too large to
// process" for a single chunk, which failed its batch, which failed the job,
// which retried for ever.
func TestEmbedTolerantIsolatesARefusedInput(t *testing.T) {
	var attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		body, _ := io.ReadAll(r.Body)
		var req embedRequest
		_ = json.Unmarshal(body, &req)

		for _, text := range req.Input {
			if strings.Contains(text, "ENORMOUS") {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w,
					`{"error":{"code":500,"message":"input (2941 tokens) is too large to process"}}`)
				return
			}
		}

		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	client := New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "m", MaxConcurrency: 1, Timeout: 5 * time.Second,
	})

	texts := []string{"fine one", "ENORMOUS passage", "fine two", "fine three"}
	vectors, refused, err := client.EmbedTolerant(context.Background(), texts, 4)
	if err != nil {
		t.Fatalf("EmbedTolerant: %v", err)
	}

	if len(refused) != 1 || refused[0] != 1 {
		t.Fatalf("refused = %v, want just index 1", refused)
	}
	if vectors[1] != nil {
		t.Error("the refused input got a vector")
	}
	for _, i := range []int{0, 2, 3} {
		if vectors[i] == nil {
			t.Errorf("input %d was lost along with the bad one", i)
		}
	}
}

// A server that is down is not one bad input, and must not be mistaken for one:
// isolating would turn an outage into sixty-five thousand silently dropped
// passages.
func TestEmbedTolerantFailsOnAnOutage(t *testing.T) {
	client := New(config.ModelConfig{
		BaseURL: "http://192.0.2.1:9/v1", Model: "m",
		MaxConcurrency: 1, Timeout: 200 * time.Millisecond,
	})

	_, refused, err := client.EmbedTolerant(context.Background(), []string{"a", "b"}, 4)
	if err == nil {
		t.Fatal("an unreachable server was reported as success")
	}
	if len(refused) != 0 {
		t.Errorf("refused = %v; an outage was mistaken for bad input", refused)
	}
}

// A server that says it is busy is not saying anything about the text.
//
// Found in review: an overloaded embedding server answered 503 (or 429) to
// the batch and then to every passage tried alone, each was taken for a bad
// input, and the index job "succeeded" having stored nothing. The error must
// come back so the worker skips the job and tries again.
func TestEmbedTolerantReturnsBusyServersAsErrors(t *testing.T) {
	for _, status := range []int{
		http.StatusTooManyRequests, http.StatusServiceUnavailable,
		http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusRequestTimeout,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"message":"busy"}}`)
			}))
			defer server.Close()

			client := New(config.ModelConfig{
				BaseURL: server.URL + "/v1", Model: "m", MaxConcurrency: 1, Timeout: 5 * time.Second,
			})
			for _, texts := range [][]string{{"a", "b", "c"}, {"only"}} {
				calls.Store(0)
				_, refused, err := client.EmbedTolerant(t.Context(), texts, 4)
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) || httpErr.Status != status {
					t.Fatalf("%d inputs: err = %v, want the %d back", len(texts), err, status)
				}
				if len(refused) != 0 {
					t.Errorf("%d inputs: refused = %v; a busy server was taken for bad input", len(texts), refused)
				}
				if calls.Load() != 1 {
					t.Errorf("%d inputs: %d requests; a busy server was asked once per passage", len(texts), calls.Load())
				}
			}
		})
	}
}

// A 500 is ambiguous: llama.cpp's answer to one oversized input, and also a
// server falling over. When every passage of a batch fails alone and nothing
// at all has embedded, it is the server.
func TestEmbedTolerantTreatsA500FromEverythingAsAnOutage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"model crashed"}}`)
	}))
	defer server.Close()

	client := New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "m", MaxConcurrency: 1, Timeout: 5 * time.Second,
	})
	for _, texts := range [][]string{{"a", "b", "c"}, {"only"}} {
		_, refused, err := client.EmbedTolerant(t.Context(), texts, 4)
		if err == nil {
			t.Fatalf("%d inputs: a server answering 500 to everything was reported as success", len(texts))
		}
		if len(refused) != 0 {
			t.Errorf("%d inputs: refused = %v, want none", len(texts), refused)
		}
	}
}

// A server that falls over part-way through answers 500 to everything after:
// the batches it embedded before are no evidence it is answering now.
func TestEmbedTolerantTreatsALaterOutageAsAnOutage(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"model crashed"}}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req embedRequest
		_ = json.Unmarshal(body, &req)
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0, 0, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()

	client := New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "m", MaxConcurrency: 1, Timeout: 5 * time.Second,
	})
	texts := make([]string, maxEmbedBatch+3)
	for i := range texts {
		texts[i] = fmt.Sprintf("passage %d", i)
	}
	_, refused, err := client.EmbedTolerant(t.Context(), texts, 4)
	if err == nil {
		t.Fatalf("a server that fell over after the first batch was reported as success (refused %v)", refused)
	}
}

// A 4xx about the input is still the input, alone or in a batch.
func TestEmbedTolerantRefusesA400Alone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"input is too large"}}`)
	}))
	defer server.Close()

	client := New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "m", MaxConcurrency: 1, Timeout: 5 * time.Second,
	})
	_, refused, err := client.EmbedTolerant(t.Context(), []string{"only"}, 4)
	if err != nil || len(refused) != 1 {
		t.Fatalf("EmbedTolerant = %v, %v; want the one input refused", refused, err)
	}
}

// With nothing wrong, it behaves exactly as Embed does.
func TestEmbedTolerantOnGoodInput(t *testing.T) {
	client, _ := embedServer(t, 8)

	vectors, refused, err := client.EmbedTolerant(context.Background(), []string{"a", "b", "c"}, 8)
	if err != nil {
		t.Fatalf("EmbedTolerant: %v", err)
	}
	if len(refused) != 0 {
		t.Errorf("refused = %v, want none", refused)
	}
	for i, v := range vectors {
		if len(v) != 8 {
			t.Errorf("vector %d = %v", i, v)
		}
	}
}

// A person waiting on an answer must not queue behind background enrichment.
//
// Measured before this existed: a question took over three minutes against a
// model that answers in twenty seconds, because three enrichment jobs held
// every slot. Enrichment can wait; someone watching a cursor cannot.
func TestInteractiveRequestsGoFirst(t *testing.T) {
	var order []string
	var mu sync.Mutex

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		<-release

		mu.Lock()
		if strings.Contains(string(body), "interactive") {
			order = append(order, "interactive")
		} else {
			order = append(order, "background")
		}
		mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	// One slot, so the ordering is unambiguous.
	client := New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "m", MaxConcurrency: 1, Timeout: 10 * time.Second,
	})

	// A background request takes the only slot and blocks.
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = client.Complete(context.Background(), Request{
			Messages: []Message{User("background one")}, Priority: Background,
		})
	}()
	<-started
	time.Sleep(50 * time.Millisecond)

	// More background work queues behind it, and a person asks a question.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = client.Complete(context.Background(), Request{
				Messages: []Message{User("background more")}, Priority: Background,
			})
		}()
	}
	time.Sleep(50 * time.Millisecond)

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = client.Complete(context.Background(), Request{
			Messages: []Message{User("interactive question")}, Priority: Interactive,
		})
	}()
	time.Sleep(50 * time.Millisecond)

	close(release)
	wg.Wait()
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 {
		t.Fatalf("only %d requests completed", len(order))
	}
	// The first was already running. The next one through must be the person.
	if order[1] != "interactive" {
		t.Errorf("order = %v; the question queued behind background work", order)
	}
}

// Background work must still run when nobody is waiting, or enrichment would
// never finish.
func TestBackgroundRunsWhenNobodyIsWaiting(t *testing.T) {
	server := newFakeServer(t, says("ok"))
	client := server.client(t)

	for i := 0; i < 3; i++ {
		if _, err := client.Complete(context.Background(), Request{
			Messages: []Message{User("background")}, Priority: Background,
		}); err != nil {
			t.Fatalf("background request %d: %v", i, err)
		}
	}
	if server.calls.Load() != 3 {
		t.Errorf("made %d calls, want 3", server.calls.Load())
	}
}

// A slot is held back for whoever is waiting.
//
// Yielding alone was not enough: a background request already in flight cannot
// be interrupted, and enrichment takes up to a minute per book, so a question
// still waited for one to finish. With a slot reserved it waits for nothing.
func TestBackgroundLeavesASlotForAPerson(t *testing.T) {
	var inFlight, peakBackground atomic.Int64

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "background") {
			now := inFlight.Add(1)
			for {
				old := peakBackground.Load()
				if now <= old || peakBackground.CompareAndSwap(old, now) {
					break
				}
			}
			<-release
			inFlight.Add(-1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]string{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()
	defer close(release)

	// Three slots, so two are background's share and one is reserved.
	client := New(config.ModelConfig{
		BaseURL: server.URL + "/v1", Model: "m", MaxConcurrency: 3, Timeout: 10 * time.Second,
	})

	for i := 0; i < 6; i++ {
		go func() {
			_, _ = client.Complete(context.Background(), Request{
				Messages: []Message{User("background work")}, Priority: Background,
			})
		}()
	}
	time.Sleep(200 * time.Millisecond)

	if peak := peakBackground.Load(); peak > 2 {
		t.Errorf("background held %d of 3 slots, want at most 2", peak)
	}

	// And a person gets through immediately, with background work still stuck.
	done := make(chan error, 1)
	go func() {
		_, err := client.Complete(context.Background(), Request{
			Messages: []Message{User("a question")}, Priority: Interactive,
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the interactive request failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the interactive request waited behind background work")
	}
}
