package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func (g *Gate) askHuman(ctx context.Context, taskID, fp string, gen uint64, proposal Proposal, failure *FailureEvent, diagNotes []string, kind ChangeKind, rationale string) (Decision, error) {
	failureSource := ""
	failureSummary := ""
	if failure != nil {
		failureSource = failure.SourceAgent
		failureSummary = failure.ErrSummary
	}
	pending := PendingProposal{
		Tool:        proposal.Tool,
		Subject:     proposal.Subject,
		Preview:     proposal.Preview,
		Args:        append(json.RawMessage(nil), proposal.Args...),
		Fingerprint: fp,
		SourceAgent: firstNonEmpty(proposal.AgentID, failureSource),
		ChangeKind:  kind,
		Rationale:   firstNonEmpty(rationale, userFacingReason(kind)),
		Diagnosis:   strings.Join(diagNotes, "\n"),
		Failure:     failureSummary,
		Proposed:    firstNonEmpty(proposal.Subject, proposal.Preview, proposal.Tool),
		PlanBefore:  proposal.PlanBefore,
		PlanAfter:   proposal.PlanAfter,
	}

	if g.opts.Headless || g.opts.EmitPrompt == nil {
		return Decision{
			Allow:      false,
			Blocked:    true,
			Message:    headlessBlockerMessage(pending, failure),
			Generation: gen,
		}, nil
	}

	reply := make(chan resolvePayload, 1)
	g.mu.Lock()
	g.metrics.HumanPrompts++
	if st := g.tasks[taskID]; st != nil && st.failureCount() > 1 {
		g.metrics.RepeatPrompts++
	}
	provisional := "pending:" + taskID
	g.waiters[provisional] = reply
	g.taskOf[provisional] = taskID
	g.pending[provisional] = pending
	g.awaiting[taskID] = struct{}{}
	g.mu.Unlock()

	approvalID, err := g.opts.EmitPrompt(ctx, taskID, pending, failure)
	if err != nil {
		g.mu.Lock()
		delete(g.waiters, provisional)
		delete(g.taskOf, provisional)
		delete(g.pending, provisional)
		delete(g.awaiting, taskID)
		g.mu.Unlock()
		return Decision{Allow: false, Blocked: true, Message: "blocked: Auto Guard prompt failed: " + err.Error(), Generation: gen}, err
	}
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		g.mu.Lock()
		delete(g.waiters, provisional)
		delete(g.taskOf, provisional)
		delete(g.pending, provisional)
		delete(g.awaiting, taskID)
		g.mu.Unlock()
		return Decision{Allow: false, Blocked: true, Message: "blocked: Auto Guard prompt returned empty id", Generation: gen}, fmt.Errorf("empty Auto Guard approval id")
	}

	g.mu.Lock()

	if provisionalReply, ok := g.waiters[provisional]; ok && provisionalReply != nil {
		delete(g.waiters, provisional)
		delete(g.taskOf, provisional)
		if p, exists := g.pending[provisional]; exists {
			delete(g.pending, provisional)
			g.pending[approvalID] = p
		}
		if existing, exists := g.waiters[approvalID]; exists && existing != nil {
			reply = existing
		} else {
			reply = provisionalReply
			g.waiters[approvalID] = reply
			g.taskOf[approvalID] = taskID
		}
	} else if existing, ok := g.waiters[approvalID]; ok && existing != nil {
		reply = existing
	}
	g.awaiting[taskID] = struct{}{}
	g.mu.Unlock()
	g.persist()

	select {
	case payload := <-reply:
		decision, err := g.decisionFromResolve(payload)
		if err == nil && decision.Allow && proposal.PlanTransition {
			decision.AuthorizePlanReplacement = true
		}
		decision.Generation = gen
		return decision, err
	case <-ctx.Done():
		g.mu.Lock()
		delete(g.waiters, approvalID)
		delete(g.taskOf, approvalID)
		delete(g.pending, approvalID)
		delete(g.awaiting, taskID)
		g.mu.Unlock()
		g.persist()
		return Decision{Allow: false, Blocked: true, Message: "blocked: Auto Guard confirmation cancelled", Generation: gen}, ctx.Err()
	}
}

func taskGrantScopeKey(proposal Proposal) string {

	taskScope := strings.TrimSpace(proposal.TaskScopeID)
	if taskScope == "" {
		taskScope = strings.TrimSpace(proposal.TaskSummary)
	}
	return CallFingerprint(
		"task-grant",
		normalizeTaskID(proposal.TaskID),
		taskScope,
		nil,
	)
}

func taskGrantRuntimeKey(semanticKey, taskScope string) string {
	if semanticKey == "" || taskScope == "" {
		return ""
	}
	return semanticKey + "#" + taskScope
}

func (g *Gate) decisionFromResolve(payload resolvePayload) (Decision, error) {
	switch payload.action {
	case ActionContinue, ActionContinueTask:
		return Decision{Allow: true}, nil
	case ActionRevise:
		msg := "blocked: user requested a revised Auto Guard action"
		feedback := strings.TrimSpace(payload.feedback)
		if feedback == "" {
			feedback = DefaultReviseFeedback
		}
		msg += ": " + feedback
		return Decision{Allow: false, Blocked: true, Message: msg}, nil
	default:
		return Decision{Allow: false, Blocked: true, Message: "blocked: unknown Auto Guard action"}, nil
	}
}

// RecordDiagnosis appends a diagnosis note while recovering.
func (g *Gate) RecordDiagnosis(taskID, note string) {
	if g == nil || strings.TrimSpace(note) == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	st := g.tasks[normalizeTaskID(taskID)]
	if st == nil || st.lastFailure == nil {
		return
	}
	if appendDiagnosisNote(st.lastFailure, note) {
		g.persistUnlocked()
	}
}

func (g *Gate) activeMode() bool {
	mode := strings.ToLower(strings.TrimSpace(g.opts.Mode()))
	return mode == "auto"
}

func (g *Gate) recoveryGuidanceLocked(st *taskRuntime) string {
	if st.guidanceSent {
		return ""
	}
	st.guidanceSent = true
	if st.lastFailure != nil && st.lastFailure.evidence.Class == FailureClassTransient {
		return "The tool timed out or hit a transient execution limit. Inspect its current state and output before retrying so partial effects are not duplicated. " +
			"Read-only diagnosis and unrelated work remain available without asking the user; retry the exact operation only after ruling out partial effects."
	}
	return "A tool failed. Use read-only diagnosis as needed, continue unrelated work automatically, and do not ask the user unless a genuine product or plan choice is required. " +
		"Repeated retries of the exact failed operation remain bounded."
}

func diagnosticObservationNote(obs Observation) string {
	tool := clip(strings.TrimSpace(obs.Tool), 120)
	if tool == "" {
		tool = "diagnostic"
	}
	subject := clip(firstNonEmpty(obs.Subject, ArgsSummary(obs.Args, 160)), 160)
	header := tool
	if subject != "" && subject != tool {
		header += " (" + subject + ")"
	}
	output := strings.TrimSpace(obs.Output)
	if output == "" {
		return clipDiagnosisNote(header + ": completed successfully")
	}
	return clipDiagnosisNote(header + ": " + output)
}

func (g *Gate) persist() {
	if g == nil || g.opts.Persist == nil {
		return
	}

	g.schedulePersist(g.PersistenceSnapshot(), false)
}

func (g *Gate) persistUnlocked() {

	if g == nil || g.opts.Persist == nil {
		return
	}
	g.schedulePersist(g.snapshotLocked(true), true)
}

func (g *Gate) schedulePersist(snap Snapshot, async bool) {
	if g == nil || g.opts.Persist == nil {
		return
	}
	key := ""
	if g.opts.PersistenceKey != nil {
		key = g.opts.PersistenceKey()
	}
	g.persistMu.Lock()
	g.persistSeq++
	seq := g.persistSeq
	g.persistPending[key]++
	g.persistMu.Unlock()
	write := func() {
		g.persistMu.Lock()
		defer g.persistMu.Unlock()
		defer func() {
			g.persistPending[key]--
			if g.persistPending[key] == 0 {
				delete(g.persistPending, key)
				delete(g.persistDone, key)
				g.persistCond.Broadcast()
			}
		}()
		if seq < g.persistDone[key] {
			return
		}
		g.opts.Persist(key, snap)
		g.persistDone[key] = seq
	}
	if async {
		go write()
		return
	}
	write()
}

// userFacingReason is the short localized-friendly reason shown on the card.
func userFacingReason(kind ChangeKind) string {
	switch kind {
	case ChangeRisk:
		return "This proposal is a technical execution-risk blocker, not a user-owned plan choice."
	case ChangeScope:
		return "This step would expand the change scope."
	case ChangeStrategy:
		return "Auto is about to try a different approach."
	default:
		return "Auto cannot establish how this proposal relates to the active task and plan."
	}
}

func headlessBlockerMessage(pending PendingProposal, failure *FailureEvent) string {
	var b strings.Builder
	b.WriteString("blocked: Auto Guard requires human confirmation, but this environment has no decision channel.\n")
	if failure != nil {
		b.WriteString("Failure: ")
		b.WriteString(firstNonEmpty(failure.ErrSummary, failure.Tool))
		b.WriteString("\n")
	}
	if pending.Diagnosis != "" {
		b.WriteString("Diagnosis: ")
		b.WriteString(pending.Diagnosis)
		b.WriteString("\n")
	}
	b.WriteString("Proposed: ")
	b.WriteString(firstNonEmpty(pending.Proposed, pending.Subject, pending.Tool))
	b.WriteString("\n")
	if pending.Rationale != "" {
		b.WriteString("Why confirm: ")
		b.WriteString(pending.Rationale)
	}
	return b.String()
}
