package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, url, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestToolsList_LinksDashboardAndSetsVisibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(newServer().HandleRequest))
	defer server.Close()

	result := post(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := result["result"].(map[string]any)["tools"].([]any)

	byName := map[string]map[string]any{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		byName[tool["name"].(string)] = tool
	}

	report, ok := byName["sales_report"]
	if !ok {
		t.Fatal("sales_report tool missing from tools/list")
	}
	reportMeta := report["_meta"].(map[string]any)["ui"].(map[string]any)
	if reportMeta["resourceUri"] != dashboardResourceURI {
		t.Errorf("sales_report resourceUri = %v", reportMeta["resourceUri"])
	}
	if _, hasVisibility := reportMeta["visibility"]; hasVisibility {
		t.Errorf("sales_report should omit visibility (spec default), got %v", reportMeta["visibility"])
	}
	icons, ok := report["icons"].([]any)
	if !ok || len(icons) != 1 {
		t.Fatalf("sales_report icons = %#v, want 1 entry", report["icons"])
	}
	icon, ok := icons[0].(map[string]any)
	if !ok || icon["mimeType"] != "image/svg+xml" {
		t.Errorf("sales_report icon = %#v", icon)
	}

	outputSchema, ok := report["outputSchema"].(map[string]any)
	if !ok {
		t.Fatalf("sales_report missing outputSchema, got %+v", report)
	}
	props, ok := outputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("outputSchema.properties missing: %+v", outputSchema)
	}
	records, ok := props["records"].(map[string]any)
	if !ok || records["type"] != "array" {
		t.Errorf("outputSchema.properties.records = %+v, want an array schema", props["records"])
	}

	addSale, ok := byName["add_sale"]
	if !ok {
		t.Fatal("add_sale tool missing from tools/list")
	}
	addSaleMeta := addSale["_meta"].(map[string]any)["ui"].(map[string]any)
	vis, _ := addSaleMeta["visibility"].([]any)
	if len(vis) != 1 || vis[0] != "app" {
		t.Errorf("add_sale visibility = %v, want [app]", addSaleMeta["visibility"])
	}
	// add_sale is only ever called by a dashboard that's already open, so it
	// has no view of its own to render — resourceUri is optional per spec and
	// must be omitted here, not just left equal to sales_report's.
	if _, hasResourceURI := addSaleMeta["resourceUri"]; hasResourceURI {
		t.Errorf("add_sale should omit resourceUri (app-only action tool, no view of its own), got %v", addSaleMeta["resourceUri"])
	}
	// Icons aren't required on every tool; add_sale intentionally has none,
	// demonstrating that only sales_report (the model-visible entry point)
	// needs one.
	if _, hasIcons := addSale["icons"]; hasIcons {
		t.Errorf("add_sale should have no icons, got %v", addSale["icons"])
	}
}

func TestResourcesRead_ReturnsHTMLWithCSP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(newServer().HandleRequest))
	defer server.Close()

	result := post(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"`+dashboardResourceURI+`"}}`)
	contents := result["result"].(map[string]any)["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("expected 1 content entry, got %d", len(contents))
	}
	c := contents[0].(map[string]any)
	if c["mimeType"] != "text/html;profile=mcp-app" {
		t.Errorf("mimeType = %v", c["mimeType"])
	}
	text, _ := c["text"].(string)
	if !strings.Contains(text, "<html") || !strings.Contains(text, "Sales Dashboard") {
		t.Errorf("expected dashboard HTML, got %d bytes", len(text))
	}
	if !strings.Contains(text, "alpinejs") || !strings.Contains(text, "chart.js") {
		t.Errorf("expected AlpineJS and Chart.js script tags in dashboard HTML")
	}
	csp := c["_meta"].(map[string]any)["ui"].(map[string]any)["csp"].(map[string]any)
	domains, _ := csp["resourceDomains"].([]any)
	if len(domains) != 1 || domains[0] != "https://cdn.jsdelivr.net" {
		t.Errorf("resourceDomains = %v", csp["resourceDomains"])
	}
}

func TestSalesReport_ReturnsSeedData(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(newServer().HandleRequest))
	defer server.Close()

	result := post(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"sales_report","arguments":{}}}`)
	structured := result["result"].(map[string]any)["structuredContent"].(map[string]any)
	records := structured["records"].([]any)
	if len(records) != 4 {
		t.Fatalf("expected 4 seed records, got %d", len(records))
	}
}

func TestAddSale_AppendsAndReturnsUpdatedRecords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(newServer().HandleRequest))
	defer server.Close()

	result := post(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_sale","arguments":{"date":"2026-09-15","product":"Widget","amount":99.99}}}`)
	structured := result["result"].(map[string]any)["structuredContent"].(map[string]any)
	records := structured["records"].([]any)
	if len(records) != 5 {
		t.Fatalf("expected 5 records after add, got %d", len(records))
	}
	last := records[4].(map[string]any)
	if last["product"] != "Widget" || last["amount"].(float64) != 99.99 {
		t.Errorf("last record = %v", last)
	}

	// A second read must see the same appended record (shared store).
	result2 := post(t, server.URL, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"sales_report","arguments":{}}}`)
	structured2 := result2["result"].(map[string]any)["structuredContent"].(map[string]any)
	if len(structured2["records"].([]any)) != 5 {
		t.Errorf("sales_report after add_sale should also see 5 records")
	}
}

// --- Unhappy paths ---

func TestAddSale_MissingRequiredParam(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(newServer().HandleRequest))
	defer server.Close()

	result := post(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_sale","arguments":{"date":"2026-09-15","product":"Widget"}}}`)
	// A missing argument is an input validation error: a tool execution
	// error (isError) the model can read, answered before the tool runs.
	res, _ := result["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("expected an isError result when amount is missing, got %v", result)
	}
}

func TestAddSale_WrongParamType(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(newServer().HandleRequest))
	defer server.Close()

	result := post(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"add_sale","arguments":{"date":"2026-09-15","product":"Widget","amount":"not-a-number"}}}`)
	// A value of the wrong JSON type fails the tool's inputSchema: an input
	// validation error, reported with isError before the tool runs.
	res, _ := result["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("expected an isError result when amount is not a number, got %v", result)
	}
}

func TestResourcesRead_UnknownURI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(newServer().HandleRequest))
	defer server.Close()

	result := post(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"ui://sales-dashboard/missing"}}`)
	if result["error"] == nil {
		t.Fatal("expected an error for an unregistered resource")
	}
}

func TestToolsCall_UnknownTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(newServer().HandleRequest))
	defer server.Close()

	result := post(t, server.URL, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"does_not_exist","arguments":{}}}`)
	if result["error"] == nil {
		t.Fatal("expected an error for an unknown tool")
	}
}
