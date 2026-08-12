package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// BindApprovalID associates a prompt id with the task waiting on it so
// Resolve can find the waiter after EmitPrompt returns. If a provisional
// waiter is parked under pending:<taskID>, it is re-keyed to approvalID.
func (g *Gate) BindApprovalID(taskID, approvalID string) {
	if g == nil {
		return
	}
	taskID = normalizeTaskID(taskID)
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	provisional := "pending:" + taskID
	if ch := g.waiters[provisional]; ch != nil {
		delete(g.waiters, provisional)
		delete(g.taskOf, provisional)
		g.waiters[approvalID] = ch
	}
	if pending, ok := g.pending[provisional]; ok {
		delete(g.pending, provisional)
		g.pending[approvalID] = pending
	}
	g.taskOf[approvalID] = taskID
	g.awaiting[taskID] = struct{}{}
}

// Resolve applies a user decision to a pending Auto Guard approval.
// action is continue|continue_task|revise. For revise, feedback is returned through the
// blocked tool result and the current mutation is refused in the same operation.
func (g *Gate) Resolve(id string, action Action, feedback string) error {
	if g == nil {
		return fmt.Errorf("recovery gate is nil")
	}
	id = strings.TrimSpace(id)
	g.mu.Lock()
	ch := g.waiters[id]
	taskID := g.taskOf[id]
	pending := g.pending[id]
	if taskID == "" {
		g.mu.Unlock()
		return fmt.Errorf("unknown recovery approval %q", id)
	}
	st := g.tasks[taskID]
	rotateEpisode := false
	switch action {
	case ActionContinue, ActionContinueTask:
		if action == ActionContinueTask {
			if pending.TaskGrantKey == "" {
				g.mu.Unlock()
				return fmt.Errorf("recovery approval %q cannot grant similar actions", id)
			}
			if st == nil {
				st = &taskRuntime{episodeID: g.episodeID}
				g.tasks[taskID] = st
			}
			st.useTaskGrantScope(pending.TaskGrantTaskScope)
			st.addTaskGrant(pending.TaskGrantKey)
			g.metrics.TaskGrantContinues++
		}

		g.metrics.HumanContinues++
	case ActionRevise:

		rotateEpisode = true
		g.metrics.HumanRevises++
		if strings.TrimSpace(feedback) == "" {
			feedback = DefaultReviseFeedback
		}
	default:
		g.mu.Unlock()
		return fmt.Errorf("unknown recovery action %q", action)
	}
	delete(g.waiters, id)
	delete(g.taskOf, id)
	delete(g.pending, id)
	delete(g.awaiting, taskID)
	if !rotateEpisode {
		if st == nil || (st.empty() && !st.hasTaskGrants()) {
			delete(g.tasks, taskID)
		}
	}
	g.mu.Unlock()

	if ch != nil {
		select {
		case ch <- resolvePayload{action: action, feedback: feedback}:
		default:
		}
	}
	if rotateEpisode {

		g.BeginEpisode()
	} else {
		g.persist()
	}
	return nil
}

// ObserveResult implements agent.RecoveryGate. It returns one-shot guidance
// for the caller to enqueue on the exact Agent.Run that observed the failure.
func (g *Gate) ObserveResult(_ context.Context, obs Observation) string {
	if g == nil || !g.activeMode() {
		return ""
	}
	taskID := normalizeTaskID(obs.TaskID)

	g.mu.Lock()
	defer g.mu.Unlock()

	if obs.Generation != 0 && obs.Generation != g.generation {
		g.metrics.StaleObservationsIgnored++
		return ""
	}

	st := g.ensureTaskLocked(taskID)

	if obs.Success && obs.Verification {
		g.clearNoProgressLocked(taskID, st)
		g.persistUnlocked()
		return ""
	}

	if obs.Success && obs.Mutates {
		g.clearNoProgressLocked(taskID, st)
		g.persistUnlocked()
		return ""
	}

	if obs.Success {
		if st.lastFailure != nil && IsDiagnosticSuccess(obs) {
			if appendDiagnosisNote(st.lastFailure, diagnosticObservationNote(obs)) {
				g.persistUnlocked()
			}
		}
		return ""
	}
	if !QualifyingFailure(obs) {
		return ""
	}

	fp := observationFingerprint(obs)
	st.ensureMaps()
	st.episodeID = g.episodeID
	if st.operationFailures[fp] < 255 {
		st.operationFailures[fp]++
	}

	if g.episode.totalFailures < 255 {
		g.episode.totalFailures++
	}
	if st.operationFailures[fp] >= MaxOperationFailures {
		st.markOperationStopped(fp)
		g.metrics.OperationStops++
	}
	if g.episode.totalFailures >= MaxEpisodeFailures {
		g.episode.stopped = true
		g.episode.stopReason = StopReasonEpisodeFailures
		g.metrics.EpisodeFailureStops++
	}

	st.lastFailure = &activeFailure{
		evidence: FailureEvent{
			Class:         ClassifyFailure(obs),
			Tool:          obs.Tool,
			ArgsSummary:   ArgsSummary(obs.Args, 200),
			Subject:       obs.Subject,
			ErrSummary:    obs.ErrSummary,
			OutputExcerpt: clip(obs.Output, 1500),
			SourceAgent:   obs.AgentID,
			TaskID:        taskID,
			TaskScopeID:   persistentRecoveryScope(obs.TaskScopeID),
			ReadOnly:      obs.ReadOnly,
			Verification:  obs.Verification,
			Mutates:       obs.Mutates,
			CreatedAt:     g.opts.Now(),
			Args:          append(json.RawMessage(nil), obs.Args...),
			Fingerprint:   fp,
		},
		safeRetryUsed: false,
	}

	g.metrics.FailureEvents++
	guidance := g.recoveryGuidanceLocked(st)
	g.persistUnlocked()
	return guidance
}

// BeforeMutation implements agent.RecoveryGate.
func (g *Gate) BeforeMutation(ctx context.Context, proposal Proposal) (Decision, error) {
	if g == nil {
		return Decision{Allow: true}, nil
	}

	facts, failure, diagNotes, taskID, fp, gen := g.classify(proposal)
	route := Decide(facts)

	if facts.AutoMode && facts.OperationAlreadyStopped && facts.SameFailedOperation {
		dec, escalated := g.noteStoppedOpRetry(taskID, fp, gen, proposal)
		if escalated {
			return dec, nil
		}

		route = DecisionResult{Route: RouteStop, StopReason: StopReasonOperationFailures}
	}

	if facts.AutoMode && facts.EpisodeFailureCount >= MaxEpisodeFailures && (facts.Mutates || facts.Verification) {
		return g.stopTurnDecision(taskID, gen, StopReasonEpisodeFailures, proposal), nil
	}

	switch route.Route {
	case RouteBypass, RouteAllow:
		if route.ConsumeSafeRetry {
			g.mu.Lock()
			if st := g.tasks[taskID]; st != nil && st.lastFailure != nil && !st.lastFailure.safeRetryUsed {
				st.lastFailure.safeRetryUsed = true
				g.metrics.RuleContinues++
			}
			g.mu.Unlock()
			g.persist()
		}
		return Decision{Allow: true, Generation: gen}, nil
	case RouteReview:
		return g.reviewOrEscalate(ctx, taskID, fp, gen, proposal, failure, diagNotes)
	case RouteStop:
		return Decision{
			Allow:      false,
			Blocked:    true,
			Message:    repeatedFailureStopMessage(int(facts.FailureCount), proposal),
			Generation: gen,
			StopReason: string(StopReasonOperationFailures),
		}, nil
	case RouteStopTurn:
		return g.stopTurnDecision(taskID, gen, route.StopReason, proposal), nil
	default:
		return Decision{Allow: true, Generation: gen}, nil
	}
}

func (g *Gate) noteStoppedOpRetry(taskID, _ string, gen uint64, proposal Proposal) (Decision, bool) {
	g.mu.Lock()
	_ = g.ensureTaskLocked(taskID)
	if g.episode.stoppedOpRetries < 255 {
		g.episode.stoppedOpRetries++
	}
	retries := g.episode.stoppedOpRetries
	if retries >= MaxStoppedOperationRetries {
		g.episode.stopped = true
		if g.episode.stopReason == StopReasonNone {
			g.episode.stopReason = StopReasonStoppedOpRetries
		}
		g.metrics.StoppedOpRetryStops++
		g.mu.Unlock()
		g.persist()
		return g.stopTurnDecision(taskID, gen, StopReasonStoppedOpRetries, proposal), true
	}
	g.mu.Unlock()
	g.persist()
	return Decision{}, false
}

func (g *Gate) stopTurnDecision(taskID string, gen uint64, reason StopReason, proposal Proposal) Decision {
	g.mu.Lock()
	_ = g.ensureTaskLocked(taskID)
	g.episode.stopped = true
	if g.episode.stopReason == StopReasonNone {
		g.episode.stopReason = reason
	}
	stopReason := g.episode.stopReason
	g.mu.Unlock()
	g.persist()
	msg := episodeStopMessage(stopReason, proposal)
	return Decision{
		Allow:      false,
		Blocked:    true,
		Message:    msg,
		Generation: gen,
		StopTurn:   true,
		StopReason: string(stopReason),
	}
}

// MarkFinalizationOffered records that the agent was given its one summarize-only
// round after an Episode stop. Subsequent tool proposals while still stopped
// should surface RecoveryPauseError.
func (g *Gate) MarkFinalizationOffered(taskID string) {
	if g == nil {
		return
	}
	_ = taskID
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.episode.stopped {
		g.episode.finalizationOffered = true
	}
}

// ConsumeFinalization reports whether the finalization round already ran and
// the model still attempted tools. Also marks it consumed on first true check
// after offered. Finalization is Episode-scoped (shared by all TaskIDs).
func (g *Gate) ConsumeFinalization(taskID string) (offered, alreadyConsumed bool) {
	if g == nil {
		return false, false
	}
	_ = taskID
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.episode.stopped {
		return false, false
	}
	offered = g.episode.finalizationOffered
	alreadyConsumed = g.episode.finalizationConsumed
	if offered && !alreadyConsumed {
		g.episode.finalizationConsumed = true
	}
	return offered, alreadyConsumed
}

// EpisodeStopped reports whether the shared Recovery Episode is exhausted for
// any TaskID (root or sub-agent).
func (g *Gate) EpisodeStopped(taskID string) bool {
	if g == nil {
		return false
	}
	_ = taskID
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.episode.stopped
}

// clearNoProgressLocked clears Episode totals and the observing task's local
// counters after real mutation/verification success. Caller holds g.mu.
func (g *Gate) clearNoProgressLocked(taskID string, st *taskRuntime) {
	g.episode.clear()
	if st != nil {
		st.clearTaskRecoveryState()
		st.episodeID = g.episodeID
		if !st.hasTaskGrants() {
			delete(g.tasks, taskID)
		}
	}
}

// classify builds pure Facts for Decide. It never calls the model or UI.
func (g *Gate) classify(proposal Proposal) (Facts, *FailureEvent, []string, string, string, uint64) {
	facts := Facts{
		AutoMode:       g.activeMode(),
		ReadOnly:       proposal.ReadOnly,
		Mutates:        proposal.Mutates,
		Verification:   proposal.Verification,
		PlanTransition: proposal.PlanTransition,
	}

	boundary := riskBoundaryForProposal(proposal)
	proposal.HighRisk = boundary.highRisk
	facts.HighRisk = boundary.highRisk

	taskID := normalizeTaskID(proposal.TaskID)

	operationFP := CallFingerprint(proposal.Tool, proposal.Subject, "", proposal.Args)
	approvalFP := CallFingerprint(proposal.Tool, proposal.Subject, proposal.Preview, proposal.Args)

	g.mu.Lock()
	gen := g.generation

	st := g.tasks[taskID]
	var failure *FailureEvent
	var diagNotes []string
	stateChanged := false

	facts.EpisodeStopped = g.episode.stopped
	facts.StopReason = g.episode.stopReason
	facts.EpisodeFailureCount = g.episode.totalFailures
	facts.ReviewRejects = g.episode.reviewRejects
	if st != nil && !facts.AutoMode {
		if !st.empty() || g.episode.totalFailures > 0 || g.episode.reviewRejects > 0 || g.episode.stopped {
			st.clearTaskRecoveryState()
			g.episode.clear()
			stateChanged = true
		}
		if !st.hasTaskGrants() && st.empty() {
			delete(g.tasks, taskID)
			st = nil
		}
	}
	if st != nil {

		if st.episodeID != "" && st.episodeID != g.episodeID {
			grants := st.taskGrants
			grantScope := st.taskGrantScope
			st.clearTaskRecoveryState()
			st.taskGrants = grants
			st.taskGrantScope = grantScope
			st.episodeID = g.episodeID
			stateChanged = true
		} else if st.episodeID == "" {
			st.episodeID = g.episodeID
		}
		facts.OperationAlreadyStopped = st.isOperationStopped(operationFP)
		facts.FailureCount = st.operationFailureCount(operationFP)
		if st.lastFailure != nil {
			failure = st.evidenceCopy()
			diagNotes = st.diagnosisNotes()
			facts.HasActiveFailure = true
			facts.SameFailedOperation = sameFailedOperation(failure, proposal)

			if facts.SameFailedOperation && facts.FailureCount == 0 {

				facts.FailureCount = 1
			}
			if IsSafeVerificationRetry(failure, proposal) && st.safeRetryAvailable() {
				facts.SafeRetryAvailable = true
			}
		}
		taskScope := taskGrantScopeKey(proposal)
		st.useTaskGrantScope(taskScope)
		if st.empty() && !st.hasTaskGrants() {
			delete(g.tasks, taskID)
			st = nil
		}
		runtimeGrantKey := taskGrantRuntimeKey(boundary.taskGrantKey, taskScope)
		if facts.HighRisk && runtimeGrantKey != "" && st != nil && st.hasTaskGrant(runtimeGrantKey) {
			facts.HighRisk = false
			g.metrics.TaskGrantUses++
		}
	}
	g.mu.Unlock()
	if stateChanged {
		g.persist()
	}

	if failure != nil {
		if !proposal.ExpandedScope {
			proposal.ExpandedScope = ScopeExpanded(failure, proposal)
		}
		if !proposal.StrategyChanged {
			proposal.StrategyChanged = StrategyChanged(failure, proposal)
		}
		facts.ExpandedScope = proposal.ExpandedScope
		facts.StrategyChanged = proposal.StrategyChanged
		if facts.SafeRetryAvailable && (facts.ExpandedScope || facts.StrategyChanged || facts.HighRisk) {
			facts.SafeRetryAvailable = false
		}
	}
	return facts, failure, diagNotes, taskID, approvalFP, gen
}

func (g *Gate) ensureTaskLocked(taskID string) *taskRuntime {
	st := g.tasks[taskID]
	if st == nil {
		st = &taskRuntime{episodeID: g.episodeID}
		g.tasks[taskID] = st
	}
	if st.episodeID == "" {
		st.episodeID = g.episodeID
	}
	return st
}

func (g *Gate) reviewOrEscalate(ctx context.Context, taskID, fp string, gen uint64, proposal Proposal, failure *FailureEvent, diagNotes []string) (Decision, error) {

	g.mu.Lock()
	if g.episode.reviewRejects >= uint8(g.opts.MaxReviewBlocks) {
		g.episode.stopped = true
		if g.episode.stopReason == StopReasonNone {
			g.episode.stopReason = StopReasonReviewRejects
		}
		g.metrics.ReviewStops++
		g.mu.Unlock()
		return g.stopTurnDecision(taskID, gen, StopReasonReviewRejects, proposal), nil
	}
	g.mu.Unlock()

	var verdict ReviewVerdict
	if g.opts.Reviewer != nil {
		start := g.opts.Now()
		taskSummary := strings.TrimSpace(proposal.TaskSummary)
		if taskSummary == "" && g.opts.TaskSummary != nil {
			taskSummary = g.opts.TaskSummary()
		}
		v, err := g.opts.Reviewer.Review(ctx, failure, diagNotes, proposal, taskSummary)
		latency := g.opts.Now().Sub(start).Milliseconds()
		g.mu.Lock()
		g.metrics.ReviewLatencyMsSum += latency
		g.metrics.ReviewLatencyCount++
		if err != nil {
			g.metrics.ReviewErrors++
		}
		g.mu.Unlock()
		if err != nil {
			if proposal.PlanTransition {
				return g.askHuman(ctx, taskID, fp, gen, proposal, failure, diagNotes, ChangeScope,
					"The active execution plan changed, but the independent plan reviewer is unavailable.")
			}
			g.mu.Lock()
			g.metrics.RuleContinues++
			g.mu.Unlock()
			return Decision{Allow: true, Generation: gen}, nil
		}
		verdict = normalizeVerdict(v, failure, proposal, diagNotes)
		if verdict.Outcome == ReviewContinue && reviewerContinueKind(verdict.ChangeKind) {

			g.mu.Lock()
			g.metrics.ReviewContinues++
			g.mu.Unlock()
			return Decision{
				Allow:                    true,
				AuthorizePlanReplacement: proposal.PlanTransition,
				Generation:               gen,
			}, nil
		}
		if proposal.PlanTransition && reviewerPlanDecision(verdict) {
			return g.askHuman(ctx, taskID, fp, gen, proposal, failure, diagNotes, verdict.ChangeKind, verdict.Rationale)
		}
		blocks := g.recordReviewBlock(taskID, verdict)
		if blocks < g.opts.MaxReviewBlocks {
			return Decision{
				Allow:      false,
				Blocked:    true,
				Message:    reviewerBlockerMessage(verdict, blocks, g.opts.MaxReviewBlocks),
				Generation: gen,
			}, nil
		}
		g.mu.Lock()
		g.episode.stopped = true
		if g.episode.stopReason == StopReasonNone {
			g.episode.stopReason = StopReasonReviewRejects
		}
		g.metrics.ReviewStops++
		g.mu.Unlock()
		return g.stopTurnDecision(taskID, gen, StopReasonReviewRejects, proposal), nil
	}
	if proposal.PlanTransition {
		return g.askHuman(ctx, taskID, fp, gen, proposal, failure, diagNotes, ChangeScope,
			"The active execution plan changed and needs your choice because no independent plan reviewer is configured.")
	}
	g.mu.Lock()
	g.metrics.RuleContinues++
	g.mu.Unlock()
	return Decision{Allow: true, Generation: gen}, nil
}
