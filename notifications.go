package mcp

import "sync"

// MCP notification method names for list-change signalling, as every
// protocol revision (2024-11-05 through 2026-07-28) spells them.
const (
	NotificationToolsChanged     = "notifications/tools/list_changed"
	NotificationResourcesChanged = "notifications/resources/list_changed"
	NotificationPromptsChanged   = "notifications/prompts/list_changed"
)

// Pre-v0.34 releases of this library emitted (and listened for) a camelCase
// spelling the spec never used. The Client still accepts it so a newer
// client keeps working against a server built with an older release.
const (
	legacyNotificationToolsChanged     = "notifications/tools/listChanged"
	legacyNotificationResourcesChanged = "notifications/resources/listChanged"
	legacyNotificationPromptsChanged   = "notifications/prompts/listChanged"
)

// canonicalNotificationMethod maps the pre-v0.34 camelCase spelling of a
// list-changed notification to the spec's name; anything else passes through.
func canonicalNotificationMethod(method string) string {
	switch method {
	case legacyNotificationToolsChanged:
		return NotificationToolsChanged
	case legacyNotificationResourcesChanged:
		return NotificationResourcesChanged
	case legacyNotificationPromptsChanged:
		return NotificationPromptsChanged
	}
	return method
}

// notificationSink delivers one outbound notification to a single connected
// client. Implementations are transport-specific (HTTP SSE, stdio peer).
//
// send MUST be non-blocking: a slow or full sink drops the notification, since a
// listChanged notification is an idempotent "your cached list is stale, re-fetch"
// hint — a dropped one is recovered by the next one or by the client's next
// list call.
type notificationSink interface {
	send(method string, params any)
}

type notificationSubscriber struct {
	id   uint64
	sink notificationSink
}

// notificationHub fans outbound notifications out to every registered sink. The
// Server owns one; HTTP SSE GET requests and stdio peers register sinks into it.
type notificationHub struct {
	mu          sync.RWMutex
	nextID      uint64
	subscribers map[uint64]*notificationSubscriber
}

func newNotificationHub() *notificationHub {
	return &notificationHub{subscribers: make(map[uint64]*notificationSubscriber)}
}

func (h *notificationHub) subscribe(sink notificationSink) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	h.subscribers[id] = &notificationSubscriber{id: id, sink: sink}
	return id
}

func (h *notificationHub) unsubscribe(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subscribers, id)
}

func (h *notificationHub) broadcast(method string, params any) {
	h.mu.RLock()
	subs := make([]*notificationSubscriber, 0, len(h.subscribers))
	for _, s := range h.subscribers {
		subs = append(subs, s)
	}
	h.mu.RUnlock()
	for _, s := range subs {
		s.sink.send(method, params)
	}
}

func (h *notificationHub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subscribers)
}

// emitNotification broadcasts a notification to every connected client. It is a
// no-op when there are no subscribers, so it is cheap to call during server
// setup (before serving) and on every register/unregister.
func (s *Server) emitNotification(method string, params any) {
	if s.notifications.count() == 0 {
		return
	}
	s.notifications.broadcast(method, params)
}

// NotifyToolsChanged emits notifications/tools/listChanged to every connected
// client, signalling that its cached tool list is stale and should be re-fetched.
//
// RegisterTool, RegisterTools and UnregisterTool emit this automatically. Call
// this method for changes the server cannot detect itself — for example when a
// ToolProvider's backing data changes, or when tools are loaded from an external
// source that mutates at runtime.
func (s *Server) NotifyToolsChanged() {
	s.emitNotification(NotificationToolsChanged, nil)
}

// NotifyResourcesChanged emits notifications/resources/listChanged. Register*,
// UnregisterResource and RegisterResourceTemplate emit automatically; call this
// for external/resource-provider changes.
func (s *Server) NotifyResourcesChanged() {
	s.emitNotification(NotificationResourcesChanged, nil)
}

// NotifyPromptsChanged emits notifications/prompts/listChanged. RegisterPrompt
// and UnregisterPrompt emit automatically; call this for external/prompt-provider
// changes.
func (s *Server) NotifyPromptsChanged() {
	s.emitNotification(NotificationPromptsChanged, nil)
}
