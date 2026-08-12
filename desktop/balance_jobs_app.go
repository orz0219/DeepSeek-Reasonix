package main

import (
	"fmt"
	"strings"

	"reasonix/internal/billing"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/evidence"
)

// ContextUsage returns the latest context-window gauge numbers.
func (a *App) ContextUsage() ContextInfo {
	return a.ContextUsageForTab("")
}

func (a *App) ContextUsageForTab(tabID string) ContextInfo {
	a.mu.RLock()
	tab := a.tabByIDLocked(tabID)
	var ctrl control.SessionAPI
	if tab != nil {
		ctrl = tab.Ctrl
	}
	a.mu.RUnlock()

	var info ContextInfo
	var snap tabTelemetrySnapshot
	if tab != nil {

		if ctrl != nil {
			if sp := ctrl.SessionPath(); sp != "" {
				tab.syncTelemetryToSession(sp)
			}
		}
		snap = tab.displayTelemetrySnapshot()
		info.SessionTokens = snap.Usage.TotalTokens
		info.SessionCost = snap.Usage.SessionCost
		info.SessionCurrency = snap.Usage.SessionCurrency
		info.CacheHitTokens = snap.Usage.CacheHitTokens
		info.CacheMissTokens = snap.Usage.CacheMissTokens
		info.Estimated = snap.Usage.Estimated
		info.SessionCostComplete = snap.Usage.SessionCostComplete
		info.SessionCostQuote = snap.Usage.SessionCostQuote
		info.Sources = snap.Usage.Sources
	}
	if ctrl == nil {
		return info
	}

	used, window := ctrl.ContextSnapshot()
	info.Used = used
	info.Window = window
	info.CompactRatio = ctrl.CompactRatio()
	info.Maintenance = contextMaintenanceInfo(ctrl.ContextMaintenanceSnapshot())
	return info
}

// BalanceInfo is the wallet-balance readout for the status bar. Available is true
// only when a balance was fetched; Display is the exact formatted amount (e.g.
// "¥110.00")
// and is "" when the active provider declares no balance_url — the frontend then
// omits the readout. Err carries a fetch failure for an optional tooltip.
// Wallet balances are displayed in their original currencies; no conversion
// or cross-currency sum is performed.
type BalanceInfo struct {
	Available           bool     `json:"available"`
	Display             string   `json:"display"`
	Detail              string   `json:"detail,omitempty"` // per-wallet original balances
	Complete            bool     `json:"complete"`
	RateDate            string   `json:"rateDate,omitempty"`
	Approx              bool     `json:"approx,omitempty"`
	Currencies          []string `json:"currencies,omitempty"`
	PrimaryCurrency     string   `json:"primaryCurrency,omitempty"`
	CostDisplayCurrency string   `json:"costDisplayCurrency,omitempty"`
	MultiCurrency       bool     `json:"multiCurrency,omitempty"`
	Err                 string   `json:"err,omitempty"`
}

// Balance queries the active provider's wallet balance (a network call). It
// returns an empty (unavailable) readout when no provider balance_url is set, the
// controller is down, or the fetch fails — so the status bar simply shows nothing
// rather than an error.
func (a *App) Balance() BalanceInfo {
	return a.BalanceForTab("")
}

func (a *App) BalanceForTab(tabID string) BalanceInfo {
	currency := a.balanceDisplayCurrency()
	tab, ctrl, generation := a.balanceRequestTarget(tabID)
	if ctrl == nil {
		return BalanceInfo{}
	}
	b, err := ctrl.Balance(a.ctx)
	if err != nil {
		return BalanceInfo{Err: err.Error()}
	}
	if b == nil {
		return BalanceInfo{}
	}
	display := b.DisplayForCurrency(currency)
	currencies := b.Currencies()
	primary := b.PrimaryCurrency()
	a.applyBalanceDisplayHint(tabID, tab, ctrl, currency, primary, generation)
	detail := balanceDetail(b)
	return BalanceInfo{
		Available:           true,
		Display:             display,
		Detail:              detail,
		Complete:            true,
		Currencies:          currencies,
		PrimaryCurrency:     primary,
		CostDisplayCurrency: firstNonEmptyString(currency, primary),
		MultiCurrency:       len(currencies) > 1,
	}
}

func balanceDetail(b *billing.Balance) string {
	if b == nil || len(b.Infos) == 0 {
		return ""
	}
	parts := make([]string, 0, len(b.Infos))
	for _, info := range b.Infos {
		cur := strings.ToUpper(strings.TrimSpace(info.Currency))
		if cur == "" {
			cur = "UNKNOWN"
		}
		parts = append(parts, cur+" "+strings.TrimSpace(info.TotalBalance))
	}
	return strings.Join(parts, "\n")
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// balanceDisplayCurrency resolves only an explicit global display currency.
// Automatic mode leaves the wallet in its original currency.
func (a *App) balanceDisplayCurrency() string {
	cfg, _, err := a.loadDesktopUserConfigForView()
	if err != nil {
		return ""
	}
	if pref := cfg.DisplayCurrencyPref(); pref != "" {
		return pref
	}
	return cfg.ExplicitDisplayCurrency()
}

// JobView is one running background job (bash/task started with
// run_in_background) for the status-bar indicator.
type JobView struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"`
}

// Jobs returns the still-running background jobs for the status bar. It refreshes
// on demand (mount, turn end, and on each notice the frontend receives).
func (a *App) Jobs() []JobView {
	return a.JobsForTab("")
}

func (a *App) JobsForTab(tabID string) []JobView {
	out := []JobView{}
	ctrl := a.ctrlForRuntimeTabID(tabID)
	return a.jobsForCtrl(ctrl, out)
}

// CancelJob stops one running background job in the active tab.
func (a *App) CancelJob(jobID string) (bool, error) {
	return a.CancelJobForTab("", jobID)
}

// CancelJobForTab stops one running background job without relying on whatever
// tab happens to be active when the asynchronous frontend call completes.
func (a *App) CancelJobForTab(tabID, jobID string) (bool, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return false, fmt.Errorf("job id is required")
	}
	if tabID != "" {
		ctrl := a.ctrlForRuntimeTabID(tabID)
		if ctrl != nil {
			return cancelJobForController(ctrl, jobID)
		}
		return false, nil
	}
	return cancelJobForController(a.ctrlForRuntimeTabID(tabID), jobID)
}

func cancelJobForController(ctrl control.SessionAPI, jobID string) (bool, error) {
	if ctrl == nil {
		return false, nil
	}
	canceller, ok := ctrl.(interface{ CancelJob(string) bool })
	if !ok {
		return false, fmt.Errorf("background job cancellation is unavailable")
	}
	return canceller.CancelJob(jobID), nil
}

func (a *App) jobsForCtrl(ctrl control.SessionAPI, out []JobView) []JobView {
	if ctrl == nil {
		return out
	}
	for _, v := range ctrl.Jobs() {
		out = append(out, JobView{ID: v.ID, Kind: v.Kind, Label: v.Label, Status: v.Status, StartedAt: v.StartedAt})
	}
	return out
}

// Meta describes the session for the frontend's header and status line.
type Meta struct {
	Label             string             `json:"label"`
	Ready             bool               `json:"ready"`
	Runtime           SessionRuntimeView `json:"runtime"`
	StartupErr        string             `json:"startupErr,omitempty"`
	EventChannel      string             `json:"eventChannel"`
	SessionPath       string             `json:"sessionPath,omitempty"`
	SessionRevision   int64              `json:"sessionRevision,omitempty"`
	SessionDigest     string             `json:"sessionDigest,omitempty"`
	Cwd               string             `json:"cwd"`
	WorkspaceRoot     string             `json:"workspaceRoot,omitempty"`
	WorkspaceName     string             `json:"workspaceName,omitempty"`
	WorkspacePath     string             `json:"workspacePath,omitempty"`
	GitBranch         string             `json:"gitBranch,omitempty"`
	ImageInputEnabled bool               `json:"imageInputEnabled"`
	AutoApproveTools  bool               `json:"autoApproveTools"`
	Bypass            bool               `json:"bypass"` // legacy JSON key for YOLO/full-access tool auto-approval
	CollaborationMode string             `json:"collaborationMode"`
	ToolApprovalMode  string             `json:"toolApprovalMode"`
	TokenMode         string             `json:"tokenMode"`
	// AgentPreset is the canonical role setting (light|balanced|delivery).
	// TokenMode remains the dual-write legacy wire value for one version.
	AgentPreset string           `json:"agentPreset,omitempty"`
	Goal        string           `json:"goal,omitempty"`
	GoalStatus  string           `json:"goalStatus,omitempty"`
	GoalRuntime *GoalRuntimeView `json:"goalRuntime,omitempty"`
	// Nil means no authoritative snapshot; non-nil empty means clear the panel.
	CanonicalTodos *[]evidence.TodoItem `json:"canonicalTodos,omitempty"`
}

type GoalRuntimeView struct {
	TurnsUsed        int    `json:"turnsUsed"`
	TurnsLimit       int    `json:"turnsLimit"` // Deprecated: always 0.
	TokensUsed       int    `json:"tokensUsed"`
	RequestsUsed     int    `json:"requestsUsed,omitempty"`
	WorkDurationMs   int64  `json:"workDurationMs,omitempty"`
	TokensLimit      int    `json:"tokensLimit"` // Deprecated: always 0; retained for bridge compatibility.
	NoProgressTurns  int    `json:"noProgressTurns"`
	NoProgressLimit  int    `json:"noProgressLimit"` // Deprecated: always 0.
	LastReason       string `json:"lastReason,omitempty"`
	StopCause        string `json:"stopCause,omitempty"`
	BudgetExtensions int    `json:"budgetExtensions"` // Deprecated: always 0.
}

func goalRuntimeViewFromController(ctrl control.SessionAPI) *GoalRuntimeView {
	if ctrl == nil {
		return nil
	}
	rt := ctrl.GoalRuntime()
	return &GoalRuntimeView{
		TurnsUsed:        rt.TurnsUsed,
		TurnsLimit:       rt.TurnsLimit,
		TokensUsed:       rt.TokensUsed,
		RequestsUsed:     rt.RequestsUsed,
		WorkDurationMs:   rt.WorkDurationMs,
		TokensLimit:      rt.TokensLimit,
		NoProgressTurns:  rt.NoProgressTurns,
		NoProgressLimit:  rt.NoProgressLimit,
		LastReason:       rt.LastReason,
		StopCause:        rt.StopCause,
		BudgetExtensions: rt.BudgetExtensions,
	}
}

// Meta reports the model label, readiness, any startup error, the working
// directory (for the status line), and the runtime event channel the frontend
// subscribes to.
func (a *App) Meta() Meta {
	return a.MetaForTab("")
}

// imageInputEnabledForRootModel reports whether the resolved model supports
// image input. EXPENSIVE: it loads the workspace config and resolves the model
// catalog. Never call it on a bound-method request path — MetaForTab serves
// the cached tabMetaExtras instead, and only refreshTabMetaExtras (background)
// calls this.
func (a *App) imageInputEnabledForRootModel(root, ref string) bool {
	if hook := a.configLoadForRootHook; hook != nil {
		hook(root)
	}
	cfg, err := config.LoadForRoot(root)
	if err == nil && ref == "" {
		ref = cfg.DefaultModel
	}
	if err != nil || ref == "" {
		return false
	}
	entry, ok := cfg.ResolveModel(ref)
	return ok && config.EffectiveVision(entry)
}
