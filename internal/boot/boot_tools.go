package boot

import (
	"context"
	"fmt"
	"io"
	"reasonix/internal/ablation"
	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/instruction"
	"reasonix/internal/lsp"
	"reasonix/internal/mcplaunch"
	"reasonix/internal/permission"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/sandbox"
	"reasonix/internal/sessiontemp"
	"reasonix/internal/skill"
	"reasonix/internal/tool"
	"reasonix/internal/tool/builtin"
	"strings"
	"time"
)

// buildTools wires the tool layer: built-ins, MCP/plugin specs, LSP, the
// permission policy and headless gate, and the sub-agent task tool.
// It also persists the composed + skill-indexed prompt so buildAssemble
// freezes the enhanced version (boot-split regression: without it the
// memory and implicit-skill-index enhancements are dropped).
func buildTools(ctx context.Context, bc *bootContext, opts Options) error {
	sink := bc.sink
	cfg := bc.cfg
	root := bc.root
	stderr := bc.stderr
	sysPrompt := bc.sysPrompt
	runtimeProfile := bc.runtimeProfile
	fileWriteReceipt := bc.fileWriteReceipt
	proxySpec := bc.proxySpec
	additionalDirs := bc.additionalDirs
	balanceClient := bc.balanceClient
	shell := bc.shell
	jm := bc.jm
	sessionDir := bc.sessionDir
	entry := bc.entry
	effectiveResolver := bc.effectiveResolver
	modelName := bc.modelName
	execProv := bc.execProv
	keepPolicy := bc.keepPolicy
	tokenDelivery := bc.tokenDelivery
	workspaceLease := bc.workspaceLease
	// durable, cache-stable prefix every turn reuses, so memory costs nothing per
	// turn. Mid-session changes never touch this prefix — they ride the
	// controller's transient turn-injection and fold in on the next session.
	resolved := instruction.Resolve(instruction.ResolveOptions{TargetDir: root, UserDir: config.MemoryUserDir()})
	projectChecks := instruction.ExtractHostChecks(resolved.Documents)
	instrBlock := instruction.Block(resolved.Documents)
	if instrBlock != "" {
		sysPrompt = strings.TrimRight(sysPrompt, "\n") + "\n\n" + instrBlock
	}

	implicitSkillInvocation := cfg.ImplicitSkillInvocationEnabled()
	// Skills: rediscovery skipped on no-op/interceptor/UI rebuilds when
	// ReuseAssembly is retained from the previous BuildResult.
	var skillStore *skill.Store
	var skills []skill.Skill
	var allSkillStore *skill.Store
	var allSkills []skill.Skill
	canReuseSkills := opts.ReuseAssembly != nil && shouldReuseDiscovery(opts.PreviousPlan) &&
		opts.ReuseAssembly.ImplicitSkillInvocation == implicitSkillInvocation
	if canReuseSkills {
		skills = opts.ReuseAssembly.Skills
		allSkills = skills
		skillStore = skill.New(skill.Options{ProjectRoot: root, Stderr: io.Discard})
		allSkillStore = skillStore
		if s := strings.TrimSpace(opts.ReuseAssembly.SystemPrompt); s != "" {
			sysPrompt = s
		}
	} else {
		skillStore = skill.New(skill.Options{
			ProjectRoot: root, CustomPaths: cfg.SkillCustomPaths(), PluginPaths: cfg.PluginPackageSkillOwners(),
			PluginAgentPaths: cfg.PluginPackageAgentOwners(), ExcludedPaths: cfg.SkillExcludedPaths(),
			DisabledNames: cfg.DisabledSkillNames(), MaxDepth: cfg.SkillMaxDepth(), Stderr: opts.Stderr,
		})
		skillStore.ConfigureInvocationPolicy(string(runtimeProfile), nil)
		skills = skillStore.List()
		allSkillStore = skill.New(skill.Options{ProjectRoot: root, CustomPaths: cfg.SkillCustomPaths(), PluginPaths: cfg.PluginPackageSkillOwners(), PluginAgentPaths: cfg.PluginPackageAgentOwners(), ExcludedPaths: cfg.SkillExcludedPaths(), MaxDepth: cfg.SkillMaxDepth(), Stderr: io.Discard})
		allSkills = allSkillStore.List()
		if implicitSkillInvocation {
			sysPrompt = skill.ApplyIndex(sysPrompt, skills)
		}
	}

	reg := tool.NewRegistry()
	writeRoots := cfg.WriteRootsForRoot(root)
	writeRoots = appendUniquePaths(writeRoots, additionalDirs...)
	if opts.WorkspaceOnly {
		writeRoots = []string{root}
	}
	networkEnabled := cfg.Sandbox.Network
	if opts.SandboxNetworkOverride != nil {
		networkEnabled = *opts.SandboxNetworkOverride
	}
	bashMode := cfg.BashMode()
	if override := strings.TrimSpace(opts.SandboxBashOverride); override != "" {
		bashMode = override
	}
	forbidReadRoots := RuntimeForbidReadRoots(cfg, root)
	// managedConfig names the Reasonix-owned config FILES the file-writers may
	// repair outside the workspace after per-write human approval; the bash
	// OS-sandbox write roots deliberately stay unwidened.
	managedConfig := builtin.NewManagedConfigPaths(config.ReasonixManagedConfigPaths())
	bashSpec := sandbox.Spec{Mode: bashMode, WriteRoots: writeRoots, ForbidReadRoots: forbidReadRoots, Network: networkEnabled}
	bashSpec.Shell = shell
	// The session-data guard blocks agent writes into Reasonix's own session
	// stores (they race the app's saves and surface as conflict-copy loops);
	// explicit allow_write entries stay a sanctioned escape hatch.
	allowWriteRoots := cfg.AllowWriteRoots()
	if opts.WorkspaceOnly {
		allowWriteRoots = nil
	}
	sessionGuard := builtin.NewSessionDataGuard(config.MemoryUserDir(), allowWriteRoots)
	if bashSpec.Mode == "enforce" && !sandbox.Available() {
		fmt.Fprintln(stderr, "warning: "+sandbox.UnavailableMessage())
	}
	if autoShellPrefer(cfg.Tools.Shell.Prefer) && shell.Kind == sandbox.ShellPowerShell {
		fmt.Fprintln(stderr, "warning: bash not found on PATH; the shell tool will run commands under Windows PowerShell. Install Git for Windows or WSL to use bash, or set [tools.shell] prefer=\"powershell\" to silence this.")
	}
	searchSpec := builtin.ResolveSearch(cfg.Tools.Search.Engine, cfg.Tools.Search.RgPath, stderr)
	bashTimeout := time.Duration(cfg.BashTimeoutSeconds()) * time.Second
	enabledBuiltins := cfg.Tools.Enabled
	readPathResolver := builtin.NewPathResolver()
	// Session-private temporary directory manager for Bash/grep. Rebuild
	// reuses the previous Controller's Manager; a fresh build creates one
	// here so tools and the Controller share the same instance from boot.
	sessionTemp := opts.SessionTemp
	if sessionTemp == nil {
		sessionTemp = sessiontemp.New()
	}
	// Register the full built-in inventory for use_capability dispatch. The
	// provider-visible surface is narrowed later via SetProviderVisibleTools.
	addBuiltins(reg, enabledBuiltins, writeRoots, bashSpec, bashTimeout, searchSpec, stderr, root, proxySpec, forbidReadRoots, readPathResolver, sessionGuard, managedConfig, opts.FileOverlay, opts.TerminalRunner, sessionTemp, fileWriteReceipt)
	// Use the caller-supplied shared host when set, so controllers for the same
	// workspace root reuse running MCP processes (e.g. one CodeGraph daemon
	// instead of one per tab). Otherwise construct a private host per controller.
	pluginHost := opts.SharedHost
	if pluginHost == nil {
		pluginHost = plugin.NewHost()
	}

	// Enabled MCP servers enter the catalog at boot: cached schemas register
	// placeholders without starting processes, cache-miss servers get one
	// background discovery, and the first real call uses EnsureConnected.
	pluginSpecOptions := PluginSpecOptions{
		DefaultStartupTimeout: time.Duration(cfg.MCPStartupTimeoutSeconds()) * time.Second,
		DefaultCallTimeout:    time.Duration(cfg.MCPCallTimeoutSeconds()) * time.Second,
		LaunchManager:         mcplaunch.ForWorkspace(config.ReasonixHomeDir(), root),
		ConfigSource:          "workspace_config",
		StateHome:             config.ReasonixHomeDir(),
		WriterRoots:           writeRoots,
		ForbidReadRoots:       forbidReadRoots,
		Network:               networkEnabled,
		PackageOwners:         pluginPackageOwners(cfg),
		OAuthHTTPClient:       balanceClient,
	}
	autoStartEntries := cfg.EnabledPlugins(root, config.DefaultMCPActivationStore())
	enabledMCPNames := make(map[string]bool, len(autoStartEntries))
	for _, enabled := range autoStartEntries {
		if name := strings.TrimSpace(enabled.Name); name != "" {
			enabledMCPNames[name] = true
		}
	}
	// Legacy eager/background tiers are still parsed for config compatibility
	// but no longer change process start timing. Keep the partition only so
	// demotion notices remain meaningful for chronically slow eager configs.
	eagerEntries, bgEntries := partitionByTier(autoStartEntries)
	extraSpecs := applyDefaultMCPStartupTimeout(
		applyDefaultMCPCallTimeout(
			applyKnownPluginOverrides(opts.ExtraPlugins, root),
			pluginSpecOptions.DefaultCallTimeout,
		),
		pluginSpecOptions.DefaultStartupTimeout,
	)
	for i := range extraSpecs {
		if strings.TrimSpace(extraSpecs[i].WorkspaceRoot) == "" {
			extraSpecs[i].WorkspaceRoot = root
		}
		if extraSpecs[i].LaunchManager == nil {
			extraSpecs[i].LaunchManager = pluginSpecOptions.LaunchManager
		}
		if strings.TrimSpace(extraSpecs[i].ConfigSource) == "" {
			extraSpecs[i].ConfigSource = "host_session"
		}
		if !extraSpecs[i].RequireLaunchApproval {
			// Session-scoped MCP specs arrive through an explicit host/user action
			// (for example ACP session/new), so they follow installed-server
			// authorization without another per-tool or per-session prompt.
			extraSpecs[i].Authorized = true
		}
		applyMCPIsolation(&extraSpecs[i], root, pluginSpecOptions)
	}
	// Auto-demote: any eager plugin that has been chronically slow (recent
	// samples repeatedly hit the blocking startup budget) drops to background
	// for this session. The user keeps eager intent, just doesn't pay for it
	// on a server that's been misbehaving. A notice surfaces the demotion.
	var demoteMessages []string
	budget := plugin.DefaultStartupBudget()
	kept := eagerEntries[:0]
	for _, e := range eagerEntries {
		rec := plugin.Recommend(e.Name, budget, 0)
		if rec.Demote {
			demoteMessages = append(demoteMessages, rec.Reason)
			bgEntries = append(bgEntries, e)
			continue
		}
		kept = append(kept, e)
	}
	eagerEntries = kept

	eagerSpecs := PluginSpecsForRootWithOptions(eagerEntries, root, pluginSpecOptions)
	bgSpecs := PluginSpecsForRootWithOptions(bgEntries, root, pluginSpecOptions)

	eagerSpecs = append(eagerSpecs, extraSpecs...)

	// Apply caller-supplied stderr override to every spec across tiers.
	if opts.Stderr != nil {
		for i := range eagerSpecs {
			eagerSpecs[i].Stderr = opts.Stderr
		}
		for i := range bgSpecs {
			bgSpecs[i].Stderr = opts.Stderr
		}
	}

	// Host-session ExtraPlugins (e.g. ACP session servers) are explicit for
	// this controller and take a short readiness probe so recovery and
	// session-scoped servers are deterministic; config MCP stays catalog-first
	// until first real use.
	if len(extraSpecs) > 0 {
		for _, s := range extraSpecs {
			if pluginHost.HasClient(s.Name) {
				if tools, err := pluginHost.ToolsFor(ctx, s.Name); err == nil {
					for _, t := range tools {
						reg.Add(t)
					}
					continue
				}
			}
			addCtx, addCancel := context.WithTimeout(ctx, 5*time.Second)
			tools, err := pluginHost.EnsureConnectedWithLifecycle(ctx, addCtx, s, 0)
			addCancel()
			if err != nil {
				if plugin.IsServerAlreadyConnected(err) {
					if tools, err2 := pluginHost.ToolsFor(ctx, s.Name); err2 == nil {
						for _, t := range tools {
							reg.Add(t)
						}
						continue
					}
				}
				// Leave a catalog entry for diagnostics; failures surface in /mcp.
				cs, _ := plugin.LoadCachedSchemaForSpec(s)
				for _, t := range plugin.LazyToolset(s, cs, pluginHost, reg, ctx, false) {
					reg.Add(t)
				}
				sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn,
					Text: "An MCP server failed to start.", Detail: fmt.Sprintf("mcp %s: %v", s.Name, err)})
				continue
			}
			for _, t := range tools {
				reg.Add(t)
			}
		}
	}

	// Configured enabled MCP: cache-hit placeholders without starting processes;
	// cache-miss servers get one background catalog discovery.
	registerEnabledMCP := func(specs []plugin.Spec) {
		for _, s := range specs {
			if pluginHost.HasClient(s.Name) {
				tools, err := pluginHost.ToolsFor(ctx, s.Name)
				if err == nil {
					for _, t := range tools {
						reg.Add(t)
					}
					continue
				}
			}
			cs, _ := plugin.LoadCachedSchemaForSpec(s)
			// Only kick a process for catalog discovery when no usable schema is
			// cached. Cache-hit sessions stay process-idle until first tool call.
			kick := cs == nil || len(cs.Tools) == 0
			for _, t := range plugin.LazyToolset(s, cs, pluginHost, reg, ctx, kick) {
				reg.Add(t)
			}
		}
	}
	// eagerSpecs already includes extraSpecs; avoid double
	// registration of host-session servers that connected above.
	configSpecs := append(append([]plugin.Spec{}, eagerSpecs...), bgSpecs...)
	if len(extraSpecs) > 0 {
		extraNames := map[string]bool{}
		for _, s := range extraSpecs {
			extraNames[s.Name] = true
		}
		filtered := configSpecs[:0]
		for _, s := range configSpecs {
			if extraNames[s.Name] {
				continue
			}
			filtered = append(filtered, s)
		}
		configSpecs = filtered
	}
	registerEnabledMCP(configSpecs)

	for _, msg := range demoteMessages {
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: msg})
	}

	cleanup := pluginHost.Close
	if opts.SharedHost != nil {
		// The caller owns the shared host's lifecycle; the controller must not
		// close it. A no-op cleanup keeps Controller.Close happy without
		// shutting down MCP processes that other controllers still use.
		cleanup = func() {}
	}

	// addTools registers tools on reg and returns the names that were added.
	addTools := func(reg *tool.Registry, tools []tool.Tool) []string {
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			if t == nil {
				continue
			}
			reg.Add(t)
			names = append(names, t.Name())
		}
		return names
	}

	// LSP tools resolve their servers on PATH and spawn lazily on first query, so
	// registering them is cheap even when no server is installed (a query then
	// returns an install hint). The manager is session-scoped; chain its shutdown
	// into the controller's cleanup so servers stop with the session, not the turn.
	var lspMgr *lsp.Manager
	lspToolsAdded := false
	addLSPTools := func() []string {
		if lspMgr == nil || lspToolsAdded {
			return nil
		}
		lspToolsAdded = true
		return addTools(reg, lsp.Tools(lspMgr))
	}
	if cfg.LSP.Enabled {
		lspMgr = lsp.NewManager(root, LSPSpecs(cfg.LSP))
		addLSPTools()
		prev := cleanup
		cleanup = func() { prev(); lspMgr.Close() }
	}

	maxSteps := max(opts.MaxSteps, 0)
	subagentStore, err := newSubagentStore(sessionDir, opts.SubagentParentLive)
	if err != nil {
		return err
	}
	if subagentStore != nil {
		subagentStore.WithDestroyedChecker(jm.IsDestroying)
	}

	// Permission policy gates every tool call: a real headless caller supplies
	// a mode (Ask fails closed, Auto allows writer fallbacks, DontAsk denies);
	// sub-agents always run headless, so they cannot be a weaker path around
	// the parent gate.
	policy := permission.New(cfg.Permissions.Mode, cfg.Permissions.Allow, cfg.Permissions.Ask, cfg.Permissions.Deny).
		WithAllowDynamicBashFallback(cfg.Permissions.AllowDynamicBash).
		WithSessionAllow(opts.PermissionAllow)
	headlessGate := control.NewSharedHeadlessGate(policy, opts.HeadlessApprovalMode)

	// The `task` tool spawns sub-agents reusing the parent's provider and tool
	// registry; wired here after built-ins/plugins load so sub-agents inherit
	// the full tool set (minus `task` itself).
	resolveSubagentProvider := func(modelRef, effort string) (provider.Provider, *provider.Pricing, int, error) {
		me := *entry
		selectedRef := modelRefFromEntry(entry)
		if strings.TrimSpace(modelRef) != "" {
			if resolved, ok := cfg.ResolveModel(modelRef); ok {
				me = *resolved
				selectedRef = modelRefFromEntry(resolved)
			} else if effectiveResolver != nil {
				me = *syntheticEntryFromResolver(effectiveResolver, modelRef)
				selectedRef = modelRef
			} else {
				return nil, nil, 0, fmt.Errorf("unknown model %q", modelRef)
			}
		}
		var effortOverride *string
		if strings.TrimSpace(effort) != "" {
			normalized, err := config.NormalizeEffort(&me, effort)
			if err != nil {
				if effectiveResolver == nil {
					return nil, nil, 0, err
				}
				normalized = effort
			}
			me.Effort = normalized
			effortOverride = &normalized
			if me.Kind == "anthropic" && strings.TrimSpace(me.Effort) != "" && strings.TrimSpace(me.Thinking) == "" {
				me.Thinking = "adaptive"
			}
		}
		p, err := resolveProvider(effectiveResolver, cfg, proxySpec, provider.Selection{Ref: selectedRef, Effort: effortOverride})
		if err != nil {
			return nil, nil, 0, err
		}
		return p, me.Price, me.ContextWindow, nil
	}
	subagentIdentity := func(modelRef, effort string) (string, string) {
		return subagentEffectiveIdentity(cfg, opts.ProviderResolver, modelName, entry, modelRef, effort)
	}
	taskModel := firstNonEmpty(cfg.Agent.SubagentModels["task"], cfg.Agent.SubagentModel)
	taskEffort := firstNonEmpty(cfg.Agent.SubagentEfforts["task"], cfg.Agent.SubagentEffort)
	maxSubagentDepth := agent.NormalizeMaxSubagentDepth(cfg.Agent.MaxSubagentDepth)
	maxSubagentConcurrency, maxParallelWriters := agent.NormalizeConcurrencyLimits(
		cfg.Agent.MaxSubagentConcurrency, cfg.Agent.MaxParallelWriters,
	)
	subagentScheduler := agent.NewSubagentScheduler(maxSubagentConcurrency, maxParallelWriters)
	profileLookup := func(name string) (agent.ProfileDefinition, bool) {
		sk, ok := skillStore.Read(name)
		if !ok || sk.RunAs != skill.RunSubagent {
			return agent.ProfileDefinition{}, false
		}
		return agent.ProfileFromSkill(skillStore.Prepare(sk)), true
	}
	profileConfigModel := func(profile string) string {
		for _, key := range SubagentModelKeys(profile) {
			if m := strings.TrimSpace(cfg.Agent.SubagentModels[key]); m != "" {
				return m
			}
		}
		return ""
	}
	profileConfigEffort := func(profile string) string {
		for _, key := range SubagentModelKeys(profile) {
			if e := strings.TrimSpace(cfg.Agent.SubagentEfforts[key]); e != "" {
				return e
			}
		}
		return ""
	}
	bashSandboxEnforced := bashSpec.Enforce
	taskToolAdded := false
	readOnlyTaskToolAdded := false
	var taskTool *agent.TaskTool
	// capRuntime is assigned after MCP specs load; closures capture the variable
	// so task tools created later still receive the session-shared substrate.
	var capRuntime *agent.MCPCapabilityRuntime
	newTaskTool := func() *agent.TaskTool {
		return agent.NewTaskToolWithOptions(agent.TaskToolOptions{
			Provider:            execProv,
			Pricing:             entry.Price,
			ParentRegistry:      reg,
			MaxSteps:            maxSteps,
			ContextWindow:       entry.ContextWindow,
			RecentKeep:          cfg.Agent.RecentKeep,
			SoftCompactRatio:    cfg.Agent.SoftCompactRatio,
			ToolResultSnipRatio: cfg.Agent.ToolResultSnipRatio,
			CompactRatio:        cfg.Agent.CompactRatio,
			CompactForceRatio:   cfg.Agent.CompactForceRatio,
			ContextEditing:      cfg.Agent.ContextEditing,
			Temperature:         cfg.Agent.Temperature,
			ArchiveDir:          config.ArchiveDir(),
			SysPrompt:           "",
			Gate:                headlessGate,
			KeepPolicy:          keepPolicy,
			SubagentModel:       taskModel,
			SubagentEffort:      taskEffort,
			ResolveProvider:     resolveSubagentProvider,
		}).
			WithTranscripts(subagentStore, root, modelName, entry.Effort).
			WithTranscriptIdentityResolver(subagentIdentity).
			WithMaxSubagentDepth(maxSubagentDepth).
			WithDeliveryProfile(tokenDelivery).
			WithAblation(opts.Ablation).
			WithWorkspaceLease(workspaceLease).
			WithScheduler(subagentScheduler).
			WithProfileLookup(profileLookup).
			WithProfileConfigResolvers(profileConfigModel, profileConfigEffort).
			WithBashSandboxEnforced(bashSandboxEnforced).
			WithCapabilityRuntime(capRuntime)
	}
	addTaskTool := func() string {
		if opts.Ablation.Off(ablation.Subagent) {
			return "task tool is disabled for this run."
		}
		if taskToolAdded {
			return "task tool is already enabled."
		}
		taskToolAdded = true
		if taskTool == nil {
			taskTool = newTaskTool()
		}
		// The registry exports schemas in stable name order. Keep this surface
		// static: profile names and result refs never enter provider-visible
		// schemas, and the result reader does not change between turns.
		reg.Add(taskTool)
		reg.Add(agent.NewParallelTasksTool(taskTool, reg))
		reg.Add(agent.NewFleetTool(taskTool))
		reg.Add(agent.NewSubagentResultTool(taskTool))
		return "enabled task."
	}
	addReadOnlyTaskTool := func() string {
		if opts.Ablation.Off(ablation.Subagent) {
			return "read_only_task tool is disabled for this run."
		}
		if readOnlyTaskToolAdded {
			return "read_only_task tool is already enabled."
		}
		readOnlyTaskToolAdded = true
		if taskTool == nil {
			taskTool = newTaskTool()
		}
		reg.Add(agent.NewReadOnlyTaskTool(taskTool))
		return "enabled read_only_task."
	}
	addTaskTool()
	addReadOnlyTaskTool()

	bc.projectChecks = projectChecks
	bc.sysPrompt = sysPrompt
	bc.skillStore = skillStore
	bc.skills = skills
	bc.allSkillStore = allSkillStore
	bc.allSkills = allSkills
	bc.implicitSkillInvocation = implicitSkillInvocation
	bc.reg = reg
	bc.pluginHost = pluginHost
	bc.pluginSpecOptions = pluginSpecOptions
	bc.extraSpecs = extraSpecs
	bc.enabledMCPNames = enabledMCPNames
	bc.lspMgr = lspMgr
	bc.maxSteps = maxSteps
	bc.maxSubagentDepth = maxSubagentDepth
	bc.policy = policy
	bc.headlessGate = headlessGate
	bc.resolveSubagentProvider = resolveSubagentProvider
	bc.subagentIdentity = subagentIdentity
	bc.subagentScheduler = subagentScheduler
	bc.taskTool = taskTool
	bc.capRuntime = capRuntime
	bc.configSpecs = configSpecs
	bc.cleanup = cleanup
	bc.readPathResolver = readPathResolver
	bc.sessionTemp = sessionTemp
	bc.subagentStore = subagentStore
	return nil
}
