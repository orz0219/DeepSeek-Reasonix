package recovery

import (
	"fmt"
	"strings"
)

func (g *Gate) recordReviewBlock(taskID string, verdict ReviewVerdict) int {
	g.mu.Lock()
	st := g.ensureTaskLocked(taskID)

	if g.episode.reviewRejects < 255 {
		g.episode.reviewRejects++
	}
	blocks := int(g.episode.reviewRejects)
	if st.lastFailure != nil {
		note := "Auto Guard reviewer blocked the proposal: " + firstNonEmpty(verdict.Rationale, string(verdict.ChangeKind))
		appendDiagnosisNote(st.lastFailure, note)
	}
	g.mu.Unlock()
	g.persist()
	return blocks
}

func reviewerBlockerMessage(verdict ReviewVerdict, attempt, limit int) string {
	reason := firstNonEmpty(verdict.Rationale, "the proposal could not be classified as a bounded plan continuation")
	return fmt.Sprintf(
		"blocked: Auto plan reviewer could not accept this transition (attempt %d/%d): %s. Continue the current plan, propose a task-aligned plan, or ask the user about a genuine product choice.",
		attempt, limit, reason,
	)
}

func repeatedFailureStopMessage(failures int, proposal Proposal) string {
	operation := clip(firstNonEmpty(proposal.Subject, proposal.Tool), 240)
	return fmt.Sprintf(
		"blocked: Auto stopped repeating this operation after %d consecutive failures: %s. Do not retry the same operation in this turn. Diagnose it with read-only tools, then use a different task-aligned edit or verification approach; other operations remain available. Ask the user only for a genuine product or plan choice.",
		failures, operation,
	)
}

func episodeStopMessage(reason StopReason, proposal Proposal) string {
	operation := clip(firstNonEmpty(proposal.Subject, proposal.Tool), 240)
	switch reason {
	case StopReasonReviewRejects:
		return fmt.Sprintf(
			"blocked: Auto recovery paused this turn after %d reviewer rejections (last proposal: %s). Do not call more tools; summarize what was tried and what remains. The user can continue in the next message.",
			MaxReviewRejects, operation,
		)
	case StopReasonStoppedOpRetries:
		return fmt.Sprintf(
			"blocked: Auto recovery paused this turn after repeated attempts of already-stopped operations (last: %s). Do not call more tools; summarize completed work and blockers. The user can continue in the next message.",
			operation,
		)
	default:
		return fmt.Sprintf(
			"blocked: Auto recovery paused this turn after %d execution failures without progress (last: %s). Do not call more tools; summarize completed work and blockers. The user can continue in the next message.",
			MaxEpisodeFailures, operation,
		)
	}
}

func observationFingerprint(obs Observation) string {
	return CallFingerprint(obs.Tool, obs.Subject, "", obs.Args)
}

func persistentRecoveryScope(scope string) string {
	scope = strings.TrimSpace(scope)
	if strings.HasPrefix(scope, "goal:") {
		return scope
	}
	return ""
}

func sameFailedOperation(failure *FailureEvent, proposal Proposal) bool {
	if failure == nil {
		return false
	}
	want := strings.TrimSpace(failure.Fingerprint)
	if want == "" {
		want = CallFingerprint(failure.Tool, failure.Subject, "", failure.Args)
	}
	return want == CallFingerprint(proposal.Tool, proposal.Subject, "", proposal.Args)
}

func normalizeVerdict(v ReviewVerdict, failure *FailureEvent, proposal Proposal, diagNotes []string) ReviewVerdict {
	switch strings.ToLower(strings.TrimSpace(string(v.Outcome))) {
	case "continue":
		v.Outcome = ReviewContinue
	case "confirm":
		v.Outcome = ReviewConfirm
	default:

		v.Outcome = ReviewConfirm
		if v.ChangeKind == "" {
			v.ChangeKind = ChangeUncertain
		}
	}
	switch ChangeKind(strings.ToLower(strings.TrimSpace(string(v.ChangeKind)))) {
	case ChangeSameStrategy, ChangeStrategy, ChangeScope, ChangeRisk, ChangeUncertain:
		v.ChangeKind = ChangeKind(strings.ToLower(strings.TrimSpace(string(v.ChangeKind))))
	default:
		if v.Outcome == ReviewContinue {

			v.Outcome = ReviewConfirm
		}
		v.ChangeKind = ChangeUncertain
	}

	if v.Outcome == ReviewContinue && !reviewerContinueKind(v.ChangeKind) {
		v.Outcome = ReviewConfirm
	}
	if strings.TrimSpace(v.FailureSummary) == "" && failure != nil {
		v.FailureSummary = failure.ErrSummary
	}
	if strings.TrimSpace(v.Diagnosis) == "" {
		v.Diagnosis = strings.Join(diagNotes, "\n")
	}
	if strings.TrimSpace(v.ProposedAction) == "" {
		v.ProposedAction = firstNonEmpty(proposal.Subject, proposal.Preview, proposal.Tool)
	}
	if strings.TrimSpace(v.Rationale) == "" {
		v.Rationale = userFacingReason(v.ChangeKind)
	} else {
		v.Rationale = clip(v.Rationale, 500)
	}
	return v
}

func reviewerContinueKind(kind ChangeKind) bool {
	switch kind {
	case ChangeSameStrategy, ChangeStrategy, ChangeScope:
		return true
	default:
		return false
	}
}

func reviewerPlanDecision(verdict ReviewVerdict) bool {
	if verdict.Outcome != ReviewConfirm {
		return false
	}
	switch verdict.ChangeKind {
	case ChangeStrategy, ChangeScope:
		return true
	default:
		return false
	}
}
