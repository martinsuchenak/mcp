package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/paularlott/jsonrpc"
)

// --- test helpers -----------------------------------------------------

// modernRequest builds a fully-valid Modern-era HTTP request for method with
// the given params (name/uri, if any, must already be set on params so the
// Mcp-Name header can be derived from it).
func modernRequest(t *testing.T, url, method string, params map[string]any) *http.Request {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	name, needsName := modernRequestName(method, params)
	params["_meta"] = map[string]any{
		metaKeyProtocolVersion:    MCPProtocolVersionModern,
		metaKeyClientCapabilities: map[string]any{},
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      float64(1),
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerMcpMethod, method)
	req.Header.Set(headerProtocolVersion, MCPProtocolVersionModern)
	if needsName {
		req.Header.Set(headerMcpName, encodeModernHeaderValue(name))
	}
	return req
}

func decodeMCPResponse(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode response: %v, body: %s", err, body)
	}
	return out
}

// --- detection ----------------------------------------------------------

func TestIsModernRequest(t *testing.T) {
	t.Run("legacy request has neither signal", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		req := &MCPRequest{Method: "tools/list", Params: map[string]any{}}
		if isModernRequest(r, req) {
			t.Fatal("expected false for a plain legacy request")
		}
	})

	t.Run("Mcp-Method header alone is not a modern signal", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set(headerMcpMethod, "tools/list")
		req := &MCPRequest{Method: "tools/list", Params: map[string]any{}}
		if isModernRequest(r, req) {
			t.Fatal("expected false: Mcp-Method is mirrored by Legacy clients too (mcp-remote)")
		}
	})

	t.Run("Mcp-Method with a Legacy MCP-Protocol-Version is legacy", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set(headerMcpMethod, "tools/call")
		r.Header.Set(headerProtocolVersion, "2025-11-25")
		req := &MCPRequest{Method: "tools/call", Params: map[string]any{"name": "t"}}
		if isModernRequest(r, req) {
			t.Fatal("expected false: a Legacy client that also sends Mcp-Method")
		}
	})

	t.Run("initialize with Mcp-Method and no version header is legacy", func(t *testing.T) {
		// mcp-remote's default (legacy) mode forwards initialize with
		// Mcp-Method: initialize and no MCP-Protocol-Version header (the
		// version is only known once initialize has been answered).
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set(headerMcpMethod, "initialize")
		req := &MCPRequest{Method: "initialize", Params: map[string]any{"protocolVersion": "2025-11-25"}}
		if isModernRequest(r, req) {
			t.Fatal("expected false for a Legacy initialize")
		}
	})

	t.Run("Modern MCP-Protocol-Version header without a _meta claim is modern (so it is refused)", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set(headerMcpMethod, "tools/list")
		r.Header.Set(headerProtocolVersion, MCPProtocolVersionModern)
		req := &MCPRequest{Method: "tools/list", Params: map[string]any{}}
		if !isModernRequest(r, req) {
			t.Fatal("expected true: a Modern header is a Modern request, even a malformed one")
		}
	})

	t.Run("_meta claim naming a Legacy revision is legacy", func(t *testing.T) {
		req := &MCPRequest{
			Method: "tools/list",
			Params: map[string]any{"_meta": map[string]any{metaKeyProtocolVersion: "2025-11-25"}},
		}
		if isModernRequest(nil, req) {
			t.Fatal("expected false: the claim classifies into the era of the named revision")
		}
	})

	t.Run("_meta claim naming an unknown revision is modern (gets UnsupportedProtocolVersion)", func(t *testing.T) {
		req := &MCPRequest{
			Method: "tools/list",
			Params: map[string]any{"_meta": map[string]any{metaKeyProtocolVersion: "2099-01-01"}},
		}
		if !isModernRequest(nil, req) {
			t.Fatal("expected true")
		}
	})

	t.Run("_meta.protocolVersion alone is a modern signal (e.g. stdio)", func(t *testing.T) {
		req := &MCPRequest{
			Method: "tools/list",
			Params: map[string]any{"_meta": map[string]any{metaKeyProtocolVersion: MCPProtocolVersionModern}},
		}
		if !isModernRequest(nil, req) {
			t.Fatal("expected true when _meta.protocolVersion is present, even with a nil *http.Request")
		}
	})
}

// A Legacy client that adds the Modern per-request headers (mcp-remote and
// Claude Code send Mcp-Method and Mcp-Name with MCP-Protocol-Version
// 2025-11-25 and no _meta version) must be served on the Legacy path, not
// refused with a header/body version mismatch.
func TestLegacyClientWithMcpMethodHeader(t *testing.T) {
	s := NewServer("legacy-headers", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	srv := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer srv.Close()

	for _, tc := range []struct {
		method string
		params map[string]any
	}{
		// initialize goes out before the version is negotiated, so it
		// carries Mcp-Method but no MCP-Protocol-Version header at all.
		{"initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "mcp-remote", "version": "1"}}},
		{"tools/list", map[string]any{}},
		{"tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}},
	} {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": tc.method, "params": tc.params})
		req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if tc.method != "initialize" {
			req.Header.Set(headerProtocolVersion, "2025-11-25")
		}
		req.Header.Set(headerMcpMethod, tc.method)
		if name, ok := tc.params["name"].(string); ok {
			req.Header.Set(headerMcpName, name)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d, body %s", tc.method, resp.StatusCode, raw)
		}
		out := decodeMCPResponse(t, raw)
		if out["error"] != nil {
			t.Fatalf("%s: error %v", tc.method, out["error"])
		}
		if tc.method == "tools/call" && !strings.Contains(string(raw), `"ok"`) {
			t.Fatalf("tools/call: unexpected result %s", raw)
		}
	}
}

// --- server/discover ------------------------------------------------------

func TestServerDiscoverHTTP(t *testing.T) {
	s := NewServer("discoverable", "9.9")
	s.SetInstructions("read the docs")
	s.RegisterTool(NewTool("t", "a tool"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	req := modernRequest(t, ts.URL, "server/discover", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	out := decodeMCPResponse(t, body)
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %#v", out)
	}
	if result["resultType"] != "complete" {
		t.Errorf("resultType = %v, want complete", result["resultType"])
	}
	if result["instructions"] != "read the docs" {
		t.Errorf("instructions = %v", result["instructions"])
	}
	// The spec requires caching hints (ttlMs as a number) on server/discover;
	// a real client's schema validation rejects a response missing it.
	if ttlMs, ok := result["ttlMs"].(float64); !ok || ttlMs < 0 {
		t.Errorf("ttlMs = %#v, want a present, non-negative number", result["ttlMs"])
	}
	if result["cacheScope"] != "public" {
		t.Errorf("cacheScope = %v, want public", result["cacheScope"])
	}
	supported, _ := result["supportedVersions"].([]any)
	if len(supported) == 0 {
		t.Fatal("expected non-empty supportedVersions")
	}
	foundModern, foundLegacy := false, false
	for _, v := range supported {
		if v == MCPProtocolVersionModern {
			foundModern = true
		}
		if v == MCPProtocolVersionLatest {
			foundLegacy = true
		}
	}
	if !foundModern || !foundLegacy {
		t.Errorf("supportedVersions = %v, want both modern and legacy versions listed (dual-era)", supported)
	}
	meta, _ := result["_meta"].(map[string]any)
	serverInfo, _ := meta[metaKeyServerInfo].(map[string]any)
	if serverInfo["name"] != "discoverable" || serverInfo["version"] != "9.9" {
		t.Errorf("serverInfo = %#v", serverInfo)
	}
}

func TestServerDiscoverStdio(t *testing.T) {
	s := NewServer("stdio-discoverable", "1.0")

	clientReader, serverWriter := io.Pipe() // server -> client
	serverReader, clientWriter := io.Pipe() // client -> server

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = s.ServeStream(context.Background(), serverReader, serverWriter)
	}()

	transport := jsonrpc.NewStreamTransport(clientReader, clientWriter)
	rpc := jsonrpc.NewClient(transport)
	defer func() {
		clientWriter.Close()
		<-serveDone
		serverWriter.Close()
	}()

	var result map[string]any
	if err := rpc.Call(context.Background(), "server/discover", map[string]any{}, &result); err != nil {
		t.Fatalf("server/discover: %v", err)
	}
	supported, _ := result["supportedVersions"].([]any)
	found := false
	for _, v := range supported {
		if v == MCPProtocolVersionModern {
			found = true
		}
	}
	if !found {
		t.Errorf("supportedVersions = %v, want it to include %q", supported, MCPProtocolVersionModern)
	}
}

// TestStdioModernRequest_ResultShape closes the same gap the HTTP path had
// (a real client's strict schema validation rejects a tools/list result
// missing ttlMs): a stdio request whose raw params carry
// io.modelcontextprotocol/protocolVersion must get the same resultType/
// ttlMs/cacheScope/_meta.serverInfo shaping HTTP's modernShape
// applies, even though stdio has no headers to signal the era with.
func TestStdioModernRequest_ResultShape(t *testing.T) {
	s := NewServer("stdio-modern-shape", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = s.ServeStream(context.Background(), serverReader, serverWriter)
	}()
	transport := jsonrpc.NewStreamTransport(clientReader, clientWriter)
	rpc := jsonrpc.NewClient(transport)
	defer func() {
		clientWriter.Close()
		<-serveDone
		serverWriter.Close()
	}()

	modernParams := map[string]any{
		"_meta": map[string]any{
			metaKeyProtocolVersion:    MCPProtocolVersionModern,
			metaKeyClientCapabilities: map[string]any{},
		},
	}

	var listResult map[string]any
	if err := rpc.Call(context.Background(), "tools/list", modernParams, &listResult); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if listResult["resultType"] != "complete" {
		t.Errorf("resultType = %v, want complete", listResult["resultType"])
	}
	if ttlMs, ok := listResult["ttlMs"].(float64); !ok || ttlMs < 0 {
		t.Errorf("ttlMs = %#v, want a present, non-negative number", listResult["ttlMs"])
	}
	if listResult["cacheScope"] != "public" {
		t.Errorf("cacheScope = %v, want public", listResult["cacheScope"])
	}
	meta, _ := listResult["_meta"].(map[string]any)
	if meta[metaKeyServerInfo] == nil {
		t.Errorf("expected _meta.serverInfo, got %#v", listResult["_meta"])
	}

	// tools/call is not a cacheable list/read op — resultType/serverInfo yes,
	// ttlMs/cacheScope no.
	callParams := map[string]any{
		"name":      "echo",
		"arguments": map[string]any{},
		"_meta": map[string]any{
			metaKeyProtocolVersion:    MCPProtocolVersionModern,
			metaKeyClientCapabilities: map[string]any{},
		},
	}
	var callResult map[string]any
	if err := rpc.Call(context.Background(), "tools/call", callParams, &callResult); err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if callResult["resultType"] != "complete" {
		t.Errorf("tools/call resultType = %v, want complete", callResult["resultType"])
	}
	if _, ok := callResult["ttlMs"]; ok {
		t.Errorf("tools/call must not carry ttlMs (not a cacheable op), got %#v", callResult["ttlMs"])
	}
}

// TestStdioModernRequest_Validation is the stdio counterpart of
// TestModernRequest_HeaderValidation's "unsupported protocol version" and
// "missing clientCapabilities" cases: wrapStdioModern used to run the
// underlying handler unconditionally and only ever used the Modern _meta to
// decide how to reshape a successful result, so neither case was actually
// rejected over stdio the way HTTP's handleModernRequest rejects them.
// There's no HTTP status code on this transport, so these assert on the
// JSON-RPC error code alone.
func TestStdioModernRequest_Validation(t *testing.T) {
	s := NewServer("stdio-modern-validation", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = s.ServeStream(context.Background(), serverReader, serverWriter)
	}()
	transport := jsonrpc.NewStreamTransport(clientReader, clientWriter)
	rpc := jsonrpc.NewClient(transport)
	defer func() {
		clientWriter.Close()
		<-serveDone
		serverWriter.Close()
	}()

	t.Run("unsupported protocol version", func(t *testing.T) {
		params := map[string]any{
			"_meta": map[string]any{
				metaKeyProtocolVersion:    "1900-01-01",
				metaKeyClientCapabilities: map[string]any{},
			},
		}
		var result map[string]any
		err := rpc.Call(context.Background(), "tools/list", params, &result)
		rpcErr, ok := err.(*jsonrpc.Error)
		if !ok {
			t.Fatalf("expected a *jsonrpc.Error, got %T: %v", err, err)
		}
		if rpcErr.Code != ErrorCodeUnsupportedProtocolVersion {
			t.Errorf("code = %d, want UnsupportedProtocolVersion (%d)", rpcErr.Code, ErrorCodeUnsupportedProtocolVersion)
		}
	})

	// A missing required _meta field (clientCapabilities itself absent) is
	// InvalidParams per the spec's general _meta rule, NOT
	// MissingRequiredClientCapabilityError — that code is reserved for
	// clientCapabilities being present but lacking a specific capability an
	// operation requires, a different, more specific case this codebase
	// doesn't check for today.
	t.Run("missing clientCapabilities", func(t *testing.T) {
		params := map[string]any{
			"_meta": map[string]any{
				metaKeyProtocolVersion: MCPProtocolVersionModern,
			},
		}
		var result map[string]any
		err := rpc.Call(context.Background(), "tools/list", params, &result)
		rpcErr, ok := err.(*jsonrpc.Error)
		if !ok {
			t.Fatalf("expected a *jsonrpc.Error, got %T: %v", err, err)
		}
		if rpcErr.Code != ErrorCodeInvalidParams {
			t.Errorf("code = %d, want InvalidParams (%d)", rpcErr.Code, ErrorCodeInvalidParams)
		}
	})

	t.Run("ping and initialize are gone in Modern", func(t *testing.T) {
		for _, method := range []string{"ping", "initialize"} {
			params := map[string]any{
				"_meta": map[string]any{
					metaKeyProtocolVersion:    MCPProtocolVersionModern,
					metaKeyClientCapabilities: map[string]any{},
				},
			}
			var result map[string]any
			err := rpc.Call(context.Background(), method, params, &result)
			rpcErr, ok := err.(*jsonrpc.Error)
			if !ok {
				t.Fatalf("%s: expected a *jsonrpc.Error, got %T: %v", method, err, err)
			}
			if rpcErr.Code != ErrorCodeMethodNotFound {
				t.Errorf("%s: code = %d, want MethodNotFound (%d)", method, rpcErr.Code, ErrorCodeMethodNotFound)
			}
		}
	})

	// server/discover gets the same validation as every other Modern
	// method (see server_stdio.go's registration comment) when a request
	// presents itself as Modern at all — a bare/no-_meta probe (the
	// backward-compat case) is unaffected, see TestServerDiscoverStdio.
	t.Run("server/discover validates Modern _meta like any other method", func(t *testing.T) {
		params := map[string]any{
			"_meta": map[string]any{
				metaKeyProtocolVersion: "1900-01-01",
			},
		}
		var result map[string]any
		err := rpc.Call(context.Background(), "server/discover", params, &result)
		rpcErr, ok := err.(*jsonrpc.Error)
		if !ok {
			t.Fatalf("expected a *jsonrpc.Error, got %T: %v", err, err)
		}
		if rpcErr.Code != ErrorCodeUnsupportedProtocolVersion {
			t.Errorf("code = %d, want UnsupportedProtocolVersion (%d)", rpcErr.Code, ErrorCodeUnsupportedProtocolVersion)
		}
	})

	// A valid Modern request must still populate ClientCapabilities/
	// SupportsUIApps, the stdio counterpart of
	// TestServer_ClientCapabilities_CapturedFromModernRequest.
	t.Run("valid request populates ClientCapabilities", func(t *testing.T) {
		params := map[string]any{
			"_meta": map[string]any{
				metaKeyProtocolVersion: MCPProtocolVersionModern,
				metaKeyClientCapabilities: map[string]any{
					"extensions": map[string]any{
						UIAppsExtensionID: map[string]any{"mimeTypes": []string{UIAppMimeType}},
					},
				},
			},
		}
		var result map[string]any
		if err := rpc.Call(context.Background(), "tools/list", params, &result); err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		mimeTypes, ok := SupportsUIApps(s.ClientCapabilities())
		if !ok {
			t.Fatalf("expected SupportsUIApps to be true, capabilities = %v", s.ClientCapabilities())
		}
		if len(mimeTypes) != 1 || mimeTypes[0] != UIAppMimeType {
			t.Errorf("mimeTypes = %v", mimeTypes)
		}
	})
}

// TestStdioLegacyRequest_ResultShapeUnchanged is the stdio counterpart of
// TestLegacyResponse_HasNoModernFields: a plain Legacy stdio request (no
// _meta at all) must not gain resultType or ttlMs merely because Modern-era
// support exists in the same binary.
func TestStdioLegacyRequest_ResultShapeUnchanged(t *testing.T) {
	s := NewServer("stdio-legacy-shape", "1")

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = s.ServeStream(context.Background(), serverReader, serverWriter)
	}()
	transport := jsonrpc.NewStreamTransport(clientReader, clientWriter)
	rpc := jsonrpc.NewClient(transport)
	defer func() {
		clientWriter.Close()
		<-serveDone
		serverWriter.Close()
	}()

	var result map[string]any
	if err := rpc.Call(context.Background(), "tools/list", map[string]any{}, &result); err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if _, ok := result["resultType"]; ok {
		t.Errorf("Legacy stdio result must not contain resultType: %#v", result)
	}
	if _, ok := result["ttlMs"]; ok {
		t.Errorf("Legacy stdio result must not contain ttlMs: %#v", result)
	}
}

// --- header/body validation (HeaderMismatch, UnsupportedProtocolVersion) ---

func TestModernRequest_HeaderValidation(t *testing.T) {
	s := NewServer("hv", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	post := func(body string, headers map[string]string) (*http.Response, map[string]any) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, decodeMCPResponse(t, b)
	}

	modernBody := func(method string, params map[string]any) string {
		if params == nil {
			params = map[string]any{}
		}
		params["_meta"] = map[string]any{
			metaKeyProtocolVersion:    MCPProtocolVersionModern,
			metaKeyClientCapabilities: map[string]any{},
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		return string(b)
	}

	t.Run("missing Mcp-Method header", func(t *testing.T) {
		resp, out := post(modernBody("tools/list", nil), map[string]string{
			headerProtocolVersion: MCPProtocolVersionModern,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		errObj, _ := out["error"].(map[string]any)
		if int(errObj["code"].(float64)) != ErrorCodeHeaderMismatch {
			t.Errorf("error = %#v, want HeaderMismatch", errObj)
		}
	})

	t.Run("Mcp-Method mismatches body", func(t *testing.T) {
		resp, out := post(modernBody("tools/list", nil), map[string]string{
			headerMcpMethod:       "prompts/list",
			headerProtocolVersion: MCPProtocolVersionModern,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		errObj, _ := out["error"].(map[string]any)
		if int(errObj["code"].(float64)) != ErrorCodeHeaderMismatch {
			t.Errorf("error = %#v, want HeaderMismatch", errObj)
		}
	})

	t.Run("missing Mcp-Name for tools/call", func(t *testing.T) {
		resp, out := post(modernBody("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}), map[string]string{
			headerMcpMethod:       "tools/call",
			headerProtocolVersion: MCPProtocolVersionModern,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		errObj, _ := out["error"].(map[string]any)
		if int(errObj["code"].(float64)) != ErrorCodeHeaderMismatch {
			t.Errorf("error = %#v, want HeaderMismatch", errObj)
		}
	})

	t.Run("Mcp-Name mismatches body", func(t *testing.T) {
		resp, out := post(modernBody("tools/call", map[string]any{"name": "echo", "arguments": map[string]any{}}), map[string]string{
			headerMcpMethod:       "tools/call",
			headerMcpName:         "not-echo",
			headerProtocolVersion: MCPProtocolVersionModern,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		errObj, _ := out["error"].(map[string]any)
		if int(errObj["code"].(float64)) != ErrorCodeHeaderMismatch {
			t.Errorf("error = %#v, want HeaderMismatch", errObj)
		}
	})

	t.Run("missing MCP-Protocol-Version header", func(t *testing.T) {
		resp, out := post(modernBody("tools/list", nil), map[string]string{
			headerMcpMethod: "tools/list",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		errObj, _ := out["error"].(map[string]any)
		if int(errObj["code"].(float64)) != ErrorCodeHeaderMismatch {
			t.Errorf("error = %#v, want HeaderMismatch", errObj)
		}
	})

	t.Run("MCP-Protocol-Version header mismatches _meta", func(t *testing.T) {
		resp, out := post(modernBody("tools/list", nil), map[string]string{
			headerMcpMethod:       "tools/list",
			headerProtocolVersion: "1900-01-01",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		errObj, _ := out["error"].(map[string]any)
		if int(errObj["code"].(float64)) != ErrorCodeHeaderMismatch {
			t.Errorf("error = %#v, want HeaderMismatch", errObj)
		}
	})

	t.Run("unsupported protocol version", func(t *testing.T) {
		body := modernBody("tools/list", nil)
		body = strings.Replace(body, MCPProtocolVersionModern, "1900-01-01", 1)
		resp, out := post(body, map[string]string{
			headerMcpMethod:       "tools/list",
			headerProtocolVersion: "1900-01-01",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		errObj, _ := out["error"].(map[string]any)
		if int(errObj["code"].(float64)) != ErrorCodeUnsupportedProtocolVersion {
			t.Errorf("error = %#v, want UnsupportedProtocolVersion", errObj)
		}
		data, _ := errObj["data"].(map[string]any)
		supported, _ := data["supported"].([]any)
		// The error must describe the whole dual-era server, matching
		// server/discover's supportedVersions and the spec's own example
		// (which mixes a Modern and a Legacy version) — a client deciding
		// whether to retry Modern or fall back to Legacy needs both eras.
		foundModern, foundLegacy := false, false
		for _, v := range supported {
			s, _ := v.(string)
			if s == MCPProtocolVersionModern {
				foundModern = true
			}
			for _, legacy := range supportedProtocolVersions {
				if s == legacy {
					foundLegacy = true
				}
			}
		}
		if !foundModern || !foundLegacy {
			t.Errorf("data.supported = %v, want both modern and legacy versions listed", supported)
		}
	})

	t.Run("missing clientCapabilities", func(t *testing.T) {
		b, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/list",
			"params": map[string]any{"_meta": map[string]any{metaKeyProtocolVersion: MCPProtocolVersionModern}},
		})
		resp, out := post(string(b), map[string]string{
			headerMcpMethod:       "tools/list",
			headerProtocolVersion: MCPProtocolVersionModern,
		})
		// A missing required _meta field (like an unsupported version or a
		// header mismatch) is a protocol/negotiation-level error and MUST
		// use HTTP 400, unlike an ordinary method-level JSON-RPC error
		// (which stays 200) — but the code itself is plain InvalidParams,
		// per the spec's general "_meta missing a required field" rule, not
		// MissingRequiredClientCapabilityError (reserved for
		// clientCapabilities being present but lacking one specific
		// capability an operation needs).
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
		errObj, _ := out["error"].(map[string]any)
		if int(errObj["code"].(float64)) != ErrorCodeInvalidParams {
			t.Errorf("error = %#v, want InvalidParams", errObj)
		}
	})

	t.Run("unknown method", func(t *testing.T) {
		resp, out := post(modernBody("nonexistent/method", nil), map[string]string{
			headerMcpMethod:       "nonexistent/method",
			headerProtocolVersion: MCPProtocolVersionModern,
		})
		// Per the spec, an unrecognized RPC method MUST get HTTP 404 (distinct
		// from the 200 used for an ordinary method-level JSON-RPC error, e.g.
		// tools/call with an unknown tool name — see the "nonexistent tool"
		// test elsewhere in this file).
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
		errObj, _ := out["error"].(map[string]any)
		if int(errObj["code"].(float64)) != ErrorCodeMethodNotFound {
			t.Errorf("error = %#v, want MethodNotFound", errObj)
		}
	})

	// "ping" and "initialize" are both dispatchable for Legacy clients (see
	// dispatchMethod), but the 2026-07-28 revision removes both outright —
	// ping entirely, initialize in favor of the stateless per-request model
	// server/discover replaces. A Modern request naming either must 404
	// exactly like any other method this server never supported.
	for _, method := range []string{"ping", "initialize"} {
		t.Run("removed-in-modern: "+method, func(t *testing.T) {
			resp, out := post(modernBody(method, nil), map[string]string{
				headerMcpMethod:       method,
				headerProtocolVersion: MCPProtocolVersionModern,
			})
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status = %d, want 404", resp.StatusCode)
			}
			errObj, _ := out["error"].(map[string]any)
			if int(errObj["code"].(float64)) != ErrorCodeMethodNotFound {
				t.Errorf("error = %#v, want MethodNotFound", errObj)
			}
		})
	}
}

// TestServer_ClientCapabilities_CapturedFromModernRequest is the Modern-era
// counterpart to TestServer_ClientCapabilities_CapturedFromInitialize: there
// is no initialize call in this era, so every request's own _meta.
// clientCapabilities must populate ClientCapabilities()/SupportsUIApps the
// same way Legacy's initialize does, or the documented conditional-tool-
// registration pattern silently never fires for Modern clients.
func TestServer_ClientCapabilities_CapturedFromModernRequest(t *testing.T) {
	s := NewServer("cc-modern", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	if got := s.ClientCapabilities(); got != nil {
		t.Errorf("ClientCapabilities before any request = %v, want nil", got)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{
		"` + metaKeyProtocolVersion + `":"` + MCPProtocolVersionModern + `",
		"` + metaKeyClientCapabilities + `":{"extensions":{"` + UIAppsExtensionID + `":{"mimeTypes":["` + UIAppMimeType + `"]}}}
	}}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerMcpMethod, "tools/list")
	req.Header.Set(headerProtocolVersion, MCPProtocolVersionModern)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	mimeTypes, ok := SupportsUIApps(s.ClientCapabilities())
	if !ok {
		t.Fatalf("expected SupportsUIApps to be true after a Modern request, capabilities = %v", s.ClientCapabilities())
	}
	if len(mimeTypes) != 1 || mimeTypes[0] != UIAppMimeType {
		t.Errorf("mimeTypes = %v", mimeTypes)
	}
}

// --- successful modern round trip: same handlers, reshaped result ---------

func TestModernRequest_ToolsListRoundTrip(t *testing.T) {
	s := NewServer("rt", "2.0")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	req := modernRequest(t, ts.URL, "tools/list", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := decodeMCPResponse(t, body)
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %#v", out)
	}
	tools, _ := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool (same handler as Legacy), got %d: %#v", len(tools), result)
	}
	if result["resultType"] != "complete" {
		t.Errorf("resultType = %v, want complete", result["resultType"])
	}
	if ttlMs, ok := result["ttlMs"].(float64); !ok || ttlMs < 0 {
		t.Errorf("ttlMs = %#v, want a present, non-negative number (spec-required on tools/list)", result["ttlMs"])
	}
	if result["cacheScope"] != "public" {
		t.Errorf("cacheScope = %v, want public", result["cacheScope"])
	}
	meta, _ := result["_meta"].(map[string]any)
	serverInfo, _ := meta[metaKeyServerInfo].(map[string]any)
	if serverInfo["name"] != "rt" {
		t.Errorf("serverInfo = %#v", serverInfo)
	}
}

// TestModernRequest_ResourcesAndPromptsRoundTrip closes the gap in
// TestModernRequest_ToolsListRoundTrip: resources/list, resources/read (with
// Mcp-Name validated against the uri, not a name), resources/templates/list,
// prompts/list, and prompts/get all go through the same reused-handler +
// result-shaping path as tools/list, and each needs its own coverage since
// resources/read's cacheScope ("private") differs from every list op's
// ("public"), and prompts/get (like tools/call) must not gain ttlMs at all.
func TestModernRequest_ResourcesAndPromptsRoundTrip(t *testing.T) {
	s := NewServer("rp", "1")
	s.RegisterResource(
		NewResource("file:///a.txt", "a", "a text file", "text/plain"),
		func(ctx context.Context, req *ResourceRequest) (*ResourceResponse, error) {
			return NewResourceResponseText(req.URI(), "hello", "text/plain"), nil
		},
	)
	s.RegisterResourceTemplate(
		NewResourceTemplate("file:///{name}", "tmpl", "a templated file", "text/plain"),
		func(ctx context.Context, req *ResourceRequest) (*ResourceResponse, error) {
			return NewResourceResponseText(req.URI(), "hello", "text/plain"), nil
		},
	)
	s.RegisterPrompt(
		NewPrompt("greet", "greets"),
		func(ctx context.Context, req *PromptRequest) (*PromptResponse, error) {
			return NewPromptResponseText("hi"), nil
		},
	)
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	get := func(method string, params map[string]any) map[string]any {
		t.Helper()
		req := modernRequest(t, ts.URL, method, params)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: Do: %v", method, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		out := decodeMCPResponse(t, body)
		result, ok := out["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no result: %#v", method, out)
		}
		return result
	}

	assertCache := func(t *testing.T, method string, result map[string]any, wantScope string) {
		t.Helper()
		if result["resultType"] != "complete" {
			t.Errorf("%s: resultType = %v, want complete", method, result["resultType"])
		}
		if ttlMs, ok := result["ttlMs"].(float64); !ok || ttlMs < 0 {
			t.Errorf("%s: ttlMs = %#v, want a present, non-negative number", method, result["ttlMs"])
		}
		if result["cacheScope"] != wantScope {
			t.Errorf("%s: cacheScope = %v, want %v", method, result["cacheScope"], wantScope)
		}
	}

	t.Run("resources/list", func(t *testing.T) {
		result := get("resources/list", nil)
		resources, _ := result["resources"].([]any)
		if len(resources) != 1 {
			t.Fatalf("expected 1 resource, got %d: %#v", len(resources), result)
		}
		assertCache(t, "resources/list", result, "public")
	})

	t.Run("resources/templates/list", func(t *testing.T) {
		result := get("resources/templates/list", nil)
		templates, _ := result["resourceTemplates"].([]any)
		if len(templates) != 1 {
			t.Fatalf("expected 1 template, got %d: %#v", len(templates), result)
		}
		assertCache(t, "resources/templates/list", result, "public")
	})

	t.Run("resources/read", func(t *testing.T) {
		result := get("resources/read", map[string]any{"uri": "file:///a.txt"})
		contents, _ := result["contents"].([]any)
		if len(contents) != 1 {
			t.Fatalf("expected 1 content entry, got %#v", result)
		}
		assertCache(t, "resources/read", result, "private")
	})

	t.Run("prompts/list", func(t *testing.T) {
		result := get("prompts/list", nil)
		prompts, _ := result["prompts"].([]any)
		if len(prompts) != 1 {
			t.Fatalf("expected 1 prompt, got %d: %#v", len(prompts), result)
		}
		assertCache(t, "prompts/list", result, "public")
	})

	t.Run("prompts/get", func(t *testing.T) {
		result := get("prompts/get", map[string]any{"name": "greet"})
		if result["resultType"] != "complete" {
			t.Errorf("resultType = %v, want complete", result["resultType"])
		}
		if _, ok := result["ttlMs"]; ok {
			t.Errorf("prompts/get must not carry ttlMs (not a cacheable op), got %#v", result["ttlMs"])
		}
	})
}

// TestCacheHintsFor pins down exactly which operations the spec requires
// caching hints on (tools/list, prompts/list, resources/list,
// resources/templates/list, resources/read — server/discover is handled
// separately in buildDiscoverResult) and confirms tools/call and prompts/get,
// which aren't cacheable list/read operations, are correctly excluded.
func TestCacheHintsFor(t *testing.T) {
	wantApplicable := map[string]string{
		"tools/list":               "public",
		"prompts/list":             "public",
		"resources/list":           "public",
		"resources/templates/list": "public",
		"resources/read":           "private",
	}
	for method, wantScope := range wantApplicable {
		ttlMs, scope, ok := cacheHintsFor(method)
		if !ok {
			t.Errorf("cacheHintsFor(%q): applicable = false, want true", method)
			continue
		}
		if ttlMs < 0 {
			t.Errorf("cacheHintsFor(%q): ttlMs = %d, want >= 0", method, ttlMs)
		}
		if scope != wantScope {
			t.Errorf("cacheHintsFor(%q): cacheScope = %q, want %q", method, scope, wantScope)
		}
	}
	for _, method := range []string{"tools/call", "prompts/get", "server/discover", "subscriptions/listen"} {
		if _, _, ok := cacheHintsFor(method); ok {
			t.Errorf("cacheHintsFor(%q): applicable = true, want false (not a cacheable list/read op)", method)
		}
	}
	for method := range wantApplicable {
		if _, scope, _ := cacheHintsForRequest(method, true); scope != "private" {
			t.Errorf("cacheHintsForRequest(%q, perUser): cacheScope = %q, want private", method, scope)
		}
		_, want, _ := cacheHintsFor(method)
		if _, scope, _ := cacheHintsForRequest(method, false); scope != want {
			t.Errorf("cacheHintsForRequest(%q, !perUser): cacheScope = %q, want %q", method, scope, want)
		}
	}
}

// TestModernRequest_CacheScopePerUser covers that a list result is only
// "public" when nothing about the request makes it per user: credentials
// (Authorization, Cookie) or context-scoped providers make it "private".
func TestModernRequest_CacheScopePerUser(t *testing.T) {
	s := NewServer("cache-scope", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	withProvider := func(w http.ResponseWriter, r *http.Request) {
		s.HandleRequest(w, r.WithContext(WithToolProviders(r.Context(), headerProvider{})))
	}

	cases := []struct {
		name    string
		handler http.HandlerFunc
		method  string
		headers map[string]string
		want    string
	}{
		{"anonymous tools/list", s.HandleRequest, "tools/list", nil, "public"},
		{"authorized tools/list", s.HandleRequest, "tools/list", map[string]string{"Authorization": "Bearer x"}, "private"},
		{"cookie tools/list", s.HandleRequest, "tools/list", map[string]string{"Cookie": "a=b"}, "private"},
		{"provider tools/list", withProvider, "tools/list", nil, "private"},
		{"anonymous server/discover", s.HandleRequest, "server/discover", nil, "public"},
		{"authorized server/discover", s.HandleRequest, "server/discover", map[string]string{"Authorization": "Bearer x"}, "private"},
		{"provider server/discover", withProvider, "server/discover", nil, "private"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := modernRequest(t, "/", tc.method, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			tc.handler(rec, req)
			out := decodeMCPResponse(t, rec.Body.Bytes())
			result, _ := out["result"].(map[string]any)
			if result["cacheScope"] != tc.want {
				t.Errorf("cacheScope = %v, want %s (body %s)", result["cacheScope"], tc.want, rec.Body.String())
			}
		})
	}
}

// TestModernRequest_ToolCallErrorPassesThrough covers that an ordinary
// method-level error (unknown tool) from the shared handler is forwarded
// as-is — 200 status, no resultType/serverInfo injected into an error body —
// since that's a ToolError, not a protocol-level Modern error.
func TestModernRequest_ToolCallErrorPassesThrough(t *testing.T) {
	s := NewServer("err", "1")
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	req := modernRequest(t, ts.URL, "tools/call", map[string]any{"name": "nonexistent", "arguments": map[string]any{}})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 for a JSON-RPC-level error", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	out := decodeMCPResponse(t, body)
	if _, ok := out["error"]; !ok {
		t.Fatalf("expected an error for an unknown tool, got %#v", out)
	}
	if _, ok := out["result"]; ok {
		t.Fatalf("did not expect a result alongside an error: %#v", out)
	}
}

// --- Garbled/malformed bodies get the spec-correct status -----------------

// TestModernRequest_GarbledTopLevelBody_Returns400 proves a body that fails
// to decode at all gets HTTP 400 for a Modern request. Previously this hit
// the pre-routing decode failure in HandleRequest, which always answered
// 200 (the Legacy JSON-RPC convention) — even for a request carrying the
// Mcp-Method header that unambiguously marks it Modern, before Modern
// routing even gets a chance to apply its own status-code rules.
func TestModernRequest_GarbledTopLevelBody_Returns400(t *testing.T) {
	s := NewServer("garbled", "1")
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`{not valid json`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerMcpMethod, "tools/list")
	req.Header.Set(headerProtocolVersion, MCPProtocolVersionModern)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	out := decodeMCPResponse(t, body)
	errObj, _ := out["error"].(map[string]any)
	if int(errObj["code"].(float64)) != ErrorCodeParseError {
		t.Errorf("error = %#v, want ParseError", errObj)
	}
}

// TestModernRequest_InvalidJSONRPCVersion_Returns400 proves a well-formed
// JSON body whose "jsonrpc" field isn't "2.0" also gets 400 for Modern — the
// other pre-routing envelope check that used to always return 200.
func TestModernRequest_InvalidJSONRPCVersion_Returns400(t *testing.T) {
	s := NewServer("garbled", "1")
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`{"jsonrpc":"1.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerMcpMethod, "tools/list")
	req.Header.Set(headerProtocolVersion, MCPProtocolVersionModern)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	out := decodeMCPResponse(t, body)
	errObj, _ := out["error"].(map[string]any)
	if int(errObj["code"].(float64)) != ErrorCodeInvalidRequest {
		t.Errorf("error = %#v, want InvalidRequest", errObj)
	}
}

// TestModernRequest_LegacyGarbledBody_Unaffected is the regression guard for
// the two tests above: a Legacy request (no Mcp-Method header) with the
// exact same garbled bodies must keep getting 200, per the Legacy always-200
// JSON-RPC convention (TestHandleRequestEdgeCases already covers the plain
// decode-failure case without any Modern signal at all; this specifically
// confirms adding era-awareness to that shared code path didn't flip Legacy
// too).
func TestModernRequest_LegacyGarbledBody_Unaffected(t *testing.T) {
	s := NewServer("garbled", "1")
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	for _, body := range []string{`{not valid json`, `{"jsonrpc":"1.0","id":1,"method":"tools/list","params":{}}`} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("body %q: status = %d, want 200 (Legacy convention unchanged)", body, resp.StatusCode)
		}
	}
}

// TestParamsParseFailure_EraAwareStatus is the regression test for the three
// dispatched-handler sites sharing this pattern (tools/call, resources/read,
// prompts/get): a request whose params can't unmarshal into the method's
// expected shape — a garbled body one layer below the envelope, distinct
// from a well-formed-but-semantically-invalid one (unknown tool, missing
// required field), which correctly stays a 200 method-level error in both
// eras. Exercised directly against the handlers (bypassing the full HTTP
// round-trip's Mcp-Name/body-consistency gate, which resources/read in
// particular can't satisfy once its one field, uri, is the thing being
// garbled — the gate would reject the request before parseParams ever runs)
// since what's under test is sendProtocolAwareError's era detection, the
// same header check the gate itself uses.
func TestParamsParseFailure_EraAwareStatus(t *testing.T) {
	s := NewServer("garbled-params", "1")
	garbledParams := map[string]any{"name": 5, "uri": 5, "arguments": 5}

	cases := []struct {
		name   string
		method string
		call   func(w http.ResponseWriter, r *http.Request, req *MCPRequest)
	}{
		{"tools/call", "tools/call", func(w http.ResponseWriter, r *http.Request, req *MCPRequest) { s.handleToolsCall(w, r, req) }},
		{"resources/read", "resources/read", func(w http.ResponseWriter, r *http.Request, req *MCPRequest) { s.handleResourcesRead(w, r, req) }},
		{"prompts/get", "prompts/get", func(w http.ResponseWriter, r *http.Request, req *MCPRequest) { s.handlePromptsGet(w, r, req) }},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/modern gets 400", func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			r.Header.Set(headerMcpMethod, tc.method)
			capture := newModernResponseCapture()
			// The body's _meta claim is what classifies this as Modern.
			modernParams := map[string]any{"_meta": map[string]any{metaKeyProtocolVersion: MCPProtocolVersionModern}}
			for k, v := range garbledParams {
				modernParams[k] = v
			}
			req := &MCPRequest{JSONRPC: "2.0", ID: "x", Method: tc.method, Params: modernParams}
			tc.call(capture, r, req)
			if capture.status != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", capture.status)
			}
			var out map[string]any
			if err := json.Unmarshal(capture.body.Bytes(), &out); err != nil {
				t.Fatalf("capture body is not valid JSON: %v (%s)", err, capture.body.String())
			}
			errObj, _ := out["error"].(map[string]any)
			if errObj == nil {
				t.Fatalf("expected an error, got %#v", out)
			}
			if int(errObj["code"].(float64)) != ErrorCodeInvalidParams {
				t.Errorf("error code = %v, want InvalidParams", errObj["code"])
			}
		})

		t.Run(tc.name+"/legacy stays 200", func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil) // no Mcp-Method header
			w := httptest.NewRecorder()
			req := &MCPRequest{JSONRPC: "2.0", ID: "x", Method: tc.method, Params: garbledParams}
			tc.call(w, r, req)
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (Legacy convention unchanged)", w.Code)
			}
		})
	}
}

// --- Legacy is untouched ----------------------------------------------

// TestLegacyResponse_HasNoModernFields is the wire-format-freeze guarantee:
// a plain Legacy request's response must not gain resultType or serverInfo
// _meta merely because the Modern feature exists in the same binary.
func TestLegacyResponse_HasNoModernFields(t *testing.T) {
	s := NewServer("legacy", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerProtocolVersion, MCPProtocolVersionLatest)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "resultType") {
		t.Fatalf("Legacy response must not contain resultType: %s", raw)
	}
	if strings.Contains(string(raw), metaKeyServerInfo) {
		t.Fatalf("Legacy response must not contain serverInfo _meta: %s", raw)
	}
	out := decodeMCPResponse(t, raw)
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %#v", result)
	}
}

// --- Base64 sentinel header encoding ------------------------------------

func TestModernHeaderEncoding(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"plain ascii", "us-west1"},
		{"non-ascii", "Hello, 世界"},
		{"leading/trailing space", " padded "},
		{"newline", "line1\nline2"},
		{"already looks like sentinel", "=?base64?literal?="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			encoded := encodeModernHeaderValue(tc.value)
			decoded, err := decodeModernHeaderValue(encoded)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if decoded != tc.value {
				t.Errorf("round trip: got %q, want %q (encoded: %q)", decoded, tc.value, encoded)
			}
		})
	}

	t.Run("plain ascii is not encoded", func(t *testing.T) {
		if got := encodeModernHeaderValue("us-west1"); got != "us-west1" {
			t.Errorf("encodeModernHeaderValue(%q) = %q, want unchanged", "us-west1", got)
		}
	})
}

// --- subscriptions/listen -------------------------------------------------

func TestSubscriptionsListen(t *testing.T) {
	s := NewServer("subs", "1")
	s.RegisterTool(NewTool("a", "a"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("a"), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	req := modernRequest(t, ts.URL, "subscriptions/listen", map[string]any{
		"notifications": map[string]any{"toolsListChanged": true},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	reader := newSSELineReader(resp.Body)

	ack := reader.next(t)
	if !strings.Contains(ack, "notifications/subscriptions/acknowledged") {
		t.Fatalf("expected ack first, got: %s", ack)
	}
	if !strings.Contains(ack, `"toolsListChanged":true`) {
		t.Fatalf("expected acked filter to include toolsListChanged: %s", ack)
	}
	if !strings.Contains(ack, metaKeySubscriptionID) {
		t.Fatalf("expected ack to carry subscriptionId: %s", ack)
	}

	waitForSubscribers(t, s, 1)
	s.NotifyPromptsChanged() // not subscribed to -> must NOT be delivered
	s.NotifyToolsChanged()   // subscribed to -> must be delivered, modern-spelled

	line := reader.next(t)
	if !strings.Contains(line, "notifications/tools/list_changed") {
		t.Fatalf("expected modern-spelled tools list_changed notification (and prompts filtered out), got: %s", line)
	}
	if !strings.Contains(line, metaKeySubscriptionID) {
		t.Fatalf("expected notification to carry subscriptionId: %s", line)
	}
}

// TestSubscriptionsListen_MalformedFilter_ReturnsError covers a
// subscriptions/listen request whose params.notifications is absent, not an
// object, or has a wrongly-typed field: each gets a synchronous JSON-RPC
// error and never registers a live subscriber. An object that selects no
// supported type is valid (see TestSubscriptionsListen_UnsupportedOnlyFilter_AcksEmpty).
func TestSubscriptionsListen_MalformedFilter_ReturnsError(t *testing.T) {
	s := NewServer("subs-malformed", "1")
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	post := func(t *testing.T, params map[string]any) *http.Response {
		t.Helper()
		req := modernRequest(t, ts.URL, "subscriptions/listen", params)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		return resp
	}

	cases := []struct {
		name   string
		params map[string]any
	}{
		{"notifications missing entirely", map[string]any{}},
		{"notifications not an object", map[string]any{"notifications": "toolsListChanged"}},
		{"notifications null", map[string]any{"notifications": nil}},
		{"notifications field wrong type", map[string]any{"notifications": map[string]any{"toolsListChanged": "yes"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := post(t, tc.params)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			out := decodeMCPResponse(t, body)
			errObj, _ := out["error"].(map[string]any)
			if errObj == nil {
				t.Fatalf("expected a JSON-RPC error, got %#v", out)
			}
			if int(errObj["code"].(float64)) != ErrorCodeInvalidParams {
				t.Errorf("error code = %v, want InvalidParams", errObj["code"])
			}
			if n := s.notifications.count(); n != 0 {
				t.Errorf("subscriber count = %d, want 0 — a rejected request must not register a live, permanently-idle subscriber", n)
			}
		})
	}
}

// TestSubscriptionsListen_UnsupportedOnlyFilter_AcksEmpty covers the spec's
// "every field of params.notifications is optional" and "Notification types
// the server does not support are omitted": a filter selecting none of the
// list_changed types (only resourceSubscriptions, all false, unknown keys,
// or {}) opens the stream and acknowledges an empty notifications object.
func TestSubscriptionsListen_UnsupportedOnlyFilter_AcksEmpty(t *testing.T) {
	cases := []struct {
		name          string
		notifications map[string]any
	}{
		{"resourceSubscriptions only", map[string]any{"resourceSubscriptions": []any{"file:///a.txt"}}},
		{"empty object", map[string]any{}},
		{"all false", map[string]any{"toolsListChanged": false}},
		{"unrecognized keys only", map[string]any{"somethingElse": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer("subs-empty", "1")
			ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
			defer ts.Close()

			req := modernRequest(t, ts.URL, "subscriptions/listen", map[string]any{"notifications": tc.notifications})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			resp, err := http.DefaultClient.Do(req.WithContext(ctx))
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			ack := newSSELineReader(resp.Body).next(t)
			payload := strings.TrimSpace(strings.TrimPrefix(ack, "data:"))
			var out map[string]any
			if err := json.Unmarshal([]byte(payload), &out); err != nil {
				t.Fatalf("decode ack: %v, payload: %s", err, payload)
			}
			if out["method"] != "notifications/subscriptions/acknowledged" {
				t.Fatalf("expected ack first, got: %s", payload)
			}
			params, _ := out["params"].(map[string]any)
			acked, ok := params["notifications"].(map[string]any)
			if !ok || len(acked) != 0 {
				t.Fatalf("acked notifications = %#v, want {}", params["notifications"])
			}
		})
	}
}

// TestSubscriptionsListen_GracefulShutdown covers the spec's "Graceful
// Closure": Server.Shutdown must make an open subscriptions/listen stream
// respond to its ORIGINAL request (matching id, resultType "complete") before
// the stream ends, rather than the connection just going dead the way it
// does on an ordinary client disconnect.
func TestSubscriptionsListen_GracefulShutdown(t *testing.T) {
	s := NewServer("subs-shutdown", "1")
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	req := modernRequest(t, ts.URL, "subscriptions/listen", map[string]any{
		"notifications": map[string]any{"toolsListChanged": true},
	})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	reader := newSSELineReader(resp.Body)
	ack := reader.next(t)
	if !strings.Contains(ack, "notifications/subscriptions/acknowledged") {
		t.Fatalf("expected ack first, got: %s", ack)
	}

	waitForSubscribers(t, s, 1)
	s.Shutdown()

	closure := reader.next(t)
	payload := strings.TrimSpace(strings.TrimPrefix(closure, "data:"))
	var out map[string]any
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("decode closure message: %v, payload: %s", err, payload)
	}
	// It must be a RESPONSE (has "result", correlated by "id"), not another
	// notification (which would have "method" and no "id").
	if _, ok := out["method"]; ok {
		t.Fatalf("graceful closure must be a response, not a notification: %s", payload)
	}
	if id, ok := out["id"].(float64); !ok || id != 1 {
		t.Fatalf("closure id = %v, want it to match the original request's id (1): %s", out["id"], payload)
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in closure message: %s", payload)
	}
	if result["resultType"] != "complete" {
		t.Errorf("resultType = %v, want complete", result["resultType"])
	}
	if meta, _ := result["_meta"].(map[string]any); meta[metaKeySubscriptionID] == nil {
		t.Errorf("expected _meta.subscriptionId on the closure result: %#v", result)
	}
}

// sseLineReader reads one "data: ...\n\n" frame at a time from an SSE stream,
// tolerating heartbeat comment lines.
type sseLineReader struct {
	r   io.Reader
	buf []byte
}

func newSSELineReader(r io.Reader) *sseLineReader { return &sseLineReader{r: r} }

func (s *sseLineReader) next(t *testing.T) string {
	t.Helper()
	chunk := make([]byte, 4096)
	for i := 0; i < 20; i++ {
		n, err := s.r.Read(chunk)
		// Per io.Reader's contract, a call may legitimately return n > 0
		// alongside a non-nil error (e.g. the final chunk together with
		// io.EOF, as happens right after the server writes its last message
		// and returns, closing the connection) — that data must still be
		// processed before treating the error as a failure.
		if n > 0 {
			text := string(chunk[:n])
			if strings.HasPrefix(text, "data: ") {
				return text
			}
			// heartbeat/comment line ("\n" prefix) — keep reading
		}
		if err != nil {
			t.Fatalf("read SSE: %v", err)
		}
	}
	t.Fatal("did not receive a data: line")
	return ""
}

// --- server validation: duplicate and malformed standard headers ---------

// modernRecorderRequest builds a valid Modern tools/call request for
// HandleRequest via httptest, so tests can set raw header values that a
// real client would canonicalise or refuse to send.
func modernRecorderRequest(t *testing.T, tool string) *http.Request {
	t.Helper()
	return modernRequest(t, "/", "tools/call", map[string]any{"name": tool, "arguments": map[string]any{}})
}

func TestModernRequest_DuplicateHeaders(t *testing.T) {
	s := NewServer("dup", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})

	for _, header := range []string{headerMcpMethod, headerMcpName, headerProtocolVersion} {
		t.Run(header, func(t *testing.T) {
			req := modernRecorderRequest(t, "echo")
			req.Header.Add(header, req.Header.Get(header)) // same value twice
			rec := httptest.NewRecorder()
			s.HandleRequest(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			out := decodeMCPResponse(t, rec.Body.Bytes())
			errObj, _ := out["error"].(map[string]any)
			if int(errObj["code"].(float64)) != ErrorCodeHeaderMismatch {
				t.Errorf("error = %#v, want HeaderMismatch", errObj)
			}
			want := "Header mismatch: multiple " + header + " header values"
			if errObj["message"] != want {
				t.Errorf("message = %v, want %q", errObj["message"], want)
			}
		})
	}
}

func TestModernRequest_InvalidHeaderCharacters(t *testing.T) {
	s := NewServer("badchars", "1")
	s.RegisterTool(NewTool("café", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})

	for _, header := range []string{headerMcpMethod, headerMcpName} {
		t.Run(header, func(t *testing.T) {
			req := modernRecorderRequest(t, "café")
			raw := map[string]string{headerMcpMethod: "tools/callé", headerMcpName: "café"}[header]
			req.Header[header] = []string{raw} // raw UTF-8, not Base64-encoded
			rec := httptest.NewRecorder()
			s.HandleRequest(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			out := decodeMCPResponse(t, rec.Body.Bytes())
			errObj, _ := out["error"].(map[string]any)
			if int(errObj["code"].(float64)) != ErrorCodeHeaderMismatch {
				t.Errorf("error = %#v, want HeaderMismatch", errObj)
			}
			want := "Header mismatch: " + header + " header value contains invalid characters"
			if errObj["message"] != want {
				t.Errorf("message = %v, want %q", errObj["message"], want)
			}
		})
	}

	t.Run("Base64-encoded Mcp-Name is accepted", func(t *testing.T) {
		req := modernRecorderRequest(t, "café")
		rec := httptest.NewRecorder()
		s.HandleRequest(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
	})
}

// --- clientCapabilities must be an object --------------------------------

func TestModernRequest_ClientCapabilitiesNotObject(t *testing.T) {
	s := NewServer("caps-shape", "1")
	for _, tc := range []struct {
		name string
		caps string
	}{
		{"null", `null`},
		{"string", `"all"`},
		{"array", `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"` +
				metaKeyProtocolVersion + `":"` + MCPProtocolVersionModern + `","` +
				metaKeyClientCapabilities + `":` + tc.caps + `}}}`
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(headerMcpMethod, "tools/list")
			req.Header.Set(headerProtocolVersion, MCPProtocolVersionModern)
			rec := httptest.NewRecorder()
			s.HandleRequest(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			out := decodeMCPResponse(t, rec.Body.Bytes())
			errObj, _ := out["error"].(map[string]any)
			if int(errObj["code"].(float64)) != ErrorCodeInvalidParams {
				t.Errorf("error = %#v, want InvalidParams", errObj)
			}
			data, _ := errObj["data"].(map[string]any)
			if details, _ := data["details"].(string); !strings.Contains(details, "must be a JSON object") {
				t.Errorf("details = %v", data["details"])
			}
		})
	}
}

// --- server/discover omits empty instructions ------------------------------

func TestServerDiscover_OmitsEmptyInstructions(t *testing.T) {
	s := NewServer("no-instructions", "1")
	req := modernRequest(t, "/", "server/discover", nil)
	rec := httptest.NewRecorder()
	s.HandleRequest(rec, req)
	out := decodeMCPResponse(t, rec.Body.Bytes())
	result, _ := out["result"].(map[string]any)
	if result == nil {
		t.Fatalf("no result: %s", rec.Body.String())
	}
	if _, ok := result["instructions"]; ok {
		t.Errorf("instructions = %#v, want it omitted when empty", result["instructions"])
	}
}

// --- per-request client context -------------------------------------------

// TestModernRequest_ClientContextIsPerRequest covers that each Modern
// request's handler sees its own clientCapabilities and clientInfo via the
// context accessors, even while another client's request is in flight
// (Server.ClientCapabilities only remembers the last one).
func TestModernRequest_ClientContextIsPerRequest(t *testing.T) {
	s := NewServer("per-request", "1")
	var arrived sync.WaitGroup
	arrived.Add(2)
	s.RegisterTool(NewTool("who", "reports the caller"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		arrived.Done()
		arrived.Wait() // both requests are now in flight
		caps := ClientCapabilitiesFromContext(ctx)
		info := ClientInfoFromContext(ctx)
		return NewToolResponseText(fmt.Sprintf("%v/%v", caps["marker"], info["name"])), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	call := func(client string) (string, error) {
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{
				"name": "who", "arguments": map[string]any{},
				"_meta": map[string]any{
					metaKeyProtocolVersion:    MCPProtocolVersionModern,
					metaKeyClientCapabilities: map[string]any{"marker": client},
					metaKeyClientInfo:         map[string]any{"name": client, "version": "1"},
				},
			},
		})
		req, _ := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerMcpMethod, "tools/call")
		req.Header.Set(headerMcpName, "who")
		req.Header.Set(headerProtocolVersion, MCPProtocolVersionModern)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var out struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", err
		}
		if len(out.Result.Content) == 0 {
			return "", fmt.Errorf("no content")
		}
		return out.Result.Content[0].Text, nil
	}

	results := make(chan [2]string, 2)
	for _, client := range []string{"alpha", "beta"} {
		go func(client string) {
			got, err := call(client)
			if err != nil {
				got = err.Error()
			}
			results <- [2]string{client, got}
		}(client)
	}
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if want := r[0] + "/" + r[0]; r[1] != want {
				t.Errorf("client %s saw %q, want %q", r[0], r[1], want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for concurrent requests")
		}
	}

	if ClientCapabilitiesFromContext(context.Background()) != nil || ClientInfoFromContext(context.Background()) != nil {
		t.Error("accessors must return nil for a context without Modern client data")
	}
}

// TestStdioModernRequest_ClientContext covers the stdio counterpart of
// TestModernRequest_ClientContextIsPerRequest: the handler's ctx carries the
// request's own clientCapabilities and clientInfo.
func TestStdioModernRequest_ClientContext(t *testing.T) {
	s := NewServer("stdio-ctx", "1")
	s.RegisterTool(NewTool("who", "reports the caller"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		caps := ClientCapabilitiesFromContext(ctx)
		info := ClientInfoFromContext(ctx)
		return NewToolResponseText(fmt.Sprintf("%v/%v", caps["marker"], info["name"])), nil
	})
	rpc, done := stdioTestClient(t, s)
	defer done()

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	err := rpc.Call(context.Background(), "tools/call", map[string]any{
		"name": "who", "arguments": map[string]any{},
		"_meta": map[string]any{
			metaKeyProtocolVersion:    MCPProtocolVersionModern,
			metaKeyClientCapabilities: map[string]any{"marker": "gamma"},
			metaKeyClientInfo:         map[string]any{"name": "gamma"},
		},
	}, &result)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if len(result.Content) == 0 || result.Content[0].Text != "gamma/gamma" {
		t.Errorf("result = %#v, want gamma/gamma", result)
	}
}

// --- stdio era routing ------------------------------------------------------

// stdioTestClient serves s over an in-memory stdio pipe.
func stdioTestClient(t *testing.T, s *Server) (*jsonrpc.Client, func()) {
	t.Helper()
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = s.ServeStream(context.Background(), serverReader, serverWriter)
	}()
	rpc := jsonrpc.NewClient(jsonrpc.NewStreamTransport(clientReader, clientWriter))
	return rpc, func() {
		clientWriter.Close()
		<-serveDone
		serverWriter.Close()
	}
}

// TestStdioModernMeta_LegacyClaimIsLegacy covers that stdio routing mirrors
// isModernRequest: a _meta protocolVersion naming a Legacy revision is
// Legacy (no Modern validation or shaping), anything else is Modern.
func TestStdioModernMeta_LegacyClaimIsLegacy(t *testing.T) {
	claim := func(v string) json.RawMessage {
		b, _ := json.Marshal(map[string]any{"_meta": map[string]any{metaKeyProtocolVersion: v}})
		return b
	}
	if _, isModern := stdioModernMeta(claim("2025-11-25")); isModern {
		t.Error("Legacy claim 2025-11-25 must not be Modern")
	}
	if _, isModern := stdioModernMeta(claim(MCPProtocolVersionModern)); !isModern {
		t.Error("Modern claim must be Modern")
	}
	if _, isModern := stdioModernMeta(claim("2099-01-01")); !isModern {
		t.Error("unknown claim must be Modern, so it gets UnsupportedProtocolVersionError")
	}

	s := NewServer("stdio-legacy-claim", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	rpc, done := stdioTestClient(t, s)
	defer done()

	// No clientCapabilities: Modern validation would reject this, Legacy
	// dispatch serves it unshaped.
	var result map[string]any
	err := rpc.Call(context.Background(), "tools/list", map[string]any{
		"_meta": map[string]any{metaKeyProtocolVersion: "2025-11-25"},
	}, &result)
	if err != nil {
		t.Fatalf("tools/list with a Legacy claim: %v", err)
	}
	if _, ok := result["resultType"]; ok {
		t.Errorf("Legacy-claim result must not be Modern-shaped: %#v", result)
	}
	if _, ok := result["tools"]; !ok {
		t.Errorf("expected tools in result: %#v", result)
	}
}

func TestStdioModernRequest_ClientCapabilitiesNotObject(t *testing.T) {
	s := NewServer("stdio-caps-shape", "1")
	rpc, done := stdioTestClient(t, s)
	defer done()

	var result map[string]any
	err := rpc.Call(context.Background(), "tools/list", map[string]any{
		"_meta": map[string]any{
			metaKeyProtocolVersion:    MCPProtocolVersionModern,
			metaKeyClientCapabilities: nil,
		},
	}, &result)
	rpcErr, ok := err.(*jsonrpc.Error)
	if !ok {
		t.Fatalf("expected a *jsonrpc.Error, got %T: %v", err, err)
	}
	if rpcErr.Code != ErrorCodeInvalidParams {
		t.Errorf("code = %d, want InvalidParams (%d)", rpcErr.Code, ErrorCodeInvalidParams)
	}
}
