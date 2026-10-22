package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/paularlott/mcp/ai/openai"
	"github.com/paularlott/mcp/pool"
)

const (
	defaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	providerName   = "gemini"
)

type Client struct {
	apiKey             string
	baseURL            string
	httpPool           pool.HTTPPool
	provider           string
	chatClient         *openai.Client // Use OpenAI client for chat/streaming
	responseManager    *openai.ResponseManager
	maxRetries         int
	retryBackoff       time.Duration
	retryOnRateLimit   bool
	retryOnServerError bool
}

func New(config openai.Config) (*Client, error) {
	if config.BaseURL == "" {
		config.BaseURL = defaultBaseURL
	}
	if !strings.HasSuffix(config.BaseURL, "/") {
		config.BaseURL += "/"
	}

	// Create OpenAI client for chat/streaming using /openai/ endpoint
	chatClient, err := openai.New(openai.Config{
		APIKey:              config.APIKey,
		BaseURL:             config.BaseURL + "openai/",
		ExtraHeaders:        config.ExtraHeaders,
		HTTPPool:            config.HTTPPool,
		LocalServer:         config.LocalServer,
		RemoteServerConfigs: config.RemoteServerConfigs,
		MaxTokens:           config.MaxTokens,
		Temperature:         config.Temperature,
		TopP:                config.TopP,
		FrequencyPenalty:    config.FrequencyPenalty,
		PresencePenalty:     config.PresencePenalty,
		RequestTimeout:      config.RequestTimeout,
		MaxRetries:          config.MaxRetries,
		RetryBackoff:        config.RetryBackoff,
		RetryOnRateLimit:    config.RetryOnRateLimit,
		RetryOnServerError:  config.RetryOnServerError,
	})
	if err != nil {
		return nil, err
	}

	// Responses are stored scoped to this client
	responseManager := openai.NewClientResponseManager(config, providerName, config.BaseURL)

	// Retry defaults (mirror the openai client defaults)
	maxRetries := config.MaxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}
	if maxRetries < -1 {
		return nil, fmt.Errorf("invalid MaxRetries %d: must be -1 (disable), 0 (default), or positive", maxRetries)
	}
	retryBackoff := config.RetryBackoff
	if retryBackoff == 0 {
		retryBackoff = time.Second
	}
	if retryBackoff < 0 {
		return nil, fmt.Errorf("invalid RetryBackoff %v: must be non-negative", retryBackoff)
	}
	retryOnRateLimit := true
	if config.RetryOnRateLimit != nil {
		retryOnRateLimit = *config.RetryOnRateLimit
	}
	retryOnServerError := true
	if config.RetryOnServerError != nil {
		retryOnServerError = *config.RetryOnServerError
	}

	return &Client{
		apiKey:             config.APIKey,
		baseURL:            config.BaseURL,
		httpPool:           config.HTTPPool,
		provider:           providerName,
		chatClient:         chatClient,
		responseManager:    responseManager,
		maxRetries:         maxRetries,
		retryBackoff:       retryBackoff,
		retryOnRateLimit:   retryOnRateLimit,
		retryOnServerError: retryOnServerError,
	}, nil
}

// Provider returns the provider name
func (c *Client) Provider() string {
	return c.provider
}

// SupportsCapability checks if the provider supports a capability
func (c *Client) SupportsCapability(cap string) bool {
	// Embeddings via Gemini's own API; the Responses API is emulated
	return cap == "embeddings" || cap == "responses_emulated"
}

// ChatCompletion delegates to OpenAI client (uses /openai/ endpoint)
func (c *Client) ChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (*openai.ChatCompletionResponse, error) {
	return c.chatClient.ChatCompletion(ctx, req)
}

// StreamChatCompletion delegates to OpenAI client (uses /openai/ endpoint)
func (c *Client) StreamChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) *openai.ChatStream {
	return c.chatClient.StreamChatCompletion(ctx, req)
}

// GetModels fetches the list of available models from Gemini API
func (c *Client) GetModels(ctx context.Context) (*openai.ModelsResponse, error) {
	type geminiModelResponse struct {
		Models []struct {
			Name            string `json:"name"`
			InputTokenLimit int    `json:"inputTokenLimit"`
		} `json:"models"`
	}

	var geminiResp geminiModelResponse
	if err := c.doRequest(ctx, "GET", "models", nil, &geminiResp); err != nil {
		return nil, err
	}

	models := make([]openai.Model, 0, len(geminiResp.Models))
	for _, m := range geminiResp.Models {
		modelID := m.Name
		modelID = strings.TrimPrefix(modelID, "models/")
		models = append(models, openai.Model{
			ID:            modelID,
			Object:        "model",
			ContextWindow: m.InputTokenLimit,
		})
	}

	return &openai.ModelsResponse{
		Object: "list",
		Data:   models,
	}, nil
}

// CreateEmbedding creates embeddings using Gemini's embedContent API
func (c *Client) CreateEmbedding(ctx context.Context, req openai.EmbeddingRequest) (*openai.EmbeddingResponse, error) {
	// Convert input to string or array of strings
	var content []string
	switch v := req.Input.(type) {
	case string:
		content = []string{v}
	case []string:
		content = v
	case []any:
		content = make([]string, len(v))
		for i, item := range v {
			if s, ok := item.(string); ok {
				content[i] = s
			} else {
				return nil, fmt.Errorf("invalid input type")
			}
		}
	default:
		return nil, fmt.Errorf("invalid input type")
	}

	// The model is one path segment: escaping it keeps a name containing "/",
	// "?" or ".." from reaching a different endpoint. GetModels returns IDs
	// without the "models/" prefix, but accept it too.
	endpoint := "models/" + url.PathEscape(strings.TrimPrefix(req.Model, "models/")) + ":embedContent"

	embeddings := make([]openai.Embedding, len(content))
	for i, text := range content {
		geminiReq := map[string]any{
			"content": map[string]any{
				"parts": []map[string]string{{"text": text}},
			},
			"taskType": "SEMANTIC_SIMILARITY",
		}
		if req.Dimensions > 0 {
			geminiReq["outputDimensionality"] = req.Dimensions
		}

		var geminiResp struct {
			Embedding struct {
				Values []float64 `json:"values"`
			} `json:"embedding"`
		}

		if err := c.doRequest(ctx, "POST", endpoint, geminiReq, &geminiResp); err != nil {
			return nil, err
		}

		embeddings[i] = openai.Embedding{
			Object:    "embedding",
			Embedding: geminiResp.Embedding.Values,
			Index:     i,
		}
	}

	return &openai.EmbeddingResponse{
		Object: "list",
		Data:   embeddings,
		Model:  req.Model,
		Usage: openai.Usage{
			PromptTokens: len(content),
			TotalTokens:  len(content),
		},
	}, nil
}

func (c *Client) shouldRetry(statusCode int) bool {
	if statusCode == http.StatusTooManyRequests && c.retryOnRateLimit {
		return true
	}
	if statusCode >= 500 && c.retryOnServerError {
		return true
	}
	return false
}

// backoffForAttempt returns the duration to wait before the given retry attempt (0-indexed).
func (c *Client) backoffForAttempt(attempt int) time.Duration {
	if attempt > 30 {
		return 30 * time.Second
	}
	bo := c.retryBackoff * time.Duration(int64(1)<<uint(attempt))
	if bo > 30*time.Second {
		bo = 30 * time.Second
	}
	return bo
}

// parseRetryAfter parses a Retry-After header value and returns the duration to wait.
// Returns 0 if the header is absent, zero, or cannot be parsed.
// Supports integer seconds ("60") and HTTP-date formats.
func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func (c *Client) doRequest(ctx context.Context, method, path string, body any, result any) error {
	maxAttempts := c.maxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var lastErr error
	var retryAfterHint time.Duration
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			bo := c.backoffForAttempt(attempt - 1)
			if retryAfterHint > bo {
				bo = retryAfterHint
			}
			retryAfterHint = 0
			timer := time.NewTimer(bo)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}

		var reqBody io.Reader
		if body != nil {
			data, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("failed to marshal request: %w", err)
			}
			reqBody = strings.NewReader(string(data))
		}

		var httpClient *http.Client
		if c.httpPool != nil {
			httpClient = c.httpPool.GetHTTPClient()
		} else {
			httpClient = pool.GetPool().GetHTTPClient()
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		// The key goes in a header, never the URL: errors quote the request
		// URL, and proxies and access logs record it.
		if c.apiKey != "" {
			req.Header.Set("x-goog-api-key", c.apiKey)
		}

		resp, err := httpClient.Do(req)
		if err != nil {
			return fmt.Errorf("request failed: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			statusCode := resp.StatusCode
			lastErr = fmt.Errorf("gemini API error: status %d: %s", statusCode, string(bodyBytes))
			if c.shouldRetry(statusCode) && attempt < maxAttempts-1 {
				retryAfterHint = parseRetryAfter(resp.Header.Get("Retry-After"))
				continue
			}
			return lastErr
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("failed to read response: %w", err)
		}

		if result != nil {
			if err := json.Unmarshal(bodyBytes, result); err != nil {
				return fmt.Errorf("failed to decode response: %w", err)
			}
		}

		return nil
	}

	return lastErr
}

// StreamResponse emulates the OpenAI Responses API streaming using chat completions
func (c *Client) StreamResponse(ctx context.Context, req openai.CreateResponseRequest) *openai.ResponseStream {
	eventChan := make(chan openai.ResponseStreamEvent, 50)
	errorChan := make(chan error, 1)
	go func() {
		defer close(eventChan)
		defer close(errorChan)
		openai.StreamResponseEmulatedWithManager(ctx, c.chatClient, c.responseManager, req, eventChan, errorChan)
	}()
	return openai.NewResponseStream(ctx, eventChan, errorChan)
}

// CreateResponse emulates the OpenAI Responses API using chat completions
// Processes async in the background and returns immediately with an in_progress status
func (c *Client) CreateResponse(ctx context.Context, req openai.CreateResponseRequest) (*openai.ResponseObject, error) {
	return openai.CreateResponseEmulated(ctx, c, c.responseManager, req)
}

// GetResponse retrieves a response by ID (blocking until complete or error)
func (c *Client) GetResponse(ctx context.Context, id string) (*openai.ResponseObject, error) {
	return openai.GetResponseEmulated(ctx, c.responseManager, id)
}

// CancelResponse cancels an in-progress response
func (c *Client) CancelResponse(ctx context.Context, id string) (*openai.ResponseObject, error) {
	return openai.CancelResponseEmulated(ctx, c.responseManager, id)
}

// DeleteResponse deletes a response by ID
func (c *Client) DeleteResponse(ctx context.Context, id string) error {
	return openai.DeleteResponseEmulated(ctx, c.responseManager, id)
}

// CompactResponse compacts a conversation by having the model summarise it
func (c *Client) CompactResponse(ctx context.Context, req openai.CompactResponseRequest) (*openai.CompactedResponse, error) {
	return openai.CompactResponseEmulated(ctx, c, c.responseManager, req)
}

// Close closes the client
// Note: Response managers persist for 15 minutes after last response expires
func (c *Client) Close() error {
	return nil
}
