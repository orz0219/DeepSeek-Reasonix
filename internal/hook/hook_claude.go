package hook

import (
	"encoding/json"
	"path/filepath"
	"strings"
)

// claudePermissionBlocking reports whether exit code 2 (or a timeout) on h
// aborts the action even though PermissionRequest is not one of Reasonix's own
// blocking events (docs/DESKTOP_HOOKS.md: "只有 PreToolUse 和 UserPromptSubmit
// 是阻塞型事件"). Claude's own PermissionRequest contract denies the permission
// on exit 2 the same way PreToolUse does (https://code.claude.com/docs/en/hooks),
// so an imported Claude hook (PayloadFormat "claude") honors that instead of
// silently downgrading to a notification.
func claudePermissionBlocking(h ResolvedHook) bool {
	return h.Event == PermissionRequest && h.PayloadFormat == "claude"
}

// claudeAgentSpawningTools are every Reasonix tool that spawns a subagent and
// so corresponds to Claude's single "Agent" tool: the general task delegator
// (task/read_only_task/parallel_tasks) and the dedicated named wrappers
// around a runAs=subagent skill (BuiltinSubagentTools in
// internal/skill/tools.go — each is a distinct, directly-callable tool, not
// routed through run_skill). A Claude "Agent" safety matcher must see all of
// them, or a hook scoped to it silently misses whichever entry point wasn't
// mapped.
var claudeAgentSpawningTools = []string{
	"task", "read_only_task", "parallel_tasks",
	"explore", "research", "review", "security_review",
}

// claudeAgentDefaultDescriptions fill Claude Agent's required description
// field when the corresponding Reasonix tool does not expose one or the model
// omitted Reasonix's optional description. These are stable operation labels;
// the complete task remains in prompt for hook policy decisions.
var claudeAgentDefaultDescriptions = map[string]string{
	"task":            "Run delegated subagent task",
	"read_only_task":  "Run read-only research task",
	"parallel_tasks":  "Run parallel subagent tasks",
	"explore":         "Explore the codebase",
	"research":        "Research external references",
	"review":          "Review the current changes",
	"security_review": "Review security risks",
}

// claudeToolNames maps Reasonix's own tool names to the *current* Claude Code
// built-in tool name (https://code.claude.com/docs/en/tools-reference) — what
// an imported hook's emitted tool_name payload field shows, and a script's own
// tool_name check is written against. MCP tool names already share the
// mcp__<server>__<tool> convention in both systems.
var claudeToolNames = buildClaudeToolNames()

func buildClaudeToolNames() map[string]string {
	out := map[string]string{
		"bash":            "Bash",
		"read_file":       "Read",
		"write_file":      "Write",
		"edit_file":       "Edit",
		"multi_edit":      "MultiEdit",
		"glob":            "Glob",
		"grep":            "Grep",
		"web_fetch":       "WebFetch",
		"ask":             "AskUserQuestion",
		"run_skill":       "Skill",
		"read_only_skill": "Skill",
		"todo_write":      "TodoWrite",
		"notebook_edit":   "NotebookEdit",
		"bash_output":     "TaskOutput",
		"wait":            "TaskOutput",
		"kill_shell":      "TaskStop",
	}
	for _, name := range claudeAgentSpawningTools {
		out[name] = "Agent"
	}
	return out
}

// claudeToolMatchAliases lists every tool name — current and legacy — an
// imported hook's matcher may have been authored against for a Reasonix
// tool, so a matcher written against an older Claude Code tool name keeps
// firing after Claude renames the tool (Task became Agent; BashOutput/KillShell
// became TaskOutput/TaskStop). claudeFacingToolName (the emitted tool_name
// payload) always reports the current name; only matcher evaluation considers
// aliases.
var claudeToolMatchAliases = buildClaudeToolMatchAliases()

func buildClaudeToolMatchAliases() map[string][]string {
	out := map[string][]string{}
	for _, name := range claudeAgentSpawningTools {
		out[name] = []string{"Agent", "Task"}
	}
	out["bash_output"] = []string{"TaskOutput", "BashOutput"}
	out["wait"] = []string{"TaskOutput", "BashOutput"}
	out["kill_shell"] = []string{"TaskStop", "KillShell"}
	return out
}

// claudeMatchNames returns every name an imported hook's matcher should be
// tried against for a Reasonix tool call.
func claudeMatchNames(name string) []string {
	if aliases, ok := claudeToolMatchAliases[name]; ok {
		return aliases
	}
	return []string{claudeFacingToolName(name)}
}

// claudeFacingToolName returns the current Claude tool name a Claude-imported
// hook's tool_name payload field should see for a Reasonix tool call.
// Reasonix-only tools (wait, code_index, move_file, ...) have no Claude
// equivalent and pass through unchanged — an imported hook can't have been
// authored against a name Claude never had.
func claudeFacingToolName(name string) string {
	if mapped, ok := claudeToolNames[name]; ok {
		return mapped
	}
	return name
}

// claudeToolInputKeyRenames maps, per Reasonix tool name, JSON keys in its
// tool-call arguments that must be renamed to Claude's own tool_input field
// name — Reasonix's file tools use "path", Claude's use "file_path" — so a
// hook script reading e.g. ".tool_input.file_path" sees the value instead of
// failing open on an empty field. Only tools whose Reasonix schema differs
// from Claude's by a plain key rename are listed: Bash's "command",
// Glob/Grep's "pattern"/"path", web_fetch's "url", ask's "questions",
// todo_write's "todos", and task/read_only_task's "prompt"/"description"
// already use Claude's field names. Agent description can still be absent and
// is filled separately below. NotebookEdit's cell_number (a
// 0-based index) has no Claude field — Claude targets cells only by the
// opaque cell_id, which Reasonix also accepts — so it passes through as an
// extra key. parallel_tasks is a structural mismatch handled separately in
// claudeFacingToolInput.
var claudeToolInputKeyRenames = map[string]map[string]string{
	"read_file":       {"path": "file_path"},
	"write_file":      {"path": "file_path"},
	"edit_file":       {"path": "file_path"},
	"multi_edit":      {"path": "file_path"},
	"notebook_edit":   {"path": "notebook_path"},
	"run_skill":       {"name": "skill", "arguments": "args"},
	"read_only_skill": {"name": "skill", "arguments": "args"},
	"bash_output":     {"job_id": "task_id"},
	"kill_shell":      {"job_id": "task_id"},

	"explore":         {"task": "prompt"},
	"research":        {"task": "prompt"},
	"review":          {"task": "prompt"},
	"security_review": {"task": "prompt"},
}

// claudeAbsolutePathInputKeys are the translated tool_input keys whose Claude
// schema demands an absolute path ("must be absolute, not relative" on
// Read/Write/Edit/NotebookEdit). Reasonix's file tools accept relative paths
// and resolve them against the workspace root (resolveIn in
// internal/tool/builtin/workspace.go); the payload resolves against
// payload.Cwd — the same root — so a prefix-matching guard inspects the path
// the tool actually accesses, not a relative spelling it never compares.
var claudeAbsolutePathInputKeys = []string{"file_path", "notebook_path"}

// claudeFacingToolInput adapts tool-call arguments to the tool_input a
// Claude-authored hook script was written against: keys are renamed per
// claudeToolInputKeyRenames, file paths are made absolute, current TaskOutput
// fields and required Agent/AskUserQuestion/TodoWrite fields are supplied, and
// parallel_tasks synthesizes Agent's "prompt". Args needing no translation, or
// that aren't a JSON object, pass through unchanged.
func claudeFacingToolInput(toolName string, args json.RawMessage, cwd string) json.RawMessage {
	renames := claudeToolInputKeyRenames[toolName]
	defaultAgentDescription, isAgent := claudeAgentDefaultDescriptions[toolName]
	if len(renames) == 0 && !isAgent && toolName != "ask" && toolName != "todo_write" && toolName != "wait" {
		return args
	}
	if len(args) == 0 {
		return args
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return args
	}
	changed := false
	for from, to := range renames {
		if v, exists := obj[from]; exists {
			obj[to] = v
			delete(obj, from)
			changed = true
		}
	}
	if toolName == "notebook_edit" {
		if _, exists := obj["new_source"]; !exists {
			for _, alias := range []string{"content", "source", "new_string"} {
				var value string
				if err := json.Unmarshal(obj[alias], &value); err == nil && value != "" {
					obj["new_source"] = obj[alias]
					break
				}
			}
			if _, exists := obj["new_source"]; !exists {
				obj["new_source"] = json.RawMessage(`""`)
			}
			changed = true
		}
	}
	if toolName == "bash_output" {
		obj["block"] = json.RawMessage("false")
		obj["timeout"] = json.RawMessage("0")
		changed = true
	}
	if toolName == "wait" {
		obj["block"] = json.RawMessage("true")
		var jobIDs []string
		if err := json.Unmarshal(obj["job_ids"], &jobIDs); err == nil && len(jobIDs) == 1 {
			if body, err := json.Marshal(jobIDs[0]); err == nil {
				obj["task_id"] = body
			}
		}
		// An unbounded Reasonix wait omits TaskOutput's optional timeout
		// entirely: in Claude's schema timeout is the maximum wait in ms, so
		// claiming 0 would read as "don't wait" — the opposite of the call.
		var timeoutSeconds int64
		if err := json.Unmarshal(obj["timeout_seconds"], &timeoutSeconds); err == nil && timeoutSeconds > 0 && timeoutSeconds <= (1<<63-1)/1000 {
			if body, err := json.Marshal(timeoutSeconds * 1000); err == nil {
				obj["timeout"] = body
			}
		}
		changed = true
	}
	if toolName == "ask" && fillClaudeAskDefaults(obj) {
		changed = true
	}
	if toolName == "todo_write" && fillClaudeTodoDefaults(obj) {
		changed = true
	}

	if toolName == "parallel_tasks" {
		if prompt := joinedParallelTaskPrompts(obj["tasks"]); prompt != "" {
			if v, err := json.Marshal(prompt); err == nil {
				obj["prompt"] = v
				changed = true
			}
		}
	}
	if isAgent {
		var prompt string
		_ = json.Unmarshal(obj["prompt"], &prompt)
		if strings.TrimSpace(prompt) != "" {
			var description string
			_ = json.Unmarshal(obj["description"], &description)
			if strings.TrimSpace(description) == "" {
				if v, err := json.Marshal(defaultAgentDescription); err == nil {
					obj["description"] = v
					changed = true
				}
			}
		}
	}
	for _, key := range claudeAbsolutePathInputKeys {
		v, exists := obj[key]
		if !exists || cwd == "" {
			continue
		}
		var p string
		if err := json.Unmarshal(v, &p); err != nil || p == "" || filepath.IsAbs(p) {
			continue
		}
		if abs, err := json.Marshal(filepath.Join(cwd, p)); err == nil {
			obj[key] = abs
			changed = true
		}
	}
	if !changed {
		return args
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return args
	}
	return out
}

// fillClaudeAskDefaults supplies fields Claude requires but Reasonix treats as
// optional. Empty option descriptions are honest (Reasonix has no explanation
// to add), and omitted multiSelect has the same false default in both systems.
func fillClaudeAskDefaults(obj map[string]json.RawMessage) bool {
	var questions []map[string]json.RawMessage
	if err := json.Unmarshal(obj["questions"], &questions); err != nil {
		return false
	}
	changed := false
	for _, question := range questions {
		if _, exists := question["multiSelect"]; !exists {
			question["multiSelect"] = json.RawMessage("false")
			changed = true
		}
		var options []map[string]json.RawMessage
		if err := json.Unmarshal(question["options"], &options); err != nil {
			continue
		}
		optionsChanged := false
		for _, option := range options {
			if _, exists := option["description"]; !exists {
				option["description"] = json.RawMessage(`""`)
				optionsChanged = true
				changed = true
			}
		}
		if optionsChanged {
			body, err := json.Marshal(options)
			if err != nil {
				return false
			}
			question["options"] = body
		}
	}
	if !changed {
		return false
	}
	body, err := json.Marshal(questions)
	if err != nil {
		return false
	}
	obj["questions"] = body
	return true
}

// fillClaudeTodoDefaults supplies Claude's required activeForm label from the
// Reasonix task content when the caller omitted it.
func fillClaudeTodoDefaults(obj map[string]json.RawMessage) bool {
	var todos []map[string]json.RawMessage
	if err := json.Unmarshal(obj["todos"], &todos); err != nil {
		return false
	}
	changed := false
	for _, todo := range todos {
		var activeForm string
		_ = json.Unmarshal(todo["activeForm"], &activeForm)
		if strings.TrimSpace(activeForm) != "" {
			continue
		}
		var content string
		if err := json.Unmarshal(todo["content"], &content); err != nil || strings.TrimSpace(content) == "" {
			continue
		}
		body, err := json.Marshal(content)
		if err != nil {
			return false
		}
		todo["activeForm"] = body
		changed = true
	}
	if !changed {
		return false
	}
	body, err := json.Marshal(todos)
	if err != nil {
		return false
	}
	obj["todos"] = body
	return true
}

// joinedParallelTaskPrompts flattens a parallel_tasks "tasks" array into one
// prompt string, blank-line separated. Malformed or empty input yields "".
func joinedParallelTaskPrompts(tasks json.RawMessage) string {
	if len(tasks) == 0 {
		return ""
	}
	var items []struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(tasks, &items); err != nil {
		return ""
	}
	var prompts []string
	for _, item := range items {
		if s := strings.TrimSpace(item.Prompt); s != "" {
			prompts = append(prompts, s)
		}
	}
	return strings.Join(prompts, "\n\n")
}
