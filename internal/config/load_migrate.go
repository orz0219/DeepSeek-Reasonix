package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
)

// MigrateLegacyAgentStepLimitsForRoot removes retired [agent] step-limit keys
// from the user and project config selected for root. Boot calls it immediately
// before LoadForRoot, so config-only/read-only commands never rewrite files and
// the runtime can surface exactly one migration notice.
func MigrateLegacyAgentStepLimitsForRoot(root string) (bool, error) {
	root = resolveRoot(root)
	paths := make([]string, 0, 2)
	if userPath := userConfigLoadPath(); userPath != "" {
		paths = append(paths, userPath)
	}
	projectPath := "reasonix.toml"
	if root != "." {
		projectPath = filepath.Join(root, "reasonix.toml")
	}
	paths = append(paths, projectPath)

	changedAny := false
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		changed, err := migrateLegacyAgentStepLimitsFile(path)
		if err != nil {
			return changedAny, fmt.Errorf("migrate deprecated agent step limits in %s: %w", path, err)
		}
		changedAny = changedAny || changed
	}
	return changedAny, nil
}

// migrateLegacyAgentStepLimitsFile removes retired [agent] step-limit keys
// before runtime decoding. A process-wide lock makes concurrent desktop tab
// builds observe a single migration; the atomic rewrite protects other readers.
func migrateLegacyAgentStepLimitsFile(path string) (bool, error) {
	return migrateRetiredConfigKeysFile(path, stripLegacyAgentStepLimitLines)
}

func stripLegacyAgentStepLimitLines(raw string) (string, bool) {
	return stripTOMLKeyLines(raw, "agent", "max_steps", "planner_max_steps")
}

// MigrateLegacyRedactToolOutputForRoot removes the retired
// [secrets].redact_tool_output setting from the user and project configs chosen
// for root. The setting no longer controls any runtime behavior; removing it
// avoids leaving an explicit `true` value on disk that falsely suggests live
// output or transcript redaction is still active.
func MigrateLegacyRedactToolOutputForRoot(root string) (bool, error) {
	root = resolveRoot(root)
	paths := make([]string, 0, 2)
	if userPath := userConfigLoadPath(); userPath != "" {
		paths = append(paths, userPath)
	}
	projectPath := "reasonix.toml"
	if root != "." {
		projectPath = filepath.Join(root, "reasonix.toml")
	}
	paths = append(paths, projectPath)

	changedAny := false
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		changed, err := migrateLegacyRedactToolOutputFile(path)
		if err != nil {
			return changedAny, fmt.Errorf("migrate deprecated redact_tool_output in %s: %w", path, err)
		}
		changedAny = changedAny || changed
	}
	return changedAny, nil
}

func migrateLegacyRedactToolOutputFile(path string) (bool, error) {
	return migrateRetiredConfigKeysFile(path, stripLegacyRedactToolOutputLines)
}

func stripLegacyRedactToolOutputLines(raw string) (string, bool) {
	return stripTOMLKeyLines(raw, "secrets", "redact_tool_output")
}

// MigrateLegacyMemoryCompilerForRoot removes the retired
// [agent].memory_compiler setting from the user and project configs chosen for
// root. The Memory v5 execution compiler was removed; stripping the key avoids
// leaving values on disk that falsely suggest compiler behavior (especially a
// stale verbosity = "compact") is still active.
func MigrateLegacyMemoryCompilerForRoot(root string) (bool, error) {
	root = resolveRoot(root)
	paths := make([]string, 0, 2)
	if userPath := userConfigLoadPath(); userPath != "" {
		paths = append(paths, userPath)
	}
	projectPath := "reasonix.toml"
	if root != "." {
		projectPath = filepath.Join(root, "reasonix.toml")
	}
	paths = append(paths, projectPath)

	changedAny := false
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		changed, err := migrateLegacyMemoryCompilerFile(path)
		if err != nil {
			return changedAny, fmt.Errorf("migrate deprecated memory_compiler in %s: %w", path, err)
		}
		changedAny = changedAny || changed
	}
	return changedAny, nil
}

func migrateLegacyMemoryCompilerFile(path string) (bool, error) {
	return migrateRetiredConfigKeysFile(path, stripLegacyMemoryCompilerLines)
}

func migrateRetiredConfigKeysFile(path string, strip func(string) (string, bool)) (bool, error) {
	unlock, err := LockConfigFileEdits(path)
	if err != nil {
		return false, err
	}
	defer unlock()
	resolved, exists, err := statConfigPath(path)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return false, err
	}
	raw, err := fileencoding.ReadFileUTF8(resolved)
	if err != nil {
		return false, err
	}
	next, changed := strip(string(raw))
	if !changed {
		return false, nil
	}
	if err := fileutil.AtomicWriteFile(resolved, []byte(next), info.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

func stripLegacyMemoryCompilerLines(raw string) (string, bool) {
	return stripTOMLKeyLines(raw, "agent", "memory_compiler")
}

// MigrateLegacyMultiThresholdCompactionForRoot strips retired soft/snip/force keys.
func MigrateLegacyMultiThresholdCompactionForRoot(root string) (bool, error) {
	root = resolveRoot(root)
	paths := make([]string, 0, 2)
	if userPath := userConfigLoadPath(); userPath != "" {
		paths = append(paths, userPath)
	}
	projectPath := "reasonix.toml"
	if root != "." {
		projectPath = filepath.Join(root, "reasonix.toml")
	}
	paths = append(paths, projectPath)

	changedAny := false
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		changed, err := migrateLegacyMultiThresholdCompactionFile(path)
		if err != nil {
			return changedAny, fmt.Errorf("migrate deprecated multi-threshold compaction keys in %s: %w", path, err)
		}
		changedAny = changedAny || changed
	}
	return changedAny, nil
}

func migrateLegacyMultiThresholdCompactionFile(path string) (bool, error) {
	return migrateRetiredConfigKeysFile(path, stripLegacyMultiThresholdCompactionLines)
}

func stripLegacyMultiThresholdCompactionLines(raw string) (string, bool) {
	return stripTOMLKeyLines(raw, "agent",
		"soft_compact_ratio",
		"tool_result_snip_ratio",
		"compact_force_ratio",
		"cold_resume_prune",
		"context_editing",
	)
}

func migrateLegacyMCPTiersFile(path string) error {
	_, err := migrateRetiredConfigKeysFile(path, stripLegacyMCPTierLines)
	return err
}

func stripLegacyMCPTierLines(raw string) (string, bool) {
	return stripTOMLKeyLines(raw, "plugins", "tier")
}

// tomlStringState tracks whether a line-oriented scan is currently inside a
// TOML multiline string, so retired-key strippers never treat prose inside a
// `"""..."""` or `”'...”'` value (e.g. a config example quoted in a
// system_prompt) as a section header or key assignment.
type tomlStringState int

const (
	tomlOutside tomlStringState = iota
	tomlInMultilineBasic
	tomlInMultilineLiteral
)

// advanceTOMLStringState scans one raw line and returns the multiline-string
// state after it. Outside strings it honours single-line strings and `#`
// comments so quote delimiters inside them cannot open a multiline state.
// The scan is intentionally conservative: on malformed input it prefers
// staying/returning outside, which makes callers keep lines rather than
// delete them.
func advanceTOMLStringState(state tomlStringState, line string) tomlStringState {
	i := 0
	for i < len(line) {
		switch state {
		case tomlInMultilineBasic:
			if line[i] == '\\' {
				i += 2
				continue
			}
			if strings.HasPrefix(line[i:], `"""`) {
				state = tomlOutside
				i += 3
				continue
			}
			i++
		case tomlInMultilineLiteral:
			if strings.HasPrefix(line[i:], "'''") {
				state = tomlOutside
				i += 3
				continue
			}
			i++
		default:
			switch {
			case line[i] == '#':
				return state
			case strings.HasPrefix(line[i:], `"""`):
				state = tomlInMultilineBasic
				i += 3
			case strings.HasPrefix(line[i:], "'''"):
				state = tomlInMultilineLiteral
				i += 3
			case line[i] == '"':
				i++
				for i < len(line) && line[i] != '"' {
					if line[i] == '\\' {
						i++
					}
					i++
				}
				i++
			case line[i] == '\'':
				i++
				for i < len(line) && line[i] != '\'' {
					i++
				}
				i++
			default:
				i++
			}
		}
	}
	return state
}

// stripTOMLKeyLines removes top-level `key = ...` assignment lines under the
// named section while leaving every line inside a TOML multiline string
// untouched. All retired-config-key migrations share it so none of them can
// corrupt a multiline value (such as a system_prompt quoting a config
// example). A dropped line is first checked to not itself open a multiline
// value; if it would, the line is kept — for these retired keys that never
// happens (their values are single-line), and keeping a stale line is always
// safer than truncating a string the user wrote.
func stripTOMLKeyLines(raw, section string, keys ...string) (string, bool) {
	lines := strings.Split(raw, "\n")
	current := ""
	state := tomlOutside
	changed := false
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if state != tomlOutside {

			out = append(out, line)
			state = advanceTOMLStringState(state, line)
			continue
		}
		if header := tomlSectionHeader(line); header != "" {
			current = header
		}
		next := advanceTOMLStringState(tomlOutside, line)
		if current == section && next == tomlOutside {
			dropped := false
			for _, key := range keys {
				if isTOMLKeyAssignment(line, key) {
					changed = true
					dropped = true
					break
				}
			}
			if dropped {
				continue
			}
		}
		out = append(out, line)
		state = next
	}
	return strings.Join(out, "\n"), changed
}

func tomlSectionHeader(line string) string {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "[") {
		return ""
	}
	if i := strings.Index(trimmed, "#"); i >= 0 {
		trimmed = strings.TrimSpace(trimmed[:i])
	}
	if strings.HasPrefix(trimmed, "[[") && strings.HasSuffix(trimmed, "]]") {
		return strings.TrimSpace(trimmed[2 : len(trimmed)-2])
	}
	if strings.HasSuffix(trimmed, "]") {
		return strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	}
	return "other"
}
