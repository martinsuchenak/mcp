package ollama

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/paularlott/mcp"
	"github.com/paularlott/mcp/ai/openai"
	"github.com/paularlott/mcp/pool"
)

const (
	defaultBaseURL = "https://ollama.com"
	providerName   = "ollama"
)

// Client speaks Ollama's native API (/api/chat, /api/embed, /api/tags,
// /api/show) and presents the OpenAI-shaped ai.Client interface. It is the
// Ollama peer of mcp/ai/{openai,claude,gemini}.
type Client struct {
	apiKey             string
	baseURL            string
	extraHeaders       http.Header
	httpPool           pool.HTTPPool
	provider           string
	localServer        openai.MCPServer
	remoteServers      []*mcp.Client
	maxTokens          int
	temperature        *float64
	topP               *float64
	requestTimeout     time.Duration
	responseManager    *openai.ResponseManager
	maxRetries         int
	retryBackoff       time.Duration
	retryOnRateLimit   bool
	retryOnServerError bool
}

// New creates an Ollama client. The base URL is normalized to its native root
// (any trailing /v1 — Ollama's OpenAI-compat prefix — is stripped), defaulting
// to https://ollama.com when unset.
func New(config openai.Config) (*Client, error) {
	if config.BaseURL == "" {
		config.BaseURL = defaultBaseURL
	}
	config.BaseURL = normalizeBase(config.BaseURL)

	if config.RequestTimeout == 0 {
		config.RequestTimeout = openai.DefaultRequestTimeout
	}

	remoteServers := make([]*mcp.Client, len(config.RemoteServerConfigs))
	for i, rsc := range config.RemoteServerConfigs {
		if rsc.HTTPPool != nil {
			remoteServers[i] = mcp.NewClientWithPool(rsc.BaseURL, rsc.Auth, rsc.Namespace, rsc.HTTPPool)
		} else {
			remoteServers[i] = mcp.NewClient(rsc.BaseURL, rsc.Auth, rsc.Namespace)
		}
	}

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
		extraHeaders:       config.ExtraHeaders,
		httpPool:           config.HTTPPool,
		provider:           providerName,
		localServer:        config.LocalServer,
		remoteServers:      remoteServers,
		maxTokens:          config.MaxTokens,
		temperature:        config.Temperature,
		topP:               config.TopP,
		requestTimeout:     config.RequestTimeout,
		responseManager:    openai.NewClientResponseManager(config, providerName, config.BaseURL),
		maxRetries:         maxRetries,
		retryBackoff:       retryBackoff,
		retryOnRateLimit:   retryOnRateLimit,
		retryOnServerError: retryOnServerError,
	}, nil
}

// normalizeBase strips any trailing slash and /v1 (Ollama's OpenAI-compat
// prefix) and restores a single trailing slash so paths like "api/chat"
// resolve correctly.
func normalizeBase(b string) string {
	b = strings.TrimRight(b, "/")
	b = strings.TrimSuffix(b, "/v1")
	if b == "" {
		return defaultBaseURL + "/"
	}
	return b + "/"
}

// ChatCompletion performs a non-streaming chat completion with automatic MCP
// tool processing. Mirrors the claude client's tool-call loop.
func (c *Client) ChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) (*openai.ChatCompletionResponse, error) {
	if c.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.requestTimeout)
		defer cancel()
	}

	currentMessages := req.Messages
	requestHasTools := len(req.Tools) > 0

	if !requestHasTools && !openai.MCPToolsDisabled(ctx) {
		tools, err := c.getAllTools(ctx)
		if err == nil && len(tools) > 0 {
			req.Tools = openai.MCPToolsToOpenAI(tools)
		}
	}

	toolHandler := openai.ToolHandlerFromContext(ctx)
	hasServers := c.localServer != nil || len(c.remoteServers) > 0
	c.applyDefaults(&req)

	var cumulativeUsage openai.Usage
	for iteration := 0; iteration < openai.MAX_TOOL_CALL_ITERATIONS; iteration++ {
		req.Messages = currentMessages
		ollamaReq, err := c.buildChatRequest(req, false)
		if err != nil {
			return nil, err
		}

		var resp chatResponse
		if err := c.doRequest(ctx, http.MethodPost, "api/chat", ollamaReq, &resp); err != nil {
			return nil, err
		}

		response := chatResponseToOpenAI(&resp)

		tc := openai.NewTokenCounter()
		tc.AddPromptTokensFromMessages(req.Messages)
		if len(response.Choices) > 0 {
			tc.AddCompletionTokensFromMessage(&response.Choices[0].Message)
		}
		tc.InjectUsageIfMissing(response)

		cumulativeUsage.Add(response.Usage)

		if requestHasTools || !hasServers || len(response.Choices) == 0 || len(response.Choices[0].Message.ToolCalls) == 0 {
			response.Usage = &cumulativeUsage
			return response, nil
		}

		message := response.Choices[0].Message
		toolCalls := message.ToolCalls
		if toolHandler != nil {
			for _, tc := range toolCalls {
				if err := toolHandler.OnToolCall(tc); err != nil {
					return nil, fmt.Errorf("tool handler error: %w", err)
				}
			}
		}

		currentMessages = append(currentMessages, openai.BuildAssistantToolCallMessage(
			message.GetContentAsString(), toolCalls,
		))

		toolResults, err := openai.ExecuteToolCalls(toolCalls, func(name string, args map[string]any) (string, error) {
			r, err := c.callTool(ctx, name, args)
			if err != nil {
				return "", err
			}
			result, _ := openai.ExtractToolResult(r)
			return result, nil
		}, false)
		if err != nil {
			return nil, err
		}

		if toolHandler != nil {
			for i, tc := range toolCalls {
				if err := toolHandler.OnToolResult(tc.ID, tc.Function.Name, toolResults[i].Content.(string)); err != nil {
					return nil, fmt.Errorf("tool handler error: %w", err)
				}
			}
		}
		currentMessages = append(currentMessages, toolResults...)
	}

	return nil, openai.NewMaxToolIterationsError(openai.MAX_TOOL_CALL_ITERATIONS)
}

// StreamChatCompletion streams a chat completion as OpenAI-shaped chunks,
// translating Ollama's newline-delimited JSON stream on the fly and running
// the same tool-call loop as ChatCompletion.
func (c *Client) StreamChatCompletion(ctx context.Context, req openai.ChatCompletionRequest) *openai.ChatStream {
	responseChan := make(chan openai.ChatCompletionResponse, 50)
	errorChan := make(chan error, 1)
	stream := openai.NewChatStream(ctx, responseChan, errorChan)

	go func() {
		defer close(responseChan)
		defer close(errorChan)
		defer stream.SetRetryMetadata(nil)

		if c.requestTimeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.requestTimeout)
			defer cancel()
		}

		currentMessages := req.Messages
		requestHasTools := len(req.Tools) > 0
		hasServers := c.localServer != nil || len(c.remoteServers) > 0

		if !requestHasTools && !openai.MCPToolsDisabled(ctx) {
			tools, err := c.getAllTools(ctx)
			if err == nil && hasServers && len(tools) > 0 {
				req.Tools = openai.MCPToolsToOpenAI(tools)
			}
		}
		toolHandler := openai.ToolHandlerFromContext(ctx)
		c.applyDefaults(&req)

		var cumulativeUsage openai.Usage
		sendCumulativeUsage := func(id, model string) {
			if cumulativeUsage.TotalTokens > 0 {
				select {
				case responseChan <- openai.ChatCompletionResponse{ID: id, Object: "chat.completion.chunk", Model: model, Choices: []openai.Choice{}, Usage: &cumulativeUsage}:
				case <-ctx.Done():
				}
			}
		}

		for iteration := 0; iteration < openai.MAX_TOOL_CALL_ITERATIONS; iteration++ {
			req.Messages = currentMessages
			ollamaReq, err := c.buildChatRequest(req, true)
			if err != nil {
				errorChan <- err
				return
			}

			finalResponse, retryMeta, err := c.streamChatCompletion(ctx, ollamaReq, responseChan, hasServers && !requestHasTools)
			stream.SetRetryMetadata(retryMeta)
			if err != nil {
				errorChan <- err
				return
			}
			if finalResponse != nil {
				cumulativeUsage.Add(finalResponse.Usage)
			}

			if requestHasTools || !hasServers || finalResponse == nil || len(finalResponse.Choices) == 0 || len(finalResponse.Choices[0].Message.ToolCalls) == 0 {
				if finalResponse != nil {
					sendCumulativeUsage(finalResponse.ID, finalResponse.Model)
				}
				return
			}

			message := finalResponse.Choices[0].Message
			toolCalls := message.ToolCalls
			if toolHandler != nil {
				for _, tc := range toolCalls {
					if err := toolHandler.OnToolCall(tc); err != nil {
						errorChan <- fmt.Errorf("tool handler error: %w", err)
						return
					}
				}
			}
			currentMessages = append(currentMessages, openai.BuildAssistantToolCallMessage(
				message.GetContentAsString(), toolCalls,
			))

			toolResults, err := openai.ExecuteToolCalls(toolCalls, func(name string, args map[string]any) (string, error) {
				r, err := c.callTool(ctx, name, args)
				if err != nil {
					return "", err
				}
				result, _ := openai.ExtractToolResult(r)
				return result, nil
			}, false)
			if err != nil {
				errorChan <- err
				return
			}
			if toolHandler != nil {
				for i, tc := range toolCalls {
					if err := toolHandler.OnToolResult(tc.ID, tc.Function.Name, toolResults[i].Content.(string)); err != nil {
						errorChan <- fmt.Errorf("tool handler error: %w", err)
						return
					}
				}
			}
			currentMessages = append(currentMessages, toolResults...)
		}

		errorChan <- openai.NewMaxToolIterationsError(openai.MAX_TOOL_CALL_ITERATIONS)
	}()

	return stream
}

func (c *Client) applyDefaults(req *openai.ChatCompletionRequest) {
	if effectiveMaxTokens(*req) == 0 && c.maxTokens > 0 {
		req.MaxCompletionTokens = c.maxTokens
	}
	if req.Temperature == nil && c.temperature != nil {
		req.Temperature = c.temperature
	}
	if req.TopP == nil && c.topP != nil {
		req.TopP = c.topP
	}
}

func (c *Client) getAllTools(ctx context.Context) ([]mcp.MCPTool, error) {
	var all []mcp.MCPTool
	if c.localServer != nil {
		all = append(all, c.localServer.ListToolsWithContext(ctx)...)
	}
	for _, client := range c.remoteServers {
		tools, err := client.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list tools from remote server: %w", err)
		}
		all = append(all, tools...)
	}
	return all, nil
}

func (c *Client) callTool(ctx context.Context, name string, args map[string]any) (*mcp.ToolResponse, error) {
	for _, client := range c.remoteServers {
		ns := client.Namespace()
		if ns != "" && strings.HasPrefix(name, ns) {
			return client.CallTool(ctx, name, args)
		}
	}
	if c.localServer == nil {
		return nil, fmt.Errorf("no local MCP server configured")
	}
	return c.localServer.CallTool(ctx, name, args)
}

// streamChatCompletion issues a streaming POST /api/chat, forwarding each
// newline-delimited Ollama object as an OpenAI chunk on responseChan. It also
// reassembles the streamed message so the caller can detect trailing tool
// calls and loop.
// streamChatCompletion streams one completion to responseChan. When
// handleTools is set, tool calls are handled internally (MCP servers), so
// tool-call deltas are withheld from the caller.
func (c *Client) streamChatCompletion(ctx context.Context, req chatRequest, responseChan chan<- openai.ChatCompletionResponse, handleTools bool) (*openai.ChatCompletionResponse, *openai.RetryMetadata, error) {
	maxAttempts := c.maxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}

	var attempts int
	var rateLimitHit bool
	var totalBackoff time.Duration
	var lastErr error
	var retryAfterHint time.Duration

	for attempt := 0; attempt < maxAttempts; attempt++ {
		attempts = attempt + 1
		if attempt > 0 {
			bo := c.backoffForAttempt(attempt - 1)
			if retryAfterHint > bo {
				bo = retryAfterHint
			}
			retryAfterHint = 0
			totalBackoff += bo
			timer := time.NewTimer(bo)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, nil, ctx.Err()
			case <-timer.C:
			}
		}

		data, err := json.Marshal(req)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal request: %w", err)
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"api/chat", bytes.NewReader(data))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to create request: %w", err)
		}
		c.setHeaders(httpReq)

		resp, err := c.httpClient().Do(httpReq)
		if err != nil {
			return nil, nil, fmt.Errorf("request failed: %w", err)
		}

		body := decompressBody(resp)
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(body)
			body.Close()
			resp.Body.Close()
			statusCode := resp.StatusCode
			if statusCode == 429 {
				rateLimitHit = true
			}
			lastErr = fmt.Errorf("ollama API error: status %d: %s", statusCode, strings.TrimSpace(string(b)))
			if c.shouldRetry(statusCode) && attempt < maxAttempts-1 {
				retryAfterHint = parseRetryAfter(resp.Header.Get("Retry-After"))
				continue
			}
			return nil, nil, lastErr
		}

		assembled, err := c.readChatStream(ctx, body, req.Model, responseChan, handleTools)
		body.Close()
		resp.Body.Close()

		var retryMeta *openai.RetryMetadata
		if attempts > 1 {
			retryMeta = &openai.RetryMetadata{Attempts: attempts, RateLimitHit: rateLimitHit, TotalBackoff: totalBackoff}
		}
		return assembled, retryMeta, err
	}
	return nil, nil, lastErr
}

// readChatStream parses the NDJSON response body, forwarding each object as an
// OpenAI chunk and reassembling the final assistant message (for tool-call
// detection by the loop above). Ollama sends each tool call complete, often
// one per object; calls are told apart by id, then by function.index, and
// otherwise each is taken as a new call. When handleTools is set, tool-call
// deltas are not forwarded.
func (c *Client) readChatStream(ctx context.Context, r io.Reader, model string, responseChan chan<- openai.ChatCompletionResponse, handleTools bool) (*openai.ChatCompletionResponse, error) {
	var sb strings.Builder
	type assembledCall struct {
		id   string
		name string
		args map[string]any
	}
	calls := map[int]*assembledCall{}
	indexByID := map[string]int{}
	nextIndex := 0
	finish := ""

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var obj chatResponse
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}

		if obj.Message.Content != "" {
			sb.WriteString(obj.Message.Content)
		}
		var deltas []openai.DeltaToolCall
		for _, call := range obj.Message.ToolCalls {
			index, seen := indexByID[call.ID]
			if !seen || call.ID == "" {
				index = nextIndex
				if call.Function.Index != nil {
					index = *call.Function.Index
				}
			}
			if index >= nextIndex {
				nextIndex = index + 1
			}
			ac := calls[index]
			if ac == nil {
				ac = &assembledCall{id: toolCallID(call, index)}
				calls[index] = ac
			} else if call.ID != "" {
				ac.id = call.ID
			}
			if call.ID != "" {
				indexByID[call.ID] = index
			}
			ac.name = call.Function.Name
			if len(call.Function.Arguments) > 0 {
				ac.args = call.Function.Arguments
			}
			args, _ := openai.ArgumentsJSON(call.Function.Arguments)
			deltas = append(deltas, openai.DeltaToolCall{
				Index: index, ID: ac.id, Type: "function",
				Function: openai.DeltaFunction{Name: call.Function.Name, Arguments: string(args)},
			})
		}
		if obj.Done && obj.DoneReason != "" {
			finish = obj.DoneReason
		}

		chunk := streamChunkToOpenAI(model, &obj)
		chunk.Choices[0].Delta.ToolCalls = deltas
		if handleTools && len(deltas) > 0 {
			chunk.Choices[0].Delta.ToolCalls = nil
			if chunk.Choices[0].Delta.Content == "" && chunk.Choices[0].FinishReason == "" && chunk.Usage == nil {
				continue
			}
		}
		select {
		case responseChan <- *chunk:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	out := &openai.ChatCompletionResponse{
		ID:    model,
		Model: model,
		Choices: []openai.Choice{{
			Index:        0,
			FinishReason: finishReason(finish),
		}},
	}
	msg := openai.Message{Role: "assistant"}
	msg.SetContentAsString(sb.String())
	if len(calls) > 0 {
		// Iterate the calls received, not 0..nextIndex: the index comes from
		// the server
		indices := make([]int, 0, len(calls))
		for i := range calls {
			indices = append(indices, i)
		}
		sort.Ints(indices)
		toolCalls := make([]openai.ToolCall, 0, len(calls))
		for _, i := range indices {
			ac := calls[i]
			toolCalls = append(toolCalls, openai.ToolCall{
				Index: i,
				ID:    ac.id,
				Type:  "function",
				Function: openai.ToolCallFunction{
					Name:      ac.name,
					Arguments: ac.args,
				},
			})
		}
		msg.ToolCalls = toolCalls
		if sb.Len() == 0 {
			out.Choices[0].FinishReason = "tool_calls"
		}
	}
	out.Choices[0].Message = msg
	return out, nil
}

// doRequest sends a non-streaming request with retry, decoding a JSON result.
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
			reqBody = bytes.NewReader(data)
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
		if err != nil {
			return fmt.Errorf("failed to create request: %w", err)
		}
		c.setHeaders(req)

		resp, err := c.httpClient().Do(req)
		if err != nil {
			return fmt.Errorf("request failed: %w", err)
		}

		respBody := decompressBody(resp)
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(respBody)
			respBody.Close()
			resp.Body.Close()
			statusCode := resp.StatusCode
			lastErr = fmt.Errorf("ollama API error: status %d: %s", statusCode, strings.TrimSpace(string(b)))
			if c.shouldRetry(statusCode) && attempt < maxAttempts-1 {
				retryAfterHint = parseRetryAfter(resp.Header.Get("Retry-After"))
				continue
			}
			return lastErr
		}

		b, err := io.ReadAll(respBody)
		respBody.Close()
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("failed to read response: %w", err)
		}
		if result != nil {
			if err := json.Unmarshal(b, result); err != nil {
				return fmt.Errorf("failed to decode response: %w", err)
			}
		}
		return nil
	}
	return lastErr
}

func (c *Client) httpClient() *http.Client {
	if c.httpPool != nil {
		return c.httpPool.GetHTTPClient()
	}
	return pool.GetPool().GetHTTPClient()
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	for key, values := range c.extraHeaders {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
}

func (c *Client) shouldRetry(statusCode int) bool {
	if statusCode == 429 && c.retryOnRateLimit {
		return true
	}
	if statusCode >= 500 && c.retryOnServerError {
		return true
	}
	return false
}

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

func decompressBody(resp *http.Response) io.ReadCloser {
	if resp.Header.Get("Content-Encoding") == "gzip" {
		if gr, err := gzip.NewReader(resp.Body); err == nil {
			return gr
		}
	}
	return resp.Body
}

// Provider returns the provider name.
func (c *Client) Provider() string { return c.provider }

// SupportsCapability reports Ollama's client-level capabilities: embeddings,
// decision models (System One) and the emulated Responses API.
func (c *Client) SupportsCapability(cap string) bool {
	switch cap {
	case "embeddings", "decision", "responses_emulated":
		return true
	}
	return false
}

// Close is a no-op (response managers persist like the other providers).
func (c *Client) Close() error { return nil }
