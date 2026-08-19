package main

import (
	"reasonix/internal/taskcatalog"
	"reasonix/internal/taskmonitor"
)

// taskStore is the Store backing the task monitor panel.
func (a *App) taskStore() taskmonitor.WriteStore {
	return taskcatalog.ObservedStore()
}

// taskControl returns the process-wide ControlService backing the task
// monitor panel. A single instance keeps control operations serialized within
// this process (across processes the FileStore's per-task lock still
// arbitrates), and avoids re-creating the service on every Wails call.
func (a *App) taskControl() *taskmonitor.ControlService {
	a.taskCtrlOnce.Do(func() {
		a.taskCtrl = taskmonitor.NewControlService(a.taskStore())
	})
	return a.taskCtrl
}

func (a *App) projectDir() string {
	return a.activeWorkspaceRoot()
}

type taskMonitorTabTarget struct {
	projectDir  string
	sessionDir  string
	sessionPath string
	sessionID   string
}
