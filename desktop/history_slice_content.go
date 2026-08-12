package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/store"
)

// HistoryContentForTab returns one chunk of a ref-replaced field's full
// value. The entry is re-resolved through the same window machinery; when the
// session's revision/digest moved past the ref, Stale is set so the frontend
// reloads.
func (a *App) HistoryContentForTab(tabID string, ref HistoryContentRef, chunkIndex int) HistoryContentChunk {
	out := HistoryContentChunk{EntryID: ref.EntryID, Field: ref.Field, Chunk: max(chunkIndex, 0)}
	msgIndex, sub, legacyRow, ok := parseHistoryEntryID(ref.EntryID)
	if !ok {
		out.Done = true
		return out
	}
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
	if ctrl != nil {
		if p := ctrl.SessionPath(); strings.TrimSpace(p) != "" {
			sessionPath = p
			sessionDir = controllerSessionDir(ctrl)
		}
	}
	if strings.TrimSpace(sessionPath) == "" {
		out.Done = true
		return out
	}
	sessionID := strings.TrimSuffix(filepath.Base(sessionPath), ".jsonl")
	if entryIDSession(ref.EntryID) != sessionID {
		out.Stale = true
		return out
	}

	var value string
	var found bool
	if legacyRow >= 0 {
		value, found = a.legacyHistoryFieldValue(sessionPath, sessionDir, legacyRow, ref)
	} else if ctrl != nil {
		var stale bool
		value, found, stale = a.liveHistoryFieldValue(ctrl, sessionDir, sessionPath, msgIndex, sub, ref)
		if stale {
			out.Stale = true
			return out
		}
	} else {
		var stale bool
		value, found, stale = a.coldHistoryFieldValue(sessionDir, sessionPath, msgIndex, sub, ref)
		if stale {
			out.Stale = true
			return out
		}
	}
	if !found {

		out.Stale = true
		return out
	}
	if len(value) != ref.Size {
		out.Stale = true
		return out
	}
	data, chunks := historyContentChunkAt(value, chunkIndex)
	out.Chunks = chunks
	out.Data = data
	out.Done = chunkIndex >= chunks-1
	return out
}

// parseHistoryEntryID parses s<id>:r<epoch>:m<msgIndex>:o<sub> and the legacy
// event-format s<id>:r<epoch>:e<row>:o0 form.
func parseHistoryEntryID(entryID string) (msgIndex, sub, legacyRow int, ok bool) {
	legacyRow = -1
	parts := strings.Split(entryID, ":")
	if len(parts) != 4 {
		return 0, 0, -1, false
	}
	if _, err := fmt.Sscanf(parts[2], "m%d", &msgIndex); err == nil {
		if _, err := fmt.Sscanf(parts[3], "o%d", &sub); err != nil {
			return 0, 0, -1, false
		}
		return msgIndex, sub, -1, true
	}
	if _, err := fmt.Sscanf(parts[2], "e%d", &legacyRow); err == nil {
		return 0, 0, legacyRow, true
	}
	return 0, 0, -1, false
}

func entryIDSession(entryID string) string {
	rest := strings.SplitN(entryID, ":", 2)
	if len(rest) != 2 {
		return ""
	}
	return strings.TrimPrefix(rest[0], "s")
}

// liveHistoryFieldValue re-resolves one entry's field from the live session.
func (a *App) liveHistoryFieldValue(ctrl control.SessionAPI, sessionDir, sessionPath string, msgIndex, sub int, ref HistoryContentRef) (string, bool, bool) {
	wc, ok := ctrl.(historyWindowController)
	if !ok {
		return "", false, true
	}
	ps, psOK := wc.SessionPersistedState()
	revKnown := psOK && ps.RevisionKnown
	revision := int64(0)
	digest := ""
	if psOK {
		revision = ps.Revision
		digest = ps.DigestHex
	}
	if !psOK || revKnown != ref.RevKnown || revision != ref.Revision || digest != ref.Digest {
		return "", false, true
	}
	resolver := sessionDisplayResolver(sessionDir, sessionPath)
	src, _ := a.liveHistorySliceSource(ctrl, sessionPath, resolver)
	if src == nil {
		return "", false, true
	}
	return a.historyFieldValueForSource(src, msgIndex, sub, ref, resolver, sessionPlannerDisplayTurns(sessionDir, sessionPath), ctrl.CheckpointTurnsByMessageIndex())
}

// coldHistoryFieldValue re-resolves one entry's field through the same
// authoritative source selection as HistorySliceForTab. It never trusts a
// stale checkpoint merely because the requested message's old offset exists.
func (a *App) coldHistoryFieldValue(sessionDir, sessionPath string, msgIndex, sub int, ref HistoryContentRef) (string, bool, bool) {
	absPath, _, err := validateSessionPath(sessionDir, sessionPath)
	if err != nil {
		return "", false, true
	}
	info, err := os.Stat(absPath)
	if err != nil {
		return "", false, true
	}
	resolver := sessionDisplayResolver(sessionDir, absPath)
	idx, idxErr := agent.LoadSessionDisplayIndex(store.SessionDisplayIndex(absPath))
	identity, identityKnown, identityErr := agent.SessionContentIdentity(absPath)
	if identityErr != nil {
		return "", false, true
	}
	valid := idxErr == nil && idx != nil && idx.TranscriptSize == info.Size() && historyIndexTimestampValid(store.SessionDisplayIndex(absPath), absPath, info, idx, true)
	if valid && identityKnown {
		valid = agent.ValidateSessionDisplayIndex(idx, identity.Revision, identity.RevisionKnown, identity.Digest, info.Size())
	} else if valid {
		valid = !idx.RevisionKnown
	}
	var src *historySliceSource
	if valid {
		src = coldHistorySliceSource(absPath, idx)
	} else if scanned, scanErr := agent.ScanSessionDisplayIndex(absPath); scanErr == nil && (!identityKnown || scanned.ContentDigest == identity.DigestHex) {
		if identityKnown {
			scanned.Revision = identity.Revision
			scanned.RevisionKnown = identity.RevisionKnown
		}
		_ = agent.WriteSessionDisplayIndex(store.SessionDisplayIndex(absPath), scanned)
		src = coldHistorySliceSource(absPath, scanned)
	} else {
		messages, state, repairable, loadErr := agent.LoadSessionDisplayMessages(absPath)
		if loadErr != nil {
			return "", false, true
		}
		src = newInMemoryHistorySliceSource(strings.TrimSuffix(filepath.Base(absPath), ".jsonl"), messages, resolver, state, true)
		src.cacheKey = historyDerivedSourceKey(absPath, src)
		if repairable {
			a.kickHistoryReadModelRepair(absPath)
		}
	}
	return a.historyFieldValueForSource(src, msgIndex, sub, ref, resolver, sessionPlannerDisplayTurns(sessionDir, absPath), nil)
}

func (a *App) historyFieldValueForSource(src *historySliceSource, msgIndex, sub int, ref HistoryContentRef, resolver func(string) string, plannerTurns []plannerDisplayTurn, checkpointTurns map[int]int) (string, bool, bool) {
	if src == nil || !src.identityMatches(ref.Revision, ref.RevKnown, ref.Digest) || msgIndex < 0 || msgIndex >= src.total {
		return "", false, true
	}
	msgs, err := src.fetch(msgIndex, msgIndex+1)
	if err != nil || len(msgs) != 1 {
		return "", false, true
	}
	toolResults := historyToolResultsByID(msgs)
	if err := extendHistoryToolResults(src, msgs, msgIndex+1, toolResults); err != nil {
		return "", false, true
	}
	todoArgs := map[string]string{}
	if historyWindowContainsTodoWrite(msgs) {
		todoArgs, err = a.historyDerived.todoArgs(src.cacheKey, func() (map[string]string, error) {
			return historyTodoArgsForSource(src)
		})
		if err != nil {
			return "", false, true
		}
	}
	state := newHistoryMessageConvertState(plannerTurns)
	if err := primeHistoryPlannerState(src, state, msgIndex, resolver); err != nil {
		return "", false, true
	}
	rows := state.convertHistoryMessage(msgIndex, msgs[0], resolver, checkpointTurns, todoArgs, toolResults)
	if sub < 0 || sub >= len(rows) {
		return "", false, true
	}
	value, found := historyEntryFieldValue(&rows[sub], ref.Field, ref.ToolCallID)
	return value, found, false
}

// historyEntryFieldValue reads one field of a converted row by ref field name.
func historyEntryFieldValue(m *HistoryMessage, field, toolCallID string) (string, bool) {
	switch field {
	case "content":
		return m.Content, true
	case "reasoning":
		return m.Reasoning, true
	case "submitText":
		return m.SubmitText, true
	case "detail":
		return m.Detail, true
	case "code":
		return m.Code, true
	case "summary":
		return m.Summary, true
	case "archive":
		return m.Archive, true
	case "toolResultError":
		return m.ToolResultError, true
	case "toolArguments", "toolSubject", "toolSummary", "toolDiff":
		for i := range m.ToolCalls {
			if m.ToolCalls[i].ID != toolCallID {
				continue
			}
			switch field {
			case "toolArguments":
				return m.ToolCalls[i].Arguments, true
			case "toolSubject":
				return m.ToolCalls[i].Subject, true
			case "toolSummary":
				return m.ToolCalls[i].Summary, true
			case "toolDiff":
				return m.ToolCalls[i].Diff, true
			}
		}
		return "", false
	}
	return "", false
}
