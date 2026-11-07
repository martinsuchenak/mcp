package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OnToolsChanged registers a callback fired whenever a
// notifications/tools/listChanged arrives from the server (HTTP SSE or stdio).
// The client also invalidates its own tool cache automatically; use this to
// trigger application-level refreshes (e.g. re-pulling for a UI). Returns the
// client for chaining.
func (c *Client) OnToolsChanged(fn func()) *Client {
	c.mu.Lock()
	c.onToolsChanged = fn
	c.mu.Unlock()
	return c
}

// OnResourcesChanged registers a callback for notifications/resources/listChanged.
func (c *Client) OnResourcesChanged(fn func()) *Client {
	c.mu.Lock()
	c.onResourcesChanged = fn
	c.mu.Unlock()
	return c
}

// OnPromptsChanged registers a callback for notifications/prompts/listChanged.
func (c *Client) OnPromptsChanged(fn func()) *Client {
	c.mu.Lock()
	c.onPromptsChanged = fn
	c.mu.Unlock()
	return c
}

// EnableNotifications opts the HTTP client into receiving server-pushed
// notifications over a long-lived SSE stream. When enabled, the client opens a
// background GET event-stream connection after Initialize, invalidates its tool
// cache on notifications/tools/listChanged, and fires any On*Changed callbacks.
//
// Notifications are off by default: a long-lived connection is only wanted when
// the caller intends to consume change events, and it must be released with
// [Client.Close]. stdio clients always receive notifications (no opt-in needed)
// because their transport is already a persistent connection.
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
			c.runEventStreamReader(ctx, "subscriptions/listen", c.connectModernSubscription)
		} else {
			c.runEventStreamReader(ctx, "event stream", c.connectLegacySSE)
		}
	}()
}

// runEventStreamReader maintains a long-lived event-stream connection,
// reconnecting with exponential backoff (1s initial, doubling to a 30s cap)
// after transient failures until the context is cancelled. label names the
// connection in error messages; connect performs one connection attempt and
// returns the response plus translate, which maps each inbound notification's
// method name to the constant handleNotification expects (identity for the
// Legacy reader; the Modern subscription reader reverses the snake_case
// renaming). ok=false from translate drops the message.
func (c *Client) runEventStreamReader(ctx context.Context, label string, connect func(ctx context.Context) (*http.Response, func(string) (string, bool), error)) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.readEventStream(ctx, label, connect)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = time.Second
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// readEventStream opens one event-stream connection via connect and blocks
// reading notifications until the stream ends or the context is cancelled.
func (c *Client) readEventStream(ctx context.Context, label string, connect func(ctx context.Context) (*http.Response, func(string) (string, bool), error)) error {
	resp, translate, err := connect(ctx)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned status %d", label, resp.StatusCode)
	}

	reader := newSSEReader(resp.Body)
	for {
		if ctx.Err() != nil {
			return nil
		}
		event, err := reader.next()
		if err != nil {
			return err
		}
		payload := bytes.TrimSpace(event.Data)
		if event.Event != "message" || len(payload) == 0 {
			continue
		}
		var msg struct {
			Method string `json:"method"`
			Params any    `json:"params"`
		}
		if json.Unmarshal(payload, &msg) != nil {
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

// connectLegacySSE builds and sends one Legacy-era GET event-stream request.
func (c *Client) connectLegacySSE(ctx context.Context) (*http.Response, func(string) (string, bool), error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return nil, nil, err
	}
	c.applyRequestHeaders(req.Header)
	req.Header.Set("Accept", "text/event-stream")

	c.mu.RLock()
	sessionID := c.sessionID
	protocolVersion := c.protocolVersion
	c.mu.RUnlock()
	if protocolVersion == "" {
		protocolVersion = MCPProtocolVersionLatest
	}
	req.Header.Set(headerProtocolVersion, protocolVersion)
	if sessionID != "" {
		req.Header.Set(headerSessionID, sessionID)
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
