// SPDX-License-Identifier: AGPL-3.0-or-later

// Package llm talks to the user's own model over an OpenAI-compatible HTTP API.
//
// This targets llama.cpp's server and must keep working unchanged against
// Ollama, vLLM, LM Studio, or a hosted API. No CGo, no vendor SDK, no GPU
// coupling - which is also what keeps CGO_ENABLED=0 builds possible.
//
// The model server is a box in the user's house and will be off half the time.
// Everything here assumes that: Lexarii must remain a complete library and
// reader with the model unreachable, and only enrichment and chat degrade.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
)

// Client is an OpenAI-compatible HTTP client.
type Client struct {
	baseURL string
	model   string
	apiKey  string
	http    *http.Client

	// extraBody, extraHeaders, and reasoning are the operator's provider
	// adjustments; see provider.go. Empty for llama.cpp.
	extraBody    map[string]json.RawMessage
	extraHeaders map[string]string
	reasoning    string

	// gate bounds how many requests are in flight.
	//
	// The model server is usually one machine with one GPU serving one request
	// at a time. Sending it fifty makes every one of them slower and none of
	// them finish sooner, and on a home setup it is the difference between a
	// library enriching in the background and a machine that stops responding.
	gate chan struct{}

	// waiting counts interactive requests queued for a slot.
	//
	// Background work yields while this is above zero. Without that, a question
	// typed into the chat box queues behind however many books happen to be
	// enriching - measured at over three minutes, on a model that answers in
	// twenty seconds. Enrichment can wait; a person watching a cursor cannot.
	waiting atomic.Int64

	// backgroundInFlight counts background requests currently running, which is
	// capped below the gate so a slot is always free for a person.
	//
	// Yielding alone was not enough. A background request already in flight
	// cannot be interrupted, and enrichment requests take up to a minute each,
	// so a question still waited for one to finish. Reserving a slot means it
	// never waits for background work at all.
	backgroundInFlight atomic.Int64
	// backgroundLimit is how many of the gate's slots background work may hold.
	backgroundLimit int64
}

// Priority says whether a caller is a person or a background job.
type Priority int

const (
	// Interactive is a request someone is waiting on.
	Interactive Priority = iota
	// Background is work that can yield: enrichment, embedding, indexing.
	Background
)

// New builds a client from a model config. It returns nil when the model is not
// configured, which callers must treat as "this feature is switched off" rather
// than as an error.
func New(cfg config.ModelConfig) *Client {
	if !cfg.Enabled() {
		return nil
	}
	concurrency := cfg.MaxConcurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	// One slot held back for whoever is waiting. With a single slot configured
	// there is nothing to reserve, and background work has to be allowed to use
	// it or nothing would ever be enriched.
	backgroundLimit := int64(concurrency)
	if concurrency > 1 {
		backgroundLimit = int64(concurrency - 1)
	}

	return &Client{
		backgroundLimit: backgroundLimit,
		baseURL:         strings.TrimRight(cfg.BaseURL, "/"),
		model:           cfg.Model,
		apiKey:          cfg.APIKey,
		extraBody:       cfg.ExtraBody,
		extraHeaders:    cfg.ExtraHeaders,
		reasoning:       cfg.ReasoningEffort,
		http: &http.Client{
			Timeout:       cfg.Timeout,
			CheckRedirect: dropExtraHeadersAcrossHosts(cfg.ExtraHeaders),
		},
		gate: make(chan struct{}, concurrency),
	}
}

// yieldPoll is how often background work re-checks whether a person is waiting.
//
// Polling rather than a condition variable, because a condition variable cannot
// be cancelled by a context without wrapping it in exactly this loop anyway.
// Model calls take seconds; a check every 25ms costs nothing next to that.
const yieldPoll = 25 * time.Millisecond

// acquireAs takes a slot in the concurrency gate, returning the release.
//
// Waiting here is correct: the alternative is failing work that would have
// succeeded a second later. A cancelled context still wins, so shutdown is not
// held up by a queue.
//
// Background callers yield to interactive ones, and are additionally capped
// below the gate so a slot is always free for someone who is waiting.
func (c *Client) acquireAs(ctx context.Context, priority Priority) (func(), error) {
	if c.gate == nil {
		return func() {}, nil
	}

	if priority == Interactive {
		// Counted before waiting, so background work sees the queue and steps
		// aside rather than taking the next slot that frees.
		c.waiting.Add(1)
		defer c.waiting.Add(-1)

		select {
		case c.gate <- struct{}{}:
			return func() { <-c.gate }, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	for {
		// Nobody queued ahead, and a share of the gate still available.
		//
		// The reservation is taken with a compare-and-swap BEFORE the gate slot.
		// Checking and then incrementing is not atomic, and under load several
		// goroutines passed the check together and took every slot - which the
		// test caught only when run repeatedly.
		if c.waiting.Load() == 0 {
			held := c.backgroundInFlight.Load()
			if held < c.backgroundLimit && c.backgroundInFlight.CompareAndSwap(held, held+1) {
				select {
				case c.gate <- struct{}{}:
					return func() {
						c.backgroundInFlight.Add(-1)
						<-c.gate
					}, nil
				case <-ctx.Done():
					c.backgroundInFlight.Add(-1)
					return nil, ctx.Err()
				default:
					// The gate is full even though this share was free. Give the
					// reservation back rather than holding it while waiting.
					c.backgroundInFlight.Add(-1)
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(yieldPoll):
		}
	}
}

// Model reports the configured model id.
func (c *Client) Model() string {
	if c == nil {
		return ""
	}
	return c.model
}

// Ping checks reachability against /v1/models. A nil client reports that the
// model is not configured, which is a legitimate state and not a failure.
func (c *Client) Ping(ctx context.Context) error {
	if c == nil {
		return ErrNotConfigured
	}

	// Deliberately short: health must not block for the full generation timeout.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	status, err := c.probe(ctx, "/models")
	if err != nil {
		return err
	}
	// Some servers do not implement /models but are otherwise fine; a 404 still
	// proves something is listening and speaking HTTP.
	if err := probeVerdict(c.baseURL, "/models", status); err != nil {
		return err
	}

	// A catalogue is often public even when inference is not - OpenRouter serves
	// /models to anyone, a bad key included - so a 200 there proves nothing about
	// the key. When there is one, ask an endpoint that needs it. OpenRouter
	// answers GET /key with 401 for a bad key; servers without the route answer
	// 404, which is not held against them.
	if c.apiKey != "" {
		status, err := c.probe(ctx, "/key")
		if err != nil {
			return err
		}
		// Only a 401 says the key is wrong. A gateway that answers 403 for a
		// route it does not know says nothing about the key, and must not
		// make a working client look broken.
		if status == http.StatusUnauthorized {
			return probeVerdict(c.baseURL, "/key", status)
		}
	}
	return nil
}

// probe does one authenticated GET and returns the status.
func (c *Client) probe(ctx context.Context, path string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("building request: %w", err)
	}
	c.authorise(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("unreachable at %s: %w", c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Drained (bounded) so the connection goes back to the pool and the next
	// probe does not pay for another handshake out of a two-second budget.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

// probeVerdict turns a health-check status into an error, or nil if healthy.
// A rejected key is unhealthy: the server is up but nothing will work. The real
// status is kept in the error.
func probeVerdict(baseURL, path string, status int) error {
	switch {
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%s%s: %w", baseURL, path, &HTTPError{Status: status})
	case status >= 500:
		return fmt.Errorf("%s returned %d", baseURL, status)
	}
	return nil
}

// ErrNotConfigured reports that no model server is configured. It is not a
// failure: the library and reader work without one.
var ErrNotConfigured = errNotConfigured{}

type errNotConfigured struct{}

func (errNotConfigured) Error() string { return "not configured" }
