package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/billing"
	"reasonix/internal/control"
	"reasonix/internal/taskmonitor"
)

// taskMonitorTargetForTab snapshots the workspace and session identity owned by
// tabID. Wails dispatches bound calls concurrently, so resolving the active tab
// inside a task operation would allow a later tab switch to retarget it.
func (a *App) taskMonitorTargetForTab(tabID string) (taskMonitorTabTarget, error) {
	tabID = strings.TrimSpace(tabID)
	if tabID == "" {
		return taskMonitorTabTarget{}, fmt.Errorf("task monitor tab id is required")
	}

	a.mu.RLock()
	tab := a.tabByIDLocked(tabID)
	if tab == nil {
		a.mu.RUnlock()
		return taskMonitorTabTarget{}, fmt.Errorf("task monitor tab %q is unavailable", tabID)
	}
	workspaceRoot := strings.TrimSpace(tab.WorkspaceRoot)
	tabSessionPath := strings.TrimSpace(tab.SessionPath)
	ctrl := tab.Ctrl
	leaseKey := tab.sessionLeaseRuntimeKey()
	a.mu.RUnlock()

	projectDir := workspaceRoot
	if projectDir == "" {
		projectDir = "."
	}
	sessionDir := desktopSessionDir(workspaceRoot)
	sessionPath := tabSessionPath
	if ctrl != nil {
		if dir := strings.TrimSpace(ctrl.SessionDir()); dir != "" {
			sessionDir = dir
		}
		if path := strings.TrimSpace(ctrl.SessionPath()); path != "" {
			sessionPath = path
		}
	}

	if tabSessionPath != "" && sessionRuntimeKey(tabSessionPath) == leaseKey {
		sessionPath = tabSessionPath
		sessionDir = filepath.Dir(tabSessionPath)
	} else if ctrl == nil && tabSessionPath != "" {
		sessionDir = filepath.Dir(tabSessionPath)
	}

	target := taskMonitorTabTarget{
		projectDir:  projectDir,
		sessionDir:  sessionDir,
		sessionPath: sessionPath,
	}
	if sessionPath != "" {
		target.sessionID = agent.BranchID(sessionPath)
	}
	return target, nil
}

func (a *App) ListTasks() ([]taskmonitor.TaskSnapshot, error) {
	return a.taskStore().ListTasks(a.ctx, a.projectDir())
}

// CurrentTaskSessionID returns the stable branch ID for the active desktop
// session. Task Monitor uses this as an optional view filter; an empty value
// means that the active tab has no session controller yet.
func (a *App) CurrentTaskSessionID() string {
	_, ctrl := a.activeTabAndCtrl()
	if ctrl == nil {
		return ""
	}
	return agent.BranchID(ctrl.SessionPath())
}

// ListTasksForSession limits the project task view to one desktop session.
// The unfiltered ListTasks method remains for compatibility with existing
// callers and project-wide diagnostics.
func (a *App) ListTasksForSession(sessionID string) ([]taskmonitor.TaskSnapshot, error) {
	tasks, err := a.ListTasks()
	if err != nil || strings.TrimSpace(sessionID) == "" {
		return tasks, err
	}
	return filterTasksBySession(tasks, sessionID), nil
}

// ListTasksForTab returns the task view owned by tabID and filters it to that
// tab's session when one is available. It deliberately avoids active-tab state.
func (a *App) ListTasksForTab(tabID string) ([]taskmonitor.TaskSnapshot, error) {
	target, err := a.taskMonitorTargetForTab(tabID)
	if err != nil {
		return nil, err
	}
	tasks, err := a.taskStore().ListTasks(a.ctx, target.projectDir)
	if err != nil || target.sessionID == "" {
		return tasks, err
	}
	return filterTasksBySession(tasks, target.sessionID), nil
}

func filterTasksBySession(tasks []taskmonitor.TaskSnapshot, sessionID string) []taskmonitor.TaskSnapshot {
	filtered := make([]taskmonitor.TaskSnapshot, 0, len(tasks))
	for _, task := range tasks {
		if task.SessionID == sessionID {
			filtered = append(filtered, task)
		}
	}
	return filtered
}

func (a *App) GetTask(taskID string) (*taskmonitor.TaskSnapshot, error) {
	return a.taskStore().GetTask(a.ctx, a.projectDir(), taskID)
}

func (a *App) ListTaskEvents(taskID string, afterSequence int) ([]taskmonitor.TaskEvent, error) {
	return a.taskStore().ListEvents(a.ctx, a.projectDir(), taskID, afterSequence)
}

func (a *App) ListTaskEventsForTab(tabID, taskID string, afterSequence int) ([]taskmonitor.TaskEvent, error) {
	target, err := a.taskMonitorTargetForTab(tabID)
	if err != nil {
		return nil, err
	}
	return a.taskStore().ListEvents(a.ctx, target.projectDir, taskID, afterSequence)
}

func (a *App) StopTask(taskID string, expectedVersion uint64, reason, idemKey string) (taskmonitor.ControlResult, error) {
	projectDir := a.projectDir()
	return a.taskControl().StopTaskWithKiller(
		a.ctx, projectDir, taskID, expectedVersion, reason, idemKey,
		desktopTaskJobKiller{app: a, projectDir: projectDir},
	)
}

func (a *App) StopTaskForTab(tabID, taskID string, expectedVersion uint64, reason, idemKey string) (taskmonitor.ControlResult, error) {
	target, err := a.taskMonitorTargetForTab(tabID)
	if err != nil {
		return taskmonitor.ControlResult{}, err
	}
	return a.taskControl().StopTaskWithKiller(
		a.ctx, target.projectDir, taskID, expectedVersion, reason, idemKey,
		desktopTaskJobKiller{app: a, projectDir: target.projectDir},
	)
}

func (a *App) CancelTask(taskID string, expectedVersion uint64, reason, idemKey string) (taskmonitor.ControlResult, error) {
	projectDir := a.projectDir()
	return a.taskControl().CancelTaskWithKiller(
		a.ctx, projectDir, taskID, expectedVersion, reason, idemKey,
		desktopTaskJobKiller{app: a, projectDir: projectDir},
	)
}

func (a *App) CancelTaskForTab(tabID, taskID string, expectedVersion uint64, reason, idemKey string) (taskmonitor.ControlResult, error) {
	target, err := a.taskMonitorTargetForTab(tabID)
	if err != nil {
		return taskmonitor.ControlResult{}, err
	}
	return a.taskControl().CancelTaskWithKiller(
		a.ctx, target.projectDir, taskID, expectedVersion, reason, idemKey,
		desktopTaskJobKiller{app: a, projectDir: target.projectDir},
	)
}

func (a *App) RequeueTask(taskID string, expectedVersion uint64, idemKey string) (taskmonitor.ControlResult, error) {
	return a.taskControl().RequeueTask(a.ctx, a.projectDir(), taskID, expectedVersion, idemKey)
}

func (a *App) RequeueTaskForTab(tabID, taskID string, expectedVersion uint64, idemKey string) (taskmonitor.ControlResult, error) {
	target, err := a.taskMonitorTargetForTab(tabID)
	if err != nil {
		return taskmonitor.ControlResult{}, err
	}
	return a.taskControl().RequeueTask(a.ctx, target.projectDir, taskID, expectedVersion, idemKey)
}

func (a *App) OpenTaskSession(taskID string) (taskmonitor.ControlResult, error) {
	return a.taskControl().OpenTaskSession(a.ctx, a.projectDir(), taskID)
}

func (a *App) OpenTaskSessionForTab(tabID, taskID string) (taskmonitor.ControlResult, error) {
	target, err := a.taskMonitorTargetForTab(tabID)
	if err != nil {
		return taskmonitor.ControlResult{}, err
	}
	return a.taskControl().OpenTaskSession(a.ctx, target.projectDir, taskID)
}

type desktopTaskJobKiller struct {
	app        *App
	projectDir string
}

func (k desktopTaskJobKiller) Kill(sessionID, taskID string) bool {

	if k.app == nil || sessionID == "" || strings.TrimSpace(k.projectDir) == "" {
		return false
	}

	k.app.mu.RLock()
	tabs := k.app.runtimeTabsLocked()
	controllers := make([]control.SessionAPI, 0, len(tabs))
	for _, tab := range tabs {
		if tab != nil && tab.Ctrl != nil && sameProjectRoot(tab.WorkspaceRoot, k.projectDir) {
			controllers = append(controllers, tab.Ctrl)
		}
	}
	k.app.mu.RUnlock()

	for _, ctrl := range controllers {
		if agent.BranchID(ctrl.SessionPath()) != sessionID {
			continue
		}
		if killer, ok := ctrl.(interface{ CancelJob(string) bool }); ok && killer.CancelJob(taskID) {
			return true
		}
	}
	return false
}

// onboardingKeyEnv is the default provider (deepseek) key from config.Default().
const onboardingKeyEnv = "DEEPSEEK_API_KEY"

// onboardingBalanceURL doubles as a zero-token connectivity + auth probe:
// billing.FetchWithClient surfaces 401/403 for a bad key.
const onboardingBalanceURL = "https://api.deepseek.com/user/balance"

var connectKeyBalanceFetch = billing.FetchWithClient

// NativeConfirmRequest is the payload for ConfirmAction — a native OS confirmation
// dialog that replaces web-style confirm() for destructive or important actions.
type NativeConfirmRequest struct {
	Title        string `json:"title"`
	Message      string `json:"message"`
	Detail       string `json:"detail"`
	ConfirmLabel string `json:"confirmLabel"`
	CancelLabel  string `json:"cancelLabel"`
	Destructive  bool   `json:"destructive"`
}
