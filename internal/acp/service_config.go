package acp

import (
	"context"
	"encoding/json"
	"strings"

	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

// sessionSetConfigOption applies ACP's generic session-level selectors for
// model, reasoning effort, work mode, and tool approval.
func (s *service) sessionSetConfigOption(ctx context.Context, raw json.RawMessage) (any, error) {
	var p SetSessionConfigOptionParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/set_config_option: " + err.Error()}
	}
	sess := s.session(p.SessionID)
	if sess == nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/set_config_option: unknown session " + p.SessionID}
	}
	cfgState, err := s.configStateForSession(ctx, sess)
	if err != nil {
		return nil, &RPCError{Code: ErrInternal, Message: "session/set_config_option: " + err.Error()}
	}
	option, ok := findConfigOption(cfgState.ConfigOptions, p.ConfigID)
	if !ok {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/set_config_option: unknown config option " + p.ConfigID}
	}
	if !configOptionHasValue(option, p.Value) {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/set_config_option: invalid value " + p.Value + " for " + option.ID}
	}

	var next SessionConfigState
	switch configOptionCategory(option) {
	case "model":
		next, err = s.switchSessionModel(ctx, sess, p.Value)
	case "thought_level":
		next, err = s.switchSessionEffort(ctx, sess, p.Value)
	case "work_mode", "agent_preset":
		next, err = s.switchSessionRuntimeProfile(ctx, sess, p.Value)
	case "tool_approval":
		next, err = s.switchSessionToolApproval(ctx, sess, p.Value)
	default:
		err = &RPCError{Code: ErrInvalidParams, Message: "session/set_config_option: unsupported config option " + option.ID}
	}
	if err != nil {
		return nil, err
	}
	return SetSessionConfigOptionResult{ConfigOptions: next.ConfigOptions}, nil
}

// sessionSetModel keeps older ACP clients working while configOptions becomes
// the preferred model selector.
func (s *service) sessionSetModel(ctx context.Context, raw json.RawMessage) (any, error) {
	var p SetSessionModelParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/set_model: " + err.Error()}
	}
	sess := s.session(p.SessionID)
	if sess == nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/set_model: unknown session " + p.SessionID}
	}
	if _, err := s.switchSessionModel(ctx, sess, p.ModelID); err != nil {
		return nil, err
	}
	return SetSessionModelResult{}, nil
}

// sessionConfigDelta names exactly one config axis a caller asked to change
// (tool approval never rebuilds the controller, so it has no delta here).
// rebuildSession queues these — instead of a fully resolved SessionConfigState
// — while a turn or rebuild is in flight, one queue entry per axis, and
// applyPendingSessionConfig re-resolves the queued set against the session's
// live baseline once the session is idle. That way a queued change to one axis
// can never restore a stale value on another axis that changed in the
// meantime, whether that axis rebuilt already or is queued alongside.
type sessionConfigDelta struct {
	axis           string
	model          string
	effortOverride *string
	runtimeProfile string
}

func (d sessionConfigDelta) clone() sessionConfigDelta {
	d.effortOverride = cloneStringPtr(d.effortOverride)
	return d
}

// mergePendingConfig queues delta with last-write-wins per axis: it replaces a
// queued entry for the same axis and appends otherwise, so a change queued for
// one axis can never drop a change queued for another. Callers hold sess.mu.
func mergePendingConfig(queue []sessionConfigDelta, delta sessionConfigDelta) []sessionConfigDelta {
	for i := range queue {
		if queue[i].axis == delta.axis {
			queue[i] = delta.clone()
			return queue
		}
	}
	return append(queue, delta.clone())
}

// removePendingAxes drops the queue entries whose axis a rebuild is applying,
// keeping entries other requests queued in the meantime so the post-maintenance
// drain still applies them. Callers hold sess.mu.
func removePendingAxes(queue, applied []sessionConfigDelta) []sessionConfigDelta {
	if len(queue) == 0 {
		return nil
	}
	kept := queue[:0]
	for _, q := range queue {
		drop := false
		for _, d := range applied {
			if q.axis == d.axis {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, q)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

func clonePendingConfig(queue []sessionConfigDelta) []sessionConfigDelta {
	if len(queue) == 0 {
		return nil
	}
	out := make([]sessionConfigDelta, len(queue))
	for i := range queue {
		out[i] = queue[i].clone()
	}
	return out
}

func (d sessionConfigDelta) applyTo(p *SessionConfigStateParams) {
	switch d.axis {
	case "model":
		p.Model = d.model
	case "thought_level":
		p.EffortOverride = cloneStringPtr(d.effortOverride)
	case "work_mode", "agent_preset":
		p.RuntimeProfile = d.runtimeProfile
	}
}

// resolveSessionConfigDeltas resolves deltas against the session's current
// config baseline. Calling this fresh at every apply — instead of reusing a
// snapshot taken when a change was first requested — is what keeps a queued
// delta for one axis from clobbering another axis that rebuilt in between.
func (s *service) resolveSessionConfigDeltas(ctx context.Context, sess *acpSession, deltas []sessionConfigDelta) (SessionConfigState, error) {
	params := sess.configStateParams()
	for _, delta := range deltas {
		delta.applyTo(&params)
	}
	cfgState, err := s.sessionConfigState(ctx, params)
	if err != nil {
		return SessionConfigState{}, err
	}
	return withToolApprovalConfig(cfgState, sess.currentToolApprovalMode()), nil
}

func (s *service) switchSessionModel(ctx context.Context, sess *acpSession, modelID string) (SessionConfigState, error) {
	deltas := []sessionConfigDelta{{axis: "model", model: modelID}}
	return s.switchSessionConfig(ctx, sess, deltas)
}

func (s *service) switchSessionEffort(ctx context.Context, sess *acpSession, effort string) (SessionConfigState, error) {
	level := strings.TrimSpace(effort)
	if level == "auto" {
		level = ""
	}
	deltas := []sessionConfigDelta{{axis: "thought_level", effortOverride: &level}}
	return s.switchSessionConfig(ctx, sess, deltas)
}

func (s *service) switchSessionRuntimeProfile(ctx context.Context, sess *acpSession, profile string) (SessionConfigState, error) {

	if !sess.stateChangeMu.TryLock() {
		return SessionConfigState{}, sessionConfigActiveWorkError("session is busy; retry when idle")
	}
	defer sess.stateChangeMu.Unlock()
	sess.mu.Lock()
	if sess.deleted {
		sess.mu.Unlock()
		return SessionConfigState{}, &RPCError{Code: ErrInvalidRequest, Message: "session/set_config_option: session is deleted"}
	}
	status := sess.ctrl.RuntimeStatus()
	if status.PendingPrompt {
		sess.mu.Unlock()
		return SessionConfigState{}, sessionConfigActiveWorkError("answer pending prompts before switching execution setting")
	}
	if sess.running || status.Running {
		sess.mu.Unlock()
		return SessionConfigState{}, sessionConfigActiveWorkError("finish or cancel the active turn before switching execution setting")
	}
	if status.BackgroundJobs > 0 {
		sess.mu.Unlock()
		return SessionConfigState{}, sessionConfigActiveWorkError("stop background jobs before switching execution setting")
	}
	if sess.maintenanceDone != nil {
		sess.mu.Unlock()
		return SessionConfigState{}, sessionConfigActiveWorkError("session is busy; retry when idle")
	}
	ctrl := sess.ctrl
	sess.mu.Unlock()
	if ctrl != nil {
		ctrl.SetAgentPreset(profile)
	}
	// Dual-write session runtime profile label for config option responses.
	var normalized string
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "light", "economy", "eco", "lite":
		normalized = "economy"
	case "delivery", "deliver", "quality":
		normalized = "delivery"
	default:
		normalized = "balanced"
	}
	sess.mu.Lock()
	sess.runtimeProfile = normalized

	if isLightRuntimeProfile(normalized) {
		sess.runtimeState.PlannerMode = "off"
	} else {
		sess.runtimeState.PlannerMode = "on"
	}
	sess.mu.Unlock()
	sess.saveMetaIfPresent()
	cfgState, err := s.configStateForSession(ctx, sess)
	if err != nil {
		return SessionConfigState{}, &RPCError{Code: ErrInternal, Message: "session/set_config_option: " + err.Error()}
	}
	sess.sink.send(configOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: cfgState.ConfigOptions})
	return cfgState, nil
}

// switchSessionConfig resolves and applies one explicit config request without
// letting its full config snapshot roll back another axis. Resolution must be
// repeated after stateChangeMu is acquired: a different-axis rebuild may finish
// while this request is resolving or waiting for the lock, making the earlier
// baseline stale even though this request's own delta is still current.
func (s *service) switchSessionConfig(ctx context.Context, sess *acpSession, deltas []sessionConfigDelta) (SessionConfigState, error) {
	resolve := func() (SessionConfigState, error) {
		cfgState, err := s.resolveSessionConfigDeltas(ctx, sess, deltas)
		if err != nil {
			method := "session/set_config_option"
			if len(deltas) == 1 && deltas[0].axis == "model" {
				method = "session/set_model"
			}
			return SessionConfigState{}, &RPCError{Code: ErrInvalidParams, Message: method + ": " + err.Error()}
		}
		if len(deltas) == 1 && deltas[0].axis == "model" && cfgState.Model == "" {
			return SessionConfigState{}, &RPCError{Code: ErrInvalidRequest, Message: "session/set_model: model switching is unavailable in this session"}
		}
		return cfgState, nil
	}

	if !sess.stateChangeMu.TryLock() {

		cfgState, err := resolve()
		if err != nil {
			return SessionConfigState{}, err
		}
		sess.mu.Lock()
		if sess.maintenanceDone != nil && !sess.deleted {
			for _, delta := range deltas {
				sess.pendingConfig = mergePendingConfig(sess.pendingConfig, delta)
			}
			sess.mu.Unlock()
			sess.sink.send(configOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: cfgState.ConfigOptions})
			return cfgState, nil
		}
		sess.mu.Unlock()
		sess.stateChangeMu.Lock()
	}

	cfgState, err := resolve()
	if err != nil {
		sess.stateChangeMu.Unlock()
		return SessionConfigState{}, err
	}
	didMaintenance := false
	err = s.rebuildSessionLocked(ctx, sess, cfgState, deltas, &didMaintenance)
	sess.stateChangeMu.Unlock()
	if didMaintenance {
		pendingErr := s.applyPendingSessionConfig(ctx, sess)
		s.reportPendingSessionConfigError(ctx, sess, pendingErr, "after maintenance")

		s.drainPendingReload(ctx, sess)

		if current, stateErr := s.configStateForSession(ctx, sess); stateErr == nil {
			cfgState = current
		}
	}
	if err != nil {
		return SessionConfigState{}, err
	}
	return cfgState, nil
}

func (s *service) switchSessionToolApproval(ctx context.Context, sess *acpSession, mode string) (SessionConfigState, error) {
	sess.stateChangeMu.Lock()
	defer sess.stateChangeMu.Unlock()
	mode = normalizeACPToolApprovalMode(mode)
	ctrl := sess.currentCtrl()
	ctrl.SetToolApprovalMode(mode)
	sess.setToolApprovalMode(mode)
	sess.saveMetaIfPresent()
	cfgState, err := s.configStateForSession(ctx, sess)
	if err != nil {
		return SessionConfigState{}, &RPCError{Code: ErrInternal, Message: "session/set_config_option: " + err.Error()}
	}
	sess.sink.send(configOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: cfgState.ConfigOptions})
	return cfgState, nil
}

func (s *service) rebuildSession(ctx context.Context, sess *acpSession, cfgState SessionConfigState, deltas []sessionConfigDelta) error {
	if !sess.stateChangeMu.TryLock() {

		sess.mu.Lock()
		if sess.maintenanceDone != nil && !sess.deleted {
			for _, delta := range deltas {
				sess.pendingConfig = mergePendingConfig(sess.pendingConfig, delta)
			}
			sess.mu.Unlock()
			sess.sink.send(configOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: cfgState.ConfigOptions})
			return nil
		}
		sess.mu.Unlock()
		sess.stateChangeMu.Lock()
	}
	didMaintenance := false
	err := s.rebuildSessionLocked(ctx, sess, cfgState, deltas, &didMaintenance)
	sess.stateChangeMu.Unlock()
	if didMaintenance {
		pendingErr := s.applyPendingSessionConfig(ctx, sess)
		s.reportPendingSessionConfigError(ctx, sess, pendingErr, "after maintenance")

		s.drainPendingReload(ctx, sess)
	}
	return err
}

func (s *service) rebuildSessionLocked(ctx context.Context, sess *acpSession, cfgState SessionConfigState, deltas []sessionConfigDelta, didMaintenance *bool) (retErr error) {
	sess.mu.Lock()
	if sess.deleted {
		sess.mu.Unlock()
		return &RPCError{Code: ErrInvalidRequest, Message: "session config: session is deleted"}
	}
	status := sess.ctrl.RuntimeStatus()
	if status.PendingPrompt {
		sess.mu.Unlock()
		return sessionConfigActiveWorkError("answer pending prompts before switching config")
	}
	if !sess.running && !status.Running && status.BackgroundJobs > 0 {
		sess.mu.Unlock()
		return sessionConfigActiveWorkError("stop background jobs before switching config")
	}
	if sess.running || status.Running || sess.maintenanceDone != nil {
		for _, delta := range deltas {
			sess.pendingConfig = mergePendingConfig(sess.pendingConfig, delta)
		}
		sess.mu.Unlock()
		sess.sink.send(configOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: cfgState.ConfigOptions})
		return nil
	}

	sess.pendingConfig = removePendingAxes(sess.pendingConfig, deltas)

	cur := sess.ctrl
	sink := sess.sink
	mcpServers := clonePluginSpecs(sess.mcpServers)
	cwd := sess.cwd
	modeID := normalizeACPCollaborationMode(sess.modeID)
	goalDraftMode := sess.goalDraftMode
	toolApprovalMode := normalizeACPToolApprovalMode(sess.toolApprovalMode)
	if strings.TrimSpace(cfgState.RuntimeProfile) == "" {
		cfgState.RuntimeProfile = sess.runtimeProfile
	}
	maintenanceDone := make(chan struct{})
	sess.maintenanceDone = maintenanceDone
	*didMaintenance = true
	sess.mu.Unlock()
	defer func() {
		sess.finishMaintenance(maintenanceDone)
	}()

	if err := snapshotACPController(sess, cur); err != nil {
		return &RPCError{Code: ErrInternal, Message: "session config: snapshot before switch: " + err.Error()}
	}

	prevPath := cur.SessionPath()
	carried := cur.History()
	carriedGoal := ""
	if cur.GoalStatus() == control.GoalStatusRunning {
		carriedGoal = cur.Goal()
	}

	rebuildParams := SessionParams{
		Cwd:                cwd,
		MCPServers:         mcpServers,
		Sink:               sink,
		Model:              cfgState.Model,
		EffortOverride:     cloneStringPtr(cfgState.EffortOverride),
		RuntimeProfile:     cfgState.RuntimeProfile,
		OnSessionRecovered: s.sessionRecoveredHandler(sess.id),
	}

	s.bindClientIO(&rebuildParams, sess.id)
	newCtrl, err := s.factory.NewSession(ctx, rebuildParams)
	if err != nil {
		return &RPCError{Code: ErrInternal, Message: "session config: " + err.Error()}
	}
	newCtrl.EnableInteractiveApproval()
	runtimeState, err := s.sessionRuntimeState(ctx, SessionRuntimeStateParams{
		Cwd: cwd, Model: cfgState.Model, RuntimeProfile: cfgState.RuntimeProfile,
	})
	if err != nil {
		newCtrl.ReleaseResources()
		return &RPCError{Code: ErrInternal, Message: "session config: runtime state: " + err.Error()}
	}

	if fresh := newCtrl.History(); len(fresh) > 0 && fresh[0].Role == provider.RoleSystem {
		if len(carried) > 0 && carried[0].Role == provider.RoleSystem {
			carried[0] = fresh[0]
		} else {
			carried = append([]provider.Message{fresh[0]}, carried...)
		}
	}
	newCtrl.AdoptHistory(carried, prevPath)

	newCtrl.SetToolApprovalMode(toolApprovalMode)
	switch modeID {
	case sessionModePlan:
		newCtrl.SetPlanMode(true)
	case sessionModeGoal:
		newCtrl.SetPlanMode(false)
		if carriedGoal != "" {
			newCtrl.SetGoal(carriedGoal)
		}
	default:
		newCtrl.SetPlanMode(false)
	}

	if prev, ok := cur.(*control.Controller); ok {
		newCtrl.InheritLifecycleFrom(prev)

		newCtrl.RestoreSessionAuthorizations(prev.SessionAuthorizations())
	}

	if err := s.prepareACPReplacementAuthority(sess, newCtrl, cur, prevPath, "snapshot after switch"); err != nil {
		newCtrl.ReleaseResources()
		return &RPCError{Code: ErrInternal, Message: "session config: " + err.Error()}
	}

	sess.mu.Lock()
	if sess.deleted {
		sess.mu.Unlock()
		newCtrl.ReleaseResources()
		return &RPCError{Code: ErrInvalidRequest, Message: "session config: session is deleted"}
	}
	if sess.ctrl != cur {
		sess.mu.Unlock()
		newCtrl.ReleaseResources()
		return sessionConfigActiveWorkError("session changed while switching config; retry")
	}
	sess.ctrl = newCtrl
	sess.model = cfgState.Model
	sess.effortOverride = cloneStringPtr(cfgState.EffortOverride)
	sess.runtimeProfile = cfgState.RuntimeProfile
	sess.toolApprovalMode = toolApprovalMode
	sess.runtimeState = runtimeState
	sess.modeID = modeID
	sess.goalDraftMode = goalDraftMode
	if sess.transcript != "" && sessionFileExists(sess.transcript) {
		_ = saveACPMeta(sess.transcript, sess.metaLocked())
	}
	sess.mu.Unlock()
	sink.bindApprove(newCtrl.Approve)
	sink.bindAnswer(newCtrl.AnswerQuestion)

	cur.ReleaseResources()
	s.sendAvailableCommands(sess)
	sink.send(configOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: cfgState.ConfigOptions})
	return nil
}

func (s *service) applyPendingSessionConfig(ctx context.Context, sess *acpSession) error {
	var firstErr error
	for {
		if s.session(sess.id) != sess {
			return firstErr
		}

		sess.stateChangeMu.Lock()
		didMaintenance := false
		sess.mu.Lock()
		if sess.deleted || len(sess.pendingConfig) == 0 {
			sess.mu.Unlock()
			sess.stateChangeMu.Unlock()
			return firstErr
		}
		deltas := clonePendingConfig(sess.pendingConfig)

		sess.mu.Unlock()

		cfgState, err := s.resolveSessionConfigDeltas(ctx, sess, deltas)
		if err != nil {
			sess.mu.Lock()
			if !sess.deleted && !sess.running && sess.maintenanceDone == nil {
				sess.pendingConfig = removePendingAxes(sess.pendingConfig, deltas)
			}
			sess.mu.Unlock()
			sess.stateChangeMu.Unlock()
			if firstErr != nil {
				s.reportPendingSessionConfigError(ctx, sess, err, "after failed maintenance")
				return firstErr
			}
			return err
		}

		err = s.rebuildSessionLocked(ctx, sess, cfgState, deltas, &didMaintenance)
		if err != nil && !didMaintenance {

			sess.mu.Lock()
			if !sess.deleted && !sess.running && sess.maintenanceDone == nil {
				sess.pendingConfig = removePendingAxes(sess.pendingConfig, deltas)
			}
			sess.mu.Unlock()
		}
		sess.stateChangeMu.Unlock()

		if err != nil {
			if firstErr == nil {
				firstErr = err
			} else {
				s.reportPendingSessionConfigError(ctx, sess, err, "after failed maintenance")
			}
			if !didMaintenance {
				return firstErr
			}
		}
		if !didMaintenance {
			return firstErr
		}

	}
}

func (s *service) reportPendingSessionConfigError(ctx context.Context, sess *acpSession, err error, when string) {
	if err == nil || sess == nil || sess.sink == nil {
		return
	}
	sess.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "session config switch failed " + when + ": " + err.Error()})

	if current, stateErr := s.configStateForSession(ctx, sess); stateErr == nil {
		sess.sink.send(configOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: current.ConfigOptions})
	}
}

type activeSessionConfigWorkError struct {
	*RPCError
}

func (e *activeSessionConfigWorkError) Unwrap() error {
	return e.RPCError
}

func sessionConfigActiveWorkError(message string) error {
	return &activeSessionConfigWorkError{
		RPCError: &RPCError{Code: ErrInvalidRequest, Message: "session config: " + message},
	}
}
