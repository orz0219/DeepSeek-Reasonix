package recovery

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"reasonix/internal/agent"
)

// ModeProvider reports the current tool-approval mode (ask|auto|yolo).
type ModeProvider func() string

// EmitPromptFunc shows a fresh Auto Guard card and returns its id.
// It must not grant session or persistent authorization. The gate waits until
// Resolve is called for that id (or ctx ends).
type EmitPromptFunc func(ctx context.Context, taskID string, pending PendingProposal, failure *FailureEvent) (approvalID string, err error)

// Reviewer evaluates ambiguous failure-recovery proposals.
type Reviewer interface {
	Review(ctx context.Context, failure *FailureEvent, diagnosis []string, proposal Proposal, taskSummary string) (ReviewVerdict, error)
}

// Options configures a Gate.
type Options struct {
	Mode            ModeProvider
	EmitPrompt      EmitPromptFunc
	Reviewer        Reviewer
	TaskSummary     func() string
	MaxReviewBlocks int // consecutive reviewer blocks before stop-and-report guidance
	Now             func() time.Time
	// Headless, when true, never waits for a human: blocks the mutation with a
	// structured blocker message instead.
	Headless bool
	// PersistenceKey is sampled synchronously when a state change is scheduled.
	// Persist receives that captured key so an asynchronous write cannot follow
	// a later session switch and land in the wrong sidecar.
	PersistenceKey func() string
	// Persist is invoked after meaningful state changes (optional).
	// Receives the persistence projection (never active locks).
	Persist func(key string, snapshot Snapshot)
}

// Gate is the Auto Guard coordinator for one controller session.
// Root, foreground sub-agents, and background writer sub-agents share it.
// Exact-operation failure counts are isolated by TaskID; Episode totals,
// reviewer rejects, and hard stop are shared on episode so a new sub-agent
// cannot reset the hard ceiling. Pure routing lives in Decide.
//
// EpisodeID is host-owned temporary execution-round state. TaskScopeID continues
// to scope Goal and task grants. Episode/generation/waiters never persist.
type Gate struct {
	mu      sync.Mutex
	opts    Options
	tasks   map[string]*taskRuntime
	metrics Metrics
	waiters map[string]chan resolvePayload // keyed by approval id
	taskOf  map[string]string              // approval id -> task id
	pending map[string]PendingProposal     // approval id -> transient proposal scope
	// awaiting tracks in-flight human prompts so Phase can be derived without
	// storing Pending on the task runtime.
	awaiting map[string]struct{} // task ids with an open waiter

	// episodeSeq / episodeID identify the current host-owned Recovery Episode.
	// generation invalidates in-flight tool observations across mode switches.
	// episode holds totals and hard-stop shared by every TaskID in the Episode.
	episodeSeq uint64
	episodeID  string
	generation uint64
	episode    episodeBudget
	lastMode   string
	haveMode   bool

	// persistMu orders asynchronous snapshots. A newer state may be scheduled
	// before an older goroutine reaches disk; sequence checks prevent that older
	// snapshot from overwriting the newer checkpoint.
	persistMu   sync.Mutex
	persistSeq  uint64
	persistCond *sync.Cond
	// persistPending and persistDone are tracked per session key so old and new
	// sessions can drain independently without retaining keys after completion.
	persistPending map[string]int
	persistDone    map[string]uint64
}

type resolvePayload struct {
	action   Action
	feedback string
}

// dismissedWaiter is a recovery waiter cancelled by mode switch / episode rotate.
type dismissedWaiter struct {
	id      string
	taskID  string
	reply   chan resolvePayload
	payload resolvePayload
}

// NewGate constructs Auto Guard. The gate is active whenever approval mode is
// Auto; Ask and YOLO bypass it through the mode provider.
func NewGate(opts Options) *Gate {
	if opts.Mode == nil {
		opts.Mode = func() string { return "auto" }
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.MaxReviewBlocks <= 0 {
		opts.MaxReviewBlocks = MaxReviewRejects
	}
	g := &Gate{
		opts:           opts,
		tasks:          map[string]*taskRuntime{},
		waiters:        map[string]chan resolvePayload{},
		taskOf:         map[string]string{},
		pending:        map[string]PendingProposal{},
		awaiting:       map[string]struct{}{},
		episodeSeq:     1,
		episodeID:      "ep:1",
		generation:     1,
		persistPending: map[string]int{},
		persistDone:    map[string]uint64{},
	}
	g.persistCond = sync.NewCond(&g.persistMu)
	return g
}

// EpisodeID returns the current host-owned Recovery Episode id.
func (g *Gate) EpisodeID() string {
	if g == nil {
		return ""
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.episodeID
}

// Generation returns the current observation/proposal generation.
func (g *Gate) Generation() uint64 {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.generation
}

// BeginEpisode rotates into a fresh Recovery Episode. Failure, reviewer, and
// stop budgets clear. Explicit task grants and TaskScope authorizations are
// preserved. Call on: real user messages, Plan "start execution", Recovery
// "try another approach", real tool-approval mode changes, and new Session /
// Controller restore. Same-value mode replays must not call this.
func (g *Gate) BeginEpisode() {
	if g == nil {
		return
	}
	dismissed := g.beginEpisodeLockedCollect(true)
	g.finishDismissed(dismissed)
	g.persist()
}

// OnModeChange rotates Episode and generation when the tool-approval mode
// actually changes. Same-value replays (desktop hydration/reconcile) are no-ops
// so in-flight Auto state is not wiped. Returns dismissed recovery approval ids
// so the controller can clear matching cards outside the gate lock.
func (g *Gate) OnModeChange(mode string) []string {
	if g == nil {
		return nil
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		return nil
	}
	g.mu.Lock()
	if g.haveMode && g.lastMode == mode {
		g.mu.Unlock()
		return nil
	}
	// First observation only pins the baseline mode (desktop hydrate / initial
	// ApplyToolApprovalMode). Same-value later replays are no-ops above; a real
	// change rotates Episode and generation.
	if !g.haveMode {
		g.lastMode = mode
		g.haveMode = true
		g.mu.Unlock()
		return nil
	}
	g.lastMode = mode
	g.metrics.ModeResets++
	dismissed := g.beginEpisodeLockedCollect(false)
	// Bump generation even when episode collection already did — mode switch
	// must invalidate in-flight observations.
	if g.generation == 0 {
		g.generation = 1
	}
	ids := make([]string, 0, len(dismissed))
	for _, d := range dismissed {
		ids = append(ids, d.id)
	}
	g.mu.Unlock()
	g.finishDismissed(dismissed)
	g.persist()
	return ids
}

// beginEpisodeLockedCollect must be called with g.mu held when alreadyLocked is
// false it acquires the lock. When alreadyHeld is true, caller holds g.mu.
func (g *Gate) beginEpisodeLockedCollect(lock bool) []dismissedWaiter {
	if lock {
		g.mu.Lock()
	}
	g.episodeSeq++
	if g.episodeSeq == 0 {
		g.episodeSeq = 1
	}
	g.episodeID = fmt.Sprintf("ep:%d", g.episodeSeq)
	g.generation++
	if g.generation == 0 {
		g.generation = 1
	}
	g.metrics.EpisodeRotations++
	// Episode-level hard-stop budgets reset for every TaskID together.
	g.episode.clear()
	// Clear task-local operation counters; preserve task grants.
	for id, st := range g.tasks {
		if st == nil {
			delete(g.tasks, id)
			continue
		}
		grants := st.taskGrants
		grantScope := st.taskGrantScope
		st.clearTaskRecoveryState()
		st.episodeID = g.episodeID
		st.taskGrants = grants
		st.taskGrantScope = grantScope
		if !st.hasTaskGrants() && st.empty() {
			delete(g.tasks, id)
		}
	}
	dismissed := g.collectWaitersLocked(resolvePayload{
		action:   ActionRevise,
		feedback: "Tool approval mode or recovery episode changed. Re-evaluate under the new mode; the previous proposal was not approved.",
	})
	if lock {
		g.mu.Unlock()
	}
	return dismissed
}

func (g *Gate) collectWaitersLocked(payload resolvePayload) []dismissedWaiter {
	out := make([]dismissedWaiter, 0, len(g.waiters))
	for id, ch := range g.waiters {
		taskID := g.taskOf[id]
		out = append(out, dismissedWaiter{id: id, taskID: taskID, reply: ch, payload: payload})
		delete(g.waiters, id)
		delete(g.taskOf, id)
		delete(g.pending, id)
		delete(g.awaiting, taskID)
	}
	return out
}

func (g *Gate) finishDismissed(dismissed []dismissedWaiter) {
	for _, d := range dismissed {
		if d.reply == nil {
			continue
		}
		select {
		case d.reply <- d.payload:
		default:
		}
	}
}

// Metrics returns a copy of content-free counters accumulated since gate
// construction or the most recent DrainMetrics call.
func (g *Gate) Metrics() Metrics {
	if g == nil {
		return Metrics{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.metrics
}

// DrainMetrics atomically returns and clears recovery counters accumulated
// since the last drain. Desktop telemetry uses this delta API at TurnDone so a
// historical event is never counted again on later turns.
func (g *Gate) DrainMetrics() Metrics {
	if g == nil {
		return Metrics{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.metrics
	g.metrics = Metrics{}
	return out
}

// FlushPersistence waits until every snapshot already scheduled for key has
// finished. Session destruction uses this before removing sidecars so a late
// asynchronous write cannot resurrect an artifact that was just deleted.
func (g *Gate) FlushPersistence(key string) {
	if g == nil || g.opts.Persist == nil {
		return
	}
	g.persistMu.Lock()
	for g.persistPending[key] > 0 {
		g.persistCond.Wait()
	}
	g.persistMu.Unlock()
}

// HasApproval reports whether a live Auto decision waiter is parked under id.
// Unlike Snapshot, this includes normal-execution plan transitions that have a
// waiter but no armed failure/taskRuntime yet. Legacy Approve paths must use
// this (or Resolve) instead of inferring from a persistence snapshot.
func (g *Gate) HasApproval(id string) bool {
	if g == nil {
		return false
	}
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "pending:") {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.waiters[id]; ok {
		return true
	}
	_, ok := g.taskOf[id]
	return ok
}

// Snapshot returns a live debug copy of task state (may include budgets).
func (g *Gate) Snapshot() Snapshot {
	if g == nil {
		return Snapshot{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.snapshotLocked(false)
}

// PersistenceSnapshot returns the disk projection: historical last_failure
// evidence only. Active locks, Episode counters, generation, and waiters never
// appear.
func (g *Gate) PersistenceSnapshot() Snapshot {
	if g == nil {
		return Snapshot{}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.snapshotLocked(true)
}

func (g *Gate) snapshotLocked(persistence bool) Snapshot {
	// Map task id -> live approval id for observability. Restore always drops
	// these fields so a restart never replays a transient authorization.
	approvalByTask := map[string]string{}
	if !persistence {
		for approvalID, taskID := range g.taskOf {
			if strings.HasPrefix(approvalID, "pending:") {
				continue
			}
			approvalByTask[taskID] = approvalID
		}
	}
	out := Snapshot{Tasks: map[string]*TaskState{}}
	for id, st := range g.tasks {
		var cp *TaskState
		if persistence {
			cp = st.toPersistenceState()
		} else {
			phase := PhaseDiagnosing
			if _, waiting := g.awaiting[id]; waiting {
				phase = PhaseAwaitingDecision
			}
			cp = st.toTaskState(phase)
		}
		if cp == nil {
			continue
		}
		if !persistence {
			if aid := approvalByTask[id]; aid != "" {
				cp.ApprovalID = aid
				cp.Phase = PhaseAwaitingDecision
			}
			// Project shared Episode budgets onto each task for live debug views.
			cp.ReviewBlocks = int(g.episode.reviewRejects)
			cp.EpisodeStopped = g.episode.stopped
			cp.StopReason = string(g.episode.stopReason)
			if cp.EpisodeID == "" {
				cp.EpisodeID = g.episodeID
			}
		}
		out.Tasks[id] = cp
	}
	return out
}

// BindApprovalID associates a prompt id with the task waiting on it so
// Resolve can find the waiter after EmitPrompt returns. If a provisional
// waiter is parked under pending:<taskID>, it is re-keyed to approvalID.

// Resolve applies a user decision to a pending Auto Guard approval.
// action is continue|continue_task|revise. For revise, feedback is returned through the
// blocked tool result and the current mutation is refused in the same operation.

// Human continue does not reset Episode reviewer rejects; only real
// mutation/verification progress, a new Episode, or revise does.

// Revise rejects the pending action and starts a fresh Recovery Episode
// so alternative approaches get a clean budget.

// Fresh Episode after "try another approach" so alternatives get a clean
// budget. BeginEpisode also dismisses any other waiters safely.

// ObserveResult implements agent.RecoveryGate. It returns one-shot guidance
// for the caller to enqueue on the exact Agent.Run that observed the failure.

// Stale observations from a previous generation (mode switch / episode
// rotate mid-flight) are ignored so they cannot re-arm old locks.

// Successful host-recognized verification clears Episode no-progress budgets.

// Any successful mutation ends the current no-progress budget.

// Diagnostic read successes do not clear failure state. Preserve a bounded
// evidence excerpt for the isolated reviewer; otherwise it sees the failure
// and proposed diff but none of the investigation that connected them.

// Episode totals accumulate across every TaskID (root + sub-agents).

// Keep diagnosis notes if same fingerprint; otherwise start fresh list.

// BeforeMutation implements agent.RecoveryGate.

// Host-proven read-only diagnostics always continue, including after the
// Episode execution budget is exhausted. Decide also encodes the non-Auto
// bypass so Ask and YOLO keep their existing semantics.

// Escalation: re-proposing an already-stopped operation burns the
// stopped-op retry budget and may stop the whole turn.

// Still under retry budget: fall through to RouteStop for this op.

// Episode total failure hard stop before reviewer work.

// MarkFinalizationOffered records that the agent was given its one summarize-only
// round after an Episode stop. Subsequent tool proposals while still stopped
// should surface RecoveryPauseError.

// ConsumeFinalization reports whether the finalization round already ran and
// the model still attempted tools. Also marks it consumed on first true check
// after offered. Finalization is Episode-scoped (shared by all TaskIDs).

// EpisodeStopped reports whether the shared Recovery Episode is exhausted for
// any TaskID (root or sub-agent).

// clearNoProgressLocked clears Episode totals and the observing task's local
// counters after real mutation/verification success. Caller holds g.mu.

// classify builds pure Facts for Decide. It never calls the model or UI.

// Deterministic boundary checks run before the failure-recovery path.

// Operation failure accounting intentionally excludes Preview. Agent calls
// always carry a display/approval preview, while completed observations do
// not; mixing the two shapes would make an exact retry look like an unseen
// operation and bypass its three-failure stop. Keep the preview-bound
// fingerprint for one-shot human approval below.

// Leaving Auto does not wait for the next proposal: OnModeChange handles
// real mode switches. Here we still clear when mode is non-Auto so a
// bypass path cannot keep armed Auto locks if OnModeChange was skipped.

// Shared Episode budget applies even when this TaskID has no local state.

// Align task runtime with current Episode without wiping mid-Episode.

// When proposing the same op, FailureCount is the map value.
// When proposing a different op after failures, HasActiveFailure
// remains true for accounting, but Decide keeps that unrelated
// operation on the automatic path.

// Evidence exists but count was cleared somehow — treat as 1.

// If Episode reviewer budget already exhausted, stop the turn.

// Reviewer Continue does NOT reset cumulative rejects. Only real
// mutation/verification success, a new Episode, or revise does.

// Create the waiter channel before EmitPrompt. Resolve may race in as soon
// as the approval id is known (desktop/bot), so re-key the waiter under the
// real id immediately after EmitPrompt returns.

// EmitPrompt implementations may bind the real id before emitting, which
// lets a synchronous frontend resolve the card before EmitPrompt returns.
// Only re-key a waiter that is still provisional; if both mappings are gone,
// Resolve already completed and its buffered payload is waiting on reply.

// Root task ids span a controller session. TaskScopeID is host-owned and
// unique per ordinary turn, while goal continuations reuse their delivery
// scope. Hash it so task-local runtime state never contains raw task text.

// RecordDiagnosis appends a diagnosis note while recovering.

// internals

// Disk never receives active lock state.

// Caller holds g.mu.

// userFacingReason is the short localized-friendly reason shown on the card.

// Cumulative across all candidates and TaskIDs inside the Episode.

// Unparseable/unknown outcome fails closed.

// Cannot silently continue without a clear bounded-recovery label.

// Risk and uncertainty cannot silently continue, but they are technical
// blockers rather than human approval requests. Strategy/scope may continue
// when the reviewer established that the change remains task-aligned.

// Ensure Gate implements agent.RecoveryGate.
var _ agent.RecoveryGate = (*Gate)(nil)
