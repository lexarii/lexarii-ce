// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// LDAPConfig configures an LDAP or Active Directory directory.
type LDAPConfig struct {
	ProviderName string
	Label        string

	// URL is ldap:// or ldaps://. Plain ldap:// with StartTLS is the common
	// on-premises arrangement.
	URL string
	// StartTLS upgrades a plain connection before any credential is sent.
	StartTLS bool
	// InsecureSkipVerify disables certificate verification. Present because
	// internal directories routinely use a private CA, and logged loudly when
	// used, because it removes the protection TLS was there to provide.
	InsecureSkipVerify bool

	// BindDN and BindPassword are a service account used to SEARCH for the user
	// before binding as them. Leave empty for anonymous search, or use
	// UserDNTemplate to skip searching entirely.
	BindDN       string
	BindPassword string

	// BaseDN is where user search begins.
	BaseDN string

	// UserFilter finds the user. %s is replaced with the escaped username.
	// Defaults suit OpenLDAP; Active Directory wants sAMAccountName.
	UserFilter string

	// UserDNTemplate binds directly without searching, %s being the username.
	//
	// Simpler where it works, and the usual choice for Active Directory, where
	// "%s@corp.example" binds with a userPrincipalName. It cannot read
	// attributes or groups, because no search happens.
	UserDNTemplate string

	// Attribute names. Defaults suit OpenLDAP.
	UsernameAttr    string
	EmailAttr       string
	DisplayNameAttr string
	GroupAttr       string

	// RequiredGroup, when set, refuses anyone who is not a member. This is how
	// a directory serving a whole company grants access to a few people.
	RequiredGroup string

	// AllowSignup permits creating an account on first sign-in.
	AllowSignup bool

	Timeout time.Duration
}

// LDAPProvider authenticates against an LDAP or Active Directory server.
//
// It is a PasswordProvider, exactly like local accounts, and needed no new
// plumbing: the claim that the abstraction made LDAP additive is tested by this
// file being the whole of the change.
//
// Authentication is by BINDING as the user. Lexarii never reads, compares, or
// stores a directory password - the directory decides, which is the entire
// point of using one.
type LDAPProvider struct {
	cfg LDAPConfig
}

// NewLDAPProvider builds a provider, applying defaults.
func NewLDAPProvider(cfg LDAPConfig) (*LDAPProvider, error) {
	if cfg.URL == "" {
		return nil, errors.New("ldap: a server URL is required")
	}
	if cfg.BaseDN == "" && cfg.UserDNTemplate == "" {
		return nil, errors.New("ldap: either a base DN or a user DN template is required")
	}
	if cfg.ProviderName == "" {
		cfg.ProviderName = "ldap"
	}
	if cfg.Label == "" {
		cfg.Label = "Directory"
	}
	if cfg.UserFilter == "" {
		cfg.UserFilter = "(uid=%s)"
	}
	if cfg.UsernameAttr == "" {
		cfg.UsernameAttr = "uid"
	}
	if cfg.EmailAttr == "" {
		cfg.EmailAttr = "mail"
	}
	if cfg.DisplayNameAttr == "" {
		cfg.DisplayNameAttr = "cn"
	}
	if cfg.GroupAttr == "" {
		cfg.GroupAttr = "memberOf"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &LDAPProvider{cfg: cfg}, nil
}

// ActiveDirectoryDefaults returns a config pre-set for Active Directory.
//
// AD differs from OpenLDAP in every attribute name that matters, and getting
// them wrong produces an authentication that succeeds while returning no
// identity at all.
func ActiveDirectoryDefaults(cfg LDAPConfig) LDAPConfig {
	if cfg.ProviderName == "" {
		cfg.ProviderName = "ad"
	}
	if cfg.Label == "" {
		cfg.Label = "Active Directory"
	}
	if cfg.UserFilter == "" {
		// Match either the login name or the full UPN, since people type both.
		cfg.UserFilter = "(&(objectClass=user)(|(sAMAccountName=%s)(userPrincipalName=%s)))"
	}
	if cfg.UsernameAttr == "" {
		cfg.UsernameAttr = "sAMAccountName"
	}
	if cfg.DisplayNameAttr == "" {
		cfg.DisplayNameAttr = "displayName"
	}
	return cfg
}

func (p *LDAPProvider) Name() string        { return p.cfg.ProviderName }
func (p *LDAPProvider) DisplayName() string { return p.cfg.Label }
func (p *LDAPProvider) Kind() Kind          { return KindPassword }

// AllowSignup reports whether first sign-in may create an account.
func (p *LDAPProvider) AllowSignup() bool { return p.cfg.AllowSignup }

// Authenticate binds to the directory as the user.
func (p *LDAPProvider) Authenticate(ctx context.Context, username, password string) (Identity, error) {
	username = strings.TrimSpace(username)
	// An empty password must never reach a bind.
	//
	// LDAP treats a bind with an empty password as an ANONYMOUS bind, which
	// succeeds against most servers - so without this check, every account
	// authenticates with a blank password. It is the single most common way an
	// LDAP integration is catastrophically wrong.
	if username == "" || password == "" {
		return Identity{}, ErrInvalidCredentials
	}

	conn, err := p.connect(ctx)
	if err != nil {
		return Identity{}, fmt.Errorf("ldap: %w", err)
	}
	defer func() { _ = conn.Close() }()

	userDN, attrs, err := p.findUser(conn, username)
	if err != nil {
		return Identity{}, err
	}

	// The actual authentication: bind as them. The directory decides.
	if err := conn.Bind(userDN, password); err != nil {
		return Identity{}, ErrInvalidCredentials
	}

	identity := Identity{
		Provider: p.cfg.ProviderName,
		// The directory's own identifier, not the typed username: someone may
		// sign in as "areader" or "areader@lexarii.test", and both must land
		// on one account.
		Subject:     firstValue(attrs, p.cfg.UsernameAttr, username),
		Email:       firstValue(attrs, p.cfg.EmailAttr, ""),
		DisplayName: firstValue(attrs, p.cfg.DisplayNameAttr, ""),
		Groups:      attrs[p.cfg.GroupAttr],
		Data:        map[string]any{"dn": userDN},
	}

	if p.cfg.RequiredGroup != "" && !hasGroup(identity.Groups, p.cfg.RequiredGroup) {
		// Authenticated, but not permitted. Reported as invalid credentials so
		// the directory's group membership is not enumerable through this
		// endpoint.
		return Identity{}, ErrInvalidCredentials
	}

	return identity, nil
}

// findUser resolves a username to a DN and its attributes.
func (p *LDAPProvider) findUser(conn *ldap.Conn, username string) (string, map[string][]string, error) {
	escaped := ldap.EscapeFilter(username)

	if p.cfg.UserDNTemplate != "" {
		// Direct bind: no search, so no attributes and no group membership.
		return strings.ReplaceAll(p.cfg.UserDNTemplate, "%s", username), map[string][]string{}, nil
	}

	// Search needs a connection that is bound as something. A service account
	// where one is configured, anonymous otherwise.
	if p.cfg.BindDN != "" {
		if err := conn.Bind(p.cfg.BindDN, p.cfg.BindPassword); err != nil {
			return "", nil, fmt.Errorf("ldap: the service account could not bind: %w", err)
		}
	}

	filter := strings.ReplaceAll(p.cfg.UserFilter, "%s", escaped)
	req := ldap.NewSearchRequest(
		p.cfg.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		2, int(p.cfg.Timeout.Seconds()), false, filter,
		[]string{p.cfg.UsernameAttr, p.cfg.EmailAttr, p.cfg.DisplayNameAttr, p.cfg.GroupAttr},
		nil,
	)

	result, err := conn.Search(req)
	if err != nil {
		return "", nil, ErrInvalidCredentials
	}
	// Exactly one. A filter matching several people is a misconfiguration, and
	// binding as an arbitrary one of them would be worse than refusing.
	if len(result.Entries) != 1 {
		return "", nil, ErrInvalidCredentials
	}

	entry := result.Entries[0]
	attrs := make(map[string][]string, len(entry.Attributes))
	for _, a := range entry.Attributes {
		attrs[a.Name] = a.Values
	}
	return entry.DN, attrs, nil
}

func (p *LDAPProvider) connect(ctx context.Context) (*ldap.Conn, error) {
	tlsConfig := &tls.Config{InsecureSkipVerify: p.cfg.InsecureSkipVerify} //nolint:gosec // opt-in, and logged at startup

	conn, err := ldap.DialURL(p.cfg.URL, ldap.DialWithTLSConfig(tlsConfig))
	if err != nil {
		return nil, err
	}

	if p.cfg.StartTLS && !strings.HasPrefix(p.cfg.URL, "ldaps://") {
		// Upgrade BEFORE any credential crosses the wire.
		if err := conn.StartTLS(tlsConfig); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("starttls: %w", err)
		}
	}

	conn.SetTimeout(p.cfg.Timeout)
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetTimeout(time.Until(deadline))
	}
	return conn, nil
}

// hasGroup reports membership, comparing the whole DN or just the common name
// so a configuration can name either.
func hasGroup(groups []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, g := range groups {
		if strings.EqualFold(g, want) {
			return true
		}
		// "cn=librarians,ou=groups,dc=..." should match "librarians".
		if cn, _, found := strings.Cut(g, ","); found {
			if name, ok := strings.CutPrefix(strings.ToLower(cn), "cn="); ok {
				if strings.EqualFold(name, want) {
					return true
				}
			}
		}
	}
	return false
}

func firstValue(attrs map[string][]string, key, fallback string) string {
	if values, ok := attrs[key]; ok && len(values) > 0 && values[0] != "" {
		return values[0]
	}
	return fallback
}
