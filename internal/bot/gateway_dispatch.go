package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"reasonix/internal/control"
	"reasonix/internal/sessioninbox"
)

func (gw *BotGateway) dispatchLoop(ctx context.Context, binding AdapterBinding) {
	for {
		select {
		case <-ctx.Done():
			gw.markAdapterClosed(binding)
			return
		case msg, ok := <-binding.Adapter.Messages():
			if !ok {
				gw.markAdapterClosed(binding)
				return
			}
			gw.markAdapterMessage(binding)
			gw.handleMessage(ctx, binding, msg)
		}
	}
}

func (gw *BotGateway) handleMessage(ctx context.Context, binding AdapterBinding, msg InboundMessage) {
	msg.Platform = binding.Platform
	if msg.ConnectionID == "" {
		msg.ConnectionID = binding.ID
	}
	if msg.Domain == "" {
		msg.Domain = binding.Domain
	}
	if gw.isSelfMessage(msg) {
		gw.logger.Debug("bot ignored self message", "platform", binding.Platform, "connection", msg.ConnectionID, "chat", hashID(msg.ChatID), "message", hashID(msg.MessageID), "user", hashID(msg.UserID))
		return
	}
	src := msg.Session()
	key := BuildSessionKey(src)
	logFields := []any{
		"platform", binding.Platform,
		"connection", msg.ConnectionID,
		"domain", msg.Domain,
		"chat_type", msg.ChatType,
		"chat", hashID(msg.ChatID),
		"user", hashID(msg.UserID),
		"operator", hashID(msg.OperatorID),
		"thread", hashID(msg.ThreadID),
		"message", hashID(msg.MessageID),
		"text_chars", len([]rune(msg.Text)),
		"session", key[:8],
	}
	gw.logger.Info("bot inbound message", logFields...)

	if !gw.checkAllowlist(binding.Platform, msg) {
		gw.logger.Info("user not in allowlist", "platform", binding.Platform, "connection", msg.ConnectionID, "user", hashID(msg.UserID))
		if gw.offerPairing(ctx, binding.Adapter, msg) {
			return
		}
		_ = gw.sendText(ctx, binding.Adapter, msg, "抱歉，您没有使用此 bot 的权限。")
		return
	}
	if gw.cfg.OnInbound != nil {
		gw.cfg.OnInbound(msg)
	}

	if normalized, ok := gw.normalizeApprovalShortcut(key, msg.Text); ok {
		msg.Text = normalized
	} else if normalized, ok := gw.normalizeAskShortcut(key, msg.Text); ok {
		msg.Text = normalized
	} else if _, ok := decisionShortcutCommand(msg.Text); ok && gw.sessions.IsActive(key) {
		_ = gw.sendText(ctx, binding.Adapter, msg, "没有找到可匹配的待处理操作。请重新触发一次操作后回复编号，或按消息中的 ID 使用 /approve、/deny 或 /answer。")
		return
	}

	if IsSlashBypass(msg.Text) {
		gw.logger.Info("bot slash command", logFields...)
		gw.handleSlashCommand(ctx, binding.Adapter, key, msg)
		return
	}

	if gw.divertToDesktopTakeover(ctx, binding.Adapter, msg) {
		gw.logger.Info("bot message diverted to desktop takeover", logFields...)
		return
	}

	cleanup := gw.addPendingReaction(ctx, binding.Platform, binding.Adapter, msg)

	queueMode := gw.queueMode(key, msg)
	warnDeprecatedQueueDrop(gw.cfg.QueueDrop)
	if gw.sessions.IsActive(key) {

		if IsSlashBypass(msg.Text) {

		} else {
			switch queueMode {
			case QueueModeSteer:
				if rec, ok := gw.steerActiveSessionDurable(ctx, binding.Adapter, key, msg); ok {
					gw.logger.Info("bot message steered into active turn", "session", key[:8], "item", rec.ItemID)
					if cleanup != nil {
						cleanup()
					}
					_ = gw.sendText(ctx, binding.Adapter, msg, formatQueuedReceipt(rec)+"（已并入当前任务）")
					return
				}
			case QueueModeInterrupt:
				gw.cancelActiveSession(key)
				runReactionCleanups(gw.takeReactionCleanups(key))
				rec, err := gw.interruptActiveSessionDurable(ctx, binding.Adapter, key, msg)
				gw.storeReactionCleanup(key, cleanup)
				if err != nil {
					gw.logger.Warn("bot interrupt enqueue failed", "session", key[:8], "err", err)
					_ = gw.sendText(ctx, binding.Adapter, msg, "排队失败："+err.Error())
					return
				}
				gw.logger.Info("bot active turn interrupted; newest message durable-queued", "session", key[:8], "item", rec.ItemID)
				_ = gw.sendText(ctx, binding.Adapter, msg, "已停止当前任务。"+formatQueuedReceipt(rec))
				return
			case QueueModeCollect:
				if rec, err := gw.collectActiveSessionDurable(ctx, binding.Adapter, key, msg); err == nil {
					gw.storeReactionCleanup(key, cleanup)
					_ = gw.sendText(ctx, binding.Adapter, msg, formatQueuedReceipt(rec))
					return
				} else if errors.Is(err, sessioninbox.ErrCapacityItems) || errors.Is(err, sessioninbox.ErrCapacityBytes) || errors.Is(err, sessioninbox.ErrItemTooLarge) {
					if cleanup != nil {
						cleanup()
					}
					_ = gw.sendText(ctx, binding.Adapter, msg, "当前会话排队已满，请稍后再发，或使用 /queue pause 后清理。")
					return
				}
			default:
				if rec, err := gw.followupActiveSessionDurable(ctx, binding.Adapter, key, msg); err == nil {
					gw.storeReactionCleanup(key, cleanup)
					_ = gw.sendText(ctx, binding.Adapter, msg, formatQueuedReceipt(rec))
					return
				} else if errors.Is(err, sessioninbox.ErrCapacityItems) || errors.Is(err, sessioninbox.ErrCapacityBytes) || errors.Is(err, sessioninbox.ErrItemTooLarge) {
					if cleanup != nil {
						cleanup()
					}
					_ = gw.sendText(ctx, binding.Adapter, msg, "当前会话排队已满，请稍后再发。")
					return
				}
			}
		}
	}

	result := gw.sessions.TryAcquireWithQueue(key, msg, QueueOptions{
		Mode: QueueModeFollowup,
		Cap:  sessioninbox.DefaultMaxItems,
		Drop: QueueDropNew,
	})
	if result.Rejected {
		gw.logger.Warn("bot queue rejected message", "session", key[:8], "pending", result.Pending, "mode", result.Mode)
		if cleanup != nil {
			cleanup()
		}
		_ = gw.sendText(ctx, binding.Adapter, msg, "当前会话排队已满，请稍后再发，或使用 /queue 管理队列。")
		return
	}
	gw.dispatchQueueResult(ctx, binding.Adapter, key, msg, cleanup, result)
}

func (gw *BotGateway) queueMode(key string, msg InboundMessage) string {
	return gw.sessions.QueueMode(key, gw.cfg.QueueMode)
}

func (gw *BotGateway) sessionAPI(key string) control.SessionAPI {
	gw.mu.Lock()
	state, ok := gw.controllers[key]
	gw.mu.Unlock()
	if !ok || state == nil || state.ctrl == nil {
		return nil
	}
	if api, ok := state.ctrl.(control.SessionAPI); ok {
		return api
	}
	return nil
}

func (gw *BotGateway) steerActiveSessionDurable(ctx context.Context, adapter Adapter, key string, msg InboundMessage) (sessioninbox.InboxReceipt, bool) {
	text := strings.TrimSpace(msg.Text)
	if text == "" && len(msg.MediaURLs) == 0 && len(msg.Media) == 0 {
		return sessioninbox.InboxReceipt{}, false
	}
	gw.mu.Lock()
	state, ok := gw.controllers[key]
	gw.mu.Unlock()
	if !ok || state.ctrl == nil {
		return sessioninbox.InboxReceipt{}, false
	}
	msg = gw.prepareDurableInboxMessage(ctx, adapter, msg, state)
	text = msg.Text
	if strings.TrimSpace(text) == "" {
		return sessioninbox.InboxReceipt{}, false
	}
	msg.Text = text
	api, ok := state.ctrl.(control.SessionAPI)
	if !ok {

		if steerer, ok := state.ctrl.(interface{ TrySteer(string) bool }); ok && steerer.TrySteer(text) {
			return sessioninbox.InboxReceipt{Disposition: sessioninbox.DispositionSteerAccepted}, true
		}
		return sessioninbox.InboxReceipt{}, false
	}
	rec, err := enqueueViaInbox(api, msg, sessioninbox.IntentSteer)
	if err != nil {
		return sessioninbox.InboxReceipt{}, false
	}
	return rec, true
}

func (gw *BotGateway) cancelActiveSession(key string) {
	// state.cancel is rewritten under gw.mu on every turn (runTurn), so copy it
	// inside the lock and invoke it outside.
	var cancel context.CancelFunc
	gw.mu.Lock()
	state, ok := gw.controllers[key]
	if ok && state != nil {
		cancel = state.cancel
	}
	gw.mu.Unlock()
	if !ok || state == nil {
		return
	}
	if cancel != nil {
		cancel()
		return
	}
	if state.ctrl != nil {
		state.ctrl.Cancel()
	}
}

func (gw *BotGateway) storeReactionCleanup(key string, cleanup func()) {
	if cleanup == nil {
		return
	}
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.pendingReactionCleanups[key] = append(gw.pendingReactionCleanups[key], cleanup)
}

func (gw *BotGateway) flushReactionCleanups(key string, cleanup func()) {
	stored := gw.takeReactionCleanups(key)
	runReactionCleanups(stored)
	if cleanup != nil {
		cleanup()
	}
}

func (gw *BotGateway) takeReactionCleanups(key string) []func() {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	stored := gw.pendingReactionCleanups[key]
	delete(gw.pendingReactionCleanups, key)
	return stored
}

func runReactionCleanups(cleanups []func()) {
	for _, cleanup := range cleanups {
		if cleanup != nil {
			cleanup()
		}
	}
}

func makeReactionCleanup(cleanups []func()) func() {
	if len(cleanups) == 0 {
		return nil
	}
	return func() {
		runReactionCleanups(cleanups)
	}
}

func (gw *BotGateway) addPendingReaction(ctx context.Context, plat Platform, adapter Adapter, msg InboundMessage) func() {
	if strings.TrimSpace(msg.MessageID) == "" {
		return nil
	}
	reactor, ok := adapter.(pendingReactionAdapter)
	if !ok {
		return nil
	}
	cleanup, err := reactor.AddPendingReaction(ctx, msg.MessageID)
	if err != nil {
		gw.logger.Warn("pending reaction failed", "platform", plat, "err", err)
		return nil
	}
	return cleanup
}

func (gw *BotGateway) isSelfMessage(msg InboundMessage) bool {
	if !gw.cfg.IgnoreSelfMessages {
		return false
	}
	actor := strings.TrimSpace(msg.UserID)
	if strings.TrimSpace(msg.OperatorID) != "" {
		actor = strings.TrimSpace(msg.OperatorID)
	}
	if actor != "" && gw.selfUserIDs[msg.Platform][actor] {
		return true
	}
	messageID := strings.TrimSpace(msg.MessageID)
	if messageID == "" {
		return false
	}
	key := outboundMessageKey(msg.Platform, msg.ConnectionID, msg.Domain, msg.ChatID, messageID)
	now := time.Now()
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.pruneOutboundMessagesLocked(now)
	_, ok := gw.outboundMessageIDs[key]
	return ok
}

func (gw *BotGateway) rememberOutboundMessage(platform Platform, connID, domain, chatID, messageID string) {
	messageID = strings.TrimSpace(messageID)
	if !gw.cfg.IgnoreSelfMessages || messageID == "" {
		return
	}
	now := time.Now()
	key := outboundMessageKey(platform, connID, domain, chatID, messageID)
	gw.mu.Lock()
	defer gw.mu.Unlock()
	gw.pruneOutboundMessagesLocked(now)
	gw.outboundMessageIDs[key] = now.Add(outboundEchoTTL)
}

func (gw *BotGateway) pruneOutboundMessagesLocked(now time.Time) {
	for key, expiresAt := range gw.outboundMessageIDs {
		if !expiresAt.After(now) {
			delete(gw.outboundMessageIDs, key)
		}
	}
}

func outboundMessageKey(platform Platform, connID, domain, chatID, messageID string) string {
	return strings.Join([]string{
		string(platform),
		strings.TrimSpace(connID),
		strings.TrimSpace(domain),
		strings.TrimSpace(chatID),
		strings.TrimSpace(messageID),
	}, "\x00")
}

func (gw *BotGateway) connectionAccess(msg InboundMessage) (AccessConfig, bool) {
	if gw.cfg.ConnectionAccess == nil {
		return AccessConfig{}, false
	}
	id := strings.TrimSpace(msg.ConnectionID)
	if id == "" {
		return AccessConfig{}, false
	}
	access, ok := gw.cfg.ConnectionAccess[id]
	if !ok {
		return AccessConfig{}, false
	}
	if !accessConfigActive(access) {
		return AccessConfig{}, false
	}
	return access, true
}

func accessConfigActive(access AccessConfig) bool {
	return access.Enabled ||
		access.AllowAll ||
		access.PairingEnabled ||
		len(access.Users) > 0 ||
		len(access.Groups) > 0 ||
		len(access.Approvers) > 0 ||
		len(access.Admins) > 0
}

func (gw *BotGateway) checkAllowlist(plat Platform, msg InboundMessage) bool {
	if access, ok := gw.connectionAccess(msg); ok {
		return checkConnectionAllowlist(access, msg)
	}
	if gw.cfg.Allowlist.AllowAll {
		return true
	}
	if !gw.cfg.Allowlist.Enabled {
		return false
	}
	actor := msg.UserID
	if msg.OperatorID != "" {
		actor = msg.OperatorID
	}
	if !gw.allowlist[plat][actor] {
		return false
	}
	groups := gw.groupAllowlist[plat]
	if chatUsesGroupAllowlist(msg.ChatType) && len(groups) > 0 && !groups[msg.ChatID] {
		return false
	}
	return true
}

func checkConnectionAllowlist(access AccessConfig, msg InboundMessage) bool {
	if access.AllowAll {
		return true
	}
	if !access.Enabled {
		return false
	}
	actor := msg.UserID
	if msg.OperatorID != "" {
		actor = msg.OperatorID
	}
	users := stringSet(append(append(append([]string{}, access.Users...), access.Admins...), access.Approvers...))
	groups := stringSet(access.Groups)
	actorAllowed := users[actor]
	groupAllowed := chatUsesGroupAllowlist(msg.ChatType) && groups[msg.ChatID]
	if len(users) == 0 && len(groups) == 0 {
		return false
	}
	return actorAllowed || groupAllowed
}

func (gw *BotGateway) requireCommandRole(ctx context.Context, adapter Adapter, msg InboundMessage, role string) bool {
	if gw.checkCommandRole(msg.Platform, msg, role) {
		return true
	}
	_ = gw.sendText(ctx, adapter, msg, "抱歉，你没有执行此 bot 命令的权限。")
	return false
}

func (gw *BotGateway) checkCommandRole(plat Platform, msg InboundMessage, role string) bool {
	actor := msg.UserID
	if msg.OperatorID != "" {
		actor = msg.OperatorID
	}
	if strings.TrimSpace(actor) == "" {
		return false
	}
	if access, ok := gw.connectionAccess(msg); ok {
		admins := stringSet(access.Admins)
		approvers := stringSet(access.Approvers)
		if len(admins) == 0 && len(approvers) == 0 {
			return true
		}
		if admins[actor] {
			return true
		}
		return role == "approver" && approvers[actor]
	}
	admins := stringSet(gw.cfg.Allowlist.Admins[plat])
	approvers := stringSet(gw.cfg.Allowlist.Approvers[plat])
	if len(admins) == 0 && len(approvers) == 0 {
		return true
	}
	if admins[actor] {
		return true
	}
	if role == "approver" && approvers[actor] {
		return true
	}
	return false
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out[value] = true
		}
	}
	return out
}

func (gw *BotGateway) offerPairing(ctx context.Context, adapter Adapter, msg InboundMessage) bool {
	if access, ok := gw.connectionAccess(msg); ok {
		if !access.PairingEnabled {
			return false
		}
	} else if !gw.cfg.PairingEnabled {
		return false
	}
	req, created, err := CreateOrRefreshPairingRequest(msg, PairingConfig{
		Enabled:               true,
		RequestTTL:            gw.cfg.PairingTTL,
		MaxPendingPerPlatform: gw.cfg.PairingMaxPending,
	})
	if err != nil {
		gw.logger.Warn("bot pairing request failed", "platform", msg.Platform, "chat_type", msg.ChatType, "err", err)
		return false
	}
	prefix := "需要先完成配对。"
	if !created {
		prefix = "你已有待批准的配对请求。"
	}
	text := fmt.Sprintf("%s\n配对码: %s\n请在本机运行: reasonix bot pairing approve %s\n此码将在 %s 过期。",
		prefix, req.Code, req.Code, req.ExpiresAt.Local().Format("2006-01-02 15:04"))
	_ = gw.sendText(ctx, adapter, msg, text)
	return true
}

func chatUsesGroupAllowlist(chatType ChatType) bool {
	switch chatType {
	case ChatGroup, ChatGuild, ChatThread:
		return true
	default:
		return false
	}
}

func (gw *BotGateway) normalizeApprovalShortcut(key, text string) (string, bool) {
	approvalID := gw.currentPendingApprovalID(key)
	if approvalID == "" {
		return "", false
	}
	if gw.pendingApprovalIsRecovery(key, approvalID) {
		if command, ok := recoveryShortcutCommand(text, gw.pendingRecoveryCanGrantTask(key, approvalID)); ok {
			return command + " " + approvalID, true
		}
		return "", false
	}
	command, ok := approvalShortcutCommand(text)
	if !ok {
		return "", false
	}
	return command + " " + approvalID, true
}

func approvalShortcutCommand(text string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "1", "y", "yes", "ok", "同意", "批准", "允许", "允许一次":
		return "/approve", true
	case "2", "0", "n", "no", "deny", "拒绝":
		return "/deny", true
	default:
		return "", false
	}
}

func recoveryShortcutCommand(text string, canGrantTask bool) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "1", "y", "yes", "ok", "继续", "继续此变更", "continue":
		return "/recovery-continue", true
	case "2", "a", "同类", "本任务允许", "allow similar":
		if canGrantTask {
			return "/recovery-continue-task", true
		}
		return "/recovery-revise", true
	case "3":
		if canGrantTask {
			return "/recovery-revise", true
		}
		return "", false
	case "修改", "修改方案", "换个办法", "revise":
		return "/recovery-revise", true
	default:
		return "", false
	}
}

func (gw *BotGateway) pendingRecoveryCanGrantTask(key, id string) bool {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	state, ok := gw.controllers[key]
	if !ok || state.pendingApprovals == nil {
		return false
	}
	a, ok := state.pendingApprovals[id]
	return ok && a.Recovery != nil && a.Recovery.CanGrantTask
}

func (gw *BotGateway) pendingApprovalIsRecovery(key, id string) bool {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	state, ok := gw.controllers[key]
	if !ok || state.pendingApprovals == nil {
		return false
	}
	a, ok := state.pendingApprovals[id]
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(a.Kind), "recovery") || a.Recovery != nil
}

func decisionShortcutCommand(text string) (string, bool) {
	if command, ok := approvalShortcutCommand(text); ok {
		return command, true
	}
	if _, ok := askShortcutAnswer(text); ok {
		return "/answer", true
	}
	return "", false
}

func (gw *BotGateway) currentPendingApprovalID(key string) string {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	state, ok := gw.controllers[key]
	if !ok || len(state.pendingApprovals) == 0 {
		return ""
	}
	if state.lastApprovalID != "" {
		if _, ok := state.pendingApprovals[state.lastApprovalID]; ok {
			return state.lastApprovalID
		}
	}
	for id := range state.pendingApprovals {
		return id
	}
	return ""
}

func (gw *BotGateway) forgetPendingApproval(key, id string) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	state, ok := gw.controllers[key]
	if !ok || state.pendingApprovals == nil {
		return
	}
	delete(state.pendingApprovals, id)
	if state.lastApprovalID == id {
		state.lastApprovalID = ""
		for nextID := range state.pendingApprovals {
			state.lastApprovalID = nextID
			break
		}
	}
}

func (gw *BotGateway) normalizeAskShortcut(key, text string) (string, bool) {
	raw := strings.TrimSpace(text)
	if raw == "" || strings.HasPrefix(raw, "/") {
		return "", false
	}
	askID := gw.currentPendingAskIDForReply(key)
	if askID == "" {
		return "", false
	}
	return "/answer " + askID + " " + raw, true
}

func askShortcutAnswer(text string) (string, bool) {
	raw := strings.TrimSpace(text)
	if raw == "" {
		return "", false
	}
	if strings.ContainsAny(raw, " \t\n;=") {
		return "", false
	}
	if _, err := strconv.Atoi(raw); err == nil {
		return raw, true
	}
	return "", false
}

func (gw *BotGateway) currentPendingAskIDForReply(key string) string {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	state, ok := gw.controllers[key]
	if !ok || len(state.pendingAsks) == 0 {
		return ""
	}
	if state.lastAskID != "" {
		if _, ok := state.pendingAsks[state.lastAskID]; ok {
			return state.lastAskID
		}
	}
	if len(state.pendingAsks) != 1 {
		return ""
	}
	for id := range state.pendingAsks {
		return id
	}
	return ""
}
