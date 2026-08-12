package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"time"

	"reasonix/internal/bot"
	"reasonix/internal/config"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

func (a *adapter) handleMessage(ctx context.Context, msg feishuMsgEvent) {
	mentions := webhookMentionRefs(msg.Mentions)

	chatType := bot.ChatDM
	if msg.ChatType == "group" || msg.ChatType == "topic_group" {
		chatType = bot.ChatGroup
		if a.cfg.RequireMention && !a.mentionsBot(mentions) {
			a.logger.Info("feishu message ignored", "reason", "missing_mention", "chat", logHash(msg.ChatID), "message", logHash(msg.MessageID))
			return
		}
	}

	text, media, ok := a.parseInboundContent(msg.MsgType, msg.Content, msg.MessageID)
	if !ok {
		a.logger.Info("feishu message ignored", "reason", "unsupported_type", "msg_type", msg.MsgType, "chat_type", msg.ChatType, "message", logHash(msg.MessageID))
		return
	}
	text = a.replaceMentionPlaceholders(text, mentions)
	if strings.TrimSpace(text) == "" && len(media) == 0 {
		a.logger.Info("feishu message ignored", "reason", "empty_after_parse", "msg_type", msg.MsgType, "message", logHash(msg.MessageID))
		return
	}

	userName := msg.Sender.SenderID.OpenID
	var resolveUserName func(context.Context) string
	if userName != "" {
		openID := msg.Sender.SenderID.OpenID
		resolveUserName = func(ctx context.Context) string {
			return a.resolveUserName(ctx, openID)
		}
	}
	ib := bot.InboundMessage{
		Platform:        bot.PlatformFeishu,
		ChatType:        chatType,
		ChatID:          msg.ChatID,
		UserID:          msg.Sender.SenderID.OpenID,
		UserName:        userName,
		Text:            text,
		MessageID:       msg.MessageID,
		ThreadID:        msg.ThreadID,
		Media:           media,
		ResolveUserName: resolveUserName,
	}

	select {
	case a.msgCh <- ib:
		a.logger.Info("feishu inbound queued", "chat_type", chatType, "msg_type", msg.MsgType, "chat", logHash(ib.ChatID), "user", logHash(ib.UserID), "message", logHash(ib.MessageID), "text_chars", len([]rune(ib.Text)), "media_items", len(media))
	default:
		a.logger.Warn("feishu message channel full")
	}
}

// SendText sends an interactive card with markdown content to a Feishu/Lark chat_id using the SDK.
// It is used by the desktop settings panel as an actual connection test.
func SendText(ctx context.Context, cfg config.FeishuBotConfig, chatID, text string) (bot.SendResult, error) {
	a := &adapter{cfg: cfg, logger: slog.Default().With("platform", "feishu")}
	return a.sendMessage(ctx, bot.OutboundMessage{ChatID: chatID, Text: text})
}

// sendMessage 使用飞书/Lark SDK 以 Interactive Card (JSON 2.0) 发送消息。
// Card 内嵌 markdown 元素，支持 CommonMark 标准语法。
// 当卡片体积超过 30KB 限制（如大段代码），自动降级为纯文本消息。
// MediaURLs are bare filenames staged in an operator-configured outbound media
// root. URL fetching and arbitrary-path reads are intentionally unsupported.
func (a *adapter) sendMessage(ctx context.Context, msg bot.OutboundMessage) (bot.SendResult, error) {
	if msg.Card != nil {
		return a.sendCard(ctx, msg)
	}
	if len(msg.MediaURLs) == 0 {
		return a.sendRenderedText(ctx, msg)
	}
	media, err := a.loadOutboundMedia(msg.MediaURLs)
	if err != nil {
		return bot.SendResult{}, err
	}

	var result bot.SendResult
	if strings.TrimSpace(msg.Text) != "" {
		textResult, err := a.sendRenderedText(ctx, msg)
		result.Merge(textResult)
		if err != nil {
			return result, err
		}
	}
	mediaResult, err := a.sendMedia(ctx, msg, media)
	result.Merge(mediaResult)
	return result, err
}

func (a *adapter) sendRenderedText(ctx context.Context, msg bot.OutboundMessage) (bot.SendResult, error) {
	cardContent, err := buildMarkdownCard(msg.Text)
	if err != nil {
		a.logger.Warn("build markdown card failed, falling back to text", "err", err)
		return a.sendSDKContent(ctx, msg, larkim.MsgTypeText, feishuTextContent(msg.Text))
	}
	result, err := a.sendSDKContent(ctx, msg, larkim.MsgTypeInteractive, cardContent)
	if err != nil && isCardLimitError(err) {
		a.logger.Warn("card send failed (size limit), retrying as text", "err", err)
		return a.sendSDKContent(ctx, msg, larkim.MsgTypeText, feishuTextContent(msg.Text))
	}
	return result, err
}

func buildMarkdownCard(content string) (string, error) {
	card := map[string]any{
		"schema": "2.0",

		"config": map[string]any{
			"update_multi": true,
		},
		"body": map[string]any{
			"elements": []map[string]any{
				{
					"tag":     "markdown",
					"content": content,
				},
			},
		},
	}
	data, err := json.Marshal(card)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func feishuTextContent(text string) string {
	content, _ := json.Marshal(textContent{Text: text})
	return string(content)
}

func isCardLimitError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "11310") || strings.Contains(s, "11325")
}

const feishuReplyRecalledCode = 230011

type feishuAPIError struct {
	op   string
	code int
	msg  string
}

func (e *feishuAPIError) Error() string {
	return fmt.Sprintf("feishu %s error: %s", e.op, feishuCodeError(e.code, e.msg))
}

// sdkClient lazily builds the shared lark client. It is called concurrently —
// the fetchBotOpenID goroutine, per-message resolveUserName, and per-resource
// downloads all race on first use at startup — so the check-and-build is guarded
// by clientMu (a bare a.client read/write would data-race, tripping -race).
func (a *adapter) sdkClient() (*lark.Client, error) {
	a.clientMu.Lock()
	defer a.clientMu.Unlock()
	if a.client != nil {
		return a.client, nil
	}
	secret, err := a.appSecret()
	if err != nil {
		return nil, err
	}
	opts := []lark.ClientOptionFunc{
		lark.WithLogLevel(larkcore.LogLevelError),
		lark.WithReqTimeout(15 * time.Second),
		lark.WithSource("reasonix"),
	}
	if feishuDomain(a.cfg.Domain) == "lark" {
		opts = append(opts, lark.WithOpenBaseUrl(lark.LarkBaseUrl), lark.WithOAuthBaseUrl(lark.OAuthBaseUrlLark))
	}
	a.client = lark.NewClient(a.cfg.AppID, secret, opts...)
	return a.client, nil
}

func (a *adapter) sendSDKContent(ctx context.Context, msg bot.OutboundMessage, msgType, content string) (bot.SendResult, error) {
	client, err := a.sdkClient()
	if err != nil {
		return bot.SendResult{}, err
	}
	chatID := strings.TrimSpace(msg.ChatID)
	if chatID == "" {
		return bot.SendResult{}, fmt.Errorf("feishu chat_id is empty")
	}

	if replyTo := strings.TrimSpace(msg.ReplyToMsgID); replyTo != "" {
		result, err := a.replySDKContent(ctx, replyTo, msgType, content)
		if err == nil {
			return result, nil
		}
		if !isReplyFallbackError(err) {
			return bot.SendResult{}, err
		}
		a.logger.Warn("feishu reply failed; falling back to create", "message", logHash(replyTo), "err", err)
	}

	uuid := newIdempotencyKey()
	var result bot.SendResult
	err = withTransientRetry(ctx, a.logger, "create message", func(ctx context.Context) error {
		body := larkim.NewCreateMessageReqBodyBuilder().ReceiveId(chatID).MsgType(msgType).Content(content)
		if uuid != "" {
			body = body.Uuid(uuid)
		}
		req := larkim.NewCreateMessageReqBuilder().
			ReceiveIdType(larkim.CreateMessageV1ReceiveIDTypeChatId).
			Body(body.Build()).
			Build()
		resp, err := client.Im.Message.Create(ctx, req)
		if err != nil {
			return err
		}
		if resp == nil {
			return fmt.Errorf("feishu send error: empty response")
		}
		if !resp.Success() {
			return fmt.Errorf("feishu send error: %s", feishuCodeError(resp.Code, resp.Msg))
		}
		if resp.Data != nil {
			result = bot.SendResult{MessageID: stringPtrValue(resp.Data.MessageId)}
		}
		return nil
	})
	if err != nil {
		return bot.SendResult{}, err
	}
	return result, nil
}

func (a *adapter) replySDKContent(ctx context.Context, replyTo, msgType, content string) (bot.SendResult, error) {
	client, err := a.sdkClient()
	if err != nil {
		return bot.SendResult{}, err
	}
	uuid := newIdempotencyKey()
	var result bot.SendResult
	err = withTransientRetry(ctx, a.logger, "reply message", func(ctx context.Context) error {
		body := larkim.NewReplyMessageReqBodyBuilder().MsgType(msgType).Content(content)
		if uuid != "" {
			body = body.Uuid(uuid)
		}
		req := larkim.NewReplyMessageReqBuilder().
			MessageId(replyTo).
			Body(body.Build()).
			Build()
		resp, err := client.Im.Message.Reply(ctx, req)
		if err != nil {
			return err
		}
		if resp == nil {
			return fmt.Errorf("feishu reply error: empty response")
		}
		if !resp.Success() {
			return &feishuAPIError{op: "reply", code: resp.Code, msg: resp.Msg}
		}
		if resp.Data != nil {
			result = bot.SendResult{MessageID: stringPtrValue(resp.Data.MessageId)}
		}
		return nil
	})
	if err != nil {
		return bot.SendResult{}, err
	}
	return result, nil
}

func (a *adapter) AddPendingReaction(ctx context.Context, messageID string) (func(), error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return nil, nil
	}
	client, err := a.sdkClient()
	if err != nil {
		return nil, err
	}
	req := larkim.NewCreateMessageReactionReqBuilder().
		MessageId(messageID).
		Body(larkim.NewCreateMessageReactionReqBodyBuilder().
			ReactionType(larkim.NewEmojiBuilder().EmojiType(feishuPendingReactionEmoji).Build()).
			Build()).
		Build()
	resp, err := client.Im.MessageReaction.Create(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp == nil || !resp.Success() {
		if resp != nil {
			return nil, fmt.Errorf("feishu reaction error: %s", feishuCodeError(resp.Code, resp.Msg))
		}
		return nil, fmt.Errorf("feishu reaction error: empty response")
	}
	reactionID := ""
	if resp.Data != nil && resp.Data.ReactionId != nil {
		reactionID = *resp.Data.ReactionId
	}
	if reactionID == "" {
		return nil, nil
	}
	cleanup := func() {
		delReq := larkim.NewDeleteMessageReactionReqBuilder().
			MessageId(messageID).
			ReactionId(reactionID).
			Build()
		if _, err := client.Im.MessageReaction.Delete(context.Background(), delReq); err != nil {
			a.logger.Warn("feishu reaction cleanup failed", "message", logHash(messageID), "err", err)
		}
	}
	return cleanup, nil
}

// sendCard 发送 interactive card 消息（用于审批/问答）。
func (a *adapter) sendCard(ctx context.Context, msg bot.OutboundMessage) (bot.SendResult, error) {
	card := msg.Card

	elements := make([]map[string]any, 0)
	for _, el := range card.Elements {
		item := map[string]any{"tag": el.Tag}
		if el.Content != "" {
			item["content"] = el.Content
		}
		if actions, ok := el.Extra["actions"]; ok && el.Tag == "action" {
			item["actions"] = actions
		} else {
			maps.Copy(item, el.Extra)
		}
		elements = append(elements, item)
	}

	cardPayload := map[string]any{
		"header": map[string]any{
			"title": map[string]string{
				"tag":     "plain_text",
				"content": card.Header,
			},
		},
		"elements": elements,
	}

	cardJSON, _ := json.Marshal(cardPayload)
	return a.sendSDKContent(ctx, msg, larkim.MsgTypeInteractive, string(cardJSON))
}

func feishuDomain(domain string) string {
	if strings.EqualFold(strings.TrimSpace(domain), "lark") {
		return "lark"
	}
	return "feishu"
}

func stringPtrValue(ptr *string) string {
	if ptr == nil {
		return ""
	}
	return strings.TrimSpace(*ptr)
}

func feishuCodeError(code int, msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		msg = "unknown error"
	}
	if code == 0 {
		return msg
	}
	return fmt.Sprintf("%s (code %d)", msg, code)
}
