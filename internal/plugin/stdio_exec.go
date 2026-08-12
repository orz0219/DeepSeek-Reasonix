package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"reasonix/internal/sandbox"
)

func prepareMCPPrivateState(s Spec, processSandbox sandbox.Spec, env []string) (sandbox.Spec, []string, error) {
	return prepareMCPPrivateStateForOS(s, processSandbox, env, runtime.GOOS)
}

func prepareMCPPrivateStateForOS(s Spec, processSandbox sandbox.Spec, env []string, goos string) (sandbox.Spec, []string, error) {
	root := strings.TrimSpace(s.StateDir)
	if root == "" {
		return processSandbox, env, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return processSandbox, env, err
	}
	privateRoot := root
	cacheDir := filepath.Join(privateRoot, "cache")
	stateDir := filepath.Join(privateRoot, "state")
	dirs := []string{cacheDir, stateDir}
	privateEnv := map[string]string{
		"XDG_CACHE_HOME": cacheDir, "XDG_STATE_HOME": stateDir,
		"npm_config_cache":      filepath.Join(cacheDir, "npm"),
		"UV_CACHE_DIR":          filepath.Join(cacheDir, "uv"),
		"BUN_INSTALL_CACHE_DIR": filepath.Join(cacheDir, "bun"),
	}
	if goos != "windows" {
		tmpDir := filepath.Join(privateRoot, "tmp")
		dirs = append(dirs, tmpDir)
		privateEnv["TMP"] = tmpDir
		privateEnv["TEMP"] = tmpDir
		privateEnv["TMPDIR"] = tmpDir
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return processSandbox, env, err
		}
	}

	for key, value := range privateEnv {
		env = setEnvValue(env, key, value)
	}
	processSandbox.WriteRoots = append(processSandbox.WriteRoots, root, privateRoot)
	processSandbox.AppContainerWriteRoots = append(processSandbox.AppContainerWriteRoots, root, privateRoot)
	return processSandbox, env, nil
}

func resolveStdioExecutable(ctx context.Context, s Spec, env []string) (string, []string, error) {

	env = enrichStdioShellPATH(ctx, env)

	if hasPathSeparator(s.Command) {
		exe := s.Command
		if !filepath.IsAbs(exe) {
			if dir := stdioWorkingDir(s); dir != "" {
				exe = filepath.Join(dir, exe)
			}
			abs, err := filepath.Abs(exe)
			if err != nil {
				return "", env, fmt.Errorf("stdio plugin %q: resolve command %q: %w", s.Name, s.Command, err)
			}
			exe = abs
		}
		return exe, env, nil
	}
	if exe, ok := lookPathInEnv(s.Command, env); ok {
		return exe, env, nil
	}

	currentPath, _ := envValue(env, "PATH")
	if runtime.GOOS == "windows" {
		fallbackPath := mergePathLists(windowsStdioFallbackPATH(env), currentPath)
		if fallbackPath != currentPath {
			fallbackEnv := setEnvValue(env, "PATH", fallbackPath)
			if exe, ok := lookPathInEnv(s.Command, fallbackEnv); ok {
				return exe, fallbackEnv, nil
			}
			env = fallbackEnv
			currentPath = fallbackPath
		}
	}

	return "", env, fmt.Errorf("stdio plugin %q: command %q not found on PATH; GUI launches and non-interactive sessions may not inherit your shell PATH. Use an absolute command path or set PATH in the MCP server env. PATH=%q",
		s.Name, s.Command, currentPath)
}

// stdioWorkingDir keeps WorkspaceRoot's roots/list role separate from process
// execution for user-installed servers. Only repository-declared servers need
// relative arguments to resolve against the project that supplied the config.
func stdioWorkingDir(s Spec) string {
	if s.Dir != "" {
		return s.Dir
	}
	if s.RequireLaunchApproval {
		return s.WorkspaceRoot
	}
	return ""
}

func hasPathSeparator(s string) bool {
	return strings.ContainsAny(s, `/\`)
}

func lookPathInEnv(command string, env []string) (string, bool) {
	path, _ := envValue(env, "PATH")
	pathext, _ := envValue(env, "PATHEXT")
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		for _, name := range executableNames(command, pathext) {
			candidate := filepath.Join(dir, name)
			if isExecutableFile(candidate) {
				return candidate, true
			}
		}
	}
	return "", false
}

func executableNames(command, pathext string) []string {
	if runtime.GOOS != "windows" || filepath.Ext(command) != "" {
		return []string{command}
	}
	if strings.TrimSpace(pathext) == "" {
		pathext = ".COM;.EXE;.BAT;.CMD"
	}
	names := []string{command}
	seen := map[string]bool{strings.ToLower(command): true}
	for ext := range strings.SplitSeq(pathext, ";") {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		name := command + ext
		key := strings.ToLower(name)
		if !seen[key] {
			seen[key] = true
			names = append(names, name)
		}
	}
	return names
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

func isDir(path string) bool {
	if path == "" {
		return false
	}
	if !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
