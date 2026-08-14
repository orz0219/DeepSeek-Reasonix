package main

import (
	"fmt"
	"path/filepath"
	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/worktree"
)

// TabMeta is the frontend-facing shape of one tab.
type TabMeta struct {
	ID                string             `json:"id"`
	Scope             string             `json:"scope"`
	WorkspaceRoot     string             `json:"workspaceRoot"`
	WorkspaceName     string             `json:"workspaceName"`
	WorkspacePath     string             `json:"workspacePath,omitempty"`
	GitBranch         string             `json:"gitBranch,omitempty"`
	IsolatedWorktree  bool               `json:"isolatedWorktree,omitempty"`
	TopicID           string             `json:"topicId"`
	TopicTitle        string             `json:"topicTitle"`
	SessionPath       string             `json:"sessionPath,omitempty"`
	SessionRevision   int64              `json:"sessionRevision,omitempty"`
	SessionDigest     string             `json:"sessionDigest,omitempty"`
	SessionGeneration uint64             `json:"sessionGeneration,omitempty"`
	ReadOnly          bool               `json:"readOnly,omitempty"`
	ProjectColor      string             `json:"projectColor,omitempty"`
	Label             string             `json:"label"`
	Ready             bool               `json:"ready"`
	Runtime           SessionRuntimeView `json:"runtime"`
	Running           bool               `json:"running"`
	PendingPrompt     bool               `json:"pendingPrompt,omitempty"`
	BackgroundJobs    int                `json:"backgroundJobs,omitempty"`
	CancelRequested   bool               `json:"cancelRequested,omitempty"`
	Cancellable       bool               `json:"cancellable"`
	Mode              string             `json:"mode"`
	CollaborationMode string             `json:"collaborationMode"`
	ToolApprovalMode  string             `json:"toolApprovalMode"`
	TokenMode         string             `json:"tokenMode"`
	AgentPreset       string             `json:"agentPreset,omitempty"`
	Goal              string             `json:"goal,omitempty"`
	GoalStatus        string             `json:"goalStatus,omitempty"`
	Recovered         bool               `json:"recovered,omitempty"`
	RecoveryReason    string             `json:"recoveryReason,omitempty"`
	RecoveryDigest    string             `json:"recoveryDigest,omitempty"`
	RecoveryParentID  string             `json:"recoveryParentId,omitempty"`
	StartupErr        string             `json:"startupErr,omitempty"`
	Active            bool               `json:"active"`
	Cwd               string             `json:"cwd"`
}

func enrichTabMeta(meta TabMeta) TabMeta {
	if meta.Active {
		meta.GitBranch = workspaceGitBranchForMeta(meta.WorkspaceRoot)
	}
	return meta
}

func enrichTabMetas(metas []TabMeta) []TabMeta {
	for i := range metas {
		if metas[i].Active {
			metas[i].GitBranch = workspaceGitBranchForMeta(metas[i].WorkspaceRoot)
		}
	}
	return metas
}

func (a *App) tabMeta(tab *WorkspaceTab, active bool) TabMeta {
	runtimeView := a.sessionRuntimeViewLocked(tab)
	sessionPath := tab.currentSessionPath()
	var sessionRevision int64
	var sessionDigest string
	if meta, ok, err := agent.LoadBranchMeta(sessionPath); err == nil && ok {
		sessionRevision = meta.Revision
		sessionDigest = meta.ContentDigest
	}
	m := TabMeta{
		ID:                tab.ID,
		Scope:             tab.Scope,
		WorkspaceRoot:     tab.WorkspaceRoot,
		WorkspaceName:     workspaceName(tab.WorkspaceRoot),
		WorkspacePath:     tab.WorkspaceRoot,
		TopicID:           tab.TopicID,
		TopicTitle:        a.localizedTopicTitle(tab.TopicTitle, tab.topicTitleSource),
		SessionPath:       sessionPath,
		SessionRevision:   sessionRevision,
		SessionDigest:     sessionDigest,
		SessionGeneration: tab.SessionGeneration,
		ReadOnly:          tab.ReadOnly,
		Label:             tab.Label,
		Ready:             runtimeView.Phase == sessionRuntimeReady && tab.Ctrl != nil,
		Runtime:           runtimeView,
		Mode:              currentTabMode(tab),
		CollaborationMode: currentTabCollaborationMode(tab),
		ToolApprovalMode:  currentTabToolApprovalMode(tab),
		AgentPreset:       boot.NormalizeAgentPreset(currentTabTokenMode(tab)),
		TokenMode:         currentTabTokenMode(tab),
		Goal:              currentTabGoal(tab),
		GoalStatus:        currentTabGoalStatus(tab),
		StartupErr:        tab.StartupErr,
		Active:            active,
		Cwd:               tab.WorkspaceRoot,
		IsolatedWorktree:  worktree.IsManagedPath(tab.WorkspaceRoot, config.DeliveryWorktreeDir()),
	}
	switch tab.Scope {
	case "global":
		m.ProjectColor = globalProjectColor()
		m.WorkspaceName = globalProjectTitle()
	case "project":
		m.ProjectColor = projectColor(tab.WorkspaceRoot)
	}
	if tab.Ctrl != nil {
		status := tab.Ctrl.RuntimeStatus()
		m.Running = status.Running || status.PendingPrompt || status.BackgroundJobs > 0
		m.PendingPrompt = status.PendingPrompt
		m.BackgroundJobs = status.BackgroundJobs
		m.CancelRequested = status.CancelRequested
		m.Cancellable = status.Cancellable
	}
	if meta, ok, err := agent.LoadBranchMeta(tab.currentSessionPath()); err == nil && ok && meta.Recovered {
		m.Recovered = true
		m.RecoveryReason = meta.RecoveryReason
		m.RecoveryDigest = meta.RecoveryDigest
		m.RecoveryParentID = string(meta.ParentID)
	}
	return m
}

// ListTabs returns every open view container's metadata for the frontend chrome and sidebar.
func (a *App) ListTabs() []TabMeta {
	a.mu.RLock()
	out := make([]TabMeta, 0, len(a.tabs))
	ordered, needsRepair := a.orderedTabIDsSnapshotLocked()
	for _, id := range ordered {
		if tab := a.tabs[id]; tab != nil {
			out = append(out, a.tabMeta(tab, tab.ID == a.activeTabID))
		}
	}
	a.mu.RUnlock()
	if !needsRepair {
		return enrichTabMetas(out)
	}

	a.mu.Lock()
	out = make([]TabMeta, 0, len(a.tabs))
	for _, id := range a.orderedTabIDsLocked() {
		if tab := a.tabs[id]; tab != nil {
			out = append(out, a.tabMeta(tab, tab.ID == a.activeTabID))
		}
	}
	a.mu.Unlock()
	return enrichTabMetas(out)
}

// syncTabWorkspaceRootSpellings repoints open project tabs at the registry's
// canonical root spelling after a registry write may have rewritten it
// (addProject and friends adopt the caller's spelling). Tabs, the project
// tree, and persisted tab state then agree on a single string form of each
// root, which the frontend compares exactly. Callers must not hold a.mu.
func (a *App) syncTabWorkspaceRootSpellings() {
	projects := loadProjectsFile().Projects
	a.mu.Lock()
	changed := false
	for _, tab := range a.tabs {
		if tab == nil || tab.Scope != "project" {
			continue
		}
		i := projectIndexByRoot(projects, tab.WorkspaceRoot)
		if i < 0 || tab.WorkspaceRoot == projects[i].Root {
			continue
		}
		tab.WorkspaceRoot = projects[i].Root
		changed = true
	}
	if changed {
		a.saveTabsLocked()
	}
	a.mu.Unlock()
	if changed {
		a.emitProjectTreeMetadataChanged()
	}
}

// registerProjectRoot indexes workspaceRoot in the project registry and
// realigns open tabs when the registry adopted a new spelling of the root.
func (a *App) registerProjectRoot(workspaceRoot string) {
	_ = addProject(workspaceRoot, "")
	a.syncTabWorkspaceRootSpellings()
}

// OpenProjectTab builds a controller scoped to workspaceRoot and opens the
// session selected by the given topic. Topic selection resolves to a concrete
// session path first; the visible tab is then attached to that session runtime.
func (a *App) OpenProjectTab(workspaceRoot, topicID string) (TabMeta, error) {
	return a.openProjectTab(workspaceRoot, topicID)
}

func (a *App) openProjectTab(workspaceRoot, topicID string) (TabMeta, error) {
	if workspaceRoot == "" {
		return TabMeta{}, fmt.Errorf("workspaceRoot is required")
	}
	if abs, err := filepath.Abs(workspaceRoot); err == nil {
		workspaceRoot = abs
	}
	saveWorkspace(workspaceRoot)
	a.registerProjectRoot(workspaceRoot)

	sessionPath, _ := a.findTopicSessionForTarget("project", workspaceRoot, topicID)
	return a.openTopicTab("project", workspaceRoot, topicID, sessionPath)
}
