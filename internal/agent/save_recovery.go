package agent

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"

	"reasonix/internal/provider"
)

type SessionSnapshotConflictKind string

const (
	SessionSnapshotConflictStalePrefix SessionSnapshotConflictKind = "stale_prefix"
	SessionSnapshotConflictDiverged    SessionSnapshotConflictKind = "diverged"
)

// checkSnapshotWrite decides whether this session may write msgs over path, and
// whether the safe write shape is a no-op, append-only suffix, or full rewrite.
func (s *Session) checkSnapshotWrite(path string, next []provider.Message, nextDigest [sha256.Size]byte, nextVersion uint64, allowOwnedRewrite bool) (snapshotWriteDecision, error) {
	baseState := s.persistState(path)
	current, err := loadSessionUnlocked(path)
	if err != nil {
		if os.IsNotExist(err) {
			if baseState.ok {
				return snapshotWriteDecision{}, ErrSessionExternallyRemoved
			}
			return snapshotWriteDecision{}, nil
		}
		return snapshotWriteDecision{}, err
	}
	currentRevision, currentLedgerDigest, err := sessionContentRevision(path)
	if err != nil {
		return snapshotWriteDecision{}, err
	}
	existing := current.Snapshot()
	existingDigest, err := digestSessionMessages(existing)
	if err != nil {
		return snapshotWriteDecision{}, err
	}

	raw, rawDigest := existing, existingDigest
	rawDiffers := current.normalizedDirty && len(current.rawMessages) > 0
	if rawDiffers {
		raw = current.rawMessages
		if rawDigest, err = digestSessionMessages(raw); err != nil {
			return snapshotWriteDecision{}, err
		}
	}
	contentUnchanged := bytes.Equal(existingDigest[:], nextDigest[:])
	exactAppend := messagesHavePrefix(next, existing)
	appendShaped := contentUnchanged || exactAppend || messagesHavePrefixWithCompatibleSystem(next, existing)
	repairPending := current.normalizedDirty
	if !appendShaped && rawDiffers {
		rawUnchanged := bytes.Equal(rawDigest[:], nextDigest[:])
		rawAppend := messagesHavePrefix(next, raw)
		if rawUnchanged || rawAppend || messagesHavePrefixWithCompatibleSystem(next, raw) {
			existing = raw
			contentUnchanged = rawUnchanged
			exactAppend = rawAppend
			appendShaped = true

			repairPending = false
		}
	}
	if !appendShaped && baseState.ok && baseState.revisionKnown &&
		baseState.revision == currentRevision && !contentUnchanged {

		if s.ownsWritableBaseline(path, existingDigest, rawDigest, rawDiffers, currentRevision, currentLedgerDigest, nextVersion) {
			appendShaped = true
		}
	}
	if appendShaped {

		if baseState.ok && baseState.revisionKnown && currentRevision != baseState.revision && !contentUnchanged &&
			!appendCoversPersistedBaseline(next, existing, baseState.digest) {
			return snapshotWriteDecision{}, snapshotConflict(path, existing, next, baseState.revision, currentRevision)
		}

		decision := snapshotWriteDecision{
			revision:  currentRevision,
			upToDate:  contentUnchanged && !repairPending && !current.eventLogDamaged,
			repairLog: current.eventLogDamaged,
		}

		if decision.upToDate && currentLedgerDigest != "" && currentLedgerDigest != digestString(nextDigest) {
			decision.ledgerStale = true
		}

		if exactAppend && !contentUnchanged && len(existing) < len(next) && !current.eventLogDamaged && !repairPending {
			decision.appendOnly = true
			decision.appendFrom = len(existing)
		}
		return decision, nil
	}
	if allowOwnedRewrite {
		if s.ownsWritableBaseline(path, existingDigest, rawDigest, rawDiffers, currentRevision, currentLedgerDigest, nextVersion) {
			return snapshotWriteDecision{revision: currentRevision, repairLog: current.eventLogDamaged}, nil
		}
	}

	if err := s.authorityErrorForPath(path); err != nil {
		return snapshotWriteDecision{}, err
	}
	if messagesHavePrefix(existing, next) || messagesHavePrefixWithCompatibleSystem(existing, next) ||
		(rawDiffers && (messagesHavePrefix(raw, next) || messagesHavePrefixWithCompatibleSystem(raw, next))) {
		return snapshotWriteDecision{}, &SessionSnapshotConflictError{
			Path:             path,
			Kind:             SessionSnapshotConflictStalePrefix,
			ExistingMessages: len(existing),
			SnapshotMessages: len(next),
			BaseRevision:     baseState.revision,
			DiskRevision:     currentRevision,
		}
	}
	return snapshotWriteDecision{}, &SessionSnapshotConflictError{
		Path:             path,
		Kind:             SessionSnapshotConflictDiverged,
		ExistingMessages: len(existing),
		SnapshotMessages: len(next),
		BaseRevision:     baseState.revision,
		DiskRevision:     currentRevision,
	}
}

func snapshotConflict(path string, existing, next []provider.Message, baseRevision, diskRevision int64) error {
	kind := SessionSnapshotConflictDiverged
	if messagesHavePrefix(existing, next) || messagesHavePrefixWithCompatibleSystem(existing, next) {
		kind = SessionSnapshotConflictStalePrefix
	}
	return &SessionSnapshotConflictError{
		Path:             path,
		Kind:             kind,
		ExistingMessages: len(existing),
		SnapshotMessages: len(next),
		BaseRevision:     baseRevision,
		DiskRevision:     diskRevision,
	}
}

func (s *Session) SaveRecoveryBranch(opts RecoveryBranchOptions) (RecoveryBranchInfo, error) {
	return s.saveRecoveryBranch(opts, false)
}

// SaveShutdownRecoveryBranch persists the current transcript to a distinct
// recovery branch after the normal shutdown snapshot failed with
// ErrSessionFileLockHeld. It deliberately does not re-lock or inspect the
// original session file: doing so would repeat the same bounded timeout and
// let process teardown discard the only remaining in-memory copy.
//
// The recovery filename includes this live Session's isolated lane, so another
// controller in the same process cannot replace the emergency copy.
// The result still uses the normal session, event-log, and branch-meta formats
// and is therefore discoverable and resumable through existing flows.
func (s *Session) SaveShutdownRecoveryBranch(opts RecoveryBranchOptions) (RecoveryBranchInfo, error) {
	return s.saveRecoveryBranch(opts, true)
}

// SaveConflictRecoveryBranch writes the depth-cap isolated copy (one path per
// live Session). Subsequent conflicts from that Session rewrite it in place.
func (s *Session) SaveConflictRecoveryBranch(opts RecoveryBranchOptions) (RecoveryBranchInfo, error) {
	return s.saveRecoveryBranch(opts, true)
}

func (s *Session) saveRecoveryBranch(opts RecoveryBranchOptions, shutdown bool) (RecoveryBranchInfo, error) {
	originalPath := strings.TrimSpace(opts.OriginalPath)
	if originalPath == "" {
		return RecoveryBranchInfo{}, fmt.Errorf("empty original session path")
	}
	opts.OriginalPath = originalPath
	msgs, version, rewriteVersion := s.snapshotWithVersion()
	preview, turns := SessionPreviewFromMessages(msgs)
	if turns == 0 {
		return RecoveryBranchInfo{}, ErrSessionRecoveryNotNeeded
	}
	digest, err := digestSessionMessages(msgs)
	if err != nil {
		return RecoveryBranchInfo{}, err
	}
	digestText := digestString(digest)

	if !shutdown {
		unlockOriginal := lockSessionSavePath(originalPath)
		unlockOriginalFile, lockErr := lockSessionFile(originalPath)
		if lockErr != nil {
			unlockOriginal()
			return RecoveryBranchInfo{}, fmt.Errorf("lock original session file: %w", lockErr)
		}
		current, loadErr := loadSessionUnlocked(originalPath)
		unlockOriginalFile()
		unlockOriginal()
		if loadErr != nil && !os.IsNotExist(loadErr) {
			return RecoveryBranchInfo{}, loadErr
		}
		if loadErr == nil && current != nil {
			existing := current.Snapshot()
			existingDigest, digestErr := digestSessionMessages(existing)
			if digestErr != nil {
				return RecoveryBranchInfo{}, digestErr
			}
			covered := bytes.Equal(existingDigest[:], digest[:]) ||
				messagesHavePrefix(existing, msgs) ||
				messagesHavePrefixWithCompatibleSystem(existing, msgs)
			if !covered && current.normalizedDirty && len(current.rawMessages) > 0 {

				raw := current.rawMessages
				rawDigest, rawErr := digestSessionMessages(raw)
				if rawErr != nil {
					return RecoveryBranchInfo{}, rawErr
				}
				covered = bytes.Equal(rawDigest[:], digest[:]) ||
					messagesHavePrefix(raw, msgs) ||
					messagesHavePrefixWithCompatibleSystem(raw, msgs)
			}
			if covered {
				return RecoveryBranchInfo{}, ErrSessionRecoveryNotNeeded
			}
		}
	}

	parentDepth := 0
	if parentMeta, ok, metaErr := LoadBranchMeta(originalPath); metaErr == nil && ok && parentMeta.Recovered {
		parentDepth = parentMeta.RecoveryDepth
		if parentDepth <= 0 {

			parentDepth = 1
		}
	}
	if parentDepth >= SessionRecoveryMaxDepth && !shutdown {
		return RecoveryBranchInfo{}, fmt.Errorf("%w: %s is already %d recovery forks deep",
			ErrSessionRecoveryDepthExceeded, originalPath, parentDepth)
	}
	recoveryDepth := min(parentDepth+1,

		SessionRecoveryMaxDepth)

	for range 8 {
		recoveryPath, lane := s.isolatedRecoverySessionPath(originalPath)
		info, collision, err := s.writeRecoveryBranchAtPath(recoveryPath, opts, msgs, digest,
			version, rewriteVersion, preview, turns, digestText, recoveryDepth, shutdown)
		if err != nil {
			return RecoveryBranchInfo{}, err
		}
		if !collision {
			return info, nil
		}
		s.rotateRecoveryLane(lane)
	}
	return RecoveryBranchInfo{}, fmt.Errorf("allocate isolated recovery lane: too many existing collisions")
}

func (s *Session) saveRecoveryBranchMeta(path string, opts RecoveryBranchOptions, preview string, turns int, digest string, depth int) (BranchMeta, error) {
	meta := opts.BranchMeta
	meta.ID = BranchID(path)
	if strings.TrimSpace(meta.Name) == "" {
		meta.Name = firstNonEmpty(strings.TrimSpace(opts.Name), RecoveryBranchDefaultName)
	}
	if strings.TrimSpace(meta.ParentID) == "" {
		meta.ParentID = BranchID(opts.OriginalPath)
	}
	meta.ForkTurn = -1
	meta.ForkMessageIndex = len(s.Snapshot())
	meta.Preview = preview
	meta.Turns = turns
	meta.SchemaVersion = BranchMetaCountsVersion
	meta.Recovered = true
	meta.RecoveryReason = firstNonEmpty(strings.TrimSpace(opts.Reason), "session snapshot conflict")
	meta.RecoveryDigest = digest

	meta.RecoveryDepth = depth
	if meta.Revision == 0 {
		meta.Revision = 1
	}
	if strings.TrimSpace(meta.ContentDigest) == "" {
		meta.ContentDigest = digest
	}
	if strings.TrimSpace(meta.WriterID) == "" {
		meta.WriterID = SessionWriterID()
	}

	if err := saveBranchMetaKeepInFlightTurn(path, meta); err != nil {
		return BranchMeta{}, err
	}
	if stored, ok, err := LoadBranchMeta(path); err != nil {
		return BranchMeta{}, err
	} else if ok {
		return stored, nil
	}
	return meta, nil
}

func recoveryParentStem(parent string) string {
	parent = strings.TrimSpace(parent)
	if parent == "" {
		return "session"
	}
	sum := sha256.Sum256([]byte(parent))
	if before, _, ok := strings.Cut(parent, "-recovery-"); ok {
		base := strings.Trim(before, "-_. ")
		if base == "" {
			base = "session"
		}
		base = strings.Trim(truncateUTF8Bytes(base, maxRecoveryParentStemBytes), "-_. ")
		if base == "" {
			base = "session"
		}
		return fmt.Sprintf("%s-%x", base, sum[:6])
	}
	if len(parent) <= maxRecoveryParentStemBytes {
		return parent
	}
	prefix := strings.Trim(truncateUTF8Bytes(parent, maxRecoveryParentStemBytes), "-_. ")
	if prefix == "" {
		prefix = "session"
	}
	return fmt.Sprintf("%s-%x", prefix, sum[:6])
}

func truncateUTF8Bytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	used := 0
	for i, r := range s {
		size := utf8.RuneLen(r)
		if size < 0 {
			size = 1
		}
		if used+size > max {
			return s[:i]
		}
		used += size
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// SessionRecoveryMaxDepth bounds nested recovery forks: a normal session may
// fork a recovery branch (depth 1), which may itself fork twice more under
// genuine repeated incidents; past that the caller should stop forking and
// write onto the branch it already owns.
const SessionRecoveryMaxDepth = 3

type sessionSaveMode int

const (
	sessionSaveSnapshot sessionSaveMode = iota
	sessionSaveRewrite
	sessionSaveRewriteCompact
)

type snapshotWriteDecision struct {
	revision   int64
	upToDate   bool
	appendFrom int
	appendOnly bool
	// repairLog is set when the on-disk event log was damaged (torn tail with
	// a lost suffix, or nothing decodable): the safe write shape is a full
	// rewrite that also compacts the log back to a healthy single event.
	repairLog bool
	// ledgerStale is set when the on-disk transcript already matches the
	// snapshot but the meta ledger still describes older content — the
	// aftermath of a save whose bytes landed and whose revision record then
	// failed. The up-to-date path must heal the ledger instead of skipping it.
	ledgerStale bool
}

type SessionSnapshotConflictError struct {
	Path             string
	Kind             SessionSnapshotConflictKind
	ExistingMessages int
	SnapshotMessages int
	BaseRevision     int64
	DiskRevision     int64
}

func (e *SessionSnapshotConflictError) Error() string {
	if e == nil {
		return ErrSessionSnapshotConflict.Error()
	}
	switch e.Kind {
	case SessionSnapshotConflictStalePrefix:
		return fmt.Sprintf("%s: %s has %d messages at revision %d; stale snapshot has %d messages from revision %d",
			ErrSessionSnapshotConflict, e.Path, e.ExistingMessages, e.DiskRevision, e.SnapshotMessages, e.BaseRevision)
	default:
		return fmt.Sprintf("%s: %s diverged on disk (%d messages, revision %d) from snapshot (%d messages, revision %d)",
			ErrSessionSnapshotConflict, e.Path, e.ExistingMessages, e.DiskRevision, e.SnapshotMessages, e.BaseRevision)
	}
}

func (e *SessionSnapshotConflictError) Unwrap() error {
	return ErrSessionSnapshotConflict
}

func SnapshotConflictKind(err error) (SessionSnapshotConflictKind, bool) {
	var conflict *SessionSnapshotConflictError
	if errors.As(err, &conflict) && conflict != nil {
		return conflict.Kind, true
	}
	return "", false
}

const RecoveryBranchDefaultName = "Recovered unsaved changes from stale runtime"

type RecoveryBranchOptions struct {
	OriginalPath string
	Name         string
	Reason       string
	BranchMeta   BranchMeta
}

type RecoveryBranchInfo struct {
	Path     string
	Digest   string
	Existing bool
	Meta     BranchMeta
	Preview  string
	Turns    int
}
