package config

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"reasonix/internal/provider"
)

func renderPricingInline(p *provider.Pricing) string {
	if p == nil {
		return "{}"
	}
	return fmt.Sprintf("{ cache_hit = %v, input = %v, output = %v, currency = %q }",
		p.CacheHit, p.Input, p.Output, p.Symbol())
}

func renderPricingMap(prices map[string]*provider.Pricing) string {
	if len(prices) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(prices))
	for model := range prices {
		if strings.TrimSpace(model) != "" && prices[model] != nil {
			keys = append(keys, model)
		}
	}
	if len(keys) == 0 {
		return "{}"
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{ ")
	for i, model := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s = %s", strconv.Quote(model), renderPricingInline(prices[model]))
	}
	b.WriteString(" }")
	return b.String()
}

func configVersion(c *Config) int {
	if c != nil && c.ConfigVersion > 0 {
		return c.ConfigVersion
	}
	return Default().ConfigVersion
}

func shouldRenderUI(c, defaults *Config, scope RenderScope) bool {
	if scope != RenderScopeProject {
		return true
	}
	return !reflect.DeepEqual(c.UI, defaults.UI)
}

func shouldRenderNetwork(c, defaults *Config, scope RenderScope) bool {
	if scope != RenderScopeProject {
		return true
	}
	return !reflect.DeepEqual(c.Network, defaults.Network)
}

func shouldRenderEnvironment(c, defaults *Config, scope RenderScope) bool {
	if scope != RenderScopeProject {
		return true
	}
	return !reflect.DeepEqual(c.Environment, defaults.Environment)
}

func renderEnvironmentConfig(b *strings.Builder, cfg EnvironmentConfig) {
	b.WriteString("[environment]\n")
	enabled := true
	if cfg.Enabled != nil {
		enabled = *cfg.Enabled
	}
	fmt.Fprintf(b, "enabled = %v   # inject a stable startup environment summary into the model prompt\noffline = %v   # declare that outbound network access is unavailable; prevents futile retries\n", enabled, cfg.Offline)
	if len(cfg.Tools) == 0 {
		b.WriteString("# [environment.tools]\n")
		b.WriteString("# go = \"/opt/homebrew/bin/go\"   # trusted executable path; workspace-local paths are not auto-executed\n\n")
		return
	}
	b.WriteString("\n[environment.tools]\n")
	names := make([]string, 0, len(cfg.Tools))
	for name := range cfg.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(b, "%s = %q\n", renderTOMLKeyPart(name), cfg.Tools[name])
	}
	b.WriteString("\n")
}

func shouldRenderProviders(c, defaults *Config, scope RenderScope) bool {
	if scope != RenderScopeProject {
		return true
	}
	return !reflect.DeepEqual(c.Providers, defaults.Providers)
}

func projectScopedConfigForRender(c *Config) *Config {
	if c == nil || len(c.providerSources) == 0 {
		return c
	}
	cp := *c
	cp.Providers = make([]ProviderEntry, 0, len(c.Providers)+len(c.shadowedProjectProviders))
	for _, p := range c.Providers {
		if c.providerSources[providerMergeKey(p)] == providerSourceUser {
			continue
		}
		cp.Providers = append(cp.Providers, p)
	}
	cp.Providers = append(cp.Providers, c.shadowedProjectProviders...)
	return &cp
}

func shouldRenderSystemPrompt(c, defaults *Config, scope RenderScope) bool {
	if scope == RenderScopeFull {
		return true
	}
	return strings.TrimSpace(c.Agent.SystemPrompt) != "" && c.Agent.SystemPrompt != defaults.Agent.SystemPrompt
}

func renderLSPConfig(b *strings.Builder, cfg LSPConfig) {
	b.WriteString("[lsp]\n")
	fmt.Fprintf(b, "enabled = %v   # language server tools; servers launch lazily when used\n", cfg.Enabled)
	if len(cfg.Servers) == 0 {
		b.WriteString("# [lsp.servers.go]\n")
		b.WriteString("# command = \"gopls\"\n")
		b.WriteString("# args = []\n")
		b.WriteString("# extensions = [\".go\"]\n\n")
		return
	}
	b.WriteString("\n")

	langs := make([]string, 0, len(cfg.Servers))
	for lang := range cfg.Servers {
		langs = append(langs, lang)
	}
	sort.Strings(langs)
	for _, lang := range langs {
		srv := cfg.Servers[lang]
		fmt.Fprintf(b, "[%s]\n", renderTOMLTablePath("lsp", "servers", lang))
		if srv.Command != "" {
			fmt.Fprintf(b, "command = %q\n", srv.Command)
		}
		if len(srv.Args) > 0 {
			fmt.Fprintf(b, "args = %s\n", renderStringArray(srv.Args))
		}
		if len(srv.Env) > 0 {
			fmt.Fprintf(b, "env = %s\n", renderStringMap(srv.Env))
		}
		if srv.LanguageID != "" {
			fmt.Fprintf(b, "language_id = %q\n", srv.LanguageID)
		}
		if len(srv.Extensions) > 0 {
			fmt.Fprintf(b, "extensions = %s\n", renderStringArray(srv.Extensions))
		}
		if srv.InstallHint != "" {
			fmt.Fprintf(b, "install_hint = %q\n", srv.InstallHint)
		}
		b.WriteString("\n")
	}
}

func renderTOMLKeyPart(key string) string {
	if isBareTOMLKey(key) {
		return key
	}
	return strconv.Quote(key)
}

func renderTOMLTablePath(parts ...string) string {
	rendered := make([]string, 0, len(parts))
	for _, part := range parts {
		rendered = append(rendered, renderTOMLKeyPart(part))
	}
	return strings.Join(rendered, ".")
}

func isBareTOMLKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

// renderStringArray renders a []string as a TOML inline array.
func renderStringArray(ss []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, s := range ss {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", s)
	}
	b.WriteByte(']')
	return b.String()
}

// renderStringMap renders a map[string]string as a TOML inline table with keys
// in sorted order so output is deterministic (round-trips cleanly).
func renderStringMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{ ")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s = %q", renderTOMLKeyPart(k), m[k])
	}
	b.WriteString(" }")
	return b.String()
}

func renderAnyMap(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if _, ok := renderAnyValue(v); ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{ ")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		value, _ := renderAnyValue(m[k])
		fmt.Fprintf(&b, "%s = %s", strconv.Quote(k), value)
	}
	b.WriteString(" }")
	return b.String()
}

func renderAnyValue(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		return strconv.Quote(x), true
	case bool:
		if x {
			return "true", true
		}
		return "false", true
	case int:
		return strconv.Itoa(x), true
	case int8:
		return strconv.FormatInt(int64(x), 10), true
	case int16:
		return strconv.FormatInt(int64(x), 10), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint:
		return strconv.FormatUint(uint64(x), 10), true
	case uint8:
		return strconv.FormatUint(uint64(x), 10), true
	case uint16:
		return strconv.FormatUint(uint64(x), 10), true
	case uint32:
		return strconv.FormatUint(uint64(x), 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	case float32:
		return formatFloat(float64(x)), true
	case float64:
		return formatFloat(x), true
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			part, ok := renderAnyValue(item)
			if !ok {
				return "", false
			}
			parts = append(parts, part)
		}
		return "[" + strings.Join(parts, ", ") + "]", true
	case []string:
		return renderStringArray(x), true
	case map[string]any:
		return renderAnyMap(x), true
	case map[string]string:
		return renderStringMap(x), true
	default:
		return "", false
	}
}

func renderModelOverrides(m map[string]ProviderModelOverride) string {
	keys := make([]string, 0, len(m))
	for k, ov := range m {
		if k == "" || modelOverrideEmpty(ov) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{ ")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q = %s", k, renderModelOverride(m[k]))
	}
	b.WriteString(" }")
	return b.String()
}

func renderModelOverride(ov ProviderModelOverride) string {
	var parts []string
	if ov.ReasoningProtocol != "" {
		parts = append(parts, fmt.Sprintf("reasoning_protocol = %q", ov.ReasoningProtocol))
	}
	if len(ov.SupportedEfforts) > 0 {
		parts = append(parts, "supported_efforts = "+renderStringArray(ov.SupportedEfforts))
	}
	if ov.DefaultEffort != "" {
		parts = append(parts, fmt.Sprintf("default_effort = %q", ov.DefaultEffort))
	}
	if ov.Vision != nil {
		parts = append(parts, fmt.Sprintf("vision = %t", *ov.Vision))
	}
	if ov.ContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("context_window = %d", ov.ContextWindow))
	}
	if ov.MaxOutputTokens != 0 {
		parts = append(parts, fmt.Sprintf("max_output_tokens = %d", ov.MaxOutputTokens))
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

func modelOverrideEmpty(ov ProviderModelOverride) bool {
	return ov.ReasoningProtocol == "" && len(ov.SupportedEfforts) == 0 && ov.DefaultEffort == "" && ov.Vision == nil && ov.ContextWindow <= 0 && ov.MaxOutputTokens == 0
}

func hasPositiveIntMap(m map[string]int) bool {
	for k, v := range m {
		if strings.TrimSpace(k) != "" && v > 0 {
			return true
		}
	}
	return false
}

// renderIntMap renders a map[string]int as a TOML inline table with positive
// values only, preserving deterministic key order.
func renderIntMap(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if strings.TrimSpace(k) != "" && v > 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{ ")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q = %d", k, m[k])
	}
	b.WriteString(" }")
	return b.String()
}

// renderRuleList emits a permission rule list. A populated list renders as an
// active TOML array; an empty one renders as a commented example so `reasonix setup`
// scaffolds discoverable guidance without imposing surprising rules.
func renderRuleList(key string, rules []string, example string) string {
	if len(rules) == 0 {
		return fmt.Sprintf("# %s = %s\n", key, example)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s = [", key)
	for i, r := range rules {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", r)
	}
	b.WriteString("]\n")
	return b.String()
}

// formatFloat ensures a float renders with a decimal point so TOML types it as a
// float, not an integer (e.g. 0 -> "0.0").
func formatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
