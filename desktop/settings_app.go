package main

// settings_app.go is the desktop Settings panel's command surface: it reads the
// resolved config and applies edits through internal/config/edit.go (the
// purpose-built mutation API), then rebuilds the controller so the change takes
// effect live — the same snapshot→reload→resume pattern as SetModel. Secrets are
// the exception: they go to Reasonix's global .env (upsertDotEnv), since config
// stores only the env-var name, not the key.

// read

type ProviderView struct {
	Name                        string                      `json:"name"`
	BuiltIn                     bool                        `json:"builtIn"`
	Added                       bool                        `json:"added"`
	Kind                        string                      `json:"kind"`
	BaseURL                     string                      `json:"baseUrl"`
	ChatURL                     string                      `json:"chatUrl"`
	RequestURL                  string                      `json:"requestUrl"`
	Models                      []string                    `json:"models"`
	VisionModels                []string                    `json:"visionModels"`
	VisionModelsSet             bool                        `json:"visionModelsConfigured"`
	VisionCapability            string                      `json:"visionCapability,omitempty"`
	ModelsURL                   string                      `json:"modelsUrl"`
	Default                     string                      `json:"default"`
	APIKeyEnv                   string                      `json:"apiKeyEnv"`
	Headers                     map[string]string           `json:"headers"`
	ExtraBody                   map[string]any              `json:"extraBody"`
	AuthHeader                  bool                        `json:"authHeader"`
	KeySet                      bool                        `json:"keySet"` // the env var currently resolves to a non-empty value
	RequiresKey                 bool                        `json:"requiresKey"`
	Configured                  bool                        `json:"configured"` // selectable: either key is present or no key is required
	KeySource                   string                      `json:"keySource,omitempty"`
	KeySourcePath               string                      `json:"keySourcePath,omitempty"`
	BalanceURL                  string                      `json:"balanceUrl"`
	ContextWindow               int                         `json:"contextWindow"`
	ReasoningProtocol           string                      `json:"reasoningProtocol"`
	Thinking                    string                      `json:"thinking"`
	WebSearch                   bool                        `json:"webSearch"`
	ServerWebSearchCapability   bool                        `json:"serverWebSearchCapability"`
	SupportedEfforts            []string                    `json:"supportedEfforts"`
	DefaultEffort               string                      `json:"defaultEffort"`
	ModelOverrides              []ProviderModelOverrideView `json:"modelOverrides"`
	RecommendedUpgradeAvailable bool                        `json:"recommendedUpgradeAvailable,omitempty"`
	// ModelCatalogFingerprint is an opaque digest of the provider identity and
	// current model selection. Background discovery must compare it while holding
	// the config edit lock before applying a narrow catalog-only update.
	ModelCatalogFingerprint string `json:"modelCatalogFingerprint"`
}

type ProviderModelCatalogUpdate struct {
	Name                string   `json:"name"`
	ExpectedFingerprint string   `json:"expectedFingerprint"`
	Models              []string `json:"models"`
	Default             string   `json:"default"`
	VisionModels        []string `json:"visionModels"`
}

type ProviderPresetView struct {
	ID                  string   `json:"id"`
	Label               string   `json:"label"`
	Description         string   `json:"description"`
	KeyEnv              string   `json:"keyEnv"`
	ProviderNames       []string `json:"providerNames"`
	Models              []string `json:"models"`
	Added               bool     `json:"added"`
	Status              string   `json:"status"`
	StatusProviderNames []string `json:"statusProviderNames"`
	KeySet              bool     `json:"keySet"`
	RequiresKey         bool     `json:"requiresKey"`
	Configured          bool     `json:"configured"`
	KeySource           string   `json:"keySource,omitempty"`
	KeySourcePath       string   `json:"keySourcePath,omitempty"`
}

const (
	providerPresetStatusAvailable         = "available"
	providerPresetStatusInstalled         = "installed"
	providerPresetStatusInstalledModified = "installed_modified"
	providerPresetStatusNameConflict      = "name_conflict"
	providerPresetStatusSimilarExisting   = "similar_existing"
)

type ProviderModelOverrideView struct {
	Model             string   `json:"model"`
	ReasoningProtocol string   `json:"reasoningProtocol"`
	Thinking          string   `json:"thinking"`
	SupportedEfforts  []string `json:"supportedEfforts"`
	DefaultEffort     string   `json:"defaultEffort"`
	Vision            *bool    `json:"vision"`
	ContextWindow     int      `json:"contextWindow,omitempty"`
	MaxOutputTokens   int      `json:"maxOutputTokens,omitempty"`
}

type PermissionsView struct {
	Mode  string   `json:"mode"`
	Allow []string `json:"allow"`
	Ask   []string `json:"ask"`
	Deny  []string `json:"deny"`
}

type SandboxView struct {
	Bash                   string   `json:"bash"`
	Network                bool     `json:"network"`
	WorkspaceRoot          string   `json:"workspaceRoot"`
	AllowWrite             []string `json:"allowWrite"`
	EffectiveWorkspaceRoot string   `json:"effectiveWorkspaceRoot"`
	EffectiveWriteRoots    []string `json:"effectiveWriteRoots"`
	Shell                  string   `json:"shell"` // [tools.shell] prefer: auto|bash|powershell|pwsh
	EffectiveShell         string   `json:"effectiveShell,omitempty"`
}

type NetworkProxyView struct {
	Type     string `json:"type"`
	Server   string `json:"server"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type NetworkView struct {
	ProxyMode string           `json:"proxyMode"`
	ProxyURL  string           `json:"proxyUrl"`
	NoProxy   string           `json:"noProxy"`
	Proxy     NetworkProxyView `json:"proxy"`
}

type AgentView struct {
	Temperature            float64 `json:"temperature"`
	MaxSteps               int     `json:"maxSteps"`
	PlannerMaxSteps        int     `json:"plannerMaxSteps"`
	MaxSubagentDepth       int     `json:"maxSubagentDepth"`
	MaxSubagentConcurrency int     `json:"maxSubagentConcurrency"`
	MaxParallelWriters     int     `json:"maxParallelWriters"`
	SystemPrompt           string  `json:"systemPrompt"`
	ReasoningLanguage      string  `json:"reasoningLanguage"`
	CompactRatio           float64 `json:"compactRatio,omitempty"`
	EffectiveCompactRatio  float64 `json:"effectiveCompactRatio,omitempty"`
	CompactRatioOverridden bool    `json:"compactRatioOverridden,omitempty"`
}

// SettingsView is the whole Settings panel payload.
type SettingsView struct {
	DefaultModel                 string               `json:"defaultModel"`
	PlannerModel                 string               `json:"plannerModel"`
	SubagentModel                string               `json:"subagentModel"`
	SubagentEffort               string               `json:"subagentEffort"`
	AutoPlan                     string               `json:"autoPlan"`
	Providers                    []ProviderView       `json:"providers"`
	OfficialProviders            []ProviderView       `json:"officialProviders"`
	ProviderPresets              []ProviderPresetView `json:"providerPresets"`
	Permissions                  PermissionsView      `json:"permissions"`
	Sandbox                      SandboxView          `json:"sandbox"`
	Network                      NetworkView          `json:"network"`
	Agent                        AgentView            `json:"agent"`
	DesktopLanguage              string               `json:"desktopLanguage"`
	DesktopCurrency              string               `json:"desktopCurrency"`
	DesktopLayoutStyle           string               `json:"desktopLayoutStyle"`
	DesktopTheme                 string               `json:"desktopTheme"`
	DesktopThemeStyle            string               `json:"desktopThemeStyle"`
	DesktopTerminalTheme         string               `json:"desktopTerminalTheme,omitempty"`
	CloseBehavior                string               `json:"closeBehavior"`
	DisplayMode                  string               `json:"displayMode"`
	ReasoningDisplayMode         string               `json:"reasoningDisplayMode"`
	ReasoningDisplayModeExplicit bool                 `json:"reasoningDisplayModeExplicit"`
	StatusBarStyle               string               `json:"statusBarStyle"`
	StatusBarItems               []string             `json:"statusBarItems"`
	DefaultToolApprovalMode      string               `json:"defaultToolApprovalMode"`

	ExpandThinking    bool   `json:"expandThinking"`
	ConversationWidth string `json:"conversationWidth,omitempty"`
	ConfigPath        string `json:"configPath"`
	// ShadowedByPath is the workspace reasonix.toml that outranks the file this
	// panel writes, so an edit here can be overridden with nothing on screen to
	// explain it (#4333). Empty when the panel's file is the one in effect.
	ShadowedByPath string `json:"shadowedByPath,omitempty"`
	// ProviderKinds lists the provider implementations the kernel actually
	// registered (provider.Kinds()), so the editor's "kind" picker offers only
	// kinds that resolve — selecting an unregistered one would fail the rebuild.
	ProviderKinds []string `json:"providerKinds"`
	// AutoApproveTools is the live YOLO/full-access state (runtime-only, not from
	// config), so the panel's toggle reflects whether tool approvals are currently
	// being skipped this session.
	AutoApproveTools bool `json:"autoApproveTools"`
	// Bypass is the legacy JSON key for the same live state.
	Bypass bool `json:"bypass"`
}

// DesktopStartupSettingsView is the lightweight Settings subset needed during
// frontend startup. It deliberately excludes providers and credential state so
// slow keychain/env resolution stays off the first-render path.
type DesktopStartupSettingsView struct {
	DesktopLanguage              string   `json:"desktopLanguage"`
	DesktopLayoutStyle           string   `json:"desktopLayoutStyle"`
	DesktopTheme                 string   `json:"desktopTheme"`
	DesktopThemeStyle            string   `json:"desktopThemeStyle"`
	DesktopTerminalTheme         string   `json:"desktopTerminalTheme,omitempty"`
	DisplayMode                  string   `json:"displayMode"`
	ReasoningDisplayMode         string   `json:"reasoningDisplayMode"`
	ReasoningDisplayModeExplicit bool     `json:"reasoningDisplayModeExplicit"`
	StatusBarStyle               string   `json:"statusBarStyle"`
	StatusBarItems               []string `json:"statusBarItems"`
	ConversationWidth            string   `json:"conversationWidth,omitempty"`
	// ConfigWarnings report in-memory recovery without rewriting user/project files.
	ConfigWarnings         []string `json:"configWarnings,omitempty"`
	ConfigWarningsRevision uint64   `json:"configWarningsRevision"`
	ConfigPath             string   `json:"configPath,omitempty"`
}

// shadowingConfigPath returns the config file that outranks writePath for the
// workspace at root, or "" when writePath is the one in effect. A project
// reasonix.toml beats the user config, so settings written here would otherwise
// look ignored (#4333).

// This token crosses the Wails boundary, so key the digest instead of exposing
// a reusable hash of header or credential-store metadata to the frontend.

// DesktopStartupSettings returns startup chrome preferences without provider/key state.

// Prefer the resilient workspace load so config warnings surface on first paint.

// OpenUserConfigPath reveals the user config file in the system file manager.

// Reveal the parent directory when the file does not exist yet so the user
// can still find where config.toml should live.

// ReloadUserConfig reloads configuration for the active workspace after the
// user fixes a broken file. Non-fatal load warnings remain visible when present.

// Settings returns the current configuration for the Settings panel.

// deprecated JSON compatibility for older frontends

// apply (write config, then rebuild the controller so it's live)

// applyConfigChange mutates the user-global config and rebuilds the controller so
// the change takes effect this session. Desktop settings such as providers and
// keys are account-level, not per-project: writing them to the global config
// rather than the cwd's reasonix.toml is what lets them survive a workspace switch.

// applySkillConfigChange edits the config file that owns the selected [skills]
// field. Project skill settings shadow the global setting at runtime, so
// writing only the user config would make the UI appear to save while the
// active project continued using its old value.

// Serialize the load-modify-save against other in-process config editors
// (applyConfigOnly) so neither drops the
// other's fields. rebuild() runs after unlocking — it does slow work and
// must not hold the config edit lock.

// applyGlobalProviderConfigChange persists a provider-wide setting and refreshes
// every visible runtime while runtime admission and all turn gates are frozen.
// Detached runtimes cannot participate in the failure-atomic build-and-swap, so
// reject before writing instead of leaving one session on stale provider state.

// A startup placeholder has no stale provider runtime. Its eventual
// build reads the just-persisted config.

// Bind both the warning and the retry to the tab whose refresh failed, so a
// tab switch or a multi-tab mutation cannot misroute either one.

// loadDesktopUserConfigForEdit loads the user config for a write path. Pending
// legacy migrations are assembled in memory and reach disk through the locked
// user-config save, never by rewriting a project file as a side effect.
//
// Contract: the caller must already hold config.LockUserConfigEdits() across
// its whole load→mutate→SaveTo cycle, so the migration write-back cannot race
// other in-process config editors. This helper must never acquire that lock
// itself: applyConfigChange/applyConfigOnly (and every other caller) invoke it
// with the lock held, so an inner acquire would self-deadlock. Read-only
// callers must use loadDesktopUserConfigForView (or its WithCredentials
// variant), which never writes to disk.

// loadDesktopUserConfigForView loads the user config for read-only callers.
// Contract: it never writes to disk, so it is safe without
// config.LockUserConfigEdits(). Legacy migrations (provider-access normalize)
// are applied to the returned copy in memory only;
// the on-disk file migrates the first time a locked write path runs
// loadDesktopUserConfigForEdit. Credentials (Reasonix global .env) are not
// loaded; callers that hand the config to a runtime resolving secrets from the
// process env must use loadDesktopUserConfigForViewWithCredentials.

// loadDesktopUserConfigForViewWithCredentials is loadDesktopUserConfigForView
// plus credential resolution: like config.LoadForEdit it loads Reasonix's
// global .env into the process env. Use it for read-only loads whose result
// feeds a runtime that resolves env-based secrets — MCP server connects. It
// still never writes to disk.

// loadDesktopUserConfigReadOnlyForRoot is the shared pure-read loader behind
// the View variants: same shape as loadDesktopUserConfigForEdit, but every
// legacy migration stays in memory (zero SaveTo) and resolves from root.

// The user config does not exist yet: serve the legacy config as the view;
// the write path creates the migrated user file later.

// normalizeLegacyDesktopProviderAccessForSettings is the write-path variant:
// it normalizes in memory and persists the migrated form to path. Callers must
// hold config.LockUserConfigEdits() (see loadDesktopUserConfigForEdit). Read
// paths use normalizeLegacyDesktopProviderAccessInMemory instead.

// normalizeLegacyDesktopProviderAccessInMemory seeds cfg.Desktop.ProviderAccess
// from configs written before Settings tracked explicit provider access. It
// never touches disk; it reports whether cfg now carries a normalized list
// that the file at path does not declare (i.e. whether a write path should
// persist it).

// rebuild builds a replacement controller from the (just-changed) config and
// swaps it in only after the target session lease is available. The old
// controller stays usable if the rebuild fails.

// Serialize with SetModelForTab and the deferred-rebuild retry loop: two
// concurrent build+swap sequences on the same tab leak the first-swapped
// controller and double-close the old one.

// rebuildSettingLocked is rebuildSetting's body; callers must already hold
// runtimeRebuildMu. The deferred-rebuild retry loop calls this directly because
// it takes the lock across its lease probe.

// rebuildSettingTurnLocked is rebuildSettingLocked's body; callers must hold
// runtimeRebuildMu and the passed tab's turnStartMu. admissionHeld is true for
// MCP lifecycle callers that also hold runtimeAdmissionMu's write side.
// reload selects the stage-3b runtime-reload build path (boot.Rebuild migrates
// the session) instead of the legacy boot.Build + manual migration; everything
// else — active-work guards, workspace prep, lease moves, swap, close-after-
// swap, fence — is shared.

// rebuildSettingTurnLockedWithModel optionally builds the replacement for a
// target model without changing tab.model before the swap. Provider removal
// uses this to remain failure-atomic: a failed fallback build leaves both the
// old controller and its visible model identity untouched.

// Supersede any in-flight startup build: it would otherwise finish later,
// pass its generation check, and overwrite the controller just installed.

// True subgraph rebuilds reuse the same controller pointer — never Close it.

// buildSettingReplacementController builds the replacement controller for
// rebuildSettingTurnLocked and migrates the session onto it, returning the
// controller, the runtime posture actually restored, and the session path it
// bound. reload=false is the legacy settings path (boot.Build plus the
// desktop's manual migration); reload=true is the stage-3b runtime reload,
// routing build and migration through boot.Rebuild so history, approval mode
// and grants, plan/goal state, and lifecycle move inside the boot layer. The
// caller owns the swap, closing the old controller after the swap, and the
// post-swap persistence.

// boot.Rebuild migrated history (same session file, fresh system
// prompt spliced), approval mode and grants, plan/goal state, and
// lifecycle. The interactive approval gate and the plan/yolo tab
// mode are desktop wiring Rebuild deliberately leaves out — the
// mode re-apply also restores yolo, which Rebuild does not carry.

// Same path Rebuild pinned internally (identical inputs), recomputed
// for the lease move and the post-swap persistence.

// Same-session rebuild without the full boot.Rebuild path still must keep
// the private temporary directory (Issue #7575).

// runtimeReloadSettingLabel is the settings-style label used in busy/lease
// error text and notices for an explicit runtime reload.

// ReloadRuntime rebuilds the tab's agent runtime in place — tools, skills,
// commands, hooks, providers, and MCP servers are re-discovered from the
// current config — while the session carries over (transcript, approval
// grants, goal/recovery state, shared plugin Host) via boot.Rebuild. Active
// work or a held lease queues exactly one reload on the deferred-rebuild
// loop, which runs it once the tab is idle; a failure keeps the old
// controller fully usable.

// Same serialization as rebuildSetting: two build+swap sequences on the
// same tab must not interleave.

// Queue exactly one reload per tab (the pending map coalesces
// duplicates); the loop retries once the work finishes or the lease
// clears.

// reloadRuntimeTurnLocked runs the in-place runtime reload for tab; callers
// hold runtimeRebuildMu (the deferred-rebuild retry loop also drives it).

// SetDefaultModel sets the config default and switches the live model to it.

// applyConfigChange ends in rebuild(), which reads tab.model to pick the
// runtime model — the new ref must be visible on the tab before that runs.

// SetPlannerModel sets (or, with "", clears) the two-model planner.

// SetSubagentModel sets (or clears) the default model used by subagent entry points.

// SetSubagentEffort sets (or clears) the default effort used by subagent entry points.

// deleteSubagentOverrideAliases removes every underscore/hyphen alias entry
// for name (boot.SubagentModelKeys — the same key set runtime dispatch
// reads). Deleting only the exact key would leave a legacy alias entry (e.g.
// `security_review` for the security-review skill) silently active.

// SetSubagentProfileModel sets (or clears) a per-name model override for a
// subagent — the only way to influence a built-in subagent's model in the
// Subagents settings page, since built-ins have no editable frontmatter file
// to carry a `model:` line. Writes into the same cfg.Agent.SubagentModels map
// internal/boot's subagentModelRef already reads at dispatch time. Set and
// clear both sweep the underscore/hyphen alias keys so a legacy alias entry
// can neither shadow the new value nor survive a clear.

// SetSubagentProfileEffort sets (or clears) a per-name effort override. See
// SetSubagentProfileModel.

// Validate against the model the override will actually apply to:
// the alias-aware per-name model override first, then the global
// subagent default, then the session default.

// SetMaxSubagentDepth controls whether first-layer subagents may delegate once more.

// SetMaxSubagentConcurrency sets the session-wide sub-agent concurrency cap (1–32).

// SetMaxParallelWriters sets the concurrent writer cap (1–32, ≤ total concurrency).

// SetAutoPlan is retained for older frontend bundles. Automatic plan mode is
// retired, so "off" is an idempotent compatibility call and enabling it is
// rejected without mutating user configuration or live controllers.

// SetDefaultToolApprovalMode updates the global Ask/Auto/YOLO default used only
// for newly-created desktop sessions. Existing tabs keep their persisted mode.

// SetDefaultAutoRecoveryCheckpoint is retained as a no-op Wails surface for
// older generated frontends. Auto Guard is always built into Auto.

// display language no longer selects list-price tables

// Freeze the official USD regional table; display currency is independent.

// Settings exposes this switch only for verified endpoints. Preserve an
// existing advanced override, but never carry an official default to a new URL.

// also satisfies validateProvider's model requirement

// SaveProvider adds or updates a provider. Enabled models are persisted through
// `models` even when only one model is selected, while `model` remains populated
// in-memory for validation/back-compat. The shared key/endpoint live on the entry.

// SetProviderWebSearch updates every provider represented by one Settings
// access card in a single config transaction. Legacy DeepSeek aliases can
// remain separate when their custom transport fields differ, so changing only
// the first profile would leave the grouped control in a contradictory state.

// keep validation/back-compat populated

// SaveProviderModelCatalogs applies only model-catalog fields. Each update is
// compared against the provider snapshot that launched discovery while the
// config edit lock is held, so an older async completion cannot overwrite newer
// provider edits. Stale updates are skipped rather than treated as failures.

// Re-read while holding the same lock as every Reasonix credential
// writer, then keep that lock through the config commit. A rotation that
// won the race therefore invalidates the request fingerprint.

// SaveProviderWithKey saves a custom provider and its credential as one settings
// transaction, then rebuilds once after both are visible to the runtime.

// UpgradeDeepSeekProviderAccess applies the explicit Settings action for an
// official legacy OpenAI entry. The config package performs a narrow raw-TOML
// edit so unrelated and future fields are not lost to a full config render.

// A visible startup placeholder has no stale provider runtime. Its
// eventual build will read the upgraded config directly.

// The protocol is user-global and already persisted. Keep refreshing
// sibling tabs so one workspace-specific boot failure cannot leave
// every later runtime pinned to the old protocol.

// visibleTabsForGlobalRuntimeUpgrade returns every visible runtime that must
// observe a user-global provider protocol change. A detached controller cannot
// use rebuildSettingTurnLocked because it is intentionally absent from a.tabs;
// reject before mutating config so it never remains silently pinned to the old
// protocol. runtime mutation admission and all turn gates are held by callers.

// AddProviderPresetAccess installs one editable custom-provider preset. Unlike
// official built-ins, these entries are saved as normal providers so users can
// tweak endpoints, model lists, and capability overrides after the one-click
// setup path.

// Read-only duplicate-name pre-check; applyConfigChange re-checks under the
// config edit lock before writing.

// ResetProviderPresetAccess intentionally overwrites same-name provider entries
// with the curated preset template. It only mutates config; provider secrets stay
// in Reasonix home .env under whichever api_key_env the resulting preset uses.

// Read-only existence pre-check; applyConfigChange re-checks under the
// config edit lock before writing.

// FetchProviderModels probes the provider's OpenAI-compatible model-list
// endpoint and returns the available model IDs. This is a settings-only helper:
// it never touches chat request serialization or provider-visible prompt data.

// FetchAllProviderModels fetches model lists for all providers in a single
// batch. Models are fetched concurrently (up to 4 parallel requests) and
// returned as a map keyed by provider name. Errors for individual providers
// are recorded as nil entries; callers should handle missing keys.

// Omit failed providers so the frontend can retry them through
// the cached single-provider path without emitting JSON null.

// rebuildActiveSettingRuntimeMutationLocked refreshes the active controller
// while lockRuntimeMutation and all runtime turn gates are held.

// SetProviderKey writes a secret to Reasonix's global .env under the given
// env-var name (the one a provider's api_key_env points at) and rebuilds so it
// resolves immediately.

// SaveProviderKey writes a provider secret without rebuilding the chat runtime.
// It is used by settings probes that need credentials only for a model-list
// request; explicit "save key" actions still call SetProviderKey.

// Pure load-modify-save on the user config; the caller (SetProviderKey)
// rebuilds after we return, outside the config edit lock.

// ClearProviderKey removes a provider secret from Reasonix's global .env
// and rebuilds so the provider immediately becomes unauthenticated.

// SetPermissionMode sets the writer-fallback mode (ask|allow|deny).

// AddPermissionRule appends a rule to the allow/ask/deny list.

// RemovePermissionRule drops a rule from the allow/ask/deny list.

// ReloadSettings rebuilds the active controller from the current config without
// changing any config file. It lets manual config.toml edits take effect.

// The on-disk config already diverged from the runtime; retry the
// refresh once the other window releases the session lease.

// SetSandbox updates the bash sandbox mode, network egress, and write roots.

// SetNetwork updates ordinary outbound proxy settings.

// SetCloseBehavior updates desktop-only window close behavior without rebuilding
// the active controller. It must stay out of provider-visible prompt/request data.

// SetDisplayMode updates the transcript display mode. UI-only, no rebuild needed.

// SetStatusBarStyle updates the desktop status bar metric label style. UI-only,
// no rebuild needed.

// SetStatusBarItems updates the ordered visible desktop status bar items.
// UI-only, no rebuild needed.

// SetDesktopLanguage updates the desktop UI language and the user-level response
// language preference used by model-facing desktop sessions.

// SetDesktopCurrency persists a display-only preference and re-selects the
// occurrence-time valuations already stored in each tab. Provider price tables
// and live controllers are intentionally untouched.

// Display currency only — never the provider list-price region.

// Used only for display-language adjacent UI; list prices use billing_currency.

// SetTrayLocale mirrors the resolved desktop UI language into the native tray
// menu. It is runtime-only; the persisted preference remains [desktop].language.

// SetDesktopAppearance updates only desktop theme preferences. It does not
// rebuild the active controller and must stay out of provider-visible requests.

// SetDesktopTerminalTheme updates only the integrated terminal colours. It is
// applied live by the frontend and does not rebuild the active controller.

// SetDesktopLayoutStyle updates only the desktop layout style. It does not
// rebuild the active controller and must stay out of provider-visible requests.

// SetExpandThinking sets whether reasoning text is expanded by default on
// the desktop. It is desktop-only and does not rebuild the controller.

// SetDesktopConversationWidth sets the max transcript width preference.
// standard = 960px fixed; full = 90% of the parent, with a 960px floor. Pure config-only.

// MigrateDesktopPreferences imports old browser-local desktop preferences into
// the user config once. Existing [desktop] values win so stale localStorage never
// overwrites an explicit config edit.

// SetAgentParams updates sampling temperature and the base system prompt. The
// step arguments remain in the Wails contract for older frontends, but are
// retired and deliberately normalized to automatic execution.

// Lock only the load-modify-save cycle; the live-controller fan-out below
// must not hold the config edit lock.

// trimList drops blank entries from a string slice (and returns a non-nil slice).
