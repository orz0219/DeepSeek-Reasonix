// Package feishu 实现飞书自建应用 Bot 适配器。
// 参考 Hermes Agent 的 feishu adapter：
// - 长连接 WebSocket（默认）或 Webhook 模式
// - @mention gating
// - open_id / user_id / union_id 映射
// - 消息去重
// - interactive card 审批/问答
package feishu

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"reasonix/internal/bot"
	"reasonix/internal/config"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"
	larkcontact "github.com/larksuite/oapi-sdk-go/v3/service/contact/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

// textContent 飞书消息文本内容结构。
type textContent struct {
	Text string `json:"text"`
}

const feishuPendingReactionEmoji = "OnIt"

// feishuEvent 飞书事件结构。
type feishuEvent struct {
	Schema string          `json:"schema"`
	Header feishuHeader    `json:"header"`
	Event  json.RawMessage `json:"event"`
}

type feishuHeader struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	Token      string `json:"token"`
	CreateTime string `json:"create_time"`
}

type feishuMsgEvent struct {
	MessageID string          `json:"message_id"`
	RootID    string          `json:"root_id"`
	ParentID  string          `json:"parent_id"`
	ThreadID  string          `json:"thread_id"`
	ChatID    string          `json:"chat_id"`
	ChatType  string          `json:"chat_type"`
	MsgType   string          `json:"msg_type"`
	Content   string          `json:"content"`
	Sender    feishuSender    `json:"sender"`
	Mentions  []feishuMention `json:"mentions"`
}

type feishuSender struct {
	SenderID struct {
		UserID  string `json:"user_id"`
		OpenID  string `json:"open_id"`
		UnionID string `json:"union_id"`
	} `json:"sender_id"`
}

type feishuMention struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	ID   struct {
		OpenID string `json:"open_id"`
	} `json:"id"`
}

func webhookMentionRefs(mentions []feishuMention) []mentionRef {
	refs := make([]mentionRef, 0, len(mentions))
	for _, m := range mentions {
		refs = append(refs, mentionRef{Key: m.Key, OpenID: m.ID.OpenID, Name: m.Name})
	}
	return refs
}

// adapter 飞书适配器实现。
type adapter struct {
	cfg      config.FeishuBotConfig
	logger   *slog.Logger
	msgCh    chan bot.InboundMessage
	cancel   context.CancelFunc
	client   *lark.Client
	wsClient *larkws.Client

	// fetchResource 覆盖消息资源下载（测试注入）；nil 时用 sdkFetchResource。
	fetchResource func(ctx context.Context, messageID, key, typ string) ([]byte, string, error)

	clientMu sync.Mutex // 保护 client 懒初始化

	seenMu sync.Mutex
	seen   map[string]bool // 消息去重

	botMu sync.Mutex
	botID string // bot 自身 open_id，用于群聊 @ 门控与占位符剔除

	nameMu sync.Mutex
	names  map[string]nameCacheEntry // open_id -> 显示名缓存
}

type nameCacheEntry struct {
	name    string
	expires time.Time
}

const (
	userNameCacheTTL         = time.Hour
	userNameFallbackCacheTTL = 5 * time.Minute
)

// New 创建飞书 Bot 适配器。
func New(cfg config.FeishuBotConfig, logger *slog.Logger) bot.Adapter {
	return &adapter{
		cfg:    cfg,
		logger: logger.With("platform", "feishu"),
		seen:   make(map[string]bool),
	}
}

func (a *adapter) Platform() bot.Platform { return bot.PlatformFeishu }
func (a *adapter) Name() string           { return "feishu" }

func (a *adapter) Start(ctx context.Context) error {
	a.msgCh = make(chan bot.InboundMessage, 64)
	ctx, a.cancel = context.WithCancel(ctx)

	mode := a.cfg.Mode
	if mode == "" {
		mode = "webhook"
	}

	switch mode {
	case "webhook":
		// Webhook mode exposes a public HTTP endpoint; without a verification
		// token verificationTokenValid accepts every caller, so fail closed
		// rather than let anyone drive the agent.
		if strings.TrimSpace(a.cfg.VerificationToken) == "" {
			return fmt.Errorf("feishu: webhook mode needs verification_token set — refusing to expose an unauthenticated event endpoint")
		}
		go a.runWebhook(ctx)
	default:
		if _, err := a.appSecret(); err != nil {
			return err
		}
		go a.runWebSocket(ctx)
	}
	// bot open_id 用于把群聊 @ 门控收紧为“必须 @ 本 bot”；拉取失败只降级为
	// 旧行为（任意 @ 放行），不阻塞启动。
	go a.fetchBotOpenID(ctx)
	return nil
}

func (a *adapter) botOpenID() string {
	a.botMu.Lock()
	defer a.botMu.Unlock()
	return a.botID
}

func (a *adapter) fetchBotOpenID(ctx context.Context) {
	client, err := a.sdkClient()
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := client.Get(ctx, "/open-apis/bot/v3/info", nil, larkcore.AccessTokenTypeTenant)
	if err != nil {
		a.logger.Warn("feishu bot info fetch failed; group mention gating stays permissive", "err", err)
		return
	}
	var payload struct {
		Code int `json:"code"`
		Bot  struct {
			OpenID string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(resp.RawBody, &payload); err != nil || payload.Code != 0 || payload.Bot.OpenID == "" {
		a.logger.Warn("feishu bot info unavailable; group mention gating stays permissive", "code", payload.Code, "err", err)
		return
	}
	a.botMu.Lock()
	a.botID = payload.Bot.OpenID
	a.botMu.Unlock()
	a.logger.Info("feishu bot identity resolved", "open_id", logHash(payload.Bot.OpenID))
}

// resolveUserName 把 open_id 解析为显示名（1 小时缓存）。缺少 contact 权限或
// 调用失败时回退 open_id 本身，并短暂缓存回退值避免每条消息都打一次 API。
func (a *adapter) resolveUserName(ctx context.Context, openID string) string {
	openID = strings.TrimSpace(openID)
	if openID == "" {
		return ""
	}
	now := time.Now()
	a.nameMu.Lock()
	if entry, ok := a.names[openID]; ok && now.Before(entry.expires) {
		a.nameMu.Unlock()
		return entry.name
	}
	a.nameMu.Unlock()
	name, ttl := a.lookupUserName(ctx, openID)
	a.nameMu.Lock()
	if a.names == nil {
		a.names = make(map[string]nameCacheEntry)
	}
	if len(a.names) > 10000 {
		a.names = make(map[string]nameCacheEntry)
	}
	a.names[openID] = nameCacheEntry{name: name, expires: now.Add(ttl)}
	a.nameMu.Unlock()
	return name
}

func (a *adapter) lookupUserName(ctx context.Context, openID string) (string, time.Duration) {
	client, err := a.sdkClient()
	if err != nil {
		return openID, userNameFallbackCacheTTL
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req := larkcontact.NewGetUserReqBuilder().
		UserId(openID).
		UserIdType(larkcontact.UserIdTypeOpenId).
		Build()
	resp, err := client.Contact.User.Get(ctx, req)
	if err != nil || resp == nil || !resp.Success() || resp.Data == nil || resp.Data.User == nil {
		return openID, userNameFallbackCacheTTL
	}
	name := stringPtrValue(resp.Data.User.Name)
	if name == "" {
		return openID, userNameFallbackCacheTTL
	}
	return name, userNameCacheTTL
}

func (a *adapter) Stop() error {
	if a.cancel != nil {
		a.cancel()
	}
	if a.wsClient != nil {
		a.wsClient.Close()
	}
	return nil
}

func (a *adapter) Send(ctx context.Context, msg bot.OutboundMessage) (bot.SendResult, error) {
	return a.sendMessage(ctx, msg)
}

func (a *adapter) SendTyping(ctx context.Context, chatID string) error {
	return nil
}

func (a *adapter) Messages() <-chan bot.InboundMessage {
	return a.msgCh
}

func (a *adapter) appSecret() (string, error) {
	secret := os.Getenv(a.cfg.AppSecretEnv)
	if a.cfg.AppID == "" || secret == "" {
		return "", fmt.Errorf("feishu app_id or %s is not configured", a.cfg.AppSecretEnv)
	}
	return secret, nil
}

// runWebSocket 启动飞书 WebSocket 长连接。
func (a *adapter) runWebSocket(ctx context.Context) {
	secret, err := a.appSecret()
	if err != nil {
		a.logger.Error("feishu websocket config error", "err", err)
		return
	}
	eventHandler := a.newEventDispatcher()
	bot.RunWithRetry(ctx, a.logger, "feishu sdk websocket", bot.RetryConfig{}, func(ctx context.Context) error {
		opts := []larkws.ClientOption{
			larkws.WithEventHandler(eventHandler),
			larkws.WithLogLevel(larkcore.LogLevelError),
			larkws.WithAutoReconnect(true),
			larkws.WithOnReady(func() { a.logger.Info("feishu sdk websocket connected") }),
			larkws.WithOnReconnecting(func() { a.logger.Warn("feishu sdk websocket reconnecting") }),
			larkws.WithOnReconnected(func() { a.logger.Info("feishu sdk websocket reconnected") }),
			larkws.WithOnError(func(err error) { a.logger.Error("feishu sdk websocket error", "err", err) }),
		}
		if feishuDomain(a.cfg.Domain) == "lark" {
			opts = append(opts, larkws.WithDomain(lark.LarkBaseUrl))
		}
		client := larkws.NewClient(a.cfg.AppID, secret, opts...)
		a.wsClient = client
		// client.Start blocks; run it off-loop so cancellation closes the client
		// immediately rather than waiting for Start to notice ctx. RunWithRetry
		// handles the reconnect backoff.
		errCh := make(chan error, 1)
		go func() { errCh <- client.Start(ctx) }()
		select {
		case <-ctx.Done():
			client.Close()
			return nil
		case err := <-errCh:
			client.Close()
			return err
		}
	})
}

func (a *adapter) newEventDispatcher() *dispatcher.EventDispatcher {
	return dispatcher.NewEventDispatcher(a.cfg.VerificationToken, "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			a.handleSDKMessage(ctx, event)
			return nil
		}).
		OnP2MessageReadV1(func(ctx context.Context, event *larkim.P2MessageReadV1) error {
			return nil
		}).
		OnP2MessageReactionCreatedV1(func(ctx context.Context, event *larkim.P2MessageReactionCreatedV1) error {
			return nil
		}).
		OnP2MessageReactionDeletedV1(func(ctx context.Context, event *larkim.P2MessageReactionDeletedV1) error {
			return nil
		}).
		OnP2CardActionTrigger(func(ctx context.Context, event *callback.CardActionTriggerEvent) (*callback.CardActionTriggerResponse, error) {
			if event == nil || event.EventReq == nil || !a.handleCardAction(event.Body) {
				a.logger.Warn("feishu card action ignored", "reason", "invalid_payload")
				return cardActionToast("warning", "操作无效或已过期"), nil
			}
			return cardActionToast("success", "操作已提交"), nil
		})
}

func (a *adapter) handleSDKMessage(ctx context.Context, event *larkim.P2MessageReceiveV1) {
	if event == nil || event.Event == nil || event.Event.Message == nil {
		return
	}
	eventID := ""
	if event.EventV2Base != nil && event.EventV2Base.Header != nil {
		eventID = event.EventV2Base.Header.EventID
	}
	if eventID != "" {
		if a.markSeen(eventID) {
			return
		}
	}
	msg := event.Event.Message
	messageID := stringPtrValue(msg.MessageId)
	mentions := sdkMentionRefs(msg.Mentions)
	chatType := bot.ChatDM
	if stringPtrValue(msg.ChatType) == "group" || stringPtrValue(msg.ChatType) == "topic_group" {
		chatType = bot.ChatGroup
		if a.cfg.RequireMention && !a.mentionsBot(mentions) {
			a.logger.Info("feishu message ignored", "reason", "missing_mention", "chat", logHash(stringPtrValue(msg.ChatId)), "message", logHash(messageID))
			return
		}
	}
	msgType := stringPtrValue(msg.MessageType)
	text, media, ok := a.parseInboundContent(msgType, stringPtrValue(msg.Content), messageID)
	if !ok {
		a.logger.Info("feishu message ignored", "reason", "unsupported_type", "msg_type", msgType, "chat_type", stringPtrValue(msg.ChatType), "message", logHash(messageID))
		return
	}
	text = a.replaceMentionPlaceholders(text, mentions)
	if strings.TrimSpace(text) == "" && len(media) == 0 {
		a.logger.Info("feishu message ignored", "reason", "empty_after_parse", "msg_type", msgType, "message", logHash(messageID))
		return
	}
	userID := ""
	senderOpenID := ""
	if event.Event.Sender != nil && event.Event.Sender.SenderId != nil {
		senderOpenID = stringPtrValue(event.Event.Sender.SenderId.OpenId)
		userID = firstNonEmpty(
			senderOpenID,
			stringPtrValue(event.Event.Sender.SenderId.UnionId),
			stringPtrValue(event.Event.Sender.SenderId.UserId),
		)
	}
	userName := userID
	var resolveUserName func(context.Context) string
	if senderOpenID != "" {
		resolveUserName = func(ctx context.Context) string {
			return a.resolveUserName(ctx, senderOpenID)
		}
	}
	ib := bot.InboundMessage{
		Platform:        bot.PlatformFeishu,
		ChatType:        chatType,
		ChatID:          stringPtrValue(msg.ChatId),
		UserID:          userID,
		UserName:        userName,
		Text:            text,
		MessageID:       messageID,
		ThreadID:        stringPtrValue(msg.ThreadId),
		Media:           media,
		ResolveUserName: resolveUserName,
		Raw:             event,
	}
	select {
	case a.msgCh <- ib:
		a.logger.Info("feishu inbound queued", "chat_type", chatType, "msg_type", msgType, "chat", logHash(ib.ChatID), "user", logHash(ib.UserID), "message", logHash(ib.MessageID), "text_chars", len([]rune(ib.Text)), "media_items", len(media))
	default:
		a.logger.Warn("feishu message channel full")
	}
}

func (a *adapter) handleWSEvent(ctx context.Context, raw json.RawMessage) {
	var evt feishuEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		return
	}

	if a.markSeen(evt.Header.EventID) {
		return
	}

	switch evt.Header.EventType {
	case "im.message.receive_v1":
		var msg feishuMsgEvent
		if err := json.Unmarshal(evt.Event, &msg); err != nil {
			return
		}
		a.handleMessage(ctx, msg)
	}
}

func (a *adapter) handleCardAction(raw []byte) bool {
	var payload struct {
		Header feishuHeader `json:"header"`
		Event  struct {
			Operator struct {
				UserID     string `json:"user_id"`
				OpenID     string `json:"open_id"`
				UnionID    string `json:"union_id"`
				OperatorID struct {
					UserID  string `json:"user_id"`
					OpenID  string `json:"open_id"`
					UnionID string `json:"union_id"`
				} `json:"operator_id"`
			} `json:"operator"`
			Context struct {
				OpenMessageID string `json:"open_message_id"`
				OpenChatID    string `json:"open_chat_id"`
			} `json:"context"`
			Action struct {
				Value map[string]string `json:"value"`
			} `json:"action"`
		} `json:"event"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false
	}
	command := payload.Event.Action.Value["command"]
	if command == "" || payload.Event.Context.OpenChatID == "" {
		return false
	}
	if a.markSeen(payload.Header.EventID) {
		return true
	}
	chatType := cardActionChatType(payload.Event.Action.Value["chat_type"])
	operatorID := firstNonEmpty(
		payload.Event.Operator.OperatorID.UnionID,
		payload.Event.Operator.OperatorID.OpenID,
		payload.Event.Operator.OperatorID.UserID,
		payload.Event.Operator.UnionID,
		payload.Event.Operator.OpenID,
		payload.Event.Operator.UserID,
	)
	routeUserID := firstNonEmpty(payload.Event.Action.Value["user_id"], operatorID)
	ib := bot.InboundMessage{
		Platform:   bot.PlatformFeishu,
		ChatType:   chatType,
		ChatID:     payload.Event.Context.OpenChatID,
		UserID:     routeUserID,
		UserName:   routeUserID,
		OperatorID: operatorID,
		Text:       command,
		MessageID:  payload.Event.Context.OpenMessageID,
	}
	select {
	case a.msgCh <- ib:
	default:
		a.logger.Warn("feishu card action channel full")
	}
	return true
}

func (a *adapter) markSeen(eventID string) bool {
	if eventID == "" {
		return false
	}
	a.seenMu.Lock()
	defer a.seenMu.Unlock()
	if a.seen == nil {
		a.seen = make(map[string]bool)
	}
	if a.seen[eventID] {
		return true
	}
	a.seen[eventID] = true
	if len(a.seen) > 10000 {
		a.seen = make(map[string]bool)
		a.seen[eventID] = true
	}
	return false
}

func cardActionChatType(raw string) bot.ChatType {
	switch bot.ChatType(raw) {
	case bot.ChatDM, bot.ChatGroup, bot.ChatGuild, bot.ChatDirect, bot.ChatThread:
		return bot.ChatType(raw)
	default:
		return bot.ChatGroup
	}
}

func cardActionToast(toastType, content string) *callback.CardActionTriggerResponse {
	return &callback.CardActionTriggerResponse{
		Toast: &callback.Toast{
			Type:    toastType,
			Content: content,
		},
	}
}

func (a *adapter) verificationTokenValid(token string) bool {
	if a.cfg.VerificationToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(a.cfg.VerificationToken)) == 1
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func logHash(id string) string {
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:12]
}

// @mention gating：仅在群聊中检查是否 @了 bot

// SendText sends an interactive card with markdown content to a Feishu/Lark chat_id using the SDK.
// It is used by the desktop settings panel as an actual connection test.

// sendMessage 使用飞书/Lark SDK 以 Interactive Card (JSON 2.0) 发送消息。
// Card 内嵌 markdown 元素，支持 CommonMark 标准语法。
// 当卡片体积超过 30KB 限制（如大段代码），自动降级为纯文本消息。
// MediaURLs are bare filenames staged in an operator-configured outbound media
// root. URL fetching and arbitrary-path reads are intentionally unsupported.

// update_multi marks the card as a shared card that can be patched for
// all recipients after sending; without it Im.Message.Patch (used by
// EditMessage for streaming) is rejected, which would collapse
// streaming into a flood of new messages. See references/desktop-ui.

func isReplyFallbackError(err error) bool {
	var apiErr *feishuAPIError
	return errors.As(err, &apiErr) && apiErr.op == "reply" && apiErr.code == feishuReplyRecalledCode
}

// sdkClient lazily builds the shared lark client. It is called concurrently —
// the fetchBotOpenID goroutine, per-message resolveUserName, and per-resource
// downloads all race on first use at startup — so the check-and-build is guarded
// by clientMu (a bare a.client read/write would data-race, tripping -race).

// 带触发消息 ID 时用 Reply 引用回复：话题群里回复会落到对应话题，
// 普通群里带引用上下文。只有飞书明确返回“消息已撤回”时才回退普通
// 发送；传输错误的提交结果不确定，回退 Create 可能产生重复消息。

// Stable across retries so a retry after a post-commit connection drop does
// not send a duplicate visible message (Feishu dedups on uuid).

// sendCard 发送 interactive card 消息（用于审批/问答）。

// runWebhook 启动飞书 Webhook 模式。
