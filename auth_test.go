package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"
)

// TestBearerTokenAuth covers BearerTokenAuth's GetAuthHeader and Refresh (a
// no-op), which no other test exercises directly.
func TestBearerTokenAuth(t *testing.T) {
	auth := NewBearerTokenAuth("secret-token")
	header, err := auth.GetAuthHeader()
	if err != nil {
		t.Fatalf("GetAuthHeader: %v", err)
	}
	if want := "Bearer secret-token"; header != want {
		t.Errorf("GetAuthHeader() = %q, want %q", header, want)
	}
}

// TestDiscoverOAuthMeta_OAuthWellKnown verifies DiscoverOAuthMeta finds
// metadata at the RFC 8414 well-known path.
func TestDiscoverOAuthMeta_OAuthWellKnown(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-authorization-server" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(OAuthMeta{
				Issuer:                        ts.URL,
				AuthorizationEndpoint:         "https://example.com/authorize",
				TokenEndpoint:                 "https://example.com/token",
				RegistrationEndpoint:          "https://example.com/register",
				CodeChallengeMethodsSupported: []string{"S256"},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	meta, err := DiscoverOAuthMeta(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("DiscoverOAuthMeta: %v", err)
	}
	if meta.AuthorizationEndpoint != "https://example.com/authorize" {
		t.Errorf("AuthorizationEndpoint = %q", meta.AuthorizationEndpoint)
	}
	if meta.TokenEndpoint != "https://example.com/token" {
		t.Errorf("TokenEndpoint = %q", meta.TokenEndpoint)
	}
	if meta.RegistrationEndpoint != "https://example.com/register" {
		t.Errorf("RegistrationEndpoint = %q", meta.RegistrationEndpoint)
	}
	if meta.Resource != ts.URL {
		t.Errorf("Resource = %q, want the server URL %q", meta.Resource, ts.URL)
	}
}

// TestDiscoverOAuthMeta_OIDCFallback verifies DiscoverOAuthMeta falls back to
// the OIDC well-known path when the RFC 8414 path 404s.
func TestDiscoverOAuthMeta_OIDCFallback(t *testing.T) {
	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			http.NotFound(w, r)
		case "/.well-known/openid-configuration":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(OAuthMeta{
				Issuer:                        ts.URL,
				AuthorizationEndpoint:         "https://example.com/authorize",
				TokenEndpoint:                 "https://example.com/token",
				CodeChallengeMethodsSupported: []string{"S256"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	meta, err := DiscoverOAuthMeta(context.Background(), ts.URL+"/")
	if err != nil {
		t.Fatalf("DiscoverOAuthMeta: %v", err)
	}
	if meta.TokenEndpoint != "https://example.com/token" {
		t.Errorf("TokenEndpoint = %q", meta.TokenEndpoint)
	}
}

// TestDiscoverOAuthMeta_NotFound verifies DiscoverOAuthMeta returns an error
// when neither well-known endpoint is available or metadata is incomplete.
func TestDiscoverOAuthMeta_NotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	if _, err := DiscoverOAuthMeta(context.Background(), ts.URL); err == nil {
		t.Error("expected an error when no well-known endpoint is available")
	}

	// Invalid URL should also error (url.Parse failure path).
	if _, err := DiscoverOAuthMeta(context.Background(), "http://[::1]:namedport"); err == nil {
		t.Error("expected an error for an invalid server URL")
	}
}

// TestRegisterOAuthClient covers RFC 7591 dynamic client registration's
// success and failure paths.
func TestRegisterOAuthClient(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["client_name"] != "my-client" {
			t.Errorf("client_name = %v, want my-client", body["client_name"])
		}
		if body["application_type"] != "web" {
			t.Errorf("application_type = %v, want web", body["application_type"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"client_id": "abc123"})
	}))
	defer ts.Close()

	clientID, err := RegisterOAuthClient(context.Background(), ts.URL, "my-client", "https://example.com/callback")
	if err != nil {
		t.Fatalf("RegisterOAuthClient: %v", err)
	}
	if clientID != "abc123" {
		t.Errorf("clientID = %q, want abc123", clientID)
	}
}

// TestRegisterOAuthClient_Failure covers the error path when the
// registration endpoint doesn't return a usable client_id.
func TestRegisterOAuthClient_Failure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	if _, err := RegisterOAuthClient(context.Background(), ts.URL, "my-client", "https://example.com/callback"); err == nil {
		t.Error("expected an error when client_id is missing")
	}
}

// TestOAuth2Auth_ClientCredentials verifies NewOAuth2Auth's client-credentials
// flow: GetAuthHeader fetches and caches a token, and Refresh forces a new
// fetch.
func TestOAuth2Auth_ClientCredentials(t *testing.T) {
	tokenCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "tok-1",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer ts.Close()

	auth := NewOAuth2Auth("client-id", "client-secret", ts.URL, []string{"scope1"})

	header, err := auth.GetAuthHeader()
	if err != nil {
		t.Fatalf("GetAuthHeader: %v", err)
	}
	if header != "Bearer tok-1" {
		t.Errorf("GetAuthHeader() = %q, want Bearer tok-1", header)
	}
	if tokenCalls != 1 {
		t.Fatalf("expected 1 token call, got %d", tokenCalls)
	}

	// Cached token: no new call.
	if _, err := auth.GetAuthHeader(); err != nil {
		t.Fatalf("GetAuthHeader (cached): %v", err)
	}
	if tokenCalls != 1 {
		t.Errorf("expected token to be cached, got %d calls", tokenCalls)
	}
}

// TestOAuth2Auth_ClientCredentials_Error verifies GetAuthHeader and Refresh
// surface an error when the token endpoint fails.
func TestOAuth2Auth_ClientCredentials_Error(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	auth := NewOAuth2Auth("client-id", "bad-secret", ts.URL, nil)
	if _, err := auth.GetAuthHeader(); err == nil {
		t.Error("expected GetAuthHeader to fail")
	}
}

// TestOAuth2Auth_RefreshTokenFlow verifies NewOAuth2RefreshTokenAuth: an
// initial valid access token is used as-is without hitting the token
// endpoint.
func TestOAuth2Auth_RefreshTokenFlow(t *testing.T) {
	tokenCalls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "refreshed-tok",
			"refresh_token": "refresh-2",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer ts.Close()

	// No initial access token forces a refresh-token exchange immediately.
	auth := NewOAuth2RefreshTokenAuth(ts.URL, "client-id", "", "refresh-1")
	header, err := auth.GetAuthHeader()
	if err != nil {
		t.Fatalf("GetAuthHeader: %v", err)
	}
	if header != "Bearer refreshed-tok" {
		t.Errorf("GetAuthHeader() = %q, want Bearer refreshed-tok", header)
	}
	if tokenCalls != 1 {
		t.Errorf("expected 1 token exchange, got %d", tokenCalls)
	}
}

// TestOAuth2Auth_RefreshTokenFlow_ExistingValidToken verifies that when the
// initial access token is already present and valid (as oauth2.Token.Valid
// determines: non-empty and not expired), GetAuthHeader uses it without any
// network call.
func TestOAuth2Auth_RefreshTokenFlow_ExistingValidToken(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	auth := NewOAuth2RefreshTokenAuth(ts.URL, "client-id", "already-have-token", "refresh-1")
	// Manually seed a valid, non-expiring token the way oauth2.ReuseTokenSource
	// would treat as still valid (zero Expiry means "doesn't expire").
	auth.mu.Lock()
	auth.token = &oauth2.Token{AccessToken: "already-have-token"}
	auth.mu.Unlock()

	header, err := auth.GetAuthHeader()
	if err != nil {
		t.Fatalf("GetAuthHeader: %v", err)
	}
	if header != "Bearer already-have-token" {
		t.Errorf("GetAuthHeader() = %q, want Bearer already-have-token", header)
	}
	if calls != 0 {
		t.Errorf("expected no network calls for a cached valid token, got %d", calls)
	}
}
