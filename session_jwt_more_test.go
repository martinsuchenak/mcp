package mcp

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestJWTSessionManager_GetProtocolVersion covers the happy path plus the
// malformed-token error paths, none of which any other test reaches directly.

// TestJWTSessionManager_GetShowAll_ErrorPaths covers GetShowAll's malformed
// token branches (ValidateSession's equivalents are covered by
// TestJWTSessionManager_ValidateSession, but GetShowAll has its own decode
// logic).
func TestJWTSessionManager_GetShowAll_ErrorPaths(t *testing.T) {
	sm := NewJWTSessionManager([]byte("a-32-byte-or-longer-signing-key!"), time.Hour)

	if _, err := sm.GetShowAll(context.Background(), "not-a-jwt"); err == nil {
		t.Error("expected error for malformed token (wrong segment count)")
	}
	if _, err := sm.GetShowAll(context.Background(), "header.not-valid-base64!!!.sig"); err == nil {
		t.Error("expected error for invalid base64 claims segment")
	}

	// Valid session with show-all true.
	sessionID, err := sm.CreateSession(context.Background(), "2025-06-18", true)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	showAll, err := sm.GetShowAll(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("GetShowAll: %v", err)
	}
	if !showAll {
		t.Error("expected showAll to be true")
	}
}

// TestJWTSessionManager_ValidateSession_ErrorPaths covers ValidateSession's
// malformed-token, bad-signature, invalid-base64, invalid-JSON, and expired
// branches, most of which no other test reaches (existing tests only check
// the happy path indirectly via HTTP).
func TestJWTSessionManager_ValidateSession_ErrorPaths(t *testing.T) {
	sm := NewJWTSessionManager([]byte("a-32-byte-or-longer-signing-key!"), time.Hour)

	cases := []struct {
		name  string
		token string
	}{
		{"wrong segment count", "abc.def"},
		{"too many segments", "a.b.c.d"},
		{"invalid base64 claims", "aGVhZGVy.not-valid-base64!!!.c2ln"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			valid, err := sm.ValidateSession(context.Background(), tc.token)
			if err != nil {
				t.Fatalf("ValidateSession should not itself error, got %v", err)
			}
			if valid {
				t.Errorf("token %q should be invalid", tc.token)
			}
		})
	}

	// Tampered signature.
	sessionID, err := sm.CreateSession(context.Background(), "2025-06-18", false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	parts := strings.Split(sessionID, ".")
	tampered := parts[0] + "." + parts[1] + ".tampered-signature"
	if valid, _ := sm.ValidateSession(context.Background(), tampered); valid {
		t.Error("tampered signature should be invalid")
	}

	// Invalid JSON in the claims segment (valid base64, but not valid JSON).
	badClaims := base64.RawURLEncoding.EncodeToString([]byte("not-json"))
	badToken := parts[0] + "." + badClaims + "." + sm.sign(parts[0]+"."+badClaims)
	if valid, _ := sm.ValidateSession(context.Background(), badToken); valid {
		t.Error("invalid JSON claims should be invalid")
	}

	// Expired token.
	expiredSM := NewJWTSessionManager([]byte("a-32-byte-or-longer-signing-key!"), -1*time.Hour)
	expiredID, err := expiredSM.CreateSession(context.Background(), "2025-06-18", false)
	if err != nil {
		t.Fatalf("CreateSession (expired): %v", err)
	}
	if valid, _ := expiredSM.ValidateSession(context.Background(), expiredID); valid {
		t.Error("expired token should be invalid")
	}
}

// TestJWTSessionManager_DeleteSession_NoOp covers DeleteSession, a
// deliberate no-op for stateless JWT sessions (tokens can't be revoked before
// expiry). This documents the contract: the session remains valid afterward.
func TestJWTSessionManager_DeleteSession_NoOp(t *testing.T) {
	sm := NewJWTSessionManager([]byte("a-32-byte-or-longer-signing-key!"), time.Hour)
	sessionID, err := sm.CreateSession(context.Background(), "2025-06-18", false)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// JWT sessions cannot be revoked, and DeleteSession says so (the HTTP
	// transport turns this into a 405 for the client's DELETE).
	if err := sm.DeleteSession(context.Background(), sessionID); !errors.Is(err, ErrSessionRevocationUnsupported) {
		t.Fatalf("DeleteSession: %v, want ErrSessionRevocationUnsupported", err)
	}

	// Still valid after the attempted deletion.
	valid, err := sm.ValidateSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ValidateSession after delete: %v", err)
	}
	if !valid {
		t.Error("JWT session should remain valid after DeleteSession (stateless, cannot revoke)")
	}
}
