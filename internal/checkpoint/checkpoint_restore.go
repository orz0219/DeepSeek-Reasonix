package checkpoint

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	fileenc "reasonix/internal/fileutil/encoding"
)

// FileState is the earliest pre-edit state recorded for a file in this
// session. Content == nil means the file did not exist before the session's
// first tracked edit.
type FileState struct {
	Content  *string
	Encoding *fileenc.Kind
	Mode     uint32
	SHA256   string
	BlobRef  string
	Owned    bool // true when session has after-fingerprint ownership
}

// NextTurn returns the turn number a new checkpoint should take: one past the
// highest existing turn (0 when empty), so a resumed session keeps numbering
// without colliding with checkpoints loaded from disk.
func (s *Store) NextTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := 0
	for _, c := range s.done {
		if c.Turn >= next {
			next = c.Turn + 1
		}
	}
	if s.cur != nil && s.cur.Turn >= next {
		next = s.cur.Turn + 1
	}
	return next
}

// List returns every checkpoint's metadata, oldest turn first.
func (s *Store) List() []Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Meta, 0, len(s.done)+1)
	for _, c := range s.all() {
		paths := make([]string, len(c.Files))
		for i, f := range c.Files {
			paths[i] = f.Path
		}
		meta := Meta{
			Turn:               c.Turn,
			Time:               c.Time,
			Prompt:             c.Prompt,
			Paths:              paths,
			Coverage:           c.Coverage,
			CoverageGaps:       append([]CoverageGap(nil), c.CoverageGaps...),
			ExpiredFilePayload: c.ExpiredFilePayload,
			ActiveWriters:      append([]ActiveWriter(nil), c.ActiveWriters...),
			Legacy:             c.Legacy || c.Coverage == CoverageLegacy,
		}
		switch {
		case meta.Legacy:
			meta.CanUndoFiles = false
			meta.DisabledReason = "legacy checkpoint cannot verify later manual edits"
		case meta.ExpiredFilePayload:
			meta.CanUndoFiles = false
			meta.DisabledReason = "file recovery payload expired"
		case meta.Coverage == CoverageNone:
			meta.CanUndoFiles = false
		case meta.Coverage == CoveragePartial:
			meta.CanUndoFiles = len(paths) > 0
		default:
			meta.CanUndoFiles = len(paths) > 0
		}
		out = append(out, meta)
	}
	return out
}

// FileState returns the earliest pre-edit state recorded for p across the
// session. Paths are compared after resolving them against the workspace root,
// because older checkpoints may contain absolute paths while newer writers use
// workspace-relative paths.
func (s *Store) FileState(p string) (FileState, bool) {
	want, err := safePath(s.root, p)
	if err != nil {
		return FileState{}, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var earliest *FileSnap
	var latestAfterSHA string
	var latestAfterExisted *bool
	for _, c := range s.all() {
		for _, f := range c.Files {
			got, err := safePath(s.root, f.Path)
			if err != nil || got != want {
				continue
			}
			if earliest == nil {
				copy := f
				earliest = &copy
			}

			latestAfterSHA = f.AfterSHA256
			latestAfterExisted = f.AfterExisted
		}
	}
	if earliest == nil || earliest.PayloadExpired {
		return FileState{}, false
	}
	state := FileState{
		Encoding: earliest.Encoding,
		Mode:     earliest.Mode,
		SHA256:   earliest.SHA256,
		BlobRef:  earliest.BlobRef,
		Owned:    latestAfterSHA != "" || latestAfterExisted != nil,
	}
	if earliest.Content != nil {
		content := *earliest.Content
		state.Content = &content
	} else if earliest.BlobRef != "" && s.blobs != nil {
		if raw, err := s.blobs.Get(earliest.BlobRef); err == nil {
			enc, payload := fileenc.Detect(raw)
			text := string(fileenc.Decode(payload, enc))
			state.Content = &text
			state.Encoding = &enc
		}
	}
	return state, true
}

// all returns done + cur in turn order. Caller holds the lock.
func (s *Store) all() []*Checkpoint {
	cps := append([]*Checkpoint(nil), s.done...)
	if s.cur != nil {
		cps = append(cps, s.cur)
	}
	sort.Slice(cps, func(i, j int) bool { return cps[i].Turn < cps[j].Turn })
	return cps
}

// TruncateFrom discards checkpoints at or after fromTurn. Conversation rewind
// removes those future turns from the transcript, so their file snapshots must
// not remain visible or collide with newly-created checkpoints that reuse the
// same turn numbers after the rewrite.
func (s *Store) TruncateFrom(fromTurn int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleteTurns := map[int]bool{}
	for _, c := range s.done {
		if c.Turn >= fromTurn {
			deleteTurns[c.Turn] = true
		}
	}
	if s.cur != nil && s.cur.Turn >= fromTurn {
		deleteTurns[s.cur.Turn] = true
	}
	if s.dir != "" {
		for turn := range deleteTurns {
			paths := []string{
				filepath.Join(s.dir, fmt.Sprintf("turn-%d.json", turn)),
				filepath.Join(s.expiredDir(), fmt.Sprintf("turn-%d.json", turn)),
			}
			for _, path := range paths {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove checkpoint turn %d: %w", turn, err)
				}
			}
		}
	}

	done := s.done[:0]
	for _, c := range s.done {
		if c.Turn >= fromTurn {
			continue
		}
		done = append(done, c)
	}
	for i := len(done); i < len(s.done); i++ {
		s.done[i] = nil
	}
	s.done = done
	if s.cur != nil && s.cur.Turn >= fromTurn {
		s.cur = nil
		s.seen = map[string]bool{}
	}
	return nil
}

// RestoreCode reverts the workspace to its state at the start of turn `fromTurn`
// using a transactional prepare+commit. Legacy checkpoints are refused because
// they cannot prove that a later manual edit is safe to overwrite. Returns the
// paths written and deleted.
//
// On any failure after partial publish, compensation restores the pre-rewind
// workspace. Unlike the pre-v2 loop, a mid-way error does not leave a half-applied
// restore.
func (s *Store) RestoreCode(fromTurn int) (written, deleted []string, err error) {
	plan, err := s.PrepareRewind(fromTurn, RewindCode, 0, 0, false)
	if err != nil {
		return nil, nil, err
	}
	if plan.Legacy && len(plan.Files) > 0 {
		return nil, nil, fmt.Errorf("legacy checkpoint cannot safely restore files without explicit conflict confirmation")
	}

	if !plan.CanFiles && !plan.Legacy {
		if plan.DisabledReason != "" {
			return nil, nil, fmt.Errorf("%s", plan.DisabledReason)
		}
		if len(plan.Conflicts) > 0 {
			return nil, nil, fmt.Errorf("file conflicts detected")
		}

		return nil, nil, nil
	}
	result, err := s.CommitRewindWithForward(plan.PlanID, nil, nil, nil)
	if err != nil {
		return result.Written, result.Deleted, err
	}
	return result.Written, result.Deleted, nil
}

func (s *Store) detectCurrentEncoding(path string) *fileenc.Kind {
	b, err := secureReadFile(s.root, path)
	if err != nil {
		return nil
	}
	enc, _ := fileenc.Detect(b)
	return &enc
}

// safePath resolves p against root and rejects anything escaping it — restore
// must never write outside the workspace, even if a snapshot path is hostile or
// the project moved since it was taken.
func safePath(root, p string) (string, error) {
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, p)
	}
	abs = filepath.Clean(abs)
	if root != "" {
		if err := validateWorkspacePath(root, abs); err != nil {
			return "", err
		}
	}
	return abs, nil
}

var errSymlinkPath = errors.New("workspace path contains symbolic link")

func workspaceRelative(root, abs string) (string, error) {
	if root == "" {
		return filepath.Clean(abs), nil
	}
	r := filepath.Clean(root)
	rel, err := filepath.Rel(r, filepath.Clean(abs))
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("checkpoint path %q escapes workspace %q", abs, root)
	}
	return rel, nil
}

func splitLocalPath(rel string) []string {
	var parts []string
	for rel != "." && rel != "" {
		dir, base := filepath.Split(rel)
		if base != "" {
			parts = append([]string{base}, parts...)
		}
		rel = filepath.Clean(dir)
		if rel == string(filepath.Separator) {
			break
		}
	}
	return parts
}

func validateWorkspacePath(root, abs string) error {
	rel, err := workspaceRelative(root, abs)
	if err != nil {
		return err
	}
	cur := filepath.Clean(root)
	for _, part := range splitLocalPath(rel) {
		cur = filepath.Join(cur, part)
		info, statErr := os.Lstat(cur)
		if os.IsNotExist(statErr) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", errSymlinkPath, cur)
		}
	}
	return nil
}

func writeNewFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	remove = false
	return nil
}
