package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Modern detection against a real Dual-era *Server ---------------------

func TestClient_ModernDetectionAndRoundTrip(t *testing.T) {
	s := NewServer("modern-rt", "1")
	s.RegisterTool(NewTool("upper", "to upper", String("s", "s", Required())), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		v, _ := req.String("s")
		return NewToolResponseText(strings.ToUpper(v)), nil
	})

	var sawModernHeaders []string
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(headerMcpMethod) != "" {
			sawModernHeaders = append(sawModernHeaders, r.Header.Get(headerMcpMethod))
		}
		s.HandleRequest(w, r)
	})
	ts := httptest.NewServer(wrapped)
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	c.mu.RLock()
	era := c.era
	c.mu.RUnlock()
	if era != eraModern {
		t.Fatalf("era = %v, want eraModern (server supports server/discover)", era)
	}

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "upper" {
		t.Fatalf("unexpected tools: %+v", tools)
	}

	resp, err := c.CallTool(context.Background(), "upper", map[string]any{"s": "abc"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "ABC" {
		t.Fatalf("unexpected response: %+v", resp)
	}

	found := map[string]bool{}
	for _, m := range sawModernHeaders {
		found[m] = true
	}
	if !found["server/discover"] || !found["tools/list"] || !found["tools/call"] {
		t.Fatalf("expected Mcp-Method on discover/list/call, got %v", sawModernHeaders)
	}
}

// --- Fallback to Legacy against a genuine Legacy-only fake server ---------

// legacyOnlyServer simulates a real pre-2026-07-28 server: it has never heard
// of server/discover and rejects it exactly like an unrecognized method would
// on a Legacy Streamable HTTP server (a plain 400, no JSON-RPC error body) —
// the non-modern-error case the spec's backward-compat algorithm describes.
func legacyOnlyServer(t *testing.T, s *Server) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc MCPRequest
		_ = json.Unmarshal(body, &rpc)
		if rpc.Method == "server/discover" {
			http.Error(w, "unknown method", http.StatusBadRequest) // plain text, not JSON-RPC
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.HandleRequest(w, r)
	}))
}

func TestClient_FallsBackToLegacyWhenServerHasNoDiscover(t *testing.T) {
	s := NewServer("legacy-only", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	ts := legacyOnlyServer(t, s)
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	c.mu.RLock()
	era := c.era
	c.mu.RUnlock()
	if era != eraLegacy {
		t.Fatalf("era = %v, want eraLegacy (server/discover rejected with a plain, non-JSON-RPC 400)", era)
	}

	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
}

// TestClient_LenientUnknownMethodFakeDoesNotFalselyTriggerModern is a
// regression guard: a test fake (or a real, non-compliant server) that
// answers ANY unrecognized method with a generic empty-success envelope
// (`{"result":{}}`) must not be mistaken for a Modern server just because
// server/discover happened to return HTTP 200. Only a result that actually
// carries supportedVersions counts.
func TestClient_LenientUnknownMethodFakeDoesNotFalselyTriggerModern(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc MCPRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &rpc)
		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(MCPResponse{JSONRPC: "2.0", ID: rpc.ID, Result: map[string]any{
				"protocolVersion": MCPProtocolVersionLatest,
				"capabilities":    capabilities{Tools: map[string]any{}},
				"serverInfo":      serverInfo{Name: "lenient", Version: "1"},
			}})
		default:
			// Lenient fake: unknown methods (including our server/discover
			// probe) get a generic empty success, not a proper error.
			_ = json.NewEncoder(w).Encode(MCPResponse{JSONRPC: "2.0", ID: rpc.ID, Result: map[string]any{}})
		}
	})
	ts := httptest.NewServer(h)
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	c.mu.RLock()
	era := c.era
	c.mu.RUnlock()
	if era != eraLegacy {
		t.Fatalf("era = %v, want eraLegacy — a bare {} result must not count as a DiscoverResult", era)
	}
}

// --- stdio Modern detection -------------------------------------------

func TestClient_ModernDetectionStdio(t *testing.T) {
	s := NewServer("modern-stdio", "1")
	s.RegisterTool(NewTool("ping", "pings"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("pong"), nil
	})

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = s.ServeStream(context.Background(), serverReader, serverWriter)
	}()

	client := NewStreamClient(clientReader, clientWriter, "")
	defer func() {
		client.Close()
		clientWriter.Close()
		<-serveDone
		serverWriter.Close()
	}()

	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	client.mu.RLock()
	era := client.era
	client.mu.RUnlock()
	if era != eraModern {
		t.Fatalf("era = %v, want eraModern (stdio server/discover succeeds)", era)
	}

	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Fatalf("unexpected tools: %+v", tools)
	}
}

// --- Notifications work identically regardless of detected era ------------

func TestClient_NotificationsOverModernSubscription(t *testing.T) {
	s := NewServer("modern-notify", "1")
	s.RegisterTool(NewTool("a", "a"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("a"), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	var mu sync.Mutex
	fired := false
	c.OnToolsChanged(func() {
		mu.Lock()
		fired = true
		mu.Unlock()
	})
	c.EnableNotifications()
	defer c.Close()

	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	c.mu.RLock()
	era := c.era
	c.mu.RUnlock()
	if era != eraModern {
		t.Fatalf("era = %v, want eraModern", era)
	}

	waitForSubscribers(t, s, 1)
	s.NotifyToolsChanged()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		f := fired
		mu.Unlock()
		if f {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("OnToolsChanged callback never fired over the Modern subscriptions/listen reader")
}

// --- Modern protocol-level errors surface clearly --------------------------

func TestClient_ModernRequest_SurfacesProtocolError(t *testing.T) {
	s := NewServer("modern-err", "1")
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	c.mu.RLock()
	era := c.era
	c.mu.RUnlock()
	if era != eraModern {
		t.Fatalf("era = %v, want eraModern", era)
	}

	// Force a version mismatch by sending a raw Modern request with a bogus
	// version through the low-level helper directly.
	req := c.withModernMeta(&MCPRequest{JSONRPC: "2.0", ID: "x", Method: "tools/list", Params: map[string]any{}})
	params := req.Params.(map[string]any)
	meta := params["_meta"].(map[string]any)
	meta[metaKeyProtocolVersion] = "1900-01-01"

	var resp MCPResponse
	if err := c.sendModernHTTPRequest(context.Background(), req, &resp, nil); err != nil {
		t.Fatalf("sendModernHTTPRequest: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected a JSON-RPC error for a version/header mismatch")
	}
	if resp.Error.Code != ErrorCodeHeaderMismatch {
		t.Errorf("error code = %d, want HeaderMismatch (%d): %+v", resp.Error.Code, ErrorCodeHeaderMismatch, resp.Error)
	}
}

// --- Modern detection retries with a server-advertised version ------------

// newerModernServer simulates a real server that only speaks a Modern
// protocol revision newer than what this client build knows about
// (MCPProtocolVersionModern): it rejects that version's server/discover
// probe with a genuine -32022 UnsupportedProtocolVersion error naming the
// version it does support, then accepts that named version on retry — and,
// critically, keeps enforcing it on every later request, so a client that
// only fixed up the initial handshake (without also using the negotiated
// version for subsequent requests) fails here too.
func newerModernServer(t *testing.T, newerVersion string) (*httptest.Server, *[]string) {
	t.Helper()
	var seenVersions []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc MCPRequest
		_ = json.Unmarshal(body, &rpc)
		version := r.Header.Get(headerProtocolVersion)
		seenVersions = append(seenVersions, version)

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if version != newerVersion {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(MCPResponse{
				JSONRPC: "2.0",
				ID:      rpc.ID,
				Error: &MCPError{
					Code:    ErrorCodeUnsupportedProtocolVersion,
					Message: "Unsupported protocol version",
					Data:    map[string]any{"supported": []string{newerVersion}, "requested": version},
				},
			})
			return
		}

		switch rpc.Method {
		case "server/discover":
			_ = json.NewEncoder(w).Encode(MCPResponse{JSONRPC: "2.0", ID: rpc.ID, Result: map[string]any{
				"resultType":        "complete",
				"supportedVersions": []string{newerVersion},
			}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(MCPResponse{JSONRPC: "2.0", ID: rpc.ID, Result: map[string]any{
				"resultType": "complete",
				"tools":      []MCPTool{},
			}})
		default:
			_ = json.NewEncoder(w).Encode(MCPResponse{JSONRPC: "2.0", ID: rpc.ID, Result: map[string]any{"resultType": "complete"}})
		}
	}))
	return ts, &seenVersions
}

// TestClient_ModernDetection_RetriesWithServerAdvertisedVersion is the
// regression test for the reported bug: a server answering "I speak an even
// newer version" to the initial discover probe must not be mistaken for a
// Legacy server. The client should retry discover with the version the
// server named, succeed, and keep using that negotiated version (not
// MCPProtocolVersionModern) on every later request.
func TestClient_ModernDetection_RetriesWithServerAdvertisedVersion(t *testing.T) {
	const newerVersion = "2027-01-01"
	ts, seenVersions := newerModernServer(t, newerVersion)
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	c.mu.RLock()
	era := c.era
	c.mu.RUnlock()
	if era != eraModern {
		t.Fatalf("era = %v, want eraModern — a server that answers with a Modern-shaped UnsupportedProtocolVersion error is not a Legacy server", era)
	}
	if c.ProtocolVersion() != newerVersion {
		t.Fatalf("ProtocolVersion() = %q, want %q (the version the server actually accepted)", c.ProtocolVersion(), newerVersion)
	}
	if len(*seenVersions) < 2 || (*seenVersions)[0] != MCPProtocolVersionModern || (*seenVersions)[1] != newerVersion {
		t.Fatalf("expected discover to probe %q then retry with %q, got %v", MCPProtocolVersionModern, newerVersion, *seenVersions)
	}
	afterInit := len(*seenVersions)

	// A later request must also use the negotiated version, not the
	// client's default — otherwise every request after a successful
	// negotiated handshake would immediately fail the same way discover
	// almost did. Only requests from here on are checked: the discover
	// probe's intentional first attempt at MCPProtocolVersionModern,
	// asserted above, is expected to NOT match newerVersion.
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, v := range (*seenVersions)[afterInit:] {
		if v != newerVersion {
			t.Fatalf("some post-init request used version %q instead of the negotiated %q: %v", v, newerVersion, *seenVersions)
		}
	}
}

// TestClient_ModernDetection_NoUsableModernVersionIsAnError proves the
// retry is bounded and, when the server's UnsupportedProtocolVersion error
// offers nothing new to try (it only re-lists the Modern version already
// rejected), Initialize fails instead of falling back to Legacy: the error
// is a recognized Modern error, so the server speaks a Modern revision and
// the client must not send initialize (2026-07-28 Streamable HTTP
// §Backward Compatibility).
func TestClient_ModernDetection_NoUsableModernVersionIsAnError(t *testing.T) {
	var sawInitialize atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc MCPRequest
		_ = json.Unmarshal(body, &rpc)
		if rpc.Method == "initialize" {
			sawInitialize.Store(true)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(MCPResponse{
			JSONRPC: "2.0",
			ID:      rpc.ID,
			Error: &MCPError{
				Code:    ErrorCodeUnsupportedProtocolVersion,
				Message: "Unsupported protocol version",
				Data:    map[string]any{"supported": []string{MCPProtocolVersionModern}, "requested": MCPProtocolVersionModern},
			},
		})
	}))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	err := c.Initialize(context.Background())
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != ErrorCodeUnsupportedProtocolVersion {
		t.Fatalf("Initialize err = %v, want the server's UnsupportedProtocolVersion error", err)
	}
	if sawInitialize.Load() {
		t.Fatal("client fell back to initialize against a Modern server")
	}
}

// TestPickRetryModernVersion pins the two-tier retry-version choice from an
// UnsupportedProtocolVersion error's data.supported: prefer a version this
// client speaks Modern, else a newer dated Modern revision (label
// negotiation); never a Legacy-dated version — that means fall back to the
// Legacy initialize path instead of a doomed retry.
func TestPickRetryModernVersion(t *testing.T) {
	list := func(vs ...string) map[string]any {
		anyList := make([]any, len(vs))
		for i, v := range vs {
			anyList[i] = v
		}
		return map[string]any{"supported": anyList}
	}

	if v, ok := pickRetryModernVersion(list(MCPProtocolVersionModern), "1900-01-01"); !ok || v != MCPProtocolVersionModern {
		t.Errorf("known Modern version: got (%q, %v), want (%q, true)", v, ok, MCPProtocolVersionModern)
	}
	if v, ok := pickRetryModernVersion(list("2027-03-26"), MCPProtocolVersionModern); !ok || v != "2027-03-26" {
		t.Errorf("newer revision: got (%q, %v), want (\"2027-03-26\", true)", v, ok)
	}
	// The dual-era shape this server itself now emits: after trying the
	// Modern version, only Legacy-dated entries remain — no retry.
	if _, ok := pickRetryModernVersion(list(MCPProtocolVersionModern, "2025-11-25", "2024-11-05"), MCPProtocolVersionModern); ok {
		t.Error("legacy-only remainder: want ok=false (Legacy fallback), got a retry version")
	}
	if _, ok := pickRetryModernVersion(nil, MCPProtocolVersionModern); ok {
		t.Error("nil data: want ok=false")
	}
	if _, ok := pickRetryModernVersion(map[string]any{}, MCPProtocolVersionModern); ok {
		t.Error("missing supported list: want ok=false")
	}
}
