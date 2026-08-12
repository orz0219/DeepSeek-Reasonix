package plugin

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
)

// Servers returns a status summary per connected server, in connection order.
func (h *Host) Servers() []ServerStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]ServerStatus, 0, len(h.clients))
	for _, c := range h.clients {
		s := ServerStatus{
			Name:         c.name,
			Transport:    c.transport,
			ConfigSource: strings.TrimSpace(c.spec.ConfigSource),
			Tools:        c.toolCount,
			HasTools:     c.hasTools,
		}
		c.toolsMu.Lock()
		s.ToolList = append([]ToolInfo(nil), c.tools...)
		c.toolsMu.Unlock()
		for _, p := range h.prompts {
			if p.Server == c.name {
				s.Prompts++
			}
		}
		for _, r := range h.resources {
			if r.Server == c.name {
				s.Resources++
			}
		}
		out = append(out, s)
	}
	return out
}

// RecordFailure stores a failed MCP connection attempt for status UIs.
func (h *Host) RecordFailure(s Spec, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	tt := strings.ToLower(strings.TrimSpace(s.Type))
	if tt == "" {
		tt = "stdio"
	}
	stage, elapsed, stderr := startupFailureDetails(err)
	f := Failure{
		Name: s.Name, Transport: tt, Error: summarizeFailureError(err),
		Stage: stage, Elapsed: elapsed, Stderr: stderr,
		RequiresLaunchApproval: requiresLaunchApproval(err),
	}
	for i := range h.failures {
		if h.failures[i].Name == s.Name {
			h.failures[i] = f
			return
		}
	}
	h.failures = append(h.failures, f)
}

// RecordLaunchApprovalRequired keeps an intentionally disconnected project MCP
// visible as awaiting authorization. This is used after an explicit launch
// revocation, where no failed connection attempt exists to create the status.
func (h *Host) RecordLaunchApprovalRequired(s Spec) {
	h.RecordFailure(s, &launchApprovalError{server: s.Name})
}

// ClearFailure drops a recorded startup/connection failure for status UIs.
func (h *Host) ClearFailure(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clearFailure(name)
}

// clearFailure drops the failure record for name. The caller holds h.mu (Lock) —
// it runs inside addConnected / Remove, which already mutate under the lock.
func (h *Host) clearFailure(name string) {
	kept := h.failures[:0]
	for _, f := range h.failures {
		if f.Name != name {
			kept = append(kept, f)
		}
	}
	h.failures = kept
}

// NewHost returns an empty Host. Boot always constructs one — even with no
// plugins configured — so servers can be hot-added later via Add (the `/mcp add`
// command), which keeps the controller's host pointer stable for the session.
func NewHost() *Host { return &Host{} }

func (h *Host) registerDeferredCancel(name string, cancel context.CancelFunc) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		cancel()
		return 0
	}
	if h.deferredCancels == nil {
		h.deferredCancels = make(map[string][]context.CancelFunc)
	}
	if h.deferredGenerations == nil {
		h.deferredGenerations = make(map[string]uint64)
	}
	generation := h.deferredGenerations[name]
	if generation == 0 {
		generation = 1
		h.deferredGenerations[name] = generation
	}
	h.deferredCancels[name] = append(h.deferredCancels[name], cancel)
	return generation
}

func (h *Host) beginDeferredSpawn() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	h.deferredWG.Add(1)
	return true
}

func (h *Host) endDeferredSpawn() {
	h.deferredWG.Done()
}

// ErrSpawningInFlight is returned by Host.Add when another caller is already
// spawning the same server on this host. The caller should retry later.
var ErrSpawningInFlight = errors.New("server spawn already in progress")

type spawnAttempt struct {
	server string
	done   chan struct{}
	tools  []tool.Tool
	err    error
}

// ConnectionResult is the eventual result of a session-owned background MCP
// handshake. Tools are provider adapters and remain off the caller's registry
// unless the caller explicitly registers them.
type ConnectionResult struct {
	Tools []tool.Tool
	Err   error
}

// EnsureConnectedInBackground starts or joins one shared initialize +
// tools/list handshake owned by lifeCtx. The returned channel is buffered, so a
// caller may stop waiting while the server continues toward readiness. Host
// shutdown and Remove cancel the background work and wait for its goroutine.
func (h *Host) EnsureConnectedInBackground(lifeCtx context.Context, s Spec) <-chan ConnectionResult {
	result := make(chan ConnectionResult, 1)
	startupBase, cancelStartupBase := context.WithCancel(lifeCtx)
	generation := h.registerDeferredCancel(s.Name, cancelStartupBase)
	if !h.beginDeferredSpawn() {
		cancelStartupBase()
		result <- ConnectionResult{Err: fmt.Errorf("plugin host is closed")}
		return result
	}
	go func() {
		defer h.endDeferredSpawn()
		defer cancelStartupBase()
		started := time.Now()
		startupCtx, cancelStartup := context.WithTimeout(startupBase, s.startupTimeout())
		tools, err := h.EnsureConnectedWithLifecycle(lifeCtx, startupCtx, s, generation)
		cancelStartup()
		if err != nil {
			err = newStartupFailure("connect", started, "", err)
			if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrDeferredSpawnCancelled) {
				h.RecordFailure(s, err)
			}
		}
		result <- ConnectionResult{Tools: tools, Err: err}
	}()
	return result
}

// beginSpawn atomically claims the sole right to spawn the named server.
// Returns owner=true if the caller should proceed. When another caller is
// already spawning the same server, owner=false and done is closed when that
// spawn finishes.
func (h *Host) beginSpawn(key, server string) (*spawnAttempt, bool) {
	h.spawningMu.Lock()
	defer h.spawningMu.Unlock()
	if h.spawning == nil {
		h.spawning = make(map[string]*spawnAttempt)
	}
	if attempt, ok := h.spawning[key]; ok {
		return attempt, false
	}
	attempt := &spawnAttempt{server: server, done: make(chan struct{})}
	h.spawning[key] = attempt
	return attempt, true
}

// endSpawn releases the spawn claim for the named server.
func (h *Host) endSpawn(name string, tools []tool.Tool, err error) {
	h.spawningMu.Lock()
	if attempt, ok := h.spawning[name]; ok {
		attempt.tools = append([]tool.Tool(nil), tools...)
		attempt.err = err
		delete(h.spawning, name)
		close(attempt.done)
	}
	h.spawningMu.Unlock()
}

// has reports whether a server with this name is already connected.
func (h *Host) has(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.hasLocked(name)
}

func (h *Host) hasLocked(name string) bool {
	for _, c := range h.clients {
		if c.name == name {
			return true
		}
	}
	return false
}

// HasClient reports whether a server with this name is already connected to the host.
func (h *Host) HasClient(name string) bool { return h.has(name) }

// HasClientForSpec reports whether the shared Host client for spec.Name was
// created from the same runtime connection identity. Server names are only a
// display/routing namespace; they are not sufficient authorization identity
// when controllers with different project configs share one Host.
func (h *Host) HasClientForSpec(spec Spec) bool {
	c := h.client(spec.Name)
	return c != nil && MCPRuntimeSpecMatches(c.spec, spec)
}

// ToolsFor returns the namespaced tool instances for an already-connected client.
// ctx bounds the tools/list call so a non-responsive server does not hang
// permanently. An error is returned when no client with that name is connected.
func (h *Host) ToolsFor(ctx context.Context, name string) ([]tool.Tool, error) {
	h.mu.RLock()
	closed := h.closed
	h.mu.RUnlock()
	if closed {
		return nil, fmt.Errorf("plugin host is closed")
	}

	c := h.client(name)
	if c == nil {
		return nil, fmt.Errorf("client %q not found on shared host", name)
	}
	if err := h.claimClientFromContext(ctx, c); err != nil {
		return nil, err
	}
	if tools, ok := c.cachedTools(); ok {
		return tools, nil
	}
	return c.listTools(ctx)
}

// ToolsForSpec is the identity-bound variant used by stable capability
// frontends. It refuses a same-name client from another controller, project
// identity, endpoint, or prior hot-update generation instead of treating that
// client as the current runtime's authorized server.
func (h *Host) ToolsForSpec(ctx context.Context, spec Spec) ([]tool.Tool, error) {
	h.mu.RLock()
	closed := h.closed
	h.mu.RUnlock()
	if closed {
		return nil, fmt.Errorf("plugin host is closed")
	}
	c := h.client(spec.Name)
	if c == nil {
		return nil, fmt.Errorf("client %q not found on shared host", spec.Name)
	}
	if !MCPRuntimeSpecMatches(c.spec, spec) {
		return nil, fmt.Errorf("connected MCP server %q identity does not match the current runtime configuration", spec.Name)
	}
	if err := h.claimClientFromContext(ctx, c); err != nil {
		return nil, err
	}
	if tools, ok := c.cachedTools(); ok {
		return tools, nil
	}
	return c.listTools(ctx)
}

// MCPRuntimeSpecMatches compares the complete host-local runtime behavior of
// two specs while deliberately excluding non-behavioral handles such as the
// stderr writer and LaunchManager pointer. Secret values are compared only in
// memory and are never serialized into diagnostics or provider-visible state.
func MCPRuntimeSpecMatches(a, b Spec) bool {
	return reflect.DeepEqual(mcpRuntimeSpecIdentityOf(a), mcpRuntimeSpecIdentityOf(b))
}

// MCPToolMatchesSpec reports whether a concrete plugin adapter or pinned lazy
// placeholder belongs to the requested runtime spec. Unknown tool
// implementations fail closed when a runtime-bound capability frontend asks.
func MCPToolMatchesSpec(t tool.Tool, spec Spec) bool {
	switch typed := t.(type) {
	case *remoteTool:
		return typed != nil && typed.client != nil && MCPRuntimeSpecMatches(typed.client.spec, spec)
	case *lazyTool:
		return typed != nil && typed.shared != nil && MCPRuntimeSpecMatches(typed.shared.spec, spec)
	default:
		return false
	}
}

type mcpRuntimeSpecIdentity struct {
	Name                    string
	Package                 string
	Type                    string
	Command                 string
	Args                    []string
	Env                     map[string]string
	URL                     string
	Headers                 map[string]string
	DefaultStartupTimeout   time.Duration
	StartupTimeout          time.Duration
	DefaultCallTimeout      time.Duration
	CallTimeout             time.Duration
	ToolTimeouts            map[string]time.Duration
	Dir                     string
	WorkspaceRoot           string
	LaunchWorkspace         string
	ConfigSource            string
	RequireLaunchApproval   bool
	LaunchArgs              []string
	LauncherIdentityArgs    []string
	LauncherLocator         string
	LauncherResolvedVersion string
	LauncherDigest          string
	ProcessMode             MCPProcessMode
	Sandbox                 sandbox.Spec
	StateDir                string
	StripRawPrefix          string
	LowPriority             bool
}

func mcpRuntimeSpecIdentityOf(s Spec) mcpRuntimeSpecIdentity {
	launchWorkspace := ""
	if s.LaunchManager != nil {
		launchWorkspace = s.LaunchManager.WorkspaceFingerprint()
	}
	return mcpRuntimeSpecIdentity{
		Name:                    strings.TrimSpace(s.Name),
		Package:                 strings.TrimSpace(s.Package),
		Type:                    canonicalMCPRuntimeTransport(s.Type),
		Command:                 s.Command,
		Args:                    nonEmptyStrings(s.Args),
		Env:                     nonEmptyStringMap(s.Env),
		URL:                     s.URL,
		Headers:                 nonEmptyStringMap(s.Headers),
		DefaultStartupTimeout:   s.DefaultStartupTimeout,
		StartupTimeout:          s.StartupTimeout,
		DefaultCallTimeout:      s.DefaultCallTimeout,
		CallTimeout:             s.CallTimeout,
		ToolTimeouts:            nonEmptyDurationMap(s.ToolTimeouts),
		Dir:                     s.Dir,
		WorkspaceRoot:           s.WorkspaceRoot,
		LaunchWorkspace:         launchWorkspace,
		ConfigSource:            strings.TrimSpace(s.ConfigSource),
		RequireLaunchApproval:   s.RequireLaunchApproval,
		LaunchArgs:              nonEmptyStrings(s.LaunchArgs),
		LauncherIdentityArgs:    nonEmptyStrings(s.LauncherIdentityArgs),
		LauncherLocator:         s.LauncherLocator,
		LauncherResolvedVersion: s.LauncherResolvedVersion,
		LauncherDigest:          s.LauncherDigest,
		ProcessMode:             s.ResolvedProcessMode(),
		Sandbox:                 canonicalMCPRuntimeSandbox(s.Sandbox),
		StateDir:                s.StateDir,
		StripRawPrefix:          s.StripRawPrefix,
		LowPriority:             s.LowPriority,
	}
}

func canonicalMCPRuntimeTransport(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "stdio":
		return "stdio"
	case "http", "streamable-http", "streamable_http":
		return "streamable-http"
	case "sse":
		return "sse"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func canonicalMCPRuntimeSandbox(in sandbox.Spec) sandbox.Spec {
	in.WriteRoots = nonEmptyStrings(in.WriteRoots)
	in.ReadRoots = nonEmptyStrings(in.ReadRoots)
	in.AppContainerWriteRoots = nonEmptyStrings(in.AppContainerWriteRoots)
	in.ForbidReadRoots = nonEmptyStrings(in.ForbidReadRoots)
	return in
}

func nonEmptyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return in
}

func nonEmptyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	return in
}

func nonEmptyDurationMap(in map[string]time.Duration) map[string]time.Duration {
	if len(in) == 0 {
		return nil
	}
	return in
}

func (h *Host) client(name string) *Client { return h.lookupClient(name) }
