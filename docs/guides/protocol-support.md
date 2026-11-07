# Protocol Support Guide

The MCP library supports every MCP protocol revision from 2024-11-05 through the
current 2026-07-28, across two distinct eras, on both `Server` and `Client`, over
both HTTP and stdio. Detection and behavior selection are automatic — there is no
configuration required to get this.

## Legacy and Modern

The spec's revision history splits into two eras with genuinely different wire
models:

- **Legacy** (`2024-11-05` through `2025-11-25`): the familiar `initialize`
  handshake negotiates a session once, and subsequent requests are implicitly
  scoped to that session.
- **Modern** (`2026-07-28`, current): stateless and per-request. There is no
  handshake — every request carries its own protocol version and client
  capabilities in `_meta`, and on HTTP two extra headers (`Mcp-Method`,
  `Mcp-Name`) mirror the request's method and target for routing/observability,
  validated against the JSON-RPC body on arrival.

A `Server` built with this library implements both simultaneously ("Dual-era"):
each incoming request is routed to whichever era it actually speaks, based only
on the request body: any `io.modelcontextprotocol/protocolVersion` `_meta`
claim that does not name a supported Legacy revision selects the Modern path (on
any transport), so an unknown or newer revision gets
`UnsupportedProtocolVersionError` listing every supported version; a claim naming
a Legacy revision is Legacy, and a request with no claim, including
`initialize`, is always Legacy. The `MCP-Protocol-Version` header is only a
cross-check (a Modern header with no `_meta` claim is refused as a malformed
Modern request), and `Mcp-Method`/`Mcp-Name` are never an era signal — Legacy
clients such as mcp-remote mirror them on every request too. Detection never
looks at the transport, URL, or configuration. A Legacy request never touches any
Modern-era code path, and vice versa.

A `Client` detects which era its server speaks the first time `Initialize` runs:
it attempts a Modern `server/discover` call first, and falls back to the Legacy
`initialize` handshake if that doesn't succeed (per the spec's own documented
backward-compatibility algorithm). Every other `Client` method — `ListTools`,
`CallTool`, `ReadResource`, and so on — is completely unaffected by which era was
detected; they call the same private helper either way.

## Supported Protocol Versions

- **2024-11-05** (Legacy, minimum supported)
- **2025-03-26** (Legacy)
- **2025-06-18** (Legacy)
- **2025-11-25** (Legacy, latest)
- **2026-07-28** (Modern, current)

## Legacy Version Features

### 2024-11-05 (Basic)

Basic tool listing and execution, resource access, no advanced features. Use
case: legacy clients, minimal functionality requirements.

### 2025-03-26 and Later (Enhanced)

Adds the `listChanged` capability flags and `notifications/*/list_changed`. Use
case: Legacy clients that want change notifications over the `GET` SSE stream.

### 2025-06-18 and 2025-11-25 (Latest Legacy)

2025-06-18 requires the `MCP-Protocol-Version` header on every request after
`initialize` (the server assumes 2025-03-26 when it is absent, as the spec
allows) and removes JSON-RPC batching; 2025-11-25 adds `title`, icons and
tool annotations on the primitives. Session management (`MCP-Session-Id`,
including the JWT manager) is available to every Legacy revision. Use case:
production Legacy deployments.

Note: 2024-11-05 is served over Streamable HTTP and stdio. The original
HTTP+SSE transport of that revision (a `GET` that announces an `endpoint`
event) is not implemented, on either the server or the client.

## Modern (2026-07-28)

### `server/discover`

Replaces `initialize` for Modern clients: a single stateless call returning
supported protocol versions (Legacy and Modern both — this library's server is
always Dual-era), capabilities, server identity, and instructions.

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "server/discover",
  "params": {
    "_meta": {
      "io.modelcontextprotocol/protocolVersion": "2026-07-28",
      "io.modelcontextprotocol/clientCapabilities": {}
    }
  }
}
```

### Per-request `_meta` and headers

Every other Modern request carries the same `_meta.protocolVersion` /
`clientCapabilities`, plus, on HTTP, `Mcp-Method` (mirroring `method`) and
`Mcp-Name` (mirroring `params.name` or `params.uri`, for `tools/call`,
`resources/read`, and `prompts/get`). A mismatch between a header and the body
is rejected with `HeaderMismatch` (`-32020`); an unsupported version with
`UnsupportedProtocolVersionError` (`-32022`); a missing required `_meta` field
with the standard `-32602` (Invalid params).

### Caching hints

`server/discover`, `tools/list`, `prompts/list`, `resources/list`,
`resources/templates/list`, and `resources/read` results automatically carry
`ttlMs` and `cacheScope`, as the spec requires — a real client's schema
validation rejects a result missing them. This library always sends `ttlMs: 0`
(no invented cache lifetime) and `cacheScope: "public"` for lists (or
`"private"` for `resources/read`); nothing to configure.

### `subscriptions/listen`

The Modern replacement for the Legacy `GET` SSE stream: a `POST` that opens a
stream scoped to just the notification types the caller asked for, acknowledged
first, each notification correlated by `subscriptionId`.

```go
// Before shutting down the HTTP server, let open subscriptions/listen
// streams close gracefully (they respond to their original request with a
// completion result first, per the spec, rather than the connection just
// going dead):
server.Shutdown()
httpServer.Shutdown(ctx)
```

Not implemented over stdio: stdio's existing connection-scoped notification
delivery already reaches every connected client regardless of era, so nothing
is functionally lost, and a stdio client that calls `subscriptions/listen`
gets a standard `Method not found` rather than a partial implementation.

### Icons

Tools, resources, resource templates, prompts, and a server's own identity can
all carry `Icon` values (`Src`, `MimeType`, `Sizes`, `Theme`), reported
identically in both eras (`icons` on the Legacy descriptor / `serverInfo`, and
the same field under Modern's `_meta.serverInfo`):

```go
server.RegisterTool(
    mcp.NewTool("weather", "Get the weather").
        Icons(mcp.Icon{Src: "https://example.com/weather.png", MimeType: "image/png"}),
    handler,
)
server.SetIcons(mcp.Icon{Src: "https://example.com/server.png"})
```

This library only carries icon metadata — it never fetches or renders icon
bytes. A consumer that does render icons is responsible for the spec's
security precautions (reject non-`https(s)`/`data:` schemes, verify
same-origin, fetch without credentials, validate content by magic bytes).

### Tool parameters mirrored to headers (`x-mcp-header`)

A tool can mark string, integer or boolean parameters for mirroring into
`Mcp-Param-<Name>` HTTP headers, so gateways can route on them without parsing
the body:

```go
server.RegisterTool(
    mcp.NewTool("query", "Run a query",
        mcp.String("region", "Region", mcp.Required(), mcp.HTTPHeader("Region")),
        mcp.String("sql", "Query"),
    ),
    handler,
)
```

- **Server:** a Modern `tools/call` whose `Mcp-Param-*` headers don't match
  the arguments (missing, different, invalid characters, malformed Base64, or
  sent for an argument with no value) is rejected with `400` and
  `HeaderMismatch` (`-32020`) before the tool runs. Integers compare
  numerically. This covers registered tools, federated remote tools and
  context-provider tools. `RegisterTool` panics on an invalid annotation.
- **Client (Modern, HTTP):** annotated arguments are mirrored automatically,
  Base64-encoded when not header-safe; tool definitions with invalid
  annotations are left out of `ListTools`; a `HeaderMismatch` refreshes the
  tool list and retries the call once.
- Legacy-era requests, and stdio, don't use the mechanism.

### Pagination

`ListTools`, `ListResources`, `ListResourceTemplates` and `ListPrompts` follow
`nextCursor` to the end of the list (bounded; a repeated cursor is an error).

### Known gaps

- MRTR (server-initiated sampling/elicitation/roots) doesn't apply: this
  library implements none of those capabilities in either era.

## Streamed responses (both eras)

On the Streamable HTTP transport both eras let a server answer a request with
either `application/json` or a `text/event-stream` (SSE) stream, and require
clients to handle both. A plain JSON response sends nothing until the handler
finishes, so any intermediary with a read timeout (Cloudflare's ~100s origin
timeout, nginx `proxy_read_timeout`, load balancers) kills a slow tool call.

**Server.** For `tools/call`, `resources/read` and `prompts/get`, when the
client's `Accept` header lists `text/event-stream`:

- A call that finishes within the streaming delay (default 2s) is answered as
  `application/json`, exactly as before.
- A longer call switches to SSE (`Cache-Control: no-cache, no-transform`,
  `X-Accel-Buffering: no`), sends a `: ping` comment every keep-alive interval
  (default 15s) so intermediaries see traffic, then the JSON-RPC response as a
  single event, and closes the stream.
- A client disconnect cancels the request's context and nothing further is
  sent (2026-07-28: closing the stream is cancellation).
- No SSE event ids are sent: the stream is not resumable (2026-07-28 removes
  resumability; this server never offered replay for Legacy).

```go
server.SetResponseStreaming(5*time.Second, 20*time.Second) // delay, keep-alive
server.SetResponseStreaming(-1, 0)                          // disable: always JSON
```

**Client.** A streamed response is parsed incrementally (WHATWG SSE rules:
CR/LF/CRLF, comments, multi-line data, event types). The call completes on the
response whose id matches the request; notifications that arrive first
(`notifications/progress`, `notifications/message`, list changes) reach the
notification handlers in order. Legacy era only: a server-to-client request
on the stream is answered (`ping` with an empty result, anything else with
Method not found), and a stream that closes after an event id is resumed with
`GET` + `Last-Event-ID` after the server's `retry` delay (bounded retries).
Cancelling the call's context closes the stream.

## Version Negotiation (Legacy)

During initialization, clients specify their preferred protocol version:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "initialize",
  "params": {
    "protocolVersion": "2025-11-25",
    "capabilities": {},
    "clientInfo": { "name": "client", "version": "1.0" }
  }
}
```

If the server supports the requested version it echoes it back; otherwise it
answers with the latest version it supports (the spec's MUST), and the client
refuses the session if it cannot speak that version. A value that is not a
protocol version at all (not `YYYY-MM-DD`) is rejected with
`Unsupported protocol version`. After a successful `initialize` the client
sends `notifications/initialized`, and includes the negotiated
`MCP-Protocol-Version` (and `MCP-Session-Id`, if issued) on every later
request. The server acknowledges notifications, and client responses, with
`202 Accepted` and no body.

The server responds with the negotiated version and capabilities:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "protocolVersion": "2025-11-25",
    "capabilities": {
      "tools": { "listChanged": false },
      "resources": { "subscribe": false, "listChanged": false }
    },
    "serverInfo": { "name": "server", "version": "1.0" }
  }
}
```

## Protocol Headers (Legacy)

For requests after initialization, Legacy clients must include the protocol
version header:

```
MCP-Protocol-Version: 2025-11-25
```

## Backwards Compatibility

- Every Legacy protocol version continues to work exactly as before — Modern
  support is purely additive, routed to by new code paths that a Legacy
  request never enters.
- New features are additive, not breaking, in both eras.
- Session management is optional (disabled by default) and Legacy-only; Modern
  is stateless by design.

## Testing Different Versions

```bash
# Legacy, latest
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "MCP-Protocol-Version: 2025-11-25" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}'

# Legacy, oldest
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "MCP-Protocol-Version: 2024-11-05" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}'

# Modern
curl -X POST http://localhost:8080/mcp \
  -H "Content-Type: application/json" \
  -H "MCP-Protocol-Version: 2026-07-28" \
  -H "Mcp-Method: server/discover" \
  -d '{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'
```

## Troubleshooting

**Error: "Unsupported protocol version"**

- Check that the requested version is in the supported list for its era.
- Update the client to use a supported version.

**Error: "Missing MCP-Protocol-Version header" (Legacy)**

- For Legacy protocol versions 2025-03-26+, the header is required after
  initialization. Add it to every request after `initialize`.

**Error: "Header mismatch" (Modern)**

- `Mcp-Method`, `Mcp-Name`, and `MCP-Protocol-Version` must exactly match the
  corresponding value in the JSON-RPC body. This is almost always a client bug
  — a hand-rolled Modern request that didn't mirror the body into headers.

**Sessions not working (Legacy only — Modern has none by design)**

- Session management is disabled by default.
- Use `mcp.NewJWTSessionManagerWithAutoKey()` or `mcp.NewJWTSessionManager()` to
  create a session manager.
- Call `server.SetSessionManager()` to enable sessions.
- For custom storage, implement the `SessionManager` interface.

**A Modern client seems stuck on Legacy, or vice versa**

- `Client` detects era once, during `Initialize`, and caches it for that
  `Client`'s lifetime. A fresh `Client` re-detects from scratch.
- `RemoteProvider`-based tool federation creates a fresh `Client` per refresh
  today, so era is re-detected on every refresh — correct, if slightly
  wasteful, not a bug.
