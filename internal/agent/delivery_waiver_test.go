package agent

import (
	"context"
	"errors"
	"testing"

	"reasonix/internal/event"
	"reasonix/internal/provider"
)

// completionVerdictSink records completion-summary verdicts for delivery
// waiver assertions.
type completionVerdictSink struct {
	verdicts []string
}

func (s *completionVerdictSink) Emit(e event.Event) {
	if e.Kind == event.CompletionSummary && e.Completion != nil {
		s.verdicts = append(s.verdicts, e.Completion.Verdict)
	}
}

func TestExplicitDeliveryWaiverClosesAsPartial(t *testing.T) {
	reg := evidenceRegistry()
	finalText := []provider.Chunk{{Type: provider.ChunkText, Text: "premature"}, {Type: provider.ChunkDone}}
	prov := &scriptedProvider{name: "delivery", turns: [][]provider.Chunk{
		{toolCallChunk("todo", "todo_write", `{"todos":[{"content":"Ship main","status":"in_progress"}]}`), {Type: provider.ChunkDone}},
		{toolCallChunk("write", "write_file", `{"path":"main.go"}`), {Type: provider.ChunkDone}},
		finalText,
		{{Type: provider.ChunkText, Text: "accepted, closing as partial"}, {Type: provider.ChunkDone}},
	}}
	sink := &completionVerdictSink{}
	a := New(prov, reg, NewSession("sys"), Options{DeliveryProfile: true}, sink)
	var readinessErr *FinalReadinessError
	if err := a.Run(context.Background(), "implement main"); !errors.As(err, &readinessErr) {
		t.Fatalf("first Run error = %v, want FinalReadinessError", err)
	}
	if !a.PrepareDeliveryWaiver() {
		t.Fatal("waiver should consume the pending readiness failure")
	}
	if a.PrepareDeliveryWaiver() {
		t.Fatal("delivery waiver authorization must be one-shot")
	}
	if err := a.Run(context.Background(), "用户接受未验证状态，请以部分完成结束，不要继续运行验证"); err != nil {
		t.Fatalf("waiver Run: %v", err)
	}
	if _, ok := a.task.ledger.LatestSuccessfulMutationIndex(); !ok {
		t.Fatal("waiver turn lost the prior mutation receipt")
	}
	if len(sink.verdicts) == 0 || sink.verdicts[len(sink.verdicts)-1] != "partial" {
		t.Fatalf("completion verdicts = %v, want last to be partial", sink.verdicts)
	}
}
