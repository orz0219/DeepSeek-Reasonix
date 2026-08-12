package recovery

import (
	"path/filepath"
	"strings"

	"reasonix/internal/shellsafe"
)

func gitCommandHighRisk(args []string) bool {
	if containsAny(args, "push", "clean", "prune", "filter-branch", "filter-repo") {
		return true
	}
	if containsAny(args, "gc") {
		return true
	}
	if containsAny(args, "reset") && containsAny(args, "--hard", "--merge", "--keep") {
		return true
	}
	if containsAny(args, "checkout") {

		return true
	}
	if containsAny(args, "switch") && containsAny(args, "--discard-changes") {
		return true
	}
	if containsAny(args, "restore") && (!containsAny(args, "--staged") || containsAny(args, "--worktree")) {

		return true
	}
	if containsAny(args, "branch") && containsAny(args, "-d", "--delete", "-f", "--force") {
		return true
	}
	if containsAny(args, "tag") && containsAny(args, "-d", "--delete", "-f", "--force") {
		return true
	}
	if containsAny(args, "stash") && containsAny(args, "clear", "drop") {
		return true
	}
	if containsAny(args, "reflog") && containsAny(args, "expire", "delete") {
		return true
	}
	if containsAny(args, "worktree") && containsAny(args, "remove", "prune") {
		return true
	}
	if containsAny(args, "update-ref") && containsAny(args, "-d", "--delete", "--stdin") {
		return true
	}
	if containsAny(args, "remote") && containsAny(args, "add", "remove", "rm", "rename", "set-url", "set-head", "set-branches", "prune", "update") {
		return true
	}

	if containsAny(args, "config") {
		if containsAny(args, "--unset", "--unset-all", "--add", "--replace-all", "--rename-section", "--remove-section", "--edit", "-e") {
			return true
		}
		return !containsAny(args, "--get", "--get-all", "--get-regexp", "--get-urlmatch", "--list", "-l", "--name-only")
	}
	return false
}

func curlCommandHighRisk(args []string) bool {
	method := ""
	for i, arg := range args {
		lower := strings.ToLower(arg)
		switch {
		case arg == "-X" || lower == "--request":
			if i+1 >= len(args) {
				return true
			}
			method = strings.ToUpper(args[i+1])
		case strings.HasPrefix(arg, "-X") && len(arg) > 2:
			method = strings.ToUpper(arg[2:])
		case strings.HasPrefix(lower, "--request="):
			method = strings.ToUpper(arg[len("--request="):])
		case arg == "-d" || lower == "--data" || lower == "--data-ascii" || lower == "--data-binary" ||
			lower == "--data-raw" || lower == "--data-urlencode" || lower == "--json" ||
			arg == "-F" || lower == "--form" || lower == "--form-string" ||
			arg == "-T" || lower == "--upload-file":
			return true
		case strings.HasPrefix(arg, "-d") && len(arg) > 2,
			strings.HasPrefix(arg, "-F") && len(arg) > 2,
			strings.HasPrefix(arg, "-T") && len(arg) > 2,
			strings.HasPrefix(lower, "--data="), strings.HasPrefix(lower, "--data-ascii="),
			strings.HasPrefix(lower, "--data-binary="), strings.HasPrefix(lower, "--data-raw="),
			strings.HasPrefix(lower, "--data-urlencode="), strings.HasPrefix(lower, "--json="),
			strings.HasPrefix(lower, "--form="), strings.HasPrefix(lower, "--form-string="),
			strings.HasPrefix(lower, "--upload-file="):
			return true
		}
	}
	return method != "" && method != "GET" && method != "HEAD" && method != "OPTIONS"
}

func wgetCommandHighRisk(args []string) bool {
	for i, arg := range args {
		switch {
		case arg == "--post-data" || arg == "--post-file" || strings.HasPrefix(arg, "--post-data=") || strings.HasPrefix(arg, "--post-file="):
			return true
		case arg == "--method":
			if i+1 >= len(args) {
				return true
			}
			method := strings.ToUpper(args[i+1])
			return method != "GET" && method != "HEAD" && method != "OPTIONS"
		case strings.HasPrefix(arg, "--method="):
			method := strings.ToUpper(strings.TrimPrefix(arg, "--method="))
			return method != "GET" && method != "HEAD" && method != "OPTIONS"
		}
	}
	return false
}

func ghCommandHighRisk(args []string) bool {
	group, rest := ghCommandGroup(args)
	switch group {
	case "api":
		return ghAPICommandHighRisk(rest)
	case "pr":
		return containsAny(rest, "create", "close", "comment", "edit", "merge", "ready", "reopen", "review")
	case "issue":
		return containsAny(rest, "create", "close", "comment", "delete", "edit", "reopen", "transfer", "pin", "unpin", "lock", "unlock")
	case "repo":
		return containsAny(rest, "create", "delete", "archive", "edit", "fork", "rename", "sync")
	case "release":
		return containsAny(rest, "create", "delete", "edit", "upload")
	case "workflow":
		return containsAny(rest, "run", "enable", "disable")
	case "run":
		return containsAny(rest, "cancel", "delete", "rerun")
	case "secret", "variable":
		return containsAny(rest, "set", "delete")
	case "label":
		return containsAny(rest, "create", "delete", "edit", "clone")
	case "gist":
		return containsAny(rest, "create", "delete", "edit")
	case "ssh-key", "gpg-key":
		return containsAny(rest, "add", "delete")
	case "cache":
		return containsAny(rest, "delete")
	case "auth":
		return containsAny(rest, "login", "logout", "refresh", "setup-git", "switch")
	case "alias":
		return containsAny(rest, "set", "delete")
	case "config":
		return containsAny(rest, "set", "clear")
	case "extension":
		return containsAny(rest, "install", "remove", "upgrade", "create")
	case "project", "codespace":
		return !containsAny(rest, "list", "view", "status", "logs")
	}
	return false
}

func ghCommandGroup(args []string) (string, []string) {
	groups := map[string]struct{}{
		"api": {}, "pr": {}, "issue": {}, "repo": {}, "release": {}, "workflow": {}, "run": {},
		"secret": {}, "variable": {}, "label": {}, "gist": {}, "ssh-key": {}, "gpg-key": {},
		"cache": {}, "auth": {}, "alias": {}, "config": {}, "extension": {}, "project": {}, "codespace": {},
	}
	for i, arg := range args {
		if _, ok := groups[arg]; ok {
			return arg, args[i+1:]
		}
	}
	return "", nil
}

func ghAPICommandHighRisk(args []string) bool {
	method := ""
	hasBody := false
	for i, arg := range args {
		switch {
		case arg == "-x" || arg == "--method":
			if i+1 >= len(args) {
				return true
			}
			method = strings.ToUpper(args[i+1])
		case strings.HasPrefix(arg, "-x") && len(arg) > 2:
			method = strings.ToUpper(arg[2:])
		case strings.HasPrefix(arg, "--method="):
			method = strings.ToUpper(strings.TrimPrefix(arg, "--method="))
		case arg == "-f" || arg == "--raw-field" || arg == "--field" || arg == "--input":
			hasBody = true
		case strings.HasPrefix(arg, "-f") && len(arg) > 2:
			hasBody = true
		case strings.HasPrefix(arg, "--raw-field=") || strings.HasPrefix(arg, "--field=") || strings.HasPrefix(arg, "--input="):
			hasBody = true
		}
	}
	if method == "" {
		return hasBody
	}
	return method != "GET" && method != "HEAD" && method != "OPTIONS"
}

func commandFieldsKnownSafeMutation(fields []string) bool {
	if len(fields) == 0 || commandFieldsHighRisk(fields) {
		return false
	}
	base := strings.ToLower(filepath.Base(fields[0]))
	rawArgs := fields[1:]
	args := lowerFields(rawArgs)
	switch base {
	case "env":
		wrapped, ok := unwrapEnvCommand(rawArgs)
		return ok && commandFieldsKnownSafeMutation(wrapped)
	case "command":
		wrapped, ok := unwrapCommandBuiltin(rawArgs)
		return ok && (len(wrapped) == 0 || commandFieldsKnownSafeMutation(wrapped))
	case "nohup":
		wrapped := trimLeadingOptions(rawArgs)
		return len(wrapped) > 0 && commandFieldsKnownSafeMutation(wrapped)
	case "git":
		return gitCommandKnownSafe(args)
	case "curl":
		return !curlCommandHighRisk(rawArgs)
	case "wget":
		return !wgetCommandHighRisk(args)
	case "gh":
		return !ghCommandHighRisk(args)
	case "http", "https", "xh":
		return !httpCommandHighRisk(args)
	case "sed", "gofmt", "goimports", "rustfmt", "prettier", "biome", "eslint", "black", "ruff",
		"cp", "mv", "mkdir", "touch", "ln":

		return true
	case "npm":
		return containsAny(args, "install", "add", "remove", "uninstall", "update", "dedupe") && !hasGlobalFlag(args)
	case "pnpm":
		return containsAny(args, "install", "add", "remove", "update", "dedupe", "import") && !hasGlobalFlag(args)
	case "yarn":
		return containsAny(args, "install", "add", "remove", "up", "upgrade", "dedupe") && !hasGlobalFlag(args) && !containsAny(args, "global")
	case "go":
		return containsAny(args, "get", "mod", "work", "fmt", "build", "test") && !containsAny(args, "install", "clean")
	case "cargo":
		return containsAny(args, "add", "remove", "update", "build", "check", "test", "fmt", "fix", "clippy")
	case "composer":
		return containsAny(args, "require", "remove", "update", "install", "dump-autoload") && !hasGlobalFlag(args) && !containsAny(args, "global")
	case "poetry":
		return containsAny(args, "add", "remove", "install", "update", "lock", "sync")
	case "uv":
		return containsAny(args, "add", "remove", "sync", "lock")
	case "dotnet":
		return containsAny(args, "add", "remove", "restore", "build", "test", "format") && !hasGlobalFlag(args)
	}

	if _, _, readOnly := shellsafe.CommandIsReadOnly(strings.Join(fields, " ")); readOnly {
		return true
	}
	return false
}

func gitCommandKnownSafe(args []string) bool {
	sub := gitSubcommand(args)
	switch sub {
	case "add", "commit", "status", "diff", "log", "show", "rev-parse", "rev-list", "describe",
		"blame", "grep", "ls-files", "ls-tree", "cat-file", "for-each-ref", "name-rev", "shortlog",
		"whatchanged", "cherry", "fetch", "pull", "clone", "init", "merge", "rebase", "cherry-pick",
		"revert", "apply", "am", "switch", "reset", "branch", "tag", "stash", "restore", "worktree",
		"remote", "config", "reflog":
		return true
	default:
		return false
	}
}

func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-c" || arg == "--git-dir" || arg == "--work-tree" || arg == "--namespace":
			i++
		case strings.HasPrefix(arg, "-"):
			continue
		default:
			return strings.ToLower(arg)
		}
	}
	return ""
}
