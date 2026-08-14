package agent

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"reasonix/internal/provider"
)

type sessionPersistState struct {
	path     string
	digest   [sha256.Size]byte
	version  uint64
	revision int64
	// revisionKnown marks revision as a real ledger value. It is false when
	// the baseline was established while the meta sidecar was unreadable
	// (torn or corrupt): the session must still open, but revision 0 must not
	// pose as a baseline or every honest on-disk revision would read as a
	// stale-runtime conflict. CAS checks fall back to digest+version until a
	// successful save re-learns the revision.
	revisionKnown bool
	// saveVerified marks a baseline established by a completed save in this
	// process, whose write path verified transcript and ledger agree. A
	// baseline adopted at load time pairs the disk transcript with whatever
	// the meta sidecar said — which can lag the transcript after an
	// interrupted save — so only save-verified baselines may arm the
	// snapshot no-op fast path; the first save after a load must run in full
	// and heal a stale ledger.
	saveVerified bool
	ok           bool
}

func (s *Session) ownsPersistedState(path string, existingDigest [sha256.Size]byte, existingRevision int64, existingLedgerDigest string, nextVersion uint64) bool {
	state := s.persistState(path)
	if !state.ok || state.version > nextVersion || !bytes.Equal(existingDigest[:], state.digest[:]) {
		return false
	}

	if !state.revisionKnown || existingRevision == 0 || state.revision == existingRevision {
		return true
	}

	return existingLedgerDigest == digestString(existingDigest)
}

// snapshotUpToDate reports whether a snapshot save to path is a provable
// no-op from in-memory bookkeeping alone: the last successful save went to
// this same path with a known ledger revision, the transcript version and
// rewrite version have not moved since, and no load-time repair or event-log
// damage is waiting to be persisted. Every one of these flags fails open —
// when any is unset or stale the caller falls through to the full save path,
// which re-derives the truth from disk.
func (s *Session) snapshotUpToDate(path string) bool {
	key := canonicalSessionSavePath(path)
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.persisted.ok &&
		s.persisted.saveVerified &&
		s.persisted.path == key &&
		s.persisted.version == s.version &&
		s.persisted.revisionKnown &&
		s.rewriteVersion == s.persistedRewriteVersion &&
		!s.normalizedDirty &&
		!s.eventLogDamaged
}

func (s *Session) persistState(path string) sessionPersistState {
	key := canonicalSessionSavePath(path)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.persisted.ok && s.persisted.path == key {
		return s.persisted
	}
	return sessionPersistState{}
}

// PersistedState is a read-only view of the baseline the session last
// persisted to (or loaded from) its transcript path. History paging uses it to
// validate a display-index sidecar against the live session without touching
// disk: an index built at the same revision with the same content digest
// describes exactly the persisted prefix of the in-memory log.
type PersistedState struct {
	// Digest is the content digest of the persisted transcript.
	Digest [sha256.Size]byte
	// DigestHex is Digest in the hex form sidecars store.
	DigestHex string
	// Revision is the CAS ledger revision of the persisted transcript. It is
	// meaningful only when RevisionKnown is true.
	Revision      int64
	RevisionKnown bool
	// RewriteEpoch is the highest rewriteVersion that has reached disk. It
	// changes only when a content rewrite (compaction, rewind, …) is saved, so
	// it doubles as a stable epoch token for history entry IDs: append-only
	// saves keep it, rewrites bump it.
	RewriteEpoch int
	// AppendOnlyTail reports that no rewrite landed after the baseline, so the
	// persisted transcript is still a prefix of the in-memory log (the tail,
	// if any, is unsaved appends).
	AppendOnlyTail bool
	// UnchangedSincePersisted reports that the in-memory log is exactly the
	// persisted transcript (no appends, no rewrites since the baseline).
	UnchangedSincePersisted bool
}

// PersistedState returns the session's persistence baseline for path, or
// false when the session has never persisted to (or loaded from) that path.
func (s *Session) PersistedState(path string) (PersistedState, bool) {
	key := canonicalSessionSavePath(path)
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.persisted.ok || s.persisted.path != key {
		return PersistedState{}, false
	}
	return PersistedState{
		Digest:                  s.persisted.digest,
		DigestHex:               digestString(s.persisted.digest),
		Revision:                s.persisted.revision,
		RevisionKnown:           s.persisted.revisionKnown,
		RewriteEpoch:            s.persistedRewriteVersion,
		AppendOnlyTail:          s.rewriteVersion == s.persistedRewriteVersion,
		UnchangedSincePersisted: s.persisted.version == s.version,
	}, true
}

// SessionContentIdentity returns the ledger identity of the authoritative
// persisted transcript without loading its message bodies. It is intended for
// derived sidecars such as the desktop display index: a matching checkpoint
// size alone cannot prove that offsets still describe the event-log-backed
// transcript. The bool is false for legacy sessions that have no digest in
// their branch metadata; callers must then validate against the transcript
// bytes directly.
func SessionContentIdentity(path string) (PersistedState, bool, error) {
	revision, digestHex, err := sessionContentRevision(path)
	if err != nil {
		return PersistedState{}, false, err
	}
	if digestHex == "" {
		return PersistedState{}, false, nil
	}
	var digest [sha256.Size]byte
	decoded, err := hex.DecodeString(digestHex)
	if err != nil || len(decoded) != len(digest) {
		return PersistedState{}, false, fmt.Errorf("invalid session content digest")
	}
	copy(digest[:], decoded)
	return PersistedState{
		Digest:        digest,
		DigestHex:     digestString(digest),
		Revision:      revision,
		RevisionKnown: true,
	}, true, nil
}

func (s *Session) markPersisted(path string, digest [sha256.Size]byte, version uint64, revision int64, rewriteVersion int) {
	s.setPersistedBaseline(path, digest, version, revision, true, true, rewriteVersion)
}

// markPersistedFromLoad anchors the baseline a loader learned from disk. The
// ledger revision is real, but the pairing of transcript and ledger was not
// verified by a write — an interrupted earlier save can leave the ledger
// describing older content — so the baseline never arms the snapshot no-op
// fast path.
func (s *Session) markPersistedFromLoad(path string, digest [sha256.Size]byte, version uint64, revision int64, rewriteVersion int) {
	s.setPersistedBaseline(path, digest, version, revision, true, false, rewriteVersion)
}

// markPersistedRevisionUnknown records a baseline whose ledger revision could
// not be learned because the meta sidecar was unreadable. The digest and
// version still anchor ownership checks; revision-based CAS stays disarmed
// until a successful save records the real revision via markPersisted.
func (s *Session) markPersistedRevisionUnknown(path string, digest [sha256.Size]byte, version uint64, rewriteVersion int) {
	s.setPersistedBaseline(path, digest, version, 0, false, false, rewriteVersion)
}

func (s *Session) setPersistedBaseline(path string, digest [sha256.Size]byte, version uint64, revision int64, revisionKnown, saveVerified bool, rewriteVersion int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persisted = sessionPersistState{
		path:          canonicalSessionSavePath(path),
		digest:        digest,
		version:       version,
		revision:      revision,
		revisionKnown: revisionKnown,
		saveVerified:  saveVerified,
		ok:            true,
	}

	if rewriteVersion > s.persistedRewriteVersion {
		s.persistedRewriteVersion = rewriteVersion
	}
	if saveVerified {

		s.normalizedDirty = false
		s.rawMessages = nil
		s.eventLogDamaged = false
	}
}

// sessionContentRevision reads the CAS ledger (revision + content digest) from
// the branch-meta sidecar. A missing sidecar is revision 0 — a session that
// has never recorded one. An unreadable sidecar is an error: reporting it as
// revision 0 would desync every runtime baseline from the ledger and turn the
// next honest save into a bogus conflict (and a recovery branch).
func sessionContentRevision(path string) (int64, string, error) {
	meta, ok, err := loadBranchMetaRetry(path)
	if err != nil {
		return 0, "", err
	}
	if !ok {
		return 0, "", nil
	}
	return meta.Revision, strings.TrimSpace(meta.ContentDigest), nil
}

func recordSessionContentRevision(path string, digest [sha256.Size]byte, baseRevision int64) (int64, error) {

	unlock, err := LockSessionMetaPath(path)
	if err != nil {
		return 0, err
	}
	defer unlock()

	meta, ok, err := loadBranchMetaRetry(path)
	if err != nil {

		return 0, err
	}
	if !ok {
		meta = BranchMeta{ID: BranchID(path)}
	}
	if meta.Revision < baseRevision {
		meta.Revision = baseRevision
	}
	meta.Revision++
	meta.ContentDigest = digestString(digest)
	meta.WriterID = SessionWriterID()
	if err := saveBranchMeta(path, meta, false); err != nil {
		return 0, err
	}
	stored, ok, err := loadBranchMetaRetry(path)
	if err != nil {
		return 0, err
	}
	if ok && stored.Revision > 0 {
		return stored.Revision, nil
	}
	return meta.Revision, nil
}

func digestString(digest [sha256.Size]byte) string {
	return fmt.Sprintf("%x", digest[:])
}

func SessionWriterID() string {
	return sessionWriterID
}

func newSessionWriterID() string {
	host, _ := os.Hostname()
	host = strings.TrimSpace(host)
	if host == "" {
		host = "unknown-host"
	}
	host = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '-'
		}
	}, host)
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), time.Now().UnixNano())
	}
	return fmt.Sprintf("%s-%d-%x", host, os.Getpid(), nonce[:])
}

func digestSessionMessages(msgs []provider.Message) ([sha256.Size]byte, error) {
	digest, _, err := digestAndSizeSessionMessages(msgs)
	return digest, err
}

func messageForSessionIdentity(m provider.Message) provider.Message {

	m.CreatedAt = 0
	return m
}

// digestAndSizeSessionMessages also reports the encoded transcript size, which
// the save path uses to bound the event log relative to the live content.
func digestAndSizeSessionMessages(msgs []provider.Message) ([sha256.Size]byte, int64, error) {
	h := sha256.New()
	size := int64(0)
	for _, m := range msgs {
		m = messageForSessionIdentity(m)
		b, err := json.Marshal(m)
		if err != nil {
			return [sha256.Size]byte{}, 0, err
		}
		if _, err := h.Write(b); err != nil {
			return [sha256.Size]byte{}, 0, err
		}
		if _, err := h.Write([]byte{'\n'}); err != nil {
			return [sha256.Size]byte{}, 0, err
		}
		size += int64(len(b)) + 1
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out, size, nil
}

func messagesHavePrefix(full, prefix []provider.Message) bool {
	if len(prefix) > len(full) {
		return false
	}
	for i := range prefix {
		if !messagesEqualForStorage(full[i], prefix[i]) {
			return false
		}
	}
	return true
}

// messagesPrefixDigestDepth returns the number of leading messages of msgs
// whose storage digest equals target, or -1 when no prefix matches. The
// digest accumulates exactly like digestAndSizeSessionMessages, so a match at
// depth k means msgs[:k] has the same transcript identity as target.
func messagesPrefixDigestDepth(msgs []provider.Message, target [sha256.Size]byte) int {
	h := sha256.New()
	sum := make([]byte, 0, sha256.Size)
	for i, m := range msgs {
		m = messageForSessionIdentity(m)
		b, err := json.Marshal(m)
		if err != nil {
			return -1
		}
		h.Write(b)
		h.Write([]byte{'\n'})
		sum = h.Sum(sum[:0])
		if bytes.Equal(sum, target[:]) {
			return i + 1
		}
	}
	return -1
}

// appendCoversPersistedBaseline reports whether an append-shaped write (disk
// transcript a prefix of next, modulo a compatible leading-system swap) still
// covers everything this session ever persisted: the baseline digest must be
// reachable as a prefix of the pending snapshot, and the disk transcript must
// still extend at least to that depth. A shorter disk transcript means some
// other runtime deliberately rewound below the baseline — appending over it
// would resurrect the removed suffix, so the caller must conflict instead.
func appendCoversPersistedBaseline(next, existing []provider.Message, baseDigest [sha256.Size]byte) bool {
	depth := messagesPrefixDigestDepth(next, baseDigest)
	if depth < 0 && len(next) > 0 && len(existing) > 0 &&
		next[0].Role == provider.RoleSystem && existing[0].Role == provider.RoleSystem &&
		!messagesEqualForStorage(next[0], existing[0]) {

		variant := append([]provider.Message{existing[0]}, next[1:]...)
		depth = messagesPrefixDigestDepth(variant, baseDigest)
	}
	return depth >= 0 && len(existing) >= depth
}

func messagesHavePrefixWithCompatibleSystem(full, prefix []provider.Message) bool {
	full = messagesWithoutLeadingSystem(full)
	prefix = messagesWithoutLeadingSystem(prefix)
	return messagesHavePrefix(full, prefix)
}

func messagesWithoutLeadingSystem(msgs []provider.Message) []provider.Message {
	if len(msgs) > 0 && msgs[0].Role == provider.RoleSystem {
		return msgs[1:]
	}
	return msgs
}

func messagesEqualForStorage(a, b provider.Message) bool {
	a = messageForSessionIdentity(a)
	b = messageForSessionIdentity(b)
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

func messagesEqualForStorageList(a, b []provider.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !messagesEqualForStorage(a[i], b[i]) {
			return false
		}
	}
	return true
}

func messagesCompatibleForStorageBaseline(a, b []provider.Message) bool {
	if messagesEqualForStorageList(a, b) {
		return true
	}
	return messagesEqualForStorageList(messagesWithoutLeadingSystem(a), messagesWithoutLeadingSystem(b))
}
func (s *Session) saveLocked(path string, mode sessionSaveMode) error {
	baseRevision := int64(0)
	releaseAuth, err := s.requireWriteAuthorityForSave(path)
	if err != nil {
		return err
	}
	defer releaseAuth()
	observeUnleasedSessionWrite(path, mode)

	if err := healEmptyCheckpointFromWAL(path); err != nil {
		return err
	}
	if mode == sessionSaveSnapshot && s.snapshotUpToDate(path) {

		return nil
	}

	msgs, version, rewriteVersion := s.snapshotWithVersion()
	digest, contentBytes, err := digestAndSizeSessionMessages(msgs)
	if err != nil {
		return err
	}
	probe, err := probeSessionEventLog(path)
	if err != nil {
		return err
	}
	if probe.futureSchema {
		return fmt.Errorf("session event log for %s uses schema %d; this build supports up to %d", path, probe.schemaVersion, sessionEventSchemaVersion)
	}
	if probe.native && probe.size > 0 {

		if err := repairSessionEventLogTail(path); err != nil {
			return fmt.Errorf("repair session event log: %w", err)
		}
	}
	repairLog := false
	ownedRewrite := mode == sessionSaveRewrite || mode == sessionSaveRewriteCompact
	decision, err := s.checkSnapshotWrite(path, msgs, digest, version, ownedRewrite)
	if err != nil {
		return err
	}
	if decision.upToDate && mode != sessionSaveRewriteCompact {

		if decision.ledgerStale {

			revision, err := recordSessionContentRevision(path, digest, decision.revision)
			if err != nil {
				return err
			}
			displayModelCurrent := true
			if err := writeSessionMessages(path, msgs); err != nil {
				displayModelCurrent = false
				slog.Warn("session: keeping save after display read-model repair failure", "path", path, "err", err)
			}
			if probe.native {
				if err := writeSessionEventIndex(path, msgs, digest, revision); err != nil {

					slog.Warn("session: keeping save after event index write failure", "path", path, "err", err)
				}
			}
			if displayModelCurrent {
				if err := refreshSessionDisplayIndex(path, msgs, digest, revision, -1); err != nil {

					slog.Warn("session: keeping save after display index write failure", "path", path, "err", err)
				}
			}
			s.markPersistedWithListing(path, digest, version, revision, rewriteVersion, msgs)
			return nil
		}
		s.markPersistedWithListing(path, digest, version, decision.revision, rewriteVersion, msgs)
		return nil
	}
	if decision.appendOnly && probe.native && mode != sessionSaveRewriteCompact {
		logSize := sessionEventLogSize(path)
		displayModelCurrent := false
		switch {
		case logSize == 0:
			if err := appendSessionReplaceEvent(path, msgs, digest, decision.revision, "snapshot"); err != nil {
				return err
			}
			displayModelCurrent, err = appendSessionDisplayReadModel(path, msgs, decision.appendFrom, decision.revision)
		case sessionEventLogOversized(logSize, contentBytes):

			if err := compactSessionEventLog(path, msgs, digest, decision.revision, "compact"); err != nil {
				return err
			}
			if err := writeSessionMessages(path, msgs); err != nil {
				return err
			}
			displayModelCurrent = true
		default:
			if err := appendSessionAppendEvent(path, decision.appendFrom, msgs[decision.appendFrom:], digest, decision.revision); err != nil {
				return err
			}
			displayModelCurrent, err = appendSessionDisplayReadModel(path, msgs, decision.appendFrom, decision.revision)
		}
		if err != nil {
			slog.Warn("session: keeping save after display read-model append failure", "path", path, "err", err)
		}
		revision, err := recordSessionContentRevision(path, digest, decision.revision)
		if err != nil {
			return err
		}
		if err := writeSessionEventIndex(path, msgs, digest, revision); err != nil {

			slog.Warn("session: keeping save after event index write failure", "path", path, "err", err)
		}
		if displayModelCurrent {
			if err := refreshSessionDisplayIndex(path, msgs, digest, revision, decision.appendFrom); err != nil {

				slog.Warn("session: keeping save after display index write failure", "path", path, "err", err)
			}
		}
		s.markPersistedWithListing(path, digest, version, revision, rewriteVersion, msgs)
		return nil
	}
	baseRevision = decision.revision
	repairLog = decision.repairLog

	reason := "save"
	switch mode {
	case sessionSaveSnapshot:
		reason = "snapshot"
	case sessionSaveRewrite:
		reason = "rewrite"
	case sessionSaveRewriteCompact:
		reason = "rewrite-compact"
	}
	if repairLog {
		reason = "repair"
	}
	logSize := sessionEventLogSize(path)
	switch {
	case !probe.native:

	case mode == sessionSaveRewriteCompact:

		if err := compactSessionEventLog(path, msgs, digest, baseRevision, reason); err != nil {
			return err
		}
	case repairLog, sessionEventLogOversized(logSize, contentBytes):
		if err := compactSessionEventLog(path, msgs, digest, baseRevision, reason); err != nil {
			return err
		}
	default:
		if err := appendSessionReplaceEvent(path, msgs, digest, baseRevision, reason); err != nil {
			return err
		}
	}
	if err := writeSessionMessages(path, msgs); err != nil {
		return err
	}
	revision, err := recordSessionContentRevision(path, digest, baseRevision)
	if err != nil {
		return err
	}
	if probe.native {
		if err := writeSessionEventIndex(path, msgs, digest, revision); err != nil {

			slog.Warn("session: keeping save after event index write failure", "path", path, "err", err)
		}
	}
	if err := refreshSessionDisplayIndex(path, msgs, digest, revision, -1); err != nil {

		slog.Warn("session: keeping save after display index write failure", "path", path, "err", err)
	}
	s.markPersistedWithListing(path, digest, version, revision, rewriteVersion, msgs)
	return nil
}
