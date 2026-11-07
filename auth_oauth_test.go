package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Bearer challenges are parsed with token and quoted-string values, case-insensitive names and schemes, and other
// schemes' params are not taken for Bearer's.
func TestBearerChallenges(t *testing.T) {
	cases := []struct {
		name    string
		headers []string
		want    []map[string]string
	}{
		{"quoted", []string{`Bearer resource_metadata="https://h.example/prm", scope="a b"`},
			[]map[string]string{{"resource_metadata": "https://h.example/prm", "scope": "a b"}}},
		{"token values and case", []string{`bearer Resource_Metadata=https://h.example/prm,error=insufficient_scope`},
			[]map[string]string{{"resource_metadata": "https://h.example/prm", "error": "insufficient_scope"}}},
		{"escapes and commas in quotes", []string{`Bearer realm="a\"b, c", scope = "x"`},
			[]map[string]string{{"realm": `a"b, c`, "scope": "x"}}},
		{"other schemes skipped", []string{`Basic realm="r", resource_metadata="https://evil.example"`, `Basic dXNlcg==, Bearer scope="s"`},
			[]map[string]string{{"scope": "s"}}},
		{"several bearer", []string{`Bearer scope="a", DPoP algs="ES256", Bearer resource_metadata="https://h.example/x"`},
			[]map[string]string{{"scope": "a"}, {"resource_metadata": "https://h.example/x"}}},
		{"no params", []string{`Bearer`}, []map[string]string{{}}},
		{"empty", nil, nil},
	}
	for _, c := range cases {
		if got := bearerChallenges(c.headers); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: bearerChallenges(%q) = %v, want %v", c.name, c.headers, got, c.want)
		}
	}
}

// Authorization server metadata is looked for in the spec's order: RFC 8414 inserted, OpenID Connect inserted, then
// OpenID Connect appended (only for an issuer with a path, so none is asked for twice).
func TestAuthorizationServerMeta_DiscoveryOrder(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer ts.Close()

	cases := map[string][]string{
		ts.URL + "/o/org-1/": {
			"/.well-known/oauth-authorization-server/o/org-1",
			"/.well-known/openid-configuration/o/org-1",
			"/o/org-1/.well-known/openid-configuration",
		},
		ts.URL: {
			"/.well-known/oauth-authorization-server",
			"/.well-known/openid-configuration",
		},
	}
	for issuer, want := range cases {
		paths = nil
		if _, err := authorizationServerMeta(context.Background(), http.DefaultClient, issuer); err == nil {
			t.Errorf("%s: expected an error", issuer)
		}
		if !reflect.DeepEqual(paths, want) {
			t.Errorf("%s: asked %v, want %v", issuer, paths, want)
		}
	}
}

// asMetaServer serves meta (with "{origin}" in its string values replaced by its own origin) as the RFC 8414 metadata
// of the issuer at its root.
func asMetaServer(t *testing.T, meta map[string]any) *httptest.Server {
	t.Helper()
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			http.NotFound(w, r)
			return
		}
		doc := map[string]any{}
		for k, v := range meta {
			if s, ok := v.(string); ok {
				v = strings.ReplaceAll(s, "{origin}", ts.URL)
			}
			doc[k] = v
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func validASMeta() map[string]any {
	return map[string]any{
		"issuer":                           "{origin}",
		"authorization_endpoint":           "{origin}/authorize",
		"token_endpoint":                   "{origin}/token",
		"code_challenge_methods_supported": []string{"plain", "S256"},
	}
}

// The metadata's issuer must be exactly the one asked for: no trailing-slash normalisation.
func TestAuthorizationServerMeta_IssuerExact(t *testing.T) {
	meta := validASMeta()
	meta["issuer"] = "{origin}/"
	ts := asMetaServer(t, meta)
	_, err := authorizationServerMeta(context.Background(), http.DefaultClient, ts.URL)
	if err == nil || !strings.Contains(err.Error(), "is for another issuer") {
		t.Errorf("error = %v, want another issuer", err)
	}

	ok := asMetaServer(t, validASMeta())
	if _, err := authorizationServerMeta(context.Background(), http.DefaultClient, ok.URL+"/"); err == nil {
		t.Error("an issuer given with a trailing slash should not match metadata naming it without one")
	}
	if _, err := authorizationServerMeta(context.Background(), http.DefaultClient, ok.URL); err != nil {
		t.Errorf("exact issuer: %v", err)
	}
}

// Metadata URLs must be https, or http on a loopback host.
func TestValidateOAuthMeta(t *testing.T) {
	base := OAuthMeta{
		Issuer:                "https://as.example",
		AuthorizationEndpoint: "https://as.example/authorize",
		TokenEndpoint:         "https://as.example/token",
	}
	if err := validateOAuthMeta(&base); err != nil {
		t.Errorf("https metadata: %v", err)
	}
	for _, good := range []string{"http://localhost:8080/token", "http://127.0.0.5/token", "http://[::1]:9/token", "HTTPS://as.example/token"} {
		m := base
		m.TokenEndpoint = good
		if err := validateOAuthMeta(&m); err != nil {
			t.Errorf("token_endpoint %s: %v", good, err)
		}
	}
	bad := []string{"http://as.example/token", "javascript:alert(1)", "data:text/html,x", "file:///etc/passwd", "/token", "ftp://as.example/token"}
	for _, field := range []string{"issuer", "authorization_endpoint", "token_endpoint", "registration_endpoint"} {
		for _, value := range bad {
			m := base
			switch field {
			case "issuer":
				m.Issuer = value
			case "authorization_endpoint":
				m.AuthorizationEndpoint = value
			case "token_endpoint":
				m.TokenEndpoint = value
			case "registration_endpoint":
				m.RegistrationEndpoint = value
			}
			if err := validateOAuthMeta(&m); err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("%s %s: error = %v, want it refused", field, value, err)
			}
		}
	}
}

// A candidate with an endpoint that is not https is skipped, and the reason reported.
func TestAuthorizationServerMeta_InsecureEndpointRejected(t *testing.T) {
	meta := validASMeta()
	meta["authorization_endpoint"] = "javascript:alert(1)"
	ts := asMetaServer(t, meta)
	_, err := authorizationServerMeta(context.Background(), http.DefaultClient, ts.URL)
	if err == nil || !strings.Contains(err.Error(), "authorization_endpoint") {
		t.Errorf("error = %v, want the authorization_endpoint refused", err)
	}
}

// An authorization server that does not advertise PKCE S256 fails discovery, through protected resource metadata and
// through the legacy fallback alike.
func TestDiscoverOAuthMeta_PKCERequired(t *testing.T) {
	for name, methods := range map[string]any{"absent": nil, "plain only": []string{"plain"}} {
		meta := validASMeta()
		if methods == nil {
			delete(meta, "code_challenge_methods_supported")
		} else {
			meta["code_challenge_methods_supported"] = methods
		}
		as := asMetaServer(t, meta)
		ts := mcpServer(t, as.URL, "/.well-known/oauth-protected-resource", "", true)

		_, err := DiscoverOAuthMeta(context.Background(), ts.URL+"/mcp")
		if !errors.Is(err, ErrPKCENotSupported) {
			t.Fatalf("%s: error = %v, want ErrPKCENotSupported", name, err)
		}
		if want := "authorization server " + as.URL + " does not advertise PKCE S256"; !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want it to say %q", name, err, want)
		}

		// The same metadata as the server's own (legacy): refused too.
		if _, err := DiscoverOAuthMeta(context.Background(), as.URL+"/mcp"); !errors.Is(err, ErrPKCENotSupported) {
			t.Errorf("%s (legacy): error = %v, want ErrPKCENotSupported", name, err)
		}
	}
}

// Discovery URLs that are not https (or loopback http) are not fetched: a resource_metadata from the challenge, and an
// issuer the protected resource metadata names.
func TestDiscoverOAuthMeta_InsecureDiscoveryURLsSkipped(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "169.254.169.254" {
			t.Errorf("fetched %s", r.URL)
		}
		switch r.URL.Path {
		case "/mcp":
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://169.254.169.254/latest/meta-data"`)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	_, err := DiscoverOAuthMeta(context.Background(), ts.URL+"/mcp")
	if err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Errorf("error = %v, want the resource_metadata refused", err)
	}

	insecure := mcpServer(t, "http://as.example/o/org-1", "/.well-known/oauth-protected-resource", "", true)
	_, err = DiscoverOAuthMeta(context.Background(), insecure.URL+"/mcp")
	if err == nil || !strings.Contains(err.Error(), "authorization server http://as.example/o/org-1: not an https URL") {
		t.Errorf("error = %v, want the issuer refused", err)
	}
}

// A server that only takes POST (GET answers 405) is challenged with a JSON-RPC ping; the challenge's scope and the
// metadata's scopes come back.
func TestDiscoverOAuthMeta_PostOnlyServer(t *testing.T) {
	_, issuer := authServer(t, true, "")
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			body, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Type") != "application/json" || !strings.Contains(r.Header.Get("Accept"), "text/event-stream") ||
				!strings.Contains(string(body), `"method":"ping"`) {
				t.Errorf("probe = %s %q %q", body, r.Header.Get("Content-Type"), r.Header.Get("Accept"))
			}
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", scope="mcp:read mcp:tools", resource_metadata="/prm"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "/prm":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"resource":              ts.URL + "/mcp",
				"authorization_servers": []string{issuer},
				"scopes_supported":      []string{"mcp:read", "mcp:tools", "mcp:admin"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	meta, err := DiscoverOAuthMeta(context.Background(), ts.URL+"/mcp")
	if err != nil {
		t.Fatalf("DiscoverOAuthMeta: %v", err)
	}
	if meta.ChallengeScope != "mcp:read mcp:tools" {
		t.Errorf("ChallengeScope = %q", meta.ChallengeScope)
	}
	if !reflect.DeepEqual(meta.ResourceScopesSupported, []string{"mcp:read", "mcp:tools", "mcp:admin"}) {
		t.Errorf("ResourceScopesSupported = %v", meta.ResourceScopesSupported)
	}
	if meta.Resource != ts.URL+"/mcp" || !reflect.DeepEqual(meta.CodeChallengeMethodsSupported, []string{"S256"}) {
		t.Errorf("meta = %+v", meta)
	}
}

// The authorization server metadata fields callers need are decoded.
func TestDiscoverOAuthMeta_ExposesMetadata(t *testing.T) {
	meta := validASMeta()
	meta["scopes_supported"] = []string{"openid", "mcp"}
	meta["client_id_metadata_document_supported"] = true
	meta["authorization_response_iss_parameter_supported"] = true
	as := asMetaServer(t, meta)
	ts := mcpServer(t, as.URL, "/.well-known/oauth-protected-resource", "", true)

	got, err := DiscoverOAuthMeta(context.Background(), ts.URL+"/mcp")
	if err != nil {
		t.Fatalf("DiscoverOAuthMeta: %v", err)
	}
	if !reflect.DeepEqual(got.ScopesSupported, []string{"openid", "mcp"}) || !got.ClientIDMetadataDocumentSupported ||
		!got.AuthorizationResponseIssParameterSupported {
		t.Errorf("meta = %+v", got)
	}
}

// tokenEndpoint is a token endpoint recording each request's form.
func tokenEndpoint(t *testing.T, response map[string]any) (*httptest.Server, *[]url.Values) {
	t.Helper()
	var mu sync.Mutex
	var forms []url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		mu.Lock()
		forms = append(forms, r.PostForm)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(ts.Close)
	return ts, &forms
}

// WithResource sends the resource parameter in client credentials token requests.
func TestOAuth2Auth_ClientCredentials_Resource(t *testing.T) {
	ts, forms := tokenEndpoint(t, map[string]any{"access_token": "tok", "token_type": "Bearer", "expires_in": 3600})
	auth := NewOAuth2Auth("client-id", "secret", ts.URL, []string{"mcp"}, WithResource("https://mcp.example/mcp"))
	if _, err := auth.GetAuthHeader(); err != nil {
		t.Fatalf("GetAuthHeader: %v", err)
	}
	if len(*forms) != 1 || (*forms)[0].Get("resource") != "https://mcp.example/mcp" {
		t.Errorf("token requests = %v, want resource sent", *forms)
	}
}

// WithResource sends the resource parameter in refresh token requests, with client_id in the parameters; a refresh
// token the response does not rotate is kept.
func TestOAuth2Auth_RefreshTokenFlow_Resource(t *testing.T) {
	ts, forms := tokenEndpoint(t, map[string]any{"access_token": "refreshed", "token_type": "Bearer", "expires_in": 1})
	auth := NewOAuth2RefreshTokenAuth(ts.URL, "client-id", "", "refresh-1", WithResource("https://mcp.example/mcp"))

	header, err := auth.GetAuthHeader()
	if err != nil {
		t.Fatalf("GetAuthHeader: %v", err)
	}
	if header != "Bearer refreshed" {
		t.Errorf("GetAuthHeader() = %q", header)
	}
	// expires_in 1 is within oauth2's expiry delta, so the next call refreshes again, with the same refresh token.
	if _, err := auth.GetAuthHeader(); err != nil {
		t.Fatalf("GetAuthHeader (again): %v", err)
	}
	if len(*forms) != 2 {
		t.Fatalf("token requests = %v, want 2", *forms)
	}
	for _, form := range *forms {
		want := url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {"refresh-1"},
			"client_id":     {"client-id"},
			"resource":      {"https://mcp.example/mcp"},
		}
		if !reflect.DeepEqual(form, want) {
			t.Errorf("form = %v, want %v", form, want)
		}
	}
}

// A rotated refresh token is used for the next refresh, and a failed refresh reports the server's error.
func TestOAuth2Auth_RefreshTokenFlow_ResourceRotationAndError(t *testing.T) {
	calls := 0
	var refreshTokens []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		refreshTokens = append(refreshTokens, r.PostForm.Get("refresh_token"))
		calls++
		if calls == 3 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"expired"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "refresh_token": "rotated", "expires_in": "1"})
	}))
	defer ts.Close()

	auth := NewOAuth2RefreshTokenAuth(ts.URL, "client-id", "", "refresh-1", WithResource("https://mcp.example/mcp"))
	for range 2 {
		if _, err := auth.GetAuthHeader(); err != nil {
			t.Fatalf("GetAuthHeader: %v", err)
		}
	}
	_, err := auth.GetAuthHeader()
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error = %v, want invalid_grant", err)
	}
	if !reflect.DeepEqual(refreshTokens, []string{"refresh-1", "rotated", "rotated"}) {
		t.Errorf("refresh tokens sent = %v", refreshTokens)
	}
}

// application_type is native for a loopback or custom-scheme redirect, web otherwise, unless overridden; a 201 counts
// as success.
func TestRegisterOAuthClient_ApplicationType(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got, _ = body["application_type"].(string)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "abc"})
	}))
	defer ts.Close()

	cases := []struct {
		redirect string
		opts     []RegisterOption
		want     string
	}{
		{"http://127.0.0.1:8765/callback", nil, "native"},
		{"http://localhost/callback", nil, "native"},
		{"http://[::1]:9/cb", nil, "native"},
		{"com.example.app:/callback", nil, "native"},
		{"https://app.example/callback", nil, "web"},
		{"https://app.example/callback", []RegisterOption{WithApplicationType("native")}, "native"},
	}
	for _, c := range cases {
		id, err := RegisterOAuthClientWithClient(context.Background(), ts.Client(), ts.URL, "c", c.redirect, c.opts...)
		if err != nil || id != "abc" {
			t.Errorf("%s: id %q, error %v", c.redirect, id, err)
		}
		if got != c.want {
			t.Errorf("%s: application_type = %q, want %q", c.redirect, got, c.want)
		}
	}
}

// A non-2xx registration answer is an error naming the server's error and description, even with a client_id.
func TestRegisterOAuthClient_ErrorResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"client_id":"ignored","error":"invalid_redirect_uri","error_description":"not allowed"}`))
	}))
	defer ts.Close()

	_, err := RegisterOAuthClient(context.Background(), ts.URL, "c", "https://app.example/cb")
	if err == nil || !strings.Contains(err.Error(), "status 400") || !strings.Contains(err.Error(), "invalid_redirect_uri: not allowed") {
		t.Errorf("error = %v", err)
	}
}
