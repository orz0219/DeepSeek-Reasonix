package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/checkpoint"
	"reasonix/internal/event"
	"reasonix/internal/extension"
	"reasonix/internal/extension/dispatch"
	"reasonix/internal/guardian"
	"reasonix/internal/jobs"
	"reasonix/internal/provider"
	"reasonix/internal/sessioninbox"
	"reasonix/internal/store"
)

// NewSession snapshots the current conversation, rotates to a fresh file, and
// resets the executor to a clean session carrying the same system prompt. It
// ends the old session and starts the new one for lifecycle hooks.
func (c *Controller) NewSession() error {
	if c.executor == nil {
		return nil
	}

	if err := c.beginRotation(); err != nil {
		return err
	}
	defer c.endRotation()

	oldPath := c.SessionPath()
	c.flushRecoveryPersistence(oldPath)
	if err := c.Snapshot(); err != nil {
		return err
	}

	if err := c.extensionSessionPhase(context.Background(), extension.PointSessionRotate, dispatch.PhaseRotate, oldPath); err != nil {
		return err
	}
	c.hooks.SessionEnd(context.Background(), "clear")
	c.extensionSessionEvent(extension.PointSessionEnd, dispatch.PhaseEnd, oldPath)

	c.snapshotMu.Lock()
	if c.sessionDir != "" {
		c.mu.Lock()
		c.sessionPath = agent.NewSessionPath(c.sessionDir, c.label)
		c.guardianPath = guardian.PathFor(c.sessionPath)
		c.mu.Unlock()
	}
	c.setActiveJobSession(c.SessionPath())
	c.executor.SetSession(agent.NewSession(c.systemPrompt))
	c.bindExecutorProjection(c.SessionPath(), false)
	if c.guardianSess != nil {
		c.guardianSess.Reset()
	}
	c.ResetPlannerSession()
	freshPath := c.SessionPath()
	c.rebindCheckpoints(freshPath)
	c.resetRecoveryForNewSession(freshPath)
	c.rotateSessionTemp()
	c.snapshotMu.Unlock()

	c.pauseInboxOnRotate()
	c.rebindInbox()

	c.ClearGoal()
	c.mu.Lock()
	c.startedOnce = true
	c.mu.Unlock()
	c.hooks.SetSessionID(c.parentSessionID())
	c.enqueueHookContexts(c.hooks.SessionStart(context.Background(), "clear"))
	c.extensionSessionEvent(extension.PointSessionStart, dispatch.PhaseStart, c.SessionPath())
	return nil
}

// ClearSession discards the current conversation without preserving it in
// resume/history, then rotates to a clean session carrying the same system prompt.
func (c *Controller) ClearSession() error {
	if c.executor == nil {
		return nil
	}

	if err := c.beginRotation(); err != nil {
		if errors.Is(err, errTurnRunningRotation) {
			return fmt.Errorf("cannot clear while a turn is running")
		}
		return err
	}
	defer c.endRotation()
	c.mu.Lock()
	oldPath := c.sessionPath
	c.mu.Unlock()
	preMarkedCleanup := c.hasUnfinishedSessionJobs(oldPath)
	if preMarkedCleanup {
		if err := agent.MarkCleanupPending(oldPath, "clear"); err != nil {
			return err
		}
	}

	c.loadRecoveryState("")
	c.flushRecoveryPersistence(oldPath)

	if err := c.extensionSessionPhase(context.Background(), extension.PointSessionRotate, dispatch.PhaseRotate, oldPath); err != nil {
		return err
	}

	c.snapshotMu.Lock()
	destroy := c.BeginDestroySession(oldPath)
	if !destroy.Async {
		if err := removeSessionArtifacts(oldPath); err != nil {
			destroy.Finish()
			c.snapshotMu.Unlock()
			return err
		}
		destroy.Finish()
	}
	c.hooks.SessionEnd(context.Background(), "clear")
	c.extensionSessionEvent(extension.PointSessionEnd, dispatch.PhaseEnd, oldPath)
	if c.sessionDir != "" {
		c.mu.Lock()
		c.sessionPath = agent.NewSessionPath(c.sessionDir, c.label)
		c.guardianPath = guardian.PathFor(c.sessionPath)
		c.mu.Unlock()
	}
	c.setActiveJobSession(c.SessionPath())
	c.executor.SetSession(agent.NewSession(c.systemPrompt))
	c.bindExecutorProjection(c.SessionPath(), false)
	if c.guardianSess != nil {
		c.guardianSess.Reset()
	}
	c.ResetPlannerSession()
	freshPath := c.SessionPath()
	c.rebindCheckpoints(freshPath)
	c.resetRecoveryForNewSession(freshPath)
	c.rotateSessionTemp()
	c.snapshotMu.Unlock()
	c.rebindInbox()

	c.ClearGoal()
	c.mu.Lock()
	c.startedOnce = true
	c.mu.Unlock()
	c.hooks.SetSessionID(c.parentSessionID())
	c.enqueueHookContexts(c.hooks.SessionStart(context.Background(), "clear"))
	c.extensionSessionEvent(extension.PointSessionStart, dispatch.PhaseStart, c.SessionPath())
	if destroy.Async {
		go func() {
			result := destroy.Wait()
			if result.HasTimedOut() && destroy.WaitAll != nil {
				if err := agent.MarkCleanupPending(oldPath, "clear"); err != nil {
					c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "mark cleanup pending failed: " + err.Error()})
				}
				destroy.WaitAll()
			}
			if err := removeSessionArtifacts(oldPath); err != nil {
				c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "clear session cleanup failed: " + err.Error()})
			}
			destroy.Finish()
		}()
	}
	return nil
}

func (c *Controller) hasUnfinishedSessionJobs(sessionPath string) bool {
	if c.jobs == nil {
		return false
	}
	return c.jobs.HasUnfinishedForSession(agent.BranchID(sessionPath))
}

func removeSessionArtifacts(path string) error {
	if path == "" {
		return nil
	}
	if err := jobs.RemoveArtifacts(path); err != nil {
		return err
	}
	remove := []string{path}

	remove = append(remove, store.SessionSidecarFiles(path)...)
	remove = append(remove, guardian.PathFor(path), guardian.CursorPathFor(path))
	remove = append(remove, store.SessionSidecarFiles(guardian.PathFor(path))...)
	for _, p := range remove {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := sessioninbox.RemoveDir(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if dir := ckptDir(path); dir != "" {
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := agent.DeleteSubagentsByParent(filepath.Dir(path), agent.BranchID(path)); err != nil {
		return err
	}
	if err := agent.ClearCleanupPending(path); err != nil {
		return err
	}
	return nil
}

// RemoveSessionArtifacts removes a transcript and every durable artifact owned
// by it. Remote runtimes use this when a newly-created fork fails before it can
// be registered as a live session.
func RemoveSessionArtifacts(path string) error {
	return removeSessionArtifacts(path)
}

// ReconcileCleanupPending retries physical cleanup for logically removed
// sessions that were left behind by a previous process.
func ReconcileCleanupPending(dir string) error {
	return agent.ReconcileCleanupPending(dir, func(item agent.CleanupPendingInfo) error {
		return removeSessionArtifacts(item.SessionPath)
	})
}

// RewindScope selects what a Rewind restores.
type RewindScope int

// Checkpoints lists the session's rewind points (one per user turn), oldest first.
//
// Each Meta.Prompt is reduced to what the user typed. A checkpoint opens with
// the composed turn, so the stored prompt can carry the plan-mode marker and
// transient blocks; every consumer of this list is a label (the rewind picker,
// the desktop change list, the workbench projection) and the picker also
// restores the prompt into the composer, so composed text must not reach them.
// Stripping on read rather than only on write keeps checkpoints already on disk
// readable — they were recorded composed.
func (c *Controller) Checkpoints() []checkpoint.Meta {
	metas := c.checkpoints.list()
	for i := range metas {
		metas[i].Prompt = StripComposePrefixes(metas[i].Prompt)
	}
	return metas
}

func (c *Controller) CheckpointFileState(path string) (checkpoint.FileState, bool) {
	return c.checkpoints.fileState(path)
}

func (c *Controller) CheckpointTurnsByMessageIndex() map[int]int {
	return c.checkpoints.turnsByMessageIndex()
}

// rewindFail emits the error as a Warn notice (so a frontend that swallows the
// returned error — e.g. the desktop bridge's .catch — still shows the user why
// the rewind did nothing) and returns it.
func (c *Controller) rewindFail(err error) error {
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: err.Error()})
	return err
}

// Fork branches the conversation at the start of turn into a NEW session file,
// preserving the current one as the branch point, and switches to the branch. Code
// is untouched (it's a conversation operation). Like a conversation rewind it needs
// the live boundary, so it is unavailable for resumed-session turns and refused
// while a turn runs. Returns the new session path.
func (c *Controller) Fork(turn int) (string, error) {
	return c.ForkNamed(turn, "")
}

func (c *Controller) ForkNamed(turn int, name string) (string, error) {
	return c.forkNamed(turn, name, true)
}

// ForkSession copies the conversation at the start of turn into a new session
// file without switching this controller to it. Desktop uses this to open the
// branch in a new tab while the source tab keeps its current transcript.
func (c *Controller) ForkSession(turn int, name string) (string, error) {
	return c.forkNamed(turn, name, false)
}

func (c *Controller) forkNamed(turn int, name string, switchToFork bool) (string, error) {
	if c.executor == nil {
		return "", c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}
	if c.sessionDir == "" {
		return "", c.rewindFail(fmt.Errorf("fork needs session persistence, which is disabled"))
	}

	if err := c.beginRotation(); err != nil {
		if errors.Is(err, errTurnRunningRotation) {
			return "", c.rewindFail(fmt.Errorf("cannot fork while a turn is running"))
		}
		return "", c.rewindFail(err)
	}
	defer c.endRotation()
	boundary, hasBound := c.checkpoints.boundary(turn)
	if !hasBound {
		return "", c.rewindFail(fmt.Errorf("fork unavailable for turn %d (resumed session)", turn))
	}

	if err := c.Snapshot(); err != nil {
		slog.Warn("controller: pre-fork snapshot", "err", err)
	}
	parentPath := c.SessionPath()
	parentID := agent.BranchID(parentPath)
	src := c.executor.Session().Snapshot()
	if boundary > len(src) {
		boundary = len(src)
	}
	forked := append([]provider.Message(nil), src[:boundary]...)
	sess := agent.NewSession("")
	sess.Messages = forked

	newPath := agent.NewSessionPath(c.sessionDir, c.label)
	if err := sess.SaveIfAbsent(newPath); err != nil {
		return "", c.rewindFail(err)
	}
	forkPreview, forkTurns := agent.SessionPreviewFromMessages(forked)
	if err := agent.SaveBranchMeta(newPath, agent.BranchMeta{
		Name:             strings.TrimSpace(name),
		ParentID:         parentID,
		ForkTurn:         turn,
		ForkMessageIndex: boundary,
		Preview:          forkPreview,
		Turns:            forkTurns,
		SchemaVersion:    agent.BranchMetaCountsVersion,
	}); err != nil {
		return "", c.rewindFail(err)
	}
	if switchToFork {

		c.snapshotMu.Lock()
		c.executor.SetSession(sess)
		c.mu.Lock()
		c.sessionPath = newPath
		c.guardianPath = guardian.PathFor(newPath)
		c.mu.Unlock()

		c.bindExecutorProjection(newPath, false)
		c.ResetPlannerSession()
		c.setActiveJobSession(newPath)
		c.rebindCheckpoints(newPath)

		c.loadRecoveryState(newPath)
		if c.guardianSess != nil {
			c.guardianSess.Reset()
		}

		c.rotateSessionTemp()
		c.snapshotMu.Unlock()
	}
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
		Text: fmt.Sprintf("forked conversation at turn %d into a new session", turn)})
	return newPath, nil
}

func (c *Controller) CheckpointHasBoundary(turn int) bool {
	boundary, ok := c.checkpoints.boundary(turn)
	if !ok {
		return false
	}

	return boundary <= c.executor.Session().Len()
}

// Branch copies the current conversation into a child branch and switches to it.
// Unlike Fork, it branches at the current tip and does not require a checkpoint.
func (c *Controller) Branch(name string) (string, error) {
	if c.executor == nil {
		return "", c.rewindFail(fmt.Errorf("branch unavailable"))
	}
	if c.sessionDir == "" {
		return "", c.rewindFail(fmt.Errorf("branch needs session persistence, which is disabled"))
	}

	if err := c.beginRotation(); err != nil {
		if errors.Is(err, errTurnRunningRotation) {
			return "", c.rewindFail(fmt.Errorf("cannot branch while a turn is running"))
		}
		return "", c.rewindFail(err)
	}
	defer c.endRotation()
	if !c.executor.Session().HasContent() {
		return "", c.rewindFail(fmt.Errorf("nothing to branch yet"))
	}
	if err := c.Snapshot(); err != nil {
		return "", c.rewindFail(err)
	}
	parentPath := c.SessionPath()
	parentID := agent.BranchID(parentPath)
	src := c.executor.Session().Snapshot()
	branched := append([]provider.Message(nil), src...)
	sess := agent.NewSession("")
	sess.Messages = branched

	newPath := agent.NewSessionPath(c.sessionDir, c.label)
	if err := sess.SaveIfAbsent(newPath); err != nil {
		return "", c.rewindFail(err)
	}
	branchPreview, branchTurns := agent.SessionPreviewFromMessages(branched)
	if err := agent.SaveBranchMeta(newPath, agent.BranchMeta{
		Name:             strings.TrimSpace(name),
		ParentID:         parentID,
		ForkTurn:         -1,
		ForkMessageIndex: len(branched),
		Preview:          branchPreview,
		Turns:            branchTurns,
		SchemaVersion:    agent.BranchMetaCountsVersion,
	}); err != nil {
		return "", c.rewindFail(err)
	}

	c.snapshotMu.Lock()
	c.executor.SetSession(sess)
	c.mu.Lock()
	c.sessionPath = newPath
	c.guardianPath = guardian.PathFor(newPath)
	c.mu.Unlock()
	c.bindExecutorProjection(newPath, false)
	c.ResetPlannerSession()
	c.setActiveJobSession(newPath)
	c.rebindCheckpoints(newPath)
	if c.guardianSess != nil {
		c.guardianSess.Reset()
	}
	c.carryRecoveryState(newPath)
	c.rotateSessionTemp()
	c.snapshotMu.Unlock()
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
		Text: fmt.Sprintf("created branch %s", agent.BranchID(newPath))})
	return newPath, nil
}

// Branches lists saved conversation branches in this controller's session dir.
func (c *Controller) Branches() ([]agent.BranchInfo, error) {
	if c.sessionDir == "" {
		return nil, fmt.Errorf("session persistence is disabled")
	}
	if err := c.Snapshot(); err != nil {
		return nil, err
	}
	return agent.ListBranches(c.sessionDir)
}

func (c *Controller) SwitchBranch(ref string) (agent.BranchInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return agent.BranchInfo{}, c.rewindFail(fmt.Errorf("usage: /switch <branch id|name>"))
	}

	if err := c.beginRotation(); err != nil {
		if errors.Is(err, errTurnRunningRotation) {
			return agent.BranchInfo{}, c.rewindFail(fmt.Errorf("cannot switch branches while a turn is running"))
		}
		return agent.BranchInfo{}, c.rewindFail(err)
	}
	defer c.endRotation()
	branches, err := c.Branches()
	if err != nil {
		return agent.BranchInfo{}, c.rewindFail(err)
	}
	match, err := resolveBranch(branches, ref)
	if err != nil {
		return agent.BranchInfo{}, c.rewindFail(err)
	}
	if !agent.IsVisibleSession(match.Path) {
		return agent.BranchInfo{}, c.rewindFail(fmt.Errorf("branch %q not found", ref))
	}
	loaded, err := agent.LoadSession(match.Path)
	if err != nil {
		return agent.BranchInfo{}, c.rewindFail(err)
	}

	c.snapshotMu.Lock()
	if c.executor != nil {
		c.executor.SetSession(loaded)
	}
	c.mu.Lock()
	c.sessionPath = match.Path
	c.guardianPath = guardian.PathFor(match.Path)
	c.mu.Unlock()
	c.bindExecutorProjection(match.Path, true)
	c.ResetPlannerSession()
	c.setActiveJobSession(match.Path)
	c.rebindCheckpoints(match.Path)
	c.restoreTerminalGoalTodos(match.Path)
	c.loadGuardianSession()
	c.loadRecoveryState(match.Path)
	c.rotateSessionTemp()
	c.snapshotMu.Unlock()
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo,
		Text: fmt.Sprintf("switched to branch %s", branchDisplayName(match))})
	return match, nil
}

// ResolveBranchRef resolves a /switch-style branch reference (id, unique
// prefix, name, or path) against a branch listing, using the same matching
// rules as SwitchBranch. Frontends use it to learn the target session path
// before switching — e.g. to move their session lease first.
func ResolveBranchRef(branches []agent.BranchInfo, ref string) (agent.BranchInfo, error) {
	return resolveBranch(branches, strings.TrimSpace(ref))
}

func resolveBranch(branches []agent.BranchInfo, ref string) (agent.BranchInfo, error) {
	refLower := strings.ToLower(ref)
	var matches []agent.BranchInfo
	for _, b := range branches {
		nameLower := strings.ToLower(strings.TrimSpace(b.Name))
		switch {
		case b.ID == ref || strings.EqualFold(b.ID, ref):
			return b, nil
		case b.Name != "" && nameLower == refLower:
			matches = append(matches, b)
		case strings.HasPrefix(strings.ToLower(b.ID), refLower):
			matches = append(matches, b)
		case strings.HasPrefix(strings.ToLower(shortBranchID(b.ID)), refLower):
			matches = append(matches, b)
		case b.Path == ref:
			return b, nil
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return agent.BranchInfo{}, fmt.Errorf("branch %q is ambiguous", ref)
	}
	return agent.BranchInfo{}, fmt.Errorf("branch %q not found", ref)
}

func branchDisplayName(b agent.BranchInfo) string {
	if strings.TrimSpace(b.Name) != "" {
		return fmt.Sprintf("%s (%s)", b.Name, b.ID)
	}
	return b.ID
}

// SummarizeFrom and SummarizeUpTo preserve the historical turn-index API while
// changing only the model-visible context projection. The canonical transcript
// and checkpoint boundaries remain available for rewind, undo, and fork.
func (c *Controller) SummarizeFrom(ctx context.Context, turn int) error {
	return c.summarizeAt(ctx, turn, true)
}

func (c *Controller) SummarizeUpTo(ctx context.Context, turn int) error {
	return c.summarizeAt(ctx, turn, false)
}

func (c *Controller) summarizeAt(ctx context.Context, turn int, from bool) error {
	if c.executor == nil {
		return c.rewindFail(fmt.Errorf("checkpoints unavailable"))
	}

	if err := c.beginRotation(); err != nil {
		if errors.Is(err, errTurnRunningRotation) {
			return c.rewindFail(fmt.Errorf("cannot summarize while a turn is running"))
		}
		return c.rewindFail(err)
	}
	defer c.endRotation()
	boundary, hasBound := c.checkpoints.boundary(turn)
	if !hasBound {
		return c.rewindFail(fmt.Errorf("summarize unavailable for turn %d (resumed session)", turn))
	}
	var err error
	if from {
		err = c.executor.SummarizeFrom(ctx, boundary)
	} else {
		err = c.executor.SummarizeUpTo(ctx, boundary)
	}
	if err != nil {
		return c.rewindFail(err)
	}
	return nil
}

// Resume seeds the session from a loaded transcript and pins the active file to
// its path so auto-save keeps appending there.
//
// When the controller already has a different non-empty session path, Resume
// rotates the private temporary generation so the loaded conversation cannot
// see the previous session's temporary files. Same-path Resume (hot rebuild
// migration via AdoptHistory) keeps the generation.
func (c *Controller) Resume(s *agent.Session, path string) {

	prevPath := c.SessionPath()
	c.snapshotMu.Lock()
	if c.executor != nil {
		c.executor.SetSession(s)
	}
	c.mu.Lock()
	c.sessionPath = path
	c.guardianPath = guardian.PathFor(path)
	c.mu.Unlock()
	c.bindExecutorProjection(path, true)
	c.ResetPlannerSession()
	c.setActiveJobSession(path)
	c.rebindCheckpoints(path)
	migPath, migData, migrated, legacy := c.goals.restoreFromState(path)
	if !c.restorePendingLegacyGoal(legacy) && migrated {
		c.persistGoalState(migPath, migData, true)
	}
	if c.executor != nil {
		c.executor.RestoreDeliveryCheckpoint(c.goals.deliveryState())
	}
	c.restoreTerminalGoalTodos(path)
	c.loadGuardianSession()
	c.loadRecoveryState(path)
	if shouldRotateSessionTempOnResume(prevPath, path) {
		c.rotateSessionTemp()
	}
	c.snapshotMu.Unlock()
	c.rebindInbox()
	c.recoverCheckpointTransactions()
	c.recoverInterruptedTurn(path)
	c.maybeColdResumePrune(path)

	if err := c.extensionSessionPhase(context.Background(), extension.PointSessionLoad, dispatch.PhaseLoad, path); err != nil {
		c.extensionWarn("session policy failed at session.load", err)
	}
}

func shouldRotateSessionTempOnResume(prevPath, nextPath string) bool {
	prevPath = strings.TrimSpace(prevPath)
	nextPath = strings.TrimSpace(nextPath)
	if prevPath == "" || nextPath == "" {
		return false
	}
	return filepath.Clean(prevPath) != filepath.Clean(nextPath)
}

func (c *Controller) loadGuardianSession() {
	if c.guardianSess == nil {
		return
	}
	c.guardianSess.Reset()
	path := c.guardianPath
	if path == "" {
		return
	}
	if err := c.guardianSess.Load(path); err != nil && !os.IsNotExist(err) {
		slog.Warn("controller: load guardian session", "err", err)
	}
}

// ResetPlannerSession clears the planner's conversation history so the next
// plan starts fresh. In dual-model (Plan+Execute) mode, this prevents stale
// planner output from a previous session or tab from contaminating the current
// executor's handoff. Safe to call on a single-model controller (no-op).
func (c *Controller) ResetPlannerSession() {
	runner, ok := c.runner.(plannerSessionResetter)
	if ok {
		runner.ResetPlannerSession()
	}
}

const (
	RewindCode         RewindScope = iota // files only
	RewindConversation                    // message log only
	RewindBoth                            // both
)
