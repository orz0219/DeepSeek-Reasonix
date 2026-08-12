package main

import (
	"fmt"
	"strings"
	"time"

	"reasonix/internal/botruntime"
	"reasonix/internal/config"
	"reasonix/internal/control"
)

// SetPermissionMode sets the writer-fallback mode (ask|allow|deny).
func (a *App) SetPermissionMode(mode string) error {
	return a.applyConfigChange(func(c *config.Config) error { return c.SetPermissionMode(mode) })
}

// AddPermissionRule appends a rule to the allow/ask/deny list.
func (a *App) AddPermissionRule(list, rule string) error {
	return a.applyConfigChange(func(c *config.Config) error { return c.AddPermissionRule(list, rule) })
}

// RemovePermissionRule drops a rule from the allow/ask/deny list.
func (a *App) RemovePermissionRule(list, rule string) error {
	return a.applyConfigChange(func(c *config.Config) error {
		_, err := c.RemovePermissionRule(list, rule)
		return err
	})
}

// ReloadSettings rebuilds the active controller from the current config without
// changing any config file. It lets manual config.toml edits take effect.
func (a *App) ReloadSettings() error {
	if err := a.ensureActiveTabRebuildAllowed("settings"); err != nil {
		return err
	}
	if err := a.rebuild(); err != nil {

		if _, ok := a.deferredRebuildWarning("settings", err); ok {
			return nil
		}
		return err
	}
	return nil
}

// SetSandbox updates the bash sandbox mode, network egress, and write roots.
func (a *App) SetSandbox(bash string, network bool, workspaceRoot string, allowWrite []string, shell string) error {
	return a.applyConfigChange(func(c *config.Config) error {
		c.Sandbox.Bash = bash
		c.Sandbox.Network = network
		c.Sandbox.WorkspaceRoot = strings.TrimSpace(workspaceRoot)
		c.Sandbox.AllowWrite = trimList(allowWrite)
		c.Tools.Shell.Prefer = strings.TrimSpace(shell)
		return nil
	})
}

// SetNetwork updates ordinary outbound proxy settings.
func (a *App) SetNetwork(n NetworkView) error {
	return a.applyConfigChange(func(c *config.Config) error {
		return c.SetNetwork(config.NetworkConfig{
			ProxyMode: n.ProxyMode,
			ProxyURL:  n.ProxyURL,
			NoProxy:   n.NoProxy,
			Proxy: config.NetworkProxyConfig{
				Type:     n.Proxy.Type,
				Server:   n.Proxy.Server,
				Port:     n.Proxy.Port,
				Username: n.Proxy.Username,
				Password: n.Proxy.Password,
			},
		})
	})
}

func (a *App) SetBotSettings(b BotSettingsView) error {
	err := a.applyConfigOnly(func(c *config.Config) error {
		c.Bot.Enabled = b.Enabled
		c.Bot.Model = strings.TrimSpace(b.Model)
		c.Bot.ToolApprovalMode = normalizeBotConnectionToolApprovalMode(b.ToolApprovalMode)
		c.Bot.MaxSteps = b.MaxSteps
		c.Bot.DebounceMs = b.DebounceMs
		c.Bot.QueueMode = strings.TrimSpace(b.QueueMode)
		c.Bot.QueueCap = b.QueueCap
		c.Bot.QueueDrop = strings.TrimSpace(b.QueueDrop)
		c.Bot.IgnoreSelfMessages = b.IgnoreSelfMessages
		c.Bot.SelfUserIDs = config.BotSelfUserIDs{
			QQ:     trimList(b.SelfUserIDs.QQ),
			Feishu: trimList(b.SelfUserIDs.Feishu),
			Weixin: trimList(b.SelfUserIDs.Weixin),
		}
		c.Bot.Control = config.BotControlConfig{
			Enabled:  b.Control.Enabled,
			Addr:     strings.TrimSpace(b.Control.Addr),
			TokenEnv: strings.TrimSpace(b.Control.TokenEnv),
		}
		c.Bot.Pairing = config.BotPairingConfig{
			Enabled:               b.Pairing.Enabled,
			RequestTTLMinutes:     b.Pairing.RequestTTLMinutes,
			MaxPendingPerPlatform: b.Pairing.MaxPendingPerPlatform,
		}
		c.Bot.Routes = botRouteConfigs(b.Routes)
		c.Bot.Allowlist = config.BotAllowlist{
			Enabled:         b.Allowlist.Enabled,
			AllowAll:        b.Allowlist.AllowAll,
			QQUsers:         trimList(b.Allowlist.QQUsers),
			FeishuUsers:     trimList(b.Allowlist.FeishuUsers),
			WeixinUsers:     trimList(b.Allowlist.WeixinUsers),
			QQApprovers:     trimList(b.Allowlist.QQApprovers),
			FeishuApprovers: trimList(b.Allowlist.FeishuApprovers),
			WeixinApprovers: trimList(b.Allowlist.WeixinApprovers),
			QQAdmins:        trimList(b.Allowlist.QQAdmins),
			FeishuAdmins:    trimList(b.Allowlist.FeishuAdmins),
			WeixinAdmins:    trimList(b.Allowlist.WeixinAdmins),
			QQGroups:        trimList(b.Allowlist.QQGroups),
			FeishuGroups:    trimList(b.Allowlist.FeishuGroups),
			WeixinGroups:    trimList(b.Allowlist.WeixinGroups),
		}
		c.Bot.QQ = config.QQBotConfig{
			Enabled:          b.QQ.Enabled,
			AppID:            strings.TrimSpace(b.QQ.AppID),
			AppSecretEnv:     strings.TrimSpace(b.QQ.AppSecretEnv),
			Sandbox:          b.QQ.Sandbox,
			Model:            strings.TrimSpace(b.QQ.Model),
			ToolApprovalMode: normalizeBotConnectionToolApprovalMode(b.QQ.ToolApprovalMode),
			WorkspaceRoot:    strings.TrimSpace(b.QQ.WorkspaceRoot),
			Access:           botAccessConfigFromView(b.QQ.Access),
		}
		c.Bot.Feishu = config.FeishuBotConfig{
			Enabled:            b.Feishu.Enabled,
			Domain:             botDomainOrDefault(b.Feishu.Domain),
			AppID:              strings.TrimSpace(b.Feishu.AppID),
			AppSecretEnv:       strings.TrimSpace(b.Feishu.AppSecretEnv),
			VerificationToken:  strings.TrimSpace(b.Feishu.VerificationToken),
			Mode:               strings.TrimSpace(b.Feishu.Mode),
			WebhookPort:        b.Feishu.WebhookPort,
			RequireMention:     b.Feishu.RequireMention,
			OutboundMediaRoots: append([]string(nil), c.Bot.Feishu.OutboundMediaRoots...),
		}
		c.Bot.Weixin = config.WeixinBotConfig{
			Enabled:   b.Weixin.Enabled,
			AccountID: strings.TrimSpace(b.Weixin.AccountID),
			TokenEnv:  strings.TrimSpace(b.Weixin.TokenEnv),
			APIBase:   strings.TrimRight(strings.TrimSpace(b.Weixin.APIBase), "/"),
		}
		c.Bot.Connections = botConnectionConfigs(b.Connections)
		return nil
	})
	if err == nil {
		a.refreshBotRuntimeAsync()
	}
	return err
}

// SetBotConnectionToolApprovalMode updates a single connection's tool approval
// mode without restarting the bot gateway. Only the connection's mode field is
// persisted; existing sessions on the running gateway are updated in-place.
func (a *App) SetBotConnectionToolApprovalMode(connID, mode string) error {
	connID = strings.TrimSpace(connID)
	mode = normalizeBotConnectionToolApprovalMode(mode)
	runtimeConnID := connID
	err := a.applyConfigOnly(func(c *config.Config) error {
		for i := range c.Bot.Connections {
			candidateRuntimeID := botruntime.ConnectionRuntimeID(c.Bot.Connections[i])
			if candidateRuntimeID == "" {
				candidateRuntimeID = strings.TrimSpace(c.Bot.Connections[i].ID)
			}
			if c.Bot.Connections[i].ID == connID || candidateRuntimeID == connID {
				c.Bot.Connections[i].ToolApprovalMode = mode
				c.Bot.Connections[i].UpdatedAt = time.Now().UTC().Format(time.RFC3339)
				runtimeConnID = candidateRuntimeID
				return nil
			}
		}
		return fmt.Errorf("connection %q not found", connID)
	})
	if err != nil {
		return err
	}
	if a.botRuntime != nil {
		a.botRuntime.updateConnectionToolApprovalMode(runtimeConnID, mode)
	}
	return nil
}

func (a *App) SetBotSecret(envName, value string) error {
	envName = strings.TrimSpace(envName)
	if envName == "" {
		return fmt.Errorf("bot secret env name is empty")
	}
	if err := upsertDotEnv(envName, value); err != nil {
		return err
	}
	a.refreshBotRuntimeAsync()
	return nil
}

func (a *App) ClearBotSecret(envName string) error {
	envName = strings.TrimSpace(envName)
	if envName == "" {
		return fmt.Errorf("bot secret env name is empty")
	}
	if err := removeDotEnv(envName); err != nil {
		return err
	}
	a.refreshBotRuntimeAsync()
	return nil
}

// SetCloseBehavior updates desktop-only window close behavior without rebuilding
// the active controller. It must stay out of provider-visible prompt/request data.
func (a *App) SetCloseBehavior(mode string) error {
	return a.applyConfigOnly(func(c *config.Config) error { return c.SetDesktopCloseBehavior(mode) })
}

// SetDisplayMode updates the transcript display mode. UI-only, no rebuild needed.
func (a *App) SetDisplayMode(mode string) error {
	return a.applyConfigOnly(func(c *config.Config) error { return c.SetDesktopDisplayMode(mode) })
}

// SetStatusBarStyle updates the desktop status bar metric label style. UI-only,
// no rebuild needed.
func (a *App) SetStatusBarStyle(style string) error {
	return a.applyConfigOnly(func(c *config.Config) error { return c.SetDesktopStatusBarStyle(style) })
}

// SetStatusBarItems updates the ordered visible desktop status bar items.
// UI-only, no rebuild needed.
func (a *App) SetStatusBarItems(items []string) error {
	return a.applyConfigOnly(func(c *config.Config) error { return c.SetDesktopStatusBarItems(items) })
}

// SetDesktopLanguage updates the desktop UI language and the user-level response
// language preference used by model-facing desktop sessions.
func (a *App) SetDesktopLanguage(lang string) error {
	responseLanguage := ""
	mutate := func(c *config.Config) error {
		if err := c.SetDesktopLanguage(lang); err != nil {
			return err
		}
		if err := c.SetLanguage(lang); err != nil {
			return err
		}
		responseLanguage = c.ResponseLanguage()
		return nil
	}
	err := a.applyConfigOnly(mutate)
	if err != nil {
		return err
	}
	if strings.TrimSpace(lang) != "" && !strings.EqualFold(strings.TrimSpace(lang), "auto") {
		a.setDesktopLocale(lang)
	}
	a.updateTrayLocale(lang)
	a.applyResponseLanguageToLiveControllers(responseLanguage)
	return nil
}

// SetDesktopCurrency persists a display-only preference and re-selects the
// occurrence-time valuations already stored in each tab. Provider price tables
// and live controllers are intentionally untouched.
func (a *App) SetDesktopCurrency(currency string) error {
	err := a.applyConfigOnly(func(c *config.Config) error {
		return c.SetDesktopCurrency(currency)
	})
	if err != nil {
		return err
	}

	a.sessionRemovalMu.Lock()
	defer a.sessionRemovalMu.Unlock()
	a.mu.RLock()
	tabs := append([]*WorkspaceTab(nil), a.runtimeTabsLocked()...)
	a.mu.RUnlock()
	for _, tab := range tabs {
		a.repriceTabUsageForCurrentCurrency(tab)
	}
	return nil
}

func (a *App) desktopEffectivePricingCurrency(cfg *config.Config) string {

	if cfg == nil {
		return ""
	}
	if pref := cfg.DisplayCurrencyPref(); pref != "" {
		return pref
	}
	return cfg.ExplicitDisplayCurrency()
}

func (a *App) desktopOfficialPricingLanguage(cfg *config.Config) string {

	if a.desktopEffectivePricingCurrency(cfg) == "CNY" {
		return "zh"
	}
	return "en"
}

// SetTrayLocale mirrors the resolved desktop UI language into the native tray
// menu. It is runtime-only; the persisted preference remains [desktop].language.
func (a *App) SetTrayLocale(locale string) error {
	a.setDesktopLocale(locale)
	trayLocale := "en"
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(locale)), "zh") {
		trayLocale = "zh"
	}
	a.updateTrayLocale(trayLocale)
	a.emitProjectTreeChanged()
	return nil
}

// SetDesktopAppearance updates only desktop theme preferences. It does not
// rebuild the active controller and must stay out of provider-visible requests.
func (a *App) SetDesktopAppearance(theme, style string) error {
	return a.applyConfigOnly(func(c *config.Config) error { return c.SetDesktopAppearance(theme, style) })
}

// SetDesktopTerminalTheme updates only the integrated terminal colours. It is
// applied live by the frontend and does not rebuild the active controller.
func (a *App) SetDesktopTerminalTheme(theme string) error {
	return a.applyConfigOnly(func(c *config.Config) error { return c.SetDesktopTerminalTheme(theme) })
}

// SetDesktopLayoutStyle updates only the desktop layout style. It does not
// rebuild the active controller and must stay out of provider-visible requests.
func (a *App) SetDesktopLayoutStyle(style string) error {
	normalized := ""
	if err := a.applyConfigOnly(func(c *config.Config) error {
		if err := c.SetDesktopLayoutStyle(style); err != nil {
			return err
		}
		normalized = c.DesktopLayoutStyle()
		return nil
	}); err != nil {
		return err
	}
	if singleSurfaceLayoutStyle(normalized) {
		return a.applySingleSurfaceTabPolicy()
	}
	return nil
}

// SetDesktopCheckUpdates updates only the desktop startup update-check
// preference. Manual checks in Settings are unaffected.
func (a *App) SetDesktopCheckUpdates(enabled bool) error {
	return a.applyConfigOnly(func(c *config.Config) error { return c.SetDesktopCheckUpdates(enabled) })
}

// SetDesktopUpdateChannel is retained for older Wails clients. The config layer
// clears the retired preference and every updater request uses Stable.
func (a *App) SetDesktopUpdateChannel(channel string) error {
	return a.applyConfigOnly(func(c *config.Config) error { return c.SetDesktopUpdateChannel(channel) })
}

// SetDesktopTelemetry sets whether the desktop sends the anonymous launch ping.
func (a *App) SetDesktopTelemetry(enabled bool) error {
	return a.applyConfigOnly(func(c *config.Config) error { return c.SetDesktopTelemetry(enabled) })
}

// SetDesktopMetrics sets whether the desktop sends aggregate desktop metrics,
// starting or stopping the live aggregator so the toggle takes effect immediately.
func (a *App) SetDesktopMetrics(enabled bool) error {
	if err := a.applyConfigOnly(func(c *config.Config) error { return c.SetDesktopMetrics(enabled) }); err != nil {
		return err
	}
	switch {
	case enabled && a.metrics.Load() == nil && version != "dev":
		a.metrics.Store(newMetricsAggregator(config.MemoryUserDir()))
		if cfg, err := config.Load(); err == nil {
			a.recordSettingsMetricsSnapshot(cfg)
		}
	case !enabled:
		a.metrics.Store(nil)
	}
	return nil
}

// SetExpandThinking sets whether reasoning text is expanded by default on
// the desktop. It is desktop-only and does not rebuild the controller.
func (a *App) SetExpandThinking(on bool) error {
	return a.applyConfigOnly(func(c *config.Config) error { return c.SetExpandThinking(on) })
}

// SetDesktopConversationWidth sets the max transcript width preference.
// standard = 960px fixed; full = 90% of the parent, with a 960px floor. Pure config-only.
func (a *App) SetDesktopConversationWidth(width string) error {
	return a.applyConfigOnly(func(c *config.Config) error { return c.SetDesktopConversationWidth(width) })
}

// MigrateDesktopPreferences imports old browser-local desktop preferences into
// the user config once. Existing [desktop] values win so stale localStorage never
// overwrites an explicit config edit.
func (a *App) MigrateDesktopPreferences(language, theme, style string) error {
	return a.applyConfigOnly(func(c *config.Config) error {
		if strings.TrimSpace(c.Desktop.Language) == "" {
			if err := c.SetDesktopLanguage(language); err != nil {
				return err
			}
		}
		if strings.TrimSpace(c.Desktop.Theme) == "" && strings.TrimSpace(c.Desktop.ThemeStyle) == "" {
			if err := c.SetDesktopAppearance(theme, style); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetAgentParams updates sampling temperature and the base system prompt. The
// step arguments remain in the Wails contract for older frontends, but are
// retired and deliberately normalized to automatic execution.
func (a *App) SetAgentParams(temperature float64, maxSteps int, plannerMaxSteps int, systemPrompt string) error {
	return a.applyConfigChange(func(c *config.Config) error {
		c.Agent.Temperature = temperature
		c.Agent.MaxSteps = 0
		c.Agent.PlannerMaxSteps = 0
		c.Agent.SystemPrompt = systemPrompt
		return nil
	})
}

func (a *App) SetCompactRatio(ratio float64) error {
	_, err := a.applyConfigChangeWithWarning("context compaction threshold", func(c *config.Config) error {
		return c.SetCompactRatio(ratio)
	})
	return err
}

func (a *App) SetReasoningLanguage(lang string) error {
	if err := a.ensureLiveControllersRuntimeMutationAllowed("reasoning language"); err != nil {
		return err
	}
	var cfg *config.Config

	if err := func() error {
		unlock := config.LockUserConfigEdits()
		defer unlock()
		loaded, path, err := a.loadDesktopUserConfigForEdit()
		if err != nil {
			return err
		}
		if err := loaded.SetReasoningLanguage(lang); err != nil {
			return err
		}
		if err := loaded.SaveTo(path); err != nil {
			return err
		}
		cfg = loaded
		return nil
	}(); err != nil {
		return err
	}
	a.applyReasoningLanguageToLiveControllers(cfg.ReasoningLanguage())
	return nil
}

func (a *App) applyReasoningLanguageToLiveControllers(fallback string) {
	type liveTab struct {
		root string
		ctrl control.SessionAPI
	}
	var tabs []liveTab
	a.mu.RLock()
	for _, tab := range a.tabs {
		if tab != nil && tab.Ctrl != nil {
			tabs = append(tabs, liveTab{root: tab.WorkspaceRoot, ctrl: tab.Ctrl})
		}
	}
	a.mu.RUnlock()
	for _, tab := range tabs {
		mode := fallback
		if cfg, err := config.LoadForRoot(tab.root); err == nil {
			mode = cfg.ReasoningLanguage()
		}
		tab.ctrl.SetReasoningLanguage(mode)
	}
}

func (a *App) applyResponseLanguageToLiveControllers(fallback string) {
	type liveTab struct {
		root string
		ctrl control.SessionAPI
	}
	var tabs []liveTab
	a.mu.RLock()
	for _, tab := range a.tabs {
		if tab != nil && tab.Ctrl != nil {
			tabs = append(tabs, liveTab{root: tab.WorkspaceRoot, ctrl: tab.Ctrl})
		}
	}
	a.mu.RUnlock()
	for _, tab := range tabs {
		mode := fallback
		if cfg, err := config.LoadForRoot(tab.root); err == nil {
			mode = cfg.ResponseLanguage()
		}
		tab.ctrl.SetResponseLanguage(mode)
	}
}

// trimList drops blank entries from a string slice (and returns a non-nil slice).
func trimList(in []string) []string {
	out := []string{}
	for _, s := range in {
		if t := strings.TrimSpace(s); t != "" {
			out = append(out, t)
		}
	}
	return out
}
