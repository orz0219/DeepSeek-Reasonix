package cli

import (
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"

	"reasonix/internal/boot"
	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/skill"
)

// chatTUI is a bubbletea Model that normally owns the terminal with an
// alt-screen transcript viewport. Termux is the exception: it stays in the
// normal buffer and commits finalized output to native scrollback via
// tea.Println so taps can still focus the soft keyboard.
type chatTUI struct {
	ctrl    control.SessionAPI
	label   string
	missing string // missing-key warning surfaced once in the banner, "" when ready
	webHandoffState
	// diagnostics is the process-owned TUI log/watchdog started before terminal
	// takeover. Nil in unit tests that construct chatTUI without chatREPL.
	diagnostics      *tuiDiagnostics
	firstFrameLogged bool

	width  int
	height int
	// themeSweep freezes the frame while a /theme switch wipes across it.
	themeSweep *themeSweep
	// nativeScrollback keeps Termux out of alt-screen mode so taps still focus
	// the textarea and raise the soft keyboard.
	nativeScrollback bool
	// mouseCaptureOff releases mouse ownership back to the terminal (View() sets
	// tea.MouseModeNone instead of MouseModeCellMotion) so its native
	// click-drag selection and right-click context menu work again. Toggled by
	// "/mouse" or REASONIX_DISABLE_MOUSE at startup; trades away in-app
	// drag-select, the transcript scrollbar, and wheel-scroll while it's on,
	// since the terminal no longer forwards those events to Reasonix.
	mouseCaptureOff bool

	input       textarea.Model
	composerSel composerSelection
	composerMap composerLayoutCache
	// composerScrollOffset is an independent view offset used after the user
	// wheels inside an overflowing composer. The textarea keeps ownership of the
	// real insertion cursor; a subsequent edit or cursor key reattaches the view
	// to that cursor without the wheel having moved it.
	composerScrollOffset   int
	composerScrollDetached bool
	spinner                spinner.Model

	submittedInputs      []string
	submittedInputCursor int
	submittedInputDraft  string
	pastedBlocks         []pastedBlock
	nextPasteID          int
	usedPasteIDs         map[int]struct{}

	state    tuiState
	runStart time.Time
	elapsed  int
	// retryAttempt/retryMax drive the transient "retrying (n/m)" indicator while
	// the provider re-attempts the connection; cleared by the next stream event.
	retryAttempt int
	retryMax     int
	// turnPhase is the host turn phase from turn_phase events
	// (working|checking|verifying|reviewing). Cleared on TurnDone.
	turnPhase string
	// turnTokens accumulates this turn's output tokens (summed from per-step Usage
	// events) for the live "↓N" readout in the running status line.
	turnTokens int
	// showTurnUsage controls whether completed per-request token/cost receipts are
	// retained in transcript scrollback. Usage accounting remains active either way.
	showTurnUsage bool

	// balance is the last-fetched wallet-balance readout (e.g. "¥110.00"), "" when
	// the provider declares no balance_url or a fetch failed. Refreshed async on
	// startup and after each turn so the status line stays roughly current without
	// blocking the event loop.
	balance string

	// todoArgs is the latest todo_write call's raw args; it drives the task list
	// pinned just above the input (see renderTodoPanel). "" when there's no list.
	// Persists across turns until the work completes or a new session starts.
	todoArgs string

	// marker rides in outgoing user messages so the cache-stable prompt prefix is
	// left untouched.
	planMode bool
	// legacyScrollClear keeps the per-offset ClearScreen workaround only for Warp.
	legacyScrollClear bool
	// sessionSwitch suppresses that workaround during a transcript rebuild (#5441).
	sessionSwitch bool
	// yoloRestoreToolApprovalMode remembers the Ask/Auto base mode that Ctrl+Y
	// should restore after a desktop-style YOLO toggle.
	yoloRestoreToolApprovalMode string

	// inboxSelectedID is the currently highlighted durable inbox item while
	// browsing the queue in tuiRunning. Empty means "not browsing". Full bodies
	// are never cached here — only the selected ID and the snapshot metadata.
	inboxSelectedID string
	// queueEditCursor tracks which queued message the user is currently
	// browsing/editing via ↑/↓ during tuiRunning. -1 means "not browsing".
	queueEditCursor int
	// queueEditDraft saves the in-progress input text when the user first
	// presses ↑ to browse the queue, so it can be restored when the cursor
	// moves past the end.
	queueEditDraft string
	// queueConfirmDelete, when true, the next 'd' confirms deletion of the
	// selected inbox item.
	queueConfirmDelete bool

	// history is a resumed session's messages, committed to scrollback once on
	// the first WindowSizeMsg so a reopened chat shows its prior transcript.
	history []provider.Message

	// reasoning accumulates the in-progress thinking stream (dim); pending
	// accumulates the in-progress answer (raw markdown). They are committed to
	// scrollback (reasoning collapsed by default, answer markdown-rendered) when they
	// finalize — at a tool/usage boundary or turn end — not previewed live, so
	// the bottom region stays a stable height. pendingCommit queues finalized
	// lines so a single Update emits exactly one ordered tea.Println.
	reasoning     *strings.Builder
	pending       *strings.Builder
	pendingCommit *[]string
	showReasoning bool // Ctrl+O / /verbose: show raw thinking text in the CLI
	cfg           *config.Config
	// reasoningLineIdx is the transcript index of the live "▎ thinking…" marker
	// while a reasoning block streams; it's rewritten to "▎ thought for Ns" when
	// the block closes. -1 when no block is open. transcriptDirty forces a
	// viewport re-feed after that in-place rewrite (length is unchanged).
	reasoningLineIdx int
	// reasoningTextIdx is the transcript index of the live reasoning text block
	// (the block right after the marker), streamed in as the model thinks and
	// removed when the block collapses (kept only in verbose mode). -1 when none.
	reasoningTextIdx int
	// reasoningView is a bounded trailing window (≤ reasoningViewMax bytes) of the
	// streaming thought, rendered live; the full text stays in reasoning for verbose.
	reasoningView []byte
	// reasoningNative is the Termux/native-scrollback path: reasoning is buffered
	// without a live transcript block, then appended once as a final summary.
	reasoningNative bool
	thinkStart      time.Time
	// answerIdx is the transcript index of the streaming answer block (rewritten in
	// place as completed paragraphs arrive); -1 when none is open. answerFlushed is
	// how many bytes of pending have already been rendered into it, so a Text packet
	// that doesn't close a new paragraph re-renders nothing.
	answerIdx     int
	answerFlushed int
	// toolStreamIdx is the transcript index of a running tool's live-output block
	// (streamed via ToolProgress under the tool card); -1 when none. toolStreamID
	// is the call ID it belongs to. Only a bounded tail is kept — the last few
	// complete lines (toolTail) plus the in-progress one (toolPartial) — so a
	// high-output command can't balloon memory or cost O(n²) re-splitting;
	// toolLineCount feeds the collapse summary.
	toolStreamIdx int
	toolStreamID  string
	toolTail      []string
	toolPartial   string
	toolLineCount int
	// shellOutputs stores the full accumulated output of each shell command
	// (tool IDs with "shell-" prefix), so the first 10 lines can be shown after
	// collapse and Ctrl+B can toggle the complete output.
	shellOutputs  map[string]string
	shellExpanded map[string]bool
	// shellTranscriptIdx maps a shell tool ID to the transcript index of its
	// collapsed output block, so Ctrl+B can rewrite it in place.
	shellTranscriptIdx map[string]int
	// toolLineCountByID keeps a switched-away tool's last line count so a late
	// ToolResult can still render "⎿ N lines" (shellOutputs only tracks "shell-" ids).
	toolLineCountByID map[string]int
	// toolStreamStart / toolStreamFrame drive the "⎿ working · Ns" line shown
	// under a dispatched tool that hasn't produced output yet, so a slow tool
	// reads as making progress rather than frozen.
	toolStreamStart time.Time
	toolStreamFrame int
	// Sub-agent progress previews (reserved ToolProgress channels) render per
	// child into their own fixed transcript slot, keyed by the namespaced call
	// ID — independent of the single live toolStreamID. subagentProgress keeps
	// the bounded live state (phase, elapsed, recent activity, verbose tails).
	subagentProgressIdx map[string]int
	subagentProgress    map[string]*cliSubagentProgress
	transcriptDirty     bool
	// forceGotoBottom is set by replayActiveBranch and resetFreshContextView to
	// pin the viewport to the bottom after a session / branch / clear switch
	// regardless of the previous wasAtBottom state (#4584).
	forceGotoBottom bool
	// scrollMode is the explicit followTail / userScrolled state machine.
	// Prefer this over a raw wasAtBottom snapshot so modal height changes
	// (approval, chooser, pickers) never silently disable tail-follow (#6430).
	scrollMode scrollFollowMode
	eventCh    chan event.Event
	started    bool // banner + resumed history committed once

	// transcript holds every finalized line commitLine emits; the viewport
	// renders a scrollable window of it (alt-screen owns the grid, so there's no
	// native terminal scrollback). sel is the live left-drag text selection.
	transcript []string
	// transcriptSources runs parallel to transcript and retains raw, semantic
	// content for blocks whose layout depends on terminal width. Fixed blocks
	// keep their already-rendered text; markdown, user bubbles, reasoning, tool
	// cards, and replay bundles are regenerated after a resize.
	transcriptSources []transcriptSource
	// wrappedLines is the viewport line cache; wrapBlockLines / wrapWidth /
	// wrapBlockCount support append-only updates without re-wrapping the full
	// history on every streaming commit (#6978).
	wrappedLines   []string
	wrapBlockLines [][]string
	wrapWidth      int
	wrapBlockCount int
	// lastMouseReenable rate-limits ConPTY mouse re-enable sequences (#7583).
	// mouseReenablePending + timer cover trailing-edge fires after a resize storm.
	lastMouseReenable       time.Time
	mouseReenablePending    bool
	mouseReenableTimerArmed bool
	// wantMouseReenable is set by TurnDone (and similar settle points) and
	// consumed once in Update so the raw enable sequence is batched with the
	// frame that paints the settled state.
	wantMouseReenable bool
	viewport          viewport.Model
	sel               selection
	// autoScroll drives edge-drag scrolling: -1 up, +1 down, 0 off. dragX is the
	// column the drag is held at, so the ticker can extend the selection head.
	autoScroll int
	dragX      int
	// scrollbarDrag owns left-button drags that start on the transcript scrollbar
	// column. It is separate from text selection so the visual thumb is not a
	// dead target and dragging it never leaves a transcript selection behind.
	scrollbarDrag       bool
	scrollbarGrabOffset int
	// copyNoticeText is a transient "copied to clipboard" hint shown on the status
	// line after a mouse-drag, right-click, or Ctrl+C selection copy; "" when none
	// is showing. copyNoticeSeq guards its expiry tick so an older copy's timer
	// can't clear a newer notice — each copy bumps the sequence and only a tick
	// carrying the current sequence clears the text.
	copyNoticeText string
	copyNoticeSeq  int
	// clipboardImagePending keeps the footer honest while the platform clipboard
	// is being decoded and prevents repeated Ctrl+V presses from attaching the
	// same image multiple times before the first read completes.
	clipboardImagePending bool

	// The user bubble is echoed to scrollback immediately on Enter (bubbleStartIdx
	// marks where in the transcript it landed). It stays "un-sendable" until the
	// first response packet arrives: pressing Esc/Ctrl+C before then pops those
	// lines back off the transcript and restores the text to the input box, leaving
	// no trace. bubblePending is true from startTurn until the first packet confirms
	// the send or it's un-sent; turnDiscarded then swallows the turn's
	// already-buffered events until its TurnDone settles.
	pendingRestore string
	pendingPastes  []string
	bubbleStartIdx int
	bubblePending  bool
	turnDiscarded  bool

	// pendingApproval holds the tool-call approval currently shown in the banner
	// (nil when none). While set, the controller's run goroutine is blocked
	// awaiting ctrl.Approve and key input is captured to answer it.
	pendingApproval   *event.Approval
	approvalSelection int

	// chooser holds the `ask` tool's question card (nil when none). While set, the
	// run goroutine is blocked awaiting ctrl.AnswerQuestion and keys drive the card.
	chooser *chooser

	// rewind holds the Esc-Esc / "/rewind" picker (nil when closed); while set,
	// keys drive it and it renders as an overlay. lastEsc times the double-Esc
	// gesture that opens it on an empty composer.
	rewind *rewindPicker
	// resumePick is the interactive "/resume" session picker overlay. Non-nil
	// while the user browses saved sessions with ↑/↓ and confirms with Enter.
	resumePick *resumePicker
	// quickPick owns searchable single-choice overlays such as /model and
	// /provider. It never invokes a raw-mode prompt inside Bubble Tea.
	quickPick *quickPicker
	copyPick  *copyPicker
	lastEsc   time.Time

	// mcp is the interactive "/mcp" manager overlay. mcpDisabled tracks servers
	// turned off only for this chat session, matching the desktop connector
	// toggle's non-persistent semantics.
	mcp         *mcpManager
	mcpDisabled map[string]bool

	// clearConfirm is the destructive "/clear" confirmation overlay. It is separate
	// from /new because /clear discards the current transcript instead of saving it.
	clearConfirm *clearConfirm

	// lastCtrlCAt records when Ctrl+C was pressed while idle on an empty
	// composer, enabling a "press again to quit" confirmation pattern (1.5s
	// window). Reset when Ctrl+C clears non-empty input instead.
	lastCtrlCAt time.Time

	// mcpImport holds the interactive cc-switch MCP import picker (nil when
	// closed). It writes selected servers to config and hot-connects the ones that
	// can start successfully.
	mcpImport *mcpImportPicker

	// host is the running MCP servers (nil when no plugins). The TUI reads
	// prompts (slash commands), resources (@-references), and server status
	// (/mcp) from it.
	host *plugin.Host

	// commands are custom slash commands loaded from .reasonix/commands; each renders
	// its template with the typed args and sends the result as a turn.
	commands []command.Command

	// skills are the discoverable skills (built-in + user/project); each is offered
	// in the slash menu as "/<name>" and managed via /skills.
	skills []skill.Skill

	// slashCatalog is an immutable completion list rebuilt only on explicit
	// invalidation (model switch, skill rescan, /reload-cmd, …). Ordinary
	// keystrokes only filter this snapshot — no fingerprint walk (#6417, #7090).
	slashCatalog     []compItem
	slashCatalogOnce bool // true when slashCatalog holds a valid snapshot

	// skillPick is the interactive skill picker overlay for /skills. nil when closed.
	skillPick *skillPicker

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
	buildController func(spec controllerBuildSpec, carry []provider.Message, resumePath string, oldCtrl control.SessionAPI) (*control.Controller, error)
	// rebuildRuntime builds the /reload replacement through boot.Rebuild:
	// same model/profile/effort, but tools, skills, commands, MCP
	// servers, and providers are discovered fresh and the session state
	// migrates inside the boot layer. Set by chatREPL (it must NOT touch
	// this model — the swap happens on the running copy); nil disables
	// /reload.
	rebuildRuntime  runtimeRebuilder
	lastBuildResult *boot.BuildResult
	// pendingReload coalesces /reload requests made while a turn or a runtime
	// switch is in flight; the TurnDone drain runs it once the TUI is idle.
	pendingReload  bool
	modelRef       string
	runtimeProfile string
	effortLevel    string // "" when the current provider/model has no configurable effort

	// leases owns the session lease guarding the TUI's active session file (set
	// by chatREPL; nil in tests and when persistence is disabled). Every in-TUI
	// operation that rebinds the controller to another session file must move
	// the lease first — see rebindSessionLease / followSessionLease.
	leases *control.SessionLeaseKeeper

	// outputStyle is the active output-style name (config agent.output_style),
	// shown as the current entry in the /output-style listing. "" = default.
	outputStyle string

	// diffMaxLines controls the max lines shown in a diff view. 0 = show all;
	// non-zero = fold at that many lines. Toggled by /diff-fold.
	diffMaxLines int

	// statuslineCmd is the user's custom status-line command (config
	// [statusline].command); "" disables it. statuslineOut caches its latest
	// one-line stdout, refreshed at startup and after each turn and rendered in
	// place of the built-in data row.
	statuslineCmd string
	statuslineOut string
	gitStatus     gitStatus

	// statusLineCount is the number of terminal rows the status block occupies
	// (wrapped working line + wrapped status line + wrapped data line). Updated
	// each frame via computeStatusLineCount so bottomRows can reserve the correct
	// height; starts at 2 (unwrapped) until first render.
	statusLineCount int

	// modelSwitchPending is true while any async controller rebuild is in flight.
	modelSwitchPending bool
	// pendingModelSwitch holds the tea.Cmd that triggers the async build. The
	// historical field name is retained because model, effort, skill refresh,
	// and work-mode changes all share the same atomic swap path.
	pendingModelSwitch tea.Cmd
	// oldControllers accumulates controllers retired by runtime switches.
	// They cannot be closed during the switch (Close runs the SessionEnd lifecycle event
	// and kills plugin subprocesses, both of which corrupt the terminal's
	// raw mode). Instead they are closed at process exit when the terminal
	// is already being restored.
	oldControllers []control.SessionAPI

	// completion is the live autocomplete menu (slash commands; @-refs later).
	completion completion
	// fileSearchCache memoizes fileref.Search by query so the bounded walk runs
	// once per @token fragment, not on every keystroke that re-renders the menu.
	fileSearchCache map[string][]string
}

type tuiState int

type controllerBuildSpec struct {
	ModelRef         string
	RuntimeProfile   string
	ToolApprovalMode string
	PlanMode         bool
	EffortOverride   *string
}

func (m *chatTUI) runtimeSwitchBusy() bool {
	if m == nil || m.ctrl == nil {
		return false
	}
	status := m.ctrl.RuntimeStatus()
	return status.Running || status.PendingPrompt || status.BackgroundJobs > 0 || m.pendingApproval != nil || m.chooser != nil
}

const (
	tuiIdle tuiState = iota
	tuiRunning
)
