package main

import (
	"sort"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/skill"
	"reasonix/internal/tool"
)

// SkillsSettings returns the skills management snapshot without MCP status.
func (a *App) SkillsSettings() SkillsSettingsView {
	out := SkillsSettingsView{Skills: []SkillView{}, SkillRoots: []SkillRootView{}, AllowImplicitInvocation: true}
	a.mu.RLock()
	tab := a.activeTabLocked()
	var ctrl control.SessionAPI
	workspaceRoot := "."
	if tab != nil {
		ctrl = tab.Ctrl
		if strings.TrimSpace(tab.WorkspaceRoot) != "" {
			workspaceRoot = tab.WorkspaceRoot
		}
	}
	a.mu.RUnlock()
	if ctrl == nil {
		return out
	}

	disabled := map[string]bool{}
	var configuredModels, configuredEfforts map[string]string
	if cfg, err := config.LoadForRootReadOnly(workspaceRoot); err == nil {
		out.AllowImplicitInvocation = cfg.ImplicitSkillInvocationEnabled()
		for _, name := range cfg.Skills.DisabledSkills {
			if key := config.SkillNameKey(name); key != "" {
				disabled[key] = true
			}
		}
		configuredModels = cfg.Agent.SubagentModels
		configuredEfforts = cfg.Agent.SubagentEfforts
	}
	out.SkillRoots = a.cachedSkillRootsView(workspaceRoot)
	for _, s := range ctrl.AllSkills() {
		view := SkillView{
			Name: s.Name, Description: s.Description,
			Scope: string(s.Scope), SourceDir: skillSourceDir(s, out.SkillRoots), RunAs: string(s.RunAs),
			Enabled:          !disabled[config.SkillNameKey(s.Name)],
			Plugin:           s.Plugin,
			Model:            s.Model,
			Effort:           s.Effort,
			AllowedTools:     append([]string{}, s.AllowedTools...),
			ReadOnly:         s.ReadOnly,
			Color:            s.Color,
			Invocation:       "/" + s.SlashName(),
			InvocationMode:   s.Invocation,
			ConfiguredModel:  subagentOverrideFor(configuredModels, s.Name),
			ConfiguredEffort: subagentOverrideFor(configuredEfforts, s.Name),
		}

		if s.RunAs == skill.RunSubagent {
			view.Body = s.Body
		}
		out.Skills = append(out.Skills, view)
	}
	return out
}

// SetSkillImplicitInvocation persists whether the model may discover and
// invoke skills automatically, then rebuilds the active runtime. Explicit
// /skill invocation and skill management remain available in either mode.
func (a *App) SetSkillImplicitInvocation(enabled bool) error {
	err := a.applySkillConfigChange("disable_implicit_invocation", "skills policy", func(c *config.Config) error {
		c.SetSkillImplicitInvocation(enabled)
		return nil
	})
	if err == nil {
		a.invalidateSkillRootsCache()
	}
	return err
}

// subagentOverrideFor resolves a per-name subagent override with the same
// underscore/hyphen alias fallback the runtime dispatch uses
// (boot.SubagentModelKeys) — an exact-key read would show a legacy
// `security_review` config entry as "inherit default" while it still won at
// dispatch time.
func subagentOverrideFor(overrides map[string]string, name string) string {
	for _, key := range boot.SubagentModelKeys(name) {
		if v := strings.TrimSpace(overrides[key]); v != "" {
			return v
		}
	}
	return ""
}

// AvailableSubagentTools lists the tool names a subagent profile's
// "available tools" picker may offer. Scoped to compile-time builtins for
// v1 — MCP/plugin tools are per-session/per-connection and would need a new
// live-registry accessor on control.Capabilities to enumerate safely; a
// profile's allowed-tools already degrades gracefully (FilterRegistry drops
// unknown names silently) if extended to MCP names by hand later. Tools that
// are always excluded from every subagent regardless of an explicit
// allowlist (agent.AlwaysHiddenSubagentTools) are left out entirely — they'd
// be a selectable no-op otherwise.
func (a *App) AvailableSubagentTools() []ToolView {
	hidden := map[string]bool{}
	for _, name := range agent.AlwaysHiddenSubagentTools() {
		hidden[name] = true
	}
	entries := tool.BuiltinContractEntries()
	out := make([]ToolView, 0, len(entries))
	for _, e := range entries {
		if hidden[e.Name] {
			continue
		}
		out = append(out, ToolView{Name: e.Name, Description: e.Description, ReadOnlyHint: e.ReadOnly})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
