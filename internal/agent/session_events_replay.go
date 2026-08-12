package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"reasonix/internal/provider"
	"reasonix/internal/store"
)

// sessionEventReplay is the result of a tolerant event-log replay: the
// transcript up to the last cleanly applied record, plus enough bookkeeping
// for writers to self-heal a torn tail.
type sessionEventReplay struct {
	msgs []provider.Message
	// collectionItems counts the elements in every JSON array nested below a
	// live message. Keeping this alongside msgs bounds slices such as tool calls,
	// images, memory citations, and interrupted-turn recovery metadata without
	// coupling replay safety to today's provider.Message field list.
	collectionItems int
	// times mirrors msgs with each message's record CreatedAt. Replace events
	// collapse per-turn history, so their messages get the zero time and
	// callers fall back to coarser timestamps.
	times []time.Time
	// records counts cleanly applied events.
	records int
	// lastGoodEnd is the byte offset just past the last cleanly applied
	// record; truncating the log here drops only undecodable bytes.
	lastGoodEnd int64
	// size is the log size that was replayed.
	size int64
	// damaged is set when replay stopped early on a torn/corrupt record or a
	// broken append chain. The prefix in msgs is still a valid historical
	// state.
	damaged bool
}

// replaySessionEventLog decodes an event log tolerantly: decoding stops at the
// first record that fails to parse or chain, and the state up to that point is
// returned with damaged=true so writers can self-heal. Unsupported schema
// versions and unknown event types stay hard errors — they mean a newer writer
// owns this log, and truncating it would discard that writer's data.
func replaySessionEventLog(path string) (sessionEventReplay, error) {
	return replaySessionEventLogWithLimits(path, defaultSessionReplayLimits)
}

func replaySessionEventLogWithLimits(path string, limits sessionReplayLimits) (sessionEventReplay, error) {
	f, err := os.Open(path)
	if err != nil {
		return sessionEventReplay{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return sessionEventReplay{}, err
	}
	replay := sessionEventReplay{size: info.Size()}
	if info.Size() > limits.maxBytes {
		return replay, sessionReplayLimitError(path, "encoded_bytes", info.Size(), limits.maxBytes)
	}

	limited := &io.LimitedReader{R: f, N: limits.maxBytes + 1}
	dec := json.NewDecoder(limited)
	for {
		var rec sessionEventWireRecord
		if err := dec.Decode(&rec); err != nil {
			if limited.N == 0 {
				return replay, sessionReplayLimitError(path, "encoded_bytes", limits.maxBytes+1, limits.maxBytes)
			}
			if errors.Is(err, io.EOF) {
				return replay, nil
			}
			replay.damaged = true
			return replay, nil
		}
		if rec.SchemaVersion != sessionEventSchemaVersion {
			return replay, fmt.Errorf("decode session event log %s: unsupported schema version %d", path, rec.SchemaVersion)
		}
		if replay.records >= limits.maxRecords {
			return replay, sessionReplayLimitError(path, "event_records", int64(replay.records+1), int64(limits.maxRecords))
		}
		switch rec.Type {
		case sessionEventTypeReplace:
			msgs, collectionItems, err := decodeSessionEventMessages(path, rec.Messages, 0, 0, limits)
			if err != nil {
				if errors.Is(err, ErrSessionReplayLimitExceeded) {
					return replay, err
				}
				replay.damaged = true
				return replay, nil
			}
			replay.msgs = msgs
			replay.collectionItems = collectionItems
			replay.times = make([]time.Time, len(replay.msgs))
		case sessionEventTypeAppend:
			if rec.MessageIndex != len(replay.msgs) {
				replay.damaged = true
				return replay, nil
			}
			msgs, collectionItems, err := decodeSessionEventMessages(
				path, rec.Messages, len(replay.msgs), replay.collectionItems, limits,
			)
			if err != nil {
				if errors.Is(err, ErrSessionReplayLimitExceeded) {
					return replay, err
				}
				replay.damaged = true
				return replay, nil
			}
			replay.msgs = append(replay.msgs, msgs...)
			replay.collectionItems = collectionItems
			for range msgs {
				replay.times = append(replay.times, rec.CreatedAt)
			}
		default:
			return replay, fmt.Errorf("decode session event log %s: unsupported event type %q", path, rec.Type)
		}
		replay.records++
		replay.lastGoodEnd = dec.InputOffset()
	}
}

// decodeSessionEventMessages preflights both the top-level message count and
// every nested JSON collection before constructing provider.Message values.
// The token walk is independent of today's provider.Message fields, so future
// slice fields inherit the same aggregate object-graph bound automatically.
func decodeSessionEventMessages(
	path string,
	raw json.RawMessage,
	existingMessages, existingCollectionItems int,
	limits sessionReplayLimits,
) ([]provider.Message, int, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, existingCollectionItems, nil
	}
	messageCount, collectionItems, err := preflightSessionEventMessages(
		path, trimmed, existingMessages, existingCollectionItems, limits,
	)
	if err != nil {
		return nil, existingCollectionItems, err
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil {
		return nil, existingCollectionItems, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, existingCollectionItems, fmt.Errorf("messages must be an array")
	}
	msgs := make([]provider.Message, 0, messageCount)
	for dec.More() {
		var msg provider.Message
		if err := dec.Decode(&msg); err != nil {
			return nil, existingCollectionItems, err
		}
		msgs = append(msgs, msg)
	}
	if _, err := dec.Token(); err != nil {
		return nil, existingCollectionItems, err
	}
	return msgs, collectionItems, nil
}

func preflightSessionEventMessages(
	path string,
	raw []byte,
	existingMessages, existingCollectionItems int,
	limits sessionReplayLimits,
) (messageCount, collectionItems int, err error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return 0, existingCollectionItems, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return 0, existingCollectionItems, fmt.Errorf("messages must be an array")
	}
	collectionItems = existingCollectionItems
	for dec.More() {
		if existingMessages+messageCount >= limits.maxMessages {
			return 0, existingCollectionItems, sessionReplayLimitError(
				path, "messages", int64(existingMessages+messageCount+1), int64(limits.maxMessages),
			)
		}
		messageCount++
		if err := preflightSessionEventValue(path, dec, &collectionItems, limits.maxCollectionItems); err != nil {
			return 0, existingCollectionItems, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return 0, existingCollectionItems, err
	}
	return messageCount, collectionItems, nil
}

// preflightSessionEventValue walks one JSON value without materializing maps or
// slices. Each array element is charged before its value is read, so an invalid
// over-limit element cannot allocate a typed provider collection first.
func preflightSessionEventValue(path string, dec *json.Decoder, collectionItems *int, maxCollectionItems int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			if _, ok := key.(string); !ok {
				return fmt.Errorf("object key must be a string")
			}
			if err := preflightSessionEventValue(path, dec, collectionItems, maxCollectionItems); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("object is not terminated")
		}
		return nil
	case '[':
		for dec.More() {
			if *collectionItems >= maxCollectionItems {
				return sessionReplayLimitError(
					path,
					"message_collection_items",
					int64(*collectionItems+1),
					int64(maxCollectionItems),
				)
			}
			(*collectionItems)++
			if err := preflightSessionEventValue(path, dec, collectionItems, maxCollectionItems); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("array is not terminated")
		}
		return nil
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}

// loadSessionMessages returns the session transcript, preferring the event log
// when the native layer owns it and it holds at least one decodable record.
// Foreign files squatting the log path (legacy import leftovers) are ignored
// in favor of the .jsonl checkpoint. damaged reports that a native log could
// not be replayed to its end (torn tail or corrupt record); callers that write
// should rewrite-and-compact to heal it.
func loadSessionMessages(sessionPath string) (msgs []provider.Message, fromEvents, damaged bool, err error) {
	return loadSessionMessagesWithLimits(sessionPath, defaultSessionReplayLimits)
}

func loadSessionMessagesWithLimits(sessionPath string, limits sessionReplayLimits) (msgs []provider.Message, fromEvents, damaged bool, err error) {
	probe, err := probeSessionEventLogWithLimits(sessionPath, limits)
	if err != nil {
		return nil, false, false, err
	}
	if probe.futureSchema {
		return nil, true, false, fmt.Errorf("session event log for %s uses schema %d; this build supports up to %d", sessionPath, probe.schemaVersion, sessionEventSchemaVersion)
	}
	if probe.native && probe.size > 0 {
		replay, replayErr := replaySessionEventLogWithLimits(store.SessionEventLog(sessionPath), limits)
		if replayErr != nil {
			return nil, true, false, replayErr
		}
		if replay.records > 0 {
			return replay.msgs, true, replay.damaged, nil
		}

		msgs, err = loadSessionMessagesFromJSONL(sessionPath)
		return msgs, false, true, err
	}
	msgs, err = loadSessionMessagesFromJSONL(sessionPath)
	return msgs, false, false, err
}

func loadSessionMessagesFromJSONL(path string) ([]provider.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var msgs []provider.Message
	dec := json.NewDecoder(f)
	for {
		var m provider.Message
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// repairSessionEventLogTail truncates undecodable bytes left by a crash or
// disk-full append so the next append cannot bury them mid-log where replay
// would stop forever. Callers must hold the session file lock. The event
// index's LogSize doubles as a cheap intact check so the common case never
// re-reads the log.
func repairSessionEventLogTail(sessionPath string) error {
	path := store.SessionEventLog(sessionPath)
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() || info.Size() == 0 {
		return nil
	}
	if idx, err := readSessionEventIndex(sessionPath); err == nil && idx != nil && idx.LogSize == info.Size() {
		return nil
	}
	replay, err := replaySessionEventLog(path)
	if err != nil {
		return err
	}
	if replay.lastGoodEnd >= replay.size {
		return nil
	}

	if preserveErr := preserveDamagedEventLogTail(sessionPath, path, replay.lastGoodEnd, replay.size); preserveErr != nil {
		slog.Warn("session: could not preserve damaged event log tail; truncating anyway",
			"path", path, "from", replay.lastGoodEnd, "size", replay.size, "err", preserveErr)
	}
	if err := os.Truncate(path, replay.lastGoodEnd); err != nil {
		return err
	}
	if replay.lastGoodEnd == 0 {
		return nil
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// preserveDamagedEventLogTail appends the about-to-be-truncated byte range of
// the event log to the .damaged salvage sidecar, prefixed with a one-line JSON
// header recording when and where the bytes came from. The sidecar is a
// forensic artifact for recovery, never replayed by the loader, and is removed
// with the session's other sidecars on delete.
func preserveDamagedEventLogTail(sessionPath, logPath string, from, to int64) error {
	if to <= from {
		return nil
	}
	src, err := os.Open(logPath)
	if err != nil {
		return err
	}
	defer src.Close()
	if _, err := src.Seek(from, io.SeekStart); err != nil {
		return err
	}
	dst, err := os.OpenFile(store.SessionEventLogDamaged(sessionPath), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("{\"damaged_tail\":true,\"preserved_at\":%q,\"log_offset\":%d,\"bytes\":%d}\n",
		time.Now().UTC().Format(time.RFC3339), from, to-from)
	if _, err := dst.WriteString(header); err != nil {
		dst.Close()
		return err
	}
	if _, err := io.CopyN(dst, src, to-from); err != nil && !errors.Is(err, io.EOF) {
		dst.Close()
		return err
	}
	if _, err := dst.WriteString("\n"); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}
