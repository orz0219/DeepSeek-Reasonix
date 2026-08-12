package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/extension"
	"reasonix/internal/extension/dispatch"
	"reasonix/internal/guardian"
	"reasonix/internal/provider"
)

// cacheColdAfter resolves how long the active provider keeps a prompt prefix
// cached. A session idle longer than this resumes against a cold cache, so a
// history rewrite at that moment costs no extra cache misses — it only shrinks
// the full-price first request. The TTL is vendor-aware: DeepSeek/unknown
// 24h (legacy default deliberately preserved), DashScope 5m, Anthropic 5m.
// Users can override per-provider
// with cache_ttl_minutes in config.toml.
func (c *Controller) cacheColdAfter() time.Duration {
	if c.testCacheColdAfter != 0 {
		if c.testCacheColdAfter == -1 {
			return 0
		}
		return c.testCacheColdAfter
	}

	cfg, err := config.LoadForRootReadOnly(c.workspaceRoot)
	if err != nil {
		return 24 * time.Hour
	}
	ref := c.modelRef
	if ref == "" {
		ref = cfg.DefaultModel
	}
	entry, ok := cfg.ResolveModel(ref)
	if !ok {
		return 24 * time.Hour
	}
	return entry.EffectiveCacheTTL()
}

// Snapshot writes the executor's conversation to the active session file. No-op
// when the executor is absent or the session has never been used (no user
// interaction). Returns errNoSessionPath when there IS content but no resolved
// path, so a misconfigured deployment surfaces instead of dropping data.
// Called after every turn so a crash loses at most one in-flight prompt.
func (c *Controller) Snapshot() error {
	return c.snapshot(false, false, false)
}

// SnapshotForShutdown performs the final session snapshot and, only when the
// compatibility file lock remains held for the full bounded wait, persists the
// in-memory transcript to a distinct recovery branch before teardown proceeds.
// Other snapshot errors retain their normal behavior and remain visible to the
// caller.
func (c *Controller) SnapshotForShutdown() error {
	return c.snapshot(false, false, true)
}

// SnapshotActivity writes the active conversation and marks the session as
// recently active. Use it only after a real user/model turn changes the
// transcript; switch/close snapshots should call Snapshot so they do not reorder
// recent-session pickers.
func (c *Controller) SnapshotActivity() error {
	return c.snapshot(true, false, false)
}

// SnapshotRewrite persists an intentional history rewrite, such as rewind or
// manual compaction. Ordinary autosave paths should use Snapshot so stale
// controllers cannot overwrite a newer transcript.
func (c *Controller) SnapshotRewrite() error {
	return c.snapshot(false, true, false)
}

func (c *Controller) snapshot(markActivity, forceRewrite, shutdownRecovery bool) error {
	_, err := c.snapshotWithDurability(markActivity, forceRewrite, shutdownRecovery)
	return err
}

// midTurnSnapshotInterval is atomic (nanoseconds) so a test shrinking it
// cannot race a previous test's still-parking autosave goroutine.
var midTurnSnapshotInterval atomic.Int64

// autosaveWhileRunning snapshots the session periodically while a turn runs,
// so an abrupt kill (SSH drop, force-quit) loses at most one interval of a
// long turn instead of all of it (#3772). Session.Save copies under the lock
// and replaces the file atomically, so racing the turn's appends is safe.
func (c *Controller) autosaveWhileRunning(ctx context.Context) {
	t := time.NewTicker(time.Duration(midTurnSnapshotInterval.Load()))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.snapshot(false, false, false); err != nil {
				slog.Warn("controller: mid-turn snapshot", "err", err)
			}
		}
	}
}

// snapshotWithDurability reports whether the canonical transcript reached disk
// even when a later sidecar update failed. Callers that guard a crash marker
// need this distinction: a metadata error must not make a complete transcript
// look like an in-memory-only turn.
func (c *Controller) snapshotWithDurability(markActivity, forceRewrite, shutdownRecovery bool) (bool, error) {
	c.snapshotMu.Lock()
	defer c.snapshotMu.Unlock()

	c.mu.Lock()
	path := c.sessionPath
	modelRef := c.modelRef
	c.mu.Unlock()
	if c.executor == nil {
		return false, nil
	}
	s := c.executor.Session()
	if !s.HasContent() {

		return false, nil
	}
	if !s.HasSystemMessage() {

		slog.Warn("controller: refusing to snapshot session with content but no system message",
			"label", c.Label(), "session_dir", c.SessionDir(), "message_count", len(s.Snapshot()))
		return false, nil
	}
	if path == "" {

		slog.Warn("controller: session has content but no session path; conversation will not be persisted",
			"label", c.Label(), "session_dir", c.SessionDir())
		return false, errNoSessionPath
	}

	savePayload, strategyErr := c.extensionSessionStrategy(context.Background(), extension.PointSessionSave, dispatch.PhaseSave, path)
	if strategyErr != nil {
		return false, strategyErr
	}
	err, forceRewrite := persistSessionSnapshot(s, path, forceRewrite)
	if authoritySaveError(err) {

		return false, err
	}
	if err != nil {
		if shutdownRecovery && errors.Is(err, agent.ErrSessionFileLockHeld) {
			recoveredPath, recoverErr := c.recoverShutdownSnapshot(path, err)
			if recoverErr != nil {
				return false, recoverErr
			}
			path = recoveredPath
			s = c.executor.Session()
			err = nil
		}
	}
	if err != nil {
		if errors.Is(err, agent.ErrSessionExternallyRemoved) {
			recoveredPath, recoverErr := c.recoverExternallyRemovedSession(path, err)
			if recoverErr != nil {
				return false, recoverErr
			}
			path = recoveredPath
			s = c.executor.Session()
			err = nil
		}
	}
	if err != nil {
		if !errors.Is(err, agent.ErrSessionSnapshotConflict) {
			return false, err
		}
		recoveredPath, outcome, recoverErr := c.recoverSnapshotConflict(path, err, forceRewrite)
		if recoverErr != nil {
			if shutdownRecovery && errors.Is(recoverErr, agent.ErrSessionFileLockHeld) {
				recoveredPath, recoverErr = c.recoverShutdownSnapshot(path, recoverErr)
				if recoverErr != nil {
					return false, recoverErr
				}
				path = recoveredPath
				s = c.executor.Session()
			} else {
				return false, recoverErr
			}
		} else {
			if outcome == conflictDropped {
				return false, nil
			}

			path = recoveredPath
			s = c.executor.Session()
		}
	}

	if c.guardianSess != nil {
		gp := c.guardianPath
		if gp != "" {
			if gerr := c.guardianSess.Save(gp); gerr != nil {
				slog.Warn("controller: guardian snapshot", "err", gerr)
			}
		}
	}
	transcriptDurable := true

	c.saveRecoveryState(path)

	preview, turns := agent.SessionPreviewFromMessages(s.Snapshot())
	if err := agent.UpdateSessionMeta(path, modelRef, preview, turns, markActivity); err != nil {
		return transcriptDurable, err
	}
	c.extensionSessionPayloadEvent(extension.PointSessionSave, savePayload)
	return transcriptDurable, nil
}

func (c *Controller) recoverExternallyRemovedSession(path string, saveErr error) (string, error) {
	if c.executor == nil || strings.TrimSpace(path) == "" {
		return "", saveErr
	}
	const reason = "session removed while open"
	req := SessionRecoveryRequest{OriginalPath: path, Reason: reason, Mode: "external-removal"}
	meta := agent.BranchMeta{}
	if c.sessionRecoveryMeta != nil {
		meta = c.sessionRecoveryMeta(req)
	}
	info, err := c.executor.Session().SaveConflictRecoveryBranch(agent.RecoveryBranchOptions{
		OriginalPath: path,
		Reason:       reason,
		BranchMeta:   meta,
	})
	if err != nil {
		return "", fmt.Errorf("preserve externally removed session: %w", err)
	}
	if err := c.commitRecoveredSession(path, reason, info); err != nil {
		return "", err
	}
	appendSnapshotConflictDiagnostic(path, "external-removal", "moved_to_stable_recovery", saveErr, info.Path, info.Existing)
	slog.Warn("controller: active session was removed externally; moved runtime to stable recovery path",
		"path", path, "recovery", info.Path, "existing", info.Existing)
	c.sink.Emit(sessionRecoveryNotice(event.NoticeCodeSessionRecoveryForked,
		"the open session file was removed outside Reasonix; your active conversation was preserved as one recovery copy"))
	return info.Path, nil
}

// snapshotConflictLogAttrs flattens a snapshot-conflict error into slog attrs.
// Field reports of #6069-class "session changed on disk" spam are only
// diagnosable when the logs say which trigger fired and what the revision
// ledger looked like, so every recoverSnapshotConflict outcome logs these.
func snapshotConflictLogAttrs(saveErr error, path, mode string) []any {
	attrs := []any{"path", path, "mode", mode}
	var conflict *agent.SessionSnapshotConflictError
	if errors.As(saveErr, &conflict) && conflict != nil {
		attrs = append(attrs,
			"kind", string(conflict.Kind),
			"disk_messages", conflict.ExistingMessages,
			"snapshot_messages", conflict.SnapshotMessages,
			"base_revision", conflict.BaseRevision,
			"disk_revision", conflict.DiskRevision,
		)
	}
	return attrs
}

// conflictOutcome is recoverSnapshotConflict's declared result. Callers act
// on it directly instead of re-deriving what happened from path or session
// pointer comparisons — the misclassification that broke the depth-cap
// rewrite baseline (#6120) hid in exactly that inference.
type conflictOutcome int

const recoveryDepthCapNoticeText = "repeated save conflicts were detected; saved the current conflict copy in an isolated recovery branch"

func sessionRecoveryNotice(code, text string) event.Event {
	return event.Event{
		Kind:     event.Notice,
		Level:    event.LevelWarn,
		Audience: event.NoticeAudienceOperator,
		Code:     code,
		Text:     text,
	}
}

func (c *Controller) emitRecoveryDepthCapNotice(path string) {
	key := filepath.Clean(strings.TrimSpace(path))
	c.mu.Lock()
	if c.recoveryDepthCapNotices == nil {
		c.recoveryDepthCapNotices = make(map[string]bool)
	}
	if c.recoveryDepthCapNotices[key] {
		c.mu.Unlock()
		return
	}
	c.recoveryDepthCapNotices[key] = true
	c.mu.Unlock()
	c.sink.Emit(sessionRecoveryNotice(event.NoticeCodeSessionRecoveryDepthCap, recoveryDepthCapNoticeText))
}

func (c *Controller) recoverSnapshotConflict(path string, saveErr error, forceRewrite bool) (string, conflictOutcome, error) {
	if c.executor == nil || strings.TrimSpace(path) == "" {
		return "", conflictDropped, saveErr
	}
	mode := "snapshot"
	if forceRewrite {
		mode = "rewrite"
	}
	logAttrs := snapshotConflictLogAttrs(saveErr, path, mode)
	if kind, ok := agent.SnapshotConflictKind(saveErr); ok && kind == agent.SessionSnapshotConflictStalePrefix {
		if c.adoptDiskSession(path) {
			appendSnapshotConflictDiagnostic(path, mode, "adopted_newer_disk_transcript", saveErr, "", false)
			slog.Warn("controller: snapshot conflict; adopted newer disk transcript", logAttrs...)
			c.sink.Emit(sessionRecoveryNotice(event.NoticeCodeSessionRecoveryAdopted,
				"session changed on disk; adopted the newer transcript"))
			return path, conflictAdoptedDisk, nil
		}
	}
	reason := "snapshot conflict"
	if forceRewrite {
		reason = "rewrite conflict"
	}
	req := SessionRecoveryRequest{OriginalPath: path, Reason: reason, Mode: mode}
	meta := agent.BranchMeta{}
	if c.sessionRecoveryMeta != nil {
		meta = c.sessionRecoveryMeta(req)
	}
	info, err := c.executor.Session().SaveRecoveryBranch(agent.RecoveryBranchOptions{
		OriginalPath: path,
		Reason:       reason,
		BranchMeta:   meta,
	})
	if err != nil {
		if errors.Is(err, agent.ErrSessionRecoveryDepthExceeded) {

			isolated, isolatedErr := c.executor.Session().SaveConflictRecoveryBranch(agent.RecoveryBranchOptions{
				OriginalPath: path,
				Reason:       reason,
				BranchMeta:   meta,
			})
			if isolatedErr != nil {
				return "", conflictDropped, fmt.Errorf("recovery chain depth exceeded; isolated copy failed: %w", isolatedErr)
			}
			if err := c.commitRecoveredSession(path, reason, isolated); err != nil {
				return "", conflictDropped, err
			}
			appendSnapshotConflictDiagnostic(path, mode, "recovery_depth_cap_isolated", saveErr, isolated.Path, isolated.Existing)
			slog.Warn("controller: snapshot conflict; recovery depth cap reached, isolated stale transcript", append(logAttrs, "recovery", isolated.Path)...)
			c.emitRecoveryDepthCapNotice(path)
			return isolated.Path, conflictForkedBranch, nil
		}
		if errors.Is(err, agent.ErrSessionRecoveryNotNeeded) {
			if c.adoptDiskSession(path) {
				appendSnapshotConflictDiagnostic(path, mode, "recovery_not_needed_adopted_disk_transcript", saveErr, "", false)
				slog.Warn("controller: snapshot conflict; recovery not needed, adopted disk transcript", logAttrs...)
				c.sink.Emit(sessionRecoveryNotice(event.NoticeCodeSessionRecoveryAdoptedCovered,
					"session changed on disk; adopted the newer transcript (local changes already covered)"))
				return path, conflictAdoptedDisk, nil
			}

			appendSnapshotConflictDiagnostic(path, mode, "recovery_not_needed_adopt_failed", saveErr, "", false)
			slog.Warn("controller: snapshot conflict; recovery not needed but disk transcript could not be adopted", logAttrs...)
			return "", conflictDropped, nil
		}
		return "", conflictDropped, fmt.Errorf("recover stale session snapshot: %w", err)
	}
	if err := c.commitRecoveredSession(path, reason, info); err != nil {
		return "", conflictDropped, err
	}
	appendSnapshotConflictDiagnostic(path, mode, "forked_recovery_branch", saveErr, info.Path, info.Existing)
	slog.Warn("controller: snapshot conflict; forked recovery branch",
		append(logAttrs, "recovery", info.Path, "existing", info.Existing)...)
	c.sink.Emit(sessionRecoveryNotice(event.NoticeCodeSessionRecoveryForked,
		"session changed on disk; unsaved local transcript was saved as a conflict copy"))
	return info.Path, conflictForkedBranch, nil
}

func (c *Controller) recoverShutdownSnapshot(path string, saveErr error) (string, error) {
	if c.executor == nil || strings.TrimSpace(path) == "" {
		return "", saveErr
	}
	const reason = "shutdown session file lock timeout"
	req := SessionRecoveryRequest{OriginalPath: path, Reason: reason, Mode: "shutdown"}
	meta := agent.BranchMeta{}
	if c.sessionRecoveryMeta != nil {
		meta = c.sessionRecoveryMeta(req)
	}
	info, err := c.executor.Session().SaveShutdownRecoveryBranch(agent.RecoveryBranchOptions{
		OriginalPath: path,
		Reason:       reason,
		BranchMeta:   meta,
	})
	if err != nil {
		return "", fmt.Errorf("save shutdown recovery branch: %w", err)
	}
	if err := c.commitRecoveredSession(path, reason, info); err != nil {
		return "", err
	}
	appendSnapshotConflictDiagnostic(path, "shutdown", "forked_file_lock_recovery", saveErr, info.Path, info.Existing)
	slog.Warn("controller: shutdown snapshot lock timed out; forked recovery branch",
		"path", path, "recovery", info.Path, "existing", info.Existing)
	c.sink.Emit(sessionRecoveryNotice(event.NoticeCodeSessionShutdownRecoveryForked,
		"session file stayed busy during shutdown; unsaved transcript was saved as a recovery copy"))
	return info.Path, nil
}

func (c *Controller) commitRecoveredSession(originalPath, reason string, info agent.RecoveryBranchInfo) error {
	recoveryInfo := SessionRecoveryInfo{
		OriginalPath: originalPath,
		RecoveryPath: info.Path,
		Existing:     info.Existing,
		Reason:       reason,
		Meta:         info.Meta,
	}
	if onSessionRecovered := c.sessionRecoveredHandler(); onSessionRecovered != nil {
		if err := onSessionRecovered(recoveryInfo); err != nil {
			return fmt.Errorf("commit recovered session: %w", err)
		}
	}
	c.mu.Lock()
	c.sessionPath = info.Path
	c.guardianPath = guardian.PathFor(info.Path)
	c.mu.Unlock()

	c.bindExecutorProjection(info.Path, true)
	c.setActiveJobSession(info.Path)
	c.rebindCheckpoints(info.Path)
	c.transplantInFlightTurnMarker(originalPath, info.Path)
	return nil
}

func (c *Controller) adoptDiskSession(path string) bool {
	loaded, err := agent.LoadSession(path)
	if err != nil || loaded == nil {
		return false
	}
	c.executor.SetSession(loaded)
	c.bindExecutorProjection(path, true)
	c.ResetPlannerSession()
	c.rebindCheckpoints(path)
	c.setActiveJobSession(path)
	return true
}

func (c *Controller) messageCount() int {
	if c.executor == nil {
		return 0
	}
	return c.executor.Session().Len()
}

func (c *Controller) markInFlightTurn(startMessageIndex int, preserveUser bool) agent.InFlightTurnMeta {
	path := c.SessionPath()
	if path == "" {
		return agent.InFlightTurnMeta{}
	}
	marker, err := agent.BeginSessionInFlightTurn(path, startMessageIndex, preserveUser)
	if err != nil {
		slog.Warn("controller: mark in-flight turn", "err", err)
		return agent.InFlightTurnMeta{}
	}
	return marker
}

func (c *Controller) clearInFlightTurn(marker agent.InFlightTurnMeta) {
	path := c.SessionPath()
	if path == "" || marker.ID == "" {
		return
	}
	if _, err := agent.ClearSessionInFlightTurnIfMatch(path, marker); err != nil {
		slog.Warn("controller: clear in-flight turn", "err", err)
	}
}

// finishInFlightTurn persists the completed transcript before removing the
// crash marker. A crash can therefore leave either a recoverable marker or a
// durable completed transcript, never an unmarked in-memory-only suffix.
func (c *Controller) finishInFlightTurn(startMessages int, marker agent.InFlightTurnMeta) {
	commitPrepared := marker.ID == ""
	if marker.ID != "" && c.executor != nil {
		digest, digestErr := c.executor.Session().ContentDigest()
		if digestErr != nil {
			slog.Warn("controller: compute completed turn digest", "err", digestErr)
		} else if prepared, matched, prepareErr := agent.PrepareSessionInFlightTurnCommit(c.SessionPath(), marker, digest); prepareErr != nil {
			slog.Warn("controller: prepare in-flight turn commit", "err", prepareErr)
		} else if matched {
			marker = prepared
			commitPrepared = true
		}
	}
	durable, err := c.snapshotActivityIfChanged(startMessages)
	if err != nil && !durable {

		slog.Warn("controller: keeping in-flight marker after failed turn snapshot", "err", err)
		return
	}
	if err != nil {
		slog.Warn("controller: turn transcript saved before metadata update failed", "err", err)
	}
	if !commitPrepared {

		slog.Warn("controller: keeping in-flight marker without commit digest", "marker_id", marker.ID)
		return
	}
	c.clearInFlightTurn(marker)
}

// transplantInFlightTurnMarker moves a pending in-flight-turn marker from the
// session path a recovery fork abandoned onto the branch the turn continues
// on. Left behind, the stale marker would fire recoverInterruptedTurn on the
// next open of the original branch and strip messages from a turn that in
// fact kept running on the recovery branch; missing from the recovery branch,
// a crash before turn end would leave its partial tail unmarked.
func (c *Controller) transplantInFlightTurnMarker(fromPath, toPath string) {
	if strings.TrimSpace(fromPath) == "" || strings.TrimSpace(toPath) == "" || fromPath == toPath {
		return
	}
	meta, ok, err := agent.LoadBranchMeta(fromPath)
	if err != nil || !ok || meta.InFlightTurn == nil {
		if err != nil {
			slog.Warn("controller: load in-flight turn marker for transplant", "path", fromPath, "err", err)
		}
		return
	}
	marker := meta.InFlightTurn
	if err := agent.SetSessionInFlightTurn(toPath, *marker); err != nil {

		slog.Warn("controller: transplant in-flight turn marker", "path", toPath, "err", err)
		return
	}
	if _, err := agent.ClearSessionInFlightTurnIfMatch(fromPath, *marker); err != nil {
		slog.Warn("controller: clear in-flight turn marker on forked-from branch", "path", fromPath, "err", err)
	}
}

func (c *Controller) recoverInterruptedTurn(path string) {
	if c.executor == nil || path == "" {
		return
	}
	meta, ok, err := agent.LoadBranchMeta(path)
	if err != nil || !ok || meta.InFlightTurn == nil {
		if err != nil {
			slog.Warn("controller: load in-flight turn marker", "err", err)
		}
		return
	}
	marker := meta.InFlightTurn
	if interruptedTurnContinuedOnRecoveryBranch(path, marker) {

		if _, err := agent.ClearSessionInFlightTurnIfMatch(path, *marker); err != nil {
			slog.Warn("controller: clear fork-orphaned in-flight turn", "err", err)
		}
		return
	}
	msgs := c.executor.Session().Snapshot()
	if marker.CommitDigest != "" {
		if digest, digestErr := c.executor.Session().ContentDigest(); digestErr != nil {
			slog.Warn("controller: digest resumed in-flight turn", "err", digestErr)
		} else if digest == marker.CommitDigest {

			if _, err := agent.ClearSessionInFlightTurnIfMatch(path, *marker); err != nil {
				slog.Warn("controller: clear committed in-flight turn marker", "err", err)
			}
			return
		}
	}
	start, found := resolveInterruptedTurnStart(msgs, marker.StartMessageIndex, marker.PreserveUser, marker.StartedAt, provider.Message{})
	if found && interruptedTurnCrossesLaterTurn(msgs, start) {
		slog.Warn("controller: preserving WAL transcript after stale in-flight marker",
			"path", path, "messages", len(msgs), "marker_index", marker.StartMessageIndex, "resolved_index", start,
			"marker_revision", marker.StartRevision, "current_revision", meta.Revision)
		c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn,
			Text: "Session recovery found completed turns after a stale interruption marker; the full WAL history was preserved."})
		if _, err := agent.ClearSessionInFlightTurnIfMatch(path, *marker); err != nil {
			slog.Warn("controller: clear stale multi-turn in-flight marker", "err", err)
		}
		return
	}
	changed := found && len(msgs) > start
	if changed {
		if marker.PreserveUser {
			c.stripCancelledVisibleTurnMessagesAfterWithFallbackAt(start, provider.Message{}, marker.StartedAt)
		} else {
			c.stripTurnMessagesAfter(start)
		}
		if err := c.snapshot(false, true, false); err != nil {
			slog.Warn("controller: post-interrupted-turn snapshot", "err", err)
		}
	}
	if _, err := agent.ClearSessionInFlightTurnIfMatch(path, *marker); err != nil {
		slog.Warn("controller: clear stale in-flight turn", "err", err)
	}
}

const (
	// conflictDropped: nothing was recovered and the disk transcript could
	// not be adopted; this snapshot was deliberately dropped.
	conflictDropped conflictOutcome = iota
	// conflictAdoptedDisk: the executor session object was replaced by the
	// newer disk transcript; adoptDiskSession already reset its baselines.
	conflictAdoptedDisk
	// conflictForkedBranch: the same in-memory session moved to a freshly
	// forked recovery branch path.
	conflictForkedBranch
)
