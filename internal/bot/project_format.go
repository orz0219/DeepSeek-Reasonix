package bot

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func filterBotProjects(projects []botProjectEntry, query string) []botProjectEntry {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return projects
	}
	var out []botProjectEntry
	for _, project := range projects {
		if strings.Contains(strings.ToLower(project.Name+" "+project.Root+" "+strings.Join(project.Sources, " ")), query) {
			out = append(out, project)
		}
	}
	return out
}

func filterBotSessions(sessions []botSessionEntry, query string) []botSessionEntry {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return sessions
	}
	var out []botSessionEntry
	for _, session := range sessions {
		if botSessionMatchesQuery(session, query) {
			out = append(out, session)
		}
	}
	return out
}

func botSessionMatchesQuery(session botSessionEntry, query string) bool {
	haystack := strings.ToLower(strings.Join([]string{
		session.ID,
		session.ProjectID,
		session.ProjectName,
		session.WorkspaceRoot,
		session.SessionID,
		session.SessionPath,
		session.RemoteID,
		session.ConnectionID,
		session.ChatType,
		session.UserID,
		session.ThreadID,
		session.Scope,
		session.Preview,
		session.TopicTitle,
		session.Source,
	}, " "))
	return strings.Contains(haystack, query)
}

func formatBotProjects(projects []botProjectEntry, query string, limit int) string {
	matches := filterBotProjects(projects, query)
	if len(matches) == 0 {
		if strings.TrimSpace(query) == "" {
			return "还没有可用项目索引。请先在 bot 连接、route 或当前会话里配置 workspace_root。"
		}
		return "没有匹配的项目。"
	}
	if limit <= 0 || limit > len(matches) {
		limit = len(matches)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "项目索引（%d/%d）：", limit, len(matches))
	for i := range limit {
		project := matches[i]
		fmt.Fprintf(&b, "\n%s %s — %s", project.ID, project.Name, displayBotPath(project.Root))
		if len(project.Sources) > 0 {
			fmt.Fprintf(&b, "\n  来源: %s", strings.Join(project.Sources, ", "))
		}
	}
	if len(matches) > limit {
		fmt.Fprintf(&b, "\n还有 %d 个结果，请加关键词缩小范围。", len(matches)-limit)
	}
	return b.String()
}

func formatBotSessions(sessions []botSessionEntry, query string, limit int) string {
	matches := filterBotSessions(sessions, query)
	if len(matches) == 0 {
		if strings.TrimSpace(query) == "" {
			return "还没有可用会话索引。已有项目会话或 bot session_mappings 后会出现在这里。"
		}
		return "没有匹配的会话。"
	}
	if limit <= 0 || limit > len(matches) {
		limit = len(matches)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "会话索引（%d/%d）：", limit, len(matches))
	for i := range limit {
		session := matches[i]
		project := firstNonEmptyString(session.ProjectName, "global")
		fmt.Fprintf(&b, "\n%s %s", session.ID, project)
		if session.TopicTitle != "" {
			fmt.Fprintf(&b, " · %s", singleLineBotText(session.TopicTitle, 40))
		}
		if session.Preview != "" {
			fmt.Fprintf(&b, "\n  预览: %s", singleLineBotText(session.Preview, 90))
		}
		if session.SessionPath != "" {
			fmt.Fprintf(&b, "\n  文件: %s", displayBotPath(session.SessionPath))
		} else if session.SessionID != "" {
			fmt.Fprintf(&b, "\n  目标: %s", session.SessionID)
		}
		if session.RemoteID != "" || session.ConnectionID != "" {
			fmt.Fprintf(&b, "\n  远端: %s %s", session.ConnectionID, session.RemoteID)
		}
	}
	if len(matches) > limit {
		fmt.Fprintf(&b, "\n还有 %d 个结果，请加关键词缩小范围。", len(matches)-limit)
	}
	return b.String()
}

func botProjectForPath(projects []botProjectEntry, path string) botProjectEntry {
	path = canonicalBotPath(path)
	var best botProjectEntry
	for _, project := range projects {
		root := canonicalBotPath(project.Root)
		if root == "" {
			continue
		}
		if path == root || strings.HasPrefix(path, root+string(os.PathSeparator)) {
			if len(root) > len(best.Root) {
				best = project
			}
		}
	}
	return best
}

func canonicalBotPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	return filepath.Clean(path)
}

func botProjectName(root string) string {
	root = strings.TrimRight(canonicalBotPath(root), string(os.PathSeparator))
	if root == "" {
		return ""
	}
	name := filepath.Base(root)
	if name == "." || name == string(os.PathSeparator) {
		return root
	}
	return name
}

func displayBotPath(path string) string {
	path = canonicalBotPath(path)
	home, err := os.UserHomeDir()
	if err == nil {
		home = canonicalBotPath(home)
		if home != "" && (path == home || strings.HasPrefix(path, home+string(os.PathSeparator))) {
			return "~" + strings.TrimPrefix(path, home)
		}
	}
	return path
}

func singleLineBotText(text string, limit int) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}

func shortBotID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return value
	}
	return value[:8]
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
