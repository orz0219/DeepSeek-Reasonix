package harness

import (
	"context"
	"errors"
	"fmt"

	"reasonix/internal/event"
	"reasonix/internal/run"
	"reasonix/internal/verification"
)

// Model is the model driver: given the model-facing context, it decides the
// next step — a final answer or a tool action.
type Model interface {
	Next(ctx context.Context, modelCtx ModelContext) (Decision, error)
}

// ToolExecutor executes one Action and returns what it produced.
type ToolExecutor interface {
	Execute(ctx context.Context, action Action) (Observation, error)
}

// Policy answers "allowed or not" for each action the model proposes. It never
// decides what the user wanted or whether the run is done — that is the
// Verifier's job. A nil Policy allows every action.
type Policy interface {
	Allows(ctx context.Context, r *run.Run, action Action) (allow bool, reason string)
}

// maxBlockedRounds is the number of consecutive policy-blocked rounds before
// the loop pauses the run instead of looping forever.
const maxBlockedRounds = 5

// maxVerifyFailRounds is the number of consecutive verification failures
// before the loop pauses the run instead of looping forever.
const maxVerifyFailRounds = 3

// Loop is the harness execution center: every run drives context -> model ->
// action -> tool, folds observations back into context, and routes "done"
// through the Verifier. All tool execution must flow through it; nothing else
// may drive the model.
type Loop struct {
	Model     Model
	Context   ContextManager
	Tools     ToolExecutor
	Verifier  verification.Verifier
	EventSink event.Sink
	Policy    Policy
}

// Run drives one execution lifecycle to a terminal state. It returns nil on
// clean completion, a step/limit pause (run.Status == run.RunPaused, resumable
// by the caller), or ctx.Err() on cancellation; model/tool-independent errors
// return non-nil with run.Status == run.RunFailed.
func (l *Loop) Run(ctx context.Context, r *run.Run) error {
	if err := l.validate(); err != nil {
		return err
	}
	if r == nil {
		return errors.New("harness: nil run")
	}
	l.emitRun(event.RunStarted, r, "")
	r.Start()

	blockedStreak := 0
	verifyFailStreak := 0

	for !l.shouldStop(ctx, r) {
		modelCtx := l.Context.Build(ctx, r)
		l.emitRun(event.ModelRequested, r, "")
		decision, err := l.Model.Next(ctx, modelCtx)
		if err != nil {
			return l.fail(ctx, r, "model", err)
		}
		l.emitRun(event.ModelResponded, r, decisionSummary(decision))

		if decision.IsFinal() {
			result := l.verify(ctx, r, decision)
			if result.Passed {
				return l.complete(ctx, r, result)
			}
			l.emitRun(event.VerificationFailed, r, result.Reason)
			l.Context.AppendObservation(ctx, r, NewVerificationObservation(toolActionOrZero(decision), result))
			verifyFailStreak++
			if verifyFailStreak >= maxVerifyFailRounds {
				return l.pause(ctx, r, "verification_failed")
			}
			continue
		}
		verifyFailStreak = 0 // reset on non-final decision

		action, ok := decision.ToolAction()
		if !ok {
			return l.fail(ctx, r, "model", errors.New("harness: decision is neither final nor an action"))
		}
		if allow, reason := l.allows(ctx, r, action); !allow {
			l.Context.AppendObservation(ctx, r, NewBlockedObservation(action, reason))
			blockedStreak++
			if blockedStreak >= maxBlockedRounds {
				return l.pause(ctx, r, "policy_blocked")
			}
			continue
		}
		blockedStreak = 0 // reset on successful dispatch

		l.emitTool(event.ToolStarted, r, action, "")
		observation, err := l.Tools.Execute(ctx, action)
		if err != nil {
			l.emitTool(event.ToolFailed, r, action, err.Error())
			l.Context.AppendObservation(ctx, r, Observation{
				Kind: ObservationTool, Action: action, Success: false, ErrMsg: err.Error(),
			})
			continue
		}
		l.emitTool(event.ToolCompleted, r, action, observation.Output)
		l.Context.AppendObservation(ctx, r, observation)
		r.AdvanceStep()
	}
	return l.stop(ctx, r)
}

func (l *Loop) validate() error {
	if l == nil {
		return errors.New("harness: nil loop")
	}
	if l.Model == nil {
		return errors.New("harness: missing model")
	}
	if l.Context == nil {
		return errors.New("harness: missing context manager")
	}
	if l.Tools == nil {
		return errors.New("harness: missing tool executor")
	}
	if l.Verifier == nil {
		return errors.New("harness: missing verifier")
	}
	if l.EventSink == nil {
		return errors.New("harness: missing event sink")
	}
	return nil
}

func (l *Loop) allows(ctx context.Context, r *run.Run, a Action) (bool, string) {
	if l.Policy == nil {
		return true, ""
	}
	return l.Policy.Allows(ctx, r, a)
}

func (l *Loop) verify(ctx context.Context, r *run.Run, d Decision) verification.Result {
	l.emitRun(event.VerificationStarted, r, "")
	return l.Verifier.Verify(ctx, r, verification.Claim{Text: d.Text})
}

func (l *Loop) complete(ctx context.Context, r *run.Run, result verification.Result) error {
	r.Complete()
	l.emitRun(event.RunCompleted, r, "")
	return nil
}

func (l *Loop) fail(ctx context.Context, r *run.Run, source string, err error) error {
	message := source + ": " + err.Error()
	r.Fail(message)
	l.emitRun(event.RunFailed, r, message)
	return err
}

// pause suspends the run with a reason and emits the pause event.
func (l *Loop) pause(_ context.Context, r *run.Run, reason string) error {
	r.Pause()
	r.Reason = reason
	l.emitRun(event.RunPaused, r, reason)
	return fmt.Errorf("harness: pause: %s", reason)
}

// Emit forwards one event to the configured sink.
func (l *Loop) Emit(e event.Event) {
	if l != nil && l.EventSink != nil {
		l.EventSink.Emit(e)
	}
}

func (l *Loop) emitRun(kind event.Kind, r *run.Run, detail string) {
	l.Emit(event.Event{Kind: kind, Text: r.ID, Detail: detail})
}

func (l *Loop) emitTool(kind event.Kind, r *run.Run, a Action, output string) {
	l.Emit(event.Event{Kind: kind, Text: r.ID, Tool: event.Tool{ID: a.ID, Name: a.Tool, Args: string(a.Input), Output: output}})
}

func decisionSummary(d Decision) string {
	if d.IsFinal() {
		return "final"
	}
	if d.Action != nil {
		return "tool:" + d.Action.Tool
	}
	return "none"
}

func toolActionOrZero(d Decision) Action {
	if d.Action != nil {
		return *d.Action
	}
	return Action{}
}
