package control

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/i18n"
	"reasonix/internal/memory"
	"reasonix/internal/sandbox"
	"reasonix/internal/shellrun"
	"reasonix/internal/tool"
)

func (c *Controller) rememberProjectNote(note string) {
	if note == "" {
		c.notice("nothing to remember")
		return
	}
	if path, err := c.QuickAdd(memory.ScopeProject, note); err != nil {
		c.notice("memory: " + err.Error())
	} else {
		c.notice("remembered → " + path)
	}
}

func (c *Controller) applyGoalCommand(input, display string) bool {
	cmd, ok := ParseGoalCommand(input)
	if !ok {
		return false
	}
	if cmd.DeprecatedBudgetFlag {
		c.notice(GoalBudgetFlagDeprecatedNotice)
	}
	switch cmd.Action {
	case GoalCommandSet:
		c.SetPlanMode(false)
		c.SetGoalWithResearchMode(cmd.Text, cmd.ResearchMode)
		c.GoalStrict(cmd.Strict)
		c.startGoalCommandTurn(cmd, display)
	case GoalCommandClear:
		c.ClearGoal()
		c.notice(i18n.M.GoalCleared)
	case GoalCommandPause:
		if !c.PauseGoal() {
			c.notice(i18n.M.GoalNotRunning)
		}
	case GoalCommandResume:
		if !c.ResumeGoal() {
			c.notice(i18n.M.GoalNotPaused)
		}
	default:
		goal := c.Goal()
		if strings.TrimSpace(goal) == "" {
			c.notice(i18n.M.GoalEmpty)
			break
		}
		rt := c.GoalRuntime()
		c.notice(fmt.Sprintf(i18n.M.GoalCurrentFmt, goal))
		c.notice(fmt.Sprintf(i18n.M.GoalRuntimeFmt,
			rt.TurnsUsed, rt.RequestsUsed, rt.TokensUsed,
			GoalWorkDurationText(rt.WorkDurationMs)))
		if rt.LastReason != "" {
			c.noticeDetail(i18n.M.GoalRuntimeLastReason, rt.LastReason)
		}
		if rt.StopCause != "" {
			c.notice(fmt.Sprintf(i18n.M.GoalPausedFmt, rt.StopCause))
		}
	}
	return true
}

// applyPlanExec reads the current canonical todo list and starts a goal that
// analyzes and dispatches independent steps concurrently via parallel_tasks.
// Supports --strict flag: /plan-exec --strict enables strict goal mode.
func (c *Controller) applyPlanExec(input, display string) {
	todos := c.executor.CanonicalTodoState()
	if len(todos) == 0 {
		c.notice("no active plan with todos to execute")
		return
	}

	strict := slices.Contains(strings.Fields(input), "--strict")

	total := len(todos)
	done := 0
	for _, t := range todos {
		if t.Status == "completed" {
			done++
		}
	}

	var b strings.Builder
	b.WriteString("You are the execution conductor. Route each step to the right sub-agent by module.\n\n")

	modules := c.detectProjectModules()
	if len(modules) > 0 {
		b.WriteString("## Project modules detected\n\n")
		for _, m := range modules {
			fmt.Fprintf(&b, "- %s/", m)
		}
		b.WriteString("\n\nRoute steps to the module they belong to. Steps in different modules can run in parallel.\n\n")
	}

	b.WriteString("## Plan steps\n\n")
	for _, t := range todos {
		status := t.Status
		if status == "" {
			status = "pending"
		}
		mark := " "
		if status == "completed" {
			mark = "x"
		}
		fmt.Fprintf(&b, "- [%s] %s (%s)\n", mark, t.Content, status)
	}
	b.WriteString("\n## Routing rules\n")
	b.WriteString("1. Group steps by MODULE \u2014 same module = serial, different modules = parallel batches\n")
	b.WriteString("2. Research/exploration across modules = use parallel_tasks\n")
	b.WriteString("3. Dispatch each batch via parallel_tasks \u2014 each sub-agent gets one module\u2019s context\n")
	b.WriteString("4. Verify each batch before the next\n")
	b.WriteString("5. Failures: fix before moving on\n")
	b.WriteString("\nGoal: each sub-agent focuses on one module and does not carry irrelevant context.\n")
	if done > 0 {
		fmt.Fprintf(&b, "\nNote: %d/%d steps are already completed. Focus on the remaining %d steps.\n", done, total, total-done)
	}
	prompt := b.String()

	if len(modules) > 0 {
		c.notice(fmt.Sprintf("plan-exec: detected %d modules — %s", len(modules), strings.Join(modules, ", ")))
	}

	c.SetPlanMode(false)
	c.SetGoal("execute plan: " + ShortGoalForNotice(todos[0].Content))
	c.GoalStrict(strict)
	c.notice(fmt.Sprintf("plan-exec: dispatching %d plan steps (strict=%v)", total, strict))
	if c.runner != nil {
		c.runGuarded(func(ctx context.Context) error {
			return c.runGoalLoopWithRawDisplay(ctx, prompt, prompt, display)
		})
	}
}

// prometheusPrompt is the strategic planner system prompt.
const prometheusPrompt = "You are Prometheus, a strategic planner. Interview the user one question at a time. Cover: scope, modules, files, constraints, tests. When ready, output a numbered plan with each step tagged by module. End by calling update_goal with status complete. Do not implement.\n\nFor independent research directions, use parallel_tasks before planning."

// applyPrometheus starts an interactive planning interview, inspired by OMO's
// Prometheus agent. It enters goal mode with a structured interview prompt.
func (c *Controller) applyPrometheus(input, display string) {
	args := strings.TrimSpace(strings.TrimPrefix(input, "/prometheus"))
	if args == "" || args == "--strict" {
		c.notice("usage: /prometheus <your task description>")
		return
	}
	strict := false
	if strings.HasPrefix(args, "--strict ") {
		strict = true
		args = strings.TrimPrefix(args, "--strict ")
	}
	prompt := prometheusPrompt + "\n\n## User request\n\n" + args + "\n\nBegin the interview by asking your first clarifying question."
	c.SetPlanMode(false)
	c.SetGoal("plan: " + ShortGoalForNotice(args))
	c.GoalStrict(strict)
	c.notice("prometheus: starting planning interview")
	if c.runner != nil {
		c.runGuarded(func(ctx context.Context) error {
			return c.runGoalLoopWithRawDisplay(ctx, prompt, prompt, display)
		})
	}
}

// shellTimeout is the maximum time a user-invoked "!command" may run. Matches
// the bash tool's timeout so behaviour is consistent across invocation paths.
const shellTimeout = 120 * time.Second

// shellWaitDelay bounds how long cmd.Run() waits after context cancellation for
// the child's pipes to drain, matching the bash tool's WaitDelay.
const shellWaitDelay = 5 * time.Second

func shellCommandPreview(command string) string {
	command = strings.TrimSpace(strings.ReplaceAll(command, "\n", " "))
	const max = 48
	r := []rune(command)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return command
}

// RunShell executes a shell command directly (bypassing the model) and streams
// the output as ToolDispatch/ToolProgress/ToolResult events. It uses the same
// bash-tool infrastructure (shell resolution, timeout) and shares the runGuarded
// lock with model turns — only one can run at a time. User-invoked "!" commands
// run without the OS sandbox (the user typed the command explicitly).
func (c *Controller) RunShell(command string) {
	command = strings.TrimSpace(command)
	if command == "" {
		c.notice(i18n.M.ShellExecEmpty)
		return
	}
	c.runGuarded(func(ctx context.Context) error {
		sh := c.shell
		if sh.Path == "" {
			sh = sandbox.ResolveShell("", "", nil)
		}
		argv, _ := sandbox.Command(sandbox.Spec{}, sh, command)

		preview := []rune(command)
		if len(preview) > 32 {
			preview = preview[:32]
		}
		id := "shell-" + string(preview)
		diagnosticPreview := shellCommandPreview(command)
		desc := shellrun.DescriptorFromShell(sh)

		c.sink.Emit(event.Event{
			Kind: event.ToolDispatch,
			Tool: event.Tool{
				ID:   id,
				Name: "bash",
				Args: fmt.Sprintf(`{"command":%q}`, command),
				Execution: &event.ShellExecution{
					Kind: desc.Kind, Shell: desc.Shell, ShellVersion: desc.ShellVersion,
					Platform: desc.Platform, SupportsAndAnd: desc.SupportsAndAnd,
					State: tool.ShellStateRunning,
				},
			},
		})

		start := time.Now()
		res := shellrun.RunForeground(ctx, shellrun.Request{
			Argv:           argv,
			Dir:            c.workspaceRoot,
			Timeout:        shellTimeout,
			WaitDelay:      shellWaitDelay,
			CommandPreview: diagnosticPreview,
			ShellKind:      sh.Kind.String(),
			ShellPath:      sh.Path,
			Source:         "user_shell",
			Track:          true,
			Progress: func(chunk string) {
				c.sink.Emit(event.Event{
					Kind: event.ToolProgress,
					Tool: event.Tool{ID: id, Output: chunk},
				})
			},
		})
		durationMs := time.Since(start).Milliseconds()
		ex := &event.ShellExecution{
			Kind: desc.Kind, Shell: desc.Shell, ShellVersion: desc.ShellVersion,
			Platform: desc.Platform, SupportsAndAnd: desc.SupportsAndAnd,
			State: res.State, FailurePhase: res.FailurePhase,
			OutputTail: res.OutputTail, DurationMs: durationMs,
			MutationRisk: tool.ShellMutationNone,
			Verification: tool.ShellVerificationNotVerification,
		}
		if res.ExitCode != nil {
			code := *res.ExitCode
			ex.ExitCode = &code
		}
		switch res.State {
		case tool.ShellStateCompleted:
			ex.MutationRisk = tool.ShellMutationNone
		case tool.ShellStateNotRun:
			ex.MutationRisk = tool.ShellMutationNotStarted
		case tool.ShellStateFailed:
			if res.FailurePhase == tool.ShellPhaseLaunch {
				ex.MutationRisk = tool.ShellMutationNotStarted
			} else {
				ex.MutationRisk = tool.ShellMutationMayBePartial
			}
		case tool.ShellStateTimedOut, tool.ShellStateCancelled:
			ex.MutationRisk = tool.ShellMutationMayBePartial
		}

		errText := ""
		switch res.State {
		case tool.ShellStateCancelled:
			errText = i18n.M.TurnCancelled
		case tool.ShellStateTimedOut:
			errText = fmt.Sprintf(i18n.M.ShellExecTimeoutFmt, shellTimeout)
		case tool.ShellStateFailed, tool.ShellStateNotRun:
			if res.Err != nil {
				errText = fmt.Sprintf(i18n.M.ShellExecFailedFmt, res.Err)
			}
		}
		c.sink.Emit(event.Event{
			Kind: event.ToolResult,
			Tool: event.Tool{
				ID: id, Name: "bash", Output: res.Combined, Err: errText,
				DurationMs: durationMs, Execution: ex,
			},
		})
		return nil
	})
}
