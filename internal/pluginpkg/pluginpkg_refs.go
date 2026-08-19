package pluginpkg

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/command"
	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/frontmatter"
)

type Inventory struct {
	Skills     []SkillRef
	Agents     []AgentRef
	Commands   []CommandRef
	Prompts    []PromptRef
	Themes     []ThemeRef
	MCPServers []MCPServerRef
}

func (p Package) Inventory() Inventory {
	return Inventory{
		Skills:     p.skillRefs(),
		Agents:     p.agentRefs(),
		Commands:   p.commandRefs(),
		Prompts:    p.promptRefs(),
		Themes:     p.themeRefs(),
		MCPServers: p.mcpServerRefs(),
	}
}

// commandRefs loads the package's command dirs through the same loader the
// runtime uses (internal/command), so names, namespacing, and frontmatter
// semantics can never drift between the inventory and actual invocation.
func (p Package) commandRefs() []CommandRef {
	roots := p.CommandRoots()
	if len(roots) == 0 {
		return nil
	}
	cmds, _ := command.Load(roots...)
	out := make([]CommandRef, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, CommandRef{
			Name:        c.Name,
			Description: c.Description,
			ArgHint:     c.ArgHint,
			Path:        c.Source,
			Invocation:  "/" + c.Name,
		})
	}
	return out
}

// promptRefs loads the package's prompt dirs through the same loader the
// command inventory uses (internal/command): prompt templates share the
// slash-command file shape, so names, frontmatter, and malformed-file
// handling stay identical.
func (p Package) promptRefs() []PromptRef {
	roots := p.PromptRoots()
	if len(roots) == 0 {
		return nil
	}
	cmds, _ := command.Load(roots...)
	out := make([]PromptRef, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, PromptRef{
			Name:        c.Name,
			Description: c.Description,
			ArgHint:     c.ArgHint,
			Path:        c.Source,
		})
	}
	return out
}

// themeRefs resolves the manifest's themes list (plain paths and
// per-segment globs) to concrete theme files. Parse-time validation has
// already rejected escapes and non-regular files, so unreadable entries
// here simply drop out (they were reported as parse warnings).
func (p Package) themeRefs() []ThemeRef {
	seen := map[string]bool{}
	var out []ThemeRef
	for _, pattern := range p.Manifest.Themes {
		var matches []string
		if hasGlobMeta(pattern) {
			matches, _ = globThemePattern(p.Root, pattern)
		} else {
			abs := filepath.Join(p.Root, filepath.FromSlash(pattern))
			if info, err := os.Stat(abs); err == nil && info.Mode().IsRegular() {
				matches = []string{abs}
			}
		}
		for _, match := range matches {
			match = filepath.Clean(match)
			if seen[match] {
				continue
			}
			seen[match] = true
			base := filepath.Base(match)
			out = append(out, ThemeRef{
				Name: strings.TrimSuffix(base, filepath.Ext(base)),
				Path: match,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func (p Package) skillRefs() []SkillRef {
	var out []SkillRef
	seen := map[string]bool{}
	for _, rel := range p.Manifest.Skills {
		root := filepath.Join(p.Root, filepath.FromSlash(rel))
		p.scanSkillPath(root, 1, map[string]bool{}, &out)
	}
	filtered := out[:0]
	for _, sk := range out {
		key := sk.Path
		if key == "" {
			key = sk.Name
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		filtered = append(filtered, sk)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Name != filtered[j].Name {
			return filtered[i].Name < filtered[j].Name
		}
		return filtered[i].Path < filtered[j].Path
	})
	return filtered
}

func (p Package) scanSkillPath(path string, depth int, seen map[string]bool, out *[]SkillRef) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if !info.IsDir() {
		if info.Mode().IsRegular() && strings.EqualFold(filepath.Ext(path), ".md") {
			if sk, ok := parseSkillRef(path, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))); ok {
				*out = append(*out, sk)
			}
		}
		return
	}

	key := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		key = filepath.Clean(resolved)
	}
	if seen[key] {
		return
	}
	seen[key] = true

	if sk, ok := parseSkillRef(filepath.Join(path, "SKILL.md"), filepath.Base(path)); ok {
		*out = append(*out, sk)
		return
	}
	if depth >= 5 {
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if shouldSkipSkillScanDir(name) {
			continue
		}
		full := filepath.Join(path, name)
		if entry.IsDir() {
			p.scanSkillPath(full, depth+1, seen, out)
			continue
		}
		if entry.Type().IsRegular() && strings.EqualFold(filepath.Ext(name), ".md") {
			if sk, ok := parseSkillRef(full, strings.TrimSuffix(name, filepath.Ext(name))); ok {
				*out = append(*out, sk)
			}
		}
	}
}

func shouldSkipSkillScanDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch strings.ToLower(name) {
	case "assets", "node_modules", "references", "scripts":
		return true
	default:
		return false
	}
}

func parseSkillRef(path, stem string) (SkillRef, bool) {
	if !IsValidName(stem) {
		return SkillRef{}, false
	}
	b, err := fileencoding.ReadFileUTF8(path)
	if err != nil {
		return SkillRef{}, false
	}
	content := strings.TrimPrefix(strings.ReplaceAll(string(b), "\r\n", "\n"), "\uFEFF")
	fm, _ := frontmatter.Split(content)
	name := stem
	if v := strings.TrimSpace(fm["name"]); IsValidName(v) {
		name = v
	}
	return SkillRef{
		Name:        name,
		Description: strings.TrimSpace(fm["description"]),
		Path:        filepath.Clean(path),
		Invocation:  "/" + name,
		RunAs:       pluginSkillRunMode(fm),
	}, true
}

func pluginSkillRunMode(fm map[string]string) string {
	if strings.TrimSpace(fm["runas"]) == "subagent" {
		return "subagent"
	}
	if strings.EqualFold(strings.TrimSpace(fm["context"]), "fork") {
		return "subagent"
	}
	if strings.TrimSpace(fm["agent"]) != "" {
		return "subagent"
	}
	return "inline"
}

func (p Package) mcpServerRefs() []MCPServerRef {
	names := make([]string, 0, len(p.Manifest.MCPServers))
	for name := range p.Manifest.MCPServers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]MCPServerRef, 0, len(names))
	for _, name := range names {
		server := p.Manifest.MCPServers[name]
		out = append(out, MCPServerRef{
			Name:        name,
			DisplayName: firstNonEmpty(strings.TrimSpace(server.DisplayName), name),
			Description: strings.TrimSpace(server.Description),
			Transport:   pluginMCPTransport(server),
			Command:     strings.TrimSpace(server.Command),
			URL:         strings.TrimSpace(server.URL),
			AutoStart:   server.AutoStart == nil || *server.AutoStart,
		})
	}
	return out
}
