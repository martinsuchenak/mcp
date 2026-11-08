# Error Handling

## Error Types

### Invalid Parameters

Use when a required parameter is missing, the wrong type, or fails validation:

```go
func handleGreet(ctx context.Context, req *mcp.ToolRequest) (*mcp.ToolResponse, error) {
    name, _ := req.String("name")
    if name == "" {
        return nil, mcp.NewToolErrorInvalidParams("name parameter is required")
    }
    return mcp.NewToolResponseText(fmt.Sprintf("Hello, %s!", name)), nil
}
```

### Internal Server Error

Use when a server-side operation fails:

```go
result, err := database.Query(ctx)
if err != nil {
    log.Printf("query failed: %v", err) // log the detail
    return nil, mcp.NewToolErrorInternal("database operation failed") // don't expose internals
}
```

### Custom Errors

Use when you need a specific error code or structured detail:

```go
return nil, mcp.NewToolError(1001, "file too large", map[string]any{
    "max_size": maxSize,
    "provided": size,
})
```

Prefer a tool execution error for anything that is the tool's own failure
(an upstream API error, a business rule, a value in the wrong format): return
`mcp.NewToolResponseError("...")`, or simply a plain `error`, and the server
reports it inside the result with `isError: true`, which is what the MCP tools
spec asks for and what lets the model read the message and self-correct.
`NewToolError` is for protocol-level failures only.

## Argument Validation

Arguments are checked against the tool's input schema before the handler
runs. A missing required argument, a value of the wrong type or an unknown
argument comes back as a tool execution error (`isError: true`) whose text
names the problem — the MCP tools spec counts input validation errors as tool
execution errors, so the model can correct the call. `Server.CallTool`
callers get a `*mcp.ToolError` with code `-32602` for the same failures.

## Error Code Reference

Standard JSON-RPC codes used by the library:

| Code | Meaning |
|---|---|
| `-32700` | Parse error |
| `-32600` | Invalid request |
| `-32601` | Method not found |
| `-32602` | Invalid params |
| `-32603` | Internal error |

Pick custom codes outside `-32768..-32000`: MCP 2026-07-28 reserves
`-32020..-32099` for protocol-defined codes and discourages `-32000..-32019`
for new implementations. A Modern (2026-07-28) client is never sent a foreign
code from that range — the server remaps it to `-32603` and keeps the original
in `data.originalCode`.

## Best Practices

- Write specific messages: `"email must be a valid address"` not `"invalid parameter"`
- Never expose internal error details (stack traces, query strings, file paths) to the client — log them server-side
- Use `NewToolErrorInvalidParams` for client mistakes, `NewToolErrorInternal` for server failures, custom codes for domain errors

## See Also

- [Response Types Guide](response-types.md) — successful response patterns
