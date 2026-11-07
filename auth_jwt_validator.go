package mcp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// JWTValidator is a TokenValidator for JWT access tokens issued by an
// identity provider (RFC 9068 style): the signature is checked against the
// issuer's JSON Web Key Set, discovered through OpenID Connect discovery
// unless JWKSURL is given, and the standard claims are enforced. It needs
// nothing beyond the standard library.
//
//	v, err := mcp.NewJWTValidator(mcp.JWTValidatorConfig{Issuer: "https://idp.example/realms/acme"})
//	rs := &mcp.ResourceServer{Resource: "https://mcp.example.com/mcp", AuthorizationServers: []string{"https://idp.example/realms/acme"}, ValidateToken: v.Validate}
//
// Audience and scope are checked by ResourceServer from the TokenInfo this
// returns; the validator checks issuer, signature, expiry and not-before.
type JWTValidator struct {
	cfg    JWTValidatorConfig
	client *http.Client

	mu          sync.Mutex          // guards the fields below; never held across a fetch
	keys        map[string][]jwkKey // by kid ("" for keys without one)
	fetchedAt   time.Time
	jwksURL     string
	lastRefresh time.Time // last unknown-kid refresh (rate limited)
	lastFailure time.Time // last failed fetch (backoff)
	lastErr     error

	fetchMu sync.Mutex // serialises fetches: one in flight, the rest wait for its result
}

// jwkKey is one verification key with the constraints its JWK declared.
type jwkKey struct {
	key crypto.PublicKey
	alg string // the JWK's alg, when stated: the only algorithm it may verify
}

// JWTValidatorConfig configures NewJWTValidator.
type JWTValidatorConfig struct {
	// Issuer is the authorization server's issuer identifier; the token's
	// iss claim must equal it exactly. Required.
	Issuer string

	// JWKSURL is where the issuer publishes its keys. Empty means discover
	// it (jwks_uri) from {Issuer}/.well-known/openid-configuration, then
	// the RFC 8414 well-known location, at first use.
	JWKSURL string

	// HTTPClient fetches discovery documents and keys; nil uses
	// http.DefaultClient.
	HTTPClient *http.Client

	// ClockSkew is tolerated when checking exp and nbf. Default 60s.
	ClockSkew time.Duration

	// KeyCacheTTL is how long fetched keys are reused before being
	// refreshed. Default one hour. An unknown kid also triggers a refresh,
	// at most once a minute.
	KeyCacheTTL time.Duration

	// RequireAccessTokenType requires the JWT header's typ to be "at+jwt",
	// as the RFC 9068 access-token profile mandates, which stops an ID
	// token being presented as an access token. It is a deliberate opt-in:
	// several widely used identity providers (Microsoft Entra among them)
	// issue access tokens typed "JWT", and requiring at+jwt by default would
	// lock their users out. Turn it on when your provider marks its access
	// tokens. Without it, the audience check is what keeps an ID token out:
	// never list a client_id in ResourceServer.Audiences unless the provider
	// marks its access tokens.
	RequireAccessTokenType bool

	// AllowConfirmation accepts sender-constrained tokens (a cnf claim).
	// Off by default: this library does no proof-of-possession, so such a
	// token would be accepted without the proof it demands.
	AllowConfirmation bool

	// Algorithms lists the accepted JWS algorithms. Default: RS256, RS384,
	// RS512, PS256, PS384, PS512, ES256, ES384, ES512 and EdDSA. "none" is
	// never accepted.
	Algorithms []string
}

var defaultJWTAlgorithms = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}

// NewJWTValidator builds a validator for cfg. Keys are fetched lazily, on
// the first token.
func NewJWTValidator(cfg JWTValidatorConfig) (*JWTValidator, error) {
	if cfg.Issuer == "" {
		return nil, errors.New("mcp: JWTValidatorConfig.Issuer is required")
	}
	if cfg.ClockSkew == 0 {
		cfg.ClockSkew = time.Minute
	}
	if cfg.KeyCacheTTL == 0 {
		cfg.KeyCacheTTL = time.Hour
	}
	if len(cfg.Algorithms) == 0 {
		cfg.Algorithms = defaultJWTAlgorithms
	}
	// Keys decide who is authenticated, so they travel only over https (or
	// http to loopback, for local development), as the OAuth client's own
	// discovery requires.
	if u, err := url.Parse(cfg.Issuer); err != nil || checkFetchURL(u) != nil {
		return nil, fmt.Errorf("mcp: JWTValidatorConfig.Issuer %q must be an https URL", cfg.Issuer)
	}
	if cfg.JWKSURL != "" {
		if u, err := url.Parse(cfg.JWKSURL); err != nil || checkFetchURL(u) != nil {
			return nil, fmt.Errorf("mcp: JWTValidatorConfig.JWKSURL %q must be an https URL", cfg.JWKSURL)
		}
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	client = clientCheckingRedirects(client)
	return &JWTValidator{cfg: cfg, client: client, jwksURL: cfg.JWKSURL}, nil
}

// Validate is the TokenValidator: it verifies token and reports its claims.
func (v *JWTValidator) Validate(r *http.Request, token string) (*TokenInfo, error) {
	ctx := context.Background()
	if r != nil {
		ctx = r.Context()
	}
	return v.ValidateToken(ctx, token)
}

// jwtClaimSet is the subset of claims the validator reads.
type jwtClaimSet struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Audience  json.RawMessage `json:"aud"`
	Expiry    *float64        `json:"exp"`
	NotBefore *float64        `json:"nbf"`
	Scope     json.RawMessage `json:"scope"`
	Scp       json.RawMessage `json:"scp"`
	Cnf       json.RawMessage `json:"cnf"`
}

// ValidateToken verifies token outside an HTTP request.
func (v *JWTValidator) ValidateToken(ctx context.Context, token string) (*TokenInfo, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWS compact serialization")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	var header struct {
		Alg  string          `json:"alg"`
		Kid  string          `json:"kid"`
		Typ  string          `json:"typ"`
		Crit json.RawMessage `json:"crit"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	if len(header.Crit) > 0 && string(header.Crit) != "null" {
		// RFC 7515 section 4.1.11: a token with critical extensions this
		// validator does not understand must be rejected; it understands none.
		return nil, errors.New("crit header extensions not supported")
	}
	if !v.algorithmAllowed(header.Alg) {
		return nil, fmt.Errorf("algorithm %q not accepted", header.Alg)
	}
	if v.cfg.RequireAccessTokenType && !strings.EqualFold(header.Typ, "at+jwt") && !strings.EqualFold(header.Typ, "application/at+jwt") {
		return nil, fmt.Errorf("typ %q is not an access token", header.Typ)
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("signature: %w", err)
	}
	if !plausibleSignatureLength(header.Alg, signature) {
		// Rejected before any key lookup, so junk tokens cannot drive key
		// refreshes.
		return nil, errors.New("signature length does not match the algorithm")
	}
	signingInput := []byte(parts[0] + "." + parts[1])

	verifyWith := func(keys []jwkKey) bool {
		for _, k := range keys {
			if k.alg != "" && k.alg != header.Alg {
				continue // the JWK is for another algorithm (RFC 7517 section 4.4)
			}
			if verifyJWS(header.Alg, k.key, signingInput, signature) == nil {
				return true
			}
		}
		return false
	}
	keys, err := v.keysFor(ctx, header.Kid, false)
	if err != nil {
		return nil, err
	}
	verified := verifyWith(keys)
	if !verified && header.Kid != "" {
		// The key may have rotated since the last fetch.
		if keys, err = v.keysFor(ctx, header.Kid, true); err == nil {
			verified = verifyWith(keys)
		}
	}
	if !verified {
		return nil, errors.New("signature verification failed")
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("payload: %w", err)
	}
	var claims jwtClaimSet
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}
	if claims.Issuer != v.cfg.Issuer {
		return nil, fmt.Errorf("issuer %q is not %q", claims.Issuer, v.cfg.Issuer)
	}
	now := time.Now()
	if claims.Expiry == nil {
		return nil, errors.New("exp claim missing")
	}
	expiry := time.Unix(int64(*claims.Expiry), 0)
	if now.After(expiry.Add(v.cfg.ClockSkew)) {
		return nil, errors.New("token expired")
	}
	if claims.NotBefore != nil && now.Add(v.cfg.ClockSkew).Before(time.Unix(int64(*claims.NotBefore), 0)) {
		return nil, errors.New("token not yet valid")
	}
	if len(claims.Cnf) > 0 && string(claims.Cnf) != "null" && !v.cfg.AllowConfirmation {
		return nil, errors.New("sender-constrained token (cnf) not supported")
	}

	var all map[string]any
	_ = json.Unmarshal(payload, &all)
	return &TokenInfo{
		Subject:  claims.Subject,
		Audience: jsonStringOrList(claims.Audience),
		Scopes:   jwtScopes(claims.Scope, claims.Scp),
		Expiry:   expiry,
		Claims:   all,
	}, nil
}

func (v *JWTValidator) algorithmAllowed(alg string) bool {
	if alg == "" || strings.EqualFold(alg, "none") {
		return false
	}
	for _, a := range v.cfg.Algorithms {
		if a == alg {
			return true
		}
	}
	return false
}

// keysFor returns the candidate keys for kid, fetching or refreshing the
// key set as needed. The fetch never runs under v.mu, so validations that
// already have their key are not held up by a slow identity provider; a
// refresh for an unknown kid is allowed once a minute, and after a failed
// fetch nothing is retried for a minute (the cached keys, or the cached
// error, serve meanwhile).
func (v *JWTValidator) keysFor(ctx context.Context, kid string, refresh bool) ([]jwkKey, error) {
	v.mu.Lock()
	stale := v.keys == nil || time.Since(v.fetchedAt) > v.cfg.KeyCacheTTL
	if refresh && time.Since(v.lastRefresh) < time.Minute {
		refresh = false
	}
	backoff := time.Since(v.lastFailure) < time.Minute
	need := (stale || refresh) && !backoff
	if refresh && need {
		v.lastRefresh = time.Now()
	}
	cachedErr, haveKeys := v.lastErr, v.keys != nil
	seen := v.fetchedAt
	v.mu.Unlock()

	if need {
		v.fetchKeys(ctx, seen)
		v.mu.Lock()
		cachedErr, haveKeys = v.lastErr, v.keys != nil
		v.mu.Unlock()
	}
	if !haveKeys {
		if cachedErr == nil {
			cachedErr = errors.New("no verification keys available")
		}
		return nil, cachedErr
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if kid != "" {
		return v.keys[kid], nil
	}
	return v.keys[""], nil
}

// fetchKeys fetches the key set once for all concurrent callers: the
// first does the work, the rest wait on fetchMu and find the result. seen
// is the fetchedAt the caller observed; a fetch that completed after it
// (another caller's) already serves this one.
func (v *JWTValidator) fetchKeys(ctx context.Context, seen time.Time) {
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	v.mu.Lock()
	already := v.keys != nil && v.fetchedAt.After(seen)
	failedRecently := time.Since(v.lastFailure) < time.Minute
	v.mu.Unlock()
	if already || failedRecently {
		return // another caller just fetched, or just failed
	}
	keys, jwksURL, err := v.loadKeys(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	if err != nil {
		v.lastFailure, v.lastErr = time.Now(), err
		return
	}
	v.keys, v.jwksURL, v.fetchedAt, v.lastErr = keys, jwksURL, time.Now(), nil
}

// loadKeys discovers the JWKS URL if needed and reads the key set.
func (v *JWTValidator) loadKeys(ctx context.Context) (map[string][]jwkKey, string, error) {
	v.mu.Lock()
	jwksURL := v.jwksURL
	v.mu.Unlock()
	if jwksURL == "" {
		discovered, err := v.discoverJWKS(ctx)
		if err != nil {
			return nil, "", err
		}
		jwksURL = discovered
	}
	if u, err := url.Parse(jwksURL); err != nil || checkFetchURL(u) != nil {
		return nil, "", fmt.Errorf("jwks_uri %q is not an https URL", jwksURL)
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := v.getJSON(ctx, jwksURL, &set); err != nil {
		return nil, "", fmt.Errorf("fetching keys: %w", err)
	}
	keys := map[string][]jwkKey{}
	for _, raw := range set.Keys {
		kid, k, err := parseJWK(raw)
		if err != nil || k.key == nil {
			continue // a key type, use, or size this validator does not accept
		}
		keys[kid] = append(keys[kid], k)
	}
	return keys, jwksURL, nil
}

// discoverJWKS finds jwks_uri through OpenID Connect discovery, then the
// RFC 8414 location.
func (v *JWTValidator) discoverJWKS(ctx context.Context) (string, error) {
	issuer := strings.TrimSuffix(v.cfg.Issuer, "/")
	candidates := []string{issuer + "/.well-known/openid-configuration"}
	if u, err := url.Parse(issuer); err == nil && u.Host != "" {
		candidates = append(candidates,
			originURL(u, "/.well-known/openid-configuration"+u.Path),
			originURL(u, "/.well-known/oauth-authorization-server"+u.Path))
	}
	var lastErr error
	for _, candidate := range candidates {
		var doc struct {
			Issuer  string `json:"issuer"`
			JWKSURI string `json:"jwks_uri"`
		}
		if err := v.getJSON(ctx, candidate, &doc); err != nil {
			lastErr = err
			continue
		}
		if doc.JWKSURI == "" {
			lastErr = fmt.Errorf("%s has no jwks_uri", candidate)
			continue
		}
		if doc.Issuer != v.cfg.Issuer {
			// OpenID Connect Discovery section 4.3: the issuer in the document
			// must be identical to the issuer used to look it up.
			lastErr = fmt.Errorf("%s is for another issuer (%q, want %q)", candidate, doc.Issuer, v.cfg.Issuer)
			continue
		}
		return doc.JWKSURI, nil
	}
	return "", fmt.Errorf("discovering keys for %s: %w", v.cfg.Issuer, lastErr)
}

func (v *JWTValidator) getJSON(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", target, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes)).Decode(out)
}

// parseJWK decodes one JSON Web Key into a verification key. Keys marked
// for encryption (use, key_ops, or an encryption alg), keys too small to be
// safe (RSA under 2048 bits, RFC 7518 section 3.3), and types this
// validator does not verify yield no key.
func parseJWK(raw json.RawMessage) (kid string, out jwkKey, err error) {
	var k struct {
		Kty    string   `json:"kty"`
		Kid    string   `json:"kid"`
		Use    string   `json:"use"`
		Alg    string   `json:"alg"`
		KeyOps []string `json:"key_ops"`
		Crv    string   `json:"crv"`
		N      string   `json:"n"`
		E      string   `json:"e"`
		X      string   `json:"x"`
		Y      string   `json:"y"`
	}
	if err := json.Unmarshal(raw, &k); err != nil {
		return "", jwkKey{}, err
	}
	if k.Use == "enc" || isEncryptionAlg(k.Alg) {
		return k.Kid, jwkKey{}, nil
	}
	if len(k.KeyOps) > 0 {
		canVerify := false
		for _, op := range k.KeyOps {
			if op == "verify" {
				canVerify = true
			}
		}
		if !canVerify {
			return k.Kid, jwkKey{}, nil
		}
	}
	out.alg = k.Alg
	key, err := parseJWKPublicKey(k.Kty, k.Crv, k.N, k.E, k.X, k.Y)
	if err != nil {
		return "", jwkKey{}, err
	}
	out.key = key
	return k.Kid, out, nil
}

// isEncryptionAlg reports whether alg names a JWE key-management algorithm
// (RFC 7518 section 4), i.e. a key that must never verify a signature.
func isEncryptionAlg(alg string) bool {
	switch {
	case alg == "":
		return false
	case strings.HasPrefix(alg, "RSA-OAEP"), alg == "RSA1_5", strings.HasPrefix(alg, "ECDH-ES"),
		strings.HasPrefix(alg, "A") && (strings.HasSuffix(alg, "KW") || strings.HasSuffix(alg, "GCMKW")),
		strings.HasPrefix(alg, "PBES2"), alg == "dir":
		return true
	}
	return false
}

func parseJWKPublicKey(kty, crv, nStr, eStr, xStr, yStr string) (crypto.PublicKey, error) {
	k := struct{ Kty, Crv, N, E, X, Y string }{kty, crv, nStr, eStr, xStr, yStr}
	b64 := func(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }
	switch k.Kty {
	case "RSA":
		n, err := b64(k.N)
		if err != nil {
			return nil, err
		}
		e, err := b64(k.E)
		if err != nil {
			return nil, err
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			return nil, errors.New("RSA key shorter than 2048 bits")
		}
		return pub, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, nil
		}
		x, err := b64(k.X)
		if err != nil {
			return nil, err
		}
		y, err := b64(k.Y)
		if err != nil {
			return nil, err
		}
		pub := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !curve.IsOnCurve(pub.X, pub.Y) {
			return nil, errors.New("EC point not on curve")
		}
		return pub, nil
	case "OKP":
		if k.Crv != "Ed25519" {
			return nil, nil
		}
		x, err := b64(k.X)
		if err != nil {
			return nil, err
		}
		if len(x) != ed25519.PublicKeySize {
			return nil, errors.New("bad Ed25519 key size")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, nil
}

// plausibleSignatureLength reports whether sig could be a signature under
// alg: RSA signatures are at least 256 bytes (2048-bit keys), ECDSA ones
// exactly twice the curve size, EdDSA ones 64 bytes.
func plausibleSignatureLength(alg string, sig []byte) bool {
	switch alg {
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512":
		return len(sig) >= 256
	case "ES256":
		return len(sig) == 64
	case "ES384":
		return len(sig) == 96
	case "ES512":
		return len(sig) == 132
	case "EdDSA":
		return len(sig) == ed25519.SignatureSize
	}
	return false
}

// verifyJWS checks signature over signingInput with key under alg.
func verifyJWS(alg string, key crypto.PublicKey, signingInput, signature []byte) error {
	hashFor := func() (crypto.Hash, []byte) {
		switch alg[len(alg)-3:] {
		case "384":
			sum := sha512.Sum384(signingInput)
			return crypto.SHA384, sum[:]
		case "512":
			sum := sha512.Sum512(signingInput)
			return crypto.SHA512, sum[:]
		default:
			sum := sha256.Sum256(signingInput)
			return crypto.SHA256, sum[:]
		}
	}
	switch {
	case strings.HasPrefix(alg, "RS"):
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("key type mismatch")
		}
		h, sum := hashFor()
		return rsa.VerifyPKCS1v15(pub, h, sum, signature)
	case strings.HasPrefix(alg, "PS"):
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return errors.New("key type mismatch")
		}
		h, sum := hashFor()
		return rsa.VerifyPSS(pub, h, sum, signature, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	case strings.HasPrefix(alg, "ES"):
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("key type mismatch")
		}
		wantCurve := map[string]elliptic.Curve{"ES256": elliptic.P256(), "ES384": elliptic.P384(), "ES512": elliptic.P521()}[alg]
		if wantCurve == nil || pub.Curve != wantCurve {
			return errors.New("key curve does not match algorithm")
		}
		_, sum := hashFor()
		size := (pub.Curve.Params().BitSize + 7) / 8
		if len(signature) != 2*size {
			return errors.New("bad ECDSA signature length")
		}
		r := new(big.Int).SetBytes(signature[:size])
		s := new(big.Int).SetBytes(signature[size:])
		if !ecdsa.Verify(pub, sum, r, s) {
			return errors.New("ECDSA verification failed")
		}
		return nil
	case alg == "EdDSA":
		pub, ok := key.(ed25519.PublicKey)
		if !ok {
			return errors.New("key type mismatch")
		}
		if !ed25519.Verify(pub, signingInput, signature) {
			return errors.New("EdDSA verification failed")
		}
		return nil
	}
	return fmt.Errorf("algorithm %q not supported", alg)
}

// jsonStringOrList decodes a claim that may be a string or an array of
// strings (aud, scp).
func jsonStringOrList(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}

// jwtScopes reads the granted scopes: the space-separated scope claim
// (RFC 9068 / RFC 8693) or the scp array some providers use instead.
func jwtScopes(scope, scp json.RawMessage) []string {
	split := func(vals []string) []string {
		if len(vals) == 1 {
			return strings.Fields(vals[0])
		}
		return vals
	}
	if vals := jsonStringOrList(scope); len(vals) > 0 {
		return split(vals)
	}
	return split(jsonStringOrList(scp))
}
