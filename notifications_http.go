package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// sseEvent is one queued outbound notification for an SSE subscriber.
type sseEvent struct {
	method string
	params any
}

// sseSink is a notificationSink backed by a buffered channel. It is non-blocking:
// a full buffer drops the event (listChanged is an idempotent re-fetch hint).
type sseSink struct {
	ch        chan sseEvent
	done      chan struct{}
	closeOnce sync.Once
}

func newSSESink(buffer int) *sseSink {
	return &sseSink{ch: make(chan sseEvent, buffer), done: make(chan struct{})}
}

// close ends the stream: the session opened a newer one in its place.
func (s *sseSink) close() { s.closeOnce.Do(func() { close(s.done) }) }

func (s *sseSink) send(method string, params any) {
	select {
	case s.ch <- sseEvent{method: method, params: params}:
	default:
	}
}

// handleSSEStream serves a long-lived Server-Sent Events stream to a client.
// Notifications emitted by the server (via NotifyToolsChanged and friends, or
// automatically on register/unregister) are written to the stream as SSE
// `data:` events carrying a JSON-RPC notification. This is the server side of
// the Streamable HTTP transport's push channel.
func (s *Server) handleSSEStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// Validate protocol version (default per spec for non-initialize requests).
	if _, ok := negotiateProtocolVersion(r.Header.Get(headerProtocolVersion), "2025-03-26"); !ok {
		http.Error(w, "Unsupported MCP-Protocol-Version", http.StatusBadRequest)
		return
	}

	// When session management is enabled, require a valid session so a stream is
	// bound to an authenticated client. Without sessions, the stream is anonymous
	// (broadcast to all anonymous subscribers).
	// showAll is deliberately ignored here: the stream's notifications are
	// broadcast to every subscriber, not shaped per session.
	if _, ok, status, message := s.checkSession(r.Context(), r); !ok {
		http.Error(w, message, status)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // defeat nginx buffering
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sink := newSSESink(64)
	var subID uint64
	if sessionID := r.Header.Get(headerSessionID); sessionID != "" && s.getSessionManager() != nil {
		subID = s.notifications.subscribeForSession(sessionID, sink)
	} else {
		subID = s.notifications.subscribe(sink)
	}
	defer s.notifications.unsubscribe(subID)

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case ev := <-sink.ch:
			payload, err := json.Marshal(MCPNotification{
				JSONRPC: "2.0",
				Method:  ev.method,
				Params:  ev.params,
			})
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return // client disconnected
			}
			flusher.Flush()
		case <-heartbeat.C:
			if _, err := fmt.Fprintf(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-sink.done:
			return // superseded by a newer stream for this session
		case <-r.Context().Done():
			return
		}
	}
}
