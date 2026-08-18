// Deduplication and subject-key conflict resolution for consolidation
// candidates. Runs after policy validation, before store mutation.
package memory

import (
	"strings"
)

// DedupDecision is the outcome of checking a candidate against existing memories.
type DedupDecision struct {
	Action     string // add, update, archive, ignore
	ExistingID string // set when action is update or archive
	Reason     string
}

// DeduplicateCandidate checks whether a candidate duplicates or conflicts with
// an existing memory. The priority order is:
// 1. Same SubjectKey → update/archive (new value replaces old)
// 2. Same Name → update if content differs
// 3. Same normalized Title → ignore (likely duplicate)
// 4. Otherwise → add
func DeduplicateCandidate(cand MemoryCandidate, existing []Memory) DedupDecision {
	subjectKey := NormalizeSubjectKey(cand.SubjectKey)
	name := slug(cand.Name)
	normalizedTitle := normalizedRecallTitle(cand.Title)
	normalizedDesc := normalizedMemoryPhrase(cand.Description)

	for _, m := range existing {
		// Subject key match: new value replaces old.
		if subjectKey != "" && NormalizeSubjectKey(m.SubjectKey) == subjectKey {
			if strings.TrimSpace(m.Body) == strings.TrimSpace(cand.Body) {
				return DedupDecision{Action: CandidateActionIgnore, ExistingID: m.ID, Reason: "same subject, same body"}
			}
			return DedupDecision{Action: CandidateActionUpdate, ExistingID: m.ID, Reason: "same subject, new value"}
		}

		// Name match: update if content differs.
		if name != "" && slug(m.Name) == name {
			if strings.TrimSpace(m.Body) == strings.TrimSpace(cand.Body) {
				return DedupDecision{Action: CandidateActionIgnore, ExistingID: m.ID, Reason: "same name, same body"}
			}
			return DedupDecision{Action: CandidateActionUpdate, ExistingID: m.ID, Reason: "same name, different body"}
		}

		// Title match: likely duplicate.
		if normalizedTitle != "" && normalizedRecallTitle(m.Title) == normalizedTitle {
			if normalizedDesc != "" && normalizedMemoryPhrase(m.Description) == normalizedDesc {
				return DedupDecision{Action: CandidateActionIgnore, ExistingID: m.ID, Reason: "same title and description"}
			}
		}
	}

	return DedupDecision{Action: CandidateActionAdd, Reason: "no conflict"}
}
