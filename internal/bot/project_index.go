package bot

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"reasonix/internal/agent"
)

const (
	botProjectListLimit = 20
	botSessionListLimit = 20
	botSearchListLimit  = 20
)

type botProjectEntry struct {
	ID      string
	Name    string
	Root    string
	Sources []string
}

type botSessionEntry struct {
	ID             string
	ProjectID      string
	ProjectName    string
	WorkspaceRoot  string
	SessionID      string
	SessionPath    string
	RemoteID       string
	ConnectionID   string
	ChatType       string
	UserID         string
	ThreadID       string
	Scope          string
	Preview        string
	TopicTitle     string
	LastActivityAt time.Time
	Source         string
}

func (gw *BotGateway) buildProjectIndex() []botProjectEntry {
	collector := newBotProjectCollector()
	collector.add(gw.cfg.WorkspaceRoot, "default")

	// cfg.Channels / cfg.ConnectionChannels are rewritten under gw.mu at runtime,
	// so the whole scan shares the controllers critical section below.
	gw.mu.Lock()
	platforms := make([]string, 0, len(gw.cfg.Channels))
	for platform := range gw.cfg.Channels {
		platforms = append(platforms, string(platform))
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		channel := gw.cfg.Channels[Platform(platform)]
		source := "channel:" + platform
		collector.add(channel.WorkspaceRoot, source)
		addMappingProjects(collector, channel, source)
	}

	connections := make([]string, 0, len(gw.cfg.ConnectionChannels))
	for id := range gw.cfg.ConnectionChannels {
		connections = append(connections, id)
	}
	sort.Strings(connections)
	for _, id := range connections {
		channel := gw.cfg.ConnectionChannels[id]
		source := "connection:" + id
		collector.add(channel.WorkspaceRoot, source)
		addMappingProjects(collector, channel, source)
	}

	for i, route := range gw.cfg.Routes {
		collector.add(route.Channel.WorkspaceRoot, fmt.Sprintf("route:%d", i+1))
	}

	for key, state := range gw.controllers {
		root := ""
		if state != nil {
			root = state.workspaceRoot
			if root == "" && state.ctrl != nil {
				root = state.ctrl.WorkspaceRoot()
			}
		}
		collector.add(root, "active:"+shortBotID(key))
	}
	for key, override := range gw.sessionOverrides {
		collector.add(override.channel.WorkspaceRoot, "override:"+shortBotID(key))
	}
	gw.mu.Unlock()

	return collector.entries()
}

func addMappingProjects(collector *botProjectCollector, channel ChannelConfig, source string) {
	for _, mapping := range channel.SessionMappings {
		root := workspaceRootForSessionMapping(mapping, channel.WorkspaceRoot)
		collector.add(root, source+":mapping:"+strings.TrimSpace(mapping.RemoteID))
	}
}

type botProjectCollector struct {
	byRoot map[string]*botProjectEntry
}

func newBotProjectCollector() *botProjectCollector {
	return &botProjectCollector{byRoot: make(map[string]*botProjectEntry)}
}

func (c *botProjectCollector) add(root, source string) {
	root = canonicalBotPath(root)
	if root == "" {
		return
	}
	entry := c.byRoot[root]
	if entry == nil {
		entry = &botProjectEntry{Name: botProjectName(root), Root: root}
		c.byRoot[root] = entry
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return
	}
	if slices.Contains(entry.Sources, source) {
		return
	}
	entry.Sources = append(entry.Sources, source)
}

func (c *botProjectCollector) entries() []botProjectEntry {
	out := make([]botProjectEntry, 0, len(c.byRoot))
	for _, entry := range c.byRoot {
		copied := *entry
		sort.Strings(copied.Sources)
		out = append(out, copied)
	}
	sort.Slice(out, func(i, j int) bool {
		li := strings.ToLower(out[i].Name)
		lj := strings.ToLower(out[j].Name)
		if li != lj {
			return li < lj
		}
		return out[i].Root < out[j].Root
	})
	for i := range out {
		out[i].ID = fmt.Sprintf("p%d", i+1)
	}
	return out
}

func (gw *BotGateway) buildSessionIndex(projects []botProjectEntry) []botSessionEntry {
	projectByRoot := make(map[string]botProjectEntry, len(projects))
	for _, project := range projects {
		projectByRoot[canonicalBotPath(project.Root)] = project
	}
	collector := newBotSessionCollector(projectByRoot)

	// cfg.Channels / cfg.ConnectionChannels are rewritten under gw.mu at runtime;
	// collect the mapping-derived entries under a short lock, keeping the
	// filesystem scan below outside it.
	gw.mu.Lock()
	platforms := make([]string, 0, len(gw.cfg.Channels))
	for platform := range gw.cfg.Channels {
		platforms = append(platforms, string(platform))
	}
	sort.Strings(platforms)
	for _, platform := range platforms {
		channel := gw.cfg.Channels[Platform(platform)]
		addMappingSessions(collector, channel, "", "channel:"+platform)
	}

	connections := make([]string, 0, len(gw.cfg.ConnectionChannels))
	for id := range gw.cfg.ConnectionChannels {
		connections = append(connections, id)
	}
	sort.Strings(connections)
	for _, id := range connections {
		channel := gw.cfg.ConnectionChannels[id]
		addMappingSessions(collector, channel, id, "connection:"+id)
	}
	gw.mu.Unlock()

	for _, project := range projects {
		dir := botSessionDir(project.Root)
		if dir == "" {
			continue
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		infos, err := agent.ListSessions(dir)
		if err != nil {
			gw.logger.Warn("bot project session index failed", "project", project.Name, "err", err)
			continue
		}
		for _, info := range infos {
			collector.add(botSessionEntry{
				WorkspaceRoot:  project.Root,
				SessionPath:    canonicalBotPath(info.Path),
				SessionID:      botSessionTarget(info.Path),
				Scope:          firstNonEmptyString(info.Scope, "project"),
				Preview:        info.Preview,
				TopicTitle:     info.TopicTitle,
				LastActivityAt: info.LastActivityAt,
				Source:         "project-sessions",
			})
		}
	}

	return collector.entries()
}

func addMappingSessions(collector *botSessionCollector, channel ChannelConfig, connectionID, source string) {
	for _, mapping := range channel.SessionMappings {
		sessionID := strings.TrimSpace(mapping.SessionID)
		sessionPath := botSessionPathFromTarget(sessionID)
		root := workspaceRootForSessionMapping(mapping, channel.WorkspaceRoot)
		collector.add(botSessionEntry{
			WorkspaceRoot:  root,
			SessionID:      sessionID,
			SessionPath:    sessionPath,
			RemoteID:       strings.TrimSpace(mapping.RemoteID),
			ConnectionID:   strings.TrimSpace(connectionID),
			ChatType:       strings.TrimSpace(mapping.ChatType),
			UserID:         strings.TrimSpace(mapping.UserID),
			ThreadID:       strings.TrimSpace(mapping.ThreadID),
			Scope:          strings.TrimSpace(mapping.Scope),
			LastActivityAt: parseBotMappingUpdatedAt(mapping.UpdatedAt),
			Source:         source,
		})
	}
}

type botSessionCollector struct {
	projectByRoot map[string]botProjectEntry
	byKey         map[string]*botSessionEntry
}

func newBotSessionCollector(projectByRoot map[string]botProjectEntry) *botSessionCollector {
	return &botSessionCollector{
		projectByRoot: projectByRoot,
		byKey:         make(map[string]*botSessionEntry),
	}
}

func (c *botSessionCollector) add(entry botSessionEntry) {
	entry.WorkspaceRoot = canonicalBotPath(entry.WorkspaceRoot)
	entry.SessionPath = canonicalBotPath(entry.SessionPath)
	if entry.SessionID == "" && entry.SessionPath != "" {
		entry.SessionID = botSessionTarget(entry.SessionPath)
	}
	if entry.SessionPath == "" && entry.SessionID == "" && entry.RemoteID == "" {
		return
	}
	if project, ok := c.projectByRoot[entry.WorkspaceRoot]; ok {
		entry.ProjectID = project.ID
		entry.ProjectName = project.Name
	}
	key := entry.SessionPath
	if key == "" {
		key = strings.Join([]string{"target", entry.ConnectionID, entry.RemoteID, entry.ChatType, entry.UserID, entry.ThreadID, entry.SessionID}, "\x00")
	}
	existing := c.byKey[key]
	if existing == nil {
		c.byKey[key] = &entry
		return
	}
	mergeBotSessionEntry(existing, entry)
}

func mergeBotSessionEntry(dst *botSessionEntry, src botSessionEntry) {
	if dst.ProjectID == "" {
		dst.ProjectID = src.ProjectID
	}
	if dst.ProjectName == "" {
		dst.ProjectName = src.ProjectName
	}
	if dst.WorkspaceRoot == "" {
		dst.WorkspaceRoot = src.WorkspaceRoot
	}
	if dst.SessionID == "" {
		dst.SessionID = src.SessionID
	}
	if dst.SessionPath == "" {
		dst.SessionPath = src.SessionPath
	}
	if dst.RemoteID == "" {
		dst.RemoteID = src.RemoteID
	}
	if dst.ConnectionID == "" {
		dst.ConnectionID = src.ConnectionID
	}
	if dst.ChatType == "" {
		dst.ChatType = src.ChatType
	}
	if dst.UserID == "" {
		dst.UserID = src.UserID
	}
	if dst.ThreadID == "" {
		dst.ThreadID = src.ThreadID
	}
	if dst.Scope == "" {
		dst.Scope = src.Scope
	}
	if dst.Preview == "" {
		dst.Preview = src.Preview
	}
	if dst.TopicTitle == "" {
		dst.TopicTitle = src.TopicTitle
	}
	if src.LastActivityAt.After(dst.LastActivityAt) {
		dst.LastActivityAt = src.LastActivityAt
	}
	if dst.Source == "" {
		dst.Source = src.Source
	} else if src.Source != "" && !strings.Contains(dst.Source, src.Source) {
		dst.Source += "," + src.Source
	}
}

func (c *botSessionCollector) entries() []botSessionEntry {
	out := make([]botSessionEntry, 0, len(c.byKey))
	for _, entry := range c.byKey {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastActivityAt.Equal(out[j].LastActivityAt) {
			return out[i].LastActivityAt.After(out[j].LastActivityAt)
		}
		if out[i].ProjectName != out[j].ProjectName {
			return out[i].ProjectName < out[j].ProjectName
		}
		return out[i].SessionPath < out[j].SessionPath
	})
	for i := range out {
		out[i].ID = fmt.Sprintf("s%d", i+1)
	}
	return out
}

func botSessionPathFromTarget(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	if after, ok := strings.CutPrefix(target, "path:"); ok {
		return canonicalBotPath(after)
	}
	if filepath.IsAbs(target) && strings.HasSuffix(target, ".jsonl") {
		return canonicalBotPath(target)
	}
	return ""
}

func parseBotMappingUpdatedAt(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t
	}
	return time.Time{}
}

func resolveBotProject(projects []botProjectEntry, selector string) (botProjectEntry, []botProjectEntry) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return botProjectEntry{}, nil
	}
	selectorLower := strings.ToLower(selector)
	canonicalSelector := canonicalBotPath(selector)
	var matches []botProjectEntry
	for _, project := range projects {
		if strings.EqualFold(project.ID, selector) || canonicalBotPath(project.Root) == canonicalSelector || strings.EqualFold(project.Name, selector) {
			return project, nil
		}
		if strings.Contains(strings.ToLower(project.Name), selectorLower) || strings.Contains(strings.ToLower(project.Root), selectorLower) {
			matches = append(matches, project)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return botProjectEntry{}, matches
}

func resolveBotSession(sessions []botSessionEntry, selector string) (botSessionEntry, []botSessionEntry) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return botSessionEntry{}, nil
	}
	selectorLower := strings.ToLower(selector)
	canonicalSelector := canonicalBotPath(selector)
	var matches []botSessionEntry
	for _, session := range sessions {
		if strings.EqualFold(session.ID, selector) ||
			(session.SessionPath != "" && canonicalBotPath(session.SessionPath) == canonicalSelector) ||
			(session.SessionPath != "" && strings.EqualFold(filepath.Base(session.SessionPath), selector)) ||
			(session.SessionID != "" && strings.EqualFold(session.SessionID, selector)) {
			return session, nil
		}
		if botSessionMatchesQuery(session, selectorLower) {
			matches = append(matches, session)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return botSessionEntry{}, matches
}

// The desktop app hosts bot bridges in the GUI process; without this an
// rg search flashes a console window on Windows.
