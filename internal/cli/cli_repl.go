package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/extension/providerext"
	"reasonix/internal/i18n"
	"reasonix/internal/provider"
	"reasonix/internal/telemetry"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

// chatREPL is an interactive session: a single persistent agent/session and a
// prompt loop that keeps conversation context across turns. Exit with
// 'exit'/'quit' or Ctrl-D.
func chatREPL(args []string, version string) int {
	fs := pflag.NewFlagSet("reasonix", pflag.ContinueOnError)
	fs.SetInterspersed(true)
	model := fs.String("model", "", "provider name (default: config default_model)")
	profileFlag := fs.String("profile", "", "deprecated: use --preset (economy|balanced|delivery)")
	presetFlag := fs.String("preset", "balanced", "agent execution setting: light | balanced | delivery")
	maxSteps := fs.Int("max-steps", 0, "one-off max tool-call rounds (0 = automatic)")
	cont := registerContinueFlag(fs)
	resume := fs.StringP("resume", "r", "", "resume by session ID/query, or open the picker when no value is given")
	fs.Lookup("resume").NoOptDefVal = resumePickerSentinel
	copySession := fs.Bool("copy", false, "with --resume/--continue: duplicate the selected session and continue in the copy (escape hatch when the original is held by another Reasonix process)")
	yolo := fs.Bool("dangerously-skip-permissions", false, "YOLO: auto-approve approval-gated tool calls this session; same runtime mode as Ctrl+Y")
	fs.BoolVar(yolo, "yolo", false, "alias for --dangerously-skip-permissions")
	dir := fs.String("dir", "", "change to this directory first (project root); config, sandbox and file tools resolve from here")
	effort := fs.String("effort", "", "session reasoning effort override")
	permissionMode := fs.String("permission-mode", "ask", "permission mode: manual | ask | auto | acceptEdits | dontAsk | plan | bypassPermissions")
	var additionalDirs []string
	fs.StringArrayVar(&additionalDirs, "add-dir", nil, "allow tool access to an additional directory (repeatable)")
	var allowedToolValues []string
	fs.StringArrayVar(&allowedToolValues, "allowed-tools", nil, "comma or space-separated permission rules to allow")
	fs.StringArrayVar(&allowedToolValues, "allowedTools", nil, "alias for --allowed-tools")
	if code, ok := parseCommandFlags(fs, normalizeOptionalResumeArg(args)); !ok {
		return code
	}
	allowedTools, err := splitAllowedToolRules(allowedToolValues)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
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
	permissions, err := parsePermissionMode(*permissionMode)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
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

	diagnostics := startTUIDiagnostics(config.ReasonixHomeDir())
	defer diagnostics.Close()
	diagnostics.Milestone("config_load_begin")
	cfg, err := config.Load()
	if err == nil {
		configureCLIThemeWithStyle(cfg.UITheme(), cfg.UIThemeStyle())
		cliCursorShape = cfg.UICursorShape()
	}
	diagnostics.Milestone("config_load_done")

	// Decide whether we're starting fresh or resuming. --resume opens an
	// interactive picker; --continue / -c jumps straight into the newest.
	var resumePath string
	resumeValue := strings.TrimSpace(*resume)
	switch strings.ToLower(resumeValue) {
	case "true":
		resumeValue = resumePickerSentinel
	case "false":
		resumeValue = ""
	}
	switch {
	case resumeValue == resumePickerSentinel:
		path, rc := pickSessionToResume()
		if rc != 0 {
			return rc
		}
		resumePath = path
	case resumeValue != "":
		path, err := resolveSessionQuery(resolveCLISessionDir(), resumeValue)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		resumePath = path
	case *cont:
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
		fmt.Printf("continuing in a session copy: %s\n", copied)
		resumePath = copied
	}
	sessionMode := cliTelemetrySessionMode(*cont, resumeValue != "", *copySession)
	reporter := startCLITelemetry(cfg, telemetry.Options{
		Version: version, Interactive: isInteractive(), CLIMode: "tui", Profile: profile,
		PermissionMode: *permissionMode, SessionMode: sessionMode,
	})

	leases := control.NewSessionLeaseKeeper()
	defer leases.Release()
	if resumePath != "" {
		if err := leases.Rebind(resumePath); err != nil {
			if errors.Is(err, agent.ErrSessionLeaseHeld) {
				fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, sessionLeaseResumeRefusal(err))
			} else {
				fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			}
			return 1
		}
	}

	ctx := context.Background()
	*model = modelForResumePath(*model, resumePath, cfg)

	eventCh := make(chan event.Event, 1024)

	var sink event.Sink = &eventSink{ch: eventCh}
	sink = withNotifications(sink, cfg)
	sink = reporter.Wrap(sink)
	var effortOverride *string
	if strings.TrimSpace(*effort) != "" {
		effortOverride = effort
	}
	overrides := cliBuildOverrides{
		Effort:             effortOverride,
		PermissionAllow:    allowedTools,
		AdditionalDirs:     additionalDirs,
		WorkspaceRoot:      workspaceRoot,
		Stderr:             diagnostics.Writer(),
		OnSessionRecovered: cliSessionRecoveredHandler(leases),
	}
	diagnostics.Milestone("controller_build_begin")
	ctrl, err := setupProfileWithOverrides(ctx, *model, *maxSteps, false, sink, profile, overrides)
	if err != nil && errors.Is(err, boot.ErrUnknownModel) && isInteractive() && config.SourcePath() == "" {

		fmt.Fprintln(os.Stderr, i18n.M.ReconfigureOnUnknownModel)
		if rc := interactiveSetup(defaultConfigTarget(), defaultEnvTarget()); rc != 0 {
			return rc
		}
		ctrl, err = setupProfileWithOverrides(ctx, *model, *maxSteps, false, sink, profile, overrides)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	diagnostics.Milestone("controller_build_done")

	if resumePath != "" {
		loaded, err := agent.LoadSession(resumePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		ctrl.Resume(loaded, resumePath)
	}
	ctrl.EnsureSessionPath()

	if err := rebindCLIControllerAuthority(leases, ctrl); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, control.SessionInUseMessage(err)+"; "+control.SessionLeaseCloseHint)
		return 1
	}
	reclaimCLIRecoveryBranches(ctrl.SessionDir())

	missing := ""
	if cfg, loadErr := config.Load(); loadErr == nil {
		name, _, err := resolveModelForCLI(*model, cfg)
		switch {
		case err != nil:
			missing = err.Error()
		case name != "" && providerext.PluginRefOwner(name) != "":

		case name != "":
			if vErr := cfg.Validate(name); vErr != nil {
				missing = vErr.Error()
			}
		}
	}

	termW := 80
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		termW = w
	}

	ctrl.EnableInteractiveApproval()
	applyPermissionMode(ctrl, permissions)

	if *yolo {
		ctrl.SetAutoApproveTools(true)
	}

	m := newChatTUI(ctrl, missing, eventCh, termW)
	m.diagnostics = diagnostics
	m.updateWatchdogStatusProvider()
	m.planMode = permissions.plan
	m.leases = leases
	if cfg != nil {
		m.outputStyle = cfg.Agent.OutputStyle
		m.statuslineCmd = cfg.Statusline.Command
		m.showReasoning = cfg.UI.ShowReasoning
		m.showTurnUsage = cfg.UI.ShowTurnUsage
		m.cfg = cfg
	}

	m.buildController = func(spec controllerBuildSpec, carry []provider.Message, resumePath string, oldCtrl control.SessionAPI) (*control.Controller, error) {
		effectiveOverrides := overrides
		if spec.EffortOverride != nil {
			effectiveOverrides.Effort = spec.EffortOverride
		}

		effectiveOverrides.SessionTemp = sessionTempFromCLIController(oldCtrl)
		c, err := setupQuietProfile(ctx, spec.ModelRef, *maxSteps, false, sink, spec.RuntimeProfile, effectiveOverrides)
		if err != nil {
			return nil, err
		}
		if spec.EffortOverride != nil {
			overrides.Effort = spec.EffortOverride
		}

		path := agent.ContinueSessionPath(resumePath, c.SessionDir(), c.Label())
		if err := adoptCarriedHistoryPreservingProfileAndGrants(c, carry, path, oldCtrl); err != nil {
			c.Close()
			return nil, err
		}
		c.EnableInteractiveApproval()
		c.SetPlanMode(spec.PlanMode)
		if spec.ToolApprovalMode != "" {
			c.SetToolApprovalMode(spec.ToolApprovalMode)
		}
		return c, nil
	}

	m.bindRuntimeRebuilder(*maxSteps, sink, *yolo, overrides, cliProfileBuildOptions)
	m.runtimeProfile = profile
	if effortOverride != nil {
		m.effortLevel = *effortOverride
	}
	if effortOverride == nil {
		m.refreshEffortStatus()
	}

	if m.nativeScrollback {
		prepareNativeScrollback(os.Stdout, m.bottomRows())
	}

	diagnostics.Milestone("terminal_takeover_begin")
	p := tea.NewProgram(m)
	diagnostics.StartWatchdog(p)

	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP, syscall.SIGTERM)
	go func() {
		for range hangup {
			p.Send(tuiShutdownMsg{})
		}
	}()
	final, runErr := p.Run()
	signal.Stop(hangup)
	diagnostics.Milestone("terminal_released")

	launchWeb := false
	launchWebPath := ""
	launchWebSessionID := ""
	launchWebModelRef := ""
	launchWebProfile := ""
	if fm, ok := final.(chatTUI); ok {
		launchWeb = fm.launchWebOnExit
		launchWebProfile = fm.runtimeProfile
		for _, oc := range fm.oldControllers {
			if c, ok := oc.(*control.Controller); ok {
				reporter.RecordRecovery(c.DrainRecoveryMetrics())
			}
			oc.Close()
		}
		if fm.ctrl != nil {
			launchWebPath = fm.launchWebResumePath
			launchWebSessionID = fm.launchWebSessionID
			launchWebModelRef = fm.launchWebModelRef
			if c, ok := fm.ctrl.(*control.Controller); ok {
				reporter.RecordRecovery(c.DrainRecoveryMetrics())
			}
			fm.ctrl.Close()
		} else {
			reporter.RecordRecovery(ctrl.DrainRecoveryMetrics())
			ctrl.Close()
		}
	} else {
		reporter.RecordRecovery(ctrl.DrainRecoveryMetrics())
		ctrl.Close()
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, runErr)
		return 1
	}
	if launchWeb {

		leases.Release()
		return runWebCommand(webHandoffArgs(launchWebPath, launchWebSessionID, launchWebModelRef, launchWebProfile))
	}
	return 0
}

// adoptCarriedHistoryPreservingProfileAndGrants resumes c on the carried
// conversation the way buildController's callers expect: the freshly built
// c already has its own leading system message for the target profile, but
// AdoptHistory below would otherwise replace the
// whole history — including that message — with carry's outgoing one, so the
// switch splices the new leading message in first. It also carries forward
// oldCtrl's same-session "Allow for this session" tool grants and Plan-mode
// read-only command trust, which a rebuild would otherwise silently drop,
// forcing the user to re-approve things already granted this session.
func adoptCarriedHistoryPreservingProfileAndGrants(c *control.Controller, carry []provider.Message, path string, oldCtrl control.SessionAPI) error {
	if fresh := c.History(); len(fresh) > 0 && fresh[0].Role == provider.RoleSystem {
		if len(carry) > 0 && carry[0].Role == provider.RoleSystem {
			carry[0] = fresh[0]
		} else {
			carry = append([]provider.Message{fresh[0]}, carry...)
		}
	}
	c.AdoptHistory(carry, path)
	if prev, ok := oldCtrl.(*control.Controller); ok {
		c.RestoreSessionAuthorizations(prev.SessionAuthorizations())
	}

	if path != "" {
		if err := c.Snapshot(); err != nil {
			return fmt.Errorf("snapshot after runtime switch: %w", err)
		}
	}
	return nil
}

func prepareNativeScrollback(w io.Writer, rows int) {

	fmt.Fprint(w, "\x1B[3J\x1B[2J\x1B[H")
	reserveNativeScrollbackFrame(w, rows)
}

func reserveNativeScrollbackFrame(w io.Writer, rows int) {
	for range rows {
		fmt.Fprintln(w)
	}
}
