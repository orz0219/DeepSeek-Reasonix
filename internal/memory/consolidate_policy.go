// Consolidation policy: validates candidates against workspace rules before
// they reach the store. All scope, activation, confidence, and origin
// enforcement lives here so the consolidator never bypasses policy.
package memory

import (
	"fmt"
	"sort"
	"strings"
)

const (
	defaultMaxNewMemories  = 3
	defaultConfidenceFloor = 0.70
)

// ConsolidationPolicy bounds automatic memory creation.
type ConsolidationPolicy struct {
	MaxNewMemoriesPerSession int
	ConfidenceFloor          float64
}

// ValidateCandidate checks a single candidate against policy and suppression.
// Returns the (possibly modified) candidate or an error that signals "skip".
func ValidateCandidate(cand MemoryCandidate, existing []Memory, suppressedHints []string) (MemoryCandidate, error) {
	if cand.Action == CandidateActionIgnore {
		return cand, fmt.Errorf("candidate marked ignore")
	}

	// Scope: always project for auto-consolidation.
	cand.Scope = string(FactScopeProject)

	// Activation: always retrieval.
	// (No explicit field on candidate; we enforce at mutation time.)

	// Confidence floor.
	if cand.Confidence < defaultConfidenceFloor {
		return cand, fmt.Errorf("confidence %.2f below floor %.2f", cand.Confidence, defaultConfidenceFloor)
	}

	// Type normalization.
	cand.Type = string(NormalizeType(cand.Type))

	// Volatility normalization.
	if cand.Volatility == "" {
		cand.Volatility = string(VolatilityStable)
	}

	// Subject key normalization.
	cand.SubjectKey = NormalizeSubjectKey(cand.SubjectKey)

	// Name derivation.
	if cand.Name == "" {
		cand.Name = slug(cand.Description)
	}
	if cand.Name == "" {
		return cand, fmt.Errorf("cannot derive name")
	}
	cand.Name = slug(cand.Name)

	// Body must be non-empty.
	if len(strings.TrimSpace(cand.Body)) == 0 {
		return cand, fmt.Errorf("empty body")
	}

	// Check if the subject is suppressed.
	subjectKey := cand.SubjectKey
	if subjectKey == "" {
		subjectKey = NormalizeSubjectKey(cand.Name)
	}
	for _, hint := range suppressedHints {
		normalized := NormalizeSubjectKey(hint)
		if normalized != "" && (subjectKey == normalized || strings.Contains(subjectKey, normalized)) {
			return cand, fmt.Errorf("subject %q is suppressed", subjectKey)
		}
	}

	// Origin priority: cannot override manual/explicit.
	if cand.Action == CandidateActionUpdate || cand.Action == CandidateActionArchive {
		for _, m := range existing {
			if m.ID == cand.ExistingMemoryID || m.Name == cand.Name {
				if OriginPriority(m.Origin) > OriginPriority(OriginConsolidated) {
					return cand, fmt.Errorf("refusing to override %s-origin memory %q", m.Origin, m.Name)
				}
				break
			}
		}
	}

	return cand, nil
}

// FilterByConfidence sorts candidates by confidence descending and returns
// at most MaxNewMemoriesPerSession.
func FilterByConfidence(candidates []MemoryCandidate, policy ConsolidationPolicy) []MemoryCandidate {
	if len(candidates) == 0 {
		return nil
	}
	// Sort by confidence descending.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Confidence > candidates[j].Confidence
	})
	limit := policy.MaxNewMemoriesPerSession
	if limit <= 0 {
		limit = defaultMaxNewMemories
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	return candidates
}
