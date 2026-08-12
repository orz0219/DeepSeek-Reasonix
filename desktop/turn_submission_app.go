package main

import (
	"errors"
	"fmt"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
)

type turnSubmissionState struct {
	inFlight     bool
	submissionID string
}

// setBinding reroutes the sink while invalidating correlations that were
// created for a different frontend tab.
func (s *tabEventSink) setBinding(tabID string, app *App) {
	s.mu.Lock()
	if s.tabID != tabID {
		s.turn.submissionID = ""
	}
	s.tabID = tabID
	if app != nil {
		s.app = app
	}
	s.mu.Unlock()
}

func (s *tabEventSink) setRuntimeEpoch(epoch string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.runtimeEpoch != epoch {
		s.turn.submissionID = ""
	}
	s.runtimeEpoch = epoch
	s.mu.Unlock()
}

func (s *tabEventSink) clearContext() {
	s.mu.Lock()
	s.ctx = nil
	s.turn.submissionID = ""
	s.mu.Unlock()
	s.runtimeEvents.Clear()
}

func firstSubmissionID(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (s *tabEventSink) submissionIDSnapshot() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.turn.submissionID
}

type correlatedWireEventTab struct {
	wireEventTab
	SubmissionID string `json:"submissionId,omitempty"`
}

func toWireTabWithSubmission(e event.Event, tabID, runtimeEpoch, submissionID string) any {
	wire := toWireTab(e, tabID, runtimeEpoch)
	if submissionID == "" {
		return wire
	}
	return correlatedWireEventTab{wireEventTab: wire, SubmissionID: submissionID}
}

// The WithID entry points correlate one optimistic desktop user item with the
// raw TurnDone produced by the turn that this call actually admits.
func (a *App) SubmitToTabWithID(tabID, input, submissionID string) error {
	if err := validateTurnInput(input); err != nil {
		return err
	}
	return a.submitToTab(tabID, input, false, submissionID)
}

func (a *App) SubmitDisplayToTabWithID(tabID, display, input, submissionID string) error {
	return a.submitDisplayToTab(tabID, display, input, submissionID)
}

func (a *App) submitDisplayToTab(tabID, display, input, submissionID string) error {
	if err := validateTurnInput(input); err != nil {
		return err
	}
	admission, ctrl, err := a.beginTabTurn(tabID, true, submissionID)
	if err != nil {
		return err
	}
	defer admission.abort()
	tab := admission.tab
	a.ensureTabTopicIndexedForUserTurn(tab)
	ctrl.SubmitDisplay(display, input)
	admission.finish(ctrl)
	return nil
}

func (a *App) SubmitDeliveryRecoveryToTabWithID(tabID, display, input, submissionID string) error {
	return a.submitDeliveryRecoveryToTab(tabID, display, input, submissionID)
}

func (a *App) submitDeliveryRecoveryToTab(tabID, display, input, submissionID string) error {
	if err := validateTurnInput(input); err != nil {
		return err
	}
	admission, ctrl, err := a.beginTabTurn(tabID, true, submissionID)
	if err != nil {
		return err
	}
	defer admission.abort()
	tab := admission.tab
	a.ensureTabTopicIndexedForUserTurn(tab)
	ctrl.SubmitDeliveryRecovery(display, input)
	admission.finish(ctrl)
	return nil
}

func (a *App) SubmitDeliveryWaiverToTabWithID(tabID, display, input, submissionID string) error {
	return a.submitDeliveryWaiverToTab(tabID, display, input, submissionID)
}

func (a *App) submitDeliveryWaiverToTab(tabID, display, input, submissionID string) error {
	if err := validateTurnInput(input); err != nil {
		return err
	}
	admission, ctrl, err := a.beginTabTurn(tabID, true, submissionID)
	if err != nil {
		return err
	}
	defer admission.abort()
	tab := admission.tab
	a.ensureTabTopicIndexedForUserTurn(tab)
	ctrl.SubmitDeliveryWaiver(display, input)
	admission.finish(ctrl)
	return nil
}

func (a *App) SubmitInvocationsToTabWithID(tabID, display, input string, invocations []InvocationRequest, submissionID string) error {
	return a.submitInvocationsToTab(tabID, display, input, invocations, submissionID)
}

func (a *App) submitInvocationsToTab(tabID, display, input string, invocations []InvocationRequest, submissionID string) error {
	if err := validateInvocationTurnInput(input, invocations); err != nil {
		return err
	}
	admission, ctrl, err := a.beginTabTurn(tabID, true, submissionID)
	if err != nil {
		return err
	}
	defer admission.abort()
	tab := admission.tab
	a.ensureTabTopicIndexedForUserTurn(tab)
	ctrl.SubmitInvocationDisplay(display, input, controlInvocationRequests(invocations))
	admission.finish(ctrl)
	return nil
}

func (a *App) SubmitInitialGoalToTabWithID(
	tabID, goal, display, input string,
	invocations []InvocationRequest,
	collaborationMode, toolApprovalMode, submissionID string,
) ([]string, error) {
	if err := validateInvocationTurnInput(input, invocations); err != nil {
		return []string{}, err
	}
	return a.submitInitialGoalToLocalTab(
		tabID, toolApprovalMode, goal, display, input, invocations, submissionID,
	)
}

func (a *App) SubmitEditedDisplayToTabWithID(tabID, display, input, original, submissionID string) error {
	return a.submitEditedDisplayToTab(tabID, display, input, original, submissionID)
}

func (a *App) submitEditedDisplayToTab(tabID, display, input, original, submissionID string) error {
	if err := validateTurnInput(input); err != nil {
		return err
	}
	admission, ctrl, err := a.beginTabTurn(tabID, true, submissionID)
	if err != nil {
		return err
	}
	defer admission.abort()
	tab := admission.tab
	a.ensureTabTopicIndexedForUserTurn(tab)
	ctrl.SubmitEditedDisplay(display, input, original)
	admission.finish(ctrl)
	return nil
}

// Submit runs raw user input as a turn; slash commands and @-references are
// resolved by the controller. Output arrives asynchronously on eventChannel.
func (a *App) Submit(input string) error {
	return a.SubmitToTab("", input)
}

var errEmptyTurnInput = errors.New("message cannot be empty")

func validateTurnInput(input string) error {
	if strings.TrimSpace(input) == "" {
		return errEmptyTurnInput
	}
	return nil
}

func (a *App) SubmitToTab(tabID, input string) error {
	if err := validateTurnInput(input); err != nil {
		return err
	}
	return a.submitToTab(tabID, input, false)
}

// tabTurnAdmission owns both locks acquired while a foreground turn starts.
// Call finish after invoking the controller; callers should also defer abort so
// every early-return and recovered-panic path releases the admission exactly
// once.
type tabTurnAdmission struct {
	app      *App
	tab      *WorkspaceTab
	released bool
}

func (admission *tabTurnAdmission) finish(ctrl control.SessionAPI) bool {
	if admission == nil || admission.released {
		return false
	}
	admission.released = true
	tab := admission.tab
	if tab != nil {

		defer admission.app.runtimeAdmissionMu.RUnlock()
		defer tab.turnStartMu.Unlock()
	}
	started := ctrl != nil && ctrl.RuntimeStatus().Running
	if !started && tab != nil && tab.sink != nil {
		tab.sink.cancelTurnStart()
	}
	return started
}

func (admission *tabTurnAdmission) abort() {
	admission.finish(nil)
}

// beginTabTurn locks the tab's foreground-turn admission gate and reserves the
// event sink until TurnDone has completed all of its fan-out.
func (a *App) beginTabTurn(tabID string, reclaim bool, submissionID ...string) (*tabTurnAdmission, control.SessionAPI, error) {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return nil, nil, readOnlyChannelErr()
	}
	if err := a.workspaceRuntimeAdmissionErr(tab, ctrl); err != nil {
		return nil, nil, a.workspaceNotReadyErr(tab)
	}

	if err := a.ensureTabControllerWorkspace(tab); err != nil {
		return nil, nil, err
	}

	a.runtimeAdmissionMu.RLock()
	abort := func() {
		tab.turnStartMu.Unlock()
		a.runtimeAdmissionMu.RUnlock()
	}
	tab.turnStartMu.Lock()
	if a.tabIsReadOnly(tab) {
		abort()
		return nil, nil, readOnlyChannelErr()
	}
	if reclaim && a.botBridge != nil {
		a.botBridge.reclaimFromDesktop(tab.ID)
	}
	ctrl = a.controllerForTab(tab)
	if err := a.workspaceRuntimeAdmissionErr(tab, ctrl); err != nil {
		abort()
		return nil, nil, err
	}
	ctrl = a.controllerForTab(tab)
	if err := a.workspaceRuntimeAdmissionErr(tab, ctrl); err != nil {
		abort()
		return nil, nil, err
	}
	if ctrl.RuntimeStatus().Running || (tab.sink != nil && !tab.sink.tryBeginTurn(submissionID...)) {
		abort()
		return nil, nil, control.ErrTurnRunning
	}
	return &tabTurnAdmission{app: a, tab: tab}, ctrl, nil
}

// submitToTab is the shared submit body. fromBridge marks submissions driven
// by the IM takeover bridge; local (frontend) submissions on a taken-over tab
// reclaim remote control first — typing locally is the grab-back gesture.
func (a *App) submitToTab(tabID, input string, fromBridge bool, submissionID ...string) error {
	trimmed := strings.TrimSpace(input)
	if trimmed == "/effort" || strings.HasPrefix(trimmed, "/effort ") {
		tab, _ := a.tabAndCtrlByID(tabID)
		if a.tabIsReadOnly(tab) {
			return readOnlyChannelErr()
		}
		if tab == nil {
			return a.workspaceNotReadyErr(tab)
		}
		if !fromBridge && a.botBridge != nil {
			a.botBridge.reclaimFromDesktop(tab.ID)
		}
		a.runEffortCommandForTab(tabID, trimmed)
		return nil
	}
	admission, ctrl, err := a.beginTabTurn(tabID, !fromBridge, submissionID...)
	if err != nil {
		return err
	}
	defer admission.abort()
	tab := admission.tab
	a.ensureTabTopicIndexedForUserTurn(tab)
	ctrl.SubmitDisplay(input, input)
	admission.finish(ctrl)
	return nil
}

func (a *App) submitUserTurnToTabWithSink(tabID, input string, forwarder event.Sink) bool {
	admission, ctrl, err := a.beginTabTurn(tabID, false)
	if err != nil {
		return false
	}
	defer admission.abort()
	tab := admission.tab
	var generation uint64
	if forwarder != nil {
		generation = tab.sink.SetBotSink(forwarder)
	}
	a.ensureTabTopicIndexedForUserTurn(tab)
	ctrl.SubmitUserTurn(input, input)
	started := admission.finish(ctrl)
	if !started && forwarder != nil {
		tab.sink.clearBotSink(generation)
	}
	return started
}

// RunShell executes a shell command directly (bypassing the model) and streams
// output as events on eventChannel.
func (a *App) RunShell(command string) error {
	return a.RunShellForTab("", command)
}

func (a *App) RunShellForTab(tabID, command string) error {
	admission, ctrl, err := a.beginTabTurn(tabID, true)
	if err != nil {
		return err
	}
	defer admission.abort()
	tab := admission.tab
	a.ensureTabTopicIndexedForUserTurn(tab)
	ctrl.RunShell(command)
	admission.finish(ctrl)
	return nil
}

// SubmitDisplay runs input as a turn while recording a shorter UI-only display
// string for the saved desktop transcript. The model still receives input.
func (a *App) SubmitDisplay(display, input string) error {
	return a.SubmitDisplayToTab("", display, input)
}

func (a *App) SubmitDisplayToTab(tabID, display, input string) error {
	return a.submitDisplayToTab(tabID, display, input, "")
}

func (a *App) SubmitDeliveryRecoveryToTab(tabID, display, input string) error {
	return a.submitDeliveryRecoveryToTab(tabID, display, input, "")
}

// InvocationRequest is the Wails-bound form of a composer invocation entity.
type InvocationRequest struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Offset int    `json:"offset"`
}

func controlInvocationRequests(invocations []InvocationRequest) []control.InvocationRequest {
	out := make([]control.InvocationRequest, 0, len(invocations))
	for _, invocation := range invocations {
		out = append(out, control.InvocationRequest{
			Name: invocation.Name, Kind: invocation.Kind, Offset: invocation.Offset,
		})
	}
	return out
}

func (a *App) SubmitInvocationsToTab(tabID, display, input string, invocations []InvocationRequest) error {
	return a.submitInvocationsToTab(tabID, display, input, invocations, "")
}

func validateInvocationTurnInput(input string, invocations []InvocationRequest) error {

	if len(invocations) > 0 {
		return nil
	}
	return validateTurnInput(input)
}

func (a *App) submitInitialGoalToLocalTab(
	tabID, toolApprovalMode, goal, display, input string,
	invocations []InvocationRequest,
	submissionID ...string,
) ([]string, error) {
	admission, ctrl, err := a.beginTabTurn(tabID, true, submissionID...)
	if err != nil {
		return []string{}, err
	}
	defer admission.abort()

	tab := admission.tab
	toolApprovalMode = normalizeToolApprovalMode(toolApprovalMode)
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return []string{}, fmt.Errorf("goal is required")
	}
	a.mu.Lock()
	if a.tabs[tab.ID] != tab {
		a.mu.Unlock()
		return []string{}, a.workspaceNotReadyErr(nil)
	}
	tab.toolApprovalMode = toolApprovalMode
	tab.goal = goal
	tab.mode = tabModeFromAxes(false, toolApprovalMode == control.ToolApprovalYolo)
	a.saveTabsLocked()
	a.mu.Unlock()

	ctrl.SetPlanMode(false)
	drained := applyTabToolApprovalModeToController(ctrl, toolApprovalMode)
	syncTabGoalToController(ctrl, goal)
	a.ensureTabTopicIndexedForUserTurn(tab)
	if len(invocations) > 0 {
		ctrl.SubmitInvocationDisplay(display, input, controlInvocationRequests(invocations))
	} else {
		ctrl.SubmitDisplay(display, input)
	}
	admission.finish(ctrl)
	return drained, nil
}

// SubmitInitialGoalToTab activates a Goal and submits its first turn on the
// requested tab.
func (a *App) SubmitInitialGoalToTab(
	tabID, goal, display, input string,
	invocations []InvocationRequest,
	collaborationMode, toolApprovalMode string,
) ([]string, error) {
	if err := validateInvocationTurnInput(input, invocations); err != nil {
		return []string{}, err
	}
	return a.submitInitialGoalToLocalTab(
		tabID, toolApprovalMode, goal, display, input, invocations,
	)
}

func (a *App) SubmitEditedDisplayToTab(tabID, display, input, original string) error {
	return a.submitEditedDisplayToTab(tabID, display, input, original, "")
}

func (a *App) bindControllerDisplayRecorder(ctrl control.SessionAPI) {
	if ctrl == nil {
		return
	}
	ctrl.SetDisplayRecorder(func(content, display string) {
		dir := ctrl.SessionDir()
		if dir == "" {
			dir = config.SessionDir()
		}
		_ = recordSessionDisplay(dir, ctrl.SessionPath(), content, display)
	})
}

// Cancel aborts the in-flight turn.
func (a *App) Cancel() {
	a.CancelTab("")
}

func (a *App) CancelTab(tabID string) {
	if ctrl := a.ctrlByTabID(tabID); ctrl != nil {
		ctrl.Cancel()
	}
}

// Steer sends mid-turn guidance to the agent without interrupting the in-flight request.
func (a *App) Steer(text string) error {
	return a.SteerForTab("", text)
}

// SteerForTab sends mid-turn guidance to a specific tab's active agent turn.
// A rejected steer is returned to the frontend so its guidance shelf retains
// the text and submits it as a regular follow-up after the turn completes.
func (a *App) SteerForTab(tabID, text string) error {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return readOnlyChannelErr()
	}
	if ctrl == nil {
		return a.workspaceNotReadyErr(tab)
	}
	if err := a.ensureTabControllerWorkspace(tab); err != nil {
		return err
	}
	ctrl = a.controllerForTab(tab)
	if ctrl == nil {
		return a.workspaceNotReadyErr(tab)
	}
	steerer, ok := ctrl.(interface{ TrySteer(string) bool })
	if !ok {
		return fmt.Errorf("this runtime cannot accept mid-turn guidance")
	}
	if !steerer.TrySteer(text) {
		return fmt.Errorf("the turn ended before guidance could be applied; it will remain queued for the next turn")
	}
	return nil
}

func (a *App) tabAndCtrlByID(tabID string) (*WorkspaceTab, control.SessionAPI) {
	a.mu.RLock()
	tab := a.tabByIDLocked(tabID)
	if tab == nil {
		a.mu.RUnlock()
		return nil, nil
	}
	ctrl := tab.Ctrl
	retryStartup := ctrl == nil && tab.StartupErrLeaseHeld
	a.mu.RUnlock()
	if retryStartup && a.tryRecoverStartupLeaseHeldTab(tab) {
		a.mu.RLock()
		defer a.mu.RUnlock()
		if a.tabs[tab.ID] != tab {
			return nil, nil
		}
		return tab, tab.Ctrl
	}
	return tab, ctrl
}

// activeTabAndCtrl snapshots the active tab and its controller in one locked
// read, so callers never do a check-then-use on tab.Ctrl after the lock is
// released (a rebuild can swap the controller in between).
func (a *App) activeTabAndCtrl() (*WorkspaceTab, control.SessionAPI) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	tab := a.activeTabLocked()
	if tab == nil {
		return nil, nil
	}
	return tab, tab.Ctrl
}

// activeMCPRuntime snapshots the complete target of a Wails MCP action in one
// critical section. MCP operations may outlive a frontend tab switch; carrying
// the invoking workspace root prevents config/authorization reads from drifting to the
// newly active tab while controller calls still target the original runtime.
func (a *App) activeMCPRuntime() (*WorkspaceTab, control.SessionAPI, string) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	tab := a.activeTabLocked()
	if tab == nil {
		return nil, nil, ""
	}
	return tab, tab.Ctrl, tab.WorkspaceRoot
}
