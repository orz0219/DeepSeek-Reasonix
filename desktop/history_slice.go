package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"reasonix/internal/provider"
)

// This file implements the windowed history paging API (Phase B1 of the
// history-pipeline refactor). HistoryPageForTab copies and converts the whole
// transcript on every request; HistorySliceForTab pages toward older history
// using the per-session display index sidecar (internal/agent
// SessionDisplayIndex) so only the returned window is read from disk and
// converted. The legacy API stays untouched for one compatibility cycle.
//
// Entry ID scheme: s<sessionFileID>:r<rewriteEpoch>:m<messageIndex>:o<subOrder>.
//   - sessionFileID is the transcript basename minus .jsonl — stable for the
//     life of the session file.
//   - rewriteEpoch is the persisted rewrite version (live) or the index
//     revision (cold, 0 when unknown). Append-only saves keep it, so entry IDs
//     of an unchanged prefix survive appends; rewrites (compaction, rewind)
//     bump it, and cursors go stale on rewrites anyway.
//   - messageIndex is the absolute provider-message index; subOrder is the
//     row number within that message's conversion (one message maps to 0..n
//     history rows: notices, planner turns, …).
//
// Cursor format: base64url(JSON{v, revision, revKnown, digest, before}).
// The cursor binds the page to the session's persisted revision + content
// digest; any save bumps the revision, so continuing with a pre-save cursor
// returns HistorySlice{Stale: true} and the frontend reloads the latest page.
// "before" is the absolute provider-message index the next page ends at
// (exclusive), which carries the intra-turn position for oversized turns:
// pages always cut at message boundaries, so concatenating pages reproduces
// the full conversion exactly — no duplication, no omission.
//
// Visible-turn mapping: the display index's AuthoredTurn is counted with
// IsUserAuthoredTurn semantics, which already excludes synthetic and steer
// user messages — exactly the desktop visible-turn rule — so an entry's
// visible turn is its AuthoredTurn (1-based; 0 = before the first turn). The
// unsaved in-memory tail of a live session is classified with the real
// resolver-based rule (isVisibleHistoryUser), matching today's behavior.

const (
	defaultHistorySliceTurns   = 12
	defaultHistorySliceEntries = 120
	defaultHistorySliceBytes   = 512 << 10
	maxHistorySliceTurns       = 500
	maxHistorySliceEntries     = 1000
	maxHistorySliceBytes       = 8 << 20

	// historyInlineRefThreshold is the field size above which a string field
	// is replaced inline by a preview + HistoryContentRef.
	historyInlineRefThreshold = 64 << 10
	// historyFieldPreviewBytes is the rune-safe inline preview kept for a
	// ref-replaced field. The full value stays retrievable via
	// HistoryContentForTab.
	historyFieldPreviewBytes = 4 << 10
	// historyContentChunkBytes is the HistoryContentForTab chunk size. Chunks
	// split on UTF-8 rune boundaries, never mid-rune.
	historyContentChunkBytes = 256 << 10

	// historySliceColdWindowBytes caps the raw transcript span one cold-path
	// page reads from disk. Inline output is still bounded by the byte budget;
	// this cap only keeps windows dense with multi-megabyte image lines from
	// reading unbounded file spans.
	historySliceColdWindowBytes = 32 << 20
	// historyLookupChunkMessages bounds the number of decoded messages retained
	// while deriving cross-page planner/todo state.
	historyLookupChunkMessages = 128
	historyDerivedCacheEntries = 4
)

// HistorySliceRequest is one page request. Cursor empty = latest page.
type HistorySliceRequest struct {
	Cursor  string `json:"cursor"`
	Turns   int    `json:"turns"`   // default 12
	Entries int    `json:"entries"` // default 120
	Bytes   int    `json:"bytes"`   // inline byte budget, default 512KiB
}

// HistoryContentRef marks a string field that exceeded the inline threshold.
// The field carries a rune-safe preview prefix; the full value is retrievable
// in chunks via HistoryContentForTab.
type HistoryContentRef struct {
	EntryID string `json:"entryId"`
	Field   string `json:"field"` // "content", "reasoning", "submitText", "detail", "code", "summary", "archive", "toolResultError", "toolArguments", "toolSubject", "toolSummary", "toolDiff"
	Size    int    `json:"size"`
	Chunks  int    `json:"chunks"`
	// ToolCallID identifies the tool call for tool* fields.
	ToolCallID string `json:"toolCallId,omitempty"`
	// Revision/RevKnown/Digest bind the ref to the session state it was cut
	// from; a mismatch on fetch resolves to Stale.
	Revision int64  `json:"revision"`
	RevKnown bool   `json:"revKnown,omitempty"`
	Digest   string `json:"digest"`
}

// HistoryEntry is one display row in a history page.
type HistoryEntry struct {
	EntryID string `json:"entryId"`
	// Turn is the absolute visible turn the row belongs to (1-based; 0 =
	// before the first visible turn).
	Turn int `json:"turn"`
	// Order is the absolute provider-message index the row was converted
	// from; combined with the sub-order in EntryID it is strictly increasing
	// in display order.
	Order   int            `json:"order"`
	Message HistoryMessage `json:"message"`
	// Refs lists the message fields replaced by previews. Always initialized
	// so JSON encodes [] rather than null.
	Refs []HistoryContentRef `json:"refs"`
}

// HistorySlice is one page of history toward older messages.
type HistorySlice struct {
	Entries    []HistoryEntry `json:"entries"`
	NextCursor string         `json:"nextCursor"` // toward older; empty when none
	HasOlder   bool           `json:"hasOlder"`
	TotalTurns int            `json:"totalTurns"`
	StartTurn  int            `json:"startTurn"` // oldest visible turn in the page (0 when none)
	EndTurn    int            `json:"endTurn"`   // newest visible turn in the page (0 when none)
	Stale      bool           `json:"stale"`     // cursor bound to an older session revision
	Revision   int64          `json:"revision"`  // session revision the page was cut from (0 when unknown)
	// RevisionKnown and Digest expose the complete canonical identity already
	// carried by cursors. They let same-path resident frontend projections be
	// invalidated after another process advances or rewrites the session.
	RevisionKnown bool   `json:"revisionKnown,omitempty"`
	Digest        string `json:"digest,omitempty"`
	// Source: index|scan|live-index|live-fallback. Error marks a failed read
	// (empty Entries alone means a genuinely empty session).
	Source string `json:"source,omitempty"`
	Error  string `json:"error,omitempty"`
	// AppendOnly marks that the session was append-only when the page was cut,
	// so a frontend holding an older page identity may adopt this page's
	// identity and prepend instead of discarding it.
	AppendOnly bool `json:"appendOnly,omitempty"`
}

// HistoryContentChunk is one chunk of a ref-replaced field's full value.
type HistoryContentChunk struct {
	EntryID string `json:"entryId"`
	Field   string `json:"field"`
	Chunk   int    `json:"chunk"`
	Chunks  int    `json:"chunks"`
	Data    string `json:"data"`
	Done    bool   `json:"done"`
	Stale   bool   `json:"stale"`
}

// MarshalJSON enforces the Wails contract even for zero values: entries is
// always [], never null.
func (s HistorySlice) MarshalJSON() ([]byte, error) {
	type alias HistorySlice
	if s.Entries == nil {
		s.Entries = []HistoryEntry{}
	}
	return json.Marshal(alias(s))
}

// MarshalJSON keeps refs [] on zero values, matching the entries contract.
func (e HistoryEntry) MarshalJSON() ([]byte, error) {
	type alias HistoryEntry
	if e.Refs == nil {
		e.Refs = []HistoryContentRef{}
	}
	return json.Marshal(alias(e))
}

func emptyHistorySlice() HistorySlice { return HistorySlice{Entries: []HistoryEntry{}} }

func failedHistorySlice(message string) HistorySlice {
	return HistorySlice{Entries: []HistoryEntry{}, Error: strings.TrimSpace(message)}
}

func staleHistorySlice(revision int64, revisionKnown bool, digest string) HistorySlice {
	return HistorySlice{
		Entries:       []HistoryEntry{},
		Stale:         true,
		Revision:      revision,
		RevisionKnown: revisionKnown,
		Digest:        digest,
	}
}

func normalizeHistorySliceRequest(req HistorySliceRequest) HistorySliceRequest {
	if req.Turns <= 0 {
		req.Turns = defaultHistorySliceTurns
	}
	if req.Turns > maxHistorySliceTurns {
		req.Turns = maxHistorySliceTurns
	}
	if req.Entries <= 0 {
		req.Entries = defaultHistorySliceEntries
	}
	if req.Entries > maxHistorySliceEntries {
		req.Entries = maxHistorySliceEntries
	}
	if req.Bytes <= 0 {
		req.Bytes = defaultHistorySliceBytes
	}
	if req.Bytes > maxHistorySliceBytes {
		req.Bytes = maxHistorySliceBytes
	}
	return req
}

// historySliceCursor is the opaque page position toward older history.
type historySliceCursor struct {
	V        int    `json:"v"`
	Revision int64  `json:"revision"`
	RevKnown bool   `json:"revKnown"`
	Digest   string `json:"digest"`
	Before   int    `json:"before"` // next page covers messages/rows with index < Before
}

func encodeHistorySliceCursor(c historySliceCursor) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeHistorySliceCursor(s string) (historySliceCursor, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return historySliceCursor{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return historySliceCursor{}, err
	}
	var c historySliceCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return historySliceCursor{}, err
	}
	if c.V != 1 || c.Before < 0 {
		return historySliceCursor{}, fmt.Errorf("unsupported history cursor")
	}
	return c, nil
}

// historySliceSource is the windowed read view over one session used to cut a
// page: per-message visible turns and roles plus bounded message fetches.
type historySliceSource struct {
	sessionID  string // transcript basename minus .jsonl
	total      int    // total provider messages
	turns      []int  // turns[i] = visible turn of message i (1-based; 0 = before first turn)
	roles      []provider.Role
	totalTurns int
	revision   int64
	revKnown   bool
	digest     string
	epoch      int
	// cacheKey is non-empty only when revision+digest describe the complete
	// source (no unsaved live tail). Derived cross-page state may then be reused
	// without risking a stale completion against newly appended messages.
	cacheKey string
	// fetch returns messages [lo, hi). Implementations must copy or freshly
	// decode; callers never mutate but may retain across budget checks. Decode
	// errors are propagated all the way to the cold read instead of being
	// mistaken for an empty window and indexing past its end.
	fetch func(lo, hi int) ([]provider.Message, error)
	// windowBytes estimates the raw transcript span of [lo, hi); 0 means
	// unbounded-but-cheap (in-memory). Used to cap cold-path reads.
	windowBytes func(lo, hi int) int64
}

type historyDerivedCacheEntry struct {
	ready    chan struct{}
	todoArgs map[string]string
	err      error
}

// historyDerivedCache prevents every older-page request from replaying a huge
// transcript twice to derive the same todo state. Entries are identity-bound,
// single-flight, and deliberately few; transcript bodies are never retained.
type historyDerivedCache struct {
	mu      sync.Mutex
	entries map[string]*historyDerivedCacheEntry
	order   []string
}

func (c *historyDerivedCache) todoArgs(key string, compute func() (map[string]string, error)) (map[string]string, error) {
	if key == "" {
		return compute()
	}
	c.mu.Lock()
	if entry := c.entries[key]; entry != nil {
		c.touchLocked(key)
		ready := entry.ready
		c.mu.Unlock()
		<-ready
		return entry.todoArgs, entry.err
	}
	if c.entries == nil {
		c.entries = map[string]*historyDerivedCacheEntry{}
	}
	entry := &historyDerivedCacheEntry{ready: make(chan struct{})}
	c.entries[key] = entry
	c.order = append(c.order, key)
	c.pruneLocked()
	c.mu.Unlock()

	entry.todoArgs, entry.err = compute()
	close(entry.ready)
	c.mu.Lock()
	if entry.err != nil && c.entries[key] == entry {
		// Do not retain transient I/O or decode failures. A later page request
		// should be able to retry after the underlying read model is repaired.
		delete(c.entries, key)
		for i, candidate := range c.order {
			if candidate == key {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
	c.pruneLocked()
	c.mu.Unlock()
	return entry.todoArgs, entry.err
}

func (c *historyDerivedCache) touchLocked(key string) {
	for i, candidate := range c.order {
		if candidate == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, key)
}

func (c *historyDerivedCache) pruneLocked() {
	for len(c.entries) > historyDerivedCacheEntries {
		removed := false
		for i, key := range c.order {
			entry := c.entries[key]
			if entry == nil {
				c.order = append(c.order[:i], c.order[i+1:]...)
				removed = true
				break
			}
			select {
			case <-entry.ready:
				delete(c.entries, key)
				c.order = append(c.order[:i], c.order[i+1:]...)
				removed = true
			default:
			}
			if removed {
				break
			}
		}
		if !removed {
			return
		}
	}
}

// identityMatches reports whether the cursor/ref identity describes the same
// session state as the source. Revision is normalized to 0 when unknown on
// both sides, so the comparison is exact.
func (src *historySliceSource) identityMatches(revision int64, revKnown bool, digest string) bool {
	if !revKnown {
		revision = 0
	}
	return src.revKnown == revKnown && src.revision == revision && src.digest == digest
}

// historyWindowController is the slice of *control.Controller the windowed
// live path needs. Kept as an interface assertion (like
// sessionTempFromController) so test fakes implementing control.SessionAPI
// keep working via the full-snapshot fallback.

// HistorySliceForTab returns one page of the tab's history toward older
// messages, converting only the returned window.

// liveHistorySlice pages a tab with a running controller. The display index
// supplies turn boundaries when it validates against the session's persisted
// state; otherwise a single in-memory snapshot walk classifies turns (still
// converting only the window) and a background rebuild is kicked.

// Compat for fakes: full snapshot, windowed conversion.

// Fallback: one full snapshot for classification; conversion stays
// windowed. The background rebuild republishes the index when the
// in-memory log is exactly the persisted transcript.

// newInMemoryHistorySliceSource builds a source by classifying a full
// in-memory snapshot with the resolver-based visible-turn rule — the same
// semantics the legacy history path uses.

// coldHistorySlice pages a session file with no running controller. It never
// loads the whole session: a valid on-disk display index + byte-offset reads
// serve the window; a missing/stale/corrupt index is rebuilt by streaming
// scan (constant memory) and the first page is served from the scan result.

// Legacy event-record format: stream-decode (constant memory) and page
// the decoded rows. Only ancient sessions take this path.

// Legacy sessions have no ledger digest. Atomic index publication
// after the transcript plus exact structural/size validation is their
// generation stamp; a later rewrite advances the transcript mtime.

// A missing/corrupt sidecar is cheap to repair when the checkpoint itself is
// still the authoritative transcript. Scan once and compare its digest to
// the ledger before falling back to a full event-log replay. This preserves
// the bounded cold path for ordinary legacy/index-migration reads while
// still rejecting same-size anchor rewrites.

// The event log is authoritative. During append-only saves its transcript
// is newer than the compatibility .jsonl anchor, so scanning the anchor
// would silently omit the tail even when a display index covers it.

// Legacy checkpoints have no authoritative ledger identity. Scan their
// bytes to obtain the digest before trusting (or republishing) offsets; this
// detects same-size external rewrites that a size-only comparison misses.

// The bounded scanner rejects malformed or exceptionally large single
// records before allocating without limit. A legacy transcript still
// remains readable through the ordinary authoritative loader, then gets
// a file-exact index in the background for subsequent opens.

// historyIndexTimestampValid is the cheap file-generation guard for
// cold offset reads. Save/scan publish the index atomically after the transcript
// is complete. Equal timestamps are ambiguous on coarse filesystems, so cold
// readers verify the streamed digest before trusting offsets. The migration
// probe may accept equality because it never reads indexed content.

// readSessionMessagesAtOffsets decodes the message lines for entries, whose
// byte ranges are contiguous in the transcript, with one read.

// A legitimate historical record may exceed the normal 32MiB page span.
// Decode it directly from a bounded section so the read path does not first
// allocate and copy a second full record-sized byte slice.

// historySessionLooksEventFormat reports whether the transcript is a legacy
// event-record log rather than a provider-message transcript: event records
// carry kind/type and no role.

// Legacy event headers are tiny. A megabyte first record is a provider
// message or malformed input, neither of which needs event probing.

// pageHistorySliceSource cuts one page from src. Pages are suffixes of the
// candidate window: the turn budget picks the oldest message that may be
// included, conversion runs forward (its cross-message state flows forward),
// and the entry/byte budgets drop the oldest whole-message groups — so cuts
// always land on message boundaries.

// An undecodable cursor is treated like a request for the latest page.

// Turn budget: the oldest visible turn this page may reach.

// turns is non-decreasing: binary-search the first message in the page.

// A page reaching the first turn also includes the pre-turn messages
// (system prompt), mirroring providerMessagesForVisibleTurnRange.

// Cold-path raw-span cap: shrink the window forward while the byte span
// is excessive (image-dense windows).

// Keep the newest suffix within budget; always keep the newest group
// so a single oversized message still makes progress.

// countRoleBefore counts messages with role in [0, lo).

// extendHistoryToolResults fills in tool results for window tool calls whose
// result message lies past the window's newer edge (an intra-turn page cut),
// so tool-call summaries match the full-conversion output. It keeps no result
// body except one whose call is actually visible, but deliberately scans past
// arbitrary non-tool traffic: correctness cannot depend on a result arriving
// within a guessed distance.

// forEachHistorySourceChunk decodes a bounded contiguous message window at a
// time. It is the common primitive for the cross-page lookups below; callers
// retain only their derived state, never the full transcript.

// historyTodoArgsForSource derives completed todo state in two bounded passes:
// first discover successful calls anywhere in the transcript, then replay the
// todo stream. The legacy converter does the same work over an in-memory
// slice; doing it here prevents a page cut from displaying stale todo items.

// primeHistoryPlannerState consumes the non-rendered prefix so planner
// displays remain FIFO per duplicated user text and an interrupt's canonical
// suppression crosses page boundaries exactly as in a full conversion.

// newHistoryEntry builds one entry, replacing oversized string fields with
// preview + ref. entryID is the fully-built entry ID (message- or row-form).

// truncateHistoryField replaces value with a rune-safe preview and registers
// a content ref when it exceeds the inline threshold.

// inlineBytes approximates the JSON payload contributed by the entry's inline
// string fields (post-truncation), for the byte budget.

// historyWindowWithPersistedTimes is the window-scoped form of
// historyProviderMessagesWithPersistedTimes: userOffset is the number of
// user-role messages before the window, keeping the ordinal alignment with
// the persisted user-message records.

// historyContentChunkCount returns the number of rune-aligned ≤256KiB chunks
// for s. The empty string is one empty chunk.

// historyContentChunkEnd returns the end offset of the chunk starting at off:
// off+256KiB backed off to a rune boundary.

// historyContentChunkAt returns chunk index (0-based) of s and the total
// chunk count, splitting on rune boundaries.

// HistoryContentForTab returns one chunk of a ref-replaced field's full
// value. The entry is re-resolved through the same window machinery; when the
// session's revision/digest moved past the ref, Stale is set so the frontend
// reloads.

// The entry or field no longer resolves — content changed underneath.

// parseHistoryEntryID parses s<id>:r<epoch>:m<msgIndex>:o<sub> and the legacy
// event-format s<id>:r<epoch>:e<row>:o0 form.

// liveHistoryFieldValue re-resolves one entry's field from the live session.

// coldHistoryFieldValue re-resolves one entry's field through the same
// authoritative source selection as HistorySliceForTab. It never trusts a
// stale checkpoint merely because the requested message's old offset exists.

// historyEntryFieldValue reads one field of a converted row by ref field name.

// --- Legacy event-format paging -------------------------------------------

// coldEventHistorySlice pages a legacy event-record session. The decode
// streams the file once per request (constant memory); only ancient sessions
// take this path.

// pageHistoryEventRows cuts a page from already-converted rows (legacy event
// format). Row indexes play the role of message indexes; every row is its own
// group. Turns count user rows, 1-based.

// Visible turn per row.

// Suffix cut: walk backward from the newest row, keeping whole rows until
// a budget is reached; always keep the newest row so a single oversized
// row still makes progress.

// legacyHistoryFieldValue re-resolves a field of a legacy event-format row.

// --- Background index maintenance ------------------------------------------

// kickHistoryIndexRebuild single-flight schedules a background display-index
// rebuild for a live session whose on-disk index did not validate. It never
// blocks the request path.

// kickHistoryReadModelRepair single-flights the stronger cold-session repair:
// replay the authoritative event log under the save lock, atomically refresh
// the JSONL random-read model, then publish matching offsets. The cold request
// already returned from its in-memory recovery source before this work starts.

// rebuildHistoryIndexForLiveSession republishes the display index for a live
// session, but only when the in-memory log is exactly the persisted
// transcript — an append-only tail means the next save will publish a
// covering index anyway, and a scanned .jsonl anchor cannot describe the
// event-log tail.

// startHistoryIndexMigration arms the startup background worker that builds
// display indexes for session files that predate the sidecar. Like
// enableDeferredRebuildRetry it is only called from the Wails startup hook, so
// test-constructed Apps never spawn the worker.

// stopHistoryIndexMigration stops the startup migration worker; called from
// shutdown. The worker also stops with the Wails context.

// historyIndexMigrationLoop walks every known session dir once, building
// missing or stale display indexes. It is single-concurrency, yields between
// sessions, and is idempotent: a valid index (loadable + transcript size
// match) is left untouched.

// ListSessionOrder is the lightweight listing: it never decodes
// transcript content, which keeps this worker cheap on dirs full of
// legacy sessions.

// historySessionIndexOnDiskValid reports whether the on-disk display index
// loads and describes the current transcript file size. The size guard is the
// Phase A stale-anchor rule: append-only saves leave the .jsonl anchor behind
// the canonical transcript, and the reverse (a rewritten anchor with an old
// index) must not be sliced by stale offsets either.
