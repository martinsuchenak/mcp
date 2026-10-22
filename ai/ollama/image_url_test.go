package ollama

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/paularlott/mcp/ai/openai"
)

func mustBuild(t *testing.T, c *Client, req openai.ChatCompletionRequest, stream bool) chatRequest {
	t.Helper()
	out, err := c.buildChatRequest(req, stream)
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	return out
}

func mustConvert(t *testing.T, c *Client, msgs []openai.Message) []message {
	t.Helper()
	out, err := c.convertMessages(msgs)
	if err != nil {
		t.Fatalf("convertMessages: %v", err)
	}
	return out
}

func mustSplit(t *testing.T, m openai.Message) (string, []string) {
	t.Helper()
	text, images, err := splitContent(m)
	if err != nil {
		t.Fatalf("splitContent: %v", err)
	}
	return text, images
}

func imageMessage(url string) openai.Message {
	return openai.Message{Role: "user", Content: []openai.ContentPart{
		{Type: "text", Text: "describe"},
		{Type: "image_url", ImageURL: &openai.ImageURL{URL: url}},
	}}
}

// An image_url that is not a data: URI is an error, and nothing is fetched:
// Ollama only accepts inline images, and fetching would make the caller's host
// request any URL a message names, outside the HTTP pool (and any policy) the
// client was given.
func TestImageURLMustBeDataURI(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte("PNGDATA"))
	}))
	defer srv.Close()

	for _, url := range []string{srv.URL + "/img.png", "https://example.com/x.png", "http://127.0.0.1:1/x", "file:///etc/passwd", "iVBORw0K"} {
		if _, _, err := splitContent(imageMessage(url)); !errors.Is(err, ErrImageURLNotSupported) {
			t.Errorf("%q: err = %v, want ErrImageURLNotSupported", url, err)
		}
		c := &Client{}
		if _, err := c.buildChatRequest(openai.ChatCompletionRequest{Model: "m", Messages: []openai.Message{imageMessage(url)}}, false); !errors.Is(err, ErrImageURLNotSupported) {
			t.Errorf("%q: buildChatRequest err = %v", url, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("the image URL was fetched %d times", n)
	}

	// data: URIs are still sent inline.
	_, images := mustSplit(t, imageMessage("data:image/png;base64,QUJD"))
	if len(images) != 1 || images[0] != "QUJD" {
		t.Fatalf("data URI: images = %v", images)
	}
}

// Completion and streaming completion refuse the request before contacting
// either the image URL or the Ollama server.
func TestCompletionRejectsImageURL(t *testing.T) {
	var imageHits, chatHits atomic.Int32
	image := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		imageHits.Add(1)
	}))
	defer image.Close()
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatHits.Add(1)
		w.Write([]byte(`{"model":"m","message":{"role":"assistant","content":"ok"},"done":true}`))
	}))
	defer ollama.Close()

	c, err := New(openai.Config{BaseURL: ollama.URL})
	if err != nil {
		t.Fatal(err)
	}
	req := openai.ChatCompletionRequest{Model: "m", Messages: []openai.Message{imageMessage(image.URL + "/secret")}}
	if _, err := c.ChatCompletion(context.Background(), req); !errors.Is(err, ErrImageURLNotSupported) {
		t.Fatalf("ChatCompletion err = %v, want ErrImageURLNotSupported", err)
	}

	stream := c.StreamChatCompletion(context.Background(), req)
	for stream.Next() {
	}
	if err := stream.Err(); !errors.Is(err, ErrImageURLNotSupported) {
		t.Fatalf("StreamChatCompletion err = %v, want ErrImageURLNotSupported", err)
	}
	if imageHits.Load() != 0 || chatHits.Load() != 0 {
		t.Fatalf("requests made: image=%d chat=%d, want none", imageHits.Load(), chatHits.Load())
	}
}
