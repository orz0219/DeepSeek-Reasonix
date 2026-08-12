package main

import (
	"sort"

	"reasonix/internal/agent"
	"reasonix/internal/checkpoint"
	"reasonix/internal/control"
)

// Checkpoints lists the session's rewind points, oldest first, for the rewind UI.
func (a *App) Checkpoints() []CheckpointMeta {
	return a.CheckpointsForTab("")
}

func (a *App) CheckpointsForTab(tabID string) []CheckpointMeta {
	a.mu.RLock()
	var ctrl control.SessionAPI
	if tab := a.tabByIDLocked(tabID); tab != nil {
		ctrl = tab.Ctrl
	}
	a.mu.RUnlock()
	if ctrl == nil {
		return []CheckpointMeta{}
	}
	metas := ctrl.Checkpoints()
	out := make([]CheckpointMeta, 0, len(metas))
	for _, m := range metas {
		gaps := make([]string, 0, len(m.CoverageGaps))
		for _, g := range m.CoverageGaps {
			if g.Detail != "" {
				gaps = append(gaps, g.Reason+": "+g.Detail)
			} else {
				gaps = append(gaps, g.Reason)
			}
		}
		cov := string(m.Coverage)
		meta := CheckpointMeta{
			Turn:               m.Turn,
			Prompt:             m.Prompt,
			Files:              m.Paths,
			TurnFileCount:      len(m.Paths),
			Time:               m.Time.UnixMilli(),
			CanCode:            len(m.Paths) > 0 && m.CanUndoFiles,
			CanConversation:    ctrl.CheckpointHasBoundary(m.Turn),
			Coverage:           cov,
			CoverageGaps:       gaps,
			ExpiredFilePayload: m.ExpiredFilePayload,
			ActiveWriters:      len(m.ActiveWriters),
			Legacy:             m.Legacy,
			CanUndoFiles:       m.CanUndoFiles,
			DisabledReason:     m.DisabledReason,
		}
		out = append(out, meta)
	}

	hasCodeAfter := false
	canCodeAfter := true
	codeFileSet := make(map[string]bool, len(metas)*2)
	codeFilePreview := []string{}

	for i := len(out) - 1; i >= 0; i-- {
		if len(out[i].Files) > 0 {
			hasCodeAfter = true
			if !out[i].CanUndoFiles {
				canCodeAfter = false
			}
		}
		for _, f := range out[i].Files {
			if codeFileSet[f] {
				continue
			}
			codeFileSet[f] = true
			codeFilePreview = insertCheckpointFilePreview(codeFilePreview, f, checkpointFilePreviewLimit)
		}
		out[i].CanCode = hasCodeAfter && canCodeAfter
		out[i].FileCount = len(codeFileSet)
		out[i].Files = append([]string{}, codeFilePreview...)
		out[i].FilesTruncated = out[i].FileCount > len(out[i].Files)
	}
	return out
}

func insertCheckpointFilePreview(preview []string, path string, limit int) []string {
	if limit <= 0 || path == "" {
		return preview
	}
	idx := sort.SearchStrings(preview, path)
	if idx < len(preview) && preview[idx] == path {
		return preview
	}
	if len(preview) < limit {
		preview = append(preview, "")
		copy(preview[idx+1:], preview[idx:])
		preview[idx] = path
		return preview
	}
	if idx >= limit {
		return preview
	}
	copy(preview[idx+1:], preview[idx:limit-1])
	preview[idx] = path
	return preview
}

// ToolResultForTab returns the full arguments and output for one tool call that
// were elided from the frontend's in-memory items[] for memory efficiency. The
// caller (frontend ToolCard) loads this on demand when the user expands a
// collapsed tool card. Returns nil when the tool ID is not found.
func (a *App) ToolResultForTab(tabID, toolID string) *control.ToolResultData {
	a.mu.RLock()
	var ctrl control.SessionAPI
	if tab := a.tabByIDLocked(tabID); tab != nil {
		ctrl = tab.Ctrl
	}
	a.mu.RUnlock()
	if ctrl == nil {
		return nil
	}
	return ctrl.ToolResult(toolID)
}

// Rewind restores the session to the start of turn. scope is "code",
// "conversation", or "both" (anything else is treated as "both"). The frontend
// re-reads History after this resolves.
func (a *App) Rewind(turn int, scope string) error {
	return a.RewindForTab("", turn, scope)
}

// RewindForTab rewinds the requested tab instead of resolving the active tab at
// execution time, which may have changed after frontend confirmation.
// Compatibility wrapper over the transactional path when available; falls back
// to Controller.Rewind which prechecks conversation before files for both scope.
func (a *App) RewindForTab(tabID string, turn int, scope string) error {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return readOnlyChannelErr()
	}
	if ctrl == nil {
		return nil
	}
	s := control.RewindBoth
	switch scope {
	case "code":
		s = control.RewindCode
	case "conversation":
		s = control.RewindConversation
	}
	return ctrl.Rewind(turn, s)
}

// PreviewRewindForTab returns a structured precheck without mutating state.
func (a *App) PreviewRewindForTab(tabID string, turn int, scope string) RewindPlanView {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return RewindPlanView{OK: false, Error: readOnlyChannelErr().Error()}
	}
	if ctrl == nil {
		return RewindPlanView{OK: false, Error: "no controller"}
	}
	s := control.RewindBoth
	switch scope {
	case "code":
		s = control.RewindCode
	case "conversation":
		s = control.RewindConversation
	}
	plan, err := ctrl.PrepareRewind(turn, s)
	view := rewindPlanToView(plan, scope)
	if err != nil {
		view.OK = false
		view.Error = err.Error()
		return view
	}
	view.OK = true
	return view
}

// CommitRewindForTab executes prepare (if planID empty) then commit immediately.
func (a *App) CommitRewindForTab(tabID, planID string, turn int, scope string) RewindResultView {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return RewindResultView{OK: false, Error: readOnlyChannelErr().Error()}
	}
	if ctrl == nil {
		return RewindResultView{OK: false, Error: "no controller"}
	}
	s := control.RewindBoth
	switch scope {
	case "code":
		s = control.RewindCode
	case "conversation":
		s = control.RewindConversation
	}
	if planID == "" {
		plan, err := ctrl.PrepareRewind(turn, s)
		if err != nil {
			return RewindResultView{OK: false, Error: err.Error()}
		}

		if s == control.RewindConversation {
			if !plan.CanConversation {
				return RewindResultView{OK: false, Error: nonEmptyStr(plan.DisabledReason, "conversation rewind unavailable")}
			}
		} else if !plan.CanFiles {
			return RewindResultView{OK: false, Error: nonEmptyStr(plan.DisabledReason, "file rewind unavailable"), Conflicts: conflictStrings(plan), Coverage: string(plan.Coverage)}
		}
		planID = plan.PlanID
	}
	result, err := ctrl.CommitRewind(planID)
	view := rewindResultToView(result)
	if err != nil {
		view.OK = false
		if view.Error == "" {
			view.Error = err.Error()
		}
	}
	return view
}

// UndoRewindForTab undoes the last successful rewind on the tab when available.
func (a *App) UndoRewindForTab(tabID, transactionID string) RewindResultView {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return RewindResultView{OK: false, Error: readOnlyChannelErr().Error()}
	}
	if ctrl == nil {
		return RewindResultView{OK: false, Error: "no controller"}
	}
	result, err := ctrl.UndoRewind(transactionID)
	view := rewindResultToView(result)
	if err != nil {
		view.OK = false
		if view.Error == "" {
			view.Error = err.Error()
		}
	}
	return view
}

// PreviewWorkspaceFileRevertForTab prepares a single-file session-owned revert.
func (a *App) PreviewWorkspaceFileRevertForTab(tabID, path string) RewindPlanView {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return RewindPlanView{OK: false, Error: readOnlyChannelErr().Error(), Path: path}
	}
	if ctrl == nil {
		return RewindPlanView{OK: false, Error: "no controller", Path: path}
	}
	plan, err := ctrl.PrepareFileRevert(path)
	view := rewindPlanToView(plan, "code")
	view.Path = path
	if err != nil {
		view.OK = false
		view.Error = err.Error()
		return view
	}
	view.OK = plan.CanFiles || len(plan.Conflicts) > 0
	return view
}

// CommitWorkspaceFileRevertForTab commits a single-file revert.
// resolution is "keep_current" or "overwrite_checkpoint".
func (a *App) CommitWorkspaceFileRevertForTab(tabID, planID, resolution string) RewindResultView {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return RewindResultView{OK: false, Error: readOnlyChannelErr().Error()}
	}
	if ctrl == nil {
		return RewindResultView{OK: false, Error: "no controller"}
	}
	res := checkpoint.ConflictResolution("")
	switch resolution {
	case "keep_current":
		res = checkpoint.ResolveKeepCurrent
	case "overwrite_checkpoint":
		res = checkpoint.ResolveOverwriteCheckpoint
	}
	result, err := ctrl.CommitFileRevert(planID, res)
	view := rewindResultToView(result)
	if err != nil {
		view.OK = false
		if view.Error == "" {
			view.Error = err.Error()
		}
	}
	return view
}

func rewindPlanToView(plan checkpoint.RewindPlan, scope string) RewindPlanView {
	gaps := make([]string, 0, len(plan.CoverageGaps))
	for _, g := range plan.CoverageGaps {
		if g.Detail != "" {
			gaps = append(gaps, g.Reason+": "+g.Detail)
		} else {
			gaps = append(gaps, g.Reason)
		}
	}
	return RewindPlanView{
		PlanID:             plan.PlanID,
		Turn:               plan.Turn,
		Scope:              scope,
		Coverage:           string(plan.Coverage),
		CoverageGaps:       gaps,
		Legacy:             plan.Legacy,
		ExpiredFilePayload: plan.ExpiredFilePayload,
		CanFiles:           plan.CanFiles,
		CanConversation:    plan.CanConversation,
		DisabledReason:     plan.DisabledReason,
		Conflicts:          conflictStrings(plan),
		Files:              plan.Files,
		FileCount:          plan.FileCount,
		ActiveWriters:      len(plan.ActiveWriters),
		Path:               plan.Path,
	}
}

func conflictStrings(plan checkpoint.RewindPlan) []string {
	out := make([]string, 0, len(plan.Conflicts))
	for _, c := range plan.Conflicts {
		if c.Path != "" {
			out = append(out, c.Path+": "+c.Reason)
		} else {
			out = append(out, c.Reason)
		}
	}
	return out
}

func rewindResultToView(result checkpoint.RewindResult) RewindResultView {
	conflicts := make([]string, 0, len(result.Conflicts))
	for _, c := range result.Conflicts {
		if c.Path != "" {
			conflicts = append(conflicts, c.Path+": "+c.Reason)
		} else {
			conflicts = append(conflicts, c.Reason)
		}
	}
	return RewindResultView{
		OK:             result.OK,
		TransactionID:  result.TransactionID,
		UndoAvailable:  result.UndoAvailable,
		Written:        result.Written,
		Deleted:        result.Deleted,
		ConversationOK: result.ConversationOK,
		Error:          result.Error,
		Conflicts:      conflicts,
		Coverage:       string(result.Coverage),
	}
}

func nonEmptyStr(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// Fork branches the conversation at the start of turn into a new session tab
// (preserving the current tab), keeping code intact, and switches to the new tab.
func (a *App) Fork(turn int) (TabMeta, error) {
	return a.ForkForTab("", turn)
}

// ForkForTab forks the requested source tab even if focus changes before the
// backend begins processing the request. The fork becomes active only while the
// source tab still owns focus, so a later tab selection remains authoritative.
func (a *App) ForkForTab(tabID string, turn int) (TabMeta, error) {
	sourceTab, ctrl := a.tabAndCtrlByID(tabID)
	if sourceTab == nil || ctrl == nil {
		return TabMeta{}, nil
	}
	if a.tabIsReadOnly(sourceTab) {
		return TabMeta{}, readOnlyChannelErr()
	}

	if err := a.ensureTabControllerWorkspace(sourceTab); err != nil {
		return TabMeta{}, err
	}
	a.mu.RLock()
	if a.tabs[sourceTab.ID] != sourceTab || sourceTab.Ctrl == nil {
		a.mu.RUnlock()
		return TabMeta{}, nil
	}
	ctrl = sourceTab.Ctrl
	scope := sourceTab.Scope
	workspaceRoot := sourceTab.WorkspaceRoot
	sourceTitle := sourceTab.TopicTitle
	model := sourceTab.model
	effort := cloneStringPtr(sourceTab.effort)
	mode := currentTabMode(sourceTab)
	toolApprovalMode := currentTabToolApprovalMode(sourceTab)
	disabledMCP := cloneServerViewMap(sourceTab.disabledMCP)
	mcpOrder := append([]string(nil), sourceTab.mcpOrder...)
	a.mu.RUnlock()

	newPath, err := ctrl.ForkSession(turn, "")
	if err != nil {
		return TabMeta{}, err
	}
	topicID := newTopicID()
	topicTitle := a.forkTopicTitle(sourceTitle)
	titleRoot := workspaceRoot
	if scope == "global" {
		titleRoot = ""
	}
	if err := setTopicTitle(titleRoot, topicID, topicTitle); err != nil {
		return TabMeta{}, err
	}
	m, _ := agent.EnsureBranchMeta(newPath)
	m.Scope = scope
	m.WorkspaceRoot = workspaceRoot
	m.TopicID = topicID
	m.TopicTitle = topicTitle
	if err := agent.SaveBranchMeta(newPath, m); err != nil {
		return TabMeta{}, err
	}
	invalidateTopicSessionIndexForPath(newPath)

	a.mu.Lock()
	newTabID := a.newUniqueTabIDLocked()
	tab := &WorkspaceTab{
		ID:               newTabID,
		Scope:            scope,
		WorkspaceRoot:    workspaceRoot,
		TopicID:          topicID,
		TopicTitle:       topicTitle,
		topicTitleSource: topicTitleSourceManual,
		SessionPath:      newPath,
		model:            model,
		effort:           effort,
		mode:             mode,
		toolApprovalMode: toolApprovalMode,
		disabledMCP:      disabledMCP,
		mcpOrder:         mcpOrder,
	}
	tab.sink = &tabEventSink{tabID: newTabID, app: a}
	a.tabs[newTabID] = tab
	a.tabOrder = append(a.tabOrder, newTabID)
	activateFork := a.activeTabID == sourceTab.ID
	if activateFork {
		a.activeTabID = newTabID
	}
	a.saveTabsLocked()
	meta := a.tabMeta(tab, activateFork)
	a.mu.Unlock()

	a.emitProjectTreeChangedForSessionDirs(sessionDirectoryForPath(newPath))
	a.startTabControllerBuild(tab)
	return meta, nil
}
