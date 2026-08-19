package evidence

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"

	"reasonix/internal/provider"
	"reasonix/internal/shellparse"
	"reasonix/internal/shellsafe"
)

type contextKey struct{}
type sessionMessagesKey struct{}
type deliveryProfileKey struct{}
type todoStateKey struct{}

func WithLedger(ctx context.Context, ledger *Ledger) context.Context {
	if ledger == nil {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, ledger)
}

func FromContext(ctx context.Context) (*Ledger, bool) {
	ledger, ok := ctx.Value(contextKey{}).(*Ledger)
	return ledger, ok && ledger != nil
}

// WithDeliveryProfile marks tool execution as subject to the delivery-first
// final-readiness contract. Tools use this only for stricter evidence validation;
// it is ephemeral host state and is never serialized into sessions or prompts.
func WithDeliveryProfile(ctx context.Context) context.Context {
	return context.WithValue(ctx, deliveryProfileKey{}, true)
}

// DeliveryProfileFromContext reports whether the current tool call must produce
// evidence that the delivery final-readiness gate can accept.
func DeliveryProfileFromContext(ctx context.Context) bool {
	enabled, _ := ctx.Value(deliveryProfileKey{}).(bool)
	return enabled
}

// WithSessionMessages attaches a lazy transcript accessor so verifyStepEvidence
// can fall back to scanning the conversation when the per-turn ledger misses a
// command (cross-turn references, non-bash tool calls, truncated command
// strings). The context carries the capability, not the data: snapshot is
// called only when a consumer (complete_step) actually needs the history, so
// ordinary tool calls never pay for a full transcript copy.
func WithSessionMessages(ctx context.Context, snapshot func() []provider.Message) context.Context {
	return context.WithValue(ctx, sessionMessagesKey{}, snapshot)
}

// SessionMessagesFromContext resolves the transcript accessor attached by
// WithSessionMessages, taking the snapshot at call time.
func SessionMessagesFromContext(ctx context.Context) ([]provider.Message, bool) {
	snapshot, ok := ctx.Value(sessionMessagesKey{}).(func() []provider.Message)
	if !ok || snapshot == nil {
		return nil, false
	}
	return snapshot(), true
}

// WithTodoState attaches the host's canonical task list to a tool call. The
// per-turn ledger resets between user messages, while unfinished tasks remain
// active across those turns.
func WithTodoState(ctx context.Context, todos []TodoItem) context.Context {
	return context.WithValue(ctx, todoStateKey{}, append([]TodoItem(nil), todos...))
}

// TodoStateFromContext returns a copy of the host's canonical task list.
func TodoStateFromContext(ctx context.Context) ([]TodoItem, bool) {
	todos, ok := ctx.Value(todoStateKey{}).([]TodoItem)
	return append([]TodoItem(nil), todos...), ok
}

// PathsProvenInSession reports whether every path is covered by a successful
// (non-errored) tool call somewhere in msgs — the cross-turn fallback for diff
// and files evidence, mirroring verifyCommandFromSession for the per-turn
// ledger's path receipts (which reset each turn). wantWrite restricts to writer
// tools (diff); false accepts a reader or writer (files).
func PathsProvenInSession(msgs []provider.Message, paths []string, wantWrite bool) bool {
	wanted := pathSet(normalizePaths(paths))
	if len(wanted) == 0 {
		return false
	}
	failed := failedSessionCallIDs(msgs)
	found := map[string]bool{}
	for _, msg := range msgs {
		for _, tc := range msg.ToolCalls {
			if failed[tc.ID] {
				continue
			}
			r := ReceiptFromToolCall(tc.Name, json.RawMessage(tc.Arguments), true, false)
			if wantWrite && !r.Write {
				continue
			}
			if !wantWrite && !r.Read && !r.Write {
				continue
			}
			for _, p := range normalizePaths(r.Paths) {
				if _, ok := wanted[p]; ok {
					found[p] = true
				}
			}
		}
	}
	return len(found) == len(wanted)
}

func failedSessionCallIDs(msgs []provider.Message) map[string]bool {
	failed := map[string]bool{}
	for _, msg := range msgs {
		if msg.Role != provider.RoleTool || msg.ToolCallID == "" {
			continue
		}
		if strings.HasPrefix(msg.Content, "error:") || strings.HasPrefix(msg.Content, "blocked:") {
			failed[msg.ToolCallID] = true
		}
	}
	return failed
}

func ReceiptFromToolCall(toolName string, args json.RawMessage, success bool, readOnly bool) Receipt {
	r := Receipt{
		ToolName: toolName,
		Args:     args,
		Success:  success,
		Mutation: ToolCallMutates(toolName, args, readOnly),
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err == nil {
		if toolName == "bash" {
			r.Command = stringField(fields, "command")
		}
		if toolName == "task" {
			r.Profile = stringField(fields, "profile")
		}
		if toolName == "complete_step" {
			r.Step = completeStepIdentity(fields)
			r.StepProof = completeStepHasProof(fields)
		}
		if toolName == "todo_write" {
			r.Todos = todoItemsField(fields, "todos")
		}
		r.Paths = extractPaths(fields)
	}

	if isWriterTool(toolName) {
		r.Write = true
	} else if isReadReceipt(toolName, readOnly) {
		r.Read = true
	}
	return r
}

// ToolCallPaths returns the bounded, structurally declared file paths in a
// tool call. It intentionally does not attempt to parse shell scripts; callers
// must treat bash and unknown targets as allPaths when invalidation is needed.
func ToolCallPaths(args json.RawMessage) []string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return nil
	}
	paths := extractPaths(fields)
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

// ToolCallMutates is the delivery profile's conservative state-change
// classifier. Trusted read-only tools never mutate. Meta tools that only
// delegate (task, run_skill, review, …) never mutate by themselves — real
// writes arrive via child evidence merge. Writer-capable tools do mutate,
// except for bash commands that the host can prove are inspection or
// verification commands.
func ToolCallMutates(toolName string, args json.RawMessage, readOnly bool) bool {
	if readOnly {
		return false
	}
	if IsNonMutationMetaTool(toolName) {
		return false
	}
	switch toolName {
	case "ask", "todo_write", "complete_step", "bash_output", "wait":
		return false
	case "bash":
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(args, &fields); err != nil {
			return true
		}
		return bashMayMutate(stringField(fields, "command"))
	default:
		return true
	}
}

// ToolCallRequiresDeliveryCriteria reports whether a call begins execution
// work that needs an acceptance contract. Mutations always qualify; verification
// commands also qualify even though they are intentionally not mutations.
func ToolCallRequiresDeliveryCriteria(toolName string, args json.RawMessage, readOnly bool) bool {
	if ToolCallMutates(toolName, args, readOnly) {
		return true
	}
	if toolName != "bash" {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return true
	}
	return bashCommandIsVerification(stringField(fields, "command"))
}

// BashToolCallMixesMutationAndVerification reports whether a bash call combines
// a host-recognized verifier with another segment the host cannot prove is
// read-only. Delivery mode blocks this shape before execution. Besides avoiding
// accidental workspace changes during a check, this keeps scratch-file setup
// (for example, writing /tmp/check.js before node --check) from becoming the
// latest opaque mutation and invalidating otherwise valid delivery evidence.
func BashToolCallMixesMutationAndVerification(args json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return false
	}
	command := stringField(fields, "command")
	return bashContainsVerificationSegment(command) && bashMayMutate(command)
}

// BashToolCallMixesMutationAndMaskableVerification is the ordinary-mode subset of
// BashToolCallMixesMutationAndVerification: the same mixed shape, but only when
// the shell's exit status can actually hide the earlier step's failure.
//
// Delivery mode blocks the broad shape because a mutation invalidates the
// verification *receipt* regardless of exit status. Ordinary mode has no receipt
// to protect — its only concern is a result that looks successful while an
// earlier step failed. `build && test` cannot produce that (bash short-circuits
// and reports the failing status), so blocking it would reject the single most
// common shell shape in real projects for no safety gain. `build; test` can,
// and stays blocked.
func BashToolCallMixesMutationAndMaskableVerification(args json.RawMessage) bool {
	if !BashToolCallMixesMutationAndVerification(args) {
		return false
	}
	command, ok := bashCommandFromArgs(args)
	if !ok {
		return false
	}
	canMask, analyzed := shellparse.CanMaskEarlierFailure(command)
	return analyzed && canMask
}

// BashToolCallMasksVerificationExit reports the common `check; echo $?` shape.
// The trailing reporter makes the shell call itself succeed even when the
// verifier failed, so a successful tool receipt cannot prove the check passed.
// It is separated from the broader mixed-command classifier so the agent can
// give a precise recovery instruction instead of inviting repeated rewrites.
func BashToolCallMasksVerificationExit(args json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return false
	}
	command := strings.TrimSpace(stringField(fields, "command"))
	if command == "" || !bashContainsVerificationSegment(command) {
		return false
	}
	segments, _, ok := shellparse.SplitTopLevel(command)
	if !ok {
		return false
	}
	seenVerifier := false
	for _, segment := range segments {
		normalized, _ := shellsafe.NormalizeBashSafeRedirectsForMatch(segment)
		argv, malformed := shellparse.StaticFields(normalized)
		if malformed == "" && bashSegmentIsVerification(argv) {
			seenVerifier = true
			continue
		}
		if !seenVerifier || !strings.Contains(segment, "$?") {
			continue
		}
		lower := strings.ToLower(strings.TrimSpace(segment))
		if strings.HasPrefix(lower, "echo ") || strings.HasPrefix(lower, "printf ") {
			return true
		}
	}
	return false
}

// BashToolCallUsesOpaqueInlineInterpreter reports whether a bash call executes
// source supplied directly on an interpreter's command line. Delivery mode
// cannot prove whether snippets such as node -e or python -c only inspect state
// or also write files. Letting them run and then treating them as opaque
// mutations invalidates otherwise valid review/verification receipts, while
// treating them as read-only would create a delivery bypass. The agent blocks
// this shape before execution and directs callers to auditable file tools,
// script files, or conventional verifier commands instead.
func BashToolCallUsesOpaqueInlineInterpreter(args json.RawMessage) bool {
	command, ok := bashCommandFromArgs(args)
	if !ok {
		return false
	}
	return bashCommandUsesOpaqueInlineInterpreter(command)
}

// BashToolCallUsesNonTerminalInlineInterpreter reports whether an opaque
// inline interpreter (python -c, node -e, …) is not the last top-level segment
// *and* a later segment can overwrite its exit status. Ordinary mode blocks that
// shape deterministically without rewriting the command. An `&&` chain is left
// alone: bash short-circuits it, so the interpreter's failure is still the
// call's exit status and nothing is hidden.
func BashToolCallUsesNonTerminalInlineInterpreter(args json.RawMessage) bool {
	command, ok := bashCommandFromArgs(args)
	if !ok {
		return false
	}
	segments, _, ok := shellparse.SplitTopLevel(command)
	if !ok || len(segments) < 2 {

		return false
	}
	if canMask, analyzed := shellparse.CanMaskEarlierFailure(command); !analyzed || !canMask {
		return false
	}
	for i, segment := range segments {
		if !bashSegmentUsesOpaqueInlineInterpreter(segment) {
			continue
		}
		if i < len(segments)-1 {
			return true
		}
	}
	return false
}

// BashCommandMayBeOpaqueMutation reports whether a sole opaque inline
// interpreter call is allowed to run but cannot be proven read-only for
// mutation-risk labeling.
func BashCommandMayBeOpaqueMutation(args json.RawMessage) bool {
	return BashToolCallUsesOpaqueInlineInterpreter(args)
}

// Command-based variants that accept a pre-extracted bash command string.
// Callers that already parsed the command (e.g. from a cached parsedArgs)
// avoid a redundant json.Unmarshal per call.

// BashCommandMasksVerificationExit is the command-based variant of
// BashToolCallMasksVerificationExit.
func BashCommandMasksVerificationExit(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" || !bashContainsVerificationSegment(command) {
		return false
	}
	segments, _, ok := shellparse.SplitTopLevel(command)
	if !ok {
		return false
	}
	seenVerifier := false
	for _, segment := range segments {
		normalized, _ := shellsafe.NormalizeBashSafeRedirectsForMatch(segment)
		argv, malformed := shellparse.StaticFields(normalized)
		if malformed == "" && bashSegmentIsVerification(argv) {
			seenVerifier = true
			continue
		}
		if !seenVerifier || !strings.Contains(segment, "$?") {
			continue
		}
		lower := strings.ToLower(strings.TrimSpace(segment))
		if strings.HasPrefix(lower, "echo ") || strings.HasPrefix(lower, "printf ") {
			return true
		}
	}
	return false
}

// BashCommandMixesMutationAndVerification is the command-based variant.
func BashCommandMixesMutationAndVerification(command string) bool {
	return bashContainsVerificationSegment(command) && bashMayMutate(command)
}

// BashCommandMixesMutationAndMaskableVerification is the command-based variant.
func BashCommandMixesMutationAndMaskableVerification(command string) bool {
	if !BashCommandMixesMutationAndVerification(command) {
		return false
	}
	canMask, analyzed := shellparse.CanMaskEarlierFailure(command)
	return analyzed && canMask
}

// BashCommandUsesOpaqueInlineInterpreter is the command-based variant.
func BashCommandUsesOpaqueInlineInterpreter(command string) bool {
	return bashCommandUsesOpaqueInlineInterpreter(command)
}

// BashCommandUsesNonTerminalInlineInterpreter is the command-based variant.
func BashCommandUsesNonTerminalInlineInterpreter(command string) bool {
	segments, _, ok := shellparse.SplitTopLevel(command)
	if !ok || len(segments) < 2 {
		return false
	}
	if canMask, analyzed := shellparse.CanMaskEarlierFailure(command); !analyzed || !canMask {
		return false
	}
	for i, segment := range segments {
		if !bashSegmentUsesOpaqueInlineInterpreter(segment) {
			continue
		}
		if i < len(segments)-1 {
			return true
		}
	}
	return false
}

func bashCommandFromArgs(args json.RawMessage) (string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return "", false
	}
	command := strings.TrimSpace(stringField(fields, "command"))
	return command, command != ""
}

func bashCommandUsesOpaqueInlineInterpreter(command string) bool {
	segments, _, ok := shellparse.SplitTopLevel(command)
	if !ok {
		return false
	}
	return slices.ContainsFunc(segments, bashSegmentUsesOpaqueInlineInterpreter)
}

func bashSegmentUsesOpaqueInlineInterpreter(segment string) bool {
	normalized, _ := shellsafe.NormalizeBashSafeRedirectsForMatch(segment)
	argv, malformed := shellparse.StaticFields(normalized)
	if malformed != "" || len(argv) == 0 {
		return false
	}
	base := strings.ToLower(filepath.Base(argv[0]))
	args := argv[1:]
	switch base {
	case "node", "bun":
		return hasCommandArg(args, "-e", "--eval", "-p", "--print")
	case "python", "python3", "ruby", "perl":
		return hasCommandArg(args, "-c", "-e")
	case "php":
		return hasCommandArg(args, "-r")
	case "deno":
		return len(args) > 0 && strings.EqualFold(args[0], "eval")
	}
	return false
}
