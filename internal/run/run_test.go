package run

import (
	"testing"
	"time"
)

func TestNewRun(t *testing.T) {
	spec := RunSpec{
		Goal:   "implement feature X",
		Intent: IntentMutation,
		Constraints: []Constraint{
			{Kind: ConstraintForbidExternal},
		},
		Verification: []VerificationSpec{
			{Kind: VerificationCommand, Command: "go test ./..."},
		},
		Limits: Limits{MaxSteps: 10},
	}
	r := New(spec)
	if r == nil {
		t.Fatal("New returned nil")
	}
	if r.Goal != "implement feature X" {
		t.Errorf("Goal = %q, want %q", r.Goal, "implement feature X")
	}
	if r.Status != RunPending {
		t.Errorf("Status = %v, want RunPending", r.Status)
	}
	if r.MaxSteps != 10 {
		t.Errorf("MaxSteps = %d, want 10", r.MaxSteps)
	}
	if len(r.Constraints) != 1 {
		t.Errorf("len(Constraints) = %d, want 1", len(r.Constraints))
	}
	if len(r.Verification) != 1 {
		t.Errorf("len(Verification) = %d, want 1", len(r.Verification))
	}
	if r.ID == "" {
		t.Error("ID is empty")
	}
	if r.StartedAt.IsZero() {
		t.Error("StartedAt is zero")
	}
}

func TestRunLifecycle(t *testing.T) {
	spec := RunSpec{Goal: "test", Limits: Limits{MaxSteps: 5}}
	r := New(spec)

	// Pending → Running
	r.Start()
	if r.Status != RunRunning {
		t.Errorf("after Start: Status = %v, want RunRunning", r.Status)
	}

	// Running → step advance
	r.AdvanceStep()
	r.AdvanceStep()
	if r.Step != 2 {
		t.Errorf("after 2 AdvanceStep: Step = %d, want 2", r.Step)
	}

	// Running → Paused → Running
	r.Pause()
	if r.Status != RunPaused {
		t.Errorf("after Pause: Status = %v, want RunPaused", r.Status)
	}
	r.Resume()
	if r.Status != RunRunning {
		t.Errorf("after Resume: Status = %v, want RunRunning", r.Status)
	}

	// Running → Completed
	r.Complete()
	if r.Status != RunCompleted {
		t.Errorf("after Complete: Status = %v, want RunCompleted", r.Status)
	}
	if r.FinishedAt == nil {
		t.Error("FinishedAt is nil after Complete")
	}
}

func TestRunFail(t *testing.T) {
	r := New(RunSpec{Goal: "test"})
	r.Start()
	r.Fail("model error")
	if r.Status != RunFailed {
		t.Errorf("Status = %v, want RunFailed", r.Status)
	}
	if r.Reason != "model error" {
		t.Errorf("Reason = %q, want %q", r.Reason, "model error")
	}
}

func TestRunCancel(t *testing.T) {
	r := New(RunSpec{Goal: "test"})
	r.Start()
	r.Cancel()
	if r.Status != RunCancelled {
		t.Errorf("Status = %v, want RunCancelled", r.Status)
	}
}

func TestRunStatusTerminal(t *testing.T) {
	tests := []struct {
		status   RunStatus
		terminal bool
	}{
		{RunPending, false},
		{RunRunning, false},
		{RunPaused, false},
		{RunCompleted, true},
		{RunFailed, true},
		{RunCancelled, true},
	}
	for _, tt := range tests {
		if got := tt.status.Terminal(); got != tt.terminal {
			t.Errorf("Status(%q).Terminal() = %v, want %v", tt.status, got, tt.terminal)
		}
	}
}

func TestRunNilSafety(t *testing.T) {
	var r *Run
	// All methods must be nil-safe
	r.Start()
	r.AdvanceStep()
	r.Complete()
	r.Fail("x")
	r.Pause()
	r.Resume()
	r.Cancel()
}

func TestBuilder(t *testing.T) {
	spec := NewBuilder().
		WithGoal("fix bug").
		WithIntent(IntentMutation).
		AddConstraint(Constraint{Kind: ConstraintForbidTests}).
		AddVerification(VerificationSpec{Kind: VerificationCommand, Command: "go vet ./..."}).
		WithLimits(Limits{MaxSteps: 7}).
		Build()

	if spec.Goal != "fix bug" {
		t.Errorf("Goal = %q", spec.Goal)
	}
	if spec.Intent != IntentMutation {
		t.Errorf("Intent = %v", spec.Intent)
	}
	if len(spec.Constraints) != 1 {
		t.Errorf("len(Constraints) = %d", len(spec.Constraints))
	}
	if len(spec.Verification) != 1 {
		t.Errorf("len(Verification) = %d", len(spec.Verification))
	}
	if spec.Limits.MaxSteps != 7 {
		t.Errorf("MaxSteps = %d", spec.Limits.MaxSteps)
	}
}

func TestRunIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		r := New(RunSpec{Goal: "x"})
		if seen[r.ID] {
			t.Fatalf("duplicate ID %q on iteration %d", r.ID, i)
		}
		seen[r.ID] = true
	}
}

func TestRunFinishedAtTimestamp(t *testing.T) {
	r := New(RunSpec{Goal: "test"})
	r.Start()
	before := time.Now()
	r.Complete()
	after := time.Now()
	if r.FinishedAt == nil {
		t.Fatal("FinishedAt is nil")
	}
	if r.FinishedAt.Before(before) || r.FinishedAt.After(after) {
		t.Errorf("FinishedAt %v not in [%v, %v]", *r.FinishedAt, before, after)
	}
}
