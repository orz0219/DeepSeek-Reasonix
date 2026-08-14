package main

import (
	"context"
	"encoding/json"
	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/eventwire"
	"reasonix/internal/provider"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// tabEventSink wraps a parent event.Sink and prepends a tabId to every wire
// event so the frontend can route it to the correct tab's reducer.
//
// tabID and app are rebound while the controller keeps emitting when a running
// session is detached to the background or reattached to another tab, so they
// live under mu like ctx does (a bare field write would data-race Emit). Read
// them via binding(), write via setBinding().
type tabEventSink struct {
	tabID         string
	app           *App
	mu            sync.RWMutex
	ctx           context.Context
	runtimeEpoch  string
	runtimeEvents asyncRuntimeEmitter
	turn          turnSubmissionState // stays reserved through the end of TurnDone fan-out
}

type closeableEventSink interface {
	Close()
}

// binding snapshots the sink's current tab routing under the sink lock.
func (s *tabEventSink) binding() (string, *App) {
	if s == nil {
		return "", nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tabID, s.app
}

func (s *tabEventSink) runtimeEpochSnapshot() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runtimeEpoch
}

func (s *tabEventSink) Emit(e event.Event) {

	if s == nil {
		return
	}
	if e.Kind == event.TurnStarted {
		s.mu.Lock()
		s.turn.inFlight = true
		s.mu.Unlock()
	}
	tabID, app := s.binding()
	if app != nil {
		if e.Kind == event.TurnDone {

			app.reconcileWorkspaceForTab(tabID)
		}
		switch e.Kind {
		case event.TurnStarted:
			s.resetDisplayTurn()
			s.recordTurnStarted()
		case event.Usage:
			s.recordUsageTelemetry(e)
		case event.TurnDone:
			s.recordTurnDone()
		}
		if e.Kind == event.TurnDone {
			s.flushDisplay(e.Cancelled)
		}
	}
	s.emitRuntimeEvent(eventChannel, toWireTabWithSubmission(e, tabID, s.runtimeEpochSnapshot(), s.submissionIDSnapshot()))
	if app != nil {
		if status, update := topicActivityStatusFromEvent(e); update {
			changed := app.setTabActivityStatus(tabID, status)
			if changed || isBackgroundJobLifecycleNotice(e) {
				app.emitProjectTreeMetadataChanged()
			}
		}
	}

	if e.Kind == event.ToolResult && e.Tool.Name == "read_file" && e.Tool.Err == "" {
		s.recordReadTelemetry(e)
	}
	if app != nil {
		s.recordDisplay(e)
	}

	if e.Kind == event.TurnDone && app != nil {
		app.scheduleTabSnapshot(tabID)
	}

	if e.Kind == event.TurnDone {
		s.mu.Lock()
		s.turn = turnSubmissionState{}
		s.mu.Unlock()
	}
}

// It is safe to call concurrently with Emit.
// turn. A delayed TurnDone must not detach a replacement installed meanwhile.

// tryBeginTurn reserves the tab until its TurnDone has finished fan-out. The
// controller clears RuntimeStatus().Running before it emits TurnDone, so the
// controller status alone leaves a window where a new turn can inherit the old
// turn's forwarder or have its replacement cleared by the old completion.
func (s *tabEventSink) tryBeginTurn(submissionID ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn.inFlight {
		return false
	}
	s.turn = turnSubmissionState{inFlight: true, submissionID: firstSubmissionID(submissionID)}
	return true
}

func (s *tabEventSink) cancelTurnStart() {
	s.mu.Lock()
	s.turn = turnSubmissionState{}
	s.mu.Unlock()
}

func (s *tabEventSink) setContext(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
}

func (s *tabEventSink) context() context.Context {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ctx
}

func (s *tabEventSink) emitRuntimeEvent(name string, payload ...any) {
	if s == nil {
		return
	}
	ctx := s.context()
	if ctx == nil {
		return
	}
	s.runtimeEvents.Emit(ctx, name, payload...)
}

type runtimeEventEmitFunc func(context.Context, string, ...any)

type runtimeEventEnvelope struct {
	ctx     context.Context
	name    string
	payload []any
}

// asyncRuntimeEmitter decouples Wails' runtime event bridge from agent emission.
// runtime.EventsEmit can block when the single webview event channel backs up;
// callers enqueue in-order work and return without holding the agent event lock.
// runtimeEventsEmitFallback is the emit used when no per-instance override is
// installed. Production keeps the real Wails bridge; the test binary swaps in
// a no-op via TestMain, because Wails EventsEmit log.Fatals outside a running
// Wails app and would kill the whole test process from any code path that
// emits a runtime event with a plain Background context.
var runtimeEventsEmitFallback runtimeEventEmitFunc = runtime.EventsEmit

type asyncRuntimeEmitter struct {
	mu                     sync.Mutex
	emit                   runtimeEventEmitFunc
	queue                  []runtimeEventEnvelope
	head                   int
	running                bool
	configWarningsRevision atomic.Uint64
}

func (e *asyncRuntimeEmitter) Emit(ctx context.Context, name string, payload ...any) {
	if ctx == nil {
		return
	}
	item := runtimeEventEnvelope{
		ctx:     ctx,
		name:    name,
		payload: append([]any(nil), payload...),
	}
	e.mu.Lock()
	e.queue = append(e.queue, item)
	if !e.running {
		e.running = true
		go e.run()
	}
	e.mu.Unlock()
}

func (e *asyncRuntimeEmitter) Clear() {
	e.mu.Lock()
	clear(e.queue)
	e.queue = nil
	e.head = 0
	e.mu.Unlock()
}

func (e *asyncRuntimeEmitter) run() {
	for {
		e.mu.Lock()
		if e.head >= len(e.queue) {
			clear(e.queue)
			e.queue = nil
			e.head = 0
			e.running = false
			e.mu.Unlock()
			return
		}
		item := e.queue[e.head]
		var zero runtimeEventEnvelope
		e.queue[e.head] = zero
		e.head++
		if e.head > 64 && e.head*2 >= len(e.queue) {
			e.queue = append([]runtimeEventEnvelope(nil), e.queue[e.head:]...)
			e.head = 0
		}
		emit := e.emit
		if emit == nil {
			emit = runtimeEventsEmitFallback
		}
		e.mu.Unlock()

		emit(item.ctx, item.name, item.payload...)
	}
}

func topicActivityStatusFromEvent(e event.Event) (string, bool) {
	switch e.Kind {
	case event.TurnStarted, event.Reasoning, event.ToolDispatch, event.ToolProgress, event.ToolResult, event.CompactionStarted, event.CompactionDone, event.Retrying:
		return topicStatusThinking, true
	case event.Text, event.Message:
		return topicStatusStreaming, true
	case event.ApprovalRequest, event.AskRequest:
		return topicStatusWaitingConfirmation, true
	case event.TurnDone:
		if e.Outcome == event.TurnOutcomeFinalReadiness || e.Outcome == event.TurnOutcomeRecoveryPaused {

			return topicStatusPaused, true
		}
		if e.Err != nil {
			return topicStatusError, true
		}
		return "", true
	case event.Notice:
		if isBackgroundJobLifecycleNotice(e) {
			return "", true
		}
		return "", false
	default:
		return "", false
	}
}

func isBackgroundJobLifecycleNotice(e event.Event) bool {
	if e.Kind != event.Notice {
		return false
	}
	text := strings.TrimSpace(e.Text)
	return strings.HasPrefix(text, "background ") &&
		(strings.Contains(text, " started: ") ||
			strings.Contains(text, " finished: ") ||
			strings.Contains(text, " failed: ") ||
			strings.Contains(text, " killed: "))
}

// notifyTabRuntimeRebuilt tells the frontend a tab's controller was replaced
// in place (model/effort/token-mode switch, clear-while-running). A rebuilt
// controller restarts its approval/ask id counter at "1", so tab-scoped
// frontend state keyed by prompt id (the attention-chime dedupe) must reset —
// unlike agent:ready, this event carries no reload semantics, so emitting it
// on every swap adds no hydration churn.
//
// Ordering matters: the reset must reach the frontend BEFORE the rebuilt
// controller's first approval/ask event, or the stale key still mutes it. The
// tab's agent events ride the tab sink's own async queue, so the notice goes
// through THAT queue — same lane, FIFO, guaranteed to arrive first. The
// App-level queue is only the fallback when the sink cannot deliver (no sink,
// or its webview context is cleared); it cannot order against sink traffic,
// but an unordered notice still beats none.
func (a *App) notifyTabRuntimeRebuilt(tab *WorkspaceTab) {
	if tab == nil {
		return
	}
	a.mu.Lock()
	epoch := a.advanceSessionRuntimeEpochLocked(tab)
	a.mu.Unlock()
	a.notifyTabRuntimeRebuiltAtEpoch(tab, epoch)
}

// notifyTabRuntimeRebuiltAtEpoch emits the rebuild fence for a transaction
// that advanced its epoch inside the controller/path/lease commit. Keeping the
// chosen epoch avoids a second generation bump after publication.
func (a *App) notifyTabRuntimeRebuiltAtEpoch(tab *WorkspaceTab, epoch string) {
	if tab == nil {
		return
	}
	a.mu.RLock()
	sink := tab.sink
	tabID := tab.ID
	a.mu.RUnlock()
	if sink != nil && sink.context() != nil {
		sink.emitRuntimeEvent("runtime:rebuilt", tabID, epoch)
		return
	}
	a.emitRuntimeEvent("runtime:rebuilt", tabID, epoch)
}

func (a *App) emitReady(ctx context.Context, tabID ...string) {
	a.mu.RLock()
	hook := a.readyHook
	a.mu.RUnlock()
	if hook != nil {
		hook()
		return
	}
	if ctx != nil {
		if len(tabID) > 0 && strings.TrimSpace(tabID[0]) != "" {
			a.runtimeEvents.Emit(ctx, "agent:ready", strings.TrimSpace(tabID[0]))
			return
		}
		a.runtimeEvents.Emit(ctx, "agent:ready")
	}
}

func (s *tabEventSink) recordReadTelemetry(e event.Event) {
	tabID, app := s.binding()
	if app == nil {
		return
	}
	app.mu.RLock()
	tab := app.tabByEventSinkIDLocked(tabID)
	var ctrl control.SessionAPI
	if tab != nil {
		ctrl = tab.Ctrl
	}
	app.mu.RUnlock()
	if tab == nil {
		return
	}
	turn := 0
	if ctrl != nil {
		turn = ctrl.Turn()
	}

	// Parse read_file args: {"path": "...", "offset": N, "limit": N}
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	path := e.Tool.Args
	offset := 0
	limit := 0
	if err := json.Unmarshal([]byte(e.Tool.Args), &args); err == nil && args.Path != "" {
		path = args.Path
		offset = args.Offset
		limit = args.Limit
	}

	truncated := e.Tool.Truncated || strings.Contains(e.Tool.Output, "truncated") ||
		strings.Contains(e.Tool.Output, "File truncated")

	sp := ""
	if ctrl != nil {
		sp = ctrl.SessionPath()
	}
	if sp != "" {
		tab.syncTelemetryToSession(sp)
	}
	tab.recordReadFile(readFileRecord{
		Path:      path,
		Turn:      turn,
		Time:      time.Now().UnixMilli(),
		Offset:    offset,
		Limit:     limit,
		Truncated: truncated,
	})
	if sp != "" {
		_ = saveTelemetry(sp+".telemetry.json", tab.telemetrySnapshot())
	}
}

func (s *tabEventSink) recordUsageTelemetry(e event.Event) {
	tab, sp := s.telemetryTab()
	if tab == nil {
		return
	}
	if sp != "" {
		tab.syncTelemetryToSession(sp)
	}
	tab.recordUsage(e)
	if sp != "" {
		_ = saveTelemetry(sp+".telemetry.json", tab.telemetrySnapshot())
	}
}

func (s *tabEventSink) recordDisplay(e event.Event) {
	tab, _ := s.eventTabAndController()
	if tab != nil {
		tab.recordDisplayEvent(e)
	}
}

func (s *tabEventSink) flushDisplay(cancelRequested bool) {
	tab, ctrl := s.eventTabAndController()
	if tab == nil || ctrl == nil {
		return
	}
	history := ctrl.History()
	keepExecutorDisplay := cancelRequested && (lastHistoryMessageIsUser(history) || hasPendingInterruptedRecovery(history))
	messages := tab.takeDisplayTurn(keepExecutorDisplay)
	if len(messages) == 0 {
		return
	}
	sessionPath := ctrl.SessionPath()
	if sessionPath == "" {
		return
	}
	userContent := lastUserMessageContent(history)
	if strings.TrimSpace(userContent) == "" {
		return
	}
	persistOrEnqueueDisplayWrite(tab.displayBufferState(), &pendingDisplayWrite{
		dir:         controllerSessionDir(ctrl),
		sessionPath: sessionPath,
		userContent: userContent,
		messages:    messages,
		persist:     recordSessionPlannerDisplay,
	})
}

func lastHistoryMessageIsUser(history []provider.Message) bool {
	return len(history) > 0 && history[len(history)-1].Role == provider.RoleUser
}

func hasPendingInterruptedRecovery(history []provider.Message) bool {
	for _, v := range slices.Backward(history) {
		m := v
		if m.LocalOnly && m.InterruptedTurn != nil {
			return m.InterruptedTurn.Pending
		}
		if m.Role == provider.RoleUser {
			return false
		}
	}
	return false
}

func (s *tabEventSink) eventTabAndController() (*WorkspaceTab, control.SessionAPI) {
	tabID, app := s.binding()
	if app == nil {
		return nil, nil
	}
	app.mu.RLock()
	defer app.mu.RUnlock()
	tab := app.tabByEventSinkIDLocked(tabID)
	if tab == nil {
		return nil, nil
	}
	return tab, tab.Ctrl
}

func lastUserMessageContent(msgs []provider.Message) string {
	for _, v := range slices.Backward(msgs) {
		if v.Role == provider.RoleUser {
			return agent.UserMessageText(v)
		}
	}
	return ""
}

func (s *tabEventSink) telemetryTab() (*WorkspaceTab, string) {
	tabID, app := s.binding()
	if app == nil {
		return nil, ""
	}
	app.mu.RLock()
	tab := app.tabByEventSinkIDLocked(tabID)
	var ctrl control.SessionAPI
	if tab != nil {
		ctrl = tab.Ctrl
	}
	app.mu.RUnlock()
	if tab == nil {
		return nil, ""
	}
	if ctrl == nil {
		return tab, ""
	}
	sp := ctrl.SessionPath()
	if sp == "" {
		return tab, ""
	}
	return tab, sp
}

func toWireTab(e event.Event, tabID string, runtimeEpoch ...string) wireEventTab {
	w := eventwire.ToWire(e)
	epoch := ""
	if len(runtimeEpoch) > 0 {
		epoch = runtimeEpoch[0]
	}
	return wireEventTab{
		Event:             w,
		TabID:             tabID,
		RuntimeEpoch:      epoch,
		SessionHitTokens:  e.SessionHit,
		SessionMissTokens: e.SessionMiss,
		SessionCost:       0,
		SessionCurrency:   "",
		SessionCostUsd:    0,
	}
}

// wireEventTab extends the shared event wire with tab routing info. The frontend reducer
// uses tabId to dispatch to the correct per-tab state.
type wireEventTab struct {
	eventwire.Event
	TabID        string `json:"tabId"`
	RuntimeEpoch string `json:"runtimeEpoch,omitempty"`
	// Session-cumulative tokens per tab.
	SessionHitTokens  int `json:"sessionHitTokens,omitempty"`
	SessionMissTokens int `json:"sessionMissTokens,omitempty"`
	// SessionCost is filled by the frontend's per-tab accumulator.
	SessionCost     float64 `json:"sessionCost,omitempty"`
	SessionCurrency string  `json:"sessionCurrency,omitempty"`
	// SessionCostUsd is a deprecated compatibility alias. It mirrors
	// SessionCost and does not imply USD.
	SessionCostUsd float64 `json:"sessionCostUsd,omitempty"`
}

func (a *App) emitRuntimeEvent(name string, payload ...any) {
	if a == nil || a.ctx == nil {
		return
	}
	a.runtimeEvents.Emit(a.ctx, name, payload...)
}
