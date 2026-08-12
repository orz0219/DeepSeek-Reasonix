package evidence

import (
	"encoding/json"
	"slices"
	"strings"
)

// TodoStepMatch is the result of matching a complete_step citation against the
// latest successful todo_write list in this turn.
type TodoStepMatch struct {
	Found      bool
	Index      int
	Content    string
	Status     string
	ActiveForm string
	StepID     string
}

func (l *Ledger) HasSuccessfulTodoWrite() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if r.Success && r.ToolName == "todo_write" {
			return true
		}
	}
	return false
}

// HasSuccessfulAcceptanceCriteria reports whether the current turn established
// a non-empty task list. Delivery mode uses that list as its host-observable
// acceptance contract before permitting state-changing work.
func (l *Ledger) HasSuccessfulAcceptanceCriteria() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if r.Success && r.ToolName == "todo_write" && len(r.Todos) > 0 {
			return true
		}
	}
	return false
}

// HasSuccessfulTodoProgressReceipt reports whether any successful receipt in
// the turn reflects execution progress rather than read-only context gathering
// or a bare todo snapshot.
func (l *Ledger) HasSuccessfulTodoProgressReceipt() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if !r.Success || r.ToolName == "todo_write" || r.Read {
			continue
		}
		return true
	}
	return false
}

func (l *Ledger) IncompleteLatestTodos() ([]TodoStepMatch, bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, v := range slices.Backward(l.receipts) {
		r := v
		if !r.Success || r.ToolName != "todo_write" {
			continue
		}
		return IncompleteTodos(r.Todos), true
	}
	return nil, false
}

// IncompleteTodos returns the items of a todo list that are not completed.
func IncompleteTodos(todos []TodoItem) []TodoStepMatch {
	incomplete := make([]TodoStepMatch, 0)
	for j, t := range todos {
		status := todoStatus(t.Status)
		if status == "completed" {
			continue
		}
		incomplete = append(incomplete, TodoStepMatch{
			Found:      true,
			Index:      j + 1,
			Content:    t.Content,
			Status:     status,
			ActiveForm: t.ActiveForm,
		})
	}
	return incomplete
}

// MatchStep resolves a complete_step.step (number, title, or drift-tolerant
// variant) against a todo list, returning the matched item.
func MatchStep(step string, todos []TodoItem) (TodoStepMatch, bool) {
	m := matchTodoStep(step, todos)
	return m, m.Found
}

// MatchTodoIdentity resolves an existing todo against an updated list without
// interpreting numeric content as a 1-based step citation.
func MatchTodoIdentity(todo TodoItem, todos []TodoItem) (TodoStepMatch, bool) {
	for i, candidate := range todos {
		if sameTodoIdentity(todo, candidate) {
			return todoMatchAt(i+1, candidate), true
		}
	}
	found := -1
	for i, candidate := range todos {
		match := TodoStepMatch{Content: candidate.Content, ActiveForm: candidate.ActiveForm}
		if !todoContentRelates(todo, match) {
			continue
		}
		if found >= 0 && found != i {
			return TodoStepMatch{}, false
		}
		found = i
	}
	if found < 0 {
		return TodoStepMatch{}, false
	}
	candidate := todos[found]
	return todoMatchAt(found+1, candidate), true
}

// PreservesCompletedTodoPositions reports whether every previously completed
// item remains completed at the same index in the replacement list. Completed
// sub-steps can sit behind a pending phase header, so this checks every item
// rather than assuming the literal list begins with completed statuses.
func PreservesCompletedTodoPositions(previous, next []TodoItem) bool {
	for i, todo := range previous {
		if todoStatus(todo.Status) != "completed" {
			continue
		}
		if i >= len(next) || todoStatus(next[i].Status) != "completed" {
			return false
		}
		match, found := MatchTodoIdentity(todo, next)
		if !found || match.Index != i+1 {
			return false
		}
	}
	return true
}

// HasAnySuccessfulReceipt reports whether any tool succeeded this turn — the
// signal that the turn did real work, not pure conversation.
func (l *Ledger) HasAnySuccessfulReceipt() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if r.Success {
			return true
		}
	}
	return false
}

// HasSuccessfulToolReceipt reports whether a named tool completed
// successfully in the current evidence scope.
func (l *Ledger) HasSuccessfulToolReceipt(name string) bool {
	name = strings.TrimSpace(name)
	if l == nil || name == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if r.Success && r.ToolName == name {
			return true
		}
	}
	return false
}

// HasSuccessfulMutationOtherThan distinguishes a workflow-specific state
// change (for example durable memory) from unrelated workspace mutations that
// still need the full Delivery verification/review contract.
func (l *Ledger) HasSuccessfulMutationOtherThan(allowed ...string) bool {
	if l == nil {
		return false
	}
	allow := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		allow[strings.TrimSpace(name)] = true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if r.Success && r.Mutation && !allow[r.ToolName] {
			return true
		}
	}
	return false
}

// HasSuccessfulWorkReceipt excludes workflow bookkeeping and reports whether
// the assistant actually inspected, executed, or changed something this turn.
// Delivery mode uses it to reject text-only claims for technical tasks while
// still allowing ordinary conversation to finish without tools.
func (l *Ledger) HasSuccessfulWorkReceipt() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if !r.Success {
			continue
		}
		switch r.ToolName {
		case "ask", "todo_write", "complete_step":
			continue
		}
		return true
	}
	return false
}

// HasSuccessfulVerificationCommand reports whether the turn ran at least one
// command classified as verification rather than inspection or mutation.
func (l *Ledger) HasSuccessfulVerificationCommand() bool {
	return l.HasSuccessfulVerificationCommandAfter(-1)
}

// HasSuccessfulVerificationCommandAfter reports whether verification succeeded
// after the named receipt index. Mutations before the boundary do not satisfy a
// role setting's post-change verification floor.
func (l *Ledger) HasSuccessfulVerificationCommandAfter(after int) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts[max(after+1, 0):] {
		if r.Success && r.ToolName == "bash" && bashCommandIsVerification(r.Command) {
			return true
		}
	}
	return false
}

func (l *Ledger) HasSuccessfulWrite(paths []string) bool {
	return l.hasSuccessfulPaths(paths, func(r Receipt) bool { return r.Write })
}

func (l *Ledger) HasSuccessfulReadOrWrite(paths []string) bool {
	return l.hasSuccessfulPaths(paths, func(r Receipt) bool { return r.Read || r.Write })
}

func (l *Ledger) LatestSuccessfulWriteIndex(paths []string) (int, bool) {
	wanted := pathSet(normalizePaths(paths))
	if l == nil || len(wanted) == 0 {
		return 0, false
	}
	latest := -1

	l.mu.Lock()
	defer l.mu.Unlock()
	for i, r := range l.receipts {
		if !r.Success || !r.Write {
			continue
		}
		for _, p := range r.Paths {
			if wanted[p] {
				latest = i
				break
			}
		}
	}
	return latest, latest >= 0
}

// HasSuccessfulAnchorRefreshReadAfter reports whether read_file refreshed a
// wanted path after the given receipt index. Windowed reads and grep/ls receipts
// are deliberately not enough for same-turn anchor edits: they may have observed
// a different region than the next old_string/delete_range anchor.
func (l *Ledger) HasSuccessfulAnchorRefreshReadAfter(paths []string, after int) bool {
	wanted := pathSet(normalizePaths(paths))
	if l == nil || len(wanted) == 0 {
		return false
	}
	start := max(after+1, 0)

	l.mu.Lock()
	defer l.mu.Unlock()
	for i := start; i < len(l.receipts); i++ {
		r := l.receipts[i]
		if !r.Success || !anchorRefreshRead(r) {
			continue
		}
		for _, p := range r.Paths {
			if wanted[p] {
				return true
			}
		}
	}
	return false
}

func anchorRefreshRead(r Receipt) bool {
	if r.ToolName != "read_file" || !r.Read {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(r.Args, &fields); err != nil {
		return false
	}
	if limit, ok := intField(fields, "limit"); ok && limit > 0 {
		return false
	}
	if offset, ok := intField(fields, "offset"); ok && offset > 0 {
		return false
	}
	return true
}

func (l *Ledger) LatestSuccessfulWriterIndex() (int, bool) {
	if l == nil {
		return 0, false
	}
	latest := -1

	l.mu.Lock()
	defer l.mu.Unlock()
	for i, r := range l.receipts {
		if r.Success && r.Write {
			latest = i
		}
	}
	return latest, latest >= 0
}

// LatestSuccessfulMutationIndex returns the most recent host-observed
// state-changing call. It includes known file writers, writer-capable delegated
// or external tools, and bash commands that are not demonstrably observational
// or verification-only.
func (l *Ledger) LatestSuccessfulMutationIndex() (int, bool) {
	if l == nil {
		return 0, false
	}
	latest := -1
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, r := range l.receipts {
		if r.Success && r.Mutation {
			latest = i
		}
	}
	return latest, latest >= 0
}

func (l *Ledger) MatchLatestTodoStep(step string) (TodoStepMatch, bool) {
	step = strings.TrimSpace(step)
	if l == nil || step == "" {
		return TodoStepMatch{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, v := range slices.Backward(l.receipts) {
		r := v
		if !r.Success || r.ToolName != "todo_write" {
			continue
		}
		return matchTodoStep(step, r.Todos), true
	}
	return TodoStepMatch{}, false
}

// LatestTodos returns the todo list from this turn's latest successful todo_write.
func (l *Ledger) LatestTodos() ([]TodoItem, bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, v := range slices.Backward(l.receipts) {
		r := v
		if r.Success && r.ToolName == "todo_write" {
			return append([]TodoItem(nil), r.Todos...), true
		}
	}
	return nil, false
}

// UnverifiedCompletedTodos reports current completed todos that transitioned
// from the latest prior successful todo_write receipt without a matching
// successful complete_step receipt earlier in the same turn. If this turn has no
// prior todo_write baseline, hasBaseline is false and callers should preserve
// the existing loose validation behavior.
func (l *Ledger) UnverifiedCompletedTodos(current []TodoItem) (missing []TodoStepMatch, hasBaseline bool) {
	current = normalizeTodos(current)
	if l == nil {
		return nil, false
	}

	l.mu.Lock()
	receipts := append([]Receipt(nil), l.receipts...)
	l.mu.Unlock()

	var previous []TodoItem
	baseline := -1
	for i, v := range slices.Backward(receipts) {
		r := v
		if !r.Success || r.ToolName != "todo_write" {
			continue
		}
		previous = r.Todos
		baseline = i
		hasBaseline = true
		break
	}
	if !hasBaseline {
		return nil, false
	}

	for i, t := range current {
		if todoStatus(t.Status) != "completed" {
			continue
		}
		index := i + 1
		if previousTodoCompleted(index, t, previous) {
			continue
		}
		if hasSuccessfulCompleteStepForTodo(receipts, index, current) {
			continue
		}
		if hasFailedCompleteStepRecoveryForTodo(receipts, baseline, index, current) {
			continue
		}
		missing = append(missing, TodoStepMatch{
			Found:      true,
			Index:      index,
			Content:    t.Content,
			Status:     todoStatus(t.Status),
			ActiveForm: t.ActiveForm,
		})
	}
	return missing, true
}

func hasFailedCompleteStepRecoveryForTodo(receipts []Receipt, baseline int, index int, current []TodoItem) bool {
	for i := baseline + 1; i < len(receipts); i++ {
		r := receipts[i]
		if r.Success || r.ToolName != "complete_step" || strings.TrimSpace(r.Step) == "" || !r.StepProof {
			continue
		}
		if !hasSuccessfulProgressBeforeReceipt(receipts, baseline, i) {
			continue
		}
		if r.TodoStep != nil && r.TodoStep.Found {
			if index < 1 || index > len(current) {
				continue
			}
			if sameTodoMatch(current[index-1], *r.TodoStep) {
				return true
			}
			if !todoContentRelates(current[index-1], *r.TodoStep) {
				continue
			}
		}
		match := matchTodoStep(r.Step, current)
		if match.Found && match.Index == index {
			return true
		}
	}
	return false
}

// Recovery only trusts progress that happened before the failed sign-off.
// Later unrelated work must not retroactively authorize an earlier completion.
func hasSuccessfulProgressBeforeReceipt(receipts []Receipt, baseline int, before int) bool {
	start := max(baseline+1, 0)
	for i := start; i < before && i < len(receipts); i++ {
		r := receipts[i]
		if !r.Success || r.ToolName == "todo_write" || r.ToolName == "complete_step" || r.Read {
			continue
		}
		return true
	}
	return false
}

func (l *Ledger) hasSuccessfulPaths(paths []string, accept func(Receipt) bool) bool {
	wanted := pathSet(normalizePaths(paths))
	if l == nil || len(wanted) == 0 {
		return false
	}
	found := map[string]bool{}

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if !r.Success || !accept(r) {
			continue
		}
		for _, p := range r.Paths {
			if _, ok := wanted[p]; ok {
				found[p] = true
			}
		}
	}
	return len(found) == len(wanted)
}
