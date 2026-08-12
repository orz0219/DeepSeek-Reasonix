package control

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"reasonix/internal/evidence"
	fileencoding "reasonix/internal/fileutil/encoding"
)

// foldUsage attributes a turn's billable tokens to the goal, but only while the
// goal lifecycle still matches the recorder's scope+epoch; stale or replaced
// goals reject late usage.
func (g *goalMachine) foldUsage(scopeID string, epoch uint64, tokens, requests int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if (tokens <= 0 && requests <= 0) || g.scopeID != scopeID || g.continuationEpoch != epoch {
		return false
	}
	if tokens > 0 {
		g.tokensUsed += tokens
	}
	if requests > 0 {
		g.requestsUsed += requests
	}
	return true
}

// foldWorkDuration attributes one Run's cumulative assistant work duration to
// the Goal. The caller supplies the maximum WorkDurationMs among messages
// created by that Run, so multi-round cumulative values are not double-counted.
func (g *goalMachine) foldWorkDuration(scopeID string, epoch uint64, durationMs int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if durationMs <= 0 || g.scopeID != scopeID || g.continuationEpoch != epoch {
		return false
	}
	g.workDurationMs += durationMs
	return true
}

// buildStateLocked marshals the current goal state for persistence. The caller
// holds mu; this only reads in-memory state, never touching disk. Returns ok=false
// when persistence is disabled (no state path). The matching writeState does the
// disk write OFF mu so the per-turn save can't stall a status poll.
func (g *goalMachine) buildStateLocked(todos []evidence.TodoItem) (path string, data []byte, ok bool) {
	if g.statePath == "" {
		return "", nil, false
	}
	state := goalState{
		Goal:                   g.goal,
		Status:                 g.status,
		ScopeID:                g.scopeID,
		DeliveryCheckpoint:     g.deliveryCheckpoint,
		Turns:                  g.turnsUsed,
		Block:                  g.block,
		Strict:                 g.strict,
		Todos:                  todos,
		BudgetClass:            g.budgetClass,
		TurnsUsed:              g.turnsUsed,
		TurnsLimit:             g.turnsLimit,
		TokensUsed:             g.tokensUsed,
		RequestsUsed:           g.requestsUsed,
		WorkDurationMs:         g.workDurationMs,
		TokensLimit:            g.tokensLimit,
		NoProgressTurns:        g.noProgressTurns,
		NoProgressLimit:        g.noProgressLimit,
		LastContinuationReason: g.lastContinuationReason,
		LastEvaluatorReason:    g.lastEvaluatorReason,
		StopCause:              g.stopCause,
		BudgetExtensions:       g.budgetExtensions,
		ProgressEvidence:       append([]string(nil), g.progressEvidence...),
	}

	if g.legacyTaskID != "" && g.status == GoalStatusBlocked && g.stopCause == stopCauseLegacyArchive {
		state.AutoResearchTaskID = g.legacyTaskID
		state.ResearchMode = GoalResearchOn
	} else {
		state.ResearchMode = GoalResearchOff
	}
	b, err := marshalGoalState(state, g.stateExtra)
	if err != nil {
		slog.Warn("controller: marshal goal state", "err", err)
		return "", nil, false
	}
	return g.statePath, b, true
}

// writeState preserves the existing best-effort behavior for background Goal
// progress. Callers that need transactional persistence use writeStateErr.
func (g *goalMachine) writeState(path string, data []byte) {
	if err := g.writeStateErr(path, data); err != nil {
		slog.Warn("controller: write goal state", "err", err)
	}
}

// persistWithTodos re-persists goal state with the given todos, without
// changing any in-memory goal fields. Used after force-completing todos on
// goal completion so a session reload does not revert to the old incomplete
// todo state.
func (g *goalMachine) persistWithTodos(todos []evidence.TodoItem) {
	g.mu.Lock()
	path, data, ok := g.buildStateLocked(todos)
	g.mu.Unlock()
	if ok {
		g.writeState(path, data)
	}
}

// terminalTodosFromState reads the persisted goal-state sidecar and returns its
// todo snapshot only after the goal has reached a terminal state. Running goal
// state is not refreshed on every todo_write, so its todos may be older than the
// transcript rebuilt by Agent.SetSession.
func (g *goalMachine) terminalTodosFromState(sessionPath string) ([]evidence.TodoItem, bool) {
	if strings.TrimSpace(sessionPath) == "" {
		return nil, false
	}
	data, err := fileencoding.ReadFileUTF8(goalStatePath(sessionPath))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("controller: read goal state", "err", err)
		}
		return nil, false
	}
	var state goalState
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Warn("controller: parse goal state", "err", err)
		return nil, false
	}
	switch state.Status {
	case GoalStatusComplete, GoalStatusBlocked, GoalStatusStopped:
	default:
		return nil, false
	}
	if len(state.Todos) == 0 {
		return nil, false
	}
	return append([]evidence.TodoItem(nil), state.Todos...), true
}

// restoreFromState reloads Goal state from the sidecar. The sidecar is
// authoritative; active Goals are normalized to continuous-runtime sentinels.
// migrated means path/data were atomically rewritten (without a provider call).
// legacyTaskID is returned only
// so Controller can fill missing goal text from a historical archive.
func (g *goalMachine) restoreFromState(sessionPath string) (path string, data []byte, migrated bool, legacy legacyGoalRestore) {
	if strings.TrimSpace(sessionPath) == "" {
		return "", nil, false, legacyGoalRestore{}
	}

	if g.statePath == "" {
		g.setStatePath(goalStatePath(sessionPath))
	}
	raw, err := fileencoding.ReadFileUTF8(goalStatePath(sessionPath))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("controller: read goal state", "err", err)
		}
		return "", nil, false, legacyGoalRestore{}
	}
	var state goalState
	if err := json.Unmarshal(raw, &state); err != nil {
		slog.Warn("controller: parse goal state", "err", err)
		return "", nil, false, legacyGoalRestore{}
	}
	g.mu.Lock()
	g.stateExtra = goalStateUnknownFields(raw)
	g.goal = strings.TrimSpace(state.Goal)
	g.status = state.Status
	if g.status == "" {
		g.status = GoalStatusStopped
	}

	legacy = legacyGoalRestore{
		taskID: strings.TrimSpace(state.AutoResearchTaskID),
		todos:  append([]evidence.TodoItem(nil), state.Todos...),
	}

	if g.goal == "" {
		g.legacyTaskID = legacy.taskID
	} else {
		g.legacyTaskID = ""
	}
	if legacy.taskID != "" && g.goal != "" {

		migrated = true
	}
	g.scopeID = strings.TrimSpace(state.ScopeID)
	if g.scopeID == "" {
		g.scopeID = strings.TrimSpace(state.DeliveryCheckpoint.ScopeID)
	}
	if g.goal != "" && g.scopeID == "" {
		g.scopeID = newGoalScopeID()
	}
	g.deliveryCheckpoint = state.DeliveryCheckpoint
	if g.scopeID == "" {
		g.deliveryCheckpoint = evidence.DeliveryCheckpoint{}
	} else if g.deliveryCheckpoint.ScopeID == "" {
		g.deliveryCheckpoint.ScopeID = g.scopeID
	} else if g.deliveryCheckpoint.ScopeID != g.scopeID {
		g.deliveryCheckpoint = evidence.DeliveryCheckpoint{ScopeID: g.scopeID}
	}
	g.block = state.Block
	g.strict = state.Strict
	g.stopCause = state.StopCause
	g.budgetExtensions = state.BudgetExtensions
	g.progressEvidence, _ = mergeGoalProgressEvidence(nil, state.ProgressEvidence)
	g.lastContinuationReason = state.LastContinuationReason
	g.lastEvaluatorReason = state.LastEvaluatorReason

	g.turnsUsed = state.TurnsUsed
	if g.turnsUsed == 0 && state.Turns > 0 {
		g.turnsUsed = state.Turns
	}
	g.tokensUsed = state.TokensUsed
	g.requestsUsed = state.RequestsUsed
	g.workDurationMs = state.WorkDurationMs
	g.budgetClass = normalizeBudgetClass(g.goal, state.BudgetClass, state.ResearchMode)
	g.turnsLimit = state.TurnsLimit
	g.noProgressTurns = state.NoProgressTurns
	g.noProgressLimit = state.NoProgressLimit
	g.tokensLimit = state.TokensLimit

	rollback := g.captureLocked()
	if goalStateNeedsMigration(state, g.budgetClass) {
		migrated = true
	}
	if g.normalizeContinuousState(state.ResearchMode, legacy.taskID) {
		migrated = true
	}
	g.continuationEpoch++
	legacy.epoch = g.continuationEpoch
	pendingLegacyGoal := legacy.taskID != "" && g.goal == ""
	if migrated && !pendingLegacyGoal {

		path, data, ok := g.buildStateLocked(state.Todos)
		if ok {
			g.mu.Unlock()
			if err := g.writeStateErr(path, data); err != nil {
				slog.Warn("controller: persist normalized goal state", "err", err)
				g.restore(rollback)
				return "", nil, false, legacy
			}
			return path, data, true, legacy
		}
	}
	g.mu.Unlock()
	return "", nil, false, legacy
}

// formatIncompleteTodos renders the reminder shown when a complete claim
// arrives while the executor's canonical todos or project-readiness checks
// aren't done. Returns empty when nothing is blocking. Pure: the caller gathers
// todos and the readiness reason from the executor off the goal lock.
func formatIncompleteTodos(todos []evidence.TodoItem, readiness string) string {
	var parts []string
	if len(todos) > 0 {
		if incomplete := evidence.IncompleteTodos(todos); len(incomplete) > 0 {
			var b strings.Builder
			b.WriteString("the following tasks are still incomplete:")
			for _, t := range incomplete {
				fmt.Fprintf(&b, "\n  - %s (%s)", t.Content, t.Status)
			}
			parts = append(parts, b.String())
		}
	}
	if readiness != "" {
		parts = append(parts, readiness)
	}
	if len(parts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Goal signaled complete but issues remain:\n")
	for _, p := range parts {
		b.WriteString("- ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	b.WriteString("Fix or use todo_write/complete_step to mark done, then report complete again via update_goal.")
	return b.String()
}

// clipGoalReason bounds a recorded reason for storage and display.
func clipGoalReason(reason string) string {
	reason = strings.TrimSpace(reason)
	const max = 400
	if r := []rune(reason); len(r) > max {
		return string(r[:max]) + "..."
	}
	return reason
}

func cleanGoalBlockReason(reason string) string {
	return strings.Trim(strings.TrimSpace(reason), " \t\r\n:：,，.。;；!！?？-—_[]()（）")
}

// ShortGoalForNotice collapses whitespace and truncates a goal for one-line UI.
func ShortGoalForNotice(goal string) string {
	goal = strings.Join(strings.Fields(goal), " ")
	runes := []rune(goal)
	const max = 160
	if len(runes) <= max {
		return goal
	}
	return string(runes[:max]) + "..."
}

// goalTodos snapshots the executor's canonical todos for goal-state persistence.
func (c *Controller) goalTodos() []evidence.TodoItem {
	if c.executor == nil {
		return nil
	}
	return c.executor.CanonicalTodoState()
}

// persistGoalState writes a freshly built goal state to disk, off c.mu. The
// executor guard preserves the original behavior of skipping persistence when
// no executor is attached.
func (c *Controller) persistGoalState(path string, data []byte, ok bool) {
	if !ok || c.executor == nil {
		return
	}
	c.goals.writeState(path, data)
}

func (c *Controller) persistGoalStateAtEpoch(epoch uint64, todos []evidence.TodoItem) (bool, error) {
	applied, err := c.goals.writeStateAtEpoch(epoch, todos)
	if err != nil {
		slog.Warn("controller: write goal state", "err", err)
	}
	return applied, err
}

func (c *Controller) restoreTerminalGoalTodos(sessionPath string) {
	if c.executor == nil {
		return
	}
	todos, ok := c.goals.terminalTodosFromState(sessionPath)
	if !ok {
		return
	}
	c.executor.ReplaceTodoState(todos)
}
