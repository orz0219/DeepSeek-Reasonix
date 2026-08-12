package boot

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/lsp"
	"reasonix/internal/netclient"
	"reasonix/internal/plugin"
	"reasonix/internal/sandbox"
	"reasonix/internal/sessiontemp"
	"reasonix/internal/skill"
	"reasonix/internal/tool"
	"reasonix/internal/tool/builtin"
)

// addBuiltins adds enabled built-in tools to reg. An empty list means all of
// them. writeRoots confines the file-writing built-ins to the workspace: after
// the (unconfined) defaults are added, each enabled writer is replaced by an
// instance bound to writeRoots (preserving registry order).
// forbidReadRoots confines the read/list/search built-ins so they cannot peek at
// the listed directories.
// When workDir is non-empty, tools resolve relative paths against it instead of
// the process cwd, enabling concurrent multi-project sessions.
// sessionGuard blocks writer-tool targets inside Reasonix's own session stores
// and makes bash warn when a command references them. managedConfig names the
// Reasonix-owned config files writable outside writeRoots after a fresh
// per-write human approval.
func addBuiltins(reg *tool.Registry, enabled, writeRoots []string, bashSpec sandbox.Spec, bashTimeout time.Duration, searchSpec builtin.SearchSpec, stderr io.Writer, workDir string, proxySpec netclient.ProxySpec, forbidReadRoots []string, readPathResolver *builtin.PathResolver, sessionGuard builtin.SessionDataGuard, managedConfig builtin.ManagedConfigPaths, overlay builtin.FileOverlay, terminal builtin.TerminalRunner, sessionTemp *sessiontemp.Manager, fileWriteReceipt func(path string, hadPrior bool, prior []byte)) {

	if workDir != "" {
		ws := builtin.Workspace{Dir: workDir, WriteRoots: writeRoots, ForbidReadRoots: forbidReadRoots, Bash: bashSpec, BashTimeout: bashTimeout, Search: searchSpec, ProxySpec: proxySpec, ReadPaths: readPathResolver, SessionGuard: sessionGuard, ManagedConfig: managedConfig, FileOverlay: overlay, Terminal: terminal, SessionTemp: sessionTemp, FileWriteReceipt: fileWriteReceipt}
		for _, t := range ws.Tools(enabled...) {
			reg.Add(t)
		}
		return
	}

	if len(enabled) == 0 {
		for _, t := range tool.Builtins() {
			reg.Add(t)
		}
	} else {
		for _, name := range enabled {
			if t, ok := tool.LookupBuiltin(name); ok {
				reg.Add(t)
			} else {
				fmt.Fprintf(stderr, "warning: unknown built-in tool %q\n", name)
			}
		}
	}

	bashTool := builtin.ConfineBash(bashSpec, sessionGuard, bashTimeout)
	if rebound, ok := builtin.BindSessionTemp(bashTool, sessionTemp); ok {
		bashTool = rebound
	}
	searchTool := builtin.ConfineSearch(searchSpec, bashSpec, forbidReadRoots)
	if rebound, ok := builtin.BindSessionTemp(searchTool, sessionTemp); ok {
		searchTool = rebound
	}
	writers := builtin.ConfineWriters(writeRoots, sessionGuard, managedConfig)
	for i, writer := range writers {
		writers[i] = builtin.BindFileWriteReceipt(writer, fileWriteReceipt)
	}
	confined := append(writers,
		bashTool,
		searchTool,
		builtin.ConfineWebFetch(proxySpec))
	confined = append(confined, builtin.ConfineReaders(forbidReadRoots)...)
	for _, t := range confined {
		if _, ok := reg.Get(t.Name()); ok {
			reg.Add(t)
		}
	}
}

// partitionByTier splits configured plugin entries into eager (block boot until
// ready) and background (placeholder + start spawn now). Entries with an empty,
// legacy lazy, or unrecognised tier land in background.
func partitionByTier(entries []config.PluginEntry) (eager, bg []config.PluginEntry) {
	for _, e := range entries {
		switch e.ResolvedTier() {
		case "eager":
			eager = append(eager, e)
		default:
			bg = append(bg, e)
		}
	}
	return eager, bg
}

// PluginSpecs maps configured plugin entries to plugin.Spec, expanding ${VAR}
// references. Exported so custom assemblers can connect the config's plugins
// alongside their own (e.g. ACP's per-session MCP servers).
func PluginSpecs(entries []config.PluginEntry) []plugin.Spec {
	return PluginSpecsForRoot(entries, "")
}

// PluginSpecsForRoot maps configured plugin entries to plugin.Spec and applies
// workspace-aware compatibility overrides for known cwd-sensitive servers.
func PluginSpecsForRoot(entries []config.PluginEntry, workspaceRoot string) []plugin.Spec {
	return PluginSpecsForRootWithOptions(entries, workspaceRoot, PluginSpecOptions{})
}

// PluginSpecsForRootWithOptions maps configured plugin entries to plugin.Spec
// and injects runtime policy such as the global MCP call timeout.
func PluginSpecsForRootWithOptions(entries []config.PluginEntry, workspaceRoot string, opts PluginSpecOptions) []plugin.Spec {
	specs := make([]plugin.Spec, len(entries))
	for i, e := range entries {
		specs[i] = pluginSpecFromEntryWithOptions(e, workspaceRoot, opts)
	}
	return specs
}

func pluginSpecFromEntryWithOptions(e config.PluginEntry, workspaceRoot string, opts PluginSpecOptions) plugin.Spec {
	e = e.ExpandedPlugin()
	configSource := strings.TrimSpace(string(e.Source))
	if configSource == "" {
		configSource = opts.ConfigSource
	}
	spec := plugin.ApplyKnownOverrides(plugin.Spec{
		Name:                  e.Name,
		Package:               strings.TrimSpace(opts.PackageOwners[e.Name]),
		Type:                  e.Type,
		Command:               e.Command,
		Args:                  e.Args,
		Env:                   e.Env,
		URL:                   e.URL,
		Headers:               e.Headers,
		DefaultStartupTimeout: opts.DefaultStartupTimeout,
		StartupTimeout:        secondsDuration(e.StartupTimeoutSeconds),
		DefaultCallTimeout:    opts.DefaultCallTimeout,
		CallTimeout:           secondsDuration(e.CallTimeoutSeconds),
		ToolTimeouts:          toolTimeoutDurations(e.ToolTimeoutSeconds),
		WorkspaceRoot:         strings.TrimSpace(workspaceRoot),
		LaunchManager:         opts.LaunchManager,
		ConfigSource:          configSource,
		Authorized:            e.Source.UserAuthorized(),
		OAuthHTTPClient:       opts.OAuthHTTPClient,
	}, workspaceRoot)
	if e.Source.ProjectScoped() && strings.TrimSpace(spec.Dir) == "" {
		spec.Dir = workspaceRoot
	}
	applyMCPIsolation(&spec, workspaceRoot, opts)
	return spec
}

func pluginPackageOwners(cfg *config.Config) map[string]string {
	out := map[string]string{}
	if cfg == nil {
		return out
	}
	for _, configured := range cfg.Plugins {
		if owner, ok := cfg.PluginPackageOwner(configured.Name); ok {
			out[configured.Name] = owner
		}
	}
	return out
}

func skillMCPBindings(sk skill.Skill, reg *tool.Registry, specs []plugin.Spec, cachedTools map[string][]plugin.CachedTool, cacheKeyOK map[string]bool) []tool.MCPBinding {
	var out []tool.MCPBinding
	liveServers := map[string]bool{}
	if reg != nil {
		bindings := reg.MCPBindings()
		out = make([]tool.MCPBinding, 0, len(bindings))
		for _, binding := range bindings {
			liveServers[binding.Server] = true
			if binding.Package == sk.Plugin {
				out = append(out, binding)
			}
		}
	}

	for _, spec := range specs {
		if spec.Package != sk.Plugin || liveServers[spec.Name] || !cacheKeyOK[spec.Name] {
			continue
		}
		for _, cached := range cachedTools[spec.Name] {
			visible := cached.Name
			if spec.StripRawPrefix != "" {
				visible = strings.TrimPrefix(visible, spec.StripRawPrefix)
			}
			out = append(out, tool.MCPBinding{
				Package:      spec.Package,
				Server:       spec.Name,
				RawName:      cached.Name,
				VisibleName:  visible,
				CallableName: plugin.ModelToolName(spec.Name, visible),
				CapabilityID: "mcp-tool:" + spec.Name + "/" + cached.Name,
			})
		}
	}
	return out
}

func applyMCPIsolation(spec *plugin.Spec, workspaceRoot string, opts PluginSpecOptions) {
	if spec == nil {
		return
	}

	if spec.ProcessMode == "" {
		spec.ProcessMode = plugin.MCPProcessHost
	}
	if strings.TrimSpace(opts.StateHome) == "" {
		return
	}
	stateDir := plugin.MCPStateDir(opts.StateHome, workspaceRoot, spec.Name)
	spec.StateDir = stateDir
	if spec.ResolvedProcessMode() != plugin.MCPProcessConfined {

		return
	}
	writerRoots := appendUniquePaths([]string{stateDir}, opts.WriterRoots...)
	readerRoots := []string{workspaceRoot}
	if home, err := os.UserHomeDir(); err == nil {
		readerRoots = appendUniquePaths(readerRoots, home)
	}
	spec.Sandbox = sandbox.Spec{
		Mode: "enforce", WriteRoots: writerRoots,
		ReadRoots:              readerRoots,
		AppContainerWriteRoots: append([]string(nil), writerRoots...),
		ForbidReadRoots:        append([]string(nil), opts.ForbidReadRoots...),
		Network:                opts.Network, MinimalWrites: true,
	}
}

func secondsDuration(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func toolTimeoutDurations(seconds map[string]int) map[string]time.Duration {
	if len(seconds) == 0 {
		return nil
	}
	out := make(map[string]time.Duration, len(seconds))
	for name, sec := range seconds {
		name = strings.TrimSpace(name)
		if name == "" || sec <= 0 {
			continue
		}
		out[name] = time.Duration(sec) * time.Second
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func applyKnownPluginOverrides(specs []plugin.Spec, workspaceRoot string) []plugin.Spec {
	out := make([]plugin.Spec, len(specs))
	for i, spec := range specs {
		out[i] = plugin.ApplyKnownOverrides(spec, workspaceRoot)
	}
	return out
}

func applyDefaultMCPCallTimeout(specs []plugin.Spec, timeout time.Duration) []plugin.Spec {
	if len(specs) == 0 || timeout <= 0 {
		return specs
	}
	out := make([]plugin.Spec, len(specs))
	for i, spec := range specs {
		out[i] = spec
		if out[i].DefaultCallTimeout <= 0 {
			out[i].DefaultCallTimeout = timeout
		}
	}
	return out
}

func applyDefaultMCPStartupTimeout(specs []plugin.Spec, timeout time.Duration) []plugin.Spec {
	if len(specs) == 0 || timeout <= 0 {
		return specs
	}
	out := make([]plugin.Spec, len(specs))
	for i, spec := range specs {
		out[i] = spec
		if out[i].DefaultStartupTimeout <= 0 {
			out[i].DefaultStartupTimeout = timeout
		}
	}
	return out
}

// autoShellPrefer reports whether [tools.shell] left the interpreter to
// auto-detection, so the "fell back to PowerShell" hint is suppressed once the
// user has explicitly chosen a shell.
func autoShellPrefer(prefer string) bool {
	p := strings.ToLower(strings.TrimSpace(prefer))
	return p == "" || p == "auto"
}

// MCPStartupNotice formats the warning shown when configured MCP servers failed
// to connect, naming the first few; ok is false when none failed.
func MCPStartupNotice(failures []plugin.Failure) (text, detail string, ok bool) {
	if len(failures) == 0 {
		return "", "", false
	}
	names := make([]string, 0, min(len(failures), 3))
	details := make([]string, 0, len(failures))
	for i, f := range failures {
		if i >= 3 {
			continue
		}
		names = append(names, f.Name)
	}
	for _, f := range failures {
		line := f.Name
		if strings.TrimSpace(f.Error) != "" {
			line += ": " + strings.TrimSpace(f.Error)
		}
		details = append(details, line)
	}
	more := ""
	if len(failures) > len(names) {
		more = fmt.Sprintf(" (+%d more)", len(failures)-len(names))
	}
	return "Some MCP servers failed to start; run /mcp for details.", fmt.Sprintf("%d MCP server(s) failed to start: %s%s\n%s",
		len(failures), strings.Join(names, ", "), more, strings.Join(details, "\n")), true
}

// LSPSpecs returns the language → server map: the built-in defaults overlaid with
// any user overrides. A user entry may set only the fields it wants to change;
// empty fields keep the default for that language.
func LSPSpecs(cfg config.LSPConfig) map[string]lsp.ServerSpec {
	specs := lsp.DefaultSpecs()
	for lang, s := range cfg.Servers {
		spec := specs[lang]
		if s.Command != "" {
			spec.Command = s.Command
		}
		if s.Args != nil {
			spec.Args = s.Args
		}
		if s.Env != nil {
			spec.Env = s.Env
		}
		if s.LanguageID != "" {
			spec.LanguageID = s.LanguageID
		}
		if s.Extensions != nil {
			spec.Extensions = s.Extensions
		}
		if s.InstallHint != "" {
			spec.InstallHint = s.InstallHint
		}
		if spec.LanguageID == "" {
			spec.LanguageID = lang
		}
		specs[lang] = spec
	}
	return specs
}

func providerNames(cfg *config.Config) string {
	names := make([]string, len(cfg.Providers))
	for i, p := range cfg.Providers {
		names[i] = p.Name
	}
	return strings.Join(names, "/")
}
