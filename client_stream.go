package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Client-side handling of a POST request answered with an SSE stream
// (Content-Type: text/event-stream), which both protocol eras require
// clients to support (2025-11-25 §Sending Messages 5; 2026-07-28 §Sending
// Messages 6).
//
// The stream is parsed incrementally (sse.go) rather than buffered, so a
// long-running request completes the moment its response event arrives, and
// keep-alive comments are skipped as the SSE standard requires. On it:
//
//   - The JSON-RPC response whose id matches the request completes the call;
//     the stream is then closed. (An error response with a null id — a
//     server that could not read the request id — also completes it.)
//   - Notifications (notifications/progress, notifications/message, list
//     changes, ...) are delivered to the client's notification handlers,
//     asynchronously and in order: sendRequest can run with c.mu held, and
//     handleNotification takes c.mu.
//   - Legacy era only: a server-to-client JSON-RPC request (2025-11-25 lets
//     a server send these on the stream) is answered by POSTing a response —
//     an empty result for ping, Method not found for anything else, since
//     this client declares no sampling/elicitation/roots capability. The
//     Modern era forbids them on the stream; they are ignored there.
//   - Legacy era only: if the stream ends before the response and the server
//     has sent an event id, the client resumes it with an HTTP GET carrying
//     Last-Event-ID after the server's retry delay (2025-11-25 §Resumability
//     and Redelivery). 2026-07-28 has no resumability; the stream ending
//     early is an error.
//
// Cancelling ctx closes the stream, which in 2026-07-28 is the cancellation
// signal to the server.

const (
	// maxIdleStreamResumes bounds consecutive resumes that deliver no real
	// message (priming events, keep-alives and empty data do not count), so
	// a server that keeps closing the stream cannot loop us.
	maxIdleStreamResumes = 5
	// maxStreamResumes bounds resumes for one request regardless of
	// progress: a server that trickles a notification per reconnect still
	// cannot keep the client reconnecting forever.
	maxStreamResumes = 100
	// defaultStreamRetry is the resume delay when the server sent no retry.
	defaultStreamRetry = time.Second
)

// minStreamRetry floors the server's retry delay so "retry: 1" cannot turn
// into a reconnect storm. Waiting longer than asked still honours retry,
// which sets the minimum wait. A variable so tests can shorten it.
var minStreamRetry = 100 * time.Millisecond

// streamEnvelope is the union of the JSON-RPC message shapes a response
// stream can carry.
type streamEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params any             `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func (e *streamEnvelope) hasID() bool {
	return len(e.ID) > 0 && string(e.ID) != "null"
}

// readResponseStream reads the SSE response to req from body until the
// matching JSON-RPC response arrives, decoding it into resp. body belongs to
// the caller (it closes it too; closing twice is harmless); resumed bodies
// are closed here as soon as they are superseded.
func (c *Client) readResponseStream(ctx context.Context, body io.ReadCloser, req *MCPRequest, resp *MCPResponse) error {
	wantID := normalizeJSONRPCID(req.ID)
	notifier := &streamNotifier{c: c}
	rd := newSSEReader(body)
	current := body
	defer func() { current.Close() }()
	idle, total := 0, 0

	for {
		found, progressed, err := c.consumeResponseStream(ctx, rd, wantID, resp, notifier)
		if found || err != nil {
			return err
		}
		if progressed {
			idle = 0
		}

		// The stream ended without the response.
		if c.era == eraModern || rd.LastEventID == "" {
			return fmt.Errorf("event stream ended without a response to request %v", req.ID)
		}
		if idle >= maxIdleStreamResumes {
			return fmt.Errorf("event stream for request %v: gave up after %d resume attempts without progress", req.ID, idle)
		}
		if total >= maxStreamResumes {
			return fmt.Errorf("event stream for request %v: gave up after %d resume attempts", req.ID, total)
		}
		idle++
		total++

		delay := rd.Retry
		if delay <= 0 {
			delay = defaultStreamRetry
		}
		if delay < minStreamRetry {
			delay = minStreamRetry
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}

		next, err := c.resumeResponseStream(ctx, rd.LastEventID)
		if err != nil {
			return fmt.Errorf("resuming event stream for request %v: %w", req.ID, err)
		}
		current.Close()
		current = next

		// Carry the cursor over; the resumed stream will advance it.
		lastID, retry := rd.LastEventID, rd.Retry
		rd = newSSEReader(next)
		rd.LastEventID, rd.idBuf, rd.Retry = lastID, lastID, retry
	}
}

// consumeResponseStream reads events until the response to wantID arrives
// (found) or the stream ends (found=false, err=nil). progressed reports
// whether a real JSON-RPC message arrived — not just keep-alives, priming
// events or other empty events, which a stalled server can send forever.
func (c *Client) consumeResponseStream(ctx context.Context, rd *sseReader, wantID any, resp *MCPResponse, notifier *streamNotifier) (found, progressed bool, err error) {
	for {
		msg, err := rd.next()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, progressed, ctxErr
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return false, progressed, nil
			}
			return false, progressed, fmt.Errorf("failed to read event stream: %w", err)
		}
		if msg.Event != "message" || len(bytes.TrimSpace(msg.Data)) == 0 {
			continue // other event types, and priming events with empty data
		}
		progressed = true

		var env streamEnvelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			return false, progressed, fmt.Errorf("invalid JSON-RPC message in event stream: %w", err)
		}

		switch {
		case env.Method != "" && env.hasID():
			if c.era != eraModern {
				c.replyToServerRequest(ctx, env.ID, env.Method)
			}
		case env.Method != "":
			method := env.Method
			if c.era == eraModern {
				method = canonicalNotificationMethod(method)
			}
			notifier.push(method, env.Params)
		case env.hasID() && reflect.DeepEqual(normalizeJSONRPCRawID(env.ID), wantID),
			!env.hasID() && len(env.Error) > 0:
			if err := json.Unmarshal(msg.Data, resp); err != nil {
				return false, progressed, fmt.Errorf("failed to decode response: %w", err)
			}
			return true, progressed, nil
		}
		// Anything else (a response to some other id) is not ours: skip it.
	}
}

// normalizeJSONRPCID turns an id into its JSON-decoded form (numbers become
// float64) so it compares equal to an id decoded from the wire.
func normalizeJSONRPCID(id any) any {
	raw, err := json.Marshal(id)
	if err != nil {
		return id
	}
	return normalizeJSONRPCRawID(raw)
}

func normalizeJSONRPCRawID(raw json.RawMessage) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

// resumeResponseStream reconnects a Legacy response stream with an HTTP GET
// carrying Last-Event-ID.
func (c *Client) resumeResponseStream(ctx context.Context, lastEventID string) (io.ReadCloser, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return nil, err
	}
	c.applyLegacyHeaders(httpReq.Header, "")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Last-Event-ID", lastEventID)
	if err := c.applyAuthHeader(httpReq.Header); err != nil {
		return nil, fmt.Errorf("failed to get auth header: %w", err)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode != http.StatusOK || !strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream") {
		httpResp.Body.Close()
		return nil, fmt.Errorf("server returned status %d (%s)", httpResp.StatusCode, httpResp.Header.Get("Content-Type"))
	}
	return httpResp.Body, nil
}

// replyToServerRequest answers a Legacy server-to-client request received on
// a response stream by POSTing a JSON-RPC response (the server acknowledges
// it with 202). Best effort: a failure only means the server keeps waiting.
func (c *Client) replyToServerRequest(ctx context.Context, id json.RawMessage, method string) {
	reply := map[string]any{"jsonrpc": "2.0", "id": id}
	if method == "ping" {
		reply["result"] = map[string]any{}
	} else {
		reply["error"] = map[string]any{"code": ErrorCodeMethodNotFound, "message": "Method not found"}
	}
	body, err := json.Marshal(reply)
	if err != nil {
		return
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	c.applyLegacyHeaders(httpReq.Header, "")
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if c.applyAuthHeader(httpReq.Header) != nil {
		return
	}
	if httpResp, err := c.httpClient.Do(httpReq); err == nil {
		io.Copy(io.Discard, httpResp.Body)
		httpResp.Body.Close()
	}
}

// applyLegacyHeaders sets the headers every Legacy request after initialize
// carries: custom request headers, User-Agent, MCP-Session-Id, and the
// negotiated MCP-Protocol-Version (required from 2025-06-18). method is the
// JSON-RPC method being sent ("" for GET streams and replies); initialize
// itself carries neither the session id nor the version header.
//
// Fields are read without c.mu for the same reason as applyAuthHeader: the
// request paths run with c.mu already held.
func (c *Client) applyLegacyHeaders(h http.Header, method string) {
	c.applyRequestHeaders(h)
	h.Set("User-Agent", fmt.Sprintf("%s/%s", mcpClientName, mcpClientVersion))
	if method == "initialize" {
		return
	}
	if c.sessionID != "" {
		h.Set(headerSessionID, c.sessionID)
	}
	if c.protocolVersion != "" {
		h.Set(headerProtocolVersion, c.protocolVersion)
	}
}

// streamNotifier delivers a stream's notifications to handleNotification on
// a separate goroutine, in arrival order, so the stream reader never blocks
// on c.mu.
type streamNotifier struct {
	c       *Client
	mu      sync.Mutex
	queue   []streamNotification
	running bool
}

type streamNotification struct {
	method string
	params any
}

func (n *streamNotifier) push(method string, params any) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.queue = append(n.queue, streamNotification{method, params})
	if !n.running {
		n.running = true
		go n.run()
	}
}

func (n *streamNotifier) run() {
	for {
		n.mu.Lock()
		if len(n.queue) == 0 {
			n.running = false
			n.mu.Unlock()
			return
		}
		next := n.queue[0]
		n.queue = n.queue[1:]
		n.mu.Unlock()
		n.c.handleNotification(next.method, next.params)
	}
}
