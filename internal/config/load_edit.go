package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// LoadForEdit returns a config to seed the `reasonix setup` wizard when reconfiguring:
// the built-in defaults with the file at path (if present) decoded on top, so a
// reconfigure preserves the user's existing providers and agent settings instead
// of resetting to defaults. Reasonix's global .env is loaded so api_key_env
// resolution works while the wizard decides which keys are still missing.
func LoadForEdit(path string) *Config {
	return loadForEdit(path, true, false)
}

// LoadForEditReadOnlyStrict is the error-returning commit-time variant. It must
// not fall back to defaults when another writer leaves malformed TOML, because
// saving that fallback would overwrite the user's recoverable file.
func LoadForEditReadOnlyStrict(path string) (*Config, error) {
	return loadForEditStrict(path, true, false)
}

// LoadForEditWithoutCredentialsReadOnlyStrict is the credential-free strict
// edit loader. It never writes migrations and never substitutes defaults for a
// malformed file.
func LoadForEditWithoutCredentialsReadOnlyStrict(path string) (*Config, error) {
	return loadForEditStrict(path, false, false)
}

// ValidateFile parses one TOML config in isolation without loading credentials,
// applying migrations, or writing the file. A missing file is valid.
func ValidateFile(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	_, exists, err := statConfigPath(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	cfg := Default()
	if _, err := decodeTOMLFile(path, cfg); err != nil {
		return fmt.Errorf("config %s: %w", path, err)
	}
	return nil
}

// ValidateBytes parses one in-memory TOML config without loading credentials,
// applying migrations, or writing any state.
func ValidateBytes(data []byte) error {
	cfg := Default()
	if _, err := decodeTOMLBytes(data, cfg); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

func loadForEdit(path string, loadCredentials, persistMigrations bool) *Config {
	cfg, err := loadForEditStrict(path, loadCredentials, persistMigrations)
	if err == nil {
		return cfg
	}
	slog.Warn("config: load for edit failed, using defaults", "path", path, "err", err)
	if loadCredentials {
		loadDotEnvForEditPath(path)
	}
	cfg = Default()
	normalizeConfigForEdit(cfg)
	cfg.editLoadErr = err
	return cfg
}

func LoadForEditWithoutCredentials(path string) *Config {
	return loadForEdit(path, false, false)
}

func loadForEditStrict(path string, loadCredentials, persistMigrations bool) (*Config, error) {
	if loadCredentials {
		loadDotEnvForEditPath(path)
	}
	cfg := Default()
	meta, err := mergeFileSnapshot(cfg, path)
	if err != nil {
		return nil, err
	}
	markExplicitDefaultProjectSkillKeys(cfg, path, meta)
	changed := normalizeConfigForEdit(cfg)
	if persistMigrations && changed && strings.TrimSpace(path) != "" {
		if _, err := os.Stat(path); err == nil {
			if err := cfg.SaveTo(path); err != nil {
				return nil, err
			}
		}
	}
	return cfg, nil
}

// markExplicitDefaultProjectSkillKeys preserves project skill fields that are
// explicitly present in a file but equal the built-in default. Without this
// transient provenance, saving an unrelated project setting would mistake an
// intentional `false`/empty override for a stale delta and remove it.
func markExplicitDefaultProjectSkillKeys(c *Config, path string, meta toml.MetaData) {
	if c == nil || isUserConfigPath(path) {
		return
	}
	for _, key := range projectSkillKeys {
		if !meta.IsDefined("skills", key) || !projectSkillKeyIsDefault(c, key) {
			continue
		}
		if c.explicitProjectSkillKeys == nil {
			c.explicitProjectSkillKeys = make(map[string]bool)
		}
		c.explicitProjectSkillKeys[key] = true
	}
}

func normalizeConfigForEdit(cfg *Config) bool {
	normalizePluginCommandLines(cfg)
	normalizeLegacyEffort(cfg)
	normalizeLegacyAgentStepLimits(cfg)
	changed := normalizeRetiredAutoPlan(cfg)
	changed = normalizeRetiredMultiThresholdCompaction(cfg) || changed
	normalizeLegacyMCPTiers(cfg)
	changed = normalizeLegacyStepFunBaseURLs(cfg) || changed
	changed = normalizeLegacyLongCatContextWindows(cfg) || changed
	changed = normalizeLegacyQwenContextWindows(cfg) || changed
	changed = normalizeLegacyKimiK3Catalog(cfg) || changed
	changed = normalizeLegacyOpenCodeGoKimiK3Catalog(cfg) || changed
	changed = normalizeLegacyMimoCustomProviders(cfg) || changed
	normalizeLegacyProviderModels(cfg)
	normalizeDesktopOfficialProviderAccess(cfg)
	normalizeOfficialDeepSeekModels(cfg)
	migrateBillingDisplayCurrency(cfg)
	freezeProviderBillingCurrencies(cfg)
	applyDeepSeekOfficialDefaultPricing(cfg)
	backfillDeepSeekOfficialPrices(cfg)
	normalizeEffortConfig(cfg)
	return changed
}

// normalizeRetiredMultiThresholdCompaction clears retired multi-threshold keys
// so they never reach the Agent. Disk migration removes them on ordinary start;
// loading still ignores them if migration could not rewrite the file.
func normalizeRetiredMultiThresholdCompaction(c *Config) bool {
	if c == nil {
		return false
	}
	changed := c.Agent.SoftCompactRatio != 0 ||
		c.Agent.ToolResultSnipRatio != 0 ||
		c.Agent.CompactForceRatio != 0 ||
		c.Agent.ColdResumePrune != nil ||
		strings.TrimSpace(c.Agent.ContextEditing) != ""
	c.Agent.SoftCompactRatio = 0
	c.Agent.ToolResultSnipRatio = 0
	c.Agent.CompactForceRatio = 0
	c.Agent.ColdResumePrune = nil
	c.Agent.ContextEditing = ""
	if c.Agent.CompactRatio <= 0 {
		c.Agent.CompactRatio = Default().Agent.CompactRatio
		changed = true
	}
	return changed
}

// normalizeRetiredAutoPlan keeps pre-v5 configs readable while enforcing the
// single explicit-plan experience. The deprecated fields remain in AgentConfig
// only so old TOML and older desktop payloads decode safely.
func normalizeRetiredAutoPlan(c *Config) bool {
	if c == nil {
		return false
	}
	changed := strings.TrimSpace(c.Agent.AutoPlan) != "" && !strings.EqualFold(strings.TrimSpace(c.Agent.AutoPlan), "off") ||
		strings.TrimSpace(c.Agent.AutoPlanClassifier) != ""
	c.Agent.AutoPlan = "off"
	c.Agent.AutoPlanClassifier = ""
	return changed
}

func loadDotEnvForEditPath(path string) {
	path = strings.TrimSpace(path)
	if path == "" || isUserConfigPath(path) {
		loadDotEnv()
		return
	}
	loadDotEnvForRoot(filepath.Dir(path))
}
