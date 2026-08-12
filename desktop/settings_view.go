package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/provider"
	"reasonix/internal/sandbox"
)

// DesktopStartupSettings returns startup chrome preferences without provider/key state.
func (a *App) DesktopStartupSettings() (view DesktopStartupSettingsView) {
	revision := a.nextConfigLoadWarningsRevision()
	defer func() { view.ConfigWarningsRevision = revision }()

	if cfg, err := config.LoadForRootReadOnly(a.activeWorkspaceRoot()); err == nil {
		view = desktopStartupSettingsFromConfig(cfg)
		view.ConfigWarnings = cfg.LoadWarnings()
		view.ConfigPath = config.UserConfigPath()
		return view
	}
	cfg, path, err := a.loadDesktopUserConfigForView()
	if err != nil {
		view = desktopStartupSettingsFromConfig(nil)
		view.ConfigWarnings = []string{
			"user configuration could not be loaded; using built-in defaults. Run: reasonix doctor repair",
		}
		view.ConfigPath = config.UserConfigPath()
		return view
	}
	view = desktopStartupSettingsFromConfig(cfg)
	view.ConfigPath = path
	return view
}

// OpenUserConfigPath reveals the user config file in the system file manager.
func (a *App) OpenUserConfigPath() error {
	path := config.UserConfigPath()
	if path == "" {
		return fmt.Errorf("user config path is unavailable")
	}

	if _, err := os.Stat(path); err != nil {
		return a.RevealPath(filepath.Dir(path))
	}
	return a.RevealPath(path)
}

// ReloadUserConfig reloads configuration for the active workspace after the
// user fixes a broken file. Non-fatal load warnings remain visible when present.
func (a *App) ReloadUserConfig() (DesktopStartupSettingsView, error) {
	return a.DesktopStartupSettings(), nil
}

// Settings returns the current configuration for the Settings panel.
func (a *App) Settings() SettingsView {
	cfg, cfgPath, err := a.loadDesktopUserConfigForView()
	if err != nil {
		return a.defaultSettingsView()
	}
	ctrl := a.activeCtrl()
	bash := cfg.BashMode()
	shell := cfg.Tools.Shell.Prefer
	if shell == "" {
		shell = "auto"
	}
	root := a.activeWorkspaceRoot()
	writeRoots := cfg.WriteRootsForRoot(root)
	effectiveWorkspaceRoot := ""
	if len(writeRoots) > 0 {
		effectiveWorkspaceRoot = writeRoots[0]
	}
	effectiveShell := sandbox.ResolveShell(cfg.Tools.Shell.Prefer, cfg.Tools.Shell.Path, nil)
	v := SettingsView{
		DefaultModel:      cfg.DefaultModel,
		PlannerModel:      cfg.Agent.PlannerModel,
		SubagentModel:     cfg.Agent.SubagentModel,
		SubagentEffort:    cfg.Agent.SubagentEffort,
		AutoPlan:          "off",
		Providers:         []ProviderView{},
		OfficialProviders: []ProviderView{},
		ProviderPresets:   []ProviderPresetView{},
		Permissions: PermissionsView{
			Mode:  orDefault(cfg.Permissions.Mode, "ask"),
			Allow: nonNil(cfg.Permissions.Allow),
			Ask:   nonNil(cfg.Permissions.Ask),
			Deny:  nonNil(cfg.Permissions.Deny),
		},
		Sandbox: SandboxView{
			Bash: bash, Network: cfg.Sandbox.Network,
			WorkspaceRoot: cfg.Sandbox.WorkspaceRoot, AllowWrite: nonNil(cfg.Sandbox.AllowWrite),
			EffectiveWorkspaceRoot: effectiveWorkspaceRoot, EffectiveWriteRoots: nonNil(writeRoots),
			Shell: shell, EffectiveShell: sandboxEffectiveShellView(effectiveShell),
		},
		Network: NetworkView{
			ProxyMode: cfg.NetworkProxyMode(),
			ProxyURL:  cfg.Network.ProxyURL,
			NoProxy:   cfg.Network.NoProxy,
			Proxy: NetworkProxyView{
				Type:     orDefault(cfg.Network.Proxy.Type, "socks5"),
				Server:   cfg.Network.Proxy.Server,
				Port:     cfg.Network.Proxy.Port,
				Username: cfg.Network.Proxy.Username,
				Password: cfg.Network.Proxy.Password,
			},
		},
		Agent: AgentView{
			Temperature:            cfg.Agent.Temperature,
			MaxSteps:               cfg.Agent.MaxSteps,
			PlannerMaxSteps:        cfg.Agent.PlannerMaxSteps,
			MaxSubagentDepth:       desktopMaxSubagentDepth(cfg.Agent.MaxSubagentDepth),
			MaxSubagentConcurrency: desktopSubagentConcurrency(cfg.Agent.MaxSubagentConcurrency),
			MaxParallelWriters:     desktopParallelWriters(cfg.Agent.MaxParallelWriters, cfg.Agent.MaxSubagentConcurrency),
			SystemPrompt:           cfg.Agent.SystemPrompt,
			ReasoningLanguage:      cfg.ReasoningLanguage(),
			CompactRatio:           cfg.Agent.CompactRatio,
			EffectiveCompactRatio:  cfg.Agent.CompactRatio,
		},
		Bot:                          botSettingsView(cfg.Bot),
		DesktopLanguage:              cfg.DesktopLanguage(),
		DesktopCurrency:              cfg.DesktopCurrency(),
		DesktopLayoutStyle:           cfg.DesktopLayoutStyle(),
		DesktopTheme:                 cfg.DesktopTheme(),
		DesktopThemeStyle:            cfg.DesktopThemeStyle(),
		DesktopTerminalTheme:         cfg.DesktopTerminalTheme(),
		CloseBehavior:                cfg.DesktopCloseBehavior(),
		DisplayMode:                  cfg.DesktopDisplayMode(),
		ReasoningDisplayMode:         cfg.DesktopReasoningDisplayMode(),
		ReasoningDisplayModeExplicit: cfg.DesktopReasoningDisplayModeExplicit(),
		StatusBarStyle:               cfg.DesktopStatusBarStyle(),
		StatusBarItems:               cfg.DesktopStatusBarItems(),
		DefaultToolApprovalMode:      cfg.DesktopDefaultToolApprovalMode(),
		CheckUpdates:                 cfg.DesktopCheckUpdates(),
		UpdateChannel:                cfg.DesktopUpdateChannel(),
		Telemetry:                    cfg.DesktopTelemetry(),
		Metrics:                      cfg.DesktopMetrics(),
		ExpandThinking:               cfg.Desktop.ExpandThinking,
		ConversationWidth:            cfg.DesktopConversationWidth(),
		ConfigPath:                   cfgPath,
		ShadowedByPath:               shadowingConfigPath(cfgPath, root),
		ProviderKinds:                nonNil(provider.Kinds()),
		AutoApproveTools:             ctrl != nil && ctrl.AutoApproveTools(),
		Bypass:                       ctrl != nil && ctrl.AutoApproveTools(),
	}
	if ctrl != nil {
		if effective := ctrl.CompactRatio(); effective > 0 {
			v.Agent.EffectiveCompactRatio = effective
			v.Agent.CompactRatioOverridden = math.Abs(effective-v.Agent.CompactRatio) > 0.0001
		}
	}
	added := providerAccessSet(cfg.Desktop.ProviderAccess)
	resolver := config.NewCredentialResolverForRoot(root)
	credentialsRevision := providerCredentialsRevision()
	v.OfficialProviders = officialProviderViewsForRootWithResolver(officialProviderAddedSet(cfg), a.desktopOfficialPricingLanguage(cfg), root, resolver)
	v.ProviderPresets = providerPresetViewsForRootWithResolver(cfg, root, resolver)
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		providerView := providerViewFromEntryForRootWithResolverAndCredentials(*p, isOfficialBuiltInProvider(*p), added[p.Name], root, resolver, credentialsRevision)
		providerView.RecommendedUpgradeAvailable = providerView.RecommendedUpgradeAvailable && config.CanUpgradeDeepSeekProviderProtocolUserConfig(p.Name)
		v.Providers = append(v.Providers, providerView)
	}
	return v
}

func sandboxEffectiveShellView(sh sandbox.Shell) string {
	if sh.Kind == sandbox.ShellPowerShell {
		if sh.SupportsChaining() {
			return "pwsh"
		}
		return "powershell"
	}
	path := strings.ToLower(strings.ReplaceAll(sh.Path, "\\", "/"))
	if strings.Contains(path, "/git/") && strings.HasSuffix(path, "bash.exe") {
		return "git-bash"
	}
	return "bash"
}

func botSettingsView(b config.BotConfig) BotSettingsView {
	mode := strings.TrimSpace(b.Feishu.Mode)
	if mode == "" {
		mode = "webhook"
	}
	return BotSettingsView{
		Enabled:            b.Enabled,
		Model:              b.Model,
		ToolApprovalMode:   normalizeBotConnectionToolApprovalMode(b.ToolApprovalMode),
		MaxSteps:           b.MaxSteps,
		DebounceMs:         b.DebounceMs,
		QueueMode:          b.QueueMode,
		QueueCap:           b.QueueCap,
		QueueDrop:          b.QueueDrop,
		IgnoreSelfMessages: b.IgnoreSelfMessages,
		SelfUserIDs: BotSelfUserIDsView{
			QQ:     nonNil(b.SelfUserIDs.QQ),
			Feishu: nonNil(b.SelfUserIDs.Feishu),
			Weixin: nonNil(b.SelfUserIDs.Weixin),
		},
		Control: BotControlView{
			Enabled:  b.Control.Enabled,
			Addr:     b.Control.Addr,
			TokenEnv: b.Control.TokenEnv,
		},
		Pairing: BotPairingView{
			Enabled:               b.Pairing.Enabled,
			RequestTTLMinutes:     b.Pairing.RequestTTLMinutes,
			MaxPendingPerPlatform: b.Pairing.MaxPendingPerPlatform,
		},
		Routes: botRouteViews(b.Routes),
		Allowlist: BotAllowlistView{
			Enabled:         b.Allowlist.Enabled,
			AllowAll:        b.Allowlist.AllowAll,
			QQUsers:         nonNil(b.Allowlist.QQUsers),
			FeishuUsers:     nonNil(b.Allowlist.FeishuUsers),
			WeixinUsers:     nonNil(b.Allowlist.WeixinUsers),
			QQApprovers:     nonNil(b.Allowlist.QQApprovers),
			FeishuApprovers: nonNil(b.Allowlist.FeishuApprovers),
			WeixinApprovers: nonNil(b.Allowlist.WeixinApprovers),
			QQAdmins:        nonNil(b.Allowlist.QQAdmins),
			FeishuAdmins:    nonNil(b.Allowlist.FeishuAdmins),
			WeixinAdmins:    nonNil(b.Allowlist.WeixinAdmins),
			QQGroups:        nonNil(b.Allowlist.QQGroups),
			FeishuGroups:    nonNil(b.Allowlist.FeishuGroups),
			WeixinGroups:    nonNil(b.Allowlist.WeixinGroups),
		},
		QQ: QQBotView{
			Enabled:          b.QQ.Enabled,
			AppID:            b.QQ.AppID,
			AppSecretEnv:     b.QQ.AppSecretEnv,
			SecretSet:        strings.TrimSpace(b.QQ.AppSecretEnv) != "" && os.Getenv(b.QQ.AppSecretEnv) != "",
			Sandbox:          b.QQ.Sandbox,
			Model:            b.QQ.Model,
			ToolApprovalMode: normalizeBotConnectionToolApprovalMode(b.QQ.ToolApprovalMode),
			WorkspaceRoot:    b.QQ.WorkspaceRoot,
			Access:           botAccessViewFromConfig(b.QQ.Access),
		},
		Feishu: FeishuBotView{
			Enabled:           b.Feishu.Enabled,
			Domain:            orDefault(strings.TrimSpace(b.Feishu.Domain), "feishu"),
			AppID:             b.Feishu.AppID,
			AppSecretEnv:      b.Feishu.AppSecretEnv,
			SecretSet:         strings.TrimSpace(b.Feishu.AppSecretEnv) != "" && os.Getenv(b.Feishu.AppSecretEnv) != "",
			VerificationToken: b.Feishu.VerificationToken,
			Mode:              mode,
			WebhookPort:       b.Feishu.WebhookPort,
			RequireMention:    b.Feishu.RequireMention,
		},
		Weixin: WeixinBotView{
			Enabled:   b.Weixin.Enabled,
			AccountID: b.Weixin.AccountID,
			TokenEnv:  b.Weixin.TokenEnv,
			TokenSet:  strings.TrimSpace(b.Weixin.TokenEnv) != "" && os.Getenv(b.Weixin.TokenEnv) != "",
			APIBase:   b.Weixin.APIBase,
		},
		Connections: botConnectionViews(b.Connections),
	}
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func botRouteViews(routes []config.BotRouteConfig) []BotRouteView {
	if len(routes) == 0 {
		return []BotRouteView{}
	}
	out := make([]BotRouteView, 0, len(routes))
	for _, route := range routes {
		out = append(out, BotRouteView{
			ConnectionID:     route.ConnectionID,
			Platform:         route.Platform,
			ChatType:         route.ChatType,
			ChatID:           route.ChatID,
			UserID:           route.UserID,
			ThreadID:         route.ThreadID,
			Model:            route.Model,
			ToolApprovalMode: normalizeBotConnectionToolApprovalMode(route.ToolApprovalMode),
			WorkspaceRoot:    route.WorkspaceRoot,
		})
	}
	return out
}

func botRouteConfigs(routes []BotRouteView) []config.BotRouteConfig {
	if len(routes) == 0 {
		return nil
	}
	out := make([]config.BotRouteConfig, 0, len(routes))
	for _, route := range routes {
		cfg := config.BotRouteConfig{
			ConnectionID:     strings.TrimSpace(route.ConnectionID),
			Platform:         strings.TrimSpace(route.Platform),
			ChatType:         strings.TrimSpace(route.ChatType),
			ChatID:           strings.TrimSpace(route.ChatID),
			UserID:           strings.TrimSpace(route.UserID),
			ThreadID:         strings.TrimSpace(route.ThreadID),
			Model:            strings.TrimSpace(route.Model),
			ToolApprovalMode: normalizeBotConnectionToolApprovalMode(route.ToolApprovalMode),
			WorkspaceRoot:    strings.TrimSpace(route.WorkspaceRoot),
		}
		if cfg.ConnectionID == "" && cfg.Platform == "" && cfg.ChatType == "" && cfg.ChatID == "" && cfg.UserID == "" && cfg.ThreadID == "" &&
			cfg.Model == "" && cfg.ToolApprovalMode == "" && cfg.WorkspaceRoot == "" {
			continue
		}
		out = append(out, cfg)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func botAccessViewFromConfig(access config.BotAccessConfig) BotAccessView {
	return BotAccessView{
		Enabled:        access.Enabled,
		AllowAll:       access.AllowAll,
		PairingEnabled: access.PairingEnabled,
		Users:          nonNil(access.Users),
		Groups:         nonNil(access.Groups),
		Approvers:      nonNil(access.Approvers),
		Admins:         nonNil(access.Admins),
	}
}

func botAccessConfigFromView(access BotAccessView) config.BotAccessConfig {
	return config.BotAccessConfig{
		Enabled:        access.Enabled,
		AllowAll:       access.AllowAll,
		PairingEnabled: access.PairingEnabled,
		Users:          trimList(access.Users),
		Groups:         trimList(access.Groups),
		Approvers:      trimList(access.Approvers),
		Admins:         trimList(access.Admins),
	}
}

func botDomainOrDefault(domain string) string {
	if strings.EqualFold(strings.TrimSpace(domain), "lark") {
		return "lark"
	}
	return "feishu"
}
