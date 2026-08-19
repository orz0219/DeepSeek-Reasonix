package boot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"reasonix/internal/agent"
	"reasonix/internal/billing"
	"reasonix/internal/capability"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/environment"
	"reasonix/internal/event"
	"reasonix/internal/extension/protocol"
	"reasonix/internal/extension/providerext"
	"reasonix/internal/extension/uihub"
	"reasonix/internal/jobs"
	"reasonix/internal/migration"
	"reasonix/internal/netclient"
	"reasonix/internal/outputstyle"
	"reasonix/internal/provider"
	"reasonix/internal/sandbox"
	"reasonix/internal/secrets"
	"reasonix/internal/stats"
	"reasonix/internal/workspacelease"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// prepareBuild performs the pre-assembly stage: runtime binding, config load
// and migration, the sink chain, extension preflight, model resolution, job
// manager, workspace lease, provider/client construction, and the cache-stable
// system prompt. Every object it creates that later stages need lands on the
// returned bootContext.
func prepareBuild(ctx context.Context, opts Options) (*bootContext, error) {
	ctx, opts, owner, fileWriteReceipt := bindRuntimeOwner(ctx, opts)
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	root := resolveWorkspaceRoot(opts.WorkspaceRoot)
	additionalDirs, err := normalizeAdditionalDirs(root, opts.AdditionalDirs)
	if err != nil {
		return nil, err
	}
	// Import v1/v0.5 config before Load so this boot sees the new config + ~/.env.
	// CLI Run also calls this before config-only commands; keep a shared fallback.
	migrated, migErr := config.MigrateLegacyIfNeededForRoot(root)
	deepSeekProtocolMigrated, deepSeekProtocolMigErr := config.MigrateLegacyDeepSeekProtocolUserConfig()
	stepLimitsMigrated, stepLimitMigErr := config.MigrateLegacyAgentStepLimitsForRoot(root)
	redactToolOutputMigrated, redactToolOutputMigErr := config.MigrateLegacyRedactToolOutputForRoot(root)
	multiThresholdMigrated, multiThresholdMigErr := config.MigrateLegacyMultiThresholdCompactionForRoot(root)
	cfg, err := config.LoadForRoot(root)
	if err != nil {
		return nil, err
	}
	deepSeekProtocolMigErr = deepSeekProtocolMigrationNoticeError(handleConfigLoadWarnings(opts, cfg), deepSeekProtocolMigErr)
	// Arm the credential-protection layers from the user-global [secrets]
	// section before any tool or plugin subprocess can spawn. Package
	// globals are correct here because [secrets] is user-global (project
	// reasonix.toml cannot override it), so concurrent workspaces agree.
	secrets.SetFilterSubprocessEnv(cfg.Secrets.FilterSubprocessEnv)
	secrets.SetProtectSensitiveFiles(cfg.Secrets.ProtectSensitiveFiles)
	secrets.RegisterCredentialEnvKeys(cfg.CredentialEnvNames())

	// Serialize the frontend's sink once: background jobs (below) emit from their
	// own goroutines, which can overlap a running turn's emission, so every emitter
	// shares this synchronized sink. It is created before extension preflight so
	// sidecar warnings and host/ui/* publishes land on the same channel as every
	// later notice. The job manager is session-scoped — its jobs outlive a turn
	// and are cancelled by Controller.Close.
	//
	// CostQuote must run before every host consumer (stats recorder, CLI
	// metrics via opts.Sink, ACP/eventwire bridges, Desktop) so all see the
	// same occurrence-time quote. Order from the agent:
	//   Coalesce → GoalUsageTee → Sync → CostQuote → [Recorder] → frontend
	quoteCtx := &event.QuoteContext{
		DisplayRequest: billing.DisplayRequest{
			Currency: cfg.ExplicitDisplayCurrency(),
			Source:   billing.DisplaySourceExplicit,
		},
		BillingModeForModel: func(modelRef string) string {
			entry, ok := cfg.ResolveModel(modelRef)
			if !ok {
				return ""
			}
			return entry.ProviderBillingMode()
		},
	}
	// Innermost: frontend sink (CLI metrics/ACP/Desktop bridge live here).
	quoted := opts.Sink
	// Record billable usage after quoting so history JSONL can store CostQuote.
	if source := strings.TrimSpace(opts.StatsSource); source != "" {
		quoted = stats.NewRecorder(quoted, config.StatsDir(), source)
	}
	quoted = event.NewCostQuoteSink(quoted, quoteCtx)
	sink := event.Sync(quoted)

	// Both sink wraps must complete BEFORE the extension UI hub closes over the
	// sink variable: a sidecar publish during preflight lands on this closure
	// from a wire-handler goroutine, and any later reassignment races it.
	// Goal token-budget accounting: the controller detects this tee and
	// attributes billable usage to the active goal turn's recorder. Both the
	// tee and the delta coalescer must ride the shared sink agents emit into
	// directly — wrapping only the controller's reference would leave the
	// executor's per-chunk Text/Reasoning stream uncoalesced.
	sink = control.NewGoalUsageTee(event.Coalesce(sink, event.DefaultStreamDeltaWindow))

	// Extension preflight (stages 5b/7): start the installed, enabled v2 runtime
	// packages ONCE, here, before model resolution, so plugin-namespaced refs
	// (plugin/<plugin>/<provider>/<model>) resolve on the very first boot and the
	// same sidecar generation feeds the executor, planner, guardian, sub-agents,
	// the snapshot assembly, and the frontend catalog. With no runtime package
	// installed preflight is a no-op and the whole build below takes the
	// untouched pre-sidecar path. The generation moves up with it: the sidecar
	// handshake's session context carries this build's generation, and a fresh
	// controller has no session path yet, so the session ID is generation-scoped
	// (the handshake only requires a stable, non-empty identity).
	generation := nextRuntimeGeneration()
	sessionID := fmt.Sprintf("boot-%d", generation)
	proxySpec := cfg.NetworkProxySpec()
	extWarn := func(msg string) {
		redacted := secrets.RedactCredentials(msg)
		slog.Warn("boot: extension runtime: "+redacted, "root", root)
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: redacted})
	}
	// Stage 8a: the host extension UI hub serves every sidecar's host/ui/* calls
	// for this generation — publications become frontend events through the
	// controller sink, blocking prompts ride the controller's Ask channel. The
	// controller only exists after control.New below, so both seams indirect
	// through ctrlRef; traffic before that (a sidecar publishing during its
	// handshake) falls back to the same sink directly, matching the emission the
	// controller would have made.
	var ctrlRef atomic.Pointer[control.Controller]
	// Readiness signals for gateExtensionUIRequest: a sidecar may legally
	// issue host/ui/request right after extension/initialized, before the
	// controller exists. ready closes at ctrlRef.Store; failed closes on any
	// build error before the RuntimeSet takes ownership (the pendingMgr defer
	// below), so a startup request never hangs a dying build.
	controllerReady := make(chan struct{})
	controllerBuildFailed := make(chan struct{})
	extUIHub := uihub.New(uihub.Options{
		SessionID:  sessionID,
		Generation: generation,
		Owner:      owner,
		Emit: func(ev event.Event) {
			if c := ctrlRef.Load(); c != nil {
				c.EmitExtensionEvent(ev)
				return
			}
			sink.Emit(ev)
		},
		Request: func(reqCtx context.Context, req uihub.HubRequest) (map[string]any, bool, error) {
			return gateExtensionUIRequest(reqCtx, ctrlRef.Load, controllerReady, controllerBuildFailed,
				func(c *control.Controller) (map[string]any, bool, error) {
					return uihub.AskRequestFunc(c.Ask)(reqCtx, req)
				})
		},
		Warn: func(msg string) {
			slog.Warn("boot: extension UI hub: "+msg, "root", root)
		},
	})
	extensionMgr, err := preflightExtensionRuntimes(ctx, config.ReasonixHomeDir(), extensionBoot{
		session:   protocol.SessionContext{SessionID: sessionID, WorkspaceRoot: root, Generation: generation},
		ui:        extUIHub,
		onWarning: extWarn,
	}, opts.Extensions, planForPreflight(opts, generation))
	if err != nil {
		return nil, fmt.Errorf("boot: %w", err)
	}
	// Until the RuntimeSet takes ownership at snapshot assembly, every error
	// path between here and there must retire the preflighted sidecars — no
	// process may outlive a failed build.
	pendingMgr := extensionMgr

	// The build's provider resolution base: the caller-owned broker when
	// injected, the local config-backed resolver otherwise. When a started
	// sidecar declares providers, fold them in NOW (stage 7) with the
	// provider:<ref> slot claims from the same manifest data the kernel's
	// ReplaceClaims pass uses, so first-boot model resolution sees them. A
	// conflict with the base catalog that lacks the plugin's claim is fatal,
	// the same class as a required runtime that cannot start: booting without
	// the declared provider would silently change what the session is.
	baseResolver := opts.ProviderResolver
	if baseResolver == nil {
		baseResolver = NewLocalProviderResolver(cfg, proxySpec)
	}
	effectiveResolver := opts.ProviderResolver
	var extensionResolver provider.Resolver
	if extensionMgr != nil {
		declares := false
		for _, client := range extensionMgr.Clients() {
			if len(client.Handshake().Providers) > 0 {
				declares = true
				break
			}
		}
		if declares {
			claims, claimsErr := resolveReplacementClaims(extensionMgr.Contributions())
			if claimsErr != nil {
				return nil, fmt.Errorf("boot: %w", claimsErr)
			}
			merged, mergeErr := mergeSidecarProviders(baseResolver, extensionMgr, claims, owner)
			if mergeErr != nil {
				return nil, fmt.Errorf("boot: %w", mergeErr)
			}
			installSidecarStreamRouters(extensionMgr, merged)
			effectiveResolver = merged
			extensionResolver = merged
		}
	}

	// Fall through a keyless default_model to the next configured chat model
	// instead of hard-failing every command on "missing env X_API_KEY" (issue
	// #6996). The fallback only kicks in when the caller did not pass an
	// explicit opts.Model; explicit choices still fail loudly.
	modelName := opts.Model
	if modelName == "" {
		if resolved, _, ok := cfg.ResolveNewSessionChatModel(); ok {
			modelName = resolved
		}
	}
	config.NormalizeLegacyMimoCustomProvidersForRefs(cfg, modelName)
	agentPreset := strings.TrimSpace(opts.AgentPreset)
	if agentPreset == "" {
		agentPreset = AgentPresetFromTokenMode(opts.TokenMode)
	}
	agentPreset = NormalizeAgentPreset(agentPreset)
	// tokenMode is dual-write compatibility only; policy uses agentPreset.
	tokenMode := TokenModeFromAgentPreset(agentPreset)
	tokenDelivery := agentPreset == AgentPresetDelivery
	runtimeProfile := capability.ProfileBalanced
	switch agentPreset {
	case AgentPresetLight:
		runtimeProfile = capability.ProfileEconomy
	case AgentPresetDelivery:
		runtimeProfile = capability.ProfileDelivery
	}
	keepPolicy := agentKeepPolicy(cfg.Agent.Keep)
	// Entry resolution: the caller-owned broker is authoritative for every
	// ref; the extension-merged resolver only owns plugin refs — a config ref
	// keeps the full config entry (kind, endpoint, credentials, balance URL,
	// missing-key notice), exactly as without extensions installed.
	entryResolver := opts.ProviderResolver
	if entryResolver == nil && extensionResolver != nil && providerext.PluginRefOwner(modelName) != "" {
		entryResolver = extensionResolver
	}
	entry, modelRef, err := resolveModelEntry(entryResolver, cfg, modelName)
	if err != nil {
		return nil, err
	}
	if opts.EffortOverride != nil {
		entry.Effort = *opts.EffortOverride
		if entry.Kind == "anthropic" && strings.TrimSpace(entry.Effort) != "" && strings.TrimSpace(entry.Thinking) == "" {
			entry.Thinking = "adaptive"
		}
	}
	// RequireKey fails fast on a missing credential (run/serve); plugin-
	// namespaced refs carry no config credential — the extension provider holds
	// its own keys — so the merged resolver's resolution is their only gate.
	if opts.RequireKey && opts.ProviderResolver == nil && providerext.PluginRefOwner(modelName) == "" {
		if err := cfg.Validate(modelName); err != nil {
			return nil, err
		}
	}

	if migErr != nil {
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "Config migration did not complete.", Detail: "config migration from ~/.reasonix failed: " + migErr.Error()})
	} else if migrated != nil {
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: migrated.Notice()})
	}
	if deepSeekProtocolMigrated {
		sink.Emit(event.Event{
			Kind:   event.Notice,
			Level:  event.LevelInfo,
			Text:   "DeepSeek official access was upgraded to Anthropic Messages.",
			Detail: "Your unmodified legacy OpenAI Chat Completions configuration now uses DeepSeek's recommended Anthropic endpoint with server-side web search. Existing model names and pricing were preserved. The first request starts a new provider cache prefix; later requests rebuild normal prefix-cache reuse.",
		})
	} else if deepSeekProtocolMigErr != nil {
		sink.Emit(event.Event{
			Kind:   event.Notice,
			Level:  event.LevelWarn,
			Text:   "DeepSeek protocol migration did not complete.",
			Detail: deepSeekProtocolMigErr.Error(),
		})
	}
	if stepLimitsMigrated || cfg.IgnoredLegacyAgentStepLimits() {
		level := event.LevelInfo
		text := "Deprecated agent step limits were removed."
		detail := "[agent].max_steps and planner_max_steps are no longer used; Reasonix now manages interactive progress automatically. " +
			"Use the CLI --max-steps flag for a one-off run."
		if stepLimitMigErr != nil {
			level = event.LevelWarn
			text = "Deprecated agent step limits were ignored."
			detail += " The old keys were ignored but could not be removed: " + stepLimitMigErr.Error()
		}
		sink.Emit(event.Event{
			Kind:   event.Notice,
			Level:  level,
			Text:   text,
			Detail: detail,
		})
	} else if stepLimitMigErr != nil {
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "Deprecated agent step-limit migration did not complete.", Detail: stepLimitMigErr.Error()})
	}
	if redactToolOutputMigrated || redactToolOutputMigErr != nil {
		level := event.LevelInfo
		text := "Deprecated redact_tool_output setting was removed."
		detail := "[secrets].redact_tool_output no longer has any effect: ordinary model/tool content and local session/job artifacts now preserve their original text. Explicit diagnostics and reasonix doctor redact-sessions still redact credential values."
		if redactToolOutputMigErr != nil {
			level = event.LevelWarn
			text = "Deprecated redact_tool_output setting was ignored."
			detail += " The old key could not be removed: " + redactToolOutputMigErr.Error()
		}
		sink.Emit(event.Event{Kind: event.Notice, Level: level, Text: text, Detail: detail})
	}
	if multiThresholdMigrated || multiThresholdMigErr != nil {
		level := event.LevelInfo
		text := "上下文维护已简化为单一自动压缩阈值。"
		detail := "Context maintenance now uses a single automatic compact_ratio (default 0.85). soft_compact_ratio, tool_result_snip_ratio, compact_force_ratio, cold_resume_prune, and context_editing were removed from config."
		if multiThresholdMigErr != nil {
			level = event.LevelWarn
			text = "Deprecated multi-threshold compaction keys were ignored."
			detail += " The old keys could not be removed: " + multiThresholdMigErr.Error()
		}
		sink.Emit(event.Event{Kind: event.Notice, Level: level, Text: text, Detail: detail})
	}
	migration.MigrateLegacySessionSources(sink)
	if ignored := cfg.IgnoredProjectDefaultModel(); ignored != "" {
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "Ignored the project config's default_model.", Detail: fmt.Sprintf("./reasonix.toml sets default_model = %q but no configured provider serves it; using %q from your user config instead. Edit or remove that default_model line to silence this notice.", ignored, cfg.DefaultModel)})
	}

	// A resolvable model whose API key env is unset would otherwise build fine
	// (RequireKey is false so the UI stays reachable) and then fail silently on the
	// first request, showing as an empty/dead model. Surface the cause up front.
	if !opts.RequireKey && entry.RequiresAPIKey() && entry.APIKey() == "" {
		sink.Emit(event.Event{Kind: event.Notice, Text: "Selected model is missing its API key.", Detail: fmt.Sprintf("model %q is selected but its API key %s is not set — requests will fail until you set it", modelName, entry.APIKeyEnv)})
	}
	// Every role setting lazily acquires a workspace write lease on the first
	// real writer. Read-only turns never take the lease.
	var workspaceLease *workspacelease.Owner
	jobOptions := []jobs.Option{
		jobs.WithStalledWarningAfter(time.Duration(cfg.BackgroundJobStalledWarningSeconds()) * time.Second),
		jobs.WithSessionOwnershipProbe(agent.SessionLeaseHeldByCurrentRuntime),
	}
	workspaceLease, err = workspacelease.New(root, config.WorkspaceLeaseDir(), func() {
		sink.Emit(event.Event{
			Kind:   event.Notice,
			Level:  event.LevelInfo,
			Code:   event.NoticeCodeWorkspaceLease,
			Text:   "Another session is writing to this workspace; this session will continue automatically when it is safe.",
			Detail: "workspace write lease is busy; read-only work remains concurrent",
		})
	})
	if err != nil {
		return nil, fmt.Errorf("initialize workspace write lease: %w", err)
	}
	jobOptions = append(jobOptions, jobs.WithJobStartObserver(workspaceLease.RetainUntil))
	jm := jobs.NewManager(sink, jobOptions...)
	sessionDir := opts.SessionDir
	if sessionDir == "" {
		sessionDir = config.SessionDir()
	}
	reconcileCleanupPending := opts.CleanupPendingReconciler
	if reconcileCleanupPending == nil {
		reconcileCleanupPending = control.ReconcileCleanupPending
	}
	if err := reconcileCleanupPending(sessionDir); err != nil {
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "cleanup-pending reconciliation failed: " + err.Error()})
	}

	// proxySpec was computed during extension preflight (the merged resolver's
	// local base needs it); validate it before any provider construction.
	if err := netclient.Validate(proxySpec); err != nil {
		return nil, err
	}
	balanceClient, err := netclient.NewHTTPClient(proxySpec, netclient.TransportOptions{})
	if err != nil {
		return nil, err
	}
	execProv, err := resolveProvider(effectiveResolver, cfg, proxySpec, provider.Selection{Ref: modelRef, Effort: opts.EffortOverride})
	if err != nil {
		return nil, err
	}
	shell := sandbox.ResolveShell(cfg.Tools.Shell.Prefer, cfg.Tools.Shell.Path, stderr)

	sysPrompt, err := cfg.ResolveSystemPromptForRoot(root)
	if err != nil {
		if !config.IsMissingSystemPromptFile(err) {
			return nil, err
		}
		// A stale missing prompt file must not block startup: warn and fall back
		// to the inline (or built-in default) system prompt. Other read failures
		// stay fatal so Reasonix never runs without explicitly configured policy.
		sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: err.Error() + "; falling back to inline/default system prompt"})
		sysPrompt = cfg.InlineSystemPrompt()
	}
	// Output style: fold the selected persona/tone block into the base prompt
	// before language/memory/skills append, so a "replace" style (keep-coding
	// false) still keeps those. Applied once, into the cache-stable prefix.
	if st, ok := outputstyle.Resolve(cfg.Agent.OutputStyle, outputstyle.Dirs()); ok {
		sysPrompt = outputstyle.Apply(sysPrompt, st)
	}
	sysPrompt = appendCorePolicies(sysPrompt)
	if workspaceLine := currentWorkspacePromptLine(root); workspaceLine != "" {
		sysPrompt += "\n\n" + workspaceLine
	}
	// Role settings no longer inject mode-specific system prompts. Planning,
	// verification, and review intensity travel in the per-turn transient
	// <execution-policy> user block so the cache-stable prefix stays shared.
	_ = tokenMode
	if cfg.EnvironmentEnabled() {
		shellLabel := shell.Kind.String()
		if strings.TrimSpace(cfg.Tools.Shell.Path) != "" {
			shellLabel = shell.Path
		}
		envSection := environment.FormatSection(
			environment.RunProbesWithOptions(ctx, environment.DefaultProbes(), environment.ProbeOptions{
				Overrides: cfg.Environment.Tools,
				DenyRoots: []string{root},
				// Persist probe results across restarts: the section below sits
				// inside the provider-cached prompt prefix, and re-observing
				// per boot let transient probe flaps (timeouts, PATH drift)
				// rewrite the prefix and cold-start every session's cache.
				SnapshotDir: config.CacheDir(),
			}),
			runtime.GOOS+"/"+runtime.GOARCH,
			shellLabel,
			cfg.Environment.Tools,
		)
		if envSection != "" {
			sysPrompt += "\n\n" + envSection
		}
	}
	sysPrompt = appendOfflineEnvironmentNote(sysPrompt, cfg.Environment.Offline)

	// Persistent memory (REASONIX.md / AGENTS.md hierarchy + auto-memory index)
	// folds into the system prompt exactly here, once: it becomes part of the

	return &bootContext{
		stderr: stderr, root: root, additionalDirs: additionalDirs, fileWriteReceipt: fileWriteReceipt,
		cfg: cfg, sink: sink, generation: generation, sessionID: sessionID, proxySpec: proxySpec,
		ctrlRef: &ctrlRef, controllerReady: controllerReady, controllerBuildFailed: controllerBuildFailed,
		extUIHub: extUIHub, extensionMgr: extensionMgr, pendingMgr: pendingMgr,
		baseResolver: baseResolver, effectiveResolver: effectiveResolver, extensionResolver: extensionResolver,
		modelName: modelName, modelRef: modelRef, entry: entry, agentPreset: agentPreset,
		tokenDelivery: tokenDelivery, runtimeProfile: runtimeProfile, keepPolicy: keepPolicy,
		workspaceLease: workspaceLease, jm: jm, sessionDir: sessionDir, balanceClient: balanceClient,
		execProv: execProv, shell: shell, sysPrompt: sysPrompt, owner: owner,
		entryResolver: entryResolver, reconcileCleanupPending: reconcileCleanupPending,
		extWarn: extWarn,
	}, nil
}
