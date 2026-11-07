package mcp

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestToolRequestHelpers(t *testing.T) {
	req := &ToolRequest{args: map[string]any{
		"s":    "x",
		"i":    5.0,
		"b":    true,
		"ss":   []any{"a", "b"},
		"ii":   []any{1.0, 2.0},
		"ff":   []any{1.5, 2.5},
		"obj":  map[string]any{"k": "v", "n": 3.0, "t": true},
		"objs": []any{map[string]any{"a": 1.0}},
	}}

	if v, _ := req.String("s"); v != "x" {
		t.Fatal("string")
	}
	if v := req.StringOr("sx", "d"); v != "d" {
		t.Fatal("string or")
	}
	if v, _ := req.Int("i"); v != 5 {
		t.Fatal("int")
	}
	if v := req.IntOr("ix", 7); v != 7 {
		t.Fatal("int or")
	}
	if v, _ := req.Float("i"); v != 5.0 {
		t.Fatal("float")
	}
	if v := req.FloatOr("fx", 1.2); v != 1.2 {
		t.Fatal("float or")
	}
	if v, _ := req.Bool("b"); !v {
		t.Fatal("bool")
	}
	if v := req.BoolOr("bx", true); !v {
		t.Fatal("bool or")
	}
	if v, _ := req.StringSlice("ss"); len(v) != 2 || v[1] != "b" {
		t.Fatal("str slice")
	}
	if v, _ := req.IntSlice("ii"); len(v) != 2 || v[0] != 1 {
		t.Fatal("int slice")
	}
	if v, _ := req.FloatSlice("ff"); len(v) != 2 || v[1] != 2.5 {
		t.Fatal("float slice")
	}
	if v, _ := req.Object("obj"); v["k"].(string) != "v" {
		t.Fatal("obj")
	}
	if v, _ := req.ObjectSlice("objs"); len(v) != 1 {
		t.Fatal("obj slice")
	}
	if v, _ := req.ObjectString("obj", "k"); v != "v" {
		t.Fatal("obj str prop")
	}
	if v, _ := req.ObjectInt("obj", "n"); v != 3 {
		t.Fatal("obj int prop")
	}
	if v, _ := req.ObjectBool("obj", "t"); !v {
		t.Fatal("obj bool prop")
	}
}

func TestToolResponseHelpers(t *testing.T) {
	r := NewToolResponseText("hi")
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatal("text")
	}

	r = NewToolResponseJSON(map[string]any{"a": 1})
	if r.Content[0].Type != "text" || r.Content[0].Text == "" {
		t.Fatal("json")
	}

	r = NewToolResponseTOON(map[string]any{"a": 1})
	if r.Content[0].Type != "text" || r.Content[0].Text == "" {
		t.Fatal("toon")
	}

	img := NewToolResponseImage([]byte{0x01, 0x02}, "image/png")
	if img.Content[0].Type != "image" || img.Content[0].MimeType != "image/png" {
		t.Fatal("image type")
	}
	if img.Content[0].Data != base64.StdEncoding.EncodeToString([]byte{0x01, 0x02}) {
		t.Fatal("image data")
	}

	aud := NewToolResponseAudio([]byte{0x03}, "audio/wav")
	if aud.Content[0].Type != "audio" || aud.Content[0].MimeType != "audio/wav" {
		t.Fatal("audio")
	}

	res := NewToolResponseResource("file://x", "hello", "text/plain")
	if res.Content[0].Type != "resource" || res.Content[0].Resource.URI != "file://x" {
		t.Fatal("res")
	}

	link := NewToolResponseResourceLink("https://x", "open")
	if link.Content[0].Type != "resource_link" || link.Content[0].URI != "https://x" || link.Content[0].Name != "open" {
		t.Fatal("link")
	}

	st := NewToolResponseStructured(map[string]any{"k": "v"})
	if st.StructuredContent == nil {
		t.Fatal("structured: StructuredContent not set")
	}
	if len(st.Content) != 1 || st.Content[0].Type != "text" || st.Content[0].Text != `{"k":"v"}` {
		t.Fatalf("structured: expected a text fallback block with the same JSON, got %+v", st.Content)
	}

	combined := NewToolResponseMulti(r, img)
	if len(combined.Content) != len(r.Content)+len(img.Content) {
		t.Fatal("multi")
	}
}

func TestToolRequestErrors(t *testing.T) {
	req := &ToolRequest{args: map[string]any{"x": 1.0, "arr": []any{"a", 2}}}
	if _, err := req.String("missing"); err == nil {
		t.Fatal("expected missing param error")
	}
	if _, err := req.String("x"); err == nil {
		t.Fatal("expected type error for string")
	}
	if _, err := req.StringSlice("arr"); err == nil {
		t.Fatal("expected mixed-type array to error")
	}
	if _, err := req.Object("x"); err == nil {
		t.Fatal("expected not object error")
	}
}

// TestNewToolResponseStructured_MarshalError covers the unhappy path: a value
// that can't be JSON-marshaled (a channel) must produce a clear error text
// block rather than a StructuredContent the client could never decode either.
func TestNewToolResponseStructured_MarshalError(t *testing.T) {
	resp := NewToolResponseStructured(map[string]any{"ch": make(chan int)})
	if resp.StructuredContent != nil {
		t.Errorf("StructuredContent = %v, want nil on marshal error", resp.StructuredContent)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" {
		t.Fatalf("expected a single text error block, got %+v", resp.Content)
	}
	if !strings.Contains(resp.Content[0].Text, "Error marshaling data") {
		t.Errorf("Text = %q, want it to mention the marshal error", resp.Content[0].Text)
	}
}

// TestNewToolResponseStructured_AnyValue covers that structuredContent may
// be any JSON value (2026-07-28), and that the Legacy path strips a
// non-object value since those revisions define it as an object.
func TestNewToolResponseStructured_AnyValue(t *testing.T) {
	cases := []struct {
		name   string
		data   any
		legacy bool // kept on the Legacy path?
	}{
		{"slice", []int{1, 2, 3}, false},
		{"string", "hello", false},
		{"number", 42, false},
		{"bool", true, false},
		{"object", map[string]any{"a": 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := NewToolResponseStructured(tc.data)
			if resp.IsError || resp.StructuredContent == nil {
				t.Fatalf("resp = %+v, want structuredContent kept", resp)
			}
			if len(resp.Content) != 1 || resp.Content[0].Type != "text" {
				t.Fatalf("expected a single text block, got %+v", resp.Content)
			}
			if kept := legacyStructuredContent(resp.StructuredContent) != nil; kept != tc.legacy {
				t.Errorf("legacyStructuredContent kept = %v, want %v", kept, tc.legacy)
			}
		})
	}
}

// TestNewToolResponseStructured_StructAccepted covers that a Go struct
// (which marshals to a JSON object even though its Kind is not Map) is
// accepted, not just map[string]any.
func TestNewToolResponseStructured_StructAccepted(t *testing.T) {
	type payload struct {
		Records []int `json:"records"`
	}
	resp := NewToolResponseStructured(payload{Records: []int{1, 2}})
	if resp.StructuredContent == nil {
		t.Fatal("expected StructuredContent to be set for a struct value")
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != `{"records":[1,2]}` {
		t.Fatalf("unexpected text fallback: %+v", resp.Content)
	}
}

func TestStructuredContentPrecedenceInMulti(t *testing.T) {
	a := NewToolResponseStructured(map[string]any{"a": 1})
	b := NewToolResponseStructured(map[string]any{"b": 2})
	m := NewToolResponseMulti(a, b)
	if sc, ok := m.StructuredContent.(map[string]any); !ok || sc["b"].(int) != 2 {
		t.Fatal("expected last structured content to win")
	}
}
