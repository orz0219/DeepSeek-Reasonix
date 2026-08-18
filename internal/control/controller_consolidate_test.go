package control

import (
	"context"
	"testing"

	"reasonix/internal/memory"
	"reasonix/internal/provider"
)

func TestEnqueueConsolidationNoopWhenNil(t *testing.T) {
	c := &Controller{}
	// Should not panic when worker is nil.
	c.enqueueConsolidation(memory.ConsolidationSessionEnd)
}

func TestEnqueueConsolidationNoExecutor(t *testing.T) {
	worker := memory.NewConsolidationWorker(&noopConsolidator{}, memory.ConsolidationWorkerConfig{})
	defer worker.Close()
	c := &Controller{consolidationWorker: worker}
	// Should not panic or enqueue when executor is nil.
	c.enqueueConsolidation(memory.ConsolidationSessionEnd)
}

func TestProjectMessagesForMemory(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "system prompt"},
		{Role: provider.RoleUser, Content: "Hello"},
		{Role: provider.RoleAssistant, Content: "Hi there"},
		{Role: provider.RoleTool, Content: "tool result"},
		{Role: provider.RoleUser, Content: "", LocalOnly: true},
		{Role: provider.RoleAssistant, Content: "", LocalOnly: true},
		{Role: provider.RoleUser, Content: "Remember this"},
	}
	projected := projectMessagesForMemory(msgs)
	if len(projected) != 3 {
		t.Fatalf("expected 3 projected messages, got %d: %v", len(projected), projected)
	}
	if projected[0] != "Hello" {
		t.Errorf("first message: want Hello, got %s", projected[0])
	}
	if projected[1] != "Hi there" {
		t.Errorf("second message: want 'Hi there', got %s", projected[1])
	}
	if projected[2] != "Remember this" {
		t.Errorf("third message: want 'Remember this', got %s", projected[2])
	}
}

func TestProjectMessagesExcludesSystemAndTool(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "system"},
		{Role: provider.RoleTool, Content: "tool output"},
	}
	projected := projectMessagesForMemory(msgs)
	if len(projected) != 0 {
		t.Errorf("expected 0 projected messages, got %d", len(projected))
	}
}

type noopConsolidator struct{}

func (n *noopConsolidator) Consolidate(_ context.Context, _ memory.ConsolidationInput) (memory.ConsolidationResult, error) {
	return memory.ConsolidationResult{}, nil
}
