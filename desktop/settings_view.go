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
		DesktopLanguage:              cfg.DesktopLanguage(),
		DesktopCurrency:              cfg.DesktopCurrency(),
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
		providerView.Enabled = providerEnabled(cfg, p.Name)
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

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
