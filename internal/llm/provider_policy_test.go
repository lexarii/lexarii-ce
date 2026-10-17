// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"scm.poniatowski.dev/adam/lexarii/internal/config"
)

// RouteQuestion is interactive and falls back to plain search when the model
// fails. Sleeping out a 25s Retry-After twice before falling back turned a
// 750ms give-up into ~50s of nothing happening.
func TestInteractiveRequestsDoNotSleepOnALongRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "25")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":429,"message":"slow down"}}`)
	}))
	defer srv.Close()

	start := time.Now()
	_, err := hostedClient(t, srv, nil).retry(t.Context(), Request{Messages: []Message{User("q")}, Priority: Interactive})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.RetryAfter != 25*time.Second {
		t.Fatalf("err = %v", err)
	}
	if calls.Load() != 1 || time.Since(start) > time.Second {
		t.Fatalf("%d calls in %v", calls.Load(), time.Since(start))
	}
	// Background work waits out the same header, up to its own limit.
	if got := backoff(1, httpErr, Background); got < 25*time.Second {
		t.Errorf("background backoff = %v, want at least the Retry-After", got)
	}
	// A short one is still honoured interactively.
	short := &HTTPError{Status: 429, RetryAfter: time.Second}
	if got := backoff(1, short, Interactive); got < time.Second {
		t.Errorf("interactive backoff = %v, want at least 1s", got)
	}
}

// A gateway that 403s routes it does not know says nothing about the key.
func TestPingIgnoresA403FromKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/key" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	if err := hostedClient(t, srv, nil).Ping(t.Context()); err != nil {
		t.Fatalf("Ping = %v; a 403 on /key must not read as a bad key", err)
	}
}

// The real status survives into the error.
func TestPingKeepsTheRealStatusOfARejectedKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer srv.Close()
	err := hostedClient(t, srv, nil).Ping(t.Context())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusUnauthorized {
		t.Fatalf("Ping = %v", err)
	}
}

// Both probes share one connection when the first body is drained.
func TestPingReusesTheConnection(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat(`{"data":[]}`, 500))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	if err := hostedClient(t, srv, nil).Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if conns.Load() != 1 {
		t.Fatalf("%d connections for two probes", conns.Load())
	}
}

// OpenRouter's own "reasoning" object and a reasoning_effort overlap; when the
// operator supplies the former the latter is not sent.
func TestReasoningInExtraBodySuppressesReasoningEffort(t *testing.T) {
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(chatOK("hi")))
	}))
	defer srv.Close()

	extra, err := config.ParseExtraBody(`{"reasoning":{"effort":"low"}}`)
	if err != nil {
		t.Fatal(err)
	}
	c := hostedClient(t, srv, func(cfg *config.ModelConfig) { cfg.ExtraBody = extra })
	if _, err := c.Complete(t.Context(), Request{Messages: []Message{User("q")}, ReasoningEffort: ReasoningOff}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["reasoning_effort"]; ok {
		t.Fatalf("reasoning_effort sent alongside reasoning: %v", got)
	}
	if string(got["reasoning"]) != `{"effort":"low"}` {
		t.Fatalf("reasoning = %s", got["reasoning"])
	}
}

// Extra headers can be credentials. A redirect off the configured host must not
// carry them along; one that stays on the host must.
func TestExtraHeadersAreDroppedOnACrossHostRedirect(t *testing.T) {
	var elsewhereGot http.Header
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhereGot = r.Header.Clone()
		_, _ = w.Write([]byte(chatOK("hi")))
	}))
	defer elsewhere.Close()

	var sameGot http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/chat/completions":
			http.Redirect(w, r, elsewhere.URL+"/x", http.StatusTemporaryRedirect)
		case "/api/v1/models":
			http.Redirect(w, r, "/api/v1/final", http.StatusTemporaryRedirect)
		case "/api/v1/final":
			sameGot = r.Header.Clone()
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer srv.Close()

	// Two httptest servers are the same IP on different ports: different Host.
	c := hostedClient(t, srv, func(cfg *config.ModelConfig) {
		cfg.APIKey = ""
		cfg.ExtraHeaders = map[string]string{"X-Gateway-Token": "secret"}
	})
	if _, err := c.Complete(t.Context(), Request{Messages: []Message{User("q")}}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if elsewhereGot == nil || elsewhereGot.Get("X-Gateway-Token") != "" {
		t.Fatalf("headers reached another host: %v", elsewhereGot)
	}

	if err := c.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if sameGot.Get("X-Gateway-Token") != "secret" {
		t.Fatalf("a same-host redirect lost the header: %v", sameGot)
	}
}
