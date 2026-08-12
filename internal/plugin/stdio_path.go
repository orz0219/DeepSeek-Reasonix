package plugin

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"reasonix/internal/proc"
	"reasonix/internal/secrets"
)

var stdioShellPATH = cachedShellPATH(defaultStdioShellPATH)

// cachedShellPATH memoizes the first completed shell-PATH probe: the user's
// interactive PATH is stable for the process, and resolveStdioExecutable now
// probes for every stdio plugin, so caching avoids a login shell per server.
// The probe runs up to three login shells with a 2s timeout each, so it must
// not run under the lock; concurrent spawns share the in-flight probe instead
// of each running (or queueing behind) their own. Empty results are cached too
// — a host without a usable login shell must not re-probe on every spawn —
// except when the probe's context was cancelled, since that empty reflects the
// aborted caller rather than the host, and caching it would pin "" for the
// rest of the process.
func cachedShellPATH(probe func(context.Context) string) func(context.Context) string {
	var (
		mu       sync.Mutex
		cached   string
		done     bool
		inflight chan struct{} // non-nil while a probe runs; closed when it settles
	)
	return func(ctx context.Context) string {
		for {
			mu.Lock()
			if done {
				p := cached
				mu.Unlock()
				return p
			}
			if inflight != nil {
				wait := inflight
				mu.Unlock()
				select {
				case <-wait:
					continue
				case <-ctx.Done():
					return ""
				}
			}
			ch := make(chan struct{})
			inflight = ch
			mu.Unlock()

			p := probe(ctx)

			mu.Lock()
			inflight = nil
			if p != "" || ctx.Err() == nil {
				cached, done = p, true
			}
			mu.Unlock()
			close(ch)
			return p
		}
	}
}

// enrichStdioShellPATH probes the user's interactive login shell for its PATH
// and prepends those directories to the current environment. The result is the
// subprocess environment with a PATH that matches what the user sees in their
// terminal, even when Reasonix was launched from the Finder / Dock / open(1).
func enrichStdioShellPATH(ctx context.Context, env []string) []string {
	currentPath, _ := envValue(env, "PATH")
	if shellPath := strings.TrimSpace(stdioShellPATH(ctx)); shellPath != "" {
		if fallbackPath := mergePathLists(shellPath, currentPath); fallbackPath != currentPath {
			env = setEnvValue(env, "PATH", fallbackPath)
		}
	}
	return env
}

func windowsStdioFallbackPATH(env []string) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	programFiles, _ := envValue(env, "ProgramFiles")
	programFilesX86, _ := envValue(env, "ProgramFiles(x86)")
	localAppData, _ := envValue(env, "LOCALAPPDATA")
	appData, _ := envValue(env, "APPDATA")
	userProfile, _ := envValue(env, "USERPROFILE")
	chocolatey, _ := envValue(env, "ChocolateyInstall")
	if localAppData == "" && userProfile != "" {
		localAppData = filepath.Join(userProfile, "AppData", "Local")
	}
	if appData == "" && userProfile != "" {
		appData = filepath.Join(userProfile, "AppData", "Roaming")
	}
	candidates := []string{
		filepath.Join(programFiles, "nodejs"),
		filepath.Join(programFilesX86, "nodejs"),
		filepath.Join(localAppData, "Programs", "nodejs"),
		filepath.Join(appData, "npm"),
		filepath.Join(localAppData, "Microsoft", "WindowsApps"),
		filepath.Join(userProfile, "scoop", "shims"),
		filepath.Join(userProfile, ".bun", "bin"),
		filepath.Join(userProfile, ".cargo", "bin"),
		filepath.Join(chocolatey, "bin"),
	}
	var existing []string
	for _, dir := range candidates {
		if isDir(dir) {
			existing = append(existing, dir)
		}
	}
	return strings.Join(existing, string(os.PathListSeparator))
}

func defaultStdioShellPATH(ctx context.Context) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	shell := stdioShell()
	if shell == "" {
		return ""
	}
	const marker = "__REASONIX_PATH__="
	script := "printf '\\n" + marker + "%s\\n' \"$PATH\""
	for _, args := range [][]string{
		{"-l", "-i", "-c", script},
		{"-l", "-c", script},
		{"-c", script},
	} {
		out := runShellPATHCommand(ctx, shell, args)
		if path := parseShellPATH(out, marker); path != "" {
			return path
		}
	}
	return ""
}

func stdioShell() string {
	if shell := strings.TrimSpace(os.Getenv("SHELL")); shell != "" {
		if hasPathSeparator(shell) {
			if isExecutableFile(shell) {
				return shell
			}
		} else if exe, ok := lookPathInEnv(shell, secrets.ProcessEnv()); ok {
			return exe
		}
	}
	for _, shell := range []string{"/bin/zsh", "/bin/bash", "/bin/sh"} {
		if isExecutableFile(shell) {
			return shell
		}
	}
	return ""
}

func runShellPATHCommand(parent context.Context, shell string, args []string) []byte {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, args...)

	cmd.Env = secrets.ProcessEnv()
	prepareStdioShellPATHProbe(cmd)
	cmd.Stdin = strings.NewReader("")
	out, _ := cmd.CombinedOutput()
	return out
}

func prepareStdioShellPATHProbe(cmd *exec.Cmd) {
	proc.PrepareShellPATHProbe(cmd)
}

func parseShellPATH(out []byte, marker string) string {
	lines := strings.Split(strings.ReplaceAll(string(out), "\r\n", "\n"), "\n")
	for _, line := range slices.Backward(lines) {
		if rest, ok := strings.CutPrefix(line, marker); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func mergeEnv(base []string, overrides map[string]string) []string {
	out := append([]string(nil), base...)
	for k, v := range overrides {
		out = setEnvValue(out, k, v)
	}
	return out
}

func setEnvValue(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	replaced := false
	for _, kv := range env {
		k, _, ok := strings.Cut(kv, "=")
		if ok && envKeyEqual(k, key) {
			if !replaced {
				out = append(out, key+"="+value)
				replaced = true
			}
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, key+"="+value)
	}
	return out
}

func envValue(env []string, key string) (string, bool) {
	for _, entry := range slices.Backward(env) {
		k, v, ok := strings.Cut(entry, "=")
		if ok && envKeyEqual(k, key) {
			return v, true
		}
	}
	return "", false
}

func envKeyEqual(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func mergePathLists(primary, secondary string) string {
	var out []string
	seen := map[string]bool{}
	for _, path := range []string{primary, secondary} {
		for _, dir := range filepath.SplitList(path) {
			if dir == "" || seen[dir] {
				continue
			}
			seen[dir] = true
			out = append(out, dir)
		}
	}
	return strings.Join(out, string(os.PathListSeparator))
}
