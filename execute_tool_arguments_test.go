package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// executeToolServer serves one discoverable tool, "lookup", that requires
// "id" and records the arguments it ran with.
func executeToolServer(t *testing.T) (post func(arguments string) map[string]any, ran *[]map[string]any) {
	t.Helper()
	s := NewServer("test", "1.0")
	calls := []map[string]any{}
	s.RegisterTool(NewTool("lookup", "Look up an entry", String("id", "Entry id", Required()), String("scope", "Optional scope")).Discoverable("lookup"),
		func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			calls = append(calls, req.Args())
			return NewToolResponseText("found " + req.StringOr("id", "")), nil
		})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	t.Cleanup(ts.Close)
	return func(arguments string) map[string]any {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute_tool","arguments":` + arguments + `}}`
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerProtocolVersion, "2025-11-25")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return out
	}, &calls
}

// resultText returns a tools/call result's text and isError, failing on a
// JSON-RPC error: every case here must come back as a tool result.
func resultText(t *testing.T, out map[string]any) (string, bool) {
	t.Helper()
	if e, ok := out["error"]; ok {
		t.Fatalf("want a tool result, got a protocol error: %v", e)
	}
	result, _ := out["result"].(map[string]any)
	content, _ := result["content"].([]any)
	var text []string
	for _, c := range content {
		if m, ok := c.(map[string]any); ok {
			s, _ := m["text"].(string)
			text = append(text, s)
		}
	}
	isError, _ := result["isError"].(bool)
	return strings.Join(text, "\n"), isError
}

func TestExecuteToolAcceptsParameterShapes(t *testing.T) {
	post, ran := executeToolServer(t)
	for _, arguments := range []string{
		`{"name":"lookup","parameters":{"id":"a1"}}`,
		`{"name":"lookup","parameters":"{\"id\":\"a1\"}"}`,
		`{"name":"lookup","arguments":{"id":"a1"}}`,
		`{"name":"lookup","arguments":"{\"id\":\"a1\"}"}`,
	} {
		text, isError := resultText(t, post(arguments))
		if isError || text != "found a1" {
			t.Errorf("%s: got %q (isError=%v), want found a1", arguments, text, isError)
		}
	}
	if len(*ran) != 4 {
		t.Fatalf("handler ran %d times, want 4", len(*ran))
	}
}

// A request execute_tool can't read is refused with a message saying what
// to send; the target never runs with arguments other than those sent.
func TestExecuteToolRefusesUnreadableArguments(t *testing.T) {
	post, ran := executeToolServer(t)
	for _, tc := range []struct{ arguments, want string }{
		{`{"name":"lookup","args":{"id":"a1"}}`, `unknown argument "args"`},
		{`{"tool_name":"lookup","parameters":{"id":"a1"}}`, `unknown argument "tool_name"`},
		{`{"parameters":{"id":"a1"}}`, `"name" is required`},
		{`{"name":"","parameters":{"id":"a1"}}`, `"name" is required`},
		{`{"name":"lookup","parameters":{"id":"a1"},"arguments":{"id":"a1"}}`, `in "parameters" only`},
		{`{"name":"lookup","parameters":"id=a1"}`, `"parameters" must be a JSON object`},
		{`{"name":"lookup","parameters":"[1]"}`, `"parameters" must be a JSON object`},
		{`{"name":"lookup","parameters":"null"}`, `"parameters" must be a JSON object`},
		{`{"name":"lookup","parameters":[{"id":"a1"}]}`, `"parameters" must be a JSON object`},
		{`{"name":"lookup","arguments":42}`, `"arguments" must be a JSON object`},
		// The target's own schema still applies to what gets through.
		{`{"name":"lookup","parameters":{}}`, `missing required property id`},
		{`{"name":"lookup","parameters":{"id":"a1","extra":1}}`, `parameter extra unexpected property`},
		{`{"name":"nope","parameters":{}}`, `Tool not found: nope`},
	} {
		text, isError := resultText(t, post(tc.arguments))
		if !isError || !strings.Contains(text, tc.want) {
			t.Errorf("%s: got %q (isError=%v), want an error containing %q", tc.arguments, text, isError, tc.want)
		}
	}
	if len(*ran) != 0 {
		t.Fatalf("the target must not run on refused requests, ran with %v", *ran)
	}
}

// Omitted parameters mean none: a tool without required arguments runs.
func TestExecuteToolWithoutParameters(t *testing.T) {
	s := NewServer("test", "1.0")
	s.RegisterTool(NewTool("ping", "Ping").Discoverable("ping"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("pong"), nil
	})
	for _, args := range []map[string]any{{"name": "ping"}, {"name": "ping", "parameters": nil}} {
		resp, err := s.CallTool(context.Background(), ExecuteToolName, args)
		if err != nil || resp.IsError || resp.Content[0].Text != "pong" {
			t.Fatalf("%v: got %+v, %v", args, resp, err)
		}
	}
}

// Server.CallTool callers still get a *ToolError with the invalid-params
// code for arguments the tool can't take.
func TestExecuteToolDirectCallError(t *testing.T) {
	s := NewServer("test", "1.0")
	_, err := s.CallTool(context.Background(), ExecuteToolName, map[string]any{"name": "x", "args": map[string]any{}})
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || toolErr.Code != ErrorCodeInvalidParams {
		t.Fatalf("want a *ToolError %d, got %T %v", ErrorCodeInvalidParams, err, err)
	}
}

// tool_search checks its arguments too: a misspelt query must not list
// every tool as if it had been omitted.
func TestToolSearchRefusesUnknownArguments(t *testing.T) {
	s := NewServer("test", "1.0")
	_, err := s.CallTool(context.Background(), ToolSearchName, map[string]any{"q": "lookup"})
	var toolErr *ToolError
	if !errors.As(err, &toolErr) || !strings.Contains(toolErr.Message, "q") {
		t.Fatalf("want an error naming q, got %v", err)
	}
}
