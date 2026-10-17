// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// tokenProvider accepts one password, so the handler's own path is what is
// under test rather than a directory's.
type tokenProvider struct{}

func (tokenProvider) Name() string        { return auth.ProviderLocal }
func (tokenProvider) DisplayName() string { return "This Lexarii" }
func (tokenProvider) Kind() auth.Kind     { return auth.KindPassword }
func (tokenProvider) Authenticate(_ context.Context, username, password string) (auth.Identity, error) {
	if username != "adam" || password != "right-password" {
		return auth.Identity{}, auth.ErrInvalidCredentials
	}
	return auth.Identity{Provider: auth.ProviderLocal, Subject: "adam", DisplayName: "Adam"}, nil
}

// tokenStore is the slice of the store a sign-in touches.
type tokenStore struct {
	userID uuid.UUID
}

func (s *tokenStore) UpsertIdentity(context.Context, auth.Identity, bool) (uuid.UUID, uuid.UUID, error) {
	return s.userID, uuid.New(), nil
}

func (s *tokenStore) AdoptOrCreateLocalUser(context.Context, string, string, string, bool) (uuid.UUID, error) {
	return s.userID, nil
}
func (s *tokenStore) HasAnyUser(context.Context) (bool, error) { return true, nil }
func (s *tokenStore) User(context.Context, uuid.UUID) (postgres.UserRecord, error) {
	return postgres.UserRecord{ID: s.userID, Username: "adam", Email: "adam@example.com"}, nil
}
func (s *tokenStore) UserSessions(context.Context, string) ([]postgres.UserSession, error) {
	return nil, nil
}
func (s *tokenStore) RevokeSessionByID(context.Context, string, string) error { return nil }
func (s *tokenStore) RevokeAllSessions(context.Context, string) error         { return nil }

// sessionRecorder keeps what was issued, so a test can see the device name and
// replay the token.
type sessionRecorder struct {
	hashes  map[string]auth.Session
	devices map[string]string
}

func (s *sessionRecorder) CreateSession(_ context.Context, tokenHash, userID, identityID, _, _, device string, expiresAt time.Time) (auth.Session, error) {
	session := auth.Session{ID: tokenHash[:8], UserID: userID, IdentityID: identityID, ExpiresAt: expiresAt}
	s.hashes[tokenHash] = session
	s.devices[tokenHash] = device
	return session, nil
}

func (s *sessionRecorder) SessionByTokenHash(_ context.Context, tokenHash string) (auth.Session, error) {
	session, ok := s.hashes[tokenHash]
	if !ok {
		return auth.Session{}, auth.ErrSessionInvalid
	}
	return session, nil
}
func (s *sessionRecorder) RevokeSession(context.Context, string) error     { return nil }
func (s *sessionRecorder) RevokeAllSessions(context.Context, string) error { return nil }

func tokenHandler(t *testing.T) (*AuthHandler, *sessionRecorder, uuid.UUID) {
	t.Helper()
	userID := uuid.New()
	recorder := &sessionRecorder{hashes: map[string]auth.Session{}, devices: map[string]string{}}
	return &AuthHandler{
		Store:     &tokenStore{userID: userID},
		Providers: auth.NewRegistry(tokenProvider{}),
		Sessions:  &auth.Sessions{Store: recorder},
		Throttle:  auth.NewThrottle(),
	}, recorder, userID
}

func postJSON(t *testing.T, h http.HandlerFunc, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)))
	return rec
}

// The whole point: a client that is not a browser gets a token it can hold.
func TestATokenComesBackInTheBodyAndNotInACookie(t *testing.T) {
	h, recorder, _ := tokenHandler(t)

	rec := postJSON(t, h.Token, "/api/v1/auth/token", dto.TokenRequest{
		Username: "adam", Password: "right-password", Device: "Adam's phone",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	var out dto.AuthToken
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Token == "" {
		t.Fatal("no token, so the client has nothing to send")
	}
	if out.ExpiresAt.IsZero() {
		t.Error("no expiry, so a client cannot know when to sign in again")
	}
	if recorder.devices[auth.HashToken(out.Token)] != "Adam's phone" {
		t.Errorf("device = %q, want the name the client gave",
			recorder.devices[auth.HashToken(out.Token)])
	}

	// No cookie. A token endpoint that also set one would hand a browser a
	// credential in two places, one of them readable by script.
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("the token endpoint set %d cookies, want none", len(cookies))
	}
}

// A browser login must never put the token where a script can read it.
func TestABrowserLoginDoesNotRevealItsToken(t *testing.T) {
	h, _, _ := tokenHandler(t)

	rec := postJSON(t, h.Login, "/api/v1/auth/login", dto.LoginRequest{
		Username: "adam", Password: "right-password",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != auth.SessionCookie {
		t.Fatalf("cookies = %+v, want one session cookie", cookies)
	}
	if !cookies[0].HttpOnly {
		t.Error("the session cookie is readable by script")
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(cookies[0].Value)) {
		t.Error("the session token is in the response body of a browser login")
	}
}

// The device name is what makes a month-long token revocable a year later, so
// it is required rather than encouraged.
func TestATokenWithoutADeviceNameIsRefused(t *testing.T) {
	h, _, _ := tokenHandler(t)

	for _, device := range []string{"", "   "} {
		rec := postJSON(t, h.Token, "/api/v1/auth/token", dto.TokenRequest{
			Username: "adam", Password: "right-password", Device: device,
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("device %q: status = %d, want 400", device, rec.Code)
		}
	}
}

// The throttle protects both doors, or the one without it is the one that
// gets used.
func TestTheTokenEndpointIsThrottledToo(t *testing.T) {
	h, _, _ := tokenHandler(t)

	var last int
	for i := 0; i < 25; i++ {
		rec := postJSON(t, h.Token, "/api/v1/auth/token", dto.TokenRequest{
			Username: "adam", Password: "wrong", Device: "a phone",
		})
		last = rec.Code
		if last == http.StatusTooManyRequests {
			return
		}
	}
	t.Fatalf("25 wrong passwords in a row were never throttled; last status %d", last)
}

// The token endpoint has to be reachable WITHOUT a session, which is the one
// thing it exists for. It was not: RequireAuth's allowlist named the browser
// login and not this, so the only request this endpoint will ever receive was
// answered "sign in to continue".
func TestTheTokenEndpointIsReachableWithoutASession(t *testing.T) {
	var reached bool
	guarded := RequireAuth(&auth.Sessions{Store: &sessionRecorder{
		hashes: map[string]auth.Session{}, devices: map[string]string{},
	}}, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/token", nil))

	if !reached {
		t.Fatalf("the request never reached the handler; status %d, body %s", rec.Code, rec.Body)
	}
}

// And everything else still must not be.
func TestTheAllowlistDidNotWidenBeyondTheDoor(t *testing.T) {
	guarded := RequireAuth(&auth.Sessions{Store: &sessionRecorder{
		hashes: map[string]auth.Session{}, devices: map[string]string{},
	}}, nil, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unauthenticated request reached a protected handler")
	}))

	for _, path := range []string{
		"/api/v1/library", "/api/v1/annotations", "/api/v1/auth/me",
		"/api/v1/auth/sessions", "/api/v1/works/x/mentions",
	} {
		rec := httptest.NewRecorder()
		guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, rec.Code)
		}
	}
}
