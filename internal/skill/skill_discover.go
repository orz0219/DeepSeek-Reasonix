package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"reasonix/internal/config"
	fileencoding "reasonix/internal/fileutil/encoding"
)

// PathStatus describes a root directory's readability, surfaced by `/skill paths`.
type PathStatus string

// Root is one discovery directory with its scope, priority, and status.
type Root struct {
	Dir      string
	Scope    Scope
	Priority int
	Status   PathStatus
}

type discoveryRoot struct {
	Root
	requireFlatMarker bool
	plugins           []string
	forceSubagent     bool
}

// roots returns the discovery directories, highest priority first: the
// convention dirs (config.ConventionDirs: .reasonix / .agents / .agent / .claude)
// under the project root → custom paths → the Reasonix home skills dir → other
// home-dir convention dirs. A later root never overrides an earlier one.
func (s *Store) roots() []discoveryRoot {
	if s == nil || s.disableDiscovery {
		return nil
	}
	type de struct {
		dir               string
		scope             Scope
		requireFlatMarker bool
	}
	var dirs []de
	if s.projectRoot != "" {
		for _, c := range config.ConventionDirs {
			dirs = append(dirs, de{filepath.Join(s.projectRoot, c, SkillsDirname), ScopeProject, c == ".claude"})
		}
	}
	for _, d := range s.customPaths {
		dirs = append(dirs, de{d, ScopeCustom, false})
	}
	if s.reasonixHomeDir != "" {
		dirs = append(dirs, de{filepath.Join(s.reasonixHomeDir, SkillsDirname), ScopeGlobal, false})
	}
	if config.IsolatedHomeDir() == "" {
		for _, c := range config.ConventionDirs {
			dir := filepath.Join(s.homeDir, c, SkillsDirname)
			if s.reasonixHomeDir != "" && config.CanonicalSkillPath(filepath.Dir(dir)) == config.CanonicalSkillPath(s.reasonixHomeDir) {
				continue
			}
			dirs = append(dirs, de{dir, ScopeGlobal, c == ".claude"})
		}
	}
	out := make([]discoveryRoot, 0, len(dirs))
	for _, d := range dirs {
		if s.excludedPaths[config.CanonicalSkillPath(d.dir)] {
			continue
		}
		key := config.CanonicalSkillPath(d.dir)
		out = append(out, discoveryRoot{
			Root:              Root{Dir: d.dir, Scope: d.scope, Priority: len(out), Status: pathStatus(d.dir)},
			requireFlatMarker: d.requireFlatMarker,
			plugins:           append([]string(nil), s.pluginPaths[key]...),
			forceSubagent:     len(s.pluginAgentPaths[key]) > 0,
		})
	}
	return out
}

func normalizePluginPaths(paths map[string][]string) map[string][]string {
	out := map[string][]string{}
	for path, plugins := range paths {
		key := config.CanonicalSkillPath(path)
		if key == "" {
			continue
		}
		for _, plugin := range plugins {
			plugin = strings.TrimSpace(plugin)
			if plugin == "" || stringSliceContains(out[key], plugin) {
				continue
			}
			out[key] = append(out[key], plugin)
		}
		sort.Strings(out[key])
	}
	return out
}

func stringSliceContains(items []string, want string) bool {
	return slices.Contains(items, want)
}

// Roots exposes the discovery directories with their status for `/skill paths`.
func (s *Store) Roots() []Root {
	roots := s.roots()
	out := make([]Root, 0, len(roots))
	for _, r := range roots {
		out = append(out, r.Root)
	}
	return out
}

func disabledNameSet(names []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		if key := config.SkillNameKey(name); key != "" {
			out[key] = true
		}
	}
	return out
}

func (s *Store) disabledName(name string) bool {
	return s.disabled[config.SkillNameKey(name)]
}

func normalizeMaxDepth(depth int) int {
	const (
		defaultDepth = 3
		maxDepth     = 5
	)
	if depth == 0 {
		return defaultDepth
	}
	if depth < 1 {
		return 1
	}
	if depth > maxDepth {
		return maxDepth
	}
	return depth
}

// pathStatus classifies a root directory without failing on the common case of
// "not created yet".
func pathStatus(dir string) PathStatus {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return StatusMissing
		}
		return StatusUnreadable
	}
	if !info.IsDir() {
		return StatusNotDirectory
	}
	if f, err := os.Open(dir); err != nil {
		return StatusUnreadable
	} else {
		_ = f.Close()
	}
	return StatusOK
}

func (s *Store) discoveredSkills() []Skill {
	if s == nil || s.disableDiscovery {
		return nil
	}
	var out []Skill
	for _, r := range s.roots() {
		if r.Status != StatusOK {
			continue
		}
		for _, sk := range s.discoverRoot(r) {
			if s.disabledName(sk.Name) {
				continue
			}
			if len(r.plugins) == 0 {
				out = append(out, sk)
				continue
			}
			for _, plugin := range r.plugins {
				owned := sk
				owned.Plugin = plugin
				if r.forceSubagent {
					owned.SlashPrefix = plugin + ":agent"
				}
				out = append(out, owned)
			}
		}
	}
	if !s.disableBuiltins {
		for _, sk := range builtinSkills() {
			if !s.disabledName(sk.Name) {
				out = append(out, sk)
			}
		}
	}
	return out
}

func (s *Store) enabledSkills() []Skill {
	byName := map[string]Skill{}
	for _, sk := range s.discoveredSkills() {
		if _, dup := byName[sk.Name]; !dup {
			byName[sk.Name] = sk
		}
	}
	out := make([]Skill, 0, len(byName))
	for _, sk := range byName {
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// List returns every model-visible skill, deduped by its bare internal name
// (first/highest-priority root wins) and sorted for a cache-stable index.
// Role-setting profiles do not filter this surface.
func (s *Store) List() []Skill {
	return s.enabledSkills()
}

// SlashList returns the visible user-facing skill directory. Plugin skills are
// retained per package under /<plugin>:<name>, even when their bare names
// collide; non-plugin skills keep their existing short names.
func (s *Store) SlashList() []Skill {
	return VisibleSlashSkills(s.discoveredSkills())
}

// VisibleSlashSkills deduplicates skills by their user-facing slash name and
// returns them in deterministic display order.
func VisibleSlashSkills(skills []Skill) []Skill {
	byName := map[string]Skill{}
	for _, sk := range skills {
		name := sk.SlashName()
		if name == "" {
			continue
		}
		if _, dup := byName[name]; !dup {
			byName[name] = sk
		}
	}
	out := make([]Skill, 0, len(byName))
	for _, sk := range byName {
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SlashName() < out[j].SlashName() })
	return out
}

// ResolveSlashSkill resolves a visible qualified plugin name or a compatible
// short name. A short plugin name is rejected when multiple plugin packages
// contribute it; a higher-priority non-plugin winner keeps its short name.
func ResolveSlashSkill(skills []Skill, name string) (Skill, bool) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" {
		return Skill{}, false
	}
	for _, sk := range skills {
		if sk.SlashName() == name {
			return sk, true
		}
	}
	if strings.Contains(name, ":") || !IsValidName(name) {
		return Skill{}, false
	}
	var winner Skill
	var found bool
	plugins := map[string]bool{}
	for _, sk := range skills {
		if sk.Name != name {
			continue
		}
		if !found {
			winner, found = sk, true
		}
		if sk.Plugin != "" {
			plugins[sk.Plugin] = true
		}
	}
	if !found || winner.Plugin != "" && len(plugins) > 1 {
		return Skill{}, false
	}
	return winner, true
}

// Read resolves one skill by name, scanning the roots in priority order then the
// built-ins. ok is false when no such skill exists or the file is unreadable.
func (s *Store) Read(name string) (Skill, bool) {
	if !IsValidName(name) {
		return Skill{}, false
	}
	if s.disabledName(name) {
		return Skill{}, false
	}
	for _, sk := range s.enabledSkills() {
		if sk.Name == name {
			return sk, true
		}
	}
	return Skill{}, false
}

// ReadSlash resolves a user-entered slash identifier without changing the
// bare identifiers accepted by Read/run_skill.
func (s *Store) ReadSlash(name string) (Skill, bool) {
	return ResolveSlashSkill(s.discoveredSkills(), name)
}

func (s *Store) discoverRoot(r discoveryRoot) []Skill {
	var out []Skill
	s.scanDir(r.Dir, r.Scope, r.requireFlatMarker, 1, map[string]bool{}, &out)
	if r.forceSubagent {
		for i := range out {
			out[i].RunAs = RunSubagent
			out[i].Invocation = "manual"
			out[i].AllowedTools = mapClaudeAgentTools(out[i].AllowedTools)
			if isClaudeModelAlias(out[i].Model) {
				out[i].Model = ""
			}
		}
	}
	return out
}

func (s *Store) scanDir(dir string, scope Scope, requireFlatMarker bool, depth int, seen map[string]bool, out *[]Skill) {
	key := filepath.Clean(dir)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		key = filepath.Clean(resolved)
	}
	if seen[key] {
		return
	}
	seen[key] = true

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		sk, ok := s.readEntry(dir, scope, requireFlatMarker, e)
		if ok {
			if depth == 1 || strings.TrimSpace(sk.Description) != "" {
				*out = append(*out, sk)
			}
			continue
		}
		if depth >= s.maxDepth || !s.canScanChildDir(dir, e) {
			continue
		}
		s.scanDir(filepath.Join(dir, e.Name()), scope, requireFlatMarker, depth+1, seen, out)
	}
}

func (s *Store) canScanChildDir(dir string, e os.DirEntry) bool {
	name := e.Name()
	if shouldSkipScanDir(name) {
		return false
	}
	if e.IsDir() {
		return true
	}
	if !shouldStatEntryTarget(e.Type()) {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && info.IsDir()
}

func shouldStatEntryTarget(mode os.FileMode) bool {
	return mode&os.ModeSymlink != 0 || mode&os.ModeIrregular != 0
}

func shouldSkipScanDir(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch strings.ToLower(name) {
	case "assets", "node_modules", "references", "scripts":
		return true
	default:
		return false
	}
}

// readEntry turns one directory entry into a skill. It resolves symlink and
// Windows reparse-style entries via os.Stat (os.ReadDir can report the link's
// own type, not its target's), so a linked skill directory or flat <name>.md is
// discovered like a real one; a broken link fails Stat and is skipped.
func (s *Store) readEntry(dir string, scope Scope, requireFlatMarker bool, e os.DirEntry) (Skill, bool) {
	name := e.Name()
	full := filepath.Join(dir, name)

	isDir := e.IsDir()
	isFile := e.Type().IsRegular()
	if !isDir && !isFile && shouldStatEntryTarget(e.Type()) {
		info, err := os.Stat(full)
		if err != nil {
			return Skill{}, false
		}
		isDir = info.IsDir()
		isFile = info.Mode().IsRegular()
	}

	if isDir {
		if !IsValidName(name) {
			return Skill{}, false
		}
		file := filepath.Join(full, SkillFile)
		if _, err := os.Stat(file); err != nil {
			return Skill{}, false
		}
		return s.parse(file, name, scope)
	}
	if isFile && strings.EqualFold(filepath.Ext(name), ".md") {
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		if !IsValidName(stem) {
			return Skill{}, false
		}
		return s.parseFlat(full, stem, scope, requireFlatMarker)
	}
	return Skill{}, false
}

// parse reads and decodes one skill file. The frontmatter `name:` overrides the
// filename stem when valid; a missing `description:` is a warning, not a failure
// (the skill loads but won't appear in the model's index).
func (s *Store) parse(path, stem string, scope Scope) (Skill, bool) {
	return s.parseSkill(path, stem, scope, false)
}

// parseFlat reads a flat <name>.md skill candidate. Claude skill roots can also
// contain ordinary documentation, so those flat files need explicit skill
// frontmatter before they are treated as skills.
func (s *Store) parseFlat(path, stem string, scope Scope, requireSkillMarker bool) (Skill, bool) {
	return s.parseSkill(path, stem, scope, requireSkillMarker)
}

func (s *Store) parseSkill(path, stem string, scope Scope, requireSkillMarker bool) (Skill, bool) {
	b, err := fileencoding.ReadFileUTF8(path)
	if err != nil {
		return Skill{}, false
	}
	content := strings.TrimPrefix(strings.ReplaceAll(string(b), "\r\n", "\n"), "\uFEFF")
	fm, body := splitFrontmatter(content)
	if requireSkillMarker && !hasSkillMarker(content, fm) {
		return Skill{}, false
	}

	name := stem
	if v := fm[skillFrontmatterName]; v != "" && IsValidName(v) {
		name = v
	}
	desc := strings.TrimSpace(fm[skillFrontmatterDescription])
	if desc == "" {
		fmt.Fprintf(s.stderr, "warning: skill %q at %s has no description: — it will load but won't appear in the skills index\n", name, path)
	}
	sk := Skill{
		Name:         name,
		Description:  desc,
		Body:         loadBodyWithScripts(path, loadBodyWithReferences(path, strings.TrimSpace(body))),
		Scope:        scope,
		Path:         path,
		AllowedTools: parseAllowedTools(firstNonEmptySkillValue(fm[skillFrontmatterAllowedTools], fm["tools"])),
		RunAs:        parseRunAs(fm[skillFrontmatterRunAs], fm[skillFrontmatterContext], fm[skillFrontmatterAgent]),
		Model:        strings.TrimSpace(fm[skillFrontmatterModel]),
		Effort:       strings.TrimSpace(fm[skillFrontmatterEffort]),
		ReadOnly:     parseBoolFrontmatter(fm[skillFrontmatterReadOnly]),
		Triggers:     parseCSVFrontmatter(fm[skillFrontmatterTriggers]),
		NegativeTriggers: parseCSVFrontmatter(
			fm[skillFrontmatterNegativeTriggers],
		),
		AutoUse:        parseAutoUse(fm[skillFrontmatterAutoUse]),
		NeedsFreshData: parseBoolFrontmatter(fm[skillFrontmatterNeedsFreshData]),
		Cost:           parseCost(fm[skillFrontmatterCost]),
		Color:          strings.TrimSpace(fm[skillFrontmatterColor]),
		Invocation:     parseInvocation(fm[skillFrontmatterInvocation]),
		Requires:       parseCSVFrontmatter(fm[skillFrontmatterRequires]),
	}
	sk.Profiles, sk.InvalidProfiles = parseProfilesFrontmatter(fm[skillFrontmatterProfiles])
	return sk, true
}

func firstNonEmptySkillValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func isClaudeModelAlias(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "sonnet", "opus", "haiku", "inherit":
		return true
	default:
		return false
	}
}

func mapClaudeAgentTools(in []string) []string {
	mapping := map[string]string{
		"read": "read_file", "write": "write_file", "edit": "edit_file",
		"bash": "bash", "grep": "grep", "glob": "glob", "ls": "ls",
		"webfetch": "web_fetch", "websearch": "web_search",
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, name := range in {
		mapped := strings.TrimSpace(name)
		if replacement := mapping[strings.ToLower(mapped)]; replacement != "" {
			mapped = replacement
		}
		if mapped != "" && !seen[mapped] {
			seen[mapped] = true
			out = append(out, mapped)
		}
	}
	return out
}
