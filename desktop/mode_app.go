package main

import (
	"fmt"
	"strings"

	"reasonix/internal/control"
	"reasonix/internal/event"
)

// SetPlanMode toggles the plan-first workflow while preserving the current
// tool-approval posture and sandbox settings.
func (a *App) SetPlanMode(on bool) {
	a.setPlanModeForTab("", on)
}

func (a *App) setPlanModeForTab(tabID string, on bool) {
	if on {
		a.SetCollaborationModeForTab(tabID, "plan")
		return
	}
	a.SetCollaborationModeForTab(tabID, "normal")
}

// SetMode applies a composer gating mode ("plan" | "yolo" | "plan-yolo" |
// anything else =
// normal) in one call, so a turn submitted right after the switch can't race a
// half-applied plan/tool-auto-approval pair.
func (a *App) SetMode(mode string) {
	a.SetModeForTab("", mode)
}

// SetModeForTab returns the pending approval prompt ids the switch
// auto-allowed, so the frontend dismisses exactly those cards and keeps the
// ones the backend still holds (plan/memory/sandbox-escape never drain, and
// auto keeps approvals an allow policy would not cover — #6432).
func (a *App) SetModeForTab(tabID, mode string) []string {
	tab := a.tabByID(tabID)
	if tab == nil {
		return nil
	}
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	normalized := normalizeTabMode(mode)
	a.mu.Lock()
	if a.tabs[tab.ID] != tab {
		a.mu.Unlock()
		return nil
	}
	tab.mode = normalized
	tab.toolApprovalMode = normalizeToolApprovalMode(tab.toolApprovalMode)
	if tabModeHasAutoApproveTools(normalized) {
		tab.toolApprovalMode = control.ToolApprovalYolo
	} else if tab.toolApprovalMode == control.ToolApprovalYolo {
		tab.toolApprovalMode = control.ToolApprovalAsk
	}
	ctrl := tab.Ctrl
	approvalMode := tab.toolApprovalMode
	tabIDForSave := tab.ID
	a.mu.Unlock()
	drained := applyTabModeToController(ctrl, normalized)
	drained = append(drained, applyTabToolApprovalModeToController(ctrl, approvalMode)...)
	a.mu.Lock()
	if a.tabs[tabIDForSave] == tab {
		a.saveTabsLocked()
	}
	a.mu.Unlock()
	return drained
}

// modeApplier / toolApprovalApplier are the drained-id-reporting variants of
// SessionAPI's SetMode / SetToolApprovalMode. Asserted optionally so test
// fakes implementing the plain SessionAPI keep compiling (they report nil).
type modeApplier interface {
	ApplyMode(plan, autoApproveTools bool) []string
}

type toolApprovalApplier interface {
	ApplyToolApprovalMode(mode string) []string
}

func applyTabModeToController(ctrl control.SessionAPI, mode string) []string {
	if ctrl == nil {
		return nil
	}
	plan, yolo := false, false
	switch normalizeTabMode(mode) {
	case "plan":
		plan = true
	case "yolo":
		yolo = true
	case "plan-yolo":
		plan, yolo = true, true
	}
	if applier, ok := ctrl.(modeApplier); ok {
		return applier.ApplyMode(plan, yolo)
	}
	ctrl.SetMode(plan, yolo)
	return nil
}

func applyTabToolApprovalModeToController(ctrl control.SessionAPI, mode string) []string {
	if ctrl == nil {
		return nil
	}
	mode = normalizeToolApprovalMode(mode)
	if applier, ok := ctrl.(toolApprovalApplier); ok {
		return applier.ApplyToolApprovalMode(mode)
	}
	ctrl.SetToolApprovalMode(mode)
	return nil
}

func normalizeCollaborationMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "plan":
		return "plan"
	case "goal":
		return "goal"
	default:
		return "normal"
	}
}

func (a *App) SetCollaborationMode(mode string) {
	a.SetCollaborationModeForTab("", mode)
}

// SetComposerProfileForTab applies the controller-facing profile axes under one
// turn gate. Frontends use this before submit and after controller rebuilds so a
// turn cannot observe collaboration, approval, and goal from different UI
// generations.
func (a *App) SetComposerProfileForTab(tabID, collaborationMode, toolApprovalMode, goal string) ([]string, error) {
	collaborationMode = normalizeCollaborationMode(collaborationMode)
	toolApprovalMode = normalizeToolApprovalMode(toolApprovalMode)
	goal = strings.TrimSpace(goal)

	tab := a.tabByID(tabID)
	if tab == nil {
		return []string{}, fmt.Errorf("tab is no longer available")
	}
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()

	a.mu.Lock()
	if a.tabs[tab.ID] != tab {
		a.mu.Unlock()
		return []string{}, fmt.Errorf("tab is no longer available")
	}
	tab.toolApprovalMode = toolApprovalMode
	if goal != "" {
		tab.goal = goal
		tab.mode = tabModeFromAxes(false, toolApprovalMode == control.ToolApprovalYolo)
	} else {
		tab.goal = ""
		tab.mode = tabModeFromAxes(collaborationMode == "plan", toolApprovalMode == control.ToolApprovalYolo)
	}
	ctrl := tab.Ctrl
	mode := tab.mode
	goal = tab.goal
	tabIDForSave := tab.ID
	a.mu.Unlock()

	if ctrl != nil {
		ctrl.SetPlanMode(tabModeHasPlan(mode))
	}
	drained := applyTabToolApprovalModeToController(ctrl, toolApprovalMode)
	syncTabGoalToController(ctrl, goal)

	a.mu.Lock()
	if a.tabs[tabIDForSave] == tab {
		a.saveTabsLocked()
	}
	a.mu.Unlock()
	if drained == nil {
		return []string{}, nil
	}
	return drained, nil
}

func (a *App) SetCollaborationModeForTab(tabID, mode string) {
	tab := a.tabByID(tabID)
	if tab == nil {
		return
	}
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	mode = normalizeCollaborationMode(mode)
	approvalMode := a.tabRuntimeSnapshot(tab).currentToolApprovalMode()
	a.mu.Lock()
	if a.tabs[tab.ID] != tab {
		a.mu.Unlock()
		return
	}
	switch mode {
	case "plan":
		tab.mode = tabModeFromAxes(true, approvalMode == control.ToolApprovalYolo)
		tab.goal = ""
	case "goal":
		tab.mode = tabModeFromAxes(false, approvalMode == control.ToolApprovalYolo)
	default:
		tab.mode = tabModeFromAxes(false, approvalMode == control.ToolApprovalYolo)
		tab.goal = ""
	}
	ctrl := tab.Ctrl
	goal := tab.goal
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
}

// QuestionAnswer is the frontend's reply to one question in an ask_request.
type QuestionAnswer struct {
	QuestionID string   `json:"questionId"`
	Selected   []string `json:"selected"`
}

// AnswerQuestion resolves a pending ask_request (the `ask` tool) by ID with the
// user's selections per question.
func (a *App) AnswerQuestion(id string, answers []QuestionAnswer) {
	a.AnswerQuestionForTab("", id, answers)
}

func (a *App) AnswerQuestionForTab(tabID, id string, answers []QuestionAnswer) {
	ctrl := a.ctrlByTabID(tabID)
	if ctrl == nil {
		return
	}
	out := make([]event.AskAnswer, len(answers))
	for i, an := range answers {
		out[i] = event.AskAnswer{QuestionID: an.QuestionID, Selected: an.Selected}
	}
	ctrl.AnswerQuestion(id, out)
}
