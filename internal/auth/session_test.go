// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// An IPv6 socket address is full of colons and arrives bracketed. Splitting on
// the last colon yielded "[::1]", which Postgres rejects as an inet - so every
// session created over a local IPv6 connection failed.
func TestClientIPHandlesIPv6(t *testing.T) {
	tests := map[string]string{
		"192.0.2.10:54321":  "192.0.2.10",
		"[::1]:54321":       "::1",
		"[2001:db8::1]:443": "2001:db8::1",
		"127.0.0.1:8080":    "127.0.0.1",
		// Anything unparseable stores nothing rather than something invalid.
		"not-an-address": "",
		"":               "",
	}

	for remote, want := range tests {
		t.Run(remote, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = remote
			if got := clientIP(r); got != want {
				t.Fatalf("clientIP(%q) = %q, want %q", remote, got, want)
			}
		})
	}
}

func TestHashTokenIsStableAndHidesTheToken(t *testing.T) {
	const token = "a-session-token"

	a := HashToken(token)
	if a != HashToken(token) {
		t.Fatal("hashing is not deterministic; sessions would never validate")
	}
	if a == token || len(a) != 64 {
		t.Fatalf("hash = %q, want a 64-character hex digest that is not the token", a)
	}
	if HashToken("a-session-tokem") == a {
		t.Fatal("a different token produced the same hash")
	}
}

// sweepRecorder counts purges and can fail on demand.
type sweepRecorder struct {
	mu       sync.Mutex
	calls    int
	lastKeep time.Duration
	err      error
	notify   chan struct{}
}

func (s *sweepRecorder) PurgeExpiredSessions(_ context.Context, olderThan time.Duration) (int64, error) {
	s.mu.Lock()
	s.calls++
	s.lastKeep = olderThan
	err := s.err
	notify := s.notify
	s.mu.Unlock()

	if notify != nil {
		select {
		case notify <- struct{}{}:
		default:
		}
	}
	if err != nil {
		return 0, err
	}
	return 1, nil
}

func (s *sweepRecorder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// An instance restarted more often than the interval would otherwise never
// sweep at all, so the first sweep must not wait for the first tick.
func TestSweepSessionsRunsImmediately(t *testing.T) {
	rec := &sweepRecorder{notify: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go SweepSessions(ctx, rec, time.Hour, 48*time.Hour, discardLogger())

	select {
	case <-rec.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("no sweep happened on startup")
	}
	if rec.lastKeep != 48*time.Hour {
		t.Errorf("retention = %v, want 48h", rec.lastKeep)
	}
}

func TestSweepSessionsStopsWithContext(t *testing.T) {
	rec := &sweepRecorder{notify: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		SweepSessions(ctx, rec, 10*time.Millisecond, time.Hour, discardLogger())
		close(done)
	}()

	<-rec.notify
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the sweeper ignored a cancelled context")
	}

	settled := rec.count()
	time.Sleep(50 * time.Millisecond)
	if rec.count() != settled {
		t.Error("the sweeper kept running after its context was cancelled")
	}
}

// Failing to tidy up must never take the server down, and must not stop later
// sweeps from being attempted.
func TestSweepSessionsSurvivesAFailure(t *testing.T) {
	rec := &sweepRecorder{err: errors.New("the database is having a moment"), notify: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go SweepSessions(ctx, rec, 10*time.Millisecond, time.Hour, discardLogger())

	<-rec.notify
	deadline := time.Now().Add(2 * time.Second)
	for rec.count() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the sweeper stopped after an error (%d calls)", rec.count())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Zero values are a misconfiguration, not a request to sweep constantly with no
// retention - which would delete every session the moment it expired.
func TestSweepSessionsDefaultsZeroValues(t *testing.T) {
	rec := &sweepRecorder{notify: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go SweepSessions(ctx, rec, 0, 0, nil)

	select {
	case <-rec.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("no sweep happened")
	}
	if rec.lastKeep != SweepRetention {
		t.Errorf("retention = %v, want the default %v", rec.lastKeep, SweepRetention)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// memorySessions is a SessionStore in a map, enough to exercise how a token
// is carried rather than how it is stored.
type memorySessions struct {
	mu       sync.Mutex
	byHash   map[string]Session
	devices  map[string]string
	revoked  map[string]bool
	lastCall string
}

func newMemorySessions() *memorySessions {
	return &memorySessions{
		byHash:  map[string]Session{},
		devices: map[string]string{},
		revoked: map[string]bool{},
	}
}

func (m *memorySessions) CreateSession(_ context.Context, tokenHash, userID, identityID, _, _, device string, expiresAt time.Time) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Session{ID: tokenHash[:8], UserID: userID, IdentityID: identityID, ExpiresAt: expiresAt}
	m.byHash[tokenHash] = s
	m.devices[tokenHash] = device
	return s, nil
}

func (m *memorySessions) SessionByTokenHash(_ context.Context, tokenHash string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byHash[tokenHash]
	if !ok || m.revoked[tokenHash] {
		return Session{}, ErrSessionInvalid
	}
	return s, nil
}

func (m *memorySessions) RevokeSession(_ context.Context, tokenHash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastCall = "revoke"
	m.revoked[tokenHash] = true
	return nil
}

func (m *memorySessions) RevokeAllSessions(context.Context, string) error { return nil }

// The point of the whole change: a client that holds its own token can use it,
// and it is the same session the cookie mechanism makes.
func TestABearerTokenIsAcceptedLikeACookie(t *testing.T) {
	store := newMemorySessions()
	s := &Sessions{Store: store}

	token, issued, err := s.IssueToken(context.Background(),
		httptest.NewRequest("POST", "/api/v1/auth/token", nil),
		"user-1", "identity-1", "Adam's phone")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if token == "" {
		t.Fatal("no token was returned, so a native client has nothing to hold")
	}
	if store.devices[HashToken(token)] != "Adam's phone" {
		t.Errorf("device = %q, want the name the client gave", store.devices[HashToken(token)])
	}

	r := httptest.NewRequest("GET", "/api/v1/library", nil)
	r.Header.Set("Authorization", "Bearer "+token)

	got, err := s.Validate(context.Background(), r)
	if err != nil {
		t.Fatalf("Validate with a bearer token: %v", err)
	}
	if got.ID != issued.ID {
		t.Errorf("validated session %s, want %s", got.ID, issued.ID)
	}
}

// RFC 7235 says the scheme is case-insensitive, and clients get it wrong in
// both directions.
func TestTheBearerSchemeIsCaseInsensitive(t *testing.T) {
	store := newMemorySessions()
	s := &Sessions{Store: store}
	token, _, err := s.IssueToken(context.Background(),
		httptest.NewRequest("POST", "/", nil), "user-1", "", "a phone")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	for _, scheme := range []string{"Bearer ", "bearer ", "BEARER "} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", scheme+token)
		if _, err := s.Validate(context.Background(), r); err != nil {
			t.Errorf("%q was rejected: %v", scheme, err)
		}
	}
}

// A header is a deliberate act by the caller; a cookie is attached by the
// browser whether or not anybody meant it. When both arrive, the deliberate
// one is the one that counts.
func TestAnExplicitTokenBeatsTheCookie(t *testing.T) {
	store := newMemorySessions()
	s := &Sessions{Store: store}
	ctx := context.Background()

	cookie, browser, err := s.Issue(ctx, httptest.NewRequest("POST", "/", nil), "user-1", "")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	token, device, err := s.IssueToken(ctx, httptest.NewRequest("POST", "/", nil), "user-2", "", "a phone")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(cookie)
	r.Header.Set("Authorization", "Bearer "+token)

	got, err := s.Validate(ctx, r)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.ID == browser.ID {
		t.Fatal("the cookie won; a client sending a token must get that token's session")
	}
	if got.ID != device.ID {
		t.Errorf("validated %s, want the bearer token's session %s", got.ID, device.ID)
	}
}

// Signing out has to end the session that was actually used, or a native
// client's logout button would leave its token live.
func TestRevokeEndsTheBearerSession(t *testing.T) {
	store := newMemorySessions()
	s := &Sessions{Store: store}
	ctx := context.Background()

	token, _, err := s.IssueToken(ctx, httptest.NewRequest("POST", "/", nil), "user-1", "", "a phone")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	r := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if err := s.Revoke(ctx, r); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	check := httptest.NewRequest("GET", "/", nil)
	check.Header.Set("Authorization", "Bearer "+token)
	if _, err := s.Validate(ctx, check); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("the token still works after logout: %v", err)
	}
}

// A request carrying nothing must not be mistaken for one carrying an empty
// token, which would hash to a fixed value.
func TestNoCredentialIsNotASession(t *testing.T) {
	s := &Sessions{Store: newMemorySessions()}
	for _, header := range []string{"", "Bearer", "Bearer ", "Basic abc123"} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if _, err := s.Validate(context.Background(), r); !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("Authorization %q was accepted: %v", header, err)
		}
	}
}
