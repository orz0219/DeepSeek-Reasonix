// Package config loads Reasonix's runtime configuration from TOML. Resolution order:
// flag > project ./reasonix.toml > user config.toml (in the OS user-config dir) > built-in defaults.
// Secrets come from the environment via api_key_env and are never stored in
// config files.
package config

// IsValidSkillName reports whether name is a usable skill identifier.

// SkillNameKey normalizes a skill identifier for config comparisons.

// Config is Reasonix's runtime configuration.
type Config struct {
	ConfigVersion    int                 `toml:"config_version"`
	DefaultModel     string              `toml:"default_model"`
	Language         string              `toml:"language"` // ui/model language tag (e.g. "zh"); empty = auto-detect from $LANG / $REASONIX_LANG
	CredentialsStore string              `toml:"credentials_store"`
	UI               UIConfig            `toml:"ui"`
	CLI              CLIConfig           `toml:"cli"`
	Desktop          DesktopConfig       `toml:"desktop"`
	Billing          BillingConfig       `toml:"billing"`
	Telemetry        TelemetryConfig     `toml:"telemetry"`
	Notifications    NotificationsConfig `toml:"notifications"`
	Agent            AgentConfig         `toml:"agent"`
	Providers        []ProviderEntry     `toml:"providers"`
	Tools            ToolsConfig         `toml:"tools"`
	Permissions      PermissionsConfig   `toml:"permissions"`
	Sandbox          SandboxConfig       `toml:"sandbox"`
	Network          NetworkConfig       `toml:"network"`
	Environment      EnvironmentConfig   `toml:"environment"`
	Plugins          []PluginEntry       `toml:"plugins"`
	Skills           SkillsConfig        `toml:"skills"`
	Statusline       StatuslineConfig    `toml:"statusline"`
	LSP              LSPConfig           `toml:"lsp"`
	Serve            ServeConfig         `toml:"serve"`
	Secrets          SecretsConfig       `toml:"secrets"`
	Remote           RemoteConfig        `toml:"remote"`

	systemPromptFileSource     promptFileSource
	providerSources            map[string]providerSourceScope
	shadowedProjectProviders   []ProviderEntry
	ignoredProjectDefaultModel string
	ignoredLegacyStepLimits    bool
	expansionEnv               map[string]string
	pluginPackageOwners        map[string]string
	pluginPackageSkillOwners   map[string][]string
	pluginPackageAgentOwners   map[string][]string
	// explicitProjectSkillKeys records project-level skill fields that the
	// settings UI intentionally owns even when their value equals the built-in
	// default. It is transient edit metadata and is never serialized directly.
	explicitProjectSkillKeys map[string]bool
	editLoadErr              error
	// loadWarnings are non-fatal issues observed while loading config (corrupt
	// user/project files recovered via last-known-good or defaults). They never
	// rewrite the original file; the UI may surface them for doctor repair.
	loadWarnings []string
}

// KeepProjectSkillKey marks a skill field as an intentional project override.
// An explicit empty/false project value must still be written so it can
// override a non-default user setting in the layered configuration.

// IsMissingSystemPromptFile reports whether every allowed location for a
// configured prompt file was absent. Permission, containment, and other I/O
// failures deliberately return false so callers do not start without an
// explicitly configured prompt.

// TelemetryConfig controls content-free CLI usage metrics. It is user-global:
// project reasonix.toml values are ignored so a cloned repository cannot opt a
// user into reporting.

// auto|on|off; empty means consent has not been requested

// CLITelemetryConfigured reports whether the user has made an explicit CLI
// telemetry choice. The runtime policy still treats an absent value as auto,
// but persistence must preserve absence until the first eligible consent prompt.

// CLITelemetryMode returns the normalized CLI telemetry policy.

// LoadWarnings returns non-fatal config load issues (corrupt files recovered in
// memory). The returned slice is a copy.

// HasLoadWarnings reports whether the load used a degraded in-memory fallback.

// IgnoredLegacyAgentStepLimits reports whether this load found and ignored the
// retired [agent].max_steps or planner_max_steps settings. Boot removes standard
// key assignments before loading, while read-only/config-only loads only report
// and normalize them in memory.

// IgnoredProjectDefaultModel returns the project reasonix.toml default_model
// that LoadForRoot ignored because no configured provider serves it (see
// restoreUnresolvableProjectDefaultModel), or "" when none was ignored.

// SecretsConfig controls the credential protection layers. It is a user-global
// setting: project reasonix.toml values are ignored (see LoadForRoot), so a
// cloned repository cannot silently opt the user into workflow-breaking
// protections.

// FilterSubprocessEnv strips credential-like environment variables
// (*_API_KEY, *TOKEN*, *SECRET*, ...) from tool subprocesses (bash, hooks,
// LSP, MCP stdio). Default off: it breaks token-based workflows such as
// `gh`, HTTPS `git push`, and `npm publish`.

// ProtectSensitiveFiles makes read/list/search tools treat credential
// paths (.env, .git-credentials, .netrc, *.pem/*.key/*.p12/*.pfx, ~/.ssh)
// as invisible. Default off because hiding the files breaks legitimate
// "edit my .env" workflows.

// UIConfig controls CLI presentation-only settings. Desktop appearance is kept in
