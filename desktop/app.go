package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/sessiontemp"
)

// sessionTempFromController returns the logical-session private temporary
// directory manager for a same-session controller rebuild. Nil when the
// controller is missing or is not a *control.Controller.
func sessionTempFromController(ctrl control.SessionAPI) *sessiontemp.Manager {
	c, ok := ctrl.(*control.Controller)
	if !ok || c == nil {
		return nil
	}
	return c.SessionTemp()
}

// eventChannel is the Wails runtime event name the frontend subscribes to for the
// agent's typed event stream. One channel carries every event kind; the payload's
// `kind` field discriminates — the desktop analogue of the serve transport's SSE
// `data:` frames.
const eventChannel = "agent:event"

const singleInstanceIDPrefix = "com.reasonix.desktop"

// singleInstanceID is used by Wails to route a second desktop launch back to the
// process that owns the same Reasonix data home. Basing the identity on the
// executable path let installed, portable, stable, and canary binaries write the
// same sessions concurrently. Explicit REASONIX_HOME isolation still produces
// an independent instance; REASONIX_DEV continues to bypass the lock entirely.
func singleInstanceID() string {
	root := strings.TrimSpace(config.ReasonixHomeDir())
	if root == "" {
		return singleInstanceIDPrefix
	}
	// Reuse the lease path canonicalizer so a missing home below a symlink or
	// junction still hashes to the same physical data directory.
	if marker := agent.CanonicalSessionPath(filepath.Join(root, ".reasonix-home.identity")); marker != "" {
		root = filepath.Dir(marker)
	}
	root = filepath.Clean(root)
	sum := sha256.Sum256([]byte(root))
	return singleInstanceIDPrefix + "." + hex.EncodeToString(sum[:8])
}

// PromptHistoryEntry is one user prompt extracted from a session JSONL file.
// The frontend uses these for ↑/↓ prompt-history navigation.

// unix ms

// PromptHistoryResult is returned as one Wails value. It carries one loaded tape
// segment plus the cursor needed to keep walking toward older prompts.

// App is the Wails-bound application object: the desktop frontend's command
// surface. Its exported methods (Submit/Cancel/Approve/…) are generated into JS
// bindings. The app manages multiple WorkspaceTabs — each with its own controller
// scoped to a project workspace — and routes commands to the active tab. Events
// flow the other way: each tab's controller emits to a tabEventSink that
// forwards events tagged with tabId to the webview via runtime.EventsEmit.

// sessionCatalog is a disposable, asynchronously opened projection of
// authoritative session sidecars. Project-shell APIs must tolerate nil here:
// opening, migration, repair, and corruption recovery never gate the UI.

// taskCtrl is the process-wide task-monitor control service (lazy; see
// taskControl). One instance serializes control operations in-process.

// mu protects the tab map, tabOrder, activeTabID, and per-tab fields that are read
// from bound methods. All bound methods that touch a controller use activeCtrl().

// Ticketed topic activation bookkeeping (StartTopicActivation). Guarded by
// mu. activationGen bumps on every activation-or-supersede so a background
// completion can tell whether it still owns publication; the pending
// request/tab pair identifies the in-flight ticketed activation whose
// completion may still prune and emit "ready".

// activationEventHook is test-only: when set it replaces the
// "topic:activation" runtime event emission so tests capture events
// synchronously. Set before starting concurrent work, never mutate after.

// tabBuildStartHook is test-only: called at the top of every tab
// controller build (even already-superseded ones) so ordering tests can
// gate builds. Same set-before-concurrency rule.

// configLoadForRootHook is test-only: called from the background meta
// extras refresh so tests can prove MetaForTab itself never loads config.

// runtimeByID/runtimeBySessionKey form the process-local ownership registry.
// App.mu guards both maps and every desktopSessionRuntime field.

// tabsRestored is closed when restoreOrBuildTabs has finished populating
// a.tabs from desktop-tabs.json (or built the first-launch tab). Startup
// work that inspects "which sessions are open" or persists the tab list
// (recovery GC's DeleteSession does both) must wait on it: running against
// the pre-restore empty tab map would treat every saved tab's session as
// closed and could overwrite desktop-tabs.json with an empty snapshot.

// projectTreeChangedHook is test-only: set once before any concurrency
// starts, then read lock-free from emitProjectTreeChanged (whose callers
// may or may not hold a.mu, so it cannot re-lock). Never write it after
// startup.

// singleSurfaceMu serializes open/reuse plus visible-tab pruning for the
// one-conversation layout so overlapping navigation cannot remove the tab
// another navigation is still activating.

// sessionRemovalMu serializes operations that remove visible or detached
// session bindings. Those operations may snapshot controllers before
// deletion; keep that snapshot outside a.mu, but do not let DeleteSession or
// topic/workspace removal trash the same files while it is in flight.

// runtimeRebuildMu serializes controller rebuilds (build + swap), teardown,
// and MCP lifecycle mutations. Two concurrent rebuilds of the same tab both
// pass the tab-identity check at swap time, while MCP launch authorization racing
// a toggle/reconnect can restore stale tools or launch a second single-instance
// server. MCP paths insert extensionBuildMu between runtimeRebuildMu and
// runtimeAdmissionMu; both orders end at App.mu -> Host/Registry.

// runtimeAdmissionMu is the runtime lifecycle barrier. Foreground turn-start
// tokens and the short publication phase of asynchronous controller builds
// hold the read side; runtime teardown and MCP lifecycle mutations hold the
// write side so their captured controller/Host cannot be replaced, closed, or
// handed a late turn in flight. Writers already hold runtimeRebuildMu, making
// them mutually exclusive. Read holders must never acquire runtimeRebuildMu,
// or a queued writer would deadlock the pair.

// runtimeMutationBeforeLockHook is test-only. Set it before starting concurrent
// calls and never mutate it afterward.

// modelSwitchTimingHook is test-only. Production diagnostics use the same
// sanitized timing record through debug logging.

// rebindCandidateHook is test-only. It exposes deterministic transaction
// boundaries without weakening the production lock order. Set it before
// starting a rebind and never mutate it until that rebind returns.

// providerCatalogBeforeCredentialLockHook is test-only. It pauses catalog
// compare-and-apply after its optimistic credential snapshot but before the
// shared credential lock and authoritative re-read.

// tryRunMu guards tryRunCancel — the cancel handle for the single
// in-flight settings-page subagent try run (TrySubagentProfile /
// CancelTrySubagentProfile).

// updaterOperationMu guards the single native download/install operation.
// Checks are read-only and may overlap; cache mutation and installation fail
// fast when another updater operation is already active.

// deferredRebuild tracks tabs whose settings were saved but whose runtime
// could not refresh because the session lease was held by another process.

// historySliceMu guards the windowed-history background bookkeeping:
// single-flight display-index rebuilds for live sessions and the startup
// index-migration worker's cancel handle. Never held while calling
// controller or session methods.

// detachedSessions keeps live session runtimes whose visible tab was closed.
// It is process-local by design: shutdown closes every detached controller.

// sharedHosts holds one *plugin.Host per workspace root, shared by all
// controllers/tabs in that root so MCP subprocesses (CodeGraph, etc.) are
// spawned once instead of N times. Lifecycle: first Acquire creates the
// host, last Release closes it.

// extensionGeneration fences off-lock shared-host boot against MCP mutations;
// stale generations abandon publication instead of restoring old tools.

// tabsSaveMu serializes writes to desktop-tabs.json and its fixed .tmp path.

// protected by mu; assigned when collecting a snapshot
// protected by tabsSaveMu

// botBridge gives the embedded bot gateway a god view over desktop
// sessions (/desktop commands). Set once in NewApp before any tab exists,
// read-only afterwards, so tabEventSink.Emit reads it without a lock.

// non-nil only when desktop.metrics is opted in; swapped live by SetDesktopMetrics

// terminals owns local PTY/ConPTY sessions. It is intentionally separate
// from chat runtimes: terminal lifecycle must never acquire App.mu or the
// controller rebuild locks while process I/O is blocked.

// Remote SSH module: the manager is created lazily on the first remote
// binding call and closed on shutdown.

// Remote web windows (SSH Serve child processes). The main process tracks
// the live child plus transient handoff processes for each host. Host-scoped
// lifecycle operations are generation-fenced and serialized so an overlapping
// disconnect/stop cannot miss a window that is still being spawned. Closing a
// window releases only its registration, while the remote Serve and the SSH
// connection keep running. The child deliberately skips local runtimes.

// test-only injection
// remoteWindowTicket/remoteWindowHostKey are set from argv before Wails
// starts in a child process. They gate the blank-shell middleware and the
// startup branches so the child never initializes local runtimes.

// remoteWindowOwnerID scopes child single-instance locks to one primary
// Desktop process. remoteWindowParentPID is set only in children and lets
// them exit when that owner (and therefore its SSH tunnel) disappears.

// remoteWindowMu serializes ticket consumption and navigation in a child
// process so a handoff arriving before domReady cannot be overridden by the
// initial ticket (or vice versa). remoteWindowTicketConsumed makes the
// initial handoff idempotent because WebKit fires OnDomReady again after the
// shell navigates to the remote Serve page.

// promptHistoryTape is a lazy, cursor-addressed view of prompt history. It
// stores session order and per-session parsed entries only after that session is
// reached by ↑ navigation. See ScanPromptHistory.

// scheduled heartbeat tasks; nil until startup

// diagnosticsOwner is acquired before Wails starts so Linux's OnStartup
// ordering cannot let a second-instance handoff create lifecycle evidence.

// Healthy-update identity is captured before Wails starts. A process may
// commit only the complete probationary transaction it actually booted from,
// never a rewritten or later same-version retry.

// startupReady records that the window reached domReady so LKG config
// snapshots and update health are only committed after a real UI boot.

// mediaTokenEntry holds metadata for a workspace media file served via temporary URL.

// mediaTokenStore manages temporary tokens that grant access to workspace files
// through the AssetServer middleware. Tokens expire after a fixed TTL and are
// capped at a maximum count; creating a new token evicts the oldest entry when
// the store is full.

// oldest first

// Trim oldest if the new token pushed us over the limit.

// jsProfilingMiddleware opts every asset response into the JS Self-Profiling
// document policy so the frontend performance monitor can attach sampled stacks
// to long-task reports. Chromium WebViews (WebView2) honor it; WebKit ignores
// both the header and the API, so the frontend degrades to unattributed reports.

// workspaceMediaMiddleware returns an HTTP middleware that intercepts
// /__reasonix_workspace_media/{token}/{filename} requests and serves the
// corresponding workspace file. All other paths pass through to the Wails
// default asset handler unchanged.

// NewApp constructs the bound object. Tabs are restored in startup from the
// last session's desktop-tabs.json.
func NewApp() *App {
	a := &App{
		tabs:                map[string]*WorkspaceTab{},
		runtimeByID:         map[string]*desktopSessionRuntime{},
		runtimeBySessionKey: map[string]*desktopSessionRuntime{},
		detachedSessions:    map[string]*WorkspaceTab{},
		mediaTokens:         newMediaTokenStore(),
		botInstalls:         map[string]*botInstallSession{},
		botRuntime:          newDesktopBotRuntime(),
		remoteWindows:       newRemoteWindowRegistry(),
		remoteWindowOwnerID: newRemoteWindowOwnerID(),
	}
	a.workspaceHub = newWorkspaceChangeHub(a)
	a.terminals = newTerminalManager(a)
	a.botBridge = a.newBotBridge()
	return a
}

// Platform exposes the native OS to the frontend so chrome/layout affordances can
// stay platform-scoped instead of relying on browser user-agent guesses.

// startup runs once the webview process is up, before the frontend can issue any
// bound call. It captures the Wails context (needed for EventsEmit), then kicks
// off the initialization in a background goroutine so the webview loads immediately.

// Only the process that claimed the pre-Wails diagnostics lock consumes
// lifecycle evidence. This remains correct on Linux where Wails invokes
// OnStartup before its DBus single-instance handoff.

// Remote web window child: no local tabs, tray, heartbeat, providers,
// or remote manager. domReady consumes the ticket and navigates; the
// owner watcher closes the window if the primary Desktop disappears.

// After restoreOrBuildTabs is launched: the GC's first sweep waits on
// tabsRestored so it never observes the pre-restore empty tab map.

// A remote web window closes immediately — nothing to snapshot, lease,
// or hide. Closing it must not stop the remote Serve or the main
// process's SSH connection.

// Never query native maximise state here: during close the Win32 DPI
// path can report 0 and panic inside Wails ScaleToDefaultDPI. Use the
// last frontend-reported geometry instead.

const backgroundCloseTrayReadyTimeout = 500 * time.Millisecond

// markTabsRestored closes the tabsRestored gate exactly once. Safe when the
// channel was never created (tests that drive App without startup).

// tabsRestoredSignal returns a channel closed once tab restore has completed.
// When startup never armed the gate (tests), it reports already-restored.

func hideForBackground(ctx context.Context) {
	if backgroundCloseUsesApplicationHide(goruntime.GOOS) {
		runtime.Hide(ctx)
		return
	}
	runtime.WindowHide(ctx)
}

func showFromBackground(ctx context.Context, wasMaximised bool) {
	if backgroundCloseUsesApplicationHide(goruntime.GOOS) {
		runtime.Show(ctx)
	}
	plan := backgroundRestorePlanFor(goruntime.GOOS, wasMaximised)
	if plan.maximiseBeforeShow {
		runtime.WindowMaximise(ctx)
	}
	runtime.WindowShow(ctx)
	if plan.unminimiseAfterShow {
		runtime.WindowUnminimise(ctx)
	}
}

func backgroundCloseUsesApplicationHide(goos string) bool {
	return goos == "darwin"
}

func backgroundCloseHasRestorePathFor(goos string, trayStarted, trayReady bool) bool {
	return backgroundCloseUsesApplicationHide(goos) || (trayStarted && trayReady)
}

type backgroundRestorePlan struct {
	maximiseBeforeShow  bool
	unminimiseAfterShow bool
}

func backgroundRestorePlanFor(goos string, wasMaximised bool) backgroundRestorePlan {
	if backgroundRestoreShouldMaximise(goos, wasMaximised) {
		return backgroundRestorePlan{maximiseBeforeShow: true}
	}
	return backgroundRestorePlan{unminimiseAfterShow: true}
}

func backgroundRestoreShouldMaximise(goos string, wasMaximised bool) bool {
	return wasMaximised && !backgroundCloseUsesApplicationHide(goos)
}

// restoreOrBuildTabs restores the tabs from the last session, or creates a
// default Global tab on first launch.

// Unblock startup work gated on the restore (recovery GC) no matter how
// this returns — including the recover path above.

// Reap any orphaned codegraph processes from a previous crash or older
// version that leaked them, so they don't accumulate across restarts.

// Run legacy config migration before the first config load so the
// freshly written config (including the user's default_model) is
// picked up by Load instead of falling back to built-in defaults.

// Load i18n from the first available config.
// Prefer DesktopLanguage (desktop UI setting) over Language (CLI setting),
// so the user's language choice in desktop settings takes effect.

// Prefer agentPreset; fall back to legacy tokenMode for one version.

// Validate the persisted goal against the session's goal-state
// sidecar: a typed /new or /clear rotates the session through the
// controller without passing App.NewSession/ClearSession, so
// entry.Goal can be stale. Session rotation writes a stopped
// goal-state onto the fresh path; reading it here stops a restart
// from re-seeding the cleared goal into the rotated session. A
// session without a sidecar keeps the persisted goal (legacy).

// First launch: create a default Global tab.

func desktopNewSessionDefaults(scope, workspaceRoot string) (string, string) {
	userCfg := config.LoadForEdit(config.UserConfigPath())
	modelCfg := userCfg
	if strings.TrimSpace(scope) == "project" && strings.TrimSpace(workspaceRoot) != "" {
		if cfg, err := config.LoadForRootReadOnly(workspaceRoot); err == nil {
			modelCfg = cfg
		}
	}
	return resolveNewSessionModel(modelCfg), normalizeToolApprovalMode(userCfg.DesktopDefaultToolApprovalMode())
}

// resolveNewSessionModel picks the model a fresh session starts on. A
// default_model that resolves but has no API key in the current environment
// would boot every new tab straight into the missing-key notice, so fall
// through to the first provider that is actually configured, mirroring the
// Configured() gate in Config.ResolveModelWithFallback's fallback chain. An
// allowed chat default is preserved when every eligible provider is keyless so
// the existing missing-key notice still tells the user what to fix. When no
// desktop-accessible chat model exists, the empty result lets tab startup show
// an actionable setup error instead of re-admitting an ineligible default.
func resolveNewSessionModel(cfg *config.Config) string {
	def := strings.TrimSpace(cfg.DefaultModel)
	config.NormalizeLegacyMimoCustomProvidersForRefs(cfg, def)
	if resolved, _, ok := cfg.ResolveDesktopNewSessionModel(); ok {
		// Keep provider identity explicit at the new-session boundary. A bare
		// model id is ambiguous when two configured gateways expose the same
		// model, and a provider-only ref otherwise compares unequal to the
		// canonical ref stored on a running tab.
		if entry, found := cfg.ResolveModel(resolved); found {
			return entry.Name + "/" + entry.Model
		}
		return resolved
	}
	return ""
}

// shutdown snapshots all tabs, saves the final window geometry, and closes tabs.

// Remote web window child has no local state to stop.

// Freeze publication, then cancel off-barrier history, catalog, and plugin
// work so normal quit never waits for background I/O.

// domReady is called (via OnDomReady) after the webview finishes loading its DOM
// but before the window is shown (StartHidden). It restores the saved window
// position and size, then calls WindowShow so the user never sees the default
// size/position flash.

// JSC has installed its lazy signal handlers by this point. Restore the
// SA_ONSTACK flags required by Go; this is a no-op outside Linux.

// Validate saved position against current screens. Wails v2 doesn't
// expose per-screen origin (x,y offsets) so we can only do a basic
// sanity check. Windows border insets (commonly x=-8,y=-8) are legal;
// large off-screen positions (unplugged external display) re-center.
