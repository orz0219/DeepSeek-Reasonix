package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/frontmatter"
)

const (
	skillFrontmatterDescription      = "description"
	skillFrontmatterName             = "name"
	skillFrontmatterRunAs            = "runas"
	skillFrontmatterContext          = "context"
	skillFrontmatterAgent            = "agent"
	skillFrontmatterAllowedTools     = "allowed-tools"
	skillFrontmatterModel            = "model"
	skillFrontmatterEffort           = "effort"
	skillFrontmatterReadOnly         = "read-only"
	skillFrontmatterTriggers         = "triggers"
	skillFrontmatterNegativeTriggers = "negative-triggers"
	skillFrontmatterAutoUse          = "auto-use"
	skillFrontmatterNeedsFreshData   = "needs-fresh-data"
	skillFrontmatterCost             = "cost"
	skillFrontmatterColor            = "color"
	skillFrontmatterInvocation       = "invocation"
	skillFrontmatterRequires         = "requires"
	skillFrontmatterProfiles         = "profiles"
)

var skillMarkerFrontmatterKeys = []string{
	skillFrontmatterDescription,
	skillFrontmatterName,
	skillFrontmatterRunAs,
	skillFrontmatterContext,
	skillFrontmatterAgent,
	skillFrontmatterAllowedTools,
	skillFrontmatterModel,
	skillFrontmatterEffort,
	skillFrontmatterReadOnly,
	skillFrontmatterTriggers,
	skillFrontmatterNegativeTriggers,
	skillFrontmatterAutoUse,
	skillFrontmatterNeedsFreshData,
	skillFrontmatterCost,
	skillFrontmatterColor,
	skillFrontmatterInvocation,
	skillFrontmatterRequires,
	skillFrontmatterProfiles,
}

func hasSkillMarker(content string, fm map[string]string) bool {
	for _, key := range skillMarkerFrontmatterKeys {
		if strings.TrimSpace(fm[key]) != "" {
			return true
		}
	}
	return frontmatterHasSkillMarkerKey(content)
}

func frontmatterHasSkillMarkerKey(content string) bool {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return false
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return false
	}
	for _, line := range lines[1:end] {
		key, _, ok := strings.Cut(line, ":")
		if ok && isSkillMarkerFrontmatterKey(strings.ToLower(strings.TrimSpace(key))) {
			return true
		}
	}
	return false
}

func isSkillMarkerFrontmatterKey(key string) bool {
	return slices.Contains(skillMarkerFrontmatterKeys, key)
}

// Create scaffolds a new skill stub at the chosen scope. Refuses to overwrite.
func (s *Store) Create(name string, scope Scope) (string, error) {
	return s.CreateWithContent(name, scope, stubBody(name))
}

// CreateWithContent writes caller-supplied file contents as a canonical
// <name>/SKILL.md skill, refusing to clobber an existing directory-layout or
// legacy flat skill of the same name. Returns the written path.
func (s *Store) CreateWithContent(name string, scope Scope, content string) (string, error) {
	if !IsValidName(name) {
		return "", fmt.Errorf("invalid skill name %q — use letters, digits, '_', '-', '.'", name)
	}
	var root string
	switch scope {
	case ScopeProject:
		if s.projectRoot == "" {
			return "", fmt.Errorf("project scope requires a workspace — run from a project directory, or use global scope")
		}
		root = filepath.Join(s.projectRoot, ".reasonix", SkillsDirname)
	default:
		root = s.globalSkillsRoot()
	}
	flat := filepath.Join(root, name+".md")
	folder := filepath.Join(root, name, SkillFile)
	if _, err := os.Stat(flat); err == nil {
		return "", fmt.Errorf("skill %q already exists at %s", name, flat)
	}
	if _, err := os.Stat(folder); err == nil {
		return "", fmt.Errorf("skill %q already exists at %s", name, folder)
	}
	if err := os.MkdirAll(filepath.Dir(folder), 0o755); err != nil {
		return "", err
	}

	f, err := os.OpenFile(folder, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("skill %q already exists at %s", name, folder)
		}
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		return "", err
	}
	return folder, nil
}

// UpdateContent overwrites an existing user-authored skill's file contents in
// place. Refuses built-ins and a scope mismatch, mirroring Delete's rules —
// see Delete for why a mismatch must refuse rather than silently target the
// wrong file.
func (s *Store) UpdateContent(name string, scope Scope, content string) error {
	if scope == ScopeBuiltin {
		return fmt.Errorf("skill %q is built in and cannot be edited", name)
	}
	sk, ok := s.Read(name)
	if !ok {
		return fmt.Errorf("skill %q not found", name)
	}
	if sk.Scope != scope {
		return fmt.Errorf("skill %q resolves at scope %q, not %q — refusing to edit a different scope's file", name, sk.Scope, scope)
	}
	if sk.Path == "" || sk.Path == "(builtin)" {
		return fmt.Errorf("skill %q has no file to update", name)
	}
	if err := s.validateMutablePath(sk.Path, scope); err != nil {
		return fmt.Errorf("skill %q cannot be edited: %w", name, err)
	}
	info, err := os.Stat(sk.Path)
	if err != nil {
		return err
	}
	return fileutil.AtomicWriteFile(sk.Path, []byte(content), info.Mode().Perm())
}

// validateMutablePath rejects writes through linked files or directories. Skill
// discovery intentionally follows symlinks for read compatibility, but editing
// one must never replace content outside the configured scope root.
func (s *Store) validateMutablePath(path string, scope Scope) error {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for _, root := range s.roots() {
		if root.Scope != scope {
			continue
		}
		absRoot, err := filepath.Abs(root.Dir)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(absRoot, absPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		current := absRoot
		parts := []string{"."}
		if rel != "." {
			parts = strings.Split(rel, string(filepath.Separator))
		}
		for _, part := range parts {
			if part != "." {
				current = filepath.Join(current, part)
			}
			info, err := os.Lstat(current)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("path uses symbolic link %s", current)
			}
		}
		realRoot, err := filepath.EvalSymlinks(absRoot)
		if err != nil {
			return err
		}
		realPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return err
		}
		realRel, err := filepath.Rel(realRoot, realPath)
		if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("resolved path is outside scope root %s", absRoot)
		}
		return nil
	}
	return fmt.Errorf("path is outside configured %s skill roots", scope)
}

// Delete removes a user-authored skill. Refuses built-ins (no file backs
// them) and refuses when the resolved skill's actual scope doesn't match the
// requested one — e.g. a project-scope delete for a name that only resolves
// at global scope, which would otherwise silently no-op against the wrong
// file while a same-named project-scope shadow kept showing up in List().
func (s *Store) Delete(name string, scope Scope) error {
	if scope == ScopeBuiltin {
		return fmt.Errorf("skill %q is built in and cannot be deleted", name)
	}
	sk, ok := s.Read(name)
	if !ok {
		return fmt.Errorf("skill %q not found", name)
	}
	if sk.Scope != scope {
		return fmt.Errorf("skill %q resolves at scope %q, not %q — refusing to delete a different scope's file", name, sk.Scope, scope)
	}
	if sk.Path == "" || sk.Path == "(builtin)" {
		return fmt.Errorf("skill %q has no file to delete", name)
	}
	if filepath.Base(sk.Path) == SkillFile {
		return os.RemoveAll(filepath.Dir(sk.Path))
	}
	return os.Remove(sk.Path)
}

func (s *Store) globalSkillsRoot() string {
	if s.reasonixHomeDir != "" {
		return filepath.Join(s.reasonixHomeDir, SkillsDirname)
	}
	return filepath.Join(s.homeDir, ".reasonix", SkillsDirname)
}

// loadBodyWithReferences appends a directory-layout skill's sibling
// references/*.md files to its body (Anthropic Skills compatibility), so depth
// material is available without on-demand resolution. Flat skills have no
// references dir and are returned unchanged.
func loadBodyWithReferences(skillPath, body string) string {
	if filepath.Base(skillPath) != SkillFile {
		return body
	}
	refsDir := filepath.Join(filepath.Dir(skillPath), "references")
	entries, err := os.ReadDir(refsDir)
	if err != nil {
		return body
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".md") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return body
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(body)
	for _, n := range names {
		content, err := fileencoding.ReadFileUTF8(filepath.Join(refsDir, n))
		if err != nil {
			continue
		}
		trimmed := strings.TrimSpace(string(content))
		if trimmed == "" {
			continue
		}
		slug := strings.TrimSuffix(n, filepath.Ext(n))
		b.WriteString("\n\n## Reference: " + slug + "\n\n" + trimmed)
	}
	return b.String()
}

// loadBodyWithScripts appends a directory-layout skill's sibling scripts/
// directory listing to the body, so the model knows what scripts are
// available and can run them via bash (inheriting sandbox, gate, hooks).
func loadBodyWithScripts(skillPath, body string) string {
	if filepath.Base(skillPath) != SkillFile {
		return body
	}
	scriptsDir := filepath.Join(filepath.Dir(skillPath), "scripts")
	entries, err := os.ReadDir(scriptsDir)
	if err != nil {
		return body
	}
	var names []string
	for _, e := range entries {

		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !isScriptExt(filepath.Ext(e.Name())) {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return body
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n\n## Scripts\n\nRun a listed script with bash using the exact path shown below; quote the path if it contains spaces.\n\n")
	for _, n := range names {
		b.WriteString("- `" + filepath.Join(scriptsDir, n) + "`\n")
	}
	return b.String()
}

func isScriptExt(ext string) bool {
	switch strings.ToLower(ext) {
	case "", ".sh", ".py", ".js", ".ts", ".rb", ".pl", ".php", ".ps1":
		return true
	default:
		return false
	}
}

// parseAllowedTools splits a comma-separated `allowed-tools` value into trimmed,
// non-empty tool names; nil when absent.
func parseAllowedTools(raw string) []string {
	return parseCSVFrontmatter(raw)
}

// parseCSVFrontmatter splits simple comma-separated frontmatter values. Full
// YAML lists are intentionally out of scope for the existing frontmatter parser.
func parseCSVFrontmatter(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		raw = strings.TrimSpace(raw[1 : len(raw)-1])
	}
	var out []string
	for p := range strings.SplitSeq(raw, ",") {
		if t := strings.Trim(strings.TrimSpace(p), `"'`); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func parseAutoUse(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "off", "suggest", "prefer", "require":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

// parseProfilesFrontmatter keeps only economy|balanced|delivery values and
// returns the rejected ones separately so doctor can surface typos instead of
// the parser hiding them.
func parseProfilesFrontmatter(raw string) (valid, invalid []string) {
	seen := map[string]bool{}
	for _, p := range parseCSVFrontmatter(raw) {
		p = strings.ToLower(strings.TrimSpace(p))
		switch p {
		case "economy", "balanced", "delivery":
			if !seen[p] {
				seen[p] = true
				valid = append(valid, p)
			}
		case "":
		default:
			if !seen[p] {
				seen[p] = true
				invalid = append(invalid, p)
			}
		}
	}
	return valid, invalid
}

func parseBoolFrontmatter(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "yes", "1", "on":
		return true
	default:
		return false
	}
}

func parseCost(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

// parseInvocation maps frontmatter to an invocation mode. Anything other than
// "manual" (including absent) is "auto" — the existing, universal behavior.
func parseInvocation(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), "manual") {
		return "manual"
	}
	return "auto"
}

// parseRunAs maps frontmatter to a run mode. An unknown value defaults to the
// safe (non-spawning) inline mode; a `context: fork` or a non-empty `agent:`
// field (cross-tool conventions) signals subagent isolation.
func parseRunAs(runAs, context, agent string) RunAs {
	if strings.TrimSpace(runAs) == "subagent" {
		return RunSubagent
	}
	if strings.EqualFold(strings.TrimSpace(context), "fork") {
		return RunSubagent
	}
	if strings.TrimSpace(agent) != "" {
		return RunSubagent
	}
	return RunInline
}

// stubBody is the scaffold written by `/skill new` — minimal frontmatter plus
// guidance the author fills in.
func stubBody(name string) string {
	return "---\nname: " + name + "\ndescription: One-liner — what does this skill do?\n---\n\n# " + name + `

Replace this body with the playbook the model should follow when this skill is invoked.

Tips:
- Reference tools by name (bash, edit_file, grep, read_file, ...)
- Add ` + "`runAs: subagent`" + ` to frontmatter to spawn an isolated subagent loop
- Add ` + "`allowed-tools: read_file, grep`" + ` to scope a subagent's tools
`
}

// resolveCustomPaths expands "~" and makes each custom path absolute relative to
// baseDir.
func resolveCustomPaths(paths []string, baseDir, homeDir string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		switch {
		case trimmed == "~":
			trimmed = homeDir
		case strings.HasPrefix(trimmed, "~/") || strings.HasPrefix(trimmed, `~\`):
			trimmed = filepath.Join(homeDir, trimmed[2:])
		}
		if !filepath.IsAbs(trimmed) {
			trimmed = filepath.Join(baseDir, trimmed)
		}
		out = append(out, filepath.Clean(trimmed))
	}
	return out
}

// dedupePaths drops duplicate custom roots, preserving order.
func dedupePaths(paths []string) []string {
	seen := map[string]bool{}
	out := paths[:0]
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// splitFrontmatter is a thin wrapper kept for internal use; the real parser
// lives in internal/frontmatter.
func splitFrontmatter(s string) (map[string]string, string) {
	return frontmatter.Split(s)
}
