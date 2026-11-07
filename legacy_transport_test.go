package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Legacy-era Streamable HTTP transport conformance: batching for the
// revisions that allow it, the base protocol's id rules, session and
// version checks on every message, DELETE and GET semantics.

func legacyTestServer(t *testing.T, withSessions bool) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer("legacy", "1")
	s.RegisterTool(NewTool("echo", "echoes"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	if withSessions {
		sm, err := NewJWTSessionManagerWithAutoKey(time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		s.SetSessionManager(sm)
	}
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	t.Cleanup(ts.Close)
	return s, ts
}

func legacyPost(t *testing.T, url, version, session, body string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if version != "" {
		req.Header.Set(headerProtocolVersion, version)
	}
	if session != "" {
		req.Header.Set(headerSessionID, session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw
}

func legacyInitialize(t *testing.T, url, version string) string {
	t.Helper()
	resp, _ := legacyPost(t, url, "", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+version+`","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	return resp.Header.Get(headerSessionID)
}

// A 2025-03-26 client may POST a JSON-RPC batch; the responses come back as
// an array, and a batch of notifications alone is 202.
func TestLegacyBatch_SupportedRevisions(t *testing.T) {
	_, ts := legacyTestServer(t, false)
	for _, version := range []string{"2024-11-05", "2025-03-26"} {
		resp, raw := legacyPost(t, ts.URL, version, "", `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}]`)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d body %s", version, resp.StatusCode, raw)
		}
		var out []map[string]any
		if err := json.Unmarshal(raw, &out); err != nil || len(out) != 2 {
			t.Fatalf("%s: want an array of 2 responses, got %s (%v)", version, raw, err)
		}
		if out[0]["id"] != float64(1) || out[1]["id"] != float64(2) || out[1]["error"] != nil {
			t.Fatalf("%s: unexpected responses %s", version, raw)
		}
	}
	resp, raw := legacyPost(t, ts.URL, "2025-03-26", "", `[{"jsonrpc":"2.0","method":"notifications/initialized"}]`)
	if resp.StatusCode != http.StatusAccepted || len(raw) != 0 {
		t.Fatalf("notifications-only batch: status %d body %q, want 202 and no body", resp.StatusCode, raw)
	}
}

// 2025-06-18 removed batching: a batch is input the server cannot accept.
func TestLegacyBatch_RejectedFrom20250618(t *testing.T) {
	_, ts := legacyTestServer(t, false)
	for _, version := range []string{"2025-06-18", "2025-11-25"} {
		resp, raw := legacyPost(t, ts.URL, version, "", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), `"code":-32600`) {
			t.Fatalf("%s: status %d body %s, want 400 Invalid Request", version, resp.StatusCode, raw)
		}
	}
}

// Request ids: integers are echoed exactly (no float64 rounding), null is
// invalid, and a message with neither method nor id is an Invalid Request.
func TestLegacyRequestIDRules(t *testing.T) {
	_, ts := legacyTestServer(t, false)

	_, raw := legacyPost(t, ts.URL, "2025-11-25", "", `{"jsonrpc":"2.0","id":9007199254740993,"method":"ping"}`)
	if !bytes.Contains(raw, []byte(`"id":9007199254740993`)) {
		t.Fatalf("large integer id not preserved: %s", raw)
	}
	_, raw = legacyPost(t, ts.URL, "2025-11-25", "", `{"jsonrpc":"2.0","id":"abc","method":"ping"}`)
	if !bytes.Contains(raw, []byte(`"id":"abc"`)) {
		t.Fatalf("string id not preserved: %s", raw)
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`,
		`{"jsonrpc":"2.0"}`,
	} {
		_, raw := legacyPost(t, ts.URL, "2025-11-25", "", body)
		if !bytes.Contains(raw, []byte(`"code":-32600`)) {
			t.Errorf("%s: want -32600 Invalid Request, got %s", body, raw)
		}
	}
}

// Notifications and responses are checked for a valid session and version
// before being accepted: an expired session must be learnt from any message.
func TestLegacyNotification_SessionAndVersionChecked(t *testing.T) {
	_, ts := legacyTestServer(t, true)
	sid := legacyInitialize(t, ts.URL, "2025-11-25")
	if sid == "" {
		t.Fatal("no session id from initialize")
	}
	cases := []struct {
		name    string
		version string
		session string
		want    int
	}{
		{"valid", "2025-11-25", sid, http.StatusAccepted},
		{"unknown session", "2025-11-25", "bogus", http.StatusNotFound},
		{"missing session", "2025-11-25", "", http.StatusBadRequest},
		{"bad version", "1999-01-01", sid, http.StatusBadRequest},
	}
	for _, tc := range cases {
		resp, _ := legacyPost(t, ts.URL, tc.version, tc.session, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

// DELETE: 404 for an unknown session, 400 for a bad version, and 405 when
// the session manager cannot revoke (JWT), so the session is not reported
// gone while it still works.
func TestLegacyDelete_JWTSessions(t *testing.T) {
	_, ts := legacyTestServer(t, true)
	sid := legacyInitialize(t, ts.URL, "2025-11-25")
	del := func(version, session string) *http.Response {
		req, _ := http.NewRequest(http.MethodDelete, ts.URL, nil)
		if version != "" {
			req.Header.Set(headerProtocolVersion, version)
		}
		if session != "" {
			req.Header.Set(headerSessionID, session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := del("2025-11-25", "bogus"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown session: status %d, want 404", resp.StatusCode)
	}
	if resp := del("1999-01-01", sid); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad version: status %d, want 400", resp.StatusCode)
	}
	if resp := del("2025-11-25", sid); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("JWT delete: status %d, want 405", resp.StatusCode)
	}
	if resp, _ := legacyPost(t, ts.URL, "2025-11-25", sid, `{"jsonrpc":"2.0","id":1,"method":"ping"}`); resp.StatusCode != http.StatusOK {
		t.Errorf("session after 405 DELETE: status %d, want 200", resp.StatusCode)
	}
}

// Two initializes in the same second get different JWT session ids.
func TestJWTSessionIDsUnique(t *testing.T) {
	sm := NewJWTSessionManager([]byte("a-32-byte-or-longer-signing-key!"), time.Hour)
	a, _ := sm.CreateSession(context.Background(), "2025-11-25", false)
	b, _ := sm.CreateSession(context.Background(), "2025-11-25", false)
	if a == b {
		t.Fatal("two sessions share one id")
	}
}

// GET: an Accept that names text/event-stream with q=0 is not consent; the
// 405 lists every method the endpoint serves.
func TestLegacyGet_AcceptAndAllow(t *testing.T) {
	_, ts := legacyTestServer(t, false)
	req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
	req.Header.Set("Accept", "text/event-stream;q=0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "GET") || !strings.Contains(allow, "DELETE") {
		t.Fatalf("Allow = %q", allow)
	}
}

// One standalone GET stream per session: a newer stream supersedes the
// older one, so a notification is never broadcast to both.
func TestLegacyGet_OneStreamPerSession(t *testing.T) {
	s, ts := legacyTestServer(t, true)
	sid := legacyInitialize(t, ts.URL, "2025-11-25")
	open := func() *http.Response {
		req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set(headerProtocolVersion, "2025-11-25")
		req.Header.Set(headerSessionID, sid)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET status %d", resp.StatusCode)
		}
		return resp
	}
	first := open()
	defer first.Body.Close()
	second := open()
	defer second.Body.Close()
	// Wait until the second stream has replaced the first.
	deadline := time.Now().Add(2 * time.Second)
	for s.notifications.count() != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := s.notifications.count(); n != 1 {
		t.Fatalf("subscribers = %d, want 1 (the newest stream)", n)
	}
	s.NotifyToolsChanged()

	// The first stream was closed by the server without an event.
	firstDone := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(first.Body); firstDone <- b }()
	select {
	case b := <-firstDone:
		if bytes.Contains(b, []byte("list_changed")) {
			t.Fatalf("superseded stream received a notification: %s", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("superseded stream was not closed")
	}
	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 512)
		n, _ := second.Body.Read(buf)
		got <- buf[:n]
	}()
	select {
	case b := <-got:
		if !bytes.Contains(b, []byte(`"method":"notifications/tools/list_changed"`)) {
			t.Fatalf("newest stream did not get the notification: %q", b)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("newest stream did not get the notification")
	}
}

// 2024-11-05 clients are told about resources and prompts too (as empty
// capability objects), since a client may only use advertised capabilities.
func TestLegacyInitialize_20241105Capabilities(t *testing.T) {
	_, ts := legacyTestServer(t, false)
	_, raw := legacyPost(t, ts.URL, "", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	var out struct {
		Result struct {
			Capabilities map[string]any `json:"capabilities"`
		} `json:"result"`
	}
	_ = json.Unmarshal(raw, &out)
	for _, key := range []string{"tools", "resources", "prompts"} {
		if _, ok := out.Result.Capabilities[key]; !ok {
			t.Errorf("capabilities missing %q: %s", key, raw)
		}
	}
}
