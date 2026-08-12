package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

func transactionSiblingPaths(absPath, transactionID string, index int) (publish, backup string) {
	dir := filepath.Dir(absPath)
	base := filepath.Base(absPath)
	prefix := fmt.Sprintf(".%s.reasonix-%s-%d", base, transactionID, index)
	return filepath.Join(dir, prefix+".tmp"), filepath.Join(dir, prefix+".bak")
}

func (s *Store) writePublishTemp(path string, data []byte, mode os.FileMode) error {
	if err := secureWriteNew(s.root, path, data, mode); err != nil {
		return fmt.Errorf("create publish temp: %w", err)
	}
	return nil
}

func (s *Store) cleanupPublishTemps(targets []TransactionTarget) {
	for _, target := range targets {
		if target.PublishTmp != "" {
			_ = secureRemove(s.root, target.PublishTmp)
		}
	}
}

func (s *Store) commitTransaction(tx *TransactionManifest, applier ConversationApplier, inject *InjectFail) (RewindResult, error) {
	tx.State = TxCommitting
	tx.UpdatedAt = time.Now()
	if err := s.persistTransaction(tx); err != nil {
		err = s.failTransaction(tx, tx.Targets, nil, err)
		return RewindResult{OK: false, Error: err.Error()}, err
	}

	result := RewindResult{TransactionID: tx.ID, Coverage: tx.Coverage, CoverageGaps: append([]CoverageGap(nil), tx.CoverageGaps...)}
	stages := make([]FileStage, 0, len(tx.Targets))
	filesDone := 0

	for i := range tx.Targets {
		t := &tx.Targets[i]
		st := FileStage{Path: t.Path, Phase: "commit", Action: t.Action}
		phase := "publish_file"
		if t.Action == "delete" {
			phase = "delete_file"
		}
		if inject != nil && inject.Phase == phase && filesDone >= inject.AfterFiles {
			err := fmt.Errorf("injected failure at %s after %d files", inject.Phase, inject.AfterFiles)
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(tx, tx.Targets[:i], stages, err)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}

		t.Published = true
		tx.UpdatedAt = time.Now()
		if err := s.persistTransaction(tx); err != nil {
			t.Published = false
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(tx, tx.Targets, stages, err)
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		if err := s.publishTarget(t); err != nil {
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(tx, tx.Targets[:i+1], stages, err)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		if inject != nil && inject.Phase == "after_publish_before_progress" && filesDone >= inject.AfterFiles {

			err := fmt.Errorf("injected crash after publish before progress")
			result.Error = err.Error()
			result.Files = append(stages, st)
			return result, err
		}
		tx.UpdatedAt = time.Now()
		if err := s.persistTransaction(tx); err != nil {
			st.Error = err.Error()
			stages = append(stages, st)
			err = s.failTransaction(tx, tx.Targets[:i+1], stages, err)
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		st.Phase = "done"
		stages = append(stages, st)
		filesDone++
		if t.Action == "write" {
			result.Written = append(result.Written, t.Path)
		} else {
			result.Deleted = append(result.Deleted, t.Path)
		}
	}
	if err := s.persistTransaction(tx); err != nil {
		err = s.failTransaction(tx, tx.Targets, stages, err)
		result.Error = err.Error()
		result.Files = stages
		return result, err
	}

	if tx.Scope == RewindConversation || tx.Scope == RewindBoth {
		if inject != nil && inject.Phase == "conversation" {
			err := fmt.Errorf("injected failure at conversation")
			err = s.failTransaction(tx, tx.Targets, stages, err)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		if applier != nil && tx.HasBoundary {

			if err := applier.ApplyConversationTruncate(tx.BoundaryIndex, tx.ConversationForward); err != nil {
				restoreErr := s.restoreTransactionConversation(tx, applier)
				err = s.failTransactionAfterStateCompensation(tx, tx.Targets, stages, err, restoreErr)
				result.OK = false
				result.Error = err.Error()
				result.Files = stages
				return result, err
			}
			result.ConversationOK = true
		}
		if inject != nil && inject.Phase == "truncate" {
			err := fmt.Errorf("injected failure at truncate")
			restoreErr := s.restoreTransactionConversation(tx, applier)
			err = s.failTransactionAfterStateCompensation(tx, tx.Targets, stages, err, restoreErr)
			result.OK = false
			result.Error = err.Error()
			result.Files = stages
			return result, err
		}
		if applier != nil {
			if err := applier.TruncateCheckpoints(tx.TruncateFrom); err != nil {
				restoreErr := s.restoreTransactionConversation(tx, applier)
				err = s.failTransactionAfterStateCompensation(tx, tx.Targets, stages, err, restoreErr)
				result.OK = false
				result.Error = err.Error()
				result.Files = stages
				return result, err
			}
		} else {
			if err := s.TruncateFrom(tx.TruncateFrom); err != nil {
				restoreErr := s.restoreTransactionConversation(tx, applier)
				err = s.failTransactionAfterStateCompensation(tx, tx.Targets, stages, err, restoreErr)
				result.OK = false
				result.Error = err.Error()
				result.Files = stages
				return result, err
			}
		}
	}

	if inject != nil && inject.Phase == "finalize" {
		err := fmt.Errorf("injected failure at finalize")
		restoreErr := s.restoreTransactionConversation(tx, applier)
		err = s.failTransactionAfterStateCompensation(tx, tx.Targets, stages, err, restoreErr)
		result.OK = false
		result.Error = err.Error()
		result.Files = stages
		return result, err
	}
	if inject != nil && inject.Phase == "after_conversation_before_finalize" {

		err := fmt.Errorf("injected crash after conversation before finalize")
		result.Error = err.Error()
		result.Files = stages
		return result, err
	}

	tx.State = TxCommitted
	tx.UpdatedAt = time.Now()
	if err := s.persistTransaction(tx); err != nil {
		restoreErr := s.restoreTransactionConversation(tx, applier)
		err = s.failTransactionAfterStateCompensation(tx, tx.Targets, stages, err, restoreErr)
		result.OK = false
		result.Error = err.Error()
		result.Files = stages
		return result, err
	}
	s.mu.Lock()
	s.lastUndo = tx
	s.mu.Unlock()

	result.OK = true
	result.UndoAvailable = true
	result.Files = stages
	return result, nil
}

func (s *Store) restoreTransactionConversation(tx *TransactionManifest, applier ConversationApplier) error {
	if tx == nil {
		return nil
	}
	var restoreErr error
	if applier != nil {
		if len(tx.ConversationForward) > 0 {
			restoreErr = errors.Join(restoreErr, applier.RestoreConversation(tx.ConversationForward))
		}
		if len(tx.CheckpointBackup) > 0 {
			restoreErr = errors.Join(restoreErr, applier.RestoreCheckpoints(tx.CheckpointBackup))
		}
	} else if len(tx.CheckpointBackup) > 0 {
		restoreErr = errors.Join(restoreErr, s.restoreCheckpointBackup(tx.CheckpointBackup))
	}
	return restoreErr
}

// SetConversationForward attaches the pre-truncate conversation snapshot to a
// prepared transaction before commit. The controller calls this after Prepare.
func (s *Store) SetConversationForward(txID string, forward []byte) error {
	path := s.txManifestPath(txID)
	var tx TransactionManifest
	if err := readJSONFile(path, &tx); err != nil {

		return err
	}
	tx.ConversationForward = forward
	tx.UpdatedAt = time.Now()
	return s.persistTransaction(&tx)
}

// CommitRewindWithForward is CommitRewind plus conversation forward payload.
func (s *Store) CommitRewindWithForward(planID string, forward []byte, applier ConversationApplier, inject *InjectFail) (RewindResult, error) {
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

	if plan.Scope == RewindBoth && (!plan.CanFiles || !plan.CanConversation) {
		return RewindResult{OK: false, Error: plan.DisabledReason, Conflicts: plan.Conflicts}, fmt.Errorf("%s", plan.DisabledReason)
	}
	if plan.Scope == RewindCode && !plan.CanFiles {
		return RewindResult{OK: false, Error: plan.DisabledReason, Conflicts: plan.Conflicts}, fmt.Errorf("%s", plan.DisabledReason)
	}
	if (plan.Scope == RewindConversation || plan.Scope == RewindBoth) && !plan.CanConversation {
		return RewindResult{OK: false, Error: plan.DisabledReason}, fmt.Errorf("%s", plan.DisabledReason)
	}

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
		if conflicts := s.precheckFiles(plan.Turn); len(conflicts) > 0 {
			return RewindResult{OK: false, Error: "file conflicts detected", Conflicts: conflicts}, fmt.Errorf("file conflicts detected")
		}
	}

	tx, err := s.prepareTransaction(plan, applier)
	if err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	tx.ConversationForward = forward
	if err := s.persistTransaction(tx); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	return s.commitTransaction(tx, applier, inject)
}

func (s *Store) publishTarget(t *TransactionTarget) error {
	if t.BackupPath == "" {
		return fmt.Errorf("missing transaction backup path for %s", t.Path)
	}
	backupExists, err := securePathExists(s.root, t.BackupPath)
	if err != nil {
		return err
	}
	if backupExists {
		return fmt.Errorf("transaction backup already exists for %s", t.Path)
	}
	targetExists, err := securePathExists(s.root, t.AbsPath)
	if err != nil {
		return err
	}
	if targetExists {
		if err := secureRename(s.root, t.AbsPath, t.BackupPath); err != nil {
			return fmt.Errorf("backup %s: %w", t.Path, err)
		}
	}
	if t.Action == "delete" {
		return nil
	}
	if t.PublishTmp == "" {
		return fmt.Errorf("missing publish tmp for %s", t.Path)
	}
	if err := secureRename(s.root, t.PublishTmp, t.AbsPath); err != nil {
		restoreErr := error(nil)
		if exists, statErr := securePathExists(s.root, t.BackupPath); statErr == nil && exists {
			restoreErr = secureRename(s.root, t.BackupPath, t.AbsPath)
		}
		return errors.Join(fmt.Errorf("publish %s: %w", t.Path, err), restoreErr)
	}
	if t.RestoreMode != 0 {
		if err := secureChmod(s.root, t.AbsPath, os.FileMode(t.RestoreMode)); err != nil {
			return fmt.Errorf("chmod restored %s: %w", t.Path, err)
		}
	}
	return nil
}

func (s *Store) compensatePublished(targets []TransactionTarget, stages []FileStage) error {
	var first error
	for _, v := range slices.Backward(targets) {
		t := v
		if !t.Published {
			if t.PublishTmp != "" {
				_ = secureRemove(s.root, t.PublishTmp)
			}
			continue
		}
		// Published is a durable intent. If the target still exactly matches its
		// forward image, the crash happened before publish and compensation is a
		// no-op. Any other unrelated state is preserved with a recovery copy.
		var err error
		cur, fpErr := FingerprintPath(s.root, t.AbsPath)
		if fpErr != nil {
			markCompensationStage(stages, t.Path, fpErr)
			if first == nil {
				first = fpErr
			}
			continue
		} else if fingerprintMatches(cur, t.ForwardExisted, t.ForwardSHA, t.ForwardMode) {
			if t.PublishTmp != "" {
				_ = secureRemove(s.root, t.PublishTmp)
			}
			markCompensationStage(stages, t.Path, nil)
			continue
		}

		if t.ForwardExisted && !cur.Existed && t.BackupPath != "" {
			backup, backupErr := FingerprintPath(s.root, t.BackupPath)
			publishPending := t.Action == "delete"
			if t.Action == "write" && t.PublishTmp != "" {
				publishPending, _ = securePathExists(s.root, t.PublishTmp)
			}
			if backupErr == nil && publishPending && fingerprintMatches(backup, true, t.ForwardSHA, t.ForwardMode) {
				err = secureRename(s.root, t.BackupPath, t.AbsPath)
				if err == nil && t.PublishTmp != "" {
					if removeErr := secureRemove(s.root, t.PublishTmp); removeErr != nil && !os.IsNotExist(removeErr) {
						err = removeErr
					}
				}
				markCompensationStage(stages, t.Path, err)
				if err != nil && first == nil {
					first = err
				}
				continue
			}
		}
		if t.ForwardExisted {
			data, lerr := s.loadBlobOrInline(t.ForwardBlob, t.ForwardInline)
			if lerr != nil && t.BackupPath != "" {
				data, lerr = secureReadFile(s.root, t.BackupPath)
			}
			if !fingerprintMatches(cur, t.RestoreExisted, t.RestoreSHA, t.RestoreMode) {
				if lerr == nil {
					suffix := t.RestoreSHA
					if len(suffix) > 8 {
						suffix = suffix[:8]
					}
					if suffix == "" {
						suffix = "unknown"
					}
					recov := t.AbsPath + ".reasonix-recovery-" + suffix
					_ = secureWriteNew(s.root, recov, data, os.FileMode(t.ForwardMode))
					err = fmt.Errorf("external modification after publish; recovery copy at %s", recov)
				} else {
					err = lerr
				}
			} else if backupExists, backupErr := securePathExists(s.root, t.BackupPath); backupErr == nil && backupExists {
				if cur.Existed {
					err = secureRemove(s.root, t.AbsPath)
				}
				if err == nil {
					err = secureRename(s.root, t.BackupPath, t.AbsPath)
				}
			} else if lerr != nil {
				err = lerr
			} else {
				mode := os.FileMode(0o644)
				if t.ForwardMode != 0 {
					mode = os.FileMode(t.ForwardMode)
				}
				if cur.Existed {
					if werr := secureRemove(s.root, t.AbsPath); werr != nil {
						err = werr
					}
				}
				tmp, _ := transactionSiblingPaths(t.AbsPath, newID("compensate"), 0)
				if werr := s.writePublishTemp(tmp, data, mode); werr != nil {
					err = werr
				} else if err == nil {
					if werr := secureRename(s.root, tmp, t.AbsPath); werr != nil {
						err = werr
					}
				}
			}
		} else {

			if !fingerprintMatches(cur, t.RestoreExisted, t.RestoreSHA, t.RestoreMode) {

				err = fmt.Errorf("external modification; not removing %s", t.AbsPath)
			} else {
				err = secureRemove(s.root, t.AbsPath)
				if os.IsNotExist(err) {
					err = nil
				}
			}
		}
		markCompensationStage(stages, t.Path, err)
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

func fingerprintMatches(fp Fingerprint, existed bool, sha string, mode uint32) bool {
	if fp.Existed != existed {
		return false
	}
	if !existed {
		return true
	}
	if sha != "" && fp.SHA256 != sha {
		return false
	}
	return mode == 0 || fp.Mode == 0 || fp.Mode == mode
}

func markCompensationStage(stages []FileStage, path string, err error) {
	for i := range stages {
		if stages[i].Path != path {
			continue
		}
		stages[i].Compensated = err == nil
		if err != nil {
			stages[i].CompError = err.Error()
		}
	}
}
