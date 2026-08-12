package main

import (
	"errors"
	"log"
)

// ListTasks returns a copy of the current tasks (in-memory).
func (e *HeartbeatEngine) ListTasks() []HeartbeatTask {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]HeartbeatTask, len(e.tasks))
	copy(out, e.tasks)
	return out
}

// ReloadTasks reloads the task list from disk and replaces the in-memory copy.
func (e *HeartbeatEngine) ReloadTasks() []HeartbeatTask {
	return e.ReloadConfig().Tasks
}

func (e *HeartbeatEngine) ReloadConfig() HeartbeatConfigView {
	e.mu.Lock()
	defer e.mu.Unlock()
	snapshot, err := e.readConfigSnapshot()
	if err != nil {
		log.Printf("[heartbeat] reload config: %v", err)
		return heartbeatConfigSnapshot{cfg: heartbeatConfig{Tasks: []HeartbeatTask{}}}.view()
	}
	e.recordConfigSnapshotLocked(snapshot)
	e.tasks = snapshot.cfg.Tasks
	e.prunePendingTopicsLocked(e.tasks)
	return snapshot.view()
}

// ReplaceTasks atomically replaces the task list and persists it.
func (e *HeartbeatEngine) ReplaceTasks(tasks []HeartbeatTask) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	expected, err := e.readConfigSnapshot()
	if err != nil {
		return err
	}
	if e.cfgInitialized && (expected.exists != e.cfgKnown || expected.digest != e.cfgDigest || expected.cfg.Revision != e.cfgRevision) {
		return ErrHeartbeatConfigConflict
	}
	if err := e.writeTasks(tasks, expected, true); err != nil {
		return err
	}
	latest, err := e.readConfigSnapshot()
	if err != nil {
		return err
	}
	e.recordConfigSnapshotLocked(latest)
	e.tasks = tasks
	e.prunePendingTopicsLocked(tasks)
	return nil
}

// ReplaceConfig applies a frontend edit only when its revision and ETag still
// identify the exact config the user edited. This prevents a stale panel from
// overwriting an external or second-process change.
func (e *HeartbeatEngine) ReplaceConfig(update HeartbeatConfigUpdate) (HeartbeatConfigView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	expected, err := e.readConfigSnapshot()
	if err != nil {
		return HeartbeatConfigView{}, err
	}
	if expected.cfg.Revision != update.Revision || expected.view().ETag != update.ETag {
		return expected.view(), ErrHeartbeatConfigConflict
	}
	if err := e.writeTasks(update.Tasks, expected, true); err != nil {
		return expected.view(), err
	}
	latest, err := e.readConfigSnapshot()
	if err != nil {
		return HeartbeatConfigView{}, err
	}
	e.recordConfigSnapshotLocked(latest)
	e.tasks = latest.cfg.Tasks
	e.prunePendingTopicsLocked(e.tasks)
	return latest.view(), nil
}

func (e *HeartbeatEngine) prunePendingTopicsLocked(tasks []HeartbeatTask) {
	if len(e.pendingTopics) == 0 {
		return
	}
	keep := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		if task.NewConversationEachRun {
			keep[task.ID] = true
		}
	}
	for id := range e.pendingTopics {
		if !keep[id] {
			delete(e.pendingTopics, id)
		}
	}
}

// TriggerNow runs a single task immediately by ID.
func (e *HeartbeatEngine) TriggerNow(id string) {
	e.mu.Lock()
	tasks := append([]HeartbeatTask(nil), e.tasks...)
	e.mu.Unlock()
	updates := make(map[string]HeartbeatTask, 1)
	for i, t := range tasks {
		if t.ID == id {
			tasks[i] = e.executeTask(t)
			updates[id] = tasks[i]
			break
		}
	}
	if len(updates) == 0 {
		return
	}
	e.mu.Lock()
	e.mergeRunUpdatesLocked(updates)
	e.mu.Unlock()
}

func (e *HeartbeatEngine) mergeRunUpdatesLocked(updates map[string]HeartbeatTask) {
	if len(updates) == 0 {
		return
	}

	for range 3 {
		expected, err := e.readConfigSnapshot()
		if err != nil {
			log.Printf("[heartbeat] cannot read config before run-state merge: %v", err)
			return
		}
		tasks := expected.cfg.Tasks
		if !expected.exists {
			switch {
			case e.cfgDeleted || (e.cfgInitialized && e.cfgKnown):

				e.tasks = nil
				e.pendingTopics = make(map[string]heartbeatPendingTopic)
				e.cfgDeleted = true
				e.recordConfigSnapshotLocked(expected)
				return
			case !e.cfgInitialized:

				tasks = append([]HeartbeatTask(nil), e.tasks...)
			}
		}
		mergeHeartbeatRunUpdates(tasks, updates)
		if err := e.writeTasks(tasks, expected, true); err != nil {
			if errors.Is(err, ErrHeartbeatConfigConflict) {
				continue
			}
			log.Printf("[heartbeat] run-state merge failed: %v", err)
			return
		}
		latest, err := e.readConfigSnapshot()
		if err != nil {
			log.Printf("[heartbeat] reload after run-state merge: %v", err)
			return
		}
		e.recordConfigSnapshotLocked(latest)
		e.tasks = tasks
		e.prunePendingTopicsLocked(tasks)
		return
	}
	log.Printf("[heartbeat] run-state merge lost repeated config races; next tick will retry")
}

func mergeHeartbeatRunUpdates(tasks []HeartbeatTask, updates map[string]HeartbeatTask) {
	for i := range tasks {
		update, ok := updates[tasks[i].ID]
		if !ok {
			continue
		}

		newerRun := update.LastRunAt > tasks[i].LastRunAt
		if update.TopicID != "" && (tasks[i].TopicID == "" || newerRun) {
			tasks[i].TopicID = update.TopicID
		}
		if newerRun {
			tasks[i].LastRunAt = update.LastRunAt
		}
		if tasks[i].CreatedAt == 0 && update.CreatedAt != 0 {
			tasks[i].CreatedAt = update.CreatedAt
		}
	}
}

// HeartbeatListTasks returns all heartbeat tasks.
func (a *App) HeartbeatListTasks() []HeartbeatTask {
	if a.heartbeat == nil {
		return []HeartbeatTask{}
	}
	return a.heartbeat.ListTasks()
}

// HeartbeatReloadTasks reloads tasks from disk and returns them.
func (a *App) HeartbeatReloadTasks() []HeartbeatTask {
	if a.heartbeat == nil {
		return []HeartbeatTask{}
	}
	return a.heartbeat.ReloadTasks()
}
