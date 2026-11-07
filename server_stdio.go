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
	// its client has completed a Legacy initialize (which advertises
	// listChanged): a Modern (2026-07-28) client gets no unsolicited
	// notifications at all — it opens subscriptions/listen.
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
	limit := int(s.maxRequestBytes())
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > limit {
			stream.write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid Request","data":{"details":"message too large"}}}`))
			line = nil
		}
		if msg := bytes.TrimSpace(line); len(msg) > 0 {
			s.serveStdioMessage(ctx, srv, stream, sink, &wg, msg)
		}
		if err != nil {
			stream.closeStream() // subscriptions end; requests still get answered
			wg.Wait()            // let in-flight handlers finish
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
	version  string // Legacy revision negotiated by initialize, "" before
}

type stdioInflight struct {
	cancel    context.CancelFunc
	cancelled bool
	listen    bool // a subscriptions/listen: ends with the stream, silently
}

func (st *stdioStream) track(key string, cancel context.CancelFunc, listen bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.inflight[key] = &stdioInflight{cancel: cancel, listen: listen}
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

// closeStream is called when stdin ends: open subscriptions are torn down
// without a response (their client is gone), while ordinary requests
// already read are left to finish and be answered — closing stdin is a
// shutdown signal, and every request still gets its JSON-RPC response.
func (st *stdioStream) closeStream() {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, e := range st.inflight {
		if e.listen {
			e.cancelled = true
			e.cancel()
		}
	}
}

// setVersion records the Legacy revision an initialize negotiated.
func (st *stdioStream) setVersion(v string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.version = v
}

func (st *stdioStream) negotiatedVersion() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.version
}

// stdioHead is the part of an inbound line the read loop itself needs.
type stdioHead struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
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
	writeInvalid := func(id any, details string) {
		line, _ := json.Marshal(MCPResponse{JSONRPC: "2.0", ID: id, Error: &MCPError{Code: ErrorCodeInvalidRequest, Message: "Invalid Request", Data: map[string]any{"details": details}}})
		stream.write(line)
	}

	// The base protocol's rules, as the HTTP transport enforces them: a
	// message is a JSON-RPC 2.0 object; an id is a string or an integer,
	// never null; a client's own JSON-RPC response (an id and no method)
	// is never answered.
	if msg[0] == '{' && json.Valid(msg) {
		if head.JSONRPC != "2.0" {
			writeInvalid(nil, "JSONRPC field must be '2.0'")
			return
		}
		if len(head.ID) > 0 {
			if _, ok := decodeJSONRPCID(head.ID); !ok {
				writeInvalid(nil, "id must be a string or an integer, and must not be null")
				return
			}
			if head.Method == "" {
				return
			}
		}
	}

	// A batch (JSON array) is only for a 2025-03-26 client; later revisions
	// removed it. Each element is validated like a single message, and a
	// Modern (2026-07-28) element is refused on its own, while its Legacy
	// siblings are still served.
	if msg[0] == '[' {
		if v := stream.negotiatedVersion(); v != "" && !batchProtocolVersions[v] {
			writeInvalid(nil, "JSON-RPC batching is not supported in protocol version "+v)
			return
		}
		valid, rejected := splitStdioBatch(msg)
		if len(rejected) > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				responses := rejected
				if len(valid) > 0 {
					body, _ := json.Marshal(valid)
					if resp, has := srv.HandleMessage(ctx, body); has {
						var got []json.RawMessage
						if json.Unmarshal(resp, &got) == nil {
							responses = append(responses, got...)
						} else {
							responses = append(responses, resp)
						}
					}
				}
				line, _ := json.Marshal(responses)
				stream.write(line)
			}()
			return
		}
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
		stream.track(key, cancel, head.Method == "subscriptions/listen")
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
			if head.Method == "initialize" || msg[0] == '[' {
				// Gate before the response goes out, so a notification
				// emitted right after the client reads it is delivered.
				if version, ok := negotiatedLegacyInitialize(resp); ok {
					stream.setVersion(version)
					sink.setEnabled(true)
				}
			}
			stream.write(resp)
		}
	}()
}

// negotiatedLegacyInitialize finds an initialize result in a response (or a
// batch of responses) and reports the negotiated revision; every supported
// Legacy revision advertises listChanged, so a found result also means
// list-changed notifications may be pushed to that client.
func negotiatedLegacyInitialize(resp []byte) (version string, ok bool) {
	type initResp struct {
		Result *struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    *struct {
				Tools map[string]any `json:"tools"`
			} `json:"capabilities"`
		} `json:"result"`
	}
	var many []initResp
	if bytes.HasPrefix(bytes.TrimSpace(resp), []byte("[")) {
		if json.Unmarshal(resp, &many) != nil {
			return "", false
		}
	} else {
		var one initResp
		if json.Unmarshal(resp, &one) != nil {
			return "", false
		}
		many = []initResp{one}
	}
	for _, r := range many {
		if r.Result != nil && r.Result.Capabilities != nil && r.Result.ProtocolVersion != "" {
			if v, _ := r.Result.Capabilities.Tools["listChanged"].(bool); v {
				return r.Result.ProtocolVersion, true
			}
		}
	}
	return "", false
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
			cancelled, _ := json.Marshal(MCPNotification{JSONRPC: "2.0", Method: "notifications/cancelled",
				Params: map[string]any{"requestId": req.id, "reason": "server shutting down"}})
			req.write(cancelled)
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
		return // no Legacy initialize yet, or a Modern client: no unsolicited pushes
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
		if cursorIn(params) {
			return nil, jsonrpc.NewError(errInvalidCursor.Code, errInvalidCursor.Message, nil)
		}
		return map[string]any{"tools": s.ListToolsWithContext(ctx)}, nil
	}))

	srv.Handle("tools/call", s.wrapStdioModern("tools/call", func(ctx context.Context, params json.RawMessage) (any, error) {
		return s.stdioToolsCall(ctx, params)
	}))

	srv.Handle("resources/list", s.wrapStdioModern("resources/list", func(ctx context.Context, params json.RawMessage) (any, error) {
		if cursorIn(params) {
			return nil, jsonrpc.NewError(errInvalidCursor.Code, errInvalidCursor.Message, nil)
		}
		return map[string]any{"resources": s.ListResources(ctx)}, nil
	}))

	srv.Handle("resources/templates/list", s.wrapStdioModern("resources/templates/list", func(ctx context.Context, params json.RawMessage) (any, error) {
		if cursorIn(params) {
			return nil, jsonrpc.NewError(errInvalidCursor.Code, errInvalidCursor.Message, nil)
		}
		return map[string]any{"resourceTemplates": s.ListResourceTemplates(ctx)}, nil
	}))

	srv.Handle("resources/read", s.wrapStdioModern("resources/read", func(ctx context.Context, params json.RawMessage) (any, error) {
		return s.stdioResourcesRead(ctx, params)
	}))

	srv.Handle("prompts/list", s.wrapStdioModern("prompts/list", func(ctx context.Context, params json.RawMessage) (any, error) {
		if cursorIn(params) {
			return nil, jsonrpc.NewError(errInvalidCursor.Code, errInvalidCursor.Message, nil)
		}
		return map[string]any{"prompts": s.ListPrompts(ctx)}, nil
	}))

	srv.Handle("skills/list", s.wrapStdioModern("skills/list", func(ctx context.Context, params json.RawMessage) (any, error) {
		if cursorIn(params) {
			return nil, jsonrpc.NewError(errInvalidCursor.Code, errInvalidCursor.Message, nil)
		}
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

// splitStdioBatch validates each element of a batch: well-formed Legacy
// elements are returned for dispatch, the rest get their own Invalid
// Request responses (a null or non-integer id, a wrong jsonrpc field, a
// non-object element, or a 2026-07-28 element, which cannot be batched).
func splitStdioBatch(msg []byte) (valid []json.RawMessage, rejected []json.RawMessage) {
	var elems []json.RawMessage
	if json.Unmarshal(msg, &elems) != nil {
		return nil, nil
	}
	invalid := func(id any, details string) json.RawMessage {
		line, _ := json.Marshal(MCPResponse{JSONRPC: "2.0", ID: id, Error: &MCPError{Code: ErrorCodeInvalidRequest, Message: "Invalid Request", Data: map[string]any{"details": details}}})
		return line
	}
	for _, el := range elems {
		el = bytes.TrimSpace(el)
		var req MCPRequest
		if perr := decodeMCPRequest(el, &req); perr != nil {
			rejected = append(rejected, invalid(req.ID, perr.Data.(map[string]any)["details"].(string)))
			continue
		}
		if isModernRequest(nil, &req) {
			rejected = append(rejected, invalid(req.ID, "a 2026-07-28 request cannot be sent in a batch"))
			continue
		}
		valid = append(valid, el)
	}
	return valid, rejected
}
