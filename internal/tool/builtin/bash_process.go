package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"mvdan.cc/sh/v3/syntax"

	"reasonix/internal/i18n"
	"reasonix/internal/proc"
	"reasonix/internal/sandbox"
	"reasonix/internal/shellparse"
	"reasonix/internal/tool"
)

func applyTerminalResult(ex *tool.ShellExecution, err error) {
	if ex == nil {
		return
	}
	if err == nil {
		ex.State = tool.ShellStateCompleted
		ex.ExitCode = tool.IntPtr(0)
		ex.MutationRisk = tool.ShellMutationMayHaveCompleted
		return
	}
	if errors.Is(err, context.Canceled) {
		ex.State = tool.ShellStateCancelled
		ex.FailurePhase = tool.ShellPhaseCancellation
		ex.MutationRisk = tool.ShellMutationMayBePartial
		return
	}
	var timeoutErr TerminalTimeoutError
	if errors.As(err, &timeoutErr) || errors.Is(err, context.DeadlineExceeded) {
		ex.State = tool.ShellStateTimedOut
		ex.FailurePhase = tool.ShellPhaseTimeout
		ex.MutationRisk = tool.ShellMutationMayBePartial
		return
	}
	var exitErr TerminalExitError
	if errors.As(err, &exitErr) {
		code := exitErr.Code
		ex.ExitCode = &code
		ex.State = tool.ShellStateFailed
		ex.FailurePhase = tool.ShellPhaseExecution
		ex.MutationRisk = tool.ShellMutationMayBePartial
		return
	}

	ex.State = tool.ShellStateFailed
	ex.FailurePhase = tool.ShellPhaseExecution
	ex.MutationRisk = tool.ShellMutationMayBePartial
}

func mergeRunInto(dst *tool.ShellExecution, src *tool.ShellExecution) {
	if dst == nil || src == nil {
		return
	}
	dst.State = src.State
	dst.FailurePhase = src.FailurePhase
	dst.ExitCode = src.ExitCode
	dst.OutputTail = src.OutputTail
	if src.MutationRisk != "" {
		dst.MutationRisk = src.MutationRisk
	}
}

func applyEnvOverrides(env, overrides []string) []string {
	for _, kv := range overrides {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		env = setEnvValue(env, key, value)
	}
	return env
}

// appendSessionDataHint appends the session-data guard warning to command
// output; with no output the hint stands alone. An empty hint is a no-op.
func appendSessionDataHint(out, hint string) string {
	if hint == "" {
		return out
	}
	if strings.TrimSpace(out) == "" {
		return hint
	}
	return out + "\n\n" + hint
}

func unconfinedShellArgv(sh sandbox.Shell, command string) []string {
	argv, _ := sandbox.Command(sandbox.Spec{}, sh, command)
	return argv
}

func approveBashSandboxEscape(ctx context.Context, command string, args json.RawMessage, reason string) (bool, string, error) {
	if !bashSandboxEscapePromptEnabled() {
		return false, "", nil
	}
	approver, ok := sandbox.EscapeApproverFrom(ctx)
	if !ok {
		return false, "", nil
	}
	return approver.ApproveSandboxEscape(ctx, sandbox.EscapeRequest{
		Command: command,
		Args:    append(json.RawMessage(nil), args...),
		Reason:  reason,
	})
}

func bashSandboxEscapeSessionAllowed(ctx context.Context, command string, args json.RawMessage) bool {
	if !bashSandboxEscapePromptEnabled() {
		return false
	}
	approver, ok := sandbox.EscapeApproverFrom(ctx)
	if !ok {
		return false
	}
	checker, ok := approver.(sandbox.EscapeSessionChecker)
	if !ok {
		return false
	}
	return checker.SandboxEscapeSessionAllowed(ctx, sandbox.EscapeRequest{
		Command: command,
		Args:    append(json.RawMessage(nil), args...),
		Reason:  i18n.M.SandboxEscapeRuntimeReason,
	})
}

func normalizeBashRunError(ctx context.Context, err error, preserveBackgroundProcesses bool) error {
	if preserveBackgroundProcesses && ctx.Err() == nil && errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}

func shouldReapAfterRun(ctx context.Context, sh sandbox.Shell, command string, preserveBackgroundProcesses bool) bool {
	if ctx.Err() != nil {
		return true
	}
	if preserveBackgroundProcesses {
		return false
	}
	return sh.Kind != sandbox.ShellBash || !hasExplicitBackgroundKeepalive(command)
}

// hasExplicitBackgroundKeepalive detects common shell-level daemonization intent
// without letting a plain "cmd &" bypass #3702's stray process cleanup.
func hasExplicitBackgroundKeepalive(command string) bool {
	file, err := shellparse.ParseBash(command)
	if err != nil {
		return false
	}

	hasBackground := false
	hasKeepaliveCommand := false
	syntax.Walk(file, func(node syntax.Node) bool {
		switch n := node.(type) {
		case *syntax.Stmt:
			if n.Background {
				hasBackground = true
			}
		case *syntax.CallExpr:
			name, ok := staticShellCallName(n)
			if !ok {
				break
			}
			switch name {
			case "disown", "nohup", "setsid":
				hasKeepaliveCommand = true
			}
		}
		return !(hasBackground && hasKeepaliveCommand)
	})
	return hasBackground && hasKeepaliveCommand
}

func (b bash) foregroundTimeout() time.Duration {
	if b.timeout <= 0 {
		return 0
	}
	return b.timeout
}

func shouldTrackShellProcess(wrapped bool, sh sandbox.Shell, command string, preserveBackgroundProcesses bool) bool {
	if preserveBackgroundProcesses {
		return false
	}
	if runtime.GOOS == "windows" && wrapped {
		return false
	}
	return sh.Kind != sandbox.ShellBash || !hasExplicitBackgroundKeepalive(command)
}

func runShellProcess(ctx context.Context, cmd *exec.Cmd, sh sandbox.Shell, command string, track bool) (*proc.TrackedCommand, error) {
	return proc.RunCommand(ctx, cmd, proc.RunOptions{
		Track:           track,
		CancelWaitGrace: bashWaitDelay + time.Second,
		Source:          "bash_tool",
		ShellKind:       sh.Kind.String(),
		ShellPath:       sh.Path,
		CommandPreview:  commandPreview(command),
	})
}

func reapShellProcess(cmd *exec.Cmd, tracked *proc.TrackedCommand) {
	if tracked != nil {
		tracked.Kill()
		return
	}
	proc.KillTree(cmd)
}

// hasUnquotedSeq reports whether seq appears in s outside any single- or
// double-quoted span, so a literal "a && b" string argument doesn't trip the
// PowerShell chaining guard.
func hasUnquotedSeq(s, seq string) bool {
	var quote byte
	for i := range len(s) {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if strings.HasPrefix(s[i:], seq) {
			return true
		}
	}
	return false
}

func staticShellCallName(call *syntax.CallExpr) (string, bool) {
	for _, arg := range call.Args {
		word, ok := shellparse.StaticWord(arg)
		if !ok {
			return "", false
		}
		if shellparse.IsAssignment(word) {
			continue
		}
		base := shellparse.WordBase(word)
		if base == "command" || base == "env" {
			continue
		}
		return base, true
	}
	return "", false
}

// commandPreview is a short single-line label for a background bash job, surfaced
// in the status bar and completion notices.
func commandPreview(cmd string) string {
	cmd = strings.TrimSpace(strings.ReplaceAll(cmd, "\n", " "))
	const max = 48
	r := []rune(cmd)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return cmd
}
