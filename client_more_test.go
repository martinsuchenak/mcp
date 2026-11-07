package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClient_ReadResource covers Client.ReadResource end to end against a
// real server (static resource), including the not-found error path — no
// existing test drives resources/read through the Client.
func TestClient_ReadResource(t *testing.T) {
	srv := NewServer("svc", "1")
	srv.RegisterResource(
		NewResource("config://app", "App Config", "the config", "text/plain"),
		func(ctx context.Context, req *ResourceRequest) (*ResourceResponse, error) {
			return NewResourceResponseText(req.URI(), "hello-config", "text/plain"), nil
		},
	)
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleRequest))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	ctx := context.Background()

	resp, err := c.ReadResource(ctx, "config://app")
	if err != nil {
		t.Fatalf("ReadResource: %v", err)
	}
	if len(resp.Contents) != 1 || resp.Contents[0].Text != "hello-config" {
		t.Fatalf("unexpected resource response: %+v", resp)
	}

	if _, err := c.ReadResource(ctx, "config://missing"); err == nil {
		t.Error("expected an error reading an unknown resource")
	}
}

// TestClient_ListResourceTemplates covers Client.ListResourceTemplates, which
// no existing test drives through the Client (only Server.ListResourceTemplates
// is tested directly).
func TestClient_ListResourceTemplates(t *testing.T) {
	srv := NewServer("svc", "1")
	srv.RegisterResourceTemplate(
		NewResourceTemplate("user://{id}", "User", "a user", "application/json"),
		func(ctx context.Context, req *ResourceRequest) (*ResourceResponse, error) {
			id, _ := req.String("id")
			return NewResourceResponseText(req.URI(), id, "application/json"), nil
		},
	)
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleRequest))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	templates, err := c.ListResourceTemplates(context.Background())
	if err != nil {
		t.Fatalf("ListResourceTemplates: %v", err)
	}
	if len(templates) != 1 || templates[0].URITemplate != "user://{id}" {
		t.Fatalf("unexpected templates: %+v", templates)
	}
}

// TestClient_GetPrompt covers Client.GetPrompt end to end, including the
// missing-required-argument error path.
func TestClient_GetPrompt(t *testing.T) {
	srv := NewServer("svc", "1")
	srv.RegisterPrompt(
		NewPrompt("greet", "Greet someone").Argument("name", "who to greet", true),
		func(ctx context.Context, req *PromptRequest) (*PromptResponse, error) {
			name, _ := req.String("name")
			return &PromptResponse{
				Messages: []PromptMessage{
					{Role: PromptRoleUser, Content: PromptMessageContent{Type: "text", Text: "Hello, " + name}},
				},
			}, nil
		},
	)
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleRequest))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	ctx := context.Background()

	resp, err := c.GetPrompt(ctx, "greet", map[string]string{"name": "Ada"})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	if len(resp.Messages) != 1 || resp.Messages[0].Content.Text != "Hello, Ada" {
		t.Fatalf("unexpected prompt response: %+v", resp)
	}

	if _, err := c.GetPrompt(ctx, "greet", nil); err == nil {
		t.Error("expected an error for missing required argument")
	}

	if _, err := c.GetPrompt(ctx, "no_such_prompt", nil); err == nil {
		t.Error("expected an error for unknown prompt")
	}
}

// TestArgs_Arg covers the Args fluent builder's Arg method, which no test
// exercises (CallTool call sites always pass a plain map literal).

// TestClient_Instructions covers Client.Instructions, which captures whatever
// the remote server returned in its initialize response.
func TestClient_Instructions(t *testing.T) {
	srv := NewServer("svc", "1")
	srv.SetInstructions("please read the docs")
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleRequest))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	ctx := context.Background()

	if got := c.Instructions(); got != "" {
		t.Fatalf("expected no instructions before Initialize, got %q", got)
	}

	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if got := c.Instructions(); got != "please read the docs" {
		t.Fatalf("Instructions() = %q, want %q", got, "please read the docs")
	}
}

// TestClient_InstructionsEmptyWhenServerSetsNone covers the case where the
// remote server never called SetInstructions.
func TestClient_InstructionsEmptyWhenServerSetsNone(t *testing.T) {
	srv := NewServer("svc", "1")
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleRequest))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if got := c.Instructions(); got != "" {
		t.Fatalf("Instructions() = %q, want empty", got)
	}
}

// TestClient_InstructionsIncludesDiscoveryHint covers Instructions capturing
// the SDK's auto-appended discovery-tool guidance (see appendDiscoveryInstructions),
// not just instructions set explicitly via SetInstructions.
func TestClient_InstructionsIncludesDiscoveryHint(t *testing.T) {
	srv := NewServer("svc", "1")
	srv.RegisterTool(NewTool("hidden", "A hidden tool").Discoverable("test"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleRequest))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	got := c.Instructions()
	if !strings.Contains(got, ToolSearchName) || !strings.Contains(got, ExecuteToolName) {
		t.Fatalf("Instructions() = %q, want it to mention %s and %s", got, ToolSearchName, ExecuteToolName)
	}
}

// TestClient_ProtocolVersion_Legacy covers Client.ProtocolVersion capturing
// whatever protocol revision a Legacy server actually advertises in its
// initialize response — which the spec allows to differ from what the
// client requested (e.g. an older server that only supports an earlier
// revision). A hand-written fake server (not a real *Server) proves this
// is read from the response body, not just echoed back from the request.
func TestClient_ProtocolVersion_Legacy(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req MCPRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if req.Method == "server/discover" {
			http.Error(w, "unknown method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MCPResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "old-server", "version": "0.1"},
			},
		})
	}))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	if got := c.ProtocolVersion(); got != "" {
		t.Fatalf("expected empty ProtocolVersion before Initialize, got %q", got)
	}
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if got := c.ProtocolVersion(); got != "2025-03-26" {
		t.Fatalf("ProtocolVersion() = %q, want %q", got, "2025-03-26")
	}
}

// TestClient_ProtocolVersion_Modern covers Client.ProtocolVersion reporting
// MCPProtocolVersionModern once Modern era is confirmed via server/discover.
func TestClient_ProtocolVersion_Modern(t *testing.T) {
	srv := NewServer("svc", "1")
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleRequest))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	c.mu.RLock()
	era := c.era
	c.mu.RUnlock()
	if era != eraModern {
		t.Fatalf("era = %v, want eraModern (this server supports server/discover)", era)
	}
	if got := c.ProtocolVersion(); got != MCPProtocolVersionModern {
		t.Fatalf("ProtocolVersion() = %q, want %q", got, MCPProtocolVersionModern)
	}
}
