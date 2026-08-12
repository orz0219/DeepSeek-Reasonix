// Package hook runs user-configured shell-command hooks around the agent loop:
// PreToolUse / PostToolUse fire around each tool call, PermissionRequest fires
// before a tool approval prompt is shown, UserPromptSubmit before a turn, Stop
// after it. Hooks come from settings.json — a project
// (.reasonix/settings.json, only when the project is trusted) and a global
// (<Reasonix home>/settings.json) file. A hook's exit
// code is its verdict: 0 = pass, 2 = block (only on the gating events), other =
// warn. The payload is delivered as JSON on stdin; output is captured (capped)
// and surfaced to the user. This package only loads, matches, and runs hooks;
// the agent and controller decide what a block means (see internal/agent,
// internal/control).
package hook

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf16"

	"reasonix/internal/config"
	"reasonix/internal/sandbox"
)

// Event is a point in the agent loop a hook can fire at.
type Event string

const (
	PreToolUse         Event = "PreToolUse"
	PostToolUse        Event = "PostToolUse"
	PostToolUseFailure Event = "PostToolUseFailure"
	PermissionRequest  Event = "PermissionRequest"
	UserPromptSubmit   Event = "UserPromptSubmit"
	Stop               Event = "Stop"
	StopFailure        Event = "StopFailure"
	// PostLLMCall fires after every model turn completes (streaming finishes) but
	// before the reasoning_content is stored in the session. The hook receives the
	// raw reasoning text in the payload; its stdout, if non-empty on exit 0,
	// replaces the reasoning stored and displayed to the user. It can't block — a
	// non-zero exit or empty stdout leaves the reasoning unchanged.
	PostLLMCall Event = "PostLLMCall"
	// SessionStart fires once when a session becomes active (fresh, resumed, or
	// after /new). SessionEnd fires when it is closed or rotated. SubagentStop
	// fires when a `task` sub-agent finishes. Notification fires when the agent
	// needs the user's attention (e.g. a pending approval). PreCompact fires just
	// before a compaction pass; its stdout is injected as extra summary guidance.
	SessionStart Event = "SessionStart"
	SessionEnd   Event = "SessionEnd"
	SubagentStop Event = "SubagentStop"
	Notification Event = "Notification"
	PreCompact   Event = "PreCompact"
)

// Events is every event, in a stable order — drives loading and `/hooks`.
var Events = []Event{
	PreToolUse, PostToolUse, PostToolUseFailure, PermissionRequest, UserPromptSubmit, Stop, StopFailure,
	PostLLMCall,
	SessionStart, SessionEnd, SubagentStop, Notification, PreCompact,
}

// IsBlocking reports whether a non-zero/exit-2 (or timed-out) hook on this event
// can block the loop. Only the gating events qualify. (PreCompact does not block;
// it only contributes guidance via stdout.) This governs native Reasonix hooks;
// see claudePermissionBlocking for the Claude-imported PermissionRequest case.
func IsBlocking(e Event) bool { return e == PreToolUse || e == UserPromptSubmit }

// claudePermissionBlocking reports whether exit code 2 (or a timeout) on h
// aborts the action even though PermissionRequest is not one of Reasonix's own
// blocking events (docs/DESKTOP_HOOKS.md: "只有 PreToolUse 和 UserPromptSubmit
// 是阻塞型事件"). Claude's own PermissionRequest contract denies the permission
// on exit 2 the same way PreToolUse does (https://code.claude.com/docs/en/hooks),
// so an imported Claude hook (PayloadFormat "claude") honors that instead of
// silently downgrading to a notification.

// defaultTimeout is the per-event timeout when a hook sets none. Tool/prompt
// hooks gate progress, so they're tight; post/stop hooks get more room.
func defaultTimeout(e Event) time.Duration {
	switch e {
	case PreToolUse, PermissionRequest, UserPromptSubmit:
		return 5 * time.Second
	default:
		return 30 * time.Second
	}
}

// Scope records which settings.json a hook came from. Project hooks fire before
// global ones.
type Scope string

const (
	ScopeProject Scope = "project"
	ScopePlugin  Scope = "plugin"
	ScopeGlobal  Scope = "global"
)

// ExecutionMode is the contract between a hook manifest and its process
// launcher. The zero value is the legacy Reasonix settings behavior, where a
// command string is interpreted by the platform shell after compatibility
// repairs. Plugin manifests can opt into an unambiguous exec or shell form.
type ExecutionMode string

const (
	ExecutionLegacy ExecutionMode = ""
	ExecutionExec   ExecutionMode = "exec"
	ExecutionShell  ExecutionMode = "shell"
)

// HookConfig is one hook as written in settings.json.
type HookConfig struct {
	// Match is an anchored regex selecting tools (Pre/PostToolUse and
	// PermissionRequest only); "" or "*" = every tool. Anchored: "file" won't
	// match "read_file" — use ".*file".
	Match string `json:"match,omitempty"`
	// Command is the executable, shell script, or legacy shell command to run,
	// according to ExecutionMode.
	Command string `json:"command"`
	// Argv is the literal argument vector for exec-form plugin hooks.
	Argv []string `json:"-"`
	// ExecutionMode and Shell are internal plugin-package metadata. Native
	// Reasonix settings retain their legacy shell-command behavior.
	ExecutionMode ExecutionMode `json:"-"`
	Shell         string        `json:"-"`
	// ContextFile is an internal plugin-package helper: when set, the hook reads
	// this file and treats it as stdout instead of spawning a shell command.
	ContextFile string `json:"contextFile,omitempty"`
	// Description is an optional human label surfaced in `/hooks`.
	Description string `json:"description,omitempty"`
	// Timeout overrides the per-event default, in milliseconds.
	Timeout int `json:"timeout,omitempty"`
	// Cwd overrides the working directory (defaults to the payload's cwd).
	Cwd string `json:"cwd,omitempty"`
	// Env adds environment variables for this hook invocation.
	Env map[string]string `json:"env,omitempty"`
	// Async and PayloadFormat are internal compatibility metadata populated for
	// imported Claude hooks. Native Reasonix settings keep their old behavior.
	Async         bool   `json:"-"`
	PayloadFormat string `json:"-"`
}

// Settings is the shape of a settings.json (only hooks for now).
type Settings struct {
	Hooks map[Event][]HookConfig `json:"hooks"`
}

// ResolvedHook is a loaded hook with its origin baked in.
type ResolvedHook struct {
	HookConfig
	Event  Event
	Scope  Scope
	Source string // absolute path to the settings.json it came from
}

func (h ResolvedHook) timeout() time.Duration {
	if h.Timeout > 0 {
		return time.Duration(h.Timeout) * time.Millisecond
	}
	return defaultTimeout(h.Event)
}

// SettingsDirname / SettingsFilename locate a scope's settings.json.
const (
	SettingsDirname  = ".reasonix"
	SettingsFilename = "settings.json"
)

// GlobalSettingsPath is <Reasonix home>/settings.json (homeDir overrides ~ for
// tests and legacy callers).
func GlobalSettingsPath(homeDir string) string {
	return filepath.Join(reasonixHome(homeDir), SettingsFilename)
}

// ProjectSettingsPath is <root>/.reasonix/settings.json.
func ProjectSettingsPath(projectRoot string) string {
	return filepath.Join(projectRoot, SettingsDirname, SettingsFilename)
}

// ContextFileUsable reports whether a plugin contextFile can take the same
// execution path as readContextFile. Keep machine status and diagnostics on
// this shared predicate so a path that merely exists (for example, a
// directory) is not advertised as runnable.
func ContextFileUsable(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	return file.Close() == nil
}

// LoadOptions configure Load.

// HomeDir overrides the OS user home used by legacy callers and tests. The
// derived global path is <HomeDir>/.reasonix unless ReasonixHomeDir is set.

// ReasonixHomeDir is the exact current Reasonix home (settings.json lives
// directly under it). When set, it takes precedence over HomeDir for global
// settings and plugin hooks so Windows %APPDATA%/reasonix and REASONIX_HOME
// isolation stay consistent across hook/doctor/capdiag (#7411, #7331).

// Trusted is retained for source compatibility. Project hooks are enabled
// automatically now, so callers no longer need to set it.

// Load resolves hooks: project first, then global; within a scope,
// settings.json array order. A malformed file yields no hooks (never an error
// — a typo shouldn't take down the CLI).

// ProjectDefinesHooks reports whether a project's settings.json exists and
// declares at least one hook.

// malformed → treat as no hooks, don't crash

// Plugin hook manifests are host configuration, not platform-native shell
// scripts. Scan the manifest value once so text inside the resolved root is
// never mistaken for another placeholder and expanded recursively.

// MatchesTool reports whether a hook applies to toolName. The match field is an
// anchored regex; non-tool events always match. A malformed regex never fires
// (safer than firing on everything).

// claudeAgentSpawningTools are every Reasonix tool that spawns a subagent and
// so corresponds to Claude's single "Agent" tool: the general task delegator
// (task/read_only_task/parallel_tasks) and the dedicated named wrappers
// around a runAs=subagent skill (BuiltinSubagentTools in
// internal/skill/tools.go — each is a distinct, directly-callable tool, not
// routed through run_skill). A Claude "Agent" safety matcher must see all of
// them, or a hook scoped to it silently misses whichever entry point wasn't
// mapped.

// claudeAgentDefaultDescriptions fill Claude Agent's required description
// field when the corresponding Reasonix tool does not expose one or the model
// omitted Reasonix's optional description. These are stable operation labels;
// the complete task remains in prompt for hook policy decisions.

// claudeToolNames maps Reasonix's own tool names to the *current* Claude Code
// built-in tool name (https://code.claude.com/docs/en/tools-reference) — what
// an imported hook's emitted tool_name payload field shows, and a script's own
// tool_name check is written against. MCP tool names already share the
// mcp__<server>__<tool> convention in both systems.

// claudeToolMatchAliases lists every tool name — current and legacy — an
// imported hook's matcher may have been authored against for a Reasonix
// tool, so a matcher written against an older Claude Code tool name keeps
// firing after Claude renames the tool (Task became Agent; BashOutput/KillShell
// became TaskOutput/TaskStop). claudeFacingToolName (the emitted tool_name
// payload) always reports the current name; only matcher evaluation considers
// aliases.

// claudeMatchNames returns every name an imported hook's matcher should be
// tried against for a Reasonix tool call.

// claudeFacingToolName returns the current Claude tool name a Claude-imported
// hook's tool_name payload field should see for a Reasonix tool call.
// Reasonix-only tools (wait, code_index, move_file, ...) have no Claude
// equivalent and pass through unchanged — an imported hook can't have been
// authored against a name Claude never had.

// claudeToolInputKeyRenames maps, per Reasonix tool name, JSON keys in its
// tool-call arguments that must be renamed to Claude's own tool_input field
// name — Reasonix's file tools use "path", Claude's use "file_path" — so a
// hook script reading e.g. ".tool_input.file_path" sees the value instead of
// failing open on an empty field. Only tools whose Reasonix schema differs
// from Claude's by a plain key rename are listed: Bash's "command",
// Glob/Grep's "pattern"/"path", web_fetch's "url", ask's "questions",
// todo_write's "todos", and task/read_only_task's "prompt"/"description"
// already use Claude's field names. Agent description can still be absent and
// is filled separately below. NotebookEdit's cell_number (a
// 0-based index) has no Claude field — Claude targets cells only by the
// opaque cell_id, which Reasonix also accepts — so it passes through as an
// extra key. parallel_tasks is a structural mismatch handled separately in
// claudeFacingToolInput.

// The dedicated subagent wrappers take their task text as "task";
// Claude's Agent tool calls the same thing "prompt".

// claudeAbsolutePathInputKeys are the translated tool_input keys whose Claude
// schema demands an absolute path ("must be absolute, not relative" on
// Read/Write/Edit/NotebookEdit). Reasonix's file tools accept relative paths
// and resolve them against the workspace root (resolveIn in
// internal/tool/builtin/workspace.go); the payload resolves against
// payload.Cwd — the same root — so a prefix-matching guard inspects the path
// the tool actually accesses, not a relative spelling it never compares.

// claudeFacingToolInput adapts tool-call arguments to the tool_input a
// Claude-authored hook script was written against: keys are renamed per
// claudeToolInputKeyRenames, file paths are made absolute, current TaskOutput
// fields and required Agent/AskUserQuestion/TodoWrite fields are supplied, and
// parallel_tasks synthesizes Agent's "prompt". Args needing no translation, or
// that aren't a JSON object, pass through unchanged.

// An unbounded Reasonix wait omits TaskOutput's optional timeout
// entirely: in Claude's schema timeout is the maximum wait in ms, so
// claiming 0 would read as "don't wait" — the opposite of the call.

// parallel_tasks maps to Claude's Agent tool but carries an array of
// sub-tasks where Agent has a single prompt — a structural difference no
// key rename bridges. Synthesize "prompt" from every sub-task's prompt
// (the original "tasks" array stays alongside) so an Agent-scoped guard
// reading .tool_input.prompt inspects all dispatched work instead of
// failing open on a missing field.

// fillClaudeAskDefaults supplies fields Claude requires but Reasonix treats as
// optional. Empty option descriptions are honest (Reasonix has no explanation
// to add), and omitted multiSelect has the same false default in both systems.

// fillClaudeTodoDefaults supplies Claude's required activeForm label from the
// Reasonix task content when the caller omitted it.

// joinedParallelTaskPrompts flattens a parallel_tasks "tasks" array into one
// prompt string, blank-line separated. Malformed or empty input yields "".

// Payload is the JSON envelope written to a hook's stdin.

// Notification: what needs attention
// PreCompact: "auto" | "manual"
// PostLLMCall: the model's raw reasoning text

// Decision is a single hook invocation's verdict.

const (
	DecisionPass  Decision = "pass"
	DecisionBlock Decision = "block"
	DecisionWarn  Decision = "warn"
	DecisionError Decision = "error" // spawn failed (ENOENT, EACCES, …)
)

// Outcome records one hook invocation.

// -1 when unknown (killed / spawn error)

// Report aggregates the outcomes of running an event's hooks.

// at least one outcome blocked (only meaningful on gating events)
// Allowed is set when a Claude-imported PermissionRequest hook returned an
// explicit JSON "allow" decision on exit 0 (see claudeJSONAllow) — the
// caller should treat this as an auto-approval instead of prompting.

// HookOutput is the parsed, model-facing part of a successful hook stdout.

// Deny and DenyReason carry a Claude-style JSON deny decision returned on
// exit 0: hookSpecificOutput.permissionDecision for PreToolUse,
// hookSpecificOutput.decision.behavior for PermissionRequest, or a
// top-level decision:"block" for UserPromptSubmit. Claude hooks commonly
// deny this way instead of exiting 2; see
// https://code.claude.com/docs/en/hooks.

// Allow carries a Claude PermissionRequest "allow" decision
// (hookSpecificOutput.decision.behavior == "allow"): the hook answers the
// permission dialog on the user's behalf instead of only observing it.

// Decision and Reason are UserPromptSubmit's (and Stop/SubagentStop's)
// top-level deny shape: {"decision":"block","reason":"..."}.

// ParseOutput extracts hook-specific context from stdout. Plain text is accepted
// for SessionStart compatibility; JSON output must identify the current event.

// decideOutcome maps a spawn result to a verdict for hook h.

// claudeJSONDeny reports whether a Claude-format hook's exit-0 stdout still
// carries a JSON deny decision (see HookOutput.Deny). Reasonix must honor it
// for the events it claims Claude hook compatibility for, or a plugin's
// "block this dangerous command" hook silently no-ops whenever the script
// signals deny via JSON instead of exit code 2. UserPromptSubmit uses a
// top-level decision:"block" instead of PreToolUse/PermissionRequest's
// hookSpecificOutput shape; ParseOutput handles both.

// claudeJSONAllow reports whether a Claude-format PermissionRequest hook's
// exit-0 stdout carries an explicit "allow" decision
// (hookSpecificOutput.decision.behavior == "allow"): the hook answers the
// permission dialog on the user's behalf, same as an exit-2 deny preempts it.

// SpawnInput / SpawnResult / Spawner are the test seam around the real spawn.

// RuntimeOptions carries resolved host dependencies into Hook execution.
// It is runtime-only and never changes persisted Hook configuration.

// RuntimeOptionsForShell carries an explicitly configured Bash path into Hook
// execution while leaving other interpreter preferences independent.

// RuntimeIssue identifies one plugin Hook whose host dependency is unavailable.

// CheckPackageRuntime validates every Hook exported by a plugin package without
// launching commands.

// outputCapBytes bounds per-stream capture so a runaway child can't blow up the
// heap between spawn and timeout.

// Run executes the hooks matching payload.Event (and, for tool events, the tool
// name), feeding each the JSON payload on stdin. It stops at the first block so
// a gating hook can prevent later hooks running against a phantom success.

// claudeToolResponse adapts a Reasonix tool result to the tool_response a
// Claude-authored PostToolUse hook reads. Claude's Bash response is an object
// — {stdout, stderr, interrupted}, the fields the official security-guidance
// plugin's commit/push checks read (a non-object response is treated as empty
// and the check silently passes) — while Reasonix's bash returns one combined
// output string, so it is wrapped with the failure error as stderr. Other
// tools' results pass through as before: raw JSON when the result is a JSON
// document, else the plain string.

// stderrFor returns the best human message for an outcome: real stderr, else a
// spawn-error message, else a timeout note.

// DefaultSpawner executes the hook according to its explicit execution
// contract, with the payload on stdin, capped output, and both per-hook timeout
// and parent-context cancellation.

// NewDefaultSpawner returns the standard Hook spawner with effective host
// runtime paths supplied by boot configuration.

// WaitDelay bounds Wait even if a grandchild keeps a pipe open after the
// shell is killed on timeout/cancel.

// spawnCommand picks the execution vehicle from the manifest contract.
// Explicit exec-form hooks pass their argv directly to the executable;
// explicit shell-form hooks pass the raw command to the selected interpreter.
// Legacy settings retain Reasonix's historical shell behavior and repairs.

// spawnLegacyCommand preserves the pre-contract behavior:
//   - a command this call just repaired (its broken quoting means it never
//     worked through a shell, so there is no expansion behavior to preserve);
//   - on Windows, a recognized node -e stdin-hook command: `cmd /c` mangles
//     quoted JS (&, %, nested quotes), which is the breakage this repair
//     exists for, and cmd performs no POSIX-style $ expansion to preserve.
//   - on Windows, an explicit `sh -c` / `bash -c` command: Git Bash is often
//     installed outside cmd.exe's PATH, and direct exec preserves its quoting.
//
// POSIX commands that were already well-formed keep their shell semantics
// verbatim — normalizeStaticNodeEval's rendering escapes $ and backticks, so
// even repaired commands re-entering here behave identically under sh -c.

// Retain the established #6668 compatibility path for the common
// quoted .cmd/.bat hook shape. More complex scripts continue to
// the selected shell without being parsed or re-rendered.

// CheckRuntime reports an unavailable host dependency without running a Hook.
func CheckRuntime(config HookConfig, options RuntimeOptions) error {
	return checkRuntimeForPlatform(config, options, runtime.GOOS, resolveWindowsHookBash)
}

func checkRuntimeForPlatform(config HookConfig, options RuntimeOptions, goos string, resolveBash func(string) (string, error)) error {
	if goos != "windows" || !requiresWindowsBash(config) {
		return nil
	}
	_, err := resolveBash(options.BashPath)
	return err
}

func requiresWindowsBash(config HookConfig) bool {
	return requiresWindowsBashForHook(config)
}

func rawShellCommand(ctx context.Context, sh sandbox.Shell, command string) (*exec.Cmd, error) {
	path, err := resolvedHookShellPath(sh)
	if err != nil {
		return nil, err
	}
	if sh.Kind == sandbox.ShellPowerShell {
		return powerShellCommand(ctx, path, command), nil
	}
	return exec.CommandContext(ctx, path, "-c", command), nil
}

func powerShellCommand(ctx context.Context, path, command string) *exec.Cmd {
	// PowerShell's native command-line parser does not follow
	// CommandLineToArgvW consistently for a complex -Command argument.
	// -EncodedCommand transports the exact script as UTF-16LE and avoids a
	// second layer of quote/backslash interpretation. Force captured output to
	// UTF-8 before encoding so Windows PowerShell does not emit the host console
	// code page into Reasonix's stdout/stderr text contract.
	command = sandbox.PowerShellUTF8Script(command)
	codeUnits := utf16.Encode([]rune(command))
	raw := make([]byte, len(codeUnits)*2)
	for i, unit := range codeUnits {
		raw[i*2] = byte(unit)
		raw[i*2+1] = byte(unit >> 8)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	return exec.CommandContext(ctx, path, "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded)
}

func shellInvocation(command string) (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", command}
	}
	return "sh", []string{"-c", command}
}

// cappedBuffer is an io.Writer that stops storing after outputCapBytes and
// records that it truncated, but keeps reporting full writes so the child never
// sees a short-write error.
type cappedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	remaining := outputCapBytes - c.buf.Len()
	if remaining <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf.Write(p[:remaining])
		c.truncated = true
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

func (c *cappedBuffer) Bytes() []byte  { return c.buf.Bytes() }
func (c *cappedBuffer) String() string { return c.buf.String() }

func reasonixHome(override string) string {
	if override != "" {
		return filepath.Join(override, SettingsDirname)
	}
	if dir := config.ReasonixHomeDir(); dir != "" {
		return dir
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, SettingsDirname)
	}
	return ""
}

func reasonixHomeForOptions(opts LoadOptions) string {
	if dir := strings.TrimSpace(opts.ReasonixHomeDir); dir != "" {
		return filepath.Clean(dir)
	}
	return reasonixHome(opts.HomeDir)
}

func legacyGlobalSettingsPath(homeDir string) string {
	dir := legacyReasonixHome(homeDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, SettingsFilename)
}

func legacyReasonixHome(override string) string {
	if override != "" {
		return ""
	}
	if config.IsolatedHomeDir() != "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	legacy := filepath.Join(home, SettingsDirname)
	if sameCleanPath(legacy, reasonixHome("")) {
		return ""
	}
	return legacy
}

func sameCleanPath(a, b string) bool {
	if strings.TrimSpace(a) == "" || strings.TrimSpace(b) == "" {
		return false
	}
	if aa, err := filepath.Abs(a); err == nil {
		a = aa
	}
	if bb, err := filepath.Abs(b); err == nil {
		b = bb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
