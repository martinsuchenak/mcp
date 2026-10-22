package ollama

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/paularlott/mcp/ai/openai"
)

func TestConvertMessagesFull(t *testing.T) {
	c := &Client{}
	msgs := []openai.Message{
		{Role: "user", Content: "hello"},
		{
			Role: "assistant",
			ToolCalls: []openai.ToolCall{
				{ID: "call_1", Type: "function", Function: openai.ToolCallFunction{Name: "f", Arguments: map[string]any{"x": 1}}},
			},
		},
		{Role: "tool", Content: "result", ToolCallID: "call_1"},
	}
	out := mustConvert(t, c, msgs)
	if len(out) != 3 {
		t.Fatalf("len = %d, want 3", len(out))
	}
	if out[0].Content != "hello" {
		t.Errorf("out[0] = %+v", out[0])
	}
	if len(out[1].ToolCalls) != 1 || out[1].ToolCalls[0].Function.Name != "f" {
		t.Errorf("out[1] = %+v", out[1])
	}
	if out[2].Role != "tool" || out[2].Content != "result" {
		t.Errorf("out[2] = %+v", out[2])
	}
}

func TestSplitContentVariants(t *testing.T) {
	tests := []struct {
		name       string
		msg        openai.Message
		wantText   string
		wantImages int
	}{
		{"nil content", openai.Message{Content: nil}, "", 0},
		{"string content", openai.Message{Content: "plain"}, "plain", 0},
		{"parts without image_url object", openai.Message{Content: []openai.ContentPart{
			{Type: "image_url", ImageURL: nil},
			{Type: "text", Text: "hi"},
		}}, "hi", 0},
		{"unrecognized part type ignored", openai.Message{Content: []openai.ContentPart{
			{Type: "unknown", Text: "ignored"},
		}}, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, images := mustSplit(t, tt.msg)
			if text != tt.wantText {
				t.Errorf("text = %q, want %q", text, tt.wantText)
			}
			if len(images) != tt.wantImages {
				t.Errorf("images = %v, want len %d", images, tt.wantImages)
			}
		})
	}
}

func TestSplitContentDataURIImage(t *testing.T) {
	msg := openai.Message{Content: []openai.ContentPart{
		{Type: "image_url", ImageURL: &openai.ImageURL{URL: "data:image/png;base64,QUJD"}},
	}}
	_, images := mustSplit(t, msg)
	if len(images) != 1 || images[0] != "QUJD" {
		t.Errorf("images = %v, want [QUJD]", images)
	}
}

func TestDataURIToBase64(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantB64 string
		wantOK  bool
	}{
		{"valid", "data:image/png;base64,QUJD", "QUJD", true},
		{"no data prefix", "http://example.com/x.png", "", false},
		{"data prefix no comma", "data:image/png;base64", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := dataURIToBase64(tt.in)
			if got != tt.wantB64 || ok != tt.wantOK {
				t.Errorf("dataURIToBase64(%q) = (%q,%v), want (%q,%v)", tt.in, got, ok, tt.wantB64, tt.wantOK)
			}
		})
	}
}

func TestToolCallsFromOpenAIMultiple(t *testing.T) {
	calls := []openai.ToolCall{
		{Function: openai.ToolCallFunction{Name: "a", Arguments: map[string]any{"x": 1}}},
		{Function: openai.ToolCallFunction{Name: "b", Arguments: map[string]any{"y": 2}}},
	}
	out := toolCallsFromOpenAI(calls)
	if len(out) != 2 || out[0].Function.Name != "a" || out[1].Function.Name != "b" {
		t.Fatalf("out = %+v", out)
	}
}

func TestConvertToolsDefaultsTypeToFunction(t *testing.T) {
	tools := []openai.Tool{
		{Type: "", Function: openai.ToolFunction{Name: "t1"}},
		{Type: "function", Function: openai.ToolFunction{Name: "t2", Description: "d", Parameters: map[string]any{"type": "object"}}},
	}
	out := convertTools(tools)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Type != "function" {
		t.Errorf("out[0].Type = %q, want function (defaulted)", out[0].Type)
	}
	if out[1].Function.Description != "d" {
		t.Errorf("out[1] = %+v", out[1])
	}
}

func TestBuildChatRequestExtraBodyOptions(t *testing.T) {
	c := &Client{}
	req := openai.ChatCompletionRequest{
		Model:    "m",
		Messages: []openai.Message{{Role: "user", Content: "hi"}},
		ExtraBody: map[string]any{
			"options": map[string]any{"num_ctx": 4096, "seed": 42},
		},
	}
	got := mustBuild(t, c, req, false)
	if got.Options["num_ctx"] != 4096 || got.Options["seed"] != 42 {
		t.Errorf("options = %+v, want num_ctx/seed merged in", got.Options)
	}
}

func TestBuildChatRequestExtraBodyIgnoredWhenNotOptionsMap(t *testing.T) {
	c := &Client{}
	req := openai.ChatCompletionRequest{
		Model:     "m",
		Messages:  []openai.Message{{Role: "user", Content: "hi"}},
		ExtraBody: map[string]any{"options": "not a map"},
	}
	got := mustBuild(t, c, req, false)
	if len(got.Options) != 0 {
		t.Errorf("options = %+v, want empty when ExtraBody.options isn't a map", got.Options)
	}
}

func TestEffectiveMaxTokensPrecedence(t *testing.T) {
	if got := effectiveMaxTokens(openai.ChatCompletionRequest{MaxCompletionTokens: 10, MaxTokens: 20}); got != 10 {
		t.Errorf("effectiveMaxTokens = %d, want 10 (MaxCompletionTokens wins)", got)
	}
	if got := effectiveMaxTokens(openai.ChatCompletionRequest{MaxTokens: 20}); got != 20 {
		t.Errorf("effectiveMaxTokens = %d, want 20 (falls back to MaxTokens)", got)
	}
	if got := effectiveMaxTokens(openai.ChatCompletionRequest{}); got != 0 {
		t.Errorf("effectiveMaxTokens = %d, want 0", got)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	tests := map[string]string{
		"stop":       "stop",
		"length":     "length",
		"tool_calls": "tool_calls",
		"load":       "stop",
		"":           "stop",
		"unknown_x":  "unknown_x",
	}
	for in, want := range tests {
		if got := finishReason(in); got != want {
			t.Errorf("finishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChatResponseToOpenAINoUsageWhenBothZero(t *testing.T) {
	resp := &chatResponse{Model: "m", Message: message{Role: "assistant", Content: "hi"}}
	got := chatResponseToOpenAI(resp)
	if got.Usage != nil {
		t.Errorf("Usage = %+v, want nil when both counts are zero", got.Usage)
	}
}

func TestToolCallsToOpenAIMultiple(t *testing.T) {
	calls := []toolCall{
		{Function: toolCallFunction{Name: "a", Arguments: map[string]any{"x": 1}}},
		{Function: toolCallFunction{Name: "b", Arguments: map[string]any{"y": 2}}},
	}
	out := toolCallsToOpenAI(calls)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Index != 0 || out[1].Index != 1 {
		t.Errorf("indices = %d,%d, want 0,1", out[0].Index, out[1].Index)
	}
	if out[0].ID == "" || out[0].ID == out[1].ID {
		t.Errorf("ids = %q,%q, want distinct generated ids when Ollama sends none", out[0].ID, out[1].ID)
	}
	withIDs := toolCallsToOpenAI([]toolCall{{ID: "call_x", Function: toolCallFunction{Name: "a"}}})
	if withIDs[0].ID != "call_x" {
		t.Errorf("id = %q, want Ollama's call_x", withIDs[0].ID)
	}
}

func TestStreamChunkToOpenAIToolCallDeltaArgumentsMarshalled(t *testing.T) {
	chunk := streamChunkToOpenAI("m", &chatResponse{
		Message: message{ToolCalls: []toolCall{
			{Function: toolCallFunction{Name: "f", Arguments: map[string]any{"a": 1}}},
		}},
	})
	if len(chunk.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("deltas = %+v", chunk.Choices[0].Delta.ToolCalls)
	}
	dtc := chunk.Choices[0].Delta.ToolCalls[0]
	var args map[string]any
	if err := json.Unmarshal([]byte(dtc.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if args["a"] != float64(1) {
		t.Errorf("args = %+v", args)
	}
}

func TestStreamChunkToOpenAIDoneNoUsageWhenBothZero(t *testing.T) {
	chunk := streamChunkToOpenAI("m", &chatResponse{Done: true, DoneReason: "stop"})
	if chunk.Usage != nil {
		t.Errorf("Usage = %+v, want nil when both counts are zero", chunk.Usage)
	}
	if chunk.Choices[0].FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", chunk.Choices[0].FinishReason)
	}
}

// Tool results are sent back with the name of the tool they answer, looked
// up from the assistant's tool calls by call id.
func TestConvertMessagesToolResultsCarryToolName(t *testing.T) {
	c := &Client{}
	msgs := mustConvert(t, c, []openai.Message{
		{Role: "user", Content: "weather in Paris and Tokyo?"},
		{Role: "assistant", ToolCalls: []openai.ToolCall{
			{ID: "call_a", Type: "function", Function: openai.ToolCallFunction{Name: "weather", Arguments: map[string]any{"city": "Paris"}}},
			{ID: "call_b", Type: "function", Function: openai.ToolCallFunction{Name: "time", Arguments: map[string]any{"city": "Tokyo"}}},
		}},
		{Role: "tool", ToolCallID: "call_b", Content: "09:00"},
		{Role: "tool", ToolCallID: "call_a", Content: "sunny"},
		{Role: "tool", ToolCallID: "legacy_name", Content: "x"}, // ids used to be tool names
	})
	if len(msgs) != 5 {
		t.Fatalf("messages = %+v", msgs)
	}
	if ids := []string{msgs[1].ToolCalls[0].ID, msgs[1].ToolCalls[1].ID}; ids[0] != "call_a" || ids[1] != "call_b" {
		t.Errorf("assistant tool call ids = %v", ids)
	}
	for i, want := range map[int]string{2: "time", 3: "weather", 4: "legacy_name"} {
		if msgs[i].ToolName != want {
			t.Errorf("message %d tool_name = %q, want %q", i, msgs[i].ToolName, want)
		}
	}
	data, _ := json.Marshal(msgs[3])
	if !strings.Contains(string(data), `"tool_name":"weather"`) {
		t.Errorf("tool message JSON = %s", data)
	}
	if strings.Contains(string(mustJSON(t, msgs[0])), "tool_name") {
		t.Error("tool_name set on a non-tool message")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBuildChatRequestThink(t *testing.T) {
	c := &Client{}
	body := func(req openai.ChatCompletionRequest) string {
		b, err := json.Marshal(mustBuild(t, c, req, false))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	// Not given: the field is omitted, so the model's default applies.
	if got := body(openai.ChatCompletionRequest{Model: "m"}); strings.Contains(got, `"think"`) {
		t.Errorf("think sent when not requested: %s", got)
	}
	for _, tc := range []struct {
		req  openai.ChatCompletionRequest
		want string
	}{
		{openai.ChatCompletionRequest{Model: "m", ReasoningEffort: "none"}, `"think":false`},
		{openai.ChatCompletionRequest{Model: "m", ReasoningEffort: "minimal"}, `"think":false`},
		{openai.ChatCompletionRequest{Model: "m", ReasoningEffort: "high"}, `"think":true`},
		{openai.ChatCompletionRequest{Model: "m", ExtraBody: map[string]any{"think": false}}, `"think":false`},
		{openai.ChatCompletionRequest{Model: "m", ExtraBody: map[string]any{"think": "low"}, ReasoningEffort: "none"}, `"think":"low"`},
	} {
		if got := body(tc.req); !strings.Contains(got, tc.want) {
			t.Errorf("effort %q extra %v: %s, want %s", tc.req.ReasoningEffort, tc.req.ExtraBody, got, tc.want)
		}
	}
}
