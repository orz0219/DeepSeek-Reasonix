package main

import (
	"strings"

	"reasonix/internal/control"
)

func (a *App) SetGoal(goal string) error {
	return a.SetGoalForTab("", goal)
}

// SetGoalForTab activates or clears a Goal on the given tab.
//
// Failures must return error so the Wails Promise rejects: the first Goal turn
// can submit a structured Skill without a /goal prose fallback, and the
// frontend aborts that submit when activation fails.
func (a *App) SetGoalForTab(tabID, goal string) error {
	tab := a.tabByID(tabID)
	if tab == nil {
		return a.workspaceNotReadyErr(nil)
	}
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	goal = strings.TrimSpace(goal)
	approvalMode := a.tabRuntimeSnapshot(tab).currentToolApprovalMode()
	a.mu.Lock()
	if a.tabs[tab.ID] != tab {
		a.mu.Unlock()
		return a.workspaceNotReadyErr(nil)
	}
	tab.goal = goal
	if goal != "" {
		tab.mode = tabModeFromAxes(false, approvalMode == control.ToolApprovalYolo)
	}
	ctrl := tab.Ctrl
	plan := tabModeHasPlan(tab.mode)
	tabIDForSave := tab.ID
	a.mu.Unlock()
	if ctrl != nil {
		ctrl.SetPlanMode(plan)
		syncTabGoalToController(ctrl, goal)
	}
	a.mu.Lock()
	if a.tabs[tabIDForSave] == tab {
		a.saveTabsLocked()
	}
	a.mu.Unlock()
	return nil
}

// The composer re-syncs collaboration mode and Goal immediately before every
// send. Keep those acknowledgements idempotent so one multi-turn Goal retains
// its delivery scope; a terminal Goal with the same text still starts a fresh
// scope when the user explicitly enters it again.
func syncTabGoalToController(ctrl control.SessionAPI, goal string) {
	if ctrl == nil {
		return
	}
	goal = strings.TrimSpace(goal)
	if goal != "" && strings.TrimSpace(ctrl.Goal()) == goal && ctrl.GoalStatus() == control.GoalStatusRunning {
		return
	}
	ctrl.SetGoal(goal)
}

func (a *App) ClearGoal() error {
	return a.SetGoal("")
}

func (a *App) ClearGoalForTab(tabID string) error {
	return a.SetGoalForTab(tabID, "")
}

// ResumeGoalForTab re-enters a blocked or stopped Goal while preserving its
// delivery scope, runtime history, and persisted verification checkpoint.
func (a *App) ResumeGoalForTab(tabID string) bool {
	tab := a.tabByID(tabID)
	if tab == nil {
		return false
	}
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	ctrl := a.controllerForTab(tab)
	if ctrl == nil || !ctrl.ResumeGoal() {
		return false
	}
	a.mu.Lock()
	if a.tabs[tab.ID] == tab {
		tab.goal = strings.TrimSpace(ctrl.Goal())
		a.saveTabsLocked()
	}
	a.mu.Unlock()
	return true
}

// PauseGoalForTab suspends a running Goal without clearing it; ResumeGoalForTab
// restores it (with one extra budget slice when it was budget-paused).
func (a *App) PauseGoalForTab(tabID string) bool {
	tab := a.tabByID(tabID)
	if tab == nil {
		return false
	}
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	ctrl := a.controllerForTab(tab)
	return ctrl != nil && ctrl.PauseGoal()
}

// SetAutoApproveTools toggles YOLO/full-access tool auto-approval:
// approval-gated tool calls run without asking, while ask questions and plan
// approvals still wait for the user. Runtime-only — not written to config.
func (a *App) SetAutoApproveTools(on bool) {
	if on {
		a.SetToolApprovalModeForTab("", control.ToolApprovalYolo)
		return
	}
	a.SetToolApprovalModeForTab("", control.ToolApprovalAsk)
}

// SetBypass is the legacy Wails binding for SetAutoApproveTools.
func (a *App) SetBypass(on bool) {
	a.SetAutoApproveTools(on)
}

func (a *App) SetToolApprovalMode(mode string) {
	a.SetToolApprovalModeForTab("", mode)
}

// SetToolApprovalModeForTab returns the pending approval prompt ids the
// switch auto-allowed (see SetModeForTab).
func (a *App) SetToolApprovalModeForTab(tabID, mode string) []string {
	tab := a.tabByID(tabID)
	if tab == nil {
		return nil
	}
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	mode = normalizeToolApprovalMode(mode)
	plan := tabModeHasPlan(a.tabRuntimeSnapshot(tab).currentMode())
	a.mu.Lock()
	if a.tabs[tab.ID] != tab {
		a.mu.Unlock()
		return nil
	}
	tab.toolApprovalMode = mode
	tab.mode = tabModeFromAxes(plan, mode == control.ToolApprovalYolo)
	ctrl := tab.Ctrl
	tabIDForSave := tab.ID
	a.mu.Unlock()
	drained := applyTabToolApprovalModeToController(ctrl, mode)
	a.mu.Lock()
	if a.tabs[tabIDForSave] == tab {
		a.saveTabsLocked()
	}
	a.mu.Unlock()
	return drained
}

// CommandInfo describes one available slash command for the composer's "/" menu.
type CommandInfo struct {
	Name        string `json:"name"` // without the leading slash
	Description string `json:"description"`
	Hint        string `json:"hint,omitempty"`  // argument hint, if any
	Kind        string `json:"kind"`            // "builtin" | "custom" | "mcp" | "skill" | "subagent"
	Group       string `json:"group,omitempty"` // menu group; older frontends can ignore it
	Plugin      string `json:"plugin,omitempty"`
	Color       string `json:"color,omitempty"`
}
