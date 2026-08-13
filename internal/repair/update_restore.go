package repair

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/fileutil"
)

// restoreReleaseUnit swaps every backup into place with compensation, so a
// failed rollback never leaves a mixed old/new install. Phase 1 stages each
// backup next to its target — a copy can fail halfway (disk full, unreadable
// backup) and staging keeps the live binaries untouched until every byte is
// on the target filesystem. Phase 2 swaps via renames only: each target moves
// aside first (renaming works even for the running executable, where
// overwriting does not), so a failure renames the asides back and the unit
// stays coherent on the new version for a retried rollback. Only when that
// unwinding itself fails is the install reported as mixed.
func restoreReleaseUnit(
	files []UpdateTransactionFile,
	verifyBeforeSwap func() error,
	expectedStates map[string]string,
) (mixed bool, err error) {
	stages := make([]string, len(files))
	defer func() {
		for i, stage := range stages {
			if stage != "" {
				_ = removeUpdateBackupFileMatching(stage, files[i].SHA256)
			}
		}
	}()
	for i, f := range files {
		if f.MissingBefore {
			continue
		}
		mode := os.FileMode(0o700)
		if st, statErr := os.Stat(f.TargetPath); statErr == nil {
			mode = st.Mode().Perm()
		}
		stage, stagedSHA256, copyErr := stageUpdateRollbackBackup(f, mode)
		if copyErr != nil {
			return false, fmt.Errorf("stage %s: %w", filepath.Base(f.TargetPath), copyErr)
		}
		stages[i] = stage

		if !strings.EqualFold(stagedSHA256, f.SHA256) {
			return false, fmt.Errorf("stage %s: backup hash mismatch", filepath.Base(f.TargetPath))
		}
	}
	if verifyBeforeSwap != nil {
		if err := verifyBeforeSwap(); err != nil {
			return false, err
		}
	}

	alreadyRestored := make([]bool, len(files))
	preexistingAside := make([]bool, len(files))
	for i, f := range files {
		aside := f.TargetPath + ".reasonix-rollback-aside"
		asideInfo, err := os.Lstat(aside)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("inspect retained %s: %w", filepath.Base(f.TargetPath), err)
		}
		if !asideInfo.Mode().IsRegular() {
			return false, fmt.Errorf("ambiguous rollback state for %s", filepath.Base(f.TargetPath))
		}
		preexistingAside[i] = true
		if _, err := os.Lstat(f.TargetPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("inspect restored %s: %w", filepath.Base(f.TargetPath), err)
		}
		if f.MissingBefore {
			return false, fmt.Errorf("ambiguous rollback state for %s", filepath.Base(f.TargetPath))
		}
		got, err := hashFile(f.TargetPath)
		if err != nil || !strings.EqualFold(got, f.SHA256) {
			return false, fmt.Errorf("ambiguous rollback state for %s", filepath.Base(f.TargetPath))
		}
		alreadyRestored[i] = true
	}
	asides := make([]string, len(files))
	retainedStates := make([]string, len(files))
	processed := make([]bool, len(files))
	restoreAttempted := make([]bool, len(files))
	preserveAside := make([]bool, len(files))
	ownedRetained := make([]bool, len(files))
	publishedStates := make([]string, len(files))
	failedIndex := -1
	var swapErr error
	for i, f := range files {
		aside := f.TargetPath + ".reasonix-rollback-aside"
		if alreadyRestored[i] {
			asides[i] = aside
			processed[i] = true
			continue
		}
		retainedState := repairPlanReleaseNodeState(f.TargetPath)
		if renameErr := rollbackSwapRename(f.TargetPath, aside); renameErr != nil {
			if os.IsNotExist(renameErr) {

				if f.MissingBefore {
					aside = ""
				} else if _, statErr := os.Lstat(aside); statErr != nil {
					aside = ""
				}
			} else {
				failedIndex = i
				swapErr = fmt.Errorf("retain %s: %w", filepath.Base(f.TargetPath), renameErr)
				break
			}
		}
		asides[i] = aside
		if aside != "" && !preexistingAside[i] {
			if verifyErr := verifyRepairPlanReleaseNodeStateFor(aside, f.TargetPath, retainedState); verifyErr != nil {
				failedIndex = i
				swapErr = verifyErr
				if restoreErr := restoreRepairNodeIfAbsent(aside, f.TargetPath); restoreErr != nil {
					preserveAside[i] = true
					swapErr = fmt.Errorf("%w; preserve moved live target at %s: %w", verifyErr, aside, restoreErr)
				} else {
					asides[i] = ""
				}
				break
			}
			retainedStates[i] = retainedState
			if installedState := strings.TrimSpace(f.InstalledStateID); installedState != "" {
				if verifyErr := verifyRepairPlanReleaseNodeStateFor(aside, f.TargetPath, installedState); verifyErr != nil {
					failedIndex = i
					swapErr = fmt.Errorf("installed release file %s changed before rollback: %w", filepath.Base(f.TargetPath), verifyErr)
					if restoreErr := restoreRepairNodeIfAbsent(aside, f.TargetPath); restoreErr != nil {
						preserveAside[i] = true
						swapErr = fmt.Errorf("%w; preserve moved live target at %s: %w", swapErr, aside, restoreErr)
					} else {
						asides[i] = ""
					}
					break
				}
				ownedRetained[i] = true
			}
		}
		if expected := expectedStates[f.TargetPath]; expected != "" && aside != "" {
			if verifyErr := verifyRepairPlanStateIDFor(aside, f.TargetPath, expected); verifyErr != nil {
				failedIndex = i
				swapErr = verifyErr
				if restoreErr := restoreRepairNodeIfAbsent(aside, f.TargetPath); restoreErr != nil {
					preserveAside[i] = true
					swapErr = fmt.Errorf("%w; preserve moved live target at %s: %w", verifyErr, aside, restoreErr)
				} else {
					asides[i] = ""
				}
				break
			}
			ownedRetained[i] = true
		}
		if f.MissingBefore {

			processed[i] = true
			continue
		}
		restoreAttempted[i] = true

		if publishErr := rollbackPublishStage(stages[i], f.TargetPath); publishErr != nil {
			failedIndex = i
			swapErr = fmt.Errorf("restore %s: %w", filepath.Base(f.TargetPath), publishErr)
			break
		}
		stages[i] = ""
		publishedStates[i] = repairPlanReleaseNodeState(f.TargetPath)
		processed[i] = true
		publishedHash, hashErr := hashFile(f.TargetPath)
		if hashErr != nil || !strings.EqualFold(publishedHash, f.SHA256) {
			failedIndex = i
			if hashErr != nil {
				swapErr = fmt.Errorf("verify restored %s: %w", filepath.Base(f.TargetPath), hashErr)
			} else {
				swapErr = fmt.Errorf("verify restored %s: hash mismatch", filepath.Base(f.TargetPath))
			}
			break
		}
	}
	if swapErr == nil {
		for _, f := range files {
			info, verifyErr := os.Lstat(f.TargetPath)
			if f.MissingBefore {
				if os.IsNotExist(verifyErr) {
					continue
				}
				if verifyErr != nil {
					swapErr = fmt.Errorf("verify restored release unit %s: %w", filepath.Base(f.TargetPath), verifyErr)
				} else {
					swapErr = fmt.Errorf("verify restored release unit %s: unexpected file appeared", filepath.Base(f.TargetPath))
				}
				break
			}
			if verifyErr != nil {
				swapErr = fmt.Errorf("verify restored release unit %s: %w", filepath.Base(f.TargetPath), verifyErr)
				break
			}
			if !info.Mode().IsRegular() {
				swapErr = fmt.Errorf("verify restored release unit %s: file changed type", filepath.Base(f.TargetPath))
				break
			}
			got, hashErr := hashFile(f.TargetPath)
			if hashErr != nil || !strings.EqualFold(got, f.SHA256) {
				if hashErr != nil {
					swapErr = fmt.Errorf("verify restored release unit %s: %w", filepath.Base(f.TargetPath), hashErr)
				} else {
					swapErr = fmt.Errorf("verify restored release unit %s: hash mismatch", filepath.Base(f.TargetPath))
				}
				break
			}
		}
	}
	if swapErr == nil {
		for i, f := range files {

			aside := f.TargetPath + ".reasonix-rollback-aside"
			if !preexistingAside[i] && retainedStates[i] != "" && ownedRetained[i] {
				_ = removeUpdateNodeMatching(aside, func(moved string) error {
					return verifyRepairPlanReleaseNodeStateFor(moved, f.TargetPath, retainedStates[i])
				}, false)
			}
		}
		return false, nil
	}

	for j, f := range files {
		if !processed[j] && j != failedIndex {
			continue
		}
		if preserveAside[j] {
			mixed = true
			continue
		}
		if preexistingAside[j] {

			mixed = true
			continue
		}
		if asides[j] != "" {
			if retainedStates[j] != "" {
				if verifyErr := verifyRepairPlanReleaseNodeStateFor(asides[j], f.TargetPath, retainedStates[j]); verifyErr != nil {
					mixed = true
					continue
				}
			}
			if _, statErr := os.Lstat(f.TargetPath); statErr == nil {

				if f.MissingBefore || publishedStates[j] == "" {
					mixed = true
					continue
				}
				if removeErr := removeUpdateNodeMatching(f.TargetPath, func(moved string) error {
					return verifyRepairPlanReleaseNodeStateFor(moved, f.TargetPath, publishedStates[j])
				}, false); removeErr != nil {
					mixed = true
					continue
				}
			} else if !os.IsNotExist(statErr) {
				mixed = true
				continue
			}
			if restoreErr := restoreRepairNodeIfAbsent(asides[j], f.TargetPath); restoreErr != nil {
				mixed = true
			}
			continue
		}
		if !f.MissingBefore && restoreAttempted[j] {

			mixed = true
		}
	}
	return mixed, swapErr
}

func stageUpdateRollbackBackup(
	file UpdateTransactionFile,
	mode os.FileMode,
) (string, string, error) {
	for attempt := range 16 {
		stage := fmt.Sprintf(
			"%s.reasonix-rollback-stage-%d-%d",
			file.TargetPath,
			time.Now().UTC().UnixNano(),
			attempt,
		)
		stagedSHA256, err := rollbackStageCopy(file.BackupPath, stage, mode)
		if err == nil {
			return stage, stagedSHA256, nil
		}
		if os.IsExist(err) {
			continue
		}
		return "", "", err
	}
	return "", "", fmt.Errorf("cannot allocate rollback staging path")
}

// allowedUpdateTargetBase whitelists the packaged binaries an update
// transaction may name. The main executable names are only valid as the
// primary target; Guard/launcher artifacts only as release-unit siblings.
func allowedUpdateTargetBase(base string, primary bool) bool {
	switch strings.ToLower(base) {
	case "reasonix-desktop", "reasonix-desktop.exe":
		return primary
	case "reasonix.exe":
		return !primary
	case "reasonix", "reasonix-guard", "reasonix-guard.exe", "reasonix-launcher.exe", "reasonix-update-helper.exe", "reasonix-cli.exe":
		return !primary
	default:
		return false
	}
}

func validateUpdateTransaction(tx *UpdateTransaction) error {
	if tx == nil || tx.SchemaVersion != updateTransactionVersion || strings.TrimSpace(tx.ToVersion) == "" {
		return fmt.Errorf("pending update metadata is incomplete")
	}
	launcher, err := repairExecutable()
	if err != nil {
		return fmt.Errorf("pending update launcher path is unavailable")
	}
	return validateUpdateTransactionForLauncher(tx, launcher)
}

func validateUpdateTransactionForLauncher(tx *UpdateTransaction, launcher string) error {
	if tx == nil || tx.SchemaVersion != updateTransactionVersion || strings.TrimSpace(tx.ToVersion) == "" {
		return fmt.Errorf("pending update metadata is incomplete")
	}
	if strings.TrimSpace(tx.Platform) == "" || strings.TrimSpace(tx.CreatedAt) == "" {
		return fmt.Errorf("pending update transaction identity is incomplete")
	}
	if _, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(tx.CreatedAt)); err != nil {
		return fmt.Errorf("pending update creation identity is invalid")
	}
	tx.TargetPath = filepath.Clean(tx.TargetPath)
	tx.BackupPath = filepath.Clean(tx.BackupPath)
	launcher = filepath.Clean(strings.TrimSpace(launcher))
	if launcher == "" || launcher == "." {
		return fmt.Errorf("pending update launcher path is unavailable")
	}
	if resolved, resolveErr := filepath.EvalSymlinks(launcher); resolveErr == nil {
		launcher = resolved
	}
	launcher = filepath.Clean(launcher)
	switch tx.TargetKind {
	case "file":
		if !allowedUpdateTargetBase(filepath.Base(tx.TargetPath), true) {
			return fmt.Errorf("pending update target is not a Reasonix executable")
		}
		launcherKey := canonicalRepairPath(launcher)
		targetKey := canonicalRepairPath(tx.TargetPath)
		if launcherKey == "" || targetKey == "" || filepath.Dir(launcherKey) != filepath.Dir(targetKey) {
			return fmt.Errorf("%w: pending update target is outside the current Guard installation", errPendingUpdateForeignInstall)
		}
		root := filepath.Clean(filepath.Join(config.MemoryUserDir(), "repair"))
		insideRepairDir := func(path string) bool {
			return pathInsideResolvedRoot(root, path)
		}
		if !insideRepairDir(tx.BackupPath) {
			return fmt.Errorf("pending update backup is outside the repair directory")
		}

		if strings.TrimSpace(tx.BackupSHA256) == "" {
			return fmt.Errorf("pending update backup hash is missing")
		}
		primaryListed := len(tx.Files) == 0
		seenTargets := make(map[string]struct{}, len(tx.Files))
		seenBackups := make(map[string]struct{}, len(tx.Files))
		for i := range tx.Files {
			f := &tx.Files[i]
			f.TargetPath = filepath.Clean(f.TargetPath)
			targetIdentity := canonicalRepairPath(f.TargetPath)
			if targetIdentity == "" {
				return fmt.Errorf("pending update release file path is invalid")
			}
			if _, duplicate := seenTargets[targetIdentity]; duplicate {
				return fmt.Errorf("pending update lists a duplicate release file")
			}
			seenTargets[targetIdentity] = struct{}{}
			primary := f.TargetPath == tx.TargetPath
			primaryListed = primaryListed || primary
			if !allowedUpdateTargetBase(filepath.Base(f.TargetPath), primary) {
				return fmt.Errorf("pending update lists an unexpected release file")
			}
			if filepath.Dir(f.TargetPath) != filepath.Dir(tx.TargetPath) {
				return fmt.Errorf("%w: pending update release file is outside the current Guard installation", errPendingUpdateForeignInstall)
			}
			if f.MissingBefore {
				if primary || strings.TrimSpace(f.BackupPath) != "" || strings.TrimSpace(f.SHA256) != "" {
					return fmt.Errorf("pending update missing release file metadata is invalid")
				}
				continue
			}
			f.BackupPath = filepath.Clean(f.BackupPath)
			if !insideRepairDir(f.BackupPath) {
				return fmt.Errorf("pending update backup is outside the repair directory")
			}
			if strings.TrimSpace(f.SHA256) == "" {
				return fmt.Errorf("pending update release file hash is missing")
			}
			backupIdentity := canonicalRepairPath(f.BackupPath)
			if backupIdentity == "" {
				return fmt.Errorf("pending update backup path is invalid")
			}
			if _, duplicate := seenBackups[backupIdentity]; duplicate {
				return fmt.Errorf("pending update lists a duplicate release backup")
			}
			seenBackups[backupIdentity] = struct{}{}
			if primary &&
				(f.BackupPath != tx.BackupPath || !strings.EqualFold(f.SHA256, tx.BackupSHA256)) {
				return fmt.Errorf("pending update primary backup metadata is inconsistent")
			}
		}
		installedStates := 0
		for _, f := range tx.Files {
			stateID := strings.TrimSpace(f.InstalledStateID)
			if stateID == "" {
				continue
			}
			if len(stateID) != sha256.Size*2 {
				return fmt.Errorf("pending update installed release-unit state is invalid")
			}
			if _, err := hex.DecodeString(stateID); err != nil {
				return fmt.Errorf("pending update installed release-unit state is invalid")
			}
			installedStates++
		}
		if installedStates != 0 && installedStates != len(tx.Files) {
			return fmt.Errorf("pending update installed release-unit state is incomplete")
		}
		if !primaryListed {
			return fmt.Errorf("pending update release unit omits the primary executable")
		}
	case "app-bundle":
		if !strings.HasSuffix(strings.ToLower(tx.TargetPath), ".app") || tx.BackupPath != tx.TargetPath+".reasonix-update-backup" {
			return fmt.Errorf("pending update bundle paths are invalid")
		}
		inside := tx.TargetPath + string(filepath.Separator)
		if !strings.HasPrefix(launcher, inside) {
			return fmt.Errorf("%w: pending update bundle is not the current Guard installation", errPendingUpdateForeignInstall)
		}
		if err := validateAppBundleHandoffMetadata(tx); err != nil {
			return fmt.Errorf("pending update %w", err)
		}
		if err := validateOrphanedAppBundleBackupMetadata(tx); err != nil {
			return fmt.Errorf("pending update %w", err)
		}
	default:
		return fmt.Errorf("pending update target kind is invalid")
	}
	return nil
}

func pathInsideResolvedRoot(root, path string) bool {
	root = filepath.Clean(strings.TrimSpace(root))
	path = filepath.Clean(strings.TrimSpace(path))
	if root == "" || path == "" {
		return false
	}
	lexicalRel, err := filepath.Rel(root, path)
	if err != nil || lexicalRel == ".." || strings.HasPrefix(lexicalRel, ".."+string(filepath.Separator)) {
		return false
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	resolvedRel, err := filepath.Rel(resolvedRoot, resolvedPath)
	return err == nil && resolvedRel != ".." &&
		!strings.HasPrefix(resolvedRel, ".."+string(filepath.Separator))
}

func validateAppBundleHandoffMetadata(tx *UpdateTransaction) error {
	if tx == nil {
		return fmt.Errorf("handoff metadata is incomplete")
	}
	hasAny := strings.TrimSpace(tx.HandoffAppPath) != "" ||
		strings.TrimSpace(tx.HandoffStagingPath) != "" ||
		strings.TrimSpace(tx.HandoffAppTreeID) != "" ||
		strings.TrimSpace(tx.HandoffStagingTreeID) != "" ||
		tx.HandoffOwnerPID != 0
	if !hasAny {
		return nil
	}
	tx.HandoffAppPath = filepath.Clean(strings.TrimSpace(tx.HandoffAppPath))
	tx.HandoffStagingPath = filepath.Clean(strings.TrimSpace(tx.HandoffStagingPath))
	if tx.HandoffOwnerPID <= 0 ||
		!filepath.IsAbs(tx.HandoffAppPath) ||
		!filepath.IsAbs(tx.HandoffStagingPath) ||
		!strings.HasSuffix(strings.ToLower(tx.HandoffAppPath), ".app") {
		return fmt.Errorf("handoff metadata is incomplete")
	}
	if tx.HandoffAppPath == tx.TargetPath || tx.HandoffAppPath == tx.BackupPath {
		return fmt.Errorf("handoff app overlaps the installed bundle")
	}
	rel, err := filepath.Rel(tx.HandoffStagingPath, tx.HandoffAppPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("handoff app is outside its staging directory")
	}
	tempRoot := filepath.Clean(os.TempDir())
	stagingRel, err := filepath.Rel(tempRoot, tx.HandoffStagingPath)
	if err != nil || stagingRel == "." || stagingRel == ".." || strings.HasPrefix(stagingRel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("handoff staging directory is outside the system temporary directory")
	}
	stagingBase := strings.Split(stagingRel, string(filepath.Separator))[0]
	if !strings.HasPrefix(stagingBase, "reasonix-mac-update-") {
		return fmt.Errorf("handoff staging directory has an unexpected name")
	}
	return nil
}

func validateOrphanedAppBundleBackupMetadata(tx *UpdateTransaction) error {
	if tx == nil {
		return fmt.Errorf("orphaned backup metadata is incomplete")
	}
	path := strings.TrimSpace(tx.OrphanedBackupPath)
	treeID := strings.TrimSpace(tx.OrphanedBackupTreeID)
	if path == "" && treeID == "" {
		return nil
	}
	if tx.TargetKind != "app-bundle" || path == "" || treeID == "" {
		return fmt.Errorf("orphaned backup metadata is incomplete")
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || filepath.Dir(path) != filepath.Dir(tx.BackupPath) {
		return fmt.Errorf("orphaned backup path is outside the app installation directory")
	}
	prefix := filepath.Base(tx.BackupPath) + ".reasonix-orphaned-"
	suffix, ok := strings.CutPrefix(filepath.Base(path), prefix)
	if !ok {
		return fmt.Errorf("orphaned backup path has an unexpected name")
	}
	parts := strings.Split(suffix, "-")
	if len(parts) != 2 {
		return fmt.Errorf("orphaned backup path has an unexpected name")
	}
	for _, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return fmt.Errorf("orphaned backup path has an unexpected name")
		}
	}
	if len(treeID) != sha256.Size*2 {
		return fmt.Errorf("orphaned backup digest is invalid")
	}
	if _, err := hex.DecodeString(treeID); err != nil {
		return fmt.Errorf("orphaned backup digest is invalid")
	}
	tx.OrphanedBackupPath = path
	tx.OrphanedBackupTreeID = treeID
	return nil
}

func copyFileWithHashCreate(src, dst string, mode os.FileMode) (string, error) {
	return copyFileWithHashMode(src, dst, mode, true)
}

func copyFileWithHashMode(src, dst string, mode os.FileMode, createOnly bool) (string, error) {
	in, err := openRepairRegularRead(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source %s is not a regular file", filepath.Base(src))
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".repair-copy-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), in); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if createOnly {
		if err := renameRepairNodeNoReplace(tmpPath, dst); err != nil {
			return "", err
		}
	} else {
		if err := fileutil.ReplaceFile(tmpPath, dst); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
