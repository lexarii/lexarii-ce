// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"errors"
	"net/http"
	"strings"
)

// ProxyConfig configures trust in an authenticating reverse proxy.
type ProxyConfig struct {
	ProviderName string
	Label        string

	// UserHeader carries the authenticated username. Required.
	UserHeader string
	// EmailHeader and NameHeader are optional enrichment.
	EmailHeader string
	NameHeader  string
	// GroupsHeader carries a comma-separated membership list.
	GroupsHeader string

	// TrustedProxies lists the addresses permitted to assert an identity, as IPs
	// or CIDR blocks.
	//
	// REQUIRED, and there is deliberately no way to skip it. Without it anyone
	// who can reach the port sets the header themselves and becomes whoever they
	// like - the header is a claim, and only its source makes it a credential.
	TrustedProxies []string

	// RequiredGroup restricts access to one group.
	RequiredGroup string

	// AllowSignup permits creating an account on first sign-in. Usually correct
	// here, because the proxy has already decided who may reach Lexarii.
	AllowSignup bool
}

// ProxyProvider trusts an authenticating reverse proxy in front of Lexarii.
//
// For people already running an SSO proxy - oauth2-proxy, Authelia, Authentik,
// a corporate gateway - where a second login screen is friction with no benefit.
//
// It is the most dangerous provider to configure, because the credential is a
// plain header. Everything here exists to make the dangerous configuration
// impossible rather than merely discouraged.
type ProxyProvider struct {
	cfg     ProxyConfig
	trusted *TrustedProxies
}

// ErrUntrustedProxy reports a request from an address not permitted to assert
// an identity.
var ErrUntrustedProxy = errors.New("request did not come from a trusted proxy")

// NewProxyProvider builds a provider, refusing an unsafe configuration.
func NewProxyProvider(cfg ProxyConfig) (*ProxyProvider, error) {
	if cfg.UserHeader == "" {
		return nil, errors.New("proxy: a user header is required")
	}
	if len(cfg.TrustedProxies) == 0 {
		// Refused rather than defaulted. A default of "trust everything" is an
		// open door, and a default of "trust nothing" is a provider that never
		// works and gets debugged by someone widening it until it does.
		return nil, errors.New("proxy: at least one trusted proxy address is required; " +
			"without it anyone who can reach this server can assert any identity")
	}
	if cfg.ProviderName == "" {
		cfg.ProviderName = "proxy"
	}
	if cfg.Label == "" {
		cfg.Label = "Single sign-on"
	}

	trusted, err := ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return nil, errors.New("proxy: " + err.Error())
	}
	if trusted.Empty() {
		return nil, errors.New("proxy: no usable trusted proxy addresses")
	}
	return &ProxyProvider{cfg: cfg, trusted: trusted}, nil
}

func (p *ProxyProvider) Name() string        { return p.cfg.ProviderName }
func (p *ProxyProvider) DisplayName() string { return p.cfg.Label }
func (p *ProxyProvider) Kind() Kind          { return KindHeader }

// AllowSignup reports whether first sign-in may create an account.
func (p *ProxyProvider) AllowSignup() bool { return p.cfg.AllowSignup }

// Trusts reports whether an address may assert an identity.
func (p *ProxyProvider) Trusts(remoteAddr string) bool {
	return p.trusted.TrustsAddr(remoteAddr)
}

// Trusted exposes the proxy set, so the sign-in throttle can attribute a
// request to the reader who made it rather than to the proxy they all share.
func (p *ProxyProvider) Trusted() *TrustedProxies { return p.trusted }

// FromHeaders extracts an identity from a request.
//
// The caller MUST have checked Trusts first; this checks again anyway, because
// a security control that depends on every caller remembering it is not a
// control.
func (p *ProxyProvider) FromHeaders(get func(string) string) (Identity, error) {
	username := strings.TrimSpace(get(p.cfg.UserHeader))
	if username == "" {
		return Identity{}, ErrInvalidCredentials
	}

	identity := Identity{
		Provider: p.cfg.ProviderName,
		Subject:  username,
		Data:     map[string]any{"header": p.cfg.UserHeader},
	}
	if p.cfg.EmailHeader != "" {
		identity.Email = strings.TrimSpace(get(p.cfg.EmailHeader))
	}
	if p.cfg.NameHeader != "" {
		identity.DisplayName = strings.TrimSpace(get(p.cfg.NameHeader))
	}
	if p.cfg.GroupsHeader != "" {
		for _, g := range strings.Split(get(p.cfg.GroupsHeader), ",") {
			if g = strings.TrimSpace(g); g != "" {
				identity.Groups = append(identity.Groups, g)
			}
		}
	}

	if p.cfg.RequiredGroup != "" && !hasGroup(identity.Groups, p.cfg.RequiredGroup) {
		return Identity{}, ErrInvalidCredentials
	}

	return identity, nil
}

// IdentityFromRequest is the convenience the HTTP layer uses, combining the
// trust check and the header read so neither can be forgotten.
func (p *ProxyProvider) IdentityFromRequest(r *http.Request) (Identity, error) {
	if !p.Trusts(r.RemoteAddr) {
		return Identity{}, ErrUntrustedProxy
	}
	return p.FromHeaders(r.Header.Get)
}
