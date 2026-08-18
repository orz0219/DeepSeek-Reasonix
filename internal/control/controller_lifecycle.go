package control

import (
	"context"
	"encoding/json"

	"reasonix/internal/agent"
	"reasonix/internal/extension"
	"reasonix/internal/extension/dispatch"
	"reasonix/internal/jobs"
	"reasonix/internal/memory"
	"reasonix/internal/sessiontemp"
	"reasonix/internal/workspacelease"
)

// SessionAuthorizations snapshots this controller's same-session tool
// grants ("Allow for this session") and Plan-mode read-only command trust,
// for carrying into a replacement controller across a rebuild — see
// RestoreSessionAuthorizations.
func (c *Controller) SessionAuthorizations() SessionAuthorizations {
	return c.approval.snapshotSessionAuthorizations()
}

// RestoreSessionAuthorizations re-applies session authorizations captured
// from a prior controller in the same session (see SessionAuthorizations). A
// model/effort/profile switch rebuilds the controller, and without this the
// replacement forgets every grant the user already made this session.
func (c *Controller) RestoreSessionAuthorizations(auth SessionAuthorizations) {
	c.approval.restoreSessionAuthorizations(auth)
}

// ReleaseResources stops plugin subprocesses and releases resources without
// firing SessionEnd. Use it only when replacing the controller for the same
// logical session.
func (c *Controller) ReleaseResources() {
	c.close(false, closeJobsWithGrace)
}

// Close stops plugin subprocesses and releases resources. A session that ever
// started fires SessionEnd so a teardown hook runs.
func (c *Controller) Close() {
	c.close(true, closeJobsWithGrace)
}

// CloseAfterDestroy releases controller resources after the caller has already
// begun session-specific job teardown. It avoids a second synchronous job grace
// wait while still cancelling the manager root and reaping temporary artifacts
// once every job goroutine finally exits.
func (c *Controller) CloseAfterDestroy() {
	c.close(true, closeJobsAsync)
}

type closeJobsMode int

func (c *Controller) close(fireSessionEnd bool, jobsMode closeJobsMode) {

	c.closeOnce.Do(func() {
		c.mu.Lock()
		started := c.startedOnce
		cancel := c.cancel

		c.closed = true
		c.parkedTurns = nil

		c.finishing = false
		if cancel != nil {
			c.canceling = true
		}
		c.mu.Unlock()
		if cancel != nil {

			c.approval.clearAll()
			cancel()
		}
		if fireSessionEnd && started {
			c.consolidateSession(context.Background())
			c.hooks.SessionEnd(context.Background(), "other")
			c.extensionSessionEvent(extension.PointSessionEnd, dispatch.PhaseEnd, c.SessionPath())
		}
		if c.jobs != nil {
			switch jobsMode {
			case closeJobsAsync:
				c.jobs.CloseAsync()
			default:
				c.jobs.Close()
			}
		}
		if c.cleanup != nil {
			c.cleanup()
		}

		if c.sessionTemp != nil {
			c.sessionTemp.Release()
		}
	})
}

// SessionTemp returns the logical-session private temporary directory manager.
// Hot rebuilds pass this to the replacement Controller so the directory survives
// model/settings swaps. Nil only when the Controller was constructed without one
// (should not happen after New).
func (c *Controller) SessionTemp() *sessiontemp.Manager {
	if c == nil {
		return nil
	}
	return c.sessionTemp
}

// rotateSessionTemp advances the private temporary generation so a new logical
// session cannot see the previous session's temporary files. In-flight command
// leases keep the old generation alive until they release.
func (c *Controller) rotateSessionTemp() {
	if c == nil || c.sessionTemp == nil {
		return
	}
	c.sessionTemp.Rotate()
}

// Jobs returns the still-running background jobs for the status bar (nil when
// background jobs are disabled).
func (c *Controller) Jobs() []jobs.View {
	if c.jobs == nil {
		return nil
	}
	return c.jobs.RunningForSession(c.parentSessionID())
}

// KillJob cancels a running background job by ID.
func (c *Controller) KillJob(id string) bool {
	if c.jobs == nil {
		return false
	}
	return c.jobs.Kill(id)
}

// CancelJob stops one background job owned by this controller's session.
func (c *Controller) CancelJob(id string) bool {
	if c.jobs == nil {
		return false
	}
	return c.jobs.KillForSession(c.parentSessionID(), id)
}

// WorkspaceLeaseState reports only whether this controller owns or is waiting
// for the Delivery workspace writer lease. It never exposes filesystem or
// process identity.
func (c *Controller) WorkspaceLeaseState() workspacelease.State {
	return c.workspaceLease.State()
}

// SetToolApprovalMode changes the runtime approval posture for permission-gated
// tools. It does not answer business asks or plan approval. Sub-agents (task,
// writer-capable skill sub-agents, the planner) have no UI to prompt through,
// so this also pushes the mode to the shared headless gate they read from —
// without it, a mode switch (Shift+Tab) would only rebuild the parent
// executor's gate and leave sub-agents pinned to whatever mode was active
// when the session booted.
func (c *Controller) SetToolApprovalMode(mode string) {
	c.ApplyToolApprovalMode(mode)
}

// ApplyToolApprovalMode is SetToolApprovalMode reporting which pending
// approval prompt ids the new posture auto-allowed. Prompts NOT in the
// returned set are still pending here — fresh user decisions (plan, memory,
// sandbox escape) never drain, and auto keeps approvals an allow policy would
// not cover — so a frontend must keep showing them instead of assuming the
// posture switch resolved everything (#6432).
func (c *Controller) ApplyToolApprovalMode(mode string) []string {
	mode = normalizeToolApprovalMode(mode)
	// Capture mode-change recovery dismissals before approval drain so a
	// same-value hydrate/reconcile never rotates Episode state, while a real
	// Auto↔Yolo/Ask switch clears temporary failure/reviewer locks and waiters
	// without auto-approving the original mutation.
	var recoveryDismissed []string
	c.mu.Lock()
	gate := c.recoveryGate
	c.mu.Unlock()
	if gate != nil {
		if ctrl, ok := any(gate).(agent.RecoveryEpisodeControl); ok {

			recoveryDismissed = ctrl.OnModeChange(mode)
		}
	}
	pending := c.approval.setMode(mode)
	if c.subagentGate != nil {
		c.subagentGate.Update(mode)
	}
	c.refreshInteractiveGate()

	for _, id := range recoveryDismissed {
		p := c.approval.resolve(id)
		if p.reply != nil {

			select {
			case p.reply <- approvalReply{allow: false}:
			default:
			}
		}
	}
	drained := make([]string, 0, len(pending))
	for _, p := range pending {
		p.reply <- approvalReply{allow: true}
		drained = append(drained, p.id)
	}
	return drained
}

func (c *Controller) ToolApprovalMode() string {
	return c.approval.mode()
}

// SetAutoApproveTools turns YOLO tool auto-approval on or off for the session:
// while on, every tool approval request is auto-allowed (writers and bash run
// without asking). Ask requests and plan approval still reach the user. Deny
// rules still block. Runtime-only — never written to config.
func (c *Controller) SetAutoApproveTools(on bool) {
	if on {
		c.SetToolApprovalMode(ToolApprovalYolo)
		return
	}
	c.SetToolApprovalMode(ToolApprovalAsk)
}

// SetBypass is the legacy name for SetAutoApproveTools. Keep it for existing
// desktop/serve bindings and CLI code that still uses the bypass wording.
func (c *Controller) SetBypass(on bool) {
	c.SetAutoApproveTools(on)
}

// SetMode applies the Plan workflow flag and tool auto-approval together so a turn
// submitted right after a composer mode switch can't observe a half-applied
// gate. Turning tool auto-approval on drains any pending tool approval.
func (c *Controller) SetMode(plan, autoApproveTools bool) {
	c.ApplyMode(plan, autoApproveTools)
}

// ApplyMode is SetMode reporting which pending approval prompt ids the tool
// approval switch auto-allowed (see ApplyToolApprovalMode).
func (c *Controller) ApplyMode(plan, autoApproveTools bool) []string {
	c.applyPlanMode(plan)
	if autoApproveTools {
		return c.ApplyToolApprovalMode(ToolApprovalYolo)
	}
	return c.ApplyToolApprovalMode(ToolApprovalAsk)
}

// AutoApproveTools reports whether YOLO tool auto-approval is on,
// for status indicators and mode persistence.
func (c *Controller) AutoApproveTools() bool {
	return c.ToolApprovalMode() == ToolApprovalYolo
}

// Bypass is the legacy name for AutoApproveTools.
func (c *Controller) Bypass() bool {
	return c.AutoApproveTools()
}

// QuickAdd appends a one-line note to the doc-memory file for scope (project
// REASONIX.md by default) — the write side of "#<note>". Returns the file written.
func (c *Controller) QuickAdd(scope memory.Scope, note string) (string, error) {
	return c.memory.quickAdd(scope, note)
}

// SaveDoc overwrites a recognized memory doc with body — the save side of the
// desktop panel's in-place editor. Returns the file written.
func (c *Controller) SaveDoc(path, body string) (string, error) {
	return c.memory.saveDoc(path, body)
}

// SaveMemory writes an active auto-memory fact and refreshes the in-session
// snapshot. It is the explicit user-confirmed counterpart to the model-owned
// remember tool, used by management surfaces that preview a candidate first.
func (c *Controller) SaveMemory(m memory.Memory) (string, error) {
	return c.memory.saveMemory(m)
}

// ForgetMemory removes a saved auto-memory by name — the panel/TUI forget action,
// the manual counterpart to the model's `forget` tool.
func (c *Controller) ForgetMemory(name string) error {
	return c.memory.forget(name)
}

// QueueMemory implements memory.Queue: when the model runs the remember/forget
// tool, the tool calls this with a note that rides the next turn so the change
// applies this session without touching the cache-stable prefix. It also
// refreshes the snapshot a memory panel reads.
func (c *Controller) QueueMemory(note string) {
	c.memory.queue(note)
}

// ClaimAutoMemoryWrite consumes the one-shot create-only authorization issued
// by gateApprover for a low-risk project fact.
func (c *Controller) ClaimAutoMemoryWrite(args json.RawMessage) bool {
	return c.memory.claimAutoRemember(args)
}

func (c *Controller) MemoryRevisions(ref string) []memory.Memory {
	return c.memory.revisions(ref)
}

// RestoreMemory restores an older active-memory revision as a new audited
// revision and applies it to the next user turn.
func (c *Controller) RestoreMemory(ref string, revision int) (memory.Memory, error) {
	return c.memory.restore(ref, revision)
}

// RestoreArchivedMemory recovers an archived fact as a new audited revision and
// applies it to the next user turn.
func (c *Controller) RestoreArchivedMemory(archivePath string) (memory.Memory, error) {
	return c.memory.restoreArchived(archivePath)
}

// Memory returns the loaded memory snapshot (nil when memory is disabled), for
// frontends that surface a memory panel or the /memory command. The returned
// *Set is immutable — mutations go through QuickAdd / SaveDoc.
func (c *Controller) Memory() *memory.Set {
	return c.memory.current()
}

const (
	closeJobsWithGrace closeJobsMode = iota
	closeJobsAsync
)
