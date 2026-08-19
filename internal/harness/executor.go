package harness

import (
	"context"
	"encoding/json"
	"time"

	"reasonix/internal/tool"
)

// ToolRunner is the interface the harness ToolExecutor adapter needs to call
// into the existing tool execution machinery. The agent.Agent already exposes
// tool execution through its registry and permission gate; this thin interface
// isolates the harness from those implementation details.
type ToolRunner interface {
	// RunTool executes one tool call and returns the result text, whether it
	// mutated the workspace, and any error.
	RunTool(ctx context.Context, name string, input json.RawMessage) (output string, mutated bool, err error)
}

// RegistryExecutor adapts a ToolRunner into a harness ToolExecutor. It wraps
// every Action into a tool call, executes it through the runner, and returns
// an Observation with timing and mutation metadata.
type RegistryExecutor struct {
	Runner ToolRunner
}

// Execute implements harness.ToolExecutor by delegating to the wrapped runner.
func (e *RegistryExecutor) Execute(ctx context.Context, a Action) (Observation, error) {
	if e == nil || e.Runner == nil {
		return Observation{
			Kind:   ObservationTool,
			Action: a,
			ErrMsg: "no tool runner",
		}, nil
	}
	start := time.Now()
	output, mutated, err := e.Runner.RunTool(ctx, a.Tool, a.Input)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		return Observation{
			Kind:       ObservationTool,
			Action:     a,
			Success:    false,
			ErrMsg:     err.Error(),
			DurationMs: duration,
		}, nil
	}
	return Observation{
		Kind:       ObservationTool,
		Action:     a,
		Output:     output,
		Success:    true,
		Mutation:   mutated,
		DurationMs: duration,
	}, nil
}

// DirectToolExecutor wraps a single tool.Tool and adapts it into a harness
// ToolExecutor. It is useful for tests and for bridging a single tool into
// the harness without a full registry.
type DirectToolExecutor struct {
	Tool tool.Tool
}

// Execute implements harness.ToolExecutor by calling the wrapped tool directly.
func (e *DirectToolExecutor) Execute(ctx context.Context, a Action) (Observation, error) {
	if e == nil || e.Tool == nil {
		return Observation{
			Kind:   ObservationTool,
			Action: a,
			ErrMsg: "no tool",
		}, nil
	}
	start := time.Now()
	output, err := e.Tool.Execute(ctx, a.Input)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		return Observation{
			Kind:       ObservationTool,
			Action:     a,
			Success:    false,
			ErrMsg:     err.Error(),
			DurationMs: duration,
		}, nil
	}
	return Observation{
		Kind:       ObservationTool,
		Action:     a,
		Output:     output,
		Success:    true,
		Mutation:   !e.Tool.ReadOnly(),
		DurationMs: duration,
	}, nil
}
