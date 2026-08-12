package config

import (
	"fmt"
	"regexp"
	"runtime"
	"strings"
)

var validSkillName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// IsValidSkillName reports whether name is a usable skill identifier.
func IsValidSkillName(name string) bool { return validSkillName.MatchString(name) }

// SkillNameKey normalizes a skill identifier for config comparisons.
func SkillNameKey(name string) string {
	name = strings.TrimSpace(name)
	if !IsValidSkillName(name) {
		return ""
	}
	if runtime.GOOS == "windows" {
		return strings.ToLower(name)
	}
	return name
}

// KeepProjectSkillKey marks a skill field as an intentional project override.
// An explicit empty/false project value must still be written so it can
// override a non-default user setting in the layered configuration.
func (c *Config) KeepProjectSkillKey(key string) error {
	key = strings.TrimSpace(key)
	switch key {
	case "paths", "excluded_paths", "disabled_skills", "disable_implicit_invocation", "max_depth":
	default:
		return fmt.Errorf("unknown project skill key %q", key)
	}
	if c.explicitProjectSkillKeys == nil {
		c.explicitProjectSkillKeys = make(map[string]bool)
	}
	c.explicitProjectSkillKeys[key] = true
	return nil
}

func (c *Config) keepsProjectSkillKey(key string) bool {
	return c != nil && c.explicitProjectSkillKeys[key]
}
