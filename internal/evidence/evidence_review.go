package evidence

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"reasonix/internal/shellparse"
)

// BackgroundLease identifies a background job whose evidence was provisionally
// merged into the current turn's ledger. The host commits these leases only
// after the turn passes its delivery gates, so a failed turn leaves the job's
// evidence collectable again.
type BackgroundLease struct {
	Session string
	JobID   string
}

// HasWriteOrCommandSince reports whether a successful write or command receipt
// was recorded at or after index — host-observable progress, as opposed to
// bookkeeping receipts (todo_write, complete_step, ask), which carry neither a
// write flag nor a command.
func (l *Ledger) HasWriteOrCommandSince(index int) bool {
	if l == nil {
		return false
	}
	if index < 0 {
		index = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := index; i < len(l.receipts); i++ {
		r := l.receipts[i]
		if r.Success && (r.Mutation || r.Write || r.Command != "") {
			return true
		}
	}
	return false
}

func (l *Ledger) HasSuccessfulCommand(command string) bool {
	command = strings.TrimSpace(command)
	if l == nil || command == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if r.Success && r.ToolName == "bash" && CommandMatches(command, r.Command) {
			return true
		}
	}
	return false
}

// HasCompletedReview reports whether a review completed with evidence that is
// fresh for the latest mutation. Structured review_report receipts are the
// strongest proof and also cover collected background reviews. Foreground
// review/task adapters remain compatible, but after a mutation their child
// receipts must show that the changed result was actually inspected.
func (l *Ledger) HasCompletedReview() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	receipts := append([]Receipt(nil), l.receipts...)
	l.mu.Unlock()

	mutation := -1
	for i, r := range receipts {
		if r.Success && r.Mutation {
			mutation = i
		}
	}
	start := mutation + 1
	requiredPaths := []string(nil)
	if mutation >= 0 {
		requiredPaths = receipts[mutation].Paths
	}

	for i := start; i < len(receipts); i++ {
		r := receipts[i]
		if completedStructuredReviewReceipt(r, requiredPaths) {
			return true
		}
		if !successfulForegroundReviewReceipt(r) {
			continue
		}
		if mutation < 0 || receiptsReviewChanges(receipts, start, i, mutation) {
			return true
		}
	}
	return false
}

func successfulForegroundReviewReceipt(r Receipt) bool {
	if !r.Success {
		return false
	}
	if r.ToolName == "review" {
		return true
	}
	if r.ToolName != "task" || r.Profile != "review" {
		return false
	}
	var p struct {
		RunInBackground bool `json:"run_in_background"`
	}
	return json.Unmarshal(r.Args, &p) == nil && !p.RunInBackground
}

func completedStructuredReviewReceipt(r Receipt, requiredPaths []string) bool {
	if !r.Success || r.ToolName != "review_report" {
		return false
	}
	report, err := ParseReviewReport(r.Args)
	return err == nil && report.Kind == ReviewKindReview && report.CoversPaths(requiredPaths)
}

// HasFailedCommand reports whether the cited command ran this turn but exited
// non-zero — so callers can distinguish "ran and failed" from "never ran".
func (l *Ledger) HasFailedCommand(command string) bool {
	command = strings.TrimSpace(command)
	if l == nil || command == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.receipts {
		if !r.Success && r.ToolName == "bash" && CommandMatches(command, r.Command) {
			return true
		}
	}
	return false
}

// SuccessfulCommands returns up to limit successful bash commands from this
// turn, most recent first, for self-correction hints in rejection errors.
func (l *Ledger) SuccessfulCommands(limit int) []string {
	if l == nil || limit <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for i := len(l.receipts) - 1; i >= 0 && len(out) < limit; i-- {
		r := l.receipts[i]
		if r.Success && r.ToolName == "bash" && r.Command != "" {
			out = append(out, r.Command)
		}
	}
	return out
}

// TouchedPaths returns up to limit distinct paths from this turn's successful
// receipts, most recent first; writtenOnly restricts it to writer receipts.
func (l *Ledger) TouchedPaths(limit int, writtenOnly bool) []string {
	if l == nil || limit <= 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for i := len(l.receipts) - 1; i >= 0 && len(out) < limit; i-- {
		r := l.receipts[i]
		if !r.Success || (writtenOnly && !r.Write) || (!writtenOnly && !r.Read && !r.Write) {
			continue
		}
		for _, p := range r.Paths {
			if !seen[p] && len(out) < limit {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// HasSuccessfulBashMentioningPaths reports whether every path appears in some
// successful bash command this turn — files created or edited through shell
// redirection (`seq … > file`) leave no reader/writer receipt, so the command
// text naming the path is the receipt.
func (l *Ledger) HasSuccessfulBashMentioningPaths(paths []string) bool {
	wanted := normalizePaths(paths)
	if l == nil || len(wanted) == 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, p := range wanted {
		needle := strings.ToLower(filepath.ToSlash(p))
		found := false
		for _, r := range l.receipts {
			if !r.Success || r.ToolName != "bash" {
				continue
			}
			command := strings.ToLower(strings.ReplaceAll(r.Command, `\`, `/`))
			if strings.Contains(command, needle) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (l *Ledger) HasSuccessfulCommandAfter(command string, after int) bool {
	command = strings.TrimSpace(command)
	if l == nil || command == "" {
		return false
	}
	start := max(after+1, 0)

	l.mu.Lock()
	defer l.mu.Unlock()
	for i := start; i < len(l.receipts); i++ {
		r := l.receipts[i]
		if r.Success && r.ToolName == "bash" && CommandMatches(command, r.Command) {
			return true
		}
	}
	return false
}

func (l *Ledger) HasSuccessfulCompleteStepAfter(after int) bool {
	if l == nil {
		return false
	}
	start := max(after+1, 0)

	l.mu.Lock()
	defer l.mu.Unlock()
	for i := start; i < len(l.receipts); i++ {
		r := l.receipts[i]
		if r.Success && r.ToolName == "complete_step" {
			return true
		}
	}
	return false
}

// HasSuccessfulDeliverySignoffAfter reports whether a successful complete_step
// after the latest mutation cites a verification command that also succeeded
// after that mutation. complete_step already validates the cited command against
// host receipts; the additional ordering check prevents a pre-change test from
// signing off changed code in the delivery profile.
func (l *Ledger) HasSuccessfulDeliverySignoffAfter(after int) bool {
	if l == nil {
		return false
	}
	start := max(after+1, 0)

	l.mu.Lock()
	receipts := append([]Receipt(nil), l.receipts...)
	l.mu.Unlock()
	for i := start; i < len(receipts); i++ {
		r := receipts[i]
		if !r.Success || r.ToolName != "complete_step" {
			continue
		}
		if after >= 0 && !receiptsReviewChanges(receipts, start, i, after) {
			continue
		}
		for _, command := range completeStepVerificationCommands(r.Args) {
			if !bashCommandIsVerification(command) {
				continue
			}
			for j := start; j < i; j++ {
				candidate := receipts[j]
				if candidate.Success && candidate.ToolName == "bash" && CommandMatches(command, candidate.Command) {
					return true
				}
			}
		}
	}
	return false
}

// HasSuccessfulReviewAfter reports whether the changed result was inspected
// after the latest mutation. A read of a touched path is sufficient; git/diff
// inspection commands cover shell-driven or delegated mutations whose paths are
// not knowable to the host. A negative index is the restored-checkpoint
// baseline: the mutation predates this ledger (controller rebuild or cold
// resume), so any successful review-shaped receipt counts.
func (l *Ledger) HasSuccessfulReviewAfter(after int) bool {
	if l == nil {
		return false
	}
	start := max(after+1, 0)

	l.mu.Lock()
	receipts := append([]Receipt(nil), l.receipts...)
	l.mu.Unlock()
	if after >= len(receipts) {
		return false
	}
	return receiptsReviewChanges(receipts, start, len(receipts), after)
}

// HasHostReviewCoverageAfter reports whether host-observed content inspection
// after the latest mutation covers the production paths required by a Medium
// Delivery review. A plain, output-producing `git diff` covers the current
// change set; otherwise every required path needs a read receipt or a
// content-printing command that names it. Summary/status/check-only commands
// and model prose never satisfy this stronger alternative to review_report.
func (l *Ledger) HasHostReviewCoverageAfter(after int, requiredPaths []string) bool {
	if l == nil {
		return false
	}
	start := max(after+1, 0)
	l.mu.Lock()
	receipts := append([]Receipt(nil), l.receipts...)
	l.mu.Unlock()
	if after >= len(receipts) {
		return false
	}
	for i := start; i < len(receipts); i++ {
		r := receipts[i]
		if r.Success && r.ToolName == "bash" && r.OutputBytes > 0 && commandShowsWholeGitDiff(r.Command) {
			return true
		}
	}
	wanted := normalizePaths(requiredPaths)
	if len(wanted) == 0 {
		return false
	}
	for _, path := range wanted {
		needle := strings.ToLower(filepath.ToSlash(path))
		covered := false
		for i := start; i < len(receipts); i++ {
			r := receipts[i]
			if !r.Success {
				continue
			}
			if r.Read {
				for _, observed := range r.Paths {
					candidate := strings.ToLower(filepath.ToSlash(normalizePath(observed)))
					if candidate == needle || strings.HasSuffix(candidate, "/"+needle) {
						covered = true
						break
					}
				}
			}
			if !covered && r.ToolName == "bash" && r.OutputBytes > 0 && commandShowsContentForPath(r.Command, needle) {
				covered = true
			}
			if covered {
				break
			}
		}
		if !covered {
			return false
		}
	}
	return true
}

func commandShowsWholeGitDiff(command string) bool {
	file, err := shellparse.ParseBash(command)
	if err != nil || shellparse.HasHereDoc(file) || len(file.Stmts) != 1 {
		return false
	}
	stmt := file.Stmts[0]
	if stmt == nil || stmt.Negated || stmt.Background || stmt.Coprocess || len(stmt.Redirs) > 0 {
		return false
	}
	call, ok := stmt.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Assigns) > 0 || len(call.Args) != 2 {
		return false
	}
	base, okBase := shellparse.StaticWord(call.Args[0])
	sub, okSub := shellparse.StaticWord(call.Args[1])
	return okBase && okSub && strings.EqualFold(filepath.Base(base), "git") && strings.EqualFold(sub, "diff")
}

func receiptsReviewChanges(receipts []Receipt, start, end, mutationIndex int) bool {
	if mutationIndex >= len(receipts) {
		return false
	}
	// A negative mutationIndex is the restored-checkpoint baseline: the
	// mutation's receipt is not in this ledger, so its touched paths are
	// unknowable and any successful review-shaped receipt counts.
	var wanted map[string]bool
	if mutationIndex >= 0 {
		wanted = pathSet(receipts[mutationIndex].Paths)
	}
	for i := start; i < end && i < len(receipts); i++ {
		r := receipts[i]
		if !r.Success {
			continue
		}
		if r.ToolName == "bash" && commandReviewsChanges(r.Command) {
			return true
		}
		if r.ToolName == "bash" && len(wanted) > 0 && !bashMayMutate(r.Command) && commandMentionsPaths(r.Command, wanted) {
			return true
		}
		if !r.Read {
			continue
		}
		if len(wanted) == 0 {
			return true
		}
		for _, p := range r.Paths {
			if wanted[p] {
				return true
			}
		}
	}
	return false
}
