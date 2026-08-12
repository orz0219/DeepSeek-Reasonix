package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reasonix/internal/agent"
	"strings"
)

func (t *WorkspaceTab) currentSessionPath() string {
	if t == nil {
		return ""
	}
	tabPath := strings.TrimSpace(t.SessionPath)

	if tabPath != "" && sessionRuntimeKey(tabPath) == t.sessionLeaseRuntimeKey() {
		return tabPath
	}
	if t.Ctrl != nil {
		if path := strings.TrimSpace(t.Ctrl.SessionPath()); path != "" {
			return path
		}
	}
	return tabPath
}

func (t *WorkspaceTab) hasActiveRuntimeWork() bool {
	if t == nil || t.Ctrl == nil {
		return false
	}
	status := t.Ctrl.RuntimeStatus()
	return status.Running || status.PendingPrompt || status.BackgroundJobs > 0
}

// sessionRuntimeKey is the comparison/map key for "same session" checks. It
// layers agent.CanonicalSessionPath on top of the desktop path normalization
// so the key matches the form held by session leases (lowercased on Windows).
// Comparing a lease's Path() against a raw tab path without this fold made
// every rebuild on Windows look like a foreign holder (self-lock, #5999).
// Keys are identities only — never use them as display or file paths.
func sessionRuntimeKey(path string) string {
	return agent.CanonicalSessionPath(canonicalTabSessionPath(path))
}

var sessionLeaseAcquireHookForTest func()

func (t *WorkspaceTab) ensureSessionLease(path string) error {
	if t == nil || t.ReadOnly {
		return nil
	}
	key := sessionRuntimeKey(path)
	if key == "" {
		return nil
	}
	t.sessionLeaseMu.Lock()
	if t.sessionLease != nil && sessionRuntimeKey(t.sessionLease.Path()) == key {
		t.storeSessionLeaseRuntimeKey(key)
		t.sessionLeaseMu.Unlock()
		return nil
	}
	lease, err := agent.TryAcquireSessionLease(key)
	if err != nil {
		t.sessionLeaseMu.Unlock()
		return err
	}
	if hook := sessionLeaseAcquireHookForTest; hook != nil {
		hook()
	}
	old := t.sessionLease
	t.sessionLease = lease
	t.storeSessionLeaseRuntimeKey(key)
	t.sessionLeaseMu.Unlock()
	if old != nil {
		old.Release()
	}
	return nil
}

func (t *WorkspaceTab) releaseSessionLease() {
	if t == nil {
		return
	}
	t.sessionLeaseMu.Lock()
	lease := t.sessionLease
	t.sessionLease = nil
	t.storeSessionLeaseRuntimeKey("")
	t.sessionLeaseMu.Unlock()
	if lease != nil {
		lease.Release()
	}
}

// takeSessionLease removes and returns the tab's current lease WITHOUT
// releasing it, so ownership can transfer to another holder. All access to
// t.sessionLease must go through sessionLeaseMu; never read or assign the
// field directly outside these helpers.
func (t *WorkspaceTab) takeSessionLease() *agent.SessionLease {
	if t == nil {
		return nil
	}
	t.sessionLeaseMu.Lock()
	lease := t.sessionLease
	t.sessionLease = nil
	t.storeSessionLeaseRuntimeKey("")
	t.sessionLeaseMu.Unlock()
	return lease
}

// adoptSessionLease installs lease as the tab's session lease, releasing any
// previously held lease unless it is the very same lease. A nil tab releases
// the lease immediately so ownership is never dropped on the floor.
func (t *WorkspaceTab) adoptSessionLease(lease *agent.SessionLease) {
	if t == nil {
		if lease != nil {
			lease.Release()
		}
		return
	}
	t.sessionLeaseMu.Lock()
	old := t.sessionLease
	t.sessionLease = lease
	key := ""
	if lease != nil {
		key = sessionRuntimeKey(lease.Path())
	}
	t.storeSessionLeaseRuntimeKey(key)
	t.sessionLeaseMu.Unlock()
	if old != nil && old != lease {
		old.Release()
	}
}

func (t *WorkspaceTab) storeSessionLeaseRuntimeKey(key string) {
	if t == nil || key == "" {
		if t != nil {
			t.sessionLeaseKey.Store(nil)
		}
		return
	}
	stored := key
	t.sessionLeaseKey.Store(&stored)
}

// sessionLeaseRuntimeKey reports the runtime key of the currently held lease,
// or "" when no lease is held. The mirror is lock-free so callers holding
// App.mu never wait on a concurrent lease acquisition (whose test hook and
// platform file operations run under sessionLeaseMu).
func (t *WorkspaceTab) sessionLeaseRuntimeKey() string {
	if t == nil {
		return ""
	}
	key := t.sessionLeaseKey.Load()
	if key == nil {
		return ""
	}
	return *key
}

// releaseSessionLeaseForKey releases the tab's lease only when it is bound to
// key. Superseded builds clean up with this instead of releaseSessionLease:
// on a removed tab the keys match and the lease is released as before, but
// when a session rebind superseded the build, the rebind's replacement build
// holds a lease for a *different* session key (rebind early-returns on equal
// keys), and releasing that here would strip the live session's protection.
func (t *WorkspaceTab) releaseSessionLeaseForKey(key string) {
	if t == nil || key == "" {
		return
	}
	t.sessionLeaseMu.Lock()
	lease := t.sessionLease
	if lease == nil || sessionRuntimeKey(lease.Path()) != key {
		t.sessionLeaseMu.Unlock()
		return
	}
	t.sessionLease = nil
	t.storeSessionLeaseRuntimeKey("")
	t.sessionLeaseMu.Unlock()
	lease.Release()
}

func detachedRuntimeTabID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "detached_" + hex.EncodeToString(sum[:8])
}

func (a *App) ensureDetachedSessionsLocked() {
	if a.detachedSessions == nil {
		a.detachedSessions = map[string]*WorkspaceTab{}
	}
}

func (a *App) runtimeTabsLocked() []*WorkspaceTab {
	seen := map[*WorkspaceTab]bool{}
	out := make([]*WorkspaceTab, 0, len(a.tabs)+len(a.detachedSessions))
	for _, tab := range a.tabs {
		if tab != nil && !seen[tab] {
			seen[tab] = true
			out = append(out, tab)
		}
	}
	for _, tab := range a.detachedSessions {
		if tab != nil && !seen[tab] {
			seen[tab] = true
			out = append(out, tab)
		}
	}
	return out
}

func (a *App) tabByEventSinkIDLocked(tabID string) *WorkspaceTab {
	if tab := a.tabs[tabID]; tab != nil {
		return tab
	}
	for _, tab := range a.detachedSessions {
		if tab != nil && tab.ID == tabID {
			return tab
		}
	}
	return nil
}

func (a *App) detachSessionRuntime(tab *WorkspaceTab) bool {
	if tab == nil {
		return false
	}
	a.mu.RLock()
	ctrl := tab.Ctrl
	fallbackPath := strings.TrimSpace(tab.SessionPath)
	sink := tab.sink
	a.mu.RUnlock()
	path := fallbackPath
	if ctrl != nil {
		if p := strings.TrimSpace(ctrl.SessionPath()); p != "" {
			path = p
		}
	}
	key := sessionRuntimeKey(path)
	if key == "" {
		return false
	}
	if sink != nil {
		sink.clearContext()
	}
	a.mu.Lock()
	a.ensureDetachedSessionsLocked()
	tab.SessionPath = canonicalTabSessionPath(path)
	a.bindSessionRuntimeKeyLocked(tab, path)
	a.detachedSessions[key] = tab
	a.mu.Unlock()
	return true
}

// cloneDetachedRuntimeTab copies a running tab's runtime state into a fresh
// detached tab. Callers must hold a.mu: the copied fields (Ctrl, Ready,
// ActivityStatus, disabledMCP, ...) are written under a.mu by bound methods
// and the event sink, and the disabledMCP map read would otherwise race those
// writers. The session lease is transferred separately by the caller through
// the sessionLeaseMu helpers. key is the runtime identity (map key / tab id
// hash); path is the real session path — keys are case-folded on Windows and
// must not leak into SessionPath, which is displayed and persisted.
func cloneDetachedRuntimeTab(tab *WorkspaceTab, key, path string) *WorkspaceTab {
	if tab == nil {
		return nil
	}
	tab.telemMu.Lock()
	readTelemetry := append([]readFileRecord(nil), tab.readTelemetry...)
	usageTelemetry := cloneSessionUsageStats(tab.usageTelemetry)
	telemetrySessionKey := tab.telemetrySessionKey
	tab.telemMu.Unlock()

	return &WorkspaceTab{
		ID:                  detachedRuntimeTabID(key),
		Scope:               tab.Scope,
		WorkspaceRoot:       tab.WorkspaceRoot,
		SharedHostKey:       tab.SharedHostKey,
		TopicID:             tab.TopicID,
		TopicTitle:          tab.TopicTitle,
		topicTitleSource:    tab.topicTitleSource,
		SessionPath:         canonicalTabSessionPath(path),
		Ctrl:                tab.Ctrl,
		Label:               tab.Label,
		Ready:               tab.Ready,
		StartupErr:          tab.StartupErr,
		StartupErrLeaseHeld: tab.StartupErrLeaseHeld,
		runtimeID:           tab.runtimeID,
		sink:                tab.sink,
		ActivityStatus:      tab.ActivityStatus,
		readTelemetry:       readTelemetry,
		usageTelemetry:      usageTelemetry,
		telemetrySessionKey: telemetrySessionKey,
		displayState:        tab.displayBufferState(),
		model:               tab.model,
		effort:              cloneStringPtr(tab.effort),
		tokenMode:           tab.tokenMode,
		mode:                tab.mode,
		goal:                tab.goal,
		toolApprovalMode:    tab.toolApprovalMode,
		disabledMCP:         cloneServerViewMap(tab.disabledMCP),
		mcpOrder:            append([]string(nil), tab.mcpOrder...),
	}
}

func (a *App) detachRuntimeForReplacement(tab *WorkspaceTab) bool {
	if tab == nil {
		return false
	}

	a.mu.Lock()
	detached := a.detachRuntimeForReplacementLocked(tab)
	a.mu.Unlock()
	return detached
}

// detachRuntimeForReplacementLocked transfers a visible tab's live runtime to
// the detached registry without closing its controller or releasing its lease.
// Callers must hold App.mu. The transfer itself performs no file or host I/O.
func (a *App) detachRuntimeForReplacementLocked(tab *WorkspaceTab) bool {
	if tab == nil {
		return false
	}
	if tab.removed || a.tabs[tab.ID] != tab {
		return false
	}
	sourcePath := tab.currentSessionPath()
	key := sessionRuntimeKey(sourcePath)
	if key == "" {
		return false
	}
	detached := cloneDetachedRuntimeTab(tab, key, sourcePath)
	if detached == nil {
		return false
	}

	detached.adoptSessionLease(tab.takeSessionLease())
	if rt := a.runtimeForTabLocked(tab); rt != nil {
		rt.Owner = detached
		detached.runtimeID = rt.ID
		tab.runtimeID = ""
	}
	if detached.sink != nil {
		detached.sink.setBinding(detached.ID, nil)

		detached.sink.clearContext()
	}
	a.ensureDetachedSessionsLocked()
	a.detachedSessions[key] = detached
	return true
}

// applyRuntimeTab moves source's runtime (controller, sink, lease, telemetry)
// onto target. path is the real session path for display/persistence; the
// case-folded runtime key must never be written into SessionPath.
func applyRuntimeTab(target, source *WorkspaceTab, path string, wailsCtx context.Context, app *App) {
	if target == nil || source == nil {
		return
	}
	source.telemMu.Lock()
	readTelemetry := append([]readFileRecord(nil), source.readTelemetry...)
	usageTelemetry := cloneSessionUsageStats(source.usageTelemetry)
	telemetrySessionKey := source.telemetrySessionKey
	source.telemMu.Unlock()

	target.adoptDisplayState(source.displayBufferState())
	if source.sink != nil {
		source.sink.setBinding(target.ID, app)
		source.sink.setContext(wailsCtx)
	}

	target.Ctrl = source.Ctrl
	target.sink = source.sink
	target.adoptSessionLease(source.takeSessionLease())
	target.SessionPath = canonicalTabSessionPath(path)
	target.SharedHostKey = source.SharedHostKey
	target.Label = source.Label
	target.Ready = source.Ready && source.Ctrl != nil
	clearTabStartupError(target)
	target.ActivityStatus = source.ActivityStatus
	target.model = source.model
	target.effort = cloneStringPtr(source.effort)
	target.tokenMode = source.tokenMode
	target.mode = source.mode
	target.goal = source.goal
	target.toolApprovalMode = source.toolApprovalMode
	target.disabledMCP = cloneServerViewMap(source.disabledMCP)
	target.mcpOrder = append([]string(nil), source.mcpOrder...)
	target.replaceTelemetry(tabTelemetrySnapshot{ReadFiles: readTelemetry, Usage: usageTelemetry}, telemetrySessionKey)
	if app != nil {
		key := sessionRuntimeKey(path)
		rt := app.runtimeForTabLocked(source)
		targetRuntime := app.runtimeForTabLocked(target)
		if rt == nil {
			rt = targetRuntime
		}
		if rt == nil {
			rt = app.newSessionRuntimeLocked(source, key)
		} else if targetRuntime != nil && targetRuntime != rt {
			app.removeSessionRuntimeMappingsLocked(targetRuntime)
			target.runtimeID = ""
		}
		if source.Ctrl != nil && source.Ready {
			rt.Phase = sessionRuntimeReady
			rt.Issue = nil
			closeRuntimeReadyChannelLocked(rt)
		}
		rt.Owner = target
		if rt.Key != "" && rt.Key != key && app.runtimeBySessionKey[rt.Key] == rt {
			delete(app.runtimeBySessionKey, rt.Key)
		}
		rt.Key = key
		app.runtimeBySessionKey[key] = rt
		target.runtimeID = rt.ID
		source.runtimeID = ""
		if target.sink != nil {
			target.sink.setRuntimeEpoch(rt.Epoch)
		}
	}
}

func (a *App) attachExistingSessionRuntime(tab *WorkspaceTab, path string, wailsCtx context.Context) bool {
	key := sessionRuntimeKey(path)
	if tab == nil || key == "" {
		return false
	}

	a.mu.Lock()
	if tab.removed || a.tabs[tab.ID] != tab {
		a.mu.Unlock()
		return false
	}
	if rt := a.runtimeBySessionKey[key]; rt != nil && !a.runtimeOwnerLiveLocked(rt) {
		a.removeSessionRuntimeMappingsLocked(rt)
	}
	registered := a.runtimeBySessionKey[key]
	if registered != nil && registered.Phase == sessionRuntimeStarting && registered.Owner != tab {

		a.mu.Unlock()
		return false
	}
	attachable := func(source *WorkspaceTab) bool {
		if source == nil || source.Ctrl == nil {
			return false
		}
		if rt := a.runtimeForTabLocked(source); rt != nil {
			return rt.Phase == sessionRuntimeReady
		}

		return source.Ready
	}
	detached := a.detachedSessions[key]
	if detached == nil {
		if rt := a.runtimeBySessionKey[key]; rt != nil && rt.Owner != nil && rt.Owner != tab {
			detached = rt.Owner
			if a.tabs[detached.ID] != detached {
				delete(a.detachedSessions, key)
			} else {
				detached = nil
			}
		}
	}
	if detached != nil {
		if !attachable(detached) {
			a.mu.Unlock()
			return false
		}
		delete(a.detachedSessions, key)
		applyRuntimeTab(tab, detached, path, wailsCtx, a)
		if current := a.tabs[tab.ID]; current == tab {
			a.saveTabsLocked()
		}
		attachedCtrl := tab.Ctrl
		a.mu.Unlock()
		if attachedCtrl != nil {
			attachedCtrl.ReplayPendingPrompts()
		}
		return true
	}

	var source *WorkspaceTab
	if rt := a.runtimeBySessionKey[key]; rt != nil && rt.Owner != nil && rt.Owner != tab {
		source = rt.Owner
	}
	for _, candidate := range a.tabs {
		if source != nil {
			break
		}
		if candidate == nil || candidate == tab {
			continue
		}
		if sessionRuntimeKey(candidate.currentSessionPath()) == key {
			source = candidate
			break
		}
	}
	if source == nil {
		a.mu.Unlock()
		return false
	}
	if !attachable(source) {
		a.mu.Unlock()
		return false
	}
	delete(a.tabs, source.ID)
	a.removeTabOrderLocked(source.ID)
	if a.activeTabID == source.ID {
		a.activeTabID = tab.ID
	}
	applyRuntimeTab(tab, source, path, wailsCtx, a)
	a.saveTabsLocked()
	attachedCtrl := tab.Ctrl
	a.mu.Unlock()

	if attachedCtrl != nil {
		attachedCtrl.ReplayPendingPrompts()
	}
	return true
}
