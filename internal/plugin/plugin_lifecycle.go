package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/tool"
)

// Add connects one server live: it performs the MCP handshake, discovers the
// server's tools (and prompts/resources when advertised), appends it to the
// host, and returns its namespaced tools for the caller to register. ctx bounds a
// stdio child's lifetime, so pass the session-scoped context — not a per-turn one
// — or the subprocess dies when that turn ends. Errors if the name is taken.
func (h *Host) Add(ctx context.Context, s Spec) ([]tool.Tool, error) {
	return h.addWithLifecycle(ctx, ctx, s, 0)
}

// EnsureConnected returns tools for an already-connected server, or starts the
// shared single-flight handshake and waits for it. Concurrent callers for the
// same server share one initialize/tools-list; cancelling a waiter only cancels
// that wait and never kills a process still used by other runtimes.
func (h *Host) EnsureConnected(ctx context.Context, s Spec) ([]tool.Tool, error) {
	return h.EnsureConnectedWithLifecycle(ctx, ctx, s, 0)
}

// EnsureConnectedWithLifecycle is EnsureConnected with separate subprocess
// lifetime (lifeCtx) and startup/call (callCtx) contexts, plus an optional
// deferred generation for lazy registration.
func (h *Host) EnsureConnectedWithLifecycle(lifeCtx, callCtx context.Context, s Spec, deferredGeneration uint64) ([]tool.Tool, error) {
	if deferredGeneration != 0 && !h.deferredGenerationCurrent(s.Name, deferredGeneration) {
		return nil, ErrDeferredSpawnCancelled
	}
	if tools, err := h.ToolsFor(callCtx, s.Name); err == nil {
		return tools, nil
	}
	tools, err := h.addWithLifecycle(lifeCtx, callCtx, s, deferredGeneration)
	if IsServerAlreadyConnected(err) {
		return h.ToolsFor(callCtx, s.Name)
	}
	return tools, err
}

// AddWithLifecycle connects one server live, allowing caller to specify separate
// contexts for the subprocess lifecycle (lifeCtx, session-scoped) and the startup
// handshake/list calls (callCtx, turn-scoped/timeout-bound).
func (h *Host) AddWithLifecycle(lifeCtx, callCtx context.Context, s Spec) ([]tool.Tool, error) {
	return h.addWithLifecycle(lifeCtx, callCtx, s, 0)
}

func (h *Host) addWithLifecycle(lifeCtx, callCtx context.Context, s Spec, deferredGeneration uint64) ([]tool.Tool, error) {
	if deferredGeneration != 0 && !h.deferredGenerationCurrent(s.Name, deferredGeneration) {
		return nil, ErrDeferredSpawnCancelled
	}
	if h.has(s.Name) {
		return nil, serverAlreadyConnectedError(s.Name)
	}
	spawnKey := s.Name
	if deferredGeneration != 0 {
		spawnKey = fmt.Sprintf("%s#%d", s.Name, deferredGeneration)
	}
	attempt, owner := h.beginSpawn(spawnKey, s.Name)
	if !owner {
		select {
		case <-attempt.done:
			if attempt.err != nil {
				return nil, attempt.err
			}
			return append([]tool.Tool(nil), attempt.tools...), nil
		case <-callCtx.Done():
			return nil, callCtx.Err()
		case <-lifeCtx.Done():
			return nil, lifeCtx.Err()
		}
	}
	var tools []tool.Tool
	var err error
	defer func() { h.endSpawn(spawnKey, tools, err) }()

	if h.has(s.Name) {
		err = serverAlreadyConnectedError(s.Name)
		return nil, err
	}
	tools, err = h.addConnectedWithLifecycle(lifeCtx, callCtx, s, deferredGeneration)
	return tools, err
}

func (h *Host) addConnectedWithLifecycle(lifeCtx, callCtx context.Context, s Spec, deferredGeneration uint64) ([]tool.Tool, error) {
	startupStarted := time.Now()
	h.mu.RLock()
	if h.closed {
		h.mu.RUnlock()
		return nil, fmt.Errorf("plugin host is closed")
	}
	h.mu.RUnlock()

	c, err := start(lifeCtx, callCtx, s)
	if err != nil {
		return nil, err
	}
	ts, err := c.listTools(callCtx)
	if err != nil {
		c.close()
		err = newStartupFailure("tools/list", startupStarted, c.startupStderr(), err)
		return nil, fmt.Errorf("list tools: %w", err)
	}
	c.toolCount = len(ts)
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		c.close()
		return nil, fmt.Errorf("plugin host is closed")
	}
	if deferredGeneration != 0 && h.deferredGenerations[s.Name] != deferredGeneration {
		h.mu.Unlock()
		c.close()
		return nil, ErrDeferredSpawnCancelled
	}
	if h.hasLocked(s.Name) {
		h.mu.Unlock()
		c.close()
		return nil, serverAlreadyConnectedError(s.Name)
	}

	if err := h.noteClientFromContext(lifeCtx, c); err != nil {
		h.mu.Unlock()
		c.close()
		return nil, err
	}
	h.clearFailure(s.Name)
	h.mu.Unlock()

	if c.hasPrompts {
		go h.fetchPrompts(lifeCtx, c, nil)
	}
	if c.hasResources {
		go h.fetchResources(lifeCtx, c, nil)
	}
	return ts, nil
}

// Remove disconnects the named server and drops its prompts/resources, returning
// the namespaced tool-name prefix ("mcp__<server>__") the caller unregisters from
// the tool registry, and whether the server was connected.
func (h *Host) Remove(name string) (toolPrefix string, found bool) {
	h.mu.Lock()
	cancels := append([]context.CancelFunc(nil), h.deferredCancels[name]...)
	delete(h.deferredCancels, name)
	if h.deferredGenerations == nil {
		h.deferredGenerations = make(map[string]uint64)
	}
	h.deferredGenerations[name]++
	if h.deferredGenerations[name] == 0 {
		h.deferredGenerations[name] = 1
	}
	idx := -1
	for i, c := range h.clients {
		if c.name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		h.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		if len(cancels) == 0 {
			return "", false
		}
		return ToolPrefix(name), true
	}
	removed := h.removeClientAtLocked(idx)
	h.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	removed.close()

	return "mcp__" + normalizeName(name) + "__", true
}

func (h *Host) deferredGenerationCurrent(name string, generation uint64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return !h.closed && generation != 0 && h.deferredGenerations[name] == generation
}

// ErrDeferredSpawnCancelled marks a lazy generation invalidated by remove or
// host shutdown before it could publish a client.
var ErrDeferredSpawnCancelled = errors.New("deferred MCP spawn cancelled")

// start opens the transport on lifeCtx (whose cancellation later closes the
// subprocess) and uses callCtx for the initialize round-trip (whose cancellation
// only bounds startup RPCs). Splitting the two lets a per-plugin timeout cap
// handshake latency without making the timeout context own a successfully
// registered stdio server; the child also has to outlive phase A so phase B
// (prompts + resources) can still call it later. Callers that don't care pass
// the same ctx for both.
func start(lifeCtx, callCtx context.Context, s Spec) (*Client, error) {
	started := time.Now()
	var err error
	s, err = applyStoredLauncherLock(s)
	if err != nil {
		return nil, newStartupFailure("launch", started, "", err)
	}
	s, err = resolveProjectLaunchAuthorization(callCtx, s)
	if err != nil {
		return nil, newStartupFailure("authorization", started, "", err)
	}
	t, err := newTransport(lifeCtx, s)
	if err != nil {
		return nil, newStartupFailure("launch", started, "", err)
	}
	tt := strings.ToLower(strings.TrimSpace(s.Type))
	if tt == "" {
		tt = "stdio"
	}
	c := &Client{name: s.Name, t: t, spec: s, transport: tt}
	if err := c.initialize(callCtx); err != nil {
		c.close()
		err = newStartupFailure("initialize", started, c.startupStderr(), err)
		return nil, err
	}
	return c, nil
}

// resolveProjectLaunchAuthorization deliberately skips identity resolution for
// installed and host-session servers. Their explicit installation is already
// the authorization decision; only repository-declared servers need an exact
// executable or endpoint digest before startup.
func resolveProjectLaunchAuthorization(ctx context.Context, s Spec) (Spec, error) {
	if !s.RequireLaunchApproval {
		return s, nil
	}
	identityDigest, err := projectLaunchIdentityDigest(ctx, s)
	if err != nil {
		return s, err
	}
	return applyEstablishedLaunchGrant(s, identityDigest)
}

func applyEstablishedLaunchGrant(s Spec, identityDigest string) (Spec, error) {
	if !s.RequireLaunchApproval {
		return s, nil
	}
	if s.LaunchManager == nil {
		return s, fmt.Errorf("MCP launch authorization store is unavailable")
	}
	authorized, changed, err := s.LaunchManager.LaunchAuthorized(s.Name, launchConfigSource(s), identityDigest)
	if err != nil {
		return s, err
	}
	if !authorized {
		return s, &launchApprovalError{server: s.Name, changed: changed}
	}

	s.Authorized = true
	return s, nil
}

// ResolveStoredAuthorization applies an existing exact project grant without
// starting a process or opening a network connection. Cached lazy/on-demand
// tools use it before strict read-only filtering so every execution path sees
// the same server-level authorization. Errors fail closed by returning the
// original unauthorized Spec; a parent connection surfaces the detailed error.
func ResolveStoredAuthorization(ctx context.Context, s Spec) Spec {
	if !s.RequireLaunchApproval {
		return s
	}
	locked, err := applyStoredLauncherLock(s)
	if err != nil {
		return s
	}
	authorized, err := resolveProjectLaunchAuthorization(ctx, locked)
	if err != nil {
		return s
	}
	return authorized
}

// ServerAuthorized is the single MCP authorization source. Tools do not carry
// an independent trust bit: installation or an exact project launch grant
// authorizes the server, while read-only/destructive classification remains a
// live per-tool safety fact.
func (s Spec) ServerAuthorized() bool {
	return s.Authorized
}

// newTransport builds the transport for a spec's declared type. Empty / unknown
// defaults to stdio.
func newTransport(ctx context.Context, s Spec) (transport, error) {
	switch strings.ToLower(strings.TrimSpace(s.Type)) {
	case "", "stdio":
		return newStdioTransport(ctx, s)
	case "http", "streamable-http", "streamable_http":
		return newHTTPTransport(s)
	case "sse":
		return newSSETransport(ctx, s)
	default:
		return nil, fmt.Errorf("unknown transport type %q (want stdio|http|sse)", s.Type)
	}
}
