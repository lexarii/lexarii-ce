// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/go-ldap/ldap/v3"
)

// fakeLDAPDirectory is a minimal LDAP server on loopback: simple bind, search,
// StartTLS (always refused) and unbind, speaking real BER so the provider's
// own go-ldap client is what is tested.
//
// It does not evaluate filters. It answers an equality filter on uid with the
// entries whose uid is the asserted value, which is all the provider sends by
// default, and records every filter so a test can see exactly what was asked.
// The behaviours ldap_test.go wants a real directory for - what a real server
// does with an anonymous bind, say - stay there; this covers what the provider
// does with the answers.
type fakeLDAPDirectory struct {
	listener net.Listener
	wg       sync.WaitGroup

	// serviceDN and servicePassword are the account a search binds as.
	serviceDN       string
	servicePassword string
	entries         []fakeLDAPEntry
	// searchResult, when non-zero, fails every search with that result code.
	searchResult int64

	mu          sync.Mutex
	connections int
	binds       []string // the DN of every bind attempted, in order
	searches    []fakeLDAPSearch
	startTLS    int
}

type fakeLDAPEntry struct {
	dn       string
	password string
	attrs    map[string][]string
}

type fakeLDAPSearch struct {
	baseDN     string
	equality   bool   // the filter is a single equality match
	attribute  string // and on this attribute
	value      string // asserting this literal value
	filter     string // the filter as go-ldap decompiles it
	attributes []string
}

func newFakeLDAPDirectory(t *testing.T) *fakeLDAPDirectory {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	d := &fakeLDAPDirectory{listener: l}
	d.wg.Add(1)
	go d.accept()
	t.Cleanup(func() {
		_ = l.Close()
		d.wg.Wait()
	})
	return d
}

func (d *fakeLDAPDirectory) url() string { return "ldap://" + d.listener.Addr().String() }

func (d *fakeLDAPDirectory) accept() {
	defer d.wg.Done()
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		d.mu.Lock()
		d.connections++
		d.mu.Unlock()
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer func() { _ = conn.Close() }()
			d.serve(conn)
		}()
	}
}

func (d *fakeLDAPDirectory) serve(conn net.Conn) {
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		packet, err := ber.ReadPacket(conn)
		if err != nil || len(packet.Children) < 2 {
			return
		}
		id, ok := packet.Children[0].Value.(int64)
		if !ok {
			return
		}
		op := packet.Children[1]

		var replies []*ber.Packet
		switch op.Tag {
		case ldap.ApplicationBindRequest:
			replies = []*ber.Packet{d.bind(id, op)}
		case ldap.ApplicationSearchRequest:
			replies = d.search(id, op)
		case ldap.ApplicationExtendedRequest:
			d.mu.Lock()
			d.startTLS++
			d.mu.Unlock()
			replies = []*ber.Packet{ldapResult(id, ldap.ApplicationExtendedResponse, ldap.LDAPResultProtocolError)}
		case ldap.ApplicationUnbindRequest:
			return
		default:
			return
		}
		for _, r := range replies {
			if _, err := conn.Write(r.Bytes()); err != nil {
				return
			}
		}
	}
}

func (d *fakeLDAPDirectory) bind(id int64, op *ber.Packet) *ber.Packet {
	dn := op.Children[1].Data.String()
	password := op.Children[2].Data.String()

	d.mu.Lock()
	d.binds = append(d.binds, dn)
	d.mu.Unlock()

	code := int64(ldap.LDAPResultInvalidCredentials)
	if dn == d.serviceDN && password == d.servicePassword && dn != "" {
		code = ldap.LDAPResultSuccess
	}
	for _, e := range d.entries {
		if strings.EqualFold(e.dn, dn) && e.password == password {
			code = ldap.LDAPResultSuccess
		}
	}
	return ldapResult(id, ldap.ApplicationBindResponse, code)
}

func (d *fakeLDAPDirectory) search(id int64, op *ber.Packet) []*ber.Packet {
	filterPacket := op.Children[6]
	s := fakeLDAPSearch{baseDN: op.Children[0].Data.String()}
	s.filter, _ = ldap.DecompileFilter(filterPacket)
	if filterPacket.Tag == ldap.FilterEqualityMatch && len(filterPacket.Children) == 2 {
		s.equality = true
		s.attribute = filterPacket.Children[0].Data.String()
		s.value = filterPacket.Children[1].Data.String()
	}
	for _, a := range op.Children[7].Children {
		s.attributes = append(s.attributes, a.Data.String())
	}

	d.mu.Lock()
	d.searches = append(d.searches, s)
	d.mu.Unlock()

	if d.searchResult != 0 {
		return []*ber.Packet{ldapResult(id, ldap.ApplicationSearchResultDone, d.searchResult)}
	}

	var replies []*ber.Packet
	for _, e := range d.entries {
		if s.equality && s.attribute == "uid" && len(e.attrs["uid"]) > 0 && e.attrs["uid"][0] == s.value {
			replies = append(replies, ldapEntry(id, e))
		}
	}
	return append(replies, ldapResult(id, ldap.ApplicationSearchResultDone, ldap.LDAPResultSuccess))
}

func (d *fakeLDAPDirectory) seen() (connections int, binds []string, searches []fakeLDAPSearch, startTLS int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.connections, append([]string(nil), d.binds...), append([]fakeLDAPSearch(nil), d.searches...), d.startTLS
}

func ldapEnvelope(id int64, op *ber.Packet) *ber.Packet {
	p := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "LDAP Response")
	p.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, "MessageID"))
	p.AppendChild(op)
	return p
}

func ldapResult(id int64, tag ber.Tag, code int64) *ber.Packet {
	op := ber.Encode(ber.ClassApplication, ber.TypeConstructed, tag, nil, "Result")
	op.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, code, "resultCode"))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "matchedDN"))
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", "diagnosticMessage"))
	return ldapEnvelope(id, op)
}

func ldapEntry(id int64, e fakeLDAPEntry) *ber.Packet {
	op := ber.Encode(ber.ClassApplication, ber.TypeConstructed, ldap.ApplicationSearchResultEntry, nil, "Entry")
	op.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, e.dn, "objectName"))
	attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "attributes")
	names := make([]string, 0, len(e.attrs))
	for name := range e.attrs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		a := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "attribute")
		a.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, name, "type"))
		vals := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "vals")
		for _, v := range e.attrs[name] {
			vals.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, v, "value"))
		}
		a.AppendChild(vals)
		attrs.AppendChild(a)
	}
	op.AppendChild(attrs)
	return ldapEnvelope(id, op)
}

const (
	fakeLDAPServiceDN = "cn=svc,dc=lexarii,dc=test"
	fakeLDAPReaderDN  = "uid=areader,ou=people,dc=lexarii,dc=test"
)

// directoryWithReader is a directory holding one person, a librarian.
func directoryWithReader(t *testing.T) *fakeLDAPDirectory {
	t.Helper()
	d := newFakeLDAPDirectory(t)
	d.serviceDN, d.servicePassword = fakeLDAPServiceDN, "service-secret"
	d.entries = []fakeLDAPEntry{{
		dn:       fakeLDAPReaderDN,
		password: "reader-secret",
		attrs: map[string][]string{
			"uid":      {"areader"},
			"mail":     {"areader@lexarii.test"},
			"cn":       {"A Reader"},
			"memberOf": {"cn=librarians,ou=groups,dc=lexarii,dc=test"},
		},
	}}
	return d
}

func ldapProviderFor(t *testing.T, d *fakeLDAPDirectory, edit func(*LDAPConfig)) *LDAPProvider {
	t.Helper()
	cfg := LDAPConfig{
		URL:          d.url(),
		BaseDN:       "ou=people,dc=lexarii,dc=test",
		BindDN:       fakeLDAPServiceDN,
		BindPassword: "service-secret",
		Timeout:      5 * time.Second,
	}
	if edit != nil {
		edit(&cfg)
	}
	p, err := NewLDAPProvider(cfg)
	if err != nil {
		t.Fatalf("NewLDAPProvider: %v", err)
	}
	return p
}

func TestLDAPDefaultsSuitOpenLDAP(t *testing.T) {
	p, err := NewLDAPProvider(LDAPConfig{URL: "ldap://x", BaseDN: "dc=x"})
	if err != nil {
		t.Fatalf("NewLDAPProvider: %v", err)
	}
	if p.Name() != "ldap" || p.DisplayName() != "Directory" || p.Kind() != KindPassword {
		t.Errorf("name %q label %q kind %q", p.Name(), p.DisplayName(), p.Kind())
	}
	if p.AllowSignup() {
		t.Error("signup on first sign-in is on by default")
	}
	c := p.cfg
	if c.UserFilter != "(uid=%s)" || c.UsernameAttr != "uid" || c.EmailAttr != "mail" ||
		c.DisplayNameAttr != "cn" || c.GroupAttr != "memberOf" || c.Timeout != 10*time.Second {
		t.Errorf("defaults = %+v", c)
	}

	ad, err := NewLDAPProvider(ActiveDirectoryDefaults(LDAPConfig{URL: "ldaps://dc", BaseDN: "dc=corp", AllowSignup: true}))
	if err != nil {
		t.Fatalf("NewLDAPProvider(AD): %v", err)
	}
	if ad.Name() != "ad" || ad.DisplayName() != "Active Directory" || !ad.AllowSignup() {
		t.Errorf("AD name %q label %q signup %v", ad.Name(), ad.DisplayName(), ad.AllowSignup())
	}
}

func TestLDAPAuthenticatesBySearchingThenBinding(t *testing.T) {
	d := directoryWithReader(t)
	p := ldapProviderFor(t, d, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	identity, err := p.Authenticate(ctx, "  areader ", "reader-secret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	want := Identity{Provider: "ldap", Subject: "areader", Email: "areader@lexarii.test", DisplayName: "A Reader"}
	if identity.Provider != want.Provider || identity.Subject != want.Subject ||
		identity.Email != want.Email || identity.DisplayName != want.DisplayName {
		t.Errorf("identity = %+v, want %+v", identity, want)
	}
	if len(identity.Groups) != 1 || identity.Data["dn"] != fakeLDAPReaderDN {
		t.Errorf("groups %v dn %v", identity.Groups, identity.Data["dn"])
	}

	_, binds, searches, _ := d.seen()
	// The service account searches; then the person is bound as themselves.
	if len(binds) != 2 || binds[0] != fakeLDAPServiceDN || binds[1] != fakeLDAPReaderDN {
		t.Errorf("binds = %v, want the service account then the reader", binds)
	}
	if len(searches) != 1 {
		t.Fatalf("searches = %d, want 1", len(searches))
	}
	s := searches[0]
	if s.baseDN != "ou=people,dc=lexarii,dc=test" || !s.equality || s.attribute != "uid" || s.value != "areader" {
		t.Errorf("search = %+v, want uid=areader under the base DN (trimmed)", s)
	}
	sort.Strings(s.attributes)
	if strings.Join(s.attributes, ",") != "cn,mail,memberOf,uid" {
		t.Errorf("asked for attributes %v", s.attributes)
	}
}

// Without a service account the search is anonymous: the only bind is the
// person's own.
func TestLDAPAnonymousSearch(t *testing.T) {
	d := directoryWithReader(t)
	p := ldapProviderFor(t, d, func(c *LDAPConfig) { c.BindDN, c.BindPassword = "", "" })

	if _, err := p.Authenticate(t.Context(), "areader", "reader-secret"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if _, binds, _, _ := d.seen(); len(binds) != 1 || binds[0] != fakeLDAPReaderDN {
		t.Errorf("binds = %v, want only the reader's", binds)
	}
}

// Every way of not getting in looks the same from outside: a wrong password,
// nobody by that name, a search the directory refuses, a filter that matches
// more than one person, and somebody outside the required group.
func TestLDAPRefusalsAreAllInvalidCredentials(t *testing.T) {
	tests := []struct {
		name     string
		user     string
		password string
		setup    func(d *fakeLDAPDirectory)
		edit     func(c *LDAPConfig)
		// userBind says whether the directory should have been asked to bind
		// as the person at all.
		userBind bool
	}{
		{name: "wrong password", user: "areader", password: "guess", userBind: true},
		{name: "no such person", user: "nobody", password: "reader-secret"},
		{
			name: "the directory refuses the search", user: "areader", password: "reader-secret",
			setup: func(d *fakeLDAPDirectory) { d.searchResult = ldap.LDAPResultNoSuchObject },
		},
		{
			// Binding as an arbitrary one of two matches would be worse than
			// refusing.
			name: "two people match", user: "areader", password: "reader-secret",
			setup: func(d *fakeLDAPDirectory) {
				twin := d.entries[0]
				twin.dn = "uid=areader,ou=elsewhere,dc=lexarii,dc=test"
				d.entries = append(d.entries, twin)
			},
		},
		{
			// Authenticated, but not permitted, and not distinguishable from a
			// wrong password, so group membership cannot be probed.
			name: "not in the required group", user: "areader", password: "reader-secret",
			edit:     func(c *LDAPConfig) { c.RequiredGroup = "administrators" },
			userBind: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := directoryWithReader(t)
			if tc.setup != nil {
				tc.setup(d)
			}
			p := ldapProviderFor(t, d, tc.edit)

			identity, err := p.Authenticate(t.Context(), tc.user, tc.password)
			if !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("error = %v, want ErrInvalidCredentials", err)
			}
			if identity.Subject != "" {
				t.Errorf("a refusal returned an identity: %+v", identity)
			}
			_, binds, _, _ := d.seen()
			bound := false
			for _, b := range binds {
				if b != fakeLDAPServiceDN {
					bound = true
				}
			}
			if bound != tc.userBind {
				t.Errorf("binds = %v; bound as the person: %v, want %v", binds, bound, tc.userBind)
			}
		})
	}
}

func TestLDAPRequiredGroupAdmitsAMember(t *testing.T) {
	d := directoryWithReader(t)
	p := ldapProviderFor(t, d, func(c *LDAPConfig) { c.RequiredGroup = "Librarians" })

	if _, err := p.Authenticate(t.Context(), "areader", "reader-secret"); err != nil {
		t.Fatalf("a member was refused: %v", err)
	}
}

// A username is escaped before it reaches the filter: the directory is asked
// for somebody literally called "*", never for everybody.
func TestLDAPUsernameReachesTheFilterAsALiteral(t *testing.T) {
	for _, attack := range []string{"*", "*)(uid=*", "areader)(|(uid=*", `\2a`} {
		t.Run(attack, func(t *testing.T) {
			d := directoryWithReader(t)
			p := ldapProviderFor(t, d, nil)

			if _, err := p.Authenticate(t.Context(), attack, "reader-secret"); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("error = %v, want ErrInvalidCredentials", err)
			}
			_, _, searches, _ := d.seen()
			if len(searches) != 1 {
				t.Fatalf("searches = %d, want 1", len(searches))
			}
			s := searches[0]
			if !s.equality || s.attribute != "uid" || s.value != attack {
				t.Errorf("filter %q (equality %v, value %q), want one equality match on the literal %q",
					s.filter, s.equality, s.value, attack)
			}
		})
	}
}

// A service account that cannot bind is the installation's fault, not the
// person's: it is reported as a directory error, never as a wrong password.
func TestLDAPServiceAccountFailureIsNotInvalidCredentials(t *testing.T) {
	d := directoryWithReader(t)
	p := ldapProviderFor(t, d, func(c *LDAPConfig) { c.BindPassword = "rotated-and-forgotten" })

	_, err := p.Authenticate(t.Context(), "areader", "reader-secret")
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("error = %v, want a configuration error", err)
	}
	var ldapErr *ldap.Error
	if !errors.As(err, &ldapErr) || ldapErr.ResultCode != ldap.LDAPResultInvalidCredentials {
		t.Errorf("error = %v, want the directory's own result wrapped", err)
	}
	if _, binds, searches, _ := d.seen(); len(binds) != 1 || len(searches) != 0 {
		t.Errorf("binds %v searches %d, want nothing after the failed service bind", binds, len(searches))
	}
}

// With a DN template there is no search: the person is bound directly, and
// the typed name is all there is to call them.
func TestLDAPDirectBindWithADNTemplate(t *testing.T) {
	d := directoryWithReader(t)
	p := ldapProviderFor(t, d, func(c *LDAPConfig) {
		c.BaseDN = ""
		c.BindDN, c.BindPassword = "", ""
		c.UserDNTemplate = "uid=%s,ou=people,dc=lexarii,dc=test"
	})

	identity, err := p.Authenticate(t.Context(), "areader", "reader-secret")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.Subject != "areader" || identity.Email != "" || len(identity.Groups) != 0 {
		t.Errorf("identity = %+v, want just the typed name", identity)
	}
	_, binds, searches, _ := d.seen()
	if len(searches) != 0 || len(binds) != 1 || binds[0] != fakeLDAPReaderDN {
		t.Errorf("binds %v searches %d, want one direct bind", binds, len(searches))
	}
}

// An empty password is an anonymous bind, which most directories accept. It
// must never get as far as the directory at all.
func TestLDAPEmptyCredentialsNeverReachTheDirectory(t *testing.T) {
	d := directoryWithReader(t)
	p := ldapProviderFor(t, d, nil)

	for _, creds := range [][2]string{{"areader", ""}, {"", "reader-secret"}, {"   ", "reader-secret"}} {
		if _, err := p.Authenticate(t.Context(), creds[0], creds[1]); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("Authenticate(%q, %q) = %v, want ErrInvalidCredentials", creds[0], creds[1], err)
		}
	}
	if connections, _, _, _ := d.seen(); connections != 0 {
		t.Errorf("the directory saw %d connections, want none", connections)
	}
}

// StartTLS that fails must stop everything: no credential crosses a
// connection that was supposed to be encrypted and is not.
func TestLDAPFailedStartTLSSendsNoCredentials(t *testing.T) {
	d := directoryWithReader(t)
	p := ldapProviderFor(t, d, func(c *LDAPConfig) { c.StartTLS = true })

	_, err := p.Authenticate(t.Context(), "areader", "reader-secret")
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("error = %v, want a connection error", err)
	}
	_, binds, _, startTLS := d.seen()
	if startTLS != 1 {
		t.Errorf("StartTLS requests = %d, want 1", startTLS)
	}
	if len(binds) != 0 {
		t.Errorf("binds after a failed StartTLS: %v", binds)
	}
}

// A directory that cannot be reached is an outage, not a wrong password.
func TestLDAPUnreachableDirectoryIsNotInvalidCredentials(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	p, err := NewLDAPProvider(LDAPConfig{URL: "ldap://" + addr, BaseDN: "dc=x", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Authenticate(t.Context(), "areader", "reader-secret")
	if err == nil || errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("error = %v, want a connection error", err)
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Errorf("error = %v, want the dial failure wrapped", err)
	}
}

func TestFirstValue(t *testing.T) {
	attrs := map[string][]string{"mail": {"a@x", "b@x"}, "cn": {""}, "none": {}}
	for _, tc := range []struct{ key, want string }{
		{"mail", "a@x"},
		{"cn", "fallback"},   // an empty value is no value
		{"none", "fallback"}, // and so is none at all
		{"absent", "fallback"},
	} {
		if got := firstValue(attrs, tc.key, "fallback"); got != tc.want {
			t.Errorf("firstValue(%q) = %q, want %q", tc.key, got, tc.want)
		}
	}
}
