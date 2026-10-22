package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/paularlott/mcp/ai/openai"
	"github.com/paularlott/mcp/pool"
)

// -----------------------------------------------------------------------------
// Provider / SupportsCapability
// -----------------------------------------------------------------------------

func TestProvider(t *testing.T) {
	c := &Client{provider: providerName}
	if got := c.Provider(); got != "gemini" {
		t.Errorf("Provider() = %q, want %q", got, "gemini")
	}
}

func TestSupportsCapability(t *testing.T) {
	c := &Client{provider: providerName}
	tests := []struct {
		cap  string
		want bool
	}{
		{"responses", false},
		{"responses_emulated", true},
		{"embeddings", true},
		{"decision", false},
		{"streaming", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.cap, func(t *testing.T) {
			if got := c.SupportsCapability(tt.cap); got != tt.want {
				t.Errorf("SupportsCapability(%q) = %v, want %v", tt.cap, got, tt.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Close
// -----------------------------------------------------------------------------

func TestClose(t *testing.T) {
	c := &Client{}
	if err := c.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

// -----------------------------------------------------------------------------
// New: explicit retry-flag overrides
// -----------------------------------------------------------------------------

func TestNew_RetryFlagOverrides(t *testing.T) {
	f := false
	c, err := New(openai.Config{
		BaseURL:            "http://localhost/",
		RetryOnRateLimit:   &f,
		RetryOnServerError: &f,
	})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	if c.retryOnRateLimit {
		t.Error("retryOnRateLimit = true, want false (explicit override ignored)")
	}
	if c.retryOnServerError {
		t.Error("retryOnServerError = true, want false (explicit override ignored)")
	}
}

// -----------------------------------------------------------------------------
// GetModels
// -----------------------------------------------------------------------------

func TestGetModels_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("x-goog-api-key = %q, want test-key", got)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("query = %q, want none (the key must not be in the URL)", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"models":[
			{"name":"models/gemini-pro","inputTokenLimit":32000},
			{"name":"models/gemini-flash","inputTokenLimit":1000000}
		]}`))
	}))
	defer srv.Close()

	c, err := New(openai.Config{APIKey: "test-key", BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	resp, err := c.GetModels(context.Background())
	if err != nil {
		t.Fatalf("GetModels() error: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("Object = %q, want list", resp.Object)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("len(Data) = %d, want 2", len(resp.Data))
	}
	if resp.Data[0].ID != "gemini-pro" {
		t.Errorf("Data[0].ID = %q, want gemini-pro (models/ prefix should be trimmed)", resp.Data[0].ID)
	}
	if resp.Data[0].ContextWindow != 32000 {
		t.Errorf("Data[0].ContextWindow = %d, want 32000", resp.Data[0].ContextWindow)
	}
	if resp.Data[1].ID != "gemini-flash" || resp.Data[1].ContextWindow != 1000000 {
		t.Errorf("Data[1] = %+v", resp.Data[1])
	}
}

func TestGetModels_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	c, err := New(openai.Config{BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	resp, err := c.GetModels(context.Background())
	if err != nil {
		t.Fatalf("GetModels() error: %v", err)
	}
	if len(resp.Data) != 0 {
		t.Errorf("len(Data) = %d, want 0", len(resp.Data))
	}
}

func TestGetModels_ErrorPropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`server exploded`))
	}))
	defer srv.Close()

	c, err := New(openai.Config{BaseURL: srv.URL + "/", MaxRetries: -1})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	_, err = c.GetModels(context.Background())
	if err == nil {
		t.Fatal("expected error from GetModels")
	}
	if !strings.Contains(err.Error(), "gemini API error") {
		t.Errorf("error = %v, want it to mention gemini API error", err)
	}
}

// -----------------------------------------------------------------------------
// CreateEmbedding
// -----------------------------------------------------------------------------

func TestCreateEmbedding_StringInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "embedContent") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["taskType"] != "SEMANTIC_SIMILARITY" {
			t.Errorf("taskType = %v", body["taskType"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"embedding":{"values":[0.1,0.2,0.3]}}`))
	}))
	defer srv.Close()

	c, err := New(openai.Config{BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	resp, err := c.CreateEmbedding(context.Background(), openai.EmbeddingRequest{
		Model: "embed-model",
		Input: "hello world",
	})
	if err != nil {
		t.Fatalf("CreateEmbedding() error: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("Object = %q, want list", resp.Object)
	}
	if resp.Model != "embed-model" {
		t.Errorf("Model = %q", resp.Model)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("len(Data) = %d, want 1", len(resp.Data))
	}
	if len(resp.Data[0].Embedding) != 3 {
		t.Errorf("Embedding = %v", resp.Data[0].Embedding)
	}
	if resp.Data[0].Index != 0 {
		t.Errorf("Index = %d, want 0", resp.Data[0].Index)
	}
	if resp.Usage.PromptTokens != 1 || resp.Usage.TotalTokens != 1 {
		t.Errorf("Usage = %+v, want 1/1", resp.Usage)
	}
}

func TestCreateEmbedding_StringSliceInput(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"embedding":{"values":[1,2]}}`))
	}))
	defer srv.Close()

	c, err := New(openai.Config{BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	resp, err := c.CreateEmbedding(context.Background(), openai.EmbeddingRequest{
		Model: "embed-model",
		Input: []string{"a", "b", "c"},
	})
	if err != nil {
		t.Fatalf("CreateEmbedding() error: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (one embedContent request per input)", calls)
	}
	if len(resp.Data) != 3 {
		t.Fatalf("len(Data) = %d, want 3", len(resp.Data))
	}
	for i, e := range resp.Data {
		if e.Index != i {
			t.Errorf("Data[%d].Index = %d, want %d", i, e.Index, i)
		}
	}
}

func TestCreateEmbedding_AnySliceInput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"embedding":{"values":[1]}}`))
	}))
	defer srv.Close()

	c, err := New(openai.Config{BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	resp, err := c.CreateEmbedding(context.Background(), openai.EmbeddingRequest{
		Model: "embed-model",
		Input: []any{"x", "y"},
	})
	if err != nil {
		t.Fatalf("CreateEmbedding() error: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("len(Data) = %d, want 2", len(resp.Data))
	}
}

func TestCreateEmbedding_AnySliceInvalidElement(t *testing.T) {
	c := &Client{}
	_, err := c.CreateEmbedding(context.Background(), openai.EmbeddingRequest{
		Model: "embed-model",
		Input: []any{"ok", 42},
	})
	if err == nil {
		t.Fatal("expected error for non-string element in []any input")
	}
}

func TestCreateEmbedding_InvalidInputType(t *testing.T) {
	c := &Client{}
	_, err := c.CreateEmbedding(context.Background(), openai.EmbeddingRequest{
		Model: "embed-model",
		Input: 12345,
	})
	if err == nil {
		t.Fatal("expected error for invalid input type")
	}
	if !strings.Contains(err.Error(), "invalid input type") {
		t.Errorf("error = %v", err)
	}
}

func TestCreateEmbedding_DimensionsForwarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		dim, ok := body["outputDimensionality"]
		if !ok {
			t.Fatal("expected outputDimensionality to be set")
		}
		if int(dim.(float64)) != 256 {
			t.Errorf("outputDimensionality = %v, want 256", dim)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"embedding":{"values":[1]}}`))
	}))
	defer srv.Close()

	c, err := New(openai.Config{BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	_, err = c.CreateEmbedding(context.Background(), openai.EmbeddingRequest{
		Model:      "embed-model",
		Input:      "hi",
		Dimensions: 256,
	})
	if err != nil {
		t.Fatalf("CreateEmbedding() error: %v", err)
	}
}

func TestCreateEmbedding_DoRequestErrorPropagated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`bad request`))
	}))
	defer srv.Close()

	c, err := New(openai.Config{BaseURL: srv.URL + "/", MaxRetries: -1})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}
	_, err = c.CreateEmbedding(context.Background(), openai.EmbeddingRequest{
		Model: "embed-model",
		Input: "hi",
	})
	if err == nil {
		t.Fatal("expected error from CreateEmbedding")
	}
}

// -----------------------------------------------------------------------------
// parseRetryAfter (additional branches: HTTP-date, unparseable)
// -----------------------------------------------------------------------------

func TestParseRetryAfter_Empty(t *testing.T) {
	if got := parseRetryAfter(""); got != 0 {
		t.Errorf("parseRetryAfter(\"\") = %v, want 0", got)
	}
}

func TestParseRetryAfter_FutureHTTPDate(t *testing.T) {
	future := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
	got := parseRetryAfter(future)
	if got <= 0 || got > 6*time.Second {
		t.Errorf("parseRetryAfter(%q) = %v, want ~5s", future, got)
	}
}

func TestParseRetryAfter_PastHTTPDate(t *testing.T) {
	past := time.Now().Add(-5 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(past); got != 0 {
		t.Errorf("parseRetryAfter(%q) = %v, want 0 (past date should not wait)", past, got)
	}
}

func TestParseRetryAfter_Unparseable(t *testing.T) {
	if got := parseRetryAfter("not-a-valid-header"); got != 0 {
		t.Errorf("parseRetryAfter(garbage) = %v, want 0", got)
	}
}

func TestParseRetryAfter_ZeroSeconds(t *testing.T) {
	if got := parseRetryAfter("0"); got != 0 {
		t.Errorf("parseRetryAfter(\"0\") = %v, want 0", got)
	}
}

func TestParseRetryAfter_NegativeSeconds(t *testing.T) {
	if got := parseRetryAfter("-5"); got != 0 {
		t.Errorf("parseRetryAfter(\"-5\") = %v, want 0", got)
	}
}

// -----------------------------------------------------------------------------
// doRequest additional branches
// -----------------------------------------------------------------------------

// stubPool is a minimal pool.HTTPPool used to exercise the c.httpPool != nil branch.
type stubPool struct {
	client *http.Client
}

func (s *stubPool) GetHTTPClient() *http.Client { return s.client }

func TestDoRequest_UsesConfiguredHTTPPool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	c := &Client{
		baseURL:  srv.URL + "/",
		httpPool: &stubPool{client: &http.Client{}},
	}

	var result struct {
		Models []struct{ Name string } `json:"models"`
	}
	if err := c.doRequest(context.Background(), "GET", "models", nil, &result); err != nil {
		t.Fatalf("doRequest() error: %v", err)
	}
}

func TestDoRequest_FallsBackToDefaultPool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"models":[]}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL + "/"}
	if err := c.doRequest(context.Background(), "GET", "models", nil, nil); err != nil {
		t.Fatalf("doRequest() error: %v", err)
	}
	// Sanity: the global pool is reachable and returns a usable client.
	if pool.GetPool().GetHTTPClient() == nil {
		t.Fatal("default pool returned nil client")
	}
}

func TestDoRequest_MarshalError(t *testing.T) {
	c := &Client{baseURL: "http://example.invalid/"}
	// A channel cannot be marshalled to JSON — exercises the marshal-error branch
	// before any network I/O happens.
	err := c.doRequest(context.Background(), "POST", "x", make(chan int), nil)
	if err == nil {
		t.Fatal("expected marshal error")
	}
	if !strings.Contains(err.Error(), "failed to marshal request") {
		t.Errorf("error = %v", err)
	}
}

func TestDoRequest_RequestCreationError(t *testing.T) {
	c := &Client{baseURL: "http://example.invalid/"}
	// A control character in the method name makes http.NewRequestWithContext fail.
	err := c.doRequest(context.Background(), "G\nET", "x", nil, nil)
	if err == nil {
		t.Fatal("expected request creation error")
	}
	if !strings.Contains(err.Error(), "failed to create request") {
		t.Errorf("error = %v", err)
	}
}

func TestDoRequest_TransportError(t *testing.T) {
	// No server is listening on this port; the HTTP client's Do should fail.
	c := &Client{baseURL: "http://127.0.0.1:1/"}
	err := c.doRequest(context.Background(), "GET", "x", nil, nil)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "request failed") {
		t.Errorf("error = %v", err)
	}
}

func TestDoRequest_DecodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`not valid json`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL + "/"}
	var result map[string]any
	err := c.doRequest(context.Background(), "GET", "x", nil, &result)
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "failed to decode response") {
		t.Errorf("error = %v", err)
	}
}

// errorReadCloser fails every Read, simulating a body that errors mid-read
// (e.g. a truncated connection) — exercises doRequest's io.ReadAll error path.
type errorReadCloser struct{}

func (errorReadCloser) Read(p []byte) (int, error) { return 0, errReadFailure }
func (errorReadCloser) Close() error               { return nil }

var errReadFailure = errReadFailureType{}

type errReadFailureType struct{}

func (errReadFailureType) Error() string { return "simulated read failure" }

type failingBodyTransport struct{}

func (failingBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       errorReadCloser{},
		Header:     make(http.Header),
		Request:    r,
	}, nil
}

func TestDoRequest_ReadBodyError(t *testing.T) {
	c := &Client{
		baseURL:  "http://example.invalid/",
		httpPool: &stubPool{client: &http.Client{Transport: failingBodyTransport{}}},
	}
	err := c.doRequest(context.Background(), "GET", "x", nil, nil)
	if err == nil {
		t.Fatal("expected error reading response body")
	}
	if !strings.Contains(err.Error(), "failed to read response") {
		t.Errorf("error = %v", err)
	}
}

func TestDoRequest_NilResultOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"anything":true}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL + "/"}
	if err := c.doRequest(context.Background(), "GET", "x", nil, nil); err != nil {
		t.Fatalf("doRequest() error: %v", err)
	}
}

// -----------------------------------------------------------------------------
// StreamChatCompletion (delegates to the embedded openai chat client)
// -----------------------------------------------------------------------------

func sseChunk(data string) string {
	return "data: " + data + "\n\n"
}

func TestStreamChatCompletion_Delegates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/chat/completions" {
			t.Errorf("unexpected path %q, want /openai/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("server does not support flushing")
		}
		chunk := openai.ChatCompletionResponse{
			ID:     "stream-1",
			Object: "chat.completion.chunk",
			Model:  "gemini-test",
			Choices: []openai.Choice{{
				Index: 0,
				Delta: openai.Delta{Content: "hello"},
			}},
		}
		data, _ := json.Marshal(chunk)
		w.Write([]byte(sseChunk(string(data))))
		flusher.Flush()
		w.Write([]byte(sseChunk("[DONE]")))
		flusher.Flush()
	}))
	defer srv.Close()

	c, err := New(openai.Config{BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	stream := c.StreamChatCompletion(context.Background(), openai.ChatCompletionRequest{
		Model:    "gemini-test",
		Messages: []openai.Message{{Role: "user", Content: "hi"}},
	})

	var got []openai.ChatCompletionResponse
	for stream.Next() {
		got = append(got, stream.Current())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(chunks) = %d, want 1", len(got))
	}
	if got[0].Choices[0].Delta.Content != "hello" {
		t.Errorf("content = %q, want hello", got[0].Choices[0].Delta.Content)
	}
}

func TestStreamChatCompletion_ErrorDelegates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	c, err := New(openai.Config{BaseURL: srv.URL + "/", MaxRetries: -1})
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	stream := c.StreamChatCompletion(context.Background(), openai.ChatCompletionRequest{
		Model:    "gemini-test",
		Messages: []openai.Message{{Role: "user", Content: "hi"}},
	})
	for stream.Next() {
	}
	if stream.Err() == nil {
		t.Fatal("expected stream error to propagate from the delegated openai client")
	}
}

// -----------------------------------------------------------------------------
// Responses-API emulation: StreamResponse / CreateResponse / GetResponse /
// CancelResponse / DeleteResponse / CompactResponse.
//
// Gemini does not have a native Responses API, so SupportsCapability reports
// "responses_emulated" rather than "responses" — client.go emulates the full
// lifecycle on top of the chat completions endpoint via openai's *Emulated
// helpers. We exercise the real emulation path end-to-end against an httptest
// chat/completions server.
// -----------------------------------------------------------------------------

func newTestGeminiClient(t *testing.T, handler http.HandlerFunc) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c, err := New(openai.Config{BaseURL: srv.URL + "/"})
	if err != nil {
		srv.Close()
		t.Fatalf("New() error: %v", err)
	}
	// Give each test its own response manager so state from other tests
	// (which share the openai global singleton via GetManager()) can't leak in.
	c.responseManager = openai.NewResponseManager()
	return c, srv.Close
}

func chatCompletionHandler(t *testing.T, reply string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := openai.ChatCompletionResponse{
			ID:     "chat-1",
			Object: "chat.completion",
			Model:  "gemini-test",
			Choices: []openai.Choice{{
				Index:        0,
				Message:      openai.Message{Role: "assistant", Content: reply},
				FinishReason: "stop",
			}},
		}
		data, _ := json.Marshal(resp)
		w.Write(data)
	}
}

func TestCreateResponse_SyncEmulation(t *testing.T) {
	c, closeSrv := newTestGeminiClient(t, chatCompletionHandler(t, "hello there"))
	defer closeSrv()

	resp, err := c.CreateResponse(context.Background(), openai.CreateResponseRequest{
		Model: "gemini-test",
		Input: []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateResponse() error: %v", err)
	}
	if resp.Status != "completed" {
		t.Errorf("Status = %q, want completed", resp.Status)
	}
	if resp.OutputText() != "hello there" {
		t.Errorf("OutputText() = %q, want %q", resp.OutputText(), "hello there")
	}
}

func TestCreateResponse_BackgroundThenGetResponse(t *testing.T) {
	c, closeSrv := newTestGeminiClient(t, chatCompletionHandler(t, "async reply"))
	defer closeSrv()

	created, err := c.CreateResponse(context.Background(), openai.CreateResponseRequest{
		Model:      "gemini-test",
		Background: true,
		Input:      []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateResponse() error: %v", err)
	}
	if created.Status != "in_progress" {
		t.Errorf("initial Status = %q, want in_progress", created.Status)
	}

	got, err := c.GetResponse(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetResponse() error: %v", err)
	}
	if got.Status != "completed" {
		t.Errorf("final Status = %q, want completed", got.Status)
	}
	if got.OutputText() != "async reply" {
		t.Errorf("OutputText() = %q, want %q", got.OutputText(), "async reply")
	}
}

func TestGetResponse_NotFound(t *testing.T) {
	c := &Client{responseManager: openai.NewResponseManager()}
	_, err := c.GetResponse(context.Background(), "resp_missing")
	if err == nil {
		t.Fatal("expected error for missing response")
	}
}

func TestCancelResponse(t *testing.T) {
	c, closeSrv := newTestGeminiClient(t, chatCompletionHandler(t, "irrelevant"))
	defer closeSrv()

	created, err := c.CreateResponse(context.Background(), openai.CreateResponseRequest{
		Model:      "gemini-test",
		Background: true,
		Input:      []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateResponse() error: %v", err)
	}

	cancelled, err := c.CancelResponse(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("CancelResponse() error: %v", err)
	}
	if cancelled.Status != "cancelled" && cancelled.Status != "completed" {
		// The background goroutine may race the cancel and complete first;
		// either terminal state is acceptable, but the call itself must succeed.
		t.Errorf("Status = %q, want cancelled or completed", cancelled.Status)
	}
}

func TestCancelResponse_NotFound(t *testing.T) {
	c := &Client{responseManager: openai.NewResponseManager()}
	_, err := c.CancelResponse(context.Background(), "resp_missing")
	if err == nil {
		t.Fatal("expected error for missing response")
	}
}

func TestDeleteResponse(t *testing.T) {
	c, closeSrv := newTestGeminiClient(t, chatCompletionHandler(t, "to be deleted"))
	defer closeSrv()

	created, err := c.CreateResponse(context.Background(), openai.CreateResponseRequest{
		Model: "gemini-test",
		Input: []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateResponse() error: %v", err)
	}

	if err := c.DeleteResponse(context.Background(), created.ID); err != nil {
		t.Fatalf("DeleteResponse() error: %v", err)
	}

	if _, err := c.GetResponse(context.Background(), created.ID); err == nil {
		t.Error("expected GetResponse to fail after delete")
	}
}

func TestDeleteResponse_NotFound(t *testing.T) {
	c := &Client{responseManager: openai.NewResponseManager()}
	if err := c.DeleteResponse(context.Background(), "resp_missing"); err == nil {
		t.Fatal("expected error for missing response")
	}
}

func TestCompactResponse(t *testing.T) {
	c, closeSrv := newTestGeminiClient(t, chatCompletionHandler(t, "the summary"))
	defer closeSrv()

	created, err := c.CreateResponse(context.Background(), openai.CreateResponseRequest{
		Model: "gemini-test",
		Input: []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	})
	if err != nil {
		t.Fatalf("CreateResponse() error: %v", err)
	}

	compacted, err := c.CompactResponse(context.Background(), openai.CompactResponseRequest{Model: "gemini-test", PreviousResponseID: created.ID})
	if err != nil {
		t.Fatalf("CompactResponse() error: %v", err)
	}
	if compacted.Object != "response.compaction" || len(compacted.Output) != 1 {
		t.Errorf("compacted = %+v", compacted)
	}
}

func TestCompactResponse_NotFound(t *testing.T) {
	c := &Client{responseManager: openai.NewResponseManager()}
	if _, err := c.CompactResponse(context.Background(), openai.CompactResponseRequest{Model: "m", PreviousResponseID: "resp_missing"}); err == nil {
		t.Fatal("expected error for missing response")
	}
}

func TestStreamResponse_Emulated(t *testing.T) {
	c, closeSrv := newTestGeminiClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("server does not support flushing")
		}
		chunk := openai.ChatCompletionResponse{
			ID:     "stream-1",
			Object: "chat.completion.chunk",
			Model:  "gemini-test",
			Choices: []openai.Choice{{
				Index:        0,
				Delta:        openai.Delta{Content: "streamed text"},
				FinishReason: "stop",
			}},
		}
		data, _ := json.Marshal(chunk)
		w.Write([]byte(sseChunk(string(data))))
		flusher.Flush()
		w.Write([]byte(sseChunk("[DONE]")))
		flusher.Flush()
	})
	defer closeSrv()

	stream := c.StreamResponse(context.Background(), openai.CreateResponseRequest{
		Model: "gemini-test",
		Input: []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	})

	var text strings.Builder
	var sawCompleted bool
	for stream.Next() {
		ev := stream.Current()
		text.WriteString(ev.TextDelta())
		if ev.Type == "response.completed" {
			sawCompleted = true
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("StreamResponse error: %v", err)
	}
	if text.String() != "streamed text" {
		t.Errorf("accumulated text = %q, want %q", text.String(), "streamed text")
	}
	_ = sawCompleted // response.completed is emitted by the higher-level Client.StreamResponse wrapper in openai, not required here.
}

func TestStreamResponse_PropagatesChatError(t *testing.T) {
	c, closeSrv := newTestGeminiClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"boom"}}`))
	})
	defer closeSrv()
	c.maxRetries = 0
	// rebuild chat client with retries disabled so the error surfaces quickly
	cc, err := openai.New(openai.Config{BaseURL: c.baseURL + "openai/", MaxRetries: -1})
	if err != nil {
		t.Fatalf("openai.New() error: %v", err)
	}
	c.chatClient = cc

	stream := c.StreamResponse(context.Background(), openai.CreateResponseRequest{
		Model: "gemini-test",
		Input: []any{map[string]any{"type": "message", "role": "user", "content": "hi"}},
	})
	for stream.Next() {
	}
	if stream.Err() == nil {
		t.Fatal("expected StreamResponse to surface the chat completion error")
	}
}
