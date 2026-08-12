package config

import (
	"fmt"
	"reflect"
	"strings"

	"reasonix/internal/billing"
)

type RenderScope string

const (
	RenderScopeFull    RenderScope = "full"
	RenderScopeUser    RenderScope = "user"
	RenderScopeProject RenderScope = "project"
)

// RenderTOML renders the config as annotated TOML in the `reasonix setup` house style:
// comments preserved, system_prompt as a multi-line string, helpful hints. The
// output round-trips back through Load (see render_test.go).
func RenderTOML(c *Config) string {
	return RenderTOMLForScope(c, RenderScopeFull)
}

// tomlPluginsForScope keeps merged runtime entries in their owning config
// source. Unknown provenance is retained for callers that construct a Config
// directly before saving it to a specific target.
func tomlPluginsForScope(plugins []PluginEntry, scope RenderScope) []PluginEntry {
	if scope == RenderScopeFull {
		return plugins
	}
	out := make([]PluginEntry, 0, len(plugins))
	for _, pl := range plugins {
		switch pl.Source {
		case MCPSourceUnknown:
			out = append(out, pl)
		case MCPSourceUserConfig:
			if scope == RenderScopeUser {
				out = append(out, pl)
			}
		case MCPSourceProjectConfig:
			if scope == RenderScopeProject {
				out = append(out, pl)
			}
		}
	}
	return out
}

// RenderTOMLProjectDelta generates TOML containing only the sections and fields
// that differ from built-in defaults. Unlike RenderTOMLForScope (which renders
// the full config with comments), this emits clean TOML that can be surgically
// merged into an existing project config file via replaceTOMLSection.
func RenderTOMLProjectDelta(c *Config) string {
	if c == nil {
		return ""
	}
	d := Default()
	var b strings.Builder

	if v := configVersion(c); v != d.ConfigVersion {
		fmt.Fprintf(&b, "config_version = %d\n", v)
	}
	if c.DefaultModel != d.DefaultModel {
		fmt.Fprintf(&b, "default_model = %q\n", c.DefaultModel)
	}
	if c.Language != "" && c.Language != d.Language {
		fmt.Fprintf(&b, "language = %q\n", c.Language)
	}

	if !reflect.DeepEqual(c.UI, d.UI) {
		b.WriteString("[ui]\n")
		if c.UI.Theme != d.UI.Theme {
			fmt.Fprintf(&b, "theme = %q\n", c.UITheme())
		}
		if s := c.UIThemeStyle(); s != "" && s != d.UIThemeStyle() {
			fmt.Fprintf(&b, "theme_style = %q\n", s)
		}
		if l := c.UIShortcutLayout(); l != "classic" {
			fmt.Fprintf(&b, "shortcut_layout = %q\n", l)
		}
		if strings.TrimSpace(c.UI.CursorShape) != "" {
			fmt.Fprintf(&b, "cursor_shape = %q\n", c.UICursorShape())
		}
		if c.UI.CloseBehavior != d.UI.CloseBehavior {
			fmt.Fprintf(&b, "close_behavior = %q\n", c.DesktopCloseBehavior())
		}
		if c.UI.ShowReasoning != d.UI.ShowReasoning {
			fmt.Fprintf(&b, "show_reasoning = %v\n", c.UI.ShowReasoning)
		}
		if c.UI.ShowTurnUsage != d.UI.ShowTurnUsage {
			fmt.Fprintf(&b, "show_turn_usage = %v\n", c.UI.ShowTurnUsage)
		}
		b.WriteString("\n")
	}

	if !reflect.DeepEqual(c.Network, d.Network) {
		b.WriteString("[network]\n")
		if c.Network.ProxyMode != d.Network.ProxyMode {
			fmt.Fprintf(&b, "proxy_mode = %q\n", c.NetworkProxyMode())
		}
		if c.Network.ProxyURL != "" {
			fmt.Fprintf(&b, "proxy_url = %q\n", c.Network.ProxyURL)
		}
		if c.Network.NoProxy != "" {
			fmt.Fprintf(&b, "no_proxy = %q\n", c.Network.NoProxy)
		}
		if c.Network.Proxy.Type != "" || c.Network.Proxy.Server != "" || c.Network.Proxy.Port > 0 || c.Network.Proxy.Username != "" || c.Network.Proxy.Password != "" {
			b.WriteString("[network.proxy]\n")
			pt := c.Network.Proxy.Type
			if pt == "" {
				pt = "socks5"
			}
			fmt.Fprintf(&b, "type = %q\n", pt)
			if c.Network.Proxy.Server != "" {
				fmt.Fprintf(&b, "server = %q\n", c.Network.Proxy.Server)
			}
			if c.Network.Proxy.Port > 0 {
				fmt.Fprintf(&b, "port = %d\n", c.Network.Proxy.Port)
			}
			if c.Network.Proxy.Username != "" {
				fmt.Fprintf(&b, "username = %q\n", c.Network.Proxy.Username)
			}
			if c.Network.Proxy.Password != "" {
				fmt.Fprintf(&b, "password = %q\n", c.Network.Proxy.Password)
			}
		}
		b.WriteString("\n")
	}

	// [agent] section — per-field comparison
	var agentBuf strings.Builder
	anyAgent := false

	if sp := strings.TrimSpace(c.Agent.SystemPrompt); sp != "" && sp != d.Agent.SystemPrompt {
		agentBuf.WriteString("system_prompt = \"\"\"\n")
		agentBuf.WriteString(sp)
		agentBuf.WriteString("\"\"\"\n")
		anyAgent = true
	}
	if c.Agent.SystemPromptFile != "" && c.Agent.SystemPromptFile != d.Agent.SystemPromptFile {
		fmt.Fprintf(&agentBuf, "system_prompt_file = %q\n", c.Agent.SystemPromptFile)
		anyAgent = true
	}
	if c.Agent.Temperature != d.Agent.Temperature {
		fmt.Fprintf(&agentBuf, "temperature = %s\n", formatFloat(c.Agent.Temperature))
		anyAgent = true
	}
	if c.Agent.RecoveryModel != "" && c.Agent.RecoveryModel != d.Agent.RecoveryModel {
		fmt.Fprintf(&agentBuf, "recovery_model = %q\n", c.Agent.RecoveryModel)
		anyAgent = true
	}
	if c.Agent.ReasoningLanguage != d.Agent.ReasoningLanguage {
		if l := c.ReasoningLanguage(); l != "auto" {
			fmt.Fprintf(&agentBuf, "reasoning_language = %q\n", l)
			anyAgent = true
		}
	}
	if c.Agent.CompactRatio != d.Agent.CompactRatio {
		fmt.Fprintf(&agentBuf, "compact_ratio = %s\n", formatFloat(c.Agent.CompactRatio))
		anyAgent = true
	}
	if c.Agent.Keep != nil && !reflect.DeepEqual(c.Agent.Keep, d.Agent.Keep) {
		fmt.Fprintf(&agentBuf, "keep = %s\n", renderStringArray(c.Agent.Keep))
		anyAgent = true
	}
	if c.Agent.RecentKeep > 0 && c.Agent.RecentKeep != d.Agent.RecentKeep {
		fmt.Fprintf(&agentBuf, "recent_keep = %d\n", c.Agent.RecentKeep)
		anyAgent = true
	}
	if len(c.Agent.PlanModeReadOnlyCommands) > 0 && !reflect.DeepEqual(c.Agent.PlanModeReadOnlyCommands, d.Agent.PlanModeReadOnlyCommands) {
		fmt.Fprintf(&agentBuf, "plan_mode_read_only_commands = %s\n", renderStringArray(c.Agent.PlanModeReadOnlyCommands))
		anyAgent = true
	}
	if c.Agent.PlannerModel != "" && c.Agent.PlannerModel != d.Agent.PlannerModel {
		fmt.Fprintf(&agentBuf, "planner_model = %q\n", c.Agent.PlannerModel)
		anyAgent = true
	}
	if c.Agent.SubagentModel != "" && c.Agent.SubagentModel != d.Agent.SubagentModel {
		fmt.Fprintf(&agentBuf, "subagent_model = %q\n", c.Agent.SubagentModel)
		anyAgent = true
	}
	if len(c.Agent.SubagentModels) > 0 && !reflect.DeepEqual(c.Agent.SubagentModels, d.Agent.SubagentModels) {
		fmt.Fprintf(&agentBuf, "subagent_models = %s\n", renderStringMap(c.Agent.SubagentModels))
		anyAgent = true
	}
	if c.Agent.SubagentEffort != "" && c.Agent.SubagentEffort != d.Agent.SubagentEffort {
		fmt.Fprintf(&agentBuf, "subagent_effort = %q\n", c.Agent.SubagentEffort)
		anyAgent = true
	}
	if len(c.Agent.SubagentEfforts) > 0 && !reflect.DeepEqual(c.Agent.SubagentEfforts, d.Agent.SubagentEfforts) {
		fmt.Fprintf(&agentBuf, "subagent_efforts = %s\n", renderStringMap(c.Agent.SubagentEfforts))
		anyAgent = true
	}
	if c.Agent.MaxSubagentDepth != d.Agent.MaxSubagentDepth {
		fmt.Fprintf(&agentBuf, "max_subagent_depth = %d\n", c.Agent.MaxSubagentDepth)
		anyAgent = true
	}
	if c.Agent.OutputStyle != "" && c.Agent.OutputStyle != d.Agent.OutputStyle {
		fmt.Fprintf(&agentBuf, "output_style = %q\n", c.Agent.OutputStyle)
		anyAgent = true
	}

	if anyAgent {
		b.WriteString("[agent]\n")
		b.WriteString(agentBuf.String())
		b.WriteString("\n")
	}

	proj := projectScopedConfigForRender(c)
	if proj != nil && len(proj.Providers) > 0 && !reflect.DeepEqual(proj.Providers, d.Providers) {
		for _, p := range proj.Providers {
			b.WriteString("[[providers]]\n")
			fmt.Fprintf(&b, "name        = %q\n", p.Name)
			fmt.Fprintf(&b, "kind        = %q\n", p.Kind)
			fmt.Fprintf(&b, "base_url    = %q\n", p.BaseURL)
			if p.ChatURL != "" {
				fmt.Fprintf(&b, "chat_url    = %q\n", p.ChatURL)
			}
			if p.RequestURL != "" {
				fmt.Fprintf(&b, "request_url = %q\n", p.RequestURL)
			}
			if len(p.Models) > 0 {
				fmt.Fprintf(&b, "models      = %s\n", renderStringArray(p.Models))
				if p.Default != "" {
					fmt.Fprintf(&b, "default     = %q\n", p.Default)
				}
			} else if p.Model != "" {
				fmt.Fprintf(&b, "model       = %q\n", p.Model)
			}
			if p.ModelsURL != "" {
				fmt.Fprintf(&b, "models_url  = %q\n", p.ModelsURL)
			}
			fmt.Fprintf(&b, "api_key_env = %q\n", p.APIKeyEnv)
			if p.PresetID != "" {
				fmt.Fprintf(&b, "preset_id   = %q\n", p.PresetID)
			}
			if p.PresetVersion > 0 {
				fmt.Fprintf(&b, "preset_version = %d\n", p.PresetVersion)
			}
			if len(p.Headers) > 0 {
				fmt.Fprintf(&b, "headers     = %s\n", renderStringMap(p.Headers))
			}
			if len(p.ExtraBody) > 0 {
				fmt.Fprintf(&b, "extra_body  = %s\n", renderAnyMap(p.ExtraBody))
			}
			if p.AuthHeader {
				b.WriteString("auth_header = true\n")
			}
			if p.ResponsesMode != "" {
				fmt.Fprintf(&b, "responses_mode = %q\n", p.ResponsesMode)
			}
			if p.ResponsesStateful != nil {
				fmt.Fprintf(&b, "responses_stateful = %t\n", *p.ResponsesStateful)
			}
			if p.BalanceURL != "" {
				fmt.Fprintf(&b, "balance_url = %q\n", p.BalanceURL)
			}
			if p.ContextWindow > 0 {
				fmt.Fprintf(&b, "context_window = %d\n", p.ContextWindow)
			}
			if p.MaxOutputTokens != 0 {
				fmt.Fprintf(&b, "max_output_tokens = %d\n", p.MaxOutputTokens)
			}
			if p.Price != nil {
				fmt.Fprintf(&b, "price       = %s\n", renderPricingInline(p.Price))
			}
			if len(p.Prices) > 0 {
				fmt.Fprintf(&b, "prices      = %s\n", renderPricingMap(p.Prices))
			}
			if cur := strings.TrimSpace(p.BillingCurrency); cur != "" {
				fmt.Fprintf(&b, "billing_currency = %q\n", billing.NormalizeCurrency(cur))
			}
			if mode := strings.TrimSpace(p.BillingMode); mode != "" && mode != "payg" {
				fmt.Fprintf(&b, "billing_mode = %q\n", mode)
			}
			if p.Thinking != "" {
				fmt.Fprintf(&b, "thinking    = %q\n", p.Thinking)
			}
			if p.Effort != "" {
				fmt.Fprintf(&b, "effort      = %q\n", p.Effort)
			}
			if p.Vision {
				b.WriteString("vision      = true\n")
			}
			if p.VisionModels != nil {
				fmt.Fprintf(&b, "vision_models = %s\n", renderStringArray(p.VisionModels))
			}
			if p.VisionDetail != "" {
				fmt.Fprintf(&b, "vision_detail = %q\n", p.VisionDetail)
			}
			if p.WebSearch != nil {
				fmt.Fprintf(&b, "web_search  = %t\n", *p.WebSearch)
			}
			if p.ReasoningProtocol != "" {
				fmt.Fprintf(&b, "reasoning_protocol = %q\n", p.ReasoningProtocol)
			}
			if len(p.SupportedEfforts) > 0 {
				fmt.Fprintf(&b, "supported_efforts = %s\n", renderStringArray(p.SupportedEfforts))
			}
			if p.DefaultEffort != "" {
				fmt.Fprintf(&b, "default_effort    = %q\n", p.DefaultEffort)
			}
			if len(p.ModelOverrides) > 0 {
				fmt.Fprintf(&b, "model_overrides   = %s\n", renderModelOverrides(p.ModelOverrides))
			}
			if p.NoProxy {
				b.WriteString("no_proxy    = true\n")
			}
			b.WriteString("\n")
		}
	}

	if len(c.Tools.Enabled) > 0 ||
		(c.Tools.BashTimeoutSeconds != nil && *c.Tools.BashTimeoutSeconds != 0) ||
		(c.Tools.MCPStartupTimeoutSeconds != nil && *c.Tools.MCPStartupTimeoutSeconds > 0) ||
		(c.Tools.MCPCallTimeoutSeconds != nil && *c.Tools.MCPCallTimeoutSeconds > 0) {
		b.WriteString("[tools]\n")
		if len(c.Tools.Enabled) > 0 {
			fmt.Fprintf(&b, "enabled = %s\n", renderStringArray(c.Tools.Enabled))
		}
		if c.Tools.BashTimeoutSeconds != nil && *c.Tools.BashTimeoutSeconds != 0 {
			fmt.Fprintf(&b, "bash_timeout_seconds = %d\n", *c.Tools.BashTimeoutSeconds)
		}
		if c.Tools.MCPStartupTimeoutSeconds != nil && *c.Tools.MCPStartupTimeoutSeconds > 0 {
			fmt.Fprintf(&b, "mcp_startup_timeout_seconds = %d\n", *c.Tools.MCPStartupTimeoutSeconds)
		}
		if c.Tools.MCPCallTimeoutSeconds != nil && *c.Tools.MCPCallTimeoutSeconds > 0 {
			fmt.Fprintf(&b, "mcp_call_timeout_seconds = %d\n", *c.Tools.MCPCallTimeoutSeconds)
		}
		b.WriteString("\n")
	}

	if c.Tools.BackgroundJobs != d.Tools.BackgroundJobs {
		if c.Tools.BackgroundJobs.StalledWarningSeconds != nil && *c.Tools.BackgroundJobs.StalledWarningSeconds > 0 {
			b.WriteString("[tools.background_jobs]\n")
			fmt.Fprintf(&b, "stalled_warning_seconds = %d\n", *c.Tools.BackgroundJobs.StalledWarningSeconds)
			b.WriteString("\n")
		}
	}

	if !reflect.DeepEqual(c.Tools.Shell, d.Tools.Shell) {
		b.WriteString("[tools.shell]\n")
		if c.Tools.Shell.Prefer != d.Tools.Shell.Prefer {
			fmt.Fprintf(&b, "prefer = %q\n", c.Tools.Shell.Prefer)
		}
		if c.Tools.Shell.Path != d.Tools.Shell.Path {
			fmt.Fprintf(&b, "path = %q\n", c.Tools.Shell.Path)
		}
		b.WriteString("\n")
	}

	if !reflect.DeepEqual(c.LSP, d.LSP) {
		renderLSPConfig(&b, c.LSP)
	}

	if !reflect.DeepEqual(c.Skills, d.Skills) || len(c.explicitProjectSkillKeys) > 0 {
		b.WriteString("[skills]\n")
		if len(c.Skills.Paths) > 0 || c.keepsProjectSkillKey("paths") {
			fmt.Fprintf(&b, "paths = %s\n", renderStringArray(c.Skills.Paths))
		}
		if len(c.Skills.ExcludedPaths) > 0 || c.keepsProjectSkillKey("excluded_paths") {
			fmt.Fprintf(&b, "excluded_paths = %s\n", renderStringArray(c.Skills.ExcludedPaths))
		}
		if c.Skills.DisableImplicitInvocation || c.keepsProjectSkillKey("disable_implicit_invocation") {
			fmt.Fprintf(&b, "disable_implicit_invocation = %t\n", c.Skills.DisableImplicitInvocation)
		}
		if c.Skills.MaxDepth != 0 || c.keepsProjectSkillKey("max_depth") {
			depth := c.Skills.MaxDepth
			if depth != 0 {
				depth = c.SkillMaxDepth()
			}
			fmt.Fprintf(&b, "max_depth = %d\n", depth)
		}
		if disabled := c.DisabledSkillNames(); len(disabled) > 0 || c.keepsProjectSkillKey("disabled_skills") {
			fmt.Fprintf(&b, "disabled_skills = %s\n\n", renderStringArray(disabled))
		}
	}

	if !reflect.DeepEqual(c.Permissions, d.Permissions) {
		b.WriteString("[permissions]\n")
		mode := c.Permissions.Mode
		if mode == "" {
			mode = "ask"
		}
		if mode != "ask" {
			fmt.Fprintf(&b, "mode = %q\n", mode)
		}
		if c.Permissions.AllowDynamicBash {
			b.WriteString("allow_dynamic_bash = true\n")
		}
		if len(c.Permissions.Deny) > 0 {
			fmt.Fprintf(&b, "deny = %s\n", renderStringArray(c.Permissions.Deny))
		}
		if len(c.Permissions.Allow) > 0 {
			fmt.Fprintf(&b, "allow = %s\n", renderStringArray(c.Permissions.Allow))
		}
		if len(c.Permissions.Ask) > 0 {
			fmt.Fprintf(&b, "ask = %s\n", renderStringArray(c.Permissions.Ask))
		}
		b.WriteString("\n")
	}

	if !reflect.DeepEqual(c.Sandbox, d.Sandbox) {
		var sandboxBuf strings.Builder
		if c.Sandbox.WorkspaceRoot != "" {
			fmt.Fprintf(&sandboxBuf, "workspace_root = %q\n", c.Sandbox.WorkspaceRoot)
		}
		if len(c.Sandbox.AllowWrite) > 0 {
			fmt.Fprintf(&sandboxBuf, "allow_write = %s\n", renderStringArray(c.Sandbox.AllowWrite))
		}

		if strings.TrimSpace(c.Sandbox.Bash) != "" && c.BashMode() != d.BashModeForGOOS(runtimeGOOS) {
			fmt.Fprintf(&sandboxBuf, "bash = %q\n", c.BashMode())
		}
		if c.Sandbox.Network != d.Sandbox.Network {
			fmt.Fprintf(&sandboxBuf, "network = %v\n", c.Sandbox.Network)
		}
		if sandboxBuf.Len() > 0 {
			b.WriteString("[sandbox]\n")
			b.WriteString(sandboxBuf.String())
			b.WriteString("\n")
		}
	}

	if !reflect.DeepEqual(c.Statusline, d.Statusline) {
		b.WriteString("[statusline]\n")
		if c.Statusline.Command != "" {
			fmt.Fprintf(&b, "command = %q\n", c.Statusline.Command)
		}
		b.WriteString("\n")
	}

	for _, pl := range tomlPluginsForScope(c.Plugins, RenderScopeProject) {
		b.WriteString("[[plugins]]\n")
		fmt.Fprintf(&b, "name    = %q\n", pl.Name)
		if pl.Type != "" {
			fmt.Fprintf(&b, "type    = %q\n", pl.Type)
		}
		if pl.Command != "" {
			fmt.Fprintf(&b, "command = %q\n", pl.Command)
		}
		if len(pl.Args) > 0 {
			fmt.Fprintf(&b, "args    = %s\n", renderStringArray(pl.Args))
		}
		if pl.URL != "" {
			fmt.Fprintf(&b, "url     = %q\n", pl.URL)
		}
		if len(pl.Headers) > 0 {
			fmt.Fprintf(&b, "headers = %s\n", renderStringMap(pl.Headers))
		}
		if len(pl.Env) > 0 {
			fmt.Fprintf(&b, "env     = %s\n", renderStringMap(pl.Env))
		}
		if pl.StartupTimeoutSeconds > 0 {
			fmt.Fprintf(&b, "startup_timeout_seconds = %d\n", pl.StartupTimeoutSeconds)
		}
		if pl.CallTimeoutSeconds > 0 {
			b.WriteString("# Per-server MCP call timeout; 0 keeps the global/default cap.\n")
			fmt.Fprintf(&b, "call_timeout_seconds = %d\n", pl.CallTimeoutSeconds)
		}
		if hasPositiveIntMap(pl.ToolTimeoutSeconds) {
			b.WriteString("# Raw MCP tool names with per-tool call timeouts.\n")
			fmt.Fprintf(&b, "tool_timeout_seconds = %s\n", renderIntMap(pl.ToolTimeoutSeconds))
		}
		renderPluginPolicy(&b, pl)
		b.WriteString("\n")
	}

	return b.String()
}
