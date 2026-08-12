package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/store"
	"sort"
	"strings"
	"sync"
	"time"
)

// saveTabSessionMeta persists the tab's scope/topic/mode fields into the
// session's branch-meta sidecar at path. Tab fields are snapshotted under a.mu
// (controller reads happen off-lock) so a concurrent tab mutation can't tear
// the persisted record.
func (a *App) saveTabSessionMeta(tab *WorkspaceTab, path string) error {
	if tab == nil || strings.TrimSpace(path) == "" {
		return nil
	}
	a.mu.RLock()
	ctrl := tab.Ctrl
	snap := tabSessionMetaSnapshot{
		path:             path,
		scope:            tab.Scope,
		workspaceRoot:    tab.WorkspaceRoot,
		topicID:          tab.TopicID,
		topicTitle:       tab.TopicTitle,
		tokenMode:        boot.NormalizeTokenMode(tab.tokenMode),
		mode:             normalizeTabMode(tab.mode),
		toolApprovalMode: normalizeToolApprovalMode(tab.toolApprovalMode),
		goal:             strings.TrimSpace(tab.goal),
	}
	a.mu.RUnlock()
	if ctrl != nil {
		snap.mode = tabModeFromAxes(ctrl.PlanMode(), ctrl.AutoApproveTools())
		snap.toolApprovalMode = normalizeToolApprovalMode(ctrl.ToolApprovalMode())
		if goal := strings.TrimSpace(ctrl.Goal()); goal != "" && ctrl.GoalStatus() == control.GoalStatusRunning {
			snap.goal = goal
		} else {
			snap.goal = ""
		}
	}
	return saveTabSessionMetaSnapshot(snap)
}

type tabSessionMetaSnapshot struct {
	path             string
	scope            string
	workspaceRoot    string
	topicID          string
	topicTitle       string
	tokenMode        string
	mode             string
	toolApprovalMode string
	goal             string
}

func (a *App) saveTabSessionMetaForCurrentSession(tab *WorkspaceTab) error {
	snap, ok := a.tabSessionMetaSnapshotForCurrentSession(tab)
	if !ok {
		return nil
	}
	return saveTabSessionMetaSnapshot(snap)
}

func (a *App) tabSessionMetaSnapshotForCurrentSession(tab *WorkspaceTab) (tabSessionMetaSnapshot, bool) {
	if tab == nil {
		return tabSessionMetaSnapshot{}, false
	}
	a.mu.RLock()
	if tab.ID != "" && a.tabs[tab.ID] != tab {
		a.mu.RUnlock()
		return tabSessionMetaSnapshot{}, false
	}
	readOnly := tab.ReadOnly
	ctrl := tab.Ctrl
	storedPath := strings.TrimSpace(tab.SessionPath)
	scope := tab.Scope
	workspaceRoot := tab.WorkspaceRoot
	topicID := tab.TopicID
	topicTitle := tab.TopicTitle
	tokenMode := boot.NormalizeTokenMode(tab.tokenMode)
	mode := normalizeTabMode(tab.mode)
	toolApprovalMode := normalizeToolApprovalMode(tab.toolApprovalMode)
	goal := strings.TrimSpace(tab.goal)
	a.mu.RUnlock()
	if readOnly {
		return tabSessionMetaSnapshot{}, false
	}

	ctrlPath := ""
	ctrlDir := ""
	activeWork := false
	if ctrl != nil {
		ctrlPath = strings.TrimSpace(ctrl.SessionPath())
		if dir, ok := safeControllerSessionDir(ctrl); ok {
			ctrlDir = strings.TrimSpace(dir)
		}
		status := ctrl.RuntimeStatus()
		activeWork = status.Running || status.PendingPrompt || status.BackgroundJobs > 0
		mode = tabModeFromAxes(ctrl.PlanMode(), ctrl.AutoApproveTools())
		toolApprovalMode = normalizeToolApprovalMode(ctrl.ToolApprovalMode())
		if ctrl.GoalStatus() == control.GoalStatusRunning {
			goal = strings.TrimSpace(ctrl.Goal())
		} else {
			goal = ""
		}
	}

	currentPath := ctrlPath
	if currentPath == "" {
		currentPath = storedPath
	}
	if currentPath == "" {
		return tabSessionMetaSnapshot{}, false
	}

	sessionDir := desktopSessionDir("")
	if workspaceRoot != "" {
		sessionDir = desktopSessionDir(workspaceRoot)
	} else if ctrlDir != "" {
		sessionDir = ctrlDir
	}
	runtimeDir := sessionDir
	if ctrlDir != "" {
		if _, _, err := validateSessionPath(ctrlDir, currentPath); err == nil {
			runtimeDir = ctrlDir
		}
	}
	if topicID == "" && !activeWork && storedPath != "" && sessionPathHasNoContent(sessionDir, storedPath) {
		return tabSessionMetaSnapshot{}, false
	}
	path := tabSessionMetaPathForSession(runtimeDir, sessionDir, currentPath)
	if path == "" {
		return tabSessionMetaSnapshot{}, false
	}
	return tabSessionMetaSnapshot{
		path:             path,
		scope:            scope,
		workspaceRoot:    workspaceRoot,
		topicID:          topicID,
		topicTitle:       topicTitle,
		tokenMode:        tokenMode,
		mode:             mode,
		toolApprovalMode: toolApprovalMode,
		goal:             goal,
	}, true
}

func saveTabSessionMetaSnapshot(snap tabSessionMetaSnapshot) error {
	if strings.TrimSpace(snap.path) == "" {
		return nil
	}

	unlock, err := agent.LockSessionMetaPath(snap.path)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := agent.EnsureBranchMetaLocked(snap.path)
	if err != nil {
		return err
	}
	scope := snap.scope
	workspaceRoot := snap.workspaceRoot
	if ownerScope, ownerRoot, _, ok := legacyMigrationTargetForDir(filepath.Dir(snap.path)); ok {
		if ownerScope == "project" {
			scope = ownerScope
			workspaceRoot = ownerRoot
		}
	}
	if scope == "project" {
		workspaceRoot = normalizeProjectRoot(workspaceRoot)
	} else {
		scope = "global"
		workspaceRoot = ""
	}
	m.Scope = scope
	m.WorkspaceRoot = workspaceRoot
	m.TopicID = snap.topicID
	m.TopicTitle = snap.topicTitle
	m.TokenMode = persistedTabTokenMode(snap.tokenMode)
	m.AgentPreset = boot.NormalizeAgentPreset(snap.tokenMode)
	m.Mode = persistedTabMode(snap.mode)
	m.ToolApprovalMode = persistedToolApprovalMode(snap.toolApprovalMode)
	m.Goal = strings.TrimSpace(snap.goal)
	if err := agent.SaveBranchMetaPreserveUpdatedLocked(snap.path, m); err != nil {
		return err
	}
	invalidateTopicSessionIndexForPath(snap.path)
	return nil
}

func tabSessionMetaPathForSession(runtimeDir, sessionDir, sessionPath string) string {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" {
		return ""
	}
	for _, dir := range []string{runtimeDir, sessionDir} {
		if resolved, ok := pinnedTabSessionPath(dir, sessionPath); ok {
			return resolved
		}
	}
	path := canonicalTabSessionPath(sessionPath)
	if filepath.IsAbs(path) {
		return path
	}
	return ""
}

type tabSessionProfile struct {
	tokenMode        string
	mode             string
	toolApprovalMode string
	goal             string
}

func defaultTabSessionProfile() tabSessionProfile {
	return tabSessionProfile{
		tokenMode:        boot.TokenModeFull,
		mode:             "normal",
		toolApprovalMode: control.ToolApprovalAsk,
	}
}

func tabSessionProfileFromMeta(sessionPath string, meta agent.BranchMeta) tabSessionProfile {
	profile := defaultTabSessionProfile()

	if strings.TrimSpace(meta.AgentPreset) != "" {
		profile.tokenMode = boot.TokenModeFromAgentPreset(meta.AgentPreset)
	} else {
		profile.tokenMode = boot.NormalizeTokenMode(meta.TokenMode)
	}
	profile.mode = normalizeTabMode(meta.Mode)
	profile.toolApprovalMode = normalizeToolApprovalMode(meta.ToolApprovalMode)
	if profile.toolApprovalMode == control.ToolApprovalAsk && tabModeHasAutoApproveTools(meta.Mode) {
		profile.toolApprovalMode = control.ToolApprovalYolo
	}
	profile.goal = runningTabSessionGoal(sessionPath, meta.Goal)
	return profile
}

func loadTabSessionProfile(sessionPath string) tabSessionProfile {
	meta, ok, err := agent.LoadBranchMeta(sessionPath)
	if err != nil || !ok {
		return defaultTabSessionProfile()
	}
	return tabSessionProfileFromMeta(sessionPath, meta)
}

func applyTabSessionProfile(tab *WorkspaceTab, profile tabSessionProfile) {
	if tab == nil {
		return
	}
	tab.tokenMode = boot.NormalizeTokenMode(profile.tokenMode)
	tab.mode = normalizeTabMode(profile.mode)
	tab.toolApprovalMode = normalizeToolApprovalMode(profile.toolApprovalMode)
	if tab.toolApprovalMode == control.ToolApprovalAsk && tabModeHasAutoApproveTools(tab.mode) {
		tab.toolApprovalMode = control.ToolApprovalYolo
	}
	tab.mode = tabModeFromAxes(tabModeHasPlan(tab.mode), tab.toolApprovalMode == control.ToolApprovalYolo)
	tab.goal = strings.TrimSpace(profile.goal)
}

func persistedTabGoal(tab *WorkspaceTab) string {
	goal := strings.TrimSpace(currentTabGoal(tab))
	if goal == "" || currentTabGoalStatus(tab) != control.GoalStatusRunning {
		return ""
	}
	return goal
}

type tabSessionGoalState struct {
	Goal   string `json:"goal,omitempty"`
	Status string `json:"status,omitempty"`
}

func runningTabSessionGoal(sessionPath, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if fallback == "" {
		return ""
	}
	data, err := readFileUTF8(store.SessionGoalState(sessionPath))
	if err != nil {
		return fallback
	}
	var state tabSessionGoalState
	if err := json.Unmarshal(data, &state); err != nil {
		return fallback
	}
	switch state.Status {
	case control.GoalStatusRunning:
		if goal := strings.TrimSpace(state.Goal); goal != "" {
			return goal
		}
		return fallback
	case "", control.GoalStatusStopped:
		return ""
	default:
		return ""
	}
}

func canonicalTabSessionPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if validPath, _, err := validateSessionPath(config.SessionDir(), path); err == nil {
		return validPath
	}

	cleaned := filepath.Clean(path)
	if abs, err := filepath.Abs(cleaned); err == nil {
		return abs
	}
	return cleaned
}

func (a *App) rememberTabSessionPath(tab *WorkspaceTab, path string) {
	path = canonicalTabSessionPath(path)
	if tab == nil || path == "" {
		return
	}
	a.mu.Lock()
	if current := a.tabs[tab.ID]; current == tab {
		tab.SessionPath = path
		a.saveTabsLocked()
	} else {
		tab.SessionPath = path
	}
	a.mu.Unlock()
}

func (a *App) persistTabSessionPath(tab *WorkspaceTab, path string) {
	path = canonicalTabSessionPath(path)
	if tab == nil || path == "" {
		return
	}
	if reconciled, ok := a.reconcileTabWithSessionPath(tab, path); ok {
		path = canonicalTabSessionPath(reconciled)
	}
	_ = a.saveTabSessionMeta(tab, path)
	a.rememberTabSessionPath(tab, path)
}

func (a *App) knownSessionDirs() []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(dir string) {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			return
		}
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
		if seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir)
	}
	add(config.SessionDir())
	add(desktopSessionDir(globalWorkspaceRoot()))
	for _, project := range loadProjectsFile().Projects {
		dir := desktopSessionDir(project.Root)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		add(dir)
	}
	a.mu.RLock()
	for _, tab := range a.tabs {
		add(tabSessionDir(tab))
	}
	for _, tab := range a.detachedSessions {
		add(tabSessionDir(tab))
	}
	a.mu.RUnlock()
	return out
}

func topicSessionMatchMatchesTarget(match topicSessionMatch, scope, workspaceRoot string) bool {
	if scope == "project" {
		return match.scope == "project" && sameProjectRoot(match.workspaceRoot, workspaceRoot)
	}
	return match.scope == "" || match.scope == "global"
}

func (a *App) findTopicSessionForTarget(scope, workspaceRoot, topicID string) (string, string) {
	return a.findTopicSessionForTargetByContent(scope, workspaceRoot, topicID, false)
}

func (a *App) findTopicContentSessionForTarget(scope, workspaceRoot, topicID string) (string, string) {
	return a.findTopicSessionForTargetByContent(scope, workspaceRoot, topicID, true)
}

func (a *App) findTopicSessionForTargetByContent(scope, workspaceRoot, topicID string, requireContent bool) (string, string) {
	topicID = strings.TrimSpace(topicID)
	if topicID == "" {
		return "", ""
	}
	type candidate struct {
		match topicSessionMatch
		dir   string
	}
	var candidates []candidate
	for _, dir := range a.knownSessionDirs() {
		for _, match := range topicSessionMatches(dir, topicID) {
			if !topicSessionMatchMatchesTarget(match, scope, workspaceRoot) {
				continue
			}
			candidates = append(candidates, candidate{match: match, dir: dir})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i].match, candidates[j].match
		if !a.updatedAt.Equal(b.updatedAt) {
			return a.updatedAt.After(b.updatedAt)
		}
		return a.path < b.path
	})

	for _, c := range candidates {
		if sessionFileHasConversationContent(c.match.path) {
			return c.match.path, c.dir
		}
	}
	if requireContent || len(candidates) == 0 {
		return "", ""
	}
	return candidates[0].match.path, candidates[0].dir
}

type topicSessionFileSignature struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

type topicSessionMatch struct {
	path          string
	updatedAt     time.Time
	scope         string
	workspaceRoot string
}

type topicSessionDirIndex struct {
	signature []topicSessionFileSignature
	byTopic   map[string][]topicSessionMatch
}

// mergeSessionInfos merges one directory's session listing into the maps used by
// ListProjectTree. The result collection loop calls it serially.
func mergeSessionInfos(dir string, infos []agent.SessionInfo, titles map[string]string, sessionInfos map[string]agent.SessionInfo, sessionTitles map[string]string, topicSummaries map[string]topicSummary) {
	for _, info := range infos {
		sessionKey := sessionRuntimeKey(info.Path)
		if sessionKey != "" {
			sessionInfos[sessionKey] = info
			title := strings.TrimSpace(info.CustomTitle)
			if title == "" {
				title = titles[filepath.Base(info.Path)]
			}
			sessionTitles[sessionKey] = title
		}
		if strings.TrimSpace(info.TopicID) == "" {
			continue
		}
		key := topicSummaryKey(info.Scope, info.WorkspaceRoot, info.TopicID)
		summary := topicSummaries[key]
		lastActivityAt := info.LastActivityAt.UnixMilli()
		if sessionInfoIsAutomaticRecovery(info) {

			if sessionInfoIsUnmodifiedRecoveryCopy(info, dir) {
				summary.hasRecoveryOnly = true
			} else {
				summary.hasAdoptedRecovery = true
				if info.Turns > summary.adoptedRecoveryTurns {
					summary.adoptedRecoveryTurns = info.Turns
				}
			}
			if lastActivityAt > summary.lastActivityAt {
				summary.lastActivityAt = lastActivityAt
			}
			topicSummaries[key] = summary
			continue
		}
		summary.hasNormalSession = true
		summary.turns += info.Turns
		if lastActivityAt > summary.lastActivityAt {
			summary.lastActivityAt = lastActivityAt
		}
		topicSummaries[key] = summary
	}
}

var topicSessionIndexCache = struct {
	sync.Mutex
	byDir map[string]topicSessionDirIndex
}{byDir: map[string]topicSessionDirIndex{}}

func topicSessionDirKey(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func topicSessionDirSnapshot(dir string) ([]topicSessionFileSignature, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	signature := []topicSessionFileSignature{}
	sessionNames := []string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		isSession := store.IsSessionTranscriptName(name)
		isMeta := strings.HasSuffix(name, ".jsonl.meta")
		if !isSession && !isMeta {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		signature = append(signature, topicSessionFileSignature{
			Name:    name,
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
		})
		if isSession {
			sessionNames = append(sessionNames, name)
		}
	}
	sort.Slice(signature, func(i, j int) bool {
		return signature[i].Name < signature[j].Name
	})
	sort.Strings(sessionNames)
	return signature, sessionNames, nil
}

func topicSessionSignaturesEqual(a, b []topicSessionFileSignature) bool {
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

func topicSessionIndexForDir(dir string) (topicSessionDirIndex, error) {
	key := topicSessionDirKey(dir)
	if key == "" {
		return topicSessionDirIndex{}, nil
	}
	signature, sessionNames, err := topicSessionDirSnapshot(key)
	if err != nil {
		if os.IsNotExist(err) {
			return topicSessionDirIndex{}, nil
		}
		return topicSessionDirIndex{}, err
	}
	topicSessionIndexCache.Lock()
	cached, ok := topicSessionIndexCache.byDir[key]
	if ok && topicSessionSignaturesEqual(cached.signature, signature) {
		topicSessionIndexCache.Unlock()
		return cached, nil
	}
	topicSessionIndexCache.Unlock()

	index := topicSessionDirIndex{
		signature: signature,
		byTopic:   map[string][]topicSessionMatch{},
	}
	for _, name := range sessionNames {
		path := filepath.Join(key, name)
		meta, ok, err := agent.LoadBranchMeta(path)
		if err != nil || !ok {
			continue
		}
		topicID := strings.TrimSpace(meta.TopicID)
		if topicID == "" {
			continue
		}
		index.byTopic[topicID] = append(index.byTopic[topicID], topicSessionMatch{
			path:          path,
			updatedAt:     meta.UpdatedAt,
			scope:         meta.DefaultScope(),
			workspaceRoot: meta.WorkspaceRoot,
		})
	}

	topicSessionIndexCache.Lock()
	topicSessionIndexCache.byDir[key] = index
	topicSessionIndexCache.Unlock()
	return index, nil
}

func topicSessionIndexHasContentTopic(index topicSessionDirIndex, topicID string) bool {
	matches := index.byTopic[strings.TrimSpace(topicID)]
	for _, match := range matches {
		if sessionFileHasConversationContent(match.path) {
			return true
		}
	}
	return false
}

// topicSessionIndexHasForeignLeaseTopic reports whether any session file
// indexed under topicID is currently lease-held by a runtime other than this
// process. A blank topic can still be lease-held — its session lease keeper
// keeps a leftover blank tab's lease alive across a hide-to-tray close, and a
// stale-but-live holder blocks a genuinely new session from ever settling on
// this path. Reusing it anyway would make the "new" tab collide with that
// holder: every lease-gated switch (effort/model/token mode) would fail as if
// a foreign window owned it, and creating another "new" conversation would
// keep re-picking the same stuck topic (#6028, #6109).
func topicSessionIndexHasForeignLeaseTopic(index topicSessionDirIndex, topicID string) bool {
	matches := index.byTopic[strings.TrimSpace(topicID)]
	for _, match := range matches {
		if agent.SessionLeaseHeldByOtherRuntime(match.path) {
			return true
		}
	}
	return false
}

func topicSessionMatches(dir, topicID string) []topicSessionMatch {
	index, err := topicSessionIndexForDir(dir)
	if err != nil {
		return nil
	}
	matches := index.byTopic[strings.TrimSpace(topicID)]
	if len(matches) == 0 {
		return nil
	}
	out := make([]topicSessionMatch, 0, len(matches))
	for _, match := range matches {
		if agent.IsCleanupPending(match.path) {
			continue
		}
		out = append(out, match)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func invalidateTopicSessionIndex(dir string) {
	key := topicSessionDirKey(dir)
	if key == "" {
		return
	}
	topicSessionIndexCache.Lock()
	delete(topicSessionIndexCache.byDir, key)
	topicSessionIndexCache.Unlock()
}

func invalidateTopicSessionIndexForPath(path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	invalidateTopicSessionIndex(filepath.Dir(path))
}

// findTopicSession returns the most recently updated .jsonl file whose .meta
// carries the given topicID, using a directory-level sidecar index cache.
func findTopicSession(dir, topicID string) string {
	if topicID == "" || dir == "" {
		return ""
	}
	var bestPath string
	var bestTime time.Time
	for _, match := range topicSessionMatches(dir, topicID) {
		if match.updatedAt.After(bestTime) {
			bestTime = match.updatedAt
			bestPath = match.path
		}
	}
	return bestPath
}
