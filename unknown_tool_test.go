package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnknownTool_HTTPEnvelope(t *testing.T) {
	s := NewServer("s", "1")
	body := MCPRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: ToolCallParams{Name: "nope"}}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	http.HandlerFunc(s.HandleRequest).ServeHTTP(rr, req)
	var r MCPResponse
	_ = json.NewDecoder(rr.Body).Decode(&r)
	// The spec's own example: a protocol error, -32602 "Unknown tool: ...".
	if r.Error == nil || r.Error.Code != ErrorCodeInvalidParams || !strings.HasPrefix(r.Error.Message, "Unknown tool: nope") {
		t.Fatalf("expected -32602 Unknown tool for unknown tool, got %+v", r.Error)
	}
}

func TestUnknownTool_DirectAPI(t *testing.T) {
	s := NewServer("s", "1")
	if _, err := s.CallTool(context.TODO(), "nope", nil); err == nil || err != ErrUnknownTool {
		t.Fatalf("expected ErrUnknownTool, got %v", err)
	}
}
