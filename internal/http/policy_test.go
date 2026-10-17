// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
)

// policySpy records every call the gate makes to the access policy.
type policySpy struct {
	calls []uuid.UUID
	err   error
}

func (p *policySpy) authorize(_ context.Context, _ *http.Request, userID uuid.UUID) error {
	p.calls = append(p.calls, userID)
	return p.err
}

// countingBasic is a Basic checker that counts how often it is asked.
type countingBasic struct {
	basicChecker
	calls int
}

func (c *countingBasic) AuthenticateBasic(ctx context.Context, r *http.Request, username, password string) (uuid.UUID, error) {
	c.calls++
	return c.basicChecker.AuthenticateBasic(ctx, r, username, password)
}

// sessionsWith returns sessions holding one valid cookie session for userID,
// and that cookie.
func sessionsWith(t *testing.T, userID uuid.UUID) (*auth.Sessions, *http.Cookie) {
	t.Helper()
	sessions := &auth.Sessions{Store: &sessionRecorder{
		hashes: map[string]auth.Session{}, devices: map[string]string{},
	}}
	cookie, _, err := sessions.Issue(t.Context(), httptest.NewRequest(http.MethodGet, "/", nil), userID.String(), "")
	if err != nil {
		t.Fatalf("issuing a session: %v", err)
	}
	return sessions, cookie
}

// The policy is about protected requests. An open route - the sign-in page,
// the health check - is open to everybody, signed in or not, and a policy
// that could close it could lock every person out of the sign-in page.
func TestThePolicyIsNotAskedAboutOpenRoutes(t *testing.T) {
	userID := uuid.New()
	sessions, cookie := sessionsWith(t, userID)
	spy := &policySpy{err: &PolicyError{Status: http.StatusForbidden, Code: "suspended"}}

	var reached int
	guarded := RequireAuthWithPolicy(sessions, nil, spy.authorize,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ }))

	for _, path := range []string{"/", "/signin", "/api/v1/health", "/api/v1/auth/providers", "/api/v1/auth/oidc/start"} {
		for _, signedIn := range []bool{false, true} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			if signedIn {
				r.AddCookie(cookie)
			}
			rec := httptest.NewRecorder()
			guarded.ServeHTTP(rec, r)
			if rec.Code != http.StatusOK {
				t.Errorf("%s (signed in %v): status = %d, want the open route served", path, signedIn, rec.Code)
			}
		}
	}
	if len(spy.calls) != 0 {
		t.Errorf("the policy was asked %d times about open routes, want never", len(spy.calls))
	}
	if reached != 10 {
		t.Errorf("reached the handler %d times, want 10", reached)
	}
}

// Once per request, with the identified user, whichever way they were
// identified - and never for a request nobody was identified on.
func TestThePolicyIsAskedOncePerProtectedRequest(t *testing.T) {
	userID := uuid.New()
	sessions, cookie := sessionsWith(t, userID)

	cases := []struct {
		name    string
		prepare func(*http.Request) *http.Request
		want    int
	}{
		{"cookie session", func(r *http.Request) *http.Request { r.AddCookie(cookie); return r }, 1},
		{"basic credentials", func(r *http.Request) *http.Request { r.SetBasicAuth("adam", "a-device-token"); return r }, 1},
		{"identified upstream", withUserID(userID), 1},
		{"nobody", func(r *http.Request) *http.Request { return r }, 0},
		{"wrong basic credentials", func(r *http.Request) *http.Request { r.SetBasicAuth("adam", "wrong"); return r }, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spy := &policySpy{}
			var seen uuid.UUID
			guarded := RequireAuthWithPolicy(sessions,
				basicChecker{userID: userID, user: "adam", pass: "a-device-token"}, spy.authorize,
				http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen, _ = UserFrom(r.Context()) }))

			guarded.ServeHTTP(httptest.NewRecorder(),
				c.prepare(httptest.NewRequest(http.MethodGet, "/api/v1/library", nil)))

			if len(spy.calls) != c.want {
				t.Fatalf("the policy was asked %d times, want %d", len(spy.calls), c.want)
			}
			if c.want == 1 {
				if spy.calls[0] != userID {
					t.Errorf("the policy was asked about %s, want %s", spy.calls[0], userID)
				}
				if seen != userID {
					t.Errorf("the handler saw user %s, want %s", seen, userID)
				}
			}
		})
	}
}

func withUserID(id uuid.UUID) func(*http.Request) *http.Request {
	return func(r *http.Request) *http.Request {
		return r.WithContext(context.WithValue(r.Context(), userContextKey{}, id))
	}
}

// A refusal carries the policy's own status and code in the standard error
// shape, and the handler behind it never runs. Anything that is not a
// PolicyError is a policy that could not be evaluated, which refuses too.
func TestAPolicyRefusalIsWrittenInTheStandardShape(t *testing.T) {
	userID := uuid.New()
	sessions, cookie := sessionsWith(t, userID)

	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"maintenance", &PolicyError{Status: http.StatusServiceUnavailable, Code: "read_only", Message: "the library is read-only until 18:00"},
			http.StatusServiceUnavailable, "read_only"},
		{"suspended", &PolicyError{Status: http.StatusForbidden, Code: "account_suspended", Message: "this account is suspended"},
			http.StatusForbidden, "account_suspended"},
		{"wrapped", fmt.Errorf("checking the window: %w", &PolicyError{Status: http.StatusLocked, Code: "locked"}),
			http.StatusLocked, "locked"},
		{"no status", &PolicyError{Code: "nope"}, http.StatusForbidden, "nope"},
		{"a success status", &PolicyError{Status: http.StatusOK}, http.StatusForbidden, "forbidden"},
		{"broken policy", errors.New("the policy database is down"), http.StatusInternalServerError, string(CodeInternal)},
		{"typed nil", typedNilPolicyError(), http.StatusInternalServerError, string(CodeInternal)},
		{"wrapped typed nil", fmt.Errorf("deciding: %w", typedNilPolicyError()), http.StatusInternalServerError, string(CodeInternal)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			guarded := RequireAuthWithPolicy(sessions, nil, (&policySpy{err: c.err}).authorize,
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
					t.Error("a refused request reached the handler")
				}))

			r := httptest.NewRequest(http.MethodPost, "/api/v1/collections", nil)
			r.AddCookie(cookie)
			rec := httptest.NewRecorder()
			guarded.ServeHTTP(rec, r)

			if rec.Code != c.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			var body dto.Error
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("the refusal is not the standard error shape: %v: %s", err, rec.Body)
			}
			if body.Error.Code != c.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, c.wantCode)
			}
			if body.Error.Message == "" {
				t.Error("a refusal with no message")
			}
		})
	}
}

// Without a policy nothing changes: the router behaves as it always did.
func TestNoPolicyMeansNoChange(t *testing.T) {
	userID := uuid.New()
	sessions, cookie := sessionsWith(t, userID)

	var reached bool
	guarded := RequireAuthWithPolicy(sessions, nil, nil,
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/library", nil)
	r.AddCookie(cookie)
	guarded.ServeHTTP(httptest.NewRecorder(), r)
	if !reached {
		t.Error("a signed-in request was refused with no policy configured")
	}

	rec := httptest.NewRecorder()
	guarded.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/library", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous request got %d, want 401", rec.Code)
	}
}

// The policy sits after authentication and must not disturb it: Basic
// credentials are checked once per request, and a wrong password counts once
// against the throttle - not again because a policy is configured.
func TestBasicFailuresAreCountedOnceWithAPolicy(t *testing.T) {
	checker := &countingBasic{basicChecker: basicChecker{userID: uuid.New(), user: "adam", pass: "right"}}
	spy := &policySpy{err: &PolicyError{Status: http.StatusForbidden, Code: "suspended"}}
	guarded := RequireAuthWithPolicy(&auth.Sessions{Store: &sessionRecorder{
		hashes: map[string]auth.Session{}, devices: map[string]string{},
	}}, checker, spy.authorize, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	for _, password := range []string{"wrong", "right"} {
		checker.calls = 0
		r := httptest.NewRequest(http.MethodGet, "/opds", nil)
		r.SetBasicAuth("adam", password)
		guarded.ServeHTTP(httptest.NewRecorder(), r)
		if checker.calls != 1 {
			t.Errorf("password %q: Basic credentials were checked %d times, want 1", password, checker.calls)
		}
	}

	// Through the real sign-in check and its throttle. With a limit of three,
	// two wrong passwords must leave the right one working; were each failure
	// counted twice, the second would already have locked the account.
	for _, wrong := range []int{2, 3} {
		h, _, _ := tokenHandler(t)
		h.Throttle = auth.NewThrottle()
		h.Throttle.MaxAttempts = 3
		h.Throttle.Lockout = time.Hour
		guarded := RequireAuthWithPolicy(h.Sessions, h, (&policySpy{}).authorize,
			http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

		send := func(password string) int {
			r := httptest.NewRequest(http.MethodGet, "/opds", nil)
			r.SetBasicAuth("adam", password)
			rec := httptest.NewRecorder()
			guarded.ServeHTTP(rec, r)
			return rec.Code
		}
		for i := 0; i < wrong; i++ {
			if got := send("wrong-password"); got != http.StatusUnauthorized {
				t.Fatalf("wrong password %d: status = %d, want 401", i+1, got)
			}
		}
		got := send("right-password")
		if wrong < 3 && got != http.StatusOK {
			t.Errorf("after %d failures under a limit of 3, the right password got %d, want 200", wrong, got)
		}
		if wrong == 3 && got != http.StatusUnauthorized {
			t.Errorf("after %d failures under a limit of 3, the right password got %d, want the lockout", wrong, got)
		}
	}
}

// typedNilPolicyError is the classic mistake: a nil pointer returned through
// the error interface, which is not == nil.
func typedNilPolicyError() error {
	var pe *PolicyError
	return pe
}
