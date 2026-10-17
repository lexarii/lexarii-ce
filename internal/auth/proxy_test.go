// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"net/http/httptest"
	"testing"
)

func proxyProvider(t *testing.T, trusted ...string) *ProxyProvider {
	t.Helper()
	if len(trusted) == 0 {
		trusted = []string{"10.0.0.0/8", "192.0.2.7"}
	}
	p, err := NewProxyProvider(ProxyConfig{
		UserHeader:     "X-Forwarded-User",
		EmailHeader:    "X-Forwarded-Email",
		NameHeader:     "X-Forwarded-Name",
		GroupsHeader:   "X-Forwarded-Groups",
		TrustedProxies: trusted,
	})
	if err != nil {
		t.Fatalf("NewProxyProvider: %v", err)
	}
	return p
}

// The configuration that would open the server to anyone must be impossible,
// not merely discouraged.
func TestProxyRefusesConfigurationWithoutTrustedAddresses(t *testing.T) {
	_, err := NewProxyProvider(ProxyConfig{UserHeader: "X-User"})
	if err == nil {
		t.Fatal("a provider with no trusted proxies was accepted; anyone could assert any identity")
	}
	if _, err := NewProxyProvider(ProxyConfig{TrustedProxies: []string{"10.0.0.1"}}); err == nil {
		t.Error("a provider with no user header was accepted")
	}
	if _, err := NewProxyProvider(ProxyConfig{UserHeader: "X-User", TrustedProxies: []string{"not-an-ip"}}); err == nil {
		t.Error("an unparseable trusted address was accepted")
	}
}

// The header is a claim. Only its source makes it a credential.
func TestProxyRejectsUntrustedSources(t *testing.T) {
	p := proxyProvider(t)

	for _, addr := range []string{
		"203.0.113.9:1234", // the open internet
		"172.16.0.5:1234",  // private, but not the configured range
		"[2001:db8::1]:443",
		"not-an-address",
		"",
	} {
		t.Run(addr, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = addr
			r.Header.Set("X-Forwarded-User", "attacker")

			if _, err := p.IdentityFromRequest(r); !errors.Is(err, ErrUntrustedProxy) {
				t.Fatalf("an identity was accepted from %q: %v", addr, err)
			}
		})
	}
}

func TestProxyAcceptsTrustedSources(t *testing.T) {
	p := proxyProvider(t)

	for _, addr := range []string{"10.1.2.3:5000", "10.255.255.255:1", "192.0.2.7:9"} {
		t.Run(addr, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = addr
			r.Header.Set("X-Forwarded-User", "areader")
			r.Header.Set("X-Forwarded-Email", "areader@example.com")
			r.Header.Set("X-Forwarded-Name", "A Reader")
			r.Header.Set("X-Forwarded-Groups", "readers, librarians")

			identity, err := p.IdentityFromRequest(r)
			if err != nil {
				t.Fatalf("a trusted proxy was refused: %v", err)
			}
			if identity.Subject != "areader" {
				t.Errorf("Subject = %q", identity.Subject)
			}
			if identity.Email != "areader@example.com" {
				t.Errorf("Email = %q", identity.Email)
			}
			if len(identity.Groups) != 2 {
				t.Errorf("Groups = %v, want both, trimmed", identity.Groups)
			}
		})
	}
}

func TestProxyRequiresAUsername(t *testing.T) {
	p := proxyProvider(t)

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	// Trusted source, but the proxy asserted nobody.
	if _, err := p.IdentityFromRequest(r); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("an empty username was accepted: %v", err)
	}
}

func TestProxyRequiredGroup(t *testing.T) {
	p, err := NewProxyProvider(ProxyConfig{
		UserHeader:     "X-Forwarded-User",
		GroupsHeader:   "X-Forwarded-Groups",
		TrustedProxies: []string{"10.0.0.0/8"},
		RequiredGroup:  "librarians",
	})
	if err != nil {
		t.Fatalf("NewProxyProvider: %v", err)
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1"
	r.Header.Set("X-Forwarded-User", "areader")
	r.Header.Set("X-Forwarded-Groups", "readers")
	if _, err := p.IdentityFromRequest(r); !errors.Is(err, ErrInvalidCredentials) {
		t.Error("a non-member was admitted")
	}

	r.Header.Set("X-Forwarded-Groups", "readers,librarians")
	if _, err := p.IdentityFromRequest(r); err != nil {
		t.Errorf("a member was refused: %v", err)
	}
}

// The trust check must not depend on every caller remembering to make it.
func TestFromHeadersIsStillGuardedByIdentityFromRequest(t *testing.T) {
	var _ HeaderProvider = (*ProxyProvider)(nil)

	p := proxyProvider(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.1:1"
	r.Header.Set("X-Forwarded-User", "attacker")

	if _, err := p.IdentityFromRequest(r); err == nil {
		t.Fatal("the combined path skipped the trust check")
	}
}
