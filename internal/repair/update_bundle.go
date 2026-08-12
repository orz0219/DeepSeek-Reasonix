package repair

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// CancelPendingAppBundleUpdateHandoff abandons an exact handoff only when the
// original installed bundle is still the tree captured during prepare. This is
// the safe recovery path when source verification fails after the desktop has
// exited but before any bundle swap occurred.
func CancelPendingAppBundleUpdateHandoff(
	expectedToVersion, expectedCreatedAt string,
	timeout time.Duration,
) (*UpdateTransaction, error) {
	tx, err := ReadPendingUpdate()
	if err != nil {
		return nil, fmt.Errorf("cancel update handoff: read pending transaction: %w", err)
	}
	if tx.TargetKind != "app-bundle" ||
		strings.TrimSpace(tx.ToVersion) != strings.TrimSpace(expectedToVersion) ||
		strings.TrimSpace(tx.CreatedAt) != strings.TrimSpace(expectedCreatedAt) {
		return nil, fmt.Errorf("cancel update handoff: pending transaction does not match")
	}
	return cancelPendingAppBundleUpdateHandoff(
		expectedToVersion,
		expectedCreatedAt,
		timeout,
		UpdateTransactionID(tx),
	)
}

// CancelPendingAppBundleUpdateHandoffExact abandons only the full transaction
// read or prepared by the caller. It is safe to use after a PID wait or failed
// claim where pending state may have been rewritten with copied scalar IDs.
func CancelPendingAppBundleUpdateHandoffExact(
	expected *UpdateTransaction,
	timeout time.Duration,
) (*UpdateTransaction, error) {
	if expected == nil {
		return nil, fmt.Errorf("cancel update handoff: transaction identity is incomplete")
	}
	return cancelPendingAppBundleUpdateHandoff(
		expected.ToVersion,
		expected.CreatedAt,
		timeout,
		repairPlanStateID(expected),
	)
}

func cancelPendingAppBundleUpdateHandoff(
	expectedToVersion, expectedCreatedAt string,
	timeout time.Duration,
	expectedTransactionID string,
) (*UpdateTransaction, error) {
	expectedToVersion = strings.TrimSpace(expectedToVersion)
	expectedCreatedAt = strings.TrimSpace(expectedCreatedAt)
	if expectedToVersion == "" || expectedCreatedAt == "" {
		return nil, fmt.Errorf("cancel update handoff: transaction identity is incomplete")
	}
	unlockPending, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, fmt.Errorf("cancel update handoff: lock pending transaction: %w", err)
	}
	defer unlockPending()
	tx, err := ReadPendingUpdate()
	if err != nil {
		return nil, fmt.Errorf("cancel update handoff: read pending transaction: %w", err)
	}
	if tx.TargetKind != "app-bundle" ||
		strings.TrimSpace(tx.ToVersion) != expectedToVersion ||
		strings.TrimSpace(tx.CreatedAt) != expectedCreatedAt {
		return nil, fmt.Errorf("cancel update handoff: pending transaction does not match")
	}
	if expected := strings.TrimSpace(expectedTransactionID); expected != "" && expected != repairPlanStateID(tx) {
		return nil, fmt.Errorf("cancel update handoff: pending transaction changed")
	}
	unlockTargets, err := lockRepairMutationsTimeout(timeout, pendingUpdateTargetPaths(tx)...)
	if err != nil {
		return nil, fmt.Errorf("cancel update handoff: lock targets: %w", err)
	}
	defer unlockTargets()
	current, err := ReadPendingUpdate()
	if err != nil {
		return nil, fmt.Errorf("cancel update handoff: re-read pending transaction: %w", err)
	}
	if !reflect.DeepEqual(tx, current) {
		return nil, fmt.Errorf("cancel update handoff: pending transaction changed while waiting")
	}
	if err := VerifyAppBundleUpdateHandoffOriginal(current); err != nil {
		return nil, fmt.Errorf("cancel update handoff: %w", err)
	}
	if err := verifyAppBundleUpdateHandoffBackupAbsent(current); err != nil {
		return nil, fmt.Errorf("cancel update handoff: %w", err)
	}
	if err := removePendingUpdateExactVerified(current, func() error {
		if err := VerifyAppBundleUpdateHandoffOriginal(current); err != nil {
			return fmt.Errorf("cancel update handoff: %w", err)
		}
		if err := verifyAppBundleUpdateHandoffBackupAbsent(current); err != nil {
			return fmt.Errorf("cancel update handoff: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return current, nil
}

func sameRepairMutationPaths(a, b []string) bool {
	keys := func(paths []string) []string {
		seen := make(map[string]struct{}, len(paths))
		result := make([]string, 0, len(paths))
		for _, path := range paths {
			key := canonicalRepairPath(path)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, key)
		}
		sort.Strings(result)
		return result
	}
	return reflect.DeepEqual(keys(a), keys(b))
}

// VerifyAppBundleUpdateHandoffSource checks the real staging containment and
// the complete staged tree immediately before a handoff mutates the install.
// The lexical metadata check remains readable after staging cleanup, while this
// stronger check is only used while the source bundle still exists.
func VerifyAppBundleUpdateHandoffSource(tx *UpdateTransaction) error {
	if tx == nil || tx.TargetKind != "app-bundle" {
		return fmt.Errorf("handoff source transaction is invalid")
	}
	if strings.TrimSpace(tx.HandoffAppTreeID) == "" {
		return fmt.Errorf("handoff source digest is missing")
	}
	if strings.TrimSpace(tx.HandoffStagingTreeID) == "" {
		return fmt.Errorf("handoff staging digest is missing")
	}
	if err := validateAppBundleHandoffSourcePaths(tx); err != nil {
		return err
	}
	actual, err := repairPlanTreeContentStateID(tx.HandoffAppPath)
	if err != nil {
		return fmt.Errorf("read staged bundle digest: %w", err)
	}
	if actual != tx.HandoffAppTreeID {
		return fmt.Errorf("staged bundle changed after verification")
	}
	actual, err = repairPlanTreeContentStateID(tx.HandoffStagingPath)
	if err != nil {
		return fmt.Errorf("read staging directory digest: %w", err)
	}
	if actual != tx.HandoffStagingTreeID {
		return fmt.Errorf("staging directory changed after verification")
	}
	return nil
}

// CleanupAppBundleUpdateHandoffStaging removes only the complete staging tree
// recorded by the transaction. The root is first displaced to a unique sibling,
// so a concurrent recreation at the public staging path survives.
func CleanupAppBundleUpdateHandoffStaging(tx *UpdateTransaction) error {
	if tx == nil || strings.TrimSpace(tx.HandoffStagingTreeID) == "" {
		return fmt.Errorf("cleanup update staging: transaction identity is incomplete")
	}
	if err := validateAppBundleHandoffMetadata(tx); err != nil {
		return fmt.Errorf("cleanup update staging: %w", err)
	}
	relApp, err := filepath.Rel(tx.HandoffStagingPath, tx.HandoffAppPath)
	if err != nil || relApp == "." || relApp == ".." || strings.HasPrefix(relApp, ".."+string(filepath.Separator)) {
		return fmt.Errorf("cleanup update staging: app path is invalid")
	}
	return removeUpdateNodeMatching(tx.HandoffStagingPath, func(moved string) error {
		actual, err := repairPlanTreeContentStateID(moved)
		if err != nil {
			return err
		}
		if actual != tx.HandoffStagingTreeID {
			return fmt.Errorf("staging directory changed before cleanup")
		}
		return verifyAppBundleUpdateHandoffReplacement(
			tx,
			filepath.Join(moved, relApp),
			"staged",
		)
	}, true)
}

// CleanupAppBundleUpdateReplacement removes a displaced replacement only when
// its complete tree still matches the transaction's verified source.
func CleanupAppBundleUpdateReplacement(tx *UpdateTransaction, path string) error {
	return removeUpdateNodeMatching(path, func(moved string) error {
		return verifyAppBundleUpdateHandoffReplacement(tx, moved, "replacement")
	}, true)
}

// VerifyAppBundleUpdateHandoffTarget proves that the bytes copied into the
// installed bundle are the same tree that was verified in staging.
func VerifyAppBundleUpdateHandoffTarget(tx *UpdateTransaction) error {
	if tx == nil {
		return fmt.Errorf("handoff target transaction is invalid")
	}
	return verifyAppBundleUpdateHandoffReplacement(tx, tx.TargetPath, "installed")
}

// VerifyAppBundleUpdateHandoffReplacement proves that a candidate replacement
// tree matches the bundle captured during prepare. The macOS handoff uses this
// before atomically publishing a sibling staging bundle at the install path.
func VerifyAppBundleUpdateHandoffReplacement(tx *UpdateTransaction, path string) error {
	return verifyAppBundleUpdateHandoffReplacement(tx, path, "replacement")
}

func verifyAppBundleUpdateHandoffReplacement(tx *UpdateTransaction, path, subject string) error {
	if tx == nil || tx.TargetKind != "app-bundle" {
		return fmt.Errorf("handoff target transaction is invalid")
	}
	if strings.TrimSpace(tx.HandoffAppTreeID) == "" {
		return fmt.Errorf("handoff target digest is missing")
	}
	actual, err := repairPlanTreeContentStateID(path)
	if err != nil {
		return fmt.Errorf("read %s bundle digest: %w", subject, err)
	}
	if actual != tx.HandoffAppTreeID {
		return fmt.Errorf("%s bundle differs from verified staging", subject)
	}
	return nil
}

// VerifyAppBundleUpdateHandoffOriginal checks that the installed bundle about
// to become the rollback backup is still the tree captured during prepare.
func VerifyAppBundleUpdateHandoffOriginal(tx *UpdateTransaction) error {
	if tx == nil || tx.TargetKind != "app-bundle" {
		return fmt.Errorf("handoff original transaction is invalid")
	}
	return verifyAppBundleUpdateTree(tx.TargetPath, tx.BackupTreeID, "installed bundle changed after prepare")
}

// VerifyAppBundleUpdateHandoffBackup checks the node produced by the
// target-to-backup rename before the replacement bundle is copied into place.
func VerifyAppBundleUpdateHandoffBackup(tx *UpdateTransaction) error {
	if tx == nil || tx.TargetKind != "app-bundle" {
		return fmt.Errorf("handoff backup transaction is invalid")
	}
	return verifyAppBundleUpdateTree(tx.BackupPath, tx.BackupTreeID, "rollback backup differs from prepared bundle")
}

func verifyAppBundleUpdateHandoffBackupAbsent(tx *UpdateTransaction) error {
	if tx == nil || tx.TargetKind != "app-bundle" || strings.TrimSpace(tx.BackupPath) == "" {
		return fmt.Errorf("handoff backup transaction is invalid")
	}
	if _, err := os.Lstat(tx.BackupPath); err == nil {
		return fmt.Errorf("handoff backup path already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect handoff backup path: %w", err)
	}
	return nil
}

// quarantineExistingAppBundleUpdateBackup recovers the pre-v1.20 state where
// a committed macOS update removed pending-update.json but its sibling rollback
// bundle survived best-effort cleanup. Without the transaction there is no
// trustworthy authority to delete or reuse that bundle, so preparation moves it
// aside with a no-replace rename and preserves it for diagnosis.
//
// The caller holds both the pending-update lock and the target mutation locks.
// The current executable binding prevents a crafted caller from quarantining a
// similarly named bundle beside an unrelated application.
func quarantineExistingAppBundleUpdateBackup(tx *UpdateTransaction) (string, string, error) {
	if tx == nil || tx.TargetKind != "app-bundle" ||
		tx.BackupPath != tx.TargetPath+".reasonix-update-backup" {
		return "", "", fmt.Errorf("handoff backup transaction is invalid")
	}
	info, err := os.Lstat(tx.BackupPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("inspect existing handoff backup: %w", err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("existing handoff backup is not a directory")
	}

	launcher, err := repairExecutable()
	if err != nil {
		return "", "", fmt.Errorf("resolve current Reasonix executable: %w", err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(tx.TargetPath)
	if err != nil {
		return "", "", fmt.Errorf("resolve current app bundle: %w", err)
	}
	resolvedLauncher, err := filepath.EvalSymlinks(launcher)
	if err != nil {
		return "", "", fmt.Errorf("resolve current Reasonix executable: %w", err)
	}
	rel, err := filepath.Rel(resolvedTarget, resolvedLauncher)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("existing handoff backup is outside the current Reasonix installation")
	}

	expectedTreeID, err := repairPlanTreeContentStateID(tx.BackupPath)
	if err != nil {
		return "", "", fmt.Errorf("read existing handoff backup digest: %w", err)
	}
	for attempt := range 16 {
		quarantine := fmt.Sprintf(
			"%s.reasonix-orphaned-%d-%d",
			tx.BackupPath,
			time.Now().UTC().UnixNano(),
			attempt,
		)
		if err := renameRepairNodeNoReplace(tx.BackupPath, quarantine); err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", "", fmt.Errorf("quarantine existing handoff backup: %w", err)
		}
		updateBackupAfterQuarantine(tx.BackupPath, quarantine)

		restore := func(cause error) error {
			if _, statErr := os.Lstat(tx.BackupPath); statErr == nil {
				return fmt.Errorf("%w; preserved quarantined backup at %s because the public path was recreated", cause, quarantine)
			} else if !os.IsNotExist(statErr) {
				return fmt.Errorf("%w; inspect recreated handoff backup: %w", cause, statErr)
			}
			if restoreErr := renameRepairNodeNoReplace(quarantine, tx.BackupPath); restoreErr != nil {
				return fmt.Errorf("%w; preserved quarantined backup at %s: %w", cause, quarantine, restoreErr)
			}
			return cause
		}

		actualTreeID, digestErr := repairPlanTreeContentStateID(quarantine)
		if digestErr != nil {
			return "", "", restore(fmt.Errorf("read quarantined handoff backup digest: %w", digestErr))
		}
		if actualTreeID != expectedTreeID {
			return "", "", restore(fmt.Errorf("existing handoff backup changed during quarantine"))
		}
		if _, statErr := os.Lstat(tx.BackupPath); statErr == nil {
			return "", "", fmt.Errorf("handoff backup path was recreated during recovery; preserved quarantined backup at %s", quarantine)
		} else if !os.IsNotExist(statErr) {
			return "", "", fmt.Errorf("inspect recovered handoff backup path: %w", statErr)
		}
		return quarantine, actualTreeID, nil
	}
	return "", "", fmt.Errorf("cannot allocate handoff backup quarantine path")
}

// cleanupOrphanedAppBundleUpdateBackup retires only the quarantine recorded by
// a terminal transaction. The no-replace move and digest check keep a changed
// or concurrently replaced directory intact for diagnosis instead of deleting
// a path merely because its name resembles a Reasonix quarantine.
func cleanupOrphanedAppBundleUpdateBackup(tx *UpdateTransaction) {
	if validateOrphanedAppBundleBackupMetadata(tx) != nil ||
		strings.TrimSpace(tx.OrphanedBackupPath) == "" {
		return
	}
	if err := removeUpdateBackupTreeMatching(tx.OrphanedBackupPath, tx.OrphanedBackupTreeID); err != nil {
		slog.Warn("repair: preserving quarantined app backup after cleanup failed",
			"path", tx.OrphanedBackupPath, "error", err)
	}
}

func verifyAppBundleUpdateTree(path, expected, mismatch string) error {
	if strings.TrimSpace(expected) == "" {
		return fmt.Errorf("original bundle digest is missing")
	}
	actual, err := repairPlanTreeContentStateID(path)
	if err != nil {
		return fmt.Errorf("read original bundle digest: %w", err)
	}
	if actual != expected {
		return fmt.Errorf("%s", mismatch)
	}
	return nil
}

// AppBundleTreeDigest exposes the deterministic bundle-content digest to the
// desktop handoff tests and other platform glue without exposing path identity.
func AppBundleTreeDigest(path string) (string, error) {
	return repairPlanTreeContentStateID(path)
}

func validateAppBundleHandoffSourcePaths(tx *UpdateTransaction) error {
	staging, err := filepath.EvalSymlinks(tx.HandoffStagingPath)
	if err != nil {
		return fmt.Errorf("resolve handoff staging directory: %w", err)
	}
	app, err := filepath.EvalSymlinks(tx.HandoffAppPath)
	if err != nil {
		return fmt.Errorf("resolve handoff app bundle: %w", err)
	}
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		return fmt.Errorf("resolve temporary directory: %w", err)
	}
	within := func(root, path string) bool {
		rel, relErr := filepath.Rel(root, path)
		return relErr == nil && rel != "." && rel != ".." &&
			!strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if !within(tempRoot, staging) {
		return fmt.Errorf("handoff staging directory resolves outside the system temporary directory")
	}
	if !within(staging, app) {
		return fmt.Errorf("handoff app bundle resolves outside its staging directory")
	}
	if info, statErr := os.Stat(app); statErr != nil || !info.IsDir() {
		if statErr != nil {
			return fmt.Errorf("handoff app bundle is unavailable: %w", statErr)
		}
		return fmt.Errorf("handoff app bundle is not a directory")
	}
	return nil
}

// ClearClaimedAppBundleUpdateHandoff removes a failed handoff transaction.
// The caller must still hold the claim returned above.
func ClearClaimedAppBundleUpdateHandoff(claimed *UpdateTransaction) error {
	current, err := ReadPendingUpdate()
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(claimed, current) {
		return fmt.Errorf("clear update handoff: pending transaction changed")
	}
	if err := VerifyAppBundleUpdateHandoffOriginal(current); err != nil {
		return fmt.Errorf("clear update handoff: %w", err)
	}
	if err := verifyAppBundleUpdateHandoffBackupAbsent(current); err != nil {
		return fmt.Errorf("clear update handoff: %w", err)
	}
	if err := removePendingUpdateExactVerified(current, func() error {
		if err := VerifyAppBundleUpdateHandoffOriginal(current); err != nil {
			return fmt.Errorf("clear update handoff: %w", err)
		}
		if err := verifyAppBundleUpdateHandoffBackupAbsent(current); err != nil {
			return fmt.Errorf("clear update handoff: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}
