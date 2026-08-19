package recovery

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"reasonix/internal/evidence"
	"reasonix/internal/shellparse"
	"reasonix/internal/shellsafe"
)

// QualifyingFailure reports whether an observation should arm the checkpoint.
// User rejections, host policy blocks, cancels, provider errors, and empty
// search results never qualify.
func QualifyingFailure(obs Observation) bool {
	if obs.Success || obs.Blocked || obs.UserRejected || obs.ProviderError || obs.Cancelled || obs.EmptySearch {
		return false
	}
	// Mutating tool failure always qualifies.
	if obs.Mutates {
		return true
	}
	// Host-recognized verification command non-zero exit.
	if obs.Verification {
		return true
	}
	// File/shell/MCP tools that can change state but reported non-readonly.
	if !obs.ReadOnly && strings.TrimSpace(obs.Tool) != "" {
		return true
	}
	return false
}

// ClassifyFailure identifies the owning recovery policy without treating an
// execution reliability problem as a permission or user-decision boundary.
// The classifier is deliberately narrow: permission/sandbox/user blocks are
// filtered by QualifyingFailure before this is called.
func ClassifyFailure(obs Observation) FailureClass {
	if transientFailureText(obs.ErrSummary) || transientFailureText(obs.Output) {
		return FailureClassTransient
	}
	if obs.Verification {
		return FailureClassVerification
	}
	if obs.Mutates {
		return FailureClassMutation
	}
	return FailureClassExecution
}

func transientFailureText(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return false
	}
	for _, marker := range []string{
		"command timed out",
		"timed out after",
		"timed out (>",
		"context deadline exceeded",
		"deadline exceeded",
		"execution timeout",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// IsVerificationCall reports whether the host recognizes the call as a
// verification command (test/lint/build/typecheck/compile).
func IsVerificationCall(tool string, args json.RawMessage, readOnly bool) bool {
	tool = strings.TrimSpace(tool)
	if tool == "bash" {
		return evidence.IsDeliveryVerificationCommand(commandFromArgs(args))
	}
	// Project-check style tools are verification even when not bash.
	switch tool {
	case "complete_step":
		return false
	}
	_ = readOnly
	return false
}

// IsSafeVerificationRetry reports whether proposal is a first safe retry of the
// same host-proven verification command that failed.
// Callers must also consult the runtime safe-retry budget (safeRetryUsed /
// SafeRetryLeft); a spent budget never qualifies.
func IsSafeVerificationRetry(failure *FailureEvent, proposal Proposal) bool {
	if failure == nil || !failure.Verification {
		return false
	}
	if failure.SafeRetryLeft <= 0 {
		// evidenceCopy sets SafeRetryLeft from runtime truth; 0 means spent.
		return false
	}
	if !proposal.Verification || proposal.HighRisk || proposal.ExpandedScope || proposal.StrategyChanged {
		return false
	}
	if strings.TrimSpace(proposal.Tool) != strings.TrimSpace(failure.Tool) {
		return false
	}
	// Same normalized command / subject for verification retries.
	if normalizeCommand(proposal.Subject) != "" && normalizeCommand(failure.Subject) != "" {
		return normalizeCommand(proposal.Subject) == normalizeCommand(failure.Subject)
	}
	return CallFingerprint(proposal.Tool, proposal.Subject, "", proposal.Args) ==
		CallFingerprint(failure.Tool, failure.Subject, "", failure.Args)
}

// IsHighRiskMutation preserves the legacy execution-risk classifier for event
// compatibility and focused policy tests. Auto no longer turns this result into
// a human confirmation; permission, sandbox, and tool policy own that boundary.
func IsHighRiskMutation(proposal Proposal) bool {
	return riskBoundaryForProposal(proposal).highRisk
}

// TaskGrantKey returns the legacy semantic key used by persisted recovery cards.
// New Auto decisions do not create execution-risk grants. Keys remain narrower
// than a command name but broader than raw command bytes:
// for example, ordinary pushes to the same Git remote destination share a key,
// while a different ref, force push, or arbitrary HTTP/API mutation never does.
func TaskGrantKey(proposal Proposal) string {
	return riskBoundaryForProposal(proposal).taskGrantKey
}

type riskBoundary struct {
	highRisk         bool
	taskGrantKey     string
	taskGrantDisplay string
}

func riskBoundaryForProposal(proposal Proposal) riskBoundary {
	if proposal.HighRisk {
		// Caller-supplied risk has no host-proven semantic scope, so it is never
		// eligible for a reusable task grant.
		return riskBoundary{highRisk: true}
	}
	tool := strings.TrimSpace(proposal.Tool)
	if strings.HasPrefix(tool, "mcp__") || strings.Contains(tool, "mcp") {
		// MCP already has a richer policy/destructive-hint gate. Duplicating that
		// prompt here would create two human decisions for one call.
		return riskBoundary{}
	}
	if tool == "bash" {
		cmd := commandFromArgs(proposal.Args)
		// Host-recognized test/build commands may create project-local artifacts,
		// but are already bounded by the verification classifier. Deterministic
		// destructive forms still trip commandFieldsHighRisk below.
		return bashRiskBoundary(cmd, proposal.Mutates && !proposal.Verification)
	}
	// Workspace file tools remain on Auto's fast path, including dependency,
	// configuration, and workflow files. Sandbox and explicit approval policy
	// still own writes outside the workspace; this layer only adds hard-boundary
	// confirmation for commands the host can classify deterministically.
	return riskBoundary{}
}

// ClassifyEmptySearch reports whether a successful read-only search produced
// no matches. Callers set Observation.EmptySearch from this.
func ClassifyEmptySearch(tool string, success bool, readOnly bool, output string) bool {
	if !success || !readOnly {
		return false
	}
	switch strings.TrimSpace(tool) {
	case "grep", "glob", "ls", "code_index", "codeindex":
		// fall through
	default:
		return false
	}
	out := strings.TrimSpace(output)
	if out == "" {
		return true
	}
	lower := strings.ToLower(out)
	for _, marker := range []string{
		"no matches",
		"no files found",
		"0 matches",
		"not found",
		"no results",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// IsDiagnosticSuccess reports a successful read-only diagnostic that must not
// clear the active failure event (ls/rg/grep/read_file, etc.).
func IsDiagnosticSuccess(obs Observation) bool {
	if !obs.Success || obs.Mutates || obs.Verification {
		return false
	}
	switch strings.TrimSpace(obs.Tool) {
	case "bash":
		cmd := commandFromArgs(obs.Args)
		base, _, readOnly := shellsafe.CommandIsReadOnly(cmd)
		if !readOnly {
			return false
		}
		switch strings.ToLower(filepath.Base(base)) {
		case "ls", "rg", "grep", "find", "cat", "head", "tail", "wc", "file", "stat", "pwd", "which", "type":
			return true
		}
		return true // other host-proven read-only bash diagnostics
	case "read_file", "grep", "glob", "ls", "code_index", "codeindex":
		return obs.ReadOnly
	default:
		return false
	}
}

func commandFromArgs(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil {
		return ""
	}
	raw, ok := fields["command"]
	if !ok {
		return ""
	}
	var cmd string
	if err := json.Unmarshal(raw, &cmd); err != nil {
		return ""
	}
	return strings.TrimSpace(cmd)
}

func pathsFromArgs(args json.RawMessage) []string {
	if len(args) == 0 {
		return nil
	}
	var fields map[string]any
	if err := json.Unmarshal(args, &fields); err != nil {
		return nil
	}
	var paths []string
	for _, key := range []string{
		"path", "file_path", "file", "target", "destination",
		"source_path", "destination_path", "old_path", "new_path",
	} {
		if v, ok := fields[key].(string); ok && strings.TrimSpace(v) != "" {
			paths = append(paths, strings.TrimSpace(v))
		}
	}
	return uniqueStrings(paths)
}

func normalizeCommand(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func bashRiskBoundary(command string, enforceMutationAllowlist bool) riskBoundary {
	command = strings.TrimSpace(command)
	if command == "" {
		return riskBoundary{highRisk: true}
	}
	lower := strings.ToLower(command)
	// Fast markers cover destructive redirection and commands whose static
	// tokenization may be obscured by shell punctuation. Project-local installs
	// and version-controlled configuration edits intentionally stay automatic.
	riskMarkers := []string{
		"rm -", "rmdir", "unlink ", "shred ",
		"git reset --hard", "git clean",
		"chmod ", "chown ", "mkfs", "dd if=",
		"> /", ">> /",
	}
	for _, m := range riskMarkers {
		if strings.Contains(lower, m) {
			return riskBoundary{highRisk: true}
		}
	}
	segments, _, ok := shellparse.SplitTopLevel(command)
	if !ok {
		return riskBoundary{highRisk: true}
	}
	var grant taskGrantBoundary
	for _, segment := range segments {
		fields, malformed := shellparse.StaticFields(segment)
		if malformed != "" || len(fields) == 0 {
			return riskBoundary{highRisk: true}
		}
		if commandFieldsHighRisk(fields) {
			if len(segments) == 1 {
				grant = commandFieldsTaskGrantBoundary(fields)
			}
			return riskBoundary{
				highRisk:         true,
				taskGrantKey:     grant.key,
				taskGrantDisplay: grant.display,
			}
		}
		if enforceMutationAllowlist && !commandFieldsKnownSafeMutation(fields) {
			// The host knows this call can mutate, but this policy cannot prove it is
			// a reversible workspace operation. Fail closed instead of letting an
			// unlisted shell or PowerShell command silently widen Auto.
			return riskBoundary{highRisk: true}
		}
	}
	return riskBoundary{}
}

func commandFieldsHighRisk(fields []string) bool {
	if len(fields) == 0 {
		return true
	}
	base := strings.ToLower(filepath.Base(fields[0]))
	rawArgs := fields[1:]
	args := lowerFields(rawArgs)
	switch base {
	case "sudo", "doas", "pkexec", "xargs":
		// Privilege escalation and dynamic command dispatch are high risk even
		// when the wrapped command itself is not statically recoverable here.
		return true
	case "env":
		wrapped, ok := unwrapEnvCommand(rawArgs)
		return !ok || commandFieldsHighRisk(wrapped)
	case "command":
		wrapped, ok := unwrapCommandBuiltin(rawArgs)
		return !ok || (len(wrapped) > 0 && commandFieldsHighRisk(wrapped))
	case "nohup":
		return commandFieldsHighRisk(trimLeadingOptions(rawArgs))
	case "rm", "rmdir", "unlink", "shred", "dd", "mkfs", "chmod", "chown",
		"docker", "kubectl", "terraform":
		return true
	case "remove-item", "clear-content", "set-content", "add-content", "move-item", "copy-item",
		"new-item", "rename-item", "invoke-restmethod", "invoke-webrequest", "start-process",
		"stop-process", "restart-computer", "stop-computer", "format-volume", "clear-disk",
		"initialize-disk", "powershell", "powershell.exe", "pwsh", "pwsh.exe", "cmd", "cmd.exe",
		"del", "erase", "rd", "format", "diskpart":
		// Reasonix runs the bash tool through PowerShell on Windows. Bash AST still
		// gives us useful static words for simple native commands, but these verbs
		// are not reversible workspace operations and must never fall through.
		return true
	case "find":
		return containsAny(args, "-delete", "-exec", "-execdir", "-ok", "-okdir")
	case "git":
		return gitCommandHighRisk(args)
	case "curl":
		return curlCommandHighRisk(rawArgs)
	case "wget":
		return wgetCommandHighRisk(args)
	case "gh":
		return ghCommandHighRisk(args)
	case "http", "https", "xh":
		return httpCommandHighRisk(args)
	case "aws", "gcloud", "az", "oci", "doctl", "heroku", "vercel", "netlify",
		"flyctl", "railway", "firebase", "wrangler", "cloudflared", "ssh", "scp",
		"sftp", "rsync", "psql", "mysql", "redis-cli", "mongosh":
		// These tools can mutate remote services or hosts, and their command
		// languages are too broad for this layer to prove a call read-only. Keep
		// them behind Auto's explicit external-action boundary.
		return true
	case "npm":
		return containsAny(args, "publish", "unpublish", "link", "unlink", "config") || hasGlobalFlag(args)
	case "pnpm":
		return containsAny(args, "publish", "deploy", "link", "unlink", "setup") || hasGlobalFlag(args) ||
			(containsAny(args, "env") && containsAny(args, "use", "remove") && containsAny(args, "--global"))
	case "yarn":
		return containsAny(args, "publish", "link", "unlink") || hasGlobalFlag(args) ||
			(containsAny(args, "global") && containsAny(args, "add", "remove", "upgrade"))
	case "pip", "pip3", "pipx":
		// Python installers mutate the active interpreter environment unless the
		// host can prove a project-local target, which this command layer cannot.
		return containsAny(args, "install", "uninstall", "inject", "upgrade")
	case "brew", "apt", "apt-get", "dnf", "yum", "apk", "pacman":
		return containsAny(args, "install", "add", "remove", "uninstall", "upgrade", "update")
	case "go":
		if containsAny(args, "install", "clean") {
			return true
		}
		if containsAny(args, "env") && containsAny(args, "-w", "-u") {
			return true
		}
		return false
	case "cargo":
		return containsAny(args, "install", "uninstall", "publish", "yank", "login", "logout")
	case "composer":
		return (containsAny(args, "config") && hasGlobalFlag(args)) ||
			(containsAny(args, "global") && containsAny(args, "require", "remove", "update", "install", "config", "exec"))
	case "poetry":
		return containsAny(args, "publish", "config", "self")
	case "uv":
		return containsAny(args, "publish", "tool")
	case "dotnet":
		return containsAny(args, "push", "delete") || hasGlobalFlag(args)
	case "gem", "bundle", "bundler":
		return containsAny(args, "install", "uninstall", "update", "add", "remove", "push", "yank", "publish")
	}
	return false
}

// `git checkout .` and `git checkout path` discard worktree contents even
// without -f/--. Prefer the unambiguous switch command for safe branch
// changes; keep all checkout forms behind confirmation.

// Restoring only the index is reversible from the worktree; restoring the
// worktree can discard the user's uncommitted contents.

// Repository-local git config is not version-controlled workspace config and
// can redirect tool output, credentials, or future pushes. Read-only config probes
// are the only fast path.

// gh api switches its default from GET to POST when fields/input are supplied.

// These are deterministic workspace-editing families. The ordinary
// permission/sandbox layer still owns path confinement.

// A coarse host mutation bit must not turn a statically proven read-only
// diagnostic into a confirmation. Destructive argument forms were rejected
// before reaching this point.

// Global options such as -C/--git-dir can redirect an otherwise identical
// command to another repository. Keep those forms one-shot because the
// displayed remote alias would no longer identify the same target context.

// Behavior-changing and unknown push options are deliberately one-shot.
// In particular, push-option/receive-pack/no-verify must not inherit a
// grant issued for an ordinary push to the same ref.

// A reusable grant needs both an explicit remote and exactly one explicit
// refspec. Bare `git push` depends on mutable branch/upstream configuration.

// Options before a positional target are legal in gh. Avoid guessing
// through their values; a form the host cannot scope exactly stays
// one-shot rather than sharing an accidentally broad "current" grant.

// "current" can change after a checkout or branch switch. Require an
// explicit PR/issue target before offering a reusable external-write grant.

// HTTPie query-string item; remains a GET by default.

// HTTPie-style request items with a value or file body implicitly switch
// the default method from GET to a mutating request.

// Split-string and unknown options can change the command shape.

// Inspection-only command lookup; there is no wrapped execution.

// WriteScopePaths extracts path-like targets from mutation args for scope compare.

// Best-effort: do not invent paths from free-form shell.

// ScopeExpanded reports whether the proposal writes outside the failure's
// recorded path set (when both sides have path info).

// Allow writes under the same directory as a failed file target.

// Outside all known failed paths.

// StrategyChanged reports an explicit semantic method change. A tool-name
// transition is not enough: the normal recovery flow after a failing verifier
// is to inspect the evidence and edit the diagnosed code. Risk and scope have
// deterministic classifiers; ambiguous method changes are left to the reviewer.
