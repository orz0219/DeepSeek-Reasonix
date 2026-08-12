package jobs

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"reasonix/internal/event"
)

// BeginDestroySession marks a parent session as being removed from active use
// and cancels its running jobs. WaitTeardown waits for the returned handle.
func (m *Manager) BeginDestroySession(parentSession string) SessionTeardown {
	parentSession = strings.TrimSpace(parentSession)
	if parentSession == "" {
		return SessionTeardown{}
	}
	var cancels []context.CancelFunc
	var targets []teardownTarget
	m.mu.Lock()
	m.destroying[parentSession] = true
	remaining := m.completed[:0]
	for _, item := range m.completed {
		if item.sessionID != parentSession {
			remaining = append(remaining, item)
		}
	}
	m.completed = remaining
	for _, key := range m.order {
		j := m.jobs[key]
		if !sessionMatches(parentSession, j.SessionID) {
			continue
		}
		j.mu.Lock()
		switch j.status {
		case Running:
			j.status = Killed
			cancels = append(cancels, j.cancel)
			targets = append(targets, teardownTarget{info: TeardownJob{ID: j.ID, Kind: j.Kind, Label: j.Label}, done: j.done})
		case Killed:
			targets = append(targets, teardownTarget{info: TeardownJob{ID: j.ID, Kind: j.Kind, Label: j.Label}, done: j.done})
		}
		j.mu.Unlock()
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return SessionTeardown{SessionID: parentSession, targets: targets}
}

// DestroySession preserves the legacy channel-based destroy API.
func (m *Manager) DestroySession(parentSession string) []<-chan struct{} {
	return m.BeginDestroySession(parentSession).DoneChannels()
}

// WaitTeardown waits for a destroy handle to unwind up to grace. A timed-out
// result means the caller should defer physical cleanup until the jobs exit.
func (m *Manager) WaitTeardown(ctx context.Context, h SessionTeardown, grace time.Duration) TeardownResult {
	result, timedOut := waitTeardownTargets(ctx, h.targets, grace)
	if timedOut {
		m.emitTeardownTimeout("destroy session "+h.SessionID, result)
	}
	return result
}

// IsDestroying reports whether parentSession is in the destroy window. Empty
// parent sessions are never considered destroyed.
func (m *Manager) IsDestroying(parentSession string) bool {
	parentSession = strings.TrimSpace(parentSession)
	if parentSession == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.destroying[parentSession]
}

// FinishDestroySession ends the destroy window after all owned jobs have unwound
// and persistent cleanup/move work has completed.
func (m *Manager) FinishDestroySession(parentSession string) {
	parentSession = strings.TrimSpace(parentSession)
	if parentSession == "" {
		return
	}
	m.mu.Lock()
	delete(m.destroying, parentSession)
	delete(m.artifactDirs, parentSession)
	delete(m.loaded, parentSession)
	m.purgeSessionLocked(parentSession)
	m.mu.Unlock()
}

func (m *Manager) purgeSessionLocked(parentSession string) {
	kept := m.order[:0]
	for _, key := range m.order {
		j := m.jobs[key]
		if j == nil || sessionMatches(parentSession, j.SessionID) {
			delete(m.jobs, key)
			continue
		}
		kept = append(kept, key)
	}
	m.order = kept
}

// Close cancels the session context and waits briefly for every background job
// goroutine to return before unblocking. If a non-cooperative job ignores
// cancellation, cleanup of the temporary artifact root continues in the
// background after the goroutines eventually unwind.
func (m *Manager) Close() {
	_ = m.CloseWithGrace(m.teardownGrace)
}

// CloseAsync cancels the manager and returns immediately. It is used when a
// caller has already begun session-specific teardown and owns the delayed
// persistent cleanup, but still needs the manager's root context and temporary
// artifact root released eventually.
func (m *Manager) CloseAsync() {
	m.cancel()
	go func() {
		m.wg.Wait()
		m.releaseOwner()
		m.removeTempRoot()
	}()
}

// CloseWithGrace is Close with an explicit wait window, used by tests and
// callers that need to surface non-cooperative jobs.
func (m *Manager) CloseWithGrace(grace time.Duration) TeardownResult {
	m.cancel()
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		m.releaseOwner()
		close(done)
	}()
	result, timedOut := waitTeardownTargets(context.Background(), m.closeTargets(), grace, done)
	if timedOut {
		m.emitTeardownTimeout("close", result)
		go func() {
			<-done
			m.removeTempRoot()
		}()
		return result
	}
	m.removeTempRoot()
	return result
}

func waitTeardownTargets(ctx context.Context, targets []teardownTarget, grace time.Duration, allDone ...<-chan struct{}) (TeardownResult, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	var timeout <-chan time.Time
	if grace >= 0 {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		timeout = timer.C
	}
	if len(allDone) > 0 && allDone[0] != nil {
		select {
		case <-allDone[0]:
			return TeardownResult{}, false
		case <-ctx.Done():
			return teardownTimedOut(targets, time.Since(start)), false
		case <-timeout:
			return teardownTimedOut(targets, time.Since(start)), true
		}
	}
	for _, target := range targets {
		select {
		case <-target.done:
		case <-ctx.Done():
			return teardownTimedOut(targets, time.Since(start)), false
		case <-timeout:
			return teardownTimedOut(targets, time.Since(start)), true
		}
	}
	return TeardownResult{}, false
}

func teardownTimedOut(targets []teardownTarget, waited time.Duration) TeardownResult {
	var out []TeardownJob
	for _, target := range targets {
		select {
		case <-target.done:
			continue
		default:
		}
		info := target.info
		info.Waited = waited
		out = append(out, info)
	}
	return TeardownResult{TimedOut: out}
}

func (m *Manager) closeTargets() []teardownTarget {
	m.mu.Lock()
	defer m.mu.Unlock()
	var targets []teardownTarget
	for _, key := range m.order {
		j := m.jobs[key]
		if j == nil {
			continue
		}
		select {
		case <-j.done:
			continue
		default:
		}
		j.mu.Lock()
		switch j.status {
		case Running:
			j.status = Killed
			targets = append(targets, teardownTarget{info: TeardownJob{ID: j.ID, Kind: j.Kind, Label: j.Label}, done: j.done})
		case Killed:
			targets = append(targets, teardownTarget{info: TeardownJob{ID: j.ID, Kind: j.Kind, Label: j.Label}, done: j.done})
		}
		j.mu.Unlock()
	}
	return targets
}

func (m *Manager) emitTeardownTimeout(action string, result TeardownResult) {
	if len(result.TimedOut) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "background job teardown timed out during %s", strings.TrimSpace(action))
	for i, job := range result.TimedOut {
		if i == 0 {
			b.WriteString(": ")
		} else {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s kind=%s", job.ID, job.Kind)
		if strings.TrimSpace(job.Label) != "" {
			fmt.Fprintf(&b, " label=%q", job.Label)
		}
		if job.Waited > 0 {
			fmt.Fprintf(&b, " waited=%s", job.Waited.Round(time.Millisecond))
		}
	}
	m.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "Background job teardown timed out.", Detail: b.String()})
}

func (m *Manager) removeTempRoot() {
	if m.tempRoot != "" {
		_ = os.RemoveAll(m.tempRoot)
	}
}

func nowMs() int64 { return time.Now().UnixMilli() }

func startedText(kind, id, label string) string {
	if label != "" {
		return fmt.Sprintf("background %s started: %s (%s)", kind, id, label)
	}
	return fmt.Sprintf("background %s started: %s", kind, id)
}

func (m *Manager) emitIfActive(parentSession string, ev event.Event) {
	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	if active == "" || strings.TrimSpace(parentSession) == "" || active == strings.TrimSpace(parentSession) {
		m.sink.Emit(ev)
	}
}

func sessionMatches(filter, jobSession string) bool {
	filter = strings.TrimSpace(filter)
	return filter == "" || strings.TrimSpace(jobSession) == filter
}

func jobKey(parentSession, id string) string {
	return strings.TrimSpace(parentSession) + "\x00" + strings.TrimSpace(id)
}

type ctxKey struct{}
type sessionCtxKey struct{}
type jobCtxKey struct{}
