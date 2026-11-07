package mcp

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// jwtIssuer is a fake identity provider: OIDC discovery, a JWKS, and token
// minting with RSA (RS256), EC (ES256) and Ed25519 (EdDSA) keys.
type jwtIssuer struct {
	ts     *httptest.Server
	rsaKey *rsa.PrivateKey
	ecKey  *ecdsa.PrivateKey
	edPriv ed25519.PrivateKey
	edPub  ed25519.PublicKey
	jwks   func() map[string]any
	hits   map[string]int
}

func newJWTIssuer(t *testing.T) *jwtIssuer {
	t.Helper()
	iss := &jwtIssuer{hits: map[string]int{}}
	var err error
	if iss.rsaKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	if iss.ecKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		t.Fatal(err)
	}
	if iss.edPub, iss.edPriv, err = ed25519.GenerateKey(rand.Reader); err != nil {
		t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	iss.jwks = func() map[string]any {
		return map[string]any{"keys": []map[string]any{
			{"kty": "RSA", "kid": "rsa-1", "use": "sig", "n": b64(iss.rsaKey.N.Bytes()), "e": b64(big.NewInt(int64(iss.rsaKey.E)).Bytes())},
			{"kty": "EC", "kid": "ec-1", "crv": "P-256", "x": b64(iss.ecKey.X.Bytes()), "y": b64(iss.ecKey.Y.Bytes())},
			{"kty": "OKP", "kid": "ed-1", "crv": "Ed25519", "x": b64(iss.edPub)},
			{"kty": "RSA", "kid": "enc-1", "use": "enc", "n": b64(iss.rsaKey.N.Bytes()), "e": "AQAB"},
		}}
	}
	iss.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iss.hits[r.URL.Path]++
		switch r.URL.Path {
		case "/realms/acme/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": iss.ts.URL + "/realms/acme", "jwks_uri": iss.ts.URL + "/realms/acme/keys"})
		case "/realms/acme/keys":
			_ = json.NewEncoder(w).Encode(iss.jwks())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(iss.ts.Close)
	return iss
}

func (iss *jwtIssuer) issuer() string { return iss.ts.URL + "/realms/acme" }

// mint signs claims with the named key; header may add/override fields.
func (iss *jwtIssuer) mint(t *testing.T, kid string, claims map[string]any, header map[string]any) string {
	t.Helper()
	alg := map[string]string{"rsa-1": "RS256", "ec-1": "ES256", "ed-1": "EdDSA"}[kid]
	h := map[string]any{"alg": alg, "kid": kid, "typ": "at+jwt"}
	for k, v := range header {
		h[k] = v
	}
	hj, _ := json.Marshal(h)
	cj, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString(hj) + "." + base64.RawURLEncoding.EncodeToString(cj)
	sum := sha256.Sum256([]byte(input))
	var sig []byte
	var err error
	switch kid {
	case "rsa-1":
		sig, err = rsa.SignPKCS1v15(rand.Reader, iss.rsaKey, crypto.SHA256, sum[:])
	case "ec-1":
		var r, s *big.Int
		r, s, err = ecdsa.Sign(rand.Reader, iss.ecKey, sum[:])
		if err == nil {
			sig = make([]byte, 64)
			r.FillBytes(sig[:32])
			s.FillBytes(sig[32:])
		}
	case "ed-1":
		sig = ed25519.Sign(iss.edPriv, []byte(input))
	}
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func baseClaims(iss *jwtIssuer) map[string]any {
	return map[string]any{
		"iss":   iss.issuer(),
		"sub":   "user-1",
		"aud":   "https://mcp.example.com/mcp",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"scope": "mcp:read mcp:write",
	}
}

func TestJWTValidator_AcceptsEachAlgorithmAndReadsClaims(t *testing.T) {
	iss := newJWTIssuer(t)
	v, err := NewJWTValidator(JWTValidatorConfig{Issuer: iss.issuer(), HTTPClient: iss.ts.Client()})
	if err != nil {
		t.Fatal(err)
	}
	for _, kid := range []string{"rsa-1", "ec-1", "ed-1"} {
		info, err := v.ValidateToken(context.Background(), iss.mint(t, kid, baseClaims(iss), nil))
		if err != nil {
			t.Fatalf("%s: %v", kid, err)
		}
		if info.Subject != "user-1" || len(info.Audience) != 1 || info.Audience[0] != "https://mcp.example.com/mcp" {
			t.Fatalf("%s: info = %+v", kid, info)
		}
		if !info.HasScope("mcp:read") || !info.HasScope("mcp:write") || info.HasScope("admin") {
			t.Fatalf("%s: scopes = %v", kid, info.Scopes)
		}
		if info.Expiry.IsZero() || info.Claims["iss"] != iss.issuer() {
			t.Fatalf("%s: expiry/claims = %+v", kid, info)
		}
	}
	// Discovery and the key set were fetched once, not per token.
	if iss.hits["/realms/acme/.well-known/openid-configuration"] != 1 || iss.hits["/realms/acme/keys"] != 1 {
		t.Fatalf("hits = %v", iss.hits)
	}
	// aud as an array and scopes as an scp array are read too.
	claims := baseClaims(iss)
	claims["aud"] = []string{"other", "https://mcp.example.com/mcp"}
	delete(claims, "scope")
	claims["scp"] = []string{"a", "b"}
	info, err := v.ValidateToken(context.Background(), iss.mint(t, "rsa-1", claims, nil))
	if err != nil || len(info.Audience) != 2 || len(info.Scopes) != 2 || info.Scopes[1] != "b" {
		t.Fatalf("array claims: %+v %v", info, err)
	}
}

func TestJWTValidator_Rejections(t *testing.T) {
	iss := newJWTIssuer(t)
	v, _ := NewJWTValidator(JWTValidatorConfig{Issuer: iss.issuer(), HTTPClient: iss.ts.Client()})
	other := newJWTIssuer(t) // different keys, same claim shapes
	cases := map[string]string{}
	c := baseClaims(iss)
	c["iss"] = "https://impostor.example"
	cases["wrong issuer"] = iss.mint(t, "rsa-1", c, nil)
	c = baseClaims(iss)
	c["exp"] = time.Now().Add(-10 * time.Minute).Unix()
	cases["expired"] = iss.mint(t, "rsa-1", c, nil)
	c = baseClaims(iss)
	c["nbf"] = time.Now().Add(10 * time.Minute).Unix()
	cases["not yet valid"] = iss.mint(t, "rsa-1", c, nil)
	c = baseClaims(iss)
	delete(c, "exp")
	cases["no exp"] = iss.mint(t, "rsa-1", c, nil)
	c = baseClaims(iss)
	c["cnf"] = map[string]any{"jkt": "x"}
	cases["sender-constrained"] = iss.mint(t, "rsa-1", c, nil)
	cases["signed by another issuer's key"] = other.mint(t, "rsa-1", baseClaims(iss), nil)
	cases["alg none"] = iss.mint(t, "rsa-1", baseClaims(iss), map[string]any{"alg": "none"})
	cases["unknown kid"] = iss.mint(t, "rsa-1", baseClaims(iss), map[string]any{"kid": "rsa-9"})
	cases["encryption key"] = iss.mint(t, "rsa-1", baseClaims(iss), map[string]any{"kid": "enc-1"})
	good := iss.mint(t, "rsa-1", baseClaims(iss), nil)
	cases["tampered payload"] = strings.Replace(good, ".", ".x", 1)
	cases["garbage"] = "not.a.jwt"
	for name, tok := range cases {
		if _, err := v.ValidateToken(context.Background(), tok); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A valid token is still accepted after all that.
	if _, err := v.ValidateToken(context.Background(), good); err != nil {
		t.Fatalf("good token: %v", err)
	}
	// A key missing from the cache triggers one refresh (rate limited).
	if iss.hits["/realms/acme/keys"] < 2 {
		t.Fatalf("unknown kid did not refresh the key set: hits=%v", iss.hits)
	}
}

func TestJWTValidator_Options(t *testing.T) {
	iss := newJWTIssuer(t)
	// typ required
	v, _ := NewJWTValidator(JWTValidatorConfig{Issuer: iss.issuer(), HTTPClient: iss.ts.Client(), RequireAccessTokenType: true})
	if _, err := v.ValidateToken(context.Background(), iss.mint(t, "rsa-1", baseClaims(iss), map[string]any{"typ": "JWT"})); err == nil {
		t.Error("ID-token typ accepted with RequireAccessTokenType")
	}
	if _, err := v.ValidateToken(context.Background(), iss.mint(t, "rsa-1", baseClaims(iss), nil)); err != nil {
		t.Errorf("at+jwt rejected: %v", err)
	}
	// explicit JWKS URL skips discovery; restricted algorithms
	v, _ = NewJWTValidator(JWTValidatorConfig{Issuer: iss.issuer(), JWKSURL: iss.ts.URL + "/realms/acme/keys", HTTPClient: iss.ts.Client(), Algorithms: []string{"ES256"}})
	if _, err := v.ValidateToken(context.Background(), iss.mint(t, "ec-1", baseClaims(iss), nil)); err != nil {
		t.Errorf("ES256: %v", err)
	}
	if _, err := v.ValidateToken(context.Background(), iss.mint(t, "rsa-1", baseClaims(iss), nil)); err == nil {
		t.Error("RS256 accepted although only ES256 is allowed")
	}
	if iss.hits["/realms/acme/.well-known/openid-configuration"] != 1 {
		t.Errorf("discovery hits = %d, want 1 (only the first validator discovers)", iss.hits["/realms/acme/.well-known/openid-configuration"])
	}
	// AllowConfirmation
	v, _ = NewJWTValidator(JWTValidatorConfig{Issuer: iss.issuer(), HTTPClient: iss.ts.Client(), AllowConfirmation: true})
	c := baseClaims(iss)
	c["cnf"] = map[string]any{"jkt": "x"}
	if _, err := v.ValidateToken(context.Background(), iss.mint(t, "rsa-1", c, nil)); err != nil {
		t.Errorf("cnf with AllowConfirmation: %v", err)
	}
}

// End to end: an identity-provider token accepted by ResourceServer only
// when its audience is this server.
func TestJWTValidator_WithResourceServer(t *testing.T) {
	iss := newJWTIssuer(t)
	v, _ := NewJWTValidator(JWTValidatorConfig{Issuer: iss.issuer(), HTTPClient: iss.ts.Client()})
	rs := &ResourceServer{
		Resource:             "https://mcp.example.com/mcp",
		AuthorizationServers: []string{iss.issuer()},
		RequiredScopes:       []string{"mcp:read"},
		ValidateToken:        v.Validate,
	}
	h := rs.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(TokenInfoFromContext(r.Context()).Subject))
	}))
	do := func(tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	if rr := do(iss.mint(t, "rsa-1", baseClaims(iss), nil)); rr.Code != 200 || rr.Body.String() != "user-1" {
		t.Fatalf("good: %d %s", rr.Code, rr.Body.String())
	}
	c := baseClaims(iss)
	c["aud"] = "https://other.example/api"
	if rr := do(iss.mint(t, "rsa-1", c, nil)); rr.Code != 401 || !strings.Contains(rr.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("wrong audience: %d %q", rr.Code, rr.Header().Get("WWW-Authenticate"))
	}
	c = baseClaims(iss)
	c["scope"] = "profile"
	if rr := do(iss.mint(t, "rsa-1", c, nil)); rr.Code != 403 || !strings.Contains(rr.Header().Get("WWW-Authenticate"), `error="insufficient_scope"`) {
		t.Fatalf("missing scope: %d %q", rr.Code, rr.Header().Get("WWW-Authenticate"))
	}
}

func TestJWTValidator_Hardening(t *testing.T) {
	iss := newJWTIssuer(t)
	v, _ := NewJWTValidator(JWTValidatorConfig{Issuer: iss.issuer(), HTTPClient: iss.ts.Client()})
	// scp as a space-separated string (Azure style) is split.
	c := baseClaims(iss)
	delete(c, "scope")
	c["scp"] = "a b"
	info, err := v.ValidateToken(context.Background(), iss.mint(t, "rsa-1", c, nil))
	if err != nil || !info.HasScope("b") {
		t.Fatalf("scp string: %+v %v", info, err)
	}
	// Non-integer NumericDate is accepted.
	c = baseClaims(iss)
	c["exp"] = float64(time.Now().Add(time.Hour).Unix()) + 0.5
	if _, err := v.ValidateToken(context.Background(), iss.mint(t, "rsa-1", c, nil)); err != nil {
		t.Fatalf("float exp: %v", err)
	}
	// A crit header is refused; an ES256 token signed with the wrong curve is refused.
	if _, err := v.ValidateToken(context.Background(), iss.mint(t, "rsa-1", baseClaims(iss), map[string]any{"crit": []string{"exp"}})); err == nil {
		t.Error("crit accepted")
	}
	if _, err := v.ValidateToken(context.Background(), iss.mint(t, "ec-1", baseClaims(iss), map[string]any{"alg": "ES384"})); err == nil {
		t.Error("ES384 with a P-256 key accepted")
	}
	// Discovery requires the exact issuer.
	bad, _ := NewJWTValidator(JWTValidatorConfig{Issuer: iss.issuer() + "/", HTTPClient: iss.ts.Client()})
	if _, err := bad.ValidateToken(context.Background(), iss.mint(t, "rsa-1", baseClaims(iss), nil)); err == nil {
		t.Error("issuer differing by a trailing slash accepted")
	}
}

func TestJWTValidator_KeyMetadataAndSize(t *testing.T) {
	iss := newJWTIssuer(t)
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	b64 := base64.RawURLEncoding.EncodeToString
	base := iss.jwks
	iss.jwks = func() map[string]any {
		doc := base()
		keys := doc["keys"].([]map[string]any)
		keys = append(keys,
			map[string]any{"kty": "RSA", "kid": "small", "n": b64(small.N.Bytes()), "e": "AQAB"},
			map[string]any{"kty": "RSA", "kid": "rs-only", "alg": "RS256", "n": b64(iss.rsaKey.N.Bytes()), "e": "AQAB"},
			map[string]any{"kty": "RSA", "kid": "oaep", "alg": "RSA-OAEP", "n": b64(iss.rsaKey.N.Bytes()), "e": "AQAB"},
			map[string]any{"kty": "RSA", "kid": "encops", "key_ops": []string{"encrypt"}, "n": b64(iss.rsaKey.N.Bytes()), "e": "AQAB"},
		)
		doc["keys"] = keys
		return doc
	}
	v, _ := NewJWTValidator(JWTValidatorConfig{Issuer: iss.issuer(), HTTPClient: iss.ts.Client()})
	// RS256 key published as alg RS256 refuses a PS256 token; encryption keys never verify.
	if _, err := v.ValidateToken(context.Background(), iss.mint(t, "rsa-1", baseClaims(iss), map[string]any{"kid": "rs-only", "alg": "PS256"})); err == nil {
		t.Error("PS256 verified with a key declared RS256")
	}
	if _, err := v.ValidateToken(context.Background(), iss.mint(t, "rsa-1", baseClaims(iss), map[string]any{"kid": "rs-only"})); err != nil {
		t.Errorf("RS256 with an RS256 key: %v", err)
	}
	for _, kid := range []string{"oaep", "encops"} {
		if _, err := v.ValidateToken(context.Background(), iss.mint(t, "rsa-1", baseClaims(iss), map[string]any{"kid": kid})); err == nil {
			t.Errorf("%s: an encryption key verified a signature", kid)
		}
	}
	// A 1024-bit key is never loaded.
	hj, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "small", "typ": "at+jwt"})
	cj, _ := json.Marshal(baseClaims(iss))
	input := base64.RawURLEncoding.EncodeToString(hj) + "." + base64.RawURLEncoding.EncodeToString(cj)
	sum := sha256.Sum256([]byte(input))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, small, crypto.SHA256, sum[:])
	if _, err := v.ValidateToken(context.Background(), input+"."+base64.RawURLEncoding.EncodeToString(sig)); err == nil {
		t.Error("1024-bit RSA key accepted")
	}
}

func TestJWTValidator_SourcesAndBackoff(t *testing.T) {
	if _, err := NewJWTValidator(JWTValidatorConfig{Issuer: "http://idp.example.com"}); err == nil {
		t.Error("http issuer accepted")
	}
	if _, err := NewJWTValidator(JWTValidatorConfig{Issuer: "https://idp.example.com", JWKSURL: "http://idp.example.com/keys"}); err == nil {
		t.Error("http JWKS URL accepted")
	}
	// A down key server is not hit on every request: junk tokens with a
	// plausible shape cause one fetch, then back off.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) }))
	defer down.Close()
	hits := 0
	counting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; http.Error(w, "down", 503) }))
	defer counting.Close()
	v, _ := NewJWTValidator(JWTValidatorConfig{Issuer: counting.URL + "/realm", JWKSURL: counting.URL + "/keys", HTTPClient: counting.Client()})
	hj, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": "x"})
	junk := base64.RawURLEncoding.EncodeToString(hj) + "." + base64.RawURLEncoding.EncodeToString([]byte("{}")) + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 256))
	for i := 0; i < 5; i++ {
		if _, err := v.ValidateToken(context.Background(), junk); err == nil {
			t.Fatal("junk accepted")
		}
	}
	if hits != 1 {
		t.Errorf("key server hit %d times while down, want 1 (backoff)", hits)
	}
	// A token whose signature cannot be right for its algorithm never reaches the key server.
	short := base64.RawURLEncoding.EncodeToString(hj) + "." + base64.RawURLEncoding.EncodeToString([]byte("{}")) + ".AAAA"
	v2, _ := NewJWTValidator(JWTValidatorConfig{Issuer: counting.URL + "/realm", JWKSURL: counting.URL + "/keys", HTTPClient: counting.Client()})
	hits = 0
	if _, err := v2.ValidateToken(context.Background(), short); err == nil || hits != 0 {
		t.Errorf("short signature: err=%v hits=%d", err, hits)
	}
}
