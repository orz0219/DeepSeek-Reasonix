package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/capability"
	"reasonix/internal/plugin"
	"reasonix/internal/tool"
)

// listServerInfo is one configured MCP server entry returned by action=list.
// It never starts a server or opens a network connection.
type listServerInfo struct {
	Name         string `json:"name"`
	CapabilityID string `json:"capability_id"`
	Status       string `json:"status"`
	Authorized   bool   `json:"authorized"`
	Connected    bool   `json:"connected"`
}

// listCapabilities returns the unified catalog summary: MCP servers plus
// non-provider-visible tools and skills available through this proxy. The
// top-level "servers" key stays compatible with restricted subagent list
// filtering.
func (t *UseCapabilityTool) listCapabilities() (string, error) {
	type capInfo struct {
		ID          string `json:"id"`
		Kind        string `json:"kind"`
		Name        string `json:"name"`
		Status      string `json:"status,omitempty"`
		ReadOnly    bool   `json:"read_only,omitempty"`
		Description string `json:"description,omitempty"`
	}
	var caps []capInfo
	if t.catalog != nil {
		for _, e := range t.catalog().Entries {

			if e.Kind == capability.KindTool && t.registry != nil && t.registry.ProviderVisible(e.ToolName) {
				continue
			}
			caps = append(caps, capInfo{
				ID:          e.ID,
				Kind:        string(e.Kind),
				Name:        e.Name,
				Status:      string(e.Status),
				ReadOnly:    e.ReadOnly,
				Description: e.Description,
			})
		}
	}
	serversJSON, err := t.listServers()
	if err != nil {
		return "", err
	}
	var serversPayload struct {
		Servers []listServerInfo `json:"servers"`
		Note    string           `json:"note"`
	}
	_ = json.Unmarshal([]byte(serversJSON), &serversPayload)
	payload := map[string]any{
		"capabilities": caps,
		"servers":      serversPayload.Servers,
		"note":         "Call action=call with a capability_id to invoke a non-core tool, skill, MCP tool, or other catalog entry without changing the provider tool schema.",
	}
	if serversPayload.Note != "" {
		payload["note"] = payload["note"].(string) + " " + serversPayload.Note
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// listServers returns sorted configured MCP server names, status, and
// capability IDs without starting servers. Used by Planner discovery when no
// specific capability route was provided.
func (t *UseCapabilityTool) listServers() (string, error) {
	configured := t.configuredServers()
	list := make([]listServerInfo, 0, len(configured))
	for _, server := range configured {
		spec := server.spec
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			continue
		}

		resolved := plugin.ResolveStoredAuthorization(context.Background(), spec)
		connected := server.enabled && resolved.ServerAuthorized() && t.host != nil && t.host.HasClientForSpec(resolved)
		status := "configured"
		if !server.enabled {
			status = "disabled"
		} else if connected {
			status = "ready"
		} else if t.host != nil {
			for _, f := range t.host.Failures() {
				if f.Name == name && strings.TrimSpace(f.Error) != "" {
					status = "failed"
					break
				}
			}
		}
		list = append(list, listServerInfo{
			Name:         name,
			CapabilityID: "mcp-server:" + name,
			Status:       status,
			Authorized:   resolved.ServerAuthorized(),
			Connected:    connected,
		})
	}
	b, err := json.MarshalIndent(map[string]any{
		"servers": list,
		"note":    "list does not start MCP servers. Call action=call on mcp-server:<name> to connect after authorization, or mcp-tool:<server>/<tool> for a concrete tool.",
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (t *UseCapabilityTool) inspect(ctx context.Context, id string) (string, error) {
	cat := t.currentCatalog()
	if e, ok := cat.Lookup(id); ok {
		b, _ := json.MarshalIndent(map[string]any{
			"id":          e.ID,
			"kind":        e.Kind,
			"name":        e.Name,
			"description": e.Description,
			"status":      e.Status,
			"read_only":   e.ReadOnly,
			"auto_use":    e.AutoUse,
			"requires":    e.Requires,
			"profiles":    e.Profiles,
			"tool_name":   e.ToolName,
			"auto_start":  e.AutoStart,
		}, "", "  ")

		if e.Kind == capability.KindMCPServer || e.Kind == capability.KindMCPTool {
			server := e.Source
			if server == "" {
				server = e.ConnectName
			}
			toolFilter := ""
			if e.Kind == capability.KindMCPTool {
				parsedServer, raw, err := parseMCPCapabilityID(e.ID)
				if err != nil {
					return string(b), nil
				}
				if server == "" {
					server = parsedServer
				} else if parsedServer != server {
					return string(b), nil
				}
				toolFilter = raw
			}
			if server != "" {
				if !t.serverEnabled(server) {
					return string(b) + "\n\nServer is disabled in this session.", nil
				}
				if t.host != nil && t.host.HasClient(server) {

					tools, err := t.serverTools(ctx, server)
					if err != nil {
						return string(b) + "\n\nTool listing failed: " + err.Error(), nil
					}
					return string(b) + "\n\nTools:\n" + inspectToolListJSON(server, filterInspectTools(tools, toolFilter)), nil
				}
				if spec, ok := t.specFor(server); ok {
					if cs, ok := plugin.LoadCachedSchemaForSpec(spec); ok && len(cs.Tools) > 0 {
						var list []inspectToolInfo
						for _, ct := range cs.Tools {
							if toolFilter != "" && ct.Name != toolFilter {
								continue
							}
							list = append(list, inspectToolInfo{
								ID:          "mcp-tool:" + server + "/" + ct.Name,
								Name:        plugin.ModelToolName(server, ct.Name),
								Description: ct.Description,
								ReadOnly:    ct.ReadOnly,
								Schema:      ct.Schema,
							})
						}
						extra, _ := json.MarshalIndent(list, "", "  ")
						return string(b) + "\n\nTools (from cached schema; server not started):\n" + string(extra), nil
					}
					return string(b) + "\n\nServer not connected and no cached tool schema; call use_capability(action=\"call\", capability_id=\"mcp-server:" + server + "\") to connect (after approval) and list its tools.", nil
				}
			}
		}
		return string(b), nil
	}
	return "", fmt.Errorf("unknown capability_id %q", id)
}

// filterInspectTools narrows concrete mcp-tool inspection to that exact tool.
// Server inspection intentionally keeps the full directory. This prevents a
// restricted sub-agent allowed one tool from discovering sibling tool schemas
// through action=inspect on its allowed capability ID.
func filterInspectTools(tools []tool.Tool, raw string) []tool.Tool {
	if raw == "" {
		return tools
	}
	filtered := make([]tool.Tool, 0, 1)
	for _, tl := range tools {
		if m, ok := tl.(tool.MCPMetadata); ok && m.MCPRawToolName() == raw {
			filtered = append(filtered, tl)
			break
		}
	}
	return filtered
}

type inspectToolInfo struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	ReadOnly    bool            `json:"read_only"`
	Schema      json.RawMessage `json:"input_schema,omitempty"`
}

// inspectToolListJSON renders a server's live tools as the capability-id
// directory shared by inspect and the first-discovery connect result.
func inspectToolListJSON(server string, tools []tool.Tool) string {
	var list []inspectToolInfo
	for _, tl := range tools {
		raw := ""
		if m, ok := tl.(tool.MCPMetadata); ok {
			raw = m.MCPRawToolName()
		}
		list = append(list, inspectToolInfo{
			ID:          "mcp-tool:" + server + "/" + raw,
			Name:        tl.Name(),
			Description: tl.Description(),
			ReadOnly:    tl.ReadOnly(),
			Schema:      tl.Schema(),
		})
	}
	extra, _ := json.MarshalIndent(list, "", "  ")
	return string(extra)
}
