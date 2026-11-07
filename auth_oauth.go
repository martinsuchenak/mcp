package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// OAuthMeta holds the OAuth2 server metadata discovered via RFC 8414.
type OAuthMeta struct {
	Issuer                string `json:"issuer,omitempty"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`

	// ScopesSupported is the authorization server's scopes_supported.
	ScopesSupported []string `json:"scopes_supported,omitempty"`

	// CodeChallengeMethodsSupported is the authorization server's code_challenge_methods_supported. Discovery only
	// returns metadata that includes S256: the MCP authorization spec requires PKCE with S256.
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported,omitempty"`

	// ClientIDMetadataDocumentSupported reports that the authorization server accepts an HTTPS URL as client_id (a
	// Client ID Metadata Document), so a client may skip dynamic registration.
	ClientIDMetadataDocumentSupported bool `json:"client_id_metadata_document_supported,omitempty"`

	// AuthorizationResponseIssParameterSupported reports that the authorization server returns iss in the
	// authorization response (RFC 9207), which the client should then check against Issuer.
	AuthorizationResponseIssParameterSupported bool `json:"authorization_response_iss_parameter_supported,omitempty"`

	// Resource is the MCP server's resource identifier, to send as the resource parameter (RFC 8707) in both the
	// authorization request and the token request, so the token is issued for this server: from its protected
	// resource metadata (RFC 9728), or the server's own URL when it is its own authorization server (legacyOAuthMeta).
	Resource string `json:"-"`

	// ResourceScopesSupported is scopes_supported from the MCP server's protected resource metadata.
	ResourceScopesSupported []string `json:"-"`

	// ChallengeScope is the scope of the MCP server's 401 Bearer challenge: when set, the scopes to ask for.
	ChallengeScope string `json:"-"`
}

// ErrPKCENotSupported is returned by discovery, wrapped with the authorization server's issuer, when the authorization
// server does not advertise PKCE with S256: MCP clients must then refuse to proceed.
var ErrPKCENotSupported = errors.New("does not advertise PKCE S256 (code_challenge_methods_supported)")

// maxMetadataBytes bounds a metadata document read during discovery.
const maxMetadataBytes = 1 << 20

// DiscoverOAuthMeta fetches OAuth2 server metadata for the MCP server at serverURL, with http.DefaultClient. See
// DiscoverOAuthMetaWithClient.
func DiscoverOAuthMeta(ctx context.Context, serverURL string) (*OAuthMeta, error) {
	return DiscoverOAuthMetaWithClient(ctx, http.DefaultClient, serverURL)
}

// DiscoverOAuthMetaWithClient fetches OAuth2 server metadata for the MCP server at serverURL: from the authorization
// server its protected resource metadata names (RFC 9728, the MCP authorization spec), with the resource to ask a
// token for in Resource; only when the server publishes no protected resource metadata for itself, from the root of
// its host, for a server that is its own authorization server (legacyOAuthMeta). Metadata whose URLs are not https
// (or http on a loopback host) is not used, and an authorization server that does not advertise PKCE S256 fails
// discovery with ErrPKCENotSupported.
func DiscoverOAuthMetaWithClient(ctx context.Context, client *http.Client, serverURL string) (*OAuthMeta, error) {
	u, err := url.Parse(strings.TrimSuffix(serverURL, "/"))
	if err != nil {
		return nil, err
	}
	meta, found, why := protectedResourceOAuthMeta(ctx, client, u)
	if meta != nil {
		return meta, nil
	}
	if !found {
		legacy, err := legacyOAuthMeta(ctx, client, u)
		if legacy != nil {
			return legacy, nil
		}
		why = moreTelling(why, err)
	}
	if why != nil {
		return nil, fmt.Errorf("could not discover OAuth metadata from %s: %w", serverURL, why)
	}
	return nil, fmt.Errorf("could not discover OAuth metadata from %s", serverURL)
}

// legacyOAuthMeta finds an MCP server that is its own authorization server, publishing its metadata at the root of its
// host (the MCP 2025-03-26 authorization spec). Tried only when the server publishes no protected resource metadata;
// only metadata whose issuer is exactly the server's origin counts, and its Resource is the server's URL.
//
// LEGACY(fortix-mcp-own-oauth): Freedom 3 instances that do not sign in through their identity provider (IdP) are
// served by a fortix-mcp that is its own authorization server, found here. Remove this, and its call, once every
// Freedom 3 instance signs in only through the IdP: those are found by protectedResourceOAuthMeta.
func legacyOAuthMeta(ctx context.Context, client *http.Client, u *url.URL) (*OAuthMeta, error) {
	var why error
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"} {
		candidate := originURL(u, path)
		var meta OAuthMeta
		if err := getJSON(ctx, client, candidate, &meta); err != nil {
			why = moreTelling(why, err)
			continue
		}
		if err := checkOAuthMeta(&meta, originURL(u, ""), candidate); err != nil {
			why = moreTelling(why, err)
			continue
		}
		meta.Resource = originURL(u, strings.TrimSuffix(u.Path, "/"))
		return &meta, nil
	}
	return nil, why
}

// protectedResourceOAuthMeta finds the authorization server an MCP server names in its protected resource metadata
// (RFC 9728): at the address its 401 challenge gives, else at the well-known address for its path, else for its host.
// A document whose resource is not this server is ignored (RFC 9728 section 3.3), as is an authorization server whose
// metadata names another issuer (RFC 8414 section 3.3). found reports whether a document for this server was read,
// whether or not its authorization server could be used. When none is found, the error says why when there is more to
// say than "not published": an authorization server that was named but could not be read, or a request that failed.
func protectedResourceOAuthMeta(ctx context.Context, client *http.Client, u *url.URL) (*OAuthMeta, bool, error) {
	metadataURLs, challengeScope, why := protectedResourceMetadataURLs(ctx, client, u)
	found := false
	for _, metadataURL := range metadataURLs {
		var prm struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
			ScopesSupported      []string `json:"scopes_supported"`
		}
		if err := getJSON(ctx, client, metadataURL, &prm); err != nil {
			why = moreTelling(why, err)
			continue
		}
		if !coversResource(prm.Resource, u) {
			why = moreTelling(why, fmt.Errorf("%s is for another resource (%s)", metadataURL, prm.Resource))
			continue
		}
		found = true
		for _, issuer := range prm.AuthorizationServers {
			meta, err := authorizationServerMeta(ctx, client, issuer)
			if meta != nil {
				meta.Resource = prm.Resource
				meta.ResourceScopesSupported = prm.ScopesSupported
				meta.ChallengeScope = challengeScope
				return meta, true, nil
			}
			if errors.Is(err, ErrPKCENotSupported) {
				why = err
			} else {
				why = fmt.Errorf("authorization server %s: %w", issuer, err)
			}
		}
	}
	return nil, found, why
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
// resource_metadata of the server's 401 Bearer challenge, then the well-known address for its path, then for its
// host. The challenge comes from an unauthenticated GET or, when that is not answered 401 (a server that only takes
// POST), an unauthenticated JSON-RPC ping. scope is the scope of the challenge naming the metadata; why says why a
// resource_metadata was not used (one that is not https is refused).
func protectedResourceMetadataURLs(ctx context.Context, client *http.Client, u *url.URL) (urls []string, scope string, why error) {
	challenges := unauthorizedChallenges(ctx, client, u, http.MethodGet)
	if challenges == nil {
		challenges = unauthorizedChallenges(ctx, client, u, http.MethodPost)
	}
	for _, params := range bearerChallenges(challenges) {
		raw, ok := params["resource_metadata"]
		if !ok {
			continue
		}
		ref, err := u.Parse(raw)
		if err == nil {
			err = checkFetchURL(ref)
		}
		if err != nil {
			why = moreTelling(why, fmt.Errorf("resource_metadata %q: %w", raw, err))
			continue
		}
		urls = append(urls, ref.String())
		if scope == "" {
			scope = params["scope"]
		}
	}
	if path := strings.TrimSuffix(u.Path, "/"); path != "" {
		urls = append(urls, originURL(u, "/.well-known/oauth-protected-resource"+path))
	}
	return append(urls, originURL(u, "/.well-known/oauth-protected-resource")), scope, why
}

// unauthorizedChallenges sends u an unauthenticated request (for POST, a JSON-RPC ping) and returns the
// WWW-Authenticate values of a 401 answer, or nil for any other answer.
func unauthorizedChallenges(ctx context.Context, client *http.Client, u *url.URL, method string) []string {
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"jsonrpc":"2.0","id":0,"method":"ping"}`)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		return nil
	}
	return append([]string{}, resp.Header.Values("WWW-Authenticate")...)
}

// bearerChallenges returns the auth-params of each Bearer challenge in WWW-Authenticate header values (RFC 9110
// section 11.6.1), keyed by lower-cased name, with token or quoted-string values. Other schemes are skipped.
func bearerChallenges(headers []string) []map[string]string {
	var out []map[string]string
	for _, h := range headers {
		var params map[string]string // the current challenge's; nil when it is not Bearer
		s := h
		for {
			s = strings.TrimLeft(s, " \t,")
			if s == "" {
				break
			}
			name, rest := readToken(s)
			if name == "" {
				break // malformed: give up on the rest of this value
			}
			afterName := strings.TrimLeft(rest, " \t")
			if !strings.HasPrefix(afterName, "=") {
				// A token not followed by "=" starts a new challenge: it is the scheme.
				params = nil
				if strings.EqualFold(name, "Bearer") {
					params = map[string]string{}
					out = append(out, params)
				}
				s = rest
				continue
			}
			value, rest := readParamValue(strings.TrimLeft(afterName[1:], " \t"))
			s = rest
			if params != nil {
				if key := strings.ToLower(name); params[key] == "" {
					params[key] = value
				}
			}
		}
	}
	return out
}

// readToken splits s after its leading token (a run of RFC 9110 tchar).
func readToken(s string) (string, string) {
	i := 0
	for i < len(s) && isTokenChar(s[i]) {
		i++
	}
	return s[:i], s[i:]
}

// isTokenChar reports whether c is an RFC 9110 tchar.
func isTokenChar(c byte) bool {
	if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
		return true
	}
	return strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

// readParamValue splits s after an auth-param value: a quoted string (unescaped) or, leniently, anything up to the
// next comma or whitespace.
func readParamValue(s string) (string, string) {
	if !strings.HasPrefix(s, `"`) {
		if i := strings.IndexAny(s, ", \t"); i >= 0 {
			return s[:i], s[i:]
		}
		return s, ""
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		case '"':
			return b.String(), s[i+1:]
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), "" // unterminated: take the rest
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

// authorizationServerMeta reads the metadata of the authorization server issuer, in the order the MCP authorization
// spec gives: RFC 8414 (the well-known segment inserted between host and the issuer's path), then OpenID Connect
// discovery inserted the same way, then, for an issuer with a path, OpenID Connect discovery appended to the issuer.
// Only metadata naming exactly this issuer, and passing checkOAuthMeta, counts.
func authorizationServerMeta(ctx context.Context, client *http.Client, issuer string) (*OAuthMeta, error) {
	trimmed := strings.TrimSuffix(issuer, "/")
	i, err := url.Parse(trimmed)
	if err != nil || i.Host == "" {
		return nil, fmt.Errorf("not a URL")
	}
	if err := checkFetchURL(i); err != nil {
		return nil, err
	}
	candidates := []string{
		originURL(i, "/.well-known/oauth-authorization-server"+i.Path),
		originURL(i, "/.well-known/openid-configuration"+i.Path),
	}
	if i.Path != "" {
		candidates = append(candidates, trimmed+"/.well-known/openid-configuration")
	}
	var why error
	for _, candidate := range candidates {
		var meta OAuthMeta
		if err := getJSON(ctx, client, candidate, &meta); err != nil {
			why = moreTelling(why, err)
			continue
		}
		if err := checkOAuthMeta(&meta, issuer, candidate); err != nil {
			why = moreTelling(why, err)
			continue
		}
		return &meta, nil
	}
	return nil, why
}

// checkOAuthMeta checks authorization server metadata read from candidate: it has authorization and token endpoints,
// names exactly issuer (RFC 8414 section 3.3), passes validateOAuthMeta, and advertises PKCE S256.
func checkOAuthMeta(meta *OAuthMeta, issuer, candidate string) error {
	switch {
	case meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "":
		return fmt.Errorf("%s has no authorization or token endpoint", candidate)
	case meta.Issuer != issuer:
		return fmt.Errorf("%s is for another issuer (%s)", candidate, meta.Issuer)
	}
	if err := validateOAuthMeta(meta); err != nil {
		return fmt.Errorf("%s: %w", candidate, err)
	}
	for _, method := range meta.CodeChallengeMethodsSupported {
		if method == "S256" {
			return nil
		}
	}
	return fmt.Errorf("authorization server %s %w", issuer, ErrPKCENotSupported)
}

// validateOAuthMeta checks that the issuer and the endpoints a client sends the user or its credentials to are https
// URLs, or http on a loopback host (local development); the registration endpoint only when there is one.
func validateOAuthMeta(meta *OAuthMeta) error {
	fields := []struct{ name, value string }{
		{"issuer", meta.Issuer},
		{"authorization_endpoint", meta.AuthorizationEndpoint},
		{"token_endpoint", meta.TokenEndpoint},
	}
	if meta.RegistrationEndpoint != "" {
		fields = append(fields, struct{ name, value string }{"registration_endpoint", meta.RegistrationEndpoint})
	}
	for _, f := range fields {
		u, err := url.Parse(f.value)
		if err == nil {
			err = checkFetchURL(u)
		}
		if err != nil {
			return fmt.Errorf("%s %q: %w", f.name, f.value, err)
		}
	}
	return nil
}

// checkFetchURL accepts an absolute https URL, or an http one on a loopback host; any other scheme, or plain http to
// another host, is refused.
func checkFetchURL(u *url.URL) error {
	switch {
	case u.Host == "":
		return errors.New("not an absolute URL")
	case strings.EqualFold(u.Scheme, "https"):
		return nil
	case strings.EqualFold(u.Scheme, "http") && isLoopbackHost(u.Hostname()):
		return nil
	}
	return errors.New("not an https URL")
}

// isLoopbackHost reports whether host is localhost or a loopback IP address (127.0.0.0/8, ::1).
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// RegisterOption configures RegisterOAuthClient.
type RegisterOption func(*registerOptions)

type registerOptions struct {
	applicationType string
}

// WithApplicationType sets the registration's application_type ("native" or "web"), overriding the one chosen from
// the redirect URI.
func WithApplicationType(applicationType string) RegisterOption {
	return func(o *registerOptions) { o.applicationType = applicationType }
}

// RegisterOAuthClient performs RFC 7591 dynamic client registration with http.DefaultClient. See
// RegisterOAuthClientWithClient.
func RegisterOAuthClient(ctx context.Context, registrationEndpoint, clientName, redirectURI string, opts ...RegisterOption) (string, error) {
	return RegisterOAuthClientWithClient(ctx, http.DefaultClient, registrationEndpoint, clientName, redirectURI, opts...)
}

// RegisterOAuthClientWithClient performs RFC 7591 dynamic client registration of a public client using PKCE, and
// returns the issued client_id. Its application_type is "native" for a loopback or custom-scheme redirect URI, else
// "web", unless WithApplicationType says otherwise.
func RegisterOAuthClientWithClient(ctx context.Context, client *http.Client, registrationEndpoint, clientName, redirectURI string, opts ...RegisterOption) (string, error) {
	o := registerOptions{applicationType: applicationTypeFor(redirectURI)}
	for _, opt := range opts {
		opt(&o)
	}
	body, _ := json.Marshal(map[string]any{
		"client_name":                clientName,
		"redirect_uris":              []string{redirectURI},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"application_type":           o.applicationType,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var failure struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes)).Decode(&failure)
		if failure.Error == "" {
			return "", fmt.Errorf("dynamic client registration failed (status %d)", resp.StatusCode)
		}
		if failure.ErrorDescription == "" {
			return "", fmt.Errorf("dynamic client registration failed (status %d): %s", resp.StatusCode, failure.Error)
		}
		return "", fmt.Errorf("dynamic client registration failed (status %d): %s: %s", resp.StatusCode, failure.Error, failure.ErrorDescription)
	}
	var result struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes)).Decode(&result); err != nil || result.ClientID == "" {
		return "", fmt.Errorf("dynamic client registration failed (status %d): no client_id", resp.StatusCode)
	}
	return result.ClientID, nil
}

// applicationTypeFor is the application_type for a client redirecting to redirectURI: "native" for a loopback host or
// a custom (non-http) scheme, else "web".
func applicationTypeFor(redirectURI string) string {
	u, err := url.Parse(redirectURI)
	if err != nil {
		return "web"
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return "native"
	}
	if isLoopbackHost(u.Hostname()) {
		return "native"
	}
	return "web"
}

// OAuth2Option configures NewOAuth2Auth and NewOAuth2RefreshTokenAuth.
type OAuth2Option func(*oauth2Options)

type oauth2Options struct {
	resource string
}

// WithResource sends resource as the resource parameter (RFC 8707) in token requests, so tokens are issued for that
// MCP server (OAuthMeta.Resource), as the MCP authorization spec requires.
func WithResource(resource string) OAuth2Option {
	return func(o *oauth2Options) { o.resource = resource }
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
func NewOAuth2Auth(clientID, clientSecret, tokenURL string, scopes []string, opts ...OAuth2Option) *OAuth2Auth {
	var o oauth2Options
	for _, opt := range opts {
		opt(&o)
	}
	cfg := &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     tokenURL,
		Scopes:       scopes,
		AuthStyle:    oauth2.AuthStyleInHeader,
	}
	if o.resource != "" {
		cfg.EndpointParams = url.Values{"resource": {o.resource}}
	}
	return &OAuth2Auth{
		source: cfg.TokenSource(context.Background()),
	}
}

// NewOAuth2RefreshTokenAuth creates an OAuth2 provider from an existing
// access + refresh token pair (e.g. obtained via browser-based PKCE flow).
// clientID is the dynamically registered client ID. accessToken may be empty.
func NewOAuth2RefreshTokenAuth(tokenURL, clientID, accessToken, refreshToken string, opts ...OAuth2Option) *OAuth2Auth {
	var o oauth2Options
	for _, opt := range opts {
		opt(&o)
	}
	initial := &oauth2.Token{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}
	if o.resource != "" {
		source := &resourceRefreshTokenSource{
			client:       http.DefaultClient,
			tokenURL:     tokenURL,
			clientID:     clientID,
			resource:     o.resource,
			refreshToken: refreshToken,
		}
		return &OAuth2Auth{source: oauth2.ReuseTokenSource(initial, source)}
	}
	cfg := &oauth2.Config{
		ClientID: clientID,
		Endpoint: oauth2.Endpoint{TokenURL: tokenURL, AuthStyle: oauth2.AuthStyleInParams},
	}
	return &OAuth2Auth{
		source: oauth2.ReuseTokenSource(initial, cfg.TokenSource(context.Background(), initial)),
	}
}

// resourceRefreshTokenSource refreshes a token with the resource parameter, which golang.org/x/oauth2's refresh
// cannot send: grant_type=refresh_token with client_id in the parameters, keeping the refresh token when the response
// does not rotate it.
type resourceRefreshTokenSource struct {
	client   *http.Client
	tokenURL string
	clientID string
	resource string

	mu           sync.Mutex
	refreshToken string
}

func (s *resourceRefreshTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshToken == "" {
		return nil, errors.New("oauth2: token expired and refresh token is not set")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {s.refreshToken},
		"client_id":     {s.clientID},
		"resource":      {s.resource},
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth2: cannot fetch token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return nil, fmt.Errorf("oauth2: cannot fetch token: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var failure struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &failure)
		return nil, &oauth2.RetrieveError{Response: resp, Body: body, ErrorCode: failure.Error, ErrorDescription: failure.ErrorDescription}
	}
	var tr struct {
		AccessToken  string      `json:"access_token"`
		TokenType    string      `json:"token_type"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("oauth2: cannot parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, errors.New("oauth2: server response missing access_token")
	}
	if tr.RefreshToken != "" {
		s.refreshToken = tr.RefreshToken
	}
	token := &oauth2.Token{AccessToken: tr.AccessToken, TokenType: tr.TokenType, RefreshToken: s.refreshToken}
	if secs, err := tr.ExpiresIn.Int64(); err == nil && secs > 0 {
		token.Expiry = time.Now().Add(time.Duration(secs) * time.Second)
	}
	return token, nil
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
