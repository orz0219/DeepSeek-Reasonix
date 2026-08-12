package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/tool"
)

// restrictedCapabilityProxy preserves a subagent allowed-tools boundary when
// MCP is available only through use_capability. The pseudo mcp-tool: and
// mcp-server: entries never become provider tools; they select one proxy schema
// whose resolver rejects every capability outside the exact allowlist.
//
// Provider-visible name/description/schema stay identical to the unrestricted
// proxy so allowlist expansion never changes the child cache prefix. Allowlist
// enforcement is host-local (check + filtered list results).
type restrictedCapabilityProxy struct {
	tool.Tool
	resolver tool.CallResolver
	allowed  map[string]bool
	// servers is the set of MCP server names implied by allowed IDs; list
	// results are filtered to this set so profile isolation covers discovery.
	servers map[string]bool
}

// Description is fixed: never embed dynamic capability IDs (they change with
// MCP install/tool-list and would break the stable provider tool prefix).
func (t *restrictedCapabilityProxy) Description() string {
	return t.Tool.Description()
}

func (t *restrictedCapabilityProxy) check(args json.RawMessage) error {
	var p struct {
		Action       string `json:"action"`
		CapabilityID string `json:"capability_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return fmt.Errorf("invalid args: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(p.Action), "list") {
		return nil
	}
	id := strings.TrimSpace(p.CapabilityID)
	if id == "" {
		return fmt.Errorf("capability_id is required")
	}
	if !t.allowed[id] {
		return fmt.Errorf("capability %q is outside this subagent's allowed-tools", id)
	}
	return nil
}

func (t *restrictedCapabilityProxy) ResolveCall(ctx context.Context, args json.RawMessage) (tool.ResolvedCall, error) {
	if err := t.check(args); err != nil {
		return tool.ResolvedCall{}, err
	}
	rc, err := t.resolver.ResolveCall(ctx, args)
	if err != nil {
		return rc, err
	}
	var p struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(args, &p)
	if strings.EqualFold(strings.TrimSpace(p.Action), "list") && rc.SkipExecute {
		rc.Result = filterCapabilityListResult(rc.Result, t.servers)
	}
	return rc, nil
}

// emptyCapabilityListResult is the fail-closed list payload: no server metadata.
func emptyCapabilityListResult(note string) string {
	if strings.TrimSpace(note) == "" {
		note = "list is filtered to this subagent's allowed MCP servers."
	}
	b, err := json.MarshalIndent(map[string]any{
		"servers": []listServerInfo{},
		"note":    note,
	}, "", "  ")
	if err != nil {
		return `{"servers":[],"note":"list is filtered to this subagent's allowed MCP servers."}`
	}
	return string(b)
}

// filterCapabilityListResult keeps only servers in the allowlist for restricted
// proxies. Empty allowlist or unreadable payloads fail closed (empty server
// list) so discovery never leaks the full configured MCP inventory.
func filterCapabilityListResult(raw string, servers map[string]bool) string {
	const baseNote = "list is filtered to this subagent's allowed MCP servers."
	if len(servers) == 0 {
		return emptyCapabilityListResult(baseNote + " No allowed MCP servers were resolved from the profile allowlist.")
	}
	var payload struct {
		Servers []listServerInfo `json:"servers"`
		Note    string           `json:"note"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return emptyCapabilityListResult(baseNote + " List payload was unreadable; returning no servers (fail-closed).")
	}
	filtered := make([]listServerInfo, 0, len(payload.Servers))
	for _, s := range payload.Servers {
		if servers[strings.TrimSpace(s.Name)] {
			filtered = append(filtered, s)
		}
	}
	payload.Servers = filtered
	if payload.Note == "" {
		payload.Note = baseNote
	} else if !strings.Contains(payload.Note, "Filtered to this subagent") {
		payload.Note = payload.Note + " Filtered to this subagent's allowed MCP servers."
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return emptyCapabilityListResult(baseNote + " Failed to encode filtered list (fail-closed).")
	}
	return string(b)
}

// validMCPServerCapabilityID accepts mcp-server:<non-empty-name> only.
func validMCPServerCapabilityID(id string) (server string, ok bool) {
	if !strings.HasPrefix(id, "mcp-server:") {
		return "", false
	}
	server = strings.TrimSpace(strings.TrimPrefix(id, "mcp-server:"))

	return server, server != "" && !strings.Contains(server, "/")
}

// validMCPToolCapabilityID accepts mcp-tool:<server>/<tool> with both parts non-empty.
func validMCPToolCapabilityID(id string) (server, raw string, ok bool) {
	if !strings.HasPrefix(id, "mcp-tool:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(id, "mcp-tool:")
	server, raw, cut := strings.Cut(rest, "/")
	server = strings.TrimSpace(server)
	raw = strings.TrimSpace(raw)
	return server, raw, cut && server != "" && raw != ""
}

func serversFromCapabilityAllowlist(allowed map[string]bool) map[string]bool {
	servers := map[string]bool{}
	for id := range allowed {
		id = strings.TrimSpace(id)
		if server, ok := validMCPServerCapabilityID(id); ok {
			servers[server] = true
			continue
		}
		if server, _, ok := validMCPToolCapabilityID(id); ok {
			servers[server] = true
		}
	}
	return servers
}

// attachSubagentCapabilityProxy installs a per-agent use_capability frontend.
// Any parent-copied proxy is replaced so children never share Executor ledger
// state. No allowlist → full proxy. Explicit allowlist with MCP names →
// restricted proxy. Explicit "use_capability" → full proxy. Explicit allowlist
// without MCP entries → no proxy.
func attachSubagentCapabilityProxy(parent, sub *tool.Registry, names []string, runtime *MCPCapabilityRuntime) {
	if sub == nil {
		return
	}

	if _, ok := sub.Get("use_capability"); ok {
		sub.RemovePrefix("use_capability")
	}
	frontend := newSubagentCapabilityFrontend(parent, runtime)
	if frontend == nil {
		return
	}
	if len(names) == 0 || allowlistRequestsUnrestrictedProxy(names) {
		sub.Add(frontend)
		return
	}
	allowed := mcpCapabilityAllowlist(parent, names)
	if len(allowed) == 0 {

		return
	}
	servers := serversFromCapabilityAllowlist(allowed)
	if len(servers) == 0 {

		return
	}
	resolver, ok := frontend.(tool.CallResolver)
	if !ok {
		return
	}
	sub.Add(&restrictedCapabilityProxy{
		Tool:     frontend,
		resolver: resolver,
		allowed:  allowed,
		servers:  servers,
	})
}

func newSubagentCapabilityFrontend(parent *tool.Registry, runtime *MCPCapabilityRuntime) tool.Tool {
	if runtime != nil {
		return runtime.NewFrontend(nil, nil)
	}
	if parent == nil {
		return nil
	}
	inner, ok := parent.Get("use_capability")
	if !ok {
		return nil
	}
	if uc, ok := inner.(*UseCapabilityTool); ok {
		return uc.CloneForAgent(nil, nil)
	}
	return inner
}

// mcpCapabilityAllowlist converts profile/call tool names into capability IDs
// for the restricted use_capability proxy. Accepts complete mcp-tool:<s>/<t>,
// mcp-server:<s>, model-visible mcp__* names, and wildcards expanded against
// the parent. Incomplete prefixes such as "mcp-server:" or "mcp-tool:foo" are
// rejected so they cannot install a restricted proxy with an empty server set.
func mcpCapabilityAllowlist(parent *tool.Registry, names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	expanded := names
	if parent != nil {
		expanded = expandToolPatterns(parent, names)
	}
	allowed := map[string]bool{}
	for _, name := range expanded {
		name = strings.TrimSpace(name)
		switch {
		case name == "use_capability":

			continue
		case strings.HasPrefix(name, "mcp-server:"):
			if server, ok := validMCPServerCapabilityID(name); ok {
				allowed["mcp-server:"+server] = true
			}
		case strings.HasPrefix(name, "mcp-tool:"):
			if server, raw, ok := validMCPToolCapabilityID(name); ok {
				allowed["mcp-tool:"+server+"/"+raw] = true
			}
		default:
			if parent != nil {
				if tl, ok := parent.Get(name); ok {
					if m, ok := tl.(tool.MCPMetadata); ok {
						server := strings.TrimSpace(m.MCPServerName())
						raw := strings.TrimSpace(m.MCPRawToolName())
						if server != "" && raw != "" {
							allowed["mcp-tool:"+server+"/"+raw] = true
							continue
						}
					}
				}
			}
			if server, raw, ok := tool.SplitMCPName(name); ok {
				allowed["mcp-tool:"+server+"/"+raw] = true
			}
		}
	}
	return allowed
}

func allowlistRequestsUnrestrictedProxy(names []string) bool {
	for _, name := range names {
		if strings.TrimSpace(name) == "use_capability" {
			return true
		}
	}
	return false
}
