package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/checkpoint"
	"reasonix/internal/evidence"
	"reasonix/internal/tool"
)

type toolMutationHookReporter interface {
	ToolMutationHooksEnabled() bool
}

func toolHooksMayMutateWorkspace(hooks ToolHooks) bool {
	if hooks == nil {
		return false
	}
	if reporter, ok := hooks.(toolMutationHookReporter); ok {
		return reporter.ToolMutationHooksEnabled()
	}

	return true
}

// finishToolExecution performs the concrete Execute, records evidence, runs
// post hooks and recovery observation, and truncates the model-facing result.
func (a *Agent) finishToolExecution(ctx context.Context, plan *toolCallPlan) toolOutcome {
	plan.executed = true
	cctx := plan.cctx
	runTool := plan.runTool
	runArgs := plan.runArgs
	call := plan.call
	t := plan.tool
	readOnly := plan.readOnly
	permName := plan.permName
	permArgs := plan.permArgs
	evidenceName := plan.evidenceName
	evidenceArgs := plan.evidenceArgs
	mutates := plan.mutates
	recoveryGen := plan.recoveryGen

	var result string
	var images []string
	var err error

	if readOnly && isInstalledMCPTool(runTool) && mcpServerAuthorized(runTool) && !mcpDestructiveHint(runTool) {
		cctx = tool.WithReaderExecutionIntent(cctx)
	}

	if a.plannerMCPExecution && isMCPExecutionTarget(runTool, permName) && mcpServerAuthorized(runTool) && !mcpDestructiveHint(runTool) {
		cctx = tool.WithNonDestructiveMCPExecutionIntent(cctx)
	}
	var execution *tool.ShellExecution
	if de, ok := runTool.(tool.DetailedExecutor); ok {
		var detailed tool.DetailedResult
		detailed, err = de.ExecuteDetailed(cctx, runArgs)
		result, images, execution = detailed.Output, detailed.Images, detailed.Execution

		if execution != nil && plan.verification {
			switch {
			case err != nil:
				execution.Verification = tool.ShellVerificationFailed
			default:
				execution.Verification = tool.ShellVerificationPassed
			}
		} else if execution != nil && execution.Verification == "" {
			execution.Verification = tool.ShellVerificationNotVerification
		}

		if execution != nil && evidence.BashCommandMayBeOpaqueMutation(runArgs) &&
			execution.MutationRisk == tool.ShellMutationMayHaveCompleted {
			execution.MutationRisk = tool.ShellMutationUnknown
		}
	} else if it, ok := runTool.(tool.ImageTool); ok {
		result, images, err = it.ExecuteWithImages(cctx, runArgs)
	} else {
		result, err = runTool.Execute(cctx, runArgs)
	}

	result, err = a.interceptToolAfter(ctx, call, result, err)

	if plan.trace != nil {
		plan.trace.executeDone = time.Now()
	}

	if msg, refused := tool.BlockedMessage(err); refused {
		return a.blockedToolOutcome(plan, msg)
	}
	a.recordToolReceipts(plan, result, execution, err)

	a.noteCapabilityInvocation(call.Name, json.RawMessage(call.Arguments), err)

	if a.svc.hooks != nil {
		if err != nil {
			a.svc.hooks.PostToolUseFailure(ctx, permName, permArgs, result, err)
		} else {
			a.svc.hooks.PostToolUse(ctx, permName, permArgs, result)
		}
	}

	a.observeAfterMutation(plan)
	plan.mutationAfterDone = true
	if a.svc.recoveryGate != nil {
		a.observeRecoveryResult(ctx, evidenceName, evidenceArgs, readOnly, mutates, result, err, false, false, recoveryGen)
	}
	if err != nil {
		detail := result

		if !json.Valid([]byte(call.Arguments)) {
			detail = strings.TrimRight(detail, "\n") + "\nThe arguments were not valid JSON. Re-emit them exactly per this schema:\n" + string(t.Schema())
		}
		a.recordRepeatFailure(call, t, err)
		rawErr := fmt.Sprintf("error: %v\n%s", err, detail)
		body, truncMsg := truncateToolOutputFor(rawErr, call.Name, call.ID)
		out := toolOutcome{
			output: body, errMsg: firstLine(err.Error()), truncated: truncMsg != "", truncMsg: truncMsg,
			execution: execution, recoveryGeneration: recoveryGen,
		}
		if truncMsg != "" {
			out.rawOutput = rawErr
		}
		return out
	}
	if mutates {
		a.clearRepeatFailuresAfterMutation(evidenceName, evidenceArgs, readOnly)
	}
	a.recordRepeatSuccess(call, t)

	if a.svc.hooks != nil && call.Name == "task" && !isBackgroundTaskCall(call.Arguments) {
		a.svc.hooks.SubagentStop(ctx, result)
	}
	body, truncMsg := truncateToolOutputFor(result, call.Name, call.ID)
	out := toolOutcome{
		output: body, images: images, truncated: truncMsg != "", truncMsg: truncMsg,
		execution: execution, recoveryGeneration: recoveryGen,
	}
	if truncMsg != "" {
		out.rawOutput = result
	}
	return out
}

// observeBeforeMutation captures preimages for Previewable writers and records
// explicit coverage gaps for bash / opaque MCP tools. Host-internal only.
func (a *Agent) observeBeforeMutation(ctx context.Context, plan *toolCallPlan) {
	if a == nil || plan == nil {
		return
	}
	toolName := plan.evidenceName
	if toolName == "" {
		toolName = plan.call.Name
	}
	obs := a.svc.mutationObserver
	if obs != nil {
		if pv, ok := plan.execTool.(tool.Previewer); ok {
			if change, perr := pv.Preview(ctx, plan.execArgs); perr == nil && change.Path != "" {
				obs.BeforeMutationFromChange(change, toolName)
				plan.mutationPath = change.Path
				return
			}
		}

		switch toolName {
		case "bash":
			obs.RecordGap(checkpoint.CoverageGap{Reason: checkpoint.GapBashSideEffect, Tool: toolName, Detail: "bash side effects are not path-tracked"})
		default:

			if !plan.readOnly {
				obs.RecordGap(checkpoint.CoverageGap{Reason: checkpoint.GapMCPExternal, Tool: toolName, Detail: "tool cannot describe local write paths"})
			}
		}
		return
	}

	if a.svc.preEdit != nil {
		if pv, ok := plan.execTool.(tool.Previewer); ok {
			if change, perr := pv.Preview(ctx, plan.execArgs); perr == nil {
				a.svc.preEdit(change)
				plan.mutationPath = change.Path
			}
		}
	}
}

// observeAfterMutation records the after fingerprint when a concrete path was
// known before execution, regardless of tool success or failure.
func (a *Agent) observeAfterMutation(plan *toolCallPlan) {
	if a == nil || plan == nil || plan.mutationPath == "" || a.svc.mutationObserver == nil {
		return
	}
	toolName := plan.evidenceName
	if toolName == "" {
		toolName = plan.call.Name
	}
	a.svc.mutationObserver.AfterMutation(plan.mutationPath, toolName)
}
