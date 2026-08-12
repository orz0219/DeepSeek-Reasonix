package repair

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// CancelPendingUpdate removes a transaction that failed before control was
// handed to the replacement build. A version mismatch is intentionally inert.
func CancelPendingUpdate(toVersion string) error {
	return cancelPendingUpdateInvocation(toVersion, "", "")
}

// CancelPendingUpdateMatching removes only the exact transaction prepared by
// the caller. It is used by updater failure paths where a same-version retry can
// replace pending-update.json before cleanup runs.
func CancelPendingUpdateMatching(toVersion, expectedCreatedAt string) error {
	expectedCreatedAt = strings.TrimSpace(expectedCreatedAt)
	if expectedCreatedAt == "" {
		return nil
	}
	return cancelPendingUpdateInvocation(toVersion, expectedCreatedAt, "")
}

// CancelPendingUpdateExact removes only the complete transaction returned by
// prepare. This is the updater failure path: copied creation timestamps are not
// sufficient authorization if pending-update.json itself was rewritten.
func CancelPendingUpdateExact(expected *UpdateTransaction) error {
	if expected == nil {
		return fmt.Errorf("cancel pending update: transaction identity is incomplete")
	}
	return cancelPendingUpdateInvocation(
		expected.ToVersion,
		expected.CreatedAt,
		repairPlanStateID(expected),
	)
}

func cancelPendingUpdateInvocation(toVersion, expectedCreatedAt, expectedTransactionID string) error {
	tx, stateID, _, err := readPendingUpdateInvocation()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(toVersion) != strings.TrimSpace(tx.ToVersion) {
		return nil
	}
	if expected := strings.TrimSpace(expectedCreatedAt); expected != "" && expected != strings.TrimSpace(tx.CreatedAt) {
		return nil
	}
	if expected := strings.TrimSpace(expectedTransactionID); expected != "" && expected != UpdateTransactionID(tx) {
		return fmt.Errorf("cancel pending update: pending transaction changed")
	}
	return cancelPendingUpdateMatching(
		tx.ToVersion,
		tx.CreatedAt,
		UpdateTransactionID(tx),
		stateID,
	)
}

func cancelPendingUpdateMatching(toVersion, expectedCreatedAt, expectedTransactionID, expectedStateID string) error {
	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return fmt.Errorf("cancel pending update: lock pending transaction: %w", err)
	}
	defer unlock()
	tx, err := ReadPendingUpdate()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if strings.TrimSpace(toVersion) != strings.TrimSpace(tx.ToVersion) {
		return nil
	}
	if expected := strings.TrimSpace(expectedCreatedAt); expected != "" && expected != strings.TrimSpace(tx.CreatedAt) {
		return nil
	}
	if expected := strings.TrimSpace(expectedTransactionID); expected != "" && expected != repairPlanStateID(tx) {
		return fmt.Errorf("cancel pending update: pending transaction changed")
	}
	unlockTargets, lockErr := lockRepairMutations(pendingUpdateTargetPaths(tx)...)
	if lockErr != nil {
		return fmt.Errorf("cancel pending update: lock targets: %w", lockErr)
	}
	defer unlockTargets()
	current, err := ReadPendingUpdate()
	if err != nil {
		return fmt.Errorf("cancel pending update: re-read pending transaction: %w", err)
	}
	if !reflect.DeepEqual(tx, current) {
		return fmt.Errorf("cancel pending update: pending transaction changed while waiting")
	}
	tx = current
	verifyCancellationState := func() error {
		actual, _ := pendingUpdateBoundPreview(tx)
		if strings.TrimSpace(expectedStateID) != actual {
			return fmt.Errorf("cancel pending update: pending update state changed while waiting")
		}
		switch tx.TargetKind {
		case "app-bundle":
			if err := VerifyAppBundleUpdateHandoffOriginal(tx); err != nil {
				return fmt.Errorf("cancel pending update: %w", err)
			}
			if err := verifyAppBundleUpdateHandoffBackupAbsent(tx); err != nil {
				return fmt.Errorf("cancel pending update: %w", err)
			}
		case "file":
			if _, bound, err := installedFileUpdateTargets(tx, false); err != nil {
				return fmt.Errorf("cancel pending update: %w", err)
			} else if bound {
				return fmt.Errorf("cancel pending update: installed release-unit state is already recorded")
			}
			if err := verifyPreparedFileUpdateTargets(tx); err != nil {
				return fmt.Errorf("cancel pending update: %w", err)
			}
		default:
			return fmt.Errorf("cancel pending update: unsupported target kind %q", tx.TargetKind)
		}
		return nil
	}
	if err := verifyCancellationState(); err != nil {
		return err
	}
	if err := removePendingUpdateExactVerified(tx, verifyCancellationState); err != nil {
		return err
	}
	if tx.TargetKind == "file" {
		removeUpdateBackups(tx)
	}
	return nil
}

func removeUpdateBackups(tx *UpdateTransaction) {
	if tx == nil {
		return
	}
	if tx.TargetKind == "app-bundle" {
		_ = removeUpdateBackupTreeMatching(tx.BackupPath, tx.BackupTreeID)
		return
	}
	seen := map[string]struct{}{}
	for _, f := range pendingUpdateFiles(tx) {
		if f.MissingBefore || strings.TrimSpace(f.BackupPath) == "" {
			continue
		}
		key := canonicalRepairPath(f.BackupPath)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		_ = removeUpdateBackupFileMatching(f.BackupPath, f.SHA256)
	}
}

func removeUpdateBackupFileMatching(path, expectedSHA256 string) error {
	expectedSHA256 = strings.TrimSpace(expectedSHA256)
	if path == "" || expectedSHA256 == "" {
		return nil
	}
	return removeUpdateNodeMatching(path, func(moved string) error {
		info, err := os.Lstat(moved)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("update backup changed type")
		}
		actual, err := hashFile(moved)
		if err != nil {
			return err
		}
		if !strings.EqualFold(actual, expectedSHA256) {
			return fmt.Errorf("update backup hash changed")
		}
		return nil
	}, false)
}

func removeUpdateBackupTreeMatching(path, expectedTreeID string) error {
	expectedTreeID = strings.TrimSpace(expectedTreeID)
	if path == "" || expectedTreeID == "" {
		return nil
	}
	return removeUpdateNodeMatching(path, func(moved string) error {
		actual, err := repairPlanTreeContentStateID(moved)
		if err != nil {
			return err
		}
		if actual != expectedTreeID {
			return fmt.Errorf("update backup tree changed")
		}
		return nil
	}, true)
}

func removeUpdateNodeMatching(path string, verify func(string) error, directory bool) error {
	cleanup, err := moveRepairNodeToUniqueCleanup(path)
	if err != nil || cleanup == "" {
		return err
	}
	updateCleanupAfterRename(path, cleanup)
	restore := func(cause error) error {
		if restoreErr := renameRepairNodeNoReplace(cleanup, path); restoreErr != nil {
			return fmt.Errorf("%w; changed update node retained at %s: %w", cause, cleanup, restoreErr)
		}
		return cause
	}
	if err := verify(cleanup); err != nil {
		return restore(err)
	}
	if directory {
		return os.RemoveAll(cleanup)
	}
	if err := os.Remove(cleanup); err != nil {
		return restore(err)
	}
	return nil
}

func RollbackPendingUpdate() (UpdateRollbackResult, error) {
	return rollbackPendingUpdateInvocation("", "", "")
}

// RollbackPendingUpdateMatching rolls back only the exact transaction prepared
// by the caller. This is used when an apply attempt fails after another process
// may already have replaced pending-update.json with a same-version retry.
func RollbackPendingUpdateMatching(expectedToVersion, expectedCreatedAt string) (UpdateRollbackResult, error) {
	expectedToVersion = strings.TrimSpace(expectedToVersion)
	expectedCreatedAt = strings.TrimSpace(expectedCreatedAt)
	if expectedToVersion == "" || expectedCreatedAt == "" {
		return UpdateRollbackResult{}, fmt.Errorf("rollback update: transaction identity is incomplete")
	}
	return rollbackPendingUpdateInvocation(expectedToVersion, expectedCreatedAt, "")
}

func rollbackPendingUpdateState(expectedStateID string, expectedStates map[string]string) (UpdateRollbackResult, error) {
	return rollbackPendingUpdateMatching("", "", expectedStateID, expectedStates, "", true)
}

// RollbackPendingUpdateExact restores only the complete transaction returned by
// prepare. It is used after a platform apply failure where a later same-version
// transaction must remain untouched.
func RollbackPendingUpdateExact(expected *UpdateTransaction) (UpdateRollbackResult, error) {
	if expected == nil {
		return UpdateRollbackResult{}, fmt.Errorf("rollback update: transaction identity is incomplete")
	}
	return rollbackPendingUpdateInvocation(
		expected.ToVersion,
		expected.CreatedAt,
		repairPlanStateID(expected),
	)
}

func rollbackPendingUpdateInvocation(
	expectedToVersion, expectedCreatedAt, expectedTransactionID string,
) (UpdateRollbackResult, error) {
	tx, stateID, states, err := readPendingUpdateInvocation()
	if err != nil {
		if os.IsNotExist(err) {
			return UpdateRollbackResult{}, nil
		}
		return UpdateRollbackResult{}, err
	}
	if expected := strings.TrimSpace(expectedToVersion); expected != "" && expected != strings.TrimSpace(tx.ToVersion) {
		return UpdateRollbackResult{}, nil
	}
	if expected := strings.TrimSpace(expectedCreatedAt); expected != "" && expected != strings.TrimSpace(tx.CreatedAt) {
		return UpdateRollbackResult{}, nil
	}
	if expected := strings.TrimSpace(expectedTransactionID); expected != "" && expected != UpdateTransactionID(tx) {
		return UpdateRollbackResult{}, fmt.Errorf("rollback update: pending transaction changed")
	}
	return rollbackPendingUpdateMatching(
		tx.ToVersion,
		tx.CreatedAt,
		stateID,
		states,
		UpdateTransactionID(tx),
		false,
	)
}

func rollbackPendingUpdateMatching(
	expectedToVersion, expectedCreatedAt, expectedStateID string,
	expectedStates map[string]string,
	expectedTransactionID string,
	callerConfirmedState bool,
) (UpdateRollbackResult, error) {

	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return UpdateRollbackResult{}, fmt.Errorf("rollback update: lock pending transaction: %w", err)
	}
	defer unlock()
	return rollbackPendingUpdateMatchingLocked(
		expectedToVersion,
		expectedCreatedAt,
		expectedStateID,
		expectedStates,
		expectedTransactionID,
		callerConfirmedState,
	)
}

// rollbackPendingUpdateMatchingLocked performs the transition while the caller
// holds the pending-update lock. RecoverFailedInstall uses this form so failure
// marker correlation, rollback, and marker cleanup are one serialized state
// transition.
func rollbackPendingUpdateMatchingLocked(
	expectedToVersion, expectedCreatedAt, expectedStateID string,
	expectedStates map[string]string,
	expectedTransactionID string,
	callerConfirmedState bool,
) (UpdateRollbackResult, error) {
	tx, err := ReadPendingUpdate()
	if err != nil {
		if os.IsNotExist(err) {
			return UpdateRollbackResult{}, nil
		}
		return UpdateRollbackResult{}, err
	}
	if expected := strings.TrimSpace(expectedToVersion); expected != "" && expected != strings.TrimSpace(tx.ToVersion) {
		return UpdateRollbackResult{}, nil
	}
	if expected := strings.TrimSpace(expectedCreatedAt); expected != "" && expected != strings.TrimSpace(tx.CreatedAt) {
		return UpdateRollbackResult{}, nil
	}
	if expected := strings.TrimSpace(expectedTransactionID); expected != "" && expected != repairPlanStateID(tx) {
		return UpdateRollbackResult{}, fmt.Errorf("rollback update: pending transaction changed")
	}
	hasBoundState := strings.TrimSpace(expectedStateID) != ""
	if !hasBoundState {
		expectedStateID, expectedStates = pendingUpdateBoundPreview(tx)
	}

	unlockTargets, lockErr := lockRepairMutations(pendingUpdateTargetPaths(tx)...)
	if lockErr != nil {
		return UpdateRollbackResult{}, fmt.Errorf("rollback update: lock targets: %w", lockErr)
	}
	defer unlockTargets()
	current, err := ReadPendingUpdate()
	if err != nil {
		return UpdateRollbackResult{}, fmt.Errorf("rollback update: re-read pending transaction: %w", err)
	}
	if !reflect.DeepEqual(tx, current) {
		return UpdateRollbackResult{}, fmt.Errorf("rollback update: pending transaction changed while waiting")
	}
	tx = current
	verifyBoundState := func() error {
		expected := strings.TrimSpace(expectedStateID)
		if expected == "" {
			return nil
		}
		actual, _ := pendingUpdateBoundPreview(tx)
		if expected != actual {
			return fmt.Errorf("repair plan preview changed since confirmation; re-preview and re-confirm (expected %s, got %s)", expected, actual)
		}
		return nil
	}
	if expected := strings.TrimSpace(expectedStateID); expected != "" {
		if err := verifyBoundState(); err != nil {
			if callerConfirmedState {
				return UpdateRollbackResult{}, nil
			}
			return UpdateRollbackResult{}, err
		}
	}
	confirmedStates := expectedStates
	if !callerConfirmedState {

		confirmedStates = nil
	}
	result := UpdateRollbackResult{FromVersion: tx.ToVersion, ToVersion: tx.FromVersion, TargetPath: tx.TargetPath}
	var verifyCommitState func() error
	switch tx.TargetKind {
	case "file":
		files, _, installedErr := installedFileUpdateTargets(tx, false)
		if installedErr != nil {
			return result, fmt.Errorf("rollback update: %w", installedErr)
		}

		for _, f := range files {
			if f.MissingBefore {
				continue
			}
			if strings.TrimSpace(f.SHA256) == "" {
				return result, fmt.Errorf("rollback update: backup hash missing for %s", filepath.Base(f.TargetPath))
			}
			got, hashErr := hashFile(f.BackupPath)
			if hashErr != nil || !strings.EqualFold(got, f.SHA256) {
				return result, fmt.Errorf("rollback update: backup hash mismatch for %s", filepath.Base(f.TargetPath))
			}
		}
		mixed, restoreErr := restoreReleaseUnit(files, verifyBoundState, confirmedStates)
		if restoreErr != nil {
			result.MixedInstall = mixed
			return result, fmt.Errorf("rollback update: %w", restoreErr)
		}
		verifyCommitState = func() error {
			return verifyRestoredFileUpdateTargets(files)
		}
	case "app-bundle":
		confirmedBackupState := strings.TrimSpace(confirmedStates[tx.BackupPath])
		if strings.TrimSpace(tx.BackupTreeID) == "" &&
			(!callerConfirmedState || confirmedBackupState == "") {
			return result, fmt.Errorf("rollback update: backup bundle identity is missing; explicit preview confirmation is required")
		}
		backupInfo, err := os.Lstat(tx.BackupPath)
		if err != nil {
			if os.IsNotExist(err) && strings.TrimSpace(tx.BackupTreeID) != "" {
				actual, digestErr := repairPlanTreeContentStateID(tx.TargetPath)
				if digestErr == nil && actual == tx.BackupTreeID {
					result.RolledBack = true
					if removeErr := removePendingUpdateExactVerified(tx, func() error {
						current, currentErr := repairPlanTreeContentStateID(tx.TargetPath)
						if currentErr != nil || current != tx.BackupTreeID {
							return fmt.Errorf("rollback update: restored bundle changed before commit")
						}
						return nil
					}); removeErr != nil {
						return result, fmt.Errorf("rollback update: clear pending transaction: %w", removeErr)
					}
					return result, nil
				}
			}
			return result, fmt.Errorf("rollback update: backup bundle: %w", err)
		}
		if !backupInfo.IsDir() {
			return result, fmt.Errorf("rollback update: backup bundle is not a directory")
		}
		if tx.BackupTreeID != "" {
			actual, digestErr := repairPlanTreeContentStateID(tx.BackupPath)
			if digestErr != nil || actual != tx.BackupTreeID {
				return result, fmt.Errorf("rollback update: backup bundle digest mismatch")
			}
		} else if err := verifyRepairPlanReleaseNodeStateFor(
			tx.BackupPath,
			tx.BackupPath,
			confirmedBackupState,
		); err != nil {
			return result, fmt.Errorf("rollback update: confirmed backup bundle changed: %w", err)
		}
		if err := verifyBoundState(); err != nil {
			return result, err
		}
		failed := ""
		retainedFailed := false
		retainedFailedOwned := false
		retainedFailedState := ""
		if _, statErr := os.Lstat(tx.TargetPath); statErr == nil {
			retainedFailedState = repairPlanReleaseNodeState(tx.TargetPath)
			var retainErr error
			failed, retainErr = retainUpdateRollbackNode(tx.TargetPath, "reasonix-failed")
			if retainErr != nil {
				return result, fmt.Errorf("rollback update: move failed bundle: %w", retainErr)
			}
			retainedFailed = true
			if verifyErr := verifyRepairPlanReleaseNodeStateFor(failed, tx.TargetPath, retainedFailedState); verifyErr != nil {
				if restoreErr := rollbackSwapRename(failed, tx.TargetPath); restoreErr != nil {
					result.MixedInstall = true
					return result, fmt.Errorf("%w; preserve moved live bundle at %s: %w", verifyErr, failed, restoreErr)
				}
				return result, verifyErr
			}
			if strings.TrimSpace(tx.HandoffAppTreeID) != "" {
				retainedFailedOwned = VerifyAppBundleUpdateHandoffReplacement(tx, failed) == nil
			}
			if expected := confirmedStates[tx.TargetPath]; expected != "" {
				if verifyErr := verifyRepairPlanReleaseNodeStateFor(failed, tx.TargetPath, expected); verifyErr != nil {
					if restoreErr := rollbackSwapRename(failed, tx.TargetPath); restoreErr != nil {
						result.MixedInstall = true
						return result, fmt.Errorf("%w; preserve moved live bundle at %s: %w", verifyErr, failed, restoreErr)
					}
					return result, verifyErr
				}
				retainedFailedOwned = true
			}
		} else if !os.IsNotExist(statErr) {
			return result, fmt.Errorf("rollback update: inspect live bundle: %w", statErr)
		}
		if _, statErr := os.Lstat(tx.TargetPath); statErr == nil {
			result.MixedInstall = retainedFailed
			return result, fmt.Errorf("rollback update: target bundle was recreated before restore")
		} else if !os.IsNotExist(statErr) {
			result.MixedInstall = retainedFailed
			return result, fmt.Errorf("rollback update: inspect restore target: %w", statErr)
		}
		if err := rollbackSwapRename(tx.BackupPath, tx.TargetPath); err != nil {
			if retainedFailed {
				if verifyErr := verifyRepairPlanReleaseNodeStateFor(failed, tx.TargetPath, retainedFailedState); verifyErr != nil {
					result.MixedInstall = true
					return result, fmt.Errorf("rollback update: restore bundle: %w (retained live bundle changed at %s: %w)", err, failed, verifyErr)
				}
				if restoreErr := rollbackSwapRename(failed, tx.TargetPath); restoreErr != nil {
					result.MixedInstall = true
					return result, fmt.Errorf("rollback update: restore bundle: %w (preserve replacement at %s: %w)", err, failed, restoreErr)
				}
			}
			return result, fmt.Errorf("rollback update: restore bundle: %w", err)
		}
		restoredTreeID, digestErr := repairPlanTreeContentStateID(tx.TargetPath)
		restoredMatches := digestErr == nil
		if strings.TrimSpace(tx.BackupTreeID) != "" {
			restoredMatches = restoredMatches && restoredTreeID == tx.BackupTreeID
		} else if restoredMatches {
			restoredMatches = verifyRepairPlanReleaseNodeStateFor(
				tx.TargetPath,
				tx.BackupPath,
				confirmedBackupState,
			) == nil
		}
		if !restoredMatches {
			mismatchErr := fmt.Errorf("rollback update: restored bundle digest mismatch")
			rejected, moveErr := moveRepairNodeToUniqueCleanup(tx.TargetPath)
			if moveErr != nil || rejected == "" {
				result.MixedInstall = true
				if moveErr != nil {
					return result, fmt.Errorf("%w; retain rejected bundle: %w", mismatchErr, moveErr)
				}
				return result, fmt.Errorf("%w; rejected bundle disappeared before compensation", mismatchErr)
			}
			if !retainedFailed {
				result.MixedInstall = true
				return result, fmt.Errorf("%w; rejected bundle retained at %s and no prior live bundle is available", mismatchErr, rejected)
			}
			if verifyErr := verifyRepairPlanReleaseNodeStateFor(failed, tx.TargetPath, retainedFailedState); verifyErr != nil {
				result.MixedInstall = true
				return result, fmt.Errorf("%w; rejected bundle retained at %s; prior live bundle changed at %s: %w", mismatchErr, rejected, failed, verifyErr)
			}
			if restoreErr := rollbackSwapRename(failed, tx.TargetPath); restoreErr != nil {
				result.MixedInstall = true
				return result, fmt.Errorf("%w; rejected bundle retained at %s; restore prior live bundle: %w", mismatchErr, rejected, restoreErr)
			}
			if verifyErr := verifyRepairPlanReleaseNodeStateFor(tx.TargetPath, tx.TargetPath, retainedFailedState); verifyErr != nil {
				result.MixedInstall = true
				return result, fmt.Errorf("%w; rejected bundle retained at %s; restored prior live bundle changed: %w", mismatchErr, rejected, verifyErr)
			}
			return result, fmt.Errorf("%w; rejected bundle retained at %s and prior live bundle restored", mismatchErr, rejected)
		}
		verifyCommitState = func() error {
			current, currentErr := repairPlanTreeContentStateID(tx.TargetPath)
			if currentErr != nil {
				return fmt.Errorf("rollback update: read restored bundle before commit: %w", currentErr)
			}
			if current != restoredTreeID {
				return fmt.Errorf("rollback update: restored bundle changed before commit")
			}
			return nil
		}
		if retainedFailed && retainedFailedOwned {
			_ = removeUpdateNodeMatching(failed, func(moved string) error {
				return verifyRepairPlanReleaseNodeStateFor(moved, tx.TargetPath, retainedFailedState)
			}, true)
		}
	default:
		return result, fmt.Errorf("rollback update: unsupported target kind %q", tx.TargetKind)
	}
	if err := verifyCommitState(); err != nil {
		return result, err
	}
	result.RolledBack = true
	if err := removePendingUpdateExactVerified(tx, verifyCommitState); err != nil {
		return result, fmt.Errorf("rollback update: clear pending transaction: %w", err)
	}
	if tx.TargetKind == "file" {
		_ = removeInstalledFileUpdateState(tx)
	}
	return result, nil
}

func retainUpdateRollbackNode(path, suffix string) (string, error) {
	for attempt := range 16 {
		retained := fmt.Sprintf(
			"%s.%s-%d-%d",
			path,
			suffix,
			time.Now().UTC().UnixNano(),
			attempt,
		)
		if err := rollbackSwapRename(path, retained); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		return retained, nil
	}
	return "", fmt.Errorf("cannot allocate retained update path")
}

func verifyRestoredFileUpdateTargets(files []UpdateTransactionFile) error {
	for _, f := range files {
		info, err := os.Lstat(f.TargetPath)
		if f.MissingBefore {
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("verify restored release unit %s: %w", filepath.Base(f.TargetPath), err)
			}
			return fmt.Errorf("verify restored release unit %s: unexpected file appeared", filepath.Base(f.TargetPath))
		}
		if err != nil {
			return fmt.Errorf("verify restored release unit %s: %w", filepath.Base(f.TargetPath), err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("verify restored release unit %s: file changed type", filepath.Base(f.TargetPath))
		}
		got, hashErr := hashFile(f.TargetPath)
		if hashErr != nil {
			return fmt.Errorf("verify restored release unit %s: %w", filepath.Base(f.TargetPath), hashErr)
		}
		if !strings.EqualFold(got, f.SHA256) {
			return fmt.Errorf("verify restored release unit %s: hash mismatch", filepath.Base(f.TargetPath))
		}
	}
	return nil
}
