package run

// Terminal reports whether the status ends the run's lifecycle. A terminal
// run must never return to running through the harness loop.
func (s RunStatus) Terminal() bool {
	switch s {
	case RunCompleted, RunFailed, RunCancelled:
		return true
	default:
		return false
	}
}

// String returns the wire name of the status.
func (s RunStatus) String() string { return string(s) }
