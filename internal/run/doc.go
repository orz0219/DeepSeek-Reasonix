// Package run defines the harness-centric execution model: Run is the sole
// lifecycle object for one agent execution, and RunSpec is the descriptive
// contract it executes against. This package is plain data — no execution, no
// events, no persistence, no imports below reasonix/. The old taskintent /
// taskpolicy / taskcontract layer is expected to shrink down to a
// RunSpecBuilder (internal/taskcontract) as the harness migration proceeds.
package run
