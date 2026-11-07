package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// authServer is an authorization server whose issuer has a path, publishing its metadata the RFC 8414 way
// (oauthWellKnown) or as OpenID Connect discovery under the issuer.
func authServer(t *testing.T, oauthWellKnown bool, issuerOverride string) (*httptest.Server, string) {
	t.Helper()
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer := ts.URL + "/o/org-1"
		want := "/o/org-1/.well-known/openid-configuration"
		if oauthWellKnown {
			want = "/.well-known/oauth-authorization-server/o/org-1"
		}
		if r.URL.Path != want {
			http.NotFound(w, r)
			return
		}
		if issuerOverride != "" {
			issuer = issuerOverride
		}
		_ = json.NewEncoder(w).Encode(OAuthMeta{
			Issuer:                        issuer,
			AuthorizationEndpoint:         ts.URL + "/o/org-1/oauth/authorize",
			TokenEndpoint:                 ts.URL + "/o/org-1/oauth/token",
			RegistrationEndpoint:          ts.URL + "/o/org-1/oauth/register",
			CodeChallengeMethodsSupported: []string{"S256"},
		})
	}))
	t.Cleanup(ts.Close)
	return ts, ts.URL + "/o/org-1"
}

// mcpServer is an MCP server at /mcp whose authorization server is issuer, publishing its protected resource metadata
// at metadataPath for resource (the server's own address when empty), and naming it in its 401 challenge when
// challenge is set. It has no authorization server metadata of its own.
func mcpServer(t *testing.T, issuer, metadataPath, resource string, challenge bool) *httptest.Server {
	t.Helper()
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp":
			if challenge {
				w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+ts.URL+metadataPath+`"`)
			}
			w.WriteHeader(http.StatusUnauthorized)
		case metadataPath:
			res := resource
			if res == "" {
				res = ts.URL + "/mcp"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": res, "authorization_servers": []string{issuer}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// An MCP server that is not its own authorization server: its 401 names its protected resource metadata, which names
// the authorization server. The resource comes back to ask the token for; the server's query does not matter.
func TestDiscoverOAuthMeta_ProtectedResourceFromChallenge(t *testing.T) {
	as, issuer := authServer(t, true, "")
	ts := mcpServer(t, issuer, "/.well-known/oauth-protected-resource", "", true)

	meta, err := DiscoverOAuthMeta(context.Background(), ts.URL+"/mcp?show_all=true")
	if err != nil {
		t.Fatalf("DiscoverOAuthMeta: %v", err)
	}
	if meta.TokenEndpoint != as.URL+"/o/org-1/oauth/token" || meta.RegistrationEndpoint != as.URL+"/o/org-1/oauth/register" {
		t.Errorf("endpoints = %+v", meta)
	}
	if meta.Resource != ts.URL+"/mcp" {
		t.Errorf("Resource = %q, want %q", meta.Resource, ts.URL+"/mcp")
	}
}

// Without a challenge naming it, the metadata is looked for at the well-known address for the server's path; the
// authorization server's metadata as OpenID Connect discovery under its issuer.
func TestDiscoverOAuthMeta_ProtectedResourceAtWellKnownPath(t *testing.T) {
	_, issuer := authServer(t, false, "")
	ts := mcpServer(t, issuer, "/.well-known/oauth-protected-resource/mcp", "", false)

	meta, err := DiscoverOAuthMeta(context.Background(), ts.URL+"/mcp")
	if err != nil {
		t.Fatalf("DiscoverOAuthMeta: %v", err)
	}
	if meta.Issuer != issuer || meta.Resource != ts.URL+"/mcp" {
		t.Errorf("meta = %+v", meta)
	}
}

// Metadata for another resource (RFC 9728 section 3.3), or an authorization server naming another issuer (RFC 8414
// section 3.3), is not used.
func TestDiscoverOAuthMeta_ProtectedResourceMismatchesAreIgnored(t *testing.T) {
	_, issuer := authServer(t, true, "")
	other := mcpServer(t, issuer, "/.well-known/oauth-protected-resource", "https://elsewhere.example/mcp", true)
	if _, err := DiscoverOAuthMeta(context.Background(), other.URL+"/mcp"); err == nil {
		t.Error("metadata for another resource was used")
	}

	_, wrongIssuer := authServer(t, true, "https://impostor.example/o/org-1")
	ts := mcpServer(t, wrongIssuer, "/.well-known/oauth-protected-resource", "", true)
	if _, err := DiscoverOAuthMeta(context.Background(), ts.URL+"/mcp"); err == nil {
		t.Error("an authorization server naming another issuer was used")
	}
}

// A resource covering the server (its host) is accepted; one beside it is not.
func TestCoversResource(t *testing.T) {
	cases := map[string]bool{
		"https://h.example/mcp":   true,
		"https://H.example/mcp/":  true,
		"https://h.example":       true,
		"https://h.example/mcpx":  false,
		"https://h.example/other": false,
		"http://h.example/mcp":    false,
		"https://x.example/mcp":   false,
	}
	u, _ := http.NewRequest(http.MethodGet, "https://h.example/mcp?show_all=true", nil)
	for resource, want := range cases {
		if got := coversResource(resource, u.URL, true); got != want {
			t.Errorf("coversResource(%q, parent allowed) = %v, want %v", resource, got, want)
		}
	}
	// A document found at the path-inserted or challenge URL must name the
	// server exactly (RFC 9728 section 3.3): a parent is not enough there.
	if coversResource("https://h.example", u.URL, false) {
		t.Error("a parent resource was accepted for an exact-match document")
	}
	if !coversResource("https://h.example/mcp/", u.URL, false) {
		t.Error("the server's own resource (trailing slash) was refused")
	}
}

// legacyServer is an MCP server at /mcp that is its own authorization server, publishing its metadata at the root of
// its host with issuer ("{origin}" standing for its own origin), and, when protectedBy is set, protected resource
// metadata naming the authorization server protectedBy.
func legacyServer(t *testing.T, issuer, protectedBy string) *httptest.Server {
	t.Helper()
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(OAuthMeta{
				Issuer:                        strings.ReplaceAll(issuer, "{origin}", ts.URL),
				AuthorizationEndpoint:         ts.URL + "/authorize",
				TokenEndpoint:                 ts.URL + "/token",
				CodeChallengeMethodsSupported: []string{"S256"},
			})
		case "/.well-known/oauth-protected-resource":
			if protectedBy == "" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": ts.URL + "/mcp", "authorization_servers": []string{protectedBy}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// A server publishing both protected resource metadata and authorization server metadata of its own is found through
// its protected resource metadata: RFC 9728 first, as the MCP authorization spec requires.
func TestDiscoverOAuthMeta_ProtectedResourceWinsOverOwnAuthorizationServer(t *testing.T) {
	as, issuer := authServer(t, true, "")
	ts := legacyServer(t, "{origin}", issuer)

	meta, err := DiscoverOAuthMeta(context.Background(), ts.URL+"/mcp")
	if err != nil {
		t.Fatalf("DiscoverOAuthMeta: %v", err)
	}
	if meta.TokenEndpoint != as.URL+"/o/org-1/oauth/token" || meta.Resource != ts.URL+"/mcp" {
		t.Errorf("meta = %+v", meta)
	}
}

// A server publishing no protected resource metadata, only authorization server metadata of its own (the legacy
// fallback), is found when that metadata's issuer is exactly its origin, with its own URL as the resource.
func TestDiscoverOAuthMeta_OwnAuthorizationServerFallback(t *testing.T) {
	ts := legacyServer(t, "{origin}", "")
	meta, err := DiscoverOAuthMeta(context.Background(), ts.URL+"/mcp/")
	if err != nil {
		t.Fatalf("DiscoverOAuthMeta: %v", err)
	}
	if meta.TokenEndpoint != ts.URL+"/token" || meta.Resource != ts.URL+"/mcp" {
		t.Errorf("meta = %+v", meta)
	}

	for _, issuer := range []string{"https://impostor.example", "{origin}/", "{origin}/mcp", ""} {
		other := legacyServer(t, issuer, "")
		_, err := DiscoverOAuthMeta(context.Background(), other.URL+"/mcp")
		if err == nil || !strings.Contains(err.Error(), "is for another issuer") {
			t.Errorf("issuer %q: error = %v, want it refused as another issuer", issuer, err)
		}
	}
}

// When the authorization server the metadata names cannot be read, the error says which and why (here: nothing
// listening, as an untrusted certificate would be reported too), rather than only that nothing was found.
func TestDiscoverOAuthMeta_UnreadableAuthorizationServerIsReported(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	issuer := gone.URL + "/o/org-1"
	gone.Close()
	ts := mcpServer(t, issuer, "/.well-known/oauth-protected-resource", "", true)

	_, err := DiscoverOAuthMeta(context.Background(), ts.URL+"/mcp")
	if err == nil || !strings.Contains(err.Error(), "authorization server "+issuer) {
		t.Errorf("error = %v, want it to name the authorization server %s", err, issuer)
	}
}
