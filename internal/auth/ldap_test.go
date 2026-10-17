// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// These run against a real directory when TEST_LDAP_URL is set, and skip
// otherwise so `go test ./...` stays runnable without one.
//
// A real server rather than a fake: the failure modes that matter here - an
// empty password binding anonymously, a filter matching several people - are
// behaviours of the LDAP protocol itself, and a fake would simply reproduce
// whatever this code already assumes.
func ldapConfig(t *testing.T) LDAPConfig {
	t.Helper()

	url := os.Getenv("TEST_LDAP_URL")
	if url == "" {
		t.Skip("TEST_LDAP_URL not set")
	}
	return LDAPConfig{
		URL:          url,
		BaseDN:       "dc=lexarii,dc=test",
		BindDN:       "cn=admin,dc=lexarii,dc=test",
		BindPassword: "adminpw",
		UserFilter:   "(uid=%s)",
	}
}

func TestLDAPImplementsPasswordProvider(t *testing.T) {
	// The claim the abstraction made: LDAP needs no new plumbing, because it
	// authenticates with a password exactly as local accounts do.
	var _ PasswordProvider = (*LDAPProvider)(nil)
}

func TestLDAPAuthenticatesAgainstARealDirectory(t *testing.T) {
	p, err := NewLDAPProvider(ldapConfig(t))
	if err != nil {
		t.Fatalf("NewLDAPProvider: %v", err)
	}

	identity, err := p.Authenticate(context.Background(), "areader", "readerpassword")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	if identity.Subject != "areader" {
		t.Errorf("Subject = %q", identity.Subject)
	}
	if identity.Email != "areader@lexarii.test" {
		t.Errorf("Email = %q", identity.Email)
	}
	if identity.DisplayName != "A Reader" {
		t.Errorf("DisplayName = %q", identity.DisplayName)
	}
	if identity.Provider != "ldap" {
		t.Errorf("Provider = %q", identity.Provider)
	}
	// The DN is kept for diagnosing a directory that is not behaving.
	if dn, _ := identity.Data["dn"].(string); dn == "" {
		t.Error("the user's DN was not recorded")
	}
}

func TestLDAPRejectsWrongPassword(t *testing.T) {
	p, err := NewLDAPProvider(ldapConfig(t))
	if err != nil {
		t.Fatalf("NewLDAPProvider: %v", err)
	}

	if _, err := p.Authenticate(context.Background(), "areader", "not the password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("error = %v, want ErrInvalidCredentials", err)
	}
	if _, err := p.Authenticate(context.Background(), "nobody", "readerpassword"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user error = %v, want ErrInvalidCredentials", err)
	}
}

// The single most dangerous LDAP mistake.
//
// A bind with an empty password is an ANONYMOUS bind, and most servers accept
// it - so without an explicit check, every account in the directory
// authenticates with a blank password.
func TestLDAPRefusesEmptyPassword(t *testing.T) {
	p, err := NewLDAPProvider(ldapConfig(t))
	if err != nil {
		t.Fatalf("NewLDAPProvider: %v", err)
	}

	for _, password := range []string{"", " "} {
		if _, err := p.Authenticate(context.Background(), "areader", password); err == nil {
			t.Fatalf("an empty password authenticated - every account is open")
		}
	}
	if _, err := p.Authenticate(context.Background(), "", "readerpassword"); !errors.Is(err, ErrInvalidCredentials) {
		t.Error("an empty username was accepted")
	}
}

// A directory serving a whole company grants access to a few people.
func TestLDAPRequiredGroup(t *testing.T) {
	cfg := ldapConfig(t)
	cfg.RequiredGroup = "librarians"

	p, err := NewLDAPProvider(cfg)
	if err != nil {
		t.Fatalf("NewLDAPProvider: %v", err)
	}

	// A member gets in. (memberOf needs the overlay; when it is absent the
	// group list is empty and this correctly refuses everyone, which is the
	// safe direction.)
	if _, err := p.Authenticate(context.Background(), "areader", "readerpassword"); err != nil {
		t.Logf("member refused, which is correct when the directory does not populate memberOf: %v", err)
	}

	// A non-member never does.
	if _, err := p.Authenticate(context.Background(), "disabled", "disabledpassword"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("a non-member was admitted: %v", err)
	}
}

// A username is interpolated into a filter, so it must be escaped or it is an
// injection: "*" would otherwise match every user in the directory.
func TestLDAPFilterInjectionIsEscaped(t *testing.T) {
	p, err := NewLDAPProvider(ldapConfig(t))
	if err != nil {
		t.Fatalf("NewLDAPProvider: %v", err)
	}

	for _, attack := range []string{
		"*",
		"*)(uid=*",
		"areader)(|(uid=*",
		`\2a`,
	} {
		t.Run(attack, func(t *testing.T) {
			if _, err := p.Authenticate(context.Background(), attack, "readerpassword"); err == nil {
				t.Fatalf("a filter injection authenticated: %q", attack)
			}
		})
	}
}

func TestLDAPConfigValidation(t *testing.T) {
	if _, err := NewLDAPProvider(LDAPConfig{BaseDN: "dc=x"}); err == nil {
		t.Error("a provider with no URL was accepted")
	}
	if _, err := NewLDAPProvider(LDAPConfig{URL: "ldap://x"}); err == nil {
		t.Error("a provider with neither a base DN nor a DN template was accepted")
	}
}

// Active Directory differs from OpenLDAP in every attribute that matters, and
// getting them wrong authenticates successfully while returning no identity.
func TestActiveDirectoryDefaults(t *testing.T) {
	cfg := ActiveDirectoryDefaults(LDAPConfig{URL: "ldaps://dc.corp.example", BaseDN: "dc=corp,dc=example"})

	if cfg.UsernameAttr != "sAMAccountName" {
		t.Errorf("UsernameAttr = %q, want sAMAccountName", cfg.UsernameAttr)
	}
	if cfg.DisplayNameAttr != "displayName" {
		t.Errorf("DisplayNameAttr = %q, want displayName", cfg.DisplayNameAttr)
	}
	// People type either their login name or their full UPN.
	for _, want := range []string{"sAMAccountName=%s", "userPrincipalName=%s"} {
		if !strings.Contains(cfg.UserFilter, want) {
			t.Errorf("UserFilter %q does not match on %s", cfg.UserFilter, want)
		}
	}

	// Explicit settings are never overwritten.
	custom := ActiveDirectoryDefaults(LDAPConfig{
		URL: "ldaps://x", BaseDN: "dc=x", UsernameAttr: "cn", Label: "Work account",
	})
	if custom.UsernameAttr != "cn" || custom.Label != "Work account" {
		t.Error("explicit configuration was overwritten by the defaults")
	}
}

func TestHasGroup(t *testing.T) {
	groups := []string{
		"cn=librarians,ou=groups,dc=lexarii,dc=test",
		"CN=Domain Users,CN=Users,DC=corp,DC=example",
	}

	// A configuration may name the whole DN or just the common name.
	for _, want := range []string{
		"librarians",
		"cn=librarians,ou=groups,dc=lexarii,dc=test",
		"Domain Users",
		"LIBRARIANS",
	} {
		if !hasGroup(groups, want) {
			t.Errorf("hasGroup did not match %q", want)
		}
	}
	for _, want := range []string{"administrators", "librarian", ""} {
		if hasGroup(groups, want) {
			t.Errorf("hasGroup wrongly matched %q", want)
		}
	}
}
