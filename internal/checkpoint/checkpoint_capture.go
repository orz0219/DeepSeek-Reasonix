package checkpoint

import (
	"slices"

	"reasonix/internal/diff"
	fileenc "reasonix/internal/fileutil/encoding"
)

// Snapshot records the pre-edit state of the file a writer is about to change.
// Only the first touch of a path in the current turn is kept (that is its
// turn-start content). A no-op before the first Begin.
//
// Legacy entry point used by SetPreEditHook; prefer CaptureBefore / MutationObserver.
func (s *Store) Snapshot(ch diff.Change) {
	s.CaptureBeforeFromChange(ch, CaptureBeforeOpts{Source: CapturePreviewer})
}

// CaptureBeforeFromChange records a preimage using a Previewer change when possible.
func (s *Store) CaptureBeforeFromChange(ch diff.Change, opts CaptureBeforeOpts) {
	if ch.Path == "" {
		return
	}
	pathKey := NormalizeRelPath(s.root, ch.Path)
	if opts.Source == "" {
		opts.Source = CapturePreviewer
	}

	var enc *fileenc.Kind
	var mode uint32
	var sha string
	var blobRef string
	var content *string

	if ch.Kind != diff.Create {
		old := ch.OldText
		content = &old
		sha = Digest([]byte(old))

		enc = s.detectEncoding(ch.Path)

		fp, gap, err := CapturePath(ch.Path, CaptureOptions{
			WorkspaceRoot: s.root,
			ReadContent:   false,
		})
		if gap != nil {
			s.RecordGap(*gap)
		}
		if err == nil {
			mode = fp.Mode
		}

		// Use the preview's OldText for the blob instead of re-reading from
		// disk. The preview already read the file to compute the diff, so
		// OldText is identical to what CapturePath would read. This avoids a
		// redundant file read on every mutation.
		if s.blobs != nil && len(old) > 0 {
			if ref, perr := s.blobs.Put([]byte(old)); perr == nil {
				blobRef = ref
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || s.seen[pathKey] {
		return
	}
	s.seen[pathKey] = true
	if s.blobs != nil && content != nil && blobRef == "" {
		if ref, err := s.blobs.Put([]byte(*content)); err == nil {
			blobRef = ref
		}
	}
	snap := FileSnap{
		Path:          ch.Path,
		Content:       content,
		Encoding:      enc,
		Mode:          mode,
		SHA256:        sha,
		BlobRef:       blobRef,
		CaptureSource: opts.Source,
	}

	s.cur.Files = append(s.cur.Files, snap)
	s.cur.SchemaVersion = SchemaV2
	s.recomputeCoverageLocked(s.cur)
	s.persistBestEffort(s.cur)
}

// CaptureBefore records a preimage by Lstat+read of path.
func (s *Store) CaptureBefore(path string, opts CaptureBeforeOpts) {
	if path == "" {
		return
	}
	pathKey := NormalizeRelPath(s.root, path)
	if opts.Source == "" {
		opts.Source = CaptureBeforeMutation
	}
	fp, gap, _ := CapturePath(path, CaptureOptions{
		WorkspaceRoot: s.root,
		ReadContent:   true,
	})
	if gap != nil {
		s.RecordGap(*gap)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil || s.seen[pathKey] {
		return
	}
	s.seen[pathKey] = true
	snap := FileSnap{
		Path:          path,
		CaptureSource: opts.Source,
	}
	if fp.Existed {
		snap.Mode = fp.Mode
		snap.SHA256 = fp.SHA256
		if s.blobs != nil && len(fp.Content) > 0 {
			if ref, err := s.blobs.Put(fp.Content); err == nil {
				snap.BlobRef = ref
			}
		}

		enc, raw := fileenc.Detect(fp.Content)
		text := string(fileenc.Decode(raw, enc))
		snap.Content = &text
		snap.Encoding = &enc
		if snap.SHA256 == "" {
			snap.SHA256 = Digest(fp.Content)
		}
	}

	s.cur.Files = append(s.cur.Files, snap)
	s.cur.SchemaVersion = SchemaV2
	s.recomputeCoverageLocked(s.cur)
	s.persistBestEffort(s.cur)
}

// CaptureAfter records the after fingerprint for a path already in the current
// (or any) checkpoint that owns it.
func (s *Store) CaptureAfter(path string, opts CaptureAfterOpts) {
	if path == "" {
		return
	}
	pathKey := NormalizeRelPath(s.root, path)
	fp, gap, err := CapturePath(path, CaptureOptions{
		WorkspaceRoot: s.root,
		ReadContent:   true,
	})
	if gap != nil {
		s.RecordGap(*gap)
	}
	_ = err

	s.mu.Lock()
	defer s.mu.Unlock()
	s.mutationSeq = opts.Seq
	if s.cur != nil {
		s.cur.LastMutationSeq = opts.Seq
	}

	updated := false
	if s.cur != nil {
		for i := range s.cur.Files {
			if NormalizeRelPath(s.root, s.cur.Files[i].Path) != pathKey {
				continue
			}
			existed := fp.Existed
			s.cur.Files[i].AfterExisted = &existed
			s.cur.Files[i].AfterSHA256 = fp.SHA256
			s.cur.Files[i].AfterMode = fp.Mode
			updated = true
		}
		if updated {
			s.recomputeCoverageLocked(s.cur)
			s.persistBestEffort(s.cur)
			s.lastUndo = nil
			return
		}
	}

	for _, v := range slices.Backward(s.done) {
		c := v
		for j := range c.Files {
			if NormalizeRelPath(s.root, c.Files[j].Path) != pathKey {
				continue
			}
			existed := fp.Existed
			c.Files[j].AfterExisted = &existed
			c.Files[j].AfterSHA256 = fp.SHA256
			c.Files[j].AfterMode = fp.Mode
			s.persistBestEffort(c)
			s.lastUndo = nil
			return
		}
	}
}

// RecordGap appends a coverage gap to the current checkpoint.
func (s *Store) RecordGap(gap CoverageGap) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return
	}

	for _, g := range s.cur.CoverageGaps {
		if g.Reason == gap.Reason && g.Detail == gap.Detail && g.Tool == gap.Tool && g.Path == gap.Path {
			return
		}
	}
	s.cur.CoverageGaps = append(s.cur.CoverageGaps, gap)
	s.recomputeCoverageLocked(s.cur)
	s.persistBestEffort(s.cur)
}

func (s *Store) recomputeCoverageLocked(c *Checkpoint) {
	if c == nil {
		return
	}
	if c.Legacy || c.SchemaVersion < SchemaV2 {
		c.Coverage = CoverageLegacy
		return
	}
	if c.ExpiredFilePayload {
		c.Coverage = CoveragePartial
		return
	}
	hasFiles := len(c.Files) > 0
	hasGaps := len(c.CoverageGaps) > 0
	switch {
	case !hasFiles && !hasGaps:
		c.Coverage = CoverageNone
	case !hasFiles && hasGaps:
		c.Coverage = CoverageNone
	case hasFiles && hasGaps:
		c.Coverage = CoveragePartial
	default:
		c.Coverage = CoverageComplete
	}
}

func (s *Store) detectEncoding(p string) *fileenc.Kind {
	abs, err := safePath(s.root, p)
	if err != nil {
		return nil
	}
	b, err := secureReadFile(s.root, abs)
	if err != nil {
		return nil
	}
	enc, _ := fileenc.Detect(b)
	return &enc
}
