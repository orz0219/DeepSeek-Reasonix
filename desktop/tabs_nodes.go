package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/provider"
	"strings"
	"time"
	"unicode"
)

// ProjectNode is one node in the sidebar project tree (a project folder or a
// topic leaf).
type ProjectNode struct {
	Key                          string        `json:"key"`  // stable key for React
	Kind                         string        `json:"kind"` // "project" | "topic" | "session" | "global_folder" | "global_topic" | "global_session"
	Label                        string        `json:"label"`
	Root                         string        `json:"root,omitempty"` // project workspace root
	TopicID                      string        `json:"topicId,omitempty"`
	SessionPath                  string        `json:"sessionPath,omitempty"`
	ProjectColor                 string        `json:"projectColor,omitempty"`
	Turns                        int           `json:"turns,omitempty"`
	TurnsState                   string        `json:"turnsState,omitempty"`
	Health                       string        `json:"health,omitempty"`
	CreatedAt                    int64         `json:"createdAt,omitempty"`
	LastActivityAt               int64         `json:"lastActivityAt,omitempty"`
	Open                         bool          `json:"open,omitempty"`
	Running                      bool          `json:"running,omitempty"`
	Status                       string        `json:"status,omitempty"`
	Pinned                       bool          `json:"pinned,omitempty"`
	Recovered                    bool          `json:"recovered,omitempty"`
	RecoveryReason               string        `json:"recoveryReason,omitempty"`
	RecoveryDigest               string        `json:"recoveryDigest,omitempty"`
	RecoveryParentID             string        `json:"recoveryParentId,omitempty"`
	RecoveryState                string        `json:"recoveryState,omitempty"`
	RecoveryBranchCount          int           `json:"recoveryBranchCount,omitempty"`
	RecoveryUnresolvedCount      int           `json:"recoveryUnresolvedCount,omitempty"`
	RecoveryCleanupEligibleCount int           `json:"recoveryCleanupEligibleCount,omitempty"`
	IsolatedWorktree             bool          `json:"isolatedWorktree,omitempty"`
	Children                     []ProjectNode `json:"children,omitempty"`
}

func normalizeTopicStatus(status string) string {
	switch status {
	case topicStatusThinking, topicStatusStreaming, topicStatusWaitingConfirmation, topicStatusBackgroundJob, topicStatusPaused, topicStatusError, topicStatusDivergedRecovery:
		return status
	default:
		return ""
	}
}

func legacySessionMetaMatchesMigrationTarget(meta agent.BranchMeta, scope, workspaceRoot string) bool {
	if strings.TrimSpace(meta.TopicID) != "" {
		return false
	}
	return legacySessionScopeMatchesMigrationTarget(meta, scope, workspaceRoot)
}

func legacySessionScopeMatchesMigrationTarget(meta agent.BranchMeta, scope, workspaceRoot string) bool {
	metaScope := strings.TrimSpace(meta.Scope)
	if metaScope != "" && metaScope != scope {
		return false
	}
	metaRoot := normalizeProjectRoot(meta.WorkspaceRoot)
	if scope == "project" {
		return metaRoot == "" || sameProjectRoot(workspaceRoot, metaRoot)
	}
	return metaRoot == "" || sameProjectRoot(globalWorkspaceRoot(), metaRoot)
}

func cleanDesktopPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func sameDesktopPath(a, b string) bool {
	a = cleanDesktopPath(a)
	b = cleanDesktopPath(b)
	if a == "" || b == "" {
		return false
	}
	if os.PathSeparator == '\\' {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// projectRootKey is the map-key form of a project root: cleaned, absolute,
// and case-folded on Windows — the same key form agent.CanonicalSessionPath
// uses for session paths, so equivalent spellings never split lookups.
func projectRootKey(root string) string {
	root = cleanDesktopPath(root)
	if os.PathSeparator == '\\' {
		return strings.ToLower(root)
	}
	return root
}

func restoreSessionTopicIndex(dir, sessionPath string) error {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" {
		return nil
	}
	meta, ok, err := agent.LoadBranchMeta(sessionPath)
	if err != nil {
		return err
	}
	if !ok || strings.TrimSpace(meta.TopicID) == "" {

		migrateLegacySessionsIntoGlobalTopics(dir)
		return nil
	}

	unlock, err := agent.LockSessionMetaPath(sessionPath)
	if err != nil {
		return err
	}
	defer unlock()
	meta, ok, err = agent.LoadBranchMeta(sessionPath)
	if err != nil {
		return err
	}
	if !ok || strings.TrimSpace(meta.TopicID) == "" {
		return nil
	}

	topicID := strings.TrimSpace(meta.TopicID)
	scope := strings.TrimSpace(meta.Scope)
	workspaceRoot := strings.TrimSpace(meta.WorkspaceRoot)
	if scope != "global" && scope != "project" {
		if workspaceRoot == "" {
			scope = "global"
		} else {
			scope = "project"
		}
	}
	if scope == "global" {
		workspaceRoot = ""
	} else {
		workspaceRoot = normalizeProjectRoot(workspaceRoot)
		if workspaceRoot == "" {
			scope = "global"
		}
	}

	title := restoredSessionTopicTitle(dir, sessionPath, meta)
	if title == "" {
		title = defaultTopicTitle
	}
	if err := setTopicTitleWithSource(workspaceRoot, topicID, title, topicTitleSourceManual); err != nil {
		return err
	}

	if scope == "global" {
		meta.Scope = "global"
		meta.WorkspaceRoot = ""
	} else {
		meta.Scope = "project"
		meta.WorkspaceRoot = workspaceRoot
	}
	meta.TopicID = topicID
	meta.TopicTitle = title
	if err := prependTopicInProjectsFile(workspaceRoot, topicID, scope == "project"); err != nil {
		return err
	}
	if err := agent.SaveBranchMetaPreserveUpdatedLocked(sessionPath, meta); err != nil {
		return err
	}
	invalidateTopicSessionIndexForPath(sessionPath)
	return nil
}

func restoredSessionTopicTitle(dir, sessionPath string, meta agent.BranchMeta) string {
	if title := storedSessionTopicTitle(dir, sessionPath, meta); title != "" {
		return title
	}
	if s, err := agent.LoadSession(sessionPath); err == nil {
		for _, msg := range s.Messages {
			if msg.Role == provider.RoleUser {
				if title := topicTitleFromText(agent.UserMessageText(msg)); title != "" {
					return title
				}
			}
		}
	}
	return ""
}

func storedSessionTopicTitle(dir, sessionPath string, meta agent.BranchMeta) string {
	if title := topicTitleFromText(meta.TopicTitle); title != "" {
		return title
	}
	return topicTitleFromText(loadSessionTitles(dir)[filepath.Base(sessionPath)])
}

func legacySessionTopicID(path string) string {
	id := agent.BranchID(path)
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	var b strings.Builder
	b.WriteString("legacy_")
	for _, r := range id {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	prefix := strings.TrimRight(b.String(), "_")
	if prefix == "legacy" {
		prefix = "legacy_session"
	}
	return prefix + "_" + hex.EncodeToString(sum[:])[:12]
}

// TopicMeta describes a topic for the project tree.
type TopicMeta struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"createdAt"`
}

// CreateTopic creates a new topic under a project workspace and returns its metadata.
func (a *App) CreateTopic(scope, workspaceRoot, title string) (TopicMeta, error) {
	trimmedTitle := strings.TrimSpace(title)
	titleSource := topicTitleSourceManual
	if trimmedTitle == "" {
		trimmedTitle = defaultTopicTitle
		titleSource = topicTitleSourceAuto
	}
	topicID := newTopicID()
	createdAt := time.Now().UnixMilli()
	if scope == "global" {
		workspaceRoot = ""
	}
	if workspaceRoot != "" {
		if abs, err := filepath.Abs(workspaceRoot); err == nil {
			workspaceRoot = abs
		}
	}
	if err := setTopicTitleWithSource(workspaceRoot, topicID, trimmedTitle, titleSource); err != nil {
		return TopicMeta{}, err
	}
	if err := setTopicCreatedAt(workspaceRoot, topicID, createdAt); err != nil {
		return TopicMeta{}, err
	}

	_ = prependTopicInProjectsFile(workspaceRoot, topicID, workspaceRoot != "")
	a.emitProjectTreeMetadataChanged()
	return TopicMeta{ID: topicID, Title: a.localizedTopicTitle(trimmedTitle, titleSource), CreatedAt: createdAt}, nil
}

// RenameProject updates the sidebar-only display title for a project folder.
// Empty title clears the override and falls back to the folder name.
func (a *App) RenameProject(workspaceRoot, title string) error {
	if err := renameProject(workspaceRoot, title); err != nil {
		return err
	}
	a.syncTabWorkspaceRootSpellings()
	a.emitProjectTreeMetadataChanged()
	return nil
}

// SetProjectColor updates the project-level accent color used by project topics
// in the sidebar and tabs. Empty color restores the default accent.
func (a *App) SetProjectColor(workspaceRoot, color string) error {
	if err := setProjectColor(workspaceRoot, color); err != nil {
		return err
	}
	a.syncTabWorkspaceRootSpellings()
	a.emitProjectTreeMetadataChanged()
	return nil
}

// SetProjectPinned controls whether a project folder is pinned above the rest of
// the desktop project tree.
func (a *App) SetProjectPinned(workspaceRoot string, pinned bool) error {
	root := normalizeProjectRoot(workspaceRoot)
	if root == "" {
		return fmt.Errorf("workspaceRoot is required")
	}
	if err := updateProjectsFile(func(f *desktopProjectFile) (bool, error) {
		i := projectIndexByRoot(f.Projects, root)
		if i < 0 {
			return false, fmt.Errorf("project %q not found", root)
		}
		root = f.Projects[i].Root
		next := make([]string, 0, len(f.PinnedProjects))
		for _, pinnedRoot := range f.PinnedProjects {
			if !sameProjectRoot(pinnedRoot, root) {
				next = append(next, pinnedRoot)
			}
		}
		if pinned {
			next = prependUniqueString(next, root)
		}
		if sameStringList(next, f.PinnedProjects) {
			return false, nil
		}
		f.PinnedProjects = next
		return true, nil
	}); err != nil {
		return err
	}
	a.emitProjectTreeMetadataChanged()
	return nil
}

// ReorderProjects persists the user-defined order of project folders and,
// when present, the virtual Global sidebar section.
func (a *App) ReorderProjects(workspaceRoots []string) error {
	if err := updateProjectsFile(func(f *desktopProjectFile) (bool, error) {
		var seenProjects []string
		next := make([]desktopProject, 0, len(workspaceRoots))
		sidebarOrder := make([]string, 0, len(workspaceRoots))
		hasGlobalOrder := false
		for _, root := range workspaceRoots {
			root = strings.TrimSpace(root)
			if root == desktopGlobalOrderToken {
				if hasGlobalOrder {
					return false, fmt.Errorf("duplicate global section")
				}
				hasGlobalOrder = true
				sidebarOrder = append(sidebarOrder, root)
				continue
			}
			root = normalizeProjectRoot(root)
			i := projectIndexByRoot(f.Projects, root)
			if i < 0 {
				return false, fmt.Errorf("project %q not found", root)
			}
			project := f.Projects[i]
			if projectRootInList(seenProjects, project.Root) {
				return false, fmt.Errorf("duplicate project %q", root)
			}
			seenProjects = append(seenProjects, project.Root)
			next = append(next, project)
			sidebarOrder = append(sidebarOrder, project.Root)
		}
		if len(next) != len(f.Projects) {
			return false, fmt.Errorf("project order length mismatch")
		}
		changed := !sameProjectOrder(next, f.Projects)
		f.Projects = next
		if hasGlobalOrder {
			if !sameStringList(sidebarOrder, f.SidebarOrder) {
				changed = true
			}
			f.SidebarOrder = sidebarOrder
		} else {
			if len(f.SidebarOrder) > 0 {
				changed = true
			}
			f.SidebarOrder = nil
		}
		return changed, nil
	}); err != nil {
		return err
	}
	a.emitProjectTreeMetadataChanged()
	return nil
}

// RenameTopic updates a topic's display title.
func (a *App) RenameTopic(topicID, title string) error {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		trimmed = defaultTopicTitle
	}

	f := loadProjectsFile()
	for _, p := range f.Projects {
		m := loadTopicTitles(p.Root)
		if _, ok := m[topicID]; ok {
			if err := setTopicTitle(p.Root, topicID, trimmed); err != nil {
				return err
			}
			a.updateOpenTopicTitle(topicID, trimmed, topicTitleSourceManual)
			changedDirs := a.updateTopicSessionTitles(topicID, trimmed)
			if len(changedDirs) > 0 {
				a.emitProjectTreeChangedForSessionDirs(changedDirs...)
			} else {
				a.emitProjectTreeMetadataChanged()
			}
			return nil
		}
	}

	m := loadTopicTitles("")
	if _, ok := m[topicID]; ok {
		if err := setTopicTitle("", topicID, trimmed); err != nil {
			return err
		}
		a.updateOpenTopicTitle(topicID, trimmed, topicTitleSourceManual)
		changedDirs := a.updateTopicSessionTitles(topicID, trimmed)
		if len(changedDirs) > 0 {
			a.emitProjectTreeChangedForSessionDirs(changedDirs...)
		} else {
			a.emitProjectTreeMetadataChanged()
		}
		return nil
	}
	if scope, workspaceRoot, ok := a.findTopicLocation(topicID); ok {
		if err := ensureTopicIndexed(scope, workspaceRoot, topicID, trimmed, topicTitleSourceManual); err != nil {
			return err
		}
		a.updateOpenTopicTitle(topicID, trimmed, topicTitleSourceManual)
		changedDirs := a.updateTopicSessionTitles(topicID, trimmed)
		if len(changedDirs) > 0 {
			a.emitProjectTreeChangedForSessionDirs(changedDirs...)
		} else {
			a.emitProjectTreeMetadataChanged()
		}
		return nil
	}
	return fmt.Errorf("topic %q not found", topicID)
}

func (a *App) findTopicLocation(topicID string) (string, string, bool) {
	topicID = strings.TrimSpace(topicID)
	if topicID == "" {
		return "", "", false
	}
	a.mu.RLock()
	for _, tab := range a.tabs {
		if tab == nil || tab.TopicID != topicID {
			continue
		}
		scope := tab.Scope
		workspaceRoot := tab.WorkspaceRoot
		a.mu.RUnlock()
		if scope == "global" {
			return "global", "", true
		}
		return "project", normalizeProjectRoot(workspaceRoot), true
	}
	a.mu.RUnlock()

	infos, err := agent.ListSessions(config.SessionDir())
	if err != nil {
		return "", "", false
	}
	for _, info := range infos {
		if strings.TrimSpace(info.TopicID) != topicID {
			continue
		}
		scope := strings.TrimSpace(info.Scope)
		if scope == "" {
			scope = "global"
		}
		if scope == "global" {
			return "global", "", true
		}
		return "project", normalizeProjectRoot(info.WorkspaceRoot), true
	}
	return "", "", false
}

func (a *App) updateOpenTopicTitle(topicID, title, source string) {
	if strings.TrimSpace(topicID) == "" || strings.TrimSpace(title) == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, tab := range a.runtimeTabsLocked() {
		if tab != nil && tab.TopicID == topicID {
			tab.TopicTitle = title
			tab.topicTitleSource = source
		}
	}
}

func (a *App) updateTopicSessionTitles(topicID, title string) []string {
	if strings.TrimSpace(topicID) == "" || strings.TrimSpace(title) == "" {
		return nil
	}
	var changedDirs []string
	for _, dir := range a.knownSessionDirs() {
		changed := false
		for _, match := range topicSessionMatches(dir, topicID) {

			unlock, lockErr := agent.LockSessionMetaPath(match.path)
			if lockErr != nil {
				continue
			}
			meta, ok, err := agent.LoadBranchMeta(match.path)
			if err != nil || !ok {
				unlock()
				continue
			}
			meta.TopicTitle = title
			err = agent.SaveBranchMetaPreserveUpdatedLocked(match.path, meta)
			unlock()
			if err == nil {
				invalidateTopicSessionIndex(dir)
				changed = true
			}
		}
		if changed {
			changedDirs = append(changedDirs, dir)
		}
	}
	return changedDirs
}

func (a *App) setTabActivityStatus(tabID, status string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	tab := a.tabByEventSinkIDLocked(tabID)
	if tab == nil {
		return false
	}
	status = normalizeTopicStatus(status)
	if tab.ActivityStatus == status {
		return false
	}
	tab.ActivityStatus = status
	return true
}

func (a *App) emitProjectTreeChanged() {
	a.requestSessionCatalogMetadataSync()
	for _, target := range a.sessionCatalogTargets() {
		a.requestSessionCatalogReconcile(target.Path)
	}
	a.emitProjectTreeChangedEvent()
}

// emitProjectTreeChangedForSessionDirs schedules only the affected catalog
// directories. It never scans synchronously on the mutation or UI goroutine.
func (a *App) emitProjectTreeChangedForSessionDirs(dirs ...string) {
	for _, dir := range dirs {
		a.requestSessionCatalogReconcile(dir)
	}
	a.emitProjectTreeChangedEvent()
}

// emitProjectTreeMetadataChanged refreshes ordering, titles, pins, and runtime
// status without walking session storage.
func (a *App) emitProjectTreeMetadataChanged() {
	a.requestSessionCatalogMetadataSync()
	a.emitProjectTreeChangedEvent()
}

func (a *App) emitProjectTreeChangedEvent() {
	if a.projectTreeChangedHook != nil {
		a.projectTreeChangedHook()
		return
	}
	a.emitRuntimeEvent("project-tree:changed")
}

// DeleteTopic removes a topic and its title metadata.
func (a *App) DeleteTopic(topicID string) error {
	return friendlySessionFileError(a.deleteTopic(topicID))
}

func (a *App) deleteTopic(topicID string) error {

	f := loadProjectsFile()
	indexed := map[string]bool{
		"": containsDesktopString(f.GlobalTopics, topicID) ||
			containsDesktopString(f.GlobalPinnedTopics, topicID),
	}
	roots := make([]string, 0, len(f.Projects)+1)
	for _, p := range f.Projects {
		roots = append(roots, p.Root)
		indexed[p.Root] = containsDesktopString(p.Topics, topicID) ||
			containsDesktopString(p.PinnedTopics, topicID)
	}
	roots = append(roots, "")
	for _, root := range roots {
		titles, err := loadTopicTitlesForUpdate(root)
		if err != nil {
			if indexed[root] {
				return err
			}
			continue
		}
		_, hasTitle := titles[topicID]
		if !hasTitle && !indexed[root] {
			continue
		}

		sources, err := loadTopicTitleSourcesForUpdate(root)
		if err != nil {
			return err
		}
		if _, ok := sources[topicID]; ok {
			delete(sources, topicID)
			if err := saveTopicTitleSources(root, sources); err != nil {
				return err
			}
		}
		if err := deleteTopicCreatedAt(root, topicID); err != nil {
			return err
		}
		if err := deleteTopicAutoTitleMeta(root, topicID); err != nil {
			return err
		}
		if hasTitle {
			delete(titles, topicID)
			if err := saveTopicTitles(root, titles); err != nil {
				return err
			}
		}
	}
	if err := removeTopicFromProjectsFile(topicID); err != nil {
		return err
	}
	a.emitProjectTreeMetadataChanged()
	return nil
}

// SetTopicPinned controls whether a topic is pinned to the top of its project
// or Global section in the desktop project tree.
func (a *App) SetTopicPinned(topicID string, pinned bool) error {
	topicID = strings.TrimSpace(topicID)
	if topicID == "" {
		return fmt.Errorf("topicID is required")
	}
	if err := updateProjectsFile(func(f *desktopProjectFile) (bool, error) {
		for i, p := range f.Projects {
			m := loadTopicTitles(p.Root)
			if _, ok := m[topicID]; !ok && !containsDesktopString(p.Topics, topicID) {
				continue
			}
			next := removeString(f.Projects[i].PinnedTopics, topicID)
			if pinned {
				next = prependUniqueString(f.Projects[i].PinnedTopics, topicID)
			}
			if sameStringList(next, f.Projects[i].PinnedTopics) {
				return false, nil
			}
			f.Projects[i].PinnedTopics = next
			return true, nil
		}
		globalTitles := loadTopicTitles("")
		if _, ok := globalTitles[topicID]; !ok && !containsDesktopString(f.GlobalTopics, topicID) {
			return false, fmt.Errorf("topic %q not found", topicID)
		}
		next := removeString(f.GlobalPinnedTopics, topicID)
		if pinned {
			next = prependUniqueString(f.GlobalPinnedTopics, topicID)
		}
		if sameStringList(next, f.GlobalPinnedTopics) {
			return false, nil
		}
		f.GlobalPinnedTopics = next
		return true, nil
	}); err != nil {
		return err
	}
	a.emitProjectTreeMetadataChanged()
	return nil
}
