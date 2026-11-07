package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/paularlott/jsonrpc"
)

// stdioTransport implements clientTransport over a newline-delimited JSON-RPC
// stream. It backs both an in-process stream client and a spawned subprocess
// client.
//
// When peer is set (every client built by NewStdioClient/NewStreamClient),
// requests go through the peer directly with the client's own request ids
// (jsonrpc.Peer implements jsonrpc.Transport), so the client knows each
// request's wire id: it needs it to send notifications/cancelled for an
// abandoned request and to correlate a subscriptions/listen subscription.
type stdioTransport struct {
	rpc  *jsonrpc.Client
	peer *jsonrpc.Peer
}

// cancelledNotifyTimeout bounds a best-effort notifications/cancelled write.
const cancelledNotifyTimeout = 2 * time.Second

func (t *stdioTransport) roundTrip(ctx context.Context, req *MCPRequest, resp *MCPResponse, respHeaders *http.Header) error {
	resp.JSONRPC = "2.0"
	resp.ID = req.ID
	if respHeaders != nil {
		*respHeaders = http.Header{}
	}

	if t.peer != nil && req.ID != nil {
		return t.peerRoundTrip(ctx, req, resp)
	}

	var result json.RawMessage
	err := t.rpc.Call(ctx, req.Method, req.Params, &result)
	if err != nil {
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) {
			resp.Error = &MCPError{Code: rpcErr.Code, Message: rpcErr.Message, Data: rpcErr.Data}
			return nil
		}
		return err
	}
	return decodeRawResult(result, resp)
}

// peerRoundTrip sends req through the peer with req.ID as its wire id. If ctx
// ends before the response arrives, the server is told with
// notifications/cancelled (2025-11-25 and 2026-07-28 §Cancellation; on stdio
// that notification is the only cancellation signal). initialize is never
// cancelled, as the spec requires.
func (t *stdioTransport) peerRoundTrip(ctx context.Context, req *MCPRequest, resp *MCPResponse) error {
	id, err := json.Marshal(req.ID)
	if err != nil {
		return err
	}
	var params json.RawMessage
	if req.Params != nil {
		if params, err = json.Marshal(req.Params); err != nil {
			return err
		}
	}
	r, err := t.peer.Call(ctx, &jsonrpc.Request{
		JSONRPC: json.RawMessage(`"2.0"`),
		Method:  req.Method,
		Params:  params,
		ID:      id,
	})
	if err != nil {
		if ctx.Err() != nil && req.Method != "initialize" {
			t.sendCancelled(req.ID, ctx.Err())
		}
		return err
	}
	if r.Error != nil {
		resp.Error = &MCPError{Code: r.Error.Code, Message: r.Error.Message, Data: r.Error.Data}
		return nil
	}
	return decodeRawResult(r.Result, resp)
}

// sendCancelled notifies the server that the request with id was abandoned.
// Best effort, on its own short-lived context: the request's is already done.
func (t *stdioTransport) sendCancelled(id any, cause error) {
	ctx, cancel := context.WithTimeout(context.Background(), cancelledNotifyTimeout)
	defer cancel()
	_ = t.rpc.Notify(ctx, "notifications/cancelled", map[string]any{
		"requestId": id,
		"reason":    cause.Error(),
	})
}

// decodeRawResult decodes a JSON-RPC result into resp.Result (left nil for an
// absent or null result).
func decodeRawResult(result json.RawMessage, resp *MCPResponse) error {
	if len(result) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(result, &v); err != nil {
		return err
	}
	resp.Result = v
	return nil
}

// receivesNotifications reports whether inbound server messages reach the
// client: only over a peer, whose handlers dispatch them.
func (t *stdioTransport) receivesNotifications() bool {
	return t.peer != nil
}

func (t *stdioTransport) notify(ctx context.Context, method string, params any) error {
	return t.rpc.Notify(ctx, method, params)
}

func (t *stdioTransport) Close() error {
	return t.rpc.Close()
}

// batchRoundTrip implements batchTransport by sending every request as a
// single JSON-RPC batch. Results are returned in the same order as reqs. Over
// a peer the batch carries the client's own request ids; otherwise
// jsonrpc.Client.CallBatch assigns them. Only used for protocol revisions
// that allow batching (see Client.allowsBatching).
func (t *stdioTransport) batchRoundTrip(ctx context.Context, reqs []*MCPRequest) ([]*MCPResponse, error) {
	if t.peer != nil {
		return t.peerBatchRoundTrip(ctx, reqs)
	}
	calls := make([]jsonrpc.BatchCall, len(reqs))
	raws := make([]json.RawMessage, len(reqs))
	for i, req := range reqs {
		calls[i] = jsonrpc.BatchCall{Method: req.Method, Params: req.Params, Out: &raws[i]}
	}

	batchResults := t.rpc.CallBatch(ctx, calls)

	resps := make([]*MCPResponse, len(reqs))
	for i, req := range reqs {
		resp := &MCPResponse{JSONRPC: "2.0", ID: req.ID}
		if err := batchResults[i].Err; err != nil {
			var rpcErr *jsonrpc.Error
			if errors.As(err, &rpcErr) {
				resp.Error = &MCPError{Code: rpcErr.Code, Message: rpcErr.Message, Data: rpcErr.Data}
			} else {
				return nil, err
			}
		} else if err := decodeRawResult(raws[i], resp); err != nil {
			return nil, err
		}
		resps[i] = resp
	}
	return resps, nil
}

func (t *stdioTransport) peerBatchRoundTrip(ctx context.Context, reqs []*MCPRequest) ([]*MCPResponse, error) {
	wire := make([]*jsonrpc.Request, len(reqs))
	for i, req := range reqs {
		id, err := json.Marshal(req.ID)
		if err != nil {
			return nil, err
		}
		params, err := json.Marshal(req.Params)
		if err != nil {
			return nil, err
		}
		wire[i] = &jsonrpc.Request{JSONRPC: json.RawMessage(`"2.0"`), Method: req.Method, Params: params, ID: id}
	}
	got, err := t.peer.CallBatch(ctx, wire)
	if err != nil {
		if ctx.Err() != nil {
			for _, req := range reqs {
				t.sendCancelled(req.ID, ctx.Err())
			}
		}
		return nil, err
	}
	resps := make([]*MCPResponse, len(reqs))
	for i, req := range reqs {
		resp := &MCPResponse{JSONRPC: "2.0", ID: req.ID}
		if got[i].Error != nil {
			resp.Error = &MCPError{Code: got[i].Error.Code, Message: got[i].Error.Message, Data: got[i].Error.Data}
		} else if err := decodeRawResult(got[i].Result, resp); err != nil {
			return nil, err
		}
		resps[i] = resp
	}
	return resps, nil
}

// StdioClientOption configures a subprocess-backed stdio client.
type StdioClientOption func(*stdioClientConfig)

type stdioClientConfig struct {
	stderr   io.Writer
	env      []string
	extraEnv []string
	dir      string
	onExit   func(error)
	// probeTimeout bounds the Modern server/discover probe (see
	// WithClientProbeTimeout); 0 means defaultStdioProbeTimeout.
	probeTimeout time.Duration
}

// WithClientProbeTimeout bounds how long Initialize waits for the server's
// answer to the Modern-era server/discover probe before falling back to the
// Legacy initialize handshake (default 5s). A Legacy stdio server may ignore
// a request it receives before initialize instead of answering it with an
// error; without the bound the probe would wait for it forever.
func WithClientProbeTimeout(d time.Duration) StdioClientOption {
	return func(c *stdioClientConfig) { c.probeTimeout = d }
}

// WithClientStderr routes the child server's standard error to w (default:
// inherited from the parent). Pass io.Discard to silence it.
func WithClientStderr(w io.Writer) StdioClientOption {
	return func(c *stdioClientConfig) { c.stderr = w }
}

// WithClientEnv sets the environment for the child server process. It replaces
// the entire environment: every variable the child sees must be listed,
// including PATH, HOME, etc. For the more common case of adding or overriding a
// few variables while keeping the parent environment, use [WithClientExtraEnv]
// instead.
func WithClientEnv(env []string) StdioClientOption {
	return func(c *stdioClientConfig) { c.env = env }
}

// WithClientExtraEnv adds or overrides the given KEY=VALUE environment variables
// in the child server process's environment, on top of the environment it would
// otherwise inherit (the parent's, or the full replacement set by
// [WithClientEnv]). Entries use os.Environ's "KEY=VALUE" form. Multiple options
// accumulate; for a repeated key the last value wins.
//
// Unlike [WithClientEnv] (which replaces the whole environment), this keeps the
// parent environment intact so PATH, HOME, etc. remain available to the server.
func WithClientExtraEnv(kv ...string) StdioClientOption {
	return func(c *stdioClientConfig) { c.extraEnv = append(c.extraEnv, kv...) }
}

// WithClientDir sets the working directory for the child server process.
func WithClientDir(dir string) StdioClientOption {
	return func(c *stdioClientConfig) { c.dir = dir }
}

// WithClientOnExit registers a callback invoked exactly once when the spawned
// server process exits — whether it crashes mid-session or is shut down by
// [Client.Close]. It is invoked asynchronously from a reaper goroutine, so it
// fires even if Close is never called; it must not block.
//
// Without this, a caller has no way to learn that its child server has died
// other than subsequent calls failing. This is passed through to
// [jsonrpc.WithOnExit].
func WithClientOnExit(fn func(error)) StdioClientOption {
	return func(c *stdioClientConfig) { c.onExit = fn }
}

// NewStdioClient launches command (with args) as an MCP server speaking
// newline-delimited JSON-RPC over its stdin/stdout, and returns a client
// connected to it. Call Close to shut the child process down.
//
// The namespace behaves as for NewClient: when non-empty it is prefixed to tool
// names (e.g. namespace "fs" exposes tool "read" as "fs__read").
func NewStdioClient(command string, args []string, namespace string, opts ...StdioClientOption) (*Client, error) {
	cfg := stdioClientConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	procOpts := []jsonrpc.ProcessOption{}
	if cfg.stderr != nil {
		procOpts = append(procOpts, jsonrpc.WithStderr(cfg.stderr))
	}
	if cfg.env != nil {
		procOpts = append(procOpts, jsonrpc.WithEnv(cfg.env))
	}
	if len(cfg.extraEnv) > 0 {
		procOpts = append(procOpts, jsonrpc.WithExtraEnv(cfg.extraEnv...))
	}
	if cfg.dir != "" {
		procOpts = append(procOpts, jsonrpc.WithDir(cfg.dir))
	}
	if cfg.onExit != nil {
		procOpts = append(procOpts, jsonrpc.WithOnExit(cfg.onExit))
	}

	// Use a bidirectional Peer so inbound listChanged notifications from the
	// server are delivered to handlers (and on to the client's cache). NewProcessPeer
	// starts its read loop immediately.
	peer, err := jsonrpc.NewProcessPeer(command, args, jsonrpc.NewServer(), procOpts...)
	if err != nil {
		return nil, err
	}
	c := newPeerClient(peer, namespace)
	c.probeTimeout = cfg.probeTimeout
	return c, nil
}

// NewStreamClient returns an MCP client that speaks newline-delimited JSON-RPC
// over the given streams: it writes requests to out and reads responses from
// in. Use it to connect to a server exposed via Server.ServeStream (for example
// over an in-process pipe), or any transport you manage yourself. Close stops
// the client's reader.
func NewStreamClient(in io.Reader, out io.Writer, namespace string) *Client {
	peer := jsonrpc.NewPeer(in, out, jsonrpc.NewServer())
	c := newPeerClient(peer, namespace)
	go func() { _ = peer.Serve() }() // NewPeer does not start the reader itself
	return c
}

// newPeerClient wires a bidirectional jsonrpc.Peer into a Client: outbound calls
// go through the Peer (with the client's own request ids), and inbound
// notifications and server requests are dispatched by the Peer's handlers.
func newPeerClient(peer *jsonrpc.Peer, namespace string) *Client {
	separator := DefaultNamespaceSeparator
	namespace = normalizeNamespace(namespace)
	c := &Client{
		namespace: namespace,
		separator: separator,
		transport: &stdioTransport{rpc: peer.Client(), peer: peer},
	}
	registerPeerNotificationHandlers(peer, c)
	// A server may ping the client at any time, and the receiver MUST answer
	// promptly with an empty result (2025-11-25 §Ping).
	peer.Handle("ping", func(ctx context.Context, params json.RawMessage) (any, error) {
		return map[string]any{}, nil
	})
	return c
}

// registerPeerNotificationHandlers routes the three listChanged notifications
// from the remote server to the client's shared handler (which invalidates
// caches, fires user callbacks, and runs the propagation hook).
func registerPeerNotificationHandlers(peer *jsonrpc.Peer, c *Client) {
	mk := func(method string) jsonrpc.Handler {
		return func(ctx context.Context, params json.RawMessage) (any, error) {
			c.handleNotification(method, nil)
			return nil, nil
		}
	}
	for _, method := range []string{
		NotificationToolsChanged, NotificationResourcesChanged, NotificationPromptsChanged,
		// Servers built with pre-v0.34 releases of this library.
		legacyNotificationToolsChanged, legacyNotificationResourcesChanged, legacyNotificationPromptsChanged,
	} {
		peer.Handle(method, mk(method))
	}
}
