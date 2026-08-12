package config

import (
	"strings"
)

// DesktopExternalOpener returns the selected opener id; unavailable ids fall
// back to the platform file manager in the desktop shell.
func (c *Config) DesktopExternalOpener() string {
	if c == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.Desktop.ExternalOpener))
}

// UITheme normalizes ui.theme to a supported value.
func (c *Config) UITheme() string {
	switch strings.ToLower(strings.TrimSpace(c.UI.Theme)) {
	case "dark":
		return "dark"
	case "light":
		return "light"
	default:
		return "auto"
	}
}

// UIThemeStyle normalizes ui.theme_style. Empty means "pick the default style
// for the resolved light/dark shell".
func (c *Config) UIThemeStyle() string {
	return normalizeThemeStyle(c.UI.ThemeStyle)
}

// UIShortcutLayout normalizes the legacy CLI shortcut layout setting. It is kept
// for compatibility; Shift+Tab toggles Plan and Ctrl+Y toggles YOLO in both
// layouts.
func (c *Config) UIShortcutLayout() string {
	switch strings.ToLower(strings.TrimSpace(c.UI.ShortcutLayout)) {
	case "desktop", "dual", "dual-axis", "dual_axis":
		return "desktop"
	default:
		return "classic"
	}
}

// UICursorShape normalizes ui.cursor_shape. The slim "bar" default stays
// visible without covering CJK wide characters. Valid values are "block",
// "underline", and "bar".
func (c *Config) UICursorShape() string {
	switch strings.ToLower(strings.TrimSpace(c.UI.CursorShape)) {
	case "block":
		return "block"
	case "underline":
		return "underline"
	default:
		return "bar"
	}
}

func normalizeThemeStyle(style string) string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case "graphite", "aurora", "slate", "carbon", "nocturne", "amber", "ember", "midnight", "sandstone", "porcelain", "linen", "glacier":
		return strings.ToLower(strings.TrimSpace(style))
	default:
		return ""
	}
}

func normalizeDesktopLayoutStyle(style string) string {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case "classic":
		return "classic"
	case "workbench", "workspace":
		return "workbench"
	case "creation":
		return "creation"
	default:
		return "workbench"
	}
}

func normalizeCloseBehavior(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "quit", "exit":
		return "quit"
	default:
		return "background"
	}
}

// DesktopLanguage normalizes the desktop UI language. Empty means auto-detect
// from the browser/OS locale; it deliberately does not read top-level language,
// which is used by the CLI/model-facing runtime.
func (c *Config) DesktopLanguage() string {
	switch strings.ToLower(strings.TrimSpace(c.Desktop.Language)) {
	case "en":
		return "en"
	case "zh":
		return "zh"
	default:
		return ""
	}
}

// DesktopCurrency returns the explicit user-global pricing currency. The
// persisted field keeps its original desktop namespace for compatibility;
// empty means the pricing region follows the desktop/CLI language.
func (c *Config) DesktopCurrency() string {
	if c == nil {
		return ""
	}
	switch strings.ToUpper(strings.TrimSpace(c.Desktop.Currency)) {
	case "CNY", "RMB", "CNH":
		return "CNY"
	case "USD":
		return "USD"
	default:
		return ""
	}
}

// DesktopTheme normalizes desktop.theme. New desktop users default to the OS
// automatic graphite product look; an explicit auto/light/dark is preserved.
func (c *Config) DesktopTheme() string {
	switch strings.ToLower(strings.TrimSpace(c.Desktop.Theme)) {
	case "auto":
		return "auto"
	case "light":
		return "light"
	case "dark":
		return "dark"
	default:
		return "auto"
	}
}

// DesktopThemeStyle normalizes desktop.theme_style. Empty means the frontend
// chooses the default style for the resolved desktop theme.
func (c *Config) DesktopThemeStyle() string {
	return normalizeThemeStyle(c.Desktop.ThemeStyle)
}

// DesktopTerminalTheme normalizes the integrated terminal colour preference.
// Auto deliberately follows the resolved desktop app theme, including OS theme
// changes while desktop.theme is also auto.
func (c *Config) DesktopTerminalTheme() string {
	switch strings.ToLower(strings.TrimSpace(c.Desktop.TerminalTheme)) {
	case "dark":
		return "dark"
	case "light":
		return "light"
	default:
		return "auto"
	}
}

// DesktopLayoutStyle normalizes the desktop layout style. New installs default
// to workbench; explicit classic remains respected.
func (c *Config) DesktopLayoutStyle() string {
	if strings.EqualFold(strings.TrimSpace(c.Desktop.ThemeStyle), "workbench") && strings.TrimSpace(c.Desktop.LayoutStyle) == "" {
		return "workbench"
	}
	return normalizeDesktopLayoutStyle(c.Desktop.LayoutStyle)
}

// DesktopCloseBehavior normalizes the desktop close-window preference. It falls
// back to the legacy ui.close_behavior value for configs written before [desktop]
// existed.
func (c *Config) DesktopCloseBehavior() string {
	if strings.TrimSpace(c.Desktop.CloseBehavior) != "" {
		return normalizeCloseBehavior(c.Desktop.CloseBehavior)
	}
	return normalizeCloseBehavior(c.UI.CloseBehavior)
}

// UICloseBehavior is the legacy name for DesktopCloseBehavior.
func (c *Config) UICloseBehavior() string {
	return c.DesktopCloseBehavior()
}

// DesktopDisplayMode normalizes the transcript display mode. Default is
// "standard" (flat rendering, no folding).
func (c *Config) DesktopDisplayMode() string {
	switch strings.ToLower(strings.TrimSpace(c.Desktop.DisplayMode)) {
	case "standard":
		return "standard"
	case "compact", "minimal":
		return "compact"
	default:
		return "standard"
	}
}

// DesktopConversationWidth returns the normalized desktop conversation width.
// Unknown and missing values fall back to standard for backward compatibility.
func (c *Config) DesktopConversationWidth() string {
	if c != nil && strings.EqualFold(strings.TrimSpace(c.Desktop.ConversationWidth), "full") {
		return "full"
	}
	return "standard"
}

// NormalizeToolApprovalMode returns the canonical desktop/session tool approval
// posture. Unknown or missing values fall back to ask for safety.
func NormalizeToolApprovalMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "auto":
		return "auto"
	case "yolo", "full", "full-access", "bypass":
		return "yolo"
	default:
		return "ask"
	}
}

// DesktopDefaultToolApprovalMode is the Ask/Auto/YOLO default used only when
// creating a new desktop session. Existing tabs and restored sessions keep their
// own persisted runtime state.
func (c *Config) DesktopDefaultToolApprovalMode() string {
	if c == nil {
		return "ask"
	}
	return NormalizeToolApprovalMode(c.Desktop.DefaultToolApprovalMode)
}

// DesktopStatusBarStyle normalizes the desktop status bar metric label style.
// Default is "text"; explicit "icon" preserves the user's compact choice.
func (c *Config) DesktopStatusBarStyle() string {
	switch strings.ToLower(strings.TrimSpace(c.Desktop.StatusBarStyle)) {
	case "icon":
		return "icon"
	case "text":
		return "text"
	default:
		return "text"
	}
}

var defaultDesktopStatusBarItems = []string{
	"model",
	"workspace",
	"git_branch",
	"cache",
	"cache_avg",
	"session_tokens",
	"turn_tokens",
	"turn_tps",
	"turn_output_tokens",
	"turn_cache_tokens",
	"turn_cost",
	"session_turns",
	"context",
	"compact",
	"cost",
	"balance",
}

var knownDesktopStatusBarItems = desktopStatusBarItemSet(defaultDesktopStatusBarItems)

func desktopStatusBarItemSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

// DefaultDesktopStatusBarItems returns the default ordered visible desktop
// status bar items.
func DefaultDesktopStatusBarItems() []string {
	return append([]string(nil), defaultDesktopStatusBarItems...)
}

// DesktopStatusBarItems normalizes the ordered visible desktop status bar items.
// An unset or empty list uses the default full set; explicit non-empty lists
// preserve user order and omit hidden items.
func (c *Config) DesktopStatusBarItems() []string {
	return normalizeDesktopStatusBarItems(c.Desktop.StatusBarItems)
}

func normalizeDesktopStatusBarItems(items []string) []string {
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, raw := range items {
		id := strings.TrimSpace(raw)
		if !knownDesktopStatusBarItems[id] || seen[id] {
			continue
		}
		out = append(out, id)
		seen[id] = true
	}
	if len(out) == 0 {
		return DefaultDesktopStatusBarItems()
	}
	return out
}

// DesktopCheckUpdates reports whether the desktop should check for updates on
// startup. Missing configs default to true so existing users keep update notices.
func (c *Config) DesktopCheckUpdates() bool {
	if c == nil || c.Desktop.CheckUpdates == nil {
		return true
	}
	return *c.Desktop.CheckUpdates
}

// NormalizeCLIUpdateChannel returns the only public native CLI update channel.
// The input remains accepted so older preview configurations keep loading.
func NormalizeCLIUpdateChannel(_ string) string {
	return "stable"
}

// CLIUpdateChannel returns the user-global native CLI update channel.
func (c *Config) CLIUpdateChannel() string {
	if c == nil {
		return "stable"
	}
	return NormalizeCLIUpdateChannel(c.CLI.UpdateChannel)
}

// NormalizeDesktopUpdateChannel returns the only public Desktop update channel.
// Legacy preview/canary/beta/next values are deliberately ignored so an old
// configuration cannot strand the installation on the retired channel.
func NormalizeDesktopUpdateChannel(_ string) string {
	return "stable"
}

// DesktopUpdateChannel returns the desktop channel whose latest pointer should be
// checked. Missing or unknown configs default to stable.
func (c *Config) DesktopUpdateChannel() string {
	if c == nil {
		return "stable"
	}
	return NormalizeDesktopUpdateChannel(c.Desktop.UpdateChannel)
}

// ColdResumePruneEnabled reports whether stale tool results are elided when a
// session resumes past the provider cache window. Default true (cheaper cold
// restart); users keep full history by disabling it.
func (c *Config) ColdResumePruneEnabled() bool {
	if c == nil || c.Agent.ColdResumePrune == nil {
		return true
	}
	return *c.Agent.ColdResumePrune
}

// DesktopTelemetry reports whether the desktop sends the anonymous launch ping.
// It carries no conversation, key, or file data — see desktop/README.md.
func (c *Config) DesktopTelemetry() bool {
	if c == nil || c.Desktop.Telemetry == nil {
		return true
	}
	return *c.Desktop.Telemetry
}

// DesktopMetrics reports whether the desktop sends aggregate desktop metrics —
// anonymous (signal, bucket) counters, never content. Default on.
func (c *Config) DesktopMetrics() bool {
	if c == nil || c.Desktop.Metrics == nil {
		return true
	}
	return *c.Desktop.Metrics
}
