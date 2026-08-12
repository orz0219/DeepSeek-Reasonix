package acp

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
)

// initialize advertises the agent's capability set: persisted load plus ACP v1
// list/resume/close/delete lifecycle helpers, prompts carrying inline resource
// text (embeddedContext) but not image/audio, and stdio / Streamable HTTP MCP
// (no legacy sse).
func (s *service) initialize(_ context.Context, raw json.RawMessage) (any, error) {
	var p InitializeParams
	if len(raw) > 0 && json.Unmarshal(raw, &p) == nil {
		s.setClientCapabilities(p.ClientCapabilities)
	}
	return InitializeResult{
		ProtocolVersion: ProtocolVersion,
		AgentCapabilities: AgentCapabilities{
			LoadSession: true,
			SessionCapabilities: SessionCapabilities{
				List:   &EmptyCapability{},
				Resume: &EmptyCapability{},
				Close:  &EmptyCapability{},
				Delete: &EmptyCapability{},
			},
			PromptCapabilities: PromptCapabilities{
				Image:           false,
				Audio:           false,
				EmbeddedContext: true,
			},
			MCPCapabilities: MCPCapabilities{HTTP: true, SSE: false},
			Meta: map[string]any{
				"reasonix.io": ReasonixExtensionCapabilities{
					SessionSteer: &SessionSteerCapability{Method: sessionSteerMethod},
					SessionInbox: &SessionInboxCapability{
						SchemaVersion: sessionInboxSchemaVersion,
						Methods: map[string]string{
							"enqueue":   sessionInboxEnqueueMethod,
							"list":      sessionInboxListMethod,
							"get":       sessionInboxGetMethod,
							"update":    sessionInboxUpdateMethod,
							"delete":    sessionInboxDeleteMethod,
							"move":      sessionInboxMoveMethod,
							"setPaused": sessionInboxPauseMethod,
							"retry":     sessionInboxRetryMethod,
							"refresh":   sessionInboxRefreshMethod,
						},
					},
					SessionReloadExtensions: &SessionReloadExtensionsCapability{Method: sessionReloadExtensionsMethod},
					ExtensionSurface:        &ExtensionSurfaceCapability{Supported: true, SchemaVersion: reasonixExtensionSurfaceSchemaVersion},
				},
				sessionStatusMethod:       ReasonixSchemaCapability{SchemaVersion: reasonixStatusSchemaVersion},
				sessionStatusUpdateMethod: ReasonixSchemaCapability{SchemaVersion: reasonixStatusSchemaVersion},
			},
		},
		AgentInfo:   Implementation{Name: s.info.Name, Version: s.info.Version},
		AuthMethods: []AuthMethod{reasonixSetupAuthMethod()},
	}, nil
}

func reasonixSetupAuthMethod() AuthMethod {
	return AuthMethod{
		ID:          "reasonix-setup",
		Name:        "Reasonix setup",
		Description: "Configure Reasonix providers and credentials in a terminal",
		Type:        "terminal",
		Args:        []string{"setup"},
	}
}

func (s *service) authenticate(_ context.Context, raw json.RawMessage) (any, error) {
	var p AuthenticateParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "authenticate: " + err.Error()}
	}
	if strings.TrimSpace(p.MethodID) != reasonixSetupAuthMethod().ID {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "authenticate: unknown methodId " + p.MethodID}
	}
	return AuthenticateResult{}, nil
}

// sessionNew opens a session: it mints an id, builds the session's sink bound to
// that id, asks the Factory to assemble the controller, switches the controller
// to interactive approval (so tool gates surface as ApprovalRequest events the
// sink forwards), and registers it.
func (s *service) sessionNew(ctx context.Context, raw json.RawMessage) (any, error) {
	var p SessionNewParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: ErrInvalidParams, Message: "session/new: " + err.Error()}
		}
	}
	cwd, err := s.resolveSessionCwd(p.Cwd, "")
	if err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/new: " + err.Error()}
	}
	mcpServers, err := mcpSpecs(p.MCPServers, cwd)
	if err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/new: " + err.Error()}
	}
	cfgState, err := s.sessionConfigState(ctx, SessionConfigStateParams{Cwd: cwd})
	if err != nil {
		return nil, &RPCError{Code: ErrInternal, Message: "session/new: " + err.Error()}
	}
	cfgState = withToolApprovalConfig(cfgState, control.ToolApprovalAsk)
	runtimeState, err := s.sessionRuntimeState(ctx, SessionRuntimeStateParams{
		Cwd: cwd, Model: cfgState.Model, RuntimeProfile: cfgState.RuntimeProfile,
	})
	if err != nil {
		return nil, &RPCError{Code: ErrInternal, Message: "session/new: " + err.Error()}
	}

	id, err := newSessionID()
	if err != nil {
		return nil, &RPCError{Code: ErrInternal, Message: "session/new: " + err.Error()}
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
		return nil, &RPCError{Code: ErrInternal, Message: "session/new: " + err.Error()}
	}
	ctrl.EnableInteractiveApproval()
	sink.bindApprove(ctrl.Approve)
	sink.bindAnswer(ctrl.AnswerQuestion)

	now := time.Now().UTC()
	sess := &acpSession{
		id:               id,
		ctrl:             ctrl,
		sink:             sink,
		cwd:              cwd,
		mcpServers:       clonePluginSpecs(mcpServers),
		model:            cfgState.Model,
		effortOverride:   cloneStringPtr(cfgState.EffortOverride),
		runtimeProfile:   cfgState.RuntimeProfile,
		toolApprovalMode: control.ToolApprovalAsk,
		runtimeState:     runtimeState,
		status:           newStatusTelemetry(),
		modeID:           sessionModeNormal,
		createdAt:        now,
		updatedAt:        now,
	}
	s.bindStatusEvents(sess)

	if dir := ctrl.SessionDir(); dir != "" {
		sess.transcript = transcriptPath(dir, id)
		lease, err := agent.TryAcquireSessionLease(sess.transcript)
		if err != nil {
			ctrl.Close()
			return nil, sessionLeaseBindError("session/new", err)
		}
		sess.lease = lease
		ctrl.SetFreshSessionPath(sess.transcript)
		if err := bindACPWriteAuthorityOrClose(ctrl, lease); err != nil {
			sess.lease = nil
			return nil, sessionLeaseBindError("session/new", err)
		}
	}

	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()

	cfgState = enrichStateWithExtensionModels(cfgState, ctrl.ProviderCatalog())
	return afterResponse{
		result: SessionNewResult{
			SessionID:     id,
			Models:        cfgState.Models,
			Modes:         sessionModesState(sessionModeNormal),
			ConfigOptions: cfgState.ConfigOptions,
		},
		after: func() { s.sendAvailableCommands(sess) },
	}, nil
}

func sessionModesState(current string) *SessionModeState {
	return &SessionModeState{
		CurrentModeID: current,
		AvailableModes: []SessionMode{
			{ID: sessionModeNormal, Name: "Normal", Description: "Work directly and pause when user input is required"},
			{ID: sessionModePlan, Name: "Plan", Description: "Research and propose a plan before making changes"},
			{ID: sessionModeGoal, Name: "Goal", Description: "Keep advancing the next prompt as a goal until complete or blocked"},
		},
	}
}

// sessionSetMode switches the session's operating mode and confirms it with a
// current_mode_update, per the ACP session-mode contract.
func (s *service) sessionSetMode(ctx context.Context, raw json.RawMessage) (any, error) {
	var p SessionSetModeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/set_mode: " + err.Error()}
	}
	sess := s.session(p.SessionID)
	if sess == nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/set_mode: unknown session " + p.SessionID}
	}
	sess.stateChangeMu.Lock()
	defer sess.stateChangeMu.Unlock()
	ctrl := sess.currentCtrl()
	nextMode := p.ModeID
	legacyApproval := ""
	switch p.ModeID {
	case sessionModeNormal:
		ctrl.SetPlanMode(false)
		ctrl.ClearGoal()
	case sessionModePlan:
		ctrl.ClearGoal()
		ctrl.SetPlanMode(true)
	case sessionModeGoal:
		ctrl.SetPlanMode(false)
	case sessionModeLegacyDefault:
		nextMode = sessionModeNormal
		legacyApproval = control.ToolApprovalAsk
		ctrl.SetPlanMode(false)
		ctrl.ClearGoal()
	case sessionModeLegacyAuto:
		nextMode = sessionModeNormal
		legacyApproval = control.ToolApprovalYolo
		ctrl.SetPlanMode(false)
		ctrl.ClearGoal()
	default:
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/set_mode: unknown modeId " + p.ModeID}
	}
	sess.setGoalDraftMode(nextMode == sessionModeGoal && ctrl.GoalStatus() != control.GoalStatusRunning)
	if legacyApproval != "" {
		ctrl.SetToolApprovalMode(legacyApproval)
		sess.setToolApprovalMode(legacyApproval)
		if cfgState, err := s.configStateForSession(ctx, sess); err == nil {
			sess.sink.send(configOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: cfgState.ConfigOptions})
		}
	}
	if sess.swapModeID(nextMode) != nextMode {
		sess.sink.send(currentModeUpdate{SessionUpdate: "current_mode_update", CurrentModeID: nextMode})
	}
	sess.saveMetaIfPresent()
	return SessionSetModeResult{}, nil
}

// emitModeDrift reports controller-side mode flips (plan mode auto-exits when
// a plan is approved, a config rebuild resets switches) as current_mode_update
// so the client's mode picker stays truthful.
func (s *service) emitModeDrift(sess *acpSession) {

	sess.stateChangeMu.Lock()
	defer sess.stateChangeMu.Unlock()
	ctrl := sess.currentCtrl()
	current := sessionModeNormal
	switch {
	case ctrl.PlanMode():
		current = sessionModePlan
	case ctrl.GoalStatus() == control.GoalStatusRunning || sess.isGoalDraftMode():
		current = sessionModeGoal
	}
	if sess.swapModeID(current) != current {
		sess.sink.send(currentModeUpdate{SessionUpdate: "current_mode_update", CurrentModeID: current})
		sess.saveMetaIfPresent()
	}
}

func (s *service) emitToolApprovalDrift(ctx context.Context, sess *acpSession) {

	sess.stateChangeMu.Lock()
	defer sess.stateChangeMu.Unlock()
	current := normalizeACPToolApprovalMode(sess.currentCtrl().ToolApprovalMode())
	if sess.swapToolApprovalMode(current) == current {
		return
	}
	if cfgState, err := s.configStateForSession(ctx, sess); err == nil {
		sess.sink.send(configOptionUpdate{SessionUpdate: "config_option_update", ConfigOptions: cfgState.ConfigOptions})
	}
	sess.saveMetaIfPresent()
}

// sessionLoad resumes a previously-saved session by id: it builds a controller
// (rooted at the requested cwd), seeds it from the on-disk transcript, replays
// the conversation to the client as session/update notifications, and registers
// it for subsequent prompts. A session already live in this process is replayed
// from memory without rebuilding.
func (s *service) sessionLoad(ctx context.Context, raw json.RawMessage) (any, error) {
	var p SessionLoadParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/load: " + err.Error()}
	}
	cfgState, err := s.openExistingSession(ctx, "session/load", p.SessionID, p.Cwd, p.MCPServers, true)
	if err != nil {
		return nil, err
	}
	return afterResponse{
		result: SessionLoadResult{Models: cfgState.Models, Modes: s.sessionModesFor(p.SessionID), ConfigOptions: cfgState.ConfigOptions},
		after:  func() { s.sendAvailableCommands(s.session(p.SessionID)) },
	}, nil
}

// sessionModesFor reports the modes state for a just-opened session. A live
// session keeps its current normal/plan/goal selection, so load/resume must not
// reset a reconnecting client's mode picker to normal.
func (s *service) sessionModesFor(id string) *SessionModeState {
	if sess := s.session(id); sess != nil {
		return sessionModesState(sess.currentModeID())
	}
	return sessionModesState(sessionModeNormal)
}

// sessionResume restores a previously-saved session without replaying its
// conversation history to the client.
func (s *service) sessionResume(ctx context.Context, raw json.RawMessage) (any, error) {
	var p SessionResumeParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/resume: " + err.Error()}
	}
	cfgState, err := s.openExistingSession(ctx, "session/resume", p.SessionID, p.Cwd, p.MCPServers, false)
	if err != nil {
		return nil, err
	}
	return afterResponse{
		result: SessionResumeResult{Models: cfgState.Models, Modes: s.sessionModesFor(p.SessionID), ConfigOptions: cfgState.ConfigOptions},
		after:  func() { s.sendAvailableCommands(s.session(p.SessionID)) },
	}, nil
}
