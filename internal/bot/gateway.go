package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"reasonix/internal/control"
	"reasonix/internal/event"
)

// GatewayConfig 是 BotGateway 的配置。
type GatewayConfig struct {
	Model             string
	ToolApprovalMode  string
	MaxSteps          int
	QueueMode         string
	QueueCap          int
	QueueDrop         string
	PairingEnabled    bool
	PairingTTL        time.Duration
	PairingMaxPending int
	// IgnoreSelfMessages drops messages that are clearly sent by this bot. It
	// uses configured SelfUserIDs plus recently returned outbound message IDs.
	IgnoreSelfMessages bool
	SelfUserIDs        map[Platform][]string
	ControlEnabled     bool
	ControlAddr        string
	ControlToken       string
	// ApprovalTimeout bounds how long a tool-approval/ask prompt blocks a bot
	// session waiting for a remote user's reply. Zero falls back to
	// defaultBotApprovalTimeout so an abandoned prompt can't wedge the bot forever
	// (#4626, #4402). A negative value disables the timeout (wait indefinitely).
	ApprovalTimeout    time.Duration
	WorkspaceRoot      string
	Channels           map[Platform]ChannelConfig
	ConnectionChannels map[string]ChannelConfig
	Routes             []RouteConfig
	ConnectionAccess   map[string]AccessConfig
	Allowlist          AllowlistConfig
	Enabled            map[Platform]bool
	Debounce           time.Duration
	// OnInbound observes every allowlisted inbound message before dispatch.
	//
	// Reentrancy contract for all GatewayConfig callbacks (OnInbound,
	// OnSessionReady, OnToolApprovalModeChange): they run synchronously on
	// gateway-owned dispatch/turn goroutines; OnSessionReady can also run on a
	// controller recovery/autosave goroutine. Stop drains all of those paths
	// before returning. A callback must therefore never call Stop, nor block
	// until a goroutine that does so completes — Stop would wait on the very
	// goroutine running the callback, a guaranteed deadlock. Hosts that want to
	// shut the gateway down in reaction to a callback must trigger the shutdown
	// asynchronously.
	OnInbound func(InboundMessage)
	// OnSessionReady notifies the host after the bot has created, reused, or
	// recovered the controller for an inbound remote. Hosts may persist the
	// concrete session ID or keep the remote as a read-only channel.
	OnSessionReady func(InboundMessage, string) error
	// OnToolApprovalModeChange persists a remote IM request such as /yolo on.
	// The gateway updates the live session and in-memory defaults first; this
	// callback lets desktop save the chosen connection mode to user config.
	OnToolApprovalModeChange func(InboundMessage, string) error
	// Desktop, when the gateway is embedded in the desktop app, gives bot
	// chats a god view over desktop sessions (/desktop commands): global
	// status, event subscriptions, and remote approvals for any live desktop
	// session. Nil when the gateway runs standalone (reasonix bot start).
	Desktop DesktopBridge
}

// ChannelConfig overrides gateway defaults for one IM channel.
type ChannelConfig struct {
	Model            string
	ToolApprovalMode string
	WorkspaceRoot    string
	SessionMappings  []SessionMapping
}

// SessionMapping is the runtime subset of a saved bot connection mapping used
// to route a remote chat/user/thread back to its intended workspace.
type SessionMapping struct {
	RemoteID      string
	SessionID     string
	SessionSource string
	ChatType      string
	UserID        string
	ThreadID      string
	Scope         string
	WorkspaceRoot string
	UpdatedAt     string
}

// RouteConfig applies per-remote overrides. Empty match fields are wildcards;
// the first matching route wins.
type RouteConfig struct {
	ConnectionID string
	Platform     Platform
	ChatType     ChatType
	ChatID       string
	UserID       string
	ThreadID     string
	Channel      ChannelConfig
}

// AdapterBinding attaches an adapter instance to one saved bot connection.
// Feishu and Lark share PlatformFeishu, so ID/Domain keep their sessions,
// replies, and per-connection settings separated at runtime.
type AdapterBinding struct {
	ID       string
	Domain   string
	Platform Platform
	Adapter  Adapter
}

// AllowlistConfig 控制哪些用户/群可以使用 bot。
type AllowlistConfig struct {
	Enabled   bool
	AllowAll  bool
	Users     map[Platform][]string
	Approvers map[Platform][]string
	Admins    map[Platform][]string
	Groups    map[Platform][]string
}

// AccessConfig controls who may use one concrete bot connection.
type AccessConfig struct {
	Enabled        bool
	AllowAll       bool
	PairingEnabled bool
	Users          []string
	Groups         []string
	Approvers      []string
	Admins         []string
}

// AdapterHealthSnapshot describes the gateway's current view of one adapter.
type AdapterHealthSnapshot struct {
	ID            string    `json:"id"`
	Platform      Platform  `json:"platform"`
	Domain        string    `json:"domain,omitempty"`
	Name          string    `json:"name,omitempty"`
	Status        string    `json:"status"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	LastMessageAt time.Time `json:"last_message_at,omitempty"`
	LastSendAt    time.Time `json:"last_send_at,omitempty"`
	LastErrorAt   time.Time `json:"last_error_at,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	Messages      int64     `json:"messages"`
	Sends         int64     `json:"sends"`
	SendErrors    int64     `json:"send_errors"`
	Closed        bool      `json:"closed"`
}

// BotGateway 是 reasonix bot 消息网关，管理 Controller 生命周期、session 并发、
// 事件渲染和平台适配器。
type BotGateway struct {
	cfg      GatewayConfig
	adapters []AdapterBinding
	sessions *SessionManager
	startErr []error

	lifecycleMu sync.Mutex
	started     bool
	stopped     bool
	runCancel   context.CancelFunc
	startDone   chan struct{}
	stopDone    chan struct{}
	gatewayWG   sync.WaitGroup
	turnWG      sync.WaitGroup

	mu                      sync.Mutex
	controllers             map[string]*sessionState // session key -> active state
	pendingReactionCleanups map[string][]func()
	allowlist               map[Platform]map[string]bool
	groupAllowlist          map[Platform]map[string]bool
	selfUserIDs             map[Platform]map[string]bool
	outboundMessageIDs      map[string]time.Time
	adapterHealth           map[string]*AdapterHealthSnapshot
	controlServer           *controlHTTPServer
	sessionOverrides        map[string]sessionRuntimeOverride

	logger *slog.Logger
}

// botController is the slice of the controller's driving port the gateway needs:
// session lifecycle, turn execution, and approval/ask handling. The bot never
// touches goals, checkpoints, or memory, so it depends on those sub-ports only —
// not the concrete *control.Controller and its ~99 methods.
type botController interface {
	control.Lifecycle
	control.TurnControl
	control.Approvals
}

type sessionState struct {
	lifecycleMu      sync.Mutex
	retired          bool
	ctrl             botController
	sink             *sessionEventSink
	leases           *control.SessionLeaseKeeper
	platform         Platform
	connectionID     string
	model            string
	workspaceRoot    string
	toolApprovalMode string
	sessionPath      string
	// mappingDegraded records that this state intentionally runs on a fresh
	// session because its session_mappings target could not be used at build
	// time. It keeps later messages (whose profile re-resolves the mapping)
	// from tearing the state down every turn; convergence back onto the
	// mapped file happens on the next gateway restart.
	mappingDegraded  bool
	cancel           context.CancelFunc
	pendingAsks      map[string][]event.AskQuestion
	pendingApprovals map[string]event.Approval
	lastApprovalID   string
	lastAskID        string
	createdAt        time.Time
	lastActive       time.Time
}

var errBotSessionRetired = errors.New("bot session retired during recovery")

type sessionRuntimeProfile struct {
	model            string
	workspaceRoot    string
	toolApprovalMode string
	sessionPath      string
	// sessionPathOptional marks sessionPath as a persisted session_mappings
	// binding rather than an explicit /attach: when the mapped file cannot be
	// loaded or leased, the session degrades to a fresh path instead of
	// dropping the message (#6917).
	sessionPathOptional bool
}

type sessionRuntimeOverride struct {
	channel     ChannelConfig
	sessionPath string
	label       string
}

type sessionEventSink struct {
	mu     sync.RWMutex
	target event.Sink
}

type pendingReactionAdapter interface {
	AddPendingReaction(ctx context.Context, messageID string) (func(), error)
}

const outboundEchoTTL = 10 * time.Minute

func (s *sessionEventSink) setTarget(target event.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.target = target
}

func (s *sessionEventSink) Emit(e event.Event) {
	s.mu.RLock()
	target := s.target
	s.mu.RUnlock()
	if target != nil {
		target.Emit(e)
	}
}

// NewGateway 创建一个新的 BotGateway。
func NewGateway(cfg GatewayConfig, adapters map[Platform]Adapter, logger *slog.Logger) *BotGateway {
	bindings := make([]AdapterBinding, 0, len(adapters))
	for plat, adapter := range adapters {
		bindings = append(bindings, AdapterBinding{ID: string(plat), Platform: plat, Adapter: adapter})
	}
	return NewGatewayWithAdapterBindings(cfg, bindings, logger)
}

// NewGatewayWithAdapterBindings creates a gateway with one or more adapter
// instances per platform.
func NewGatewayWithAdapterBindings(cfg GatewayConfig, adapters []AdapterBinding, logger *slog.Logger) *BotGateway {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.Debounce <= 0 {
		cfg.Debounce = 1500 * time.Millisecond
	}
	cfg.QueueMode = NormalizeQueueMode(cfg.QueueMode)
	if cfg.QueueCap <= 0 {
		cfg.QueueCap = DefaultQueueCap
	}
	cfg.QueueDrop = NormalizeQueueDrop(cfg.QueueDrop)
	if cfg.PairingTTL <= 0 {
		cfg.PairingTTL = defaultPairingTTL
	}
	if cfg.PairingMaxPending <= 0 {
		cfg.PairingMaxPending = defaultPairingMaxPending
	}
	gw := &BotGateway{
		cfg:                     cfg,
		adapters:                normalizeAdapterBindings(adapters),
		sessions:                NewSessionManager(cfg.Debounce),
		controllers:             make(map[string]*sessionState),
		pendingReactionCleanups: make(map[string][]func()),
		allowlist:               make(map[Platform]map[string]bool),
		groupAllowlist:          make(map[Platform]map[string]bool),
		selfUserIDs:             make(map[Platform]map[string]bool),
		outboundMessageIDs:      make(map[string]time.Time),
		adapterHealth:           make(map[string]*AdapterHealthSnapshot),
		sessionOverrides:        make(map[string]sessionRuntimeOverride),
		logger:                  logger.With("component", "bot_gateway"),
	}
	gw.buildAllowlist()
	gw.buildSelfUserIDs()
	for _, binding := range gw.adapters {
		gw.setAdapterConfigured(binding)
	}
	return gw
}

func normalizeAdapterBindings(adapters []AdapterBinding) []AdapterBinding {
	out := make([]AdapterBinding, 0, len(adapters))
	for _, binding := range adapters {
		if binding.Adapter == nil {
			continue
		}
		if binding.Platform == "" {
			binding.Platform = binding.Adapter.Platform()
		}
		if strings.TrimSpace(binding.ID) == "" {
			binding.ID = string(binding.Platform)
		}
		binding.ID = strings.TrimSpace(binding.ID)
		binding.Domain = strings.TrimSpace(binding.Domain)
		out = append(out, binding)
	}
	return out
}

func (gw *BotGateway) buildAllowlist() {
	for _, plat := range []Platform{PlatformQQ, PlatformFeishu, PlatformWeixin} {
		gw.allowlist[plat] = make(map[string]bool)
		if !gw.cfg.Allowlist.Enabled {
			continue
		}
		addAllowlistUsers(gw.allowlist[plat], gw.cfg.Allowlist.Users[plat])
		addAllowlistUsers(gw.allowlist[plat], gw.cfg.Allowlist.Admins[plat])
		addAllowlistUsers(gw.allowlist[plat], gw.cfg.Allowlist.Approvers[plat])
		gw.groupAllowlist[plat] = make(map[string]bool)
		for _, gid := range gw.cfg.Allowlist.Groups[plat] {
			gw.groupAllowlist[plat][gid] = true
		}
	}
}

func addAllowlistUsers(dst map[string]bool, users []string) {
	for _, uid := range users {
		uid = strings.TrimSpace(uid)
		if uid != "" {
			dst[uid] = true
		}
	}
}

func (gw *BotGateway) buildSelfUserIDs() {
	for _, plat := range []Platform{PlatformQQ, PlatformFeishu, PlatformWeixin} {
		gw.selfUserIDs[plat] = stringSet(gw.cfg.SelfUserIDs[plat])
	}
}

// Start 启动所有已启用的平台适配器并开始处理消息。
func (gw *BotGateway) Start(ctx context.Context) (err error) {
	gw.lifecycleMu.Lock()
	if gw.stopped {
		gw.lifecycleMu.Unlock()
		return errors.New("bot gateway already stopped")
	}
	if gw.started {
		gw.lifecycleMu.Unlock()
		return errors.New("bot gateway already started")
	}
	gw.started = true
	runCtx, cancel := context.WithCancel(ctx)
	gw.runCancel = cancel
	startDone := make(chan struct{})
	gw.startDone = startDone
	gw.lifecycleMu.Unlock()
	defer func() {
		if err != nil {
			cancel()
		}
		gw.lifecycleMu.Lock()
		if err != nil {
			gw.runCancel = nil
		}
		close(startDone)
		gw.lifecycleMu.Unlock()
	}()

	started := make([]AdapterBinding, 0, len(gw.adapters))
	var startErr []error
	for _, binding := range gw.adapters {
		if !gw.cfg.Enabled[binding.Platform] {
			gw.logger.Info("platform disabled, skipping", "platform", binding.Platform, "connection", binding.ID)
			gw.markAdapterDisabled(binding)
			continue
		}
		gw.logger.Info("starting adapter", "platform", binding.Platform, "connection", binding.ID, "domain", binding.Domain)
		if err := binding.Adapter.Start(runCtx); err != nil {
			wrapped := fmt.Errorf("start adapter %s: %w", binding.ID, err)
			startErr = append(startErr, wrapped)
			gw.markAdapterStartFailed(binding, err)
			gw.logger.Warn("adapter start failed", "platform", binding.Platform, "connection", binding.ID, "domain", binding.Domain, "err", err)
			continue
		}
		gw.markAdapterStarted(binding)
		started = append(started, binding)
	}
	// SendToAdapter reads gw.adapters under gw.mu; publish the started set under
	// the same lock.
	gw.mu.Lock()
	gw.adapters = started
	gw.startErr = startErr
	gw.mu.Unlock()
	if len(started) == 0 && len(startErr) > 0 {
		return errors.Join(startErr...)
	}
	if err := gw.startControlServer(runCtx); err != nil {
		for _, binding := range started {
			_ = binding.Adapter.Stop()
		}
		return err
	}

	// 合并所有适配器的消息通道
	for _, binding := range gw.adapters {
		gw.gatewayWG.Go(func() {
			gw.dispatchLoop(runCtx, binding)
		})
	}

	return nil
}

func (gw *BotGateway) AdapterCount() int {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	return len(gw.adapters)
}

func (gw *BotGateway) StartErrors() []error {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	out := make([]error, len(gw.startErr))
	copy(out, gw.startErr)
	return out
}

// AdapterHealth returns a stable snapshot of all configured adapter instances.

// Stop 停止所有适配器并关闭所有 session。它会等待 dispatch 与 turn goroutine
// 全部退出，所以绝不能在 GatewayConfig 回调里同步调用（见 OnInbound 的
// reentrancy contract），否则 Stop 会等待正在运行该回调的 goroutine 自己。
func (gw *BotGateway) Stop() {
	gw.lifecycleMu.Lock()
	if gw.stopped {
		stopDone := gw.stopDone
		gw.lifecycleMu.Unlock()
		if stopDone != nil {
			<-stopDone
		}
		return
	}
	gw.stopped = true
	stopDone := make(chan struct{})
	gw.stopDone = stopDone
	cancel := gw.runCancel
	gw.runCancel = nil
	startDone := gw.startDone
	gw.lifecycleMu.Unlock()
	defer close(stopDone)

	if cancel != nil {
		cancel()
	}
	if startDone != nil {
		<-startDone
	}

	// Cancel sessions that already exist before waiting for dispatch to drain.
	// A dispatch already inside handleMessage may still publish a late session,
	// so closeSessions is repeated after gatewayWG and turnWG reach zero.
	gw.closeSessions()
	for _, binding := range gw.adapters {
		if err := binding.Adapter.Stop(); err != nil {
			gw.logger.Warn("error stopping adapter", "platform", binding.Platform, "connection", binding.ID, "err", err)
		}
		gw.markAdapterClosed(binding)
	}
	gw.stopControlServer()
	gw.gatewayWG.Wait()
	gw.closeSessions()
	gw.turnWG.Wait()
	gw.closeSessions()
}

func (gw *BotGateway) closeSessions() {
	var states []*sessionState
	gw.mu.Lock()
	for key, state := range gw.controllers {
		states = append(states, state)
		delete(gw.controllers, key)
	}
	gw.mu.Unlock()
	for _, state := range states {
		gw.closeSessionState(state)
	}
}

// closeSessionState tears down a session state that has been unlinked from
// gw.controllers. runTurn publishes state.cancel under gw.mu on every turn —
// possibly after the state was already unlinked — so snapshot and clear the
// field inside the lock and invoke it outside (the same discipline as
// cancelActiveSession).
func (gw *BotGateway) closeSessionState(state *sessionState) {
	if state == nil {
		return
	}
	// Serialize retirement with recovery ownership handoffs. Stop unlinks
	// sessions before turn goroutines drain, so a recovery callback captured by
	// the controller can still arrive here. Marking the state retired under the
	// same lock prevents that callback from reacquiring a lease after teardown;
	// an already-running handoff completes before the lease is released below.
	state.lifecycleMu.Lock()
	if state.retired {
		state.lifecycleMu.Unlock()
		return
	}
	state.retired = true
	state.lifecycleMu.Unlock()

	gw.mu.Lock()
	cancel := state.cancel
	state.cancel = nil
	gw.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if state.ctrl != nil {
		state.ctrl.Close()
	}
	if state.leases != nil {
		state.leases.Release()
	}
}

// unlinkAndCloseSessionState removes state from the live gateway before closing
// it. It is used when a controller has already rotated its transcript but the
// replacement lease could not be acquired: retaining that state would let the
// next message reuse a controller that no longer owns its active session path.
func (gw *BotGateway) unlinkAndCloseSessionState(key string, state *sessionState) {
	if state == nil {
		return
	}
	gw.mu.Lock()
	if gw.controllers[key] == state {
		delete(gw.controllers, key)
	}
	gw.mu.Unlock()
	gw.closeSessionState(state)
}

// allowlist 检查

// 斜杠命令处理

// 已接管桌面会话的聊天：普通消息直接驱动那个桌面会话，不进 bot 自己的
// 会话机器（斜杠命令仍走上面的分支，/desktop release 永远可达）。

// Busy session: durable inbox is the authority (not SessionManager.pending).

// Slash commands still acquire through the session lock below.

// followup

// session 并发控制 — only the active-turn lock remains here; bodies live in inbox.

// never drop_old; capacity enforced by inbox

// Legacy fallback.

// state.cancel is rewritten under gw.mu on every turn (runTurn), so copy it
// inside the lock and invoke it outside.

// NewSession refuses to rotate while a turn is running; the cancel
// above is asynchronous, so give the turn a bounded window to
// unwind before rotating.

// /new leaves an attached transcript and continues in the freshly
// rotated path. Clear only the path pin while preserving any project
// override, otherwise the next message would rebuild the old attached
// transcript and silently undo the rotation.

// 从消息中解析 approval ID

// Recovery cards map allow → continue for older clients that only know Approve.

// Backward compatibility for cards rendered by an older client: reject
// the proposed mutation but leave task cancellation to ordinary /stop.

// God view over the embedding desktop app: listing every live desktop
// session and answering its approvals is strictly more power than the
// per-session approver role, so gate on admin.

// 获取或创建 Controller

// 构建输入文本：群聊中在消息前加上发送者名，并把 IM 媒体保存为 @附件引用。

// 发送"正在输入"状态

// 创建事件渲染 sink

// Finish initializing the sink before publishing it as the live target: once
// setTarget runs, other goroutines can reach this sink via state.sink.Emit.

// 创建带取消的 context

// The session was closed (gateway stop or runtime rebuild) after this
// turn picked it up; a cancel published now would never be consumed, so
// abort the turn instead of running it uncancellable.

// 运行一轮对话

// Create the lease owner before the controller so automatic conflict
// recovery can move ownership to the recovery branch before the controller
// commits to writing it. Without this callback the bot kept guarding the
// original path while continuing on an unleased recovery path.

// A mapped binding degrades to a fresh session on failure; only an
// explicit /attach is allowed to hard-fail the message, because the
// user named that exact session.

// Re-check under the lock: while we were off-lock in boot.Build, a second
// message for the same key may have built and registered its own session.
// Reuse it only when it still targets this message's runtime profile.

// A persisted session_mappings binding is the durable chat→session link
// the desktop writes into the connection config. Without consuming it
// here, every gateway restart or runtime rebuild opened a brand-new
// session file for the chat and the configured binding was display-only
// (#6917, #6934).

// sessionMappingPathForMessage resolves the persisted session_mappings entry
// for a message to an existing session file. Only bindings that resolve to a
// present, readable file participate — a moved or deleted target quietly
// degrades to normal session creation rather than blocking the chat.

// A state that already degraded off its mapped session keeps running on
// its fresh path even though the profile re-resolves the mapping each
// message; rebuilding here would spawn a new session per message while the
// mapped file stays unavailable.

// defaultBotApprovalTimeout caps how long a bot session waits for a remote
// user's approval/ask reply before treating it as denied, so an abandoned
// prompt (or a dropped IM event) can't leave the session wedged forever
// (#4626, #4402). 30 minutes is generous for a human reply yet bounded.

// approvalTimeout resolves the configured bot approval wait: zero uses the
// bounded default; a negative value opts out (wait indefinitely).

// botSessionRecoveredHandler keeps the controller path, its writer lease, and
// the remote-to-session mapping on the same recovery generation. The lease
// handoff runs first and is failure-atomic: if the recovery path is already
// owned, the controller stays on the original path and the old lease remains
// held. Mapping updates are limited to this exact sessionState so a late
// callback from a retired controller cannot overwrite its replacement.

// Keep the lease handoff and mapping publication atomic with respect to
// state retirement. In particular, never let a callback that outlives
// Stop reacquire a lease after closeSessionState has released it.

// cfg.ToolApprovalMode / Channels / ConnectionChannels are rewritten under
// gw.mu at runtime (/yolo, UpdateConnectionToolApprovalMode), so snapshot them
// under a short lock and resolve outside it — applyRuntimeOverrideOptions
// takes gw.mu itself. Copying the ChannelConfig value is enough: writers
// replace whole map entries and never mutate SessionMappings in place.

// UpdateConnectionToolApprovalMode updates the in-memory tool approval mode for
// a single bot connection without restarting the gateway. Empty mode clears the
// connection override, so existing sessions inherit the current gateway default.

// Update every active session that belongs to this connection.

// SendToAdapter sends a message through the adapter identified by connID.
// Returns an error if no matching adapter is found.

// SendTextToAdapter sends a plain text message through the adapter identified by connID.
