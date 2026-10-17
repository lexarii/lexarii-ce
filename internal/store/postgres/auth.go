// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"scm.poniatowski.dev/adam/lexarii/internal/auth"
)

// LocalIdentity returns the stored local identity and its password hash.
func (p *Pool) LocalIdentity(ctx context.Context, username string) (auth.Identity, string, error) {
	var (
		identity auth.Identity
		secret   *string
		email    *string
		display  *string
	)

	err := p.QueryRow(ctx, `
		SELECT ui.provider, ui.provider_user_id, ui.secret, ui.email, ui.display_name
		FROM user_identities ui
		WHERE ui.provider = 'local' AND lower(ui.provider_user_id) = lower($1)`, username,
	).Scan(&identity.Provider, &identity.Subject, &secret, &email, &display)
	if err != nil {
		return auth.Identity{}, "", mapError(err)
	}

	identity.Email = deref(email)
	identity.DisplayName = deref(display)
	return identity, deref(secret), nil
}

// UpsertIdentity links a verified identity to a user, creating the user on
// first sign-in when the provider is allowed to.
//
// This is the join point every provider funnels through. A person arriving via
// OIDC and the same person arriving via LDAP land on the same user row when
// their identities are linked, which is the reason identities are a table
// rather than columns on users.
func (p *Pool) UpsertIdentity(ctx context.Context, identity auth.Identity, allowCreate bool) (uuid.UUID, uuid.UUID, error) {
	tx, err := p.Begin(ctx)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("begin: %w", mapError(err))
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var userID, identityID uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id, user_id FROM user_identities
		WHERE provider = $1 AND lower(provider_user_id) = lower($2)`,
		identity.Provider, identity.Subject,
	).Scan(&identityID, &userID)

	switch {
	case err == nil:
		// Refresh what the provider says about them. Never authoritative, but
		// out-of-date display names are a support question nobody needs.
		if _, err := tx.Exec(ctx, `
			UPDATE user_identities
			SET email = COALESCE(NULLIF($2, ''), email),
			    display_name = COALESCE(NULLIF($3, ''), display_name),
			    provider_data = $4,
			    last_login_at = now()
			WHERE id = $1`,
			identityID, identity.Email, identity.DisplayName, identityData(identity)); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("refreshing identity: %w", mapError(err))
		}

	case isNotFound(mapError(err)):
		if !allowCreate {
			// A verified identity with no account, and this provider is not
			// permitted to create one. Refusing is the safe default: automatic
			// account creation on a public OIDC issuer would let anyone in.
			return uuid.Nil, uuid.Nil, auth.ErrInvalidCredentials
		}

		username := identity.Subject
		if identity.DisplayName != "" {
			username = identity.DisplayName
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (username, email) VALUES ($1, NULLIF($2, ''))
			RETURNING id`, uniqueUsername(username), identity.Email,
		).Scan(&userID); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("creating user: %w", mapError(err))
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO user_identities
				(user_id, provider, provider_user_id, email, display_name, provider_data, last_login_at)
			VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, now())
			RETURNING id`,
			userID, identity.Provider, identity.Subject, identity.Email,
			identity.DisplayName, identityData(identity),
		).Scan(&identityID); err != nil {
			return uuid.Nil, uuid.Nil, fmt.Errorf("creating identity: %w", mapError(err))
		}

	default:
		return uuid.Nil, uuid.Nil, fmt.Errorf("looking up identity: %w", mapError(err))
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("commit: %w", mapError(err))
	}
	return userID, identityID, nil
}

// CreateLocalUser creates an account with a password, in a transaction of its
// own.
func (p *Pool) CreateLocalUser(ctx context.Context, username, email, password string, admin bool) (uuid.UUID, error) {
	// Hashed before the transaction opens: argon2 is deliberately slow, and a
	// connection held open across it is a connection nobody else can use.
	hash, err := auth.HashPassword(password)
	if err != nil {
		return uuid.Nil, err
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin: %w", mapError(err))
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	userID, err := insertLocalUser(ctx, tx, username, email, hash, admin)
	if err != nil {
		return uuid.Nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit: %w", mapError(err))
	}
	return userID, nil
}

// CreateLocalUserTx creates an account with a password inside the caller's
// transaction, so the account and rows of the caller's own - a first
// collection, a membership in something outside Lexarii's schema - are
// created together or not at all. Nothing is committed or rolled back here.
//
// The errors are CreateLocalUser's: auth.ErrWeakPassword for a password the
// policy refuses, before anything touches the transaction, and
// store.ErrConflict for a username that is taken. After a database error
// Postgres has aborted the transaction, so the caller must roll it back.
//
// The password is hashed while the transaction is open, which is slow on
// purpose; a caller creating many accounts should keep its transactions short.
func CreateLocalUserTx(ctx context.Context, tx pgx.Tx, username, email, password string, admin bool) (uuid.UUID, error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return uuid.Nil, err
	}
	return insertLocalUser(ctx, tx, username, email, hash, admin)
}

// insertLocalUser writes the user row and its local identity.
func insertLocalUser(ctx context.Context, tx pgx.Tx, username, email, hash string, admin bool) (uuid.UUID, error) {
	var userID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (username, email, is_admin) VALUES ($1, NULLIF($2, ''), $3)
		RETURNING id`, strings.TrimSpace(username), email, admin,
	).Scan(&userID); err != nil {
		return uuid.Nil, fmt.Errorf("creating user: %w", mapError(err))
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO user_identities (user_id, provider, provider_user_id, secret, email)
		VALUES ($1, 'local', $2, $3, NULLIF($4, ''))`,
		userID, strings.TrimSpace(username), hash, email); err != nil {
		return uuid.Nil, fmt.Errorf("creating local identity: %w", mapError(err))
	}
	return userID, nil
}

// SetLocalPassword replaces a user's password and, by design, signs them out
// everywhere else: a password change is usually a response to a compromise, and
// leaving other sessions alive defeats the point.
func (p *Pool) SetLocalPassword(ctx context.Context, userID uuid.UUID, password string) error {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	// One transaction: a new password with the old sessions still alive is
	// exactly the state a change made after a compromise must never leave.
	tx, err := p.Begin(ctx)
	if err != nil {
		return fmt.Errorf("setting password: %w", mapError(err))
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	tag, err := tx.Exec(ctx, `
		UPDATE user_identities SET secret = $2
		WHERE user_id = $1 AND provider = 'local'`, userID, hash)
	if err != nil {
		return fmt.Errorf("setting password: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("setting password: %w", errNotFound)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
		return fmt.Errorf("revoking sessions after a password change: %w", mapError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("setting password: %w", mapError(err))
	}
	return nil
}

// HasAnyUser reports whether anyone can actually sign in.
//
// Deliberately NOT "does a user row exist". An instance upgraded from the
// version that had no authentication carries a placeholder account with no
// credential: a row exists, nobody can sign in, and a naive check would close
// setup and lock the owner out of their own library permanently.
//
// The question that matters is whether a USABLE credential exists - a local
// identity with a password, or any federated identity, where the provider holds
// the credential.
func (p *Pool) HasAnyUser(ctx context.Context) (bool, error) {
	var exists bool
	err := p.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM user_identities
			WHERE (provider = 'local' AND secret IS NOT NULL AND secret <> '')
			   OR provider <> 'local'
		)`).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking for usable accounts: %w", mapError(err))
	}
	return exists, nil
}

// AdoptOrCreateLocalUser gives the first account a password.
//
// When a credential-less placeholder already exists - left by an upgrade from
// the version with no authentication - its password is set rather than a second
// account being created. That account already owns reading positions,
// annotations, and collections, and creating a fresh one beside it would orphan
// all of them while appearing to work.
func (p *Pool) AdoptOrCreateLocalUser(ctx context.Context, username, email, password string, admin bool) (uuid.UUID, error) {
	hash, err := auth.HashPassword(password)
	if err != nil {
		return uuid.Nil, err
	}

	tx, err := p.Begin(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin: %w", mapError(err))
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	var existingUser, existingIdentity uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT ui.user_id, ui.id
		FROM user_identities ui
		WHERE ui.provider = 'local' AND (ui.secret IS NULL OR ui.secret = '')
		ORDER BY ui.created_at
		LIMIT 1`).Scan(&existingUser, &existingIdentity)

	switch {
	case err == nil:
		// Adopt the placeholder, keeping everything attached to it.
		if _, err := tx.Exec(ctx, `
			UPDATE user_identities SET secret = $2, provider_user_id = $3, email = NULLIF($4, '')
			WHERE id = $1`, existingIdentity, hash, strings.TrimSpace(username), email); err != nil {
			return uuid.Nil, fmt.Errorf("adopting the placeholder identity: %w", mapError(err))
		}
		if _, err := tx.Exec(ctx, `
			UPDATE users SET username = $2, email = NULLIF($3, ''), is_admin = $4
			WHERE id = $1`, existingUser, strings.TrimSpace(username), email, admin); err != nil {
			return uuid.Nil, fmt.Errorf("adopting the placeholder user: %w", mapError(err))
		}
		if err := tx.Commit(ctx); err != nil {
			return uuid.Nil, fmt.Errorf("commit: %w", mapError(err))
		}
		return existingUser, nil

	case isNotFound(mapError(err)):
		// Nothing to adopt: a genuinely fresh instance.
		var userID uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (username, email, is_admin) VALUES ($1, NULLIF($2, ''), $3)
			RETURNING id`, strings.TrimSpace(username), email, admin,
		).Scan(&userID); err != nil {
			return uuid.Nil, fmt.Errorf("creating user: %w", mapError(err))
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_identities (user_id, provider, provider_user_id, secret, email)
			VALUES ($1, 'local', $2, $3, NULLIF($4, ''))`,
			userID, strings.TrimSpace(username), hash, email); err != nil {
			return uuid.Nil, fmt.Errorf("creating local identity: %w", mapError(err))
		}
		if err := tx.Commit(ctx); err != nil {
			return uuid.Nil, fmt.Errorf("commit: %w", mapError(err))
		}
		return userID, nil

	default:
		return uuid.Nil, fmt.Errorf("looking for a placeholder account: %w", mapError(err))
	}
}

// User reads an account.
func (p *Pool) User(ctx context.Context, id uuid.UUID) (UserRecord, error) {
	var u UserRecord
	var email *string
	err := p.QueryRow(ctx, `
		SELECT id, username, email, is_admin, created_at FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.Username, &email, &u.IsAdmin, &u.CreatedAt)
	if err != nil {
		return UserRecord{}, mapError(err)
	}
	u.Email = deref(email)
	return u, nil
}

// UserRecord is an account.
type UserRecord struct {
	ID        uuid.UUID
	Username  string
	Email     string
	IsAdmin   bool
	CreatedAt time.Time
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// CreateSession stores a session by the hash of its token.
func (p *Pool) CreateSession(ctx context.Context, tokenHash, userID, identityID, userAgent, clientIP, device string, expiresAt time.Time) (auth.Session, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return auth.Session{}, fmt.Errorf("session user id: %w", err)
	}

	var iid *uuid.UUID
	if identityID != "" {
		parsed, err := uuid.Parse(identityID)
		if err == nil {
			iid = &parsed
		}
	}

	var s auth.Session
	var sessionID uuid.UUID
	var identity *uuid.UUID
	err = p.QueryRow(ctx, `
		INSERT INTO sessions (user_id, identity_id, token_hash, user_agent, client_ip, device, expires_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, '')::inet, NULLIF($6, ''), $7)
		RETURNING id, user_id, identity_id, expires_at, last_used_at`,
		uid, iid, tokenHash, userAgent, clientIP, device, expiresAt,
	).Scan(&sessionID, &uid, &identity, &s.ExpiresAt, &s.LastUsedAt)
	if err != nil {
		return auth.Session{}, fmt.Errorf("creating session: %w", mapError(err))
	}

	s.ID = sessionID.String()
	s.UserID = uid.String()
	if identity != nil {
		s.IdentityID = identity.String()
	}
	return s, nil
}

// SessionByTokenHash returns a live session.
//
// Expiry, revocation, and a deleted user are all one answer - the session is
// not valid - because nothing above this needs to tell them apart and a caller
// that did would be tempted to say which, which tells an attacker whether a
// token was ever real.
func (p *Pool) SessionByTokenHash(ctx context.Context, tokenHash string) (auth.Session, error) {
	var (
		s         auth.Session
		sessionID uuid.UUID
		userID    uuid.UUID
		identity  *uuid.UUID
	)

	err := p.QueryRow(ctx, `
		UPDATE sessions SET last_used_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()
		RETURNING id, user_id, identity_id, expires_at, last_used_at`, tokenHash,
	).Scan(&sessionID, &userID, &identity, &s.ExpiresAt, &s.LastUsedAt)
	if err != nil {
		return auth.Session{}, auth.ErrSessionInvalid
	}

	s.ID = sessionID.String()
	s.UserID = userID.String()
	if identity != nil {
		s.IdentityID = identity.String()
	}
	return s, nil
}

// RevokeSession ends one session.
func (p *Pool) RevokeSession(ctx context.Context, tokenHash string) error {
	if _, err := p.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash); err != nil {
		return fmt.Errorf("revoking session: %w", mapError(err))
	}
	return nil
}

// RevokeAllSessions signs a user out everywhere.
func (p *Pool) RevokeAllSessions(ctx context.Context, userID string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("user id: %w", err)
	}
	if _, err := p.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL`, uid); err != nil {
		return fmt.Errorf("revoking sessions: %w", mapError(err))
	}
	return nil
}

// UserSession is one live sign-in, as shown to the person who owns it.
//
// The token hash is deliberately absent. Nothing above the store needs it, and
// putting it in a list that reaches a browser would hand out the one value that
// IS the credential.
type UserSession struct {
	ID        uuid.UUID
	UserAgent string
	ClientIP  string
	Provider  string
	// Device is the name a native client gave itself, and is empty for a
	// browser. It is also what says this session is a bearer token rather than
	// a cookie, which is worth showing plainly.
	Device     string
	CreatedAt  time.Time
	LastUsedAt time.Time
	ExpiresAt  time.Time
}

// UserSessions lists a user's sessions that can still be used, most recently
// used first.
//
// Revoked and expired ones are excluded: the question this answers is "what can
// get into my account right now?", and padding it with dead rows makes the
// answer harder to read rather than more complete.
func (p *Pool) UserSessions(ctx context.Context, userID string) ([]UserSession, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, fmt.Errorf("user id: %w", err)
	}

	rows, err := p.Query(ctx, `
		SELECT s.id, COALESCE(s.user_agent, ''), COALESCE(host(s.client_ip), ''),
		       COALESCE(i.provider, ''), COALESCE(s.device, ''),
		       s.created_at, s.last_used_at, s.expires_at
		FROM sessions s
		LEFT JOIN user_identities i ON i.id = s.identity_id
		WHERE s.user_id = $1 AND s.revoked_at IS NULL AND s.expires_at > now()
		ORDER BY s.last_used_at DESC`, uid)
	if err != nil {
		return nil, fmt.Errorf("reading sessions: %w", mapError(err))
	}
	defer rows.Close()

	var out []UserSession
	for rows.Next() {
		var s UserSession
		if err := rows.Scan(&s.ID, &s.UserAgent, &s.ClientIP, &s.Provider, &s.Device,
			&s.CreatedAt, &s.LastUsedAt, &s.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scanning session: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeSessionByID ends one session on behalf of its owner.
//
// Scoped to the user, so a guessed id cannot sign somebody else out. Revoking
// rather than deleting keeps the row visible to a later audit, and the sweeper
// removes it once it is old enough to be useless.
func (p *Pool) RevokeSessionByID(ctx context.Context, userID, sessionID string) error {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return fmt.Errorf("user id: %w", err)
	}
	sid, err := uuid.Parse(sessionID)
	if err != nil {
		return fmt.Errorf("session id: %w", err)
	}

	tag, err := p.Exec(ctx, `
		UPDATE sessions SET revoked_at = now()
		WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, sid, uid)
	if err != nil {
		return fmt.Errorf("revoking session: %w", mapError(err))
	}
	if tag.RowsAffected() == 0 {
		return errNotFound
	}
	return nil
}

// PurgeExpiredSessions removes sessions that can no longer be used.
func (p *Pool) PurgeExpiredSessions(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := p.Exec(ctx, `
		DELETE FROM sessions
		WHERE expires_at < now() - $1::interval
		   OR (revoked_at IS NOT NULL AND revoked_at < now() - $1::interval)`,
		olderThan.String())
	if err != nil {
		return 0, fmt.Errorf("purging sessions: %w", mapError(err))
	}
	return tag.RowsAffected(), nil
}

func identityData(identity auth.Identity) []byte {
	if len(identity.Data) == 0 && len(identity.Groups) == 0 {
		return []byte("{}")
	}
	payload := map[string]any{}
	for k, v := range identity.Data {
		payload[k] = v
	}
	if len(identity.Groups) > 0 {
		payload["groups"] = identity.Groups
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return []byte("{}")
	}
	return data
}

// uniqueUsername trims a provider-supplied name to something storable. It does
// not resolve collisions; the unique index does, and a collision is a real
// condition an administrator should see rather than have papered over.
func uniqueUsername(raw string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		name = "user"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}
