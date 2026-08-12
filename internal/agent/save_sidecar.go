package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"reasonix/internal/filelock"
	"reasonix/internal/store"
)

func lockSessionSavePath(path string) func() {
	key := canonicalSessionSavePath(path)
	v, _ := sessionSaveLocks.LoadOrStore(key, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// lockSessionFile waits briefly for the cross-process compatibility save lock.
// A short overlap with a legitimate writer is allowed to settle, but an
// stalled or indefinitely held lock fails the save instead of freezing tab
// switching or application shutdown. The caller keeps its in-memory transcript
// and can retry through the existing autosave/recovery paths.
func lockSessionFile(path string) (func(), error) {
	wait := sessionFileLockWait
	poll := sessionFileLockPollInterval
	if poll <= 0 {
		poll = time.Millisecond
	}
	deadline := time.Now().Add(wait)
	for {
		unlock, err := tryLockSessionFile(path)
		if err == nil {
			return unlock, nil
		}
		if !errors.Is(err, ErrSessionFileLockHeld) {
			return nil, err
		}
		remaining := time.Until(deadline)
		if wait <= 0 || remaining <= 0 {
			return nil, ErrSessionFileLockHeld
		}
		if poll > remaining {
			poll = remaining
		}
		time.Sleep(poll)
	}
}

// LockSessionMetaPath serializes a complete branch-meta read-modify-write
// cycle with both goroutines in this process and other Reasonix processes.
// Callers must hold it from the first read through the final replace.
func LockSessionMetaPath(path string) (func(), error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("empty session path")
	}
	canonical := canonicalSessionSavePath(path)
	if err := os.MkdirAll(filepath.Dir(canonical), 0o755); err != nil {
		return nil, fmt.Errorf("create session metadata dir: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionMetaLockWait)
	releaseFile, err := filelock.Acquire(ctx, sessionMetaLockPath(canonical))
	cancel()
	if err != nil {
		return nil, fmt.Errorf("lock session metadata: %w", err)
	}
	return func() {
		releaseFile()
	}, nil
}

func sessionMetaLockPath(path string) string {
	canonical := canonicalSessionSavePath(path)
	digest := sha256.Sum256([]byte(canonical))
	return filepath.Join(filepath.Dir(canonical), "."+hex.EncodeToString(digest[:8])+".meta.lock")
}

// UpdateBranchMeta is the owner-level metadata API. The callback runs while
// the cross-process metadata lock is held, so callers cannot accidentally
// load a stale sidecar and overwrite fields written by another runtime.
func UpdateBranchMeta(path string, touchUpdated bool, update func(*BranchMeta) error) error {
	unlock, err := LockSessionMetaPath(path)
	if err != nil {
		return err
	}
	defer unlock()
	m, err := ensureBranchMetaUnlocked(path)
	if err != nil {
		return err
	}
	if update != nil {
		if err := update(&m); err != nil {
			return err
		}
	}
	return saveBranchMeta(path, m, touchUpdated)
}

func canonicalSessionSavePath(path string) string {
	key := filepath.Clean(strings.TrimSpace(path))
	if abs, err := filepath.Abs(key); err == nil {
		key = abs
	}

	key = resolvePathThroughExistingAncestor(key)
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(strings.ToUpper(key), `\\?\UNC\`) {
			key = `\\` + key[len(`\\?\UNC\`):]
		} else {
			key = strings.TrimPrefix(key, `\\?\`)
		}
		key = strings.ToLower(key)
	}
	return key
}

// resolvePathThroughExistingAncestor resolves the deepest existing ancestor
// and appends every still-missing component. Fresh sessions can be nested under
// directories that have not been created yet; resolving only the immediate
// parent leaves aliases above that directory split into different lease keys.
func resolvePathThroughExistingAncestor(path string) string {
	current := filepath.Clean(path)
	missing := make([]string, 0, 4)
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			for _, v := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, v)
			}
			return resolved
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// CanonicalSessionPath is the identity key of a session path: cleaned,
// absolute, and case-folded on Windows, matching the key form used by the
// lease registry and the save-path locks. Any runtime bookkeeping that
// compares or maps session paths (desktop tabs, detached runtimes) must use
// this exact form, or the same file splits into distinct keys — e.g.
// `C:\Users\...` vs the lease's lowercased `c:\users\...`. Empty input stays
// empty instead of resolving to the working directory.
func CanonicalSessionPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	return canonicalSessionSavePath(path)
}

// LoadSession reads a saved session into a fresh Session value. New sessions
// replay the append-only event log; legacy sessions without an event log fall
// back to the compatibility .jsonl checkpoint. A damaged log is replayed to its
// last clean record (or the checkpoint when nothing decodes) and flagged so the
// next save heals it with a rewrite-and-compact.
// In-process loads share the save path mutex so they cannot observe a local
// SaveSnapshot between appending an event-log record and refreshing the index.
// Missing files surface as os.IsNotExist so callers can fall through to a
// new session.
func LoadSession(path string) (*Session, error) {
	unlock := lockSessionSavePath(path)
	defer unlock()
	return loadSessionUnlocked(path)
}

func loadSessionUnlocked(path string) (*Session, error) {
	msgs, _, damaged, err := loadSessionMessages(path)
	if err != nil {
		return nil, err
	}
	s := &Session{Messages: msgs, eventLogDamaged: damaged}

	normalized := NormalizeSession(s.Messages)
	normalized = migrateLegacyProviderContent(normalized)
	if len(normalized) != len(s.Messages) || (len(s.Messages) > 0 && &normalized[0] != &s.Messages[0]) {
		s.normalizedDirty = true

		s.rawMessages = msgs
	}
	s.Messages = normalized
	if digest, err := digestSessionMessages(s.Messages); err == nil {
		if meta, ok, metaErr := loadBranchMetaRetry(path); metaErr != nil {

			s.markPersistedRevisionUnknown(path, digest, s.version, s.rewriteVersion)
		} else {
			revision := int64(0)
			if ok {
				revision = meta.Revision
			}
			s.markPersistedFromLoad(path, digest, s.version, revision, s.rewriteVersion)
		}
	}
	return s, nil
}

func removeStaleSessionLockSidecar(basePath, sidecarPath string) error {
	basePath = canonicalSessionSavePath(basePath)
	if sessionLeaseHeldLocally(basePath) || SessionLeaseHeldByOtherRuntime(basePath) {
		return nil
	}
	lock, err := tryTakeSessionLockFile(sidecarPath)
	if err != nil {
		if errors.Is(err, ErrSessionFileLockHeld) {
			return nil
		}
		return err
	}

	return lock.RemoveAndUnlock()
}

// removeStaleSessionLeaseLockSidecar retires a leftover .lease.lock. The file
// is the lease lock itself, so taking it non-blocking proves no runtime holds
// the lease, and RemoveAndUnlock deletes it atomically with the release.
func removeStaleSessionLeaseLockSidecar(basePath, sidecarPath string) error {
	basePath = canonicalSessionSavePath(basePath)
	if sessionLeaseHeldLocally(basePath) {
		return nil
	}
	lock, err := tryTakeSessionLockFile(sidecarPath)
	if err != nil {
		if errors.Is(err, ErrSessionFileLockHeld) {
			return nil
		}
		return err
	}
	return lock.RemoveAndUnlock()
}

// removeStaleSessionLeaseInfoSidecar retires a leftover .lease.json while
// holding the lease lock, so no runtime can adopt the info file mid-removal.
// The info file itself is never held open by anyone, so a plain remove under
// the lock is safe on every platform.
func removeStaleSessionLeaseInfoSidecar(basePath, sidecarPath string) error {
	basePath = canonicalSessionSavePath(basePath)
	if sessionLeaseHeldLocally(basePath) {
		return nil
	}
	lockPath := basePath + ".lease.lock"
	if _, err := os.Stat(lockPath); err == nil {
		unlock, err := tryLockSessionLeaseFile(basePath)
		if err != nil {
			if errors.Is(err, ErrSessionLeaseHeld) {
				return nil
			}
			return err
		}
		removeErr := os.Remove(sidecarPath)
		if unlock != nil {
			unlock()
		}
		if removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.Remove(sidecarPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func sessionLeaseHeldLocally(path string) bool {
	_, ok := sessionLeaseOwners.Load(canonicalSessionSavePath(path))
	return ok
}

// sessionLockSidecarFits reports whether basePath's .lock sidecar name stays
// within the filesystem's per-component limit; past it, no process can hold
// (or ever have held) the file lock, because the lock file cannot be created.
func sessionLockSidecarFits(basePath string) bool {
	return len(filepath.Base(basePath))+len(".lock") <= nameMaxBytes
}

// sessionLeaseSidecarFits is the lease-file analogue of sessionLockSidecarFits.
func sessionLeaseSidecarFits(basePath string) bool {
	return len(filepath.Base(basePath))+len(".lease.lock") <= nameMaxBytes
}

// reconcileOverlongSessionFilenames renames transcripts whose basenames grew
// past maxSessionBasenameBytes — the leftover shape of the pre-bounded
// recovery cascade (#5923), where lock and lease sidecars could no longer be
// created and the session became unsaveable. The conversation bytes are kept
// verbatim under a bounded name derived the same way new recovery branches
// are named; branch meta moves along with its ID rewritten, and sessions
// pointing at the old ID are re-parented so lineage survives the rename.
func reconcileOverlongSessionFilenames(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	renamed := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !store.IsSessionTranscriptName(name) {
			continue
		}
		if len(name) <= maxSessionBasenameBytes {
			continue
		}
		oldPath := filepath.Join(dir, name)
		if IsCleanupPending(oldPath) {

			continue
		}
		newID, err := renameOverlongSession(oldPath)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", oldPath, err))
		}

		if newID != "" {
			renamed[BranchID(oldPath)] = newID
		}
	}
	if len(renamed) > 0 {
		if err := reparentSessionBranches(dir, renamed); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// renameOverlongSession moves one overlong transcript to its bounded name and
// migrates the sidecars that carry user state. It returns the new branch ID,
// or "" when the session was skipped because a runtime may still own it.
func renameOverlongSession(oldPath string) (string, error) {
	oldID := BranchID(oldPath)
	newID := recoveryParentStem(oldID)
	if newID == oldID {
		return "", nil
	}
	newPath := filepath.Join(filepath.Dir(oldPath), newID+".jsonl")
	if _, err := os.Stat(newPath); err == nil {
		return "", fmt.Errorf("rename target %s already exists", filepath.Base(newPath))
	} else if !os.IsNotExist(err) {
		return "", err
	}
	unlockOld := lockSessionSavePath(oldPath)
	defer unlockOld()
	unlockNew := lockSessionSavePath(newPath)
	defer unlockNew()
	if sessionLeaseHeldLocally(oldPath) {
		return "", nil
	}

	if sessionLeaseSidecarFits(oldPath) && SessionLeaseHeldByOtherRuntime(oldPath) {
		return "", nil
	}
	var lockFile *sessionLockFile
	if sessionLockSidecarFits(oldPath) {
		lock, err := tryTakeSessionLockFile(oldPath + ".lock")
		if err != nil {
			if errors.Is(err, ErrSessionFileLockHeld) {
				return "", nil
			}
			return "", err
		}
		lockFile = lock
	}
	if err := os.Rename(oldPath, newPath); err != nil {

		if lockFile != nil {
			lockFile.Unlock()
		}
		return "", err
	}
	// The transcript is committed under its new name from here on. Sidecar
	// migration and lock cleanup failures are reported, but the new ID is
	// still returned so the caller re-parents children: the old name is gone,
	// and a later run would have no way to reconstruct this mapping.
	var errs []error
	if err := migrateSessionSidecars(oldPath, newPath, newID); err != nil {
		errs = append(errs, err)
	}

	if sessionLeaseSidecarFits(oldPath) {
		for _, stale := range []string{oldPath + ".lease.lock", oldPath + ".lease.json"} {
			if err := os.Remove(stale); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}

	if lockFile != nil {
		if err := lockFile.RemoveAndUnlock(); err != nil {
			errs = append(errs, err)
		}
	}
	return newID, errors.Join(errs...)
}

// migrateSessionSidecars moves the user-state sidecars of a renamed session:
// branch meta (with its ID rewritten to match the new filename), goal state,
// and the checkpoint/job directories. Lock and lease files are disposable and
// are removed by the caller instead.
func migrateSessionSidecars(oldPath, newPath, newID string) error {
	var errs []error
	if len(filepath.Base(oldPath))+len(".meta") <= nameMaxBytes {
		if meta, ok, err := LoadBranchMeta(oldPath); err != nil {
			errs = append(errs, err)
		} else if ok {
			meta.ID = newID
			if err := SaveBranchMetaPreserveUpdated(newPath, meta); err != nil {
				errs = append(errs, err)
			} else if err := os.Remove(BranchMetaPath(oldPath)); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	for _, pair := range [][2]string{
		{store.SessionGoalState(oldPath), store.SessionGoalState(newPath)},
		{store.SessionEventLog(oldPath), store.SessionEventLog(newPath)},
		{store.SessionEventLogDamaged(oldPath), store.SessionEventLogDamaged(newPath)},
		{store.SessionEventIndex(oldPath), store.SessionEventIndex(newPath)},
		{store.SessionConflictLog(oldPath), store.SessionConflictLog(newPath)},
		{store.SessionRecoveryState(oldPath), store.SessionRecoveryState(newPath)},
		{store.SessionCheckpointDir(oldPath), store.SessionCheckpointDir(newPath)},
		{store.SessionJobsDir(oldPath), store.SessionJobsDir(newPath)},
		{store.SessionInboxDir(oldPath), store.SessionInboxDir(newPath)},
	} {

		if len(filepath.Base(pair[0])) > nameMaxBytes {
			continue
		}
		if err := os.Rename(pair[0], pair[1]); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// reparentSessionBranches rewrites ParentID references from renamed branch IDs
// to their bounded replacements so the branch tree stays connected.
func reparentSessionBranches(dir string, renamed map[string]string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !store.IsSessionTranscriptName(name) {
			continue
		}
		if len(name)+len(".meta") > nameMaxBytes {
			continue
		}
		path := filepath.Join(dir, name)
		unlock := lockSessionSavePath(path)
		meta, ok, err := LoadBranchMeta(path)
		if err == nil && ok {
			if newParent, hit := renamed[meta.ParentID]; hit && newParent != meta.ParentID {
				meta.ParentID = newParent
				err = SaveBranchMetaPreserveUpdated(path, meta)
			}
		}
		unlock()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}
