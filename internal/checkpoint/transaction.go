package checkpoint

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"time"

	fileenc "reasonix/internal/fileutil/encoding"
)

// InjectFail is a test seam. When set, CommitRewind fails at the named phase
// after optionally publishing the first N files. Empty = disabled.
//
// Known phases: "publish_file", "delete_file", "conversation", "truncate",
// "after_conversation_before_finalize", "finalize".
type InjectFail struct {
	Phase      string
	AfterFiles int // fail after successfully handling this many file targets
}

// ConversationApplier applies conversation truncation during commit and restores
// forward conversation on compensate. Implemented by the control layer.
type ConversationApplier interface {
	// ApplyConversationTruncate replaces the live message log with msgs[:boundary].
	// forward is the full pre-truncate snapshot for later restore.
	ApplyConversationTruncate(boundary int, forward []byte) error
	// RestoreConversation reinstalls the forward snapshot.
	RestoreConversation(forward []byte) error
	// TruncateCheckpoints drops checkpoints at or after turn.
	TruncateCheckpoints(fromTurn int) error
	// RestoreCheckpoints reinstalls backed-up future checkpoints.
	RestoreCheckpoints(backup []byte) error
}

// PrepareRewind builds a plan and optionally a prepared transaction without
// mutating workspace or conversation. Conflict detection uses last-owned after
// fingerprints when available.
func (s *Store) PrepareRewind(turn int, scope RewindScope, sessionRev int64, boundary int, hasBound bool) (RewindPlan, error) {
	if s == nil {
		return RewindPlan{}, fmt.Errorf("checkpoints unavailable")
	}
	plan := RewindPlan{
		PlanID:          newID("plan"),
		Turn:            turn,
		Scope:           scope,
		SessionRevision: sessionRev,
		BoundaryIndex:   boundary,
		HasBoundary:     hasBound,
		CreatedAt:       time.Now(),
		WorkspaceToken:  fmt.Sprintf("%d", s.barrier.Generation()),
	}

	s.mu.Lock()
	writers := append([]ActiveWriter(nil), s.activeWriters...)
	plan.ActiveWriters = writers
	cov, gaps, legacy, expired := s.coverageFromTurnLocked(turn)
	plan.Coverage = cov
	plan.CoverageGaps = gaps
	plan.Legacy = legacy
	plan.ExpiredFilePayload = expired
	files := s.filesFromTurnLocked(turn)
	plan.Files = files
	plan.FileCount = len(files)
	s.mu.Unlock()

	wantFiles := scope == RewindCode || scope == RewindBoth
	wantConv := scope == RewindConversation || scope == RewindBoth

	if len(writers) > 0 {
		plan.CanFiles = false
		plan.CanConversation = false
		plan.DisabledReason = "active background writer"
		for _, w := range writers {
			plan.Conflicts = append(plan.Conflicts, RewindConflict{
				Path:   "",
				Reason: ConflictBusyWriter,
			})
			_ = w
		}
		return plan, nil
	}

	if wantConv {
		if !hasBound {
			plan.CanConversation = false
			if scope == RewindConversation || scope == RewindBoth {
				plan.DisabledReason = "conversation boundary unavailable"
			}
		} else {
			plan.CanConversation = true
		}
	}

	if wantFiles {
		if len(files) == 0 && scope == RewindBoth {
			// The file half of a combined rewind is an atomic no-op when this
			// conversation range never touched a tracked file.
			plan.CanFiles = true
		} else if cov == CoverageNone {
			plan.CanFiles = false
			plan.DisabledReason = "no file captures"
		} else if expired {
			plan.CanFiles = false
			plan.DisabledReason = "file recovery payload expired"
			plan.Conflicts = append(plan.Conflicts, RewindConflict{Reason: ConflictExpired})
		} else if legacy {
			// Legacy: files can be restored only with explicit warning; batch
			// overwrite without prompt is forbidden. Prepare still reports files
			// but CanFiles stays false for the unprompted path.
			plan.CanFiles = false
			plan.DisabledReason = "legacy checkpoint cannot verify later manual edits"
			plan.Conflicts = append(plan.Conflicts, RewindConflict{Reason: ConflictCoverageLegacy})
		} else {
			conflicts := s.precheckFiles(turn)
			plan.Conflicts = append(plan.Conflicts, conflicts...)
			plan.CanFiles = len(conflicts) == 0 && len(files) > 0
			if len(conflicts) > 0 {
				plan.DisabledReason = "file conflicts detected"
			}
		}
	}

	// both requires both sides to pass precheck.
	if scope == RewindBoth {
		if !plan.CanFiles || !plan.CanConversation {
			if plan.DisabledReason == "" {
				plan.DisabledReason = "both scope requires file and conversation precheck"
			}
		}
	}

	// Persist plan token so Commit can verify freshness.
	s.mu.Lock()
	if s.plans == nil {
		s.plans = map[string]preparedPlan{}
	}
	s.plans[plan.PlanID] = preparedPlan{plan: plan, created: time.Now()}
	// Drop stale plans older than 10 minutes.
	for id, p := range s.plans {
		if time.Since(p.created) > 10*time.Minute {
			delete(s.plans, id)
		}
	}
	s.mu.Unlock()
	return plan, nil
}

type preparedPlan struct {
	plan               RewindPlan
	created            time.Time
	previewFingerprint *Fingerprint
}

// ValidatePlanSessionRevision binds a preview to the controller's exact
// conversation revision. The controller holds its rotation gate while calling
// this and committing, so no turn can slip between validation and mutation.
func (s *Store) ValidatePlanSessionRevision(planID string, current int64) error {
	if s == nil {
		return fmt.Errorf("checkpoints unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prepared, ok := s.plans[planID]
	if !ok {
		return fmt.Errorf("unknown or expired plan %q", planID)
	}
	if prepared.plan.SessionRevision != current {
		return fmt.Errorf("conversation changed since preview")
	}
	return nil
}

// CommitRewind executes a previously prepared plan under exclusive barrier.
// conversation/checkpoints are applied via applier when non-nil.
func (s *Store) CommitRewind(planID string, applier ConversationApplier, inject *InjectFail) (RewindResult, error) {
	if s == nil {
		return RewindResult{}, fmt.Errorf("checkpoints unavailable")
	}
	s.mu.Lock()
	pp, ok := s.plans[planID]
	if ok {
		delete(s.plans, planID)
	}
	s.mu.Unlock()
	if !ok {
		return RewindResult{OK: false, Error: "unknown or expired plan"}, fmt.Errorf("unknown or expired plan %q", planID)
	}
	plan := pp.plan

	// Re-validate gate conditions before any mutation.
	if plan.Scope == RewindBoth && (!plan.CanFiles || !plan.CanConversation) {
		return RewindResult{OK: false, Error: plan.DisabledReason, Conflicts: plan.Conflicts, Coverage: plan.Coverage}, fmt.Errorf("%s", plan.DisabledReason)
	}
	if (plan.Scope == RewindCode || plan.Scope == RewindBoth) && !plan.CanFiles && plan.Scope != RewindConversation {
		if plan.Scope == RewindCode || plan.Scope == RewindBoth {
			return RewindResult{OK: false, Error: plan.DisabledReason, Conflicts: plan.Conflicts, Coverage: plan.Coverage}, fmt.Errorf("%s", plan.DisabledReason)
		}
	}
	if (plan.Scope == RewindConversation || plan.Scope == RewindBoth) && !plan.CanConversation {
		return RewindResult{OK: false, Error: plan.DisabledReason}, fmt.Errorf("%s", plan.DisabledReason)
	}

	// Workspace exclusive barrier.
	if !s.barrier.TryEnterExclusive() {
		err := fmt.Errorf("workspace mutation in progress")
		return RewindResult{OK: false, Error: err.Error(), Conflicts: []RewindConflict{{Reason: ConflictBusyWriter}}}, err
	}
	defer s.barrier.ExitExclusive()
	if conflicts := s.activeWriterConflicts(); len(conflicts) > 0 {
		err := fmt.Errorf("active background writer")
		return RewindResult{OK: false, Error: err.Error(), Conflicts: conflicts, Coverage: plan.Coverage}, err
	}

	if plan.Scope == RewindCode || plan.Scope == RewindBoth {
		if plan.WorkspaceToken != fmt.Sprintf("%d", s.barrier.Generation()) {
			conflict := RewindConflict{Reason: ConflictStalePlan}
			return RewindResult{OK: false, Error: "workspace changed since preview", Conflicts: []RewindConflict{conflict}, Coverage: plan.Coverage}, fmt.Errorf("workspace changed since preview")
		}
		conflicts := s.precheckFiles(plan.Turn)
		if len(conflicts) > 0 {
			return RewindResult{OK: false, Error: "file conflicts detected", Conflicts: conflicts, Coverage: plan.Coverage}, fmt.Errorf("file conflicts detected")
		}
	}

	tx, err := s.prepareTransaction(plan, applier)
	if err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}

	result, err := s.commitTransaction(tx, applier, inject)
	return result, err
}

// UndoRewind reverses a committed transaction when still available.
func (s *Store) UndoRewind(transactionID string, applier ConversationApplier) (RewindResult, error) {
	if s == nil {
		return RewindResult{}, fmt.Errorf("checkpoints unavailable")
	}
	s.mu.Lock()
	last := s.lastUndo
	s.mu.Unlock()
	if last == nil || last.ID != transactionID || last.State != TxCommitted {
		return RewindResult{OK: false, Error: "undo not available"}, fmt.Errorf("undo not available for %q", transactionID)
	}

	if !s.barrier.TryEnterExclusive() {
		err := fmt.Errorf("workspace mutation in progress")
		return RewindResult{OK: false, Error: err.Error(), Conflicts: []RewindConflict{{Reason: ConflictBusyWriter}}}, err
	}
	defer s.barrier.ExitExclusive()
	if conflicts := s.activeWriterConflicts(); len(conflicts) > 0 {
		err := fmt.Errorf("active background writer")
		return RewindResult{OK: false, Error: err.Error(), Conflicts: conflicts}, err
	}

	// Precheck that current disk still matches what we published (targets' restore state).
	for _, t := range last.Targets {
		fp, err := FingerprintPath(s.root, t.AbsPath)
		if err != nil && !os.IsNotExist(err) {
			return RewindResult{OK: false, Error: err.Error()}, fmt.Errorf("fingerprint %s before undo: %w", t.Path, err)
		}
		// After commit, disk should match restore image. If it doesn't, refuse.
		if t.Action == "delete" {
			if fp.Existed {
				return RewindResult{OK: false, Error: "file changed since rewind", Conflicts: []RewindConflict{{
					Path: t.Path, Reason: ConflictManualEdit, CurrentSHA: fp.SHA256,
				}}}, fmt.Errorf("file changed since rewind: %s", t.Path)
			}
		} else {
			if !fingerprintMatches(fp, t.RestoreExisted, t.RestoreSHA, t.RestoreMode) {
				restoreExisted := t.RestoreExisted
				return RewindResult{OK: false, Error: "file changed since rewind", Conflicts: []RewindConflict{{
					Path: t.Path, Reason: CompareIdentity(fp, t.RestoreSHA, &restoreExisted, t.RestoreMode),
					CurrentSHA: fp.SHA256, LastOwnedSHA: t.RestoreSHA, CurrentMode: fp.Mode, CheckpointMode: t.RestoreMode,
				}}}, fmt.Errorf("file changed since rewind: %s", t.Path)
			}
		}
	}

	// Build inverse transaction: restore forward images.
	undo := &TransactionManifest{
		SchemaVersion:       SchemaV2,
		ID:                  newID("tx"),
		SessionID:           last.SessionID,
		WorkspaceRoot:       last.WorkspaceRoot,
		State:               TxPrepared,
		Kind:                "undo",
		Turn:                last.Turn,
		Scope:               last.Scope,
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
		SessionRevision:     last.SessionRevision,
		ParentTransaction:   last.ID,
		HasBoundary:         last.HasBoundary,
		BoundaryIndex:       last.BoundaryIndex,
		ConversationForward: last.ConversationForward,
		CheckpointBackup:    last.CheckpointBackup,
		TruncateFrom:        last.TruncateFrom,
	}
	for _, t := range last.Targets {
		inv := TransactionTarget{
			Path:           t.Path,
			AbsPath:        t.AbsPath,
			RestoreExisted: t.ForwardExisted,
			RestoreMode:    t.ForwardMode,
			RestoreSHA:     t.ForwardSHA,
			RestoreBlob:    t.ForwardBlob,
			RestoreInline:  clonePayload(t.ForwardInline),
			ForwardExisted: t.RestoreExisted,
			ForwardMode:    t.RestoreMode,
			ForwardSHA:     t.RestoreSHA,
			ForwardBlob:    t.RestoreBlob,
			ForwardInline:  clonePayload(t.RestoreInline),
		}
		if t.ForwardExisted {
			inv.Action = "write"
		} else {
			inv.Action = "delete"
		}
		inv.PublishTmp, inv.BackupPath = transactionSiblingPaths(inv.AbsPath, undo.ID, len(undo.Targets))
		if inv.Action != "write" {
			inv.PublishTmp = ""
		}
		undo.Targets = append(undo.Targets, inv)
	}
	if err := s.persistTransaction(undo); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}

	// Stage publish temps for write targets.
	for i := range undo.Targets {
		t := &undo.Targets[i]
		if t.Action != "write" {
			continue
		}
		data, err := s.loadBlobOrInline(t.RestoreBlob, t.RestoreInline)
		if err != nil {
			s.cleanupPublishTemps(undo.Targets)
			_ = s.abortTransaction(undo, err)
			return RewindResult{OK: false, Error: err.Error()}, err
		}
		mode := os.FileMode(0o644)
		if t.RestoreMode != 0 {
			mode = os.FileMode(t.RestoreMode)
		}
		if err := s.writePublishTemp(t.PublishTmp, data, mode); err != nil {
			s.cleanupPublishTemps(undo.Targets)
			_ = s.abortTransaction(undo, err)
			return RewindResult{OK: false, Error: err.Error()}, err
		}
	}
	undo.State = TxPrepared
	if err := s.persistTransaction(undo); err != nil {
		err = s.failTransaction(undo, undo.Targets, nil, err)
		return RewindResult{OK: false, Error: err.Error()}, err
	}

	// For undo of conversation: restore forward conversation and checkpoints.
	// Commit path for undo: publish files, then restore conversation/checkpoints.
	result, err := s.commitUndoTransaction(undo, last, applier)
	return result, err
}

func (s *Store) commitUndoTransaction(undo, original *TransactionManifest, applier ConversationApplier) (RewindResult, error) {
	undo.State = TxCommitting
	undo.UpdatedAt = time.Now()
	if err := s.persistTransaction(undo); err != nil {
		err = s.failTransaction(undo, undo.Targets, nil, err)
		return RewindResult{OK: false, Error: err.Error()}, err
	}

	result := RewindResult{TransactionID: undo.ID, Coverage: CoverageComplete}
	var stages []FileStage

	// Publish files (inverse).
	for i := range undo.Targets {
		t := &undo.Targets[i]
		st := FileStage{Path: t.Path, Phase: "commit", Action: t.Action}
		t.Published = true
		undo.UpdatedAt = time.Now()
		if err := s.persistTransaction(undo); err != nil {
			t.Published = false
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(undo, undo.Targets, stages, err)
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		if err := s.publishTarget(t); err != nil {
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(undo, undo.Targets[:i+1], stages, err)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		undo.UpdatedAt = time.Now()
		if err := s.persistTransaction(undo); err != nil {
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(undo, undo.Targets[:i+1], stages, err)
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		st.Phase = "done"
		stages = append(stages, st)
		if t.Action == "write" {
			result.Written = append(result.Written, t.Path)
		} else {
			result.Deleted = append(result.Deleted, t.Path)
		}
	}

	// Restore conversation and checkpoints to pre-rewind state.
	if applier != nil && len(original.ConversationForward) > 0 {
		if err := applier.RestoreConversation(original.ConversationForward); err != nil {
			restoreErr := s.restoreOriginalRewind(original, applier)
			err = s.failTransactionAfterStateCompensation(undo, undo.Targets, stages, err, restoreErr)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		result.ConversationOK = true
	}
	if applier != nil && len(original.CheckpointBackup) > 0 {
		if err := applier.RestoreCheckpoints(original.CheckpointBackup); err != nil {
			// Return every side to the original rewind state before compensating
			// the inverse file publish. Re-restoring the forward conversation here
			// would leave conversation and files at opposite endpoints.
			restoreErr := s.restoreOriginalRewind(original, applier)
			err = s.failTransactionAfterStateCompensation(undo, undo.Targets, stages, err, restoreErr)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
	}

	// Controller appliers restore this same store and then rebuild their boundary
	// index. Only the store-only path needs a direct restore here.
	if applier == nil && len(original.CheckpointBackup) > 0 {
		if err := s.restoreCheckpointBackup(original.CheckpointBackup); err != nil {
			err = s.failTransactionAfterStateCompensation(undo, undo.Targets, stages, err, nil)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
	}

	undo.State = TxCommitted
	undo.UpdatedAt = time.Now()
	if err := s.persistTransaction(undo); err != nil {
		restoreErr := s.restoreOriginalRewind(original, applier)
		err = s.failTransactionAfterStateCompensation(undo, undo.Targets, stages, err, restoreErr)
		result.OK = false
		result.Error = err.Error()
		result.Files = stages
		return result, err
	}

	// Mark original as undone; clear lastUndo.
	original.State = TxUndone
	original.UpdatedAt = time.Now()
	if err := s.persistTransaction(original); err != nil {
		// The committed undo manifest durably names its parent, so startup will
		// suppress the stale parent even if this secondary write failed.
		slog.Warn("checkpoint: persist original transaction as undone", "err", err)
	}
	s.mu.Lock()
	s.lastUndo = nil
	s.mu.Unlock()

	result.OK = true
	result.UndoAvailable = false
	result.Files = stages
	return result, nil
}

func (s *Store) restoreOriginalRewind(original *TransactionManifest, applier ConversationApplier) error {
	if original == nil || applier == nil {
		return nil
	}
	if original.Scope != RewindConversation && original.Scope != RewindBoth {
		return nil
	}
	var err error
	if original.HasBoundary {
		err = errors.Join(err, applier.ApplyConversationTruncate(original.BoundaryIndex, original.ConversationForward))
	}
	err = errors.Join(err, applier.TruncateCheckpoints(original.TruncateFrom))
	return err
}

func (s *Store) prepareTransaction(plan RewindPlan, applier ConversationApplier) (*TransactionManifest, error) {
	tx := &TransactionManifest{
		SchemaVersion:   SchemaV2,
		ID:              newID("tx"),
		WorkspaceRoot:   s.root,
		State:           TxPrepared,
		Kind:            "rewind",
		Turn:            plan.Turn,
		Scope:           plan.Scope,
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
		SessionRevision: plan.SessionRevision,
		WorkspaceToken:  plan.WorkspaceToken,
		Coverage:        plan.Coverage,
		CoverageGaps:    append([]CoverageGap(nil), plan.CoverageGaps...),
		BoundaryIndex:   plan.BoundaryIndex,
		HasBoundary:     plan.HasBoundary,
		TruncateFrom:    plan.Turn,
	}
	prepared := false
	defer func() {
		if !prepared {
			s.cleanupPublishTemps(tx.Targets)
		}
	}()

	if plan.Scope == RewindCode || plan.Scope == RewindBoth {
		earliest := s.earliestRevisions(plan.Turn)
		// Stable order for deterministic inject tests.
		paths := make([]string, 0, len(earliest))
		for p := range earliest {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for targetIndex, p := range paths {
			rev := earliest[p]
			abs, err := safePath(s.root, p)
			if err != nil {
				return nil, err
			}
			// Capture forward image.
			fwd, gap, err := CapturePath(abs, CaptureOptions{WorkspaceRoot: s.root, ReadContent: true})
			if err != nil && gap != nil {
				return nil, fmt.Errorf("capture forward %s: %w", p, err)
			}
			t := TransactionTarget{
				Path:            p,
				AbsPath:         abs,
				RestoreExisted:  rev.Existed,
				RestoreMode:     rev.Mode,
				RestoreSHA:      rev.SHA256,
				RestoreBlob:     rev.BlobRef,
				RestoreEncoding: rev.Encoding,
				ForwardExisted:  fwd.Existed,
				ForwardMode:     fwd.Mode,
				ForwardSHA:      fwd.SHA256,
			}
			if rev.Existed {
				t.Action = "write"
				if t.RestoreBlob == "" && rev.Content == nil {
					return nil, fmt.Errorf("missing restore payload for %s", p)
				}
				// Stage publish temp. Blobs hold raw on-disk bytes; inline
				// Content is decoded text and must be re-encoded. Legacy v1
				// snapshots often omit Encoding — fall back to the current
				// file's encoding (same as the pre-v2 RestoreCode path).
				var data []byte
				if rev.BlobRef != "" {
					var lerr error
					data, lerr = s.loadRevisionBytes(rev)
					if lerr != nil {
						return nil, lerr
					}
				} else if rev.Content != nil {
					enc := fileenc.UTF8
					if rev.Encoding != nil {
						enc = *rev.Encoding
					} else if current := s.detectCurrentEncoding(abs); current != nil {
						enc = *current
					}
					data = fileenc.Encode(*rev.Content, enc)
				} else {
					return nil, fmt.Errorf("missing restore payload for %s", p)
				}
				mode := os.FileMode(0o644)
				if rev.Mode != 0 {
					mode = os.FileMode(rev.Mode)
				}
				if t.RestoreBlob == "" && s.blobs != nil {
					ref, err := s.blobs.Put(data)
					if err != nil {
						return nil, err
					}
					t.RestoreBlob = ref
				} else if t.RestoreBlob == "" {
					t.RestoreInline = clonePayload(data)
				}
				t.PublishTmp, t.BackupPath = transactionSiblingPaths(abs, tx.ID, targetIndex)
				if err := s.writePublishTemp(t.PublishTmp, data, mode); err != nil {
					return nil, err
				}
			} else {
				t.Action = "delete"
				_, t.BackupPath = transactionSiblingPaths(abs, tx.ID, targetIndex)
			}
			if fwd.Existed && s.blobs != nil {
				ref, err := s.blobs.Put(fwd.Content)
				if err != nil {
					if t.PublishTmp != "" {
						_ = secureRemove(s.root, t.PublishTmp)
					}
					return nil, err
				}
				t.ForwardBlob = ref
			} else if fwd.Existed {
				t.ForwardInline = clonePayload(fwd.Content)
			}
			// Backup existing file for delete path (move later at commit).
			tx.Targets = append(tx.Targets, t)
		}
	}

	if (plan.Scope == RewindConversation || plan.Scope == RewindBoth) && applier != nil {
		// Backup future checkpoints for undo.
		backup, err := s.backupCheckpointsFrom(plan.Turn)
		if err != nil {
			return nil, err
		}
		tx.CheckpointBackup = backup
	}

	if err := s.persistTransaction(tx); err != nil {
		return nil, err
	}
	prepared = true
	return tx, nil
}

// Persist a conservative "may have published" intent before the first
// filesystem rename. Recovery can safely compensate even if the crash
// happened just before publish.

// Deliberately leave the durable state as committing to simulate a
// process crash at the narrowest progress-persistence window.

// Conversation after files.

// Controller supplies forward via ApplyConversationTruncate.

// Simulate process death after both conversation mutations are durable but
// before the transaction can be marked committed. Startup must restore the
// forward transcript/checkpoints before compensating files.

// SetConversationForward attaches the pre-truncate conversation snapshot to a
// prepared transaction before commit. The controller calls this after Prepare.

// Also check in-memory last prepare path: store plans don't hold tx yet.
// Commit builds tx fresh; controller should pass forward via Commit options.

// CommitRewindWithForward is CommitRewind plus conversation forward payload.

// Published is a durable intent. If the target still exactly matches its
// forward image, the crash happened before publish and compensation is a
// no-op. Any other unrelated state is preserved with a recovery copy.

// Crash window: publishTarget durably records Published before moving the
// target to its backup. A process death after that first rename leaves the
// target absent, the forward image in BackupPath, and (for writes) the
// publish temp still present. Recognize that owned intermediate state before
// classifying the absent target as an external modification.

// Forward did not exist — remove what we published.

// External rewrite of a file we restored then someone changed —
// for compensate of delete action inverse: leave it.

// failTransactionAfterStateCompensation compensates files and records whether
// the conversation/checkpoint side was also restored. Any incomplete side keeps
// the manifest committing so startup can retry the whole compensation.

// Do not make a failed compensation terminal. Startup recovery retries
// committing manifests; marking this aborted would strand a half-applied
// workspace permanently.

// RecoverTransactions scans for incomplete file-only transactions. Conversation
// transactions are intentionally deferred until the controller has installed the
// resumed session and can provide a ConversationApplier.

// RecoverTransactionsWithApplier finishes startup recovery after the resumed
// conversation is live. A committing rewind first restores its forward
// transcript/checkpoints, then compensates files; a committing undo first
// reapplies its parent rewind, then compensates files. The manifest remains
// committing if either side fails so a later startup can retry idempotently.

// Never published — safe to discard.

// Compensate published files back to forward images.

// Keep as last undo if newer.

// unreadable etc.

// Prefer after fingerprint for conflict detection.

// Legacy handled at plan level; skip per-file for batch.

// Preserve the earliest preimage, but carry forward the final
// mutation's ownership identity. Missing final identity deliberately
// clears an older proof instead of authorizing an unsafe restore.

// v1 had no schemaVersion field

// Merge future checkpoints back (by turn).

// Rebuild done/cur: highest turn as cur if it was cur; else all in done.

// RestoreCheckpointBackupPublic reloads backed-up checkpoints after an undo.

// PrepareFileRevert prepares a single-file restore to the earliest session preimage.

// A v1 or incomplete capture has a preimage but no evidence that the
// current file is still the session's last write. Do not turn the
// generic conflict-overwrite affordance into an unsafe legacy restore.

// CommitFileRevert commits a single-file restore.

// Restore via restoreCodeLegacy for the single earliest path using turn 0.
// Build synthetic order of one path.

// Find which turn first touched this path for RestoreCode semantics:
// restoring one file = write earliest preimage (not all files from a turn).
