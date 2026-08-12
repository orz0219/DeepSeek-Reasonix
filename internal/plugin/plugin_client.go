package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"reasonix/internal/mcplaunch"
	"reasonix/internal/tool"
)

// Client is one MCP server connection: a name plus the transport carrying its
// JSON-RPC. The MCP-level methods (initialize, listTools, …) are transport-
// agnostic — they go through t.
type Client struct {
	name       string
	instanceID uint64 // Host-local identity for RemoveIfInstance rollback
	t          transport
	spec       Spec

	// registrationClaims and registrationCommitted are guarded by Host.mu.
	// Claims keep a tentative shared instance alive across overlapping builds;
	// the first published controller promotes it to ordinary Host ownership.
	registrationClaims    map[uint64]struct{}
	registrationCommitted bool

	// Capabilities advertised by the server at initialize. prompts/list and
	// resources/list are only called when advertised, so we never provoke a
	// "method not found" on a tools-only server.
	hasTools     bool
	hasPrompts   bool
	hasResources bool

	toolCount int    // tools discovered, for /mcp status
	transport string // declared transport type, for /mcp status ("stdio"/"http")

	// Prompts and resources discovered during StartAll, stored here so the
	// parallel startup can collect them per-client before merging into Host.
	prompts   []Prompt
	resources []Resource
	toolsMu   sync.Mutex
	tools     []ToolInfo

	// toolAdapters caches the model-visible remote tool adapters produced by
	// the first successful tools/list call. Shared hosts reuse Client instances
	// across controllers, so subsequent ToolsFor calls must not re-query slow
	// MCP servers just to rebuild identical schemas.
	toolsListed  bool
	toolAdapters []tool.Tool
	progressID   atomic.Uint64
}

func (c *Client) auxiliaryClient(ctx context.Context) (*Client, context.Context, context.CancelFunc, error) {
	auxCtx, cancel := context.WithTimeout(ctx, defaultStartTimeout)
	aux, err := start(auxCtx, auxCtx, c.spec)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return aux, auxCtx, cancel, nil
}

// ToolInfo is the human-facing metadata returned by MCP tools/list for one tool.
type ToolInfo struct {
	Name            string
	Description     string
	ReadOnlyHint    bool
	DestructiveHint bool
	SchemaError     string
}

// ServerStatus summarises one connected server for the /mcp command.
type ServerStatus struct {
	Name      string
	Transport string
	// ConfigSource is the config plane that registered this server
	// (user_config, project_config, workspace, built-in, …). Empty when unknown.
	// Surfaced in /mcp status so operators can tell where a tool came from (#6578).
	ConfigSource string
	Tools        int
	Prompts      int
	Resources    int
	HasTools     bool
	ToolList     []ToolInfo
}

// AuthorizeSpecLaunch records durable consent for an explicitly user-installed
// project MCP without starting it a second time. The normal project discovery
// path still requires a user action; install_source calls this only while
// applying a plan the user already requested. Reuse an existing launcher lock
// when one exists, but do not add a second network/version-resolution step to an
// explicit install: the durable grant follows the exact configured command or
// endpoint and future changes still invalidate it.
func AuthorizeSpecLaunch(ctx context.Context, spec Spec) error {
	return authorizeSpecLaunch(ctx, spec, false)
}

// AuthorizeProjectSpecLaunch records the one durable launch confirmation used
// for repository-discovered MCP configuration. Mutable package launchers are
// resolved and locked, but the MCP server itself is not started: the caller can
// connect it exactly once after this function returns.
func AuthorizeProjectSpecLaunch(ctx context.Context, spec Spec) error {
	return authorizeSpecLaunch(ctx, spec, true)
}

func authorizeSpecLaunch(ctx context.Context, spec Spec, lockMutableLauncher bool) error {
	if !spec.RequireLaunchApproval {
		return nil
	}
	manager := spec.LaunchManager
	if manager == nil {
		return fmt.Errorf("MCP launch authorization store is unavailable")
	}
	var prepared Spec
	var launcherLock *mcplaunch.LauncherLock
	var err error
	if lockMutableLauncher {
		prepared, launcherLock, err = preparePersistentLauncher(ctx, spec)
	} else {
		prepared, err = applyStoredLauncherLock(spec)
	}
	if err != nil {
		return err
	}
	identityDigest, err := projectLaunchIdentityDigest(ctx, prepared)
	if err != nil {
		return err
	}
	if launcherLock != nil {

		if err := manager.PutLauncherLock(*launcherLock); err != nil {
			return err
		}
	}
	return manager.Authorize(prepared.Name, launchConfigSource(prepared), identityDigest)
}

// Failure records one MCP server that was configured but could not connect.
type Failure struct {
	Name                   string
	Transport              string
	Error                  string
	Stage                  string
	Elapsed                time.Duration
	Stderr                 string
	RequiresLaunchApproval bool
}

type launchApprovalError struct {
	server  string
	changed bool
}

func (e *launchApprovalError) Error() string {
	if e.changed {
		return fmt.Sprintf("project-provided MCP server %q changed; blocked before process or network startup and requires explicit re-authorization", e.server)
	}
	return fmt.Sprintf("project-provided MCP server %q is blocked before process or network startup until the user authorizes it", e.server)
}

func requiresLaunchApproval(err error) bool {
	var launchTarget *launchApprovalError
	return errors.As(err, &launchTarget)
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	params, unregisterProgress := c.withProgress(ctx, method, params)
	defer unregisterProgress()

	callCtx, cancel, timeout := c.contextWithCallTimeout(ctx, method, params)
	if cancel != nil {
		defer cancel()
	}

	res, err := c.callTransport(callCtx, method, params)
	if timeout > 0 && errors.Is(err, context.DeadlineExceeded) && callCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil {
		slog.Warn("plugin: MCP call timed out",
			"server", c.name, "method", method, "tool", rawToolNameFromCallParams(params), "timeout", timeout)
		return nil, c.timeoutError(method, params, timeout)
	}
	return res, err
}

func (c *Client) withProgress(ctx context.Context, method string, params any) (any, func()) {
	if method != "tools/call" {
		return params, func() {}
	}
	sink, ok := tool.ProgressFrom(ctx)
	if !ok {
		return params, func() {}
	}
	router, ok := c.t.(progressTransport)
	if !ok {
		return params, func() {}
	}
	callParams, ok := params.(map[string]any)
	if !ok {
		return params, func() {}
	}

	token := fmt.Sprintf("reasonix-%d", c.progressID.Add(1))
	copyParams := make(map[string]any, len(callParams))
	maps.Copy(copyParams, callParams)
	meta := map[string]any{}
	if existing, ok := callParams["_meta"].(map[string]any); ok {
		maps.Copy(meta, existing)
	}
	meta["progressToken"] = token
	copyParams["_meta"] = meta
	unregister := router.registerProgress(token, sink)
	return copyParams, unregister
}

func (c *Client) callTransport(ctx context.Context, method string, params any) (json.RawMessage, error) {
	res, err := c.t.call(ctx, method, params)
	if err == nil || method == "initialize" || !isHTTPSessionExpired(err) {
		return res, err
	}
	if initErr := c.initializeSession(ctx, false); initErr != nil {
		return nil, fmt.Errorf("%w; reinitialize failed: %w", err, initErr)
	}
	return c.t.call(ctx, method, params)
}

func (c *Client) contextWithCallTimeout(ctx context.Context, method string, params any) (context.Context, context.CancelFunc, time.Duration) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, nil, 0
	}
	timeout := c.callTimeout(method, params)
	if timeout <= 0 {
		timeout = defaultCallTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	return callCtx, cancel, timeout
}

func (c *Client) callTimeout(method string, params any) time.Duration {
	if method == "tools/call" {
		if raw := rawToolNameFromCallParams(params); raw != "" {
			if timeout := c.spec.ToolTimeouts[raw]; timeout > 0 {
				return timeout
			}
		}
	}
	if c.spec.CallTimeout > 0 {
		return c.spec.CallTimeout
	}
	if c.spec.DefaultCallTimeout > 0 {
		return c.spec.DefaultCallTimeout
	}
	return defaultCallTimeout
}

func rawToolNameFromCallParams(params any) string {
	m, ok := params.(map[string]any)
	if !ok {
		return ""
	}
	name, _ := m["name"].(string)
	return name
}

func (c *Client) timeoutError(method string, params any, timeout time.Duration) error {
	if method == "tools/call" {
		if raw := rawToolNameFromCallParams(params); raw != "" {
			return fmt.Errorf("MCP tool %q timed out after %s; increase tool_timeout_seconds or call_timeout_seconds to allow longer runs: %w",
				c.name+"."+raw, formatTimeout(timeout), context.DeadlineExceeded)
		}
	}
	return fmt.Errorf("MCP method %q on server %q timed out after %s; increase mcp_call_timeout_seconds or call_timeout_seconds to allow longer runs: %w",
		method, c.name, formatTimeout(timeout), context.DeadlineExceeded)
}

func formatTimeout(timeout time.Duration) string {
	if timeout > 0 && timeout%time.Second == 0 {
		return fmt.Sprintf("%ds", int(timeout/time.Second))
	}
	return timeout.String()
}

func (c *Client) notify(ctx context.Context, method string, params any) error {
	return c.t.notify(ctx, method, params)
}

func isHTTPSessionExpired(err error) bool {
	var expired *httpSessionExpiredError
	return errors.As(err, &expired)
}

func (c *Client) initialize(ctx context.Context) error {
	return c.initializeSession(ctx, true)
}

func (c *Client) initializeSession(ctx context.Context, recordCapabilities bool) error {
	capabilities := map[string]any{}
	if len(mcpRoots(c.spec.WorkspaceRoot)) > 0 {
		capabilities["roots"] = map[string]any{"listChanged": false}
	}
	res, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    capabilities,
		"clientInfo":      map[string]any{"name": "reasonix", "version": "dev"},
	})
	if err != nil {
		return err
	}
	if !recordCapabilities {

		return c.notify(ctx, "notifications/initialized", map[string]any{})
	}
	// Record which optional capabilities the server advertises. Presence of the
	// key (even with an empty object) signals support.
	var ir struct {
		Capabilities map[string]json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(res, &ir); err != nil {
		slog.Warn("plugin: parse initialize capabilities", "server", c.name, "err", err)
	}
	_, c.hasTools = ir.Capabilities["tools"]
	_, c.hasPrompts = ir.Capabilities["prompts"]
	_, c.hasResources = ir.Capabilities["resources"]

	return c.notify(ctx, "notifications/initialized", map[string]any{})
}
