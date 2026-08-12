package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"

	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/pluginpkg"
)

// LoadOptions configure Load.
type LoadOptions struct {
	ProjectRoot string
	// HomeDir overrides the OS user home used by legacy callers and tests. The
	// derived global path is <HomeDir>/.reasonix unless ReasonixHomeDir is set.
	HomeDir string
	// ReasonixHomeDir is the exact current Reasonix home (settings.json lives
	// directly under it). When set, it takes precedence over HomeDir for global
	// settings and plugin hooks so Windows %APPDATA%/reasonix and REASONIX_HOME
	// isolation stay consistent across hook/doctor/capdiag (#7411, #7331).
	ReasonixHomeDir string
	// Trusted is retained for source compatibility. Project hooks are enabled
	// automatically now, so callers no longer need to set it.
	Trusted bool
}

// Load resolves hooks: project first, then global; within a scope,
// settings.json array order. A malformed file yields no hooks (never an error
// — a typo shouldn't take down the CLI).
func Load(opts LoadOptions) []ResolvedHook {
	var out []ResolvedHook
	if opts.ProjectRoot != "" {
		p := ProjectSettingsPath(opts.ProjectRoot)
		if s := readSettings(p); s != nil {
			appendResolved(&out, s, ScopeProject, p)
		}
	}
	reasonixHomeDir := reasonixHomeForOptions(opts)
	appendPluginHooks(&out, reasonixHomeDir, opts.ProjectRoot)
	g := filepath.Join(reasonixHomeDir, SettingsFilename)
	if reasonixHomeDir == "" {
		g = GlobalSettingsPath(opts.HomeDir)
	}
	if s := readSettings(g); s != nil {
		appendResolved(&out, s, ScopeGlobal, g)
	} else if !pathExists(g) {
		if legacy := legacyGlobalSettingsPath(opts.HomeDir); legacy != "" {
			if s := readSettings(legacy); s != nil {
				appendResolved(&out, s, ScopeGlobal, legacy)
			}
		}
	}
	return out
}

// ProjectDefinesHooks reports whether a project's settings.json exists and
// declares at least one hook.
func ProjectDefinesHooks(projectRoot string) bool {
	s := readSettings(ProjectSettingsPath(projectRoot))
	if s == nil {
		return false
	}
	for _, e := range Events {
		for _, cfg := range s.Hooks[e] {
			if strings.TrimSpace(cfg.Command) != "" {
				return true
			}
		}
	}
	return false
}

func readSettings(path string) *Settings {
	b, err := fileencoding.ReadFileUTF8(path)
	if err != nil {
		return nil
	}
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	return &s
}

func pathExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil || !os.IsNotExist(err)
}

func appendResolved(out *[]ResolvedHook, s *Settings, scope Scope, source string) {
	if s.Hooks == nil {
		return
	}
	for _, event := range Events {
		for _, cfg := range s.Hooks[event] {
			if strings.TrimSpace(cfg.Command) == "" {
				continue
			}
			cfg.Command = NormalizeCommand(cfg.Command)
			*out = append(*out, ResolvedHook{HookConfig: cfg, Event: event, Scope: scope, Source: source})
		}
	}
}

func appendPluginHooks(out *[]ResolvedHook, reasonixHomeDir, projectRoot string) {
	if strings.TrimSpace(reasonixHomeDir) == "" {
		return
	}
	installed, _ := pluginpkg.LoadInstalled(reasonixHomeDir)
	for _, item := range installed {
		pkg := item.Package
		events := make([]string, 0, len(pkg.Manifest.Hooks))
		for event := range pkg.Manifest.Hooks {
			events = append(events, event)
		}
		sort.Strings(events)
		for _, eventName := range events {
			event := Event(eventName)
			if !validEvent(event) {
				continue
			}
			for _, h := range pkg.Manifest.Hooks[eventName] {
				execution := pluginHookExecutionConfig(h, pkg.Root)
				contextFile := expandPluginRoot(h.ContextFile, pkg.Root)
				if contextFile != "" {
					contextFile = filepath.FromSlash(contextFile)
					if !filepath.IsAbs(contextFile) {
						contextFile = filepath.Join(pkg.Root, contextFile)
					} else {
						contextFile = filepath.Clean(contextFile)
					}
				}
				cwd := expandPluginRoot(h.Cwd, pkg.Root)
				if cwd == "" {
					cwd = pkg.Root
				} else {
					cwd = filepath.FromSlash(cwd)
					if !filepath.IsAbs(cwd) {
						cwd = filepath.Join(pkg.Root, cwd)
					} else {
						cwd = filepath.Clean(cwd)
					}
				}
				env := cloneEnv(h.Env)
				for key, value := range env {
					env[key] = expandPluginRoot(value, pkg.Root)
				}
				env["REASONIX_PLUGIN_ROOT"] = pkg.Root
				env["REASONIX_PLUGIN_NAME"] = item.Installed.Name
				env["REASONIX_HOME"] = reasonixHomeDir
				env["REASONIX_WORKSPACE_ROOT"] = projectRoot
				env["CLAUDE_PROJECT_DIR"] = projectRoot
				env["CLAUDE_PLUGIN_ROOT"] = pkg.Root
				if item.Installed.Version != "" {
					env["REASONIX_PLUGIN_VERSION"] = item.Installed.Version
				}
				*out = append(*out, ResolvedHook{
					HookConfig: HookConfig{
						Match:         h.Match,
						Command:       execution.Command,
						Argv:          execution.Argv,
						ExecutionMode: execution.ExecutionMode,
						Shell:         execution.Shell,
						ContextFile:   contextFile,
						Description:   h.Description,
						Timeout:       h.Timeout,
						Cwd:           cwd,
						Env:           env,
						Async:         h.Async,
						PayloadFormat: h.PayloadFormat,
					},
					Event:  event,
					Scope:  ScopePlugin,
					Source: filepath.Join(pkg.Root, pluginpkg.ManifestPath(pkg.ManifestKind)),
				})
			}
		}
	}
}

func pluginHookExecutionConfig(h pluginpkg.Hook, root string) HookConfig {
	return pluginHookExecutionConfigForPlatform(h, root, runtime.GOOS)
}

func pluginHookExecutionConfigForPlatform(h pluginpkg.Hook, root, goos string) HookConfig {
	mode := ExecutionLegacy
	switch {
	case h.ArgsSet:
		mode = ExecutionExec
	case h.ShellCommand:
		mode = ExecutionShell
	}
	return completePluginHookExecutionConfig(h, root, goos, mode)
}

func expandPluginRoot(value, root string) string {

	lastWrite := 0
	replaced := false
	var out strings.Builder
	for i := 0; i < len(value); {
		tokenLen := pluginRootTokenLen(value[i:])
		if tokenLen == 0 {
			i++
			continue
		}
		if !replaced {
			out.Grow(len(value) - tokenLen + len(root))
			replaced = true
		}
		out.WriteString(value[lastWrite:i])
		out.WriteString(root)
		i += tokenLen
		lastWrite = i
	}
	if !replaced {
		return value
	}
	out.WriteString(value[lastWrite:])
	return out.String()
}

var pluginRootTokens = [...]struct {
	value         string
	needsBoundary bool
}{
	{value: "${CLAUDE_PLUGIN_ROOT}"},
	{value: "$CLAUDE_PLUGIN_ROOT", needsBoundary: true},
	{value: "%CLAUDE_PLUGIN_ROOT%"},
	{value: "${REASONIX_PLUGIN_ROOT}"},
	{value: "$REASONIX_PLUGIN_ROOT", needsBoundary: true},
	{value: "%REASONIX_PLUGIN_ROOT%"},
}

func pluginRootTokenLen(value string) int {
	for _, token := range pluginRootTokens {
		if !strings.HasPrefix(value, token.value) {
			continue
		}
		if token.needsBoundary && len(value) > len(token.value) && isShellVariableNameByte(value[len(token.value)]) {
			continue
		}
		return len(token.value)
	}
	return 0
}

func isShellVariableNameByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func validEvent(event Event) bool {
	return slices.Contains(Events, event)
}

func cloneEnv(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if strings.TrimSpace(k) != "" {
			out[k] = v
		}
	}
	return out
}

// MatchesTool reports whether a hook applies to toolName. The match field is an
// anchored regex; non-tool events always match. A malformed regex never fires
// (safer than firing on everything).
func MatchesTool(h ResolvedHook, toolName string) bool {
	if !UsesToolMatcher(h.Event) {
		return true
	}
	m := h.Match
	if m == "" || m == "*" {
		return true
	}
	re, err := regexp.Compile("^(?:" + m + ")$")
	if err != nil {
		return false
	}
	if h.PayloadFormat != "claude" {
		return re.MatchString(toolName)
	}
	return slices.ContainsFunc(claudeMatchNames(toolName), re.MatchString)
}
