package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

func toEventShellExecution(in *tool.ShellExecution, durationMs int64) *event.ShellExecution {
	if in == nil {
		return nil
	}
	out := &event.ShellExecution{
		Kind:           in.Kind,
		Shell:          in.Shell,
		ShellVersion:   in.ShellVersion,
		Platform:       in.Platform,
		SupportsAndAnd: in.SupportsAndAnd,
		State:          in.State,
		FailurePhase:   in.FailurePhase,
		OutputTail:     in.OutputTail,
		MutationRisk:   in.MutationRisk,
		Verification:   in.Verification,
		DurationMs:     in.DurationMs,
	}
	if out.DurationMs == 0 && durationMs > 0 {
		out.DurationMs = durationMs
	}
	if in.ExitCode != nil {
		code := *in.ExitCode
		out.ExitCode = &code
	}
	return out
}

func toProviderToolExecution(in *tool.ShellExecution) *provider.ToolExecution {
	if in == nil {
		return nil
	}
	out := &provider.ToolExecution{
		Kind:           in.Kind,
		Shell:          in.Shell,
		ShellVersion:   in.ShellVersion,
		Platform:       in.Platform,
		SupportsAndAnd: in.SupportsAndAnd,
		State:          in.State,
		FailurePhase:   in.FailurePhase,
		OutputTail:     in.OutputTail,
		MutationRisk:   in.MutationRisk,
		Verification:   in.Verification,
		DurationMs:     in.DurationMs,
	}
	if in.ExitCode != nil {
		code := *in.ExitCode
		out.ExitCode = &code
	}
	return out
}

func (a *Agent) emitFullToolDispatch(ctx context.Context, c provider.ToolCall, refreshed bool) {
	t, _, ambiguous := a.svc.tools.ResolveCall(c.Name)
	ok := t != nil && len(ambiguous) == 0
	ev := event.Tool{ID: c.ID, Name: c.Name, Args: c.Arguments, ReadOnly: ok && t.ReadOnly(), Refreshed: refreshed}
	ev.FileDiff = event.FileDiff{Diff: c.Diff, Added: c.Added, Removed: c.Removed}
	if ok && ev.Diff == "" && ev.Added == 0 && ev.Removed == 0 {
		if ch, ok := tool.PreviewChange(ctx, t, json.RawMessage(c.Arguments)); ok {
			ev.FileDiff = event.FileDiff{Diff: ch.Diff, Added: ch.Added, Removed: ch.Removed}
		}
	}
	if ok {
		if pr, ok := t.(interface {
			ResolveProfile(json.RawMessage) *event.Profile
		}); ok {
			ev.Profile = pr.ResolveProfile(json.RawMessage(c.Arguments))
		}
	}
	a.svc.sink.Emit(event.Event{Kind: event.ToolDispatch, Tool: ev})
}

// emitResolvedToolDispatch upserts the real target classification of a stable
// proxy call without changing the provider-visible Name/Args. Append-only sinks
// ignore Refreshed events; stateful frontends replace the existing card by ID.
func (a *Agent) emitResolvedToolDispatch(c provider.ToolCall) {
	if c.ResolvedReadOnly == nil {
		return
	}
	if c.ResolvedName != "" && c.ResolvedName != c.Name {
		EmitProxyAudit(a.svc.sink, tool.ResolvedCall{
			DisplayName:  c.Name,
			TargetName:   c.ResolvedName,
			CapabilityID: c.CapabilityID,
		})
	}
	a.svc.sink.Emit(event.Event{Kind: event.ToolDispatch, Tool: event.Tool{
		ID:           c.ID,
		Name:         c.Name,
		Args:         c.Arguments,
		ResolvedName: c.ResolvedName,
		CapabilityID: c.CapabilityID,
		ReadOnly:     *c.ResolvedReadOnly,
		Refreshed:    true,
		FileDiff: event.FileDiff{
			Diff: c.Diff, Added: c.Added, Removed: c.Removed,
		},
	}})
}

// refreshCurrentFileDiff recomputes a writer preview against the state left by
// earlier successful writers in the same provider batch. Preview failures clear
// any stale initial diff; a later Execute will then fail or ask for recovery
// without presenting the user with a preview that no longer describes disk.
func refreshCurrentFileDiff(ctx context.Context, t tool.Tool, call provider.ToolCall) (provider.ToolCall, bool) {
	pv, ok := t.(tool.Previewer)
	if !ok {
		return call, false
	}
	refreshed := call
	refreshed.Diff = ""
	refreshed.Added = 0
	refreshed.Removed = 0
	if change, err := pv.Preview(ctx, json.RawMessage(call.Arguments)); err == nil {
		refreshed.Diff = change.Diff
		refreshed.Added = change.Added
		refreshed.Removed = change.Removed
	}
	changed := refreshed.Diff != call.Diff || refreshed.Added != call.Added || refreshed.Removed != call.Removed
	return refreshed, changed
}

func (a *Agent) withPreviewFileDiffs(ctx context.Context, calls []provider.ToolCall) []provider.ToolCall {
	if len(calls) == 0 {
		return calls
	}
	out := make([]provider.ToolCall, len(calls))
	copy(out, calls)
	for i := range out {
		if out[i].Diff != "" || out[i].Added != 0 || out[i].Removed != 0 {
			continue
		}
		t, _, ambiguous := a.svc.tools.ResolveCall(out[i].Name)
		ok := t != nil && len(ambiguous) == 0
		if !ok {
			continue
		}
		if ch, ok := tool.PreviewChange(ctx, t, json.RawMessage(out[i].Arguments)); ok {
			out[i].Diff = ch.Diff
			out[i].Added = ch.Added
			out[i].Removed = ch.Removed
		}
	}
	return out
}

// completedMCPConnect recognizes a synthetic cache-miss connect call whose
// background discovery finished after the provider request was serialized. The
// connect placeholder is intentionally absent once real tools replace it, but
// the already-advertised call still completed its only job and must not surface
// as an unknown tool.
func completedMCPConnect(reg *tool.Registry, name string) (string, bool) {
	server, rawName, ok := tool.SplitMCPName(name)
	if !ok || rawName != "connect" {
		return "", false
	}
	prefix := tool.MCPNamePrefix + server + "__"
	for _, current := range reg.Names() {
		if current != name && strings.HasPrefix(current, prefix) {
			return server, true
		}
	}
	return "", false
}

// recoveryPlanTransition detects structural rewrites of an active canonical
// task list. Initial plans and progress-only status updates stay on the fast
// path; changing step identity, order, or hierarchy while work remains is a
// semantic transition for the independent Auto reviewer.
func (a *Agent) recoveryPlanTransition(toolName string, args json.RawMessage) (bool, string, string, string) {
	if a == nil || toolName != "todo_write" || a.planMode.Load() {
		return false, "", "", ""
	}
	before := a.CanonicalTodoState()
	if len(before) == 0 || len(evidence.IncompleteTodos(before)) == 0 {
		return false, "", "", ""
	}
	after := evidence.ReceiptFromToolCall("todo_write", args, true, true).Todos
	if len(after) == 0 || evidence.ValidateSerialTodos(after) != nil || !evidence.PreservesCompletedTodoPositions(before, after) {

		return false, "", "", ""
	}
	if samePlanStructure(before, after) {
		return false, "", "", ""
	}
	return true, planReviewText(before), planReviewText(after), planTransitionDiff(before, after)
}

func samePlanStructure(a, b []evidence.TodoItem) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Level != b[i].Level || normalizePlanStep(a[i].Content) != normalizePlanStep(b[i].Content) {
			return false
		}
	}
	return true
}

func normalizePlanStep(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func planReviewText(todos []evidence.TodoItem) string {
	var b strings.Builder
	for i, todo := range todos {
		indent := ""
		if todo.Level == 1 {
			indent = "  "
		}
		fmt.Fprintf(&b, "%s%d. %s [%s]", indent, i+1, normalizePlanStep(todo.Content), canonicalTodoStatus(todo.Status))
		if i+1 < len(todos) {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func recoveryTaskScopeID(deliveryScopeID string, runSeq uint64) string {
	if scope := strings.TrimSpace(deliveryScopeID); scope != "" {
		return "goal:" + scope
	}
	return fmt.Sprintf("turn:%d", runSeq)
}
