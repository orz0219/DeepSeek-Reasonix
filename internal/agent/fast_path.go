package agent

import "reasonix/internal/tool"

// executionPath classifies a tool call into fast or slow execution.
// Fast path skips recovery gate, permission gate, checkpoint, hooks,
// and other mutation-only checks for read-only tools.
type executionPath int

const (
	pathUnknown executionPath = iota
	pathFast                  // read-only, no special policies, no mutation overhead
	pathSlow                  // mutation, proxy, policy, recovery, or hooks
)

// classifyExecutionPath determines whether a tool call can take the fast
// execution path. Fast path conditions (all must hold):
//   - tool is read-only
//   - tool is not a proxy (CallResolver)
//   - tool is not contextual (ContextualTool)
//   - plan mode is off
//   - delivery profile is off
//   - mutation dependency barrier is off
//
// The aggressive strategy: recovery gate, permission gate, and hooks are
// NOT checked — read-only tools are safe to skip them because:
//   - permission.Policy.Decide falls back to Allow for read-only tools
//   - recoveryGate.BeforeMutation is only called when mutates==true
//   - hooks.PreToolUse is typically a no-op for read-only tools
func (a *Agent) classifyExecutionPath(plan *toolCallPlan) executionPath {
	if plan == nil || plan.tool == nil {
		return pathSlow
	}
	if !plan.readOnly {
		return pathSlow
	}
	if _, ok := plan.tool.(tool.CallResolver); ok {
		return pathSlow
	}
	if _, ok := plan.tool.(tool.ContextualTool); ok {
		return pathSlow
	}
	if a.planMode.Load() {
		return pathSlow
	}
	if a.deliveryProfile {
		return pathSlow
	}
	if a.mutationDependencyBarrier.Load() {
		return pathSlow
	}
	return pathFast
}
