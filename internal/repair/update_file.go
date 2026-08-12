package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ClaimPendingFileUpdate binds an updater's actual replacement window to the
// exact transaction and release-unit paths prepared by the desktop. The
// launcher path is explicit because the Windows helper runs from a cache
// directory rather than from the installation it is authorized to replace.
func ClaimPendingFileUpdate(
	expectedToVersion, expectedCreatedAt, launcherPath string,
	expectedTargetPaths []string,
	timeout time.Duration,
) (*UpdateTransaction, func(), error) {
	tx, err := readPendingUpdateForLauncher(launcherPath)
	if err != nil {
		return nil, nil, fmt.Errorf("claim file update: read pending transaction: %w", err)
	}
	if tx.TargetKind != "file" ||
		strings.TrimSpace(tx.ToVersion) != strings.TrimSpace(expectedToVersion) ||
		strings.TrimSpace(tx.CreatedAt) != strings.TrimSpace(expectedCreatedAt) {
		return nil, nil, fmt.Errorf("claim file update: pending transaction does not match")
	}
	return claimPendingFileUpdate(
		expectedToVersion,
		expectedCreatedAt,
		UpdateTransactionID(tx),
		launcherPath,
		expectedTargetPaths,
		timeout,
	)
}

// ClaimPendingFileUpdateExact additionally binds the updater to every field in
// the transaction prepared by the desktop process.
func ClaimPendingFileUpdateExact(
	expectedToVersion, expectedCreatedAt, expectedTransactionID, launcherPath string,
	expectedTargetPaths []string,
	timeout time.Duration,
) (*UpdateTransaction, func(), error) {
	expectedTransactionID = strings.TrimSpace(expectedTransactionID)
	if expectedTransactionID == "" {
		return nil, nil, fmt.Errorf("claim file update: transaction identity is incomplete")
	}
	return claimPendingFileUpdate(
		expectedToVersion,
		expectedCreatedAt,
		expectedTransactionID,
		launcherPath,
		expectedTargetPaths,
		timeout,
	)
}

func claimPendingFileUpdate(
	expectedToVersion, expectedCreatedAt, expectedTransactionID, launcherPath string,
	expectedTargetPaths []string,
	timeout time.Duration,
) (*UpdateTransaction, func(), error) {
	expectedToVersion = strings.TrimSpace(expectedToVersion)
	expectedCreatedAt = strings.TrimSpace(expectedCreatedAt)
	launcherPath = filepath.Clean(strings.TrimSpace(launcherPath))
	if expectedToVersion == "" || expectedCreatedAt == "" || launcherPath == "" || launcherPath == "." {
		return nil, nil, fmt.Errorf("claim file update: transaction identity is incomplete")
	}
	if len(expectedTargetPaths) == 0 {
		return nil, nil, fmt.Errorf("claim file update: release unit is empty")
	}

	unlockPending, err := acquirePendingUpdateLock()
	if err != nil {
		return nil, nil, fmt.Errorf("claim file update: lock pending transaction: %w", err)
	}
	fail := func(err error) (*UpdateTransaction, func(), error) {
		unlockPending()
		return nil, nil, err
	}
	tx, err := readPendingUpdateForLauncher(launcherPath)
	if err != nil {
		return fail(fmt.Errorf("claim file update: read pending transaction: %w", err))
	}
	if tx.TargetKind != "file" ||
		strings.TrimSpace(tx.ToVersion) != expectedToVersion ||
		strings.TrimSpace(tx.CreatedAt) != expectedCreatedAt {
		return fail(fmt.Errorf("claim file update: pending transaction does not match"))
	}
	if expectedTransactionID != "" && UpdateTransactionID(tx) != expectedTransactionID {
		return fail(fmt.Errorf("claim file update: pending transaction changed"))
	}
	if tx.Platform != runtime.GOOS+"/"+runtime.GOARCH {
		return fail(fmt.Errorf("claim file update: pending transaction platform does not match"))
	}
	targetPaths := pendingUpdateTargetPaths(tx)
	if !sameRepairMutationPaths(targetPaths, expectedTargetPaths) {
		return fail(fmt.Errorf("claim file update: release unit does not match"))
	}

	unlockTargets, err := lockRepairMutationsTimeout(timeout, targetPaths...)
	if err != nil {
		return fail(fmt.Errorf("claim file update: lock targets: %w", err))
	}
	current, err := readPendingUpdateForLauncher(launcherPath)
	if err != nil {
		unlockTargets()
		return fail(fmt.Errorf("claim file update: re-read pending transaction: %w", err))
	}
	if !reflect.DeepEqual(tx, current) {
		unlockTargets()
		return fail(fmt.Errorf("claim file update: pending transaction changed while waiting"))
	}
	if err := verifyPreparedFileUpdateTargets(current); err != nil {
		unlockTargets()
		return fail(fmt.Errorf("claim file update: %w", err))
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

// verifyPreparedFileUpdateTargets proves that the release unit still matches
// the exact files snapshotted by PrepareFileUpdate. Path and transaction
// identity alone are insufficient: another installer can replace the binaries
// between prepare and claim while leaving pending-update.json untouched.
func verifyPreparedFileUpdateTargets(tx *UpdateTransaction) error {
	if err := verifyPreparedFileUpdateBackups(tx); err != nil {
		return err
	}
	for _, f := range pendingUpdateFiles(tx) {
		info, err := os.Lstat(f.TargetPath)
		if f.MissingBefore {
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect prepared release file %s: %w", filepath.Base(f.TargetPath), err)
			}
			return fmt.Errorf("prepared release file %s appeared after backup", filepath.Base(f.TargetPath))
		}
		if err != nil {
			return fmt.Errorf("inspect prepared release file %s: %w", filepath.Base(f.TargetPath), err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("prepared release file %s changed type", filepath.Base(f.TargetPath))
		}
		got, err := hashFile(f.TargetPath)
		if err != nil {
			return fmt.Errorf("hash prepared release file %s: %w", filepath.Base(f.TargetPath), err)
		}
		if !strings.EqualFold(got, f.SHA256) {
			return fmt.Errorf("prepared release file %s changed after backup", filepath.Base(f.TargetPath))
		}
	}
	return nil
}

func verifyPreparedFileUpdateBackups(tx *UpdateTransaction) error {
	for _, f := range pendingUpdateFiles(tx) {
		if f.MissingBefore {
			continue
		}
		backupInfo, err := os.Lstat(f.BackupPath)
		if err != nil {
			return fmt.Errorf("inspect prepared backup for %s: %w", filepath.Base(f.TargetPath), err)
		}
		if !backupInfo.Mode().IsRegular() {
			return fmt.Errorf("prepared backup for %s changed type", filepath.Base(f.TargetPath))
		}
		backupHash, err := hashFile(f.BackupPath)
		if err != nil {
			return fmt.Errorf("hash prepared backup for %s: %w", filepath.Base(f.TargetPath), err)
		}
		if !strings.EqualFold(backupHash, f.SHA256) {
			return fmt.Errorf("prepared backup for %s changed after backup", filepath.Base(f.TargetPath))
		}
	}
	return nil
}

// PublishClaimedFileUpdateMember replaces one release-unit member without ever
// overwriting an unverified node. The platform updater must hold the claim
// returned by ClaimPendingFileUpdateExact for the whole release-unit operation.
// A concurrent recreation after the prepared node moves aside wins; the new
// bytes and the verified prior node remain staged for recovery.
func PublishClaimedFileUpdateMember(claimed *UpdateTransaction, targetPath string, content []byte, mode os.FileMode) error {
	_, err := PublishClaimedFileUpdateMemberExact(claimed, targetPath, content, mode)
	return err
}

// PublishClaimedFileUpdateMemberExact returns proof of the exact node it
// published. Callers must retain every receipt and pass them to
// RecordClaimedFileUpdateInstalled before releasing the update claim.
func PublishClaimedFileUpdateMemberExact(
	claimed *UpdateTransaction,
	targetPath string,
	content []byte,
	mode os.FileMode,
) (FileUpdateInstallReceipt, error) {
	if claimed == nil || claimed.TargetKind != "file" {
		return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: transaction identity is incomplete")
	}
	current, err := readPendingUpdateForLauncher(claimed.TargetPath)
	if err != nil {
		return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: read pending transaction: %w", err)
	}
	if !reflect.DeepEqual(claimed, current) {
		return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: pending transaction changed")
	}
	targetPath = filepath.Clean(strings.TrimSpace(targetPath))
	targetKey := canonicalRepairPath(targetPath)
	var member *UpdateTransactionFile
	for i := range current.Files {
		if canonicalRepairPath(current.Files[i].TargetPath) == targetKey {
			member = &current.Files[i]
			break
		}
	}
	if targetKey == "" || member == nil {
		return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: target is outside the claimed release unit")
	}
	preparedState := ""
	if !member.MissingBefore {
		if err := verifyUpdateFileMatchesPrepared(*member); err != nil {
			return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: %w", err)
		}
		if err := verifyUpdateBackupFile(*member); err != nil {
			return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: %w", err)
		}
		preparedState = repairPlanReleaseNodeState(member.TargetPath)
	} else if _, err := os.Lstat(member.TargetPath); err == nil {
		return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: prepared release file %s appeared after backup", filepath.Base(member.TargetPath))
	} else if !os.IsNotExist(err) {
		return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: inspect prepared release file %s: %w", filepath.Base(member.TargetPath), err)
	}

	stage, expectedHash, installedStateID, err := stageFileUpdateContent(member.TargetPath, content, mode)
	if err != nil {
		return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: stage %s: %w", filepath.Base(member.TargetPath), err)
	}
	stagePublished := false
	defer func() {
		if !stagePublished {
			_ = removeUpdateBackupFileMatching(stage, expectedHash)
		}
	}()

	retained := ""
	if !member.MissingBefore {
		transactionID := UpdateTransactionID(claimed)
		if len(transactionID) < 16 {
			return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: transaction identity is incomplete")
		}
		retained = member.TargetPath + ".reasonix-update-aside-" + transactionID[:16]
		if err := renameRepairNodeNoReplace(member.TargetPath, retained); err != nil {
			return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: retain %s: %w", filepath.Base(member.TargetPath), err)
		}
		fileUpdateAfterRetain(member.TargetPath, retained)
		if err := verifyRepairPlanReleaseNodeStateFor(retained, member.TargetPath, preparedState); err != nil {
			if restoreErr := restoreRepairNodeIfAbsent(retained, member.TargetPath); restoreErr != nil {
				return FileUpdateInstallReceipt{}, fmt.Errorf("%w; verified prior release file retained at %s: %w", err, retained, restoreErr)
			}
			return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: prepared release file changed during retain: %w", err)
		}
	}
	if err := renameRepairNodeNoReplace(stage, member.TargetPath); err != nil {
		if retained != "" {
			if restoreErr := restoreRepairNodeIfAbsent(retained, member.TargetPath); restoreErr != nil {
				return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: publish %s: %w; verified prior release file retained at %s: %w", filepath.Base(member.TargetPath), err, retained, restoreErr)
			}
		}
		return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: publish %s: %w", filepath.Base(member.TargetPath), err)
	}
	stagePublished = true
	if err := verifyRepairPlanReleaseNodeStateFor(member.TargetPath, member.TargetPath, installedStateID); err != nil {
		rejected, retainErr := moveRepairNodeToUniqueCleanup(member.TargetPath)
		if retainErr != nil || rejected == "" {
			if retainErr != nil {
				return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: installed %s changed: %w; retain rejected file: %w", filepath.Base(member.TargetPath), err, retainErr)
			}
			return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: installed %s changed: %w; rejected file disappeared before compensation", filepath.Base(member.TargetPath), err)
		}
		if retained == "" {
			return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: installed %s changed: %w; rejected file retained at %s", filepath.Base(member.TargetPath), err, rejected)
		}
		if verifyErr := verifyRepairPlanReleaseNodeStateFor(retained, member.TargetPath, preparedState); verifyErr != nil {
			return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: installed %s changed: %w; rejected file retained at %s; prepared file changed at %s: %w", filepath.Base(member.TargetPath), err, rejected, retained, verifyErr)
		}
		if restoreErr := restoreRepairNodeIfAbsent(retained, member.TargetPath); restoreErr != nil {
			return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: installed %s changed: %w; rejected file retained at %s; restore prepared file: %w", filepath.Base(member.TargetPath), err, rejected, restoreErr)
		}
		return FileUpdateInstallReceipt{}, fmt.Errorf("publish file update: installed %s changed: %w; rejected file retained at %s and prepared file restored", filepath.Base(member.TargetPath), err, rejected)
	}
	if retained != "" {
		_ = removeUpdateNodeMatching(retained, func(moved string) error {
			return verifyRepairPlanReleaseNodeStateFor(moved, member.TargetPath, preparedState)
		}, false)
	}
	return FileUpdateInstallReceipt{
		UpdateTransactionID: UpdateTransactionID(current),
		TargetPath:          member.TargetPath,
		InstalledStateID:    installedStateID,
	}, nil
}

func verifyUpdateFileMatchesPrepared(f UpdateTransactionFile) error {
	info, err := os.Lstat(f.TargetPath)
	if err != nil {
		return fmt.Errorf("inspect prepared release file %s: %w", filepath.Base(f.TargetPath), err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("prepared release file %s changed type", filepath.Base(f.TargetPath))
	}
	if err := verifyRegularFileHash(f.TargetPath, f.SHA256); err != nil {
		return fmt.Errorf("prepared release file %s changed after backup: %w", filepath.Base(f.TargetPath), err)
	}
	return nil
}

func verifyUpdateBackupFile(f UpdateTransactionFile) error {
	info, err := os.Lstat(f.BackupPath)
	if err != nil {
		return fmt.Errorf("inspect prepared backup for %s: %w", filepath.Base(f.TargetPath), err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("prepared backup for %s changed type", filepath.Base(f.TargetPath))
	}
	if err := verifyRegularFileHash(f.BackupPath, f.SHA256); err != nil {
		return fmt.Errorf("prepared backup for %s changed after backup: %w", filepath.Base(f.TargetPath), err)
	}
	return nil
}

func verifyRegularFileHash(path, expected string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	actual, err := hashFile(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("hash mismatch")
	}
	return nil
}

func stageFileUpdateContent(targetPath string, content []byte, mode os.FileMode) (string, string, string, error) {
	tmp, err := os.CreateTemp(filepath.Dir(targetPath), "."+filepath.Base(targetPath)+".reasonix-update-stage-*")
	if err != nil {
		return "", "", "", err
	}
	path := tmp.Name()
	cleanup := func(err error) (string, string, string, error) {
		_ = tmp.Close()
		_ = os.Remove(path)
		return "", "", "", err
	}
	if _, err := tmp.Write(content); err != nil {
		return cleanup(err)
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return cleanup(err)
	}
	info, err := tmp.Stat()
	if err != nil {
		return cleanup(err)
	}
	installedStateID := repairPlanReadStateIDFor(
		targetPath,
		info.Mode(),
		"file",
		"",
		content,
		true,
	)
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return "", "", "", err
	}
	sum := sha256.Sum256(content)
	return path, hex.EncodeToString(sum[:]), installedStateID, nil
}
