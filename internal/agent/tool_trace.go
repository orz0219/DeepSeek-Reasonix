package agent

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// toolCallTrace records per-stage wall-clock durations for one tool call.
// Allocated only when tracing is enabled (benchmark / diagnostic mode).
type toolCallTrace struct {
	callID string

	parseStart     time.Time
	parseDone      time.Time
	interceptStart time.Time
	interceptDone  time.Time
	policyStart    time.Time
	policyDone     time.Time
	prepareStart   time.Time
	prepareDone    time.Time
	executeStart   time.Time
	executeDone    time.Time
	finalizeStart  time.Time
	finalizeDone   time.Time
}

func (t *toolCallTrace) total() time.Duration {
	if t == nil {
		return 0
	}
	return t.finalizeDone.Sub(t.parseStart)
}

func (t *toolCallTrace) executeDuration() time.Duration {
	if t == nil {
		return 0
	}
	return t.executeDone.Sub(t.executeStart)
}

func (t *toolCallTrace) hostOverhead() time.Duration {
	return t.total() - t.executeDuration()
}

// traceEnabled gates trace allocation. Atomic so benchmarks can toggle it.
var traceEnabled atomic.Bool

// enableTracing turns on per-call trace collection. Returns a disable func.
func enableTracing() func() {
	traceEnabled.Store(true)
	return func() { traceEnabled.Store(false) }
}

func newTrace(callID string) *toolCallTrace {
	if !traceEnabled.Load() {
		return nil
	}
	return &toolCallTrace{callID: callID}
}

// traceReport is the accumulated result for a batch of tool calls.
type traceReport struct {
	calls []toolCallTrace
}

func (r *traceReport) add(t *toolCallTrace) {
	if t != nil {
		r.calls = append(r.calls, *t)
	}
}

// PrintReport writes a human-readable per-call timing breakdown.
func (r *traceReport) PrintReport(w io.Writer) {
	if r == nil || len(r.calls) == 0 {
		fmt.Fprintln(w, "no trace data")
		return
	}

	var totalOverhead, totalExec, totalAll time.Duration
	var parseTotal, policyTotal, prepareTotal, finalizeTotal time.Duration

	for _, c := range r.calls {
		total := c.total()
		exec := c.executeDuration()
		overhead := total - exec
		totalAll += total
		totalExec += exec
		totalOverhead += overhead
		parseTotal += c.parseDone.Sub(c.parseStart)
		policyTotal += c.policyDone.Sub(c.policyStart)
		prepareTotal += c.prepareDone.Sub(c.prepareStart)
		finalizeTotal += c.finalizeDone.Sub(c.finalizeStart)
	}

	n := time.Duration(len(r.calls))
	fmt.Fprintf(w, "--- Tool Call Trace Report (%d calls) ---\n", len(r.calls))
	fmt.Fprintf(w, "  %-20s %10s  (per-call avg)\n", "Phase", "Total")
	fmt.Fprintf(w, "  %-20s %10s\n", "parse", parseTotal)
	fmt.Fprintf(w, "  %-20s %10s\n", "policy", policyTotal)
	fmt.Fprintf(w, "  %-20s %10s\n", "prepare", prepareTotal)
	fmt.Fprintf(w, "  %-20s %10s\n", "execute", totalExec)
	fmt.Fprintf(w, "  %-20s %10s\n", "finalize", finalizeTotal)
	fmt.Fprintf(w, "  %-20s %10s\n", "---", "---")
	fmt.Fprintf(w, "  %-20s %10s  (%s/call)\n", "total", totalAll, totalAll/n)
	fmt.Fprintf(w, "  %-20s %10s  (%s/call)\n", "host overhead", totalOverhead, totalOverhead/n)
	fmt.Fprintf(w, "  %-20s %10s\n", "overhead ratio", fmt.Sprintf("%.1f%%", float64(totalOverhead)/float64(totalAll)*100))
}
