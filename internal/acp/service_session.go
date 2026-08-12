package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/plugin"
)

// sessionClose releases an active session. Unknown sessions are accepted as a
// no-op because closing is an idempotent resource cleanup request.
func (s *service) sessionClose(_ context.Context, raw json.RawMessage) (any, error) {
	var p SessionCloseParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/close: " + err.Error()}
	}
	if err := validateSessionID("session/close", p.SessionID); err != nil {
		return nil, err
	}
	if sess := s.takeSession(p.SessionID); sess != nil {
		sess.abortAndWait()
		sess.ctrl.Close()
		sess.releaseSessionLease()
	}
	return SessionCloseResult{}, nil
}

// sessionList returns ACP sessions known to this process or persisted as ACP
// sidecars. It deliberately ignores ordinary CLI timestamp sessions.
func (s *service) sessionList(_ context.Context, raw json.RawMessage) (any, error) {
	var p SessionListParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, &RPCError{Code: ErrInvalidParams, Message: "session/list: " + err.Error()}
		}
	}
	filterCwd := strings.TrimSpace(p.Cwd)
	if filterCwd != "" && !filepath.IsAbs(filterCwd) {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/list: cwd must be an absolute path"}
	}
	if strings.TrimSpace(p.Cursor) != "" {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/list: unsupported cursor"}
	}

	byID := map[string]SessionInfo{}
	if dir := s.sessionDir(); dir != "" {
		metas, err := listACPMetas(dir)
		if err != nil {
			return nil, &RPCError{Code: ErrInternal, Message: "session/list: " + err.Error()}
		}

		best := map[string]acpSessionMeta{}
		for _, meta := range metas {
			cur, ok := best[meta.SessionID]
			if !ok || listMetaBeats(meta, cur) {
				best[meta.SessionID] = meta
			}
		}
		for _, meta := range best {
			info := meta.info(nil)
			if sessionInfoMatchesCwd(info, filterCwd) {
				byID[info.SessionID] = info
			}
		}
	}
	for _, sess := range s.liveSessions() {
		info := sess.info()
		if sessionInfoMatchesCwd(info, filterCwd) {
			byID[info.SessionID] = info
		}
	}

	sessions := make([]SessionInfo, 0, len(byID))
	for _, info := range byID {
		sessions = append(sessions, info)
	}
	sort.Slice(sessions, func(i, j int) bool {
		ti := parseSessionUpdatedAt(sessions[i].UpdatedAt)
		tj := parseSessionUpdatedAt(sessions[j].UpdatedAt)
		if ti.Equal(tj) {
			return sessions[i].SessionID < sessions[j].SessionID
		}
		return ti.After(tj)
	})
	return SessionListResult{Sessions: sessions}, nil
}

// sessionDelete removes a session from future list results. Deleting a missing
// session succeeds silently, matching ACP's idempotent delete guidance.
func (s *service) sessionDelete(_ context.Context, raw json.RawMessage) (any, error) {
	var p SessionDeleteParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &RPCError{Code: ErrInvalidParams, Message: "session/delete: " + err.Error()}
	}
	if err := validateSessionID("session/delete", p.SessionID); err != nil {
		return nil, err
	}

	path := ""
	var destroy control.SessionDestroyHandle
	var delayed bool
	if sess := s.takeSession(p.SessionID); sess != nil {
		sess.deleteAndWait()

		sess.releaseSessionLease()
		path = sess.transcript
		destroy = sess.ctrl.BeginDestroySession(path)
		if result := destroy.Wait(); result.HasTimedOut() {
			if err := agent.MarkCleanupPending(path, "delete"); err != nil {
				go delayedDeleteSessionFiles(path, destroy)
				sess.ctrl.CloseAfterDestroy()
				return nil, &RPCError{Code: ErrInternal, Message: "session/delete: " + err.Error()}
			}
			go delayedDeleteSessionFiles(path, destroy)
			delayed = true
		}
		sess.ctrl.CloseAfterDestroy()
	}
	if path == "" {
		if dir := s.sessionDir(); dir != "" {
			path = resolveTranscriptPath(dir, p.SessionID)
		}
	}
	if path != "" && !delayed {
		if err := deleteSessionFiles(path); err != nil {
			return nil, &RPCError{Code: ErrInternal, Message: "session/delete: " + err.Error()}
		}
		if destroy.Finish != nil {
			destroy.Finish()
		}
	}

	if dir := s.sessionDir(); dir != "" {
		if idPath := transcriptPath(dir, p.SessionID); idPath != path {
			if err := deleteSessionFiles(idPath); err != nil {
				return nil, &RPCError{Code: ErrInternal, Message: "session/delete: " + err.Error()}
			}
		}
	}
	return SessionDeleteResult{}, nil
}

// sessionCancel aborts a session's in-flight turn, if any. It is a notification:
// no reply, and an unknown session is silently ignored.
func (s *service) sessionCancel(_ context.Context, raw json.RawMessage) {
	var p SessionCancelParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return
	}
	if sess := s.session(p.SessionID); sess != nil {
		sess.abort()
	}
}

func (s *service) session(id string) *acpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

func (s *service) takeSession(id string) *acpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	delete(s.sessions, id)
	return sess
}

func (s *service) liveSessions() []*acpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*acpSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

func (s *service) sessionDir() string {
	if p, ok := s.factory.(SessionDirProvider); ok {
		if dir := strings.TrimSpace(p.SessionDir()); dir != "" {
			return dir
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if dir := sess.currentCtrl().SessionDir(); dir != "" {
			return dir
		}
	}
	return ""
}

func (s *service) sessionConfigState(ctx context.Context, p SessionConfigStateParams) (SessionConfigState, error) {
	if provider, ok := s.factory.(SessionConfigStateProvider); ok {
		return provider.SessionConfigState(ctx, p)
	}
	return SessionConfigState{}, nil
}

func (s *service) configStateForSession(ctx context.Context, sess *acpSession) (SessionConfigState, error) {
	state, err := s.sessionConfigState(ctx, sess.configStateParams())
	if err != nil {
		return SessionConfigState{}, err
	}

	state = enrichStateWithExtensionModels(state, sess.currentCtrl().ProviderCatalog())
	return withToolApprovalConfig(state, sess.currentToolApprovalMode()), nil
}

func (s *acpSession) configStateParams() SessionConfigStateParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionConfigStateParams{
		Cwd:            s.cwd,
		Model:          s.model,
		EffortOverride: cloneStringPtr(s.effortOverride),
		RuntimeProfile: s.runtimeProfile,
	}
}

func (s *acpSession) currentToolApprovalMode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return normalizeACPToolApprovalMode(s.toolApprovalMode)
}

func normalizeACPToolApprovalMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case control.ToolApprovalAuto:
		return control.ToolApprovalAuto
	case control.ToolApprovalYolo:
		return control.ToolApprovalYolo
	default:
		return control.ToolApprovalAsk
	}
}

func normalizeACPCollaborationMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case sessionModePlan:
		return sessionModePlan
	case sessionModeGoal:
		return sessionModeGoal
	default:
		return sessionModeNormal
	}
}

func withToolApprovalConfig(state SessionConfigState, mode string) SessionConfigState {
	mode = normalizeACPToolApprovalMode(mode)
	option := SessionConfigOption{
		ID:           "tool_approval",
		Name:         "Tool Approval",
		Category:     "tool_approval",
		Type:         "select",
		CurrentValue: mode,
		Options: []SessionConfigSelectOption{
			{Value: control.ToolApprovalAsk, Name: "Ask", Description: "Ask before permission-gated tool calls"},
			{Value: control.ToolApprovalAuto, Name: "Auto", Description: "Follow configured permission rules without fallback prompts"},
			{Value: control.ToolApprovalYolo, Name: "Yolo", Description: "Approve tool calls except protected decisions"},
		},
	}
	for i := range state.ConfigOptions {
		if normalizeConfigID(state.ConfigOptions[i].ID) == option.ID {
			state.ConfigOptions[i] = option
			return state
		}
	}
	state.ConfigOptions = append(state.ConfigOptions, option)
	return state
}

func findConfigOption(options []SessionConfigOption, id string) (SessionConfigOption, bool) {
	id = normalizeConfigID(id)
	for _, opt := range options {
		if normalizeConfigID(opt.ID) == id {
			return opt, true
		}
	}
	return SessionConfigOption{}, false
}

func normalizeConfigID(id string) string {
	switch strings.TrimSpace(id) {
	case "models":
		return "model"
	case "reasoning_effort", "thought_level":
		return "effort"
	case "profile", "runtime_profile", "token_mode":
		return "work_mode"
	case "approval", "approval_mode", "tool_approval_mode":
		return "tool_approval"
	default:
		return strings.TrimSpace(id)
	}
}

func configOptionHasValue(option SessionConfigOption, value string) bool {
	for _, opt := range option.Options {
		if opt.Value == value {
			return true
		}
	}
	return false
}

func configOptionCategory(option SessionConfigOption) string {
	if option.Category != "" {
		return option.Category
	}
	switch normalizeConfigID(option.ID) {
	case "model":
		return "model"
	case "effort":
		return "thought_level"
	case "work_mode":
		return "work_mode"
	case "tool_approval":
		return "tool_approval"
	default:
		return ""
	}
}

func cloneStringPtr(p *string) *string {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}

func clonePluginSpecs(in []plugin.Spec) []plugin.Spec {
	if len(in) == 0 {
		return nil
	}
	out := make([]plugin.Spec, len(in))
	copy(out, in)
	return out
}

func (s *service) resolveSessionCwd(cwd, sessionID string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd != "" {
		if !filepath.IsAbs(cwd) {
			return "", fmt.Errorf("cwd must be an absolute path")
		}
		return filepath.Clean(cwd), nil
	}
	if sessionID != "" {
		if meta, ok := s.loadMeta(sessionID); ok && meta.Cwd != "" {
			if !filepath.IsAbs(meta.Cwd) {
				return "", fmt.Errorf("stored cwd must be an absolute path")
			}
			return filepath.Clean(meta.Cwd), nil
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	return wd, nil
}
