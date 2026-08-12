package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/tool"
)

// StartAll connects every plugin in parallel, performs the MCP handshake, and
// returns the union of their tools (namespaced "mcp__<server>__<tool>"). On any
// failure it tears down everything started so far. The caller must Close the Host.
//
// For stdio plugins, subprocess lifetime is bound to ctx (via
// exec.CommandContext): cancelling ctx kills the children and unblocks reads.
func StartAll(ctx context.Context, specs []Spec) (*Host, []tool.Tool, error) {
	return Start(ctx, specs, StartPolicy{
		Concurrency:  defaultStartConcurrency,
		AbortOnError: true,
	})
}

// StartAvailable connects every plugin it can and records failures on the host
// instead of aborting the whole session. The returned tools are the union of the
// successfully connected servers.
func StartAvailable(ctx context.Context, specs []Spec) (*Host, []tool.Tool) {
	h, tools, _ := Start(ctx, specs, StartPolicy{
		PerPluginTimeout: defaultStartTimeout,
		Concurrency:      defaultStartConcurrency,
	})
	return h, tools
}

// Start is the unified batch-startup primitive behind StartAll / StartAvailable.
// It fans out handshakes in parallel under the policy's concurrency cap, gives
// each plugin its own per-plugin timeout, and either aborts the batch on first
// failure (AbortOnError=true) or records failures on the host and keeps going.
//
// Result ordering matches specs (stable for /mcp status). For stdio plugins the
// subprocess is bound to the parent ctx, not the per-plugin startup timeout:
// successful servers stay alive after startup, while failed/time-limited starts
// are closed explicitly before the goroutine returns.
func Start(ctx context.Context, specs []Spec, p StartPolicy) (*Host, []tool.Tool, error) {
	if len(specs) == 0 {
		return &Host{}, nil, nil
	}

	type result struct {
		idx    int
		spec   Spec
		client *Client
		tools  []tool.Tool
		err    error
	}

	concurrency := p.Concurrency
	if concurrency <= 0 || concurrency > len(specs) {
		concurrency = len(specs)
	}
	sem := make(chan struct{}, concurrency)
	ch := make(chan result, len(specs))

	h := &Host{}

	for i, s := range specs {
		go func(idx int, spec Spec) {
			sem <- struct{}{}
			defer func() { <-sem }()

			callCtx := ctx
			cancelStartup := func() {}
			if p.PerPluginTimeout > 0 {
				var cancel context.CancelFunc
				callCtx, cancel = context.WithTimeout(ctx, p.PerPluginTimeout)
				cancelStartup = cancel
			}

			phaseAStart := time.Now()
			recordedPhaseADur := func() time.Duration {
				dur := time.Since(phaseAStart)
				if p.PerPluginTimeout > 0 && callCtx.Err() == context.DeadlineExceeded && dur < p.PerPluginTimeout {
					return p.PerPluginTimeout
				}
				return dur
			}

			c, err := start(ctx, callCtx, spec)
			if err != nil {
				phaseADur := recordedPhaseADur()
				cancelStartup()
				if !p.SkipPersistence {
					h.bgWrites.Go(func() { ; _ = RecordStartup(spec.Name, phaseADur) })
				}
				ch <- result{idx: idx, spec: spec, err: fmt.Errorf("start plugin %q: %w", spec.Name, err)}
				return
			}

			ts, err := c.listTools(callCtx)
			if err != nil {
				phaseADur := recordedPhaseADur()
				cancelStartup()
				if !p.SkipPersistence {
					h.bgWrites.Go(func() { ; _ = RecordStartup(spec.Name, phaseADur) })
				}
				c.close()
				err = newStartupFailure("tools/list", phaseAStart, c.startupStderr(), err)
				ch <- result{idx: idx, spec: spec, err: fmt.Errorf("list tools from %q: %w", spec.Name, err)}
				return
			}
			c.toolCount = len(ts)

			phaseADur := recordedPhaseADur()
			cancelStartup()
			if !p.SkipPersistence {
				h.bgWrites.Go(func() {
					_ = RecordStartup(spec.Name, phaseADur)
					_ = SaveCachedSchema(spec.Name, CachedSchema{
						CacheKey: SchemaCacheKey(spec),
						Capabilities: map[string]bool{
							"tools":     c.hasTools,
							"prompts":   c.hasPrompts,
							"resources": c.hasResources,
						},
						Tools: cacheableToolsOf(ts),
					})
				})
			}

			ch <- result{idx: idx, spec: spec, client: c, tools: ts}
		}(i, s)
	}

	results := make([]result, len(specs))
	for range specs {
		r := <-ch
		results[r.idx] = r
	}

	var tools []tool.Tool
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if p.AbortOnError {
				if firstErr == nil {
					firstErr = r.err
				}
			} else {
				h.RecordFailure(r.spec, r.err)
			}
			continue
		}
		if err := h.noteClientLocked(r.client, nil); err != nil {
			r.client.close()
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		tools = append(tools, r.tools...)

	}
	if firstErr != nil {
		h.Close()
		return nil, nil, firstErr
	}
	return h, tools, nil
}

// Close terminates all plugin connections.
func (h *Host) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	var cancels []context.CancelFunc
	for _, serverCancels := range h.deferredCancels {
		cancels = append(cancels, serverCancels...)
	}
	h.deferredCancels = nil
	h.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	h.deferredWG.Wait()

	h.mu.Lock()
	clients := append([]*Client(nil), h.clients...)
	proxies := h.proxies
	h.proxies = nil
	h.clients = nil
	h.mu.Unlock()
	closeServerProxies(proxies)
	for _, c := range clients {
		if c != nil && c.t != nil {
			c.close()
		}
	}
	h.bgWrites.Wait()
}

// queueBackgroundWrite keeps detached persistence inside the Host lifecycle.
// Callers must enqueue before their Close-drained startup owner completes, so
// Close cannot begin waiting before the WaitGroup increment is visible.
func (h *Host) queueBackgroundWrite(write func()) {
	h.bgWrites.Go(func() {
		write()
	})
}

// StartPhaseB asynchronously fetches the auxiliary surfaces (prompts and
// resources) for every connected client. Boot calls it right after Start
// returns, on a session-scoped ctx, so the agent becomes responsive as soon as
// tools are ready and the slower list calls stream in afterwards. Each finished
// surface fires an MCPSurfaceReady event on sink so UIs (e.g. /mcp status) can
// refresh without polling. A nil sink is tolerated — the merge still happens.
// Errors are logged and swallowed: prompts/resources are non-essential and must
// not break the session over one slow server.
func (h *Host) StartPhaseB(ctx context.Context, sink event.Sink) {
	h.mu.RLock()
	clients := append([]*Client(nil), h.clients...)
	h.mu.RUnlock()
	for _, c := range clients {
		if c.hasPrompts {
			go h.fetchPrompts(ctx, c, sink)
		}
		if c.hasResources {
			go h.fetchResources(ctx, c, sink)
		}
	}
}

func (h *Host) fetchPrompts(ctx context.Context, c *Client, sink event.Sink) {
	aux, auxCtx, cancel, err := c.auxiliaryClient(ctx)
	if err != nil {
		slog.Warn("plugin: start auxiliary prompt client failed", "server", c.name, "err", err)
		return
	}
	defer cancel()
	defer aux.close()

	ps, err := aux.listPrompts(auxCtx)
	if err != nil {
		slog.Warn("plugin: listPrompts failed", "server", c.name, "err", err)
		return
	}
	for i := range ps {
		ps[i].client = c
	}
	h.mu.Lock()
	c.prompts = ps
	h.prompts = append(h.prompts, ps...)
	h.mu.Unlock()
	if sink != nil {
		sink.Emit(event.Event{
			Kind: event.MCPSurfaceReady,
			Text: fmt.Sprintf("%s: prompts ready (%d items)", c.name, len(ps)),
		})
	}
}

func (h *Host) fetchResources(ctx context.Context, c *Client, sink event.Sink) {
	aux, auxCtx, cancel, err := c.auxiliaryClient(ctx)
	if err != nil {
		slog.Warn("plugin: start auxiliary resource client failed", "server", c.name, "err", err)
		return
	}
	defer cancel()
	defer aux.close()

	rs, err := aux.listResources(auxCtx)
	if err != nil {
		slog.Warn("plugin: listResources failed", "server", c.name, "err", err)
		return
	}
	h.mu.Lock()
	c.resources = rs
	h.resources = append(h.resources, rs...)
	h.mu.Unlock()
	if sink != nil {
		sink.Emit(event.Event{
			Kind: event.MCPSurfaceReady,
			Text: fmt.Sprintf("%s: resources ready (%d items)", c.name, len(rs)),
		})
	}
}
