package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/fileutil"
	"sort"
	"strings"
)

const desktopProjectsFile = "desktop-projects.json"
const tabsFileName = "desktop-tabs.json"
const desktopGlobalOrderToken = "__global__"
const legacyProjectSidebarRecoveryMarker = "desktop-projects-legacy-recovered"

type desktopProject struct {
	Root         string   `json:"root"`
	Title        string   `json:"title,omitempty"`
	Color        string   `json:"color,omitempty"`
	Topics       []string `json:"topics"` // ordered topic IDs
	PinnedTopics []string `json:"pinnedTopics,omitempty"`
	LockedTopics []string `json:"lockedTopics,omitempty"`
}

type desktopProjectFile struct {
	GlobalTitle        string           `json:"globalTitle,omitempty"`
	GlobalColor        string           `json:"globalColor,omitempty"`
	GlobalTopics       []string         `json:"globalTopics,omitempty"`
	GlobalPinnedTopics []string         `json:"globalPinnedTopics,omitempty"`
	GlobalLockedTopics []string         `json:"globalLockedTopics,omitempty"`
	DeletedTopics      []string         `json:"deletedTopics,omitempty"`
	PinnedProjects     []string         `json:"pinnedProjects,omitempty"`
	SidebarOrder       []string         `json:"sidebarOrder,omitempty"`
	Projects           []desktopProject `json:"projects"`
}

type desktopTabEntry struct {
	ID               string  `json:"id"`
	Scope            string  `json:"scope"`
	WorkspaceRoot    string  `json:"workspaceRoot"`
	TopicID          string  `json:"topicId"`
	SessionPath      string  `json:"sessionPath,omitempty"`
	ReadOnly         bool    `json:"readOnly,omitempty"`
	Model            string  `json:"model,omitempty"`
	Effort           *string `json:"effort,omitempty"`
	TokenMode        string  `json:"tokenMode,omitempty"`
	AgentPreset      string  `json:"agentPreset,omitempty"`
	Mode             string  `json:"mode,omitempty"`
	Goal             string  `json:"goal,omitempty"`
	ToolApprovalMode string  `json:"toolApprovalMode,omitempty"`
}

type desktopTabsFile struct {
	Tabs      []desktopTabEntry `json:"tabs"`
	ActiveTab string            `json:"activeTab"`
}

func singleSurfaceLayoutStyle(style string) bool {
	switch strings.ToLower(strings.TrimSpace(style)) {
	case "workbench", "creation":
		return true
	default:
		return false
	}
}

func singleSurfaceTabsFile(f desktopTabsFile) desktopTabsFile {
	if len(f.Tabs) <= 1 {
		return f
	}
	chosen := f.Tabs[0]
	if active := strings.TrimSpace(f.ActiveTab); active != "" {
		for _, entry := range f.Tabs {
			if entry.ID == active {
				chosen = entry
				break
			}
		}
	}
	return desktopTabsFile{Tabs: []desktopTabEntry{chosen}, ActiveTab: chosen.ID}
}

func desktopConfigDir() string {
	return config.ReasonixHomeDir()
}

func (a *App) saveTabsLocked() {
	dir, entries, activeID, version := a.saveTabsCollectLocked()
	a.saveTabsWrite(dir, entries, activeID, version)
}

// saveTabsCollectLocked gathers the tab-snapshot data under the caller's lock
// (it calls orderedTabIDsLocked which requires a.mu). Returns the config dir,
// the serializable entries, the active tab ID, and a monotonic snapshot version.
// The write can happen outside the lock to avoid blocking the UI with disk I/O.
func (a *App) saveTabsCollectLocked() (string, []desktopTabEntry, string, uint64) {
	dir := desktopConfigDir()
	var entries []desktopTabEntry
	for _, id := range a.orderedTabIDsLocked() {
		if tab := a.tabs[id]; tab != nil {
			entries = append(entries, desktopTabEntry{
				ID:               tab.ID,
				Scope:            tab.Scope,
				WorkspaceRoot:    tab.WorkspaceRoot,
				TopicID:          tab.TopicID,
				SessionPath:      tab.currentSessionPath(),
				ReadOnly:         tab.ReadOnly,
				Model:            tab.model,
				Effort:           cloneStringPtr(tab.effort),
				TokenMode:        persistedTabTokenMode(currentTabTokenMode(tab)),
				AgentPreset:      boot.NormalizeAgentPreset(currentTabTokenMode(tab)),
				Mode:             persistedTabMode(currentTabMode(tab)),
				Goal:             persistedTabGoal(tab),
				ToolApprovalMode: persistedToolApprovalMode(currentTabToolApprovalMode(tab)),
			})
		}
	}
	a.tabsSaveVersion++
	return dir, entries, a.activeTabID, a.tabsSaveVersion
}

// saveTabsWrite writes the tab-snapshot to disk. It does not require a.mu, but
// writes must be serialized because every save uses the same destination and
// fixed .tmp path.
func (a *App) saveTabsWrite(dir string, entries []desktopTabEntry, activeID string, version uint64) {
	a.tabsSaveMu.Lock()
	defer a.tabsSaveMu.Unlock()
	if version < a.tabsLastWrittenVersion {
		return
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f := desktopTabsFile{Tabs: entries, ActiveTab: activeID}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(dir, tabsFileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	if err := fileutil.ReplaceFile(tmp, path); err != nil {
		return
	}
	a.tabsLastWrittenVersion = version
}

func (a *App) orderedTabIDsLocked() []string {
	ordered, needsRepair := a.orderedTabIDsSnapshotLocked()
	if needsRepair {
		a.tabOrder = append([]string(nil), ordered...)
	}
	return ordered
}

func (a *App) orderedTabIDsSnapshotLocked() ([]string, bool) {
	seen := make(map[string]bool, len(a.tabs))
	ordered := make([]string, 0, len(a.tabs))
	for _, id := range a.tabOrder {
		if _, ok := a.tabs[id]; ok && !seen[id] {
			ordered = append(ordered, id)
			seen[id] = true
		}
	}
	var missing []string
	for id := range a.tabs {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	ordered = append(ordered, missing...)
	return ordered, len(ordered) != len(a.tabOrder) || len(missing) > 0
}

func (a *App) removeTabOrderLocked(tabID string) {
	next := a.tabOrder[:0]
	for _, id := range a.tabOrder {
		if id != tabID {
			next = append(next, id)
		}
	}
	a.tabOrder = next
}

func loadTabsFile() desktopTabsFile {
	path := filepath.Join(desktopConfigDir(), tabsFileName)
	b, err := readFileUTF8(path)
	if err != nil {
		return desktopTabsFile{}
	}
	var f desktopTabsFile
	_ = json.Unmarshal(b, &f)
	return f
}

func desktopMCPMigrationRoots(tabs desktopTabsFile) []string {
	seen := map[string]bool{}
	var roots []string
	add := func(root string) {
		root = normalizeProjectRoot(root)
		key := projectRootKey(root)
		if root == "" || seen[key] {
			return
		}
		seen[key] = true
		roots = append(roots, root)
	}
	if cur := loadWorkspace(); cur != "" {
		add(cur)
	}
	for _, root := range loadWorkspaces() {
		add(root)
	}
	for _, entry := range tabs.Tabs {
		if entry.Scope == "project" {
			add(entry.WorkspaceRoot)
		}
	}
	for _, project := range loadProjectsFile().Projects {
		add(project.Root)
	}
	return roots
}

func recoverLegacyProjectSidebarRoots(tabs desktopTabsFile) (bool, error) {
	markerPath := filepath.Join(desktopConfigDir(), legacyProjectSidebarRecoveryMarker)
	if _, err := os.Stat(markerPath); err == nil {
		return false, nil
	}

	changed := false
	err := updateProjectsFile(func(f *desktopProjectFile) (bool, error) {
		seen := map[string]bool{}
		for _, project := range f.Projects {
			root := normalizeProjectRoot(project.Root)
			if root != "" {
				seen[projectRootKey(root)] = true
			}
		}

		add := func(root string) {
			root = normalizeProjectRoot(root)
			key := projectRootKey(root)
			if root == "" || seen[key] || !existingDirectory(root) {
				return
			}
			seen[key] = true
			f.Projects = append(f.Projects, desktopProject{Root: root})
			changed = true
		}
		if cur := loadWorkspace(); cur != "" {
			add(cur)
		}
		for _, root := range loadWorkspaces() {
			add(root)
		}
		for _, entry := range tabs.Tabs {
			if entry.Scope == "project" {
				add(entry.WorkspaceRoot)
			}
		}
		return changed, nil
	})
	if err != nil {
		return false, err
	}
	return changed, writeLegacyProjectSidebarRecoveryMarker(markerPath)
}

func existingDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func writeLegacyProjectSidebarRecoveryMarker(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("ok\n"), 0o644)
}

func loadProjectsFile() desktopProjectFile {
	path := filepath.Join(desktopConfigDir(), desktopProjectsFile)
	b, err := readFileUTF8(path)
	if err != nil {
		return desktopProjectFile{}
	}
	var f desktopProjectFile
	_ = json.Unmarshal(b, &f)
	return normalizeProjectsFile(f)
}

func saveProjectsFile(f desktopProjectFile) error {
	dir := desktopConfigDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f = normalizeProjectsFile(f)
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, desktopProjectsFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return fileutil.ReplaceFile(tmp, path)
}

func updateProjectsFile(mutator func(*desktopProjectFile) (bool, error)) error {
	desktopProjectsFileMu.Lock()
	defer desktopProjectsFileMu.Unlock()

	f := loadProjectsFile()
	changed, err := mutator(&f)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return saveProjectsFile(f)
}

func prependTopicInProjectsFile(workspaceRoot, topicID string, ensureProject bool) error {

	return prependTopicsInProjectsFileOpts(workspaceRoot, []string{topicID}, ensureProject, false)
}

func prependTopicsInProjectsFile(workspaceRoot string, topicIDs []string, ensureProject bool) error {

	return prependTopicsInProjectsFileOpts(workspaceRoot, topicIDs, ensureProject, true)
}

func prependTopicsInProjectsFileOpts(workspaceRoot string, topicIDs []string, ensureProject, respectTombstones bool) error {
	workspaceRoot = normalizeProjectRoot(workspaceRoot)
	topicIDs = uniqueStrings(topicIDs)
	if len(topicIDs) == 0 {
		return nil
	}
	return updateProjectsFile(func(f *desktopProjectFile) (bool, error) {

		live := topicIDs
		changed := false
		if respectTombstones {
			live = make([]string, 0, len(topicIDs))
			for _, id := range topicIDs {
				if !containsDesktopString(f.DeletedTopics, id) {
					live = append(live, id)
				}
			}
			if len(live) == 0 {
				return false, nil
			}
		} else {
			for _, id := range topicIDs {
				if next := removeString(f.DeletedTopics, id); !sameStringList(next, f.DeletedTopics) {
					f.DeletedTopics = next
					changed = true
				}
			}
		}
		if workspaceRoot == "" {
			next := uniqueStrings(append(append([]string(nil), live...), f.GlobalTopics...))
			if sameStringList(next, f.GlobalTopics) {
				return changed, nil
			}
			f.GlobalTopics = next
			return true, nil
		}
		for i, p := range f.Projects {
			if !sameProjectRoot(p.Root, workspaceRoot) {
				continue
			}
			next := uniqueStrings(append(append([]string(nil), live...), p.Topics...))
			if sameStringList(next, p.Topics) {
				return changed, nil
			}
			f.Projects[i].Topics = next
			return true, nil
		}
		if !ensureProject {
			return changed, nil
		}
		f.Projects = append(f.Projects, desktopProject{Root: workspaceRoot, Topics: live})
		return true, nil
	})
}

func removeTopicFromProjectsFile(topicID string) error {
	topicID = strings.TrimSpace(topicID)
	if topicID == "" {
		return nil
	}
	return updateProjectsFile(func(f *desktopProjectFile) (bool, error) {
		changed := false
		if next := removeString(f.GlobalTopics, topicID); !sameStringList(next, f.GlobalTopics) {
			f.GlobalTopics = next
			changed = true
		}
		if next := removeString(f.GlobalPinnedTopics, topicID); !sameStringList(next, f.GlobalPinnedTopics) {
			f.GlobalPinnedTopics = next
			changed = true
		}
		if next := removeString(f.GlobalLockedTopics, topicID); !sameStringList(next, f.GlobalLockedTopics) {
			f.GlobalLockedTopics = next
			changed = true
		}
		if next := prependUniqueString(f.DeletedTopics, topicID); !sameStringList(next, f.DeletedTopics) {
			f.DeletedTopics = next
			changed = true
		}
		for i, p := range f.Projects {
			if next := removeString(p.Topics, topicID); !sameStringList(next, p.Topics) {
				f.Projects[i].Topics = next
				changed = true
			}
			if next := removeString(p.PinnedTopics, topicID); !sameStringList(next, p.PinnedTopics) {
				f.Projects[i].PinnedTopics = next
				changed = true
			}
			if next := removeString(p.LockedTopics, topicID); !sameStringList(next, p.LockedTopics) {
				f.Projects[i].LockedTopics = next
				changed = true
			}
		}
		return changed, nil
	})
}

func normalizeProjectRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return root
}

func sameProjectRoot(a, b string) bool {
	return sameDesktopPath(normalizeProjectRoot(a), normalizeProjectRoot(b))
}

func projectIndexByRoot(projects []desktopProject, root string) int {
	root = normalizeProjectRoot(root)
	if root == "" {
		return -1
	}
	for i, project := range projects {
		if sameProjectRoot(project.Root, root) {
			return i
		}
	}
	return -1
}

func projectRootInList(roots []string, root string) bool {
	root = normalizeProjectRoot(root)
	if root == "" {
		return false
	}
	for _, candidate := range roots {
		if sameProjectRoot(candidate, root) {
			return true
		}
	}
	return false
}
