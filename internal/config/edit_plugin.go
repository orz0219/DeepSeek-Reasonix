package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/fileutil"
	fileencoding "reasonix/internal/fileutil/encoding"
	"reasonix/internal/mcpdiag"
)

// UpsertPlugin adds e, or replaces an MCP server with the same name (preserving
// position). The transport-specific required fields are validated: stdio needs
// a command, http/sse need a url.
func (c *Config) UpsertPlugin(e PluginEntry) error {
	e, _ = NormalizePluginCommandLine(e)
	if err := validatePlugin(e); err != nil {
		return err
	}
	for i := range c.Plugins {
		if c.Plugins[i].Name == e.Name {
			c.Plugins[i] = e
			return nil
		}
	}
	c.Plugins = append(c.Plugins, e)
	return nil
}

// RemovePlugin deletes the named MCP server, reporting whether it was present.
func (c *Config) RemovePlugin(name string) bool {
	for i := range c.Plugins {
		if c.Plugins[i].Name == name {
			c.Plugins = append(c.Plugins[:i], c.Plugins[i+1:]...)
			return true
		}
	}
	return false
}

// ClearPluginAuthentication removes locally stored auth-like material for one
// MCP server while keeping the server entry itself. It intentionally leaves
// non-auth config (command, URL host/path, ordinary env/header keys, tier) alone.
func (c *Config) ClearPluginAuthentication(name string) (PluginEntry, bool, error) {
	for i := range c.Plugins {
		if c.Plugins[i].Name != name {
			continue
		}
		headers, env, url, changed := mcpdiag.ClearAuthConfig(c.Plugins[i].Headers, c.Plugins[i].Env, c.Plugins[i].URL)
		c.Plugins[i].Headers = headers
		c.Plugins[i].Env = env
		c.Plugins[i].URL = url
		return c.Plugins[i], changed, nil
	}
	return PluginEntry{}, false, fmt.Errorf("clear plugin authentication: no plugin %q", name)
}

// ClearPluginAuthenticationInSource clears auth material in the file that actually
// owns the MCP server. Load() merges user/project TOML and project .mcp.json into
// one Config, so callers must not mutate that merged view and Save() it back: a
// .mcp.json-only server would otherwise be serialized into reasonix.toml or the
// user config. Source priority mirrors Load(): project TOML, user TOML, then the
// project .mcp.json entry if TOML did not define that server.
func ClearPluginAuthenticationInSource(name string) (PluginEntry, bool, string, error) {
	return ClearPluginAuthenticationInSourceForRoot(".", name)
}

// ClearPluginAuthenticationInSourceForRoot clears auth material in the source
// that owns name for the supplied workspace. The root is explicit so a desktop
// action cannot drift to another project's reasonix.toml or .mcp.json after the
// user switches tabs while the action is waiting on a lifecycle lock.
func ClearPluginAuthenticationInSourceForRoot(root, name string) (PluginEntry, bool, string, error) {
	resolvedRoot := resolveRoot(root)
	projectTOML := "reasonix.toml"
	projectMCPJSON := mcpJSONFile
	if resolvedRoot != "." {
		projectTOML = filepath.Join(resolvedRoot, "reasonix.toml")
		projectMCPJSON = filepath.Join(resolvedRoot, mcpJSONFile)
	}
	lockPaths := append([]string{}, userConfigCandidatePaths()...)
	lockPaths = append(lockPaths, projectTOML, projectMCPJSON)
	if legacy := legacyConfigPath(); strings.TrimSpace(legacy) != "" {
		lockPaths = append(lockPaths, legacy)
	}
	unlock, err := lockConfigFilesEdits(lockPaths...)
	if err != nil {
		return PluginEntry{}, false, "", fmt.Errorf("clear plugin authentication: %w", err)
	}
	defer unlock()

	cfg, err := LoadForRootReadOnly(root)
	if err != nil {
		return PluginEntry{}, false, "", err
	}
	entry, found := pluginEntryByName(cfg.Plugins, strings.TrimSpace(name))
	if !found {
		return PluginEntry{}, false, "", fmt.Errorf("clear plugin authentication: no plugin %q", name)
	}
	path := MCPConfigPathForEntry(root, entry)
	if entry.Source != MCPSourceProjectMCPJSON {
		cfg, err := LoadForEditReadOnlyStrict(path)
		if err != nil {
			return PluginEntry{}, false, path, err
		}
		updated, changed, err := cfg.ClearPluginAuthentication(name)
		if err != nil {
			return PluginEntry{}, false, path, err
		}
		if changed {
			if err := cfg.SaveTo(path); err != nil {
				return PluginEntry{}, false, path, err
			}
		}
		return updated, changed, path, nil
	}
	updated, changed, err := clearMCPJSONAuthentication(path, name)
	if err != nil {
		return PluginEntry{}, false, "", err
	}
	return updated, changed, path, nil
}

func pluginTOMLSourcePathForRoot(root, name string) string {
	projectTOML := "reasonix.toml"
	if resolved := resolveRoot(root); resolved != "." {
		projectTOML = filepath.Join(resolved, "reasonix.toml")
	}
	paths := append([]string{projectTOML}, userConfigCandidatePaths()...)
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		cfg := LoadForEdit(path)
		for _, p := range cfg.Plugins {
			if p.Name == name {
				return path
			}
		}
	}
	return ""
}

// MCPConfigPathForEntry returns the writable config file that owns entry.
// Runtime configuration is merged by name, so callers must use provenance
// instead of saving the merged Config back to whichever file happens to have
// the highest priority.
func MCPConfigPathForEntry(root string, entry PluginEntry) string {
	resolvedRoot := resolveRoot(root)
	projectTOML := "reasonix.toml"
	projectMCPJSON := mcpJSONFile
	if resolvedRoot != "." {
		projectTOML = filepath.Join(resolvedRoot, "reasonix.toml")
		projectMCPJSON = filepath.Join(resolvedRoot, mcpJSONFile)
	}
	switch entry.Source {
	case MCPSourceProjectConfig:
		return projectTOML
	case MCPSourceProjectMCPJSON:
		return projectMCPJSON
	case MCPSourceUserConfig:
		for _, path := range userConfigCandidatePaths() {
			cfg := LoadForEditWithoutCredentials(path)
			if _, ok := pluginEntryByName(cfg.Plugins, entry.Name); ok {
				return path
			}
		}
		return UserConfigPath()
	case MCPSourceLegacyUser:
		return legacyConfigPath()
	case MCPSourcePluginPackage:
		return ""
	}
	if path := pluginTOMLSourcePathForRoot(root, entry.Name); path != "" {
		return path
	}
	if _, found, err := LoadMCPJSONPlugin(projectMCPJSON, entry.Name); err == nil && found {
		return projectMCPJSON
	}
	return UserConfigPath()
}

// UpsertPluginInSourceForRoot writes entry back to its owning scope. New and
// legacy user entries are normalized into the current user-global config;
// project entries remain in their original project file.
func UpsertPluginInSourceForRoot(root string, entry PluginEntry) (string, error) {
	path := MCPConfigPathForEntry(root, entry)
	switch entry.Source {
	case MCPSourceProjectMCPJSON:
		unlock, err := LockConfigFileEdits(path)
		if err != nil {
			return path, err
		}
		defer unlock()
		if _, err := UpsertMCPJSONPlugin(path, entry); err != nil {
			return path, err
		}
		return path, nil
	case MCPSourcePluginPackage:
		return "", fmt.Errorf("MCP server %q is managed by an installed plugin package", entry.Name)
	case MCPSourceProjectConfig:

	default:
		path = UserConfigPath()
		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("cannot resolve user config path")
		}
		entry.Source = MCPSourceUserConfig
	}

	unlock, err := LockConfigFileEdits(path)
	if err != nil {
		return path, err
	}
	defer unlock()
	cfg, err := LoadForEditReadOnlyStrict(path)
	if err != nil {
		return path, err
	}
	if err := cfg.UpsertPlugin(entry); err != nil {
		return path, err
	}
	return path, cfg.SaveTo(path)
}

// InstallUserPluginForRoot persists an explicit global MCP install together
// with its durable activation state. All config sources that can shadow the
// global declaration stay locked through conflict detection, save, activation,
// and any rollback, so a failed activation write cannot remove or overwrite a
// concurrent config update.
func InstallUserPluginForRoot(root string, entry PluginEntry, forceEnable bool) (string, error) {
	entry.Source = MCPSourceUserConfig
	unlock, err := LockConfigFilesEdits(mcpConfigSourcePathsForRoot(root)...)
	if err != nil {
		return "", fmt.Errorf("install MCP server: %w", err)
	}
	defer unlock()

	effective, err := LoadForRootReadOnly(root)
	if err != nil {
		return "", err
	}
	for _, configured := range effective.Plugins {
		if configured.Name != entry.Name {
			continue
		}
		if configured.Source != MCPSourceUserConfig && configured.Source != MCPSourceLegacyUser {
			return "", fmt.Errorf(
				"MCP server %q is already configured by %s; edit or remove that declaration before installing a global server with the same name",
				entry.Name,
				configured.Source,
			)
		}
		break
	}

	path := UserConfigPath()
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("cannot resolve user config path")
	}
	cfg, err := LoadForEditReadOnlyStrict(path)
	if err != nil {
		return path, err
	}
	previous, hadPrevious := pluginEntryByName(cfg.Plugins, entry.Name)
	if err := cfg.UpsertPlugin(entry); err != nil {
		return path, err
	}
	if err := cfg.SaveTo(path); err != nil {
		return path, err
	}

	store := DefaultMCPActivationStore()
	var activationErr error
	if forceEnable {
		activationErr = store.SetServerEnabled(entry, root, true)
	} else {
		activationErr = store.ClearServer(entry, root)
	}
	if activationErr == nil {
		return path, nil
	}

	var restoreErr error
	if hadPrevious {
		restoreErr = cfg.UpsertPlugin(previous)
	} else {
		cfg.RemovePlugin(entry.Name)
	}
	if restoreErr == nil {
		restoreErr = cfg.SaveTo(path)
	}
	if restoreErr != nil {
		restoreErr = fmt.Errorf("restore MCP server config: %w", restoreErr)
	}
	return path, errors.Join(activationErr, restoreErr)
}

// RemovePluginFromSourceForRoot removes exactly the declaration represented by
// entry. Lower-priority same-name declarations are intentionally preserved so
// they can become effective after a project override is removed.
func RemovePluginFromSourceForRoot(root string, entry PluginEntry) (bool, string, error) {
	path := MCPConfigPathForEntry(root, entry)
	if entry.Source == MCPSourcePluginPackage {
		return false, "", fmt.Errorf("MCP server %q is managed by an installed plugin package", entry.Name)
	}
	if strings.TrimSpace(path) == "" {
		return false, "", nil
	}
	unlock, err := LockConfigFileEdits(path)
	if err != nil {
		return false, path, err
	}
	defer unlock()
	return removePluginFromSourceForRootLocked(entry, path)
}

// removePluginFromSourceForRootLocked removes exactly one source declaration
// while the caller holds that source's config edit lock.
func removePluginFromSourceForRootLocked(entry PluginEntry, path string) (bool, string, error) {
	switch entry.Source {
	case MCPSourceProjectMCPJSON:
		removed, err := RemoveMCPJSONPlugin(path, entry.Name)
		return removed, path, err
	case MCPSourcePluginPackage:
		return false, "", fmt.Errorf("MCP server %q is managed by an installed plugin package", entry.Name)
	case MCPSourceLegacyUser:
		edit, changed, err := planLegacyMCPDisable(path, entry.Name)
		if err != nil || !changed {
			return false, path, err
		}
		if err := applyConfigSourceEdits([]configSourceEdit{edit}); err != nil {
			return false, path, err
		}
		return true, path, nil
	}
	cfg, err := LoadForEditReadOnlyStrict(path)
	if err != nil {
		return false, path, err
	}
	if !cfg.RemovePlugin(entry.Name) {
		return false, path, nil
	}
	if err := cfg.SaveTo(path); err != nil {
		return false, path, err
	}
	return true, path, nil
}

// RemovePluginFromEffectiveSourceForRoot removes only the declaration currently
// selected by the project-over-global precedence rules.
func RemovePluginFromEffectiveSourceForRoot(root, name string) (PluginEntry, bool, string, error) {
	unlock, err := LockConfigFilesEdits(mcpConfigSourcePathsForRoot(root)...)
	if err != nil {
		return PluginEntry{}, false, "", fmt.Errorf("remove effective MCP server: %w", err)
	}
	defer unlock()

	cfg, err := LoadForRootReadOnly(root)
	if err != nil {
		return PluginEntry{}, false, "", err
	}
	entry, found := pluginEntryByName(cfg.Plugins, strings.TrimSpace(name))
	if !found {
		return PluginEntry{}, false, "", nil
	}
	path := MCPConfigPathForEntry(root, entry)
	removed, path, err := removePluginFromSourceForRootLocked(entry, path)
	return entry, removed, path, err
}

// mcpConfigSourcePathsForRoot returns every writable source whose precedence can
// decide which declaration is effective. A source-selection operation must lock
// all of them before loading, otherwise another process can add a higher-priority
// declaration after the load and turn a "remove effective" action into a removal
// of a now-shadowed source.
func mcpConfigSourcePathsForRoot(root string) []string {
	resolvedRoot := resolveRoot(root)
	projectTOML := "reasonix.toml"
	projectMCPJSON := mcpJSONFile
	if resolvedRoot != "." {
		projectTOML = filepath.Join(resolvedRoot, "reasonix.toml")
		projectMCPJSON = filepath.Join(resolvedRoot, mcpJSONFile)
	}
	paths := append([]string{}, userConfigCandidatePaths()...)
	paths = append(paths, projectTOML, projectMCPJSON)
	if legacy := legacyConfigPath(); strings.TrimSpace(legacy) != "" {
		paths = append(paths, legacy)
	}
	return paths
}

type configSourceEdit struct {
	path         string
	resolvedPath string
	before       []byte
	perm         os.FileMode
	write        func() error
}

func newConfigSourceEdit(path string, write func() error) (configSourceEdit, error) {
	userOwned := isUserConfigPath(path) || samePath(path, legacyConfigPath())
	resolved, err := resolveConfigAccessPath(path, userOwned)
	if err != nil {
		return configSourceEdit{}, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return configSourceEdit{}, err
	}
	before, err := os.ReadFile(resolved)
	if err != nil {
		return configSourceEdit{}, err
	}
	return configSourceEdit{
		path:         path,
		resolvedPath: resolved,
		before:       before,
		perm:         info.Mode().Perm(),
		write:        write,
	}, nil
}

func applyConfigSourceEdits(edits []configSourceEdit) error {
	for i := range edits {
		if err := edits[i].write(); err != nil {
			var rollbackErrs []error
			for j := i; j >= 0; j-- {
				if rollbackErr := fileutil.AtomicWriteFile(edits[j].resolvedPath, edits[j].before, edits[j].perm); rollbackErr != nil {
					rollbackErrs = append(rollbackErrs, fmt.Errorf("restore %s: %w", edits[j].path, rollbackErr))
				}
			}
			if rollbackErr := errors.Join(rollbackErrs...); rollbackErr != nil {
				return errors.Join(err, fmt.Errorf("roll back MCP config removal: %w", rollbackErr))
			}
			return err
		}
	}
	return nil
}

func planTOMLPluginRemoval(path, name string) (configSourceEdit, bool, error) {
	_, exists, err := statConfigPath(path)
	if err != nil {
		return configSourceEdit{}, false, err
	}
	if !exists {
		return configSourceEdit{}, false, nil
	}
	cfg := Default()
	if err := mergeFile(cfg, path); err != nil {
		return configSourceEdit{}, false, err
	}
	normalizeConfigForEdit(cfg)
	if !cfg.RemovePlugin(name) {
		return configSourceEdit{}, false, nil
	}
	edit, err := newConfigSourceEdit(path, func() error { return cfg.SaveTo(path) })
	return edit, err == nil, err
}

func planMCPJSONPluginRemoval(path, name string) (configSourceEdit, bool, error) {
	resolved, exists, err := statConfigPath(path)
	if err != nil {
		return configSourceEdit{}, false, err
	}
	if !exists {
		return configSourceEdit{}, false, nil
	}
	root, servers, err := readMCPJSONRaw(resolved)
	if err != nil {
		return configSourceEdit{}, false, err
	}
	if _, ok := servers[name]; !ok {
		return configSourceEdit{}, false, nil
	}
	delete(servers, name)
	edit, err := newConfigSourceEdit(path, func() error { return writeMCPJSONServers(resolved, root, servers) })
	return edit, err == nil, err
}

func planLegacyMCPDisable(path, name string) (configSourceEdit, bool, error) {
	if strings.TrimSpace(path) == "" {
		return configSourceEdit{}, false, nil
	}
	resolved, err := resolveConfigAccessPath(path, true)
	if err != nil {
		return configSourceEdit{}, false, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			return configSourceEdit{}, false, nil
		}
		return configSourceEdit{}, false, err
	}
	data, err := fileencoding.ReadFileUTF8(resolved)
	if err != nil {
		return configSourceEdit{}, false, err
	}
	var root map[string]json.RawMessage
	var view struct {
		MCP         []string                   `json:"mcp"`
		MCPServers  map[string]json.RawMessage `json:"mcpServers"`
		MCPDisabled []string                   `json:"mcpDisabled"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return configSourceEdit{}, false, nil
	}
	if err := json.Unmarshal(data, &view); err != nil {
		return configSourceEdit{}, false, nil
	}

	foundNamed := false
	changed := false
	filtered := make([]string, 0, len(view.MCP))
	for i, raw := range view.MCP {
		entry, ok := parseLegacyMCPSpec(raw)
		if !ok {
			filtered = append(filtered, raw)
			continue
		}
		effectiveName := entry.Name
		if effectiveName == "" {
			effectiveName = anonymousMCPName(i)
		}
		if effectiveName != name {
			filtered = append(filtered, raw)
			continue
		}
		if entry.Name == "" {
			changed = true
			continue
		}
		foundNamed = true
		filtered = append(filtered, raw)
	}
	if _, ok := view.MCPServers[name]; ok {
		foundNamed = true
	}
	if foundNamed && !containsString(view.MCPDisabled, name) {
		view.MCPDisabled = append(view.MCPDisabled, name)
		changed = true
	}
	if !changed {
		return configSourceEdit{}, false, nil
	}
	if len(filtered) != len(view.MCP) {
		raw, marshalErr := json.Marshal(filtered)
		if marshalErr != nil {
			return configSourceEdit{}, false, marshalErr
		}
		root["mcp"] = raw
	}
	disabledRaw, err := json.Marshal(view.MCPDisabled)
	if err != nil {
		return configSourceEdit{}, false, err
	}
	root["mcpDisabled"] = disabledRaw
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return configSourceEdit{}, false, err
	}
	out = append(out, '\n')
	edit, err := newConfigSourceEdit(path, func() error {
		return fileutil.AtomicWriteFile(resolved, out, info.Mode().Perm())
	})
	return edit, err == nil, err
}

// RemovePluginFromSourcesForRoot removes an MCP server from every writable
// config source that can contribute it for root. Removing all matching TOML
// declarations prevents a lower-priority duplicate from reappearing after the
// higher-priority entry is deleted. Every edit is planned before the first write,
// and legacy JSON receives a disable marker for older Reasonix versions.
func RemovePluginFromSourcesForRoot(root, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, fmt.Errorf("remove MCP server: name is required")
	}

	userPaths := userConfigCandidatePaths()
	resolvedRoot := resolveRoot(root)
	projectTOML := "reasonix.toml"
	if resolvedRoot != "." {
		projectTOML = filepath.Join(resolvedRoot, "reasonix.toml")
	}
	isUserPath := false
	for _, path := range userPaths {
		if samePath(path, projectTOML) {
			isUserPath = true
			break
		}
	}
	mcpPath := mcpJSONFile
	if resolvedRoot != "." {
		mcpPath = filepath.Join(resolvedRoot, mcpJSONFile)
	}
	legacyPath := legacyConfigPath()
	lockPaths := append([]string{}, userPaths...)
	if !isUserPath {
		lockPaths = append(lockPaths, projectTOML)
	}
	lockPaths = append(lockPaths, mcpPath)
	if legacyPath != "" {
		lockPaths = append(lockPaths, legacyPath)
	}
	unlock, err := lockConfigFilesEdits(lockPaths...)
	if err != nil {
		return false, fmt.Errorf("remove MCP server: %w", err)
	}
	defer unlock()

	var edits []configSourceEdit
	planTOML := func(path string) error {
		edit, changed, err := planTOMLPluginRemoval(path, name)
		if err != nil {
			return err
		}
		if changed {
			edits = append(edits, edit)
		}
		return nil
	}
	for _, path := range userPaths {
		if err := planTOML(path); err != nil {
			return false, err
		}
	}
	if !isUserPath {
		if err := planTOML(projectTOML); err != nil {
			return false, err
		}
	}

	mcpEdit, changed, err := planMCPJSONPluginRemoval(mcpPath, name)
	if err != nil {
		return false, err
	}
	if changed {
		edits = append(edits, mcpEdit)
	}
	legacyEdit, changed, err := planLegacyMCPDisable(legacyPath, name)
	if err != nil {
		return false, err
	}
	if changed {
		edits = append(edits, legacyEdit)
	}
	if len(edits) == 0 {
		return false, nil
	}
	if err := applyConfigSourceEdits(edits); err != nil {
		return false, err
	}
	return true, nil
}

// validatePlugin checks a plugin entry by transport. An empty Type means stdio.
func validatePlugin(e PluginEntry) error {
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("plugin: name is required")
	}
	if e.StartupTimeoutSeconds < 0 {
		return fmt.Errorf("plugin %q: startup_timeout_seconds must be >= 0", e.Name)
	}
	if e.CallTimeoutSeconds < 0 {
		return fmt.Errorf("plugin %q: call_timeout_seconds must be >= 0", e.Name)
	}
	for name, sec := range e.ToolTimeoutSeconds {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("plugin %q: tool_timeout_seconds contains an empty tool name", e.Name)
		}
		if sec < 0 {
			return fmt.Errorf("plugin %q: tool_timeout_seconds[%q] must be >= 0", e.Name, name)
		}
	}
	switch strings.ToLower(strings.TrimSpace(e.Type)) {
	case "", "stdio":
		if strings.TrimSpace(e.Command) == "" {
			return fmt.Errorf("plugin %q: command is required for a stdio server", e.Name)
		}
	case "http", "sse", "streamable-http":
		if strings.TrimSpace(e.URL) == "" {
			return fmt.Errorf("plugin %q: url is required for a %s server", e.Name, e.Type)
		}
	default:
		return fmt.Errorf("plugin %q: unknown type %q (want stdio|http|sse)", e.Name, e.Type)
	}
	return nil
}
