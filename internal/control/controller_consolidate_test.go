package control

import (
	"context"
	"testing"

	"reasonix/internal/memory"
)

func TestConsolidateSessionNoopWhenNil(t *testing.T) {
	c := &Controller{}
	// Should not panic when consolidator is nil.
	c.consolidateSession(context.Background())
}

type mockConsolidator struct {
	called bool
	input  memory.ConsolidationInput
	result memory.ConsolidationResult
	err    error
}

func (m *mockConsolidator) Consolidate(_ context.Context, input memory.ConsolidationInput) (memory.ConsolidationResult, error) {
	m.called = true
	m.input = input
	return m.result, m.err
}

func TestConsolidateSessionSingleflight(t *testing.T) {
	mock := &mockConsolidator{}
	c := &Controller{
		consolidator: mock,
	}
	// Set consolidateDone to simulate already-consolidated session.
	c.consolidateDone = true

	c.consolidateSession(context.Background())

	if mock.called {
		t.Error("consolidation should not run when consolidateDone is true")
	}
}

func TestResetConsolidation(t *testing.T) {
	c := &Controller{}
	c.consolidateDone = true
	c.resetConsolidation()
	if c.consolidateDone {
		t.Error("consolidateDone should be false after reset")
	}
}
