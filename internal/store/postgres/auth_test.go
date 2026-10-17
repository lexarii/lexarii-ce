// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// clearIdentitiesForAuthTest removes every identity, so questions about the
// whole installation ("can anybody sign in?", "is there a placeholder to
// adopt?") start from a known answer. Accounts made during the test are removed
// again afterwards by freshUsername.
func clearIdentitiesForAuthTest(t *testing.T, p *Pool) {
	t.Helper()
	if _, err := p.Exec(t.Context(), `DELETE FROM user_identities`); err != nil {
		t.Fatalf("clearing identities: %v", err)
	}
}

// removeUserAfterAuthTest deletes an account by id when the test ends.
func removeUserAfterAuthTest(t *testing.T, p *Pool, id uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := p.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id); err != nil {
			t.Logf("removing %s: %v", id, err)
		}
	})
}

func TestUpsertIdentity(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()

	t.Run("a stranger is refused when the provider may not create accounts", func(t *testing.T) {
		subject := "sub-" + uuid.NewString()
		_, _, err := p.UpsertIdentity(ctx, auth.Identity{Provider: "oidc", Subject: subject}, false)
		if !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Fatalf("UpsertIdentity: %v, want auth.ErrInvalidCredentials", err)
		}
		if n := rowCount(t, p, `SELECT count(*) FROM user_identities WHERE provider_user_id = $1`, subject); n != 0 {
			t.Errorf("a refused sign-in left %d identities behind", n)
		}
	})

	t.Run("first sign-in creates the account, later ones land on it", func(t *testing.T) {
		name := freshUsername(t, p)
		subject := "Sub-" + uuid.NewString()
		first := auth.Identity{
			Provider:    "oidc",
			Subject:     subject,
			Email:       "reader@example.org",
			DisplayName: name,
			Groups:      []string{"readers"},
			Data:        map[string]any{"iss": "https://idp.example.org"},
		}
		userID, identityID, err := p.UpsertIdentity(ctx, first, true)
		if err != nil {
			t.Fatalf("UpsertIdentity (create): %v", err)
		}

		u, err := p.User(ctx, userID)
		if err != nil {
			t.Fatalf("User: %v", err)
		}
		if u.Username != name || u.Email != "reader@example.org" || u.IsAdmin {
			t.Errorf("created account = %+v, want username %q, the email, and no admin", u, name)
		}

		var raw []byte
		if err := p.QueryRow(ctx, `SELECT provider_data FROM user_identities WHERE id = $1`, identityID).Scan(&raw); err != nil {
			t.Fatalf("reading provider data: %v", err)
		}
		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			t.Fatalf("provider data is not JSON: %v", err)
		}
		if data["iss"] != "https://idp.example.org" {
			t.Errorf("provider data lost the claims: %v", data)
		}
		if groups, _ := data["groups"].([]any); len(groups) != 1 || groups[0] != "readers" {
			t.Errorf("provider data lost the groups: %v", data)
		}

		// The subject matches case-insensitively, and a sign-in that says less
		// does not erase what is known.
		again, againIdentity, err := p.UpsertIdentity(ctx, auth.Identity{
			Provider: "oidc", Subject: strings.ToLower(subject), DisplayName: "Renamed Reader",
		}, false)
		if err != nil {
			t.Fatalf("UpsertIdentity (return): %v", err)
		}
		if again != userID || againIdentity != identityID {
			t.Fatalf("second sign-in landed on %s/%s, want %s/%s", again, againIdentity, userID, identityID)
		}
		var email, display string
		var lastLogin *time.Time
		if err := p.QueryRow(ctx, `
			SELECT email, display_name, last_login_at FROM user_identities WHERE id = $1`, identityID,
		).Scan(&email, &display, &lastLogin); err != nil {
			t.Fatalf("reading identity: %v", err)
		}
		if email != "reader@example.org" {
			t.Errorf("email = %q, an empty claim must not erase it", email)
		}
		if display != "Renamed Reader" {
			t.Errorf("display name = %q, want the refreshed one", display)
		}
		if lastLogin == nil {
			t.Error("last_login_at was not recorded")
		}
	})

	t.Run("without a display name the subject names the account", func(t *testing.T) {
		subject := freshUsername(t, p)
		userID, _, err := p.UpsertIdentity(ctx, auth.Identity{Provider: "ldap", Subject: subject}, true)
		if err != nil {
			t.Fatalf("UpsertIdentity: %v", err)
		}
		u, err := p.User(ctx, userID)
		if err != nil {
			t.Fatalf("User: %v", err)
		}
		if u.Username != subject || u.Email != "" {
			t.Errorf("account = %+v, want username %q and no email", u, subject)
		}
	})

	t.Run("a username collision is a conflict, and nothing is half made", func(t *testing.T) {
		name := freshUsername(t, p)
		if _, err := p.CreateLocalUser(ctx, name, "", localUserPassword, false); err != nil {
			t.Fatalf("CreateLocalUser: %v", err)
		}
		subject := "sub-" + uuid.NewString()
		_, _, err := p.UpsertIdentity(ctx, auth.Identity{Provider: "oidc", Subject: subject, DisplayName: name}, true)
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("UpsertIdentity: %v, want store.ErrConflict", err)
		}
		if n := rowCount(t, p, `SELECT count(*) FROM user_identities WHERE provider_user_id = $1`, subject); n != 0 {
			t.Errorf("the refused sign-in left %d identities", n)
		}
	})
}

func TestUniqueUsername(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"trimmed", "  Ada  ", "Ada"},
		{"blank becomes a placeholder", "   ", "user"},
		{"long names are cut to 100 bytes", strings.Repeat("x", 150), strings.Repeat("x", 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := uniqueUsername(tc.in); got != tc.want {
				t.Errorf("uniqueUsername(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIdentityDataWithNothingToKeepIsAnEmptyObject(t *testing.T) {
	if got := string(identityData(auth.Identity{Provider: "proxy", Subject: "x"})); got != "{}" {
		t.Errorf("identityData = %s, want {}", got)
	}
}

func TestUserReadsAnAccount(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	name := freshUsername(t, p)

	id, err := p.CreateLocalUser(ctx, name, "admin@example.org", localUserPassword, true)
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	u, err := p.User(ctx, id)
	if err != nil {
		t.Fatalf("User: %v", err)
	}
	if u.ID != id || u.Username != name || u.Email != "admin@example.org" || !u.IsAdmin || u.CreatedAt.IsZero() {
		t.Errorf("User = %+v", u)
	}

	if _, err := p.User(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("User(unknown) = %v, want store.ErrNotFound", err)
	}
}

func TestCreateLocalUserRefusesAWeakPassword(t *testing.T) {
	p := testPool(t)
	name := freshUsername(t, p)
	if _, err := p.CreateLocalUser(t.Context(), name, "", "short", false); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("CreateLocalUser: %v, want auth.ErrWeakPassword", err)
	}
	if n := userCount(t, p, name); n != 0 {
		t.Errorf("a refused password still made %d accounts", n)
	}
}

// A password change signs the person out everywhere: it is usually a response
// to a compromise.
func TestSetLocalPassword(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	name := freshUsername(t, p)

	id, err := p.CreateLocalUser(ctx, name, "", localUserPassword, false)
	if err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	newSession(t, p, id.String(), "hash-pw-a-"+name, "A", "", time.Hour)
	newSession(t, p, id.String(), "hash-pw-b-"+name, "B", "", time.Hour)

	if err := p.SetLocalPassword(ctx, id, "short"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password: %v, want auth.ErrWeakPassword", err)
	}
	if _, err := p.SessionByTokenHash(ctx, "hash-pw-a-"+name); err != nil {
		t.Fatalf("a refused change signed the person out: %v", err)
	}

	const replacement = "an entirely different passphrase"
	if err := p.SetLocalPassword(ctx, id, replacement); err != nil {
		t.Fatalf("SetLocalPassword: %v", err)
	}
	_, hash, err := p.LocalIdentity(ctx, name)
	if err != nil {
		t.Fatalf("LocalIdentity: %v", err)
	}
	if !auth.VerifyPassword(replacement, hash) || auth.VerifyPassword(localUserPassword, hash) {
		t.Error("the stored password is not the new one")
	}
	for _, token := range []string{"hash-pw-a-" + name, "hash-pw-b-" + name} {
		if _, err := p.SessionByTokenHash(ctx, token); !errors.Is(err, auth.ErrSessionInvalid) {
			t.Errorf("session %s after a password change: %v, want auth.ErrSessionInvalid", token, err)
		}
	}

	t.Run("an account with no local identity has no password to set", func(t *testing.T) {
		stranger := secondUser(t, p)
		removeUserAfterAuthTest(t, p, stranger)
		if err := p.SetLocalPassword(ctx, stranger, replacement); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("SetLocalPassword: %v, want store.ErrNotFound", err)
		}
	})
}

// "Can anybody sign in?" is about usable credentials, not user rows.
func TestHasAnyUserCountsOnlyUsableCredentials(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	clearIdentitiesForAuthTest(t, p)

	has := func() bool {
		t.Helper()
		ok, err := p.HasAnyUser(ctx)
		if err != nil {
			t.Fatalf("HasAnyUser: %v", err)
		}
		return ok
	}

	if _, err := p.EnsureDefaultUser(ctx); err != nil {
		t.Fatalf("EnsureDefaultUser: %v", err)
	}
	if has() {
		t.Fatal("a user row with no identity counted as somebody who can sign in")
	}

	placeholder := secondUser(t, p)
	removeUserAfterAuthTest(t, p, placeholder)
	if _, err := p.Exec(ctx, `
		INSERT INTO user_identities (user_id, provider, provider_user_id, secret)
		VALUES ($1, 'local', $2, '')`, placeholder, "placeholder-"+uuid.NewString()[:8]); err != nil {
		t.Fatalf("inserting a placeholder identity: %v", err)
	}
	if has() {
		t.Fatal("a local identity with no password counted as usable")
	}

	if _, _, err := p.UpsertIdentity(ctx, auth.Identity{Provider: "oidc", Subject: freshUsername(t, p)}, true); err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	if !has() {
		t.Fatal("a federated identity did not count: the provider holds its credential")
	}

	clearIdentitiesForAuthTest(t, p)
	if _, err := p.CreateLocalUser(ctx, freshUsername(t, p), "", localUserPassword, true); err != nil {
		t.Fatalf("CreateLocalUser: %v", err)
	}
	if !has() {
		t.Fatal("a local identity with a password did not count")
	}
}

func TestAdoptOrCreateLocalUser(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()

	t.Run("a fresh installation gets a new account", func(t *testing.T) {
		clearIdentitiesForAuthTest(t, p)
		name := freshUsername(t, p)
		id, err := p.AdoptOrCreateLocalUser(ctx, "  "+name+"  ", "owner@example.org", localUserPassword, true)
		if err != nil {
			t.Fatalf("AdoptOrCreateLocalUser: %v", err)
		}
		u, err := p.User(ctx, id)
		if err != nil {
			t.Fatalf("User: %v", err)
		}
		if u.Username != name || !u.IsAdmin || u.Email != "owner@example.org" {
			t.Errorf("account = %+v", u)
		}
		_, hash, err := p.LocalIdentity(ctx, name)
		if err != nil {
			t.Fatalf("LocalIdentity: %v", err)
		}
		if !auth.VerifyPassword(localUserPassword, hash) {
			t.Error("the new account cannot sign in with its password")
		}
	})

	t.Run("a credential-less placeholder is adopted, keeping what it owns", func(t *testing.T) {
		clearIdentitiesForAuthTest(t, p)
		placeholder := secondUser(t, p)
		removeUserAfterAuthTest(t, p, placeholder)
		if _, err := p.Exec(ctx, `
			INSERT INTO user_identities (user_id, provider, provider_user_id)
			VALUES ($1, 'local', 'old-placeholder')`, placeholder); err != nil {
			t.Fatalf("inserting a placeholder identity: %v", err)
		}
		if _, err := p.Exec(ctx, `INSERT INTO collections (user_id, name) VALUES ($1, 'Kept')`, placeholder); err != nil {
			t.Fatalf("giving the placeholder a collection: %v", err)
		}

		name := freshUsername(t, p)
		id, err := p.AdoptOrCreateLocalUser(ctx, name, "", localUserPassword, true)
		if err != nil {
			t.Fatalf("AdoptOrCreateLocalUser: %v", err)
		}
		if id != placeholder {
			t.Fatalf("got account %s, want the adopted placeholder %s", id, placeholder)
		}
		u, err := p.User(ctx, id)
		if err != nil {
			t.Fatalf("User: %v", err)
		}
		if u.Username != name || !u.IsAdmin {
			t.Errorf("adopted account = %+v, want username %q and admin", u, name)
		}
		_, hash, err := p.LocalIdentity(ctx, name)
		if err != nil {
			t.Fatalf("LocalIdentity under the new name: %v", err)
		}
		if !auth.VerifyPassword(localUserPassword, hash) {
			t.Error("the adopted account cannot sign in")
		}
		if n := rowCount(t, p, `SELECT count(*) FROM collections WHERE user_id = $1`, placeholder); n != 1 {
			t.Errorf("the placeholder's collection did not survive adoption (%d)", n)
		}
		if n := rowCount(t, p, `SELECT count(*) FROM user_identities WHERE user_id = $1`, placeholder); n != 1 {
			t.Errorf("adoption left %d identities, want the one adopted", n)
		}
	})

	t.Run("a weak password is refused before anything is written", func(t *testing.T) {
		name := freshUsername(t, p)
		if _, err := p.AdoptOrCreateLocalUser(ctx, name, "", "short", true); !errors.Is(err, auth.ErrWeakPassword) {
			t.Fatalf("AdoptOrCreateLocalUser: %v, want auth.ErrWeakPassword", err)
		}
		if n := userCount(t, p, name); n != 0 {
			t.Errorf("%d accounts made despite the refusal", n)
		}
	})

	t.Run("a taken name when creating is a conflict", func(t *testing.T) {
		clearIdentitiesForAuthTest(t, p)
		if _, err := p.EnsureDefaultUser(ctx); err != nil {
			t.Fatalf("EnsureDefaultUser: %v", err)
		}
		if _, err := p.AdoptOrCreateLocalUser(ctx, DefaultUsername, "", localUserPassword, true); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("AdoptOrCreateLocalUser: %v, want store.ErrConflict", err)
		}
	})
}

func TestCreateSessionWithAnIdentityAndADevice(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	name := freshUsername(t, p)

	userID, identityID, err := p.UpsertIdentity(ctx, auth.Identity{Provider: "oidc", Subject: name}, true)
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}
	token := "hash-device-" + name
	s, err := p.CreateSession(ctx, token, userID.String(), identityID.String(), "Lexarii Android", "", "Pixel", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if s.UserID != userID.String() || s.IdentityID != identityID.String() || s.ID == "" {
		t.Errorf("session = %+v", s)
	}

	got, err := p.SessionByTokenHash(ctx, token)
	if err != nil {
		t.Fatalf("SessionByTokenHash: %v", err)
	}
	if got.ID != s.ID || got.IdentityID != identityID.String() || got.UserID != userID.String() {
		t.Errorf("validated session = %+v, want %+v", got, s)
	}

	list, err := p.UserSessions(ctx, userID.String())
	if err != nil {
		t.Fatalf("UserSessions: %v", err)
	}
	if len(list) != 1 || list[0].Device != "Pixel" || list[0].Provider != "oidc" || list[0].ClientIP != "" {
		t.Errorf("UserSessions = %+v, want one oidc session on device Pixel with no address", list)
	}
}

func TestCreateSessionInputs(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	userID := sessionUser(t, p)
	expiry := time.Now().Add(time.Hour)

	t.Run("a malformed identity id is ignored, not stored", func(t *testing.T) {
		s, err := p.CreateSession(ctx, "hash-bad-identity", userID, "not-a-uuid", "", "", "", expiry)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if s.IdentityID != "" {
			t.Errorf("IdentityID = %q, want none", s.IdentityID)
		}
	})

	t.Run("a malformed user id is refused", func(t *testing.T) {
		if _, err := p.CreateSession(ctx, "hash-bad-user", "not-a-uuid", "", "", "", "", expiry); err == nil {
			t.Fatal("CreateSession accepted a malformed user id")
		}
	})

	t.Run("a user that does not exist is refused", func(t *testing.T) {
		if _, err := p.CreateSession(ctx, "hash-ghost", uuid.NewString(), "", "", "", "", expiry); err == nil {
			t.Fatal("CreateSession made a session for nobody")
		}
	})

	t.Run("a reused token hash is a conflict", func(t *testing.T) {
		_, err := p.CreateSession(ctx, "hash-bad-identity", userID, "", "", "", "", expiry)
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("CreateSession with a used hash: %v, want store.ErrConflict", err)
		}
	})
}

// Expiry, revocation, a deleted user, and a token that never existed are one
// answer.
func TestSessionByTokenHashRefusesDeadSessionsAlike(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	userID := sessionUser(t, p)

	expired := newSession(t, p, userID, "hash-dead-expired", "", "", time.Hour)
	expireSession(t, p, expired, time.Minute)

	newSession(t, p, userID, "hash-dead-revoked", "", "", time.Hour)
	if err := p.RevokeSession(ctx, "hash-dead-revoked"); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	gone := secondUser(t, p)
	newSession(t, p, gone.String(), "hash-dead-user", "", "", time.Hour)
	if _, err := p.Exec(ctx, `DELETE FROM users WHERE id = $1`, gone); err != nil {
		t.Fatalf("deleting the user: %v", err)
	}

	for _, token := range []string{"hash-dead-expired", "hash-dead-revoked", "hash-dead-user", "hash-never-issued"} {
		t.Run(token, func(t *testing.T) {
			if _, err := p.SessionByTokenHash(ctx, token); !errors.Is(err, auth.ErrSessionInvalid) {
				t.Errorf("SessionByTokenHash: %v, want auth.ErrSessionInvalid", err)
			}
		})
	}
}

// Using a session records that it was used, which is what the session list
// sorts by.
func TestSessionByTokenHashTouchesLastUsed(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	userID := sessionUser(t, p)
	id := newSession(t, p, userID, "hash-touch", "", "", time.Hour)
	if _, err := p.Exec(ctx, `UPDATE sessions SET created_at = now() - interval '1 day', last_used_at = now() - interval '1 day' WHERE id = $1`, id); err != nil {
		t.Fatalf("ageing the session: %v", err)
	}

	s, err := p.SessionByTokenHash(ctx, "hash-touch")
	if err != nil {
		t.Fatalf("SessionByTokenHash: %v", err)
	}
	if time.Since(s.LastUsedAt) > time.Hour {
		t.Errorf("last used %v, want now", s.LastUsedAt)
	}
}

func TestRevokeSessionEndsOnlyThatSession(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	userID := sessionUser(t, p)
	newSession(t, p, userID, "hash-revoke-one", "", "", time.Hour)
	newSession(t, p, userID, "hash-keep-one", "", "", time.Hour)

	if err := p.RevokeSession(ctx, "hash-revoke-one"); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if _, err := p.SessionByTokenHash(ctx, "hash-revoke-one"); !errors.Is(err, auth.ErrSessionInvalid) {
		t.Errorf("revoked session: %v, want auth.ErrSessionInvalid", err)
	}
	if _, err := p.SessionByTokenHash(ctx, "hash-keep-one"); err != nil {
		t.Errorf("the other session was ended too: %v", err)
	}
	// Signing out twice, or with a token that was never issued, is not an error.
	if err := p.RevokeSession(ctx, "hash-revoke-one"); err != nil {
		t.Errorf("revoking again: %v", err)
	}
}

func TestRevokeAllSessionsIsPerUser(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	userID := sessionUser(t, p)
	other := secondUser(t, p)
	removeUserAfterAuthTest(t, p, other)

	newSession(t, p, userID, "hash-all-a", "", "", time.Hour)
	newSession(t, p, userID, "hash-all-b", "", "", time.Hour)
	newSession(t, p, other.String(), "hash-all-other", "", "", time.Hour)

	if err := p.RevokeAllSessions(ctx, userID); err != nil {
		t.Fatalf("RevokeAllSessions: %v", err)
	}
	for _, token := range []string{"hash-all-a", "hash-all-b"} {
		if _, err := p.SessionByTokenHash(ctx, token); !errors.Is(err, auth.ErrSessionInvalid) {
			t.Errorf("%s after signing out everywhere: %v", token, err)
		}
	}
	if _, err := p.SessionByTokenHash(ctx, "hash-all-other"); err != nil {
		t.Errorf("somebody else was signed out: %v", err)
	}

	if err := p.RevokeAllSessions(ctx, "not-a-uuid"); err == nil {
		t.Error("a malformed user id was accepted")
	}
}

func TestUserSessionsRejectsAMalformedUserID(t *testing.T) {
	p := testPool(t)
	if _, err := p.UserSessions(t.Context(), "not-a-uuid"); err == nil {
		t.Error("UserSessions accepted a malformed user id")
	}
}

// A revoked session is kept for the retention window, then swept.
func TestPurgeExpiredSessionsSweepsOldRevocations(t *testing.T) {
	p := testPool(t)
	ctx := t.Context()
	userID := sessionUser(t, p)

	old := newSession(t, p, userID, "hash-old-revoked", "", "", 365*24*time.Hour)
	recent := newSession(t, p, userID, "hash-recent-revoked", "", "", 365*24*time.Hour)
	if _, err := p.Exec(ctx, `UPDATE sessions SET revoked_at = now() - interval '30 days' WHERE id = $1`, old); err != nil {
		t.Fatalf("revoking long ago: %v", err)
	}
	if _, err := p.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE id = $1`, recent); err != nil {
		t.Fatalf("revoking now: %v", err)
	}

	removed, err := p.PurgeExpiredSessions(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("PurgeExpiredSessions: %v", err)
	}
	if removed != 1 {
		t.Errorf("purged %d, want only the old revocation", removed)
	}
	if n := rowCount(t, p, `SELECT count(*) FROM sessions WHERE id = $1`, recent); n != 1 {
		t.Error("a recent revocation was swept inside the retention window")
	}
}
