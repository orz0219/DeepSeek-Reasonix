package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/extension/providerext"
	"reasonix/internal/provider"
)

// Models flattens the configured providers into their (provider, model) pairs —
// the switcher's options — marking the active one. A vendor with a `models` list
// yields one entry per model, all sharing the same endpoint/key. Unconfigured
// providers are skipped. Result is non-nil: the frontend reads .length, so a nil
// slice (JSON null) would crash the switcher on an empty list.
func (a *App) Models() []ModelInfo {
	return a.ModelsForTab("")
}

func (a *App) ModelsForTab(tabID string) []ModelInfo {
	a.mu.RLock()
	curModel := ""
	workspaceRoot := ""
	var ctrl control.SessionAPI
	if tab := a.tabByIDLocked(tabID); tab != nil {
		curModel = tab.model
		workspaceRoot = tab.WorkspaceRoot
		ctrl = tab.Ctrl
	}
	a.mu.RUnlock()
	// The tab controller's merged catalog carries extension sidecar providers
	// (plugin/... refs). Read it off-lock: a cold catalog fetch can block on a
	// sidecar RPC and must never park a.mu.
	var extensionCatalog []provider.Descriptor
	if ctrl != nil {
		extensionCatalog = ctrl.ProviderCatalog()
	}
	cfg, err := config.LoadForRoot(workspaceRoot)
	if err != nil {
		return []ModelInfo{}
	}
	if entry, ok := cfg.ResolveModel(curModel); ok {
		curModel = entry.Name + "/" + entry.Model
	}
	out := []ModelInfo{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !modelProviderAccessAllowed(cfg.Desktop.ProviderAccess, p.Name) || !p.Configured() {
			continue
		}
		for _, m := range p.ChatModelList() {
			ref := p.Name + "/" + m
			out = append(out, ModelInfo{Ref: ref, Provider: p.Name, Model: m, Current: ref == curModel})
		}
	}
	return mergeExtensionModelInfos(out, extensionCatalog, curModel)
}

// mergeExtensionModelInfos adds namespaced plugin models from the controller's
// merged provider catalog. Base descriptors are already represented by out;
// plugin refs need no provider-access gate because enabling the package grants
// access. A nil catalog leaves the config-backed list untouched.
func mergeExtensionModelInfos(out []ModelInfo, catalog []provider.Descriptor, curModel string) []ModelInfo {
	if len(catalog) == 0 {
		return out
	}
	seen := make(map[string]bool, len(out)+len(catalog))
	for _, info := range out {
		seen[info.Ref] = true
	}
	for _, d := range catalog {
		ref := strings.TrimSpace(d.Ref)
		owner := providerext.PluginRefOwner(ref)
		if ref == "" || owner == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		providerName := "plugin/" + owner
		model := strings.TrimPrefix(ref, providerName+"/")
		out = append(out, ModelInfo{Ref: ref, Provider: providerName, Model: model, Current: ref == curModel})
	}
	return out
}

// extensionModelDescriptor finds a plugin-namespaced ref in a controller's
// merged catalog: an exact match, or the prefix form where ref names the
// provider and the descriptor adds the model segment. Non-plugin refs never
// match — they belong to the config catalog.
func extensionModelDescriptor(catalog []provider.Descriptor, ref string) (provider.Descriptor, bool) {
	ref = strings.TrimSpace(ref)
	if providerext.PluginRefOwner(ref) == "" {
		return provider.Descriptor{}, false
	}
	for _, d := range catalog {
		if d.Ref == ref || strings.HasPrefix(d.Ref, ref+"/") {
			return d, true
		}
	}
	return provider.Descriptor{}, false
}

func modelProviderAccessAllowed(access []string, name string) bool {
	if access == nil {
		return true
	}
	name = strings.TrimSpace(name)
	for _, candidate := range access {
		if strings.TrimSpace(candidate) == name {
			return true
		}
	}
	return false
}

// providerCatalogForTab returns the tab controller's merged provider catalog
// (extension sidecar providers over the config base), or nil when the tab has
// no live controller or no sidecar declared providers.
func (a *App) providerCatalogForTab(tab *WorkspaceTab) []provider.Descriptor {
	if tab == nil {
		return nil
	}
	if ctrl := a.controllerForTab(tab); ctrl != nil {
		return ctrl.ProviderCatalog()
	}
	return nil
}

type activeRuntimeWork struct {
	running        bool
	pendingPrompt  bool
	backgroundJobs int
}

func controllerActiveRuntimeWork(ctrl control.SessionAPI) activeRuntimeWork {
	if ctrl == nil {
		return activeRuntimeWork{}
	}
	status := ctrl.RuntimeStatus()
	return activeRuntimeWork{
		running:        status.Running,
		pendingPrompt:  status.PendingPrompt,
		backgroundJobs: status.BackgroundJobs,
	}
}

func (w activeRuntimeWork) active() bool {
	return w.running || w.pendingPrompt || w.backgroundJobs > 0
}

func controllerHasActiveRuntimeWork(ctrl control.SessionAPI) bool {
	return controllerActiveRuntimeWork(ctrl).active()
}

// rebuildBusyError reports a rebuild rejected because the controller still has
// a running turn, pending prompt, or background jobs. Typed so the
// deferred-rebuild retry loop can keep waiting instead of giving up.
type rebuildBusyError struct {
	setting string
	work    activeRuntimeWork
}

func (e *rebuildBusyError) Error() string {
	return fmt.Sprintf(
		"active work is still running; running=%t; pending_prompt=%t; background_jobs=%d; finish or cancel the current turn, answer pending prompts, and stop background jobs before changing %s",
		e.work.running,
		e.work.pendingPrompt,
		e.work.backgroundJobs,
		e.setting,
	)
}

func rebuildControllerActiveWorkErrorFor(ctrl control.SessionAPI, setting string) error {
	work := controllerActiveRuntimeWork(ctrl)
	if !work.active() {
		return nil
	}
	return &rebuildBusyError{setting: setting, work: work}
}

type sessionLeaseBusyError struct {
	setting string
	err     error
}

func (e *sessionLeaseBusyError) Error() string {

	setting := strings.TrimSpace(e.setting)
	if setting == "" {
		return "this session is already open in another Reasonix window or still running in the background; close the other window or open a copy"
	}
	return fmt.Sprintf("this session is already open in another Reasonix window or still running in the background; close the other window or open a copy before changing %s", setting)
}

func (e *sessionLeaseBusyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func userFacingSessionLeaseError(setting string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, agent.ErrSessionLeaseHeld) {
		return &sessionLeaseBusyError{setting: setting, err: err}
	}
	return err
}

// sessionPathAfterSnapshot returns where a controller rebuild should keep
// persisting after the old controller was snapshotted. Snapshotting is not
// path-neutral: a snapshot conflict can recover by retargeting the controller
// (and the tab's session lease, via handleTabSessionRecovered) to a recovery
// branch, so a prevPath captured before the snapshot may be stale. Reusing the
// stale path would bind the rebuilt controller — carrying the just-recovered
// transcript — back to the original file, turning every later save into a new
// conflict that derives yet another recovery branch. Falls back to fallback
// when the controller is gone or persistence is disabled (empty SessionPath).
func sessionPathAfterSnapshot(ctrl control.SessionAPI, fallback string) string {
	if ctrl == nil {
		return fallback
	}
	if path := strings.TrimSpace(ctrl.SessionPath()); path != "" {
		return path
	}
	return fallback
}

// withSessionLeaseContentionRetry retries acquire while it fails with
// agent.ErrSessionLeaseHeld, absorbing sub-second contention windows created
// by transient in-process lease probes. Any other error is returned
// immediately, and a lease that remains held after the bounded retries is
// reported as-is.
func withSessionLeaseContentionRetry[T any](acquire func() (T, error)) (T, error) {
	var zero T
	for attempt := 0; ; attempt++ {
		got, err := acquire()
		if err == nil {
			return got, nil
		}
		if !errors.Is(err, agent.ErrSessionLeaseHeld) || attempt >= sessionLeaseContentionRetryAttempts {
			return zero, err
		}
		time.Sleep(sessionLeaseContentionRetryInterval)
	}
}

func (a *App) ensureTabSessionLeaseForRebuild(tab *WorkspaceTab, path, setting string) error {
	transition, reserveErr := a.reserveSessionRuntimePath(tab, path)
	if reserveErr != nil {
		return userFacingSessionLeaseError(setting, reserveErr)
	}
	if _, err := withSessionLeaseContentionRetry(func() (struct{}, error) {
		if err := tab.ensureSessionLease(path); err != nil {
			if a.canReclaimCurrentProcessSessionLease(tab, path, err) {
				if lease, reclaimErr := agent.TryReclaimCurrentProcessSessionLease(path); reclaimErr == nil {
					tab.adoptSessionLease(lease)
					return struct{}{}, nil
				} else {
					err = reclaimErr
				}
			}
			return struct{}{}, err
		}
		return struct{}{}, nil
	}); err != nil {
		a.rollbackSessionRuntimePath(transition)
		return userFacingSessionLeaseError(setting, err)
	}
	a.commitSessionRuntimePath(transition)
	return nil
}

func (a *App) canReclaimCurrentProcessSessionLease(tab *WorkspaceTab, path string, err error) bool {
	key := sessionRuntimeKey(path)
	if tab == nil || key == "" || !errors.Is(err, agent.ErrSessionLeaseHeld) {
		return false
	}
	var leaseErr *agent.SessionLeaseError
	if !errors.As(err, &leaseErr) || leaseErr == nil {
		return false
	}

	if leaseErr.Info != nil &&
		(leaseErr.Info.PID != os.Getpid() || leaseErr.Info.WriterID != agent.SessionWriterID()) {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, candidate := range a.runtimeTabsLocked() {
		if candidate == nil || candidate == tab {
			continue
		}
		if candidate.sessionLeaseRuntimeKey() == key {
			return false
		}
		if candidate.Ctrl != nil && sessionRuntimeKey(candidate.currentSessionPath()) == key {
			return false
		}
	}

	if detached := a.detachedSessions[key]; detached != nil && detached.Ctrl != nil {
		return false
	}
	return true
}

// SetModel switches the active model and carries the current conversation into the
// new model's session, so the chat continues seamlessly and subsequent turns use
// the new model. No-op if name is already active or the controller is down.
func (a *App) SetModel(name string) error {
	return a.SetModelForTab("", name)
}

// persistTabModelIfCurrent repairs stale model metadata without letting an
// older default overwrite a newer explicit model switch. Model switches use
// the same runtimeRebuildMu, so whichever operation acquires it last owns the
// persisted provider identity.
func (a *App) persistTabModelIfCurrent(tab *WorkspaceTab, model string) error {
	model = strings.TrimSpace(model)
	if tab == nil || model == "" {
		return nil
	}
	a.runtimeRebuildMu.Lock()
	defer a.runtimeRebuildMu.Unlock()

	a.mu.RLock()
	if tab.removed || a.tabs[tab.ID] != tab {
		a.mu.RUnlock()
		return fmt.Errorf("tab %q changed while persisting model; retry", tab.ID)
	}
	if tab.Ctrl == nil || strings.TrimSpace(tab.model) != model {
		a.mu.RUnlock()
		return nil
	}
	a.mu.RUnlock()

	path := a.currentSessionPathFor(tab)
	if path == "" {
		return nil
	}
	if err := agent.SetBranchModelPreserveUpdated(path, model); err != nil {
		return fmt.Errorf("persist selected model: %w", err)
	}
	return nil
}

type modelSwitchTiming struct {
	Total          time.Duration
	LockWait       time.Duration
	Prepare        time.Duration
	Config         time.Duration
	Snapshot       time.Duration
	Build          time.Duration
	LeaseAndResume time.Duration
	SwapAndPersist time.Duration
	Outcome        string
}

func (a *App) SetModelForTab(tabID, name string) (retErr error) {
	if a.ctx == nil || name == "" {
		return nil
	}
	tab := a.tabByID(tabID)
	if tab == nil {
		return nil
	}
	a.mu.RLock()
	currentModel := tab.model
	a.mu.RUnlock()
	if name == currentModel {
		return nil
	}
	timing := modelSwitchTiming{}
	totalStarted := time.Now()
	defer func() {
		timing.Total = time.Since(totalStarted)
		if retErr != nil {
			timing.Outcome = "failed"
		} else {
			timing.Outcome = "ok"
		}
		slog.Debug(
			"desktop: model switch timing",
			"tab", tab.ID,
			"outcome", timing.Outcome,
			"total_ms", timing.Total.Milliseconds(),
			"lock_wait_ms", timing.LockWait.Milliseconds(),
			"prepare_ms", timing.Prepare.Milliseconds(),
			"config_ms", timing.Config.Milliseconds(),
			"snapshot_ms", timing.Snapshot.Milliseconds(),
			"build_ms", timing.Build.Milliseconds(),
			"lease_resume_ms", timing.LeaseAndResume.Milliseconds(),
			"swap_persist_ms", timing.SwapAndPersist.Milliseconds(),
		)
		if a.modelSwitchTimingHook != nil {
			a.modelSwitchTimingHook(timing)
		}
	}()

	stageStarted := time.Now()
	a.runtimeRebuildMu.Lock()
	timing.LockWait = time.Since(stageStarted)
	defer a.runtimeRebuildMu.Unlock()
	stageStarted = time.Now()
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	prevPath := a.reconciledSessionPathForTab(tab)
	if prevPath == "" {
		prevPath = a.currentSessionPathFor(tab)
	}
	if a.controllerForTab(tab) == nil && prevPath != "" {
		a.attachExistingSessionRuntime(tab, prevPath, a.ctx)
	}
	if err := rebuildControllerActiveWorkErrorFor(a.controllerForTab(tab), "model"); err != nil {
		return err
	}
	if err := a.ensureTabControllerWorkspace(tab); err != nil {
		return err
	}
	prevPath = a.reconciledSessionPathForTab(tab)
	if prevPath == "" {
		prevPath = a.currentSessionPathFor(tab)
	}
	if a.controllerForTab(tab) == nil && prevPath != "" && a.attachExistingSessionRuntime(tab, prevPath, a.ctx) {
		prevPath = a.reconciledSessionPathForTab(tab)
		if prevPath == "" {
			prevPath = a.currentSessionPathFor(tab)
		}
		if err := rebuildControllerActiveWorkErrorFor(a.controllerForTab(tab), "model"); err != nil {
			return err
		}
	}
	timing.Prepare = time.Since(stageStarted)

	stageStarted = time.Now()
	snap := a.tabRuntimeSnapshot(tab)
	runtime := snap.normalizedRuntime()
	cfg, err := config.LoadForRoot(snap.workspaceRoot)
	if err != nil {
		return err
	}
	entry, ok := cfg.ResolveModel(name)
	pluginRef := false
	if !ok {

		if d, found := extensionModelDescriptor(a.providerCatalogForTab(tab), name); found {
			pluginRef = true
			ok = true
			name = d.Ref
		}
	}
	if !ok {
		return fmt.Errorf("unknown model %q", name)
	}
	if !pluginRef {
		if !modelProviderAccessAllowed(cfg.Desktop.ProviderAccess, entry.Name) {
			return fmt.Errorf("model %q is not available because provider %q is not added", name, entry.Name)
		}
		name = entry.Name + "/" + entry.Model
	}
	effortOverride := cloneStringPtr(snap.effort)
	if effortOverride != nil && !pluginRef {
		normalized, err := config.NormalizeEffort(entry, config.EffortDisplay(&config.ProviderEntry{Effort: *effortOverride}))
		if err != nil {
			effortOverride = nil
		} else {
			effortOverride = &normalized
		}
	}
	timing.Config = time.Since(stageStarted)

	stageStarted = time.Now()
	var carried []provider.Message
	oldCtrl := a.controllerForTab(tab)
	if oldCtrl != nil {
		if prevPath == "" {
			prevPath = oldCtrl.SessionPath()
		}
		if err := a.ensureTabSessionLeaseForRebuild(tab, prevPath, "model"); err != nil {
			return err
		}
		if err := a.snapshotTabForAction(tab, "changing model"); err != nil {
			return err
		}
		prevPath = sessionPathAfterSnapshot(oldCtrl, prevPath)
		carried = oldCtrl.History()
	}
	timing.Snapshot = time.Since(stageStarted)

	sharedHost := a.lookupSharedHost(snap.sharedHostKey)

	stageStarted = time.Now()
	newCtrl, err := boot.Build(a.bootContext(), boot.Options{
		Model:                    name,
		RequireKey:               false,
		StatsSource:              "desktop",
		TaskStore:                a.taskStore(),
		OnConfigLoadWarnings:     a.configLoadWarningsHandler(),
		Sink:                     snap.sink,
		WorkspaceRoot:            snap.workspaceRoot,
		SessionDir:               sessionDirForSnapshot(snap),
		EffortOverride:           cloneStringPtr(effortOverride),
		AgentPreset:              boot.NormalizeAgentPreset(runtime.tokenMode),
		TokenMode:                runtime.tokenMode,
		SharedHost:               sharedHost,
		CleanupPendingReconciler: reconcileDesktopCleanupPending,
		SubagentParentLive:       a.subagentParentProbeForBuild(tab),
		SessionRecoveryMeta:      a.tabSessionRecoveryMeta(tab),
		OnSessionRecovered:       a.handleTabSessionRecovered(tab),

		SessionTemp: sessionTempFromController(oldCtrl),
	})
	if err != nil {
		return err
	}
	timing.Build = time.Since(stageStarted)
	a.bindControllerDisplayRecorder(newCtrl)
	configureControllerRuntime(newCtrl, oldCtrl, runtime)

	stageStarted = time.Now()
	path := agent.ContinueSessionPath(prevPath, newCtrl.SessionDir(), newCtrl.Label())
	if err := a.ensureTabSessionLeaseForRebuild(tab, path, "model"); err != nil {
		newCtrl.Close()
		return err
	}
	restoredRuntime, err := resumeControllerRuntimeWithMessages(newCtrl, carried, path, runtime)
	if err != nil {
		newCtrl.Close()
		return err
	}
	timing.LeaseAndResume = time.Since(stageStarted)
	stageStarted = time.Now()
	a.mu.Lock()
	if err := a.authorizeTabReplacementLocked(tab, newCtrl, "switching model", "model-switch"); err != nil {

		a.mu.Unlock()
		newCtrl.Close()
		tab.releaseSessionLease()
		return err
	}
	tab.Ctrl = newCtrl
	tab.model = name
	tab.effort = cloneStringPtr(effortOverride)
	tab.Label = newCtrl.Label()
	applyNormalizedRuntimeToTabLocked(tab, restoredRuntime)

	a.supersedeTabBuildLocked(tab)
	a.saveTabsLocked()
	a.mu.Unlock()
	if oldCtrl != nil {
		oldCtrl.Close()
	}

	a.clearDeferredRebuild(tab.ID)
	a.persistTabSessionPath(tab, path)

	if path != "" {
		if err := agent.SetBranchModelPreserveUpdated(path, name); err != nil {
			return fmt.Errorf("persist selected model: %w", err)
		}
	}

	tab.clearRuntimeDisplayCurrency()
	a.notifyTabRuntimeRebuilt(tab)
	timing.SwapAndPersist = time.Since(stageStarted)
	return nil
}

var (
	// sessionLeaseContentionRetryInterval and sessionLeaseContentionRetryAttempts
	// bound the retry window for startup session-lease binds that hit a
	// transient in-process holder. CleanupStaleRunning probes a running
	// sub-agent's parent session lease inside every controller build, holding
	// it only for the duration of a metadata rewrite (sub-millisecond); a
	// concurrent tab build that races that probe must not surface a spurious
	// "already open in another Reasonix window" error for a lease that is
	// genuinely free once the probe releases it. A lease held by another
	// window or process stays held for its whole lifetime, so the bounded
	// retry still fails fast there.
	sessionLeaseContentionRetryInterval = 50 * time.Millisecond
	sessionLeaseContentionRetryAttempts = 2
)
