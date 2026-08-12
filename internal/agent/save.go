package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"reasonix/internal/fileutil"
	"reasonix/internal/provider"
	"reasonix/internal/store"
)

// nameMaxBytes is the single-component filename limit shared by the
// filesystems Reasonix targets (APFS, ext4, NTFS all cap at 255).

// maxSessionBasenameBytes bounds transcript basenames that reconciliation
// leaves in place. Sidecars append up to ~16 bytes to the transcript name
// or its stem (".lease.lock", ".cleanup-pending.json", ".guardian.jsonl"),
// so 224 keeps every sidecar comfortably under nameMaxBytes with headroom
// for future suffixes. Names past this bound come from the pre-bounded
// recovery cascade and get renamed by reconcileOverlongSessionFilenames.

var (
	sessionSaveLocks sync.Map
	// sessionFileLockWait bounds cross-process save-lock acquisition. Session
	// leases normally prevent competing writers, but CLI/legacy writers and a
	// stalled process can still hold the compatibility .lock file. Navigation
	// and desktop shutdown snapshot synchronously; waiting forever here wedges
	// the UI and keeps the session lease (and WebView) alive indefinitely.
	// Package vars let focused tests shorten the wait without slowing the suite.
	sessionFileLockWait         = 5 * time.Second
	sessionFileLockPollInterval = 25 * time.Millisecond
	sessionMetaLockWait         = 5 * time.Second
	ErrSessionSnapshotConflict  = errors.New("session snapshot conflicts with newer transcript")
	// ErrSessionExternallyRemoved means a live Session still has a verified
	// baseline for path, but every authoritative transcript artifact disappeared.
	// Treating it as a first save would silently recreate a file the user or an
	// external cleanup tool deliberately removed.
	ErrSessionExternallyRemoved = errors.New("session was removed while still open")
	ErrSessionRecoveryNotNeeded = errors.New("session recovery not needed")
	// ErrSessionFileLockHeld reports that another process kept the
	// compatibility save lock for the full bounded acquisition window. Callers
	// that are about to terminate can use this sentinel to persist a recovery
	// branch without waiting on the same stalled file again.
	ErrSessionFileLockHeld = errors.New("session file lock held")
	// ErrSessionRecoveryDepthExceeded refuses a recovery fork whose parent is
	// already SessionRecoveryMaxDepth recovery forks deep. A chain that deep
	// means saves keep conflicting on branches this runtime itself created;
	// forking further multiplies session files without converging (#5993).
	ErrSessionRecoveryDepthExceeded = errors.New("session recovery chain depth exceeded")
	sessionWriterID                 = newSessionWriterID()
)

// SessionRecoveryMaxDepth bounds nested recovery forks: a normal session may
// fork a recovery branch (depth 1), which may itself fork twice more under
// genuine repeated incidents; past that the caller should stop forking and
// write onto the branch it already owns.

// revisionKnown marks revision as a real ledger value. It is false when
// the baseline was established while the meta sidecar was unreadable
// (torn or corrupt): the session must still open, but revision 0 must not
// pose as a baseline or every honest on-disk revision would read as a
// stale-runtime conflict. CAS checks fall back to digest+version until a
// successful save re-learns the revision.

// saveVerified marks a baseline established by a completed save in this
// process, whose write path verified transcript and ledger agree. A
// baseline adopted at load time pairs the disk transcript with whatever
// the meta sidecar said — which can lag the transcript after an
// interrupted save — so only save-verified baselines may arm the
// snapshot no-op fast path; the first save after a load must run in full
// and heal a stale ledger.

// repairLog is set when the on-disk event log was damaged (torn tail with
// a lost suffix, or nothing decodable): the safe write shape is a full
// rewrite that also compacts the log back to a healthy single event.

// ledgerStale is set when the on-disk transcript already matches the
// snapshot but the meta ledger still describes older content — the
// aftermath of a save whose bytes landed and whose revision record then
// failed. The up-to-date path must heal the ledger instead of skipping it.

// Save persists the session using the normal CAS-protected snapshot protocol.
// It is kept as the convenient default for callers that do not need to spell
// out rewrite intent; it must never provide a force-overwrite escape hatch.
// The .jsonl file remains as a compatibility checkpoint and discovery anchor;
// the append-only event log is authoritative once present, while the .jsonl
// checkpoint also serves as the random-read model for history paging.
func (s *Session) Save(path string) error {
	return s.saveObserved(path, sessionSaveSnapshot)
}

// SaveSnapshot writes a normal autosave/snapshot only when doing so cannot hide
// a newer transcript already on disk. Explicit history rewrites such as rewind,
// compaction, and cancel recovery should call SaveRewrite instead.
func (s *Session) SaveSnapshot(path string) error {
	return s.saveObserved(path, sessionSaveSnapshot)
}

// SaveRewrite writes an intentional non-append history rewrite only while this
// Session still owns the current on-disk transcript baseline. It prevents a
// stale controller from force-rewinding a newer transcript written elsewhere.
func (s *Session) SaveRewrite(path string) error {
	return s.saveObserved(path, sessionSaveRewrite)
}

// SaveRewriteCompact performs a CAS-protected rewrite and folds the event log
// to one replace record. It is for destructive maintenance such as redaction:
// retaining old WAL records would keep the removed bytes recoverable on disk.
func (s *Session) SaveRewriteCompact(path string) error {
	return s.saveObserved(path, sessionSaveRewriteCompact)
}

func (s *Session) save(path string, mode sessionSaveMode) error {
	if path == "" {
		return fmt.Errorf("empty session path")
	}
	return s.withSessionSaveLocks(path, func() error {
		return s.saveLocked(path, mode)
	})
}

func (s *Session) withSessionSaveLocks(path string, fn func() error) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("empty session path")
	}
	unlock := lockSessionSavePath(path)
	defer unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}
	unlockFile, err := lockSessionFile(path)
	if err != nil {
		return fmt.Errorf("lock session file: %w", err)
	}
	defer unlockFile()
	return fn()
}

func sessionArtifactExists(path string) bool {
	if _, err := os.Lstat(path); err == nil {
		return true
	}
	for _, artifact := range store.SessionSidecarFiles(path) {
		// A legacy v0 event transcript can share the native
		// `<id>.events.jsonl` name with the destination being imported. It is
		// intentionally left in place, and must not make SaveIfAbsent believe
		// that the v1 `<id>.jsonl` destination already exists. Native event logs
		// remain owned artifacts and still protect the destination from a second
		// writer.
		if artifact == store.SessionEventLog(path) {
			probe, probeErr := probeSessionEventLog(path)
			if probeErr != nil {
				return true
			}
			if !probe.native {
				continue
			}
		}
		if _, err := os.Lstat(artifact); err == nil {
			return true
		}
	}
	return false
}

// Heal an empty/missing checkpoint from a valid WAL before classification
// so a 0-byte .jsonl never forces a false diverged recovery.

// Nothing changed since the last successful save to this exact path:
// skip the rest of the save — including the full transcript serialize
// + digest + disk probe the up-to-date decision below would still
// pay. Desktop switch/close/prune paths snapshot defensively on every
// navigation, and on large sessions that per-save cost is the
// user-visible seconds of UI freeze in #6607. Version bookkeeping
// makes this exact: any Add/Replace/preview update bumps version, any
// rewrite bumps rewriteVersion, and load-time repairs or log damage
// disarm the fast path until a real save persists them. The check
// runs under the save locks, not before them, so a saver that waited
// on a concurrent writer still re-evaluates against the state it must
// persist when it finally enters the critical section.

// Capture the snapshot only while holding the save locks. Concurrent
// in-process savers (turn-end snapshot, periodic autosave, shutdown
// snapshot) that captured before locking could land out of order: the
// stalest capture written last would then read the newer transcript it
// lost the race to as a bogus stale-prefix conflict.

// Drop any torn tail a crashed or disk-full append left behind before
// it can be buried under new records where replay would stop forever.

// Disk already holds exactly this transcript. Rewriting it would only
// bump the revision, invalidating the persistence baseline of every
// other runtime resumed on this file and turning their next
// legitimate save into a stale-runtime conflict. Skip the write and
// adopt the current on-disk revision as this session's baseline.

// ...unless the ledger never learned about this transcript: a
// prior save landed its bytes and then failed to record the
// revision. Same-content retries are exactly the "later save"
// that failure deferred to, and skipping here would strand the
// ledger on the old digest forever. Record now, reproducing
// the state the interrupted save would have left.

// See the append path below: index loss must not fail a
// save whose transcript and revision already landed.

// The display index is a derived sidecar; transcript durability
// must not depend on rebuilding it successfully.

// Fold history into one replace event and refresh the random-read
// model atomically. Normal appends keep it current below too.

// The event index is only a listing accelerator; the transcript
// and its revision are already durable above. Failing the save
// here would skip markPersisted and leave the in-memory baseline
// behind the disk state it just wrote, misreading the next save
// as a stale-runtime conflict.

// The append boundary lets the refresh extend the previous index
// instead of re-encoding the whole transcript.

// Full-rewrite path: new snapshots, intentional history rewrites, and
// damage repairs. The event log mutates first so a crash between the two
// writes leaves the newer transcript authoritative; the anchor rewrite
// keeps the compatibility .jsonl fresh for direct readers.

// A foreign file (legacy import leftover) squats the native log path.
// Never write into or over it — the session stays checkpoint-only.

// Maintenance rewrites compact even a short log so redacted or otherwise
// removed bytes do not remain recoverable in historical WAL records.

// See the append path above: index loss must not fail a save whose
// transcript and revision already landed.

// Warn-only like the event index above: the display index is a pure
// derived sidecar and must never fail a save.

func writeSessionMessages(path string, msgs []provider.Message) error {
	// Write to a sibling tmp file then rename, so a crash mid-write can't
	// leave a partial JSONL that won't reload. The fsync guards the anchor
	// against power loss — it is the fallback when the event log is damaged.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".session.*.tmp")
	if err != nil {
		return fmt.Errorf("create session tmp: %w", err)
	}
	tmpPath := tmp.Name()
	enc := json.NewEncoder(tmp)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			tmp.Close()
			os.Remove(tmpPath)
			return fmt.Errorf("encode message: %w", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := fileutil.ReplaceFile(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// checkSnapshotWrite decides whether this session may write msgs over path, and
// whether the safe write shape is a no-op, append-only suffix, or full rewrite.

// raw is the transcript as stored, before load-time normalization repaired
// it; it equals existing when no repair ran. The prefix checks below must
// be able to fall back to it: a mid-turn snapshot legitimately cuts an
// assistant tool call from its still-running result, normalization then
// fabricates a placeholder answer on load, and the live session's real
// result collides with that placeholder — misreading a pure append as
// divergence (and forking a bogus recovery branch).

// The snapshot supersedes the repaired view — appending it lands
// the real tool results where the placeholders were fabricated —
// so no load-time repair is left to force a rewrite.

// Revision equality alone is not ownership proof. Require digest
// ancestry or a live generation-bound write authority covering path.

// An unknown-revision baseline (meta sidecar unreadable at load) cannot
// vouch for revision equality; the digest/prefix checks above already
// vouch for the content, so only a known baseline arms the CAS check.
// Under an append-shaped write (at most a compatible leading-system
// swap) a stale revision is ledger drift — a reset sidecar, a
// same-content heal, or another runtime recording messages this
// snapshot already contains — unless the transcript was rewound.
// Locating the persisted baseline among the snapshot's prefixes and
// requiring the disk transcript to still reach it tells the two apart:
// drift keeps the baseline reachable, while a rewind cut below it and
// appending would resurrect the suffix another runtime removed.

// A normalized-dirty load means LoadSession repaired the history on the
// way in: the digests match but the raw bytes on disk do not, so the
// repair still needs a real write to persist. A damaged event log
// likewise needs a real write (rewrite + compact) even when the
// replayable prefix already matches this snapshot.

// A ledger digest that describes different content than the transcript
// on disk is the aftermath of a save whose bytes landed and whose
// revision record then failed (crash or fail-closed record between the
// two writes). Only a non-empty mismatch counts: a missing sidecar or
// a legacy one without a digest is a legitimate state, and stamping it
// here would bump revisions other runtimes still hold as baselines.

// An append is only chain-safe when existing measures the transcript
// the event log actually replays. Under a pending load-time repair the
// normalized view differs from the raw log, so an append event indexed
// against it breaks the replay chain and orphans the appended suffix;
// fall through to the full rewrite, which also persists the repair.

// Bound controllers: missing/stale authority must not fork recovery.

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

// SaveConflictRecoveryBranch writes the depth-cap isolated copy (one path per
// live Session). Subsequent conflicts from that Session rewrite it in place.

// Judge coverage against the pre-repair transcript too, for the
// same reason as checkSnapshotWrite: load-time normalization can
// reshape what is actually stored, and a recovery fork is only
// warranted when the stored bytes themselves fail to cover this
// snapshot.

// Refuse to deepen a runaway chain: forking FROM a branch that is already
// at the depth cap only multiplies recovery files (#5993 reached 8 nested
// levels). The caller preserves the stale transcript in a writer-specific
// isolated branch instead of replacing the contested canonical branch.

// Legacy recovery meta predating RecoveryDepth.

// A shutdown copy is allowed even when the ordinary conflict chain is
// capped because losing the only in-memory transcript is worse than one
// additional branch. Keep the saturated depth so later ordinary saves
// still enforce the existing anti-cascade policy.

// A live Session gets one stable lane. A different live Session in the same
// process gets a different lane, so it never overwrites an independent
// recovery branch merely because the process-wide writer ID matches.

// Always stamped from the parent chain, never trusted from opts: callers
// copy tab/session meta wholesale and would carry a stale depth.

// Keep any in-flight turn marker across isolated in-place rewrites.

// An unknown-revision baseline still owns the transcript it loaded — the
// digest+version match proves it. Requiring revision equality here would
// make every rewrite from such a baseline a permanent conflict, because
// the revision can only be re-learned by a successful save.
// A disk ledger with no recorded revision is the mirror case: recorded
// revisions start at 1, so revision 0 means the sidecar was deleted or
// rebuilt by a listing-only writer after this session's save. An absent
// claim cannot revoke the ownership the digest+version match proves.

// A foreign revision stamp whose recorded digest still describes these
// exact bytes (a same-content heal or no-op record by another runtime)
// vouches for no content of its own: the transcript is byte-for-byte what
// this session last persisted, so rewriting it destroys nothing of
// theirs — at worst the conflict moves to the stamper's next divergent
// save, where its in-memory history forks a recovery branch as usual.
// A stamp that disagrees with the on-disk transcript (or a legacy stamp
// with no digest) keeps revoking ownership: that is the aftermath of a
// save whose bytes and record split, the bytes cannot be attributed, and
// only the conservative conflict path preserves both sides.

// snapshotUpToDate reports whether a snapshot save to path is a provable
// no-op from in-memory bookkeeping alone: the last successful save went to
// this same path with a known ledger revision, the transcript version and
// rewrite version have not moved since, and no load-time repair or event-log
// damage is waiting to be persisted. Every one of these flags fails open —
// when any is unset or stale the caller falls through to the full save path,
// which re-derives the truth from disk.

// PersistedState is a read-only view of the baseline the session last
// persisted to (or loaded from) its transcript path. History paging uses it to
// validate a display-index sidecar against the live session without touching
// disk: an index built at the same revision with the same content digest
// describes exactly the persisted prefix of the in-memory log.

// Digest is the content digest of the persisted transcript.

// DigestHex is Digest in the hex form sidecars store.

// Revision is the CAS ledger revision of the persisted transcript. It is
// meaningful only when RevisionKnown is true.

// RewriteEpoch is the highest rewriteVersion that has reached disk. It
// changes only when a content rewrite (compaction, rewind, …) is saved, so
// it doubles as a stable epoch token for history entry IDs: append-only
// saves keep it, rewrites bump it.

// AppendOnlyTail reports that no rewrite landed after the baseline, so the
// persisted transcript is still a prefix of the in-memory log (the tail,
// if any, is unsaved appends).

// UnchangedSincePersisted reports that the in-memory log is exactly the
// persisted transcript (no appends, no rewrites since the baseline).

// PersistedState returns the session's persistence baseline for path, or
// false when the session has never persisted to (or loaded from) that path.

// SessionContentIdentity returns the ledger identity of the authoritative
// persisted transcript without loading its message bodies. It is intended for
// derived sidecars such as the desktop display index: a matching checkpoint
// size alone cannot prove that offsets still describe the event-log-backed
// transcript. The bool is false for legacy sessions that have no digest in
// their branch metadata; callers must then validate against the transcript
// bytes directly.

// markPersistedFromLoad anchors the baseline a loader learned from disk. The
// ledger revision is real, but the pairing of transcript and ledger was not
// verified by a write — an interrupted earlier save can leave the ledger
// describing older content — so the baseline never arms the snapshot no-op
// fast path.

// markPersistedRevisionUnknown records a baseline whose ledger revision could
// not be learned because the meta sidecar was unreadable. The digest and
// version still anchor ownership checks; revision-based CAS stays disarmed
// until a successful save records the real revision via markPersisted.

// rewriteVersion was captured together with the persisted snapshot; only
// move forward so a slower save that captured earlier cannot roll the
// baseline back below a rewrite a faster save already persisted.

// A completed save landed the current transcript — including any
// load-time normalization repair — and healed the on-disk event log
// (tail repair runs on every save; a damaged log forces the
// rewrite-and-compact shape). Leaving these flags set would disarm
// the snapshot no-op fast path for the rest of the process lifetime,
// so a session that was repaired once kept paying a full serialize +
// digest on every defensive snapshot. Nothing reads the live
// session's copies after a save: checkSnapshotWrite re-loads the
// on-disk state and consults that object's flags, not these.

// sessionContentRevision reads the CAS ledger (revision + content digest) from
// the branch-meta sidecar. A missing sidecar is revision 0 — a session that
// has never recorded one. An unreadable sidecar is an error: reporting it as
// revision 0 would desync every runtime baseline from the ledger and turn the
// next honest save into a bogus conflict (and a recovery branch).

// Revision allocation is a read-modify-write transaction. Holding only the
// final SaveBranchMeta lock would still let another writer replace the
// sidecar between our read and write, so keep the ledger lock across the
// increment and read-back as well.

// Fail the save instead of rebuilding the ledger from a bad read: the
// transcript bytes already landed, so a later save can record the
// revision once the sidecar reads cleanly again — a content-bearing
// save lands here again, and a same-content retry heals through the
// up-to-date ledgerStale path in save().

// CreatedAt is local display metadata. Keep it out of transcript identity
// so older builds that ignore the optional field can share the same event-
// log revision and append without false conflicts.

// digestAndSizeSessionMessages also reports the encoded transcript size, which
// the save path uses to bound the event log relative to the live content.

// messagesPrefixDigestDepth returns the number of leading messages of msgs
// whose storage digest equals target, or -1 when no prefix matches. The
// digest accumulates exactly like digestAndSizeSessionMessages, so a match at
// depth k means msgs[:k] has the same transcript identity as target.

// appendCoversPersistedBaseline reports whether an append-shaped write (disk
// transcript a prefix of next, modulo a compatible leading-system swap) still
// covers everything this session ever persisted: the baseline digest must be
// reachable as a prefix of the pending snapshot, and the disk transcript must
// still extend at least to that depth. A shorter disk transcript means some
// other runtime deliberately rewound below the baseline — appending over it
// would resurrect the removed suffix, so the caller must conflict instead.

// A resume that swapped the system prompt persisted its baseline with
// the previous system message — the one still on disk. Re-anchor the
// search on that message so the swap alone doesn't hide the baseline.

// lockSessionFile waits briefly for the cross-process compatibility save lock.
// A short overlap with a legitimate writer is allowed to settle, but an
// stalled or indefinitely held lock fails the save instead of freezing tab
// switching or application shutdown. The caller keeps its in-memory transcript
// and can retry through the existing autosave/recovery paths.

// LockSessionMetaPath serializes a complete branch-meta read-modify-write
// cycle with both goroutines in this process and other Reasonix processes.
// Callers must hold it from the first read through the final replace.

// UpdateBranchMeta is the owner-level metadata API. The callback runs while
// the cross-process metadata lock is held, so callers cannot accidentally
// load a stale sidecar and overwrite fields written by another runtime.

// Resolve physical identity, not just spelling. Otherwise a symlink or
// junction alias can acquire a second sidecar lock for the same transcript.

// resolvePathThroughExistingAncestor resolves the deepest existing ancestor
// and appends every still-missing component. Fresh sessions can be nested under
// directories that have not been created yet; resolving only the immediate
// parent leaves aliases above that directory split into different lease keys.

// CanonicalSessionPath is the identity key of a session path: cleaned,
// absolute, and case-folded on Windows, matching the key form used by the
// lease registry and the save-path locks. Any runtime bookkeeping that
// compares or maps session paths (desktop tabs, detached runtimes) must use
// this exact form, or the same file splits into distinct keys — e.g.
// `C:\Users\...` vs the lease's lowercased `c:\users\...`. Empty input stays
// empty instead of resolving to the working directory.

// LoadSession reads a saved session into a fresh Session value. New sessions
// replay the append-only event log; legacy sessions without an event log fall
// back to the compatibility .jsonl checkpoint. A damaged log is replayed to its
// last clean record (or the checkpoint when nothing decodes) and flagged so the
// next save heals it with a rewrite-and-compact.
// In-process loads share the save path mutex so they cannot observe a local
// SaveSnapshot between appending an event-log record and refreshing the index.
// Missing files surface as os.IsNotExist so callers can fall through to a
// new session.

// Repair persisted-history-safe issues before anything reads the session.
// Old sessions (pre adde2d3e) and interrupted turns can carry empty tool-call
// names, dangling tool_calls, or half-streamed argument JSON that DeepSeek
// rejects with a 400 on replay. Wire-only cleanup, such as dropping orphan
// tool messages, stays in the provider send path so Save/LoadSession keeps
// its round-trip contract. The fast path returns the input slice unchanged
// for a well-formed history, so we detect an actual repair by comparing
// slice headers: when NormalizeSession allocated a new backing array, the
// session is marked dirty so the next Save persists the fix.

// Keep the pre-repair transcript: checkSnapshotWrite must be able to
// recognize a snapshot that extends the bytes actually on disk, which
// the repaired view no longer represents (an interrupted tool turn
// gets a placeholder result fabricated here that the live session
// answered for real).

// The sidecar exists but is unreadable even after retries (torn or
// corrupt). The session must still open, but revision 0 must not
// pose as a real baseline: the next save would misread the honest
// on-disk revision as another runtime's write and fork a recovery
// branch. Anchor the baseline on digest+version only until a
// successful save re-learns the revision.

// SessionInfo summarises a saved session for the --resume picker: where it is on
// disk, when it was created/last active, the first user message as a preview, and
// a rough turn count.

// compatibility alias for LastActivityAt

// SessionOrderInfo is the lightweight sidecar/mtime ordering record shared by
// session pickers and prompt-history navigation. It intentionally avoids reading
// JSONL content; callers that need previews can layer that on afterwards.

// compatibility alias for LastActivityAt

// Turns and Preview are the cached listing fields from the sidecar; SchemaVersion
// >= agent.BranchMetaCountsVersion means they were recorded from content and can
// be trusted (even Turns == 0). ListSessions uses them to skip the whole-file decode.

// Revision and ContentDigest bind a listing backfill to the transcript
// generation it decoded. They are sidecar-only compare-and-apply guards and
// are not exposed through SessionInfo.

// CleanupPendingMeta records that a session was logically removed but still has
// artifacts waiting for a background job to unwind before physical cleanup.

// CleanupPendingInfo describes one durable delayed-cleanup marker and the
// session transcript it belongs to.

// CleanupPendingPath returns the durable marker path for a session transcript.

// MarkCleanupPending hides a logically removed session from resume/list surfaces
// until delayed physical cleanup has finished.

// The marker controls session visibility during delayed cleanup. Publish it
// atomically so a crash cannot leave malformed JSON that hides the session
// and blocks reconciliation on the next startup.

// ClearCleanupPending removes a delayed-cleanup marker after physical cleanup.

// IsCleanupPending reports whether a session is hidden pending delayed cleanup.

// IsVisibleSession reports whether a persisted session should appear on normal
// user/agent-facing list, restore, and retrieval surfaces.

// ListCleanupPending returns delayed-cleanup markers left in dir. A missing
// directory is not an error.

// ReconcileCleanupPending retries physical cleanup for leftover delayed-cleanup
// markers and stale lock/lease sidecars. It keeps going after individual
// cleanup errors and returns them joined.

// ReconcileSessionSidecars renames transcripts whose filenames outgrew their
// sidecars and removes stale lock and lease files left beside sessions by
// older runtimes. It never removes .jsonl transcripts; recovered conversations
// may contain useful user history even when their names are ugly.

// Re-list after the rename pass: it retires old names and their sidecars.

// The removal is atomic with the release (unlink-under-flock on Unix,
// delete-disposition on the held handle on Windows), so a concurrent
// saver can never acquire a lock file that is being deleted under it.

// removeStaleSessionLeaseLockSidecar retires a leftover .lease.lock. The file
// is the lease lock itself, so taking it non-blocking proves no runtime holds
// the lease, and RemoveAndUnlock deletes it atomically with the release.

// removeStaleSessionLeaseInfoSidecar retires a leftover .lease.json while
// holding the lease lock, so no runtime can adopt the info file mid-removal.
// The info file itself is never held open by anyone, so a plain remove under
// the lock is safe on every platform.

// No lease lock file: holders keep it present (and locked) for their whole
// lifetime, so the leftover info sidecar has no owner to race with.

// sessionLockSidecarFits reports whether basePath's .lock sidecar name stays
// within the filesystem's per-component limit; past it, no process can hold
// (or ever have held) the file lock, because the lock file cannot be created.

// sessionLeaseSidecarFits is the lease-file analogue of sessionLockSidecarFits.

// reconcileOverlongSessionFilenames renames transcripts whose basenames grew
// past maxSessionBasenameBytes — the leftover shape of the pre-bounded
// recovery cascade (#5923), where lock and lease sidecars could no longer be
// created and the session became unsaveable. The conversation bytes are kept
// verbatim under a bounded name derived the same way new recovery branches
// are named; branch meta moves along with its ID rewritten, and sessions
// pointing at the old ID are re-parented so lineage survives the rename.

// old branch ID -> new branch ID

// Being deleted; renaming would orphan the cleanup marker.

// A non-empty newID means the transcript rename landed even if some
// sidecar migration failed; record it so children still re-parent —
// this run is the only one that knows the old-to-new mapping.

// renameOverlongSession moves one overlong transcript to its bounded name and
// migrates the sidecars that carry user state. It returns the new branch ID,
// or "" when the session was skipped because a runtime may still own it.

// Names past the sidecar limit cannot have lease or lock holders in any
// process — the holder files themselves are uncreatable — so probing them
// would only manufacture ENAMETOOLONG errors and wrongly skip the exact
// sessions this pass exists to repair.

// Nothing moved: the old transcript is intact and the next
// reconciliation can retry, so its lock file stays in place too.

// The transcript is committed under its new name from here on. Sidecar
// migration and lock cleanup failures are reported, but the new ID is
// still returned so the caller re-parents children: the old name is gone,
// and a later run would have no way to reconstruct this mapping.

// Retire the old disposable lease sidecars: any holder was ruled out
// above, and nothing keeps these files open, so a plain remove is safe.

// The old .lock goes atomically with the release of the lock we hold on it.

// migrateSessionSidecars moves the user-state sidecars of a renamed session:
// branch meta (with its ID rewritten to match the new filename), goal state,
// and the checkpoint/job directories. Lock and lease files are disposable and
// are removed by the caller instead.

// A source name past the filesystem limit cannot exist; renaming it
// would just manufacture ENAMETOOLONG instead of a clean not-exist.

// reparentSessionBranches rewrites ParentID references from renamed branch IDs
// to their bounded replacements so the branch tree stays connected.

// ListSessionOrder returns every *.jsonl session under dir in the same
// most-recently-active order used by ListSessions, using only file metadata and
// branch sidecars. A missing directory is not an error.

// Old recovery files may lack Recovered meta; filename still proves
// automatic recovery lineage for catalog folding.

// ListSessions returns every non-empty *.jsonl session under dir,
// most-recently-active first, each with a preview line so the picker can show
// something the user recognises. It never decodes a transcript: legacy counts
// remain explicitly unknown until the session catalog's single repair worker
// validates them. A missing directory is not an error.

// Never had user interaction — an empty conversation that should not
// appear in the history panel or the resume picker.

// SessionPreview returns the same preview and user-turn count used by
// ListSessions for one session file.

// SessionPreviewFromMessages computes the same preview line and user-turn count
// as previewSession, but from an in-memory message slice. Session.Save writes
// exactly these messages to the .jsonl, so this is byte-for-byte equivalent to
// decoding the file — letting the autosave path persist the counts into the
// sidecar without a disk read.

// previewSession returns the first user message (truncated) and the number of
// user-role messages so the picker can show "5 turns · 'help me debug the…'".
// Errors are swallowed — a malformed file just shows up with an empty preview.

// previewProse drops the leading @file references a prompt opens with so the
// preview shows what was asked rather than a row of paths. A prompt that is
// nothing but references keeps them — there is nothing else to show.

// truncatePreview clamps a preview line to 80 runes with an ellipsis, matching
// what the pickers render.

// ContinueSessionPath returns where a conversation carried into a rebuilt
// controller (model switch, config change) should keep auto-saving: its existing
// file when it has one, so the continued session stays a single file instead of
// the old one being orphaned as an identical duplicate (#2807). A session with no
// file yet gets a fresh path; "" when persistence is disabled.

// NewSessionPath returns the path to use for a fresh session, namespaced by
// the model so the filename hints at what the conversation was with. dir is
// typically config.SessionDir().
