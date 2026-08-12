package agent

type repeatFailureRecord struct {
	count        int
	errClass     string
	paths        []string
	stateRecheck bool
}

// KeepPolicy is a bitmask controlling which messages are preserved beyond the
// recent tail during compaction.
type KeepPolicy int

const (
	KeepErrors KeepPolicy = 1 << iota
	KeepUserMarked
)
