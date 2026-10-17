// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
)

type checkerFunc func(context.Context) error

func (f checkerFunc) Ping(ctx context.Context) error { return f(ctx) }

// httpChecker pings a real server, so tests count requests that actually
// crossed the wire rather than calls to a fake.
type httpChecker struct{ url string }

func (c httpChecker) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable at %s: %w", c.url, err)
	}
	_ = resp.Body.Close()
	return nil
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const secretURL = "http://llama-embed.lexarii.svc:8080/v1"

var errModelDown = errors.New("unreachable at " + secretURL + ": connection refused")

func getHealth(t *testing.T, h *HealthHandler, signedIn bool) (int, string, dto.Health) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/health", nil)
	if signedIn {
		req = asUser(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var got dto.Health
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body, err)
	}
	return rec.Code, rec.Body.String(), got
}

func TestHealthRevealsDetailOnlyToAdministrators(t *testing.T) {
	newHandler := func(accounts AdminStore) *HealthHandler {
		return &HealthHandler{
			Version:  "1.2.3",
			Required: map[string]Checker{"postgres": checkerFunc(func(context.Context) error { return nil })},
			Optional: map[string]Checker{
				"embeddings": checkerFunc(func(context.Context) error { return errModelDown }),
				"llm":        checkerFunc(func(context.Context) error { return nil }),
			},
			Accounts: accounts,
		}
	}

	tests := []struct {
		name     string
		accounts AdminStore
		signedIn bool
		detail   bool
	}{
		{"anonymous", anAdmin, false, false},
		{"signed-in reader", aReader, true, false},
		{"administrator", anAdmin, true, true},
		{"no account store fails closed", nil, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, body, got := getHealth(t, newHandler(tc.accounts), tc.signedIn)

			// The status code semantics do not depend on who asks.
			if code != http.StatusOK || got.Status != "degraded" {
				t.Fatalf("got %d %q, want 200 degraded", code, got.Status)
			}
			if tc.detail {
				if got.Version != "1.2.3" || got.Checks["llm"] != "ok" ||
					!strings.Contains(got.Checks["embeddings"], secretURL) ||
					len(got.Degraded) != 1 || got.Degraded[0] != "embeddings" {
					t.Fatalf("admin detail incomplete: %s", body)
				}
				return
			}
			for _, leak := range []string{"http://", "svc", "refused", "embeddings", "llm", "postgres", "1.2.3", "unreachable"} {
				if strings.Contains(body, leak) {
					t.Errorf("anonymous body leaks %q: %s", leak, body)
				}
			}
			if got.Checks != nil || got.Degraded != nil || got.Version != "" {
				t.Errorf("anonymous response carries detail: %s", body)
			}
		})
	}
}

func TestHealthDatabaseDownIsReportedLive(t *testing.T) {
	var down atomic.Bool
	h := &HealthHandler{
		Required: map[string]Checker{"postgres": checkerFunc(func(context.Context) error {
			if down.Load() {
				return errors.New("connection refused to 10.0.0.5:5432")
			}
			return nil
		})},
		Optional: map[string]Checker{"llm": checkerFunc(func(context.Context) error { return nil })},
		Accounts: anAdmin,
	}

	if code, _, got := getHealth(t, h, false); code != http.StatusOK || got.Status != "ok" {
		t.Fatalf("healthy: got %d %q", code, got.Status)
	}

	// Flipped between two calls with no clock movement: the database check
	// must not be cached the way the model checks are.
	down.Store(true)
	code, body, got := getHealth(t, h, false)
	if code != http.StatusServiceUnavailable || got.Status != "down" {
		t.Fatalf("db down: got %d %q, want 503 down", code, got.Status)
	}
	if strings.Contains(body, "10.0.0.5") || strings.Contains(body, "postgres") {
		t.Errorf("anonymous body leaks the database error: %s", body)
	}

	_, _, admin := getHealth(t, h, true)
	if !strings.Contains(admin.Checks["postgres"], "10.0.0.5") {
		t.Errorf("admin should see the database error, got %v", admin.Checks)
	}
}

func TestHealthModelPingsAreCached(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clock := newFakeClock()
	h := &HealthHandler{
		Optional: map[string]Checker{"llm": httpChecker{url: srv.URL}},
		ModelTTL: 30 * time.Second,
		Now:      clock.Now,
	}

	for range 20 {
		getHealth(t, h, false)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("20 requests made %d model pings, want 1", got)
	}

	clock.Advance(29 * time.Second)
	getHealth(t, h, false)
	if got := hits.Load(); got != 1 {
		t.Fatalf("inside the TTL made %d pings, want 1", got)
	}

	clock.Advance(2 * time.Second)
	getHealth(t, h, false)
	if got := hits.Load(); got != 2 {
		t.Fatalf("after the TTL made %d pings, want 2", got)
	}
}

func TestHealthCachesFailuresToo(t *testing.T) {
	var calls atomic.Int32
	h := &HealthHandler{
		Optional: map[string]Checker{"llm": checkerFunc(func(context.Context) error {
			calls.Add(1)
			return errModelDown
		})},
		Now: newFakeClock().Now,
	}
	for range 10 {
		if _, _, got := getHealth(t, h, false); got.Status != "degraded" {
			t.Fatalf("status = %q, want degraded", got.Status)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("unreachable model pinged %d times, want 1", got)
	}
}

func TestHealthConcurrentRequestsShareOnePing(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{})
	release := make(chan struct{})
	h := &HealthHandler{
		Optional: map[string]Checker{"llm": checkerFunc(func(context.Context) error {
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-release
			return nil
		})},
		Now: newFakeClock().Now,
	}

	const n = 25
	var wg sync.WaitGroup
	statuses := make(chan string, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, got := getHealth(t, h, false)
			statuses <- got.Status
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	close(statuses)

	for s := range statuses {
		if s != "ok" {
			t.Errorf("status = %q, want ok", s)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d concurrent requests made %d pings, want 1", n, got)
	}
}

// A request that runs out of budget must neither hang nor poison the cache:
// the ping it abandoned finishes on its own and the next caller gets its answer.
func TestCachedCheckerOutlivesAnImpatientCaller(t *testing.T) {
	release := make(chan struct{})
	done := make(chan struct{})
	var calls atomic.Int32
	c := &cachedChecker{
		inner: checkerFunc(func(context.Context) error {
			calls.Add(1)
			defer close(done)
			<-release
			return nil
		}),
		ttl: time.Minute,
		now: newFakeClock().Now,
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Ping(ctx); !errors.Is(err, ErrHealthBudget) {
		t.Fatalf("err = %v, want ErrHealthBudget", err)
	}

	close(release)
	<-done
	// The result is stored after inner returns; a live Ping either joins the
	// flight or reads the cache. Neither may start a second ping.
	if err := c.Ping(t.Context()); err != nil {
		t.Fatalf("second ping err = %v, want nil", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("inner pinged %d times, want 1", got)
	}
}
