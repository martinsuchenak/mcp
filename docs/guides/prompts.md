# Prompts Guide

Prompts are reusable, named message templates with arguments. A client fills in the arguments and the server renders them into messages for the model. They are the third MCP primitive, alongside tools (which *do* things) and resources (which *expose* data).

This server implements `prompts/list` and `prompts/get` over both HTTP and stdio.

## When to Use Prompts

| Primitive | Verb | Identified by | Best for |
| --- | --- | --- | --- |
| Tools | `tools/call` (action) | name | Side effects, computations |
| Resources | `resources/read` (fetch) | URI | Addressable data |
| Prompts | `prompts/get` (render) | name | Pre-built workflows, structured instructions |

Use a prompt when you want to ship a canned workflow or instruction template that the client fills in — e.g. a `code_review` prompt that takes `code` + `language` and emits a review request.

## Defining a Prompt

Register a prompt with `RegisterPrompt`. Declare its arguments with the fluent `Argument` method:

```go
server.RegisterPrompt(
    mcp.NewPrompt("code_review", "Review code").
        Argument("code", "The code to review", true).
        Argument("language", "Programming language", false),
    func(ctx context.Context, req *mcp.PromptRequest) (*mcp.PromptResponse, error) {
        code, _ := req.String("code")
        language := req.StringOr("language", "unknown")
        return mcp.NewPromptResponseText(fmt.Sprintf("Review this %s code:\n%s", language, code)), nil
    },
)
```

`Argument(name, description, required)` — the third argument marks whether the client must supply it. Required arguments are validated automatically before the handler runs; a missing required argument returns an `Invalid params` error.

The handler receives a `*PromptRequest` with string accessors:

- `req.String(name)` — returns the value or `ErrUnknownParameter`
- `req.StringOr(name, default)` — returns the value or a default
- `req.Args()` — the full argument map

## Response Constructors

Handlers return a `*PromptResponse` made of one or more messages. Each message has a role (`user` or `assistant`) and a content block.

- `mcp.NewPromptResponseText(text)` — a single `user` text message (the common case)
- `mcp.NewPromptResponseMessages(messages...)` — multiple/arbitrary messages
- `mcp.NewPromptTextMessage(role, text)` — build a single text message

```go
// A multi-turn prompt: assistant sets context, then user asks.
return mcp.NewPromptResponseMessages(
    mcp.NewPromptTextMessage(mcp.PromptRoleAssistant, "I'm ready to review code."),
    mcp.NewPromptTextMessage(mcp.PromptRoleUser, "Please review: "+code),
), nil
```

### Rich content

Message content uses the same block shape as tool responses, so you can embed images, audio, or resources in a prompt:

```go
img, _ := os.ReadFile("screenshot.png")
msg := mcp.PromptMessage{
    Role: mcp.PromptRoleUser,
    Content: mcp.PromptMessageContent{
        Type: "image", Data: base64.StdEncoding.EncodeToString(img), MimeType: "image/png",
    },
}
return mcp.NewPromptResponseMessages(msg), nil
```

## Unregistering

```go
server.UnregisterPrompt("code_review") // returns true if it existed
```

## Listing and Rendering

### Direct API

```go
prompts := server.ListPrompts(ctx)
resp, err := server.GetPrompt(ctx, "code_review", map[string]string{"code": "fmt.Println()"})
```

### Protocol

`prompts/list` returns the prompt descriptors (name, description, arguments). `prompts/get` takes `name` and `arguments` and returns the rendered messages.

## Per-User / Session Prompts

Statically-registered prompts are global. For multi-tenant or per-user prompts, use `PromptProvider` — the prompt analogue of `ToolProvider` and `ResourceProvider`.

### The PromptProvider Interface

```go
type PromptProvider interface {
    GetPrompts(ctx context.Context) ([]MCPPrompt, error)
    GetPrompt(ctx context.Context, name string, args map[string]string) (*PromptResponse, error)
}
```

Attach it in request middleware:

```go
type UserPromptProvider struct{ user *User }

func (p *UserPromptProvider) GetPrompts(ctx context.Context) ([]mcp.MCPPrompt, error) {
    return []mcp.MCPPrompt{
        {Name: "summarize_my_work", Description: "Summarize " + p.user.Name + "'s work"},
    }, nil
}

func (p *UserPromptProvider) GetPrompt(ctx context.Context, name string, args map[string]string) (*mcp.PromptResponse, error) {
    if name != "summarize_my_work" {
        return nil, mcp.ErrUnknownPrompt // miss: let other providers try
    }
    return mcp.NewPromptResponseText(summarize(p.user)), nil
}

func handler(w http.ResponseWriter, r *http.Request) {
    user := authenticateUser(r)
    ctx := mcp.WithPromptProviders(r.Context(), &UserPromptProvider{user: user})
    server.HandleRequest(w, r.WithContext(ctx))
}
```

Providers stack — `prompts/list` returns static prompts **plus** provider prompts (duplicates by name removed, static wins). On `prompts/get`, static prompts are tried first, then providers in attachment order; first hit wins.

### The Miss Contract

When a provider does not handle a name, return `(nil, mcp.ErrUnknownPrompt)`. The server moves on to the next provider. Reserve real errors for genuine failures; a non-nil, non-`ErrUnknownPrompt` error aborts dispatch immediately.

## Client Usage

```go
prompts, _ := client.ListPrompts(ctx)
for _, p := range prompts {
    fmt.Println(p.Name, p.Description)
    for _, a := range p.Arguments {
        fmt.Println("  -", a.Name, "(required:", a.Required, ")")
    }
}

resp, _ := client.GetPrompt(ctx, "code_review", map[string]string{"code": "x = 1"})
for _, msg := range resp.Messages {
    fmt.Println(msg.Role, msg.Content.Text)
}
```

Prompts are not cached on the client — each call performs a fresh request.

## Capabilities

The server always advertises the `prompts` capability in `initialize` (`listChanged` is `true` from 2025-03-26 on — see the [Notifications guide](notifications.md)).

## See Also

- [Resources Guide](resources.md) — addressable data by URI
- [Tool Providers Guide](tool-providers.md) — the tool-side provider pattern
- [Response Types Guide](response-types.md) — content block types shared with tool responses
