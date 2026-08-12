package acp

import (
	"context"
	"sort"
	"strings"
	"time"

	"reasonix/internal/control"
	"reasonix/internal/extension/uihub"
)

func (s *service) loadMeta(id string) (acpSessionMeta, bool) {
	dir := s.sessionDir()
	if dir == "" {
		return acpSessionMeta{}, false
	}
	meta, ok, err := loadACPMeta(resolveTranscriptPath(dir, id))
	if err != nil {
		return acpSessionMeta{}, false
	}
	return meta, ok
}

// closeAll tears down every open session (aborting any in-flight turn and
// stopping its MCP subprocesses) when the connection ends.
func (s *service) closeAll() {
	s.mu.Lock()
	sessions := s.sessions
	s.sessions = make(map[string]*acpSession)
	s.mu.Unlock()
	for _, sess := range sessions {
		sess.abortAndWait()
		sess.currentCtrl().Close()
		sess.releaseSessionLease()
	}
}

func (s *acpSession) persistAfterTurn(prompt string) {
	s.mu.Lock()
	if s.deleted {
		s.mu.Unlock()
		return
	}
	ctrl := s.ctrl
	s.mu.Unlock()

	_ = snapshotACPController(s, ctrl)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleted || s.ctrl != ctrl {
		return
	}
	if s.title == "" {
		s.title = previewTitle(prompt)
	}
	s.updatedAt = time.Now().UTC()
	if s.createdAt.IsZero() {
		s.createdAt = s.updatedAt
	}
	if s.transcript != "" && sessionFileExists(s.transcript) {
		_ = saveACPMeta(s.transcript, s.metaLocked())
	}
}

func (s *acpSession) meta() acpSessionMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metaLocked()
}

func (s *acpSession) metaLocked() acpSessionMeta {
	return acpSessionMeta{
		SessionID:         s.id,
		Cwd:               s.cwd,
		Model:             s.model,
		EffortOverride:    cloneStringPtr(s.effortOverride),
		RuntimeProfile:    s.runtimeProfile,
		ToolApprovalMode:  normalizeACPToolApprovalMode(s.toolApprovalMode),
		CollaborationMode: normalizeACPCollaborationMode(s.modeID),
		Title:             s.title,
		CreatedAt:         s.createdAt,
		UpdatedAt:         s.updatedAt,
		Status:            s.status.persisted(),
	}
}

func (s *acpSession) info() SessionInfo {
	meta := s.meta()
	ctrl := s.currentCtrl()
	extra := map[string]any{}
	if n := len(ctrl.History()); n > 0 {
		extra["messageCount"] = n
	}
	if len(extra) == 0 {
		extra = nil
	}
	return meta.info(extra)
}

func (s *service) sendAvailableCommands(sess *acpSession) {
	if sess == nil {
		return
	}
	ctrl := sess.currentCtrl()
	if ctrl == nil {
		return
	}
	cmds := availableCommandsFor(ctrl)
	if len(cmds) == 0 {
		return
	}
	sess.sink.send(availableCommandsUpdate{
		SessionUpdate:     "available_commands_update",
		AvailableCommands: cmds,
	})
}

func availableCommandsFor(ctrl acpController) []AvailableCommand {
	if ctrl == nil {
		return nil
	}
	byName := map[string]AvailableCommand{}
	for _, cmd := range ctrl.Commands() {
		if cmd.Hidden {
			continue
		}
		name := strings.TrimSpace(cmd.Name)
		if name == "" {
			continue
		}
		desc := strings.TrimSpace(cmd.Description)
		if desc == "" {
			desc = "Run the " + name + " command"
		}
		ac := AvailableCommand{Name: name, Description: desc}
		if hint := strings.TrimSpace(cmd.ArgHint); hint != "" {
			ac.Input = &AvailableCommandInput{Hint: hint}
		}
		byName[name] = ac
	}
	for _, sk := range ctrl.SlashSkills() {
		name := strings.TrimSpace(sk.SlashName())
		if name == "" {
			continue
		}
		if _, exists := byName[name]; exists {
			continue
		}
		desc := strings.TrimSpace(sk.Description)
		if desc == "" {
			desc = "Run the " + name + " skill"
		}
		byName[name] = AvailableCommand{
			Name:        name,
			Description: desc,
			Input:       &AvailableCommandInput{Hint: "instructions"},
		}
	}
	if host := ctrl.Host(); host != nil {
		for _, prompt := range host.Prompts() {
			name := strings.TrimSpace(prompt.Name)
			if name == "" {
				continue
			}
			desc := strings.TrimSpace(prompt.Description)
			if desc == "" {
				desc = "Run the " + name + " MCP prompt"
			}
			ac := AvailableCommand{Name: name, Description: desc}
			if len(prompt.Args) > 0 {
				ac.Input = &AvailableCommandInput{Hint: "arguments"}
			}
			byName[name] = ac
		}
	}

	for _, action := range ctrl.ExtensionActions() {
		name := strings.TrimPrefix(strings.TrimSpace(action.Slash), "/")
		if name == "" {
			continue
		}
		if _, exists := byName[name]; exists {
			continue
		}
		desc := strings.TrimSpace(action.Label)
		if desc == "" {
			desc = "Run the " + name + " extension action"
		}
		byName[name] = AvailableCommand{
			Name:        name,
			Description: desc,
			Input:       &AvailableCommandInput{Hint: "arguments"},
		}
	}
	out := make([]AvailableCommand, 0, len(byName))
	for _, cmd := range byName {
		out = append(out, cmd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *service) resolveSlashPrompt(ctx context.Context, sess *acpSession, text string) string {
	line := strings.TrimSpace(text)
	if sess == nil || !strings.HasPrefix(line, "/") {
		return text
	}
	ctrl := sess.currentCtrl()
	if ctrl == nil {
		return text
	}
	if sent, ok := ctrl.CustomCommand(line); ok {
		return sent
	}
	if sent, ok := ctrl.RunSkill(line); ok {
		return sent
	}
	if sent, ok, err := ctrl.MCPPrompt(ctx, line); err == nil && ok {
		return sent
	}
	if sent, ok := invokeExtensionAction(ctx, ctrl, line); ok {
		return sent
	}
	return text
}

// invokeExtensionAction resolves a "/<plugin>:<action> args…" line against the
// handshake-declared extension actions and invokes it — the last resolution
// step in resolveSlashPrompt, after custom commands, skills, and MCP prompts.
// The extension's result message becomes the prompt text. A parse miss, an
// undeclared action, an invocation error, or an empty result all leave the
// line untouched (ok=false), matching how unknown slash commands fall through.
func invokeExtensionAction(ctx context.Context, ctrl acpController, line string) (string, bool) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", false
	}
	pluginID, actionID, ok := uihub.ParseSlashName(fields[0])
	if !ok {
		return "", false
	}
	declared := false
	for _, action := range ctrl.ExtensionActions() {
		if action.PluginID == pluginID && action.ActionID == actionID {
			declared = true
			break
		}
	}
	if !declared {
		return "", false
	}
	message, err := ctrl.InvokeExtensionAction(ctx, fields[0], control.ParseExtensionActionArgs(fields[1:]))
	if err != nil || strings.TrimSpace(message) == "" {
		return "", false
	}
	return message, true
}

func (m acpSessionMeta) info(extra map[string]any) SessionInfo {
	updatedAt := ""
	if !m.UpdatedAt.IsZero() {
		updatedAt = m.UpdatedAt.Format(time.RFC3339Nano)
	}
	return SessionInfo{
		SessionID: m.SessionID,
		Cwd:       m.Cwd,
		Title:     m.Title,
		UpdatedAt: updatedAt,
		Meta:      extra,
	}
}
