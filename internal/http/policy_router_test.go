// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
)

// policyRouter is the real router with a policy, an auth handler and the
// session store behind it.
func policyRouter(t *testing.T, policy *policySpy, proxy *auth.ProxyProvider) (http.Handler, *AuthHandler, uuid.UUID) {
	t.Helper()
	h, _, userID := tokenHandler(t)
	rt := &Router{Auth: h, Sessions: h.Sessions, Proxy: proxy, Authorize: policy.authorize}
	return rt.Handler(), h, userID
}

// However the user is identified, through the real router, the policy is asked
// once about them - and a refusal is the answer the client gets.
func TestThePolicyIsAskedHoweverTheUserSignedIn(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, h *AuthHandler, userID uuid.UUID, r *http.Request)
	}{
		{"browser session sent as a bearer token", func(t *testing.T, h *AuthHandler, userID uuid.UUID, r *http.Request) {
			cookie, _, err := h.Sessions.Issue(t.Context(), r, userID.String(), "")
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			r.Header.Set("Authorization", "Bearer "+cookie.Value)
		}},
		{"device token as a bearer token", func(t *testing.T, h *AuthHandler, userID uuid.UUID, r *http.Request) {
			token, _, err := h.Sessions.IssueToken(t.Context(), r, userID.String(), "", "a phone")
			if err != nil {
				t.Fatalf("IssueToken: %v", err)
			}
			r.Header.Set("Authorization", "Bearer "+token)
		}},
		{"device token as a Basic password", func(t *testing.T, h *AuthHandler, userID uuid.UUID, r *http.Request) {
			token, _, err := h.Sessions.IssueToken(t.Context(), r, userID.String(), "", "a reader app")
			if err != nil {
				t.Fatalf("IssueToken: %v", err)
			}
			r.SetBasicAuth("adam", token)
		}},
		{"account password over Basic", func(_ *testing.T, _ *AuthHandler, _ uuid.UUID, r *http.Request) {
			r.SetBasicAuth("adam", "right-password")
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, refuse := range []bool{false, true} {
				spy := &policySpy{}
				if refuse {
					spy.err = &PolicyError{Status: http.StatusForbidden, Code: "suspended", Message: "suspended"}
				}
				router, h, userID := policyRouter(t, spy, nil)

				r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
				c.prepare(t, h, userID, r)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, r)

				if len(spy.calls) != 1 || spy.calls[0] != userID {
					t.Fatalf("refuse=%v: policy calls = %v, want exactly one about %s", refuse, spy.calls, userID)
				}
				want := http.StatusOK
				if refuse {
					want = http.StatusForbidden
				}
				if rec.Code != want {
					t.Errorf("refuse=%v: status = %d, want %d (body %s)", refuse, rec.Code, want, rec.Body)
				}
			}
		})
	}
}

// A user established by a trusted proxy's headers, on the very request that
// establishes them, is asked about like any other.
func TestThePolicyIsAskedAboutAProxyIdentifiedUser(t *testing.T) {
	proxy, err := auth.NewProxyProvider(auth.ProxyConfig{
		UserHeader:     "X-Forwarded-User",
		TrustedProxies: []string{"192.0.2.1"},
		AllowSignup:    true,
	})
	if err != nil {
		t.Fatalf("NewProxyProvider: %v", err)
	}
	spy := &policySpy{err: &PolicyError{Status: http.StatusServiceUnavailable, Code: "read_only"}}
	router, _, userID := policyRouter(t, spy, proxy)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	r.RemoteAddr = "192.0.2.1:4000"
	r.Header.Set("X-Forwarded-User", "adam")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, r)

	if len(spy.calls) != 1 || spy.calls[0] != userID {
		t.Fatalf("policy calls = %v, want exactly one about %s", spy.calls, userID)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the policy's 503", rec.Code)
	}
}

// DELETE /api/v1/auth/sessions/start ends in "/start" like a federated
// sign-in route and is not one. Signed in, it is a protected request: the
// policy is asked, and its refusal stands.
func TestASessionNamedLikeASignInRouteIsStillProtected(t *testing.T) {
	for _, path := range []string{"/api/v1/auth/sessions/start", "/api/v1/auth/sessions/callback"} {
		spy := &policySpy{err: &PolicyError{Status: http.StatusForbidden, Code: "suspended"}}
		router, h, userID := policyRouter(t, spy, nil)

		r := httptest.NewRequest(http.MethodDelete, path, nil)
		cookie, _, err := h.Sessions.Issue(t.Context(), r, userID.String(), "")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, r)

		if len(spy.calls) != 1 {
			t.Errorf("%s: policy calls = %d, want 1", path, len(spy.calls))
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want the policy's 403", path, rec.Code)
		}
	}
}
