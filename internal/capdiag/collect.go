package capdiag

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/instruction"
	"reasonix/internal/plugin"
	"reasonix/internal/pluginpkg"
	"reasonix/internal/skill"
)

// Collect builds a capability diagnostics report. It never writes config,
// cache, state, or log files. Live MCP is opt-in via Options.Live.
func Collect(opts Options) Report {
	root := opts.Root
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		} else {
			root = "."
		}
	}
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}

	home := opts.HomeDir
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	reasonixHome := opts.ReasonixHomeDir
	if reasonixHome == "" {
		if opts.HomeDir != "" {
			reasonixHome = filepath.Join(home, ".reasonix")
		} else {
			reasonixHome = config.ReasonixHomeDir()
		}
	}

	// Read-only load: never rewrite legacy tier lines or other config on disk.
	cfg, cfgErr := config.LoadForRootReadOnly(root)
	if cfg == nil {
		cfg = config.Default()
	}

	issues := []Issue{}
	if cfgErr != nil {
		issues = append(issues, Issue{
			Severity: "error", Code: "config.load_failed", Subsystem: "config",
			Message:     "failed to load configuration: " + sanitizeErrTextWithPaths(cfgErr.Error(), root, home, reasonixHome),
			Remediation: "Fix reasonix.toml / config.toml syntax, then re-run doctor capabilities",
		})
	}

	disp := func(p string) string { return displayPath(p, root, home, reasonixHome) }

	instr, instructionIssues := collectInstructions(root, home, disp)
	skillsR, skillIssues := collectSkills(root, home, reasonixHome, cfg, disp)
	cmdsR, cmdIssues := collectCommands(root, disp)
	pluginsR, pluginIssues := collectPlugins(reasonixHome, disp)
	mcpR, mcpIssues := collectMCP(cfg, root, home, reasonixHome, disp)

	issues = append(issues, instructionIssues...)
	issues = append(issues, skillIssues...)
	issues = append(issues, cmdIssues...)
	issues = append(issues, pluginIssues...)
	issues = append(issues, mcpIssues...)

	// Runtime host merge (desktop) or live probe (CLI).
	if opts.Live {
		liveIssues := probeLiveMCP(&mcpR, cfg, root, home, reasonixHome, opts.LiveTimeout)
		issues = append(issues, liveIssues...)
	} else if opts.RuntimeHost != nil {
		mergeRuntimeHost(&mcpR, opts.RuntimeHost, root, home, reasonixHome, &issues)
	}

	sortIssues(issues)
	report := Report{
		SchemaVersion: SchemaVersion,
		Root:          disp(root),
		Live:          opts.Live,
		Instructions:  instr,
		Skills:        skillsR,
		Commands:      cmdsR,
		Plugins:       pluginsR,
		MCP:           mcpR,
		Issues:        issues,
	}
	report.Summary = buildSummary(report)
	return report
}

// CollectWithRuntimeUnavailable adds mcp.runtime_unavailable when desktop
// requested session runtime but no Host is available.
func CollectWithRuntimeUnavailable(opts Options) Report {
	r := Collect(opts)
	if opts.RuntimeHost == nil && !opts.Live {
		r.Issues = append(r.Issues, Issue{
			Severity: "info", Code: "mcp.runtime_unavailable", Subsystem: "mcp",
			Message:     "no active session Host; showing static configuration only",
			Remediation: "Open or select a workspace chat tab, then refresh with session runtime enabled",
			SettingsTab: "mcp",
		})
		sortIssues(r.Issues)
		r.Summary = buildSummary(r)
	}
	return r
}

func buildSummary(r Report) Summary {
	s := Summary{
		Instructions: len(r.Instructions.Docs),
		Skills:       r.Skills.Winners,
		Commands:     r.Commands.Winners,
		Plugins:      len(r.Plugins.Packages),
		MCPServers:   len(r.MCP.Servers),
	}
	for _, is := range r.Issues {
		switch is.Severity {
		case "error":
			s.Errors++
		case "warning":
			s.Warnings++
		default:
			s.Infos++
		}
	}
	return s
}

func collectInstructions(root, home string, disp func(string) string) (InstructionsReport, []Issue) {
	userDir := config.MemoryUserDir()
	if home != "" && (userDir == "" || strings.Contains(userDir, home)) {
		// Prefer explicit test home when Reasonix home is under it.
		if custom := filepath.Join(home, ".reasonix"); custom != "" {
			if userDir == "" {
				userDir = custom
			}
		}
	}
	resolved := instruction.Resolve(instruction.ResolveOptions{TargetDir: root, UserDir: userDir})
	out := InstructionsReport{Docs: []InstructionDoc{}}
	for i, d := range resolved.Documents {
		out.Docs = append(out.Docs, InstructionDoc{
			Path: disp(d.Path), Scope: string(d.Scope), Directory: disp(d.Directory),
			Depth: d.Depth, Order: i + 1,
		})
	}
	issues := make([]Issue, 0, len(resolved.Diagnostics))
	for _, diagnostic := range resolved.Diagnostics {
		source := diagnostic.SourcePath
		if source == "" {
			source = diagnostic.Path
		}
		issues = append(issues, Issue{
			Severity: "warning", Code: "instruction." + diagnostic.Code, Subsystem: "instructions",
			Source: disp(source), Message: diagnostic.Message,
			Remediation: "Fix or remove the referenced instruction import, then start a new session",
			SettingsTab: "memory",
		})
	}
	return out, issues
}

func collectSkills(root, home, reasonixHome string, cfg *config.Config, disp func(string) string) (AssetReport, []Issue) {
	var issues []Issue
	store := skill.New(skill.Options{
		HomeDir:         home,
		ReasonixHomeDir: reasonixHome,
		ProjectRoot:     root,
		CustomPaths:     cfg.SkillCustomPaths(),
		ExcludedPaths:   cfg.SkillExcludedPaths(),
		DisabledNames:   cfg.DisabledSkillNames(),
		MaxDepth:        cfg.SkillMaxDepth(),
		Stderr:          ioDiscard(),
	})
	insp := store.Inspect()
	rep := AssetReport{Roots: []RootInfo{}, Entries: []AssetEntry{}}
	for _, r := range insp.Roots {
		rep.Roots = append(rep.Roots, RootInfo{
			Path: disp(r.Dir), Scope: string(r.Scope), Status: string(r.Status),
		})
	}
	for _, c := range insp.Candidates {
		ent := AssetEntry{
			Name: c.Name, Description: c.Description, Scope: string(c.Scope),
			Path: disp(c.Path), Status: string(c.Status), RunAs: string(c.RunAs),
		}
		if c.WinnerPath != "" {
			ent.WinnerPath = disp(c.WinnerPath)
		}
		rep.Entries = append(rep.Entries, ent)
		switch c.Status {
		case skill.CandidateWinner:
			rep.Winners++
			if skill.MissingDescription(c.Description) {
				issues = append(issues, Issue{
					Severity: "warning", Code: "skill.missing_description", Subsystem: "skills",
					Name: c.Name, Source: disp(c.Path),
					Message:     "skill has no description frontmatter; index quality is reduced",
					Remediation: "Add a one-line description: field to the skill frontmatter",
					SettingsTab: "skills",
				})
			}
		case skill.CandidateShadowed:
			rep.Shadowed++
			issues = append(issues, Issue{
				Severity: "info", Code: "skill.shadowed", Subsystem: "skills",
				Name: c.Name, Source: disp(c.Path),
				Message:     "skill is shadowed by a higher-priority winner at " + disp(c.WinnerPath),
				Remediation: "Rename, remove, or disable the lower-priority skill if the winner is unintended",
				SettingsTab: "skills",
			})
		case skill.CandidateDisabled:
			rep.Disabled++
			issues = append(issues, Issue{
				Severity: "info", Code: "skill.disabled", Subsystem: "skills",
				Name: c.Name, Source: disp(c.Path),
				Message:     "skill is listed in disabled_skills and will not load",
				Remediation: "Remove the name from [skills].disabled_skills to re-enable",
				SettingsTab: "skills",
			})
		}
	}
	return rep, issues
}

func collectCommands(root string, disp func(string) string) (AssetReport, []Issue) {
	var issues []Issue
	dirs := config.CommandDirsForRoot(root)
	insp := command.Inspect(dirs...)
	rep := AssetReport{Roots: []RootInfo{}, Entries: []AssetEntry{}}
	for _, r := range insp.Roots {
		rep.Roots = append(rep.Roots, RootInfo{Path: disp(r.Dir), Status: r.Status})
	}
	for _, c := range insp.Candidates {
		ent := AssetEntry{
			Name: c.Name, Description: c.Description, Path: disp(c.Path), Status: string(c.Status),
		}
		if c.WinnerPath != "" {
			ent.WinnerPath = disp(c.WinnerPath)
		}
		if c.Error != "" {
			ent.Error = sanitizeErrText(c.Error)
		}
		rep.Entries = append(rep.Entries, ent)
		switch c.Status {
		case command.CandidateWinner:
			rep.Winners++
		case command.CandidateShadowed:
			rep.Shadowed++
			issues = append(issues, Issue{
				Severity: "info", Code: "command.shadowed", Subsystem: "commands",
				Name: c.Name, Source: disp(c.Path),
				Message:     "command is overridden by later directory winner at " + disp(c.WinnerPath),
				Remediation: "Remove or rename the earlier command file if the override is unintended",
			})
		case command.CandidateError:
			rep.ParseErrors++
			issues = append(issues, Issue{
				Severity: "error", Code: "command.read_failed", Subsystem: "commands",
				Name: c.Name, Source: disp(c.Path),
				Message:     "failed to read or parse command file",
				Remediation: "Fix file permissions/encoding or remove the broken file",
			})
		}
	}
	return rep, issues
}

func collectPlugins(reasonixHome string, disp func(string) string) (PluginPackageReport, []Issue) {
	var issues []Issue
	rep := PluginPackageReport{
		StatePath: disp(filepath.Join(reasonixHome, pluginpkg.StateFilename)),
		Packages:  []PluginPackageInfo{},
	}
	st, err := pluginpkg.LoadState(reasonixHome)
	if err != nil {
		issues = append(issues, Issue{
			Severity: "error", Code: "plugin.state_read_failed", Subsystem: "plugins",
			Source:      rep.StatePath,
			Message:     "failed to read plugin-packages state",
			Remediation: "Ensure Reasonix home is readable or reinstall packages",
			SettingsTab: "plugins",
		})
		return rep, issues
	}
	// Stable order by name.
	sort.SliceStable(st.Plugins, func(i, j int) bool {
		return st.Plugins[i].Name < st.Plugins[j].Name
	})
	for _, p := range st.Plugins {
		info := PluginPackageInfo{
			Name: p.Name, Enabled: p.Enabled, Version: p.Version,
			Root:         disp(pluginpkg.ResolveRoot(reasonixHome, p.Root)),
			ManifestKind: p.ManifestKind, Status: "ok",
		}
		root := pluginpkg.ResolveRoot(reasonixHome, p.Root)
		if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
			info.Status = "missing_root"
			issues = append(issues, Issue{
				Severity: "error", Code: "plugin.missing_root", Subsystem: "plugins",
				Name: p.Name, Source: disp(root),
				Message:     "plugin package root directory is missing",
				Remediation: "Reinstall the plugin package or remove it from plugin-packages.json",
				SettingsTab: "plugins",
			})
			rep.Packages = append(rep.Packages, info)
			continue
		}
		pkg, warnings, perr := pluginpkg.ParseDir(root)
		if perr != nil {
			info.Status = "invalid_manifest"
			issues = append(issues, Issue{
				Severity: "error", Code: "plugin.invalid_manifest", Subsystem: "plugins",
				Name: p.Name, Source: disp(root),
				Message:     "plugin package manifest is invalid: " + sanitizeErr(perr),
				Remediation: "Fix reasonix-plugin.json / Codex / Claude plugin manifest",
				SettingsTab: "plugins",
			})
			rep.Packages = append(rep.Packages, info)
			continue
		}
		sk, commands, mcp := pkg.CapabilityCounts()
		info.Skills, info.Commands, info.MCPServers = sk, commands, mcp
		info.Prompts, info.Themes = pkg.PromptCount(), pkg.ThemeCount()
		info.Runtime = pkg.Manifest.Runtime != nil
		if p.ManifestKind == "" {
			info.ManifestKind = pkg.ManifestKind
		}
		for _, w := range warnings {
			info.Warnings = append(info.Warnings, w)
			issues = append(issues, Issue{
				Severity: "warning", Code: "plugin.compatibility", Subsystem: "plugins",
				Name: p.Name, Source: disp(root),
				Message:     w,
				Remediation: "Review plugin package documentation for unsupported Claude/Codex features",
				SettingsTab: "plugins",
			})
		}
		if !p.Enabled {
			info.Status = "disabled"
		}
		rep.Packages = append(rep.Packages, info)
	}
	return rep, issues
}

func collectMCP(cfg *config.Config, root, home, reasonixHome string, disp func(string) string) (MCPReport, []Issue) {
	var issues []Issue
	rep := MCPReport{Servers: []MCPServerInfo{}}
	if cfg == nil {
		return rep, issues
	}
	// Stable order by name.
	entries := append([]config.PluginEntry(nil), cfg.Plugins...)
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	for _, p := range entries {
		info := MCPServerInfo{
			Name:        p.Name,
			Effective:   true,
			Transport:   transportOf(p.Type),
			StartIntent: "automatic",
			EnvKeys:     sortedKeys(p.Env),
			HeaderKeys:  sortedKeys(p.Headers),
		}
		if !p.ShouldAutoStart() {
			info.StartIntent = "off"
		}
		if owner, ok := cfg.PluginPackageOwner(p.Name); ok {
			info.PackageOwner = owner
			info.Source = "plugin_package"
		} else {
			info.Source = strings.TrimSpace(string(p.Source))
			if info.Source == "" {
				info.Source = guessMCPSource(root, p.Name)
			}
			if sourcePath := config.MCPConfigPathForEntry(root, p); strings.TrimSpace(sourcePath) != "" {
				info.SourcePath = disp(sourcePath)
			}
		}
		if info.Transport == "stdio" {
			info.Command = redactCommandDisplay(p.Command, root, home, reasonixHome)
		} else {
			info.URLHost = urlHostOnly(p.URL)
		}
		if !isValidTransport(p.Type) && strings.TrimSpace(p.Type) != "" {
			issues = append(issues, Issue{
				Severity: "error", Code: "mcp.invalid_transport", Subsystem: "mcp",
				Name: p.Name, Source: info.Source,
				Message:     "unsupported MCP transport " + p.Type,
				Remediation: "Use type stdio, http, or sse",
				SettingsTab: "mcp",
			})
		}
		if transportOf(p.Type) == "stdio" {
			if strings.TrimSpace(p.Command) == "" {
				issues = append(issues, Issue{
					Severity: "error", Code: "mcp.missing_command", Subsystem: "mcp",
					Name: p.Name, Source: info.Source,
					Message:     "stdio MCP server has empty command",
					Remediation: "Set command (and optional args) for the server",
					SettingsTab: "mcp",
				})
			} else if !commandExists(p.Command) {
				// Static LookPath cannot mirror GUI/login-shell PATH enrichment used
				// at runtime; treat as warning so diagnostics do not hard-fail a
				// command that may still start under the real session environment.
				issues = append(issues, Issue{
					Severity: "warning", Code: "mcp.command_not_found", Subsystem: "mcp",
					Name: p.Name, Source: info.Source,
					Message:     "MCP command is not found via static PATH lookup (GUI/login-shell PATH may still resolve it at runtime)",
					Remediation: "Use an absolute command path, set PATH in the server env, or verify with session runtime / --live",
					SettingsTab: "mcp",
				})
			}
		} else if strings.TrimSpace(p.URL) == "" {
			issues = append(issues, Issue{
				Severity: "error", Code: "mcp.missing_url", Subsystem: "mcp",
				Name: p.Name, Source: info.Source,
				Message:     "remote MCP server has empty url",
				Remediation: "Set a valid http(s) URL for the server",
				SettingsTab: "mcp",
			})
		}
		rep.Servers = append(rep.Servers, info)
	}
	return rep, issues
}

func guessMCPSource(root, name string) string {
	// Best-effort: check project .mcp.json presence of the name without loading secrets.
	mcpPath := filepath.Join(root, ".mcp.json")
	if b, err := os.ReadFile(mcpPath); err == nil {
		// Cheap substring check; false positives are acceptable for diagnostics.
		if strings.Contains(string(b), `"`+name+`"`) {
			return "mcp_json"
		}
	}
	return "toml"
}

func commandExists(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	if filepath.IsAbs(cmd) {
		st, err := os.Stat(cmd)
		return err == nil && !st.IsDir()
	}
	// Relative path or PATH lookup — use LookPath.
	if _, err := lookPath(cmd); err == nil {
		return true
	}
	// Relative workspace path.
	if st, err := os.Stat(cmd); err == nil && !st.IsDir() {
		return true
	}
	return false
}

func mergeRuntimeHost(rep *MCPReport, host *plugin.Host, root, home, reasonixHome string, issues *[]Issue) {
	if host == nil {
		return
	}
	byName := map[string]int{}
	for i, s := range rep.Servers {
		byName[s.Name] = i
	}
	for _, s := range host.Servers() {
		tools := make([]MCPToolInfo, 0, len(s.ToolList))
		for _, t := range s.ToolList {
			tools = append(tools, MCPToolInfo{Name: t.Name, ReadOnlyHint: t.ReadOnlyHint, DestructiveHint: t.DestructiveHint})
		}
		if i, ok := byName[s.Name]; ok {
			rep.Servers[i].RuntimeStatus = "connected"
			rep.Servers[i].ToolCount = s.Tools
			rep.Servers[i].Tools = tools
			// Only warn when the server advertised a tools capability but listed none.
			if s.HasTools && s.Tools == 0 {
				*issues = append(*issues, Issue{
					Severity: "warning", Code: "mcp.no_tools", Subsystem: "mcp",
					Name: s.Name, Message: "MCP server connected but exposes no tools",
					Remediation: "Check server configuration and authentication",
					SettingsTab: "mcp",
				})
			}
		} else {
			rep.Servers = append(rep.Servers, MCPServerInfo{
				Name: s.Name, Transport: s.Transport, RuntimeStatus: "connected",
				ToolCount: s.Tools, Tools: tools, StartIntent: "automatic",
			})
		}
	}
	for _, f := range host.Failures() {
		errText := sanitizeErrTextWithPaths(f.Error, root, home, reasonixHome)
		if i, ok := byName[f.Name]; ok {
			rep.Servers[i].RuntimeStatus = "failed"
			rep.Servers[i].Error = errText
			rep.Servers[i].StartupStage = f.Stage
			rep.Servers[i].StartupElapsedMS = f.Elapsed.Milliseconds()
			rep.Servers[i].Stderr = sanitizeErrTextWithPaths(f.Stderr, root, home, reasonixHome)
		}
		*issues = append(*issues, Issue{
			Severity: "error", Code: "mcp.start_failed", Subsystem: "mcp",
			Name: f.Name, Message: "MCP server failed in the current session: " + errText,
			Remediation: "Inspect server logs, command/URL, and authentication; retry from Settings → MCP",
			SettingsTab: "mcp",
		})
	}
	for _, name := range host.ConnectingServers() {
		if i, ok := byName[name]; ok && rep.Servers[i].RuntimeStatus == "" {
			rep.Servers[i].RuntimeStatus = "deferred"
		}
	}
	// Mark auto_start=off with empty runtime as disabled.
	for i := range rep.Servers {
		if rep.Servers[i].RuntimeStatus == "" && rep.Servers[i].StartIntent == "off" {
			rep.Servers[i].RuntimeStatus = "disabled"
		}
	}
}

func sortIssues(issues []Issue) {
	sev := map[string]int{"error": 0, "warning": 1, "info": 2}
	sort.SliceStable(issues, func(i, j int) bool {
		if sev[issues[i].Severity] != sev[issues[j].Severity] {
			return sev[issues[i].Severity] < sev[issues[j].Severity]
		}
		if issues[i].Code != issues[j].Code {
			return issues[i].Code < issues[j].Code
		}
		if issues[i].Name != issues[j].Name {
			return issues[i].Name < issues[j].Name
		}
		return issues[i].Source < issues[j].Source
	})
}

// sanitizeErrText redacts secrets and machine-local identity from diagnostic
// strings. Prefer sanitizeErrTextWithPaths when workspace/home are known.

// Collapse whitespace so multi-line stderr is one line.

// Strip URL query/fragment early.

// Only cut when it looks like a URL fragment, not ordinary prose "?".

// PATH=... (often embedded in stdio resolve errors; not a credential, so
// the shared redactor below leaves it alone).

// Transport errors embed arbitrary HTTP bodies and stdio stderr, so run
// the product-wide credential recognizer (KEY=value and JSON
// "key":"value" forms, Authorization schemes, Cookie/Set-Cookie values,
// Bearer/JWT/vendor token shapes) instead of a second, narrower list.

// The shared Bearer pattern only masks tokens of 16+ chars; diagnostics
// text can afford to redact shorter bearer tokens too.

// Absolute paths: rewrite with displayPath when possible.

// Cap length after redaction.

// Walk for POSIX and Windows absolute path-like tokens.

// Find candidate start: / or X:\

// Only rewrite if it looks like a path with a directory separator beyond root.

// Only show the command token, redacted if it looks like a path.

// ioDiscard avoids importing io in every call site for skill.Options.Stderr.
func ioDiscard() *discardWriter { return &discardWriter{} }

type discardWriter struct{}

func (d *discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// DefaultLiveTimeout is used when --live is set without --timeout.
const DefaultLiveTimeout = 5 * time.Second

// MinLiveTimeout / MaxLiveTimeout bound --timeout.
const (
	MinLiveTimeout = 1 * time.Second
	MaxLiveTimeout = 60 * time.Second
)
