package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"

	"reasonix/internal/config"
)

const (
	fatalCrashDirName           = "crash-fatal"
	fatalCrashLogSuffix         = ".log"
	fatalCrashCoveredSuffix     = ".covered"
	legacyFatalCrashFile        = "crash-fatal.log"
	legacyFatalCrashCoveredFile = "crash-fatal-covered"
)

var fatalCrashProcessAlive = desktopProcessAlive

func fatalCrashDir() string {
	return filepath.Join(config.MemoryUserDir(), fatalCrashDirName)
}

func fatalCrashPath() string {
	return fatalCrashPathForPID(os.Getpid())
}

func fatalCrashPathForPID(pid int) string {
	return filepath.Join(fatalCrashDir(), strconv.Itoa(pid)+fatalCrashLogSuffix)
}

func fatalCrashCoveredPathForPID(pid int) string {
	return filepath.Join(fatalCrashDir(), strconv.Itoa(pid)+fatalCrashCoveredSuffix)
}

func legacyFatalCrashPath() string {
	return filepath.Join(config.MemoryUserDir(), legacyFatalCrashFile)
}

func legacyFatalCrashCoveredPath() string {
	return filepath.Join(config.MemoryUserDir(), legacyFatalCrashCoveredFile)
}

// capturePreviousFatalCrash manages runtime.SetCrashOutput dumps from dead
// processes. Dumps are retained on disk as local diagnostics (no queueing or
// network reporting). Per-PID files keep a routine second launch from
// truncating or unlinking the running primary process's dump.
func capturePreviousFatalCrash() {
	// Preserve compatibility with the single-file format used by older builds.
	// An empty legacy file may still be owned by a running older process, so it
	// must be left untouched until it contains a completed crash dump.
	captureFatalCrashFile(legacyFatalCrashPath(), legacyFatalCrashCoveredPath(), false)

	entries, err := os.ReadDir(fatalCrashDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, ok := fatalCrashPID(entry.Name())
		if !ok || pid == os.Getpid() || fatalCrashProcessAlive(pid) {
			continue
		}
		captureFatalCrashFile(
			filepath.Join(fatalCrashDir(), entry.Name()),
			fatalCrashCoveredPathForPID(pid),
			true,
		)
	}
	for _, entry := range entries {
		pid, ok := fatalCrashCoveredPID(entry.Name())
		if !ok || pid == os.Getpid() || fatalCrashProcessAlive(pid) {
			continue
		}
		if _, err := os.Stat(fatalCrashPathForPID(pid)); os.IsNotExist(err) {
			_ = os.Remove(filepath.Join(fatalCrashDir(), entry.Name()))
		}
	}
	// Best effort: succeeds only when no live/current process artifacts remain.
	_ = os.Remove(fatalCrashDir())
}

func fatalCrashPID(name string) (int, bool) {
	return fatalCrashPIDWithSuffix(name, fatalCrashLogSuffix)
}

func fatalCrashCoveredPID(name string) (int, bool) {
	return fatalCrashPIDWithSuffix(name, fatalCrashCoveredSuffix)
}

func fatalCrashPIDWithSuffix(name, suffix string) (int, bool) {
	if !strings.HasSuffix(name, suffix) {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSuffix(name, suffix))
	return pid, err == nil && pid > 0
}

func captureFatalCrashFile(path, coveredPath string, removeEmpty bool) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, maxCrashStackBytes+1))
	_ = f.Close()
	if readErr != nil || len(strings.TrimSpace(string(raw))) == 0 {
		if removeEmpty {
			_ = os.Remove(coveredPath)
			_ = os.Remove(path)
		}
		return
	}
	if _, err := os.Stat(coveredPath); err == nil {
		_ = os.Remove(coveredPath)
		_ = os.Remove(path)
		return
	}
	// The crash dump is retained on disk as a local diagnostic; queued crash
	// reporting was removed. Clear any covered marker from earlier builds so
	// the dump is not mistaken for already-consumed.
	_ = os.Remove(coveredPath)
}

// installFatalCrashOutput asks the Go runtime to mirror unrecovered panics and
// fatal runtime errors to a durable file. The runtime duplicates the descriptor,
// so the file may be closed after SetCrashOutput returns.
func installFatalCrashOutput() {
	path := fatalCrashPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return
	}
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return
	}
	_ = f.Close()
}
