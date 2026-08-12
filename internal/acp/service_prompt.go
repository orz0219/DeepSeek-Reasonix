package acp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/sessioninbox"
)

func (s *service) openExistingSession(ctx context.Context, method, id, cwdParam string, servers []MCPServerSpec, replay bool) (SessionConfigState, error) {
	if err := validateSessionID(method, id); err != nil {
		return SessionConfigState{}, err
	}
	cwd, err := s.resolveSessionCwd(cwdParam, id)
	if err != nil {
		return SessionConfigState{}, &RPCError{Code: ErrInvalidParams, Message: method + ": " + err.Error()}
	}
	mcpServers, err := mcpSpecs(servers, cwd)
	if err != nil {
		return SessionConfigState{}, &RPCError{Code: ErrInvalidParams, Message: method + ": " + err.Error()}
	}

	if sess := s.session(id); sess != nil {
		if agent.IsCleanupPending(sess.transcript) {
			return SessionConfigState{}, &RPCError{Code: ErrInvalidParams, Message: method + ": unknown session " + id}
		}
		if replay {
			ctrl := sess.currentCtrl()
			replaySink := newUpdateSink(s.conn, id)
			replaySink.bindCwd(sess.cwd)
			replaySink.replay(ctrl.History())
		}
		cfgState, err := s.configStateForSession(ctx, sess)
		if err != nil {
			return SessionConfigState{}, &RPCError{Code: ErrInternal, Message: method + ": " + err.Error()}
		}
		return cfgState, nil
	}

	var saved acpSessionMeta
	persistedPath := ""
	if dir := s.sessionDir(); dir != "" {
		persistedPath = resolveTranscriptPath(dir, id)
		if agent.IsCleanupPending(persistedPath) {
			return SessionConfigState{}, &RPCError{Code: ErrInvalidParams, Message: method + ": unknown session " + id}
		}
		meta, _, metaErr := loadACPMeta(persistedPath)
		if metaErr != nil {
			return SessionConfigState{}, &RPCError{Code: ErrInternal, Message: method + ": " + metaErr.Error()}
		}
		saved = meta
	}
	cfgParams := SessionConfigStateParams{
		Cwd:            cwd,
		Model:          saved.Model,
		EffortOverride: cloneStringPtr(saved.EffortOverride),
		RuntimeProfile: saved.RuntimeProfile,
	}
	cfgState, err := s.sessionConfigState(ctx, cfgParams)
	if err != nil && (strings.TrimSpace(saved.Model) != "" || saved.EffortOverride != nil || strings.TrimSpace(saved.RuntimeProfile) != "") {
		cfgState, err = s.sessionConfigState(ctx, SessionConfigStateParams{Cwd: cwd})
	}
	if err != nil {
		return SessionConfigState{}, &RPCError{Code: ErrInternal, Message: method + ": " + err.Error()}
	}
	runtimeState, err := s.sessionRuntimeState(ctx, SessionRuntimeStateParams{
		Cwd: cwd, Model: cfgState.Model, RuntimeProfile: cfgState.RuntimeProfile,
	})
	if err != nil {
		return SessionConfigState{}, &RPCError{Code: ErrInternal, Message: method + ": " + err.Error()}
	}

	sink := newUpdateSink(s.conn, id)
	sink.bindCwd(cwd)
	sink.bindExtensionSurface(s.extensionSurfaceSupported())
	sessionParams := SessionParams{
		Cwd:                cwd,
		MCPServers:         mcpServers,
		Sink:               sink,
		Model:              cfgState.Model,
		EffortOverride:     cloneStringPtr(cfgState.EffortOverride),
		RuntimeProfile:     cfgState.RuntimeProfile,
		OnSessionRecovered: s.sessionRecoveredHandler(id),
	}
	s.bindClientIO(&sessionParams, id)
	ctrl, err := s.factory.NewSession(ctx, sessionParams)
	if err != nil {
		return SessionConfigState{}, &RPCError{Code: ErrInternal, Message: method + ": " + err.Error()}
	}
	ctrl.EnableInteractiveApproval()
	sink.bindApprove(ctrl.Approve)
	sink.bindAnswer(ctrl.AnswerQuestion)

	dir := ctrl.SessionDir()
	if dir == "" {
		ctrl.Close()
		return SessionConfigState{}, &RPCError{Code: ErrInternal, Message: method + ": persistence is disabled"}
	}
	path := resolveTranscriptPath(dir, id)
	if path != persistedPath && agent.IsCleanupPending(path) {
		ctrl.Close()
		return SessionConfigState{}, &RPCError{Code: ErrInvalidParams, Message: method + ": unknown session " + id}
	}

	lease, leaseErr := agent.TryAcquireSessionLease(path)
	if leaseErr != nil {
		ctrl.Close()
		return SessionConfigState{}, sessionLeaseBindError(method, leaseErr)
	}
	loaded, err := agent.LoadSession(path)
	if err != nil {
		lease.Release()
		ctrl.Close()
		return SessionConfigState{}, &RPCError{Code: ErrInvalidParams, Message: method + ": unknown session " + id}
	}
	if err := resumeACPControllerForWrite(ctrl, loaded, path, lease); err != nil {
		return SessionConfigState{}, sessionLeaseBindError(method, err)
	}
	toolApprovalMode := normalizeACPToolApprovalMode(saved.ToolApprovalMode)
	ctrl.SetToolApprovalMode(toolApprovalMode)
	modeID := normalizeACPCollaborationMode(saved.CollaborationMode)
	goalDraftMode := false
	switch modeID {
	case sessionModePlan:
		ctrl.SetPlanMode(true)
	case sessionModeGoal:
		ctrl.SetPlanMode(false)
		goalDraftMode = ctrl.GoalStatus() != control.GoalStatusRunning
	default:
		if ctrl.GoalStatus() == control.GoalStatusRunning {
			modeID = sessionModeGoal
		} else {
			modeID = sessionModeNormal
			ctrl.SetPlanMode(false)
		}
	}

	meta := metadataForLoadedSession(path, id, cwd, ctrl.History())
	meta.Model = cfgState.Model
	meta.EffortOverride = cloneStringPtr(cfgState.EffortOverride)
	meta.RuntimeProfile = cfgState.RuntimeProfile
	meta.ToolApprovalMode = toolApprovalMode
	meta.CollaborationMode = modeID
	cfgState = withToolApprovalConfig(cfgState, toolApprovalMode)
	sess := &acpSession{
		id:               id,
		ctrl:             ctrl,
		sink:             sink,
		transcript:       path,
		cwd:              meta.Cwd,
		mcpServers:       clonePluginSpecs(mcpServers),
		model:            cfgState.Model,
		effortOverride:   cloneStringPtr(cfgState.EffortOverride),
		runtimeProfile:   cfgState.RuntimeProfile,
		toolApprovalMode: toolApprovalMode,
		runtimeState:     runtimeState,
		status:           restoreStatusTelemetry(saved.Status),
		modeID:           modeID,
		goalDraftMode:    goalDraftMode,
		title:            meta.Title,
		createdAt:        meta.CreatedAt,
		updatedAt:        meta.UpdatedAt,
		lease:            lease,
	}
	s.bindStatusEvents(sess)
	if err := saveACPMeta(path, sess.meta()); err != nil {
		sess.releaseSessionLease()
		ctrl.Close()
		return SessionConfigState{}, &RPCError{Code: ErrInternal, Message: method + ": " + err.Error()}
	}
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()

	if replay {
		sink.replay(ctrl.History())
	}
	return enrichStateWithExtensionModels(cfgState, ctrl.ProviderCatalog()), nil
}

// transcriptPath is where a session's transcript lives — keyed by id so
// session/load can recover it. Distinct from the cli's timestamp-labelled
// chat/run session files (those are addressed by a picker, not by id).
func transcriptPath(dir, id string) string {
	return filepath.Join(dir, id+".jsonl")
}

// resolveTranscriptPath returns the transcript file session id currently
// lives in. That is the id-keyed path by default; after a snapshot recovery
// moved the live session onto a recovery branch, the id-keyed sidecar carries
// an ActiveTranscript redirect (written by sessionRecoveredHandler) that
// load/resume/delete/meta lookups must follow, or a restart silently reopens
// the pre-recovery transcript. The redirect is a basename, must stay inside
// dir, and its target must exist and claim the same session id; anything else
// falls back to the id-keyed path.
func resolveTranscriptPath(dir, id string) string {
	path := transcriptPath(dir, id)
	meta, ok, err := loadACPMeta(path)
	if err != nil || !ok {
		return path
	}
	active := strings.TrimSpace(meta.ActiveTranscript)
	if active == "" || active == filepath.Base(path) {
		return path
	}
	if filepath.Base(active) != active {
		return path
	}
	resolved := filepath.Join(dir, active)
	if !sessionFileExists(resolved) {
		return path
	}
	targetMeta, ok, err := loadACPMeta(resolved)
	if err != nil || !ok || targetMeta.SessionID != id {
		return path
	}
	return resolved
}

// sessionPrompt runs one turn. It flattens the prompt blocks to text and runs the
// session's controller synchronously under a per-turn cancelable context (so
// session/cancel can stop it), then reports why the turn ended. The controller
// streams the turn's events to the session's sink as it runs.
func (s *service) sessionPrompt(ctx context.Context, raw json.RawMessage) (any, error) {
	var p SessionPromptParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/prompt: " + err.Error()}
	}
	sess := s.session(p.SessionID)
	if sess == nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/prompt: unknown session " + p.SessionID}
	}
	text := FlattenPrompt(p.Prompt)
	if text == "" {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/prompt: empty prompt"}
	}
	text = s.resolveSlashPrompt(ctx, sess, text)

	runCtx, cancel, ok := sess.begin(ctx)
	if !ok {
		return nil, &RPCError{Code: ErrInvalidRequest, Message: "session/prompt: session already has an active prompt"}
	}
	if sess.status == nil {
		sess.status = newStatusTelemetry()
	}
	sess.status.beginTurn()
	s.publishStatus(sess, "phase")
	sess.sink.setTurnContext(runCtx)
	if sess.takeGoalDraftMode() {
		sess.currentCtrl().SetGoal(text)
		sess.saveMetaIfPresent()
	}
	defer func() {
		sess.sink.clearTurnContext()
		s.finishTurn(ctx, sess)
		cancel()
	}()
	runErr := drainACPInbox(runCtx, sess.ctrl, sess.ctrl.RunTurn(runCtx, text))

	statusEvent := sess.status.finishTurn(
		runErr,
		runCtx.Err() != nil,
		sess.currentCtrl().GoalStatus(),
		finalAssistantSummary(sess.currentCtrl()),
	)
	s.publishStatus(sess, statusEvent)

	sess.persistAfterTurn(text)

	stop := StopEndTurn
	if runErr != nil {
		if runCtx.Err() != nil {
			stop = StopCancelled
		} else {
			stop = StopError
		}
	}
	res := SessionPromptResult{StopReason: stop}
	if sess.transcript != "" {
		res.TranscriptPath = &sess.transcript
	}
	return res, nil
}

// sessionSteer durably persists guidance then attempts mid-turn admission.
// Parameter/session errors remain RPC errors; busy rejection returns a
// disposition so clients can keep the durable follow-up.
func (s *service) sessionSteer(_ context.Context, raw json.RawMessage) (any, error) {
	var p SessionSteerParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: sessionSteerMethod + ": " + err.Error()}
	}
	sess := s.session(p.SessionID)
	if sess == nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: sessionSteerMethod + ": unknown session " + p.SessionID}
	}
	text := FlattenPrompt(p.Prompt)
	if text == "" {
		return nil, &RPCError{Code: ErrInvalidParams, Message: sessionSteerMethod + ": empty prompt"}
	}
	ctrl := sess.currentCtrl()
	if api, ok := ctrl.(control.SessionAPI); ok {
		if ensurer, ok := any(api).(interface{ EnsureSessionPath() }); ok {
			ensurer.EnsureSessionPath()
		}

		if api.SessionPath() != "" {
			rec, err := api.TryEnqueueAndSteer(control.InboxRequest{
				Intent:  sessioninbox.IntentSteer,
				Display: text,
				Raw:     text,
				Submit:  text,
				Source:  "acp",
			})
			if err != nil {
				return nil, &RPCError{Code: ErrInvalidRequest, Message: sessionSteerMethod + ": " + err.Error()}
			}
			return SessionSteerResult{ItemID: rec.ItemID, Disposition: string(rec.Disposition)}, nil
		}
	}

	if !ctrl.TrySteer(text) {
		return nil, &RPCError{Code: ErrInvalidRequest, Message: sessionSteerMethod + ": session has no active prompt"}
	}
	return SessionSteerResult{Disposition: "steer_accepted"}, nil
}

// sessionReloadExtensions rebuilds a session's agent runtime in place —
// tools, skills, commands, hooks, MCP servers, and providers are re-discovered
// — while the session (transcript, approval grants, goal and recovery state)
// carries over via boot.Rebuild. It follows the same contract as a config
// switch: a turn or rebuild in flight coalesces exactly one queued reload,
// drained when the session goes idle; a failure keeps the old controller fully
// usable; the old controller's resources are released only after the swap.
func (s *service) sessionReloadExtensions(ctx context.Context, raw json.RawMessage) (any, error) {
	var p SessionReloadExtensionsParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: sessionReloadExtensionsMethod + ": " + err.Error()}
	}
	sess := s.session(p.SessionID)
	if sess == nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: sessionReloadExtensionsMethod + ": unknown session " + p.SessionID}
	}
	return s.reloadSessionExtensions(ctx, sess)
}

func (s *service) reloadSessionExtensions(ctx context.Context, sess *acpSession) (any, error) {
	rebuilder, ok := s.factory.(SessionRebuilder)
	if !ok {
		return nil, &RPCError{Code: ErrInvalidRequest, Message: sessionReloadExtensionsMethod + ": runtime reload is unavailable in this session"}
	}
	if !sess.stateChangeMu.TryLock() {

		sess.mu.Lock()
		if sess.maintenanceDone != nil && !sess.deleted {
			sess.pendingReload = true
			sess.mu.Unlock()
			return SessionReloadExtensionsResult{Queued: true}, nil
		}
		sess.mu.Unlock()
		sess.stateChangeMu.Lock()
	}
	didMaintenance := false
	res, err := s.reloadSessionExtensionsLocked(ctx, sess, rebuilder, &didMaintenance)
	sess.stateChangeMu.Unlock()
	if didMaintenance {
		s.reportPendingSessionConfigError(ctx, sess, s.applyPendingSessionConfig(ctx, sess), "after maintenance")
		s.drainPendingReload(ctx, sess)
	}
	return res, err
}

// reloadSessionExtensionsLocked is reloadSessionExtensions' body; callers hold
// stateChangeMu. The busy/queue checks and the publish/close ordering mirror
// rebuildSessionLocked, but the build itself goes through the factory's
// boot.Rebuild path instead of NewSession + manual migration.
func (s *service) reloadSessionExtensionsLocked(ctx context.Context, sess *acpSession, rebuilder SessionRebuilder, didMaintenance *bool) (any, error) {
	sess.mu.Lock()
	if sess.deleted {
		sess.mu.Unlock()
		return nil, &RPCError{Code: ErrInvalidRequest, Message: sessionReloadExtensionsMethod + ": session is deleted"}
	}
	status := sess.ctrl.RuntimeStatus()
	if status.PendingPrompt {
		sess.mu.Unlock()
		return nil, sessionConfigActiveWorkError("answer pending prompts before reloading the runtime")
	}
	if !sess.running && !status.Running && status.BackgroundJobs > 0 {
		sess.mu.Unlock()
		return nil, sessionConfigActiveWorkError("stop background jobs before reloading the runtime")
	}
	if sess.running || status.Running || sess.maintenanceDone != nil {

		sess.pendingReload = true
		sess.mu.Unlock()
		return SessionReloadExtensionsResult{Queued: true}, nil
	}

	sess.pendingReload = false
	cur := sess.ctrl
	sink := sess.sink
	mcpServers := clonePluginSpecs(sess.mcpServers)
	cwd := sess.cwd
	model := sess.model
	effortOverride := cloneStringPtr(sess.effortOverride)
	runtimeProfile := sess.runtimeProfile
	maintenanceDone := make(chan struct{})
	sess.maintenanceDone = maintenanceDone
	*didMaintenance = true
	sess.mu.Unlock()
	defer func() {
		sess.finishMaintenance(maintenanceDone)
	}()

	if err := snapshotACPController(sess, cur); err != nil {
		return nil, &RPCError{Code: ErrInternal, Message: sessionReloadExtensionsMethod + ": snapshot before reload: " + err.Error()}
	}

	prevPath := cur.SessionPath()
	old, ok := cur.(*control.Controller)
	if !ok {
		return nil, &RPCError{Code: ErrInternal, Message: sessionReloadExtensionsMethod + ": session controller does not support rebuild"}
	}
	rebuildParams := SessionParams{
		Cwd:                cwd,
		MCPServers:         mcpServers,
		Sink:               sink,
		Model:              model,
		EffortOverride:     effortOverride,
		RuntimeProfile:     runtimeProfile,
		OnSessionRecovered: s.sessionRecoveredHandler(sess.id),
	}

	s.bindClientIO(&rebuildParams, sess.id)
	newCtrl, err := rebuilder.RebuildSession(ctx, rebuildParams, old)
	if err != nil {
		return nil, &RPCError{Code: ErrInternal, Message: sessionReloadExtensionsMethod + ": " + err.Error()}
	}
	newCtrl.EnableInteractiveApproval()

	runtimeState, err := s.sessionRuntimeState(ctx, SessionRuntimeStateParams{
		Cwd: cwd, Model: model, RuntimeProfile: runtimeProfile,
	})
	if err != nil {
		newCtrl.ReleaseResources()
		return nil, &RPCError{Code: ErrInternal, Message: sessionReloadExtensionsMethod + ": runtime state: " + err.Error()}
	}

	if err := s.prepareACPReplacementAuthority(sess, newCtrl, cur, prevPath, "snapshot after reload"); err != nil {
		newCtrl.ReleaseResources()
		return nil, &RPCError{Code: ErrInternal, Message: sessionReloadExtensionsMethod + ": " + err.Error()}
	}

	sess.mu.Lock()
	if sess.deleted {
		sess.mu.Unlock()
		newCtrl.ReleaseResources()
		return nil, &RPCError{Code: ErrInvalidRequest, Message: sessionReloadExtensionsMethod + ": session is deleted"}
	}
	if sess.ctrl != cur {
		sess.mu.Unlock()
		newCtrl.ReleaseResources()
		return nil, sessionConfigActiveWorkError("session changed while reloading; retry")
	}
	sess.ctrl = newCtrl
	sess.runtimeState = runtimeState
	if sess.transcript != "" && sessionFileExists(sess.transcript) {
		_ = saveACPMeta(sess.transcript, sess.metaLocked())
	}
	sess.mu.Unlock()
	sink.bindApprove(newCtrl.Approve)
	sink.bindAnswer(newCtrl.AnswerQuestion)

	cur.ReleaseResources()

	s.sendAvailableCommands(sess)
	return SessionReloadExtensionsResult{}, nil
}

// drainPendingReload runs the coalesced reloadExtensions request once the
// session is idle. Called from finishTurn and after a config switch's or a
// reload's own maintenance completes; callers must NOT hold stateChangeMu
// (the reload re-acquires it).
func (s *service) drainPendingReload(ctx context.Context, sess *acpSession) {
	if _, ok := s.factory.(SessionRebuilder); !ok {
		return
	}
	sess.mu.Lock()
	if !sess.pendingReload || sess.deleted || sess.running || sess.maintenanceDone != nil || len(sess.pendingConfig) > 0 {
		sess.mu.Unlock()
		return
	}
	sess.mu.Unlock()
	if _, err := s.reloadSessionExtensions(ctx, sess); err != nil {
		s.reportPendingSessionConfigError(ctx, sess, err, "after queued reload")
	}
}

// finishTurn reconciles controller-side drift and drains any config switch
// queued during the turn. Drift must be reconciled before finish() exposes
// the session as idle: a concurrent config switch races on sess.running, and
// if it wins that race while modeID/toolApprovalMode are still stale (a
// slash command or plan/goal completion changed them inside the turn), it
// rebuilds the replacement controller from the outgoing state instead of the
// one this turn actually ended in.
func (s *service) finishTurn(ctx context.Context, sess *acpSession) {
	s.emitModeDrift(sess)
	s.emitToolApprovalDrift(ctx, sess)
	sess.finish()
	s.reportPendingSessionConfigError(ctx, sess, s.applyPendingSessionConfig(ctx, sess), "after turn")

	s.drainPendingReload(ctx, sess)

	s.emitModeDrift(sess)
	s.emitToolApprovalDrift(ctx, sess)
}
