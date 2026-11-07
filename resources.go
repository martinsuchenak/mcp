package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// remoteResourceFanoutTimeout bounds each remote server's ReadResource call
// during the fan-out described below. Without it, a slow, unreachable, or
// (worst case) mutually-federated remote server — one that itself forwards
// unresolved reads back through a client pointed at this server — could
// block this call indefinitely, since a ctx with no deadline of its own
// gives context.WithTimeout nothing shorter to fall back to.
// A var, not a const, so tests can shorten it rather than actually waiting
// out the default.
var remoteResourceFanoutTimeout = 10 * time.Second

// maxResourceFanoutHops bounds how many times a single resources/read may be
// forwarded server-to-server by ReadResource's fan-out (step 4). Federation
// graphs aren't guaranteed acyclic — two servers that register each other as
// remotes form a loop, and every hop of that loop is a fresh HTTP request
// re-entering the same fan-out on the far side. The per-attempt timeout alone
// bounds such a loop only in time (a mutual pair churns doomed requests for
// the full window before unwinding, and a richer cyclic graph amplifies
// branchingly); the hop counter bounds it structurally. The count travels on
// the X-MCP-Resource-Fanout-Hop header (see headerResourceFanoutHop): the
// server seeds it from the incoming request, the fan-out increments it per
// forward, and forwarding stops entirely once the limit is reached — a
// cyclic graph then resolves to ErrUnknownResource after at most this many
// forwards per branch instead of amplifying.
const maxResourceFanoutHops = 3

// headerResourceFanoutHop carries maxResourceFanoutHops' counter across the
// HTTP hop between federating servers. A private X- header rather than a
// _meta field or an Mcp-* name: it's an implementation detail of this
// library's client/server pair, not protocol a third party needs to
// understand — a third-party server between two of ours simply won't
// increment it, degrading to the timeout-only bound above.
const headerResourceFanoutHop = "X-MCP-Resource-Fanout-Hop"

type resourceFanoutHopKey struct{}

// withResourceFanoutHop returns ctx annotated with a read's current hop
// count — consumed by ReadResource's fan-out and by the Client's HTTP paths
// (sendRequest/sendModernHTTPRequest), which mirror it onto
// headerResourceFanoutHop so the receiving server can seed its own count.
func withResourceFanoutHop(ctx context.Context, hop int) context.Context {
	return context.WithValue(ctx, resourceFanoutHopKey{}, hop)
}

// resourceFanoutHopFrom reports ctx's hop count, 0 when unset.
func resourceFanoutHopFrom(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	if hop, ok := ctx.Value(resourceFanoutHopKey{}).(int); ok {
		return hop
	}
	return 0
}

// legacyResourceNotFoundCode is the resource-not-found error code from the
// Legacy revisions (2025-03-26 through 2025-11-25); the Modern revision and
// this library's own servers answer the same condition with -32602.
const legacyResourceNotFoundCode = -32002

// isResourceNotFoundErr reports whether err is a remote's ordinary "I don't
// have that resource" answer, as opposed to a transport or server failure
// (auth rejected, 5xx, timeout, unreachable). Only the former may silently
// continue the fan-out to the next remote; the latter are recorded so the
// caller can tell a genuine miss from a broken federation.
func isResourceNotFoundErr(err error) bool {
	var te *ToolError
	if errors.As(err, &te) {
		return te.Code == ErrorCodeInvalidParams || te.Code == legacyResourceNotFoundCode
	}
	return false
}

// registeredResource holds a static resource and its read handler.
type registeredResource struct {
	descriptor MCPResource
	handler    ResourceHandler
}

// registeredResourceTemplate holds a resource template, its read handler, and a
// precompiled pattern used to match concrete URIs against the template. varNames
// holds the template's placeholder names in order, aligned with the pattern's
// capturing groups.
type registeredResourceTemplate struct {
	descriptor MCPResourceTemplate
	handler    ResourceHandler
	pattern    *regexp.Regexp
	varNames   []string
}

// RegisterResource registers a static resource. The resource appears in
// resources/list and is served by resources/read. Registering a resource with a
// URI that already exists replaces the previous one.
//
// Thread-safe. Configure before serving in simple setups; concurrent
// registration while requests are in flight is permitted (readers observe
// either the old or new set) but discouraged for per-request data — use
// [WithResourceProviders] instead.
func (s *Server) RegisterResource(rb *ResourceBuilder, handler ResourceHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources[rb.uri] = &registeredResource{
		descriptor: rb.ToMCPResource(),
		handler:    handler,
	}
	s.NotifyResourcesChanged()
}

// UnregisterResource removes a static resource by URI. Returns true if a
// resource was removed.
func (s *Server) UnregisterResource(uri string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, existed := s.resources[uri]
	delete(s.resources, uri)
	if existed {
		s.NotifyResourcesChanged()
	}
	return existed
}

// UnregisterResourceTemplate removes a resource template by its URI template
// string. Returns true if a template was removed.
func (s *Server) UnregisterResourceTemplate(uriTemplate string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, rt := range s.resourceTemplates {
		if rt.descriptor.URITemplate == uriTemplate {
			s.resourceTemplates = append(s.resourceTemplates[:i], s.resourceTemplates[i+1:]...)
			s.NotifyResourcesChanged()
			return true
		}
	}
	return false
}

// RegisterResourceTemplate registers a parameterized resource template. The
// template appears in resources/templates/list; a client expands it into a
// concrete URI and reads it via resources/read. The handler receives the
// expanded URI and is responsible for parsing any variables out of it.
//
// A template's URITemplate may contain one or more {var} placeholders. Each
// placeholder matches one or more characters in the requested URI. Templates are
// matched in registration order; the first match wins.
//
// Thread-safe.
func (s *Server) RegisterResourceTemplate(tb *ResourceTemplateBuilder, handler ResourceHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	varNames, pattern := parseResourceTemplate(tb.uriTemplate)
	s.resourceTemplates = append(s.resourceTemplates, &registeredResourceTemplate{
		descriptor: tb.ToMCPResourceTemplate(),
		handler:    handler,
		pattern:    pattern,
		varNames:   varNames,
	})
	s.NotifyResourcesChanged()
}

// ListResources returns all registered static resources plus any contributed by
// [ResourceProvider]s on ctx, sorted by URI. Duplicates (by URI) are removed,
// with static registrations taking precedence.
func (s *Server) ListResources(ctx context.Context) []MCPResource {
	s.mu.RLock()
	result := make([]MCPResource, 0, len(s.resources))
	seen := make(map[string]bool, len(s.resources))
	for _, rr := range s.resources {
		result = append(result, rr.descriptor)
		seen[rr.descriptor.URI] = true
	}
	s.mu.RUnlock()

	sort.Slice(result, func(i, j int) bool { return result[i].URI < result[j].URI })

	if ctx != nil {
		result = append(result, listResourcesFromProviders(ctx, seen)...)
	}
	return result
}

// ListResourceTemplates returns all registered resource templates plus any
// contributed by [ResourceProvider]s on ctx, sorted by URITemplate. Duplicates
// (by URITemplate) are removed, with static registrations taking precedence.
func (s *Server) ListResourceTemplates(ctx context.Context) []MCPResourceTemplate {
	s.mu.RLock()
	result := make([]MCPResourceTemplate, 0, len(s.resourceTemplates))
	seen := make(map[string]bool, len(s.resourceTemplates))
	for _, rt := range s.resourceTemplates {
		result = append(result, rt.descriptor)
		seen[rt.descriptor.URITemplate] = true
	}
	s.mu.RUnlock()

	sort.Slice(result, func(i, j int) bool { return result[i].URITemplate < result[j].URITemplate })

	if ctx != nil {
		result = append(result, listResourceTemplatesFromProviders(ctx, seen)...)
	}
	return result
}

// ReadResource resolves a URI to its content. Resolution order:
//  1. Static resources by exact URI match.
//  2. Static resource templates by pattern match (first match wins).
//  3. [ResourceProvider]s on ctx, in attachment order (first hit wins).
//  4. Remote servers registered via [Server.ReplaceRemoteServers] /
//     [Server.RegisterRemoteServer], tried in registration order (first hit
//     wins). Unlike tools, remote resources aren't namespaced or cached in a
//     lookup table — a federated server's resource URIs (e.g. a `ui://`
//     MCP Apps view) are opaque and forwarded as-is, so this is a best-effort
//     fan-out rather than an indexed lookup. That fan-out is bounded in two
//     dimensions: per-attempt by [remoteResourceFanoutTimeout], and in depth
//     by [maxResourceFanoutHops]' cycle guard, so mutually-federated servers
//     can't loop or amplify an unresolved read.
//
// Returns ErrUnknownResource if nothing handles the uri. When one or more
// remotes failed outright (rather than answering not-found), the returned
// error still wraps ErrUnknownResource — errors.Is holds — but names each
// failing remote, so a misconfigured or down federation is diagnosable
// instead of masquerading as a plain miss. See [Server.ReadResourceFrom]
// for a variant restricted to one specific source rather than fanning out
// to every registered remote.
func (s *Server) ReadResource(ctx context.Context, uri string) (*ResourceResponse, error) {
	resp, err := s.readNativeResource(ctx, uri)
	if err != ErrUnknownResource {
		return resp, err
	}

	// Cycle guard: a read that has already been forwarded the limit of times
	// is answered here as a miss rather than forwarded again, which unwinds
	// any loop the federation graph contains.
	hop := resourceFanoutHopFrom(ctx)
	if hop >= maxResourceFanoutHops {
		return nil, ErrUnknownResource
	}

	s.mu.RLock()
	remoteClients := make([]*registeredClient, 0, len(s.remoteClients))
	for _, rc := range s.remoteClients {
		remoteClients = append(remoteClients, rc)
	}
	s.mu.RUnlock()

	// A namespaced skill URI (skill://<ns>/…) belongs to the server
	// federating that namespace, and the spec routes every skill read to
	// originating servers: send it there alone, with the namespace segment
	// stripped back to the remote's own URI. A URI naming no registered
	// skills-federating namespace falls through to the generic fan-out — a
	// chained gateway downstream may serve it verbatim.
	if ns, original, ok := splitFederatedSkillURI(uri); ok {
		for _, rc := range remoteClients {
			if !rc.federateSkills || rc.namespace != ns {
				continue
			}
			attemptCtx, cancel := context.WithTimeout(withResourceFanoutHop(ctx, hop+1), remoteResourceFanoutTimeout)
			resp, err := rc.client.ReadResource(attemptCtx, original)
			cancel()
			if err == nil {
				// Echo the namespaced URI the client asked for: the remote's
				// envelope names its own (stripped) URI, and a client that
				// checks the content URI against its request — MCP
				// Inspector's conformance view, for one — reads that
				// mismatch as no content. Only the URI field is rewritten;
				// bytes and MIME type pass through verbatim.
				for i := range resp.Contents {
					if resp.Contents[i].URI == original {
						resp.Contents[i].URI = uri
					}
				}
				return resp, nil
			}
			if isResourceNotFoundErr(err) {
				return nil, ErrUnknownResource
			}
			return nil, fmt.Errorf("%s: %w", rc.client.BaseURL(), err)
		}
	}

	var remoteFailures []error
	for _, rc := range remoteClients {
		// An app-excluding registration federates no MCP Apps surface: a
		// ui:// resource is an app view by definition, so don't forward
		// reads for it either.
		if rc.excludeApps && strings.HasPrefix(uri, "ui:") {
			continue
		}
		attemptCtx, cancel := context.WithTimeout(withResourceFanoutHop(ctx, hop+1), remoteResourceFanoutTimeout)
		resp, err := rc.client.ReadResource(attemptCtx, uri)
		cancel()
		if err == nil {
			return resp, nil
		}
		if isResourceNotFoundErr(err) {
			continue
		}
		remoteFailures = append(remoteFailures, fmt.Errorf("%s: %w", rc.client.BaseURL(), err))
	}

	if len(remoteFailures) > 0 {
		return nil, fmt.Errorf("%w; %d remote server(s) failed: %w",
			ErrUnknownResource, len(remoteFailures), errors.Join(remoteFailures...))
	}
	return nil, ErrUnknownResource
}

// readNativeResource is ReadResource's steps 1-3 (static resources,
// templates, providers) with no remote fan-out — shared with
// ReadResourceFrom's source == "" case, which must never fall through to a
// federated server the way the public ReadResource does.
func (s *Server) readNativeResource(ctx context.Context, uri string) (*ResourceResponse, error) {
	s.mu.RLock()

	if rr, ok := s.resources[uri]; ok {
		handler := rr.handler
		s.mu.RUnlock()
		return handler(ctx, NewResourceRequest(uri, nil))
	}

	templates := make([]*registeredResourceTemplate, len(s.resourceTemplates))
	copy(templates, s.resourceTemplates)
	s.mu.RUnlock()

	for _, rt := range templates {
		if rt.pattern == nil {
			continue
		}
		if vars, ok := matchResourceTemplate(rt.pattern, rt.varNames, uri); ok {
			return rt.handler(ctx, NewResourceRequest(uri, vars))
		}
	}

	if ctx != nil {
		resp, err := readResourceFromProviders(ctx, uri)
		if err != ErrUnknownResource {
			return resp, err
		}
	}

	return nil, ErrUnknownResource
}

// ReadResourceFrom reads uri restricted to a single source, as identified
// by [Server.ToolSource]: "" for this server's own native resources only
// (static, templates, providers — no fan-out to any remote), or a remote
// server's [Client.BaseURL] to read from that one remote exclusively,
// never falling through to any other registered remote the way
// [Server.ReadResource]'s fan-out does. Returns ErrUnknownResource if
// source doesn't match any registered remote, or if uri isn't found there.
//
// Pairs with ToolSource for a host enforcing that an MCP Apps view may
// only read a resource belonging to the same server as the tool that
// mounted it: ReadResourceFrom(ctx, toolSource, uri) rather than the
// ambient ReadResource(ctx, uri), which would otherwise happily serve a
// same-named (or any) resource from a completely different federated
// server than the one the view is scoped to.
func (s *Server) ReadResourceFrom(ctx context.Context, source, uri string) (*ResourceResponse, error) {
	if source == "" {
		return s.readNativeResource(ctx, uri)
	}

	s.mu.RLock()
	var target *registeredClient
	for _, rc := range s.remoteClients {
		if rc.client.BaseURL() == source {
			target = rc
			break
		}
	}
	s.mu.RUnlock()
	if target == nil {
		return nil, ErrUnknownResource
	}

	attemptCtx, cancel := context.WithTimeout(ctx, remoteResourceFanoutTimeout)
	defer cancel()
	resp, err := target.client.ReadResource(attemptCtx, uri)
	if err != nil {
		return nil, ErrUnknownResource
	}
	return resp, nil
}

// handleResourcesList handles resources/list over HTTP.
func (s *Server) handleResourcesList(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	resources := s.ListResources(r.Context())
	s.sendMCPResponse(w, req.ID, map[string]any{"resources": resources})
}

// handleResourcesTemplatesList handles resources/templates/list over HTTP.
func (s *Server) handleResourcesTemplatesList(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	templates := s.ListResourceTemplates(r.Context())
	s.sendMCPResponse(w, req.ID, map[string]any{"resourceTemplates": templates})
}

// handleResourcesRead handles resources/read over HTTP.
func (s *Server) handleResourcesRead(w http.ResponseWriter, r *http.Request, req *MCPRequest) {
	var params resourceReadParams
	if err := s.parseParams(req, &params); err != nil {
		s.sendProtocolAwareError(w, r, req, req.ID, ErrorCodeInvalidParams, "Invalid params", nil)
		return
	}
	if params.URI == "" {
		s.sendMCPError(w, req.ID, ErrorCodeInvalidParams, "uri parameter is required", nil)
		return
	}

	// Seed the fan-out hop counter from the incoming forward marker (see
	// maxResourceFanoutHops): a read arriving from another server's fan-out
	// already carries hops that this server's own fan-out must count.
	readCtx := r.Context()
	if raw := r.Header.Get(headerResourceFanoutHop); raw != "" {
		if hop, err := strconv.Atoi(raw); err == nil && hop > 0 {
			readCtx = withResourceFanoutHop(readCtx, hop)
		}
	}

	resp, err := s.ReadResource(readCtx, params.URI)
	if err != nil {
		e := resourcesReadWireError(params.URI, err, !isModernRequest(r, req))
		s.sendMCPError(w, req.ID, e.Code, e.Message, e.Data)
		return
	}

	s.sendMCPResponse(w, req.ID, resp)
}

// parseResourceTemplate compiles an RFC 6570 level-1 URI template (with {var}
// placeholders) into an anchored regexp that matches concrete URIs, and returns
// the placeholder names in order (aligned with the pattern's capturing groups).
// Each placeholder becomes a group matching one or more characters. A malformed
// template returns a nil pattern, in which case the template never matches.
func parseResourceTemplate(template string) ([]string, *regexp.Regexp) {
	var (
		b        strings.Builder
		varNames []string
	)
	b.WriteByte('^')
	i := 0
	for i < len(template) {
		if template[i] == '{' {
			end := strings.IndexByte(template[i:], '}')
			if end == -1 {
				// Unterminated placeholder; treat the rest as a literal.
				b.WriteString(regexp.QuoteMeta(template[i:]))
				break
			}
			name := strings.TrimSpace(template[i+1 : i+end])
			varNames = append(varNames, name)
			b.WriteString("(.+)")
			i += end + 1
			continue
		}
		next := strings.IndexByte(template[i:], '{')
		if next == -1 {
			b.WriteString(regexp.QuoteMeta(template[i:]))
			break
		}
		b.WriteString(regexp.QuoteMeta(template[i : i+next]))
		i += next
	}
	b.WriteByte('$')
	re, err := regexp.Compile(b.String())
	if err != nil {
		return varNames, nil
	}
	return varNames, re
}

// matchResourceTemplate matches uri against a compiled template pattern and, on
// success, returns the extracted variables keyed by their placeholder names.
func matchResourceTemplate(pattern *regexp.Regexp, varNames []string, uri string) (map[string]string, bool) {
	m := pattern.FindStringSubmatch(uri)
	if m == nil {
		return nil, false
	}
	// m[0] is the full match; m[1:] are the capturing groups in order.
	vars := make(map[string]string, len(varNames))
	for i, name := range varNames {
		if i+1 < len(m) {
			vars[name] = m[i+1]
		}
	}
	return vars, true
}

// MatchResourceTemplate extracts the {var} values from uri against an RFC 6570
// level-1 URI template. It is intended for [ResourceProvider] implementations
// and advanced uses that need to parse their own template URIs; handlers
// registered via [Server.RegisterResourceTemplate] receive the variables
// already extracted on the [ResourceRequest].
//
// Returns the variables and nil on a match, or (nil, error) if uri does not
// match the template.
func MatchResourceTemplate(template, uri string) (map[string]string, error) {
	varNames, pattern := parseResourceTemplate(template)
	if pattern == nil {
		return nil, fmt.Errorf("invalid resource template: %q", template)
	}
	vars, ok := matchResourceTemplate(pattern, varNames, uri)
	if !ok {
		return nil, fmt.Errorf("uri %q does not match template %q", uri, template)
	}
	return vars, nil
}
