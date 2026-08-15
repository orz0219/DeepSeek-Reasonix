package eventwire

import (
	"encoding/json"
	"testing"

	"reasonix/internal/event"
)

func TestToWireAskCarriesInput(t *testing.T) {
	a := event.Ask{
		ID: "ask-1",
		Questions: []event.AskQuestion{
			{ID: "q1", Header: "Flow", Prompt: "Where does this start?", Input: &event.AskInput{Recommended: "main flow", Required: true}},
			{ID: "q2", Header: "Pick", Prompt: "Which frontend?", Options: []event.AskOption{{Label: "TUI"}, {Label: "Desktop"}}},
		},
	}
	raw, err := json.Marshal(ToWireAsk(a))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Questions []struct {
			Input *struct {
				Recommended string `json:"recommended"`
				Multiline   bool   `json:"multiline"`
				Required    bool   `json:"required"`
			} `json:"input"`
			Options []AskOption `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Questions[0].Input == nil || got.Questions[0].Input.Recommended != "main flow" || !got.Questions[0].Input.Required {
		t.Fatalf("wire input = %+v, want recommended+required", got.Questions[0].Input)
	}
	if got.Questions[1].Input != nil || len(got.Questions[1].Options) != 2 {
		t.Fatalf("option question wire = %+v, want options only", got.Questions[1])
	}
}
