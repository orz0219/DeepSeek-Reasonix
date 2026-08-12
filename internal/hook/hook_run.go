package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/pluginpkg"
	"reasonix/internal/proc"
	"reasonix/internal/sandbox"
	"reasonix/internal/secrets"
)

// SpawnInput / SpawnResult / Spawner are the test seam around the real spawn.
type SpawnInput struct {
	Command string
	Args    []string
	Mode    ExecutionMode
	Shell   string
	Cwd     string
	Env     map[string]string
	Stdin   string
	Timeout time.Duration
}

// RuntimeOptions carries resolved host dependencies into Hook execution.
// It is runtime-only and never changes persisted Hook configuration.
type RuntimeOptions struct {
	BashPath string
}

// RuntimeOptionsForShell carries an explicitly configured Bash path into Hook
// execution while leaving other interpreter preferences independent.
func RuntimeOptionsForShell(prefer, path string) RuntimeOptions {
	if !strings.EqualFold(strings.TrimSpace(prefer), "bash") {
		return RuntimeOptions{}
	}
	return RuntimeOptions{BashPath: strings.TrimSpace(path)}
}

// RuntimeIssue identifies one plugin Hook whose host dependency is unavailable.
type RuntimeIssue struct {
	Event       Event
	Description string
	Err         error
}

// CheckPackageRuntime validates every Hook exported by a plugin package without
// launching commands.
func CheckPackageRuntime(pkg pluginpkg.Package, options RuntimeOptions) []RuntimeIssue {
	events := make([]string, 0, len(pkg.Manifest.Hooks))
	for event := range pkg.Manifest.Hooks {
		events = append(events, event)
	}
	sort.Strings(events)
	var issues []RuntimeIssue
	for _, eventName := range events {
		for _, h := range pkg.Manifest.Hooks[eventName] {
			if err := CheckRuntime(pluginHookExecutionConfig(h, pkg.Root), options); err != nil {
				issues = append(issues, RuntimeIssue{
					Event: Event(eventName), Description: h.Description, Err: err,
				})
			}
		}
	}
	return issues
}

type SpawnResult struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	TimedOut  bool
	SpawnErr  error
	Truncated bool
}

type Spawner func(ctx context.Context, in SpawnInput) SpawnResult

// outputCapBytes bounds per-stream capture so a runaway child can't blow up the
// heap between spawn and timeout.
const outputCapBytes = 256 * 1024

// Run executes the hooks matching payload.Event (and, for tool events, the tool
// name), feeding each the JSON payload on stdin. It stops at the first block so
// a gating hook can prevent later hooks running against a phantom success.
func Run(ctx context.Context, payload Payload, hooks []ResolvedHook, spawner Spawner) Report {
	if spawner == nil {
		spawner = DefaultSpawner
	}
	event := payload.Event
	report := Report{Event: event}
	for _, h := range hooks {
		if h.Event != event || !MatchesTool(h, payload.ToolName) {
			continue
		}
		cwd := h.Cwd
		if cwd == "" {
			cwd = payload.Cwd
		}
		timeout := h.timeout()
		stdin := marshalPayload(payload, h.PayloadFormat)
		input := SpawnInput{
			Command: h.Command,
			Args:    h.Argv,
			Mode:    h.ExecutionMode,
			Shell:   h.Shell,
			Cwd:     cwd,
			Env:     h.Env,
			Stdin:   stdin,
			Timeout: timeout,
		}
		if h.Async {
			asyncCtx := context.WithoutCancel(ctx)
			go runResolvedHook(asyncCtx, h, input, spawner)
			report.Outcomes = append(report.Outcomes, Outcome{Hook: h, Decision: DecisionPass})
			continue
		}
		start := time.Now()
		r := runResolvedHook(ctx, h, input, spawner)
		decision := decideOutcome(h, r)
		if decision == DecisionPass && h.PayloadFormat == "claude" {
			if deny, reason := claudeJSONDeny(event, r.Stdout); deny {
				decision = DecisionBlock
				if reason != "" {
					r.Stdout = reason
				}
			} else if claudeJSONAllow(event, r.Stdout) {
				report.Allowed = true
			}
		}
		report.Outcomes = append(report.Outcomes, Outcome{
			Hook:      h,
			Decision:  decision,
			ExitCode:  r.ExitCode,
			Stdout:    r.Stdout,
			Stderr:    stderrFor(r, timeout),
			TimedOut:  r.TimedOut,
			Truncated: r.Truncated,
			Duration:  time.Since(start),
		})
		if decision == DecisionBlock {
			report.Blocked = true
			break
		}
	}
	return report
}

func marshalPayload(payload Payload, format string) string {
	var body []byte
	if format == "claude" {
		claude := map[string]any{
			"hook_event_name":        payload.Event,
			"session_id":             payload.SessionID,
			"cwd":                    payload.Cwd,
			"tool_name":              claudeFacingToolName(payload.ToolName),
			"tool_input":             claudeFacingToolInput(payload.ToolName, payload.ToolArgs, payload.Cwd),
			"tool_response":          claudeToolResponse(payload),
			"prompt":                 payload.Prompt,
			"last_assistant_message": payload.LastAssistant,
			"source":                 payload.Source,
			"reason":                 payload.Reason,
			"notification_type":      payload.NotificationType,
			"message":                payload.Message,
			"trigger":                payload.Trigger,
			"error":                  payload.Error,
			"is_interrupt":           payload.IsInterrupt,
		}
		body, _ = json.Marshal(claude)
	} else {
		body, _ = json.Marshal(payload)
	}
	return string(body) + "\n"
}

// claudeToolResponse adapts a Reasonix tool result to the tool_response a
// Claude-authored PostToolUse hook reads. Claude's Bash response is an object
// — {stdout, stderr, interrupted}, the fields the official security-guidance
// plugin's commit/push checks read (a non-object response is treated as empty
// and the check silently passes) — while Reasonix's bash returns one combined
// output string, so it is wrapped with the failure error as stderr. Other
// tools' results pass through as before: raw JSON when the result is a JSON
// document, else the plain string.
func claudeToolResponse(p Payload) any {
	if (p.Event == PostToolUse || p.Event == PostToolUseFailure) && claudeFacingToolName(p.ToolName) == "Bash" {
		return map[string]any{
			"stdout":      p.ToolResult,
			"stderr":      p.Error,
			"interrupted": p.IsInterrupt,
		}
	}
	trimmed := strings.TrimSpace(p.ToolResult)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return p.ToolResult
	}
	return json.RawMessage(trimmed)
}

func runResolvedHook(ctx context.Context, h ResolvedHook, in SpawnInput, spawner Spawner) SpawnResult {
	if h.Scope == ScopePlugin && h.ContextFile != "" {
		return readContextFile(h.ContextFile)
	}
	return spawner(ctx, in)
}

func readContextFile(path string) SpawnResult {
	body, err := fileencoding.ReadFileUTF8(path)
	if err != nil {
		return SpawnResult{ExitCode: -1, SpawnErr: err}
	}
	truncated := false
	if len(body) > outputCapBytes {
		body = body[:outputCapBytes]
		truncated = true
	}
	return SpawnResult{ExitCode: 0, Stdout: string(body), Truncated: truncated}
}

// stderrFor returns the best human message for an outcome: real stderr, else a
// spawn-error message, else a timeout note.
func stderrFor(r SpawnResult, timeout time.Duration) string {
	if r.Stderr != "" {
		return r.Stderr
	}
	if r.SpawnErr != nil {
		return r.SpawnErr.Error()
	}
	if r.TimedOut {
		return fmt.Sprintf("hook timed out after %s", timeout)
	}
	return ""
}

// DefaultSpawner executes the hook according to its explicit execution
// contract, with the payload on stdin, capped output, and both per-hook timeout
// and parent-context cancellation.
func DefaultSpawner(ctx context.Context, in SpawnInput) SpawnResult {
	return defaultSpawner(ctx, in, RuntimeOptions{})
}

// NewDefaultSpawner returns the standard Hook spawner with effective host
// runtime paths supplied by boot configuration.
func NewDefaultSpawner(options RuntimeOptions) Spawner {
	return func(ctx context.Context, in SpawnInput) SpawnResult {
		return defaultSpawner(ctx, in, options)
	}
}

func defaultSpawner(ctx context.Context, in SpawnInput, options RuntimeOptions) SpawnResult {
	in = normalizeWindowsHookSpawnInputForPlatform(in, runtime.GOOS)
	cctx, cancel := context.WithTimeout(ctx, in.Timeout)
	defer cancel()

	cmd, spawnErr := spawnCommand(cctx, in.Command, in.Mode, in.Shell, in.Args, options)
	if spawnErr != nil {
		return SpawnResult{ExitCode: -1, SpawnErr: spawnErr}
	}
	proc.HideWindow(cmd)
	cmd.Dir = in.Cwd
	env := secrets.ProcessEnv()
	if len(in.Env) > 0 {
		keys := make([]string, 0, len(in.Env))
		for k := range in.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			env = append(env, k+"="+in.Env[k])
		}
	}
	cmd.Env = env
	cmd.Stdin = strings.NewReader(in.Stdin)
	var outBuf, errBuf cappedBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	cmd.WaitDelay = 500 * time.Millisecond

	err := cmd.Run()
	res := SpawnResult{
		ExitCode:  -1,
		Stdout:    decodeHookOutput(outBuf.Bytes(), outBuf.truncated),
		Stderr:    decodeHookOutput(errBuf.Bytes(), errBuf.truncated),
		Truncated: outBuf.truncated || errBuf.truncated,
	}
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		res.TimedOut = true
	case cctx.Err() == context.Canceled:
		res.SpawnErr = cctx.Err()
	case err != nil:
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
		} else {
			res.SpawnErr = err
		}
	default:
		res.ExitCode = 0
	}
	return res
}

// spawnCommand picks the execution vehicle from the manifest contract.
// Explicit exec-form hooks pass their argv directly to the executable;
// explicit shell-form hooks pass the raw command to the selected interpreter.
// Legacy settings retain Reasonix's historical shell behavior and repairs.
func spawnCommand(ctx context.Context, command string, mode ExecutionMode, shell string, args []string, options RuntimeOptions) (*exec.Cmd, error) {
	switch mode {
	case ExecutionExec:
		return spawnExecCommand(ctx, command, args, options)
	case ExecutionShell:
		return spawnShellCommand(ctx, command, shell, options)
	case ExecutionLegacy:
		return spawnLegacyCommand(ctx, command, args, options)
	default:
		return nil, fmt.Errorf("unsupported hook execution mode %q", mode)
	}
}

func spawnExecCommand(ctx context.Context, command string, args []string, options RuntimeOptions) (*exec.Cmd, error) {
	if runtime.GOOS == "windows" {
		if cmd, matched := windowsBatchArgvCommand(ctx, command, args); matched {
			return cmd, nil
		}
		if resolvedShell, resolvedArgs, matched, err := windowsPOSIXShellArgvInvocationWith(command, args, func() (string, error) {
			return resolveWindowsHookBash(options.BashPath)
		}); matched {
			if err != nil {
				return nil, err
			}
			return exec.CommandContext(ctx, resolvedShell, resolvedArgs...), nil
		}
	}
	return exec.CommandContext(ctx, command, args...), nil
}

// spawnLegacyCommand preserves the pre-contract behavior:
//   - a command this call just repaired (its broken quoting means it never
//     worked through a shell, so there is no expansion behavior to preserve);
//   - on Windows, a recognized node -e stdin-hook command: `cmd /c` mangles
//     quoted JS (&, %, nested quotes), which is the breakage this repair
//     exists for, and cmd performs no POSIX-style $ expansion to preserve.
//   - on Windows, an explicit `sh -c` / `bash -c` command: Git Bash is often
//     installed outside cmd.exe's PATH, and direct exec preserves its quoting.
//
// POSIX commands that were already well-formed keep their shell semantics
// verbatim — normalizeStaticNodeEval's rendering escapes $ and backticks, so
// even repaired commands re-entering here behave identically under sh -c.
func spawnLegacyCommand(ctx context.Context, command string, args []string, options RuntimeOptions) (*exec.Cmd, error) {
	if args != nil {
		return spawnExecCommand(ctx, command, args, options)
	}
	if node, flag, script, ok := repairableNodeEvalArgs(command); ok {
		return exec.CommandContext(ctx, node, flag, script), nil
	}
	if powershell, args, ok := repairablePowerShellFileArgs(command); ok {
		return exec.CommandContext(ctx, powershell, args...), nil
	}
	if runtime.GOOS == "windows" {
		if cmd, matched := windowsBatchCommand(ctx, command); matched {
			return cmd, nil
		}
		if shell, args, matched, err := windowsPOSIXShellInvocationWith(command, func() (string, error) {
			return resolveWindowsHookBash(options.BashPath)
		}); matched {
			if err != nil {
				return nil, err
			}
			return exec.CommandContext(ctx, shell, args...), nil
		}
		if node, flag, script, ok := directNodeEvalArgs(command); ok {
			return exec.CommandContext(ctx, node, flag, script), nil
		}
		if cmd, ok := windowsCmdShellCommand(ctx, command); ok {
			return cmd, nil
		}
	}
	name, args := shellInvocation(command)
	return exec.CommandContext(ctx, name, args...), nil
}

func spawnShellCommand(ctx context.Context, command, preferred string, options RuntimeOptions) (*exec.Cmd, error) {
	preferred = strings.ToLower(strings.TrimSpace(preferred))
	switch preferred {
	case "", "auto":
		if runtime.GOOS == "windows" {

			if cmd, matched := windowsBatchCommand(ctx, command); matched {
				return cmd, nil
			}
			sh, err := cachedWindowsDefaultHookShell()
			if err != nil {
				return nil, err
			}
			return rawShellCommand(ctx, sh, command)
		}
		return exec.CommandContext(ctx, "sh", "-c", command), nil
	case "bash":
		if runtime.GOOS == "windows" {
			path, err := resolveWindowsHookBash(options.BashPath)
			if err != nil {
				return nil, err
			}
			return exec.CommandContext(ctx, path, "-c", command), nil
		}
		return exec.CommandContext(ctx, "bash", "-c", command), nil
	case "powershell", "pwsh":
		sh := sandbox.ResolveShell(preferred, "", nil)
		if sh.Kind != sandbox.ShellPowerShell {
			return nil, fmt.Errorf("hook requires %s, but no usable PowerShell was found", preferred)
		}
		path, err := resolvedHookShellPath(sh)
		if err != nil {
			return nil, err
		}
		return powerShellCommand(ctx, path, command), nil
	case "cmd":
		if cmd, ok := windowsCmdShellCommand(ctx, command); ok {
			return cmd, nil
		}
		return nil, errors.New("hook shell \"cmd\" is only available on Windows")
	default:
		return nil, fmt.Errorf("unsupported hook shell %q", preferred)
	}
}
