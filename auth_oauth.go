package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// OAuthMeta holds the OAuth2 server metadata discovered via RFC 8414.
type OAuthMeta struct {
	Issuer                string `json:"issuer,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`

	// Resource is the MCP server's resource identifier from its protected resource metadata (RFC 9728), to send as the
	// resource parameter (RFC 8707) when authorizing and exchanging the code, so the token is issued for this server.
	// Empty when the server is its own authorization server (legacyOAuthMeta), which needs none.
	Resource string `json:"-"`
}

// maxMetadataBytes bounds a metadata document read during discovery.
const maxMetadataBytes = 1 << 20

var resourceMetadataParam = regexp.MustCompile(`resource_metadata="([^"]+)"`)

// DiscoverOAuthMeta fetches OAuth2 server metadata for the MCP server at serverURL, with http.DefaultClient. See
// DiscoverOAuthMetaWithClient.
func DiscoverOAuthMeta(ctx context.Context, serverURL string) (*OAuthMeta, error) {
	return DiscoverOAuthMetaWithClient(ctx, http.DefaultClient, serverURL)
}

// DiscoverOAuthMetaWithClient fetches OAuth2 server metadata for the MCP server at serverURL: first at the root of
// its host, for a server that is its own authorization server (legacyOAuthMeta); otherwise from the authorization
// server its protected resource metadata names (RFC 9728, the MCP 2025-06-18 authorization spec), with the resource
// to ask a token for in Resource.
func DiscoverOAuthMetaWithClient(ctx context.Context, client *http.Client, serverURL string) (*OAuthMeta, error) {
	u, err := url.Parse(strings.TrimSuffix(serverURL, "/"))
	if err != nil {
		return nil, err
	}
	if meta := legacyOAuthMeta(ctx, client, u); meta != nil {
		return meta, nil
	}
	meta, why := protectedResourceOAuthMeta(ctx, client, u)
	if meta != nil {
		return meta, nil
	}
	if why != nil {
		return nil, fmt.Errorf("could not discover OAuth metadata from %s: %w", serverURL, why)
	}
	return nil, fmt.Errorf("could not discover OAuth metadata from %s", serverURL)
}

// legacyOAuthMeta finds an MCP server that is its own authorization server, publishing its metadata at the root of its
// host (the MCP 2025-03-26 authorization spec). Tried first, so such servers are found exactly as before.
//
// LEGACY(fortix-mcp-own-oauth): Freedom 3 instances that do not sign in through their identity provider (IdP) are
// served by a fortix-mcp that is its own authorization server, found here. Remove this, and its call, once every
// Freedom 3 instance signs in only through the IdP: those are found by protectedResourceOAuthMeta.
func legacyOAuthMeta(ctx context.Context, client *http.Client, u *url.URL) *OAuthMeta {
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"} {
		var meta OAuthMeta
		if getJSON(ctx, client, originURL(u, path), &meta) == nil && meta.AuthorizationEndpoint != "" && meta.TokenEndpoint != "" {
			return &meta
		}
	}
	return nil
}

// protectedResourceOAuthMeta finds the authorization server an MCP server names in its protected resource metadata
// (RFC 9728): at the address its 401 challenge gives, else at the well-known address for its path, else for its host.
// A document whose resource is not this server is ignored (RFC 9728 section 3.3), as is an authorization server whose
// metadata names another issuer (RFC 8414 section 3.3). When none is found, the error says why when there is more to
// say than "not published": an authorization server that was named but could not be read, or a request that failed.
func protectedResourceOAuthMeta(ctx context.Context, client *http.Client, u *url.URL) (*OAuthMeta, error) {
	var why error
	for _, metadataURL := range protectedResourceMetadataURLs(ctx, client, u) {
		var prm struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
		}
		if err := getJSON(ctx, client, metadataURL, &prm); err != nil {
			why = moreTelling(why, err)
			continue
		}
		if !coversResource(prm.Resource, u) {
			why = moreTelling(why, fmt.Errorf("%s is for another resource (%s)", metadataURL, prm.Resource))
			continue
		}
		for _, issuer := range prm.AuthorizationServers {
			meta, err := authorizationServerMeta(ctx, client, issuer)
			if meta != nil {
				meta.Resource = prm.Resource
				return meta, nil
			}
			why = fmt.Errorf("authorization server %s: %w", issuer, err)
		}
	}
	return nil, why
}

// moreTelling keeps the more telling of two discovery failures: a document that is simply not there says least.
func moreTelling(kept, err error) error {
	var notThere *notPublishedError
	if kept == nil || errors.As(kept, &notThere) {
		return err
	}
	return kept
}

// protectedResourceMetadataURLs lists where u's protected resource metadata may be, most specific first: the
// resource_metadata of the server's 401 challenge, then the well-known address for its path, then for its host.
func protectedResourceMetadataURLs(ctx context.Context, client *http.Client, u *url.URL) []string {
	var urls []string
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil); err == nil {
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				for _, challenge := range resp.Header.Values("WWW-Authenticate") {
					if m := resourceMetadataParam.FindStringSubmatch(challenge); m != nil {
						if ref, err := u.Parse(m[1]); err == nil {
							urls = append(urls, ref.String())
						}
					}
				}
			}
		}
	}
	if path := strings.TrimSuffix(u.Path, "/"); path != "" {
		urls = append(urls, originURL(u, "/.well-known/oauth-protected-resource"+path))
	}
	return append(urls, originURL(u, "/.well-known/oauth-protected-resource"))
}

// coversResource reports whether resource, the identifier a protected resource metadata document is for, is the MCP
// server at u or a resource containing it: same scheme and host, and a path that is u's or one of its parents.
func coversResource(resource string, u *url.URL) bool {
	r, err := url.Parse(resource)
	if err != nil || !strings.EqualFold(r.Scheme, u.Scheme) || !strings.EqualFold(r.Host, u.Host) {
		return false
	}
	rp, up := strings.TrimSuffix(r.Path, "/"), strings.TrimSuffix(u.Path, "/")
	return rp == "" || rp == up || strings.HasPrefix(up, rp+"/")
}

// authorizationServerMeta reads the metadata of the authorization server issuer: at the RFC 8414 address (the
// well-known segment between host and the issuer's path), then the OpenID Connect discovery address, then OpenID
// Connect discovery the RFC 8414 way. Only metadata naming the same issuer counts.
func authorizationServerMeta(ctx context.Context, client *http.Client, issuer string) (*OAuthMeta, error) {
	i, err := url.Parse(strings.TrimSuffix(issuer, "/"))
	if err != nil || i.Host == "" {
		return nil, fmt.Errorf("not a URL")
	}
	candidates := []string{
		originURL(i, "/.well-known/oauth-authorization-server"+i.Path),
		strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration",
		originURL(i, "/.well-known/openid-configuration"+i.Path),
	}
	var why error
	for _, candidate := range candidates {
		var meta OAuthMeta
		err := getJSON(ctx, client, candidate, &meta)
		switch {
		case err != nil:
			why = moreTelling(why, err)
		case meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "":
			why = moreTelling(why, fmt.Errorf("%s has no authorization or token endpoint", candidate))
		case strings.TrimSuffix(meta.Issuer, "/") != strings.TrimSuffix(issuer, "/"):
			why = moreTelling(why, fmt.Errorf("%s is for another issuer (%s)", candidate, meta.Issuer))
		default:
			return &meta, nil
		}
	}
	return nil, why
}

// originURL is path on u's scheme and host, without u's query.
func originURL(u *url.URL, path string) string {
	probe := url.URL{Scheme: u.Scheme, Host: u.Host, Path: path}
	return probe.String()
}

// notPublishedError is a metadata address that answered without a document (any status but 200).
type notPublishedError struct {
	url    string
	status string
}

func (e *notPublishedError) Error() string { return e.url + ": " + e.status }

// getJSON reads the JSON document at target into v: nil when it answered 200 with one, else why not - a
// *notPublishedError for another status, or the request's own error (such as an untrusted certificate).
func getJSON(ctx context.Context, client *http.Client, target string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &notPublishedError{url: target, status: resp.Status}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes)).Decode(v); err != nil {
		return fmt.Errorf("%s: %w", target, err)
	}
	return nil
}

// RegisterOAuthClient performs RFC 7591 dynamic client registration.
// Returns the issued client_id.
func RegisterOAuthClient(ctx context.Context, registrationEndpoint, clientName, redirectURI string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                clientName,
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var result struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.ClientID == "" {
		return "", fmt.Errorf("dynamic client registration failed (status %d)", resp.StatusCode)
	}
	return result.ClientID, nil
}

// OAuth2Auth implements OAuth2 authentication backed by an oauth2.TokenSource.
// Supports both client credentials (machine-to-machine) and refresh token
// (user-delegated, e.g. PKCE) flows.
type OAuth2Auth struct {
	source oauth2.TokenSource
	token  *oauth2.Token
	mu     sync.RWMutex
}

// NewOAuth2Auth creates an OAuth2 provider using the client credentials flow.
func NewOAuth2Auth(clientID, clientSecret, tokenURL string, scopes []string) *OAuth2Auth {
	cfg := &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     tokenURL,
		Scopes:       scopes,
		AuthStyle:    oauth2.AuthStyleInHeader,
	}
	return &OAuth2Auth{
		source: cfg.TokenSource(context.Background()),
	}
}

// NewOAuth2RefreshTokenAuth creates an OAuth2 provider from an existing
// access + refresh token pair (e.g. obtained via browser-based PKCE flow).
// clientID is the dynamically registered client ID. accessToken may be empty.
func NewOAuth2RefreshTokenAuth(tokenURL, clientID, accessToken, refreshToken string) *OAuth2Auth {
	cfg := &oauth2.Config{
		ClientID: clientID,
		Endpoint: oauth2.Endpoint{TokenURL: tokenURL, AuthStyle: oauth2.AuthStyleInParams},
	}
	initial := &oauth2.Token{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}
	return &OAuth2Auth{
		source: oauth2.ReuseTokenSource(initial, cfg.TokenSource(context.Background(), initial)),
	}
}

func (o *OAuth2Auth) GetAuthHeader() (string, error) {
	o.mu.RLock()
	token := o.token
	valid := token != nil && token.Valid()
	o.mu.RUnlock()

	if !valid {
		o.mu.Lock()
		if o.token == nil || !o.token.Valid() {
			t, err := o.source.Token()
			if err != nil {
				o.mu.Unlock()
				return "", fmt.Errorf("failed to get oauth2 token: %w", err)
			}
			o.token = t
		}
		token = o.token
		o.mu.Unlock()
	}

	return fmt.Sprintf("Bearer %s", token.AccessToken), nil
}
