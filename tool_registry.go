package mcp

import (
	"context"
	"sort"
	"strings"
	"sync"

	"github.com/paularlott/cli/fuzzy"
)

// SearchResult represents a tool found via search
type SearchResult struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Score       float64        `json:"score"`
	InputSchema any            `json:"inputSchema,omitempty"`
	Keywords    []string       `json:"keywords,omitempty"`
	Meta        map[string]any `json:"_meta,omitempty"` // Extension metadata, e.g. MCP Apps' ui.resourceUri
	Icons       []Icon         `json:"icons,omitempty"`
}

// internalRegistry implements ToolRegistry and provides tool search functionality
type internalRegistry struct {
	mu    sync.RWMutex
	tools map[string]*internalRegisteredTool
}

// internalRegisteredTool holds a tool registered with the internal registry
type internalRegisteredTool struct {
	tool     *MCPTool
	keywords []string
	handler  ToolHandler
}

// newInternalRegistry creates a new internal tool registry
func newInternalRegistry() *internalRegistry {
	return &internalRegistry{
		tools: make(map[string]*internalRegisteredTool),
	}
}

// RegisterTool registers a searchable tool
func (r *internalRegistry) RegisterTool(tool *ToolBuilder, handler ToolHandler, keywords ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	schema := tool.BuildSchema()
	outputSchema := tool.BuildOutputSchema()

	mcpTool := &MCPTool{
		Name:        tool.Name(),
		Description: tool.Description(),
		InputSchema: schema,
		Meta:        tool.meta,
		Icons:       tool.icons,
		Keywords:    keywords,
	}
	if outputSchema != nil {
		mcpTool.OutputSchema = outputSchema
	}

	r.tools[tool.Name()] = &internalRegisteredTool{
		tool:     mcpTool,
		keywords: keywords,
		handler:  handler,
	}
}

// RegisterMCPTool registers an already-built MCPTool
func (r *internalRegistry) RegisterMCPTool(tool *MCPTool, handler ToolHandler, keywords ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Combine provided keywords with any already on the tool
	allKeywords := append([]string{}, tool.Keywords...)
	allKeywords = append(allKeywords, keywords...)

	r.tools[tool.Name] = &internalRegisteredTool{
		tool:     tool,
		keywords: allKeywords,
		handler:  handler,
	}
}

// UnregisterTool removes a tool by name from the registry.
func (r *internalRegistry) UnregisterTool(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tools, name)
}

// GetRegisteredTools returns all tools in the registry
func (r *internalRegistry) GetRegisteredTools() []MCPTool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]MCPTool, 0, len(r.tools))
	for _, rt := range r.tools {
		tools = append(tools, *rt.tool)
	}
	return tools
}

// Search finds tools matching the query

// SearchWithAdditionalTools finds tools matching the query, including additional tools passed in.
// The additional tools are typically discoverable tools from providers.
// listedTools are additional searchable tools that may already appear in tools/list.
func (r *internalRegistry) SearchWithAdditionalTools(ctx context.Context, query string, maxResults int, additionalTools []MCPTool, listedTools []MCPTool) []SearchResult {
	r.mu.RLock()
	toolsCopy := make(map[string]*internalRegisteredTool, len(r.tools))
	for k, v := range r.tools {
		toolsCopy[k] = v
	}
	r.mu.RUnlock()

	var results []SearchResult
	queryLower := strings.ToLower(strings.TrimSpace(query))
	listAll := queryLower == ""
	seen := make(map[string]bool)

	// Search registered tools (statically registered discoverable tools)
	for _, dt := range toolsCopy {
		var score float64
		if listAll {
			score = 1.0
		} else {
			score = calculateScore(queryLower, dt.tool.Name, dt.tool.Description, dt.keywords)
		}
		if score > 0 {
			results = append(results, SearchResult{
				Name:        dt.tool.Name,
				Description: dt.tool.Description,
				Score:       score,
				InputSchema: dt.tool.InputSchema,
				Keywords:    dt.keywords,
				Meta:        dt.tool.Meta,
				Icons:       dt.tool.Icons,
			})
			seen[dt.tool.Name] = true
		}
	}

	// Search additional tools (discoverable tools from providers)
	for _, tool := range additionalTools {
		if seen[tool.Name] {
			continue
		}
		var score float64
		if listAll {
			score = 1.0
		} else {
			score = calculateScore(queryLower, tool.Name, tool.Description, tool.Keywords)
		}
		if score > 0 {
			results = append(results, SearchResult{
				Name:        tool.Name,
				Description: tool.Description,
				Score:       score,
				InputSchema: tool.InputSchema,
				Keywords:    tool.Keywords,
				Meta:        tool.Meta,
				Icons:       tool.Icons,
			})
			seen[tool.Name] = true
		}
	}

	// Search listed tools
	for _, tool := range listedTools {
		if seen[tool.Name] {
			continue
		}
		var score float64
		if listAll {
			score = 1.0
		} else {
			score = calculateScore(queryLower, tool.Name, tool.Description, tool.Keywords)
		}
		if score > 0 {
			results = append(results, SearchResult{
				Name:        tool.Name,
				Description: tool.Description,
				Score:       score,
				InputSchema: tool.InputSchema,
				Keywords:    tool.Keywords,
				Meta:        tool.Meta,
				Icons:       tool.Icons,
			})
			seen[tool.Name] = true
		}
	}

	// Sort by score descending, then by name
	sort.Slice(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Name < results[j].Name
	})

	if maxResults > 0 && len(results) > maxResults {
		results = results[:maxResults]
	}

	return results
}

// GetTool retrieves a tool by name

// CallTool executes a tool by name

// Search scoring functions

func calculateScore(queryLower, name, description string, keywords []string) float64 {
	nameLower := strings.ToLower(name)
	descLower := strings.ToLower(description)

	if nameLower == queryLower {
		return 1.0
	}

	queryWords := strings.Fields(queryLower)
	if len(queryWords) <= 1 {
		return calculateSingleWordScore(queryLower, nameLower, descLower, keywords)
	}

	var totalScore float64
	matchedWords := 0

	for _, word := range queryWords {
		wordScore := calculateSingleWordScore(word, nameLower, descLower, keywords)
		if wordScore > 0 {
			matchedWords++
			totalScore += wordScore
		}
	}

	if matchedWords == 0 {
		return 0
	}

	// Average across ALL query words: unmatched words contribute 0, which
	// naturally penalises partial matches in proportion to how much of the
	// query was understood. No additional ratio multiplier is applied — that
	// would double-count the miss. A clean full match returns its true
	// average without an artificial penalty.
	return totalScore / float64(len(queryWords))
}

func calculateSingleWordScore(word, nameLower, descLower string, keywords []string) float64 {
	var score float64

	if strings.HasPrefix(nameLower, word) {
		score = max(score, 0.95)
	}

	// Tokenise the tool name on common separators so a query word matches a
	// segment of a multi-word tool name as a real word, not just a substring.
	// e.g. query 'send' against name 'tools_send_email' should score as a
	// token match. The tool's own name is the strongest signal of intent, so
	// these tiers sit above keyword-tag matches.
	for _, token := range nameTokens(nameLower) {
		if token == word {
			score = max(score, 0.92)
		} else if strings.HasPrefix(token, word) {
			score = max(score, 0.9)
		}
	}

	if strings.Contains(nameLower, word) {
		score = max(score, 0.8)
	}

	for _, kw := range keywords {
		kwLower := strings.ToLower(kw)
		if kwLower == word {
			score = max(score, 0.85)
			continue
		}

		// Tokenise multi-word keyword phrases (e.g. 'work request' becomes
		// ['work', 'request']) so an individual word in a phrase is still a
		// strong match. A developer tagging 'work request' is also tagging
		// the topic 'work' and the topic 'request'.
		matchedToken := false
		for _, kwToken := range strings.Fields(kwLower) {
			if kwToken == word {
				score = max(score, 0.85)
				matchedToken = true
				break
			}
		}
		if matchedToken {
			continue
		}

		if strings.Contains(kwLower, word) {
			score = max(score, 0.7)
		}
	}

	if containsWord(descLower, word) {
		score = max(score, 0.6)
	} else if strings.Contains(descLower, word) {
		score = max(score, 0.5)
	}

	if score == 0 {
		if fuzzyScore := fuzzy.Score(word, nameLower); fuzzyScore > 0.6 {
			score = max(score, fuzzyScore*0.7)
		}

		for _, kw := range keywords {
			if fuzzyScore := fuzzy.Score(word, strings.ToLower(kw)); fuzzyScore > 0.6 {
				score = max(score, fuzzyScore*0.6)
			}
		}
	}

	return score
}

// nameTokens splits a tool name on common separators (_ and -) into its
// constituent words. Used for token-aware matching in tool_search so that a
// query word matches a segment of a multi-word tool name as a real word
// rather than a raw substring.
func nameTokens(nameLower string) []string {
	return strings.FieldsFunc(nameLower, func(r rune) bool {
		return r == '_' || r == '-'
	})
}

func containsWord(text, query string) bool {
	words := strings.Fields(text)
	for _, word := range words {
		word = strings.Trim(word, ".,;:!?()[]{}\"'")
		if strings.ToLower(word) == query {
			return true
		}
	}
	return false
}

// validateRequiredParameters checks a call's arguments against the tool's
// whole inputSchema (validateJSONSchema): required properties, every
// property's type and constraints, nested objects and arrays, enums and
// additionalProperties. A violation is a malformed request (-32602) that
// never reaches the handler, as the tools spec's "validate all tool
// inputs" asks; the error names the offending argument.
func validateRequiredParameters(inputSchema any, args map[string]any) error {
	if _, ok := inputSchema.(map[string]any); !ok {
		return nil
	}
	value := map[string]any{}
	for k, v := range args {
		value[k] = v
	}
	if err := validateJSONSchema(inputSchema, value); err != nil {
		msg := err.Error()
		if se, ok := err.(schemaError); ok && se.path == "" {
			msg = "arguments " + se.msg
		} else if ok {
			msg = "parameter " + se.path + " " + se.msg
		}
		return invalidArguments(msg, pathParameter(err))
	}
	return nil
}

// invalidArguments is the error for a tool call whose arguments the tool
// can't take: a *ToolError (-32602) to callers of Server.CallTool, which
// tools/call reports as a tool execution error (isError), as the spec asks
// of input validation errors, so the model can correct the call.
func invalidArguments(msg, parameter string) error {
	return &ToolError{Code: ErrorCodeInvalidParams, Message: msg, Data: map[string]any{"parameter": parameter}, invalidArguments: true}
}

// pathParameter is the top-level argument name a schema error refers to.
func pathParameter(err error) string {
	se, ok := err.(schemaError)
	if !ok || se.path == "" {
		return ""
	}
	name := se.path
	if i := strings.IndexAny(name, ".["); i > 0 {
		name = name[:i]
	}
	return name
}
