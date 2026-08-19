package pluginpkg

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strings"

	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/frontmatter"
)

const (
	claudeMCPPath = ".mcp.json"
)

type claudeMCPIdentity struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// appendClaudeCompatibility maps Claude package conventions onto Reasonix's
// normalized manifest. It returns structured issues as well as compatibility
// warnings so frontends do not have to infer severity from English text.
func appendClaudeCompatibility(root string, manifest *Manifest) ([]string, []CompatibilityIssue) {
	var warnings []string
	var issues []CompatibilityIssue
	w, i := appendClaudeMCPFile(root, manifest)
	warnings = append(warnings, w...)
	issues = append(issues, i...)
	return uniqueSorted(warnings), issues
}

func appendClaudeMCPFile(root string, manifest *Manifest) ([]string, []CompatibilityIssue) {
	path := filepath.Join(root, claudeMCPPath)
	body, err := fileencoding.ReadFileUTF8(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return compatibilityFailure("mcp", claudeMCPPath, err)
	}
	var raw struct {
		MCPServers map[string]struct {
			Type        string            `json:"type"`
			Command     string            `json:"command"`
			Args        []string          `json:"args"`
			Env         map[string]string `json:"env"`
			URL         string            `json:"url"`
			Headers     map[string]string `json:"headers"`
			Title       string            `json:"title"`
			Description string            `json:"description"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return compatibilityFailure("mcp", claudeMCPPath, err)
	}
	if len(raw.MCPServers) == 0 {
		return nil, nil
	}
	if manifest.MCPServers == nil {
		manifest.MCPServers = map[string]MCPServer{}
	}
	names := make([]string, 0, len(raw.MCPServers))
	for name := range raw.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	var warnings []string
	var issues []CompatibilityIssue
	for _, displayName := range names {
		spec := raw.MCPServers[displayName]
		typ := strings.ToLower(strings.TrimSpace(spec.Type))
		switch typ {
		case "streamable-http":
			typ = "http"
		case "local":
			typ = "stdio"
		}
		if typ == "" {
			if strings.TrimSpace(spec.URL) != "" {
				typ = "http"
			} else {
				typ = "stdio"
			}
		}
		var reason string
		switch {
		case typ != "stdio" && typ != "http" && typ != "sse":
			reason = fmt.Sprintf("MCP server %q has unsupported transport %q", displayName, spec.Type)
		case typ == "stdio" && strings.TrimSpace(spec.Command) == "":
			reason = fmt.Sprintf("MCP server %q has no command", displayName)
		case (typ == "http" || typ == "sse") && strings.TrimSpace(spec.URL) == "":
			reason = fmt.Sprintf("MCP server %q has no URL", displayName)
		}
		if reason != "" {
			warnings = append(warnings, claudeMCPPath+": "+reason)
			issues = append(issues, CompatibilityIssue{Capability: "mcp", Path: claudeMCPPath, Reason: reason})
			continue
		}
		identity := claudeMCPIdentity{
			Type: typ, Command: strings.TrimSpace(spec.Command), Args: cleanStringList(spec.Args),
			Env: cloneHookEnv(spec.Env), URL: strings.TrimSpace(spec.URL), Headers: cloneHookEnv(spec.Headers),
		}
		id := claudeMCPServerID(displayName, identity)
		if _, exists := manifest.MCPServers[id]; exists {
			reason := fmt.Sprintf("MCP server %q maps to duplicate internal name %q", displayName, id)
			warnings = append(warnings, claudeMCPPath+": "+reason)
			issues = append(issues, CompatibilityIssue{Capability: "mcp", Path: claudeMCPPath, Reason: reason})
			continue
		}
		autoStart := false
		manifest.MCPServers[id] = MCPServer{
			Type:        typ,
			Command:     strings.TrimSpace(spec.Command),
			Args:        cleanStringList(spec.Args),
			Env:         cloneHookEnv(spec.Env),
			URL:         strings.TrimSpace(spec.URL),
			Headers:     cloneHookEnv(spec.Headers),
			AutoStart:   &autoStart,
			DisplayName: firstNonEmpty(strings.TrimSpace(spec.Title), strings.TrimSpace(displayName)),
			Description: strings.TrimSpace(spec.Description),
			Imported:    true,
		}
	}
	return uniqueSorted(warnings), issues
}

func claudeMCPServerID(name string, identity claudeMCPIdentity) string {
	trimmed := strings.TrimSpace(name)
	if IsValidName(trimmed) {
		return trimmed
	}
	var b strings.Builder
	lastDash := false
	for _, r := range trimmed {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if valid {
			b.WriteRune(r)
			lastDash = false
		} else if b.Len() > 0 && !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	base := strings.Trim(b.String(), "-_")
	if base == "" {
		base = "server"
	}
	body, _ := json.Marshal(identity)
	h := fnv.New32a()
	_, _ = h.Write([]byte(trimmed))
	_, _ = h.Write(body)
	suffix := fmt.Sprintf("_%08x", h.Sum32())
	maxBase := 64 - len(suffix)
	if len(base) > maxBase {
		base = base[:maxBase]
	}
	return base + suffix
}

func compatibilityFailure(capability, path string, err error) ([]string, []CompatibilityIssue) {
	reason := err.Error()
	return []string{path + ": " + reason}, []CompatibilityIssue{{Capability: capability, Path: path, Reason: reason}}
}

// compatibilityFor reports what appendClaudeCompatibility could determine
// statically at install time: whether every declared capability in the
// manifest parsed and mapped to a Reasonix construct ("full"), some did not
// ("partial", see issues), or none did ("none").
func compatibilityFor(pkg Package, issues []CompatibilityIssue) Compatibility {
	mapped := make([]string, 0, 4)
	skills, commands, mcp := pkg.CapabilityCounts()
	if skills > 0 {
		mapped = append(mapped, "skills")
	}
	if commands > 0 {
		mapped = append(mapped, "commands")
	}
	if pkg.AgentCount() > 0 {
		mapped = append(mapped, "agents")
	}
	if mcp > 0 {
		mapped = append(mapped, "mcp")
	}
	status := "full"
	if len(mapped) == 0 && pkg.ManifestKind != "reasonix" {
		status = "none"
	} else if len(issues) > 0 {
		status = "partial"
	}
	return Compatibility{Status: status, Mapped: mapped, Skipped: issues}
}

func dirContainsAgentMd(dir string) bool { return len(loadAgentRefs(dir)) > 0 }

func (p Package) agentRefs() []AgentRef {
	var out []AgentRef
	for _, root := range p.AgentRoots() {
		out = append(out, loadAgentRefs(root)...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func loadAgentRefs(dir string) []AgentRef {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []AgentRef
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		body, err := fileencoding.ReadFileUTF8(path)
		if err != nil {
			continue
		}
		fm, _ := frontmatter.Split(string(body))
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if declared := strings.TrimSpace(fm["name"]); IsValidName(declared) {
			name = declared
		}
		if !IsValidName(name) {
			continue
		}
		out = append(out, AgentRef{
			Name:         name,
			Description:  strings.TrimSpace(fm["description"]),
			Path:         path,
			Invocation:   "/" + name,
			Model:        strings.TrimSpace(fm["model"]),
			AllowedTools: splitCSV(fm["tools"]),
		})
	}
	return out
}

func splitCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = strings.TrimSpace(raw[1 : len(raw)-1])
	}
	var out []string
	for item := range strings.SplitSeq(raw, ",") {
		if item = strings.Trim(strings.TrimSpace(item), `"'`); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func cleanStringList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, value := range in {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func uniqueSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		if value = strings.TrimSpace(value); value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}
