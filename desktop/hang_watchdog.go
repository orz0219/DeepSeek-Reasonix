package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

const (
	mainThreadHeartbeatInterval = time.Second
	mainThreadHangThreshold     = 12 * time.Second
	mainThreadHangCheckInterval = 2 * time.Second
	mainThreadSleepSkip         = 30 * time.Second
)

var (
	mainThreadClockBase            = time.Now()
	mainThreadLastHeartbeatElapsed atomic.Int64
	mainThreadLastHeartbeatWall    atomic.Int64
	mainThreadHangReported         atomic.Bool
)

func recordMainThreadHeartbeat(t time.Time) {
	elapsed := max(t.Sub(mainThreadClockBase), 0)
	mainThreadLastHeartbeatElapsed.Store(int64(elapsed))
	mainThreadLastHeartbeatWall.Store(t.UnixNano())
}

func mainThreadHeartbeatAge(now time.Time) (time.Duration, time.Time, bool) {
	lastElapsed := time.Duration(mainThreadLastHeartbeatElapsed.Load())
	lastWall := mainThreadLastHeartbeatWall.Load()
	if lastWall <= 0 {
		return 0, time.Time{}, false
	}
	age := max(now.Sub(mainThreadClockBase)-lastElapsed, 0)
	return age, time.Unix(0, lastWall), true
}

func resetMainThreadHeartbeatAfterSleep(lastCheck, now time.Time) bool {
	if now.Sub(lastCheck) <= mainThreadSleepSkip {
		return false
	}
	// The native UI heartbeat and this Go ticker are both suspended while the
	// machine sleeps. Treat wake as a fresh observation epoch so the sleep gap
	// cannot be reported as a multi-minute UI-thread hang on the next tick.
	recordMainThreadHeartbeat(now)
	return true
}

func (a *App) startMainThreadWatchdog() {
	if !mainThreadWatchdogSupported() {
		return
	}
	a.hangWatchdogMu.Lock()
	if a.hangWatchdogCancel != nil {
		a.hangWatchdogMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.hangWatchdogCancel = cancel
	mainThreadHangReported.Store(false)
	recordMainThreadHeartbeat(time.Now())
	startNativeMainThreadHeartbeat(uint64(mainThreadHeartbeatInterval / time.Millisecond))
	a.hangWatchdogMu.Unlock()

	a.goSafe("mainThreadHangWatchdog", func() {
		a.watchMainThreadHeartbeat(ctx)
	})
}

func (a *App) stopMainThreadWatchdog() {
	if !mainThreadWatchdogSupported() {
		return
	}
	a.hangWatchdogMu.Lock()
	cancel := a.hangWatchdogCancel
	a.hangWatchdogCancel = nil
	a.hangWatchdogMu.Unlock()
	if cancel != nil {
		cancel()
	}
	stopNativeMainThreadHeartbeat()
}

func (a *App) watchMainThreadHeartbeat(ctx context.Context) {
	ticker := time.NewTicker(mainThreadHangCheckInterval)
	defer ticker.Stop()
	lastCheck := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if resetMainThreadHeartbeatAfterSleep(lastCheck, now) {
				lastCheck = now
				continue
			}
			lastCheck = now
			age, last, ok := mainThreadHeartbeatAge(now)
			if !ok {
				continue
			}
			if age < mainThreadHangThreshold {
				continue
			}
			if mainThreadHangReported.CompareAndSwap(false, true) {
				a.recordMainThreadHang(age, last, now)
			}
		}
	}
}

func (a *App) recordMainThreadHang(age time.Duration, lastHeartbeat, observedAt time.Time) {
	// Local log only; crash/metrics reporting was removed.
	slog.Warn("desktop: native UI thread heartbeat stalled",
		"age", age.Round(time.Millisecond).String(),
		"lastHeartbeat", lastHeartbeat.Format(time.RFC3339),
		"observedAt", observedAt.Format(time.RFC3339),
	)
}
