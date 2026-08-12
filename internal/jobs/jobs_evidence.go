package jobs

import (
	"context"
	"strings"

	"reasonix/internal/evidence"
)

type noManager struct{}

// WithManager stamps ctx with the job manager so tools can reach it via
// FromContext. The agent sets this on every tool call's context.
func WithManager(ctx context.Context, m *Manager) context.Context {
	return context.WithValue(ctx, ctxKey{}, m)
}

// WithoutManager shadows an ancestor manager. Child agents without an owned
// Jobs manager must not operate the parent's background jobs by inheritance.
func WithoutManager(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, noManager{})
}

// FromContext returns the job manager set by the agent, if any. ok is false for a
// plain context (headless tests, calls outside the run loop).
func FromContext(ctx context.Context) (*Manager, bool) {
	m, ok := ctx.Value(ctxKey{}).(*Manager)
	return m, ok && m != nil
}

// WithSession stamps ctx with the active parent session ID for session-scoped job
// operations.
func WithSession(ctx context.Context, parentSession string) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, strings.TrimSpace(parentSession))
}

// SessionFromContext returns the active parent session ID for job ownership and
// filtering. Empty means no session scope is available.
func SessionFromContext(ctx context.Context) string {
	session, _ := ctx.Value(sessionCtxKey{}).(string)
	return strings.TrimSpace(session)
}

// PublishEvidence attaches a background agent's host-observed receipts to its
// job. The receipts stay independent of the parent turn ledger until the
// parent collects the terminal result with wait or bash_output.
func PublishEvidence(ctx context.Context, summary evidence.ChildEvidenceSummary) {
	j, _ := ctx.Value(jobCtxKey{}).(*Job)
	if j == nil || len(summary.Receipts) == 0 {
		return
	}
	j.mu.Lock()
	j.evidence.Receipts = append(j.evidence.Receipts, summary.Receipts...)
	j.mu.Unlock()
}

// LeaseEvidenceForSession returns a copy of a terminal job's evidence without
// consuming it. Collection is only provisional: the receipts merge into the
// collecting turn's ledger, but that ledger is discarded if the turn is
// cancelled, errors, or the process exits before the turn commits. Consuming
// here would then lose the mutation for good — the parent's next turn resets its
// ledger and this job would report nothing, so a background change would ship
// unreviewed. The evidence is drained only by CommitEvidenceForSession, which
// the agent calls after the collecting turn passes its delivery gates. A
// committed job returns empty so a re-poll after successful delivery does not
// re-demand review.
func (m *Manager) LeaseEvidenceForSession(parentSession, id string) evidence.ChildEvidenceSummary {
	summary, _ := m.tryLeaseEvidenceForSession(parentSession, id)
	return summary
}

// TryLeaseEvidenceForSession is LeaseEvidenceForSession plus a ready flag that
// separates "terminal evidence available" (possibly empty — a committed job or
// one with no mutations) from "not ready to lease yet": unknown job, still
// running, or killed but its run goroutine has not yet flushed PublishEvidence
// and closed done. KillForSession flips status to Killed synchronously, well
// before the goroutine actually returns, so a bash_output poll that lands in
// that window must not treat the empty read as final. Callers that record a
// lease (collectBackgroundEvidence) must gate on ready so they never note a
// lease before the evidence exists — noting it early would let a later commit
// drain evidence nobody ever merged or reviewed.
func (m *Manager) TryLeaseEvidenceForSession(parentSession, id string) (evidence.ChildEvidenceSummary, bool) {
	return m.tryLeaseEvidenceForSession(parentSession, id)
}

func (m *Manager) tryLeaseEvidenceForSession(parentSession, id string) (evidence.ChildEvidenceSummary, bool) {
	j := m.get(parentSession, id)
	if j == nil {
		return evidence.ChildEvidenceSummary{}, false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	select {
	case <-j.done:
	default:
		return evidence.ChildEvidenceSummary{}, false
	}
	if j.evidenceCommitted {
		return evidence.ChildEvidenceSummary{}, true
	}
	out := make([]evidence.Receipt, len(j.evidence.Receipts))
	copy(out, j.evidence.Receipts)
	return evidence.ChildEvidenceSummary{Receipts: out}, true
}

// PendingEvidenceJobIDsForSession returns the IDs of parentSession's terminal
// jobs that carry uncommitted mutation evidence — a prior turn leased it but
// never delivered (the turn failed or was cancelled, and the next turn's Reset
// wiped it from the per-turn ledger), or the process restarted before any turn
// collected it at all. The agent re-leases these at the start of every turn so
// a turn that never calls wait/bash_output still surfaces the pending mutation
// to its final-readiness checks instead of silently shipping it unreviewed.
func (m *Manager) PendingEvidenceJobIDsForSession(parentSession string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, key := range m.order {
		j := m.jobs[key]
		if j == nil || !sessionMatches(parentSession, j.SessionID) {
			continue
		}
		j.mu.Lock()
		terminal := false
		select {
		case <-j.done:
			terminal = true
		default:
		}
		pending := terminal && !j.evidenceCommitted && len(j.evidence.Receipts) > 0
		j.mu.Unlock()
		if pending {
			ids = append(ids, j.ID)
		}
	}
	return ids
}

// CommitEvidenceForSession permanently consumes a terminal job's evidence after
// the collecting turn has accounted for it (passed final-readiness). It clears
// the in-memory copy and drains the persisted mutation summary so neither a
// same-process re-poll nor a restart resurrects receipts the delivered turn
// already reviewed. Best-effort on the disk rewrite — a failed rewrite merely
// restores the conservative resurrection behavior.
func (m *Manager) CommitEvidenceForSession(parentSession, id string) {
	j := m.get(parentSession, id)
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	select {
	case <-j.done:
	default:
		return
	}
	if j.evidenceCommitted {
		return
	}
	hadEvidence := len(j.evidence.Receipts) > 0
	j.evidenceCommitted = true
	j.evidence = evidence.ChildEvidenceSummary{}
	if hadEvidence {
		if err := m.writeJobMetaLocked(j, j.status); err != nil {
			j.noteArtifactErr("evidence drain: " + err.Error())
		}
	}
}
