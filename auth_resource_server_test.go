package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testResourceServer(resource string) *ResourceServer {
	return &ResourceServer{
		Resource:             resource,
		AuthorizationServers: []string{"https://idp.example/realms/acme"},
		ScopesSupported:      []string{"mcp:read", "mcp:write"},
		ResourceName:         "Test MCP",
		ValidateToken: func(r *http.Request, token string) (*TokenInfo, error) {
			switch token {
			case "tok-good":
				return &TokenInfo{Subject: "alice", Audience: []string{resource}, Scopes: []string{"mcp:read"}}, nil
			case "tok-otheraud":
				return &TokenInfo{Subject: "bob", Audience: []string{"https://other.example/api"}, Scopes: []string{"mcp:read"}}, nil
			case "tok-stale":
				return &TokenInfo{Subject: "carol", Audience: []string{resource}, Expiry: time.Now().Add(-time.Minute)}, nil
			}
			return nil, errors.New("unknown token")
		},
	}
}

func TestResourceServer_MetadataPathAndDocument(t *testing.T) {
	rs := testResourceServer("https://mcp.example.com/public/mcp/")
	if got, want := rs.MetadataPath(), "/.well-known/oauth-protected-resource/public/mcp"; got != want {
		t.Fatalf("MetadataPath = %q, want %q", got, want)
	}
	if got, want := (&ResourceServer{Resource: "https://mcp.example.com"}).MetadataPath(), "/.well-known/oauth-protected-resource"; got != want {
		t.Fatalf("root MetadataPath = %q, want %q", got, want)
	}
	rr := httptest.NewRecorder()
	rs.MetadataHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, rs.MetadataPath(), nil))
	if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status %d content-type %q", rr.Code, rr.Header().Get("Content-Type"))
	}
	var doc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["resource"] != "https://mcp.example.com/public/mcp/" || doc["resource_name"] != "Test MCP" {
		t.Fatalf("doc = %v", doc)
	}
	if as, _ := doc["authorization_servers"].([]any); len(as) != 1 || as[0] != "https://idp.example/realms/acme" {
		t.Fatalf("authorization_servers = %v", doc["authorization_servers"])
	}
	if sc, _ := doc["scopes_supported"].([]any); len(sc) != 2 {
		t.Fatalf("scopes_supported = %v", doc["scopes_supported"])
	}
	if bm, _ := doc["bearer_methods_supported"].([]any); len(bm) != 1 || bm[0] != "header" {
		t.Fatalf("bearer_methods_supported = %v", doc["bearer_methods_supported"])
	}
}

func TestResourceServer_Protect(t *testing.T) {
	resource := "https://mcp.example.com/mcp"
	rs := testResourceServer(resource)
	var seen *TokenInfo
	h := rs.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = TokenInfoFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	do := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	wantMeta := `resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource/mcp"`

	// No token: 401 with the metadata URL and the scopes to request.
	rr := do("")
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status %d", rr.Code)
	}
	ch := rr.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(ch, "Bearer ") || !strings.Contains(ch, wantMeta) || !strings.Contains(ch, `scope="mcp:read mcp:write"`) || strings.Contains(ch, "error=") {
		t.Fatalf("no token challenge = %q", ch)
	}

	// Invalid, expired, and wrong-audience tokens: 401 invalid_token, and
	// the token itself never appears in the response.
	for _, tok := range []string{"tok-bogus", "tok-stale", "tok-otheraud"} {
		rr := do("Bearer " + tok)
		ch := rr.Header().Get("WWW-Authenticate")
		if rr.Code != http.StatusUnauthorized || !strings.Contains(ch, `error="invalid_token"`) || !strings.Contains(ch, wantMeta) {
			t.Fatalf("%s: status %d challenge %q", tok, rr.Code, ch)
		}
		if strings.Contains(rr.Body.String(), tok) || strings.Contains(ch, tok) {
			t.Fatalf("%s: token leaked into the response", tok)
		}
	}

	// Good token: through, with TokenInfo in the context.
	if rr := do("Bearer tok-good"); rr.Code != http.StatusOK {
		t.Fatalf("good token: status %d body %s", rr.Code, rr.Body.String())
	}
	if seen == nil || seen.Subject != "alice" || !seen.HasScope("mcp:read") {
		t.Fatalf("TokenInfoFromContext = %+v", seen)
	}
	// Case-insensitive scheme and a trailing slash in aud are tolerated.
	rs.ValidateToken = func(r *http.Request, token string) (*TokenInfo, error) {
		return &TokenInfo{Audience: []string{"HTTPS://MCP.example.com/mcp/"}}, nil
	}
	if rr := do("Bearer x"); rr.Code != http.StatusOK {
		t.Fatalf("robust audience: status %d", rr.Code)
	}
	// Alternative audience accepted when configured.
	rs.Audiences = []string{"acme-api"}
	rs.ValidateToken = func(r *http.Request, token string) (*TokenInfo, error) {
		return &TokenInfo{Audience: []string{"acme-api"}}, nil
	}
	if rr := do("Bearer x"); rr.Code != http.StatusOK {
		t.Fatalf("alternative audience: status %d", rr.Code)
	}

	// Preflight passes without a token.
	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("OPTIONS: status %d", rr.Code)
	}
}

func TestResourceServer_InsufficientScope(t *testing.T) {
	rs := testResourceServer("https://mcp.example.com/mcp")
	rs.RequiredScopes = []string{"mcp:read", "mcp:write"}
	h := rs.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer tok-good") // has mcp:read only
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	ch := rr.Header().Get("WWW-Authenticate")
	if rr.Code != http.StatusForbidden || !strings.Contains(ch, `error="insufficient_scope"`) || !strings.Contains(ch, `scope="mcp:read mcp:write"`) || !strings.Contains(ch, "resource_metadata=") {
		t.Fatalf("status %d challenge %q", rr.Code, ch)
	}
	var body map[string]string
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	if body["error"] != "insufficient_scope" {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

func TestResourceServer_MountEndToEnd(t *testing.T) {
	s := NewServer("protected", "1")
	s.RegisterTool(NewTool("whoami", "who"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText(TokenInfoFromContext(ctx).Subject), nil
	})
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()
	rs := testResourceServer(ts.URL + "/mcp")
	rs.ServeRootMetadata = true
	rs.Mount(mux, http.HandlerFunc(s.HandleRequest))

	resp, err := http.Get(ts.URL + "/.well-known/oauth-protected-resource/mcp")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("metadata: %v %v", err, resp)
	}
	resp.Body.Close()

	call := func(auth string) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set(headerProtocolVersion, "2025-11-25")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}
	if resp, _ := call(""); resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp") {
		t.Fatalf("unauthenticated: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if resp, body := call("Bearer tok-good"); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"alice"`) {
		t.Fatalf("authenticated: %d %s", resp.StatusCode, body)
	}

	// The root well-known path is served too, and DELETE/GET pass auth the same way.
	resp, _ = http.Get(ts.URL + "/.well-known/oauth-protected-resource")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("root metadata: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// Per-request resource and authorization servers (a multi-tenant server)
// flow into the metadata, the audience check and the challenge.
func TestResourceServer_PerRequest(t *testing.T) {
	rs := &ResourceServer{
		ResourceFunc:             func(r *http.Request) string { return "https://" + r.Host + "/mcp" },
		AuthorizationServersFunc: func(r *http.Request) []string { return []string{"https://idp.example/" + r.Host} },
		ValidateToken: func(r *http.Request, token string) (*TokenInfo, error) {
			return &TokenInfo{Audience: []string{"https://" + r.Host + "/mcp"}}, nil
		},
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil)
	req.Host = "tenant-a.example"
	rs.MetadataHandler().ServeHTTP(rr, req)
	var doc map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &doc)
	if doc["resource"] != "https://tenant-a.example/mcp" {
		t.Fatalf("doc = %v", doc)
	}
	if as, _ := doc["authorization_servers"].([]any); len(as) != 1 || as[0] != "https://idp.example/tenant-a.example" {
		t.Fatalf("authorization_servers = %v", doc["authorization_servers"])
	}
	h := rs.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	req = httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Host = "tenant-b.example"
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if ch := rr.Header().Get("WWW-Authenticate"); rr.Code != http.StatusUnauthorized || !strings.Contains(ch, `resource_metadata="https://tenant-b.example/.well-known/oauth-protected-resource/mcp"`) {
		t.Fatalf("status %d challenge %q", rr.Code, ch)
	}
	req.Header.Set("Authorization", "Bearer x")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("tenant token: status %d", rr.Code)
	}
}

// StaticTokenValidator: the configured secret, compared in constant time,
// with no audience needed.
func TestStaticTokenValidator(t *testing.T) {
	rs := &ResourceServer{
		Resource:      "https://mcp.example.com/mcp",
		ValidateToken: StaticTokenValidator(map[string]*TokenInfo{"s3cret": {Subject: "ops", Scopes: []string{"mcp"}}}),
	}
	// A validator that reports no audience and does not vouch for the
	// token (an identity-provider token with no aud) is refused.
	noAud := &ResourceServer{Resource: "https://mcp.example.com/mcp", ValidateToken: func(r *http.Request, token string) (*TokenInfo, error) {
		return &TokenInfo{Subject: "anon"}, nil
	}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer x")
	noAud.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("token with no audience: status %d, want 401", rr.Code)
	}
	h := rs.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(TokenInfoFromContext(r.Context()).Subject))
	}))
	for tok, want := range map[string]int{"s3cret": 200, "s3cre": 401, "s3cret2": 401, "": 401} {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Errorf("%q: status %d, want %d", tok, rr.Code, want)
		}
		if want == 200 && rr.Body.String() != "ops" {
			t.Errorf("subject = %q", rr.Body.String())
		}
	}
}

func TestResourceServer_Hardening(t *testing.T) {
	rs := testResourceServer("https://mcp.example.com/mcp")
	h := rs.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	do := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}
	// Malformed Bearer credentials: 400 invalid_request. Another scheme: 401 without error.
	for _, auth := range []string{"Bearer", "Bearer ", "Bearer a b", "Bearer x\"y"} {
		if rr := do(auth); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Header().Get("WWW-Authenticate"), `error="invalid_request"`) {
			t.Errorf("%q: status %d challenge %q", auth, rr.Code, rr.Header().Get("WWW-Authenticate"))
		}
	}
	if rr := do("Basic abc"); rr.Code != http.StatusUnauthorized || strings.Contains(rr.Header().Get("WWW-Authenticate"), "error=") {
		t.Errorf("Basic: status %d challenge %q", rr.Code, rr.Header().Get("WWW-Authenticate"))
	}
	// Audience with a query, fragment or userinfo never matches.
	for _, aud := range []string{"https://mcp.example.com/mcp?x=1", "https://mcp.example.com/mcp#f", "https://u@mcp.example.com/mcp"} {
		rs.ValidateToken = func(r *http.Request, token string) (*TokenInfo, error) {
			return &TokenInfo{Audience: []string{aud}}, nil
		}
		if rr := do("Bearer tok"); rr.Code != http.StatusUnauthorized {
			t.Errorf("aud %q accepted", aud)
		}
	}
	// A non-URL alternative audience compares exactly.
	rs.Audiences = []string{"api://mcp"}
	rs.ValidateToken = func(r *http.Request, token string) (*TokenInfo, error) {
		return &TokenInfo{Audience: []string{"API://MCP"}}, nil
	}
	if rr := do("Bearer tok"); rr.Code != http.StatusUnauthorized {
		t.Error("case-folded alternative audience accepted")
	}
	// A static token with a configured audience is checked like any other.
	rs.Audiences = nil
	rs.ValidateToken = StaticTokenValidator(map[string]*TokenInfo{"tok": {Audience: []string{"https://other.example/api"}}})
	if rr := do("Bearer tok"); rr.Code != http.StatusUnauthorized {
		t.Error("static token with a foreign audience accepted")
	}
	// Scope tokens that would break the challenge are dropped from it.
	rs.RequiredScopes = []string{"ok", `bad"scope`}
	rr := httptest.NewRecorder()
	rs.RejectScope(rr, httptest.NewRequest(http.MethodPost, "/mcp", nil), rs.RequiredScopes)
	if ch := rr.Header().Get("WWW-Authenticate"); !strings.Contains(ch, `scope="ok"`) || strings.Contains(ch, "bad") {
		t.Errorf("challenge = %q", ch)
	}
}

// Mount with per-request resources serves every advertised metadata path.
func TestResourceServer_MountPerRequest(t *testing.T) {
	rs := &ResourceServer{
		ResourceFunc:             func(r *http.Request) string { return "https://" + r.Host + "/mcp" },
		AuthorizationServersFunc: func(r *http.Request) []string { return []string{"https://idp.example"} },
		ValidateToken:            StaticTokenValidator(map[string]*TokenInfo{"t": nil}),
	}
	mux := http.NewServeMux()
	rs.Mount(mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	for _, p := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-protected-resource"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Host = "t.example"
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"resource":"https://t.example/mcp"`) {
			t.Fatalf("%s: %d %s", p, rr.Code, rr.Body.String())
		}
	}
}

func TestResourceServer_AudienceEdgeCases(t *testing.T) {
	rs := &ResourceServer{Resource: "https://mcp.example.com/a/b"}
	h := rs.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	try := func(aud string) int {
		rs.ValidateToken = func(r *http.Request, token string) (*TokenInfo, error) {
			return &TokenInfo{Audience: []string{aud}}, nil
		}
		req := httptest.NewRequest(http.MethodPost, "/a/b", nil)
		req.Header.Set("Authorization", "Bearer tok")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr.Code
	}
	for _, aud := range []string{"https://mcp.example.com/a%2Fb", "https://mcp.example.com/a/b?", "https://mcp.example.com/a/b#", ""} {
		if try(aud) != http.StatusUnauthorized {
			t.Errorf("aud %q accepted", aud)
		}
	}
	if try("https://mcp.example.com/a/b/") != http.StatusOK {
		t.Error("trailing slash refused")
	}
	// An unconfigured resource matches nothing, even an empty audience.
	empty := &ResourceServer{ValidateToken: func(r *http.Request, token string) (*TokenInfo, error) {
		return &TokenInfo{Audience: []string{""}}, nil
	}}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	empty.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Error("empty resource accepted an empty audience")
	}
	// Bearer credentials of only padding are malformed.
	for _, auth := range []string{"Bearer =", "Bearer =="} {
		req := httptest.NewRequest(http.MethodPost, "/a/b", nil)
		req.Header.Set("Authorization", auth)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", auth, rr.Code)
		}
	}
}
