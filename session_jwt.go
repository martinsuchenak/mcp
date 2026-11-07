package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SessionManager defines the interface for session storage and validation
// Implement this interface to create custom session stores (Redis, Database, etc.)
type SessionManager interface {
	// CreateSession creates a new session and returns its ID
	// showAll specifies whether this session shows all tools regardless of visibility
	CreateSession(ctx context.Context, protocolVersion string, showAll bool) (sessionID string, err error)

	// ValidateSession checks if a session exists and is valid
	// Returns true if valid, updates lastUsed timestamp if applicable
	ValidateSession(ctx context.Context, sessionID string) (valid bool, err error)

	// GetShowAll returns whether the session was created with show-all mode
	GetShowAll(ctx context.Context, sessionID string) (showAll bool, err error)

	// DeleteSession removes a session ahead of its natural expiry
	DeleteSession(ctx context.Context, sessionID string) error
}

// ErrSessionRevocationUnsupported is returned by a SessionManager whose
// sessions cannot be ended before they expire (JWTSessionManager). The HTTP
// transport answers a client's DELETE with 405 then, as the Streamable HTTP
// spec allows, rather than claiming a session is gone while it still works.
var ErrSessionRevocationUnsupported = errors.New("session revocation not supported")

// JWTSessionManager provides stateless session management using JWT tokens
// This is the RECOMMENDED approach for production clusters as it:
// - Requires no external storage (Redis, Database)
// - Scales horizontally without coordination
// - Works across all server instances
// - Has zero infrastructure dependencies
//
// Trade-off: Sessions cannot be revoked before expiry (acceptable for most use cases)
type JWTSessionManager struct {
	signingKey []byte
	ttl        time.Duration
}

type jwtClaims struct {
	Protocol  string `json:"protocol"`
	ShowAll   bool   `json:"show_all,omitempty"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	// ID (jti) makes every session id globally unique, as the spec asks:
	// without it two sessions created in the same second are the same token.
	ID string `json:"jti,omitempty"`
}

// NewJWTSessionManager creates a new JWT-based session manager
// signingKey should be a cryptographically secure random key (at least 32 bytes recommended)
// ttl is the session lifetime (e.g., 30 * time.Minute)
func NewJWTSessionManager(signingKey []byte, ttl time.Duration) *JWTSessionManager {
	return &JWTSessionManager{
		signingKey: signingKey,
		ttl:        ttl,
	}
}

// GenerateSigningKey creates a cryptographically secure random signing key
func GenerateSigningKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("failed to generate signing key: %w", err)
	}
	return key, nil
}

// NewJWTSessionManagerWithAutoKey creates a JWT session manager with an auto-generated signing key.
// This is convenient for development or single-instance deployments.
//
// For production clusters with multiple instances, use NewJWTSessionManager with a
// persisted key to ensure all instances can validate each other's sessions.
func NewJWTSessionManagerWithAutoKey(ttl time.Duration) (*JWTSessionManager, error) {
	key, err := GenerateSigningKey()
	if err != nil {
		return nil, err
	}
	return NewJWTSessionManager(key, ttl), nil
}

// CreateSession generates a new JWT session token
func (m *JWTSessionManager) CreateSession(ctx context.Context, protocolVersion string, showAll bool) (string, error) {
	now := time.Now()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("failed to generate session id: %w", err)
	}
	claims := jwtClaims{
		Protocol:  protocolVersion,
		ShowAll:   showAll,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(m.ttl).Unix(),
		ID:        base64.RawURLEncoding.EncodeToString(nonce),
	}

	// Create JWT: header.payload.signature
	header := map[string]string{
		"alg": "HS256",
		"typ": "JWT",
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("failed to marshal header: %w", err)
	}

	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("failed to marshal claims: %w", err)
	}

	headerEncoded := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsEncoded := base64.RawURLEncoding.EncodeToString(claimsJSON)

	// Sign with HMAC-SHA256
	message := headerEncoded + "." + claimsEncoded
	signature := m.sign(message)

	token := message + "." + signature
	return token, nil
}

// ValidateSession validates a JWT session token
func (m *JWTSessionManager) ValidateSession(ctx context.Context, sessionID string) (bool, error) {
	// Parse JWT
	parts := strings.Split(sessionID, ".")
	if len(parts) != 3 {
		return false, nil // Invalid format
	}

	headerEncoded := parts[0]
	claimsEncoded := parts[1]
	signature := parts[2]

	// Verify signature
	message := headerEncoded + "." + claimsEncoded
	expectedSignature := m.sign(message)
	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		return false, nil // Invalid signature
	}

	// Decode and validate claims
	claimsJSON, err := base64.RawURLEncoding.DecodeString(claimsEncoded)
	if err != nil {
		return false, nil // Invalid encoding
	}

	var claims jwtClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return false, nil // Invalid claims
	}

	// Check expiration
	if time.Now().Unix() > claims.ExpiresAt {
		return false, nil // Expired
	}

	return true, nil
}

// GetShowAll extracts the show-all flag from a JWT session token
func (m *JWTSessionManager) GetShowAll(ctx context.Context, sessionID string) (bool, error) {
	parts := strings.Split(sessionID, ".")
	if len(parts) != 3 {
		return false, fmt.Errorf("invalid token format")
	}

	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false, fmt.Errorf("failed to decode claims: %w", err)
	}

	var claims jwtClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return false, fmt.Errorf("failed to unmarshal claims: %w", err)
	}

	return claims.ShowAll, nil
}

// DeleteSession reports ErrSessionRevocationUnsupported: a JWT session
// cannot be revoked before it expires (the trade-off for being stateless),
// and saying so lets the HTTP transport answer DELETE with 405 instead of
// pretending the session ended.
func (m *JWTSessionManager) DeleteSession(ctx context.Context, sessionID string) error {
	return ErrSessionRevocationUnsupported
}

// sign creates HMAC-SHA256 signature
func (m *JWTSessionManager) sign(message string) string {
	h := hmac.New(sha256.New, m.signingKey)
	h.Write([]byte(message))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
