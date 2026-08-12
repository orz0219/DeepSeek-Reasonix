package acp

import (
	"context"
	"io"
	"sync"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/plugin"
	"reasonix/internal/tool/builtin"
)

// SessionParams is everything a Factory needs to assemble one ACP session's
// controller. Sink is owned by this package (an updateSink bound to the session
// id) and must be wired into the controller's event sink; the controller's
// interactive approval (see control.Controller.EnableInteractiveApproval) then
// routes "ask" decisions back through that sink as ApprovalRequest events, which
// the sink forwards to the client over session/request_permission.
//
// Cwd roots the session's file tools and bash (built via builtin.Workspace).
// Model, EffortOverride, and RuntimeProfile are optional session-local selectors
// from ACP config options. MCPServers are the MCP servers the client asked the
// agent to connect for this session. OnSessionRecovered is the service's
// bookkeeping hook for automatic transcript recovery branches (see
// sessionRecoveredHandler); factories must wire it into the controller they build.
type SessionParams struct {
	Cwd                string
	MCPServers         []plugin.Spec
	Sink               event.Sink
	Model              string
	EffortOverride     *string
	RuntimeProfile     string
	OnSessionRecovered func(control.SessionRecoveryInfo) error
	// FileOverlay and Terminal are non-nil when the client advertised the
	// matching capability at initialize: file tools then see unsaved editor
	// buffers, and foreground bash can run in a client-owned terminal.
	// Factories thread them into the controller's tool assembly.
	FileOverlay builtin.FileOverlay
	Terminal    builtin.TerminalRunner
}

// Factory builds the per-session controller. The composition root (the cli's
// `reasonix acp` command) implements it by reusing setup()'s assembly: a
// Provider for Model, a tool Registry rooted at Cwd via builtin.Workspace, a
// per-session MCP host from MCPServers, the event Sink, all wired into a
// control.Controller. The returned controller owns its own cleanup (Close stops
// MCP subprocesses), so the service calls ctrl.Close() on teardown.
type Factory interface {
	NewSession(ctx context.Context, p SessionParams) (*control.Controller, error)
}

// SessionConfigStateParams asks the Factory for normalized session config
// selectors. Empty Model and RuntimeProfile use configured defaults. Nil
// EffortOverride means provider config wins; a non-nil empty string means
// provider default for this session.
type SessionConfigStateParams struct {
	Cwd            string
	Model          string
	EffortOverride *string
	RuntimeProfile string
}

// SessionConfigState is the complete ACP-visible config state for a session.
type SessionConfigState struct {
	Model          string
	EffortOverride *string
	RuntimeProfile string
	Models         *SessionModelState
	ConfigOptions  []SessionConfigOption
}

// SessionConfigStateProvider lets a Factory expose model, effort, and work-mode
// selectors without making the ACP transport depend on a concrete config backend.
type SessionConfigStateProvider interface {
	SessionConfigState(ctx context.Context, p SessionConfigStateParams) (SessionConfigState, error)
}

// SessionDirProvider lets a Factory expose the persistent session directory
// without forcing session/list to build a controller first.
type SessionDirProvider interface {
	SessionDir() string
}

// SessionRebuilder lets a Factory rebuild a session's controller via
// boot.Rebuild: the replacement is built with the same boot.Options NewSession
// would use, and the session state (history, approval grants, goal/recovery,
// lifecycle) migrates off old inside the boot layer. The caller keeps the
// swap/close ordering. Factories that do not implement it leave
// _reasonix.io/session/reloadExtensions reporting unavailable.
type SessionRebuilder interface {
	RebuildSession(ctx context.Context, p SessionParams, old *control.Controller) (*control.Controller, error)
}

// AgentInfo identifies this agent to clients in the initialize reply.
type AgentInfo struct {
	Name    string
	Version string
}

// Serve runs an ACP agent on r/w (stdin/stdout in production) until the input
// ends or ctx is cancelled. It owns the JSON-RPC connection and the session
// registry; the Factory supplies the kernel wiring. This is the single entry
// point the `reasonix acp` command calls.
//
// stdout is the JSON-RPC channel: callers must keep all other output (logs,
// diagnostics) off w and on stderr, or the wire corrupts.
func Serve(ctx context.Context, r io.Reader, w io.Writer, factory Factory, info AgentInfo) error {
	conn := NewConn(r, w)
	svc := &service{
		conn:     conn,
		factory:  factory,
		info:     info,
		sessions: make(map[string]*acpSession),
	}
	conn.Handle("initialize", svc.initialize)
	conn.Handle("authenticate", svc.authenticate)
	conn.Handle("session/new", svc.sessionNew)
	conn.Handle("session/load", svc.sessionLoad)
	conn.Handle("session/resume", svc.sessionResume)
	conn.Handle("session/prompt", svc.sessionPrompt)
	conn.Handle(sessionSteerMethod, svc.sessionSteer)
	conn.Handle(sessionInboxEnqueueMethod, svc.sessionInboxEnqueue)
	conn.Handle(sessionInboxListMethod, svc.sessionInboxList)
	conn.Handle(sessionInboxGetMethod, svc.sessionInboxGet)
	conn.Handle(sessionInboxUpdateMethod, svc.sessionInboxUpdate)
	conn.Handle(sessionInboxDeleteMethod, svc.sessionInboxDelete)
	conn.Handle(sessionInboxMoveMethod, svc.sessionInboxMove)
	conn.Handle(sessionInboxPauseMethod, svc.sessionInboxSetPaused)
	conn.Handle(sessionInboxRetryMethod, svc.sessionInboxRetry)
	conn.Handle(sessionInboxRefreshMethod, svc.sessionInboxRefresh)
	conn.Handle(sessionReloadExtensionsMethod, svc.sessionReloadExtensions)
	conn.Handle(sessionStatusMethod, svc.sessionStatus)
	conn.Handle("session/set_config_option", svc.sessionSetConfigOption)
	conn.Handle("session/set_model", svc.sessionSetModel)
	conn.Handle("session/set_mode", svc.sessionSetMode)
	conn.Handle("session/close", svc.sessionClose)
	conn.Handle("session/list", svc.sessionList)
	conn.Handle("session/delete", svc.sessionDelete)
	conn.HandleNotify("session/cancel", svc.sessionCancel)
	defer svc.closeAll()
	return conn.Serve(ctx)
}

// service holds the connection-wide ACP state: the factory, agent identity, and
// the live session registry.
type service struct {
	conn    *Conn
	factory Factory
	info    AgentInfo

	mu       sync.Mutex
	sessions map[string]*acpSession
	// clientCaps is what the client offered at initialize (fs proxy, host
	// terminals). Zero until initialize arrives; sessions opened later bind a
	// clientIO built from it.
	clientCaps ClientCapabilities
}

// afterResponse wraps a result with work that must run after the transport has
// successfully written that result. Session-opening notifications use this so a
// client can register the returned session before receiving its first update.
type afterResponse struct {
	result any
	after  func()
}

func (r afterResponse) Response() any { return r.result }

func (r afterResponse) AfterResponse() {
	if r.after != nil {
		r.after()
	}
}

func (s *service) setClientCapabilities(caps ClientCapabilities) {
	s.mu.Lock()
	s.clientCaps = caps
	s.mu.Unlock()
}

func (s *service) clientCapabilities() ClientCapabilities {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientCaps
}

// extensionSurfaceSupported reports whether the connected client advertised
// reasonix.extensionSurface support in its initialize handshake.
func (s *service) extensionSurfaceSupported() bool {
	return clientExtensionSurfaceSupported(s.clientCapabilities())
}

// clientExtensionSurfaceSupported tolerantly parses the client's vendor
// capability block: _meta["reasonix.io"]["extensionSurface"]["supported"] must
// be an explicit true. Absent keys, wrong shapes, or a malformed block all
// mean unsupported — the sink then sends only the text fallback.
func clientExtensionSurfaceSupported(caps ClientCapabilities) bool {
	vendor, ok := caps.Meta["reasonix.io"].(map[string]any)
	if !ok {
		return false
	}
	capability, ok := vendor["extensionSurface"].(map[string]any)
	if !ok {
		return false
	}
	supported, _ := capability["supported"].(bool)
	return supported
}

// bindClientIO fills SessionParams' overlay/terminal fields from the client's
// declared capabilities. The nil checks keep absent capabilities as nil
// interface fields (a typed-nil *clientIO must never reach the interface).
func (s *service) bindClientIO(p *SessionParams, sessionID string) {
	io := newClientIO(s.conn, sessionID, s.clientCapabilities())
	if !io.hasAny() {
		return
	}
	if fo := io.fileOverlay(); fo != nil {
		p.FileOverlay = fo
	}
	if tr := io.terminalRunner(); tr != nil {
		p.Terminal = tr
	}
}

// acpController is the slice of the controller's driving port the ACP transport
// drives: session lifecycle + persistence, turn execution, interactive approval,
// and the capability surface (commands/skills/MCP prompts). ACP never touches
// goals, checkpoints, or memory, so it depends on those sub-ports only — not the
// concrete *control.Controller.
type acpController interface {
	control.Lifecycle
	control.TurnControl
	TrySteer(text string) bool
	control.Approvals
	control.Capabilities
	control.SessionPersistence
	// Goals backs ACP's normal/plan/goal collaboration-mode surface.
	control.Goals
}

// acpSession is one open session: its controller, the on-disk transcript path
// (empty when persistence is off), and the cancel func of the in-flight turn
// (nil when idle) so session/cancel can abort it.
type acpSession struct {
	id         string
	ctrl       acpController
	sink       *updateSink
	transcript string
	cwd        string
	mcpServers []plugin.Spec
	model      string
	// nil means use config; non-nil empty string means provider default.
	effortOverride   *string
	runtimeProfile   string
	toolApprovalMode string
	// runtimeState is the effective planner/sandbox posture captured after CLI
	// hard overrides. status snapshots never reconstruct it from user config.
	runtimeState SessionRuntimeState
	status       *statusTelemetry
	// modeID is the ACP collaboration mode last reported to the client (normal |
	// plan | goal). Goal draft mode turns the next user prompt into the goal.
	// Both are guarded by mu; controller-side completion/plan exit is reconciled
	// after each turn through current_mode_update.
	modeID        string
	goalDraftMode bool
	// pendingConfig queues config deltas requested while a turn or rebuild is
	// in flight, holding at most one entry per axis: a later request replaces
	// only its own axis (last-write-wins per axis), so a model change and a
	// work-mode change queued back to back during one turn both survive to the
	// drain instead of the second overwriting the first.
	pendingConfig []sessionConfigDelta
	// pendingReload coalesces _reasonix.io/session/reloadExtensions requests
	// made while a turn or a rebuild is in flight; the finishTurn /
	// post-maintenance drains run it once the session is idle.
	pendingReload bool
	title         string
	createdAt     time.Time
	updatedAt     time.Time

	mu sync.Mutex
	// stateChangeMu serializes controller rebuilds with collaboration/approval
	// changes so a swap cannot overwrite a newer user selection.
	stateChangeMu sync.Mutex
	cancel        context.CancelFunc
	done          chan struct{}
	running       bool
	deleted       bool
	// lease is the session lease guarding transcript against other runtimes
	// (a desktop window, the CLI) for the life of this session. Held from
	// session/new / session/load and released on close/delete/teardown.
	// Config rebuilds keep the same transcript; when a snapshot conflict
	// retargets the controller to a recovery branch, sessionRecoveredHandler
	// moves transcript and this lease to the recovery file at commit time.
	lease *agent.SessionLease
	// retiredLeases tracks outgoing leases whose Release must run after the
	// authority-guarded save that triggered a recovery callback returns. Any
	// ACP operation that exposes a completed Snapshot waits for these channels,
	// so callers never observe the old transcript as still owned after the
	// handoff has completed.
	retiredLeases []<-chan struct{}
	// maintenanceDone is non-nil while session-owned maintenance, such as an
	// idle config rebuild, is in flight outside mu.
	maintenanceDone chan struct{}
}

// Prompt admission and config-axis changes share this lock. TryLock keeps
// ACP admission non-blocking while closing the idle-check/use window in an
// in-place role switch.

// A queued pendingConfig blocks new turns so a prompt never runs on the
// outgoing config. The turn or maintenance that queued it applies it from
// its defer, so no new turn is needed to drain the queue.

// swapModeID records the mode reported to the client and returns the previous
// value, so callers can emit current_mode_update only on change.

// currentModeID returns the mode last reported to the client.

// currentCtrl returns the session's controller under mu. rebuildSession swaps
// ctrl while holding mu, so any read of the field outside mu races with a
// concurrent config rebuild; always go through this accessor unless mu is
// already held.

// releaseSessionLease drops the session's transcript lease, if any. Idempotent.

// retireSessionLease defers Release until the authority-guarded save that
// invoked a recovery callback can return. Releasing synchronously inside that
// callback would wait on the very save executing the callback and deadlock.

// sessionLeaseBindError maps a lease-acquisition failure to the protocol
// error the client sees: a held session names its holder with the shared CLI
// wording; anything else is an internal error.

// initialize advertises the agent's capability set: persisted load plus ACP v1
// list/resume/close/delete lifecycle helpers, prompts carrying inline resource
// text (embeddedContext) but not image/audio, and stdio / Streamable HTTP MCP
// (no legacy sse).

// sessionNew opens a session: it mints an id, builds the session's sink bound to
// that id, asks the Factory to assemble the controller, switches the controller
// to interactive approval (so tool gates surface as ApprovalRequest events the
// sink forwards), and registers it.

// Pin a transcript file keyed by session id when the controller has a session
// dir, so every turn auto-saves there, session/prompt can hand the path back,
// and session/load can find it again by id across process restarts. The
// session lease is taken with it (defensive: the id-keyed path is brand new)
// so no other runtime can bind the transcript while this session lives.

// Fold in the live controller's extension catalog so plugin/... models
// are discoverable from the very first session/new result.

// Session modes exposed over ACP describe how the agent advances the task.
// Tool approval and runtime profile are independent config options. The legacy
// default/auto ids remain accepted for clients that used the old mixed axis.
const (
	sessionModeNormal        = "normal"
	sessionModePlan          = "plan"
	sessionModeGoal          = "goal"
	sessionModeLegacyDefault = "default"
	sessionModeLegacyAuto    = "auto"
)

// sessionSetMode switches the session's operating mode and confirms it with a
// current_mode_update, per the ACP session-mode contract.

// emitModeDrift reports controller-side mode flips (plan mode auto-exits when
// a plan is approved, a config rebuild resets switches) as current_mode_update
// so the client's mode picker stays truthful.

// Hold stateChangeMu across the controller read and the session-state swap:
// a session/set_mode completing between them (it holds this lock) would
// otherwise be read back as drift, roll the session's modeID and metadata
// back to the pre-selection value, and make the next rebuild re-apply that
// stale mode to the replacement controller.

// Same contract as emitModeDrift: serialize with switchSessionToolApproval
// and rebuilds so a user selection landing between the controller read and
// the swap below is never reverted.

// sessionLoad resumes a previously-saved session by id: it builds a controller
// (rooted at the requested cwd), seeds it from the on-disk transcript, replays
// the conversation to the client as session/update notifications, and registers
// it for subsequent prompts. A session already live in this process is replayed
// from memory without rebuilding.

// sessionModesFor reports the modes state for a just-opened session. A live
// session keeps its current normal/plan/goal selection, so load/resume must not
// reset a reconnecting client's mode picker to normal.

// sessionResume restores a previously-saved session without replaying its
// conversation history to the client.

// Bind the transcript for writing only if no other runtime (a desktop
// window, the CLI) holds it; the editor should not silently double-write a
// session that is open elsewhere.

// transcriptPath is where a session's transcript lives — keyed by id so
// session/load can recover it. Distinct from the cli's timestamp-labelled
// chat/run session files (those are addressed by a picker, not by id).

// resolveTranscriptPath returns the transcript file session id currently
// lives in. That is the id-keyed path by default; after a snapshot recovery
// moved the live session onto a recovery branch, the id-keyed sidecar carries
// an ActiveTranscript redirect (written by sessionRecoveredHandler) that
// load/resume/delete/meta lookups must follow, or a restart silently reopens
// the pre-recovery transcript. The redirect is a basename, must stay inside
// dir, and its target must exist and claim the same session id; anything else
// falls back to the id-keyed path.

// sessionPrompt runs one turn. It flattens the prompt blocks to text and runs the
// session's controller synchronously under a per-turn cancelable context (so
// session/cancel can stop it), then reports why the turn ended. The controller
// streams the turn's events to the session's sink as it runs.

// Persist after status finalization (best-effort) so reconnect recovers both
// the transcript and the same sequence/usage/outcome snapshot.

// sessionSteer durably persists guidance then attempts mid-turn admission.
// Parameter/session errors remain RPC errors; busy rejection returns a
// disposition so clients can keep the durable follow-up.

// Durable path when the session has a transcript path; ephemeral
// test controllers without persistence fall back to TrySteer.

// Compatibility for older controller stubs / pathless sessions.

// sessionReloadExtensions rebuilds a session's agent runtime in place —
// tools, skills, commands, hooks, MCP servers, and providers are re-discovered
// — while the session (transcript, approval grants, goal and recovery state)
// carries over via boot.Rebuild. It follows the same contract as a config
// switch: a turn or rebuild in flight coalesces exactly one queued reload,
// drained when the session goes idle; a failure keeps the old controller fully
// usable; the old controller's resources are released only after the swap.

// A config switch or reload is in maintenance: coalesce one reload
// behind it; the maintenance owner's post-maintenance drain runs it
// (mirrors the pendingConfig queue contract in rebuildSession).

// reloadSessionExtensionsLocked is reloadSessionExtensions' body; callers hold
// stateChangeMu. The busy/queue checks and the publish/close ordering mirror
// rebuildSessionLocked, but the build itself goes through the factory's
// boot.Rebuild path instead of NewSession + manual migration.

// Busy: coalesce exactly one reload; finishTurn (or the maintenance
// owner's post-maintenance drain) runs it once the session is idle.

// Claim the queued reload and raise maintenance in the same critical
// section (mirrors rebuildSessionLocked): begin must never observe an
// idle session between the two.

// Read the path only after Snapshot: a conflict can retarget cur to a
// recovery branch, and boot.Rebuild binds the replacement to whatever
// cur reports now (see rebuildSessionLocked). SessionPath is
// controller-locked, so reading it off sess.mu is safe.

// The rebuilt controller must keep the client-capability wiring (fs
// overlay, host terminal) — mirrors rebuildSessionLocked.

// Config on disk may have changed the effective planner/sandbox posture;
// recompute the status snapshot from the same resolved inputs.

// Persist before publishing the replacement. If this fails, the outgoing
// controller and transcript still agree and remain fully usable (mirrors
// the config switch).

// Release the outgoing controller only after the swap published the
// replacement. ReleaseResources (not Close): the session logically
// continues, so SessionEnd hooks must not fire — mirrors the config
// switch.

// Clients see refreshed plugin commands without waiting for the next turn.

// drainPendingReload runs the coalesced reloadExtensions request once the
// session is idle. Called from finishTurn and after a config switch's or a
// reload's own maintenance completes; callers must NOT hold stateChangeMu
// (the reload re-acquires it).

// finishTurn reconciles controller-side drift and drains any config switch
// queued during the turn. Drift must be reconciled before finish() exposes
// the session as idle: a concurrent config switch races on sess.running, and
// if it wins that race while modeID/toolApprovalMode are still stale (a
// slash command or plan/goal completion changed them inside the turn), it
// rebuilds the replacement controller from the outgoing state instead of the
// one this turn actually ended in.

// A reloadExtensions request queued during the turn runs now that the
// session may be idle; the drain re-checks busy state.

// Re-check after a rebuild in case the replacement normalized state.

// sessionSetConfigOption applies ACP's generic session-level selectors for
// model, reasoning effort, work mode, and tool approval.

// sessionSetModel keeps older ACP clients working while configOptions becomes
// the preferred model selector.

// sessionConfigDelta names exactly one config axis a caller asked to change
// (tool approval never rebuilds the controller, so it has no delta here).
// rebuildSession queues these — instead of a fully resolved SessionConfigState
// — while a turn or rebuild is in flight, one queue entry per axis, and
// applyPendingSessionConfig re-resolves the queued set against the session's
// live baseline once the session is idle. That way a queued change to one axis
// can never restore a stale value on another axis that changed in the
// meantime, whether that axis rebuilt already or is queued alongside.

// mergePendingConfig queues delta with last-write-wins per axis: it replaces a
// queued entry for the same axis and appends otherwise, so a change queued for
// one axis can never drop a change queued for another. Callers hold sess.mu.

// removePendingAxes drops the queue entries whose axis a rebuild is applying,
// keeping entries other requests queued in the meantime so the post-maintenance
// drain still applies them. Callers hold sess.mu.

// resolveSessionConfigDeltas resolves deltas against the session's current
// config baseline. Calling this fresh at every apply — instead of reusing a
// snapshot taken when a change was first requested — is what keeps a queued
// delta for one axis from clobbering another axis that rebuilt in between.

// Role settings switch in place without rebuilding the controller when
// the session is idle. Busy sessions return an explicit error (no silent
// queue). TryLock so a concurrent model/effort rebuild cannot deadlock us.

// Dual-write session runtime profile label for config option responses.

// Keep status planner mode aligned without a controller rebuild.

// switchSessionConfig resolves and applies one explicit config request without
// letting its full config snapshot roll back another axis. Resolution must be
// repeated after stateChangeMu is acquired: a different-axis rebuild may finish
// while this request is resolving or waiting for the lock, making the earlier
// baseline stale even though this request's own delta is still current.

// Preserve the non-blocking queue contract while a rebuild is already in
// maintenance. Resolve once for validation and the immediate client update;
// the drain resolves the queued deltas again against live state.

// Always resolve inside the serialization domain. Even a successful TryLock
// can follow a concurrent rebuild that completed after this request began.

// A reloadExtensions request queued behind this maintenance runs next.

// The pending drain completes before this request returns. Refresh the RPC
// result so an older response cannot overwrite the newer config_option_update
// with the pre-drain full snapshot on the client.

// Preserve the existing queue contract: a config change arriving during
// a controller build returns immediately and is applied after that
// build. The queue keeps one delta per axis (last-write-wins within an
// axis), so changes queued for different axes never clobber each other.
// Collaboration/approval changes do not use this queue; they wait for
// the swap and then update the replacement controller.

// A reloadExtensions request queued behind this maintenance runs next.

// Claim this rebuild's axes from the queue in the same critical section
// that raises maintenanceDone below: begin must never observe an idle
// session between the two. Axes queued by other requests stay queued and
// are drained by the post-maintenance apply.

// Capture the adopt path and history only after Snapshot: a snapshot
// conflict can retarget cur to a recovery branch (or adopt the newer disk
// transcript), and a pre-snapshot capture would bind the rebuilt controller
// back to the original file, re-conflicting on every later save. When that
// recovery fired, sessionRecoveredHandler already moved sess.transcript
// and the session lease to the recovery file, so prevPath, the session
// bookkeeping, and the controller agree on one path here.
// SessionPath is controller-locked, so reading it off sess.mu is safe.

// The rebuilt controller must keep the client-capability wiring (fs
// overlay, host terminal) a model/effort switch would otherwise drop.

// The freshly built controller's own leading system message carries the
// target profile's contract (see boot/token_profile.go); AdoptHistory below
// replaces the whole history with carried, so splice that message in first
// or the model keeps seeing the outgoing profile's contract after every
// switch.

// Re-apply all three independent session axes. A controller rebuild must not
// turn Plan into tool approval, drop a running Goal, or reset Ask/Auto/Yolo.

// InheritLifecycleFrom wires two concrete controllers' turn/hook state; it's a
// construction concern, not part of the driving port. cur is always the
// *control.Controller the factory built for this session, so this is safe.

// A rebuild must not force the user to re-approve tools already granted
// for this session, or re-trust Plan-mode read-only commands already
// trusted this session.

// Persist before publishing the replacement. If this fails, the outgoing
// controller and transcript still agree and remain fully usable; publishing
// first would report a successful switch whose refreshed profile contract
// disappears on restart. AdoptHistory preserves the loaded CAS baseline, so
// this compatible leading-system rewrite is safe to snapshot here.

// Claim the queue in the same serialization domain as explicit config
// switches. Without this lock, a newer same-axis request can rebuild after
// the clone below but before this apply starts, then the stale cloned delta
// queues behind it and wins last instead of preserving request order.

// Keep pendingConfig set while rebuilding: begin refuses new turns until
// rebuildSession claims it together with raising maintenanceDone, so no
// promptable instant is visible in between.

// Re-resolve against the session's current state rather than reusing
// whatever baseline existed when each delta queued: another axis may have
// finished rebuilding in the meantime, and replaying its old value here
// would silently roll it back. All queued axes resolve into one state so a
// single rebuild applies them together.

// Once this attempt failed nothing in flight is left to retry the
// claimed axes, and begin refuses new turns while any are queued — drop
// them so the session stays promptable. Once maintenance started, those
// axes were already removed; anything queued now is a newer request and
// must survive this failure.

// Requests can queue while NewSession/Snapshot runs. Iterate even when this
// rebuild failed so their already-successful RPCs cannot leave the session
// blocked. A loop keeps sustained config traffic from growing the call stack.

// A queued request already announced its desired config to the client. Every
// apply failure leaves the outgoing controller/config active, so always send
// the live state back; otherwise snapshot/build/resolve failures leave the
// picker claiming a switch that never happened.

// sessionClose releases an active session. Unknown sessions are accepted as a
// no-op because closing is an idempotent resource cleanup request.

// sessionList returns ACP sessions known to this process or persisted as ACP
// sidecars. It deliberately ignores ordinary CLI timestamp sessions.

// A recovered session has two sidecars claiming the same id: the
// active recovery transcript's own meta and the id-keyed redirect.
// Reduce to one representative per id before filtering, so the entry
// shown never carries the stale pre-recovery title/timestamps.

// sessionDelete removes a session from future list results. Deleting a missing
// session succeeds silently, matching ACP's idempotent delete guidance.

// The session is going away; drop its lease before removing files so
// the lease sidecars retire with the release (they are not in
// SessionSidecarFiles and would otherwise linger).

// A recovered session lives in two files: the recovery transcript (deleted
// above) and the id-keyed original holding the redirect. Remove the twin
// too, or it resurfaces in session/list as a ghost that delete-by-id can
// never reach again.

// sessionCancel aborts a session's in-flight turn, if any. It is a notification:
// no reply, and an unknown session is silently ignored.

// Fold in the live controller's extension catalog so plugin/... models
// are discoverable on every config-state read, not only when current.

// closeAll tears down every open session (aborting any in-flight turn and
// stopping its MCP subprocesses) when the connection ends.

// Extension actions surface as "<plugin>:<action>" commands so ACP clients
// can discover them in the slash menu alongside commands/skills/prompts.

// invokeExtensionAction resolves a "/<plugin>:<action> args…" line against the
// handshake-declared extension actions and invokes it — the last resolution
// step in resolveSlashPrompt, after custom commands, skills, and MCP prompts.
// The extension's result message becomes the prompt text. A parse miss, an
// undeclared action, an invocation error, or an empty result all leave the
// line untouched (ok=false), matching how unknown slash commands fall through.

// ActiveTranscript, when set on the id-keyed sidecar, is the basename of
// the transcript this session currently lives in: a snapshot recovery
// moved the live session onto a recovery branch and left this redirect
// behind so restart-time lookups (resolveTranscriptPath) follow the
// session instead of reopening the pre-recovery file.

// listMetaBeats reports whether a should represent its session id in
// session/list over b. A meta without an ActiveTranscript redirect is the
// session's live transcript and always beats a redirect sidecar; between two
// of the same kind the later UpdatedAt wins.

// ReconcileCleanupPending retries delayed ACP session cleanup left by a previous
// process, including ACP's own metadata sidecar.

// mcpSpecs converts ACP MCP server declarations to plugin.Spec.

// newSessionID returns a random RFC 4122 v4 UUID string used to address a session.
