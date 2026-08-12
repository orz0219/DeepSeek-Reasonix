package bot

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/secrets"
)

func (gw *BotGateway) inputTextWithMedia(ctx context.Context, adapter Adapter, msg InboundMessage, state *sessionState) string {
	input := msg.Text
	if len(msg.MediaURLs) == 0 && len(msg.Media) == 0 {
		return input
	}
	workspaceRoot := ""
	if state != nil && state.ctrl != nil {
		workspaceRoot = state.ctrl.WorkspaceRoot()
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		_, workspaceRoot, _ = gw.sessionOptionsForMessage(msg)
	}
	refs, errs := saveInboundMedia(ctx, workspaceRoot, msg.MediaURLs)
	itemRefs, fallbacks, itemErrs := saveInboundMediaItems(ctx, workspaceRoot, msg.Media)
	refs = append(refs, itemRefs...)
	errs = append(errs, itemErrs...)
	if len(errs) > 0 {
		gw.logger.Warn("bot media attachment failed", "platform", msg.Platform, "chat", hashID(msg.ChatID), "errors", len(errs))
		_ = gw.sendText(ctx, adapter, msg, fmt.Sprintf("有 %d 个附件保存失败；我会先处理可用内容。", len(errs)))
	}
	return appendMediaRefs(appendMediaFallbacks(input, fallbacks), refs)
}

func (gw *BotGateway) getOrCreateSession(ctx context.Context, key string, msg InboundMessage) *sessionState {
	profile := gw.sessionProfileForMessage(msg)
	var stale *sessionState
	gw.mu.Lock()
	if state, ok := gw.controllers[key]; ok {
		if !sessionStateMatchesRuntime(state, profile) {
			if botSessionHasActiveWork(state) {
				gw.mu.Unlock()
				safeBotSetToolApprovalMode(state.ctrl, profile.toolApprovalMode)
				gw.logger.Warn("bot session runtime change deferred while work is active", "platform", msg.Platform, "chat_type", msg.ChatType, "chat", hashID(msg.ChatID), "session", key[:8])
				return state
			}
			delete(gw.controllers, key)
			stale = state
			gw.mu.Unlock()
			gw.closeSessionState(stale)
			gw.logger.Warn("bot session runtime changed; rebuilding", "platform", msg.Platform, "chat_type", msg.ChatType, "chat", hashID(msg.ChatID), "session", key[:8], "old_workspace_set", strings.TrimSpace(stale.workspaceRoot) != "", "new_workspace_set", profile.workspaceRoot != "", "old_model", stale.model, "new_model", profile.model)
		} else {
			updateSessionStateRuntime(state, msg, profile)
			gw.mu.Unlock()
			safeBotSetToolApprovalMode(state.ctrl, profile.toolApprovalMode)
			gw.logger.Info("bot session reused", "platform", msg.Platform, "chat_type", msg.ChatType, "chat", hashID(msg.ChatID), "session", key[:8])
			return state
		}
	} else {
		gw.mu.Unlock()
	}

	sessionSink := &sessionEventSink{}
	leases := control.NewSessionLeaseKeeper()
	state := &sessionState{
		sink:             sessionSink,
		leases:           leases,
		platform:         msg.Platform,
		connectionID:     strings.TrimSpace(msg.ConnectionID),
		model:            profile.model,
		workspaceRoot:    profile.workspaceRoot,
		toolApprovalMode: profile.toolApprovalMode,
		sessionPath:      profile.sessionPath,
		pendingAsks:      make(map[string][]event.AskQuestion),
		createdAt:        time.Now(),
		lastActive:       time.Now(),
	}
	gw.logger.Info("bot session creating", "platform", msg.Platform, "chat_type", msg.ChatType, "chat", hashID(msg.ChatID), "session", key[:8], "model", profile.model, "workspace_set", profile.workspaceRoot != "", "tool_approval_mode", profile.toolApprovalMode)
	ctrl, err := boot.Build(ctx, boot.Options{
		Model:              profile.model,
		MaxSteps:           gw.cfg.MaxSteps,
		MaxStepsKey:        "bot.max_steps",
		RequireKey:         true,
		Sink:               sessionSink,
		StatsSource:        "bot",
		WorkspaceRoot:      profile.workspaceRoot,
		SessionDir:         botSessionDir(profile.workspaceRoot),
		ApprovalTimeout:    gw.approvalTimeout(),
		OnSessionRecovered: gw.botSessionRecoveredHandler(key, msg, state),
	})
	if err != nil {
		leases.Release()
		gw.logger.Error("build controller failed", "err", secrets.RedactError(err))
		return nil
	}
	state.ctrl = ctrl
	if profile.sessionPath != "" {

		degrade := func(reason string, err error) bool {
			if !profile.sessionPathOptional {
				return false
			}
			gw.logger.Warn("mapped bot session unavailable; starting fresh", "reason", reason, "session_path", profile.sessionPath, "err", err)
			profile.sessionPath = ""
			state.sessionPath = ""
			state.mappingDegraded = true
			return true
		}
		if err := leases.Rebind(profile.sessionPath); err != nil {
			if !degrade("lease held elsewhere", err) {
				ctrl.Close()
				leases.Release()
				gw.logger.Error("attached bot session is in use", "err", control.SessionInUseMessage(err))
				return nil
			}
		} else if loaded, err := agent.LoadSession(profile.sessionPath); err != nil {
			if !degrade("load failed", err) {
				ctrl.Close()
				leases.Release()
				if os.IsNotExist(err) {
					gw.logger.Error("attached bot session missing", "session_path", profile.sessionPath)
				} else {
					gw.logger.Error("attached bot session load failed", "session_path", profile.sessionPath, "err", err)
				}
				return nil
			}
		} else {
			ctrl.Resume(loaded, profile.sessionPath)
		}
	}
	ctrl.EnableInteractiveApproval()
	ctrl.SetToolApprovalMode(profile.toolApprovalMode)
	ctrl.EnsureSessionPath()
	if err := rebindBotControllerWriteAuthority(leases, ctrl); err != nil {
		ctrl.Close()
		leases.Release()
		gw.logger.Error("bot session lease failed", "err", control.SessionInUseMessage(err))
		return nil
	}

	var replace *sessionState
	gw.mu.Lock()

	if existing, ok := gw.controllers[key]; ok {
		if sessionStateMatchesRuntime(existing, profile) {
			updateSessionStateRuntime(existing, msg, profile)
			gw.mu.Unlock()
			ctrl.Close()
			leases.Release()
			safeBotSetToolApprovalMode(existing.ctrl, profile.toolApprovalMode)
			gw.logger.Info("bot session built concurrently; discarding duplicate", "platform", msg.Platform, "chat", hashID(msg.ChatID), "session", key[:8])
			return existing
		}
		delete(gw.controllers, key)
		replace = existing
	}
	gw.controllers[key] = state
	gw.mu.Unlock()
	gw.closeSessionState(replace)

	gw.logger.Info("bot session created", "platform", msg.Platform, "chat_type", msg.ChatType, "chat", hashID(msg.ChatID), "session", key[:8])
	return state
}

func updateSessionStateRuntime(state *sessionState, msg InboundMessage, profile sessionRuntimeProfile) {
	if state == nil {
		return
	}
	if state.connectionID == "" {
		state.connectionID = strings.TrimSpace(msg.ConnectionID)
	}
	if state.platform == "" {
		state.platform = msg.Platform
	}
	state.model = profile.model
	state.workspaceRoot = profile.workspaceRoot
	state.toolApprovalMode = profile.toolApprovalMode
	state.sessionPath = profile.sessionPath
	state.lastActive = time.Now()
}

func (gw *BotGateway) sessionProfileForMessage(msg InboundMessage) sessionRuntimeProfile {
	model, workspaceRoot, toolApprovalMode := gw.sessionOptionsForMessage(msg)
	var sessionPath string
	sessionPathOptional := false
	if override, ok := gw.sessionRuntimeOverrideForMessage(msg); ok {
		sessionPath = override.sessionPath
	}

	if sessionPath == "" {
		if mapped := gw.sessionMappingPathForMessage(msg); mapped != "" {
			sessionPath = mapped
			sessionPathOptional = true
		}
	}
	return sessionRuntimeProfile{
		model:               strings.TrimSpace(model),
		workspaceRoot:       strings.TrimSpace(workspaceRoot),
		toolApprovalMode:    normalizeBotToolApprovalMode(toolApprovalMode),
		sessionPath:         canonicalBotPath(sessionPath),
		sessionPathOptional: sessionPathOptional,
	}
}

// sessionMappingPathForMessage resolves the persisted session_mappings entry
// for a message to an existing session file. Only bindings that resolve to a
// present, readable file participate — a moved or deleted target quietly
// degrades to normal session creation rather than blocking the chat.
func (gw *BotGateway) sessionMappingPathForMessage(msg InboundMessage) string {
	gw.mu.Lock()
	var mappings []SessionMapping
	if msg.ConnectionID != "" {
		if channel, ok := gw.cfg.ConnectionChannels[msg.ConnectionID]; ok {
			mappings = channel.SessionMappings
		}
	}
	if len(mappings) == 0 {
		if channel, ok := gw.cfg.Channels[msg.Platform]; ok {
			mappings = channel.SessionMappings
		}
	}
	gw.mu.Unlock()
	mapping, ok := matchingSessionMapping(mappings, msg)
	if !ok {
		return ""
	}
	path := botSessionPathFromTarget(mapping.SessionID)
	if path == "" {
		path = botSessionPathFromTarget(mapping.SessionSource)
	}
	if path == "" {
		return ""
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return ""
	}
	return path
}

func sessionStateMatchesRuntime(state *sessionState, profile sessionRuntimeProfile) bool {
	if state == nil || state.ctrl == nil {
		return false
	}
	if stateModel := strings.TrimSpace(state.model); stateModel != "" && profile.model != "" && stateModel != profile.model {
		return false
	}
	stateRoot := strings.TrimSpace(state.workspaceRoot)
	wantRoot := strings.TrimSpace(profile.workspaceRoot)
	if stateRoot == "" {
		root, ok := safeBotControllerWorkspaceRoot(state.ctrl)
		if ok {
			stateRoot = strings.TrimSpace(root)
		} else if wantRoot != "" {
			return false
		}
	}
	if stateRoot != wantRoot {
		return false
	}

	if profile.sessionPathOptional && state.mappingDegraded {
		return true
	}
	if canonicalBotPath(state.sessionPath) != canonicalBotPath(profile.sessionPath) {
		return false
	}
	if profile.sessionPath != "" && canonicalBotPath(state.ctrl.SessionPath()) != canonicalBotPath(profile.sessionPath) {
		return false
	}
	return true
}

func safeBotControllerWorkspaceRoot(ctrl botController) (root string, ok bool) {
	if ctrl == nil {
		return "", false
	}
	defer func() {
		if recover() != nil {
			root = ""
			ok = false
		}
	}()
	return ctrl.WorkspaceRoot(), true
}

func safeBotSetToolApprovalMode(ctrl botController, mode string) {
	if ctrl == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	ctrl.SetToolApprovalMode(mode)
}

// defaultBotApprovalTimeout caps how long a bot session waits for a remote
// user's approval/ask reply before treating it as denied, so an abandoned
// prompt (or a dropped IM event) can't leave the session wedged forever
// (#4626, #4402). 30 minutes is generous for a human reply yet bounded.
const defaultBotApprovalTimeout = 30 * time.Minute

// approvalTimeout resolves the configured bot approval wait: zero uses the
// bounded default; a negative value opts out (wait indefinitely).
func (gw *BotGateway) approvalTimeout() time.Duration {
	switch {
	case gw.cfg.ApprovalTimeout < 0:
		return 0
	case gw.cfg.ApprovalTimeout == 0:
		return defaultBotApprovalTimeout
	default:
		return gw.cfg.ApprovalTimeout
	}
}

func botSessionDir(workspaceRoot string) string {
	if strings.TrimSpace(workspaceRoot) == "" {
		return config.SessionDir()
	}
	if dir := config.ProjectSessionDir(workspaceRoot); dir != "" {
		return dir
	}
	return config.SessionDir()
}

func (gw *BotGateway) rememberSessionReady(msg InboundMessage, ctrl botController) {
	if gw.cfg.OnSessionReady == nil || ctrl == nil {
		return
	}
	gw.rememberSessionPath(msg, ctrl.SessionPath())
}

func (gw *BotGateway) rememberSessionPath(msg InboundMessage, sessionPath string) {
	if gw.cfg.OnSessionReady == nil {
		return
	}
	sessionID := botSessionTarget(sessionPath)
	if sessionID == "" {
		return
	}
	if err := gw.cfg.OnSessionReady(msg, sessionID); err != nil {
		gw.logger.Warn("remember bot session failed", "platform", msg.Platform, "connection", msg.ConnectionID, "err", err)
	}
}

// botSessionRecoveredHandler keeps the controller path, its writer lease, and
// the remote-to-session mapping on the same recovery generation. The lease
// handoff runs first and is failure-atomic: if the recovery path is already
// owned, the controller stays on the original path and the old lease remains
// held. Mapping updates are limited to this exact sessionState so a late
// callback from a retired controller cannot overwrite its replacement.
func (gw *BotGateway) botSessionRecoveredHandler(key string, msg InboundMessage, state *sessionState) func(control.SessionRecoveryInfo) error {
	return func(info control.SessionRecoveryInfo) error {
		if state == nil || state.leases == nil {
			return nil
		}

		state.lifecycleMu.Lock()
		defer state.lifecycleMu.Unlock()
		if state.retired {
			return errBotSessionRetired
		}
		if err := state.leases.HandleSessionRecovered(info); err != nil {
			return err
		}

		originalPath := canonicalBotPath(info.OriginalPath)
		recoveryPath := canonicalBotPath(info.RecoveryPath)
		live := false
		gw.mu.Lock()
		if gw.controllers[key] == state {
			live = true
			if canonicalBotPath(state.sessionPath) == originalPath {
				state.sessionPath = recoveryPath
			}
			if override, ok := gw.sessionOverrides[key]; ok && canonicalBotPath(override.sessionPath) == originalPath {
				override.sessionPath = recoveryPath
				gw.sessionOverrides[key] = override
			}
		}
		gw.mu.Unlock()

		if live {
			gw.rememberSessionPath(msg, recoveryPath)
		}
		return nil
	}
}

func botSessionTarget(sessionPath string) string {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" {
		return ""
	}
	return "path:" + sessionPath
}

func (gw *BotGateway) sessionOptionsForMessage(msg InboundMessage) (model string, workspaceRoot string, toolApprovalMode string) {

	gw.mu.Lock()
	model = gw.cfg.Model
	workspaceRoot = gw.cfg.WorkspaceRoot
	toolApprovalMode = normalizeBotToolApprovalMode(gw.cfg.ToolApprovalMode)
	var connChannel ChannelConfig
	connOK := false
	if msg.ConnectionID != "" {
		connChannel, connOK = gw.cfg.ConnectionChannels[msg.ConnectionID]
	}
	platChannel, platOK := gw.cfg.Channels[msg.Platform]
	gw.mu.Unlock()

	var mappings []SessionMapping
	if connOK {
		applyBotChannelOptions(connChannel, &model, &workspaceRoot, &toolApprovalMode)
		mappings = connChannel.SessionMappings
		if mapping, ok := matchingSessionMapping(mappings, msg); ok {
			workspaceRoot = workspaceRootForSessionMapping(mapping, workspaceRoot)
		}
		model, workspaceRoot, toolApprovalMode = gw.applyRouteOptions(msg, model, workspaceRoot, toolApprovalMode)
		model, workspaceRoot, toolApprovalMode = gw.applyRuntimeOverrideOptions(msg, model, workspaceRoot, toolApprovalMode)
		return model, workspaceRoot, toolApprovalMode
	}
	if platOK {
		applyBotChannelOptions(platChannel, &model, &workspaceRoot, &toolApprovalMode)
		mappings = platChannel.SessionMappings
	}
	if mapping, ok := matchingSessionMapping(mappings, msg); ok {
		workspaceRoot = workspaceRootForSessionMapping(mapping, workspaceRoot)
	}
	model, workspaceRoot, toolApprovalMode = gw.applyRouteOptions(msg, model, workspaceRoot, toolApprovalMode)
	model, workspaceRoot, toolApprovalMode = gw.applyRuntimeOverrideOptions(msg, model, workspaceRoot, toolApprovalMode)
	return model, workspaceRoot, toolApprovalMode
}

func (gw *BotGateway) applyRuntimeOverrideOptions(msg InboundMessage, model, workspaceRoot, toolApprovalMode string) (string, string, string) {
	if override, ok := gw.sessionRuntimeOverrideForMessage(msg); ok {
		applyBotChannelOptions(override.channel, &model, &workspaceRoot, &toolApprovalMode)
	}
	return model, workspaceRoot, toolApprovalMode
}

func (gw *BotGateway) applyRouteOptions(msg InboundMessage, model, workspaceRoot, toolApprovalMode string) (string, string, string) {
	for _, route := range gw.cfg.Routes {
		if routeMatchesMessage(route, msg) {
			applyBotChannelOptions(route.Channel, &model, &workspaceRoot, &toolApprovalMode)
			break
		}
	}
	return model, workspaceRoot, toolApprovalMode
}

func applyBotChannelOptions(channel ChannelConfig, model *string, workspaceRoot *string, toolApprovalMode *string) {
	if value := strings.TrimSpace(channel.Model); value != "" {
		*model = value
	}
	if value := strings.TrimSpace(channel.WorkspaceRoot); value != "" {
		*workspaceRoot = value
	}
	if value := normalizeOptionalBotToolApprovalMode(channel.ToolApprovalMode); value != "" {
		*toolApprovalMode = value
	}
}

func matchingSessionMapping(mappings []SessionMapping, msg InboundMessage) (SessionMapping, bool) {
	for i := range mappings {
		if sessionMappingMatches(mappings[i], msg) {
			return mappings[i], true
		}
	}
	return SessionMapping{}, false
}

func sessionMappingMatches(mapping SessionMapping, msg InboundMessage) bool {
	if strings.TrimSpace(mapping.RemoteID) != strings.TrimSpace(msg.ChatID) {
		return false
	}
	chatType, userID, threadID := sessionMappingIdentity(msg)
	mappingChatType := strings.TrimSpace(mapping.ChatType)
	if mappingChatType == "" {
		return chatType == ""
	}
	if mappingChatType != chatType {
		return false
	}
	if strings.TrimSpace(mapping.UserID) != userID {
		return false
	}
	return strings.TrimSpace(mapping.ThreadID) == threadID
}

func sessionMappingIdentity(msg InboundMessage) (chatType string, userID string, threadID string) {
	switch msg.ChatType {
	case ChatGroup, ChatGuild:
		chatType = string(msg.ChatType)
		userID = strings.TrimSpace(msg.UserID)
	case ChatThread:
		chatType = string(msg.ChatType)
		threadID = strings.TrimSpace(msg.ThreadID)
		if threadID == "" {
			threadID = strings.TrimSpace(msg.ChatID)
		}
	}
	return chatType, userID, threadID
}

func workspaceRootForSessionMapping(mapping SessionMapping, fallback string) string {
	if root := strings.TrimSpace(mapping.WorkspaceRoot); root != "" {
		return root
	}
	if strings.EqualFold(strings.TrimSpace(mapping.Scope), "global") {
		return ""
	}
	return fallback
}

func routeMatchesMessage(route RouteConfig, msg InboundMessage) bool {
	if value := strings.TrimSpace(route.ConnectionID); value != "" && value != strings.TrimSpace(msg.ConnectionID) {
		return false
	}
	if route.Platform != "" && route.Platform != msg.Platform {
		return false
	}
	if route.ChatType != "" && route.ChatType != msg.ChatType {
		return false
	}
	if value := strings.TrimSpace(route.ChatID); value != "" && value != strings.TrimSpace(msg.ChatID) {
		return false
	}
	if value := strings.TrimSpace(route.UserID); value != "" && value != strings.TrimSpace(msg.UserID) {
		return false
	}
	if value := strings.TrimSpace(route.ThreadID); value != "" && value != strings.TrimSpace(msg.ThreadID) {
		return false
	}
	return true
}

func normalizeBotToolApprovalMode(mode string) string {
	if value := normalizeOptionalBotToolApprovalMode(mode); value != "" {
		return value
	}
	return control.ToolApprovalAsk
}

func normalizeOptionalBotToolApprovalMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case control.ToolApprovalAsk:
		return control.ToolApprovalAsk
	case control.ToolApprovalAuto:
		return control.ToolApprovalAuto
	case control.ToolApprovalYolo, "full", "full-access", "bypass":
		return control.ToolApprovalYolo
	default:
		return ""
	}
}
