package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/store"
)

type trashedSessionMeta struct {
	Key       string `json:"key"`
	DeletedAt int64  `json:"deletedAt"`
}

type sessionTrashArtifact struct {
	src  string
	name string
}

func sessionTelemetryPath(sessionPath string) string {
	if strings.TrimSpace(sessionPath) == "" {
		return ""
	}
	return sessionPath + ".telemetry.json"
}

func sessionTrashArtifacts(sessionPath, key string) []sessionTrashArtifact {
	stem := strings.TrimSuffix(key, ".jsonl")
	return []sessionTrashArtifact{
		{src: sessionPath, name: key},
		{src: store.SessionMeta(sessionPath), name: key + ".meta"},
		{src: store.SessionGoalState(sessionPath), name: stem + ".goal-state.json"},
		{src: store.SessionEventLog(sessionPath), name: stem + ".events.jsonl"},
		{src: store.SessionEventLogDamaged(sessionPath), name: stem + ".events.jsonl.damaged"},
		{src: store.SessionEventIndex(sessionPath), name: stem + ".event-index.json"},
		{src: store.SessionDisplayIndex(sessionPath), name: stem + ".display-index.json"},
		{src: store.SessionConflictLog(sessionPath), name: stem + ".conflicts.jsonl"},
		{src: store.SessionRecoveryState(sessionPath), name: stem + ".recovery.json"},
		{src: sessionTelemetryPath(sessionPath), name: key + ".telemetry.json"},
		{src: store.SessionCheckpointDir(sessionPath), name: stem + ".ckpt"},
		{src: store.SessionJobsDir(sessionPath), name: stem + ".jobs"},
		{src: store.SessionInboxDir(sessionPath), name: stem + ".inbox"},
	}
}

// errSessionBusyElsewhere is the sanitized error surfaced when a destructive
// session operation is blocked by a live owner. It intentionally carries no
// writer id, hostname, or path.
var errSessionBusyElsewhere = errors.New("session is in use by another Reasonix window or process")

// acquireSessionRemovalGuard wraps agent.TryAcquireSessionRemovalGuard with
// the sanitized busy error. The guard holds the session's save and lease
// locks across the destructive operation and deletes the lock files
// atomically with the release — a one-shot busy probe followed by RemoveAll
// would let another process acquire the lease in between and then lose its
// freshly locked lease file, breaking cross-process mutual exclusion.
func acquireSessionRemovalGuard(sessionPath string) (*agent.SessionRemovalGuard, error) {
	guard, err := agent.TryAcquireSessionRemovalGuard(sessionPath)
	if err != nil {
		if errors.Is(err, agent.ErrSessionLeaseHeld) {
			return nil, errSessionBusyElsewhere
		}
		return nil, err
	}
	return guard, nil
}

func sessionOwnedArtifactPaths(sessionPath string) []string {
	key := filepath.Base(sessionPath)
	artifacts := sessionTrashArtifacts(sessionPath, key)
	paths := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		if strings.TrimSpace(artifact.src) != "" {
			paths = append(paths, artifact.src)
		}
	}
	return paths
}

func trashSessionArtifacts(dir, sessionPath, key string) error {
	return trashSessionArtifactsBeforeMove(dir, sessionPath, key, nil)
}

func reconcileDesktopCleanupPending(dir string) error {
	return agent.ReconcileCleanupPending(dir, func(item agent.CleanupPendingInfo) error {
		if strings.TrimSpace(item.Meta.Operation) == "delete" {
			sessionPath, key, err := validateSessionPath(dir, item.SessionPath)
			if err != nil {
				return err
			}
			return reconcileDesktopTrashSessionArtifacts(dir, sessionPath, key)
		}
		return removeDesktopSessionArtifacts(item.SessionPath)
	})
}

func validateSessionTrashTarget(dir, sessionPath, key string) error {
	if _, err := os.Stat(sessionPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	itemDir := filepath.Join(sessionTrashPath(dir), key)
	if info, err := os.Stat(itemDir); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("session trash target is not a directory: %s", key)
		}
		trashPath := filepath.Join(itemDir, key)
		if trashInfo, err := os.Stat(trashPath); err == nil && !trashInfo.IsDir() {
			removable, err := liveSessionRemovableWithExistingTrash(sessionPath, trashPath)
			if err != nil {
				return err
			}
			if removable {
				return nil
			}
			if agent.SessionLeaseHeldByOtherRuntime(sessionPath) {
				return errSessionBusyElsewhere
			}
			return nil
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

type preparedSessionTrashTarget struct {
	shouldMove     bool
	itemDir        string
	allocateUnique bool
}

func prepareSessionTrashTarget(dir, sessionPath, key string) (preparedSessionTrashTarget, error) {
	if _, err := os.Stat(sessionPath); os.IsNotExist(err) {
		return preparedSessionTrashTarget{}, nil
	} else if err != nil {
		return preparedSessionTrashTarget{}, err
	}
	itemDir := filepath.Join(sessionTrashPath(dir), key)
	if info, err := os.Stat(itemDir); err == nil {
		if !info.IsDir() {
			return preparedSessionTrashTarget{}, fmt.Errorf("session trash target is not a directory: %s", key)
		}
		trashPath := filepath.Join(itemDir, key)
		if trashInfo, err := os.Stat(trashPath); err == nil && !trashInfo.IsDir() {
			removable, err := liveSessionRemovableWithExistingTrash(sessionPath, trashPath)
			if err != nil {
				return preparedSessionTrashTarget{}, err
			}
			if removable {
				return preparedSessionTrashTarget{}, removeDesktopSessionArtifacts(sessionPath)
			}
			if agent.SessionLeaseHeldByOtherRuntime(sessionPath) {
				return preparedSessionTrashTarget{}, errSessionBusyElsewhere
			}
			return preparedSessionTrashTarget{shouldMove: true, allocateUnique: true}, nil
		} else if err != nil && !os.IsNotExist(err) {
			return preparedSessionTrashTarget{}, err
		}
		if err := os.RemoveAll(itemDir); err != nil {
			return preparedSessionTrashTarget{}, err
		}
	} else if !os.IsNotExist(err) {
		return preparedSessionTrashTarget{}, err
	}
	return preparedSessionTrashTarget{shouldMove: true, itemDir: itemDir}, nil
}

func reserveUniqueSessionTrashItemDir(dir, key string) (string, error) {
	root := sessionTrashPath(dir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	stem := strings.TrimSuffix(key, ".jsonl")
	for i := range 100 {
		name := fmt.Sprintf("%s.jsonl-deleted-%d-%02d", stem, time.Now().UnixNano(), i)
		itemDir := filepath.Join(root, name)
		if err := os.Mkdir(itemDir, 0o755); err == nil {
			return itemDir, nil
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate unique trash target for session: %s", key)
}

// liveSessionRemovableWithExistingTrash reports whether a live session file may
// be removed even though a trash copy already exists under the same key: the
// live file must be discardable (empty stub) or byte-identical to the trash
// copy, and no other runtime may hold its session lease — another process could
// be mid-write, and removing the file would silently drop its next save.
func liveSessionRemovableWithExistingTrash(sessionPath, trashPath string) (bool, error) {
	discardable, err := liveSessionDiscardable(sessionPath)
	if err != nil {
		return false, err
	}
	duplicate := false
	if !discardable {
		duplicate, err = trashSessionMatchesLive(sessionPath, trashPath)
		if err != nil {
			return false, err
		}
	}
	if !discardable && !duplicate {
		return false, nil
	}
	return !agent.SessionLeaseHeldByOtherRuntime(sessionPath), nil
}

func liveSessionDiscardable(sessionPath string) (bool, error) {
	if agent.IsCleanupPending(sessionPath) {
		return true, nil
	}
	return liveSessionContentDiscardable(sessionPath)
}

func liveSessionContentDiscardable(sessionPath string) (bool, error) {
	info, err := os.Stat(sessionPath)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if info.IsDir() {
		return false, nil
	}
	if info.Size() == 0 {
		return true, nil
	}
	session, err := agent.LoadSession(sessionPath)
	if err != nil {
		return false, nil
	}
	return !session.HasContent(), nil
}

func trashSessionMatchesLive(sessionPath, trashPath string) (bool, error) {
	if _, err := os.Stat(sessionPath); err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}

	return agent.SessionsShareContent(sessionPath, trashPath)
}

func sessionFileHasConversationContent(sessionPath string) bool {
	if strings.TrimSpace(sessionPath) == "" || agent.IsCleanupPending(sessionPath) {
		return false
	}
	info, err := os.Stat(sessionPath)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return false
	}
	session, err := agent.LoadSession(sessionPath)
	if err != nil {
		return false
	}
	return session.HasContent()
}

func trashSessionArtifactsBeforeMove(dir, sessionPath, key string, beforeMove func()) error {
	if err := validateSessionTrashTarget(dir, sessionPath, key); err != nil {
		return err
	}
	target, err := prepareSessionTrashTarget(dir, sessionPath, key)
	if err != nil {
		return err
	}
	if !target.shouldMove {
		return agent.ClearCleanupPending(sessionPath)
	}

	guard, err := acquireSessionRemovalGuard(sessionPath)
	if err != nil {
		return err
	}
	defer guard.Release()
	if err := invalidateTopicDirMarkers(dir); err != nil {
		return err
	}
	itemDir := target.itemDir
	if target.allocateUnique {
		itemDir, err = reserveUniqueSessionTrashItemDir(dir, key)
		if err != nil {
			return err
		}
	} else if err := os.MkdirAll(itemDir, 0o755); err != nil {
		return err
	}
	if beforeMove != nil {
		beforeMove()
	}
	for _, artifact := range sessionTrashArtifacts(sessionPath, key) {
		if err := movePathIfExists(artifact.src, filepath.Join(itemDir, artifact.name)); err != nil {
			return err
		}
	}
	if err := trashSubagentArtifacts(dir, sessionPath, itemDir); err != nil {
		return err
	}
	if err := guard.RemoveSidecarsAndRelease(); err != nil {
		return err
	}
	meta := trashedSessionMeta{Key: key, DeletedAt: time.Now().UnixMilli()}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(itemDir, sessionTrashMetaFile), b, 0o644); err != nil {
		return err
	}
	if err := agent.ClearCleanupPending(sessionPath); err != nil {
		return err
	}
	return nil
}

func listTrashedSessionFiles(dir string) ([]string, error) {
	root := sessionTrashPath(dir)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, err
	}
	paths := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		itemDir := filepath.Join(root, e.Name())
		keys := []string{}
		if b, err := readFileUTF8(filepath.Join(itemDir, sessionTrashMetaFile)); err == nil {
			var meta trashedSessionMeta
			if json.Unmarshal(b, &meta) == nil && store.IsSessionTranscriptName(meta.Key) {
				keys = append(keys, meta.Key)
			}
		}
		if store.IsSessionTranscriptName(e.Name()) {
			keys = append(keys, e.Name())
		}
		for _, key := range keys {
			path := filepath.Join(itemDir, key)
			validPath, _, _, err := validateTrashedSessionPath(dir, path)
			if err != nil {
				continue
			}
			if info, err := os.Stat(validPath); err == nil && !info.IsDir() {
				paths = append(paths, validPath)
				break
			}
		}
	}
	return paths, nil
}

func trashedSessionDeletedAt(path string) int64 {
	b, err := readFileUTF8(filepath.Join(filepath.Dir(path), sessionTrashMetaFile))
	if err != nil {
		return 0
	}
	var meta trashedSessionMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		return 0
	}
	return meta.DeletedAt
}

func purgeTrashedSessionFile(dir, path string) error {
	_, key, itemDir, err := validateTrashedSessionPath(dir, path)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(itemDir); err != nil {
		return err
	}
	if err := updateSessionTitles(dir, func(m map[string]string) bool {
		if _, ok := m[key]; !ok {
			return false
		}
		delete(m, key)
		return true
	}); err != nil {
		return err
	}
	if err := removeSessionDisplayKey(dir, key); err != nil {
		return err
	}
	if err := removeSessionPlannerDisplay(dir, key); err != nil {
		return err
	}
	return nil
}

func movePathIfExists(src, dst string) error {
	if _, err := os.Lstat(src); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if sourcePathMissing(src) {
		return nil
	} else if !isRenameCrossDeviceOrBusy(err) {
		return err
	}

	return copyAndRemove(src, dst)
}

// isRenameCrossDeviceOrBusy reports whether err is a cross-device rename or
// a "file busy" error that a copy+remove fallback can recover from.
func isRenameCrossDeviceOrBusy(err error) bool {
	if err == nil {
		return false
	}

	le := &os.LinkError{}
	if errors.As(err, &le) {
		if errors.Is(le.Err, syscall.EXDEV) {
			return true
		}
		// Windows: "The process cannot access the file because it is being used by another process."
		var errno syscall.Errno
		if errors.As(le.Err, &errno) {
			return errno == 32
		}
	}
	return false
}

func sourcePathMissing(src string) bool {
	if strings.TrimSpace(src) == "" {
		return true
	}
	_, err := os.Lstat(src)
	return os.IsNotExist(err)
}

// copyPathFn is a seam for tests to simulate a source vanishing mid-copy.
var copyPathFn = copyPath

// copyAndRemove recursively copies src to dst, then removes src. Used as a
// fallback when os.Rename fails (cross-device or Windows file-lock races).
func copyAndRemove(src, dst string) error {
	if err := copyPathFn(src, dst); err != nil {
		if sourcePathMissing(src) {

			_ = os.RemoveAll(dst)
			return nil
		}
		return err
	}

	time.Sleep(10 * time.Millisecond)
	return os.RemoveAll(src)
}

func copyPath(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		return copySymlink(src, dst)
	case mode.IsDir():
		return copyDir(src, dst, mode.Perm())
	case mode.IsRegular():
		return copyFile(src, dst, mode.Perm())
	default:
		return fmt.Errorf("unsupported file type in rename fallback: %s", src)
	}
}

func copyDir(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(dst, mode); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			_ = os.RemoveAll(dst)
			return nil
		}
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if err := copyPath(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {

	in, err := os.Open(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		in.Close()
		return err
	}

	_, err = io.Copy(out, in)

	closeErr := out.Close()
	in.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

func copySymlink(src, dst string) error {
	target, err := os.Readlink(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.Symlink(target, dst)
}

func trashSubagentArtifacts(dir, sessionPath, itemDir string) error {
	artifacts, err := agent.ListSubagentsByParent(dir, agent.BranchID(sessionPath))
	if err != nil {
		return err
	}
	trashSubagentDir := filepath.Join(itemDir, "subagents")
	for _, artifact := range artifacts {
		paths := []string{artifact.SessionPath, artifact.MetaPath}
		paths = append(paths, store.SessionSidecarFiles(artifact.SessionPath)...)
		for _, src := range paths {
			if strings.TrimSpace(src) == "" {
				continue
			}
			if err := movePathIfExists(src, filepath.Join(trashSubagentDir, filepath.Base(src))); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkRestoreSubagentConflicts(dir, itemDir string) error {
	trashSubagentDir := filepath.Join(itemDir, "subagents")
	entries, err := os.ReadDir(trashSubagentDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		target := filepath.Join(dir, "subagents", entry.Name())
		if _, err := os.Stat(target); err == nil {
			return fmt.Errorf("subagent artifact already exists: %s", entry.Name())
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func restoreSubagentArtifacts(dir, itemDir string) error {
	trashSubagentDir := filepath.Join(itemDir, "subagents")
	entries, err := os.ReadDir(trashSubagentDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := movePathIfExists(filepath.Join(trashSubagentDir, entry.Name()), filepath.Join(dir, "subagents", entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
