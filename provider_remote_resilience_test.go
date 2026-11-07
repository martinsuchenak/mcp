package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestRemoteProvider_ListTimeoutHonored verifies withListTimeout applies
// DefaultRemoteToolListTimeout when unset, a custom duration when set, and no
// bound at all when negative.
func TestRemoteProvider_ListTimeoutHonored(t *testing.T) {
	base := context.Background()

	ctx, cancel := (RemoteProviderConfig{}).withListTimeout(base)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected a deadline with zero-value ListTimeout")
	}
	if d := time.Until(deadline); d <= 0 || d > DefaultRemoteToolListTimeout {
		t.Fatalf("expected deadline within DefaultRemoteToolListTimeout, got %v", d)
	}

	ctx2, cancel2 := (RemoteProviderConfig{ListTimeout: 50 * time.Millisecond}).withListTimeout(base)
	defer cancel2()
	deadline2, ok := ctx2.Deadline()
	if !ok {
		t.Fatal("expected a deadline with explicit ListTimeout")
	}
	if d := time.Until(deadline2); d <= 0 || d > 50*time.Millisecond {
		t.Fatalf("expected deadline within 50ms, got %v", d)
	}

	ctx3, cancel3 := (RemoteProviderConfig{ListTimeout: -1}).withListTimeout(base)
	defer cancel3()
	if _, ok := ctx3.Deadline(); ok {
		t.Fatal("expected no deadline with negative ListTimeout")
	}
}

// TestRemoteProvider_CallTimeoutOptIn verifies CallTimeout is opt-in: unset
// (the zero value) leaves ExecuteTool waiting as long as the caller's context
// allows (unchanged default behaviour), while an explicit CallTimeout bounds
// a hung tool call.
func TestRemoteProvider_CallTimeoutOptIn(t *testing.T) {
	base := context.Background()

	ctx, cancel := (RemoteProviderConfig{}).withCallTimeout(base)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("expected no deadline with unset CallTimeout (opt-in, not a default)")
	}

	ctx2, cancel2 := (RemoteProviderConfig{CallTimeout: 50 * time.Millisecond}).withCallTimeout(base)
	defer cancel2()
	deadline, ok := ctx2.Deadline()
	if !ok {
		t.Fatal("expected a deadline with explicit CallTimeout")
	}
	if d := time.Until(deadline); d <= 0 || d > 50*time.Millisecond {
		t.Fatalf("expected deadline within 50ms, got %v", d)
	}
}

// TestRemoteProvider_CallTimeoutBoundsHungExecuteTool is the ExecuteTool
// counterpart to TestRemoteProvider_HangingServerDoesNotBlockOthers: without
// an explicit CallTimeout, calling a tool on a permanently-hung server with
// an unbounded caller context hangs forever (by design — a tool call may
// legitimately run long, so there is no default). Setting CallTimeout lets a
// host bound it anyway.
func TestRemoteProvider_CallTimeoutBoundsHungExecuteTool(t *testing.T) {
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(release)
		hang.Close()
	}()

	p := NewRemoteProvider(resolverFor(
		RemoteProviderConfig{Name: "hang", URL: hang.URL, Auth: NewBearerTokenAuth("t"), CallTimeout: 50 * time.Millisecond},
	))

	start := time.Now()
	// context.Background() has no deadline of its own — only cfg.CallTimeout
	// bounds this call.
	_, err := p.ExecuteTool(context.Background(), "hang__whatever", nil)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error calling a hung server")
	}
	if elapsed > time.Second {
		t.Fatalf("ExecuteTool took %v; CallTimeout should have bounded it near 50ms", elapsed)
	}
}

// TestRemoteProvider_HangingServerDoesNotBlockOthers is the core regression
// test for "one dead remote server brings down tool listing for everyone."
// A server that never responds must not prevent a healthy sibling's tools
// from being returned, and GetTools must return promptly (bounded by the
// hanging server's ListTimeout, not by however long it would take to hang
// forever) rather than only after every server has been tried in sequence.
func TestRemoteProvider_HangingServerDoesNotBlockOthers(t *testing.T) {
	good := newRemoteWithTools(func(s *Server) {
		s.RegisterTool(NewTool("ok", "ok"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return NewToolResponseText("ok"), nil
		})
	})
	defer good.Close()

	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release // never responds until the test releases it
	}))
	defer func() {
		close(release)
		hang.Close()
	}()

	p := NewRemoteProvider(resolverFor(
		RemoteProviderConfig{Name: "good", URL: good.URL, Auth: NewBearerTokenAuth("t")},
		RemoteProviderConfig{Name: "hang", URL: hang.URL, Auth: NewBearerTokenAuth("t"), ListTimeout: 50 * time.Millisecond},
	))

	start := time.Now()
	tools, err := p.GetTools(context.Background())
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("GetTools should skip the hanging server, got err=%v", err)
	}
	if len(tools) != 1 || tools[0].Name != "good__ok" {
		t.Fatalf("expected only good__ok, got %+v", tools)
	}
	// Generous margin over the 50ms ListTimeout to absorb scheduler jitter,
	// but tight enough to catch a regression to sequential, unbounded fetches.
	if elapsed > time.Second {
		t.Fatalf("GetTools took %v; the hanging server should not have blocked the good one", elapsed)
	}
}

// TestRemoteProvider_OnServerErrorCalledForUnreachable verifies the error hook
// fires (so a host can log/track health) when a server cannot be listed.
func TestRemoteProvider_OnServerErrorCalledForUnreachable(t *testing.T) {
	var mu sync.Mutex
	var gotName string
	var gotErr error
	calls := 0

	p := NewRemoteProvider(resolverFor(
		RemoteProviderConfig{Name: "bad", URL: "http://127.0.0.1:0", Auth: NewBearerTokenAuth("t")},
	), WithOnServerError(func(cfg RemoteProviderConfig, err error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		gotName = cfg.Name
		gotErr = err
	}))

	tools, err := p.GetTools(context.Background())
	if err != nil || len(tools) != 0 {
		t.Fatalf("expected no tools and no top-level error, got tools=%+v err=%v", tools, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected OnServerError called exactly once, got %d", calls)
	}
	if gotName != "bad" {
		t.Fatalf("expected error for server %q, got %q", "bad", gotName)
	}
	if gotErr == nil {
		t.Fatal("expected a non-nil error passed to the hook")
	}
}

// TestRemoteProvider_OnServerErrorNotCalledForToolError verifies that a tool
// call which reaches the remote server and fails on its own terms (a
// *ToolError, whether the handler raised one explicitly or returned a plain
// error) is NOT treated as a server-health failure — the server is fine, only
// this call failed.
func TestRemoteProvider_OnServerErrorNotCalledForToolError(t *testing.T) {
	ts := newRemoteWithTools(func(s *Server) {
		s.RegisterTool(NewTool("explicit_tool_error", "always fails with a ToolError"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return nil, NewToolErrorInvalidParams("bad params")
		})
		s.RegisterTool(NewTool("plain_error", "always fails with a plain error"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return nil, errors.New("boom")
		})
	})
	defer ts.Close()

	calls := 0
	p := NewRemoteProvider(resolverFor(
		RemoteProviderConfig{Name: "svc", URL: ts.URL, Auth: NewBearerTokenAuth("t")},
	), WithOnServerError(func(cfg RemoteProviderConfig, err error) {
		calls++
	}))

	if _, err := p.ExecuteTool(context.Background(), "svc__explicit_tool_error", nil); err == nil {
		t.Fatal("expected an error from the tool call")
	}
	// A plain handler error is a tool execution error: reported inside the
	// result with isError set (per the spec), not as a JSON-RPC error.
	if resp, err := p.ExecuteTool(context.Background(), "svc__plain_error", nil); err != nil || resp == nil || !resp.IsError {
		t.Fatalf("expected an isError result from the tool call, got resp=%+v err=%v", resp, err)
	}
	if calls != 0 {
		t.Fatalf("expected OnServerError NOT called for application-level tool errors, got %d calls", calls)
	}
}

// TestRemoteProvider_OnServerErrorCalledForExecuteTransportFailure verifies
// the hook DOES fire when a tool call fails at the transport level (the
// server itself is unreachable), as opposed to responding with its own error.
func TestRemoteProvider_OnServerErrorCalledForExecuteTransportFailure(t *testing.T) {
	calls := 0
	p := NewRemoteProvider(resolverFor(
		RemoteProviderConfig{Name: "bad", URL: "http://127.0.0.1:0", Auth: NewBearerTokenAuth("t")},
	), WithOnServerError(func(cfg RemoteProviderConfig, err error) {
		calls++
	}))

	if _, err := p.ExecuteTool(context.Background(), "bad__whatever", nil); err == nil {
		t.Fatal("expected an error calling an unreachable server")
	}
	if calls != 1 {
		t.Fatalf("expected OnServerError called once for a transport failure, got %d", calls)
	}
}
