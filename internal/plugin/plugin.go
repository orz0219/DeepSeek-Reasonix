// Package plugin is Reasonix's MCP client. It connects to external MCP servers and
// adapts their tools to the tool.Tool interface, so the agent treats plugin
// tools and built-ins uniformly. The wire protocol is JSON-RPC 2.0 in every
// case; only the transport differs (stdio subprocess, Streamable HTTP, or the
// legacy HTTP+SSE). A transport interface hides that difference so the MCP-level
// logic — handshake, tools/list, tools/call — is written once.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"reasonix/internal/mcplaunch"
	"reasonix/internal/sandbox"
)

// protocolVersion is the MCP revision Reasonix advertises during initialize.
const protocolVersion = "2024-11-05"

// MCPProcessMode selects how a local stdio MCP process is launched.
// It is an internal runtime field, not a user-facing config knob.
type MCPProcessMode string

const (
	// MCPProcessHost runs authorized stdio MCP as a trusted host process that
	// does not inherit the agent Bash command sandbox. This is the product
	// default so servers such as chrome-devtools-mcp can reach the real browser,
	// Keychain, LaunchServices, and local app services.
	MCPProcessHost MCPProcessMode = "host"
	// MCPProcessConfined wraps the process with sandbox.CommandArgs. Reserved for
	// internal managed deployments and tests; never auto-selected for user installs.
	MCPProcessConfined MCPProcessMode = "confined"
)

// ResolvedProcessMode returns the effective process mode. Empty means host.
func (s Spec) ResolvedProcessMode() MCPProcessMode {
	switch s.ProcessMode {
	case MCPProcessConfined:
		return MCPProcessConfined
	default:
		return MCPProcessHost
	}
}

// defaultCallTimeout is the MCP JSON-RPC call deadline applied when neither the
// caller context nor config provides one. It is intentionally finite so a slow
// or hung MCP server cannot block an agent turn indefinitely.
const defaultCallTimeout = 300 * time.Second

// Spec declares an external MCP server. Type selects the transport: "stdio"
// (default) runs Command/Args/Env as a subprocess; "http" / "streamable-http"
// and "sse" connect to URL with optional static Headers.
type Spec struct {
	Name string
	// Package is the installed plugin package that contributed this server.
	// It is host-only provenance and intentionally excluded from fingerprints.
	Package string
	Type    string
	Command string
	Args    []string
	Env     map[string]string
	URL     string
	Headers map[string]string
	// DefaultStartupTimeout is the background initialize + tools/list safety cap
	// for this server. Zero keeps Reasonix's built-in default.
	DefaultStartupTimeout time.Duration
	// StartupTimeout overrides DefaultStartupTimeout for this server. It is
	// host-only lifecycle policy and never changes provider-visible tool schemas.
	StartupTimeout time.Duration
	// DefaultCallTimeout is the global MCP call cap for this server. Zero keeps
	// Reasonix's built-in defaultCallTimeout.
	DefaultCallTimeout time.Duration
	// CallTimeout overrides DefaultCallTimeout for all calls to this server.
	// Zero falls back to DefaultCallTimeout.
	CallTimeout time.Duration
	// ToolTimeouts overrides the per-call deadline for raw MCP tool names.
	// Keys are server-local tool names as returned by tools/list, not the
	// model-visible mcp__server__tool names.
	ToolTimeouts map[string]time.Duration
	// Dir, when set, is the working directory of a stdio subprocess. Empty means
	// inherit reasonix's cwd (the default for user-configured plugins). It exists
	// for cwd-aware servers like CodeGraph, which detect the project from the
	// directory they are launched in — they must be pinned to the project root.
	Dir string
	// WorkspaceRoot is the project root exposed through the MCP roots capability.
	// It is runtime-only and intentionally separate from Dir: user-installed
	// stdio servers keep inheriting Reasonix's cwd while still receiving the
	// explicit workspace root when they ask for roots/list.
	WorkspaceRoot string
	// Stderr optionally mirrors plugin subprocess stderr output. Stderr is always
	// captured in a bounded buffer for failure diagnostics; nil keeps it out of
	// the terminal so child logs cannot corrupt interactive UIs.
	Stderr io.Writer
	// LaunchManager owns exact project launch grants and mutable launcher locks.
	// It never contributes to SchemaCacheKey or provider-visible tool schemas.
	LaunchManager *mcplaunch.Manager
	// ConfigSource disambiguates otherwise identical server names coming from
	// workspace config, a host transport, or a user-installed plugin package.
	ConfigSource string
	// Authorized is the single runtime authorization result for this server.
	// User-installed and explicit host-session servers set it directly; project
	// servers set it only after an exact launch grant is resolved.
	Authorized            bool
	RequireLaunchApproval bool
	// LaunchArgs and launcher metadata are host-local immutable resolutions for
	// mutable package launchers. LauncherIdentityArgs is the same exact package
	// resolution without an automatically injected offline/no-install flag: that
	// enforcement-only flag changes process invocation but not the server identity
	// the user approved. These fields never contribute to SchemaCacheKey or the
	// provider-visible tool surface; Args remains the user's stable config.
	LaunchArgs              []string
	LauncherIdentityArgs    []string
	LauncherLocator         string
	LauncherResolvedVersion string
	LauncherDigest          string
	// ProcessMode selects host mode (default) or confined mode, which is reserved
	// for internal managed deployments and tests, never an automatic fallback.
	ProcessMode MCPProcessMode
	// Sandbox is only applied when ProcessMode is confined. Host-mode servers
	// keep private state/cache/temp dirs without wrapping the process in the
	// agent command sandbox.
	Sandbox         sandbox.Spec
	StateDir        string
	OAuthHTTPClient *http.Client
	// StripRawPrefix, when non-empty, removes this prefix from each MCP tool's
	// raw name before namespacing. For example, StripRawPrefix="server_" turns
	// "server_search" into "search", yielding "mcp__search__search" instead of
	// the redundant "mcp__search__server_search". The original raw name is
	// preserved for MCP protocol calls.
	StripRawPrefix string
	// LowPriority runs a stdio subprocess below normal scheduling priority, for
	// background indexers that must not starve the user's machine.
	LowPriority bool
}

// transport carries JSON-RPC messages to and from one MCP server. call sends a
// request and returns its result (correlating by id internally); notify sends a
// fire-and-forget notification; close releases resources. Transports route MCP
// progress notifications to the active tool call and answer the client
// capabilities Reasonix advertises (currently ping and roots/list).
type transport interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
	close()
}

// Host owns the running plugin connections and closes them together. It also
// aggregates the prompts and resources discovered across servers, which the
// chat UI surfaces (prompts as slash commands, resources as @-references).
type Host struct {
	// mu guards the slices below: StartAll builds the Host single-threaded, but
	// after that a /mcp hot-add or -remove (one goroutine) can run concurrently
	// with reads from a running turn's @ref resolution or the status UI.
	mu        sync.RWMutex
	clients   []*Client
	prompts   []Prompt
	resources []Resource
	failures  []Failure
	closed    bool

	// nextInstanceID assigns stable IDs to Client values appended to this Host.
	// nextScopeID assigns IDs to per-build RegistrationScope tokens.
	nextInstanceID atomic.Uint64
	nextScopeID    atomic.Uint64

	// Lazy/background servers may still be handshaking when a session closes.
	// Close cancels those startup contexts and waits for their goroutines before
	// taking the client snapshot, so a just-connected stdio child cannot escape
	// teardown and keep a Windows workspace directory locked.
	deferredCancels     map[string][]context.CancelFunc
	deferredGenerations map[string]uint64
	deferredWG          sync.WaitGroup

	// spawningMu + spawning prevent concurrent spawns of the same server from
	// multiple callers (e.g. several controller tabs sharing one Host). The
	// owner publishes its result before closing done so waiters can reuse the
	// discovered tools without issuing concurrent tools/list calls.
	spawningMu sync.Mutex
	spawning   map[string]*spawnAttempt

	// proxies holds stable per-server backends for rolling replacement without
	// changing provider-visible tool prefixes (spatiotemporal composability).
	proxies map[string]*serverProxy

	// Detached stats/schema-cache writers from Start; off the boot path but
	// drained by Close so cleanup can't race a still-open cache file.
	bgWrites sync.WaitGroup
}

// ReadResource reads a resource uri from the named server. It is how the chat
// UI resolves an @server:uri reference — the uri need not be one listed by
// resources/list (servers may expose templated uris), so we read it directly.
func (h *Host) ReadResource(ctx context.Context, server, uri string) (string, error) {
	h.mu.RLock()
	var target *Client
	for _, c := range h.clients {
		if c.name == server {
			target = c
			break
		}
	}
	h.mu.RUnlock()
	if target == nil {
		return "", fmt.Errorf("no MCP server named %q", server)
	}
	return target.readResource(ctx, uri) // network call: outside the lock
}

// StartPolicy tunes batch plugin startup. The zero value disables every safeguard,
// so most call sites should use the StartAll / StartAvailable wrappers, which
// fill in production defaults.
type StartPolicy struct {
	// PerPluginTimeout caps how long a single plugin's handshake (start +
	// initialize + listTools + listPrompts/Resources) may take. Zero disables.
	// Exceeded plugins are recorded as failures and, when AbortOnError is set,
	// tear down the whole batch with the timeout as the cause.
	PerPluginTimeout time.Duration

	// Concurrency caps how many handshakes run at once. Zero or negative means
	// no cap (every plugin gets a goroutine immediately). A small cap prevents
	// process storms / FD exhaustion when many MCP servers are configured.
	Concurrency int

	// AbortOnError makes any single failure tear down the partial batch and
	// return an error (StartAll semantics). When false, failures are recorded
	// on the host and other plugins keep going (StartAvailable semantics).
	AbortOnError bool

	// SkipPersistence disables RecordStartup / SaveCachedSchema side effects.
	// Use for read-only live probes (capability diagnostics) that must not
	// write MCP stats or schema cache files under Reasonix home.
	SkipPersistence bool
}

// defaultStartConcurrency caps parallel handshakes for the batch-start wrappers.
// Eight is the standard "process storm" guardrail (Bazel's --jobs=auto, most LSP
// managers) — large enough to mask single-plugin latency, small enough to spare
// a workstation with 20+ configured MCP servers from fork-bombing itself.
const defaultStartConcurrency = 8

// defaultStartTimeout is the per-plugin budget used by StartAvailable. Five
// seconds covers a healthy stdio MCP spawning under a slow npm/node loader; past
// that, an interactive user is better served by recording the failure and moving
// on than by stalling the whole session.
const defaultStartTimeout = 5 * time.Second

var advertisedToolsEmptyListRetryDelays = []time.Duration{
	50 * time.Millisecond,
	150 * time.Millisecond,
	300 * time.Millisecond,
}

// ErrServerAlreadyConnected marks an attempted MCP connection whose server name
// is already live on the host.
var ErrServerAlreadyConnected = errors.New("plugin server already connected")

func serverAlreadyConnectedError(name string) error {
	return fmt.Errorf("%w: %q", ErrServerAlreadyConnected, name)
}

// IsServerAlreadyConnected reports whether err means the MCP server name is
// already live on the host.
func IsServerAlreadyConnected(err error) bool {
	return errors.Is(err, ErrServerAlreadyConnected)
}

// StartAll connects every plugin in parallel, performs the MCP handshake, and
// returns the union of their tools (namespaced "mcp__<server>__<tool>"). On any
// failure it tears down everything started so far. The caller must Close the Host.
//
// For stdio plugins, subprocess lifetime is bound to ctx (via
// exec.CommandContext): cancelling ctx kills the children and unblocks reads.

// StartAvailable connects every plugin it can and records failures on the host
// instead of aborting the whole session. The returned tools are the union of the
// successfully connected servers.

// AbortOnError stays false: a misconfigured plugin must not bring down
// the whole session at boot.

// Start is the unified batch-startup primitive behind StartAll / StartAvailable.
// It fans out handshakes in parallel under the policy's concurrency cap, gives
// each plugin its own per-plugin timeout, and either aborts the batch on first
// failure (AbortOnError=true) or records failures on the host and keeps going.
//
// Result ordering matches specs (stable for /mcp status). For stdio plugins the
// subprocess is bound to the parent ctx, not the per-plugin startup timeout:
// successful servers stay alive after startup, while failed/time-limited starts
// are closed explicitly before the goroutine returns.

// A buffered channel acts as a counting semaphore. Capacity 0/negative
// means no cap — we still launch one goroutine per spec, but they all run
// immediately. Capped, the extra goroutines block on the semaphore until a
// slot frees up; collection order is still by idx so /mcp status is stable.

// Created before the fan-out so the detached cache writers can join bgWrites.

// Transport on the parent ctx, startup RPCs on the timed callCtx: the
// per-plugin timeout caps initialize+listTools, but the long-lived
// stdio child must outlive the startup scope and later phase-B calls.

// Persist for next launch on the side: a slow stats/cache write
// must not delay tools coming online, and either failure is
// recoverable (we just re-handshake or skip auto-demote).

// Prompts and resources are deferred to StartPhaseB so the boot path
// can return as soon as tools are ready — the slow-to-list surfaces
// stream in later and fan out an MCPSurfaceReady event each.

// Wait for every goroutine even on abort: started clients sit beyond a
// failing index, so we need them all back to tear them down in Close().

// prompts/resources are filled in later by StartPhaseB.

// Close terminates all plugin connections.

// drain detached stats/schema writers before returning

// queueBackgroundWrite keeps detached persistence inside the Host lifecycle.
// Callers must enqueue before their Close-drained startup owner completes, so
// Close cannot begin waiting before the WaitGroup increment is visible.

// StartPhaseB asynchronously fetches the auxiliary surfaces (prompts and
// resources) for every connected client. Boot calls it right after Start
// returns, on a session-scoped ctx, so the agent becomes responsive as soon as
// tools are ready and the slower list calls stream in afterwards. Each finished
// surface fires an MCPSurfaceReady event on sink so UIs (e.g. /mcp status) can
// refresh without polling. A nil sink is tolerated — the merge still happens.
// Errors are logged and swallowed: prompts/resources are non-essential and must
// not break the session over one slow server.

// Client is one MCP server connection: a name plus the transport carrying its
// JSON-RPC. The MCP-level methods (initialize, listTools, …) are transport-
// agnostic — they go through t.

// Host-local identity for RemoveIfInstance rollback

// registrationClaims and registrationCommitted are guarded by Host.mu.
// Claims keep a tentative shared instance alive across overlapping builds;
// the first published controller promotes it to ordinary Host ownership.

// Capabilities advertised by the server at initialize. prompts/list and
// resources/list are only called when advertised, so we never provoke a
// "method not found" on a tools-only server.

// tools discovered, for /mcp status
// declared transport type, for /mcp status ("stdio"/"http")

// Prompts and resources discovered during StartAll, stored here so the
// parallel startup can collect them per-client before merging into Host.

// toolAdapters caches the model-visible remote tool adapters produced by
// the first successful tools/list call. Shared hosts reuse Client instances
// across controllers, so subsequent ToolsFor calls must not re-query slow
// MCP servers just to rebuild identical schemas.

// ToolInfo is the human-facing metadata returned by MCP tools/list for one tool.

// ServerStatus summarises one connected server for the /mcp command.

// ConfigSource is the config plane that registered this server
// (user_config, project_config, workspace, built-in, …). Empty when unknown.
// Surfaced in /mcp status so operators can tell where a tool came from (#6578).

// AuthorizeSpecLaunch records durable consent for an explicitly user-installed
// project MCP without starting it a second time. The normal project discovery
// path still requires a user action; install_source calls this only while
// applying a plan the user already requested. Reuse an existing launcher lock
// when one exists, but do not add a second network/version-resolution step to an
// explicit install: the durable grant follows the exact configured command or
// endpoint and future changes still invalidate it.

// AuthorizeProjectSpecLaunch records the one durable launch confirmation used
// for repository-discovered MCP configuration. Mutable package launchers are
// resolved and locked, but the MCP server itself is not started: the caller can
// connect it exactly once after this function returns.

// Store the resolution before the grant so a failed state write cannot
// leave an authorization whose exact launcher identity is unavailable.

// Failure records one MCP server that was configured but could not connect.

// Servers returns a status summary per connected server, in connection order.

// RecordFailure stores a failed MCP connection attempt for status UIs.

// RecordLaunchApprovalRequired keeps an intentionally disconnected project MCP
// visible as awaiting authorization. This is used after an explicit launch
// revocation, where no failed connection attempt exists to create the status.

// ClearFailure drops a recorded startup/connection failure for status UIs.

// clearFailure drops the failure record for name. The caller holds h.mu (Lock) —
// it runs inside addConnected / Remove, which already mutate under the lock.

// NewHost returns an empty Host. Boot always constructs one — even with no
// plugins configured — so servers can be hot-added later via Add (the `/mcp add`
// command), which keeps the controller's host pointer stable for the session.

// ErrSpawningInFlight is returned by Host.Add when another caller is already
// spawning the same server on this host. The caller should retry later.

// ConnectionResult is the eventual result of a session-owned background MCP
// handshake. Tools are provider adapters and remain off the caller's registry
// unless the caller explicitly registers them.

// EnsureConnectedInBackground starts or joins one shared initialize +
// tools/list handshake owned by lifeCtx. The returned channel is buffered, so a
// caller may stop waiting while the server continues toward readiness. Host
// shutdown and Remove cancel the background work and wait for its goroutine.

// beginSpawn atomically claims the sole right to spawn the named server.
// Returns owner=true if the caller should proceed. When another caller is
// already spawning the same server, owner=false and done is closed when that
// spawn finishes.

// endSpawn releases the spawn claim for the named server.

// has reports whether a server with this name is already connected.

// HasClient reports whether a server with this name is already connected to the host.

// HasClientForSpec reports whether the shared Host client for spec.Name was
// created from the same runtime connection identity. Server names are only a
// display/routing namespace; they are not sufficient authorization identity
// when controllers with different project configs share one Host.

// ToolsFor returns the namespaced tool instances for an already-connected client.
// ctx bounds the tools/list call so a non-responsive server does not hang
// permanently. An error is returned when no client with that name is connected.

// Attempt to resolve via the existing Client.

// ToolsForSpec is the identity-bound variant used by stable capability
// frontends. It refuses a same-name client from another controller, project
// identity, endpoint, or prior hot-update generation instead of treating that
// client as the current runtime's authorized server.

// MCPRuntimeSpecMatches compares the complete host-local runtime behavior of
// two specs while deliberately excluding non-behavioral handles such as the
// stderr writer and LaunchManager pointer. Secret values are compared only in
// memory and are never serialized into diagnostics or provider-visible state.

// MCPToolMatchesSpec reports whether a concrete plugin adapter or pinned lazy
// placeholder belongs to the requested runtime spec. Unknown tool
// implementations fail closed when a runtime-bound capability frontend asks.

// Add connects one server live: it performs the MCP handshake, discovers the
// server's tools (and prompts/resources when advertised), appends it to the
// host, and returns its namespaced tools for the caller to register. ctx bounds a
// stdio child's lifetime, so pass the session-scoped context — not a per-turn one
// — or the subprocess dies when that turn ends. Errors if the name is taken.

// EnsureConnected returns tools for an already-connected server, or starts the
// shared single-flight handshake and waits for it. Concurrent callers for the
// same server share one initialize/tools-list; cancelling a waiter only cancels
// that wait and never kills a process still used by other runtimes.

// EnsureConnectedWithLifecycle is EnsureConnected with separate subprocess
// lifetime (lifeCtx) and startup/call (callCtx) contexts, plus an optional
// deferred generation for lazy registration.

// AddWithLifecycle connects one server live, allowing caller to specify separate
// contexts for the subprocess lifecycle (lifeCtx, session-scoped) and the startup
// handshake/list calls (callCtx, turn-scoped/timeout-bound).

// Double-check after acquiring the spawn token: another caller may have
// connected the server between our h.has check and beginSpawn.

// Attribute ownership from lifeCtx so LazyToolset background kicks and
// boot.Build share the same RegistrationScope token. Sibling hot-adds
// without a scope are never journaled to a concurrent build.

// Prompts and resources stream in on the long lifeCtx the caller passed (Host.Add
// uses the session-scoped PluginCtx, not a per-turn ctx), so the slow list
// calls cannot starve a /mcp add of its return value. nil sink keeps hot-add
// quiet — the chat UI re-queries Host.Prompts()/Resources() on demand.

// Remove disconnects the named server and drops its prompts/resources, returning
// the namespaced tool-name prefix ("mcp__<server>__") the caller unregisters from
// the tool registry, and whether the server was connected.

// kills the subprocess: outside the lock

// ErrDeferredSpawnCancelled marks a lazy generation invalidated by remove or
// host shutdown before it could publish a client.

// start opens the transport on lifeCtx (whose cancellation later closes the
// subprocess) and uses callCtx for the initialize round-trip (whose cancellation
// only bounds startup RPCs). Splitting the two lets a per-plugin timeout cap
// handshake latency without making the timeout context own a successfully
// registered stdio server; the child also has to outlive phase A so phase B
// (prompts + resources) can still call it later. Callers that don't care pass
// the same ctx for both.

// resolveProjectLaunchAuthorization deliberately skips identity resolution for
// installed and host-session servers. Their explicit installation is already
// the authorization decision; only repository-declared servers need an exact
// executable or endpoint digest before startup.

// A matching exact-identity launch grant is the user's authorization for
// this project server. Calls proceed like an explicit install, while global
// deny rules and execution safety boundaries remain authoritative.

// ResolveStoredAuthorization applies an existing exact project grant without
// starting a process or opening a network connection. Cached lazy/on-demand
// tools use it before strict read-only filtering so every execution path sees
// the same server-level authorization. Errors fail closed by returning the
// original unauthorized Spec; a parent connection surfaces the detailed error.

// ServerAuthorized is the single MCP authorization source. Tools do not carry
// an independent trust bit: installation or an exact project launch grant
// authorizes the server, while read-only/destructive classification remains a
// live per-tool safety fact.

// newTransport builds the transport for a spec's declared type. Empty / unknown
// defaults to stdio.

// Runtime session refresh must not rewrite startup-only capability flags.

// Record which optional capabilities the server advertises. Presence of the
// key (even with an empty object) signals support.

// Annotations carries MCP's optional tool hints. readOnlyHint controls reader
// classification; destructiveHint remains destructive even when another hint
// claims the tool is read-only. Approval policy is applied separately.

// listToolsRawSettled gives dynamically registering servers a bounded startup
// window before their initial tool catalog is considered complete.

// toolName builds Reasonix's canonical model-visible name
// "mcp__<server>__<tool>". The registry separately resolves unique portable
// and Claude plugin-qualified references without exposing duplicate schemas.

// ToolPrefix is the model-visible namespace prefix for every tool from server.

// MCPConnectPermissionName is the canonical permission and hook identity for
// starting server on demand. It is intentionally outside the mcp__ tool
// namespace: permission rules match tool names exactly, so a connect must have
// its own non-colliding name instead of pretending a tool-prefix is a glob.

// ModelToolName is the canonical model-visible name for server's raw tool —
// including the collision-hash suffix normalizeName appends when the raw name
// needed sanitising. Every permission/hook/audit surface that names an MCP
// tool must build the name through this function; a second normalization that
// skips the hash would let deny/ask rules written for the executed name miss.

// JSON-RPC message types (shared by every transport)

// omitted for notifications (id 0 unused)

// remote tool adapter

// namespaced "mcp__<server>__<tool>"
// original name for tools/call
// raw name after configured prefix stripping

// server hint, independent of server authorization
// effective reader classification for this live snapshot
// destructive is the MCP destructiveHint. It takes precedence over a
// conflicting readOnlyHint in Plan and strict read-only execution.

// ReadOnly reflects MCP readOnlyHint plus backward-compatible Spec overrides.
// It defaults to false, so opaque tools remain write-capable unless the server
// or local configuration explicitly classifies them as read-only.

// ExecuteWithImages implements tool.ImageTool: MCP results may carry image
// content items, which callers with a structural image channel (the agent)
// forward to vision models instead of relying on the text placeholders alone.

// Final, linearizable check for a reader-authorized call: the snapshot
// above and every live security reconciliation serialize on the owning
// client's toolsMu. A call approved as a non-destructive reader must never
// execute after authorization or safety metadata changed — state drift
// here returns an actionable error instead of
// dispatching.

// Planner lane: authorized + non-destructive only. Missing readOnlyHint
// is intentional and does not block; destructive promotion or lost
// authorization must produce zero tools/call.

// Tool-result images are forwarded to vision models as base64 data URLs, so
// each item is validated and budgeted here rather than trusted from the MCP
// server: payloads that are oversized, unparseable, beyond the per-result
// count, or of a mime type outside the set every supported vision API accepts
// are replaced with a text placeholder instead of poisoning the provider
// request.
const (
	maxToolResultImageBytes = 4 << 20 // base64 length; stays under provider per-image and request caps
	maxToolResultImages     = 5
)

// parseToolResult flattens an MCP tools/call result into plain text plus the
// image content items as data URLs. Every image item leaves a short placeholder
// in the text at its position, so text-only consumers (and non-vision models)
// still learn an image was returned.

// toolResultImage validates one MCP image content item and returns its text
// placeholder plus the data URL to forward ("" when the item is dropped).

// Some servers wrap base64 in whitespace; vision APIs reject non-canonical
// payloads, so normalize before validating.
