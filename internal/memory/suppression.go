// Suppression: workspace-level "don't remember" records. When a user says
// "don't save this" or "just for now", the suppression prevents future
// automatic consolidation from re-creating the same subject.
package memory

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
)

const suppressionFile = ".suppressions.json"

// Suppression is one "don't remember this" record, keyed by subject.
type Suppression struct {
	SubjectKey   string    `json:"subject_key"`
	SuppressedAt time.Time `json:"suppressed_at"`
	SessionID    string    `json:"session_id,omitempty"`
	Reason       string    `json:"reason,omitempty"`
}

// suppressionPath returns the workspace-level suppression file path.
func (s Store) suppressionPath() string {
	if s.Dir == "" {
		return ""
	}
	return filepath.Join(s.Dir, suppressionFile)
}

// LoadSuppressions reads the workspace-level suppression list from disk.
// Returns nil when the file does not exist or the store is disabled.
func (s Store) LoadSuppressions() []Suppression {
	path := s.suppressionPath()
	if path == "" {
		return nil
	}
	raw, err := fileencoding.ReadFileUTF8(path)
	if err != nil {
		return nil
	}
	var list []Suppression
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	return list
}

// SaveSuppression appends a suppression record and persists it. Duplicate
// subject keys are not added twice.
func (s Store) SaveSuppression(sub Suppression) error {
	path := s.suppressionPath()
	if path == "" {
		return nil
	}
	existing := s.LoadSuppressions()
	key := NormalizeSubjectKey(sub.SubjectKey)
	for _, e := range existing {
		if NormalizeSubjectKey(e.SubjectKey) == key {
			return nil // already suppressed
		}
	}
	if sub.SuppressedAt.IsZero() {
		sub.SuppressedAt = time.Now().UTC()
	}
	sub.SubjectKey = key
	existing = append(existing, sub)
	data, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(path, data, 0o644)
}

// IsSuppressed reports whether a subject key is workspace-level suppressed.
func (s Store) IsSuppressed(subjectKey string) bool {
	key := NormalizeSubjectKey(subjectKey)
	if key == "" {
		return false
	}
	for _, sub := range s.LoadSuppressions() {
		if NormalizeSubjectKey(sub.SubjectKey) == key {
			return true
		}
	}
	return false
}

// suppressionPatterns are multi-language phrases that signal "don't remember".
var suppressionPatterns = []string{
	"不要记", "不要保存", "不要记录", "别记", "别保存",
	"仅本次", "只限本次", "只是临时", "临时的", "先别记录",
	"忘掉之前这个", "这不是长期", "不需要记",
	"don't remember", "don't save", "just for now",
	"temporary", "not long term", "do not save", "no need to remember",
	"forget this", "skip memory",
}

// DetectSuppression scans user messages for suppression phrases and returns
// detected suppression hints. Each hint is a freeform reason string; the
// consolidator maps them to subject keys during candidate validation.
func DetectSuppression(messages []string) []string {
	var hints []string
	seen := map[string]bool{}
	for _, msg := range messages {
		lower := strings.ToLower(strings.TrimSpace(msg))
		if lower == "" {
			continue
		}
		for _, pattern := range suppressionPatterns {
			if strings.Contains(lower, pattern) {
				if !seen[pattern] {
					hints = append(hints, pattern)
					seen[pattern] = true
				}
				break
			}
		}
	}
	return hints
}
