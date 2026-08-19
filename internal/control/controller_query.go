package control

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/billing"
	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/evidence"
	"reasonix/internal/guardian"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/skill"
)

// SetSessionPath rebinds auto-save without changing the current session
// preference. Callers creating a genuinely fresh conversation should use
// SetFreshSessionPath; callers resuming history should use Resume.
func (c *Controller) SetSessionPath(p string) {
	c.setSessionPath(p, false)
}

// SetFreshSessionPath binds a path that is known to belong to a newly-created
// session and samples the configured new-session recovery default.
func (c *Controller) SetFreshSessionPath(p string) {
	c.setSessionPath(p, true)
}

func (c *Controller) setSessionPath(p string, fresh bool) {

	c.snapshotMu.Lock()
	c.mu.Lock()
	c.sessionPath = p
	c.guardianPath = guardian.PathFor(p)
	c.mu.Unlock()

	c.bindExecutorProjection(p, !fresh)
	c.setActiveJobSession(p)
	c.rebindCheckpoints(p)
	if fresh {
		c.resetRecoveryForNewSession(p)

		c.rotateSessionTemp()
	} else {
		c.loadRecoveryState(p)
	}
	c.snapshotMu.Unlock()
	c.rebindInbox()
	if !fresh {
		c.recoverCheckpointTransactions()
	}
}

func (c *Controller) setActiveJobSession(sessionPath string) {
	if c.jobs != nil {
		c.jobs.SetActiveSessionPath(agent.BranchID(sessionPath), sessionPath)
	}
}

// SessionDir reports the directory new session files land in ("" disables
// persistence), so the caller can decide whether to mint a path.
func (c *Controller) SessionDir() string { return c.sessionDir }

// SessionPath reports the file the current conversation auto-saves to ("" when
// persistence is disabled), so a history view can mark the active session.
func (c *Controller) SessionPath() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionPath
}

func (c *Controller) parentSessionID() string {
	return agent.BranchID(c.SessionPath())
}

// History returns the executor's current message log (for repopulating a
// resumed frontend's view).
func (c *Controller) History() []provider.Message {
	if c.executor == nil {
		return nil
	}
	return c.executor.Session().Snapshot()
}

// SessionHasUnsavedChanges tells desktop history whether it may safely refresh
// an idle view from the durable WAL. A failed or contended save can leave the
// controller with a newer in-memory transcript; replacing that view from disk
// would hide the user's latest turn until the next retry.
func (c *Controller) SessionHasUnsavedChanges() bool {
	if c == nil || c.executor == nil {
		return false
	}
	return c.executor.Session().HasUnsavedChanges(c.SessionPath())
}

// HistoryLen returns the number of messages in the live log.
func (c *Controller) HistoryLen() int {
	if c.executor == nil {
		return 0
	}
	return c.executor.Session().Len()
}

// HistoryWindow returns a copy of the messages in [start, end) of the live
// log. Paging frontends use it to convert a display window without copying
// the whole history.
func (c *Controller) HistoryWindow(start, end int) []provider.Message {
	if c.executor == nil {
		return []provider.Message{}
	}
	return c.executor.Session().MessageRange(start, end)
}

// SessionPersistedState exposes the session's persistence baseline for the
// controller's current session path, so a paging frontend can validate a
// display-index sidecar against the live session.
func (c *Controller) SessionPersistedState() (agent.PersistedState, bool) {
	if c.executor == nil {
		return agent.PersistedState{}, false
	}
	return c.executor.Session().PersistedState(c.SessionPath())
}

// ContextSnapshot returns (usedTokens, contextWindow) for the gauge. usedTokens
// is what the next request will send, measured the way the compaction trigger
// measures it, so the gauge and the trigger can never disagree. Both zero means
// no data yet — a gauge hides itself.
func (c *Controller) ContextSnapshot() (int, int) {
	if c.executor == nil {
		return 0, 0
	}
	return c.executor.ContextUsedTokens(), c.executor.ContextWindow()
}

// CompactRatio returns the auto-compaction threshold as a fraction of the window
// (0 when the executor is unset). The status line shows headroom against it.
func (c *Controller) CompactRatio() float64 {
	if c.executor == nil {
		return 0
	}
	return c.executor.CompactRatio()
}

// LastUsage returns the most recent turn's token telemetry (nil before the first
// turn), so frontends can derive the prompt cache-hit rate for the status line.
func (c *Controller) LastUsage() *provider.Usage {
	if c.executor == nil {
		return nil
	}
	return c.executor.LastUsage()
}

// SessionCache returns cumulative cache hit/miss prompt tokens for the session,
// so a frontend can render the aggregate (session-wide) cache-hit rate — steadier
// than the single-turn rate and unaffected by compaction.
func (c *Controller) SessionCache() (hit, miss int) {
	if c.executor == nil {
		return 0, 0
	}
	return c.executor.SessionCache()
}

// Todos returns a copy of the canonical task list (the latest todo_write state
// merged with complete_step advances) so frontends can render a live task panel.
func (c *Controller) Todos() []evidence.TodoItem {
	if c.executor == nil {
		return nil
	}
	return c.executor.CanonicalTodoState()
}

// ToolResultData holds the full arguments and output for one tool call, loaded
// on demand when a frontend expands a collapsed tool card.
type ToolResultData struct {
	Args      string                  `json:"args"`
	Output    string                  `json:"output"`
	Execution *provider.ToolExecution `json:"execution,omitempty"`
}

// ToolResult looks up a tool call by its ID in the session history and returns
// the full arguments + output that were elided from the frontend's items[].
// Returns nil when the tool ID isn't found (e.g. a sub-agent's tool call that
// lives in a different session).
func (c *Controller) ToolResult(toolID string) *ToolResultData {
	if c.executor == nil {
		return nil
	}
	msgs := c.executor.Session().Snapshot()

	for i, msg := range slices.Backward(msgs) {
		if msg.Role != provider.RoleTool || msg.ToolCallID != toolID {
			continue
		}
		out := &ToolResultData{
			Args:      "",
			Output:    msg.Content,
			Execution: msg.ToolExecution,
		}

		for j := i; j >= 0; j-- {
			if msgs[j].Role != provider.RoleAssistant {
				continue
			}
			for _, tc := range msgs[j].ToolCalls {
				if tc.ID == toolID {
					out.Args = tc.Arguments
					return out
				}
			}
		}
		return out
	}
	return nil
}

// Balance queries the active provider's wallet balance, or (nil, nil) when the
// provider declares no balance_url — so a caller treats "not configured" and
// "fetched" the same and just omits the readout when nil.
func (c *Controller) Balance(ctx context.Context) (*billing.Balance, error) {
	if strings.TrimSpace(c.balanceURL) == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return billing.FetchWithClient(ctx, c.balanceClient, c.balanceURL, c.balanceKey)
}

// Host returns the running MCP host (nil when no plugins), for frontends that
// list servers / resolve MCP prompts.
func (c *Controller) Host() *plugin.Host { return c.mcp.hostRef() }

// Commands returns the loaded custom slash commands.
func (c *Controller) Commands() []command.Command {
	if p := c.commands.Load(); p != nil {
		return *p
	}
	return nil
}

// ReloadCommands rescans all command directories and hot-swaps the slash_command
// tool and the internal command slice — no MCP restart, no extension rerun.
func (c *Controller) ReloadCommands(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	cmds, loadErr := command.LoadRoots(config.CommandRootsForRoot(c.workspaceRoot)...)
	var cmdSkills []skill.Skill
	if !c.disableImplicitSkillInvocation {
		cmdSkills = c.SlashSkills()
	}

	entries := make([]command.SlashEntry, 0, len(cmdSkills)+len(cmds))
	for _, sk := range cmdSkills {

		entries = append(entries, command.SlashEntry{
			Name:        sk.SlashName(),
			Description: sk.Description,
			Render:      func(args []string) string { return c.skills.render(sk, strings.Join(args, " ")) },
		})
	}
	for _, cmd := range cmds {
		if cmd.Hidden {
			continue
		}

		entries = append(entries, command.SlashEntry{
			Name:        cmd.Name,
			Description: cmd.Description,
			ArgHint:     cmd.ArgHint,
			Render:      func(args []string) string { return cmd.Render(args) },
		})
	}
	c.mcp.registerTool(command.NewSlashCommandTool(entries))
	cmdSlice := cmds
	c.commands.Store(&cmdSlice)
	return loadErr
}

// Skills returns the discoverable skills (for the slash menu and `/skills`).
// When a live Store is available, scan it on demand so skills installed during
// this session appear without rewriting the cache-stable system prompt.
// Executor returns the underlying agent when present (nil for pure runners).
func (c *Controller) Executor() *agent.Agent {
	if c == nil {
		return nil
	}
	return c.executor
}

func (c *Controller) Skills() []skill.Skill {
	return c.skills.list()
}

// ImplicitSkillInvocationEnabled reports whether skills are exposed to the
// model for automatic discovery and invocation. Explicit /skill handling is
// independent of this model-facing capability.
func (c *Controller) ImplicitSkillInvocationEnabled() bool {
	return c != nil && !c.disableImplicitSkillInvocation
}

// SlashSkills returns the user-visible skill directory. Plugin skills use
// package-qualified names while Skills keeps bare model/run_skill identifiers.
func (c *Controller) SlashSkills() []skill.Skill {
	return c.skills.slashList()
}

// AllSkills returns every discoverable skill, including disabled ones, for
// management surfaces that need to re-enable a hidden skill.
func (c *Controller) AllSkills() []skill.Skill {
	return c.skills.listAll()
}

// DisabledSkills returns all discoverable skills that are disabled in config.
func (c *Controller) DisabledSkills() []skill.Skill {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	var out []skill.Skill
	for _, sk := range c.AllSkills() {
		if cfg.IsSkillDisabled(sk.Name) {
			out = append(out, sk)
		}
	}
	return out
}

// SkillEnabled reports whether a discoverable skill is enabled.
func (c *Controller) SkillEnabled(name string) bool {
	cfg, err := config.Load()
	if err != nil {
		return true
	}
	return !cfg.IsSkillDisabled(name)
}

// SetSkillEnabled persists a skill enable/disable preference. The caller should
// rebuild the controller for the prompt/tool registry to reflect it immediately.
func (c *Controller) SetSkillEnabled(name string, enabled bool) error {
	found := false
	for _, sk := range c.AllSkills() {
		if config.SkillNameKey(sk.Name) == config.SkillNameKey(name) {
			name = sk.Name
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("unknown skill: %s", name)
	}

	unlock := config.LockUserConfigEdits()
	defer unlock()
	cfg := config.LoadForEdit(config.UserConfigPath())
	if err := cfg.SetSkillEnabled(name, enabled); err != nil {
		return err
	}
	return cfg.SaveTo(config.UserConfigPath())
}

// CreateSkill writes a new skill file at the given scope and returns its
// path. Skills()/AllSkills()/RunSkill() read the live store on demand, so the
// new skill is usable (by name) immediately with no rebuild; the caller
// should still rebuild the controller for the pinned Skills index and tool
// registry to reflect it on the model's next turn, mirroring how
// SetSkillEnabled's callers already rebuild after a config change.
func (c *Controller) CreateSkill(name string, scope skill.Scope, content string) (string, error) {
	w := c.skills.writer()
	if w == nil {
		return "", fmt.Errorf("no writable skill store in this session")
	}
	return w.CreateWithContent(name, scope, content)
}

// UpdateSkill overwrites an existing user-authored skill file in place. See
// skill.Store.UpdateContent for the builtin-refusal and scope-match rules.
func (c *Controller) UpdateSkill(name string, scope skill.Scope, content string) error {
	w := c.skills.writer()
	if w == nil {
		return fmt.Errorf("no writable skill store in this session")
	}
	return w.UpdateContent(name, scope, content)
}

// DeleteSkill removes a user-authored skill file at the given scope. See
// skill.Store.Delete for the builtin-refusal and scope-match rules.
func (c *Controller) DeleteSkill(name string, scope skill.Scope) error {
	w := c.skills.writer()
	if w == nil {
		return fmt.Errorf("no writable skill store in this session")
	}
	return w.Delete(name, scope)
}

// AddMCPServer connects an MCP server live and persists it to the user-global
// config. Its tools are registered immediately and become available on the next
// turn (the agent reads the registry per turn). The raw entry — ${VARS} intact —
// is what's written to disk; the live connection uses the expanded form. Returns
// the number of tools the server exposed. Persistence is transactional: a config
// or activation failure removes the just-connected client so the live registry
// never claims an install that will disappear after restart.
func (c *Controller) AddMCPServer(e config.PluginEntry) (int, error) {

	e.Source = config.MCPSourceUserConfig
	if effective, loadErr := config.LoadForRootReadOnly(c.workspaceRoot); loadErr != nil {
		return 0, loadErr
	} else {
		for _, configured := range effective.Plugins {
			if configured.Name != e.Name {
				continue
			}
			if configured.Source != config.MCPSourceUserConfig && configured.Source != config.MCPSourceLegacyUser {
				return 0, fmt.Errorf("MCP server %q is already configured by %s; edit or remove that declaration before installing a global server with the same name", e.Name, configured.Source)
			}
			break
		}
	}
	n, err := c.connectMCPServer(e)
	if err != nil {
		return 0, err
	}
	if _, err := config.InstallUserPluginForRoot(c.workspaceRoot, e, true); err != nil {
		c.DisconnectMCPServer(e.Name)
		return 0, fmt.Errorf("saving MCP server config: %w", err)
	}
	return n, nil
}

// ConnectMCPServer connects an MCP server entry for this session without writing
// it to config. Desktop owns config placement so it can keep user-level settings
// out of project reasonix.toml while preserving the CLI AddMCPServer semantics.
func (c *Controller) ConnectMCPServer(e config.PluginEntry) (int, error) {
	return c.connectMCPServer(e)
}

// RegisterMCPServerOnDemand restores a configured server's cached provider
// surface without forcing a handshake. It is the durable-enable counterpart to
// ConnectMCPServer, which remains the explicit install/retry operation.
func (c *Controller) RegisterMCPServerOnDemand(e config.PluginEntry) (int, error) {
	spec := c.mcpSpec(e)
	n, err := c.mcp.registerSpecOnDemand(spec)
	if err == nil && c.capabilityRuntime != nil {
		c.capabilityRuntime.UpsertServer(e, spec, true)
	}
	return n, err
}

// connectMCPServer expands an entry's ${VARS}, applies the known-server
// overrides scoped to the workspace, and connects it live via the mcp manager.
func (c *Controller) connectMCPServer(e config.PluginEntry) (int, error) {
	spec := c.mcpSpec(e)
	n, err := c.mcp.connectSpec(spec)
	if err == nil && c.capabilityRuntime != nil {
		c.capabilityRuntime.UpsertServer(e, spec, true)
	}
	return n, err
}

func (c *Controller) mcpSpec(e config.PluginEntry) plugin.Spec {
	exp := e.ExpandedPlugin()
	configSource := strings.TrimSpace(string(exp.Source))
	spec := plugin.ApplyKnownOverrides(plugin.Spec{
		Name:               exp.Name,
		Type:               exp.Type,
		Command:            exp.Command,
		Args:               exp.Args,
		Env:                exp.Env,
		URL:                exp.URL,
		Headers:            exp.Headers,
		StartupTimeout:     controllerMCPTimeout(exp.StartupTimeoutSeconds),
		DefaultCallTimeout: c.mcpDefaultCallTimeout,
		CallTimeout:        controllerMCPTimeout(exp.CallTimeoutSeconds),
		ToolTimeouts:       controllerMCPToolTimeouts(exp.ToolTimeoutSeconds),
		WorkspaceRoot:      c.WorkspaceRoot(),
		ConfigSource:       configSource,
		Authorized:         exp.Source.UserAuthorized(),

		ProcessMode: plugin.MCPProcessHost,
	}, c.WorkspaceRoot())
	if exp.Source.ProjectScoped() && strings.TrimSpace(spec.Dir) == "" {
		spec.Dir = c.WorkspaceRoot()
	}
	if c.mcpConfigureSpec != nil {
		c.mcpConfigureSpec(&spec)
		if spec.ProcessMode == "" {
			spec.ProcessMode = plugin.MCPProcessHost
		}
	}
	return spec
}

// syncCapabilityRuntimeFromConfig restores one server's authoritative runtime
// entry after a transactional disconnect/rollback. enabledOverride is used for
// a session-only disconnect; nil re-resolves the durable activation state.
func (c *Controller) syncCapabilityRuntimeFromConfig(name string, enabledOverride *bool) {
	if c == nil || c.capabilityRuntime == nil {
		return
	}
	name = strings.TrimSpace(name)
	cfg, err := config.LoadForRoot(c.workspaceRoot)
	if err != nil {

		return
	}
	for _, entry := range cfg.Plugins {
		if strings.TrimSpace(entry.Name) != name {
			continue
		}
		enabled := entry.ShouldAutoStart()
		if enabledOverride != nil {
			enabled = *enabledOverride
		} else if resolved, resolveErr := config.DefaultMCPActivationStore().IsEnabled(entry, c.workspaceRoot); resolveErr == nil {
			enabled = resolved
		}
		c.capabilityRuntime.UpsertServer(entry, c.mcpSpec(entry), enabled)
		return
	}
	c.capabilityRuntime.RemoveServer(name)
}

func controllerMCPTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func controllerMCPToolTimeouts(values map[string]int) map[string]time.Duration {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]time.Duration, len(values))
	for name, seconds := range values {
		if name = strings.TrimSpace(name); name != "" && seconds > 0 {
			out[name] = time.Duration(seconds) * time.Second
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ImportMCPEntries persists selected MCP entries and attempts to connect them
// live. A connection failure does not roll back the config import: the user can
// fix local dependencies and reconnect in a later session.
func (c *Controller) ImportMCPEntries(entries []config.PluginEntry) (total, added, updated, connected, failed, skipped int, err error) {
	total, added, updated, err = config.ImportCCSwitchMCPEntries(entries)
	if err != nil {
		return 0, 0, 0, 0, 0, 0, err
	}
	effectiveCfg, loadErr := config.LoadForRoot(c.workspaceRoot)
	if loadErr != nil {
		return 0, 0, 0, 0, 0, 0, loadErr
	}
	effective := make(map[string]config.PluginEntry, len(effectiveCfg.Plugins))
	for _, entry := range effectiveCfg.Plugins {
		effective[entry.Name] = entry
	}
	for _, imported := range entries {
		e, ok := effective[imported.Name]
		if !ok || e.Source != config.MCPSourceUserConfig {

			skipped++
			continue
		}
		if c.mcp.hasServer(e.Name) {
			if c.capabilityRuntime != nil {

				c.capabilityRuntime.UpsertServer(e, c.mcpSpec(e), true)
			}
			skipped++
			continue
		}
		if _, err := c.AddMCPServer(e); err != nil {
			failed++
			continue
		}
		connected++
	}
	return total, added, updated, connected, failed, skipped, nil
}

func (c *Controller) ConfiguredMCPNames() []string {
	cfg, err := config.LoadForRootReadOnly(c.workspaceRoot)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Plugins))
	for _, p := range cfg.Plugins {
		names = append(names, p.Name)
	}
	return names
}

func (c *Controller) DisconnectedMCPNames() []string {
	cfg, err := config.LoadForRootReadOnly(c.workspaceRoot)
	if err != nil {
		return nil
	}
	connected := map[string]bool{}
	for _, name := range c.mcp.serverNames() {
		connected[name] = true
	}
	var names []string
	for _, p := range cfg.Plugins {
		if !connected[p.Name] {
			names = append(names, p.Name)
		}
	}
	return names
}

func (c *Controller) ConnectConfiguredMCPServer(name string) (int, error) {
	p, err := c.configuredMCPServer(name)
	if err != nil {
		return 0, err
	}
	return c.connectMCPServer(p)
}

func (c *Controller) configuredMCPServer(name string) (config.PluginEntry, error) {
	cfg, err := config.LoadForRoot(c.workspaceRoot)
	if err != nil {
		return config.PluginEntry{}, err
	}
	for _, p := range cfg.Plugins {
		if p.Name == name {
			return p, nil
		}
	}
	return config.PluginEntry{}, fmt.Errorf("no configured MCP server named %q", name)
}

// RemoveMCPServer removes writable config before disconnecting the live server.
// A persistence failure must not produce a false-successful session-only removal.
// MCPs contributed by installed plugin packages cannot be removed independently.
func (c *Controller) RemoveMCPServer(name string) (disconnected bool, err error) {
	cfg, lerr := config.LoadForRoot(c.workspaceRoot)
	if lerr != nil {
		return false, lerr
	}
	if owner, ok := cfg.PluginPackageOwner(name); ok {
		return false, fmt.Errorf("MCP server %q is managed by plugin %q; disable or remove the plugin instead", name, owner)
	}
	entry, removed, _, rerr := config.RemovePluginFromEffectiveSourceForRoot(c.workspaceRoot, name)
	if rerr != nil {
		return false, rerr
	}
	if !removed {
		return false, fmt.Errorf("no removable MCP server named %q", name)
	}
	_ = config.DefaultMCPActivationStore().ClearServer(entry, c.workspaceRoot)
	removedState := reconcileRemovedMCPState(c.workspaceRoot, name)
	if c.capabilityRuntime != nil {

		c.capabilityRuntime.RemoveServer(name)
	}
	disconnected = c.mcp.disconnect(name)
	if !disconnected {
		c.mcp.removeToolPrefix(name)
	}

	if removedState.fallbackFound {
		enabled := removedState.fallback.ShouldAutoStart()
		if resolved, resolveErr := config.DefaultMCPActivationStore().IsEnabled(removedState.fallback, c.workspaceRoot); resolveErr == nil {
			enabled = resolved
		}
		if enabled {
			_, _ = c.RegisterMCPServerOnDemand(removedState.fallback)
		} else {
			c.syncCapabilityRuntimeFromConfig(name, &enabled)
		}
	} else {
		c.syncCapabilityRuntimeFromConfig(name, nil)
	}
	return disconnected, removedState.cleanupErr
}

// DisconnectMCPServer disconnects a live server for this session without touching
// config — the connector toggle's "off". Its tools vanish next turn; it reconnects
// on the next session start, or now via ConnectConfiguredMCPServer (the "on").
// Reports whether a live server was actually disconnected.
func (c *Controller) DisconnectMCPServer(name string) bool {
	if c.capabilityRuntime != nil {
		c.capabilityRuntime.SetServerEnabled(name, false)
	}
	disconnected := c.mcp.disconnect(name)
	removedPlaceholder := 0
	if !disconnected {
		removedPlaceholder = c.mcp.removeToolPrefix(name)
	}

	disabled := false
	c.syncCapabilityRuntimeFromConfig(name, &disabled)
	return disconnected || removedPlaceholder > 0
}

// UnregisterMCPServerTools hides a shared MCP server from this controller only.
// The desktop shared-host path uses this for per-tab connector toggles: the
// shared client stays alive for sibling tabs, while this session's registry drops
// the server's provider-visible tools before the next turn.
func (c *Controller) UnregisterMCPServerTools(name string) bool {
	if c.capabilityRuntime != nil {
		c.capabilityRuntime.SetServerEnabled(name, false)
	}
	return c.mcp.suspendToolPrefix(name)
}

// Label returns the human-readable model label, e.g. "deepseek-flash".
func (c *Controller) Label() string { return c.label }

// ModelRef returns the canonical provider/model reference for the session.
func (c *Controller) ModelRef() string { return c.modelRef }

// WorkspaceRoot returns the workspace root for this controller's session
// (the directory that file-writers and @-references are scoped to).
// Empty means no scoping is in effect.
func (c *Controller) WorkspaceRoot() string { return c.workspaceRoot }

func (c *Controller) imageInputEnabled() bool {
	ref := c.modelRef
	cfg, err := config.LoadForRoot(c.workspaceRoot)
	if err == nil && ref == "" {
		ref = cfg.DefaultModel
	}
	if err != nil || ref == "" {
		return false
	}
	entry, ok := cfg.ResolveModel(ref)
	return ok && config.EffectiveVision(entry)
}

// ImageInputEnabled reports whether the current model accepts direct image
// inputs, so frontends can gate image-only UX before a turn starts.
func (c *Controller) ImageInputEnabled() bool { return c.imageInputEnabled() }

// InheritLifecycleFrom carries same-session lifecycle state across controller
// rebuilds, such as model switches that preserve the conversation.
func (c *Controller) InheritLifecycleFrom(prev *Controller) {
	if prev == nil {
		return
	}
	prev.mu.Lock()
	started := prev.startedOnce
	turn := prev.turn
	prev.mu.Unlock()

	c.mu.Lock()
	c.startedOnce = started
	if c.turn < turn {
		c.turn = turn
	}
	c.mu.Unlock()
}
