package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/sessioninbox"
)

func parseQueueCommand(text string) (mode string, clear bool, statusOnly bool, ok bool) {
	parts := strings.Fields(text)
	if len(parts) == 0 || strings.ToLower(strings.TrimSpace(parts[0])) != "/queue" {
		return "", false, false, false
	}
	if len(parts) == 1 {
		return "", false, true, true
	}
	switch strings.ToLower(strings.TrimSpace(parts[1])) {
	case "status", "state", "show", "状态", "查看":
		return "", false, true, true
	case "default", "reset", "inherit", "默认", "重置":
		return "", true, false, true
	default:
		if normalized := NormalizeOptionalQueueMode(parts[1]); normalized != "" {
			return normalized, false, false, true
		}
		return "", false, false, false
	}
}

func (gw *BotGateway) queueStatusText(key string, msg InboundMessage) string {
	inboxN := 0
	paused := false
	if api := gw.sessionAPI(key); api != nil {
		snap := api.InboxSnapshot()
		inboxN = len(snap.Items)
		paused = snap.Paused
	}
	return fmt.Sprintf("当前队列模式：%s\n持久化 Inbox: %d%s\n全局上限: %d\n溢出策略: 拒绝新消息（queue_drop 已弃用）\n用法：/queue steer|followup|collect|interrupt|status|list|show|delete|move|pause|resume|retry|default",
		queueModeLabel(gw.queueMode(key, msg)),
		inboxN,
		map[bool]string{true: " (paused)", false: ""}[paused],
		sessioninbox.DefaultMaxItems,
	)
}

func queueModeLabel(mode string) string {
	switch NormalizeQueueMode(mode) {
	case QueueModeFollowup:
		return "逐条跟进"
	case QueueModeCollect:
		return "合并收集"
	case QueueModeInterrupt:
		return "打断重跑"
	default:
		return "即时补充"
	}
}

func (gw *BotGateway) adapterHealthSummaryText() string {
	snapshots := gw.AdapterHealth()
	if len(snapshots) == 0 {
		return "未启动"
	}
	parts := make([]string, 0, len(snapshots))
	for _, h := range snapshots {
		label := strings.TrimSpace(h.ID)
		if label == "" {
			label = string(h.Platform)
		}
		status := strings.TrimSpace(h.Status)
		if status == "" {
			status = "unknown"
		}
		parts = append(parts, fmt.Sprintf("%s=%s", label, status))
	}
	return strings.Join(parts, ", ")
}

func parseToolApprovalModeCommand(text string) (mode string, statusOnly bool, ok bool) {
	parts := strings.Fields(text)
	if len(parts) == 0 {
		return "", false, false
	}
	cmd := strings.ToLower(strings.TrimSpace(parts[0]))
	switch cmd {
	case "/yolo":
		if len(parts) == 1 {
			return control.ToolApprovalYolo, false, true
		}
		return parseToolApprovalModeArg(parts[1])
	case "/mode":
		if len(parts) == 1 {
			return "", true, true
		}
		return parseToolApprovalModeArg(parts[1])
	default:
		return "", false, false
	}
}

func parseToolApprovalModeArg(arg string) (mode string, statusOnly bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "status", "state", "show", "状态", "查看":
		return "", true, true
	case "on", "enable", "enabled", "true", "1", "yolo", "full", "full-access", "bypass", "开启", "打开":
		return control.ToolApprovalYolo, false, true
	case "off", "disable", "disabled", "false", "0", "ask", "询问", "关闭":
		return control.ToolApprovalAsk, false, true
	case "auto", "自动":
		return control.ToolApprovalAuto, false, true
	default:
		return "", false, false
	}
}

func (gw *BotGateway) setToolApprovalModeForMessage(key string, msg InboundMessage, mode string) error {
	mode = normalizeBotToolApprovalMode(mode)
	var ctrl botController

	gw.mu.Lock()
	if state, ok := gw.controllers[key]; ok {
		ctrl = state.ctrl
	}
	gw.updateToolApprovalModeDefaultLocked(msg, mode)
	gw.mu.Unlock()

	if ctrl != nil {
		ctrl.SetToolApprovalMode(mode)
	}
	if gw.cfg.OnToolApprovalModeChange != nil {
		return gw.cfg.OnToolApprovalModeChange(msg, mode)
	}
	return nil
}

func (gw *BotGateway) updateToolApprovalModeDefaultLocked(msg InboundMessage, mode string) {
	if id := strings.TrimSpace(msg.ConnectionID); id != "" {
		if gw.cfg.ConnectionChannels == nil {
			gw.cfg.ConnectionChannels = make(map[string]ChannelConfig)
		}
		channel := gw.cfg.ConnectionChannels[id]
		channel.ToolApprovalMode = mode
		gw.cfg.ConnectionChannels[id] = channel
		return
	}
	if msg.Platform != "" {
		if gw.cfg.Channels == nil {
			gw.cfg.Channels = make(map[Platform]ChannelConfig)
		}
		channel := gw.cfg.Channels[msg.Platform]
		channel.ToolApprovalMode = mode
		gw.cfg.Channels[msg.Platform] = channel
		return
	}
	gw.cfg.ToolApprovalMode = mode
}

func (gw *BotGateway) currentToolApprovalMode(key string, msg InboundMessage) string {
	var ctrl botController
	gw.mu.Lock()
	if state, ok := gw.controllers[key]; ok {
		ctrl = state.ctrl
	}
	gw.mu.Unlock()
	if ctrl != nil {
		return ctrl.ToolApprovalMode()
	}
	_, _, mode := gw.sessionOptionsForMessage(msg)
	return mode
}

func (gw *BotGateway) toolApprovalModeStatusText(key string, msg InboundMessage) string {
	mode := gw.currentToolApprovalMode(key, msg)
	return fmt.Sprintf("当前工具审批模式：%s\n用法：/yolo on|off|auto|status，或 /mode yolo|ask|auto", toolApprovalModeLabel(mode))
}

func toolApprovalModeChangedText(mode string) string {
	switch normalizeBotToolApprovalMode(mode) {
	case control.ToolApprovalYolo:
		return "已开启 YOLO：普通工具审批将自动放行；Ask 问题和计划批准仍会等待确认。"
	case control.ToolApprovalAuto:
		return "已切换为自动模式：策略允许的工具会自动放行，仍保留需要询问或拒绝的规则。"
	default:
		return "已切回询问模式：工具执行前会请求确认。"
	}
}

func toolApprovalModeLabel(mode string) string {
	switch normalizeBotToolApprovalMode(mode) {
	case control.ToolApprovalYolo:
		return "YOLO"
	case control.ToolApprovalAuto:
		return "自动"
	default:
		return "询问"
	}
}

func (gw *BotGateway) runTurn(ctx context.Context, adapter Adapter, key string, msg InboundMessage, cleanup func()) {
	gw.runTurnItem(ctx, adapter, key, msg, "", cleanup)
}

func (gw *BotGateway) runTurnItem(ctx context.Context, adapter Adapter, key string, msg InboundMessage, inboxItemID string, cleanup func()) {
	gw.logger.Info("bot turn started", "platform", msg.Platform, "chat_type", msg.ChatType, "chat", hashID(msg.ChatID), "session", key[:8])
	defer gw.finishTurnItem(ctx, adapter, key, msg, cleanup)

	state := gw.getOrCreateSession(ctx, key, msg)
	if state == nil || state.ctrl == nil {
		_ = gw.sendText(ctx, adapter, msg, "内部错误：无法创建会话。")
		return
	}
	gw.rememberSessionReady(msg, state.ctrl)

	input := msg.Text
	if inboxItemID == "" {
		input = gw.inputTextWithMedia(ctx, adapter, msg, state)
	}
	if inboxItemID == "" && msg.ChatType == ChatGroup {
		userName := strings.TrimSpace(msg.UserName)
		if msg.ResolveUserName != nil {
			if resolved := strings.TrimSpace(msg.ResolveUserName(ctx)); resolved != "" {
				userName = resolved
			}
		}
		input = fmt.Sprintf("[%s] %s", userName, input)
	}

	_ = adapter.SendTyping(ctx, msg.ChatID)

	sink := newRenderSink(
		ctx,
		adapter,
		msg.ConnectionID,
		msg.Domain,
		msg.ChatID,
		msg.ChatType,
		msg.UserID,
		msg.MessageID,
		gw.logger,
		func(approval event.Approval) {
			gw.mu.Lock()
			if state.pendingApprovals == nil {
				state.pendingApprovals = make(map[string]event.Approval)
			}
			state.pendingApprovals[approval.ID] = approval
			state.lastApprovalID = approval.ID
			gw.mu.Unlock()
		},
		func(ask event.Ask) {
			gw.mu.Lock()
			if state.pendingAsks == nil {
				state.pendingAsks = make(map[string][]event.AskQuestion)
			}
			state.pendingAsks[ask.ID] = ask.Questions
			state.lastAskID = ask.ID
			gw.mu.Unlock()
		},
	)

	sink.ctrl = state.ctrl
	state.sink.setTarget(sink)
	defer state.sink.setTarget(nil)

	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	gw.mu.Lock()
	live := gw.controllers[key] == state
	if live {
		state.cancel = cancel
	}
	state.lastActive = time.Now()
	gw.mu.Unlock()
	if !live {

		cancel()
	}

	// 运行一轮对话
	var err error
	if inboxItemID == "" {
		err = state.ctrl.RunTurn(turnCtx, input)
	} else if api, ok := state.ctrl.(interface {
		RunInboxTurn(context.Context, string) error
	}); ok {
		err = api.RunInboxTurn(turnCtx, inboxItemID)
	} else {
		err = fmt.Errorf("controller cannot run durable inbox item")
	}
	sink.Emit(event.Event{Kind: event.TurnDone, Err: err})
	if err != nil {
		gw.logger.Warn("turn error", "session", key[:8], "err", err)
		return
	}
	gw.logger.Info("bot turn completed", "platform", msg.Platform, "chat_type", msg.ChatType, "chat", hashID(msg.ChatID), "session", key[:8])
}
