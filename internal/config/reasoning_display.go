package config

import (
	"fmt"
	"strings"
)

// DesktopReasoningDisplayMode normalizes the desktop-only reasoning fold mode.
func (c *Config) DesktopReasoningDisplayMode() string {
	raw := strings.ToLower(strings.TrimSpace(c.Desktop.ReasoningDisplayMode))
	switch raw {
	case "open", "half", "closed":
		return raw
	case "auto":
		// Legacy value: thinking streams expanded and collapses on completion,
		// which is exactly the closed fold behavior.
		return "closed"
	case "hidden", "summary":
		// Legacy modes (fully hidden, always-summary) were dropped in favor of
		// the three-state fold behavior; closed keeps thinking at least glanceable.
		return "closed"
	}
	// Missing and unknown values use the default fully-open behavior.
	return "open"
}

// DesktopReasoningDisplayModeExplicit reports whether a valid mode was stored.
// Legacy enum values still count as explicit so an old config does not get
// silently rehydrated as the new default.
func (c *Config) DesktopReasoningDisplayModeExplicit() bool {
	switch strings.ToLower(strings.TrimSpace(c.Desktop.ReasoningDisplayMode)) {
	case "open", "half", "closed", "hidden", "summary", "auto":
		return true
	default:
		return false
	}
}

// SetDesktopReasoningDisplayMode writes the three-state fold behavior.
func (c *Config) SetDesktopReasoningDisplayMode(mode string) error {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "open", "half", "closed":
		c.Desktop.ReasoningDisplayMode = strings.ToLower(strings.TrimSpace(mode))
		// Legacy alias kept true for every new mode: open and half expand
		// thinking by definition, and closed still expands while streaming.
		c.Desktop.ExpandThinking = true
	default:
		return fmt.Errorf("reasoning display mode %q: must be open|half|closed", mode)
	}
	return nil
}

// SetExpandThinking preserves the legacy desktop edit API. The old "false"
// modes (hidden/summary) no longer exist; both map to closed.
func (c *Config) SetExpandThinking(_ bool) error {
	return c.SetDesktopReasoningDisplayMode("closed")
}

func renderDesktopReasoningDisplayMode(b *strings.Builder, c *Config) {
	fmt.Fprintf(b, "expand_thinking = %v   # desktop: legacy reasoning display alias; use reasoning_display_mode\n", c.Desktop.ExpandThinking)
	if c.DesktopReasoningDisplayModeExplicit() {
		fmt.Fprintf(b, "reasoning_display_mode = %q   # desktop: open|half|closed reasoning fold behavior\n", c.DesktopReasoningDisplayMode())
	}
}
