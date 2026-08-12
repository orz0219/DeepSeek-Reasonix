package repair

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"reasonix/internal/config"
)

// PrepareFileUpdate snapshots the current desktop executable — plus any sibling
// binaries of the release unit the installer also replaces (Guard, launcher,
// update helper) — and records an update transaction before an updater applies
// the replacement. Sibling paths that do not exist are recorded explicitly so
// rollback can remove files introduced by the replacement release.
func PrepareFileUpdate(fromVersion, toVersion, targetPath string, siblingPaths ...string) (*UpdateTransaction, error) {
	targetPath = filepath.Clean(strings.TrimSpace(targetPath))
	if targetPath == "" || targetPath == "." {
		return nil, fmt.Errorf("prepare update: empty target path")
	}
	root := config.MemoryUserDir()
	if root == "" {
		return nil, fmt.Errorf("prepare update: Reasonix state directory is unavailable")
	}
	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, fmt.Errorf("prepare update: lock pending transaction: %w", err)
	}
	defer unlock()
	if err := ensureNoPendingUpdate(); err != nil {
		return nil, err
	}

	lockPaths := append([]string{targetPath}, siblingPaths...)
	unlockTargets, lockErr := lockRepairMutations(lockPaths...)
	if lockErr != nil {
		return nil, fmt.Errorf("prepare update: lock targets: %w", lockErr)
	}
	defer unlockTargets()
	backupDir := filepath.Join(root, "repair", "updates")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return nil, err
	}
	if !pathInsideResolvedRoot(filepath.Join(root, "repair"), backupDir) {
		return nil, fmt.Errorf("prepare update: backup directory resolves outside the repair directory")
	}
	tx := &UpdateTransaction{
		SchemaVersion: updateTransactionVersion,
		FromVersion:   fromVersion,
		ToVersion:     toVersion,
		Platform:      runtime.GOOS + "/" + runtime.GOARCH,
		TargetKind:    "file",
		TargetPath:    targetPath,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	seen := map[string]bool{}
	for i, path := range append([]string{targetPath}, siblingPaths...) {
		path = filepath.Clean(strings.TrimSpace(path))
		key := canonicalRepairPath(path)
		if path == "" || path == "." || key == "" || seen[key] {
			continue
		}
		seen[key] = true
		info, statErr := os.Lstat(path)
		if statErr != nil {
			if i > 0 && os.IsNotExist(statErr) {
				tx.Files = append(tx.Files, UpdateTransactionFile{TargetPath: path, MissingBefore: true})
				continue
			}
			return nil, fmt.Errorf("prepare update backup: %w", statErr)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("prepare update backup: release file %s is not a regular file", filepath.Base(path))
		}
		backupIdentity := repairPlanStateID(struct {
			CreatedAt  string `json:"createdAt"`
			TargetPath string `json:"targetPath"`
			Index      int    `json:"index"`
		}{
			CreatedAt:  tx.CreatedAt,
			TargetPath: canonicalRepairPath(path),
			Index:      i,
		})
		backupPath := filepath.Join(
			backupDir,
			fmt.Sprintf("%s.%s.previous", filepath.Base(path), backupIdentity[:16]),
		)
		hash, err := copyFileWithHashCreate(path, backupPath, 0o700)
		if err != nil {
			return nil, fmt.Errorf("prepare update backup: %w", err)
		}
		tx.Files = append(tx.Files, UpdateTransactionFile{TargetPath: path, BackupPath: backupPath, SHA256: hash})
		if i == 0 {
			tx.BackupPath = backupPath
			tx.BackupSHA256 = hash
		}
	}
	if err := verifyPreparedFileUpdateTargets(tx); err != nil {
		return nil, fmt.Errorf("prepare update: %w", err)
	}
	if err := ensureNoPendingUpdate(); err != nil {
		return nil, err
	}
	if err := createPendingUpdate(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

// PrepareAppBundleUpdate records the sibling bundle backup that the macOS
// handoff script creates. The script performs the directory move after exit.
func PrepareAppBundleUpdate(fromVersion, toVersion, appPath, backupPath string) (*UpdateTransaction, error) {
	tx, err := newAppBundleUpdateTransaction(fromVersion, toVersion, appPath, backupPath)
	if err != nil {
		return nil, err
	}
	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, fmt.Errorf("prepare update: lock pending transaction: %w", err)
	}
	defer unlock()
	if err := ensureNoPendingUpdate(); err != nil {
		return nil, err
	}
	unlockTargets, lockErr := lockRepairMutations(tx.TargetPath, tx.BackupPath)
	if lockErr != nil {
		return nil, fmt.Errorf("prepare update: lock targets: %w", lockErr)
	}
	defer unlockTargets()
	tx.BackupTreeID, err = repairPlanTreeContentStateID(tx.TargetPath)
	if err != nil {
		return nil, fmt.Errorf("prepare update: current bundle digest: %w", err)
	}
	if err := ensureNoPendingUpdate(); err != nil {
		return nil, err
	}
	if err := createPendingUpdate(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

// PrepareAppBundleUpdateHandoff records every path the detached macOS updater
// may mutate. The child receives only the transaction identity and must claim
// these recorded paths under the pending-update and mutation locks.
func PrepareAppBundleUpdateHandoff(fromVersion, toVersion, appPath, backupPath, stagedAppPath, stagingPath string, ownerPID int) (*UpdateTransaction, error) {
	tx, err := newAppBundleUpdateTransaction(fromVersion, toVersion, appPath, backupPath)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(tx.TargetPath) {
		return nil, fmt.Errorf("prepare update: invalid macOS bundle paths")
	}
	tx.HandoffAppPath = filepath.Clean(strings.TrimSpace(stagedAppPath))
	tx.HandoffStagingPath = filepath.Clean(strings.TrimSpace(stagingPath))
	tx.HandoffOwnerPID = ownerPID
	if err := validateAppBundleHandoffMetadata(tx); err != nil {
		return nil, fmt.Errorf("prepare update: %w", err)
	}

	unlock, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, fmt.Errorf("prepare update: lock pending transaction: %w", err)
	}
	defer unlock()
	if err := ensureNoPendingUpdate(); err != nil {
		return nil, err
	}
	unlockTargets, err := lockRepairMutations(tx.TargetPath, tx.BackupPath)
	if err != nil {
		return nil, fmt.Errorf("prepare update: lock targets: %w", err)
	}
	defer unlockTargets()
	tx.HandoffAppTreeID, err = repairPlanTreeContentStateID(tx.HandoffAppPath)
	if err != nil {
		return nil, fmt.Errorf("prepare update: stage bundle digest: %w", err)
	}
	tx.HandoffStagingTreeID, err = repairPlanTreeContentStateID(tx.HandoffStagingPath)
	if err != nil {
		return nil, fmt.Errorf("prepare update: staging directory digest: %w", err)
	}
	tx.BackupTreeID, err = repairPlanTreeContentStateID(tx.TargetPath)
	if err != nil {
		return nil, fmt.Errorf("prepare update: current bundle digest: %w", err)
	}
	if err := VerifyAppBundleUpdateHandoffSource(tx); err != nil {
		return nil, fmt.Errorf("prepare update: %w", err)
	}
	if err := VerifyAppBundleUpdateHandoffOriginal(tx); err != nil {
		return nil, fmt.Errorf("prepare update: %w", err)
	}
	orphanedBackup, orphanedTreeID, err := quarantineExistingAppBundleUpdateBackup(tx)
	if err != nil {
		return nil, fmt.Errorf("prepare update: recover existing handoff backup: %w", err)
	}
	tx.OrphanedBackupPath = orphanedBackup
	tx.OrphanedBackupTreeID = orphanedTreeID

	if err := verifyAppBundleUpdateHandoffBackupAbsent(tx); err != nil {
		return nil, fmt.Errorf("prepare update: %w", err)
	}
	if err := ensureNoPendingUpdate(); err != nil {
		return nil, err
	}
	if err := createPendingUpdate(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

func newAppBundleUpdateTransaction(fromVersion, toVersion, appPath, backupPath string) (*UpdateTransaction, error) {
	tx := &UpdateTransaction{
		SchemaVersion: updateTransactionVersion,
		FromVersion:   fromVersion,
		ToVersion:     toVersion,
		Platform:      runtime.GOOS + "/" + runtime.GOARCH,
		TargetKind:    "app-bundle",
		TargetPath:    filepath.Clean(strings.TrimSpace(appPath)),
		BackupPath:    filepath.Clean(strings.TrimSpace(backupPath)),
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	if !strings.HasSuffix(strings.ToLower(tx.TargetPath), ".app") ||
		tx.BackupPath != tx.TargetPath+".reasonix-update-backup" {
		return nil, fmt.Errorf("prepare update: invalid macOS bundle paths")
	}
	return tx, nil
}

// ClaimPendingAppBundleUpdateHandoff authorizes a detached child to perform the
// recorded bundle swap. It returns with both the pending transaction lock and
// the target mutation locks held; release must be called on every path.
func ClaimPendingAppBundleUpdateHandoff(expectedToVersion, expectedCreatedAt string, timeout time.Duration) (*UpdateTransaction, func(), error) {
	tx, err := ReadPendingUpdate()
	if err != nil {
		return nil, nil, fmt.Errorf("claim update handoff: read pending transaction: %w", err)
	}
	if tx.TargetKind != "app-bundle" ||
		strings.TrimSpace(tx.ToVersion) != strings.TrimSpace(expectedToVersion) ||
		strings.TrimSpace(tx.CreatedAt) != strings.TrimSpace(expectedCreatedAt) {
		return nil, nil, fmt.Errorf("claim update handoff: pending transaction does not match")
	}
	return claimPendingAppBundleUpdateHandoff(
		expectedToVersion,
		expectedCreatedAt,
		UpdateTransactionID(tx),
		timeout,
	)
}

// ClaimPendingAppBundleUpdateHandoffExact additionally binds the detached
// updater to the complete transaction prepared by the parent process.
func ClaimPendingAppBundleUpdateHandoffExact(
	expectedToVersion, expectedCreatedAt, expectedTransactionID string,
	timeout time.Duration,
) (*UpdateTransaction, func(), error) {
	expectedTransactionID = strings.TrimSpace(expectedTransactionID)
	if expectedTransactionID == "" {
		return nil, nil, fmt.Errorf("claim update handoff: transaction identity is incomplete")
	}
	return claimPendingAppBundleUpdateHandoff(
		expectedToVersion,
		expectedCreatedAt,
		expectedTransactionID,
		timeout,
	)
}

func claimPendingAppBundleUpdateHandoff(
	expectedToVersion, expectedCreatedAt, expectedTransactionID string,
	timeout time.Duration,
) (*UpdateTransaction, func(), error) {
	expectedToVersion = strings.TrimSpace(expectedToVersion)
	expectedCreatedAt = strings.TrimSpace(expectedCreatedAt)
	if expectedToVersion == "" || expectedCreatedAt == "" {
		return nil, nil, fmt.Errorf("claim update handoff: transaction identity is incomplete")
	}
	unlockPending, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, nil, fmt.Errorf("claim update handoff: lock pending transaction: %w", err)
	}
	fail := func(err error) (*UpdateTransaction, func(), error) {
		unlockPending()
		return nil, nil, err
	}

	tx, err := ReadPendingUpdate()
	if err != nil {
		return fail(fmt.Errorf("claim update handoff: read pending transaction: %w", err))
	}
	if tx.TargetKind != "app-bundle" ||
		strings.TrimSpace(tx.ToVersion) != expectedToVersion ||
		strings.TrimSpace(tx.CreatedAt) != expectedCreatedAt {
		return fail(fmt.Errorf("claim update handoff: pending transaction does not match"))
	}
	if expectedTransactionID != "" && UpdateTransactionID(tx) != expectedTransactionID {
		return fail(fmt.Errorf("claim update handoff: pending transaction changed"))
	}
	if tx.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		return fail(fmt.Errorf("claim update handoff: pending transaction platform does not match"))
	}
	if err := validateAppBundleHandoffMetadata(tx); err != nil {
		return fail(fmt.Errorf("claim update handoff: %w", err))
	}
	if strings.TrimSpace(tx.HandoffAppPath) == "" ||
		strings.TrimSpace(tx.HandoffStagingPath) == "" ||
		tx.HandoffOwnerPID <= 0 {
		return fail(fmt.Errorf("claim update handoff: handoff metadata is missing"))
	}

	unlockTargets, err := lockRepairMutationsTimeout(timeout, pendingUpdateTargetPaths(tx)...)
	if err != nil {
		return fail(fmt.Errorf("claim update handoff: lock targets: %w", err))
	}
	current, err := ReadPendingUpdate()
	if err != nil {
		unlockTargets()
		return fail(fmt.Errorf("claim update handoff: re-read pending transaction: %w", err))
	}
	if !reflect.DeepEqual(tx, current) {
		unlockTargets()
		return fail(fmt.Errorf("claim update handoff: pending transaction changed while waiting"))
	}
	if err := verifyAppBundleUpdateHandoffBackupAbsent(current); err != nil {
		unlockTargets()
		return fail(fmt.Errorf("claim update handoff: %w", err))
	}
	if err := VerifyAppBundleUpdateHandoffSource(current); err != nil {
		unlockTargets()
		return fail(fmt.Errorf("claim update handoff: %w", err))
	}
	if err := VerifyAppBundleUpdateHandoffOriginal(current); err != nil {
		unlockTargets()
		return fail(fmt.Errorf("claim update handoff: %w", err))
	}

	var once sync.Once
	release := func() {
		once.Do(func() {
			unlockTargets()
			unlockPending()
		})
	}
	return current, release, nil
}
