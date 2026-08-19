package main

import (
	"strings"

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
