// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testIssuer is a working OpenID Connect issuer: discovery, JWKS, and a token
// endpoint that mints real RS256 tokens.
//
// A real signing key rather than a stub, because the point of the test is that
// signature and claim verification actually reject what they should.
type testIssuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string

	// Knobs for forging the tokens a hostile or misconfigured issuer would send.
	issuerOverride   string
	audienceOverride string
	expiry           time.Time
	algOverride      string
	signWithOtherKey bool
	otherKey         *rsa.PrivateKey
	claims           map[string]any
	// dropNonce mints with no nonce claim at all.
	dropNonce bool
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating second key: %v", err)
	}

	iss := &testIssuer{key: key, otherKey: other, kid: "test-key-1"}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := iss.server.URL
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
			"jwks_uri":               base + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA",
				"kid": iss.kid,
				"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			}},
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": iss.mint(t)})
	})

	iss.server = httptest.NewServer(mux)
	t.Cleanup(iss.server.Close)
	return iss
}

// mint builds an ID token, honouring whichever forgery knob is set.
func (i *testIssuer) mint(t *testing.T) string {
	t.Helper()

	alg := i.algOverride
	if alg == "" {
		alg = "RS256"
	}
	header := map[string]string{"alg": alg, "kid": i.kid, "typ": "JWT"}

	issuer := i.issuerOverride
	if issuer == "" {
		issuer = i.server.URL
	}
	audience := i.audienceOverride
	if audience == "" {
		audience = "lexarii-test"
	}
	expiry := i.expiry
	if expiry.IsZero() {
		expiry = time.Now().Add(time.Hour)
	}

	claims := map[string]any{
		"iss":   issuer,
		"aud":   audience,
		"exp":   expiry.Unix(),
		"iat":   time.Now().Unix(),
		"sub":   "subject-12345",
		"nonce": testNonce,
	}
	for k, v := range i.claims {
		claims[k] = v
	}
	if i.dropNonce {
		delete(claims, "nonce")
	}

	encode := func(v any) string {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(data)
	}

	signing := encode(header) + "." + encode(claims)

	signer := i.key
	if i.signWithOtherKey {
		signer = i.otherKey
	}
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, signer, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("signing: %v", err)
	}

	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// testNonce is the nonce the test issuers put in their tokens unless told
// otherwise, and the one the tests hand Exchange.
const testNonce = "nonce-of-this-sign-in"

func newProvider(t *testing.T, iss *testIssuer) *OIDCProvider {
	t.Helper()
	p, err := NewOIDCProvider(OIDCConfig{
		Issuer:     iss.server.URL,
		ClientID:   "lexarii-test",
		HTTPClient: iss.server.Client(),
	})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	return p
}

// The whole point of the abstraction: a provider whose credentials arrive in a
// callback rather than a form.
func TestOIDCImplementsRedirectProvider(t *testing.T) {
	var _ RedirectProvider = (*OIDCProvider)(nil)

	iss := newTestIssuer(t)
	p := newProvider(t, iss)

	if p.Kind() != KindRedirect {
		t.Errorf("Kind = %q, want redirect", p.Kind())
	}
	if p.Name() == "" || p.DisplayName() == "" {
		t.Error("a provider must have a name and a label for the sign-in screen")
	}
}

func TestOIDCAuthURL(t *testing.T) {
	iss := newTestIssuer(t)
	p := newProvider(t, iss)

	got := p.AuthURL("state-value", testNonce, "https://books.example/callback")
	for _, want := range []string{
		"response_type=code",
		"client_id=lexarii-test",
		"state=state-value",
		"nonce=" + testNonce,
		"redirect_uri=https%3A%2F%2Fbooks.example%2Fcallback",
		"scope=openid+profile+email",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("auth URL missing %q:\n%s", want, got)
		}
	}
}

func TestOIDCExchangeReturnsVerifiedIdentity(t *testing.T) {
	iss := newTestIssuer(t)
	iss.claims = map[string]any{
		"email":  "reader@example.com",
		"name":   "A Reader",
		"groups": []any{"librarians", "staff"},
	}
	p := newProvider(t, iss)

	identity, err := p.Exchange(context.Background(), "the-code", testNonce, "https://books.example/callback")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	// The subject must be `sub`, never the email: emails are reassigned between
	// employees, and keying on one hands a new joiner the previous library.
	if identity.Subject != "subject-12345" {
		t.Errorf("Subject = %q, want the sub claim", identity.Subject)
	}
	if identity.Subject == identity.Email {
		t.Error("the subject is the email address")
	}
	if identity.Email != "reader@example.com" {
		t.Errorf("Email = %q", identity.Email)
	}
	if identity.DisplayName != "A Reader" {
		t.Errorf("DisplayName = %q", identity.DisplayName)
	}
	if len(identity.Groups) != 2 {
		t.Errorf("Groups = %v, want both", identity.Groups)
	}
	if identity.Provider != "oidc" {
		t.Errorf("Provider = %q", identity.Provider)
	}
}

// A signature check alone is not verification. Each of these tokens is validly
// signed and must still be refused.
func TestOIDCRejectsInvalidTokens(t *testing.T) {
	tests := map[string]func(*testIssuer){
		"issued by someone else": func(i *testIssuer) {
			i.issuerOverride = "https://evil.example"
		},
		"for a different client": func(i *testIssuer) {
			i.audienceOverride = "some-other-app"
		},
		"expired": func(i *testIssuer) {
			i.expiry = time.Now().Add(-time.Hour)
		},
		"signed with the wrong key": func(i *testIssuer) {
			i.signWithOtherKey = true
		},
		"alg none": func(i *testIssuer) {
			i.algOverride = "none"
		},
		"HMAC where RSA is expected": func(i *testIssuer) {
			i.algOverride = "HS256"
		},
		"no subject": func(i *testIssuer) {
			i.claims = map[string]any{"sub": ""}
		},
	}

	for name, forge := range tests {
		t.Run(name, func(t *testing.T) {
			iss := newTestIssuer(t)
			forge(iss)
			p := newProvider(t, iss)

			if _, err := p.Exchange(context.Background(), "code", testNonce, "https://books.example/cb"); err == nil {
				t.Fatal("a token that should have been rejected was accepted")
			}
		})
	}
}

// The nonce ties an ID token to one sign-in. Each of these tokens is validly
// signed, for this client, by this issuer, unexpired - and was not issued for
// the sign-in presenting it.
//
// Until this was fixed the nonce sent was the state, and nothing read the
// claim back: a token captured from one person's sign-in could be replayed
// through anybody else's callback.
func TestOIDCRejectsATokenForAnotherSignIn(t *testing.T) {
	tests := map[string]struct {
		claims map[string]any
		drop   bool   // mint with no nonce claim at all
		want   string // the nonce this sign-in kept
	}{
		"replayed from another sign-in":  {claims: map[string]any{"nonce": "somebody-elses-nonce"}, want: testNonce},
		"no nonce claim":                 {drop: true, want: testNonce},
		"an empty nonce claim":           {claims: map[string]any{"nonce": ""}, want: testNonce},
		"a nonce that is not a string":   {claims: map[string]any{"nonce": 12345}, want: testNonce},
		"a prefix of the kept nonce":     {claims: map[string]any{"nonce": testNonce[:5]}, want: testNonce},
		"no nonce kept for this sign-in": {drop: true, want: ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			iss := newTestIssuer(t)
			iss.claims = tc.claims
			iss.dropNonce = tc.drop
			p := newProvider(t, iss)

			_, err := p.Exchange(t.Context(), "code", tc.want, "https://books.example/cb")
			if !errors.Is(err, ErrNonceMismatch) {
				t.Fatalf("Exchange = %v, want ErrNonceMismatch", err)
			}
		})
	}

	t.Run("the same token for the sign-in it was issued for", func(t *testing.T) {
		iss := newTestIssuer(t)
		p := newProvider(t, iss)
		if _, err := p.Exchange(t.Context(), "code", testNonce, "https://books.example/cb"); err != nil {
			t.Fatalf("Exchange: %v", err)
		}
	})
}

// The nonce and the state are different values: the state comes back in the
// callback URL, and a nonce equal to it would be readable by anyone who saw
// that URL.
func TestOIDCAuthURLSendsTheNonceItWasGiven(t *testing.T) {
	iss := newTestIssuer(t)
	p := newProvider(t, iss)

	raw := p.AuthURL("the-state", "the-nonce", "https://books.example/cb")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("AuthURL %q: %v", raw, err)
	}
	if got := u.Query().Get("state"); got != "the-state" {
		t.Errorf("state = %q", got)
	}
	if got := u.Query().Get("nonce"); got != "the-nonce" {
		t.Errorf("nonce = %q, want the nonce, not the state", got)
	}
}

// A token with no expiry must be refused rather than treated as never expiring.
func TestOIDCRejectsTokenWithoutExpiry(t *testing.T) {
	iss := newTestIssuer(t)
	p := newProvider(t, iss)

	// Mint by hand so exp can be omitted entirely.
	encode := func(v any) string {
		data, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(data)
	}
	signing := encode(map[string]string{"alg": "RS256", "kid": iss.kid}) + "." +
		encode(map[string]any{"iss": iss.server.URL, "aud": "lexarii-test", "sub": "x", "nonce": testNonce})
	digest := sha256.Sum256([]byte(signing))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, iss.key, crypto.SHA256, digest[:])
	token := signing + "." + base64.RawURLEncoding.EncodeToString(sig)

	if _, err := p.verifyIDToken(context.Background(), token, testNonce); err == nil {
		t.Fatal("a token with no expiry was accepted")
	}
}

// Issuers rotate keys without warning, so an unknown kid must trigger a refetch
// rather than break every sign-in at the moment of rotation.
func TestOIDCRefetchesKeysOnRotation(t *testing.T) {
	iss := newTestIssuer(t)
	p := newProvider(t, iss)

	if _, err := p.Exchange(context.Background(), "code", testNonce, "https://books.example/cb"); err != nil {
		t.Fatalf("first exchange: %v", err)
	}

	// The issuer rotates: new key id, and the JWKS now serves it.
	iss.kid = "test-key-2"
	p.mu.Lock()
	p.keysAt = time.Time{} // allow an immediate refetch
	p.mu.Unlock()

	if _, err := p.Exchange(context.Background(), "code", testNonce, "https://books.example/cb"); err != nil {
		t.Fatalf("after key rotation: %v", err)
	}
}

// Signing up on first sign-in is off by default: against a public issuer it
// would let anyone with an account there into the library.
func TestOIDCSignupIsOffByDefault(t *testing.T) {
	iss := newTestIssuer(t)
	p := newProvider(t, iss)

	if p.AllowSignup() {
		t.Fatal("account creation on first sign-in is enabled by default")
	}
}

func TestOIDCRequiresIssuerAndClient(t *testing.T) {
	if _, err := NewOIDCProvider(OIDCConfig{ClientID: "x"}); err == nil {
		t.Error("a provider with no issuer was accepted")
	}
	if _, err := NewOIDCProvider(OIDCConfig{Issuer: "https://example"}); err == nil {
		t.Error("a provider with no client id was accepted")
	}
}

func TestAudienceMatches(t *testing.T) {
	cases := []struct {
		aud  any
		want bool
	}{
		{"lexarii", true},
		{"other", false},
		{[]any{"other", "lexarii"}, true},
		{[]any{"other"}, false},
		{[]any{}, false},
		{nil, false},
		{42, false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.aud), func(t *testing.T) {
			if got := audienceMatches(c.aud, "lexarii"); got != c.want {
				t.Fatalf("audienceMatches(%v) = %v, want %v", c.aud, got, c.want)
			}
		})
	}
}

func TestNewStateIsUnguessable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		s, err := NewState()
		if err != nil {
			t.Fatalf("NewState: %v", err)
		}
		if len(s) < 40 {
			t.Fatalf("state %q is too short to be unguessable", s)
		}
		if seen[s] {
			t.Fatal("NewState repeated a value")
		}
		seen[s] = true
	}
}
