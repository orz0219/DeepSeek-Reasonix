package memory

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
)

// Index returns the provider-visible index that loads into the cached
// prefix: every active fact from both scopes with a scope-qualified
// reference, shadowed global facts annotated rather than hidden — the index
// agrees with the project-over-global rule recall enforces (#7995). The
// per-directory MEMORY.md files keep their unqualified format.
func (s Store) Index() string {
	memories := s.ListAll()
	if len(memories) == 0 {
		return ""
	}
	shadowed := map[string]string{}
	for _, o := range FindOverrides(memories) {
		shadowed[o.Global.ID] = providerMemoryReference(o.Project)
	}
	sort.SliceStable(memories, func(i, j int) bool {
		if memories[i].Name != memories[j].Name {
			return memories[i].Name < memories[j].Name
		}
		return NormalizeFactScope(string(memories[i].Scope)) == FactScopeProject
	})
	var b strings.Builder
	seen := map[string]bool{}
	for _, memory := range memories {
		if ref := providerMemoryReference(memory); seen[ref] {
			continue
		} else {
			seen[ref] = true
		}
		b.WriteString(renderQualifiedIndexLine(memory))
		if winner, ok := shadowed[memory.ID]; ok &&
			NormalizeFactScope(string(memory.Scope)) == FactScopeGlobal {
			b.WriteString(" (overridden by " + winner + ")")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderQualifiedIndexLine is the provider-index variant of renderIndexLine:
// the link is the scope-qualified reference every memory tool accepts, so a
// name collision across scopes can never be misread.
func renderQualifiedIndexLine(m Memory) string {
	marker := ""
	if ResolveActivation(m) == ActivationPinned {
		marker = " pinned"
	}
	ref := providerMemoryReference(m)
	return fmt.Sprintf("- [%s](%s) — [%s/%s%s] %s",
		displayTitle(m.Title, m.Name), ref,
		NormalizeFactScope(string(m.Scope)), NormalizeType(string(m.Type)), marker, oneLine(m.Description))
}

// indexFile is the human-readable index of saved memories.
const indexFile = "MEMORY.md"

// indexLineRe matches a managed index line so reindex/Delete can target the line
// for one memory by its filename without disturbing the rest of a hand-edited
// MEMORY.md.
var indexLineRe = regexp.MustCompile(`(?m)^\s*-\s\[.+?\]\(([^)]+)\.md\)\s*—\s.*$`)

// indexLinesExceptIn returns the managed MEMORY.md lines keyed by filename stem
// in the given directory, dropping the entry for name (a missing index → empty map).
func indexLinesExceptIn(dir, name string) map[string]string {
	existing, _ := fileencoding.ReadFileUTF8(filepath.Join(dir, indexFile))
	keep := map[string]string{}
	for line := range strings.SplitSeq(string(existing), "\n") {
		if mt := indexLineRe.FindStringSubmatch(line); mt != nil && mt[1] != name {
			keep[mt[1]] = strings.TrimRight(line, "\r")
		}
	}
	return keep
}

func indexContainsIn(dir, name string) bool {
	existing, err := fileencoding.ReadFileUTF8(filepath.Join(dir, indexFile))
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(existing), "\n") {
		if mt := indexLineRe.FindStringSubmatch(line); mt != nil && mt[1] == name {
			return true
		}
	}
	return false
}

// flushIndexIn rewrites MEMORY.md in the given directory from the managed lines,
// preserving hand-written content. Managed lines are updated or removed, and
// new managed entries are appended in sorted order.
func flushIndexIn(dir string, lines map[string]string) error {
	path := filepath.Join(dir, indexFile)
	existing, _ := fileencoding.ReadFileUTF8(path)
	processed := map[string]bool{}
	var preserved strings.Builder
	preservedEmpty := true
	for line := range strings.SplitSeq(string(existing), "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if mt := indexLineRe.FindStringSubmatch(trimmed); mt != nil {
			name := mt[1]
			if fresh, ok := lines[name]; ok {
				preserved.WriteString(fresh)
				preserved.WriteString("\n")
				processed[name] = true
				preservedEmpty = false
			}
			continue
		}
		preserved.WriteString(trimmed)
		preserved.WriteString("\n")
		if strings.TrimSpace(trimmed) != "" {
			preservedEmpty = false
		}
	}

	names := make([]string, 0, len(lines))
	for n := range lines {
		if !processed[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	var b strings.Builder
	if preservedEmpty && len(names) > 0 {
		b.WriteString("# Memory\n\n")
	} else {
		b.WriteString(preserved.String())
	}
	for _, n := range names {
		b.WriteString(lines[n])
		b.WriteString("\n")
	}
	result := strings.TrimRight(b.String(), "\n")
	if result != "" {
		result += "\n"
	}

	return fileutil.AtomicWriteFile(path, []byte(result), 0o644)
}

// reindexIn rewrites the MEMORY.md line for name in the given directory,
// preserving every other managed line.
func reindexIn(dir, name string, m Memory) error {
	lines := indexLinesExceptIn(dir, name)
	lines[name] = renderIndexLine(name, m)
	return flushIndexIn(dir, lines)
}

func renderIndexLine(name string, m Memory) string {
	marker := ""
	if ResolveActivation(m) == ActivationPinned {
		marker = " pinned"
	}
	return fmt.Sprintf("- [%s](%s.md) — [%s/%s%s] %s",
		displayTitle(m.Title, name), name,
		NormalizeFactScope(string(m.Scope)), NormalizeType(string(m.Type)), marker, oneLine(m.Description))
}
