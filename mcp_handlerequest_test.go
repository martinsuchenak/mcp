package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// erroringSessionManager lets tests force ValidateSession/DeleteSession to
// return an error, which mockSessionManager (used elsewhere) never does.
type erroringSessionManager struct {
	validateErr error
	deleteErr   error
	validResult bool
	showAll     bool
}

func (e *erroringSessionManager) CreateSession(ctx context.Context, protocolVersion string, showAll bool) (string, error) {
	return "sess-1", nil
}
func (e *erroringSessionManager) ValidateSession(ctx context.Context, sessionID string) (bool, error) {
	if e.validateErr != nil {
		return false, e.validateErr
	}
	return e.validResult, nil
}
func (e *erroringSessionManager) GetProtocolVersion(ctx context.Context, sessionID string) (string, error) {
	return "2025-06-18", nil
}
func (e *erroringSessionManager) GetShowAll(ctx context.Context, sessionID string) (bool, error) {
	return e.showAll, nil
}
func (e *erroringSessionManager) DeleteSession(ctx context.Context, sessionID string) error {
	return e.deleteErr
}
func (e *erroringSessionManager) CleanupExpiredSessions(ctx context.Context, maxIdleTime time.Duration) error {
	return nil
}

var _ SessionManager = (*erroringSessionManager)(nil)

// TestHandleRequest_DeleteSession covers the DELETE method branch of
// HandleRequest end to end: no session manager configured, missing header,
// manager error, and success.
func TestHandleRequest_DeleteSession(t *testing.T) {
	t.Run("no session manager", func(t *testing.T) {
		s := NewServer("test", "1.0")
		req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rr.Code)
		}
	})

	t.Run("missing session header", func(t *testing.T) {
		s := NewServer("test", "1.0")
		s.SetSessionManager(newMockSessionManager())
		req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rr.Code)
		}
	})

	t.Run("manager error", func(t *testing.T) {
		s := NewServer("test", "1.0")
		s.SetSessionManager(&erroringSessionManager{deleteErr: errors.New("boom"), validResult: true})
		req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
		req.Header.Set(headerSessionID, "sess-1")
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rr.Code)
		}
	})

	t.Run("success", func(t *testing.T) {
		s := NewServer("test", "1.0")
		mockSM := newMockSessionManager()
		sessionID, err := mockSM.CreateSession(context.Background(), "2025-06-18", false)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		s.SetSessionManager(mockSM)
		req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
		req.Header.Set(headerSessionID, sessionID)
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
		}
		if mockSM.deleteCalled != 1 {
			t.Errorf("DeleteSession calls = %d, want 1", mockSM.deleteCalled)
		}
	})
}

// TestHandleRequest_GETStream covers the GET method branch: a plain GET
// (no Accept: text/event-stream) is a 405, while an SSE-accepting GET is
// dispatched into handleSSEStream and gets a text/event-stream response.
func TestHandleRequest_GETStream(t *testing.T) {
	s := NewServer("test", "1.0")

	t.Run("plain GET is method not allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want 405", rr.Code)
		}
		if got := rr.Header().Get("Allow"); got == "" {
			t.Error("expected an Allow header on 405")
		}
	})

	t.Run("SSE GET opens a stream", func(t *testing.T) {
		// Use a real httptest.Server (not just a ResponseRecorder) since
		// handleSSEStream needs a genuine http.Flusher and a cancellable
		// request context to return.
		ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
		defer ts.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		req.Header.Set("Accept", "text/event-stream")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			// A context-deadline transport error is expected once the server
			// keeps the stream open past our short timeout; that still proves
			// the GET was routed into the SSE handler rather than rejected.
			return
		}
		defer resp.Body.Close()
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
			t.Errorf("Content-Type = %q, want text/event-stream", ct)
		}
	})
}

// TestHandleRequest_ProtocolVersionAndSession covers the non-initialize
// request path's protocol-version and session-validation branches: an
// unsupported header version, a missing session header when a session
// manager is configured, an invalid session ID, and a manager error during
// validation.
func TestHandleRequest_ProtocolVersionAndSession(t *testing.T) {
	t.Run("unsupported protocol version header", func(t *testing.T) {
		s := NewServer("test", "1.0")
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerProtocolVersion, "1999-01-01")
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rr.Code)
		}
		// Regression test: this used to be a plain-text, no-body response —
		// a client had no way to learn which versions this server actually
		// supports. It must now be a proper JSON-RPC error naming them,
		// matching every other protocol-version rejection in this codebase.
		var out struct {
			Error struct {
				Data struct {
					Requested string   `json:"requested"`
					Supported []string `json:"supported"`
				} `json:"data"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("response body is not valid JSON-RPC: %v (%s)", err, rr.Body.String())
		}
		if out.Error.Data.Requested != "1999-01-01" {
			t.Errorf("data.requested = %q, want %q", out.Error.Data.Requested, "1999-01-01")
		}
		if !slices.Equal(out.Error.Data.Supported, supportedProtocolVersions) {
			t.Errorf("data.supported = %v, want the Legacy version list %v", out.Error.Data.Supported, supportedProtocolVersions)
		}
	})

	t.Run("session manager configured but header missing", func(t *testing.T) {
		s := NewServer("test", "1.0")
		s.SetSessionManager(newMockSessionManager())
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rr.Code)
		}
	})

	t.Run("session validation error", func(t *testing.T) {
		s := NewServer("test", "1.0")
		s.SetSessionManager(&erroringSessionManager{validateErr: errors.New("db down")})
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerSessionID, "whatever")
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if rr.Code != http.StatusInternalServerError {
			t.Errorf("status = %d, want 500", rr.Code)
		}
	})

	t.Run("invalid session", func(t *testing.T) {
		s := NewServer("test", "1.0")
		s.SetSessionManager(&erroringSessionManager{validResult: false})
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerSessionID, "whatever")
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rr.Code)
		}
	})

	t.Run("valid session with show-all propagates to context", func(t *testing.T) {
		s := NewServer("test", "1.0")
		s.RegisterTool(NewTool("disc", "discoverable").Discoverable("kw"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return NewToolResponseText("ok"), nil
		})
		s.SetSessionManager(&erroringSessionManager{validResult: true, showAll: true})
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(headerSessionID, "whatever")
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
		}
		// In show-all mode the discoverable tool itself appears in tools/list.
		if !strings.Contains(rr.Body.String(), `"disc"`) {
			t.Errorf("expected discoverable tool in show-all tools/list, body: %s", rr.Body.String())
		}
	})

	t.Run("no session manager: show-all via header", func(t *testing.T) {
		s := NewServer("test", "1.0")
		s.RegisterTool(NewTool("disc", "discoverable").Discoverable("kw"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return NewToolResponseText("ok"), nil
		})
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(ShowAllHeader, "true")
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), `"disc"`) {
			t.Errorf("expected discoverable tool via header show-all, body: %s", rr.Body.String())
		}
	})
}

// TestServer_ListTools_DeprecatedWrapper covers the deprecated ListTools()
// thin wrapper around ListToolsWithContext(context.Background()).
func TestServer_ListTools_DeprecatedWrapper(t *testing.T) {
	s := NewServer("test", "1.0")
	s.RegisterTool(NewTool("a", "tool a"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("a"), nil
	})
	tools := s.ListToolsWithContext(context.Background())
	if len(tools) != 1 || tools[0].Name != "a" {
		t.Errorf("ListTools() = %+v, want [{a}]", tools)
	}
}

// TestFirstPresent covers firstPresent's hit, later-key-hit, and miss paths.
func TestFirstPresent(t *testing.T) {
	values := map[string]any{"input_schema": "snake", "other": 1}
	if got := firstPresent(values, "inputSchema", "input_schema"); got != "snake" {
		t.Errorf("firstPresent = %v, want snake (second key)", got)
	}
	values2 := map[string]any{"inputSchema": "camel"}
	if got := firstPresent(values2, "inputSchema", "input_schema"); got != "camel" {
		t.Errorf("firstPresent = %v, want camel (first key)", got)
	}
	if got := firstPresent(map[string]any{}, "a", "b"); got != nil {
		t.Errorf("firstPresent on empty map = %v, want nil", got)
	}
}

// TestParseParams_Errors covers Server.parseParams' error branches: nil
// params (no-op success) and an unmarshalable target/shape.
func TestParseParams_Errors(t *testing.T) {
	s := NewServer("test", "1.0")

	// nil Params is a no-op success.
	var target ToolCallParams
	if err := s.parseParams(&MCPRequest{}, &target); err != nil {
		t.Errorf("parseParams with nil Params = %v, want nil", err)
	}

	// Params that can't unmarshal into the target type errors.
	req := &MCPRequest{Params: map[string]any{"name": 123, "arguments": "not-a-map"}}
	var badTarget ToolCallParams
	if err := s.parseParams(req, &badTarget); err == nil {
		t.Error("expected an error unmarshaling incompatible params")
	}
}

// TestRegisterTools_DiscoverableBatch covers RegisterTools' discoverable
// branch (the existing TestRegisterTools_BatchRegistration only exercises
// native tools), including a mixed batch and verifying discoverable tools
// are searchable but absent from tools/list.
func TestRegisterTools_DiscoverableBatch(t *testing.T) {
	s := NewServer("test", "1.0")

	s.RegisterTools(
		NewToolRegistration(
			NewTool("native_one", "native"),
			func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
				return NewToolResponseText("native"), nil
			},
		),
		NewToolRegistration(
			NewTool("disc_one", "discoverable", String("q", "q")).Discoverable("findme"),
			func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
				return NewToolResponseText("disc"), nil
			},
		),
	)

	tools := s.ListToolsWithContext(context.Background())
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
	}
	if !names["native_one"] {
		t.Errorf("expected native_one in tools/list, got %+v", tools)
	}
	if names["disc_one"] {
		t.Errorf("disc_one should NOT appear in tools/list, got %+v", tools)
	}
	// tool_search / execute_tool should be present since a discoverable tool exists.
	if !names[ToolSearchName] || !names[ExecuteToolName] {
		t.Errorf("expected discovery meta-tools in tools/list, got %+v", tools)
	}

	// The discoverable tool is directly callable via CallTool (server's
	// CallTool checks s.tools regardless of visibility).
	resp, err := s.CallTool(context.Background(), "disc_one", map[string]any{"q": "x"})
	if err != nil {
		t.Fatalf("CallTool(disc_one): %v", err)
	}
	if resp.Content[0].Text != "disc" {
		t.Errorf("resp = %+v, want disc", resp)
	}

	// And discoverable via tool_search.
	searchResp, err := s.CallTool(context.Background(), "tool_search", map[string]any{"query": "findme"})
	if err != nil {
		t.Fatalf("tool_search: %v", err)
	}
	if !strings.Contains(searchResp.Content[0].Text, "disc_one") {
		t.Errorf("tool_search results = %s, want to contain disc_one", searchResp.Content[0].Text)
	}
}

// TestRegisterTools_EmptyIsNoOp covers RegisterTools' early return when
// called with zero tools.
func TestRegisterTools_EmptyIsNoOp(t *testing.T) {
	s := NewServer("test", "1.0")
	s.RegisterTools() // must not panic and must not register anything
	if tools := s.ListToolsWithContext(context.Background()); len(tools) != 0 {
		t.Errorf("expected no tools, got %+v", tools)
	}
}
