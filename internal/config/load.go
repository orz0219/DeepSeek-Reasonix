package config

import (
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/provider"
)

// Load builds the configuration: defaults, then user config, then project
// config, then MCP servers from Claude Code's .mcp.json, then (lowest priority)
// the v0.x ~/.reasonix/config.json's mcpServers. Provider api_key_env values
// resolve from Reasonix's global .env, not from project .env files.
func Load() (*Config, error) {
	return LoadForRoot(".")
}

// LoadForRoot builds the configuration with project files resolved from root
// instead of the current working directory. When root is "" or ".", it behaves
// like Load(). This is the workspace-aware entry point: desktop tabs use it so
// each project's reasonix.toml + .mcp.json are resolved independently without
// changing the process cwd, while provider keys stay rooted in Reasonix home.
//
// Note: LoadForRoot may rewrite legacy MCP `tier` lines on disk (see
// mergeRuntimeTOMLFileSnapshot). Callers that must not mutate config files should use
// LoadForRootReadOnly instead.
func LoadForRoot(root string) (*Config, error) {
	return loadForRoot(root, true)
}

// LoadForRootReadOnly is like LoadForRoot but never writes config files: it skips
// on-disk legacy MCP tier migration. Prefer this for diagnostics, doctor, and
// other read-only inspection paths.
func LoadForRootReadOnly(root string) (*Config, error) {
	return loadForRoot(root, false)
}

// LoadUserConfigReadOnly loads only the trusted user-global config. It never
// reads project reasonix.toml files and never performs on-disk migrations.
// Host-owned features that may execute a configured binary should use this
// instead of LoadForRoot so an untrusted checkout cannot choose the process.
func LoadUserConfigReadOnly() (*Config, error) {
	cfg := Default()
	if path := userConfigLoadPath(); path != "" {
		meta, err := mergeFileSnapshot(cfg, path)
		if err != nil {
			return nil, err
		}
		if meta.IsDefined("agent", "system_prompt_file") {
			cfg.systemPromptFileSource = promptFileSourceUser
		}
	}
	normalizeConfigForEdit(cfg)
	return cfg, nil
}

func loadForRoot(root string, migrateOnDisk bool) (*Config, error) {
	root = resolveRoot(root)
	expansionEnv := loadDotEnvForRoot(root)
	cfg := Default()
	cfg.setExpansionEnv(expansionEnv)
	cfg.CredentialsStore = credentialsStoreMode()

	projectTOML := "reasonix.toml"
	if root != "." {
		projectTOML = filepath.Join(root, "reasonix.toml")
	}
	if primary := userConfigPath(); primary != "" {
		if _, err := resolveConfigAccessPath(primary, true); err != nil {
			return nil, err
		}
	}
	if _, err := resolveConfigAccessPath(projectTOML, false); err != nil {
		return nil, err
	}

	mergeTOML := mergeFileSnapshot
	if migrateOnDisk {
		mergeTOML = mergeRuntimeTOMLFileSnapshot
	}

	var tomlSources []string
	userDefaultModelExplicit := false
	if uc := userConfigLoadPath(); uc != "" {
		tomlSources = append(tomlSources, uc)
		meta, err := mergeTOML(cfg, uc)
		if err != nil {
			// Never rewrite the broken original file. Prefer the last verified
			// snapshot in memory, then built-in defaults, and keep loading so
			// the rest of the app stays usable.
			lkgCfg := Default()
			lkgCfg.setExpansionEnv(expansionEnv)
			lkgCfg.CredentialsStore = credentialsStoreMode()
			if lkgErr := loadLastKnownGoodUserConfig(lkgCfg); lkgErr == nil {
				*cfg = *lkgCfg
				cfg.addLoadWarning(fmt.Sprintf(
					"user config %s is invalid (%v); using last-known-good snapshot in memory without modifying the original file",
					uc, err,
				))
			} else {
				cfg.addLoadWarning(fmt.Sprintf(
					"user config %s is invalid (%v); using built-in defaults in memory without modifying the original file",
					uc, err,
				))
			}
		} else {
			userDefaultModelExplicit = meta.IsDefined("default_model")
			if meta.IsDefined("agent", "system_prompt_file") {
				cfg.systemPromptFileSource = promptFileSourceUser
			}
		}
	}
	// A last-known-good recovery is still trusted user configuration even though
	// the broken source file cannot provide usable TOML metadata.
	if cfg.systemPromptFileSource == promptFileSourceUnknown && cfg.Agent.SystemPromptFile != "" {
		cfg.systemPromptFileSource = promptFileSourceUser
	}
	userDefaultModel := cfg.DefaultModel
	globalCLI := cfg.CLI
	globalSecrets := cfg.Secrets
	globalRemote := cfg.Remote.Clone()
	globalDesktopLanguage := cfg.Desktop.Language
	globalPricingCurrency := cfg.Desktop.Currency
	globalBillingDisplayCurrency := cfg.Billing.DisplayCurrency
	globalTelemetry := cfg.Telemetry

	tomlSources = append(tomlSources, projectTOML)
	projectMeta, err := mergeTOML(cfg, projectTOML)
	if err != nil {
		// Project config damage is isolated to this workspace: continue with
		// user/global config so other tabs stay available.
		cfg.addLoadWarning(fmt.Sprintf(
			"project config %s is invalid (%v); ignored for this workspace",
			projectTOML, err,
		))
		// Drop the project path from later multi-file merges so a broken TOML
		// cannot fail plugin/provider re-merges.
		tomlSources = tomlSources[:len(tomlSources)-1]
	} else if projectMeta.IsDefined("agent", "system_prompt_file") {
		cfg.systemPromptFileSource = promptFileSourceProject
	}
	// The native CLI update channel controls the one user-installed binary.
	// A repository-local reasonix.toml must never switch that global choice.
	cfg.CLI = globalCLI
	// Secret protection is a user-global security control: a cloned repo's
	// reasonix.toml must not be able to flip on the workflow-breaking env/path
	// protections.
	cfg.Secrets = globalSecrets
	// Remote SSH hosts are equally user-global: a cloned repo's reasonix.toml
	// must not be able to inject hosts, jump chains, or port forwards that
	// steer where Reasonix opens connections.
	cfg.Remote = globalRemote
	// Desktop language and pricing currency are user-level regional preferences.
	// A repository must not be able to alter how the user's spend is shown.
	cfg.Desktop.Language = globalDesktopLanguage
	cfg.Desktop.Currency = globalPricingCurrency
	cfg.Billing.DisplayCurrency = globalBillingDisplayCurrency
	// CLI telemetry is an explicit user-global privacy choice. Project config
	// cannot opt a user in or out, including when the global value is absent.
	cfg.Telemetry = globalTelemetry
	// TOML decoding replaces [[plugins]] wholesale, so cfg.Plugins now holds
	// only the last file's. Re-merge by name across all sources (later wins) so a
	// project reasonix.toml doesn't drop the global config's MCP servers.
	// mergeTOMLPlugins only reads files; it does not run on-disk migrations.
	plugins, err := mergeTOMLPlugins(tomlSources)
	if err != nil {
		cfg.addLoadWarning(fmt.Sprintf("plugin configuration could not be merged (%v); continuing without those entries", err))
	} else {
		cfg.Plugins = plugins
	}
	if providers, providerSources, shadowedProjectProviders, ok, err := mergeTOMLProviders(tomlSources); err != nil {
		cfg.addLoadWarning(fmt.Sprintf("provider configuration could not be merged (%v); keeping providers already loaded", err))
	} else if ok {
		cfg.Providers = providers
		cfg.providerSources = providerSources
		cfg.shadowedProjectProviders = shadowedProjectProviders
	}
	if access, ok, err := mergeTOMLProviderAccess(tomlSources); err != nil {
		cfg.addLoadWarning(fmt.Sprintf("provider access configuration could not be merged (%v)", err))
	} else if ok {
		cfg.Desktop.ProviderAccess = access
	}

	// Claude Code's .mcp.json (project root) is read last and merged into
	// [[plugins]], so a server configured for Claude works here unchanged.
	// Project reasonix.toml wins on a name collision; project .mcp.json wins
	// over a same-name user-global entry (see mergeMCPJSON).
	mcpFile := mcpJSONFile
	if root != "." {
		mcpFile = filepath.Join(root, mcpJSONFile)
	}
	entries, err := loadMCPJSON(mcpFile)
	if err != nil {
		cfg.addLoadWarning(fmt.Sprintf("project .mcp.json is invalid (%v); MCP servers from that file are ignored", err))
	} else {
		cfg.mergeMCPJSON(entries)
	}

	// Lowest priority before the one-time v1.9.1 MCP migration: the v0.x
	// ~/.reasonix/config.json's mcpServers. Once the migration marker exists, the
	// current config is authoritative even when it is empty; reading the legacy
	// source again would resurrect servers the user removed from current config.
	if !mcpGlobalMigrationComplete() {
		cfg.mergeMCPJSON(loadLegacyMCP(legacyConfigPath()))
	}
	_ = mergeInstalledPluginPackages(cfg, root)
	normalizePluginCommandLines(cfg)
	normalizeLegacyEffort(cfg)
	cfg.ignoredLegacyStepLimits = normalizeLegacyAgentStepLimits(cfg)
	normalizeRetiredAutoPlan(cfg)
	normalizeLegacyMCPTiers(cfg)
	normalizeLegacyStepFunBaseURLs(cfg)
	normalizeLegacyLongCatContextWindows(cfg)
	normalizeLegacyQwenContextWindows(cfg)
	normalizeLegacyKimiK3Catalog(cfg)
	normalizeLegacyOpenCodeGoKimiK3Catalog(cfg)
	normalizeLegacyMimoCustomProviders(cfg)
	normalizeLegacyProviderModels(cfg)
	normalizeDesktopOfficialProviderAccess(cfg)
	normalizeOfficialDeepSeekModels(cfg)
	migrateBillingDisplayCurrency(cfg)
	freezeProviderBillingCurrencies(cfg)
	applyDeepSeekOfficialDefaultPricing(cfg)
	backfillDeepSeekOfficialPrices(cfg)
	normalizeEffortConfig(cfg)
	backfillDeepSeekPro(cfg)
	if userDefaultModelExplicit {
		restoreUnresolvableProjectDefaultModel(cfg, userDefaultModel)
	}
	cfg.CredentialsStore = credentialsStoreMode()
	cfg.setExpansionEnv(expansionEnv)
	resolveProviderCredentialsForRoot(root, cfg)
	return cfg, nil
}

// LoadBuiltinDefaultsForRoot returns a read-only built-in-only configuration
// without reading or migrating user/project TOML. Diagnostic and recovery tools
// use it when configuration is malformed; it does not put the process into any
// degraded product "mode". Provider credentials still resolve only from
// Reasonix's global credential store.
func LoadBuiltinDefaultsForRoot(root string) *Config {
	cfg := Default()
	cfg.Plugins = nil
	cfg.Skills = SkillsConfig{}
	cfg.Bot.Enabled = false
	cfg.Bot.Connections = nil
	cfg.Bot.Routes = nil
	cfg.Statusline.Command = ""
	cfg.LSP.Enabled = false
	cfg.setExpansionEnv(nil)
	cfg.CredentialsStore = credentialsStoreMode()
	resolveProviderCredentialsForRoot(root, cfg)
	return cfg
}

// LoadRecoveryDefaultsForRoot is retained as an alias of LoadBuiltinDefaultsForRoot
// for older recovery call sites.
func LoadRecoveryDefaultsForRoot(root string) *Config {
	return LoadBuiltinDefaultsForRoot(root)
}

func (c *Config) setExpansionEnv(env map[string]string) {
	if c == nil {
		return
	}
	c.expansionEnv = cloneStringMap(env)
	for i := range c.Plugins {
		c.Plugins[i].expansionEnv = c.expansionEnv
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

// restoreUnresolvableProjectDefaultModel falls back to the user/global
// default_model when a project reasonix.toml overrides it with a reference no
// configured provider serves (#4218). Pre-v1.11 persistence paths (e.g. the
// "always allow" writer) full-rendered ./reasonix.toml and pinned the built-in
// default_model ("deepseek-flash") into it; once the user's [[providers]]
// replaced the built-in presets, that stale name resolved to nothing and boot
// hard-failed in every launch from that folder. In-memory only — the project
// file is untouched, and a project override that does resolve still wins. The
// ignored value is kept so boot can surface a notice.
//
// Callers must only invoke this when the user config explicitly defines
// default_model: falling back to the built-in default would silently mask a
// broken ref when the project file is the user's only config, and that case
// must keep the actionable boot error (TestBuildUnknownModelErrorIsActionable).
func restoreUnresolvableProjectDefaultModel(c *Config, userDefault string) {
	if c == nil {
		return
	}
	if c.DefaultModel == userDefault {
		return
	}
	if _, ok := c.ResolveModel(c.DefaultModel); ok {
		return
	}
	if _, ok := c.ResolveModel(userDefault); !ok {
		return
	}
	c.ignoredProjectDefaultModel = c.DefaultModel
	c.DefaultModel = userDefault
}

// tomlFileDefinesKey reports whether the TOML file at path explicitly defines
// the given top-level key. Missing or unparseable files report false.
func tomlFileDefinesKey(path string, key ...string) bool {
	var f Config
	meta, err := decodeTOMLFile(path, &f)
	if err != nil {
		return false
	}
	return meta.IsDefined(key...)
}

// ConfigFileDefinesCompactRatio reports whether path explicitly overrides the
// automatic compaction threshold. It is used by config surfaces that need to
// explain whether the effective value came from defaults, user config, or the
// current project.
func ConfigFileDefinesCompactRatio(path string) bool {
	return tomlFileDefinesKey(path, "agent", "compact_ratio")
}

// ConfigFileDefinesSkillKey reports whether a project or user TOML file
// explicitly owns one of the supported [skills] settings. Desktop settings use
// this narrow provenance check to edit the file that wins at runtime instead
// of persisting a shadowed value to the global config.
func ConfigFileDefinesSkillKey(path, key string) bool {
	switch strings.TrimSpace(key) {
	case "paths", "excluded_paths", "disabled_skills", "disable_implicit_invocation", "max_depth":
		return tomlFileDefinesKey(path, "skills", key)
	default:
		return false
	}
}

// backfillDeepSeekPro restores deepseek-pro for configs the pre-fix setup wizard
// wrote with only deepseek-v4-flash: a keyless /models probe used to drop the Pro
// SKU, leaving users unable to switch to it. In-memory only — the user's file is
// untouched. Narrowly scoped to the official DeepSeek endpoint (which is known to
// serve pro) so a custom flash-only deployment isn't given an entry that 404s.
func backfillDeepSeekPro(c *Config) {
	const flashModel, proModel = "deepseek-v4-flash", "deepseek-v4-pro"
	var flash *ProviderEntry
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Name == "deepseek-pro" {
			return
		}
		for _, m := range p.ModelList() {
			switch m {
			case proModel:
				return // pro already reachable
			case flashModel:
				if strings.Contains(p.BaseURL, "api.deepseek.com") {
					flash = p
				}
			}
		}
	}
	if flash == nil {
		return
	}
	// If the user has explicitly curated a model list for the flash provider
	// (e.g. unchecked pro in Settings), respect that choice and do not backfill.
	if len(flash.Models) > 0 {
		return
	}
	for _, bp := range Default().Providers {
		if bp.Name == "deepseek-pro" {
			bp.APIKeyEnv = flash.APIKeyEnv
			// Inherit the flash provider's frozen billing currency for list prices.
			currency := flash.ProviderBillingCurrency()
			if currency == "" {
				currency = flash.persistedOfficialCurrency
			}
			if currency == "" {
				currency = "USD"
			}
			bp.BillingCurrency = currency
			bp.persistedOfficialCurrency = currency
			bp.Price = deepSeekV4PriceForModel(currency, proModel)
			c.Providers = append(c.Providers, bp)
			return
		}
	}
}

func backfillDeepSeekOfficialPrices(c *Config) {
	if c == nil {
		return
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if officialProviderKind(p) != "deepseek" {
			continue
		}
		backfillDeepSeekOfficialEndpointDefaults(p)
		currency := p.ProviderBillingCurrency()
		if currency == "" {
			currency = p.persistedOfficialCurrency
		}
		if currency == "" {
			currency = "USD"
		}
		defaults := DeepSeekV4PricesForCurrency(currency)
		if p.Price != nil {
			continue
		}
		if p.Prices == nil {
			p.Prices = map[string]*provider.Pricing{}
		}
		for model, price := range defaults {
			if p.HasModel(model) && p.Prices[model] == nil {
				p.Prices[model] = clonePricing(price)
			}
		}
	}
}

// backfillDeepSeekOfficialEndpointDefaults restores the two official-endpoint
// fields a config may legitimately omit. Both are safe to infer here precisely
// because the caller already matched api.deepseek.com: the wallet endpoint is
// the vendor's own, and 1M is that vendor's real window. Values the file
// declares are never overwritten.
//
// This is keyed on the endpoint rather than on list position, so it cannot leak
// onto a custom provider the way the previous positional decode overlay did
// (#7357, #7358).
func backfillDeepSeekOfficialEndpointDefaults(p *ProviderEntry) {
	if p == nil {
		return
	}
	if strings.TrimSpace(p.BalanceURL) == "" {
		p.BalanceURL = "https://api.deepseek.com/user/balance"
	}
	backfillOfficialContextWindow(p, 1_000_000)
}

func officialProviderKind(p *ProviderEntry) string {
	if p == nil {
		return ""
	}
	u, err := url.Parse(strings.TrimSpace(p.BaseURL))
	if err != nil {
		return ""
	}
	if strings.EqualFold(u.Hostname(), "api.deepseek.com") {
		return "deepseek"
	}
	return ""
}

func resolveRoot(root string) string {
	if root == "" || root == "." {
		return "."
	}
	return filepath.Clean(root)
}

// normalizeLegacyEffort migrates the retired DeepSeek effort="off" (the old
// /thinking off that disabled thinking) to the provider default, so a config
// written by an older version keeps loading instead of erroring on a value the
// provider no longer accepts.
func normalizeLegacyEffort(c *Config) {
	for i := range c.Providers {
		if strings.EqualFold(strings.TrimSpace(c.Providers[i].Effort), "off") {
			c.Providers[i].Effort = ""
		}
	}
}

// mergeTOMLPlugins merges [[plugins]] across TOML sources by name (later source wins).

// mergeTOMLProviders merges [[providers]] across TOML sources by provider name.
// User-global providers win over same-named project providers; project providers
// only fill names the global config does not define. Keep official legacy aliases
// distinct here: they can carry different default models and effort capabilities,
// and the later desktop normalization layer handles canonical Settings access.

// mergeTOMLProviderAccess merges desktop.provider_access across TOML sources so
// project desktop settings do not hide account-level providers from the desktop
// model switcher.

// Preserve declaration state even when the list is explicitly empty.
// A nil slice means legacy/undeclared access; a non-nil empty slice
// means the user intentionally removed every desktop provider.

// An undeclared user list means "allow all"; a union with a project-only
// list would silently narrow that to whatever the project happens to name.

// ConfigFileDeclarations contains provider settings explicitly declared by one
// TOML file, without defaults or values inherited from another scope.

// InspectConfigFileDeclarations returns the provider-related fields explicitly
// present in one TOML file. It deliberately does not include built-in defaults
// or values inherited from another config scope.

// DesktopProviderAccessDeclared reports whether path explicitly declares
// desktop.provider_access. It distinguishes omission from an intentional [].

// LoadForEdit returns a config to seed the `reasonix setup` wizard when reconfiguring:
// the built-in defaults with the file at path (if present) decoded on top, so a
// reconfigure preserves the user's existing providers and agent settings instead
// of resetting to defaults. Reasonix's global .env is loaded so api_key_env
// resolution works while the wizard decides which keys are still missing.

// LoadForEditReadOnlyStrict is the error-returning commit-time variant. It must
// not fall back to defaults when another writer leaves malformed TOML, because
// saving that fallback would overwrite the user's recoverable file.

// LoadForEditWithoutCredentialsReadOnlyStrict is the credential-free strict
// edit loader. It never writes migrations and never substitutes defaults for a
// malformed file.

// ValidateFile parses one TOML config in isolation without loading credentials,
// applying migrations, or writing the file. A missing file is valid.

// ValidateBytes parses one in-memory TOML config without loading credentials,
// applying migrations, or writing any state.

// markExplicitDefaultProjectSkillKeys preserves project skill fields that are
// explicitly present in a file but equal the built-in default. Without this
// transient provenance, saving an unrelated project setting would mistake an
// intentional `false`/empty override for a stale delta and remove it.

// normalizeRetiredMultiThresholdCompaction clears retired multi-threshold keys
// so they never reach the Agent. Disk migration removes them on ordinary start;
// loading still ignores them if migration could not rewrite the file.

// normalizeRetiredAutoPlan keeps pre-v5 configs readable while enforcing the
// single explicit-plan experience. The deprecated fields remain in AgentConfig
// only so old TOML and older desktop payloads decode safely.

// mergeFile decodes a TOML file onto cfg if it exists. An absent file is not an error.
func mergeFile(cfg *Config, path string) error {
	_, err := mergeFileSnapshot(cfg, path)
	return err
}

// mergeFileSnapshot decodes one immutable read of a TOML file onto cfg and
// returns metadata from those exact bytes. Callers that derive source or
// precedence decisions from metadata must use this result instead of reading
// the path again: a config file may be atomically replaced between reads.
func mergeFileSnapshot(cfg *Config, path string) (toml.MetaData, error) {
	return mergeFileSnapshotWithRead(cfg, path, fileencoding.ReadFileUTF8)
}

func mergeFileSnapshotWithRead(cfg *Config, path string, readFile func(string) ([]byte, error)) (toml.MetaData, error) {
	resolved, exists, err := statConfigPath(path)
	if err != nil {
		return toml.MetaData{}, err
	}
	if !exists {
		return toml.MetaData{}, nil
	}
	data, err := readFile(resolved)
	if err != nil {
		return toml.MetaData{}, fmt.Errorf("config %s: %w", path, err)
	}
	// BurntSushi/toml decodes struct fields incrementally and can leave earlier
	// fields mutated when a later value has the wrong type. Validate the complete
	// snapshot against a disposable Config before merging those same bytes into
	// the active object. This makes user LKG fallback, project-level isolation,
	// and metadata-derived provenance transactional with respect to file changes.
	var validated Config
	if _, err := decodeTOMLBytes(data, &validated); err != nil {
		return toml.MetaData{}, fmt.Errorf("config %s: %w", path, err)
	}
	meta, err := decodeTOMLBytes(data, cfg)
	if err != nil {
		return toml.MetaData{}, fmt.Errorf("config %s: %w", path, err)
	}
	if meta.IsDefined("providers") {
		var persisted Config
		if _, err := decodeTOMLBytes(data, &persisted); err != nil {
			return toml.MetaData{}, fmt.Errorf("config %s: %w", path, err)
		}
		markPersistedDeepSeekOfficialPricing(&persisted)
		markers := map[string]string{}
		for i := range persisted.Providers {
			markers[providerMergeKey(persisted.Providers[i])] = persisted.Providers[i].persistedOfficialCurrency
		}
		for i := range cfg.Providers {
			cfg.Providers[i].persistedOfficialCurrency = markers[providerMergeKey(cfg.Providers[i])]
		}
	}
	return meta, nil
}

func mergeRuntimeTOMLFileSnapshot(cfg *Config, path string) (toml.MetaData, error) {
	if _, err := os.Stat(path); err == nil {
		if err := migrateLegacyMCPTiersFile(path); err != nil {
			slog.Warn("config: legacy mcp tier migration failed", "path", path, "err", err)
		}
	}
	return mergeFileSnapshot(cfg, path)
}

// normalizeLegacyMCPTiers keeps loaded legacy config files on the new product
// behavior: enabled MCP servers connect in the background by default, and the
// retired per-server startup tier is no longer a user-facing setting.

// normalizeLegacyAgentStepLimits keeps old TOML readable without allowing a
// stale hidden value to override the adaptive progress policy. The fields stay
// in AgentConfig for decoder and cross-version desktop compatibility only.

// MigrateLegacyAgentStepLimitsForRoot removes retired [agent] step-limit keys
// from the user and project config selected for root. Boot calls it immediately
// before LoadForRoot, so config-only/read-only commands never rewrite files and
// the runtime can surface exactly one migration notice.

// migrateLegacyAgentStepLimitsFile removes retired [agent] step-limit keys
// before runtime decoding. A process-wide lock makes concurrent desktop tab
// builds observe a single migration; the atomic rewrite protects other readers.

// MigrateLegacyRedactToolOutputForRoot removes the retired
// [secrets].redact_tool_output setting from the user and project configs chosen
// for root. The setting no longer controls any runtime behavior; removing it
// avoids leaving an explicit `true` value on disk that falsely suggests live
// output or transcript redaction is still active.

// MigrateLegacyMemoryCompilerForRoot removes the retired
// [agent].memory_compiler setting from the user and project configs chosen for
// root. The Memory v5 execution compiler was removed; stripping the key avoids
// leaving values on disk that falsely suggest compiler behavior (especially a
// stale verbosity = "compact") is still active.

// MigrateLegacyMultiThresholdCompactionForRoot strips retired soft/snip/force keys.

// tomlStringState tracks whether a line-oriented scan is currently inside a
// TOML multiline string, so retired-key strippers never treat prose inside a
// `"""..."""` or `”'...”'` value (e.g. a config example quoted in a
// system_prompt) as a section header or key assignment.

// advanceTOMLStringState scans one raw line and returns the multiline-string
// state after it. Outside strings it honours single-line strings and `#`
// comments so quote delimiters inside them cannot open a multiline state.
// The scan is intentionally conservative: on malformed input it prefers
// staying/returning outside, which makes callers keep lines rather than
// delete them.

// tomlOutside

// rest of the line is a comment

// single-line basic string

// closing quote (or line end on malformed input)
// single-line literal string

// stripTOMLKeyLines removes top-level `key = ...` assignment lines under the
// named section while leaving every line inside a TOML multiline string
// untouched. All retired-config-key migrations share it so none of them can
// corrupt a multiline value (such as a system_prompt quoting a config
// example). A dropped line is first checked to not itself open a multiline
// value; if it would, the line is kept — for these retired keys that never
// happens (their values are single-line), and keeping a stale line is always
// safer than truncating a string the user wrote.

// Inside a multiline string: never a section header or key line.

// normalizeLegacyProviderModels repairs provider entries written by older
// desktop builds that carried the official provider name/endpoint but omitted the
// model field. The repair is intentionally narrow: valid user-provided model
// lists are left untouched, while known official aliases get the model implied by
// their preset name so model pickers and provider validation have an option.

// Both stepfun.ai (global) and stepfun.com (China) are official endpoints.
// BaseURL is user-owned provider configuration, so neither runtime loading
// nor an unrelated settings save may infer a region and rewrite it.

// normalizeLegacyQwenContextWindows upgrades only installed official Qwen
// presets that still carry the old zero context window and untouched model
// catalog. Custom endpoints, catalogs, provider-wide windows, and existing
// per-model override values remain user-owned.

// normalizeLegacyKimiK3Catalog upgrades only untouched Kimi direct-API model
// catalogs on the official regional endpoints. Custom model lists, endpoints,
// defaults, credentials, and provider-wide settings remain user-owned.

// migrateKimiK3VisionModels preserves explicit provider-level vision choices.
// A nil list or an exact copy of the old preset list indicates that the user
// has not customized vision support and should receive Kimi K3's capability.

// normalizeLegacyOpenCodeGoKimiK3Catalog upgrades only the untouched model
// catalog from the original editable OpenCode Go preset. A user-curated model
// list or custom endpoint is left alone, while other provider edits (headers,
// key env, provider-wide context) survive the additive K3 capability update.

// If the user has explicitly curated a model list (via Settings), respect
// that choice and do not merge additional required models.

// NormalizeLegacyMimoCustomProvidersForRefs appends custom OpenAI-compatible
// MiMo providers needed by legacy refs that live outside reasonix.toml, such as
// restored desktop tab state.

// NormalizeLegacyDesktopProviderAccess seeds the desktop provider-access list
// for configs written before Settings tracked explicit provider access. Callers
// should only use this when they know the TOML did not declare provider_access;
// an explicit empty list means the user removed all access entries.

// CanonicalDesktopOfficialProviderName returns the Settings Center provider ID
// for built-in official provider aliases.

// If the user has explicitly curated a model list (via Settings), respect
// that choice and do not merge additional curated models.

// Start from the legacy entry so current and future transport fields are not
// silently dropped from the effective canonical provider. Identity, catalog,
// pricing and capability fields are merged model by model below.

// These fields can be represented independently for every model in the
// canonical provider. They are compared by legacyDeepSeekModelFieldsCompatible.
