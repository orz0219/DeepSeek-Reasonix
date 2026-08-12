package agent

import (
	"encoding/json"
	"strings"

	"reasonix/internal/planmode"
	"reasonix/internal/tool"
)

func (a *Agent) readOnlyExecutionBlock(visible tool.Tool, resolved *tool.ResolvedCall) (toolOutcome, bool) {
	if a == nil || !a.readOnlyExecution {
		return toolOutcome{}, false
	}
	block := func(reason string) (toolOutcome, bool) {
		return toolOutcome{
			output:  "blocked: read-only agent cannot " + reason,
			blocked: true,
			errMsg:  "blocked by read-only execution boundary",
		}, true
	}

	blockDestructiveForExecutor := func(name string) (toolOutcome, bool) {
		msg := "blocked: MCP capability " + name + " is destructive and is reserved for the Executor. Write the required operation into the plan/handoff so the Coordinator can hand it to the Executor; do not treat this as missing MCP configuration or an unavailable capability."
		return toolOutcome{
			output:  msg,
			blocked: true,
			errMsg:  "blocked: destructive MCP reserved for executor",
		}, true
	}
	if resolved == nil {
		if a.plannerMCPExecution && isMCPExecutionTarget(visible, "") {
			if !mcpServerAuthorized(visible) {
				return block("execute an MCP capability from an unauthorized server")
			}
			if readOnlyExecutionMCPDestructive(visible) {
				return blockDestructiveForExecutor(visible.Name())
			}
			return toolOutcome{}, false
		}
		if visible == nil || !visible.ReadOnly() {
			if reasoner, ok := visible.(tool.ReadOnlyExecutionBlockReason); ok && strings.TrimSpace(reasoner.ReadOnlyExecutionBlockReason()) != "" {
				return block(reasoner.ReadOnlyExecutionBlockReason())
			}
			return block("execute a state-changing tool")
		}
		if isInstalledMCPTool(visible) && !mcpServerAuthorized(visible) {
			return block("execute a reader from an unauthorized MCP server")
		}
		if readOnlyExecutionMCPDestructive(visible) {
			return block("execute a destructive MCP capability")
		}
		if h, ok := visible.(tool.ReadOnlyExecutionHostMutation); ok && h.ReadOnlyExecutionHostMutation() && !readOnlyExecutionAllowsMCPStartup(visible) {
			return block("start or mutate a host capability")
		}
		return toolOutcome{}, false
	}

	switch resolved.ProxyAction {
	case "list", "inspect":
		if !resolved.SkipExecute || resolved.Target != nil || !resolved.ReadOnly {
			return block("execute a malformed dynamic inspection")
		}
		return toolOutcome{}, false
	case "decline":
		return block("decline a capability decision")
	case "call":
		if resolved.Target == nil {
			if a.plannerMCPExecution && resolved.HostCompleted && resolved.SkipExecute && resolved.ReadOnly && !resolved.Unavailable {
				if _, ok := parseMCPServerCapabilityID(resolved.CapabilityID); ok {
					return toolOutcome{}, false
				}
			}
			return block("execute an unresolved dynamic capability")
		}
		if a.plannerMCPExecution && plannerAllowsMCPTarget(resolved.Target, resolved.TargetName) {
			if isMCPLifecycleConnectTarget(resolved.Target) {
				if !plannerMCPConnectAllowed(resolved.Target) {
					return block("start an unauthorized MCP server")
				}
			} else if !mcpServerAuthorized(resolved.Target) {
				return block("execute an MCP capability from an unauthorized server")
			}
			if readOnlyExecutionMCPDestructive(resolved.Target) {
				name := resolved.TargetName
				if name == "" {
					name = resolved.CapabilityID
				}
				return blockDestructiveForExecutor(name)
			}
			return toolOutcome{}, false
		}
		if !resolved.ReadOnly {
			if reasoner, ok := resolved.Target.(tool.ReadOnlyExecutionBlockReason); ok && strings.TrimSpace(reasoner.ReadOnlyExecutionBlockReason()) != "" {
				return block(reasoner.ReadOnlyExecutionBlockReason())
			}
			return block("execute a state-changing dynamic capability")
		}
		if isInstalledMCPTool(resolved.Target) && !mcpServerAuthorized(resolved.Target) {
			return block("execute a dynamic reader from an unauthorized MCP server")
		}
		if readOnlyExecutionMCPDestructive(resolved.Target) {
			return block("execute a destructive MCP capability")
		}
		if h, ok := resolved.Target.(tool.ReadOnlyExecutionHostMutation); ok && h.ReadOnlyExecutionHostMutation() && !readOnlyExecutionAllowsMCPStartup(resolved.Target) {
			return block("start or mutate a host capability")
		}
		return toolOutcome{}, false
	default:
		return block("execute an unknown dynamic capability action")
	}
}

func readOnlyExecutionMCPDestructive(t tool.Tool) bool {
	return mcpDestructiveHint(t)
}

func readOnlyExecutionAllowsMCPStartup(t tool.Tool) bool {
	if t == nil || !t.ReadOnly() || readOnlyExecutionMCPDestructive(t) {
		return false
	}
	if !mcpServerAuthorized(t) {
		return false
	}
	meta, ok := t.(tool.MCPMetadata)
	if !ok || strings.TrimSpace(meta.MCPServerName()) == "" || strings.TrimSpace(meta.MCPRawToolName()) == "" {
		return false
	}
	return true
}

// plannerAllowsMCPTarget reports whether a resolved use_capability target is an
// MCP tool or lifecycle connect that Planner may consider under
// PlannerMCPExecution (authorization and destructive checks run separately).
func plannerAllowsMCPTarget(t tool.Tool, targetName string) bool {
	if t == nil {
		return false
	}
	if isInstalledMCPTool(t) || isMCPLifecycleConnectTarget(t) {
		return true
	}
	return isMCPExecutionTarget(t, targetName)
}

// isMCPLifecycleConnectTarget identifies on-demand MCP connect-and-list targets
// (mcp_connect__<server>) used by use_capability action=call on mcp-server ids.
func isMCPLifecycleConnectTarget(t tool.Tool) bool {
	if t == nil {
		return false
	}
	if _, ok := t.(mcpLifecycleConnect); ok {
		return true
	}
	name := strings.TrimSpace(t.Name())
	return strings.HasPrefix(name, "mcp_connect__")
}

// mcpLifecycleConnect is implemented by deferred connect targets so Planner
// can authorize lifecycle actions without relying on name prefixes alone.
type mcpLifecycleConnect interface {
	MCPLifecycleConnect() bool
	MCPServerAuthorized() bool
}

func plannerMCPConnectAllowed(t tool.Tool) bool {
	if life, ok := t.(mcpLifecycleConnect); ok {
		return life.MCPServerAuthorized()
	}
	return mcpServerAuthorized(t)
}

func isInstalledMCPTool(t tool.Tool) bool {
	meta, ok := t.(tool.MCPMetadata)
	return ok && strings.TrimSpace(meta.MCPServerName()) != "" && strings.TrimSpace(meta.MCPRawToolName()) != ""
}

func isMCPExecutionTarget(t tool.Tool, name string) bool {
	return isInstalledMCPTool(t) || strings.HasPrefix(strings.TrimSpace(name), "mcp__")
}

func mcpServerAuthorized(t tool.Tool) bool {
	authority, ok := t.(tool.MCPServerAuthorization)
	return ok && authority.MCPServerAuthorized()
}

func mcpDestructiveHint(t tool.Tool) bool {
	annotations, ok := t.(tool.MCPAnnotations)
	return ok && annotations.MCPDestructiveHint()
}

func (a *Agent) planModeDecision(toolName string, readOnly bool, safety planmode.PlanSafety, args json.RawMessage) planmode.Decision {
	return (planmode.Policy{}).Decide(planmode.Call{
		Name:     toolName,
		ReadOnly: readOnly,
		Safety:   safety,
		Args:     args,
	})
}
