package main

// startRecoveryGC is intentionally a no-op for physical moves.
//
// Catalog v4 folds recovery lineages into one ordinary list row. Automatic
// startup/upgrade/timer GC must not move JSONL/meta into trash — only explicit
// CleanRecoveryLineage (UI, with preview) and `reasonix sessions cleanup
// --apply` may reclaim covered copies. reclaimRecoveryBranchesIn remains for
// those explicit entry points and focused tests.
func (a *App) startRecoveryGC() {}

// reclaimAdoptedRecoveryGroup compacts an entire legacy recovery chain once a
// canonical leaf has proved it covers every member. Every candidate is still
// revalidated under removal guards immediately before it is moved to trash.

// sessionOpenInAnyTab reports whether any tab's current session is path.
// Lease checks cover live runtimes; this additionally covers tabs that hold a
// session without a lease (read-only channel views).
func (a *App) sessionOpenInAnyTab(path string) bool {
	key := sessionRuntimeKey(path)
	if key == "" {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, tab := range a.tabs {
		if tab == nil {
			continue
		}
		if sessionRuntimeKey(tab.currentSessionPath()) == key {
			return true
		}
	}
	return false
}
