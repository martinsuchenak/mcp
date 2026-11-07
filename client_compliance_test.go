package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Client-side MCP spec compliance: session expiry and termination, the
// Legacy GET notification stream, request ids, cancellation, era detection,
// resultType, stdio batching, the stdio probe timeout, stdio ping and the
// Modern stdio subscription.

// --- helpers ---------------------------------------------------------------

// rpcMessage is a decoded inbound JSON-RPC message with its raw id.
type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params map[string]any  `json:"params"`
	Result any             `json:"result"`
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": "error", "data": data}})
}

func legacyInitializeResult(version string) map[string]any {
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "fake", "version": "1"},
	}
}

// newLegacyHTTPServer is a fake Legacy (2025-11-25) Streamable HTTP server:
// server/discover gets a plain 404 (so the client falls back to initialize),
// initialize is answered with a session id from nextSession, and
// notifications are acknowledged with 202. Every other request goes to
// handle; DELETE and GET also go to handle.
func newLegacyHTTPServer(t *testing.T, nextSession func() string, handle func(w http.ResponseWriter, r *http.Request, msg rpcMessage)) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			handle(w, r, rpcMessage{})
			return
		}
		var msg rpcMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		switch {
		case msg.Method == "server/discover":
			http.NotFound(w, r)
		case msg.Method == "initialize":
			w.Header().Set(headerSessionID, nextSession())
			writeRPCResult(w, msg.ID, legacyInitializeResult(MCPProtocolVersionLatest))
		case msg.Method == "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			handle(w, r, msg)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func constSession(id string) func() string { return func() string { return id } }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeStdio is a scripted stdio MCP server on the far end of a
// NewStreamClient pipe pair. Every line the client writes is recorded and
// handed to handle, which may answer with send.
type fakeStdio struct {
	toClient *io.PipeWriter
	writeMu  sync.Mutex
	mu       sync.Mutex
	lines    []string
}

func (f *fakeStdio) send(v any) {
	b, _ := json.Marshal(v)
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	_, _ = f.toClient.Write(append(b, '\n'))
}

func (f *fakeStdio) result(id json.RawMessage, result any) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *fakeStdio) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lines...)
}

// findMessage returns the first recorded single message with method.
func (f *fakeStdio) findMessage(method string) (rpcMessage, bool) {
	for _, line := range f.recorded() {
		var msg rpcMessage
		if json.Unmarshal([]byte(line), &msg) == nil && msg.Method == method {
			return msg, true
		}
	}
	return rpcMessage{}, false
}

func newFakeStdioClient(t *testing.T, handle func(f *fakeStdio, msg rpcMessage)) (*Client, *fakeStdio) {
	t.Helper()
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	f := &fakeStdio{toClient: serverWriter}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(serverReader)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			f.mu.Lock()
			f.lines = append(f.lines, line)
			f.mu.Unlock()
			if strings.HasPrefix(strings.TrimSpace(line), "[") {
				var batch []rpcMessage
				_ = json.Unmarshal([]byte(line), &batch)
				var out []any
				for _, msg := range batch {
					out = append(out, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"content": []any{}}})
				}
				f.send(out)
				continue
			}
			var msg rpcMessage
			if json.Unmarshal([]byte(line), &msg) == nil {
				go handle(f, msg)
			}
		}
	}()
	c := NewStreamClient(clientReader, clientWriter, "")
	t.Cleanup(func() {
		c.Close()
		clientWriter.Close()
		<-done
		serverWriter.Close()
	})
	return c, f
}

// legacyStdioHandler answers like a Legacy stdio server negotiating version:
// server/discover is Method not found, initialize succeeds, and tools/call
// echoes the tool name unless it is "slow" (never answered).
func legacyStdioHandler(version string) func(f *fakeStdio, msg rpcMessage) {
	return func(f *fakeStdio, msg rpcMessage) {
		switch msg.Method {
		case "server/discover":
			f.send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": ErrorCodeMethodNotFound, "message": "Method not found"}})
		case "initialize":
			f.result(msg.ID, legacyInitializeResult(version))
		case "tools/call":
			if msg.Params["name"] == "slow" {
				return
			}
			f.result(msg.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(msg.Params["name"])}}})
		}
	}
}

// --- 1. Legacy 404 re-initialize --------------------------------------------

func TestClient_Legacy404ReinitializesAndRetries(t *testing.T) {
	var sessions atomic.Int32
	var mu sync.Mutex
	var listSessions []string
	ts := newLegacyHTTPServer(t, func() string {
		return fmt.Sprintf("s%d", sessions.Add(1))
	}, func(w http.ResponseWriter, r *http.Request, msg rpcMessage) {
		if msg.Method != "tools/list" {
			w.WriteHeader(http.StatusOK)
			return
		}
		sid := r.Header.Get(headerSessionID)
		mu.Lock()
		listSessions = append(listSessions, sid)
		mu.Unlock()
		if sid == "s1" {
			http.NotFound(w, r) // the session expired
			return
		}
		writeRPCResult(w, msg.ID, map[string]any{"tools": []any{map[string]any{"name": "t", "inputSchema": map[string]any{"type": "object"}}}})
	})

	c := NewClient(ts.URL, nil, "")
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools = %+v", tools)
	}
	if got := sessions.Load(); got != 2 {
		t.Fatalf("initialize ran %d times, want 2", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(listSessions, ",") != "s1,s2" {
		t.Fatalf("tools/list sessions = %v, want [s1 s2]", listSessions)
	}
	if got := c.getSessionID(); got != "s2" {
		t.Fatalf("session = %q, want s2", got)
	}
}

// --- 2. DELETE on Close ---------------------------------------------------

type recordedDelete struct {
	session, version, auth string
}

func TestClient_CloseDeletesLegacySession(t *testing.T) {
	var mu sync.Mutex
	var deletes []recordedDelete
	ts := newLegacyHTTPServer(t, constSession("sess-1"), func(w http.ResponseWriter, r *http.Request, msg rpcMessage) {
		if r.Method == http.MethodDelete {
			mu.Lock()
			deletes = append(deletes, recordedDelete{r.Header.Get(headerSessionID), r.Header.Get(headerProtocolVersion), r.Header.Get("Authorization")})
			mu.Unlock()
			w.WriteHeader(http.StatusMethodNotAllowed) // must be tolerated
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	c := NewClient(ts.URL, staticAuth{"Bearer tok"}, "")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(deletes) != 1 {
		t.Fatalf("DELETEs = %d, want 1", len(deletes))
	}
	d := deletes[0]
	if d.session != "sess-1" || d.version != MCPProtocolVersionLatest || d.auth != "Bearer tok" {
		t.Fatalf("DELETE headers = %+v", d)
	}
}

func TestClient_UnsupportedNegotiatedVersionDeletesSession(t *testing.T) {
	var deleted atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted.Store(r.Header.Get(headerSessionID))
			return
		}
		var msg rpcMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		if msg.Method == "initialize" {
			w.Header().Set(headerSessionID, "sess-x")
			writeRPCResult(w, msg.ID, legacyInitializeResult("1999-01-01"))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	if err := c.Initialize(context.Background()); err == nil {
		t.Fatal("Initialize succeeded against an unsupported negotiated version")
	}
	if got, _ := deleted.Load().(string); got != "sess-x" {
		t.Fatalf("DELETE session = %q, want sess-x", got)
	}
}

// --- 3. Legacy GET notification stream --------------------------------------

func TestClient_EventStreamHonoursRetryAndLastEventID(t *testing.T) {
	var mu sync.Mutex
	var connects []time.Time
	var lastEventIDs []string
	ts := newLegacyHTTPServer(t, constSession("s"), func(w http.ResponseWriter, r *http.Request, msg rpcMessage) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		n := len(connects)
		connects = append(connects, time.Now())
		lastEventIDs = append(lastEventIDs, r.Header.Get("Last-Event-ID"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if n == 0 {
			fmt.Fprintf(w, "id: ev-1\nretry: 1500\ndata: {\"jsonrpc\":\"2.0\",\"method\":%q}\n\n", NotificationToolsChanged)
			return // end the stream: the client must wait retry before reconnecting
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})

	c := NewClient(ts.URL, nil, "")
	c.EnableNotifications()
	defer c.Close()
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	waitFor(t, "reconnect", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(connects) >= 2
	})
	mu.Lock()
	defer mu.Unlock()
	if gap := connects[1].Sub(connects[0]); gap < 1400*time.Millisecond {
		t.Fatalf("reconnected after %v, want at least the server's retry of 1.5s", gap)
	}
	if lastEventIDs[0] != "" || lastEventIDs[1] != "ev-1" {
		t.Fatalf("Last-Event-ID headers = %q, want [\"\" \"ev-1\"]", lastEventIDs)
	}
}

func TestClient_EventStream405StopsRetrying(t *testing.T) {
	var gets atomic.Int32
	ts := newLegacyHTTPServer(t, constSession("s"), func(w http.ResponseWriter, r *http.Request, msg rpcMessage) {
		if r.Method == http.MethodGet {
			gets.Add(1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	c := NewClient(ts.URL, nil, "")
	c.EnableNotifications()
	defer c.Close()
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	waitFor(t, "first GET", func() bool { return gets.Load() >= 1 })
	time.Sleep(1300 * time.Millisecond) // past the first 1s backoff
	if got := gets.Load(); got != 1 {
		t.Fatalf("GET attempts = %d, want 1 (405 means no stream is offered)", got)
	}
}

func TestClient_EventStreamAnswersPing(t *testing.T) {
	replied := make(chan rpcMessage, 1)
	ts := newLegacyHTTPServer(t, constSession("s"), func(w http.ResponseWriter, r *http.Request, msg rpcMessage) {
		switch {
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"p1\",\"method\":\"ping\"}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == http.MethodPost && msg.Method == "":
			select {
			case replied <- msg:
			default:
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	c := NewClient(ts.URL, nil, "")
	c.EnableNotifications()
	defer c.Close()
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	select {
	case msg := <-replied:
		if string(msg.ID) != `"p1"` {
			t.Fatalf("reply id = %s, want \"p1\"", msg.ID)
		}
		if m, ok := msg.Result.(map[string]any); !ok || len(m) != 0 {
			t.Fatalf("reply result = %#v, want {}", msg.Result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client did not answer the server's ping on the GET stream")
	}
}

// --- 4. stdio ping --------------------------------------------------------

func TestStdioClient_AnswersServerPing(t *testing.T) {
	_, f := newFakeStdioClient(t, func(f *fakeStdio, msg rpcMessage) {})
	f.send(map[string]any{"jsonrpc": "2.0", "id": "srv-1", "method": "ping"})
	waitFor(t, "ping response", func() bool {
		for _, line := range f.recorded() {
			var msg rpcMessage
			if json.Unmarshal([]byte(line), &msg) == nil && string(msg.ID) == `"srv-1"` && msg.Method == "" {
				m, ok := msg.Result.(map[string]any)
				return ok && len(m) == 0
			}
		}
		return false
	})
}

// --- 5. Unique request ids ------------------------------------------------

func TestClient_ParallelCallsOfOneToolUseDistinctIDs(t *testing.T) {
	var mu sync.Mutex
	ids := map[string]bool{}
	ts := newLegacyHTTPServer(t, constSession("s"), func(w http.ResponseWriter, r *http.Request, msg rpcMessage) {
		if msg.Method != "tools/call" {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		ids[string(msg.ID)] = true
		mu.Unlock()
		writeRPCResult(w, msg.ID, map[string]any{"content": []any{}})
	})

	c := NewClient(ts.URL, nil, "")
	results := c.CallToolsParallel(context.Background(), []ToolCall{{Name: "same"}, {Name: "same"}, {Name: "same"}})
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("results[%d]: %v", i, r.Err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ids) != 3 {
		t.Fatalf("distinct tools/call ids = %v, want 3", ids)
	}
}

// --- 6. Modern probe -----------------------------------------------------------

func TestClient_ModernProbeErrorHandling(t *testing.T) {
	cases := []struct {
		name     string
		discover func(w http.ResponseWriter, id json.RawMessage)
		wantErr  bool
	}{
		{"HeaderMismatch", func(w http.ResponseWriter, id json.RawMessage) {
			writeRPCError(w, http.StatusBadRequest, id, ErrorCodeHeaderMismatch, nil)
		}, true},
		{"MissingRequiredClientCapability", func(w http.ResponseWriter, id json.RawMessage) {
			writeRPCError(w, http.StatusBadRequest, id, ErrorCodeMissingRequiredClientCapability, nil)
		}, true},
		{"UnsupportedVersion only Modern", func(w http.ResponseWriter, id json.RawMessage) {
			writeRPCError(w, http.StatusBadRequest, id, ErrorCodeUnsupportedProtocolVersion, map[string]any{"supported": []string{MCPProtocolVersionModern}})
		}, true},
		{"UnsupportedVersion only Legacy", func(w http.ResponseWriter, id json.RawMessage) {
			writeRPCError(w, http.StatusBadRequest, id, ErrorCodeUnsupportedProtocolVersion, map[string]any{"supported": []string{"2025-06-18", "2025-11-25"}})
		}, false},
		{"MethodNotFound", func(w http.ResponseWriter, id json.RawMessage) {
			writeRPCError(w, http.StatusNotFound, id, ErrorCodeMethodNotFound, nil)
		}, false},
		{"405", func(w http.ResponseWriter, id json.RawMessage) {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}, false},
		{"400 non-JSON", func(w http.ResponseWriter, id json.RawMessage) {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "bad request")
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sawInitialize atomic.Bool
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var msg rpcMessage
				_ = json.NewDecoder(r.Body).Decode(&msg)
				switch msg.Method {
				case "server/discover":
					tc.discover(w, msg.ID)
				case "initialize":
					sawInitialize.Store(true)
					writeRPCResult(w, msg.ID, legacyInitializeResult(MCPProtocolVersionLatest))
				default:
					w.WriteHeader(http.StatusAccepted)
				}
			}))
			defer ts.Close()

			c := NewClient(ts.URL, nil, "")
			err := c.Initialize(context.Background())
			if tc.wantErr {
				var toolErr *ToolError
				if !errors.As(err, &toolErr) {
					t.Fatalf("Initialize err = %v, want the server's Modern error", err)
				}
				if sawInitialize.Load() {
					t.Fatal("client fell back to initialize against a Modern server")
				}
				return
			}
			if err != nil {
				t.Fatalf("Initialize: %v", err)
			}
			if !sawInitialize.Load() || c.era != eraLegacy {
				t.Fatal("client did not fall back to the Legacy initialize handshake")
			}
		})
	}
}

// --- 7. resultType ----------------------------------------------------------

// newModernHTTPServer is a fake Modern server whose every result carries
// resultType (omitted when "").
func newModernHTTPServer(t *testing.T, resultType string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg rpcMessage
		_ = json.NewDecoder(r.Body).Decode(&msg)
		var result map[string]any
		switch msg.Method {
		case "server/discover":
			writeRPCResult(w, msg.ID, map[string]any{"supportedVersions": []string{MCPProtocolVersionModern}})
			return
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "t", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []any{}}
		case "resources/read":
			result = map[string]any{"contents": []any{}}
		case "prompts/get":
			result = map[string]any{"messages": []any{}}
		default:
			result = map[string]any{}
		}
		if resultType != "" {
			result["resultType"] = resultType
		}
		writeRPCResult(w, msg.ID, result)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestClient_ModernResultType(t *testing.T) {
	ctx := context.Background()
	for _, rt := range []string{"", "complete"} {
		c := NewClient(newModernHTTPServer(t, rt).URL, nil, "")
		if _, err := c.CallTool(ctx, "t", nil); err != nil {
			t.Fatalf("resultType %q: CallTool: %v", rt, err)
		}
		if _, err := c.GetPrompt(ctx, "p", nil); err != nil {
			t.Fatalf("resultType %q: GetPrompt: %v", rt, err)
		}
	}

	c := NewClient(newModernHTTPServer(t, "input_required").URL, nil, "")
	if _, err := c.ListTools(ctx); !errors.Is(err, ErrInputRequired) {
		t.Fatalf("ListTools err = %v, want ErrInputRequired", err)
	}
	if _, err := c.ReadResource(ctx, "file:///x"); !errors.Is(err, ErrInputRequired) {
		t.Fatalf("ReadResource err = %v, want ErrInputRequired", err)
	}

	c = NewClient(newModernHTTPServer(t, "bogus").URL, nil, "")
	if _, err := c.GetPrompt(ctx, "p", nil); err == nil || !strings.Contains(err.Error(), "resultType") {
		t.Fatalf("GetPrompt err = %v, want an invalid resultType error", err)
	}
}

func TestClient_LegacyResultTypeIgnored(t *testing.T) {
	ts := newLegacyHTTPServer(t, constSession("s"), func(w http.ResponseWriter, r *http.Request, msg rpcMessage) {
		writeRPCResult(w, msg.ID, map[string]any{"content": []any{}, "resultType": "input_required"})
	})
	c := NewClient(ts.URL, nil, "")
	if _, err := c.CallTool(context.Background(), "t", nil); err != nil {
		t.Fatalf("CallTool on a Legacy server: %v", err)
	}
}

// --- 8. stdio batching ------------------------------------------------------

func TestStdioCallToolsParallel_BatchOnlyForBatchingRevisions(t *testing.T) {
	for _, tc := range []struct {
		version   string
		wantBatch bool
	}{
		{"2025-11-25", false},
		{"2025-06-18", false},
		{"2025-03-26", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			c, f := newFakeStdioClient(t, legacyStdioHandler(tc.version))
			results := c.CallToolsParallel(context.Background(), []ToolCall{{Name: "a"}, {Name: "b"}, {Name: "c"}})
			for i, r := range results {
				if r.Err != nil {
					t.Fatalf("results[%d]: %v", i, r.Err)
				}
			}
			sawBatch := false
			for _, line := range f.recorded() {
				if strings.HasPrefix(strings.TrimSpace(line), "[") {
					sawBatch = true
				}
			}
			if sawBatch != tc.wantBatch {
				t.Fatalf("sent a JSON-RPC batch = %v, want %v", sawBatch, tc.wantBatch)
			}
		})
	}
}

// --- 9 & 10. stdio probe timeout and cancellation ----------------------------

func TestStdioClient_ProbeTimeoutFallsBackToInitialize(t *testing.T) {
	c, f := newFakeStdioClient(t, func(f *fakeStdio, msg rpcMessage) {
		if msg.Method == "initialize" {
			f.result(msg.ID, legacyInitializeResult(MCPProtocolVersionLatest))
		}
		// server/discover is ignored, as some Legacy servers do before initialize
	})
	c.probeTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Initialize(ctx); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if c.era != eraLegacy {
		t.Fatalf("era = %v, want eraLegacy", c.era)
	}

	// The abandoned probe is cancelled on the wire.
	discover, _ := f.findMessage("server/discover")
	waitFor(t, "notifications/cancelled for server/discover", func() bool {
		cancelled, ok := f.findMessage("notifications/cancelled")
		return ok && fmt.Sprint(cancelled.Params["requestId"]) == string(discover.ID)
	})
}

func TestStdioClient_CancelledRequestSendsNotification(t *testing.T) {
	c, f := newFakeStdioClient(t, legacyStdioHandler(MCPProtocolVersionLatest))
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.CallTool(ctx, "slow", nil); err == nil {
		t.Fatal("CallTool succeeded without a response")
	}
	call, ok := f.findMessage("tools/call")
	if !ok {
		t.Fatal("tools/call never sent")
	}
	waitFor(t, "notifications/cancelled", func() bool {
		cancelled, ok := f.findMessage("notifications/cancelled")
		return ok && fmt.Sprint(cancelled.Params["requestId"]) == string(call.ID) && cancelled.Params["reason"] != ""
	})
	if _, ok := f.findMessage("initialize"); !ok {
		t.Fatal("initialize never sent")
	}
}

func TestClient_LegacyHTTPCancelledRequestSendsNotification(t *testing.T) {
	cancelled := make(chan rpcMessage, 1)
	callID := make(chan string, 1)
	ts := newLegacyHTTPServer(t, constSession("s"), func(w http.ResponseWriter, r *http.Request, msg rpcMessage) {
		switch msg.Method {
		case "tools/call":
			callID <- string(msg.ID)
			<-r.Context().Done()
		case "notifications/cancelled":
			if r.Header.Get(headerSessionID) == "s" {
				cancelled <- msg
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})

	c := NewClient(ts.URL, nil, "")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.CallTool(ctx, "slow", nil); err == nil {
		t.Fatal("CallTool succeeded without a response")
	}
	id := <-callID
	select {
	case msg := <-cancelled:
		if got := fmt.Sprint(msg.Params["requestId"]); got != id {
			t.Fatalf("cancelled requestId = %s, want %s", got, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notifications/cancelled POST")
	}
}

// --- 11. Modern stdio subscription ------------------------------------------------

func TestStdioClient_ModernSubscribesForListChanged(t *testing.T) {
	var fired atomic.Bool
	c, f := newFakeStdioClient(t, func(f *fakeStdio, msg rpcMessage) {
		switch msg.Method {
		case "server/discover":
			f.result(msg.ID, map[string]any{"supportedVersions": []string{MCPProtocolVersionModern}})
		case "subscriptions/listen":
			var subID any
			_ = json.Unmarshal(msg.ID, &subID)
			meta := map[string]any{metaKeySubscriptionID: subID}
			f.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/subscriptions/acknowledged", "params": map[string]any{"_meta": meta}})
			f.send(map[string]any{"jsonrpc": "2.0", "method": NotificationToolsChanged, "params": map[string]any{"_meta": meta}})
			// The listen request stays open until the client cancels it.
		}
	})
	c.OnToolsChanged(func() { fired.Store(true) })
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if c.era != eraModern {
		t.Fatalf("era = %v, want eraModern", c.era)
	}
	waitFor(t, "OnToolsChanged", fired.Load)

	listen, ok := f.findMessage("subscriptions/listen")
	if !ok {
		t.Fatal("subscriptions/listen never sent")
	}
	c.Close()
	waitFor(t, "notifications/cancelled for the subscription", func() bool {
		cancelled, ok := f.findMessage("notifications/cancelled")
		return ok && fmt.Sprint(cancelled.Params["requestId"]) == string(listen.ID)
	})
}

// A 404 on the Legacy GET notification stream re-initializes the session
// and reconnects with the new session id.
func TestClient_EventStream404Reinitializes(t *testing.T) {
	var mu sync.Mutex
	sessions := 0
	gets := []string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var req MCPRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.Method == "initialize" {
				mu.Lock()
				sessions++
				sid := fmt.Sprintf("sess-%d", sessions)
				mu.Unlock()
				w.Header().Set(headerSessionID, sid)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(MCPResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
					"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "s", "version": "1"},
				}})
				return
			}
			w.WriteHeader(http.StatusAccepted)
		case http.MethodGet:
			sid := r.Header.Get(headerSessionID)
			mu.Lock()
			gets = append(gets, sid)
			mu.Unlock()
			if sid == "sess-1" {
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	c.EnableNotifications()
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(gets) >= 2 && gets[len(gets)-1] == "sess-2"
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if sessions != 2 || len(gets) < 2 || gets[0] != "sess-1" || gets[len(gets)-1] != "sess-2" {
		t.Fatalf("sessions=%d gets=%v, want a re-initialize and a reconnect with sess-2", sessions, gets)
	}
}
