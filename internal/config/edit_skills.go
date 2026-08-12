package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// AddSkillPath appends a custom skill root, deduping by its expanded absolute
// path while preserving the caller's original spelling in the config file.
func (c *Config) AddSkillPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("skill path: empty path")
	}
	want := CanonicalSkillPath(path)
	c.removeExcludedSkillPath(want)
	for _, existing := range c.Skills.Paths {
		if CanonicalSkillPath(existing) == want {
			return nil
		}
	}
	c.Skills.Paths = append(c.Skills.Paths, path)
	return nil
}

// RemoveSkillPath removes the first custom skill root matching path after
// expansion and path cleaning. It reports whether anything changed.
func (c *Config) RemoveSkillPath(path string) (bool, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return false, fmt.Errorf("skill path: empty path")
	}
	want := CanonicalSkillPath(path)
	for i, existing := range c.Skills.Paths {
		if CanonicalSkillPath(existing) == want {
			c.Skills.Paths = append(c.Skills.Paths[:i], c.Skills.Paths[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

// RestoreSkillPath removes a pseudo-deleted skill source from excluded_paths.
func (c *Config) RestoreSkillPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("skill path: empty path")
	}
	want := CanonicalSkillPath(path)
	if want == "" {
		return fmt.Errorf("skill path: empty path")
	}
	c.removeExcludedSkillPath(want)
	return nil
}

// ExcludeSkillPath hides any skill discovery root matching path. This is used by
// UI "remove source" actions for convention roots that are not stored in paths.
func (c *Config) ExcludeSkillPath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("skill path: empty path")
	}
	want := CanonicalSkillPath(path)
	if want == "" {
		return fmt.Errorf("skill path: empty path")
	}
	for _, existing := range c.Skills.ExcludedPaths {
		if CanonicalSkillPath(existing) == want {
			return nil
		}
	}
	c.Skills.ExcludedPaths = append(c.Skills.ExcludedPaths, path)
	return nil
}

// SetSkillPathEnabled enables or disables a skill discovery root without
// deleting its configured path. Disabled roots are recorded in excluded_paths
// and can be restored without asking the user to browse for the folder again.
func (c *Config) SetSkillPathEnabled(path string, enabled bool) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("skill path: empty path")
	}
	want := CanonicalSkillPath(path)
	if want == "" {
		return fmt.Errorf("skill path: empty path")
	}
	if enabled {
		c.removeExcludedSkillPath(want)
		return nil
	}
	return c.ExcludeSkillPath(path)
}

func (c *Config) removeExcludedSkillPath(want string) {
	next := c.Skills.ExcludedPaths[:0]
	for _, existing := range c.Skills.ExcludedPaths {
		if CanonicalSkillPath(existing) != want {
			next = append(next, existing)
		}
	}
	c.Skills.ExcludedPaths = next
}

// SetSkillEnabled persists a per-skill enable/disable preference. Skills are
// enabled by default; disabling records the name, enabling removes it.
func (c *Config) SetSkillEnabled(name string, enabled bool) error {
	name = strings.TrimSpace(name)
	key := SkillNameKey(name)
	if key == "" {
		return fmt.Errorf("skill name %q: use letters, digits, '_', '-', '.', 1-64 chars, starting alphanumeric", name)
	}
	next := c.DisabledSkillNames()
	idx := -1
	for i, existing := range next {
		if SkillNameKey(existing) == key {
			idx = i
			break
		}
	}
	if enabled {
		if idx >= 0 {
			next = append(next[:idx], next[idx+1:]...)
		}
		c.Skills.DisabledSkills = next
		return nil
	}
	if idx < 0 {
		next = append(next, name)
	}
	c.Skills.DisabledSkills = next
	return nil
}

// SetSkillImplicitInvocation controls whether skills are exposed to the model
// for automatic discovery and invocation. Explicit /skill commands remain
// available regardless of this setting.
func (c *Config) SetSkillImplicitInvocation(enabled bool) {
	c.Skills.DisableImplicitInvocation = !enabled
}

// CanonicalSkillPath expands env vars, ~ and relative segments to an absolute
// cleaned path for comparing skill roots. On Windows it folds case so paths that
// differ only in casing dedupe. Use only for comparison, never as stored config.
func CanonicalSkillPath(path string) string {
	path = ExpandVars(strings.TrimSpace(path))
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	} else if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			path = home
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}
