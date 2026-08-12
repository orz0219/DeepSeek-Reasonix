package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"reasonix/internal/provider"
	"reasonix/internal/store"
)

const (
	sessionEventSchemaVersion = 1
	sessionEventTypeReplace   = "replace"
	sessionEventTypeAppend    = "append"
	// sessionEventReplayMaxBytes caps decoder input before encoding/json can
	// allocate an arbitrarily large record. Session logs normally compact far
	// below this threshold; the generous ceiling still accommodates histories
	// with embedded images while keeping corrupt logs from exhausting the host.
	sessionEventReplayMaxBytes = int64(128 << 20)
	// A byte limit alone is insufficient: a compact JSON array can expand into
	// a much larger graph of messages and event records after decoding.
	sessionEventReplayMaxRecords         = 100_000
	sessionEventReplayMaxMessages        = 100_000
	sessionEventReplayMaxCollectionItems = 100_000
	sessionEventProbeMaxBytes            = int64(4 << 10)
	// sessionEventLogCompactFloor is the smallest log size that can trigger
	// event-log maintenance, so short sessions never pay a checkpoint rewrite.
	sessionEventLogCompactFloor = int64(256 << 10)
	// sessionEventLogCompactFactor bounds the log at this multiple of the live
	// transcript's encoded size; past it the log is rewritten to one replace
	// event so replace-heavy histories (rewind and recovery) cannot grow the
	// file without bound.
	sessionEventLogCompactFactor = int64(4)
)

// ErrSessionReplayLimitExceeded identifies a session that was left untouched
// because replaying it would exceed the process safety budget. Callers must not
// fall back to an older checkpoint: the event log may contain newer turns.
var ErrSessionReplayLimitExceeded = errors.New("session history exceeds safe replay limits")

// SessionReplayLimitError carries machine-readable diagnostics while keeping
// Error free of local paths for Desktop surfaces that display startup errors.
type SessionReplayLimitError struct {
	Path     string
	Resource string
	Value    int64
	Limit    int64
}

func (e *SessionReplayLimitError) Error() string {
	if e == nil {
		return ErrSessionReplayLimitExceeded.Error()
	}
	return fmt.Sprintf("%s: %s=%d, limit=%d; session files were left unchanged",
		ErrSessionReplayLimitExceeded, e.Resource, e.Value, e.Limit)
}

func (e *SessionReplayLimitError) Unwrap() error {
	return ErrSessionReplayLimitExceeded
}

type sessionReplayLimits struct {
	maxBytes           int64
	maxRecords         int
	maxMessages        int
	maxCollectionItems int
}

var defaultSessionReplayLimits = sessionReplayLimits{
	maxBytes:           sessionEventReplayMaxBytes,
	maxRecords:         sessionEventReplayMaxRecords,
	maxMessages:        sessionEventReplayMaxMessages,
	maxCollectionItems: sessionEventReplayMaxCollectionItems,
}

func sessionReplayLimitError(path, resource string, value, limit int64) error {
	err := &SessionReplayLimitError{Path: path, Resource: resource, Value: value, Limit: limit}
	slog.Warn("session: refusing unsafe event-log replay",
		"path", path, "resource", resource, "value", value, "limit", limit)
	return err
}

type sessionEventRecord struct {
	SchemaVersion int                `json:"schema_version"`
	Type          string             `json:"type"`
	Revision      int64              `json:"revision,omitempty"`
	BaseRevision  int64              `json:"base_revision,omitempty"`
	MessageIndex  int                `json:"message_index,omitempty"`
	Messages      []provider.Message `json:"messages,omitempty"`
	ContentDigest string             `json:"content_digest,omitempty"`
	WriterID      string             `json:"writer_id,omitempty"`
	Reason        string             `json:"reason,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
}

// sessionEventWireRecord keeps the messages array encoded until the replay
// budget has been checked. Decoding directly into sessionEventRecord would
// materialize every provider.Message before replay could enforce maxMessages.
type sessionEventWireRecord struct {
	SchemaVersion int             `json:"schema_version"`
	Type          string          `json:"type"`
	Revision      int64           `json:"revision,omitempty"`
	BaseRevision  int64           `json:"base_revision,omitempty"`
	MessageIndex  int             `json:"message_index,omitempty"`
	Messages      json.RawMessage `json:"messages,omitempty"`
	ContentDigest string          `json:"content_digest,omitempty"`
	WriterID      string          `json:"writer_id,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

type sessionEventIndex struct {
	SchemaVersion int       `json:"schema_version"`
	LogSize       int64     `json:"log_size"`
	MessageCount  int       `json:"message_count"`
	Revision      int64     `json:"revision"`
	ContentDigest string    `json:"content_digest"`
	WriterID      string    `json:"writer_id"`
	UpdatedAt     time.Time `json:"updated_at"`
}

func SessionEventLogPath(sessionPath string) string {
	return store.SessionEventLog(sessionPath)
}

func SessionEventIndexPath(sessionPath string) string {
	return store.SessionEventIndex(sessionPath)
}

func sessionEventLogSize(sessionPath string) int64 {
	path := store.SessionEventLog(sessionPath)
	if path == "" {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return 0
	}
	return info.Size()
}

func sessionEventLogOversized(logSize, contentBytes int64) bool {
	limit := sessionEventLogCompactFloor
	if scaled := contentBytes * sessionEventLogCompactFactor; scaled > limit {
		limit = scaled
	}
	return logSize > limit
}

// sessionEventReplay is the result of a tolerant event-log replay: the
// transcript up to the last cleanly applied record, plus enough bookkeeping
// for writers to self-heal a torn tail.

// collectionItems counts the elements in every JSON array nested below a
// live message. Keeping this alongside msgs bounds slices such as tool calls,
// images, memory citations, and interrupted-turn recovery metadata without
// coupling replay safety to today's provider.Message field list.

// times mirrors msgs with each message's record CreatedAt. Replace events
// collapse per-turn history, so their messages get the zero time and
// callers fall back to coarser timestamps.

// records counts cleanly applied events.

// lastGoodEnd is the byte offset just past the last cleanly applied
// record; truncating the log here drops only undecodable bytes.

// size is the log size that was replayed.

// damaged is set when replay stopped early on a torn/corrupt record or a
// broken append chain. The prefix in msgs is still a valid historical
// state.

// sessionEventLogProbe classifies whatever sits at the session's event-log
// path. Legacy imports can leave a foreign ".events.jsonl" (e.g. the v0.x
// Claude-style event transcript) at exactly the native log path; writing into
// or over it would corrupt the user's original file, so foreign logs are
// read-ignored and never touched.
type sessionEventLogProbe struct {
	size          int64
	native        bool // missing/empty, or first record is a supported native event
	futureSchema  bool // first record declares a newer schema than this build
	schemaVersion int
}

// sessionEventSidecarsFit reports whether the event log and index filenames
// stay within the filesystem's name limit. Overlong transcript names (from the
// pre-bounded recovery cascade, until reconcileOverlongSessionFilenames renames
// them) must run checkpoint-only: creating their sidecars would fail with
// ENAMETOOLONG mid-save.
func sessionEventSidecarsFit(sessionPath string) bool {
	logName := filepath.Base(store.SessionEventLog(sessionPath))
	indexName := filepath.Base(store.SessionEventIndex(sessionPath))
	return len(logName) <= nameMaxBytes && len(indexName) <= nameMaxBytes
}

// probeSessionEventLog inspects the first record of the event log to decide
// whether the native persistence layer owns the file. Missing or empty logs
// count as native (we may create/append); an undecodable or foreign first
// record — or a transcript name too long for the sidecars to fit — marks the
// file as not ours.
func probeSessionEventLog(sessionPath string) (sessionEventLogProbe, error) {
	return probeSessionEventLogWithLimits(sessionPath, defaultSessionReplayLimits)
}

func probeSessionEventLogWithLimits(sessionPath string, limits sessionReplayLimits) (sessionEventLogProbe, error) {
	path := store.SessionEventLog(sessionPath)
	if path == "" {
		return sessionEventLogProbe{native: true}, nil
	}
	if !sessionEventSidecarsFit(sessionPath) {
		return sessionEventLogProbe{}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sessionEventLogProbe{native: true}, nil
		}
		return sessionEventLogProbe{}, err
	}
	if info.IsDir() {
		return sessionEventLogProbe{}, nil
	}
	if info.Size() == 0 {
		return sessionEventLogProbe{native: true}, nil
	}
	probe := sessionEventLogProbe{size: info.Size()}
	f, err := os.Open(path)
	if err != nil {
		return sessionEventLogProbe{}, err
	}
	defer f.Close()
	var schemaVersion int
	var eventType string
	var ok bool
	schemaVersion, eventType, ok = probeSessionEventHeader(f)
	if !ok && info.Size() <= limits.maxBytes {
		// Native writers put both identifying fields in the bounded prefix. For
		// other valid in-budget JSON, fall back to a minimal struct decode so
		// field order remains a compatibility property rather than a format
		// requirement. Unknown fields are not materialized into messages.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return sessionEventLogProbe{}, err
		}
		var header struct {
			SchemaVersion int    `json:"schema_version"`
			Type          string `json:"type"`
		}
		dec := json.NewDecoder(&io.LimitedReader{R: f, N: limits.maxBytes + 1})
		if err := dec.Decode(&header); err == nil {
			schemaVersion, eventType, ok = header.SchemaVersion, header.Type, true
		}
	}
	if !ok {
		// Nothing decodable at the head: not a native log this build can own.
		return probe, nil
	}
	probe.schemaVersion = schemaVersion
	switch {
	case schemaVersion == sessionEventSchemaVersion &&
		(eventType == sessionEventTypeReplace || eventType == sessionEventTypeAppend):
		probe.native = true
	case schemaVersion > sessionEventSchemaVersion:
		// A newer writer owns this log; ignoring or truncating it would
		// silently discard that writer's transcript.
		probe.futureSchema = true
	}
	return probe, nil
}

// probeSessionEventHeader searches a bounded prefix for the identifying fields.
// Using Decode on a partial struct still buffers the whole JSON value, so native
// writer output must take this fast path before replay's byte budget is checked.
func probeSessionEventHeader(r io.Reader) (schemaVersion int, eventType string, ok bool) {
	dec := json.NewDecoder(io.LimitReader(r, sessionEventProbeMaxBytes))
	tok, err := dec.Token()
	if err != nil {
		return 0, "", false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '{' {
		return 0, "", false
	}
	var haveSchema, haveType bool
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, "", false
		}
		name, isString := key.(string)
		if !isString {
			return 0, "", false
		}
		switch name {
		case "schema_version":
			if err := dec.Decode(&schemaVersion); err != nil {
				return 0, "", false
			}
			haveSchema = true
		case "type":
			if err := dec.Decode(&eventType); err != nil {
				return 0, "", false
			}
			haveType = true
		default:
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				return 0, "", false
			}
		}
		if haveSchema && haveType {
			return schemaVersion, eventType, true
		}
	}
	return 0, "", false
}

// replaySessionEventLog decodes an event log tolerantly: decoding stops at the
// first record that fails to parse or chain, and the state up to that point is
// returned with damaged=true so writers can self-heal. Unsupported schema
// versions and unknown event types stay hard errors — they mean a newer writer
// owns this log, and truncating it would discard that writer's data.

// Stat and read are not atomic across processes. LimitReader keeps a log
// that grows after Stat inside the same byte budget.

// decodeSessionEventMessages preflights both the top-level message count and
// every nested JSON collection before constructing provider.Message values.
// The token walk is independent of today's provider.Message fields, so future
// slice fields inherit the same aggregate object-graph bound automatically.

// preflightSessionEventValue walks one JSON value without materializing maps or
// slices. Each array element is charged before its value is read, so an invalid
// over-limit element cannot allocate a typed provider collection first.

// loadSessionMessages returns the session transcript, preferring the event log
// when the native layer owns it and it holds at least one decodable record.
// Foreign files squatting the log path (legacy import leftovers) are ignored
// in favor of the .jsonl checkpoint. damaged reports that a native log could
// not be replayed to its end (torn tail or corrupt record); callers that write
// should rewrite-and-compact to heal it.

// Defensive: the probe saw a native head but nothing replayed; fall
// back to the checkpoint and let the next save rebuild the log.

// repairSessionEventLogTail truncates undecodable bytes left by a crash or
// disk-full append so the next append cannot bury them mid-log where replay
// would stop forever. Callers must hold the session file lock. The event
// index's LogSize doubles as a cheap intact check so the common case never
// re-reads the log.

// Salvage the bytes the truncation below discards. A torn tail is usually
// one partial record, but replay also stops at a buried undecodable or
// out-of-order record (e.g. two runtimes interleaving appends on one log) —
// then everything past it, including intact turns, would be silently and
// permanently lost (#6607). Preservation is best-effort: it must not block
// the repair (the log has to become appendable again either way), and its
// most likely failure — a full disk — is the same condition that tears
// tails in the first place.

// The truncation point sits exactly at the end of a JSON value; restore
// the trailing newline so the file stays line-oriented for external tools.

// preserveDamagedEventLogTail appends the about-to-be-truncated byte range of
// the event log to the .damaged salvage sidecar, prefixed with a one-line JSON
// header recording when and where the bytes came from. The sidecar is a
// forensic artifact for recovery, never replayed by the loader, and is removed
// with the session's other sidecars on delete.

// The event log carries the complete transcript. Chmod after opening so
// upgrading a pre-v0.53-boundary 0644 sidecar tightens the existing inode
// before any unredacted message is appended; OpenFile's perm only applies
// when the file is newly created.

// Replace events carry the whole transcript and mark intentional history
// rewrites; they are rare and fsynced so a power cut cannot lose one.

// compactSessionEventLog rewrites the log as a single replace event via an
// atomic tmp+fsync+rename, so readers observe either the old log or the
// compacted one and never a partial state. It also heals a damaged log by
// construction.

// No log means nothing for the index to describe; drop a stale
// index left by migration or manual sidecar cleanup.
