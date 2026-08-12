package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

// ReadinessResult is the host-consumable outcome of the Delivery final-answer
// readiness check. The Controller reads it after each goal turn; plain turns
// receive the same outcome as a FinalReadinessError.
type ReadinessResult struct {
	// Ready is true when no missing requirement remains.
	Ready bool
	// Missing lists stable category ids of the missing requirements
	// (project_check, todo, criteria, verification, review, signoff, action,
	// mutation, capability). Empty when Ready.
	Missing []string
	// Reason is the user-facing summary of what is still missing.
	Reason string
	// ProgressKey is the host-verifiable progress signature of the current
	// evidence state. Identical ProgressKey across consecutive goal turns
	// means no host-observable progress was made.
	ProgressKey string
}

// ReadinessResult returns the current final-readiness outcome for the host.
func (a *Agent) ReadinessResult() ReadinessResult {
	check := a.finalReadinessCheckFor()
	if check.reason == "" {
		return ReadinessResult{Ready: true, ProgressKey: check.progressSignature()}
	}
	return ReadinessResult{
		Ready:       false,
		Missing:     check.missingIDs(),
		Reason:      check.reason,
		ProgressKey: check.progressSignature(),
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// DeliveryCheckpoint returns the compact Goal-scoped delivery state. It is safe
// to persist next to the Goal sidecar because it contains no raw arguments.
func (a *Agent) DeliveryCheckpoint() evidence.DeliveryCheckpoint {
	return a.task.checkpoint
}

// RestoreDeliveryCheckpoint seeds a rebuilt controller before its next Goal
// run. A mismatched/empty scope is ignored conservatively.
func (a *Agent) RestoreDeliveryCheckpoint(checkpoint evidence.DeliveryCheckpoint) {
	checkpoint.ScopeID = strings.TrimSpace(checkpoint.ScopeID)
	if checkpoint.ScopeID == "" {
		return
	}
	a.task.checkpoint = checkpoint
	a.task.scopeID = checkpoint.ScopeID
}

// PrepareDeliveryRecovery and PrepareDeliveryWaiver keep the exhausted ledger; the waiver stands readiness down.
func (a *Agent) PrepareDeliveryRecovery() bool { return a.prepareDeliveryConsumption(false) }
func (a *Agent) PrepareDeliveryWaiver() bool   { return a.prepareDeliveryConsumption(true) }

func (a *Agent) prepareDeliveryConsumption(waiver bool) bool {
	if !a.pending.deliveryRecovery {
		return false
	}
	a.pending.preserveEvidence, a.pending.deliveryWaiver, a.pending.deliveryRecovery = true, waiver, false
	return true
}

func (a *Agent) updateDeliveryCheckpoint(runErr error) {
	if !a.turn.deliveryScopeActive || a.task.scopeID == "" || a.task.ledger == nil {
		return
	}
	cp := a.task.checkpoint
	if cp.ScopeID != a.task.scopeID {
		cp = evidence.DeliveryCheckpoint{ScopeID: a.task.scopeID}
	}
	cp.CriteriaEstablished = cp.CriteriaEstablished || a.turn.deliveryCriteriaEstablished || a.task.ledger.HasSuccessfulTodoWrite()
	cp.WorkObserved = cp.WorkObserved || a.task.ledger.HasSuccessfulWorkReceipt()
	persistentOnlyReady := a.turn.deliveryPersistentExpected && !a.turn.deliveryMutationExpected &&
		a.task.ledger.HasSuccessfulToolReceipt("remember") && !a.task.ledger.HasSuccessfulMutationOtherThan("remember")
	if _, ok := a.task.ledger.LatestSuccessfulMutationIndex(); ok && !persistentOnlyReady {
		cp.MutationObserved = true
		cp.PendingMutation = true
	}
	if persistentOnlyReady {
		cp.MutationObserved = true
	}
	if runErr == nil && cp.PendingMutation && (a.deliveryMutationCheckpointReady() || a.turn.deliveryWaiverActive) {
		cp.PendingMutation = false
	}
	a.task.checkpoint = cp
}

func (a *Agent) deliveryMutationCheckpointReady() bool {
	if a.task.ledger == nil || !a.turn.deliveryCriteriaEstablished {
		return false
	}
	mutation, ok := a.task.ledger.LatestSuccessfulMutationIndex()
	if !ok {
		mutation = -1
	}
	return a.task.ledger.HasSuccessfulCompleteStepAfter(mutation) &&
		a.task.ledger.HasSuccessfulDeliverySignoffAfter(mutation) &&
		a.task.ledger.HasSuccessfulReviewAfter(mutation) &&
		a.deliveryReviewGateFailure() == ""
}

func (a *Agent) setTodoState(todos []evidence.TodoItem) {
	a.sess.todoMu.Lock()
	a.sess.todoState = evidence.NormalizeSerialTodos(todos)
	a.sess.todoMu.Unlock()
}

func (a *Agent) hasActiveCanonicalTodo() bool {
	a.sess.todoMu.Lock()
	defer a.sess.todoMu.Unlock()
	for _, todo := range a.sess.todoState {
		if canonicalTodoStatus(todo.Status) == "in_progress" {
			return true
		}
	}
	return false
}

func (a *Agent) canonicalTodoProgress() (int, bool) {
	a.sess.todoMu.Lock()
	defer a.sess.todoMu.Unlock()
	completed := 0
	incomplete := false
	for _, todo := range a.sess.todoState {
		status := canonicalTodoStatus(todo.Status)
		if status == "completed" {
			completed++
		} else {
			incomplete = true
		}
	}
	return completed, incomplete
}

// registryHasWriterTools reports whether any registered tool can mutate state.
// A strictly read-only registry (read_only_task / read_only_skill subagents)
// can never satisfy a "state change required" delivery expectation, so that
// expectation must not be armed for it.
func registryHasWriterTools(reg *tool.Registry) bool {
	if reg == nil {
		return false
	}
	for _, name := range reg.Names() {
		if t, ok := reg.Get(name); ok && !t.ReadOnly() {
			return true
		}
	}
	return false
}

// advanceCanonicalTodo flips the canonical todo matching a signed-off step to
// completed (promoting the next pending item to in_progress) and emits a
// synthetic todo_write so the task panel reflects it without the model
// re-sending the whole list. No-op when nothing matches or it is already done.
func (a *Agent) advanceCanonicalTodo(step string) {
	a.sess.todoMu.Lock()
	if len(a.sess.todoState) == 0 {
		a.sess.todoMu.Unlock()
		return
	}
	m, ok := evidence.MatchStep(step, a.sess.todoState)
	if !ok || !evidence.AdvanceSerialTodo(a.sess.todoState, m.Index-1) {
		a.sess.todoMu.Unlock()
		return
	}
	snapshot := append([]evidence.TodoItem(nil), a.sess.todoState...)
	a.sess.todoMu.Unlock()
	a.recordTodoState(snapshot)
	a.emitTodoState(snapshot, m.Index)
}

// emitTodoState emits a synthetic todo_write event so the frontend task panel
// reflects a host-advanced completion without the model re-sending the list.
// itemIndex is the 1-based position of the completed todo in the panel.
func (a *Agent) emitTodoState(todos []evidence.TodoItem, itemIndex int) {
	args, err := json.Marshal(map[string]any{"todos": todos})
	if err != nil {
		return
	}
	id := fmt.Sprintf("host-advance-%d-%d", a.hostAdvanceSeq.Add(1), itemIndex)
	t := event.Tool{ID: id, Name: "todo_write", Args: string(args), ReadOnly: true}
	a.svc.sink.Emit(event.Event{Kind: event.ToolDispatch, Tool: t})
	t.Output = "task list advanced by complete_step"
	a.svc.sink.Emit(event.Event{Kind: event.ToolResult, Tool: t})
}

// RebuildTodoState re-derives canonical task state from the current session
// transcript. Call after externally truncating the session (e.g. after a
// user-cancel strip) so Agent.todoState stays consistent with the messages.
func (a *Agent) RebuildTodoState() {
	a.rebuildTodoState(a.Session().Snapshot())
}

// rebuildTodoState reconstructs the canonical task list from a transcript: the
// latest successful todo_write is the base, then every complete_step after it
// advances an item. Deterministic from persisted messages, so it survives a
// fresh load or a rewind (the truncated history yields the historical state).
// Empty after compaction drops the todo_write — no worse than no canonical list.
func (a *Agent) rebuildTodoState(msgs []provider.Message) {
	successful := successfulToolCallIDs(msgs)
	var todos []evidence.TodoItem
	baseIdx := -1
	for i, msg := range msgs {
		for _, tc := range msg.ToolCalls {
			if tc.Name != "todo_write" || !successful[tc.ID] {
				continue
			}
			rec := evidence.ReceiptFromToolCall(tc.Name, json.RawMessage(tc.Arguments), true, true)

			todos = evidence.NormalizeSerialTodos(rec.Todos)
			baseIdx = i
		}
	}
	if baseIdx < 0 {
		a.setTodoState(nil)
		return
	}
	for i := baseIdx; i < len(msgs); i++ {
		for _, tc := range msgs[i].ToolCalls {
			if tc.Name != "complete_step" || !successful[tc.ID] {
				continue
			}
			rec := evidence.ReceiptFromToolCall(tc.Name, json.RawMessage(tc.Arguments), true, true)
			if m, ok := evidence.MatchStep(rec.Step, todos); ok {
				evidence.AdvanceSerialTodo(todos, m.Index-1)
			}
		}
	}
	a.setTodoState(todos)
}

func successfulToolCallIDs(msgs []provider.Message) map[string]bool {
	successful := map[string]bool{}
	for _, msg := range msgs {
		if msg.Role != provider.RoleTool || msg.ToolCallID == "" {
			continue
		}
		if !toolResultFailed(msg.Content) {
			successful[msg.ToolCallID] = true
		}
	}
	return successful
}

func toolResultFailed(content string) bool {
	content = strings.TrimSpace(content)
	return strings.HasPrefix(content, "error:") ||
		strings.HasPrefix(content, "blocked:") ||
		strings.HasPrefix(content, "Error:") ||
		strings.HasPrefix(content, "[error")
}
