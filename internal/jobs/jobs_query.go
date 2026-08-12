package jobs

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/nilutil"
)

const (
	Running     Status = "running"
	Done        Status = "done"
	Failed      Status = "failed"
	Killed      Status = "killed"
	Interrupted Status = "interrupted"
)

// recordCompletion queues the finished-job summary for DrainCompletedNote and
// emits a closing Notice (warn for a failure, info otherwise).
func (m *Manager) recordCompletion(parentSession, id, kind, label string, st Status, err error) {
	tag := id
	if label != "" {
		tag = fmt.Sprintf("%s (%s)", id, label)
	}
	parentSession = strings.TrimSpace(parentSession)
	shouldEmit := false
	m.mu.Lock()
	if parentSession != "" && m.destroying[parentSession] {
		m.mu.Unlock()
		return
	}
	m.completed = append(m.completed, completion{
		sessionID: parentSession,
		text:      fmt.Sprintf("%s — %s", tag, st),
	})
	active := m.active
	shouldEmit = active == "" || parentSession == "" || active == parentSession
	m.mu.Unlock()

	if !nilutil.IsNil(m.taskRecorder) {
		m.taskRecorder.RecordDone(id, st, err)
	}

	level, text := event.LevelInfo, fmt.Sprintf("background %s finished: %s", kind, id)
	detail := ""
	switch st {
	case Failed:
		level, text = event.LevelWarn, fmt.Sprintf("background %s failed: needs attention", kind)
		detail = fmt.Sprintf("background %s failed: %s — %v", kind, id, err)
	case Killed:
		text = fmt.Sprintf("background %s killed: %s", kind, id)
	}
	if shouldEmit {
		m.sink.Emit(event.Event{Kind: event.Notice, Level: level, Text: text, Detail: detail})
	}
}

func (m *Manager) recordStalled(parentSession, id, kind, label string) {
	tag := id
	if label != "" {
		tag = fmt.Sprintf("%s (%s)", id, label)
	}
	parentSession = strings.TrimSpace(parentSession)
	m.mu.Lock()
	if parentSession != "" && m.destroying[parentSession] {
		m.mu.Unlock()
		return
	}
	text := fmt.Sprintf("%s may be stalled — still running after %s with no visible output. Inspect it with wait or bash_output, or stop it with kill_shell.", tag, m.stalledWarning.Round(time.Second))
	m.completed = append(m.completed, completion{sessionID: parentSession, text: text})
	active := m.active
	shouldEmit := active == "" || parentSession == "" || active == parentSession
	m.mu.Unlock()
	if shouldEmit {
		m.sink.Emit(event.Event{
			Kind:  event.Notice,
			Level: event.LevelWarn,
			Text:  fmt.Sprintf("background %s may be stalled: %s — still running after %s with no visible output; inspect with wait/bash_output or stop with kill_shell", kind, id, m.stalledWarning.Round(time.Second)),
		})
	}
}

func (m *Manager) get(parentSession, id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.findJobLocked(parentSession, id)
}

func (m *Manager) findJobLocked(parentSession, id string) *Job {
	parentSession = strings.TrimSpace(parentSession)
	id = strings.TrimSpace(id)
	if parentSession != "" {
		return m.jobs[jobKey(parentSession, id)]
	}
	for _, key := range m.order {
		j := m.jobs[key]
		if j != nil && j.ID == id {
			return j
		}
	}
	return nil
}

// Output returns the job's output produced since the last Output call plus its
// current status. ok is false when the id is unknown.
func (m *Manager) Output(id string) (text string, status Status, ok bool) {
	return m.OutputForSession("", id)
}

// OutputForSession returns output only when id belongs to parentSession. Empty
// parentSession preserves the legacy unscoped behavior.
func (m *Manager) OutputForSession(parentSession, id string) (text string, status Status, ok bool) {
	j := m.get(parentSession, id)
	if j == nil {
		return "", "", false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.artifactPath != "" {
		text = j.readArtifactSinceOffsetLocked()
	} else {
		full := string(j.tail)
		if j.readOffset < int64(len(full)) {
			text = full[j.readOffset:]
			j.readOffset = int64(len(full))
		}
	}

	if text == "" && j.status != Running && j.result != "" && !j.resultRead {
		text = j.result
		j.resultRead = true
	}
	if j.artifactErr != "" {
		if text != "" {
			text += "\n"
		}
		text += "job artifact incomplete: " + j.artifactErr
	}
	return text, j.status, true
}

func (j *Job) readArtifactSinceOffsetLocked() string {
	f, err := os.Open(j.artifactPath)
	if err != nil {
		if j.artifactErr == "" {
			j.artifactErr = err.Error()
		}
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		if j.artifactErr == "" {
			j.artifactErr = err.Error()
		}
		return ""
	}
	size := info.Size()
	if j.readOffset > size {
		j.readOffset = size
		return ""
	}
	if _, err := f.Seek(j.readOffset, io.SeekStart); err != nil {
		if j.artifactErr == "" {
			j.artifactErr = err.Error()
		}
		return ""
	}
	b, err := io.ReadAll(f)
	if err != nil {
		if j.artifactErr == "" {
			j.artifactErr = err.Error()
		}
		return ""
	}
	text := string(b)
	j.readOffset = size
	return text
}

// readArtifactAllLocked deliberately reads raw bytes: the artifact is captured
// subprocess output (possibly binary), not a user-edited config file, and the
// incremental reader (readArtifactSinceOffsetLocked) is raw byte-offset based —
// decoding only the whole-file path would render the same artifact in two
// different encodings and could garble binary output via UTF-16 misdetection.
func (j *Job) readArtifactAllLocked() string {
	if j.artifactPath == "" {
		return ""
	}
	b, err := os.ReadFile(j.artifactPath)
	if err != nil {
		if j.artifactErr == "" {
			j.artifactErr = err.Error()
		}
		return ""
	}
	return string(b)
}

// Kill cancels a running job. Returns false when the id is unknown or the job has
// already finished.
func (m *Manager) Kill(id string) bool {
	return m.KillForSession("", id)
}

// KillForSession cancels a running job only when it belongs to parentSession.
// Empty parentSession preserves the legacy unscoped behavior.
func (m *Manager) KillForSession(parentSession, id string) bool {
	j := m.get(parentSession, id)
	if j == nil {
		return false
	}
	j.mu.Lock()
	running := j.status == Running
	if running {

		j.status = Killed
	}
	j.mu.Unlock()
	if !running {
		return false
	}
	j.cancel()
	return true
}

// Wait blocks until the named jobs (or every currently-running job when ids is
// empty) reach a terminal state, or ctx is cancelled, or timeoutSec elapses
// (0 = no timeout). It returns each target's snapshot regardless of why it
// returned, so a timeout still reports partial progress.
func (m *Manager) Wait(ctx context.Context, ids []string, timeoutSec int) []Result {
	return m.WaitForSession(ctx, "", ids, timeoutSec)
}

// WaitForSession waits only on jobs owned by parentSession. Empty parentSession
// preserves the legacy unscoped behavior.
func (m *Manager) WaitForSession(ctx context.Context, parentSession string, ids []string, timeoutSec int) []Result {
	targets := m.resolve(parentSession, ids)
	if len(targets) == 0 {
		return nil
	}
	var timeout <-chan time.Time
	if timeoutSec > 0 {
		t := time.NewTimer(time.Duration(timeoutSec) * time.Second)
		defer t.Stop()
		timeout = t.C
	}
	for _, j := range targets {
		select {
		case <-j.done:
		case <-ctx.Done():
			return m.results(targets)
		case <-timeout:
			return m.results(targets)
		}
	}
	return m.results(targets)
}

// resolve maps requested ids to jobs; an empty list selects all running jobs.
func (m *Manager) resolve(parentSession string, ids []string) []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Job
	if len(ids) == 0 {
		for _, key := range m.order {
			j := m.jobs[key]
			if !sessionMatches(parentSession, j.SessionID) {
				continue
			}
			j.mu.Lock()
			running := j.status == Running
			j.mu.Unlock()
			if running {
				out = append(out, j)
			}
		}
		return out
	}
	for _, id := range ids {
		if j := m.findJobLocked(parentSession, id); j != nil {
			out = append(out, j)
		}
	}
	return out
}

func (m *Manager) results(targets []*Job) []Result {
	out := make([]Result, 0, len(targets))
	for _, j := range targets {
		j.mu.Lock()
		text := j.result
		if text == "" && j.artifactPath != "" {
			text = j.readArtifactAllLocked()
		}
		if text == "" {
			text = string(j.tail)
		}
		if j.artifactErr != "" {
			if text != "" {
				text += "\n"
			}
			text += "job artifact incomplete: " + j.artifactErr
		}
		out = append(out, Result{ID: j.ID, Kind: j.Kind, Label: j.Label, Status: j.status, Output: text})
		j.mu.Unlock()
	}
	return out
}

// Running returns a snapshot of the still-running jobs (for the status bar).
func (m *Manager) Running() []View {
	return m.RunningForSession("")
}

// RunningForSession returns still-running jobs owned by parentSession. Empty
// parentSession preserves the legacy unscoped behavior.
func (m *Manager) RunningForSession(parentSession string) []View {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []View
	for _, key := range m.order {
		j := m.jobs[key]
		if !sessionMatches(parentSession, j.SessionID) {
			continue
		}
		select {
		case <-j.done:
			continue
		default:
		}
		j.mu.Lock()

		out = append(out, View{ID: j.ID, Kind: j.Kind, Label: j.Label, Status: string(Running), StartedAt: j.startedAt})
		j.mu.Unlock()
	}
	return out
}

// ReserveStartForSession atomically reserves capacity for a job start. The
// caller must release the reservation after StartForSession has registered the
// job (or when setup fails). Running jobs and in-flight start reservations both
// count toward limit, so concurrent callers cannot overshoot it.
func (m *Manager) ReserveStartForSession(parentSession, kind string, limit int) (release func(), running int, ok bool) {
	if limit <= 0 {
		return func() {}, 0, true
	}
	parentSession = strings.TrimSpace(parentSession)
	key := jobKey(parentSession, kind)
	m.mu.Lock()
	for _, jobKey := range m.order {
		j := m.jobs[jobKey]
		if j == nil || !sessionMatches(parentSession, j.SessionID) || j.Kind != kind {
			continue
		}
		select {
		case <-j.done:
		default:
			running++
		}
	}
	running += m.reservations[key]
	if running >= limit {
		m.mu.Unlock()
		return func() {}, running, false
	}
	m.reservations[key]++
	m.mu.Unlock()

	var once sync.Once
	release = func() {
		once.Do(func() {
			m.mu.Lock()
			m.reservations[key]--
			if m.reservations[key] == 0 {
				delete(m.reservations, key)
			}
			m.mu.Unlock()
		})
	}
	return release, running, true
}

// HasUnfinishedForSession reports whether parentSession owns any job whose
// goroutine has not fully exited yet. Empty parentSession preserves the legacy
// unscoped behavior.
func (m *Manager) HasUnfinishedForSession(parentSession string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, key := range m.order {
		j := m.jobs[key]
		if !sessionMatches(parentSession, j.SessionID) {
			continue
		}
		select {
		case <-j.done:
		default:
			return true
		}
	}
	return false
}

// DrainCompletedNote returns (and clears) a one-line summary of jobs that
// finished since the last drain, for the controller to fold into the next turn
// so the model learns of completions. "" when nothing finished.
func (m *Manager) DrainCompletedNote() string {
	return m.DrainCompletedNoteForSession("")
}

// DrainCompletedNoteForSession drains completion notes for parentSession only.
// Notes for other sessions stay queued until that session becomes active again.
// Empty parentSession preserves the legacy unscoped behavior.
func (m *Manager) DrainCompletedNoteForSession(parentSession string) string {
	m.mu.Lock()
	var c []string
	if strings.TrimSpace(parentSession) == "" {
		for _, item := range m.completed {
			c = append(c, item.text)
		}
		m.completed = nil
	} else {
		remaining := m.completed[:0]
		for _, item := range m.completed {
			if item.sessionID == parentSession {
				c = append(c, item.text)
			} else {
				remaining = append(remaining, item)
			}
		}
		m.completed = remaining
	}
	m.mu.Unlock()
	if len(c) == 0 {
		return ""
	}
	return "Background job updates since your last message: " + strings.Join(c, "; ") +
		". Read their output with bash_output or wait if you still need it."
}

// SetActiveSession controls which session receives lifecycle notices for jobs
// that finish asynchronously. Empty active session preserves legacy behavior.
func (m *Manager) SetActiveSession(parentSession string) {
	m.mu.Lock()
	m.active = strings.TrimSpace(parentSession)
	m.mu.Unlock()
}

// validateTrustedSessionPath performs defense-in-depth syntax validation on a
// transcript path already trusted by the store/controller layer. It rejects
// control characters, but deliberately preserves separators and `..`: those are
// valid host-path syntax, and rejecting them without a trusted root would break
// legitimate relative paths without establishing filesystem containment.
func validateTrustedSessionPath(sessionPath string) error {
	if sessionPath == "" {
		return fmt.Errorf("jobs: sessionPath must not be empty")
	}
	for i, r := range sessionPath {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("jobs: sessionPath contains control character 0x%02x at index %d", r, i)
		}
	}
	return nil
}

// SetActiveSessionPath binds a parent session id to its persistent transcript
// path, migrates any temporary artifacts, and loads completed job tombstones from
// the session sidecar. sessionPath must come from the trusted store/controller
// path; this method does not establish filesystem containment on its own.
func (m *Manager) SetActiveSessionPath(parentSession, sessionPath string) {
	parentSession = strings.TrimSpace(parentSession)
	sessionPath = strings.TrimSpace(sessionPath)

	if parentSession == "" || sessionPath == "" {
		m.mu.Lock()
		m.active = parentSession
		m.mu.Unlock()
		return
	}

	if err := validateTrustedSessionPath(sessionPath); err != nil {
		m.mu.Lock()
		m.active = parentSession

		delete(m.artifactDirs, parentSession)
		delete(m.loaded, parentSession)
		m.mu.Unlock()
		m.sink.Emit(event.Event{
			Kind:   event.Notice,
			Level:  event.LevelWarn,
			Text:   "Ignoring SetActiveSessionPath with invalid session path",
			Detail: fmt.Sprintf("session %q: %v", parentSession, err),
		})
		return
	}
	m.mu.Lock()
	m.active = parentSession
	oldDir := m.artifactDirLocked(parentSession)
	adoptDefault := false
	if _, hasDir := m.artifactDirs[parentSession]; !hasDir && m.hasUnscopedJobsLocked() {
		oldDir = m.artifactDirLocked("")
		adoptDefault = true
	}
	newDir := ArtifactDir(sessionPath)
	m.artifactDirs[parentSession] = newDir
	loaded := m.loaded[parentSession]
	m.mu.Unlock()

	if oldDir != "" && newDir != "" && oldDir != newDir {
		oldSession := parentSession
		if adoptDefault {
			oldSession = ""
		}
		if err := m.migrateArtifactDirForSession(oldSession, oldDir, newDir); err != nil {
			if adoptDefault {
				m.mu.Lock()
				m.adoptUnscopedJobsLocked(parentSession)
				m.mu.Unlock()
			}
			m.recordArtifactMigrationError(parentSession, err)
		} else {
			m.mu.Lock()
			if adoptDefault {
				m.adoptUnscopedJobsLocked(parentSession)
			}
			m.mu.Unlock()
		}
	}
	if !loaded {
		m.loadSessionArtifacts(parentSession, sessionPath, newDir)
	}
}

func (m *Manager) hasUnscopedJobsLocked() bool {
	for _, j := range m.jobs {
		if j != nil && strings.TrimSpace(j.SessionID) == "" {
			return true
		}
	}
	return false
}

func (m *Manager) adoptUnscopedJobsLocked(parentSession string) {
	parentSession = strings.TrimSpace(parentSession)
	if parentSession == "" {
		return
	}
	for i := range m.completed {
		if strings.TrimSpace(m.completed[i].sessionID) == "" {
			m.completed[i].sessionID = parentSession
		}
	}
	for oldKey, j := range m.jobs {
		if j == nil || strings.TrimSpace(j.SessionID) != "" {
			continue
		}
		newKey := jobKey(parentSession, j.ID)
		if existing := m.jobs[newKey]; existing != nil && existing != j {
			j.mu.Lock()
			j.artifactErr = "migration: job id collision while adopting temporary session"
			j.artifactComplete = false
			j.mu.Unlock()
			continue
		}
		delete(m.jobs, oldKey)
		j.SessionID = parentSession
		m.jobs[newKey] = j
		for i, key := range m.order {
			if key == oldKey {
				m.order[i] = newKey
			}
		}
	}
}
