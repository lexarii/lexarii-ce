// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"strings"
)

// IdentityLookup fetches a stored identity for the local provider.
//
// Declared here by the consumer so the provider depends on the narrow thing it
// needs rather than on the whole store.
type IdentityLookup interface {
	// LocalIdentity returns the stored identity and its password hash, or
	// ErrInvalidCredentials when there is no such account.
	LocalIdentity(ctx context.Context, username string) (Identity, string, error)
}

// LocalProvider authenticates against password hashes Lexarii itself stores.
type LocalProvider struct {
	Store IdentityLookup
}

// ProviderLocal is the stable name recorded against local identities.
const ProviderLocal = "local"

func (p *LocalProvider) Name() string        { return ProviderLocal }
func (p *LocalProvider) DisplayName() string { return "Password" }
func (p *LocalProvider) Kind() Kind          { return KindPassword }

// decoyHash is a real argon2id hash of a random password, used to keep the cost
// of a failed lookup the same as a failed password check.
//
// Without it, an unknown username returns in microseconds while a known one
// takes the full hashing time, and that difference is a reliable way to
// enumerate accounts. Generated once at startup so it is never a known value.
var decoyHash string

func init() {
	filler := make([]byte, 32)
	_, _ = rand.Read(filler)
	if h, err := HashPassword(string(filler) + "xxxxxxxxxx"); err == nil {
		decoyHash = h
	}
}

// Authenticate verifies a username and password.
func (p *LocalProvider) Authenticate(ctx context.Context, username, password string) (Identity, error) {
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return Identity{}, ErrInvalidCredentials
	}

	identity, hash, err := p.Store.LocalIdentity(ctx, username)
	if err != nil {
		// Hash anyway. An unknown username must cost the same as a known one
		// with a wrong password, or the timing difference enumerates accounts.
		VerifyPassword(password, decoyHash)
		return Identity{}, ErrInvalidCredentials
	}

	if hash == "" {
		// An identity with no password: created by an administrator for a
		// federated provider, or left unset during first-run setup. It cannot be
		// used to sign in with a password, and the cost is still paid.
		VerifyPassword(password, decoyHash)
		return Identity{}, ErrInvalidCredentials
	}

	if !VerifyPassword(password, hash) {
		return Identity{}, ErrInvalidCredentials
	}

	identity.Provider = ProviderLocal
	return identity, nil
}

// ConstantTimeEqualString compares two strings without leaking their contents
// through timing. Used for session tokens and CSRF state.
func ConstantTimeEqualString(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
