package jobs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"reasonix/internal/event"
)

func (m *Manager) recordArtifactMigrationError(parentSession string, err error) {
	text := "job artifact migration failed: " + err.Error()
	m.mu.Lock()
	for _, j := range m.jobs {
		if j == nil || !sessionMatches(parentSession, j.SessionID) {
			continue
		}
		j.mu.Lock()
		if j.artifactErr == "" {
			j.artifactErr = "migration: " + err.Error()
			j.artifactComplete = false
		}
		j.mu.Unlock()
	}
	active := m.active
	m.mu.Unlock()
	if active == "" || active == parentSession {
		m.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: "Job artifact migration failed.", Detail: text})
	}
}

type artifactMigrationJob struct {
	job     *Job
	wasOpen bool
}

func (m *Manager) migrateArtifactDirForSession(parentSession, oldDir, newDir string) error {
	locked := m.lockArtifactJobsForMigration(parentSession, oldDir)
	defer unlockArtifactMigrationJobs(locked)
	skip := openArtifactMigrationFiles(locked)
	migrateErr := migrateArtifactDirSkipping(oldDir, newDir, skip)
	if migrateErr == nil {
		rebaseArtifactMigrationJobs(locked, newDir)
	}
	return migrateErr
}

func (m *Manager) lockArtifactJobsForMigration(parentSession, dir string) []artifactMigrationJob {
	parentSession = strings.TrimSpace(parentSession)
	dir = filepath.Clean(strings.TrimSpace(dir))
	m.mu.Lock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		if j == nil || strings.TrimSpace(j.SessionID) != parentSession {
			continue
		}
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	sort.Slice(jobs, func(i, k int) bool {
		return jobs[i].ID < jobs[k].ID
	})
	locked := make([]artifactMigrationJob, 0, len(jobs))
	for _, j := range jobs {
		j.mu.Lock()
		if !artifactPathInDir(j.artifactPath, dir) {
			j.mu.Unlock()
			continue
		}
		locked = append(locked, artifactMigrationJob{job: j, wasOpen: j.artifactFile != nil})
	}
	return locked
}

func artifactPathInDir(path, dir string) bool {
	path = filepath.Clean(strings.TrimSpace(path))
	dir = filepath.Clean(strings.TrimSpace(dir))
	if path == "." || dir == "." {
		return false
	}
	return filepath.Dir(path) == dir
}

func openArtifactMigrationFiles(jobs []artifactMigrationJob) map[string]bool {
	skip := map[string]bool{}
	for _, item := range jobs {
		j := item.job
		if j == nil || j.artifactFile == nil {
			continue
		}
		if j.artifactPath != "" {
			skip[filepath.Base(j.artifactPath)] = true
		}
		if j.artifactMetaPath != "" {
			skip[filepath.Base(j.artifactMetaPath)] = true
		}
	}
	return skip
}

func rebaseArtifactMigrationJobs(jobs []artifactMigrationJob, dir string) {
	for _, item := range jobs {
		j := item.job
		if j == nil || item.wasOpen {
			continue
		}
		if j.artifactPath != "" {
			j.artifactPath = filepath.Join(dir, filepath.Base(j.artifactPath))
		}
		if j.artifactMetaPath != "" {
			j.artifactMetaPath = filepath.Join(dir, filepath.Base(j.artifactMetaPath))
		}
	}
}

func unlockArtifactMigrationJobs(jobs []artifactMigrationJob) {
	for _, v := range slices.Backward(jobs) {
		if v.job != nil {
			v.job.mu.Unlock()
		}
	}
}

func migrateArtifactDirSkipping(src, dst string, skip map[string]bool) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := ensurePrivateArtifactDir(dst); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if skip[entry.Name()] {
			continue
		}
		if err := moveArtifactFile(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	_ = os.Remove(src)
	return nil
}

func moveArtifactFile(src, dst string) error {

	if err := os.Chmod(src, 0o600); err != nil {
		return err
	}
	if err := renamePath(src, dst); err == nil {
		return nil
	}
	if err := copyArtifactFile(src, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

func copyArtifactFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := out.Chmod(0o600); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return closeErr
	}
	return nil
}

func (m *Manager) loadSessionArtifacts(parentSession, sessionPath, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		m.mu.Lock()
		m.loaded[parentSession] = true
		m.mu.Unlock()
		return
	}
	var loaded []*Job
	deferredLiveOwner := false
	var repairErrors []string
	maxSeq := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != jobMetaExt {
			continue
		}
		metaPath := filepath.Join(dir, entry.Name())
		meta, err := readMeta(metaPath)
		if err != nil || strings.TrimSpace(meta.ID) == "" {
			continue
		}
		id := strings.TrimSpace(meta.ID)
		if seq := maxJobSeq(id); seq > maxSeq {
			maxSeq = seq
		}

		if meta.Status == Running {
			if managerOwnerIsLive(meta.OwnerID) {
				deferredLiveOwner = true
				continue
			}
			if m.sessionOwnershipProbe == nil || !m.sessionOwnershipProbe(sessionPath) {
				deferredLiveOwner = true
				continue
			}
			meta.Status = Interrupted
			if meta.FinishedAt == 0 {
				meta.FinishedAt = nowMs()
			}
			meta.ArtifactComplete = false
			if err := repairArtifactMeta(metaPath, meta); err != nil {

				deferredLiveOwner = true
				repairErrors = append(repairErrors, fmt.Sprintf("repair job %s metadata: %v", id, err))
				continue
			}
		}
		done := make(chan struct{})
		close(done)
		logPath := filepath.Join(dir, id+jobLogExt)
		if strings.TrimSpace(meta.LogPath) != "" {
			logPath = filepath.Join(dir, filepath.Base(meta.LogPath))
		}
		loaded = append(loaded, &Job{
			ID:               id,
			Kind:             meta.Kind,
			Label:            meta.Label,
			SessionID:        parentSession,
			status:           meta.Status,
			startedAt:        meta.StartedAt,
			finishedAt:       meta.FinishedAt,
			activityAt:       meta.FinishedAt,
			done:             done,
			artifactPath:     logPath,
			artifactMetaPath: filepath.Join(dir, id+jobMetaExt),
			artifactComplete: meta.ArtifactComplete,
			artifactErr:      meta.ArtifactError,
			tombstone:        true,
			evidence:         mutationEvidenceFromArtifact(meta),
		})
	}
	if len(repairErrors) > 0 {
		m.sink.Emit(event.Event{
			Kind:   event.Notice,
			Level:  event.LevelWarn,
			Text:   "Background job recovery did not complete.",
			Detail: strings.Join(repairErrors, "; "),
		})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range loaded {
		key := jobKey(parentSession, j.ID)
		if _, exists := m.jobs[key]; exists {
			continue
		}
		m.jobs[key] = j
		m.order = append(m.order, key)
	}
	if maxSeq > m.seq {
		m.seq = maxSeq
	}
	m.loaded[parentSession] = !deferredLiveOwner
}
