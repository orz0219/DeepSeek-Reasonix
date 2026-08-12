package evidence

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"mvdan.cc/sh/v3/syntax"

	"reasonix/internal/shellparse"
	"reasonix/internal/shellsafe"
)

// commandShowsContentForPath reports whether a bash command demonstrably
// printed the content of the (normalized, slash-lowered) claimed path: a
// content-printing program — cat/head/tail/diff/cmp or git diff/git show —
// whose statically parsed argv names the path exactly or by trailing path
// components. The receipt must contain exactly one simple statement; compound
// statements and pipelines are rejected because unrelated output can satisfy
// the aggregate OutputBytes receipt. Redirected, negated, background, or
// dynamically expanded commands and summary/quiet flags that suppress the
// patch body (--stat, --name-only, -q, …) are rejected too. Matching is
// per-argument and exact, so reading path.bak never satisfies path.
func commandShowsContentForPath(command, needle string) bool {
	file, err := shellparse.ParseBash(command)
	if err != nil || shellparse.HasHereDoc(file) || len(file.Stmts) != 1 {
		return false
	}
	return contentStatementShowsPath(file.Stmts[0], needle)
}

func contentStatementShowsPath(stmt *syntax.Stmt, needle string) bool {
	if stmt == nil || stmt.Negated || stmt.Background || stmt.Coprocess {
		return false
	}
	if len(stmt.Redirs) > 0 {

		return false
	}
	switch cmd := stmt.Cmd.(type) {
	case *syntax.BinaryCmd:

		return false
	case *syntax.CallExpr:
		argv := make([]string, 0, len(cmd.Args))
		for _, w := range cmd.Args {
			f, ok := shellparse.StaticWord(w)
			if !ok {
				return false
			}
			argv = append(argv, f)
		}
		return contentArgvShowsPath(argv, needle)
	default:
		return false
	}
}

// contentSuppressingFlags turn a content command into a summary that never
// shows the patch body; their presence disqualifies the receipt as evidence.
var contentSuppressingFlags = map[string]bool{
	"-q": true, "--quiet": true, "-s": true, "--silent": true,
	"--brief": true, "--no-patch": true, "--name-only": true,
	"--name-status": true, "--numstat": true, "--shortstat": true,
	"--summary": true, "--check": true,
}

func contentArgvShowsPath(argv []string, needle string) bool {
	if len(argv) == 0 {
		return false
	}
	rest := argv[1:]
	gitShow := false
	switch strings.ToLower(filepath.Base(argv[0])) {
	case "cat", "head", "tail", "diff", "cmp":
	case "git":
		if len(rest) == 0 {
			return false
		}
		sub := strings.ToLower(rest[0])
		if sub != "diff" && sub != "show" {
			return false
		}
		gitShow = sub == "show"
		rest = rest[1:]
	default:
		return false
	}
	named := false
	for _, a := range rest {
		lower := strings.ToLower(a)
		if contentSuppressingFlags[lower] || strings.HasPrefix(lower, "--stat") || strings.HasPrefix(lower, "--dirstat") {
			return false
		}
		if gitShow {
			if argNamesGitRevisionPath(a, needle) {
				named = true
			}
		} else if argNamesPath(a, needle) {
			named = true
		}
	}
	return named
}

// argNamesGitRevisionPath accepts only git show's REV:path form. The ordinary
// `git show REV -- path` form can print commit metadata with no file body while
// still producing a non-empty aggregate receipt.
func argNamesGitRevisionPath(arg, needle string) bool {
	tok := strings.ToLower(filepath.ToSlash(normalizePath(arg)))
	if tok == "" || strings.HasPrefix(tok, "-") {
		return false
	}
	i := strings.Index(tok, ":")
	if i <= 0 || i == len(tok)-1 {
		return false
	}
	path := tok[i+1:]
	return path == needle || strings.HasSuffix(path, "/"+needle)
}

// argNamesPath reports whether one static argv token names the claimed path:
// exact after normalization, a trailing-components match of a fuller token,
// or the path part of a git REV:path spec.
func argNamesPath(arg, needle string) bool {
	tok := strings.ToLower(filepath.ToSlash(normalizePath(arg)))
	if tok == "" || strings.HasPrefix(tok, "-") {
		return false
	}
	if tok == needle || strings.HasSuffix(tok, "/"+needle) {
		return true
	}
	if _, after, ok := strings.Cut(tok, ":"); ok {
		rest := after
		if rest == needle || strings.HasSuffix(rest, "/"+needle) {
			return true
		}
	}
	return false
}

func commandReviewsChanges(command string) bool {
	segments, _, ok := shellparse.SplitTopLevel(command)
	if !ok {
		return false
	}
	for _, segment := range segments {
		normalized, safe := shellsafe.NormalizeBashSafeRedirectsForMatch(segment)
		if !safe {
			continue
		}
		fields, malformed := shellparse.StaticFields(normalized)
		if malformed != "" || len(fields) == 0 {
			continue
		}
		base := strings.ToLower(filepath.Base(fields[0]))
		if base == "diff" || base == "cmp" {
			return true
		}
		if base == "git" && len(fields) > 1 {
			sub := strings.ToLower(fields[1])
			if sub == "diff" || sub == "status" || sub == "show" {
				return true
			}
		}
	}
	return false
}

func commandMentionsPaths(command string, wanted map[string]bool) bool {
	normalized := strings.ToLower(strings.ReplaceAll(command, `\`, "/"))
	for path := range wanted {
		if strings.Contains(normalized, strings.ToLower(filepath.ToSlash(path))) {
			return true
		}
	}
	return false
}

func isReadReceipt(name string, readOnly bool) bool {
	switch name {
	case "todo_write", "complete_step":
		return false
	default:
		return isReaderTool(name) || readOnly
	}
}

func isWriterTool(name string) bool {
	switch name {
	case "write_file", "edit_file", "multi_edit", "move_file", "notebook_edit", "delete_range", "delete_symbol":
		return true
	default:
		return false
	}
}

func isReaderTool(name string) bool {
	switch name {
	case "read_file", "ls", "grep":
		return true
	default:
		return false
	}
}

func extractPaths(fields map[string]json.RawMessage) []string {
	var paths []string
	for _, key := range []string{"path", "file_path", "notebook_path", "source_path", "destination_path"} {
		if s := stringField(fields, key); s != "" {
			paths = append(paths, s)
		}
	}
	for _, key := range []string{"paths", "file_paths"} {
		paths = append(paths, stringSliceField(fields, key)...)
	}
	return paths
}

func stringField(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// completeStepIdentity is the citation a receipt records, most stable first: a
// step id survives a replan, an index survives a retitle, the title survives
// neither.
func completeStepIdentity(fields map[string]json.RawMessage) string {
	if id := stringField(fields, "step_id"); id != "" {
		return id
	}
	if n, ok := intField(fields, "step_index"); ok && n > 0 {
		return strconv.Itoa(n)
	}
	return stringField(fields, "step")
}

func intField(fields map[string]json.RawMessage, key string) (int, bool) {
	raw, ok := fields[key]
	if !ok {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return n, true
}

func stringSliceField(fields map[string]json.RawMessage, key string) []string {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	return values
}

func todoItemsField(fields map[string]json.RawMessage, key string) []TodoItem {
	raw, ok := fields[key]
	if !ok {
		return nil
	}
	var todos []TodoItem
	if err := json.Unmarshal(raw, &todos); err != nil {
		return nil
	}
	return normalizeTodos(todos)
}

// A failed complete_step can unlock todo recovery only when the payload had the
// same structural proof shape Execute expects before host verification runs.
func completeStepHasProof(fields map[string]json.RawMessage) bool {
	if strings.TrimSpace(stringField(fields, "result")) == "" {
		return false
	}
	raw, ok := fields["evidence"]
	if !ok {
		return false
	}
	var items []struct {
		Kind    string   `json:"kind"`
		Summary string   `json:"summary"`
		Command string   `json:"command"`
		Paths   []string `json:"paths"`
	}
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		return false
	}
	for _, item := range items {
		kind := strings.TrimSpace(item.Kind)
		if kind == "" || strings.TrimSpace(item.Summary) == "" {
			return false
		}
		switch kind {
		case "verification":
			if strings.TrimSpace(item.Command) == "" {
				return false
			}
		case "diff", "files":
			if len(normalizePaths(item.Paths)) == 0 {
				return false
			}
		case "manual":

		default:
			return false
		}
	}
	return true
}

func normalizeTodos(todos []TodoItem) []TodoItem {
	out := make([]TodoItem, 0, len(todos))
	for _, t := range todos {
		t.Content = strings.TrimSpace(t.Content)
		t.Status = strings.TrimSpace(t.Status)
		t.ActiveForm = strings.TrimSpace(t.ActiveForm)
		out = append(out, t)
	}
	return out
}

func todoStatus(status string) string {
	status = strings.TrimSpace(status)
	if status == "" {
		return "pending"
	}
	return status
}

func previousTodoCompleted(index int, current TodoItem, previous []TodoItem) bool {
	if index >= 1 && index <= len(previous) {
		p := previous[index-1]
		if todoStatus(p.Status) == "completed" && sameTodoIdentity(current, p) {
			return true
		}
	}
	for _, p := range previous {
		if todoStatus(p.Status) == "completed" && sameTodoIdentity(current, p) {
			return true
		}
	}
	return false
}

func hasSuccessfulCompleteStepForTodo(receipts []Receipt, index int, current []TodoItem) bool {
	for _, r := range receipts {
		if !r.Success || r.ToolName != "complete_step" || strings.TrimSpace(r.Step) == "" {
			continue
		}
		if r.TodoStep != nil && r.TodoStep.Found {
			if index < 1 || index > len(current) {
				continue
			}
			if sameTodoMatch(current[index-1], *r.TodoStep) {
				return true
			}
			if !todoContentRelates(current[index-1], *r.TodoStep) {
				continue
			}
		}
		match := matchTodoStep(r.Step, current)
		if match.Found && match.Index == index {
			return true
		}
	}
	return false
}

func latestTodoStep(step string, receipts []Receipt) TodoStepMatch {
	for _, v := range slices.Backward(receipts) {
		r := v
		if !r.Success || r.ToolName != "todo_write" {
			continue
		}
		return matchTodoStep(step, r.Todos)
	}
	return TodoStepMatch{}
}

// matchTodoStep resolves a citation to a todo. A stable id wins outright; only
// a list without ids falls back to position and wording, which a retitle or an
// inserted step silently invalidates.
func matchTodoStep(step string, todos []TodoItem) TodoStepMatch {
	if m, ok := MatchStepID(step, todos); ok {
		return m
	}
	if n, ok := parseStepIndex(normalizeStepText(step)); ok && n >= 1 && n <= len(todos) {
		t := todos[n-1]
		return todoMatchAt(n, t)
	}
	for i, t := range todos {
		if sameStepText(step, t.Content) || sameStepText(step, t.ActiveForm) {
			return todoMatchAt(i+1, t)
		}
	}

	norm := normalizeStepText(step)
	found := -1
	for i, t := range todos {
		if stepTextContains(norm, normalizeStepText(t.Content)) || stepTextContains(norm, normalizeStepText(t.ActiveForm)) {
			if found >= 0 && found != i {
				return TodoStepMatch{}
			}
			found = i
		}
	}
	if found >= 0 {
		t := todos[found]
		return todoMatchAt(found+1, t)
	}
	return TodoStepMatch{}
}

func parseStepIndex(step string) (int, bool) {
	step = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(step), "."))
	n, err := strconv.Atoi(step)
	return n, err == nil
}

// normalizeStepText folds the drift models introduce when citing a todo:
// fullwidth ASCII forms → halfwidth (："５ → :"5), all whitespace dropped,
// case-insensitive.
func normalizeStepText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		b.WriteRune(r)
	}
	return strings.ToLower(strings.Join(strings.Fields(b.String()), ""))
}

func sameStepText(a, b string) bool {
	na, nb := normalizeStepText(a), normalizeStepText(b)
	return na != "" && na == nb
}

// stepTextContains: substring match between normalized texts, but only when the
// shorter side is substantial enough (≥6 runes) to not match by accident.
func stepTextContains(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	short := a
	if utf8.RuneCountInString(b) < utf8.RuneCountInString(a) {
		short = b
	}
	if utf8.RuneCountInString(short) < 6 {
		return false
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}

func pathSet(paths []string) map[string]bool {
	out := map[string]bool{}
	for _, p := range paths {
		if p != "" {
			out[p] = true
		}
	}
	return out
}

func normalizePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = normalizePath(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func normalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, `\`, `/`)
	p = filepath.Clean(filepath.FromSlash(p))
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}
