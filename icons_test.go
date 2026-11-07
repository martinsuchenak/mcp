package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/paularlott/jsonrpc"
)

var testIcon = Icon{Src: "https://example.com/icon.png", MimeType: "image/png", Sizes: []string{"48x48"}, Theme: "light"}

// TestIcons_DoNotBreakOlderLegacyProtocolVersions is a direct backward-
// compatibility check: Icons is new, additive metadata, but every prior
// Legacy protocol version (2025-03-26 through 2025-06-18) must keep working
// exactly as before even when a tool has icons set — old clients that don't
// recognize the field are expected to ignore it (ordinary JSON forward
// compatibility), not choke on it, and the negotiated capabilities shape for
// each version must be completely unaffected by icons existing at all.
func TestIcons_DoNotBreakOlderLegacyProtocolVersions(t *testing.T) {
	s := NewServer("icon-version-compat", "1")
	s.SetIcons(testIcon)
	s.RegisterTool(NewTool("t", "a tool").Icons(testIcon), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			initBody, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "initialize",
				"params": map[string]any{
					"protocolVersion": version,
					"capabilities":    map[string]any{},
					"clientInfo":      map[string]any{"name": "t", "version": "1"},
				},
			})
			req, _ := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(initBody))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("initialize: %v", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("decode: %v, body: %s", err, raw)
			}
			result, ok := out["result"].(map[string]any)
			if !ok {
				t.Fatalf("no result: %s", raw)
			}
			if result["protocolVersion"] != version {
				t.Errorf("negotiated protocolVersion = %v, want %v (icons must not affect negotiation)", result["protocolVersion"], version)
			}
			caps, ok := result["capabilities"].(map[string]any)
			if !ok {
				t.Fatalf("no capabilities: %s", raw)
			}
			// The capability shape is unrelated to icons — confirm it is
			// still exactly what it was before icons existed.
			toolsCaps, _ := caps["tools"].(map[string]any)
			if _, hasListChanged := toolsCaps["listChanged"]; !hasListChanged {
				t.Errorf("tools.listChanged missing for %s (unrelated to icons — must not regress)", version)
			}
			serverInfo, _ := result["serverInfo"].(map[string]any)
			icons, _ := serverInfo["icons"].([]any)
			if len(icons) != 1 {
				t.Errorf("serverInfo.icons = %v, want 1 entry even under %s", serverInfo["icons"], version)
			}

			// tools/list must also work normally, with the tool's own icon intact.
			listBody, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{}})
			listReq, _ := http.NewRequest(http.MethodPost, ts.URL, bytes.NewReader(listBody))
			listReq.Header.Set("Content-Type", "application/json")
			listReq.Header.Set(headerProtocolVersion, version)
			listResp, err := http.DefaultClient.Do(listReq)
			if err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			defer listResp.Body.Close()
			listRaw, _ := io.ReadAll(listResp.Body)
			var listOut map[string]any
			if err := json.Unmarshal(listRaw, &listOut); err != nil {
				t.Fatalf("decode tools/list: %v, body: %s", err, listRaw)
			}
			listResult, ok := listOut["result"].(map[string]any)
			if !ok {
				t.Fatalf("tools/list under %s: no result, body: %s", version, listRaw)
			}
			tools, _ := listResult["tools"].([]any)
			if len(tools) != 1 {
				t.Fatalf("tools/list under %s: expected 1 tool, got %#v", version, tools)
			}
			tool, _ := tools[0].(map[string]any)
			toolIcons, _ := tool["icons"].([]any)
			if len(toolIcons) != 1 {
				t.Errorf("tool icons under %s = %v, want 1 entry", version, tool["icons"])
			}
			// Legacy responses (any version) must never gain Modern-only fields.
			if _, hasResultType := tool["resultType"]; hasResultType {
				t.Errorf("Legacy tools/list under %s must not contain resultType", version)
			}
		})
	}
}

// --- Tool/resource/template/prompt icons ------------------------------

func TestIcons_ToolDescriptor(t *testing.T) {
	s := NewServer("icon-tools", "1")
	s.RegisterTool(NewTool("t", "a tool").Icons(testIcon), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	tools := s.ListToolsWithContext(context.Background())
	if len(tools) != 1 || len(tools[0].Icons) != 1 || !reflect.DeepEqual(tools[0].Icons[0], testIcon) {
		t.Fatalf("unexpected tools: %+v", tools)
	}
}

func TestIcons_ResourceDescriptor(t *testing.T) {
	s := NewServer("icon-resources", "1")
	s.RegisterResource(
		NewResource("file:///a.txt", "a", "a file", "text/plain").Icons(testIcon),
		func(ctx context.Context, req *ResourceRequest) (*ResourceResponse, error) {
			return NewResourceResponseText(req.URI(), "hi", "text/plain"), nil
		},
	)
	resources := s.ListResources(context.Background())
	if len(resources) != 1 || len(resources[0].Icons) != 1 || !reflect.DeepEqual(resources[0].Icons[0], testIcon) {
		t.Fatalf("unexpected resources: %+v", resources)
	}
}

func TestIcons_ResourceTemplateDescriptor(t *testing.T) {
	s := NewServer("icon-templates", "1")
	s.RegisterResourceTemplate(
		NewResourceTemplate("file:///{name}", "tmpl", "a template", "text/plain").Icons(testIcon),
		func(ctx context.Context, req *ResourceRequest) (*ResourceResponse, error) {
			return NewResourceResponseText(req.URI(), "hi", "text/plain"), nil
		},
	)
	templates := s.ListResourceTemplates(context.Background())
	if len(templates) != 1 || len(templates[0].Icons) != 1 || !reflect.DeepEqual(templates[0].Icons[0], testIcon) {
		t.Fatalf("unexpected templates: %+v", templates)
	}
}

func TestIcons_PromptDescriptor(t *testing.T) {
	s := NewServer("icon-prompts", "1")
	s.RegisterPrompt(
		NewPrompt("greet", "greets").Icons(testIcon),
		func(ctx context.Context, req *PromptRequest) (*PromptResponse, error) {
			return NewPromptResponseText("hi"), nil
		},
	)
	prompts := s.ListPrompts(context.Background())
	if len(prompts) != 1 || len(prompts[0].Icons) != 1 || !reflect.DeepEqual(prompts[0].Icons[0], testIcon) {
		t.Fatalf("unexpected prompts: %+v", prompts)
	}
}

// --- Server identity icons, Legacy ------------------------------------

func TestIcons_ServerIdentity_LegacyHTTP(t *testing.T) {
	s := NewServer("icon-server", "1")
	s.SetIcons(testIcon)
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	c := NewClient(ts.URL, nil, "")
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// Client doesn't expose serverInfo today, so hit the wire directly to
	// confirm the icons are actually present in the initialize response.
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	result, _ := out["result"].(map[string]any)
	serverInfo, _ := result["serverInfo"].(map[string]any)
	icons, _ := serverInfo["icons"].([]any)
	if len(icons) != 1 {
		t.Fatalf("expected 1 icon in serverInfo, got %#v", serverInfo)
	}
}

func TestIcons_ServerIdentity_LegacyStdio(t *testing.T) {
	s := NewServer("icon-server-stdio", "1")
	s.SetIcons(testIcon)

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = s.ServeStream(context.Background(), serverReader, serverWriter)
	}()
	transport := jsonrpc.NewStreamTransport(clientReader, clientWriter)
	rpc := jsonrpc.NewClient(transport)
	defer func() {
		clientWriter.Close()
		<-serveDone
		serverWriter.Close()
	}()

	var result map[string]any
	if err := rpc.Call(context.Background(), "initialize", map[string]any{
		"protocolVersion": MCPProtocolVersionLatest,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "t", "version": "1"},
	}, &result); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	serverInfo, _ := result["serverInfo"].(map[string]any)
	icons, _ := serverInfo["icons"].([]any)
	if len(icons) != 1 {
		t.Fatalf("expected 1 icon in serverInfo, got %#v", serverInfo)
	}
}

// --- Server identity icons, Modern -------------------------------------

func TestIcons_ServerIdentity_ModernDiscover(t *testing.T) {
	s := NewServer("icon-modern-discover", "1")
	s.SetIcons(testIcon)
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	req := modernRequest(t, ts.URL, "server/discover", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := decodeMCPResponse(t, body)
	result, _ := out["result"].(map[string]any)
	meta, _ := result["_meta"].(map[string]any)
	serverInfo, _ := meta[metaKeyServerInfo].(map[string]any)
	icons, _ := serverInfo["icons"].([]any)
	if len(icons) != 1 {
		t.Fatalf("expected 1 icon in _meta.serverInfo, got %#v", serverInfo)
	}
}

func TestIcons_ServerIdentity_ModernToolsList(t *testing.T) {
	s := NewServer("icon-modern-list", "1")
	s.SetIcons(testIcon)
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	req := modernRequest(t, ts.URL, "tools/list", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	out := decodeMCPResponse(t, body)
	result, _ := out["result"].(map[string]any)
	meta, _ := result["_meta"].(map[string]any)
	serverInfo, _ := meta[metaKeyServerInfo].(map[string]any)
	icons, _ := serverInfo["icons"].([]any)
	if len(icons) != 1 {
		t.Fatalf("expected 1 icon in _meta.serverInfo on tools/list, got %#v", serverInfo)
	}
}

func TestIcons_ServerIdentity_ModernStdio(t *testing.T) {
	s := NewServer("icon-modern-stdio", "1")
	s.SetIcons(testIcon)

	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = s.ServeStream(context.Background(), serverReader, serverWriter)
	}()
	transport := jsonrpc.NewStreamTransport(clientReader, clientWriter)
	rpc := jsonrpc.NewClient(transport)
	defer func() {
		clientWriter.Close()
		<-serveDone
		serverWriter.Close()
	}()

	var result map[string]any
	err := rpc.Call(context.Background(), "tools/list", map[string]any{
		"_meta": map[string]any{
			metaKeyProtocolVersion:    MCPProtocolVersionModern,
			metaKeyClientCapabilities: map[string]any{},
		},
	}, &result)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	meta, _ := result["_meta"].(map[string]any)
	serverInfo, _ := meta[metaKeyServerInfo].(map[string]any)
	icons, _ := serverInfo["icons"].([]any)
	if len(icons) != 1 {
		t.Fatalf("expected 1 icon in _meta.serverInfo, got %#v", serverInfo)
	}
}

// --- Regressions found and fixed while wiring Icons through --------------

// TestRefreshTools_PreservesMetaAndIcons is a regression test for a
// pre-existing bug found while adding Icons support: RefreshTools rebuilt
// the native tool cache from scratch but only copied Name/Description/
// InputSchema/OutputSchema, silently dropping _meta (including MCP Apps'
// _meta.ui.resourceUri) and, now, Icons on every refresh.
func TestRefreshTools_PreservesMetaAndIcons(t *testing.T) {
	s := NewServer("refresh-meta", "1")
	s.RegisterTool(
		NewTool("t", "a tool").Meta("custom", "value").Icons(testIcon),
		func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return NewToolResponseText("ok"), nil
		},
	)

	if err := s.RefreshTools(context.Background()); err != nil {
		t.Fatalf("RefreshTools: %v", err)
	}

	tools := s.ListToolsWithContext(context.Background())
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Meta["custom"] != "value" {
		t.Errorf("Meta lost after RefreshTools: %#v", tools[0].Meta)
	}
	if len(tools[0].Icons) != 1 || !reflect.DeepEqual(tools[0].Icons[0], testIcon) {
		t.Errorf("Icons lost after RefreshTools: %#v", tools[0].Icons)
	}
}

// TestShowAllDiscoverableTools_PreservesMetaAndIcons is a regression test for
// a second pre-existing instance of the same bug: the show-all tools/list
// path (which includes discoverable tools directly) also only copied
// Name/Description/InputSchema/OutputSchema.
func TestShowAllDiscoverableTools_PreservesMetaAndIcons(t *testing.T) {
	s := NewServer("showall-meta", "1")
	s.RegisterTool(
		NewTool("hidden", "a discoverable tool").Meta("custom", "value").Icons(testIcon).Discoverable("kw"),
		func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return NewToolResponseText("ok"), nil
		},
	)

	ctx := WithShowAllTools(context.Background())
	tools := s.ListToolsWithContext(ctx)
	var found *MCPTool
	for i := range tools {
		if tools[i].Name == "hidden" {
			found = &tools[i]
		}
	}
	if found == nil {
		t.Fatalf("expected to find the discoverable tool in show-all mode, got %+v", tools)
	}
	if found.Meta["custom"] != "value" {
		t.Errorf("Meta lost in show-all listing: %#v", found.Meta)
	}
	if len(found.Icons) != 1 || !reflect.DeepEqual(found.Icons[0], testIcon) {
		t.Errorf("Icons lost in show-all listing: %#v", found.Icons)
	}
}
