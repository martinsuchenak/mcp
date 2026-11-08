package mcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Numeric arguments arriving over HTTP read as numbers through ToolRequest
// (ids are decoded exactly, but params stay ordinary float64 values).
func TestHTTPNumericArgumentsReadAsNumbers(t *testing.T) {
	s := NewServer("n", "1")
	s.RegisterTool(NewTool("sum", "adds", Number("a", "", Required()), Number("b", "", Required()), Integer("n", "", Required())), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		a, err := req.Float("a")
		if err != nil {
			return nil, err
		}
		b, err := req.Float("b")
		if err != nil {
			return nil, err
		}
		n, err := req.Int("n")
		if err != nil {
			return nil, err
		}
		return NewToolResponseStructured(map[string]any{"sum": a + b, "n": n}), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()
	post := func(body string) string {
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerProtocolVersion, "2025-11-25")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	out := post(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sum","arguments":{"a":2.5,"b":3,"n":42}}}`)
	if !strings.Contains(out, `"sum":5.5`) || !strings.Contains(out, `"n":42`) || strings.Contains(out, "isError") {
		t.Fatalf("got %s", out)
	}
	// A wrong type is an input validation error: a tool execution error
	// the model can read, not a protocol error.
	out = post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sum","arguments":{"a":"x","b":3,"n":1}}}`)
	if !strings.Contains(out, `"isError":true`) || !strings.Contains(out, "parameter a must be of type number") || strings.Contains(out, `"error"`) {
		t.Fatalf("got %s", out)
	}
}
