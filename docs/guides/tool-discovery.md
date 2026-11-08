# Tool Discovery and Searchable Tools Guide

When working with many tools, the context window can become bloated with tool definitions. The server provides built-in support for discoverable tools - tools that can be discovered via keyword search rather than appearing in the main `tools/list`.

This approach is inspired by [Anthropic's Tool Search Tool](https://www.anthropic.com/engineering/advanced-tool-use) pattern, which can reduce token usage by up to 85% while maintaining access to your full tool library.

## Tool Visibility

Tools have a visibility property that determines how they appear to clients:

### Native Tools (default) - Visible in `tools/list`

```go
// Using RegisterTool - creates a native tool
server.RegisterTool(
    mcp.NewTool("get_status", "Get system status"),
    handleGetStatus,
)

// Or using the builder (native tool - no .Discoverable())
tool := mcp.NewTool("get_status", "Get system status").Build()
```

- **Visible in `tools/list`**: Yes
- **Directly callable**: Yes
- **Searchable via `tool_search`**: Yes

### Discoverable Tools - Hidden from `tools/list`, searchable only

```go
// Using RegisterTool with Discoverable()
server.RegisterTool(
    mcp.NewTool("send_email", "Send an email",
        mcp.String("to", "Recipient", mcp.Required()),
        mcp.String("subject", "Subject", mcp.Required()),
    ).Discoverable("email", "notification", "smtp"),
    handleSendEmail,
)

// Or using the builder with Discoverable()
tool := mcp.NewTool("send_email", "Send an email",
    mcp.String("to", "Recipient", mcp.Required()),
    mcp.String("subject", "Subject", mcp.Required()),
).Discoverable("email", "notification", "smtp").Build()
```

- **Visible in `tools/list`**: No (only via `tool_search`)
- **Directly callable**: Yes (if you know the name)
- **Searchable via `tool_search`**: Yes

When you register any discoverable tool, `tool_search` and `execute_tool` are automatically registered as native tools.

## Visibility Modes

The server supports two modes for controlling how tools are exposed to clients.

### Normal Mode (default)

Native tools appear in `tools/list`. Discoverable tools are hidden but searchable.

```go
http.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
    ctx := mcp.WithToolProviders(r.Context(), providers...)
    server.HandleRequest(w, r.WithContext(ctx))
})
```

**Behavior:**

- Native tools (including from providers) appear in `tools/list`
- Discoverable tools are hidden but searchable via `tool_search`
- `tool_search` and `execute_tool` appear if any discoverable tools exist

### Show-All Mode (for MCP chaining)

ALL tools appear in `tools/list`, including discoverable ones. This is useful when one MCP server connects to another and needs to see all available tools.

**Header-Based:**

```
X-MCP-Show-All: true
```

**Query Parameter:**

```
/mcp?show_all=true
```

**Programmatic:**

```go
http.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
    ctx := mcp.WithToolProviders(r.Context(), providers...)
    ctx = mcp.WithShowAllTools(ctx)
    server.HandleRequest(w, r.WithContext(ctx))
})
```

**Behavior:**

- ALL tools (native and discoverable) appear in `tools/list`
- `tool_search` searches both native and discoverable tools
- `execute_tool` is the always-safe way to call a tool returned by `tool_search`
- Tools that are also present in `tools/list` may still be called directly
- All tools remain callable

**When to use:**

- MCP server chaining (downstream server needs full tool list)
- Debugging/development to see all available tools
- Internal tooling that needs complete tool visibility

### Session-Based Mode

With session management enabled, the mode is captured during `initialize` and stored in the session:

```go
// Enable session management
sm, _ := mcp.NewJWTSessionManagerWithAutoKey(30 * time.Minute)
server.SetSessionManager(sm)

// The server automatically handles mode from headers/query params
http.HandleFunc("/mcp", server.HandleRequest)
```

- Mode is captured during `initialize` and stored in the session
- All subsequent requests in the session use the stored mode
- Mode cannot be changed mid-session (create a new session to change)

## Provider Tools with Visibility

When creating tool providers, set the `Visibility` field on the MCPTool:

```go
type MyProvider struct{}

func (p *MyProvider) GetTools(ctx context.Context) ([]mcp.MCPTool, error) {
    return []mcp.MCPTool{
        {
            Name:        "native_tool",
            Description: "Always visible",
            Visibility:  mcp.ToolVisibilityNative,
        },
        {
            Name:        "discoverable_tool",
            Description: "Only via search",
            Keywords:    []string{"search", "keywords"},
            Visibility:  mcp.ToolVisibilityDiscoverable,
        },
    }, nil
}
```

## The LLM Workflow with Tool Discovery

1. **Initialize with minimal context**: Only essential tools + `tool_search`/`execute_tool` visible
2. **Search for needed tools**: `tool_search(query="email")` → finds "send_email" with full schema
3. **Execute discovered tool**: `execute_tool(name="send_email", parameters={...})` → executes the tool
4. **Repeat as needed**: LLM can search for different tools as needed

### execute_tool arguments

`execute_tool` takes `name` (the exact tool name) and `parameters` (the
tool's arguments). `parameters` may be an object or a string holding a JSON
object; `arguments` is accepted in its place for backward compatibility.
Anything else is refused with a tool execution error (`isError: true`) that
says what to send — an unknown key, a `parameters` value that isn't an
object, or both `parameters` and `arguments` — so the target tool never runs
with arguments other than those sent. The target's own input schema then
applies as for a direct call. `tool_search` refuses unknown keys the same way.

## Example Implementation

See `examples/tool-discovery/` for a complete example demonstrating:

- Tool registration strategies
- Search filtering
- Mode switching
- Response formatting

## Context Window Optimization

**Typical scenario without tool discovery:**

- 20 tools × 50 lines per tool definition = 1000 tokens wasted for a simple query

**With tool discovery:**

- `tool_search` and `execute_tool` = ~20 tokens
- Only requested tool definitions are sent = ~50 tokens per tool needed
- Savings: Up to 85% for typical use cases

## See Also

- [Tool Providers Guide](tool-providers.md) - Dynamic tool loading
- [Dynamic Tool States Guide](dynamic-tool-states.md) - Context-based visibility control
