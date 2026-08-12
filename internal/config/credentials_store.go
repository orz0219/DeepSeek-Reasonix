package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"
)

// StoreCredentialLines stores KEY=value assignments in Reasonix's global .env
// and pins them into the current process environment.
func StoreCredentialLines(lines []string) (string, error) {
	assignments := parseCredentialLines(lines)
	if len(assignments) == 0 {
		return CredentialsTargetDescription(), nil
	}
	unlock, err := LockUserCredentialEdits()
	if err != nil {
		return "", err
	}
	defer unlock()
	return storeCredentialAssignmentsLocked(assignments)
}

// storeCredentialIfAbsentAndNotCleared writes key=value only when the current
// credential store still lacks a value and a cleared tombstone for key. The
// re-check and write share LockUserCredentialEdits so a concurrent settings
// save or tombstone cannot be overwritten by a stale keyring import.
// stored is false when the write was skipped because the key is already present
// or cleared; err is non-nil only on lock/IO failures.
func storeCredentialIfAbsentAndNotCleared(key, value string) (stored bool, err error) {
	key = strings.TrimSpace(key)
	if key == "" || !isCredentialKey(key) {
		return false, nil
	}
	if strings.ContainsAny(value, "\r\n") {
		return false, fmt.Errorf("credential value for %s contains a newline", key)
	}
	unlock, err := LockUserCredentialEdits()
	if err != nil {
		return false, err
	}
	defer unlock()
	if credentialCurrentStoreHasKey(key) || credentialCurrentStoreClearedKey(key) {
		return false, nil
	}
	if _, err := storeCredentialAssignmentsLocked(map[string]string{key: value}); err != nil {
		return false, err
	}
	return true, nil
}

func storeCredentialAssignmentsLocked(assignments map[string]string) (string, error) {
	if err := storeCredentialsInFile(UserCredentialsPath(), assignments); err != nil {
		return "", err
	}
	pinCredentialAssignments(assignments)
	return UserCredentialsPath(), nil
}

func SetCredential(key, value string) (string, error) {
	key = strings.TrimSpace(key)
	if !isCredentialKey(key) {
		return "", fmt.Errorf("invalid credential key %q", key)
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("credential value for %s contains a newline", key)
	}
	return StoreCredentialLines([]string{key + "=" + value})
}

// SetCredentialIfRevision stores one credential only when the global
// credential file still has expectedRevision. The comparison and write share
// the same process and advisory file lock, preventing a stale setup page in one
// Reasonix process from overwriting a credential saved by another process.
func SetCredentialIfRevision(key, value, expectedRevision string) (string, bool, error) {
	key = strings.TrimSpace(key)
	if !isCredentialKey(key) {
		return "", false, fmt.Errorf("invalid credential key %q", key)
	}
	if strings.ContainsAny(value, "\r\n") {
		return "", false, fmt.Errorf("credential value for %s contains a newline", key)
	}
	assignments := parseCredentialLines([]string{key + "=" + value})
	if len(assignments) != 1 {
		return "", false, fmt.Errorf("invalid credential assignment for %s", key)
	}

	unlock, err := LockUserCredentialEdits()
	if err != nil {
		return "", false, err
	}
	defer unlock()
	if expectedRevision == "" || CredentialStoreRevision() != expectedRevision {
		return CredentialsTargetDescription(), false, nil
	}
	path, err := storeCredentialAssignmentsLocked(assignments)
	if err != nil {
		return "", false, err
	}
	return path, true, nil
}

// IsValidCredentialKey reports whether key can be stored in Reasonix's dotenv
// credential file and exposed as an environment variable.
func IsValidCredentialKey(key string) bool {
	return isCredentialKey(strings.TrimSpace(key))
}

func RemoveCredential(key string) error {
	key = strings.TrimSpace(key)
	if key == "" || !isCredentialKey(key) {
		return nil
	}
	unlock, err := LockUserCredentialEdits()
	if err != nil {
		return err
	}
	defer unlock()
	if path := UserCredentialsPath(); path != "" {
		if err := removeCredentialFromFile(path, key); err != nil {
			return err
		}
	}
	return os.Unsetenv(key)
}

// LockUserCredentialEdits serializes credential-store compare/write
// transactions in this process and across Reasonix processes. When both the
// user config and credential store are needed, acquire LockUserConfigEdits
// first, then this lock.
func LockUserCredentialEdits() (func(), error) {
	userCredentialEditMu.Lock()
	path := UserCredentialsPath()
	if strings.TrimSpace(path) == "" {
		userCredentialEditMu.Unlock()
		return nil, fmt.Errorf("credentials store unavailable")
	}
	unlockFile, err := acquireConfigFileEditLockWithTimeout(path, configEditLockTimeout)
	if err != nil {
		userCredentialEditMu.Unlock()
		return nil, fmt.Errorf("lock credential edits: %w", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockFile()
			userCredentialEditMu.Unlock()
		})
	}, nil
}

// CredentialStoreRevision returns a content-derived revision for the current
// Reasonix credential store. Callers performing compare-and-apply must hold
// LockUserCredentialEdits from this read through their commit.
func CredentialStoreRevision() string {
	path := UserCredentialsPath()
	if strings.TrimSpace(path) == "" {
		return "unavailable"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "missing"
		}
		return "unreadable"
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("sha256:%x", sum[:])
}

func CredentialIsSet(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	return CredentialStored(key)
}

func CredentialStored(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	return envFileHasValue(UserCredentialsPath(), key)
}

func credentialCurrentStoreHasKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	return envFileHasValue(UserCredentialsPath(), key)
}

func credentialCurrentStoreClearedKey(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	return envFileHasClearedKey(UserCredentialsPath(), key)
}

func CredentialsTargetDescription() string {
	return UserCredentialsPath()
}

func pinCredentialAssignments(assignments map[string]string) {
	for key, value := range assignments {
		_ = os.Setenv(key, value)
		recordCredentialSource(key, value, CredentialSource{Kind: CredentialSourceCredentials, Path: UserCredentialsPath(), Label: "Reasonix credentials (.env)"})
	}
}
