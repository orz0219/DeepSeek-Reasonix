package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/fileutil"
	"reasonix/internal/store"
)

const (
	recoveryTrashDir             = ".trash"
	recoveryTrashMetaFile        = ".trash-meta.json"
	recoveryTrashOperationPrefix = "recovery-trash:"
	recoveryTrashPendingFile     = ".recovery-trash-pending.json"
	recoveryTrashStagingPrefix   = ".recovery-trash-staging-"
)

// reconcileRecoveryTrashPending completes an interrupted move left by the
// pre-staging recovery-trash protocol. Keep this compatibility path so users
// upgrading from an intermediate build do not strand its typed marker.
func reconcileRecoveryTrashPending(item CleanupPendingInfo) (bool, error) {
	operation := strings.TrimSpace(item.Meta.Operation)
	if !strings.HasPrefix(operation, recoveryTrashOperationPrefix) {
		return false, nil
	}
	itemName := strings.TrimPrefix(operation, recoveryTrashOperationPrefix)
	if itemName == "" || filepath.Base(itemName) != itemName || itemName == "." || itemName == ".." {
		return true, fmt.Errorf("invalid recovery trash target")
	}
	path := filepath.Clean(item.SessionPath)
	dir := filepath.Dir(path)
	key := filepath.Base(path)
	guard, err := TryAcquireSessionRemovalGuard(path)
	if err != nil {
		return true, err
	}
	defer guard.Release()
	return true, finishRecoveryTrashMove(dir, path, key, filepath.Join(dir, recoveryTrashDir, itemName), guard)
}

// reconcileRecoveryTrashStages completes the atomic staging protocol used by
// new runtimes. A staging directory is intentionally not a valid Desktop trash
// item: it has neither a session-shaped directory name nor .trash-meta.json.
// Once complete, the whole directory is renamed into place atomically.
func reconcileRecoveryTrashStages(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	root := filepath.Join(dir, recoveryTrashDir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		itemDir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(itemDir, recoveryTrashPendingFile)); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("inspect recovery trash stage %s: %w", itemDir, err))
			continue
		}
		if err := reconcileRecoveryTrashStage(dir, itemDir); err != nil {
			errs = append(errs, fmt.Errorf("reconcile recovery trash stage %s: %w", itemDir, err))
		}
	}
	return errors.Join(errs...)
}

func reconcileRecoveryTrashStage(dir, itemDir string) error {
	pending, err := readRecoveryTrashPending(itemDir)
	if err != nil {
		return err
	}
	key := strings.TrimSpace(pending.Key)
	if !validRecoveryTrashKey(key) {
		return fmt.Errorf("invalid recovery trash key")
	}
	stagedPath := filepath.Join(itemDir, key)
	staged, err := regularRecoveryTrashPath(stagedPath)
	if err != nil {
		return err
	}

	if !strings.HasPrefix(filepath.Base(itemDir), recoveryTrashStagingPrefix) {
		if !staged {
			return fmt.Errorf("published recovery trash item is missing transcript")
		}
		if _, err := os.Stat(filepath.Join(itemDir, recoveryTrashMetaFile)); err == nil {
			return clearRecoveryTrashPending(itemDir)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := writeRecoveryTrashMetaExisting(itemDir, key); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		return clearRecoveryTrashPending(itemDir)
	}

	livePath := filepath.Join(dir, key)
	live, err := regularRecoveryTrashPath(livePath)
	if err != nil {
		return err
	}
	switch {
	case live && !staged:

		return removeEmptyRecoveryTrashStage(itemDir)
	case live && staged:
		return fmt.Errorf("live and staged recovery transcripts both exist")
	case !live && !staged:
		return fmt.Errorf("recovery trash stage is missing transcript")
	}

	guard, err := TryAcquireSessionRemovalGuard(livePath)
	if err != nil {
		return err
	}
	defer guard.Release()
	return finishRecoveryTrashStage(dir, livePath, key, itemDir, guard)
}

func regularRecoveryTrashPath(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("recovery trash path is not a regular file: %s", path)
	}
	return true, nil
}

func removeEmptyRecoveryTrashStage(itemDir string) error {
	entries, err := os.ReadDir(itemDir)
	if err != nil {
		return err
	}
	if len(entries) != 1 || entries[0].Name() != recoveryTrashPendingFile || entries[0].IsDir() {
		return fmt.Errorf("recovery trash stage contains artifacts without a transcript")
	}
	return os.RemoveAll(itemDir)
}

func reserveRecoveryTrashStage(dir string) (string, error) {
	root := filepath.Join(dir, recoveryTrashDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(root, recoveryTrashStagingPrefix)
}

func prepareRecoveryTrashStage(path, key, stageDir string) error {
	if err := writeRecoveryTrashPending(stageDir, key); err != nil {
		return err
	}
	return moveRecoveryTrashPath(path, filepath.Join(stageDir, key))
}

func finishRecoveryTrashStage(dir, path, key, stageDir string, guard *SessionRemovalGuard) error {
	if err := moveRecoveryTrashArtifacts(dir, path, stageDir); err != nil {
		return err
	}
	itemDir, err := publishRecoveryTrashStage(dir, key, stageDir)
	if err != nil {
		return err
	}
	if err := writeRecoveryTrashMetaExisting(itemDir, key); err != nil {
		if !os.IsNotExist(err) {
			return err
		}

		return guard.RemoveSidecarsAndRelease()
	}
	if err := clearRecoveryTrashPending(itemDir); err != nil {
		return err
	}
	return guard.RemoveSidecarsAndRelease()
}

func publishRecoveryTrashStage(dir, key, stageDir string) (string, error) {
	root := filepath.Join(dir, recoveryTrashDir)
	stem := strings.TrimSuffix(key, filepath.Ext(key))
	stamp := time.Now().UTC().UnixMilli()
	for i := range 1000 {
		name := key
		if i > 0 {
			name = fmt.Sprintf("%s-recovery-%d-%d", stem, stamp, i)
		}
		itemDir := filepath.Join(root, name)
		if _, err := os.Lstat(itemDir); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.Rename(stageDir, itemDir); err == nil {
			return itemDir, nil
		} else if _, statErr := os.Lstat(itemDir); statErr == nil {
			continue
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		} else {
			return "", err
		}
	}
	return "", fmt.Errorf("could not publish recovery trash target")
}

func writeRecoveryTrashPending(itemDir, key string) error {
	if !validRecoveryTrashKey(key) {
		return fmt.Errorf("invalid recovery trash key")
	}
	b, err := json.MarshalIndent(recoveryTrashPendingMeta{Key: key}, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFileStrict(filepath.Join(itemDir, recoveryTrashPendingFile), b, 0o644)
}

func readRecoveryTrashPending(itemDir string) (recoveryTrashPendingMeta, error) {
	b, err := os.ReadFile(filepath.Join(itemDir, recoveryTrashPendingFile))
	if err != nil {
		return recoveryTrashPendingMeta{}, err
	}
	var meta recoveryTrashPendingMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		return recoveryTrashPendingMeta{}, err
	}
	return meta, nil
}

func clearRecoveryTrashPending(itemDir string) error {
	err := os.Remove(filepath.Join(itemDir, recoveryTrashPendingFile))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func validRecoveryTrashKey(key string) bool {
	return key != "" && filepath.Base(key) == key && key != "." && key != ".." &&
		strings.HasSuffix(key, ".jsonl") && !strings.HasSuffix(key, ".events.jsonl")
}

func reserveRecoveryTrashItemDir(dir, key string) (string, string, error) {
	root := filepath.Join(dir, recoveryTrashDir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", "", err
	}
	stem := strings.TrimSuffix(key, filepath.Ext(key))
	for i := range 1000 {
		name := key
		if i > 0 {
			name = fmt.Sprintf("%s-recovery-%d-%d", stem, time.Now().UTC().UnixMilli(), i)
		}
		itemDir := filepath.Join(root, name)
		if err := os.Mkdir(itemDir, 0o755); err == nil {
			return name, itemDir, nil
		} else if !os.IsExist(err) {
			return "", "", err
		}
	}
	return "", "", fmt.Errorf("could not reserve recovery trash target")
}

func finishRecoveryTrashMove(dir, path, key, itemDir string, guard *SessionRemovalGuard) error {
	if err := os.MkdirAll(itemDir, 0o755); err != nil {
		return err
	}
	if err := moveRecoveryTrashArtifacts(dir, path, itemDir); err != nil {
		return err
	}
	if err := writeRecoveryTrashMeta(itemDir, key); err != nil {
		return err
	}

	if err := ClearCleanupPending(path); err != nil {
		return err
	}
	return guard.RemoveSidecarsAndRelease()
}

func moveRecoveryTrashArtifacts(dir, path, itemDir string) error {
	for _, src := range recoveryTrashSidecars(path) {
		if err := moveRecoveryTrashPath(src, filepath.Join(itemDir, filepath.Base(src))); err != nil {
			return err
		}
	}
	return moveRecoverySubagentArtifacts(dir, path, itemDir)
}

func prepareRecoveryTrashEntry(path, key, itemDir string) error {
	if err := writeRecoveryTrashMeta(itemDir, key); err != nil {
		return err
	}
	return moveRecoveryTrashPath(path, filepath.Join(itemDir, key))
}

func writeRecoveryTrashMeta(itemDir, key string) error {
	meta := recoveryTrashMeta{Key: key, DeletedAt: time.Now().UnixMilli()}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(itemDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(itemDir, recoveryTrashMetaFile), b, 0o644)
}

func writeRecoveryTrashMetaExisting(itemDir, key string) error {
	meta := recoveryTrashMeta{Key: key, DeletedAt: time.Now().UnixMilli()}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(itemDir, recoveryTrashMetaFile), b, 0o644)
}

func recoveryTrashSidecars(path string) []string {
	artifacts := append([]string(nil), store.SessionSidecarFiles(path)...)
	artifacts = append(artifacts,
		path+".telemetry.json",
		store.SessionCheckpointDir(path),
		store.SessionJobsDir(path),
		store.SessionInboxDir(path),
	)
	return artifacts
}

func moveRecoverySubagentArtifacts(dir, path, itemDir string) error {
	artifacts, err := ListSubagentsByParent(dir, BranchID(path))
	if err != nil {
		return err
	}
	targetDir := filepath.Join(itemDir, "subagents")
	for _, artifact := range artifacts {
		paths := []string{artifact.SessionPath, artifact.MetaPath}
		paths = append(paths, store.SessionSidecarFiles(artifact.SessionPath)...)
		for _, src := range paths {
			if err := moveRecoveryTrashPath(src, filepath.Join(targetDir, filepath.Base(src))); err != nil {
				return err
			}
		}
	}
	return nil
}

func moveRecoveryTrashPath(src, dst string) error {
	if strings.TrimSpace(src) == "" {
		return nil
	}
	if _, err := os.Lstat(src); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}
