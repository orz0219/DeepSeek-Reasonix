// Package stats records per-call token usage as append-only daily JSONL files
// under the user state root (config.StatsDir), and aggregates them for the
// desktop "usage statistics" panel.
//
// Design notes:
//   - Only provider usage (including request-only failures) and turn
//     completions (event.TurnDone) are recorded here. Turn markers power the
//     panel's "completed turns" metric;
//     they are deliberately not presented as distinct conversation sessions.
//     Token usage was never persisted before this feature, so token numbers
//     accumulate from the day the feature ships.
//   - Files are append-only: each record is one JSON line appended with
//     O_APPEND. A crash mid-line leaves at most one torn trailing line, which
//     decodeRecords tolerates and skips.
package stats

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reasonix/internal/filelock"
	"reasonix/internal/usagecatalog"
)

// dayLayout names one stats file per UTC-free local day, e.g. 2026-08-02.jsonl.
const dayLayout = "2006-01-02"

const appendLockTimeout = 2 * time.Second

// record is one line in a daily stats file. TurnDone marks a completed turn so
// per-day turn counts are available without touching session files.
type record struct {
	Timestamp  time.Time `json:"ts"`
	ModelRef   string    `json:"model,omitempty"`  // canonical "provider/model"
	Source     string    `json:"source,omitempty"` // desktop | cli | serve | bot | remote
	Prompt     int       `json:"prompt,omitempty"`
	Completion int       `json:"completion,omitempty"`
	Reasoning  int       `json:"reasoning,omitempty"`
	CacheHit   int       `json:"cache_hit,omitempty"`
	CacheMiss  int       `json:"cache_miss,omitempty"`
	Total      int       `json:"total,omitempty"`
	Requests   int       `json:"requests,omitempty"` // provider requests represented by this row
	Turn       bool      `json:"turn,omitempty"`     // true for TurnDone marker rows
	// Cost quote fields (additive; older readers ignore them).
	UsageSource        string   `json:"usage_source,omitempty"`
	CostAmount         string   `json:"cost_amount,omitempty"`     // original amount decimal
	CostCurrency       string   `json:"cost_currency,omitempty"`   // original ISO
	SelectedAmount     string   `json:"selected_amount,omitempty"` // display valuation
	SelectedCurrency   string   `json:"selected_currency,omitempty"`
	CostComplete       *bool    `json:"cost_complete,omitempty"`
	DisplayComplete    *bool    `json:"display_complete,omitempty"`
	DisplayStatus      string   `json:"display_status,omitempty"`
	AggregateMode      string   `json:"aggregate_mode,omitempty"`
	OriginalTotals     []string `json:"original_totals,omitempty"`
	CostEstimated      bool     `json:"cost_estimated,omitempty"`
	LegacyEstimate     bool     `json:"legacy_estimate,omitempty"`
	PricingFingerprint string   `json:"pricing_fingerprint,omitempty"`
	RateDate           string   `json:"rate_date,omitempty"`
	IncompleteReason   string   `json:"incomplete_reason,omitempty"`
	BillingMode        string   `json:"billing_mode,omitempty"`
	// ValuationCNY/USD amounts when present (occurrence-time).
	ValuationCNY string `json:"valuation_cny,omitempty"`
	ValuationUSD string `json:"valuation_usd,omitempty"`
	// SelectedCost is a float compatibility mirror of SelectedAmount.
	SelectedCost float64 `json:"selected_cost,omitempty"`
}

// Writer appends records to the daily stats file for a given stats dir.
type Writer struct {
	dir   string
	usage *usageManager
}

// NewWriter returns a Writer rooted at dir. An empty dir disables recording
// (query-only usage).
func NewWriter(dir string) *Writer {
	dir = strings.TrimSpace(dir)
	return &Writer{dir: dir}
}

// Append writes one record, appending to the daily file (O_APPEND) so records
// from concurrent turns never overwrite each other. Each record is a single
// JSON line; a crash mid-line leaves at most one torn trailing line, which
// decodeRecords tolerates.
func (w *Writer) Append(r record) error {
	if w == nil || w.dir == "" {
		return nil
	}
	day := r.Timestamp.Format(dayLayout)
	path := filepath.Join(w.dir, day+".jsonl")
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), appendLockTimeout)
	defer cancel()
	release, err := filelock.Acquire(ctx, filepath.Join(w.dir, ".append.lock"))
	if err != nil {
		return err
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := ensureRecordBoundary(f); err != nil {
		_ = f.Close()
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	offset := info.Size()
	line := append(b, '\n')
	if _, err = f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	release()
	released = true
	if w.usage != nil {
		if catalog := w.usage.catalog.Load(); catalog != nil {
			hash := sha256.Sum256(b)
			catalog.Enqueue(usagecatalog.AppendReceipt{Path: path, Day: day, Offset: offset, Length: len(line), LineHash: fmtHash(hash[:])}, usageEntry(day, r))
		}
	}
	return nil
}

func fmtHash(hash []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(hash)*2)
	for i, value := range hash {
		out[i*2] = digits[value>>4]
		out[i*2+1] = digits[value&15]
	}
	return string(out)
}

// ensureRecordBoundary separates a torn trailing JSON object from the next
// append. The caller holds the cross-process append lock, so checking the last
// byte and repairing it cannot race another Reasonix writer.
func ensureRecordBoundary(f *os.File) error {
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	var tail [1]byte
	if _, err := f.ReadAt(tail[:], st.Size()-1); err != nil {
		return err
	}
	if tail[0] == '\n' {
		return nil
	}
	_, err = f.Write([]byte{'\n'})
	return err
}
