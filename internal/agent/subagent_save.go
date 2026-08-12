package agent

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
)

func (s *SubagentStore) MarkRunning(run *SubagentRun) error {
	if s == nil || run == nil || run.Ref == "" {
		return nil
	}
	if s.parentDestroyed(run) {
		return nil
	}
	meta := run.Meta
	meta.Status = SubagentRunning
	meta.UpdatedAt = time.Now().UTC()
	return s.saveMeta(meta)
}

func (s *SubagentStore) SaveCompleted(run *SubagentRun) error {
	if s == nil || run == nil || run.Ref == "" {
		return nil
	}
	if s.parentDestroyed(run) {
		return nil
	}
	if err := s.ensureBranchCreatedAt(run); err != nil {
		return err
	}
	if err := run.Session.Save(s.sessionPath(run.Ref)); err != nil {
		return err
	}
	meta := run.Meta
	meta.Status = SubagentCompleted
	meta.UpdatedAt = time.Now().UTC()
	run.Meta = meta
	return s.saveMeta(meta)
}

func (s *SubagentStore) SaveFailed(run *SubagentRun) error {
	if s == nil || run == nil || run.Ref == "" {
		return nil
	}
	if s.parentDestroyed(run) {
		return nil
	}

	branchErr := s.ensureBranchCreatedAt(run)
	var sessionErr error
	if run.Session != nil {
		sessionErr = run.Session.Save(s.sessionPath(run.Ref))
	}
	meta := run.Meta
	meta.Status = SubagentFailed
	meta.UpdatedAt = time.Now().UTC()
	run.Meta = meta
	return errors.Join(branchErr, sessionErr, s.saveMeta(meta))
}

// ensureBranchCreatedAt seeds the session list sidecar before the first
// transcript save. Subagent transcripts are written only on completion, so
// Session.Save would otherwise backfill BranchMeta.CreatedAt with the save
// moment (completion time). The real start time already lives on run.Meta.
func (s *SubagentStore) ensureBranchCreatedAt(run *SubagentRun) error {
	if s == nil || run == nil || run.Ref == "" {
		return nil
	}
	path := s.sessionPath(run.Ref)
	if _, ok, err := LoadBranchMeta(path); err != nil {
		return err
	} else if ok {
		return nil
	}
	created := run.Meta.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	return SaveBranchMetaPreserveUpdated(path, BranchMeta{
		ID:        BranchID(path),
		CreatedAt: created,
	})
}

func (s *SubagentStore) LoadMeta(ref string) (SubagentMeta, error) {
	var meta SubagentMeta
	if !validSubagentRef(ref) {
		return meta, fmt.Errorf("invalid subagent reference %q", ref)
	}
	data, err := fileencoding.ReadFileUTF8(s.metaPath(ref))
	if err != nil {
		return meta, fmt.Errorf("load subagent metadata %q: %w", ref, err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, &subagentMetaDecodeError{ref: ref, err: err}
	}
	return meta, nil
}

func validateMeta(meta SubagentMeta, spec SubagentSpec) error {
	if meta.Status == SubagentRunning {
		return fmt.Errorf("subagent reference %q is still in progress", meta.Ref)
	}
	if meta.Status == SubagentFailed {
		return fmt.Errorf("subagent reference %q failed and cannot be continued", meta.Ref)
	}
	if meta.Status == SubagentInterrupted {
		return fmt.Errorf("subagent reference %q was interrupted by a previous shutdown or crash and cannot be continued or forked; run a fresh subagent instead", meta.Ref)
	}
	want := metaFromSpec(meta.Ref, meta.Status, meta.CreatedAt, meta.UpdatedAt, spec)
	switch {
	case meta.Kind != want.Kind:
		return fmt.Errorf("subagent reference %q has kind %q, want %q", meta.Ref, meta.Kind, want.Kind)
	case meta.Name != want.Name:
		return fmt.Errorf("subagent reference %q has name %q, want %q", meta.Ref, meta.Name, want.Name)
	case meta.WorkspaceRoot != want.WorkspaceRoot:
		return fmt.Errorf("subagent reference %q belongs to workspace %q, current workspace is %q", meta.Ref, meta.WorkspaceRoot, want.WorkspaceRoot)
	case meta.SystemPromptHash != want.SystemPromptHash:
		return fmt.Errorf("subagent reference %q uses a different subagent persona; run a fresh subagent to use the current persona", meta.Ref)
	case !sameStrings(meta.ToolScope, want.ToolScope):
		return fmt.Errorf("subagent reference %q uses a different tool scope", meta.Ref)
	case meta.ToolSchemaHash != want.ToolSchemaHash:
		return fmt.Errorf("subagent reference %q uses different tool schemas", meta.Ref)
	case meta.Model != want.Model || meta.Effort != want.Effort:
		return fmt.Errorf("subagent reference %q uses model/effort %q/%q, current run would use %q/%q", meta.Ref, meta.Model, meta.Effort, want.Model, want.Effort)
	}
	return nil
}

func requireParentSession(spec SubagentSpec) error {
	if strings.TrimSpace(spec.ParentSession) == "" {
		return fmt.Errorf("subagent transcript parent session is required")
	}
	return nil
}

func validateContinueOwner(meta SubagentMeta, spec SubagentSpec) error {
	current := strings.TrimSpace(spec.ParentSession)
	owner := strings.TrimSpace(meta.ParentSession)
	if owner == current {
		return nil
	}
	if owner == "" {
		return fmt.Errorf("subagent reference %q has no parent session; run a fresh subagent instead", meta.Ref)
	}
	return fmt.Errorf("subagent reference %q belongs to parent session %q, current parent session is %q", meta.Ref, owner, current)
}

func (s *SubagentStore) validateForkOwner(meta SubagentMeta, spec SubagentSpec) error {
	current := strings.TrimSpace(spec.ParentSession)
	owner := strings.TrimSpace(meta.ParentSession)
	if owner == current {
		return nil
	}
	if owner == "" {
		return fmt.Errorf("subagent reference %q has no parent session; run a fresh subagent instead", meta.Ref)
	}
	ok, err := s.isAncestorSession(owner, current)
	if err != nil {
		return fmt.Errorf("subagent reference %q belongs to parent session %q, but current parent session %q lineage could not be verified: %w", meta.Ref, owner, current, err)
	}
	if ok {
		return nil
	}
	return fmt.Errorf("subagent reference %q belongs to parent session %q, which is not in current parent session %q lineage", meta.Ref, owner, current)
}

func (s *SubagentStore) isAncestorSession(ancestor, current string) (bool, error) {
	ancestor = strings.TrimSpace(ancestor)
	current = strings.TrimSpace(current)
	if ancestor == "" || current == "" {
		return false, nil
	}
	seen := map[string]bool{}
	for cursor := current; cursor != ""; {
		if seen[cursor] {
			return false, fmt.Errorf("cycle at session %q", cursor)
		}
		seen[cursor] = true

		metaPath, valid := s.parentSessionPath(cursor)
		if !valid {
			return false, fmt.Errorf("invalid session identifier %q", cursor)
		}
		meta, ok, err := LoadBranchMeta(metaPath)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, fmt.Errorf("missing branch metadata for session %q", cursor)
		}
		if strings.TrimSpace(meta.ID) != cursor {
			return false, fmt.Errorf("branch metadata for session %q declares id %q", cursor, meta.ID)
		}
		if cursor == ancestor {
			return true, nil
		}
		parent := strings.TrimSpace(meta.ParentID)
		cursor = parent
	}
	return false, nil
}

func (s *SubagentStore) lock(ref string) (func(), error) {
	if !validSubagentRef(ref) {
		return nil, fmt.Errorf("invalid subagent reference %q", ref)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked[ref] {
		return nil, fmt.Errorf("subagent reference %q is already running; retry after it finishes", ref)
	}
	s.locked[ref] = true
	return func() {
		s.mu.Lock()
		delete(s.locked, ref)
		s.mu.Unlock()
	}, nil
}

func (s *SubagentStore) newRef() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "sa_" + time.Now().UTC().Format("20060102_150405_000000000") + "_" + hex.EncodeToString(b[:]), nil
}

func (s *SubagentStore) sessionPath(ref string) string { return filepath.Join(s.dir, ref+".jsonl") }
func (s *SubagentStore) metaPath(ref string) string    { return filepath.Join(s.dir, ref+".meta.json") }

func (s *SubagentStore) saveMeta(meta SubagentMeta) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(s.dir, ".subagent-meta.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return fileutil.ReplaceFile(tmpPath, s.metaPath(meta.Ref))
}

func (s *SubagentStore) parentDestroyed(run *SubagentRun) bool {
	if s == nil || s.destroyed == nil || run == nil {
		return false
	}
	return s.destroyed(run.Meta.ParentSession)
}

func validSubagentRef(ref string) bool {
	if !strings.HasPrefix(ref, "sa_") {
		return false
	}
	for _, r := range ref {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func bytesHash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
