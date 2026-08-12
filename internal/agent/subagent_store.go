package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/store"
	"reasonix/internal/tool"
)

type SubagentStatus string

const (
	SubagentRunning     SubagentStatus = "running"
	SubagentCompleted   SubagentStatus = "completed"
	SubagentFailed      SubagentStatus = "failed"
	SubagentInterrupted SubagentStatus = "interrupted"
)

// SubagentMeta is the sidecar for a persisted sub-agent transcript. It captures
// the execution identity that must stay stable for continuation/fork.
type SubagentMeta struct {
	Ref              string         `json:"ref"`
	CreatedAt        time.Time      `json:"createdAt"`
	UpdatedAt        time.Time      `json:"updatedAt"`
	Status           SubagentStatus `json:"status"`
	Kind             string         `json:"kind"` // task | skill
	Name             string         `json:"name"`
	WorkspaceRoot    string         `json:"workspaceRoot"`
	ParentSession    string         `json:"parentSession,omitempty"`
	ParentToolCallID string         `json:"parentToolCallId,omitempty"`
	ForkedFrom       string         `json:"forkedFrom,omitempty"`
	SystemPromptHash string         `json:"systemPromptHash"`
	ToolScope        []string       `json:"toolScope"`
	ToolSchemaHash   string         `json:"toolSchemaHash"`
	Model            string         `json:"model"`
	Effort           string         `json:"effort"`
	// Capsule records what context this run was given; CapsuleHash is its
	// stable identity for comparing two runs.
	Capsule     ContextCapsule `json:"capsule"`
	CapsuleHash string         `json:"capsuleHash"`
}

// subagentMetaDecodeError distinguishes malformed metadata content from file
// I/O failures. Cleanup may safely skip one undecodable record, but storage
// errors must remain visible because they can affect every subagent record.
type subagentMetaDecodeError struct {
	ref string
	err error
}

func (e *subagentMetaDecodeError) Error() string {
	return fmt.Sprintf("decode subagent metadata %q: %v", e.ref, e.err)
}

func (e *subagentMetaDecodeError) Unwrap() error { return e.err }

func isSubagentMetaDecodeError(err error) bool {
	var decodeErr *subagentMetaDecodeError
	return errors.As(err, &decodeErr)
}

// SubagentSpec describes the current invocation identity.
type SubagentSpec struct {
	Kind             string
	Name             string
	WorkspaceRoot    string
	ParentSession    string
	ParentToolCallID string
	SystemPrompt     string
	Registry         *tool.Registry
	ToolContext      context.Context
	Model            string
	Effort           string
	// ResumedFrom feeds the context capsule; it does not change how the
	// transcript itself is stored.
	ResumedFrom string
}

// SubagentRun is a prepared transcript run. Call Release exactly once.
type SubagentRun struct {
	Ref        string
	Session    *Session
	Meta       SubagentMeta
	ForkedFrom string

	store   *SubagentStore
	release func()
}

// SubagentArtifact is a persisted sub-agent transcript and metadata pair owned
// by a parent session. One file may be missing after a crash; lifecycle cleanup
// should operate on the paths that exist.
type SubagentArtifact struct {
	Ref         string
	SessionPath string
	MetaPath    string
	Meta        SubagentMeta
}

func (r *SubagentRun) Release() {
	if r != nil && r.release != nil {
		r.release()
		r.release = nil
	}
}

// EphemeralSubagentRun is a non-persisted run for callers without an owning
// parent session — e.g. headless `reasonix run`, which never mints a session
// path. Its empty Ref makes the store's MarkRunning/SaveCompleted/SaveFailed
// methods no-op and keeps FormatSubagentResult from emitting a transcript
// reference, so the sub-agent behaves exactly as it did before persisted
// transcripts existed. It holds no lock, so Release is a no-op.
func EphemeralSubagentRun(systemPrompt string) *SubagentRun {
	return &SubagentRun{Session: NewSession(systemPrompt)}
}

// SubagentStore persists sub-agent transcripts under config.SessionDir()/subagents.
// Its locks are process-local; cross-process mutation is intentionally out of v1.
type SubagentStore struct {
	dir                string
	destroyed          func(parentSession string) bool
	parentSessionProbe func(sessionPath string) bool

	// cleanupBeforeReread is a test seam for deterministic lease interleavings.
	cleanupBeforeReread func(parentSession, ref string)

	mu     sync.Mutex
	locked map[string]bool
}

func NewSubagentStore(dir string) *SubagentStore {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return &SubagentStore{dir: dir, locked: map[string]bool{}}
}

// WithDestroyedChecker makes saves for destroyed parent sessions no-op. This is
// used when a background sub-agent is cancelled because its parent session was
// cleared or moved out of active history.
func (s *SubagentStore) WithDestroyedChecker(fn func(parentSession string) bool) *SubagentStore {
	if s != nil {
		s.destroyed = fn
	}
	return s
}

// WithParentSessionProbe installs a process-local liveness check used before
// stale cleanup probes a parent transcript lease. Desktop supplies this for
// tabs and builds that are live before their durable lease is bound. A nil
// probe preserves the lease-only behavior used by CLI and server frontends.
func (s *SubagentStore) WithParentSessionProbe(fn func(sessionPath string) bool) *SubagentStore {
	if s != nil {
		s.parentSessionProbe = fn
	}
	return s
}

// ListSubagentsByParent returns persisted sub-agent artifacts whose metadata
// declares the given parent session owner.
func ListSubagentsByParent(sessionDir, parentSession string) ([]SubagentArtifact, error) {
	parentSession = strings.TrimSpace(parentSession)
	if strings.TrimSpace(sessionDir) == "" || parentSession == "" {
		return nil, nil
	}
	dir := filepath.Join(sessionDir, "subagents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := []SubagentArtifact{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		ref := strings.TrimSuffix(entry.Name(), ".meta.json")
		if !validSubagentRef(ref) {
			continue
		}
		metaPath := filepath.Join(dir, entry.Name())
		data, err := fileencoding.ReadFileUTF8(metaPath)
		if err != nil {
			return nil, err
		}
		var meta SubagentMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		if strings.TrimSpace(meta.ParentSession) != parentSession {
			continue
		}
		out = append(out, SubagentArtifact{
			Ref:         ref,
			SessionPath: filepath.Join(dir, ref+".jsonl"),
			MetaPath:    metaPath,
			Meta:        meta,
		})
	}
	return out, nil
}

// DeleteSubagentsByParent permanently removes sub-agent artifacts owned by a
// parent session. Missing counterpart files are ignored.
func DeleteSubagentsByParent(sessionDir, parentSession string) error {
	artifacts, err := ListSubagentsByParent(sessionDir, parentSession)
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		paths := []string{artifact.SessionPath, artifact.MetaPath}
		// Sub-agent saves are single-file today, but sweep transcript sidecars
		// (event log, event index, …) so no earlier build's artifacts survive
		// the delete.
		paths = append(paths, store.SessionSidecarFiles(artifact.SessionPath)...)
		for _, path := range paths {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// CleanupStaleRunning marks persisted running sub-agents as interrupted while
// holding each parent transcript's session lease. A live parent in this or
// another process therefore keeps its children untouched, while crash leftovers
// remain repairable before this process accepts new background work.
func (s *SubagentStore) CleanupStaleRunning() (int, error) {
	if s == nil {
		return 0, nil
	}
	// On Windows, os.ReadDir can report ERROR_DIRECTORY as an IsNotExist
	// error when the store path exists but is a regular file. Check the leaf
	// first so a malformed store remains a startup error instead of being
	// mistaken for an absent store.
	info, err := os.Stat(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("subagent store path %q is not a directory", s.dir)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		// Windows reports a non-directory at this path as ENOENT, so a plain
		// IsNotExist check would silently accept a corrupt store instead of
		// surfacing it. Only treat it as "no store yet" when nothing is there.
		if os.IsNotExist(err) {
			if _, statErr := os.Lstat(s.dir); statErr != nil {
				return 0, nil
			}
		}
		return 0, err
	}
	type staleParent struct {
		sessionPath string
		refs        []string
	}
	parents := map[string]*staleParent{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		ref := strings.TrimSuffix(entry.Name(), ".meta.json")
		if !validSubagentRef(ref) {
			continue
		}
		meta, err := s.LoadMeta(ref)
		if err != nil {
			// A corrupt metadata file (truncated write, killed process) must
			// not abort startup. Skip all content decode failures, including
			// errors from custom field decoders such as time.Time, while genuine
			// file I/O errors remain fatal so storage problems stay visible.
			if isSubagentMetaDecodeError(err) {
				continue
			}
			return 0, err
		}
		if meta.Status != SubagentRunning {
			continue
		}
		parentSession := strings.TrimSpace(meta.ParentSession)
		sessionPath, ok := s.parentSessionPath(parentSession)
		if !ok {
			// Old or malformed metadata without a provable parent cannot
			// authorize a destructive lifecycle rewrite.
			continue
		}
		parent := parents[parentSession]
		if parent == nil {
			parent = &staleParent{sessionPath: sessionPath}
			parents[parentSession] = parent
		}
		parent.refs = append(parent.refs, ref)
	}

	parentIDs := make([]string, 0, len(parents))
	for parentID := range parents {
		parentIDs = append(parentIDs, parentID)
	}
	sort.Strings(parentIDs)

	now := time.Now().UTC()
	cleaned := 0
	for _, parentID := range parentIDs {
		parent := parents[parentID]
		if s.parentSessionProbe != nil && s.parentSessionProbe(parent.sessionPath) {
			continue
		}
		lease, err := TryAcquireSessionLease(parent.sessionPath)
		if errors.Is(err, ErrSessionLeaseHeld) {
			continue
		}
		if err != nil {
			return cleaned, fmt.Errorf("acquire parent session lease %q: %w", parentID, err)
		}
		for _, ref := range parent.refs {
			if s.cleanupBeforeReread != nil {
				s.cleanupBeforeReread(parentID, ref)
			}
			// Re-read after acquiring the parent lease: the former owner may
			// have completed the child between the initial scan and handoff.
			meta, err := s.LoadMeta(ref)
			if err != nil {
				if isSubagentMetaDecodeError(err) {
					continue
				}
				lease.Release()
				return cleaned, err
			}
			if meta.Status != SubagentRunning || strings.TrimSpace(meta.ParentSession) != parentID {
				continue
			}
			meta.Status = SubagentInterrupted
			meta.UpdatedAt = now
			if err := s.saveMeta(meta); err != nil {
				lease.Release()
				return cleaned, err
			}
			cleaned++
		}
		lease.Release()
	}
	return cleaned, nil
}

func (s *SubagentStore) parentSessionPath(parentSession string) (string, bool) {
	parentSession = strings.TrimSpace(parentSession)
	if parentSession == "" || parentSession == "." || parentSession == ".." || filepath.Base(parentSession) != parentSession {
		return "", false
	}
	return filepath.Join(filepath.Dir(s.dir), parentSession+".jsonl"), true
}

func (s *SubagentStore) PrepareFresh(spec SubagentSpec) (*SubagentRun, error) {
	if s == nil {
		return nil, fmt.Errorf("subagent transcript store is required")
	}
	if err := requireParentSession(spec); err != nil {
		return nil, err
	}
	ref, err := s.newRef()
	if err != nil {
		return nil, err
	}
	release, err := s.lock(ref)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	meta := metaFromSpec(ref, SubagentRunning, now, now, spec)
	return &SubagentRun{Ref: ref, Session: NewSession(spec.SystemPrompt), Meta: meta, store: s, release: release}, nil
}

func (s *SubagentStore) PrepareContinue(ref string, spec SubagentSpec) (*SubagentRun, error) {
	if s == nil {
		return nil, fmt.Errorf("subagent continuation is not available in this session")
	}
	if err := requireParentSession(spec); err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("continue_from requires a subagent reference")
	}
	release, err := s.lock(ref)
	if err != nil {
		return nil, err
	}
	meta, err := s.LoadMeta(ref)
	if err != nil {
		release()
		return nil, err
	}
	if strings.TrimSpace(meta.ParentSession) != strings.TrimSpace(spec.ParentSession) {
		release()
		return s.prepareContinueFromAncestor(ref, spec)
	}
	if err := validateContinueOwner(meta, spec); err != nil {
		release()
		return nil, err
	}
	if err := validateMeta(meta, spec); err != nil {
		release()
		return nil, err
	}
	sess, err := LoadSession(s.sessionPath(ref))
	if err != nil {
		release()
		return nil, fmt.Errorf("load subagent transcript %q: %w", ref, err)
	}
	meta.ParentSession = spec.ParentSession
	meta.ParentToolCallID = spec.ParentToolCallID
	return &SubagentRun{Ref: ref, Session: sess, Meta: meta, store: s, release: release}, nil
}

func (s *SubagentStore) PrepareLegacyForkFrom(ref string, spec SubagentSpec) (*SubagentRun, error) {
	if s == nil {
		return nil, fmt.Errorf("subagent continuation is not available in this session")
	}
	if err := requireParentSession(spec); err != nil {
		return nil, err
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("fork_from requires a subagent reference")
	}
	release, err := s.lock(ref)
	if err != nil {
		return nil, err
	}
	meta, err := s.LoadMeta(ref)
	if err != nil {
		release()
		return nil, err
	}
	owner := strings.TrimSpace(meta.ParentSession)
	current := strings.TrimSpace(spec.ParentSession)
	if owner == "" {
		release()
		return nil, fmt.Errorf("subagent reference %q has no parent session; run a fresh subagent instead", ref)
	}
	if owner == current {
		release()
		if err := validateMeta(meta, spec); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("fork_from cannot be safely converted for subagent reference %q in the current conversation; use continue_from to continue it in place or start a fresh subagent for independent work", ref)
	}
	release()
	return s.PrepareContinue(ref, spec)
}

func (s *SubagentStore) prepareContinueFromAncestor(sourceRef string, spec SubagentSpec) (*SubagentRun, error) {
	sourceRef, err := s.nearestLineageSource(sourceRef, spec)
	if err != nil {
		return nil, err
	}
	copies, err := s.compatibleCopiesFromSource(sourceRef, spec)
	if err != nil {
		return nil, err
	}
	switch len(copies) {
	case 0:
		return s.prepareFork(sourceRef, spec)
	case 1:
		run, err := s.PrepareContinue(copies[0].Ref, spec)
		if err != nil {
			return nil, err
		}
		run.ForkedFrom = sourceRef
		return run, nil
	default:
		return nil, fmt.Errorf("subagent reference %q has multiple copied transcripts in current parent session %q", sourceRef, spec.ParentSession)
	}
}

func (s *SubagentStore) nearestLineageSource(requestedRef string, spec SubagentSpec) (string, error) {
	ancestors, err := s.sessionAncestors(spec.ParentSession)
	if err != nil {
		return "", err
	}
	for _, ancestor := range ancestors {
		artifacts, err := ListSubagentsByParent(filepath.Dir(s.dir), ancestor)
		if err != nil {
			return "", err
		}
		var candidates []SubagentArtifact
		for _, artifact := range artifacts {
			if artifact.Ref == requestedRef || s.derivesFrom(artifact.Meta, requestedRef) {
				candidates = append(candidates, artifact)
			}
		}
		if len(candidates) > 1 {
			return "", fmt.Errorf("subagent reference %q has multiple candidate transcripts in ancestor parent session %q", requestedRef, ancestor)
		}
		if len(candidates) == 1 {
			if err := validateMeta(candidates[0].Meta, spec); err != nil {
				return "", err
			}
			return candidates[0].Ref, nil
		}
	}
	return "", fmt.Errorf("subagent reference %q is not in current parent session %q lineage", requestedRef, spec.ParentSession)
}

func (s *SubagentStore) sessionAncestors(current string) ([]string, error) {
	current = strings.TrimSpace(current)
	if current == "" {
		return nil, nil
	}
	var ancestors []string
	seen := map[string]bool{}
	for cursor := current; cursor != ""; {
		if seen[cursor] {
			return nil, fmt.Errorf("cycle at session %q", cursor)
		}
		seen[cursor] = true
		// Route through parentSessionPath so both the caller-provided id and
		// every parent id read from disk metadata get the same bare-filename
		// validation — a raw Join would let "../"-shaped ids escape the
		// session directory.
		metaPath, valid := s.parentSessionPath(cursor)
		if !valid {
			return nil, fmt.Errorf("invalid session identifier %q", cursor)
		}
		meta, ok, err := LoadBranchMeta(metaPath)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("missing branch metadata for session %q", cursor)
		}
		if strings.TrimSpace(meta.ID) != cursor {
			return nil, fmt.Errorf("branch metadata for session %q declares id %q", cursor, meta.ID)
		}
		parent := strings.TrimSpace(meta.ParentID)
		if parent == "" {
			break
		}
		ancestors = append(ancestors, parent)
		cursor = parent
	}
	return ancestors, nil
}

// Terminal status is independent from transcript persistence. Keep going so
// a sidecar failure cannot leave a failed run marked as running on disk.

// ensureBranchCreatedAt seeds the session list sidecar before the first
// transcript save. Subagent transcripts are written only on completion, so
// Session.Save would otherwise backfill BranchMeta.CreatedAt with the save
// moment (completion time). The real start time already lives on run.Meta.

// Route through parentSessionPath so both the caller-provided id and
// every parent id read from disk metadata get the same bare-filename
// validation — a raw Join would let "../"-shaped ids escape the
// session directory.
