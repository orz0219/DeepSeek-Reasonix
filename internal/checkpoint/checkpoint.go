// Package checkpoint is reasonix's snapshot-based edit safety net. Before a writer
// tool changes a file, the agent records the file's pre-edit content here, keyed
// to the current user turn; a frontend can then rewind the workspace (and, via the
// controller, the conversation) to an earlier turn.
//
// It is deliberately git-free (like Claude Code's rewind): snapshots live beside
// the session, never touch the user's git, and work in a non-git directory. Only
// edit-tool changes are tracked — bash side effects are not (a shell command's
// targets can't be known in advance), which is why the capture hook only fires for
// tools that can Preview their change.
//
// Schema v2 adds content-addressed blob storage, after-write fingerprints,
// coverage gaps, and transactional restore with compensation.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"reasonix/internal/fileutil"
	fileenc "reasonix/internal/fileutil/encoding"
)

// FileSnap is one file's state at the moment it was first touched in a turn.
// Content == nil means the file did not exist then, so a restore deletes it.
//
// v2 fields (Mode, SHA256, BlobRef, After*, CaptureSource) are omitempty so v1
// readers ignore them and old JSON still unmarshals cleanly.
type FileSnap struct {
	Path          string        `json:"path"`
	Content       *string       `json:"content"`
	Encoding      *fileenc.Kind `json:"encoding,omitempty"`
	Mode          uint32        `json:"mode,omitempty"`
	SHA256        string        `json:"sha256,omitempty"`
	BlobRef       string        `json:"blobRef,omitempty"`
	CaptureSource CaptureSource `json:"captureSource,omitempty"`
	AfterSHA256   string        `json:"afterSha256,omitempty"`
	AfterExisted  *bool         `json:"afterExisted,omitempty"`
	AfterMode     uint32        `json:"afterMode,omitempty"`
	// PayloadExpired marks that the blob was GC'd while metadata remains.
	PayloadExpired bool `json:"payloadExpired,omitempty"`
}

// FileState is the earliest pre-edit state recorded for a file in this
// session. Content == nil means the file did not exist before the session's
// first tracked edit.

// true when session has after-fingerprint ownership

// Checkpoint anchors the pre-edit state of every distinct file touched during one
// user turn. MsgIndex is len(Session.Messages) at the turn's start — the
// conversation-rewind boundary — persisted so a resumed session can rewind the
// conversation and fork, not just the code.
type Checkpoint struct {
	SchemaVersion      int            `json:"schemaVersion,omitempty"`
	Turn               int            `json:"turn"`
	Time               time.Time      `json:"time"`
	Prompt             string         `json:"prompt"`
	MsgIndex           int            `json:"msgIndex"`
	SessionID          string         `json:"sessionId,omitempty"`
	Files              []FileSnap     `json:"files"`
	Coverage           Coverage       `json:"coverage,omitempty"`
	CoverageGaps       []CoverageGap  `json:"coverageGaps,omitempty"`
	ActiveWriters      []ActiveWriter `json:"activeWriters,omitempty"`
	LastMutationSeq    int64          `json:"lastMutationSeq,omitempty"`
	SessionRevision    int64          `json:"sessionRevision,omitempty"`
	Legacy             bool           `json:"legacy,omitempty"`
	ExpiredFilePayload bool           `json:"expiredFilePayload,omitempty"`
}

// revisions returns FileRevision views of Files.
func (c *Checkpoint) revisions() []FileRevision {
	if c == nil {
		return nil
	}
	out := make([]FileRevision, 0, len(c.Files))
	for _, f := range c.Files {
		rev := FileRevision{
			Path:          f.Path,
			Existed:       f.Content != nil || f.BlobRef != "" || f.SHA256 != "",
			Mode:          f.Mode,
			Encoding:      f.Encoding,
			SHA256:        f.SHA256,
			BlobRef:       f.BlobRef,
			CaptureSource: f.CaptureSource,
			AfterSHA256:   f.AfterSHA256,
			AfterExisted:  f.AfterExisted,
			AfterMode:     f.AfterMode,
			Content:       f.Content,
		}
		// v1 create: Content nil and no blob → did not exist.
		if f.Content == nil && f.BlobRef == "" && f.SHA256 == "" {
			rev.Existed = false
		}
		if f.Content != nil {
			rev.Existed = true
			if rev.SHA256 == "" {
				rev.SHA256 = Digest([]byte(*f.Content))
			}
		}
		if f.PayloadExpired {
			rev.BlobRef = ""
			rev.Content = nil
		}
		out = append(out, rev)
	}
	return out
}

// Meta is the picker-facing summary of a checkpoint (no file contents).
type Meta struct {
	Turn               int
	Time               time.Time
	Prompt             string
	Paths              []string
	Coverage           Coverage
	CoverageGaps       []CoverageGap
	ExpiredFilePayload bool
	ActiveWriters      []ActiveWriter
	Legacy             bool
	CanUndoFiles       bool
	DisabledReason     string
}

// Store holds a session's checkpoints in memory and, when dir is set, persists one
// JSON file per turn under it (cheap delete, corruption-isolated). All methods are
// safe for concurrent use — the agent snapshots from tool goroutines.
type Store struct {
	dir  string // <session>.ckpt/, or "" for in-memory only
	root string // workspace root, for restore path-escape guards

	mu   sync.Mutex
	done []*Checkpoint   // finalized turns
	cur  *Checkpoint     // the active turn's checkpoint
	seen map[string]bool // paths already snapshotted this turn (dedup)

	blobs         *BlobStore
	barrier       *MutationBarrier
	activeWriters []ActiveWriter
	plans         map[string]preparedPlan
	lastUndo      *TransactionManifest
	sessionID     string
	mutationSeq   int64
	retainN       int
	blobQuota     int64
	// protectTurns prevents GC of these turn payloads (active tx / last undo).
	protectTurns map[int]bool
}

// New returns a store for the given checkpoint dir and workspace root, loading any
// checkpoints already persisted under dir. A "" dir disables persistence (the
// store still works in memory for the session).
func New(dir, root string) *Store {
	s := &Store{
		dir:          dir,
		root:         root,
		seen:         map[string]bool{},
		barrier:      NewMutationBarrier(),
		plans:        map[string]preparedPlan{},
		retainN:      DefaultRetainCheckpoints,
		blobQuota:    DefaultBlobQuotaBytes,
		protectTurns: map[int]bool{},
	}
	if dir != "" {
		s.blobs = NewBlobStore(filepath.Join(dir, "blobs"))
		s.load()
		s.RecoverTransactions()
	}
	return s
}

// Barrier returns the workspace mutation barrier for this store.
func (s *Store) Barrier() *MutationBarrier {
	if s == nil {
		return nil
	}
	return s.barrier
}

// Blobs returns the content-addressed blob store (may be nil for in-memory).
func (s *Store) Blobs() *BlobStore {
	if s == nil {
		return nil
	}
	return s.blobs
}

// SetSessionID records the owning session id on new checkpoints.
func (s *Store) SetSessionID(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.sessionID = id
	s.mu.Unlock()
}

// SetActiveWriters updates the active writer list mirrored into the current checkpoint.
func (s *Store) SetActiveWriters(writers []ActiveWriter) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeWriters = append([]ActiveWriter(nil), writers...)
	if s.cur != nil {
		s.cur.ActiveWriters = append([]ActiveWriter(nil), writers...)
		s.recomputeCoverageLocked(s.cur)
		s.persistBestEffort(s.cur)
	}
}

func (s *Store) activeWriterConflicts() []RewindConflict {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	conflicts := make([]RewindConflict, 0, len(s.activeWriters))
	for range s.activeWriters {
		conflicts = append(conflicts, RewindConflict{Reason: ConflictBusyWriter})
	}
	return conflicts
}

// LastUndoTransactionID returns the committed transaction id available for undo.
func (s *Store) LastUndoTransactionID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastUndo == nil || s.lastUndo.State != TxCommitted {
		return ""
	}
	return s.lastUndo.ID
}

// InvalidateUndo clears the last undo slot (new turn / new mutation / new rewind).
func (s *Store) InvalidateUndo() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lastUndo = nil
	s.mu.Unlock()
}

func (s *Store) load() {
	seen := map[int]bool{}
	loadDir := func(dir string, expired bool) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			var turnNum int
			if _, err := fmt.Sscanf(e.Name(), "turn-%d.json", &turnNum); err != nil || seen[turnNum] {
				continue
			}
			b, err := fileenc.ReadFileUTF8(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			var c Checkpoint
			if json.Unmarshal(b, &c) != nil {
				continue
			}
			if expired {
				c.ExpiredFilePayload = true
				for i := range c.Files {
					c.Files[i].PayloadExpired = true
					c.Files[i].BlobRef = ""
					c.Files[i].Content = nil
				}
			}
			// Mark v1 as legacy_unverified.
			if c.SchemaVersion == 0 || c.SchemaVersion < SchemaV2 {
				c.SchemaVersion = SchemaV1
				c.Legacy = true
				c.Coverage = CoverageLegacy
				hasLegacyGap := false
				for _, g := range c.CoverageGaps {
					if g.Reason == GapLegacyUnverified {
						hasLegacyGap = true
						break
					}
				}
				if !hasLegacyGap {
					c.CoverageGaps = append(c.CoverageGaps, CoverageGap{Reason: GapLegacyUnverified, Detail: "v1 checkpoint cannot verify later manual edits"})
				}
			}
			seen[turnNum] = true
			s.done = append(s.done, &c)
		}
	}
	// Root turn files remain deliberately readable by previous releases. Expired
	// metadata lives below a directory those releases never scan.
	loadDir(s.dir, false)
	loadDir(s.expiredDir(), true)
	sort.Slice(s.done, func(i, j int) bool { return s.done[i].Turn < s.done[j].Turn })
}

// Begin opens a checkpoint for a new user turn, finalizing the previous one. The
// prompt labels it in the picker; msgIndex is the conversation-rewind boundary.
func (s *Store) Begin(turn int, prompt string, msgIndex int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil {
		s.recomputeCoverageLocked(s.cur)
		s.done = append(s.done, s.cur)
	}
	s.cur = &Checkpoint{
		SchemaVersion: SchemaV2,
		Turn:          turn,
		Time:          time.Now(),
		Prompt:        prompt,
		MsgIndex:      msgIndex,
		SessionID:     s.sessionID,
		Coverage:      CoverageNone,
	}
	s.seen = map[string]bool{}
	s.lastUndo = nil // new turn invalidates undo
	s.persistBestEffort(s.cur)
	s.gcLocked()
}

// Bounds returns turn → MsgIndex over all checkpoints (persisted + current), so
// the controller can rebuild its conversation-rewind boundaries after loading a
// resumed session's checkpoints from disk.
func (s *Store) Bounds() map[int]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := make(map[int]int, len(s.done))
	for _, c := range s.done {
		m[c.Turn] = c.MsgIndex
	}
	if s.cur != nil {
		m[s.cur.Turn] = s.cur.MsgIndex
	}
	return m
}

// Snapshot records the pre-edit state of the file a writer is about to change.
// Only the first touch of a path in the current turn is kept (that is its
// turn-start content). A no-op before the first Begin.
//
// Legacy entry point used by SetPreEditHook; prefer CaptureBefore / MutationObserver.

// CaptureBeforeFromChange records a preimage using a Previewer change when possible.

// Detect encoding from disk for non-UTF8 restore fidelity.

// Capture mode via Lstat; also detect symlink/hardlink gaps.

// Prefer disk bytes when available for exact restore (encoding).

// Keep decoded text content for in-memory FileState/API compat.

// For non-UTF8, Content stays as decoded OldText; bytes live in blob.

// Keep inline content alongside the blob ref so older binaries can still
// distinguish existing files from the nil-content deletion sentinel.

// CaptureBefore records a preimage by Lstat+read of path.

// Decoded text for API compat (FileState / legacy RestoreCode path).

// Content nil + no blob → create (did not exist)

// CaptureAfter records the after fingerprint for a path already in the current
// (or any) checkpoint that owns it.

// Update after fingerprint on the most recent snap of this path.

// mutation invalidates undo

// Path might only appear in earlier turns; still record after on earliest?
// Ownership after is per-path last write — update the latest checkpoint that
// has this path.

// RecordGap appends a coverage gap to the current checkpoint.

// Dedupe identical gaps.

func (s *Store) expiredDir() string {
	return filepath.Join(s.dir, "expired")
}

func (s *Store) checkpointPath(c *Checkpoint) string {
	dir := s.dir
	if c != nil && c.ExpiredFilePayload {
		dir = s.expiredDir()
	}
	return filepath.Join(dir, fmt.Sprintf("turn-%d.json", c.Turn))
}

func (s *Store) persist(c *Checkpoint) error {
	if s.dir == "" || c == nil {
		return nil
	}
	// Keep inline Content even when BlobRef is present. Previous Reasonix builds
	// ignore BlobRef and interpret nil Content as "the file did not exist";
	// omitting it would make an older concurrently running binary delete files.
	wire := *c
	wire.Files = make([]FileSnap, len(c.Files))
	copy(wire.Files, c.Files)
	b, err := json.Marshal(&wire)
	if err != nil {
		return err
	}
	path := s.checkpointPath(c)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := fileutil.AtomicWriteFileStrict(path, b, 0o644); err != nil {
		return err
	}
	return nil
}

func (s *Store) persistBestEffort(c *Checkpoint) {
	if err := s.persist(c); err != nil {
		slog.Warn("checkpoint: persist failed", "turn", c.Turn, "err", err)
	}
}

// gcLocked drops file payloads for old checkpoints beyond retainN / blobQuota.
// Caller holds s.mu.
func (s *Store) gcLocked() {
	if s.blobs == nil || s.retainN <= 0 {
		return
	}
	// Collect recoverable checkpoints (have file payloads) oldest first.
	all := s.all()
	type entry struct {
		c *Checkpoint
	}
	var withFiles []entry
	for _, c := range all {
		if len(c.Files) > 0 {
			withFiles = append(withFiles, entry{c: c})
		}
	}
	// Expire payloads for all but the newest retainN.
	if len(withFiles) > s.retainN {
		expiredAny := false
		for _, e := range withFiles[:len(withFiles)-s.retainN] {
			if s.protectTurns[e.c.Turn] {
				continue
			}
			if err := s.expirePayloadLocked(e.c); err != nil {
				slog.Warn("checkpoint: expire payload failed", "turn", e.c.Turn, "err", err)
				continue
			}
			expiredAny = true
		}
		if expiredAny {
			s.pruneBlobsLocked()
		}
	}
	// Blob quota.
	size, err := s.blobs.Size()
	if err != nil || size <= s.blobQuota {
		return
	}
	for _, e := range withFiles {
		if size <= s.blobQuota {
			break
		}
		if s.protectTurns[e.c.Turn] || e.c.ExpiredFilePayload {
			continue
		}
		// Rough: expire and recompute size.
		if err := s.expirePayloadLocked(e.c); err != nil {
			slog.Warn("checkpoint: expire payload failed", "turn", e.c.Turn, "err", err)
			continue
		}
		s.pruneBlobsLocked()
		size, _ = s.blobs.Size()
	}
}

// pruneBlobsLocked performs mark-and-sweep after checkpoint metadata has been
// persisted. Transaction manifests and the current undo slot also keep their
// forward/restore payloads live. Caller holds s.mu.
func (s *Store) pruneBlobsLocked() {
	if s.blobs == nil {
		return
	}
	live := map[string]struct{}{}
	mark := func(ref string) {
		if validBlobRef(ref) {
			live[ref] = struct{}{}
		}
	}
	for _, c := range s.all() {
		for _, f := range c.Files {
			mark(f.BlobRef)
		}
	}
	if s.lastUndo != nil {
		for _, target := range s.lastUndo.Targets {
			mark(target.RestoreBlob)
			mark(target.ForwardBlob)
		}
	}
	if s.dir != "" {
		entries, _ := os.ReadDir(s.txDir())
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			var tx TransactionManifest
			if readJSONFile(filepath.Join(s.txDir(), entry.Name()), &tx) != nil {
				continue
			}
			for _, target := range tx.Targets {
				mark(target.RestoreBlob)
				mark(target.ForwardBlob)
			}
		}
	}
	if err := s.blobs.Prune(live); err != nil {
		slog.Warn("checkpoint: prune blobs", "err", err)
	}
}

func (s *Store) expirePayloadLocked(c *Checkpoint) error {
	if c == nil || c.ExpiredFilePayload {
		return nil
	}
	expired := *c
	expired.Files = append([]FileSnap(nil), c.Files...)
	expired.CoverageGaps = append([]CoverageGap(nil), c.CoverageGaps...)
	for i := range expired.Files {
		expired.Files[i].BlobRef = ""
		expired.Files[i].Content = nil
		expired.Files[i].PayloadExpired = true
	}
	expired.ExpiredFilePayload = true
	expired.Coverage = CoveragePartial
	expired.CoverageGaps = append(expired.CoverageGaps, CoverageGap{Reason: GapExpiredPayload, Detail: "file recovery payload expired"})
	if err := s.persist(&expired); err != nil {
		return err
	}
	if s.dir != "" {
		legacyVisible := filepath.Join(s.dir, fmt.Sprintf("turn-%d.json", c.Turn))
		if err := os.Remove(legacyVisible); err != nil && !os.IsNotExist(err) {
			_ = os.Remove(s.checkpointPath(&expired))
			return err
		}
	}
	*c = expired
	return nil
}

// NextTurn returns the turn number a new checkpoint should take: one past the
// highest existing turn (0 when empty), so a resumed session keeps numbering
// without colliding with checkpoints loaded from disk.

// List returns every checkpoint's metadata, oldest turn first.

// FileState returns the earliest pre-edit state recorded for p across the
// session. Paths are compared after resolving them against the workspace root,
// because older checkpoints may contain absolute paths while newer writers use
// workspace-relative paths.

// Ownership belongs to the final observed mutation, while the restore
// payload remains the earliest preimage. A later capture without an
// after fingerprint deliberately clears an older ownership proof.

// all returns done + cur in turn order. Caller holds the lock.

// TruncateFrom discards checkpoints at or after fromTurn. Conversation rewind
// removes those future turns from the transcript, so their file snapshots must
// not remain visible or collide with newly-created checkpoints that reuse the
// same turn numbers after the rewrite.

// RestoreCode reverts the workspace to its state at the start of turn `fromTurn`
// using a transactional prepare+commit. Legacy checkpoints are refused because
// they cannot prove that a later manual edit is safe to overwrite. Returns the
// paths written and deleted.
//
// On any failure after partial publish, compensation restores the pre-rewind
// workspace. Unlike the pre-v2 loop, a mid-way error does not leave a half-applied
// restore.

// When complete/partial with no conflicts, commit.

// No files — success no-op.

// safePath resolves p against root and rejects anything escaping it — restore
// must never write outside the workspace, even if a snapshot path is hostile or
// the project moved since it was taken.
