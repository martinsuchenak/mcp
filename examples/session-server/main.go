package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/paularlott/mcp"
)

func main() {
	// Create server with JWT-based session management (production-ready)
	server := mcp.NewServer("session-aware-server", "1.0.0")

	// Enable JWT session management (stateless, scalable, no external dependencies)
	sm, err := mcp.NewJWTSessionManagerWithAutoKey(30 * time.Minute)
	if err != nil {
		log.Fatalf("Failed to create session manager: %v", err)
	}
	server.SetSessionManager(sm)

	// No background cleanup needed for JWT sessions (they expire automatically)
	// Sessions are stateless and validated on each request

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
	fmt.Println("Server with JWT session management starting on :8000")
	fmt.Println("- JWT-based sessions (stateless, scalable)")
	fmt.Println("- No external dependencies required")
	fmt.Println("- Sessions expire after 30 minutes")
	fmt.Println("- Supports protocol versions: 2025-03-26, 2025-06-18, 2025-11-25")
	log.Fatal(http.ListenAndServe(":8000", nil))
}
