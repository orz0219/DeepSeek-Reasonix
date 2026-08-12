package jobs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"reasonix/internal/evidence"
)

func runRecovered(ctx context.Context, out io.Writer, run func(context.Context, io.Writer) (string, error)) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error: panic: %v\n%s", r, debug.Stack())
		}
	}()
	return run(ctx, out)
}

func (m *Manager) openArtifactLocked(parentSession, id string) (logPath, metaPath string, file *os.File, artifactErr string) {
	dir := m.artifactDirLocked(parentSession)
	if dir == "" {
		return "", "", nil, "artifact directory unavailable"
	}
	if err := ensurePrivateArtifactDir(dir); err != nil {
		return filepath.Join(dir, id+jobLogExt), filepath.Join(dir, id+jobMetaExt), nil, err.Error()
	}
	logPath = filepath.Join(dir, id+jobLogExt)
	metaPath = filepath.Join(dir, id+jobMetaExt)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return logPath, metaPath, nil, err.Error()
	}

	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return logPath, metaPath, nil, err.Error()
	}
	return logPath, metaPath, f, ""
}

func ensurePrivateArtifactDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	return os.Chmod(dir, 0o700)
}

func (m *Manager) artifactDirLocked(parentSession string) string {
	parentSession = strings.TrimSpace(parentSession)
	if parentSession != "" {
		if dir := strings.TrimSpace(m.artifactDirs[parentSession]); dir != "" {
			return dir
		}
	}
	if strings.TrimSpace(m.tempRoot) == "" {
		return ""
	}
	if parentSession == "" {
		return filepath.Join(m.tempRoot, "default")
	}
	return filepath.Join(m.tempRoot, parentSession)
}

func (m *Manager) writeJobMetaLocked(j *Job, st Status) error {
	if j.artifactMetaPath == "" {
		return nil
	}
	meta := artifactMeta{
		ID:               j.ID,
		Kind:             j.Kind,
		Label:            j.Label,
		SessionID:        j.SessionID,
		OwnerID:          m.ownerID,
		Status:           st,
		StartedAt:        j.startedAt,
		FinishedAt:       j.finishedAt,
		ArtifactComplete: st != Running && j.artifactComplete && j.artifactErr == "",
		ArtifactError:    j.artifactErr,
		LogPath:          filepath.Base(j.artifactPath),
	}
	if j.Kind == "task" {
		meta.MutationEvidenceVersion = mutationEvidenceVersion
		meta.MutationEvidence = mutationEvidenceForArtifact(j.evidence)
	}
	return writeMeta(j.artifactMetaPath, meta)
}

func mutationEvidenceForArtifact(summary evidence.ChildEvidenceSummary) *artifactMutationEvidence {
	firstMutation := -1
	for i, receipt := range summary.Receipts {
		if receipt.Success && receipt.Mutation {
			firstMutation = i
			break
		}
	}
	if firstMutation < 0 {
		return nil
	}
	return &artifactMutationEvidence{
		Risk:  string(evidence.ClassifyMutationRisk(summary.Receipts, firstMutation)),
		Paths: summary.MutationPaths(),
	}
}

func mutationEvidenceFromArtifact(meta artifactMeta) evidence.ChildEvidenceSummary {
	if meta.Kind != "task" {
		return evidence.ChildEvidenceSummary{}
	}
	if meta.MutationEvidenceVersion != mutationEvidenceVersion {

		return opaqueRecoveredTaskMutation()
	}
	if meta.MutationEvidence == nil {

		return evidence.ChildEvidenceSummary{}
	}

	paths := append([]string(nil), meta.MutationEvidence.Paths...)
	switch evidence.RiskLevel(meta.MutationEvidence.Risk) {
	case evidence.RiskLow, evidence.RiskMedium:

	case evidence.RiskHigh:

		paths = nil
	default:
		return opaqueRecoveredTaskMutation()
	}
	return evidence.ChildEvidenceSummary{Receipts: []evidence.Receipt{{
		ToolName: recoveredBackgroundTaskToolName,
		Success:  true,
		Write:    true,
		Mutation: true,
		Paths:    paths,
	}}}
}

func opaqueRecoveredTaskMutation() evidence.ChildEvidenceSummary {
	return evidence.ChildEvidenceSummary{Receipts: []evidence.Receipt{{
		ToolName: recoveredBackgroundTaskToolName,
		Success:  true,
		Write:    true,
		Mutation: true,
	}}}
}

func (m *Manager) artifactTargetDirForJob(j *Job) string {
	if j == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := strings.TrimSpace(j.SessionID)
	if session == "" {
		return ""
	}
	return strings.TrimSpace(m.artifactDirs[session])
}

func (j *Job) noteArtifactErr(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	if j.artifactErr == "" {
		j.artifactErr = msg
	} else {
		j.artifactErr += "; " + msg
	}
	j.artifactComplete = false
}

func (j *Job) moveArtifactToDirLocked(dir string) error {
	dir = strings.TrimSpace(dir)
	if dir == "" || j.artifactPath == "" {
		return nil
	}
	if filepath.Clean(filepath.Dir(j.artifactPath)) == filepath.Clean(dir) {
		return nil
	}
	if err := ensurePrivateArtifactDir(dir); err != nil {
		return err
	}
	newLogPath := filepath.Join(dir, filepath.Base(j.artifactPath))
	if err := moveArtifactFile(j.artifactPath, newLogPath); err != nil {
		return err
	}
	j.artifactPath = newLogPath
	if j.artifactMetaPath != "" {
		j.artifactMetaPath = filepath.Join(dir, filepath.Base(j.artifactMetaPath))
	}
	return nil
}

func (m *Manager) monitorStalled(parentSession string, j *Job) {
	defer m.wg.Done()
	timer := time.NewTimer(m.stalledWarning)
	defer timer.Stop()
	for {
		select {
		case <-j.done:
			return
		case <-timer.C:
			j.mu.Lock()
			if j.runReturned || j.status != Running {
				j.mu.Unlock()
				return
			}
			idle := time.Since(time.UnixMilli(j.activityAt))
			if idle >= m.stalledWarning && !j.stalled {
				j.stalled = true
				j.mu.Unlock()
				m.recordStalled(parentSession, j.ID, j.Kind, j.Label)
				return
			}
			wait := m.stalledWarning - idle
			if wait <= 0 {
				wait = m.stalledWarning
			}
			j.mu.Unlock()
			timer.Reset(wait)
		}
	}
}
