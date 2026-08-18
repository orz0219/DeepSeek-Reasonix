package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/netclient"
)

// DefaultSystemPrompt is used when config provides none.
const DefaultSystemPrompt = `You are Reasonix, a coding agent.
Use the available tools when they help you complete the user's request.
Keep changes focused and responses concise.`

// UserDecisionPolicy is appended to every system prompt, including user-custom
// prompts, so custom personas cannot accidentally remove the `ask` UI contract.
const UserDecisionPolicy = `User-owned choices: when a consequential decision has no safe, obvious default, call the ask tool so the user can choose. Otherwise proceed with a sensible reversible default. Do not ask in prose when ask is available. In non-interactive runs, state the assumption and take the safest reversible path.`

// LanguagePolicy is the auto fallback appended to the system prompt when no
// concrete UI language is resolved. It is static English text, so it stays part
// of the cache-stable prefix and avoids per-turn language injection.
const LanguagePolicy = `Reply in the same language the user is using in their most recent message: ` +
	`if they write in Chinese answer in Chinese, in English answer in English, and switch ` +
	`whenever they switch. Let this also guide the language you think in. Always keep code, ` +
	`identifiers, file paths, shell commands, and technical terms in their original form — never translate them.`

// Default returns the built-in default configuration.
func Default() *Config {
	return &Config{
		ConfigVersion:    6,
		DefaultModel:     "deepseek-flash",
		CredentialsStore: CredentialsStoreAuto,
		UI:               UIConfig{Theme: "auto", ShowTurnUsage: true},
		Desktop:          DesktopConfig{DefaultToolApprovalMode: "auto", ConversationWidth: "standard"},
		Billing:          BillingConfig{},
		Notifications: NotificationsConfig{
			Enabled:         false,
			TurnDone:        true,
			ApprovalRequest: true,
			AskRequest:      true,
		},
		Agent: AgentConfig{
			SystemPrompt: DefaultSystemPrompt,

			MaxSteps:        0,
			PlannerMaxSteps: 0,
			AutoPlan:        "off",

			SoftCompactRatio:       0,
			ToolResultSnipRatio:    0,
			CompactRatio:           0.85,
			CompactForceRatio:      0,
			ContextEditing:         "",
			MaxSubagentDepth:       2,
			MaxSubagentConcurrency: 6,
			MaxParallelWriters:     3,
		},

		Permissions: PermissionsConfig{Mode: "ask"},

		Sandbox: SandboxConfig{Network: true},

		LSP:     LSPConfig{Enabled: true},
		Network: NetworkConfig{ProxyMode: netclient.ModeAuto},
		Providers: []ProviderEntry{
			{
				Name: "deepseek-flash", Kind: "anthropic", BaseURL: deepSeekAnthropicBaseURL,
				Model: "deepseek-v4-flash", APIKeyEnv: "DEEPSEEK_API_KEY",
				BalanceURL: "https://api.deepseek.com/user/balance", Thinking: "enabled",
				WebSearch: boolPointer(true), SupportedEfforts: []string{"disabled", "low", "high", "max"}, DefaultEffort: "high",
				ContextWindow: 1_000_000, Price: deepSeekV4FlashPriceUSD(),
				BillingCurrency: "USD", BillingMode: "payg",
			},
			{
				Name: "deepseek-pro", Kind: "anthropic", BaseURL: deepSeekAnthropicBaseURL,
				Model: "deepseek-v4-pro", APIKeyEnv: "DEEPSEEK_API_KEY",
				BalanceURL: "https://api.deepseek.com/user/balance", Thinking: "enabled",
				WebSearch: boolPointer(true), SupportedEfforts: []string{"disabled", "high", "max"}, DefaultEffort: "high",
				ContextWindow: 1_000_000, Price: deepSeekV4ProPriceUSD(),
				BillingCurrency: "USD", BillingMode: "payg",
			},
		},
	}
}

// WriteFile writes the configuration to path as annotated TOML. The write is
// atomic + fsynced so an interrupted write or power loss can never truncate the
// main config into an unparseable state that leaves the app with no usable
// models (#4615, #4708).
func (c *Config) WriteFile(path string) error {
	return atomicWriteToConfigFile(path, RenderTOMLForScope(c, renderScopeForPath(path)), configFilePerm(path))
}

// Provider returns the named provider entry.
func (c *Config) Provider(name string) (*ProviderEntry, bool) {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i], true
		}
	}
	return nil, false
}

// ResolveModel resolves a model reference to a provider entry whose Model is the
// selected model string (a copy, so the config's lists stay intact). It accepts:
//   - "provider/model" — that exact model under that provider;
//   - a provider name   — the provider's default model;
//   - a bare model name — the (first) provider that lists it.
//
// The returned entry is ready to build a provider from (NewProvider reads .Model),
// so a single "vendor with many models" entry yields one instance per model
// without duplicating base_url/api_key_env. Single-`model` entries still resolve
// by provider name, keeping older configs working unchanged.
func (c *Config) ResolveModel(ref string) (*ProviderEntry, bool) {
	if ref == "" {
		return nil, false
	}
	if access := desktopProviderAccessMap(c.Desktop.ProviderAccess); len(access) > 0 {
		if access["deepseek"] && !canCanonicalizeLegacyDeepSeekProviders(c) {
			delete(access, "deepseek")
		}
		ref = retargetDesktopOfficialRef(ref, access)
	}

	if prov, model, ok := strings.Cut(ref, "/"); ok {
		if e, found := c.Provider(prov); found && e.HasModel(model) {
			cp := *e
			cp.Model = model
			cp.applyModelPrice()
			cp.applyModelOverride()
			return &cp, true
		}
	}

	if e, found := c.Provider(ref); found {
		cp := *e
		cp.Model = e.DefaultModel()
		cp.applyModelPrice()
		cp.applyModelOverride()
		return &cp, true
	}

	for i := range c.Providers {
		if c.Providers[i].HasModel(ref) {
			cp := c.Providers[i]
			cp.Model = ref
			cp.applyModelPrice()
			cp.applyModelOverride()
			return &cp, true
		}
	}
	return nil, false
}

// ResolveModelWithFallback resolves a model reference to the canonical
// "provider/model" form used by the desktop runtime. If ref is stale or empty,
// it tries the user's configured default_model before falling back to the first
// configured provider — so preference isn't overwritten by iteration order.
func (c *Config) ResolveModelWithFallback(ref string) (resolvedRef string, fallback bool, ok bool) {
	ref = strings.TrimSpace(ref)
	if ref != "" {
		if e, found := c.ResolveModel(ref); found {
			return e.Name + "/" + e.Model, false, true
		}
	}

	if ref != c.DefaultModel && c.DefaultModel != "" {
		if e, found := c.ResolveModel(c.DefaultModel); found && e.Configured() {
			return e.Name + "/" + e.Model, true, true
		}
	}
	for i := range c.Providers {
		p := &c.Providers[i]

		if len(p.ModelList()) == 0 || !p.Configured() {
			continue
		}
		return p.Name + "/" + p.DefaultModel(), true, true
	}
	return "", false, false
}

// ResolveNewSessionChatModel selects the model for a newly-created chat
// session. Configured candidates win; if every chat candidate is keyless, the
// valid default (or first chat model) is preserved so callers can surface their
// existing missing-key recovery UI. An unknown default is also preserved for
// the CLI's actionable configuration error. Provider order is otherwise stable.
func (c *Config) ResolveNewSessionChatModel() (resolvedRef string, fallback bool, ok bool) {
	return c.resolveNewSessionChatModel(nil, true)
}

func (c *Config) resolveNewSessionChatModel(providerAllowed func(string) bool, preserveUnknownDefault bool) (resolvedRef string, fallback bool, ok bool) {
	if c == nil {
		return "", false, false
	}
	if providerAllowed == nil {
		providerAllowed = func(string) bool { return true }
	}

	def := strings.TrimSpace(c.DefaultModel)
	keylessDefault := ""
	if def != "" {
		if entry, found := c.ResolveModel(def); found {
			if providerAllowed(entry.Name) && IsLikelyChatModel(entry.Model) {
				if entry.Configured() {
					return def, false, true
				}
				keylessDefault = def
			}
		} else if preserveUnknownDefault {

			return def, false, true
		}
	}

	keylessFallback := ""
	for i := range c.Providers {
		p := &c.Providers[i]
		if !providerAllowed(p.Name) {
			continue
		}
		chatModels := p.ChatModelList()
		if len(chatModels) == 0 {
			continue
		}
		model := chatModels[0]
		for _, candidate := range chatModels {
			if candidate == p.DefaultModel() {
				model = candidate
				break
			}
		}
		resolved := p.Name + "/" + model
		if p.Configured() {
			return resolved, true, true
		}
		if keylessFallback == "" {
			keylessFallback = resolved
		}
	}
	if keylessDefault != "" {
		return keylessDefault, false, true
	}
	if keylessFallback != "" {
		return keylessFallback, true, true
	}
	return "", false, false
}

// ResolveDesktopNewSessionModel selects the model for a newly-created desktop
// session. It shares the chat-model fallback policy with other frontends while
// limiting candidates to providers exposed by the desktop access catalog.
func (c *Config) ResolveDesktopNewSessionModel() (resolvedRef string, fallback bool, ok bool) {
	if c == nil {
		return "", false, false
	}
	access := desktopProviderAccessMap(c.Desktop.ProviderAccess)
	disabled := desktopProviderAccessMap(c.Desktop.ProviderDisabled)
	return c.resolveNewSessionChatModel(func(name string) bool {
		name = strings.TrimSpace(name)
		return (c.Desktop.ProviderAccess == nil || access[name]) && !disabled[name]
	}, false)
}

// APIKey resolves the entry's API key from its api_key_env.
func (e *ProviderEntry) APIKey() string {
	if e == nil {
		return ""
	}
	if e.resolvedAPIKey != "" {
		return e.resolvedAPIKey
	}
	if e.APIKeyEnv == "" {
		return ""
	}
	value, _, ok := storedCredentialValue(e.APIKeyEnv)
	if !ok {
		return ""
	}
	return value
}

// ResolveAPIKeyFromProcessEnvForProbe pins a setup-time, user-entered key onto
// this entry for an immediate connectivity probe. Normal runtime resolution does
// not call this; loaded provider entries still resolve only from Reasonix's
// global .env.
func (e *ProviderEntry) ResolveAPIKeyFromProcessEnvForProbe() {
	if e == nil {
		return
	}
	key := strings.TrimSpace(e.APIKeyEnv)
	if key == "" {
		return
	}
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return
	}
	e.resolvedAPIKey = value
	e.resolvedSource = CredentialSource{Kind: CredentialSourceEnvironment, Label: "setup prompt"}
}

func (e *ProviderEntry) APIKeySourceLabel() string {
	if e == nil || strings.TrimSpace(e.APIKeyEnv) == "" {
		return ""
	}
	if e.resolvedAPIKey != "" {
		return credentialSourceLabel(e.resolvedSource)
	}
	return ResolveCredentialForRootGlobalFirst(".", e.APIKeyEnv).Source.Label
}

// RequiresAPIKey reports whether this provider should be hidden/validated when
// its configured api_key_env is empty. A blank api_key_env means the provider is
// intentionally no-auth. Local OpenAI-compatible gateways often keep a legacy
// api_key_env in config even though they accept unauthenticated requests, so
// loopback/private endpoints are also allowed to run without a resolved key.
func (e *ProviderEntry) RequiresAPIKey() bool {
	if e == nil {
		return false
	}
	if strings.TrimSpace(e.APIKeyEnv) == "" {
		return providerBaseURLRequiresAPIKey(e.BaseURL)
	}
	return !providerBaseURLAllowsMissingAPIKey(e.BaseURL)
}

func providerBaseURLRequiresAPIKey(raw string) bool {
	switch officialProviderHost(raw) {
	case "api.deepseek.com", "api.xiaomimimo.com", "token-plan-cn.xiaomimimo.com", "api.minimaxi.com", "api.openai.com":
		return true
	default:
		return false
	}
}

func providerBaseURLAllowsMissingAPIKey(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.Trim(strings.ToLower(u.Hostname()), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast()
}

// Configured reports whether the provider is selectable. Providers that do not
// require an API key are configured by definition; providers that name an env var
// require that variable to resolve unless their endpoint is local/private.
func (e *ProviderEntry) Configured() bool {
	return e != nil && (!e.RequiresAPIKey() || e.APIKey() != "")
}

// ResolveSystemPrompt returns the system prompt, reading system_prompt_file if set.
func (c *Config) ResolveSystemPrompt() (string, error) {
	return c.ResolveSystemPromptForRoot(".")
}

// ResolveSystemPromptForRoot is like ResolveSystemPrompt but resolves a relative
// system_prompt_file against root. Desktop tabs pass their workspace root here so
// prompt files are project-scoped even when the process cwd is elsewhere. A path
// inherited from user config may fall back to Reasonix home, while a path chosen
// by project config is confined to the workspace and never probes user files.
func (c *Config) ResolveSystemPromptForRoot(root string) (string, error) {
	path := c.Agent.SystemPromptFile
	if path == "" {
		return c.InlineSystemPrompt(), nil
	}

	if c.systemPromptFileSource == promptFileSourceProject {
		if filepath.IsAbs(path) || !filepath.IsLocal(filepath.Clean(path)) {
			return "", fmt.Errorf("project system_prompt_file %q must be a relative path within the workspace", path)
		}
		candidate := filepath.Join(resolveRoot(root), path)
		b, err := readProjectSystemPromptFile(root, path)
		if err != nil {
			return "", newSystemPromptFileError(path, []string{candidate}, []error{err})
		}
		return strings.TrimSpace(string(b)), nil
	}

	if filepath.IsAbs(path) {
		b, err := fileencoding.ReadFileUTF8(path)
		if err != nil {
			return "", newSystemPromptFileError(path, []string{path}, []error{err})
		}
		return strings.TrimSpace(string(b)), nil
	}

	candidates := []string{filepath.Join(resolveRoot(root), path)}
	if home := ReasonixHomeDir(); home != "" {
		homeCandidate := filepath.Join(home, path)
		if filepath.Clean(homeCandidate) != filepath.Clean(candidates[0]) {
			candidates = append(candidates, homeCandidate)
		}
	}
	readErrors := make([]error, 0, len(candidates))
	for _, candidate := range candidates {
		b, err := fileencoding.ReadFileUTF8(candidate)
		if err == nil {
			return strings.TrimSpace(string(b)), nil
		}
		readErrors = append(readErrors, fmt.Errorf("%s: %w", candidate, err))
	}
	return "", newSystemPromptFileError(path, candidates, readErrors)
}

func readProjectSystemPromptFile(root, path string) ([]byte, error) {
	workspace, err := filepath.Abs(resolveRoot(root))
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	rootHandle, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, fmt.Errorf("open workspace root %q: %w", workspace, err)
	}
	defer rootHandle.Close()
	f, err := rootHandle.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	return fileencoding.DecodeToUTF8(b), nil
}

func newSystemPromptFileError(configured string, candidates []string, readErrors []error) error {
	allMissing := len(readErrors) > 0
	for _, err := range readErrors {
		if !errors.Is(err, fs.ErrNotExist) {
			allMissing = false
			break
		}
	}
	return &systemPromptFileError{
		configured: configured,
		candidates: append([]string(nil), candidates...),
		errors:     append([]error(nil), readErrors...),
		allMissing: allMissing,
	}
}

// InlineSystemPrompt returns the configured system_prompt, or DefaultSystemPrompt
// when unset. It is the fallback when system_prompt_file cannot be read.
func (c *Config) InlineSystemPrompt() string {
	if strings.TrimSpace(c.Agent.SystemPrompt) == "" {
		return DefaultSystemPrompt
	}
	return c.Agent.SystemPrompt
}

// Validate checks that the selected model's provider is usable.
func (c *Config) Validate(model string) error {
	e, ok := c.ResolveModel(model)
	if !ok {
		return fmt.Errorf("unknown model %q (configured: %s)", model, c.providerNames())
	}
	if e.Kind == "" {
		return fmt.Errorf("provider %q: kind is required", model)
	}
	if e.BaseURL == "" {
		return fmt.Errorf("provider %q: base_url is required", model)
	}
	if strings.TrimSpace(e.APIKeyEnv) != "" && !IsValidCredentialKey(e.APIKeyEnv) {
		return fmt.Errorf("provider %q: api_key_env %q is invalid; use letters, numbers, and underscores, not a model name", model, e.APIKeyEnv)
	}
	if e.RequiresAPIKey() && e.APIKey() == "" {
		return fmt.Errorf("provider %q: missing env %s", model, e.APIKeyEnv)
	}
	return nil
}

func (c *Config) providerNames() string {
	names := make([]string, len(c.Providers))
	for i, p := range c.Providers {
		names[i] = p.Name
	}
	return strings.Join(names, ", ")
}
