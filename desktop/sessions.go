package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/filelock"
	"reasonix/internal/fileutil"
	"reasonix/internal/store"
)

// sessions.go holds the desktop-only session-management state that the shared
// kernel doesn't model: custom display titles. A session on disk is just a JSONL
// transcript named by timestamp+model, with no title slot — so the history panel
// stores user-chosen names in a sidecar map (basename → title) next to the .jsonl
// files. The preview (first user message) is the default name; a title overrides
// it. Deleting a session also drops its title entry.

const sessionTitlesFile = ".titles.json"
const sessionDisplayFile = ".display.json"
const sessionPlannerDisplayFile = ".planner-display.json"
const sessionTrashDir = ".trash"
const sessionTrashMetaFile = ".trash-meta.json"

const (
	// Durable sidecar publication includes an fsync while holding the update
	// lock. Keep enough queue budget for a burst of in-process writers on
	// slower Windows disks, but fail external contention quickly so turn
	// completion and retry-queue handoff do not stall behind another process.
	sessionSidecarQueueTimeout        = 5 * time.Second
	sessionSidecarExternalLockTimeout = 750 * time.Millisecond
)

var (
	sessionTitlesQueueTimeout                = sessionSidecarQueueTimeout
	sessionPlannerDisplayExternalLockTimeout = sessionSidecarExternalLockTimeout
	sessionDisplayExternalLockTimeout        = sessionSidecarExternalLockTimeout
)

func sessionTitlesPath(dir string) string  { return filepath.Join(dir, sessionTitlesFile) }
func sessionDisplayPath(dir string) string { return filepath.Join(dir, sessionDisplayFile) }
func sessionTrashPath(dir string) string   { return filepath.Join(dir, sessionTrashDir) }

func desktopSessionDir(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return config.SessionDir()
		}
		root = cwd
	}
	if dir := config.ProjectSessionDir(root); dir != "" {
		return dir
	}
	return config.SessionDir()
}

// loadSessionTitles reads the basename→title map (missing/corrupt → empty).
func loadSessionTitles(dir string) map[string]string {
	m := map[string]string{}
	b, err := readFileWithTimeout(sessionTitlesPath(dir), topicFileReadTimeout)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	// Older builds could persist titles polluted with internal wrappers
	// (memory-compiler contracts, transient blocks) — clean at the read
	// boundary; UserPreviewText is a no-op on clean titles (#5666).
	for key, title := range m {
		m[key] = agent.UserPreviewText(title)
	}
	return m
}

func loadSessionTitlesForUpdate(dir string) (map[string]string, error) {
	return loadStringMapForUpdate(sessionTitlesPath(dir))
}

func updateSessionTitles(dir string, mutate func(map[string]string) bool) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("title directory is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionTitlesQueueTimeout)
	defer cancel()
	release, err := filelock.AcquireWithExternalTimeout(ctx, sessionTitlesPath(dir)+".lock", sessionSidecarExternalLockTimeout)
	if err != nil {
		return fmt.Errorf("lock title sidecar: %w", err)
	}
	defer release()

	m, err := loadSessionTitlesForUpdate(dir)
	if err != nil {
		return err
	}
	if !mutate(m) {
		return nil
	}
	return saveSessionTitles(dir, m)
}

// saveSessionTitles writes the map durably and atomically. Keep this on the
// shared helper so the temporary file is fsynced before it is published.
func saveSessionTitles(dir string, m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(sessionTitlesPath(dir), b, 0o600)
}

// setSessionTitle sets (or, with an empty title, clears) a session's custom name.
func setSessionTitle(dir, sessionPath, title string) error {
	sessionPath, _, err := validateSessionPath(dir, sessionPath)
	if err != nil {
		return err
	}
	key := filepath.Base(sessionPath)
	return updateSessionTitles(dir, func(m map[string]string) bool {
		title = strings.TrimSpace(title)
		if title == "" {
			if _, ok := m[key]; !ok {
				return false
			}
			delete(m, key)
			return true
		}
		if m[key] == title {
			return false
		}
		m[key] = title
		return true
	})
}

// deleteSessionFile moves a session's .jsonl and file sidecars into the local
// trash. Title/display sidecars stay in place so trash previews and restores can
// preserve the user's labels.

// errSessionBusyElsewhere is the sanitized error surfaced when a destructive
// session operation is blocked by a live owner. It intentionally carries no
// writer id, hostname, or path.

// acquireSessionRemovalGuard wraps agent.TryAcquireSessionRemovalGuard with
// the sanitized busy error. The guard holds the session's save and lease
// locks across the destructive operation and deletes the lock files
// atomically with the release — a one-shot busy probe followed by RemoveAll
// would let another process acquire the lease in between and then lose its
// freshly locked lease file, breaking cross-process mutual exclusion.

// liveSessionRemovableWithExistingTrash reports whether a live session file may
// be removed even though a trash copy already exists under the same key: the
// live file must be discardable (empty stub) or byte-identical to the trash
// copy, and no other runtime may hold its session lease — another process could
// be mid-write, and removing the file would silently drop its next save.

// Compare decoded transcripts, not .jsonl bytes: the checkpoint only
// changes at checkpoints, so two byte-identical .jsonl files can hide
// diverged event logs — and treating them as duplicates would delete the
// live session's newer history.

// Acquired after prepareSessionTrashTarget: the duplicate-trash path in
// there takes its own removal guard, and the guard is not reentrant.

// Try os.Rename first — it's atomic and fast when it works.

// Fallback: copy then remove. This handles cross-device moves and the
// Windows case where a directory rename fails because a handle is briefly
// held open (e.g. antivirus scan, indexing, or a just-closed file).

// isRenameCrossDeviceOrBusy reports whether err is a cross-device rename or
// a "file busy" error that a copy+remove fallback can recover from.

// Cross-device link.

// Windows: "The process cannot access the file because it is being used by another process."

// ERROR_SHARING_VIOLATION

// copyPathFn is a seam for tests to simulate a source vanishing mid-copy.

// copyAndRemove recursively copies src to dst, then removes src. Used as a
// fallback when os.Rename fails (cross-device or Windows file-lock races).

// The source vanished mid-copy; drop the partial destination so
// the trash never keeps a truncated artifact that a later restore
// would resurrect as a corrupted transcript.

// On Windows, wait briefly for any file handle release.

// Open source file.

// Create destination file.

// Copy content.

// Close both files before any removal.

func validateSessionPath(dir, sessionPath string) (string, string, error) {
	if strings.TrimSpace(sessionPath) == "" {
		return "", "", fmt.Errorf("empty session path")
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	path := sessionPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(absDir, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	if !store.IsSessionTranscriptName(filepath.Base(absPath)) {
		return "", "", fmt.Errorf("not a session file: %s", sessionPath)
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("session path outside session dir: %s", sessionPath)
	}
	if info, err := os.Lstat(absPath); err == nil {
		if info.IsDir() {
			return "", "", fmt.Errorf("not a session file: %s", sessionPath)
		}
		realDir, dirErr := filepath.EvalSymlinks(absDir)
		if dirErr != nil {
			realDir = absDir
		}
		realPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return "", "", err
		}
		rel, err := filepath.Rel(realDir, realPath)
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
			return "", "", fmt.Errorf("session path escapes session dir: %s", sessionPath)
		}
	} else if !os.IsNotExist(err) {
		return "", "", err
	}
	return absPath, filepath.Base(absPath), nil
}

func validateTrashedSessionPath(dir, sessionPath string) (string, string, string, error) {
	if strings.TrimSpace(sessionPath) == "" {
		return "", "", "", fmt.Errorf("empty session path")
	}
	root, err := filepath.Abs(sessionTrashPath(dir))
	if err != nil {
		return "", "", "", err
	}
	path := sessionPath
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", "", "", err
	}
	if !store.IsSessionTranscriptName(filepath.Base(absPath)) {
		return "", "", "", fmt.Errorf("not a session file: %s", sessionPath)
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", "", "", fmt.Errorf("session path outside trash dir: %s", sessionPath)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid trash session path: %s", sessionPath)
	}
	if parts[0] != parts[1] {
		b, err := readFileUTF8(filepath.Join(root, parts[0], sessionTrashMetaFile))
		if err != nil {
			return "", "", "", fmt.Errorf("invalid trash session path: %s", sessionPath)
		}
		var meta trashedSessionMeta
		if err := json.Unmarshal(b, &meta); err != nil || meta.Key != parts[1] {
			return "", "", "", fmt.Errorf("invalid trash session path: %s", sessionPath)
		}
	}
	if info, err := os.Lstat(absPath); err == nil {
		if info.IsDir() {
			return "", "", "", fmt.Errorf("not a session file: %s", sessionPath)
		}
		realRoot, dirErr := filepath.EvalSymlinks(root)
		if dirErr != nil {
			realRoot = root
		}
		realPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return "", "", "", err
		}
		rel, err := filepath.Rel(realRoot, realPath)
		if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
			return "", "", "", fmt.Errorf("session path escapes trash dir: %s", sessionPath)
		}
	} else if !os.IsNotExist(err) {
		return "", "", "", err
	}
	return absPath, filepath.Base(absPath), filepath.Dir(absPath), nil
}

// sessionPlannerDisplayUpdateAfterLoad is a subprocess-test seam. Production
// leaves it nil; tests use it to force two independent processes into the old
// stale read-modify-write window without relying on scheduler timing.

// A corrupt shared map cannot be edited safely. Destructive cleanup is
// allowed to retire the unreadable sidecar so deleted-session display
// data does not linger and later records can start from a valid map.

// updateSessionDisplays serializes the display sidecar's read-modify-write
// cycle. Parallel tabs can record display text concurrently; atomic rename
// protects readers from partial JSON but cannot prevent the last writer from
// replacing another tab's freshly added keys (#6873).

// sessionDisplayResolver loads the sidecar once and returns a per-message
// resolver, so a transcript of N messages doesn't re-read .display.json N times.
