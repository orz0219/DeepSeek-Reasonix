package agent

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

func (s *SubagentStore) derivesFrom(meta SubagentMeta, sourceRef string) bool {
	sourceRef = strings.TrimSpace(sourceRef)
	seen := map[string]bool{}
	for cursor := strings.TrimSpace(meta.ForkedFrom); cursor != ""; {
		if cursor == sourceRef {
			return true
		}
		if seen[cursor] {
			return false
		}
		seen[cursor] = true
		parent, err := s.LoadMeta(cursor)
		if err != nil {
			return false
		}
		cursor = strings.TrimSpace(parent.ForkedFrom)
	}
	return false
}

func (s *SubagentStore) compatibleCopiesFromSource(sourceRef string, spec SubagentSpec) ([]SubagentArtifact, error) {
	artifacts, err := ListSubagentsByParent(filepath.Dir(s.dir), spec.ParentSession)
	if err != nil {
		return nil, err
	}
	var copies []SubagentArtifact
	for _, artifact := range artifacts {
		if strings.TrimSpace(artifact.Meta.ForkedFrom) != sourceRef {
			continue
		}
		if err := validateMeta(artifact.Meta, spec); err != nil {
			return nil, err
		}
		copies = append(copies, artifact)
	}
	return copies, nil
}

func (s *SubagentStore) prepareFork(ref string, spec SubagentSpec) (*SubagentRun, error) {
	if s == nil {
		return nil, fmt.Errorf("subagent continuation is not available in this session")
	}
	if err := requireParentSession(spec); err != nil {
		return nil, err
	}
	sourceRef := strings.TrimSpace(ref)
	if sourceRef == "" {
		return nil, fmt.Errorf("subagent copy requires a source reference")
	}
	sourceRelease, err := s.lock(sourceRef)
	if err != nil {
		return nil, err
	}
	meta, err := s.LoadMeta(sourceRef)
	if err != nil {
		sourceRelease()
		return nil, err
	}
	if strings.TrimSpace(meta.ParentSession) == "" {
		sourceRelease()
		return nil, fmt.Errorf("subagent reference %q has no parent session; run a fresh subagent instead", sourceRef)
	}
	if err := validateMeta(meta, spec); err != nil {
		sourceRelease()
		return nil, err
	}
	if err := s.validateForkOwner(meta, spec); err != nil {
		sourceRelease()
		return nil, err
	}
	sess, err := LoadSession(s.sessionPath(sourceRef))
	if err != nil {
		sourceRelease()
		return nil, fmt.Errorf("load subagent transcript %q: %w", sourceRef, err)
	}
	sourceRelease()
	newRef, err := s.newRef()
	if err != nil {
		return nil, err
	}
	newRelease, err := s.lock(newRef)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	newMeta := metaFromSpec(newRef, SubagentRunning, now, now, spec)
	newMeta.ForkedFrom = sourceRef
	return &SubagentRun{Ref: newRef, Session: sess, Meta: newMeta, ForkedFrom: sourceRef, store: s, release: newRelease}, nil
}
