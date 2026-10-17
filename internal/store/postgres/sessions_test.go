// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// sessionUser returns a user with no sessions attached.
func sessionUser(t *testing.T, p *Pool) string {
	t.Helper()
	cleanLibrary(t, p)

	userID, err := p.EnsureDefaultUser(t.Context())
	if err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}
	if _, err := p.Exec(t.Context(), `DELETE FROM sessions WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("clearing sessions: %v", err)
	}
	return userID.String()
}

func newSession(t *testing.T, p *Pool, userID, tokenHash, agent, ip string, lifetime time.Duration) string {
	t.Helper()
	s, err := p.CreateSession(t.Context(), tokenHash, userID, "", agent, ip, "", time.Now().Add(lifetime))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return s.ID
}

// expireSession ages a session so that it expired `ago` in the past.
//
// The schema requires expires_at > created_at, so the session is aged rather
// than just having its expiry pulled backwards: an expired row is one that was
// issued long ago, not one that was born already dead. Nothing should ever be
// able to issue a dead credential, and the constraint says so.
func expireSession(t *testing.T, p *Pool, id string, ago time.Duration) {
	t.Helper()
	if _, err := p.Exec(t.Context(), `
		UPDATE sessions
		SET created_at   = now() - $2::interval - interval '1 day',
		    expires_at   = now() - $2::interval,
		    last_used_at = now() - $2::interval
		WHERE id = $1`, id, ago.String()); err != nil {
		t.Fatalf("expiring session: %v", err)
	}
}

func TestUserSessionsListsLiveSignIns(t *testing.T) {
	p := testPool(t)
	userID := sessionUser(t, p)

	newSession(t, p, userID, "hash-phone", "Firefox on Android", "192.0.2.10", time.Hour)
	newSession(t, p, userID, "hash-laptop", "Firefox on Linux", "192.0.2.11", time.Hour)

	sessions, err := p.UserSessions(t.Context(), userID)
	if err != nil {
		t.Fatalf("UserSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}

	agents := map[string]string{}
	for _, s := range sessions {
		agents[s.UserAgent] = s.ClientIP
	}
	if agents["Firefox on Android"] != "192.0.2.10" {
		t.Errorf("the phone session lost its address: %v", agents)
	}
	if agents["Firefox on Linux"] != "192.0.2.11" {
		t.Errorf("the laptop session lost its address: %v", agents)
	}
}

// The list answers "what can get into my account right now?". A revoked or
// expired row is not an answer to that and only makes the real ones harder to
// find.
func TestUserSessionsExcludesDeadOnes(t *testing.T) {
	p := testPool(t)
	userID := sessionUser(t, p)

	live := newSession(t, p, userID, "hash-live", "Live", "192.0.2.10", time.Hour)
	revoked := newSession(t, p, userID, "hash-revoked", "Revoked", "192.0.2.11", time.Hour)
	expired := newSession(t, p, userID, "hash-expired", "Expired", "192.0.2.12", time.Hour)
	expireSession(t, p, expired, time.Hour)

	if err := p.RevokeSessionByID(t.Context(), userID, revoked); err != nil {
		t.Fatalf("RevokeSessionByID: %v", err)
	}

	sessions, err := p.UserSessions(t.Context(), userID)
	if err != nil {
		t.Fatalf("UserSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want only the live one", len(sessions))
	}
	if sessions[0].ID.String() != live {
		t.Errorf("listed %s, want the live session %s", sessions[0].ID, live)
	}
	_ = expired
}

func TestUserSessionsIsPerUser(t *testing.T) {
	p := testPool(t)
	userID := sessionUser(t, p)
	newSession(t, p, userID, "hash-mine", "Mine", "192.0.2.10", time.Hour)

	other, err := p.UserSessions(t.Context(), uuid.New().String())
	if err != nil {
		t.Fatalf("UserSessions: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("a stranger saw %d sessions", len(other))
	}
}

// A guessed id must not sign someone else out.
func TestRevokeSessionByIDIsScopedToTheOwner(t *testing.T) {
	p := testPool(t)
	userID := sessionUser(t, p)
	id := newSession(t, p, userID, "hash-target", "Target", "192.0.2.10", time.Hour)

	err := p.RevokeSessionByID(t.Context(), uuid.New().String(), id)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a stranger's revoke returned %v, want ErrNotFound", err)
	}
	sessions, err := p.UserSessions(t.Context(), userID)
	if err != nil {
		t.Fatalf("UserSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatal("a stranger revoked someone else's session")
	}

	if err := p.RevokeSessionByID(t.Context(), userID, id); err != nil {
		t.Fatalf("the owner's revoke: %v", err)
	}
	// Revoking twice is not found, so a double-click reports honestly rather
	// than claiming to have ended something that was already over.
	if err := p.RevokeSessionByID(t.Context(), userID, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoking twice returned %v, want ErrNotFound", err)
	}
}

// A revoked session must stop authenticating immediately, or the screen is
// decoration.
func TestRevokedSessionStopsValidating(t *testing.T) {
	p := testPool(t)
	userID := sessionUser(t, p)
	id := newSession(t, p, userID, "hash-revoke-me", "Doomed", "192.0.2.10", time.Hour)

	if _, err := p.SessionByTokenHash(t.Context(), "hash-revoke-me"); err != nil {
		t.Fatalf("the session did not validate before being revoked: %v", err)
	}
	if err := p.RevokeSessionByID(t.Context(), userID, id); err != nil {
		t.Fatalf("RevokeSessionByID: %v", err)
	}
	if _, err := p.SessionByTokenHash(t.Context(), "hash-revoke-me"); err == nil {
		t.Fatal("a revoked session still authenticates")
	}
}

func TestRevokeSessionByIDRejectsMalformedIDs(t *testing.T) {
	p := testPool(t)
	userID := sessionUser(t, p)

	if err := p.RevokeSessionByID(t.Context(), userID, "not-a-uuid"); err == nil {
		t.Error("a malformed session id was accepted")
	}
	if err := p.RevokeSessionByID(t.Context(), "not-a-uuid", uuid.New().String()); err == nil {
		t.Error("a malformed user id was accepted")
	}
}

// The sweeper is housekeeping: it removes rows that can no longer be used and
// leaves live ones alone.
func TestPurgeExpiredSessionsKeepsLiveOnes(t *testing.T) {
	p := testPool(t)
	userID := sessionUser(t, p)

	newSession(t, p, userID, "hash-still-good", "Live", "192.0.2.10", time.Hour)
	ancient := newSession(t, p, userID, "hash-long-gone", "Ancient", "192.0.2.11", time.Hour)
	expireSession(t, p, ancient, 30*24*time.Hour)

	removed, err := p.PurgeExpiredSessions(t.Context(), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeExpiredSessions: %v", err)
	}
	if removed != 1 {
		t.Errorf("purged %d sessions, want 1", removed)
	}

	sessions, err := p.UserSessions(t.Context(), userID)
	if err != nil {
		t.Fatalf("UserSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("got %d live sessions after the purge, want 1", len(sessions))
	}
}

// Retention is why a session that expired an hour ago is still there: it is
// evidence for someone reviewing their own devices, and deleting it instantly
// buys nothing, since an expired row cannot authenticate anything.
func TestPurgeExpiredSessionsRespectsRetention(t *testing.T) {
	p := testPool(t)
	userID := sessionUser(t, p)

	recent := newSession(t, p, userID, "hash-just-expired", "Recent", "192.0.2.10", time.Hour)
	expireSession(t, p, recent, time.Hour)

	removed, err := p.PurgeExpiredSessions(t.Context(), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeExpiredSessions: %v", err)
	}
	if removed != 0 {
		t.Errorf("purged %d sessions, want 0 while inside the retention window", removed)
	}
}
