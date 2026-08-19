package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/checkpoint"
	"reasonix/internal/event"
	"reasonix/internal/i18n"
	"reasonix/internal/permission"
	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
)

// gateApprover adapts the Controller to permission.Approver. It is distinct
// from the public Approve command (different signature, different direction).
type gateApprover struct{ c *Controller }

const dynamicBashApprovalReason = "This command uses nested or indirect shell execution. Auto and broad allow rules cannot verify the inner command; approve this exact command or use YOLO."

func (g gateApprover) ApproveWithReason(ctx context.Context, tool, subject string, args json.RawMessage) (bool, bool, string, error) {
	return g.approveWithPolicyReason(ctx, tool, subject, args, "")
}

func (g gateApprover) ApproveWithPolicyReason(ctx context.Context, tool, subject string, args json.RawMessage, policyReason string) (bool, bool, string, error) {
	return g.approveWithPolicyReason(ctx, tool, subject, args, policyReason)
}

func combineApprovalReasons(reasons ...string) string {
	var kept []string
	for _, reason := range reasons {
		if reason = strings.TrimSpace(reason); reason != "" {
			kept = append(kept, reason)
		}
	}
	return strings.Join(kept, "\n")
}

func (g gateApprover) approveWithPolicyReason(ctx context.Context, tool, subject string, args json.RawMessage, policyReason string) (bool, bool, string, error) {
	subject = approvalDisplaySubject(tool, subject, args)
	requireHuman := strings.EqualFold(tool, "bash") && permission.BashSubjectRequiresExplicitApproval(subject)

	if requireHuman && g.c.approval.preApprovedForRequiredHuman(tool, subject) {
		return true, false, "", nil
	}
	if !requireHuman && g.c.approval.preApproved(tool, subject, args) {
		return true, false, "", nil
	}
	if g.c.guardianSess != nil && !requireHuman {
		allow, reason, reviewErr := g.c.guardianSess.Review(ctx, tool, args, g.c.executor.Session())
		if reviewErr != nil {
			return false, false, "", reviewErr
		}
		if allow && !requiresFreshApprovalTool(tool) {
			return true, false, "", nil
		}
		reason = combineApprovalReasons(policyReason, reason)
		humanAllow, remember, err := g.c.requestApprovalWithReason(ctx, tool, subject, args, reason)
		if err != nil {
			return false, false, reason, err
		}
		if !humanAllow {
			return false, false, reason, nil
		}
		return true, remember, "", nil
	}
	if requireHuman {
		reason := combineApprovalReasons(policyReason, dynamicBashApprovalReason)
		allow, remember, err := g.c.requestApprovalWithReasonOptions(ctx, tool, subject, args, reason, approvalDecisionOptions{requireHuman: true})
		return allow, remember, "", err
	}
	allow, remember, err := g.c.requestApprovalWithReason(ctx, tool, subject, args, policyReason)
	return allow, remember, "", err
}

type planModeReadOnlyTrustApprover struct{ c *Controller }

type sandboxEscapeApprover struct{ c *Controller }

func (s sandboxEscapeApprover) ApproveSandboxEscape(ctx context.Context, req sandbox.EscapeRequest) (bool, string, error) {
	subject := sandboxEscapeApprovalSubject(req.Command)
	reason := sandboxEscapeApprovalReason(req.Reason)
	reply, err := s.c.requestFreshApprovalDecision(ctx, SandboxEscapeApprovalTool, subject, req.Args, reason)
	if err != nil {
		return false, "approval aborted", err
	}
	if !reply.allow {
		return false, i18n.M.SandboxEscapeDeclined, nil
	}
	if reply.session {
		s.c.approval.grantSession(SandboxEscapeApprovalTool, subject)
	}
	return true, "", nil
}

func (s sandboxEscapeApprover) SandboxEscapeSessionAllowed(_ context.Context, req sandbox.EscapeRequest) bool {
	return s.c.approval.preApprovedForDecision(SandboxEscapeApprovalTool, sandboxEscapeApprovalSubject(req.Command), nil, true)
}

func sandboxEscapeApprovalSubject(command string) string {
	subject := strings.TrimSpace(command)
	if subject == "" {
		return i18n.M.SandboxEscapeSubjectFallback
	}
	return i18n.M.SandboxEscapeSubjectPrefix + subject
}

func sandboxEscapeApprovalReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return i18n.M.SandboxEscapeRuntimeReason
	}
	return reason
}

// managedConfigWriteApprover routes a file tool's Reasonix-managed config write
// through the fresh-human approval prompt (see ManagedConfigWriteApprovalTool).
// A session grant is tool-wide (mirroring sandbox_escape): one "allow for this
// session" covers the rest of the repair flow across the handful of managed
// config files without re-prompting on every incremental edit.
type managedConfigWriteApprover struct{ c *Controller }

func (m managedConfigWriteApprover) ApproveManagedConfigWrite(ctx context.Context, req tool.ConfigWriteRequest) (bool, string, error) {
	subject := managedConfigWriteApprovalSubject(req.Path)
	args, _ := json.Marshal(map[string]string{"path": req.Path})
	reply, err := m.c.requestFreshApprovalDecision(ctx, ManagedConfigWriteApprovalTool, subject, args, i18n.M.ConfigWriteReason)
	if err != nil {
		return false, "approval aborted", err
	}
	if !reply.allow {
		return false, i18n.M.ConfigWriteDeclined, nil
	}
	if reply.session {
		m.c.approval.grantSession(ManagedConfigWriteApprovalTool, subject)
	}
	return true, "", nil
}

func (m managedConfigWriteApprover) ManagedConfigWriteSessionAllowed(_ context.Context, req tool.ConfigWriteRequest) bool {
	return m.c.approval.preApprovedForDecision(ManagedConfigWriteApprovalTool, managedConfigWriteApprovalSubject(req.Path), nil, true)
}

func managedConfigWriteApprovalSubject(path string) string {
	return i18n.M.ConfigWriteSubjectPrefix + strings.TrimSpace(path)
}

func (p planModeReadOnlyTrustApprover) CheckPlanModeReadOnlyTrust(ctx context.Context, req agent.PlanModeReadOnlyTrustRequest) (bool, string, error) {
	prefix := normalizePlanModeReadOnlyCommandPrefix(req.Prefix)
	if prefix == "" {
		return false, "missing plan-mode read-only command prefix", nil
	}
	return p.checkBashReadOnlyCommandTrust(ctx, req, prefix)
}

func (p planModeReadOnlyTrustApprover) checkBashReadOnlyCommandTrust(ctx context.Context, req agent.PlanModeReadOnlyTrustRequest, prefix string) (bool, string, error) {
	if p.c.approval.planModeReadOnlyCommandTrusted(prefix) {
		return true, "", nil
	}
	command := strings.TrimSpace(req.Command)
	if command == "" {
		command = strings.TrimSpace(string(req.Args))
	}
	subject := fmt.Sprintf(i18n.M.PlanModeBashTrustSubjectFmt, prefix, command)
	reason := i18n.M.PlanModeBashTrustReason
	reply, err := p.c.requestFreshApprovalDecision(ctx, agent.PlanModeReadOnlyCommandApprovalTool, subject, req.Args, reason)
	if err != nil {
		return false, "approval aborted", err
	}
	if !reply.allow {
		return false, i18n.M.PlanModeBashTrustDeclined, nil
	}
	if reply.session {
		p.c.approval.grantPlanModeReadOnlyCommand(prefix)
	}
	if reply.persist && p.c.onRememberPlanModeReadOnlyCommand != nil {
		p.c.emitPlanModeReadOnlyCommandTrustResult(p.c.onRememberPlanModeReadOnlyCommand(prefix))
		p.c.approval.grantPlanModeReadOnlyCommand(prefix)
	}
	return true, "", nil
}

func approvalDisplaySubject(tool, subject string, args json.RawMessage) string {
	switch tool {
	case "move_file":
		return moveApprovalSubject(subject, args)
	default:
		return subject
	}
}

func moveApprovalSubject(fallback string, args json.RawMessage) string {
	if len(args) == 0 {
		return fallback
	}
	var in struct {
		SourcePath      string `json:"source_path"`
		DestinationPath string `json:"destination_path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return fallback
	}
	if in.SourcePath == "" || in.DestinationPath == "" {
		return fallback
	}
	return in.SourcePath + " -> " + in.DestinationPath
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func approvalCompactText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func approvalTruncate(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "..."
}

func (c *Controller) sessionMessageCount() int {
	if c.executor == nil {
		return 0
	}
	return c.executor.Session().Len()
}

// parseRewind parses the arguments after "/rewind". The user may provide:
//
//	/rewind              → latest checkpoint, both
//	/rewind <turn>       → that turn, both
//	/rewind <turn> <scope> → that turn, code|conversation|both
//
// If no turn is given, the latest checkpoint is used. If no scope is given, Both is assumed.
func parseRewind(args string, cps []checkpoint.Meta) (int, RewindScope, error) {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		if len(cps) == 0 {
			return 0, RewindBoth, fmt.Errorf("no checkpoints available")
		}
		return cps[len(cps)-1].Turn, RewindBoth, nil
	}
	turn, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, RewindBoth, fmt.Errorf("invalid turn: %w", err)
	}
	scope := RewindBoth
	if len(fields) >= 2 {
		switch strings.ToLower(fields[1]) {
		case "code":
			scope = RewindCode
		case "conversation":
			scope = RewindConversation
		case "both":
			scope = RewindBoth
		default:
			return 0, RewindBoth, fmt.Errorf("unknown scope %q", fields[1])
		}
	}
	return turn, scope, nil
}

// requestApproval emits an ApprovalRequest and blocks until Approve(ID, …)
// answers or ctx is cancelled. A prior session grant (or a bypass posture) for
// the same approval scope short-circuits. The approvalManager's promptMu
// serialises outstanding prompts; this method keeps the I/O (events, hooks,
// remember) that the manager deliberately stays out of.
func (c *Controller) requestApproval(ctx context.Context, tool, subject string, args json.RawMessage) (bool, bool, error) {
	return c.requestApprovalWithReason(ctx, tool, subject, args, "")
}

func (c *Controller) requestApprovalWithReason(ctx context.Context, tool, subject string, args json.RawMessage, reason string) (bool, bool, error) {
	return c.requestApprovalWithReasonOptions(ctx, tool, subject, args, reason, approvalDecisionOptions{})
}

func (c *Controller) requestApprovalWithReasonOptions(ctx context.Context, tool, subject string, args json.RawMessage, reason string, opts approvalDecisionOptions) (bool, bool, error) {
	r, err := c.requestApprovalDecisionWithOptions(ctx, tool, subject, args, reason, opts)
	if err != nil {
		return false, false, err
	}

	if r.allow && r.session && !requiresFreshApprovalTool(tool) {
		c.approval.grantSession(tool, subject)
	}
	if r.allow && r.persist && !requiresFreshApprovalTool(tool) && c.onRemember != nil {
		c.emitRememberResult(c.onRemember(permission.RememberRuleForScope(tool, subject)))
	}
	return r.allow, false, nil
}

func (c *Controller) requestFreshApprovalDecision(ctx context.Context, tool, subject string, args json.RawMessage, reason string) (approvalReply, error) {
	return c.requestApprovalDecisionWithOptions(ctx, tool, subject, args, reason, approvalDecisionOptions{fresh: true})
}

type approvalDecisionOptions struct {
	// fresh marks a user trust/business decision rather than an ordinary tool
	// permission. It may reuse an explicit session grant, but YOLO/auto approval
	// must not answer or drain the prompt.
	fresh bool
	// requireHuman marks an ordinary tool approval that Auto, an approved-plan
	// window, Guardian, or an allowing hook must not answer. Unlike fresh it
	// retains the ordinary four-choice UI and YOLO remains an explicit bypass.
	requireHuman bool
}

func (c *Controller) requestApprovalDecisionWithOptions(ctx context.Context, tool, subject string, args json.RawMessage, reason string, opts approvalDecisionOptions) (approvalReply, error) {

	if c.approval.preApprovedForDecisionOptions(tool, subject, args, opts.fresh, opts.requireHuman) {
		return approvalReply{allow: true}, nil
	}

	c.approval.promptMu.Lock()
	defer c.approval.promptMu.Unlock()

	if c.approval.preApprovedForDecisionOptions(tool, subject, args, opts.fresh, opts.requireHuman) {
		return approvalReply{allow: true}, nil
	}

	if hookSubject, hookArgs, ok := permissionRequestHookPayload(tool, subject, args); ok {
		if decision, _ := c.hooks.PermissionRequest(ctx, tool, hookSubject, hookArgs); decision != nil {
			switch {
			case !*decision:
				return approvalReply{}, nil
			case !opts.fresh && !opts.requireHuman && !requiresFreshApprovalTool(tool):
				return approvalReply{allow: true}, nil
			}

		}
	}

	c.approval.promptEmitMu.Lock()
	var id string
	var reply chan approvalReply
	if opts.fresh || opts.requireHuman || tool == planApprovalTool {
		kind := ""
		if tool == planApprovalTool {
			kind = "plan"
		}
		id, reply = c.approval.registerDecisionKindWithInput(tool, subject, reason, args, opts.fresh, opts.requireHuman, kind, nil)
	} else {
		id, reply = c.approval.registerWithInput(tool, subject, reason, args)
	}

	c.sink.Emit(c.approvalRequestEvent(event.Approval{ID: id, Tool: tool, Subject: subject, Reason: reason, RawInput: append(json.RawMessage(nil), args...), Fresh: opts.fresh}))
	c.approval.promptEmitMu.Unlock()

	go c.hooks.Notification(ctx, approvalNotificationText(tool, subject), "permission_prompt")

	waitCtx, cancelWait := c.approval.waitContext(ctx)
	defer cancelWait()

	select {
	case r := <-reply:
		return r, nil
	case <-waitCtx.Done():
		c.approval.cancel(id)
		return approvalReply{}, waitCtx.Err()
	}
}

func (c *Controller) approvalRequestEvent(approval event.Approval) event.Event {
	return event.Event{Kind: event.ApprovalRequest, Approval: approval}
}

func (c *Controller) emitRememberResult(r RememberResult) {
	if r.Err != nil {
		c.sink.Emit(event.Event{
			Kind:  event.Notice,
			Level: event.LevelWarn,
			Text:  fmt.Sprintf(i18n.M.PermissionSaveFailedFmt, r.Rule, r.Err),
		})
		return
	}
	switch {
	case r.Saved:
		c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: fmt.Sprintf(i18n.M.PermissionSavedFmt, r.Path, r.Rule)})
	case strings.TrimSpace(r.CoveredBy) != "":
		c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: fmt.Sprintf(i18n.M.PermissionAlreadyAllowedFmt, r.Path, r.CoveredBy)})
	}
}

func (c *Controller) emitPlanModeReadOnlyCommandTrustResult(r PlanModeReadOnlyCommandTrustResult) {
	prefix := strings.TrimSpace(r.Prefix)
	if r.Err != nil {
		c.sink.Emit(event.Event{
			Kind:  event.Notice,
			Level: event.LevelWarn,
			Text:  fmt.Sprintf(i18n.M.PlanModeReadOnlyCommandTrustFailedFmt, prefix, r.Err),
		})
		return
	}
	switch {
	case r.Saved:
		c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: fmt.Sprintf(i18n.M.PlanModeReadOnlyCommandTrustSavedFmt, r.Path, prefix)})
	case strings.TrimSpace(r.CoveredBy) != "":
		c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Text: fmt.Sprintf(i18n.M.PlanModeReadOnlyCommandTrustAlreadyFmt, r.Path, r.CoveredBy)})
	}
}
