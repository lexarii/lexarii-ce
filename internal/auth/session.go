// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// SessionCookie is the cookie a browser session is carried in.
const SessionCookie = "lexarii_session"

// SessionLifetime is how long a session lasts without use.
const SessionLifetime = 30 * 24 * time.Hour

// SessionStore persists sessions.
type SessionStore interface {
	// CreateSession stores a session keyed by the HASH of the token. The plain
	// token is never persisted. A device names the client a bearer token was
	// issued to, and is empty for a browser.
	CreateSession(ctx context.Context, tokenHash, userID, identityID, userAgent, clientIP, device string, expiresAt time.Time) (Session, error)
	// SessionByTokenHash returns a live session, or ErrSessionInvalid.
	SessionByTokenHash(ctx context.Context, tokenHash string) (Session, error)
	// RevokeSession marks one session unusable.
	RevokeSession(ctx context.Context, tokenHash string) error
	// RevokeAllSessions signs a user out everywhere.
	RevokeAllSessions(ctx context.Context, userID string) error
}

// Sessions issues and validates browser sessions.
type Sessions struct {
	Store SessionStore
	// Secure marks cookies Secure. On by default; it can only be disabled for
	// plain-HTTP access on a trusted network, and doing so is logged.
	Secure bool
	// Lifetime defaults to SessionLifetime.
	Lifetime time.Duration
}

// BearerPrefix is the scheme a client sends its token under.
const BearerPrefix = "Bearer "

// Issue creates a browser session and returns the cookie to set.
//
// The token is returned to the browser once and stored only as a SHA-256 hash.
// A database backup, a log line, or a stray SELECT therefore cannot be replayed
// as a login - the same reasoning as never storing a password in plaintext.
func (s *Sessions) Issue(ctx context.Context, r *http.Request, userID, identityID string) (*http.Cookie, Session, error) {
	token, session, err := s.issue(ctx, r, userID, identityID, "")
	if err != nil {
		return nil, Session{}, err
	}

	lifetime := s.lifetime()
	return &http.Cookie{
		Name:  SessionCookie,
		Value: token,
		Path:  "/",
		// HttpOnly: a session token must not be readable by JavaScript, which is
		// what turns any content-injection bug into account takeover.
		HttpOnly: true,
		// Lax rather than Strict: Strict would drop the cookie when arriving from
		// an external link, so a shared book URL would bounce the reader to a
		// login page they do not need.
		SameSite: http.SameSiteLaxMode,
		Secure:   s.Secure,
		Expires:  session.ExpiresAt,
		MaxAge:   int(lifetime.Seconds()),
	}, session, nil
}

// IssueToken creates a session for a client that holds its own token.
//
// The token is RETURNED rather than set as a cookie, which is the whole
// difference. A native client has no cookie jar worth relying on and no
// same-origin to be protected by; it stores the token itself, sends it as
// `Authorization: Bearer`, and is responsible for keeping it secret.
//
// That is a meaningfully weaker position than a browser session, where
// HttpOnly keeps the token out of reach of any script on the page. It is the
// right trade for a client on a phone and the wrong one for a web page, which
// is why this is a separate call on a separate endpoint rather than a flag on
// the browser's login: no web client can be made to take a token by accident.
//
// The device is the name the person gave the phone, so they can find it in
// their session list a year later and revoke that one.
func (s *Sessions) IssueToken(ctx context.Context, r *http.Request, userID, identityID, device string) (string, Session, error) {
	return s.issue(ctx, r, userID, identityID, device)
}

func (s *Sessions) issue(ctx context.Context, r *http.Request, userID, identityID, device string) (string, Session, error) {
	token, err := newToken()
	if err != nil {
		return "", Session{}, err
	}

	expires := time.Now().Add(s.lifetime())
	session, err := s.Store.CreateSession(ctx, HashToken(token), userID, identityID,
		r.UserAgent(), clientIP(r), device, expires)
	if err != nil {
		return "", Session{}, fmt.Errorf("creating session: %w", err)
	}
	return token, session, nil
}

func (s *Sessions) lifetime() time.Duration {
	if s.Lifetime <= 0 {
		return SessionLifetime
	}
	return s.Lifetime
}

// Validate resolves the session a request carries.
//
// A bearer token wins over a cookie. They are the same kind of token against
// the same table, so this is not two authentication schemes - it is one
// scheme with two ways of carrying it - but an explicit Authorization header
// is a deliberate act by the caller, while a cookie is attached by the
// browser whether or not anybody meant it.
func (s *Sessions) Validate(ctx context.Context, r *http.Request) (Session, error) {
	token := tokenIn(r)
	if token == "" {
		return Session{}, ErrSessionInvalid
	}
	return s.Store.SessionByTokenHash(ctx, HashToken(token))
}

// ValidateToken resolves a session from a bare token.
//
// For the places a token arrives somewhere other than the Authorization
// header - an OPDS reader that only knows how to send a username and a
// password, and is given a device token as the password.
func (s *Sessions) ValidateToken(ctx context.Context, token string) (Session, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Session{}, ErrSessionInvalid
	}
	return s.Store.SessionByTokenHash(ctx, HashToken(token))
}

// Revoke ends the session a request carries, however it was carried.
func (s *Sessions) Revoke(ctx context.Context, r *http.Request) error {
	token := tokenIn(r)
	if token == "" {
		return nil
	}
	return s.Store.RevokeSession(ctx, HashToken(token))
}

// tokenIn finds the session token on a request.
func tokenIn(r *http.Request) string {
	if header := r.Header.Get("Authorization"); len(header) > len(BearerPrefix) {
		// Case-insensitive on the scheme, which RFC 7235 requires and clients
		// get wrong in both directions.
		if strings.EqualFold(header[:len(BearerPrefix)], BearerPrefix) {
			return strings.TrimSpace(header[len(BearerPrefix):])
		}
	}
	if cookie, err := r.Cookie(SessionCookie); err == nil {
		return cookie.Value
	}
	return ""
}

// ClearCookie returns a cookie that removes the session cookie.
func (s *Sessions) ClearCookie() *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.Secure,
		MaxAge:   -1,
	}
}

// newToken generates a session token with 256 bits of entropy.
func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// HashToken hashes a session token for storage.
//
// A plain SHA-256 rather than a password hash: the token already has 256 bits
// of entropy from a CSPRNG, so there is nothing to brute-force and nothing for
// argon2 to slow down. Using a slow hash here would only make every request
// expensive.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// clientIP reports the address a request came from.
//
// The socket address only. X-Forwarded-For is attacker-controlled unless a
// trusted proxy is known to rewrite it, and this value is shown to a person
// reviewing their own sessions, where a spoofed entry would be worse than none.
//
// net.SplitHostPort rather than splitting on the last colon: an IPv6 address is
// full of colons and arrives bracketed, so the hand-rolled version yielded
// "[::1]" and Postgres rejected it as an invalid inet. Every local connection
// over IPv6 failed to create a session.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// SplitHostPort strips the brackets, but a bare RemoteAddr may still carry
	// them.
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")

	// Only store something Postgres will accept as an inet.
	if net.ParseIP(host) == nil {
		return ""
	}
	return host
}

// SweepInterval is how often expired sessions are purged.
const SweepInterval = 6 * time.Hour

// SweepRetention is how long a dead session is kept before deletion.
//
// Not zero. A session that expired an hour ago is evidence: it is what someone
// reviewing their own devices sees when they ask "what signed in from there?".
// Deleting it the instant it expires throws that away for no benefit, since an
// expired row cannot authenticate anything - SessionByTokenHash filters on
// expires_at, and the sweep is housekeeping, not a security control.
const SweepRetention = 7 * 24 * time.Hour

// SessionSweeper deletes sessions that can no longer be used.
type SessionSweeper interface {
	PurgeExpiredSessions(ctx context.Context, olderThan time.Duration) (int64, error)
}

// SweepSessions purges dead sessions periodically until ctx is cancelled.
//
// Housekeeping, not enforcement: an expired session is already refused at
// validation. Without this the table grows without bound on a long-lived
// instance, which is a slow leak rather than a hole.
//
// It sweeps once on startup, because an instance that is restarted more often
// than the interval would otherwise never sweep at all.
func SweepSessions(ctx context.Context, store SessionSweeper, interval, retention time.Duration, log *slog.Logger) {
	if interval <= 0 {
		interval = SweepInterval
	}
	if retention <= 0 {
		retention = SweepRetention
	}
	if log == nil {
		log = slog.Default()
	}

	sweep := func() {
		// Bounded independently of ctx's lifetime so a slow delete cannot wedge
		// the loop, and detached from any request.
		runCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()

		removed, err := store.PurgeExpiredSessions(runCtx, retention)
		if err != nil {
			// Logged, never fatal. Failing to tidy up must not take the server
			// down; the next tick tries again.
			log.Warn("sweeping expired sessions", "error", err)
			return
		}
		if removed > 0 {
			log.Info("swept expired sessions", "removed", removed)
		}
	}

	sweep()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
