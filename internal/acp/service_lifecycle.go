package acp

import (
	"context"
	"errors"

	"reasonix/internal/agent"
	"reasonix/internal/control"
)

func (s *acpSession) begin(ctx context.Context) (context.Context, context.CancelFunc, bool) {
	runCtx, cancel := context.WithCancel(ctx)

	if !s.stateChangeMu.TryLock() {
		cancel()
		return nil, nil, false
	}
	defer s.stateChangeMu.Unlock()
	s.mu.Lock()

	if s.running || s.deleted || s.maintenanceDone != nil || len(s.pendingConfig) > 0 {
		s.mu.Unlock()
		cancel()
		return nil, nil, false
	}
	s.running = true
	s.cancel = cancel
	s.done = make(chan struct{})
	s.mu.Unlock()
	return runCtx, cancel, true
}

func (s *acpSession) finish() {
	s.mu.Lock()
	done := s.done
	s.running = false
	s.cancel = nil
	s.done = nil
	s.mu.Unlock()
	if done != nil {
		close(done)
	}
}

func (s *acpSession) abort() {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

func (s *acpSession) abortAndWait() {
	s.mu.Lock()
	c := s.cancel
	done := s.done
	maintenanceDone := s.maintenanceDone
	s.mu.Unlock()
	if c != nil {
		c()
	}
	if done != nil {
		<-done
	}
	if maintenanceDone != nil {
		<-maintenanceDone
	}
}

func (s *acpSession) deleteAndWait() {
	s.mu.Lock()
	s.deleted = true
	c := s.cancel
	done := s.done
	maintenanceDone := s.maintenanceDone
	s.mu.Unlock()
	if c != nil {
		c()
	}
	if done != nil {
		<-done
	}
	if maintenanceDone != nil {
		<-maintenanceDone
	}
}

func (s *acpSession) finishMaintenance(done chan struct{}) {
	if done == nil {
		return
	}
	closeDone := false
	s.mu.Lock()
	if s.maintenanceDone == done {
		s.maintenanceDone = nil
		closeDone = true
	}
	s.mu.Unlock()
	if closeDone {
		close(done)
	}
}

// swapModeID records the mode reported to the client and returns the previous
// value, so callers can emit current_mode_update only on change.
func (s *acpSession) swapModeID(id string) (old string) {
	s.mu.Lock()
	old = s.modeID
	s.modeID = id
	s.mu.Unlock()
	return old
}

// currentModeID returns the mode last reported to the client.
func (s *acpSession) currentModeID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modeID == "" {
		return sessionModeNormal
	}
	return s.modeID
}

func (s *acpSession) setGoalDraftMode(on bool) {
	s.mu.Lock()
	s.goalDraftMode = on
	s.mu.Unlock()
}

func (s *acpSession) takeGoalDraftMode() bool {
	s.mu.Lock()
	on := s.goalDraftMode
	s.goalDraftMode = false
	s.mu.Unlock()
	return on
}

func (s *acpSession) isGoalDraftMode() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.goalDraftMode
}

func (s *acpSession) setToolApprovalMode(mode string) {
	s.mu.Lock()
	s.toolApprovalMode = normalizeACPToolApprovalMode(mode)
	s.mu.Unlock()
}

func (s *acpSession) swapToolApprovalMode(mode string) (old string) {
	mode = normalizeACPToolApprovalMode(mode)
	s.mu.Lock()
	old = normalizeACPToolApprovalMode(s.toolApprovalMode)
	s.toolApprovalMode = mode
	s.mu.Unlock()
	return old
}

func (s *acpSession) saveMetaIfPresent() {
	s.mu.Lock()
	path := s.transcript
	meta := s.metaLocked()
	s.mu.Unlock()
	if path != "" && sessionFileExists(path) {
		_ = saveACPMeta(path, meta)
	}
}

// currentCtrl returns the session's controller under mu. rebuildSession swaps
// ctrl while holding mu, so any read of the field outside mu races with a
// concurrent config rebuild; always go through this accessor unless mu is
// already held.
func (s *acpSession) currentCtrl() acpController {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctrl
}

// releaseSessionLease drops the session's transcript lease, if any. Idempotent.
func (s *acpSession) releaseSessionLease() {
	s.mu.Lock()
	lease := s.lease
	s.lease = nil
	s.mu.Unlock()
	if lease != nil {
		lease.Release()
	}
	s.waitForRetiredSessionLeases()
}

// retireSessionLease defers Release until the authority-guarded save that
// invoked a recovery callback can return. Releasing synchronously inside that
// callback would wait on the very save executing the callback and deadlock.
func (s *acpSession) retireSessionLease(lease *agent.SessionLease) {
	if lease == nil {
		return
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.retiredLeases = append(s.retiredLeases, done)
	s.mu.Unlock()
	go func() {
		lease.Release()
		close(done)
	}()
}

func (s *acpSession) waitForRetiredSessionLeases() {
	s.mu.Lock()
	retired := append([]<-chan struct{}(nil), s.retiredLeases...)
	s.mu.Unlock()
	for _, done := range retired {
		<-done
	}
	s.mu.Lock()
	pending := s.retiredLeases[:0]
	for _, done := range s.retiredLeases {
		select {
		case <-done:
		default:
			pending = append(pending, done)
		}
	}
	s.retiredLeases = pending
	s.mu.Unlock()
}

// sessionLeaseBindError maps a lease-acquisition failure to the protocol
// error the client sees: a held session names its holder with the shared CLI
// wording; anything else is an internal error.
func sessionLeaseBindError(method string, err error) *RPCError {
	if errors.Is(err, agent.ErrSessionLeaseHeld) {
		return &RPCError{
			Code:    ErrInvalidRequest,
			Message: method + ": " + control.SessionInUseMessage(err) + "; " + control.SessionLeaseCloseHint,
		}
	}
	return &RPCError{Code: ErrInternal, Message: method + ": session lease: " + err.Error()}
}
