package evidence

import (
	"fmt"
)

// TodoItem mirrors the todo_write item shape the host needs for step matching.
// StepID is the item's stable identity: it survives a retitle and a reorder, so
// completion attribution never has to be inferred from wording or position. It
// is optional — a list written freehand has none, and matches by text instead.
type TodoItem struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm,omitempty"`
	Level      int    `json:"level,omitempty"`
	StepID     string `json:"step_id,omitempty"`
}

// ValidateSerialTodos enforces the task-list state machine promised by
// todo_write: at most one item in the whole list is in_progress, completed
// work forms a serial prefix, and pending work follows the current item. The
// rule is segment-aware for two-level lists: a level-0 phase owns the level-1
// sub-steps after it, sub-steps complete in order while their phase stays
// pending, and the phase becomes the single in_progress item only after every
// sub-step has completed — the phase signs off last. A fully completed or
// empty list is also valid.
func ValidateSerialTodos(todos []TodoItem) error {
	ipSeen := false
	for i, todo := range todos {
		switch todoStatus(todo.Status) {
		case "completed", "pending":
		case "in_progress":
			if ipSeen {
				return fmt.Errorf("todo %d %q is a second in_progress item; serial task lists allow exactly one current item", i+1, todo.Content)
			}
			ipSeen = true
		default:
			return fmt.Errorf("todo %d %q has invalid status %q", i+1, todo.Content, todo.Status)
		}
	}
	if len(todos) > 0 && todos[0].Level == 1 {
		return fmt.Errorf("todo 1 %q is a level-1 sub-step with no phase above it; add a level-0 phase header or use level 0", todos[0].Content)
	}
	seenCurrent := false
	seenPending := false
	for _, seg := range serialTodoSegments(todos) {
		state, err := validateSerialSegment(todos, seg)
		if err != nil {
			return err
		}
		switch state {
		case "completed":
			if seenCurrent || seenPending {
				return fmt.Errorf("todo %d %q is completed after unfinished work; serial task lists require completed items to form a prefix", seg.head+1, todos[seg.head].Content)
			}
		case "in_progress":
			if seenPending {
				ip := seg.head
				for i := seg.head; i < seg.end; i++ {
					if todoStatus(todos[i].Status) == "in_progress" {
						ip = i
						break
					}
				}
				return fmt.Errorf("todo %d %q is in_progress after pending work; the current item must be the first unfinished item", ip+1, todos[ip].Content)
			}
			seenCurrent = true
		case "pending":
			seenPending = true
		default:
			if seenCurrent {
				first := seg.head
				for i := seg.head; i < seg.end; i++ {
					if todoStatus(todos[i].Status) == "completed" {
						first = i
						break
					}
				}
				return fmt.Errorf("todo %d %q is completed after unfinished work; serial task lists require completed items to form a prefix", first+1, todos[first].Content)
			}
			seenPending = true
		}
	}
	if len(todos) > 0 && seenPending && !seenCurrent {
		return fmt.Errorf("serial task list has pending work but no in_progress item")
	}
	return nil
}

// todoSegment is one serial unit of a task list: a level-0 phase header plus
// its level-1 sub-steps, or a single plain step. end is exclusive.
type todoSegment struct {
	head int
	end  int
}

// serialTodoSegments splits a task list into serial units. A level-0 item
// directly followed by level-1 items owns them as one phase segment; every
// other item — including a level-1 item with no preceding phase — is its own
// single-step segment.
func serialTodoSegments(todos []TodoItem) []todoSegment {
	var segs []todoSegment
	for i := 0; i < len(todos); {
		end := i + 1
		if todos[i].Level == 0 {
			for end < len(todos) && todos[end].Level == 1 {
				end++
			}
		}
		segs = append(segs, todoSegment{head: i, end: end})
		i = end
	}
	return segs
}

// validateSerialSegment checks one segment's internal shape and returns its
// serial state: "completed" (every item completed), "in_progress" (the
// segment holds the current item), "pending" (untouched), or "stale"
// (partially completed with no current item). Item statuses and the global
// single-in_progress rule are already validated by the caller.
func validateSerialSegment(todos []TodoItem, seg todoSegment) (string, error) {
	head := todos[seg.head]
	headStatus := todoStatus(head.Status)
	if seg.end == seg.head+1 {
		return headStatus, nil
	}
	seenSubCurrent := false
	seenSubPending := false
	completedSubs := 0
	unfinished := -1
	for i := seg.head + 1; i < seg.end; i++ {
		sub := todos[i]
		switch todoStatus(sub.Status) {
		case "completed":
			if seenSubCurrent || seenSubPending {
				return "", fmt.Errorf("todo %d %q is completed after unfinished work; serial task lists require completed items to form a prefix", i+1, sub.Content)
			}
			completedSubs++
		case "in_progress":
			if seenSubPending {
				return "", fmt.Errorf("todo %d %q is in_progress after pending work; the current item must be the first unfinished item", i+1, sub.Content)
			}
			seenSubCurrent = true
			if unfinished < 0 {
				unfinished = i
			}
		default:
			seenSubPending = true
			if unfinished < 0 {
				unfinished = i
			}
		}
	}
	switch headStatus {
	case "completed":
		if unfinished >= 0 {
			return "", fmt.Errorf("phase %d %q is completed but sub-step %d %q is unfinished; complete every sub-step, then sign the phase off with complete_step", seg.head+1, head.Content, unfinished+1, todos[unfinished].Content)
		}
		return "completed", nil
	case "in_progress":
		if unfinished >= 0 {
			return "", fmt.Errorf("phase %d %q cannot be in_progress while sub-step %d %q is unfinished; keep the phase pending, finish its sub-steps in order, then mark the phase in_progress to sign it off", seg.head+1, head.Content, unfinished+1, todos[unfinished].Content)
		}
		return "in_progress", nil
	default:
		if seenSubCurrent {
			return "in_progress", nil
		}
		if completedSubs == 0 {
			return "pending", nil
		}
		return "stale", nil
	}
}

// NormalizeSerialTodos repairs legacy host state that predates
// ValidateSerialTodos. It preserves the leading run of fully completed
// segments and makes the first unfinished segment current: its completed
// sub-step prefix is kept and its first unfinished sub-step becomes the
// single in_progress item — or the phase itself when every sub-step is
// already completed. Every later segment returns to pending.
func NormalizeSerialTodos(todos []TodoItem) []TodoItem {
	out := append([]TodoItem(nil), todos...)
	unfinished := false
	for _, seg := range serialTodoSegments(out) {
		if !unfinished && serialSegmentCompleted(out, seg) {
			continue
		}
		if unfinished {
			for i := seg.head; i < seg.end; i++ {
				out[i].Status = "pending"
			}
			continue
		}
		unfinished = true
		if seg.end == seg.head+1 {
			out[seg.head].Status = "in_progress"
			continue
		}
		subUnfinished := false
		for i := seg.head + 1; i < seg.end; i++ {
			if !subUnfinished && todoStatus(out[i].Status) == "completed" {
				continue
			}
			if !subUnfinished {
				out[i].Status = "in_progress"
				subUnfinished = true
				continue
			}
			out[i].Status = "pending"
		}
		if subUnfinished {
			out[seg.head].Status = "pending"
		} else {
			out[seg.head].Status = "in_progress"
		}
	}
	return out
}

func serialSegmentCompleted(todos []TodoItem, seg todoSegment) bool {
	for i := seg.head; i < seg.end; i++ {
		if todoStatus(todos[i].Status) != "completed" {
			return false
		}
	}
	return true
}

// FirstUnfinishedSubStep reports whether todos[index] is a level-0 phase with
// level-1 sub-steps, and if so the 0-based index of its first sub-step that is
// not yet completed. ok is false when index is not a phase header; a phase
// whose sub-steps are all completed returns (-1, true).
func FirstUnfinishedSubStep(todos []TodoItem, index int) (int, bool) {
	if index < 0 || index >= len(todos) || todos[index].Level != 0 {
		return -1, false
	}
	if index+1 >= len(todos) || todos[index+1].Level != 1 {
		return -1, false
	}
	for i := index + 1; i < len(todos) && todos[i].Level == 1; i++ {
		if todoStatus(todos[i].Status) != "completed" {
			return i, true
		}
	}
	return -1, true
}

// AdvanceSerialTodo completes the in_progress item at index (0-based) as a
// signed-off step and promotes the next serial item so exactly one item stays
// current. A phase with unfinished sub-steps does not complete. Completing a
// sub-step promotes its next pending sibling, or returns its phase to
// in_progress for sign-off once every sibling is completed. Completing a
// phase or plain step promotes the next pending unit — a phase's first
// pending sub-step (the phase itself stays pending until its sub-steps
// finish), or the plain step itself. A level-1 item with no phase above it
// advances as a standalone step. It reports whether the item was completed.
func AdvanceSerialTodo(todos []TodoItem, index int) bool {
	if index < 0 || index >= len(todos) {
		return false
	}
	if todoStatus(todos[index].Status) != "in_progress" {
		return false
	}
	if unfinished, ok := FirstUnfinishedSubStep(todos, index); ok && unfinished >= 0 {
		return false
	}
	todos[index].Status = "completed"
	if todos[index].Level == 1 {
		for i := index + 1; i < len(todos) && todos[i].Level == 1; i++ {
			if todoStatus(todos[i].Status) == "pending" {
				todos[i].Status = "in_progress"
				return true
			}
		}
		head := index - 1
		for head >= 0 && todos[head].Level == 1 {
			head--
		}
		if head >= 0 {
			if todoStatus(todos[head].Status) != "completed" {
				todos[head].Status = "in_progress"
			}
			return true
		}

	}
	for i := range todos {
		if todoStatus(todos[i].Status) == "in_progress" {
			return true
		}
	}
	for i := range todos {
		if todoStatus(todos[i].Status) != "pending" {
			continue
		}
		if sub, ok := FirstUnfinishedSubStep(todos, i); ok && sub >= 0 {
			if todoStatus(todos[sub].Status) == "pending" {
				todos[sub].Status = "in_progress"
			}
			return true
		}
		todos[i].Status = "in_progress"
		return true
	}
	return true
}
