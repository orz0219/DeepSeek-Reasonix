package main

import (
	"context"
	"log/slog"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/i18n"
	"reasonix/internal/notify"
	"reasonix/internal/repair"
	"reasonix/internal/sessioncatalog"
	"reasonix/internal/taskmonitor"
)

// App is the Wails-bound application object: the desktop frontend's command
// surface. Its exported methods (Submit/Cancel/Approve/…) are generated into JS
// bindings. The app manages multiple WorkspaceTabs — each with its own controller
// scoped to a project workspace — and routes commands to the active tab. Events
// flow the other way: each tab's controller emits to a tabEventSink that
// forwards events tagged with tabId to the webview via runtime.EventsEmit.
type App struct {
	ctx          context.Context
	workspaceHub *workspaceChangeHub

	// sessionCatalog is a disposable, asynchronously opened projection of
	// authoritative session sidecars. Project-shell APIs must tolerate nil here:
	// opening, migration, repair, and corruption recovery never gate the UI.
	sessionCatalog     atomic.Pointer[sessioncatalog.Catalog]
	catalogLifecycleMu sync.Mutex
	catalogCancel      context.CancelFunc
	catalogDone        chan struct{}
	catalogRebuilding  atomic.Bool
	shuttingDown       atomic.Bool

	// taskCtrl is the process-wide task-monitor control service (lazy; see
	// taskControl). One instance serializes control operations in-process.
	taskCtrl     *taskmonitor.ControlService
	taskCtrlOnce sync.Once

	// mu protects the tab map, tabOrder, activeTabID, and per-tab fields that are read
	// from bound methods. All bound methods that touch a controller use activeCtrl().
	mu          sync.RWMutex
	tabs        map[string]*WorkspaceTab
	tabOrder    []string
	activeTabID string
	readyHook   func()

	// Ticketed topic activation bookkeeping (StartTopicActivation). Guarded by
	// mu. activationGen bumps on every activation-or-supersede so a background
	// completion can tell whether it still owns publication; the pending
	// request/tab pair identifies the in-flight ticketed activation whose
	// completion may still prune and emit "ready".
	activationGen             uint64
	latestActivationRequestID string
	pendingActivationTabID    string
	// activationEventHook is test-only: when set it replaces the
	// "topic:activation" runtime event emission so tests capture events
	// synchronously. Set before starting concurrent work, never mutate after.
	activationEventHook func(TopicActivationEvent)
	// tabBuildStartHook is test-only: called at the top of every tab
	// controller build (even already-superseded ones) so ordering tests can
	// gate builds. Same set-before-concurrency rule.
	tabBuildStartHook func(tabID string)
	// configLoadForRootHook is test-only: called from the background meta
	// extras refresh so tests can prove MetaForTab itself never loads config.
	configLoadForRootHook func(root string)

	// runtimeByID/runtimeBySessionKey form the process-local ownership registry.
	// App.mu guards both maps and every desktopSessionRuntime field.
	runtimeByID         map[string]*desktopSessionRuntime
	runtimeBySessionKey map[string]*desktopSessionRuntime

	// tabsRestored is closed when restoreOrBuildTabs has finished populating
	// a.tabs from desktop-tabs.json (or built the first-launch tab). Startup
	// work that inspects "which sessions are open" or persists the tab list
	// (recovery GC's DeleteSession does both) must wait on it: running against
	// the pre-restore empty tab map would treat every saved tab's session as
	// closed and could overwrite desktop-tabs.json with an empty snapshot.
	tabsRestored chan struct{}

	// projectTreeChangedHook is test-only: set once before any concurrency
	// starts, then read lock-free from emitProjectTreeChanged (whose callers
	// may or may not hold a.mu, so it cannot re-lock). Never write it after
	// startup.
	projectTreeChangedHook func()

	// singleSurfaceMu serializes open/reuse plus visible-tab pruning for the
	// one-conversation layout so overlapping navigation cannot remove the tab
	// another navigation is still activating.
	singleSurfaceMu sync.Mutex

	// sessionRemovalMu serializes operations that remove visible or detached
	// session bindings. Those operations may snapshot controllers before
	// deletion; keep that snapshot outside a.mu, but do not let DeleteSession or
	// topic/workspace removal trash the same files while it is in flight.
	sessionRemovalMu sync.Mutex

	// runtimeRebuildMu serializes controller rebuilds (build + swap), teardown,
	// and MCP lifecycle mutations. Two concurrent rebuilds of the same tab both
	// pass the tab-identity check at swap time, while MCP launch authorization racing
	// a toggle/reconnect can restore stale tools or launch a second single-instance
	// server. MCP paths insert extensionBuildMu between runtimeRebuildMu and
	// runtimeAdmissionMu; both orders end at App.mu -> Host/Registry.
	runtimeRebuildMu sync.Mutex
	// runtimeAdmissionMu is the runtime lifecycle barrier. Foreground turn-start
	// tokens and the short publication phase of asynchronous controller builds
	// hold the read side; runtime teardown and MCP lifecycle mutations hold the
	// write side so their captured controller/Host cannot be replaced, closed, or
	// handed a late turn in flight. Writers already hold runtimeRebuildMu, making
	// them mutually exclusive. Read holders must never acquire runtimeRebuildMu,
	// or a queued writer would deadlock the pair.
	runtimeAdmissionMu sync.RWMutex
	// runtimeMutationBeforeLockHook is test-only. Set it before starting concurrent
	// calls and never mutate it afterward.
	runtimeMutationBeforeLockHook func(string)
	// modelSwitchTimingHook is test-only. Production diagnostics use the same
	// sanitized timing record through debug logging.
	modelSwitchTimingHook func(modelSwitchTiming)
	// rebindCandidateHook is test-only. It exposes deterministic transaction
	// boundaries without weakening the production lock order. Set it before
	// starting a rebind and never mutate it until that rebind returns.
	rebindCandidateHook func(string) error
	// providerCatalogBeforeCredentialLockHook is test-only. It pauses catalog
	// compare-and-apply after its optimistic credential snapshot but before the
	// shared credential lock and authoritative re-read.
	providerCatalogBeforeCredentialLockHook func(string)

	// tryRunMu guards tryRunCancel — the cancel handle for the single
	// in-flight settings-page subagent try run (TrySubagentProfile /
	// CancelTrySubagentProfile).
	tryRunMu     sync.Mutex
	tryRunCancel context.CancelFunc

	// deferredRebuild tracks tabs whose settings were saved but whose runtime
	// could not refresh because the session lease was held by another process.
	deferredRebuild deferredRebuildState

	// historySliceMu guards the windowed-history background bookkeeping:
	// single-flight display-index rebuilds for live sessions and the startup
	// index-migration worker's cancel handle. Never held while calling
	// controller or session methods.
	historySliceMu              sync.Mutex
	historyIndexRebuilds        map[string]struct{}
	historyIndexMigrationCancel context.CancelFunc
	historyDerived              historyDerivedCache

	// detachedSessions keeps live session runtimes whose visible tab was closed.
	// It is process-local by design: shutdown closes every detached controller.
	detachedSessions map[string]*WorkspaceTab

	// sharedHosts holds one *plugin.Host per workspace root, shared by all
	// controllers/tabs in that root so MCP subprocesses (CodeGraph, etc.) are
	// spawned once instead of N times. Lifecycle: first Acquire creates the
	// host, last Release closes it.
	sharedHosts   map[string]*sharedPluginHost
	sharedHostsMu sync.Mutex
	// extensionGeneration fences off-lock shared-host boot against MCP mutations;
	// stale generations abandon publication instead of restoring old tools.
	extensionGeneration atomic.Uint64
	extensionBuildMu    sync.RWMutex

	// tabsSaveMu serializes writes to desktop-tabs.json and its fixed .tmp path.
	tabsSaveMu             sync.Mutex
	tabsSaveVersion        uint64 // protected by mu; assigned when collecting a snapshot
	tabsLastWrittenVersion uint64 // protected by tabsSaveMu

	forceQuit           atomic.Bool
	backgroundMaximised atomic.Bool
	desktopLocale       atomic.Int32
	trayReady           bool
	tray                *desktopTray
	hangWatchdogMu      sync.Mutex
	hangWatchdogCancel  context.CancelFunc

	mediaTokens *mediaTokenStore

	notificationSenderOnce sync.Once
	notificationSender     notify.Sender

	runtimeEvents asyncRuntimeEmitter

	// terminals owns local PTY/ConPTY sessions. It is intentionally separate
	// from chat runtimes: terminal lifecycle must never acquire App.mu or the
	// controller rebuild locks while process I/O is blocked.
	terminals *terminalManager

	// promptHistoryTape is a lazy, cursor-addressed view of prompt history. It
	// stores session order and per-session parsed entries only after that session is
	// reached by ↑ navigation. See ScanPromptHistory.
	promptHistoryMu   sync.Mutex
	promptHistoryTape *promptHistoryTape

	skillRootsMu    sync.Mutex
	skillRootsCache skillRootsCache

	lifecycle desktopLifecycleRuntime
	// diagnosticsOwner is acquired before Wails starts so Linux's OnStartup
	// ordering cannot let a second-instance handoff create lifecycle evidence.
	diagnosticsOwner        bool
	diagnosticsOwnerRelease func()
	diagnosticsConfigLoaded bool
	// startupReady records that the window reached domReady so LKG config
	// snapshots are only committed after a real UI boot.
	startupReady atomic.Bool
}

func (a *App) bootContext() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

// Platform exposes the native OS to the frontend so chrome/layout affordances can
// stay platform-scoped instead of relying on browser user-agent guesses.
func (a *App) Platform() string {
	return goruntime.GOOS
}

// Version returns the build version injected via -ldflags (see main.go). The
// desktop settings "About" page displays it; an un-injected dev build stays
// "dev". It used to live on the removed self-updater surface, but the About
// panel still needs it.
func (a *App) Version() string { return version }

// startup runs once the webview process is up, before the frontend can issue any
// bound call. It captures the Wails context (needed for EventsEmit), then kicks
// off the initialization in a background goroutine so the webview loads immediately.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.shuttingDown.Store(false)

	initializeLifecycleDiagnostics(a)
	a.startWindowsWebView2StartupFallback(ctx)
	a.lifecycle.tracker.markAsync("ready")
	installSystemQuitHook()
	a.startTray()
	a.enableDeferredRebuildRetry()
	a.startHistoryIndexMigration()
	a.goSafe("repairDesktopIconIntegration", func() {
		if err := repairDesktopIconIntegration(); err != nil {
			slog.Debug("desktop: repair native icon integration", "err", err)
		}
	})

	a.observeIncompleteWindowRestore()
	a.startMainThreadWatchdog()

	a.mu.Lock()
	a.tabsRestored = make(chan struct{})
	a.mu.Unlock()
	go a.restoreOrBuildTabs()
	a.registerHistoryIndexEvents()
	a.startSessionCatalog(false)

	a.startRecoveryGC()
}

func (a *App) beforeClose(ctx context.Context) bool {
	if a.forceQuit.Swap(false) || consumeSystemQuitRequested() {
		return false
	}
	cfg, _, err := a.loadDesktopUserConfigForView()
	if err != nil {
		cfg = config.LoadForEdit(config.UserConfigPath())
	}
	if cfg.DesktopCloseBehavior() == "background" {
		if !a.backgroundCloseHasRestorePath() {
			return false
		}

		a.backgroundMaximised.Store(a.lastKnownMaximised())
		a.saveWindowStateSync()
		a.snapshotAllTabs()
		hideForBackground(ctx)
		return true
	}
	return false
}

func (a *App) backgroundCloseHasRestorePath() bool {
	if backgroundCloseUsesApplicationHide(goruntime.GOOS) {
		return backgroundCloseHasRestorePathFor(goruntime.GOOS, false, false)
	}
	if !a.startTray() {
		return false
	}
	return backgroundCloseHasRestorePathFor(goruntime.GOOS, true, a.waitForTrayReady(backgroundCloseTrayReadyTimeout))
}

func (a *App) waitForTrayReady(timeout time.Duration) bool {
	if a.isTrayReady() {
		return true
	}
	ready := a.trayReadySignal()
	if ready == nil {
		return false
	}
	if timeout <= 0 {
		select {
		case <-ready:
			return a.isTrayReady()
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ready:
		return a.isTrayReady()
	case <-timer.C:
		return a.isTrayReady()
	}
}

func (a *App) isTrayReady() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.trayReady
}

func (a *App) trayReadySignal() <-chan struct{} {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.tray == nil {
		return nil
	}
	return a.tray.ready
}

// markTabsRestored closes the tabsRestored gate exactly once. Safe when the
// channel was never created (tests that drive App without startup).
func (a *App) markTabsRestored() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tabsRestored == nil {
		return
	}
	select {
	case <-a.tabsRestored:
	default:
		close(a.tabsRestored)
	}
}

// tabsRestoredSignal returns a channel closed once tab restore has completed.
// When startup never armed the gate (tests), it reports already-restored.
func (a *App) tabsRestoredSignal() <-chan struct{} {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.tabsRestored == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return a.tabsRestored
}

func (a *App) showMainWindow() {
	a.showMainWindowFrom("menu")
}

func (a *App) secondInstanceLaunch() {
	a.showMainWindowFrom("second_instance")
}

func (a *App) quitApp() {
	if a.ctx == nil {
		return
	}
	a.forceQuit.Store(true)
	runtime.Quit(a.ctx)
}

// restoreOrBuildTabs restores the tabs from the last session, or creates a
// default Global tab on first launch.
func (a *App) restoreOrBuildTabs() {
	defer a.recoverToPending("restoreOrBuildTabs")

	defer a.markTabsRestored()

	a.reapOrphanCodeGraph()
	ctx := a.ctx
	ensureWorkspace()

	_, _ = config.MigrateLegacyIfNeeded()
	if err := reconcileTopicArchiveMetadataPending(a.deleteTopic); err != nil {
		slog.Warn("desktop: topic archive metadata reconciliation remains pending")
	}
	f := loadTabsFile()
	_, _ = recoverLegacyProjectSidebarRoots(f)
	_, _ = config.ApplyUserConfigUpgradesOnStartup(config.UserConfigPath())
	_, _ = config.MigrateMCPToUserConfigOnUpgrade(desktopMCPMigrationRoots(f))

	startupCfg, cfgErr := config.Load()
	if cfgErr == nil {
		cfg := startupCfg
		lang := cfg.DesktopLanguage()
		if lang == "" {
			lang = cfg.Language
		}
		a.setDesktopLocale(i18n.DetectLanguage(lang))
	}
	if cfgErr != nil || singleSurfaceLayoutStyle(startupCfg.DesktopLayoutStyle()) {
		f = singleSurfaceTabsFile(f)
	}

	if len(f.Tabs) > 0 {
		toBuild := make([]*WorkspaceTab, 0, len(f.Tabs))
		for _, entry := range f.Tabs {
			a.mu.Lock()
			id := a.restoredTabIDLocked(entry.ID)
			a.mu.Unlock()

			var tab *WorkspaceTab
			if entry.Scope == "project" {
				tab = a.createTabEntryWithID(entry.Scope, entry.WorkspaceRoot, entry.TopicID, id)
			} else {
				tab = a.createTabEntryWithID("global", globalTabWorkspaceRoot(), entry.TopicID, id)
			}
			tab.model = entry.Model
			tab.effort = cloneStringPtr(entry.Effort)

			if strings.TrimSpace(entry.AgentPreset) != "" {
				tab.tokenMode = boot.TokenModeFromAgentPreset(entry.AgentPreset)
			} else {
				tab.tokenMode = boot.NormalizeTokenMode(entry.TokenMode)
			}
			tab.mode = persistedTabMode(entry.Mode)

			tab.goal = runningTabSessionGoal(strings.TrimSpace(entry.SessionPath), strings.TrimSpace(entry.Goal))
			tab.toolApprovalMode = normalizeToolApprovalMode(entry.ToolApprovalMode)
			if tab.toolApprovalMode == control.ToolApprovalAsk && tabModeHasAutoApproveTools(entry.Mode) {
				tab.toolApprovalMode = control.ToolApprovalYolo
			}
			tab.SessionPath = strings.TrimSpace(entry.SessionPath)
			tab.ReadOnly = entry.ReadOnly
			tab.sink = &tabEventSink{tabID: tab.ID, app: a, ctx: ctx}
			a.mu.Lock()
			a.tabs[tab.ID] = tab
			a.tabOrder = append(a.tabOrder, tab.ID)
			a.mu.Unlock()
			toBuild = append(toBuild, tab)
		}
		a.mu.Lock()
		if _, ok := a.tabs[f.ActiveTab]; ok {
			a.activeTabID = f.ActiveTab
		} else {
			ordered := a.orderedTabIDsLocked()
			if len(ordered) > 0 {
				a.activeTabID = ordered[0]
			}
		}
		a.saveTabsLocked()
		a.mu.Unlock()
		for _, tab := range toBuild {
			a.startTabControllerBuild(tab)
		}
		return
	}

	tab := a.createTabEntry("global", globalTabWorkspaceRoot(), "")
	tab.sink = &tabEventSink{tabID: tab.ID, app: a, ctx: ctx}
	tab.TopicTitle = "Global"
	a.mu.Lock()
	a.tabs[tab.ID] = tab
	a.tabOrder = append(a.tabOrder, tab.ID)
	a.activeTabID = tab.ID
	a.mu.Unlock()
	a.startTabControllerBuild(tab)
}

func (a *App) createTabEntry(scope, workspaceRoot, topicID string) *WorkspaceTab {
	return a.createTabEntryWithID(scope, workspaceRoot, topicID, newTabID())
}

func (a *App) createTabEntryWithID(scope, workspaceRoot, topicID, id string) *WorkspaceTab {
	model, toolApprovalMode := desktopNewSessionDefaults(scope, workspaceRoot)
	return &WorkspaceTab{
		ID:               id,
		Scope:            scope,
		WorkspaceRoot:    workspaceRoot,
		TopicID:          topicID,
		TopicTitle:       topicTitleForTab(scope, workspaceRoot, topicID),
		topicTitleSource: loadTopicTitleSource(topicTitleRoot(scope, workspaceRoot), topicID),
		model:            model,
		tokenMode:        boot.TokenModeFull,
		mode:             tabModeFromAxes(false, toolApprovalMode == control.ToolApprovalYolo),
		toolApprovalMode: toolApprovalMode,
		disabledMCP:      map[string]ServerView{},
	}
}

func (a *App) snapshotAllTabs() {
	a.mu.RLock()
	tabs := a.runtimeTabsLocked()
	a.mu.RUnlock()
	for _, t := range tabs {
		if err := a.snapshotTab(t); err != nil {
			slog.Warn("desktop: snapshot all tabs failed", "tab", t.ID, "err", err)
		}
	}
}

// shutdown snapshots all tabs, saves the final window geometry, and closes tabs.
func (a *App) shutdown(context.Context) {
	a.shuttingDown.Store(true)
	a.cancelAllTabBuilds()
	a.stopSessionCatalog(250 * time.Millisecond)
	completeDesktopShutdown(a.lifecycle.tracker, a.shutdownBody)
}

// domReady is called (via OnDomReady) after the webview finishes loading its DOM
// but before the window is shown (StartHidden). It restores the saved window
// position and size, then calls WindowShow so the user never sees the default
// size/position flash.
func (a *App) domReady(_ context.Context) {

	repairWebKitSignalHandlers()

	state, ok := loadWindowState()
	if ok {

		maxW, maxH := 0, 0
		screens, err := runtime.ScreenGetAll(a.ctx)
		if err == nil {
			for _, sc := range screens {
				if sc.Size.Width > maxW {
					maxW = sc.Size.Width
				}
				if sc.Size.Height > maxH {
					maxH = sc.Size.Height
				}
			}
		}
		if windowPositionRestorable(state, maxW, maxH) {
			runtime.WindowSetPosition(a.ctx, state.X, state.Y)
		} else {
			runtime.WindowCenter(a.ctx)
		}
	} else {
		runtime.WindowCenter(a.ctx)
	}

	if ok && state.Maximised {
		runtime.WindowMaximise(a.ctx)
	}

	runtime.WindowShow(a.ctx)
	a.markDesktopHealthy()
	ctx := a.ctx
	a.goSafe("recordHealthyConfig", func() {
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return
		}
		if err := repair.RecordHealthyConfig(version); err != nil {
			slog.Debug("desktop: record last-known-good config", "err", err)
		}
	})
}
