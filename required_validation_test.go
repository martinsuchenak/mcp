package mcp

import (
	"context"
	"testing"
)

func TestRequiredParameterValidation(t *testing.T) {
	server := NewServer("test", "1.0")

	tool := NewTool("test", "Test tool",
		String("name", "Name parameter", Required()),
	)

	handler := func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("success"), nil
	}

	server.RegisterTool(tool, handler)

	// Test with missing required parameter
	_, err := server.CallTool(context.Background(), "test", map[string]any{})
	if err == nil {
		t.Fatal("Expected error for missing required parameter, got nil")
	}

	toolErr, ok := err.(*ToolError)
	if !ok {
		t.Fatalf("Expected ToolError, got %T", err)
	}

	if toolErr.Code != ErrorCodeInvalidParams {
		t.Errorf("Expected error code %d, got %d", ErrorCodeInvalidParams, toolErr.Code)
	}

	// An empty string is a present value: JSON Schema's required means
	// present, not non-empty, so it is accepted.
	if _, err = server.CallTool(context.Background(), "test", map[string]any{"name": ""}); err != nil {
		t.Fatalf("empty string for a required parameter must be accepted, got %v", err)
	}

	// Test with valid parameter
	_, err = server.CallTool(context.Background(), "test", map[string]any{"name": "Alice"})
	if err != nil {
		t.Fatalf("Expected no error with valid parameter, got %v", err)
	}
}
