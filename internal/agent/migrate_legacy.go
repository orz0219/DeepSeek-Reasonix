package agent

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/provider"
)

// legacyEvent is the subset of the v0.x typed event stream (<name>.events.jsonl)
// needed to rebuild the conversation: user input, assistant turns (text + tool
// calls), and tool results. All other event types (UI, plan, checkpoint, …) are
// presentation and carry no message state.
type legacyEvent struct {
	Type             string           `json:"type"`
	Text             string           `json:"text"`             // user.message
	Content          string           `json:"content"`          // model.final
	ReasoningContent string           `json:"reasoningContent"` // model.final
	ToolCalls        []legacyToolCall `json:"toolCalls"`        // model.final
	CallID           string           `json:"callId"`           // tool.result
	Output           string           `json:"output"`           // tool.result
}

type legacyToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
		ThoughtSignature string `json:"thought_signature"`
	} `json:"function"`
}

// isMessageFormat returns true when path's first non-whitespace bytes look like
// a JSON object with a "role" key — i.e. the v1+ message format — as opposed to
// the legacy event-log format whose first key is "id".
func isMessageFormat(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var buf [64]byte
	n, _ := f.Read(buf[:])
	s := strings.TrimLeft(string(buf[:n]), " \t\r\n")
	return strings.HasPrefix(s, `{"role":`)
}

// isNativeSessionEventLog reports whether the file at an .events.jsonl path is
// a native session event log (as opposed to a legacy v0.x event transcript
// that happens to share the suffix).
func isNativeSessionEventLog(path string) bool {
	sessionPath := strings.TrimSuffix(path, ".events.jsonl") + ".jsonl"
	probe, err := probeSessionEventLog(sessionPath)
	return err == nil && probe.native && probe.size > 0
}

func saveNativeSessionCopy(src, dst string) error {
	session, err := LoadSession(src)
	if err != nil {
		return err
	}
	return session.SaveIfAbsent(dst)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// legacyAssistantMsg is the minimal JSON shape needed to detect and transform
// the legacy nested-function tool-call format into the flat format the Go
// version expects.
type legacyAssistantMsg struct {
	Role      string          `json:"role"`
	ToolCalls json.RawMessage `json:"tool_calls"`
}

// legacyToolCallObj matches the OpenAI-style tool call where name and
// arguments live under a "function" key.
type legacyToolCallObj struct {
	ID       string `json:"id"`
	Function struct {
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
		ThoughtSignature string `json:"thought_signature"`
	} `json:"function"`
}

// transformAndCopyJsonl copies src to dst, flattening any legacy nested-function
// tool calls into the flat name/arguments format the v1+ message format uses.
// Non-assistant messages and messages without tool_calls pass through unchanged.
func transformAndCopyJsonl(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".session.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmpPath)
		}
	}()
	enc := json.NewEncoder(tmp)
	dec := json.NewDecoder(in)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}

			break
		}
		var m legacyAssistantMsg
		if err := json.Unmarshal(raw, &m); err != nil || m.Role != "assistant" || len(m.ToolCalls) == 0 {

			if err := enc.Encode(raw); err != nil {
				return err
			}
			continue
		}
		// Try legacy nested-function format; if it doesn't match, pass through.
		var legacyCalls []legacyToolCallObj
		if err := json.Unmarshal(m.ToolCalls, &legacyCalls); err != nil || len(legacyCalls) == 0 {
			if err := enc.Encode(raw); err != nil {
				return err
			}
			continue
		}

		flatCalls := make([]provider.ToolCall, len(legacyCalls))
		for i, tc := range legacyCalls {
			flatCalls[i] = provider.ToolCall{
				ID:               tc.ID,
				Name:             tc.Function.Name,
				Arguments:        tc.Function.Arguments,
				ThoughtSignature: tc.Function.ThoughtSignature,
			}
		}
		// Re-serialize the full message with flat tool_calls. We only modify
		// tool_calls; all other fields (content, reasoning_content, etc.) stay
		// as-is by round-tripping through a map.
		var full map[string]json.RawMessage
		if err := json.Unmarshal(raw, &full); err != nil {
			if err := enc.Encode(raw); err != nil {
				return err
			}
			continue
		}
		b, err := json.Marshal(flatCalls)
		if err != nil {
			if err := enc.Encode(raw); err != nil {
				return err
			}
			continue
		}
		full["tool_calls"] = b
		if err := enc.Encode(full); err != nil {
			return err
		}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := publishFileNoReplace(tmpPath, dst); err != nil {
		return err
	}
	ok = true
	return nil
}

// readLegacyMeta loads the v0.x sidecar for a session; missing or corrupt
// sidecars yield the zero value (session routes to the global dir, untitled).
func readLegacyMeta(srcDir, base string) legacyMeta {
	var m legacyMeta
	b, err := fileencoding.ReadFileUTF8(filepath.Join(srcDir, base+".meta.json"))
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	m.Workspace = strings.TrimSpace(m.Workspace)
	m.Summary = strings.TrimSpace(m.Summary)
	return m
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// publishFileNoReplace atomically publishes a completed sibling temp file
// without replacing a destination another startup/import writer created.
// The temp and destination share a directory, so a hard link is atomic and
// portable across the filesystems Reasonix supports.
func publishFileNoReplace(tmp, dst string) error {
	if err := linkFileNoReplace(tmp, dst); err != nil {
		return err
	}
	return os.Remove(tmp)
}

func linkFileNoReplace(src, dst string) error {
	if err := os.Link(src, dst); err != nil {
		if os.IsExist(err) {
			return os.ErrExist
		}
		return err
	}
	return nil
}

// recordImportedTitle stores the legacy summary as the session's display title
// in the dir's .titles.json — the same map the desktop sidebar reads
// (desktop/sessions.go). Existing titles are never overwritten.
func recordImportedTitle(destDir, base, summary string) {
	if summary == "" {
		return
	}
	path := filepath.Join(destDir, ".titles.json")
	titles := map[string]string{}
	if b, err := fileencoding.ReadFileUTF8(path); err == nil {
		_ = json.Unmarshal(b, &titles)
	}
	key := base + ".jsonl"
	if titles[key] != "" {
		return
	}
	titles[key] = summary
	b, err := json.MarshalIndent(titles, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func importMarkerExists(destDir, marker string) bool {
	if strings.TrimSpace(destDir) == "" || strings.TrimSpace(marker) == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(destDir, marker))
	return err == nil
}

func writeImportMarkers(destDir string, markers ...string) {
	if strings.TrimSpace(destDir) == "" {
		return
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return
	}
	seen := map[string]bool{}
	for _, marker := range markers {
		marker = strings.TrimSpace(marker)
		if marker == "" || seen[marker] {
			continue
		}
		seen[marker] = true
		_ = os.WriteFile(filepath.Join(destDir, marker), nil, 0o644)
	}
}

// rehomeStrandedSessions copies project-scoped sessions that were written into
// the flat global dir AFTER the one-time routing pass already ran — the
// signature of a user who downgraded to a pre-routing build (which writes every
// session to the flat dir regardless of workspace) and then upgraded again
// (#4666). Without this, the routing marker hides those sessions from the
// desktop sidebar forever, even though they are sitting in the flat dir.
//
// It is deliberately conservative:
//   - Only sessions whose mtime is newer than the marker (the last migration
//     watermark) are considered, so a session the user imported and then
//     deleted is never resurrected.
//   - Only sessions that explicitly name a still-existing workspace — via a v1+
//     branch-meta sidecar with scope=project, or a v0.x .meta.json — are moved.
//     Flat global sessions (CLI conversations, the desktop's global tab) carry
//     no workspace and are left untouched.
//   - It never modifies the source files; the destination is written via the
//     same transform-and-copy path the full passes use, and the branch-meta
//     sidecar is copied alongside so the sidebar shows the right title/topic.
//
// The marker mtime is advanced to now after a successful scan so the next boot
// does not re-walk the same files.
func rehomeStrandedSessions(srcDir, globalDest, marker string, projectDir func(string) string) (int, error) {
	if projectDir == nil {
		return 0, nil
	}
	markerPath := filepath.Join(globalDest, marker)
	markerInfo, err := os.Stat(markerPath)
	if err != nil {
		return 0, nil
	}
	watermark := markerInfo.ModTime()

	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return 0, nil
	}
	imported := 0
	hadCopyFailure := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") ||
			strings.HasSuffix(name, ".events.jsonl") || strings.HasSuffix(name, ".jsonl.bak") {
			continue
		}
		base := strings.TrimSuffix(name, ".jsonl")
		if strings.HasPrefix(base, "subagent-") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || !info.ModTime().After(watermark) {
			continue
		}
		srcPath := filepath.Join(srcDir, name)
		if !isMessageFormat(srcPath) {
			continue
		}
		destDir, summary := strandedSessionDestDir(srcDir, srcPath, base, projectDir)
		if destDir == "" || sameDirPath(destDir, globalDest) {
			continue
		}
		dest := filepath.Join(destDir, name)
		if _, err := os.Stat(dest); err == nil {
			if err := copySubagentArtifacts(srcDir, destDir, base); err != nil {
				hadCopyFailure = true
			}
			continue
		}
		if err := transformAndCopyJsonl(srcPath, dest); err != nil {
			hadCopyFailure = true
			continue
		}
		_ = os.Chtimes(dest, info.ModTime(), info.ModTime())
		copyBranchMetaSidecar(srcPath, dest)
		if err := copySubagentArtifacts(srcDir, destDir, base); err != nil {
			hadCopyFailure = true
		}
		recordImportedTitle(destDir, base, summary)
		imported++
	}

	if !hadCopyFailure {
		now := time.Now()
		_ = os.Chtimes(markerPath, now, now)
	}
	return imported, nil
}

// strandedSessionDestDir resolves the per-project session dir a flat-dir session
// belongs to, preferring the v1+ branch-meta sidecar and falling back to the
// v0.x .meta.json. It returns "" when the session is global or names a workspace
// that no longer exists on disk. The second return is the display summary, if any.
func strandedSessionDestDir(srcDir, srcPath, base string, projectDir func(string) string) (string, string) {
	if meta, ok, err := LoadBranchMeta(srcPath); err == nil && ok {
		if meta.DefaultScope() == "project" && meta.WorkspaceRoot != "" && dirExists(meta.WorkspaceRoot) {
			if d := projectDir(meta.WorkspaceRoot); d != "" {
				return d, strings.TrimSpace(meta.TopicTitle)
			}
		}

		if meta.Scope != "" {
			return "", ""
		}
	}
	legacy := readLegacyMeta(srcDir, base)
	if legacy.Workspace != "" && dirExists(legacy.Workspace) {
		if d := projectDir(legacy.Workspace); d != "" {
			return d, legacy.Summary
		}
	}
	return "", ""
}

// copyBranchMetaSidecar copies <src>.meta to <dst>.meta when present so the
// desktop sidebar keeps the session's title, topic, and tree position. Best
// effort: a missing or unreadable sidecar just means the session shows with a
// generated title.
func copyBranchMetaSidecar(srcPath, dstPath string) {
	b, err := os.ReadFile(BranchMetaPath(srcPath))
	if err != nil {
		return
	}
	dstMeta := BranchMetaPath(dstPath)
	if err := os.MkdirAll(filepath.Dir(dstMeta), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(dstMeta), ".branch.*.tmp")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return
	}
	if err := os.Rename(tmpPath, dstMeta); err != nil {
		os.Remove(tmpPath)
	}
}

func copySubagentArtifacts(srcSessionDir, dstSessionDir, parentSession string) error {
	if sameDirPath(srcSessionDir, dstSessionDir) {
		return nil
	}
	artifacts, err := ListSubagentsByParent(srcSessionDir, parentSession)
	if err != nil {
		return err
	}
	var errs []error
	dstSubagentDir := filepath.Join(dstSessionDir, "subagents")
	for _, artifact := range artifacts {
		for _, src := range []string{artifact.SessionPath, artifact.MetaPath} {
			if err := copyFileIfExists(src, filepath.Join(dstSubagentDir, filepath.Base(src))); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func copyFileIfExists(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return nil
	}
	if _, err := os.Stat(dst); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".subagent.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return err
	}
	_ = os.Chtimes(dst, info.ModTime(), info.ModTime())
	return nil
}

// sameDirPath reports whether two directory paths resolve to the same location.
func sameDirPath(a, b string) bool {
	ca, cb := filepath.Clean(a), filepath.Clean(b)
	if ca == cb {
		return true
	}
	if aa, err := filepath.Abs(ca); err == nil {
		if bb, err := filepath.Abs(cb); err == nil {
			return aa == bb
		}
	}
	return false
}

// reconstructSession folds the chronological event stream into the provider
// message sequence. Tool results inherit their tool name from the assistant turn
// that issued the call (the v0.x result event carries only the call id).
func reconstructSession(path string) ([]provider.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var msgs []provider.Message
	toolName := map[string]string{}
	dec := json.NewDecoder(f)
	for {
		var e legacyEvent
		if err := dec.Decode(&e); err != nil {
			if !errors.Is(err, io.EOF) {
				return msgs, nil
			}
			break
		}
		switch e.Type {
		case "user.message":
			if e.Text != "" {
				msgs = append(msgs, provider.Message{Role: provider.RoleUser, Content: e.Text})
			}
		case "model.final":
			m := provider.Message{Role: provider.RoleAssistant, Content: e.Content, ReasoningContent: e.ReasoningContent}
			for _, tc := range e.ToolCalls {
				m.ToolCalls = append(m.ToolCalls, provider.ToolCall{
					ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
					ThoughtSignature: tc.Function.ThoughtSignature,
				})
				toolName[tc.ID] = tc.Function.Name
			}
			msgs = append(msgs, m)
		case "tool.result":
			msgs = append(msgs, provider.Message{Role: provider.RoleTool, ToolCallID: e.CallID, Name: toolName[e.CallID], Content: e.Output})
		}
	}
	return msgs, nil
}
