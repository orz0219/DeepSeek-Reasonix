package acp

import (
	"encoding/json"
	"strings"
)

// ContentBlock is one piece of a prompt. The agent reads text blocks and the
// inline text of resource blocks (embeddedContext); image/audio are accepted on
// the wire but ignored, matching the advertised capabilities.
type ContentBlock struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Resource *ResourceContents `json:"resource,omitempty"`
	MimeType string            `json:"mimeType,omitempty"`
	Data     string            `json:"data,omitempty"`
}

// ResourceContents is the embedded resource of a "resource" content block.
type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
}

// FlattenPrompt extracts the user-visible prompt text out of ACP content blocks.
// Text blocks contribute their text; resource blocks contribute their inline
// text when present (embeddedContext). Other block kinds are dropped. Ported from
// protocol.ts flattenPrompt.
func FlattenPrompt(blocks []ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "resource":
			if b.Resource != nil && b.Resource.Text != "" {
				parts = append(parts, b.Resource.Text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// SessionPromptParams sends a turn's prompt to a session.
type SessionPromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// SessionSteerParams is the Reasonix ACP v1 extension for injecting user
// guidance into an active prompt without cancelling it.
type SessionSteerParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// SessionSteerResult acknowledges durable steer admission.
type SessionSteerResult struct {
	ItemID      string `json:"itemId,omitempty"`
	Disposition string `json:"disposition,omitempty"`
}

// sessionSteerMethod follows ACP v1's reserved vendor-extension namespace.
const sessionSteerMethod = "_reasonix.io/session/steer"

// SessionInboxEnqueueParams is the durable inbox enqueue request.
type SessionInboxEnqueueParams struct {
	SessionID      string `json:"sessionId"`
	Text           string `json:"text"`
	Intent         string `json:"intent,omitempty"` // followup | steer
	IdempotencyKey string `json:"idempotencyKey,omitempty"`
}

// SessionInboxItemParams identifies one inbox item.
type SessionInboxItemParams struct {
	SessionID string `json:"sessionId"`
	ItemID    string `json:"itemId"`
}

// SessionInboxUpdateParams rewrites an item body.
type SessionInboxUpdateParams struct {
	SessionID string `json:"sessionId"`
	ItemID    string `json:"itemId"`
	Text      string `json:"text"`
}

// SessionInboxMoveParams reorders an item (toIndex is 0-based).
type SessionInboxMoveParams struct {
	SessionID string `json:"sessionId"`
	ItemID    string `json:"itemId"`
	ToIndex   int    `json:"toIndex"`
}

// SessionInboxPauseParams toggles pause.
type SessionInboxPauseParams struct {
	SessionID string `json:"sessionId"`
	Paused    bool   `json:"paused"`
}

// SessionReloadExtensionsParams addresses one live ACP session.
type SessionReloadExtensionsParams struct {
	SessionID string `json:"sessionId"`
}

// SessionReloadExtensionsResult reports whether the runtime reload ran
// immediately (Queued false) or was coalesced behind a turn/rebuild in flight
// to run when the session goes idle (Queued true).
type SessionReloadExtensionsResult struct {
	Queued bool `json:"queued,omitempty"`
}

// sessionReloadExtensionsMethod follows ACP v1's reserved vendor-extension
// namespace, like sessionSteerMethod: only the "_<vendor>/" prefix is reserved
// for vendor methods, so the bare "reasonix/session/reloadExtensions" form
// could collide with a future official ACP method and must not be used.
const sessionReloadExtensionsMethod = "_reasonix.io/session/reloadExtensions"

// StopReason tells the client why a turn ended. Values match main's wire.
type StopReason string

// SessionPromptResult ends a session/prompt. TranscriptPath is reserved for a
// future on-disk transcript pointer; omitted (null) for now.
type SessionPromptResult struct {
	StopReason     StopReason `json:"stopReason"`
	TranscriptPath *string    `json:"transcriptPath,omitempty"`
}

// SessionUpdateParams wraps one update for a session.
type SessionUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

// messageChunk is agent_message_chunk / agent_thought_chunk.
type messageChunk struct {
	SessionUpdate string       `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
	Metadata      *updateMeta  `json:"metadata,omitempty"`
}

// extensionSurfaceUpdate is the vendor session/update variant that carries one
// structured extension-UI surface to clients that negotiated
// reasonix.extensionSurface in initialize. ACP has no standard notification for
// extension surfaces, so the DTO (the shared eventwire JSON contract) rides
// _meta["reasonix.io"]["extensionSurface"], mirroring how the initialize
// handshake namespaces vendor data under "reasonix.io". The sink always pairs
// it with a flattened agent_message_chunk text fallback (belt and suspenders):
// a client that ignores the vendor variant still shows the content.
type extensionSurfaceUpdate struct {
	SessionUpdate string         `json:"sessionUpdate"`
	Meta          map[string]any `json:"_meta"`
}

// updateMeta carries optional error detail on an agent_message_chunk.
type updateMeta struct {
	Error *updateError `json:"error,omitempty"`
}

type updateError struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

// toolCall is a "tool_call" update (announces a call, with title/kind/rawInput).
type toolCall struct {
	SessionUpdate string             `json:"sessionUpdate"`
	ToolCallID    string             `json:"toolCallId"`
	Title         string             `json:"title,omitempty"`
	Kind          string             `json:"kind,omitempty"`
	Status        string             `json:"status,omitempty"`
	RawInput      json.RawMessage    `json:"rawInput,omitempty"`
	Locations     []ToolCallLocation `json:"locations,omitempty"`
}

// ToolCallLocation names a file (and optionally a line) a tool call touches, so
// the client can follow along in the editor.
type ToolCallLocation struct {
	Path string `json:"path"`
	Line *int   `json:"line,omitempty"`
}

// toolCallUpdateMsg is a "tool_call_update" update (status + result content).
type toolCallUpdateMsg struct {
	SessionUpdate string        `json:"sessionUpdate"`
	ToolCallID    string        `json:"toolCallId"`
	Status        string        `json:"status,omitempty"`
	Content       []toolContent `json:"content,omitempty"`
}

// toolContent wraps a tool result's text, per the ACP tool_call_update shape.
type toolContent struct {
	Type    string       `json:"type"`
	Content ContentBlock `json:"content"`
}

// availableCommandsUpdate advertises slash commands that the ACP client may
// surface in its composer. The client sends invocations back as normal
// session/prompt text such as "/review diff".
type availableCommandsUpdate struct {
	SessionUpdate     string             `json:"sessionUpdate"`
	AvailableCommands []AvailableCommand `json:"availableCommands"`
}

// AvailableCommand is one slash command available in a session.
type AvailableCommand struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Input       *AvailableCommandInput `json:"input,omitempty"`
}

// AvailableCommandInput describes a command's free-form text argument.
type AvailableCommandInput struct {
	Hint string `json:"hint"`
}

// configOptionUpdate reports a complete refreshed session config state.
type configOptionUpdate struct {
	SessionUpdate string                `json:"sessionUpdate"`
	ConfigOptions []SessionConfigOption `json:"configOptions"`
}

// planUpdate is a "plan" update: the agent's current task list. Each update
// carries the complete plan and replaces the previous one, mirroring the
// todo_write contract it is derived from.
type planUpdate struct {
	SessionUpdate string      `json:"sessionUpdate"`
	Entries       []PlanEntry `json:"entries"`
}

// PlanEntry is one task in a plan update.
type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

// currentModeUpdate reports that the session switched operating modes.
type currentModeUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	CurrentModeID string `json:"currentModeId"`
}
