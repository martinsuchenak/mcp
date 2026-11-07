# Remote Servers and MCP Clients Guide

## MCP Client

Connect to remote MCP servers:

```go
// Bearer token authentication
auth := mcp.NewBearerTokenAuth("your-token")
client := mcp.NewClient("https://api.example.com/mcp", auth, "namespace")

// OAuth2 authentication
oauth := mcp.NewOAuth2Auth("client-id", "client-secret", "https://auth.example.com/token", []string{"mcp:read"})
client := mcp.NewClient("https://api.example.com/mcp", oauth, "namespace")

// Use client directly
tools, err := client.ListTools(ctx)

// Call a tool with a plain map
result, err := client.CallTool(ctx, "tool-name", map[string]any{"key": "value"})

// Or with the Args fluent builder
result, err := client.CallTool(ctx, "tool-name", mcp.Args{}.Arg("key", "value").Arg("limit", 10))
```

## Args Builder

`Args` is a `map[string]any` type alias with a fluent `Arg` method. Both forms are interchangeable anywhere a `map[string]any` is accepted:

```go
// Plain map
client.CallTool(ctx, "search", map[string]any{"query": "go", "limit": 10})

// Fluent builder - same result
client.CallTool(ctx, "search", mcp.Args{}.Arg("query", "go").Arg("limit", 10))

// Also works in ToolCall for parallel calls
mcp.ToolCall{Name: "search", Arguments: mcp.Args{}.Arg("query", "go")}
```

## Tool Filtering

Filter which tools are exposed from a remote server. The filter is applied to both `ListTools()` and `CallTool()`:

```go
// Create client with namespace
client := mcp.NewClient("https://api.example.com/mcp", auth, "github")

// Set a filter - only allow specific tools
client.WithToolFilter(func(toolName string) bool {
    // toolName is the original name WITHOUT namespace prefix
    allowedTools := map[string]bool{
        "search_repos":  true,
        "list_issues":   true,
        "create_issue":  true,
    }
    return allowedTools[toolName]
})

// ListTools only returns filtered tools
tools, _ := client.ListTools(ctx)  // Only returns 3 tools

// CallTool rejects filtered-out tools
_, err := client.CallTool(ctx, "github/delete_repo", args)  // Returns ErrToolFiltered
```

### Filter Patterns

```go
// Exclude dangerous tools
client.WithToolFilter(func(name string) bool {
    return name != "delete" && name != "drop_database"
})

// Allow only read operations
client.WithToolFilter(func(name string) bool {
    return strings.HasPrefix(name, "get_") || strings.HasPrefix(name, "list_")
})

// Dynamic filter from database/config
client.WithToolFilter(func(name string) bool {
    return db.IsToolEnabled(serverID, name)
})

// Clear filter to re-enable all tools
client.WithToolFilter(nil)
_ = client.RefreshToolCache(ctx)  // Refresh to pick up new tools
```

### Filter Behavior

- Filter receives the **original tool name** (without namespace prefix)
- Setting a filter **clears the tool cache** automatically
- `ErrToolFiltered` is returned when calling a filtered-out tool
- `nil` filter means all tools are allowed (default)

## Unified Server with Remote Tools

Register remote servers directly with your local server for a unified tool interface:

```go
// Create server
server := mcp.NewServer("my-server", "1.0.0")

// Register local tools (as usual)
server.RegisterTool(
    mcp.NewTool("local-greet", "Local greeting",
        mcp.String("name", "Name to greet", mcp.Required()),
    ),
    func(ctx context.Context, req *mcp.ToolRequest) (*mcp.ToolResponse, error) {
        name, _ := req.String("name")
        return mcp.NewToolResponseText(fmt.Sprintf("Hello, %s!", name)), nil
    },
)

// Create clients for remote servers
aiAuth := mcp.NewBearerTokenAuth("ai-tools-token")
aiClient := mcp.NewClient("https://ai.example.com/mcp", aiAuth, "ai")

dataAuth := mcp.NewOAuth2Auth("client-id", "client-secret", "https://auth.example.com/token", []string{"mcp:read"})
dataClient := mcp.NewClient("https://data.example.com/mcp", dataAuth, "data")

// Register remote servers - tools appear with namespace prefix
server.RegisterRemoteServer(aiClient)
server.RegisterRemoteServer(dataClient)

// ListToolsWithContext returns all tools (local + remote with namespaces)
tools := server.ListToolsWithContext(ctx) // Returns: ["local-greet", "ai/generate-text", "data/query", ...]

// CallTool with intelligent routing
result, err := server.CallTool(ctx, "local-greet", args)       // Calls local tool
result, err := server.CallTool(ctx, "ai/generate-text", args)  // Calls remote AI tool
result, err := server.CallTool(ctx, "unknown-tool", args)      // Returns ErrUnknownTool

// Serve unified interface as HTTP endpoint
http.HandleFunc("/mcp", server.HandleRequest)
```

### Excluding MCP Apps tools

A remote's tool list can include MCP Apps views — tools linked to a `ui://` resource via `_meta.ui.resourceUri` that a host UI renders as a sandboxed app. Federating such a tool re-serves it under your namespace, but the view's in-page code calls its own tools by bare, host-agnostic names, which a namespaced endpoint cannot resolve. If the server you're building mounts no views of its own, exclude them:

```go
server.RegisterRemoteServer(aiClient, mcp.WithRemoteExcludeApps())

// or with ReplaceRemoteServers:
server.ReplaceRemoteServers([]mcp.RemoteServerEntry{
    {Client: aiClient, Visibility: mcp.ToolVisibilityNative, ExcludeApps: true},
})
```

Excluded app tools are absent from `tools/list` and `tool_search`, cannot be called through the server (including via the namespace-prefix fallback that search-discovered tools use), and their `ui://` resources are not forwarded for `resources/read`. `ToolIsApp(tool)` reports whether a descriptor is an app, if you need the same check elsewhere.

Registration without the option federates app tools unchanged — a server that does mount views (a chat host, for example) keeps them. The same client can be registered on multiple servers, once with `ExcludeApps` and once without, to serve both audiences from one connection.

## Parallel Tool Calls

Execute multiple tools concurrently and collect all results in one call. Results are returned in the same order as the input, and a failure in one call does not affect the others.

### CallToolsParallel

For tools that are directly callable (native tools):

```go
results := client.CallToolsParallel(ctx, []mcp.ToolCall{
    {Name: "weather",     Arguments: mcp.Args{}.Arg("city", "London")},
    {Name: "news",        Arguments: mcp.Args{}.Arg("topic", "tech")},
    {Name: "ai/summarize", Arguments: map[string]any{"text": doc}},
})

for _, r := range results {
    if r.Err != nil {
        log.Printf("%s failed: %v", r.Name, r.Err)
        continue
    }
    // use r.Response
}
```

### ExecuteDiscoveredToolsParallel

For tools found via `ToolSearch` (discoverable tools):

```go
results := client.ExecuteDiscoveredToolsParallel(ctx, []mcp.ToolCall{
    {Name: "send_email",   Arguments: mcp.Args{}.Arg("to", "<email>").Arg("subject", "Hello")},
    {Name: "send_sms",     Arguments: mcp.Args{}.Arg("to", "<phone>").Arg("body", "Hello")},
    {Name: "send_webhook", Arguments: map[string]any{"url": "https://example.com/hook"}},
})

for _, r := range results {
    if r.Err != nil {
        log.Printf("%s failed: %v", r.Name, r.Err)
        continue
    }
    // use r.Response
}
```

### ParallelToolResult Fields

| Field | Type | Description |
|---|---|---|
| `Name` | `string` | Tool name from the input `ToolCall` |
| `Response` | `*ToolResponse` | Response on success, `nil` on error |
| `Err` | `error` | Error on failure, `nil` on success |

### When to Use Parallel Calls

- Fetching data from multiple independent sources simultaneously
- Fan-out patterns where results are aggregated before responding
- Reducing total latency when tools have no dependencies on each other

## Tool Resolution

- **Namespaced calls** (`namespace/tool-name`): Route directly to the specified remote server
- **Non-namespaced calls**: Try local tools first, then fast lookup for remote tools
- **Fallback resolution**: If a tool isn't in the known tool list but matches a remote server's namespace prefix, it's executed via `execute_tool` on that remote (supports tools discovered through remote search)
- **Caching**: Remote tool lists are cached and can be refreshed with `RefreshTools(ctx)`
- **Error handling**: Failed remote servers are skipped gracefully during registration

## Remote Tool Search

When aggregating multiple MCP servers, some remote servers may have their own discoverable tools that aren't visible in `tools/list`. Remote search lets the aggregator delegate `tool_search` to those remotes, collecting and prefixing their results.

### Enabling Remote Search

Remote search is **off by default** and must be explicitly enabled per-server:

```go
// Option 1: Functional option on registration
server.RegisterRemoteServer(client, mcp.WithRemoteSearch())
server.RegisterRemoteServerDiscoverable(client, mcp.WithRemoteSearch())

// Option 2: Via ReplaceRemoteServers
server.ReplaceRemoteServers([]mcp.RemoteServerEntry{
    {Client: client, Visibility: mcp.ToolVisibilityNative, RemoteSearch: true},
})
```

### How It Works

When a remote server has remote search enabled and `tool_search` is called on the aggregator:

1. The aggregator performs its local search as usual
2. For each remote with remote search enabled, it calls `tool_search` on that remote
3. Remote results are prefixed with the server's namespace (e.g., `github__create_issue`)
4. Combined results are sorted by score and truncated to `max_results`

```go
// Set up an aggregator with remote search
server := mcp.NewServer("aggregator", "1.0.0")

githubClient := mcp.NewClient("https://github-mcp.example.com", auth, "github")
dbClient := mcp.NewClient("https://db-mcp.example.com", auth, "db")

// Both remotes have hidden tools we want to discover
server.RegisterRemoteServer(githubClient, mcp.WithRemoteSearch())
server.RegisterRemoteServer(dbClient, mcp.WithRemoteSearch())

// When the LLM calls tool_search:
// - Aggregator searches its own tools
// - Calls tool_search on github-mcp and db-mcp
// - Returns merged results: ["github__create_issue", "db__run_query", ...]
```

### Executing Remote Discovered Tools

Tools found through remote search can be executed via `execute_tool` or directly by name. The aggregator routes namespaced tool names to the correct remote server:

```go
// Via execute_tool (always works)
result, _ := server.CallTool(ctx, "execute_tool", map[string]any{
    "name":       "github__create_issue",
    "parameters": map[string]any{"title": "Bug fix"},
})

// Direct call (also works - namespace prefix routes to the right remote)
result, _ := server.CallTool(ctx, "github__create_issue", map[string]any{
    "title": "Bug fix",
})
```

When a tool name isn't in the pre-fetched tool list but matches a remote server's namespace prefix, the aggregator automatically delegates via `execute_tool` on that remote. This means tools discovered through remote search that weren't in the initial `tools/list` are still callable.

### When to Use Remote Search

- **Aggregator patterns**: When llmrouter or similar proxies federate multiple MCP servers
- **Nested discovery**: When remote servers have their own discoverable tools
- **Large tool sets**: When you don't want to enumerate every tool upfront

### Performance Considerations

- Remote search is only called on servers where it's explicitly enabled
- If no servers have it enabled, the search path is a no-op with no overhead
- Remote calls happen sequentially per-server but can be parallelized in the future
- The `max_results` limit is passed through to remote servers to bound response size

## Per-User Remote Servers (Request-Scoped)

`RegisterRemoteServer` is process-wide: every request sees the same remotes with
the same credentials. When the set of remote servers, their auth, or their
enabled tools differs **per user**, use `NewRemoteProvider` instead. It is a
`ToolProvider` that resolves the current request's servers from the context.

Create it **once** at startup and reuse it for every request. Because it reads
the user from the context (not from construction), one instance serves all
users without leaking tools between them, and its per-server tool-list cache
persists across requests.

```go
// Created once, at startup.
remoteProvider := mcp.NewRemoteProvider(func(ctx context.Context) ([]mcp.RemoteProviderConfig, error) {
    user := userFromContext(ctx) // your own context accessor
    if user == nil {
        return nil, nil
    }

    records, err := store.ListRemoteServersForUser(user.ID)
    if err != nil {
        return nil, err
    }

    configs := make([]mcp.RemoteProviderConfig, 0, len(records))
    for _, rec := range records {
        rec := rec
        configs = append(configs, mcp.RemoteProviderConfig{
            Name: rec.Namespace,
            URL:  rec.URL,
            // Lazy auth: only resolved when this server is actually used.
            AuthFunc: func(ctx context.Context) (mcp.AuthProvider, error) {
                return authForServer(ctx, user, rec)
            },
            Visibility: visibilityFor(rec), // native or discoverable
            ToolFilter: rec.IsToolEnabled,  // applied to list and call paths
            CacheTTL:   time.Duration(rec.CacheSeconds) * time.Second,
            // Set CacheKey to isolate catalogs per user when needed:
            CacheKey: fmt.Sprintf("%s|%s", user.ID, rec.ID),
        })
    }
    return configs, nil
})

// Per request: attach the shared provider.
func handler(w http.ResponseWriter, r *http.Request) {
    ctx := mcp.WithShowAllFromRequest(r.Context(), r, remoteProvider)
    server.HandleRequest(w, r.WithContext(ctx))
}
```

### RemoteProviderConfig fields

| Field | Purpose |
|---|---|
| `Name` | Namespace prefix for the server's tools (e.g. `github` → `github__list_repos`). Must be unique per resolver result. |
| `URL` | Remote MCP endpoint. |
| `Auth` | Static auth provider. Ignored when `AuthFunc` is set. |
| `AuthFunc` | Lazy per-request auth; only called when the server is listed or called. Use for per-user OAuth/bearer lookups. |
| `Visibility` | `ToolVisibilityNative` (in `tools/list`) or `ToolVisibilityDiscoverable` (only via `tool_search`). |
| `ToolFilter` | Restricts exposed tools; receives the original (un-namespaced) name. Applied to both list and call. |
| `CacheTTL` | Tool-list cache lifetime. Zero uses the 60s default; negative disables caching. |
| `CacheKey` | Overrides the cache key (defaults to `Name`+URL). Include a user/tenant id to isolate per-user catalogs. |
| `HTTPPool` | Optional custom HTTP pool (e.g. for self-signed internal services). |
| `Keywords` | Extra search keywords; the namespace and `remote` are always included. |

### Cache invalidation

When a user's server configuration changes, drop the cached tool list so the
next request refetches:

```go
remoteProvider.InvalidateCache(cacheKey) // single server
remoteProvider.InvalidateAllCache()      // everything
```

The cache key must match the `CacheKey` you set (or `Name`+URL when unset).

The cache is bounded by an LRU with a configurable maximum (default
`DefaultRemoteToolCacheMaxEntries`, override with `WithMaxCacheEntries`), so a
per-user/per-tenant `CacheKey` cannot grow memory without limit — the
least-recently-used entry is evicted once the cache is full, and entries also
expire after their `CacheTTL`. For very large active populations, raise the
maximum or shorten `CacheTTL` so hot users are not evicted prematurely:

```go
remoteProvider := mcp.NewRemoteProvider(resolver, mcp.WithMaxCacheEntries(10000))
```

### Native vs static remote registration

- Use `RegisterRemoteServer` / `ReplaceRemoteServers` for **global** remotes
  shared by every request (e.g. a fixed set of configured servers).
- Use `NewRemoteProvider` for **per-user/per-tenant** remotes resolved from the
  request.

## Authentication

### Bearer Token

```go
auth := mcp.NewBearerTokenAuth("your-token")
```

### OAuth2 with Client Credentials

```go
auth := mcp.NewOAuth2Auth(
    "client-id",
    "client-secret",
    "https://auth.example.com/token",
    []string{"mcp:read", "mcp:execute"},
)
```

Pass `mcp.WithResource(resourceURL)` to send the RFC 8707 `resource`
parameter in every token request.

### Authorization (OAuth 2.1)

For servers that need a user's authorization, the library implements the
client side of the
[MCP authorization spec](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization)
up to the browser step, which is left to your application.

**Discovery.** `mcp.DiscoverOAuthMeta(ctx, serverURL)` (or
`DiscoverOAuthMetaWithClient` with your own `*http.Client`) finds the
authorization server:

1. Protected resource metadata (RFC 9728): the `resource_metadata` of the
   server's 401 `WWW-Authenticate: Bearer` challenge (an unauthenticated GET,
   then a JSON-RPC `ping` POST for servers that only take POST), then
   `/.well-known/oauth-protected-resource/<path>`, then
   `/.well-known/oauth-protected-resource`. A document for another resource
   is ignored.
2. The authorization server's metadata, for an issuer with a path, at
   `/.well-known/oauth-authorization-server/<path>`, then
   `/.well-known/openid-configuration/<path>`, then
   `<issuer>/.well-known/openid-configuration`. Its `issuer` must match exactly.
3. Only when the server publishes no protected resource metadata: metadata
   at the root of the server's own host, whose `issuer` must be the server's
   origin (servers that are their own authorization server).

All metadata and discovery URLs must be `https` (`http` is accepted only for
loopback hosts). The authorization server must advertise PKCE `S256` in
`code_challenge_methods_supported`; otherwise discovery fails with
`mcp.ErrPKCENotSupported`, and the client must not proceed.

The returned `OAuthMeta` gives what the authorization flow needs:

- `Resource`: send it as `resource=` in **both** the authorization request
  and the token request, so the token is issued for this server.
- Scopes: ask for `ChallengeScope` (the 401 challenge's `scope`) when set,
  else `ResourceScopesSupported` (from the protected resource metadata), else
  leave `scope` out; `ScopesSupported` lists the authorization server's.
- `ClientIDMetadataDocumentSupported` and
  `AuthorizationResponseIssParameterSupported` (check `iss` against `Issuer`
  when set).

```go
meta, err := mcp.DiscoverOAuthMeta(ctx, "https://api.example.com/mcp")
if err != nil {
    return err // includes errors.Is(err, mcp.ErrPKCENotSupported)
}

// Without a pre-registered client, register one (RFC 7591). application_type
// is "native" for a loopback or custom-scheme redirect URI, else "web";
// override with mcp.WithApplicationType.
clientID, err := mcp.RegisterOAuthClient(ctx, meta.RegistrationEndpoint,
    "my-app", "http://127.0.0.1:8765/callback")

// ... run the browser authorization code flow with PKCE S256 against
// meta.AuthorizationEndpoint and meta.TokenEndpoint, sending
// resource=meta.Resource in both requests ...

auth := mcp.NewOAuth2RefreshTokenAuth(meta.TokenEndpoint, clientID,
    accessToken, refreshToken, mcp.WithResource(meta.Resource))
client := mcp.NewClient("https://api.example.com/mcp", auth, "namespace")
```

`RegisterOAuthClientWithClient` takes your own `*http.Client`. A
registration answered with anything but 2xx is an error naming the server's
`error` and `error_description`.

### Protecting your server

The library does not provide resource server helpers. An MCP server that
requires authorization must:

- answer unauthenticated requests with `401` and
  `WWW-Authenticate: Bearer resource_metadata="https://host/.well-known/oauth-protected-resource/mcp"`
  (optionally with `scope="..."`);
- serve its protected resource metadata at
  `/.well-known/oauth-protected-resource[/path]`, with `resource` set to its
  own URL and `authorization_servers` naming its authorization server;
- validate every token, including that its audience is this server;
- answer a token lacking a required scope with `403` and
  `WWW-Authenticate: Bearer error="insufficient_scope", scope="..."`;
- never forward the token it received to upstream services: obtain a
  separate token for them.

## Unified Interface Benefits

- Single endpoint for multiple tool libraries
- Transparent routing with namespaces
- Works with any authentication method
- Graceful degradation if remote server unavailable
- Full protocol support (all MCP versions)

## See Also

- [Tool Providers Guide](tool-providers.md) - Local dynamic tool loading
- Protocol support documentation for version compatibility
