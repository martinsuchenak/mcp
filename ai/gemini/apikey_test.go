package gemini

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/paularlott/mcp/ai/openai"
)

const secretKey = "GEMINI-SECRET-KEY-123"

// The API key travels in the x-goog-api-key header, never the URL: request
// errors quote the URL, and proxies and access logs record it, so a key there
// reached anyone who could see an error (CWE-209).
func TestAPIKeySentInHeaderNotURL(t *testing.T) {
	var gotPaths, gotQueries, gotKeys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.EscapedPath())
		gotQueries = append(gotQueries, r.URL.RawQuery)
		gotKeys = append(gotKeys, r.Header.Get("x-goog-api-key"))
		if strings.HasSuffix(r.URL.Path, ":embedContent") {
			w.Write([]byte(`{"embedding":{"values":[0.5]}}`))
			return
		}
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	c, err := New(openai.Config{APIKey: secretKey, BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"text-embedding-004", "models/text-embedding-004", "a/../b?x=1"} {
		if _, err := c.CreateEmbedding(context.Background(), openai.EmbeddingRequest{Model: model, Input: "hi"}); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
	}

	wantPaths := []string{
		"/models",
		"/models/text-embedding-004:embedContent",
		"/models/text-embedding-004:embedContent",
		"/models/a%2F..%2Fb%3Fx=1:embedContent", // one escaped path segment
	}
	for i := range wantPaths {
		if gotPaths[i] != wantPaths[i] {
			t.Errorf("request %d path = %q, want %q", i, gotPaths[i], wantPaths[i])
		}
		if gotQueries[i] != "" {
			t.Errorf("request %d query = %q, want none", i, gotQueries[i])
		}
		if gotKeys[i] != secretKey {
			t.Errorf("request %d x-goog-api-key = %q", i, gotKeys[i])
		}
	}
}

// Neither a failed connection nor a rejected request puts the key in the
// error, even when the server echoes the request back in its error body.
func TestAPIKeyNotInErrors(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"Echo: %s"}`, r.URL.RequestURI())
	}))
	defer echo.Close()

	for name, base := range map[string]string{
		"connection refused": "http://127.0.0.1:1/",
		"rejected with echo": echo.URL + "/",
	} {
		c, err := New(openai.Config{APIKey: secretKey, BaseURL: base, MaxRetries: -1})
		if err != nil {
			t.Fatal(err)
		}
		_, modelsErr := c.GetModels(context.Background())
		_, embedErr := c.CreateEmbedding(context.Background(), openai.EmbeddingRequest{Model: "m", Input: "hi"})
		for call, err := range map[string]error{"models": modelsErr, "embedding": embedErr} {
			if err == nil {
				t.Fatalf("%s %s: expected an error", name, call)
			}
			if strings.Contains(err.Error(), secretKey) {
				t.Errorf("%s %s: error leaks the API key: %v", name, call, err)
			}
		}
	}
}
