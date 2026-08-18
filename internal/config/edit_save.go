package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
)

// SaveTo writes the configuration to path as annotated TOML, atomically: it
// writes a sibling temp file then renames, so a crash mid-write can't leave a
// half-written reasonix.toml that fails to parse on next load. Parent directories
// are created as needed.
//
// For project configs (./reasonix.toml) the write is incremental: only sections
// and fields that differ from built-in defaults are written, so the file never
// accumulates fields that override the user's global config. User configs still
// write the full annotated template since they are the user's own settings store.
func (c *Config) SaveTo(path string) error {
	if c == nil {
		return fmt.Errorf("save config: nil config")
	}
	if c.editLoadErr != nil {
		return fmt.Errorf("save config loaded from %q: %w", path, c.editLoadErr)
	}
	scope := renderScopeForPath(path)
	if scope == RenderScopeUser {
		if err := currentUserConfigEditLockError(); err != nil {
			return fmt.Errorf("save user config: %w", err)
		}
	}
	resolved, err := resolveConfigAccessPath(path, scope == RenderScopeUser)
	if err != nil {
		return err
	}
	if scope == RenderScopeProject {
		return c.saveProjectIncrementalResolved(path, resolved)
	}
	return writeConfigFileResolved(resolved, RenderTOMLForScope(c, scope), configFilePerm(path))
}

func (c *Config) SaveToScope(path string, scope RenderScope) error {
	if c == nil {
		return fmt.Errorf("save config: nil config")
	}
	if c.editLoadErr != nil {
		return fmt.Errorf("save config loaded from %q: %w", path, c.editLoadErr)
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("save: empty config path")
	}
	userConfig := scope == RenderScopeUser || (scope == RenderScopeFull && isUserConfigPath(path))
	if userConfig {
		if err := currentUserConfigEditLockError(); err != nil {
			return fmt.Errorf("save user config: %w", err)
		}
	}
	resolved, err := resolveConfigAccessPath(path, userConfig)
	if err != nil {
		return err
	}
	return writeConfigFileResolved(resolved, RenderTOMLForScope(c, scope), configFilePerm(path))
}

func (c *Config) saveProjectIncrementalResolved(logicalPath, resolvedPath string) error {
	raw, err := fileencoding.ReadFileUTF8(resolvedPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		raw = nil
	}

	body := string(raw)
	isNew := body == ""

	if isNew {
		return writeConfigFileResolved(resolvedPath, RenderTOMLForScope(c, RenderScopeProject), configFilePerm(logicalPath))
	}

	delta := RenderTOMLProjectDelta(c)
	if tomlBodyHasTopLevelKey(body, "config_version") && !tomlBodyHasTopLevelKey(delta, "config_version") {
		delta = fmt.Sprintf("config_version = %d\n", configVersion(c)) + delta
	}
	removePlugins := len(tomlPluginsForScope(c.Plugins, RenderScopeProject)) == 0 && tomlBodyHasSection(body, "plugins")
	removeSandboxBash := shouldRemoveIneffectiveProjectSandboxBash(body, c)
	removeSkills := projectSkillsKeysToRemove(body, c)
	_, hasLegacyDesktopAutoGuard := tomlSectionKeyValue(body, "desktop", "default_auto_recovery_checkpoint")
	_, hasRetiredAgentAutoGuard := tomlSectionKeyValue(body, "agent", "auto_recovery_checkpoint")
	removeRetiredAutoGuard := hasLegacyDesktopAutoGuard || hasRetiredAgentAutoGuard
	writeProviderAccess := c.Desktop.ProviderAccess != nil
	writeProviderDisabled := len(c.Desktop.ProviderDisabled) > 0
	if strings.TrimSpace(delta) == "" && !removePlugins && !removeSandboxBash && !removeSkills && !removeRetiredAutoGuard && !writeProviderAccess && !writeProviderDisabled {
		return nil
	}

	if strings.TrimSpace(delta) != "" {
		body = mergeTOMLDelta(body, delta)
	}
	if removePlugins {
		body = removeTOMLSection(body, "plugins")
	}
	if removeSandboxBash {
		body = removeTOMLSectionKey(body, "sandbox", "bash")
	}
	if removeSkills {
		body = cleanupProjectSkillsKeys(body, c)
	}
	if removeRetiredAutoGuard {
		body = removeTOMLSectionKey(body, "desktop", "default_auto_recovery_checkpoint")
		body = removeTOMLSectionKey(body, "agent", "auto_recovery_checkpoint")
	}
	if writeProviderAccess {
		body = upsertTOMLSectionKey(body, "desktop", "provider_access", "provider_access = "+renderStringArray(c.Desktop.ProviderAccess))
	}
	if writeProviderDisabled {
		body = upsertTOMLSectionKey(body, "desktop", "provider_disabled", "provider_disabled = "+renderStringArray(c.Desktop.ProviderDisabled))
	}
	return writeConfigFileResolved(resolvedPath, body, configFilePerm(logicalPath))
}

// projectSkillsKeysToRemove reports whether an existing project [skills]
// section contains a field whose current edit value is the built-in default.
// Project saves are incremental, so an empty RenderTOMLProjectDelta cannot
// remove a stale override without this explicit cleanup pass.
func projectSkillsKeysToRemove(body string, c *Config) bool {
	if c == nil || !tomlBodyHasSection(body, "skills") {
		return false
	}
	for _, key := range projectSkillKeys {
		if projectSkillKeyIsDefault(c, key) {
			if _, ok := tomlSectionKeyValue(body, "skills", key); ok {
				return true
			}
		}
	}
	return false
}

var projectSkillKeys = [...]string{"paths", "excluded_paths", "disabled_skills", "disable_implicit_invocation", "max_depth"}

func projectSkillKeyIsDefault(c *Config, key string) bool {
	if c != nil && c.keepsProjectSkillKey(key) {
		return false
	}
	switch key {
	case "paths":
		return len(c.Skills.Paths) == 0
	case "excluded_paths":
		return len(c.Skills.ExcludedPaths) == 0
	case "disabled_skills":
		return len(c.Skills.DisabledSkills) == 0
	case "disable_implicit_invocation":
		return !c.Skills.DisableImplicitInvocation
	case "max_depth":
		return c.Skills.MaxDepth == 0
	default:
		return false
	}
}

func cleanupProjectSkillsKeys(body string, c *Config) string {
	if c == nil {
		return body
	}
	for _, key := range projectSkillKeys {
		if projectSkillKeyIsDefault(c, key) {
			body = removeTOMLSectionKey(body, "skills", key)
		}
	}
	return body
}

func shouldRemoveIneffectiveProjectSandboxBash(body string, c *Config) bool {
	if c == nil || runtimeGOOS != "windows" {
		return false
	}
	if c.BashMode() != "off" {
		return false
	}
	value, ok := tomlSectionKeyValue(body, "sandbox", "bash")
	return ok && tomlStringLiteralEquals(value, "enforce")
}

// mergeTOMLDelta parses delta into named TOML blocks and merges each into body
// via replaceTOMLSection. Consecutive array-of-tables entries ([[plugins]],
// [[providers]]) with the same name are merged into a single block so the
// replacement doesn't lose entries.
func mergeTOMLDelta(body, delta string) string {
	lines := strings.Split(delta, "\n")
	type section struct {
		name    string
		content string
		isArray bool
	}
	var topLevel strings.Builder
	var sections []section
	var curName string
	var curBuf strings.Builder
	curIsArray := false

	flush := func() {
		if curName == "" {
			return
		}
		content := curBuf.String()
		if curIsArray && len(sections) > 0 && sections[len(sections)-1].isArray && sections[len(sections)-1].name == curName {
			sections[len(sections)-1].content += content
		} else {
			sections = append(sections, section{curName, content, curIsArray})
		}
		curBuf.Reset()
	}

	for _, line := range lines {
		if name, isArray, ok := tomlEditSectionHeader(line); ok {
			flush()
			curName = name
			curIsArray = isArray
			curBuf.WriteString(line + "\n")
			continue
		}
		if curName != "" {
			curBuf.WriteString(line + "\n")
			continue
		}
		if strings.TrimSpace(line) != "" {
			topLevel.WriteString(line + "\n")
		}
	}
	flush()

	if top := strings.TrimSpace(topLevel.String()); top != "" {
		body = mergeTOMLTopLevelFields(body, top+"\n")
	}
	for _, s := range sections {
		body = replaceTOMLSection(body, s.name, s.content)
	}
	return body
}

func mergeTOMLTopLevelFields(body, fields string) string {
	for line := range strings.SplitSeq(fields, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, ok := tomlTopLevelKey(line)
		if !ok {
			continue
		}
		body = replaceTOMLTopLevelField(body, key, line+"\n")
	}
	return body
}

// SaveMinimalProjectReasoningLanguage writes a new project config that only
// overrides [agent].reasoning_language.
func SaveMinimalProjectReasoningLanguage(path, lang string) (string, error) {
	cfg := Default()
	if err := cfg.SetReasoningLanguage(lang); err != nil {
		return "", err
	}
	body := fmt.Sprintf(`# Reasonix project configuration.
# Project-local overrides are merged over the user config.

[agent]
reasoning_language = %q
`, cfg.ReasoningLanguage())
	return cfg.ReasoningLanguage(), writeConfigFile(path, body)
}

// SaveMinimalProjectCompactRatio writes a new project config that only
// overrides [agent].compact_ratio.
func SaveMinimalProjectCompactRatio(path string, ratio float64) (float64, error) {
	cfg := Default()
	if err := cfg.SetCompactRatio(ratio); err != nil {
		return 0, err
	}
	body := fmt.Sprintf(`# Reasonix project configuration.
# Project-local overrides are merged over the user config.

[agent]
compact_ratio = %s
`, formatFloat(cfg.Agent.CompactRatio))
	return cfg.Agent.CompactRatio, writeConfigFile(path, body)
}

func writeConfigFile(path, body string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("save: empty config path")
	}
	return atomicWriteToConfigFile(path, body, configFilePerm(path))
}

func writeConfigFileResolved(path, body string, perm os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("save: empty config path")
	}
	return fileutil.AtomicWriteFile(path, []byte(body), perm)
}

// atomicWriteToConfigFile resolves the path once and writes only the validated
// final target. This preserves valid links and fails closed for broken user
// links or project links that escape their project root.
func atomicWriteToConfigFile(path, body string, perm os.FileMode) error {
	resolved, err := resolveConfigReadPath(path)
	if err != nil {
		return err
	}
	if err := fileutil.AtomicWriteFile(resolved, []byte(body), perm); err != nil {
		return fmt.Errorf("write symlink target %q: %w", resolved, err)
	}
	return nil
}

func configFilePerm(path string) os.FileMode {
	if isUserConfigPath(path) {
		return 0o600
	}
	return 0o644
}

// WritePermissionsAllow updates only permissions.allow in a TOML file. All
// other permission policy fields and unrelated content remain byte-for-byte
// unchanged. Callers must validate and lock the latest file across their full
// read-modify-write transaction before calling this function.
func WritePermissionsAllow(path string, allow []string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("write permissions: empty config path")
	}

	resolved, exists, err := statConfigPath(path)
	if err != nil {
		return err
	}
	var raw []byte
	if exists {
		raw, err = fileencoding.ReadFileUTF8(resolved)
		if err != nil {
			return err
		}
	} else {
		raw = nil
	}

	body := string(raw)
	if body == "" {
		body = fmt.Sprintf("[permissions]\nallow = %s\n", renderStringArray(allow))
	} else {
		body = upsertTOMLSectionKey(body, "permissions", "allow", "allow = "+renderStringArray(allow))
	}

	var candidate Config
	if _, err := toml.Decode(body, &candidate); err != nil {
		return fmt.Errorf("write permissions: validate updated config: %w", err)
	}
	if !slices.Equal(candidate.Permissions.Allow, allow) {
		return fmt.Errorf("write permissions: validate updated allow: got %v, want %v", candidate.Permissions.Allow, allow)
	}
	return writeConfigFileResolved(resolved, body, configFilePerm(path))
}

func renderScopeForPath(path string) RenderScope {
	if isUserConfigPath(path) {
		return RenderScopeUser
	}
	return RenderScopeProject
}

func isUserConfigPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	for _, uc := range userConfigCandidatePaths() {
		uc = strings.TrimSpace(uc)
		if uc == "" {
			continue
		}
		pathAbs, pathErr := filepath.Abs(path)
		ucAbs, ucErr := filepath.Abs(uc)
		if pathErr == nil && ucErr == nil {
			if filepath.Clean(pathAbs) == filepath.Clean(ucAbs) {
				return true
			}
			continue
		}
		if filepath.Clean(path) == filepath.Clean(uc) {
			return true
		}
	}
	return false
}

// IsUserConfigPath reports whether path is one of Reasonix's current or legacy
// user-global config locations. Other paths use project-scoped rendering.
func IsUserConfigPath(path string) bool {
	return isUserConfigPath(path)
}

// Save writes the configuration back to the file it was loaded from
// (SourcePath), or to ./reasonix.toml when none exists yet — the conventional
// project-local target a fresh GUI session would create.
func (c *Config) Save() error {
	path := SourcePath()
	if path == "" {
		path = "reasonix.toml"
	}
	return c.SaveTo(path)
}

// SaveForRoot saves root's project config when it exists, falling back to the
// user's global config when root has no reasonix.toml. Existing project files
// are edited from their own TOML only, never from a runtime user+project merge.
func (c *Config) SaveForRoot(root string) error {
	root = resolveRoot(root)
	projectTOML := "reasonix.toml"
	if root != "." {
		projectTOML = filepath.Join(root, "reasonix.toml")
	}
	if _, err := os.Stat(projectTOML); err == nil {
		projectCfg := LoadForEditWithoutCredentials(projectTOML)
		return projectCfg.SaveTo(projectTOML)
	}
	if uc := userConfigPath(); uc != "" {
		if err := os.MkdirAll(filepath.Dir(uc), 0o755); err != nil {
			return err
		}
		return c.SaveTo(uc)
	}
	return c.SaveTo(projectTOML)
}
