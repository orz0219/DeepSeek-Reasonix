package main

import (
	"fmt"

	"reasonix/internal/agent"
	"reasonix/internal/control"
)

// Approve answers a pending approval_request by ID: allow runs the call, session
// also remembers the grant for the rest of the session.
func (a *App) Approve(id string, allow, session, persist bool) {
	ctrl := a.ctrlByTabID("")
	if ctrl != nil {
		ctrl.Approve(id, allow, session, persist)
	}
}

// ApproveTab is like Approve but scoped to a specific tab.
func (a *App) ApproveTab(tabID, id string, allow, session, persist bool) {
	ctrl := a.ctrlForRuntimeTabID(tabID)
	if ctrl != nil {
		ctrl.Approve(id, allow, session, persist)
	}
}

// ResolvePlanDecision answers a Plan card while preserving whether the user
// chose to start execution, revise the plan, or exit without executing.
func (a *App) ResolvePlanDecision(id, action string) error {
	ctrl := a.ctrlByTabID("")
	if ctrl == nil {
		return fmt.Errorf("no active session")
	}
	return ctrl.ResolvePlanDecision(id, control.PlanDecisionAction(action))
}

// ResolvePlanDecisionTab is like ResolvePlanDecision but scoped to a runtime
// tab so a delayed bridge call cannot answer a prompt in another tab.
func (a *App) ResolvePlanDecisionTab(tabID, id, action string) error {
	ctrl := a.ctrlForRuntimeTabID(tabID)
	if ctrl == nil {
		return fmt.Errorf("no active session")
	}
	return ctrl.ResolvePlanDecision(id, control.PlanDecisionAction(action))
}

// ResolveRecovery answers an Auto Guard card. action is continue|revise. For
// revise, feedback is steered into the
// agent and the pending mutation is refused in the same operation.
func (a *App) ResolveRecovery(id, action, feedback string) error {
	return a.ResolveRecoveryTab("", id, action, feedback)
}

// ResolveRecoveryTab is like ResolveRecovery but scoped to a specific tab.
func (a *App) ResolveRecoveryTab(tabID, id, action, feedback string) error {
	ctrl := a.ctrlByTabID(tabID)
	if ctrl == nil {
		return fmt.Errorf("no active session")
	}
	return ctrl.ResolveRecovery(id, agent.RecoveryAction(action), feedback)
}

// SetRecoveryCheckpointEnabled is retained as a no-op Wails surface for older
// generated frontends. Auto Guard is always built into Auto.
func (a *App) SetRecoveryCheckpointEnabled(_ bool) {}

// SetRecoveryCheckpointEnabledTab is retained as a no-op Wails surface.
func (a *App) SetRecoveryCheckpointEnabledTab(_ string, _ bool) {}

// RecoveryCheckpointEnabled is retained for older generated frontends. Auto
// Guard is always built into Auto, so it always reports true.
func (a *App) RecoveryCheckpointEnabled() bool {
	return true
}

// RecoveryCheckpointEnabledTab is the tab-scoped compatibility alias.
func (a *App) RecoveryCheckpointEnabledTab(_ string) bool {
	return true
}

// ReplayPendingPrompts asks every tab's controller to re-emit any approval/ask
// prompt that is currently blocking its run loop. The frontend calls this once
// its event subscription is live (on load/reconnect) so a session that was
// already awaiting confirmation rebuilds its modal instead of showing a
// "waiting" status with no way to answer — and no way to stop.
func (a *App) ReplayPendingPrompts() {
	a.mu.RLock()
	tabs := a.runtimeTabsLocked()
	ctrls := make([]control.SessionAPI, 0, len(tabs))
	for _, t := range tabs {
		if t.Ctrl != nil {
			ctrls = append(ctrls, t.Ctrl)
		}
	}
	a.mu.RUnlock()
	for _, ctrl := range ctrls {
		ctrl.ReplayPendingPrompts()
	}
}
