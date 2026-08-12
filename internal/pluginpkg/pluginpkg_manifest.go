package pluginpkg

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func validateManifest(root string, m *Manifest) error {
	if !IsValidName(m.Name) {
		return fmt.Errorf("invalid plugin name %q", m.Name)
	}
	for _, p := range m.Skills {
		if err := validateRelativePath(p); err != nil {
			return err
		}
	}
	for _, p := range m.Commands {
		if err := validateRelativePath(p); err != nil {
			return err
		}
	}
	for _, p := range m.Agents {
		if err := validateRelativePath(p); err != nil {
			return err
		}
	}
	for _, p := range m.Prompts {
		if err := validateRelativePath(p); err != nil {
			return err
		}
	}
	for _, p := range m.Themes {
		if err := validateRelativePath(p); err != nil {
			return err
		}
	}
	for event, hooks := range m.Hooks {
		if strings.TrimSpace(event) == "" {
			return fmt.Errorf("hook event is required")
		}
		for _, h := range hooks {
			if h.Command == "" && h.ContextFile == "" {
				return fmt.Errorf("hook command or contextFile is required")
			}
			if !h.ArgsSet && !validHookShell(h.Shell) {
				return fmt.Errorf("hook shell %q is not supported (use auto, bash, powershell, pwsh, or cmd)", h.Shell)
			}
			if h.Command != "" && !h.ShellCommand && !filepath.IsAbs(h.Command) {
				if err := validateRelativePath(h.Command); err != nil {
					return err
				}
			}
			if h.ContextFile != "" {
				if err := validateRelativePath(h.ContextFile); err != nil {
					return err
				}
			}
			if h.Cwd != "" && !filepath.IsAbs(h.Cwd) {
				if err := validateRelativePath(h.Cwd); err != nil {
					return err
				}
			}
		}
	}
	for name := range m.MCPServers {
		if !IsValidName(name) {
			return fmt.Errorf("invalid MCP server name %q", name)
		}
	}
	if _, err := os.Stat(root); err != nil {
		return err
	}
	return nil
}

func validHookShell(shell string) bool {
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "", "auto", "bash", "powershell", "pwsh", "cmd":
		return true
	default:
		return false
	}
}

func validateRelativePath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("plugin path is required")
	}
	_, err := cleanPortableRelativePath(p)
	return err
}

// cleanPortableRelativePath applies the same manifest path contract on every
// host OS. A plugin prepared on Windows must not turn a drive/UNC path into a
// harmless-looking relative path on Unix, and a Unix-rooted path must remain
// absolute when the same package is parsed on Windows.
func cleanPortableRelativePath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	normalized := strings.ReplaceAll(trimmed, `\`, "/")
	if filepath.IsAbs(trimmed) || path.IsAbs(normalized) || windowsAbsolutePath.MatchString(normalized) {
		return "", fmt.Errorf("plugin path %q must be relative and stay inside the plugin root", trimmed)
	}
	cleaned := path.Clean(normalized)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("plugin path %q must be relative and stay inside the plugin root", trimmed)
	}
	return cleaned, nil
}

func (p Package) SkillRoots() []string {
	var out []string
	for _, rel := range p.Manifest.Skills {
		out = append(out, filepath.Join(p.Root, filepath.FromSlash(rel)))
	}
	sort.Strings(out)
	return out
}

func (p Package) AgentRoots() []string {
	var out []string
	for _, rel := range p.Manifest.Agents {
		out = append(out, filepath.Join(p.Root, filepath.FromSlash(rel)))
	}
	sort.Strings(out)
	return out
}

// CommandRoots returns the absolute command directories this package
// contributes to custom slash-command discovery.
func (p Package) CommandRoots() []string {
	var out []string
	for _, rel := range p.Manifest.Commands {
		out = append(out, filepath.Join(p.Root, filepath.FromSlash(rel)))
	}
	sort.Strings(out)
	return out
}

// PromptRoots returns the absolute prompt-template directories this package
// contributes through a native Manifest v2.
func (p Package) PromptRoots() []string {
	var out []string
	for _, rel := range p.Manifest.Prompts {
		out = append(out, filepath.Join(p.Root, filepath.FromSlash(rel)))
	}
	sort.Strings(out)
	return out
}

func (p Package) CapabilityCounts() (skills, commands, hooks, mcp int) {
	skills = len(p.skillRefs())
	commands = len(p.commandRefs())
	for _, hs := range p.Manifest.Hooks {
		hooks += len(hs)
	}
	mcp = len(p.Manifest.MCPServers)
	return
}

func (p Package) AgentCount() int { return len(p.agentRefs()) }

// PromptCount counts the prompt templates discovered under Manifest.Prompts.
func (p Package) PromptCount() int { return len(p.promptRefs()) }

// ThemeCount counts the theme files resolved from Manifest.Themes.
func (p Package) ThemeCount() int { return len(p.themeRefs()) }

// CapabilitySummary is the full per-package capability count set. The
// four-value CapabilityCounts predates native runtime manifests and keeps its
// signature for existing callers (the desktop module among them); newer fields
// live here.
type CapabilitySummary struct {
	Skills     int
	Agents     int
	Commands   int
	Hooks      int
	MCPServers int
	Prompts    int
	Themes     int
	Runtime    bool
}

// CapabilitySummary counts everything the package contributes, including
// native Manifest v2 prompts, themes, and runtime.
func (p Package) CapabilitySummary() CapabilitySummary {
	skills, commands, hooks, mcp := p.CapabilityCounts()
	return CapabilitySummary{
		Skills:     skills,
		Agents:     p.AgentCount(),
		Commands:   commands,
		Hooks:      hooks,
		MCPServers: mcp,
		Prompts:    len(p.promptRefs()),
		Themes:     len(p.themeRefs()),
		Runtime:    p.Manifest.Runtime != nil,
	}
}
