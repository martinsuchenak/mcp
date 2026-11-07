package mcp

import (
	"context"
	"net/http"
	"sort"
)

// registeredPrompt holds a prompt descriptor and its render handler.
type registeredPrompt struct {
	descriptor MCPPrompt
	handler    PromptHandler
}

// RegisterPrompt registers a prompt. The prompt appears in prompts/list and is
// rendered by prompts/get. Registering a prompt with a name that already exists
// replaces the previous one.
//
// Thread-safe. For per-request or per-user prompts, prefer [WithPromptProviders]
// over mutating the shared server.
func (s *Server) RegisterPrompt(pb *PromptBuilder, handler PromptHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts[pb.name] = &registeredPrompt{
		descriptor: pb.ToMCPPrompt(),
		handler:    handler,
	}
	s.NotifyPromptsChanged()
}

// UnregisterPrompt removes a prompt by name. Returns true if a prompt was removed.
func (s *Server) UnregisterPrompt(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.prompts[name]
	delete(s.prompts, name)
	if existed {
		s.NotifyPromptsChanged()
	}
	return existed
}

// ListPrompts returns all registered prompts plus any contributed by
// [PromptProvider]s on ctx, sorted by name. Duplicates (by name) are removed,
// with static registrations taking precedence.
func (s *Server) ListPrompts(ctx context.Context) []MCPPrompt {
	s.mu.RLock()
	result := make([]MCPPrompt, 0, len(s.prompts))
	seen := make(map[string]bool, len(s.prompts))
	for _, rp := range s.prompts {
		result = append(result, rp.descriptor)
		seen[rp.descriptor.Name] = true
	}
	s.mu.RUnlock()

	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })

	if ctx != nil {
		result = append(result, listPromptsFromProviders(ctx, seen)...)
	}
	return result
}

// GetPrompt renders a prompt by name with the given arguments. Resolution order:
//  1. Statically-registered prompts (exact name match).
//  2. [PromptProvider]s on ctx, in attachment order (first hit wins).
//
// Required arguments are validated before the handler runs. Returns
// ErrUnknownPrompt if nothing handles the name.
func (s *Server) GetPrompt(ctx context.Context, name string, args map[string]string) (*PromptResponse, error) {
	if args == nil {
		args = map[string]string{}
	}

	s.mu.RLock()
	rp, exists := s.prompts[name]
	s.mu.RUnlock()

	if exists {
		if err := validatePromptArguments(rp.descriptor, args); err != nil {
			return nil, err
		}
		return rp.handler(ctx, NewPromptRequest(args))
	}

	if ctx != nil {
		// Providers validate their own arguments, but for a consistent client
		// experience we validate against the static descriptor if present.
		return getPromptFromProviders(ctx, name, args)
	}
	return nil, ErrUnknownPrompt
}

// handlePromptsList handles prompts/list over HTTP.
func (s *Server) handlePromptsList(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	if cursorIn(req.Params) {
		s.sendMCPError(w, req.ID, errInvalidCursor.Code, errInvalidCursor.Message, nil)
		return
	}
	prompts := s.ListPrompts(r.Context())
	s.sendMCPResponse(w, req.ID, map[string]any{"prompts": prompts})
}

// handlePromptsGet handles prompts/get over HTTP.
func (s *Server) handlePromptsGet(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	var params promptGetParams
	if err := s.parseParams(req, &params); err != nil {
		s.sendProtocolAwareError(w, r, req, req.ID, ErrorCodeInvalidParams, "Invalid params", nil)
		return
	}
	if params.Name == "" {
		s.sendMCPError(w, req.ID, ErrorCodeInvalidParams, "name parameter is required", nil)
		return
	}

	resp, err := s.GetPrompt(r.Context(), params.Name, params.Arguments)
	if err != nil {
		e := promptsGetWireError(params.Name, err)
		s.sendMCPError(w, req.ID, e.Code, e.Message, e.Data)
		return
	}

	s.sendMCPResponse(w, req.ID, resp)
}

// validatePromptArguments returns an InvalidParams ToolError if a required
// argument is missing or empty.
func validatePromptArguments(descriptor MCPPrompt, args map[string]string) error {
	for _, arg := range descriptor.Arguments {
		if !arg.Required {
			continue
		}
		val, ok := args[arg.Name]
		if !ok || val == "" {
			return NewToolError(ErrorCodeInvalidParams, "missing required argument: "+arg.Name, nil)
		}
	}
	return nil
}
