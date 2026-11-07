package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// clientEra records which protocol era a Client has detected its server
// speaks, once Initialize has run. eraUnknown only exists before the first
// Initialize call completes.
type clientEra int

const (
	eraUnknown clientEra = iota
	eraLegacy
	eraModern
)

// tryModernInitialize attempts the Modern-era (protocol revision 2026-07-28+)
// discovery probe described in the spec's backward-compatibility algorithm:
// send server/discover in the Modern shape and see whether the server
// understands it. Returns true only when Modern era was fully detected and
// applied (c.initialized, c.era, and c.protocolVersion are set); false means
// "could not confirm Modern support" for any reason (network error, non-JSON
// body, or a Legacy server's plain error) and the caller should fall back to
// the existing Legacy c.initializeLegacy, unchanged.
//
// A server that answers with a well-formed Modern protocol error — code
// [ErrorCodeUnsupportedProtocolVersion], naming the versions it does
// support in data.supported — has just proven it understands the Modern
// wire format; it isn't a Legacy server at all, it simply doesn't support
// [MCPProtocolVersionModern] specifically (e.g. it only speaks a newer
// revision this client build predates). Falling back to Legacy in that case
// would be wrong and would just fail again against a server that likely
// doesn't implement the Legacy initialize handshake either. So this retries
// server/discover once, using a version drawn from the server's own
// data.supported list, before giving up on Modern.
//
// This never returns an error itself: a failed probe is not a fatal
// condition, it just means "assume Legacy" — if the server is genuinely
// unreachable, the subsequent Legacy attempt will surface that error itself.
func (c *Client) tryModernInitialize(ctx context.Context) bool {
	probeVersion := MCPProtocolVersionModern
	for attempt := 0; attempt < 2; attempt++ {
		// c.protocolVersion drives the _meta/header version withModernMeta
		// (and sendModernHTTPRequest) sends — see modernProtocolVersion.
		// Setting it before the call, rather than threading probeVersion
		// through as a parameter, lets this reuse the exact same request-
		// building path every other Modern request already goes through.
		c.protocolVersion = probeVersion

		discoverReq := c.withModernMeta(&MCPRequest{
			JSONRPC: "2.0",
			ID:      "discover",
			Method:  "server/discover",
			Params:  map[string]any{},
		})

		var resp MCPResponse
		var err error
		if c.transport != nil {
			err = c.transport.roundTrip(ctx, discoverReq, &resp, nil)
		} else {
			err = c.sendModernHTTPRequest(ctx, discoverReq, &resp, nil)
		}
		if err != nil {
			c.protocolVersion = ""
			return false
		}
		if resp.Error != nil {
			if attempt == 0 && resp.Error.Code == ErrorCodeUnsupportedProtocolVersion {
				if next, ok := pickRetryModernVersion(resp.Error.Data, probeVersion); ok {
					probeVersion = next
					continue
				}
			}
			c.protocolVersion = ""
			return false
		}

		result, ok := resp.Result.(map[string]any)
		if !ok {
			c.protocolVersion = ""
			return false
		}
		// A real DiscoverResult always carries supportedVersions (spec-required).
		// Require it explicitly rather than accepting any success envelope, so a
		// lenient/non-compliant server that echoes "{}" for unknown methods isn't
		// mistaken for a Modern one.
		if _, ok := result["supportedVersions"]; !ok {
			c.protocolVersion = ""
			return false
		}

		c.era = eraModern
		// c.protocolVersion already holds probeVersion — the version the
		// server just accepted, which may differ from MCPProtocolVersionModern
		// when this took the retry branch above.
		if instructions, ok := result["instructions"].(string); ok {
			c.instructions = instructions
		}
		c.initialized = true
		return true
	}
	c.protocolVersion = ""
	return false
}

// pickRetryModernVersion extracts a version to retry server/discover with
// from an UnsupportedProtocolVersion error's data.supported list (see
// writeModernProtocolError's callers in modern.go): the "server speaks an
// even newer version" case, where the server told us exactly what it
// accepts. Two tiers, in order:
//
//  1. A version this client knows how to speak Modern (i.e. one of its own
//     supportedModernProtocolVersions) other than alreadyTried.
//  2. Any version dated at or after the Modern era's first revision — a
//     newer dated revision this client predates. Since this library's
//     Modern wire shape is uniform across dated revisions (the version is
//     just a negotiated label, not a structural difference this client
//     parses differently), such a label is worth trying rather than
//     assuming Legacy.
//
// A list offering only versions dated before the Modern era (dual-era and
// Legacy servers legitimately list their Legacy versions alongside — the
// spec's own error example mixes eras) yields ok=false on purpose: a Legacy
// label isn't speakable via server/discover, and the right response is the
// caller's Legacy initialize fallback, not a retry doomed to the same error.
func pickRetryModernVersion(data any, alreadyTried string) (version string, ok bool) {
	m, ok := data.(map[string]any)
	if !ok {
		return "", false
	}
	supported, ok := m["supported"].([]any)
	if !ok {
		return "", false
	}
	pick := func(accept func(string) bool) (string, bool) {
		for _, v := range supported {
			if s, ok := v.(string); ok && s != "" && s != alreadyTried && accept(s) {
				return s, true
			}
		}
		return "", false
	}
	if v, ok := pick(isSupportedModernProtocolVersion); ok {
		return v, true
	}
	return pick(func(s string) bool { return s >= MCPProtocolVersionModern })
}

// withModernMeta returns a shallow copy of req with the Modern-era
// io.modelcontextprotocol/* _meta fields set, without mutating req's own
// Params map (which the caller may still hold a reference to).
func (c *Client) withModernMeta(req *MCPRequest) *MCPRequest {
	params, _ := req.Params.(map[string]any)
	paramsCopy := make(map[string]any, len(params)+1)
	for k, v := range params {
		paramsCopy[k] = v
	}
	paramsCopy["_meta"] = map[string]any{
		metaKeyProtocolVersion: c.modernProtocolVersion(),
		metaKeyClientInfo: map[string]any{
			"name":    mcpClientName,
			"version": mcpClientVersion,
		},
		metaKeyClientCapabilities: c.capabilitiesMap(),
	}
	out := *req
	out.Params = paramsCopy
	return &out
}

// modernProtocolVersion is the Modern protocol version string this client
// sends on requests: c.protocolVersion once tryModernInitialize has
// negotiated one (which may not be MCPProtocolVersionModern — see its
// retry-on-UnsupportedProtocolVersion behavior), or MCPProtocolVersionModern
// as the default probe value before/during negotiation.
//
// Deliberately unlocked, same convention and reason as capabilitiesMap:
// called from within tryModernInitialize while Initialize already holds
// c.mu, and from ordinary requests afterward where c.protocolVersion is
// only ever read, never mutated again post-Initialize.
func (c *Client) modernProtocolVersion() string {
	if c.protocolVersion != "" {
		return c.protocolVersion
	}
	return MCPProtocolVersionModern
}

// sendModernHTTPRequest is the Modern-era counterpart of sendRequest's
// unexported HTTP body: it mirrors req.Method (and params.name/params.uri,
// where applicable) into the Mcp-Method/Mcp-Name headers per the spec's
// header-based routing, and reads the response body regardless of HTTP
// status — Modern protocol errors (HeaderMismatch, UnsupportedProtocolVersion)
// use 4xx with a JSON-RPC error body, unlike Legacy's always-200 convention.
//
// req must already carry Modern _meta (see withModernMeta); this function
// does not add it, so it can also serve as the era-detection probe itself.
func (c *Client) sendModernHTTPRequest(ctx context.Context, req *MCPRequest, resp *MCPResponse, respHeaders *http.Header) error {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewBuffer(reqBody))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	c.applyRequestHeaders(httpReq.Header)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("User-Agent", fmt.Sprintf("%s/%s", mcpClientName, mcpClientVersion))
	httpReq.Header.Set(headerMcpMethod, req.Method)
	httpReq.Header.Set(headerProtocolVersion, c.modernProtocolVersion())

	// Same fan-out hop propagation as sendRequest's Legacy path — see
	// resources.go's maxResourceFanoutHops.
	if hop := resourceFanoutHopFrom(ctx); hop > 0 {
		httpReq.Header.Set(headerResourceFanoutHop, strconv.Itoa(hop))
	}

	if params, ok := req.Params.(map[string]any); ok {
		if name, needsName := modernRequestName(req.Method, params); needsName {
			httpReq.Header.Set(headerMcpName, encodeModernHeaderValue(name))
		}
	}
	// Mcp-Param-* headers mirrored from x-mcp-header tool arguments (set by
	// CallTool, see client_xmcpheader.go).
	for k, v := range toolCallHeadersFrom(ctx) {
		httpReq.Header[k] = v
	}

	if err := c.applyAuthHeader(httpReq.Header); err != nil {
		return fmt.Errorf("failed to get auth header: %w", err)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer httpResp.Body.Close()

	if respHeaders != nil {
		*respHeaders = httpResp.Header
	}

	// A request may be answered with an SSE stream (see client_stream.go).
	if httpResp.StatusCode == http.StatusOK && strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream") {
		return c.readResponseStream(ctx, httpResp.Body, req, resp)
	}

	bodyBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		// A Modern protocol-level error still carries a JSON-RPC error body;
		// surface it as a normal *MCPResponse rather than just the bare
		// status, since it has actionable detail (e.g. supported versions).
		if json.Unmarshal(bodyBytes, resp) == nil && resp.Error != nil {
			return nil
		}
		return fmt.Errorf("server returned status %d", httpResp.StatusCode)
	}

	if err := json.Unmarshal(bodyBytes, resp); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	return nil
}

// runModernSubscriptionReader is the Modern-era counterpart of
// runSSEReader: it maintains a long-lived subscriptions/listen stream
// (POST, since Modern removes the GET endpoint entirely) instead of the
// Legacy GET event-stream, reconnecting with the same exponential backoff.
// connectModernSubscription builds and sends one Modern-era
// subscriptions/listen request, subscribed to every list-changed type
// (matching the Legacy reader's receive-everything behavior). The ack is
// consumed and discarded via the translator, and each subsequent
// notification's Modern (snake_case) method name is translated back to the
// Legacy constant handleNotification already knows, so cache invalidation
// and the On*Changed callbacks work identically regardless of which era
// delivered the event.
func (c *Client) connectModernSubscription(ctx context.Context) (*http.Response, func(string) (string, bool), error) {
	req := c.withModernMeta(&MCPRequest{
		JSONRPC: "2.0",
		ID:      "subscribe",
		Method:  "subscriptions/listen",
		Params: map[string]any{
			"notifications": map[string]any{
				"toolsListChanged":     true,
				"resourcesListChanged": true,
				"promptsListChanged":   true,
			},
		},
	})
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(reqBody))
	if err != nil {
		return nil, nil, err
	}
	c.applyRequestHeaders(httpReq.Header)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set(headerMcpMethod, "subscriptions/listen")
	httpReq.Header.Set(headerProtocolVersion, c.modernProtocolVersion())
	if err := c.applyAuthHeader(httpReq.Header); err != nil {
		return nil, nil, err
	}

	resp, err := c.httpClient.Do(httpReq)
	return resp, translateModernNotification, err
}

// translateModernNotification drops the subscription acknowledgement for the
// subscription reader (it is a handshake reply, not a change event); the
// list-changed method names are spelled the same in both eras.
func translateModernNotification(modernMethod string) (string, bool) {
	if modernMethod == "notifications/subscriptions/acknowledged" {
		return "", false
	}
	return modernMethod, true
}
