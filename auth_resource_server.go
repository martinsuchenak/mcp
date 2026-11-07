package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuth 2.1 resource-server helpers: what the MCP authorization spec requires
// of a protected MCP server, as opt-in pieces an application mounts in front
// of Server.HandleRequest.
//
//   - ResourceServer.MetadataHandler serves the RFC 9728 protected resource
//     metadata document (MCP servers MUST implement it).
//   - ResourceServer.Protect is middleware that requires a bearer token,
//     validates it through the application's TokenValidator, checks that
//     the token was issued for this server (audience: a MUST), answers a
//     missing or invalid token with 401 and a WWW-Authenticate challenge
//     naming the metadata document (the discovery mechanism the spec
//     requires), and answers a token lacking a required scope with 403 and
//     an insufficient_scope challenge.
//   - TokenInfoFromContext gives tool handlers the caller's identity and
//     scopes.
//
// The token format is the application's concern: an identity provider's
// JWT verified against its JWKS, an introspection call, or an opaque lookup.
// The inbound token is never forwarded anywhere by this library (the spec
// forbids token passthrough) and never written to logs or responses.

// TokenInfo is what a TokenValidator reports about an accepted access token.
type TokenInfo struct {
	// Subject identifies the caller (the token's sub claim, typically).
	Subject string
	// Audience lists the resources the token was issued for (aud). Protect
	// requires one of them to be this server's resource identifier.
	Audience []string
	// Scopes are the token's granted scopes.
	Scopes []string
	// Expiry is when the token expires; zero when unknown.
	Expiry time.Time
	// Claims holds any further claims the validator wants handlers to see.
	Claims map[string]any
	// IssuedForResource marks a token that the validator itself issued or
	// configured for this server (StaticTokenValidator), so it carries no
	// audience to check. A token from an identity provider must instead
	// name this server in Audience, or it is refused.
	IssuedForResource bool
}

// HasScope reports whether the token grants scope.
func (t *TokenInfo) HasScope(scope string) bool {
	if t == nil {
		return false
	}
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// TokenValidator checks a bearer token and reports what it grants. Return
// an error for a token that is invalid, expired, revoked or malformed; the
// error's text is not sent to the client. Audience and scope are checked by
// Protect from the returned TokenInfo, so a validator need not repeat them.
// The request is available for multi-tenant servers that pick a validator
// or issuer from the host; most validators ignore it.
type TokenValidator func(r *http.Request, token string) (*TokenInfo, error)

// StaticTokenValidator accepts exactly the given tokens (compared in
// constant time), each granting the given TokenInfo. It suits a server
// protected by a configured secret rather than an identity provider; such a
// token is issued for this server by construction, so an empty Audience in
// the TokenInfo is treated as this resource.
func StaticTokenValidator(tokens map[string]*TokenInfo) TokenValidator {
	return func(r *http.Request, token string) (*TokenInfo, error) {
		for want, info := range tokens {
			if subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1 {
				out := TokenInfo{IssuedForResource: true}
				if info != nil {
					out = *info
					out.IssuedForResource = len(info.Audience) == 0 // a configured audience is checked
				}
				return &out, nil
			}
		}
		return nil, ErrInvalidToken
	}
}

// ResourceServer describes an MCP server as an OAuth 2.1 protected resource.
type ResourceServer struct {
	// Resource is this server's canonical resource identifier (RFC 8707):
	// the URL clients reach the MCP endpoint at, such as
	// "https://mcp.example.com/mcp" — lowercase scheme and host, no query,
	// no fragment, no trailing slash. Required unless ResourceFunc is set.
	// It is the value clients send as the resource parameter and must find
	// in tokens' audience.
	Resource string

	// ResourceFunc, when set, derives the resource identifier from the
	// request (a multi-tenant server whose identity depends on the host).
	ResourceFunc func(r *http.Request) string

	// AuthorizationServers lists the issuer identifiers of the
	// authorization servers (identity providers) that issue tokens for this
	// resource. Required (the MCP spec requires at least one in the
	// metadata) unless AuthorizationServersFunc is set.
	AuthorizationServers []string

	// AuthorizationServersFunc, when set, derives the authorization servers
	// from the request (per tenant, say).
	AuthorizationServersFunc func(r *http.Request) []string

	// ScopesSupported is advertised in the metadata and, when RequiredScopes
	// is empty, in the 401 challenge's scope parameter: the scopes a client
	// should request (the spec's scope selection strategy uses it).
	ScopesSupported []string

	// RequiredScopes are the scopes every request must carry. A token
	// lacking one is answered 403 insufficient_scope naming them. Leave
	// empty to accept any valid token and decide per tool with
	// TokenInfoFromContext.
	RequiredScopes []string

	// ResourceName is a human-readable name for the metadata (recommended).
	ResourceName string

	// Audiences lists the audience values accepted in a token in addition
	// to Resource itself (an identity provider may issue aud values of its
	// own choosing, such as an API identifier). Leave empty to accept only
	// Resource.
	Audiences []string

	// MetadataURL overrides the metadata document URL advertised in
	// challenges. By default it is derived from the resource (RFC 9728
	// section 3): the well-known path with the resource's path appended.
	MetadataURL string

	// MetadataURLFunc, when set, derives that URL from the request.
	MetadataURLFunc func(r *http.Request) string

	// Extra holds further metadata fields to publish verbatim (for example
	// "bearer_methods_supported", "resource_documentation").
	Extra map[string]any

	// ValidateToken checks each bearer token. Required by Protect.
	ValidateToken TokenValidator

	// ClockSkew is tolerated when checking a token's Expiry. Default 60s.
	ClockSkew time.Duration

	// ServeRootMetadata makes Mount also serve the document at the plain
	// "/.well-known/oauth-protected-resource" path for a resource with a
	// path (clients that ignore the path-inserted form look there). Off by
	// default: RFC 9728 clients compare the document's resource with the
	// URL they asked about, and a mux can only hold one root document.
	ServeRootMetadata bool
}

// ErrNoToken, ErrMalformedToken, ErrInvalidToken, ErrWrongAudience and
// ErrInsufficientScope classify a rejected request; Protect maps them to
// the spec's responses.
var (
	ErrNoToken           = errors.New("no bearer token")
	ErrMalformedToken    = errors.New("malformed bearer credentials")
	ErrInvalidToken      = errors.New("invalid token")
	ErrWrongAudience     = errors.New("token not issued for this resource")
	ErrInsufficientScope = errors.New("insufficient scope")
)

// ProtectedResourceMetadata is the RFC 9728 document MetadataHandler serves.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers,omitempty"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported,omitempty"`
	ResourceName           string   `json:"resource_name,omitempty"`
}

// resource is the resource identifier for r.
func (rs *ResourceServer) resource(r *http.Request) string {
	if rs.ResourceFunc != nil && r != nil {
		if v := rs.ResourceFunc(r); v != "" {
			return v
		}
	}
	return rs.Resource
}

func (rs *ResourceServer) authorizationServers(r *http.Request) []string {
	if rs.AuthorizationServersFunc != nil && r != nil {
		if v := rs.AuthorizationServersFunc(r); len(v) > 0 {
			return v
		}
	}
	return rs.AuthorizationServers
}

// Metadata is the protected resource metadata this server publishes for
// the request (nil for the static configuration).
func (rs *ResourceServer) Metadata(r *http.Request) map[string]any {
	doc := map[string]any{
		"resource":                 rs.resource(r),
		"bearer_methods_supported": []string{"header"},
	}
	if as := rs.authorizationServers(r); len(as) > 0 {
		doc["authorization_servers"] = as
	}
	if len(rs.ScopesSupported) > 0 {
		doc["scopes_supported"] = rs.ScopesSupported
	}
	if rs.ResourceName != "" {
		doc["resource_name"] = rs.ResourceName
	}
	for k, v := range rs.Extra {
		doc[k] = v
	}
	return doc
}

// MetadataPath is the path the metadata document is served at (RFC 9728
// section 3): "/.well-known/oauth-protected-resource" followed by the
// resource's path, with any trailing slash of the resource removed first.
func (rs *ResourceServer) MetadataPath() string {
	return metadataPathFor(rs.Resource)
}

const protectedResourceWellKnown = "/.well-known/oauth-protected-resource"

func metadataPathFor(resource string) string {
	path := ""
	if u, err := url.Parse(resource); err == nil {
		path = strings.TrimSuffix(u.Path, "/")
	}
	return protectedResourceWellKnown + path
}

// metadataURL is the absolute URL named in challenges for the request.
func (rs *ResourceServer) metadataURL(r *http.Request) string {
	if rs.MetadataURLFunc != nil && r != nil {
		if v := rs.MetadataURLFunc(r); v != "" {
			return v
		}
	}
	if rs.MetadataURL != "" {
		return rs.MetadataURL
	}
	u, err := url.Parse(rs.resource(r))
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: metadataPathFor(rs.resource(r))}).String()
}

// MetadataHandler serves the metadata document as application/json. Mount
// it at MetadataPath (and, for a resource at the host root, that is the
// plain well-known path); see Mount.
func (rs *ResourceServer) MetadataHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodHead {
			return
		}
		_ = json.NewEncoder(w).Encode(rs.Metadata(r))
	})
}

// Mount registers the metadata document on mux — at MetadataPath and, for
// a resource with a path, at the plain well-known path too, since the MCP
// spec lets clients look in either place — and the MCP endpoint (the path
// of Resource) with handler wrapped by Protect:
//
//	rs := &mcp.ResourceServer{Resource: "https://mcp.example.com/mcp", ...}
//	rs.Mount(mux, http.HandlerFunc(server.HandleRequest))
func (rs *ResourceServer) Mount(mux *http.ServeMux, handler http.Handler) {
	if rs.ResourceFunc != nil || rs.MetadataURLFunc != nil {
		// Per-request resources: the advertised path varies, so serve the
		// whole well-known prefix.
		mux.Handle(protectedResourceWellKnown+"/", rs.MetadataHandler())
		mux.Handle(protectedResourceWellKnown, rs.MetadataHandler())
	} else {
		mux.Handle(rs.MetadataPath(), rs.MetadataHandler())
		if rs.MetadataPath() != protectedResourceWellKnown && rs.ServeRootMetadata {
			mux.Handle(protectedResourceWellKnown, rs.MetadataHandler())
		}
		// A configured MetadataURL on this host is advertised in challenges,
		// so it is served too.
		if mu, err := url.Parse(rs.MetadataURL); err == nil && rs.MetadataURL != "" && mu.Path != "" && mu.Path != rs.MetadataPath() && mu.Path != protectedResourceWellKnown {
			if ru, err := url.Parse(rs.Resource); err == nil && strings.EqualFold(ru.Host, mu.Host) {
				mux.Handle(mu.Path, rs.MetadataHandler())
			}
		}
	}
	path := "/"
	if u, err := url.Parse(rs.Resource); err == nil && u.Path != "" {
		path = u.Path
	}
	mux.Handle(path, rs.Protect(handler))
}

// Protect wraps handler so only requests carrying a valid bearer token for
// this resource reach it. CORS preflights (OPTIONS) pass through, since a
// browser sends them without credentials. The accepted token's TokenInfo is
// placed in the request context for TokenInfoFromContext.
func (rs *ResourceServer) Protect(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			handler.ServeHTTP(w, r)
			return
		}
		info, err := rs.Authenticate(r)
		if err != nil {
			rs.Reject(w, r, err)
			return
		}
		handler.ServeHTTP(w, r.WithContext(WithTokenInfo(r.Context(), info)))
	})
}

// Authenticate checks r's bearer token and reports what it grants, or why
// the request is refused: ErrNoToken, ErrInvalidToken (the validator
// rejected it), ErrWrongAudience, or ErrInsufficientScope. Protect calls
// it; an application with its own routing can call it directly and answer
// with Reject.
func (rs *ResourceServer) Authenticate(r *http.Request) (*TokenInfo, error) {
	token, present, ok := bearerToken(r.Header.Get("Authorization"))
	if !present {
		return nil, ErrNoToken
	}
	if !ok {
		return nil, ErrMalformedToken
	}
	if rs.ValidateToken == nil {
		return nil, ErrInvalidToken
	}
	info, err := rs.ValidateToken(r, token)
	if err != nil || info == nil {
		return nil, ErrInvalidToken
	}
	skew := rs.ClockSkew
	if skew == 0 {
		skew = time.Minute
	}
	if !info.Expiry.IsZero() && time.Now().After(info.Expiry.Add(skew)) {
		return nil, ErrInvalidToken
	}
	// The spec's MUST: a token is accepted only if it was issued for this
	// server. A validator that configured the token for this server says so
	// with IssuedForResource; any other token must name this resource in
	// its audience — one with no audience at all is refused.
	if !info.IssuedForResource && !rs.audienceAccepted(r, info.Audience) {
		return nil, ErrWrongAudience
	}
	for _, scope := range rs.RequiredScopes {
		if !info.HasScope(scope) {
			return nil, ErrInsufficientScope
		}
	}
	return info, nil
}

// Reject answers a request Authenticate refused, as the MCP authorization
// spec and RFC 6750 require: 401 with a Bearer challenge naming the
// resource metadata (and error="invalid_token" when a token was presented),
// or 403 with error="insufficient_scope" naming the scopes needed. Any
// other error is treated as an invalid token. An application with its own
// authentication can call it alone to produce spec-shaped refusals.
func (rs *ResourceServer) Reject(w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case errors.Is(err, ErrInsufficientScope):
		rs.RejectScope(w, r, rs.RequiredScopes)
	case errors.Is(err, ErrNoToken):
		w.Header().Set("WWW-Authenticate", rs.challenge(r, "", "", rs.challengeScopes()))
		writeAuthError(w, http.StatusUnauthorized, "unauthorized", "a bearer token is required")
	case errors.Is(err, ErrMalformedToken):
		const desc = "the Authorization header is malformed"
		w.Header().Set("WWW-Authenticate", rs.challenge(r, "invalid_request", desc, rs.challengeScopes()))
		writeAuthError(w, http.StatusBadRequest, "invalid_request", desc)
	default:
		const desc = "the token is not valid for this resource"
		w.Header().Set("WWW-Authenticate", rs.challenge(r, "invalid_token", desc, rs.challengeScopes()))
		writeAuthError(w, http.StatusUnauthorized, "invalid_token", desc)
	}
}

// RejectScope answers 403 insufficient_scope naming the scopes the request
// needs (the spec's step-up flow reads them from the challenge). Tool
// handlers cannot write HTTP responses, so an application enforcing scopes
// per tool calls this from its own middleware before dispatch.
func (rs *ResourceServer) RejectScope(w http.ResponseWriter, r *http.Request, scopes []string) {
	const desc = "the token does not grant the scopes this request needs"
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("WWW-Authenticate", rs.challenge(r, "insufficient_scope", desc, scopes))
	writeAuthError(w, http.StatusForbidden, "insufficient_scope", desc)
}

func (rs *ResourceServer) challengeScopes() []string {
	if len(rs.RequiredScopes) > 0 {
		return rs.RequiredScopes
	}
	return rs.ScopesSupported
}

// challenge builds the Bearer challenge value: resource_metadata always,
// then scope and error/error_description when given.
func (rs *ResourceServer) challenge(r *http.Request, errCode, desc string, scopes []string) string {
	parts := []string{fmt.Sprintf("resource_metadata=%q", rs.metadataURL(r))}
	if valid := validScopeTokens(scopes); len(valid) > 0 {
		parts = append(parts, fmt.Sprintf("scope=%q", strings.Join(valid, " ")))
	}
	if errCode != "" {
		parts = append(parts, fmt.Sprintf("error=%q", errCode), fmt.Sprintf("error_description=%q", desc))
	}
	return "Bearer " + strings.Join(parts, ", ")
}

func writeAuthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

// audienceAccepted reports whether one of aud names this resource (or a
// configured alternative audience). Scheme and host compare
// case-insensitively; a trailing slash is ignored, as the spec recommends
// accepting for robustness.
func (rs *ResourceServer) audienceAccepted(r *http.Request, aud []string) bool {
	resource := rs.resource(r)
	for _, a := range aud {
		if sameResource(a, resource) {
			return true
		}
		for _, alt := range rs.Audiences {
			lower := strings.ToLower(alt)
			if a == alt || ((strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://")) && sameResource(a, alt)) {
				return true
			}
		}
	}
	return false
}

func sameResource(a, b string) bool {
	if a == "" || b == "" {
		return false // an unconfigured resource matches nothing
	}
	// A resource identifier has no query, fragment or userinfo (RFC 8707);
	// the raw text is checked so an empty "?" or "#" cannot slip through.
	if strings.ContainsAny(a, "?#@") {
		return false
	}
	ua, errA := url.Parse(strings.TrimSuffix(a, "/"))
	ub, errB := url.Parse(strings.TrimSuffix(b, "/"))
	if errA != nil || errB != nil || ua.Host == "" || ub.Host == "" {
		return a == b
	}
	// Paths compare in their escaped form: %2F is not a slash (RFC 3986).
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) && ua.EscapedPath() == ub.EscapedPath()
}

// bearerToken extracts the token of an "Authorization: Bearer ..." header:
// present reports a Bearer scheme, ok that its credentials are one
// well-formed b64token (RFC 6750 section 2.1).
func bearerToken(header string) (token string, present, ok bool) {
	if len(header) < 6 || !strings.EqualFold(header[:6], "Bearer") {
		return "", false, false
	}
	rest := header[6:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return "", false, false // another scheme, e.g. "BearerX"
	}
	token = strings.TrimSpace(rest)
	if token == "" || !isB64Token(token) {
		return "", true, false
	}
	return token, true, true
}

// isB64Token reports whether s matches RFC 6750's b64token grammar.
func isB64Token(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == '_', c == '~', c == '+', c == '/':
		case c == '=':
			if i == 0 {
				return false // at least one token character before padding
			}
			// padding only at the end
			for j := i; j < len(s); j++ {
				if s[j] != '=' {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	return true
}

type tokenInfoKey struct{}

// WithTokenInfo returns a context carrying info, as Protect does for the
// request it accepted. Use it from custom middleware that validates tokens
// another way.
func WithTokenInfo(ctx context.Context, info *TokenInfo) context.Context {
	return context.WithValue(ctx, tokenInfoKey{}, info)
}

// TokenInfoFromContext returns the accepted token's TokenInfo for a request
// that passed Protect (or carries WithTokenInfo), else nil. Tool handlers
// use it to see who is calling and which scopes they hold.
func TokenInfoFromContext(ctx context.Context) *TokenInfo {
	info, _ := ctx.Value(tokenInfoKey{}).(*TokenInfo)
	return info
}

// validScopeTokens keeps the scopes RFC 6749 allows in a scope-token
// (printable ASCII except space, double quote and backslash), so a
// challenge is always well-formed.
func validScopeTokens(scopes []string) []string {
	var out []string
	for _, s := range scopes {
		ok := s != ""
		for i := 0; i < len(s) && ok; i++ {
			c := s[i]
			ok = c == 0x21 || (c >= 0x23 && c <= 0x5B) || (c >= 0x5D && c <= 0x7E)
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}
