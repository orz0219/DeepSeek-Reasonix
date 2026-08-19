package harness

// Decision is one model turn's output: either a final answer to be verified,
// or a tool action to execute.
type Decision struct {
	Final  bool
	Text   string
	Action *Action
}

// IsFinal reports whether the model finished and expects verification.
func (d Decision) IsFinal() bool { return d.Final }

// Answer returns the final-answer text (empty for a tool decision).
func (d Decision) Answer() string { return d.Text }

// ToolAction returns the action to execute for a tool decision.
func (d Decision) ToolAction() (Action, bool) {
	if d.Action == nil {
		return Action{}, false
	}
	return *d.Action, true
}

// FinalAnswer builds a final-answer decision.
func FinalAnswer(text string) Decision {
	return Decision{Final: true, Text: text}
}

// TakeAction builds a tool decision.
func TakeAction(a Action) Decision {
	return Decision{Action: &a}
}
