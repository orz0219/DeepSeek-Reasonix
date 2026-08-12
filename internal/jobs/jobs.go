// Package jobs is the session-scoped background-job registry behind the agent's
// background tools (bash run_in_background, task run_in_background) and the
// bash_output / kill_shell / wait tools. A Manager owns a context whose lifetime
// is the session, NOT a single turn — so a job started in one turn keeps running
// across turns and is cancelled only when the controller closes (or kill_shell is
// called). Tools reach the Manager through the call context (WithManager /
// FromContext), the same injection pattern the `ask` tool uses for the asker.
//
// The Manager emits a user-visible Notice when a job starts and finishes, and
// accumulates a one-line completion summary that the controller drains into the
// next turn (DrainCompletedNote) so the model itself learns of completions.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/nilutil"
)

var renamePath = os.Rename
var repairArtifactMeta = writeMeta

var (
	managerOwnerSeq   atomic.Uint64
	liveManagerOwners = struct {
		sync.RWMutex
		ids map[string]struct{}
	}{ids: map[string]struct{}{}}
)

// Status is a job's lifecycle state.
type Status string

// DefaultTeardownGrace bounds Close and destroy waits for non-cooperative jobs.
const DefaultTeardownGrace = 15 * time.Second

// View is a read-only snapshot of a job for the status bar.
type View struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"` // unix milliseconds
}

// Result is one job's terminal (or current) state returned by Wait.
type Result struct {
	ID     string
	Kind   string
	Label  string
	Status Status
	Output string // the terminal result text, or the streamed buffer when no result was set
}

// TeardownJob identifies a job that is still unwinding after teardown waited.
type TeardownJob struct {
	ID     string
	Kind   string
	Label  string
	Waited time.Duration
}

// TeardownResult reports jobs that did not unwind within the teardown grace.
type TeardownResult struct {
	TimedOut []TeardownJob
}

// HasTimedOut reports whether teardown returned before every job had unwound.
func (r TeardownResult) HasTimedOut() bool { return len(r.TimedOut) > 0 }

type teardownTarget struct {
	info TeardownJob
	done <-chan struct{}
}

// SessionTeardown is the destroy handle for a session's owned background jobs.
type SessionTeardown struct {
	SessionID string
	targets   []teardownTarget
}

// Async reports whether the handle has jobs to wait on.
func (h SessionTeardown) Async() bool { return len(h.targets) > 0 }

// DoneChannels returns each target's completion channel for legacy callers.
func (h SessionTeardown) DoneChannels() []<-chan struct{} {
	out := make([]<-chan struct{}, 0, len(h.targets))
	for _, target := range h.targets {
		out = append(out, target.done)
	}
	return out
}

// Job is one background job. The mutex guards the streaming buffer and the
// terminal fields; the run goroutine writes them, readers (Output/Wait/snapshots)
// take the same lock.
type Job struct {
	ID        string
	Kind      string // "bash" | "task"
	Label     string
	SessionID string

	mu          sync.Mutex
	tail        []byte
	readOffset  int64
	status      Status
	result      string
	resultRead  bool // result already surfaced by Output (task jobs stream nothing to buf)
	startedAt   int64
	finishedAt  int64
	activityAt  int64
	runReturned bool
	cancel      context.CancelFunc
	done        chan struct{}
	stalled     bool

	artifactPath     string
	artifactMetaPath string
	artifactFile     *os.File
	artifactComplete bool
	artifactErr      string
	tombstone        bool

	evidence          evidence.ChildEvidenceSummary
	evidenceCommitted bool
}

// Manager is the session's background-job table. It is safe for concurrent use.
type Manager struct {
	sink       event.Sink
	root       context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	onJobStart func(done <-chan struct{})
	ownerID    string
	ownerDone  sync.Once
	// sessionOwnershipProbe authorizes destructive repair of persisted running
	// artifacts. A nil probe is conservative: an observer that cannot prove it
	// owns the transcript must never publish an interrupted tombstone.
	sessionOwnershipProbe func(path string) bool

	mu           sync.Mutex
	seq          int
	jobs         map[string]*Job
	order        []string
	completed    []completion // finished-job summaries awaiting drain into the next turn
	active       string
	destroying   map[string]bool
	artifactDirs map[string]string
	loaded       map[string]bool
	tempRoot     string
	reservations map[string]int

	stalledWarning time.Duration
	teardownGrace  time.Duration

	taskRecorder TaskRecorder // optional task-monitoring lifecycle hook
}

type completion struct {
	sessionID string
	text      string
}

// Option configures a Manager.
type Option func(*Manager)

// TaskRecorder observes background-job lifecycle for task monitoring. The
// store-backed write side lives outside jobs (typically internal/taskmonitor);
// jobs only calls the hooks. RecordStart runs on the caller's goroutine,
// RecordDone on the job's own goroutine — implementations must be safe for
// concurrent use and must not block or fail the job pipeline (best-effort).
type TaskRecorder interface {
	RecordStart(id, kind, label string)
	RecordDone(id string, st Status, err error)
}

// WithStalledWarningAfter enables one stalled warning per job after d without
// job-owned visible output. A non-positive duration disables stalled warnings.
func WithStalledWarningAfter(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.stalledWarning = d
		}
	}
}

// WithTeardownGrace overrides the Close/destroy grace window. Tests can set a
// short value; production uses DefaultTeardownGrace.
func WithTeardownGrace(d time.Duration) Option {
	return func(m *Manager) {
		if d >= 0 {
			m.teardownGrace = d
		}
	}
}

// WithJobStartObserver observes every registered background job before its
// goroutine starts. Delivery uses this to retain a workspace writer lease until
// the job is truly terminal. The callback must return quickly.
func WithJobStartObserver(observer func(done <-chan struct{})) Option {
	return func(m *Manager) { m.onJobStart = observer }
}

// WithSessionOwnershipProbe supplies the runtime ownership check used when
// loading persisted Running artifacts. The probe must return true only when the
// current runtime owns the session transcript for writing.
func WithSessionOwnershipProbe(probe func(path string) bool) Option {
	return func(m *Manager) { m.sessionOwnershipProbe = probe }
}

// WithTaskRecorder installs an optional background-job lifecycle recorder for
// task monitoring. A nil recorder disables recording.
func WithTaskRecorder(r TaskRecorder) Option {
	return func(m *Manager) { m.taskRecorder = r }
}

// SetTaskRecorder installs (or clears, with nil) the lifecycle recorder after
// construction. Controllers that assemble their job manager before the
// recorder's dependencies (workspace root, session id) are known use this.
func (m *Manager) SetTaskRecorder(r TaskRecorder) { m.taskRecorder = r }

// TeardownGrace reports the manager's configured close/destroy wait window.
func (m *Manager) TeardownGrace() time.Duration { return m.teardownGrace }

// NewManager returns a Manager whose jobs run under a fresh session-scoped
// context (cancelled by Close). sink receives job-lifecycle notices; pass the
// session's synchronized sink (event.Sync) since jobs emit from goroutines.
func NewManager(sink event.Sink, opts ...Option) *Manager {
	if nilutil.IsNil(sink) {
		sink = event.Discard
	}
	root, cancel := context.WithCancel(context.Background())
	tempRoot, _ := os.MkdirTemp("", "reasonix-jobs-*")
	m := &Manager{
		sink:          sink,
		root:          root,
		cancel:        cancel,
		jobs:          map[string]*Job{},
		destroying:    map[string]bool{},
		artifactDirs:  map[string]string{},
		reservations:  map[string]int{},
		loaded:        map[string]bool{},
		tempRoot:      tempRoot,
		teardownGrace: DefaultTeardownGrace,
		ownerID:       newManagerOwnerID(),
	}
	registerManagerOwner(m.ownerID)
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

func newManagerOwnerID() string {
	var token [16]byte
	if _, err := rand.Read(token[:]); err == nil {
		return hex.EncodeToString(token[:])
	}
	return fmt.Sprintf("%d-%d-%d", os.Getpid(), time.Now().UnixNano(), managerOwnerSeq.Add(1))
}

func registerManagerOwner(ownerID string) {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return
	}
	liveManagerOwners.Lock()
	liveManagerOwners.ids[ownerID] = struct{}{}
	liveManagerOwners.Unlock()
}

func managerOwnerIsLive(ownerID string) bool {
	ownerID = strings.TrimSpace(ownerID)
	if ownerID == "" {
		return false
	}
	liveManagerOwners.RLock()
	_, ok := liveManagerOwners.ids[ownerID]
	liveManagerOwners.RUnlock()
	return ok
}

func (m *Manager) releaseOwner() {
	if m == nil {
		return
	}
	m.ownerDone.Do(func() {
		liveManagerOwners.Lock()
		delete(liveManagerOwners.ids, m.ownerID)
		liveManagerOwners.Unlock()
	})
}

// jobWriter appends a job's streamed output under its lock so a concurrent
// Output read never races the producing goroutine.
type jobWriter struct{ j *Job }

func (w jobWriter) Write(p []byte) (int, error) {
	w.j.mu.Lock()
	defer w.j.mu.Unlock()
	w.j.activityAt = nowMs()
	w.j.tail = appendTail(w.j.tail, p, defaultTailBytes)
	if w.j.artifactFile != nil {
		if _, err := w.j.artifactFile.Write(p); err != nil {
			w.j.artifactErr = err.Error()
		}
	}
	return len(p), nil
}

// Start launches run on a goroutine under the manager's session context and
// returns the job immediately. run streams output to the writer and returns the
// terminal result text (a task's final answer; a bash job streams everything to
// the buffer and returns ""). The job is marked killed when its context was
// cancelled, failed on any other error, else done.
func (m *Manager) Start(kind, label string, run func(ctx context.Context, out io.Writer) (string, error)) *Job {
	return m.StartForSession("", kind, label, run)
}

// validatePathSegment rejects values that would let parentSession or kind
// escape the temp-root fallback built by artifactDirLocked. Persistent artifact
// directories bound by SetActiveSessionPath are trusted store paths and are
// intentionally outside that temp root. The check is intentionally conservative:
// it forbids any path-separator character (forward slash, backslash), NUL, and
// any control character. Empty parentSession is allowed (the unscoped default);
// kind must be non-empty.
//
// See #6932. Before this check existed, a malicious or malformed parentSession
// such as "../../etc" combined with filepath.Join(tempRoot, parentSession, id)
// resolved to a directory outside the manager's temp root, allowing the
// subsequent os.MkdirAll + os.OpenFile to create files at locations controlled
// by the caller (subject to the running process's filesystem permissions).
func validatePathSegment(name, field string) error {
	if field == "kind" && name == "" {
		return fmt.Errorf("jobs: %s must not be empty", field)
	}
	for i, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("jobs: %s contains control character 0x%02x at index %d", field, r, i)
		case r == '/' || r == '\\':
			return fmt.Errorf("jobs: %s contains path separator %q at index %d", field, r, i)
		}
	}
	if name == "." || name == ".." {
		return fmt.Errorf("jobs: %s is reserved (%q)", field, name)
	}
	return nil
}

// startInvalid registers a job that failed validation BEFORE any goroutine or
// artifact was created. The job is observable to Wait / list calls as Failed
// with the validation error recorded in artifactErr, and no run goroutine is
// started so the manager's wg is unaffected.
func (m *Manager) startInvalid(parentSession, kind, label string, validationErr error) *Job {
	finishedAt := nowMs()
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("invalid-%d", m.seq)
	j := &Job{
		ID:               id,
		Kind:             kind,
		Label:            label,
		SessionID:        parentSession,
		status:           Failed,
		startedAt:        finishedAt,
		activityAt:       finishedAt,
		finishedAt:       finishedAt,
		runReturned:      true,
		cancel:           func() {},
		done:             make(chan struct{}),
		artifactComplete: false,
		artifactErr:      validationErr.Error(),
	}
	key := jobKey(parentSession, id)
	m.jobs[key] = j
	m.order = append(m.order, key)
	m.mu.Unlock()
	close(j.done)
	m.recordCompletion(parentSession, id, kind, label, Failed, validationErr)
	return j
}

// StartForSession launches a job owned by parentSession. Session-scoped readers
// only see jobs whose owner matches the active session.
func (m *Manager) StartForSession(parentSession, kind, label string, run func(ctx context.Context, out io.Writer) (string, error)) *Job {
	parentSession = strings.TrimSpace(parentSession)
	kind = strings.TrimSpace(kind)
	if err := validatePathSegment(parentSession, "parentSession"); err != nil {
		return m.startInvalid(parentSession, kind, label, err)
	}
	if err := validatePathSegment(kind, "kind"); err != nil {
		return m.startInvalid(parentSession, kind, label, err)
	}
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("%s-%d", kind, m.seq)
	ctx, cancel := context.WithCancel(m.root)
	startedAt := nowMs()
	logPath, metaPath, file, artifactErr := m.openArtifactLocked(parentSession, id)
	j := &Job{
		ID:               id,
		Kind:             kind,
		Label:            label,
		SessionID:        parentSession,
		status:           Running,
		startedAt:        startedAt,
		activityAt:       startedAt,
		cancel:           cancel,
		done:             make(chan struct{}),
		artifactPath:     logPath,
		artifactMetaPath: metaPath,
		artifactFile:     file,
		artifactComplete: artifactErr == "",
		artifactErr:      artifactErr,
	}
	ctx = WithSession(ctx, parentSession)
	ctx = context.WithValue(ctx, jobCtxKey{}, j)
	key := jobKey(parentSession, id)
	m.jobs[key] = j
	m.order = append(m.order, key)
	m.mu.Unlock()
	j.mu.Lock()
	if err := m.writeJobMetaLocked(j, Running); err != nil {
		j.artifactComplete = false
		j.artifactErr = err.Error()
	}
	j.mu.Unlock()
	if m.onJobStart != nil {
		m.onJobStart(j.done)
	}

	m.emitIfActive(parentSession, event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: startedText(kind, id, label)})

	if !nilutil.IsNil(m.taskRecorder) {
		m.taskRecorder.RecordStart(id, kind, label)
	}

	m.wg.Add(1)
	if m.stalledWarning > 0 {
		m.wg.Add(1)
		go m.monitorStalled(parentSession, j)
	}
	go func() {
		defer m.wg.Done()
		result, err := runRecovered(ctx, jobWriter{j}, run)
		j.mu.Lock()
		j.runReturned = true
		j.mu.Unlock()

		var st Status
		switch {
		case ctx.Err() != nil:
			st = Killed
		case err != nil:
			st = Failed
			if result == "" {
				result = err.Error()
			}
		default:
			st = Done
		}
		finishedAt := nowMs()
		if result != "" {
			j.mu.Lock()
			if j.artifactFile != nil {
				if _, writeErr := j.artifactFile.WriteString(result); writeErr != nil {
					j.artifactErr = writeErr.Error()
				}
			} else {
				j.result = result
			}
			j.tail = appendTail(j.tail, []byte(result), defaultTailBytes)
			j.mu.Unlock()
		}
		targetDir := m.artifactTargetDirForJob(j)
		j.mu.Lock()
		if j.artifactFile != nil {
			if closeErr := j.artifactFile.Close(); closeErr != nil && j.artifactErr == "" {
				j.artifactErr = closeErr.Error()
			}
			j.artifactFile = nil
		}
		if j.artifactErr != "" {
			j.artifactComplete = false
		}
		j.finishedAt = finishedAt
		if targetDir != "" {
			if moveErr := j.moveArtifactToDirLocked(targetDir); moveErr != nil {
				j.noteArtifactErr("migration: " + moveErr.Error())
			}
		}
		metaErr := m.writeJobMetaLocked(j, st)
		if metaErr != nil {
			j.noteArtifactErr("metadata: " + metaErr.Error())
		}
		j.mu.Unlock()
		// Queue the drain note (and emit the closing Notice) BEFORE publishing the
		// terminal status. Wait(nil)/resolve only block on Running jobs, so if the
		// status flipped to terminal before the note was queued, a Wait could observe
		// completion, skip j.done, and DrainCompletedNote would race ahead of the
		// bookkeeping (the TestDrainMultiple -race flake). Recording first makes an
		// observed terminal status imply the note is already queued.
		m.recordCompletion(parentSession, id, kind, label, st, err)

		j.mu.Lock()
		if j.status != Killed { // a concurrent Kill already published Killed — keep it
			j.status = st
		}
		if j.artifactPath != "" && j.artifactComplete {
			j.result = ""
			j.tail = nil
		}
		j.mu.Unlock()
		close(j.done)
	}()
	return j
}

// O_TRUNC does not apply the requested mode to an existing artifact. Tighten
// it before any raw tool output is written so upgrades cannot append secrets
// to a legacy 0644 log.

// MkdirAll leaves an existing 0755 directory unchanged.

// Any version this build cannot parse — a pre-feature artifact
// (version 0) or one written by a newer build — is treated as an
// opaque mutation. A missing summary only proves the mutation state
// was not recorded, not that the task made no changes: a legacy
// background writer task collected after upgrade could carry real,
// unreviewed edits. Recovering it as opaque RiskHigh forces fresh
// inspection and review rather than silently skipping it, and keeps
// downgrade coexistence on a shared state directory conservative.

// Same-version artifact with no summary: this build DID record the
// mutation state and found none, so there is genuinely nothing to
// recover.

// Known paths preserve the original adaptive risk level while still
// requiring fresh inspection and verification after recovery.

// The original risk may have come from an opaque or privileged tool,
// which the sanitized artifact intentionally does not retain. Recover it
// as opaque so restart cannot downgrade the security-review requirement.

// recordCompletion queues the finished-job summary for DrainCompletedNote and
// emits a closing Notice (warn for a failure, info otherwise).

// Output returns the job's output produced since the last Output call plus its
// current status. ok is false when the id is unknown.

// OutputForSession returns output only when id belongs to parentSession. Empty
// parentSession preserves the legacy unscoped behavior.

// A task job streams nothing to the buffer — its answer lands in result. Once
// it is terminal with no buffered output, surface that result once so a task's
// answer is visible here too (bash_output's description promises task support).

// readArtifactAllLocked deliberately reads raw bytes: the artifact is captured
// subprocess output (possibly binary), not a user-edited config file, and the
// incremental reader (readArtifactSinceOffsetLocked) is raw byte-offset based —
// decoding only the whole-file path would render the same artifact in two
// different encodings and could garble binary output via UTF-16 misdetection.

// Kill cancels a running job. Returns false when the id is unknown or the job has
// already finished.

// KillForSession cancels a running job only when it belongs to parentSession.
// Empty parentSession preserves the legacy unscoped behavior.

// Flip to Killed synchronously so Output/Wait reflect the kill the instant
// it's requested, not whenever the run goroutine's cmd.Run returns (which
// trails by WaitDelay while a cancelled process tree tears down). The
// goroutine still sets Killed + records completion on return; this only
// fires when the job is actually Running, so a job that just finished
// keeps its real terminal status.

// Wait blocks until the named jobs (or every currently-running job when ids is
// empty) reach a terminal state, or ctx is cancelled, or timeoutSec elapses
// (0 = no timeout). It returns each target's snapshot regardless of why it
// returned, so a timeout still reports partial progress.

// WaitForSession waits only on jobs owned by parentSession. Empty parentSession
// preserves the legacy unscoped behavior.

// resolve maps requested ids to jobs; an empty list selects all running jobs.

// Running returns a snapshot of the still-running jobs (for the status bar).

// RunningForSession returns still-running jobs owned by parentSession. Empty
// parentSession preserves the legacy unscoped behavior.

// A cancellation request flips the persisted/result status to Killed
// synchronously, but the process tree may still be unwinding. Keep the job
// on the operational running surface until its done channel closes so
// Desktop rebuild guards and Delivery workspace leases cannot declare the
// runtime idle early. The public view remains "running" while a stop is
// in flight; clients may render a local "stopping" state after they
// request cancellation.

// ReserveStartForSession atomically reserves capacity for a job start. The
// caller must release the reservation after StartForSession has registered the
// job (or when setup fails). Running jobs and in-flight start reservations both
// count toward limit, so concurrent callers cannot overshoot it.

// HasUnfinishedForSession reports whether parentSession owns any job whose
// goroutine has not fully exited yet. Empty parentSession preserves the legacy
// unscoped behavior.

// DrainCompletedNote returns (and clears) a one-line summary of jobs that
// finished since the last drain, for the controller to fold into the next turn
// so the model learns of completions. "" when nothing finished.

// DrainCompletedNoteForSession drains completion notes for parentSession only.
// Notes for other sessions stay queued until that session becomes active again.
// Empty parentSession preserves the legacy unscoped behavior.

// SetActiveSession controls which session receives lifecycle notices for jobs
// that finish asynchronously. Empty active session preserves legacy behavior.

// validateTrustedSessionPath performs defense-in-depth syntax validation on a
// transcript path already trusted by the store/controller layer. It rejects
// control characters, but deliberately preserves separators and `..`: those are
// valid host-path syntax, and rejecting them without a trusted root would break
// legitimate relative paths without establishing filesystem containment.

// SetActiveSessionPath binds a parent session id to its persistent transcript
// path, migrates any temporary artifacts, and loads completed job tombstones from
// the session sidecar. sessionPath must come from the trusted store/controller
// path; this method does not establish filesystem containment on its own.

// Preserve the legacy active-only behavior for calls without a complete
// binding. In particular, an empty path is not an error or filesystem input.

// Reject malformed trusted paths before any filesystem side effect. This is
// syntax hardening, not a boundary for arbitrary caller-controlled paths.

// A rejected rebinding must not leave future jobs writing to a stale
// transcript that happened to use the same parent session id.

// A rename preserves the source mode, so tighten legacy artifacts before
// either the fast rename or the cross-device copy fallback.

// A persisted Running record may belong to another manager in this
// process or to another Reasonix process entirely. Only the runtime that
// owns the session lease may repair an abandoned record as Interrupted.
// Observers without proof of ownership defer the artifact and leave the
// session reloadable for a later owned bind.

// Do not publish an in-memory Interrupted tombstone when the durable
// state still says Running. Keep the session reloadable so a later bind
// can retry the repair, and surface the failure instead of letting live
// and machine-facing status silently disagree.

// BeginDestroySession marks a parent session as being removed from active use
// and cancels its running jobs. WaitTeardown waits for the returned handle.

// DestroySession preserves the legacy channel-based destroy API.

// WaitTeardown waits for a destroy handle to unwind up to grace. A timed-out
// result means the caller should defer physical cleanup until the jobs exit.

// IsDestroying reports whether parentSession is in the destroy window. Empty
// parent sessions are never considered destroyed.

// FinishDestroySession ends the destroy window after all owned jobs have unwound
// and persistent cleanup/move work has completed.

// Close cancels the session context and waits briefly for every background job
// goroutine to return before unblocking. If a non-cooperative job ignores
// cancellation, cleanup of the temporary artifact root continues in the
// background after the goroutines eventually unwind.

// CloseAsync cancels the manager and returns immediately. It is used when a
// caller has already begun session-specific teardown and owns the delayed
// persistent cleanup, but still needs the manager's root context and temporary
// artifact root released eventually.

// CloseWithGrace is Close with an explicit wait window, used by tests and
// callers that need to surface non-cooperative jobs.

// call-context injection (mirrors agent.CallContext)

// WithManager stamps ctx with the job manager so tools can reach it via
// FromContext. The agent sets this on every tool call's context.

// WithoutManager shadows an ancestor manager. Child agents without an owned
// Jobs manager must not operate the parent's background jobs by inheritance.

// FromContext returns the job manager set by the agent, if any. ok is false for a
// plain context (headless tests, calls outside the run loop).

// WithSession stamps ctx with the active parent session ID for session-scoped job
// operations.

// SessionFromContext returns the active parent session ID for job ownership and
// filtering. Empty means no session scope is available.

// PublishEvidence attaches a background agent's host-observed receipts to its
// job. The receipts stay independent of the parent turn ledger until the
// parent collects the terminal result with wait or bash_output.

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

// PendingEvidenceJobIDsForSession returns the IDs of parentSession's terminal
// jobs that carry uncommitted mutation evidence — a prior turn leased it but
// never delivered (the turn failed or was cancelled, and the next turn's Reset
// wiped it from the per-turn ledger), or the process restarted before any turn
// collected it at all. The agent re-leases these at the start of every turn so
// a turn that never calls wait/bash_output still surfaces the pending mutation
// to its final-readiness checks instead of silently shipping it unreviewed.

// CommitEvidenceForSession permanently consumes a terminal job's evidence after
// the collecting turn has accounted for it (passed final-readiness). It clears
// the in-memory copy and drains the persisted mutation summary so neither a
// same-process re-poll nor a restart resurrects receipts the delivered turn
// already reviewed. Best-effort on the disk rewrite — a failed rewrite merely
// restores the conservative resurrection behavior.
