package control

import (
	"context"
	"fmt"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/event"
	"reasonix/internal/extension"
	"reasonix/internal/jobs"
	"reasonix/internal/skill"
	"reasonix/internal/tool"
)

// RuntimeStatus is the frontend-facing snapshot of foreground turn state. It is
// intentionally more explicit than the legacy Running bool so UI code can
// distinguish a cancellable foreground turn from pending prompts and background
// jobs.
type RuntimeStatus struct {
	Running         bool
	PendingPrompt   bool
	BackgroundJobs  int
	CancelRequested bool
	Cancellable     bool
}

// runRefTurn resolves a line's @references into a context block and starts a
// turn with it prepended (or the raw line when nothing resolved).
func (c *Controller) runRefTurn(input, display string) {
	c.runRefTurnWithRefs(input, input, display)
}

// runRefTurnWithFormat runs a reference turn with a structured-output
// format bound to its context (symmetric with runGoalLoop's withTurnFormat
// injection — format is a property of every accepted turn, not just the
// plain-goal path; review #7234 binds format to the accepted turn).
func (c *Controller) runRefTurnWithFormat(input, display, format string) {
	c.runGuarded(func(ctx context.Context) error {
		return c.runRefTurnWithResolverSync(c.withTurnFormat(ctx, format), input, input, display, "", c.ResolveRefs)
	})
}

func (c *Controller) runScopedRefTurnWithFormat(input, display, format string) {
	c.runGuarded(func(ctx context.Context) error {
		return c.runRefTurnWithResolverSync(c.withTurnFormat(ctx, format), input, input, display, "", c.ResolveScopedRefs)
	})
}

func (c *Controller) runRefTurnWithRefsFormat(input, refLine, display, format string) {
	c.runGuarded(func(ctx context.Context) error {
		return c.runRefTurnWithResolverSync(c.withTurnFormat(ctx, format), input, refLine, display, "", c.ResolveRefs)
	})
}

func (c *Controller) runScopedRefTurnWithRefsFormat(input, refLine, display, format string) {
	c.runGuarded(func(ctx context.Context) error {
		return c.runRefTurnWithResolverSync(c.withTurnFormat(ctx, format), input, refLine, display, "", c.ResolveScopedRefs)
	})
}

func (c *Controller) runEditedRefTurnWithFormat(input, display, original, format string) {
	c.runGuarded(func(ctx context.Context) error {
		return c.runRefTurnWithResolverSync(c.withTurnFormat(ctx, format), input, input, display, original, c.ResolveRefs)
	})
}

func (c *Controller) runEditedRefTurnWithRefsFormat(input, refLine, display, original, format string) {
	c.runGuarded(func(ctx context.Context) error {
		return c.runRefTurnWithResolverSync(c.withTurnFormat(ctx, format), input, refLine, display, original, c.ResolveRefs)
	})
}

// runRefTurnWithRefs resolves references from refLine while preserving input as
// the user's actual prompt text. This lets compiler diagnostics such as
// "/path/File.kt:12: error" attach @/path/File.kt without rewriting the error.
func (c *Controller) runRefTurnWithRefs(input, refLine, display string) {
	c.runRefTurnWithResolver(input, refLine, display, c.ResolveRefs)
}

func (c *Controller) runRefTurnWithResolver(input, refLine, display string, resolve func(context.Context, string) (string, []string)) {
	c.runGuarded(func(ctx context.Context) error {
		return c.runRefTurnWithResolverSync(ctx, input, refLine, display, "", resolve)
	})
}

func (c *Controller) runRefTurnWithResolverSync(ctx context.Context, input, refLine, display, original string, resolve func(context.Context, string) (string, []string)) error {
	block, errs := resolve(ctx, refLine)
	for _, e := range errs {
		c.notice(e)
	}
	sent := input
	if block != "" {
		sent = "Referenced context:\n\n" + block + "\n\n" + input
	}
	if strings.TrimSpace(original) != "" {
		return c.runEditedGoalLoopWithImageRefsRawDisplay(ctx, sent, input, refLine, display, original)
	}
	return c.runGoalLoopWithImageRefsRawDisplay(ctx, sent, input, refLine, display)
}

// notice emits an informational Notice event.
func (c *Controller) notice(text string) {
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: text})
}

func (c *Controller) noticeDetail(text, detail string) {
	c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: text, Detail: detail})
}

// Run executes a turn synchronously, returning the agent's error. Used by the
// headless `reasonix run` path, where the Sink renders to stdout and the caller
// just needs the exit status — no TurnDone event, no cancel bookkeeping.
func (c *Controller) runReady(ctx context.Context, input string) (err error) {
	ctx = extension.ContextWithRuntimeOwner(ctx, c.RuntimeOwner())
	if c.RuntimePhase() == RuntimePhaseDraining {
		c.emitDrainingNotice()
		return ErrRuntimeDraining
	}
	defer event.RecordTurnCompletion(c.sink)
	c.maybeSessionStart(ctx)
	parentSession := c.parentSessionID()
	ctx = agent.WithParentSession(ctx, parentSession)
	ctx = jobs.WithSession(ctx, parentSession)
	rawInput := input
	ctx = c.withTurnImages(ctx, rawInput)
	ctx = agent.WithRawUserInput(ctx, rawInput)
	input = c.Compose(input)

	input, blocked, interceptErr := c.interceptInputReceive(ctx, input)
	if interceptErr != nil {
		return interceptErr
	}
	if blocked {
		return nil
	}
	startMessages := c.messageCount()
	var marker agent.InFlightTurnMeta
	defer func() { c.finishInFlightTurn(startMessages, marker) }()
	c.beginCheckpoint(ctx, input)
	if c.guardianSess != nil {
		c.guardianSess.ResetTurn()
	}
	if c.hooks.Enabled() {
		c.mu.Lock()
		c.turn++
		turn := c.turn
		c.mu.Unlock()
		if block, _ := c.hooks.PromptSubmit(ctx, input, turn); block {
			return nil
		}
		defer func() { c.hooks.StopResult(context.Background(), lastAssistantText(c.History()), turn, err) }()
	}
	marker = c.markInFlightTurn(startMessages, true)
	ctx = c.withPlannerTurnMetadata(ctx, rawInput, false, startMessages)
	err = c.runner.Run(ctx, c.withCapabilityRoute(ctx, input, rawInput))
	return err
}

// RunSubagentProfile executes one named runAs=subagent skill synchronously and
// returns only its final answer. It is the headless CLI counterpart to explicit
// slash invocation: the child keeps an isolated session, while the caller owns
// stdout rendering and exit status. readOnly selects the preview-safe runner
// used by `reasonix subagent try`.
func (c *Controller) RunSubagentProfile(ctx context.Context, name, task string, readOnly bool) (string, error) {
	name = strings.TrimSpace(name)
	task = strings.TrimSpace(task)
	if name == "" {
		return "", fmt.Errorf("subagent name is required")
	}
	if task == "" {
		return "", fmt.Errorf("subagent task is required")
	}
	sk, ok := c.skills.bySlashName(name)
	if !ok {
		return "", fmt.Errorf("unknown or disabled subagent profile %q", name)
	}
	if sk.RunAs != skill.RunSubagent {
		return "", fmt.Errorf("skill %q is not runAs=subagent", name)
	}
	sk = c.skills.prepare(sk)
	runner := c.skillRunner
	if readOnly {
		runner = c.readOnlySkillRunner
	}
	if runner == nil {
		return "", fmt.Errorf("subagent skill runner is unavailable for %q", name)
	}

	c.maybeSessionStart(ctx)
	parentSession := c.parentSessionID()
	ctx = agent.WithParentSession(ctx, parentSession)
	ctx = jobs.WithSession(ctx, parentSession)
	ctx = c.withTurnImages(ctx, task)
	ctx = agent.WithResponseLanguagePreference(ctx, c.responseLanguage)
	ctx = agent.WithReasoningLanguagePreference(ctx, c.reasoningLanguage)
	ctx = agent.WithSubagentDepth(ctx, 0)
	answer, err := runner(ctx, sk, task, skill.SubagentRunOptions{HostInitiated: true})
	if err != nil {
		return "", err
	}
	return tool.GuardSubagentHostDecisionText(answer), nil
}

// Cancel aborts the in-flight turn. A goroutine blocked awaiting approval
// unblocks via the cancelled context.
func (c *Controller) Cancel() {
	c.mu.Lock()
	cancel := c.cancel
	if cancel != nil {
		c.canceling = true
	}
	c.mu.Unlock()
	if cancel != nil {
		c.approval.clearAll()
		cancel()
		return
	}
	if c.goals.active() {
		c.stopGoal(GoalStatusStopped)
	}
}

// Running reports whether a turn is currently in flight.
func (c *Controller) Running() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running || c.finishing
}

// beginRotation claims the session-rotation gate. It fails if a turn is running
// or another rotation is already in progress, so the caller holds exclusive
// rights to swap the executor session from the check here through endRotation.
// This closes the TOCTOU window that a bare `if c.running` check left open:
// between that check and the actual SetSession, a turn could start and then be
// yanked out from under the run loop.
func (c *Controller) beginRotation() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.running || c.finishing {
		return errTurnRunningRotation
	}
	if c.rotating {
		return errRotationInProgress
	}
	c.rotating = true
	return nil
}

// CancelRequested reports whether Cancel has been requested for the active turn.
func (c *Controller) CancelRequested() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.canceling
}

// PendingPrompt reports whether the current turn is blocked waiting for a user
// approval, plan approval, memory approval, or ask-tool answer.
func (c *Controller) PendingPrompt() bool {
	return c.approval.hasPending()
}

// RuntimeStatus reports the active work owned by the foreground controller.
func (c *Controller) RuntimeStatus() RuntimeStatus {
	c.mu.Lock()
	running := c.running
	active := running || c.finishing
	canceling := c.canceling
	c.mu.Unlock()
	pending := c.approval.hasPending()
	backgroundJobs := len(c.Jobs())
	return RuntimeStatus{
		Running:         active,
		PendingPrompt:   pending,
		BackgroundJobs:  backgroundJobs,
		CancelRequested: canceling,
		Cancellable:     running || pending,
	}
}

// Turn returns the current turn number (0 before the first submit).
func (c *Controller) Turn() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turn
}
