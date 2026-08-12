package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	fileenc "reasonix/internal/fileutil/encoding"
)

func (s *Store) precheckFiles(fromTurn int) []RewindConflict {
	earliest := s.earliestRevisions(fromTurn)
	var conflicts []RewindConflict
	for p, rev := range earliest {
		abs, err := safePath(s.root, p)
		if err != nil {
			conflicts = append(conflicts, RewindConflict{Path: p, Reason: ConflictPathUnsafe})
			continue
		}
		if rev.BlobRef == "" && rev.Content == nil && rev.Existed {
			if rev.SHA256 != "" && s.blobs != nil && !s.blobs.Has(rev.SHA256) && (rev.BlobRef == "" || !s.blobs.Has(rev.BlobRef)) {
				conflicts = append(conflicts, RewindConflict{Path: p, Reason: ConflictMissingPayload, CheckpointSHA: rev.SHA256})
				continue
			}
			if rev.Content == nil && rev.BlobRef == "" {
				conflicts = append(conflicts, RewindConflict{Path: p, Reason: ConflictMissingPayload})
				continue
			}
		}
		fp, err := FingerprintPath(s.root, abs)
		if err != nil {

			conflicts = append(conflicts, RewindConflict{Path: p, Reason: ConflictExternalChange, CheckpointSHA: rev.SHA256})
			continue
		}

		reason := CompareIdentity(fp, rev.AfterSHA256, rev.AfterExisted, rev.AfterMode)
		if reason == ConflictCoverageLegacy {

			continue
		}
		if reason != "" {
			conflicts = append(conflicts, RewindConflict{
				Path:            p,
				Reason:          reason,
				CheckpointSHA:   rev.SHA256,
				LastOwnedSHA:    rev.AfterSHA256,
				CurrentSHA:      fp.SHA256,
				CheckpointMode:  rev.Mode,
				CurrentMode:     fp.Mode,
				CurrentExisted:  fp.Existed,
				CheckpointExist: rev.Existed,
			})
		}
	}
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	return conflicts
}

func (s *Store) earliestRevisions(fromTurn int) map[string]FileRevision {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.earliestRevisionsLocked(fromTurn)
}

func (s *Store) earliestRevisionsLocked(fromTurn int) map[string]FileRevision {
	earliest := map[string]FileRevision{}
	for _, c := range s.all() {
		if c.Turn < fromTurn {
			continue
		}
		for _, rev := range c.revisions() {
			pathKey := NormalizeRelPath(s.root, rev.Path)
			if first, ok := earliest[pathKey]; ok {

				first.AfterExisted = rev.AfterExisted
				first.AfterSHA256 = rev.AfterSHA256
				first.AfterMode = rev.AfterMode
				earliest[pathKey] = first
				continue
			}
			rev.Path = pathKey
			earliest[pathKey] = rev
		}
	}
	return earliest
}

func (s *Store) filesFromTurnLocked(fromTurn int) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range s.all() {
		if c.Turn < fromTurn {
			continue
		}
		for _, rev := range c.revisions() {
			pathKey := NormalizeRelPath(s.root, rev.Path)
			if seen[pathKey] {
				continue
			}
			seen[pathKey] = true
			out = append(out, pathKey)
		}
	}
	sort.Strings(out)
	return out
}

func (s *Store) coverageFromTurnLocked(fromTurn int) (Coverage, []CoverageGap, bool, bool) {
	var gaps []CoverageGap
	legacy := false
	expired := false
	hasFiles := false
	partial := false
	for _, c := range s.all() {
		if c.Turn < fromTurn {
			continue
		}
		if c.SchemaVersion < SchemaV2 && c.SchemaVersion != 0 {
			legacy = true
		}
		if c.SchemaVersion == 0 {

			legacy = true
		}
		if c.Coverage == CoverageLegacy || c.Legacy {
			legacy = true
		}
		if c.ExpiredFilePayload {
			expired = true
		}
		if c.Coverage == CoveragePartial {
			partial = true
		}
		gaps = append(gaps, c.CoverageGaps...)
		if len(c.revisions()) > 0 {
			hasFiles = true
		}
	}
	if legacy {
		return CoverageLegacy, append(gaps, CoverageGap{Reason: GapLegacyUnverified}), true, expired
	}
	if expired {
		return CoveragePartial, append(gaps, CoverageGap{Reason: GapExpiredPayload}), false, true
	}
	if !hasFiles {
		if len(gaps) > 0 {
			return CoverageNone, gaps, false, false
		}
		return CoverageNone, nil, false, false
	}
	if partial || len(gaps) > 0 {
		return CoveragePartial, gaps, false, false
	}
	return CoverageComplete, nil, false, false
}

func (s *Store) loadRevisionBytes(rev FileRevision) ([]byte, error) {
	if rev.BlobRef != "" && s.blobs != nil {
		return s.blobs.Get(rev.BlobRef)
	}
	if rev.Content != nil {
		return []byte(*rev.Content), nil
	}
	if rev.SHA256 != "" && s.blobs != nil && s.blobs.Has(rev.SHA256) {
		return s.blobs.Get(rev.SHA256)
	}
	return nil, fmt.Errorf("missing payload for %s", rev.Path)
}

func (s *Store) loadBlobOrInline(ref string, inline []byte) ([]byte, error) {
	if ref != "" && s.blobs != nil {
		return s.blobs.Get(ref)
	}
	if inline != nil {
		return inline, nil
	}
	return nil, fmt.Errorf("missing blob %q", ref)
}

func (s *Store) backupCheckpointsFrom(fromTurn int) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var future []*Checkpoint
	for _, c := range s.all() {
		if c.Turn >= fromTurn {
			cp := *c
			future = append(future, &cp)
		}
	}
	return json.Marshal(future)
}

func (s *Store) restoreCheckpointBackup(backup []byte) error {
	var future []*Checkpoint
	if err := json.Unmarshal(backup, &future); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	byTurn := map[int]*Checkpoint{}
	for _, c := range s.done {
		byTurn[c.Turn] = c
	}
	if s.cur != nil {
		byTurn[s.cur.Turn] = s.cur
	}
	for _, c := range future {
		byTurn[c.Turn] = c
		if err := s.persist(c); err != nil {
			return fmt.Errorf("persist restored checkpoint turn %d: %w", c.Turn, err)
		}
		counterpart := filepath.Join(s.expiredDir(), fmt.Sprintf("turn-%d.json", c.Turn))
		if c.ExpiredFilePayload {
			counterpart = filepath.Join(s.dir, fmt.Sprintf("turn-%d.json", c.Turn))
		}
		if err := os.Remove(counterpart); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale checkpoint counterpart turn %d: %w", c.Turn, err)
		}
	}

	turns := make([]int, 0, len(byTurn))
	for t := range byTurn {
		turns = append(turns, t)
	}
	sort.Ints(turns)
	s.done = nil
	s.cur = nil
	for _, t := range turns {
		s.done = append(s.done, byTurn[t])
	}
	return nil
}

func newID(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), Digest(fmt.Appendf(nil, "%d", time.Now().UnixNano()))[:8])
}

// RestoreCheckpointBackupPublic reloads backed-up checkpoints after an undo.
func (s *Store) RestoreCheckpointBackupPublic(backup []byte) error {
	return s.restoreCheckpointBackup(backup)
}

// PrepareFileRevert prepares a single-file restore to the earliest session preimage.
func (s *Store) PrepareFileRevert(path string, sessionRev int64) (RewindPlan, error) {
	if s == nil {
		return RewindPlan{}, fmt.Errorf("checkpoints unavailable")
	}
	plan := RewindPlan{
		PlanID:          newID("plan"),
		Scope:           RewindCode,
		Path:            path,
		SessionRevision: sessionRev,
		CreatedAt:       time.Now(),
		WorkspaceToken:  fmt.Sprintf("%d", s.barrier.Generation()),
		Files:           []string{path},
		FileCount:       1,
	}
	state, ok := s.FileState(path)
	if !ok {
		plan.CanFiles = false
		plan.DisabledReason = "file is not session-owned"
		return plan, nil
	}
	_ = state
	abs, err := safePath(s.root, path)
	if err != nil {
		plan.CanFiles = false
		plan.DisabledReason = "path unsafe"
		plan.Conflicts = []RewindConflict{{Path: path, Reason: ConflictPathUnsafe}}
		return plan, nil
	}
	revs := s.earliestRevisions(0)
	rev, has := revs[path]
	if !has {
		for p, r := range revs {
			if ap, e := safePath(s.root, p); e == nil && ap == abs {
				rev, has = r, true
				plan.Path = p
				break
			}
		}
	}
	if !has {
		plan.CanFiles = false
		plan.DisabledReason = "file is not session-owned"
		return plan, nil
	}
	if rev.AfterExisted == nil && rev.AfterSHA256 == "" {

		plan.PlanID = ""
		plan.CanFiles = false
		plan.Legacy = true
		plan.Coverage = CoverageLegacy
		plan.DisabledReason = "legacy checkpoint cannot verify later manual edits"
		return plan, nil
	}
	fp, fperr := FingerprintPath(s.root, abs)
	if fperr == nil {
		reason := CompareIdentity(fp, rev.AfterSHA256, rev.AfterExisted, rev.AfterMode)
		if reason != "" {
			plan.Conflicts = []RewindConflict{{
				Path: path, Reason: reason,
				CheckpointSHA: rev.SHA256, LastOwnedSHA: rev.AfterSHA256, CurrentSHA: fp.SHA256,
				CurrentExisted: fp.Existed, CheckpointExist: rev.Existed,
			}}
			plan.CanFiles = true
			plan.DisabledReason = "conflict requires explicit resolution"
		} else {
			plan.CanFiles = true
		}
	} else {
		plan.CanFiles = false
		plan.DisabledReason = "current file identity unavailable"
		plan.Conflicts = []RewindConflict{{Path: path, Reason: ConflictExternalChange}}
	}
	if rev.BlobRef == "" && rev.Content == nil && rev.Existed {
		plan.CanFiles = false
		plan.DisabledReason = "missing file payload"
		plan.Conflicts = append(plan.Conflicts, RewindConflict{Path: path, Reason: ConflictMissingPayload})
	}
	s.mu.Lock()
	if s.plans == nil {
		s.plans = map[string]preparedPlan{}
	}
	s.plans[plan.PlanID] = preparedPlan{plan: plan, created: time.Now(), previewFingerprint: &fp}
	s.mu.Unlock()
	return plan, nil
}

// CommitFileRevert commits a single-file restore.
func (s *Store) CommitFileRevert(planID string, resolution ConflictResolution) (RewindResult, error) {
	if s == nil {
		return RewindResult{}, fmt.Errorf("checkpoints unavailable")
	}
	s.mu.Lock()
	pp, ok := s.plans[planID]
	if ok {
		delete(s.plans, planID)
	}
	s.mu.Unlock()
	if !ok {
		return RewindResult{OK: false, Error: "unknown or expired plan"}, fmt.Errorf("unknown or expired plan")
	}
	plan := pp.plan
	if plan.Path == "" {
		return RewindResult{OK: false, Error: "not a file plan"}, fmt.Errorf("not a file plan")
	}
	if !plan.CanFiles {
		return RewindResult{OK: false, Error: plan.DisabledReason, Conflicts: plan.Conflicts}, fmt.Errorf("%s", plan.DisabledReason)
	}
	if len(plan.Conflicts) > 0 && resolution != ResolveOverwriteCheckpoint {
		if resolution == ResolveKeepCurrent {
			return RewindResult{OK: true, UndoAvailable: false}, nil
		}
		return RewindResult{OK: false, Error: "conflict requires explicit resolution", Conflicts: plan.Conflicts}, fmt.Errorf("conflict requires explicit resolution")
	}
	if !s.barrier.TryEnterExclusive() {
		err := fmt.Errorf("workspace mutation in progress")
		return RewindResult{OK: false, Error: err.Error(), Conflicts: []RewindConflict{{Path: plan.Path, Reason: ConflictBusyWriter}}}, err
	}
	defer s.barrier.ExitExclusive()
	if conflicts := s.activeWriterConflicts(); len(conflicts) > 0 {
		err := fmt.Errorf("active background writer")
		return RewindResult{OK: false, Error: err.Error(), Conflicts: conflicts}, err
	}
	if plan.WorkspaceToken != fmt.Sprintf("%d", s.barrier.Generation()) {
		conflict := RewindConflict{Path: plan.Path, Reason: ConflictStalePlan}
		return RewindResult{OK: false, Error: "workspace changed since preview", Conflicts: []RewindConflict{conflict}}, fmt.Errorf("workspace changed since preview")
	}
	absPreview, err := safePath(s.root, plan.Path)
	if err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	current, err := FingerprintPath(s.root, absPreview)
	if err != nil || pp.previewFingerprint == nil || !sameFingerprint(current, *pp.previewFingerprint) {
		conflict := RewindConflict{Path: plan.Path, Reason: ConflictStalePlan, CurrentSHA: current.SHA256, CurrentExisted: current.Existed}
		return RewindResult{OK: false, Error: "file changed since preview; preview again", Conflicts: []RewindConflict{conflict}}, fmt.Errorf("file changed since preview; preview again")
	}

	revs := s.earliestRevisions(0)
	rev, has := revs[plan.Path]
	if !has {
		abs, _ := safePath(s.root, plan.Path)
		for p, r := range revs {
			if ap, e := safePath(s.root, p); e == nil && ap == abs {
				rev, has = r, true
				plan.Path = p
				break
			}
		}
	}
	if !has {
		return RewindResult{OK: false, Error: "file is not session-owned"}, fmt.Errorf("file is not session-owned")
	}

	abs, err := safePath(s.root, rev.Path)
	if err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	fwd, _, _ := CapturePath(abs, CaptureOptions{WorkspaceRoot: s.root, ReadContent: true})
	tx := &TransactionManifest{
		SchemaVersion: SchemaV2,
		ID:            newID("tx"),
		WorkspaceRoot: s.root,
		State:         TxPrepared,
		Kind:          "file_revert",
		Scope:         RewindCode,
		Path:          rev.Path,
		CreatedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	t := TransactionTarget{
		Path: rev.Path, AbsPath: abs,
		RestoreExisted: rev.Existed, RestoreMode: rev.Mode, RestoreSHA: rev.SHA256, RestoreBlob: rev.BlobRef, RestoreEncoding: rev.Encoding,
		ForwardExisted: fwd.Existed, ForwardMode: fwd.Mode, ForwardSHA: fwd.SHA256,
	}
	t.PublishTmp, t.BackupPath = transactionSiblingPaths(abs, tx.ID, 0)
	if rev.Existed {
		t.Action = "write"
		data, lerr := s.loadRevisionBytes(rev)
		if lerr != nil && rev.Content != nil {
			enc := fileenc.UTF8
			if rev.Encoding != nil {
				enc = *rev.Encoding
			} else if current := s.detectCurrentEncoding(abs); current != nil {
				enc = *current
			}
			data = fileenc.Encode(*rev.Content, enc)
			lerr = nil
		}
		if lerr != nil {
			return RewindResult{OK: false, Error: lerr.Error()}, lerr
		}
		mode := os.FileMode(0o644)
		if rev.Mode != 0 {
			mode = os.FileMode(rev.Mode)
		}
		if t.RestoreBlob == "" && s.blobs != nil {
			ref, perr := s.blobs.Put(data)
			if perr != nil {
				return RewindResult{OK: false, Error: perr.Error()}, perr
			}
			t.RestoreBlob = ref
		} else if t.RestoreBlob == "" {
			t.RestoreInline = clonePayload(data)
		}
		if err := s.writePublishTemp(t.PublishTmp, data, mode); err != nil {
			return RewindResult{OK: false, Error: err.Error()}, err
		}
	} else {
		t.Action = "delete"
		t.PublishTmp = ""
	}
	if fwd.Existed && s.blobs != nil {
		ref, perr := s.blobs.Put(fwd.Content)
		if perr != nil {
			return RewindResult{OK: false, Error: perr.Error()}, perr
		}
		t.ForwardBlob = ref
	} else if fwd.Existed {
		t.ForwardInline = clonePayload(fwd.Content)
	}
	tx.Targets = []TransactionTarget{t}
	if err := s.persistTransaction(tx); err != nil {
		return RewindResult{OK: false, Error: err.Error()}, err
	}
	return s.commitTransaction(tx, nil, nil)
}

func sameFingerprint(a, b Fingerprint) bool {
	return a.Existed == b.Existed && a.IsDir == b.IsDir && a.IsSymlink == b.IsSymlink &&
		a.Nlink == b.Nlink && a.Mode == b.Mode && a.Size == b.Size && a.SHA256 == b.SHA256
}

func clonePayload(data []byte) []byte {
	out := make([]byte, len(data))
	copy(out, data)
	return out
}
