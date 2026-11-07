package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/paularlott/jsonrpc"
)

// stdioModernKey marks a stdio handler context as serving a Modern-era
// (2026-07-28) request, so shared result/error shaping (toolsCallResult,
// resourcesReadWireError) can tell the eras apart without HTTP headers.
type stdioModernKey struct{}

func withStdioModern(ctx context.Context) context.Context {
	return context.WithValue(ctx, stdioModernKey{}, true)
}

// isStdioModern reports whether ctx belongs to a Modern-era stdio request.
func isStdioModern(ctx context.Context) bool {
	v, _ := ctx.Value(stdioModernKey{}).(bool)
	return v
}

// StdioOption configures a stdio MCP server run.
type StdioOption func(*stdioConfig)

type stdioConfig struct {
	showAll bool
}

// WithStdioShowAllTools makes the stdio server expose every tool (including
// discoverable ones) on tools/list, bypassing the discovery meta-tools. It is
// the stdio equivalent of the HTTP show-all header/query flag.
func WithStdioShowAllTools() StdioOption {
	return func(c *stdioConfig) { c.showAll = true }
}

// ServeStdio serves the MCP protocol over newline-delimited JSON-RPC 2.0 on
// os.Stdin/os.Stdout. It is the entry point for an MCP stdio server (the
// transport a host launches as a subprocess). It blocks until stdin reaches EOF.
//
// Anything written to stdout must be protocol frames only, so send logs to
// stderr.
func (s *Server) ServeStdio(ctx context.Context, opts ...StdioOption) error {
	return s.ServeStream(ctx, os.Stdin, os.Stdout, opts...)
}

// ServeStream serves the MCP protocol over an arbitrary pair of
// newline-delimited JSON-RPC 2.0 streams. Use it for in-process pipes or any
// transport that is not the process's own stdio; ServeStdio wraps it for the
// common case.
//
// The read loop runs here (in mcp) rather than in jsonrpc.Server.ServeStream so
// that outbound notifications can share the stream's write mutex, and so the
// caller's ctx (which carries the show-all flag) is threaded to handlers.
func (s *Server) ServeStream(ctx context.Context, in io.Reader, out io.Writer, opts ...StdioOption) error {
	cfg := stdioConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.showAll {
		ctx = WithShowAllTools(ctx)
	}

	srv := s.newStdioDispatcher()

	// A shared write mutex guards out so responses and notifications serialize.
	var writeMu sync.Mutex
	var wg sync.WaitGroup
	write := func(line []byte) {
		writeMu.Lock()
		_, _ = out.Write(append(line, '\n'))
		writeMu.Unlock()
	}

	// Legacy-era change notifications are pushed to this stream only once
	// its client has completed a Legacy initialize that advertised
	// listChanged (2025-03-26 and later): a client is only sent the
	// capabilities it negotiated, and a Modern (2026-07-28) client gets no
	// unsolicited notifications at all — it opens subscriptions/listen.
	// The sink is non-blocking: it queues notifications to a buffered channel
	// and a drainer writes them under writeMu, so emit (which can run under
	// s.mu from RegisterTool) never blocks on a slow/stuck client.
	sink := newStreamNotifySink(out, &writeMu)
	subID := s.notifications.subscribe(sink)
	defer s.notifications.unsubscribe(subID)
	defer sink.stop()

	stream := &stdioStream{write: write, inflight: map[string]*stdioInflight{}}

	// Close the reader on ctx cancellation so the read unblocks. Without
	// this, Ctrl+C cancels the context but the read loop is stuck and the
	// process hangs. os.Stdin (the common case) is an io.Closer.
	if closer, ok := in.(io.Closer); ok {
		go func() {
			<-ctx.Done()
			closer.Close()
		}()
	}

	// Newline framing, as the stdio transport defines it: one message per
	// line. A line that is not valid JSON is answered with a parse error
	// (-32700) and the stream carries on, rather than the whole server
	// stopping at the first malformed byte.
	reader := bufio.NewReader(in)
	for {
		line, err := reader.ReadBytes('\n')
		if msg := bytes.TrimSpace(line); len(msg) > 0 {
			s.serveStdioMessage(ctx, srv, stream, sink, &wg, msg)
		}
		if err != nil {
			stream.cancelAll() // nothing can be answered on a closed stream
			wg.Wait()          // let in-flight handlers finish
			if ctx.Err() != nil {
				return nil // context cancelled (reader closed) — clean shutdown
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// stdioStream is the per-connection state the read loop keeps: the requests
// still being handled (so notifications/cancelled can reach them) and the
// writer handlers use for messages of their own (subscriptions/listen).
type stdioStream struct {
	write    func(line []byte)
	mu       sync.Mutex
	inflight map[string]*stdioInflight
}

type stdioInflight struct {
	cancel    context.CancelFunc
	cancelled bool
}

func (st *stdioStream) track(key string, cancel context.CancelFunc) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.inflight[key] = &stdioInflight{cancel: cancel}
}

// untrack forgets a finished request and reports whether the client had
// cancelled it (in which case its response must not be sent).
func (st *stdioStream) untrack(key string) (cancelled bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if e := st.inflight[key]; e != nil {
		cancelled = e.cancelled
		e.cancel()
		delete(st.inflight, key)
	}
	return cancelled
}

func (st *stdioStream) cancel(key string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if e := st.inflight[key]; e != nil {
		e.cancelled = true
		e.cancel()
	}
}

func (st *stdioStream) cancelAll() {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, e := range st.inflight {
		e.cancel()
	}
}

// stdioHead is the part of an inbound line the read loop itself needs.
type stdioHead struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// stdioIDKey normalises a JSON-RPC id for inflight tracking.
func stdioIDKey(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// stdioRequestKey carries a request's id and the stream writer to handlers
// that must write messages before their result (subscriptions/listen).
type stdioRequestKey struct{}

type stdioRequest struct {
	id    any
	write func(line []byte)
}

// serveStdioMessage dispatches one inbound line. Requests run concurrently,
// each under a cancellable context that notifications/cancelled reaches via
// its id (the spec: a cancelled request's work stops and no response is sent).
func (s *Server) serveStdioMessage(ctx context.Context, srv *jsonrpc.Server, stream *stdioStream, sink *streamNotifySink, wg *sync.WaitGroup, msg []byte) {
	var head stdioHead
	if msg[0] == '{' {
		_ = json.Unmarshal(msg, &head)
	}

	if head.Method == "notifications/cancelled" && len(head.ID) == 0 {
		var params struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if json.Unmarshal(head.Params, &params) == nil && len(params.RequestID) > 0 {
			stream.cancel(stdioIDKey(params.RequestID))
		}
		return
	}

	reqCtx, cancel := context.WithCancel(ctx)
	key := ""
	if len(head.ID) > 0 && string(bytes.TrimSpace(head.ID)) != "null" && head.Method != "" {
		key = stdioIDKey(head.ID)
		stream.track(key, cancel)
		if id, ok := decodeJSONRPCID(head.ID); ok {
			reqCtx = context.WithValue(reqCtx, stdioRequestKey{}, &stdioRequest{id: id, write: stream.write})
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer cancel()
		resp, hasResp := srv.HandleMessage(reqCtx, msg)
		if key != "" && stream.untrack(key) {
			return // cancelled by the client: no response
		}
		if hasResp {
			if head.Method == "initialize" {
				// Gate before the response goes out, so a notification
				// emitted right after the client reads it is delivered.
				sink.setEnabled(legacyInitializeAdvertisesListChanged(resp))
			}
			stream.write(resp)
		}
	}()
}

// legacyInitializeAdvertisesListChanged reports whether an initialize
// response negotiated a revision whose capabilities carry listChanged, i.e.
// whether list-changed notifications may be pushed to that client.
func legacyInitializeAdvertisesListChanged(resp []byte) bool {
	var out struct {
		Result *struct {
			Capabilities struct {
				Tools map[string]any `json:"tools"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	if json.Unmarshal(resp, &out) != nil || out.Result == nil {
		return false
	}
	v, _ := out.Result.Capabilities.Tools["listChanged"].(bool)
	return v
}

// stdioSubscriptionsListen serves subscriptions/listen over stdio (Modern
// era): it acknowledges with the honoured subset of the filter, then writes
// each matching list-changed notification tagged with the subscription id
// (the request's own id), until the client cancels the request
// (notifications/cancelled, after which nothing more is sent) or the server
// shuts down, when the request is answered with a completion result — the
// spec's graceful closure. wrapStdioModern validates _meta and shapes that
// result like any other.
func (s *Server) stdioSubscriptionsListen(ctx context.Context, params json.RawMessage) (any, error) {
	req, _ := ctx.Value(stdioRequestKey{}).(*stdioRequest)
	if req == nil {
		return nil, jsonrpc.NewError(ErrorCodeInternalError, "subscriptions/listen needs a request id", nil)
	}
	wanted, acked, errDetails := parseListenFilter(params)
	if errDetails != "" {
		return nil, jsonrpc.NewError(ErrorCodeInvalidParams, "Invalid params", map[string]any{"details": errDetails})
	}

	sink := newModernSubscriptionSink(64, wanted)
	subID := s.notifications.subscribe(sink)
	defer s.notifications.unsubscribe(subID)

	meta := map[string]any{metaKeySubscriptionID: req.id}
	ack, _ := json.Marshal(MCPNotification{
		JSONRPC: "2.0",
		Method:  "notifications/subscriptions/acknowledged",
		Params:  map[string]any{"_meta": meta, "notifications": acked},
	})
	req.write(ack)

	for {
		select {
		case ev := <-sink.ch:
			line, err := json.Marshal(MCPNotification{JSONRPC: "2.0", Method: ev.method, Params: map[string]any{"_meta": meta}})
			if err == nil {
				req.write(line)
			}
		case <-ctx.Done():
			return nil, jsonrpc.NewError(ErrorCodeInternalError, "cancelled", nil) // never sent: the request was cancelled
		case <-s.shutdownCh:
			return map[string]any{"_meta": meta}, nil
		}
	}
}

// parseListenFilter reads a subscriptions/listen params object: notifications
// must be an object (every field optional, wrongly typed fields rejected);
// the result is the set of methods to forward and the acknowledgement's
// honoured subset (types this server does not support are omitted).
func parseListenFilter(params json.RawMessage) (wanted map[string]bool, acked map[string]any, errDetails string) {
	var shape struct {
		Notifications any `json:"notifications"`
	}
	if json.Unmarshal(params, &shape) != nil {
		return nil, nil, "params must be an object"
	}
	if _, ok := shape.Notifications.(map[string]any); !ok {
		return nil, nil, "params.notifications must be an object"
	}
	var filter struct {
		Notifications struct {
			ToolsListChanged     bool `json:"toolsListChanged"`
			PromptsListChanged   bool `json:"promptsListChanged"`
			ResourcesListChanged bool `json:"resourcesListChanged"`
		} `json:"notifications"`
	}
	if err := json.Unmarshal(params, &filter); err != nil {
		return nil, nil, err.Error()
	}
	wanted, acked = map[string]bool{}, map[string]any{}
	if filter.Notifications.ToolsListChanged {
		wanted[NotificationToolsChanged], acked["toolsListChanged"] = true, true
	}
	if filter.Notifications.PromptsListChanged {
		wanted[NotificationPromptsChanged], acked["promptsListChanged"] = true, true
	}
	if filter.Notifications.ResourcesListChanged {
		wanted[NotificationResourcesChanged], acked["resourcesListChanged"] = true, true
	}
	return wanted, acked, ""
}

// streamNotifySink is a notificationSink that writes JSON-RPC notifications to a
// stdio stream. send is non-blocking: it enqueues to a buffered channel and a
// drain goroutine does the actual write under the shared response mutex. This
// keeps a slow/stuck client from stalling notification emission (which can run
// under s.mu via RegisterTool). Events are dropped if the buffer fills.
type streamNotifySink struct {
	ch      chan sseEvent
	out     io.Writer
	mu      *sync.Mutex
	done    chan struct{}
	enabled atomic.Bool // set once a Legacy initialize negotiated listChanged
}

func (sn *streamNotifySink) setEnabled(on bool) { sn.enabled.Store(on) }

func newStreamNotifySink(out io.Writer, mu *sync.Mutex) *streamNotifySink {
	sn := &streamNotifySink{
		ch:   make(chan sseEvent, 64),
		out:  out,
		mu:   mu,
		done: make(chan struct{}),
	}
	go sn.drain()
	return sn
}

func (sn *streamNotifySink) send(method string, params any) {
	if !sn.enabled.Load() {
		return // not negotiated (2024-11-05) or a Modern client: no unsolicited pushes
	}
	select {
	case sn.ch <- sseEvent{method: method, params: params}:
	default: // buffer full: drop (listChanged is an idempotent re-fetch hint)
	}
}

func (sn *streamNotifySink) drain() {
	for {
		select {
		case ev := <-sn.ch:
			payload, err := json.Marshal(MCPNotification{JSONRPC: "2.0", Method: ev.method, Params: ev.params})
			if err != nil {
				continue
			}
			sn.mu.Lock()
			_, _ = sn.out.Write(append(payload, '\n'))
			sn.mu.Unlock()
		case <-sn.done:
			return
		}
	}
}

func (sn *streamNotifySink) stop() { close(sn.done) }

// newStdioDispatcher builds the jsonrpc.Server that frames and dispatches MCP
// methods over a stream. The MCP protocol is JSON-RPC 2.0, so the framing,
// batching and notification handling are provided by the jsonrpc package; the
// handlers here reuse the same dispatch logic (ListToolsWithContext, CallTool)
// as the HTTP transport. Unknown notifications (e.g. notifications/initialized)
// are ignored automatically by the jsonrpc server.
func (s *Server) newStdioDispatcher() *jsonrpc.Server {
	srv := jsonrpc.NewServer()

	srv.Handle("initialize", rejectStdioModern("initialize", func(ctx context.Context, params json.RawMessage) (any, error) {
		return s.stdioInitialize(params)
	}))

	srv.Handle("ping", rejectStdioModern("ping", func(ctx context.Context, params json.RawMessage) (any, error) {
		return map[string]any{}, nil
	}))

	srv.Handle("tools/list", s.wrapStdioModern("tools/list", func(ctx context.Context, params json.RawMessage) (any, error) {
		return map[string]any{"tools": s.ListToolsWithContext(ctx)}, nil
	}))

	srv.Handle("tools/call", s.wrapStdioModern("tools/call", func(ctx context.Context, params json.RawMessage) (any, error) {
		return s.stdioToolsCall(ctx, params)
	}))

	srv.Handle("resources/list", s.wrapStdioModern("resources/list", func(ctx context.Context, params json.RawMessage) (any, error) {
		return map[string]any{"resources": s.ListResources(ctx)}, nil
	}))

	srv.Handle("resources/templates/list", s.wrapStdioModern("resources/templates/list", func(ctx context.Context, params json.RawMessage) (any, error) {
		return map[string]any{"resourceTemplates": s.ListResourceTemplates(ctx)}, nil
	}))

	srv.Handle("resources/read", s.wrapStdioModern("resources/read", func(ctx context.Context, params json.RawMessage) (any, error) {
		return s.stdioResourcesRead(ctx, params)
	}))

	srv.Handle("prompts/list", s.wrapStdioModern("prompts/list", func(ctx context.Context, params json.RawMessage) (any, error) {
		return map[string]any{"prompts": s.ListPrompts(ctx)}, nil
	}))

	srv.Handle("skills/list", s.wrapStdioModern("skills/list", func(ctx context.Context, params json.RawMessage) (any, error) {
		return map[string]any{"skills": s.ListSkillsWithContext(ctx)}, nil
	}))
	srv.Handle("skills/get", s.wrapStdioModern("skills/get", func(ctx context.Context, params json.RawMessage) (any, error) {
		var p struct {
			URI string `json:"uri"`
		}
		if len(params) > 0 {
			if err := json.Unmarshal(params, &p); err != nil || p.URI == "" {
				return nil, jsonrpc.NewError(ErrorCodeInvalidParams, "uri parameter is required", nil)
			}
		}
		skill, ok := s.GetSkillWithContext(ctx, p.URI)
		if !ok {
			return nil, jsonrpc.NewError(ErrorCodeInvalidParams, fmt.Sprintf("Skill not found: %s", p.URI), map[string]any{"uri": p.URI})
		}
		return map[string]any{"skill": skill}, nil
	}))

	srv.Handle("prompts/get", s.wrapStdioModern("prompts/get", func(ctx context.Context, params json.RawMessage) (any, error) {
		return s.stdioPromptsGet(ctx, params)
	}))

	// server/discover is the Modern-era (protocol revision 2026-07-28+)
	// discovery RPC — see modern.go. Routed through wrapStdioModern like
	// every other Modern method, for the same reason HTTP's
	// handleModernRequest validates it identically to any other request:
	// the spec's _meta rules (protocolVersion/clientCapabilities required)
	// apply to every client request, discover included — a real Modern
	// client always sends full _meta even for its own discovery probe (see
	// client_modern.go's withModernMeta). buildDiscoverResult already sets
	// resultType/ttlMs/cacheScope/_meta.serverInfo itself, so
	// wrapStdioModern's post-success reshaping is a harmless no-op on top.
	srv.Handle("server/discover", s.wrapStdioModern("server/discover", func(ctx context.Context, params json.RawMessage) (any, error) {
		return s.buildDiscoverResult(ctx, GetShowAllTools(ctx)), nil
	}))

	// subscriptions/listen (Modern only): see stdioSubscriptionsListen. The
	// handler blocks for the life of the subscription; ServeStream runs
	// every message on its own goroutine, so that costs nothing else.
	srv.Handle("subscriptions/listen", s.wrapStdioModern("subscriptions/listen", s.stdioSubscriptionsListen))

	return srv
}

func (s *Server) stdioInitialize(raw json.RawMessage) (any, error) {
	var params initializeParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, jsonrpc.NewError(ErrorCodeInvalidParams, "Invalid params", nil)
		}
	}

	protocolVersion, ok := negotiateInitializeVersion(params.ProtocolVersion)
	if !ok {
		return nil, jsonrpc.NewError(ErrorCodeInvalidParams, "Unsupported protocol version", map[string]any{
			"requested": params.ProtocolVersion,
			"supported": supportedProtocolVersions,
		})
	}

	s.mu.Lock()
	instructions := s.instructions
	icons := s.icons
	s.lastClientCapabilities = params.Capabilities
	hasDiscoverable := s.hasDiscoverableTools
	s.mu.Unlock()

	// Match the HTTP initialize: when discoverable tools exist, tell the
	// client about tool_search/execute_tool. stdio has no show-all mode, so
	// the hint is unconditional on that.
	if hasDiscoverable {
		instructions = appendDiscoveryInstructions(instructions)
	}

	return initializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    s.buildCapabilities(protocolVersion),
		ServerInfo: serverInfo{
			Name:    s.name,
			Version: s.version,
			Icons:   icons,
		},
		Instructions: instructions,
	}, nil
}

func (s *Server) stdioToolsCall(ctx context.Context, raw json.RawMessage) (any, error) {
	var params ToolCallParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, jsonrpc.NewError(ErrorCodeInvalidParams, "Invalid params", nil)
		}
	}

	response, err := s.CallTool(ctx, params.Name, params.Arguments)
	result, wireErr := s.toolsCallResult(ctx, params.Name, response, err, !isStdioModern(ctx))
	if wireErr != nil {
		return nil, jsonrpc.NewError(wireErr.Code, wireErr.Message, wireErr.Data)
	}
	return result, nil
}

func (s *Server) stdioResourcesRead(ctx context.Context, raw json.RawMessage) (any, error) {
	var params resourceReadParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, jsonrpc.NewError(ErrorCodeInvalidParams, "Invalid params", nil)
		}
	}
	if params.URI == "" {
		return nil, jsonrpc.NewError(ErrorCodeInvalidParams, "uri parameter is required", nil)
	}

	resp, err := s.ReadResource(ctx, params.URI)
	if err != nil {
		e := resourcesReadWireError(params.URI, err, !isStdioModern(ctx))
		return nil, jsonrpc.NewError(e.Code, e.Message, e.Data)
	}
	return resp, nil
}

func (s *Server) stdioPromptsGet(ctx context.Context, raw json.RawMessage) (any, error) {
	var params promptGetParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, jsonrpc.NewError(ErrorCodeInvalidParams, "Invalid params", nil)
		}
	}
	if params.Name == "" {
		return nil, jsonrpc.NewError(ErrorCodeInvalidParams, "name parameter is required", nil)
	}

	resp, err := s.GetPrompt(ctx, params.Name, params.Arguments)
	if err != nil {
		e := promptsGetWireError(params.Name, err)
		return nil, jsonrpc.NewError(e.Code, e.Message, e.Data)
	}
	return resp, nil
}
