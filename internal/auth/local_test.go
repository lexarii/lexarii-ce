// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// fakeLocalIdentities is an IdentityLookup over a map of username to stored
// identity and hash, recording which usernames it was asked about.
type fakeLocalIdentities struct {
	mu       sync.Mutex
	accounts map[string]fakeLocalAccount
	asked    []string
}

type fakeLocalAccount struct {
	identity Identity
	hash     string
}

func (f *fakeLocalIdentities) LocalIdentity(_ context.Context, username string) (Identity, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, username)
	a, ok := f.accounts[username]
	if !ok {
		return Identity{}, "", ErrInvalidCredentials
	}
	return a.identity, a.hash, nil
}

func localProviderWithReader(t *testing.T) (*LocalProvider, *fakeLocalIdentities) {
	t.Helper()
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	store := &fakeLocalIdentities{accounts: map[string]fakeLocalAccount{
		"reader": {identity: Identity{Subject: "user-1", DisplayName: "A Reader"}, hash: hash},
		// Created for a federated provider: no password, so no password sign-in.
		"federated": {identity: Identity{Subject: "user-2"}, hash: ""},
	}}
	return &LocalProvider{Store: store}, store
}

func TestLocalProviderDescribesItself(t *testing.T) {
	var p PasswordProvider = &LocalProvider{}
	if p.Name() != ProviderLocal || p.DisplayName() == "" || p.Kind() != KindPassword {
		t.Errorf("name %q label %q kind %q", p.Name(), p.DisplayName(), p.Kind())
	}
}

func TestLocalAuthenticate(t *testing.T) {
	p, _ := localProviderWithReader(t)

	identity, err := p.Authenticate(t.Context(), "  reader  ", "correct horse battery")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.Subject != "user-1" || identity.Provider != ProviderLocal || identity.DisplayName != "A Reader" {
		t.Errorf("identity = %+v, want the stored identity under the local provider", identity)
	}
}

// Every refusal is the same error, so a caller cannot tell a wrong password
// from an account that does not exist.
func TestLocalRefusalsAreIndistinguishable(t *testing.T) {
	p, store := localProviderWithReader(t)

	tests := []struct {
		name, user, password string
		lookedUp             bool
	}{
		{name: "wrong password", user: "reader", password: "incorrect horse", lookedUp: true},
		{name: "no such account", user: "nobody", password: "correct horse battery", lookedUp: true},
		{name: "an account with no password", user: "federated", password: "correct horse battery", lookedUp: true},
		{name: "no username", user: "   ", password: "correct horse battery"},
		{name: "no password", user: "reader", password: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store.mu.Lock()
			store.asked = nil
			store.mu.Unlock()

			identity, err := p.Authenticate(t.Context(), tc.user, tc.password)
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("error = %v, want ErrInvalidCredentials", err)
			}
			if identity.Subject != "" {
				t.Errorf("a refusal returned an identity: %+v", identity)
			}
			store.mu.Lock()
			asked := len(store.asked)
			store.mu.Unlock()
			if (asked > 0) != tc.lookedUp {
				t.Errorf("store consulted %d times, want consulted=%v", asked, tc.lookedUp)
			}
		})
	}
}

func TestConstantTimeEqualString(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"state-value", "state-value", true},
		{"state-value", "state-valuE", false},
		{"state-value", "state", false},
		{"", "", true},
	} {
		if got := ConstantTimeEqualString(tc.a, tc.b); got != tc.want {
			t.Errorf("ConstantTimeEqualString(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestRegistry(t *testing.T) {
	local := &LocalProvider{}
	proxy := proxyProvider(t)
	r := NewRegistry(local, nil, proxy)

	all := r.All()
	if len(all) != 2 || all[0] != Provider(local) || all[1] != Provider(proxy) {
		t.Fatalf("All = %v, want local then proxy with the nil skipped", all)
	}
	// The slice handed out is a copy: the login page cannot reorder providers.
	all[0] = proxy
	if r.All()[0] != Provider(local) {
		t.Error("changing All's result changed the registry")
	}

	if got, err := r.Get(ProviderLocal); err != nil || got != Provider(local) {
		t.Errorf("Get(local) = %v, %v", got, err)
	}
	if _, err := r.Get("saml"); !errors.Is(err, ErrNoSuchProvider) {
		t.Errorf("Get(unknown) = %v, want ErrNoSuchProvider", err)
	}

	if pp, err := r.Password(ProviderLocal); err != nil || pp != PasswordProvider(local) {
		t.Errorf("Password(local) = %v, %v", pp, err)
	}
	// A header provider takes no password; asking it to is unsupported, not
	// "no such provider".
	if _, err := r.Password(proxy.Name()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Password(proxy) = %v, want ErrUnsupported", err)
	}
	if _, err := r.Password("saml"); !errors.Is(err, ErrNoSuchProvider) {
		t.Errorf("Password(unknown) = %v, want ErrNoSuchProvider", err)
	}
}
