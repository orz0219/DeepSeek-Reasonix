package cli

import (
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"

	"reasonix/internal/control"
	"reasonix/internal/event"
)

// chatTUI is a bubbletea Model that normally owns the terminal with an
// alt-screen transcript viewport. Termux is the exception: it stays in the
// normal buffer and commits finalized output to native scrollback via
// tea.Println so taps can still focus the soft keyboard.

// missing-key warning surfaced once in the banner, "" when ready

// diagnostics is the process-owned TUI log/watchdog started before terminal
// takeover. Nil in unit tests that construct chatTUI without chatREPL.

// themeSweep freezes the frame while a /theme switch wipes across it.

// nativeScrollback keeps Termux out of alt-screen mode so taps still focus
// the textarea and raise the soft keyboard.

// mouseCaptureOff releases mouse ownership back to the terminal (View() sets
// tea.MouseModeNone instead of MouseModeCellMotion) so its native
// click-drag selection and right-click context menu work again. Toggled by
// "/mouse" or REASONIX_DISABLE_MOUSE at startup; trades away in-app
// drag-select, the transcript scrollbar, and wheel-scroll while it's on,
// since the terminal no longer forwards those events to Reasonix.

// composerScrollOffset is an independent view offset used after the user
// wheels inside an overflowing composer. The textarea keeps ownership of the
// real insertion cursor; a subsequent edit or cursor key reattaches the view
// to that cursor without the wheel having moved it.

// retryAttempt/retryMax drive the transient "retrying (n/m)" indicator while
// the provider re-attempts the connection; cleared by the next stream event.

// turnPhase is the host turn phase from turn_phase events
// (working|checking|verifying|reviewing). Cleared on TurnDone.

// turnTokens accumulates this turn's output tokens (summed from per-step Usage
// events) for the live "↓N" readout in the running status line.

// showTurnUsage controls whether completed per-request token/cost receipts are
// retained in transcript scrollback. Usage accounting remains active either way.

// balance is the last-fetched wallet-balance readout (e.g. "¥110.00"), "" when
// the provider declares no balance_url or a fetch failed. Refreshed async on
// startup and after each turn so the status line stays roughly current without
// blocking the event loop.

// todoArgs is the latest todo_write call's raw args; it drives the task list
// pinned just above the input (see renderTodoPanel). "" when there's no list.
// Persists across turns until the work completes or a new session starts.

// marker rides in outgoing user messages so the cache-stable prompt prefix is
// left untouched.

// legacyScrollClear keeps the per-offset ClearScreen workaround only for Warp.

// sessionSwitch suppresses that workaround during a transcript rebuild (#5441).

// yoloRestoreToolApprovalMode remembers the Ask/Auto base mode that Ctrl+Y
// should restore after a desktop-style YOLO toggle.

// inboxSelectedID is the currently highlighted durable inbox item while
// browsing the queue in tuiRunning. Empty means "not browsing". Full bodies
// are never cached here — only the selected ID and the snapshot metadata.

// queueEditCursor tracks which queued message the user is currently
// browsing/editing via ↑/↓ during tuiRunning. -1 means "not browsing".

// queueEditDraft saves the in-progress input text when the user first
// presses ↑ to browse the queue, so it can be restored when the cursor
// moves past the end.

// queueConfirmDelete, when true, the next 'd' confirms deletion of the
// selected inbox item.

// history is a resumed session's messages, committed to scrollback once on
// the first WindowSizeMsg so a reopened chat shows its prior transcript.

// reasoning accumulates the in-progress thinking stream (dim); pending
// accumulates the in-progress answer (raw markdown). They are committed to
// scrollback (reasoning collapsed by default, answer markdown-rendered) when they
// finalize — at a tool/usage boundary or turn end — not previewed live, so
// the bottom region stays a stable height. pendingCommit queues finalized
// lines so a single Update emits exactly one ordered tea.Println.

// Ctrl+O / /verbose: show raw thinking text in the CLI

// reasoningLineIdx is the transcript index of the live "▎ thinking…" marker
// while a reasoning block streams; it's rewritten to "▎ thought for Ns" when
// the block closes. -1 when no block is open. transcriptDirty forces a
// viewport re-feed after that in-place rewrite (length is unchanged).

// reasoningTextIdx is the transcript index of the live reasoning text block
// (the block right after the marker), streamed in as the model thinks and
// removed when the block collapses (kept only in verbose mode). -1 when none.

// reasoningView is a bounded trailing window (≤ reasoningViewMax bytes) of the
// streaming thought, rendered live; the full text stays in reasoning for verbose.

// reasoningNative is the Termux/native-scrollback path: reasoning is buffered
// without a live transcript block, then appended once as a final summary.

// answerIdx is the transcript index of the streaming answer block (rewritten in
// place as completed paragraphs arrive); -1 when none is open. answerFlushed is
// how many bytes of pending have already been rendered into it, so a Text packet
// that doesn't close a new paragraph re-renders nothing.

// toolStreamIdx is the transcript index of a running tool's live-output block
// (streamed via ToolProgress under the tool card); -1 when none. toolStreamID
// is the call ID it belongs to. Only a bounded tail is kept — the last few
// complete lines (toolTail) plus the in-progress one (toolPartial) — so a
// high-output command can't balloon memory or cost O(n²) re-splitting;
// toolLineCount feeds the collapse summary.

// shellOutputs stores the full accumulated output of each shell command
// (tool IDs with "shell-" prefix), so the first 10 lines can be shown after
// collapse and Ctrl+B can toggle the complete output.

// shellTranscriptIdx maps a shell tool ID to the transcript index of its
// collapsed output block, so Ctrl+B can rewrite it in place.

// toolLineCountByID keeps a switched-away tool's last line count so a late
// ToolResult can still render "⎿ N lines" (shellOutputs only tracks "shell-" ids).

// toolStreamStart / toolStreamFrame drive the "⎿ working · Ns" line shown
// under a dispatched tool that hasn't produced output yet, so a slow tool
// reads as making progress rather than frozen.

// Sub-agent progress previews (reserved ToolProgress channels) render per
// child into their own fixed transcript slot, keyed by the namespaced call
// ID — independent of the single live toolStreamID. subagentProgress keeps
// the bounded live state (phase, elapsed, recent activity, verbose tails).

// forceGotoBottom is set by replayActiveBranch and resetFreshContextView to
// pin the viewport to the bottom after a session / branch / clear switch
// regardless of the previous wasAtBottom state (#4584).

// scrollMode is the explicit followTail / userScrolled state machine.
// Prefer this over a raw wasAtBottom snapshot so modal height changes
// (approval, chooser, pickers) never silently disable tail-follow (#6430).

// banner + resumed history committed once

// transcript holds every finalized line commitLine emits; the viewport
// renders a scrollable window of it (alt-screen owns the grid, so there's no
// native terminal scrollback). sel is the live left-drag text selection.

// transcriptSources runs parallel to transcript and retains raw, semantic
// content for blocks whose layout depends on terminal width. Fixed blocks
// keep their already-rendered text; markdown, user bubbles, reasoning, tool
// cards, and replay bundles are regenerated after a resize.

// wrappedLines is the viewport line cache; wrapBlockLines / wrapWidth /
// wrapBlockCount support append-only updates without re-wrapping the full
// history on every streaming commit (#6978).

// lastMouseReenable rate-limits ConPTY mouse re-enable sequences (#7583).
// mouseReenablePending + timer cover trailing-edge fires after a resize storm.

// wantMouseReenable is set by TurnDone (and similar settle points) and
// consumed once in Update so the raw enable sequence is batched with the
// frame that paints the settled state.

// autoScroll drives edge-drag scrolling: -1 up, +1 down, 0 off. dragX is the
// column the drag is held at, so the ticker can extend the selection head.

// scrollbarDrag owns left-button drags that start on the transcript scrollbar
// column. It is separate from text selection so the visual thumb is not a
// dead target and dragging it never leaves a transcript selection behind.

// copyNoticeText is a transient "copied to clipboard" hint shown on the status
// line after a mouse-drag, right-click, or Ctrl+C selection copy; "" when none
// is showing. copyNoticeSeq guards its expiry tick so an older copy's timer
// can't clear a newer notice — each copy bumps the sequence and only a tick
// carrying the current sequence clears the text.

// clipboardImagePending keeps the footer honest while the platform clipboard
// is being decoded and prevents repeated Ctrl+V presses from attaching the
// same image multiple times before the first read completes.

// The user bubble is echoed to scrollback immediately on Enter (bubbleStartIdx
// marks where in the transcript it landed). It stays "un-sendable" until the
// first response packet arrives: pressing Esc/Ctrl+C before then pops those
// lines back off the transcript and restores the text to the input box, leaving
// no trace. bubblePending is true from startTurn until the first packet confirms
// the send or it's un-sent; turnDiscarded then swallows the turn's
// already-buffered events until its TurnDone settles.

// pendingApproval holds the tool-call approval currently shown in the banner
// (nil when none). While set, the controller's run goroutine is blocked
// awaiting ctrl.Approve and key input is captured to answer it.

// chooser holds the `ask` tool's question card (nil when none). While set, the
// run goroutine is blocked awaiting ctrl.AnswerQuestion and keys drive the card.

// rewind holds the Esc-Esc / "/rewind" picker (nil when closed); while set,
// keys drive it and it renders as an overlay. lastEsc times the double-Esc
// gesture that opens it on an empty composer.

// resumePick is the interactive "/resume" session picker overlay. Non-nil
// while the user browses saved sessions with ↑/↓ and confirms with Enter.

// quickPick owns searchable single-choice overlays such as /model and
// /provider. It never invokes a raw-mode prompt inside Bubble Tea.

// mcp is the interactive "/mcp" manager overlay. mcpDisabled tracks servers
// turned off only for this chat session, matching the desktop connector
// toggle's non-persistent semantics.

// clearConfirm is the destructive "/clear" confirmation overlay. It is separate
// from /new because /clear discards the current transcript instead of saving it.

// lastCtrlCAt records when Ctrl+C was pressed while idle on an empty
// composer, enabling a "press again to quit" confirmation pattern (1.5s
// window). Reset when Ctrl+C clears non-empty input instead.

// mcpImport holds the interactive cc-switch MCP import picker (nil when
// closed). It writes selected servers to config and hot-connects the ones that
// can start successfully.

// host is the running MCP servers (nil when no plugins). The TUI reads
// prompts (slash commands), resources (@-references), and server status
// (/mcp) from it.

// commands are custom slash commands loaded from .reasonix/commands; each renders
// its template with the typed args and sends the result as a turn.

// skills are the discoverable skills (built-in + user/project); each is offered
// in the slash menu as "/<name>" and managed via /skills.

// slashCatalog is an immutable completion list rebuilt only on explicit
// invalidation (model switch, skill rescan, /reload-cmd, …). Ordinary
// keystrokes only filter this snapshot — no fingerprint walk (#6417, #7090).

// true when slashCatalog holds a valid snapshot

// skillPick is the interactive skill picker overlay for /skills. nil when closed.

// buildController builds a fresh controller for a model/profile pair, carrying prior
// history across and pinning auto-save to resumePath so the continued
// conversation stays in one file (set by chatREPL; it must NOT touch this
// model — the swap happens on the running copy). nil disables runtime
// rebuild commands. modelRef is the active "provider/model" ref, marked
// current in the picker. runtimeProfile stores boot's normalized token mode:
// full (displayed as balanced), economy, or delivery. oldCtrl is the
// outgoing controller, passed through so the replacement can carry forward
// same-session tool grants and Plan-mode read-only command trust that
// don't travel through carry/resumePath (see Controller.RestoreSessionAuthorizations).

// rebuildRuntime builds the /reload replacement through boot.Rebuild:
// same model/profile/effort, but tools, skills, commands, MCP
// servers, and providers are discovered fresh and the session state
// migrates inside the boot layer. Set by chatREPL (it must NOT touch
// this model — the swap happens on the running copy); nil disables
// /reload.

// pendingReload coalesces /reload requests made while a turn or a runtime
// switch is in flight; the TurnDone drain runs it once the TUI is idle.

// "" when the current provider/model has no configurable effort

// leases owns the session lease guarding the TUI's active session file (set
// by chatREPL; nil in tests and when persistence is disabled). Every in-TUI
// operation that rebinds the controller to another session file must move
// the lease first — see rebindSessionLease / followSessionLease.

// outputStyle is the active output-style name (config agent.output_style),
// shown as the current entry in the /output-style listing. "" = default.

// diffMaxLines controls the max lines shown in a diff view. 0 = show all;
// non-zero = fold at that many lines. Toggled by /diff-fold.

// statuslineCmd is the user's custom status-line command (config
// [statusline].command); "" disables it. statuslineOut caches its latest
// one-line stdout, refreshed at startup and after each turn and rendered in
// place of the built-in data row.

// statusLineCount is the number of terminal rows the status block occupies
// (wrapped working line + wrapped status line + wrapped data line). Updated
// each frame via computeStatusLineCount so bottomRows can reserve the correct
// height; starts at 2 (unwrapped) until first render.

// modelSwitchPending is true while any async controller rebuild is in flight.

// pendingModelSwitch holds the tea.Cmd that triggers the async build. The
// historical field name is retained because model, effort, skill refresh,
// and work-mode changes all share the same atomic swap path.

// oldControllers accumulates controllers retired by runtime switches.
// They cannot be closed during the switch (Close runs the SessionEnd lifecycle event
// and kills plugin subprocesses, both of which corrupt the terminal's
// raw mode). Instead they are closed at process exit when the terminal
// is already being restored.

// completion is the live autocomplete menu (slash commands; @-refs later).

// fileSearchCache memoizes fileref.Search by query so the bounded walk runs
// once per @token fragment, not on every keystroke that re-renders the menu.

// agentEventMsg is one typed event from the agent's run loop.

// maxEventDrain caps how many buffered events one Update coalesces before
// yielding to render, so a sustained output flood still shows live progress.

// compactDoneMsg reports that an async /compact pass returned. The card was
// already drawn from the CompactionDone event; this only surfaces a failure and
// snapshots on success.

// tuiShutdownMsg asks the live TUI model to persist its current controller and
// quit. It is injected from the signal handler so shutdown does not snapshot a
// stale controller captured before an in-TUI rebuild.

// shutdownNow is the tea.Cmd every in-TUI quit gesture returns instead of
// tea.Quit. Routing through tuiShutdownMsg gives all exits the same
// finalization (Snapshot + lease follow); quitting directly would drop
// whatever the controller holds beyond the last snapshot (#5879).

// elapsedTickMsg fires once a second while a turn runs, driving the "thinking
// Ns" counter in the status line.

// balanceMsg carries the result of an async wallet-balance fetch; text is the
// formatted readout ("" when none/failed).

// statuslineMsg carries the latest custom status-line output (one line, ""
// when none/failed).

// gitStatusMsg carries the latest lightweight git readout for the built-in
// status line. Empty means "not a git worktree" or "git unavailable".

// runStatusline runs the user's custom status-line command off the event loop,
// feeding it a small JSON context on stdin and returning its first stdout line.
// A no-op (nil) when no command is configured. Tight timeout so a slow script
// can't stall the UI; failures collapse to an empty line rather than an error.

const statuslineCommandTimeout = 2 * time.Second

// runStatuslineCmd runs a status-line command with the JSON context on stdin and
// returns its first stdout line (status lines are a single row). A tight timeout
// keeps a slow script from stalling the UI; any failure collapses to "".

// modelSwitchMsg carries the result of an async /model switch. A nil err means
// the new controller is ready in ctrl; label/commands/skills/host mirror the
// fields that runModelSubcommand used to set synchronously. oldCtrl is the
// previous controller that must be closed after the switch — its cleanup
// (SessionEnd lifecycle event, plugin subprocess kill) is deferred to a tea.Cmd so it
// runs after the render completes, avoiding corruption of the terminal's raw
// mode that would occur if Close() were called from the build goroutine.

// fetchBalance queries the provider's wallet balance off the event loop. It's a
// no-op readout ("") when the provider declares no balance_url or the fetch
// fails, so the status line stays quiet rather than surfacing an error.
// Wallets are displayed in their original currencies; no conversion or sum is
// attempted when more than one currency is returned.

// promptResolvedMsg carries the result of fetching an MCP prompt (an async
// prompts/get). display is the command line echoed as the user bubble; sent is
// the rendered prompt text that becomes the model turn.

// extensionActionMsg carries the result of invoking one extension UI action
// (an async extension/ui/action round-trip to the sidecar). The extension's
// (already redacted) message surfaces as a transcript notice.

// refsResolvedMsg carries the result of resolving the @references in a
// submitted line (async file reads / MCP resources/read).

// newChatTUI assembles the initial model. The controller has already been wired
// with an event sink that feeds eventCh; the TUI issues commands to it and
// renders the events it emits. Model identity, label, history, host, and commands
// are read from the controller, so explicit selections and resumed sessions stay
// authoritative.
func newChatTUI(ctrl control.SessionAPI, missing string, eventCh chan event.Event, termW int) chatTUI {
	ti := textarea.New()
	configureChatTextarea(&ti)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = themeStyle(activeCLITheme.accent)

	commitBuf := []string{}
	nativeScrollback := detectTermuxTerminal()
	history := ctrl.History()
	nextPasteID, usedPasteIDs := pasteIDStateForHistory(history)
	return chatTUI{
		ctrl:                 ctrl,
		label:                ctrl.Label(),
		modelRef:             ctrl.ModelRef(),
		missing:              missing,
		nativeScrollback:     nativeScrollback,
		legacyScrollClear:    useLegacyViewportScrollClear(runtime.GOOS, os.Environ()),
		mouseCaptureOff:      mouseCaptureOffByDefault(),
		input:                ti,
		spinner:              sp,
		submittedInputCursor: -1,
		queueEditCursor:      -1,
		nextPasteID:          nextPasteID,
		usedPasteIDs:         usedPasteIDs,
		reasoningLineIdx:     -1,
		reasoningTextIdx:     -1,
		answerIdx:            -1,
		toolStreamIdx:        -1,
		reasoning:            &strings.Builder{},
		pending:              &strings.Builder{},
		pendingCommit:        &commitBuf,
		diffMaxLines:         diffFoldLimit,
		showReasoning:        nativeScrollback,
		showTurnUsage:        true,
		shellOutputs:         make(map[string]string),
		shellExpanded:        make(map[string]bool),
		shellTranscriptIdx:   make(map[string]int),
		toolLineCountByID:    make(map[string]int),
		subagentProgressIdx:  make(map[string]int),
		subagentProgress:     make(map[string]*cliSubagentProgress),
		eventCh:              eventCh,
		history:              history,
		host:                 ctrl.Host(),
		commands:             ctrl.Commands(),
		skills:               ctrl.SlashSkills(),
		viewport:             viewport.New(viewport.WithWidth(termW)),
		statusLineCount:      3,
	}
}

var cliImageRefRe = regexp.MustCompile(`(?:^|\s)@\.reasonix/attachments/clipboard-\d{8}-\d{6}\.\d+(?:-(?:\d{6}|[a-f0-9]{8}))?\.(?:png|jpg|jpeg|gif|webp)`)

// eventSink is the event.Sink the agent emits to in TUI mode. Each event
// becomes an agentEventMsg. The channel is generously buffered so streaming
// bursts don't back-pressure the agent goroutine.
type eventSink struct {
	ch chan<- event.Event
}

func (s *eventSink) Emit(e event.Event) { s.ch <- e }
