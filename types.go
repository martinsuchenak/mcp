package mcp

import "encoding/json"

// MCP Protocol types
type MCPRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// MCPNotification is a JSON-RPC 2.0 notification — a message that carries no
// "id" member. Unlike MCPRequest (whose ID field has no omitempty and would
// serialize as "id":null), this struct makes it structurally impossible to
// emit an "id" field, conforming to the JSON-RPC 2.0 spec which requires that
// notifications omit "id" entirely.
type MCPNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type MCPResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *MCPError `json:"error,omitempty"`
}

type MCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ServerInfo      serverInfo   `json:"serverInfo"`
	Instructions    string       `json:"instructions,omitempty"`
}

type capabilities struct {
	Tools map[string]any `json:"tools"`
	// Resources and Prompts are always advertised (an empty object for
	// 2024-11-05): the lifecycle spec lets a client use only the
	// capabilities it was told about, so omitting them would hide
	// resources/* and prompts/* from a compliant client.
	Resources  map[string]any `json:"resources"`
	Prompts    map[string]any `json:"prompts"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Icons   []Icon `json:"icons,omitempty"`
}

type MCPTool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"` // Human-readable display name (2025-06-18+)
	Description  string           `json:"description"`
	InputSchema  any              `json:"inputSchema"`
	OutputSchema any              `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
	Meta         map[string]any   `json:"_meta,omitempty"` // Extension metadata, e.g. MCP Apps' ui.resourceUri
	Icons        []Icon           `json:"icons,omitempty"`
	Keywords     []string         `json:"-"` // For discovery search, not serialized to clients
	Visibility   ToolVisibility   `json:"-"` // Native or Discoverable
}

// ToolAnnotations are the spec's hints about a tool's behaviour
// (2025-03-26+). Each hint is a pointer so "not stated" is distinguishable
// from false: clients treat an absent destructiveHint and openWorldHint as
// true and an absent readOnlyHint and idempotentHint as false. Clients MUST
// treat these as untrusted unless the server is trusted.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ContentAnnotations are the spec's annotations on a content block or
// resource: who it is for, how important it is, and when it last changed.
type ContentAnnotations struct {
	Audience     []string `json:"audience,omitempty"`     // "user" and/or "assistant"
	Priority     *float64 `json:"priority,omitempty"`     // 0 (least) to 1 (most important)
	LastModified string   `json:"lastModified,omitempty"` // ISO 8601
}

type ToolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// ToolResult is the wire shape of a tools/call result. Content is required
// by the schema, so it is never omitted (an empty list, not null).
type ToolResult struct {
	Content           []ToolContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

// ToolContent is one content block of a tool result or prompt message: a
// "text", "image", "audio", "resource" (embedded) or "resource_link" block.
// Only the fields of the block's type are serialized; see MarshalJSON.
type ToolContent struct {
	Type     string           `json:"type"`
	Text     string           `json:"text,omitempty"`     // text
	Data     string           `json:"data,omitempty"`     // image, audio: base64
	MimeType string           `json:"mimeType,omitempty"` // image, audio, resource_link
	Resource *ResourceContent `json:"resource,omitempty"` // resource (embedded)

	// resource_link fields (the linked Resource, flattened as the schema has it).
	URI         string `json:"uri,omitempty"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Size        *int64 `json:"size,omitempty"`

	Annotations *ContentAnnotations `json:"annotations,omitempty"`
	Meta        map[string]any      `json:"_meta,omitempty"`
}

// MarshalJSON keeps the block schema-valid: a text block always carries its
// (possibly empty) text, which omitempty alone would drop.
func (c ToolContent) MarshalJSON() ([]byte, error) {
	type plain ToolContent
	out := struct {
		plain
		Text *string `json:"text,omitempty"`
	}{plain: plain(c)}
	if c.Type == "text" {
		text := c.Text
		out.Text = &text
	}
	return json.Marshal(out)
}

type ResourceResponse struct {
	Contents []ResourceContent `json:"contents"`
}

type ResourceContent struct {
	URI      string         `json:"uri"`
	MimeType string         `json:"mimeType,omitempty"`
	Text     string         `json:"text,omitempty"`
	Blob     string         `json:"blob,omitempty"`  // base64 encoded
	Meta     map[string]any `json:"_meta,omitempty"` // Extension metadata, e.g. MCP Apps' ui.csp
}

// MarshalJSON keeps the contents schema-valid: exactly one of text or blob
// must be present, so an empty text document still carries "text": "".
func (c ResourceContent) MarshalJSON() ([]byte, error) {
	type plain ResourceContent
	out := struct {
		plain
		Text *string `json:"text,omitempty"`
	}{plain: plain(c)}
	if c.Blob == "" {
		text := c.Text
		out.Text = &text
	}
	return json.Marshal(out)
}

// MCPResource describes a static resource exposed via resources/list.
// A resource is data the server can serve to clients by URI, such as a file,
// a configuration document, or a database record.
type MCPResource struct {
	URI         string              `json:"uri"`
	Name        string              `json:"name"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	MimeType    string              `json:"mimeType,omitempty"`
	Size        *int64              `json:"size,omitempty"`
	Annotations *ContentAnnotations `json:"annotations,omitempty"`
	Meta        map[string]any      `json:"_meta,omitempty"` // Extension metadata, e.g. MCP Apps' ui hints
	Icons       []Icon              `json:"icons,omitempty"`
}

// MCPResourceTemplate describes a parameterized resource exposed via
// resources/templates/list. The URITemplate may contain {var} placeholders
// (RFC 6570 level 1) that clients expand to concrete URIs and then read.
type MCPResourceTemplate struct {
	URITemplate string              `json:"uriTemplate"`
	Name        string              `json:"name"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	MimeType    string              `json:"mimeType,omitempty"`
	Annotations *ContentAnnotations `json:"annotations,omitempty"`
	Meta        map[string]any      `json:"_meta,omitempty"` // Extension metadata, e.g. MCP Apps' ui hints
	Icons       []Icon              `json:"icons,omitempty"`
}

// resourceReadParams is the params object for resources/read.
type resourceReadParams struct {
	URI string `json:"uri"`
}

// MCPPrompt describes a prompt exposed via prompts/list. A prompt is a reusable
// message template with named arguments that produces messages for the model.
type MCPPrompt struct {
	Name        string              `json:"name"`
	Title       string              `json:"title,omitempty"`
	Description string              `json:"description,omitempty"`
	Arguments   []MCPPromptArgument `json:"arguments,omitempty"`
	Icons       []Icon              `json:"icons,omitempty"`
	Meta        map[string]any      `json:"_meta,omitempty"`
}

// MCPPromptArgument describes one argument a prompt accepts.
type MCPPromptArgument struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
}

// PromptMessageRole is the role of a message within a prompt response.
type PromptMessageRole string

const (
	// PromptRoleUser marks a message from the user.
	PromptRoleUser PromptMessageRole = "user"
	// PromptRoleAssistant marks a message from the assistant.
	PromptRoleAssistant PromptMessageRole = "assistant"
)

// PromptMessageContent is the content block of a prompt message. It is the same
// shape as the content block used by tool responses (text, image, audio, or an
// embedded resource), so the ToolContent constructors can be reused.
type PromptMessageContent = ToolContent

// PromptMessage is a single message in a prompt response.
type PromptMessage struct {
	Role    PromptMessageRole    `json:"role"`
	Content PromptMessageContent `json:"content"`
}

// PromptResponse is the result of prompts/get.
type PromptResponse struct {
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

// promptGetParams is the params object for prompts/get.
type promptGetParams struct {
	Name      string            `json:"name"`
	Arguments map[string]string `json:"arguments,omitempty"`
}
