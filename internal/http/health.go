// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
)

// Checker reports whether a dependency is reachable.
type Checker interface {
	Ping(ctx context.Context) error
}

const (
	// defaultModelTTL is how long a model endpoint's answer is reused. Long
	// enough that a probe every few seconds, or a hostile loop, costs one
	// outbound request per endpoint per window; short enough that an operator
	// who has just restarted the model server sees it come back within the
	// minute.
	defaultModelTTL = 30 * time.Second

	// healthBudget bounds one request. A dead model endpoint must never make
	// the readiness probe slower than this.
	healthBudget = 2 * time.Second

	// modelPingBudget bounds the shared background ping. It is detached from
	// any one request (see cachedChecker.Ping) so it needs a limit of its own.
	modelPingBudget = 5 * time.Second
)

// ErrHealthBudget is what a cached check reports when its ping did not finish
// inside the request's budget. The ping carries on and fills the cache.
var ErrHealthBudget = errors.New("no answer within the health check budget")

// HealthHandler serves GET /api/v1/health.
//
// The endpoint is open: it is in RequireAuth's allowlist because load
// balancers and container probes carry no session. So the anonymous answer is
// deliberately minimal - an overall status and nothing about what is behind
// it - and the per-dependency detail, which names internal hosts and carries
// raw error text, is returned only when the caller holds an administrator
// session. A separate admin route was considered and rejected: it would make
// every operator tool learn two URLs, and the session the caller already has
// is what decides.
type HealthHandler struct {
	Version string
	// Required reports dependencies whose failure makes the app unhealthy.
	// Checked live on every request: it is a cheap local call and readiness
	// depends on it being current.
	Required map[string]Checker
	// Optional reports dependencies whose failure merely degrades the app.
	// The LLM lives here: with it unreachable, Lexarii is still a complete
	// library and reader. These are remote, may be a paid provider, and are
	// therefore cached and deduplicated; see cachedChecker.
	Optional map[string]Checker
	// Accounts decides who is an administrator. Nil means nobody is, which
	// fails closed: the detail stays hidden.
	Accounts AdminStore
	// ModelTTL overrides how long an Optional result is reused. Zero means
	// 30 seconds.
	ModelTTL time.Duration
	// Now is the clock, injectable so tests never sleep. Nil means time.Now.
	Now func() time.Time

	once   sync.Once
	cached map[string]*cachedChecker
}

func (h *HealthHandler) init() {
	h.once.Do(func() {
		ttl := h.ModelTTL
		if ttl <= 0 {
			ttl = defaultModelTTL
		}
		now := h.Now
		if now == nil {
			now = time.Now
		}
		h.cached = make(map[string]*cachedChecker, len(h.Optional))
		for name, c := range h.Optional {
			h.cached[name] = &cachedChecker{inner: c, ttl: ttl, now: now}
		}
	})
}

type healthResult struct {
	name     string
	err      error
	optional bool
}

func (h *HealthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.init()

	ctx, cancel := context.WithTimeout(r.Context(), healthBudget)
	defer cancel()

	// Every check runs concurrently, so the whole request costs the slowest
	// one rather than the sum.
	results := make(chan healthResult, len(h.Required)+len(h.cached))
	var wg sync.WaitGroup
	for name, c := range h.Required {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- healthResult{name: name, err: c.Ping(ctx)}
		}()
	}
	for name, c := range h.cached {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- healthResult{name: name, err: c.Ping(ctx), optional: true}
		}()
	}
	wg.Wait()
	close(results)

	status, code := "ok", http.StatusOK
	checks := make(map[string]string)
	var degraded []string
	for res := range results {
		switch {
		case res.err == nil:
			checks[res.name] = "ok"
		case res.optional:
			checks[res.name] = "unavailable: " + res.err.Error()
			degraded = append(degraded, res.name)
		default:
			checks[res.name] = "error: " + res.err.Error()
			status, code = "down", http.StatusServiceUnavailable
		}
	}
	if status == "ok" && len(degraded) > 0 {
		status = "degraded"
	}

	resp := dto.Health{Status: status}
	if h.callerIsAdmin(r) {
		slices.Sort(degraded)
		resp.Version = h.Version
		resp.Checks = checks
		resp.Degraded = degraded
	}
	respondJSON(w, code, resp)
}

// callerIsAdmin reports whether the request carries an administrator's
// session. Any failure to find out counts as no: this decides whether
// internals are shown, so it must fail closed, and it must not turn a lookup
// error into a response that tells an outsider anything.
func (h *HealthHandler) callerIsAdmin(r *http.Request) bool {
	if h.Accounts == nil {
		return false
	}
	id, ok := UserFrom(r.Context())
	if !ok {
		return false
	}
	user, err := h.Accounts.User(r.Context(), id)
	return err == nil && user.IsAdmin
}

// cachedChecker remembers the last answer from a remote dependency and shares
// one in-flight ping between concurrent callers.
//
// Failures are cached as well as successes: an unreachable endpoint is
// exactly the one that costs a connect timeout per probe.
type cachedChecker struct {
	inner Checker
	ttl   time.Duration
	now   func() time.Time

	mu      sync.Mutex
	err     error
	checked time.Time
	valid   bool

	flight singleflight.Group
}

// fresh returns the remembered answer when it is still within the TTL.
func (c *cachedChecker) fresh() (error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.now().Sub(c.checked) < c.ttl {
		return c.err, true
	}
	return nil, false
}

func (c *cachedChecker) Ping(ctx context.Context) error {
	if err, ok := c.fresh(); ok {
		return err
	}

	// The ping is detached from this request. If it ran under the first
	// caller's context, that caller going away - a probe with a short
	// deadline - would fail the ping for everyone sharing it, and the failure
	// would be cached as if the model were down. It keeps its own budget, and
	// its answer lands in the cache even if every waiter has given up.
	ch := c.flight.DoChan("ping", func() (any, error) {
		// A flight that finished between the check above and this one has
		// just filled the cache; pinging again would defeat the sharing.
		if err, ok := c.fresh(); ok {
			return nil, err
		}
		pingCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelPingBudget)
		defer cancel()
		err := c.inner.Ping(pingCtx)

		c.mu.Lock()
		c.err, c.checked, c.valid = err, c.now(), true
		c.mu.Unlock()
		return nil, err
	})

	select {
	case res := <-ch:
		return res.Err
	case <-ctx.Done():
		// Out of budget: report the dependency as not answering in time,
		// without touching the cache; the ping in flight will fill it.
		return ErrHealthBudget
	}
}
