package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- ExtensionCapability / SupportsUIApps: pure-function unit tests ---

func TestExtensionCapability_Present(t *testing.T) {
	caps := map[string]any{
		"extensions": map[string]any{
			UIAppsExtensionID: map[string]any{
				"mimeTypes": []any{UIAppMimeType},
			},
		},
	}
	settings, ok := ExtensionCapability(caps, UIAppsExtensionID)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if settings["mimeTypes"] == nil {
		t.Error("expected mimeTypes to be present in settings")
	}
}

func TestExtensionCapability_Unhappy(t *testing.T) {
	cases := []struct {
		name string
		caps map[string]any
	}{
		{"nil capabilities", nil},
		{"empty capabilities", map[string]any{}},
		{"extensions wrong type", map[string]any{"extensions": "not-a-map"}},
		{"extensions missing key", map[string]any{"extensions": map[string]any{}}},
		{"extension id wrong type", map[string]any{"extensions": map[string]any{UIAppsExtensionID: "not-a-map"}}},
		{"different extension only", map[string]any{"extensions": map[string]any{"some.other/ext": map[string]any{}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			settings, ok := ExtensionCapability(tc.caps, UIAppsExtensionID)
			if ok {
				t.Errorf("expected ok=false, got settings=%v", settings)
			}
			if settings != nil {
				t.Errorf("expected nil settings, got %v", settings)
			}
		})
	}
}

func TestSupportsUIApps_Happy(t *testing.T) {
	caps := map[string]any{
		"extensions": map[string]any{
			UIAppsExtensionID: map[string]any{
				"mimeTypes": []any{UIAppMimeType, "text/html"},
			},
		},
	}
	mimeTypes, ok := SupportsUIApps(caps)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(mimeTypes) != 2 || mimeTypes[0] != UIAppMimeType || mimeTypes[1] != "text/html" {
		t.Errorf("mimeTypes = %v, want [%q text/html]", mimeTypes, UIAppMimeType)
	}
}

func TestSupportsUIApps_Unhappy(t *testing.T) {
	t.Run("extension not declared", func(t *testing.T) {
		if _, ok := SupportsUIApps(map[string]any{}); ok {
			t.Error("expected ok=false when extension absent")
		}
	})
	t.Run("nil capabilities", func(t *testing.T) {
		if _, ok := SupportsUIApps(nil); ok {
			t.Error("expected ok=false for nil capabilities")
		}
	})
	t.Run("mimeTypes missing", func(t *testing.T) {
		caps := map[string]any{"extensions": map[string]any{UIAppsExtensionID: map[string]any{}}}
		mimeTypes, ok := SupportsUIApps(caps)
		if !ok {
			t.Fatal("expected ok=true even without mimeTypes (extension itself was declared)")
		}
		if len(mimeTypes) != 0 {
			t.Errorf("mimeTypes = %v, want empty", mimeTypes)
		}
	})
	t.Run("mimeTypes wrong element type", func(t *testing.T) {
		caps := map[string]any{"extensions": map[string]any{UIAppsExtensionID: map[string]any{
			"mimeTypes": []any{"text/html", 42, nil},
		}}}
		mimeTypes, ok := SupportsUIApps(caps)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if len(mimeTypes) != 1 || mimeTypes[0] != "text/html" {
			t.Errorf("mimeTypes = %v, want non-string entries skipped", mimeTypes)
		}
	})
}

// --- UIPermissions marshaling ---

func TestUIPermissions_MarshalJSON(t *testing.T) {
	t.Run("all requested", func(t *testing.T) {
		p := UIPermissions{Camera: true, Microphone: true, Geolocation: true, ClipboardWrite: true}
		b, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"camera", "microphone", "geolocation", "clipboardWrite"} {
			if _, ok := got[key]; !ok {
				t.Errorf("expected key %q to be present", key)
			}
		}
	})
	t.Run("none requested serializes to empty object", func(t *testing.T) {
		b, err := json.Marshal(UIPermissions{})
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "{}" {
			t.Errorf("got %s, want {}", b)
		}
	})
	t.Run("partial", func(t *testing.T) {
		b, _ := json.Marshal(UIPermissions{Camera: true})
		if !strings.Contains(string(b), "camera") || strings.Contains(string(b), "microphone") {
			t.Errorf("got %s, want only camera present", b)
		}
	})
}

// --- ToolBuilder.UIResource / Meta ---

func TestToolBuilder_UIResource(t *testing.T) {
	tool := NewTool("get_weather", "Get weather").UIResource("ui://weather/dashboard", "model", "app")
	mcpTool := tool.ToMCPTool()

	uiMeta, ok := mcpTool.Meta["ui"].(UIToolMeta)
	if !ok {
		t.Fatalf("Meta[ui] type = %T, want UIToolMeta", mcpTool.Meta["ui"])
	}
	if uiMeta.ResourceURI != "ui://weather/dashboard" {
		t.Errorf("ResourceURI = %q", uiMeta.ResourceURI)
	}
	if len(uiMeta.Visibility) != 2 || uiMeta.Visibility[0] != "model" || uiMeta.Visibility[1] != "app" {
		t.Errorf("Visibility = %v", uiMeta.Visibility)
	}

	// Verify wire shape: _meta.ui.resourceUri / visibility.
	b, err := json.Marshal(mcpTool)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	meta, ok := wire["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta missing or wrong type in %s", b)
	}
	ui, ok := meta["ui"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.ui missing or wrong type in %s", b)
	}
	if ui["resourceUri"] != "ui://weather/dashboard" {
		t.Errorf("_meta.ui.resourceUri = %v", ui["resourceUri"])
	}
}

func TestToolBuilder_UIResource_DefaultVisibilityOmitted(t *testing.T) {
	tool := NewTool("t", "d").UIResource("ui://x")
	b, _ := json.Marshal(tool.ToMCPTool())
	if strings.Contains(string(b), "visibility") {
		t.Errorf("expected visibility to be omitted when not supplied, got %s", b)
	}
}

// TestToolBuilder_Visibility_OmitsResourceURI covers the app-only "action"
// tool case (e.g. claim_prize in the prize-wheel example): a tool that's only
// ever called by an already-open view has no rendering purpose of its own,
// so [ToolBuilder.Visibility] must not emit resourceUri at all — a host that
// enumerates _meta.ui.resourceUri to find "apps" shouldn't mistake this for
// one.
func TestToolBuilder_Visibility_OmitsResourceURI(t *testing.T) {
	tool := NewTool("claim_prize", "Claim a prize").Visibility("app")
	mcpTool := tool.ToMCPTool()

	uiMeta, ok := mcpTool.Meta["ui"].(UIToolMeta)
	if !ok {
		t.Fatalf("Meta[ui] type = %T, want UIToolMeta", mcpTool.Meta["ui"])
	}
	if uiMeta.ResourceURI != "" {
		t.Errorf("ResourceURI = %q, want empty", uiMeta.ResourceURI)
	}
	if len(uiMeta.Visibility) != 1 || uiMeta.Visibility[0] != "app" {
		t.Errorf("Visibility = %v", uiMeta.Visibility)
	}

	b, err := json.Marshal(mcpTool)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "resourceUri") {
		t.Errorf("expected resourceUri to be omitted entirely, got %s", b)
	}
	var wire map[string]any
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	meta, _ := wire["_meta"].(map[string]any)
	ui, ok := meta["ui"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.ui missing or wrong type in %s", b)
	}
	if visibility, _ := ui["visibility"].([]any); len(visibility) != 1 || visibility[0] != "app" {
		t.Errorf("_meta.ui.visibility = %v", ui["visibility"])
	}
}

// TestToolBuilder_UIResource_PanicsOnInvalidInput and its sibling below pin
// down that a typo in a direct Go call — a misspelled visibility value or a
// resourceURI with no scheme — panics rather than silently reaching the
// wire, where a host checking for exactly "app"/"model" would just never
// grant the access the caller intended. This is deliberately a panic, not
// an error return: it's a Go call-site mistake, not bad external data (see
// toolmetadata.BuildMCPTool's tests for the data-driven case, which
// recovers this same panic into an ordinary error instead).
func TestToolBuilder_UIResource_PanicsOnInvalidInput(t *testing.T) {
	assertPanics := func(t *testing.T, f func()) {
		t.Helper()
		defer func() {
			if recover() == nil {
				t.Error("expected a panic, got none")
			}
		}()
		f()
	}

	t.Run("empty resourceURI", func(t *testing.T) {
		assertPanics(t, func() { NewTool("t", "d").UIResource("") })
	})
	t.Run("resourceURI with no scheme", func(t *testing.T) {
		assertPanics(t, func() { NewTool("t", "d").UIResource("dashboard/sales") })
	})
	// Regression test: an absolute URI with SOME scheme used to pass
	// validation as long as it had one at all — https://, file://, even a
	// typo'd "iu://" all slipped through — even though a host only ever
	// recognizes "ui://" as a linked view per the MCP Apps extension, so
	// anything else would just silently never render.
	t.Run("resourceURI with an https scheme", func(t *testing.T) {
		assertPanics(t, func() { NewTool("t", "d").UIResource("https://dashboard/sales") })
	})
	t.Run("resourceURI with a typo'd scheme", func(t *testing.T) {
		assertPanics(t, func() { NewTool("t", "d").UIResource("iu://dashboard/sales") })
	})
	t.Run("invalid visibility via UIResource", func(t *testing.T) {
		assertPanics(t, func() { NewTool("t", "d").UIResource("ui://x/y", "appp") })
	})
	t.Run("invalid visibility via Visibility", func(t *testing.T) {
		assertPanics(t, func() { NewTool("t", "d").Visibility("appp") })
	})
	t.Run("valid input does not panic", func(t *testing.T) {
		NewTool("t", "d").UIResource("ui://x/y", "model", "app")
		NewTool("t", "d").Visibility("model")
	})
}

// TestToolBuilder_UIResourceThenVisibility_Compose and its sibling below pin
// down that UIResource and Visibility read back whatever the other already
// set on the same tool, rather than one wholesale-overwriting the "ui" meta
// value the other just set — a caller reasonably expecting these two
// builder methods to compose (UIResource's own visibility parameter covers
// the common case, but nothing stops chaining Visibility afterward to
// change it) would otherwise silently lose whichever field the first call
// set.
func TestToolBuilder_UIResourceThenVisibility_Compose(t *testing.T) {
	tool := NewTool("t", "d").UIResource("ui://x/y").Visibility("app")
	uiMeta, ok := tool.ToMCPTool().Meta["ui"].(UIToolMeta)
	if !ok {
		t.Fatalf("Meta[ui] type = %T, want UIToolMeta", tool.ToMCPTool().Meta["ui"])
	}
	if uiMeta.ResourceURI != "ui://x/y" {
		t.Errorf("ResourceURI = %q, want %q (should survive the later Visibility call)", uiMeta.ResourceURI, "ui://x/y")
	}
	if len(uiMeta.Visibility) != 1 || uiMeta.Visibility[0] != "app" {
		t.Errorf("Visibility = %v, want [app]", uiMeta.Visibility)
	}
}

func TestToolBuilder_VisibilityThenUIResource_Compose(t *testing.T) {
	tool := NewTool("t", "d").Visibility("app").UIResource("ui://x/y")
	uiMeta, ok := tool.ToMCPTool().Meta["ui"].(UIToolMeta)
	if !ok {
		t.Fatalf("Meta[ui] type = %T, want UIToolMeta", tool.ToMCPTool().Meta["ui"])
	}
	if uiMeta.ResourceURI != "ui://x/y" {
		t.Errorf("ResourceURI = %q, want %q", uiMeta.ResourceURI, "ui://x/y")
	}
	// UIResource was called here with no visibility argument of its own, so
	// it must preserve the earlier Visibility("app") call rather than
	// clearing it.
	if len(uiMeta.Visibility) != 1 || uiMeta.Visibility[0] != "app" {
		t.Errorf("Visibility = %v, want [app] (should survive the later UIResource call)", uiMeta.Visibility)
	}
}

// TestVisibilityOnlyTool_WorksAcrossOlderLegacyProtocolVersions is a direct
// backward-compatibility check for the resourceUri-optional fix (the other
// change alongside Icons this session): a visibility-only app-action tool
// (no UIResource) must round-trip correctly through tools/list under every
// prior Legacy protocol version, not just the latest.
func TestVisibilityOnlyTool_WorksAcrossOlderLegacyProtocolVersions(t *testing.T) {
	s := NewServer("visibility-version-compat", "1")
	s.RegisterTool(NewTool("claim_prize", "Claim a prize").Visibility("app"), func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
		return NewToolResponseText("ok"), nil
	})
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
			req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(headerProtocolVersion, version)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("tools/list: %v", err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				t.Fatalf("decode: %v, body: %s", err, raw)
			}
			result, ok := out["result"].(map[string]any)
			if !ok {
				t.Fatalf("no result under %s: %s", version, raw)
			}
			tools, _ := result["tools"].([]any)
			if len(tools) != 1 {
				t.Fatalf("expected 1 tool under %s, got %#v", version, tools)
			}
			tool, _ := tools[0].(map[string]any)
			meta, _ := tool["_meta"].(map[string]any)
			ui, ok := meta["ui"].(map[string]any)
			if !ok {
				t.Fatalf("missing _meta.ui under %s: %#v", version, tool)
			}
			if _, hasResourceURI := ui["resourceUri"]; hasResourceURI {
				t.Errorf("resourceUri present under %s, want omitted: %v", version, ui["resourceUri"])
			}
			vis, _ := ui["visibility"].([]any)
			if len(vis) != 1 || vis[0] != "app" {
				t.Errorf("visibility under %s = %v, want [app]", version, ui["visibility"])
			}
		})
	}
}

func TestToolBuilder_NoMeta_OmitsField(t *testing.T) {
	tool := NewTool("plain", "no ui metadata")
	b, err := json.Marshal(tool.ToMCPTool())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "_meta") {
		t.Errorf("expected no _meta field for a tool with no metadata, got %s", b)
	}
}

func TestToolBuilder_Meta_Generic(t *testing.T) {
	tool := NewTool("t", "d").Meta("custom", 42)
	mcpTool := tool.ToMCPTool()
	if mcpTool.Meta["custom"] != 42 {
		t.Errorf("Meta[custom] = %v, want 42", mcpTool.Meta["custom"])
	}
}

// --- ResourceBuilder / ResourceTemplateBuilder UIMeta ---

func TestResourceBuilder_UIMeta(t *testing.T) {
	prefersBorder := true
	rb := NewResource("ui://dash", "Dashboard", "", UIAppMimeType).UIMeta(UIResourceMeta{
		PrefersBorder: &prefersBorder,
		CSP:           &UICSP{ResourceDomains: []string{"https://cdn.jsdelivr.net"}},
	})
	res := rb.ToMCPResource()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	json.Unmarshal(b, &wire)
	meta, ok := wire["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta missing in %s", b)
	}
	ui, ok := meta["ui"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.ui missing in %s", b)
	}
	if ui["prefersBorder"] != true {
		t.Errorf("prefersBorder = %v", ui["prefersBorder"])
	}
	csp, ok := ui["csp"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.ui.csp missing in %s", b)
	}
	domains, ok := csp["resourceDomains"].([]any)
	if !ok || len(domains) != 1 || domains[0] != "https://cdn.jsdelivr.net" {
		t.Errorf("resourceDomains = %v", csp["resourceDomains"])
	}
}

func TestResourceBuilder_NoMeta_OmitsField(t *testing.T) {
	rb := NewResource("config://app", "App Config", "", "application/json")
	b, _ := json.Marshal(rb.ToMCPResource())
	if strings.Contains(string(b), "_meta") {
		t.Errorf("expected no _meta field, got %s", b)
	}
}

func TestResourceTemplateBuilder_UIMeta(t *testing.T) {
	tb := NewResourceTemplate("ui://dash/{id}", "Dashboard", "", UIAppMimeType).
		UIMeta(UIResourceMeta{Domain: "example.claudemcpcontent.com"})
	tmpl := tb.ToMCPResourceTemplate()
	uiMeta, ok := tmpl.Meta["ui"].(UIResourceMeta)
	if !ok {
		t.Fatalf("Meta[ui] type = %T", tmpl.Meta["ui"])
	}
	if uiMeta.Domain != "example.claudemcpcontent.com" {
		t.Errorf("Domain = %q", uiMeta.Domain)
	}
}

// --- NewUIResourceResponseText / WithMeta ---

func TestNewUIResourceResponseText_NoMeta(t *testing.T) {
	resp := NewUIResourceResponseText("ui://dash", "<html></html>", nil)
	if len(resp.Contents) != 1 {
		t.Fatalf("Contents len = %d, want 1", len(resp.Contents))
	}
	c := resp.Contents[0]
	if c.MimeType != UIAppMimeType {
		t.Errorf("MimeType = %q, want %q", c.MimeType, UIAppMimeType)
	}
	if c.Text != "<html></html>" {
		t.Errorf("Text = %q", c.Text)
	}
	if c.Meta != nil {
		t.Errorf("Meta = %v, want nil", c.Meta)
	}
	b, _ := json.Marshal(resp)
	if strings.Contains(string(b), "_meta") {
		t.Errorf("expected no _meta in wire output, got %s", b)
	}
}

func TestNewUIResourceResponseText_WithCSP(t *testing.T) {
	resp := NewUIResourceResponseText("ui://dash", "<html></html>", &UIResourceMeta{
		CSP: &UICSP{ConnectDomains: []string{"https://api.example.com"}},
	})
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Contents []struct {
			Meta struct {
				UI struct {
					CSP struct {
						ConnectDomains []string `json:"connectDomains"`
					} `json:"csp"`
				} `json:"ui"`
			} `json:"_meta"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, b)
	}
	if len(wire.Contents) != 1 || len(wire.Contents[0].Meta.UI.CSP.ConnectDomains) != 1 ||
		wire.Contents[0].Meta.UI.CSP.ConnectDomains[0] != "https://api.example.com" {
		t.Errorf("unexpected wire shape: %s", b)
	}
}

func TestResourceResponse_WithMeta_EmptyContents(t *testing.T) {
	rr := &ResourceResponse{}
	rr.WithMeta(map[string]any{"ui": UIResourceMeta{Domain: "x"}})
	if len(rr.Contents) != 1 {
		t.Fatalf("expected WithMeta to create a content entry when none exists, got %d", len(rr.Contents))
	}
}

// --- Server.ClientCapabilities / DeclareExtension: end-to-end over HTTP ---

func TestServer_ClientCapabilities_CapturedFromInitialize(t *testing.T) {
	s := NewServer("test", "1.0")
	if got := s.ClientCapabilities(); got != nil {
		t.Errorf("ClientCapabilities before initialize = %v, want nil", got)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"2025-06-18",
		"capabilities":{"extensions":{"` + UIAppsExtensionID + `":{"mimeTypes":["` + UIAppMimeType + `"]}}},
		"clientInfo":{"name":"test-client","version":"1.0"}
	}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.HandleRequest(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	mimeTypes, ok := SupportsUIApps(s.ClientCapabilities())
	if !ok {
		t.Fatalf("expected SupportsUIApps to be true after initialize, capabilities = %v", s.ClientCapabilities())
	}
	if len(mimeTypes) != 1 || mimeTypes[0] != UIAppMimeType {
		t.Errorf("mimeTypes = %v", mimeTypes)
	}
}

func TestServer_ClientCapabilities_UnhappyNoExtensions(t *testing.T) {
	s := NewServer("test", "1.0")
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"2025-06-18",
		"capabilities":{},
		"clientInfo":{"name":"test-client","version":"1.0"}
	}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.HandleRequest(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if _, ok := SupportsUIApps(s.ClientCapabilities()); ok {
		t.Error("expected SupportsUIApps=false when client declared no extensions")
	}
}

// TestClient_DeclareExtension_ReachesServer_Modern and its Legacy sibling
// below are the client-side counterpart of
// TestServer_ClientCapabilities_CapturedFromInitialize: without
// Client.DeclareExtension, every request this Client sends declares empty
// capabilities, so a remote server that conditionally attaches
// extension-specific data (e.g. MCP Apps' _meta.ui, only for clients that
// declared io.modelcontextprotocol/ui support) has no way to know this
// client can use it and silently falls back to a plain response —
// federated MCP Apps views never render, with no error anywhere. Both
// tests use the *server's* own ClientCapabilities()/SupportsUIApps to
// observe what actually reached it, proving the declaration round-trips
// over the wire, not just that the Client stores it locally.
func TestClient_DeclareExtension_ReachesServer_Modern(t *testing.T) {
	s := NewServer("modern-remote", "1")
	ts := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer ts.Close()

	client := NewClient(ts.URL, nil, "")
	client.DeclareExtension(UIAppsExtensionID, map[string]any{"mimeTypes": []string{UIAppMimeType}})
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	mimeTypes, ok := SupportsUIApps(s.ClientCapabilities())
	if !ok {
		t.Fatalf("expected SupportsUIApps=true after a Modern client declared the extension, capabilities = %v", s.ClientCapabilities())
	}
	if len(mimeTypes) != 1 || mimeTypes[0] != UIAppMimeType {
		t.Errorf("mimeTypes = %v", mimeTypes)
	}
}

func TestClient_DeclareExtension_ReachesServer_Legacy(t *testing.T) {
	s := NewServer("legacy-remote", "1")
	ts := legacyOnlyServer(t, s)
	defer ts.Close()

	client := NewClient(ts.URL, nil, "")
	client.DeclareExtension(UIAppsExtensionID, map[string]any{"mimeTypes": []string{UIAppMimeType}})
	if err := client.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	mimeTypes, ok := SupportsUIApps(s.ClientCapabilities())
	if !ok {
		t.Fatalf("expected SupportsUIApps=true after a Legacy client declared the extension, capabilities = %v", s.ClientCapabilities())
	}
	if len(mimeTypes) != 1 || mimeTypes[0] != UIAppMimeType {
		t.Errorf("mimeTypes = %v", mimeTypes)
	}
}

func TestServer_DeclareExtension_AppearsInInitializeResponse(t *testing.T) {
	s := NewServer("test", "1.0")
	s.DeclareExtension(UIAppsExtensionID, map[string]any{"mimeTypes": []string{UIAppMimeType}})

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.HandleRequest(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	result := resp["result"].(map[string]any)
	caps := result["capabilities"].(map[string]any)
	extensions, ok := caps["extensions"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities.extensions missing: %s", rr.Body.String())
	}
	uiExt, ok := extensions[UIAppsExtensionID].(map[string]any)
	if !ok {
		t.Fatalf("capabilities.extensions[%s] missing: %s", UIAppsExtensionID, rr.Body.String())
	}
	if mts, _ := uiExt["mimeTypes"].([]any); len(mts) != 1 || mts[0] != UIAppMimeType {
		t.Errorf("mimeTypes = %v", uiExt["mimeTypes"])
	}
}

func TestServer_NoDeclareExtension_OmitsExtensionsField(t *testing.T) {
	s := NewServer("test", "1.0")
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.HandleRequest(rr, req)

	if strings.Contains(rr.Body.String(), "extensions") {
		t.Errorf("expected no extensions field when none declared, got %s", rr.Body.String())
	}
}

// --- Full MCP Apps flow, end to end: register, list, read ---

func TestMCPApps_FullFlow(t *testing.T) {
	s := NewServer("apps-test", "1.0")
	s.DeclareExtension(UIAppsExtensionID, map[string]any{"mimeTypes": []string{UIAppMimeType}})

	s.RegisterResource(
		NewResource("ui://dash/sales", "Sales Dashboard", "Interactive sales view", UIAppMimeType),
		func(ctx context.Context, req *ResourceRequest) (*ResourceResponse, error) {
			return NewUIResourceResponseText(req.URI(), "<html><body>dashboard</body></html>", &UIResourceMeta{
				CSP: &UICSP{ResourceDomains: []string{"https://cdn.jsdelivr.net"}},
			}), nil
		},
	)

	s.RegisterTool(
		NewTool("sales_report", "Get the sales report").UIResource("ui://dash/sales"),
		func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return NewToolResponseJSON(map[string]any{"total": 1000}), nil
		},
	)

	server := httptest.NewServer(http.HandlerFunc(s.HandleRequest))
	defer server.Close()

	// tools/list must carry _meta.ui.resourceUri.
	listBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	resp, err := http.Post(server.URL, "application/json", strings.NewReader(listBody))
	if err != nil {
		t.Fatal(err)
	}
	var listResult struct {
		Result struct {
			Tools []struct {
				Name string         `json:"name"`
				Meta map[string]any `json:"_meta"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResult); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if len(listResult.Result.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(listResult.Result.Tools))
	}
	ui, ok := listResult.Result.Tools[0].Meta["ui"].(map[string]any)
	if !ok {
		t.Fatalf("tool _meta.ui missing: %+v", listResult.Result.Tools[0])
	}
	if ui["resourceUri"] != "ui://dash/sales" {
		t.Errorf("resourceUri = %v", ui["resourceUri"])
	}

	// resources/read must return the HTML with the declared CSP.
	readBody := `{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"ui://dash/sales"}}`
	resp2, err := http.Post(server.URL, "application/json", strings.NewReader(readBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var readResult struct {
		Result struct {
			Contents []struct {
				URI      string         `json:"uri"`
				MimeType string         `json:"mimeType"`
				Text     string         `json:"text"`
				Meta     map[string]any `json:"_meta"`
			} `json:"contents"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&readResult); err != nil {
		t.Fatal(err)
	}
	if len(readResult.Result.Contents) != 1 {
		t.Fatalf("expected 1 content entry, got %d", len(readResult.Result.Contents))
	}
	c := readResult.Result.Contents[0]
	if c.MimeType != UIAppMimeType {
		t.Errorf("MimeType = %q, want %q", c.MimeType, UIAppMimeType)
	}
	if !strings.Contains(c.Text, "dashboard") {
		t.Errorf("Text = %q", c.Text)
	}
	uiMeta, ok := c.Meta["ui"].(map[string]any)
	if !ok {
		t.Fatalf("resource _meta.ui missing: %+v", c)
	}
	csp, ok := uiMeta["csp"].(map[string]any)
	if !ok {
		t.Fatalf("resource _meta.ui.csp missing: %+v", uiMeta)
	}
	if domains, _ := csp["resourceDomains"].([]any); len(domains) != 1 || domains[0] != "https://cdn.jsdelivr.net" {
		t.Errorf("resourceDomains = %v", csp["resourceDomains"])
	}

	// Unhappy path: reading an unregistered ui:// URI must still fail cleanly.
	badReadBody := `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"ui://dash/missing"}}`
	resp3, err := http.Post(server.URL, "application/json", strings.NewReader(badReadBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	var errResult struct {
		Error *MCPError `json:"error"`
	}
	if err := json.NewDecoder(resp3.Body).Decode(&errResult); err != nil {
		t.Fatal(err)
	}
	if errResult.Error == nil {
		t.Error("expected an error for an unregistered ui:// resource")
	}
}

// TestMCPApps_FullFlow_Stdio exercises the same registration over the stdio
// transport, which has its own initialize/resources/read code paths distinct
// from the HTTP handlers exercised above.
func TestMCPApps_FullFlow_Stdio(t *testing.T) {
	s := NewServer("apps-stdio-test", "1.0")
	s.RegisterResource(
		NewResource("ui://dash/sales", "Sales Dashboard", "", UIAppMimeType),
		func(ctx context.Context, req *ResourceRequest) (*ResourceResponse, error) {
			return NewUIResourceResponseText(req.URI(), "<html></html>", nil), nil
		},
	)
	s.RegisterTool(
		NewTool("sales_report", "Get the sales report").UIResource("ui://dash/sales", "model", "app"),
		func(ctx context.Context, req *ToolRequest) (*ToolResponse, error) {
			return NewToolResponseText("ok"), nil
		},
	)

	clientReader, serverWriter := io.Pipe() // server -> client
	serverReader, clientWriter := io.Pipe() // client -> server

	done := make(chan struct{})
	go func() {
		_ = s.ServeStream(context.Background(), serverReader, serverWriter)
		close(done)
	}()

	send := func(payload string) {
		if _, err := clientWriter.Write([]byte(payload + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	dec := json.NewDecoder(clientReader)

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{"extensions":{"` + UIAppsExtensionID + `":{"mimeTypes":["` + UIAppMimeType + `"]}}},"clientInfo":{"name":"c","version":"1"}}}`)
	var initResp map[string]any
	if err := dec.Decode(&initResp); err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}

	if mimeTypes, ok := SupportsUIApps(s.ClientCapabilities()); !ok || len(mimeTypes) != 1 {
		t.Errorf("stdio ClientCapabilities not captured: %v ok=%v", s.ClientCapabilities(), ok)
	}

	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var listResp struct {
		Result struct {
			Tools []MCPTool `json:"tools"`
		} `json:"result"`
	}
	if err := dec.Decode(&listResp); err != nil {
		t.Fatalf("decode tools/list response: %v", err)
	}
	if len(listResp.Result.Tools) != 1 {
		t.Fatalf("expected 1 tool over stdio, got %d", len(listResp.Result.Tools))
	}
	ui, ok := listResp.Result.Tools[0].Meta["ui"].(map[string]any)
	if !ok {
		t.Fatalf("stdio tool _meta.ui missing: %+v", listResp.Result.Tools[0])
	}
	if ui["resourceUri"] != "ui://dash/sales" {
		t.Errorf("resourceUri = %v", ui["resourceUri"])
	}

	clientWriter.Close()
	<-done
}
