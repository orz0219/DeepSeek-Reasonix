package main

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/botruntime"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/sessioncatalog"
)

// SummarizeFrom / SummarizeUpTo compress model context after / before the start
// of a selected turn. Visible history and checkpoints remain unchanged.
func (a *App) SummarizeFrom(turn int) error {
	return a.SummarizeFromForTab("", turn)
}

func (a *App) SummarizeFromForTab(tabID string, turn int) error {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return readOnlyChannelErr()
	}
	if ctrl == nil {
		return nil
	}
	return ctrl.SummarizeFrom(a.ctx, turn)
}

func (a *App) SummarizeUpTo(turn int) error {
	return a.SummarizeUpToForTab("", turn)
}

func (a *App) SummarizeUpToForTab(tabID string, turn int) error {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return readOnlyChannelErr()
	}
	if ctrl == nil {
		return nil
	}
	return ctrl.SummarizeUpTo(a.ctx, turn)
}

type channelSessionRoute struct {
	channel       string
	channelLabel  string
	remoteID      string
	chatType      string
	userID        string
	threadID      string
	sessionSource string
}

type WorkspaceMeta struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Current bool   `json:"current"`
}

func controllerSessionDir(ctrl control.SessionAPI) string {
	if ctrl != nil {
		if dir := ctrl.SessionDir(); dir != "" {
			return dir
		}
	}
	return desktopSessionDir("")
}

func tabSessionDir(tab *WorkspaceTab) string {
	if tab != nil {
		if tab.WorkspaceRoot != "" {
			return desktopSessionDir(tab.WorkspaceRoot)
		}
		if tab.Ctrl != nil {
			if dir := tab.Ctrl.SessionDir(); dir != "" {
				return dir
			}
		}
	}
	return desktopSessionDir("")
}

func tabRuntimeSessionDir(tab *WorkspaceTab) string {
	if tab != nil && tab.Ctrl != nil {
		if dir, ok := safeControllerSessionDir(tab.Ctrl); ok && strings.TrimSpace(dir) != "" {
			if path := strings.TrimSpace(tab.currentSessionPath()); path != "" {
				if _, _, err := validateSessionPath(dir, path); err == nil {
					return dir
				}
			} else {
				return dir
			}
		}
	}
	return tabSessionDir(tab)
}

func (a *App) activeSessionDir() string {
	tab := a.activeTab()
	if path, ok := a.reconcileTabWithPinnedSessionMeta(tab); ok && strings.TrimSpace(path) != "" {
		return filepath.Dir(path)
	}
	if tab != nil && tab.Ctrl != nil {
		return tabRuntimeSessionDir(tab)
	}
	return tabSessionDir(tab)
}

// ListSessions returns the saved sessions newest-first for the history panel,
// marking the one the current conversation is writing to and attaching any
// user-chosen titles.
func (a *App) ListSessions() []SessionMeta {
	dir := a.activeSessionDir()
	return a.listSessionsFromDir(dir, a.activeSessionPath(dir))
}

// ListSessionsForTab returns sessions from the directory owned by tabID. Task
// Monitor uses this stable target after asynchronous control lookups so a tab
// switch cannot redirect the eventual session lookup to another workspace.
func (a *App) ListSessionsForTab(tabID string) []SessionMeta {
	target, err := a.taskMonitorTargetForTab(tabID)
	if err != nil {
		return []SessionMeta{}
	}
	return a.listSessionsFromDir(target.sessionDir, target.sessionPath)
}

func (a *App) listSessionsFromDir(dir, active string) []SessionMeta {
	catalog := a.sessionCatalog.Load()
	if catalog == nil {
		return []SessionMeta{}
	}
	target := sessioncatalog.DirectoryTarget{Path: dir, Scope: "global"}
	for _, candidate := range a.sessionCatalogTargets() {
		if sameProjectRoot(candidate.Path, dir) {
			target = candidate
			break
		}
	}
	records, err := listCatalogSessionsForDirectory(a.bootContext(), catalog, target, dir)
	if err != nil {
		return []SessionMeta{}
	}
	open := a.openSessionPaths(dir)
	channelRoutes := channelSessionRoutesForDir(dir)
	out := make([]SessionMeta, 0, len(records))
	for _, record := range records {
		_, isOpen := open[record.Path]
		meta := sessionMetaFromCatalog(record, record.Path == active, isOpen)
		if route, ok := channelRoutes[sessionRuntimeKey(record.Path)]; ok {
			applyChannelSessionRoute(&meta, route)
		}
		out = append(out, meta)
	}
	return out
}

// ListTrashedSessions returns sessions that were moved to the local trash,
// newest-deleted first. These can be previewed, restored, or permanently purged.
func (a *App) ListTrashedSessions() []SessionMeta {
	out := []SessionMeta{}
	for _, dir := range a.knownSessionDirs() {
		paths, err := listTrashedSessionFiles(dir)
		if err != nil {
			continue
		}
		titles := loadSessionTitles(dir)
		for _, path := range paths {
			infos, err := agent.ListSessions(filepath.Dir(path))
			if err != nil || len(infos) == 0 {
				continue
			}
			deletedAt := trashedSessionDeletedAt(path)
			title := strings.TrimSpace(infos[0].CustomTitle)
			if title == "" {
				title = titles[filepath.Base(path)]
			}
			out = append(out, sessionMetaFromInfo(infos[0], title, false, false, deletedAt, dir))
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DeletedAt == out[j].DeletedAt {
			return out[i].LastActivityAt > out[j].LastActivityAt
		}
		return out[i].DeletedAt > out[j].DeletedAt
	})
	return out
}

func (a *App) trashedSessionDir(path string) (string, error) {
	for _, dir := range a.knownSessionDirs() {
		if _, _, _, err := validateTrashedSessionPath(dir, path); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("trashed session path outside known session dirs: %s", path)
}

func (a *App) sessionDirForPath(path string) (string, string, error) {
	for _, dir := range a.knownSessionDirs() {
		sessionPath, _, err := validateSessionPath(dir, path)
		if err == nil {
			return dir, sessionPath, nil
		}
	}
	return "", "", fmt.Errorf("session path outside known session dirs: %s", path)
}

func applyChannelSessionRoute(meta *SessionMeta, route channelSessionRoute) {
	if meta == nil {
		return
	}
	meta.Kind = "channel"
	meta.Channel = route.channel
	meta.ChannelLabel = route.channelLabel
	meta.RemoteID = route.remoteID
	meta.ChatType = route.chatType
	meta.UserID = route.userID
	meta.ThreadID = route.threadID
	meta.SessionSource = route.sessionSource
}

func channelSessionRoutesForDir(dir string) map[string]channelSessionRoute {
	userPath := config.UserConfigPath()
	if strings.TrimSpace(userPath) == "" {
		return nil
	}
	cfg := config.LoadForEdit(userPath)
	out := map[string]channelSessionRoute{}
	for _, conn := range cfg.Bot.Connections {
		channel := strings.TrimSpace(conn.Provider)
		if channel == "" {
			continue
		}
		channelLabel := strings.TrimSpace(conn.Label)
		if channelLabel == "" {
			channelLabel = channelDisplayName(channel, conn.Domain)
		}
		for _, mapping := range conn.SessionMappings {
			if strings.TrimSpace(mapping.SessionSource) != "auto" {
				continue
			}
			sessionPath := botSessionPathTarget(mapping.SessionID)
			if sessionPath == "" {
				continue
			}
			validPath, _, err := validateSessionPath(dir, sessionPath)
			if err != nil {
				continue
			}
			key := sessionRuntimeKey(validPath)
			if key == "" {
				continue
			}
			out[key] = channelSessionRoute{
				channel:       channel,
				channelLabel:  channelLabel,
				remoteID:      strings.TrimSpace(mapping.RemoteID),
				chatType:      strings.TrimSpace(mapping.ChatType),
				userID:        strings.TrimSpace(mapping.UserID),
				threadID:      strings.TrimSpace(mapping.ThreadID),
				sessionSource: strings.TrimSpace(mapping.SessionSource),
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func botSessionPathTarget(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(sessionID), "path:") {
		return strings.TrimSpace(sessionID[5:])
	}
	if strings.HasSuffix(sessionID, ".jsonl") || strings.Contains(sessionID, "/") || strings.Contains(sessionID, `\`) || strings.HasPrefix(sessionID, "~") {
		return sessionID
	}
	return ""
}

func channelDisplayName(provider, domain string) string {
	provider = strings.TrimSpace(provider)
	domain = strings.TrimSpace(domain)
	switch provider {
	case "feishu":
		if strings.EqualFold(domain, "lark") {
			return "Lark"
		}
		return "Feishu"
	case "weixin":
		return "WeChat"
	case "qq":
		return "QQ"
	default:
		return provider
	}
}

// DeleteSession moves a saved session to the local trash. If the session still
// has an in-process runtime, the runtime is cancelled and removed first so
// autosave cannot recreate or append to the deleted file later.
func (a *App) DeleteSession(path string) error {
	return friendlySessionFileError(a.deleteSession(path))
}

// DeleteRecoveryCopy is the guarded bulk-cleanup path. The frontend's copy
// marker is only a hint. Open copies are preserved, and the backend holds both
// parent and branch removal guards while re-proving coverage and publishing a
// recoverable trash entry.
func (a *App) DeleteRecoveryCopy(path string) error {
	return friendlySessionFileError(a.deleteRecoveryCopy(path))
}

var errRecoveryCopyNotRedundant = errors.New("recovery session contains content not preserved by its parent")

func (a *App) deleteSession(path string) error {
	dir := a.activeSessionDir()
	sessionPath, key, err := validateSessionPath(dir, path)
	if err != nil {
		var foundErr error
		if dir, sessionPath, foundErr = a.sessionDirForPath(path); foundErr != nil {
			return err
		}
		key = filepath.Base(sessionPath)
	}
	if err := validateSessionTrashTarget(dir, sessionPath, key); err != nil {
		return err
	}
	var fallback fallbackRuntimeTarget
	if err := func() error {
		defer a.lockRuntimeMutation("delete-session")()
		a.sessionRemovalMu.Lock()
		defer a.sessionRemovalMu.Unlock()
		removed, nextFallback := a.removeSessionRuntimeBindings(dir, sessionPath)
		fallback = nextFallback
		if err := a.prepareRemovedSessionRuntimes(removed); err != nil {
			a.closeRemainingRemovedSessionRuntimesAdmissionHeld(removed, map[control.SessionAPI]bool{})
			return err
		}
		closedRemoved := map[control.SessionAPI]bool{}
		destroys := a.destroyHandlesForSession(dir, sessionPath, removed)
		teardownTimedOut := waitDestroyHandles(destroys)
		a.closeRemovedSessionRuntimesForSessionAfterDestroyAdmissionHeld(removed, dir, sessionPath, closedRemoved)
		if teardownTimedOut {
			if err := agent.MarkCleanupPending(sessionPath, "delete"); err != nil {
				a.closeRemainingRemovedSessionRuntimesAfterDestroyAdmissionHeld(removed, closedRemoved)
				return err
			}
			go delayedDesktopSessionTrash(dir, sessionPath, key, destroys)
		} else {
			err = trashSessionArtifacts(dir, sessionPath, key)
			finishDestroyHandles(destroys)
			if err != nil {
				a.closeRemainingRemovedSessionRuntimesAfterDestroyAdmissionHeld(removed, closedRemoved)
				return err
			}
		}
		a.closeRemainingRemovedSessionRuntimesAfterDestroyAdmissionHeld(removed, closedRemoved)
		return nil
	}(); err != nil {
		return err
	}
	if err := botruntime.ForgetAutoSessionMappingsForPath(sessionPath); err != nil {
		slog.Warn("desktop: failed to clear auto bot session mapping", "err", err)
	}
	if fallback.needs {
		fallback = a.sessionDeleteFallbackTarget(fallback)
		if err := a.openFallbackRuntime(fallback); err != nil {
			return err
		}
	}
	a.removeSessionCatalogPath(sessionPath, "session_deleted")
	a.emitProjectTreeChangedForSessionDirs(dir)
	a.invalidatePromptHistoryCache()
	return nil
}

type removedSessionRuntime struct {
	tab           *WorkspaceTab
	ctrl          control.SessionAPI
	sink          *tabEventSink
	sessionDir    string
	sessionPath   string
	scope         string
	workspaceRoot string
	topicID       string
	readOnly      bool
}

type fallbackRuntimeTarget struct {
	needs         bool
	scope         string
	workspaceRoot string
	topicID       string
}

func (a *App) removeSessionRuntimeBindings(dir, sessionPath string) ([]removedSessionRuntime, fallbackRuntimeTarget) {
	var removed []removedSessionRuntime
	var fallback fallbackRuntimeTarget

	a.mu.Lock()
	for id, tab := range a.tabs {
		if !tabMatchesSession(tab, dir, sessionPath) {
			continue
		}
		if len(removed) == 0 {
			fallback = fallbackRuntimeTarget{scope: tab.Scope, workspaceRoot: tab.WorkspaceRoot, topicID: tab.TopicID}
		}
		removed = append(removed, removedRuntimeFromTab(tab, dir, sessionPath))
		a.markTabRemovedLocked(tab)
		delete(a.tabs, id)
		a.removeTabOrderLocked(id)
		if a.activeTabID == id {
			a.activeTabID = ""
		}
	}
	for key, tab := range a.detachedSessions {
		if !tabMatchesSession(tab, dir, sessionPath) {
			continue
		}
		if len(removed) == 0 {
			fallback = fallbackRuntimeTarget{scope: tab.Scope, workspaceRoot: tab.WorkspaceRoot, topicID: tab.TopicID}
		}
		removed = append(removed, removedRuntimeFromTab(tab, dir, sessionPath))
		a.markTabRemovedLocked(tab)
		delete(a.detachedSessions, key)
	}
	if a.activeTabID == "" && len(a.tabOrder) > 0 {
		a.activeTabID = a.tabOrder[0]
	}
	fallback.needs = len(removed) > 0 && len(a.tabs) == 0
	dir, entries, activeID, version := a.saveTabsCollectLocked()
	a.mu.Unlock()

	a.saveTabsWrite(dir, entries, activeID, version)

	return removed, fallback
}

func (a *App) sessionDeleteFallbackTarget(target fallbackRuntimeTarget) fallbackRuntimeTarget {
	topicID := strings.TrimSpace(target.topicID)
	if topicID == "" {
		return target
	}
	if path, _ := a.findTopicContentSessionForTarget(target.scope, target.workspaceRoot, topicID); path != "" {
		return target
	}
	target.topicID = ""
	return target
}

func removedRuntimeFromTab(tab *WorkspaceTab, dir, sessionPath string) removedSessionRuntime {
	return removedSessionRuntime{
		tab:           tab,
		ctrl:          tab.Ctrl,
		sink:          tab.sink,
		sessionDir:    dir,
		sessionPath:   sessionPath,
		scope:         tab.Scope,
		workspaceRoot: tab.WorkspaceRoot,
		topicID:       tab.TopicID,
		readOnly:      tab.ReadOnly,
	}
}

func tabMatchesSession(tab *WorkspaceTab, dir, sessionPath string) bool {
	if tab == nil {
		return false
	}
	currentPath, _, err := validateSessionPath(dir, tab.currentSessionPath())
	if err == nil && currentPath == sessionPath {
		return true
	}
	if tabRuntimeSessionDir(tab) != dir {
		return false
	}
	currentPath, _, err = validateSessionPath(dir, tab.currentSessionPath())
	return err == nil && currentPath == sessionPath
}

func (a *App) prepareRemovedSessionRuntimes(removed []removedSessionRuntime) error {
	for _, item := range removed {
		if item.sink != nil {
			item.sink.clearContext()
		}
		if item.ctrl == nil {
			continue
		}
		if item.ctrl.Running() {
			item.ctrl.Cancel()
			if err := waitControllerStopped(item.ctrl); err != nil {
				return err
			}
		}
		if item.readOnly {
			continue
		}
		if err := item.ctrl.Snapshot(); err != nil {
			if !errors.Is(err, agent.ErrSessionSnapshotConflict) {
				return err
			}
			slog.Warn("desktop: skipping stale runtime snapshot before removing session",
				"session", item.sessionPath, "err", err)
		}
		item.ctrl.SetSessionPath("")
		a.quiesceTabAutosave(item.tab)
	}
	return nil
}

func waitControllerStopped(ctrl control.SessionAPI) error {
	deadline := time.Now().Add(5 * time.Second)
	for ctrl.Running() {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for cancelled session work to stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func (a *App) destroyHandlesForSession(dir, sessionPath string, removed []removedSessionRuntime) []control.SessionDestroyHandle {
	destroys := a.beginDestroySessionJobs(dir, sessionPath)
	for _, item := range removed {
		if item.ctrl == nil || item.sessionDir != dir || item.sessionPath != sessionPath {
			continue
		}
		destroys = append(destroys, item.ctrl.BeginDestroySession(sessionPath))
	}
	return destroys
}

func waitAllDestroyHandles(destroys []control.SessionDestroyHandle) {
	for _, destroy := range destroys {
		if destroy.WaitAll != nil {
			destroy.WaitAll()
		}
	}
}

func finishDestroyHandles(destroys []control.SessionDestroyHandle) {
	for _, destroy := range destroys {
		if destroy.Finish != nil {
			destroy.Finish()
		}
	}
}

func delayedDesktopSessionCleanup(path string, destroys []control.SessionDestroyHandle) {
	waitAllDestroyHandles(destroys)
	if err := removeDesktopSessionArtifacts(path); err != nil {
		slog.Warn("desktop: delayed session cleanup failed", "path", path, "err", err)
	}
	finishDestroyHandles(destroys)
}

func delayedDesktopSessionTrash(dir, sessionPath, key string, destroys []control.SessionDestroyHandle) {
	waitAllDestroyHandles(destroys)
	if err := trashSessionArtifacts(dir, sessionPath, key); err != nil {
		slog.Warn("desktop: delayed session trash failed", "path", sessionPath, "err", err)
	}
	finishDestroyHandles(destroys)
}

func (a *App) closeRemovedSessionRuntimes(removed []removedSessionRuntime) {
	defer a.lockRuntimeMutation("close-removed-session-runtimes")()
	a.closeRemainingRemovedSessionRuntimesAdmissionHeld(removed, map[control.SessionAPI]bool{})
}

func (a *App) closeRemovedSessionRuntimesForSessionAfterDestroyAdmissionHeld(removed []removedSessionRuntime, dir, sessionPath string, closed map[control.SessionAPI]bool) {
	releasedTabs := map[*WorkspaceTab]bool{}
	for _, item := range removed {
		if item.sessionDir != dir || item.sessionPath != sessionPath {
			continue
		}
		a.closeRemovedSessionRuntime(item, closed, releasedTabs, true)
	}
}

func (a *App) closeRemainingRemovedSessionRuntimesAdmissionHeld(removed []removedSessionRuntime, closed map[control.SessionAPI]bool) {
	releasedTabs := map[*WorkspaceTab]bool{}
	for _, item := range removed {
		a.closeRemovedSessionRuntime(item, closed, releasedTabs, false)
	}
}

func (a *App) closeRemainingRemovedSessionRuntimesAfterDestroyAdmissionHeld(removed []removedSessionRuntime, closed map[control.SessionAPI]bool) {
	releasedTabs := map[*WorkspaceTab]bool{}
	for _, item := range removed {
		a.closeRemovedSessionRuntime(item, closed, releasedTabs, true)
	}
}

func (a *App) closeRemovedSessionRuntime(item removedSessionRuntime, closed map[control.SessionAPI]bool, releasedTabs map[*WorkspaceTab]bool, afterDestroy bool) {
	if item.tab != nil {
		if releasedTabs == nil || !releasedTabs[item.tab] {
			if releasedTabs != nil {
				releasedTabs[item.tab] = true
			}
			a.releaseTabSharedHost(item.tab)
			item.tab.releaseSessionLease()
		}
	}
	if item.ctrl == nil {
		return
	}
	if closed == nil {
		closed = map[control.SessionAPI]bool{}
	}
	if closed[item.ctrl] {
		return
	}
	closed[item.ctrl] = true
	if afterDestroy {
		item.ctrl.CloseAfterDestroy()
		return
	}
	item.ctrl.Close()
}

func (a *App) openFallbackRuntime(target fallbackRuntimeTarget) error {
	scope := target.scope
	root := target.workspaceRoot
	topicID := strings.TrimSpace(target.topicID)
	if scope == "global" {
		root = ""
	}
	if topicID == "" {
		return a.openTransientBlankRuntime(scope, root)
	}
	var err error
	if a.singleSurfaceLayoutEnabled() {
		_, err = a.ActivateTopic(scope, root, topicID, "")
	} else if scope == "global" {
		_, err = a.OpenGlobalTab(topicID)
	} else {
		_, err = a.OpenProjectTab(root, topicID)
	}
	return err
}
