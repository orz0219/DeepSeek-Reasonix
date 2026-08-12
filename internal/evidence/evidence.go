package evidence

import (
	"encoding/json"
	"strings"
	"sync"
)

// TodoItem mirrors the todo_write item shape the host needs for step matching.
// StepID is the item's stable identity: it survives a retitle and a reorder, so
// completion attribution never has to be inferred from wording or position. It
// is optional — a list written freehand has none, and matches by text instead.

// ValidateSerialTodos enforces the task-list state machine promised by
// todo_write: at most one item in the whole list is in_progress, completed
// work forms a serial prefix, and pending work follows the current item. The
// rule is segment-aware for two-level lists: a level-0 phase owns the level-1
// sub-steps after it, sub-steps complete in order while their phase stays
// pending, and the phase becomes the single in_progress item only after every
// sub-step has completed — the phase signs off last. A fully completed or
// empty list is also valid.

// stale: partially completed with no current item

// todoSegment is one serial unit of a task list: a level-0 phase header plus
// its level-1 sub-steps, or a single plain step. end is exclusive.

// serialTodoSegments splits a task list into serial units. A level-0 item
// directly followed by level-1 items owns them as one phase segment; every
// other item — including a level-1 item with no preceding phase — is its own
// single-step segment.

// validateSerialSegment checks one segment's internal shape and returns its
// serial state: "completed" (every item completed), "in_progress" (the
// segment holds the current item), "pending" (untouched), or "stale"
// (partially completed with no current item). Item statuses and the global
// single-in_progress rule are already validated by the caller.

// pending

// pending head: its sub-steps carry the segment's progress

// NormalizeSerialTodos repairs legacy host state that predates
// ValidateSerialTodos. It preserves the leading run of fully completed
// segments and makes the first unfinished segment current: its completed
// sub-step prefix is kept and its first unfinished sub-step becomes the
// single in_progress item — or the phase itself when every sub-step is
// already completed. Every later segment returns to pending.

// FirstUnfinishedSubStep reports whether todos[index] is a level-0 phase with
// level-1 sub-steps, and if so the 0-based index of its first sub-step that is
// not yet completed. ok is false when index is not a phase header; a phase
// whose sub-steps are all completed returns (-1, true).

// AdvanceSerialTodo completes the in_progress item at index (0-based) as a
// signed-off step and promotes the next serial item so exactly one item stays
// current. A phase with unfinished sub-steps does not complete. Completing a
// sub-step promotes its next pending sibling, or returns its phase to
// in_progress for sign-off once every sibling is completed. Completing a
// phase or plain step promotes the next pending unit — a phase's first
// pending sub-step (the phase itself stays pending until its sub-steps
// finish), or the plain step itself. A level-1 item with no phase above it
// advances as a standalone step. It reports whether the item was completed.

// No phase above: an orphan sub-step falls through and promotes the
// next pending unit like a plain step, so the list keeps one current
// item.

// TodoStepMatch is the result of matching a complete_step citation against the
// latest successful todo_write list in this turn.

// BackgroundLease identifies a background job whose evidence was provisionally
// merged into the current turn's ledger. The host commits these leases only
// after the turn passes its delivery gates, so a failed turn leaves the job's
// evidence collectable again.

// DeliveryCheckpoint is the compact, persistence-safe state carried across
// runs of one host-owned Goal. It intentionally stores no raw tool arguments or
// output. PendingMutation means a previously observed change still needs fresh
// verification, review, and sign-off before the Goal can finalize.
type DeliveryCheckpoint struct {
	ScopeID             string `json:"scopeID,omitempty"`
	CriteriaEstablished bool   `json:"criteriaEstablished,omitempty"`
	WorkObserved        bool   `json:"workObserved,omitempty"`
	MutationObserved    bool   `json:"mutationObserved,omitempty"`
	PendingMutation     bool   `json:"pendingMutation,omitempty"`
}

// Ledger stores the receipts available to complete_step for the current turn.
type Ledger struct {
	mu               sync.Mutex
	receipts         []Receipt
	backgroundLeases []BackgroundLease
}

func NewLedger() *Ledger { return &Ledger{} }

// Reset clears receipts and background leases between user turns.
func (l *Ledger) Reset() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.receipts = nil
	l.backgroundLeases = nil
}

// ResetBackgroundLeases starts a new run inside the same delivery scope. The
// durable receipts remain available, while per-run job leases must be collected
// and committed independently.
func (l *Ledger) ResetBackgroundLeases() {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.backgroundLeases = nil
	l.mu.Unlock()
}

// NoteBackgroundLease records that a background job's evidence was merged into
// this turn. It returns false when the job was already noted this turn so the
// caller can skip a duplicate merge — collection is idempotent within a turn,
// while a fresh turn (after Reset) leases again.
func (l *Ledger) NoteBackgroundLease(session, jobID string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, lease := range l.backgroundLeases {
		if lease.Session == session && lease.JobID == jobID {
			return false
		}
	}
	l.backgroundLeases = append(l.backgroundLeases, BackgroundLease{Session: session, JobID: jobID})
	return true
}

// BackgroundLeases returns the background jobs merged into this turn, for the
// host to commit once the turn's delivery gates pass.
func (l *Ledger) BackgroundLeases() []BackgroundLease {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.backgroundLeases) == 0 {
		return nil
	}
	out := make([]BackgroundLease, len(l.backgroundLeases))
	copy(out, l.backgroundLeases)
	return out
}

// Record appends a receipt. Failed receipts are retained for auditability but
// are never accepted by the HasSuccessful* matchers.
func (l *Ledger) Record(r Receipt) {
	if l == nil {
		return
	}
	r.Command = strings.TrimSpace(r.Command)
	r.Step = strings.TrimSpace(r.Step)
	r.Paths = normalizePaths(r.Paths)
	r.Todos = normalizeTodos(r.Todos)
	if r.Args != nil {
		cp := make(json.RawMessage, len(r.Args))
		copy(cp, r.Args)
		r.Args = cp
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if r.ToolName == "complete_step" && r.Step != "" && r.TodoStep == nil {
		if match := latestTodoStep(r.Step, l.receipts); match.Found {
			r.TodoStep = &match
		}
	}
	l.receipts = append(l.receipts, r)
}

// Len returns the number of receipts recorded this turn, giving callers a
// stable index to pass to the *Since matchers.
func (l *Ledger) Len() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.receipts)
}

// ReceiptProgressSummary counts successful host-observable receipts by category
// for cross-turn progress signatures. Failed receipts and reads never count:
// repeated reads, failed bookkeeping, and reworded answers must not masquerade
// as progress. Categories are not mutually exclusive (a successful bash command
// that also writes counts in both), which is fine for a change detector.
type ReceiptProgressSummary struct {
	Writes   int // successful mutations/writes
	Commands int // successful commands (bash receipts)
	Todos    int // successful todo_write receipts
	Signoffs int // successful complete_step signoffs
	Reviews  int // successful review receipts
}

// ReceiptProgressSummary returns the current ledger's progress counts.
func (l *Ledger) ReceiptProgressSummary() ReceiptProgressSummary {
	if l == nil {
		return ReceiptProgressSummary{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out ReceiptProgressSummary
	for _, r := range l.receipts {
		if !r.Success {
			continue
		}
		if r.Mutation || r.Write {
			out.Writes++
		}
		if r.Command != "" {
			out.Commands++
		}
		if r.ToolName == "todo_write" {
			out.Todos++
		}
		if r.ToolName == "complete_step" && r.StepProof {
			out.Signoffs++
		}
		if successfulForegroundReviewReceipt(r) || completedStructuredReviewReceipt(r, nil) {
			out.Reviews++
		}
	}
	return out
}

// HasWriteOrCommandSince reports whether a successful write or command receipt
// was recorded at or after index — host-observable progress, as opposed to
// bookkeeping receipts (todo_write, complete_step, ask), which carry neither a
// write flag nor a command.

// HasCompletedReview reports whether a review completed with evidence that is
// fresh for the latest mutation. Structured review_report receipts are the
// strongest proof and also cover collected background reviews. Foreground
// review/task adapters remain compatible, but after a mutation their child
// receipts must show that the changed result was actually inspected.

// HasFailedCommand reports whether the cited command ran this turn but exited
// non-zero — so callers can distinguish "ran and failed" from "never ran".

// SuccessfulCommands returns up to limit successful bash commands from this
// turn, most recent first, for self-correction hints in rejection errors.

// TouchedPaths returns up to limit distinct paths from this turn's successful
// receipts, most recent first; writtenOnly restricts it to writer receipts.

// HasSuccessfulBashMentioningPaths reports whether every path appears in some
// successful bash command this turn — files created or edited through shell
// redirection (`seq … > file`) leave no reader/writer receipt, so the command
// text naming the path is the receipt.

// HasSuccessfulDeliverySignoffAfter reports whether a successful complete_step
// after the latest mutation cites a verification command that also succeeded
// after that mutation. complete_step already validates the cited command against
// host receipts; the additional ordering check prevents a pre-change test from
// signing off changed code in the delivery profile.

// HasSuccessfulReviewAfter reports whether the changed result was inspected
// after the latest mutation. A read of a touched path is sufficient; git/diff
// inspection commands cover shell-driven or delegated mutations whose paths are
// not knowable to the host. A negative index is the restored-checkpoint
// baseline: the mutation predates this ledger (controller rebuild or cold
// resume), so any successful review-shaped receipt counts.

// HasHostReviewCoverageAfter reports whether host-observed content inspection
// after the latest mutation covers the production paths required by a Medium
// Delivery review. A plain, output-producing `git diff` covers the current
// change set; otherwise every required path needs a read receipt or a
// content-printing command that names it. Summary/status/check-only commands
// and model prose never satisfy this stronger alternative to review_report.

// A negative mutationIndex is the restored-checkpoint baseline: the
// mutation's receipt is not in this ledger, so its touched paths are
// unknowable and any successful review-shaped receipt counts.

// HasSuccessfulAcceptanceCriteria reports whether the current turn established
// a non-empty task list. Delivery mode uses that list as its host-observable
// acceptance contract before permitting state-changing work.

// HasSuccessfulTodoProgressReceipt reports whether any successful receipt in
// the turn reflects execution progress rather than read-only context gathering
// or a bare todo snapshot.

// IncompleteTodos returns the items of a todo list that are not completed.

// MatchStep resolves a complete_step.step (number, title, or drift-tolerant
// variant) against a todo list, returning the matched item.

// MatchTodoIdentity resolves an existing todo against an updated list without
// interpreting numeric content as a 1-based step citation.

// PreservesCompletedTodoPositions reports whether every previously completed
// item remains completed at the same index in the replacement list. Completed
// sub-steps can sit behind a pending phase header, so this checks every item
// rather than assuming the literal list begins with completed statuses.

// HasAnySuccessfulReceipt reports whether any tool succeeded this turn — the
// signal that the turn did real work, not pure conversation.

// HasSuccessfulToolReceipt reports whether a named tool completed
// successfully in the current evidence scope.

// HasSuccessfulMutationOtherThan distinguishes a workflow-specific state
// change (for example durable memory) from unrelated workspace mutations that
// still need the full Delivery verification/review contract.

// HasSuccessfulWorkReceipt excludes workflow bookkeeping and reports whether
// the assistant actually inspected, executed, or changed something this turn.
// Delivery mode uses it to reject text-only claims for technical tasks while
// still allowing ordinary conversation to finish without tools.

// HasSuccessfulVerificationCommand reports whether the turn ran at least one
// command classified as verification rather than inspection or mutation.

// HasSuccessfulVerificationCommandAfter reports whether verification succeeded
// after the named receipt index. Mutations before the boundary do not satisfy a
// role setting's post-change verification floor.

// HasSuccessfulAnchorRefreshReadAfter reports whether read_file refreshed a
// wanted path after the given receipt index. Windowed reads and grep/ls receipts
// are deliberately not enough for same-turn anchor edits: they may have observed
// a different region than the next old_string/delete_range anchor.

// LatestSuccessfulMutationIndex returns the most recent host-observed
// state-changing call. It includes known file writers, writer-capable delegated
// or external tools, and bash commands that are not demonstrably observational
// or verification-only.

// LatestTodos returns the todo list from this turn's latest successful todo_write.

// UnverifiedCompletedTodos reports current completed todos that transitioned
// from the latest prior successful todo_write receipt without a matching
// successful complete_step receipt earlier in the same turn. If this turn has no
// prior todo_write baseline, hasBaseline is false and callers should preserve
// the existing loose validation behavior.

// Recovery only trusts progress that happened before the failed sign-off.
// Later unrelated work must not retroactively authorize an earlier completion.

// WithDeliveryProfile marks tool execution as subject to the delivery-first
// final-readiness contract. Tools use this only for stricter evidence validation;
// it is ephemeral host state and is never serialized into sessions or prompts.

// DeliveryProfileFromContext reports whether the current tool call must produce
// evidence that the delivery final-readiness gate can accept.

// WithSessionMessages attaches a lazy transcript accessor so verifyStepEvidence
// can fall back to scanning the conversation when the per-turn ledger misses a
// command (cross-turn references, non-bash tool calls, truncated command
// strings). The context carries the capability, not the data: snapshot is
// called only when a consumer (complete_step) actually needs the history, so
// ordinary tool calls never pay for a full transcript copy.

// SessionMessagesFromContext resolves the transcript accessor attached by
// WithSessionMessages, taking the snapshot at call time.

// WithTodoState attaches the host's canonical task list to a tool call. The
// per-turn ledger resets between user messages, while unfinished tasks remain
// active across those turns.

// TodoStateFromContext returns a copy of the host's canonical task list.

// PathsProvenInSession reports whether every path is covered by a successful
// (non-errored) tool call somewhere in msgs — the cross-turn fallback for diff
// and files evidence, mirroring verifyCommandFromSession for the per-turn
// ledger's path receipts (which reset each turn). wantWrite restricts to writer
// tools (diff); false accepts a reader or writer (files).

// ToolCallPaths returns the bounded, structurally declared file paths in a
// tool call. It intentionally does not attempt to parse shell scripts; callers
// must treat bash and unknown targets as allPaths when invalidation is needed.

// ToolCallMutates is the delivery profile's conservative state-change
// classifier. Trusted read-only tools never mutate. Meta tools that only
// delegate (task, run_skill, review, …) never mutate by themselves — real
// writes arrive via child evidence merge. Writer-capable tools do mutate,
// except for bash commands that the host can prove are inspection or
// verification commands.

// ToolCallRequiresDeliveryCriteria reports whether a call begins execution
// work that needs an acceptance contract. Mutations always qualify; verification
// commands also qualify even though they are intentionally not mutations.

// BashToolCallMixesMutationAndVerification reports whether a bash call combines
// a host-recognized verifier with another segment the host cannot prove is
// read-only. Delivery mode blocks this shape before execution. Besides avoiding
// accidental workspace changes during a check, this keeps scratch-file setup
// (for example, writing /tmp/check.js before node --check) from becoming the
// latest opaque mutation and invalidating otherwise valid delivery evidence.

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

// BashToolCallMasksVerificationExit reports the common `check; echo $?` shape.
// The trailing reporter makes the shell call itself succeed even when the
// verifier failed, so a successful tool receipt cannot prove the check passed.
// It is separated from the broader mixed-command classifier so the agent can
// give a precise recovery instruction instead of inviting repeated rewrites.

// BashToolCallUsesOpaqueInlineInterpreter reports whether a bash call executes
// source supplied directly on an interpreter's command line. Delivery mode
// cannot prove whether snippets such as node -e or python -c only inspect state
// or also write files. Letting them run and then treating them as opaque
// mutations invalidates otherwise valid review/verification receipts, while
// treating them as read-only would create a delivery bypass. The agent blocks
// this shape before execution and directs callers to auditable file tools,
// script files, or conventional verifier commands instead.

// BashToolCallUsesNonTerminalInlineInterpreter reports whether an opaque
// inline interpreter (python -c, node -e, …) is not the last top-level segment
// *and* a later segment can overwrite its exit status. Ordinary mode blocks that
// shape deterministically without rewriting the command. An `&&` chain is left
// alone: bash short-circuits it, so the interpreter's failure is still the
// call's exit status and nothing is hidden.

// Unknown / unparseable syntax: do not pretend full analysis.

// BashCommandMayBeOpaqueMutation reports whether a sole opaque inline
// interpreter call is allowed to run but cannot be proven read-only for
// mutation-risk labeling.

// ShellContractPreflightMessage is the model-facing recovery text when a
// deterministic shell contract blocks a call before launch.

// IsDeliveryVerificationCommand reports whether command is a host-recognized
// verification command for delivery finalization. Keep complete_step and the
// final-readiness gate on this single classifier so a sign-off cannot claim a
// command that the final gate will immediately reject.

// verificationCommandRecommendations is the single source for the concrete
// model-readable examples and the family labels used to diagnose test failures.
// It is intentionally a safe recommended subset rather than an exhaustive
// rendering of bashSegmentIsVerification: accepted commands that may install
// dependencies or create workspace outputs should not be suggested as the
// first recovery action.

// VerificationCommandSummary returns compact, model-readable recovery
// guidance. It lists only recommended command families that the classifier
// accepts, while omitting known self-installing and direct workspace-output
// command forms from first-line guidance.

// A package pattern can expand to one main package, so even `go build
// ./...` may write a workspace binary. Package expansion and inherited
// GOFLAGS are unavailable to this static classifier; fail closed for all
// build forms and keep test/vet as the recognized Go verifiers.

// swift test runs the SwiftPM test suite; build artifacts stay under
// the package's own .build directory (including --enable-code-coverage
// reports). Other swift subcommands (build/run/package) can write
// binaries or mutate the package, so only the test form is a
// recognized verifier. Explicit report destinations, attachment dirs,
// and scratch-dir redirects are rejected by writeOutputFlags. Note
// that swift test may run Package.swift build plugins (arbitrary
// code) — the same trust boundary as go test / cargo test.

// Control modes that do not run the test suite (help, listing) must
// not count as verification; mirror the tsc treatment of --help.

// tscSegmentIsVerification accepts only one-shot, explicit no-emit type checks.
// Bare tsc commands may emit JavaScript, declarations, and source maps; control
// modes may write config, skip checking, exit after printing metadata, or watch
// indefinitely. Any explicit false value wins conservatively even if another
// no-emit flag appears in the same command.

// tscFlagDisqualifiesVerification rejects modes that do not perform a bounded
// type check and destinations that write independently of JavaScript/declaration
// emit. Default incremental metadata remains conventional verifier cache;
// explicit output destinations and control modes fail closed as mutations.

// npxSegmentIsVerification unwraps only known test runners invoked directly,
// with no npx control flags. Treating arbitrary npx packages as verification
// would let package installation or an opaque executable masquerade as a
// read-only check. Runner flags that update snapshots, write reports, or enable
// coverage are rejected by the caller and the checks below.

// Known test/lint runners are verification unless an argument asks them
// to update snapshots, collect coverage, or write a report.

// Prettier without an explicit check mode formats to stdout and is not a
// project verification receipt. Keep only its read-only check forms.

// Playwright/Cypress produce project reports, screenshots, or videos by
// default; tsx/ts-node execute source. They remain mutations.

// npxRunnerName accepts only a bare package name with an optional ordinary
// version or dist-tag suffix. Paths and package protocols such as
// eslint@npm:other-package must not inherit a known runner's trust boundary.

// Node CLI flags are case-sensitive: -c/--check is the syntax-only mode,
// while -C/--conditions executes the target with custom export conditions.

// Syntax-check mode does not execute the target. Fail closed on any
// additional option: preload/eval/import flags could execute code before
// the check and turn a purported verifier into an opaque mutation.

// Match the repository's treatment of other conventional test runners,
// but fail closed on test-runner and Node runtime flags that write files.

// writeOutputFlags are test-runner and linter flags that write snapshot,
// report, or profile files. Snapshot flags rewrite checked-in fixtures (the
// --update/-u class rejected above); the others write explicit output paths.
// A runner invoked with one of them changes workspace state, so the segment
// must not count as read-only verification.

// pytest-snapshot / syrupy
// jest --updateSnapshot via npm/yarn wrappers
// pytest
// pytest / mypy
// gotestsum
// gotestsum
// go test
// go test
// go test
// go test
// go test
// go test binary
// go test binary
// jest/vitest --outputFile (with --json)
// pytest-reportlog
// swift test --xunit-output writes a JUnit XML report
// swift test --scratch-path redirects the build dir
// swift test --build-path: legacy alias of --scratch-path
// swift test --cache-path redirects the shared cache dir
// swift test (Swift 6.x): swift-testing JSON output
// swift test (Swift 6.x): experimental event-stream output
// swift test (Swift 6.x): Swift Testing attachments dir
// swift test (Swift 6.x): experimental attachments dir

// not a flag

// go test flags accept an optional test. prefix (-test.coverprofile)
// that the go tool passes through to the test binary.

// Vitest exposes dotted per-reporter forms (--outputFile.json=path).

// mypyFlagWritesReport reports whether a mypy flag writes a report directory:
// every mypy report option follows the --<type>-report DIR shape (txt, html,
// xml, cobertura-xml, any-exprs, linecount, linecoverage, lineprecision), and
// mypy has no read-only flag with that suffix. --junit-xml is covered by the
// global write-output flags.

// goTestFlagWritesFile reports whether a go test flag writes a workspace
// artifact: -c/-o emit the test binary, -trace and the profile flags write
// profiles, and -artifacts/-testlogfile/-gocoverdir write test outputs. The
// short and ambiguous names stay out of writeOutputFlags because the
// dash-stripped global match would also hit node -c (a syntax-only check)
// and pytest --trace (a read-only debugger flag). go test flags accept
// single- and double-dash forms and an optional test. prefix that the go
// tool passes through to the test binary.

// not a flag

// commandShowsContentForPath reports whether a bash command demonstrably
// printed the content of the (normalized, slash-lowered) claimed path: a
// content-printing program — cat/head/tail/diff/cmp or git diff/git show —
// whose statically parsed argv names the path exactly or by trailing path
// components. The receipt must contain exactly one simple statement; compound
// statements and pipelines are rejected because unrelated output can satisfy
// the aggregate OutputBytes receipt. Redirected, negated, background, or
// dynamically expanded commands and summary/quiet flags that suppress the
// patch body (--stat, --name-only, -q, …) are rejected too. Matching is
// per-argument and exact, so reading path.bak never satisfies path.

// Any redirect can divert the content away from the transcript.

// Pipelines transform or swallow content; AND/OR lists can contribute
// unrelated bytes to the aggregate receipt. Neither proves file output.

// contentSuppressingFlags turn a content command into a summary that never
// shows the patch body; their presence disqualifies the receipt as evidence.

// argNamesGitRevisionPath accepts only git show's REV:path form. The ordinary
// `git show REV -- path` form can print commit metadata with no file body while
// still producing a non-empty aggregate receipt.

// argNamesPath reports whether one static argv token names the claimed path:
// exact after normalization, a trailing-components match of a fuller token,
// or the path part of a git REV:path spec.

// completeStepIdentity is the citation a receipt records, most stable first: a
// step id survives a replan, an index survives a retitle, the title survives
// neither.

// A failed complete_step can unlock todo recovery only when the payload had the
// same structural proof shape Execute expects before host verification runs.

// Summary is enough for manual evidence.

// matchTodoStep resolves a citation to a todo. A stable id wins outright; only
// a list without ids falls back to position and wording, which a retitle or an
// inserted step silently invalidates.

// Containment fallback for wording drift; an ambiguous citation (containing
// or contained by two different todos) stays unmatched rather than guessing.

// normalizeStepText folds the drift models introduce when citing a todo:
// fullwidth ASCII forms → halfwidth (："５ → :"5), all whitespace dropped,
// case-insensitive.

// stepTextContains: substring match between normalized texts, but only when the
// shorter side is substantial enough (≥6 runes) to not match by accident.
