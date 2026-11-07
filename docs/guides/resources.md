# Resources Guide

Resources are server-provided data that clients read by URI — files, configuration, database records, or any addressable content. They are the data counterpart to tools: tools *do* things, resources *expose* things.

This server implements the full MCP resources protocol — `resources/list`, `resources/read`, and `resources/templates/list` — over both HTTP and stdio.

## Resources vs Tools

| | Tools | Resources |
| --- | --- | --- |
| Verb | `tools/call` (an action) | `resources/read` (a fetch) |
| Identified by | name | URI |
| Input | arguments | none (just the URI) |
| Appears in | `tools/list` | `resources/list` |

Use a tool when the model should take an action with arguments; use a resource when the model should read a piece of data the server knows how to serve.

## Static Resources

A static resource has a fixed URI. Register it with `RegisterResource`:

```go
server.RegisterResource(
    mcp.NewResource("config://app", "App Config", "The application configuration", "application/json"),
    func(ctx context.Context, uri string) (*mcp.ResourceResponse, error) {
        cfg := readConfig()
        return mcp.NewResourceResponseText(uri, cfg, "application/json"), nil
    },
)
```

`NewResource` takes `uri, name, description, mimeType` (description and mimeType may be empty). The resource appears in `resources/list`; reading it invokes the handler with that URI.

### Response Constructors

Handlers return a `*ResourceResponse`. Use:

- `mcp.NewResourceResponseText(uri, text, mimeType)` — text content (e.g. JSON, source code, logs)
- `mcp.NewResourceResponseBlob(uri, data, mimeType)` — binary content, base64-encoded automatically (e.g. images, PDFs)

```go
// Binary example
server.RegisterResource(
    mcp.NewResource("image://logo", "Logo", "The company logo", "image/png"),
    func(ctx context.Context, uri string) (*mcp.ResourceResponse, error) {
        data, _ := os.ReadFile("logo.png")
        return mcp.NewResourceResponseBlob(uri, data, "image/png"), nil
    },
)
```

### Unregistering

```go
server.UnregisterResource("config://app") // returns true if it existed
```

## Resource Templates

A resource template has a URI with `{var}` placeholders (RFC 6570 level 1). It appears in `resources/templates/list`; the client expands it into a concrete URI and reads that. Register one with `RegisterResourceTemplate`:

```go
server.RegisterResourceTemplate(
    mcp.NewResourceTemplate("user://{id}", "User Profile", "A user profile by ID", "application/json"),
    func(ctx context.Context, req *mcp.ResourceRequest) (*mcp.ResourceResponse, error) {
        // The server matches the URI against the template and extracts the
        // variables for you — no manual parsing required.
        id := req.StringOr("id", "")
        profile := fetchProfile(id)
        return mcp.NewResourceResponseText(req.URI(), profile, "application/json"), nil
    },
)
```

The handler receives a `*ResourceRequest` (the resource analogue of `ToolRequest` / `PromptRequest`):

- `req.URI()` — the full expanded URI the client requested.
- `req.String("name")` — a template variable, or `ErrUnknownParameter` if absent.
- `req.StringOr("name", default)` — a variable with a fallback.
- `req.Vars()` — all extracted variables as a map.

A single template can have several placeholders, e.g. `repo://{owner}/{repo}/readme`, each available by name.

Matching rules:

- Each `{var}` matches one or more characters in the requested URI.
- On `resources/read`, static resources (exact match) are tried first, then templates in registration order (first match wins), then request-scoped providers.
- Static resources have no variables; only `req.URI()` is meaningful for them.

### Parsing template URIs yourself

`ResourceProvider` implementations (which the server can't match for you) and other advanced cases can extract variables from a template URI with the standalone helper:

```go
vars, err := mcp.MatchResourceTemplate("repo://{owner}/{repo}", "repo://acme/widget")
// vars == map[string]string{"owner": "acme", "repo": "widget"}
```

## Reading Resources

Reads resolve in this order:

1. **Static resources** — exact URI match.
2. **Static templates** — pattern match, first match wins.
3. **Resource providers** — see [Per-User / Session Resources](#per-user--session-resources).

An unrecognised URI returns a `Resource not found` error (`mcp.ErrUnknownResource` from the direct `ReadResource` API).

### Direct API

You can read a resource in-process without going through the protocol:

```go
resp, err := server.ReadResource(ctx, "config://app")
resp, err := server.ReadResource(ctx, "user://42") // resolves through a template
```

## Per-User / Session Resources

For multi-tenant or per-user data, statically-registered resources are global and shared across all requests. To expose resources scoped to the current user/session, use `ResourceProvider` — the resource analogue of `ToolProvider`.

### The ResourceProvider Interface

```go
type ResourceProvider interface {
    GetResources(ctx context.Context) (*ProvidedResources, error)
    ReadResource(ctx context.Context, uri string) (*ResourceResponse, error)
}
```

`GetResources` returns the descriptors (static resources and/or templates) this provider exposes; `ReadResource` serves a URI. Attach providers to the request context with `WithResourceProviders`:

```go
type UserResourceProvider struct {
    user *User
}

func (p *UserResourceProvider) GetResources(ctx context.Context) (*mcp.ProvidedResources, error) {
    return &mcp.ProvidedResources{
        Resources: []mcp.MCPResource{
            {URI: "user://" + p.user.ID + "/profile", Name: "My Profile", MimeType: "application/json"},
        },
    }, nil
}

func (p *UserResourceProvider) ReadResource(ctx context.Context, uri string) (*mcp.ResourceResponse, error) {
    if !strings.HasPrefix(uri, "user://"+p.user.ID+"/") {
        return nil, mcp.ErrUnknownResource // miss: let other providers try
    }
    return mcp.NewResourceResponseText(uri, loadProfile(p.user), "application/json"), nil
}
```

### The Per-Request Entry Point

Inject the provider in HTTP middleware, exactly like tool providers:

```go
func handler(w http.ResponseWriter, r *http.Request) {
    user := authenticateUser(r)
    if user == nil {
        http.Error(w, "Unauthorized", 401)
        return
    }
    ctx := mcp.WithResourceProviders(r.Context(), &UserResourceProvider{user: user})
    server.HandleRequest(w, r.WithContext(ctx))
}
```

Providers stack — multiple can be attached and all are queried. On `resources/read` they are tried in attachment order; the first that handles the URI wins.

### The Miss Contract

When a provider does not handle a URI, return `(nil, mcp.ErrUnknownResource)`. This is the canonical "not handled" signal — the server moves on to the next provider, and if none handles the URI the caller gets `ErrUnknownResource`. Reserve real errors for genuine failures (backend down, permission denied); a non-nil, non-`ErrUnknownResource` error aborts dispatch immediately.

### How resources merge with providers

`resources/list` returns static resources **plus** provider resources (duplicates by URI removed, static wins). `resources/templates/list` does the same for templates. So a request with providers attached sees the union of global and per-user resources.

## Client Usage

The `Client` talks to any MCP server (HTTP or stdio) and exposes resource methods:

```go
// List resources
resources, _ := client.ListResources(ctx)
for _, r := range resources {
    fmt.Println(r.URI, r.Name)
}

// Read a resource
resp, _ := client.ReadResource(ctx, "config://app")
for _, c := range resp.Contents {
    fmt.Println(c.URI, c.MimeType, c.Text) // or c.Blob for binary
}
```

Resources are not cached on the client (unlike tools) — each call performs a fresh request, since resource sets can change between calls.

## Transports

Resources work identically over both transports:

- **HTTP**: `resources/list`, `resources/read`, `resources/templates/list` are routed automatically by `HandleRequest`.
- **Stdio**: the same methods are routed by the jsonrpc dispatcher in `ServeStdio`.

See the [stdio-server](../../examples/stdio-server) and [stdio-client](../../examples/stdio-client) examples for a complete resource round-trip over stdio.

## Capabilities

The server always advertises the `resources` capability in `initialize` (`listChanged` is `true` from 2025-03-26 on — see the [Notifications guide](notifications.md); `subscribe` is `false`, since per-resource update subscriptions are not implemented). Clients can therefore rely on `resources/*` methods being available regardless of how many resources are registered.

## Security

- **Providers are per-request**: create them fresh with the authenticated user/session each time.
- **Hiding ≠ access control**: a resource not listed for a user can still be read if you don't check ownership inside `ReadResource`. Always validate inside the handler/provider.
- **No shared state**: the server stores no user/session data; all isolation lives in the provider.

## See Also

- [Response Types Guide](response-types.md) — embedding resource content in tool responses
- [Tool Providers Guide](tool-providers.md) — the tool-side equivalent of `ResourceProvider`
- [Protocol Support Guide](protocol-support.md) — capabilities by protocol version
