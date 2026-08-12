package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"reasonix/internal/ablation"
	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/i18n"
	"reasonix/internal/notify"
	"reasonix/internal/sessiontemp"

	"github.com/spf13/pflag"
)

func isDoctorRepairCommand(args []string) bool {
	return len(args) > 1 && args[0] == "doctor" && args[1] == "repair"
}

func isDefaultInteractiveFlag(arg string) bool {
	switch arg {
	case "--model", "--max-steps", "--continue", "-c", "--resume", "-r", "--copy", "--dangerously-skip-permissions", "--yolo", "--permission-mode", "--effort", "--dir", "--add-dir", "--allowed-tools", "--allowedTools", "--profile":
		return true
	}
	if name, _, ok := strings.Cut(arg, "="); ok && isDefaultInteractiveFlag(name) {
		return true
	}
	return false
}

func shouldMigrateLegacyConfigForCLI(cmd string) bool {
	switch cmd {
	case "", "run", "chat", "code", "serve", "web", "setup", "config", "init", "acp", "mcp", "remote", "plugin", "subagent", "doctor", "bot", "upgrade", "update":
		return true
	default:
		return false
	}
}

func migrateLegacyConfigForCLI() {
	if _, err := config.MigrateLegacyIfNeeded(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: config migration failed:", err)
	}
	if _, err := config.ApplyUserConfigUpgradesOnStartup(config.UserConfigPath()); err != nil {
		fmt.Fprintln(os.Stderr, "warning: config upgrade failed:", err)
	}
}

func migrateMCPConfigForCLIWorkspace() {
	if wd, err := os.Getwd(); err == nil {
		if _, err := config.MigrateMCPToUserConfigOnUpgrade([]string{wd}); err != nil {
			fmt.Fprintln(os.Stderr, "warning: MCP config migration failed:", err)
		}
	}
}

func configureCLIThemeFromConfig() {
	if cfg, err := config.Load(); err == nil {
		configureCLIThemeWithStyle(cfg.UITheme(), cfg.UIThemeStyle())
		cliCursorShape = cfg.UICursorShape()
	} else {
		configureCLITheme("auto")
		cliCursorShape = "bar"
	}
}

func configureCLIThemeFromConfigForTTYOutput() {
	if isTTY(os.Stdout) {
		withTerminalProbe(configureCLIThemeFromConfig)
		return
	}
	configureCLIThemeFromConfig()
}

// setupProfile builds a ready-to-drive Controller from config via boot.Build.
// The assembly (model resolution, tool registry, permission gate, two-model
// Coordinator) lives in internal/boot, shared with the desktop frontend.
// requireKey forces the executor's API key to be present (used by run); chat
// passes false so the session UI is reachable before a key is set. sink receives
// the agent's typed event stream — runAgent passes a TextSink that renders to
// stdout, the TUI passes an event-channel sink so events become tea.Msgs.
// profile selects economy|balanced|delivery (empty = balanced/full).
// workspaceRoot pins the project root explicitly (from --dir); empty falls back
// to git-root detection.
func setupProfile(ctx context.Context, modelName string, maxStepsOverride int, requireKey bool, sink event.Sink, profile string, workspaceRoot string) (*control.Controller, error) {
	return setupProfileWithOverrides(ctx, modelName, maxStepsOverride, requireKey, sink, profile, cliBuildOverrides{WorkspaceRoot: workspaceRoot})
}

type cliBuildOverrides struct {
	Effort               *string
	PermissionAllow      []string
	AdditionalDirs       []string
	WorkspaceRoot        string
	HeadlessApprovalMode string
	Stderr               io.Writer
	OnSessionRecovered   func(control.SessionRecoveryInfo) error
	Ablation             ablation.Set
	// SessionTemp carries the previous Controller's private temporary directory
	// manager across model/profile rebuilds so temporary files survive.
	SessionTemp *sessiontemp.Manager
}

// sessionTempFromCLIController returns the logical-session private temporary
// directory manager for a same-session CLI controller rebuild. Nil keeps fresh
// builds on control.New's normal new-manager path.
func sessionTempFromCLIController(ctrl control.SessionAPI) *sessiontemp.Manager {
	prev, ok := ctrl.(*control.Controller)
	if !ok || prev == nil {
		return nil
	}
	return prev.SessionTemp()
}

func setupProfileWithOverrides(ctx context.Context, modelName string, maxStepsOverride int, requireKey bool, sink event.Sink, profile string, overrides cliBuildOverrides) (*control.Controller, error) {
	migrateMCPConfigForCLIWorkspace()
	return boot.Build(ctx, cliProfileBuildOptions(modelName, maxStepsOverride, requireKey, sink, profile, overrides))
}

func cliProfileBuildOptions(modelName string, maxStepsOverride int, requireKey bool, sink event.Sink, profile string, overrides cliBuildOverrides) boot.Options {

	return boot.Options{
		Model:                modelName,
		MaxSteps:             maxStepsOverride,
		MaxStepsKey:          "--max-steps",
		RequireKey:           requireKey,
		Sink:                 sink,
		AgentPreset:          boot.NormalizeAgentPreset(profile),
		TokenMode:            boot.NormalizeTokenMode(profile),
		SessionDir:           resolveCLISessionDir(),
		WorkspaceRoot:        overrides.WorkspaceRoot,
		EffortOverride:       overrides.Effort,
		PermissionAllow:      overrides.PermissionAllow,
		AdditionalDirs:       overrides.AdditionalDirs,
		HeadlessApprovalMode: overrides.HeadlessApprovalMode,
		StatsSource:          "cli",
		Stderr:               overrides.Stderr,
		OnSessionRecovered:   overrides.OnSessionRecovered,
		Ablation:             overrides.Ablation,
		SessionTemp:          overrides.SessionTemp,
	}
}

type cliPermissionMode struct {
	approval string
	plan     bool
	allow    []string
}

func parsePermissionMode(value string) (cliPermissionMode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "default", "ask":
		return cliPermissionMode{approval: control.ToolApprovalAsk}, nil
	case "auto":
		return cliPermissionMode{approval: control.ToolApprovalAuto}, nil
	case "acceptedits", "accept-edits":
		return cliPermissionMode{approval: control.ToolApprovalAsk, allow: []string{
			"write_file", "edit_file", "multi_edit", "move_file", "notebook_edit", "delete_range", "delete_symbol",
		}}, nil
	case "manual":
		return cliPermissionMode{approval: control.ToolApprovalAsk}, nil
	case "dontask", "dont-ask":
		return cliPermissionMode{approval: control.ToolApprovalDontAsk}, nil
	case "plan":
		return cliPermissionMode{approval: control.ToolApprovalAsk, plan: true}, nil
	case "bypasspermissions", "bypass-permissions", "yolo":
		return cliPermissionMode{approval: control.ToolApprovalYolo}, nil
	default:
		return cliPermissionMode{}, fmt.Errorf("unknown permission mode %q (want manual, ask, auto, acceptEdits, dontAsk, plan, or bypassPermissions)", value)
	}
}

func resolveRunPermissionMode(value string, auto, modeExplicit bool) (string, error) {
	if !auto {
		return value, nil
	}
	if modeExplicit {
		return "", errors.New("--auto/-y cannot be combined with --permission-mode")
	}
	return "auto", nil
}

func applyPermissionMode(ctrl *control.Controller, mode cliPermissionMode) {
	if ctrl == nil {
		return
	}
	ctrl.SetToolApprovalMode(mode.approval)
	ctrl.SetPlanMode(mode.plan)
}

// resolveCLISessionDir returns the session dir for CLI invocations. When the
// current working directory maps to a project session dir, the project dir is
// used so /resume shows project history. Falls back to the global session dir.
func resolveCLISessionDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		return config.SessionDir()
	}
	if projDir := config.ProjectSessionDir(cwd); projDir != "" && projDir != config.SessionDir() {
		return projDir
	}
	return config.SessionDir()
}

// setupQuietProfile is like setupProfile but guarantees plugin subprocess
// stderr stays off the terminal. Interactive callers provide the private TUI
// diagnostic writer; other callers fall back to io.Discard.
func setupQuietProfile(ctx context.Context, modelName string, maxStepsOverride int, requireKey bool, sink event.Sink, profile string, overrides cliBuildOverrides) (*control.Controller, error) {
	if overrides.Stderr == nil {
		overrides.Stderr = io.Discard
	}
	return boot.Build(ctx, cliProfileBuildOptions(modelName, maxStepsOverride, requireKey, sink, profile, overrides))
}

func parseRuntimeProfile(value string) (string, error) {

	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "balanced", boot.TokenModeFull:
		return boot.TokenModeFull, nil
	case boot.TokenModeEconomy, "light", "lite", "eco":
		return boot.TokenModeEconomy, nil
	case boot.TokenModeDelivery, "deliver", "quality":
		return boot.TokenModeDelivery, nil
	default:
		return "", fmt.Errorf("unknown execution setting %q (want light, balanced, or delivery; legacy: economy, full)", value)
	}
}

// chdirTo honours --dir: it switches the working directory before anything reads
// it, so config discovery, the sandbox root, and file tools all resolve from the
// chosen project root. Returns 2 (already reported) on failure, 0 otherwise.
func chdirTo(dir string) int {
	if dir == "" {
		return 0
	}
	if err := os.Chdir(dir); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	return 0
}

// workspaceRootForDir returns the explicit project root to pin when --dir was
// given. It runs after chdirTo has already switched into dir, so the process
// working directory is the resolved root. An empty dir means no override (fall
// back to git-root detection). A Getwd failure is returned rather than swallowed:
// silently reverting to "" would re-trigger git-root/default resolution and break
// the explicit --dir guarantee, so the caller must fail loudly instead.
func workspaceRootForDir(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve --dir workspace root: %w", err)
	}
	return wd, nil
}

func modelForResumePath(modelName, resumePath string, cfg *config.Config) string {
	if strings.TrimSpace(modelName) != "" || strings.TrimSpace(resumePath) == "" {
		return modelName
	}
	sessionModel, ok := agent.LoadSessionModel(resumePath)
	if !ok {
		return modelName
	}
	if cfg == nil {
		return sessionModel
	}
	if _, ok := cfg.ResolveModel(sessionModel); !ok {
		return modelName
	}
	return sessionModel
}

func loadResumableSession(path string) (*agent.Session, error) {
	if agent.IsCleanupPending(path) {
		return nil, fmt.Errorf("session is pending cleanup")
	}
	return agent.LoadSession(path)
}

var newNotificationSender = func() notify.Sender { return notify.NewPlatformSender() }

// withNotifications adds system notifications to CLI event streams when configured.
func withNotifications(sink event.Sink, cfg *config.Config) event.Sink {
	if cfg == nil || !cfg.Notifications.Enabled {
		return sink
	}
	return notify.NewSink(sink, newNotificationSender(), cfg.Notifications)
}

// registerContinueFlag registers --continue with its -c shorthand. The
// shorthand must go through BoolP (pflag shorthand), not BoolVar: BoolVar
// registers "c" as a long flag name, which leaves "-c" unparseable
// ("unknown shorthand flag: 'c' in -c") while accidentally accepting "--c".
func registerContinueFlag(fs *pflag.FlagSet) *bool {
	return fs.BoolP("continue", "c", false, "resume the most recent saved session")
}
