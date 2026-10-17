// SPDX-License-Identifier: AGPL-3.0-or-later

package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// oidcFakeIssuer is an OpenID Connect issuer whose every endpoint can be made
// to misbehave, and which counts what it is asked. testIssuer in oidc_test.go
// forges tokens; this one breaks the conversation around them.
type oidcFakeIssuer struct {
	server *httptest.Server
	key    *rsa.PrivateKey

	mu sync.Mutex
	// discovery, when set, replaces the discovery document.
	discovery     map[string]string
	discoveryCode int
	discoveryRaw  string
	// token, when set, is the raw body of the token response.
	tokenRaw  string
	tokenCode int
	idToken   string
	tokenForm url.Values
	// jwksRaw, when set, is the raw body of the key set.
	jwksRaw    string
	jwksCode   int
	jwksFetchs int
}

var (
	oidcFakeKeyOnce sync.Once
	oidcFakeKey     *rsa.PrivateKey
)

// oidcFakeSigningKey is generated once for the whole file: RSA generation is
// the slow part, and nothing here depends on the key being fresh.
func oidcFakeSigningKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	oidcFakeKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		oidcFakeKey = k
	})
	return oidcFakeKey
}

func newOIDCFakeIssuer(t *testing.T) *oidcFakeIssuer {
	t.Helper()
	f := &oidcFakeIssuer{key: oidcFakeSigningKey(t)}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.discoveryCode != 0 {
			w.WriteHeader(f.discoveryCode)
		}
		if f.discoveryRaw != "" {
			_, _ = w.Write([]byte(f.discoveryRaw))
			return
		}
		doc := map[string]string{
			"issuer":                 f.server.URL,
			"authorization_endpoint": f.server.URL + "/authorize",
			"token_endpoint":         f.server.URL + "/token",
			"jwks_uri":               f.server.URL + "/jwks",
		}
		for k, v := range f.discovery {
			doc[k] = v
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := r.ParseForm(); err == nil {
			f.tokenForm = r.PostForm
		}
		if f.tokenCode != 0 {
			w.WriteHeader(f.tokenCode)
		}
		if f.tokenRaw != "" {
			_, _ = w.Write([]byte(f.tokenRaw))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": f.idToken})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.jwksFetchs++
		if f.jwksCode != 0 {
			w.WriteHeader(f.jwksCode)
		}
		if f.jwksRaw != "" {
			_, _ = w.Write([]byte(f.jwksRaw))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{oidcFakeJWK("k1", &f.key.PublicKey)}})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func oidcFakeJWK(kid string, pub *rsa.PublicKey) map[string]string {
	return map[string]string{
		"kty": "RSA", "kid": kid,
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func oidcSegment(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

// sign signs header.payload segments as given, so a test can sign a payload
// that is not JSON at all.
func (f *oidcFakeIssuer) sign(t *testing.T, header, payload string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(header + "." + payload))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("signing: %v", err)
	}
	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// token is a valid ID token for the provider, with claims overridden.
func (f *oidcFakeIssuer) token(t *testing.T, kid string, claims map[string]any) string {
	t.Helper()
	all := map[string]any{
		"iss": f.server.URL, "aud": "lexarii-test", "sub": "subject-1",
		"exp": time.Now().Add(time.Hour).Unix(), "nonce": testNonce,
	}
	for k, v := range claims {
		all[k] = v
	}
	return f.sign(t, oidcSegment(t, map[string]string{"alg": "RS256", "kid": kid}), oidcSegment(t, all))
}

func (f *oidcFakeIssuer) set(fn func(f *oidcFakeIssuer)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *oidcFakeIssuer) provider(t *testing.T, edit func(*OIDCConfig)) *OIDCProvider {
	t.Helper()
	cfg := OIDCConfig{Issuer: f.server.URL + "/", ClientID: "lexarii-test", HTTPClient: f.server.Client()}
	if edit != nil {
		edit(&cfg)
	}
	p, err := NewOIDCProvider(cfg)
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	return p
}

// closedURL is a loopback address with nothing listening.
func closedURL(t *testing.T) string {
	t.Helper()
	s := httptest.NewServer(http.NotFoundHandler())
	u := s.URL
	s.Close()
	return u
}

func TestOIDCDefaults(t *testing.T) {
	p, err := NewOIDCProvider(OIDCConfig{Issuer: "https://id.example", ClientID: "c", AllowSignup: true})
	if err != nil {
		t.Fatalf("NewOIDCProvider: %v", err)
	}
	if p.Name() != "oidc" || p.DisplayName() != "Single sign-on" || !p.AllowSignup() {
		t.Errorf("name %q label %q signup %v", p.Name(), p.DisplayName(), p.AllowSignup())
	}
	if strings.Join(p.cfg.Scopes, " ") != "openid profile email" {
		t.Errorf("scopes = %v", p.cfg.Scopes)
	}
	// An issuer that never answers must not hang a sign-in for ever.
	if p.cfg.HTTPClient == nil || p.cfg.HTTPClient.Timeout <= 0 {
		t.Errorf("default HTTP client = %+v, want one with a timeout", p.cfg.HTTPClient)
	}
}

func TestOIDCAuthURLEdges(t *testing.T) {
	t.Run("an issuer that cannot be discovered gives no URL", func(t *testing.T) {
		f := newOIDCFakeIssuer(t)
		f.set(func(f *oidcFakeIssuer) { f.discoveryCode = http.StatusInternalServerError })
		if got := f.provider(t, nil).AuthURL("s", testNonce, "https://books.example/cb"); got != "" {
			t.Errorf("AuthURL = %q, want empty", got)
		}
	})

	t.Run("an endpoint that already has a query is appended to", func(t *testing.T) {
		f := newOIDCFakeIssuer(t)
		f.set(func(f *oidcFakeIssuer) {
			f.discovery = map[string]string{"authorization_endpoint": f.server.URL + "/authorize?tenant=books"}
		})
		got := f.provider(t, func(c *OIDCConfig) { c.Scopes = []string{"openid"} }).AuthURL("s", testNonce, "https://books.example/cb")
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("AuthURL %q: %v", got, err)
		}
		q := u.Query()
		if q.Get("tenant") != "books" || q.Get("state") != "s" || q.Get("scope") != "openid" {
			t.Errorf("AuthURL = %q, want the tenant kept and our parameters added", got)
		}
	})
}

// Exchange sends what the token endpoint needs, including the client secret
// when there is one and never an empty one.
func TestOIDCExchangeSendsTheCodeAndSecret(t *testing.T) {
	for _, secret := range []string{"s3cret", ""} {
		f := newOIDCFakeIssuer(t)
		f.set(func(f *oidcFakeIssuer) { f.idToken = f.token(t, "k1", nil) })
		p := f.provider(t, func(c *OIDCConfig) { c.ClientSecret = secret })

		if _, err := p.Exchange(t.Context(), "the-code", testNonce, "https://books.example/cb"); err != nil {
			t.Fatalf("Exchange: %v", err)
		}
		f.mu.Lock()
		form := f.tokenForm
		f.mu.Unlock()
		if form.Get("grant_type") != "authorization_code" || form.Get("code") != "the-code" ||
			form.Get("redirect_uri") != "https://books.example/cb" || form.Get("client_id") != "lexarii-test" {
			t.Errorf("token form = %v", form)
		}
		if _, sent := form["client_secret"]; sent != (secret != "") || form.Get("client_secret") != secret {
			t.Errorf("client_secret sent = %v (%q) with secret %q", sent, form.Get("client_secret"), secret)
		}
	}
}

func TestOIDCIdentityClaims(t *testing.T) {
	tests := []struct {
		name   string
		claims map[string]any
		want   Identity
	}{
		{
			// An issuer need not release an email. The account is keyed on
			// sub, so its absence is not a reason to refuse.
			name:   "no email, a preferred username for a name",
			claims: map[string]any{"preferred_username": "areader", "aud": []any{"other-app", "lexarii-test"}},
			want:   Identity{Subject: "subject-1", DisplayName: "areader"},
		},
		{
			name:   "a name beats the preferred username; non-string groups are dropped",
			claims: map[string]any{"name": "A Reader", "preferred_username": "areader", "email": "a@x", "groups": []any{"staff", 7, "readers"}},
			want:   Identity{Subject: "subject-1", DisplayName: "A Reader", Email: "a@x", Groups: []string{"staff", "readers"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newOIDCFakeIssuer(t)
			f.set(func(f *oidcFakeIssuer) { f.idToken = f.token(t, "k1", tc.claims) })

			got, err := f.provider(t, nil).Exchange(t.Context(), "code", testNonce, "https://books.example/cb")
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if got.Subject != tc.want.Subject || got.Email != tc.want.Email || got.DisplayName != tc.want.DisplayName ||
				strings.Join(got.Groups, ",") != strings.Join(tc.want.Groups, ",") {
				t.Errorf("identity = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Every way the conversation with the issuer can go wrong ends in an error,
// never an identity.
func TestOIDCExchangeFailures(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(t *testing.T, f *oidcFakeIssuer)
		edit   func(t *testing.T, c *OIDCConfig)
	}{
		// Discovery.
		{name: "issuer unreachable", edit: func(t *testing.T, c *OIDCConfig) { c.Issuer = closedURL(t) }},
		{name: "issuer URL unusable", edit: func(_ *testing.T, c *OIDCConfig) { c.Issuer = "http://[::1" }},
		{name: "discovery fails", break_: func(_ *testing.T, f *oidcFakeIssuer) { f.discoveryCode = http.StatusNotFound }},
		{name: "discovery is not JSON", break_: func(_ *testing.T, f *oidcFakeIssuer) { f.discoveryRaw = "<html>" }},
		{name: "discovery has no token endpoint", break_: func(_ *testing.T, f *oidcFakeIssuer) {
			f.discovery = map[string]string{"token_endpoint": ""}
		}},
		// The token endpoint.
		{name: "token endpoint URL unusable", break_: func(_ *testing.T, f *oidcFakeIssuer) {
			f.discovery = map[string]string{"token_endpoint": "http://[::1"}
		}},
		{name: "token endpoint unreachable", break_: func(t *testing.T, f *oidcFakeIssuer) {
			f.discovery = map[string]string{"token_endpoint": closedURL(t) + "/token"}
		}},
		{name: "the code is refused", break_: func(_ *testing.T, f *oidcFakeIssuer) {
			f.tokenCode = http.StatusBadRequest
			f.tokenRaw = `{"error":"invalid_grant"}`
		}},
		{name: "token response is not JSON", break_: func(_ *testing.T, f *oidcFakeIssuer) { f.tokenRaw = "<html>" }},
		{name: "token response has no id_token", break_: func(_ *testing.T, f *oidcFakeIssuer) { f.tokenRaw = `{"access_token":"x"}` }},
		// The token itself.
		{name: "not a JWT", break_: func(_ *testing.T, f *oidcFakeIssuer) { f.idToken = "only.two" }},
		{name: "header is not base64", break_: func(_ *testing.T, f *oidcFakeIssuer) { f.idToken = "!!!.e30.sig" }},
		{name: "header is not JSON", break_: func(t *testing.T, f *oidcFakeIssuer) {
			f.idToken = base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".e30.sig"
		}},
		{name: "signature is not base64", break_: func(t *testing.T, f *oidcFakeIssuer) {
			parts := strings.Split(f.token(t, "k1", nil), ".")
			f.idToken = parts[0] + "." + parts[1] + ".!!!"
		}},
		{name: "a validly signed payload that is not base64", break_: func(t *testing.T, f *oidcFakeIssuer) {
			f.idToken = f.sign(t, oidcSegment(t, map[string]string{"alg": "RS256", "kid": "k1"}), "!!!")
		}},
		{name: "a validly signed payload that is not JSON", break_: func(t *testing.T, f *oidcFakeIssuer) {
			f.idToken = f.sign(t, oidcSegment(t, map[string]string{"alg": "RS256", "kid": "k1"}),
				base64.RawURLEncoding.EncodeToString([]byte("not json")))
		}},
		// The key set.
		{name: "key set URL unusable", break_: func(_ *testing.T, f *oidcFakeIssuer) {
			f.discovery = map[string]string{"jwks_uri": "http://[::1"}
		}},
		{name: "key set unreachable", break_: func(t *testing.T, f *oidcFakeIssuer) {
			f.discovery = map[string]string{"jwks_uri": closedURL(t) + "/jwks"}
		}},
		{name: "key set fails", break_: func(_ *testing.T, f *oidcFakeIssuer) { f.jwksCode = http.StatusInternalServerError }},
		{name: "key set is not JSON", break_: func(_ *testing.T, f *oidcFakeIssuer) { f.jwksRaw = "<html>" }},
		{name: "key set holds nothing usable", break_: func(t *testing.T, f *oidcFakeIssuer) {
			good := oidcFakeJWK("k1", &f.key.PublicKey)
			bad := func(edit func(map[string]string)) map[string]string {
				k := map[string]string{}
				for name, v := range good {
					k[name] = v
				}
				edit(k)
				return k
			}
			raw, err := json.Marshal(map[string]any{"keys": []map[string]string{
				bad(func(k map[string]string) { k["kty"] = "EC" }),
				bad(func(k map[string]string) { k["n"] = "!!!" }),
				bad(func(k map[string]string) { k["e"] = "!!!" }),
				bad(func(k map[string]string) { k["e"] = "AA" }), // an exponent of zero
			}})
			if err != nil {
				t.Fatal(err)
			}
			f.jwksRaw = string(raw)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newOIDCFakeIssuer(t)
			f.set(func(f *oidcFakeIssuer) { f.idToken = f.token(t, "k1", nil) })
			if tc.break_ != nil {
				f.set(func(f *oidcFakeIssuer) { tc.break_(t, f) })
			}
			p := f.provider(t, func(c *OIDCConfig) {
				if tc.edit != nil {
					tc.edit(t, c)
				}
			})

			identity, err := p.Exchange(t.Context(), "code", testNonce, "https://books.example/cb")
			if err == nil {
				t.Fatalf("Exchange succeeded with %+v, want an error", identity)
			}
			if identity.Subject != "" {
				t.Errorf("a failed exchange returned an identity: %+v", identity)
			}
		})
	}
}

// Keys are cached, an unknown key id triggers at most one refetch a minute,
// and a key id the issuer never publishes is refused after it.
func TestOIDCKeyRefetchIsRateLimited(t *testing.T) {
	f := newOIDCFakeIssuer(t)
	f.set(func(f *oidcFakeIssuer) { f.idToken = f.token(t, "k1", nil) })
	p := f.provider(t, nil)
	fetches := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.jwksFetchs
	}

	for range 2 {
		if _, err := p.Exchange(t.Context(), "code", testNonce, "https://books.example/cb"); err != nil {
			t.Fatalf("Exchange: %v", err)
		}
	}
	if n := fetches(); n != 1 {
		t.Fatalf("key set fetched %d times for two sign-ins with one key, want 1", n)
	}

	// A key id nobody published, moments after a fetch: refused without
	// asking the issuer again, so a stream of forged kids cannot be used to
	// hammer it.
	f.set(func(f *oidcFakeIssuer) { f.idToken = f.token(t, "never-published", nil) })
	if _, err := p.Exchange(t.Context(), "code", testNonce, "https://books.example/cb"); err == nil {
		t.Fatal("a token signed under an unknown key id was accepted")
	}
	if n := fetches(); n != 1 {
		t.Errorf("key set fetched %d times, want no refetch within the minute", n)
	}

	// Once the key set is stale it is fetched again, and a kid still not in
	// it is still refused.
	p.mu.Lock()
	p.keysAt = time.Now().Add(-2 * time.Minute)
	p.mu.Unlock()
	if _, err := p.Exchange(t.Context(), "code", testNonce, "https://books.example/cb"); err == nil {
		t.Fatal("a token signed under an unknown key id was accepted after a refetch")
	}
	if n := fetches(); n != 2 {
		t.Errorf("key set fetched %d times, want one refetch", n)
	}
}
