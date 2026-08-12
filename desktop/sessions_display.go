package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/filelock"
	"reasonix/internal/fileutil"
	"reasonix/internal/store"
)

type sessionDisplayMap map[string]map[string]string

type sessionPlannerDisplayMap map[string][]plannerDisplayTurn

type plannerDisplayTurn struct {
	UserHash string           `json:"userHash"`
	Messages []HistoryMessage `json:"messages"`
}

var errCorruptSessionPlannerDisplay = errors.New("corrupt planner display sidecar")

// sessionPlannerDisplayUpdateAfterLoad is a subprocess-test seam. Production
// leaves it nil; tests use it to force two independent processes into the old
// stale read-modify-write window without relying on scheduler timing.
var sessionPlannerDisplayUpdateAfterLoad func()

func messageDisplayKey(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum[:])
}

func loadSessionDisplays(dir string) sessionDisplayMap {
	m := sessionDisplayMap{}
	b, err := readFileUTF8(sessionDisplayPath(dir))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func sessionPlannerDisplayPath(dir string) string {
	return filepath.Join(dir, sessionPlannerDisplayFile)
}

func loadSessionPlannerDisplays(dir string) sessionPlannerDisplayMap {
	m := sessionPlannerDisplayMap{}
	if strings.TrimSpace(dir) == "" {
		return m
	}
	b, err := readFileUTF8(sessionPlannerDisplayPath(dir))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func loadSessionPlannerDisplaysForUpdate(dir string) (sessionPlannerDisplayMap, error) {
	m := sessionPlannerDisplayMap{}
	b, err := readFileUTF8(sessionPlannerDisplayPath(dir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return m, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%w: %w", errCorruptSessionPlannerDisplay, err)
	}
	if m == nil {
		m = sessionPlannerDisplayMap{}
	}
	return m, nil
}

func saveSessionPlannerDisplays(dir string, m sessionPlannerDisplayMap) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(sessionPlannerDisplayPath(dir), b, 0o600)
}

func saveOrRemoveSessionPlannerDisplays(dir string, m sessionPlannerDisplayMap) error {
	if len(m) == 0 {
		err := os.Remove(sessionPlannerDisplayPath(dir))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return saveSessionPlannerDisplays(dir, m)
}

func updateSessionPlannerDisplays(dir string, recoverCorrupt bool, mutate func(sessionPlannerDisplayMap) bool) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("planner display directory is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionSidecarQueueTimeout)
	defer cancel()
	release, err := filelock.AcquireWithExternalTimeout(ctx, sessionPlannerDisplayPath(dir)+".lock", sessionPlannerDisplayExternalLockTimeout)
	if err != nil {
		return fmt.Errorf("lock planner display sidecar: %w", err)
	}
	defer release()

	m, err := loadSessionPlannerDisplaysForUpdate(dir)
	if err != nil {
		if !recoverCorrupt || !errors.Is(err, errCorruptSessionPlannerDisplay) {
			return err
		}

		if removeErr := os.Remove(sessionPlannerDisplayPath(dir)); removeErr != nil && !os.IsNotExist(removeErr) {
			return errors.Join(err, removeErr)
		}
		m = sessionPlannerDisplayMap{}
	}
	if sessionPlannerDisplayUpdateAfterLoad != nil {
		sessionPlannerDisplayUpdateAfterLoad()
	}
	if !mutate(m) {
		return nil
	}
	return saveOrRemoveSessionPlannerDisplays(dir, m)
}

func recordSessionPlannerDisplay(dir, sessionPath, userContent string, messages []HistoryMessage) error {
	if strings.TrimSpace(sessionPath) == "" || strings.TrimSpace(userContent) == "" || len(messages) == 0 {
		return nil
	}
	key := filepath.Base(sessionPath)
	turn := plannerDisplayTurn{
		UserHash: messageDisplayKey(userContent),
		Messages: cloneHistoryMessages(messages),
	}
	return updateSessionPlannerDisplays(dir, false, func(m sessionPlannerDisplayMap) bool {
		m[key] = append(m[key], turn)
		return true
	})
}

func removeSessionPlannerDisplay(dir, sessionPath string) error {
	if strings.TrimSpace(sessionPath) == "" {
		return nil
	}
	key := filepath.Base(sessionPath)
	return updateSessionPlannerDisplays(dir, true, func(m sessionPlannerDisplayMap) bool {
		if _, ok := m[key]; !ok {
			return false
		}
		delete(m, key)
		return true
	})
}

func pruneSessionPlannerDisplays(dir string, protected map[string]struct{}) error {
	return updateSessionPlannerDisplays(dir, true, func(m sessionPlannerDisplayMap) bool {
		changed := false
		for key := range m {
			if sessionDisplayKeyStillOwned(dir, key, protected) {
				continue
			}
			delete(m, key)
			changed = true
		}
		return changed
	})
}

func sessionPlannerDisplayTurns(dir, sessionPath string) []plannerDisplayTurn {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(sessionPath) == "" {
		return nil
	}
	turns := loadSessionPlannerDisplays(dir)[filepath.Base(sessionPath)]
	if len(turns) == 0 {
		return nil
	}
	out := make([]plannerDisplayTurn, 0, len(turns))
	for _, turn := range turns {
		if strings.TrimSpace(turn.UserHash) == "" || len(turn.Messages) == 0 {
			continue
		}
		out = append(out, plannerDisplayTurn{
			UserHash: turn.UserHash,
			Messages: cloneHistoryMessages(turn.Messages),
		})
	}
	return out
}

func saveSessionDisplays(dir string, m sessionDisplayMap) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(sessionDisplayPath(dir), b, 0o600)
}

func saveOrRemoveSessionDisplays(dir string, m sessionDisplayMap) error {
	if len(m) == 0 {
		err := os.Remove(sessionDisplayPath(dir))
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return saveSessionDisplays(dir, m)
}

// updateSessionDisplays serializes the display sidecar's read-modify-write
// cycle. Parallel tabs can record display text concurrently; atomic rename
// protects readers from partial JSON but cannot prevent the last writer from
// replacing another tab's freshly added keys (#6873).
func updateSessionDisplays(dir string, mutate func(sessionDisplayMap) bool) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("display directory is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionSidecarQueueTimeout)
	defer cancel()
	release, err := filelock.AcquireWithExternalTimeout(ctx, sessionDisplayPath(dir)+".lock", sessionDisplayExternalLockTimeout)
	if err != nil {
		return fmt.Errorf("lock display sidecar: %w", err)
	}
	defer release()

	m := loadSessionDisplays(dir)
	if !mutate(m) {
		return nil
	}
	return saveOrRemoveSessionDisplays(dir, m)
}

func removeSessionDisplayKey(dir, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	return updateSessionDisplays(dir, func(m sessionDisplayMap) bool {
		if m[key] == nil {
			return false
		}
		delete(m, key)
		return true
	})
}

func removeSessionDisplay(dir, sessionPath string) error {
	if strings.TrimSpace(sessionPath) == "" {
		return nil
	}
	return removeSessionDisplayKey(dir, filepath.Base(sessionPath))
}

func pruneSessionDisplays(dir string, protected map[string]struct{}) error {
	return updateSessionDisplays(dir, func(m sessionDisplayMap) bool {
		if len(m) == 0 {
			return false
		}
		changed := false
		for key := range m {
			if sessionDisplayKeyStillOwned(dir, key, protected) {
				continue
			}
			delete(m, key)
			changed = true
		}
		return changed
	})
}

func sessionDisplayKeyStillOwned(dir, key string, protected map[string]struct{}) bool {
	key = strings.TrimSpace(key)
	if key == "" || filepath.Base(key) != key || !store.IsSessionTranscriptName(key) {
		return false
	}
	if protected != nil {
		if _, ok := protected[key]; ok {
			return true
		}
	}
	sessionPath := filepath.Join(dir, key)
	if info, err := os.Stat(sessionPath); err == nil && !info.IsDir() {
		return true
	}
	trashPath := filepath.Join(sessionTrashPath(dir), key, key)
	if info, err := os.Stat(trashPath); err == nil && !info.IsDir() {
		return true
	}
	if paths, err := listTrashedSessionFiles(dir); err == nil {
		for _, path := range paths {
			if filepath.Base(path) == key {
				return true
			}
		}
	}
	return false
}

func recordSessionDisplay(dir, sessionPath, content, display string) error {
	if strings.TrimSpace(sessionPath) == "" || content == display || strings.TrimSpace(display) == "" {
		return nil
	}
	return updateSessionDisplays(dir, func(m sessionDisplayMap) bool {
		key := filepath.Base(sessionPath)
		if m[key] == nil {
			m[key] = map[string]string{}
		}
		m[key][messageDisplayKey(content)] = display
		return true
	})
}

// sessionDisplayResolver loads the sidecar once and returns a per-message
// resolver, so a transcript of N messages doesn't re-read .display.json N times.
func sessionDisplayResolver(dir, sessionPath string) func(content string) string {
	return sessionDisplayResolverFromMap(loadSessionDisplays(dir), sessionPath)
}

func sessionDisplayResolverFromMap(displays sessionDisplayMap, sessionPath string) func(content string) string {
	byHash := displays[filepath.Base(sessionPath)]
	return func(content string) string {
		if byHash != nil {
			if display := byHash[messageDisplayKey(content)]; strings.TrimSpace(display) != "" {
				return display
			}
		}
		return historyReplayUserContent(content)
	}
}
