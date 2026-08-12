package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sort"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/plugin"
)

func mcpServerInputEntry(in MCPServerInput) config.PluginEntry {
	entry := config.PluginEntry{
		Name:               strings.TrimSpace(in.Name),
		Type:               normalizeMCPTransport(in.Transport),
		Command:            strings.TrimSpace(in.Command),
		Args:               append([]string(nil), in.Args...),
		URL:                strings.TrimSpace(in.URL),
		Env:                in.Env,
		Headers:            in.Headers,
		AutoStart:          in.AutoStart,
		CallTimeoutSeconds: mcpIntValue(in.CallTimeoutSeconds),
		ToolTimeoutSeconds: cloneStringIntMap(in.ToolTimeoutSeconds),
		Source:             config.MCPSourceUserConfig,
	}
	entry, _ = config.NormalizePluginCommandLine(entry)
	return entry
}

// InstallMCPServer is the desktop's high-level install transaction. A normal
// handshake failure leaves no config behind; authentication-required servers
// are retained so the user can complete OAuth and retry. Only a ready result is
// published to every controller sharing the Host.
func (a *App) InstallMCPServer(in MCPServerInput) (plugin.MCPInstallResult, error) {
	defer a.lockMCPMutation("add")()

	_, ctrl, root := a.activeMCPRuntime()
	if ctrl == nil {
		return plugin.MCPInstallResult{}, fmt.Errorf("no active session")
	}
	host, releaseGates, err := a.lockMCPHostTurnGates("MCP server", ctrl)
	if err != nil {
		return plugin.MCPInstallResult{}, err
	}
	defer releaseGates()

	entry := mcpServerInputEntry(in)
	if entry.Name == "" {
		return plugin.InstallResultForError(entry.Name, fmt.Errorf("MCP server name is required")), nil
	}
	if _, found, lookupErr := desktopEffectiveMCPServer(root, entry.Name); lookupErr != nil {
		return plugin.MCPInstallResult{}, lookupErr
	} else if found {
		return plugin.InstallResultForError(entry.Name, fmt.Errorf("MCP server %q is already installed", entry.Name)), nil
	}

	controllers := a.mcpControllersSharingHost(host, entry.Name, ctrl)
	toolCount, connectErr := ctrl.ConnectMCPServer(entry)
	if connectErr != nil {
		result := plugin.InstallResultForError(entry.Name, connectErr)
		if result.State != "action_required" {
			if host != nil {
				host.ClearFailure(entry.Name)
			}
			return result, nil
		}
		if err := a.saveDesktopMCPServer(root, entry); err != nil {
			return plugin.MCPInstallResult{}, err
		}
		if err := persistMCPInstallActivation(entry, root); err != nil {
			_, rollbackErr := a.removeDesktopMCPServer(root, entry.Name)
			if host != nil {
				host.ClearFailure(entry.Name)
			}
			return plugin.MCPInstallResult{}, errors.Join(err, rollbackErr)
		}
		a.bumpExtensionGeneration()
		recordMCPFailure(ctrl, entry, connectErr)
		return result, nil
	}
	var publishErrs []error
	for _, target := range controllers {
		if target.ctrl == ctrl || !target.enabled {
			continue
		}
		if _, err := target.ctrl.ConnectMCPServer(entry); err != nil {
			publishErrs = append(publishErrs, err)
		}
	}
	if err := errors.Join(publishErrs...); err != nil {
		disconnectMCPServerControllers(entry.Name, ctrl, controllers)
		return plugin.MCPInstallResult{}, fmt.Errorf("publish MCP tools: %w", err)
	}
	if err := a.saveDesktopMCPServer(root, entry); err != nil {
		disconnectMCPServerControllers(entry.Name, ctrl, controllers)
		return plugin.MCPInstallResult{}, err
	}
	if err := persistMCPInstallActivation(entry, root); err != nil {
		disconnectMCPServerControllers(entry.Name, ctrl, controllers)
		_, rollbackErr := a.removeDesktopMCPServer(root, entry.Name)

		disconnectMCPServerControllers(entry.Name, ctrl, controllers)
		return plugin.MCPInstallResult{}, errors.Join(err, rollbackErr)
	}
	a.bumpExtensionGeneration()
	return plugin.ReadyInstallResult(entry.Name, toolCount), nil
}
func persistMCPInstallActivation(entry config.PluginEntry, root string) error {
	store := config.DefaultMCPActivationStore()
	if !entry.ShouldAutoStart() {
		return store.ClearServer(entry, root)
	}
	return store.SetServerEnabled(entry, root, true)
}

// AddMCPServer is retained for old generated Wails clients. New clients use
// InstallMCPServer so authentication and retry states remain structured.
func (a *App) AddMCPServer(in MCPServerInput) (int, error) {
	result, err := a.InstallMCPServer(in)
	if err != nil {
		return 0, err
	}
	if result.State != "ready" {
		return 0, fmt.Errorf("%s", result.Message)
	}
	return result.ToolCount, nil
}

// UpdateMCPServer edits a persisted external MCP server. The name is the stable
// identity; callers must remove + add if they want to rename a server.
func (a *App) UpdateMCPServer(name string, in MCPServerInput) error {
	defer a.lockMCPMutation("update")()

	tab, ctrl, root := a.activeMCPRuntime()
	if tab == nil || ctrl == nil {
		return fmt.Errorf("no active session")
	}
	host, releaseGates, err := a.lockMCPHostTurnGates("MCP server", ctrl)
	if err != nil {
		return err
	}
	defer releaseGates()
	controllers := a.mcpControllersSharingHost(host, name, ctrl)
	if strings.TrimSpace(in.Name) != "" && strings.TrimSpace(in.Name) != name {
		return fmt.Errorf("renaming MCP servers is not supported; remove and add a new server")
	}
	updated, found, err := a.desktopMCPServerForEdit(root, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no configured MCP server named %q", name)
	}
	original := updated
	updated.Type = normalizeMCPTransport(in.Transport)
	updated.Command = strings.TrimSpace(in.Command)
	updated.Args = append([]string(nil), in.Args...)
	updated.URL = strings.TrimSpace(in.URL)
	updated.Tier = ""
	if in.Env != nil {
		updated.Env = in.Env
	}
	if in.Headers != nil {
		updated.Headers = in.Headers
	}
	if in.AutoStart != nil {
		value := *in.AutoStart
		updated.AutoStart = &value
	}
	if in.CallTimeoutSeconds != nil {
		updated.CallTimeoutSeconds = *in.CallTimeoutSeconds
	}
	if in.ToolTimeoutSeconds != nil {
		updated.ToolTimeoutSeconds = cloneStringIntMap(in.ToolTimeoutSeconds)
	}
	updated, _ = config.NormalizePluginCommandLine(updated)
	if updated.Type == "stdio" {
		updated.URL = ""
	} else {
		updated.Command = ""
		updated.Args = nil
	}
	enabled := false
	for _, target := range controllers {
		enabled = enabled || target.enabled
	}
	if !enabled {
		return a.saveDesktopMCPServerAndBump(root, updated)
	}
	spec, specErr := a.mcpLaunchSpecForEntry(root, updated)
	if specErr != nil {
		return specErr
	}
	if spec.RequireLaunchApproval {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := plugin.AuthorizeProjectSpecLaunch(ctx, spec); err != nil {
			return err
		}
	}
	disconnectMCPServerControllers(name, ctrl, controllers)
	if err := reconnectMCPServerControllers(updated, controllers); err != nil {
		rollbackErr := reconnectMCPServerControllers(original, controllers)
		recordMCPFailure(ctrl, updated, err)
		return errors.Join(err, rollbackErr)
	}
	if err := a.saveDesktopMCPServer(root, updated); err != nil {
		disconnectMCPServerControllers(name, ctrl, controllers)
		rollbackErr := reconnectMCPServerControllers(original, controllers)
		return errors.Join(err, rollbackErr)
	}
	a.bumpExtensionGeneration()
	return nil
}

// RemoveMCPServer disconnects a live server and drops it from config (the row's ✕).
// Uninstall also clears durable activation overrides for that server.
func (a *App) RemoveMCPServer(name string) error {
	defer a.lockMCPMutation("remove")()

	tab, ctrl, root := a.activeMCPRuntime()
	if tab == nil || ctrl == nil {
		return fmt.Errorf("no active session")
	}
	host, releaseGates, err := a.lockMCPHostTurnGates("MCP server", ctrl)
	if err != nil {
		return err
	}
	defer releaseGates()
	controllers := a.mcpControllersSharingHost(host, name, ctrl)
	if err := ensureMCPServerDirectlyWritable(root, name); err != nil {
		return err
	}
	entry, hasEntry, _ := desktopEffectiveMCPServer(root, name)
	removed, err := a.removeDesktopMCPServer(root, name)
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("no removable MCP server named %q", name)
	}
	if hasEntry {
		_ = config.DefaultMCPActivationStore().ClearServer(entry, root)
	}
	authCleanupErr := reconcileRemovedMCPAuthentication(name, a.mcpWorkspaceRoots(root))
	disconnectMCPServerControllers(name, ctrl, controllers)
	if host != nil {
		host.ClearFailure(name)
	}
	restoreMCPServerFallbacks(name, controllers)
	a.clearMCPServerTabState(name, controllers)
	a.bumpExtensionGeneration()
	return authCleanupErr
}

// restoreMCPServerFallbacks makes a lower-priority declaration immediately
// available after its project override is removed. Registration is cache-first:
// it restores cached tools or a connect placeholder without starting a process.
func restoreMCPServerFallbacks(name string, controllers []mcpControllerTarget) {
	for _, target := range controllers {
		root := target.ctrl.WorkspaceRoot()
		cfg, err := config.LoadForRoot(root)
		if err != nil {
			slog.Warn("desktop: reload MCP fallback after remove", "name", name, "workspace", root, "err", err)
			continue
		}
		entry, found := findPluginEntry(cfg.Plugins, name)
		if !found || !mcpEntryEnabled(entry, root) {
			continue
		}
		if _, err := target.ctrl.RegisterMCPServerOnDemand(entry); err != nil {
			slog.Warn("desktop: restore MCP fallback after remove", "name", name, "workspace", root, "err", err)
		}
	}
}

// ReconnectMCPServer disconnects the server if it is already connected (to force
// a fresh handshake and tool re-registration), then reconnects.  Failures are
// recorded on the Host so the UI can render them.
func (a *App) ReconnectMCPServer(name string) error {
	defer a.lockMCPMutation("reconnect")()

	tab, ctrl, root := a.activeMCPRuntime()
	if tab == nil || ctrl == nil {
		return fmt.Errorf("no active session")
	}
	host, releaseGates, err := a.lockMCPHostTurnGates("MCP server", ctrl)
	if err != nil {
		return err
	}
	defer releaseGates()
	entry, found, err := desktopEffectiveMCPServer(root, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no configured MCP server named %q", name)
	}
	controllers := a.mcpControllersSharingHost(host, name, ctrl)
	for i := range controllers {
		if controllers[i].ctrl == ctrl {
			controllers[i].enabled = true
		}
	}
	disconnectMCPServerControllers(name, ctrl, controllers)
	if host != nil {
		host.ClearFailure(name)
	}
	if err := reconnectMCPServerControllers(entry, controllers); err != nil {
		recordMCPFailure(ctrl, entry, err)
		return err
	}
	a.mu.Lock()
	delete(tab.disabledMCP, name)
	a.mu.Unlock()
	a.bumpExtensionGeneration()
	return nil
}

// SetMCPServerEnabled is the durable enable/disable switch for an installed MCP
// server. It writes $REASONIX_HOME/mcp-activation.json and updates the live
// registry: disable removes tools and may stop the process; enable restores
// cached tools and starts the process only on the next real tool call.
func (a *App) SetMCPServerEnabled(name string, enabled bool) error {
	defer a.lockMCPMutation("set-enabled")()

	tab, ctrl, root := a.activeMCPRuntime()
	if tab == nil || ctrl == nil {
		return fmt.Errorf("no active session")
	}
	a.mu.RLock()
	hostKey := tab.SharedHostKey
	a.mu.RUnlock()
	if err := rebuildControllerActiveWorkErrorFor(ctrl, "MCP server"); err != nil {
		return err
	}
	configuredEntry, hasConfiguredEntry, err := desktopEffectiveMCPServer(root, name)
	if err != nil {
		return err
	}
	if !hasConfiguredEntry {
		return fmt.Errorf("no configured MCP server named %q", name)
	}
	activationStore := config.DefaultMCPActivationStore()
	scope, workspaceFP, source, owner := config.ActivationIdentity(configuredEntry, root)
	previousEnabled, previousFound, err := activationStore.Lookup(scope, workspaceFP, source, owner, configuredEntry.Name)
	if err != nil {
		return err
	}
	if err := activationStore.SetServerEnabled(configuredEntry, root, enabled); err != nil {
		return err
	}
	a.bumpExtensionGeneration()
	if enabled {

		_, err := a.registerConfiguredMCPServerForTab(tab, name)
		if err == nil {
			a.mu.Lock()
			delete(tab.disabledMCP, name)
			a.mu.Unlock()
			return nil
		}
		var rollbackErr error
		if previousFound {
			rollbackErr = activationStore.SetServerEnabled(configuredEntry, root, previousEnabled)
		} else {
			rollbackErr = activationStore.ClearServer(configuredEntry, root)
		}
		return errors.Join(err, rollbackErr)
	}
	if s, ok := findMCPServerView(ctrl, name); ok {
		s.Status = "disabled"
		s.Enabled = false
		s.Error = ""
		s = finalizeServerView(s)
		a.mu.Lock()
		if tab.disabledMCP == nil {
			tab.disabledMCP = map[string]ServerView{}
		}
		tab.disabledMCP[name] = s
		tab.mcpOrder = mergeServerOrder(tab.mcpOrder, []ServerView{s})
		a.mu.Unlock()
	} else {
		s := finalizeServerView(withPluginConfig(ServerView{Name: name, Status: "disabled", Enabled: false}, configuredEntry))
		a.mu.Lock()
		if tab.disabledMCP == nil {
			tab.disabledMCP = map[string]ServerView{}
		}
		tab.disabledMCP[name] = s
		tab.mcpOrder = mergeServerOrder(tab.mcpOrder, []ServerView{s})
		a.mu.Unlock()
	}
	if hostKey != "" {
		ctrl.UnregisterMCPServerTools(name)
	} else {
		ctrl.DisconnectMCPServer(name)
	}
	return nil
}

func (a *App) registerConfiguredMCPServerForTab(tab *WorkspaceTab, name string) (int, error) {
	a.mu.RLock()
	var ctrl control.SessionAPI
	root := ""
	if tab != nil {
		ctrl = tab.Ctrl
		root = tab.WorkspaceRoot
	}
	a.mu.RUnlock()
	if ctrl == nil {
		return 0, fmt.Errorf("no active session")
	}
	cfg, err := config.LoadForRoot(root)
	if err != nil {
		return 0, err
	}
	for _, p := range cfg.Plugins {
		if p.Name == name {
			return ctrl.RegisterMCPServerOnDemand(p)
		}
	}
	return 0, fmt.Errorf("no configured MCP server named %q", name)
}

// SetMCPServerTier is kept for old desktop bindings. New config writes drop the
// retired tier field.
func (a *App) SetMCPServerTier(name, tier string) error {
	defer a.lockMCPMutation("set-tier")()

	tier = normalizeMCPTier(tier)
	tab, ctrl, root := a.activeMCPRuntime()
	if tab != nil {
		if err := rebuildControllerActiveWorkErrorFor(ctrl, "MCP server"); err != nil {
			return err
		}
	}
	updated, found, err := a.desktopMCPServerForEdit(root, name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no configured MCP server named %q", name)
	}
	updated.Tier = tier
	if !updated.ShouldAutoStart() {
		on := true
		updated.AutoStart = &on
	}
	if err := a.saveDesktopMCPServer(root, updated); err != nil {
		return err
	}
	a.bumpExtensionGeneration()
	if tab != nil && ctrl != nil && !mcpConnected(ctrl, name) {
		if _, err := ctrl.ConnectMCPServer(updated); err != nil {
			recordMCPFailure(ctrl, updated, err)
			return nil
		}
		a.mu.Lock()
		delete(tab.disabledMCP, name)
		a.mu.Unlock()
	}
	return nil
}

func (a *App) desktopMCPServerForEdit(root, name string) (config.PluginEntry, bool, error) {

	return desktopEffectiveMCPServer(root, name)
}

// desktopEffectiveMCPServer returns the same merged entry the runtime starts.
// Its provenance identifies the exact project or global declaration that edit
// and remove operations must mutate.
func desktopEffectiveMCPServer(root, name string) (config.PluginEntry, bool, error) {
	cfg, err := config.LoadForRoot(root)
	if err != nil {
		return config.PluginEntry{}, false, err
	}
	p, ok := findPluginEntry(cfg.Plugins, name)
	return p, ok, nil
}

func (a *App) saveDesktopMCPServer(root string, entry config.PluginEntry) error {
	if err := ensureMCPServerDirectlyWritable(root, entry.Name); err != nil {
		return err
	}
	_, err := config.UpsertPluginInSourceForRoot(root, entry)
	return err
}

func ensureMCPServerDirectlyWritable(root, name string) error {
	cfg, err := config.LoadForRoot(root)
	if err != nil {
		return err
	}
	if owner, ok := cfg.PluginPackageOwner(name); ok {
		return fmt.Errorf("MCP server %q is managed by plugin %q; disable or remove the plugin instead", name, owner)
	}
	return nil
}

func (a *App) removeDesktopMCPServer(root, name string) (bool, error) {
	_, removed, _, err := config.RemovePluginFromEffectiveSourceForRoot(root, name)
	return removed, err
}

func findPluginEntry(entries []config.PluginEntry, name string) (config.PluginEntry, bool) {
	for _, p := range entries {
		if p.Name == name {
			return p, true
		}
	}
	return config.PluginEntry{}, false
}

func normalizeMCPTier(tier string) string {
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

func normalizeMCPTransport(transport string) string {
	switch strings.ToLower(strings.TrimSpace(transport)) {
	case "http", "streamable-http":
		return "http"
	case "sse":
		return "sse"
	case "", "stdio":
		return "stdio"
	default:
		return strings.ToLower(strings.TrimSpace(transport))
	}
}

func mcpIntValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func cloneStringIntMap(values map[string]int) map[string]int {
	if values == nil {
		return nil
	}
	out := make(map[string]int, len(values))
	maps.Copy(out, values)
	return out
}

func mcpConnected(ctrl control.SessionAPI, name string) bool {
	if ctrl == nil || ctrl.Host() == nil {
		return false
	}
	for _, s := range ctrl.Host().Servers() {
		if s.Name == name {
			return true
		}
	}
	return false
}

func mcpFailed(ctrl control.SessionAPI, name string) bool {
	if ctrl == nil || ctrl.Host() == nil {
		return false
	}
	for _, f := range ctrl.Host().Failures() {
		if f.Name == name {
			return true
		}
	}
	return false
}

func recordMCPFailure(ctrl control.SessionAPI, e config.PluginEntry, err error) {
	if ctrl == nil || ctrl.Host() == nil || err == nil {
		return
	}
	exp := e.ExpandedPlugin()
	ctrl.Host().RecordFailure(plugin.Spec{
		Name:    exp.Name,
		Type:    exp.Type,
		Command: exp.Command,
		Args:    exp.Args,
		Env:     exp.Env,
		URL:     exp.URL,
		Headers: exp.Headers,
	}, err)
}

func findMCPServerView(ctrl control.SessionAPI, name string) (ServerView, bool) {
	if ctrl == nil || ctrl.Host() == nil {
		return ServerView{}, false
	}
	for _, s := range ctrl.Host().Servers() {
		if s.Name == name {
			view := ServerView{
				Name: s.Name, Transport: s.Transport, Status: "connected",
				Tools: s.Tools, Prompts: s.Prompts, Resources: s.Resources,
				HasTools: s.HasTools,
				ToolList: pluginToolsToView(s.ToolList),
			}
			return view, true
		}
	}
	for _, f := range ctrl.Host().Failures() {
		if f.Name == name {
			return ServerView{
				Name: f.Name, Transport: f.Transport, Status: "failed", Error: f.Error,
				RequiresLaunchApproval: f.RequiresLaunchApproval,
			}, true
		}
	}
	return ServerView{}, false
}

func pluginToolsToView(tools []plugin.ToolInfo) []ToolView {
	if len(tools) == 0 {
		return []ToolView{}
	}
	out := make([]ToolView, 0, len(tools))
	for _, t := range tools {
		out = append(out, ToolView{
			Name: t.Name, Description: t.Description, ReadOnlyHint: t.ReadOnlyHint, DestructiveHint: t.DestructiveHint, SchemaError: t.SchemaError,
		})
	}
	return out
}

func sameStringList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func orderServerViews(servers []ServerView, order []string) []ServerView {
	pos := make(map[string]int, len(order))
	for i, name := range order {
		pos[name] = i
	}
	sort.SliceStable(servers, func(i, j int) bool {
		pi, iok := pos[servers[i].Name]
		pj, jok := pos[servers[j].Name]
		switch {
		case iok && jok:
			return pi < pj
		case iok:
			return true
		case jok:
			return false
		default:
			return false
		}
	})
	return servers
}

func mergeServerOrder(order []string, servers []ServerView) []string {
	seen := make(map[string]bool, len(order)+len(servers))
	next := make([]string, 0, len(order)+len(servers))
	for _, name := range order {
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		next = append(next, name)
	}
	for _, s := range servers {
		if s.Name == "" || seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		next = append(next, s.Name)
	}
	return next
}

func removeServerOrder(order []string, name string) []string {
	if name == "" || len(order) == 0 {
		return order
	}
	next := order[:0]
	for _, n := range order {
		if n != name {
			next = append(next, n)
		}
	}
	return next
}

// ModelInfo is one (provider, model) the bottom switcher can pick. Ref ("provider/
// model") is what SetModel takes; Provider/Model are for display.
type ModelInfo struct {
	Ref      string `json:"ref"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Current  bool   `json:"current"`
}

type EffortInfo struct {
	Supported bool     `json:"supported"`
	Current   string   `json:"current"`
	Default   string   `json:"default"`
	Levels    []string `json:"levels"`
}
