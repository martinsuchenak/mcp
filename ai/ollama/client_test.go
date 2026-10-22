package ollama

import (
	"encoding/json"
	"testing"

	"github.com/paularlott/mcp/ai/openai"
)

func TestNormalizeBase(t *testing.T) {
	cases := map[string]string{
		"":                          "https://ollama.com/",
		"http://h:11434":            "http://h:11434/",
		"http://h:11434/":           "http://h:11434/",
		"http://h:11434/v1":         "http://h:11434/",
		"http://h:11434/v1/":        "http://h:11434/",
		"http://h:11434/v1//":       "http://h:11434/",
		"https://ollama.com":        "https://ollama.com/",
		"https://ollama.com/v1":     "https://ollama.com/",
	}
	for in, want := range cases {
		if got := normalizeBase(in); got != want {
			t.Errorf("normalizeBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChatResponseToOpenAI(t *testing.T) {
	t.Run("text + usage", func(t *testing.T) {
		resp := &chatResponse{
			Model: "llama3",
			Message: message{Role: "assistant", Content: "hello"},
			DoneReason: "stop",
			Done: true,
			PromptEvalCount: 5,
			EvalCount: 3,
		}
		got := chatResponseToOpenAI(resp)
		if len(got.Choices) != 1 || got.Choices[0].Message.GetContentAsString() != "hello" {
			t.Fatalf("unexpected choices/content: %+v", got)
		}
		if got.Usage == nil || got.Usage.PromptTokens != 5 || got.Usage.CompletionTokens != 3 || got.Usage.TotalTokens != 8 {
			t.Fatalf("unexpected usage: %+v", got.Usage)
		}
		if got.Choices[0].FinishReason != "stop" {
			t.Fatalf("finish = %q, want stop", got.Choices[0].FinishReason)
		}
	})

	t.Run("tool calls set finish_reason", func(t *testing.T) {
		resp := &chatResponse{
			Model: "llama3",
			Message: message{Role: "assistant", ToolCalls: []toolCall{{
				Function: toolCallFunction{Name: "get_weather", Arguments: map[string]any{"city": "SF"}},
			}}},
		}
		got := chatResponseToOpenAI(resp)
		if len(got.Choices[0].Message.ToolCalls) != 1 {
			t.Fatalf("want 1 tool call, got %d", len(got.Choices[0].Message.ToolCalls))
		}
		tc := got.Choices[0].Message.ToolCalls[0]
		if tc.Function.Name != "get_weather" || tc.Function.Arguments["city"] != "SF" {
			t.Fatalf("unexpected tool call: %+v", tc)
		}
		if got.Choices[0].FinishReason != "tool_calls" {
			t.Fatalf("finish = %q, want tool_calls", got.Choices[0].FinishReason)
		}
	})
}

func TestStreamChunkToOpenAI(t *testing.T) {
	chunk := streamChunkToOpenAI("llama3", &chatResponse{
		Message: message{Role: "assistant", Content: "hi"},
	})
	if chunk.Object != "chat.completion.chunk" || chunk.Choices[0].Delta.Content != "hi" || chunk.Choices[0].Delta.Role != "assistant" {
		t.Fatalf("unexpected chunk: %+v", chunk)
	}

	// Terminal chunk carries finish reason + usage.
	done := streamChunkToOpenAI("llama3", &chatResponse{
		Done: true, DoneReason: "length",
		PromptEvalCount: 2, EvalCount: 9,
	})
	if done.Choices[0].FinishReason != "length" || done.Usage.TotalTokens != 11 {
		t.Fatalf("unexpected done chunk: %+v", done)
	}
}

func TestBuildChatRequestMapsOptions(t *testing.T) {
	temp := 0.7
	top := 0.9
	req := openai.ChatCompletionRequest{
		Model:               "llama3",
		Messages:            []openai.Message{{Role: "user", Content: "hi"}},
		MaxCompletionTokens: 50,
		Temperature:         &temp,
		TopP:                &top,
		Tools: []openai.Tool{{Type: "function", Function: openai.ToolFunction{Name: "t", Parameters: map[string]any{"type": "object"}}}},
	}
	c := &Client{}
	got := mustBuild(t, c, req, true)
	if !got.Stream || got.Model != "llama3" {
		t.Fatalf("stream/model wrong: %+v", got)
	}
	if got.Options["temperature"] != 0.7 || got.Options["top_p"] != 0.9 || got.Options["num_predict"] != 50 {
		t.Fatalf("options not mapped: %+v", got.Options)
	}
	if len(got.Tools) != 1 || got.Tools[0].Function.Name != "t" {
		t.Fatalf("tools not mapped: %+v", got.Tools)
	}
	if len(got.Messages) != 1 || got.Messages[0].Content != "hi" {
		t.Fatalf("messages not converted: %+v", got.Messages)
	}
}

func TestSplitContentImages(t *testing.T) {
	msg := openai.Message{Role: "user", Content: []openai.ContentPart{
		{Type: "text", Text: "look"},
		{Type: "image_url", ImageURL: &openai.ImageURL{URL: "data:image/png;base64,iVBORw0K"}},
	}}
	text, images := mustSplit(t, msg)
	if text != "look" {
		t.Fatalf("text = %q", text)
	}
	if len(images) != 1 || images[0] != "iVBORw0K" {
		t.Fatalf("images = %+v", images)
	}
}

func TestContextLengthFrom(t *testing.T) {
	cases := []struct {
		in   any
		want int
		ok   bool
	}{
		{float64(8192), 8192, true},
		{int64(4096), 4096, true},
		{float64(0), 0, false},
		{"x", 0, false},
		{nil, 0, false},
	}
	for _, tc := range cases {
		got, ok := contextLengthFrom(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("contextLengthFrom(%v) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestEmbeddingInputs(t *testing.T) {
	in, err := embeddingInputs("hello")
	if err != nil || len(in) != 1 || in[0] != "hello" {
		t.Fatalf("string input: %+v %v", in, err)
	}
	in, err = embeddingInputs([]string{"a", "b"})
	if err != nil || len(in) != 2 {
		t.Fatalf("[]string input: %+v %v", in, err)
	}
}

// TestChatRequestJSONShape sanity-checks that a chatRequest marshals to the
// field names Ollama expects (model, messages, stream, options).
func TestChatRequestJSONShape(t *testing.T) {
	temp := 0.5
	c := &Client{}
	cr := mustBuild(t, c, openai.ChatCompletionRequest{
		Model:       "m",
		Messages:    []openai.Message{{Role: "user", Content: "x"}},
		Temperature: &temp,
	}, false)
	b, _ := json.Marshal(cr)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	for _, key := range []string{"model", "messages", "stream", "options"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing top-level key %q in %s", key, string(b))
		}
	}
}
