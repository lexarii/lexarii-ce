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
	"net/url"
	"strings"
	"sync"
	"time"
)

// OIDCConfig configures an OpenID Connect provider.
type OIDCConfig struct {
	// ProviderName is the stable identifier stored against identities. Changing
	// it orphans every account that signed in through it.
	ProviderName string
	// Label is what appears on the sign-in screen.
	Label string

	// Issuer is the base URL. Discovery appends /.well-known/openid-configuration.
	Issuer       string
	ClientID     string
	ClientSecret string

	// Scopes defaults to openid, profile, email.
	Scopes []string

	// AllowSignup permits creating an account on first sign-in.
	//
	// Off by default and deliberately so: against a public issuer such as Google,
	// turning this on lets anyone with an account there into the library.
	AllowSignup bool

	// HTTPClient is used for discovery and token exchange.
	HTTPClient *http.Client
}

// OIDCProvider authenticates through an OpenID Connect issuer.
//
// The first federated provider, and the one that proves the abstraction holds:
// it arrives through a callback rather than a form, which is exactly the shape
// a password-only design cannot accommodate.
type OIDCProvider struct {
	cfg OIDCConfig

	mu       sync.RWMutex
	metadata *oidcMetadata
	keys     map[string]*rsa.PublicKey
	keysAt   time.Time
}

type oidcMetadata struct {
	Issuer        string `json:"issuer"`
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
	JWKSURI       string `json:"jwks_uri"`
}

// NewOIDCProvider builds a provider. Discovery happens lazily on first use, so
// a misconfigured or unreachable issuer does not stop Lexarii from starting
// and locking out local accounts too.
func NewOIDCProvider(cfg OIDCConfig) (*OIDCProvider, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" {
		return nil, errors.New("oidc: issuer and client id are required")
	}
	if cfg.ProviderName == "" {
		cfg.ProviderName = "oidc"
	}
	if cfg.Label == "" {
		cfg.Label = "Single sign-on"
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "profile", "email"}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &OIDCProvider{cfg: cfg, keys: map[string]*rsa.PublicKey{}}, nil
}

func (p *OIDCProvider) Name() string        { return p.cfg.ProviderName }
func (p *OIDCProvider) DisplayName() string { return p.cfg.Label }
func (p *OIDCProvider) Kind() Kind          { return KindRedirect }

// AllowSignup reports whether first sign-in may create an account.
func (p *OIDCProvider) AllowSignup() bool { return p.cfg.AllowSignup }

// ErrNonceMismatch is an ID token that was not issued for this sign-in: its
// nonce is missing or is not the one this browser was sent off with.
var ErrNonceMismatch = errors.New("oidc: id token nonce does not match this sign-in")

// AuthURL is where the browser is sent to sign in.
//
// The caller must generate state and nonce, store both against the browser,
// verify the state on return and hand the nonce to Exchange; without the
// state the callback is forgeable and an attacker can log a victim into the
// attacker's account, and without the nonce an ID token issued for one
// sign-in can be replayed into another.
func (p *OIDCProvider) AuthURL(state, nonce, redirectURI string) string {
	meta, err := p.discover(context.Background())
	if err != nil {
		return ""
	}

	q := url.Values{}
	q.Set("client_id", p.cfg.ClientID)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(p.cfg.Scopes, " "))
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	// A separate value from the state, which travels back in the callback's
	// URL - and so into proxy logs and browser history - where the nonce never
	// does. Exchange checks the ID token carries exactly this.
	q.Set("nonce", nonce)

	sep := "?"
	if strings.Contains(meta.AuthEndpoint, "?") {
		sep = "&"
	}
	return meta.AuthEndpoint + sep + q.Encode()
}

// Exchange turns a callback code into a verified identity.
//
// Nonce is the value AuthURL was given for this sign-in. The ID token must
// carry it: one that does not was issued for some other sign-in - a token
// captured from another browser's flow, replayed here - and is refused with
// ErrNonceMismatch, as is one that carries no nonce at all.
func (p *OIDCProvider) Exchange(ctx context.Context, code, nonce, redirectURI string) (Identity, error) {
	if nonce == "" {
		// Never compare against an empty expectation: a token without a
		// nonce would match it.
		return Identity{}, fmt.Errorf("%w: no nonce was kept for this sign-in", ErrNonceMismatch)
	}
	meta, err := p.discover(ctx)
	if err != nil {
		return Identity{}, err
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", p.cfg.ClientID)
	if p.cfg.ClientSecret != "" {
		form.Set("client_secret", p.cfg.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return Identity{}, fmt.Errorf("oidc: building token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("oidc: token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Identity{}, fmt.Errorf("oidc: token endpoint returned %s", resp.Status)
	}

	var token struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&token); err != nil {
		return Identity{}, fmt.Errorf("oidc: decoding token response: %w", err)
	}
	if token.IDToken == "" {
		return Identity{}, errors.New("oidc: no id_token in the token response")
	}

	claims, err := p.verifyIDToken(ctx, token.IDToken, nonce)
	if err != nil {
		return Identity{}, err
	}

	subject, _ := claims["sub"].(string)
	if subject == "" {
		return Identity{}, errors.New("oidc: id token has no subject")
	}

	identity := Identity{
		Provider: p.cfg.ProviderName,
		// The `sub` claim, never the email. Email addresses are reassigned
		// between employees; keying on one eventually hands a new joiner the
		// previous holder's library.
		Subject: subject,
		Data:    claims,
	}
	if email, ok := claims["email"].(string); ok {
		identity.Email = email
	}
	if name, ok := claims["name"].(string); ok {
		identity.DisplayName = name
	} else if preferred, ok := claims["preferred_username"].(string); ok {
		identity.DisplayName = preferred
	}
	if groups, ok := claims["groups"].([]any); ok {
		for _, g := range groups {
			if s, ok := g.(string); ok {
				identity.Groups = append(identity.Groups, s)
			}
		}
	}

	return identity, nil
}

// verifyIDToken checks the signature and the claims that matter.
//
// The signature alone is not enough. An unverified issuer, audience, expiry or
// nonce each allows a token minted elsewhere - long expired, or for another
// sign-in - to be replayed as a valid login, and a library that checks only
// the signature is the classic way this goes wrong.
func (p *OIDCProvider) verifyIDToken(ctx context.Context, raw, nonce string) (map[string]any, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("oidc: id token is not a JWT")
	}

	header, err := decodeSegment(parts[0])
	if err != nil {
		return nil, fmt.Errorf("oidc: id token header: %w", err)
	}
	var head struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(header, &head); err != nil {
		return nil, fmt.Errorf("oidc: id token header: %w", err)
	}

	// Only RSA signatures are accepted.
	//
	// "none" must never be honoured, and an HMAC algorithm must not be accepted
	// where an RSA key is expected: both are long-standing JWT confusion attacks
	// that turn a public key into a signing secret.
	if head.Alg != "RS256" {
		return nil, fmt.Errorf("oidc: unsupported id token algorithm %q", head.Alg)
	}

	key, err := p.publicKey(ctx, head.Kid)
	if err != nil {
		return nil, err
	}

	signature, err := decodeSegment(parts[2])
	if err != nil {
		return nil, fmt.Errorf("oidc: id token signature: %w", err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
		return nil, errors.New("oidc: id token signature is not valid")
	}

	payload, err := decodeSegment(parts[1])
	if err != nil {
		return nil, fmt.Errorf("oidc: id token payload: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("oidc: id token payload: %w", err)
	}

	meta, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	if issuer, _ := claims["iss"].(string); issuer != meta.Issuer {
		return nil, fmt.Errorf("oidc: id token issued by %q, expected %q", issuer, meta.Issuer)
	}
	if !audienceMatches(claims["aud"], p.cfg.ClientID) {
		return nil, errors.New("oidc: id token is for a different client")
	}
	if exp, ok := claims["exp"].(float64); ok {
		if time.Now().After(time.Unix(int64(exp), 0)) {
			return nil, errors.New("oidc: id token has expired")
		}
	} else {
		return nil, errors.New("oidc: id token has no expiry")
	}
	got, _ := claims["nonce"].(string)
	if nonce == "" || got == "" || !ConstantTimeEqualString(got, nonce) {
		return nil, ErrNonceMismatch
	}

	return claims, nil
}

// audienceMatches handles `aud` being a string or an array, as the spec allows.
func audienceMatches(aud any, clientID string) bool {
	switch v := aud.(type) {
	case string:
		return v == clientID
	case []any:
		for _, entry := range v {
			if s, ok := entry.(string); ok && s == clientID {
				return true
			}
		}
	}
	return false
}

// discover fetches and caches the issuer's metadata.
func (p *OIDCProvider) discover(ctx context.Context) (*oidcMetadata, error) {
	p.mu.RLock()
	cached := p.metadata
	p.mu.RUnlock()
	if cached != nil {
		return cached, nil
	}

	endpoint := strings.TrimRight(p.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery request: %w", err)
	}

	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidc: discovery: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oidc: discovery returned %s", resp.Status)
	}

	var meta oidcMetadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("oidc: decoding discovery: %w", err)
	}
	if meta.TokenEndpoint == "" || meta.AuthEndpoint == "" || meta.JWKSURI == "" {
		return nil, errors.New("oidc: discovery document is incomplete")
	}

	p.mu.Lock()
	p.metadata = &meta
	p.mu.Unlock()
	return &meta, nil
}

// publicKey returns the signing key for a key id, refreshing the key set when
// an unknown one appears.
//
// Issuers rotate keys without warning, so an unknown kid is a routine event
// rather than an attack; refusing without refetching would break every sign-in
// at the moment of rotation.
func (p *OIDCProvider) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	p.mu.RLock()
	key, ok := p.keys[kid]
	fresh := time.Since(p.keysAt) < time.Minute
	p.mu.RUnlock()
	if ok {
		return key, nil
	}
	if fresh {
		// Already refreshed moments ago: a genuinely unknown key.
		return nil, fmt.Errorf("oidc: unknown signing key %q", kid)
	}

	if err := p.refreshKeys(ctx); err != nil {
		return nil, err
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	if key, ok := p.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("oidc: unknown signing key %q", kid)
}

func (p *OIDCProvider) refreshKeys(ctx context.Context) error {
	meta, err := p.discover(ctx)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.JWKSURI, nil)
	if err != nil {
		return fmt.Errorf("oidc: jwks request: %w", err)
	}
	resp, err := p.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("oidc: fetching jwks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oidc: jwks returned %s", resp.Status)
	}

	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("oidc: decoding jwks: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(jwks.Keys))
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nBytes, err := decodeSegment(k.N)
		if err != nil {
			continue
		}
		eBytes, err := decodeSegment(k.E)
		if err != nil {
			continue
		}
		e := 0
		for _, b := range eBytes {
			e = e<<8 | int(b)
		}
		if e == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}
	}

	p.mu.Lock()
	p.keys = keys
	p.keysAt = time.Now()
	p.mu.Unlock()
	return nil
}

// decodeSegment decodes base64url without padding, as JWT and JWK use.
func decodeSegment(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// NewState generates an unguessable value for the state or the nonce
// parameter. Call it once for each: they must not be the same value.
func NewState() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
