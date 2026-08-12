package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime/debug"
	"strings"

	"reasonix/internal/checkpoint"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/jobs"
	"reasonix/internal/memory"
	"reasonix/internal/permission"
	"reasonix/internal/planmode"
	"reasonix/internal/tool"
)

func (b foregroundOnlyBash) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		RunInBackground bool `json:"run_in_background"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if p.RunInBackground {
		return "", tool.Blocked("blocked: background bash is unavailable in subagents; run a foreground command or ask the parent agent to start a background job")
	}
	return b.inner.Execute(ctx, args)
}

func (b readOnlyBash) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if !permission.BashCommandIsReadOnly(args) {
		return "", tool.Blocked("blocked: read-only subagents can run only permission-classified foreground read-only commands")
	}
	return b.inner.Execute(ctx, args)
}

func (r *ReadOnlyTaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if r == nil || r.task == nil {
		return "", fmt.Errorf("read_only_task is not configured")
	}
	var p struct {
		Prompt      string   `json:"prompt"`
		Description string   `json:"description"`
		Tools       []string `json:"tools"`
		MaxSteps    int      `json:"max_steps"`
		Model       string   `json:"model"`
		Effort      string   `json:"effort"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}

	spec, err := r.task.buildTaskSpec(ctx, p.Prompt, p.Description, "", nil, p.Tools, p.MaxSteps, p.Model, p.Effort, "", "", false, true)
	if err != nil {
		return "", err
	}
	spec.Worker.SystemPrompt = DefaultReadOnlyTaskSystemPrompt
	spec.Context.Ephemeral = true
	return r.task.RunProfileSpec(ctx, spec)
}

func (t *TaskTool) effectiveProfile(model, effort string) (string, string) {
	model = strings.TrimSpace(model)
	effort = strings.TrimSpace(effort)
	if model == "" {
		model = strings.TrimSpace(t.subagentModel)
	}
	if effort == "" {
		effort = strings.TrimSpace(t.subagentEffort)
	}
	return model, effort
}

func (t *TaskTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Prompt          string   `json:"prompt"`
		Description     string   `json:"description"`
		Profile         string   `json:"profile"`
		WritePaths      []string `json:"write_paths"`
		Tools           []string `json:"tools"`
		MaxSteps        int      `json:"max_steps"`
		RunInBackground bool     `json:"run_in_background"`
		Model           string   `json:"model"`
		Effort          string   `json:"effort"`
		ContinueFrom    string   `json:"continue_from"`
		ForkFrom        string   `json:"fork_from"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if strings.TrimSpace(p.Prompt) == "" {
		return "", fmt.Errorf("prompt is required")
	}

	spec, err := t.buildTaskSpec(ctx, p.Prompt, p.Description, p.Profile, p.WritePaths, p.Tools, p.MaxSteps, p.Model, p.Effort, p.ContinueFrom, p.ForkFrom, p.RunInBackground, false)
	if err != nil {
		return "", err
	}
	return t.RunProfileSpec(ctx, spec)
}

// buildTaskSpec resolves profile, tools, model/effort, and write claims for a
// single task/fleet item. forceReadOnly forces the read-only registry.
func (t *TaskTool) buildTaskSpec(ctx context.Context, prompt, description, profile string, writePaths, tools []string, maxSteps int, model, effort, continueFrom, forkFrom string, background, forceReadOnly bool) (ProfileExecSpec, error) {
	spec := ProfileExecSpec{
		Task:    TaskSpec{Objective: prompt, Description: description},
		Worker:  WorkerSpec{Kind: "task", Name: "task", SystemPrompt: t.sysPrompt},
		Grant:   CapabilityGrant{CallTools: tools},
		Context: ContextRequest{ContinueFrom: strings.TrimSpace(continueFrom), ForkFrom: strings.TrimSpace(forkFrom)},
		Sched:   SchedulerPolicy{MaxSteps: maxSteps, RunInBackground: background, Nested: SubagentDepth(ctx) > 0},
	}
	profile = strings.TrimSpace(profile)
	readOnly := forceReadOnly
	var profileTools []string
	var profileModel, profileEffort string
	if profile != "" {
		def, err := ResolveProfileDefinition(t.profileLookup, profile)
		if err != nil {
			return ProfileExecSpec{}, err
		}
		spec.Worker.Profile = def.Name
		spec.Worker.Name = def.Name
		spec.Worker.Kind = "skill"
		spec.Worker.SystemPrompt = def.Body
		spec.Worker.UseProfilePrompt = true
		profileTools = def.AllowedTools
		profileModel, profileEffort = def.Model, def.Effort
		if def.ReadOnly {
			readOnly = true
		}
	}
	spec.Grant.ReadOnly = readOnly
	spec.Grant.ProfileTools = profileTools

	configModel, configEffort := "", ""
	if profile != "" {
		if t.profileConfigModel != nil {
			configModel = t.profileConfigModel(profile)
		}
		if t.profileConfigEffort != nil {
			configEffort = t.profileConfigEffort(profile)
		}
	}
	spec.Worker.Model, spec.Worker.Effort = ResolveModelEffort(
		configModel, configEffort,
		model, effort,
		profileModel, profileEffort,
		t.subagentModel, t.subagentEffort,
	)

	if !readOnly {

		requireClaim := t.scheduler != nil || strings.TrimSpace(t.workspaceRoot) != "" || background || len(writePaths) > 0
		claims, err := t.resolveWriterClaims(writePaths, requireClaim)
		if err != nil {
			return ProfileExecSpec{}, err
		}
		spec.Grant.WritePaths = claims
		if requireClaim && claims.Empty() {
			return ProfileExecSpec{}, fmt.Errorf("writer claim resolved empty")
		}
	} else if len(writePaths) > 0 {
		return ProfileExecSpec{}, fmt.Errorf("write_paths is not valid for read-only tasks")
	}
	return spec, nil
}

func (t *TaskTool) resolveWriterClaims(writePaths []string, requireClaim bool) (WritePathSet, error) {
	if len(writePaths) > 0 {
		return NormalizeWritePaths(t.workspaceRoot, writePaths)
	}
	if !requireClaim {
		return WritePathSet{}, nil
	}
	return WholeWorkspaceWriteClaim(t.workspaceRoot)
}

// RunProfileSpec executes a unified profile/task specification. Shared by task,
// fleet items, and boot-wired skill runners so prompt, tools, claims, and
// scheduling cannot drift across entry points.
func (t *TaskTool) RunProfileSpec(ctx context.Context, spec ProfileExecSpec) (result string, err error) {
	if t == nil {
		return "", fmt.Errorf("task tool is not configured")
	}

	trk := newSubagentProgressTracker(ctx, subSink(ctx))
	backgroundHandoff := false
	defer func() {
		if backgroundHandoff {
			return
		}
		if p := recover(); p != nil {
			trk.finish(nil, fmt.Errorf("panic: %v", p))
			panic(p)
		}
		trk.finish(ctx.Err(), err)
	}()
	if !spec.Sched.RunInBackground {
		trk.running()
	}
	if strings.TrimSpace(spec.Task.Objective) == "" {
		return "", fmt.Errorf("prompt is required")
	}
	if strings.TrimSpace(spec.Worker.SystemPrompt) == "" {
		if spec.Worker.UseProfilePrompt {
			return "", fmt.Errorf("profile system prompt is empty")
		}
		spec.Worker.SystemPrompt = t.sysPrompt
	}

	maxSteps := t.childMaxStepsForContext(ctx, spec.Sched.MaxSteps)
	childDepth, err := t.nextSubagentDepth(ctx)
	if err != nil {
		return "", err
	}

	toolNames, err := IntersectToolLists(t.parentReg, spec.Grant.ProfileTools, spec.Grant.CallTools)
	if err != nil {
		return "", err
	}
	var subReg *tool.Registry
	if spec.Grant.ReadOnly {
		subReg = ReadOnlySubagentToolRegistryForDepthWithRuntime(t.parentReg, toolNames, childDepth, t.maxDepth(), t.capabilityRuntime)
		if subReg.Len() == 0 && !spec.Grant.AllowNoTools {
			return "", fmt.Errorf("no read-only tools available for this sub-agent")
		}
	} else {
		subReg = t.buildSubReg(toolNames, childDepth)

		if !spec.Grant.WritePaths.Empty() && !spec.Grant.WritePaths.WholeWorkspace {
			keepBash := t.bashCanEnforceWriteRoots()
			bound, removed := BindWritePaths(subReg, spec.Grant.WritePaths, t.workspaceRoot, keepBash)
			subReg = bound
			if len(removed) > 0 && subReg.Len() == 0 {
				return "", fmt.Errorf("no path-bound write tools available after dropping unbound writers: %s", strings.Join(removed, ", "))
			}
		}
	}

	modelRef, effortRef := spec.Worker.Model, spec.Worker.Effort
	usageModelRef := t.usageModelRef(modelRef, effortRef)
	parentID, _, _, _ := CallContext(ctx)
	run, err := t.prepareTranscriptRunWithPrompt(ctx, subReg, modelRef, effortRef, spec.Context.parentSession(ctx), parentID, spec.Context.ContinueFrom, spec.Context.ForkFrom, spec.Worker.SystemPrompt, spec.Worker.Kind, spec.Worker.Name)
	if err != nil {
		return "", err
	}
	prov, pricing, ctxWin, err := t.resolveSubSessionRuntime(modelRef, effortRef)
	if err != nil {
		run.Release()
		return "", fmt.Errorf("sub-agent profile: %w", err)
	}

	isWriter := !spec.Grant.ReadOnly
	acquireReq := AcquireRequest{
		Writer:     isWriter,
		WritePaths: spec.Grant.WritePaths,
		Nested:     spec.Sched.Nested,
		Label:      firstNonEmpty(spec.Task.Description, spec.Worker.Name, "task"),
	}

	if isWriter && spec.Grant.WritePaths.Empty() && spec.Sched.RunInBackground {
		whole, werr := WholeWorkspaceWriteClaim(t.workspaceRoot)
		if werr != nil {
			run.Release()
			return "", werr
		}
		acquireReq.WritePaths = whole
		spec.Grant.WritePaths = whole
	}

	recoveryTaskID := subagentRecoveryTaskID(ctx, run.Ref)
	backgroundWriter := (spec.Sched.RunInBackground || spec.Sched.BackgroundWriter) && !spec.Grant.ReadOnly
	var mutationObserver *checkpoint.MutationObserver
	if t.mutationObserver != nil {
		turn := t.mutationObserver.OwnershipTurn()
		mutationObserver = t.mutationObserver.CloneForSubagent(recoveryTaskID, turn, backgroundWriter)
	}
	runSession := func(runCtx context.Context, sink event.Sink, writerAlreadyRegistered bool) (string, error) {
		if mutationObserver != nil && backgroundWriter && !writerAlreadyRegistered {
			turn := mutationObserver.OwnershipTurn()
			if err := mutationObserver.RegisterWriter(recoveryTaskID, "background_subagent", turn); err != nil {
				return "", err
			}
			defer mutationObserver.UnregisterWriter(recoveryTaskID)
		}
		if spec.Grant.ReadOnly {
			return t.runReadOnlySubSession(runCtx, spec.Task.Objective, subReg, sink, maxSteps, prov, pricing, ctxWin, run.Session, childDepth, recoveryTaskID, usageModelRef, mutationObserver)
		}
		return t.runSubSession(WithSubagentWriteClaim(runCtx, spec.Grant.WritePaths), spec.Task.Objective, subReg, sink, maxSteps, prov, pricing, ctxWin, run.Session, childDepth, recoveryTaskID, usageModelRef, mutationObserver)
	}

	if spec.Sched.RunInBackground {
		jm, ok := jobs.FromContext(ctx)
		if !ok {
			run.Release()
			return "", fmt.Errorf("background execution is not available in this context")
		}
		// Legacy hard-cap remains only when no scheduler is attached. With a
		// scheduler, return the job immediately and queue for a slot inside the
		// job so the parent turn is not blocked at concurrency limits.
		var releaseStart func()
		if t.scheduler == nil {
			var running int
			var okReserve bool
			releaseStart, running, okReserve = jm.ReserveStartForSession(jobs.SessionFromContext(ctx), "task", maxConcurrentBackgroundTasks)
			if !okReserve {
				run.Release()
				return "", fmt.Errorf("%d background tasks are already running for this session (limit %d); collect their results with wait — or run this sub-task in the foreground — before starting more", running, maxConcurrentBackgroundTasks)
			}
			defer releaseStart()
		} else {
			releaseStart = func() {}
		}
		label := firstNonEmpty(spec.Task.Description, spec.Worker.Name, "task")
		if t.transcripts != nil && run != nil && run.Ref != "" {
			if err := t.transcripts.MarkRunning(run); err != nil {
				releaseStart()
				run.Release()
				return "", err
			}
		}
		writerRegistered := false
		if mutationObserver != nil && backgroundWriter {
			turn := mutationObserver.OwnershipTurn()
			if err := mutationObserver.RegisterWriter(recoveryTaskID, "background_subagent", turn); err != nil {
				releaseStart()
				run.Release()
				return "", errors.Join(err, t.transcripts.SaveFailed(run))
			}
			writerRegistered = true
		}
		parentSession := ParentSession(ctx)
		backgroundEvidence := evidence.NewLedger()

		slotReq := acquireReq

		trk.queued()
		job := jm.StartForSession(jobs.SessionFromContext(ctx), "task", label, func(jobCtx context.Context, _ io.Writer) (result string, err error) {
			if writerRegistered {
				defer mutationObserver.UnregisterWriter(recoveryTaskID)
			}
			jobCtx = WithParentSession(jobCtx, parentSession)
			jobCtx = evidence.WithLedger(jobCtx, backgroundEvidence)
			defer run.Release()
			defer func() { jobs.PublishEvidence(jobCtx, backgroundEvidence.Summary()) }()
			defer func() {
				if r := recover(); r != nil {
					panicErr := fmt.Errorf("internal error: panic: %v\n%s", r, debug.Stack())
					result = FormatSubagentRunResult("", run, true)
					err = errors.Join(panicErr, t.transcripts.SaveFailed(run))
				}

				trk.finish(jobCtx.Err(), err)
			}()

			releaseSlot, slotErr := t.acquireSlot(jobCtx, slotReq)
			if slotErr != nil {
				return FormatSubagentRunResult("", run, true), errors.Join(slotErr, t.transcripts.SaveFailed(run))
			}
			defer releaseSlot()
			trk.running()
			answer, err := runSession(jobCtx, trk.wrap(), writerRegistered)
			if err != nil {
				return FormatSubagentRunResult("", run, true), errors.Join(err, t.transcripts.SaveFailed(run))
			}
			if err := t.transcripts.SaveCompleted(run); err != nil {
				return FormatSubagentRunResult("", run, true), errors.Join(err, t.transcripts.SaveFailed(run))
			}
			return FormatSubagentRunResult(answer, run, false), nil
		})
		releaseStart()

		backgroundHandoff = true
		queuedNote := ""
		if t.scheduler != nil {
			queuedNote = " It may wait in the session queue until a concurrency/write slot is free."
		}
		if run != nil && run.Ref != "" {
			return fmt.Sprintf("Started background task %q (%s).%s\n%s\nIt runs across turns; collect its final answer with wait (or wait will return it once done), and you'll be notified when it finishes.", job.ID, label, queuedNote, FormatSubagentReference(run)), nil
		}
		return fmt.Sprintf("Started background task %q (%s).%s It runs across turns; collect its final answer with wait (or wait will return it once done), and you'll be notified when it finishes.", job.ID, label, queuedNote), nil
	}

	releaseSlot, err := t.acquireSlot(ctx, acquireReq)
	if err != nil {
		run.Release()
		return "", err
	}
	defer releaseSlot()
	defer run.Release()
	answer, err := runSession(ctx, trk.wrap(), false)
	if err != nil {
		return "", errors.Join(err, t.transcripts.SaveFailed(run))
	}
	if t.transcripts != nil && run.Ref != "" {
		if err := t.transcripts.SaveCompleted(run); err != nil {
			return "", errors.Join(err, t.transcripts.SaveFailed(run))
		}
		return FormatSubagentRunResult(answer, run, false), nil
	}
	return GuardSubagentHostDecisionText(answer), nil
}

func (t *TaskTool) acquireSlot(ctx context.Context, req AcquireRequest) (func(), error) {
	noop := func() {}
	if t.scheduler == nil {
		return noop, nil
	}
	return t.scheduler.Acquire(ctx, req)
}

func (t *TaskTool) bashCanEnforceWriteRoots() bool {
	if t != nil && t.bashSandboxEnforced != nil {
		return t.bashSandboxEnforced()
	}
	return false
}

func (t *TaskTool) prepareTranscriptRunWithPrompt(ctx context.Context, subReg *tool.Registry, modelRef, effortRef, parentSession, parentID, continueFrom, legacyForkFrom, systemPrompt, kind, name string) (*SubagentRun, error) {
	continueFrom = strings.TrimSpace(continueFrom)
	legacyForkFrom = strings.TrimSpace(legacyForkFrom)
	parentSession = strings.TrimSpace(parentSession)
	if continueFrom != "" && legacyForkFrom != "" {
		return nil, fmt.Errorf("continue_from and fork_from are mutually exclusive; pass only continue_from")
	}
	if t.transcripts == nil {
		return nil, fmt.Errorf("subagent transcript store is required")
	}
	if systemPrompt == "" {
		systemPrompt = t.sysPrompt
	}
	if kind == "" {
		kind = "task"
	}
	if name == "" {
		name = "task"
	}
	if parentSession == "" {
		if continueFrom != "" || legacyForkFrom != "" {
			return nil, fmt.Errorf("subagent continuation requires a persisted session; none is active in this run")
		}
		return EphemeralSubagentRun(systemPrompt), nil
	}
	identityModel, identityEffort := t.effectiveIdentity(modelRef, effortRef)
	spec := SubagentSpec{
		Kind:             kind,
		Name:             name,
		WorkspaceRoot:    t.workspaceRoot,
		ParentSession:    parentSession,
		ParentToolCallID: parentID,
		SystemPrompt:     systemPrompt,
		Registry:         subReg,
		ToolContext:      childToolIdentityContext(ctx),
		Model:            identityModel,
		Effort:           identityEffort,
		ResumedFrom:      firstNonEmpty(continueFrom, legacyForkFrom),
	}
	if continueFrom != "" {
		return t.transcripts.PrepareContinue(continueFrom, spec)
	}
	if legacyForkFrom != "" {
		return t.transcripts.PrepareLegacyForkFrom(legacyForkFrom, spec)
	}
	return t.transcripts.PrepareFresh(spec)
}

func childToolIdentityContext(ctx context.Context) context.Context {
	ctx = tool.WithoutGoalTurnRecorder(ctx)
	ctx = memory.WithoutQueue(ctx)
	ctx = jobs.WithoutManager(ctx)
	return planmode.WithActive(ctx, PlanModeFromContext(ctx))
}

func (t *TaskTool) effectiveIdentity(modelRef, effort string) (string, string) {
	if t.identityProfile != nil {
		model, eff := t.identityProfile(modelRef, effort)
		return strings.TrimSpace(model), strings.TrimSpace(eff)
	}
	return t.effectiveModelIdentity(modelRef), t.effectiveEffortIdentity(effort)
}

// usageModelRef returns the canonical provider/model identity of the runtime
// selected for a child. The resolver expands aliases and supplies the parent
// model when no child override is configured.
func (t *TaskTool) usageModelRef(modelRef, effort string) string {
	model, _ := t.effectiveIdentity(modelRef, effort)
	if model != "" {
		return model
	}
	return firstNonEmpty(modelRef, t.baseModel, t.subagentModel)
}

func (t *TaskTool) effectiveModelIdentity(modelRef string) string {
	if strings.TrimSpace(modelRef) != "" {
		return strings.TrimSpace(modelRef)
	}
	return strings.TrimSpace(t.baseModel)
}

func (t *TaskTool) effectiveEffortIdentity(effort string) string {
	if strings.TrimSpace(effort) != "" {
		return strings.TrimSpace(effort)
	}
	return strings.TrimSpace(t.baseEffort)
}

// buildSubReg returns the sub-agent's tool set: the named whitelist (minus
// unavailable sub-agent tools), or every parent tool except those tools.
func (t *TaskTool) buildSubReg(names []string, childDepth int) *tool.Registry {
	return SubagentToolRegistryForDepthWithRuntime(t.parentReg, names, childDepth, t.maxDepth(), t.capabilityRuntime)
}

func (t *TaskTool) maxDepth() int {
	if t == nil {
		return DefaultMaxSubagentDepth
	}
	if t.maxSubagentDepth == 0 {
		return DefaultMaxSubagentDepth
	}
	return NormalizeMaxSubagentDepth(t.maxSubagentDepth)
}

func (t *TaskTool) nextSubagentDepth(ctx context.Context) (int, error) {
	current := SubagentDepth(ctx)
	next := current + 1
	maxDepth := t.maxDepth()
	if next > maxDepth {
		return 0, fmt.Errorf("subagent delegation depth limit reached (max_subagent_depth=%d)", maxDepth)
	}
	return next, nil
}

// FilterRegistry builds a sub-registry from parent: the named whitelist (empty =
// every parent tool), minus any excluded names. Used to scope what a spawned
// sub-agent — a `task` sub-agent or a subagent skill — may call, e.g. excluding
// `task` to bar recursive nesting, or restricting to a skill's allowed-tools.
// Direct MCP tools may be copied here; callers that need a stable MCP surface
// should strip them and attach use_capability via attachSubagentCapabilityProxy.
func FilterRegistry(parent *tool.Registry, names []string, exclude ...string) *tool.Registry {
	sub := tool.NewRegistry()
	if parent == nil {
		return sub
	}
	ex := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		ex[e] = true
	}
	customAllowlist := len(names) > 0
	src := names
	if !customAllowlist {
		src = parent.Names()
	} else {
		src = expandToolPatterns(parent, src)
	}
	for _, name := range src {
		if ex[name] {
			continue
		}

		if strings.HasPrefix(name, "mcp-tool:") || strings.HasPrefix(name, "mcp-server:") {
			continue
		}
		tl, ok := parent.Get(name)
		if !ok {
			continue
		}
		sub.Add(tl)
	}
	return sub
}

// stripDirectMCPTools removes provider-visible mcp__* tools so sub-agents use
// only the stable use_capability proxy for MCP.
func stripDirectMCPTools(reg *tool.Registry) {
	if reg == nil {
		return
	}
	for _, name := range append([]string(nil), reg.Names()...) {
		if strings.HasPrefix(name, tool.MCPNamePrefix) {
			reg.RemovePrefix(name)
		}
	}
}

func (t *restrictedCapabilityProxy) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if err := t.check(args); err != nil {
		return "", err
	}
	out, err := t.Tool.Execute(ctx, args)
	if err != nil {
		return out, err
	}
	var p struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(args, &p)
	if strings.EqualFold(strings.TrimSpace(p.Action), "list") {
		return filterCapabilityListResult(out, t.servers), nil
	}
	return out, nil
}
