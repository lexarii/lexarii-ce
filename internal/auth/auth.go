// SPDX-License-Identifier: AGPL-3.0-or-later

// Package auth authenticates people and manages their sessions.
//
// Authentication is pluggable by design. Lexarii ships local passwords, and
// the shape here is what lets OIDC, OAuth2, LDAP, Active Directory, and
// reverse-proxy headers be added later as configuration rather than as a
// rewrite - which is only true because a person and the ways they prove who
// they are were separated before the first account existed.
package auth

import (
	"context"
	"errors"
	"time"
)

// Errors a caller may reasonably branch on.
var (
	// ErrInvalidCredentials covers a wrong password, an unknown user, and a
	// disabled account alike.
	//
	// Deliberately ONE error. Distinguishing "no such user" from "wrong
	// password" tells an attacker which usernames exist, and that is worth more
	// to them than it is to anyone else.
	ErrInvalidCredentials = errors.New("invalid credentials")

	// ErrNoSuchProvider reports a provider that is not configured.
	ErrNoSuchProvider = errors.New("unknown authentication provider")

	// ErrSessionInvalid reports a missing, expired, or revoked session.
	ErrSessionInvalid = errors.New("session is not valid")

	// ErrUnsupported reports an operation a provider cannot perform, such as
	// changing a password held in a directory Lexarii does not own.
	ErrUnsupported = errors.New("not supported by this provider")
)

// Identity is a person as one provider describes them.
type Identity struct {
	// Provider names the source: "local", "oidc", "ldap", "proxy".
	Provider string

	// Subject is the provider's own stable identifier.
	//
	// For OIDC this MUST be the `sub` claim, never the email address: email
	// addresses are reassigned between employees, and keying on one eventually
	// hands a new joiner the previous holder's library.
	Subject string

	Email       string
	DisplayName string

	// Groups carries whatever the provider says about membership. Unused today,
	// captured now because a directory integration that discards them forces a
	// second round trip later.
	Groups []string

	// Data is the raw claim set or attribute set, kept for diagnosing a
	// federation that is misbehaving.
	Data map[string]any
}

// Kind tells the UI how a provider expects to be used, so a login page can
// render a form for one and a button for another without knowing the details.
type Kind string

const (
	// KindPassword takes a username and a password directly: local accounts,
	// and LDAP or Active Directory, which authenticate by binding as the user.
	KindPassword Kind = "password"

	// KindRedirect sends the browser to the provider and receives a callback:
	// OIDC and OAuth2.
	KindRedirect Kind = "redirect"

	// KindHeader trusts an authenticating reverse proxy in front of Lexarii.
	// Off unless explicitly configured, because a misconfigured instance would
	// otherwise let anyone set the header themselves.
	KindHeader Kind = "header"
)

// Provider is the minimum every authentication method implements.
type Provider interface {
	// Name is the stable identifier stored against an identity. Changing it
	// orphans every account that used it.
	Name() string
	// DisplayName is what a person sees on the login screen.
	DisplayName() string
	Kind() Kind
}

// PasswordProvider verifies a username and password.
//
// Implemented by local accounts and by LDAP and Active Directory, which verify
// by binding to the directory as the user rather than by comparing a hash.
type PasswordProvider interface {
	Provider
	Authenticate(ctx context.Context, username, password string) (Identity, error)
}

// RedirectProvider authenticates by sending the browser elsewhere and handling
// a callback: OIDC and OAuth2.
//
// Defined now, with no implementation, because the session and identity plumbing
// has to accommodate it from the start. A design that assumes credentials arrive
// in the request cannot be extended to one where they arrive in a callback.
type RedirectProvider interface {
	Provider
	// AuthURL is where the browser is sent. State must be verified on return.
	// Nonce is a second random value, kept with the state against the
	// browser, that the provider must echo inside the identity it returns.
	AuthURL(state, nonce, redirectURI string) string
	// Exchange turns a callback code into a verified identity. Nonce is the
	// value given to AuthURL for this sign-in; an identity carrying any other
	// nonce, or none, is refused.
	Exchange(ctx context.Context, code, nonce, redirectURI string) (Identity, error)
}

// HeaderProvider trusts an authenticating reverse proxy.
type HeaderProvider interface {
	Provider
	// FromHeaders extracts an identity from request headers, or
	// ErrInvalidCredentials when they are absent or unusable.
	FromHeaders(get func(string) string) (Identity, error)
}

// Session is an authenticated browser session.
type Session struct {
	ID         string
	UserID     string
	IdentityID string
	ExpiresAt  time.Time
	LastUsedAt time.Time
}

// Registry holds the configured providers in a stable order.
type Registry struct {
	providers []Provider
	byName    map[string]Provider
}

// NewRegistry builds a registry. The first provider listed is the default one
// the login page offers.
func NewRegistry(providers ...Provider) *Registry {
	r := &Registry{byName: make(map[string]Provider, len(providers))}
	for _, p := range providers {
		if p == nil {
			continue
		}
		r.providers = append(r.providers, p)
		r.byName[p.Name()] = p
	}
	return r
}

// All returns every configured provider, in configuration order.
func (r *Registry) All() []Provider {
	out := make([]Provider, len(r.providers))
	copy(out, r.providers)
	return out
}

// Get returns a provider by name.
func (r *Registry) Get(name string) (Provider, error) {
	p, ok := r.byName[name]
	if !ok {
		return nil, ErrNoSuchProvider
	}
	return p, nil
}

// Password returns a provider by name, if it authenticates with a password.
func (r *Registry) Password(name string) (PasswordProvider, error) {
	p, err := r.Get(name)
	if err != nil {
		return nil, err
	}
	pp, ok := p.(PasswordProvider)
	if !ok {
		return nil, ErrUnsupported
	}
	return pp, nil
}
