package config

import (
	"strings"
)

// CLITelemetryConfigured reports whether the user has made an explicit CLI
// telemetry choice. The runtime policy still treats an absent value as auto,
// but persistence must preserve absence until the first eligible consent prompt.
func (c *Config) CLITelemetryConfigured() bool {
	if c == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(c.Telemetry.CLIMetrics)) {
	case "auto", "on", "off":
		return true
	default:
		return false
	}
}

// CLITelemetryMode returns the normalized CLI telemetry policy.
func (c *Config) CLITelemetryMode() string {
	if c == nil {
		return "auto"
	}
	switch strings.ToLower(strings.TrimSpace(c.Telemetry.CLIMetrics)) {
	case "on":
		return "on"
	case "off":
		return "off"
	default:
		return "auto"
	}
}

// LoadWarnings returns non-fatal config load issues (corrupt files recovered in
// memory). The returned slice is a copy.
func (c *Config) LoadWarnings() []string {
	if c == nil || len(c.loadWarnings) == 0 {
		return nil
	}
	out := make([]string, len(c.loadWarnings))
	copy(out, c.loadWarnings)
	return out
}

// HasLoadWarnings reports whether the load used a degraded in-memory fallback.
func (c *Config) HasLoadWarnings() bool {
	return c != nil && len(c.loadWarnings) > 0
}

func (c *Config) addLoadWarning(msg string) {
	if c == nil {
		return
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	c.loadWarnings = append(c.loadWarnings, msg)
}

// IgnoredLegacyAgentStepLimits reports whether this load found and ignored the
// retired [agent].max_steps or planner_max_steps settings. Boot removes standard
// key assignments before loading, while read-only/config-only loads only report
// and normalize them in memory.
func (c *Config) IgnoredLegacyAgentStepLimits() bool {
	return c != nil && c.ignoredLegacyStepLimits
}

// IgnoredProjectDefaultModel returns the project reasonix.toml default_model
// that LoadForRoot ignored because no configured provider serves it (see
// restoreUnresolvableProjectDefaultModel), or "" when none was ignored.
func (c *Config) IgnoredProjectDefaultModel() string {
	if c == nil {
		return ""
	}
	return c.ignoredProjectDefaultModel
}
