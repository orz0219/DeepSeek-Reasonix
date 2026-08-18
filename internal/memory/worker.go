// Background consolidation worker: decouples memory consolidation from the
// session lifecycle. The controller enqueues immutable snapshots; the worker
// processes them asynchronously with its own context and per-job timeout.
package memory

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ConsolidationReason identifies what triggered a consolidation job.
type ConsolidationReason string

const (
	ConsolidationSessionEnd ConsolidationReason = "session_end"
	ConsolidationCompact    ConsolidationReason = "compact"
	ConsolidationExplicit   ConsolidationReason = "explicit"
)

// ConsolidationState tracks a single job's lifecycle.
type ConsolidationState int

const (
	ConsolidationIdle ConsolidationState = iota
	ConsolidationRunning
	ConsolidationSucceeded
	ConsolidationFailed
)

// ConsolidationJob is an immutable snapshot submitted to the worker.
// The controller populates it at enqueue time so the worker never reads
// a live session that may have changed or been closed.
type ConsolidationJob struct {
	SessionID        string
	Reason           ConsolidationReason
	Messages         []string
	ExistingMemories []Memory
	SuppressedKeys   []string
}

// ConsolidationWorkerConfig tunes the worker. Zero values select defaults.
type ConsolidationWorkerConfig struct {
	QueueSize   int           // buffered channel capacity (default 4)
	JobTimeout  time.Duration // per-job deadline (default 3 min)
	MaxRetries  int           // retry attempts on failure (default 3)
	RetryDelay  time.Duration // base delay between retries (default 5s)
	ShutdownGrace time.Duration // max wait for running jobs on close (default 30s)
}

func (c ConsolidationWorkerConfig) queueSize() int {
	if c.QueueSize > 0 {
		return c.QueueSize
	}
	return 4
}

func (c ConsolidationWorkerConfig) jobTimeout() time.Duration {
	if c.JobTimeout > 0 {
		return c.JobTimeout
	}
	return 3 * time.Minute
}

func (c ConsolidationWorkerConfig) maxRetries() int {
	if c.MaxRetries > 0 {
		return c.MaxRetries
	}
	return 3
}

func (c ConsolidationWorkerConfig) retryDelay() time.Duration {
	if c.RetryDelay > 0 {
		return c.RetryDelay
	}
	return 5 * time.Second
}

func (c ConsolidationWorkerConfig) shutdownGrace() time.Duration {
	if c.ShutdownGrace > 0 {
		return c.ShutdownGrace
	}
	return 30 * time.Second
}

// ConsolidationWorker processes consolidation jobs asynchronously. It owns
// its own context and never depends on a request context.
type ConsolidationWorker struct {
	consolidator Consolidator
	config       ConsolidationWorkerConfig

	ctx    context.Context
	cancel context.CancelFunc
	queue  chan ConsolidationJob
	wg     sync.WaitGroup

	mu       sync.Mutex
	closed   bool           // true after Close is called
	succeeded map[string]bool  // session IDs that completed successfully
	states    map[string]ConsolidationState
}

// NewConsolidationWorker creates a worker that processes consolidation jobs
// in a background goroutine. Call Close to stop it.
func NewConsolidationWorker(consolidator Consolidator, cfg ConsolidationWorkerConfig) *ConsolidationWorker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &ConsolidationWorker{
		consolidator: consolidator,
		config:       cfg,
		ctx:          ctx,
		cancel:       cancel,
		queue:        make(chan ConsolidationJob, cfg.queueSize()),
		succeeded:    make(map[string]bool),
		states:       make(map[string]ConsolidationState),
	}
	w.wg.Add(1)
	go w.run()
	return w
}

// Enqueue submits a consolidation job. Returns an error if the worker is
// closed or the queue is full (non-blocking).
func (w *ConsolidationWorker) Enqueue(job ConsolidationJob) error {
	if job.SessionID == "" {
		return fmt.Errorf("consolidation job missing session ID")
	}
	w.mu.Lock()
	if w.succeeded[job.SessionID] {
		w.mu.Unlock()
		return nil // already processed successfully
	}
	if w.closed {
		w.mu.Unlock()
		return fmt.Errorf("consolidation worker is closed")
	}
	w.mu.Unlock()

	select {
	case <-w.ctx.Done():
		return fmt.Errorf("consolidation worker is closed")
	case w.queue <- job:
		return nil
	default:
		return fmt.Errorf("consolidation queue is full")
	}
}

// Close stops accepting new jobs and waits for the current job to finish.
// It respects the configured shutdown grace; if the grace expires, remaining
// jobs are abandoned.
func (w *ConsolidationWorker) Close() error {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.cancel()
	close(w.queue)

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(w.config.shutdownGrace()):
		return fmt.Errorf("consolidation worker shutdown timed out after %s", w.config.shutdownGrace())
	}
}

func (w *ConsolidationWorker) run() {
	defer w.wg.Done()
	for job := range w.queue {
		w.processJob(job)
	}
}

func (w *ConsolidationWorker) processJob(job ConsolidationJob) {
	w.mu.Lock()
	if w.succeeded[job.SessionID] {
		w.mu.Unlock()
		return
	}
	w.states[job.SessionID] = ConsolidationRunning
	w.mu.Unlock()

	var lastErr error
	maxRetries := w.config.maxRetries()

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := w.config.retryDelay() * time.Duration(attempt)
			select {
			case <-w.ctx.Done():
				w.markFailed(job.SessionID, fmt.Errorf("worker shutdown during retry"))
				return
			case <-time.After(delay):
			}
		}

		jobCtx, jobCancel := context.WithTimeout(w.ctx, w.config.jobTimeout())
		result, err := w.consolidator.Consolidate(jobCtx, ConsolidationInput{
			SessionID:        job.SessionID,
			Messages:         job.Messages,
			ExistingMemories: job.ExistingMemories,
			SuppressedKeys:   job.SuppressedKeys,
		})
		jobCancel()

		if err == nil && result.Error == "" {
			w.markSucceeded(job.SessionID)
			slog.Info("memory consolidation succeeded",
				"session", job.SessionID,
				"reason", string(job.Reason),
				"added", len(result.Added),
				"updated", len(result.Updated),
				"archived", len(result.Archived),
				"ignored", result.Ignored,
				"attempt", attempt+1,
			)
			return
		}

		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("consolidation pipeline error: %s", result.Error)
		}

		slog.Warn("memory consolidation attempt failed",
			"session", job.SessionID,
			"reason", string(job.Reason),
			"attempt", attempt+1,
			"maxAttempts", maxRetries+1,
			"err", lastErr,
		)
	}

	w.markFailed(job.SessionID, lastErr)
	slog.Warn("memory consolidation failed after all retries",
		"session", job.SessionID,
		"reason", string(job.Reason),
		"attempts", maxRetries+1,
		"err", lastErr,
	)
}

func (w *ConsolidationWorker) markSucceeded(sessionID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.succeeded[sessionID] = true
	w.states[sessionID] = ConsolidationSucceeded
}

func (w *ConsolidationWorker) markFailed(sessionID string, _ error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.states[sessionID] = ConsolidationFailed
	delete(w.succeeded, sessionID)
}

// State returns the current consolidation state for a session.
func (w *ConsolidationWorker) State(sessionID string) ConsolidationState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.states[sessionID]
}
