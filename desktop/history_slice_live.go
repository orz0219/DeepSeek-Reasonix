package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/provider"
	"reasonix/internal/store"
)

// historyWindowController is the slice of *control.Controller the windowed
// live path needs. Kept as an interface assertion (like
// sessionTempFromController) so test fakes implementing control.SessionAPI
// keep working via the full-snapshot fallback.
type historyWindowController interface {
	HistoryLen() int
	HistoryWindow(start, end int) []provider.Message
	SessionPersistedState() (agent.PersistedState, bool)
}

// HistorySliceForTab returns one page of the tab's history toward older
// messages, converting only the returned window.
func (a *App) HistorySliceForTab(tabID string, req HistorySliceRequest) HistorySlice {
	req = normalizeHistorySliceRequest(req)
	a.mu.RLock()
	tab := a.tabByIDLocked(tabID)
	var ctrl control.SessionAPI
	var sessionDir, sessionPath string
	if tab != nil {
		ctrl = tab.Ctrl
		sessionDir = tabSessionDir(tab)
		sessionPath = tab.currentSessionPath()
	}
	a.mu.RUnlock()

	if ctrl == nil {
		if strings.TrimSpace(sessionPath) == "" {
			return failedHistorySlice("session path unavailable before controller ready")
		}
		slice, err := a.coldHistorySlice(sessionDir, sessionPath, req)
		if err != nil {
			slog.Debug("desktop: cold history slice failed", "path", sessionPath, "err", err)
			return failedHistorySlice(err.Error())
		}
		return slice
	}
	if p := ctrl.SessionPath(); strings.TrimSpace(p) != "" {
		sessionPath = p
		sessionDir = controllerSessionDir(ctrl)
	}
	return a.liveHistorySlice(ctrl, sessionDir, sessionPath, req)
}

// liveHistorySlice pages a tab with a running controller. The display index
// supplies turn boundaries when it validates against the session's persisted
// state; otherwise a single in-memory snapshot walk classifies turns (still
// converting only the window) and a background rebuild is kicked.
func (a *App) liveHistorySlice(ctrl control.SessionAPI, sessionDir, sessionPath string, req HistorySliceRequest) HistorySlice {
	resolver := sessionDisplayResolver(sessionDir, sessionPath)
	src, indexUsed := a.liveHistorySliceSource(ctrl, sessionPath, resolver)
	if src == nil {
		return emptyHistorySlice()
	}
	if !indexUsed {
		a.kickHistoryIndexRebuild(sessionPath)
	}
	slice, err := a.pageHistorySliceSource(src, req, resolver, sessionPlannerDisplayTurns(sessionDir, sessionPath), ctrl.CheckpointTurnsByMessageIndex(), sessionPath)
	if err != nil {
		slog.Debug("desktop: live history slice failed", "path", sessionPath, "err", err)
		return failedHistorySlice(err.Error())
	}
	if indexUsed {
		slice.Source = "live-index"
	} else {
		slice.Source = "live-fallback"
	}
	return slice
}

func (a *App) liveHistorySliceSource(ctrl control.SessionAPI, sessionPath string, resolver func(string) string) (*historySliceSource, bool) {
	sessionID := strings.TrimSuffix(filepath.Base(sessionPath), ".jsonl")
	wc, ok := ctrl.(historyWindowController)
	if !ok {

		msgs := ctrl.History()
		src := newInMemoryHistorySliceSource(sessionID, msgs, resolver, agent.PersistedState{}, false)
		return src, false
	}
	n := wc.HistoryLen()
	ps, psOK := wc.SessionPersistedState()
	if psOK && ps.AppendOnlyTail && n > 0 {
		if idx, err := agent.LoadSessionDisplayIndex(store.SessionDisplayIndex(sessionPath)); err == nil &&
			idx.RevisionKnown == ps.RevisionKnown &&
			(!ps.RevisionKnown || idx.Revision == ps.Revision) &&
			idx.ContentDigest == ps.DigestHex &&
			idx.MessageCount <= n {
			turns := make([]int, n)
			roles := make([]provider.Role, n)
			for i, e := range idx.Entries {
				turns[i] = e.AuthoredTurn
				roles[i] = e.Role
			}
			turn := idx.AuthoredTurns
			if idx.MessageCount < n {
				tail := wc.HistoryWindow(idx.MessageCount, n)
				for j, m := range tail {
					if isVisibleHistoryUser(m, resolver) {
						turn++
					}
					turns[idx.MessageCount+j] = turn
					roles[idx.MessageCount+j] = m.Role
				}
			}
			src := &historySliceSource{
				sessionID:  sessionID,
				total:      n,
				turns:      turns,
				roles:      roles,
				totalTurns: turn,
				revision:   ps.Revision,
				revKnown:   ps.RevisionKnown,
				digest:     ps.DigestHex,
				epoch:      ps.RewriteEpoch,
				fetch: func(lo, hi int) ([]provider.Message, error) {
					return wc.HistoryWindow(lo, hi), nil
				},
			}
			if ps.UnchangedSincePersisted && idx.MessageCount == n {
				src.cacheKey = historyDerivedSourceKey(sessionPath, src)
			}
			return src, true
		}
	}

	msgs := ctrl.History()
	var state agent.PersistedState
	if psOK {
		state = ps
	}
	src := newInMemoryHistorySliceSource(sessionID, msgs, resolver, state, psOK)
	if psOK && ps.UnchangedSincePersisted {
		src.cacheKey = historyDerivedSourceKey(sessionPath, src)
	}
	return src, false
}

// newInMemoryHistorySliceSource builds a source by classifying a full
// in-memory snapshot with the resolver-based visible-turn rule — the same
// semantics the legacy history path uses.
func newInMemoryHistorySliceSource(sessionID string, msgs []provider.Message, resolver func(string) string, ps agent.PersistedState, psOK bool) *historySliceSource {
	turns := make([]int, len(msgs))
	roles := make([]provider.Role, len(msgs))
	turn := 0
	for i, m := range msgs {
		if isVisibleHistoryUser(m, resolver) {
			turn++
		}
		turns[i] = turn
		roles[i] = m.Role
	}
	src := &historySliceSource{
		sessionID:  sessionID,
		total:      len(msgs),
		turns:      turns,
		roles:      roles,
		totalTurns: turn,
		fetch: func(lo, hi int) ([]provider.Message, error) {
			if lo < 0 {
				lo = 0
			}
			if hi > len(msgs) {
				hi = len(msgs)
			}
			if lo >= hi {
				return []provider.Message{}, nil
			}
			return msgs[lo:hi], nil
		},
	}
	if psOK {
		src.revision = ps.Revision
		src.revKnown = ps.RevisionKnown
		src.digest = ps.DigestHex
		src.epoch = ps.RewriteEpoch
	}
	return src
}

func historyDerivedSourceKey(sessionPath string, src *historySliceSource) string {
	if src == nil || strings.TrimSpace(sessionPath) == "" || strings.TrimSpace(src.digest) == "" {
		return ""
	}
	return fmt.Sprintf("%s|%t|%d|%s|%d", agent.CanonicalSessionPath(sessionPath), src.revKnown, src.revision, src.digest, src.total)
}

// coldHistorySlice pages a session file with no running controller. It never
// loads the whole session: a valid on-disk display index + byte-offset reads
// serve the window; a missing/stale/corrupt index is rebuilt by streaming
// scan (constant memory) and the first page is served from the scan result.
func (a *App) coldHistorySlice(sessionDir, path string, req HistorySliceRequest) (HistorySlice, error) {
	sessionPath, _, err := validateSessionPath(sessionDir, path)
	if err != nil {
		return emptyHistorySlice(), err
	}
	info, err := os.Stat(sessionPath)
	if err != nil {
		return emptyHistorySlice(), err
	}
	if info.IsDir() {
		return emptyHistorySlice(), fmt.Errorf("not a session file: %s", sessionPath)
	}
	if historySessionLooksEventFormat(sessionPath) {

		slice, err := coldEventHistorySlice(sessionPath, info, req)
		slice.Source = "scan"
		return slice, err
	}
	resolver := sessionDisplayResolver(sessionDir, sessionPath)
	indexPath := store.SessionDisplayIndex(sessionPath)
	idx, err := agent.LoadSessionDisplayIndex(indexPath)
	identity, identityKnown, identityErr := agent.SessionContentIdentity(sessionPath)
	if identityErr != nil {
		return emptyHistorySlice(), identityErr
	}
	indexIdentityValid := false
	if idx != nil && err == nil {
		if identityKnown {
			indexIdentityValid = agent.ValidateSessionDisplayIndex(idx, identity.Revision, identity.RevisionKnown, identity.Digest, info.Size())
		} else {

			indexIdentityValid = !idx.RevisionKnown
		}
	}
	if idx != nil && err == nil && idx.TranscriptSize == info.Size() && indexIdentityValid && historyIndexTimestampValid(indexPath, sessionPath, info, idx, true) {
		slice, pageErr := a.pageHistorySliceSource(coldHistorySliceSource(sessionPath, idx), req, resolver, sessionPlannerDisplayTurns(sessionDir, sessionPath), nil, sessionPath)
		if pageErr != nil {
			return emptyHistorySlice(), pageErr
		}
		slice.Source = "index"
		return slice, nil
	}

	scanned, scanErr := agent.ScanSessionDisplayIndex(sessionPath)
	if scanErr == nil {
		if !identityKnown || scanned.ContentDigest == identity.DigestHex {
			if identityKnown {
				scanned.Revision = identity.Revision
				scanned.RevisionKnown = identity.RevisionKnown
			}
			if writeErr := agent.WriteSessionDisplayIndex(store.SessionDisplayIndex(sessionPath), scanned); writeErr != nil {
				slog.Debug("desktop: history display index republish failed", "path", sessionPath, "err", writeErr)
			}
			slice, pageErr := a.pageHistorySliceSource(coldHistorySliceSource(sessionPath, scanned), req, resolver, sessionPlannerDisplayTurns(sessionDir, sessionPath), nil, sessionPath)
			if pageErr != nil {
				return emptyHistorySlice(), pageErr
			}
			slice.Source = "scan"
			return slice, nil
		}
	}

	if eventInfo, statErr := os.Stat(store.SessionEventLog(sessionPath)); statErr == nil && !eventInfo.IsDir() && eventInfo.Size() > 0 {
		messages, state, repairable, loadErr := agent.LoadSessionDisplayMessages(sessionPath)
		if loadErr != nil {
			return emptyHistorySlice(), loadErr
		}
		src := newInMemoryHistorySliceSource(strings.TrimSuffix(filepath.Base(sessionPath), ".jsonl"), messages, resolver, state, true)
		src.cacheKey = historyDerivedSourceKey(sessionPath, src)
		slice, pageErr := a.pageHistorySliceSource(src, req, resolver, sessionPlannerDisplayTurns(sessionDir, sessionPath), nil, sessionPath)
		if pageErr != nil {
			return emptyHistorySlice(), pageErr
		}
		slice.Source = "event-log"
		if repairable {
			a.kickHistoryReadModelRepair(sessionPath)
		}
		return slice, nil
	}

	if scanErr != nil {

		messages, state, repairable, loadErr := agent.LoadSessionDisplayMessages(sessionPath)
		if loadErr != nil {
			return emptyHistorySlice(), errors.Join(scanErr, loadErr)
		}
		src := newInMemoryHistorySliceSource(strings.TrimSuffix(filepath.Base(sessionPath), ".jsonl"), messages, resolver, state, true)
		src.cacheKey = historyDerivedSourceKey(sessionPath, src)
		slice, pageErr := a.pageHistorySliceSource(src, req, resolver, sessionPlannerDisplayTurns(sessionDir, sessionPath), nil, sessionPath)
		if pageErr != nil {
			return emptyHistorySlice(), pageErr
		}
		slice.Source = "scan"
		if repairable {
			a.kickHistoryReadModelRepair(sessionPath)
		}
		return slice, nil
	}
	if identityKnown {
		if scanned.ContentDigest == identity.DigestHex {
			scanned.Revision = identity.Revision
			scanned.RevisionKnown = identity.RevisionKnown
		}
	}
	if writeErr := agent.WriteSessionDisplayIndex(store.SessionDisplayIndex(sessionPath), scanned); writeErr != nil {
		slog.Debug("desktop: history display index republish failed", "path", sessionPath, "err", writeErr)
	}
	slice, pageErr := a.pageHistorySliceSource(coldHistorySliceSource(sessionPath, scanned), req, resolver, sessionPlannerDisplayTurns(sessionDir, sessionPath), nil, sessionPath)
	if pageErr != nil {
		return emptyHistorySlice(), pageErr
	}
	slice.Source = "scan"
	return slice, nil
}

// historyIndexTimestampValid is the cheap file-generation guard for
// cold offset reads. Save/scan publish the index atomically after the transcript
// is complete. Equal timestamps are ambiguous on coarse filesystems, so cold
// readers verify the streamed digest before trusting offsets. The migration
// probe may accept equality because it never reads indexed content.
func historyIndexTimestampValid(indexPath, sessionPath string, transcriptInfo os.FileInfo, idx *agent.SessionDisplayIndex, verifyEqual bool) bool {
	indexInfo, err := os.Stat(indexPath)
	if err != nil || indexInfo.IsDir() || idx == nil {
		return false
	}
	if indexInfo.ModTime().After(transcriptInfo.ModTime()) {
		return true
	}
	if !indexInfo.ModTime().Equal(transcriptInfo.ModTime()) {
		return false
	}
	if !verifyEqual {
		return true
	}
	scanned, err := agent.ScanSessionDisplayIndex(sessionPath)
	matches := err == nil && scanned.TranscriptSize == idx.TranscriptSize && scanned.MessageCount == idx.MessageCount && scanned.ContentDigest == idx.ContentDigest
	if matches {
		if err := agent.WriteSessionDisplayIndex(indexPath, idx); err != nil {
			slog.Debug("desktop: history display index tie republish failed", "path", sessionPath, "err", err)
		}
	}
	return matches
}

func coldHistorySliceSource(sessionPath string, idx *agent.SessionDisplayIndex) *historySliceSource {
	n := idx.MessageCount
	turns := make([]int, n)
	roles := make([]provider.Role, n)
	for i, e := range idx.Entries {
		turns[i] = e.AuthoredTurn
		roles[i] = e.Role
	}
	revision := idx.Revision
	if !idx.RevisionKnown {
		revision = 0
	}
	epoch := 0
	if idx.RevisionKnown {
		epoch = int(idx.Revision)
	}
	src := &historySliceSource{
		sessionID:  strings.TrimSuffix(filepath.Base(sessionPath), ".jsonl"),
		total:      n,
		turns:      turns,
		roles:      roles,
		totalTurns: idx.AuthoredTurns,
		revision:   revision,
		revKnown:   idx.RevisionKnown,
		digest:     idx.ContentDigest,
		epoch:      epoch,
		fetch: func(lo, hi int) ([]provider.Message, error) {
			if lo < 0 || hi < lo || hi > len(idx.Entries) {
				return nil, fmt.Errorf("history display index window [%d,%d) is out of range", lo, hi)
			}
			return readSessionMessagesAtOffsets(sessionPath, idx.Entries[lo:hi])
		},
		windowBytes: func(lo, hi int) int64 {
			if lo >= hi || hi > len(idx.Entries) {
				return 0
			}
			last := idx.Entries[hi-1]
			return last.Offset + last.Length - idx.Entries[lo].Offset
		},
	}
	src.cacheKey = historyDerivedSourceKey(sessionPath, src)
	return src
}

// readSessionMessagesAtOffsets decodes the message lines for entries, whose
// byte ranges are contiguous in the transcript, with one read.
func readSessionMessagesAtOffsets(sessionPath string, entries []agent.DisplayIndexEntry) ([]provider.Message, error) {
	out := make([]provider.Message, 0, len(entries))
	if len(entries) == 0 {
		return out, nil
	}
	f, err := os.Open(sessionPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	spanStart := entries[0].Offset
	spanEnd := entries[len(entries)-1].Offset + entries[len(entries)-1].Length
	spanLength := spanEnd - spanStart
	if spanLength < 0 {
		return nil, fmt.Errorf("invalid history display index span")
	}
	if spanLength <= historySliceColdWindowBytes {
		buf := make([]byte, int(spanLength))
		if _, err := f.ReadAt(buf, spanStart); err != nil {
			return nil, err
		}
		for _, e := range entries {
			start := e.Offset - spanStart
			end := start + e.Length
			if start < 0 || end < start || end > int64(len(buf)) {
				return nil, fmt.Errorf("history display index line %d escapes fetched span", e.Index)
			}
			var m provider.Message
			if err := json.Unmarshal(buf[int(start):int(end)], &m); err != nil {
				return nil, fmt.Errorf("decode session transcript line %d: %w", e.Index, err)
			}
			out = append(out, m)
		}
		return out, nil
	}

	for _, e := range entries {
		var m provider.Message
		dec := json.NewDecoder(io.NewSectionReader(f, e.Offset, e.Length))
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("decode oversized session transcript line %d: %w", e.Index, err)
		}
		out = append(out, m)
	}
	return out, nil
}

// historySessionLooksEventFormat reports whether the transcript is a legacy
// event-record log rather than a provider-message transcript: event records
// carry kind/type and no role.
func historySessionLooksEventFormat(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 1<<20).ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {

		return false
	}
	if len(line) == 0 || err != nil && len(line) == 0 {
		return false
	}
	var probe struct {
		Role provider.Role `json:"role"`
		Kind string        `json:"kind"`
		Type string        `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return false
	}
	return probe.Role == "" && (probe.Kind != "" || probe.Type != "")
}

// pageHistorySliceSource cuts one page from src. Pages are suffixes of the
// candidate window: the turn budget picks the oldest message that may be
// included, conversion runs forward (its cross-message state flows forward),
// and the entry/byte budgets drop the oldest whole-message groups — so cuts
// always land on message boundaries.
func (a *App) pageHistorySliceSource(src *historySliceSource, req HistorySliceRequest, resolver func(string) string, plannerTurns []plannerDisplayTurn, checkpointTurns map[int]int, sessionPath string) (HistorySlice, error) {
	cursor, err := decodeHistorySliceCursor(req.Cursor)

	hasCursor := req.Cursor != "" && err == nil
	if hasCursor && !src.identityMatches(cursor.Revision, cursor.RevKnown, cursor.Digest) {
		return staleHistorySlice(src.revision, src.revKnown, src.digest), nil
	}
	hi := src.total
	if hasCursor && cursor.Before < hi {
		hi = cursor.Before
	}
	page := HistorySlice{
		Entries:       []HistoryEntry{},
		TotalTurns:    src.totalTurns,
		Revision:      src.revision,
		RevisionKnown: src.revKnown,
		Digest:        src.digest,
	}
	if hi <= 0 || src.total == 0 {
		return page, nil
	}

	newestTurn := src.turns[hi-1]
	oldestTurn := 0
	if newestTurn > 0 {
		oldestTurn = max(newestTurn-req.Turns+1, 1)
	}

	candidateLo := sort.Search(hi, func(i int) bool { return src.turns[i] >= oldestTurn })
	if oldestTurn <= 1 {

		candidateLo = 0
	}

	if src.windowBytes != nil {
		for candidateLo < hi-1 && src.windowBytes(candidateLo, hi) > historySliceColdWindowBytes {
			candidateLo++
		}
	}

	window, fetchErr := src.fetch(candidateLo, hi)
	if fetchErr != nil {
		return emptyHistorySlice(), fetchErr
	}
	if len(window) != hi-candidateLo {
		return emptyHistorySlice(), fmt.Errorf("history window length %d, want %d", len(window), hi-candidateLo)
	}
	window = historyWindowWithPersistedTimes(window, sessionPath, countRoleBefore(src.roles, candidateLo, provider.RoleUser))
	todoArgs := map[string]string{}
	if historyWindowContainsTodoWrite(window) {
		var todoErr error
		todoArgs, todoErr = a.historyDerived.todoArgs(src.cacheKey, func() (map[string]string, error) {
			return historyTodoArgsForSource(src)
		})
		if todoErr != nil {
			return emptyHistorySlice(), todoErr
		}
	}
	toolResults := historyToolResultsByID(window)
	if err := extendHistoryToolResults(src, window, hi, toolResults); err != nil {
		return emptyHistorySlice(), err
	}

	type entryGroup struct {
		msgIndex int
		entries  []HistoryEntry
		bytes    int
	}
	groups := []entryGroup{}
	entryCount, byteCount := 0, 0
	state := newHistoryMessageConvertState(plannerTurns)
	if err := primeHistoryPlannerState(src, state, candidateLo, resolver); err != nil {
		return emptyHistorySlice(), err
	}
	for i := candidateLo; i < hi; i++ {
		m := window[i-candidateLo]
		rows := state.convertHistoryMessage(i, m, resolver, checkpointTurns, todoArgs, toolResults)
		if len(rows) == 0 {
			continue
		}
		g := entryGroup{msgIndex: i, entries: make([]HistoryEntry, 0, len(rows))}
		for sub, row := range rows {
			entry := newHistoryEntry(src, fmt.Sprintf("s%s:r%d:m%d:o%d", src.sessionID, src.epoch, i, sub), i, sub, row)
			g.bytes += entry.inlineBytes()
			g.entries = append(g.entries, entry)
		}
		groups = append(groups, g)
		entryCount += len(g.entries)
		byteCount += g.bytes

		for len(groups) > 1 && (entryCount > req.Entries || byteCount > req.Bytes) {
			entryCount -= len(groups[0].entries)
			byteCount -= groups[0].bytes
			groups = groups[1:]
		}
	}

	pageStart := candidateLo
	if len(groups) > 0 {
		pageStart = groups[0].msgIndex
	}
	for _, g := range groups {
		page.Entries = append(page.Entries, g.entries...)
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
	page.HasOlder = pageStart > 0
	if page.HasOlder {
		page.NextCursor = encodeHistorySliceCursor(historySliceCursor{
			V:        1,
			Revision: src.revision,
			RevKnown: src.revKnown,
			Digest:   src.digest,
			Before:   pageStart,
		})
	}
	return page, nil
}
