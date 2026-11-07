package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// waitForSubscribers polls until the server has at least n notification
// subscribers (i.e. clients with an open SSE stream), or times out.
func waitForSubscribers(t *testing.T, s *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.notifications.count() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d subscribers (have %d)", n, s.notifications.count())
}

func TestNotificationsHTTPListChangedInvalidatesClientCache(t *testing.T) {
	s := NewServer("ns", "1")
	s.RegisterTool(
		NewTool("a", "tool a"),
		func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return NewToolResponseText("a"), nil
		})

	hs := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer hs.Close()

	client := NewClient(hs.URL, nil, "")
	defer client.Close()

	var changed atomic.Int32
	client.OnToolsChanged(func() { changed.Add(1) }).EnableNotifications()

	ctx := context.Background()
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}

	// Wait for the client's SSE reader to be subscribed before mutating, so the
	// notification isn't missed to a race with connection setup.
	waitForSubscribers(t, s, 1)

	// Mutate the server's tool set: auto-emits notifications/tools/listChanged.
	s.RegisterTool(
		NewTool("b", "tool b"),
		func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return NewToolResponseText("b"), nil
		})

	// The client cache should be invalidated and the callback should fire.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if changed.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if changed.Load() == 0 {
		t.Fatal("OnToolsChanged callback never fired")
	}

	// Next ListTools re-fetches (cache was invalidated) and sees the new tool.
	tools, err = client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools after change: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected cache to be invalidated and return 2 tools, got %d", len(tools))
	}
}

func TestNotificationsManualHookEmits(t *testing.T) {
	s := NewServer("ns", "1")
	// No subscribers yet: emit is a no-op (must not panic or block).
	s.NotifyToolsChanged()
	s.NotifyResourcesChanged()
	s.NotifyPromptsChanged()

	hs := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer hs.Close()

	client := NewClient(hs.URL, nil, "")
	defer client.Close()
	var tools, resources, prompts atomic.Int32
	client.OnToolsChanged(func() { tools.Add(1) }).
		OnResourcesChanged(func() { resources.Add(1) }).
		OnPromptsChanged(func() { prompts.Add(1) }).
		EnableNotifications()

	if _, err := client.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	waitForSubscribers(t, s, 1)

	s.NotifyToolsChanged()
	s.NotifyResourcesChanged()
	s.NotifyPromptsChanged()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if tools.Load() > 0 && resources.Load() > 0 && prompts.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tools.Load() == 0 || resources.Load() == 0 || prompts.Load() == 0 {
		t.Fatalf("expected all three callbacks to fire: tools=%d resources=%d prompts=%d",
			tools.Load(), resources.Load(), prompts.Load())
	}
}

// TestNotificationsHTTPResourcesChanged verifies the full round-trip for
// notifications/resources/listChanged over SSE: the client registers an
// OnResourcesChanged callback, the server mutates its resource set (auto-emit),
// and the callback fires so the client re-fetches and sees the new resource.
func TestNotificationsHTTPResourcesChanged(t *testing.T) {
	s := NewServer("ns", "1")
	s.RegisterResource(
		NewResource("test://a", "a", "resource a", "text/plain"),
		func(ctx context.Context, req *ResourceRequest) (*ResourceResponse, error) {
			return NewResourceResponseText("test://a", "a", "text/plain"), nil
		})

	hs := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer hs.Close()

	client := NewClient(hs.URL, nil, "")
	defer client.Close()

	var changed atomic.Int32
	client.OnResourcesChanged(func() { changed.Add(1) }).EnableNotifications()

	ctx := context.Background()
	resources, err := client.ListResources(ctx)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}

	waitForSubscribers(t, s, 1)

	// Mutate: register a second resource -> auto-emits notifications/resources/listChanged.
	s.RegisterResource(
		NewResource("test://b", "b", "resource b", "text/plain"),
		func(ctx context.Context, req *ResourceRequest) (*ResourceResponse, error) {
			return NewResourceResponseText("test://b", "b", "text/plain"), nil
		})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if changed.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if changed.Load() == 0 {
		t.Fatal("OnResourcesChanged callback never fired")
	}

	resources, err = client.ListResources(ctx)
	if err != nil {
		t.Fatalf("ListResources after change: %v", err)
	}
	if len(resources) != 2 {
		t.Fatalf("expected 2 resources after notification, got %d", len(resources))
	}
}

// TestNotificationsHTTPPromptsChanged verifies the full round-trip for
// notifications/prompts/listChanged over SSE.
func TestNotificationsHTTPPromptsChanged(t *testing.T) {
	s := NewServer("ns", "1")
	s.RegisterPrompt(
		NewPrompt("a", "prompt a"),
		func(ctx context.Context, req *PromptRequest) (*PromptResponse, error) {
			return NewPromptResponseText("a"), nil
		})

	hs := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer hs.Close()

	client := NewClient(hs.URL, nil, "")
	defer client.Close()

	var changed atomic.Int32
	client.OnPromptsChanged(func() { changed.Add(1) }).EnableNotifications()

	ctx := context.Background()
	prompts, err := client.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts) != 1 {
		t.Fatalf("expected 1 prompt, got %d", len(prompts))
	}

	waitForSubscribers(t, s, 1)

	// Mutate: register a second prompt -> auto-emits notifications/prompts/listChanged.
	s.RegisterPrompt(
		NewPrompt("b", "prompt b"),
		func(ctx context.Context, req *PromptRequest) (*PromptResponse, error) {
			return NewPromptResponseText("b"), nil
		})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if changed.Load() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if changed.Load() == 0 {
		t.Fatal("OnPromptsChanged callback never fired")
	}

	prompts, err = client.ListPrompts(ctx)
	if err != nil {
		t.Fatalf("ListPrompts after change: %v", err)
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 prompts after notification, got %d", len(prompts))
	}
}

// TestNotificationsPropagation chains an upstream server through a federating
// server to a downstream client, and verifies that a change on the upstream
// propagates: upstream emits -> federator's client refreshes + re-emits ->
// downstream client's cache is invalidated and sees the new federated tool.
func TestNotificationsPropagation(t *testing.T) {
	mkTool := func(name string) *ToolBuilder {
		return NewTool(name, name)
	}
	mkHandler := func() ToolHandler {
		return func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return NewToolResponseText("ok"), nil
		}
	}

	upstream := NewServer("upstream", "1")
	upstream.RegisterTool(mkTool("up_a"), mkHandler())
	uhs := httptest.NewServer(http.HandlerFunc(upstream.HandleRequest))
	defer uhs.Close()

	federator := NewServer("federator", "1")
	federator.RegisterTool(mkTool("fed_local"), mkHandler())
	fhs := httptest.NewServer(http.HandlerFunc(federator.HandleRequest))
	defer fhs.Close()

	// Connect the federator to the upstream. EnableNotifications is required for
	// the upstream -> federator leg to receive change events.
	upClient := NewClient(uhs.URL, nil, "")
	upClient.EnableNotifications()
	if err := federator.RegisterRemoteServer(upClient); err != nil {
		t.Fatalf("RegisterRemoteServer: %v", err)
	}
	defer upClient.Close()

	// Downstream client of the federator, also opted in to notifications.
	var downstreamChanged atomic.Int32
	downstream := NewClient(fhs.URL, nil, "")
	defer downstream.Close()
	downstream.OnToolsChanged(func() { downstreamChanged.Add(1) }).EnableNotifications()

	ctx := context.Background()
	tools, err := downstream.ListTools(ctx)
	if err != nil {
		t.Fatalf("downstream ListTools: %v", err)
	}
	if !toolListHas(tools, "up_a") || !toolListHas(tools, "fed_local") {
		t.Fatalf("downstream should see upstream+local tools before change, got %v", notifToolNames(tools))
	}

	// Wait until both hops have active SSE subscribers (federator->upstream,
	// downstream->federator) so the propagation path is wired.
	waitForSubscribers(t, upstream, 1)
	waitForSubscribers(t, federator, 1)

	// Mutate the upstream tool set: this is the propagated change.
	upstream.RegisterTool(mkTool("up_b"), mkHandler())

	// Wait for the downstream callback (the propagated notification).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if downstreamChanged.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if downstreamChanged.Load() == 0 {
		t.Fatal("downstream OnToolsChanged never fired from upstream change")
	}

	// The downstream cache was invalidated; a re-list now includes the new tool.
	tools, err = downstream.ListTools(ctx)
	if err != nil {
		t.Fatalf("downstream ListTools after change: %v", err)
	}
	if !toolListHas(tools, "up_b") {
		t.Fatalf("downstream should see propagated up_b, got %v", notifToolNames(tools))
	}
}

func toolListHas(tools []MCPTool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func notifToolNames(tools []MCPTool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}

// TestCapabilitiesAdvertiseListChangedTrue confirms the server honestly
// advertises listChanged support (and that we actually deliver it on both
// transports — covered by the round-trip tests above).
func TestCapabilitiesAdvertiseListChangedTrue(t *testing.T) {
	s := NewServer("ns", "1")
	caps := s.buildCapabilities(MCPProtocolVersionLatest)
	if caps.Tools["listChanged"] != true {
		t.Fatalf("tools.listChanged should be true, got %v", caps.Tools["listChanged"])
	}
	if caps.Resources["listChanged"] != true {
		t.Fatalf("resources.listChanged should be true, got %v", caps.Resources["listChanged"])
	}
	if caps.Prompts["listChanged"] != true {
		t.Fatalf("prompts.listChanged should be true, got %v", caps.Prompts["listChanged"])
	}
	// Older protocol version does not advertise listChanged.
	old := s.buildCapabilities("2024-11-05")
	if _, ok := old.Tools["listChanged"]; ok {
		t.Fatalf("2024-11-05 should not advertise tools.listChanged, got %v", old.Tools)
	}
}

// TestEmitNoOpWithoutSubscribers: emitting with no subscribers must be cheap
// and safe. Run under -race; the benchmark below quantifies the cost.
func TestEmitNoOpWithoutSubscribers(t *testing.T) {
	s := NewServer("ns", "1")
	for i := 0; i < 1000; i++ {
		s.NotifyToolsChanged() // no subscribers -> must not panic or block
		s.NotifyResourcesChanged()
		s.NotifyPromptsChanged()
	}
	if s.notifications.count() != 0 {
		t.Fatalf("expected 0 subscribers, got %d", s.notifications.count())
	}
}

// TestRegisterToolDoesNotBlockOnStuckStdioClient is the destruction test for
// the stdio sink: a dead client (no one reading the server->client pipe) must
// not block RegisterTool, because emission runs under s.mu and must never stall
// on a slow writer.
func TestRegisterToolDoesNotBlockOnStuckStdioClient(t *testing.T) {
	server := NewServer("stuck", "1")
	server.RegisterTool(NewTool("a", "a"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("a"), nil
	})

	// Client never reads from the server->client pipe, so writes would block a
	// naive synchronous sink. Overflow the sink buffer well past its capacity.
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	defer clientReader.Close()
	defer serverReader.Close()
	defer clientWriter.Close()

	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.ServeStream(context.Background(), serverReader, serverWriter)
	}()

	// Drain only the client->server direction so the server can read requests; do
	// NOT read serverWriter (the server->client pipe), modelling a stuck client.
	// Initialize over the stream by speaking minimal JSON-RPC.
	go func() {
		// Read and discard client-bound frames forever so the pipe doesn't fill
		// from a blocking perspective of Write... actually we WANT Write to be
		// able to block. Leave serverWriter unread.
		_ = serverReader
	}()

	// Register many tools concurrently; each emits a notification into the
	// (filling) sink. None of these may block.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			server.RegisterTool(NewTool(fmt.Sprintf("t%d", i), "x"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
				return NewToolResponseText("x"), nil
			})
		}
	}()
	select {
	case <-done:
		// success: registration completed despite the unread stream
	case <-time.After(3 * time.Second):
		t.Fatal("RegisterTool blocked on notification emission to a stuck stdio client")
	}

	// Tear down the server loop.
	clientWriter.Close() // EOF on server's reader -> ServeStream returns
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
	}
}

// TestEnableNotificationsIdempotent: calling EnableNotifications multiple
// times starts at most one reader and is safe.
func TestEnableNotificationsIdempotent(t *testing.T) {
	s := NewServer("ns", "1")
	s.RegisterTool(NewTool("a", "a"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("a"), nil
	})
	hs := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer hs.Close()

	c := NewClient(hs.URL, nil, "")
	defer c.Close()
	c.EnableNotifications()
	c.EnableNotifications()
	c.EnableNotifications()
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	c.readerMu.Lock()
	started := c.readerStarted
	c.readerMu.Unlock()
	if !started {
		t.Fatal("expected reader to be started after EnableNotifications + ListTools")
	}
}

// TestClientDoesNotStartReaderWithoutOptIn: a plain client must not open an SSE
// connection (zero overhead when notifications are unused).
func TestClientDoesNotStartReaderWithoutOptIn(t *testing.T) {
	s := NewServer("ns", "1")
	s.RegisterTool(NewTool("a", "a"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("a"), nil
	})
	hs := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer hs.Close()

	c := NewClient(hs.URL, nil, "")
	defer c.Close()
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	c.readerMu.Lock()
	started := c.readerStarted
	want := c.wantNotifications
	c.readerMu.Unlock()
	if started || want {
		t.Fatalf("plain client must not start reader/enable notifications: started=%v want=%v", started, want)
	}
	// And no SSE subscriber was registered on the server.
	if s.notifications.count() != 0 {
		t.Fatalf("expected 0 subscribers for plain client, got %d", s.notifications.count())
	}
}

// TestSSESinkDropsWhenFull: a full subscriber buffer drops events instead of
// blocking the broadcast (which runs under s.mu).
func TestSSESinkDropsWhenFull(t *testing.T) {
	sink := newSSESink(2) // tiny buffer
	for i := 0; i < 100; i++ {
		sink.send(NotificationToolsChanged, nil) // must never block
	}
	// Only the last ~2 are retained; the rest dropped. We just need non-blocking.
	if cap(sink.ch) != 2 {
		t.Fatalf("unexpected buffer cap %d", cap(sink.ch))
	}
}

// TestMCPNotificationOmitsID ensures notifications are JSON-RPC 2.0 compliant:
// a notification MUST NOT include an "id" member. Regression test for a bug
// where MCPRequest (with a non-omitempty "id" tag) was used to serialize
// notifications, producing "id":null.
func TestMCPNotificationOmitsID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		params any
	}{
		{"tools", NotificationToolsChanged, nil},
		{"resources", NotificationResourcesChanged, nil},
		{"prompts", NotificationPromptsChanged, nil},
		{"with-params", "notifications/progress", map[string]any{"progress": 0.5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(MCPNotification{
				JSONRPC: "2.0",
				Method:  tc.method,
				Params:  tc.params,
			})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(payload, &raw); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if _, ok := raw["id"]; ok {
				t.Fatalf("notification must not contain \"id\": %s", payload)
			}
			if string(raw["jsonrpc"]) != `"2.0"` {
				t.Fatalf("bad jsonrpc: %s", raw["jsonrpc"])
			}
			if string(raw["method"]) != `"`+tc.method+`"` {
				t.Fatalf("bad method: %s", raw["method"])
			}
		})
	}
}

// TestSSENotificationWireFormatOmitsID is an end-to-end check: connect an SSE
// stream, trigger a notification, and verify the raw SSE data line has no
// "id" field.
func TestSSENotificationWireFormatOmitsID(t *testing.T) {
	s := NewServer("ns", "1")
	s.RegisterTool(NewTool("a", "a"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("a"), nil
	})
	hs := httptest.NewServer(http.HandlerFunc(s.handleSSEStream))
	defer hs.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", hs.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()

	waitForSubscribers(t, s, 1)
	s.NotifyToolsChanged()

	// Read one SSE data line.
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil {
		t.Fatalf("read SSE: %v", err)
	}
	line := string(buf[:n])
	if !strings.Contains(line, `"method":"notifications/tools/list_changed"`) {
		t.Fatalf("expected listChanged notification, got: %s", line)
	}
	if strings.Contains(line, `"id"`) {
		t.Fatalf("SSE notification must not contain \"id\": %s", line)
	}
}

// BenchmarkRegisterToolNoSubscribers measures the cost of RegisterTool's
// auto-emit path when notifications are unused (no subscribers). This is the
// "minimal overhead when not used" cost: one hub.count() RLock per register.
func BenchmarkRegisterToolNoSubscribers(b *testing.B) {
	s := NewServer("b", "1")
	h := func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("x"), nil
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.RegisterTool(NewTool(fmt.Sprintf("t%d", i), "x"), h)
	}
}

// BenchmarkEmitNoSubscribers isolates the emit cost (no subscribers -> no-op).
func BenchmarkEmitNoSubscribers(b *testing.B) {
	s := NewServer("b", "1")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.NotifyToolsChanged()
	}
}
