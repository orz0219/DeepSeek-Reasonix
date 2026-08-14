package boot

import (
	"context"
	"errors"
	"fmt"
	"reasonix/internal/ablation"
	"reasonix/internal/agent"
	"reasonix/internal/capability"
	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/history"
	"reasonix/internal/installsource"
	"reasonix/internal/memory"
	"reasonix/internal/plugin"
	"reasonix/internal/productdocs"
	"reasonix/internal/provider"
	"reasonix/internal/skill"
	"reasonix/internal/tool"
	"reasonix/internal/tool/sessiontool"
	"strings"
)

// buildSubagents wires the skill-tool surface: capability ledger/audit,
// slash commands, and the skill runners that drive sub-agent invocation.
func buildSubagents(ctx context.Context, bc *bootContext, opts Options) error {
	cfg := bc.cfg
	reg := bc.reg
	root := bc.root
	skillStore := bc.skillStore
	pluginSpecOptions := bc.pluginSpecOptions
	implicitSkillInvocation := bc.implicitSkillInvocation
	sessionDir := bc.sessionDir
	mem := bc.mem
	headlessGate := bc.headlessGate
	keepPolicy := bc.keepPolicy
	maxSubagentDepth := bc.maxSubagentDepth
	tokenDelivery := bc.tokenDelivery
	workspaceLease := bc.workspaceLease
	resolveSubagentProvider := bc.resolveSubagentProvider
	subagentIdentity := bc.subagentIdentity
	subagentScheduler := bc.subagentScheduler
	execProv := bc.execProv
	entry := bc.entry
	capRuntime := bc.capRuntime
	maxSteps := bc.maxSteps
	subagentStore := bc.subagentStore
	balanceClient := bc.balanceClient
	pluginHost := bc.pluginHost
	skills := bc.skills
	// Product documentation, session, and memory tools are always present on the
	// unified host registry for every role setting. Provider-visible surface stays
	// lean via use_capability; these tools are dispatchable without schema growth.
	docsToolAdded := false
	addDocsTool := func() string {
		if docsToolAdded {
			return "docs is already enabled."
		}
		docsToolAdded = true
		reg.Add(productdocs.NewTool())
		return "enabled docs."
	}
	sessionToolsAdded := false
	addSessionTools := func() string {
		if sessionToolsAdded {
			return "sessions are already enabled."
		}
		sessionToolsAdded = true
		// history and memory are the BM25-backed surfaces; the ablation arm drops
		// only those two and leaves the direct-access tools alone, so a lost solve
		// is attributable to retrieval and not to a missing session reader.
		if opts.Ablation.Off(ablation.Retrieval) {
			reg.Add(sessiontool.NewListSessionsTool(sessionDir))
			reg.Add(sessiontool.NewReadSessionTool(sessionDir))
			return "enabled list_sessions, read_session."
		}
		reg.Add(history.NewIndexedTool(history.Options{SessionDir: sessionDir, GlobalSessionDir: config.SessionDir(), ArchiveDir: config.ArchiveDir()}))
		reg.Add(sessiontool.NewListSessionsTool(sessionDir))
		reg.Add(sessiontool.NewReadSessionTool(sessionDir))
		return "enabled history, list_sessions, read_session."
	}
	memoryToolsAdded := false
	addMemoryTools := func() string {
		if memoryToolsAdded {
			return "memory tools are already enabled."
		}
		memoryToolsAdded = true
		if opts.Ablation.Off(ablation.Retrieval) {
			reg.Add(memory.NewRememberTool(mem.Store))
			reg.Add(memory.NewForgetTool(mem.Store))
			return "enabled remember, forget."
		}
		reg.Add(memory.NewRecallTool(mem.Store))
		reg.Add(memory.NewRememberTool(mem.Store))
		reg.Add(memory.NewForgetTool(mem.Store))
		return "enabled memory, remember, forget."
	}
	addDocsTool()
	addSessionTools()
	addMemoryTools()

	// `ask` puts structured multiple-choice questions to the user via the Asker
	// on the call context; a headless run has none, so it resolves to
	// "decide for yourself".
	reg.Add(agent.NewAskTool())

	// Skill tools: read_only_skill is a narrow read-only entry; the full skills
	// source adds run_skill plus subagent wrappers. Read-only subagent skills
	// cannot write, install, mutate memory, or delegate further.
	//
	// subagentSkillOptions is the single construction point for skill sub-agent
	// run options, so the read-only and writer-capable runners cannot drift on
	// compaction or language settings — add new fields here, not per runner.
	subagentSkillOptions := func(sctx context.Context, steps int, price *provider.Pricing, ctxWin, childDepth int) agent.Options {
		return agent.Options{
			MaxSteps:            steps,
			Temperature:         cfg.Agent.Temperature,
			Pricing:             price,
			UsageSource:         event.UsageSourceSubagent,
			Gate:                headlessGate,
			ContextWindow:       ctxWin,
			RecentKeep:          cfg.Agent.RecentKeep,
			SoftCompactRatio:    cfg.Agent.SoftCompactRatio,
			ToolResultSnipRatio: cfg.Agent.ToolResultSnipRatio,
			CompactRatio:        cfg.Agent.CompactRatio,
			CompactForceRatio:   cfg.Agent.CompactForceRatio,
			ContextEditing:      cfg.Agent.ContextEditing,
			ArchiveDir:          config.ArchiveDir(),
			KeepPolicy:          keepPolicy,
			ResponseLanguage:    agent.ResponseLanguageFromContext(sctx),
			ReasoningLanguage:   agent.ReasoningLanguageFromContext(sctx),
			SubagentDepth:       childDepth,
			MaxSubagentDepth:    maxSubagentDepth,
			DeliveryProfile:     tokenDelivery,
			Ablation:            opts.Ablation,
			WorkspaceLease:      workspaceLease,
		}
	}
	readOnlySkillRunner := func(sctx context.Context, sk skill.Skill, task string, runOpts skill.SubagentRunOptions) (string, error) {
		if strings.TrimSpace(runOpts.ContinueFrom) != "" || strings.TrimSpace(runOpts.ForkFrom) != "" {
			return "", fmt.Errorf("read_only_skill does not support continue_from/fork_from")
		}
		releaseSlot, err := subagentScheduler.Acquire(sctx, agent.AcquireRequest{
			Writer: false,
			Nested: agent.SubagentDepth(sctx) > 0,
			Label:  sk.Name,
		})
		if err != nil {
			return "", err
		}
		defer releaseSlot()
		sk = skill.WithCodeGraphTools(sk, skill.CodeGraphReadTools(reg))
		prov, price, ctxWin := execProv, entry.Price, entry.ContextWindow
		modelRef := subagentModelRef(cfg, sk)
		effortRef := subagentEffortRef(cfg, sk)
		if modelRef != "" || effortRef != "" {
			p, pr, cw, err := resolveSubagentProvider(modelRef, effortRef)
			if err != nil {
				return "", fmt.Errorf("read-only subagent skill %q profile: %w", sk.Name, err)
			}
			prov, price, ctxWin = p, pr, cw
		}
		childDepth := agent.SubagentDepth(sctx) + 1
		if childDepth > maxSubagentDepth {
			return "", fmt.Errorf("subagent delegation depth limit reached (max_subagent_depth=%d)", maxSubagentDepth)
		}
		subReg := agent.ReadOnlySubagentToolRegistryForDepthWithRuntime(reg, sk.AllowedTools, childDepth, maxSubagentDepth, capRuntime)
		if subReg.Len() == 0 {
			return "", fmt.Errorf("read_only_skill: skill %q has no read-only tools available", sk.Name)
		}
		switch sk.Name {
		case "review", "security-review", "security_review":
			agent.AttachReviewReportTool(subReg)
		}
		steps := maxSteps
		if steps > 0 {
			if steps /= 2; steps < 5 {
				steps = 5
			}
		}
		// Custom and named built-in profiles fully control their system prompt
		// (no implicit concise/DefaultReadOnlyTaskSystemPrompt overlay).
		sysPrompt := strings.TrimSpace(sk.Body)
		if sysPrompt == "" {
			sysPrompt = agent.DefaultReadOnlyTaskSystemPrompt
		}
		runOptions := subagentSkillOptions(sctx, steps, price, ctxWin, childDepth)
		usageModelRef, _ := subagentIdentity(modelRef, effortRef)
		runOptions.ModelRef = usageModelRef
		// Delivery risk gates consume typed reports; outside Delivery a casual
		// /review run may finish with prose only.
		if runOptions.DeliveryProfile {
			runOptions.RequireReviewReportKind = agent.ReviewReportKindForSkill(sk.Name)
		}
		// Provider serializers decide whether these images are wire-visible from
		// the child model's own vision capability. Text-only children retain the
		// attachment metadata locally but never receive image parts on the wire.
		childCtx := agent.WithUserImages(sctx, agent.SubagentImageCandidates(sctx))
		return agent.RunReadOnlySubAgentWithSession(childCtx, prov, subReg, agent.NewSession(sysPrompt), task,
			runOptions, agent.NestedSink(sctx, event.Discard))
	}
	// Writer-capable subagent skills run through an isolated loop with the skill
	// body as system prompt and a tool set scoped to allowed-tools (minus
	// recursive meta-tools); activity nests under the invoking call, like `task`.
	skillRunner := func(sctx context.Context, sk skill.Skill, task string, runOpts skill.SubagentRunOptions) (string, error) {
		// Writer skills without write_paths claim the whole workspace so they
		// cannot race fleet/task writers that declared disjoint paths.
		acq := agent.AcquireRequest{
			Writer: !sk.ReadOnly,
			Nested: agent.SubagentDepth(sctx) > 0,
			Label:  sk.Name,
		}
		if !sk.ReadOnly {
			whole, werr := agent.WholeWorkspaceWriteClaim(root)
			if werr != nil {
				return "", fmt.Errorf("subagent skill %q write claim: %w", sk.Name, werr)
			}
			acq.WritePaths = whole
		}
		releaseSlot, err := subagentScheduler.Acquire(sctx, acq)
		if err != nil {
			return "", err
		}
		defer releaseSlot()
		sk = skill.WithCodeGraphTools(sk, skill.CodeGraphReadTools(reg))
		prov, price, ctxWin := execProv, entry.Price, entry.ContextWindow
		modelRef := subagentModelRef(cfg, sk)
		effortRef := subagentEffortRef(cfg, sk)
		if modelRef != "" || effortRef != "" {
			p, pr, cw, err := resolveSubagentProvider(modelRef, effortRef)
			if err != nil {
				return "", fmt.Errorf("subagent skill %q profile: %w", sk.Name, err)
			}
			prov, price, ctxWin = p, pr, cw
		}
		childDepth := agent.SubagentDepth(sctx) + 1
		if childDepth > maxSubagentDepth {
			return "", fmt.Errorf("subagent delegation depth limit reached (max_subagent_depth=%d)", maxSubagentDepth)
		}
		// A read-only skill (builtin review/security-review, or frontmatter
		// `read-only: true`) is enforced at the tool boundary: writer tools are
		// stripped and bash runs under the read-only command policy. Transcripts
		// against the writer-capable registry stop matching on continue_from
		// (schema-hash check reports the mismatch).
		var subReg *tool.Registry
		if sk.ReadOnly {
			subReg = agent.ReadOnlySubagentToolRegistryForDepthWithRuntime(reg, sk.AllowedTools, childDepth, maxSubagentDepth, capRuntime)
		} else {
			subReg = agent.SubagentToolRegistryForDepthWithRuntime(reg, sk.AllowedTools, childDepth, maxSubagentDepth, capRuntime)
		}
		// Delivery risk gates require structured review_report from review
		// subagents only — never expose it on the parent tool surface.
		switch sk.Name {
		case "review", "security-review", "security_review":
			agent.AttachReviewReportTool(subReg)
		}
		continueFrom := strings.TrimSpace(runOpts.ContinueFrom)
		legacyForkFrom := strings.TrimSpace(runOpts.ForkFrom)
		if continueFrom != "" && legacyForkFrom != "" {
			return "", fmt.Errorf("continue_from and fork_from are mutually exclusive; pass only continue_from")
		}
		parentID, _, _, _ := agent.CallContext(sctx)
		if runOpts.HostInitiated {
			parentID = ""
		}
		parentSession := agent.ParentSession(sctx)
		var run *agent.SubagentRun
		if subagentStore == nil || parentSession == "" {
			// Headless runs have no persistent session to own a transcript, so
			// run the skill sub-agent ephemerally instead of failing; continuation
			// needs a persisted owner and errors here.
			if continueFrom != "" || legacyForkFrom != "" {
				return "", fmt.Errorf("subagent continuation requires a persisted session; none is active in this run")
			}
			run = agent.EphemeralSubagentRun(sk.Body)
		} else {
			identityModel, identityEffort := subagentIdentity(modelRef, effortRef)
			spec := agent.SubagentSpec{
				Kind:             "skill",
				Name:             sk.Name,
				WorkspaceRoot:    root,
				ParentSession:    parentSession,
				ParentToolCallID: parentID,
				SystemPrompt:     sk.Body,
				Registry:         subReg,
				Model:            identityModel,
				Effort:           identityEffort,
			}
			var prepErr error
			if continueFrom != "" {
				run, prepErr = subagentStore.PrepareContinue(continueFrom, spec)
			} else if legacyForkFrom != "" {
				run, prepErr = subagentStore.PrepareLegacyForkFrom(legacyForkFrom, spec)
			} else {
				run, prepErr = subagentStore.PrepareFresh(spec)
			}
			if prepErr != nil {
				return "", prepErr
			}
		}
		defer run.Release()
		steps := maxSteps
		if steps > 0 {
			if steps /= 2; steps < 5 {
				steps = 5
			}
		}
		runOptions := subagentSkillOptions(sctx, steps, price, ctxWin, childDepth)
		usageModelRef, _ := subagentIdentity(modelRef, effortRef)
		runOptions.ModelRef = usageModelRef
		// Delivery risk gates consume typed reports; outside Delivery a casual
		// /review run may finish with prose only.
		if runOptions.DeliveryProfile {
			runOptions.RequireReviewReportKind = agent.ReviewReportKindForSkill(sk.Name)
		}
		var answer string
		// See the read-only runner above: the child provider, not the parent
		// model, owns the final vision decision.
		childCtx := agent.WithUserImages(sctx, agent.SubagentImageCandidates(sctx))
		if sk.ReadOnly {
			answer, err = agent.RunReadOnlySubAgentWithSession(childCtx, prov, subReg, run.Session, task,
				runOptions, agent.NestedSink(sctx, event.Discard))
		} else {
			answer, err = agent.RunSubAgentWithSession(childCtx, prov, subReg, run.Session, task,
				runOptions, agent.NestedSink(sctx, event.Discard))
		}
		if err != nil {
			return "", errors.Join(err, subagentStore.SaveFailed(run))
		}
		if err := subagentStore.SaveCompleted(run); err != nil {
			return "", errors.Join(err, subagentStore.SaveFailed(run))
		}
		return agent.FormatSubagentRunResult(answer, run, false), nil
	}
	skillProfile := func(sk skill.Skill) *event.Profile {
		model, effort := subagentModelRef(cfg, sk), subagentEffortRef(cfg, sk)
		if model == "" && effort == "" {
			return nil
		}
		return &event.Profile{Model: model, Effort: effort}
	}
	var cmds []command.Command
	if opts.ReuseAssembly != nil && shouldReuseDiscovery(opts.PreviousPlan) {
		cmds = opts.ReuseAssembly.Commands
	} else {
		cmds, _ = command.LoadRoots(config.CommandRootsForRoot(root)...)
	}
	slashCommandAdded := false
	slashCommandIncludesSkills := false
	addSlashCommandTool := func(includeSkills bool) string {
		if slashCommandAdded && (!includeSkills || slashCommandIncludesSkills) {
			return "slash commands are already enabled."
		}
		// Expose loaded slash commands to the model via slash_command. In economy
		// mode skills join this list only after the skills source is enabled.
		var slashEntries []command.SlashEntry
		if includeSkills && implicitSkillInvocation {
			for _, sk := range skillStore.SlashList() {
				slashEntries = append(slashEntries, command.SlashEntry{
					Name:        sk.SlashName(),
					Description: sk.Description,
					Render:      func(args []string) string { return skillStore.Render(sk, strings.Join(args, " ")) },
				})
			}
		}
		for _, cmd := range cmds {
			if cmd.Hidden {
				continue
			}

			slashEntries = append(slashEntries, command.SlashEntry{
				Name:        cmd.Name,
				Description: cmd.Description,
				ArgHint:     cmd.ArgHint,
				Render:      func(args []string) string { return cmd.Render(args) },
			})
		}
		reg.Add(command.NewSlashCommandTool(slashEntries))
		slashCommandAdded = true
		slashCommandIncludesSkills = slashCommandIncludesSkills || includeSkills
		return "enabled slash_command."
	}
	installSourceAdded := false
	addInstallSourceTool := func() string {
		if installSourceAdded {
			return "install_source is already enabled."
		}
		installSourceAdded = true
		reg.Add(installsource.NewTool(installsource.Options{
			ProjectRoot: root,
			HTTPClient:  balanceClient,
			ConnectMCP: func(e config.PluginEntry) (installsource.MCPConnectResult, error) {
				spec := pluginSpecFromEntryWithOptions(e, root, pluginSpecOptions)
				if opts.Stderr != nil {
					spec.Stderr = opts.Stderr
				}
				// Applying an install plan is an explicit user decision; record the
				// durable launch grant now so neither this connection nor the next
				// session re-asks for the same install.
				launchAuthorized := false
				if spec.RequireLaunchApproval {
					if err := plugin.AuthorizeSpecLaunch(ctx, spec); err != nil {
						return installsource.MCPConnectResult{}, err
					}
					launchAuthorized = true
				}
				tools, err := pluginHost.Add(ctx, spec)
				if err != nil {
					// The install did not complete, so do not retain consent for a
					// server that never connected. Replacement rollback reauthorizes
					// the previous project entry before reconnecting it.
					if launchAuthorized && spec.LaunchManager != nil {
						_ = spec.LaunchManager.Revoke(spec.Name)
					}
					return installsource.MCPConnectResult{}, err
				}
				reg.RemovePrefix(plugin.ToolPrefix(spec.Name))
				for _, t := range tools {
					reg.Add(t)
				}
				// Disconnect closes the server and drops its namespaced tools.
				// Used by the install_source rollback path when SaveTo fails.
				disconnect := func() {
					if prefix, ok := pluginHost.Remove(spec.Name); ok {
						reg.RemovePrefix(prefix)
					}
					if spec.LaunchManager != nil {
						_ = spec.LaunchManager.Revoke(spec.Name)
					}
				}
				return installsource.MCPConnectResult{
					ToolCount:  len(tools),
					Disconnect: disconnect,
				}, nil
			},
			OnDisconnect: func(serverName string) bool {
				if prefix, ok := pluginHost.Remove(serverName); ok {
					reg.RemovePrefix(prefix)
					return true
				}
				return false
			},
		}))
		return "enabled install_source."
	}
	readOnlySkillToolsAdded := false
	addReadOnlySkillTools := func() string {
		if !implicitSkillInvocation {
			return "automatic skill invocation is disabled; use an explicit /skill command instead."
		}
		if readOnlySkillToolsAdded {
			return "read_only_skill tool is already enabled.\n\n" + skill.ReadOnlyIndexBlock(skills)
		}
		readOnlySkillToolsAdded = true
		reg.Add(skill.NewReadOnlySkillTool(skillStore, gateSubagentArm(opts.Ablation, readOnlySkillRunner), skillProfile))
		return "enabled read_only_skill. Use read_only_skill for inline skills or read-only subagent skills on the next model request.\n\n" + skill.ReadOnlyIndexBlock(skills)
	}
	skillToolsAdded := false
	addSkillTools := func() string {
		if !implicitSkillInvocation {
			return "automatic skill invocation is disabled; use an explicit /skill command instead."
		}
		if skillToolsAdded {
			return "skills are already enabled.\n\n" + skill.IndexBlock(skills)
		}
		skillToolsAdded = true
		addReadOnlySkillTools()
		reg.Add(skill.NewRunSkillTool(skillStore, gateSubagentArm(opts.Ablation, skillRunner), skillProfile))
		reg.Add(skill.NewReadSkillTool(skillStore))
		reg.Add(skill.NewInstallSkillTool(skillStore, nil))
		for _, t := range builtinSubagentTools(opts.Ablation, skillStore, skillRunner, skillProfile) {
			reg.Add(t)
		}
		addSlashCommandTool(implicitSkillInvocation)
		return "enabled skills. Use run_skill/read_skill/read_only_skill or the dedicated skill tools on the next model request.\n\n" + skill.IndexBlock(skills)
	}
	addInstallSourceTool()
	if implicitSkillInvocation {
		addSkillTools()
	} else {
		addSlashCommandTool(false)
	}

	// Session-shared MCP runtime: Host, specs, and connection snapshots; each
	// agent gets its own use_capability frontend (ledger/audit isolation) while
	// reusing processes, without inheriting dynamic mcp__* schemas.
	capSpecs := PluginSpecsForRootWithOptions(cfg.Plugins, root, pluginSpecOptions)
	cachedTools, cacheKeyOK := capability.LoadCachedToolsForSpecs(capSpecs)
	skillStore.ConfigureToolBindings(func(sk skill.Skill) []tool.MCPBinding {
		return skillMCPBindings(sk, reg, capSpecs, cachedTools, cacheKeyOK)
	})

	bc.capSpecs = capSpecs
	bc.cmds = cmds
	bc.skillRunner = skillRunner
	bc.skillProfile = skillProfile
	bc.readOnlySkillRunner = readOnlySkillRunner
	bc.cachedTools = cachedTools
	bc.cacheKeyOK = cacheKeyOK
	return nil
}
