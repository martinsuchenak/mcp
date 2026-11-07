package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/paularlott/mcp/toon"
)

// ToolResponse represents the response from a tool.
//
// IsError marks a tool execution error (an API failure, a business-rule or
// input-validation failure): per the spec it is reported inside the result,
// with Content explaining what went wrong so the model can self-correct,
// not as a JSON-RPC protocol error. A handler that returns a plain Go error
// is converted to this shape; return a *ToolError only for a genuine
// protocol error (invalid params, internal failure).
type ToolResponse struct {
	Content           []ToolContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

// NewToolResponseError builds a tool execution error result: text explains
// the failure and isError is set, as the spec's tool error model requires.
func NewToolResponseError(text string) *ToolResponse {
	return &ToolResponse{Content: []ToolContent{{Type: "text", Text: text}}, IsError: true}
}

func NewToolResponseMulti(responses ...*ToolResponse) *ToolResponse {
	var allContent []ToolContent
	var structuredContent any

	isError := false
	for _, resp := range responses {
		if resp.Content != nil {
			allContent = append(allContent, resp.Content...)
		}
		if resp.StructuredContent != nil {
			structuredContent = resp.StructuredContent
		}
		isError = isError || resp.IsError
	}

	return &ToolResponse{
		Content:           allContent,
		StructuredContent: structuredContent,
		IsError:           isError,
	}
}

func NewToolResponseText(text string) *ToolResponse {
	return &ToolResponse{Content: []ToolContent{{Type: "text", Text: text}}}
}

func NewToolResponseJSON(data any) *ToolResponse {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return &ToolResponse{Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Error marshaling data: %v", err)}}}
	}
	return NewToolResponseText(string(jsonData))
}

// NewToolResponseAuto builds a ToolResponse from a loose value, applying the
// same conversion the server uses internally:
//   - a *ToolResponse is returned unchanged,
//   - a string becomes a text response,
//   - anything else is JSON-encoded.
//
// Use it in ToolProvider.ExecuteTool when you have a dynamic value (e.g. the
// output of a script or a remote call) rather than an already-built response.
// Prefer the specific NewToolResponse* constructors when you know the type.
func NewToolResponseAuto(value any) *ToolResponse {
	if tr, ok := value.(*ToolResponse); ok {
		return tr
	}
	if str, ok := value.(string); ok {
		return NewToolResponseText(str)
	}
	return NewToolResponseJSON(value)
}

func NewToolResponseTOON(data any) *ToolResponse {
	toonData, err := toon.Encode(data)
	if err != nil {
		return &ToolResponse{Content: []ToolContent{{Type: "text", Text: fmt.Sprintf("Error encoding data: %v", err)}}}
	}
	return NewToolResponseText(toonData)
}

func NewToolResponseImage(data []byte, mimeType string) *ToolResponse {
	return &ToolResponse{Content: []ToolContent{{Type: "image", Data: base64.StdEncoding.EncodeToString(data), MimeType: mimeType}}}
}

func NewToolResponseAudio(data []byte, mimeType string) *ToolResponse {
	return &ToolResponse{Content: []ToolContent{{Type: "audio", Data: base64.StdEncoding.EncodeToString(data), MimeType: mimeType}}}
}

func NewToolResponseResource(uri, text, mimeType string) *ToolResponse {
	return &ToolResponse{Content: []ToolContent{{Type: "resource", Resource: &ResourceContent{URI: uri, Text: text, MimeType: mimeType}}}}
}

// NewToolResponseResourceLink returns a link to a resource the client may
// read or subscribe to, as the spec's resource_link block: the resource's
// uri and name at the top level. Use NewToolResponseResource to embed the
// resource's contents instead.
func NewToolResponseResourceLink(uri, name string) *ToolResponse {
	return &ToolResponse{Content: []ToolContent{{Type: "resource_link", URI: uri, Name: name}}}
}

// NewToolResponseStructured builds a structured tool result: data is set as
// StructuredContent, and — per the spec's backwards compatibility guidance
// ("a tool that returns structured content SHOULD also return the
// serialized JSON in a TextContent block") — the same JSON is also included
// as a text content block, for clients that don't read structuredContent.
//
// Any JSON value is accepted: 2026-07-28 allows any value in
// structuredContent, while the Legacy revisions require an object, so the
// Legacy transport path drops a non-object value from the result (the text
// block still carries it). Data that cannot be marshaled is reported as a
// tool execution error.
func NewToolResponseStructured(data any) *ToolResponse {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return NewToolResponseError(fmt.Sprintf("Error marshaling data: %v", err))
	}
	return &ToolResponse{
		Content:           []ToolContent{{Type: "text", Text: string(jsonData)}},
		StructuredContent: data,
	}
}

// legacyStructuredContent returns v when it marshals to a JSON object, else
// nil: Legacy revisions (through 2025-11-25) define structuredContent as an
// object, so a Legacy client is not sent anything else.
func legacyStructuredContent(v any) any {
	if v == nil {
		return nil
	}
	if jsonData, err := json.Marshal(v); err != nil || !isJSONObject(jsonData) {
		return nil
	}
	return v
}

// isJSONObject reports whether marshaled JSON is an object ("{...}"), per the
// MCP spec's requirement that structuredContent be a JSON object.
func isJSONObject(jsonData []byte) bool {
	for _, b := range jsonData {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '{':
			return true
		default:
			return false
		}
	}
	return false
}

// jsonValueKind gives a short human-readable description of a non-object
// JSON value's shape, for the NewToolResponseStructured error message.
func jsonValueKind(jsonData []byte) string {
	for _, b := range jsonData {
		switch b {
		case ' ', '\t', '\n', '\r':
			continue
		case '[':
			return "an array"
		case '"':
			return "a string"
		case 'n':
			return "null"
		case 't', 'f':
			return "a boolean"
		default:
			return "a number"
		}
	}
	return "an empty value"
}
