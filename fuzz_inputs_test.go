package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Fuzz targets for every decoder that faces untrusted bytes. They assert
// only that nothing panics and that outputs stay well-formed.

func FuzzHTTPRequestBody(f *testing.F) {
	for _, seed := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"msg":5}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		`{bad`, `42`, `"str"`, ``, `{"jsonrpc":"2.0"} xyz`,
	} {
		f.Add(seed, "2025-11-25", "")
	}
	s := NewServer("fuzz", "1")
	s.RegisterTool(NewTool("echo", "e", String("msg", "m")), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText(req.StringOr("msg", "")), nil
	})
	f.Fuzz(func(t *testing.T, body, version, mcpMethod string) {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if version != "" {
			req.Header.Set(headerProtocolVersion, version)
		}
		if mcpMethod != "" {
			req.Header.Set(headerMcpMethod, mcpMethod)
		}
		rr := httptest.NewRecorder()
		s.HandleRequest(rr, req)
		if ct := rr.Header().Get("Content-Type"); strings.HasPrefix(ct, "application/json") && rr.Body.Len() > 0 && !json.Valid(rr.Body.Bytes()) {
			t.Fatalf("invalid JSON response: %s", rr.Body.String())
		}
	})
}

func FuzzStdioLine(f *testing.F) {
	for _, seed := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":null,"method":"ping"}]`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`,
		`{"jsonrpc":"2.0","id":"s","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`,
		`{not json`, `[`, `{"id":1}`, `{"jsonrpc":"2.0","id":1,"result":{}}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		if strings.ContainsAny(line, "\n") {
			return
		}
		s := NewServer("fuzz", "1")
		s.RegisterTool(NewTool("echo", "e"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return NewToolResponseText("ok"), nil
		})
		in := strings.NewReader(line + "\n")
		var out strings.Builder
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { s.Shutdown() }() // ends any listen the line may open
		if err := s.ServeStream(ctx, in, &out); err != nil {
			t.Fatalf("ServeStream: %v", err)
		}
		for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			if l != "" && !json.Valid([]byte(l)) {
				t.Fatalf("invalid JSON line on stdout: %q", l)
			}
		}
	})
}

func FuzzJWT(f *testing.F) {
	f.Add("eyJhbGciOiJSUzI1NiIsImtpZCI6IngifQ.e30.AAAA")
	f.Add("a.b.c")
	f.Add("")
	v, _ := NewJWTValidator(JWTValidatorConfig{Issuer: "https://idp.example", JWKSURL: "https://idp.example/keys", HTTPClient: &http.Client{Transport: fuzzRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}, nil
	})}})
	f.Fuzz(func(t *testing.T, token string) {
		_, _ = v.ValidateToken(context.Background(), token)
	})
}

func FuzzJWK(f *testing.F) {
	f.Add(`{"kty":"RSA","kid":"a","n":"AQAB","e":"AQAB"}`)
	f.Add(`{"kty":"EC","crv":"P-256","x":"AA","y":"AA"}`)
	f.Add(`{"kty":"OKP","crv":"Ed25519","x":"AA"}`)
	f.Fuzz(func(t *testing.T, raw string) {
		_, _, _ = parseJWK(json.RawMessage(raw))
	})
}

func FuzzBearerChallenge(f *testing.F) {
	f.Add(`Bearer resource_metadata="https://h/.well-known/oauth-protected-resource", scope="a b"`)
	f.Add(`Basic realm="x", Bearer error=invalid_token`)
	f.Fuzz(func(t *testing.T, header string) {
		_ = bearerChallenges([]string{header})
		_, _, _ = bearerToken(header)
	})
}

func FuzzSchema(f *testing.F) {
	f.Add(`{"type":"object","properties":{"a":{"type":"string","pattern":"^a+$"}},"required":["a"],"additionalProperties":false}`, `{"a":"aa"}`)
	f.Add(`{"anyOf":[{"type":"string"},{"$ref":"#/$defs/x"}],"$defs":{"x":{"$ref":"#/$defs/x"}}}`, `1`)
	f.Fuzz(func(t *testing.T, schema, value string) {
		var s, v any
		if json.Unmarshal([]byte(schema), &s) != nil || json.Unmarshal([]byte(value), &v) != nil {
			return
		}
		_ = validateJSONSchema(s, v)
	})
}

type fuzzRoundTrip func(*http.Request) (*http.Response, error)

func (f fuzzRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
