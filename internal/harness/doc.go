// Package harness is the execution center of the harness-centric
// architecture: one AgentLoop drives every run, and all model + tool execution
// flows through it. It owns five concepts and nothing else:
//
//   - Run: which agent execution this is (internal/run).
//   - Context: what the model currently needs to know (a projection).
//   - Action: what the model decided to execute.
//   - Observation: what that execution produced.
//   - Verification: whether the work is genuinely done.
//
// The old task layer (taskintent/taskpolicy/taskcontract/evidence) is expected
// to shrink into RunSpec builders and projections as the migration proceeds;
// the Loop below is the target the legacy agent loop migrates onto.
package harness

import "reasonix/internal/event"

// EventSink is an alias for event.Sink, convenient in tests and adapters
// that need not import the event package directly.
type EventSink = event.Sink
