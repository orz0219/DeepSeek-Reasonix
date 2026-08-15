package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"reasonix/internal/event"
)

type mockAsker struct {
	got []event.AskQuestion
	ans []event.AskAnswer
}

func (m *mockAsker) Ask(_ context.Context, questions []event.AskQuestion) ([]event.AskAnswer, error) {
	m.got = questions
	return m.ans, nil
}

func execAsk(t *testing.T, args string, asker Asker) string {
	t.Helper()
	var ctx context.Context = context.Background()
	if asker != nil {
		ctx = WithToolCallContext(ctx, "parent", nil, asker, false)
	}
	out, err := NewAskTool().Execute(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

func TestAskInputQuestion(t *testing.T) {
	a := &mockAsker{ans: []event.AskAnswer{{QuestionID: "q1", Selected: []string{"prototype"}}}}
	out := execAsk(t, `{"questions":[
		{"header":"Flow","question":"Where does this work start?","input":{"recommended":"idea to ship main flow","required":true}},
		{"header":"Supplement","question":"Anything else?","input":{"multiline":true}}
	]}`, a)
	if len(a.got) != 2 {
		t.Fatalf("got %d questions, want 2", len(a.got))
	}
	q1, q2 := a.got[0], a.got[1]
	if q1.Input == nil || q1.Input.Recommended != "idea to ship main flow" || !q1.Input.Required {
		t.Fatalf("q1 input = %+v, want recommended+required", q1.Input)
	}
	if len(q1.Options) != 0 {
		t.Fatalf("q1 options = %v, want none for input question", q1.Options)
	}
	if q2.Input == nil || !q2.Input.Multiline || q2.Input.Required {
		t.Fatalf("q2 input = %+v, want multiline, not required", q2.Input)
	}
	if !strings.Contains(out, "prototype") {
		t.Fatalf("result %q missing the user answer", out)
	}
}

func TestAskInputValidation(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"both options and input", `{"questions":[{"header":"Q","question":"x","options":[{"label":"a"},{"label":"b"}],"input":{"recommended":"r"}}]}`, "mutually exclusive"},
		{"neither options nor input", `{"questions":[{"header":"Q","question":"x"}]}`, "either options or input are required"},
		{"blank question", `{"questions":[{"header":"Q","question":"  ","input":{}}]}`, "a question is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAskTool().Execute(context.Background(), json.RawMessage(tc.args))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestAskOptionQuestionBackwardCompatible(t *testing.T) {
	a := &mockAsker{ans: []event.AskAnswer{{QuestionID: "q1", Selected: []string{"TUI"}}}}
	execAsk(t, `{"questions":[{"header":"Frontend","question":"Where first?","options":[{"label":"TUI"},{"label":"Desktop"}]}]}`, a)
	if len(a.got) != 1 || a.got[0].Input != nil || len(a.got[0].Options) != 2 {
		t.Fatalf("option question mangled: %+v", a.got)
	}
}

func TestAskHeadlessFallback(t *testing.T) {
	out := execAsk(t, `{"questions":[{"header":"Q","question":"x","input":{}}]}`, nil)
	if !strings.Contains(out, "model-assumption fallback") {
		t.Fatalf("headless result %q missing fallback marker", out)
	}
}
