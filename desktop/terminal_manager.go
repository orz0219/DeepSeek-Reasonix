package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/secrets"
)

func (m *terminalManager) create(tabID, workspaceKey, dir string, command terminalCommand) (TerminalSessionView, error) {
	tabID = strings.TrimSpace(tabID)
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return TerminalSessionView{}, errTerminalManagerOff
	}
	if tabID == "" {
		m.mu.Unlock()
		return TerminalSessionView{}, errTerminalStaleTab
	}
	if _, closed := m.closedTabIDs[tabID]; closed {
		m.mu.Unlock()
		return TerminalSessionView{}, errTerminalStaleTab
	}
	generation := m.tabGeneration[tabID]
	if len(m.byWorkspace[workspaceKey])+m.starting[workspaceKey] >= maxTerminalsPerWorkspace {
		m.mu.Unlock()
		return TerminalSessionView{}, fmt.Errorf("terminal session limit reached (%d)", maxTerminalsPerWorkspace)
	}
	m.starting[workspaceKey]++
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.starting[workspaceKey]--
		if m.starting[workspaceKey] == 0 {
			delete(m.starting, workspaceKey)
		}
		m.mu.Unlock()
	}()

	id, err := newTerminalID()
	if err != nil {
		return TerminalSessionView{}, err
	}
	proc, err := m.start(terminalStartSpec{
		command: command,
		dir:     dir,
		env:     terminalEnvironment(secrets.ProcessEnv()),
		cols:    defaultTerminalColumns,
		rows:    defaultTerminalRows,
	})
	if err != nil {
		return TerminalSessionView{}, fmt.Errorf("start terminal: %w", err)
	}

	session := &terminalSession{
		view: TerminalSessionView{
			ID:        id,
			Title:     command.label,
			Shell:     command.label,
			Cwd:       dir,
			CreatedAt: time.Now().UnixMilli(),
			Running:   true,
		},
		tabID:        tabID,
		workspaceKey: workspaceKey,
		process:      proc,
		readDone:     make(chan struct{}),
		done:         make(chan struct{}),
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = proc.Close()
		return TerminalSessionView{}, errTerminalManagerOff
	}
	if _, closed := m.closedTabIDs[tabID]; closed {
		m.mu.Unlock()
		_ = proc.Close()
		return TerminalSessionView{}, errTerminalStaleTab
	}
	if m.tabGeneration[tabID] != generation {
		m.mu.Unlock()
		_ = proc.Close()
		return TerminalSessionView{}, errTerminalStaleTab
	}
	m.sessions[id] = session
	m.byWorkspace[workspaceKey] = append(m.byWorkspace[workspaceKey], id)
	view := session.view
	m.mu.Unlock()

	go m.readLoop(session)
	go m.waitLoop(session)
	return view, nil
}

func (m *terminalManager) snapshot(workspaceKey, sessionID string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[strings.TrimSpace(sessionID)]
	if session == nil || session.workspaceKey != workspaceKey {
		return ""
	}
	return string(session.output)
}

func appendTerminalSnapshot(current, data []byte) []byte {
	if len(data) >= maxTerminalSnapshotBytes {
		return append([]byte(nil), data[len(data)-maxTerminalSnapshotBytes:]...)
	}
	if over := len(current) + len(data) - maxTerminalSnapshotBytes; over > 0 {
		if over >= len(current) {
			current = current[:0]
		} else {
			current = append([]byte(nil), current[over:]...)
		}
	}
	return append(current, data...)
}

func (m *terminalManager) list(workspaceKey string) []TerminalSessionView {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := m.byWorkspace[workspaceKey]
	out := make([]TerminalSessionView, 0, len(ids))
	for _, id := range ids {
		if session := m.sessions[id]; session != nil {
			out = append(out, session.view)
		}
	}
	return out
}

func (m *terminalManager) sessionLocked(workspaceKey, sessionID string) (*terminalSession, error) {
	session := m.sessions[strings.TrimSpace(sessionID)]
	if session == nil || session.workspaceKey != workspaceKey {
		return nil, errors.New("terminal session not found in the active workspace")
	}
	return session, nil
}

func (m *terminalManager) write(workspaceKey, sessionID string, data []byte) error {
	m.mu.Lock()
	session, err := m.sessionLocked(workspaceKey, sessionID)
	if err == nil && !session.view.Running {
		err = errors.New("terminal session has exited")
	}
	var proc terminalProcess
	if err == nil {
		proc = session.process
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = proc.Write(data)
	return err
}

func (m *terminalManager) resize(workspaceKey, sessionID string, cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	if cols > maxTerminalColumns {
		cols = maxTerminalColumns
	}
	if rows > maxTerminalRows {
		rows = maxTerminalRows
	}
	m.mu.Lock()
	session, err := m.sessionLocked(workspaceKey, sessionID)
	var proc terminalProcess
	if err == nil && session.view.Running {
		proc = session.process
	}
	m.mu.Unlock()
	if err != nil || proc == nil {
		return err
	}
	return proc.Resize(cols, rows)
}

func (m *terminalManager) rename(workspaceKey, sessionID, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("terminal title is required")
	}
	if len([]rune(title)) > 80 {
		return errors.New("terminal title is too long")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.sessionLocked(workspaceKey, sessionID)
	if err != nil {
		return err
	}
	session.view.Title = title
	return nil
}

func (m *terminalManager) closeTerminal(workspaceKey, sessionID string) error {
	m.mu.Lock()
	session, err := m.sessionLocked(workspaceKey, sessionID)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.removeSessionLocked(session)
	m.mu.Unlock()

	_ = session.process.Close()
	select {
	case <-session.done:
	case <-time.After(terminalCloseWait):
	}
	return nil
}

func (m *terminalManager) closeForTab(tabID string) {
	m.closeSessions(m.detachForTab(tabID))
}

// detachForTab closes the creation gate and removes every registered session
// without waiting on process I/O. Callers can use it while serializing an App
// capability transition, then close the returned processes after releasing
// App.mu.
func (m *terminalManager) detachForTab(tabID string) []*terminalSession {
	if m == nil {
		return nil
	}
	tabID = strings.TrimSpace(tabID)
	if tabID == "" {
		return nil
	}
	m.mu.Lock()
	m.closedTabIDs[tabID] = struct{}{}
	m.tabGeneration[tabID]++
	sessions := make([]*terminalSession, 0)
	for _, session := range m.sessions {
		if session.tabID != tabID {
			continue
		}
		m.removeSessionLocked(session)
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	return sessions
}

func (m *terminalManager) closeSessions(sessions []*terminalSession) {
	if m == nil || len(sessions) == 0 {
		return
	}
	for _, session := range sessions {
		_ = session.process.Close()
	}
	deadline := time.NewTimer(terminalCloseWait)
	defer deadline.Stop()
	for _, session := range sessions {
		select {
		case <-session.done:
		case <-deadline.C:
			return
		}
	}
}

func (m *terminalManager) reopenForTab(tabID string) {
	if m == nil {
		return
	}
	tabID = strings.TrimSpace(tabID)
	if tabID == "" {
		return
	}
	m.mu.Lock()
	if !m.closed {
		delete(m.closedTabIDs, tabID)
	}
	m.mu.Unlock()
}

func (m *terminalManager) removeSessionLocked(session *terminalSession) {
	delete(m.sessions, session.view.ID)
	ids := m.byWorkspace[session.workspaceKey]
	filtered := ids[:0]
	for _, id := range ids {
		if id != session.view.ID {
			filtered = append(filtered, id)
		}
	}
	if len(filtered) == 0 {
		delete(m.byWorkspace, session.workspaceKey)
	} else {
		m.byWorkspace[session.workspaceKey] = filtered
	}
}

func (m *terminalManager) closeAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.closed = true
	sessions := make([]*terminalSession, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessions = make(map[string]*terminalSession)
	m.byWorkspace = make(map[string][]string)
	m.mu.Unlock()

	for _, session := range sessions {
		_ = session.process.Close()
	}
	deadline := time.NewTimer(terminalCloseWait)
	defer deadline.Stop()
	for _, session := range sessions {
		select {
		case <-session.done:
		case <-deadline.C:
			return
		}
	}
}

func (m *terminalManager) readLoop(session *terminalSession) {
	defer close(session.readDone)
	buf := make([]byte, 8*1024)
	for {
		n, err := session.process.Read(buf)
		if n > 0 {
			active := false
			m.mu.Lock()
			if current := m.sessions[session.view.ID]; current == session {
				session.output = appendTerminalSnapshot(session.output, buf[:n])
				active = true
			}
			m.mu.Unlock()
			if active {
				m.emitOutput(session.view.ID, buf[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

func (m *terminalManager) waitLoop(session *terminalSession) {
	exitCode, waitErr := session.process.Wait()
	select {
	case <-session.readDone:
	case <-time.After(terminalCloseWait):
	}
	_ = session.process.Close()
	if waitErr != nil && exitCode == 0 {
		exitCode = -1
	}
	m.mu.Lock()
	removed := true
	if current := m.sessions[session.view.ID]; current == session {
		current.view.Running = false
		current.view.ExitCode = &exitCode
		removed = false
	}
	m.mu.Unlock()
	close(session.done)
	m.emitExit(session.view.ID, exitCode, removed)
}

func (m *terminalManager) emitOutput(id string, data []byte) {
	if m.app == nil || len(data) == 0 {
		return
	}
	m.app.emitRuntimeEvent(terminalOutputChannel, map[string]any{
		"id":   id,
		"data": base64.StdEncoding.EncodeToString(data),
	})
}

func (m *terminalManager) emitExit(id string, exitCode int, removed bool) {
	if m.app == nil {
		return
	}
	m.app.emitRuntimeEvent(terminalExitChannel, map[string]any{
		"id":       id,
		"exitCode": exitCode,
		"removed":  removed,
	})
}

func newTerminalID() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "term-" + hex.EncodeToString(raw[:]), nil
}
