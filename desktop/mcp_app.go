package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/mcpregistry"
	"reasonix/internal/plugin"
)

// Capabilities projects the session's MCP servers (connected + failed) and skills
// for the MCP & Skills drawer. Non-nil slices so the frontend can map over them.
func (a *App) Capabilities() CapabilitiesView {
	skills := a.SkillsSettings()
	return CapabilitiesView{
		Servers:    a.MCPServers(),
		Skills:     skills.Skills,
		SkillRoots: skills.SkillRoots,
		Plugins:    a.Plugins(),
	}
}

// MCPServers returns only MCP server status for settings pages that do not need
// skill discovery.
func (a *App) MCPServers() []ServerView {
	return a.mcpServersView()
}

type MCPMarketplaceEntryView struct {
	Name              string   `json:"name"`
	SuggestedName     string   `json:"suggestedName"`
	Title             string   `json:"title,omitempty"`
	Description       string   `json:"description,omitempty"`
	Version           string   `json:"version,omitempty"`
	RepositoryURL     string   `json:"repositoryUrl,omitempty"`
	Installable       bool     `json:"installable"`
	UnavailableReason string   `json:"unavailableReason,omitempty"`
	Transport         string   `json:"transport,omitempty"`
	Command           string   `json:"command,omitempty"`
	Args              []string `json:"args"`
	URL               string   `json:"url,omitempty"`
}

type MCPMarketplaceView struct {
	Servers []MCPMarketplaceEntryView `json:"servers"`
	Cached  bool                      `json:"cached"`
	Warning string                    `json:"warning,omitempty"`
}

// MCPMarketplace explicitly queries the official MCP Registry. It is only
// called from the settings marketplace; startup and tool discovery never touch
// the network. A query-specific cache keeps the page useful during a registry
// outage without treating cached entries as installed servers.
func (a *App) MCPMarketplace(query string) (MCPMarketplaceView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := mcpregistry.New(mcpRegistryCachePath()).Search(ctx, query, 50)
	if err != nil {
		return MCPMarketplaceView{Servers: []MCPMarketplaceEntryView{}}, err
	}
	view := MCPMarketplaceView{
		Servers: make([]MCPMarketplaceEntryView, 0, len(result.Entries)),
		Cached:  result.Cached,
		Warning: result.Warning,
	}
	for _, entry := range result.Entries {
		view.Servers = append(view.Servers, mcpMarketplaceEntryView(entry))
	}
	return view, nil
}

// MCPMarketplaceResolve re-fetches one Registry entry immediately before the
// settings UI installs it. Offline cache remains useful for browsing, but it is
// never accepted as installation metadata.
func (a *App) MCPMarketplaceResolve(registryName string) (MCPMarketplaceEntryView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	entry, _, err := mcpregistry.New(mcpRegistryCachePath()).Resolve(ctx, registryName)
	if err != nil {
		return MCPMarketplaceEntryView{}, err
	}
	if _, err := entry.PluginEntry(""); err != nil {
		return MCPMarketplaceEntryView{}, err
	}
	return mcpMarketplaceEntryView(entry), nil
}

func mcpRegistryCachePath() string {
	if cacheDir := config.CacheDir(); cacheDir != "" {
		return filepath.Join(cacheDir, "mcp-registry-v0.1.json")
	}
	return ""
}

func mcpMarketplaceEntryView(entry mcpregistry.Entry) MCPMarketplaceEntryView {
	return MCPMarketplaceEntryView{
		Name:              entry.Name,
		SuggestedName:     entry.SuggestedName,
		Title:             entry.Title,
		Description:       entry.Description,
		Version:           entry.Version,
		RepositoryURL:     entry.RepositoryURL,
		Installable:       entry.Installable,
		UnavailableReason: entry.UnavailableReason,
		Transport:         entry.Transport,
		Command:           entry.Command,
		Args:              append([]string{}, entry.Args...),
		URL:               entry.URL,
	}
}

// lockRuntimeMutation serializes controller rebuild/teardown operations and
// freezes runtime admission so a captured controller or Host cannot be replaced
// or closed in flight. The caller must not hold App.mu; the lock order is
// runtimeRebuildMu -> runtimeAdmissionMu -> App/Host/Registry.
func (a *App) lockRuntimeMutation(operation string) func() {
	if hook := a.runtimeMutationBeforeLockHook; hook != nil {
		hook(operation)
	}
	a.runtimeRebuildMu.Lock()
	a.runtimeAdmissionMu.Lock()
	return func() {
		a.runtimeAdmissionMu.Unlock()
		a.runtimeRebuildMu.Unlock()
	}
}

// AuthorizeAndConnectMCPServer is retained for older generated Wails clients.
// Project configuration is trusted by default now, so the normal path simply
// reconnects the effective entry. Explicitly gated host specs still record
// their exact launch grant before reconnecting.
func (a *App) AuthorizeAndConnectMCPServer(name string) error {
	defer a.lockMCPMutation("authorize-connect")()

	tab, ctrl, root := a.activeMCPRuntime()
	if tab == nil || ctrl == nil {
		return fmt.Errorf("no active session")
	}
	host, releaseGates, err := a.lockMCPHostTurnGates("MCP authorization", ctrl)
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
	spec, err := a.mcpLaunchSpec(root, name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if spec.RequireLaunchApproval {
		if err := plugin.AuthorizeProjectSpecLaunch(ctx, spec); err != nil {
			return err
		}
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
	return nil
}

type mcpControllerTarget struct {
	ctrl    control.SessionAPI
	enabled bool
}

// lockMCPHostTurnGates freezes every runtime sharing ctrl's Host. Callers hold
// lockMCPMutation, so runtimeAdmissionMu's write side already prevents new turn
// admissions, builds, and teardown while this helper snapshots and gates the
// existing runtimes.
func (a *App) lockMCPHostTurnGates(setting string, ctrl control.SessionAPI) (*plugin.Host, func(), error) {
	if ctrl == nil {
		return nil, nil, fmt.Errorf("no active session")
	}
	host := ctrl.Host()
	release, err := a.lockRuntimeTurnGates(setting, func(tab *WorkspaceTab) bool {
		if host == nil {
			return tab.Ctrl == ctrl
		}
		return tab.Ctrl != nil && tab.Ctrl.Host() == host
	})
	return host, release, err
}

func disconnectMCPServerControllers(name string, preferred control.SessionAPI, controllers []mcpControllerTarget) bool {
	for _, target := range controllers {
		target.ctrl.UnregisterMCPServerTools(name)
	}
	disconnected := false
	if preferred != nil {
		disconnected = preferred.DisconnectMCPServer(name)
	}

	for _, target := range controllers {
		if target.ctrl == preferred {
			continue
		}
		disconnected = target.ctrl.DisconnectMCPServer(name) || disconnected
	}
	return disconnected
}

func (a *App) clearMCPServerTabState(name string, controllers []mcpControllerTarget) {
	selected := make(map[control.SessionAPI]bool, len(controllers))
	for _, target := range controllers {
		selected[target.ctrl] = true
	}
	a.mu.Lock()
	for _, tab := range a.runtimeTabsLocked() {
		if tab == nil || !selected[tab.Ctrl] {
			continue
		}
		delete(tab.disabledMCP, name)
		tab.mcpOrder = removeServerOrder(tab.mcpOrder, name)
	}
	a.mu.Unlock()
}

// reconnectMCPServerControllers establishes one shared client, then refreshes
// every enabled controller's provider-visible Registry. Disabled tabs remain
// suspended and reconnect only when explicitly enabled.
func reconnectMCPServerControllers(entry config.PluginEntry, controllers []mcpControllerTarget) error {
	var startErrors []error
	connectedTarget := -1
	for i, target := range controllers {
		if !target.enabled {
			continue
		}
		if _, err := target.ctrl.ConnectMCPServer(entry); err != nil {
			startErrors = append(startErrors, err)
			continue
		}
		connectedTarget = i
		break
	}
	if connectedTarget < 0 {

		return errors.Join(startErrors...)
	}

	var refreshErrors []error
	for i, target := range controllers {
		if !target.enabled || i == connectedTarget {
			continue
		}
		if _, err := target.ctrl.ConnectMCPServer(entry); err != nil {
			refreshErrors = append(refreshErrors, err)
		}
	}
	return errors.Join(refreshErrors...)
}

// mcpControllersSharingHost snapshots visible and detached runtimes before
// calling controller methods. App.mu is never held across Host/controller
// locks or network work. preferred (normally the active tab) is returned first.
func (a *App) mcpControllersSharingHost(host *plugin.Host, name string, preferred control.SessionAPI) []mcpControllerTarget {
	if host == nil {
		enabled := true
		a.mu.RLock()
		for _, tab := range a.runtimeTabsLocked() {
			if tab != nil && tab.Ctrl == preferred {
				_, disabled := tab.disabledMCP[name]
				enabled = !disabled
				break
			}
		}
		a.mu.RUnlock()
		return []mcpControllerTarget{{ctrl: preferred, enabled: enabled}}
	}
	a.mu.RLock()
	candidates := make([]mcpControllerTarget, 0, len(a.tabs)+len(a.detachedSessions))
	for _, tab := range a.runtimeTabsLocked() {
		if tab == nil || tab.Ctrl == nil {
			continue
		}
		_, disabled := tab.disabledMCP[name]
		candidates = append(candidates, mcpControllerTarget{ctrl: tab.Ctrl, enabled: !disabled})
	}
	a.mu.RUnlock()

	byController := make(map[control.SessionAPI]int, len(candidates))
	targets := make([]mcpControllerTarget, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ctrl.Host() != host {
			continue
		}
		if idx, ok := byController[candidate.ctrl]; ok {
			targets[idx].enabled = targets[idx].enabled || candidate.enabled
			continue
		}
		byController[candidate.ctrl] = len(targets)
		targets = append(targets, candidate)
	}
	if len(targets) == 0 {
		return []mcpControllerTarget{{ctrl: preferred, enabled: true}}
	}
	if idx, ok := byController[preferred]; ok && idx > 0 {
		targets[0], targets[idx] = targets[idx], targets[0]
	}
	return targets
}

// lockRuntimeTurnGates locks the turn gate of every runtime tab selected by
// affected (nil selects all visible and detached runtime tabs) in stable tab-ID
// order, then verifies under the gates that no gated controller has active
// runtime work. Callers must hold runtimeRebuildMu and the write side of
// runtimeAdmissionMu (normally through lockMCPMutation), which freezes new turn
// admission, controller builds, and runtime teardown before this snapshot.
// On success the returned release func unlocks the per-tab gates in reverse
// order; on error every gate acquired here is already unlocked.
func (a *App) lockRuntimeTurnGates(setting string, affected func(*WorkspaceTab) bool) (func(), error) {
	a.mu.RLock()
	all := a.runtimeTabsLocked()
	tabs := make([]*WorkspaceTab, 0, len(all))
	for _, tab := range all {
		if tab == nil || (affected != nil && !affected(tab)) {
			continue
		}
		tabs = append(tabs, tab)
	}
	a.mu.RUnlock()
	sort.Slice(tabs, func(i, j int) bool { return tabs[i].ID < tabs[j].ID })
	locked := 0
	release := func() {
		for i := locked - 1; i >= 0; i-- {
			tabs[i].turnStartMu.Unlock()
		}
	}
	for _, tab := range tabs {
		tab.turnStartMu.Lock()
		locked++
	}

	a.mu.RLock()
	for _, tab := range tabs {
		if err := rebuildControllerActiveWorkErrorFor(tab.Ctrl, setting); err != nil {
			a.mu.RUnlock()
			release()
			return nil, err
		}
	}
	a.mu.RUnlock()
	return release, nil
}

// disconnectMCPServerAllRuntimes removes an uninstalled MCP server from every
// live runtime: all visible and detached runtime tabs, across every shared
// Host — a global plugin uninstall must not leave sibling tabs exposing stale
// provider-visible tools or other workspaces running the removed server.
// DisconnectMCPServer stops the shared client once per Host and drops the tool
// prefix from every other controller's registry.
func (a *App) disconnectMCPServerAllRuntimes(serverName string) bool {
	a.mu.RLock()
	ctrls := make([]control.SessionAPI, 0, len(a.tabs)+len(a.detachedSessions))
	seen := make(map[control.SessionAPI]bool, len(a.tabs)+len(a.detachedSessions))
	for _, tab := range a.runtimeTabsLocked() {
		if tab == nil || tab.Ctrl == nil || seen[tab.Ctrl] {
			continue
		}
		seen[tab.Ctrl] = true
		ctrls = append(ctrls, tab.Ctrl)
	}
	a.mu.RUnlock()
	disconnected := false
	for _, ctrl := range ctrls {
		if ctrl.DisconnectMCPServer(serverName) {
			disconnected = true
		}
	}
	return disconnected
}
