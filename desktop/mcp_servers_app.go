package main

import (
	"maps"
	"sort"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/mcpdiag"
)

func (a *App) mcpServersView() []ServerView {
	out := []ServerView{}
	a.mu.RLock()
	tab := a.activeTabLocked()
	if tab == nil {
		a.mu.RUnlock()
		return out
	}
	ctrl := tab.Ctrl
	disabled := make(map[string]ServerView, len(tab.disabledMCP))
	maps.Copy(disabled, tab.disabledMCP)
	order := append([]string(nil), tab.mcpOrder...)
	workspaceRoot := tab.WorkspaceRoot
	tabID := tab.ID
	a.mu.RUnlock()
	if ctrl == nil {
		return out
	}
	seen := map[string]bool{}
	connected := map[string]bool{}
	retainedDisabled := map[string]ServerView{}
	configured := map[string]config.PluginEntry{}
	managedByPlugin := map[string]string{}
	var configuredEntries []config.PluginEntry
	if cfg, err := config.LoadForRoot(workspaceRoot); err == nil {
		configuredEntries = append(configuredEntries, cfg.Plugins...)
		for _, p := range configuredEntries {
			configured[p.Name] = p
			if owner, ok := cfg.PluginPackageOwner(p.Name); ok {
				managedByPlugin[p.Name] = owner
			}
		}
	}
	if h := ctrl.Host(); h != nil {
		for _, s := range h.Servers() {
			if disabledView, ok := disabled[s.Name]; ok {
				disabledView.Status = "disabled"
				disabledView.RuntimeState = "idle"
				disabledView.StartIntent = "off"
				disabledView.Error = ""
				if p, ok := configured[s.Name]; ok {
					disabledView = withPluginConfigInWorkspace(disabledView, p, workspaceRoot)
				}
				out = append(out, disabledView)
				retainedDisabled[s.Name] = disabledView
				seen[s.Name] = true
				delete(disabled, s.Name)
				continue
			}
			seen[s.Name] = true
			connected[s.Name] = true
			view := ServerView{
				Name: s.Name, Transport: s.Transport, Status: "connected", RuntimeState: "ready",
				Tools: s.Tools, Prompts: s.Prompts, Resources: s.Resources,
				HasTools: s.HasTools,
				ToolList: pluginToolsToView(s.ToolList),
			}
			if p, ok := configured[s.Name]; ok {
				view = withPluginConfigInWorkspace(view, p, workspaceRoot)
			}
			out = append(out, view)
		}
		for _, f := range h.Failures() {
			seen[f.Name] = true
			view := ServerView{
				Name: f.Name, Transport: f.Transport, Status: "failed", RuntimeState: "issue", Error: f.Error,
				RequiresLaunchApproval: f.RequiresLaunchApproval,
			}
			if p, ok := configured[f.Name]; ok {
				view = withPluginConfigInWorkspace(view, p, workspaceRoot)
			}
			out = append(out, view)
		}
		for _, name := range h.ConnectingServers() {
			if seen[name] {
				continue
			}
			seen[name] = true
			view := ServerView{Name: name, Status: "initializing", RuntimeState: "connecting"}
			if p, ok := configured[name]; ok {
				view = withPluginConfigInWorkspace(view, p, workspaceRoot)
			}
			out = append(out, view)
		}
	}

	if len(configuredEntries) > 0 {
		for _, p := range configuredEntries {
			if seen[p.Name] {
				continue
			}
			if s, ok := disabled[p.Name]; ok {
				s.Status = "disabled"
				s.RuntimeState = "idle"
				s.StartIntent = "off"
				s = withPluginConfigInWorkspace(s, p, workspaceRoot)
				s.Error = ""
				out = append(out, s)
				retainedDisabled[p.Name] = s
				seen[p.Name] = true
				delete(disabled, p.Name)
				continue
			}
			status := "disabled"
			startIntent := "off"
			if mcpEntryEnabled(p, workspaceRoot) {
				status = "deferred"
				startIntent = "automatic"
			}
			out = append(out, withPluginConfigInWorkspace(ServerView{Name: p.Name, Status: status, StartIntent: startIntent, RuntimeState: "idle"}, p, workspaceRoot))
			seen[p.Name] = true
		}
	}
	out = orderServerViews(out, order)
	for i := range out {
		out[i].ManagedByPlugin = managedByPlugin[out[i].Name]
		out[i] = finalizeServerView(out[i])
	}

	a.mu.Lock()
	if tab, ok := a.tabs[tabID]; ok {
		for name := range connected {
			delete(retainedDisabled, name)
		}
		tab.disabledMCP = retainedDisabled
		tab.mcpOrder = mergeServerOrder(tab.mcpOrder, out)
	}
	a.mu.Unlock()
	return out
}

func mcpEntryEnabled(p config.PluginEntry, workspace string) bool {
	enabled, err := config.DefaultMCPActivationStore().IsEnabled(p, workspace)
	if err != nil {
		return p.ShouldAutoStart()
	}
	return enabled
}

func mcpRuntimeState(status string) string {
	switch status {
	case "connected":
		return "ready"
	case "initializing":
		return "connecting"
	case "failed":
		return "issue"
	default:
		return "idle"
	}
}

func mcpAvailability(v ServerView) string {
	if !v.Enabled {
		return "disabled"
	}
	switch v.RuntimeState {
	case "ready":
		return "connected"
	case "connecting":
		return "starting"
	case "issue":
		if v.RequiresLaunchApproval {
			return "project_auth_changed"
		}
		if v.AuthStatus == "required" || v.AuthStatus == "possible" {
			return "auth_required"
		}
		return "start_failed"
	default:

		return "available_on_demand"
	}
}

func mcpActionForView(v ServerView) string {
	if v.RequiresLaunchApproval {
		return "authorize"
	}
	if v.AuthStatus == "required" {
		return "authenticate"
	}
	if v.RuntimeState == "issue" {
		return "retry"
	}
	return "none"
}

func finalizeServerView(v ServerView) ServerView {
	if v.ToolList == nil {
		v.ToolList = []ToolView{}
	}
	if v.Args == nil {
		v.Args = []string{}
	}
	if v.EnvKeys == nil {
		v.EnvKeys = []string{}
	}
	if v.HeaderKeys == nil {
		v.HeaderKeys = []string{}
	}
	v.ToolCount = v.Tools
	if v.ToolCount == 0 && len(v.ToolList) > 0 {
		v.ToolCount = len(v.ToolList)
		v.Tools = v.ToolCount
	}
	v.Installed = v.Configured || v.BuiltIn || v.Status != ""
	if v.Source == "" {
		switch {
		case v.BuiltIn:
			v.Source = "builtin"
		case v.ManagedByPlugin != "":
			v.Source = "plugin"
		case v.Configured:
			v.Source = "user"
		}
	}
	if v.RuntimeState == "" {
		v.RuntimeState = mcpRuntimeState(v.Status)
	}
	v.Availability = mcpAvailability(v)
	if v.Action == "" {
		v.Action = mcpActionForView(v)
	}

	v.AutoStart = v.Enabled
	if !v.Enabled {
		v.StartIntent = "off"
	} else if v.StartIntent == "" {
		v.StartIntent = "automatic"
	}
	return v
}

func withPluginConfig(v ServerView, p config.PluginEntry) ServerView {
	return withPluginConfigInWorkspace(v, p, "")
}

func withPluginConfigInWorkspace(v ServerView, p config.PluginEntry, workspace string) ServerView {
	tt := p.Type
	if tt == "" {
		tt = "stdio"
	}
	v.Transport = tt
	v.Configured = true
	v.Installed = true
	v.Source, v.ConfigSource = mcpServerSource(p.Source)
	v.Enabled = mcpEntryEnabled(p, workspace)
	v.AutoStart = v.Enabled
	v.Tier = p.ResolvedTier()
	if v.StartIntent == "" {
		if v.Enabled {
			v.StartIntent = "automatic"
		} else {
			v.StartIntent = "off"
		}
	}
	if !v.Enabled || v.Status == "disabled" {
		v.Status = "disabled"
		v.StartIntent = "off"
		v.RuntimeState = "idle"
	}
	if v.RuntimeState == "" {
		v.RuntimeState = mcpRuntimeState(v.Status)
	}
	v.Command = p.Command
	v.Args = append([]string(nil), p.Args...)
	v.URL = p.URL
	v.CallTimeoutSeconds = p.CallTimeoutSeconds
	v.ToolTimeoutSeconds = cloneStringIntMap(p.ToolTimeoutSeconds)

	v.RequiresLaunchApproval = false
	v.AuthConfigured = mcpdiag.HasAuthConfig(p.Headers, p.Env, p.URL)
	v.EnvKeys = nil
	v.HeaderKeys = nil
	if len(p.Env) > 0 {
		v.EnvKeys = make([]string, 0, len(p.Env))
		for k := range p.Env {
			v.EnvKeys = append(v.EnvKeys, k)
		}
		sort.Strings(v.EnvKeys)
	}
	if len(p.Headers) > 0 {
		v.HeaderKeys = make([]string, 0, len(p.Headers))
		for k := range p.Headers {
			v.HeaderKeys = append(v.HeaderKeys, k)
		}
		sort.Strings(v.HeaderKeys)
	}
	auth := mcpdiag.DiagnoseAuth(v.Transport, v.Status, v.Error, v.URL, v.AuthConfigured)
	v.AuthStatus = auth.Status
	v.AuthURL = auth.URL
	return v
}

func mcpServerSource(source config.MCPConfigSource) (kind, configSource string) {
	switch source {
	case config.MCPSourceProjectConfig:
		return "project", "reasonix.toml"
	case config.MCPSourceProjectMCPJSON:
		return "project", ".mcp.json"
	case config.MCPSourcePluginPackage:
		return "plugin", "plugin"
	case config.MCPSourceLegacyUser:
		return "user", "legacy config"
	case config.MCPSourceUserConfig:
		return "user", "config.toml"
	default:
		return "", ""
	}
}

const skillRootsCacheTTL = 10 * time.Second
