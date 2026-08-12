package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	fileencoding "reasonix/internal/fileutil/encoding"
)

// MCPGlobalMigrationResult summarizes the v1.9.1 MCP backfill that lifts MCP
// servers from legacy and project-local sources into the user-global config.
type MCPGlobalMigrationResult struct {
	To      string
	Added   int
	Sources int
}

// MigrateMCPToUserConfigOnUpgrade runs a one-time best-effort backfill for the
// v1.9.1 desktop/CLI upgrade: MCP servers found in legacy TOML, known project
// roots, and legacy v0.x JSON are copied into the user-global config so the MCP
// settings page is stable across Global/project tabs. Existing global entries win
// on name collisions, and source files are left untouched.
func MigrateMCPToUserConfigOnUpgrade(projectRoots []string) (*MCPGlobalMigrationResult, error) {
	dest := userConfigPath()
	if dest == "" {
		return nil, nil
	}
	unlock, err := LockConfigFileEdits(dest)
	if err != nil {
		return nil, err
	}
	defer unlock()

	marker := mcpGlobalMigrationMarkerPath()
	if marker == "" {
		return nil, nil
	}
	if _, err := os.Stat(marker); err == nil {
		return nil, nil
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	res, err := migrateMCPToUserConfig(projectRoots)
	if err != nil {
		return res, err
	}
	if res == nil {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(marker, []byte("v1\n"), 0o644); err != nil {
		return res, err
	}
	return res, nil
}

func migrateMCPToUserConfig(projectRoots []string) (*MCPGlobalMigrationResult, error) {
	dest := userConfigPath()
	if dest == "" {
		return nil, nil
	}
	userCfg, err := loadForEditStrict(dest, true, true)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(userCfg.Plugins))
	for _, p := range userCfg.Plugins {
		if name := strings.TrimSpace(p.Name); name != "" {
			have[name] = true
		}
	}

	result := &MCPGlobalMigrationResult{To: dest}
	addEntries := func(entries []PluginEntry) {
		if len(entries) == 0 {
			return
		}
		result.Sources++
		for _, entry := range entries {
			entry, _ = NormalizePluginCommandLine(entry)
			name := strings.TrimSpace(entry.Name)
			if name == "" || have[name] || validatePlugin(entry) != nil {
				continue
			}
			entry.Source = MCPSourceUserConfig
			userCfg.Plugins = append(userCfg.Plugins, entry)
			have[name] = true
			result.Added++
		}
	}

	home, _ := os.UserHomeDir()
	for _, path := range mcpMigrationLegacyTOMLPaths(dest, home) {
		addEntries(loadPluginEntriesFromTOML(path))
	}
	for _, root := range normalizedMCPMigrationRoots(projectRoots) {
		addEntries(loadPluginEntriesFromTOML(filepath.Join(root, "reasonix.toml")))
		if entries, err := loadMCPJSON(filepath.Join(root, mcpJSONFile)); err == nil {
			addEntries(entries)
		}
	}
	addEntries(loadLegacyConfigPlugins(legacyConfigPath()))

	if result.Sources == 0 {
		return nil, nil
	}
	if result.Added > 0 {
		if err := userCfg.SaveTo(dest); err != nil {
			return result, err
		}
	}
	return result, nil
}

func mcpGlobalMigrationMarkerPath() string {
	dir := userSupportDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "mcp-global-migration-v1")
}

func mcpGlobalMigrationComplete() bool {
	marker := mcpGlobalMigrationMarkerPath()
	if marker == "" {
		return false
	}
	_, err := os.Stat(marker)
	return err == nil
}

func mcpMigrationLegacyTOMLPaths(dest, home string) []string {
	var paths []string
	for _, path := range legacyTOMLPaths(dest, home) {
		if path == "" || samePath(path, dest) {
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

func loadPluginEntriesFromTOML(path string) []PluginEntry {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	var cfg Config
	if _, err := decodeTOMLFile(path, &cfg); err != nil {
		return nil
	}
	out := make([]PluginEntry, 0, len(cfg.Plugins))
	for _, p := range cfg.Plugins {
		p, _ = NormalizePluginCommandLine(p)
		out = append(out, p)
	}
	return out
}

func loadLegacyConfigPlugins(path string) []PluginEntry {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := fileencoding.ReadFileUTF8(path)
	if err != nil {
		return nil
	}
	var legacy legacyConfig
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil
	}
	return legacyPlugins(legacy)
}

func normalizedMCPMigrationRoots(roots []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		root = filepath.Clean(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, root)
	}
	return out
}
