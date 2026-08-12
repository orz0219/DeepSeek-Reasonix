package cli

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"reasonix/internal/control"
	"reasonix/internal/fileref"
	"reasonix/internal/i18n"
)

// fileItems lists one directory level for a path token. dir is the part up to
// the last '/', frag the part after; entries of dir starting with frag are
// offered (directories descend, files complete). Hidden entries are skipped
// unless frag starts with '.'. Top-level tokens also surface MCP resources.
func (m *chatTUI) fileItems(token string) []compItem {
	dir, frag := splitPathToken(token)

	fsFrag := control.UnescapeRefPath(frag)
	workspaceRoot := ""
	if m.ctrl != nil {
		workspaceRoot = m.ctrl.WorkspaceRoot()
	}
	readDir := control.UnescapeRefPath(dir)
	if workspaceRoot != "" {
		if readDir == "" {
			readDir = workspaceRoot
		} else if !filepath.IsAbs(readDir) {
			readDir = filepath.Join(workspaceRoot, filepath.FromSlash(readDir))
		}
	} else if readDir == "" {
		readDir = "."
	}
	entries, err := os.ReadDir(readDir)
	if err != nil {
		entries = nil
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].IsDir() && !entries[j].IsDir()
	})

	showHidden := strings.HasPrefix(fsFrag, ".")
	var items []compItem
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, fsFrag) {
			continue
		}
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			items = append(items, compItem{label: name + "/", insert: "@" + dir + control.EscapeRefPath(name) + "/", hint: "dir", descend: true})
		} else {
			items = append(items, compItem{label: name, insert: "@" + dir + control.EscapeRefPath(name)})
		}
		if len(items) >= maxCompItems {
			break
		}
	}

	if !strings.Contains(token, "/") {
		seen := map[string]bool{}
		for _, it := range items {
			seen[strings.TrimPrefix(it.insert, "@")] = true
		}
		remaining := min(maxCompItems-len(items), maxFileSearchItems)
		results := m.searchFileRefs(fsFrag)
		if len(results) > remaining {
			results = results[:remaining]
		}
		for _, path := range results {
			escaped := control.EscapeRefPath(path)
			if seen[escaped] {
				continue
			}
			items = append(items, compItem{label: path, insert: "@" + escaped, hint: "file"})
			if len(items) >= maxCompItems {
				break
			}
		}
		items = append(items, m.resourceItems("", token)...)
	}
	return items
}

// searchFileRefs memoizes the bounded basename walk so re-rendering the menu
// for an unchanged @token fragment doesn't re-walk the workspace each keystroke.
func (m *chatTUI) searchFileRefs(frag string) []string {
	if m.fileSearchCache == nil {
		m.fileSearchCache = map[string][]string{}
	}
	if r, ok := m.fileSearchCache[frag]; ok {
		return r
	}
	searchRoot := "."
	if m.ctrl != nil {
		if wr := m.ctrl.WorkspaceRoot(); wr != "" {
			searchRoot = wr
		}
	}
	results := fileref.Search(searchRoot, frag, maxFileSearchItems)
	paths := make([]string, 0, len(results))
	for _, r := range results {
		paths = append(paths, r.Path)
	}
	m.fileSearchCache[frag] = paths
	return paths
}

// splitPathToken splits a path token into (dir, frag): dir keeps its trailing
// slash ("internal/" ), frag is the segment being typed.
func splitPathToken(token string) (dir, frag string) {
	if i := strings.LastIndex(token, "/"); i >= 0 {
		return token[:i+1], token[i+1:]
	}
	return "", token
}

// isMCPServer reports whether name is a connected MCP server.
func (m *chatTUI) isMCPServer(name string) bool {
	if m.host == nil {
		return false
	}
	return slices.Contains(m.host.ServerNames(), name)
}

// resourceItems lists MCP resources as @server:uri completions. When server is
// "" (top level) it matches by the whole "server:uri" prefix; otherwise it lists
// the named server's resources filtered by the uri prefix.
func (m *chatTUI) resourceItems(server, frag string) []compItem {
	if m.host == nil {
		return nil
	}
	var items []compItem
	for _, r := range m.host.Resources() {
		ref := r.Server + ":" + r.URI
		switch {
		case server == "":
			if !strings.HasPrefix(ref, frag) {
				continue
			}
		case r.Server == server:
			if !strings.HasPrefix(r.URI, frag) {
				continue
			}
		default:
			continue
		}
		label := r.Name
		if label == "" {
			label = "resource"
		}
		items = append(items, compItem{label: "@" + ref, insert: "@" + ref, hint: label})
	}
	return items
}

// moveCompletion advances the selection by delta, wrapping around.
func (m *chatTUI) moveCompletion(delta int) {
	n := len(m.completion.items)
	if n == 0 {
		return
	}
	m.completion.sel = ((m.completion.sel+delta)%n + n) % n
}

func (m *chatTUI) completionExactLabel() bool {
	if !m.completion.active || m.completion.sel >= len(m.completion.items) {
		return false
	}
	val := strings.TrimSpace(m.input.Value())
	return val == m.completion.items[m.completion.sel].label
}

func (m *chatTUI) completionBareOverlayCommand() bool {
	switch strings.TrimSpace(m.input.Value()) {
	case "/mcp", "/skills":
		return true
	default:
		return false
	}
}

func (m *chatTUI) completionSelectedInsertPresent() bool {
	if !m.completion.active || m.completion.sel >= len(m.completion.items) {
		return false
	}
	val := m.input.Value()
	rf, rt := m.completion.replaceFrom, m.completion.replaceTo
	if rf < 0 || rf > len(val) {
		return false
	}
	if rt < rf || rt > len(val) {
		rt = len(val)
	}
	return val[rf:rt] == m.completion.items[m.completion.sel].insert
}

// acceptCompletion applies the selected item to the input, then recomputes the
// menu from the new value: it re-opens one level deeper (a descended directory
// or a freshly completed command's arguments) or closes when nothing applies.
// Cursor moves to the end of the inserted token only on accept — ordinary
// keystrokes never call this path, so mid-line typing keeps its caret.
func (m *chatTUI) acceptCompletion() {
	if m.completion.sel >= len(m.completion.items) {
		m.completion = completion{}
		return
	}
	it := m.completion.items[m.completion.sel]
	val := m.input.Value()
	rf := m.completion.replaceFrom
	rt := m.completion.replaceTo
	if rf < 0 || rf > len(val) {
		rf = 0
	}

	if rt < rf || rt > len(val) {
		rt = len(val)
	} else if rt == rf && rf == 0 && len(val) > 0 {

		rt = len(val)
	}

	newVal := val[:rf] + it.insert + val[rt:]
	insertEnd := rf + len(it.insert)
	m.input.SetValue(newVal)

	if m.width > 0 {
		m.setComposerCursor(len([]rune(newVal[:min(insertEnd, len(newVal))])))
	} else {
		m.input.CursorEnd()
	}
	if it.descend || strings.HasSuffix(it.insert, " ") {
		m.updateCompletion()
		return
	}
	m.updateCompletion()

	if m.completion.active && len(m.completion.items) == 1 {
		rf, rt := m.completion.replaceFrom, m.completion.replaceTo
		val := m.input.Value()
		if rf >= 0 && rf <= len(val) && rt >= rf && rt <= len(val) {
			if val[rf:rt] == m.completion.items[0].insert {
				m.completion = completion{}
			}
		}
	}
}

var compSelStyle lipgloss.Style

const completionPadCell = "\u00a0"

// padCompletionLine pads completion rows with NBSPs instead of ASCII spaces.
// Ultraviolet treats trailing ASCII spaces as clearable cells and may emit EL
// or ECH erase sequences; mintty can leave stale CJK glyph cells after those
// erases. NBSP is visually blank but forces the renderer to overwrite cells.
func padCompletionLine(s string, w int) string {
	pad := w - visibleWidth(s)
	if pad <= 0 {
		return s
	}
	return s + strings.Repeat(completionPadCell, pad)
}

// renderCompletion draws the menu above the input box: matching items, windowed
// around the selection, the current row highlighted, hints dimmed. Every line is
// padded to m.width with non-clearable blank cells so bubbletea's delta renderer
// has no ordinary trailing-space run to collapse into EL/ECH erase sequences.
// That avoids ghost cells on terminals (mintty) with unreliable erases after
// wide CJK glyphs.
func (m chatTUI) renderCompletion() string {
	if !m.completion.active || len(m.completion.items) == 0 {
		return ""
	}
	items := m.completion.items
	start := 0
	if len(items) > maxCompRows {
		start = min(max(m.completion.sel-maxCompRows/2, 0), len(items)-maxCompRows)
	}
	end := min(start+maxCompRows, len(items))

	var b strings.Builder
	for i := start; i < end; i++ {
		it := items[i]
		var line string
		if i == m.completion.sel {
			line = accent("› ") + compSelStyle.Render(it.label)
		} else {
			line = "  " + it.label
		}
		if it.hint != "" {
			line += "  " + dim(it.hint)
		}
		b.WriteString(padCompletionLine(line, m.width))
		b.WriteByte('\n')
	}

	hint := i18n.M.CompHintSlash
	if m.completion.kind == compAt {
		hint = i18n.M.CompHintFile
	}
	b.WriteString(padCompletionLine(dim(hint), m.width))
	return b.String()
}
