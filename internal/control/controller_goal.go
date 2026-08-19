package control

import (
	"context"
	"errors"
	"fmt"

	"reasonix/internal/config"
	"reasonix/internal/extension"
	"reasonix/internal/extension/dispatch"
	"reasonix/internal/goaleval"
	"reasonix/internal/i18n"
)

func (c *Controller) applyPlanMode(v bool) {
	c.mu.Lock()
	c.planMode = v
	c.mu.Unlock()
	if setter, ok := c.runner.(interface{ SetPlanMode(bool) }); ok {
		setter.SetPlanMode(v)
		return
	}
	if c.executor != nil {
		c.executor.SetPlanMode(v)
	}
}

// SetResponseLanguage updates the final-answer language preference for
// subsequent turns.
func (c *Controller) SetResponseLanguage(lang string) {
	mode := config.NormalizeLanguage(lang)
	c.mu.Lock()
	c.responseLanguage = mode
	c.mu.Unlock()
	if setter, ok := c.runner.(interface{ SetResponseLanguage(string) }); ok {
		setter.SetResponseLanguage(mode)
	} else if c.executor != nil {
		c.executor.SetResponseLanguage(mode)
	}
}

// SetReasoningLanguage updates the visible reasoning language preference for
// subsequent turns.
func (c *Controller) SetReasoningLanguage(lang string) {
	mode := config.NormalizeReasoningLanguage(lang)
	c.mu.Lock()
	c.reasoningLanguage = mode
	c.mu.Unlock()
	if setter, ok := c.runner.(interface{ SetReasoningLanguage(string) }); ok {
		setter.SetReasoningLanguage(mode)
	} else if c.executor != nil {
		c.executor.SetReasoningLanguage(mode)
	}
}

// PlanMode reports whether outgoing turns currently receive the plan-mode
// marker.
func (c *Controller) PlanMode() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.planMode
}

// GoalStrict enables or disables strict goal mode. Since the structured
// protocol, every complete claim is validated against host readiness and an
// incomplete-todo intercept can never be overridden, so the flag is persisted
// for compatibility with older frontends but no longer changes FSM behavior.
func (c *Controller) GoalStrict(strict bool) {
	path, data, ok := c.goals.setStrict(strict, c.goalTodos())
	c.persistGoalState(path, data, ok)
}

// SetGoal stores a session-scoped active goal. Compose injects it into outgoing
// user turns, not the system prompt or tool schema, so it does not disturb the
// cache-stable prefix.
func (c *Controller) SetGoal(goal string) {
	c.SetGoalWithResearchMode(goal, GoalResearchAuto)
}

// SetGoalDurable updates the Goal only when its sidecar can be replaced
// atomically.
func (c *Controller) SetGoalDurable(goal string) error {
	snapshot := c.goals.capture()
	legacySnapshot, hadLegacySnapshot := c.legacyRestoreSnapshot()
	resolved, setup := c.resolveGoalText(goal, GoalResearchAuto)
	var path string
	var data []byte
	var persist bool
	if setup.blockReason != "" {
		path, data, persist = c.goals.setLegacyArchiveBlockedWithTaskID(resolved, setup.budgetClass, setup.blockReason, setup.legacyTaskID, c.goalTodos())
		c.replaceLegacyRestore(legacyGoalRestore{taskID: setup.legacyTaskID, epoch: c.goals.continuationToken(), explicit: setup.explicit})
	} else {
		path, data, persist = c.goals.set(resolved, setup.budgetClass, c.goalTodos())
		c.replaceLegacyRestore(legacyGoalRestore{})
	}
	if persist {
		if err := c.goals.writeStateErr(path, data); err != nil {
			c.goals.restore(snapshot)
			if hadLegacySnapshot {
				legacySnapshot.epoch = c.goals.continuationToken()
				c.replaceLegacyRestore(legacySnapshot)
			} else {
				c.replaceLegacyRestore(legacyGoalRestore{})
			}
			return err
		}
	}
	if setup.notice != "" {
		c.notice(setup.notice)
	}
	if setup.blockReason != "" {
		c.notice("legacy research archive resume failed: " + setup.blockReason)
	}
	return nil
}

func (c *Controller) SetGoalWithResearchMode(goal string, researchMode GoalResearchMode) {
	resolved, setup := c.resolveGoalText(goal, researchMode)
	if setup.notice != "" {
		c.notice(setup.notice)
	}
	var path string
	var data []byte
	var ok bool
	if setup.blockReason != "" {
		path, data, ok = c.goals.setLegacyArchiveBlockedWithTaskID(resolved, setup.budgetClass, setup.blockReason, setup.legacyTaskID, c.goalTodos())
		c.replaceLegacyRestore(legacyGoalRestore{taskID: setup.legacyTaskID, epoch: c.goals.continuationToken(), explicit: setup.explicit})
		c.notice("legacy research archive resume failed: " + setup.blockReason)
	} else {
		path, data, ok = c.goals.set(resolved, setup.budgetClass, c.goalTodos())
		c.replaceLegacyRestore(legacyGoalRestore{})
	}
	c.persistGoalState(path, data, ok)
}

// goalSetSetup is the resolved objective and budget class after archive lookup.
type goalSetSetup struct {
	budgetClass  string
	notice       string
	blockReason  string
	legacyTaskID string
	explicit     bool
}

func (c *Controller) resolveGoalText(goal string, researchMode GoalResearchMode) (string, goalSetSetup) {
	setup := goalSetSetup{budgetClass: budgetClassForLegacyMode(goal, researchMode)}
	legacy := c.prepareLegacyResearchTask(goal)
	if !legacy.explicit {
		return goal, setup
	}
	setup.notice, setup.blockReason, setup.legacyTaskID, setup.explicit = legacy.notice, legacy.blockReason, legacy.taskID, legacy.explicit
	if legacy.blockReason != "" {
		return goal, setup
	}
	setup.budgetClass = budgetClassResearch
	return legacy.goal, setup
}

// ResumeGoal re-enters a recoverable blocked/stopped Goal without resetting its
// delivery evidence scope or accumulated usage statistics.
func (c *Controller) ResumeGoal() bool {
	if handled, resumed := c.retryBlockedLegacyGoal(); handled {
		return resumed
	}
	spentBudget := c.goals.runtimeView().StopCause == stopCauseBudgetSpend
	path, data, persist, resumed := c.goals.resume(c.goalTodos())
	if !resumed {
		return false
	}
	c.persistGoalState(path, data, persist)
	if c.executor != nil {
		if spentBudget {
			c.executor.ResetTaskBudget()
		}
		c.executor.RestoreDeliveryCheckpoint(c.goals.deliveryState())
	}
	return true
}

// PauseGoal suspends a running Goal without losing its todo list, Delivery
// checkpoint, or runtime history; ResumeGoal restores it. Returns false when no
// running Goal exists.
func (c *Controller) PauseGoal() bool {
	if !c.goals.active() {
		return false
	}
	path, data, ok := c.goals.pauseFor(stopCauseManual, i18n.M.GoalPausedReason, c.goalTodos())
	c.persistGoalState(path, data, ok)
	c.notice(i18n.M.GoalPaused)
	return true
}

// GoalRuntime returns the active Goal's usage/runtime summary for frontends.
func (c *Controller) GoalRuntime() GoalRuntimeView {
	return c.goals.runtimeView()
}

// goalEvaluatorEvidence assembles the bounded evaluator's evidence: the goal
// contract, the current assistant final, a todo/readiness summary,
// turn/budget state, and the last
// continuation reason. Every field is treated as untrusted by the evaluator.
func (c *Controller) goalEvaluatorEvidence() goaleval.GoalEvidence {
	goal, _ := c.goals.snapshot()
	ev := goaleval.GoalEvidence{
		GoalContract:           goal,
		LastContinuationReason: c.goals.lastContinuationReasonText(),
	}
	if c.executor != nil {
		ev.AssistantFinal = lastAssistantText(c.History())
		todos := c.goalTodos()
		incomplete := 0
		for _, t := range todos {
			if t.Status != "completed" {
				incomplete++
			}
		}
		rr := c.executor.ReadinessResult()
		readinessText := "ready"
		if rr.Reason != "" {
			readinessText = rr.Reason
		}
		ev.TodoSummary = fmt.Sprintf("todos: %d total, %d incomplete; delivery readiness: %s", len(todos), incomplete, readinessText)
	}
	ev.TurnStatus = c.goals.budgetStatusText()
	return ev
}

func (c *Controller) persistGoalDeliveryCheckpoint() {
	if c.executor == nil {
		return
	}
	checkpoint := c.executor.DeliveryCheckpoint()
	path, data, ok := c.goals.setDeliveryCheckpoint(checkpoint, c.goalTodos())
	c.persistGoalState(path, data, ok)
}

func (c *Controller) ClearGoal() {
	c.SetGoal("")
}

func (c *Controller) Goal() string {
	return c.goals.goalText()
}

func (c *Controller) GoalStatus() string {
	return c.goals.statusForDisplay()
}

// Compact runs one compaction pass on the executor's session on demand.
// instructions is optional `/compact <focus>` guidance steering what to keep.
func (c *Controller) Compact(ctx context.Context, instructions string) error {
	if c.executor == nil {
		return nil
	}

	if err := c.beginRotation(); err != nil {
		if errors.Is(err, errTurnRunningRotation) {
			return fmt.Errorf("cannot compact while a turn is running")
		}
		return err
	}
	defer c.endRotation()
	if err := c.executor.CompactNow(ctx, instructions); err != nil {
		return err
	}

	return nil
}

// maybeSessionStart fires the SessionStart extension event exactly once per
// session, lazily on the first turn — by then the sink/notify is wired, and a
// resumed session fires it too (its first post-resume turn).
func (c *Controller) maybeSessionStart(ctx context.Context) {
	c.mu.Lock()
	if c.startedOnce {
		c.mu.Unlock()
		return
	}
	c.startedOnce = true
	c.mu.Unlock()
	c.extensionSessionEvent(extension.PointSessionStart, dispatch.PhaseStart, c.SessionPath())
}
