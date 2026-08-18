package control

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/evidence"
	"reasonix/internal/goaleval"
	"reasonix/internal/store"
	"reasonix/internal/taskintent"
)

const (
	goalContinueTurn   = "Continue pursuing the active goal under its task contract. Do the next useful work, then call update_goal with your disposition: continue (include the next concrete step in next_action), complete (only when fully done and verified), or blocked (when only the user can unblock)."
	goalCompleteNotice = "goal complete"
	unlimitedGoalTurns = -1

	// Bound the persisted novelty window. Signatures are compact hashes, and
	// retaining the most recent window is enough to stop short repeat cycles
	// without allowing an unbounded Goal sidecar.
	maxGoalProgressEvidence = 512
)

// Budget class aliases remain as sidecar/CLI compatibility metadata only.
const (
	budgetClassSimple   = taskintent.BudgetClassSimple
	budgetClassWrite    = taskintent.BudgetClassWrite
	budgetClassResearch = taskintent.BudgetClassResearch
)

// Stop causes distinguish a safe pause from a genuine block. Removed numeric
// causes remain migration-only constants so old sidecars can be normalized.
const (
	stopCauseBudgetTurns   = "budget_turns" // legacy; the class-derived turn quota is gone
	stopCauseBudgetSpend   = "budget_spend"
	stopCauseBudgetTokens  = "budget_tokens"   // legacy; never written by current runtime
	stopCauseNoProgress    = "no_progress"     // legacy; never written by current runtime
	stopCauseGoalRunBudget = "goal_run_budget" // legacy; the per-Run round ceiling is gone
	stopCauseGoalStuck     = "goal_stuck"
	stopCauseEvaluator     = "evaluator_unavailable"
	stopCauseLegacyArchive = "legacy_archive"
	stopCauseManual        = "manual"
)

// budgetClassForLegacyMode translates old sidecars and deprecated CLI flags at
// the compatibility boundary. The active Goal runtime stores only budgetClass.
func budgetClassForLegacyMode(goal string, researchMode GoalResearchMode) string {
	switch researchMode {
	case GoalResearchOn:
		return budgetClassResearch
	case GoalResearchOff:
		if taskintent.GoalNeedsWriteBudget(goal) {
			return budgetClassWrite
		}
		return budgetClassSimple
	default:
		return taskintent.ClassifyGoalBudget(goal)
	}
}

// goalMachine owns the active goal FSM and its persistence. It is a strict
// leaf: methods take only machine locks and never call back into Controller.
// advance() takes already-gathered inputs so no disk/executor work holds mu.
type goalMachine struct {
	// mu guards the FSM fields below; every critical section under it is short
	// and non-blocking (no disk I/O, no executor calls).
	mu                 sync.Mutex
	goal               string
	status             string
	scopeID            string
	deliveryCheckpoint evidence.DeliveryCheckpoint
	block              string
	strict             bool
	continuationEpoch  uint64

	tokenBudget int // configured ceiling for an unattended loop; 0 = unbounded

	// Runtime statistics and optional user-selected spend state, persisted
	// across turns and restarts. turnsUsed and noProgressTurns are observational;
	// tokensLimit is non-zero only when the user configured a Goal token budget.
	budgetClass            string
	turnsUsed              int
	turnsLimit             int
	tokensUsed             int
	requestsUsed           int
	workDurationMs         int64
	tokensLimit            int // always 0 at runtime; deprecated hard limit
	noProgressTurns        int
	noProgressLimit        int
	lastContinuationReason string
	lastEvaluatorReason    string
	stopCause              string
	budgetExtensions       int // deprecated historical sidecar field
	progressEvidence       []string
	// stateExtra preserves fields written by a newer peer during read/modify/
	// write cycles. Known current fields always win on serialization.
	stateExtra map[string]json.RawMessage
	// legacyTaskID is retained only while a historical AutoResearch archive is
	// awaiting migration. It is serialized on fail-closed blocked sidecars so a
	// restart can retry the migration without treating the raw archive path as a
	// new Goal.
	legacyTaskID string

	// statePath is the persisted goal-state sidecar; empty disables persistence.
	statePath string
	// writeMu serializes goal-state disk writes so concurrent saves don't
	// interleave or land out of order. Taken OFF mu by writeState.
	writeMu sync.Mutex
}

// goalState is the serializable form of a running goal. New fields are
// safe-to-omit JSON: old readers ignore them, and restoreFromState re-derives
// defaults when they are missing.
type goalState struct {
	Goal               string                      `json:"goal,omitempty"`
	Status             string                      `json:"status,omitempty"`
	ResearchMode       GoalResearchMode            `json:"researchMode,omitempty"`
	AutoResearchTaskID string                      `json:"autoResearchTaskID,omitempty"`
	ScopeID            string                      `json:"scopeID,omitempty"`
	DeliveryCheckpoint evidence.DeliveryCheckpoint `json:"deliveryCheckpoint,omitempty"`
	Turns              int                         `json:"turns,omitempty"`
	Blocks             int                         `json:"blocks,omitempty"`
	Block              string                      `json:"block,omitempty"`
	Strict             bool                        `json:"strict,omitempty"`
	Todos              []evidence.TodoItem         `json:"todos,omitempty"`

	BudgetClass            string   `json:"budgetClass,omitempty"`
	TurnsUsed              int      `json:"turnsUsed,omitempty"`
	TurnsLimit             int      `json:"turnsLimit,omitempty"`
	TokensUsed             int      `json:"tokensUsed,omitempty"`
	RequestsUsed           int      `json:"requestsUsed,omitempty"`
	WorkDurationMs         int64    `json:"workDurationMs,omitempty"`
	TokensLimit            int      `json:"tokensLimit,omitempty"`
	NoProgressTurns        int      `json:"noProgressTurns,omitempty"`
	NoProgressLimit        int      `json:"noProgressLimit,omitempty"`
	LastContinuationReason string   `json:"lastContinuationReason,omitempty"`
	LastEvaluatorReason    string   `json:"lastEvaluatorReason,omitempty"`
	StopCause              string   `json:"stopCause,omitempty"`
	BudgetExtensions       int      `json:"budgetExtensions,omitempty"`
	ProgressEvidence       []string `json:"progressEvidence,omitempty"`
}

// goalAdvanceInput carries everything the FSM needs for one continuation step,
// gathered by the caller off the machine's lock. The FSM is the exclusive
// decision point: it applies readiness and decides complete / continue /
// blocked / evaluator fail-closed pause.
type goalAdvanceInput struct {
	report           *goalTurnReport // validated update_goal report; nil when none
	readiness        agent.ReadinessResult
	evaluator        *goalEvaluatorVerdict // evaluator verdict; nil when not run
	evaluatorFailed  string                // evaluator error/timeout text; pause fail-closed
	todos            []evidence.TodoItem
	progressEvidence []string // host evidence identities visible after this turn
	pauseCause       string   // explicit spend boundary reported by the Agent
	pauseReason      string
	expectedEpoch    *uint64
}

// goalEvaluatorVerdict is the bounded evaluator's structured outcome.
type goalEvaluatorVerdict struct {
	outcome goaleval.Outcome
	reason  string
}

// goalAdvanceResult reports the FSM step's outcome. data/path/ok describe the
// state to persist (built under mu when something changed); notice is surfaced
// to the user; cont reports whether the goal loop should continue; intercept
// (with interceptNotice) is the next synthetic turn's prompt.
type goalAdvanceResult struct {
	notice            string
	intercept         string
	interceptNotice   string
	cont              bool
	continuationEpoch uint64
	path              string
	data              []byte
	ok                bool
}

// goalContinuationSnapshot binds a continuation to the exact Goal lifecycle
// state admitted for its synthetic turn. The orchestrator uses these captured
// fields throughout the turn instead of re-reading a possibly replaced Goal.
type goalContinuationSnapshot struct {
	goal    string
	scopeID string
}

// goalStatePath derives a session's persisted goal-state sidecar.
func goalStatePath(sessionPath string) string {
	return store.SessionGoalState(sessionPath)
}

func (g *goalMachine) setStatePath(path string) {
	g.mu.Lock()
	g.statePath = path
	g.mu.Unlock()
}

// snapshot returns the fields Compose injects into outgoing turns.
func (g *goalMachine) snapshot() (goal, status string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.goal, g.status
}

func (g *goalMachine) goalText() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.goal
}

// continuationToken captures the Goal lifecycle that owns an outgoing turn.
// The matching assistant output may advance the FSM only while this epoch is
// still current.
func (g *goalMachine) continuationToken() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.continuationEpoch
}

func (g *goalMachine) deliveryScope() (id, task string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if strings.TrimSpace(g.goal) == "" || g.status != GoalStatusRunning {
		return "", "", false
	}
	if g.scopeID == "" {
		g.scopeID = newGoalScopeID()
	}
	return g.scopeID, g.goal, true
}

// goalScopeIDForTurn resolves the active goal scope for an outgoing turn: the
// continuation snapshot's scope, or the running goal's (assigning one when
// needed). ok=false means no active goal.
func (g *goalMachine) goalScopeIDForTurn(continuation *goalContinuationSnapshot) (string, bool) {
	if continuation != nil {
		return continuation.scopeID, true
	}
	id, _, ok := g.deliveryScope()
	return id, ok
}

func newGoalScopeID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return fmt.Sprintf("goal-%x", raw[:])
	}
	return fmt.Sprintf("goal-fallback-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// active reports whether a goal is currently running.
func (g *goalMachine) active() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.TrimSpace(g.goal) != "" && g.status == GoalStatusRunning
}

// statusForDisplay maps the empty zero status to "stopped" for frontends.
func (g *goalMachine) statusForDisplay() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.status == "" {
		return GoalStatusStopped
	}
	return g.status
}

// set installs a session-scoped goal (or clears it when goal is empty), resets
// the per-goal runtime counters, and returns the state to persist. ok is
// false (no persistence) when the goal is unchanged or no state path is
// configured.
func (g *goalMachine) set(goal, preferredBudgetClass string, todos []evidence.TodoItem) (string, []byte, bool) {
	goal = strings.TrimSpace(goal)
	if goal != "" && preferredBudgetClass == "" {
		preferredBudgetClass = taskintent.ClassifyGoalBudget(goal)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if goal != "" && g.goal == goal && g.status == GoalStatusRunning && g.budgetClass == preferredBudgetClass {
		return "", nil, false
	}
	g.installGoalLocked(goal, preferredBudgetClass)
	return g.buildStateLocked(todos)
}

// setLegacyArchiveBlocked atomically installs and blocks an explicit legacy
// archive goal. A concurrent Goal replacement cannot be blocked between two
// separate FSM mutations.

func (g *goalMachine) setLegacyArchiveBlockedWithTaskID(goal, preferredBudgetClass, reason, taskID string, todos []evidence.TodoItem) (string, []byte, bool) {
	goal = strings.TrimSpace(goal)
	taskID = strings.TrimSpace(taskID)
	if goal != "" && preferredBudgetClass == "" {
		preferredBudgetClass = taskintent.ClassifyGoalBudget(goal)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.installGoalLocked(goal, preferredBudgetClass)
	if goal != "" {
		g.status = GoalStatusBlocked
	}
	g.stopCause = stopCauseLegacyArchive
	g.block = clipGoalReason(reason)
	g.legacyTaskID = taskID
	return g.buildStateLocked(todos)
}

func (g *goalMachine) installGoalLocked(goal, preferredBudgetClass string) {
	g.continuationEpoch++
	g.turnsUsed, g.tokensUsed, g.requestsUsed, g.noProgressTurns = 0, 0, 0, 0
	g.workDurationMs = 0
	g.block = ""
	g.lastContinuationReason, g.lastEvaluatorReason = "", ""
	g.stopCause = ""
	g.budgetExtensions = 0
	g.progressEvidence = nil
	if goal == "" {
		g.goal, g.status = "", GoalStatusStopped
		g.budgetClass = ""
		g.turnsLimit = 0
		g.noProgressLimit = 0
		g.scopeID = ""
		g.deliveryCheckpoint = evidence.DeliveryCheckpoint{}
	} else {
		g.goal, g.status = goal, GoalStatusRunning
		g.scopeID = newGoalScopeID()
		g.deliveryCheckpoint = evidence.DeliveryCheckpoint{ScopeID: g.scopeID}
		g.budgetClass = preferredBudgetClass
		g.turnsLimit = unlimitedGoalTurns
		g.tokensLimit = g.tokenBudget
		g.noProgressLimit = 0
	}
	// Installing a normal Goal always abandons any pending legacy migration.
	g.legacyTaskID = ""
}

func (g *goalMachine) setStrict(strict bool, todos []evidence.TodoItem) (string, []byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.strict = strict
	return g.buildStateLocked(todos)
}

// stop transitions a running goal to the given terminal status and clears the
// transient runtime bookkeeping. stopCause is cleared: a host stop is not a
// safe pause.
func (g *goalMachine) stop(status string, todos []evidence.TodoItem) (string, []byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.continuationEpoch++
	if strings.TrimSpace(g.goal) != "" && g.status == GoalStatusRunning {
		g.status = status
	}
	g.stopCause = ""
	g.noProgressTurns = 0
	return g.buildStateLocked(todos)
}

// pauseFor transitions a running goal to a safe pause: status blocked plus a
// stop cause, keeping every runtime counter for a later resume.
func (g *goalMachine) pauseFor(stopCause, reason string, todos []evidence.TodoItem) (string, []byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.continuationEpoch++
	if strings.TrimSpace(g.goal) != "" && g.status == GoalStatusRunning {
		g.status = GoalStatusBlocked
	}
	g.stopCause = stopCause
	if reason != "" {
		g.block = reason
	}
	return g.buildStateLocked(todos)
}

// resume re-enters a recoverable blocked/stopped goal without resetting scope
// or runtime history. Continuous Goals never extend a numeric quota.
func (g *goalMachine) resume(todos []evidence.TodoItem) (path string, data []byte, persist, resumed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopCause == stopCauseLegacyArchive {
		// A legacy archive block is recoverable only through the read-only
		// archive boundary; never reinterpret it as an ordinary Goal resume.
		return "", nil, false, false
	}
	if strings.TrimSpace(g.goal) == "" || g.status == GoalStatusComplete {
		return "", nil, false, false
	}
	// A user-selected spend pause grants one fresh configured slice. Usage
	// remains cumulative; only the absolute threshold moves forward.
	spentBudget := g.stopCause == stopCauseBudgetSpend
	g.continuationEpoch++
	g.status = GoalStatusRunning
	g.block = ""
	g.stopCause = ""
	g.noProgressTurns = 0
	g.turnsLimit = unlimitedGoalTurns
	g.noProgressLimit = 0
	g.budgetExtensions = 0
	g.grantSpendSliceLocked(spentBudget)
	if g.scopeID == "" {
		g.scopeID = newGoalScopeID()
	}
	path, data, persist = g.buildStateLocked(todos)
	return path, data, persist, true
}

func (g *goalMachine) setDeliveryCheckpoint(checkpoint evidence.DeliveryCheckpoint, todos []evidence.TodoItem) (string, []byte, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.scopeID == "" || checkpoint.ScopeID != g.scopeID {
		return "", nil, false
	}
	g.deliveryCheckpoint = checkpoint
	return g.buildStateLocked(todos)
}

func (g *goalMachine) deliveryState() evidence.DeliveryCheckpoint {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deliveryCheckpoint
}

// acceptContinuation checks an advance result before the orchestrator surfaces
// its notice. admitContinuation revalidates after synchronous notice callbacks
// and captures the Goal state at the synthetic-turn admission boundary.
func (g *goalMachine) acceptContinuation(res goalAdvanceResult) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !res.cont ||
		res.continuationEpoch != g.continuationEpoch ||
		strings.TrimSpace(g.goal) == "" ||
		g.status != GoalStatusRunning {
		return "", false
	}
	return res.intercept, true
}

// admitContinuation atomically validates an advance result and captures the
// Goal state used to compose and scope its synthetic turn. Keeping validation
// and capture in one critical section prevents a stale intercept from being
// paired with a replacement Goal between those operations.
func (g *goalMachine) admitContinuation(res goalAdvanceResult) (goalContinuationSnapshot, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !res.cont ||
		res.continuationEpoch != g.continuationEpoch ||
		strings.TrimSpace(g.goal) == "" ||
		g.status != GoalStatusRunning {
		return goalContinuationSnapshot{}, false
	}
	if g.scopeID == "" {
		g.scopeID = newGoalScopeID()
	}
	return goalContinuationSnapshot{
		goal:    g.goal,
		scopeID: g.scopeID,
	}, true
}

// advance runs one continuation step of the goal FSM from already-gathered
// inputs. It mutates the machine, decides whether to keep looping, and builds
// the state to persist after every Goal turn.
//
// Decision priority (the FSM is the exclusive decision point):
//  1. complete + readiness ready (report or evaluator) → complete
//  2. blocked (report or evaluator) → blocked immediately (no triple confirm)
//  3. evaluator failed/uncertain → safe pause (fail closed, never default to continue)
//  4. otherwise continue, carrying the missing requirements (complete rejected
//     by readiness, or no report with an explicit missing list) or the report's
//     next_action as the next turn's prompt.
func (g *goalMachine) advance(in goalAdvanceInput) goalAdvanceResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	if in.expectedEpoch != nil && *in.expectedEpoch != g.continuationEpoch {
		return goalAdvanceResult{cont: false}
	}
	if strings.TrimSpace(g.goal) == "" || g.status != GoalStatusRunning {
		return goalAdvanceResult{cont: false}
	}
	g.continuationEpoch++
	// A top-level goal turn (the first turn or a synthetic continuation) counts
	// as an observational statistic; the in-Run model/tool loop is not re-counted.
	g.turnsUsed++
	g.observeGoalProgress(in)
	var notice string
	var intercept string
	var interceptNotice string
	evaluatorComplete := in.evaluator != nil && in.evaluator.outcome == goaleval.OutcomeComplete
	evaluatorBlocked := in.evaluator != nil && in.evaluator.outcome == goaleval.OutcomeBlocked
	// Terminal dispositions first, then evaluator fail-closed; otherwise continue.
	reportBlocked := in.report != nil && in.report.status == GoalStatusBlocked
	reportComplete := in.report != nil && in.report.status == GoalStatusComplete
	completeOK := (reportComplete || evaluatorComplete) && formatIncompleteTodos(in.todos, in.readiness.Reason) == ""
	switch {
	case reportBlocked:
		// A single blocked report ends the goal immediately; the host no longer
		// repeats a three-turn confirmation ritual.
		reason := cleanGoalBlockReason(in.report.reason)
		if reason == "" {
			reason = "blocked"
		}
		g.status = GoalStatusBlocked
		g.block = reason
		g.stopCause = ""
		g.lastContinuationReason = clipGoalReason(in.report.reason)
		notice = "goal blocked: " + reason
	case evaluatorBlocked:
		reason := cleanGoalBlockReason(in.evaluator.reason)
		if reason == "" {
			reason = "blocked"
		}
		g.status = GoalStatusBlocked
		g.block = reason
		g.stopCause = ""
		g.lastEvaluatorReason = clipGoalReason(in.evaluator.reason)
		notice = "goal blocked: " + reason
	case completeOK:
		g.goal = ""
		g.status = GoalStatusComplete
		g.block = ""
		g.stopCause = ""
		g.progressEvidence = nil
		g.lastContinuationReason, g.lastEvaluatorReason = "", ""
		notice = goalCompleteNotice
	case g.tokensLimit > 0 && g.tokensUsed >= g.tokensLimit:
		reason := fmt.Sprintf("token budget reached (%d/%d tokens used)", g.tokensUsed, g.tokensLimit)
		g.status = GoalStatusBlocked
		g.stopCause = stopCauseBudgetSpend
		g.block = clipGoalReason(reason)
		notice = "goal paused: " + reason
	case in.evaluatorFailed != "" || (in.evaluator != nil && in.evaluator.outcome == goaleval.OutcomeUncertain):
		// Fail closed: an unavailable, erroring, or uncertain evaluator pauses
		// the goal instead of defaulting to continue.
		reason := "the completion evaluator is unavailable or could not judge the turn"
		if in.evaluatorFailed != "" {
			reason = "the completion evaluator failed: " + in.evaluatorFailed
		}
		g.status = GoalStatusBlocked
		g.stopCause = stopCauseEvaluator
		g.block = clipGoalReason(reason)
		g.lastEvaluatorReason = clipGoalReason(reason)
		notice = "goal paused: " + reason
	case in.pauseCause != "":
		reason := strings.TrimSpace(in.pauseReason)
		if reason == "" {
			reason = "the current Goal run reached a recoverable execution boundary"
		}
		g.status = GoalStatusBlocked
		g.stopCause = in.pauseCause
		g.block = clipGoalReason(reason)
		notice = "goal paused: " + reason
	default:
		// Continue. A complete claim rejected by readiness, or a turn with no
		// report but an explicit missing list, carries the missing requirements
		// into the next turn; a continue report carries its next_action.
		switch {
		case reportComplete:
			intercept = formatIncompleteTodos(in.todos, in.readiness.Reason)
			interceptNotice = "Goal is not ready to complete yet; continuing the remaining work."
			g.lastContinuationReason = clipGoalReason("readiness missing: " + in.readiness.Reason)
		case in.report != nil && in.report.status == GoalStatusRunning:
			g.lastContinuationReason = clipGoalReason(in.report.reason)
			if in.report.nextAction != "" {
				intercept = in.report.nextAction
			}
		case len(in.readiness.Missing) > 0:
			intercept = formatIncompleteTodos(in.todos, in.readiness.Reason)
			interceptNotice = "Goal is not ready to complete yet; continuing the remaining work."
			g.lastContinuationReason = clipGoalReason("readiness missing: " + in.readiness.Reason)
		case evaluatorComplete:
			intercept = formatIncompleteTodos(in.todos, in.readiness.Reason)
			interceptNotice = "Goal is not ready to complete yet; continuing the remaining work."
			g.lastEvaluatorReason = clipGoalReason(in.evaluator.reason)
			g.lastContinuationReason = clipGoalReason("readiness missing: " + in.readiness.Reason)
		case in.evaluator != nil && in.evaluator.outcome == goaleval.OutcomeContinue:
			g.lastEvaluatorReason = clipGoalReason(in.evaluator.reason)
		}
	}
	res := goalAdvanceResult{
		notice:            notice,
		intercept:         intercept,
		interceptNotice:   interceptNotice,
		cont:              notice == "",
		continuationEpoch: g.continuationEpoch,
	}
	res.path, res.data, res.ok = g.buildStateLocked(in.todos)
	return res
}

// foldUsage attributes a turn's billable tokens to the goal, but only while the
// goal lifecycle still matches the recorder's scope+epoch; stale or replaced
// goals reject late usage.

// foldWorkDuration attributes one Run's cumulative assistant work duration to
// the Goal. The caller supplies the maximum WorkDurationMs among messages
// created by that Run, so multi-round cumulative values are not double-counted.

// buildStateLocked marshals the current goal state for persistence. The caller
// holds mu; this only reads in-memory state, never touching disk. Returns ok=false
// when persistence is disabled (no state path). The matching writeState does the
// disk write OFF mu so the per-turn save can't stall a status poll.

// GoalResearchOff is a downgrade fence for ordinary Goal sidecars. A
// fail-closed legacy migration keeps its task identity and compatibility mode
// until the archive has been validated and the Goal-only state is committed.

// writeState preserves the existing best-effort behavior for background Goal
// progress. Callers that need transactional persistence use writeStateErr.

// persistWithTodos re-persists goal state with the given todos, without
// changing any in-memory goal fields. Used after force-completing todos on
// goal completion so a session reload does not revert to the old incomplete
// todo state.

// todosFromState reads the persisted goal-state sidecar and returns its todo
// snapshot. Restores both running and terminal goals so a restart does not
// lose completed-step visibility.

// restoreFromState reloads Goal state from the sidecar. The sidecar is
// authoritative; active Goals are normalized to continuous-runtime sentinels.
// migrated means path/data were atomically rewritten (without a provider call).
// legacyTaskID is returned only
// so Controller can fill missing goal text from a historical archive.

// Ensure write path is bound even when the controller rebuilds.

// Legacy task identity is migration-only compatibility data. It is returned to
// the Controller's archive boundary and retained in the machine only while a
// fail-closed migration remains pending.

// A task id is pending only when the sidecar has no Goal text. A legacy
// sidecar that already contains an objective can be migrated directly and
// must serialize as ordinary Goal state on the first write.

// Sidecars that already carry the Goal objective do not depend on the
// historical archive. Complete the migration immediately.

// Old sidecars carry Turns (pre-budget counting); treat it as turn usage.

// Roll back to the loaded, still-paused semantics if the atomic
// normalization write fails. This prevents a session from appearing
// unlocked only in memory while its sidecar remains blocked on disk.

// Migration rewrites only the removed budget state. Preserve the todo
// snapshot carried by the authoritative sidecar instead of clearing it.

// formatIncompleteTodos renders the reminder shown when a complete claim
// arrives while the executor's canonical todos or project-readiness checks
// aren't done. Returns empty when nothing is blocking. Pure: the caller gathers
// todos and the readiness reason from the executor off the goal lock.

// clipGoalReason bounds a recorded reason for storage and display.

// ShortGoalForNotice collapses whitespace and truncates a goal for one-line UI.

// goalTodos snapshots the executor's canonical todos for goal-state persistence.

// persistGoalState writes a freshly built goal state to disk, off c.mu. The
// executor guard preserves the original behavior of skipping persistence when
// no executor is attached.

// goalTurnRecorder is the per-turn recorder bound to one goal turn's scope and
// epoch. update_goal calls land here as candidate state; the FSM commits them
// only when the goal lifecycle still matches (scope + epoch), so late calls
// from a replaced or cleared goal are rejected. Usage events emitted during the
// turn are folded through the recorder into the goal's observational token total.

// RecordGoalReport validates the report against the turn's goal lifecycle and
// records it as the turn's candidate disposition. Same-value repeats are
// idempotent; continue may upgrade to complete/blocked; complete and blocked
// are terminal and reject conflicting later calls.

// The wire status "continue" maps to the FSM's internal "running" state;
// retain the wire value for user-facing acknowledgements and errors.

// first record

// continue → terminal upgrade allowed.

// validReport returns the recorded report only when the goal lifecycle still
// matches the recorder's binding; stale (replaced/cleared) turns report nothing.

// goalTurnReport is the validated update_goal report for one goal turn.
type goalTurnReport struct {
	status     string
	reason     string
	nextAction string
}

// turnActive reports whether the machine's goal lifecycle matches the binding.
func (g *goalMachine) turnActive(scopeID string, epoch uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.TrimSpace(g.goal) != "" && g.status == GoalStatusRunning &&
		g.scopeID == scopeID && g.continuationEpoch == epoch
}
