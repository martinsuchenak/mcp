package mcp

import "fmt"

// MCP JSON-RPC Error Codes
// These are standard JSON-RPC 2.0 error codes used by the MCP protocol.
// See: https://www.jsonrpc.org/specification#error_object
const (
	// ErrorCodeParseError indicates invalid JSON was received by the server.
	// Use when the request body cannot be parsed as JSON.
	ErrorCodeParseError = -32700

	// ErrorCodeInvalidRequest indicates the JSON sent is not a valid Request object.
	// Use when required fields are missing or have wrong types.
	ErrorCodeInvalidRequest = -32600

	// ErrorCodeMethodNotFound indicates the method does not exist or is not available.
	// Used internally when an unknown MCP method is called.
	ErrorCodeMethodNotFound = -32601

	// ErrorCodeInvalidParams indicates invalid method parameters.
	// Use this in tool handlers when required parameters are missing or invalid.
	// Prefer using NewToolErrorInvalidParams() helper.
	ErrorCodeInvalidParams = -32602

	// ErrorCodeInternalError indicates an internal JSON-RPC error.
	// Use this in tool handlers for unexpected server-side errors.
	// Prefer using NewToolErrorInternal() helper.
	ErrorCodeInternalError = -32603

	// ErrorCodeImplementationErrorStart is the start of JSON-RPC's
	// implementation-defined server error range (-32000 to -32099).
	//
	// Do not pick new application codes here: MCP 2026-07-28 reserves
	// -32020..-32099 for protocol-defined codes (implementations MUST NOT
	// emit others in that range) and says new implementations SHOULD NOT use
	// -32000..-32019 either. Application-specific errors should use a code
	// outside -32768..-32000, and tool failures should be tool execution
	// errors (NewToolResponseError) rather than protocol errors at all.
	ErrorCodeImplementationErrorStart = -32000

	// ErrorCodeImplementationErrorEnd is the end of that range.
	ErrorCodeImplementationErrorEnd = -32099

	// ErrorCodeResourceNotFound is the Legacy (through 2025-11-25) code for a
	// resources/read of an unknown resource. 2026-07-28 withdraws it in
	// favour of ErrorCodeInvalidParams, which the Modern path uses.
	ErrorCodeResourceNotFound = -32002

	// ErrorCodeHeaderMismatch indicates a Modern-era (protocol revision
	// 2026-07-28+) request's mirrored HTTP headers (MCP-Protocol-Version,
	// Mcp-Method, Mcp-Name, Mcp-Param-*) don't match the corresponding values
	// in the JSON-RPC request body, or a required header is missing.
	ErrorCodeHeaderMismatch = -32020

	// ErrorCodeMissingRequiredClientCapability indicates a Modern-era request
	// needs a capability the client didn't declare in its per-request
	// io.modelcontextprotocol/clientCapabilities metadata.
	ErrorCodeMissingRequiredClientCapability = -32021

	// ErrorCodeUnsupportedProtocolVersion indicates the requested protocol
	// version (Modern or Legacy) isn't one this server implements. The
	// error's data.supported field lists the versions it does support.
	ErrorCodeUnsupportedProtocolVersion = -32022
)

// ToolError represents an MCP protocol error that can be returned from tool handlers.
// When returned from a ToolHandler, the error code and message are sent to the client
// in the JSON-RPC error response.
//
// Returning one in a tool handler or provider:
//
//	func myHandler(ctx context.Context, req *mcp.ToolRequest) (*mcp.ToolResponse, error) {
//	    name, err := req.String("name")
//	    if err != nil {
//	        return nil, mcp.NewToolErrorInvalidParams("name parameter is required")
//	    }
//	    // ... process request
//	}
//
// Detecting one as a caller (middleware, tests, custom transports) — use
// errors.As, which also unwraps wrapped errors, to recover the code/message/data:
//
//	resp, err := server.CallTool(ctx, name, args)
//	var toolErr *mcp.ToolError
//	if errors.As(err, &toolErr) {
//	    log.Printf("tool error %d: %s (data=%v)", toolErr.Code, toolErr.Message, toolErr.Data)
//	}
type ToolError struct {
	Code    int
	Message string
	Data    any

	// invalidArguments marks the arguments failing the tool's input
	// schema: tools/call reports it as a tool execution error.
	invalidArguments bool
}

func (e *ToolError) Error() string {
	return fmt.Sprintf("MCP Error %d: %s", e.Code, e.Message)
}

// NewToolErrorInvalidParams creates an error for invalid or missing parameters.
// Use this when a required parameter is missing, has the wrong type, or fails validation.
// This returns ErrorCodeInvalidParams (-32602).
func NewToolErrorInvalidParams(message string) error {
	return &ToolError{
		Code:    ErrorCodeInvalidParams,
		Message: message,
	}
}

// NewToolErrorInternal creates an error for internal server errors.
// Use this for unexpected failures like database errors, network issues, etc.
// This returns ErrorCodeInternalError (-32603).
func NewToolErrorInternal(message string) error {
	return &ToolError{
		Code:    ErrorCodeInternalError,
		Message: message,
	}
}

// NewToolError creates a custom MCP protocol error with a specific code.
// Reserve it for genuine protocol-level failures; a failure of the tool's own
// work (an upstream API error, a business rule, bad input values) should be
// a tool execution error — return NewToolResponseError, or simply a plain
// error, which the server reports inside the result with isError set so the
// model can read it and self-correct.
//
// Pick application-specific codes outside -32768..-32000: 2026-07-28
// reserves -32020..-32099 for the protocol (a Modern client is never sent a
// foreign code from that range — it is remapped to -32603, with the original
// kept in data) and discourages -32000..-32019 for new implementations. The
// data parameter can include additional error details and will be serialized to JSON.
//
// Example:
//
//	return nil, mcp.NewToolError(1001, "Rate limit exceeded", map[string]any{
//	    "retry_after": 60,
//	    "limit": 100,
//	})
func NewToolError(code int, message string, data any) error {
	return &ToolError{
		Code:    code,
		Message: message,
		Data:    data,
	}
}
