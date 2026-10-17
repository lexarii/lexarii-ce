// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// errAuthStoreDown stands for the database failing underneath a sign-in.
var errAuthStoreDown = errors.New("auth store unavailable")

// authFakeStore is an AuthStore whose answers a test sets and whose calls it
// reads back.
type authFakeStore struct {
	mu sync.Mutex

	userID     uuid.UUID
	identityID uuid.UUID
	admin      bool

	hasUser      bool
	hasUserErr   error
	upsertErr    error
	userErr      error
	adoptErr     error
	sessions     []postgres.UserSession
	sessionsErr  error
	revokeErr    error
	revokeAllErr error

	upserted      []auth.Identity
	allowCreate   []bool
	adopted       []dto.SetupRequest
	adoptedAdmin  bool
	revoked       [][2]string // user id, session id
	revokedAllFor []string
}

func (s *authFakeStore) UpsertIdentity(_ context.Context, identity auth.Identity, allowCreate bool) (uuid.UUID, uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserted = append(s.upserted, identity)
	s.allowCreate = append(s.allowCreate, allowCreate)
	if s.upsertErr != nil {
		return uuid.Nil, uuid.Nil, s.upsertErr
	}
	return s.userID, s.identityID, nil
}

func (s *authFakeStore) AdoptOrCreateLocalUser(_ context.Context, username, email, password string, admin bool) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adopted = append(s.adopted, dto.SetupRequest{Username: username, Email: email, Password: password})
	s.adoptedAdmin = admin
	if s.adoptErr != nil {
		return uuid.Nil, s.adoptErr
	}
	return s.userID, nil
}

func (s *authFakeStore) HasAnyUser(context.Context) (bool, error) { return s.hasUser, s.hasUserErr }

func (s *authFakeStore) User(_ context.Context, id uuid.UUID) (postgres.UserRecord, error) {
	if s.userErr != nil {
		return postgres.UserRecord{}, s.userErr
	}
	return postgres.UserRecord{ID: id, Username: "adam", Email: "adam@example.com", IsAdmin: s.admin}, nil
}

func (s *authFakeStore) UserSessions(context.Context, string) ([]postgres.UserSession, error) {
	return s.sessions, s.sessionsErr
}

func (s *authFakeStore) RevokeSessionByID(_ context.Context, userID, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = append(s.revoked, [2]string{userID, sessionID})
	return s.revokeErr
}

func (s *authFakeStore) RevokeAllSessions(_ context.Context, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokedAllFor = append(s.revokedAllFor, userID)
	return s.revokeAllErr
}

// authSessionLedger is the session table without a database: it remembers
// every session by token hash and every revocation.
type authSessionLedger struct {
	mu        sync.Mutex
	byHash    map[string]auth.Session
	devices   map[string]string
	revoked   []string
	createErr error
	revokeErr error
}

func newAuthSessionLedger() *authSessionLedger {
	return &authSessionLedger{byHash: map[string]auth.Session{}, devices: map[string]string{}}
}

func (l *authSessionLedger) CreateSession(_ context.Context, tokenHash, userID, identityID, _, _, device string, expiresAt time.Time) (auth.Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.createErr != nil {
		return auth.Session{}, l.createErr
	}
	s := auth.Session{ID: uuid.NewString(), UserID: userID, IdentityID: identityID, ExpiresAt: expiresAt}
	l.byHash[tokenHash] = s
	l.devices[tokenHash] = device
	return s, nil
}

func (l *authSessionLedger) SessionByTokenHash(_ context.Context, tokenHash string) (auth.Session, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.byHash[tokenHash]
	if !ok {
		return auth.Session{}, auth.ErrSessionInvalid
	}
	return s, nil
}

func (l *authSessionLedger) RevokeSession(_ context.Context, tokenHash string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.revoked = append(l.revoked, tokenHash)
	delete(l.byHash, tokenHash)
	return l.revokeErr
}

func (l *authSessionLedger) RevokeAllSessions(context.Context, string) error { return nil }

// seed records a session for a token of the test's choosing, so a request can
// carry it as a cookie or a bearer header.
func (l *authSessionLedger) seed(userID string) (token string, session auth.Session) {
	l.mu.Lock()
	defer l.mu.Unlock()
	token = "token-" + uuid.NewString()
	session = auth.Session{ID: uuid.NewString(), UserID: userID, ExpiresAt: time.Now().Add(time.Hour)}
	l.byHash[auth.HashToken(token)] = session
	return token, session
}

// authPasswordFake accepts one username and password, under whatever provider
// name the test gives it: local, or a directory such as LDAP.
type authPasswordFake struct {
	name     string
	username string
	password string
}

func (p authPasswordFake) Name() string        { return p.name }
func (p authPasswordFake) DisplayName() string { return "Provider " + p.name }
func (p authPasswordFake) Kind() auth.Kind     { return auth.KindPassword }
func (p authPasswordFake) Authenticate(_ context.Context, username, password string) (auth.Identity, error) {
	if username != p.username || password != p.password {
		return auth.Identity{}, auth.ErrInvalidCredentials
	}
	return auth.Identity{Provider: p.name, Subject: username}, nil
}

// authRedirectFake is an identity provider reached by redirect, as OIDC is.
type authRedirectFake struct {
	name        string
	signup      bool
	exchangeErr error

	mu        sync.Mutex
	exchanged []string // code, nonce, then redirect URI
}

func (p *authRedirectFake) Name() string        { return p.name }
func (p *authRedirectFake) DisplayName() string { return "Sign in with " + p.name }
func (p *authRedirectFake) Kind() auth.Kind     { return auth.KindRedirect }
func (p *authRedirectFake) AllowSignup() bool   { return p.signup }
func (p *authRedirectFake) AuthURL(state, nonce, redirectURI string) string {
	return "https://idp.example/authorize?state=" + url.QueryEscape(state) +
		"&nonce=" + url.QueryEscape(nonce) +
		"&redirect_uri=" + url.QueryEscape(redirectURI)
}
func (p *authRedirectFake) Exchange(_ context.Context, code, nonce, redirectURI string) (auth.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exchanged = append(p.exchanged, code, nonce, redirectURI)
	if p.exchangeErr != nil {
		return auth.Identity{}, p.exchangeErr
	}
	return auth.Identity{Provider: p.name, Subject: "subject-" + code}, nil
}

// authHeaderOnly is a provider that is neither a password nor a redirect,
// as an authenticating proxy is.
type authHeaderOnly struct{}

func (authHeaderOnly) Name() string        { return "proxy" }
func (authHeaderOnly) DisplayName() string { return "Single sign-on" }
func (authHeaderOnly) Kind() auth.Kind     { return auth.KindHeader }

// authFixture is an AuthHandler wired to fakes, with the fakes kept to hand.
type authFixture struct {
	h      *AuthHandler
	store  *authFakeStore
	ledger *authSessionLedger
	oidc   *authRedirectFake
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	st := &authFakeStore{userID: uuid.New(), identityID: uuid.New(), hasUser: true}
	ledger := newAuthSessionLedger()
	oidc := &authRedirectFake{name: "oidc", signup: true}
	return &authFixture{
		h: &AuthHandler{
			Store: st,
			Providers: auth.NewRegistry(
				authPasswordFake{name: auth.ProviderLocal, username: "adam", password: "right-password"},
				authPasswordFake{name: "ldap", username: "adam", password: "directory-password"},
				oidc,
				authHeaderOnly{},
			),
			Sessions: &auth.Sessions{Store: ledger, Secure: true},
			Throttle: auth.NewThrottle(),
			Log:      quietLogger(),
		},
		store: st, ledger: ledger, oidc: oidc,
	}
}

// authErrorCode reads the stable code out of the standard error shape.
func authErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e dto.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("not the standard error shape: %v; body = %s", err, rec.Body)
	}
	return e.Error.Code
}

func authCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func authPost(h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

// The session cookie is the credential a browser holds, so every attribute on
// it is a security decision: script cannot read it, it is sent only over
// HTTPS when the installation says so, it survives arriving from a link, and
// it expires with the session.
func TestTheSessionCookieCarriesItsSecurityAttributes(t *testing.T) {
	f := newAuthFixture(t)

	rec := authPost(f.h.Login, "/api/v1/auth/login", `{"username":"adam","password":"right-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	c := authCookie(rec, auth.SessionCookie)
	if c == nil {
		t.Fatal("no session cookie was set")
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("cookie = HttpOnly:%v Secure:%v SameSite:%v Path:%q; want HttpOnly, Secure, Lax, /",
			c.HttpOnly, c.Secure, c.SameSite, c.Path)
	}
	if c.MaxAge <= 0 {
		t.Errorf("MaxAge = %d, want the session's lifetime", c.MaxAge)
	}
	// The cookie is the session the ledger issued, and only that.
	if _, ok := f.ledger.byHash[auth.HashToken(c.Value)]; !ok {
		t.Error("the cookie does not name a session the store issued")
	}

	var user dto.User
	if err := json.Unmarshal(rec.Body.Bytes(), &user); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if user.ID != f.store.userID.String() || user.Username != "adam" {
		t.Errorf("user = %+v, want the account that signed in", user)
	}
	// A local password never creates an account: one only exists if somebody
	// made it.
	if len(f.store.allowCreate) != 1 || f.store.allowCreate[0] {
		t.Errorf("allowCreate = %v, want a single false for a local sign-in", f.store.allowCreate)
	}
}

// Every way a sign-in can be refused, with the status and code a client
// branches on.
func TestSignInRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		setup  func(*authFixture)
		status int
		code   string
	}{
		{"malformed JSON", `{"username":`, nil, http.StatusBadRequest, string(CodeInvalid)},
		{"unknown provider", `{"provider":"kerberos","username":"adam","password":"x"}`, nil,
			http.StatusBadRequest, string(CodeInvalid)},
		{"a redirect provider takes no password", `{"provider":"oidc","username":"adam","password":"x"}`, nil,
			http.StatusBadRequest, string(CodeInvalid)},
		{"wrong password", `{"username":"adam","password":"wrong"}`, nil,
			http.StatusUnauthorized, "invalid_credentials"},
		{"unknown username", `{"username":"eve","password":"right-password"}`, nil,
			http.StatusUnauthorized, "invalid_credentials"},
		{"authenticated with no account here", `{"username":"adam","password":"right-password"}`,
			func(f *authFixture) { f.store.upsertErr = auth.ErrInvalidCredentials },
			http.StatusForbidden, "no_account"},
		{"the account store fails", `{"username":"adam","password":"right-password"}`,
			func(f *authFixture) { f.store.upsertErr = errAuthStoreDown },
			http.StatusInternalServerError, string(CodeInternal)},
		{"the session cannot be recorded", `{"username":"adam","password":"right-password"}`,
			func(f *authFixture) { f.ledger.createErr = errAuthStoreDown },
			http.StatusInternalServerError, string(CodeInternal)},
		{"the account cannot be read back", `{"username":"adam","password":"right-password"}`,
			func(f *authFixture) { f.store.userErr = errAuthStoreDown },
			http.StatusInternalServerError, string(CodeInternal)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			rec := authPost(f.h.Login, "/api/v1/auth/login", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if got := authErrorCode(t, rec); got != tc.code {
				t.Errorf("code = %q, want %q", got, tc.code)
			}
			// Only a failure after the session was issued can have set one.
			if c := authCookie(rec, auth.SessionCookie); c != nil && tc.name != "the account cannot be read back" {
				t.Errorf("a refused sign-in set a session cookie")
			}
		})
	}
}

// A directory may create the account on first sign-in; this installation's
// own password list never does.
func TestADirectorySignInMayCreateAnAccount(t *testing.T) {
	f := newAuthFixture(t)

	rec := authPost(f.h.Login, "/api/v1/auth/login",
		`{"provider":"ldap","username":"adam","password":"directory-password"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(f.store.allowCreate) != 1 || !f.store.allowCreate[0] {
		t.Errorf("allowCreate = %v, want true for a directory", f.store.allowCreate)
	}
	if f.store.upserted[0].Provider != "ldap" {
		t.Errorf("identity provider = %q, want ldap", f.store.upserted[0].Provider)
	}
}

// Too many wrong passwords from one address are refused before the password
// is even checked, and the refusal says when to come back.
func TestRepeatedFailuresAreThrottledWithRetryAfter(t *testing.T) {
	f := newAuthFixture(t)
	f.h.Throttle.MaxAttempts = 2

	for i := 0; i < 2; i++ {
		if rec := authPost(f.h.Login, "/x", `{"username":"adam","password":"wrong"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", i, rec.Code)
		}
	}

	// Now even the right password waits: the address is locked out.
	rec := authPost(f.h.Login, "/x", `{"username":"adam","password":"right-password"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body = %s", rec.Code, rec.Body)
	}
	if code := authErrorCode(t, rec); code != "too_many_attempts" {
		t.Errorf("code = %q, want too_many_attempts", code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After, so a client cannot know when to try again")
	}
	if len(f.store.upserted) != 0 {
		t.Errorf("a throttled attempt reached the account store %d times", len(f.store.upserted))
	}
}

// authLogRecorder keeps every record logged, so a test can see a warning was
// raised.
type authLogRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (l *authLogRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *authLogRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, r)
	return nil
}
func (l *authLogRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *authLogRecorder) WithGroup(string) slog.Handler      { return l }

func (l *authLogRecorder) warnedAbout(key, value string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.records {
		if r.Level != slog.LevelWarn {
			continue
		}
		found := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == key && a.Value.String() == value {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// Failures against one account from several addresses are what a distributed
// attack looks like. It is said out loud, and the owner still gets in from
// their own address with their own password.
func TestAContestedAccountIsReportedButNotLockedOut(t *testing.T) {
	f := newAuthFixture(t)
	logs := &authLogRecorder{}
	f.h.Log = slog.New(logs)
	f.h.Throttle.MaxAttempts = 2

	for _, addr := range []string{"198.51.100.1:1000", "198.51.100.2:1000"} {
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"username":"adam","password":"wrong"}`))
		req.RemoteAddr = addr
		rec := httptest.NewRecorder()
		f.h.Login(rec, req)
	}

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"username":"adam","password":"right-password"}`))
	req.RemoteAddr = "203.0.113.9:1000"
	rec := httptest.NewRecorder()
	f.h.Login(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("the owner, from a fresh address, got %d; body = %s", rec.Code, rec.Body)
	}
	if !logs.warnedAbout("username", "adam") {
		t.Error("no warning naming the contested account was logged")
	}
}

func TestATokenRequestIsValidatedBeforeSigningIn(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"device":`},
		{"device name too long", `{"username":"adam","password":"right-password","device":"` +
			strings.Repeat("é", maxDeviceName+1) + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			rec := authPost(f.h.Token, "/api/v1/auth/token", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
			}
			if len(f.store.upserted) != 0 {
				t.Error("an invalid request reached the account store")
			}
		})
	}

	t.Run("a name of exactly the limit is accepted", func(t *testing.T) {
		f := newAuthFixture(t)
		rec := authPost(f.h.Token, "/api/v1/auth/token", `{"username":"adam","password":"right-password","device":"`+
			strings.Repeat("é", maxDeviceName)+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body)
		}
	})
}

func TestATokenIsNotIssuedWhenSignInFails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		setup  func(*authFixture)
		status int
	}{
		{"wrong password", `{"username":"adam","password":"wrong","device":"phone"}`, nil, http.StatusUnauthorized},
		{"session store down", `{"username":"adam","password":"right-password","device":"phone"}`,
			func(f *authFixture) { f.ledger.createErr = errAuthStoreDown }, http.StatusInternalServerError},
		{"account unreadable", `{"username":"adam","password":"right-password","device":"phone"}`,
			func(f *authFixture) { f.store.userErr = errAuthStoreDown }, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			rec := authPost(f.h.Token, "/api/v1/auth/token", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if strings.Contains(rec.Body.String(), `"token"`) {
				t.Errorf("a failed request carried a token: %s", rec.Body)
			}
		})
	}
}

// Signing out ends the session the request carried, on the server, and clears
// the cookie in the browser. Ending it only in the browser would leave a
// stolen copy working.
func TestSigningOutEndsTheSessionItCarries(t *testing.T) {
	f := newAuthFixture(t)
	token, _ := f.ledger.seed(f.store.userID.String())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
	rec := httptest.NewRecorder()
	f.h.Logout(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(f.ledger.revoked) != 1 || f.ledger.revoked[0] != auth.HashToken(token) {
		t.Errorf("revoked = %v, want exactly the session the request carried", f.ledger.revoked)
	}
	c := authCookie(rec, auth.SessionCookie)
	if c == nil || c.MaxAge >= 0 || c.Value != "" {
		t.Errorf("cookie = %+v, want one that deletes the session cookie", c)
	}
}

// A sign-out that cannot reach the session table still clears the browser:
// the person asked to be signed out of this device, and they are.
func TestSigningOutClearsTheCookieEvenIfRevokingFails(t *testing.T) {
	f := newAuthFixture(t)
	f.ledger.revokeErr = errAuthStoreDown
	token, _ := f.ledger.seed(f.store.userID.String())

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.h.Logout(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if c := authCookie(rec, auth.SessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("cookie = %+v, want the session cookie cleared", c)
	}
}

func TestMe(t *testing.T) {
	t.Run("signed out", func(t *testing.T) {
		f := newAuthFixture(t)
		rec := httptest.NewRecorder()
		f.h.Me(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil))
		if rec.Code != http.StatusUnauthorized || authErrorCode(t, rec) != "not_signed_in" {
			t.Fatalf("status = %d body = %s, want 401 not_signed_in", rec.Code, rec.Body)
		}
	})

	t.Run("signed in", func(t *testing.T) {
		f := newAuthFixture(t)
		f.store.admin = true
		rec := httptest.NewRecorder()
		f.h.Me(rec, asUserID(httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil), f.store.userID))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
		var u dto.User
		if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
			t.Fatalf("decoding: %v", err)
		}
		// An administrator may always upload, whatever the policy says.
		if u.ID != f.store.userID.String() || !u.IsAdmin || !u.CanUpload {
			t.Errorf("user = %+v, want this administrator, able to upload", u)
		}
	})

	t.Run("a reader may upload only when the installation says so", func(t *testing.T) {
		for _, anyone := range []bool{false, true} {
			f := newAuthFixture(t)
			f.h.AnyoneMayUpload = anyone
			rec := httptest.NewRecorder()
			f.h.Me(rec, asUserID(httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil), f.store.userID))
			var u dto.User
			if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if u.CanUpload != anyone {
				t.Errorf("AnyoneMayUpload=%v: canUpload = %v", anyone, u.CanUpload)
			}
		}
	})

	// Asking needs a chat model. Without one the client hides its Ask entry
	// points, and it learns that here.
	t.Run("asking is offered only with a chat model", func(t *testing.T) {
		for _, configured := range []bool{false, true} {
			f := newAuthFixture(t)
			f.h.ChatConfigured = configured
			rec := httptest.NewRecorder()
			f.h.Me(rec, asUserID(httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil), f.store.userID))
			var u dto.User
			if err := json.Unmarshal(rec.Body.Bytes(), &u); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if u.CanAsk != configured {
				t.Errorf("ChatConfigured=%v: canAsk = %v", configured, u.CanAsk)
			}
		}
	})

	t.Run("the account cannot be read", func(t *testing.T) {
		f := newAuthFixture(t)
		f.store.userErr = errAuthStoreDown
		rec := httptest.NewRecorder()
		f.h.Me(rec, asUserID(httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil), f.store.userID))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

// The session list marks the device making the request, so somebody revoking
// the others does not sign themselves out by mistake.
func TestTheSessionListMarksTheCurrentDevice(t *testing.T) {
	f := newAuthFixture(t)
	here, there := uuid.New(), uuid.New()
	f.store.sessions = []postgres.UserSession{
		{ID: here, UserAgent: "Firefox", Provider: "local"},
		{ID: there, Device: "Adam's phone", Provider: "local"},
	}

	// Through the real middleware, so the session id on the request is put
	// there the way it is in production.
	token, _ := f.ledger.seed(f.store.userID.String())
	f.ledger.mu.Lock()
	s := f.ledger.byHash[auth.HashToken(token)]
	s.ID = here.String()
	f.ledger.byHash[auth.HashToken(token)] = s
	f.ledger.mu.Unlock()

	handler := RequireAuth(f.h.Sessions, nil, http.HandlerFunc(f.h.SessionList))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var out dto.SessionList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(out.Items) != 2 {
		t.Fatalf("items = %+v, want 2", out.Items)
	}
	current := map[string]bool{}
	for _, item := range out.Items {
		current[item.ID] = item.Current
	}
	if !current[here.String()] || current[there.String()] {
		t.Errorf("current = %v, want only %s marked", current, here)
	}
	if out.Items[1].Device != "Adam's phone" {
		t.Errorf("device = %q, want the name the phone was given", out.Items[1].Device)
	}
}

func TestSessionManagementRefusals(t *testing.T) {
	f := newAuthFixture(t)
	for _, tc := range []struct {
		name string
		call http.HandlerFunc
	}{
		{"list", f.h.SessionList},
		{"revoke one", f.h.RevokeSession},
		{"revoke all", f.h.RevokeAllSessions},
	} {
		t.Run(tc.name+" signed out", func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/x", nil)
			req.SetPathValue("id", uuid.NewString())
			tc.call(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}

	t.Run("list fails", func(t *testing.T) {
		f := newAuthFixture(t)
		f.store.sessionsErr = errAuthStoreDown
		rec := httptest.NewRecorder()
		f.h.SessionList(rec, asUser(httptest.NewRequest(http.MethodGet, "/x", nil)))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("revoke-all fails", func(t *testing.T) {
		f := newAuthFixture(t)
		f.store.revokeAllErr = errAuthStoreDown
		rec := httptest.NewRecorder()
		f.h.RevokeAllSessions(rec, asUser(httptest.NewRequest(http.MethodPost, "/x", nil)))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if authCookie(rec, auth.SessionCookie) != nil {
			t.Error("a failed revoke-all cleared the cookie as though it had worked")
		}
	})
}

// Revoking a session that is not yours is the same 404 as one that does not
// exist; revoking the one in your hand also clears its cookie.
func TestRevokingOneSession(t *testing.T) {
	t.Run("someone else's, or nobody's", func(t *testing.T) {
		f := newAuthFixture(t)
		f.store.revokeErr = store.ErrNotFound
		req := asUserID(httptest.NewRequest(http.MethodDelete, "/x", nil), f.store.userID)
		req.SetPathValue("id", "a-session")
		rec := httptest.NewRecorder()
		f.h.RevokeSession(rec, req)
		if rec.Code != http.StatusNotFound || authErrorCode(t, rec) != string(CodeNotFound) {
			t.Fatalf("status = %d body = %s, want 404 not_found", rec.Code, rec.Body)
		}
	})

	t.Run("the store fails", func(t *testing.T) {
		f := newAuthFixture(t)
		f.store.revokeErr = errAuthStoreDown
		req := asUser(httptest.NewRequest(http.MethodDelete, "/x", nil))
		req.SetPathValue("id", "a-session")
		rec := httptest.NewRecorder()
		f.h.RevokeSession(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})

	t.Run("another of my devices", func(t *testing.T) {
		f := newAuthFixture(t)
		req := asUserID(httptest.NewRequest(http.MethodDelete, "/x", nil), f.store.userID)
		req.SetPathValue("id", "the-phone")
		rec := httptest.NewRecorder()
		f.h.RevokeSession(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		if got := f.store.revoked; len(got) != 1 || got[0] != [2]string{f.store.userID.String(), "the-phone"} {
			t.Errorf("revoked = %v, want the phone, for this user", got)
		}
		if authCookie(rec, auth.SessionCookie) != nil {
			t.Error("revoking another device signed this one out")
		}
	})

	t.Run("this device", func(t *testing.T) {
		f := newAuthFixture(t)
		token, session := f.ledger.seed(f.store.userID.String())
		handler := RequireAuth(f.h.Sessions, nil, http.HandlerFunc(f.h.RevokeSession))
		mux := http.NewServeMux()
		mux.Handle("DELETE /api/v1/auth/sessions/{id}", handler)

		req := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/sessions/"+session.ID, nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body)
		}
		if c := authCookie(rec, auth.SessionCookie); c == nil || c.MaxAge >= 0 {
			t.Errorf("cookie = %+v, want this device's cookie cleared", c)
		}
	})
}

func TestRevokingEverySessionSignsThisDeviceOutToo(t *testing.T) {
	f := newAuthFixture(t)
	rec := httptest.NewRecorder()
	f.h.RevokeAllSessions(rec, asUserID(httptest.NewRequest(http.MethodPost, "/x", nil), f.store.userID))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(f.store.revokedAllFor) != 1 || f.store.revokedAllFor[0] != f.store.userID.String() {
		t.Errorf("revoked all for %v, want this user", f.store.revokedAllFor)
	}
	if c := authCookie(rec, auth.SessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("cookie = %+v, want it cleared", c)
	}
}

// Setup creates the first account, an administrator, and signs them in. Once
// any account exists it is closed for good.
func TestFirstRunSetup(t *testing.T) {
	t.Run("creates an administrator and signs them in", func(t *testing.T) {
		f := newAuthFixture(t)
		f.store.hasUser = false
		f.store.admin = true

		rec := authPost(f.h.Setup, "/api/v1/auth/setup",
			`{"username":"adam","email":"adam@example.com","password":"a long passphrase"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body)
		}
		if len(f.store.adopted) != 1 || f.store.adopted[0].Username != "adam" || !f.store.adoptedAdmin {
			t.Errorf("adopted = %+v admin=%v, want adam as an administrator", f.store.adopted, f.store.adoptedAdmin)
		}
		if c := authCookie(rec, auth.SessionCookie); c == nil || !c.HttpOnly {
			t.Errorf("cookie = %+v, want an HttpOnly session cookie", c)
		}
	})

	for _, tc := range []struct {
		name   string
		body   string
		setup  func(*authFixture)
		status int
		code   string
	}{
		{"closed once an account exists", `{"username":"eve","password":"x"}`,
			func(f *authFixture) { f.store.hasUser = true }, http.StatusConflict, "setup_complete"},
		{"cannot tell whether an account exists", `{"username":"eve","password":"x"}`,
			func(f *authFixture) { f.store.hasUserErr = errAuthStoreDown }, http.StatusInternalServerError, string(CodeInternal)},
		{"malformed JSON", `{"username":`, nil, http.StatusBadRequest, string(CodeInvalid)},
		{"no username", `{"username":"   ","password":"a long passphrase"}`, nil, http.StatusBadRequest, string(CodeInvalid)},
		{"weak password", `{"username":"adam","password":"x"}`,
			func(f *authFixture) { f.store.adoptErr = auth.ErrWeakPassword }, http.StatusBadRequest, string(CodeInvalid)},
		{"the account cannot be created", `{"username":"adam","password":"a long passphrase"}`,
			func(f *authFixture) { f.store.adoptErr = errAuthStoreDown }, http.StatusInternalServerError, string(CodeInternal)},
		{"the session cannot be recorded", `{"username":"adam","password":"a long passphrase"}`,
			func(f *authFixture) { f.ledger.createErr = errAuthStoreDown }, http.StatusInternalServerError, string(CodeInternal)},
		{"the account cannot be read back", `{"username":"adam","password":"a long passphrase"}`,
			func(f *authFixture) { f.store.userErr = errAuthStoreDown }, http.StatusInternalServerError, string(CodeInternal)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			f.store.hasUser = false
			if tc.setup != nil {
				tc.setup(f)
			}
			rec := authPost(f.h.Setup, "/api/v1/auth/setup", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.status, rec.Body)
			}
			if got := authErrorCode(t, rec); got != tc.code {
				t.Errorf("code = %q, want %q", got, tc.code)
			}
		})
	}

	t.Run("the setup door is closed before the body is read", func(t *testing.T) {
		f := newAuthFixture(t)
		f.store.hasUser = true
		authPost(f.h.Setup, "/api/v1/auth/setup", `{"username":"eve","password":"a long passphrase"}`)
		if len(f.store.adopted) != 0 {
			t.Error("setup created an account on an installation that already has one")
		}
	})
}

func TestTheProvidersListSaysWhenSetupIsNeeded(t *testing.T) {
	f := newAuthFixture(t)
	f.store.hasUser = false

	rec := httptest.NewRecorder()
	f.h.ProvidersList(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/providers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out dto.AuthOptions
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !out.SetupRequired {
		t.Error("setupRequired = false with no accounts")
	}
	kinds := map[string]string{}
	for _, p := range out.Providers {
		kinds[p.Name] = p.Kind
	}
	if kinds["local"] != "password" || kinds["ldap"] != "password" || kinds["oidc"] != "redirect" {
		t.Errorf("providers = %v, want each listed with its kind, in order", out.Providers)
	}
	if out.Providers[0].Name != "local" {
		t.Errorf("first provider = %q, want the configured default", out.Providers[0].Name)
	}

	f.store.hasUserErr = errAuthStoreDown
	rec = httptest.NewRecorder()
	f.h.ProvidersList(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/providers", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure: status = %d, want 500", rec.Code)
	}
}

func authStart(f *authFixture, provider string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/"+provider+"/start", nil)
	req.SetPathValue("provider", provider)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	f.h.StartRedirect(rec, req)
	return rec
}

// Starting a federated sign-in sends the browser to the provider with a state
// it will have to bring back, and keeps that state in a short-lived cookie
// the callback can check.
func TestStartingAFederatedSignIn(t *testing.T) {
	f := newAuthFixture(t)

	rec := authStart(f, "oidc", func(r *http.Request) {
		r.Host = "internal:8080"
		r.Header.Set("X-Forwarded-Proto", "https")
		r.Header.Set("X-Forwarded-Host", "books.example.org")
	})
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302; body = %s", rec.Code, rec.Body)
	}

	state := authCookie(rec, oidcStateCookie)
	if state == nil || state.Value == "" {
		t.Fatal("no state cookie, so the callback cannot be verified")
	}
	if !state.HttpOnly || !state.Secure || state.SameSite != http.SameSiteLaxMode ||
		state.Path != "/api/v1/auth/" || state.MaxAge <= 0 || state.MaxAge > 3600 {
		t.Errorf("state cookie = %+v, want short-lived, HttpOnly, Secure, Lax, scoped to the auth routes", state)
	}

	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	if location.Host != "idp.example" {
		t.Errorf("redirected to %s, want the identity provider", location)
	}
	kept, nonce, ok := strings.Cut(state.Value, ".")
	if !ok || kept == "" || nonce == "" {
		t.Fatalf("state cookie %q does not keep both the state and the nonce", state.Value)
	}
	if location.Query().Get("state") != kept {
		t.Error("the state sent to the provider is not the one kept in the cookie")
	}
	// A nonce of its own, never the state: the state comes back in the
	// callback URL, the nonce only inside the signed token.
	if location.Query().Get("nonce") != nonce || nonce == kept {
		t.Errorf("nonce sent = %q, kept = %q, state = %q; want the kept nonce, distinct from the state",
			location.Query().Get("nonce"), nonce, kept)
	}

	// And the callback hands that nonce to the exchange, which checks the
	// identity carries it.
	cb := authCallback(f, "oidc", "state="+url.QueryEscape(kept)+"&code=abc", state.Value)
	if cb.Code != http.StatusFound || cb.Header().Get("Location") != "/" {
		t.Fatalf("callback: status = %d, Location = %q", cb.Code, cb.Header().Get("Location"))
	}
	if len(f.oidc.exchanged) != 3 || f.oidc.exchanged[1] != nonce {
		t.Errorf("exchanged = %v, want the nonce from the cookie", f.oidc.exchanged)
	}
	// Behind a proxy the callback must name the address the browser used.
	if got := location.Query().Get("redirect_uri"); got != "https://books.example.org/api/v1/auth/oidc/callback" {
		t.Errorf("redirect_uri = %q, want the forwarded scheme and host", got)
	}

	t.Run("without a proxy the request's own address is used", func(t *testing.T) {
		rec := authStart(f, "oidc", func(r *http.Request) { r.Host = "lexarii.lan" })
		location, _ := url.Parse(rec.Header().Get("Location"))
		if got := location.Query().Get("redirect_uri"); got != "http://lexarii.lan/api/v1/auth/oidc/callback" {
			t.Errorf("redirect_uri = %q", got)
		}
	})

	t.Run("over TLS the scheme is https", func(t *testing.T) {
		rec := authStart(f, "oidc", func(r *http.Request) {
			r.Host = "lexarii.lan"
			r.TLS = &tls.ConnectionState{}
		})
		location, _ := url.Parse(rec.Header().Get("Location"))
		if got := location.Query().Get("redirect_uri"); !strings.HasPrefix(got, "https://lexarii.lan/") {
			t.Errorf("redirect_uri = %q, want https", got)
		}
	})
}

func TestStartingAFederatedSignInRefusals(t *testing.T) {
	f := newAuthFixture(t)
	if rec := authStart(f, "nonesuch", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown provider: status = %d, want 404", rec.Code)
	}
	if rec := authStart(f, "local", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("a password provider: status = %d, want 400", rec.Code)
	}
}

func authCallback(f *authFixture, provider, query, state string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/"+provider+"/callback?"+query, nil)
	req.SetPathValue("provider", provider)
	if state != "" {
		req.AddCookie(&http.Cookie{Name: oidcStateCookie, Value: state})
	}
	rec := httptest.NewRecorder()
	f.h.Callback(rec, req)
	return rec
}

// signInError is the message a failed callback sends the browser back with.
func signInError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want a 302 back to the sign-in page; body = %s", rec.Code, rec.Body)
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || location.Path != "/signin" {
		t.Fatalf("Location = %q, want /signin", rec.Header().Get("Location"))
	}
	return location.Query().Get("error")
}

func TestACallbackSignsInAndEstablishesASession(t *testing.T) {
	f := newAuthFixture(t)

	rec := authCallback(f, "oidc", "state=s3cret&code=abc", "s3cret.n0nce")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("status = %d, Location = %q; want 302 to /", rec.Code, rec.Header().Get("Location"))
	}
	if c := authCookie(rec, auth.SessionCookie); c == nil || c.Value == "" {
		t.Fatal("no session cookie after a successful callback")
	}
	// The state is single-use.
	if c := authCookie(rec, oidcStateCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("state cookie = %+v, want it cleared", c)
	}
	if len(f.oidc.exchanged) != 3 || f.oidc.exchanged[0] != "abc" || f.oidc.exchanged[1] != "n0nce" ||
		!strings.HasSuffix(f.oidc.exchanged[2], "/api/v1/auth/oidc/callback") {
		t.Errorf("exchanged = %v, want the code, the kept nonce and the same callback URL", f.oidc.exchanged)
	}
	if len(f.store.allowCreate) != 1 || !f.store.allowCreate[0] {
		t.Errorf("allowCreate = %v, want what the provider allows", f.store.allowCreate)
	}
}

func TestACallbackIsRefusedBackToTheSignInPage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		query  string
		cookie string
		setup  func(*authFixture)
	}{
		{"no state cookie: a stale tab", "state=s&code=abc", "", nil},
		{"a state cookie with no nonce", "state=s&code=abc", "s", nil},
		{"a state cookie with an empty nonce", "state=s&code=abc", "s.", nil},
		{"state does not match: a forged callback", "state=other&code=abc", "s.n", nil},
		{"the nonce sent as the state", "state=n&code=abc", "s.n", nil},
		{"the provider refused", "state=s&error=access_denied", "s.n", nil},
		{"no code", "state=s", "s.n", nil},
		{"the exchange failed", "state=s&code=abc", "s.n", func(f *authFixture) { f.oidc.exchangeErr = auth.ErrInvalidCredentials }},
		{"the identity was for another sign-in", "state=s&code=abc", "s.n", func(f *authFixture) { f.oidc.exchangeErr = auth.ErrNonceMismatch }},
		{"no account here", "state=s&code=abc", "s.n", func(f *authFixture) { f.store.upsertErr = auth.ErrInvalidCredentials }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			rec := authCallback(f, "oidc", tc.query, tc.cookie)
			if msg := signInError(t, rec); msg == "" {
				t.Error("sent back with no message to show")
			}
			if c := authCookie(rec, auth.SessionCookie); c != nil {
				t.Error("a refused callback established a session")
			}
		})
	}

	t.Run("a forged callback never reaches the provider", func(t *testing.T) {
		f := newAuthFixture(t)
		authCallback(f, "oidc", "state=other&code=abc", "s.n")
		authCallback(f, "oidc", "state=s&code=abc", "s")
		if len(f.oidc.exchanged) != 0 {
			t.Error("a code from a callback with the wrong state, or no nonce, was exchanged")
		}
	})
}

func TestACallbackForAProviderThatCannotTakeOne(t *testing.T) {
	f := newAuthFixture(t)
	if rec := authCallback(f, "nonesuch", "state=s&code=c", "s.n"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown provider: status = %d, want 404", rec.Code)
	}
	if rec := authCallback(f, "ldap", "state=s&code=c", "s.n"); rec.Code != http.StatusBadRequest {
		t.Errorf("a password provider: status = %d, want 400", rec.Code)
	}
}

func TestACallbackWhoseStoreFailsIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*authFixture)
	}{
		{"account store", func(f *authFixture) { f.store.upsertErr = errAuthStoreDown }},
		{"session store", func(f *authFixture) { f.ledger.createErr = errAuthStoreDown }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			tc.setup(f)
			rec := authCallback(f, "oidc", "state=s&code=abc", "s.n")
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
		})
	}
}

// authProxy is a proxy provider trusting loopback, which is where
// httptest.NewRequest says requests come from only when told to.
func authProxy(t *testing.T, signup bool) *auth.ProxyProvider {
	t.Helper()
	p, err := auth.NewProxyProvider(auth.ProxyConfig{
		UserHeader: "Remote-User", TrustedProxies: []string{"127.0.0.1/32"}, AllowSignup: signup,
	})
	if err != nil {
		t.Fatalf("NewProxyProvider: %v", err)
	}
	return p
}

// authSeenUser is the user a request reached the next handler with.
type authSeenUser struct {
	reached bool
	user    uuid.UUID
	signed  bool
}

func (s *authSeenUser) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.reached = true
		s.user, s.signed = UserFrom(r.Context())
		w.WriteHeader(http.StatusTeapot)
	})
}

func TestHeaderAuthentication(t *testing.T) {
	proxied := func(remote, user string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/library", nil)
		req.RemoteAddr = remote
		if user != "" {
			req.Header.Set("Remote-User", user)
		}
		return req
	}

	t.Run("no proxy configured changes nothing", func(t *testing.T) {
		next := http.NotFoundHandler()
		if got := HeaderAuth(nil, nil, nil, quietLogger(), next); got == nil {
			t.Fatal("nil handler")
		}
	})

	t.Run("the proxy's word establishes a session", func(t *testing.T) {
		f := newAuthFixture(t)
		seen := &authSeenUser{}
		h := HeaderAuth(authProxy(t, true), f.store, f.h.Sessions, quietLogger(), seen.handler())

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, proxied("127.0.0.1:5000", "adam"))

		if !seen.signed || seen.user != f.store.userID {
			t.Fatalf("next saw user %v (signed in %v), want %v", seen.user, seen.signed, f.store.userID)
		}
		if c := authCookie(rec, auth.SessionCookie); c == nil {
			t.Error("no session cookie, so every request would establish another")
		}
		if len(f.store.allowCreate) != 1 || !f.store.allowCreate[0] {
			t.Errorf("allowCreate = %v, want the proxy's signup setting", f.store.allowCreate)
		}
		if f.store.upserted[0].Subject != "adam" {
			t.Errorf("subject = %q, want the header's user", f.store.upserted[0].Subject)
		}
	})

	t.Run("an existing session wins and no second one is made", func(t *testing.T) {
		f := newAuthFixture(t)
		token, _ := f.ledger.seed(f.store.userID.String())
		seen := &authSeenUser{}
		h := HeaderAuth(authProxy(t, true), f.store, f.h.Sessions, quietLogger(), seen.handler())

		req := proxied("127.0.0.1:5000", "adam")
		req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: token})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if !seen.reached || len(f.store.upserted) != 0 || authCookie(rec, auth.SessionCookie) != nil {
			t.Errorf("reached=%v upserts=%d; want straight through with nothing issued",
				seen.reached, len(f.store.upserted))
		}
	})

	for _, tc := range []struct {
		name   string
		req    func() *http.Request
		setup  func(*authFixture)
		upsert int
	}{
		{"a header from an untrusted address is ignored",
			func() *http.Request { return proxied("192.0.2.66:5000", "admin") }, nil, 0},
		{"a trusted proxy asserting nobody falls through",
			func() *http.Request { return proxied("127.0.0.1:5000", "") }, nil, 0},
		{"an identity with no account falls through",
			func() *http.Request { return proxied("127.0.0.1:5000", "stranger") },
			func(f *authFixture) { f.store.upsertErr = auth.ErrInvalidCredentials }, 1},
		{"a session that cannot be recorded falls through",
			func() *http.Request { return proxied("127.0.0.1:5000", "adam") },
			func(f *authFixture) { f.ledger.createErr = errAuthStoreDown }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			if tc.setup != nil {
				tc.setup(f)
			}
			seen := &authSeenUser{}
			h := HeaderAuth(authProxy(t, false), f.store, f.h.Sessions, quietLogger(), seen.handler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tc.req())

			if !seen.reached {
				t.Fatal("the request did not fall through to the ordinary paths")
			}
			if seen.signed {
				t.Error("the request was carried as signed in")
			}
			if authCookie(rec, auth.SessionCookie) != nil {
				t.Error("a session cookie was set")
			}
			if len(f.store.upserted) != tc.upsert {
				t.Errorf("upserts = %d, want %d", len(f.store.upserted), tc.upsert)
			}
		})
	}
}

// Behind the real router, a proxy-authenticated first request is not refused
// for lacking the cookie it is in the middle of being given.
func TestTheRouterWiresHeaderAuthenticationAheadOfTheSessionCheck(t *testing.T) {
	f := newAuthFixture(t)
	rt := &Router{
		Health:   &HealthHandler{},
		Auth:     f.h,
		Sessions: f.h.Sessions,
		Proxy:    authProxy(t, true),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Remote-User", "adam")
	rec := httptest.NewRecorder()
	rt.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body)
	}
	if authCookie(rec, auth.SessionCookie) == nil {
		t.Error("no session was established")
	}
}

// Basic credentials, as a reader app sends them: a device token in the
// password box first, the real password second, and both through the same
// throttle as every other door.
func TestBasicCredentials(t *testing.T) {
	basic := func(f *authFixture, username, password string) (uuid.UUID, error) {
		return f.h.AuthenticateBasic(t.Context(), httptest.NewRequest(http.MethodGet, "/opds", nil), username, password)
	}

	t.Run("a device token as the password", func(t *testing.T) {
		f := newAuthFixture(t)
		token, _ := f.ledger.seed(f.store.userID.String())
		id, err := basic(f, "anything", token)
		if err != nil || id != f.store.userID {
			t.Fatalf("= %v, %v; want the token's user", id, err)
		}
		if len(f.store.upserted) != 0 {
			t.Error("a token sign-in also tried the password")
		}
	})

	t.Run("a token for a session with no usable user", func(t *testing.T) {
		f := newAuthFixture(t)
		token, _ := f.ledger.seed("not-a-uuid")
		if _, err := basic(f, "anything", token); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("err = %v, want ErrInvalidCredentials", err)
		}
	})

	t.Run("the account password", func(t *testing.T) {
		f := newAuthFixture(t)
		id, err := basic(f, "adam", "right-password")
		if err != nil || id != f.store.userID {
			t.Fatalf("= %v, %v; want adam", id, err)
		}
		// A reader app never creates an account.
		if len(f.store.allowCreate) != 1 || f.store.allowCreate[0] {
			t.Errorf("allowCreate = %v, want false", f.store.allowCreate)
		}
	})

	t.Run("a wrong password", func(t *testing.T) {
		f := newAuthFixture(t)
		if _, err := basic(f, "adam", "wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("err = %v, want ErrInvalidCredentials", err)
		}
	})

	t.Run("no account for the identity", func(t *testing.T) {
		f := newAuthFixture(t)
		f.store.upsertErr = errAuthStoreDown
		if _, err := basic(f, "adam", "right-password"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("err = %v, want ErrInvalidCredentials", err)
		}
	})

	t.Run("no local accounts on this installation", func(t *testing.T) {
		f := newAuthFixture(t)
		f.h.Providers = auth.NewRegistry(f.oidc)
		if _, err := basic(f, "adam", "right-password"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("err = %v, want ErrInvalidCredentials", err)
		}
	})

	t.Run("throttled even with the right password", func(t *testing.T) {
		f := newAuthFixture(t)
		f.h.Throttle.MaxAttempts = 1
		_, _ = basic(f, "adam", "wrong")
		if _, err := basic(f, "adam", "right-password"); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("err = %v, want refusal while locked out", err)
		}
		if len(f.store.upserted) != 0 {
			t.Error("a throttled attempt reached the account store")
		}
	})
}

// A session row whose user is not an id is not a sign-in.
func TestASessionWithoutAUsableUserIsRefused(t *testing.T) {
	f := newAuthFixture(t)
	token, _ := f.ledger.seed("not-a-uuid")
	seen := &authSeenUser{}
	h := RequireAuth(f.h.Sessions, nil, seen.handler())

	req := httptest.NewRequest(http.MethodGet, "/api/v1/library", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized || seen.reached {
		t.Fatalf("status = %d reached = %v, want 401 and nothing reached", rec.Code, seen.reached)
	}
}

// An open route still knows who is asking when they are signed in, and a
// path with a dot segment is never treated as open.
func TestTheGateOnOpenRoutes(t *testing.T) {
	f := newAuthFixture(t)
	token, _ := f.ledger.seed(f.store.userID.String())

	t.Run("an open route sees the signed-in user", func(t *testing.T) {
		seen := &authSeenUser{}
		h := RequireAuth(f.h.Sessions, nil, seen.handler())
		req := httptest.NewRequest(http.MethodHead, "/api/v1/auth/providers", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(httptest.NewRecorder(), req)
		if !seen.signed || seen.user != f.store.userID {
			t.Errorf("open route saw %v (signed in %v), want the user", seen.user, seen.signed)
		}
	})

	for _, target := range []string{"/assets/./app.js", "/assets/../api/v1/library", "/assets//app.js"} {
		t.Run(target, func(t *testing.T) {
			if _, plain := gatePath(rawRequest(t, http.MethodGet, target)); plain {
				t.Errorf("gatePath(%q) is plain, want it to need a session", target)
			}
		})
	}
	if p, plain := gatePath(rawRequest(t, http.MethodGet, "/assets/app.js")); !plain || p != "/assets/app.js" {
		t.Errorf("gatePath(/assets/app.js) = %q, %v; want it plain", p, plain)
	}
}

func TestSessionFromIsAbsentWithoutACookieSession(t *testing.T) {
	if _, ok := SessionFrom(context.Background()); ok {
		t.Error("a bare context claims a session")
	}
	ctx := context.WithValue(context.Background(), sessionContextKey{}, "abc")
	if id, ok := SessionFrom(ctx); !ok || id != "abc" {
		t.Errorf("SessionFrom = %q, %v; want abc", id, ok)
	}
}
