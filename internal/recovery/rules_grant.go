package recovery

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

type taskGrantBoundary struct {
	key     string
	display string
}

func commandFieldsTaskGrantBoundary(fields []string) taskGrantBoundary {
	if len(fields) == 0 {
		return taskGrantBoundary{}
	}
	base := strings.ToLower(filepath.Base(fields[0]))
	rawArgs := fields[1:]
	switch base {
	case "env":
		wrapped, ok := unwrapEnvCommand(rawArgs)
		if ok {
			return commandFieldsTaskGrantBoundary(wrapped)
		}
	case "command":
		wrapped, ok := unwrapCommandBuiltin(rawArgs)
		if ok {
			return commandFieldsTaskGrantBoundary(wrapped)
		}
	case "git":
		return gitPushTaskGrantBoundary(rawArgs)
	case "gh":
		return ghTaskGrantBoundary(rawArgs)
	}
	return taskGrantBoundary{}
}

func gitPushTaskGrantBoundary(args []string) taskGrantBoundary {
	lower := lowerFields(args)
	if gitSubcommand(lower) != "push" || containsAny(lower,
		"-f", "--force", "--mirror", "--delete", "--prune", "--all", "--tags", "--follow-tags",
	) {
		return taskGrantBoundary{}
	}
	for _, arg := range lower {
		if strings.HasPrefix(arg, "--force") || strings.HasPrefix(arg, ":") || strings.HasPrefix(arg, "+") {
			return taskGrantBoundary{}
		}
	}
	pushAt := -1
	for i, arg := range lower {
		if arg == "push" {
			pushAt = i
			break
		}
	}
	if pushAt != 0 {

		return taskGrantBoundary{}
	}
	var positionals []string
	for i := pushAt + 1; i < len(args); i++ {
		arg := lower[i]
		switch arg {
		case "-u", "--set-upstream", "-q", "--quiet", "-v", "--verbose", "--progress", "--no-progress":
			continue
		}
		if strings.HasPrefix(arg, "-") {

			return taskGrantBoundary{}
		}
		positionals = append(positionals, strings.TrimSpace(args[i]))
	}

	if len(positionals) != 2 {
		return taskGrantBoundary{}
	}
	remote, refspec := positionals[0], positionals[1]
	if remote == "" || refspec == "" || strings.Contains(refspec, "*") {
		return taskGrantBoundary{}
	}
	target := refspec
	if before, after, ok := strings.Cut(refspec, ":"); ok {
		if strings.TrimSpace(before) == "" || strings.TrimSpace(after) == "" {
			return taskGrantBoundary{}
		}
		target = strings.TrimSpace(after)
	}
	if target == "HEAD" || target == "@" {
		return taskGrantBoundary{}
	}
	return taskGrantBoundary{
		key:     "bash:git.push:" + CallFingerprint("git.push", remote, target, nil),
		display: "git push " + remote + " → " + target,
	}
}

func ghTaskGrantBoundary(args []string) taskGrantBoundary {
	lower := lowerFields(args)
	group, rest := ghCommandGroup(lower)
	if len(rest) == 0 {
		return taskGrantBoundary{}
	}
	verb := rest[0]
	if (group != "pr" && group != "issue") || verb != "comment" {
		return taskGrantBoundary{}
	}
	if containsAny(lower, "--edit-last", "--delete-last") {
		return taskGrantBoundary{}
	}
	repo := "current"
	for i, arg := range lower {
		switch {
		case (arg == "--repo" || arg == "-r") && i+1 < len(args):
			repo = args[i+1]
		case strings.HasPrefix(arg, "--repo="):
			repo = strings.TrimSpace(args[i][len("--repo="):])
		case strings.HasPrefix(arg, "-r") && len(arg) > 2:
			repo = strings.TrimSpace(args[i][2:])
		}
	}
	target := "current"
	if len(rest) > 1 && !strings.HasPrefix(rest[1], "-") {
		target = rest[1]
	} else if len(rest) > 1 {

		return taskGrantBoundary{}
	}

	if target == "current" {
		return taskGrantBoundary{}
	}
	repo = strings.TrimSpace(repo)
	target = strings.TrimSpace(target)
	display := "gh " + group + " comment " + target
	if repo != "current" {
		display += " --repo " + repo
	}
	return taskGrantBoundary{
		key:     "bash:gh." + group + ".comment:" + CallFingerprint("gh."+group+".comment", repo, target, nil),
		display: display,
	}
}

func httpCommandHighRisk(args []string) bool {
	for _, arg := range args {
		upper := strings.ToUpper(arg)
		switch upper {
		case "POST", "PUT", "PATCH", "DELETE", "CONNECT", "PURGE", "LOCK", "UNLOCK":
			return true
		}
		lower := strings.ToLower(arg)
		if lower == "--raw" || lower == "--form" || strings.HasPrefix(lower, "--raw=") {
			return true
		}
		if strings.HasPrefix(arg, "-") || strings.Contains(arg, "://") {
			continue
		}
		if strings.Contains(arg, "==") && !strings.Contains(arg, ":=") && !strings.Contains(arg, "@") {
			continue
		}

		if strings.Contains(arg, "=") || strings.Contains(arg, "@") {
			return true
		}
	}
	return false
}

func hasGlobalFlag(fields []string) bool {
	return containsAny(fields, "-g", "--global", "--system", "--user")
}

func unwrapEnvCommand(args []string) ([]string, bool) {
	for len(args) > 0 {
		arg := args[0]
		lower := strings.ToLower(arg)
		switch {
		case lower == "-i" || lower == "--ignore-environment" || lower == "-0" || lower == "--null":
			args = args[1:]
		case lower == "-u" || lower == "--unset" || lower == "-c" || lower == "--chdir":
			if len(args) < 2 {
				return nil, false
			}
			args = args[2:]
		case strings.HasPrefix(lower, "--unset=") || strings.HasPrefix(lower, "--chdir="):
			args = args[1:]
		case strings.HasPrefix(arg, "-"):

			return nil, false
		case strings.Contains(arg, "="):
			args = args[1:]
		default:
			return args, true
		}
	}
	return nil, false
}

func unwrapCommandBuiltin(args []string) ([]string, bool) {
	for len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "-p":
			args = args[1:]
		case "-v":

			return nil, true
		default:
			if strings.HasPrefix(args[0], "-") {
				return nil, false
			}
			return args, true
		}
	}
	return nil, false
}

func trimLeadingOptions(args []string) []string {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = args[1:]
	}
	return args
}

func lowerFields(fields []string) []string {
	out := make([]string, len(fields))
	for i, field := range fields {
		out[i] = strings.ToLower(strings.TrimSpace(field))
	}
	return out
}

func containsAny(fields []string, values ...string) bool {
	wanted := make(map[string]struct{}, len(values))
	for _, value := range values {
		wanted[value] = struct{}{}
	}
	for _, field := range fields {
		if _, ok := wanted[field]; ok {
			return true
		}
	}
	return false
}

// WriteScopePaths extracts path-like targets from mutation args for scope compare.
func WriteScopePaths(tool string, args json.RawMessage) []string {
	tool = strings.TrimSpace(tool)
	paths := pathsFromArgs(args)
	for i := range paths {
		paths[i] = filepath.Clean(paths[i])
	}
	if tool == "multi_edit" || tool == "multi-edit" {
		var payload struct {
			Edits []struct {
				Path string `json:"path"`
			} `json:"edits"`
		}
		if err := json.Unmarshal(args, &payload); err == nil {
			for _, e := range payload.Edits {
				if strings.TrimSpace(e.Path) != "" {
					paths = append(paths, filepath.Clean(e.Path))
				}
			}
		}
	}
	if tool == "bash" {

		return paths
	}
	return uniqueStrings(paths)
}

// ScopeExpanded reports whether the proposal writes outside the failure's
// recorded path set (when both sides have path info).
func ScopeExpanded(failure *FailureEvent, proposal Proposal) bool {
	if proposal.ExpandedScope {
		return true
	}
	if failure == nil {
		return false
	}
	failedPaths := WriteScopePaths(failure.Tool, failure.Args)
	nextPaths := WriteScopePaths(proposal.Tool, proposal.Args)
	if len(failedPaths) == 0 || len(nextPaths) == 0 {
		return false
	}
	allowed := map[string]struct{}{}
	for _, p := range failedPaths {
		allowed[filepath.Clean(p)] = struct{}{}

		allowed[filepath.Clean(filepath.Dir(p))] = struct{}{}
	}
	for _, p := range nextPaths {
		p = filepath.Clean(p)
		if _, ok := allowed[p]; ok {
			continue
		}
		parent := filepath.Clean(filepath.Dir(p))
		if _, ok := allowed[parent]; ok {
			continue
		}

		return true
	}
	return false
}

// StrategyChanged reports an explicit semantic method change. A tool-name
// transition is not enough: the normal recovery flow after a failing verifier
// is to inspect the evidence and edit the diagnosed code. Risk and scope have
// deterministic classifiers; ambiguous method changes are left to the reviewer.
func StrategyChanged(failure *FailureEvent, proposal Proposal) bool {
	_ = failure
	return proposal.StrategyChanged
}

func uniqueStrings(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
