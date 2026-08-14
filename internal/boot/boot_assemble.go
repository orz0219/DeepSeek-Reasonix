package boot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reasonix/internal/agent"
	"reasonix/internal/capability"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/extension"
	"reasonix/internal/extension/dispatch"
	"reasonix/internal/extension/protocol"
	"reasonix/internal/extension/providerext"
	"reasonix/internal/extension/sidecar"
	"reasonix/internal/goaleval"
	"reasonix/internal/guardian"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/recovery"
	"strings"
)

// buildAssemble finishes the build: the executor, the optional two-model
// coordinator, guardian/recovery/goal hooks, the capability router, and the
// frozen extension kernel snapshot. It returns the final BuildResult.
func buildAssemble(ctx context.Context, bc *bootContext, opts Options) (*BuildResult, error) {
	sink := bc.sink
	cfg := bc.cfg
	root := bc.root
	sysPrompt := bc.sysPrompt
	runtimeProfile := bc.runtimeProfile
	keepPolicy := bc.keepPolicy
	entry := bc.entry
	modelRef := bc.modelRef
	agentPreset := bc.agentPreset
	jm := bc.jm
	mem := bc.mem
	projectChecks := bc.projectChecks
	implicitSkillInvocation := bc.implicitSkillInvocation
	reg := bc.reg
	pluginHost := bc.pluginHost
	execProv := bc.execProv
	balanceClient := bc.balanceClient
	shell := bc.shell
	sessionDir := bc.sessionDir
	workspaceLease := bc.workspaceLease
	proxySpec := bc.proxySpec
	baseResolver := bc.baseResolver
	effectiveResolver := bc.effectiveResolver
	extensionResolver := bc.extensionResolver
	extensionMgr := bc.extensionMgr
	extraSpecs := bc.extraSpecs
	enabledMCPNames := bc.enabledMCPNames
	tokenDelivery := bc.tokenDelivery
	sessionTemp := bc.sessionTemp
	readPathResolver := bc.readPathResolver
	owner := bc.owner
	generation := bc.generation
	sessionID := bc.sessionID
	ctrlRef := bc.ctrlRef
	controllerReady := bc.controllerReady
	extUIHub := bc.extUIHub
	skills := bc.skills
	skillStore := bc.skillStore
	allSkills := bc.allSkills
	allSkillStore := bc.allSkillStore
	headlessGate := bc.headlessGate
	policy := bc.policy
	maxSteps := bc.maxSteps
	maxSubagentDepth := bc.maxSubagentDepth
	resolvedHooks := bc.resolvedHooks
	hookRunner := bc.hookRunner
	extWarn := bc.extWarn
	taskTool := bc.taskTool
	capRuntime := bc.capRuntime
	resolveSubagentProvider := bc.resolveSubagentProvider
	subagentIdentity := bc.subagentIdentity
	subagentScheduler := bc.subagentScheduler
	lspMgr := bc.lspMgr
	configSpecs := bc.configSpecs
	cleanup := bc.cleanup
	capLedger := bc.capLedger
	capAudit := bc.capAudit
	capSpecs := bc.capSpecs
	cmds := bc.cmds
	skillRunner := bc.skillRunner
	skillProfile := bc.skillProfile
	readOnlySkillRunner := bc.readOnlySkillRunner
	cachedTools := bc.cachedTools
	cacheKeyOK := bc.cacheKeyOK
	pluginSpecOptions := bc.pluginSpecOptions
	// Detect dual-model planner early so Balanced/Delivery can attach the same
	// stable use_capability surface to both Planner and Executor.
	profile := runtimeProfile
	var capProxy *agent.UseCapabilityTool
	// Catalog closes over capRuntime so proxy-connected tools stay routable.
	// Use AllContractEntries so tool: capabilities include non-provider-visible
	// tools that use_capability can still dispatch.
	catalogFn := func() capability.Catalog {
		conn := map[string]bool{}
		failedNow := map[string]string{}
		if pluginHost != nil {
			for _, n := range pluginHost.ServerNames() {
				conn[n] = true
			}
			for _, failure := range pluginHost.Failures() {
				failedNow[failure.Name] = failure.Error
			}
		}
		catOpts := capability.CatalogOptions{
			Tools:       reg.AllContractEntries(),
			Skills:      skillStore.List(),
			Plugins:     cfg.Plugins,
			Profile:     profile,
			Connected:   conn,
			Failed:      failedNow,
			CachedTools: cachedTools,
			CacheKeyOK:  cacheKeyOK,
		}
		if capRuntime != nil {
			catOpts.Plugins, catOpts.CachedTools, catOpts.CacheKeyOK, catOpts.Disabled, catOpts.ProxyTools = capRuntime.CapabilityCatalogState()
		}
		return capability.BuildCatalog(catOpts)
	}
	// Always build the capability runtime and provider-visible use_capability
	// proxy so all three role settings share one tool schema.
	capRuntime = agent.NewMCPCapabilityRuntime(ctx, pluginHost, capSpecs, reg, catalogFn)
	capRuntime.ConfigureServers(cfg.Plugins, capSpecs, enabledMCPNames)
	capLedger = capability.NewLedger()
	capAudit = &capability.Audit{}
	capProxy = capRuntime.NewFrontend(capLedger, capAudit)
	reg.Add(capProxy)
	skillStore.ConfigureInvocationPolicy(string(runtimeProfile), func(requires []string) []string {
		connected := map[string]bool{}
		failedNow := map[string]string{}
		if pluginHost != nil {
			for _, name := range pluginHost.ServerNames() {
				connected[name] = true
			}
			for _, failure := range pluginHost.Failures() {
				failedNow[failure.Name] = failure.Error
			}
		}
		catOpts := capability.CatalogOptions{
			Tools:       reg.AllContractEntries(),
			Skills:      skillStore.List(),
			Plugins:     cfg.Plugins,
			Profile:     runtimeProfile,
			Connected:   connected,
			Failed:      failedNow,
			CachedTools: cachedTools,
			CacheKeyOK:  cacheKeyOK,
		}
		if capRuntime != nil {
			catOpts.Plugins, catOpts.CachedTools, catOpts.CacheKeyOK, catOpts.Disabled, catOpts.ProxyTools = capRuntime.CapabilityCatalogState()
		}
		catalog := capability.BuildCatalog(catOpts)
		_, missing := catalog.RequiresReady(requires)
		return missing
	})

	execSess := newObservedSession(sysPrompt)
	executor := agent.New(execProv, reg, execSess, agent.Options{
		MaxSteps:    maxSteps,
		MaxStepsKey: opts.MaxStepsKey,
		Temperature: cfg.Agent.Temperature,
		TaskBudget:  taskBudgetFromConfig(cfg),
		Pricing:     entry.Price,
		ModelRef:    modelRef,
		Gate:        headlessGate,
		Hooks:       hookRunner,
		Jobs:        jm,
		// Parent write reservation at the executor entry covers all writers
		// (including late Economy/MCP adds) without wrapping tool schemas.
		WriteScheduler:               subagentScheduler,
		WriteWorkspaceRoot:           root,
		ProjectChecks:                projectChecks,
		AgentPreset:                  agentPreset,
		DeliveryProfile:              tokenDelivery,
		Ablation:                     opts.Ablation,
		WorkspaceLease:               workspaceLease,
		CapabilityLedger:             capLedger,
		CapabilityAudit:              capAudit,
		ContextWindow:                entry.ContextWindow,
		SoftCompactRatio:             cfg.Agent.SoftCompactRatio,
		ToolResultSnipRatio:          cfg.Agent.ToolResultSnipRatio,
		CompactRatio:                 cfg.Agent.CompactRatio,
		CompactForceRatio:            cfg.Agent.CompactForceRatio,
		ContextEditing:               cfg.Agent.ContextEditing,
		RecentKeep:                   cfg.Agent.RecentKeep,
		ArchiveDir:                   config.ArchiveDir(),
		KeepPolicy:                   keepPolicy,
		ReasoningLanguage:            cfg.ReasoningLanguage(),
		PlanModeReadOnlyCommands:     cfg.Agent.PlanModeReadOnlyCommands,
		SubagentDepth:                0,
		MaxSubagentDepth:             maxSubagentDepth,
		MissingReasoningWarnStateDir: config.MissingReasoningWarnStateDir(),
	}, sink)

	var runner agent.Runner = executor
	label := entry.Model
	// Two-model collaboration: a distinct planner_model wraps the executor in a
	// Coordinator with its own session, kept separate for cache stability. The
	// planner gets the same standing memory context and a filtered read-only
	// research tool set, so it can inspect rules/code without side effects.
	pm := effectivePlannerModel(cfg, opts)
	pe, plannerResolved := resolveOptionalEntry(effectiveResolver, cfg, pm)
	if pm != "" && !plannerResolved {
		// An unusable optional planner must not take the session down with it —
		// the executor is what the user talks to. Degrades like the guardian
		// model below (#4615).
		slog.Warn("planner model is not a configured provider — planning disabled", "model", pm)
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn,
			Text: fmt.Sprintf("planner_model %q is not a configured provider — continuing with the executor alone", pm)})
	}
	if pm != "" && plannerResolved {
		if pe.Model != entry.Model {
			plannerProv, err := resolveProvider(effectiveResolver, cfg, proxySpec, provider.Selection{Ref: modelRefFromEntry(pe)})
			if err != nil {
				return nil, fmt.Errorf("planner %q: %w", pm, err)
			}
			plannerSess := agent.NewSession(agent.PlannerPromptWithContext(mem.Block()))
			// Planner owns an independent ledger/audit and use_capability frontend
			// so its MCP calls cannot satisfy or poison Executor Delivery gates.
			plannerLedger := capability.NewLedger()
			plannerAudit := &capability.Audit{}
			plannerTools := agent.PlannerToolRegistry(reg)
			if capRuntime != nil {
				// Replace any cloned parent frontend with one bound to the
				// planner ledger (PlannerToolRegistry clones with nil ledger).
				if _, ok := plannerTools.Get("use_capability"); ok {
					plannerTools.RemovePrefix("use_capability")
				}
				plannerTools.Add(capRuntime.NewFrontend(plannerLedger, plannerAudit))
			}
			plannerOpts := agent.Options{
				MaxSteps:                     0,
				Gate:                         headlessGate,
				ModelRef:                     modelRefFromEntry(pe),
				ContextWindow:                pe.ContextWindow,
				SoftCompactRatio:             cfg.Agent.SoftCompactRatio,
				ToolResultSnipRatio:          cfg.Agent.ToolResultSnipRatio,
				CompactRatio:                 cfg.Agent.CompactRatio,
				CompactForceRatio:            cfg.Agent.CompactForceRatio,
				ContextEditing:               cfg.Agent.ContextEditing,
				RecentKeep:                   cfg.Agent.RecentKeep,
				ArchiveDir:                   config.ArchiveDir(),
				KeepPolicy:                   keepPolicy,
				ReasoningLanguage:            cfg.ReasoningLanguage(),
				PlanModeReadOnlyCommands:     cfg.Agent.PlanModeReadOnlyCommands,
				CapabilityLedger:             plannerLedger,
				CapabilityAudit:              plannerAudit,
				MissingReasoningWarnStateDir: config.MissingReasoningWarnStateDir(),
			}
			runner = agent.NewCoordinatorWithPlannerPolicy(plannerProv, plannerSess, pe.Price, plannerTools, plannerOpts, executor, cfg.Agent.Temperature, sink, control.NewPlannerPolicy())
			label = entry.Model + " + planner " + pe.Model
		}
	}

	ctrlOpts := control.Options{
		TaskBudget:                     taskBudgetFromConfig(cfg),
		GoalTokenBudget:                cfg.Agent.GoalTokenBudget,
		Runner:                         runner,
		Executor:                       executor,
		Sink:                           sink,
		Policy:                         policy,
		SubagentGate:                   headlessGate,
		Label:                          label,
		ModelRef:                       modelRef,
		SystemPrompt:                   sysPrompt,
		SessionDir:                     sessionDir,
		Host:                           pluginHost,
		Commands:                       cmds,
		Skills:                         skills,
		AllSkills:                      allSkills,
		SkillStore:                     skillStore,
		AllSkillStore:                  allSkillStore,
		DisableImplicitSkillInvocation: !implicitSkillInvocation,
		SkillRunner:                    skillRunner,
		ReadOnlySkillRunner:            readOnlySkillRunner,
		SkillProfile:                   skillProfile,
		Hooks:                          hookRunner,
		Memory:                         mem,
		// Indirection: the cleanup variable gains the extension runtime set at
		// the end of build (snapshot assembly runs after control.New), and the
		// controller must observe the final chain at Close time.
		Cleanup:               func() { cleanup() },
		BalanceURL:            entry.BalanceURL,
		BalanceKey:            entry.APIKey(),
		BalanceClient:         balanceClient,
		Jobs:                  jm,
		TaskStore:             opts.TaskStore,
		WorkspaceLease:        workspaceLease,
		Registry:              reg,
		PluginCtx:             ctx,
		MCPDefaultCallTimeout: pluginSpecOptions.DefaultCallTimeout,
		MCPConfigureSpec: func(spec *plugin.Spec) {
			if spec == nil {
				return
			}
			spec.LaunchManager = pluginSpecOptions.LaunchManager
			if strings.TrimSpace(spec.ConfigSource) == "" {
				spec.ConfigSource = pluginSpecOptions.ConfigSource
			}
			if spec.DefaultStartupTimeout <= 0 {
				spec.DefaultStartupTimeout = pluginSpecOptions.DefaultStartupTimeout
			}
			applyMCPIsolation(spec, root, pluginSpecOptions)
		},
		CapabilityRuntime:      capRuntime,
		WorkspaceRoot:          root,
		ExternalFolderToolRefs: readPathResolver,
		ResponseLanguage:       cfg.ResponseLanguage(),
		ReasoningLanguage:      cfg.ReasoningLanguage(),
		DisableColdResumePrune: !cfg.ColdResumePruneEnabled(),
		Shell:                  shell,
		ApprovalTimeout:        opts.ApprovalTimeout,
		RuntimeProfile:         runtimeProfile,
		Ablation:               opts.Ablation,
		OnRemember: func(rule string) control.RememberResult {
			return rememberPermissionRule(root, rule)
		},
		OnRememberPlanModeReadOnlyCommand: func(prefix string) control.PlanModeReadOnlyCommandTrustResult {
			return rememberPlanModeReadOnlyCommand(root, prefix)
		},
		SessionRecoveryMeta: opts.SessionRecoveryMeta,
		OnSessionRecovered:  opts.OnSessionRecovered,
		// The merged catalog (nil without provider-declaring sidecars) lets
		// frontends enumerate plugin/... models through ProviderCatalog.
		ProviderResolver:  extensionResolver,
		RuntimeGeneration: generation,
		RuntimeOwner:      owner,
		// Share the Manager already bound into bash/grep so tools and the
		// Controller observe the same temporary generation across rebuilds.
		SessionTemp: sessionTemp,
	}
	// Guardian: when guardian_model is configured, spawn an LLM safety reviewer
	// that can auto-allow safe Ask decisions and annotate risky ones before
	// escalating to the human approval prompt.
	if guardianModel := cfg.Agent.GuardianModel; guardianModel != "" {
		ge, ok := resolveOptionalEntry(effectiveResolver, cfg, guardianModel)
		if !ok {
			slog.Warn("guardian model is not a configured provider — guardian disabled", "model", guardianModel)
			sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "Guardian was disabled because its model was not found.", Detail: fmt.Sprintf("guardian_model %q not found — guardian disabled", guardianModel)})
		} else {
			pProv, err := resolveProvider(effectiveResolver, cfg, proxySpec, provider.Selection{Ref: modelRefFromEntry(ge)})
			if err != nil {
				slog.Warn("guardian provider construction failed — guardian disabled", "model", guardianModel, "err", err)
				sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "Guardian was disabled because it could not start.", Detail: fmt.Sprintf("guardian construction failed: %v — guardian disabled", err)})
			} else {
				guardianReg := agent.FilterReadOnlyRegistry(reg, agent.SubagentMetaTools()...)
				ctrlOpts.Guardian = guardian.NewSession(pProv, guardianReg, guardian.PolicyPrompt(), modelRefFromEntry(ge), cfg.Agent.GuardianTemperature, ge.Price, sink)
				sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: fmt.Sprintf("guardian enabled · model=%s", ge.Model)})
			}
		}
	}
	// Recovery reviewer: prefer recovery_model, then guardian_model, then the
	// active main model with an isolated session/policy.
	{
		recoveryModel := strings.TrimSpace(cfg.Agent.RecoveryModel)
		if recoveryModel == "" {
			recoveryModel = strings.TrimSpace(cfg.Agent.GuardianModel)
		}
		if recoveryModel == "" {
			recoveryModel = modelRef
		}
		if recoveryModel != "" {
			if extensionResolver != nil && providerext.PluginRefOwner(recoveryModel) != "" {
				// A plugin-namespaced recovery reviewer resolves through the
				// merged resolver; the config path cannot see extension refs.
				if re, ok := resolveOptionalEntry(extensionResolver, cfg, recoveryModel); ok {
					if rProv, err := extensionResolver.Resolve(provider.Selection{Ref: modelRefFromEntry(re)}); err == nil {
						ctrlOpts.RecoveryReviewer = recovery.NewSessionWithSink(rProv, re.Price, modelRefFromEntry(re), sink)
					} else {
						slog.Warn("recovery reviewer provider construction failed — rule-only recovery", "model", recoveryModel, "err", err)
					}
				}
			} else if re, ok := cfg.ResolveModel(recoveryModel); ok {
				if rProv, err := NewProviderWithProxy(re, proxySpec); err == nil {
					ctrlOpts.RecoveryReviewer = recovery.NewSessionWithSink(rProv, re.Price, modelRefFromEntry(re), sink)
				} else {
					slog.Warn("recovery reviewer provider construction failed — rule-only recovery", "model", recoveryModel, "err", err)
				}
			}
		}
		// HeadlessApprovalMode is an explicit declaration that this frontend has
		// no decision channel (`reasonix run`). ApprovalTimeout is not a proxy for
		// that capability: headless frontends have a bounded timeout and can still answer cards.
		ctrlOpts.RecoveryHeadless = recoveryHeadlessMode(opts)
	}
	// Goal evaluator: the same zero-config model fallback as the recovery
	// reviewer (recovery_model → guardian_model → main model), isolated session
	// and policy. When unavailable, Goal turns without an update_goal report
	// fail closed and pause instead of defaulting to continue.
	{
		evalModel := strings.TrimSpace(cfg.Agent.RecoveryModel)
		if evalModel == "" {
			evalModel = strings.TrimSpace(cfg.Agent.GuardianModel)
		}
		if evalModel == "" {
			evalModel = modelRef
		}
		if evalModel != "" {
			if re, ok := cfg.ResolveModel(evalModel); ok {
				if eProv, err := NewProviderWithProxy(re, proxySpec); err == nil {
					ctrlOpts.GoalEvaluator = goaleval.NewSessionWithSink(eProv, re.Price, modelRefFromEntry(re), sink)
				} else {
					slog.Warn("goal evaluator provider construction failed — goals without an update_goal report will pause", "model", evalModel, "err", err)
				}
			}
		}
	}
	ctrl := control.New(ctrlOpts)
	// Publish the controller to the extension UI hub's indirection: from here
	// on, host/ui/* publishes ride ctrl.EmitExtensionEvent and blocking prompts
	// ride ctrl.Ask, exactly as if the hub had been built after control.New.
	ctrlRef.Store(ctrl)
	close(controllerReady)
	// Share the recovery checkpoint with task/fleet sub-agents so background
	// writers observe the same failure state as the root agent.
	if taskTool != nil {
		if g := ctrl.Executor(); g != nil {
			taskTool.WithRecoveryGate(g.RecoveryGate())
		}
	}
	if capRuntime != nil {
		ctrl.SetCapabilityProxyTools(capRuntime.ConnectedProxyTools)
	}
	// Task tools created before capRuntime assignment still need the runtime if
	// they were built early; re-bind when present.
	if taskTool != nil && capRuntime != nil {
		taskTool.WithCapabilityRuntime(capRuntime)
	}
	// Build one role-neutral semantic router so an in-place switch never needs a
	// controller rebuild. The frozen TaskPolicy decides whether a turn may call
	// it; construction alone does not add a provider request.
	var router *capability.SemanticRouter
	if modelRef := strings.TrimSpace(cfg.Agent.SubagentModels["capability-router"]); modelRef != "" {
		effortRef := strings.TrimSpace(cfg.Agent.SubagentEfforts["capability-router"])
		if p, price, _, err := resolveSubagentProvider(modelRef, effortRef); err == nil && p != nil {
			usageModelRef, _ := subagentIdentity(modelRef, effortRef)
			router = &capability.SemanticRouter{Provider: p, Sink: sink, Model: usageModelRef, Pricing: price, Audit: capAudit}
		}
	}
	if router == nil {
		router = &capability.SemanticRouter{Provider: execProv, Sink: sink, Model: modelRef, Pricing: entry.Price, Audit: capAudit}
	}
	ctrl.WireCapabilityRouting(cfg.Plugins, capSpecs, router, capAudit)
	ctrl.SetCapabilityProxyRouting(true)

	// Provider-visible tool surface is identical for every role setting before
	// the extension snapshot freezes registry schemas for cache diagnostics.
	applyUnifiedProviderToolSurface(reg)

	// Freeze the extension kernel's snapshot of exactly what this build wired.
	// The snapshot is assembled from the in-hand objects above — discovery
	// never re-runs — and assembly must never fail the boot: a kernel error
	// degrades to a nil snapshot (logged) while the controller behaves exactly
	// as before. The sidecar Manager comes from preflight (started once,
	// before model resolution); assembly takes over its ownership and freezes
	// the same generation the sidecars were handshaken with. The frozen
	// provider catalog is the BASE catalog, exactly as before the preflight
	// refactor: sidecar providers enter the snapshot through the Manager's own
	// contributions, not through the legacy provider list.
	mcpSpecs := enabledMCPSpecs(configSpecs, extraSpecs)
	snap, runtimeSet, extensionDispatcher, snapErr := assembleLegacySnapshot(ctx, legacyAssembly{
		systemPrompt: sysPrompt,
		registry:     reg,
		skills:       skills,
		commands:     cmds,
		hooks:        resolvedHooks,
		mcpSpecs:     mcpSpecs,
		providers:    baseResolver.Catalog(),
	}, generation, extensionBoot{
		session:            protocol.SessionContext{SessionID: sessionID, WorkspaceRoot: root, Generation: generation},
		ui:                 extUIHub,
		onWarning:          extWarn,
		skipPromptStrategy: shouldSkipPromptStrategy(opts.PreviousPlan),
		previousDispatcher: opts.PreviousDispatcher,
	}, extensionMgr)
	// Ownership of the preflighted Manager transferred to assembly on every
	// path: it was either closed inside or registered into the RuntimeSet.
	bc.pendingMgr = nil
	if snapErr != nil {
		// These assembly failures are fatal rather than degradable: two
		// runtimes claiming the same replacement slot (the kernel's
		// ReplaceClaims verdict) and a failed system_prompt.build strategy
		// ruling (the slot owner is required-class, so dispatch surfaces its
		// failure as one of these types) mean the extension contract the user
		// installed cannot be honored; booting without it would silently
		// change what the session is. (A required runtime that cannot start
		// fails earlier, in preflight, with the same fatality.)
		var requiredErr *sidecar.RequiredStartError
		var slotErr *extension.SlotConflictError
		var blockErr *dispatch.BlockError
		var failureErr *dispatch.FailureError
		var violationErr *dispatch.ViolationError
		if errors.As(snapErr, &requiredErr) || errors.As(snapErr, &slotErr) ||
			errors.As(snapErr, &blockErr) || errors.As(snapErr, &failureErr) || errors.As(snapErr, &violationErr) {
			ctrl.ReleaseResources()
			return nil, fmt.Errorf("boot: %w", snapErr)
		}
		slog.Warn("boot: extension snapshot assembly failed; continuing without a runtime snapshot", "err", snapErr)
		runtimeSet = extension.NewRuntimeSet(generation)
		// Assembly retired the preflighted Manager on the error path; the
		// controller must not bind a hub or expose a manager whose sidecars
		// are already shut down.
		extensionMgr = nil
	}
	// The stage-7 provider merge happened at preflight, before model
	// resolution; BuildResult.ProviderResolver exposes that same merged
	// resolver (the base when no sidecar declared providers).
	providerResolver := baseResolver
	if extensionResolver != nil {
		providerResolver = extensionResolver
	}
	cleanup = wireRuntimeScopeCleanup(runtimeSet, cleanup, opts.SharedHost, pluginHost, lspMgr, opts.SessionTemp)
	ctrl.SetExtensions(extensionDispatcher)
	if extensionMgr == nil {
		extUIHub = nil
	} else {
		ctrl.SetExtensionUI(extUIHub)
	}
	if providerResolver != nil {
		ctrl.SetProviderResolver(providerResolver)
	}
	// Stage 6b2 system-prompt handoff: the 6b1 strategy pass may have replaced
	// the prompt while the snapshot was freezing, but the executor session was
	// built earlier with the host-composed prompt. Swap in a fresh session
	// carrying the final prompt now — before any turn or history resume, so
	// the live session and the frozen snapshot describe the same session.
	if snap != nil {
		if final := snap.SystemPrompt(); final != sysPrompt {
			ctrl.ApplyExtensionSystemPrompt(final)
		}
	}
	assembly := &ReusedAssembly{
		SystemPrompt:            sysPrompt,
		Skills:                  skills,
		Commands:                cmds,
		Hooks:                   resolvedHooks,
		Registry:                reg,
		ImplicitSkillInvocation: implicitSkillInvocation,
	}
	return finalizeBuildResult(&BuildResult{Controller: ctrl, Snapshot: snap, Runtime: runtimeSet, Owner: owner, Extensions: extensionMgr, Dispatcher: extensionDispatcher, ExtensionUI: extUIHub, ProviderResolver: providerResolver, BaseProviderResolver: baseResolver, Assembly: assembly}, !opts.deferPublish), nil
}
