package agent

import (
	"context"
	"encoding/json"

	"reasonix/internal/evidence"
	"reasonix/internal/instruction"
	"reasonix/internal/jobs"
	"reasonix/internal/memory"
	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
)

// bashCommand returns the cached bash command string, parsing args once.
func (plan *toolCallPlan) bashCommand() string {
	if plan.cachedBashChecked {
		return plan.cachedBashCommand
	}
	plan.cachedBashChecked = true
	if plan.parsedArgs != nil {
		plan.cachedBashCommand = plan.parsedArgs.StringField("command")
	} else {
		var p struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(plan.evidenceArgs, &p) == nil {
			plan.cachedBashCommand = p.Command
		}
	}
	return plan.cachedBashCommand
}

// rebuildToolContextBase builds the session-level context prefix that
// prepareToolExecution derives per-call context from. Call when session-level
// state (ledger, jobs, sandbox, etc.) changes.
func (a *Agent) rebuildToolContextBase() {
	ctx := context.Background()
	ctx = WithSubagentDepth(ctx, a.subagentDepth)
	if a.task.ledger != nil {
		ctx = evidence.WithLedger(ctx, a.task.ledger)
		if a.deliveryProfile {
			ctx = evidence.WithDeliveryProfile(ctx)
		}
	}
	if !a.planMode.Load() {
		ctx = a.withContractState(ctx)
	}
	if len(a.projectChecks) > 0 {
		ctx = instruction.WithChecks(ctx, a.projectChecks)
	}
	if a.svc.jobs != nil {
		ctx = jobs.WithManager(ctx, a.svc.jobs)
	}
	if a.svc.sandboxEscape != nil {
		ctx = sandbox.WithEscapeApprover(ctx, a.svc.sandboxEscape)
	}
	if a.svc.configWrite != nil {
		ctx = tool.WithConfigWriteApprover(ctx, a.svc.configWrite)
	}
	if a.svc.memQueue != nil {
		ctx = memory.WithQueue(ctx, a.svc.memQueue)
	}
	a.toolContextBase = ctx
}
