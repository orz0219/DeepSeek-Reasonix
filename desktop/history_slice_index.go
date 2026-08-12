package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/store"
)

// coldEventHistorySlice pages a legacy event-record session. The decode
// streams the file once per request (constant memory); only ancient sessions
// take this path.
func coldEventHistorySlice(sessionPath string, info os.FileInfo, req HistorySliceRequest) (HistorySlice, error) {
	messages, ok, err := previewEventSessionMessages(sessionPath)
	if err != nil || !ok {
		return emptyHistorySlice(), err
	}
	digest := fmt.Sprintf("event:%d:%d", info.Size(), info.ModTime().UnixNano())
	src := &historySliceSource{
		sessionID: strings.TrimSuffix(filepath.Base(sessionPath), ".jsonl"),
		digest:    digest,
	}
	return pageHistoryEventRows(src, messages, req), nil
}

// pageHistoryEventRows cuts a page from already-converted rows (legacy event
// format). Row indexes play the role of message indexes; every row is its own
// group. Turns count user rows, 1-based.
func pageHistoryEventRows(src *historySliceSource, rows []HistoryMessage, req HistorySliceRequest) HistorySlice {
	cursor, err := decodeHistorySliceCursor(req.Cursor)
	hasCursor := req.Cursor != "" && err == nil
	if hasCursor && src.digest != cursor.Digest {
		return staleHistorySlice(0, false, src.digest)
	}
	hi := len(rows)
	if hasCursor && cursor.Before < hi {
		hi = cursor.Before
	}

	turns := make([]int, len(rows))
	turn := 0
	for i, r := range rows {
		if r.Role == "user" {
			turn++
		}
		turns[i] = turn
	}
	src.turns = turns
	src.totalTurns = turn
	src.total = len(rows)
	page := HistorySlice{Entries: []HistoryEntry{}, TotalTurns: turn, Digest: src.digest}
	if hi <= 0 {
		return page
	}
	newestTurn := turns[hi-1]
	oldestTurn := 0
	if newestTurn > 0 {
		oldestTurn = max(newestTurn-req.Turns+1, 1)
	}
	candidateLo := sort.Search(hi, func(i int) bool { return turns[i] >= oldestTurn })
	if oldestTurn <= 1 {
		candidateLo = 0
	}

	kept := make([]HistoryEntry, 0, req.Entries)
	entryCount, byteCount := 0, 0
	lo := hi
	for i := hi - 1; i >= candidateLo; i-- {
		entry := newHistoryEntry(src, fmt.Sprintf("s%s:r0:e%d:o0", src.sessionID, i), i, 0, rows[i])
		b := entry.inlineBytes()
		if len(kept) > 0 && (entryCount+1 > req.Entries || byteCount+b > req.Bytes) {
			break
		}
		kept = append(kept, entry)
		entryCount++
		byteCount += b
		lo = i
	}
	for _, e := range slices.Backward(kept) {
		page.Entries = append(page.Entries, e)
	}
	for _, e := range page.Entries {
		if e.Turn <= 0 {
			continue
		}
		if page.StartTurn == 0 || e.Turn < page.StartTurn {
			page.StartTurn = e.Turn
		}
		if e.Turn > page.EndTurn {
			page.EndTurn = e.Turn
		}
	}
	page.HasOlder = lo > 0
	if page.HasOlder {
		page.NextCursor = encodeHistorySliceCursor(historySliceCursor{V: 1, Digest: src.digest, Before: lo})
	}
	return page
}

// legacyHistoryFieldValue re-resolves a field of a legacy event-format row.
func (a *App) legacyHistoryFieldValue(sessionPath, sessionDir string, row int, ref HistoryContentRef) (string, bool) {
	absPath, _, err := validateSessionPath(sessionDir, sessionPath)
	if err != nil {
		return "", false
	}
	messages, ok, err := previewEventSessionMessages(absPath)
	if err != nil || !ok || row < 0 || row >= len(messages) {
		return "", false
	}
	return historyEntryFieldValue(&messages[row], ref.Field, ref.ToolCallID)
}

// kickHistoryIndexRebuild single-flight schedules a background display-index
// rebuild for a live session whose on-disk index did not validate. It never
// blocks the request path.
func (a *App) kickHistoryIndexRebuild(sessionPath string) {
	if strings.TrimSpace(sessionPath) == "" {
		return
	}
	a.historySliceMu.Lock()
	if a.historyIndexRebuilds == nil {
		a.historyIndexRebuilds = map[string]struct{}{}
	}
	if _, ok := a.historyIndexRebuilds[sessionPath]; ok {
		a.historySliceMu.Unlock()
		return
	}
	a.historyIndexRebuilds[sessionPath] = struct{}{}
	a.historySliceMu.Unlock()
	a.goSafe("historyIndexRebuild", func() {
		defer func() {
			a.historySliceMu.Lock()
			delete(a.historyIndexRebuilds, sessionPath)
			a.historySliceMu.Unlock()
		}()
		a.rebuildHistoryIndexForLiveSession(sessionPath)
	})
}

// kickHistoryReadModelRepair single-flights the stronger cold-session repair:
// replay the authoritative event log under the save lock, atomically refresh
// the JSONL random-read model, then publish matching offsets. The cold request
// already returned from its in-memory recovery source before this work starts.
func (a *App) kickHistoryReadModelRepair(sessionPath string) {
	if strings.TrimSpace(sessionPath) == "" {
		return
	}
	key := "read-model:" + agent.CanonicalSessionPath(sessionPath)
	a.historySliceMu.Lock()
	if a.historyIndexRebuilds == nil {
		a.historyIndexRebuilds = map[string]struct{}{}
	}
	if _, ok := a.historyIndexRebuilds[key]; ok {
		a.historySliceMu.Unlock()
		return
	}
	a.historyIndexRebuilds[key] = struct{}{}
	a.historySliceMu.Unlock()
	a.goSafe("historyReadModelRepair", func() {
		defer func() {
			a.historySliceMu.Lock()
			delete(a.historyIndexRebuilds, key)
			a.historySliceMu.Unlock()
		}()
		if err := agent.RepairSessionDisplayReadModel(sessionPath); err != nil {
			slog.Debug("desktop: history read-model repair failed", "path", sessionPath, "err", err)
		}
	})
}

// rebuildHistoryIndexForLiveSession republishes the display index for a live
// session, but only when the in-memory log is exactly the persisted
// transcript — an append-only tail means the next save will publish a
// covering index anyway, and a scanned .jsonl anchor cannot describe the
// event-log tail.
func (a *App) rebuildHistoryIndexForLiveSession(sessionPath string) {
	a.mu.RLock()
	ctrls := make([]control.SessionAPI, 0, len(a.tabs))
	for _, tab := range a.tabs {
		if tab != nil && tab.Ctrl != nil {
			ctrls = append(ctrls, tab.Ctrl)
		}
	}
	a.mu.RUnlock()
	var ctrl control.SessionAPI
	for _, c := range ctrls {
		if c.SessionPath() == sessionPath {
			ctrl = c
			break
		}
	}
	wc, ok := ctrl.(historyWindowController)
	if !ok {
		return
	}
	ps, ok := wc.SessionPersistedState()
	if !ok || !ps.UnchangedSincePersisted {
		return
	}
	if err := agent.RepairSessionDisplayReadModel(sessionPath); err != nil {
		slog.Debug("desktop: live history read-model rebuild failed", "path", sessionPath, "err", err)
	}
}

// startHistoryIndexMigration arms the startup background worker that builds
// display indexes for session files that predate the sidecar. Like
// enableDeferredRebuildRetry it is only called from the Wails startup hook, so
// test-constructed Apps never spawn the worker.
func (a *App) startHistoryIndexMigration() {
	if a.ctx == nil {
		return
	}
	a.historySliceMu.Lock()
	if a.historyIndexMigrationCancel != nil {
		a.historySliceMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.historyIndexMigrationCancel = cancel
	a.historySliceMu.Unlock()
	a.goSafe("historyIndexMigration", func() { a.historyIndexMigrationLoop(ctx) })
}

// stopHistoryIndexMigration stops the startup migration worker; called from
// shutdown. The worker also stops with the Wails context.
func (a *App) stopHistoryIndexMigration() {
	a.historySliceMu.Lock()
	cancel := a.historyIndexMigrationCancel
	a.historySliceMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// historyIndexMigrationLoop walks every known session dir once, building
// missing or stale display indexes. It is single-concurrency, yields between
// sessions, and is idempotent: a valid index (loadable + transcript size
// match) is left untouched.
func (a *App) historyIndexMigrationLoop(ctx context.Context) {
	for _, dir := range a.knownSessionDirs() {
		if ctx.Err() != nil {
			return
		}

		infos, err := agent.ListSessionOrder(dir)
		if err != nil {
			continue
		}
		for _, info := range infos {
			if ctx.Err() != nil {
				return
			}
			path := info.Path
			if !store.IsSessionTranscriptName(filepath.Base(path)) {
				continue
			}
			if historySessionIndexOnDiskValid(path) || historySessionLooksEventFormat(path) {
				continue
			}
			if err := agent.RepairSessionDisplayReadModel(path); err != nil {
				slog.Debug("desktop: history read-model migration failed", "path", path, "err", err)
			}
			timer := time.NewTimer(25 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
}

// historySessionIndexOnDiskValid reports whether the on-disk display index
// loads and describes the current transcript file size. The size guard is the
// Phase A stale-anchor rule: append-only saves leave the .jsonl anchor behind
// the canonical transcript, and the reverse (a rewritten anchor with an old
// index) must not be sliced by stale offsets either.
func historySessionIndexOnDiskValid(sessionPath string) bool {
	indexPath := store.SessionDisplayIndex(sessionPath)
	idx, err := agent.LoadSessionDisplayIndex(indexPath)
	if err != nil {
		return false
	}
	info, err := os.Stat(sessionPath)
	if err != nil {
		return false
	}
	if idx.TranscriptSize != info.Size() || !historyIndexTimestampValid(indexPath, sessionPath, info, idx, false) {
		return false
	}
	identity, known, err := agent.SessionContentIdentity(sessionPath)
	if err != nil {
		return false
	}
	if !known {
		return !idx.RevisionKnown
	}
	return agent.ValidateSessionDisplayIndex(idx, identity.Revision, identity.RevisionKnown, identity.Digest, info.Size())
}
