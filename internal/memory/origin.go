// Memory origin: tracks how a fact entered the store so policy can enforce
// priority (manual > explicit > consolidated > inferred) and auto-generated
// facts never silently override user-curated ones.
package memory

import "strings"

// MemoryOrigin records how a memory was created.
type MemoryOrigin string

const (
	OriginManual       MemoryOrigin = "manual"       // user created via desktop panel or direct file edit
	OriginExplicit     MemoryOrigin = "explicit"     // model ran the remember tool with user confirmation
	OriginConsolidated MemoryOrigin = "consolidated" // automatic session consolidation
	OriginInferred     MemoryOrigin = "inferred"     // low-confidence inference, never auto-saved
)

// NormalizeOrigin validates and normalizes an origin string. Empty or unknown
// values return OriginExplicit as the safe default (existing memories predate
// the field and were all user-confirmed).
func NormalizeOrigin(s string) MemoryOrigin {
	switch MemoryOrigin(strings.ToLower(strings.TrimSpace(s))) {
	case OriginManual:
		return OriginManual
	case OriginExplicit:
		return OriginExplicit
	case OriginConsolidated:
		return OriginConsolidated
	case OriginInferred:
		return OriginInferred
	}
	return OriginExplicit
}

// OriginPriority returns a numeric priority where higher wins. Auto-generated
// facts must not silently override user-curated ones.
func OriginPriority(o MemoryOrigin) int {
	switch NormalizeOrigin(string(o)) {
	case OriginManual:
		return 40
	case OriginExplicit:
		return 30
	case OriginConsolidated:
		return 20
	case OriginInferred:
		return 10
	}
	return 30
}
