package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"strings"
	"time"
)

func (a *App) openTopicTab(scope, workspaceRoot, topicID, sessionPath string) (TabMeta, error) {
	return a.openTopicTabWithActivation(scope, workspaceRoot, topicID, sessionPath, true)
}

func (a *App) openTopicTabWithActivation(scope, workspaceRoot, topicID, sessionPath string, activate bool) (TabMeta, error) {
	actualRoot := workspaceRoot
	if scope == "global" {
		actualRoot = globalWorkspaceRoot()
	}
	targetKey := sessionRuntimeKey(sessionPath)

	a.mu.Lock()
	if targetKey != "" {
		for _, tab := range a.tabs {
			if tab == nil {
				continue
			}
			if sessionRuntimeKey(tab.currentSessionPath()) == targetKey {
				if activate {
					a.activeTabID = tab.ID
				}
				meta := a.tabMeta(tab, tab.ID == a.activeTabID)
				a.saveTabsLocked()
				a.mu.Unlock()
				return enrichTabMeta(meta), nil
			}
		}
	}

	for _, tab := range a.tabs {
		if tabMatchesTopicTarget(tab, scope, workspaceRoot, topicID) {
			if activate {
				a.activeTabID = tab.ID
			}
			sameSession := targetKey == "" || sessionRuntimeKey(tab.currentSessionPath()) == targetKey
			meta := a.tabMeta(tab, tab.ID == a.activeTabID)
			a.saveTabsLocked()
			a.mu.Unlock()
			if sameSession {
				return enrichTabMeta(meta), nil
			}
			if err := a.rebindTabToSessionPath(tab, sessionPath); err != nil {
				return TabMeta{}, err
			}
			a.mu.RLock()
			meta = a.tabMeta(tab, tab.ID == a.activeTabID)
			a.mu.RUnlock()
			return enrichTabMeta(meta), nil
		}
	}

	tabID := a.newUniqueTabIDLocked()
	topicTitle := topicTitleForTab(scope, workspaceRoot, topicID)
	if t, source, ok := topicTitleFallbackForOpen(workspaceRoot, topicID, sessionPath); ok {
		topicTitle = t
		_ = setTopicTitleWithSource(workspaceRoot, topicID, t, source)
	}

	if sessionPath == "" {
		var err error
		sessionPath, err = createEmptySessionFile(desktopSessionDir(actualRoot), "")
		if err != nil {
			a.mu.Unlock()
			return TabMeta{}, err
		}
		if err := pinNewEmptySessionBranchMeta(sessionPath, scope, actualRoot, topicID, topicTitle); err != nil {
			a.mu.Unlock()
			return TabMeta{}, err
		}
	}
	profile := loadTabSessionProfile(sessionPath)
	tab := &WorkspaceTab{
		ID:               tabID,
		Scope:            scope,
		WorkspaceRoot:    actualRoot,
		TopicID:          topicID,
		TopicTitle:       topicTitle,
		topicTitleSource: loadTopicTitleSource(topicTitleRoot(scope, workspaceRoot), topicID),
		SessionPath:      sessionPath,
		disabledMCP:      map[string]ServerView{},
	}
	applyTabSessionProfile(tab, profile)
	tab.sink = &tabEventSink{tabID: tabID, app: a}

	a.tabs[tabID] = tab
	a.tabOrder = append(a.tabOrder, tabID)
	if activate {
		a.activeTabID = tabID
	}
	a.saveTabsLocked()
	meta := a.tabMeta(tab, tab.ID == a.activeTabID)
	a.mu.Unlock()

	a.startTabControllerBuild(tab)
	if scope == "project" {
		a.emitProjectTreeChangedForSessionDirs(sessionDirectoryForPath(sessionPath))
	}
	return enrichTabMeta(meta), nil
}

// OpenGlobalTab opens a new global-scope tab (no project root). The global
// workspace root is the reasonix user config directory.
func (a *App) OpenGlobalTab(topicID string) (TabMeta, error) {
	return a.openGlobalTab(topicID)
}

func (a *App) openGlobalTab(topicID string) (TabMeta, error) {
	globalRoot := globalWorkspaceRoot()
	if err := os.MkdirAll(globalRoot, 0o755); err != nil {
		return TabMeta{}, fmt.Errorf("create global workspace: %w", err)
	}

	sessionPath, _ := a.findTopicSessionForTarget("global", "", topicID)
	return a.openTopicTab("global", "", topicID, sessionPath)
}

// OpenTopicSession opens a concrete saved session from the sidebar. Unlike
// OpenProjectTab/OpenGlobalTab, it does not resolve the topic to the latest
// session first; sessionPath is the runtime identity being selected.
func (a *App) OpenTopicSession(scope, workspaceRoot, topicID, sessionPath string) (TabMeta, error) {
	return a.openTopicSession(scope, workspaceRoot, topicID, sessionPath)
}

func (a *App) openTopicSession(scope, workspaceRoot, topicID, sessionPath string) (TabMeta, error) {
	scope = strings.TrimSpace(scope)
	if scope != "project" {
		scope = "global"
		workspaceRoot = ""
	}
	if scope == "project" {
		workspaceRoot = normalizeProjectRoot(workspaceRoot)
		if workspaceRoot == "" {
			return TabMeta{}, fmt.Errorf("workspaceRoot is required")
		}
		saveWorkspace(workspaceRoot)
		a.registerProjectRoot(workspaceRoot)
	}
	_, validPath, err := a.sessionDirForPath(sessionPath)
	if err != nil {
		return TabMeta{}, err
	}
	return a.openTopicTab(scope, workspaceRoot, topicID, validPath)
}

// ActivateTopic opens a topic into the single visible conversation surface used
// by layouts without a tab strip. It delegates the actual open/reuse behavior to
// the classic tab path, then prunes every non-active visible tab so historical
// clicks do not accumulate hidden startup work.
//
// Interop with StartTopicActivation: a legacy ActivateTopic call supersedes any
// pending ticketed activation (its background completion becomes a no-op and a
// "cancelled" event is emitted for the old requestId), and ticketed
// activations supersede each other the same way. The synchronous return
// contract — TabMeta after the prune — is unchanged.
func (a *App) ActivateTopic(scope, workspaceRoot, topicID, sessionPath string) (TabMeta, error) {
	a.singleSurfaceMu.Lock()
	defer a.singleSurfaceMu.Unlock()

	var meta TabMeta
	var err error
	if strings.TrimSpace(sessionPath) != "" {
		meta, err = a.openTopicSession(scope, workspaceRoot, topicID, sessionPath)
	} else if strings.TrimSpace(scope) == "project" {
		meta, err = a.openProjectTab(workspaceRoot, topicID)
	} else {
		meta, err = a.openGlobalTab(topicID)
	}
	if err != nil {
		return TabMeta{}, err
	}

	if reqID, tabID := a.supersedePendingTopicActivation(meta.ID); reqID != "" {
		a.emitTopicActivation(TopicActivationEvent{RequestID: reqID, TabID: tabID, Phase: topicActivationPhaseCancelled})
	}
	return a.keepOnlyVisibleTab(meta.ID)
}

// EnsureBlankSurface mirrors EnsureBlankTab for no-tab-strip layouts: after
// creating or reusing a blank session, it removes other visible tabs while
// preserving running runtimes as detached background sessions.
func (a *App) EnsureBlankSurface(scope, workspaceRoot string) (TabMeta, error) {
	return a.ensureBlankSurface(scope, workspaceRoot, "")
}

func (a *App) ensureBlankSurface(scope, workspaceRoot, tokenMode string) (TabMeta, error) {
	a.singleSurfaceMu.Lock()
	defer a.singleSurfaceMu.Unlock()

	meta, err := a.ensureBlankTab(scope, workspaceRoot, tokenMode)
	if err != nil {
		return TabMeta{}, err
	}

	if reqID, tabID := a.supersedePendingTopicActivation(meta.ID); reqID != "" {
		a.emitTopicActivation(TopicActivationEvent{RequestID: reqID, TabID: tabID, Phase: topicActivationPhaseCancelled})
	}
	return a.keepOnlyVisibleTab(meta.ID)
}

func tabMatchesTopicTarget(tab *WorkspaceTab, scope, workspaceRoot, topicID string) bool {
	if tab == nil || tab.Scope != scope || tab.TopicID != topicID {
		return false
	}
	if scope == "global" {
		return true
	}
	return sameProjectRoot(tab.WorkspaceRoot, workspaceRoot)
}

func tabInWorkspace(tab *WorkspaceTab, workspaceRoot string) bool {
	return tab != nil &&
		tab.Scope == "project" &&
		sameProjectRoot(tab.WorkspaceRoot, workspaceRoot)
}

// EnsureBlankTab activates the existing blank tab for the target scope, or
// creates one if none exists. Reusing a blank tab keeps repeated "new session"
// clicks from piling up empty conversations.
func (a *App) EnsureBlankTab(scope, workspaceRoot string) (TabMeta, error) {
	return a.ensureBlankTab(scope, workspaceRoot, "")
}

func (a *App) ensureBlankTab(scope, workspaceRoot, forcedTokenMode string) (TabMeta, error) {
	scope = strings.TrimSpace(scope)
	if scope != "project" {
		scope = "global"
	}

	globalRoot := ""
	if scope == "project" {
		workspaceRoot = strings.TrimSpace(workspaceRoot)
		if workspaceRoot == "" {
			return TabMeta{}, fmt.Errorf("workspaceRoot is required")
		}
		if abs, err := filepath.Abs(workspaceRoot); err == nil {
			workspaceRoot = abs
		}
		saveWorkspace(workspaceRoot)
		a.registerProjectRoot(workspaceRoot)
	} else {
		workspaceRoot = ""
		globalRoot = globalWorkspaceRoot()
		if err := os.MkdirAll(globalRoot, 0o755); err != nil {
			return TabMeta{}, fmt.Errorf("create global workspace: %w", err)
		}
	}

	var created *WorkspaceTab

	actualRoot := workspaceRoot
	if scope == "global" {
		actualRoot = globalRoot
	}
	defaultModel, defaultToolApprovalMode := desktopNewSessionDefaults(scope, actualRoot)

	a.mu.Lock()
	var reusable *WorkspaceTab
	for _, id := range a.orderedTabIDsLocked() {
		tab := a.tabs[id]
		if a.blankTabMatchesTargetLocked(tab, scope, workspaceRoot) {
			if err := resetReusableBlankTabTitle(tab, scope, workspaceRoot); err != nil {
				a.mu.Unlock()
				return TabMeta{}, err
			}
			reusable = tab
			break
		}
	}
	if reusable != nil {
		a.mu.Unlock()
		if err := a.alignReusableBlankTabModel(reusable, defaultModel); err != nil {
			return TabMeta{}, err
		}
		a.mu.Lock()
		if reusable.removed || a.tabs[reusable.ID] != reusable {
			a.mu.Unlock()
			return TabMeta{}, fmt.Errorf("blank session changed while applying the default model; retry")
		}
		a.activeTabID = reusable.ID
		meta := a.tabMeta(reusable, true)
		a.saveTabsLocked()
		a.mu.Unlock()
		return enrichTabMeta(meta), nil
	}

	inheritedModel := defaultModel
	var inheritedEffort *string
	inheritedTokenMode := boot.TokenModeFull
	inheritedMode := tabModeFromAxes(false, defaultToolApprovalMode == control.ToolApprovalYolo)
	inheritedToolApprovalMode := defaultToolApprovalMode
	inheritedDisabledMCP := map[string]ServerView{}
	var inheritedMCPOrder []string
	if active := a.activeTabLocked(); active != nil {
		inheritedEffort = cloneStringPtr(active.effort)
		inheritedTokenMode = currentTabTokenMode(active)
		inheritedDisabledMCP = cloneServerViewMap(active.disabledMCP)
		inheritedMCPOrder = append([]string(nil), active.mcpOrder...)
	}
	if strings.TrimSpace(forcedTokenMode) != "" {
		inheritedTokenMode = boot.NormalizeTokenMode(forcedTokenMode)
	}

	if topicID := a.indexedBlankTopicIDLocked(scope, workspaceRoot); topicID != "" {

		if loadTopicCreatedAt(topicTitleRoot(scope, workspaceRoot), topicID) <= 0 {
			createdAt := topicIDCreatedAt(topicID)
			if createdAt <= 0 {
				createdAt = time.Now().UnixMilli()
			}
			_ = setTopicCreatedAt(topicTitleRoot(scope, workspaceRoot), topicID, createdAt)
		}
		tabID := a.newUniqueTabIDLocked()
		topicTitle := topicTitleForTab(scope, workspaceRoot, topicID)
		created = &WorkspaceTab{
			ID:               tabID,
			Scope:            scope,
			WorkspaceRoot:    actualRoot,
			TopicID:          topicID,
			TopicTitle:       topicTitle,
			topicTitleSource: loadTopicTitleSource(topicTitleRoot(scope, workspaceRoot), topicID),
			model:            inheritedModel,
			effort:           inheritedEffort,
			tokenMode:        inheritedTokenMode,
			mode:             inheritedMode,
			toolApprovalMode: inheritedToolApprovalMode,
			disabledMCP:      inheritedDisabledMCP,
			mcpOrder:         inheritedMCPOrder,
		}
		created.sink = &tabEventSink{tabID: tabID, app: a}
		a.tabs[tabID] = created
		a.tabOrder = append(a.tabOrder, tabID)
		a.activeTabID = tabID
		prePath, err := createEmptySessionFile(desktopSessionDir(actualRoot), inheritedModel)
		if err != nil {
			delete(a.tabs, tabID)
			a.removeTabOrderLocked(tabID)
			a.mu.Unlock()
			return TabMeta{}, err
		}
		if err := pinNewEmptySessionBranchMeta(prePath, scope, actualRoot, topicID, topicTitle); err != nil {
			delete(a.tabs, tabID)
			a.removeTabOrderLocked(tabID)
			a.mu.Unlock()
			return TabMeta{}, err
		}
		created.SessionPath = prePath
		a.saveTabsLocked()
		meta := a.tabMeta(created, true)
		a.mu.Unlock()

		a.startTabControllerBuild(created)
		a.emitProjectTreeChangedForSessionDirs(sessionDirectoryForPath(prePath))
		return enrichTabMeta(meta), nil
	}

	topicID := newTopicID()
	topicTitle := defaultTopicTitle
	createdAt := time.Now().UnixMilli()
	if err := setTopicTitleWithSource(workspaceRoot, topicID, topicTitle, topicTitleSourceAuto); err != nil {
		a.mu.Unlock()
		return TabMeta{}, err
	}
	if err := setTopicCreatedAt(workspaceRoot, topicID, createdAt); err != nil {
		a.mu.Unlock()
		return TabMeta{}, err
	}
	_ = prependTopicInProjectsFile(workspaceRoot, topicID, false)

	tabID := a.newUniqueTabIDLocked()
	created = &WorkspaceTab{
		ID:               tabID,
		Scope:            scope,
		WorkspaceRoot:    actualRoot,
		TopicID:          topicID,
		TopicTitle:       topicTitleForTab(scope, workspaceRoot, topicID),
		topicTitleSource: topicTitleSourceAuto,
		model:            inheritedModel,
		effort:           inheritedEffort,
		tokenMode:        inheritedTokenMode,
		mode:             inheritedMode,
		toolApprovalMode: inheritedToolApprovalMode,
		disabledMCP:      inheritedDisabledMCP,
		mcpOrder:         inheritedMCPOrder,
	}
	created.sink = &tabEventSink{tabID: tabID, app: a}
	a.tabs[tabID] = created
	a.tabOrder = append(a.tabOrder, tabID)
	a.activeTabID = tabID
	prePath, err := createEmptySessionFile(desktopSessionDir(actualRoot), inheritedModel)
	if err != nil {
		delete(a.tabs, tabID)
		a.removeTabOrderLocked(tabID)
		a.mu.Unlock()
		return TabMeta{}, err
	}
	if err := pinNewEmptySessionBranchMeta(prePath, scope, actualRoot, topicID, topicTitle); err != nil {
		delete(a.tabs, tabID)
		a.removeTabOrderLocked(tabID)
		a.mu.Unlock()
		return TabMeta{}, err
	}
	created.SessionPath = prePath
	a.saveTabsLocked()
	meta := a.tabMeta(created, true)
	a.mu.Unlock()

	a.startTabControllerBuild(created)
	a.emitProjectTreeChangedForSessionDirs(sessionDirectoryForPath(prePath))
	return enrichTabMeta(meta), nil
}

// alignReusableBlankTabModel makes a reused empty session obey the same
// provider/model default as a newly-created session. Ready runtimes use the
// normal failure-atomic model switch. A tab that is still starting has no
// controller to swap, so invalidate its startup generation, update the empty
// session's model metadata, and restart the build from the intended provider.
func (a *App) alignReusableBlankTabModel(tab *WorkspaceTab, model string) error {
	model = strings.TrimSpace(model)
	if tab == nil || model == "" {
		return nil
	}

	a.mu.RLock()
	if tab.removed || a.tabs[tab.ID] != tab {
		a.mu.RUnlock()
		return fmt.Errorf("blank session changed while applying the default model; retry")
	}
	currentModel := strings.TrimSpace(tab.model)
	ctrl := tab.Ctrl
	path := strings.TrimSpace(tab.SessionPath)
	a.mu.RUnlock()

	storedModel, hasStoredModel := agent.LoadSessionModel(path)
	storedModelChanged := path != "" && (!hasStoredModel || strings.TrimSpace(storedModel) != model)

	if ctrl != nil {
		if currentModel != model {
			if err := a.SetModelForTab(tab.ID, model); err != nil {
				return err
			}
		} else if storedModelChanged {
			return a.persistTabModelIfCurrent(tab, model)
		}
		return nil
	}

	if currentModel == model && !storedModelChanged {
		return nil
	}
	if storedModelChanged {

		if err := agent.SetBranchModelPreserveUpdated(path, model); err != nil {
			return fmt.Errorf("persist default model for blank session: %w", err)
		}
	}

	a.mu.Lock()
	if tab.removed || a.tabs[tab.ID] != tab {
		a.mu.Unlock()
		return fmt.Errorf("blank session changed while applying the default model; retry")
	}
	if tab.Ctrl != nil {
		a.mu.Unlock()
		return a.alignReusableBlankTabModel(tab, model)
	}
	a.supersedeTabBuildLocked(tab)
	tab.model = model
	tab.Label = model
	tab.Ready = false
	clearTabStartupError(tab)
	a.saveTabsLocked()
	a.mu.Unlock()
	a.startTabControllerBuild(tab)
	return nil
}

// blankTabMatchesTargetLocked returns true if tab is a reusable blank tab
// matching the given scope/project root — no running controller, no real history.
func (a *App) blankTabMatchesTargetLocked(tab *WorkspaceTab, scope, workspaceRoot string) bool {
	if tab == nil || tab.Scope != scope {
		return false
	}
	if scope == "project" && !sameProjectRoot(tab.WorkspaceRoot, workspaceRoot) {
		return false
	}
	if tab.Ctrl == nil {
		return blankTabSessionPathHasNoContent(tab)
	}
	if tab.hasActiveRuntimeWork() {
		return false
	}
	return !messagesHaveConversationContent(tab.Ctrl.History())
}

func createEmptySessionFile(dir, model string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", fmt.Errorf("session dir is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for range 3 {
		path := agent.NewSessionPath(dir, model)
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			if closeErr := f.Close(); closeErr != nil {
				return "", closeErr
			}

			_, _ = agent.EnsureBranchMeta(path)
			return path, nil
		}
		if os.IsExist(err) {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("create empty session file: exhausted filename retries")
}

func pinNewEmptySessionBranchMeta(path, scope, workspaceRoot, topicID, topicTitle string) error {
	if err := pinSessionBranchMeta(path, scope, workspaceRoot, topicID, topicTitle); err != nil {
		pinErr := fmt.Errorf("pin empty session metadata: %w", err)
		if cleanupErr := removeDesktopSessionArtifacts(path); cleanupErr != nil {
			return errors.Join(pinErr, fmt.Errorf("clean up unbound empty session: %w", cleanupErr))
		}
		return pinErr
	}
	return nil
}

// pinSessionBranchMeta stores the workspace scope, root, and topic on a newly
// created session before a controller can reconcile the tab against it.
func pinSessionBranchMeta(sessionPath, scope, workspaceRoot, topicID, topicTitle string) error {
	unlock, err := agent.LockSessionMetaPath(sessionPath)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := agent.EnsureBranchMetaLocked(sessionPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(scope) == "project" {
		workspaceRoot = normalizeProjectRoot(workspaceRoot)
		if workspaceRoot == "" {
			return fmt.Errorf("project workspace root is required")
		}
		scope = "project"
	} else {
		scope = "global"
		workspaceRoot = ""
	}
	m.Scope = scope
	m.WorkspaceRoot = workspaceRoot
	m.TopicID = topicID
	m.TopicTitle = topicTitle
	return agent.SaveBranchMetaPreserveUpdatedLocked(sessionPath, m)
}

func blankTabSessionPathHasNoContent(tab *WorkspaceTab) bool {
	if tab == nil {
		return false
	}
	if strings.TrimSpace(tab.SessionPath) == "" {
		return true
	}
	return sessionPathHasNoContent(tabSessionDir(tab), tab.SessionPath)
}

func sessionPathHasNoContent(sessionDir, sessionPath string) bool {
	if strings.TrimSpace(sessionPath) == "" {
		return true
	}
	path, ok := pinnedTabSessionPath(sessionDir, sessionPath)
	if !ok {
		return false
	}
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	if info.Size() == 0 {
		return true
	}
	session, err := agent.LoadSession(path)
	if err != nil {
		return false
	}
	return !session.HasContent()
}

func resetReusableBlankTabTitle(tab *WorkspaceTab, scope, workspaceRoot string) error {
	if tab == nil {
		return nil
	}
	topicID := strings.TrimSpace(tab.TopicID)
	if topicID == "" {
		return nil
	}
	titleRoot := topicTitleRoot(scope, workspaceRoot)
	if source := loadTopicTitleSource(titleRoot, topicID); source != topicTitleSourceAuto {
		return nil
	}
	if err := setTopicTitleWithSource(titleRoot, topicID, defaultTopicTitle, topicTitleSourceAuto); err != nil {
		return err
	}
	_ = deleteTopicAutoTitleMeta(titleRoot, topicID)
	tab.TopicTitle = defaultTopicTitle
	tab.topicTitleSource = topicTitleSourceAuto
	return nil
}

// indexedBlankTopicIDLocked finds a blank topic ID that is indexed on disk
// but not open in any tab — for reusing without creating a new topic.
func (a *App) indexedBlankTopicIDLocked(scope, workspaceRoot string) string {
	titleRoot := topicTitleRoot(scope, workspaceRoot)
	titles := loadTopicTitles(titleRoot)
	f := loadProjectsFile()

	var topicIDs []string
	if scope == "global" {
		topicIDs = orderedTopicIDs(f.GlobalTopics, titles)
	} else if i := projectIndexByRoot(f.Projects, workspaceRoot); i >= 0 {
		topicIDs = orderedTopicIDs(f.Projects[i].Topics, titles)
	}
	if len(topicIDs) == 0 {
		return ""
	}

	deletedTopics := make(map[string]bool, len(f.DeletedTopics))
	for _, id := range f.DeletedTopics {
		deletedTopics[id] = true
	}

	openTopics := map[string]bool{}
	for _, tab := range a.tabs {
		if tab == nil || tab.Scope != scope || strings.TrimSpace(tab.TopicID) == "" {
			continue
		}
		if scope == "project" && !sameProjectRoot(tab.WorkspaceRoot, workspaceRoot) {
			continue
		}
		openTopics[tab.TopicID] = true
	}
	seenSessionDirs := map[string]bool{}
	sessionIndexes := []topicSessionDirIndex{}
	addSessionIndex := func(dir string) {
		dir = cleanDesktopPath(dir)
		if dir == "" {
			return
		}
		if seenSessionDirs[dir] {
			return
		}
		seenSessionDirs[dir] = true
		if index, err := topicSessionIndexForDir(dir); err == nil {
			sessionIndexes = append(sessionIndexes, index)
		}
	}
	if scope == "project" {
		addSessionIndex(desktopSessionDir(workspaceRoot))
	} else {
		addSessionIndex(config.SessionDir())
		addSessionIndex(desktopSessionDir(globalWorkspaceRoot()))
	}
	for _, topicID := range topicIDs {
		if deletedTopics[topicID] || openTopics[topicID] {
			continue
		}
		if topicTitleForTab(scope, workspaceRoot, topicID) != defaultTopicTitle {
			continue
		}
		hasSession := false
		leaseHeld := false
		for _, index := range sessionIndexes {
			if topicSessionIndexHasContentTopic(index, topicID) {
				hasSession = true
				break
			}
			if topicSessionIndexHasForeignLeaseTopic(index, topicID) {
				leaseHeld = true
			}
		}
		if hasSession || leaseHeld {
			continue
		}
		return topicID
	}
	return ""
}
