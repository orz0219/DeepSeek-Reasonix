package permission

import (
	"encoding/json"
	"strings"

	"reasonix/internal/shellparse"
)

// subjectKeys are the JSON argument keys, in priority order, that carry a tool
// call's "subject" — the thing a Subject glob matches against. Generic so tools
// need not implement a permission-specific method: bash exposes command, the
// file tools expose path / file_path, grep & glob expose pattern.
var subjectKeys = []string{"command", "file_path", "path", "source_path", "destination_path", "pattern"}

// Subject extracts the primary matchable subject string from a call's raw JSON
// args, returning "" when none of the known keys is present (such a call only
// matches bare "ToolName" rules). Use Subjects for permission decisions that
// must account for every touched endpoint.
func Subject(args json.RawMessage) string {
	subjects := Subjects(args)
	if len(subjects) > 0 {
		return subjects[0]
	}
	return ""
}

// Subjects extracts every matchable subject from a call's raw JSON args. Most
// tools expose one subject; move_file exposes both source_path and
// destination_path so path-scoped permission rules can protect either endpoint.
func Subjects(args json.RawMessage) []string {
	if len(args) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return nil
	}
	src := stringArg(m, "source_path")
	dst := stringArg(m, "destination_path")
	if src != "" && dst != "" {
		out := []string{src}
		if dst != src {
			out = append(out, dst)
		}
		return out
	}
	for _, k := range subjectKeys {
		if s := stringArg(m, k); s != "" {
			return []string{s}
		}
	}
	return nil
}

func stringArg(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// matchGlob reports whether name matches pattern, where '*' matches any run of
// characters (including separators) and '?' matches exactly one. Unlike
// path.Match, '*' is not stopped by '/', which is what command-line and path
// prefixes ("rm -rf*", "/etc/*") intuitively expect. Linear time with
// backtracking, byte-oriented.
func matchGlob(pattern, name string) bool {
	var px, nx, starPx, starNx int
	starPx = -1
	for nx < len(name) {
		switch {
		case px < len(pattern) && pattern[px] == '*':
			starPx = px
			starNx = nx
			px++
		case px < len(pattern) && (pattern[px] == '?' || pattern[px] == name[nx]):
			px++
			nx++
		case starPx != -1:
			px = starPx + 1
			starNx++
			nx = starNx
		default:
			return false
		}
	}
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}

func isPackageManagerRun(base string) bool {
	switch base {
	case "npm", "pnpm", "yarn", "bun":
		return true
	default:
		return false
	}
}

// IsFileMutationTool reports whether a built-in tool mutates workspace files.
func IsFileMutationTool(toolName string) bool {
	switch toolName {
	case "write_file", "edit_file", "multi_edit", "move_file", "notebook_edit", "delete_range", "delete_symbol":
		return true
	default:
		return false
	}
}

func ruleToolMatches(ruleTool, toolName string) bool {
	ruleTool = canonicalRuleTool(ruleTool)
	return ruleTool == toolName || (ruleTool == "file_mutation" && IsFileMutationTool(toolName))
}

func ruleToolCompatible(existingTool, candidateTool string) bool {
	existingTool = canonicalRuleTool(existingTool)
	candidateTool = canonicalRuleTool(candidateTool)
	return existingTool == candidateTool ||
		(existingTool == "file_mutation" && (candidateTool == "file_mutation" || IsFileMutationTool(candidateTool)))
}

func canonicalRuleTool(toolName string) string {
	switch strings.TrimSpace(toolName) {
	case "Bash", "bash":
		return "bash"
	case "Edit", "edit", "file_mutation":
		return "file_mutation"
	default:
		return toolName
	}
}

func ruleSubjectMatches(rule Rule, subject string) bool {
	if rule.Subject == "" {
		return true
	}
	if subject == "" {
		return false
	}
	if rule.Literal {
		return rule.Subject == subject
	}
	if canonicalRuleTool(rule.Tool) == "bash" {
		if base, ok := bashColonPrefixBase(rule.Subject); ok {
			return bashPrefixMatches(base, subject)
		}
		if base, ok := legacyBashSpaceStarPrefixBase(rule.Subject); ok {
			return bashPrefixMatches(base, subject)
		}
	}
	return matchGlob(rule.Subject, subject)
}

func bashColonPrefixBase(pattern string) (string, bool) {
	if !strings.HasSuffix(pattern, ":*") {
		return "", false
	}
	base := strings.TrimSuffix(pattern, ":*")
	return base, base != ""
}

func legacyBashSpaceStarPrefixBase(pattern string) (string, bool) {
	if !strings.HasSuffix(pattern, " *") {
		return "", false
	}
	base := strings.TrimSuffix(pattern, " *")
	return base, base != ""
}

func bashPrefixBase(pattern string) (string, bool) {
	if base, ok := bashColonPrefixBase(pattern); ok {
		return base, true
	}
	return legacyBashSpaceStarPrefixBase(pattern)
}

func bashPrefixMatches(base, subject string) bool {
	if normalized, ok := normalizeBashSafeRedirectsForMatch(subject); ok {
		subject = normalized
	}
	fields, malformed := shellparse.StaticFields(subject)
	if malformed != "" {
		return false
	}
	baseFields, malformed := shellparse.StaticFields(base)
	if malformed != "" || len(baseFields) == 0 || len(fields) < len(baseFields) {
		return false
	}
	for i, want := range baseFields {
		if fields[i] != want {
			return false
		}
	}
	return true
}
