package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestWorkerEnqueueAndProcess(t *testing.T) {
	store, _ := newTestStore(t)
	llm := mockCompletion(`{"memories":[]}`, nil)
	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{})
	worker := NewConsolidationWorker(consolidator, ConsolidationWorkerConfig{
		JobTimeout: 5 * time.Second,
	})
	defer worker.Close()

	err := worker.Enqueue(ConsolidationJob{
		SessionID: "test-session",
		Reason:    ConsolidationSessionEnd,
		Messages:  []string{"User: Hello", "Assistant: Hi"},
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	// Wait for processing.
	time.Sleep(500 * time.Millisecond)
	if worker.State("test-session") != ConsolidationSucceeded {
		t.Errorf("expected Succeeded, got %v", worker.State("test-session"))
	}
}

func TestWorkerRejectsAfterClose(t *testing.T) {
	store, _ := newTestStore(t)
	llm := mockCompletion(`{"memories":[]}`, nil)
	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{})
	worker := NewConsolidationWorker(consolidator, ConsolidationWorkerConfig{})
	worker.Close()

	err := worker.Enqueue(ConsolidationJob{
		SessionID: "test-session",
		Reason:    ConsolidationSessionEnd,
		Messages:  []string{"Hello"},
	})
	if err == nil {
		t.Error("expected error after close")
	}
}

func TestWorkerJobTimeout(t *testing.T) {
	store, _ := newTestStore(t)
	llm := func(ctx context.Context, _, _ string) (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
			return "", fmt.Errorf("should not reach here")
		}
	}
	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{})
	worker := NewConsolidationWorker(consolidator, ConsolidationWorkerConfig{
		JobTimeout:  100 * time.Millisecond,
		MaxRetries:  0,
		RetryDelay:  1 * time.Millisecond,
	})
	defer worker.Close()

	err := worker.Enqueue(ConsolidationJob{
		SessionID: "timeout-session",
		Reason:    ConsolidationSessionEnd,
		Messages:  []string{"Hello"},
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	time.Sleep(500 * time.Millisecond)
	if worker.State("timeout-session") != ConsolidationFailed {
		t.Errorf("expected Failed after timeout, got %v", worker.State("timeout-session"))
	}
}

func TestWorkerRetry(t *testing.T) {
	store, _ := newTestStore(t)
	callCount := 0
	llm := func(_ context.Context, _, _ string) (string, error) {
		callCount++
		if callCount == 1 {
			return "", fmt.Errorf("transient failure")
		}
		return `{"memories":[]}`, nil
	}
	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{})
	worker := NewConsolidationWorker(consolidator, ConsolidationWorkerConfig{
		JobTimeout: 5 * time.Second,
		MaxRetries: 2,
		RetryDelay: 10 * time.Millisecond,
	})
	defer worker.Close()

	err := worker.Enqueue(ConsolidationJob{
		SessionID: "retry-session",
		Reason:    ConsolidationSessionEnd,
		Messages:  []string{"Hello"},
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	time.Sleep(1 * time.Second)
	if worker.State("retry-session") != ConsolidationSucceeded {
		t.Errorf("expected Succeeded after retry, got %v", worker.State("retry-session"))
	}
}

func TestWorkerRetryExhausted(t *testing.T) {
	store, _ := newTestStore(t)
	llm := func(_ context.Context, _, _ string) (string, error) {
		return "", fmt.Errorf("permanent failure")
	}
	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{})
	worker := NewConsolidationWorker(consolidator, ConsolidationWorkerConfig{
		JobTimeout: 5 * time.Second,
		MaxRetries: 2,
		RetryDelay: 10 * time.Millisecond,
	})
	defer worker.Close()

	err := worker.Enqueue(ConsolidationJob{
		SessionID: "fail-session",
		Reason:    ConsolidationSessionEnd,
		Messages:  []string{"Hello"},
	})
	if err != nil {
		t.Fatalf("enqueue failed: %v", err)
	}

	time.Sleep(1 * time.Second)
	if worker.State("fail-session") != ConsolidationFailed {
		t.Errorf("expected Failed after retries exhausted, got %v", worker.State("fail-session"))
	}
}

func TestWorkerDuplicateJob(t *testing.T) {
	store, _ := newTestStore(t)
	consolidateCount := 0
	llm := func(_ context.Context, _, _ string) (string, error) {
		return `{"memories":[]}`, nil
	}
	consolidator := &countingConsolidator{
		inner: NewLLMConsolidator(llm, store, ConsolidationPolicy{}),
		count: &consolidateCount,
	}
	worker := NewConsolidationWorker(consolidator, ConsolidationWorkerConfig{
		JobTimeout: 5 * time.Second,
	})
	defer worker.Close()

	_ = worker.Enqueue(ConsolidationJob{
		SessionID: "dup-session",
		Reason:    ConsolidationSessionEnd,
		Messages:  []string{"Hello"},
	})

	// Poll until the first job finishes.
	deadline := time.After(2 * time.Second)
	for {
		if worker.State("dup-session") == ConsolidationSucceeded {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for first consolidation")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Second enqueue for same session should be silently ignored.
	_ = worker.Enqueue(ConsolidationJob{
		SessionID: "dup-session",
		Reason:    ConsolidationSessionEnd,
		Messages:  []string{"Hello again"},
	})
	time.Sleep(200 * time.Millisecond)

	if consolidateCount != 1 {
		t.Errorf("expected 1 consolidation call, got %d", consolidateCount)
	}
}

type countingConsolidator struct {
	inner Consolidator
	count *int
}

func (c *countingConsolidator) Consolidate(ctx context.Context, input ConsolidationInput) (ConsolidationResult, error) {
	*c.count++
	return c.inner.Consolidate(ctx, input)
}

func TestWorkerContextIndependence(t *testing.T) {
	store, _ := newTestStore(t)
	llm := mockCompletion(`{"memories":[]}`, nil)
	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{})
	worker := NewConsolidationWorker(consolidator, ConsolidationWorkerConfig{
		JobTimeout: 5 * time.Second,
	})
	defer worker.Close()

	// Create a context that we cancel immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// The worker should still process the job even though this ctx is cancelled.
	_ = worker.Enqueue(ConsolidationJob{
		SessionID: "ctx-test",
		Reason:    ConsolidationSessionEnd,
		Messages:  []string{"Hello"},
	})
	time.Sleep(500 * time.Millisecond)

	if worker.State("ctx-test") != ConsolidationSucceeded {
		t.Errorf("expected Succeeded despite caller ctx cancel, got %v", worker.State("ctx-test"))
	}
	_ = ctx
}

func TestWorkerShutdownWaits(t *testing.T) {
	store, _ := newTestStore(t)
	var startOnce sync.Once
	started := make(chan struct{})
	llm := func(_ context.Context, _, _ string) (string, error) {
		startOnce.Do(func() { close(started) })
		time.Sleep(200 * time.Millisecond)
		return `{"memories":[]}`, nil
	}
	consolidator := NewLLMConsolidator(llm, store, ConsolidationPolicy{})
	worker := NewConsolidationWorker(consolidator, ConsolidationWorkerConfig{
		JobTimeout:    5 * time.Second,
		ShutdownGrace: 5 * time.Second,
	})

	_ = worker.Enqueue(ConsolidationJob{
		SessionID: "shutdown-test",
		Reason:    ConsolidationSessionEnd,
		Messages:  []string{"Hello"},
	})
	<-started

	err := worker.Close()
	if err != nil {
		t.Errorf("Close should succeed, got: %v", err)
	}
	if worker.State("shutdown-test") != ConsolidationSucceeded {
		t.Errorf("expected Succeeded after graceful shutdown, got %v", worker.State("shutdown-test"))
	}
}
