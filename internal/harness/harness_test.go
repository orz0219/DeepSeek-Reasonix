package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/run"
	"reasonix/internal/verification"
)

// --- Loop tests ---

// stubModel returns the next decision from a queue.
type stubModel struct {
	decisions []Decision
	idx       int
}

func (m *stubModel) Next(_ context.Context, _ ModelContext) (Decision, error) {
	if m.idx >= len(m.decisions) {
		return FinalAnswer("default-done"), nil
	}
	d := m.decisions[m.idx]
	m.idx++
	return d, nil
}

// stubExecutor records every action and returns a fixed output.
type stubExecutor struct {
	actions []Action
	output  string
	err     error
}

func (e *stubExecutor) Execute(_ context.Context, a Action) (Observation, error) {
	e.actions = append(e.actions, a)
	return Observation{Kind: ObservationTool, Action: a, Output: e.output, Success: e.err == nil}, e.err
}

// stubVerifier always passes.
type stubVerifier struct{}

func (v *stubVerifier) Verify(_ context.Context, _ *run.Run, _ verification.Claim) verification.Result {
	return verification.Result{Passed: true, Kind: verification.KindCommand}
}

// failingVerifier always fails.
type failingVerifier struct{}

func (v *failingVerifier) Verify(_ context.Context, _ *run.Run, _ verification.Claim) verification.Result {
	return verification.Result{Passed: false, Kind: verification.KindCommand, Reason: "not done"}
}

func TestLoopRunFinalAnswer(t *testing.T) {
	loop := &Loop{
		Model:     &stubModel{decisions: []Decision{FinalAnswer("done")}},
		Context:   &MemoryContext{},
		Tools:     &stubExecutor{output: "ok"},
		Verifier:  &stubVerifier{},
		EventSink: &collectSink{},
	}
	r := run.New(run.RunSpec{Goal: "test", Limits: run.Limits{MaxSteps: 10}})
	err := loop.Run(context.Background(), r)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if r.Status != run.RunCompleted {
		t.Errorf("Status = %v, want RunCompleted", r.Status)
	}
}

func TestLoopRunToolThenFinal(t *testing.T) {
	loop := &Loop{
		Model: &stubModel{decisions: []Decision{
			TakeAction(Action{ID: "a1", Tool: "bash", Input: json.RawMessage(`{"command":"echo hi"}`)}),
			FinalAnswer("done"),
		}},
		Context:   &MemoryContext{},
		Tools:     &stubExecutor{output: "hi"},
		Verifier:  &stubVerifier{},
		EventSink: &collectSink{},
	}
	r := run.New(run.RunSpec{Goal: "test", Limits: run.Limits{MaxSteps: 10}})
	err := loop.Run(context.Background(), r)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if r.Status != run.RunCompleted {
		t.Errorf("Status = %v, want RunCompleted", r.Status)
	}
	if r.Step != 1 {
		t.Errorf("Step = %d, want 1", r.Step)
	}
}

func TestLoopVerificationFailedRetries(t *testing.T) {
	// Model says final 3 times (all fail), then takes an action
	loop := &Loop{
		Model: &stubModel{decisions: []Decision{
			FinalAnswer("attempt 1"),
			FinalAnswer("attempt 2"),
			FinalAnswer("attempt 3"),
			TakeAction(Action{ID: "a1", Tool: "bash", Input: json.RawMessage(`{"command":"echo fix"}`)}),
		}},
		Context:   &MemoryContext{},
		Tools:     &stubExecutor{output: "ok"},
		Verifier:  &failingVerifier{},
		EventSink: &collectSink{},
	}
	r := run.New(run.RunSpec{Goal: "test", Limits: run.Limits{MaxSteps: 10}})
	err := loop.Run(context.Background(), r)
	// After 3 failed verifications it pauses (maxVerifyFailRounds=3)
	if err == nil {
		t.Fatal("expected error from verify-fail pause")
	}
	if !strings.Contains(err.Error(), "verification_failed") {
		t.Errorf("err = %v, want verification_failed", err)
	}
	if r.Status != run.RunPaused {
		t.Errorf("Status = %v, want RunPaused", r.Status)
	}
}

func TestLoopMaxStepsPause(t *testing.T) {
	loop := &Loop{
		Model: &stubModel{decisions: []Decision{
			TakeAction(Action{ID: "a1", Tool: "bash", Input: json.RawMessage(`{"command":"echo x"}`)}),
			TakeAction(Action{ID: "a2", Tool: "bash", Input: json.RawMessage(`{"command":"echo y"}`)}),
			TakeAction(Action{ID: "a3", Tool: "bash", Input: json.RawMessage(`{"command":"echo z"}`)}),
			TakeAction(Action{ID: "a4", Tool: "bash", Input: json.RawMessage(`{"command":"echo w"}`)}),
		}},
		Context:   &MemoryContext{},
		Tools:     &stubExecutor{output: "ok"},
		Verifier:  &stubVerifier{},
		EventSink: &collectSink{},
	}
	r := run.New(run.RunSpec{Goal: "test", Limits: run.Limits{MaxSteps: 3}})
	err := loop.Run(context.Background(), r)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if r.Status != run.RunPaused {
		t.Errorf("Status = %v, want RunPaused", r.Status)
	}
	if r.Reason != "max_steps" {
		t.Errorf("Reason = %q, want %q", r.Reason, "max_steps")
	}
}

func TestLoopContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	loop := &Loop{
		Model:     &stubModel{decisions: []Decision{FinalAnswer("done")}},
		Context:   &MemoryContext{},
		Tools:     &stubExecutor{},
		Verifier:  &stubVerifier{},
		EventSink: &collectSink{},
	}
	r := run.New(run.RunSpec{Goal: "test", Limits: run.Limits{MaxSteps: 10}})
	err := loop.Run(ctx, r)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if r.Status != run.RunCancelled {
		t.Errorf("Status = %v, want RunCancelled", r.Status)
	}
}

func TestLoopPolicyBlocks(t *testing.T) {
	executor := &stubExecutor{output: "ok"}
	loop := &Loop{
		Model: &stubModel{decisions: []Decision{
			TakeAction(Action{ID: "a1", Tool: "writefile", Input: json.RawMessage(`{"content":"x"}`)}),
			TakeAction(Action{ID: "a2", Tool: "writefile", Input: json.RawMessage(`{"content":"y"}`)}),
		}},
		Context:   &MemoryContext{},
		Tools:     executor,
		Verifier:  &stubVerifier{},
		EventSink: &collectSink{},
		Policy:    &constraintPolicy{forbidMutation: true},
	}
	r := run.New(run.RunSpec{Goal: "test", Limits: run.Limits{MaxSteps: 10}})
	err := loop.Run(context.Background(), r)
	// After maxBlockedRounds=5 blocks it pauses; our 2 blocks fit before that
	// so it finishes the decision queue and exits normally.
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(executor.actions) != 0 {
		t.Errorf("executor saw %d actions, want 0 (should be blocked)", len(executor.actions))
	}
}

func TestLoopPolicyBlocksPause(t *testing.T) {
	// Model keeps proposing blocked actions — should pause after streak
	decisions := make([]Decision, 10)
	for i := range decisions {
		decisions[i] = TakeAction(Action{ID: "a" + string(rune('0'+i)), Tool: "writefile"})
	}
	executor := &stubExecutor{output: "ok"}
	loop := &Loop{
		Model:     &stubModel{decisions: decisions},
		Context:   &MemoryContext{},
		Tools:     executor,
		Verifier:  &stubVerifier{},
		EventSink: &collectSink{},
		Policy:    &constraintPolicy{forbidMutation: true},
	}
	r := run.New(run.RunSpec{Goal: "test", Limits: run.Limits{MaxSteps: 100}})
	err := loop.Run(context.Background(), r)
	if err == nil {
		t.Fatal("expected error from blocked pause")
	}
	if !strings.Contains(err.Error(), "policy_blocked") {
		t.Errorf("err = %v, want policy_blocked", err)
	}
	if r.Status != run.RunPaused {
		t.Errorf("Status = %v, want RunPaused", r.Status)
	}
	if len(executor.actions) != 0 {
		t.Errorf("executor saw %d actions, want 0", len(executor.actions))
	}
}

func TestLoopNilValidation(t *testing.T) {
	loop := &Loop{}
	err := loop.Run(context.Background(), nil)
	if err == nil {
		t.Error("expected error for nil run")
	}

	loop2 := &Loop{Model: &stubModel{decisions: []Decision{FinalAnswer("x")}}}
	err = loop2.Run(context.Background(), run.New(run.RunSpec{Goal: "x"}))
	if err == nil {
		t.Error("expected error for incomplete loop")
	}
}

func TestLoopToolError(t *testing.T) {
	loop := &Loop{
		Model: &stubModel{decisions: []Decision{
			TakeAction(Action{ID: "a1", Tool: "bash"}),
			FinalAnswer("done"),
		}},
		Context:   &MemoryContext{},
		Tools:     &stubExecutor{err: errors.New("tool crashed")},
		Verifier:  &stubVerifier{},
		EventSink: &collectSink{},
	}
	r := run.New(run.RunSpec{Goal: "test", Limits: run.Limits{MaxSteps: 10}})
	err := loop.Run(context.Background(), r)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	// Tool error should not complete the run — step not advanced, still goes to final
	if r.Status != run.RunCompleted {
		t.Errorf("Status = %v, want RunCompleted", r.Status)
	}
}

// --- Policy tests ---

type constraintPolicy struct {
	forbidMutation bool
	forbidTests    bool
}

func (p *constraintPolicy) Allows(_ context.Context, _ *run.Run, a Action) (bool, string) {
	if p.forbidMutation && strings.Contains(a.Tool, "write") {
		return false, "mutation forbidden"
	}
	if p.forbidTests && a.Tool == "bash" {
		return false, "tests forbidden"
	}
	return true, ""
}

func TestConstraintPolicyAllows(t *testing.T) {
	p := NewConstraintPolicy([]run.Constraint{
		{Kind: run.ConstraintForbidMutation},
	})
	if !p.ForbidMutation {
		t.Error("ForbidMutation not set")
	}
	allow, _ := p.Allows(context.Background(), nil, Action{Tool: "readfile"})
	if !allow {
		t.Error("read should be allowed")
	}
	allow, _ = p.Allows(context.Background(), nil, Action{Tool: "writefile"})
	if allow {
		t.Error("write should be forbidden")
	}
}

// --- Context tests ---

func TestMemoryContext(t *testing.T) {
	mc := &MemoryContext{}
	r := run.New(run.RunSpec{Goal: "test", Limits: run.Limits{MaxSteps: 10}})
	r.Start()

	ctx := mc.Build(context.Background(), r)
	if ctx.Goal != "test" {
		t.Errorf("Goal = %q", ctx.Goal)
	}
	if len(ctx.Observations) != 0 {
		t.Errorf("Observations = %d, want 0", len(ctx.Observations))
	}

	obs := Observation{Kind: ObservationTool, Output: "result", Success: true}
	mc.AppendObservation(context.Background(), r, obs)

	ctx = mc.Build(context.Background(), r)
	if len(ctx.Observations) != 1 {
		t.Errorf("Observations = %d, want 1", len(ctx.Observations))
	}
	if ctx.Observations[0].Output != "result" {
		t.Errorf("Observations[0].Output = %q", ctx.Observations[0].Output)
	}
}

func TestMemoryContextNilSafety(t *testing.T) {
	var mc *MemoryContext
	r := run.New(run.RunSpec{Goal: "x"})
	_ = mc.Build(context.Background(), r)
	mc.AppendObservation(context.Background(), r, Observation{})
}

// --- Decision tests ---

func TestDecisionFinalAnswer(t *testing.T) {
	d := FinalAnswer("hello")
	if !d.IsFinal() {
		t.Error("IsFinal = false, want true")
	}
	if d.Answer() != "hello" {
		t.Errorf("Answer = %q", d.Answer())
	}
	if _, ok := d.ToolAction(); ok {
		t.Error("ToolAction should return false for final")
	}
}

func TestDecisionTakeAction(t *testing.T) {
	a := Action{ID: "x", Tool: "bash"}
	d := TakeAction(a)
	if d.IsFinal() {
		t.Error("IsFinal = true, want false")
	}
	got, ok := d.ToolAction()
	if !ok {
		t.Error("ToolAction should return true")
	}
	if got.Tool != "bash" {
		t.Errorf("ToolAction.Tool = %q", got.Tool)
	}
}

// --- Action / Observation tests ---

func TestNewToolObservation(t *testing.T) {
	a := Action{ID: "a1", Tool: "bash"}
	obs := NewToolObservation(a, "output", true)
	if obs.Kind != ObservationTool {
		t.Errorf("Kind = %d, want ObservationTool", obs.Kind)
	}
	if obs.Output != "output" {
		t.Errorf("Output = %q", obs.Output)
	}
	if !obs.Success {
		t.Error("Success = false, want true")
	}
}

func TestNewBlockedObservation(t *testing.T) {
	a := Action{ID: "a1", Tool: "writefile"}
	obs := NewBlockedObservation(a, "forbidden")
	if obs.Kind != ObservationBlocked {
		t.Errorf("Kind = %d, want ObservationBlocked", obs.Kind)
	}
	if obs.BlockedReason != "forbidden" {
		t.Errorf("BlockedReason = %q", obs.BlockedReason)
	}
}

func TestNewVerificationObservation(t *testing.T) {
	a := Action{ID: "a1", Tool: "bash"}
	result := verification.Result{Passed: false, Reason: "failed"}
	obs := NewVerificationObservation(a, result)
	if obs.Kind != ObservationVerification {
		t.Errorf("Kind = %d, want ObservationVerification", obs.Kind)
	}
	if obs.Verification.Reason != "failed" {
		t.Errorf("Verification.Reason = %q", obs.Verification.Reason)
	}
}

// --- collectSink ---

type collectSink struct {
	events []event.Event
}

func (s *collectSink) Emit(e event.Event) {
	s.events = append(s.events, e)
}

var _ event.Sink = (*collectSink)(nil)
