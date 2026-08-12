package main

import (
	"context"
	"errors"
	"maps"
	"reasonix/internal/agent"
	"reasonix/internal/billing"
	"reasonix/internal/boot"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// WorkspaceTab

// tabDisplayState follows one live runtime across visible, detached, and
// reattached WorkspaceTab wrappers. Keeping one shared state pointer closes the
// handoff window where an event already routed to the old wrapper could append
// after a clone copied its buffers.
type tabDisplayState struct {
	mu             sync.Mutex
	planner        displayTurnBuffer
	executor       displayTurnBuffer
	pendingWrites  []*pendingDisplayWrite
	persistRunning bool
}

const displayPersistRetryLimit = 4

var errNoDesktopChatModel = errors.New("no desktop chat model is available; add a chat-capable provider in Settings > Model > Access")

type pendingDisplayWrite struct {
	dir         string
	sessionPath string
	userContent string
	messages    []HistoryMessage
	persist     func(string, string, string, []HistoryMessage) error
}

// displayTextAccumulator retains provider chunks without repeatedly copying
// the complete prefix. A turn only materializes the final string when its
// display-only history is persisted; successful executor turns are discarded
// without ever joining their chunks.
type displayTextAccumulator struct {
	parts []string
	size  int
}

func (a *displayTextAccumulator) append(text string) {
	if text == "" {
		return
	}
	a.parts = append(a.parts, text)
	a.size += len(text)
}

func (a *displayTextAccumulator) replace(text string) {
	a.parts = nil
	a.size = 0
	a.append(text)
}

func (a *displayTextAccumulator) hasNonWhitespace() bool {
	for _, part := range a.parts {
		if strings.TrimSpace(part) != "" {
			return true
		}
	}
	return false
}

func (a *displayTextAccumulator) string() string {
	switch len(a.parts) {
	case 0:
		return ""
	case 1:
		return a.parts[0]
	}
	var out strings.Builder
	out.Grow(a.size)
	for _, part := range a.parts {
		out.WriteString(part)
	}
	return out.String()
}

type bufferedHistoryMessage struct {
	message   HistoryMessage
	content   displayTextAccumulator
	reasoning displayTextAccumulator
}

func (m *bufferedHistoryMessage) materialize() HistoryMessage {
	out := m.message
	if out.Role == "assistant" {
		out.Content = m.content.string()
		out.Reasoning = m.reasoning.string()
	}
	if len(out.MemoryCitations) > 0 {
		out.MemoryCitations = append([]provider.MemoryCitation(nil), out.MemoryCitations...)
	}
	if len(out.ToolCalls) > 0 {
		out.ToolCalls = append([]HistoryToolCall(nil), out.ToolCalls...)
	}
	return out
}

type displayTurnBuffer struct {
	messages []*bufferedHistoryMessage
	tools    map[string]string
}

func (b *displayTurnBuffer) reset() {
	b.messages = nil
	b.tools = nil
}

func (b *displayTurnBuffer) materialize() []HistoryMessage {
	if len(b.messages) == 0 {
		return nil
	}
	out := make([]HistoryMessage, 0, len(b.messages))
	for _, message := range b.messages {
		out = append(out, message.materialize())
	}
	return out
}

// WorkspaceTab is one open conversation tab in the desktop. Each tab owns an
// independent controller (its own agent, session, tool registry, plugin host,
// memory, permissions) scoped to a workspace root, so multiple projects and
// topics can be active concurrently without interfering.
type WorkspaceTab struct {
	ID                  string             // stable random id
	Scope               string             // "project" | "global"
	WorkspaceRoot       string             // project root dir (empty for global)
	SharedHostKey       string             // opaque key for the shared plugin host (set by buildTabController)
	TopicID             string             // topic within the project
	TopicTitle          string             // display title
	topicTitleSource    string             // auto or manual; controls localization at API boundaries
	SessionPath         string             // exact .jsonl file this tab continues
	SessionGeneration   uint64             // bumps on session rotation (clear/new); frontend hydrate identity
	ReadOnly            bool               // true for external channel transcripts opened for browsing
	Ctrl                control.SessionAPI // nil while booting / on error
	Label               string             // model label (for the tab badge)
	Ready               bool               // true once boot.Build completes
	StartupErr          string             // build error, surfaced to the frontend
	StartupErrLeaseHeld bool               // true when StartupErr can be retried after a session lease releases
	runtimeID           string             // process-local SessionRuntime registry identity
	sessionLease        *agent.SessionLease
	sessionLeaseMu      sync.Mutex
	sessionLeaseKey     atomic.Pointer[string] // lock-free mirror; updated with sessionLease under sessionLeaseMu
	sink                *tabEventSink          // routes events with this tab's ID
	buildCancel         context.CancelFunc     // cancels in-flight boot for tabs removed before Ready
	buildGeneration     uint64                 // identifies the current in-flight build
	// buildDone is closed exactly once when the build that owns buildDoneGen
	// terminates (success, failure, or superseded abandon). Topic-activation
	// completions wait on it to learn that the controller build finished
	// without polling. Guarded by App.mu alongside buildGeneration; always
	// nil-ed after close so a replacement build can install a fresh channel.
	buildDone    chan struct{}
	buildDoneGen uint64
	removed      bool       // set when the visible tab is pruned/closed before build completes
	reconcileMu  sync.Mutex // serializes stale controller workspace repair for this tab
	turnStartMu  sync.Mutex // serializes foreground turn admission for this tab

	ActivityStatus string // transient project-tree status for the in-flight turn

	saveMu       sync.Mutex
	saving       bool
	saveAgain    bool
	saveFailures int
	// lastAutosaveWarnAt debounces the user-facing autosave-failure notice:
	// a persistently failing disk (AV hold, full volume) otherwise emits a
	// chat warning for every completed turn. Logs are never debounced.
	lastAutosaveWarnAt time.Time

	// closing is set under saveMu when the tab is being torn down. Once set,
	// tabSnapshotLoop stops taking new snapshot work and CloseTab waits on
	// saveCond until any in-flight snapshot finishes - so no background
	// snapshot can write a session file back to disk after CloseTab returns.
	// Without this, deleting a just-closed session races that write and the
	// session "resurrects" (#4384).
	closing  bool
	saveCond *sync.Cond

	// readTelemetry tracks files read during this tab's session.
	readTelemetry  []readFileRecord
	usageTelemetry sessionUsageStats
	// runtimeCostQuote is an automatic wallet-currency hint for the live tab.
	// It is deliberately outside usageTelemetry so it cannot be persisted into
	// telemetry/history or become configuration. Guarded by telemMu.
	runtimeCostDisplayCurrency string
	runtimeCostQuote           *billing.CostQuote
	runtimeCostGeneration      uint64 // invalidates stale wallet responses
	// telemetrySessionKey is the sessionRuntimeKey the telemetry above belongs
	// to. Controller-side session rotations (typed /new, bot /reset) bypass the
	// App bindings, so telemetry writers and readers re-key through
	// syncTelemetryToSession before trusting the in-memory totals — otherwise a
	// previous session's cost keeps accumulating under the new session and gets
	// persisted into its sidecar (#5850).
	telemetrySessionKey string
	telemMu             sync.Mutex

	// Display-only output belongs to the live runtime, not a particular visible
	// tab wrapper. detach/reattach paths share this state before rebinding the
	// event sink so output cannot fall into a discarded wrapper.
	displayStateMu sync.Mutex
	displayState   *tabDisplayState

	model            string // active model ref (for meta)
	effort           *string
	tokenMode        string
	mode             string // "normal" | "plan" | "yolo" | "plan-yolo"; yolo/full access is runtime-only
	goal             string
	toolApprovalMode string
	disabledMCP      map[string]ServerView
	mcpOrder         []string
	lastBuildResult  *boot.BuildResult // incremental extension reload

	// metaExtras caches the expensive MetaForTab fields (git branch, image
	// input capability) computed off the request path by
	// refreshTabMetaExtras. Lock-free reads keep MetaForTab synchronous and
	// cheap; refresh dedup goes through metaExtrasRefreshing.
	metaExtras           atomic.Pointer[tabMetaExtras]
	metaExtrasRefreshing atomic.Bool
}

const (
	topicStatusThinking            = "thinking"
	topicStatusStreaming           = "streaming"
	topicStatusWaitingConfirmation = "waiting_confirmation"
	topicStatusBackgroundJob       = "background_job"
	topicStatusPaused              = "paused"
	topicStatusError               = "error"
	// topicStatusDivergedRecovery marks a topic holding two or more independent
	// recovery branches. It is informational: the user picks which to keep, so
	// it must not gate archiving the way live runtime states do.
	topicStatusDivergedRecovery = "diverged_recovery"
)

type readFileRecord struct {
	Path      string `json:"path"`
	Turn      int    `json:"turn"`
	Time      int64  `json:"time"`
	Offset    int    `json:"offset,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type sessionUsageStats struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
	ReasoningTokens  int `json:"reasoningTokens"`
	CacheHitTokens   int `json:"cacheHitTokens"`
	CacheMissTokens  int `json:"cacheMissTokens"`
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`
	// CacheWriteBilledTokens preserves provider-specific cache-write pricing
	// across persisted telemetry repricing without changing hit-rate totals.
	CacheWriteBilledTokens float64 `json:"cacheWriteBilledTokens,omitempty"`
	Estimated              bool    `json:"estimated,omitempty"`
	// LastUsedTokens is the executor-reported context fill (prompt+completion)
	// from the most recent turn. It is persisted so the status bar / context
	// panel can show a meaningful fill percentage after a session rebind
	// rebuilds the controller (which resets the in-memory executor state).
	LastUsedTokens int `json:"lastUsedTokens,omitempty"`
	// Per-turn token breakdown from the most recent turn. Persisted separately
	// from the cumulative totals above so the context-panel donut chart and
	// type breakdown survive a session rebind (which resets executor.LastUsage).
	LastPromptTokens     int     `json:"lastPromptTokens,omitempty"`
	LastCompletionTokens int     `json:"lastCompletionTokens,omitempty"`
	LastReasoningTokens  int     `json:"lastReasoningTokens,omitempty"`
	LastCacheHitTokens   int     `json:"lastCacheHitTokens,omitempty"`
	LastCacheMissTokens  int     `json:"lastCacheMissTokens,omitempty"`
	LastEstimated        bool    `json:"lastEstimated,omitempty"`
	RequestCount         int     `json:"requestCount"`
	ElapsedMs            int64   `json:"elapsedMs"`
	SessionCost          float64 `json:"sessionCost,omitempty"`
	SessionCurrency      string  `json:"sessionCurrency,omitempty"`
	SessionCostUsd       float64 `json:"sessionCostUsd,omitempty"`
	// SessionCostComplete is false when any entry lacks a shared display valuation.
	SessionCostComplete bool `json:"sessionCostComplete,omitempty"`
	// CostLedger stores occurrence-time quotes keyed by model+source+fingerprint+rateDate.
	CostLedger *billing.Ledger `json:"costLedger,omitempty"`
	// SessionCostQuote is the aggregate quote for the current display currency.
	SessionCostQuote *billing.CostQuote          `json:"sessionCostQuote,omitempty"`
	Sources          map[string]usageSourceStats `json:"sources,omitempty"`

	activeTurnStartedAt int64
	sourceSessionCache  map[string]sourceSessionCacheCounters
}

type usageSourceStats struct {
	PromptTokens           int     `json:"promptTokens"`
	CompletionTokens       int     `json:"completionTokens"`
	TotalTokens            int     `json:"totalTokens"`
	ReasoningTokens        int     `json:"reasoningTokens"`
	CacheHitTokens         int     `json:"cacheHitTokens"`
	CacheMissTokens        int     `json:"cacheMissTokens"`
	CacheWriteTokens       int     `json:"cacheWriteTokens,omitempty"`
	CacheWriteBilledTokens float64 `json:"cacheWriteBilledTokens,omitempty"`
	Estimated              bool    `json:"estimated,omitempty"`
	RequestCount           int     `json:"requestCount"`
	SessionCost            float64 `json:"sessionCost,omitempty"`
	SessionCurrency        string  `json:"sessionCurrency,omitempty"`
	SessionCostUsd         float64 `json:"sessionCostUsd,omitempty"`
}

type sourceSessionCacheCounters struct {
	Hit  int
	Miss int
}

func cloneSessionUsageStats(in sessionUsageStats) sessionUsageStats {
	out := in
	if len(in.Sources) > 0 {
		out.Sources = make(map[string]usageSourceStats, len(in.Sources))
		maps.Copy(out.Sources, in.Sources)
	}
	if len(in.sourceSessionCache) > 0 {
		out.sourceSessionCache = make(map[string]sourceSessionCacheCounters, len(in.sourceSessionCache))
		maps.Copy(out.sourceSessionCache, in.sourceSessionCache)
	}
	return out
}

func (s *sessionUsageStats) cacheTokenDelta(source string, u *provider.Usage, sessionHit, sessionMiss int) (hit, miss int) {
	if u != nil {
		hit = u.CacheHitTokens
		miss = u.CacheMissTokens
	}
	if source != event.UsageSourceExecutor && source != event.UsageSourcePlanner {
		return hit, miss
	}
	if sessionHit+sessionMiss <= 0 {
		return hit, miss
	}
	if s.sourceSessionCache == nil {
		s.sourceSessionCache = map[string]sourceSessionCacheCounters{}
	}
	prev, ok := s.sourceSessionCache[source]
	s.sourceSessionCache[source] = sourceSessionCacheCounters{Hit: sessionHit, Miss: sessionMiss}
	if !ok {
		return sessionHit, sessionMiss
	}
	if sessionHit < prev.Hit || sessionMiss < prev.Miss {
		if hit+miss > 0 {
			return hit, miss
		}
		return sessionHit, sessionMiss
	}
	return sessionHit - prev.Hit, sessionMiss - prev.Miss
}

type tabTelemetrySnapshot struct {
	Version   int               `json:"version"`
	ReadFiles []readFileRecord  `json:"readFiles"`
	Usage     sessionUsageStats `json:"usage"`
}

func cloneStringPtr(v *string) *string {
	if v == nil {
		return nil
	}
	cp := *v
	return &cp
}

func cloneServerViewMap(in map[string]ServerView) map[string]ServerView {
	out := make(map[string]ServerView, len(in))
	for name, view := range in {
		view.EnvKeys = append([]string(nil), view.EnvKeys...)
		view.HeaderKeys = append([]string(nil), view.HeaderKeys...)
		out[name] = view
	}
	return out
}

// Recovery handoff is two-phase: the desktop callback acquires the new
// lease and updates SessionPath before Controller commits its own path. The
// lease-backed tab path is authoritative during that window; otherwise a
// concurrent, newer tab-layout save can overwrite the recovery anchor with
// the controller's old path. Outside a handoff, keep the controller-first
// behavior so an unleased/stale tab field cannot mask the live runtime.

// sessionRuntimeKey is the comparison/map key for "same session" checks. It
// layers agent.CanonicalSessionPath on top of the desktop path normalization
// so the key matches the form held by session leases (lowercased on Windows).
// Comparing a lease's Path() against a raw tab path without this fold made
// every rebuild on Windows look like a foreign holder (self-lock, #5999).
// Keys are identities only — never use them as display or file paths.

// takeSessionLease removes and returns the tab's current lease WITHOUT
// releasing it, so ownership can transfer to another holder. All access to
// t.sessionLease must go through sessionLeaseMu; never read or assign the
// field directly outside these helpers.

// adoptSessionLease installs lease as the tab's session lease, releasing any
var desktopProjectsFileMu sync.Mutex

// ordered topic IDs

// saveTabsCollectLocked gathers the tab-snapshot data under the caller's lock
// (it calls orderedTabIDsLocked which requires a.mu). Returns the config dir,
// the serializable entries, the active tab ID, and a monotonic snapshot version.
// The write can happen outside the lock to avoid blocking the UI with disk I/O.

// saveTabsWrite writes the tab-snapshot to disk. It does not require a.mu, but
// writes must be serialized because every save uses the same destination and
// fixed .tmp path.

// Single-topic prepends are intentional writes (topic creation, a live tab
// indexing its session, restore from trash): they clear any delete
// tombstone so the topic fully returns instead of landing in a half-state
// where only its title resurfaces.

// Batch prepends come from the legacy migration and index-repair scans:
// they must respect delete tombstones so a scan never resurrects a topic
// the user removed.

// Tombstones are checked under the projects-file lock: a DeleteTopic
// that lands between a scan reading DeletedTopics and this write must
// not be resurrected by the stale batch.

// Dedupe roots against a roots-only list: out also holds the global order
// token, which must never be path-compared against project roots.

// topic helpers

const (
	topicTitlesFile        = "desktop-topic-titles.json"
	topicTitleSourcesFile  = "desktop-topic-title-sources.json"
	topicCreatedAtsFile    = "desktop-topic-created-at.json"
	topicAutoTitlesFile    = "desktop-topic-auto-title-meta.json"
	defaultTopicTitle      = "新的会话"
	defaultTopicTitleEn    = "New session"
	defaultTopicTitleZhTW  = "新的會話"
	topicTitleSourceAuto   = "auto"
	topicTitleSourceManual = "manual"
)

const (
	desktopLocaleUnknown int32 = iota
	desktopLocaleEn
	desktopLocaleZh
	desktopLocaleZhTW
)

// Same read-boundary cleaning as loadSessionTitles: older builds could
// persist titles carrying internal wrappers (#5666).
