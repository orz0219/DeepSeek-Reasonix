package harness

import (
	"encoding/json"

	"reasonix/internal/verification"
)

// Action is what the model decided to execute: one tool call. It is the unit
// of work the Loop hands to the ToolExecutor.
type Action struct {
	ID    string
	Tool  string
	Input json.RawMessage
}

// ObservationKind classifies where an observation came from.
type ObservationKind uint8

const (
	// ObservationTool is a real tool execution result.
	ObservationTool ObservationKind = iota
	// ObservationVerification is a failed final claim fed back to the model.
	ObservationVerification
	// ObservationBlocked is an action the Policy refused.
	ObservationBlocked
)

// Observation is what an execution produced. Tool results carry Action/Output;
// verification and blocked observations carry the structured reason. It is the
// only channel that carries results back into the model's context.
type Observation struct {
	Kind          ObservationKind
	Action        Action
	Output        string
	Success       bool
	ErrMsg        string
	Truncated     bool
	DurationMs    int64
	Paths         []string
	Mutation      bool
	Verification  verification.Result // set when Kind is ObservationVerification
	BlockedReason string              // set when Kind is ObservationBlocked
}

// NewToolObservation wraps a tool execution result.
func NewToolObservation(a Action, output string, success bool) Observation {
	return Observation{Kind: ObservationTool, Action: a, Output: output, Success: success}
}

// NewVerificationObservation renders a failed verification as feedback for the
// model's next round.
func NewVerificationObservation(a Action, result verification.Result) Observation {
	return Observation{
		Kind:         ObservationVerification,
		Action:       a,
		Success:      false,
		Verification: result,
	}
}

// NewBlockedObservation renders a policy refusal as feedback for the model.
func NewBlockedObservation(a Action, reason string) Observation {
	return Observation{Kind: ObservationBlocked, Action: a, BlockedReason: reason}
}
