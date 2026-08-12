package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// PickWorkspace opens a folder chooser and, on a pick, opens a new project tab
// scoped to that folder. Returns the chosen path ("" if cancelled).
func (a *App) PickWorkspace() (string, error) {
	if a.ctx == nil {
		return "", nil
	}
	cur, _ := os.Getwd()
	a.mu.RLock()
	if tab := a.activeTabLocked(); tab != nil && tab.WorkspaceRoot != "" {
		cur = tab.WorkspaceRoot
	}
	a.mu.RUnlock()
	dir, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "Choose working folder",
		DefaultDirectory: dialogDefaultDirectory(cur),
	})
	if err != nil || dir == "" {
		return "", err
	}
	return a.SwitchWorkspace(dir)
}

func dialogDefaultDirectory(preferred string) string {
	if dir := nearestExistingDirectory(preferred); dir != "" {
		return dir
	}
	if cwd, err := os.Getwd(); err == nil {
		if dir := nearestExistingDirectory(cwd); dir != "" {
			return dir
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if dir := nearestExistingDirectory(home); dir != "" {
			return dir
		}
	}
	return ""
}

func nearestExistingDirectory(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	for {
		info, err := os.Stat(path)
		if err == nil {
			if info.IsDir() {
				return path
			}
			path = filepath.Dir(path)
			continue
		}
		parent := filepath.Dir(path)
		if parent == path {
			return ""
		}
		path = parent
	}
}

func (a *App) ListWorkspaces() []WorkspaceMeta {
	migrateLegacyWorkspacesIntoProjects()
	activeRoot := ""
	cur, _ := os.Getwd()
	a.mu.RLock()
	if tab := a.activeTabLocked(); tab != nil && tab.WorkspaceRoot != "" {
		activeRoot = normalizeProjectRoot(tab.WorkspaceRoot)
	}
	a.mu.RUnlock()
	if activeRoot == "" {
		activeRoot = normalizeProjectRoot(cur)
	}
	projects := loadProjectsFile().Projects
	out := make([]WorkspaceMeta, 0, len(projects))
	for _, project := range projects {
		out = append(out, WorkspaceMeta{
			Path:    project.Root,
			Name:    projectDisplayName(project),
			Current: activeRoot != "" && sameProjectRoot(project.Root, activeRoot),
		})
	}
	return out
}

func (a *App) RemoveWorkspace(dir string) error {
	if dir == "" {
		return fmt.Errorf("workspace path is required")
	}
	dir = normalizeProjectRoot(dir)

	var fallback *WorkspaceTab

	if err := func() error {
		defer a.lockRuntimeMutation("remove-workspace")()
		a.sessionRemovalMu.Lock()
		defer a.sessionRemovalMu.Unlock()

		type workspaceTabCandidate struct {
			id  string
			tab *WorkspaceTab
		}

		var closeTabs []*WorkspaceTab
		var closeDetached []*WorkspaceTab
		a.mu.Lock()
		for _, tab := range a.tabs {
			if tabInWorkspace(tab, dir) && tab.hasActiveRuntimeWork() {
				a.mu.Unlock()
				return fmt.Errorf("workspace has running sessions; stop them before removing")
			}
		}
		for _, tab := range a.detachedSessions {
			if tabInWorkspace(tab, dir) && tab.hasActiveRuntimeWork() {
				a.mu.Unlock()
				return fmt.Errorf("workspace has running sessions; stop them before removing")
			}
		}
		candidates := make([]workspaceTabCandidate, 0)
		for id, tab := range a.tabs {
			if !tabInWorkspace(tab, dir) {
				continue
			}
			candidates = append(candidates, workspaceTabCandidate{id: id, tab: tab})
		}
		a.mu.Unlock()

		snapshotted := make(map[string]*WorkspaceTab, len(candidates))
		for _, candidate := range candidates {
			id, tab := candidate.id, candidate.tab
			snapshotted[id] = tab
			if err := a.snapshotTab(tab); err != nil {
				slog.Warn("desktop: snapshot before removing workspace failed", "tab", id, "workspace", dir, "err", err)
				return fmt.Errorf("save current session before removing workspace: %w", err)
			}
		}

		a.mu.Lock()
		for _, tab := range a.tabs {
			if tabInWorkspace(tab, dir) && tab.hasActiveRuntimeWork() {
				a.mu.Unlock()
				return fmt.Errorf("workspace has running sessions; stop them before removing")
			}
		}
		for _, tab := range a.detachedSessions {
			if tabInWorkspace(tab, dir) && tab.hasActiveRuntimeWork() {
				a.mu.Unlock()
				return fmt.Errorf("workspace has running sessions; stop them before removing")
			}
		}
		for id, tab := range a.tabs {
			if tabInWorkspace(tab, dir) && snapshotted[id] != tab {
				a.mu.Unlock()
				return fmt.Errorf("workspace tabs changed while removing; retry")
			}
		}
		for _, candidate := range candidates {
			id, tab := candidate.id, candidate.tab
			if tab == nil || a.tabs[id] != tab || !tabInWorkspace(tab, dir) {
				continue
			}
			a.markTabRemovedLocked(tab)
			closeTabs = append(closeTabs, tab)
			delete(a.tabs, id)
			a.removeTabOrderLocked(id)
			if a.activeTabID == id {
				a.activeTabID = ""
			}
		}
		for key, tab := range a.detachedSessions {
			if !tabInWorkspace(tab, dir) {
				continue
			}
			closeDetached = append(closeDetached, tab)
			delete(a.detachedSessions, key)
		}
		if len(a.tabs) == 0 {
			fallback = a.createTabEntry("global", globalTabWorkspaceRoot(), "")
			fallback.TopicTitle = "Global"
			fallback.sink = &tabEventSink{tabID: fallback.ID, app: a, ctx: a.ctx}
			a.tabs[fallback.ID] = fallback
			a.tabOrder = append(a.tabOrder, fallback.ID)
			a.activeTabID = fallback.ID
		} else if a.activeTabID == "" {
			if ordered := a.orderedTabIDsLocked(); len(ordered) > 0 {
				a.activeTabID = ordered[0]
			}
		}
		a.saveTabsLocked()
		a.mu.Unlock()

		for _, tab := range closeTabs {
			a.closeTabRuntimeAdmissionHeld(tab)
		}
		for _, tab := range closeDetached {
			a.closeTabRuntimeAdmissionHeld(tab)
		}
		return nil
	}(); err != nil {
		return err
	}

	if fallback != nil {
		a.startTabControllerBuild(fallback)
	}

	forgetWorkspace(dir)
	if err := removeProject(dir); err != nil {
		return err
	}

	if loadWorkspace() == dir {
		if remaining := loadProjectsFile(); len(remaining.Projects) > 0 {

			saveWorkspace(remaining.Projects[0].Root)
		} else {

			clearWorkspace()
		}
	}
	a.emitProjectTreeMetadataChanged()
	return nil
}

func migrateLegacyWorkspacesIntoProjects() {
	legacy := loadWorkspaces()
	if len(legacy) == 0 {
		return
	}
	_ = updateProjectsFile(func(f *desktopProjectFile) (bool, error) {
		seen := make(map[string]bool, len(f.Projects)+len(legacy))
		for _, p := range f.Projects {
			seen[p.Root] = true
		}
		changed := false
		for _, path := range legacy {
			root := normalizeProjectRoot(path)
			if root == "" || seen[root] {
				continue
			}
			f.Projects = append(f.Projects, desktopProject{Root: root})
			seen[root] = true
			changed = true
		}
		return changed, nil
	})
}

func workspaceName(path string) string {
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) || name == "" {
		return path
	}
	return name
}

// tabWorkspaceNameForScope resolves the display name for a tab's workspace.
// Callers pass tab.Scope copied under a.mu instead of re-reading the tab.
func tabWorkspaceNameForScope(scope, cwd string) string {
	if scope == "global" {
		return globalProjectTitle()
	}
	return workspaceName(cwd)
}

func (a *App) SwitchWorkspace(dir string) (string, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = home
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	saveWorkspace(dir)

	topic, err := a.CreateTopic("project", dir, "")
	if err != nil {
		return "", err
	}
	var meta TabMeta
	if a.singleSurfaceLayoutEnabled() {
		meta, err = a.ActivateTopic("project", dir, topic.ID, "")
	} else {
		meta, err = a.OpenProjectTab(dir, topic.ID)
	}
	if err != nil {
		return "", err
	}
	return meta.WorkspaceRoot, nil
}

func (a *App) singleSurfaceLayoutEnabled() bool {
	cfg, _, err := a.loadDesktopUserConfigForView()
	if err != nil {
		return true
	}
	return singleSurfaceLayoutStyle(cfg.DesktopLayoutStyle())
}
