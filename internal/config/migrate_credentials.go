package config

import (
	"encoding/base64"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"

	fileencoding "reasonix/internal/fileutil/encoding"
)

func MigrateLegacyCredentialsForRoot(root string) error {
	if IsolatedHomeDir() != "" {
		return nil
	}
	return migrateLegacyCredentialsIfNeededForRoot(root)
}

func migrateLegacyCredentialsIfNeededForRoot(root string) error {
	missing := map[string]string{}

	skipStore := func(key string) bool {
		return credentialCurrentStoreHasKey(key) || credentialCurrentStoreClearedKey(key)
	}

	for _, src := range legacyCredentialsPaths() {
		if src == "" {
			continue
		}
		data, err := fileencoding.ReadFileUTF8(src)
		if err != nil {
			continue
		}
		assignments := parseCredentialLines(strings.Split(string(data), "\n"))
		for key, value := range assignments {
			if _, exists := missing[key]; !exists && !skipStore(key) {
				missing[key] = value
			}
		}
	}
	keys := credentialEnvNamesForRoot(root)
	needKeyring := make([]string, 0, len(keys))
	for _, key := range keys {
		if skipStore(key) {
			continue
		}
		if _, exists := missing[key]; exists {
			continue
		}

		if legacyKeyringMigrationDone(key) {
			continue
		}
		needKeyring = append(needKeyring, key)
	}
	if len(needKeyring) > 0 {
		outcomes := lookupLegacyKeyringBatch(needKeyring, legacyKeyringLookupTimeout)
		for _, key := range needKeyring {
			o := outcomes[key]
			switch o.Status {
			case legacyKeyringFound:

			case legacyKeyringAbsent:

				_ = markLegacyKeyringMigrationDone(key)
			case legacyKeyringError, legacyKeyringTimeout:

			default:

			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	_, err := StoreCredentialLines(credentialLines(missing))
	return err
}

func legacyKeyringMigrationMarkerPath(key string) string {
	home := ReasonixHomeDir()
	key = strings.TrimSpace(key)
	if strings.TrimSpace(home) == "" || key == "" {
		return ""
	}

	name := base64.RawURLEncoding.EncodeToString([]byte(key))
	return filepath.Join(home, "state", "legacy-keyring-checked", name)
}

func legacyKeyringMigrationDone(key string) bool {
	path := legacyKeyringMigrationMarkerPath(key)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func markLegacyKeyringMigrationDone(key string) error {
	path := legacyKeyringMigrationMarkerPath(key)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte("v1\n"), 0o644)
}

func credentialLines(assignments map[string]string) []string {
	keys := make([]string, 0, len(assignments))
	for key := range assignments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, key := range keys {
		lines = append(lines, key+"="+assignments[key])
	}
	return lines
}

func migrateLegacyBaseURL(cfg *Config, baseURL string) {
	baseURL = strings.TrimSpace(baseURL)
	if cfg == nil || baseURL == "" {
		return
	}
	officialDeepSeek := isOfficialDeepSeekOpenAIEndpoint(baseURL)
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if p.APIKeyEnv != "DEEPSEEK_API_KEY" {
			continue
		}
		if officialDeepSeek {

			p.Kind = "anthropic"
			p.BaseURL = deepSeekAnthropicBaseURL
			continue
		}

		p.Kind = "openai"
		p.BaseURL = baseURL
		p.Thinking = ""
		p.WebSearch = nil
		p.SupportedEfforts = nil
		p.DefaultEffort = ""
		p.ModelOverrides = nil
	}
}

// mergeEnv overlays the per-server env map onto the spec's own env (overlay wins,
// matching v0.x mcpEnv precedence). Returns nil when both are empty.
func mergeEnv(base, overlay map[string]string) map[string]string {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(overlay))
	maps.Copy(out, base)
	maps.Copy(out, overlay)
	return out
}

// writeCredentialsEnv merges lines into Reasonix's global .env
// and pins them into the current process env so the just-built session resolves
// the key without a restart. Falls back to ~/.env only when Reasonix home can't
// be resolved — never a project .env, so a migration keeps secrets out of the
// user's project tree.
func writeCredentialsEnv(home string, lines []string) error {
	if _, err := StoreCredentialLines(lines); err != nil {
		if UserCredentialsPath() == "" && home != "" {
			return os.WriteFile(filepath.Join(home, ".env"), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
		}
		return err
	}
	return nil
}
