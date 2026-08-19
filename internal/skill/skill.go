// Package skill loads invokable playbooks ("skills") from Markdown files. A skill
// is a named, described prompt body the model can invoke via the run_skill tool
// (or the user via a slash name): an "inline" skill folds its body into the turn as
// a tool result, a "subagent" skill runs in an isolated child loop and returns
// only its final answer. Project scope wins over global; only names+descriptions
// enter the cache-stable system-prompt index (see index.go) — bodies load on
// demand. Discovery scans several conventions (.reasonix / .agents / .agent /
// .claude under the project root and the home dir — see config.ConventionDirs) so
// skills authored for other agent tools migrate in unchanged. Directory skills
// use <name>/SKILL.md; flat <name>.md files from Claude roots are loaded only
// when they carry skill frontmatter. Discovery follows symlinks, so linked
// skills are picked up like real ones.
package skill

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/tool"
)

// ErrInvocationUnavailable marks a profile/dependency gate that can become
// runnable after switching profile or connecting the required capability.
var ErrInvocationUnavailable = errors.New("skill invocation unavailable")

// Scope records where a skill was loaded from. Higher-priority scopes win on a
// name collision: project > custom > global > builtin.
type Scope string

const (
	ScopeProject Scope = "project"
	ScopeCustom  Scope = "custom"
	ScopeGlobal  Scope = "global"
	ScopeBuiltin Scope = "builtin"
)

// RunAs selects how an invoked skill executes. Inline folds the body into the
// parent turn; subagent spawns an isolated child loop and returns only the final
// answer (its tool calls and reasoning never enter the parent context).
type RunAs string

const (
	RunInline   RunAs = "inline"
	RunSubagent RunAs = "subagent"
)

const (
	// SkillsDirname is the directory under each root that holds skills.
	SkillsDirname = "skills"
	// SkillFile is the canonical filename inside a directory-layout skill.
	SkillFile = "SKILL.md"
)

// Skill is a loaded playbook.
type Skill struct {
	Name        string // canonical identifier; matches the directory / filename stem
	Description string // one-liner shown in the pinned index
	Body        string // full markdown body (post-frontmatter), loaded eagerly
	Scope       Scope  // where it came from
	Path        string // absolute path to the SKILL.md / <name>.md, or "(builtin)"
	Plugin      string // installed plugin package name; empty for non-plugin skills
	// runtimeBindingsPrepared is session-local invocation state. It must not be
	// inferred from untrusted Markdown content or persisted skill metadata.
	runtimeBindingsPrepared bool
	// SlashPrefix overrides Plugin only for the user-facing invocation name.
	// Imported Claude agents use <plugin>:agent so an agent and skill may safely
	// share the same upstream name.
	SlashPrefix string
	// AllowedTools, when non-empty, scopes a subagent skill's tool registry to
	// these literal tool names (from the `allowed-tools` frontmatter).
	AllowedTools []string
	RunAs        RunAs  // inline | subagent
	Model        string // optional model override for runAs=subagent (frontmatter `model:`)
	Effort       string // optional effort for runAs=subagent (frontmatter `effort:`)
	// ReadOnly, when true, runs a subagent skill against the read-only tool
	// registry: writer tools are stripped and bash enforces the read-only
	// command policy at execution time (frontmatter `read-only:`). This is a
	// tool-boundary contract, not a prompt promise.
	ReadOnly bool
	Color    string // optional display tag for UI surfaces (frontmatter `color:`); no runtime effect
	// Invocation gates whether this skill enters the pinned Skills index the
	// model reads every turn. "auto" (default) behaves like every skill always
	// has. "manual" keeps the skill invocable by name (/<name>, run_skill) but
	// invisible to model-initiated discovery — for user-authored subagent
	// profiles meant to be triggered deliberately, not autonomously.
	Invocation string // auto | manual (frontmatter `invocation:`)
	// Routing metadata is intentionally kept out of the cache-stable Skills
	// index; it feeds per-turn capability hints only.
	Triggers         []string
	NegativeTriggers []string
	AutoUse          string // off | suggest | prefer | require
	NeedsFreshData   bool
	Cost             string // low | medium | high (advisory)
	// Requires lists capability IDs this skill depends on (e.g. mcp-server:github).
	// Optional; empty keeps full backward compatibility with older skills.
	Requires []string
	// Profiles restricts availability to economy|balanced|delivery. Empty means
	// the skill is eligible in every profile.
	Profiles []string
	// InvalidProfiles preserves rejected profiles frontmatter values so doctor
	// can warn about typos; the parser drops them from Profiles silently.
	InvalidProfiles []string
}

// SlashName returns the user-facing slash identifier. Plugin skills use a
// package-qualified name while the internal Name remains stable for run_skill.
func (s Skill) SlashName() string {
	prefix := strings.TrimSpace(s.SlashPrefix)
	if prefix == "" {
		prefix = strings.TrimSpace(s.Plugin)
	}
	if prefix == "" {
		return s.Name
	}
	return prefix + ":" + s.Name
}

// IsValidName reports whether name is a usable skill identifier.
func IsValidName(name string) bool { return config.IsValidSkillName(name) }

// Options configure a Store. ProjectRoot "" reads only the global + custom
// scopes. HomeDir "" resolves to the OS home dir (tests point it at a tmpdir).
// ReasonixHomeDir overrides the canonical Reasonix home; empty uses
// config.ReasonixHomeDir(), or HomeDir/.reasonix when HomeDir is explicitly set.
type Options struct {
	HomeDir          string
	ReasonixHomeDir  string
	ProjectRoot      string
	CustomPaths      []string
	PluginPaths      map[string][]string // canonical custom root -> installed plugin package names
	PluginAgentPaths map[string][]string // plugin roots whose flat Markdown files are Claude agents
	ExcludedPaths    []string
	DisabledNames    []string
	MaxDepth         int
	DisableBuiltins  bool // suppress shipped built-ins (test-only knob)
	// DisableDiscovery returns an empty store without probing project, custom,
	// global, plugin, or built-in skill sources. It is a test-only isolation knob.
	DisableDiscovery bool
	// Stderr is the writer for diagnostic warnings. When nil, defaults to
	// os.Stderr. Set to io.Discard to suppress output (e.g. during model
	// switch inside a bubbletea session).
	Stderr io.Writer
}

// Store resolves skills across the configured roots.
type Store struct {
	homeDir          string
	reasonixHomeDir  string
	projectRoot      string
	customPaths      []string
	pluginPaths      map[string][]string
	pluginAgentPaths map[string][]string
	excludedPaths    map[string]bool
	disabled         map[string]bool
	maxDepth         int
	disableBuiltins  bool
	disableDiscovery bool
	stderr           io.Writer
	runtimeProfile   string
	requiresReady    func([]string) []string
	toolBindings     func(Skill) []tool.MCPBinding
}

// New builds a Store. Relative custom paths and a relative project root are made
// absolute; "~" in a custom path expands to the home dir.
func New(opts Options) *Store {
	home := opts.HomeDir
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	reasonixHome := opts.ReasonixHomeDir
	if reasonixHome == "" {
		if opts.HomeDir != "" {
			reasonixHome = filepath.Join(home, ".reasonix")
		} else {
			reasonixHome = config.ReasonixHomeDir()
		}
	}
	root := opts.ProjectRoot
	if root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
	}
	base := root
	if base == "" {
		if wd, err := os.Getwd(); err == nil {
			base = wd
		}
	}
	custom := dedupePaths(resolveCustomPaths(opts.CustomPaths, base, home))
	pluginPaths := normalizePluginPaths(opts.PluginPaths)
	pluginAgentPaths := normalizePluginPaths(opts.PluginAgentPaths)
	excluded := map[string]bool{}
	for _, p := range dedupePaths(resolveCustomPaths(opts.ExcludedPaths, base, home)) {
		excluded[config.CanonicalSkillPath(p)] = true
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	return &Store{
		homeDir:          home,
		reasonixHomeDir:  reasonixHome,
		projectRoot:      root,
		customPaths:      custom,
		pluginPaths:      pluginPaths,
		pluginAgentPaths: pluginAgentPaths,
		excludedPaths:    excluded,
		disabled:         disabledNameSet(opts.DisabledNames),
		maxDepth:         normalizeMaxDepth(opts.MaxDepth),
		disableBuiltins:  opts.DisableBuiltins,
		disableDiscovery: opts.DisableDiscovery,
		stderr:           stderr,
	}
}

// ConfigureInvocationPolicy installs session-local runtime constraints for
// skill calls. It does not alter discovery or the provider-visible tool schema;
// callers validate the selected skill immediately before execution.
func (s *Store) ConfigureInvocationPolicy(profile string, requiresReady func([]string) []string) {
	if s == nil {
		return
	}
	s.runtimeProfile = normalizeRuntimeProfile(profile)
	s.requiresReady = requiresReady
}

// ConfigureToolBindings installs a session-local resolver for plugin-owned MCP
// tools. It affects only an invoked skill body and never the cache-stable index.
func (s *Store) ConfigureToolBindings(resolve func(Skill) []tool.MCPBinding) {
	if s == nil {
		return
	}
	s.toolBindings = resolve
}

// Prepare binds a plugin skill's portable MCP references to this session's
// exact callable names. Non-plugin skills and sessions without bindings are
// returned byte-for-byte unchanged.
func (s *Store) Prepare(sk Skill) Skill {
	if s == nil || s.toolBindings == nil || strings.TrimSpace(sk.Plugin) == "" || sk.runtimeBindingsPrepared {
		return sk
	}
	bindings := append([]tool.MCPBinding(nil), s.toolBindings(sk)...)
	if len(bindings) == 0 {
		return sk
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].CallableName < bindings[j].CallableName })
	seen := map[string]bool{}
	unique := bindings[:0]
	for _, binding := range bindings {
		if binding.CallableName == "" || seen[binding.CallableName] {
			continue
		}
		seen[binding.CallableName] = true
		unique = append(unique, binding)
	}
	bindings = unique
	if len(bindings) == 0 {
		return sk
	}
	sk.AllowedTools = bindAllowedTools(sk.AllowedTools, bindings)
	sk.runtimeBindingsPrepared = true

	var b strings.Builder
	b.WriteString(strings.TrimRight(sk.Body, " \t\r\n"))
	b.WriteString("\n\n## Runtime MCP tool bindings\n\n")
	b.WriteString("These host-generated bindings are authoritative for this invocation. Use the exact direct name below; if only `use_capability` is available, use the stable capability ID. Short or Claude-style MCP names in this skill refer to these bindings.\n")
	for _, binding := range bindings {
		fmt.Fprintf(&b, "\n- `%s/%s` → `%s` (capability `%s`)", binding.Server, binding.RawName, binding.CallableName, binding.CapabilityID)
	}
	sk.Body = b.String()
	return sk
}

// Render prepares and renders a skill for a direct slash invocation.
func (s *Store) Render(sk Skill, args string) string { return Render(s.Prepare(sk), args) }

func bindAllowedTools(refs []string, bindings []tool.MCPBinding) []string {
	if len(refs) == 0 {
		return refs
	}
	out := make([]string, 0, len(refs))
	seen := map[string]bool{}
	appendOne := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, ref := range refs {
		matches := map[string]tool.MCPBinding{}
		isPattern := strings.ContainsAny(ref, "*?[")
		for _, binding := range bindings {
			aliases := append(tool.MCPBindingAliases(binding), binding.CallableName)
			for _, alias := range aliases {
				matched := ref == alias
				if isPattern {
					matched, _ = path.Match(ref, alias)
				}
				if matched {
					matches[binding.CallableName] = binding
					break
				}
			}
		}
		if isPattern {
			// Preserve the original pattern so an existing broad allowlist such as
			// "*" keeps all of its prior tools. Add only canonical MCP names the
			// upstream/Claude pattern itself cannot match in Reasonix.
			appendOne(ref)
			names := make([]string, 0, len(matches))
			for name := range matches {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				if matched, err := path.Match(ref, name); err != nil || !matched {
					appendOne(name)
				}
				// Capability IDs are host-only allowlist entries consumed when the
				// session exposes this MCP tool solely through use_capability. Do not
				// add one when the authored pattern already grants the proxy itself.
				proxyMatched, _ := path.Match(ref, "use_capability")
				if !proxyMatched {
					appendOne(matches[name].CapabilityID)
				}
			}
			continue
		}
		if len(matches) == 1 {
			for name, binding := range matches {
				appendOne(name)
				appendOne(binding.CapabilityID)
			}
			continue
		}
		// Preserve unresolved or ambiguous literals. The child registry will not
		// gain any broader permission from them.
		appendOne(ref)
	}
	return out
}

// ValidateInvocation enforces profiles/requires frontmatter at the host tool
// boundary, including direct run_skill calls that bypass capability routing.
// Skill profiles frontmatter is diagnostic-only: it never blocks invocation.
// Required capabilities still gate execution.
func (s *Store) ValidateInvocation(sk Skill) error {
	if s == nil {
		return nil
	}
	if len(sk.Requires) > 0 && s.requiresReady != nil {
		if missing := s.requiresReady(sk.Requires); len(missing) > 0 {
			return fmt.Errorf("%w: skill %q requires unavailable capabilities: %s", ErrInvocationUnavailable, sk.Name, strings.Join(missing, ", "))
		}
	}
	return nil
}

// AllowedInProfile reports whether a skill lists profile among its frontmatter
// profiles. Empty profiles mean "all". Role settings no longer filter the
// model-visible skill index or block run_skill; this helper remains for doctor
// diagnostics and capability inventory reports.
func AllowedInProfile(sk Skill, profile string) bool {
	if len(sk.Profiles) == 0 {
		return true
	}
	want := normalizeRuntimeProfile(profile)
	if want == "" {
		return true
	}
	for _, candidate := range sk.Profiles {
		if normalizeRuntimeProfile(candidate) == want {
			return true
		}
	}
	return false
}

// FilterForProfile returns skills that declare eligibility for profile.
// Host boot no longer uses this to hide skills from the model; doctor and
// inventory tooling may still call it for recommended-profile diagnostics.
func FilterForProfile(skills []Skill, profile string) []Skill {
	out := make([]Skill, 0, len(skills))
	for _, sk := range skills {
		if AllowedInProfile(sk, profile) {
			out = append(out, sk)
		}
	}
	return out
}

func normalizeRuntimeProfile(profile string) string {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "economy":
		return "economy"
	case "delivery":
		return "delivery"
	case "balanced", "full":
		return "balanced"
	default:
		return ""
	}
}

// HasProjectScope reports whether the store was configured with a project root.
func (s *Store) HasProjectScope() bool { return s.projectRoot != "" }

// PathStatus describes a root directory's readability, surfaced by `/skill paths`.

const (
	StatusOK           PathStatus = "ok"
	StatusMissing      PathStatus = "missing"
	StatusNotDirectory PathStatus = "not-directory"
	StatusUnreadable   PathStatus = "unreadable"
)

// Root is one discovery directory with its scope, priority, and status.

// roots returns the discovery directories, highest priority first: the
// convention dirs (config.ConventionDirs: .reasonix / .agents / .agent / .claude)
// under the project root → custom paths → the Reasonix home skills dir → other
// home-dir convention dirs. A later root never overrides an earlier one.

// Roots exposes the discovery directories with their status for `/skill paths`.

// pathStatus classifies a root directory without failing on the common case of
// "not created yet".

// List returns every model-visible skill, deduped by its bare internal name
// (first/highest-priority root wins) and sorted for a cache-stable index.
// Role-setting profiles do not filter this surface.

// SlashList returns the visible user-facing skill directory. Plugin skills are
// retained per package under /<plugin>:<name>, even when their bare names
// collide; non-plugin skills keep their existing short names.

// VisibleSlashSkills deduplicates skills by their user-facing slash name and
// returns them in deterministic display order.

// ResolveSlashSkill resolves a visible qualified plugin name or a compatible
// short name. A short plugin name is rejected when multiple plugin packages
// contribute it; a higher-priority non-plugin winner keeps its short name.

// Read resolves one skill by name, scanning the roots in priority order then the
// built-ins. ok is false when no such skill exists or the file is unreadable.

// ReadSlash resolves a user-entered slash identifier without changing the
// bare identifiers accepted by Read/run_skill.

// readEntry turns one directory entry into a skill. It resolves symlink and
// Windows reparse-style entries via os.Stat (os.ReadDir can report the link's
// own type, not its target's), so a linked skill directory or flat <name>.md is
// discovered like a real one; a broken link fails Stat and is skipped.

// follows the link

// broken link

// a directory without a SKILL.md is not a skill

// parse reads and decodes one skill file. The frontmatter `name:` overrides the
// filename stem when valid; a missing `description:` is a warning, not a failure
// (the skill loads but won't appear in the model's index).

// parseFlat reads a flat <name>.md skill candidate. Claude skill roots can also
// contain ordinary documentation, so those flat files need explicit skill
// frontmatter before they are treated as skills.

// Create scaffolds a new skill stub at the chosen scope. Refuses to overwrite.

// CreateWithContent writes caller-supplied file contents as a canonical
// <name>/SKILL.md skill, refusing to clobber an existing directory-layout or
// legacy flat skill of the same name. Returns the written path.

// O_EXCL so a concurrent create (or an existing file) is reported, not clobbered.

// UpdateContent overwrites an existing user-authored skill's file contents in
// place. Refuses built-ins and a scope mismatch, mirroring Delete's rules —
// see Delete for why a mismatch must refuse rather than silently target the
// wrong file.

// validateMutablePath rejects writes through linked files or directories. Skill
// discovery intentionally follows symlinks for read compatibility, but editing
// one must never replace content outside the configured scope root.

// Delete removes a user-authored skill. Refuses built-ins (no file backs
// them) and refuses when the resolved skill's actual scope doesn't match the
// requested one — e.g. a project-scope delete for a name that only resolves
// at global scope, which would otherwise silently no-op against the wrong
// file while a same-named project-scope shadow kept showing up in List().

// directory-layout skill: <name>/SKILL.md + siblings

// legacy flat <name>.md skill

// loadBodyWithReferences appends a directory-layout skill's sibling
// references/*.md files to its body (Anthropic Skills compatibility), so depth
// material is available without on-demand resolution. Flat skills have no
// references dir and are returned unchanged.

// loadBodyWithScripts appends a directory-layout skill's sibling scripts/
// directory listing to the body, so the model knows what scripts are
// available and can run them via bash (inheriting sandbox, gate).

// Filter hidden files — bash should not see config dotfiles in scripts/.

// parseAllowedTools splits a comma-separated `allowed-tools` value into trimmed,
// non-empty tool names; nil when absent.

// parseCSVFrontmatter splits simple comma-separated frontmatter values. Full
// YAML lists are intentionally out of scope for the existing frontmatter parser.

// parseProfilesFrontmatter keeps only economy|balanced|delivery values and
// returns the rejected ones separately so doctor can surface typos instead of
// the parser hiding them.

// parseInvocation maps frontmatter to an invocation mode. Anything other than
// "manual" (including absent) is "auto" — the existing, universal behavior.

// parseRunAs maps frontmatter to a run mode. An unknown value defaults to the
// safe (non-spawning) inline mode; a `context: fork` or a non-empty `agent:`
// field (cross-tool conventions) signals subagent isolation.

// stubBody is the scaffold written by `/skill new` — minimal frontmatter plus
// guidance the author fills in.

// resolveCustomPaths expands "~" and makes each custom path absolute relative to
// baseDir.

// dedupePaths drops duplicate custom roots, preserving order.

// splitFrontmatter is a thin wrapper kept for internal use; the real parser
// lives in internal/frontmatter.
