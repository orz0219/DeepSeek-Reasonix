package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"reasonix/internal/provider"
	"reasonix/internal/store"
)

// SessionInfo summarises a saved session for the --resume picker: where it is on
// disk, when it was created/last active, the first user message as a preview, and
// a rough turn count.
type SessionInfo struct {
	Path           string
	CreatedAt      time.Time
	LastActivityAt time.Time
	ModTime        time.Time // compatibility alias for LastActivityAt
	Preview        string
	Turns          int
	CountsKnown    bool
	Scope          string
	WorkspaceRoot  string
	TopicID        string
	TopicTitle     string
	CustomTitle    string
	Recovered      bool
	RecoveryReason string
	RecoveryDigest string
	ParentID       string
}

// SessionOrderInfo is the lightweight sidecar/mtime ordering record shared by
// session pickers and prompt-history navigation. It intentionally avoids reading
// JSONL content; callers that need previews can layer that on afterwards.
type SessionOrderInfo struct {
	Path              string
	CreatedAt         time.Time
	LastActivityAt    time.Time
	ModTime           time.Time // compatibility alias for LastActivityAt
	Scope             string
	WorkspaceRoot     string
	TopicID           string
	TopicTitle        string
	CustomTitle       string
	Recovered         bool
	RecoveryReason    string
	RecoveryDigest    string
	ParentID          string
	RecoveryPreferred bool
	// Turns and Preview are the cached listing fields from the sidecar; SchemaVersion
	// >= agent.BranchMetaCountsVersion means they were recorded from content and can
	// be trusted (even Turns == 0). ListSessions uses them to skip the whole-file decode.
	Turns         int
	Preview       string
	SchemaVersion int
	// Revision and ContentDigest bind a listing backfill to the transcript
	// generation it decoded. They are sidecar-only compare-and-apply guards and
	// are not exposed through SessionInfo.
	Revision      int64
	ContentDigest string
}

// ListSessionOrder returns every *.jsonl session under dir in the same
// most-recently-active order used by ListSessions, using only file metadata and
// branch sidecars. A missing directory is not an error.
func ListSessionOrder(dir string) ([]SessionOrderInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []SessionOrderInfo
	for _, e := range entries {
		if e.IsDir() || !store.IsSessionTranscriptName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(dir, e.Name())
		if !IsVisibleSession(full) {
			continue
		}
		contentMod := SessionContentModTime(full)
		if contentMod.IsZero() {
			contentMod = info.ModTime()
		}
		createdAt := info.ModTime()
		lastActivityAt := contentMod
		scope := "global"
		workspaceRoot := ""
		topicID := ""
		topicTitle := ""
		customTitle := ""
		recovered := false
		recoveryReason := ""
		recoveryDigest := ""
		parentID := ""
		recoveryPreferred := false
		turns := 0
		preview := ""
		schemaVersion := 0
		revision := int64(0)
		contentDigest := ""
		if meta, ok, err := LoadBranchMeta(full); err == nil && ok {
			if !meta.CreatedAt.IsZero() {
				createdAt = meta.CreatedAt
			}
			if !meta.UpdatedAt.IsZero() {
				lastActivityAt = meta.UpdatedAt
			}
			scope = meta.DefaultScope()
			workspaceRoot = meta.WorkspaceRoot
			topicID = meta.TopicID
			topicTitle = meta.TopicTitle
			customTitle = meta.CustomTitle
			recovered = meta.Recovered
			recoveryReason = meta.RecoveryReason
			recoveryDigest = meta.RecoveryDigest
			parentID = meta.ParentID
			recoveryPreferred = RecoveryPreferenceCurrent(full, meta)
			turns = meta.Turns
			preview = meta.Preview
			schemaVersion = meta.SchemaVersion
			revision = meta.Revision
			contentDigest = meta.ContentDigest
		}

		if !recovered && LooksLikeRecoveryFilename(full) {
			recovered = true
			if parentID == "" {
				if parent, ok := RecoveryFilenameParentID(full); ok {
					parentID = parent
				}
			}
		}
		out = append(out, SessionOrderInfo{
			Path:              full,
			CreatedAt:         createdAt,
			LastActivityAt:    lastActivityAt,
			ModTime:           lastActivityAt,
			Scope:             scope,
			WorkspaceRoot:     workspaceRoot,
			TopicID:           topicID,
			TopicTitle:        topicTitle,
			CustomTitle:       customTitle,
			Recovered:         recovered,
			RecoveryReason:    recoveryReason,
			RecoveryDigest:    recoveryDigest,
			ParentID:          parentID,
			RecoveryPreferred: recoveryPreferred,
			Turns:             turns,
			Preview:           preview,
			SchemaVersion:     schemaVersion,
			Revision:          revision,
			ContentDigest:     contentDigest,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastActivityAt.Equal(out[j].LastActivityAt) {
			return out[i].Path < out[j].Path
		}
		return out[i].LastActivityAt.After(out[j].LastActivityAt)
	})
	return out, nil
}

// ListSessions returns every non-empty *.jsonl session under dir,
// most-recently-active first, each with a preview line so the picker can show
// something the user recognises. It never decodes a transcript: legacy counts
// remain explicitly unknown until the session catalog's single repair worker
// validates them. A missing directory is not an error.
func ListSessions(dir string) ([]SessionInfo, error) {
	ordered, err := ListSessionOrder(dir)
	if err != nil {
		return nil, err
	}
	var out []SessionInfo
	for _, session := range ordered {
		preview, turns := session.Preview, session.Turns
		if sessionListingCountsNeedRefresh(session.SchemaVersion, turns) {
			if !sessionArtifactsHaveContent(session.Path) {
				continue
			}
			if strings.TrimSpace(preview) == "" {
				preview = "History is being indexed — " + filepath.Base(session.Path)
			}
			out = append(out, sessionInfoFromOrder(session, preview, turns, false))
			continue
		}
		if turns == 0 {

			continue
		}
		out = append(out, sessionInfoFromOrder(session, preview, turns, true))
	}
	return out, nil
}

// SessionPreview returns the same preview and user-turn count used by
// ListSessions for one session file.
func SessionPreview(path string) (string, int) {
	return previewSession(path)
}

// SessionPreviewFromMessages computes the same preview line and user-turn count
// as previewSession, but from an in-memory message slice. Session.Save writes
// exactly these messages to the .jsonl, so this is byte-for-byte equivalent to
// decoding the file — letting the autosave path persist the counts into the
// sidecar without a disk read.
func SessionPreviewFromMessages(msgs []provider.Message) (string, int) {
	first := ""
	turns := 0
	for _, m := range msgs {
		if m.Role == provider.RoleUser && IsUserAuthoredTurn(UserMessageText(m)) {
			turns++
			if first == "" {
				first = truncatePreview(previewProse(UserMessageText(m)))
			}
		}
	}
	return first, turns
}

// previewSession returns the first user message (truncated) and the number of
// user-role messages so the picker can show "5 turns · 'help me debug the…'".
// Errors are swallowed — a malformed file just shows up with an empty preview.
func previewSession(path string) (string, int) {
	preview, turns, _ := previewSessionWithError(path)
	return preview, turns
}

// previewProse drops the leading @file references a prompt opens with so the
// preview shows what was asked rather than a row of paths. A prompt that is
// nothing but references keeps them — there is nothing else to show.
func previewProse(s string) string {
	rest := strings.TrimLeft(s, " \t")
	for strings.HasPrefix(rest, "@") {
		end := strings.IndexAny(rest, " \t\r\n")
		if end < 0 {
			return s
		}
		next := strings.TrimLeft(rest[end:], " \t")
		if strings.TrimSpace(next) == "" {
			return s
		}
		rest = next
	}
	if rest == "" {
		return s
	}
	return rest
}

// truncatePreview clamps a preview line to 80 runes with an ellipsis, matching
// what the pickers render.
func truncatePreview(s string) string {
	if r := []rune(s); len(r) > 80 {
		return string(r[:77]) + "…"
	}
	return s
}

// ContinueSessionPath returns where a conversation carried into a rebuilt
// controller (model switch, config change) should keep auto-saving: its existing
// file when it has one, so the continued session stays a single file instead of
// the old one being orphaned as an identical duplicate (#2807). A session with no
// file yet gets a fresh path; "" when persistence is disabled.
func ContinueSessionPath(prevPath, dir, model string) string {
	if prevPath != "" {
		return prevPath
	}
	if dir == "" {
		return ""
	}
	return NewSessionPath(dir, model)
}

// NewSessionPath returns the path to use for a fresh session, namespaced by
// the model so the filename hints at what the conversation was with. dir is
// typically config.SessionDir().
func NewSessionPath(dir, model string) string {
	safe := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "<", "-", ">", "-", "\"", "-", "|", "-", "?", "-", "*", "-").Replace(model)
	if safe == "" {
		safe = "session"
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%s.jsonl", time.Now().UTC().Format("20060102-150405.000000000"), safe))
}
