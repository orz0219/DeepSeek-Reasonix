package main

import (
	"os"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/control"
	"reasonix/internal/evidence"
)

func (a *App) MetaForTab(tabID string) Meta {
	a.mu.RLock()
	tab := a.tabByIDLocked(tabID)
	snap := snapshotTabRuntimeLocked(tab)
	runtimeView := a.sessionRuntimeViewLocked(tab)
	a.mu.RUnlock()
	if tab == nil {
		return Meta{EventChannel: eventChannel}
	}
	cwd := snap.workspaceRoot
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	extras, refreshExtras := tabMetaExtrasFor(tab, cwd, snap.model)
	if refreshExtras {
		a.scheduleTabMetaExtrasRefresh(tab.ID)
	}
	autoApproveTools := snap.ctrl != nil && snap.ctrl.AutoApproveTools()
	collaborationMode := snap.collaborationMode()
	toolApprovalMode := snap.currentToolApprovalMode()
	tokenMode := snap.currentTokenMode()
	goal := snap.currentGoal()
	goalStatus := snap.currentGoalStatus()
	sessionPath := strings.TrimSpace(snap.sessionPath)
	var sessionRevision int64
	var sessionDigest string
	if branchMeta, ok, err := agent.LoadBranchMeta(sessionPath); err == nil && ok {
		sessionRevision = branchMeta.Revision
		sessionDigest = branchMeta.ContentDigest
	}
	return Meta{
		Label:             snap.label,
		Ready:             runtimeView.Phase == sessionRuntimeReady && snap.ctrl != nil,
		Runtime:           runtimeView,
		StartupErr:        snap.startupErr,
		EventChannel:      eventChannel,
		SessionPath:       sessionPath,
		SessionRevision:   sessionRevision,
		SessionDigest:     sessionDigest,
		Cwd:               cwd,
		WorkspaceRoot:     cwd,
		WorkspaceName:     tabWorkspaceNameForScope(snap.scope, cwd),
		WorkspacePath:     cwd,
		GitBranch:         extras.gitBranch,
		ImageInputEnabled: extras.imageInputEnabled,
		AutoApproveTools:  autoApproveTools,
		Bypass:            autoApproveTools,
		CollaborationMode: collaborationMode,
		ToolApprovalMode:  toolApprovalMode,
		AgentPreset:       boot.NormalizeAgentPreset(tokenMode),
		TokenMode:         tokenMode,
		Goal:              goal,
		GoalStatus:        goalStatus,
		GoalRuntime:       goalRuntimeViewFromController(snap.ctrl),
		CanonicalTodos:    ctrlTodos(snap.ctrl),
	}
}

// ctrlTodos returns the canonical task list from a session controller, or nil
// if the controller is not yet bound. Used by MetaForTab so the frontend
// task panel has access to the authoritative server-side todo state.
func ctrlTodos(ctrl control.SessionAPI) *[]evidence.TodoItem {
	if ctrl == nil {
		return nil
	}
	todos := ctrl.Todos()
	if todos == nil {
		todos = []evidence.TodoItem{}
	}
	return &todos
}
