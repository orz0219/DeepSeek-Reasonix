package main

import (
	"path/filepath"
	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"strings"
)

type sessionBinding struct {
	path          string
	scope         string
	workspaceRoot string
	topicID       string
	topicTitle    string
	hasMeta       bool
	meta          agent.BranchMeta
}

func (a *App) reconcileTabWithPinnedSessionMeta(tab *WorkspaceTab) (string, bool) {
	if tab == nil {
		return "", false
	}
	a.mu.RLock()
	current := a.tabs[tab.ID]
	path := strings.TrimSpace(tab.SessionPath)
	ctrl := tab.Ctrl
	scope := tab.Scope
	workspaceRoot := tab.WorkspaceRoot
	a.mu.RUnlock()
	if current != tab {
		return "", false
	}
	if path != "" {
		if resolved, ok := a.reconcileTabWithSessionPath(tab, path); ok {
			return resolved, true
		}
	}
	if ctrl == nil {
		return "", false
	}
	path = strings.TrimSpace(ctrl.SessionPath())
	if path == "" {
		return "", false
	}
	binding, ok := a.resolveSessionBinding(path)
	if !ok {
		return "", false
	}
	if scope == "project" && binding.scope != "project" && normalizeProjectRoot(workspaceRoot) != "" {
		if root, ok := safeControllerWorkspaceRoot(ctrl); ok && sameProjectRoot(root, workspaceRoot) {
			return "", false
		}
	}
	a.applySessionBindingToTab(tab, binding)
	return binding.path, true
}

func (a *App) reconcileTabWithSessionPath(tab *WorkspaceTab, sessionPath string) (string, bool) {
	if tab == nil || strings.TrimSpace(sessionPath) == "" {
		return "", false
	}
	binding, ok := a.resolveSessionBinding(sessionPath)
	if !ok {
		return "", false
	}
	a.applySessionBindingToTab(tab, binding)
	return binding.path, true
}

func (a *App) applySessionBindingToTab(tab *WorkspaceTab, binding sessionBinding) {
	if tab == nil || binding.path == "" {
		return
	}
	var terminalSessions []*terminalSession
	reopenTerminalGate := false
	scope := binding.scope
	workspaceRoot := binding.workspaceRoot
	if scope == "" {
		scope = "global"
	}
	if scope == "project" {
		workspaceRoot = normalizeProjectRoot(workspaceRoot)
		if workspaceRoot == "" {
			return
		}
		a.registerProjectRoot(workspaceRoot)
	} else {
		scope = "global"
		workspaceRoot = globalTabWorkspaceRoot()
	}
	topicID := strings.TrimSpace(binding.topicID)
	topicTitle := strings.TrimSpace(binding.topicTitle)
	if topicTitle == "" && topicID != "" {
		topicTitle = topicTitleForTab(scope, workspaceRoot, topicID)
	}
	topicSource := ""
	if topicID != "" {
		topicSource = loadTopicTitleSource(topicTitleRoot(scope, workspaceRoot), topicID)
	}

	a.mu.Lock()
	current := a.tabs[tab.ID]
	if current != nil && current != tab {
		a.mu.Unlock()
		return
	}
	oldScope := tab.Scope
	oldWorkspaceRoot := tab.WorkspaceRoot
	changed := tab.Scope != scope ||
		tab.WorkspaceRoot != workspaceRoot ||
		canonicalTabSessionPath(tab.SessionPath) != canonicalTabSessionPath(binding.path)

	workspaceChanged := tab.Scope != scope || !sameProjectRoot(tab.WorkspaceRoot, workspaceRoot)
	if workspaceChanged && current == tab && a.terminals != nil {

		terminalSessions = a.terminals.detachForTab(tab.ID)
		reopenTerminalGate = !tab.ReadOnly && !tab.removed
	}
	tab.Scope = scope
	tab.WorkspaceRoot = workspaceRoot
	tab.SessionPath = canonicalTabSessionPath(binding.path)
	if topicID != "" {
		changed = changed || tab.TopicID != topicID
		tab.TopicID = topicID
		tab.topicTitleSource = topicSource
	}
	if topicTitle != "" {
		changed = changed || tab.TopicTitle != topicTitle
		tab.TopicTitle = topicTitle
	}
	if changed && current == tab {
		a.saveTabsLocked()
	}
	sink := tab.sink
	a.mu.Unlock()
	if workspaceChanged && a.workspaceHub != nil {
		a.workspaceHub.reconcileRoots()
	}
	if reopenTerminalGate {
		a.terminals.reopenForTab(tab.ID)
	}
	if len(terminalSessions) > 0 {
		a.terminals.closeSessions(terminalSessions)
	}
	if workspaceChanged && sink != nil {
		sink.Emit(event.Event{
			Kind:  event.Notice,
			Level: event.LevelWarn,
			Text:  sessionBindingWorkspaceNotice(oldScope, oldWorkspaceRoot, scope, workspaceRoot),
		})
	}
}

func sessionBindingWorkspaceNotice(oldScope, oldWorkspaceRoot, scope, workspaceRoot string) string {
	return "Session belongs to " + describeSessionBindingWorkspace(scope, workspaceRoot) +
		"; switched tab from " + describeSessionBindingWorkspace(oldScope, oldWorkspaceRoot) +
		" to match the saved session."
}

func describeSessionBindingWorkspace(scope, workspaceRoot string) string {
	if strings.TrimSpace(scope) == "project" && strings.TrimSpace(workspaceRoot) != "" {

		root := strings.ReplaceAll(strings.TrimSpace(workspaceRoot), `"`, `\"`)
		return `project workspace "` + root + `"`
	}
	return "global workspace"
}

func (a *App) resolveSessionBinding(sessionPath string) (sessionBinding, bool) {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" {
		return sessionBinding{}, false
	}
	for _, dir := range a.knownSessionDirs() {
		if binding, ok := sessionBindingInDir(dir, sessionPath); ok {
			return binding, true
		}
	}
	if !filepath.IsAbs(sessionPath) {
		return sessionBinding{}, false
	}
	path, err := filepath.Abs(sessionPath)
	if err != nil {
		return sessionBinding{}, false
	}
	meta, ok, err := agent.LoadBranchMeta(path)
	if err != nil || !ok {
		return sessionBinding{}, false
	}
	for _, dir := range sessionBindingCandidateDirs(meta) {
		if binding, ok := sessionBindingInDir(dir, path); ok {
			return binding, true
		}
	}
	return sessionBindingFromMeta(path, meta)
}

func sessionBindingCandidateDirs(meta agent.BranchMeta) []string {
	if meta.DefaultScope() == "project" {
		if root := normalizeProjectRoot(meta.WorkspaceRoot); root != "" {
			return []string{desktopSessionDir(root)}
		}
		return nil
	}
	return []string{desktopSessionDir(globalWorkspaceRoot()), config.SessionDir()}
}

func sessionBindingInDir(dir, sessionPath string) (sessionBinding, bool) {
	path, ok := pinnedTabSessionPath(dir, sessionPath)
	if !ok {
		return sessionBinding{}, false
	}
	meta, hasMeta, err := agent.LoadBranchMeta(path)
	if err != nil {
		return sessionBinding{}, false
	}
	scope, workspaceRoot, _, ownerOK := legacyMigrationTargetForDir(dir)
	if !ownerOK {
		if !hasMeta {
			return sessionBinding{}, false
		}
		return sessionBindingFromMeta(path, meta)
	}
	if scope == "global" {
		if !hasMeta {
			return sessionBinding{}, false
		}
		return sessionBindingFromMeta(path, meta)
	}
	binding := sessionBinding{
		path:          path,
		scope:         scope,
		workspaceRoot: workspaceRoot,
		hasMeta:       hasMeta,
		meta:          meta,
	}
	if hasMeta {
		binding.topicID = strings.TrimSpace(meta.TopicID)
		binding.topicTitle = strings.TrimSpace(meta.TopicTitle)
	}
	if binding.scope == "project" {
		binding.workspaceRoot = normalizeProjectRoot(binding.workspaceRoot)
	}
	return binding, true
}

func sessionBindingFromMeta(path string, meta agent.BranchMeta) (sessionBinding, bool) {
	scope := meta.DefaultScope()
	workspaceRoot := ""
	if scope == "project" {
		workspaceRoot = normalizeProjectRoot(meta.WorkspaceRoot)
		if workspaceRoot == "" {
			return sessionBinding{}, false
		}
	} else {
		scope = "global"
		workspaceRoot = globalTabWorkspaceRoot()
	}
	return sessionBinding{
		path:          path,
		scope:         scope,
		workspaceRoot: workspaceRoot,
		topicID:       strings.TrimSpace(meta.TopicID),
		topicTitle:    strings.TrimSpace(meta.TopicTitle),
		hasMeta:       true,
		meta:          meta,
	}, true
}

// activeTab returns the currently active tab (nil when there are no tabs).
// Self-locking; safe to call from any goroutine without external lock.
func (a *App) activeTab() *WorkspaceTab {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.activeTabID == "" {
		return nil
	}
	return a.tabs[a.activeTabID]
}

// activeTabLocked is like activeTab but assumes the caller already holds a.mu
// (either RLock or Lock). Use this inside critical sections that already own
// the lock to avoid double-locking a write-lock holder.
func (a *App) activeTabLocked() *WorkspaceTab {
	if a.activeTabID == "" {
		return nil
	}
	return a.tabs[a.activeTabID]
}

// activeCtrl returns the controller of the active tab, or nil.
// Self-locking; safe to call from any goroutine without external lock.
func (a *App) activeCtrl() control.SessionAPI {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.activeCtrlLocked()
}

// activeCtrlLocked is like activeCtrl but assumes the caller already holds a.mu.
func (a *App) activeCtrlLocked() control.SessionAPI {
	t := a.activeTabLocked()
	if t == nil {
		return nil
	}
	return t.Ctrl
}

func (a *App) tabByID(tabID string) *WorkspaceTab {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.tabByIDLocked(tabID)
}

func (a *App) tabByIDLocked(tabID string) *WorkspaceTab {
	if strings.TrimSpace(tabID) == "" {
		return a.activeTabLocked()
	}
	return a.tabs[tabID]
}

func (a *App) ctrlByTabID(tabID string) control.SessionAPI {
	a.mu.RLock()
	defer a.mu.RUnlock()
	tab := a.tabByIDLocked(tabID)
	if tab == nil {
		return nil
	}
	return tab.Ctrl
}
