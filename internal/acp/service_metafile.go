package acp

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/jobs"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/store"
)

type acpSessionMeta struct {
	SessionID         string                    `json:"sessionId"`
	Cwd               string                    `json:"cwd"`
	Model             string                    `json:"model,omitempty"`
	EffortOverride    *string                   `json:"effortOverride,omitempty"`
	RuntimeProfile    string                    `json:"runtimeProfile,omitempty"`
	ToolApprovalMode  string                    `json:"toolApprovalMode,omitempty"`
	CollaborationMode string                    `json:"collaborationMode,omitempty"`
	Title             string                    `json:"title,omitempty"`
	CreatedAt         time.Time                 `json:"createdAt"`
	UpdatedAt         time.Time                 `json:"updatedAt"`
	Status            *persistedStatusTelemetry `json:"status,omitempty"`
	// ActiveTranscript, when set on the id-keyed sidecar, is the basename of
	// the transcript this session currently lives in: a snapshot recovery
	// moved the live session onto a recovery branch and left this redirect
	// behind so restart-time lookups (resolveTranscriptPath) follow the
	// session instead of reopening the pre-recovery file.
	ActiveTranscript string `json:"activeTranscript,omitempty"`
}

func metadataForLoadedSession(path, id, cwd string, history []provider.Message) acpSessionMeta {
	now := time.Now().UTC()
	meta, ok, err := loadACPMeta(path)
	if err != nil || !ok {
		meta = acpSessionMeta{
			SessionID: id,
			Cwd:       cwd,
			Title:     titleFromHistory(history),
			CreatedAt: now,
			UpdatedAt: now,
		}
		if info, statErr := os.Stat(path); statErr == nil {
			meta.CreatedAt = info.ModTime().UTC()
			meta.UpdatedAt = info.ModTime().UTC()
		}
	}
	if meta.SessionID == "" {
		meta.SessionID = id
	}
	if cwd != "" {
		meta.Cwd = cwd
	}
	if meta.Title == "" {
		meta.Title = titleFromHistory(history)
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = meta.CreatedAt
	}
	return meta
}

func loadACPMeta(sessionPath string) (acpSessionMeta, bool, error) {
	path := acpMetaPath(sessionPath)
	if path == "" {
		return acpSessionMeta{}, false, nil
	}
	b, err := fileencoding.ReadFileUTF8(path)
	if err != nil {
		if os.IsNotExist(err) {
			return acpSessionMeta{}, false, nil
		}
		return acpSessionMeta{}, false, err
	}
	var meta acpSessionMeta
	if err := json.Unmarshal(b, &meta); err != nil {
		return acpSessionMeta{}, false, fmt.Errorf("decode ACP session metadata %s: %w", path, err)
	}
	return meta, true, nil
}

func saveACPMeta(sessionPath string, meta acpSessionMeta) error {
	path := acpMetaPath(sessionPath)
	if path == "" {
		return nil
	}
	now := time.Now().UTC()
	if meta.SessionID == "" {
		meta.SessionID = sessionIDFromTranscript(sessionPath)
	}
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = now
	}
	if meta.UpdatedAt.IsZero() {
		meta.UpdatedAt = meta.CreatedAt
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".acp-session.*.tmp")
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
	return fileutil.ReplaceFile(tmpPath, path)
}

func listACPMetas(dir string) ([]acpSessionMeta, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := []acpSessionMeta{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".acp.json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".acp.json")
		sessionPath := transcriptPath(dir, id)
		if agent.IsCleanupPending(sessionPath) {
			continue
		}
		if !sessionFileExists(sessionPath) {
			continue
		}
		meta, ok, err := loadACPMeta(sessionPath)
		if err != nil || !ok {
			continue
		}
		if meta.SessionID == "" {
			meta.SessionID = id
		}
		if meta.Cwd == "" {
			continue
		}
		out = append(out, meta)
	}
	return out, nil
}

func sessionFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func acpMetaPath(sessionPath string) string {
	if sessionPath == "" {
		return ""
	}
	return strings.TrimSuffix(sessionPath, filepath.Ext(sessionPath)) + ".acp.json"
}

func sessionIDFromTranscript(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return base
}

// listMetaBeats reports whether a should represent its session id in
// session/list over b. A meta without an ActiveTranscript redirect is the
// session's live transcript and always beats a redirect sidecar; between two
// of the same kind the later UpdatedAt wins.
func listMetaBeats(a, b acpSessionMeta) bool {
	aRedirect := strings.TrimSpace(a.ActiveTranscript) != ""
	bRedirect := strings.TrimSpace(b.ActiveTranscript) != ""
	if aRedirect != bRedirect {
		return !aRedirect
	}
	return a.UpdatedAt.After(b.UpdatedAt)
}

func sessionInfoMatchesCwd(info SessionInfo, filter string) bool {
	if filter == "" {
		return true
	}
	return filepath.Clean(info.Cwd) == filepath.Clean(filter)
}

func titleFromHistory(history []provider.Message) string {
	for _, m := range history {
		if m.Role == provider.RoleUser {
			if title := previewTitle(m.Content); title != "" {
				return title
			}
		}
	}
	return ""
}

func previewTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len([]rune(text)) <= 80 {
		return text
	}
	runes := []rune(text)
	return string(runes[:77]) + "..."
}

func validateSessionID(method, id string) error {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return &RPCError{Code: ErrInvalidParams, Message: method + ": missing sessionId"}
	}
	if trimmed != id || trimmed == "." || trimmed == ".." || !isSafeSessionID(trimmed) {
		return &RPCError{Code: ErrInvalidParams, Message: method + ": invalid sessionId"}
	}
	return nil
}

func isSafeSessionID(id string) bool {
	for _, r := range id {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		if r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func parseSessionUpdatedAt(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func deleteSessionFiles(sessionPath string) error {
	paths := []string{
		sessionPath,
		acpMetaPath(sessionPath),
	}
	paths = append(paths, store.SessionSidecarFiles(sessionPath)...)
	for _, path := range paths {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if dir := checkpointPath(sessionPath); dir != "" {
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := agent.DeleteSubagentsByParent(filepath.Dir(sessionPath), agent.BranchID(sessionPath)); err != nil {
		return err
	}
	if err := jobs.RemoveArtifacts(sessionPath); err != nil {
		return err
	}
	return agent.ClearCleanupPending(sessionPath)
}

// ReconcileCleanupPending retries delayed ACP session cleanup left by a previous
// process, including ACP's own metadata sidecar.
func ReconcileCleanupPending(dir string) error {
	return agent.ReconcileCleanupPending(dir, func(item agent.CleanupPendingInfo) error {
		return deleteSessionFiles(item.SessionPath)
	})
}

func delayedDeleteSessionFiles(sessionPath string, destroy control.SessionDestroyHandle) {
	if destroy.WaitAll != nil {
		destroy.WaitAll()
	}
	if err := deleteSessionFiles(sessionPath); err != nil {
		slog.Warn("acp: delayed session delete failed", "path", sessionPath, "err", err)
	}
	if destroy.Finish != nil {
		destroy.Finish()
	}
}

func checkpointPath(sessionPath string) string {
	return store.SessionCheckpointDir(sessionPath)
}

// mcpSpecs converts ACP MCP server declarations to plugin.Spec.
func mcpSpecs(in []MCPServerSpec, cwd string) ([]plugin.Spec, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]plugin.Spec, 0, len(in))
	for _, m := range in {
		typ := strings.ToLower(strings.TrimSpace(m.Type))
		if typ == "" {
			typ = "stdio"
		}
		if strings.TrimSpace(m.Name) == "" {
			return nil, fmt.Errorf("MCP server name is required")
		}
		switch typ {
		case "stdio":
			if strings.TrimSpace(m.Command) == "" {
				return nil, fmt.Errorf("MCP server %q command is required", m.Name)
			}
		case "http", "streamable-http", "streamable_http", "sse":
			if strings.TrimSpace(m.URL) == "" {
				return nil, fmt.Errorf("MCP server %q url is required", m.Name)
			}
			if typ != "sse" {
				typ = "http"
			}
		default:
			return nil, fmt.Errorf("MCP server %q uses unsupported transport %q", m.Name, m.Type)
		}
		out = append(out, plugin.Spec{
			Name:          strings.TrimSpace(m.Name),
			Type:          typ,
			Command:       strings.TrimSpace(m.Command),
			Args:          append([]string(nil), m.Args...),
			Env:           mapString(m.Env),
			URL:           strings.TrimSpace(m.URL),
			Headers:       mapString(m.Headers),
			Dir:           cwd,
			WorkspaceRoot: cwd,
		})
	}
	return out, nil
}

func mapString(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

// newSessionID returns a random RFC 4122 v4 UUID string used to address a session.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
