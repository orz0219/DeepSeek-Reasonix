package boot

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"reasonix/internal/ablation"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/permission"
	"reasonix/internal/tool"
)

// applyUnifiedProviderToolSurface restricts Schemas/ContractEntries to the
// shared core + host-control tools. use_capability can still Get every
// registered tool, including those hidden from the provider schema.
func applyUnifiedProviderToolSurface(reg *tool.Registry) {
	if reg == nil {
		return
	}
	allow := make([]string, 0, 16)
	for _, name := range UnifiedProviderToolNames() {
		if _, ok := reg.Get(name); ok {
			allow = append(allow, name)
		}
	}

	if len(allow) == 0 {
		if _, ok := reg.Get("use_capability"); ok {
			allow = []string{"use_capability"}
		}
	}
	reg.SetProviderVisibleTools(allow)
}

// effectivePlannerModel centralizes planner precedence. Every role setting
// builds the configured planner so later in-place switches retain the same
// runtime; the per-turn TaskPolicy decides whether it is invoked.
func effectivePlannerModel(cfg *config.Config, opts Options) string {
	if cfg == nil || opts.Ablation.Off(ablation.Planner) {
		return ""
	}
	return strings.TrimSpace(cfg.Agent.PlannerModel)
}

func rememberPermissionRule(workspaceRoot, rule string) control.RememberResult {
	path := rememberPermissionConfigPath(workspaceRoot)
	result := control.RememberResult{Rule: strings.TrimSpace(rule), Path: path}
	unlock, err := config.LockConfigFileEdits(path)
	if err != nil {
		slog.Warn("lock config for permission rule", "path", path, "err", err)
		result.Err = err
		return result
	}
	defer unlock()

	edit, err := config.LoadForEditReadOnlyStrict(path)
	if err != nil {
		slog.Warn("load config for permission rule", "path", path, "err", err)
		result.Err = err
		return result
	}
	if coveredBy := coveredPermissionRule(edit.Permissions.Allow, result.Rule); coveredBy != "" {
		result.CoveredBy = coveredBy
		return result
	}
	edit.Permissions.Allow = pruneCoveredPermissionRules(edit.Permissions.Allow, result.Rule)
	if err := edit.AddPermissionRule("allow", rule); err != nil {
		slog.Warn("persist permission rule", "rule", rule, "err", err)
		result.Err = err
		return result
	}
	if err := config.WritePermissionsAllow(path, edit.Permissions.Allow); err != nil {
		slog.Warn("save config after permission rule", "err", err)
		result.Err = err
		return result
	}
	result.Saved = true
	return result
}

func rememberPermissionConfigPath(workspaceRoot string) string {
	workspaceRoot = strings.TrimSpace(workspaceRoot)
	if workspaceRoot != "" {
		return filepath.Join(workspaceRoot, "reasonix.toml")
	}
	path := config.SourcePath()
	if path == "" {
		path = "reasonix.toml"
	}
	return path
}

func rememberPlanModeReadOnlyCommand(workspaceRoot, prefix string) control.PlanModeReadOnlyCommandTrustResult {
	prefix = strings.TrimSpace(prefix)
	path := rememberPermissionConfigPath(workspaceRoot)
	result := control.PlanModeReadOnlyCommandTrustResult{Prefix: prefix, Path: path}
	if prefix == "" {
		result.Err = fmt.Errorf("empty plan-mode read-only command prefix")
		return result
	}
	unlock, err := config.LockConfigFileEdits(path)
	if err != nil {
		result.Err = err
		return result
	}
	defer unlock()
	edit, err := config.LoadForEditReadOnlyStrict(path)
	if err != nil {
		result.Err = err
		return result
	}
	if coveredBy := coveredPlanModeReadOnlyCommand(edit.Agent.PlanModeReadOnlyCommands, prefix); coveredBy != "" {
		result.CoveredBy = coveredBy
		return result
	}
	edit.Agent.PlanModeReadOnlyCommands = append(edit.Agent.PlanModeReadOnlyCommands, prefix)
	if err := edit.SaveTo(path); err != nil {
		slog.Warn("persist plan-mode read-only command trust", "prefix", prefix, "err", err)
		result.Err = err
		return result
	}
	result.Saved = true
	return result
}

func coveredPlanModeReadOnlyCommand(existing []string, candidate string) string {
	candidateFields := strings.Fields(strings.TrimSpace(candidate))
	if len(candidateFields) == 0 {
		return ""
	}
	for _, item := range existing {
		itemFields := strings.Fields(strings.TrimSpace(item))
		if len(itemFields) == 0 || len(itemFields) > len(candidateFields) {
			continue
		}
		matches := true
		for i, field := range itemFields {
			if candidateFields[i] != field {
				matches = false
				break
			}
		}
		if matches {
			return strings.Join(itemFields, " ")
		}
	}
	return ""
}

func coveredPermissionRule(rules []string, rule string) string {
	for _, existing := range rules {
		if permission.RuleCoversString(existing, rule) {
			return strings.TrimSpace(existing)
		}
	}
	return ""
}

func pruneCoveredPermissionRules(rules []string, rule string) []string {
	out := rules[:0]
	for _, existing := range rules {
		if strings.TrimSpace(existing) == "" || permission.RuleCoversString(rule, existing) {
			continue
		}
		out = append(out, existing)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
