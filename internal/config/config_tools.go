package config

import (
	"strings"
)

// ToolsConfig selects which built-in tools are enabled. Empty means all of them.
type ToolsConfig struct {
	Enabled                  []string             `toml:"enabled"`
	BashTimeoutSeconds       *int                 `toml:"bash_timeout_seconds"`
	MCPStartupTimeoutSeconds *int                 `toml:"mcp_startup_timeout_seconds"`
	MCPCallTimeoutSeconds    *int                 `toml:"mcp_call_timeout_seconds"`
	BackgroundJobs           BackgroundJobsConfig `toml:"background_jobs"`
	Search                   SearchConfig         `toml:"search"`
	Shell                    ShellConfig          `toml:"shell"`
}

const (
	defaultBashTimeoutSeconds             = 120
	defaultMCPStartupTimeoutSeconds       = 30
	defaultMCPCallTimeoutSeconds          = 300
	defaultBackgroundJobStalledWarningSec = 900
	maxBackgroundJobStalledWarningSec     = 86400
)

// BashTimeoutSeconds returns the foreground bash timeout in seconds. An omitted
// config keeps the historical 120s safety cap, explicit 0 disables the
// tool-local cap, and positive values set a custom cap. Negative values fall
// back to the default so a typo cannot silently remove the safety net.
func (c *Config) BashTimeoutSeconds() int {
	if c.Tools.BashTimeoutSeconds == nil || *c.Tools.BashTimeoutSeconds < 0 {
		return defaultBashTimeoutSeconds
	}
	return *c.Tools.BashTimeoutSeconds
}

// MCPCallTimeoutSeconds returns the default MCP JSON-RPC call timeout in
// seconds. Omitted, zero, and negative values keep the built-in safety cap so a
// hung MCP server cannot block a turn indefinitely.
func (c *Config) MCPCallTimeoutSeconds() int {
	if c.Tools.MCPCallTimeoutSeconds == nil || *c.Tools.MCPCallTimeoutSeconds <= 0 {
		return defaultMCPCallTimeoutSeconds
	}
	return *c.Tools.MCPCallTimeoutSeconds
}

// MCPStartupTimeoutSeconds returns the background initialize + tools/list
// safety cap. Omitted, zero, and negative values keep the built-in default so
// a slow but healthy MCP can outlive the short interactive wait without running
// indefinitely.
func (c *Config) MCPStartupTimeoutSeconds() int {
	if c.Tools.MCPStartupTimeoutSeconds == nil || *c.Tools.MCPStartupTimeoutSeconds <= 0 {
		return defaultMCPStartupTimeoutSeconds
	}
	return *c.Tools.MCPStartupTimeoutSeconds
}

// BackgroundJobsConfig tunes parent-created background jobs.
type BackgroundJobsConfig struct {
	StalledWarningSeconds *int `toml:"stalled_warning_seconds"`
}

// BackgroundJobStalledWarningSeconds returns the stalled warning threshold in
// seconds. Omitted/negative values keep the default, explicit 0 disables the
// notice, and oversized values clamp to one day so a typo cannot become
// effectively invisible.
func (c *Config) BackgroundJobStalledWarningSeconds() int {
	if c.Tools.BackgroundJobs.StalledWarningSeconds == nil || *c.Tools.BackgroundJobs.StalledWarningSeconds < 0 {
		return defaultBackgroundJobStalledWarningSec
	}
	if *c.Tools.BackgroundJobs.StalledWarningSeconds > maxBackgroundJobStalledWarningSec {
		return maxBackgroundJobStalledWarningSec
	}
	return *c.Tools.BackgroundJobs.StalledWarningSeconds
}

// SearchConfig tunes the grep tool's engine. Engine is "auto" (default — use
// ripgrep when it's on PATH, else the native Go scanner), "native" (always Go),
// or "rg" (require ripgrep; warn at startup and fall back to native if absent).
// RgPath optionally points at a specific ripgrep binary instead of a PATH lookup.
type SearchConfig struct {
	Engine string `toml:"engine"`
	RgPath string `toml:"rg_path"`
}

// ShellConfig chooses the interpreter the bash tool runs commands under. Prefer
// is "auto" (default — real bash when present, else PowerShell on Windows),
// "bash", or "powershell"/"pwsh" (force it; warn at startup and fall back to
// auto if absent). Path optionally points at a specific shell executable.
type ShellConfig struct {
	Prefer string `toml:"prefer"`
	Path   string `toml:"path"`
}

// PermissionsConfig declares the per-call permission policy (see
// internal/permission). Mode is the fallback decision for writer tools when no
// rule matches ("ask" | "allow" | "deny"; default "ask"); read-only tools always
// fall back to allow. Allow/Ask/Deny are rule lists of the form "ToolName" or
// "ToolName(glob)". Precedence: deny > ask > allow > fallback.
type PermissionsConfig struct {
	Mode             string   `toml:"mode"`
	Allow            []string `toml:"allow"`
	Ask              []string `toml:"ask"`
	Deny             []string `toml:"deny"`
	AllowDynamicBash bool     `toml:"allow_dynamic_bash"`
}

// MCPConfigSource records where a merged MCP entry came from. It is runtime
// provenance only and is never serialized back into TOML or .mcp.json.
type MCPConfigSource string

const (
	MCPSourceUnknown        MCPConfigSource = ""
	MCPSourceUserConfig     MCPConfigSource = "user_config"
	MCPSourceProjectConfig  MCPConfigSource = "project_config"
	MCPSourceProjectMCPJSON MCPConfigSource = "project_mcp_json"
	MCPSourceLegacyUser     MCPConfigSource = "legacy_user_config"
	MCPSourcePluginPackage  MCPConfigSource = "plugin_package"
)

func (s MCPConfigSource) UserAuthorized() bool {
	switch s {
	case MCPSourceUserConfig, MCPSourceLegacyUser, MCPSourcePluginPackage,
		MCPSourceProjectConfig, MCPSourceProjectMCPJSON:
		return true
	default:
		return false
	}
}

// ProjectScoped reports whether an MCP entry belongs to one workspace. Project
// scope remains useful for provenance, activation, and relative-path handling;
// it no longer implies a separate launch-approval workflow.
func (s MCPConfigSource) ProjectScoped() bool {
	return s == MCPSourceProjectConfig || s == MCPSourceProjectMCPJSON
}

func (e PluginEntry) ShouldAutoStart() bool {
	return e.AutoStart == nil || *e.AutoStart
}

// ResolvedTier returns the normalized tier ("eager"|"background") with the
// project default applied. Legacy lazy and unknown values fall back to
// background so enabled MCPs are available without manual connection.
//
// Tier no longer changes runtime process start timing; it remains for config
// compatibility and diagnostics only.
func (e PluginEntry) ResolvedTier() string {
	return resolvedMCPTier(e.Tier)
}

func resolvedMCPTier(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "eager":
		return "eager"
	case "background", "lazy":
		return "background"
	case "":
		return "background"
	default:
		return "background"
	}
}

// AutoStartPlugins returns enabled MCP entries for the catalog. Durable
// enable/disable overrides in mcp-activation.json take precedence over the
// legacy auto_start field. auto_start=false without an override still maps to
// disabled; true/nil map to enabled. "Auto start" no longer means "spawn the
// process at session boot" — enabled servers register cached tools and start
// on first real tool call.
func (c *Config) AutoStartPlugins() []PluginEntry {
	return c.EnabledPlugins("", DefaultMCPActivationStore())
}

// EnabledPlugins returns catalog-enabled MCP entries for workspace, consulting
// the activation store when provided.
func (c *Config) EnabledPlugins(workspace string, activation *MCPActivationStore) []PluginEntry {
	if c == nil {
		return nil
	}
	out := make([]PluginEntry, 0, len(c.Plugins))
	for _, p := range c.Plugins {
		enabled := p.ShouldAutoStart()
		if activation != nil {
			if resolved, err := activation.IsEnabled(p, workspace); err == nil {
				enabled = resolved
			}
		}
		if enabled {
			out = append(out, p)
		}
	}
	return out
}
