// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// unwritableSessions is memorySessions with a database that refuses new rows.
type unwritableSessions struct {
	*memorySessions
	err error
}

func (u *unwritableSessions) CreateSession(context.Context, string, string, string, string, string, string, time.Time) (Session, error) {
	return Session{}, u.err
}

func TestIssueReportsAStoreFailure(t *testing.T) {
	storeDown := errors.New("database unavailable")
	s := &Sessions{Store: &unwritableSessions{memorySessions: newMemorySessions(), err: storeDown}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)

	cookie, _, err := s.Issue(t.Context(), r, "user-1", "identity-1")
	if !errors.Is(err, storeDown) || cookie != nil {
		t.Errorf("Issue = %v, %v; want no cookie and the store's error", cookie, err)
	}
	token, _, err := s.IssueToken(t.Context(), r, "user-1", "identity-1", "phone")
	if !errors.Is(err, storeDown) || token != "" {
		t.Errorf("IssueToken = %q, %v; want no token and the store's error", token, err)
	}
}

func TestIssueHonoursAConfiguredLifetime(t *testing.T) {
	s := &Sessions{Store: newMemorySessions(), Secure: true, Lifetime: 2 * time.Hour}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)

	before := time.Now()
	cookie, session, err := s.Issue(t.Context(), r, "user-1", "identity-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if cookie.MaxAge != int((2 * time.Hour).Seconds()) {
		t.Errorf("MaxAge = %d, want two hours", cookie.MaxAge)
	}
	if d := session.ExpiresAt.Sub(before); d < 2*time.Hour-time.Minute || d > 2*time.Hour+time.Minute {
		t.Errorf("expires in %v, want about two hours", d)
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Name != SessionCookie {
		t.Errorf("cookie = %+v, want HttpOnly, Secure, Lax", cookie)
	}
}

func TestACookieAloneCarriesTheSession(t *testing.T) {
	s := &Sessions{Store: newMemorySessions()}
	cookie, issued, err := s.Issue(t.Context(), httptest.NewRequest(http.MethodPost, "/", nil), "user-1", "identity-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.AddCookie(cookie)
	got, err := s.Validate(t.Context(), r)
	if err != nil || got.UserID != issued.UserID {
		t.Fatalf("Validate = %+v, %v; want the issued session", got, err)
	}
}

// A token that arrives as a password - an OPDS reader that knows only basic
// authentication - resolves to the same session.
func TestValidateToken(t *testing.T) {
	s := &Sessions{Store: newMemorySessions()}
	token, issued, err := s.IssueToken(t.Context(), httptest.NewRequest(http.MethodPost, "/", nil), "user-1", "identity-1", "e-reader")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	got, err := s.ValidateToken(t.Context(), "  "+token+"\n")
	if err != nil || got.UserID != issued.UserID {
		t.Errorf("ValidateToken = %+v, %v; want the issued session", got, err)
	}
	for _, bad := range []string{"", "   ", "not-a-token"} {
		if _, err := s.ValidateToken(t.Context(), bad); !errors.Is(err, ErrSessionInvalid) {
			t.Errorf("ValidateToken(%q) = %v, want ErrSessionInvalid", bad, err)
		}
	}
}

func TestRevokeWithoutATokenDoesNothing(t *testing.T) {
	store := newMemorySessions()
	s := &Sessions{Store: store}
	if err := s.Revoke(t.Context(), httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)); err != nil {
		t.Fatalf("Revoke = %v, want nil", err)
	}
	if store.lastCall != "" {
		t.Errorf("the store was asked to %s with no token", store.lastCall)
	}
}

func TestClearCookieRemovesTheSessionCookie(t *testing.T) {
	for _, secure := range []bool{true, false} {
		c := (&Sessions{Secure: secure}).ClearCookie()
		if c.Name != SessionCookie || c.Value != "" || c.MaxAge >= 0 || c.Path != "/" {
			t.Errorf("ClearCookie = %+v, want an expired empty session cookie at /", c)
		}
		// Overwriting a cookie needs the same attributes it was set with.
		if !c.HttpOnly || c.Secure != secure || c.SameSite != http.SameSiteLaxMode {
			t.Errorf("ClearCookie = %+v, want the attributes Issue sets", c)
		}
	}
}

func TestProxyProviderDescribesItself(t *testing.T) {
	p := proxyProvider(t)
	if p.Name() != "proxy" || p.DisplayName() == "" || p.Kind() != KindHeader {
		t.Errorf("name %q label %q kind %q", p.Name(), p.DisplayName(), p.Kind())
	}
	if p.AllowSignup() {
		t.Error("signup on first sign-in is on by default")
	}
	if p.Trusted() == nil || p.Trusted().Empty() || !p.Trusted().TrustsAddr("192.0.2.7:443") {
		t.Error("the trusted set handed to the throttle is not the configured one")
	}
}

// Blank entries are not addresses: a list of nothing but blanks is refused,
// exactly like an empty list.
func TestProxyRefusesAListOfBlanks(t *testing.T) {
	if _, err := NewProxyProvider(ProxyConfig{UserHeader: "X-User", TrustedProxies: []string{" ", ""}}); err == nil {
		t.Fatal("a proxy trusting nothing usable was accepted")
	}
}

func TestVerifyRejectsHashesWithUnreadableFields(t *testing.T) {
	for _, bad := range []string{
		"$argon2id$v=x$m=1,t=1,p=1$c2FsdA$aGFzaA",       // version is not a number
		"$argon2id$v=19$m=x,t=1,p=1$c2FsdA$aGFzaA",      // parameters are not numbers
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$!!!",     // key is not base64
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdA$aGFzaA$", // one field too many
	} {
		t.Run(bad, func(t *testing.T) {
			if VerifyPassword("anything", bad) {
				t.Fatalf("a malformed hash verified: %q", bad)
			}
		})
	}
}
