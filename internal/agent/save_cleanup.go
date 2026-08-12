package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/store"
)

const (
	cleanupPendingExt             = ".cleanup-pending.json"
	maxRecoveryParentStemBytes    = 80
	sessionLockSidecarSuffix      = ".jsonl.lock"
	sessionLeaseLockSidecarSuffix = ".jsonl.lease.lock"
	sessionLeaseInfoSidecarSuffix = ".jsonl.lease.json"
	guardianSidecarSuffix         = ".guardian.jsonl"
	// nameMaxBytes is the single-component filename limit shared by the
	// filesystems Reasonix targets (APFS, ext4, NTFS all cap at 255).
	nameMaxBytes = 255
	// maxSessionBasenameBytes bounds transcript basenames that reconciliation
	// leaves in place. Sidecars append up to ~16 bytes to the transcript name
	// or its stem (".lease.lock", ".cleanup-pending.json", ".guardian.jsonl"),
	// so 224 keeps every sidecar comfortably under nameMaxBytes with headroom
	// for future suffixes. Names past this bound come from the pre-bounded
	// recovery cascade and get renamed by reconcileOverlongSessionFilenames.
	maxSessionBasenameBytes = 224
)

// CleanupPendingMeta records that a session was logically removed but still has
// artifacts waiting for a background job to unwind before physical cleanup.
type CleanupPendingMeta struct {
	Operation string `json:"operation"`
	CreatedAt int64  `json:"createdAt"`
}

// CleanupPendingInfo describes one durable delayed-cleanup marker and the
// session transcript it belongs to.
type CleanupPendingInfo struct {
	SessionPath string
	MarkerPath  string
	Meta        CleanupPendingMeta
}

// CleanupPendingPath returns the durable marker path for a session transcript.
func CleanupPendingPath(sessionPath string) string {
	return store.SessionCleanupPending(sessionPath)
}

// MarkCleanupPending hides a logically removed session from resume/list surfaces
// until delayed physical cleanup has finished.
func MarkCleanupPending(sessionPath, operation string) error {
	path := CleanupPendingPath(sessionPath)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	meta := CleanupPendingMeta{Operation: strings.TrimSpace(operation), CreatedAt: time.Now().UnixMilli()}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}

	return fileutil.AtomicWriteFile(path, b, 0o644)
}

// ClearCleanupPending removes a delayed-cleanup marker after physical cleanup.
func ClearCleanupPending(sessionPath string) error {
	path := CleanupPendingPath(sessionPath)
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// IsCleanupPending reports whether a session is hidden pending delayed cleanup.
func IsCleanupPending(sessionPath string) bool {
	path := CleanupPendingPath(sessionPath)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// IsVisibleSession reports whether a persisted session should appear on normal
// user/agent-facing list, restore, and retrieval surfaces.
func IsVisibleSession(sessionPath string) bool {
	return strings.TrimSpace(sessionPath) != "" && !IsCleanupPending(sessionPath)
}

// ListCleanupPending returns delayed-cleanup markers left in dir. A missing
// directory is not an error.
func ListCleanupPending(dir string) ([]CleanupPendingInfo, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []CleanupPendingInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), cleanupPendingExt) {
			continue
		}
		markerPath := filepath.Join(dir, e.Name())
		var meta CleanupPendingMeta
		b, err := fileencoding.ReadFileUTF8(markerPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if strings.TrimSpace(string(b)) != "" {
			if err := json.Unmarshal(b, &meta); err != nil {
				return nil, fmt.Errorf("read cleanup-pending marker %s: %w", markerPath, err)
			}
		}
		name := strings.TrimSuffix(e.Name(), cleanupPendingExt) + ".jsonl"
		out = append(out, CleanupPendingInfo{
			SessionPath: filepath.Join(dir, name),
			MarkerPath:  markerPath,
			Meta:        meta,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SessionPath < out[j].SessionPath
	})
	return out, nil
}

// ReconcileCleanupPending retries physical cleanup for leftover delayed-cleanup
// markers and stale lock/lease sidecars. It keeps going after individual
// cleanup errors and returns them joined.
func ReconcileCleanupPending(dir string, cleanup func(CleanupPendingInfo) error) error {
	var errs []error
	if err := ReconcileSessionSidecars(dir); err != nil {
		errs = append(errs, err)
	}
	if err := reconcileRecoveryTrashStages(dir); err != nil {
		errs = append(errs, err)
	}
	pending, err := ListCleanupPending(dir)
	if err != nil {
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	for _, item := range pending {
		handled, err := reconcileRecoveryTrashPending(item)
		if !handled {
			if cleanup == nil {
				continue
			}
			err = cleanup(item)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", item.SessionPath, err))
		}
	}
	return errors.Join(errs...)
}

// ReconcileSessionSidecars renames transcripts whose filenames outgrew their
// sidecars and removes stale lock and lease files left beside sessions by
// older runtimes. It never removes .jsonl transcripts; recovered conversations
// may contain useful user history even when their names are ugly.
func ReconcileSessionSidecars(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	var errs []error
	if err := reconcileOverlongSessionFilenames(dir); err != nil {
		errs = append(errs, err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.Join(errs...)
		}
		errs = append(errs, err)
		return errors.Join(errs...)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		sidecarPath := filepath.Join(dir, name)
		switch {
		case strings.HasSuffix(name, sessionLeaseInfoSidecarSuffix):
			base := filepath.Join(dir, strings.TrimSuffix(name, ".lease.json"))
			if err := removeStaleSessionLeaseInfoSidecar(base, sidecarPath); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", sidecarPath, err))
			}
		case strings.HasSuffix(name, sessionLeaseLockSidecarSuffix):
			base := filepath.Join(dir, strings.TrimSuffix(name, ".lease.lock"))
			if err := removeStaleSessionLeaseLockSidecar(base, sidecarPath); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", sidecarPath, err))
			}
		case strings.HasSuffix(name, sessionLockSidecarSuffix):
			base := filepath.Join(dir, strings.TrimSuffix(name, ".lock"))
			if err := removeStaleSessionLockSidecar(base, sidecarPath); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", sidecarPath, err))
			}
		}
	}
	return errors.Join(errs...)
}
