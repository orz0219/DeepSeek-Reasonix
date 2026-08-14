package config

import (
	"slices"
	"strings"

	"reasonix/internal/provider"
)

// AgentConfig configures the harness loop. PlannerModel is optional: when set
// to another provider's name it enables two-model collaboration, where the
// planner handles low-frequency planning in its own session (kept separate so
// each model's prompt prefix stays cache-stable). SubagentModel is the optional
// default for runAs=subagent skills; SubagentModels overrides it per skill name.
type AgentConfig struct {
	SystemPrompt     string `toml:"system_prompt"`
	SystemPromptFile string `toml:"system_prompt_file"`
	// Deprecated compatibility fields. Old TOML and desktop clients may still
	// send them, but config loading normalizes both to zero and rendering omits
	// them. The one-off CLI --max-steps flag remains the explicit control.
	MaxSteps            int     `toml:"max_steps"`
	PlannerMaxSteps     int     `toml:"planner_max_steps"`
	Temperature         float64 `toml:"temperature"`
	PlannerModel        string  `toml:"planner_model"`
	GuardianModel       string  `toml:"guardian_model"`
	GuardianTemperature float64 `toml:"guardian_temperature"`
	// RecoveryModel optionally names a dedicated model for the independent
	// recovery reviewer. Empty falls back to GuardianModel, then the main model.
	RecoveryModel string `toml:"recovery_model"`
	// RecoveryTemperature is accepted from older configs but ignored. Auto
	// Guard review is deterministic at temperature zero.
	RecoveryTemperature float64           `toml:"recovery_temperature"`
	SubagentModel       string            `toml:"subagent_model"`
	SubagentModels      map[string]string `toml:"subagent_models"`
	SubagentEffort      string            `toml:"subagent_effort"`
	SubagentEfforts     map[string]string `toml:"subagent_efforts"`
	MaxSubagentDepth    int               `toml:"max_subagent_depth"`
	// TaskCostBudget lands a task on one summary once it spends this much.
	TaskCostBudget float64 `toml:"task_cost_budget"`
	// TaskTimeBudgetMinutes is the same gate on wall clock. Both ship off.
	TaskTimeBudgetMinutes float64 `toml:"task_time_budget_minutes"`
	// GoalTokenBudget bounds an unattended Goal loop by cumulative tokens.
	// Off unless set: a Goal runs until it finishes or you stop it.
	GoalTokenBudget int `toml:"goal_token_budget"`
	// MaxSubagentConcurrency bounds how many sub-agents (task, fleet items,
	// profile skills, nested children) may run at once in one session.
	// 0 means the default (6). Values outside 1–32 are clamped on load.
	MaxSubagentConcurrency int `toml:"max_subagent_concurrency"`
	// MaxParallelWriters bounds concurrent writer-capable sub-agents that
	// declare non-overlapping write_paths. 0 means the default (3). Must not
	// exceed MaxSubagentConcurrency after normalization.
	MaxParallelWriters int `toml:"max_parallel_writers"`
	// OutputStyle selects a persona/tone block folded into the system prompt at
	// startup (a built-in like "explanatory"/"learning"/"concise", or a custom
	// .reasonix/output-styles/<name>.md). Empty = the unmodified prompt.
	OutputStyle string `toml:"output_style"`
	// Deprecated compatibility field. Automatic plan mode was retired in config
	// version 5; old TOML remains readable, but loading normalizes it to "off"
	// and rendering omits it. Plan mode remains available as an explicit user
	// choice.
	AutoPlan string `toml:"auto_plan"`
	// ReasoningLanguage controls the preferred language for visible reasoning
	// text. Empty/auto follows the conversation language. Applied as transient
	// turn context, not the stable prompt.
	ReasoningLanguage string `toml:"reasoning_language"`
	// Deprecated compatibility field paired with AutoPlan. Old TOML remains
	// readable, but loading clears it and rendering omits it.
	AutoPlanClassifier string `toml:"auto_plan_classifier"`
	// Soft/snip/force are retired compatibility keys; only CompactRatio is active.
	SoftCompactRatio    float64 `toml:"soft_compact_ratio"`
	ToolResultSnipRatio float64 `toml:"tool_result_snip_ratio"`
	CompactRatio        float64 `toml:"compact_ratio"`
	CompactForceRatio   float64 `toml:"compact_force_ratio"`
	// ContextEditing is retired; native tool clearing is no longer an auto path.
	ContextEditing string `toml:"context_editing"`
	// Keep controls which compactable messages stay verbatim beyond the current
	// user-fact/digest floor and recent tail. Empty uses the conservative default
	// of keeping error tool results.
	Keep       []string `toml:"keep"`
	RecentKeep int      `toml:"recent_keep"`
	// ColdResumePrune elides stale tool results when a session reopens past the
	// provider cache window. nil = default enabled.
	ColdResumePrune *bool `toml:"cold_resume_prune"`
	// PlanModeReadOnlyCommands is retained for old config/session round trips. Main
	// Plan bash calls now use the ordinary Permissions classifier and Sandbox.
	PlanModeReadOnlyCommands []string `toml:"plan_mode_read_only_commands"`
}

// ProviderEntry declares a model provider instance. ContextWindow is the model's
// token budget; the harness compacts older history as a turn's prompt approaches
// it (see agent compaction). 0 disables compaction for the instance.
type ProviderEntry struct {
	Name          string            `toml:"name"`
	Kind          string            `toml:"kind"`
	BaseURL       string            `toml:"base_url"`
	ChatURL       string            `toml:"chat_url"`    // legacy OpenAI chat endpoint override; retained with its historical semantics
	RequestURL    string            `toml:"request_url"` // exact provider request URL written by current settings UI
	Model         string            `toml:"model"`       // a single model (back-compat)
	Models        []string          `toml:"models"`      // a vendor's model list (one base_url/key, many models)
	ModelsURL     string            `toml:"models_url"`  // auto-fetch models from this URL on startup
	Default       string            `toml:"default"`     // default model when Models is set (else Models[0])
	APIKeyEnv     string            `toml:"api_key_env"`
	PresetID      string            `toml:"preset_id"`      // curated preset identity; UI-only metadata, not sent to model providers.
	PresetVersion int               `toml:"preset_version"` // curated preset schema version for future migrations.
	Headers       map[string]string `toml:"headers"`        // optional extra HTTP headers for compatible gateways; secrets should stay in api_key_env.
	ExtraBody     map[string]any    `toml:"extra_body"`     // optional extra top-level JSON request body fields for OpenAI-compatible gateways.
	AuthHeader    bool              `toml:"auth_header"`    // for Anthropic-compatible gateways that expect Authorization: Bearer instead of x-api-key.
	// ResponsesMode selects the Responses API context strategy. Empty preserves
	// vendor detection; DeepSeek is stateless while compatible endpoints may use
	// stateful previous_response_id continuation.
	ResponsesMode string `toml:"responses_mode"`
	// ResponsesStateful is the legacy boolean form retained for config
	// compatibility. ResponsesMode wins when both are present.
	ResponsesStateful *bool `toml:"responses_stateful"`
	resolvedAPIKey    string
	resolvedSource    CredentialSource
	BalanceURL        string `toml:"balance_url"` // optional; a provider-specific wallet-balance endpoint (DeepSeek: https://api.deepseek.com/user/balance). Empty = no balance readout.
	ContextWindow     int    `toml:"context_window"`
	// MaxOutputTokens is a protocol-neutral total output budget for one turn.
	// Zero means automatic (not unlimited): ordinary 16K, reasoning 32K, high/max
	// 64K — DeepSeek's default effort is high, so auto is typically ~64K.
	// User guidance: 0 recommended; 32768 ordinary coding/cost control;
	// 65536 heavy reasoning/long tools; 131072 only after finish_reason=length.
	// A negative value omits optional wire limits when the protocol allows;
	// Anthropic still requires max_tokens. Never feeds compact_ratio.
	MaxOutputTokens int                          `toml:"max_output_tokens"`
	Price           *provider.Pricing            `toml:"price"`  // legacy/provider-wide fallback
	Prices          map[string]*provider.Pricing `toml:"prices"` // optional per-model prices; keys are model ids
	// BillingCurrency is the frozen list-price currency (ISO-4217). Independent
	// of [billing].display_currency; switching display never rewrites this.
	BillingCurrency string `toml:"billing_currency"`
	// BillingMode is payg (default) or subscription_equivalent (e.g. MiMo Token Plan).
	BillingMode string `toml:"billing_mode"`

	persistedOfficialCurrency string

	// Thinking / Effort are provider-kind-specific knobs forwarded to the provider
	// via Config.Extra. The anthropic provider reads Thinking="adaptive" to enable
	// extended thinking and Effort ("low".."max") to tune depth. The
	// openai-compatible provider forwards Effort as reasoning_effort for
	// thinking-capable models; DeepSeek V4 Flash accepts low|high|max while
	// other DeepSeek models retain their model-specific capability mapping.
	// Empty = provider default.
	Thinking string `toml:"thinking"`
	Effort   string `toml:"effort"`
	// Vision marks the model as accepting image input. When set, images the user
	// attaches are embedded in the request (image_url for openai-kind, base64
	// blocks for anthropic). Off by default: text-only models 400 on image input,
	// and image tokens are heavy — gating keeps text-only flows cheap (the prompt
	// prefix is byte-identical with no image, so the cache is unaffected either way).
	Vision bool `toml:"vision"`
	// VisionModels narrows image input support to specific models in a multi-model
	// provider. This lets one provider expose both text-only and multimodal chat
	// models without enabling image payloads for every model.
	VisionModels []string `toml:"vision_models"`
	// VisionDetail sets the openai image_url detail hint (low|high); empty = auto
	// (the field is omitted). "low" caps an image to a fixed ~85 tokens for cheap
	// coarse reads; ignored by providers without the knob (e.g. anthropic).
	VisionDetail string `toml:"vision_detail"`
	// WebSearch controls the provider-executed web_search tool for compatible
	// Anthropic and Responses endpoints. Nil lets official DeepSeek endpoints use
	// their product default; non-nil preserves an explicit user choice across
	// config rewrites. DeepSeek returns web_search_tool_result blocks on the
	// Anthropic wire and response.web_search_call events on the Responses wire.
	WebSearch *bool `toml:"web_search"`
	// ReasoningProtocol selects the request shape for OpenAI-compatible reasoning
	// models. Empty/auto uses the model capability registry plus endpoint
	// heuristics. Explicit values select DeepSeek, GLM, Kimi K3, or standard
	// OpenAI reasoning contracts; none disables automatic reasoning controls.
	ReasoningProtocol string `toml:"reasoning_protocol"`
	// SupportedEfforts lists the /effort levels this provider/model exposes.
	// Non-empty values override built-in Kind/BaseURL defaults except for fixed
	// Kimi K3 reasoning. "auto" is the implicit prefix — always accepted.
	// DefaultEffort resolves it; omit DefaultEffort (or set one outside this
	// list) to fall back to SupportedEfforts[0].
	SupportedEfforts []string `toml:"supported_efforts"`
	// DefaultEffort is the /effort level used when the user picks "auto" or
	// has not set Effort. Ignored for empty SupportedEfforts or fixed Kimi K3.
	DefaultEffort string `toml:"default_effort"`
	// ModelOverrides customizes capability metadata after ResolveModel selects a
	// concrete model from a multi-model provider. Use it when a gateway exposes
	// mixed DeepSeek/OpenAI/no-reasoning or mixed vision/text models under one
	// base_url/key.
	ModelOverrides map[string]ProviderModelOverride `toml:"model_overrides"`
	visionOverride *bool
	// NoProxy reaches this provider's base_url directly, never through the proxy.
	// For China-only endpoints a foreign-exit proxy resets the TLS handshake (#2803).
	NoProxy bool `toml:"no_proxy"`
	// CacheTTLMinutes overrides the vendor-default prefix-cache retention used by
	// cold-resume prune. Zero uses the vendor default (DeepSeek/unknown 24h, DashScope/Anthropic 5m).
	CacheTTLMinutes int `toml:"cache_ttl_minutes"`
}

type ProviderModelOverride struct {
	ReasoningProtocol string   `toml:"reasoning_protocol"`
	SupportedEfforts  []string `toml:"supported_efforts"`
	DefaultEffort     string   `toml:"default_effort"`
	Vision            *bool    `toml:"vision"`
	// ContextWindow overrides the provider-wide context budget for this model.
	// Zero inherits ProviderEntry.ContextWindow so existing configurations keep
	// their current compaction behavior.
	ContextWindow int `toml:"context_window"`
	// MaxOutputTokens overrides the provider-wide output budget. Zero inherits;
	// positive values set a cap and negative values omit optional wire limits.
	MaxOutputTokens int `toml:"max_output_tokens"`
}

// ModelList returns the models this provider exposes: the explicit `models` list,
// or the single `model` as a one-element list (back-compat). Empty if neither set.
func (e *ProviderEntry) ModelList() []string {
	if len(e.Models) > 0 {
		return e.Models
	}
	if e.Model != "" {
		return []string{e.Model}
	}
	return nil
}

// IsLikelyChatModel reports whether a model ID looks like a chat/completion
// model rather than a specialised audio/vision/embedding model. It applies a
// conservative name-based heuristic — the OpenAI-compatible /models API does
// not return capability/modality metadata, so this is the most reliable
// fallback until providers add such fields.
//
// The heuristic works in two passes:
//  1. Multi-word substring check for compound terms that span separators
//     (e.g. "text-embedding", "text-to-speech").
//  2. Token-level check: the model ID is split on common separators (- _ . / :)
//     and each token is compared against a set of known non-chat keywords.
//
// "voice" is intentionally absent from the non-chat set because it is too
// broad — legitimate future chat models may include it in their name.
func IsLikelyChatModel(model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	lower := strings.ToLower(model)

	// Pass 1: compound terms that span separator boundaries.
	var compoundNonChat = []string{
		"text-embedding", "text-to-speech", "speech-to-text",
	}
	for _, c := range compoundNonChat {
		if strings.Contains(lower, c) {
			return false
		}
	}

	tokens := strings.FieldsFunc(lower, func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == '/' || r == ':'
	})
	var nonChatTokens = map[string]bool{
		"asr": true, "stt": true, "tts": true,
		"whisper": true, "embedding": true,
		"moderation": true, "rerank": true, "dall": true,
		"transcription": true,
	}
	for _, tok := range tokens {
		if nonChatTokens[tok] {
			return false
		}
	}
	return true
}

// ChatModelList returns ModelList filtered to likely chat/completion models.
// Non-chat models (TTS, STT, ASR, embedding, etc.) are excluded so they do
// not appear in the chat model picker. Use ModelList() only when the full
// raw provider model list is needed, such as config serialization, provider
// diagnostics, or model-fetch editing.
func (e *ProviderEntry) ChatModelList() []string {
	raw := e.ModelList()
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, m := range raw {
		if IsLikelyChatModel(m) {
			out = append(out, m)
		}
	}
	return out
}

// DefaultModel returns the provider's default model: the explicit `default`, else
// the first of ModelList.
func (e *ProviderEntry) DefaultModel() string {
	if e.Default != "" {
		return e.Default
	}
	if l := e.ModelList(); len(l) > 0 {
		return l[0]
	}
	return ""
}

// HasModel reports whether m is one of the provider's models.
func (e *ProviderEntry) HasModel(m string) bool {
	return slices.Contains(e.ModelList(), m)
}

// PriceForModel returns the configured per-1M-token price for model. Per-model
// prices win; the legacy provider-wide price is a fallback for older configs.
func (e *ProviderEntry) PriceForModel(model string) *provider.Pricing {
	if e == nil {
		return nil
	}
	if e.Prices != nil {
		if p := e.Prices[strings.TrimSpace(model)]; p != nil {
			return clonePricing(p)
		}
	}
	return clonePricing(e.Price)
}

func (e *ProviderEntry) applyModelPrice() {
	if e == nil {
		return
	}
	e.Price = e.PriceForModel(e.Model)
}

func (e *ProviderEntry) applyModelOverride() {
	if e == nil || len(e.ModelOverrides) == 0 {
		return
	}
	ov, ok := e.modelOverrideForModel(e.Model)
	if !ok {
		return
	}
	if ov.ReasoningProtocol != "" {
		e.ReasoningProtocol = ov.ReasoningProtocol
	}
	if ov.SupportedEfforts != nil {
		e.SupportedEfforts = append([]string(nil), ov.SupportedEfforts...)
	}
	if ov.DefaultEffort != "" || ov.SupportedEfforts != nil {
		e.DefaultEffort = ov.DefaultEffort
	}
	if ov.Vision != nil {
		e.visionOverride = ov.Vision
	}
	if ov.ContextWindow > 0 {
		e.ContextWindow = ov.ContextWindow
	}
	if ov.MaxOutputTokens != 0 {
		e.MaxOutputTokens = ov.MaxOutputTokens
	}
}

func (e *ProviderEntry) modelOverrideForModel(model string) (ProviderModelOverride, bool) {
	model = strings.TrimSpace(model)
	if e == nil || model == "" || len(e.ModelOverrides) == 0 {
		return ProviderModelOverride{}, false
	}
	if ov, ok := e.ModelOverrides[model]; ok {
		return ov, true
	}
	for k, ov := range e.ModelOverrides {
		if strings.EqualFold(strings.TrimSpace(k), model) {
			return ov, true
		}
	}
	return ProviderModelOverride{}, false
}

func clonePricing(p *provider.Pricing) *provider.Pricing {
	if p == nil {
		return nil
	}
	cp := *p
	return &cp
}
