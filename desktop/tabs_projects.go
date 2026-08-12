package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

func normalizeProjectsFile(f desktopProjectFile) desktopProjectFile {
	out := desktopProjectFile{
		GlobalTitle:        strings.TrimSpace(f.GlobalTitle),
		GlobalColor:        normalizeProjectColor(f.GlobalColor),
		GlobalTopics:       uniqueStrings(f.GlobalTopics),
		GlobalPinnedTopics: uniqueStrings(f.GlobalPinnedTopics),
		DeletedTopics:      uniqueStrings(f.DeletedTopics),
	}
	for _, p := range f.Projects {
		root := normalizeProjectRoot(p.Root)
		if root == "" {
			continue
		}
		p.Root = root
		p.Title = strings.TrimSpace(p.Title)
		p.Color = normalizeProjectColor(p.Color)
		p.Topics = uniqueStrings(p.Topics)
		p.PinnedTopics = uniqueStrings(p.PinnedTopics)
		if i := projectIndexByRoot(out.Projects, root); i >= 0 {
			if out.Projects[i].Title == "" && p.Title != "" {
				out.Projects[i].Title = p.Title
			}
			if out.Projects[i].Color == "" && p.Color != "" {
				out.Projects[i].Color = p.Color
			}
			out.Projects[i].Topics = uniqueStrings(append(out.Projects[i].Topics, p.Topics...))
			out.Projects[i].PinnedTopics = uniqueStrings(append(out.Projects[i].PinnedTopics, p.PinnedTopics...))
			continue
		}
		out.Projects = append(out.Projects, p)
	}
	for _, root := range uniqueStrings(f.PinnedProjects) {
		root = normalizeProjectRoot(root)
		if i := projectIndexByRoot(out.Projects, root); i >= 0 && !projectRootInList(out.PinnedProjects, out.Projects[i].Root) {
			out.PinnedProjects = append(out.PinnedProjects, out.Projects[i].Root)
		}
	}
	out.SidebarOrder = normalizeSidebarOrder(f.SidebarOrder, out.Projects)
	return out
}

func normalizeSidebarOrder(order []string, projects []desktopProject) []string {
	seenGlobal := false
	// Dedupe roots against a roots-only list: out also holds the global order
	// token, which must never be path-compared against project roots.
	var seenRoots []string
	out := make([]string, 0, len(order))
	for _, value := range order {
		value = strings.TrimSpace(value)
		if value == desktopGlobalOrderToken {
			if !seenGlobal {
				seenGlobal = true
				out = append(out, value)
			}
			continue
		}
		root := normalizeProjectRoot(value)
		i := projectIndexByRoot(projects, root)
		if i < 0 {
			continue
		}
		root = projects[i].Root
		if projectRootInList(seenRoots, root) {
			continue
		}
		seenRoots = append(seenRoots, root)
		out = append(out, root)
	}
	return out
}

func sameProjectOrder(a, b []desktopProject) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Root != b[i].Root {
			return false
		}
	}
	return true
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func prependUniqueString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return uniqueStrings(values)
	}
	return uniqueStrings(append([]string{value}, values...))
}

func removeString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return uniqueStrings(values)
	}
	out := make([]string, 0, len(values))
	for _, item := range uniqueStrings(values) {
		if item != value {
			out = append(out, item)
		}
	}
	return out
}

func containsDesktopString(values []string, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	return slices.Contains(uniqueStrings(values), value)
}

func pinnedTopicIDs(topicIDs []string, pinned []string) []string {
	if len(topicIDs) == 0 || len(pinned) == 0 {
		return topicIDs
	}
	available := make(map[string]bool, len(topicIDs))
	for _, tid := range topicIDs {
		available[tid] = true
	}
	out := make([]string, 0, len(topicIDs))
	seen := make(map[string]bool, len(topicIDs))
	for _, tid := range uniqueStrings(pinned) {
		if available[tid] && !seen[tid] {
			out = append(out, tid)
			seen[tid] = true
		}
	}
	for _, tid := range topicIDs {
		if !seen[tid] {
			out = append(out, tid)
		}
	}
	return out
}

func orderedTopicIDs(explicit []string, titleMap map[string]string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(explicit)+len(titleMap))
	for _, tid := range explicit {
		tid = strings.TrimSpace(tid)
		if tid == "" || seen[tid] {
			continue
		}
		seen[tid] = true
		out = append(out, tid)
	}
	var remaining []string
	for tid := range titleMap {
		if !seen[tid] {
			remaining = append(remaining, tid)
		}
	}
	sort.Strings(remaining)
	return append(out, remaining...)
}

func projectTreeOrderKey(node ProjectNode) string {
	switch node.Kind {
	case "global_folder":
		return desktopGlobalOrderToken
	case "project":
		return normalizeProjectRoot(node.Root)
	default:
		return ""
	}
}

func applyProjectTreeOrder(nodes []ProjectNode, order []string) []ProjectNode {
	if len(order) == 0 {
		return nodes
	}
	byKey := make(map[string]ProjectNode, len(nodes))
	for _, node := range nodes {
		key := projectTreeOrderKey(node)
		if key != "" {
			byKey[key] = node
		}
	}
	seen := make(map[string]bool, len(nodes))
	out := make([]ProjectNode, 0, len(nodes))
	for _, value := range order {
		key := strings.TrimSpace(value)
		if key != desktopGlobalOrderToken {
			key = normalizeProjectRoot(key)
		}
		if key == "" || seen[key] {
			continue
		}
		node, ok := byKey[key]
		if !ok {
			continue
		}
		seen[key] = true
		out = append(out, node)
	}
	for _, node := range nodes {
		key := projectTreeOrderKey(node)
		if key != "" && seen[key] {
			continue
		}
		if key != "" {
			seen[key] = true
		}
		out = append(out, node)
	}
	return out
}

func applyPinnedProjectOrder(nodes []ProjectNode, pinnedRoots []string) []ProjectNode {
	pinnedRoots = uniqueStrings(pinnedRoots)
	if len(pinnedRoots) == 0 {
		return nodes
	}
	byRoot := make(map[string]ProjectNode, len(nodes))
	for _, node := range nodes {
		if node.Kind == "project" && node.Root != "" {
			byRoot[normalizeProjectRoot(node.Root)] = node
		}
	}
	seen := make(map[string]bool, len(pinnedRoots))
	out := make([]ProjectNode, 0, len(nodes))
	for _, root := range pinnedRoots {
		root = normalizeProjectRoot(root)
		node, ok := byRoot[root]
		if !ok || seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, node)
	}
	for _, node := range nodes {
		if node.Kind == "project" && node.Root != "" && seen[normalizeProjectRoot(node.Root)] {
			continue
		}
		out = append(out, node)
	}
	return out
}

func projectDisplayName(p desktopProject) string {
	if title := strings.TrimSpace(p.Title); title != "" {
		return title
	}
	return workspaceName(p.Root)
}

func normalizeProjectColor(color string) string {
	switch strings.TrimSpace(strings.ToLower(color)) {
	case "red", "orange", "amber", "green", "teal", "blue", "purple", "pink":
		return strings.TrimSpace(strings.ToLower(color))
	default:
		return ""
	}
}

func projectColor(root string) string {
	root = normalizeProjectRoot(root)
	if root == "" {
		return globalProjectColor()
	}
	for _, p := range loadProjectsFile().Projects {
		if sameProjectRoot(p.Root, root) {
			return normalizeProjectColor(p.Color)
		}
	}
	return ""
}

func globalProjectColor() string {
	return normalizeProjectColor(loadProjectsFile().GlobalColor)
}

func globalProjectTitle() string {
	if title := strings.TrimSpace(loadProjectsFile().GlobalTitle); title != "" {
		return title
	}
	return "Global"
}

func addProject(root, title string) error {
	root = normalizeProjectRoot(root)
	if root == "" {
		return fmt.Errorf("project root is required")
	}
	title = strings.TrimSpace(title)
	return updateProjectsFile(func(f *desktopProjectFile) (bool, error) {
		for i, p := range f.Projects {
			if sameProjectRoot(p.Root, root) {
				changed := false
				if f.Projects[i].Root != root {
					f.Projects[i].Root = root
					changed = true
				}
				if title != "" && f.Projects[i].Title != title {
					f.Projects[i].Title = title
					changed = true
				}
				if !changed {
					return false, nil
				}
				return true, nil
			}
		}
		f.Projects = append(f.Projects, desktopProject{Root: root, Title: title})
		return true, nil
	})
}

func renameProject(root, title string) error {
	title = strings.TrimSpace(title)
	root = normalizeProjectRoot(root)
	return updateProjectsFile(func(f *desktopProjectFile) (bool, error) {
		if root == "" {
			if f.GlobalTitle == title {
				return false, nil
			}
			f.GlobalTitle = title
			return true, nil
		}
		for i, p := range f.Projects {
			if sameProjectRoot(p.Root, root) {
				if f.Projects[i].Root == root && f.Projects[i].Title == title {
					return false, nil
				}
				f.Projects[i].Root = root
				f.Projects[i].Title = title
				return true, nil
			}
		}
		f.Projects = append(f.Projects, desktopProject{Root: root, Title: title})
		return true, nil
	})
}

func setProjectColor(root, color string) error {
	root = normalizeProjectRoot(root)
	color = normalizeProjectColor(color)
	return updateProjectsFile(func(f *desktopProjectFile) (bool, error) {
		if root == "" {
			if f.GlobalColor == color {
				return false, nil
			}
			f.GlobalColor = color
			return true, nil
		}
		for i, p := range f.Projects {
			if sameProjectRoot(p.Root, root) {
				if f.Projects[i].Root == root && f.Projects[i].Color == color {
					return false, nil
				}
				f.Projects[i].Root = root
				f.Projects[i].Color = color
				return true, nil
			}
		}
		f.Projects = append(f.Projects, desktopProject{Root: root, Color: color})
		return true, nil
	})
}

func removeProject(root string) error {
	root = normalizeProjectRoot(root)
	return updateProjectsFile(func(f *desktopProjectFile) (bool, error) {
		projects := make([]desktopProject, 0, len(f.Projects))
		for _, p := range f.Projects {
			if !sameProjectRoot(p.Root, root) {
				projects = append(projects, p)
			}
		}
		if len(projects) == len(f.Projects) {
			return false, nil
		}
		f.Projects = projects
		return true, nil
	})
}
