package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"reasonix/internal/ablation"
	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/i18n"
	"reasonix/internal/notify"
	"reasonix/internal/serve"
	"reasonix/internal/telemetry"

	"github.com/spf13/pflag"
)

func runAgent(args []string, version string) int {
	defer closeCLIUsageCatalogs()
	fs := pflag.NewFlagSet("run", pflag.ContinueOnError)
	fs.SetInterspersed(true)
	model := fs.String("model", "", "provider name (default: config default_model)")
	profileFlag := fs.String("profile", "", "deprecated: use --preset (economy|balanced|delivery)")
	presetFlag := fs.String("preset", "balanced", "agent execution setting: light | balanced | delivery")
	maxSteps := fs.Int("max-steps", 0, "one-off max tool-call rounds (0 = automatic)")
	showThinking := fs.Bool("show-thinking", false, "show thinking text instead of the collapsed thinking marker")
	metricsPath := fs.String("metrics", "", "write a JSON token/cache/cost summary of the run to this path")
	trajectoryPath := fs.String("trajectory", "", "append a timestamped JSONL trajectory of the run's full event stream (tool calls, reasoning, decisions) to this path")
	ablateFlag := fs.String("ablate", "", "benchmark arm: comma-separated subsystems to switch off (evidence, planner, subagent, retrieval, compaction; none|all)")
	dir := fs.String("dir", "", "change to this directory first (project root); config, sandbox and file tools resolve from here")
	cont := registerContinueFlag(fs)
	resume := fs.String("resume", "", "resume by session file path, session ID, or machine session ID (takes precedence over --continue)")
	copySession := fs.Bool("copy", false, "with --resume/--continue: duplicate the session and continue in the copy (escape hatch when the original is held by another Reasonix process)")
	effort := fs.String("effort", "", "session reasoning effort override")
	permissionMode := fs.String("permission-mode", "ask", "permission mode: manual | ask | auto | acceptEdits | dontAsk | plan | bypassPermissions")
	autoApprove := fs.BoolP("auto", "y", false, "explicitly auto-approve ordinary writer fallbacks (alias for --permission-mode auto)")
	printOnly := fs.BoolP("print", "p", false, "print only the final response")
	eventsJSONL := fs.Bool("events-jsonl", false, "emit a redacted structured event stream as JSONL")
	outputFormat := fs.String("output-format", "text", "output format: text | json | stream-json")
	var additionalDirs []string
	fs.StringArrayVar(&additionalDirs, "add-dir", nil, "allow tool access to an additional directory (repeatable)")
	var allowedToolValues []string
	fs.StringArrayVar(&allowedToolValues, "allowed-tools", nil, "comma or space-separated permission rules to allow")
	fs.StringArrayVar(&allowedToolValues, "allowedTools", nil, "alias for --allowed-tools")
	if code, ok := parseCommandFlags(fs, args); !ok {
		return code
	}
	resolvedPermissionMode, err := resolveRunPermissionMode(*permissionMode, *autoApprove, fs.Changed("permission-mode"))
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	*permissionMode = resolvedPermissionMode
	allowedTools, err := splitAllowedToolRules(allowedToolValues)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	format, err := parseRunOutputFormat(*outputFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	if *eventsJSONL {
		if fs.Changed("output-format") {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "--events-jsonl cannot be combined with --output-format")
			return 2
		}
		format = runOutputEventsJSONL
	}
	profileRaw := strings.TrimSpace(*profileFlag)
	if profileRaw != "" {
		fmt.Fprintln(os.Stderr, "warning: --profile is deprecated; use --preset light|balanced|delivery")
	} else {
		profileRaw = strings.TrimSpace(*presetFlag)
	}
	profile, err := parseRuntimeProfile(profileRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	ablated, err := ablation.Parse(*ablateFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	permissions, err := parsePermissionMode(*permissionMode)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	if permissions.plan {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "--permission-mode plan requires an interactive session")
		return 2
	}
	allowedTools = uniqueStrings(append(allowedTools, permissions.allow...))
	if rc := chdirTo(*dir); rc != 0 {
		return rc
	}
	workspaceRoot, err := workspaceRootForDir(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	cfg, _ := config.Load()
	configureCLIThemeFromConfigForTTYOutput()

	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		prompt = readStdin()
	}
	if prompt == "" {
		fmt.Fprintln(os.Stderr, i18n.M.UsageRunHint)
		return 2
	}
	var machineIdentityKey []byte
	if format == runOutputEventsJSONL {
		machineIdentityKey, err = loadMachineIdentityKey()
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "machine identity is unavailable")
			return 1
		}
	}

	resumePath := strings.TrimSpace(*resume)
	if resumePath != "" {
		resolved, err := resolveSessionQuery(resolveCLISessionDir(), resumePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		resumePath = resolved
	}
	if resumePath == "" && *cont {
		sessionDir := resolveCLISessionDir()
		reclaimCLIRecoveryBranches(sessionDir)
		session, ok := mostRecentSession(sessionDir)
		if !ok {
			fmt.Fprintln(os.Stderr, i18n.M.NoSessionToResume)
			return 1
		}
		resumePath = session.Path
	}
	if *copySession && resumePath == "" {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "--copy requires --resume or --continue")
		return 2
	}
	if *copySession {
		copied, err := copySessionForWriting(resumePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}

		if format == runOutputText && !*printOnly {
			fmt.Printf("continuing in a session copy: %s\n", copied)
		} else {
			fmt.Fprintf(os.Stderr, "continuing in a session copy: %s\n", copied)
		}
		resumePath = copied
	}
	sessionMode := cliTelemetrySessionMode(*cont, strings.TrimSpace(*resume) != "", *copySession)
	reporter := startCLITelemetry(cfg, telemetry.Options{
		Version: version, Interactive: false, CLIMode: "run", Profile: profile,
		PermissionMode: *permissionMode, SessionMode: sessionMode,
	})

	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	var resumeSession *agent.Session
	if resumePath != "" {
		if err := leases.Rebind(resumePath); err != nil {
			if errors.Is(err, agent.ErrSessionLeaseHeld) {
				fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, sessionLeaseResumeRefusal(err))
			} else {
				fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			}
			return 1
		}
		var err error
		resumeSession, err = loadResumableSession(resumePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	started := time.Now()

	chain, err := buildRunSink(format, *printOnly, *showThinking, *metricsPath, *trajectoryPath, cfg, reporter)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	sink, resultOutput, metrics := chain.sink, chain.resultOutput, chain.metrics
	if resumePath != "" {
		*model = modelForResumePath(*model, resumePath, cfg)
	}
	var effortOverride *string
	if strings.TrimSpace(*effort) != "" {
		effortOverride = effort
	}

	overrides := cliBuildOverrides{
		Effort:               effortOverride,
		PermissionAllow:      allowedTools,
		AdditionalDirs:       additionalDirs,
		WorkspaceRoot:        workspaceRoot,
		HeadlessApprovalMode: permissions.approval,
		OnSessionRecovered:   cliSessionRecoveredHandler(leases),
		Ablation:             ablated,
	}
	ctrl, err := setupProfileWithOverrides(ctx, *model, *maxSteps, true, sink, profile, overrides)
	if err != nil {
		if resultOutput != nil && format != runOutputText {
			if encodeErr := resultOutput.Finalize("", started, err); encodeErr != nil {
				fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, encodeErr)
			}
			return 1
		}
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	defer ctrl.Close()
	SetTaskJobKiller(ctrlKillerAdapter{ctrl})
	ctrl.ApplyHeadlessApprovalMode(permissions.approval)

	if resumePath != "" {
		ctrl.Resume(resumeSession, resumePath)
	}
	if ctrl.SessionPath() == "" && ctrl.SessionDir() != "" {
		ctrl.SetFreshSessionPath(agent.NewSessionPath(ctrl.SessionDir(), ctrl.Label()))
	}

	if err := rebindCLIControllerAuthority(leases, ctrl); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, control.SessionInUseMessage(err)+"; "+control.SessionLeaseCloseHint)
		return 1
	}
	reclaimCLIRecoveryBranches(ctrl.SessionDir())

	runErr := ctrl.Run(ctx, prompt)
	reporter.RecordRecovery(ctrl.DrainRecoveryMetrics())
	completion := classifyRunCompletion(runErr)
	if cfg != nil {
		notify.SendEvent(newNotificationSender(), cfg.Notifications, event.Event{
			Kind:    event.TurnDone,
			Err:     runErr,
			Outcome: completion.outcome,
		})
	}
	if metrics != nil {

		final := metrics.Snapshot()
		final.DurationMs = time.Since(started).Milliseconds()
		final.Outcome = completion.class
		final.Arm = ablated.Arm()
		if exec := ctrl.Executor(); exec != nil {
			if audit := exec.CapabilityAudit(); audit != nil {
				snap := audit.Snapshot()
				final.MergeCapabilityAuditCounters(
					snap.Routes, snap.RoutedCandidates, snap.RoutedRequire, snap.RoutedPrefer, snap.RoutedSuggest, snap.Declines,
					snap.SemanticRoutes, snap.SemanticFallbacks,
					snap.RequireMissing, snap.RequireRecovered, snap.PreferMissing, snap.PreferRecovered,
					snap.SkillInvocations, snap.SkillFailures, snap.SkillUnavailable,
					snap.MCPInspect, snap.MCPCall, snap.MCPCallFailures,
					snap.ReviewBlocks, snap.SecurityReviewBlocks,
					snap.RouterPromptTokens, snap.RouterCompletionTokens,
					snap.RouterCost, snap.RouterLatencyMs,
				)
			}
		}
		if err := writeMetrics(*metricsPath, final); err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		}
	}
	if chain.trajectory != nil {
		if err := chain.trajectory.Close(); err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		}
	}
	if resultOutput != nil {
		sessionID := runOutputSessionID(format, agent.BranchID(ctrl.SessionPath()), machineIdentityKey)
		if err := resultOutput.Finalize(sessionID, started, runErr); err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
	}
	if runErr != nil {
		reportRunFailure(os.Stderr, format, resultOutput != nil, completion, runErr)
		return completion.exitCode
	}
	return completion.exitCode
}

func runServeWithOptions(args []string, opts serveRunOptions) int {
	if opts.command == "" {
		opts.command = "serve"
	}
	fs := flag.NewFlagSet(opts.command, flag.ContinueOnError)
	model := fs.String("model", "", "provider name (default: config default_model)")
	profileFlag := fs.String("profile", "", "deprecated: use --preset (economy|balanced|delivery)")
	presetFlag := fs.String("preset", "balanced", "agent execution setting: light | balanced | delivery")
	maxSteps := fs.Int("max-steps", 0, "one-off max tool-call rounds (0 = automatic)")
	addr := fs.String("addr", "127.0.0.1:8787", "listen address")
	resume := fs.String("resume", "", "resume a saved session file")
	sessionIDValue := ""
	sessionID := &sessionIDValue
	if opts.command == "web" {
		sessionID = fs.String("session-id", "", "bind a fresh Web session identity (used by /web handoff)")
	}
	authHelp := "auth mode: none, token, or password (default: config/none)"
	if opts.command == "web" {
		authHelp = "auth mode: none, token, or password (default: generated token)"
	}
	auth := fs.String("auth", "", authHelp)
	token := fs.String("token", "", "pre-shared token for auth=token (auto-generated if empty)")
	password := fs.String("password", "", "password for auth=password (use --hash-password to store a hash instead)")
	hashPassword := fs.Bool("hash-password", false, "print a bcrypt hash of --password and exit")
	behindProxy := fs.Bool("behind-proxy", false, "trust X-Forwarded-For / X-Forwarded-Proto headers from a reverse proxy")
	portFile := fs.String("port-file", "", "write the actual bound listen address (host:port) to this file after binding")
	tokenFile := fs.String("token-file", "", "read the auth=token pre-shared token from this file (overrides --token; keeps the secret out of argv)")
	pidFile := fs.String("pid-file", "", "write the server process id to this file")
	openBrowser := fs.Bool("open", opts.openBrowser, "open the Web UI in the default browser")
	noOpen := fs.Bool("no-open", false, "do not open the Web UI in the default browser")
	if code, ok := parseCommandFlags(fs, args); !ok {
		return code
	}
	authExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "auth" {
			authExplicit = true
		}
	})
	if *resume != "" && *sessionID != "" {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "--resume and --session-id cannot be used together")
		return 2
	}
	if *sessionID != "" {
		if err := validateWebSessionID(*sessionID); err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 2
		}
	}
	profileRaw := strings.TrimSpace(*profileFlag)
	if profileRaw != "" {
		fmt.Fprintln(os.Stderr, "warning: --profile is deprecated; use --preset light|balanced|delivery")
	} else {
		profileRaw = strings.TrimSpace(*presetFlag)
	}
	profile, err := parseRuntimeProfile(profileRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}

	if *hashPassword {
		if *password == "" {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "--hash-password requires --password")
			return 1
		}
		h, err := serve.HashPassword(*password)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		fmt.Println(h)
		return 0
	}

	ctx := context.Background()
	bc := serve.NewBroadcaster()
	cfg, _ := config.Load()

	serveCfg := serveConfigWithCommandDefaults(opts.command, authExplicit, cfg.Serve)

	if *auth != "" {
		serveCfg.AuthMode = *auth
	}
	if *token != "" {
		serveCfg.Token = *token
	}
	if *tokenFile != "" {
		tok, err := readServeTokenFile(*tokenFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		serveCfg.Token = tok
	}
	if *behindProxy {
		serveCfg.BehindProxy = true
	}
	mode, err := serve.NormalizeAuthMode(serveCfg.AuthMode)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	serveCfg.AuthMode = mode
	if *password != "" && serveCfg.AuthMode == "password" {

		h, err := serve.HashPassword(*password)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "failed to hash password:", err)
			return 1
		}
		serveCfg.PasswordHash = h
	}
	if serveCfg.AuthMode == "password" && strings.TrimSpace(serveCfg.PasswordHash) == "" {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "auth mode password requires --password or serve.password_hash")
		return 1
	}

	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	var resumeSession *agent.Session
	if *resume != "" {
		if err := leases.Rebind(*resume); err != nil {
			if errors.Is(err, agent.ErrSessionLeaseHeld) {
				fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, control.SessionInUseMessage(err)+"; "+control.SessionLeaseCloseHint)
			} else {
				fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			}
			return 1
		}
		var err error
		resumeSession, err = loadResumableSession(*resume)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
	}
	*model = modelForResumePath(*model, *resume, cfg)

	*model = resolveServeModel(*model)

	ctrl, err := setupProfileWithOverrides(ctx, *model, *maxSteps, false, bc, profile, cliBuildOverrides{
		OnSessionRecovered: cliSessionRecoveredHandler(leases),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	defer ctrl.Close()
	SetTaskJobKiller(ctrlKillerAdapter{ctrl})

	if *resume != "" {
		ctrl.Resume(resumeSession, *resume)
	} else if *sessionID != "" {
		freshPath, err := freshWebSessionPath(ctrl.SessionDir(), *sessionID)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		ctrl.SetFreshSessionPath(freshPath)
	}
	ctrl.EnsureSessionPath()

	if err := rebindCLIControllerAuthority(leases, ctrl); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, control.SessionInUseMessage(err)+"; "+control.SessionLeaseCloseHint)
		return 1
	}

	srv := serve.New(ctrl, bc, serveCfg)
	_ = srv.SetSessionLeases(leases)
	return runServeFrontend(ctrl, srv, serveCfg, serveFrontendOptions{
		command: opts.command, address: *addr,
		portFile: *portFile, tokenFile: *tokenFile, pidFile: *pidFile,
		openBrowser: *openBrowser && !*noOpen,
		hasSession:  *resume != "" || *sessionID != "",
	})
}
