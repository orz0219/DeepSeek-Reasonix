package control

import (
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/extension/dispatch"
	"reasonix/internal/provider"
	"reasonix/internal/store"
	"reasonix/internal/tool"
)

// SetDisplayRecorder installs an optional hook used by frontends that persist a
// shorter user-facing transcript than the fully composed model prompt.
func (c *Controller) SetDisplayRecorder(fn func(content, display string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.displayRecorder = fn
}

// SetExtensions installs the extension dispatcher after construction. Boot
// uses it because sidecars — and therefore the dispatcher — only exist after
// snapshot assembly, which runs after New. First non-nil install wins for the
// cold-start path; use ReplaceExtensions for generation-safe rebuild swaps.
// Nil is a no-op. The executor agent receives the same dispatcher (stage 6b2).
func (c *Controller) SetExtensions(d *dispatch.Dispatcher) {
	if d == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.extensions != nil {
		return
	}
	c.installExtensionsLocked(d)
}

// ReplaceExtensions atomically swaps the dispatcher for a reused controller
// after a narrow rebuild. Updates sink strategy owner and executor together.
func (c *Controller) ReplaceExtensions(d *dispatch.Dispatcher) {
	if c == nil || d == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.installExtensionsLocked(d)
}

func (c *Controller) installExtensionsLocked(d *dispatch.Dispatcher) {
	c.extensions = d

	switch sink := c.sink.(type) {
	case *inboxEventSink:
		if existing, ok := sink.inner.(*frontendEventSink); ok {
			existing.setDispatcher(d)
		} else {
			sink.inner = newFrontendEventSink(sink.inner, d)
		}
	case *frontendEventSink:
		sink.setDispatcher(d)

		c.sink = &inboxEventSink{inner: sink, c: c}
	default:
		c.sink = &inboxEventSink{inner: newFrontendEventSink(c.sink, d), c: c}
	}
	if c.executor != nil {
		c.executor.SetExtensions(d)
		c.executor.SetSink(c.sink)
	}
}

// SetProviderResolver replaces the session's merged provider catalog (narrow
// rebuild after sidecar Manager roll). Nil clears extension-hosted providers.
func (c *Controller) SetProviderResolver(r provider.Resolver) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.providerResolver = r
	c.mu.Unlock()
}

// ApplyExtensionSystemPrompt swaps the executor to a fresh session carrying
// the extension strategy's final system prompt and makes it the controller's
// rotation prompt, so /new and /clear keep the strategy-composed prompt too.
// Boot calls it when a system_prompt.build replacement changed the prompt
// after the controller (and its session) was built with the host-composed
// one. It must run before any turn or history resume: the fresh session holds
// only the system message, so a later resume cleanly layers history on top.
func (c *Controller) ApplyExtensionSystemPrompt(prompt string) {
	if c == nil || c.executor == nil {
		return
	}
	c.mu.Lock()
	c.systemPrompt = prompt
	c.mu.Unlock()
	c.executor.SetSession(agent.NewSession(prompt))
}

// SetOnSessionRecovered installs the ownership handoff invoked before the
// controller commits to an automatically created recovery branch. Frontends
// that acquire their session owner after controller construction (for example
// reasonix serve) use this before publishing the controller.
func (c *Controller) SetOnSessionRecovered(fn func(SessionRecoveryInfo) error) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onSessionRecovered = fn
}

func (c *Controller) sessionRecoveredHandler() func(SessionRecoveryInfo) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.onSessionRecovered
}

func (c *Controller) recordDisplay(content, display string) {
	if strings.TrimSpace(display) == "" || content == display {
		return
	}
	c.mu.Lock()
	record := c.displayRecorder
	c.mu.Unlock()
	if record != nil {
		record(content, display)
	}
}

// ToolContractEntries returns a stable snapshot of the executor's live tool
// contract: provider-visible names, descriptions, canonical schemas, and
// read-only flags. It is intended for diagnostics and regression tests.
func (c *Controller) ToolContractEntries() []tool.ContractEntry {
	if c == nil {
		return nil
	}
	reg := c.mcp.registry()
	if reg == nil {
		return nil
	}
	return reg.ContractEntries()
}

// AllToolContractEntries returns every registered tool, including those hidden
// from the provider-visible schema and only reachable via use_capability.
func (c *Controller) AllToolContractEntries() []tool.ContractEntry {
	if c == nil {
		return nil
	}
	reg := c.mcp.registry()
	if reg == nil {
		return nil
	}
	return reg.AllContractEntries()
}

// ProviderCatalog returns the session's merged provider catalog: the config
// (or broker) base plus every provider a live extension sidecar declared,
// keyed by ref — extension refs carry their plugin/<plugin>/<provider>/<model>
// namespace. Nil when no sidecar declared providers, so frontends can tell
// "enumerate config only" apart from "the extension catalog is empty".
func (c *Controller) ProviderCatalog() []provider.Descriptor {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	r := c.providerResolver
	c.mu.Unlock()
	if r == nil {
		return nil
	}
	return r.Catalog()
}

func (c *Controller) recordDisplayForNewUser(startMessages int, display string) {
	if strings.TrimSpace(display) == "" {
		return
	}
	msgs := c.History()
	if startMessages > len(msgs) {
		startMessages = len(msgs)
	}
	for _, m := range msgs[startMessages:] {
		if m.Role == provider.RoleUser {
			c.recordDisplay(m.Content, display)
			return
		}
	}
}

func (c *Controller) markEditedForNewUser(startMessages int, original string) {
	if strings.TrimSpace(original) == "" || c.executor == nil {
		return
	}
	s := c.executor.Session()
	msgs := s.Snapshot()
	if startMessages > len(msgs) {
		startMessages = len(msgs)
	}
	for i := startMessages; i < len(msgs); i++ {
		if msgs[i].Role != provider.RoleUser {
			continue
		}
		if agent.UserMessageText(msgs[i]) == original {
			return
		}
		msgs[i].Edited = true
		msgs[i].Original = original

		s.ReplaceLocalMetadata(msgs)
		return
	}
}

// ckptDir derives a session's checkpoint directory from its file path
// (…/<id>.jsonl → …/<id>.ckpt). Empty path → empty (in-memory checkpoints).
func ckptDir(sessionPath string) string {
	return store.SessionCheckpointDir(sessionPath)
}

// rebindCheckpoints points the store at the (possibly new) session, loading any
// checkpoints already on disk, and resets the turn boundaries. Called on
// construction and whenever the session path changes (NewSession/Resume/SetSessionPath).
// Also re-wires the mutation observer so capture targets the new store.
func (c *Controller) rebindCheckpoints(sessionPath string) {
	c.goals.setStatePath(goalStatePath(sessionPath))
	c.checkpoints.rebind(ckptDir(sessionPath), c.workspaceRoot)
	if c.executor != nil {
		c.wireMutationObserver()
	}
}
