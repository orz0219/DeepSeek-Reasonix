package repair

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
)

// ReconcilePendingUpdate resolves an older immutable update transaction before
// startup or a new install. It first attempts the narrow cancellation path,
// which succeeds only while every original target still matches the state
// captured by prepare and no replacement state is durable. If publication has
// started, it falls back to the exact verified rollback path. Both transitions
// re-read the complete transaction under the pending and target mutation locks.
//
// A transaction targeting runningVersion is left untouched only when the
// transaction also proves that its replacement release unit is installed and
// its rollback state is intact. Version equality alone is not installation
// evidence: a same-version/manual launch may observe an abandoned prepare.
func ReconcilePendingUpdate(runningVersion string) (PendingUpdateReconcileResult, error) {
	disposition, tx, classifyErr := classifyPendingUpdate()
	if classifyErr != nil {
		return PendingUpdateReconcileResult{Pending: true}, fmt.Errorf("reconcile pending update: %w", classifyErr)
	}
	switch {
	case disposition == pendingUpdateNone:
		return PendingUpdateReconcileResult{}, nil
	case disposition == pendingUpdateDebris:

		digest, digestErr := pendingUpdateMarkerDigest(PendingUpdatePath())
		if digestErr != nil {
			if os.IsNotExist(digestErr) {
				return PendingUpdateReconcileResult{}, nil
			}
			return PendingUpdateReconcileResult{Pending: true}, fmt.Errorf("reconcile pending update: read marker: %w", digestErr)
		}
		quarantined, err := quarantinePendingUpdateAfterReconcile("no recoverable transaction to reconcile", digest, false)
		if err != nil {
			return PendingUpdateReconcileResult{Pending: true}, fmt.Errorf("reconcile pending update: quarantine unusable transaction: %w", err)
		}
		if !quarantined {
			return PendingUpdateReconcileResult{}, nil
		}
		return PendingUpdateReconcileResult{Pending: true, Cleared: true}, nil
	case tx == nil:

		digest, digestErr := pendingUpdateMarkerDigest(PendingUpdatePath())
		if digestErr != nil {
			if os.IsNotExist(digestErr) {
				return PendingUpdateReconcileResult{}, nil
			}
			return PendingUpdateReconcileResult{Pending: true}, fmt.Errorf("reconcile pending update: read marker: %w", digestErr)
		}
		quarantined, err := quarantinePendingUpdateAfterReconcile("not valid for this installation", digest, true)
		if err != nil {
			return PendingUpdateReconcileResult{Pending: true}, fmt.Errorf("reconcile pending update: quarantine unusable transaction: %w", err)
		}
		if !quarantined {
			return PendingUpdateReconcileResult{}, nil
		}
		return PendingUpdateReconcileResult{Pending: true, Cleared: true}, nil
	}
	result := PendingUpdateReconcileResult{
		Pending:     true,
		FromVersion: tx.FromVersion,
		ToVersion:   tx.ToVersion,
		TargetPath:  tx.TargetPath,
	}
	if UpdateVersionsEqual(runningVersion, tx.ToVersion) &&
		pendingUpdateInstalledForHealth(tx) {

		if pendingUpdateHealthIsStale(tx) {
			if healErr := MarkUpdateHealthy(runningVersion); healErr == nil && !PendingUpdateExists() {
				result.Pending = false
				result.Healthy = true
				result.Cleared = true
				return result, nil
			} else if healErr != nil {
				slog.Warn("repair: stale probationary update could not be committed automatically",
					"toVersion", tx.ToVersion, "err", healErr)
			}
		}
		result.AwaitingHealth = true
		return result, ErrPendingUpdateAwaitingHealth
	}

	if cancelErr := CancelPendingUpdateExact(tx); cancelErr == nil {

		current, currentErr := ReadPendingUpdate()
		if os.IsNotExist(currentErr) {
			result.Cleared = true
			cleanupPendingUpdateStaging(tx)
			return result, nil
		}
		if currentErr != nil {
			return result, fmt.Errorf("reconcile pending update: verify cancellation: %w", currentErr)
		}
		if !reflect.DeepEqual(tx, current) {
			return result, fmt.Errorf("reconcile pending update: pending transaction changed during cancellation")
		}
		return result, fmt.Errorf("reconcile pending update: transaction remained after cancellation")
	}

	rollback, rollbackErr := RollbackPendingUpdateExact(tx)
	if rollbackErr != nil {
		result.RolledBack = rollback.RolledBack
		result.MixedInstall = rollback.MixedInstall
		return result, fmt.Errorf("reconcile pending update: %w", rollbackErr)
	}
	if !rollback.RolledBack {

		if _, currentErr := ReadPendingUpdate(); os.IsNotExist(currentErr) {
			return PendingUpdateReconcileResult{}, nil
		}
		return result, fmt.Errorf("reconcile pending update: transaction could not be cancelled or rolled back")
	}
	result.RolledBack = true
	cleanupPendingUpdateStaging(tx)
	return result, nil
}

// CommitProbationaryPendingUpdate commits a still-running probationary update
// when install evidence matches. Returns true when the marker is gone.
func CommitProbationaryPendingUpdate(runningVersion string) (bool, error) {
	if strings.TrimSpace(runningVersion) == "" {
		return false, nil
	}
	tx, err := ReadPendingUpdate()
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	if !UpdateVersionsEqual(runningVersion, tx.ToVersion) || !pendingUpdateInstalledForHealth(tx) {
		return false, nil
	}
	if err := MarkUpdateHealthy(runningVersion); err != nil {
		return false, err
	}
	return !PendingUpdateExists(), nil
}

// AbandonPendingUpdate is the user-initiated recovery path for a stuck
// transaction: commit if possible, else reconcile, else force-retire.
func AbandonPendingUpdate(runningVersion string) (PendingUpdateReconcileResult, error) {
	committed, commitErr := CommitProbationaryPendingUpdate(runningVersion)
	if commitErr == nil && committed {
		return PendingUpdateReconcileResult{Cleared: true, Healthy: true}, nil
	}
	if commitErr != nil {

		slog.Debug("repair: probationary commit during abandon failed; continuing",
			"err", commitErr)
	}
	result, reconcileErr := ReconcilePendingUpdate(runningVersion)
	if reconcileErr == nil {
		return result, nil
	}

	if errors.Is(reconcileErr, ErrPendingUpdateAwaitingHealth) {
		if retired, retireErr := forceRetireProbationaryPendingUpdate(runningVersion); retireErr != nil {
			return result, fmt.Errorf("abandon pending update: %w", retireErr)
		} else if retired {
			result.Pending = false
			result.AwaitingHealth = false
			result.Healthy = true
			result.Cleared = true
			return result, nil
		}
	}
	if commitErr != nil && reconcileErr != nil {
		return result, fmt.Errorf("abandon pending update: %w", errors.Join(reconcileErr, commitErr))
	}
	return result, reconcileErr
}

// forceRetireProbationaryPendingUpdate retires a probationary marker when the
// live target is installed but MarkUpdateHealthy cannot finish (e.g. bad backup).
func forceRetireProbationaryPendingUpdate(runningVersion string) (bool, error) {
	tx, err := ReadPendingUpdate()
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}

	if !UpdateVersionsEqual(runningVersion, tx.ToVersion) || !pendingUpdateTargetInstalled(tx) {
		return false, nil
	}
	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return false, fmt.Errorf("force retire probationary update: lock pending transaction: %w", err)
	}
	defer unlock()
	current, err := ReadPendingUpdate()
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	if UpdateTransactionID(current) != UpdateTransactionID(tx) {
		return false, fmt.Errorf("force retire probationary update: pending transaction changed")
	}
	if !UpdateVersionsEqual(runningVersion, current.ToVersion) || !pendingUpdateTargetInstalled(current) {
		return false, nil
	}
	unlockTargets, lockErr := lockRepairMutations(pendingUpdateTargetPaths(current)...)
	if lockErr != nil {
		return false, fmt.Errorf("force retire probationary update: lock targets: %w", lockErr)
	}
	defer unlockTargets()
	recheck, err := ReadPendingUpdate()
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	if !reflect.DeepEqual(current, recheck) {
		return false, fmt.Errorf("force retire probationary update: pending transaction changed while waiting")
	}
	verifyInstalled := func() error {
		if !pendingUpdateTargetInstalled(recheck) {
			return fmt.Errorf("installed target no longer matches the pending transaction")
		}
		return nil
	}
	if err := removePendingUpdateExactVerified(recheck, verifyInstalled); err != nil {
		return false, err
	}
	removeUpdateBackups(recheck)
	slog.Warn("repair: force-retired a probationary pending update after explicit abandon",
		"toVersion", recheck.ToVersion, "target", recheck.TargetPath)
	return true, nil
}

// pendingUpdateTargetInstalled reports live replacement install evidence only
// (no rollback backup requirement). Used by force-retire on explicit abandon.
func pendingUpdateTargetInstalled(tx *UpdateTransaction) bool {
	if tx == nil {
		return false
	}
	switch tx.TargetKind {
	case "app-bundle":
		return VerifyAppBundleUpdateHandoffTarget(tx) == nil
	case "file":
		_, bound, err := installedFileUpdateTargets(tx, true)
		return err == nil && bound
	default:
		return false
	}
}

// pendingUpdateInstalledForHealth requires transaction-bound evidence for the
// complete replacement and rollback unit. It intentionally treats missing or
// drifted evidence as uninstalled so reconciliation can take the existing
// exact cancel/rollback paths instead of trusting a version string.
func pendingUpdateInstalledForHealth(tx *UpdateTransaction) bool {
	if tx == nil {
		return false
	}
	switch tx.TargetKind {
	case "app-bundle":
		return VerifyAppBundleUpdateHandoffTarget(tx) == nil &&
			VerifyAppBundleUpdateHandoffBackup(tx) == nil
	case "file":
		_, bound, err := installedFileUpdateTargets(tx, true)
		return err == nil && bound
	default:
		return false
	}
}

// cleanupPendingUpdateStaging is best-effort after the pending transaction has
// been safely committed away. CleanupAppBundleUpdateHandoffStaging verifies the
// complete recorded tree before removal, so drifted or recreated paths survive.
func cleanupPendingUpdateStaging(tx *UpdateTransaction) {
	if tx == nil || tx.TargetKind != "app-bundle" ||
		strings.TrimSpace(tx.HandoffStagingPath) == "" ||
		strings.TrimSpace(tx.HandoffStagingTreeID) == "" {
		return
	}
	_ = CleanupAppBundleUpdateHandoffStaging(tx)
}

func readPendingUpdateInvocation() (*UpdateTransaction, string, map[string]string, error) {
	tx, err := ReadPendingUpdate()
	if err != nil {
		return nil, "", nil, err
	}
	stateID, states := pendingUpdateBoundPreview(tx)
	return tx, stateID, states, nil
}

// MarkUpdateHealthy commits a probationary update and removes its backup. A
// version mismatch is ignored so an older process cannot bless a newer update.
func MarkUpdateHealthy(runningVersion string) error {
	return markUpdateHealthyInvocation(runningVersion, "", "")
}

// MarkUpdateHealthyMatching commits only the exact pending transaction observed
// when this desktop process started. The creation identity prevents an older
// process from blessing a later same-version retry.
func MarkUpdateHealthyMatching(runningVersion, expectedCreatedAt string) error {
	expectedCreatedAt = strings.TrimSpace(expectedCreatedAt)
	if expectedCreatedAt == "" {
		return nil
	}
	return markUpdateHealthyInvocation(runningVersion, expectedCreatedAt, "")
}

// MarkUpdateHealthyExact commits only the complete transaction captured before
// the replacement process started.
func MarkUpdateHealthyExact(runningVersion, expectedCreatedAt, expectedTransactionID string) error {
	expectedCreatedAt = strings.TrimSpace(expectedCreatedAt)
	expectedTransactionID = strings.TrimSpace(expectedTransactionID)
	if expectedCreatedAt == "" || expectedTransactionID == "" {
		return nil
	}
	return markUpdateHealthyInvocation(runningVersion, expectedCreatedAt, expectedTransactionID)
}

func markUpdateHealthyInvocation(runningVersion, expectedCreatedAt, expectedTransactionID string) error {
	tx, stateID, _, err := readPendingUpdateInvocation()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !UpdateVersionsEqual(runningVersion, tx.ToVersion) {
		return nil
	}
	if expected := strings.TrimSpace(expectedCreatedAt); expected != "" && expected != strings.TrimSpace(tx.CreatedAt) {
		return nil
	}
	if expected := strings.TrimSpace(expectedTransactionID); expected != "" && expected != UpdateTransactionID(tx) {
		return fmt.Errorf("mark update healthy: pending transaction changed")
	}
	return markUpdateHealthyMatching(
		runningVersion,
		tx.CreatedAt,
		UpdateTransactionID(tx),
		stateID,
	)
}

func markUpdateHealthyMatching(runningVersion, expectedCreatedAt, expectedTransactionID, expectedStateID string) error {
	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return fmt.Errorf("mark update healthy: lock pending transaction: %w", err)
	}
	defer unlock()
	tx, err := ReadPendingUpdate()
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !UpdateVersionsEqual(runningVersion, tx.ToVersion) {
		return nil
	}
	if expected := strings.TrimSpace(expectedCreatedAt); expected != "" && expected != strings.TrimSpace(tx.CreatedAt) {
		return nil
	}
	if expected := strings.TrimSpace(expectedTransactionID); expected != "" && expected != UpdateTransactionID(tx) {
		return fmt.Errorf("mark update healthy: pending transaction changed")
	}
	unlockTargets, lockErr := lockRepairMutations(pendingUpdateTargetPaths(tx)...)
	if lockErr != nil {
		return fmt.Errorf("mark update healthy: lock targets: %w", lockErr)
	}
	defer unlockTargets()
	current, err := ReadPendingUpdate()
	if err != nil {
		return fmt.Errorf("mark update healthy: re-read pending transaction: %w", err)
	}
	if !reflect.DeepEqual(tx, current) {
		return fmt.Errorf("mark update healthy: pending transaction changed while waiting")
	}
	tx = current
	verifyInvocationState := func() error {
		actual, _ := pendingUpdateBoundPreview(tx)
		if strings.TrimSpace(expectedStateID) != actual {
			return fmt.Errorf("mark update healthy: pending update state changed while waiting")
		}
		return nil
	}
	verifyHealthyState := func() error {
		if err := verifyInvocationState(); err != nil {
			return err
		}
		switch tx.TargetKind {
		case "app-bundle":
			if strings.TrimSpace(tx.HandoffAppTreeID) == "" {
				return fmt.Errorf("mark update healthy: installed bundle state is missing")
			}
			if err := VerifyAppBundleUpdateHandoffTarget(tx); err != nil {
				return fmt.Errorf("mark update healthy: %w", err)
			}
			if strings.TrimSpace(tx.BackupTreeID) == "" {
				return fmt.Errorf("mark update healthy: rollback backup state is missing")
			}
			if err := VerifyAppBundleUpdateHandoffBackup(tx); err != nil {
				return fmt.Errorf("mark update healthy: %w", err)
			}
		case "file":
			if err := verifyPreparedFileUpdateBackups(tx); err != nil {
				return fmt.Errorf("mark update healthy: %w", err)
			}
			if _, _, err := installedFileUpdateTargets(tx, true); err != nil {
				return fmt.Errorf("mark update healthy: %w", err)
			}
		}
		return nil
	}
	if err := verifyHealthyState(); err != nil {
		return err
	}
	if err := removePendingUpdateExactVerified(tx, verifyHealthyState); err != nil {
		return err
	}
	removeUpdateBackups(tx)
	_ = removeInstalledFileUpdateState(tx)
	return nil
}
