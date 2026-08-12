package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reasonix/internal/agent"
	"reasonix/internal/billing"
	"reasonix/internal/boot"
	"reasonix/internal/control"
	"reasonix/internal/fileutil"
	"strings"
	"sync"
	"time"
)

func (a *App) setDesktopLocale(locale string) {
	normalized := strings.ToLower(strings.TrimSpace(locale))
	switch {
	case strings.HasPrefix(normalized, "zh-tw"), strings.HasPrefix(normalized, "zh-hant"):
		a.desktopLocale.Store(desktopLocaleZhTW)
	case strings.HasPrefix(normalized, "zh"):
		a.desktopLocale.Store(desktopLocaleZh)
	default:
		a.desktopLocale.Store(desktopLocaleEn)
	}
}

func (a *App) localizedDefaultTopicTitle() string {
	switch a.desktopLocale.Load() {
	case desktopLocaleZh:
		return defaultTopicTitle
	case desktopLocaleZhTW:
		return defaultTopicTitleZhTW
	case desktopLocaleEn:
		return defaultTopicTitleEn
	default:
		return defaultTopicTitle
	}
}

func isDefaultTopicTitle(title string) bool {
	switch strings.TrimSpace(title) {
	case defaultTopicTitle, defaultTopicTitleEn, defaultTopicTitleZhTW:
		return true
	default:
		return false
	}
}

func (a *App) localizedTopicTitle(title, source string) string {
	if strings.TrimSpace(source) == topicTitleSourceAuto && isDefaultTopicTitle(title) {
		return a.localizedDefaultTopicTitle()
	}
	return title
}

func topicTitlesPath(workspaceRoot string) string {
	if workspaceRoot == "" {
		return filepath.Join(desktopConfigDir(), "global", topicTitlesFile)
	}
	return filepath.Join(workspaceRoot, ".reasonix", topicTitlesFile)
}

func topicTitleSourcesPath(workspaceRoot string) string {
	if workspaceRoot == "" {
		return filepath.Join(desktopConfigDir(), "global", topicTitleSourcesFile)
	}
	return filepath.Join(workspaceRoot, ".reasonix", topicTitleSourcesFile)
}

func topicCreatedAtsPath(workspaceRoot string) string {
	if workspaceRoot == "" {
		return filepath.Join(desktopConfigDir(), "global", topicCreatedAtsFile)
	}
	return filepath.Join(workspaceRoot, ".reasonix", topicCreatedAtsFile)
}

func topicAutoTitleMetaPath(workspaceRoot string) string {
	if workspaceRoot == "" {
		return filepath.Join(desktopConfigDir(), "global", topicAutoTitlesFile)
	}
	return filepath.Join(workspaceRoot, ".reasonix", topicAutoTitlesFile)
}

const topicFileReadTimeout = 200 * time.Millisecond

var readFileWithTimeoutSlots = make(chan struct{}, 16)

func readFileWithTimeout(path string, timeout time.Duration) ([]byte, error) {
	if timeout <= 0 {
		return readFileUTF8(path)
	}
	select {
	case readFileWithTimeoutSlots <- struct{}{}:
	default:
		return nil, fmt.Errorf("too many pending file reads")
	}
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, err := readFileUTF8(path)
		<-readFileWithTimeoutSlots
		ch <- result{data: data, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.data, r.err
	case <-timer.C:
		return nil, fmt.Errorf("timed out after %v reading %s", timeout, filepath.Base(path))
	}
}

func loadTopicTitles(workspaceRoot string) map[string]string {
	m := map[string]string{}
	b, err := readFileWithTimeout(topicTitlesPath(workspaceRoot), topicFileReadTimeout)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)

	for key, title := range m {
		m[key] = agent.UserPreviewText(title)
	}
	return m
}

func loadTopicTitleSources(workspaceRoot string) map[string]string {
	m := map[string]string{}
	b, err := readFileWithTimeout(topicTitleSourcesPath(workspaceRoot), topicFileReadTimeout)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func loadTopicCreatedAts(workspaceRoot string) map[string]int64 {
	m := map[string]int64{}
	b, err := readFileWithTimeout(topicCreatedAtsPath(workspaceRoot), topicFileReadTimeout)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

type topicAutoTitleMeta struct {
	Stage     int    `json:"stage,omitempty"`
	UserTurns int    `json:"userTurns,omitempty"`
	BasisHash string `json:"basisHash,omitempty"`
	UpdatedAt int64  `json:"updatedAt,omitempty"`
}

func loadTopicAutoTitleMeta(workspaceRoot string) map[string]topicAutoTitleMeta {
	m := map[string]topicAutoTitleMeta{}
	b, err := readFileWithTimeout(topicAutoTitleMetaPath(workspaceRoot), topicFileReadTimeout)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func loadStringMapForUpdate(path string) (map[string]string, error) {
	m := map[string]string{}
	b, err := readFileUTF8(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return map[string]string{}, nil
	}
	return m, nil
}

func loadTopicAutoTitleMetaForUpdate(workspaceRoot string) (map[string]topicAutoTitleMeta, error) {
	m := map[string]topicAutoTitleMeta{}
	path := topicAutoTitleMetaPath(workspaceRoot)
	b, err := readFileUTF8(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return map[string]topicAutoTitleMeta{}, nil
	}
	return m, nil
}

func loadInt64MapForUpdate(path string) (map[string]int64, error) {
	m := map[string]int64{}
	b, err := readFileUTF8(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil || m == nil {
		return map[string]int64{}, nil
	}
	return m, nil
}

func loadTopicTitlesForUpdate(workspaceRoot string) (map[string]string, error) {
	return loadStringMapForUpdate(topicTitlesPath(workspaceRoot))
}

func loadTopicTitleSourcesForUpdate(workspaceRoot string) (map[string]string, error) {
	return loadStringMapForUpdate(topicTitleSourcesPath(workspaceRoot))
}

func loadTopicCreatedAtsForUpdate(workspaceRoot string) (map[string]int64, error) {
	return loadInt64MapForUpdate(topicCreatedAtsPath(workspaceRoot))
}

// ensureTopicStateDir prepares the directory holding a topic-state file. A
// project file lives under the workspace root, so the directory is only created
// while that root still exists — otherwise deleting the folder outside Reasonix
// resurrects it on the next launch (#4566).
func ensureTopicStateDir(workspaceRoot, path string) error {
	if root := strings.TrimSpace(workspaceRoot); root != "" && !existingDirectory(root) {
		return fmt.Errorf("workspace root %q no longer exists", root)
	}
	return os.MkdirAll(filepath.Dir(path), 0o755)
}

func saveTopicTitles(workspaceRoot string, m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := topicTitlesPath(workspaceRoot)
	if err := ensureTopicStateDir(workspaceRoot, path); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return fileutil.ReplaceFile(tmp, path)
}

func saveTopicTitleSources(workspaceRoot string, m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := topicTitleSourcesPath(workspaceRoot)
	if err := ensureTopicStateDir(workspaceRoot, path); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return fileutil.ReplaceFile(tmp, path)
}

func saveTopicCreatedAts(workspaceRoot string, m map[string]int64) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := topicCreatedAtsPath(workspaceRoot)
	if err := ensureTopicStateDir(workspaceRoot, path); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return fileutil.ReplaceFile(tmp, path)
}

func saveTopicAutoTitleMeta(workspaceRoot string, m map[string]topicAutoTitleMeta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := topicAutoTitleMetaPath(workspaceRoot)
	if err := ensureTopicStateDir(workspaceRoot, path); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return fileutil.ReplaceFile(tmp, path)
}

func loadTopicTitle(workspaceRoot, topicID string) string {
	return loadTopicTitles(workspaceRoot)[topicID]
}

func loadTopicTitleSource(workspaceRoot, topicID string) string {
	return loadTopicTitleSources(workspaceRoot)[topicID]
}

func loadTopicCreatedAt(workspaceRoot, topicID string) int64 {
	return loadTopicCreatedAts(workspaceRoot)[topicID]
}

func topicIDCreatedAt(topicID string) int64 {
	topicID = strings.TrimSpace(topicID)
	for _, prefix := range []string{"topic_", "legacy_"} {
		if !strings.HasPrefix(topicID, prefix) {
			continue
		}
		stamp := strings.TrimPrefix(topicID, prefix)
		if len(stamp) < len("20060102-150405") {
			continue
		}
		stamp = stamp[:len("20060102-150405")]
		t, err := time.ParseInLocation("20060102-150405", stamp, time.UTC)
		if err != nil {
			continue
		}
		return t.UnixMilli()
	}
	return 0
}

func topicCreatedAtForTree(createdAts map[string]int64, topicID string) int64 {
	if createdAt := createdAts[topicID]; createdAt > 0 {
		return createdAt
	}
	return topicIDCreatedAt(topicID)
}

func topicTitleForTab(scope, workspaceRoot, topicID string) string {
	titleRoot := topicTitleRoot(scope, workspaceRoot)
	if title := strings.TrimSpace(loadTopicTitle(titleRoot, topicID)); title != "" {
		return title
	}
	if scope == "global" {
		return "Global"
	}
	return defaultTopicTitle
}

func topicTitleRoot(scope, workspaceRoot string) string {
	if scope == "global" {
		return ""
	}
	return workspaceRoot
}

func (a *App) forkTopicTitle(title string) string {
	base := strings.TrimSpace(title)
	if base == "" || isDefaultTopicTitle(base) || base == "Global" {
		switch a.desktopLocale.Load() {
		case desktopLocaleEn:
			return "Forked session"
		case desktopLocaleZhTW:
			return "分叉會話"
		default:
			return "分叉会话"
		}
	}
	if strings.HasSuffix(base, " · 分叉") || strings.HasSuffix(base, " · fork") {
		return base
	}
	if a.desktopLocale.Load() == desktopLocaleEn {
		return base + " · fork"
	}
	return base + " · 分叉"
}

type sessionRecoveryEvent struct {
	OriginalPath     string `json:"originalPath,omitempty"`
	RecoveryPath     string `json:"recoveryPath"`
	Scope            string `json:"scope,omitempty"`
	WorkspaceRoot    string `json:"workspaceRoot,omitempty"`
	TopicID          string `json:"topicId,omitempty"`
	TopicTitle       string `json:"topicTitle,omitempty"`
	RecoveryReason   string `json:"recoveryReason,omitempty"`
	RecoveryDigest   string `json:"recoveryDigest,omitempty"`
	RecoveryParentID string `json:"recoveryParentId,omitempty"`
	Existing         bool   `json:"existing,omitempty"`
}

type sessionRecoveryFailedEvent struct {
	Reason string `json:"reason,omitempty"`
}

func (a *App) tabSessionRecoveryMeta(tab *WorkspaceTab) func(control.SessionRecoveryRequest) agent.BranchMeta {
	return func(req control.SessionRecoveryRequest) agent.BranchMeta {
		if tab == nil {
			return agent.BranchMeta{Name: agent.RecoveryBranchDefaultName}
		}

		a.mu.RLock()
		ctrl := tab.Ctrl
		scope := strings.TrimSpace(tab.Scope)
		workspaceRoot := strings.TrimSpace(tab.WorkspaceRoot)
		topicID := tab.TopicID
		topicTitle := tab.TopicTitle
		model := strings.TrimSpace(tab.model)
		tokenMode := persistedTabTokenMode(boot.NormalizeTokenMode(tab.tokenMode))
		mode := normalizeTabMode(tab.mode)
		toolApprovalMode := normalizeToolApprovalMode(tab.toolApprovalMode)
		goal := strings.TrimSpace(tab.goal)
		a.mu.RUnlock()
		if ctrl != nil {
			mode = tabModeFromAxes(ctrl.PlanMode(), ctrl.AutoApproveTools())
			toolApprovalMode = normalizeToolApprovalMode(ctrl.ToolApprovalMode())
			if g := strings.TrimSpace(ctrl.Goal()); g != "" && ctrl.GoalStatus() == control.GoalStatusRunning {
				goal = g
			} else {
				goal = ""
			}
		}
		if scope != "project" {
			scope = "global"
		}
		if scope == "global" {
			workspaceRoot = ""
		}
		return agent.BranchMeta{
			Name:             agent.RecoveryBranchDefaultName,
			Scope:            scope,
			WorkspaceRoot:    workspaceRoot,
			TopicID:          topicID,
			TopicTitle:       topicTitle,
			Model:            model,
			AgentPreset:      boot.NormalizeAgentPreset(tokenMode),
			TokenMode:        tokenMode,
			Mode:             persistedTabMode(mode),
			ToolApprovalMode: persistedToolApprovalMode(toolApprovalMode),
			Goal:             goal,
		}
	}
}

func (a *App) handleTabSessionRecovered(tab *WorkspaceTab) func(control.SessionRecoveryInfo) error {
	return func(info control.SessionRecoveryInfo) error {
		if strings.TrimSpace(info.RecoveryPath) == "" {
			return nil
		}
		if err := a.handoffTabRecoveryLease(tab, info.RecoveryPath); err != nil {
			return err
		}
		meta := info.Meta
		scope := strings.TrimSpace(meta.Scope)
		if scope != "project" {
			scope = "global"
		}
		workspaceRoot := strings.TrimSpace(meta.WorkspaceRoot)
		if scope == "global" {
			workspaceRoot = ""
		}
		invalidateTopicSessionIndexForPath(info.RecoveryPath)
		a.mu.Lock()
		if tab != nil && !tab.removed {
			oldKey := sessionRuntimeKey(info.OriginalPath)
			newKey := sessionRuntimeKey(info.RecoveryPath)
			if oldKey != "" && newKey != "" && a.detachedSessions[oldKey] == tab {
				delete(a.detachedSessions, oldKey)
				a.ensureDetachedSessionsLocked()
				a.detachedSessions[newKey] = tab
			}
			tab.SessionPath = canonicalTabSessionPath(info.RecoveryPath)
			if a.tabs[tab.ID] == tab {
				a.saveTabsLocked()
			}
		}
		a.mu.Unlock()

		if tab != nil && !tab.removed {
			origKey := sessionRuntimeKey(info.OriginalPath)
			newKey := sessionRuntimeKey(info.RecoveryPath)
			carried := false
			if newKey != "" {
				tab.telemMu.Lock()
				if tab.telemetrySessionKey == origKey || tab.telemetrySessionKey == "" {
					tab.telemetrySessionKey = newKey
					carried = true
				}
				tab.telemMu.Unlock()
			}
			if carried {
				_ = saveTelemetry(info.RecoveryPath+".telemetry.json", tab.telemetrySnapshot())
			}
		}
		a.emitProjectTreeChangedForSessionDirs(sessionDirectoryForPath(info.RecoveryPath))
		a.emitRuntimeEvent("session:recovered", sessionRecoveryEvent{
			OriginalPath:     info.OriginalPath,
			RecoveryPath:     info.RecoveryPath,
			Scope:            scope,
			WorkspaceRoot:    workspaceRoot,
			TopicID:          meta.TopicID,
			TopicTitle:       meta.TopicTitle,
			RecoveryReason:   meta.RecoveryReason,
			RecoveryDigest:   meta.RecoveryDigest,
			RecoveryParentID: string(meta.ParentID),
			Existing:         info.Existing,
		})
		a.invalidatePromptHistoryCache()
		return nil
	}
}

func setTopicTitle(workspaceRoot, topicID, title string) error {
	return setTopicTitleWithSource(workspaceRoot, topicID, title, topicTitleSourceManual)
}

func setTopicTitleWithSource(workspaceRoot, topicID, title, source string) error {
	m, err := loadTopicTitlesForUpdate(workspaceRoot)
	if err != nil {
		return err
	}
	if strings.TrimSpace(title) == "" {
		delete(m, topicID)
	} else {
		m[topicID] = strings.TrimSpace(title)
	}
	if err := saveTopicTitles(workspaceRoot, m); err != nil {
		return err
	}

	sources, err := loadTopicTitleSourcesForUpdate(workspaceRoot)
	if err != nil {
		return err
	}
	if strings.TrimSpace(title) == "" || strings.TrimSpace(source) == "" {
		delete(sources, topicID)
	} else {
		sources[topicID] = strings.TrimSpace(source)
	}
	if err := saveTopicTitleSources(workspaceRoot, sources); err != nil {
		return err
	}
	if strings.TrimSpace(source) == topicTitleSourceManual ||
		(strings.TrimSpace(source) == topicTitleSourceAuto && isDefaultTopicTitle(title)) {
		_ = deleteTopicAutoTitleMeta(workspaceRoot, topicID)
	}
	return nil
}

func recordTopicAutoTitleMeta(workspaceRoot, topicID string, proposal autoTopicTitleProposal) error {
	topicID = strings.TrimSpace(topicID)
	if topicID == "" || proposal.Stage <= 0 || proposal.BasisHash == "" {
		return nil
	}
	m, err := loadTopicAutoTitleMetaForUpdate(workspaceRoot)
	if err != nil {
		return err
	}
	m[topicID] = topicAutoTitleMeta{
		Stage:     proposal.Stage,
		UserTurns: proposal.UserTurns,
		BasisHash: proposal.BasisHash,
		UpdatedAt: time.Now().UnixMilli(),
	}
	return saveTopicAutoTitleMeta(workspaceRoot, m)
}

func deleteTopicAutoTitleMeta(workspaceRoot, topicID string) error {
	topicID = strings.TrimSpace(topicID)
	if topicID == "" {
		return nil
	}
	m, err := loadTopicAutoTitleMetaForUpdate(workspaceRoot)
	if err != nil {
		return err
	}
	if _, ok := m[topicID]; !ok {
		return nil
	}
	delete(m, topicID)
	return saveTopicAutoTitleMeta(workspaceRoot, m)
}

func setTopicCreatedAt(workspaceRoot, topicID string, createdAt int64) error {
	created, err := loadTopicCreatedAtsForUpdate(workspaceRoot)
	if err != nil {
		return err
	}
	topicID = strings.TrimSpace(topicID)
	if topicID == "" || createdAt <= 0 {
		delete(created, topicID)
	} else {
		created[topicID] = createdAt
	}
	return saveTopicCreatedAts(workspaceRoot, created)
}

func deleteTopicCreatedAt(workspaceRoot, topicID string) error {
	created, err := loadTopicCreatedAtsForUpdate(workspaceRoot)
	if err != nil {
		return err
	}
	if _, ok := created[topicID]; !ok {
		return nil
	}
	delete(created, topicID)
	return saveTopicCreatedAts(workspaceRoot, created)
}

// topicIndexMu serializes recovery writes to desktop-projects.json and topic
// title indexes. Startup builds restored tabs concurrently, and each tab may
// repair its missing index.
var topicIndexMu sync.Mutex

func ensureTopicIndexed(scope, workspaceRoot, topicID, title, source string) error {
	topicID = strings.TrimSpace(topicID)
	if topicID == "" {
		return fmt.Errorf("topicID is required")
	}
	topicIndexMu.Lock()
	defer topicIndexMu.Unlock()
	if strings.TrimSpace(scope) == "global" {
		workspaceRoot = ""
	} else {
		workspaceRoot = normalizeProjectRoot(workspaceRoot)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = defaultTopicTitle
	}
	source = strings.TrimSpace(source)
	if source == "" {
		source = topicTitleSourceManual
	}
	if err := setTopicTitleWithSource(workspaceRoot, topicID, title, source); err != nil {
		return err
	}
	return prependTopicInProjectsFile(workspaceRoot, topicID, true)
}

func saveTelemetry(path string, snapshot tabTelemetrySnapshot) error {
	if snapshot.Version == 0 {
		snapshot.Version = 3
	}
	if snapshot.ReadFiles == nil {
		snapshot.ReadFiles = []readFileRecord{}
	}
	b, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return fileutil.ReplaceFile(tmp, path)
}

func loadTelemetry(path string) tabTelemetrySnapshot {
	b, err := readFileUTF8(path)
	if err != nil {
		return tabTelemetrySnapshot{Version: 3, ReadFiles: []readFileRecord{}}
	}
	var snapshot tabTelemetrySnapshot
	if err := json.Unmarshal(b, &snapshot); err == nil && (snapshot.Version > 0 || snapshot.ReadFiles != nil) {
		if snapshot.ReadFiles == nil {
			snapshot.ReadFiles = []readFileRecord{}
		}
		if snapshot.Usage.SessionCost == 0 && snapshot.Usage.SessionCostUsd > 0 {
			snapshot.Usage.SessionCost = snapshot.Usage.SessionCostUsd
		}

		if snapshot.Version < 3 && snapshot.Usage.CostLedger == nil && snapshot.Usage.SessionCost > 0 {
			q := billing.MigrateLegacyUsage(billing.LegacyUsageRecord{
				SessionCost:     snapshot.Usage.SessionCost,
				SessionCurrency: snapshot.Usage.SessionCurrency,
				EndedAt:         time.Now().UTC(),
			})
			ledger := billing.NewLedger()
			ledger.Add(q, billing.UsageTokens{
				PromptTokens:     snapshot.Usage.PromptTokens,
				CompletionTokens: snapshot.Usage.CompletionTokens,
			}, time.Now().UTC())
			snapshot.Usage.CostLedger = ledger
			total := ledger.Total(billing.NormalizeCurrency(snapshot.Usage.SessionCurrency))
			snapshot.Usage.SessionCostQuote = &total
			snapshot.Usage.SessionCostComplete = total.Complete
			snapshot.Version = 3
		} else if snapshot.Version < 3 && snapshot.Usage.SessionCost <= 0 && strings.TrimSpace(snapshot.Usage.SessionCurrency) != "" {

			q := billing.MigrateLegacyUsage(billing.LegacyUsageRecord{
				SessionCost:     0,
				SessionCurrency: snapshot.Usage.SessionCurrency,
			})
			snapshot.Usage.SessionCostQuote = &q
			snapshot.Usage.SessionCostComplete = false
			snapshot.Version = 3
		} else if snapshot.Version < 3 {
			snapshot.Version = 3
		}
		return snapshot
	}
	var records []readFileRecord
	if err := json.Unmarshal(b, &records); err != nil || records == nil {
		records = []readFileRecord{}
	}
	return tabTelemetrySnapshot{Version: 1, ReadFiles: records}
}
