package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/plugin"
	"reasonix/internal/skill"
)

// agentEventMsg is one typed event from the agent's run loop.
type agentEventMsg event.Event

// maxEventDrain caps how many buffered events one Update coalesces before
// yielding to render, so a sustained output flood still shows live progress.
const maxEventDrain = 512

const resetMouseTracking = ansi.ResetModeMouseX10 +
	ansi.ResetModeMouseNormal +
	ansi.ResetModeMouseHighlight +
	ansi.ResetModeMouseButtonEvent +
	ansi.ResetModeMouseAnyEvent +
	ansi.ResetModeMouseExtSgr +
	ansi.ResetModeMouseExtUtf8 +
	ansi.ResetModeMouseExtUrxvt +
	ansi.ResetModeMouseExtSgrPixel

// compactDoneMsg reports that an async /compact pass returned. The card was
// already drawn from the CompactionDone event; this only surfaces a failure and
// snapshots on success.
type compactDoneMsg struct{ err error }

// tuiShutdownMsg asks the live TUI model to persist its current controller and
// quit. It is injected from the signal handler so shutdown does not snapshot a
// stale controller captured before an in-TUI rebuild.
type tuiShutdownMsg struct{}

// shutdownNow is the tea.Cmd every in-TUI quit gesture returns instead of
// tea.Quit. Routing through tuiShutdownMsg gives all exits the same
// finalization (Snapshot + lease follow); quitting directly would drop
// whatever the controller holds beyond the last snapshot (#5879).
func shutdownNow() tea.Msg { return tuiShutdownMsg{} }

// elapsedTickMsg fires once a second while a turn runs, driving the "thinking
// Ns" counter in the status line.
type elapsedTickMsg struct{}

// balanceMsg carries the result of an async wallet-balance fetch; text is the
// formatted readout ("" when none/failed).
type balanceMsg struct{ text string }

// statuslineMsg carries the latest custom status-line output (one line, ""
// when none/failed).
type statuslineMsg struct{ out string }

// gitStatusMsg carries the latest lightweight git readout for the built-in
// status line. Empty means "not a git worktree" or "git unavailable".
type gitStatusMsg struct{ status gitStatus }

// runStatusline runs the user's custom status-line command off the event loop,
// feeding it a small JSON context on stdin and returning its first stdout line.
// A no-op (nil) when no command is configured. Tight timeout so a slow script
// can't stall the UI; failures collapse to an empty line rather than an error.
func (m chatTUI) runStatusline() tea.Cmd {
	cmd := m.statuslineCmd
	if cmd == "" {
		return nil
	}
	used, window := m.ctrl.ContextSnapshot()
	cwd, _ := os.Getwd()
	payload, _ := json.Marshal(map[string]any{
		"model":         m.label,
		"contextUsed":   used,
		"contextWindow": window,
		"cwd":           cwd,
	})
	return func() tea.Msg { return statuslineMsg{out: runStatuslineCmd(cmd, string(payload))} }
}

// runStatuslineCmd runs a status-line command with the JSON context on stdin and
// returns its first stdout line (status lines are a single row). A tight timeout
// keeps a slow script from stalling the UI; any failure collapses to "".
func runStatuslineCmd(cmd, stdinPayload string) string {
	return runStatuslineCmdWithTimeout(cmd, stdinPayload, statuslineCommandTimeout)
}

func runStatuslineCmdWithTimeout(cmd, stdinPayload string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	c.Stdin = strings.NewReader(stdinPayload + "\n")
	raw, _ := c.Output()
	out := strings.TrimSpace(string(raw))
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		out = strings.TrimSpace(out[:i])
	}
	return out
}

func (m chatTUI) refreshGitStatus() tea.Cmd {
	if m.statuslineCmd != "" {
		return nil
	}
	return fetchGitStatus()
}

// modelSwitchMsg carries the result of an async /model switch. A nil err means
// the new controller is ready in ctrl; label/commands/skills/host mirror the
// fields that runModelSubcommand used to set synchronously. oldCtrl is the
// previous controller that must be closed after the switch — its cleanup
// (SessionEnd lifecycle event, plugin subprocess kill) is deferred to a tea.Cmd so it
// runs after the render completes, avoiding corruption of the terminal's raw
// mode that would occur if Close() were called from the build goroutine.
type modelSwitchMsg struct {
	ref           string
	profile       string
	ctrl          control.SessionAPI
	oldCtrl       control.SessionAPI
	label         string
	commands      []command.Command
	skills        []skill.Skill
	host          *plugin.Host
	failurePrefix string
	successNotice string
	err           error
}

// fetchBalance queries the provider's wallet balance off the event loop. It's a
// no-op readout ("") when the provider declares no balance_url or the fetch
// fails, so the status line stays quiet rather than surfacing an error.
// Wallets are displayed in their original currencies; no conversion or sum is
// attempted when more than one currency is returned.
func fetchBalance(ctrl control.Status) tea.Cmd {
	return func() tea.Msg {
		b, err := ctrl.Balance(context.Background())
		if err != nil || b == nil {
			return balanceMsg{}
		}
		displayCurrency := ""
		if cfg, err := config.LoadForRootReadOnly("."); err == nil && cfg != nil {
			displayCurrency = cfg.ExplicitDisplayCurrency()
		}
		return balanceMsg{text: b.DisplayForCurrency(displayCurrency)}
	}
}

// promptResolvedMsg carries the result of fetching an MCP prompt (an async
// prompts/get). display is the command line echoed as the user bubble; sent is
// the rendered prompt text that becomes the model turn.
type promptResolvedMsg struct {
	display string
	sent    string
	err     error
}

// extensionActionMsg carries the result of invoking one extension UI action
// (an async extension/ui/action round-trip to the sidecar). The extension's
// (already redacted) message surfaces as a transcript notice.
type extensionActionMsg struct {
	message string
	err     error
}

// refsResolvedMsg carries the result of resolving the @references in a
// submitted line (async file reads / MCP resources/read).
type refsResolvedMsg struct {
	sent    string
	display string
	restore string
	block   string
	errs    []string
}

type clipboardImageMsg struct {
	path string
	err  error
}
