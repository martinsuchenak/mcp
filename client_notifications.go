package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// OnToolsChanged registers a callback fired whenever a
// notifications/tools/list_changed arrives from the server (HTTP SSE or stdio).
// The client also invalidates its own tool cache automatically; use this to
// trigger application-level refreshes (e.g. re-pulling for a UI). Returns the
// client for chaining.
func (c *Client) OnToolsChanged(fn func()) *Client {
	c.mu.Lock()
	c.onToolsChanged = fn
	c.mu.Unlock()
	return c
}

// OnResourcesChanged registers a callback for notifications/resources/list_changed.
func (c *Client) OnResourcesChanged(fn func()) *Client {
	c.mu.Lock()
	c.onResourcesChanged = fn
	c.mu.Unlock()
	return c
}

// OnPromptsChanged registers a callback for notifications/prompts/list_changed.
func (c *Client) OnPromptsChanged(fn func()) *Client {
	c.mu.Lock()
	c.onPromptsChanged = fn
	c.mu.Unlock()
	return c
}

// EnableNotifications opts the HTTP client into receiving server-pushed
// notifications over a long-lived SSE stream. When enabled, the client opens a
// background GET event-stream connection after Initialize, invalidates its tool
// cache on notifications/tools/list_changed, and fires any On*Changed callbacks.
//
// Notifications are off by default: a long-lived connection is only wanted when
// the caller intends to consume change events, and it must be released with
// [Client.Close]. stdio clients need no opt-in, because their transport is
// already a persistent connection; against a Modern server they subscribe
// with subscriptions/listen automatically (see startStreamSubscription).
//
// Returns the client for chaining. Safe to call before or after Initialize.
func (c *Client) EnableNotifications() *Client {
	c.readerMu.Lock()
	c.wantNotifications = true
	c.readerMu.Unlock()
	// If already initialized over HTTP, start the reader now; otherwise Initialize
	// will start it when it completes.
	c.mu.RLock()
	ready := c.initialized && c.transport == nil
	era := c.era
	c.mu.RUnlock()
	if ready {
		c.startNotifications(era)
	}
	return c
}

// startNotifications launches the background notification reader for an HTTP
// client, once — the Legacy GET SSE stream, or, for a Modern-era server,
// subscriptions/listen (see client_modern.go). era is passed in rather than
// read from c.era here because Initialize calls this while already holding
// c.mu (a write lock), and EnableNotifications calls it without holding c.mu
// at all; either caller reads era itself under whatever lock is valid for it.
// It is a no-op for stream transports (stdio), which receive notifications
// via their peer handlers and call handleNotification directly regardless of
// era.
func (c *Client) startNotifications(era clientEra) {
	if c.transport != nil {
		return
	}
	c.readerMu.Lock()
	defer c.readerMu.Unlock()
	if c.readerStarted {
		return
	}
	c.readerStarted = true
	c.ctx, c.cancel = context.WithCancel(context.Background())
	ctx := c.ctx
	c.readerWG.Add(1)
	go func() {
		defer c.readerWG.Done()
		if era == eraModern {
			c.runEventStreamReader(ctx, "subscriptions/listen", false, c.connectModernSubscription)
		} else {
			c.runEventStreamReader(ctx, "event stream", true, c.connectLegacySSE)
		}
	}()
}

// startStreamSubscription opens a Modern-era subscriptions/listen request
// on a stream transport (stdio), once, and keeps it open until Close. The
// request's own id is the subscription id; the server's acknowledgement and
// the list-changed notifications it then sends (carrying that id in
// _meta io.modelcontextprotocol/subscriptionId) arrive as ordinary inbound
// notifications, which the peer's handlers route to handleNotification
// exactly as the HTTP subscription reader does. Close cancels the request,
// which sends notifications/cancelled for it and so ends the subscription.
//
// A server that does not implement subscriptions/listen on stdio (Method not
// found, or any other error) simply leaves the client without change
// notifications; a subscription the server completes (e.g. on shutdown) is
// reopened after a short delay. A transport that cannot receive inbound
// messages (no peer) never subscribes. Safe under c.mu: only readerMu is
// taken.
func (c *Client) startStreamSubscription() {
	st, ok := c.transport.(*stdioTransport)
	if !ok || !st.receivesNotifications() {
		return
	}
	c.readerMu.Lock()
	defer c.readerMu.Unlock()
	if c.readerStarted {
		return
	}
	c.readerStarted = true
	c.ctx, c.cancel = context.WithCancel(context.Background())
	ctx := c.ctx

	// The server acknowledges a subscription before sending anything on
	// it; waiting (briefly) for that acknowledgement means a change made
	// right after Initialize returns is not emitted to a subscription that
	// does not exist yet.
	acked := make(chan struct{})
	var ackOnce sync.Once
	if st.peer != nil {
		st.peer.Handle("notifications/subscriptions/acknowledged", func(ctx context.Context, params json.RawMessage) (any, error) {
			ackOnce.Do(func() { close(acked) })
			return nil, nil
		})
	}
	settled := make(chan struct{}) // closed when the first attempt is answered or acknowledged
	var settleOnce sync.Once
	settle := func() { settleOnce.Do(func() { close(settled) }) }

	c.readerWG.Add(1)
	go func() {
		defer c.readerWG.Done()
		defer settle()
		for ctx.Err() == nil {
			req := c.withModernMeta(c.listenRequest())
			var resp MCPResponse
			err := st.roundTrip(ctx, req, &resp, nil)
			if ctx.Err() != nil {
				return
			}
			if err != nil || resp.Error != nil {
				if resp.Error != nil && resp.Error.Code != ErrorCodeMethodNotFound {
					slog.Warn("mcp: subscriptions/listen failed; change notifications unavailable", "code", resp.Error.Code, "message", resp.Error.Message)
				}
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	select {
	case <-acked:
	case <-settled:
	case <-time.After(2 * time.Second):
	}
}

// eventStreamConnect performs one event-stream connection attempt.
// lastEventID is the id of the last event received on an earlier connection
// ("" if none), for a Legacy reconnect's Last-Event-ID header. It returns the
// response plus translate, which maps each inbound notification's method name
// to the one handleNotification expects; ok=false from translate drops the
// message.
type eventStreamConnect func(ctx context.Context, lastEventID string) (*http.Response, func(string) (string, bool), error)

// errStreamNotOffered is returned by readEventStream when the server answers
// the connection with 405 Method Not Allowed: it does not offer the stream at
// all (2025-11-25 §Listening for Messages from the Server), so retrying is
// pointless.
var errStreamNotOffered = errors.New("server does not offer an event stream (405)")

// eventStreamState is what one connection leaves behind for the next: the
// SSE id of the last event received (sent back as Last-Event-ID) and the
// server's last retry field.
type eventStreamState struct {
	lastEventID string
	retry       time.Duration
}

// runEventStreamReader maintains a long-lived event-stream connection until
// the context is cancelled. After a connection ends it waits the larger of
// the server's SSE retry field (which the client MUST respect, 2025-11-25
// §Resumability and Redelivery) and an exponential backoff (1s initial,
// doubling to a 30s cap after failed connection attempts, reset after a
// successful one), then reconnects. A Legacy reconnect carries Last-Event-ID
// so the server can replay what was missed. A 405 ends the reader for good:
// the server does not offer the stream. label names the connection in log
// messages; legacy selects Legacy-only behaviour (resumption, answering
// server requests).
func (c *Client) runEventStreamReader(ctx context.Context, label string, legacy bool, connect eventStreamConnect) {
	backoff := time.Second
	var state eventStreamState
	for {
		if ctx.Err() != nil {
			return
		}
		connected, err := c.readEventStream(ctx, legacy, connect, &state)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errStreamNotOffered) {
			slog.Info("mcp: server does not offer a notification stream; not retrying", "server", c.baseURL, "stream", label)
			return
		}
		if connected {
			backoff = time.Second
		}
		delay := backoff
		if state.retry > delay {
			delay = state.retry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if !connected {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// readEventStream opens one event-stream connection via connect and blocks
// reading it until the stream ends or the context is cancelled. connected
// reports whether the server accepted the connection (200), so the caller
// can reset its backoff. state carries the resumption cursor and retry delay
// across connections.
func (c *Client) readEventStream(ctx context.Context, legacy bool, connect eventStreamConnect, state *eventStreamState) (connected bool, err error) {
	lastEventID := ""
	if legacy {
		lastEventID = state.lastEventID
	}
	resp, translate, err := connect(ctx, lastEventID)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed {
		return false, errStreamNotOffered
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("event stream returned status %d", resp.StatusCode)
	}

	reader := newSSEReader(resp.Body)
	reader.LastEventID, reader.idBuf, reader.Retry = state.lastEventID, state.lastEventID, state.retry
	defer func() {
		state.lastEventID, state.retry = reader.LastEventID, reader.Retry
	}()
	for {
		if ctx.Err() != nil {
			return true, nil
		}
		event, err := reader.next()
		if err != nil {
			return true, err
		}
		payload := bytes.TrimSpace(event.Data)
		if event.Event != "message" || len(payload) == 0 {
			continue
		}
		var msg streamEnvelope
		if json.Unmarshal(payload, &msg) != nil || msg.Method == "" {
			continue
		}
		if msg.hasID() {
			// A server-to-client request (e.g. ping). Legacy servers may send
			// these on the GET stream and the client must answer; the Modern
			// era forbids them, so they are ignored there.
			if legacy {
				c.replyToServerRequest(ctx, msg.ID, msg.Method)
			}
			continue
		}
		if method, ok := translate(msg.Method); ok {
			c.handleNotification(method, msg.Params)
		}
	}
}

// applyAuthHeader sets the Authorization header from the client's auth
// provider. A failure is returned rather than swallowed: a connection must
// never be made unauthenticated just because building the header failed.
// The request paths treat the error as fatal; the notification reader
// treats it as a failed attempt and retries with backoff.
//
// c.auth is read without c.mu: it is immutable after construction, and
// sendRequest/sendModernHTTPRequest run with c.mu already held (Initialize
// calls through under its write lock), so taking it here would deadlock.
func (c *Client) applyAuthHeader(h http.Header) error {
	if c.auth == nil {
		return nil
	}
	header, err := c.auth.GetAuthHeader()
	if err != nil {
		return err
	}
	if header != "" {
		h.Set("Authorization", header)
	}
	return nil
}

// connectLegacySSE builds and sends one Legacy-era GET event-stream request,
// carrying Last-Event-ID when resuming after an earlier connection.
func (c *Client) connectLegacySSE(ctx context.Context, lastEventID string) (*http.Response, func(string) (string, bool), error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return nil, nil, err
	}
	c.applyRequestHeaders(req.Header)
	req.Header.Set("Accept", "text/event-stream")

	c.mu.RLock()
	sessionID := c.getSessionID()
	protocolVersion := c.protocolVersion
	c.mu.RUnlock()
	if protocolVersion == "" {
		protocolVersion = MCPProtocolVersionLatest
	}
	req.Header.Set(headerProtocolVersion, protocolVersion)
	if sessionID != "" {
		req.Header.Set(headerSessionID, sessionID)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	if err := c.applyAuthHeader(req.Header); err != nil {
		return nil, nil, err
	}

	resp, err := c.httpClient.Do(req)
	return resp, passthroughMethod, err
}

// passthroughMethod is the Legacy reader's identity translation.
func passthroughMethod(method string) (string, bool) { return method, true }

// handleNotification processes one inbound notification: invalidates the
// relevant cache, fires the user callback, then the internal propagation hook.
// Shared by the HTTP SSE reader and the stdio peer handlers.
func (c *Client) handleNotification(method string, params any) {
	switch canonicalNotificationMethod(method) {
	case NotificationToolsChanged:
		c.mu.Lock()
		c.cachedTools = nil
		cb := c.onToolsChanged
		c.mu.Unlock()
		if cb != nil {
			cb()
		}
	case NotificationResourcesChanged:
		c.mu.RLock()
		cb := c.onResourcesChanged
		c.mu.RUnlock()
		if cb != nil {
			cb()
		}
	case NotificationPromptsChanged:
		c.mu.RLock()
		cb := c.onPromptsChanged
		c.mu.RUnlock()
		if cb != nil {
			cb()
		}
	}

	c.mu.RLock()
	hooks := make([]func(string, any), len(c.onNotification))
	copy(hooks, c.onNotification)
	c.mu.RUnlock()
	for _, hook := range hooks {
		hook(method, params)
	}
}

// setPropagationHook appends an internal callback fired for every inbound
// notification (after cache handling). It is used by [Server.RegisterRemoteServer]
// to propagate upstream listChanged notifications downstream; appending (not
// replacing) keeps every registration's propagation alive when one client is
// registered on multiple servers. Package-private.
func (c *Client) setPropagationHook(fn func(method string, params any)) {
	c.mu.Lock()
	c.onNotification = append(c.onNotification, fn)
	c.mu.Unlock()
}
