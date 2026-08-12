package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"path/filepath"
	"reasonix/internal/agent"
	"reasonix/internal/control"
	"strings"
	"sync"
	"time"
	"unicode"
)

const maxTabSnapshotFailureRetries = 2

// autosaveWarnInterval rate-limits the user-facing autosave-failure notice
// per tab; slog keeps recording every failure regardless.
const autosaveWarnInterval = 5 * time.Minute

func tabSnapshotRetryDelay(failures int) time.Duration {
	switch {
	case failures <= 1:
		return 100 * time.Millisecond
	case failures == 2:
		return 250 * time.Millisecond
	default:
		return 500 * time.Millisecond
	}
}

func (a *App) scheduleTabSnapshot(tabID string) {
	a.mu.RLock()
	tab := a.tabByEventSinkIDLocked(tabID)
	a.mu.RUnlock()
	if tab == nil {
		return
	}
	tab.saveMu.Lock()
	defer tab.saveMu.Unlock()
	if tab.closing {

		return
	}
	if tab.saving {
		tab.saveAgain = true
		return
	}
	tab.saving = true
	tab.saveFailures = 0
	go a.tabSnapshotLoop(tab)
}

// quiesceTabAutosave marks the tab as closing and blocks until any in-flight
// tabSnapshotLoop has finished its current (and final) write. After it returns,
// no background goroutine can call Snapshot on this tab's controller again, so
// a subsequent DeleteSession cannot race a late write. Safe to call after the
// controller's session path has been cleared: the loop's Snapshot becomes a
// no-op and it exits on its next iteration.
func (a *App) quiesceTabAutosave(tab *WorkspaceTab) {
	if tab == nil {
		return
	}
	tab.saveMu.Lock()
	if tab.saveCond == nil {

		tab.closing = true
		tab.saveMu.Unlock()
		return
	}
	tab.closing = true
	for tab.saving {
		tab.saveCond.Wait()
	}
	tab.saveMu.Unlock()
}

func (a *App) tabSnapshotLoop(tab *WorkspaceTab) {
	defer a.recoverToPending("tabSnapshotLoop")
	for {
		var snapshotErr error
		a.mu.RLock()
		ctrl := tab.Ctrl
		a.mu.RUnlock()
		if ctrl != nil {
			if err := a.snapshotTab(tab); err == nil {
				a.mu.RLock()
				scope, workspaceRoot := tab.Scope, tab.WorkspaceRoot
				a.mu.RUnlock()
				a.requestSessionCatalogPath(scope, workspaceRoot, ctrl.SessionPath())
				if !a.maybeAutoTitleTopic(tab) {
					a.emitProjectTreeChangedForSessionDirs(ctrl.SessionDir())
				}
			} else {
				snapshotErr = err
			}
		}
		tab.saveMu.Lock()
		if tab.saveCond == nil {
			tab.saveCond = sync.NewCond(&tab.saveMu)
		}
		if snapshotErr == nil {
			tab.saveFailures = 0
		} else {
			tab.saveFailures++
		}
		if tab.closing {

			tab.saving = false
			tab.saveCond.Broadcast()
			tab.saveMu.Unlock()
			if snapshotErr != nil {
				slog.Warn("desktop: session autosave failed during teardown", "tab", tab.ID, "err", snapshotErr)
			}
			return
		}
		if tab.saveAgain {
			tab.saveAgain = false
			tab.saveMu.Unlock()
			if snapshotErr != nil {
				slog.Warn("desktop: session autosave failed; newer snapshot queued", "tab", tab.ID, "err", snapshotErr)
			}
			continue
		}
		if snapshotErr != nil && tab.saveFailures <= maxTabSnapshotFailureRetries {
			delay := tabSnapshotRetryDelay(tab.saveFailures)
			attempt := tab.saveFailures
			tab.saveMu.Unlock()

			slog.Warn("desktop: session autosave failed; retrying", "tab", tab.ID, "attempt", attempt, "err", snapshotErr)
			time.Sleep(delay)
			continue
		}
		exhausted := snapshotErr
		tab.saving = false
		tab.saveCond.Broadcast()
		tab.saveMu.Unlock()
		if exhausted != nil {
			a.reportTabSnapshotError(tab, "autosave", exhausted)
		}
		return
	}
}

func (a *App) maybeAutoTitleTopic(tab *WorkspaceTab) bool {
	if tab == nil {
		return false
	}

	a.mu.RLock()
	topicID := strings.TrimSpace(tab.TopicID)
	titleRoot := tab.WorkspaceRoot
	if tab.Scope == "global" {
		titleRoot = ""
	}
	ctrl := tab.Ctrl
	a.mu.RUnlock()
	if topicID == "" || ctrl == nil {
		return false
	}
	if source := loadTopicTitleSource(titleRoot, topicID); source != topicTitleSourceAuto {
		return false
	}
	sessionPath := ctrl.SessionPath()
	if sessionPath == "" {
		return false
	}
	if sessionHasManualDisplayTitle(sessionPath) {
		return false
	}
	nextTitle, updated := autoTitleTopicFromSession(titleRoot, topicID, sessionPath)
	if !updated {
		return false
	}
	a.updateOpenTopicTitle(topicID, nextTitle, topicTitleSourceAuto)
	changedDirs := a.updateTopicSessionTitles(topicID, nextTitle)
	if len(changedDirs) > 0 {
		a.emitProjectTreeChangedForSessionDirs(changedDirs...)
	} else {
		a.emitProjectTreeMetadataChanged()
	}
	return true
}

func autoTitleTopicFromSession(workspaceRoot, topicID, sessionPath string) (string, bool) {
	if source := loadTopicTitleSource(workspaceRoot, topicID); source != topicTitleSourceAuto {
		return "", false
	}
	if sessionHasManualDisplayTitle(sessionPath) {
		return "", false
	}
	proposal := autoTopicTitleProposalFromSession(sessionPath)
	if proposal.Title == "" {
		return "", false
	}
	if !shouldApplyAutoTopicTitle(workspaceRoot, topicID, proposal) {
		return "", false
	}
	nextTitle := proposal.Title
	if nextTitle == strings.TrimSpace(loadTopicTitle(workspaceRoot, topicID)) {
		_ = recordTopicAutoTitleMeta(workspaceRoot, topicID, proposal)
		return "", false
	}
	if err := setTopicTitleWithSource(workspaceRoot, topicID, nextTitle, topicTitleSourceAuto); err != nil {
		return "", false
	}
	_ = recordTopicAutoTitleMeta(workspaceRoot, topicID, proposal)
	return nextTitle, true
}

type autoTopicTitleProposal struct {
	Title     string
	Stage     int
	UserTurns int
	BasisHash string
}

func autoTopicTitleProposalFromSession(path string) autoTopicTitleProposal {
	users := topicTitleUserTurnsFromSession(path)
	if len(users) == 0 {
		return autoTopicTitleProposal{}
	}
	stage := 1
	if len(users) >= 3 {
		stage = 3
	}
	basis := users
	if len(basis) > stage {
		basis = basis[:stage]
	}
	title := topicTitleFromUserTurns(basis)
	if title == "" {
		return autoTopicTitleProposal{}
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%d\x00%s", stage, strings.Join(basis, "\x00")))
	return autoTopicTitleProposal{
		Title:     title,
		Stage:     stage,
		UserTurns: len(users),
		BasisHash: hex.EncodeToString(sum[:8]),
	}
}

func shouldApplyAutoTopicTitle(workspaceRoot, topicID string, proposal autoTopicTitleProposal) bool {
	if proposal.Stage <= 0 || proposal.BasisHash == "" {
		return false
	}
	meta := loadTopicAutoTitleMeta(workspaceRoot)[topicID]
	if meta.Stage > proposal.Stage {
		return false
	}
	if meta.Stage == proposal.Stage && meta.BasisHash == proposal.BasisHash {
		return false
	}
	return true
}

func sessionHasManualDisplayTitle(sessionPath string) bool {
	sessionPath = strings.TrimSpace(sessionPath)
	if sessionPath == "" {
		return false
	}
	if meta, ok, err := agent.LoadBranchMeta(sessionPath); err == nil && ok {
		if strings.TrimSpace(meta.CustomTitle) != "" {
			return true
		}
	}
	dir := filepath.Dir(sessionPath)
	if dir == "." || dir == string(filepath.Separator) {
		return false
	}
	return strings.TrimSpace(loadSessionTitles(dir)[filepath.Base(sessionPath)]) != ""
}

func topicTitleFallbackForOpen(workspaceRoot, topicID, sessionPath string) (string, string, bool) {
	topicID = strings.TrimSpace(topicID)
	sessionPath = strings.TrimSpace(sessionPath)
	if topicID == "" || sessionPath == "" {
		return "", "", false
	}
	storedTitle := strings.TrimSpace(loadTopicTitle(workspaceRoot, topicID))
	storedSource := strings.TrimSpace(loadTopicTitleSource(workspaceRoot, topicID))
	if storedTitle != "" {
		if storedSource == topicTitleSourceManual || !isDefaultTopicTitle(storedTitle) {
			return "", "", false
		}
	}

	if storedTitle == "" {
		dir := filepath.Dir(sessionPath)
		if meta, ok, err := agent.LoadBranchMeta(sessionPath); err == nil && ok {
			if title := storedSessionTopicTitle(dir, sessionPath, meta); title != "" {
				return title, topicTitleSourceManual, true
			}
		} else if title := topicTitleFromText(loadSessionTitles(dir)[filepath.Base(sessionPath)]); title != "" {
			return title, topicTitleSourceManual, true
		}
	}

	if storedSource == topicTitleSourceManual {
		return "", "", false
	}
	if storedSource == "" || storedSource == topicTitleSourceAuto {
		if title := topicTitleFromSession(sessionPath); title != "" {
			return title, topicTitleSourceAuto, true
		}
	}
	return "", "", false
}

func topicTitleFromSession(path string) string {
	users := topicTitleUserTurnsFromSession(path)
	if len(users) == 0 {
		return ""
	}
	return topicTitleFromText(users[0])
}

func topicTitleUserTurnsFromSession(path string) []string {

	msgs, err := agent.LoadSessionUserMessages(path)
	if err != nil {
		return nil
	}
	var users []string
	for _, msg := range msgs {

		if !agent.IsUserAuthoredTurn(msg.Text) {
			continue
		}

		content := control.StripComposePrefixes(agent.UserPreviewText(msg.Text))
		content = control.StripReferencedContextPrefix(content)
		if strings.TrimSpace(content) != "" {
			users = append(users, content)
		}
	}
	return users
}

func topicTitleFromUserTurns(users []string) string {
	type candidate struct {
		title string
		score int
	}
	best := candidate{score: -1}
	for i, text := range users {
		title := topicTitleFromText(text)
		if title == "" || lowSignalTopicTitle(title) {
			continue
		}
		runes := len([]rune(title))
		score := min(runes, 24)
		if i == 0 {
			score += 3
		}
		if runes < 5 {
			score -= 6
		}
		if score > best.score {
			best = candidate{title: title, score: score}
		}
	}
	if best.title != "" {
		return best.title
	}
	if len(users) > 0 {
		return topicTitleFromText(users[0])
	}
	return ""
}

func lowSignalTopicTitle(title string) bool {
	normalized := strings.ToLower(strings.TrimSpace(title))
	normalized = strings.Trim(normalized, " \t\r\n，。！？；：、,.!?;:\"'`“”‘’()（）[]【】")
	switch normalized {
	case "", "好", "好的", "好啊", "可以", "嗯", "对", "是的", "继续", "继续吧", "采纳建议", "采用建议", "收到", "明白", "ok", "okay", "yes", "yep", "go on", "continue", "thanks", "thank you":
		return true
	default:
		return false
	}
}

func topicTitleFromText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.Join(strings.Fields(text), " ")
	text = strings.Trim(text, " \t\r\n，。！？；：、,.!?;:\"'`“”‘’()（）[]【】")
	if text == "" {
		return ""
	}
	const maxRunes = 18
	runes := []rune(text)
	if len(runes) > maxRunes {
		text = strings.TrimRightFunc(string(runes[:maxRunes]), unicode.IsPunct) + "…"
	}
	if isDefaultTopicTitle(text) {
		return ""
	}
	return text
}
