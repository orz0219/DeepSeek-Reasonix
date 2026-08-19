package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

// --- Mock tools for benchmarking ---

type benchReadOnlyTool struct{ name string }

func (t *benchReadOnlyTool) Name() string            { return t.name }
func (t *benchReadOnlyTool) Description() string     { return "bench read-only" }
func (t *benchReadOnlyTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *benchReadOnlyTool) ReadOnly() bool          { return true }
func (t *benchReadOnlyTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "ok", nil
}

type benchWriteTool struct{ name string }

func (t *benchWriteTool) Name() string        { return t.name }
func (t *benchWriteTool) Description() string { return "bench writer" }
func (t *benchWriteTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)
}
func (t *benchWriteTool) ReadOnly() bool { return false }
func (t *benchWriteTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "written", nil
}

// benchBashTool simulates bash's read-only classification from args.
type benchBashTool struct{}

func (t *benchBashTool) Name() string        { return "bash" }
func (t *benchBashTool) Description() string { return "bench bash" }
func (t *benchBashTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"}}}`)
}
func (t *benchBashTool) ReadOnly() bool { return false }
func (t *benchBashTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "done", nil
}

// alwaysAllowGate permits every tool call without prompting.
type alwaysAllowGate struct{}

func (g alwaysAllowGate) Check(_ context.Context, _ string, _ json.RawMessage, _ bool) (bool, string, error) {
	return true, "", nil
}

// --- Helpers ---

func benchRegistry() *tool.Registry {
	r := tool.NewRegistry()
	r.Add(&benchReadOnlyTool{name: "read_file"})
	r.Add(&benchReadOnlyTool{name: "grep"})
	r.Add(&benchReadOnlyTool{name: "glob"})
	r.Add(&benchReadOnlyTool{name: "ls"})
	r.Add(&benchWriteTool{name: "edit_file"})
	r.Add(&benchWriteTool{name: "write_file"})
	r.Add(&benchWriteTool{name: "delete_range"})
	r.Add(&benchBashTool{})
	return r
}

func benchAgent(b *testing.B) *Agent {
	b.Helper()
	reg := benchRegistry()
	sess := NewSession("bench system prompt")
	a := New(nil, reg, sess, Options{
		Gate: alwaysAllowGate{},
	}, event.Discard)
	return a
}

func benchTurn() *turnRuntime {
	return &turnRuntime{
		input:            "benchmark task",
		turnInput:        "benchmark task",
		budget:           runBudget{started: time.Now()},
		policySet:        true,
		seenTodoProgress: make(map[string]struct{}),
	}
}

func benchToolCall(toolName, args string) provider.ToolCall {
	return provider.ToolCall{
		ID:        fmt.Sprintf("bench-%s-%d", toolName, time.Now().UnixNano()),
		Name:      toolName,
		Arguments: args,
	}
}

// --- Benchmarks ---

// BenchmarkExecuteBatchReadOnly measures the hot path for a batch of read-only tool calls.
func BenchmarkExecuteBatchReadOnly(b *testing.B) {
	a := benchAgent(b)
	turn := benchTurn()
	ctx := context.Background()

	calls := []provider.ToolCall{
		benchToolCall("read_file", `{"path":"internal/agent/agent.go"}`),
		benchToolCall("read_file", `{"path":"internal/agent/execute_one.go"}`),
		benchToolCall("grep", `{"pattern":"TODO","path":"."}`),
		benchToolCall("ls", `{"path":"."}`),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.executeBatch(ctx, turn, calls)
	}
}

// BenchmarkExecuteBatchSingleRead measures one read-only tool call.
func BenchmarkExecuteBatchSingleRead(b *testing.B) {
	a := benchAgent(b)
	turn := benchTurn()
	ctx := context.Background()

	calls := []provider.ToolCall{
		benchToolCall("read_file", `{"path":"go.mod"}`),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.executeBatch(ctx, turn, calls)
	}
}

// BenchmarkExecuteBatchWrite measures a batch with write tools.
func BenchmarkExecuteBatchWrite(b *testing.B) {
	a := benchAgent(b)
	turn := benchTurn()
	ctx := context.Background()

	calls := []provider.ToolCall{
		benchToolCall("edit_file", `{"path":"tmp_bench.go","old_string":"a","new_string":"b"}`),
		benchToolCall("read_file", `{"path":"tmp_bench.go"}`),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.executeBatch(ctx, turn, calls)
	}
}

// BenchmarkExecuteBatchBash measures bash tool call overhead.
func BenchmarkExecuteBatchBash(b *testing.B) {
	a := benchAgent(b)
	turn := benchTurn()
	ctx := context.Background()

	calls := []provider.ToolCall{
		benchToolCall("bash", `{"command":"echo hello"}`),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.executeBatch(ctx, turn, calls)
	}
}

// BenchmarkExecuteBatch10 measures 10 mixed tool calls.
func BenchmarkExecuteBatch10(b *testing.B) {
	a := benchAgent(b)
	turn := benchTurn()
	ctx := context.Background()

	calls := make([]provider.ToolCall, 10)
	for i := range calls {
		if i%2 == 0 {
			calls[i] = benchToolCall("read_file", fmt.Sprintf(`{"path":"file_%d.go"}`, i))
		} else {
			calls[i] = benchToolCall("grep", fmt.Sprintf(`{"pattern":"func","path":"dir_%d"}`, i))
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.executeBatch(ctx, turn, calls)
	}
}

// BenchmarkExecuteBatch50 measures 50 mixed tool calls.
func BenchmarkExecuteBatch50(b *testing.B) {
	a := benchAgent(b)
	turn := benchTurn()
	ctx := context.Background()

	calls := make([]provider.ToolCall, 50)
	for i := range calls {
		if i%3 == 0 {
			calls[i] = benchToolCall("read_file", fmt.Sprintf(`{"path":"file_%d.go"}`, i))
		} else if i%3 == 1 {
			calls[i] = benchToolCall("grep", fmt.Sprintf(`{"pattern":"func","path":"dir_%d"}`, i))
		} else {
			calls[i] = benchToolCall("ls", fmt.Sprintf(`{"path":"dir_%d"}`, i))
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.executeBatch(ctx, turn, calls)
	}
}

// BenchmarkExecuteBatch100 measures 100 mixed tool calls.
func BenchmarkExecuteBatch100(b *testing.B) {
	a := benchAgent(b)
	turn := benchTurn()
	ctx := context.Background()

	calls := make([]provider.ToolCall, 100)
	for i := range calls {
		if i%4 == 0 {
			calls[i] = benchToolCall("read_file", fmt.Sprintf(`{"path":"file_%d.go"}`, i))
		} else if i%4 == 1 {
			calls[i] = benchToolCall("grep", fmt.Sprintf(`{"pattern":"TODO","path":"dir_%d"}`, i))
		} else if i%4 == 2 {
			calls[i] = benchToolCall("ls", fmt.Sprintf(`{"path":"dir_%d"}`, i))
		} else {
			calls[i] = benchToolCall("bash", fmt.Sprintf(`{"command":"echo %d"}`, i))
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.executeBatch(ctx, turn, calls)
	}
}

// BenchmarkExecuteBatchMixedReadWrite measures alternating read/write calls.
func BenchmarkExecuteBatchMixedReadWrite(b *testing.B) {
	a := benchAgent(b)
	turn := benchTurn()
	ctx := context.Background()

	calls := []provider.ToolCall{
		benchToolCall("read_file", `{"path":"file_a.go"}`),
		benchToolCall("edit_file", `{"path":"file_a.go","old_string":"x","new_string":"y"}`),
		benchToolCall("read_file", `{"path":"file_b.go"}`),
		benchToolCall("write_file", `{"path":"file_c.go","content":"new"}`),
		benchToolCall("bash", `{"command":"git diff"}`),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.executeBatch(ctx, turn, calls)
	}
}

// BenchmarkResolveCallOverhead isolates the cost of Registry.ResolveCall.
func BenchmarkResolveCallOverhead(b *testing.B) {
	reg := benchRegistry()
	names := []string{"read_file", "edit_file", "write_file", "bash", "grep", "glob", "ls", "delete_range"}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, name := range names {
			reg.ResolveCall(name)
		}
	}
}

// BenchmarkExecuteBatchWithTrace runs a mixed batch with tracing enabled
// and prints the per-stage breakdown.
func BenchmarkExecuteBatchWithTrace(b *testing.B) {
	a := benchAgent(b)
	turn := benchTurn()
	ctx := context.Background()

	calls := []provider.ToolCall{
		benchToolCall("read_file", `{"path":"file_a.go"}`),
		benchToolCall("bash", `{"command":"echo hello"}`),
		benchToolCall("edit_file", `{"path":"file_b.go","old_string":"x","new_string":"y"}`),
	}

	stop := enableTracing()
	defer stop()

	report := &traceReport{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.executeBatch(ctx, turn, calls)
		if t := a.lastTrace.Load(); t != nil {
			report.add(t)
		}
	}
	b.StopTimer()
	report.PrintReport(b.Output())
}
