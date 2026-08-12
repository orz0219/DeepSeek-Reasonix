package acp

import (
	"encoding/json"
)

// FSReadTextFileParams asks the client for a file's current text, including
// unsaved editor state. Line (1-based) and Limit page the content; Reasonix
// always reads whole files and pages locally, so it sends neither.
type FSReadTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	Limit     *int   `json:"limit,omitempty"`
}

// FSReadTextFileResult carries the file content.
type FSReadTextFileResult struct {
	Content string `json:"content"`
}

// FSWriteTextFileParams asks the client to write content to path, updating any
// open buffer as well as the file on disk.
type FSWriteTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

// TerminalCreateParams starts a command in a client-owned terminal.
// Env follows ACP v1's official EnvVariable[] shape (same as MCP env): only
// the overrides Reasonix owns (typically TMPDIR/TMP/TEMP) are sent — never a
// full host environment dump.
type TerminalCreateParams struct {
	SessionID       string        `json:"sessionId"`
	Command         string        `json:"command"`
	Args            []string      `json:"args,omitempty"`
	Cwd             string        `json:"cwd,omitempty"`
	Env             []EnvVariable `json:"env,omitempty"`
	OutputByteLimit int           `json:"outputByteLimit,omitempty"`
}

// TerminalCreateResult returns the id used by the other terminal methods.
type TerminalCreateResult struct {
	TerminalID string `json:"terminalId"`
}

// TerminalIDParams addresses one terminal (output / kill / wait / release).
type TerminalIDParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

// TerminalOutputResult is the terminal's captured output so far.
type TerminalOutputResult struct {
	Output     string              `json:"output"`
	Truncated  bool                `json:"truncated"`
	ExitStatus *TerminalExitStatus `json:"exitStatus,omitempty"`
}

// TerminalWaitResult reports how the command exited.
type TerminalWaitResult struct {
	ExitCode *int    `json:"exitCode,omitempty"`
	Signal   *string `json:"signal,omitempty"`
}

// TerminalExitStatus mirrors TerminalWaitResult inside terminal/output.
type TerminalExitStatus struct {
	ExitCode *int    `json:"exitCode,omitempty"`
	Signal   *string `json:"signal,omitempty"`
}

// SessionCancelParams cancels an in-progress turn.
type SessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

// PermissionOptionKind classifies an option for host UI styling. It is an ACP v1
// wire enum, so host-visible permission choices must stay within the official
// protocol values.
type PermissionOptionKind string

// PermissionOption is one choice offered to the user for a permission request.
type PermissionOption struct {
	OptionID string               `json:"optionId"`
	Name     string               `json:"name"`
	Kind     PermissionOptionKind `json:"kind"`
}

// PermissionRequestParams asks the client to approve a pending tool call.
type PermissionRequestParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  PermissionToolCall `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// PermissionToolCall describes the call awaiting approval.
type PermissionToolCall struct {
	ToolCallID string             `json:"toolCallId"`
	Title      string             `json:"title,omitempty"`
	Kind       string             `json:"kind,omitempty"`
	Status     string             `json:"status,omitempty"`
	Content    []toolContent      `json:"content,omitempty"`
	RawInput   json.RawMessage    `json:"rawInput,omitempty"`
	Locations  []ToolCallLocation `json:"locations,omitempty"`
	Meta       map[string]any     `json:"_meta,omitempty"`
}

// PermissionRequestResult is the client's reply to a permission request.
type PermissionRequestResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}

// PermissionOutcome is "selected" (with optionId) or "cancelled".
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}
