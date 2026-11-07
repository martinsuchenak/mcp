package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCapabilitiesByProtocolVersion(t *testing.T) {
	s := NewServer("s", "1")
	h := http.HandlerFunc(s.HandleRequest)

	// oldest supported version
	body := MCPRequest{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: map[string]any{
		"protocolVersion": "2025-03-26",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "n", "version": "v"},
	}}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	var rpc MCPResponse
	_ = json.NewDecoder(rr.Body).Decode(&rpc)
	res := rpc.Result.(map[string]any)
	caps := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Fatal("missing tools in caps")
	}

	// latest version
	body.Params.(map[string]any)["protocolVersion"] = MCPProtocolVersionLatest
	b, _ = json.Marshal(body)
	req = httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	_ = json.NewDecoder(rr.Body).Decode(&rpc)
	res = rpc.Result.(map[string]any)
	caps = res["capabilities"].(map[string]any)
	tools := caps["tools"].(map[string]any)
	if _, ok := tools["listChanged"]; !ok {
		t.Fatal("expected listChanged in latest tools caps")
	}
	resources := caps["resources"].(map[string]any)
	if _, ok := resources["subscribe"]; !ok {
		t.Fatal("expected subscribe in latest resources caps")
	}

	// mid version 2025-03-26 (should match latest shape in current impl)
	body.Params.(map[string]any)["protocolVersion"] = "2025-03-26"
	b, _ = json.Marshal(body)
	req = httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	_ = json.NewDecoder(rr.Body).Decode(&rpc)
	res = rpc.Result.(map[string]any)
	caps = res["capabilities"].(map[string]any)
	tools = caps["tools"].(map[string]any)
	if _, ok := tools["listChanged"]; !ok {
		t.Fatal("expected listChanged in 2025-03-26 tools caps")
	}
}
