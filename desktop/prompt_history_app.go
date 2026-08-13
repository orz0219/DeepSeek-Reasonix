package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/store"
)

type promptHistoryRequest struct {
	Nonce  string `json:"nonce,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	legacy bool
}

type promptHistoryCursor struct {
	Nonce   string `json:"n"`
	Session int    `json:"s"`
	Offset  int    `json:"o"`
}

type promptHistoryTape struct {
	nonce       string
	dir         string
	currentPath string
	displays    sessionDisplayMap
	sessions    []promptHistorySessionFile
	loaded      map[string][]PromptHistoryEntry
}

// ScanPromptHistory returns the next prompt-history tape segment. The request is
// a JSON string so the Wails binding stays one-argument while the protocol can
// carry a cursor and page limit. Older clients may still pass a bare nonce; that
// path keeps the old cache-hit behavior.
func (a *App) ScanPromptHistory(rawRequest string) (PromptHistoryResult, error) {
	req := parsePromptHistoryRequest(rawRequest)
	dir := a.activeSessionDir()
	sessionPath := a.activeSessionPath(dir)

	a.promptHistoryMu.Lock()
	tape, err := a.promptHistoryTapeForLocked(dir, sessionPath)
	if err != nil {
		a.promptHistoryMu.Unlock()
		return PromptHistoryResult{}, err
	}
	if req.legacy && req.Nonce != "" && req.Nonce == tape.nonce {
		a.promptHistoryMu.Unlock()
		return PromptHistoryResult{Entries: nil, Nonce: req.Nonce}, nil
	}
	result := tape.readOlder(req.Cursor, promptHistoryLimit(req.Limit))
	a.promptHistoryMu.Unlock()
	return result, nil
}

func parsePromptHistoryRequest(raw string) promptHistoryRequest {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return promptHistoryRequest{}
	}
	if strings.HasPrefix(raw, "{") {
		var req promptHistoryRequest
		if err := json.Unmarshal([]byte(raw), &req); err == nil {
			return req
		}
	}
	return promptHistoryRequest{Nonce: raw, legacy: true}
}

func promptHistoryLimit(limit int) int {
	if limit <= 0 {
		return promptHistoryPageLimit
	}
	if limit > promptHistoryMaxPageLimit {
		return promptHistoryMaxPageLimit
	}
	return limit
}

func (a *App) promptHistoryTapeForLocked(dir, sessionPath string) (*promptHistoryTape, error) {
	currentPath := ""
	if path, _, err := validateSessionPath(dir, sessionPath); err == nil {
		currentPath = path
	}
	if a.promptHistoryTape != nil && a.promptHistoryTape.dir == dir && a.promptHistoryTape.currentPath == currentPath {
		return a.promptHistoryTape, nil
	}
	tape, err := newPromptHistoryTape(dir, currentPath)
	if err != nil {
		return nil, err
	}
	a.promptHistoryTape = tape
	return tape, nil
}

func newPromptHistoryTape(dir, currentPath string) (*promptHistoryTape, error) {
	tape := &promptHistoryTape{
		nonce:       fmt.Sprintf("%d", time.Now().UnixNano()),
		dir:         dir,
		currentPath: currentPath,
		displays:    loadSessionDisplays(dir),
		loaded:      map[string][]PromptHistoryEntry{},
	}
	sessions, err := promptHistorySessionFiles(dir)
	if err != nil {
		return nil, err
	}
	if currentPath != "" {
		currentPath = filepath.Clean(currentPath)
		currentSession := promptHistorySessionFile{}
		currentIndex := -1
		for i, session := range sessions {
			if filepath.Clean(session.path) == currentPath {
				currentSession = session
				currentIndex = i
				break
			}
		}
		if currentIndex >= 0 {
			sessions = append([]promptHistorySessionFile{currentSession}, append(sessions[:currentIndex], sessions[currentIndex+1:]...)...)
		} else if info, err := os.Stat(currentPath); err == nil && !info.IsDir() {
			sessions = append([]promptHistorySessionFile{{
				path: currentPath,
			}}, sessions...)
		}
	}
	tape.sessions = sessions
	return tape, nil
}

func (t *promptHistoryTape) readOlder(cursor string, limit int) PromptHistoryResult {
	c := promptHistoryCursor{Nonce: t.nonce}
	if decoded, ok := decodePromptHistoryCursor(cursor); ok && decoded.Nonce == t.nonce {
		c = decoded
	}
	if c.Session < 0 {
		c.Session = 0
	}
	if c.Offset < 0 {
		c.Offset = 0
	}

	out := make([]PromptHistoryEntry, 0, limit)
	sessionIndex := c.Session
	offset := c.Offset
	for sessionIndex < len(t.sessions) && len(out) < limit {
		entries, err := t.entriesForSession(sessionIndex)
		if err != nil || offset >= len(entries) {
			sessionIndex++
			offset = 0
			continue
		}

		end := min(len(entries), offset+limit-len(out))
		out = append(out, entries[offset:end]...)
		offset = end
		if offset >= len(entries) && len(out) < limit {
			sessionIndex++
			offset = 0
		}
	}

	if sessionIndex < len(t.sessions) {
		if entries, ok := t.loaded[t.sessions[sessionIndex].path]; ok && offset >= len(entries) {
			sessionIndex++
			offset = 0
		}
	}
	hasOlder := sessionIndex < len(t.sessions)
	olderCursor := ""
	if hasOlder {
		olderCursor = encodePromptHistoryCursor(promptHistoryCursor{Nonce: t.nonce, Session: sessionIndex, Offset: offset})
	}
	return PromptHistoryResult{Entries: out, Nonce: t.nonce, OlderCursor: olderCursor, HasOlder: hasOlder}
}

func (t *promptHistoryTape) entriesForSession(index int) ([]PromptHistoryEntry, error) {
	if index < 0 || index >= len(t.sessions) {
		return nil, nil
	}
	path := t.sessions[index].path
	if entries, ok := t.loaded[path]; ok {
		return entries, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		t.loaded[path] = nil
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := scanPromptHistoryFile(path, info, sessionDisplayResolverFromMap(t.displays, path))
	if err != nil {
		t.loaded[path] = nil
		return nil, err
	}
	t.loaded[path] = entries
	return entries, nil
}

func encodePromptHistoryCursor(cursor promptHistoryCursor) string {
	b, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePromptHistoryCursor(value string) (promptHistoryCursor, bool) {
	if strings.TrimSpace(value) == "" {
		return promptHistoryCursor{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return promptHistoryCursor{}, false
	}
	var cursor promptHistoryCursor
	if err := json.Unmarshal(b, &cursor); err != nil {
		return promptHistoryCursor{}, false
	}
	return cursor, true
}

func scanPromptHistoryFile(path string, info os.FileInfo, resolveUserContent func(string) string) ([]PromptHistoryEntry, error) {
	entries, err := collectPromptHistoryEntries(path, info, resolveUserContent)
	if err != nil {
		return nil, err
	}
	sortPromptHistoryNewestFirst(entries)
	return entries, nil
}

type promptHistorySessionFile struct {
	path string
}

func promptHistorySessionFiles(dir string) ([]promptHistorySessionFile, error) {
	infos, err := agent.ListSessionOrder(dir)
	if err != nil {
		return nil, err
	}
	sessions := make([]promptHistorySessionFile, 0, len(infos))
	for _, info := range infos {
		sessions = append(sessions, promptHistorySessionFile{path: info.Path})
	}
	return sessions, nil
}

func promptHistoryEntryNewer(a, b PromptHistoryEntry) bool {
	if a.At != b.At {
		return a.At > b.At
	}
	if a.SessionPath != b.SessionPath {
		return a.SessionPath > b.SessionPath
	}
	return a.Turn > b.Turn
}

func sortPromptHistoryNewestFirst(entries []PromptHistoryEntry) {
	sort.Slice(entries, func(i, j int) bool {
		return promptHistoryEntryNewer(entries[i], entries[j])
	})
}

func collectPromptHistoryEntries(path string, info os.FileInfo, resolveUserContent func(string) string) ([]PromptHistoryEntry, error) {
	var out []PromptHistoryEntry
	emit := func(entry PromptHistoryEntry) {
		out = append(out, entry)
	}

	if handled, err := collectEventLogUserPrompts(path, info, resolveUserContent, emit); handled {
		return out, err
	}
	err := collectJSONLUserPrompts(path, info, resolveUserContent, emit)
	return out, err
}

func collectEventLogUserPrompts(path string, info os.FileInfo, resolveUserContent func(string) string, emit func(PromptHistoryEntry)) (bool, error) {
	logPath := store.SessionEventLog(path)
	if logPath == "" {
		return false, nil
	}
	if logInfo, err := os.Stat(logPath); err != nil || logInfo.IsDir() || logInfo.Size() == 0 {
		return false, nil
	}
	users, err := agent.LoadSessionUserMessages(path)
	if err != nil {
		return true, err
	}
	fallbackAt := promptHistoryFallbackMillis(path, info)
	turn := 0
	for _, user := range users {
		text := strings.TrimSpace(resolveUserContent(strings.TrimSpace(user.Text)))
		if text == "" || control.IsSyntheticUserMessage(text) {
			continue
		}
		at := fallbackAt
		if !user.At.IsZero() {
			at = user.At.UnixMilli()
		}
		emit(PromptHistoryEntry{
			Text:        text,
			At:          at,
			SessionPath: path,
			Turn:        turn,
		})
		turn++
	}
	return true, nil
}

func collectJSONLUserPrompts(path string, info os.FileInfo, resolveUserContent func(string) string, emit func(PromptHistoryEntry)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	fallbackAt := promptHistoryFallbackMillis(path, info)

	dec := json.NewDecoder(f)
	turn := 0
	for {
		var rec previewEventRecord
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil
		}

		text := ""
		kindOrType := strings.TrimSpace(rec.Kind)
		if kindOrType == "" {
			kindOrType = strings.TrimSpace(rec.Type)
		}
		if kindOrType == "user.message" {
			text = strings.TrimSpace(rec.Text)
		} else if strings.TrimSpace(rec.Role) == "user" {
			text = strings.TrimSpace(rec.Content)
		}
		if text != "" {
			text = resolveUserContent(text)
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			if control.IsSyntheticUserMessage(text) {
				continue
			}
			at := fallbackAt
			if eventAt, ok := promptHistoryEventMillis(rec); ok {
				at = eventAt
			}
			entry := PromptHistoryEntry{
				Text:        text,
				At:          at,
				SessionPath: path,
				Turn:        turn,
			}
			emit(entry)
			turn++
		}
	}
	return nil
}

func promptHistoryFallbackMillis(path string, info os.FileInfo) int64 {
	if meta, ok, err := agent.LoadBranchMeta(path); err == nil && ok && !meta.UpdatedAt.IsZero() {
		return meta.UpdatedAt.UnixMilli()
	}
	if info != nil {
		return info.ModTime().UnixMilli()
	}
	return 0
}

func promptHistoryEventMillis(rec previewEventRecord) (int64, bool) {
	for _, raw := range []json.RawMessage{
		rec.TS,
		rec.Time,
		rec.Timestamp,
		rec.CreatedAt,
		rec.CreatedAtSnake,
		rec.UpdatedAt,
		rec.UpdatedAtSnake,
	} {
		if at, ok := parseJSONTimestampMillis(raw); ok {
			return at, true
		}
	}
	return 0, false
}

func parseJSONTimestampMillis(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0, false
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return normalizeTimestampMillis(n)
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return normalizeTimestampMillisFloat(f)
		}
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UnixMilli(), true
		}
		return 0, false
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return 0, false
	}
	if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return normalizeTimestampMillis(i)
	}
	if f, err := strconv.ParseFloat(n.String(), 64); err == nil {
		return normalizeTimestampMillisFloat(f)
	}
	return 0, false
}

func normalizeTimestampMillis(v int64) (int64, bool) {
	if v <= 0 {
		return 0, false
	}
	switch {
	case v >= 1_000_000_000_000_000_000:
		return v / 1_000_000, true
	case v >= 1_000_000_000_000_000:
		return v / 1_000, true
	case v >= 100_000_000_000:
		return v, true
	case v >= 1_000_000_000:
		return v * 1_000, true
	default:
		return 0, false
	}
}

func normalizeTimestampMillisFloat(v float64) (int64, bool) {
	if v <= 0 {
		return 0, false
	}
	switch {
	case v >= 1_000_000_000_000_000_000:
		return int64(v / 1_000_000), true
	case v >= 1_000_000_000_000_000:
		return int64(v / 1_000), true
	case v >= 100_000_000_000:
		return int64(v), true
	case v >= 1_000_000_000:
		return int64(v * 1_000), true
	default:
		return 0, false
	}
}

const (
	promptHistoryPageLimit    = 50
	promptHistoryMaxPageLimit = 200
)
