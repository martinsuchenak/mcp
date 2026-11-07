# Notifications Guide (listChanged)

When a server's tool, resource, or prompt set changes, it can push a `list_changed` notification to connected clients rather than waiting for them to poll. The client invalidates its cache and re-fetches on the next call. This works over **both HTTP (SSE) and stdio**, and propagates through federated servers automatically.

A Legacy client (2025-03-26 through 2025-11-25) receives them unsolicited once it has completed `initialize`. A Modern client (2026-07-28) receives only what it asked for through `subscriptions/listen`, over HTTP and stdio alike.

The three notification methods:

| Method | Fired when |
| --- | --- |
| `notifications/tools/list_changed` | The tool set changes |
| `notifications/resources/list_changed` | The resource/template set changes |
| `notifications/prompts/list_changed` | The prompt set changes |

## Server: automatic emission

`RegisterTool`/`RegisterTools`/`UnregisterTool`, the resource `Register*`/`Unregister*`, and `RegisterPrompt`/`UnregisterPrompt` all emit the relevant notification **automatically** when there are connected clients. The capability is advertised with `listChanged: true`.

You don't need to do anything for changes the server can detect itself.

## Server: manual hook

For changes the server **cannot** detect — a `ToolProvider` whose backing data mutated, tools loaded from a database that changed externally, a periodic reload — call the manual hook:

```go
// After some external change to the data behind a provider:
server.NotifyToolsChanged()
server.NotifyResourcesChanged()
server.NotifyPromptsChanged()
```

These are no-ops (and cheap) when no clients are connected, so it is safe to call them eagerly. The emission is a broadcast: every connected client re-fetches and gets its own (e.g. per-user, provider-scoped) list.

## Client: receiving notifications (HTTP)

A client receives notifications over a long-lived SSE stream. Because that stream is a persistent connection you must explicitly opt in and close it:

```go
client := mcp.NewClient(url, auth, "")

// Opt in to the SSE push channel. After Initialize the client opens a GET
// event-stream connection; on a listChanged notification it invalidates its
// tool cache automatically and fires the callback.
client.OnToolsChanged(func() {
    log.Println("tool set changed — re-fetch when convenient")
}).EnableNotifications()

defer client.Close() // releases the SSE connection

tools, _ := client.ListTools(ctx) // caches 1 tool
server.NotifyToolsChanged()       // (server-side change)
// ... the callback fires; the cache is cleared. The next call re-fetches:
tools, _ = client.ListTools(ctx)
```

- `EnableNotifications()` opens the SSE reader (HTTP only; no-op for stdio, which is already a persistent connection). Returns the client for chaining.
- `OnToolsChanged` / `OnResourcesChanged` / `OnPromptsChanged` register callbacks. The tools callback fires *after* the cache is cleared.
- `Close()` stops the reader and releases the connection. **Always close a client that has enabled notifications** — otherwise the SSE connection lingers.

Notifications are off by default because a long-lived connection is only wanted when you intend to consume change events.

## Client: receiving notifications (stdio)

stdio clients need no `EnableNotifications` call — the transport is already a persistent connection. The same `On*Changed` callbacks fire, and the tool cache is invalidated automatically:

- **Legacy server:** the server sends list-changed notifications on the connection, and the client receives them.
- **Modern (2026-07-28) server:** the server sends them only on a `subscriptions/listen` request, so after `Initialize` the client opens one automatically (for tools, resources and prompts) and keeps it open until `Close`, which cancels it. A server that does not implement `subscriptions/listen` over stdio answers Method not found, and the client then receives no change notifications; this library's own `Server` is such a server, but it also sends every connected stdio client its notifications on the connection, so nothing is lost against it.
- Clients built with `NewStdioClient` or `NewStreamClient` receive notifications. A client over a bare `jsonrpc.Client` has no inbound message handler and receives none.

```go
client, _ := mcp.NewStdioClient("my-server", nil, "")
defer client.Close()
client.OnToolsChanged(func() { log.Println("tools changed") })
```

## Federation: propagation

When a server re-exports a remote MCP server (via `RegisterRemoteServer`), it can propagate that remote's changes to its own clients. The propagation hook is installed automatically; it fires when the remote client receives a `notifications/tools/list_changed` and is opted in to notifications:

```go
upstreamClient := mcp.NewClient(upstreamURL, auth, "")
upstreamClient.EnableNotifications() // required to receive from upstream
server.RegisterRemoteServer(upstreamClient)
```

Now a tool added upstream flows through automatically:

```
upstream adds a tool  ->  emits tools/list_changed
                     ->  federator's client receives it, refreshes its merged cache
                     ->  federator re-emits tools/list_changed
                     ->  federator's clients re-fetch and see the new tool
```

The federator calls `RefreshTools` before re-emitting, so the new tool is actually present in the merged list its clients re-fetch. Resources/prompts are not federated, so only tool changes propagate.

The propagation hook is per registration, not per client: registering the same client on several servers (for example a chat-side server and a public endpoint server built from one connection) gives each server its own propagation, and none of them cancels another's.

## Transports

- **HTTP**: the server serves a `GET` event-stream (opened when the client sends `Accept: text/event-stream`). Notifications are written as SSE `data:` events carrying a JSON-RPC notification. A heartbeat keeps the stream alive.
- **stdio**: the server writes notification frames directly to the stream (sharing the response write mutex so frames never interleave); the client's bidirectional peer delivers them to handlers.

## See Also

- [Tool Providers Guide](tool-providers.md) — the main reason to call `NotifyToolsChanged` manually
- [Resources Guide](resources.md) / [Prompts Guide](prompts.md)
- [Remote Servers Guide](remote-servers.md) — federation via `RegisterRemoteServer`
