package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/event"
	"reasonix/internal/extension"
	"reasonix/internal/provider"
	"reasonix/internal/skill"
)

// spawnGuardedTurn launches an admitted turn body plus its autosave companion.
// The caller must already have claimed admission (running=true) under c.mu.
func (c *Controller) spawnGuardedTurn(ctx context.Context, cancel context.CancelFunc, body func(ctx context.Context) error) {
	ctx, completion := withGuardedTurnCompletion(ctx)
	c.autosaveWG.Go(func() {
		c.autosaveWhileRunning(ctx)
	})
	go func() {
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				c.finishGuardedTurn(fmt.Errorf("internal error: %v", r), completion)
			}
		}()
		err := body(ctx)
		c.finishGuardedTurn(explainError(err), completion)
	}()
}

// finishGuardedTurn keeps admission closed while TurnDone is delivered. The
// sink fan-out may detach per-turn transports; allowing a replacement turn in
// after running=false but before that fan-out completed let the old completion
// clear or inherit the replacement turn's transport.
//
// When the window closes, the oldest parked turn (if any) is started under the
// SAME critical section that clears finishing: opening the gate first and then
// re-admitting would let an unrelated submit slip in ahead and bounce the
// parked turn back to a drop. Remaining parked turns drain one per
// finishGuardedTurn, preserving FIFO order. Rotation cannot interleave here:
// beginRotation refuses while running or finishing, and the drain flips
// finishing directly into running.
func (c *Controller) finishGuardedTurn(err error, completion *guardedTurnCompletion) {
	c.mu.Lock()
	cancelRequested := c.canceling
	c.running = false

	c.finishing = !c.closed
	c.cancel = nil
	c.canceling = false
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.finishing = false
		if c.closed {
			c.mu.Unlock()
			return
		}
		if len(c.parkedTurns) == 0 {
			c.mu.Unlock()

			c.maybeDispatchInbox()
			return
		}
		next := c.parkedTurns[0]
		c.parkedTurns = c.parkedTurns[1:]
		ctx, cancel := context.WithCancel(extension.ContextWithRuntimeOwner(context.Background(), c.runtimeOwner))
		c.cancel = cancel
		c.running = true
		c.canceling = false
		c.mu.Unlock()
		c.spawnGuardedTurn(ctx, cancel, next)
	}()
	c.inbox.mu.Lock()

	activeInboxID := ""
	for id := range c.inbox.activeItemIDs {
		activeInboxID = id
		break
	}
	c.inbox.mu.Unlock()
	done := event.Event{
		Kind:           event.TurnDone,
		Err:            err,
		Cancelled:      cancelRequested,
		Outcome:        turnOutcome(err),
		CheckpointTurn: c.validatedCheckpointTurn(completion),
		Receipt:        c.executor.CompletionReceipt(),
		ItemID:         activeInboxID,
	}
	var readinessErr *agent.FinalReadinessError
	if errors.As(err, &readinessErr) {
		done.Readiness = &event.FinalReadiness{Attempts: readinessErr.Attempts, Missing: append([]string(nil), readinessErr.Missing...)}
	}

	c.onInboxTurnDone()
	c.sink.Emit(done)
}

func turnOutcome(err error) string {
	var readinessErr *agent.FinalReadinessError
	if errors.As(err, &readinessErr) {
		return event.TurnOutcomeFinalReadiness
	}
	var pauseErr *agent.RecoveryPauseError
	if errors.As(err, &pauseErr) {
		return event.TurnOutcomeRecoveryPaused
	}
	return ""
}

// Send starts a turn with an uncomposed message. The controller applies
// plan-mode, memory, and background-job framing inside the async turn path.
func (c *Controller) Send(input string) {
	c.SendWithRaw(input, input)
}

// SendWithRaw starts a turn with separate model input and raw prompt text.
func (c *Controller) SendWithRaw(input, raw string) {
	c.runGuarded(func(ctx context.Context) error { return c.runGoalLoopWithRaw(ctx, input, raw) })
}

// runTurn runs one model turn, then applies the plan-approval gate. This is the
// single, frontend-agnostic plan flow: in Plan the model is instructed to
// research and write its plan as a normal answer, while any tool calls still use
// the active Permissions/Sandbox path.
// When the turn ends with a text proposal, the controller asks the user to
// approve (reusing the ApprovalRequest channel both frontends already render);
// on approval it exits plan mode, seeds the task list from the plan, and
// continues straight into execution; on rejection it stays in plan mode so the
// next turn can revise. Plan mode is only ever set interactively, so the headless
// `Run` path (which doesn't call this) never blocks on a prompt.
func (c *Controller) runTurn(ctx context.Context, input string) error {
	return c.runGoalLoopWithRaw(ctx, input, input)
}

// RunTurn executes one foreground turn synchronously through the same lifecycle
// used by interactive frontends: transient memory/background-job
// composition, checkpoints, hooks, and plan approval. It is for transports that
// need a blocking request/response boundary, such as ACP session/prompt.
func (c *Controller) RunTurn(ctx context.Context, input string) error {
	return c.runSynchronousTurn(ctx, nil, func(runCtx context.Context) error {
		return c.runTurn(runCtx, input)
	})
}

func (c *Controller) runGoalLoopWithRaw(ctx context.Context, input, raw string) error {
	return c.runGoalLoopWithRawDisplay(ctx, input, raw, "")
}

// withTurnFormat binds a structured-output format to the turn context
// (empty is a no-op). Extracted from the runGoalLoop closure so tests can
// assert the format actually reaches the agent request path.
func (c *Controller) withTurnFormat(ctx context.Context, format string) context.Context {
	if format == "" {
		return ctx
	}
	return agent.WithResponseFormat(ctx, format)
}

func (c *Controller) runGoalLoopWithRawDisplay(ctx context.Context, input, raw, display string) error {

	return newTurnOrchestrator(c).runGoalLoopWithRawDisplay(ctx, input, raw, display)
}

func (c *Controller) runEditedGoalLoopWithRawDisplay(ctx context.Context, input, raw, display, original string) error {
	return newTurnOrchestrator(c).runEditedGoalLoopWithRawDisplay(ctx, input, raw, display, original)
}

func (c *Controller) runSubagentSkillSlash(sk skill.Skill, task, raw, display string) {
	sk = c.skills.prepare(sk)
	c.runGuarded(func(ctx context.Context) error {
		planMode := c.PlanMode()
		runner := c.skillRunner
		if runner == nil {
			return fmt.Errorf("subagent skill runner is unavailable for /%s", sk.Name)
		}
		return newTurnOrchestrator(c).runSubagentSkillGoalLoop(ctx, sk, task, raw, display, runner, planMode)
	})
}

func (c *Controller) stopGoal(status string) {
	path, data, ok := c.goals.stop(status, c.goalTodos())
	c.persistGoalState(path, data, ok)
}

// lastAssistantText returns the content of the most recent assistant message with
// non-empty text — the model's final answer for the turn (its plan, in plan mode).
func lastAssistantText(msgs []provider.Message) string {
	for _, msg := range slices.Backward(msgs) {
		if msg.Role == provider.RoleAssistant && strings.TrimSpace(msg.Content) != "" {
			return msg.Content
		}
	}
	return ""
}

// Submit is the one-call entry for a simple frontend: it takes raw user input
// and does everything — slash-command dispatch, @-reference expansion, plan-mode
// composition — emitting all output as events. The HTTP/SSE server uses this so
// a browser client only POSTs the typed line.
//
// Slash commands route to the matching primitive: /compact, /new, and /clear
// run their session op and emit a Notice; /mcp__server__prompt and custom /commands
// resolve to a turn; an unknown slash emits a Notice. Anything else is a normal
// turn with its @-references resolved first.
func (c *Controller) Submit(input string) {
	c.submit(input, "", "")
}

// SubmitHTTP accepts input from the unauthenticated localhost HTTP frontend. It
// deliberately omits the trusted TUI-only "!cmd" shell shortcut and resolves file
// references only through the controller's workspace root.
func (c *Controller) SubmitHTTP(input string) {
	c.submitHTTP(input, "")
}

// SubmitHTTPFormat is SubmitHTTP with an optional structured-output format
// ("json_object") applied to the turn's completion requests. Empty format
// behaves exactly like SubmitHTTP. A format attached to a slash command,
// or other non-turn input is discarded; @reference turns preserve it because
// the format is bound to every submitted turn rather than a global slot.
func (c *Controller) SubmitHTTPFormat(input, format string) {

	f := strings.TrimSpace(format)
	if f != "" && isNonTurnHTTPInput(input) {
		f = ""
	}

	c.submitHTTPWithFormat(input, "", f)
}

// isNonTurnHTTPInput reports inputs that never reach the agent turn loop, so a
// structured-output request attached to them would otherwise leak into the
// next real turn (the format slot is consumed only by runGoalLoopWithRawDisplay).
func isNonTurnHTTPInput(input string) bool {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return true
	}

	if _, ok := RememberCommandNote(trimmed); ok {
		return true
	}

	if strings.HasPrefix(trimmed, "!") {
		return true
	}

	if strings.HasPrefix(trimmed, "/") {
		return true
	}
	return false
}

// SubmitDisplay runs input as a turn while remembering the user-facing display
// text for transcript replay when controller-side composition expands input.
func (c *Controller) SubmitDisplay(display, input string) {
	c.submit(input, display, "")
}

func (c *Controller) SubmitDeliveryRecovery(display, input string) { c.deliver(display, input, false) }
func (c *Controller) SubmitDeliveryWaiver(display, input string)   { c.deliver(display, input, true) }
func (c *Controller) deliver(display, input string, waiver bool) {
	c.runGuarded(func(ctx context.Context) error {
		if c.executor != nil && waiver {
			c.executor.PrepareDeliveryWaiver()
		} else if c.executor != nil {
			c.executor.PrepareDeliveryRecovery()
		}
		return c.runGoalLoopWithRawDisplay(ctx, input, input, display)
	})
}

// SubmitInvocationDisplay executes composer-selected invocation entities
// independently of slash-command parsing. Plain string submit entry points keep
// their existing behavior for CLI, HTTP, and backward-compatible clients.
func (c *Controller) SubmitInvocationDisplay(display, input string, invocations []InvocationRequest) {
	c.submitInvocations(input, display, invocations)
}

func (c *Controller) submitInvocations(input, display string, requests []InvocationRequest) {
	if len(requests) == 0 {
		c.SubmitDisplay(display, input)
		return
	}
	prepared, err := c.prepareInvocationTurn(input, requests)
	if err != nil {
		c.notice(err.Error())
		return
	}
	c.runGuarded(func(ctx context.Context) error {
		return c.runPreparedInvocationTurn(ctx, prepared, input, input, display, nil)
	})
}

type preparedInvocationTurn struct {
	composed  string
	subagents []skill.Skill
}

func (c *Controller) prepareInvocationTurn(input string, requests []InvocationRequest) (preparedInvocationTurn, error) {
	ordered := append([]InvocationRequest(nil), requests...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Offset < ordered[j].Offset })
	inline := make([]skill.Skill, 0, len(ordered))
	subagents := make([]skill.Skill, 0, len(ordered))
	for _, request := range ordered {
		sk, _, ok := c.resolveSkillInvocation("/" + strings.TrimSpace(request.Name))
		if !ok {
			return preparedInvocationTurn{}, fmt.Errorf("unknown invocation: /%s", strings.TrimSpace(request.Name))
		}
		kind := "skill"
		if sk.RunAs == skill.RunSubagent {
			kind = "subagent"
		}
		if strings.TrimSpace(request.Kind) != "" && request.Kind != kind {
			return preparedInvocationTurn{}, fmt.Errorf("invocation /%s is %s, not %s", sk.SlashName(), kind, request.Kind)
		}
		if sk.RunAs == skill.RunSubagent {
			subagents = append(subagents, sk)
		} else {
			inline = append(inline, sk)
		}
	}

	parts := make([]string, 0, len(inline)+1)
	for _, sk := range inline {
		parts = append(parts, c.skills.render(sk, ""))
	}
	if strings.TrimSpace(input) != "" {
		parts = append(parts, input)
	}
	composed := strings.Join(parts, "\n\n")
	if strings.TrimSpace(input) == "" {
		if len(subagents) > 0 {
			return preparedInvocationTurn{}, fmt.Errorf("subagent invocation requires a task")
		}
	}
	return preparedInvocationTurn{composed: composed, subagents: subagents}, nil
}

func (c *Controller) runPreparedInvocationTurn(
	ctx context.Context,
	prepared preparedInvocationTurn,
	input, raw, display string,
	frozenImages []string,
) error {
	if len(prepared.subagents) == 0 {
		return c.runGoalLoopWithFrozenImagesRawDisplay(ctx, prepared.composed, raw, display, frozenImages)
	}
	runner := c.skillRunner
	if runner == nil {
		return fmt.Errorf("subagent skill runner is unavailable")
	}
	return newTurnOrchestrator(c).runSubagentSkillTurnsGoalLoop(
		ctx,
		prepared.subagents,
		prepared.composed,
		input,
		display,
		runner,
		c.PlanMode(),
	)
}

// SubmitEditedDisplay is SubmitDisplay for an inline-edited prompt. The model
// sees input; the saved user message also keeps the pre-edit prompt as local UI
// metadata so the edit survives session rewrites.
func (c *Controller) SubmitEditedDisplay(display, input, original string) {
	c.submit(input, display, original)
}

// SubmitUserTurn starts a normal model turn without interpreting shell or slash
// commands. It still resolves references, so callers can submit trusted
// user-authored prompt text without expanding the command surface.
func (c *Controller) SubmitUserTurn(input, display string) {
	c.runRefTurn(input, display)
}

func (c *Controller) submit(input, display, editedOriginal string) {
	trimmed := strings.TrimSpace(input)
	if c.applyGoalCommand(trimmed, display) {
		return
	}
	if strings.HasPrefix(trimmed, "!") {
		c.RunShell(trimmed[1:])
		return
	}
	c.submitCommandOrTurn(trimmed, input, display, false, editedOriginal, "")
}

func (c *Controller) submitHTTP(input, display string) {
	c.submitHTTPWithFormat(input, display, "")
}

func (c *Controller) submitHTTPWithFormat(input, display, format string) {
	trimmed := strings.TrimSpace(input)
	if c.applyGoalCommand(trimmed, display) {
		return
	}
	if strings.HasPrefix(trimmed, "!") {
		c.notice("shell commands are unavailable from this frontend")
		return
	}
	c.submitCommandOrTurn(trimmed, input, display, true, "", format)
}

func (c *Controller) submitCommandOrTurnReady(trimmed, input, display string, scopedRefsOnly bool, editedOriginal, format string) {
	runRefTurn := func(input, display string) {
		c.runRefTurnWithFormat(input, display, format)
	}
	runRefTurnWithRefs := func(input, refLine, display string) {
		c.runRefTurnWithRefsFormat(input, refLine, display, format)
	}
	runGoalLoop := func(ctx context.Context, input, raw, display string) error {
		return c.runGoalLoopWithRawDisplay(c.withTurnFormat(ctx, format), input, raw, display)
	}
	if scopedRefsOnly {
		runRefTurn = func(input, display string) {
			c.runScopedRefTurnWithFormat(input, display, format)
		}
		runRefTurnWithRefs = func(input, refLine, display string) {
			c.runScopedRefTurnWithRefsFormat(input, refLine, display, format)
		}
	}
	if strings.TrimSpace(editedOriginal) != "" {
		runRefTurn = func(input, display string) {
			c.runEditedRefTurnWithFormat(input, display, editedOriginal, format)
		}
		runRefTurnWithRefs = func(input, refLine, display string) {
			c.runEditedRefTurnWithRefsFormat(input, refLine, display, editedOriginal, format)
		}
		runGoalLoop = func(ctx context.Context, input, raw, display string) error {
			return c.runEditedGoalLoopWithRawDisplay(ctx, input, raw, display, editedOriginal)
		}
	}
	switch {
	case trimmed == "/compact" || strings.HasPrefix(trimmed, "/compact "):
		focus := strings.TrimSpace(strings.TrimPrefix(trimmed, "/compact"))
		go func() {
			if err := c.Compact(context.Background(), focus); err != nil {
				c.notice("compaction failed: " + err.Error())
			} else {
				c.notice("compacted")
				if err := c.SnapshotRewrite(); err != nil {
					slog.Warn("controller: snapshot after compact", "err", err)
				}
			}
		}()
	case trimmed == "/context":
		c.noticeDetail(c.ContextReport())
	case trimmed == "/new":
		c.runSessionVerb(c.NewSession, "new session", "new session failed: ")
	case trimmed == "/clear":
		c.runSessionVerb(c.ClearSession, "context cleared", "clear context failed: ")
	case strings.HasPrefix(trimmed, "/mcp__"):
		c.runGuarded(func(ctx context.Context) error {
			sent, found, err := c.MCPPrompt(ctx, trimmed)
			if err != nil {
				return err
			}
			if !found {
				c.notice("unknown command: " + trimmed)
				return nil
			}
			return runGoalLoop(ctx, sent, sent, display)
		})
	case SlashCodeCommentLine(trimmed):

		runRefTurn(input, display)
	case strings.HasPrefix(trimmed, "/"):
		if ref, ok := FileRefLine(trimmed); ok {
			runRefTurn(ref, display)
			return
		}
		if ref, ok := SlashPathLineRef(trimmed, c.workspaceRoot); ok {
			runRefTurnWithRefs(input, ref, display)
			return
		}
		if SlashPathLikeLine(trimmed) {
			runRefTurn(input, display)
			return
		}

		fields := strings.Fields(trimmed)
		switch fields[0] {
		case "/tree":
			c.notice(c.BranchTreeText())
			return
		case "/branch":
			args := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
			if turn, name, fromTurn, err := ParseBranchTarget(args); err != nil {
				c.notice(err.Error())
			} else if fromTurn {
				if _, err := c.ForkNamed(turn-1, name); err != nil {
					c.notice(err.Error())
				}
			} else {
				if _, err := c.Branch(name); err != nil {
					c.notice(err.Error())
				}
			}
			return
		case "/switch":
			ref := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
			if _, err := c.SwitchBranch(ref); err != nil {
				c.notice(err.Error())
			}
			return
		case "/rewind":
			args := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
			turn, scope, err := parseRewind(args, c.Checkpoints())
			if err != nil {
				c.notice("usage: /rewind [turn] [code|conversation|both]")
				return
			}
			if err := c.Rewind(turn, scope); err != nil {
				c.notice(err.Error())
			}
			return
		case "/plan-exec":
			c.applyPlanExec(trimmed, display)
			return
		case "/prometheus":
			c.applyPrometheus(trimmed, display)
			return
		}
		if c.managementNotice(trimmed) {
			return
		}
		if IsBuiltinDocsSlash(fields[0], c.Commands(), c.SlashSkills()) {
			query := strings.TrimSpace(strings.TrimPrefix(trimmed, fields[0]))
			if query == "" {
				text, err := DocsCommandOverviewFor(fields[0])
				if err != nil {
					c.notice("docs: " + err.Error())
				} else {
					c.notice(text)
				}
				return
			}
			c.runGuarded(func(ctx context.Context) error {
				sent, err := docsCommandPrompt(ctx, query)
				if err != nil {
					return fmt.Errorf("docs: %w", err)
				}
				return runGoalLoop(ctx, sent, sent, display)
			})
			return
		}

		if sent, ok := c.CustomCommand(trimmed); ok {
			c.runGuarded(func(ctx context.Context) error {
				return runGoalLoop(ctx, sent, sent, display)
			})
			return
		}
		if sk, task, ok := c.resolveSkillInvocation(trimmed); ok {
			if sk.RunAs == skill.RunSubagent {
				if strings.TrimSpace(task) == "" {
					c.notice("usage: /" + sk.Name + " <task>")
					return
				}
				c.runSubagentSkillSlash(sk, task, trimmed, display)
				return
			}
			sent := c.skills.render(sk, task)
			c.runGuarded(func(ctx context.Context) error {
				return runGoalLoop(ctx, sent, sent, display)
			})
			return
		}

		c.notice("unknown command: " + trimmed + " — sent as a regular message")
		runRefTurn(input, display)
	default:
		runRefTurn(input, display)
	}
}
