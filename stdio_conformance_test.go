package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// stdio transport conformance: newline framing with parse-error recovery,
// cancellation, era-gated list-changed delivery, and subscriptions/listen.

// stdioHarness runs ServeStream over pipes and reads its output line by line.
type stdioHarness struct {
	s      *Server
	in     *io.PipeWriter
	lines  chan string
	cancel context.CancelFunc
	done   chan error
}

func newStdioHarness(t *testing.T, s *Server) *stdioHarness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h := &stdioHarness{s: s, in: inW, lines: make(chan string, 64), cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- s.ServeStream(ctx, inR, outW); outW.Close() }()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			h.lines <- sc.Text()
		}
		close(h.lines)
	}()
	t.Cleanup(func() { cancel(); inW.Close() })
	return h
}

func (h *stdioHarness) send(t *testing.T, line string) {
	t.Helper()
	if _, err := io.WriteString(h.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (h *stdioHarness) next(t *testing.T, timeout time.Duration) string {
	t.Helper()
	select {
	case l, ok := <-h.lines:
		if !ok {
			t.Fatal("stream closed")
		}
		return l
	case <-time.After(timeout):
		t.Fatal("no line within " + timeout.String())
	}
	return ""
}

func (h *stdioHarness) none(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case l := <-h.lines:
		t.Fatalf("unexpected line: %s", l)
	case <-time.After(wait):
	}
}

func stdioToolServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer("stdio", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	return s
}

const stdioInit2025 = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

// A malformed line is answered with -32700 and the stream keeps serving.
func TestStdioMalformedLineRecovers(t *testing.T) {
	h := newStdioHarness(t, stdioToolServer(t))
	h.send(t, `{not json`)
	if l := h.next(t, 2*time.Second); !strings.Contains(l, `"code":-32700`) {
		t.Fatalf("want parse error, got %s", l)
	}
	h.send(t, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if l := h.next(t, 2*time.Second); !strings.Contains(l, `"id":2`) || !strings.Contains(l, `"result"`) {
		t.Fatalf("stream did not recover: %s", l)
	}
}

// notifications/cancelled cancels the request's context and suppresses its
// response.
func TestStdioCancellation(t *testing.T) {
	s := NewServer("stdio", "1")
	cancelled := make(chan bool, 1)
	s.RegisterTool(NewTool("slow", "waits"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		select {
		case <-ctx.Done():
			cancelled <- true
		case <-time.After(3 * time.Second):
			cancelled <- false
		}
		return NewToolResponseText("late"), nil
	})
	h := newStdioHarness(t, s)
	h.send(t, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow","arguments":{}}}`)
	time.Sleep(100 * time.Millisecond)
	h.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"reason":"user"}}`)
	select {
	case was := <-cancelled:
		if !was {
			t.Fatal("handler context was not cancelled")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("handler did not finish")
	}
	h.none(t, 300*time.Millisecond) // no response for the cancelled request
	h.send(t, `{"jsonrpc":"2.0","id":8,"method":"ping"}`)
	if l := h.next(t, 2*time.Second); !strings.Contains(l, `"id":8`) {
		t.Fatalf("got %s", l)
	}
}

// Legacy list-changed pushes are gated on a completed initialize:
// nothing before it, the spec's method name afterwards.
func TestStdioListChangedGating(t *testing.T) {
	s := stdioToolServer(t)
	h := newStdioHarness(t, s)

	s.NotifyToolsChanged()
	h.none(t, 200*time.Millisecond)

	h.send(t, stdioInit2025)
	h.next(t, 2*time.Second)
	s.NotifyToolsChanged()
	if l := h.next(t, 2*time.Second); !strings.Contains(l, `"method":"notifications/tools/list_changed"`) {
		t.Fatalf("got %s", l)
	}
}

// A Modern stdio client gets no unsolicited notifications; it subscribes
// with subscriptions/listen, is acknowledged first, then receives tagged
// notifications, and nothing more once it cancels the request.
func TestStdioModernSubscriptionsListen(t *testing.T) {
	s := stdioToolServer(t)
	h := newStdioHarness(t, s)
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

	h.send(t, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`)
	h.next(t, 2*time.Second)
	s.NotifyToolsChanged()
	h.none(t, 200*time.Millisecond)

	h.send(t, `{"jsonrpc":"2.0","id":"sub-1","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true,"resourceSubscriptions":["file:///x"]},`+meta+`}}`)
	ack := h.next(t, 2*time.Second)
	var ackMsg struct {
		Method string `json:"method"`
		Params struct {
			Meta          map[string]any `json:"_meta"`
			Notifications map[string]any `json:"notifications"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(ack), &ackMsg); err != nil || ackMsg.Method != "notifications/subscriptions/acknowledged" {
		t.Fatalf("want acknowledged first, got %s", ack)
	}
	if ackMsg.Params.Meta[metaKeySubscriptionID] != "sub-1" || ackMsg.Params.Notifications["toolsListChanged"] != true || len(ackMsg.Params.Notifications) != 1 {
		t.Fatalf("ack = %s", ack)
	}

	s.NotifyToolsChanged()
	if l := h.next(t, 2*time.Second); !strings.Contains(l, `"method":"notifications/tools/list_changed"`) || !strings.Contains(l, `"io.modelcontextprotocol/subscriptionId":"sub-1"`) {
		t.Fatalf("got %s", l)
	}
	s.NotifyPromptsChanged() // not subscribed
	h.none(t, 200*time.Millisecond)

	h.send(t, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"sub-1"}}`)
	time.Sleep(100 * time.Millisecond)
	s.NotifyToolsChanged()
	h.none(t, 300*time.Millisecond)
}

// Shutdown answers an open stdio subscription with a completion result.
func TestStdioModernSubscriptionsListenGracefulClose(t *testing.T) {
	s := stdioToolServer(t)
	h := newStdioHarness(t, s)
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	h.send(t, `{"jsonrpc":"2.0","id":"sub-2","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},`+meta+`}}`)
	h.next(t, 2*time.Second) // ack
	s.Shutdown()
	if l := h.next(t, 2*time.Second); !strings.Contains(l, `"method":"notifications/cancelled"`) || !strings.Contains(l, `"requestId":"sub-2"`) {
		t.Fatalf("want notifications/cancelled first, got %s", l)
	}
	l := h.next(t, 2*time.Second)
	if !strings.Contains(l, `"id":"sub-2"`) || !strings.Contains(l, `"resultType":"complete"`) || !strings.Contains(l, `"io.modelcontextprotocol/subscriptionId":"sub-2"`) {
		t.Fatalf("want completion result, got %s", l)
	}
}

// A Modern client is never sent a foreign code from the reserved range: the
// error becomes -32603 with the original code kept in data.
func TestModernReservedErrorCodeRemapped(t *testing.T) {
	s := NewServer("stdio", "1")
	s.RegisterTool(NewTool("limited", "fails"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return nil, NewToolError(-32050, "Rate limit exceeded", map[string]any{"retry_after": 60})
	})
	h := newStdioHarness(t, s)
	meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
	h.send(t, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"limited","arguments":{},`+meta+`}}`)
	l := h.next(t, 2*time.Second)
	if !strings.Contains(l, `"code":-32603`) || !strings.Contains(l, `"originalCode":-32050`) || !strings.Contains(l, `"retry_after":60`) {
		t.Fatalf("got %s", l)
	}
	// A Legacy client gets the code as the tool chose it.
	h.send(t, stdioInit2025)
	h.next(t, 2*time.Second)
	h.send(t, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"limited","arguments":{}}}`)
	if l := h.next(t, 2*time.Second); !strings.Contains(l, `"code":-32050`) {
		t.Fatalf("got %s", l)
	}
}

// A tool with an outputSchema must return conforming structuredContent;
// otherwise the result is a tool execution error, not an invalid result.
func TestStructuredOutputConformance(t *testing.T) {
	s := NewServer("s", "1")
	s.RegisterTool(NewTool("weather", "w", String("mode", "test mode"), Output(Number("temp", "", Required()), String("cond", "", Required()))), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		switch req.StringOr("mode", "") {
		case "none":
			return NewToolResponseText("just text"), nil
		case "missing":
			return NewToolResponseStructured(map[string]any{"temp": 21.5}), nil
		case "wrong":
			return NewToolResponseStructured(map[string]any{"temp": "hot", "cond": "sunny"}), nil
		}
		return NewToolResponseStructured(map[string]any{"temp": 21.5, "cond": "sunny"}), nil
	})
	for mode, wantErr := range map[string]bool{"ok": false, "none": true, "missing": true, "wrong": true} {
		resp, err := s.CallTool(context.Background(), "weather", map[string]any{"mode": mode})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		if resp.IsError != wantErr {
			t.Errorf("%s: isError = %v, want %v (%+v)", mode, resp.IsError, wantErr, resp.Content)
		}
	}
}

var _ = sync.Mutex{}
