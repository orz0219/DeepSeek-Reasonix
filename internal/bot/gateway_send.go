package bot

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"reasonix/internal/event"
)

func (gw *BotGateway) sendText(ctx context.Context, adapter Adapter, msg InboundMessage, text string) error {
	out := OutboundMessage{
		ConnectionID: msg.ConnectionID,
		Domain:       msg.Domain,
		ChatID:       msg.ChatID,
		ChatType:     msg.ChatType,
		Text:         text,
		ReplyToMsgID: msg.MessageID,
	}
	binding := AdapterBinding{
		ID:       strings.TrimSpace(msg.ConnectionID),
		Domain:   strings.TrimSpace(msg.Domain),
		Platform: msg.Platform,
		Adapter:  adapter,
	}
	if binding.Platform == "" && adapter != nil {
		binding.Platform = adapter.Platform()
	}
	if binding.ID == "" && adapter != nil {
		binding.ID = adapter.Name()
	}
	result, err := gw.sendViaAdapter(ctx, binding, out)
	if err != nil {
		gw.logger.Warn("bot send failed", "platform", msg.Platform, "chat_type", msg.ChatType, "chat", hashID(msg.ChatID), "reply_to", hashID(msg.MessageID), "err", err)
		return err
	}
	gw.logger.Info("bot send completed", "platform", msg.Platform, "chat_type", msg.ChatType, "chat", hashID(msg.ChatID), "reply_to", hashID(msg.MessageID), "message", hashID(result.MessageID))
	return err
}

func (gw *BotGateway) sendViaAdapter(ctx context.Context, binding AdapterBinding, msg OutboundMessage) (SendResult, error) {
	if binding.Adapter == nil {
		return SendResult{}, errors.New("bot send: adapter is nil")
	}
	if strings.TrimSpace(msg.ConnectionID) == "" {
		msg.ConnectionID = binding.ID
	}
	if strings.TrimSpace(msg.Domain) == "" {
		msg.Domain = binding.Domain
	}
	result, err := binding.Adapter.Send(ctx, msg)
	gw.markAdapterSend(binding, err)
	for _, messageID := range result.DeliveredMessageIDs() {
		gw.rememberOutboundMessage(binding.Platform, binding.ID, binding.Domain, msg.ChatID, messageID)
	}
	return result, err
}

func parseAskAnswers(questions []event.AskQuestion, raw string) []event.AskAnswer {
	raw = strings.TrimSpace(raw)
	if len(questions) == 0 {
		return []event.AskAnswer{{Selected: []string{raw}}}
	}
	byID := make(map[string]*event.AskQuestion, len(questions))
	for i := range questions {
		q := &questions[i]
		byID[q.ID] = q
		byID[fmt.Sprintf("%d", i+1)] = q
	}
	answerMap := make(map[string][]string, len(questions))
	if strings.Contains(raw, "=") {
		for part := range strings.SplitSeq(raw, ";") {
			k, v, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			q := byID[strings.TrimSpace(k)]
			if q == nil {
				continue
			}
			answerMap[q.ID] = normalizeAskSelection(*q, strings.TrimSpace(v))
		}
	} else if len(questions) == 1 {
		answerMap[questions[0].ID] = normalizeAskSelection(questions[0], raw)
	}
	out := make([]event.AskAnswer, 0, len(questions))
	for _, q := range questions {
		out = append(out, event.AskAnswer{QuestionID: q.ID, Selected: answerMap[q.ID]})
	}
	return out
}

func normalizeAskSelection(q event.AskQuestion, raw string) []string {
	parts := []string{raw}
	if q.Multi && strings.Contains(raw, ",") {
		parts = strings.Split(raw, ",")
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx, err := strconv.Atoi(part); err == nil && idx >= 1 && idx <= len(q.Options) {
			out = append(out, q.Options[idx-1].Label)
			continue
		}
		out = append(out, part)
	}
	return out
}

// UpdateConnectionToolApprovalMode updates the in-memory tool approval mode for
// a single bot connection without restarting the gateway. Empty mode clears the
// connection override, so existing sessions inherit the current gateway default.
func (gw *BotGateway) UpdateConnectionToolApprovalMode(connID, mode string) {
	connID = strings.TrimSpace(connID)
	if connID == "" {
		return
	}
	mode = normalizeOptionalBotToolApprovalMode(mode)
	type controllerMode struct {
		ctrl botController
		mode string
	}
	var updates []controllerMode

	gw.mu.Lock()
	if gw.cfg.ConnectionChannels == nil {
		gw.cfg.ConnectionChannels = make(map[string]ChannelConfig)
	}
	ch := gw.cfg.ConnectionChannels[connID]
	ch.ToolApprovalMode = mode
	gw.cfg.ConnectionChannels[connID] = ch

	for _, state := range gw.controllers {
		if state == nil || state.ctrl == nil || strings.TrimSpace(state.connectionID) != connID {
			continue
		}
		effectiveMode := mode
		if effectiveMode == "" {
			effectiveMode = normalizeBotToolApprovalMode(gw.cfg.ToolApprovalMode)
		}
		updates = append(updates, controllerMode{ctrl: state.ctrl, mode: effectiveMode})
	}
	gw.mu.Unlock()

	for _, update := range updates {
		update.ctrl.SetToolApprovalMode(update.mode)
	}
}

// SendToAdapter sends a message through the adapter identified by connID.
// Returns an error if no matching adapter is found.
func (gw *BotGateway) SendToAdapter(ctx context.Context, connID, domain string, msg OutboundMessage) (SendResult, error) {
	connID = strings.TrimSpace(connID)
	domain = strings.TrimSpace(domain)
	var target AdapterBinding
	gw.mu.Lock()
	for _, binding := range gw.adapters {
		if strings.TrimSpace(binding.ID) == connID &&
			(domain == "" || strings.EqualFold(strings.TrimSpace(binding.Domain), domain)) {
			target = binding
			break
		}
	}
	gw.mu.Unlock()
	if target.Adapter != nil {
		return gw.sendViaAdapter(ctx, target, msg)
	}
	return SendResult{}, fmt.Errorf("SendToAdapter: no adapter found for connection %q (domain %q)", connID, domain)
}

// SendTextToAdapter sends a plain text message through the adapter identified by connID.
func (gw *BotGateway) SendTextToAdapter(ctx context.Context, connID, domain, chatID string, chatType ChatType, text string) (SendResult, error) {
	return gw.SendToAdapter(ctx, connID, domain, OutboundMessage{
		ChatID:   chatID,
		ChatType: chatType,
		Text:     text,
	})
}
