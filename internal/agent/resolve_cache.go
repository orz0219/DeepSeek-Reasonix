package agent

import (
	"reasonix/internal/tool"
)

// toolResolveEntry caches one ResolveCall result so 4-6 redundant lookups
// per tool call within a single batch collapse to one.
type toolResolveEntry struct {
	tool      tool.Tool
	canonical string
	known     bool
	readOnly  bool
}

// resolveCache is a per-batch snapshot of ResolveCall results, keyed by the
// provider-visible tool name. It is allocated once in executeBatch and passed
// to every helper that would otherwise call Registry.ResolveCall again.
type resolveCache struct {
	reg     *tool.Registry
	entries map[string]toolResolveEntry
}

func newResolveCache(reg *tool.Registry) *resolveCache {
	return &resolveCache{
		reg:     reg,
		entries: make(map[string]toolResolveEntry, 8),
	}
}

// get returns the cached entry for name, resolving on first access.
func (c *resolveCache) get(name string) toolResolveEntry {
	if e, ok := c.entries[name]; ok {
		return e
	}
	t, canonical, ambiguous := c.reg.ResolveCall(name)
	e := toolResolveEntry{
		tool:      t,
		canonical: canonical,
		known:     t != nil && len(ambiguous) == 0,
	}
	if e.known {
		e.readOnly = t.ReadOnly()
	}
	c.entries[name] = e
	return e
}
