package main

import (
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/i18n"
	"reasonix/internal/pluginpkg"
	"reasonix/internal/skill"
)

// Commands lists the slash commands available this session — built-in actions,
// custom commands (.reasonix/commands), and MCP prompts — for the composer's "/"
// autocomplete menu.
func (a *App) Commands() []CommandInfo {
	out := []CommandInfo{
		{Name: "new", Description: i18n.M.CmdNew, Kind: "builtin", Group: "actions"},
		{Name: "clear", Description: i18n.M.CmdClear, Kind: "builtin", Group: "actions"},
		{Name: "compact", Description: i18n.M.CmdCompact, Kind: "builtin", Group: "actions"},
		{Name: "model", Description: i18n.M.CmdModel, Kind: "builtin", Group: "actions"},
		{Name: "provider", Description: i18n.M.CmdProvider, Kind: "builtin", Group: "management"},
		{Name: "effort", Description: i18n.M.CmdEffort, Kind: "builtin", Group: "actions"},
		{Name: "memory", Description: i18n.M.CmdMemory, Kind: "builtin", Group: "management"},
		{Name: "migrate", Description: i18n.M.CmdMigrate, Kind: "builtin", Group: "management"},
		{Name: "goal", Description: i18n.M.CmdGoal, Kind: "builtin", Group: "actions"},
		{Name: "remember", Description: i18n.M.CmdRemember, Kind: "builtin", Group: "management"},
		{Name: "mcp", Description: i18n.M.CmdMcp, Kind: "builtin", Group: "integrations"},
		{Name: "hooks", Description: i18n.M.CmdHooks, Kind: "builtin", Group: "management"},
		{Name: "plugins", Description: i18n.M.CmdPlugins, Kind: "builtin", Group: "integrations"},
		{Name: "theme", Description: i18n.M.CmdTheme, Kind: "builtin", Group: "management"},
		{Name: "skill", Description: i18n.M.CmdSkill, Kind: "builtin", Group: "skills"},
		{Name: "reload-cmd", Description: i18n.M.CmdReloadCmd, Kind: "builtin", Group: "management"},
	}
	a.mu.RLock()
	ctrl := a.activeCtrlLocked()
	a.mu.RUnlock()
	if ctrl == nil {
		return append(out, docsBuiltinCommand(control.DocsSlashName))
	}
	commands := ctrl.Commands()
	slashSkills := ctrl.SlashSkills()
	out = append(out, docsBuiltinCommand(control.ResolvedBuiltinSlashName(control.DocsSlashName, commands, slashSkills)))

	for _, s := range slashSkills {
		kind := "skill"
		if s.RunAs == skill.RunSubagent {
			kind = "subagent"
		}
		group := "skills"
		if kind == "subagent" {
			group = "subagents"
		}
		out = append(out, CommandInfo{Name: s.SlashName(), Description: s.Description, Kind: kind, Group: group, Plugin: s.Plugin, Color: s.Color})
	}
	for _, c := range commands {
		if c.Hidden {
			continue
		}
		out = append(out, CommandInfo{Name: c.Name, Description: c.Description, Hint: c.ArgHint, Kind: "custom", Group: "skills", Plugin: c.Plugin})
	}
	if h := ctrl.Host(); h != nil {
		for _, p := range h.Prompts() {
			out = append(out, CommandInfo{Name: p.Name, Description: p.Description, Kind: "mcp", Group: "integrations"})
		}
	}
	return resolveDocsCommand(out)
}

func docsBuiltinCommand(name string) CommandInfo {
	return CommandInfo{Name: name, Description: i18n.M.CmdDocs, Hint: "<question>", Kind: "builtin", Group: "integrations"}
}

func resolveDocsCommand(commands []CommandInfo) []CommandInfo {
	winner := -1
	winnerRank := -1
	for i, cmd := range commands {
		if cmd.Name != "docs" {
			continue
		}
		rank := 0
		switch cmd.Kind {
		case "custom":
			rank = 2
		case "skill", "subagent":
			rank = 1
		}
		if rank > winnerRank {
			winner = i
			winnerRank = rank
		}
	}
	if winner < 0 {
		return commands
	}
	out := make([]CommandInfo, 0, len(commands))
	for i, cmd := range commands {
		if cmd.Name != "docs" || i == winner {
			out = append(out, cmd)
		}
	}
	return out
}

// SlashArgItem is one sub-command / argument suggestion for the composer's slash
// menu (the part after the command word). Mirrors the CLI's arg completion via
// the shared control.SlashArgItems, so desktop and CLI offer the same hints.
type SlashArgItem struct {
	Label   string `json:"label"`
	Insert  string `json:"insert"`
	Hint    string `json:"hint"`
	Descend bool   `json:"descend"`
}

// SlashArgsResult carries the suggestions plus the byte offset in the input where
// the current token begins, so the composer replaces just that token.
type SlashArgsResult struct {
	Items []SlashArgItem `json:"items"`
	From  int            `json:"from"`
}

// SlashArgs completes the arguments of a management slash command (/mcp, /model,
// /skill, /hooks) for the composer — the same logic the chat TUI uses. Empty
// Items means the input has no structured arguments to complete.
func (a *App) SlashArgs(input string) SlashArgsResult {
	a.mu.RLock()
	ctrl := a.activeCtrlLocked()
	model := ""
	if tab := a.activeTabLocked(); tab != nil {
		model = tab.model
	}
	a.mu.RUnlock()
	if ctrl == nil {
		return SlashArgsResult{Items: []SlashArgItem{}}
	}
	data := control.ArgData{
		Skills:          ctrl.Skills(),
		DisabledSkills:  ctrl.DisabledSkills(),
		ConfiguredMCP:   ctrl.ConfiguredMCPNames(),
		DisconnectedMCP: ctrl.DisconnectedMCPNames(),
		CurrentModel:    model,
	}
	if names, err := pluginpkg.InstalledNames(config.ReasonixHomeDir()); err == nil {
		data.PluginNames = names
	}
	seen := map[string]bool{}
	for _, m := range a.Models() {
		data.ModelRefs = append(data.ModelRefs, m.Ref)
		if m.Provider != "" && !seen[m.Provider] {
			seen[m.Provider] = true
			data.ProviderNames = append(data.ProviderNames, m.Provider)
		}
		if m.Current {
			data.CurrentProvider = m.Provider
		}
	}
	if h := ctrl.Host(); h != nil {
		data.ServerNames = h.ServerNames()
	}
	items, from := control.SlashArgItems(input, data)

	out := SlashArgsResult{Items: []SlashArgItem{}, From: from}
	for _, it := range items {
		out.Items = append(out.Items, SlashArgItem{Label: it.Label, Insert: it.Insert, Hint: it.Hint, Descend: it.Descend})
	}
	return out
}

// CapabilitiesView is the MCP & Skills drawer's data: connected/failed MCP
// servers and the discoverable skills, the GUI counterpart to `/mcp` + `/skill`.
type CapabilitiesView struct {
	Servers    []ServerView    `json:"servers"`
	Skills     []SkillView     `json:"skills"`
	SkillRoots []SkillRootView `json:"skillRoots"`
	Plugins    []PluginView    `json:"plugins"`
}

// SkillsSettingsView is the skills management page's data, split from MCP
// status so opening MCP settings does not scan skill roots.
type SkillsSettingsView struct {
	Skills                  []SkillView     `json:"skills"`
	SkillRoots              []SkillRootView `json:"skillRoots"`
	AllowImplicitInvocation bool            `json:"allowImplicitInvocation"`
}

// ServerView is one MCP server for the drawer. Status is "connected" (with
// tool/prompt/resource counts), "deferred" (enabled but idle), "failed" (with
// the connection error), "initializing" (background startup in progress), or
// "disabled".
//
// Product fields for the simplified MCP panel are Enabled/Installed/
// Availability/RuntimeState/ToolCount/ToolList/Action. Legacy AutoStart, Tier,
// and StartIntent remain for one major as derived compatibility fields only.
type ServerView struct {
	Name                   string         `json:"name"`
	Transport              string         `json:"transport"`
	Status                 string         `json:"status"`
	StartIntent            string         `json:"startIntent,omitempty"` // deprecated: derived from Enabled
	RuntimeState           string         `json:"runtimeState,omitempty"`
	Availability           string         `json:"availability,omitempty"`
	Enabled                bool           `json:"enabled"`
	Installed              bool           `json:"installed"`
	Action                 string         `json:"action,omitempty"`
	Source                 string         `json:"source,omitempty"`
	ConfigSource           string         `json:"configSource,omitempty"`
	BuiltIn                bool           `json:"builtIn,omitempty"`
	Configured             bool           `json:"configured,omitempty"`
	AutoStart              bool           `json:"autoStart"` // deprecated: same as Enabled
	Tier                   string         `json:"tier,omitempty"`
	Command                string         `json:"command,omitempty"`
	Args                   []string       `json:"args,omitempty"`
	URL                    string         `json:"url,omitempty"`
	EnvKeys                []string       `json:"envKeys,omitempty"`
	HeaderKeys             []string       `json:"headerKeys,omitempty"`
	Tools                  int            `json:"tools"`
	ToolCount              int            `json:"toolCount"`
	Prompts                int            `json:"prompts"`
	Resources              int            `json:"resources"`
	HasTools               bool           `json:"hasTools,omitempty"`
	Error                  string         `json:"error,omitempty"`
	ToolList               []ToolView     `json:"toolList"`
	CallTimeoutSeconds     int            `json:"callTimeoutSeconds,omitempty"`
	ToolTimeoutSeconds     map[string]int `json:"toolTimeoutSeconds,omitempty"`
	RequiresLaunchApproval bool           `json:"requiresLaunchApproval,omitempty"`
	AuthStatus             string         `json:"authStatus,omitempty"`
	AuthURL                string         `json:"authUrl,omitempty"`
	AuthConfigured         bool           `json:"authConfigured,omitempty"`
	ManagedByPlugin        string         `json:"managedByPlugin,omitempty"`
}

type ToolView struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint bool   `json:"destructiveHint,omitempty"`
	SchemaError     string `json:"schemaError,omitempty"`
}

// SkillView is one discoverable skill for the drawer. Also backs the
// Subagents settings surface: the frontend filters this same list to
// RunAs=="subagent" rather than calling a second, redundant endpoint.
type SkillView struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Scope        string   `json:"scope"`
	SourceDir    string   `json:"sourceDir,omitempty"`
	RunAs        string   `json:"runAs"`
	Enabled      bool     `json:"enabled"`
	Plugin       string   `json:"plugin,omitempty"`
	Model        string   `json:"model,omitempty"`
	Effort       string   `json:"effort,omitempty"`
	AllowedTools []string `json:"allowedTools,omitempty"`
	// ReadOnly mirrors frontmatter read-only; omitted/false keeps the legacy
	// writable default for older profiles.
	ReadOnly bool   `json:"readOnly,omitempty"`
	Color    string `json:"color,omitempty"`
	// Invocation is the user-facing slash name; InvocationMode preserves the
	// frontmatter policy used by the subagent profile editor.
	Invocation     string `json:"invocation,omitempty"`
	InvocationMode string `json:"invocationMode,omitempty"`
	// Body is the skill's full markdown body (post-frontmatter) — the
	// subagent profile editor pre-fills its system-prompt field from this.
	Body string `json:"body,omitempty"`
	// ConfiguredModel/ConfiguredEffort are the per-name overrides from
	// cfg.Agent.SubagentModels/SubagentEfforts (internal/boot's
	// subagentModelRef/subagentEffortRef read the same map at dispatch time).
	// This is the only lever for a built-in subagent's model/effort, since
	// built-ins have no editable frontmatter file to carry Model/Effort.
	ConfiguredModel  string `json:"configuredModel,omitempty"`
	ConfiguredEffort string `json:"configuredEffort,omitempty"`
}

type SkillRootSkillView struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Scope        string   `json:"scope"`
	RunAs        string   `json:"runAs"`
	Plugin       string   `json:"plugin,omitempty"`
	Model        string   `json:"model,omitempty"`
	Effort       string   `json:"effort,omitempty"`
	AllowedTools []string `json:"allowedTools,omitempty"`
	Color        string   `json:"color,omitempty"`
	Invocation   string   `json:"invocation,omitempty"`
}

// SkillRootView is one skill discovery root for the drawer's Sources section.
type SkillRootView struct {
	Dir        string               `json:"dir"`
	Scope      string               `json:"scope"`
	Priority   int                  `json:"priority"`
	Status     string               `json:"status"`
	Enabled    bool                 `json:"enabled"`
	Configured bool                 `json:"configured"`
	Removable  bool                 `json:"removable"`
	Skills     int                  `json:"skills"`
	SkillItems []SkillRootSkillView `json:"skillItems,omitempty"`
	Warning    string               `json:"warning,omitempty"`
}
