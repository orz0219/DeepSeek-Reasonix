package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reasonix/internal/agent"
	"reasonix/internal/billing"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"strings"
	"time"
)

// ListProjectTree builds the sidebar tree: project folders each containing
// their topics, plus a Global section.
// topicSummary is used by ListProjectTree and mergeSessionInfos to track
// per-topic turn count and last activity.
type topicSummary struct {
	turns                int
	adoptedRecoveryTurns int
	lastActivityAt       int64
	hasNormalSession     bool
	hasRecoveryOnly      bool
	hasAdoptedRecovery   bool
}

// runtimeSessionStatus is one open or detached runtime session, as shown in
// the sidebar tree.
type runtimeSessionStatus struct {
	open    bool
	running bool
}

// topicHiddenAsRecoveryOnly hides topics whose only on-disk sessions are
// conflict-recovery copies: they stay reachable from History, but must not sit
// in the tree as regular conversations. Pinned topics and topics with any
// open or running runtime session remain visible — note topicRuntimeStatus
// reports open/running only for single-session topics, so it must not gate
// topic existence.
func topicHiddenAsRecoveryOnly(summary topicSummary, pinned bool, runtimeSessions []runtimeSessionStatus) bool {
	if !summary.hasRecoveryOnly || summary.hasNormalSession || summary.hasAdoptedRecovery || pinned {
		return false
	}
	for _, session := range runtimeSessions {
		if session.open || session.running {
			return false
		}
	}
	return true
}

func topicSummaryKey(scope, workspaceRoot, topicID string) string {
	if scope == "global" {
		return "global::" + topicID
	}

	return "project:" + projectRootKey(workspaceRoot) + ":" + topicID
}

func projectSessionNodeKey(scope, sessionPath string) string {
	sum := sha256.Sum256([]byte(sessionRuntimeKey(sessionPath)))
	return scope + "_session_" + hex.EncodeToString(sum[:8])
}

// ContextPanelInfo is the right-side panel's data for one tab.
type ContextPanelInfo struct {
	UsedTokens       int  `json:"usedTokens"`
	WindowTokens     int  `json:"windowTokens"`
	PromptTokens     int  `json:"promptTokens"`
	CompletionTokens int  `json:"completionTokens"`
	TotalTokens      int  `json:"totalTokens"`
	ReasoningTokens  int  `json:"reasoningTokens"`
	CacheHitTokens   int  `json:"cacheHitTokens"`
	CacheMissTokens  int  `json:"cacheMissTokens"`
	Estimated        bool `json:"estimated,omitempty"`
	// Session-cumulative token counts (from telemetry, atomic snapshot).
	// Separate from the per-turn fields above so existing consumers (status bar
	// turn tokens, donut chart) are unaffected.
	SessionCacheHitTokens   int                         `json:"sessionCacheHitTokens"`
	SessionCacheMissTokens  int                         `json:"sessionCacheMissTokens"`
	SessionCompletionTokens int                         `json:"sessionCompletionTokens"`
	SessionEstimated        bool                        `json:"sessionEstimated,omitempty"`
	RequestCount            int                         `json:"requestCount"`
	ElapsedMs               int64                       `json:"elapsedMs"`
	SessionCost             float64                     `json:"sessionCost"`
	SessionCurrency         string                      `json:"sessionCurrency,omitempty"`
	SessionCostUsd          float64                     `json:"sessionCostUsd,omitempty"`
	SessionCostComplete     bool                        `json:"sessionCostComplete,omitempty"`
	SessionCostEstimated    bool                        `json:"sessionCostEstimated,omitempty"`
	SessionBillingMode      string                      `json:"sessionBillingMode,omitempty"`
	SessionCostQuote        *billing.CostQuote          `json:"sessionCostQuote,omitempty"`
	Sources                 map[string]usageSourceStats `json:"sources,omitempty"`
	Mock                    bool                        `json:"mock,omitempty"`
	ReadFiles               []readFileRecord            `json:"readFiles"`
	ChangedFiles            []ChangedFileInfo           `json:"changedFiles"`
}

type ChangedFileInfo struct {
	Path         string   `json:"path"`
	OldPath      string   `json:"oldPath,omitempty"`
	Sources      []string `json:"sources"`
	GitStatus    string   `json:"gitStatus,omitempty"`
	Turns        []int    `json:"turns"`
	LatestPrompt string   `json:"latestPrompt,omitempty"`
	LatestTime   int64    `json:"latestTime,omitempty"`
}

// ContextPanel returns the context usage, read files, and changed files for a
// specific tab.
func (a *App) ContextPanel(tabID string) ContextPanelInfo {
	a.mu.RLock()
	tab, ok := a.tabs[tabID]
	var ctrl control.SessionAPI
	if ok && tab != nil {
		ctrl = tab.Ctrl
	}
	a.mu.RUnlock()
	if !ok {
		return ContextPanelInfo{ReadFiles: []readFileRecord{}, ChangedFiles: []ChangedFileInfo{}}
	}

	info := ContextPanelInfo{ReadFiles: []readFileRecord{}, ChangedFiles: []ChangedFileInfo{}}
	if ctrl != nil {
		if sp := ctrl.SessionPath(); sp != "" {
			tab.syncTelemetryToSession(sp)
		}
		_, window := ctrl.ContextSnapshot()
		info.WindowTokens = window

		if u := ctrl.LastUsage(); u != nil {
			info.UsedTokens = u.PromptTokens + u.CompletionTokens
		}
		if info.UsedTokens == 0 {
			if snap := tab.displayTelemetrySnapshot(); snap.Usage.LastUsedTokens > 0 {
				info.UsedTokens = snap.Usage.LastUsedTokens
			}
		}
		if u := ctrl.LastUsage(); u != nil {
			info.PromptTokens = u.PromptTokens
			info.CompletionTokens = u.CompletionTokens
			info.ReasoningTokens = u.ReasoningTokens
			info.CacheHitTokens = u.CacheHitTokens
			info.CacheMissTokens = u.CacheMissTokens
			info.Estimated = u.Estimated
		} else {

			snap := tab.displayTelemetrySnapshot()
			info.PromptTokens = snap.Usage.LastPromptTokens
			info.CompletionTokens = snap.Usage.LastCompletionTokens
			info.ReasoningTokens = snap.Usage.LastReasoningTokens
			info.CacheHitTokens = snap.Usage.LastCacheHitTokens
			info.CacheMissTokens = snap.Usage.LastCacheMissTokens
			info.Estimated = snap.Usage.LastEstimated
		}
	}

	telemetry := tab.displayTelemetrySnapshot()
	if records := telemetry.ReadFiles; records != nil {
		info.ReadFiles = records
	}
	usage := telemetry.Usage
	info.TotalTokens = usage.TotalTokens
	info.RequestCount = usage.RequestCount
	info.ElapsedMs = usage.ElapsedMs
	info.SessionCost = usage.SessionCost
	info.SessionCurrency = usage.SessionCurrency
	info.SessionCostUsd = usage.SessionCostUsd
	info.SessionCostComplete = usage.SessionCostComplete
	info.SessionCostEstimated = true
	info.SessionCostQuote = usage.SessionCostQuote
	if usage.SessionCostQuote != nil {
		info.SessionBillingMode = usage.SessionCostQuote.BillingMode
		info.SessionCostEstimated = usage.SessionCostQuote.Estimated
		if !usage.SessionCostQuote.Complete {
			info.SessionCostComplete = false
		}
	}
	info.Sources = usage.Sources
	info.SessionCacheHitTokens = usage.CacheHitTokens
	info.SessionCacheMissTokens = usage.CacheMissTokens
	info.SessionCompletionTokens = usage.CompletionTokens
	info.SessionEstimated = usage.Estimated

	if ctrl != nil && tab.WorkspaceRoot != "" {
		for _, meta := range ctrl.Checkpoints() {
			for _, path := range meta.Paths {
				info.ChangedFiles = append(info.ChangedFiles, ChangedFileInfo{
					Path:         path,
					Sources:      []string{"session"},
					Turns:        []int{meta.Turn},
					LatestPrompt: meta.Prompt,
					LatestTime:   meta.Time.UnixMilli(),
				})
			}
		}
	}

	return info
}

func (a *App) newUniqueTabIDLocked() string {
	for {
		id := newTabID()
		if _, exists := a.tabs[id]; !exists {
			return id
		}
	}
}

func (a *App) restoredTabIDLocked(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return a.newUniqueTabIDLocked()
	}
	if _, exists := a.tabs[id]; exists {
		return a.newUniqueTabIDLocked()
	}
	return id
}

func normalizeTabMode(mode string) string {
	switch mode {
	case "plan", "yolo", "plan-yolo", "yolo-plan":
		if mode == "yolo-plan" {
			return "plan-yolo"
		}
		return mode
	default:
		return "normal"
	}
}

func tabModeFromAxes(plan, autoApproveTools bool) string {
	switch {
	case plan && autoApproveTools:
		return "plan-yolo"
	case plan:
		return "plan"
	case autoApproveTools:
		return "yolo"
	default:
		return "normal"
	}
}

func tabModeHasPlan(mode string) bool {
	switch normalizeTabMode(mode) {
	case "plan", "plan-yolo":
		return true
	default:
		return false
	}
}

func tabModeHasAutoApproveTools(mode string) bool {
	switch normalizeTabMode(mode) {
	case "yolo", "plan-yolo":
		return true
	default:
		return false
	}
}

func currentTabMode(tab *WorkspaceTab) string {
	if tab == nil {
		return "normal"
	}
	if tab.Ctrl != nil {
		return tabModeFromAxes(tab.Ctrl.PlanMode(), tab.Ctrl.AutoApproveTools())
	}
	return normalizeTabMode(tab.mode)
}

func currentTabGoal(tab *WorkspaceTab) string {
	if tab == nil {
		return ""
	}
	if tab.Ctrl != nil {
		return tab.Ctrl.Goal()
	}
	return strings.TrimSpace(tab.goal)
}

func currentTabGoalStatus(tab *WorkspaceTab) string {
	if tab == nil {
		return control.GoalStatusStopped
	}
	if tab.Ctrl != nil {
		return tab.Ctrl.GoalStatus()
	}
	if strings.TrimSpace(tab.goal) != "" {
		return control.GoalStatusRunning
	}
	return control.GoalStatusStopped
}

func currentTabCollaborationMode(tab *WorkspaceTab) string {
	if tab == nil {
		return "normal"
	}
	if tabModeHasPlan(currentTabMode(tab)) {
		return "plan"
	}
	if strings.TrimSpace(currentTabGoal(tab)) != "" && currentTabGoalStatus(tab) == control.GoalStatusRunning {
		return "goal"
	}
	return "normal"
}

func currentTabToolApprovalMode(tab *WorkspaceTab) string {
	if tab == nil {
		return control.ToolApprovalAsk
	}
	if tab.Ctrl != nil {
		return tab.Ctrl.ToolApprovalMode()
	}
	return normalizeToolApprovalMode(tab.toolApprovalMode)
}

func currentTabTokenMode(tab *WorkspaceTab) string {
	if tab == nil {
		return boot.TokenModeFull
	}
	return boot.NormalizeTokenMode(tab.tokenMode)
}

// tabRuntimeSnapshot is a consistent under-a.mu copy of the per-tab fields
// that bound methods and rebuild paths need after releasing the lock. The
// build/rebuild goroutines write these fields under a.mu, so lock-free reads
// from other goroutines are data races (same class as the sessionLease race
// fixed for #5955). Controller methods are invoked on the snapshot's ctrl
// AFTER unlocking, never while holding a.mu.
type tabRuntimeSnapshot struct {
	ctrl             control.SessionAPI
	sink             *tabEventSink
	label            string
	ready            bool
	readOnly         bool
	startupErr       string
	scope            string
	workspaceRoot    string
	sessionPath      string
	topicID          string
	topicTitle       string
	sharedHostKey    string
	model            string
	effort           *string
	tokenMode        string
	mode             string
	goal             string
	toolApprovalMode string
}

// normalizedTabRuntime is the internal, orthogonal runtime profile restored
// across controller rebuilds. Goal sidecars remain authoritative; legacyGoal is
// only a fallback for a running legacy Goal with no sidecar.
type normalizedTabRuntime struct {
	collaborationMode string
	toolApprovalMode  string
	tokenMode         string
	legacyGoal        string
}

// snapshotTabRuntimeLocked copies the racy per-tab fields. Callers must hold
// a.mu (read or write side).
func snapshotTabRuntimeLocked(tab *WorkspaceTab) tabRuntimeSnapshot {
	if tab == nil {
		return tabRuntimeSnapshot{}
	}
	return tabRuntimeSnapshot{
		ctrl:             tab.Ctrl,
		sink:             tab.sink,
		label:            tab.Label,
		ready:            tab.Ready,
		readOnly:         tab.ReadOnly,
		startupErr:       tab.StartupErr,
		scope:            tab.Scope,
		workspaceRoot:    tab.WorkspaceRoot,
		sessionPath:      tab.SessionPath,
		topicID:          tab.TopicID,
		topicTitle:       tab.TopicTitle,
		sharedHostKey:    tab.SharedHostKey,
		model:            tab.model,
		effort:           cloneStringPtr(tab.effort),
		tokenMode:        tab.tokenMode,
		mode:             tab.mode,
		goal:             tab.goal,
		toolApprovalMode: tab.toolApprovalMode,
	}
}

func (a *App) tabRuntimeSnapshot(tab *WorkspaceTab) tabRuntimeSnapshot {
	if tab == nil {
		return tabRuntimeSnapshot{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return snapshotTabRuntimeLocked(tab)
}

func (s tabRuntimeSnapshot) currentMode() string {
	if s.ctrl != nil {
		return tabModeFromAxes(s.ctrl.PlanMode(), s.ctrl.AutoApproveTools())
	}
	return normalizeTabMode(s.mode)
}

func (s tabRuntimeSnapshot) currentGoal() string {
	if s.ctrl != nil {
		return s.ctrl.Goal()
	}
	return strings.TrimSpace(s.goal)
}

func (s tabRuntimeSnapshot) currentGoalStatus() string {
	if s.ctrl != nil {
		return s.ctrl.GoalStatus()
	}
	if strings.TrimSpace(s.goal) != "" {
		return control.GoalStatusRunning
	}
	return control.GoalStatusStopped
}

func (s tabRuntimeSnapshot) collaborationMode() string {
	if tabModeHasPlan(s.currentMode()) {
		return "plan"
	}
	if strings.TrimSpace(s.currentGoal()) != "" && s.currentGoalStatus() == control.GoalStatusRunning {
		return "goal"
	}
	return "normal"
}

func (s tabRuntimeSnapshot) currentToolApprovalMode() string {
	if s.ctrl != nil {
		return s.ctrl.ToolApprovalMode()
	}
	return normalizeToolApprovalMode(s.toolApprovalMode)
}

func (s tabRuntimeSnapshot) currentTokenMode() string {
	return boot.NormalizeTokenMode(s.tokenMode)
}

// normalizedRuntime reads live Controller state only after the App snapshot has
// released a.mu. Rebuild callers hold turnStartMu while invoking it, so all
// three axes and the legacy Goal fallback describe one admitted runtime state.
func (s tabRuntimeSnapshot) normalizedRuntime() normalizedTabRuntime {
	plan := tabModeHasPlan(normalizeTabMode(s.mode))
	approvalMode := normalizeToolApprovalMode(s.toolApprovalMode)
	goal := strings.TrimSpace(s.goal)
	goalStatus := control.GoalStatusStopped
	if goal != "" {
		goalStatus = control.GoalStatusRunning
	}
	if s.ctrl != nil {
		plan = s.ctrl.PlanMode()
		approvalMode = normalizeToolApprovalMode(s.ctrl.ToolApprovalMode())
		goal = strings.TrimSpace(s.ctrl.Goal())
		goalStatus = s.ctrl.GoalStatus()
	}

	runtime := normalizedTabRuntime{
		collaborationMode: "normal",
		toolApprovalMode:  approvalMode,
		tokenMode:         boot.NormalizeTokenMode(s.tokenMode),
	}
	switch {
	case plan:
		runtime.collaborationMode = "plan"
	case goal != "" && goalStatus == control.GoalStatusRunning:
		runtime.collaborationMode = "goal"
		runtime.legacyGoal = goal
	}
	return runtime
}

func (r normalizedTabRuntime) tabMode() string {
	return tabModeFromAxes(r.collaborationMode == "plan", r.toolApprovalMode == control.ToolApprovalYolo)
}

func applyNormalizedRuntimeToTabLocked(tab *WorkspaceTab, runtime normalizedTabRuntime) {
	if tab == nil {
		return
	}
	tab.mode = runtime.tabMode()
	tab.toolApprovalMode = normalizeToolApprovalMode(runtime.toolApprovalMode)
	tab.tokenMode = boot.NormalizeTokenMode(runtime.tokenMode)
	if runtime.collaborationMode == "goal" {
		tab.goal = strings.TrimSpace(runtime.legacyGoal)
	} else {
		tab.goal = ""
	}
}

func persistedTabTokenMode(mode string) string {
	mode = boot.NormalizeTokenMode(mode)
	if mode == boot.TokenModeEconomy || mode == boot.TokenModeDelivery {
		return mode
	}
	return ""
}

func normalizeToolApprovalMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case control.ToolApprovalAuto:
		return control.ToolApprovalAuto
	case control.ToolApprovalYolo, "full", "full-access", "bypass":
		return control.ToolApprovalYolo
	default:
		return control.ToolApprovalAsk
	}
}

func persistedToolApprovalMode(mode string) string {
	switch normalizeToolApprovalMode(mode) {
	case control.ToolApprovalAuto, control.ToolApprovalYolo:
		return normalizeToolApprovalMode(mode)
	default:
		return ""
	}
}

// persistedTabMode is the composer mode saved with a tab so it survives reload
// and app relaunch. plan, yolo, and plan-yolo are remembered (a restored yolo
// tab keeps its status-bar indicator); "normal" is the default and isn't
// persisted. (#3517)
func persistedTabMode(mode string) string {
	switch normalizeTabMode(mode) {
	case "plan", "yolo", "plan-yolo":
		return normalizeTabMode(mode)
	}
	return ""
}

func newTabID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UTC()
		return "tab_" + now.Format("20060102150405") + "_" + fmt.Sprintf("%09d", now.Nanosecond())
	}
	return "tab_" + hex.EncodeToString(b[:])
}

func newTopicID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UTC()
		return "topic_" + now.Format("20060102-150405") + "_" + fmt.Sprintf("%09d", now.Nanosecond())
	}
	return "topic_" + time.Now().UTC().Format("20060102-150405") + "_" + hex.EncodeToString(b[:])
}

func globalWorkspaceRoot() string {
	return filepath.Join(desktopConfigDir(), "global-workspace")
}

func ensureGlobalWorkspaceRoot() (string, error) {
	root := globalWorkspaceRoot()
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return root, nil
}

func globalTabWorkspaceRoot() string {
	root, err := ensureGlobalWorkspaceRoot()
	if err != nil {
		return globalWorkspaceRoot()
	}
	return root
}

func loadPinnedTabSessionWithPreload(dir, sessionPath string, preloaded loadedTabSession) (*agent.Session, string, bool, error) {
	return loadPinnedTabSessionWithPreloadAndMigrationFallback(dir, sessionPath, preloaded, false)
}

func loadPinnedTabSessionWithPreloadAndMigrationFallback(dir, sessionPath string, preloaded loadedTabSession, allowMigrationFallback bool) (*agent.Session, string, bool, error) {
	path, ok := pinnedTabSessionPath(dir, sessionPath)
	if !ok && allowMigrationFallback {
		path, ok = migratedPinnedTabSessionPath(dir, sessionPath)
	}
	if !ok {
		return nil, "", false, nil
	}
	if agent.IsCleanupPending(path) {
		return nil, "", false, nil
	}
	if preloaded.matches(path) {
		if preloaded.Session != nil && len(preloaded.Session.Snapshot()) == 0 {
			return nil, path, true, nil
		}
		return preloaded.Session, path, true, nil
	}
	loaded, err := agent.LoadSession(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, path, true, nil
		}
		return nil, path, true, err
	}

	if len(loaded.Snapshot()) == 0 {
		return nil, path, true, nil
	}
	return loaded, path, true, nil
}

func migratedPinnedTabSessionPath(dir, sessionPath string) (string, bool) {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" || dir == "" || !filepath.IsAbs(sessionPath) {
		return "", false
	}
	if _, err := os.Stat(sessionPath); err == nil || !os.IsNotExist(err) {
		return "", false
	}
	base := filepath.Base(sessionPath)
	if base == "." || base == string(filepath.Separator) || !strings.HasSuffix(base, ".jsonl") {
		return "", false
	}
	path, _, err := validateSessionPath(dir, filepath.Join(dir, base))
	if err != nil {
		return "", false
	}
	return path, true
}

func pinnedTabSessionPath(dir, sessionPath string) (string, bool) {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" || dir == "" {
		return "", false
	}
	path, _, err := validateSessionPath(dir, sessionPath)
	if err != nil {
		if filepath.IsAbs(sessionPath) {
			return "", false
		}
		base := filepath.Base(sessionPath)
		if base == "." || base == string(filepath.Separator) || !strings.HasSuffix(base, ".jsonl") {
			return "", false
		}
		path, _, err = validateSessionPath(dir, filepath.Join(dir, base))
		if err != nil {
			return "", false
		}
	}
	return path, true
}

func pinnedTabSessionPathForBuild(scope, workspaceRoot, targetDir, sessionPath string) (string, bool) {
	if path, ok := pinnedTabSessionPath(targetDir, sessionPath); ok {
		return path, true
	}

	legacyDir := config.SessionDir()
	path, ok := pinnedTabSessionPath(legacyDir, sessionPath)
	if !ok {
		return "", false
	}
	meta, hasMeta, err := agent.LoadBranchMeta(path)
	if strings.TrimSpace(scope) == "project" {
		if err != nil || !hasMeta || meta.Scope != "project" || !sameProjectRoot(meta.WorkspaceRoot, workspaceRoot) {
			return "", false
		}
	} else if err == nil && hasMeta && meta.Scope == "project" {
		return "", false
	}
	return path, true
}
