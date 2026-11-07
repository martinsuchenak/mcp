# Session Management Guide

Enable optional session tracking for stateful interactions. **Sessions are disabled by default** and remain optional per the MCP spec.

## Quick Start

JWT-based sessions provide stateless, scalable session management with zero infrastructure dependencies:

```go
server := mcp.NewServer("my-server", "1.0.0")

// Development/single instance: auto-generated key
sm, err := mcp.NewJWTSessionManagerWithAutoKey(30 * time.Minute)
if err != nil {
    log.Fatal(err)
}
server.SetSessionManager(sm)

// Production cluster: use persisted key so all instances validate each other's sessions
signingKey := loadFromSecretStore()
server.SetSessionManager(mcp.NewJWTSessionManager(signingKey, 30*time.Minute))
```

### Why JWT Sessions?

- ✅ **Zero Dependencies**: No Redis, Database, or external storage needed
- ✅ **Horizontal Scaling**: Works perfectly across all server instances (with shared key)
- ✅ **Stateless**: Each server validates sessions independently
- ✅ **Simple**: Just create and set
- ✅ **Secure**: Cryptographically signed with HMAC-SHA256

Trade-off: Sessions cannot be revoked before expiry (acceptable for most use cases)

## Deployment Patterns

### Single Instance / Development

Use auto-generated key for simplicity:

```go
sm, _ := mcp.NewJWTSessionManagerWithAutoKey(30 * time.Minute)
server.SetSessionManager(sm)
```

### Production Cluster

Use a persisted key so all instances can validate sessions:

```go
// Load key from secure storage (Vault, K8s secrets, etc.)
signingKey := loadFromSecretStore()
server.SetSessionManager(mcp.NewJWTSessionManager(signingKey, 30*time.Minute))
```

### Generate and Persist a Key

```go
// Generate once, persist securely
key, err := mcp.GenerateSigningKey()
if err != nil {
    log.Fatal(err)
}
saveToSecretStore(key)
```

## Pluggable Session Management

The session management system is fully pluggable via the `SessionManager` interface:

```go
// SessionManager interface for pluggable session storage
type SessionManager interface {
    CreateSession(ctx context.Context, protocolVersion string, showAll bool) (string, error)
    ValidateSession(ctx context.Context, sessionID string) (bool, error)
    GetProtocolVersion(ctx context.Context, sessionID string) (string, error)
    GetShowAll(ctx context.Context, sessionID string) (bool, error)
    DeleteSession(ctx context.Context, sessionID string) error
    CleanupExpiredSessions(ctx context.Context) error
}
```

Use `SetSessionManager()` to plug in custom implementations:

```go
// Redis-backed sessions (requires external dependency)
rdb := redis.NewClient(&redis.Options{Addr: "redis:6379"})
server.SetSessionManager(NewRedisSessionManager(rdb, 30*time.Minute))

// Completely custom implementation
server.SetSessionManager(myCustomManager)
```

## Session Behavior

When session management is enabled:

- Server generates a secure session ID on initialization
- Returns `MCP-Session-Id` header in the initialize response
- Clients must include `MCP-Session-Id` on all subsequent requests
- Server validates session existence and returns 404 if not found
- Clients can terminate sessions via DELETE requests when the session manager can revoke them (Redis); with `JWTSessionManager` a DELETE is answered 405, as the spec allows, since a JWT session only ends when it expires

Session management is **optional** and backwards compatible. Servers without session management enabled work exactly as before.

## Use Cases

- **Stateful workflows**: Maintain state across multiple tool calls
- **User tracking**: Track which user made which requests
- **Rate limiting**: Per-session rate limiting
- **Context preservation**: Keep context between requests
- **Audit logging**: Track session history

## Security Considerations

1. **Session expiration**: Configure appropriate TTL for your use case
2. **Transport security**: Always use HTTPS in production
3. **Key rotation**: Implement key rotation for signing keys
4. **Revocation**: Use custom SessionManager if revocation is required

## See Also

- Tool Providers Guide - Per-request context and authentication
- Protocol Support documentation for MCP 2025-11-25 specification
