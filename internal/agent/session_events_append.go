package agent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/provider"
	"reasonix/internal/store"
)

func appendSessionEvent(sessionPath string, rec sessionEventRecord, sync bool) error {
	path := store.SessionEventLog(sessionPath)
	if path == "" {
		return fmt.Errorf("empty session event log path")
	}
	fileutil.Crash("wal-append", path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	rec.SchemaVersion = sessionEventSchemaVersion
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	if rec.WriterID == "" {
		rec.WriterID = SessionWriterID()
	}
	buf, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode session event: %w", err)
	}
	buf = append(buf, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open session event log: %w", err)
	}

	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("protect session event log: %w", err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		return fmt.Errorf("append session event: %w", err)
	}
	if sync {
		if err := f.Sync(); err != nil {
			_ = f.Close()
			return err
		}
	}
	return f.Close()
}

func appendSessionReplaceEvent(sessionPath string, msgs []provider.Message, digest [sha256.Size]byte, baseRevision int64, reason string) error {

	return appendSessionEvent(sessionPath, sessionEventRecord{
		Type:          sessionEventTypeReplace,
		Revision:      baseRevision + 1,
		BaseRevision:  baseRevision,
		MessageIndex:  0,
		Messages:      append([]provider.Message(nil), msgs...),
		ContentDigest: digestString(digest),
		Reason:        reason,
	}, true)
}

func appendSessionAppendEvent(sessionPath string, messageIndex int, msgs []provider.Message, digest [sha256.Size]byte, baseRevision int64) error {
	if len(msgs) == 0 {
		return nil
	}
	return appendSessionEvent(sessionPath, sessionEventRecord{
		Type:          sessionEventTypeAppend,
		Revision:      baseRevision + 1,
		BaseRevision:  baseRevision,
		MessageIndex:  messageIndex,
		Messages:      append([]provider.Message(nil), msgs...),
		ContentDigest: digestString(digest),
	}, true)
}

// compactSessionEventLog rewrites the log as a single replace event via an
// atomic tmp+fsync+rename, so readers observe either the old log or the
// compacted one and never a partial state. It also heals a damaged log by
// construction.
func compactSessionEventLog(sessionPath string, msgs []provider.Message, digest [sha256.Size]byte, baseRevision int64, reason string) error {
	path := store.SessionEventLog(sessionPath)
	if path == "" {
		return fmt.Errorf("empty session event log path")
	}
	rec := sessionEventRecord{
		SchemaVersion: sessionEventSchemaVersion,
		Type:          sessionEventTypeReplace,
		Revision:      baseRevision + 1,
		BaseRevision:  baseRevision,
		Messages:      append([]provider.Message(nil), msgs...),
		ContentDigest: digestString(digest),
		WriterID:      SessionWriterID(),
		Reason:        reason,
		CreatedAt:     time.Now().UTC(),
	}
	buf, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode session event: %w", err)
	}
	buf = append(buf, '\n')
	return fileutil.AtomicWriteFile(path, buf, 0o600)
}

func readSessionEventIndex(sessionPath string) (*sessionEventIndex, error) {
	path := store.SessionEventIndex(sessionPath)
	if path == "" {
		return nil, nil
	}
	b, err := fileencoding.ReadFileUTF8(path)
	if err != nil {
		return nil, err
	}
	var idx sessionEventIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, err
	}
	if idx.SchemaVersion != sessionEventSchemaVersion {
		return nil, fmt.Errorf("unsupported session event index schema %d", idx.SchemaVersion)
	}
	return &idx, nil
}

func writeSessionEventIndex(path string, msgs []provider.Message, digest [sha256.Size]byte, revision int64) error {
	indexPath := store.SessionEventIndex(path)
	if indexPath == "" {
		return nil
	}
	logInfo, err := os.Stat(store.SessionEventLog(path))
	if err != nil {
		if os.IsNotExist(err) {

			if err := os.Remove(indexPath); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		return err
	}
	idx := sessionEventIndex{
		SchemaVersion: sessionEventSchemaVersion,
		LogSize:       logInfo.Size(),
		MessageCount:  len(msgs),
		Revision:      revision,
		ContentDigest: digestString(digest),
		WriterID:      SessionWriterID(),
		UpdatedAt:     time.Now().UTC(),
	}
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(indexPath), ".session-event-index.*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := fileutil.ReplaceFile(tmpPath, indexPath); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
