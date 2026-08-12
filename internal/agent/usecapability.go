package agent

import (
	"context"
	"encoding/json"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"reasonix/internal/capability"
	"reasonix/internal/config"
	"reasonix/internal/plugin"
	"reasonix/internal/tool"
)

// MCPCapabilityRuntime is the session-shared MCP substrate: Host, boot specs,
// provider-visible registry for already-registered tools, schema cache catalog,
// and live connection snapshots. Each agent (executor, planner, task/fleet
// child) gets its own UseCapabilityTool frontend so ledger/audit never cross
// agent boundaries, while process connections remain on the shared Host.
type MCPCapabilityRuntime struct {
	lifeCtx  context.Context
	host     *plugin.Host
	registry *tool.Registry
	catalog  func() capability.Catalog

	// dispatchMu linearizes server enable/spec mutations against MCP process
	// startup and tools/call. Calls may run concurrently under RLock; a disable,
	// uninstall, or hot update waits for in-flight dispatch and invalidates every
	// target that has not begun its final runtime-bound execution check.
	dispatchMu sync.RWMutex
	mu         sync.RWMutex
	servers    map[string]mcpRuntimeServer
	gates      mcpServerGates
	// shared connection observation across all frontends on this session.
	state *mcpProxySharedState
}

type mcpRuntimeServer struct {
	entry      config.PluginEntry
	spec       plugin.Spec
	enabled    bool
	cached     []plugin.CachedTool
	cacheKeyOK bool
}

type mcpProxySharedState struct {
	mu        sync.Mutex
	connected map[string]bool
	liveTools map[string][]plugin.CachedTool
}

// NewMCPCapabilityRuntime builds the session-shared MCP substrate. lifeCtx owns
// on-demand MCP child process lifetimes; specs must be the boot-converted specs.
func NewMCPCapabilityRuntime(lifeCtx context.Context, host *plugin.Host, specs []plugin.Spec, reg *tool.Registry, catalog func() capability.Catalog) *MCPCapabilityRuntime {
	r := &MCPCapabilityRuntime{
		lifeCtx:  lifeCtx,
		host:     host,
		registry: reg,
		catalog:  catalog,
		servers:  map[string]mcpRuntimeServer{},
		state:    &mcpProxySharedState{connected: map[string]bool{}},
	}
	r.ConfigureServers(nil, specs, nil)
	return r
}

// ConfigureServers replaces the runtime's configured MCP inventory. enabled is
// keyed by server name; nil keeps the standalone/test default that every spec is
// enabled. Boot passes the activation-resolved set so disabled servers are
// visible to discovery but cannot reuse a sibling tab's shared Host client.
func (r *MCPCapabilityRuntime) ConfigureServers(entries []config.PluginEntry, specs []plugin.Spec, enabled map[string]bool) {
	if r == nil {
		return
	}
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	byName := make(map[string]config.PluginEntry, len(entries))
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name != "" {
			byName[name] = runtimePluginEntry(entry)
		}
	}
	next := make(map[string]mcpRuntimeServer, len(specs))
	for _, raw := range specs {
		spec := cloneMCPSpec(raw)
		name := strings.TrimSpace(spec.Name)
		if name == "" {
			continue
		}
		entry, ok := byName[name]
		if !ok {
			entry = config.PluginEntry{Name: name}
		}
		isEnabled := true
		if enabled != nil {
			isEnabled = enabled[name]
		}
		cached, keyOK := cachedToolsForSpec(spec)
		next[name] = mcpRuntimeServer{
			entry:      entry,
			spec:       spec,
			enabled:    isEnabled,
			cached:     cached,
			cacheKeyOK: keyOK,
		}
	}
	r.mu.Lock()
	r.servers = next
	r.mu.Unlock()
	for name := range next {
		if !next[name].enabled {
			r.state.clearServer(name)
		}
	}
}

// UpsertServer makes a hot-added or updated MCP spec authoritative for every
// frontend on this controller. Dynamic state stays host-local and never changes
// the provider-visible use_capability schema.
func (r *MCPCapabilityRuntime) UpsertServer(entry config.PluginEntry, raw plugin.Spec, enabled bool) {
	if r == nil {
		return
	}
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	spec := cloneMCPSpec(raw)
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return
	}
	entry = runtimePluginEntry(entry)
	if strings.TrimSpace(entry.Name) == "" {
		entry.Name = name
	}
	cached, keyOK := cachedToolsForSpec(spec)
	r.mu.Lock()
	r.servers[name] = mcpRuntimeServer{
		entry:      entry,
		spec:       spec,
		enabled:    enabled,
		cached:     cached,
		cacheKeyOK: keyOK,
	}
	r.mu.Unlock()
	// Endpoint/tool metadata may have changed. Never route a stale live snapshot
	// across an update; a connected client or the next call will repopulate it.
	r.state.clearServer(name)
}

// SetServerEnabled revokes or restores this controller's right to use a server.
// It is intentionally independent from Host connectivity because desktop tabs
// may share one Host while keeping different enable states.
func (r *MCPCapabilityRuntime) SetServerEnabled(name string, enabled bool) bool {
	if r == nil {
		return false
	}
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	name = strings.TrimSpace(name)
	r.mu.Lock()
	server, ok := r.servers[name]
	if ok {
		server.enabled = enabled
		r.servers[name] = server
	}
	r.mu.Unlock()
	if ok && !enabled {
		r.state.clearServer(name)
	}
	return ok
}

// RemoveServer removes an uninstalled/runtime-only MCP from discovery and
// clears any live tool snapshot that could otherwise keep it routable.
func (r *MCPCapabilityRuntime) RemoveServer(name string) bool {
	if r == nil {
		return false
	}
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	name = strings.TrimSpace(name)
	r.mu.Lock()
	_, ok := r.servers[name]
	delete(r.servers, name)
	r.mu.Unlock()
	r.state.clearServer(name)
	return ok
}

// CatalogState returns deterministic, privacy-minimal routing inputs for this
// controller. Configuration secrets are never copied into the transient route.
func (r *MCPCapabilityRuntime) CatalogState() (entries []config.PluginEntry, cached map[string][]plugin.CachedTool, keyOK map[string]bool, disabled map[string]bool) {
	if r == nil {
		return nil, nil, nil, nil
	}
	r.dispatchMu.RLock()
	defer r.dispatchMu.RUnlock()
	return r.catalogStateLocked()
}

// CapabilityCatalogState returns configuration and live proxy tools from one
// lifecycle generation. Callers must use this combined snapshot when building
// a route: taking the two halves separately can otherwise pair a just-updated
// spec with a stale pre-update live-tool directory.
func (r *MCPCapabilityRuntime) CapabilityCatalogState() (entries []config.PluginEntry, cached map[string][]plugin.CachedTool, keyOK map[string]bool, disabled map[string]bool, proxyTools map[string][]plugin.CachedTool) {
	if r == nil {
		return nil, nil, nil, nil, nil
	}
	r.dispatchMu.RLock()
	defer r.dispatchMu.RUnlock()
	entries, cached, keyOK, disabled = r.catalogStateLocked()
	proxyTools = r.connectedProxyToolsLocked()
	return entries, cached, keyOK, disabled, proxyTools
}

func (r *MCPCapabilityRuntime) catalogStateLocked() (entries []config.PluginEntry, cached map[string][]plugin.CachedTool, keyOK map[string]bool, disabled map[string]bool) {
	r.mu.RLock()
	names := make([]string, 0, len(r.servers))
	for name := range r.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	entries = make([]config.PluginEntry, 0, len(names))
	cached = make(map[string][]plugin.CachedTool, len(names))
	keyOK = make(map[string]bool, len(names))
	disabled = make(map[string]bool)
	for _, name := range names {
		server := r.servers[name]
		entries = append(entries, runtimePluginEntry(server.entry))
		if len(server.cached) > 0 {
			cached[name] = cloneCachedTools(server.cached)
			keyOK[name] = server.cacheKeyOK
		}
		if !server.enabled {
			disabled[name] = true
		}
	}
	r.mu.RUnlock()
	if len(cached) == 0 {
		cached = nil
		keyOK = nil
	}
	if len(disabled) == 0 {
		disabled = nil
	}
	return entries, cached, keyOK, disabled
}

func (r *MCPCapabilityRuntime) enabledSpec(server string) (plugin.Spec, bool) {
	if r == nil {
		return plugin.Spec{}, false
	}
	r.mu.RLock()
	configured, ok := r.servers[strings.TrimSpace(server)]
	r.mu.RUnlock()
	if !ok || !configured.enabled {
		return plugin.Spec{}, false
	}
	return cloneMCPSpec(configured.spec), true
}

func cachedToolsForSpec(spec plugin.Spec) ([]plugin.CachedTool, bool) {
	cached, keyOK := capability.LoadCachedToolsForSpecs([]plugin.Spec{spec})
	return cloneCachedTools(cached[spec.Name]), keyOK[spec.Name]
}

func runtimePluginEntry(entry config.PluginEntry) config.PluginEntry {
	out := config.PluginEntry{Name: strings.TrimSpace(entry.Name), Source: entry.Source}
	if entry.AutoStart != nil {
		value := *entry.AutoStart
		out.AutoStart = &value
	}
	return out
}

func cloneCachedTools(in []plugin.CachedTool) []plugin.CachedTool {
	if len(in) == 0 {
		return nil
	}
	out := make([]plugin.CachedTool, len(in))
	copy(out, in)
	for i := range out {
		out[i].Schema = append(json.RawMessage(nil), in[i].Schema...)
	}
	return out
}

func cloneMCPSpec(in plugin.Spec) plugin.Spec {
	out := in
	out.Args = append([]string(nil), in.Args...)
	out.LaunchArgs = append([]string(nil), in.LaunchArgs...)
	out.LauncherIdentityArgs = append([]string(nil), in.LauncherIdentityArgs...)
	out.Env = cloneStringMap(in.Env)
	out.Headers = cloneStringMap(in.Headers)
	if in.ToolTimeouts != nil {
		out.ToolTimeouts = make(map[string]time.Duration, len(in.ToolTimeouts))
		maps.Copy(out.ToolTimeouts, in.ToolTimeouts)
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

// NewFrontend returns a per-agent use_capability instance. ledger/audit may be
// nil for ordinary sub-agents that do not run Delivery capability gates.
func (r *MCPCapabilityRuntime) NewFrontend(ledger *capability.Ledger, audit *capability.Audit) *UseCapabilityTool {
	if r == nil {
		return NewUseCapabilityTool(context.Background(), nil, nil, nil, ledger, audit, nil)
	}
	return &UseCapabilityTool{
		host:     r.host,
		lifeCtx:  r.lifeCtx,
		runtime:  r,
		registry: r.registry,
		ledger:   ledger,
		audit:    audit,
		catalog:  r.catalog,
		state:    r.state,
	}
}

// ConnectedProxyTools returns live tool metadata for servers connected through
// any frontend on this runtime, keyed by server name.

func (r *MCPCapabilityRuntime) connectedProxyToolsLocked() map[string][]plugin.CachedTool {
	live := r.state.snapshotLiveTools()
	if len(live) == 0 {
		return nil
	}
	r.mu.RLock()
	for name := range live {
		server, ok := r.servers[name]
		if !ok || !server.enabled {
			delete(live, name)
		}
	}
	r.mu.RUnlock()
	if len(live) == 0 {
		return nil
	}
	return live
}

// UseCapabilityTool is the stable MCP capability proxy for Delivery, the
// two-model Planner, and task/fleet sub-agents. It lists, inspects, calls, or
// declines catalog capabilities without adding dynamic MCP tools to the
// provider-visible registry — subsequent calls keep using this stable schema.
// Multiple frontends may share one MCPCapabilityRuntime (Host + connection
// state) while keeping independent ledger/audit.

// lifeCtx is the session-scoped context that owns on-demand MCP child
// processes (mirrors lazySpawn.ctx): a proxied server must outlive the tool
// call that started it and die with the session, not with a resolve-phase
// timeout. nil falls back to context.Background() for direct/test use.

// specs are the boot-converted plugin specs (env expansion, workspace
// overrides and timeouts). The proxy never rebuilds
// specs from raw config entries — that would fork the conversion logic.

// live registry for already-exposed MCP tools

// state is session-shared connection observation when built via
// MCPCapabilityRuntime; nil falls back to a private map for tests.

// runtimeBoundMCPTool keeps the provider-visible MCP adapter unchanged while
// binding execution to the current controller runtime. The underlying Host may
// be shared by sibling tabs, so a server name alone must never authorize reuse.

// NewUseCapabilityTool builds a standalone capability proxy (tests and simple
// boots). Prefer MCPCapabilityRuntime.NewFrontend when multiple agents share
// one session Host.

// CloneForAgent returns a new frontend sharing Host/specs/connection state but
// with independent ledger and audit (nil unless provided).

// Stable schema — must not change across turns or when MCP connects.
// capability_id is optional only for action=list; inspect/call/decline still
// require it at resolve time. One intentional prefix upgrade; thereafter
// install/connect churn does not change this schema.

// ResolveCall implements tool.CallResolver so the agent can run permission,
// hooks, and evidence against the real MCP target before execution.

// Decline must not skip require. The mutation itself is delayed until the
// agent has applied its post-resolution host boundary.

// listServerInfo is one configured MCP server entry returned by action=list.
// It never starts a server or opens a network connection.

// listCapabilities returns the unified catalog summary: MCP servers plus
// non-provider-visible tools and skills available through this proxy. The
// top-level "servers" key stays compatible with restricted subagent list
// filtering.

// Skip provider-visible core tools — they are already top-level.

// listServers returns sorted configured MCP server names, status, and
// capability IDs without starting servers. Used by Planner discovery when no
// specific capability route was provided.

// Apply stored project grants without process/network side effects so
// list status matches resolve/execute authorization.

// For MCP entries, list tools without side effects: live tools when the
// server is already connected, cached schema otherwise. Inspect runs
// during call resolution — before permission and hook gates — so it must
// never start a subprocess or open a network connection.

// serverTools refreshes the snapshot too: inspecting a
// server another tab connected restores tool routing here.

// filterInspectTools narrows concrete mcp-tool inspection to that exact tool.
// Server inspection intentionally keeps the full directory. This prevents a
// restricted sub-agent allowed one tool from discovering sibling tool schemas
// through action=inspect on its allowed capability ID.

// inspectToolListJSON renders a server's live tools as the capability-id
// directory shared by inspect and the first-discovery connect result.

// Registry-backed tools and skills share the unified proxy. Real writers
// still pass permission/plan/sandbox/lease checks via ResolvedCall.Target.

// Server-level call is the first-discovery path for servers with no
// schema cache: it resolves to a gated connect-and-list target so the
// model can learn tool names without inspect ever starting a process.

// Prefer already-exposed registry tool (auto-started MCP). The model name
// MUST come from the plugin layer's canonical constructor: it appends a
// collision hash for sanitised raw names, and permission/hook rules are
// written against that executed name — a proxy-local normalization would
// let them silently miss.

// Server already connected (auto-started, a previous proxy call, or a
// sibling tab sharing this host): resolving against live tools is
// side-effect-free. serverTools also refreshes the catalog snapshot so a
// cross-tab connect still yields routable mcp-tool entries here.

// Unconnected server: resolution must stay pure — no subprocess, no network.
// Return a deferred target that connects in Execute, after the permission
// gate and PreToolUse hooks have approved the real target name/arguments.

// Cached server hints control ordinary approval. Strict read-only execution
// additionally requires server authorization and live read-only metadata.

// resolveRegistryTool binds a registry tool by name for use_capability call.
// Provider-visible core tools remain callable this way, but the catalog prefers
// listing only non-visible tools so the model uses the top-level surface first.

// resolveSkillCall routes skill:<name> through run_skill / read_only_skill /
// read_skill when present, preserving the real skill tool name for evidence.

// Prefer full run_skill; fall back to read-only variants when the session
// only exposes them (planner / plan mode).

// resolveUnavailable fills the host-proven unavailable shape shared by the
// side-effect-free resolution failures (missing config, unknown tool).

// findMCPTool matches a server's tool list by raw MCP name or by the
// canonical namespaced model-visible name (plugin.ModelToolName).

// onDemandMCPTool defers MCP server startup to Execute so permission and hook
// gates always run before any subprocess or network side effect. Before the live
// handshake it remains write-capable until the resolved MCP tool is classified.

// destructive comes from the schema cache when available. A live promotion
// is detected in Execute so a retry re-enters the current Plan/read-only
// execution boundary.

// Spec.Authorized is the single runtime authorization result. Boot/install
// and ResolveStoredAuthorization set it; this path never invents trust.

// MCPServerName/MCPRawToolName expose the deferred target for audit and
// diagnostics (tool.MCPMetadata).

// ExecuteWithImages preserves structured MCP image results on the first call,
// when the deferred target must connect the server before dispatch. Keeping the
// resolution and safety checks in executeWithImages ensures text-only and image
// callers share the same authorization and runtime-identity boundary.

// Final runtime-bound authorization and identity check before any
// process/network start. The read lock also linearizes this dispatch against
// disable, uninstall, and same-name hot replacement.

// Audit for the call path is recorded once by the agent loop
// (noteCapabilityInvocation); only the ledger outcome lands here.

// Planner non-destructive lane and reader lane: re-check live metadata
// before tools/call even when Reconcile did not see a cache promotion.

// Reuse shared host if already connected (including auto-started).

// On-demand connect: the child and handshake belong to the session. This
// tool call waits briefly, but a slow healthy server continues in the
// background instead of being killed and restarted on every retry. Tools
// stay off the main provider-visible registry.

// Intentionally do NOT add tools to t.registry — provider schema stays stable.

// serverTools fetches the live tools for a connected server and refreshes the
// shared catalog snapshot so mcp-tool entries stay routable once the server
// is StatusReady (its tools are absent from the provider-visible registry).

// ConnectedProxyTools returns raw tool metadata for servers connected through
// any frontend sharing this proxy state, keyed by server name. Catalog builders
// consume it so concrete mcp-tool capabilities survive an on-demand connect
// without ever touching the provider-visible registry.

// specFor looks up the boot-converted spec for server. The proxy deliberately
// holds []plugin.Spec, not raw config entries: env expansion, workspace
// overrides, call timeouts, and read-only tool names all live in the
// shared conversion and must not be re-derived here.

// lockAuthorizedRuntimeServer acquires the shared runtime dispatch read lock
// and returns the current enabled, authorized spec. The caller must keep the
// returned lock until the identity-bound Host operation or tools/call has
// crossed its dispatch boundary; lifecycle mutations take the write lock.

// Standalone proxies predate the authoritative runtime and may resolve
// already-registered MCP tools without carrying a duplicate spec slice.

// parseMCPServerCapabilityID extracts the server name from an mcp-server id.

// resolveServerConnect resolves action=call on an mcp-server id. A connected
// server lists its tools immediately (side-effect free); an unconnected one
// resolves to a deferred connect target that runs only after the permission
// gate and PreToolUse hooks approve it. Stored project authorization is applied
// at resolve time so unauthorized project MCP never reaches process startup.

// A dedicated exact identity names the connect for permission and hook
// rules. It cannot collide with a real mcp__ tool, and rules do not need to
// rely on unsupported tool-name glob matching.

// Connecting spawns a subprocess, so it is never a read-only fast path for
// ordinary Plan/strict agents. PlannerMCPExecution may allow authorized
// connects; unauthorized specs are blocked before process/network start.

// onDemandMCPConnect is the deferred first-discovery target: it connects the
// server post-approval and returns the live tool directory.

// MCPLifecycleConnect marks this target as an MCP connect-and-list lifecycle
// action for Planner authorization (not a remote tools/call).

// Zero process/network start when authorization, enable state, or exact
// runtime identity changed after resolve.

// listServerTools renders the live tool directory of a connected server and
// refreshes the proxy snapshot on the way (via serverTools).

// Ensure UseCapabilityTool satisfies the tool contracts used by the agent.
var (
	_ tool.Tool         = (*UseCapabilityTool)(nil)
	_ tool.CallResolver = (*UseCapabilityTool)(nil)
)

// EmitProxyAudit is a helper for frontends: returns a notice describing the
// proxy name and real target for user audit trails.
