// Package taskintent answers classification questions from task text: is this
// obviously chat, a read, a mutation, or a persistent action, and which Goal
// turn-budget class (simple/write/research) the objective should start on.
// It must not grow into complexity, risk, planner depth, verification depth,
// completion, or tool-surface decisions — those belong to runtime evidence
// (see internal/taskcontract), where a receipt outranks any keyword.
//
// The vocabulary is a liability, not an asset: every added keyword, negation
// rule, or language case moves this package toward an unowned NLP parser.
// boundary_test.go enforces the line: the exported surface is a whitelist
// and the heuristic files carry a hard size budget, so growth is a reviewed
// decision, never an accretion. When a classification is wrong, prefer
// fixing it downstream with evidence over teaching this package more words.
//
// MIGRATION NOTICE: Intent classification is becoming a one-shot derived
// value (run.IntentHint) computed by RunSpecBuilder, not a persisted entity.
// See internal/run/spec.go for the IntentHint type and
// internal/taskcontract/spec.go for the bridge. This package remains for
// backward compatibility during the migration; new code should use
// run.IntentHint directly.
package taskintent
