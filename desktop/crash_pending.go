package main

// crash_pending.go keeps the goroutine panic guard: a panicking goroutine is
// recovered and re-raised so the process still crashes as before. The
// pending-crash queue and its next-launch network flush were removed along with
// all other crash reporting.

func (a *App) recoverToPending(site string) {
	r := recover()
	if r == nil {
		return
	}
	// The panic is re-raised so the runtime's SetCrashOutput dump (crash-fatal)
	// and any crash handler still observe it exactly as before.
	panic(r)
}

func (a *App) goSafe(site string, fn func()) {
	go func() {
		defer a.recoverToPending(site)
		fn()
	}()
}
