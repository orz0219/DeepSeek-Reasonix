package config

import (
	"net/url"
	"os"
	"path/filepath"
	"reasonix/internal/netclient"
	"strings"
)

// LSPConfig governs the optional Language Server Protocol tools (lsp_definition,
// lsp_references, lsp_hover, lsp_diagnostics). Enabled defaults to true; the
// servers themselves are never bundled — each resolves on PATH and the tool
// returns an install hint when it is missing, so the capability is dormant until
// the user installs a server. Servers overrides or extends the built-in language
// → server map, keyed by language id (e.g. "go", "rust", "python").
type LSPConfig struct {
	Enabled bool                 `toml:"enabled"`
	Servers map[string]LSPServer `toml:"servers"`
}

// LSPServer overrides a built-in language's server or, when keyed by a new
// language, adds one. An empty field falls back to the built-in default for that
// language; Extensions is required when adding a language the built-ins don't
// cover (e.g. ".ex" for Elixir) so files route to it.
type LSPServer struct {
	Command     string            `toml:"command"`
	Args        []string          `toml:"args"`
	Env         map[string]string `toml:"env"`
	LanguageID  string            `toml:"language_id"`
	Extensions  []string          `toml:"extensions"`
	InstallHint string            `toml:"install_hint"`
}

// StatuslineConfig configures a custom status line. Command, when set, is run at
// startup and after each turn; its first line of stdout replaces the built-in
// status data row. A JSON payload (model, context tokens, cwd) is fed on stdin.
type StatuslineConfig struct {
	Command string `toml:"command"`
}

// BotConfig 控制多渠道 IM bot 消息网关。
type BotConfig struct {
	Enabled            bool                  `toml:"enabled"`
	Model              string                `toml:"model"` // 用于 bot 的模型名，空则用 default_model
	ToolApprovalMode   string                `toml:"tool_approval_mode"`
	MaxSteps           int                   `toml:"max_steps"`
	DebounceMs         int                   `toml:"debounce_ms"` // 消息合并窗口，毫秒
	QueueMode          string                `toml:"queue_mode"`  // steer|followup|collect|interrupt
	QueueCap           int                   `toml:"queue_cap"`
	QueueDrop          string                `toml:"queue_drop"` // summarize|old|new
	IgnoreSelfMessages bool                  `toml:"ignore_self_messages"`
	SelfUserIDs        BotSelfUserIDs        `toml:"self_user_ids"`
	Control            BotControlConfig      `toml:"control"`
	Pairing            BotPairingConfig      `toml:"pairing"`
	Allowlist          BotAllowlist          `toml:"allowlist"`
	QQ                 QQBotConfig           `toml:"qq"`
	Feishu             FeishuBotConfig       `toml:"feishu"`
	Weixin             WeixinBotConfig       `toml:"weixin"`
	Routes             []BotRouteConfig      `toml:"routes"`
	Connections        []BotConnectionConfig `toml:"connections"`
	// DesktopWatchers persists /desktop watch subscriptions so god-view
	// notifications survive a desktop restart. Managed by the desktop bot
	// bridge, not the settings UI.
	DesktopWatchers []BotDesktopWatcherConfig `toml:"desktop_watchers"`
}

// BotDesktopWatcherConfig is one bot chat subscribed to desktop events
// (/desktop watch on).
type BotDesktopWatcherConfig struct {
	Platform     string `toml:"platform"`
	ConnectionID string `toml:"connection_id"`
	Domain       string `toml:"domain"`
	ChatType     string `toml:"chat_type"`
	ChatID       string `toml:"chat_id"`
}

type BotSelfUserIDs struct {
	QQ     []string `toml:"qq"`
	Feishu []string `toml:"feishu"`
	Weixin []string `toml:"weixin"`
}

type BotControlConfig struct {
	Enabled  bool   `toml:"enabled"`
	Addr     string `toml:"addr"`
	TokenEnv string `toml:"token_env"`
}

type BotRouteConfig struct {
	ConnectionID     string `toml:"connection_id"`
	Platform         string `toml:"platform"`
	ChatType         string `toml:"chat_type"`
	ChatID           string `toml:"chat_id"`
	UserID           string `toml:"user_id"`
	ThreadID         string `toml:"thread_id"`
	Model            string `toml:"model"`
	ToolApprovalMode string `toml:"tool_approval_mode"`
	WorkspaceRoot    string `toml:"workspace_root"`
}

// BotAllowlist 控制哪些用户可以使用 bot。
type BotAllowlist struct {
	Enabled         bool     `toml:"enabled"`
	AllowAll        bool     `toml:"allow_all"`
	QQUsers         []string `toml:"qq_users"`
	FeishuUsers     []string `toml:"feishu_users"`
	WeixinUsers     []string `toml:"weixin_users"`
	QQApprovers     []string `toml:"qq_approvers"`
	FeishuApprovers []string `toml:"feishu_approvers"`
	WeixinApprovers []string `toml:"weixin_approvers"`
	QQAdmins        []string `toml:"qq_admins"`
	FeishuAdmins    []string `toml:"feishu_admins"`
	WeixinAdmins    []string `toml:"weixin_admins"`
	QQGroups        []string `toml:"qq_groups"`
	FeishuGroups    []string `toml:"feishu_groups"`
	WeixinGroups    []string `toml:"weixin_groups"`
}

type BotPairingConfig struct {
	Enabled               bool `toml:"enabled"`
	RequestTTLMinutes     int  `toml:"request_ttl_minutes"`
	MaxPendingPerPlatform int  `toml:"max_pending_per_platform"`
}

// BotAccessConfig controls who may use one concrete bot connection.
type BotAccessConfig struct {
	Enabled        bool     `toml:"enabled"`
	AllowAll       bool     `toml:"allow_all"`
	PairingEnabled bool     `toml:"pairing_enabled"`
	Users          []string `toml:"users"`
	Groups         []string `toml:"groups"`
	Approvers      []string `toml:"approvers"`
	Admins         []string `toml:"admins"`
}

// QQBotConfig QQ 官方 Bot API v2 配置。
type QQBotConfig struct {
	Enabled          bool            `toml:"enabled"`
	AppID            string          `toml:"app_id"`
	AppSecretEnv     string          `toml:"app_secret_env"` // 环境变量名，如 QQ_BOT_APP_SECRET
	Sandbox          bool            `toml:"sandbox"`        // true 使用 QQ 沙箱 API / gateway
	Model            string          `toml:"model"`
	ToolApprovalMode string          `toml:"tool_approval_mode"`
	WorkspaceRoot    string          `toml:"workspace_root"`
	Access           BotAccessConfig `toml:"access"`
}

// FeishuBotConfig 飞书自建应用 Bot 配置。
type FeishuBotConfig struct {
	Enabled           bool   `toml:"enabled"`
	Domain            string `toml:"domain"` // feishu（默认）| lark
	AppID             string `toml:"app_id"`
	AppSecretEnv      string `toml:"app_secret_env"`     // 如 FEISHU_BOT_APP_SECRET
	VerificationToken string `toml:"verification_token"` // 事件订阅验证 token
	Mode              string `toml:"mode"`               // webhook（默认）| websocket
	WebhookPort       int    `toml:"webhook_port"`       // webhook 模式端口
	RequireMention    bool   `toml:"require_mention"`
	// OutboundMediaRoots contains absolute local directories the loopback /send
	// control API may attach files from. Media refs must be bare filenames and
	// must exist in exactly one configured root. Empty (the default) disables
	// outbound file sending.
	OutboundMediaRoots []string `toml:"outbound_media_roots"`
}

// WeixinBotConfig 微信 iLink Bot 配置。
type WeixinBotConfig struct {
	Enabled   bool   `toml:"enabled"`
	AccountID string `toml:"account_id"`
	TokenEnv  string `toml:"token_env"` // 环境变量名，如 WEIXIN_BOT_TOKEN
	APIBase   string `toml:"api_base"`  // iLink API base URL
}

// BotConnectionConfig is the desktop-friendly connection record for IM bot
// channels. It keeps install/runtime state separate from legacy per-provider
// knobs so the UI can expose a simple "connect first" flow while old configs
// keep working.
type BotConnectionConfig struct {
	ID               string                        `toml:"id"`
	Provider         string                        `toml:"provider"` // qq|feishu|weixin
	Domain           string                        `toml:"domain"`   // feishu|lark|weixin|qq
	Label            string                        `toml:"label"`
	Enabled          bool                          `toml:"enabled"`
	Status           string                        `toml:"status"` // disconnected|pending|connected|error
	Model            string                        `toml:"model"`
	ToolApprovalMode string                        `toml:"tool_approval_mode"`
	WorkspaceRoot    string                        `toml:"workspace_root"`
	Access           BotAccessConfig               `toml:"access"`
	Credential       BotConnectionCredential       `toml:"credential"`
	SessionMappings  []BotConnectionSessionMapping `toml:"session_mappings"`
	LastError        string                        `toml:"last_error"`
	CreatedAt        string                        `toml:"created_at"`
	UpdatedAt        string                        `toml:"updated_at"`
}

type BotConnectionCredential struct {
	AppID        string `toml:"app_id"`
	AppSecretEnv string `toml:"app_secret_env"`
	AccountID    string `toml:"account_id"`
	TokenEnv     string `toml:"token_env"`
}

type BotConnectionSessionMapping struct {
	RemoteID      string `toml:"remote_id"`
	SessionID     string `toml:"session_id"`
	SessionSource string `toml:"session_source"`
	ChatType      string `toml:"chat_type"`
	UserID        string `toml:"user_id"`
	ThreadID      string `toml:"thread_id"`
	Scope         string `toml:"scope"`
	WorkspaceRoot string `toml:"workspace_root"`
	UpdatedAt     string `toml:"updated_at"`
}

// ServeConfig controls the HTTP serve frontend security settings.
type ServeConfig struct {
	// AuthMode selects the authentication mode for the HTTP serve frontend.
	// "none" (default): no authentication.
	// "token": a pre-shared token in the URL query string.
	// "password": a login page with bcrypt password verification.
	AuthMode string `toml:"auth_mode"`
	// Token is a pre-shared token for auth_mode = "token". When empty, a
	// cryptographically random token is generated at startup and printed.
	Token string `toml:"token"`
	// PasswordHash is a bcrypt hash of the password for auth_mode = "password".
	// Generate one with: reasonix serve --hash-password --password '...'
	PasswordHash string `toml:"password_hash"`
	// BehindProxy indicates the server sits behind a trusted reverse proxy
	// (nginx, Caddy, Cloudflare, etc.) that sets X-Forwarded-For and
	// X-Forwarded-Proto headers. When true, those headers are used for
	// rate-limiting and Secure-cookie decisions. When false (default), they
	// are ignored — an attacker can otherwise forge them.
	BehindProxy bool `toml:"behind_proxy"`
}

// NetworkConfig controls ordinary outbound HTTP traffic such as model providers,
// wallet-balance lookups, updater checks, CodeGraph downloads, and web_fetch.
// web_fetch reuses these proxy settings while keeping its own SSRF-guarded
// dialer.
type NetworkConfig struct {
	// ProxyMode is "auto" (default; environment proxy for now), "env", "custom",
	// or "off". auto leaves room for OS proxy detection later without changing the
	// config shape.
	ProxyMode string `toml:"proxy_mode"`
	// ProxyURL is an advanced custom override such as "socks5://127.0.0.1:7890".
	// When set and proxy_mode = "custom", it wins over the structured proxy table.
	ProxyURL string `toml:"proxy_url"`
	// NoProxy is honored for custom proxies. Env/auto modes use NO_PROXY from the
	// process environment instead.
	NoProxy string             `toml:"no_proxy"`
	Proxy   NetworkProxyConfig `toml:"proxy"`
}

// NetworkProxyConfig is the structured custom-proxy editor shape. Password is
// optional and supports ${VAR} expansion, so users can avoid storing it literally.
type NetworkProxyConfig struct {
	Type     string `toml:"type"` // http|https|socks5|socks5h
	Server   string `toml:"server"`
	Port     int    `toml:"port"`
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// NetworkProxySpec returns the expanded proxy settings used by netclient.
func (c *Config) NetworkProxySpec() netclient.ProxySpec {
	return netclient.ProxySpec{
		Mode:        c.Network.ProxyMode,
		URL:         c.expandVars(c.Network.ProxyURL),
		NoProxy:     c.expandVars(c.Network.NoProxy),
		Type:        c.Network.Proxy.Type,
		Server:      c.expandVars(c.Network.Proxy.Server),
		Port:        c.Network.Proxy.Port,
		Username:    c.expandVars(c.Network.Proxy.Username),
		Password:    c.expandVars(c.Network.Proxy.Password),
		DirectHosts: c.directProxyHosts(),
	}
}

// directProxyHosts collects the base_url hosts of providers marked no_proxy, so
// netclient bypasses the proxy for them without knowing any provider by name.
//
// Only for an auto-detected proxy (auto/env): that proxy is typically a
// GFW-circumvention one not meant for domestic endpoints (e.g. mimo), so keep
// them direct. An explicit proxy_mode = "custom" is the user saying "route
// everything through this" — e.g. a mandatory corporate proxy — so honor it for
// every provider; a custom-proxy user who wants a host direct uses
// network.no_proxy instead (#3635).
func (c *Config) directProxyHosts() []string {
	if c.NetworkProxyMode() == netclient.ModeCustom {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range c.Providers {
		if !p.NoProxy {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(p.BaseURL))
		if err != nil {
			continue
		}
		if h := u.Hostname(); h != "" && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// NetworkProxyMode normalizes network.proxy_mode to a known value.
func (c *Config) NetworkProxyMode() string {
	return netclient.NormalizeMode(c.Network.ProxyMode)
}

// SkillsConfig configures skill discovery. Paths adds extra "custom"-scope skill
// roots — each a directory of SKILL.md / <name>.md playbooks — scanned between
// the project roots (.reasonix/.agents/.agent/.claude under the workspace) and
// the global roots. ExcludedPaths hides matching discovery roots without deleting
// folders. ~, relative paths, and ${VAR} expansion are supported. DisabledSkills
// hides named skills from the agent prompt, slash invocation, and skill tools
// while keeping them manageable. DisableImplicitInvocation keeps skills
// discoverable to the host for explicit /skill use and management, but hides
// their index and model-facing invocation tools.
type SkillsConfig struct {
	Paths                     []string `toml:"paths"`
	ExcludedPaths             []string `toml:"excluded_paths"`
	DisabledSkills            []string `toml:"disabled_skills"`
	DisableImplicitInvocation bool     `toml:"disable_implicit_invocation"`
	MaxDepth                  int      `toml:"max_depth"`
}

// ImplicitSkillInvocationEnabled reports whether the model may discover and
// invoke skills without an explicit user slash command. The zero value keeps
// the historical default enabled for old configs.
func (c *Config) ImplicitSkillInvocationEnabled() bool {
	return c == nil || !c.Skills.DisableImplicitInvocation
}

// SkillCustomPaths returns the configured custom skill roots with ${VAR}
// expanded; empty entries are dropped.
func (c *Config) SkillCustomPaths() []string {
	var out []string
	for _, p := range c.Skills.Paths {
		if p = c.expandVars(p); strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// SkillExcludedPaths returns configured skill roots that should be hidden from
// discovery, with ${VAR} expanded and empty entries dropped.
func (c *Config) SkillExcludedPaths() []string {
	var out []string
	for _, p := range c.Skills.ExcludedPaths {
		if p = c.expandVars(p); strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// SkillMaxDepth bounds nested skill discovery. Depth 3 favors bundled skill
// packs while Store keeps nested markdown safe by requiring descriptions.
func (c *Config) SkillMaxDepth() int {
	const (
		defaultDepth = 3
		maxDepth     = 5
	)
	if c == nil || c.Skills.MaxDepth == 0 {
		return defaultDepth
	}
	if c.Skills.MaxDepth < 1 {
		return 1
	}
	if c.Skills.MaxDepth > maxDepth {
		return maxDepth
	}
	return c.Skills.MaxDepth
}

// DisabledSkillNames returns valid disabled skill identifiers, preserving the
// first spelling and dropping duplicates/empty entries.
func (c *Config) DisabledSkillNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range c.Skills.DisabledSkills {
		name = strings.TrimSpace(name)
		if !IsValidSkillName(name) {
			continue
		}
		key := SkillNameKey(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// IsSkillDisabled reports whether name is configured as disabled.
func (c *Config) IsSkillDisabled(name string) bool {
	key := SkillNameKey(name)
	if key == "" {
		return false
	}
	for _, disabled := range c.DisabledSkillNames() {
		if SkillNameKey(disabled) == key {
			return true
		}
	}
	return false
}

// SandboxConfig bounds the blast radius of tool calls (Phase 0: file-writer
// confinement). WorkspaceRoot is the directory the built-in file writers
// (write_file / edit_file / multi_edit / move_file) may modify; empty means the
// current working directory, so writes stay inside the project by default.
// AllowWrite lists extra directories writers may also touch (e.g. a sibling repo
// or a temp dir). ForbidRead lists files or directories the agent may not read or list
// (e.g. ~/.ssh for secrets). Both support ${VAR} / ${VAR:-default} expansion. Reads are
// unrestricted; confining `bash` is Phase 1 (OS-level sandbox).
type SandboxConfig struct {
	WorkspaceRoot string   `toml:"workspace_root"`
	AllowWrite    []string `toml:"allow_write"`
	ForbidRead    []string `toml:"forbid_read"`
	// Bash is the OS-sandbox mode for the bash tool: "enforce" jails each
	// command when an OS sandbox is available and refuses bash otherwise; "off"
	// runs it unconfined. Empty uses the platform default.
	Bash string `toml:"bash"`
	// Network allows network egress from inside the bash sandbox. Defaults true
	// so module/package downloads keep working; the boundary is then writes.
	Network bool `toml:"network"`
}

// WriteRoots returns the directories file-writer tools may modify: the
// workspace root (defaulting to the current working directory when unset), plus
// any AllowWrite extras, with ${VAR} expanded. The roots are returned as given
// (relative or absolute); the confiner resolves them to absolute, symlink-free
// paths. The result is always non-empty, so confinement is on by default.
func (c *Config) WriteRoots() []string {
	return c.WriteRootsForRoot(".")
}

// WriteRootsForRoot is like WriteRoots but falls back to fallbackRoot when the
// config doesn't explicitly set a workspace_root. Desktop tabs pass their
// project root here so tool confinement is correct without changing cwd.
func (c *Config) WriteRootsForRoot(fallbackRoot string) []string {
	root := c.expandVars(c.Sandbox.WorkspaceRoot)
	if root == "" {
		root = fallbackRoot
		if root == "" || root == "." {
			if wd, err := os.Getwd(); err == nil {
				root = wd
			} else {
				root = "."
			}
		}
	}
	roots := []string{root}
	for _, d := range c.Sandbox.AllowWrite {
		if d = c.expandVars(d); d != "" {
			roots = append(roots, d)
		}
	}
	return roots
}

// AllowWriteRoots returns only the configured [sandbox] allow_write extras with
// ${VAR} expanded — the explicit escape-hatch entries, without the workspace
// root that WriteRoots prepends. The session-data write guard treats these as
// user-sanctioned raw access.
func (c *Config) AllowWriteRoots() []string {
	var roots []string
	for _, d := range c.Sandbox.AllowWrite {
		if d = c.expandVars(d); d != "" {
			roots = append(roots, d)
		}
	}
	return roots
}

// ForbidReadRoots returns the paths the agent is forbidden from reading
// or listing, with ${VAR} expanded. Relative roots are resolved against the
// current working directory; the confiner resolves them to symlink-free paths.
// Empty when no forbid_read entries are configured.
func (c *Config) ForbidReadRoots() []string {
	return c.ForbidReadRootsForRoot(".")
}

// ForbidReadRootsForRoot is like ForbidReadRoots but uses fallbackRoot when
// resolving relative paths (for desktop tabs that pass their project root).
func (c *Config) ForbidReadRootsForRoot(fallbackRoot string) []string {
	root := fallbackRoot
	if root == "" || root == "." {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		} else {
			root = "."
		}
	}
	roots := make([]string, 0, len(c.Sandbox.ForbidRead))
	for _, d := range c.Sandbox.ForbidRead {
		if d = c.expandVars(d); d != "" {
			if !filepath.IsAbs(d) {
				d = filepath.Join(root, d)
			}
			roots = append(roots, d)
		}
	}
	return roots
}

// BashMode normalises the bash-sandbox mode for the current host.
func (c *Config) BashMode() string {
	return c.BashModeForGOOS(runtimeGOOS)
}

// BashModeForGOOS normalises the bash-sandbox mode for tests and cross-platform
// rendering. Windows has no OS-level Bash sandbox and forces the effective mode
// off, even when older configs explicitly requested "enforce". macOS/Linux keep
// the existing explicit-mode behavior.
func (c *Config) BashModeForGOOS(goos string) string {
	if goos == "windows" {
		return "off"
	}
	switch strings.TrimSpace(c.Sandbox.Bash) {
	case "enforce":
		return "enforce"
	case "off":
		return "off"
	case "":
		return "enforce"
	default:
		return "enforce"
	}
}

// TelemetryConfig controls content-free CLI usage metrics. It is user-global:
// project reasonix.toml values are ignored so a cloned repository cannot opt a
// user into reporting.
type TelemetryConfig struct {
	CLIMetrics string `toml:"cli_metrics"` // auto|on|off; empty means consent has not been requested
}

// SecretsConfig controls the credential protection layers. It is a user-global
// setting: project reasonix.toml values are ignored (see LoadForRoot), so a
// cloned repository cannot silently opt the user into workflow-breaking
// protections.
type SecretsConfig struct {
	// FilterSubprocessEnv strips credential-like environment variables
	// (*_API_KEY, *TOKEN*, *SECRET*, ...) from tool subprocesses (bash, hooks,
	// LSP, MCP stdio). Default off: it breaks token-based workflows such as
	// `gh`, HTTPS `git push`, and `npm publish`.
	FilterSubprocessEnv bool `toml:"filter_subprocess_env"`
	// ProtectSensitiveFiles makes read/list/search tools treat credential
	// paths (.env, .git-credentials, .netrc, *.pem/*.key/*.p12/*.pfx, ~/.ssh)
	// as invisible. Default off because hiding the files breaks legitimate
	// "edit my .env" workflows.
	ProtectSensitiveFiles bool `toml:"protect_sensitive_files"`
}

type providerSourceScope string

// UIConfig controls CLI presentation-only settings. Desktop appearance is kept in
// DesktopConfig so desktop preferences cannot alter terminal output or prompts.
type UIConfig struct {
	Theme          string `toml:"theme"`           // auto|dark|light; empty resolves to auto
	ThemeStyle     string `toml:"theme_style"`     // graphite|aurora|slate|carbon|nocturne|amber and legacy aliases
	ShortcutLayout string `toml:"shortcut_layout"` // classic|desktop; accepted for compatibility
	CloseBehavior  string `toml:"close_behavior"`  // legacy desktop close behavior; prefer desktop.close_behavior
	ShowReasoning  bool   `toml:"show_reasoning"`  // Ctrl+O / /verbose: show thinking text in CLI; false = collapsed
	ShowTurnUsage  bool   `toml:"show_turn_usage"` // show per-request token/cost receipts in the CLI/TUI transcript
	CursorShape    string `toml:"cursor_shape"`    // block|underline|bar; empty defaults to bar
}

// CLIConfig controls user-global native CLI behavior. It is separate from
// project runtime settings so a repository cannot change the installed
// binary's update channel.
type CLIConfig struct {
	// UpdateChannel is decoded for compatibility with pre-single-channel
	// configurations. Runtime behavior is always the official release channel,
	// and the canonical renderer intentionally drops this field.
	UpdateChannel string `toml:"update_channel"`
}

// DesktopConfig controls desktop-only UI preferences. It is intentionally
// separate from top-level language and [ui] so desktop choices do not affect CLI
// language, terminal colours, or provider-visible prompt/request data.
type DesktopConfig struct {
	Language                string   `toml:"language"`                   // auto|en|zh; empty/auto = browser/OS auto-detect
	Currency                string   `toml:"currency"`                   // legacy display currency; migrated to [billing].display_currency
	LayoutStyle             string   `toml:"layout_style"`               // classic|workbench|creation; desktop layout style
	Theme                   string   `toml:"theme"`                      // auto|dark|light; empty resolves to auto
	ThemeStyle              string   `toml:"theme_style"`                // graphite|aurora|slate|carbon|nocturne|amber and legacy aliases
	TerminalTheme           string   `toml:"terminal_theme"`             // auto|dark|light; auto follows the desktop app theme
	ExternalOpener          string   `toml:"external_opener"`            // preferred installed app used by the desktop Open control
	CloseBehavior           string   `toml:"close_behavior"`             // quit|background; desktop window close behavior
	DisplayMode             string   `toml:"display_mode"`               // standard|compact (legacy "minimal" maps to compact); transcript display mode
	StatusBarStyle          string   `toml:"status_bar_style"`           // icon|text; desktop status bar metric labels
	StatusBarItems          []string `toml:"status_bar_items"`           // ordered visible desktop status bar items
	DefaultToolApprovalMode string   `toml:"default_tool_approval_mode"` // ask|auto|yolo; defaults to auto for newly-created desktop sessions
	CheckUpdates            *bool    `toml:"check_updates"`              // startup update checks; nil keeps the default enabled
	// UpdateChannel is a legacy compatibility field. It is accepted on read but
	// ignored and omitted from future canonical writes.
	UpdateChannel        string   `toml:"update_channel"`
	Telemetry            *bool    `toml:"telemetry"`       // anonymous launch ping plus scrubbed next-launch native crash diagnostics; nil keeps the default enabled
	Metrics              *bool    `toml:"metrics"`         // aggregate desktop metrics (anonymous signal/bucket counts, including lifecycle health; no content); nil keeps the default enabled
	ProviderAccess       []string `toml:"provider_access"` // desktop-only list of provider entries shown in Settings > Model > Access
	ExpandThinking       bool     `toml:"expand_thinking"` // deprecated compatibility alias: true maps to auto
	ReasoningDisplayMode string   `toml:"reasoning_display_mode"`
	ConversationWidth    string   `toml:"conversation_width"` // standard|full; max transcript width; empty = standard
}

// NotificationsConfig controls optional system notifications for CLI chat/run.
type NotificationsConfig struct {
	Enabled         bool `toml:"enabled"`
	TurnDone        bool `toml:"turn_done"`
	ApprovalRequest bool `toml:"approval_request"`
	AskRequest      bool `toml:"ask_request"`
}

// EnvironmentEnabled reports whether startup environment probing should feed the
// cache-stable system prompt.
func (c *Config) EnvironmentEnabled() bool {
	return c == nil || c.Environment.Enabled == nil || *c.Environment.Enabled
}

const (
	providerSourceUser    providerSourceScope = "user"
	providerSourceProject providerSourceScope = "project"
)
