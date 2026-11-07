package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The Legacy-era HTTP transport: request handling, dispatch, initialization, capabilities, sessions, and wire writers.

// HandleRequest handles MCP protocol requests
func (s *Server) HandleRequest(w http.ResponseWriter, r *http.Request) {
	// Validate Origin before anything else — the Streamable HTTP transport's
	// MUST, protecting against DNS rebinding (a malicious page's JS in a
	// victim's browser reaching this server, which would otherwise trust
	// anything arriving over localhost). This runs ahead of the OPTIONS
	// preflight branch too: CORS response headers only stop a browser from
	// reading a disallowed response, they don't stop the server from having
	// already acted on the request, which is exactly what this check is
	// for. See origin.go's defaultOriginValidator for the policy and how to
	// widen it.
	origin := r.Header.Get("Origin")
	if !s.originAllowed(origin) {
		http.Error(w, "Origin not allowed", http.StatusForbidden)
		return
	}

	// Handle CORS preflight
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", corsOriginHeader(origin))
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, DELETE")
		// The Modern era's Mcp-Method/Mcp-Name headers (required on every
		// request — see modern.go) aren't in the static list below, so a
		// cross-origin Modern browser client's preflight would otherwise
		// fail; reflecting the browser's own Access-Control-Request-Headers
		// back, when present, additionally covers Mcp-Param-{Name} (a
		// server-declared, per-tool-parameter header set — see the
		// x-mcp-header spec section — whose names can't be enumerated
		// ahead of time in a static list at all).
		allowHeaders := "Content-Type, Authorization, MCP-Protocol-Version, MCP-Session-Id, Mcp-Method, Mcp-Name"
		if requested := r.Header.Get("Access-Control-Request-Headers"); requested != "" {
			allowHeaders = requested
		}
		w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
		w.Header().Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Set CORS headers for actual requests
	w.Header().Set("Access-Control-Allow-Origin", corsOriginHeader(origin))

	// Handle DELETE requests (session termination, Streamable HTTP). The
	// spec lets a server answer 405 when it does not allow clients to end
	// sessions; a session manager that cannot revoke (JWTSessionManager)
	// says so with ErrSessionRevocationUnsupported, so the client is not
	// told a session is gone while it keeps working. After a real
	// termination the session id answers 404 like any unknown one.
	if r.Method == http.MethodDelete {
		sm := s.getSessionManager()
		if sm == nil {
			w.Header().Set("Allow", "POST, GET, OPTIONS")
			http.Error(w, "Session management not enabled", http.StatusMethodNotAllowed)
			return
		}
		if _, ok := negotiateProtocolVersion(r.Header.Get(headerProtocolVersion), "2025-03-26"); !ok {
			http.Error(w, "Unsupported MCP-Protocol-Version", http.StatusBadRequest)
			return
		}
		if _, ok, status, message := s.checkSession(r.Context(), r); !ok {
			http.Error(w, message, status)
			return
		}
		if err := sm.DeleteSession(r.Context(), r.Header.Get(headerSessionID)); err != nil {
			if errors.Is(err, ErrSessionRevocationUnsupported) {
				w.Header().Set("Allow", "POST, GET, OPTIONS")
				http.Error(w, "Session termination not supported", http.StatusMethodNotAllowed)
				return
			}
			http.Error(w, fmt.Sprintf("Failed to delete session: %v", err), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		return
	}

	// Handle GET requests: open a long-lived SSE stream for server->client
	// notifications, but only when the client asks for an event stream (the
	// Streamable HTTP push channel). A plain GET stays a 405.
	if r.Method == http.MethodGet {
		if acceptsEventStream(r.Header.Get("Accept")) {
			s.handleSSEStream(w, r)
			return
		}
		w.Header().Set("Allow", "POST, GET, DELETE, OPTIONS")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, GET, DELETE, OPTIONS")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Validate Content-Type
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" && !strings.HasPrefix(contentType, "application/json;") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxRequestBytes()))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		s.sendProtocolAwareError(w, r, &MCPRequest{}, nil, ErrorCodeParseError, "Parse error", map[string]any{
			"details": err.Error(),
		})
		return
	}
	raw = bytes.TrimSpace(raw)

	// A JSON array is a JSON-RPC batch: required of a 2025-03-26 server,
	// withdrawn by 2025-06-18 and later revisions.
	if len(raw) > 0 && raw[0] == '[' {
		s.handleLegacyBatch(w, r, raw)
		return
	}

	var req MCPRequest
	if perr := decodeMCPRequest(raw, &req); perr != nil {
		s.sendProtocolAwareError(w, r, &req, req.ID, perr.Code, perr.Message, perr.Data)
		return
	}

	// A JSON-RPC notification (a method but no id) or response (an id but no
	// method — a Legacy client answering a server-to-client request) is
	// acknowledged with 202 Accepted and no body, per the Streamable HTTP
	// transport in both eras. This server sends no server-to-client requests
	// and defines no handling for client notifications
	// (notifications/initialized, notifications/cancelled), so accepting is
	// all there is to do. Deliberately lenient: a response is accepted even
	// though 2026-07-28 forbids clients from sending one (a response carries
	// no era signal to reject it by). A Legacy message is still checked for
	// a valid MCP-Protocol-Version and session first: the spec requires a
	// 400/404 for those, whatever the message, and a client must learn its
	// session is gone from the first message it sends, not the first request.
	if (req.ID == nil && req.Method != "") || (req.ID != nil && req.Method == "") {
		if isModernRequest(r, &req) {
			if !s.modernMessagePreflight(w, r, &req) {
				return
			}
		} else if _, ok := s.legacyPreflight(w, r, nil); !ok {
			return
		}
		if req.Method == "notifications/cancelled" {
			s.cancelHTTPRequest(r, req.Params)
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Modern-era (protocol revision 2026-07-28+) requests are detected from
	// the body's io.modelcontextprotocol/protocolVersion _meta claim (or a
	// Modern MCP-Protocol-Version header, see isModernRequest) and are
	// handled by a dedicated stateless path in modern.go. A request with
	// neither signal falls through to every line below completely unchanged.
	if isModernRequest(r, &req) {
		s.handleModernRequest(w, r, &req)
		return
	}

	// For non-initialize requests, validate MCP-Protocol-Version header and session.
	if req.Method != "initialize" {
		var ok bool
		if r, ok = s.legacyPreflight(w, r, req.ID); !ok {
			return
		}
	}

	r, release := s.trackHTTPRequest(r, &req)
	defer release()
	s.dispatchResponse(w, r, &req, legacyShape)
}

// decodeMCPRequest parses one JSON-RPC message, keeping integer ids exact
// (a float64 would round ids above 2^53) and enforcing the base protocol's
// id rules: an id, when present, is a string or an integer and never null.
// Params are decoded as ordinary JSON values (numbers as float64), which is
// what tool handlers and ToolRequest expect. The returned error is the
// JSON-RPC error to answer with; req.ID is set when it could be read.
func decodeMCPRequest(raw []byte, req *MCPRequest) *MCPError {
	var wire struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  any             `json:"params"`
	}
	if !json.Valid(raw) {
		return &MCPError{Code: ErrorCodeParseError, Message: "Parse error", Data: map[string]any{"details": "body is not valid JSON"}}
	}
	if len(raw) == 0 || raw[0] != '{' {
		return &MCPError{Code: ErrorCodeInvalidRequest, Message: "Invalid Request", Data: map[string]any{"details": "a JSON-RPC message is a JSON object"}}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&wire); err != nil {
		return &MCPError{Code: ErrorCodeInvalidRequest, Message: "Invalid Request", Data: map[string]any{"details": "malformed JSON-RPC message"}}
	}
	if dec.More() {
		return &MCPError{Code: ErrorCodeParseError, Message: "Parse error", Data: map[string]any{"details": "unexpected data after the JSON-RPC message"}}
	}
	req.JSONRPC, req.Method, req.Params = wire.JSONRPC, wire.Method, wire.Params
	if len(wire.ID) > 0 {
		id, ok := decodeJSONRPCID(wire.ID)
		if !ok {
			return &MCPError{Code: ErrorCodeInvalidRequest, Message: "Invalid Request", Data: map[string]any{
				"details": "id must be a string or an integer, and must not be null",
			}}
		}
		req.ID = id
	}
	if req.JSONRPC != "2.0" {
		return &MCPError{Code: ErrorCodeInvalidRequest, Message: "Invalid Request", Data: map[string]any{
			"details": "JSONRPC field must be '2.0'",
		}}
	}
	if req.Method == "" && req.ID == nil {
		return &MCPError{Code: ErrorCodeInvalidRequest, Message: "Invalid Request", Data: map[string]any{
			"details": "a request or notification needs a method; a response needs an id",
		}}
	}
	return nil
}

// decodeJSONRPCID accepts a string or integer id (as json.Number, echoed
// back exactly), rejecting null, fractional numbers, objects and arrays.
func decodeJSONRPCID(raw json.RawMessage) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	switch id := v.(type) {
	case string:
		return id, true
	case json.Number:
		if _, err := id.Int64(); err != nil {
			return nil, false
		}
		return id, true
	}
	return nil, false
}

// legacyPreflight applies the checks every Legacy message but initialize
// must pass before it is acted on: a supported MCP-Protocol-Version header
// (400 otherwise, with a JSON-RPC body naming the supported versions) and,
// when session management is on, a valid MCP-Session-Id (400 when missing,
// 404 when unknown or expired). ok is false once the error has been
// written. The returned request carries the session's show-all flag.
func (s *Server) legacyPreflight(w http.ResponseWriter, r *http.Request, id any) (*http.Request, bool) {
	// Per spec: assume 2025-03-26 if missing for backwards compatibility
	protocolVersion, versionOK := negotiateProtocolVersion(r.Header.Get(headerProtocolVersion), "2025-03-26")

	// Still a 400 (this rejects a header, not a JSON-RPC method call, so it
	// stays outside the "Legacy JSON-RPC errors are always 200" convention),
	// with a proper JSON-RPC error body naming what this server supports.
	// writeModernProtocolError just means "JSON-RPC error at this HTTP
	// status" despite the name — nothing about it is Modern-specific.
	if !versionOK {
		s.writeModernProtocolError(w, id, http.StatusBadRequest, ErrorCodeInvalidParams,
			fmt.Sprintf("Unsupported MCP-Protocol-Version: %s", protocolVersion), map[string]any{
				"requested": protocolVersion,
				"supported": supportedProtocolVersions,
			})
		return r, false
	}

	if showAll, ok, status, message := s.checkSession(r.Context(), r); !ok {
		http.Error(w, message, status)
		return r, false
	} else if showAll {
		r = r.WithContext(WithShowAllTools(r.Context()))
	} else if sm := s.getSessionManager(); sm == nil {
		// No session management - check header/query on each request
		if GetShowAllFromRequest(r) {
			r = r.WithContext(WithShowAllTools(r.Context()))
		}
	}
	return r, true
}

// batchProtocolVersions are the Legacy revisions whose Streamable HTTP
// transport lets a client POST a JSON-RPC batch; 2025-06-18 removed it.
var batchProtocolVersions = map[string]bool{"2025-03-26": true}

// handleLegacyBatch serves a JSON-RPC batch (a JSON array) for the revisions
// that allow one: each request is dispatched in turn and the responses are
// returned as an array, or 202 when the batch holds only notifications
// (and responses). Later revisions get 400 Invalid Request, since a batch is
// input the server cannot accept there.
func (s *Server) handleLegacyBatch(w http.ResponseWriter, r *http.Request, raw []byte) {
	var ok bool
	if r, ok = s.legacyPreflight(w, r, nil); !ok {
		return
	}
	version, _ := negotiateProtocolVersion(r.Header.Get(headerProtocolVersion), "2025-03-26")
	if !batchProtocolVersions[version] {
		s.writeModernProtocolError(w, nil, http.StatusBadRequest, ErrorCodeInvalidRequest, "Invalid Request", map[string]any{
			"details": "JSON-RPC batching is not supported in protocol version " + version,
		})
		return
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		s.writeModernProtocolError(w, nil, http.StatusBadRequest, ErrorCodeParseError, "Parse error", map[string]any{"details": err.Error()})
		return
	}
	if len(items) == 0 {
		s.writeModernProtocolError(w, nil, http.StatusBadRequest, ErrorCodeInvalidRequest, "Invalid Request", map[string]any{"details": "empty batch"})
		return
	}

	responses := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		var req MCPRequest
		if perr := decodeMCPRequest(bytes.TrimSpace(item), &req); perr != nil {
			body, _ := json.Marshal(MCPResponse{JSONRPC: "2.0", ID: req.ID, Error: perr})
			responses = append(responses, body)
			continue
		}
		if isModernRequest(nil, &req) {
			body, _ := json.Marshal(MCPResponse{JSONRPC: "2.0", ID: req.ID, Error: &MCPError{Code: ErrorCodeInvalidRequest, Message: "Invalid Request",
				Data: map[string]any{"details": "a 2026-07-28 request cannot be sent in a batch"}}})
			responses = append(responses, body)
			continue
		}
		if req.ID == nil || req.Method == "" {
			continue // notification or response: accepted, nothing to answer
		}
		capture := newModernResponseCapture()
		s.dispatchMethod(capture, r, &req)
		if sid := capture.Header().Get(headerSessionID); sid != "" {
			w.Header().Set(headerSessionID, sid) // initialize inside a batch
		}
		responses = append(responses, bytes.TrimSpace(capture.body.Bytes()))
	}
	if len(responses) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(responses)
}

// dispatchMethod routes a parsed request to its handler. Both the Legacy path
// above and the Modern path (handleModernRequest, in modern.go — via a
// response-capturing writer so it can reshape the result afterward) call this
// same table, so a method's behavior is defined exactly once regardless of
// which era's request reached it.
// dispatchableMethods lists the methods handleModernRequest (modern.go) may
// dispatch through dispatchMethod's shared switch below, so it can
// distinguish an unknown RPC method (which the 2026-07-28 revision requires
// HTTP 404 for) from an ordinary method-level JSON-RPC error like an unknown
// tool name (which stays 200, per the Streamable HTTP spec's requirement
// that only routing failures — unknown method, unsupported version, header
// mismatch — get a non-200 status).
//
// Deliberately narrower than dispatchMethod's own case list: "initialize"
// and "ping" are both dispatchable there for Legacy clients, but the
// 2026-07-28 revision removes both (stateless per-request replaces the
// initialize handshake; ping is gone outright) — a Modern request naming
// either is exactly as unknown as a method this server never supported at
// all, and must 404 the same way.
var dispatchableMethods = map[string]bool{
	"tools/list":               true,
	"tools/call":               true,
	"resources/list":           true,
	"resources/read":           true,
	"resources/templates/list": true,
	"prompts/list":             true,
	"prompts/get":              true,
	"skills/list":              true,
	"skills/get":               true,
}

func (s *Server) dispatchMethod(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	switch req.Method {
	case "initialize":
		s.handleInitialize(w, r, req)
	case "ping":
		s.handlePing(w, r, req)
	case "tools/list":
		s.handleToolsList(w, r, req)
	case "tools/call":
		s.handleToolsCall(w, r, req)
	case "resources/list":
		s.handleResourcesList(w, r, req)
	case "resources/read":
		s.handleResourcesRead(w, r, req)
	case "resources/templates/list":
		s.handleResourcesTemplatesList(w, r, req)
	case "prompts/list":
		s.handlePromptsList(w, r, req)
	case "prompts/get":
		s.handlePromptsGet(w, r, req)
	case "skills/list":
		s.handleSkillsList(w, r, req)
	case "skills/get":
		s.handleSkillsGet(w, r, req)
	default:
		s.sendMCPError(w, req.ID, ErrorCodeMethodNotFound, "Method not found", map[string]any{
			"method": req.Method,
		})
	}
}

// negotiateProtocolVersion resolves the protocol version for one request:
// requested when non-empty, fallback when empty. ok is false when the
// requested version is not supported; callers render their own era- and
// transport-appropriate error.
func negotiateProtocolVersion(requested, fallback string) (version string, ok bool) {
	if requested == "" {
		return fallback, true
	}
	return requested, isSupportedProtocolVersion(requested)
}

// negotiateInitializeVersion implements Legacy initialize version negotiation
// (2025-11-25 lifecycle §Version Negotiation): a supported requested version
// is echoed back; for any other protocol version "the server MUST respond
// with another protocol version it supports", which SHOULD be the latest —
// so an unknown version (a newer Legacy revision, or the Modern label sent
// through initialize by mistake) negotiates down to MCPProtocolVersionLatest
// and the client decides whether it can speak that. ok is false only for a
// value that is not a protocol version at all (not YYYY-MM-DD), which is
// rejected with the spec's own "Unsupported protocol version" error example.
// An empty value means the client did not say; the latest is used.
func negotiateInitializeVersion(requested string) (version string, ok bool) {
	requested = strings.TrimSpace(requested)
	switch {
	case requested == "":
		return MCPProtocolVersionLatest, true
	case isSupportedProtocolVersion(requested):
		return requested, true
	case isProtocolVersionLabel(requested):
		return MCPProtocolVersionLatest, true
	default:
		return "", false
	}
}

// isProtocolVersionLabel reports whether v has the YYYY-MM-DD shape MCP
// protocol revisions use.
func isProtocolVersionLabel(v string) bool {
	_, err := time.Parse("2006-01-02", v)
	return err == nil
}

// isSupportedProtocolVersion checks if the given version string matches one of the
// supported MCP protocol versions. Protocol versions follow ISO date format (YYYY-MM-DD).
// Leading/trailing whitespace is trimmed before comparison.
func isSupportedProtocolVersion(version string) bool {
	version = strings.TrimSpace(version)
	for _, supported := range supportedProtocolVersions {
		if supported == version {
			return true
		}
	}
	return false
}

// hasDiscoverableToolsNow reports whether the server currently has any discoverable
// tools, either statically registered or exposed by a context-scoped ToolProvider.
func (s *Server) hasDiscoverableToolsNow(ctx context.Context) bool {
	s.mu.RLock()
	hasStatic := s.hasDiscoverableTools
	s.mu.RUnlock()
	return hasStatic || hasDiscoverableToolsFromProviders(ctx)
}

// appendDiscoveryInstructions appends guidance about the discovery tools to whatever
// instructions the server already has (via SetInstructions), without overwriting them.
// This is how a model learns about tool_search/execute_tool from the initialize
// response, which is a stronger, session-wide signal than a tool's own description.
func appendDiscoveryInstructions(instructions string) string {
	hint := fmt.Sprintf("This server has additional tools not shown in the initial tool list - call %s to find them, then %s to invoke them.", ToolSearchName, ExecuteToolName)
	if instructions == "" {
		return hint
	}
	return instructions + "\n\n" + hint
}

func (s *Server) handleInitialize(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	var params initializeParams
	if err := s.parseParams(req, &params); err != nil {
		s.sendMCPError(w, req.ID, ErrorCodeInvalidParams, "Invalid params", nil)
		return
	}

	// Determine which protocol version to use
	protocolVersion, ok := negotiateInitializeVersion(params.ProtocolVersion)
	if !ok {
		s.sendMCPError(w, req.ID, ErrorCodeInvalidParams, "Unsupported protocol version", map[string]any{
			"requested": params.ProtocolVersion,
			"supported": supportedProtocolVersions,
		})
		return
	}

	// Check for show-all flag from header or query param
	showAll := GetShowAllFromRequest(r)

	// Read instructions and icons under lock
	s.mu.RLock()
	instructions := s.instructions
	icons := s.icons
	s.mu.RUnlock()

	// Discovery tools aren't shown in show-all mode, so don't tell the model to use them there.
	if !showAll && s.hasDiscoverableToolsNow(r.Context()) {
		instructions = appendDiscoveryInstructions(instructions)
	}

	// Remember the client's declared capabilities (e.g. capabilities.extensions)
	// so handlers can later check them via ClientCapabilities. See that method's
	// doc comment for the multi-client-HTTP caveat.
	s.mu.Lock()
	s.lastClientCapabilities = params.Capabilities
	s.mu.Unlock()

	result := initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    s.buildCapabilities(protocolVersion),
		ServerInfo: serverInfo{
			Name:    s.name,
			Version: s.version,
			Icons:   icons,
		},
		Instructions: instructions,
	}

	// Generate and store session if session management is enabled
	sm := s.getSessionManager()
	if sm != nil {
		sessionID, err := sm.CreateSession(r.Context(), protocolVersion, showAll)
		if err != nil {
			s.sendMCPError(w, req.ID, ErrorCodeInternalError, "Failed to create session", nil)
			return
		}

		// Set session ID header
		w.Header().Set(headerSessionID, sessionID)
	}

	s.sendMCPResponse(w, req.ID, result)
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	s.sendMCPResponse(w, req.ID, map[string]any{})
}

func (s *Server) buildCapabilities(protocolVersion string) capabilities {
	caps := capabilities{
		Tools: map[string]any{},
	}

	// Every supported Legacy revision (2025-03-26 and later) has the same
	// capability shape.
	caps.Tools = map[string]any{
		"listChanged": true,
	}
	caps.Resources = map[string]any{
		"subscribe":   false,
		"listChanged": true,
	}
	caps.Prompts = map[string]any{
		"listChanged": true,
	}
	s.mu.RLock()
	if len(s.extensionCapabilities) > 0 {
		caps.Extensions = make(map[string]any, len(s.extensionCapabilities))
		for id, settings := range s.extensionCapabilities {
			caps.Extensions[id] = settings
		}
	}
	s.mu.RUnlock()

	return caps
}

// DeclareExtension advertises this server's support for an MCP extension (per
// SEP-1724) in its initialize response, under capabilities.extensions[id].
// Call it during setup, before serving. For example, a server offering MCP
// Apps (SEP-1865) tools would declare:
//
//	server.DeclareExtension(mcp.UIAppsExtensionID, map[string]any{
//		"mimeTypes": []string{mcp.UIAppMimeType},
//	})
func (s *Server) DeclareExtension(id string, settings map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.extensionCapabilities == nil {
		s.extensionCapabilities = map[string]any{}
	}
	s.extensionCapabilities[id] = settings
}

// ClientCapabilities returns the capabilities object (capabilities.extensions,
// roots, sampling, etc., as sent in initialize) declared by the most recently
// initialized client.
//
// For a stdio server — one process per client connection, the common case for
// locally-spawned MCP servers — this reliably reflects the single connected
// client. An HTTP server handling multiple concurrent client sessions should
// not rely on this for per-request decisions: it is a single process-wide
// value reflecting only the most recent initialize seen across all sessions,
// not any particular request's caller. This is deliberately not upgraded to
// per-session tracking here: doing so with an in-memory map would silently
// misbehave in exactly the deployment this library optimizes an HTTP server
// for — horizontal scaling behind a load balancer with [JWTSessionManager],
// where a later request from the same client can land on a different,
// stateless instance that never saw that client's initialize. If you need
// per-request extension awareness on HTTP, the robust option is to register
// UI-linked tools unconditionally (the [UIAppsExtensionID] cost to a
// non-supporting host is nil — it just ignores unknown `_meta`) rather than
// branching on this method. Use [SupportsUIApps] to interpret the MCP Apps
// extension's settings from the returned map.
func (s *Server) ClientCapabilities() map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastClientCapabilities
}

func (s *Server) handleToolsList(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	if cursorIn(req.Params) {
		s.sendMCPError(w, req.ID, errInvalidCursor.Code, errInvalidCursor.Message, nil)
		return
	}
	tools := s.ListToolsWithContext(r.Context())
	result := map[string]any{
		"tools": tools,
	}
	s.sendMCPResponse(w, req.ID, result)
}

func (s *Server) handleToolsCall(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	var params ToolCallParams
	if err := s.parseParams(req, &params); err != nil {
		s.sendProtocolAwareError(w, r, req, req.ID, ErrorCodeInvalidParams, "Invalid params", nil)
		return
	}

	response, err := s.CallTool(r.Context(), params.Name, params.Arguments)
	result, wireErr := s.toolsCallResult(r.Context(), params.Name, response, err, !isModernRequest(r, req))
	if wireErr != nil {
		s.sendMCPError(w, req.ID, wireErr.Code, wireErr.Message, wireErr.Data)
		return
	}
	s.sendMCPResponse(w, req.ID, result)
}

// checkSession validates the MCP-Session-Id header when session management
// is enabled. ok=false means the request is rejected: status and message
// carry the HTTP error for the caller to render. showAll reports the
// session's stored show-all flag (the SSE stream deliberately ignores it:
// its notifications are broadcast, not session-scoped). With no session
// manager configured the request is anonymous and always passes.
func (s *Server) checkSession(ctx context.Context, r *http.Request) (showAll, ok bool, status int, message string) {
	sm := s.getSessionManager()
	if sm == nil {
		return false, true, 0, ""
	}
	sessionID := r.Header.Get(headerSessionID)
	if sessionID == "" {
		return false, false, http.StatusBadRequest, "MCP-Session-Id header required"
	}
	valid, err := sm.ValidateSession(ctx, sessionID)
	if err != nil {
		return false, false, http.StatusInternalServerError, fmt.Sprintf("Session validation error: %v", err)
	}
	if !valid {
		return false, false, http.StatusNotFound, "Session not found"
	}
	showAll, _ = sm.GetShowAll(ctx, sessionID)
	return showAll, true, 0, ""
}

func (s *Server) sendMCPResponse(w http.ResponseWriter, id any, result any) {
	s.writeMCPResponse(w, http.StatusOK, id, result)
}

// writeMCPResponse writes a JSON-RPC result at the given HTTP status. Legacy
// JSON-RPC is always 200; the Modern era maps routing failures to 4xx — one
// body, both conventions.
func (s *Server) writeMCPResponse(w http.ResponseWriter, status int, id any, result any) {
	response := MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}

func (s *Server) parseParams(req *MCPRequest, target any) error {
	if req.Params == nil {
		return nil
	}
	paramsBytes, err := json.Marshal(req.Params)
	if err != nil {
		return err
	}
	return json.Unmarshal(paramsBytes, target)
}

// sendProtocolAwareError reports a garbled/malformed request — one that
// failed to decode, didn't carry a valid JSON-RPC envelope, or whose params
// couldn't be unmarshaled into the shape a method expects — as opposed to a
// well-formed request whose particular method/params turned out to be
// semantically invalid (an unknown tool, a missing-but-parseable required
// field, and the like), which correctly stays a 200 JSON-RPC error in both
// eras per the spec's ordinary-method-error convention (see
// modernShape's doc comment).
//
// A Modern-era request gets the spec-required HTTP 400 for this class of
// error; Legacy keeps its always-200 JSON-RPC convention, unchanged. Era is
// detected the same way isModernRequest does (req may be a zero-value or
// partially-decoded MCPRequest here — that detection's header check doesn't
// depend on req having decoded successfully).
func (s *Server) sendProtocolAwareError(w http.ResponseWriter, r *http.Request, req *MCPRequest, id any, code int, message string, data any) {
	if isModernRequest(r, req) {
		s.writeModernProtocolError(w, id, http.StatusBadRequest, code, message, data)
		return
	}
	// A body that is not a JSON-RPC message at all (parse error, invalid
	// request) is input the server cannot accept: the Streamable HTTP
	// transport has that answered with an HTTP error status, as the
	// official SDKs do, rather than the 200 that carries a method's result.
	if code == ErrorCodeParseError || code == ErrorCodeInvalidRequest {
		s.writeModernProtocolError(w, id, http.StatusBadRequest, code, message, data)
		return
	}
	s.sendMCPError(w, id, code, message, data)
}

func (s *Server) sendMCPError(w http.ResponseWriter, id any, code int, message string, data any) {
	// Always 200 for JSON-RPC responses
	s.writeMCPError(w, http.StatusOK, id, code, message, data)
}

// writeMCPError writes a JSON-RPC error at the given HTTP status; see
// writeMCPResponse for the status convention.
func (s *Server) writeMCPError(w http.ResponseWriter, status int, id any, code int, message string, data any) {
	response := MCPResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error: &MCPError{
			Code:    code,
			Message: message,
			Data:    data,
		},
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(response)
}

// handleSkillsList answers skills/list with the registered skills plus any
// from SkillProviders on the request context.
func (s *Server) handleSkillsList(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	if cursorIn(req.Params) {
		s.sendMCPError(w, req.ID, errInvalidCursor.Code, errInvalidCursor.Message, nil)
		return
	}
	s.sendMCPResponse(w, req.ID, map[string]any{"skills": s.ListSkillsWithContext(r.Context())})
}

// handleSkillsGet answers skills/get with one skill's entry by URI: the
// SKILL.md URI or the skill's root directory URI. Content is read with
// resources/read like any other resource.
func (s *Server) handleSkillsGet(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	var params struct {
		URI string `json:"uri"`
	}
	if err := s.parseParams(req, &params); err != nil {
		s.sendProtocolAwareError(w, r, req, req.ID, ErrorCodeInvalidParams, "Invalid params", nil)
		return
	}
	if params.URI == "" {
		s.sendMCPError(w, req.ID, ErrorCodeInvalidParams, "uri parameter is required", nil)
		return
	}
	skill, ok := s.GetSkillWithContext(r.Context(), params.URI)
	if !ok {
		s.sendMCPError(w, req.ID, ErrorCodeInvalidParams, fmt.Sprintf("Skill not found: %s", params.URI), map[string]any{"uri": params.URI})
		return
	}
	s.sendMCPResponse(w, req.ID, map[string]any{"skill": skill})
}

// modernMessagePreflight applies the 2026-07-28 header rules to a Modern
// notification (every POST must carry MCP-Protocol-Version matching the
// _meta claim, and Mcp-Method matching the method) before it is accepted;
// a failure is answered 400 the way handleModernRequest answers a request.
func (s *Server) modernMessagePreflight(w http.ResponseWriter, r *http.Request, req *MCPRequest) bool {
	_, meta := modernRequestParams(req)
	claim, _ := meta[metaKeyProtocolVersion].(string)
	headerVersion := r.Header.Get(headerProtocolVersion)
	if len(r.Header.Values(headerProtocolVersion)) > 1 || len(r.Header.Values(headerMcpMethod)) > 1 {
		s.writeModernProtocolError(w, nil, http.StatusBadRequest, ErrorCodeHeaderMismatch, "Header mismatch: multiple header values", nil)
		return false
	}
	switch {
	case headerVersion == "":
		s.writeModernProtocolError(w, nil, http.StatusBadRequest, ErrorCodeHeaderMismatch, "Header mismatch: MCP-Protocol-Version header is required", nil)
	case headerVersion != claim:
		s.writeModernProtocolError(w, nil, http.StatusBadRequest, ErrorCodeHeaderMismatch,
			fmt.Sprintf("Header mismatch: MCP-Protocol-Version header value %q does not match body value %q", headerVersion, claim), nil)
	case !isSupportedModernProtocolVersion(claim):
		s.writeModernProtocolError(w, nil, http.StatusBadRequest, ErrorCodeUnsupportedProtocolVersion, "Unsupported protocol version",
			map[string]any{"supported": allSupportedProtocolVersions(), "requested": claim})
	case req.Method != "" && r.Header.Get(headerMcpMethod) == "":
		s.writeModernProtocolError(w, nil, http.StatusBadRequest, ErrorCodeHeaderMismatch, "Header mismatch: Mcp-Method header is required", nil)
	case req.Method != "" && r.Header.Get(headerMcpMethod) != req.Method:
		s.writeModernProtocolError(w, nil, http.StatusBadRequest, ErrorCodeHeaderMismatch,
			fmt.Sprintf("Header mismatch: Mcp-Method header value %q does not match body method %q", r.Header.Get(headerMcpMethod), req.Method), nil)
	default:
		return true
	}
	return false
}

// DefaultMaxRequestBytes bounds a POST body (and a stdio line) when
// SetMaxRequestBytes has not been called.
const DefaultMaxRequestBytes = 32 << 20

// SetMaxRequestBytes bounds the size of one inbound JSON-RPC message over
// HTTP (413 beyond it) and stdio; 0 restores DefaultMaxRequestBytes.
func (s *Server) SetMaxRequestBytes(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxRequest = n
}

func (s *Server) maxRequestBytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.maxRequest <= 0 {
		return DefaultMaxRequestBytes
	}
	return s.maxRequest
}

// httpInflightKey identifies an in-flight Legacy HTTP request for
// notifications/cancelled: its JSON-RPC id, scoped to the session when
// there is one (anonymous requests share a scope).
func httpInflightKey(r *http.Request, id any) string {
	raw, _ := json.Marshal(id)
	return r.Header.Get(headerSessionID) + "|" + string(raw)
}

// trackHTTPRequest derives a cancellable context for a Legacy request so a
// later notifications/cancelled naming its id can stop the handler
// (2025-11-25 utilities/cancellation). The returned release must be called
// when the request completes.
func (s *Server) trackHTTPRequest(r *http.Request, req *MCPRequest) (*http.Request, func()) {
	ctx, cancel := context.WithCancel(r.Context())
	key := httpInflightKey(r, req.ID)
	s.mu.Lock()
	if s.httpInflight == nil {
		s.httpInflight = map[string]context.CancelFunc{}
	}
	s.httpInflight[key] = cancel
	s.mu.Unlock()
	return r.WithContext(ctx), func() {
		s.mu.Lock()
		delete(s.httpInflight, key)
		s.mu.Unlock()
		cancel()
	}
}

// cancelHTTPRequest acts on a notifications/cancelled: the named request's
// context is cancelled; its handler decides what that means (a tool that
// honours its context stops and its result reports the cancellation).
func (s *Server) cancelHTTPRequest(r *http.Request, params any) {
	p, _ := params.(map[string]any)
	id, ok := p["requestId"]
	if !ok {
		return
	}
	if n, isNum := id.(float64); isNum && n == float64(int64(n)) {
		id = json.Number(fmt.Sprintf("%d", int64(n)))
	}
	key := httpInflightKey(r, id)
	s.mu.RLock()
	cancel := s.httpInflight[key]
	s.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}
