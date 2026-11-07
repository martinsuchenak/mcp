package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/paularlott/mcp"
)

// TestStdioCallToolsParallelOrderAndErrors verifies CallToolsParallel over the
// stdio transport returns results in call order and isolates per-call errors.
func TestStdioCallToolsParallelOrderAndErrors(t *testing.T) {
	client, cleanup := pipeStdioClient(t, buildStdioTestServer())
	defer cleanup()

	results := client.CallToolsParallel(context.Background(), []mcp.ToolCall{
		{Name: "greet", Arguments: map[string]any{"name": "Ada"}},
		{Name: "boom"},
		{Name: "greet", Arguments: map[string]any{"name": "Grace"}},
	})

	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("results[0].Err = %v", results[0].Err)
	}
	if results[0].Response == nil || results[0].Response.Content[0].Text != "Hello, Ada!" {
		t.Errorf("results[0].Response = %+v", results[0].Response)
	}

	var toolErr *mcp.ToolError
	if !errors.As(results[1].Err, &toolErr) {
		t.Fatalf("results[1].Err = %v, want *mcp.ToolError", results[1].Err)
	}
	if toolErr.Message != "kaboom" {
		t.Errorf("results[1].Err.Message = %q, want kaboom", toolErr.Message)
	}

	if results[2].Err != nil {
		t.Errorf("results[2].Err = %v", results[2].Err)
	}
	if results[2].Response == nil || results[2].Response.Content[0].Text != "Hello, Grace!" {
		t.Errorf("results[2].Response = %+v", results[2].Response)
	}
}

// TestStdioCallToolsParallelSendsNoBatch proves CallToolsParallel over stdio
// never sends a JSON-RPC batch to a server speaking a protocol revision
// without batching (removed in 2025-06-18, absent from 2026-07-28; this
// server negotiates Modern), but one individual request per call.
func TestStdioCallToolsParallelSendsNoBatch(t *testing.T) {
	server := buildStdioTestServer()
	client, cleanup, counted := countingPipeStdioClient(t, server)
	defer cleanup()

	results := client.CallToolsParallel(context.Background(), []mcp.ToolCall{
		{Name: "greet", Arguments: map[string]any{"name": "A"}},
		{Name: "greet", Arguments: map[string]any{"name": "B"}},
		{Name: "greet", Arguments: map[string]any{"name": "C"}},
	})
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("results[%d].Err = %v", i, r.Err)
		}
	}
	if got := counted.arrays.Load(); got != 0 {
		t.Errorf("client sent %d JSON-RPC batch arrays, want 0", got)
	}
}

// TestStdioExecuteDiscoveredToolsParallel exercises the execute_tool wrapping
// path for discovered tools over the batch transport.
func TestStdioExecuteDiscoveredToolsParallel(t *testing.T) {
	server := mcp.NewServer("discoverable-test-server", "1.0.0")
	server.RegisterTool(
		mcp.NewTool("secret", "A discoverable tool", mcp.String("name", "name", mcp.Required())).Discoverable("secret"),
		func(ctx context.Context, req *mcp.ToolRequest) (*mcp.ToolResponse, error) {
			name, _ := req.String("name")
			return mcp.NewToolResponseText("secret:" + name), nil
		},
	)
	client, cleanup := pipeStdioClient(t, server)
	defer cleanup()

	results := client.ExecuteDiscoveredToolsParallel(context.Background(), []mcp.ToolCall{
		{Name: "secret", Arguments: map[string]any{"name": "X"}},
		{Name: "secret", Arguments: map[string]any{"name": "Y"}},
	})
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("results[%d].Err = %v", i, r.Err)
		}
	}
	if results[0].Response.Content[0].Text != "secret:X" {
		t.Errorf("results[0] = %+v", results[0].Response)
	}
	if results[1].Response.Content[0].Text != "secret:Y" {
		t.Errorf("results[1] = %+v", results[1].Response)
	}
}

// TestHTTPCallToolsParallelStillWorks confirms the HTTP client path (which has
// no batchTransport) still falls back to concurrent individual calls and
// produces correct, order-preserved results.
func TestHTTPCallToolsParallelStillWorks(t *testing.T) {
	server := mcp.NewServer("http-test-server", "1.0.0")
	server.RegisterTool(
		mcp.NewTool("greet", "Greet someone", mcp.String("name", "name", mcp.Required())),
		func(ctx context.Context, req *mcp.ToolRequest) (*mcp.ToolResponse, error) {
			name, _ := req.String("name")
			return mcp.NewToolResponseText("Hello, " + name + "!"), nil
		},
	)
	httpServer := httptest.NewServer(http.HandlerFunc(server.HandleRequest))
	defer httpServer.Close()

	client := mcp.NewClient(httpServer.URL, nil, "")
	results := client.CallToolsParallel(context.Background(), []mcp.ToolCall{
		{Name: "greet", Arguments: map[string]any{"name": "Ada"}},
		{Name: "greet", Arguments: map[string]any{"name": "Grace"}},
	})
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Response.Content[0].Text != "Hello, Ada!" {
		t.Errorf("results[0] = %+v", results[0].Response)
	}
	if results[1].Response.Content[0].Text != "Hello, Grace!" {
		t.Errorf("results[1] = %+v", results[1].Response)
	}
}

// TestStdioCallToolsParallelEmpty checks the zero-call case is handled without
// touching the transport.
func TestStdioCallToolsParallelEmpty(t *testing.T) {
	client, cleanup := pipeStdioClient(t, buildStdioTestServer())
	defer cleanup()

	results := client.CallToolsParallel(context.Background(), nil)
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0", len(results))
	}
}

// countingPipeStdioClient is like pipeStdioClient but counts the number of
// top-level JSON messages the client writes to the server, to verify batching
// actually reduces wire messages.
func countingPipeStdioClient(t *testing.T, s *mcp.Server) (*mcp.Client, func(), *jsonMessageCountingWriter) {
	t.Helper()
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	counted := &jsonMessageCountingWriter{w: clientWriter}

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = s.ServeStream(context.Background(), serverReader, serverWriter)
	}()

	client := mcp.NewStreamClient(clientReader, counted, "")
	cleanup := func() {
		client.Close()
		clientWriter.Close()
		<-serveDone
		serverWriter.Close()
	}
	return client, cleanup, counted
}

// jsonMessageCountingWriter counts each Write call as one JSON message. The
// jsonrpc stream transport writes exactly one message (request or batch array)
// per Write call (each followed by its own newline), so counting writes is
// equivalent to counting top-level wire messages.
type jsonMessageCountingWriter struct {
	mu     sync.Mutex
	w      io.Writer
	count  atomic.Int64
	arrays atomic.Int64 // messages that are JSON-RPC batch arrays
}

func (c *jsonMessageCountingWriter) Write(p []byte) (int, error) {
	if bytes.TrimSpace(p) != nil && json.Valid(bytes.TrimSpace(bytes.TrimRight(p, "\n"))) {
		c.count.Add(1)
		if bytes.HasPrefix(bytes.TrimSpace(p), []byte("[")) {
			c.arrays.Add(1)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.w.Write(p)
}
