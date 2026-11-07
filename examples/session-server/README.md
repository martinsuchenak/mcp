# Session-Aware MCP Server Example

This example demonstrates the new MCP 2025-11-25 protocol features with **JWT-based session management**:

- **Stateless Sessions**: JWT tokens require no external storage (Redis, Database)
- **Horizontal Scaling**: Works across all server instances without coordination
- **Zero Dependencies**: No infrastructure required beyond the Go server
- **Production Ready**: Cryptographically secure and self-expiring

## Why JWT Sessions?

JWT (JSON Web Token) sessions are the **recommended default** for production because:

- ✅ **Stateless**: No session storage infrastructure needed
- ✅ **Scalable**: Works perfectly in multi-instance clusters
- ✅ **Simple**: Zero external dependencies
- ✅ **Secure**: Cryptographically signed with HMAC-SHA256

Trade-off: Sessions cannot be revoked before expiry (acceptable for most use cases)

## Features Implemented

### 1. JWT Session Management

- Server generates signed JWT tokens on initialization
- Tokens contain protocol version and expiration
- Validated on every request (signature + expiry check)
- Automatic expiration (default 30 minutes)

### 2. Protocol Version Validation

Per the MCP 2025-11-25 spec:

- Clients send `MCP-Protocol-Version` header on all requests after initialization
- Server validates the version and returns 400 Bad Request if unsupported
- Defaults to `2025-03-26` if header is missing (for backwards compatibility)

### 3. HTTP Method Support

- **POST**: Standard JSON-RPC requests
- **GET**: Returns 405 (SSE streaming not yet implemented)
- **DELETE**: Terminates sessions (note: JWT sessions auto-expire, cannot be revoked)
- **OPTIONS**: CORS preflight support

## Running the Example

```bash
cd examples/session-server
go run main.go
```

## Testing with curl

### Initialize (receive JWT session token)

```bash
curl -X POST http://localhost:8000/mcp \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","clientInfo":{"name":"test","version":"1.0"}}}' \
  -v
```

Look for the `MCP-Session-Id` header in the response - this will be a JWT token.

### Subsequent requests (with JWT session token)

```bash
curl -X POST http://localhost:8000/mcp \
  -H "Content-Type: application/json" \
  -H "MCP-Protocol-Version: 2025-11-25" \
  -H "MCP-Session-Id: <jwt-token-from-init>" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
```

### Session termination

```bash
curl -X DELETE http://localhost:8000/mcp \
  -H "MCP-Session-Id: <jwt-token>"
```

Note: JWT sessions cannot be revoked - they expire naturally after 30 minutes.

## Alternative Session Managers

### Redis (If Revocation Needed)

```go
// If you need to revoke sessions before expiry
rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
server.SetSessionManager(mcp.NewRedisSessionManager(rdb, 30*time.Minute))
```

### Custom Signing Key (Persist Across Restarts)

```go
// Load key from environment or secure storage
signingKey := []byte(os.Getenv("SESSION_SIGNING_KEY"))
server.SetSessionManager(mcp.NewJWTSessionManager(signingKey, 30*time.Minute))
```

## Backwards Compatibility

The server supports older MCP protocol versions:

- `2025-03-26`: With listChanged support
- `2025-06-18`: Enhanced capabilities
- `2025-11-25`: Latest spec with full session management

Clients using older versions will work without session management or protocol version headers.
