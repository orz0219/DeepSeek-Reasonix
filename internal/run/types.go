package run

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// RunStatus is the lifecycle state of a Run.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunPaused    RunStatus = "paused"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

// Run is the sole execution-lifecycle object: one Run is one agent execution.
// Everything the harness needs about an execution — the goal, the host
// boundaries it honors, the proofs it owes, and where it stands — lives here.
// It never writes session state or emits events itself; the harness loop owns
// both.
type Run struct {
	ID           string
	Goal         string
	Status       RunStatus
	Step         int
	MaxSteps     int
	Constraints  []Constraint
	Verification []VerificationSpec
	Limits       Limits
	// Reason names why the run ended the way it did (failure/pause/cancel).
	Reason     string
	StartedAt  time.Time
	FinishedAt *time.Time
}

// New creates a pending Run from a spec. The caller starts it with Start when
// the harness loop begins.
func New(spec RunSpec) *Run {
	return &Run{
		ID:           newRunID(),
		Goal:         spec.Goal,
		Status:       RunPending,
		MaxSteps:     spec.Limits.MaxSteps,
		Constraints:  spec.Constraints,
		Verification: spec.Verification,
		Limits:       spec.Limits,
		StartedAt:    time.Now(),
	}
}

// Start marks the run as running.
func (r *Run) Start() {
	if r == nil {
		return
	}
	r.Status = RunRunning
}

// AdvanceStep records one completed tool round.
func (r *Run) AdvanceStep() {
	if r == nil {
		return
	}
	r.Step++
}

// Complete marks the run completed. Overwrites any earlier terminal status.
func (r *Run) Complete() {
	if r == nil {
		return
	}
	r.Status = RunCompleted
	now := time.Now()
	r.FinishedAt = &now
}

// Fail marks the run failed with a reason.
func (r *Run) Fail(reason string) {
	if r == nil {
		return
	}
	r.Status = RunFailed
	r.Reason = reason
	now := time.Now()
	r.FinishedAt = &now
}

// Pause suspends the run so a caller can resume it later.
func (r *Run) Pause() {
	if r == nil {
		return
	}
	r.Status = RunPaused
}

// Resume returns a paused run to running.
func (r *Run) Resume() {
	if r == nil {
		return
	}
	r.Status = RunRunning
}

// Cancel marks the run cancelled by the user or host.
func (r *Run) Cancel() {
	if r == nil {
		return
	}
	r.Status = RunCancelled
	now := time.Now()
	r.FinishedAt = &now
}

func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "run-" + time.Now().Format("150405.000000000")
	}
	return "run-" + hex.EncodeToString(b[:])
}
