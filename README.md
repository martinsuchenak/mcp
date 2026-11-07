# MCP Server Library for Go

A Go library for building [Model Context Protocol (MCP)](https://modelcontextprotocol.io/) servers with a clean, fluent API.

## Features

- **HTTP & Stdio Transports**: Serve the same tools over HTTP or newline-delimited JSON-RPC on stdin/stdout, and consume remote servers over either
- **Simple API**: Fluent interface for defining tools and parameters
- **Type Safety**: Strongly typed parameter access with automatic conversion
- **Rich Responses**: Support for text, image, audio, resource, and structured content
- **Resources**: Serve addressable data by URI (`resources/list`, `resources/read`, resource templates), including per-user/session resources via `ResourceProvider`
- **MCP Apps**: Link a tool to an interactive HTML UI resource (SEP-1865), with CSP/permission metadata, extension capability negotiation, and host/view auto-resize via `containerDimensions`
- **Prompts**: Reusable message templates with arguments (`prompts/list`, `prompts/get`), including per-user/session prompts via `PromptProvider`
- **Skills**: Agent Skills directories served per SEP-2640 (`skills/list`, `skills/get`): every skill file is an ordinary `skill://` resource with digests, frontmatter served verbatim from the SKILL.md; per-user/session skills via a `SkillProvider` on the request context
- **ListChanged Notifications**: Push-based tool/resource/prompt refresh over HTTP (SSE) and stdio, with automatic propagation through federated servers
- **TOON Support**: Compact, human-readable JSON encoding for LLM prompts
- **Thread Safe**: Concurrent request handling with mutex protection
- **Remote Servers**: Connect to and proxy remote MCP servers with authentication
- **Remote Search**: Delegate tool_search to remote servers to discover hidden tools
- **Parallel Tool Calls**: Execute multiple tools concurrently and collect all results in one call
- **Searchable Tools**: Reduce context window usage with on-demand tool discovery
- **Dynamic Tool Providers**: Load tools from external sources (databases, scripts, APIs)
- **Per-User Remote Servers**: Request-scoped `RemoteProvider` for federating remote MCP servers with per-user auth, filtering, and caching
- **Provider Composition**: Combine providers with `MultiProvider` using clear miss/error semantics
- **Streamed Responses**: Long-running `tools/call`, `resources/read` and `prompts/get` switch to an SSE stream with keep-alives, so proxies with read timeouts (Cloudflare, nginx, load balancers) don't cut them off; fast calls stay plain JSON
- **Header Routing**: `x-mcp-header` tool parameters mirrored to `Mcp-Param-*` HTTP headers (`HTTPHeader()` option), validated by the server and sent automatically by the client
- **MCP Compliant**: Full support for protocol versions 2025-03-26 through 2026-07-28, on both server and client, negotiating the highest version both sides support
- **Authorization**: OAuth 2.1 resource-server helpers (protected resource metadata, Bearer challenges, audience and scope checks, a JWT/JWKS validator) and a spec-compliant OAuth client (RFC 9728 discovery, PKCE, resource indicators)

## Installation

```bash
go get github.com/paularlott/mcp
```

## Quick Start

```go
package main

import (
    "context"
    "fmt"
    "log"
    "net/http"

    "github.com/paularlott/mcp"
)

func main() {
    // Create server
    server := mcp.NewServer("my-server", "1.0.0")

    // Register a tool
    server.RegisterTool(
        mcp.NewTool("greet", "Greet someone",
            mcp.String("name", "Name to greet", mcp.Required()),
            mcp.String("greeting", "Custom greeting"),
        ),
        func(ctx context.Context, req *mcp.ToolRequest) (*mcp.ToolResponse, error) {
            name, _ := req.String("name")
            greeting := req.StringOr("greeting", "Hello")
            return mcp.NewToolResponseText(fmt.Sprintf("%s, %s!", greeting, name)), nil
        },
    )

    // Start server
    http.HandleFunc("/mcp", server.HandleRequest)
    log.Fatal(http.ListenAndServe(":8000", nil))
}
```

## Transports

The same server and its tools can be served over two transports.

### HTTP

Mount the server as an `http.Handler` (as in Quick Start above):

```go
http.HandleFunc("/mcp", server.HandleRequest)
```

A request from a client that accepts `text/event-stream` and runs longer than
2 seconds is answered as an SSE stream with a keep-alive comment every 15
seconds; anything faster is a normal JSON response. Tune or disable it with:

```go
server.SetResponseStreaming(5*time.Second, 20*time.Second) // delay, keep-alive
server.SetResponseStreaming(-1, 0)                          // always JSON
```

The client handles both response forms transparently.

### Stdio

Serve the MCP protocol over stdin/stdout (newline-delimited JSON-RPC 2.0) — the
transport a host launches as a subprocess. `ServeStdio` blocks until stdin
reaches EOF; keep stdout for protocol frames only and send logs to stderr:

```go
server := mcp.NewServer("my-server", "1.0.0")
// ... RegisterTool(...) as usual ...
if err := server.ServeStdio(context.Background()); err != nil {
    log.Fatal(err)
}
```

Use `ServeStream(ctx, in, out)` to serve over any pair of streams (for example
an in-process pipe) rather than the process's own stdio.

A client connects to a stdio server by launching it as a subprocess:

```go
client, err := mcp.NewStdioClient("my-server-binary", []string{"--flag"}, "")
if err != nil {
    log.Fatal(err)
}
defer client.Close()

tools, _ := client.ListTools(ctx)
resp, _ := client.CallTool(ctx, "greet", map[string]any{"name": "Ada"})
```

`NewStdioClient` accepts options such as `WithClientStderr`, `WithClientEnv`,
`WithClientExtraEnv`, and `WithClientDir`. `WithClientEnv` replaces the child's
whole environment, while `WithClientExtraEnv` adds or overrides individual
`KEY=VALUE` variables on top of the inherited parent environment (so `PATH`,
`HOME`, etc. are preserved). To talk to a server over streams you already hold, use
`NewStreamClient(in, out, namespace)`. The stdio transport is built on the
[`paularlott/jsonrpc`](https://github.com/paularlott/jsonrpc) package. The
client API (`ListTools`, `CallTool`, namespacing, filtering) is identical across
HTTP and stdio.

`CallToolsParallel` and `ExecuteDiscoveredToolsParallel` send every call as a
single JSON-RPC batch over the stdio transport (one round-trip instead of one
per call), falling back to concurrent individual calls over HTTP. Either way,
results are always returned in call order.

## Documentation

For comprehensive guides, patterns, and API documentation, see the [docs/](docs/) directory:

### Get Started

- **[Tool Providers](docs/guides/tool-providers.md)** - Dynamic tool loading, per-request providers, visibility control, and show-all mode
- **[Tool Discovery](docs/guides/tool-discovery.md)** - Searchable tools and context window optimization

### How-To Guides

- **[Remote Servers](docs/guides/remote-servers.md)** - Connecting and proxying remote MCP servers, parallel tool calls
- **[Resources](docs/guides/resources.md)** - Serving addressable data by URI, resource templates, and per-user/session resources
- **[Prompts](docs/guides/prompts.md)** - Reusable message templates with arguments, and per-user/session prompts
- **Skills** - Serving Agent Skills directories (`skills/list`, `skills/get`, `skill://` resources) per SEP-2640, statically or per-request via `SkillProvider`
- **[Notifications](docs/guides/notifications.md)** - Push-based list refresh (listChanged) over HTTP and stdio, with federation propagation
- **[Protocol Support](docs/guides/protocol-support.md)** - Supported protocol versions (2025-03-26 through 2026-07-28), version negotiation, streamed responses, `x-mcp-header`, and pagination
- **[Sessions](docs/guides/sessions.md)** - Optional session management (MCP 2025-11-25)
- **[Response Types](docs/guides/response-types.md)** - Text, images, audio, and structured responses
- **[Error Handling](docs/guides/error-handling.md)** - Structured error patterns and best practices
- **[Thread Safety & Concurrency](docs/guides/concurrency.md)** - Safe concurrent usage patterns
- **[MCP Apps](docs/guides/mcp-apps.md)** - Interactive UI resources linked to tools (SEP-1865), CSP metadata, and extension capability negotiation

## Tool Discovery Mode

The server supports two modes for tool visibility, selectable via HTTP header:

### Header-Based Mode Selection

Clients can request show-all mode (useful for MCP server chaining):

```http
POST /mcp HTTP/1.1
Content-Type: application/json
X-MCP-Show-All: true

{"jsonrpc":"2.0","id":1,"method":"initialize",...}
```

Or via query parameter: `/mcp?show_all=true`

**Normal Mode (default):** Native tools visible in `tools/list`. If discoverable tools exist, `tool_search` and `execute_tool` are also available.

**Show-All Mode:** ALL tools visible in `tools/list` including discoverable ones. This is useful when chaining MCP servers together.

### Discoverable Tools

Tools can be marked as discoverable, meaning they won't appear in the normal `tools/list` but can be found via `tool_search`:

```go
// Discoverable tool using the fluent builder
server.RegisterTool(
    mcp.NewTool("specialized_tool", "A specialized tool").Discoverable("keyword1", "keyword2"),
    handler,
)
```

With session management enabled, the mode is stored in the session token. See [Tool Discovery Guide](docs/guides/tool-discovery.md) for details.

## Examples

See the [examples/](examples/) directory for complete, runnable examples:

- [Basic Server (HTTP)](examples/server/)
- [Stdio Server](examples/stdio-server/) and [Stdio Client](examples/stdio-client/)
- [Multi-Tenant Tools](examples/per-user-tools/)
- [Tool Discovery](examples/tool-discovery/)
- [Remote Servers](examples/remote-server/)
- [Session Management](examples/session-server/)
- [OpenAI Integration](examples/openai/)
- [MCP Apps Dashboard](examples/mcp-app-dashboard/) — a tool linked to an interactive HTML UI (SEP-1865)
- [MCP Apps Host Harness](examples/mcp-app-host-harness/) — a minimal host to render and test one

## Testing

Run tests to ensure everything works:

```bash
go test ./...
```

View specific test categories:

```bash
# Provider tests
go test -run TestAddRemoveProviders -v

# Dynamic tool state transitions
go test -run TestDynamicToolState -v

# Full test suite
go test -v
```
