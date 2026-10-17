// SPDX-License-Identifier: AGPL-3.0-or-later

package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/http/dto"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
	"scm.poniatowski.dev/adam/lexarii/internal/store/postgres"
)

// AuthStore is what the authentication handlers need from the store.
type AuthStore interface {
	UpsertIdentity(ctx context.Context, identity auth.Identity, allowCreate bool) (uuid.UUID, uuid.UUID, error)
	AdoptOrCreateLocalUser(ctx context.Context, username, email, password string, admin bool) (uuid.UUID, error)
	HasAnyUser(ctx context.Context) (bool, error)
	User(ctx context.Context, id uuid.UUID) (postgres.UserRecord, error)
	UserSessions(ctx context.Context, userID string) ([]postgres.UserSession, error)
	RevokeSessionByID(ctx context.Context, userID, sessionID string) error
	RevokeAllSessions(ctx context.Context, userID string) error
}

// AuthHandler serves sign-in, sign-out, and first-run setup.
type AuthHandler struct {
	Store     AuthStore
	Providers *auth.Registry
	Sessions  *auth.Sessions
	Log       *slog.Logger
	// Throttle limits repeated failed sign-ins. Nil disables limiting, which is
	// only appropriate in a test.
	Throttle *auth.Throttle
	// Trusted names this installation's own reverse proxies, so a sign-in is
	// counted against the reader who made it rather than against the proxy
	// every reader shares. Empty means requests are taken at their socket
	// address, which is correct when nothing sits in front.
	Trusted *auth.TrustedProxies
	// AnyoneMayUpload mirrors UploadHandler's, so the user record can say
	// whether this person may add books and the client need not guess from
	// isAdmin, which stopped being the whole answer when uploading became a
	// setting.
	AnyoneMayUpload bool
	// ChatConfigured says a chat model is set up, so the user record can say
	// whether asking the library can work at all (dto.User.CanAsk).
	ChatConfigured bool
	// PeopleUnlisted mirrors SharingHandler.Unlisted, so the user record can
	// say whether this person is shown a list of people to lend to
	// (dto.User.CanListPeople) or asked for a username.
	PeopleUnlisted bool
	// SignInLinks are shown on the sign-in page, in this order. Validated at
	// startup (see config.ParseSignInLinks), so they are passed through as
	// they are.
	SignInLinks []dto.SignInLink
	// Branding tells the client that /branding/branding.css exists and should
	// be loaded after the app's own styles (dto.AuthOptions.Branding). Set
	// with Router.Branding when LEXARII_BRANDING_DIR is configured, or by an
	// embedder that serves /branding/ itself. See docs/BRANDING.md.
	Branding bool
}

// userContextKey carries the authenticated user through a request.
type userContextKey struct{}

// sessionContextKey carries the id of the session that authenticated a request.
//
// Only so the sessions list can mark which entry is the device being used. It
// is the session's id, never its token: the token is the credential and must
// not travel anywhere it is not needed.
type sessionContextKey struct{}

// SessionFrom returns the id of the session that authenticated this request.
//
// Absent for header-authenticated requests on the hop that established them,
// and for anything not authenticated by a cookie at all.
func SessionFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(sessionContextKey{}).(string)
	return id, ok
}

// UserFrom returns the authenticated user, if any.
func UserFrom(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(userContextKey{}).(uuid.UUID)
	return id, ok
}

// Providers serves GET /api/v1/auth/providers.
//
// The login page asks what is available rather than assuming a password form,
// so adding OIDC or LDAP changes the screen without changing the client.
func (h *AuthHandler) ProvidersList(w http.ResponseWriter, r *http.Request) {
	hasUser, err := h.Store.HasAnyUser(r.Context())
	if err != nil {
		respondError(w, err)
		return
	}

	out := dto.AuthOptions{
		// No accounts yet: the client must offer setup, not a login form nobody
		// can satisfy.
		SetupRequired: !hasUser,
		Providers:     make([]dto.AuthProvider, 0),
		Links:         make([]dto.SignInLink, 0, len(h.SignInLinks)),
		Branding:      h.Branding,
	}
	out.Links = append(out.Links, h.SignInLinks...)
	for _, p := range h.Providers.All() {
		out.Providers = append(out.Providers, dto.AuthProvider{
			Name:        p.Name(),
			DisplayName: p.DisplayName(),
			Kind:        string(p.Kind()),
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// Login serves POST /api/v1/auth/login.
//
// A browser session: the token goes into an HttpOnly cookie and is never in
// the response body, so no script on the page can read it. A client that holds
// its own token wants /api/v1/auth/token instead.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var body dto.LoginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must be a login request", err))
		return
	}

	userID, identityID, ok := h.signIn(w, r, body)
	if !ok {
		return
	}

	cookie, _, err := h.Sessions.Issue(r.Context(), r, userID.String(), identityID.String())
	if err != nil {
		respondError(w, err)
		return
	}
	http.SetCookie(w, cookie)

	user, err := h.Store.User(r.Context(), userID)
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, h.userDTO(user))
}

// Token serves POST /api/v1/auth/token, for a client that is not a browser.
//
// The same credentials and the same session table as a browser login; the
// difference is that the token comes back in the body, because a native client
// stores it itself and sends it as `Authorization: Bearer`. A phone has no
// same-origin policy to be protected by and no HttpOnly to hide behind, so
// this is a weaker position than a cookie and it is the right one for a client
// that is not a web page.
//
// It is a SEPARATE endpoint rather than a flag on the login for exactly that
// reason: the web client cannot be talked into asking for a token it would
// then have to hold in reach of any injected script, and a reader of this
// router can see at a glance which endpoint hands one out.
//
// The device name is required and is not decoration. This token lasts a month
// and lives on a thing that gets lost; the person revoking it a year from now
// has to be able to tell which line in their session list is the phone they no
// longer have, and every Android client sends the same user-agent.
func (h *AuthHandler) Token(w http.ResponseWriter, r *http.Request) {
	var body dto.TokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must be a token request", err))
		return
	}

	device := strings.TrimSpace(body.Device)
	if device == "" {
		respondError(w, Invalid("a device name is required, so this token can be found and revoked later", nil))
		return
	}
	if len([]rune(device)) > maxDeviceName {
		respondError(w, Invalid("that device name is too long", nil))
		return
	}

	userID, identityID, ok := h.signIn(w, r, dto.LoginRequest{
		Provider: body.Provider,
		Username: body.Username,
		Password: body.Password,
	})
	if !ok {
		return
	}

	token, session, err := h.Sessions.IssueToken(r.Context(), r, userID.String(), identityID.String(), device)
	if err != nil {
		respondError(w, err)
		return
	}

	user, err := h.Store.User(r.Context(), userID)
	if err != nil {
		respondError(w, err)
		return
	}

	h.logger().Info("issued a device token", "user", userID, "device", device, "session", session.ID)
	respondJSON(w, http.StatusOK, dto.AuthToken{
		Token:     token,
		SessionID: session.ID,
		ExpiresAt: session.ExpiresAt,
		User:      h.userDTO(user),
	})
}

// maxDeviceName bounds what goes in the session list. It is a label a person
// reads, not a field anybody should be able to fill a page with.
const maxDeviceName = 64

// signIn runs the shared sign-in path: throttle, authenticate, resolve the
// account. It answers the request itself on failure and reports whether the
// caller should carry on.
//
// Shared by the cookie login and the device token deliberately. Two copies of
// a throttle is two places for one of them to stop being applied.
func (h *AuthHandler) signIn(w http.ResponseWriter, r *http.Request, body dto.LoginRequest) (userID, identityID uuid.UUID, ok bool) {
	name := strings.TrimSpace(body.Provider)
	if name == "" {
		name = auth.ProviderLocal
	}

	provider, err := h.Providers.Password(name)
	if err != nil {
		respondError(w, Invalid("that sign-in method is not available", err))
		return uuid.Nil, uuid.Nil, false
	}

	// Throttled per address, and per address and username together. NOT on the
	// username alone: see auth.SignInAttempt for why that one only warns.
	attempt := h.Throttle.SignIn(body.Username, h.clientAddr(r))

	if allowed, wait := attempt.Allow(); !allowed {
		h.logger().Warn("sign-in throttled", "username", body.Username,
			"addr", h.clientAddr(r), "retryInMs", wait.Milliseconds())
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		respondError(w, &httpError{
			status:  http.StatusTooManyRequests,
			code:    "too_many_attempts",
			message: "too many failed sign-in attempts; try again shortly",
		})
		return uuid.Nil, uuid.Nil, false
	}

	// Said out loud, and nothing is refused on the strength of it: failures
	// against this name are coming from more than one address, which is what a
	// distributed attack on one account looks like. The owner still gets in
	// with their own password.
	if attempt.Contested() {
		h.logger().Warn("this account is collecting failed sign-ins from several addresses",
			"username", body.Username)
	}

	identity, err := provider.Authenticate(r.Context(), body.Username, body.Password)
	if err != nil {
		attempt.Fail()
		// One message for every failure. Saying which part was wrong tells an
		// attacker which usernames exist.
		h.logger().Info("failed sign-in", "provider", name, "username", body.Username)
		respondError(w, &httpError{
			status:  http.StatusUnauthorized,
			code:    "invalid_credentials",
			message: "that username and password do not match",
		})
		return uuid.Nil, uuid.Nil, false
	}

	attempt.Succeed()

	// A federated provider may be allowed to create accounts on first sign-in;
	// local never is, because a local account only exists if someone made it.
	allowCreate := provider.Kind() != auth.KindPassword || name != auth.ProviderLocal

	userID, identityID, err = h.Store.UpsertIdentity(r.Context(), identity, allowCreate)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			respondError(w, &httpError{
				status:  http.StatusForbidden,
				code:    "no_account",
				message: "you were authenticated, but there is no account here for you",
			})
			return uuid.Nil, uuid.Nil, false
		}
		respondError(w, err)
		return uuid.Nil, uuid.Nil, false
	}
	return userID, identityID, true
}

// Logout serves POST /api/v1/auth/logout.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if err := h.Sessions.Revoke(r.Context(), r); err != nil {
		h.logger().Warn("revoking a session failed", "error", err)
	}
	http.SetCookie(w, h.Sessions.ClearCookie())
	respondJSON(w, http.StatusNoContent, nil)
}

// Me serves GET /api/v1/auth/me.
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserFrom(r.Context())
	if !ok {
		respondError(w, &httpError{
			status:  http.StatusUnauthorized,
			code:    "not_signed_in",
			message: "not signed in",
		})
		return
	}

	user, err := h.Store.User(r.Context(), userID)
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, h.userDTO(user))
}

// Sessions serves GET /api/v1/auth/sessions.
//
// Someone who suspects a session is not theirs needs to see it before they can
// end it, and needs to know which one is their own so they do not sign
// themselves out trying.
func (h *AuthHandler) SessionList(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	items, err := h.Store.UserSessions(r.Context(), userID.String())
	if err != nil {
		respondError(w, err)
		return
	}

	current, _ := SessionFrom(r.Context())
	out := dto.SessionList{Items: make([]dto.Session, 0, len(items))}
	for _, s := range items {
		out.Items = append(out.Items, dto.Session{
			ID:         s.ID.String(),
			Current:    s.ID.String() == current,
			UserAgent:  s.UserAgent,
			ClientIP:   s.ClientIP,
			Provider:   s.Provider,
			Device:     s.Device,
			CreatedAt:  s.CreatedAt,
			LastUsedAt: s.LastUsedAt,
			ExpiresAt:  s.ExpiresAt,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// RevokeSession serves DELETE /api/v1/auth/sessions/{id}.
//
// Revoking the current session is allowed and clears the cookie too: "sign this
// device out" is a reasonable thing to mean, and leaving a cookie pointing at a
// dead session would show a signed-in shell that fails on every request.
func (h *AuthHandler) RevokeSession(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	if err := h.Store.RevokeSessionByID(r.Context(), userID.String(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			respondError(w, NotFound("session"))
			return
		}
		respondError(w, err)
		return
	}

	if current, ok := SessionFrom(r.Context()); ok && current == id {
		http.SetCookie(w, h.Sessions.ClearCookie())
	}
	respondJSON(w, http.StatusNoContent, nil)
}

// RevokeAllSessions serves POST /api/v1/auth/sessions/revoke-all.
//
// Every session including this one. That is the point: it is what someone
// reaches for when they believe a token has been taken, and sparing the current
// one would leave the attacker signed in if the attacker is the one asking.
func (h *AuthHandler) RevokeAllSessions(w http.ResponseWriter, r *http.Request) {
	userID, ok := userOrUnauthorised(w, r)
	if !ok {
		return
	}

	if err := h.Store.RevokeAllSessions(r.Context(), userID.String()); err != nil {
		respondError(w, err)
		return
	}
	http.SetCookie(w, h.Sessions.ClearCookie())
	respondJSON(w, http.StatusNoContent, nil)
}

// Setup serves POST /api/v1/auth/setup, creating the first account.
//
// Only available while there are no accounts at all. Once one exists this is
// closed permanently, or it would be an open door to making an administrator.
func (h *AuthHandler) Setup(w http.ResponseWriter, r *http.Request) {
	hasUser, err := h.Store.HasAnyUser(r.Context())
	if err != nil {
		respondError(w, err)
		return
	}
	if hasUser {
		respondError(w, &httpError{
			status:  http.StatusConflict,
			code:    "setup_complete",
			message: "this instance already has an account; sign in instead",
		})
		return
	}

	var body dto.SetupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		respondError(w, Invalid("body must be a setup request", err))
		return
	}
	if strings.TrimSpace(body.Username) == "" {
		respondError(w, Invalid("a username is required", nil))
		return
	}

	userID, err := h.Store.AdoptOrCreateLocalUser(r.Context(), body.Username, body.Email, body.Password, true)
	if err != nil {
		if errors.Is(err, auth.ErrWeakPassword) {
			respondError(w, Invalid(err.Error(), err))
			return
		}
		respondError(w, err)
		return
	}

	h.logger().Info("first account created", "username", body.Username)

	cookie, _, err := h.Sessions.Issue(r.Context(), r, userID.String(), "")
	if err != nil {
		respondError(w, err)
		return
	}
	http.SetCookie(w, cookie)

	user, err := h.Store.User(r.Context(), userID)
	if err != nil {
		respondError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, h.userDTO(user))
}

// HeaderAuth authenticates from a trusted reverse proxy's headers.
//
// It runs BEFORE the session check, because with an SSO proxy in front there is
// no login page to reach: the proxy has already decided who this is, and a
// session is established silently on the first request.
func HeaderAuth(provider *auth.ProxyProvider, store AuthStore, sessions *auth.Sessions, log *slog.Logger, next http.Handler) http.Handler {
	if provider == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// An existing session wins: re-establishing one on every request would
		// mean a new row per request.
		if _, err := sessions.Validate(r.Context(), r); err == nil {
			next.ServeHTTP(w, r)
			return
		}

		identity, err := provider.IdentityFromRequest(r)
		if err != nil {
			// Not from the proxy, or the proxy asserted nobody. Fall through to
			// the ordinary paths so local sign-in still works.
			next.ServeHTTP(w, r)
			return
		}

		userID, identityID, err := store.UpsertIdentity(r.Context(), identity, provider.AllowSignup())
		if err != nil {
			log.Warn("a proxy-asserted identity has no account here",
				"subject", identity.Subject, "error", err)
			next.ServeHTTP(w, r)
			return
		}

		cookie, _, err := sessions.Issue(r.Context(), r, userID.String(), identityID.String())
		if err != nil {
			log.Error("issuing a session for a proxy-authenticated request failed", "error", err)
			next.ServeHTTP(w, r)
			return
		}
		http.SetCookie(w, cookie)

		// Carry the user on THIS request too, so the one that established the
		// session is not the only one to be refused.
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, userID)))
	})
}

// AuthenticateBasic resolves HTTP Basic credentials, for a client that knows
// no other way to sign in.
//
// Ebook readers are that client. KOReader, Librera, Thorium and the rest speak
// OPDS and offer one box for a username and one for a password, and a
// catalogue they cannot sign in to is a catalogue they cannot open.
//
// The password may be a DEVICE TOKEN, and that is the way to use this. A token
// is issued by /auth/token, is revocable on its own, and means the account
// password never goes into a reader app's settings screen where it lives in
// cleartext on a phone. Tried first, and it costs one indexed lookup.
//
// Falling back to the real password, through the same throttle as every other
// sign-in, because a person setting up a reader has a password to hand and may
// not yet know that tokens exist.
func (h *AuthHandler) AuthenticateBasic(ctx context.Context, r *http.Request, username, password string) (uuid.UUID, error) {
	if session, err := h.Sessions.ValidateToken(ctx, password); err == nil {
		id, err := uuid.Parse(session.UserID)
		if err != nil {
			return uuid.Nil, auth.ErrInvalidCredentials
		}
		return id, nil
	}

	provider, err := h.Providers.Password(auth.ProviderLocal)
	if err != nil {
		return uuid.Nil, auth.ErrInvalidCredentials
	}

	// Keyed on the CALLER's address, not on the scheme.
	//
	// It was the constant "addr:basic", which is one bucket shared by
	// everybody: eight bad passwords from anywhere locked password sign-in for
	// every reader app on the installation for fifteen minutes, and /opds is
	// reachable without a session, so an anonymous caller could do it on
	// purpose and repeat it indefinitely. A throttle that punishes the victim
	// is worse than none, because it looks like protection.
	//
	// The same limiter as the browser login, for the same reason: one policy,
	// applied at both doors, or the door without it is the one that gets used.
	attempt := h.Throttle.SignIn(username, h.clientAddr(r))
	if allowed, _ := attempt.Allow(); !allowed {
		return uuid.Nil, auth.ErrInvalidCredentials
	}

	identity, err := provider.Authenticate(ctx, username, password)
	if err != nil {
		attempt.Fail()
		return uuid.Nil, auth.ErrInvalidCredentials
	}
	attempt.Succeed()

	userID, _, err := h.Store.UpsertIdentity(ctx, identity, false)
	if err != nil {
		return uuid.Nil, auth.ErrInvalidCredentials
	}
	return userID, nil
}

// BasicAuthenticator resolves a username and password. Declared here because
// this is what needs one, and satisfied by AuthHandler.
type BasicAuthenticator interface {
	AuthenticateBasic(ctx context.Context, r *http.Request, username, password string) (uuid.UUID, error)
}

// RequireAuth rejects unauthenticated requests.
//
// An allowlist of open paths, never a denylist: a new endpoint must be
// protected by default, because the one that is forgotten is always the one
// that mattered.
func RequireAuth(sessions *auth.Sessions, basic BasicAuthenticator, next http.Handler) http.Handler {
	return RequireAuthWithPolicy(sessions, basic, nil, next)
}

// RequireAuthWithPolicy is RequireAuth with an access policy applied once the
// user is known. The policy runs exactly once per protected request, however
// the user was identified, and never on an open route; see AuthorizeFunc. A
// nil policy is RequireAuth exactly.
func RequireAuthWithPolicy(sessions *auth.Sessions, basic BasicAuthenticator, policy AuthorizeFunc, next http.Handler) http.Handler {
	// Keyed on method and path, the way the router matches them. A path alone
	// would open every method on it, including ones a later route adds.
	open := map[string]bool{
		"GET /api/v1/health":         true,
		"GET /api/v1/auth/providers": true,
		"POST /api/v1/auth/login":    true,
		// The door a native client comes in by. Open for the same reason the
		// login is: a client cannot present a session it has not been given
		// yet. Caught by trying it - the endpoint answered 401 to the one
		// request it exists to serve.
		"POST /api/v1/auth/token":  true,
		"POST /api/v1/auth/logout": true,
		"POST /api/v1/auth/setup":  true,
	}
	isOpen := func(method, p string) bool {
		// The router answers HEAD with the GET handler.
		if method == http.MethodHead {
			method = http.MethodGet
		}
		return open[method+" "+p]
	}

	// The federated sign-in routes carry the provider name in the path:
	// GET /api/v1/auth/{provider}/start and .../callback, and nothing else.
	// Exactly that shape - one non-empty provider segment, then the last one -
	// because a looser prefix-and-suffix match also opened
	// DELETE /api/v1/auth/sessions/start, which the router sends to the
	// session-revocation handler with the id "start".
	isAuthRedirect := func(method, p string) bool {
		if method != http.MethodGet && method != http.MethodHead {
			return false
		}
		rest, ok := strings.CutPrefix(p, "/api/v1/auth/")
		if !ok {
			return false
		}
		provider, action, ok := strings.Cut(rest, "/")
		return ok && provider != "" && (action == "start" || action == "callback")
	}

	// The catalogue is not the web client and must not be treated as one. It
	// is a read of somebody's whole library, and the rule two lines below -
	// everything outside /api is a static asset - would otherwise serve it to
	// anybody who asked.
	isCatalogue := func(path string) bool {
		return path == "/opds" || strings.HasPrefix(path, "/opds/")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, plain := gatePath(r)

		// Everything outside /api is the web client itself: static assets and
		// the app shell, which must load in order to show a login screen.
		//
		// Only a plain path is ever let through unauthenticated. Anything the
		// router could read differently from this check needs a session.
		if plain && ((!strings.HasPrefix(p, "/api/") && !isCatalogue(p)) ||
			isOpen(r.Method, p) || isAuthRedirect(r.Method, p)) {
			// Attach the user when there is one, so an open endpoint can still
			// personalise itself.
			if session, err := sessions.Validate(r.Context(), r); err == nil {
				if id, err := uuid.Parse(session.UserID); err == nil {
					r = r.WithContext(context.WithValue(r.Context(), userContextKey{}, id))
				}
			}
			next.ServeHTTP(w, r)
			return
		}

		// An earlier middleware may already have authenticated this request.
		//
		// Header authentication issues its session by SETTING a cookie on the
		// response, so the incoming request does not carry one yet. Re-validating
		// from the cookie would therefore reject the very request that
		// established the session - which is every first request behind an SSO
		// proxy.
		if id, ok := UserFrom(r.Context()); ok {
			if authorize(w, r, policy, id) {
				next.ServeHTTP(w, r)
			}
			return
		}

		session, err := sessions.Validate(r.Context(), r)
		if err != nil {
			// A reader app has no session to present and no way to get one:
			// it knows a username and a password and nothing else. Tried only
			// after the session, so the ordinary path costs nothing.
			if id, ok := basicUser(r, basic); ok {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey{}, id))
				if authorize(w, r, policy, id) {
					next.ServeHTTP(w, r)
				}
				return
			}
			// Tell a reader app HOW to sign in. Without this header a
			// refusal is an error it cannot act on; with it, the app shows
			// a password box. A 401 with no challenge is why "the catalogue
			// does not work" is the commonest thing said about these.
			if isCatalogue(p) {
				w.Header().Set("WWW-Authenticate", `Basic realm="Lexarii"`)
			}
			respondError(w, &httpError{
				status:  http.StatusUnauthorized,
				code:    "not_signed_in",
				message: "sign in to continue",
			})
			return
		}

		id, err := uuid.Parse(session.UserID)
		if err != nil {
			respondError(w, &httpError{
				status:  http.StatusUnauthorized,
				code:    "not_signed_in",
				message: "sign in to continue",
			})
			return
		}

		ctx := context.WithValue(r.Context(), userContextKey{}, id)
		ctx = context.WithValue(ctx, sessionContextKey{}, session.ID)
		r = r.WithContext(ctx)
		if authorize(w, r, policy, id) {
			next.ServeHTTP(w, r)
		}
	})
}

// gatePath returns the path RequireAuth classifies a request by, and whether
// that path is plain enough to be classified as open at all.
//
// The router matches the ESCAPED path, cleaned, and unescapes it one segment
// at a time: "%2F" stays inside its segment and "%2E%2E" is a segment named
// "..", not a step up. r.URL.Path has already decoded both. Deciding on the
// decoded path therefore let "/api/v1/auth/sessions/x%2Fstart" look like a
// federated sign-in route while the router sent it to a session's id. So the
// decision is made on what the router sees, and a path is only plain when
// both readings must agree: no escape the URL had to keep (RawPath), no
// percent sign at all, no backslash, and no dot or empty segment. Every web
// client route and every open endpoint is plain; something that is not is
// never open, and simply needs a session like any other request.
func gatePath(r *http.Request) (string, bool) {
	escaped := r.URL.EscapedPath()
	plain := r.URL.RawPath == "" &&
		strings.HasPrefix(escaped, "/") &&
		!strings.ContainsAny(escaped, `%\`)
	if plain {
		segments := strings.Split(escaped[1:], "/")
		for i, s := range segments {
			// A trailing slash leaves one empty segment at the end, which is
			// harmless; an empty one anywhere else is a doubled slash.
			if s == "." || s == ".." || (s == "" && i != len(segments)-1) {
				plain = false
				break
			}
		}
	}
	return path.Clean(escaped), plain
}

func (h *AuthHandler) userDTO(u postgres.UserRecord) dto.User {
	return dto.User{
		ID:        u.ID.String(),
		Username:  u.Username,
		Email:     u.Email,
		IsAdmin:   u.IsAdmin,
		CanUpload: u.IsAdmin || h.AnyoneMayUpload,
		CanAsk:    h.ChatConfigured,
		// What GET /people does for them: administrators always get the
		// list.
		CanListPeople: u.IsAdmin || !h.PeopleUnlisted,
		CreatedAt:     u.CreatedAt,
	}
}

func (h *AuthHandler) logger() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

// clientAddr reports the address a sign-in should be counted against.
//
// The socket address, EXCEPT when the request came from one of this
// installation's own proxies, in which case the client is read off the right
// of X-Forwarded-For. An attacker-supplied header must never be believed - it
// would let them rotate the throttle key on every attempt and defeat the
// limit entirely - which is why only entries appended by a trusted hop count,
// and why an installation with nothing configured in front of it keeps using
// the socket address.
//
// Without this, every request behind nginx arrives from 127.0.0.1 and the
// whole household shares one bucket.
func (h *AuthHandler) clientAddr(r *http.Request) string {
	return auth.ClientIP(r, h.Trusted)
}

// oidcStateCookie carries the CSRF state, and the nonce the identity must
// carry back, between the redirect and the callback: "<state>.<nonce>", both
// base64url, which has no dot in its alphabet.
const oidcStateCookie = "lexarii_auth_state"

// splitAuthState reads the state cookie. A value without both halves - one
// set before the nonce existed, or tampered with - is no sign-in in progress.
func splitAuthState(value string) (state, nonce string, ok bool) {
	state, nonce, ok = strings.Cut(value, ".")
	return state, nonce, ok && state != "" && nonce != ""
}

// StartRedirect serves GET /api/v1/auth/{provider}/start.
//
// State is generated here, stored in a short-lived cookie, and verified on the
// way back. Without that check the callback is forgeable: an attacker can
// complete the flow with their own code and silently log the victim into the
// ATTACKER's account, where everything the victim then reads or annotates
// belongs to them.
func (h *AuthHandler) StartRedirect(w http.ResponseWriter, r *http.Request) {
	provider, err := h.Providers.Get(r.PathValue("provider"))
	if err != nil {
		respondError(w, NotFound("sign-in method"))
		return
	}

	redirect, ok := provider.(auth.RedirectProvider)
	if !ok {
		respondError(w, Invalid("that sign-in method does not use a redirect", nil))
		return
	}

	state, err := auth.NewState()
	if err != nil {
		respondError(w, err)
		return
	}
	// A second value, never the state: the state comes back in the callback's
	// URL, while the nonce comes back only inside the signed ID token, which
	// is what ties that token to this browser's sign-in.
	nonce, err := auth.NewState()
	if err != nil {
		respondError(w, err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookie,
		Value:    state + "." + nonce,
		Path:     "/api/v1/auth/",
		HttpOnly: true,
		// Lax, not Strict: the callback is a cross-site navigation from the
		// identity provider, and Strict would drop the cookie exactly when it is
		// needed, breaking every sign-in.
		SameSite: http.SameSiteLaxMode,
		Secure:   h.Sessions.Secure,
		MaxAge:   600,
	})

	http.Redirect(w, r, redirect.AuthURL(state, nonce, h.callbackURL(r, provider.Name())), http.StatusFound)
}

// Callback serves GET /api/v1/auth/{provider}/callback.
func (h *AuthHandler) Callback(w http.ResponseWriter, r *http.Request) {
	provider, err := h.Providers.Get(r.PathValue("provider"))
	if err != nil {
		respondError(w, NotFound("sign-in method"))
		return
	}

	redirect, ok := provider.(auth.RedirectProvider)
	if !ok {
		respondError(w, Invalid("that sign-in method does not use a redirect", nil))
		return
	}

	// Verify state before touching the code. A callback that fails this is
	// either a stale tab or an attempt to log someone into another account.
	cookie, err := r.Cookie(oidcStateCookie)
	if err != nil || cookie.Value == "" {
		h.failRedirect(w, r, "the sign-in attempt expired; please try again")
		return
	}
	state, nonce, ok := splitAuthState(cookie.Value)
	if !ok {
		h.failRedirect(w, r, "the sign-in attempt expired; please try again")
		return
	}
	if !auth.ConstantTimeEqualString(state, r.URL.Query().Get("state")) {
		h.logger().Warn("rejected a sign-in callback with mismatched state", "provider", provider.Name())
		h.failRedirect(w, r, "that sign-in could not be verified")
		return
	}
	// One use only.
	http.SetCookie(w, &http.Cookie{
		Name: oidcStateCookie, Value: "", Path: "/api/v1/auth/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: h.Sessions.Secure, MaxAge: -1,
	})

	if errCode := r.URL.Query().Get("error"); errCode != "" {
		h.logger().Info("the identity provider refused a sign-in", "provider", provider.Name(), "error", errCode)
		h.failRedirect(w, r, "your identity provider refused that sign-in")
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		h.failRedirect(w, r, "that sign-in returned no authorisation code")
		return
	}

	identity, err := redirect.Exchange(r.Context(), code, nonce, h.callbackURL(r, provider.Name()))
	if err != nil {
		h.logger().Warn("token exchange failed", "provider", provider.Name(), "error", err)
		h.failRedirect(w, r, "that sign-in could not be completed")
		return
	}

	allowCreate := false
	if s, ok := provider.(interface{ AllowSignup() bool }); ok {
		allowCreate = s.AllowSignup()
	}

	userID, identityID, err := h.Store.UpsertIdentity(r.Context(), identity, allowCreate)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			h.failRedirect(w, r, "you signed in successfully, but there is no account here for you")
			return
		}
		respondError(w, err)
		return
	}

	sessionCookie, _, err := h.Sessions.Issue(r.Context(), r, userID.String(), identityID.String())
	if err != nil {
		respondError(w, err)
		return
	}
	http.SetCookie(w, sessionCookie)

	http.Redirect(w, r, "/", http.StatusFound)
}

// callbackURL rebuilds the redirect URI.
//
// It must match what was sent to the provider byte for byte, or the exchange is
// rejected. X-Forwarded-* is honoured here, and only here, because behind a
// reverse proxy the request's own scheme and host are the proxy's, not the ones
// the browser used and the provider was told about.
func (h *AuthHandler) callbackURL(r *http.Request, provider string) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		scheme = forwarded
	}

	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}

	return scheme + "://" + host + "/api/v1/auth/" + provider + "/callback"
}

// failRedirect sends the browser back to the sign-in page with a message,
// rather than returning JSON to what is a full-page navigation.
func (h *AuthHandler) failRedirect(w http.ResponseWriter, r *http.Request, message string) {
	http.Redirect(w, r, "/signin?error="+url.QueryEscape(message), http.StatusFound)
}

// basicUser resolves the Basic credentials on a request, if there are any and
// if anything is configured to check them.
func basicUser(r *http.Request, basic BasicAuthenticator) (uuid.UUID, bool) {
	if basic == nil {
		return uuid.Nil, false
	}
	username, password, ok := r.BasicAuth()
	if !ok {
		return uuid.Nil, false
	}
	id, err := basic.AuthenticateBasic(r.Context(), r, username, password)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}
