// Package acp implements the Agent Client Protocol (https://agentclientprotocol.com)
// transport: a stdio JSON-RPC 2.0 agent that editors and other host clients speak
// to drive Reasonix. Many tools integrated with the v1 (main-branch) agent over
// ACP, so v2 keeps the wire contract identical — the wire types in this file are a
// faithful port of main's src/acp/protocol.ts (ACP protocol version 1).
//
// The package is an adapter layer over the v2 kernel and depends only on stable
// contracts: it maps the agent's typed event.Event stream onto session/update
// notifications (see dispatch.go), bridges permission.Approver onto
// session/request_permission round-trips (see permission.go), and exposes the
// whole thing over NDJSON JSON-RPC (see server.go). How a per-session agent is
// actually assembled — provider, tools rooted at the session cwd, per-session MCP
// — is left to a Factory the composition root supplies (see service.go), so this
// package stays independent of the cli wiring.
package acp

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ProtocolVersion is the ACP version this agent implements. Matches main.
const ProtocolVersion = 1

// JSON-RPC 2.0 error codes (subset used on the wire). Mirrors protocol.ts.
const (
	ErrParse          = -32700
	ErrInvalidRequest = -32600
	ErrMethodNotFound = -32601
	ErrInvalidParams  = -32602
	ErrInternal       = -32603
)

// initialize

// InitializeParams is the client's handshake. The agent records the client's
// capabilities — fs read/write proxying and host terminals are used when
// offered — and advertises its own fixed capability set in reply.
type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities,omitempty"`
}

// ClientCapabilities is what the client offers the agent: filesystem proxy
// methods (fs/read_text_file, fs/write_text_file) that see unsaved editor
// buffers, and host-owned terminals (terminal/*). Meta carries vendor
// capability blocks (e.g. _meta["reasonix.io"]) for tolerant parse — unknown
// or malformed entries simply mean the vendor feature stays off.
type ClientCapabilities struct {
	FS       FSCapabilities `json:"fs,omitempty"`
	Terminal bool           `json:"terminal,omitempty"`
	Meta     map[string]any `json:"_meta,omitempty"`
}

// FSCapabilities reports which client filesystem methods are available.
type FSCapabilities struct {
	ReadTextFile  bool `json:"readTextFile,omitempty"`
	WriteTextFile bool `json:"writeTextFile,omitempty"`
}

// Implementation names a participant (client or agent) on the wire.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

// InitializeResult advertises what this agent supports: persisted session load,
// ACP v1 session lifecycle helpers, inline resource text (embeddedContext) but
// not image/audio, and stdio / Streamable HTTP MCP (no legacy sse).
type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AgentInfo         Implementation    `json:"agentInfo"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
}

// AgentCapabilities is the agentCapabilities object in InitializeResult.
type AgentCapabilities struct {
	LoadSession         bool                `json:"loadSession"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities,omitempty"`
	PromptCapabilities  PromptCapabilities  `json:"promptCapabilities"`
	MCPCapabilities     MCPCapabilities     `json:"mcpCapabilities"`
	Meta                map[string]any      `json:"_meta,omitempty"`
}

// ReasonixExtensionCapabilities advertises Reasonix-specific ACP extensions.
// ACP v1 reserves agentCapabilities._meta for vendor capability discovery.
type ReasonixExtensionCapabilities struct {
	SessionSteer *SessionSteerCapability `json:"sessionSteer,omitempty"`
	// SessionInbox advertises the durable session-level instruction queue.
	SessionInbox *SessionInboxCapability `json:"sessionInbox,omitempty"`
	// SessionReloadExtensions advertises the vendor runtime-reload method.
	SessionReloadExtensions *SessionReloadExtensionsCapability `json:"sessionReloadExtensions,omitempty"`
	// ExtensionSurface advertises structured extension-UI surface support:
	// the agent publishes surfaces as vendor session/update payloads.
	ExtensionSurface *ExtensionSurfaceCapability `json:"extensionSurface,omitempty"`
}

// SessionSteerCapability identifies the vendor-namespaced steering method.
type SessionSteerCapability struct {
	Method string `json:"method"`
}

// SessionInboxCapability advertises durable inbox methods (schemaVersion 1).
type SessionInboxCapability struct {
	SchemaVersion int               `json:"schemaVersion"`
	Methods       map[string]string `json:"methods"`
}

// SessionReloadExtensionsCapability identifies the vendor-namespaced runtime
// reload method.
type SessionReloadExtensionsCapability struct {
	Method string `json:"method"`
}

const (
	// reasonixExtensionSurfaceSchemaVersion versions the extension-surface DTO
	// carried by the vendor session/update variant.
	reasonixExtensionSurfaceSchemaVersion = 1
	// extensionSurfaceUpdateKind discriminates the vendor session/update
	// variant that carries a structured extension-UI surface.
	extensionSurfaceUpdateKind = "_reasonix.io/extension_surface"
)

// ExtensionSurfaceCapability advertises that a participant renders structured
// extension-UI surfaces (Extension Protocol v2) natively.
type ExtensionSurfaceCapability struct {
	Supported     bool `json:"supported"`
	SchemaVersion int  `json:"schemaVersion"`
}

// EmptyCapability serializes to {} for ACP capability flags.
type EmptyCapability struct{}

// SessionCapabilities advertises optional session lifecycle methods.
type SessionCapabilities struct {
	List   *EmptyCapability `json:"list,omitempty"`
	Resume *EmptyCapability `json:"resume,omitempty"`
	Close  *EmptyCapability `json:"close,omitempty"`
	Delete *EmptyCapability `json:"delete,omitempty"`
}

// PromptCapabilities reports which content-block kinds prompts may carry.
type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

// MCPCapabilities reports which MCP transports session/new accepts.
type MCPCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

// AuthMethod advertises how a client can prepare credentials for the agent.
type AuthMethod struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Type        string            `json:"type,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
}

// AuthenticateParams selects one advertised auth method. Terminal methods are
// normally handled by the client by launching the agent with the method's args;
// accepting this request keeps clients that call authenticate directly working.
type AuthenticateParams struct {
	MethodID string `json:"methodId"`
}

// AuthenticateResult is the empty authentication ack.
type AuthenticateResult struct{}

// session/new

// SessionNewParams opens a session rooted at cwd, optionally with MCP servers
// the agent should connect for the session's lifetime.
type SessionNewParams struct {
	Cwd        string          `json:"cwd,omitempty"`
	MCPServers []MCPServerSpec `json:"mcpServers,omitempty"`
}

// MCPServerSpec describes one MCP server the client asks the agent to connect.
type MCPServerSpec struct {
	Name    string     `json:"name"`
	Type    string     `json:"type,omitempty"`
	Command string     `json:"command,omitempty"`
	Args    []string   `json:"args,omitempty"`
	Env     MCPEnv     `json:"env,omitempty"`
	URL     string     `json:"url,omitempty"`
	Headers MCPHeaders `json:"headers,omitempty"`
}

// MCPEnv accepts ACP's official EnvVariable[] shape while still accepting the
// older map shape that Reasonix v1 clients used.
type MCPEnv map[string]string

// MCPHeaders accepts ACP's official HTTPHeader[] shape while still accepting
// the older map shape that Reasonix v1 clients used. The official spec
// (https://agentclientprotocol.com) ships HTTP/SSE MCP headers as an array of
// {name,value} objects, even when empty.
type MCPHeaders map[string]string

// EnvVariable is one official ACP MCP environment variable entry. The same
// {name,value} shape is also used by HTTP/SSE headers in the ACP spec, so we
// reuse it as the parse target for [MCPHeaders] too.
type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (e *MCPEnv) UnmarshalJSON(raw []byte) error {
	out, err := unmarshalNameValueMap(raw, "env")
	if err != nil {
		return err
	}
	*e = out
	return nil
}

func (h *MCPHeaders) UnmarshalJSON(raw []byte) error {
	out, err := unmarshalNameValueMap(raw, "headers")
	if err != nil {
		return err
	}
	*h = out
	return nil
}

// unmarshalNameValueMap parses ACP's official [{name,value}, ...] array shape
// or the legacy {name: value, ...} map shape into a map. field names the JSON
// field for error messages.
func unmarshalNameValueMap(raw []byte, field string) (map[string]string, error) {
	if s := strings.TrimSpace(string(raw)); s == "" || s == "null" {
		return nil, nil
	}

	var vars []EnvVariable
	if err := json.Unmarshal(raw, &vars); err == nil {
		out := make(map[string]string, len(vars))
		for i, v := range vars {
			if strings.TrimSpace(v.Name) == "" {
				return nil, fmt.Errorf("%s[%d].name is required", field, i)
			}
			out[v.Name] = v.Value
		}
		return out, nil
	}

	var legacy map[string]string
	if err := json.Unmarshal(raw, &legacy); err == nil {
		return legacy, nil
	}
	return nil, fmt.Errorf("%s must be an array of {name,value} objects", field)
}

// SessionNewResult returns the opaque id used to address the session thereafter.
type SessionNewResult struct {
	SessionID     string                `json:"sessionId"`
	Models        *SessionModelState    `json:"models,omitempty"`
	Modes         *SessionModeState     `json:"modes,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`
}

// session modes

// SessionMode is one operating mode the client can switch the session into.
type SessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// SessionModeState reports the current mode and the full mode list.
type SessionModeState struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

// SessionSetModeParams switches a session's operating mode.
type SessionSetModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

// SessionSetModeResult is the empty ack.
type SessionSetModeResult struct{}

// ModelInfo describes one selectable model in ACP's legacy model selector.
type ModelInfo struct {
	ModelID     string `json:"modelId"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// SessionModelState is ACP's legacy model selector state. New clients should
// prefer the category:"model" config option, but some hosts still probe this.
type SessionModelState struct {
	AvailableModels []ModelInfo `json:"availableModels"`
	CurrentModelID  string      `json:"currentModelId"`
}

// session/load

// SessionLoadParams resumes a session saved under sessionId (the id a prior
// session/new returned), optionally re-rooting it at cwd with fresh MCP servers.
// The agent replays the stored conversation as session/update notifications
// before the request returns.
type SessionLoadParams struct {
	SessionID  string          `json:"sessionId"`
	Cwd        string          `json:"cwd,omitempty"`
	MCPServers []MCPServerSpec `json:"mcpServers,omitempty"`
}

// SessionLoadResult is the empty ack; the conversation has already arrived as a
// burst of session/update notifications by the time it is sent.
type SessionLoadResult struct {
	Models        *SessionModelState    `json:"models,omitempty"`
	Modes         *SessionModeState     `json:"modes,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`
}

// session/resume

// SessionResumeParams resumes a session without replaying its transcript.
type SessionResumeParams struct {
	SessionID  string          `json:"sessionId"`
	Cwd        string          `json:"cwd,omitempty"`
	MCPServers []MCPServerSpec `json:"mcpServers,omitempty"`
}

// SessionResumeResult is the empty ack returned once the session is ready.
type SessionResumeResult struct {
	Models        *SessionModelState    `json:"models,omitempty"`
	Modes         *SessionModeState     `json:"modes,omitempty"`
	ConfigOptions []SessionConfigOption `json:"configOptions,omitempty"`
}

// session/set_config_option

// SetSessionConfigOptionParams changes one advertised session config option.
type SetSessionConfigOptionParams struct {
	SessionID string `json:"sessionId"`
	ConfigID  string `json:"configId"`
	Value     string `json:"value"`
}

// SetSessionConfigOptionResult returns the full refreshed config state.
type SetSessionConfigOptionResult struct {
	ConfigOptions []SessionConfigOption `json:"configOptions"`
}

// SessionConfigOption is a single-value ACP session selector.
type SessionConfigOption struct {
	ID           string                      `json:"id"`
	Name         string                      `json:"name"`
	Description  string                      `json:"description,omitempty"`
	Category     string                      `json:"category,omitempty"`
	Type         string                      `json:"type"`
	CurrentValue string                      `json:"currentValue"`
	Options      []SessionConfigSelectOption `json:"options"`
}

// SessionConfigSelectOption is one selectable value for a config option.
type SessionConfigSelectOption struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// session/set_model

// SetSessionModelParams is ACP's legacy model-switching request.
type SetSessionModelParams struct {
	SessionID string `json:"sessionId"`
	ModelID   string `json:"modelId"`
}

// SetSessionModelResult is the empty ack for legacy model switching.
type SetSessionModelResult struct{}

// session/list

// SessionListParams lists known sessions, optionally filtered by cwd.
type SessionListParams struct {
	Cwd    string `json:"cwd,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

// SessionListResult is the first and only page of sessions Reasonix currently
// returns. NextCursor is omitted because the in-process list is unpaged.
type SessionListResult struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

// SessionInfo is the ACP session/list item shape.
type SessionInfo struct {
	SessionID string         `json:"sessionId"`
	Cwd       string         `json:"cwd"`
	Title     string         `json:"title,omitempty"`
	UpdatedAt string         `json:"updatedAt,omitempty"`
	Meta      map[string]any `json:"_meta,omitempty"`
}

// session/close

// SessionCloseParams closes an active session and releases its resources.
type SessionCloseParams struct {
	SessionID string `json:"sessionId"`
}

// SessionCloseResult is the empty close ack.
type SessionCloseResult struct{}

// session/delete

// SessionDeleteParams removes a session from future session/list results.
type SessionDeleteParams struct {
	SessionID string `json:"sessionId"`
}

// SessionDeleteResult is the empty delete ack.
type SessionDeleteResult struct{}

// content blocks (inbound prompt)

// ContentBlock is one piece of a prompt. The agent reads text blocks and the
// inline text of resource blocks (embeddedContext); image/audio are accepted on
// the wire but ignored, matching the advertised capabilities.

// ResourceContents is the embedded resource of a "resource" content block.

// FlattenPrompt extracts the user-visible prompt text out of ACP content blocks.
// Text blocks contribute their text; resource blocks contribute their inline
// text when present (embeddedContext). Other block kinds are dropped. Ported from
// protocol.ts flattenPrompt.

// session/prompt

// SessionPromptParams sends a turn's prompt to a session.

// SessionSteerParams is the Reasonix ACP v1 extension for injecting user
// guidance into an active prompt without cancelling it.

// SessionSteerResult acknowledges durable steer admission.

// sessionSteerMethod follows ACP v1's reserved vendor-extension namespace.

const (
	sessionInboxSchemaVersion = 1
	sessionInboxEnqueueMethod = "_reasonix.io/session/inbox/enqueue"
	sessionInboxListMethod    = "_reasonix.io/session/inbox/list"
	sessionInboxGetMethod     = "_reasonix.io/session/inbox/get"
	sessionInboxUpdateMethod  = "_reasonix.io/session/inbox/update"
	sessionInboxDeleteMethod  = "_reasonix.io/session/inbox/delete"
	sessionInboxMoveMethod    = "_reasonix.io/session/inbox/move"
	sessionInboxPauseMethod   = "_reasonix.io/session/inbox/setPaused"
	sessionInboxRetryMethod   = "_reasonix.io/session/inbox/retry"
	sessionInboxRefreshMethod = "_reasonix.io/session/inbox/refresh"
)

// SessionInboxEnqueueParams is the durable inbox enqueue request.

// followup | steer

// SessionInboxItemParams identifies one inbox item.

// SessionInboxUpdateParams rewrites an item body.

// SessionInboxMoveParams reorders an item (toIndex is 0-based).

// SessionInboxPauseParams toggles pause.

// SessionReloadExtensionsParams addresses one live ACP session.

// SessionReloadExtensionsResult reports whether the runtime reload ran
// immediately (Queued false) or was coalesced behind a turn/rebuild in flight
// to run when the session goes idle (Queued true).

// sessionReloadExtensionsMethod follows ACP v1's reserved vendor-extension
// namespace, like sessionSteerMethod: only the "_<vendor>/" prefix is reserved
// for vendor methods, so the bare "reasonix/session/reloadExtensions" form
// could collide with a future official ACP method and must not be used.

// StopReason tells the client why a turn ended. Values match main's wire.

const (
	StopEndTurn   StopReason = "end_turn"
	StopCancelled StopReason = "cancelled"
	StopError     StopReason = "error"
)

// SessionPromptResult ends a session/prompt. TranscriptPath is reserved for a
// future on-disk transcript pointer; omitted (null) for now.

// session/update (agent → client notifications)
//
// SessionUpdate is a tagged union discriminated by sessionUpdate. The variants
// reuse the JSON key "content" with two incompatible shapes (a single block for
// message chunks, an array for tool results), so we model each variant as its own
// struct rather than one struct with conflicting tags, and carry it through
// SessionUpdateParams.Update as an interface value.

// SessionUpdateParams wraps one update for a session.

// messageChunk is agent_message_chunk / agent_thought_chunk.

// extensionSurfaceUpdate is the vendor session/update variant that carries one
// structured extension-UI surface to clients that negotiated
// reasonix.extensionSurface in initialize. ACP has no standard notification for
// extension surfaces, so the DTO (the shared eventwire JSON contract) rides
// _meta["reasonix.io"]["extensionSurface"], mirroring how the initialize
// handshake namespaces vendor data under "reasonix.io". The sink always pairs
// it with a flattened agent_message_chunk text fallback (belt and suspenders):
// a client that ignores the vendor variant still shows the content.

// updateMeta carries optional error detail on an agent_message_chunk.

// toolCall is a "tool_call" update (announces a call, with title/kind/rawInput).

// ToolCallLocation names a file (and optionally a line) a tool call touches, so
// the client can follow along in the editor.

// toolCallUpdateMsg is a "tool_call_update" update (status + result content).

// toolContent wraps a tool result's text, per the ACP tool_call_update shape.

// availableCommandsUpdate advertises slash commands that the ACP client may
// surface in its composer. The client sends invocations back as normal
// session/prompt text such as "/review diff".

// AvailableCommand is one slash command available in a session.

// AvailableCommandInput describes a command's free-form text argument.

// configOptionUpdate reports a complete refreshed session config state.

// planUpdate is a "plan" update: the agent's current task list. Each update
// carries the complete plan and replaces the previous one, mirroring the
// todo_write contract it is derived from.

// PlanEntry is one task in a plan update.

// currentModeUpdate reports that the session switched operating modes.

// fs/* (agent → client requests)

// FSReadTextFileParams asks the client for a file's current text, including
// unsaved editor state. Line (1-based) and Limit page the content; Reasonix
// always reads whole files and pages locally, so it sends neither.

// FSReadTextFileResult carries the file content.

// FSWriteTextFileParams asks the client to write content to path, updating any
// open buffer as well as the file on disk.

// terminal/* (agent → client requests)

// TerminalCreateParams starts a command in a client-owned terminal.
// Env follows ACP v1's official EnvVariable[] shape (same as MCP env): only
// the overrides Reasonix owns (typically TMPDIR/TMP/TEMP) are sent — never a
// full host environment dump.

// TerminalCreateResult returns the id used by the other terminal methods.

// TerminalIDParams addresses one terminal (output / kill / wait / release).

// TerminalOutputResult is the terminal's captured output so far.

// TerminalWaitResult reports how the command exited.

// TerminalExitStatus mirrors TerminalWaitResult inside terminal/output.

// session/cancel (client → agent notification)

// SessionCancelParams cancels an in-progress turn.

// session/request_permission (agent → client request)

// PermissionOptionKind classifies an option for host UI styling. It is an ACP v1
// wire enum, so host-visible permission choices must stay within the official
// protocol values.

const (
	OptAllowOnce    PermissionOptionKind = "allow_once"
	OptAllowAlways  PermissionOptionKind = "allow_always"
	OptRejectOnce   PermissionOptionKind = "reject_once"
	OptRejectAlways PermissionOptionKind = "reject_always"
)

// PermissionOption is one choice offered to the user for a permission request.

// PermissionRequestParams asks the client to approve a pending tool call.

// PermissionToolCall describes the call awaiting approval.

// PermissionRequestResult is the client's reply to a permission request.

// PermissionOutcome is "selected" (with optionId) or "cancelled".
