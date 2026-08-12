package acp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"reasonix/internal/event"
	"reasonix/internal/provider"
)

func (s *service) sessionRuntimeState(ctx context.Context, p SessionRuntimeStateParams) (SessionRuntimeState, error) {
	if provider, ok := s.factory.(SessionRuntimeStateProvider); ok {
		state, err := provider.SessionRuntimeState(ctx, p)
		if err != nil {
			return SessionRuntimeState{}, err
		}
		if isLightRuntimeProfile(p.RuntimeProfile) {
			state.PlannerMode = "off"
		}
		if strings.TrimSpace(state.PlannerMode) == "" {
			state.PlannerMode = "on"
		}
		if state.Sandbox.WriteRoots == nil {
			state.Sandbox.WriteRoots = []string{}
		}
		return state, nil
	}
	state := defaultSessionRuntimeState(p.Cwd)
	if isLightRuntimeProfile(p.RuntimeProfile) {
		state.PlannerMode = "off"
	}
	return state, nil
}

func isLightRuntimeProfile(profile string) bool {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "economy", "light", "lite", "eco":
		return true
	default:
		return false
	}
}

func (s *service) bindStatusEvents(sess *acpSession) {
	if sess == nil || sess.sink == nil {
		return
	}
	if sess.status == nil {
		sess.status = newStatusTelemetry()
	}
	sess.sink.bindStatus(func(e event.Event) {
		eventName, publish := sess.status.onEvent(e)
		if publish {
			s.publishStatus(sess, eventName)
		}
	})
}

func (s *service) sessionStatus(_ context.Context, raw json.RawMessage) (any, error) {
	var p SessionStatusParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: sessionStatusMethod + ": " + err.Error()}
	}
	sess := s.session(p.SessionID)
	if sess == nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: sessionStatusMethod + ": unknown session " + p.SessionID}
	}
	return sess.statusSnapshot(), nil
}

func (s *service) publishStatus(sess *acpSession, eventName string) {
	if sess == nil {
		return
	}
	status := sess.statusSnapshot()
	_ = s.conn.Notify(sessionStatusUpdateMethod, ReasonixStatusUpdate{
		SchemaVersion: reasonixStatusSchemaVersion,
		Sequence:      status.Sequence,
		SessionID:     status.SessionID,
		Event:         eventName,
		Status:        status,
	})
}

func (s *acpSession) statusSnapshot() ReasonixSessionStatus {
	s.mu.Lock()
	id := s.id
	ctrl := s.ctrl
	model := s.model
	effort := cloneStringPtr(s.effortOverride)
	workMode := s.runtimeProfile
	mode := s.modeID
	runtimeState := s.runtimeState
	telemetry := s.status
	s.mu.Unlock()

	if telemetry == nil {
		telemetry = newStatusTelemetry()
	}
	t := telemetry.snapshot()
	goalStatus := "none"
	goalObjective := ""
	var goalRuntime *ReasonixGoalRuntime
	if ctrl != nil {
		goalStatus = normalizeGoalStatus(ctrl.GoalStatus())
		goalObjective = clipStatusText(ctrl.Goal(), 16_384)
		if strings.TrimSpace(goalObjective) != "" {
			rt := ctrl.GoalRuntime()
			goalRuntime = &ReasonixGoalRuntime{
				TurnsUsed:        rt.TurnsUsed,
				TurnsLimit:       rt.TurnsLimit,
				TokensUsed:       rt.TokensUsed,
				RequestsUsed:     rt.RequestsUsed,
				WorkDurationMs:   rt.WorkDurationMs,
				TokensLimit:      rt.TokensLimit,
				NoProgressTurns:  rt.NoProgressTurns,
				NoProgressLimit:  rt.NoProgressLimit,
				LastReason:       rt.LastReason,
				StopCause:        rt.StopCause,
				BudgetExtensions: rt.BudgetExtensions,
			}
		}
	}
	if t.goalOverride != "" {
		goalStatus = t.goalOverride
	}
	mode = normalizeACPCollaborationMode(mode)
	workMode = strings.ToLower(strings.TrimSpace(workMode))
	switch workMode {
	case "economy", "light", "delivery":
		if workMode == "light" {
			workMode = "economy"
		}
	default:
		workMode = "balanced"
	}

	if isLightRuntimeProfile(workMode) {
		runtimeState.PlannerMode = "off"
	} else if runtimeState.PlannerMode != "off" {
		runtimeState.PlannerMode = "on"
	}
	if runtimeState.Sandbox.WriteRoots == nil {
		runtimeState.Sandbox.WriteRoots = []string{}
	}
	phase := strings.TrimSpace(t.phase)
	if phase == "" {
		phase = "idle"
	}
	state := t.state
	if state != "running" {
		state = "idle"
	}
	return ReasonixSessionStatus{
		SchemaVersion: reasonixStatusSchemaVersion,
		Sequence:      t.sequence,
		SessionID:     id,
		State:         state,
		Model:         strings.TrimSpace(model),
		Effort:        normalizeStatusEffort(effort),
		Mode:          mode,
		WorkMode:      workMode,
		PlannerMode:   runtimeState.PlannerMode,
		Goal: ReasonixStatusGoal{
			Status:    goalStatus,
			Objective: goalObjective,
			Runtime:   goalRuntime,
		},
		Phase:          phase,
		TurnOutcome:    t.turnOutcome,
		FinalReadiness: t.finalReadiness,
		Sandbox:        runtimeState.Sandbox,
		Usage: ReasonixStatusUsage{
			Turn:       t.turnUsage,
			Cumulative: t.cumulative,
		},
	}
}

func finalAssistantSummary(ctrl acpController) string {
	if ctrl == nil {
		return ""
	}
	history := ctrl.History()
	for _, v := range slices.Backward(history) {
		if v.Role == provider.RoleAssistant && strings.TrimSpace(v.Content) != "" {
			return v.Content
		}
	}
	return ""
}
