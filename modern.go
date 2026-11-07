package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/paularlott/jsonrpc"
)

// Support for MCP protocol revision 2026-07-28 (the "Modern" era): a
// stateless, per-request model that replaces the initialize handshake (the
// "Legacy" era, 2024-11-05..2025-11-25) with protocol version, client
// identity, and capabilities carried on every request's _meta.
//
// A Server implements both eras simultaneously ("Dual-era"): a request is
// routed to the Modern path only when its body claims a Modern revision in
// io.modelcontextprotocol/protocolVersion _meta (any transport), or, on
// HTTP, when its MCP-Protocol-Version header names a Modern revision (a
// malformed Modern request, refused there); otherwise it falls through to
// the existing Legacy code, completely unchanged. See isModernRequest. See https://modelcontextprotocol.io/specification/2026-07-28
// and docs/guides/protocol-support.md for the full picture.
//
// Known gaps, deliberately out of scope for this pass (see the guide):
//   - MRTR (server-initiated sampling/elicitation/roots) — this library
//     implements none of these in either era, so there is nothing to migrate.
//   - stdio does not implement subscriptions/listen: doing so correctly needs
//     the calling request's own JSON-RPC id (to use as the subscription id)
//     and a way to cancel a still-open subscription on an inbound
//     notifications/cancelled — neither is something the underlying jsonrpc
//     package's Handler signature exposes today, and stdio's existing
//     connection-scoped notification delivery (every connected client
//     already receives every listChanged notification, regardless of era)
//     means nothing is functionally lost by not having it. A stdio client
//     that calls it gets a standard, honest "Method not found" (-32601) —
//     see TestStdioSubscriptionsListenNotSupported — rather than a silent or
//     partial implementation.
const (
	// MCPProtocolVersionModern is the current Modern-era protocol revision.
	MCPProtocolVersionModern = "2026-07-28"

	metaKeyProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaKeyClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaKeyClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	metaKeyServerInfo         = "io.modelcontextprotocol/serverInfo"
	metaKeySubscriptionID     = "io.modelcontextprotocol/subscriptionId"

	headerMcpMethod = "Mcp-Method"
	headerMcpName   = "Mcp-Name"

	base64SentinelPrefix = "=?base64?"
	base64SentinelSuffix = "?="
)

// supportedModernProtocolVersions lists all Modern-era protocol versions this
// server accepts, mirroring supportedProtocolVersions for the Legacy era.
var supportedModernProtocolVersions = []string{MCPProtocolVersionModern}

func isSupportedModernProtocolVersion(version string) bool {
	version = strings.TrimSpace(version)
	for _, supported := range supportedModernProtocolVersions {
		if supported == version {
			return true
		}
	}
	return false
}

// isLegacyProtocolVersion reports whether version names a Legacy-era
// revision this server supports (the MCP-Protocol-Version a Legacy client
// sends after initialize).
func isLegacyProtocolVersion(version string) bool {
	version = strings.TrimSpace(version)
	for _, legacy := range supportedProtocolVersions {
		if legacy == version {
			return true
		}
	}
	return false
}

// allSupportedProtocolVersions lists every protocol version this dual-era
// server accepts — Modern revisions first, then Legacy — for the wire places
// that must describe the whole server, not one era: server/discover's
// supportedVersions and UnsupportedProtocolVersionError's data.supported.
// The spec's own example of the latter mixes a Modern and a Legacy version
// (["2026-07-25","2025-06-18"]), and a client deciding what to do next needs
// the full truth: a Modern entry means "retry me Modern", a Legacy entry
// means "fall back to initialize". (The Legacy-era version-mismatch errors
// in the initialize path keep listing supportedProtocolVersions alone —
// those requests can only ever negotiate Legacy.)
func allSupportedProtocolVersions() []string {
	all := make([]string, 0, len(supportedModernProtocolVersions)+len(supportedProtocolVersions))
	all = append(all, supportedModernProtocolVersions...)
	all = append(all, supportedProtocolVersions...)
	return all
}

// isModernRequest reports whether an incoming request should be handled by
// the Modern (per-request, stateless) code path rather than falling through
// to the existing Legacy handshake-based one.
//
// Classification is body-primary, as the 2026-07-28 versioning spec's
// dual-era rule puts it ("a request carrying modern per-request _meta is
// served statelessly according to this revision; an initialize request
// selects legacy semantics") and as the official TypeScript SDK server
// implements it: the io.modelcontextprotocol/protocolVersion _meta claim
// decides the era on every transport, and it classifies into the era the
// named revision belongs to. A claim naming a Legacy revision is Legacy; any
// other claim is Modern, so an unknown (e.g. newer) revision gets the
// Modern path's UnsupportedProtocolVersionError with the full supported list.
//
// The MCP-Protocol-Version header is only a cross-check: it never upgrades a
// request without a claim into the Modern era, with one exception — a header
// naming a Modern revision on a request with no claim is a malformed Modern
// request, and the Modern path refuses it with HeaderMismatch (400) as the
// spec requires rather than silently serving it as Legacy.
//
// The Mcp-Method / Mcp-Name headers are deliberately never an era signal.
// Legacy clients send them too (mcp-remote stamps Mcp-Method and Mcp-Name on
// every POST, including initialize, which carries no MCP-Protocol-Version
// header at all); no pre-2026 spec defines them, and the Streamable HTTP
// spec calls the body "the source of truth" for them. They are validated
// only once a request has classified as Modern.
func isModernRequest(r *http.Request, req *MCPRequest) bool {
	_, meta := modernRequestParams(req)
	if meta != nil {
		if claim, ok := meta[metaKeyProtocolVersion].(string); ok {
			return !isLegacyProtocolVersion(claim)
		}
	}
	return r != nil && isSupportedModernProtocolVersion(r.Header.Get(headerProtocolVersion))
}

// modernRequestParams splits a request's Params into the plain params map and
// its _meta sub-object, tolerating either being absent or the wrong shape.
func modernRequestParams(req *MCPRequest) (params map[string]any, meta map[string]any) {
	params, _ = req.Params.(map[string]any)
	if params == nil {
		return nil, nil
	}
	meta, _ = params["_meta"].(map[string]any)
	return params, meta
}

// modernRequestName returns the value a Modern request's Mcp-Name header must
// match (params.name for tools/call and prompts/get, params.uri for
// resources/read), and whether that method requires the header at all.
func modernRequestName(method string, params map[string]any) (name string, needsName bool) {
	switch method {
	case "tools/call", "prompts/get":
		n, _ := params["name"].(string)
		return n, true
	case "resources/read":
		n, _ := params["uri"].(string)
		return n, true
	default:
		return "", false
	}
}

// decodeModernHeaderValue reverses encodeModernHeaderValue: a value wrapped in
// the "=?base64?...?=" sentinel is base64-decoded, anything else is returned
// as-is.
func decodeModernHeaderValue(v string) (string, error) {
	if strings.HasPrefix(v, base64SentinelPrefix) && strings.HasSuffix(v, base64SentinelSuffix) {
		encoded := strings.TrimSuffix(strings.TrimPrefix(v, base64SentinelPrefix), base64SentinelSuffix)
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", err
		}
		return string(decoded), nil
	}
	return v, nil
}

// needsModernHeaderEncoding reports whether v cannot be safely carried as a
// plain HTTP header value per RFC 9110 (visible ASCII 0x21-0x7E, space, and
// horizontal tab only, with no leading/trailing whitespace), or itself
// collides with the base64 sentinel pattern and so must be encoded to avoid
// ambiguity.
func needsModernHeaderEncoding(v string) bool {
	if v != strings.TrimSpace(v) {
		return true
	}
	if strings.HasPrefix(v, base64SentinelPrefix) && strings.HasSuffix(v, base64SentinelSuffix) {
		return true
	}
	for _, r := range v {
		if r == ' ' || r == '\t' {
			continue
		}
		if r < 0x21 || r > 0x7E {
			return true
		}
	}
	return false
}

// encodeModernHeaderValue encodes v for use as a Modern-era HTTP header value
// (Mcp-Name, Mcp-Param-*), per the spec's Base64 sentinel format.
func encodeModernHeaderValue(v string) string {
	if !needsModernHeaderEncoding(v) {
		return v
	}
	return base64SentinelPrefix + base64.StdEncoding.EncodeToString([]byte(v)) + base64SentinelSuffix
}

// writeModernProtocolError sends a Modern-era protocol-level JSON-RPC error
// with the given HTTP status, per the spec's requirement that HeaderMismatch,
// UnsupportedProtocolVersionError and similar use 400 Bad Request rather than
// the Legacy transport's always-200 convention (sendMCPError).
func (s *Server) writeModernProtocolError(w http.ResponseWriter, id any, status int, code int, message string, data any) {
	s.writeMCPError(w, status, id, code, message, data)
}

// writeModernResult sends a successful Modern-era JSON-RPC result. Callers
// (handleServerDiscoverHTTP, handleSubscriptionsListen's initial handshake)
// are expected to have already included "resultType" in result; results
// produced by reusing a Legacy handler go through modernShape
// instead, which injects it.
func (s *Server) writeModernResult(w http.ResponseWriter, id any, result any) {
	s.writeMCPResponse(w, http.StatusOK, id, result)
}

// handleModernRequest validates a Modern-era HTTP request (header/body
// consistency, protocol version, required client capabilities) and, once
// valid, either serves a Modern-only method (server/discover,
// subscriptions/listen) or dispatches to the same handler Legacy uses via
// dispatchMethod, then shapes the result for the Modern wire format.
func (s *Server) handleModernRequest(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	params, meta := modernRequestParams(req)

	mcpMethodHeader := r.Header.Get(headerMcpMethod)
	if mcpMethodHeader == "" {
		s.writeModernProtocolError(w, req.ID, http.StatusBadRequest, ErrorCodeHeaderMismatch,
			"Header mismatch: Mcp-Method header is required", nil)
		return
	}
	if mcpMethodHeader != req.Method {
		s.writeModernProtocolError(w, req.ID, http.StatusBadRequest, ErrorCodeHeaderMismatch,
			fmt.Sprintf("Header mismatch: Mcp-Method header value %q does not match body method %q", mcpMethodHeader, req.Method), nil)
		return
	}

	if name, needsName := modernRequestName(req.Method, params); needsName {
		rawHeaderName := r.Header.Get(headerMcpName)
		if rawHeaderName == "" {
			s.writeModernProtocolError(w, req.ID, http.StatusBadRequest, ErrorCodeHeaderMismatch,
				"Header mismatch: Mcp-Name header is required", nil)
			return
		}
		headerName, err := decodeModernHeaderValue(rawHeaderName)
		if err != nil || headerName != name {
			s.writeModernProtocolError(w, req.ID, http.StatusBadRequest, ErrorCodeHeaderMismatch,
				fmt.Sprintf("Header mismatch: Mcp-Name header value does not match body value %q", name), nil)
			return
		}
	}

	metaVersion, _ := meta[metaKeyProtocolVersion].(string)
	headerVersion := r.Header.Get(headerProtocolVersion)
	if headerVersion == "" {
		s.writeModernProtocolError(w, req.ID, http.StatusBadRequest, ErrorCodeHeaderMismatch,
			"Header mismatch: MCP-Protocol-Version header is required", nil)
		return
	}
	if headerVersion != metaVersion {
		s.writeModernProtocolError(w, req.ID, http.StatusBadRequest, ErrorCodeHeaderMismatch,
			fmt.Sprintf("Header mismatch: MCP-Protocol-Version header value %q does not match body value %q", headerVersion, metaVersion), nil)
		return
	}
	if !isSupportedModernProtocolVersion(metaVersion) {
		s.writeModernProtocolError(w, req.ID, http.StatusBadRequest, ErrorCodeUnsupportedProtocolVersion,
			"Unsupported protocol version", map[string]any{
				"supported": allSupportedProtocolVersions(),
				"requested": metaVersion,
			})
		return
	}

	// A request missing a required _meta field (here: clientCapabilities
	// itself absent) is malformed per the spec's general _meta rule and gets
	// InvalidParams/400 — distinct from MissingRequiredClientCapabilityError
	// (-32021), which is reserved for clientCapabilities being *present* but
	// lacking a specific capability a particular operation requires; this
	// codebase has no such per-operation capability requirement today.
	clientCapabilities, ok := meta[metaKeyClientCapabilities]
	if !ok {
		s.writeModernProtocolError(w, req.ID, http.StatusBadRequest, ErrorCodeInvalidParams,
			"Invalid params", map[string]any{
				"details": metaKeyClientCapabilities + " is required in _meta",
			})
		return
	}

	// Remember the client's declared capabilities on every Modern request (there's
	// no separate initialize call in this era) so ClientCapabilities/SupportsUIApps
	// work the same way they do for Legacy clients. See ClientCapabilities' doc
	// comment for the multi-client-HTTP caveat this shares with the Legacy path.
	if capMap, ok := clientCapabilities.(map[string]any); ok {
		s.mu.Lock()
		s.lastClientCapabilities = capMap
		s.mu.Unlock()
	}

	// x-mcp-header: every Mcp-Param-* header a tool declares must match the
	// body before the tool runs (xmcpheader.go).
	if req.Method == "tools/call" {
		if msg := s.checkToolCallHeaders(r, params); msg != "" {
			s.writeModernProtocolError(w, req.ID, http.StatusBadRequest, ErrorCodeHeaderMismatch, msg, nil)
			return
		}
	}

	switch req.Method {
	case "server/discover":
		s.handleServerDiscoverHTTP(w, r, req)
	case "subscriptions/listen":
		s.handleSubscriptionsListen(w, r, req)
	default:
		if !dispatchableMethods[req.Method] {
			// Per the spec: an unrecognized method gets HTTP 404, distinct
			// from the 200 used for an ordinary JSON-RPC method-level error
			// (e.g. tools/call with an unknown tool name) — the JSON-RPC
			// error body is what lets a client tell this apart from a 404
			// returned by a legacy HTTP+SSE server with no modern endpoint.
			s.writeModernProtocolError(w, req.ID, http.StatusNotFound, ErrorCodeMethodNotFound,
				"Method not found", map[string]any{"method": req.Method})
			return
		}
		s.dispatchResponse(w, r, req, s.modernShape)
	}
}

// cacheHintsFor returns the ttlMs/cacheScope values a Modern result for
// method must carry, per the spec's caching model: servers MUST include
// caching hints on tools/list, prompts/list, resources/list,
// resources/templates/list, skills/list, skills/get and resources/read
// (also server/discover, handled separately in buildDiscoverResult).
// ttlMs 0 ("immediately stale") is always spec-valid; this library has no
// result-caching infrastructure to compute a longer, meaningful TTL, so 0 is
// the honest value rather than an invented one. cacheScope "public" fits the
// list methods (the library's own lists are caller-independent; per-user
// listings arrive through providers, same as tools); "private" is the
// conservative choice for reads and skills/get, whose result is one
// caller-addressed document.
func cacheHintsFor(method string) (ttlMs int, cacheScope string, applicable bool) {
	switch method {
	case "tools/list", "prompts/list", "resources/list", "resources/templates/list", "skills/list":
		return 0, "public", true
	case "resources/read", "skills/get":
		return 0, "private", true
	default:
		return 0, "", false
	}
}

// modernResponseCapture buffers a Legacy handler's output so
// modernShape can inject Modern-only result fields before writing
// the real response.
type modernResponseCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newModernResponseCapture() *modernResponseCapture {
	return &modernResponseCapture{header: make(http.Header), status: http.StatusOK}
}

func (c *modernResponseCapture) Header() http.Header { return c.header }

func (c *modernResponseCapture) Write(b []byte) (int, error) { return c.body.Write(b) }

func (c *modernResponseCapture) WriteHeader(status int) { c.status = status }

// modernShape rewrites a captured Legacy-shaped response into the Modern wire
// format: successful results gain "resultType": "complete" and
// _meta.io.modelcontextprotocol/serverInfo; JSON-RPC-level errors (e.g.
// unknown tool) pass through unchanged, since those are ordinary method
// errors in both eras, not protocol-level ones. The result is written as
// JSON or as an SSE event by dispatchResponse (server_stream.go).
func (s *Server) modernShape(method string, capture *modernResponseCapture) capturedResponse {
	var raw map[string]any
	if err := json.Unmarshal(capture.body.Bytes(), &raw); err != nil {
		return capturedResponse{status: capture.status, header: capture.header, body: capture.body.Bytes()}
	}

	if resultRaw, ok := raw["result"]; ok {
		resultMap, ok := resultRaw.(map[string]any)
		if !ok {
			resultMap = map[string]any{}
		}
		resultMap["resultType"] = "complete"
		if ttlMs, cacheScope, ok := cacheHintsFor(method); ok {
			resultMap["ttlMs"] = ttlMs
			resultMap["cacheScope"] = cacheScope
		}
		metaOut, _ := resultMap["_meta"].(map[string]any)
		if metaOut == nil {
			metaOut = map[string]any{}
		}
		metaOut[metaKeyServerInfo] = s.serverInfoMap()
		resultMap["_meta"] = metaOut
		raw["result"] = resultMap
	}

	var body bytes.Buffer
	json.NewEncoder(&body).Encode(raw)
	header := http.Header{}
	header.Set("Content-Type", "application/json; charset=utf-8")
	header.Set("Cache-Control", "no-cache, no-store, must-revalidate")
	return capturedResponse{status: capture.status, header: header, body: body.Bytes()}
}

// serverInfoMap builds the io.modelcontextprotocol/serverInfo object shared
// by every Modern-era result (buildDiscoverResult, modernShape,
// shapeModernResult), so the server's identity — including icons, if set via
// [Server.SetIcons] — is reported consistently everywhere it appears.
func (s *Server) serverInfoMap() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info := map[string]any{"name": s.name, "version": s.version}
	if len(s.icons) > 0 {
		info["icons"] = s.icons
	}
	return info
}

// buildDiscoverResult builds the server/discover result shared by the HTTP
// and stdio handlers: supported versions across both eras, capabilities,
// server identity, and instructions. showAll suppresses the discovery-tool
// hint the same way it does for the Legacy initialize response (see
// handleInitialize) — a show-all client already sees discoverable tools
// directly, so it doesn't need to be told to search for them.
func (s *Server) buildDiscoverResult(ctx context.Context, showAll bool) map[string]any {
	s.mu.RLock()
	instructions := s.instructions
	s.mu.RUnlock()

	if !showAll && s.hasDiscoverableToolsNow(ctx) {
		instructions = appendDiscoveryInstructions(instructions)
	}

	return map[string]any{
		"resultType":        "complete",
		"supportedVersions": allSupportedProtocolVersions(),
		"capabilities":      s.buildCapabilities(MCPProtocolVersionModern),
		"_meta":             map[string]any{metaKeyServerInfo: s.serverInfoMap()},
		"instructions":      instructions,
		// server/discover is one of the cacheable operations the spec
		// requires caching hints on; see cacheHintsFor's doc comment for why
		// 0/"public" are the honest defaults here.
		"ttlMs":      0,
		"cacheScope": "public",
	}
}

// rejectStdioModern wraps a Legacy-only stdio method ("initialize", "ping" —
// both removed outright by the 2026-07-28 revision, see dispatchableMethods'
// doc comment) so a Modern-shaped request naming it gets the same
// MethodNotFound a Modern HTTP request would, instead of silently running
// the Legacy handler. Legacy requests (no Modern _meta) pass through
// unchanged.
func rejectStdioModern(method string, handler func(ctx context.Context, params json.RawMessage) (any, error)) func(ctx context.Context, params json.RawMessage) (any, error) {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		if _, isModern := stdioModernMeta(params); isModern {
			return nil, jsonrpc.NewError(ErrorCodeMethodNotFound, "Method not found", map[string]any{"method": method})
		}
		return handler(ctx, params)
	}
}

// wrapStdioModern wraps a stdio method handler so a Modern-era request (one
// whose raw params carry _meta.protocolVersion — the only Modern signal
// available over stdio, since there are no HTTP headers to check) gets the
// same validation and result reshaping the HTTP path applies: an
// unsupported protocolVersion or a missing clientCapabilities is rejected
// before the underlying handler ever runs (mirroring handleModernRequest's
// checks, since there's no shared entry point to put them in once for both
// transports the way HTTP's handleModernRequest can), the client's declared
// capabilities are captured the same way handleModernRequest's now does for
// ClientCapabilities/SupportsUIApps, and a successful result gets resultType,
// _meta.serverInfo, and — for the operations the spec requires it on —
// ttlMs/cacheScope. A Legacy stdio request's raw params carry no such _meta,
// so the handler's own result passes through completely unchanged.
func (s *Server) wrapStdioModern(method string, handler func(ctx context.Context, params json.RawMessage) (any, error)) func(ctx context.Context, params json.RawMessage) (any, error) {
	return func(ctx context.Context, params json.RawMessage) (any, error) {
		meta, isModern := stdioModernMeta(params)
		if !isModern {
			return handler(ctx, params)
		}

		version, _ := meta[metaKeyProtocolVersion].(string)
		if !isSupportedModernProtocolVersion(version) {
			return nil, jsonrpc.NewError(ErrorCodeUnsupportedProtocolVersion, "Unsupported protocol version", map[string]any{
				"supported": allSupportedProtocolVersions(),
				"requested": version,
			})
		}

		// See handleModernRequest's identical check (modern.go) for why this
		// is InvalidParams, not MissingRequiredClientCapabilityError: the
		// field itself is absent, which is a different, more general defect
		// than clientCapabilities being present but missing one specific
		// capability an operation requires.
		clientCapabilities, ok := meta[metaKeyClientCapabilities]
		if !ok {
			return nil, jsonrpc.NewError(ErrorCodeInvalidParams, "Invalid params", map[string]any{
				"details": metaKeyClientCapabilities + " is required in _meta",
			})
		}
		if capMap, ok := clientCapabilities.(map[string]any); ok {
			s.mu.Lock()
			s.lastClientCapabilities = capMap
			s.mu.Unlock()
		}

		result, err := handler(withStdioModern(ctx), params)
		if err != nil {
			return result, err
		}
		return s.shapeModernResult(method, result), nil
	}
}

// stdioModernMeta parses a stdio request's raw params and reports whether
// they carry the Modern-era protocolVersion signal; when they do, it also
// returns the parsed _meta map for wrapStdioModern's validation.
func stdioModernMeta(params json.RawMessage) (meta map[string]any, isModern bool) {
	if len(params) == 0 {
		return nil, false
	}
	var parsed struct {
		Meta map[string]any `json:"_meta"`
	}
	if json.Unmarshal(params, &parsed) != nil {
		return nil, false
	}
	if _, ok := parsed.Meta[metaKeyProtocolVersion].(string); !ok {
		return nil, false
	}
	return parsed.Meta, true
}

// shapeModernResult is modernShape's transport-agnostic core,
// operating on an already-decoded result value (stdio has no HTTP response
// to capture and rewrite): resultType, _meta.serverInfo, and — for cacheable
// operations — ttlMs/cacheScope.
func (s *Server) shapeModernResult(method string, result any) any {
	data, err := json.Marshal(result)
	if err != nil {
		return result
	}
	var resultMap map[string]any
	if json.Unmarshal(data, &resultMap) != nil {
		return result
	}
	resultMap["resultType"] = "complete"
	if ttlMs, cacheScope, ok := cacheHintsFor(method); ok {
		resultMap["ttlMs"] = ttlMs
		resultMap["cacheScope"] = cacheScope
	}
	meta, _ := resultMap["_meta"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
	}
	meta[metaKeyServerInfo] = s.serverInfoMap()
	resultMap["_meta"] = meta
	return resultMap
}

func (s *Server) handleServerDiscoverHTTP(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	s.writeModernResult(w, req.ID, s.buildDiscoverResult(r.Context(), GetShowAllFromRequest(r)))
}

// modernSubscriptionSink is a notificationSink that only forwards the
// notification methods a subscriptions/listen caller asked for, per the
// spec's "MUST NOT send notification types the client has not explicitly
// requested" rule. Like sseSink, sending is non-blocking.
type modernSubscriptionSink struct {
	ch     chan sseEvent
	wanted map[string]bool
}

func newModernSubscriptionSink(buffer int, wanted map[string]bool) *modernSubscriptionSink {
	return &modernSubscriptionSink{ch: make(chan sseEvent, buffer), wanted: wanted}
}

func (s *modernSubscriptionSink) send(method string, params any) {
	if !s.wanted[method] {
		return
	}
	select {
	case s.ch <- sseEvent{method: method, params: params}:
	default:
	}
}

func writeSSEData(w http.ResponseWriter, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
	return err
}

// Shutdown signals every open Modern-era subscriptions/listen stream to close
// gracefully: each one responds to its original request with a completion
// result (see the spec's "Graceful Closure") before ending, rather than the
// connection just going dead when the underlying HTTP server stops. Call it
// once, before (or while) shutting down the *http.Server this Server is
// mounted on — for example:
//
//	server.Shutdown()
//	httpServer.Shutdown(ctx)
//
// It has no effect on Legacy clients (the GET SSE stream, handleSSEStream,
// simply ends when its connection does, matching pre-existing behavior) and
// is safe to call multiple times or on a server with no open subscriptions.
func (s *Server) Shutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdownCh) })
}

// handleSubscriptionsListen serves subscriptions/listen: the Modern-era
// replacement for the Legacy GET SSE stream, scoped to just the notification
// types the caller filtered for and correlated by subscriptionId. The Legacy
// stream (handleSSEStream) is untouched and keeps serving Legacy clients
// concurrently.
func (s *Server) handleSubscriptionsListen(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeModernProtocolError(w, req.ID, http.StatusInternalServerError, ErrorCodeInternalError, "streaming unsupported", nil)
		return
	}

	var filter struct {
		Notifications struct {
			ToolsListChanged     bool `json:"toolsListChanged"`
			PromptsListChanged   bool `json:"promptsListChanged"`
			ResourcesListChanged bool `json:"resourcesListChanged"`
		} `json:"notifications"`
	}
	// A body that doesn't even unmarshal into this shape (params.notifications
	// present but not an object, say) used to be silently ignored here,
	// leaving every field at its zero value — indistinguishable from a
	// deliberate "no notifications yet" request. That, and an empty/all-false
	// notifications object, both used to fall through to a 200 that opens the
	// SSE stream anyway, subscribed to nothing, with no way for the caller to
	// learn why nothing ever arrives. Both are now rejected up front instead.
	if err := s.parseParams(req, &filter); err != nil {
		s.writeModernProtocolError(w, req.ID, http.StatusBadRequest, ErrorCodeInvalidParams,
			"Invalid params", map[string]any{"details": err.Error()})
		return
	}

	wanted := map[string]bool{}
	acked := map[string]any{}
	if filter.Notifications.ToolsListChanged {
		wanted[NotificationToolsChanged] = true
		acked["toolsListChanged"] = true
	}
	if filter.Notifications.PromptsListChanged {
		wanted[NotificationPromptsChanged] = true
		acked["promptsListChanged"] = true
	}
	if filter.Notifications.ResourcesListChanged {
		wanted[NotificationResourcesChanged] = true
		acked["resourcesListChanged"] = true
	}
	if len(wanted) == 0 {
		s.writeModernProtocolError(w, req.ID, http.StatusBadRequest, ErrorCodeInvalidParams,
			"Invalid params", map[string]any{"details": "params.notifications must select at least one notification type"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sink := newModernSubscriptionSink(64, wanted)
	subID := s.notifications.subscribe(sink)
	defer s.notifications.unsubscribe(subID)

	if err := writeSSEData(w, MCPNotification{
		JSONRPC: "2.0",
		Method:  "notifications/subscriptions/acknowledged",
		Params: map[string]any{
			"_meta":         map[string]any{metaKeySubscriptionID: req.ID},
			"notifications": acked,
		},
	}); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case ev := <-sink.ch:
			if err := writeSSEData(w, MCPNotification{
				JSONRPC: "2.0",
				Method:  ev.method,
				Params:  map[string]any{"_meta": map[string]any{metaKeySubscriptionID: req.ID}},
			}); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-s.shutdownCh:
			// Graceful closure per spec: respond to the ORIGINAL
			// subscriptions/listen request with a completion result before
			// ending the stream, distinguishing an orderly server shutdown
			// from an abrupt client/transport disconnect (the
			// r.Context().Done() case below, which gets no such response
			// since there is no longer anyone to receive it).
			_ = writeSSEData(w, MCPResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"resultType": "complete",
					"_meta":      map[string]any{metaKeySubscriptionID: req.ID},
				},
			})
			flusher.Flush()
			return
		case <-r.Context().Done():
			return
		}
	}
}
