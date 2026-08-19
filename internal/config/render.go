package config

import (
	"fmt"
	"strings"

	"reasonix/internal/billing"
)

// RenderTOML renders the config as annotated TOML in the `reasonix setup` house style:
// comments preserved, system_prompt as a multi-line string, helpful hints. The
// output round-trips back through Load (see render_test.go).

// RenderTOMLForScope renders an annotated TOML file for a specific persistence
// target. User configs can carry desktop and account-level preferences; project
// reasonix.toml stays focused on project behavior and intentionally excludes
// desktop-only preferences.
func RenderTOMLForScope(c *Config, scope RenderScope) string {
	if c == nil {
		c = Default()
	}
	switch scope {
	case RenderScopeUser, RenderScopeProject:
	default:
		scope = RenderScopeFull
	}
	if scope == RenderScopeProject {
		c = projectScopedConfigForRender(c)
	}
	defaults := Default()
	var b strings.Builder

	b.WriteString("# Reasonix configuration.\n")
	fmt.Fprintf(&b, "# Resolution order: flag > ./reasonix.toml > %s > built-in defaults.\n", userConfigDisplayPath())
	b.WriteString("# Fields marked user/global only are not overridden by ./reasonix.toml.\n")
	b.WriteString("# Secrets are named via api_key_env and stored in Reasonix's global .env; never put keys here.\n\n")

	fmt.Fprintf(&b, "config_version = %d   # schema marker for diagnostics; old versions may ignore it\n", configVersion(c))
	fmt.Fprintf(&b, "default_model = %q\n", c.DefaultModel)
	if c.Language != "" {
		fmt.Fprintf(&b, "language      = %q   # ui/model language; empty = auto-detect from $LANG / $REASONIX_LANG\n", c.Language)
	} else {
		b.WriteString("# language      = \"zh\"   # ui/model language; empty = auto-detect from $LANG / $REASONIX_LANG\n")
	}
	if scope != RenderScopeProject {
		fmt.Fprintf(&b, "credentials_store = %q   # legacy compatibility; provider keys are saved in Reasonix's global .env\n", normalizeCredentialsStore(c.CredentialsStore))
	}
	b.WriteString("\n")

	if shouldRenderUI(c, defaults, scope) {
		b.WriteString("[ui]\n")
		fmt.Fprintf(&b, "theme = %q   # auto|dark|light; CLI colors only; REASONIX_THEME can override per run\n", c.UITheme())
		if style := c.UIThemeStyle(); style != "" {
			fmt.Fprintf(&b, "theme_style = %q   # CLI accent palette; REASONIX_THEME_STYLE can override per run\n", style)
		} else {
			b.WriteString("# theme_style = \"graphite\"   # graphite|aurora|slate|carbon|nocturne|amber and legacy aliases\n")
		}
		if layout := c.UIShortcutLayout(); layout != "classic" {
			fmt.Fprintf(&b, "shortcut_layout = %q   # classic|desktop; compatibility setting; Shift+Tab toggles Plan, Ctrl+Y toggles YOLO\n", layout)
		} else {
			b.WriteString("# shortcut_layout = \"desktop\"   # classic|desktop; compatibility setting; Shift+Tab toggles Plan, Ctrl+Y toggles YOLO\n")
		}
		if strings.TrimSpace(c.UI.CursorShape) != "" {
			fmt.Fprintf(&b, "cursor_shape = %q   # block|underline|bar; text input cursor shape\n", c.UICursorShape())
		} else {
			b.WriteString("# cursor_shape = \"bar\"   # block|underline|bar; text input cursor shape\n")
		}
		if strings.TrimSpace(c.UI.CloseBehavior) != "" && scope == RenderScopeProject {
			fmt.Fprintf(&b, "close_behavior = %q   # legacy desktop close behavior; prefer [desktop].close_behavior in user config\n", c.DesktopCloseBehavior())
		}
		if c.UI.ShowReasoning {
			b.WriteString("show_reasoning = true   # CLI: show thinking text by default; false = collapsed (toggle with Ctrl+O)\n")
		} else {
			b.WriteString("# show_reasoning = true   # CLI: show thinking text by default; false = collapsed (toggle with Ctrl+O)\n")
		}
		fmt.Fprintf(&b, "show_turn_usage = %v   # CLI/TUI: show per-request token and cost receipts in the transcript\n", c.UI.ShowTurnUsage)
		b.WriteString("\n")
	}

	if scope != RenderScopeProject {
		b.WriteString("[desktop]\n")
		if lang := c.DesktopLanguage(); lang != "" {
			fmt.Fprintf(&b, "language = %q   # desktop UI language; empty/auto = browser/OS auto-detect\n", lang)
		} else {
			b.WriteString("# language = \"zh\"   # desktop UI language; empty/auto = browser/OS auto-detect\n")
		}
		// Legacy desktop.currency is still emitted when set so older binaries keep
		// reading the preference; new writers also own [billing].display_currency.
		if currency := c.DesktopCurrency(); currency != "" {
			fmt.Fprintf(&b, "currency = %q   # legacy display currency; prefer [billing].display_currency\n", currency)
		}
		fmt.Fprintf(&b, "layout_style = %q   # desktop layout: classic|workbench|creation\n", c.DesktopLayoutStyle())
		fmt.Fprintf(&b, "theme = %q   # desktop only: auto|dark|light\n", c.DesktopTheme())
		fmt.Fprintf(&b, "terminal_theme = %q   # integrated terminal: auto|dark|light; auto follows the desktop app\n", c.DesktopTerminalTheme())
		if style := c.DesktopThemeStyle(); style != "" {
			fmt.Fprintf(&b, "theme_style = %q   # desktop accent palette\n", style)
		} else {
			b.WriteString("# theme_style = \"graphite\"   # graphite|aurora|slate|carbon|nocturne|amber and legacy aliases\n")
		}
		if opener := c.DesktopExternalOpener(); opener != "" {
			fmt.Fprintf(&b, "external_opener = %q   # desktop Open control: installed application id\n", opener)
		} else {
			b.WriteString("# external_opener = \"vscode\"   # desktop Open control: installed application id\n")
		}
		fmt.Fprintf(&b, "close_behavior = %q   # desktop: quit|background when the window close button is clicked\n", c.DesktopCloseBehavior())
		fmt.Fprintf(&b, "status_bar_style = %q   # desktop: icon|text metric labels in the bottom status bar\n", c.DesktopStatusBarStyle())
		fmt.Fprintf(&b, "status_bar_items = %s   # desktop: ordered visible bottom status bar items\n", renderStringArray(c.DesktopStatusBarItems()))
		fmt.Fprintf(&b, "default_tool_approval_mode = %q   # desktop: Ask/Auto/YOLO default for newly-created sessions\n", c.DesktopDefaultToolApprovalMode())
		fmt.Fprintf(&b, "check_updates = %v   # desktop: check for new versions on startup\n", c.DesktopCheckUpdates())
		fmt.Fprintf(&b, "telemetry = %v   # desktop: anonymous launch ping + scrubbed next-launch native crash diagnostics; never content\n", c.DesktopTelemetry())
		fmt.Fprintf(&b, "metrics = %v   # desktop: aggregate quality/lifecycle metrics (anonymous signal/bucket counts); never content\n", c.DesktopMetrics())
		// A non-nil empty slice is intentional: provider_access = [] means the
		// user removed every desktop access entry. Omitting it would make the next
		// load treat the config as legacy and infer access again.
		if c.Desktop.ProviderAccess != nil {
			fmt.Fprintf(&b, "provider_access = %s   # desktop settings: providers shown on Settings > Model > Access\n", renderStringArray(c.Desktop.ProviderAccess))
		}
		if len(c.Desktop.ProviderDisabled) > 0 {
			fmt.Fprintf(&b, "provider_disabled = %s   # desktop settings: providers hidden from the model switcher\n", renderStringArray(c.Desktop.ProviderDisabled))
		}
		renderDesktopReasoningDisplayMode(&b, c)
		fmt.Fprintf(&b, "display_mode = %q   # desktop: standard|compact transcript display mode\n", c.DesktopDisplayMode())
		if width := c.DesktopConversationWidth(); width == "full" {
			fmt.Fprintf(&b, "conversation_width = %q   # desktop: standard|full transcript width; empty = standard\n", width)
		}
		b.WriteString("\n")

		b.WriteString("[billing]\n")
		if pref := c.DisplayCurrencyPref(); pref != "" {
			fmt.Fprintf(&b, "display_currency = %q   # auto|CNY|USD; display only — does not rewrite provider list prices\n", pref)
		} else {
			b.WriteString("# display_currency = \"auto\"   # auto|CNY|USD; display only — does not rewrite provider list prices\n")
		}
		b.WriteString("\n")
	} else if c.Desktop.ProviderAccess != nil {
		// provider_access is intentionally mergeable across user and project
		// configs. It is the only desktop field written to reasonix.toml: local
		// providers then appear in that workspace's desktop model switcher without
		// copying user-global appearance or security preferences into the project.
		b.WriteString("[desktop]\n")
		fmt.Fprintf(&b, "provider_access = %s   # providers available to this workspace in the desktop model switcher\n\n", renderStringArray(c.Desktop.ProviderAccess))
	}

	if scope != RenderScopeProject {
		if c.CLITelemetryConfigured() {
			b.WriteString("[telemetry]\n")
			fmt.Fprintf(&b, "cli_metrics = %q   # CLI content-free usage metrics: auto|on|off; auto requires a local interactive terminal\n\n", c.CLITelemetryMode())
		}

		b.WriteString("[notifications]\n")
		fmt.Fprintf(&b, "enabled = %v   # system notifications for CLI and desktop turns; default off\n", c.Notifications.Enabled)
		fmt.Fprintf(&b, "turn_done = %v   # notify when a turn finishes\n", c.Notifications.TurnDone)
		fmt.Fprintf(&b, "approval_request = %v   # notify when a tool approval is waiting\n", c.Notifications.ApprovalRequest)
		fmt.Fprintf(&b, "ask_request = %v   # notify when a question is waiting\n", c.Notifications.AskRequest)
		b.WriteString("\n")
	}

	if shouldRenderNetwork(c, defaults, scope) {
		b.WriteString("[network]\n")
		fmt.Fprintf(&b, "proxy_mode = %q   # auto|env|custom|off; auto currently uses env proxy\n", c.NetworkProxyMode())
		if c.Network.ProxyURL != "" {
			fmt.Fprintf(&b, "proxy_url  = %q   # custom override, e.g. socks5://127.0.0.1:7890\n", c.Network.ProxyURL)
		} else {
			b.WriteString("# proxy_url  = \"socks5://127.0.0.1:7890\"   # optional custom override\n")
		}
		if c.Network.NoProxy != "" {
			fmt.Fprintf(&b, "no_proxy   = %q   # honored for proxy_mode = \"custom\"\n", c.Network.NoProxy)
		} else {
			b.WriteString("# no_proxy   = \"localhost,127.0.0.1,.local\"   # honored for proxy_mode = \"custom\"\n")
		}
		b.WriteString("\n[network.proxy]\n")
		proxyType := c.Network.Proxy.Type
		if proxyType == "" {
			proxyType = "socks5"
		}
		fmt.Fprintf(&b, "type = %q   # http|https|socks5|socks5h\n", proxyType)
		if c.Network.Proxy.Server != "" {
			fmt.Fprintf(&b, "server = %q\n", c.Network.Proxy.Server)
		} else {
			b.WriteString("# server = \"127.0.0.1\"\n")
		}
		if c.Network.Proxy.Port > 0 {
			fmt.Fprintf(&b, "port = %d\n", c.Network.Proxy.Port)
		} else {
			b.WriteString("# port = 7890\n")
		}
		if c.Network.Proxy.Username != "" {
			fmt.Fprintf(&b, "username = %q\n", c.Network.Proxy.Username)
		} else {
			b.WriteString("# username = \"\"\n")
		}
		if c.Network.Proxy.Password != "" {
			fmt.Fprintf(&b, "password = %q   # supports ${VAR} expansion\n", c.Network.Proxy.Password)
		} else {
			b.WriteString("# password = \"${REASONIX_PROXY_PASSWORD}\"   # optional; supports ${VAR} expansion\n")
		}
		b.WriteString("\n")
	}
	if shouldRenderEnvironment(c, defaults, scope) {
		renderEnvironmentConfig(&b, c.Environment)
	}

	b.WriteString("[agent]\n")
	if shouldRenderSystemPrompt(c, defaults, scope) {
		b.WriteString("system_prompt = \"\"\"\n")
		b.WriteString(c.Agent.SystemPrompt)
		b.WriteString("\"\"\"\n")
	} else {
		b.WriteString("# system_prompt = \"\"\"...\"\"\"   # omit to use the built-in prompt for this version\n")
	}
	if c.Agent.SystemPromptFile != "" {
		fmt.Fprintf(&b, "system_prompt_file = %q\n", c.Agent.SystemPromptFile)
	} else {
		b.WriteString("# system_prompt_file = \"prompts/system.md\"   # project paths stay in <workspace>; user paths may fall back to <reasonix home>\n")
	}
	fmt.Fprintf(&b, "temperature       = %s\n", formatFloat(c.Agent.Temperature))
	if strings.TrimSpace(c.Agent.RecoveryModel) != "" {
		fmt.Fprintf(&b, "recovery_model = %q   # optional independent reviewer for low-risk automatic recovery\n", c.Agent.RecoveryModel)
	} else {
		b.WriteString("# recovery_model = \"deepseek-pro\"   # optional; falls back to guardian then main model\n")
	}
	if lang := c.ReasoningLanguage(); lang != "auto" {
		fmt.Fprintf(&b, "reasoning_language = %q   # visible reasoning language: auto|zh|en\n", lang)
	} else {
		b.WriteString("# reasoning_language = \"zh\"   # visible reasoning language: auto|zh|en\n")
	}
	fmt.Fprintf(&b, "compact_ratio       = %s   # sole auto trigger; presets 0.70/0.80/0.85 (default 0.85)\n", formatFloat(c.Agent.CompactRatio))
	if c.Agent.Keep != nil {
		fmt.Fprintf(&b, "keep                = %s   # compaction keep policy: errors, user_marked\n", renderStringArray(c.Agent.Keep))
	} else {
		b.WriteString("# keep                = [\"errors\"]   # compaction keep policy: errors, user_marked\n")
	}
	if c.Agent.RecentKeep > 0 {
		fmt.Fprintf(&b, "recent_keep         = %d   # minimum recent messages kept verbatim\n", c.Agent.RecentKeep)
	} else {
		b.WriteString("# recent_keep         = 2   # minimum recent messages kept verbatim\n")
	}
	if len(c.Agent.PlanModeReadOnlyCommands) > 0 {
		fmt.Fprintf(&b, "plan_mode_read_only_commands = %s   # legacy compatibility only; Plan bash uses Permissions\n", renderStringArray(c.Agent.PlanModeReadOnlyCommands))
	} else {
		b.WriteString("# plan_mode_read_only_commands = [\"gh issue view\"]   # legacy compatibility only; Plan bash uses Permissions\n")
	}
	if c.Agent.PlannerModel != "" {
		fmt.Fprintf(&b, "planner_model = %q   # low-frequency planner (two-model collaboration)\n", c.Agent.PlannerModel)
	} else {
		b.WriteString("# planner_model = \"deepseek-pro\"   # optional: enable two-model collaboration\n")
	}
	if c.Agent.SubagentModel != "" {
		fmt.Fprintf(&b, "subagent_model = %q   # default model for runAs=subagent skills\n", c.Agent.SubagentModel)
	} else {
		b.WriteString("# subagent_model = \"deepseek-pro\"   # optional default for runAs=subagent skills\n")
	}
	if len(c.Agent.SubagentModels) > 0 {
		fmt.Fprintf(&b, "subagent_models = %s   # per-skill overrides\n", renderStringMap(c.Agent.SubagentModels))
	} else {
		b.WriteString("# subagent_models = { review = \"deepseek-pro\", security_review = \"deepseek-pro\" }   # per-skill overrides\n")
	}
	if c.Agent.SubagentEffort != "" {
		fmt.Fprintf(&b, "subagent_effort = %q   # default effort for subagent entry points\n", c.Agent.SubagentEffort)
	} else {
		b.WriteString("# subagent_effort = \"high\"   # optional default effort for subagents\n")
	}
	if len(c.Agent.SubagentEfforts) > 0 {
		fmt.Fprintf(&b, "subagent_efforts = %s   # per-tool/skill effort overrides\n", renderStringMap(c.Agent.SubagentEfforts))
	} else {
		b.WriteString("# subagent_efforts = { review = \"max\", task = \"high\" }   # per-tool/skill effort overrides\n")
	}
	if c.Agent.MaxSubagentDepth != defaults.Agent.MaxSubagentDepth {
		fmt.Fprintf(&b, "max_subagent_depth = %d   # nested subagent delegation depth; 1 restores the old single-layer boundary\n", c.Agent.MaxSubagentDepth)
	} else {
		b.WriteString("# max_subagent_depth = 2   # nested subagent delegation depth; set 1 to disable nested delegation\n")
	}
	if c.Agent.MaxSubagentConcurrency != defaults.Agent.MaxSubagentConcurrency {
		fmt.Fprintf(&b, "max_subagent_concurrency = %d   # session-wide sub-agent concurrency (task/fleet/skills)\n", c.Agent.MaxSubagentConcurrency)
	} else {
		b.WriteString("# max_subagent_concurrency = 6   # session-wide sub-agent concurrency (task/fleet/skills)\n")
	}
	if c.Agent.MaxParallelWriters != defaults.Agent.MaxParallelWriters {
		fmt.Fprintf(&b, "max_parallel_writers = %d   # concurrent writers with non-overlapping write_paths\n", c.Agent.MaxParallelWriters)
	} else {
		b.WriteString("# max_parallel_writers = 3   # concurrent writers with non-overlapping write_paths\n")
	}
	if c.Agent.OutputStyle != "" {
		fmt.Fprintf(&b, "output_style = %q   # persona/tone folded into the prompt\n", c.Agent.OutputStyle)
	} else {
		b.WriteString("# output_style = \"explanatory\"   # explanatory | learning | concise | custom; empty = default\n")
	}
	b.WriteString("\n")

	if shouldRenderProviders(c, defaults, scope) {
		for _, p := range c.Providers {
			b.WriteString("[[providers]]\n")
			fmt.Fprintf(&b, "name        = %q\n", p.Name)
			fmt.Fprintf(&b, "kind        = %q\n", p.Kind)
			fmt.Fprintf(&b, "base_url    = %q\n", p.BaseURL)
			if p.ChatURL != "" {
				fmt.Fprintf(&b, "chat_url    = %q   # legacy OpenAI chat endpoint override\n", p.ChatURL)
			}
			if p.RequestURL != "" {
				fmt.Fprintf(&b, "request_url = %q   # exact provider request URL; no path completion\n", p.RequestURL)
			}
			if len(p.Models) > 0 {
				fmt.Fprintf(&b, "models      = %s\n", renderStringArray(p.Models))
				if p.Default != "" {
					fmt.Fprintf(&b, "default     = %q\n", p.Default)
				}
			} else if p.Model != "" {
				fmt.Fprintf(&b, "model       = %q\n", p.Model)
			}
			if p.ModelsURL != "" {
				fmt.Fprintf(&b, "models_url  = %q   # auto-fetch models from this URL on startup\n", p.ModelsURL)
			}
			fmt.Fprintf(&b, "api_key_env = %q\n", p.APIKeyEnv)
			if p.PresetID != "" {
				fmt.Fprintf(&b, "preset_id   = %q   # curated preset identity; settings UI uses it to avoid duplicate installs\n", p.PresetID)
			}
			if p.PresetVersion > 0 {
				fmt.Fprintf(&b, "preset_version = %d\n", p.PresetVersion)
			}
			if len(p.Headers) > 0 {
				fmt.Fprintf(&b, "headers     = %s   # extra static request headers; keep secrets in api_key_env\n", renderStringMap(p.Headers))
			}
			if len(p.ExtraBody) > 0 {
				fmt.Fprintf(&b, "extra_body  = %s   # extra top-level JSON request body fields for compatible gateways\n", renderAnyMap(p.ExtraBody))
			}
			if p.AuthHeader {
				b.WriteString("auth_header = true   # Anthropic-compatible: send Authorization: Bearer <api_key> instead of x-api-key\n")
			}
			if p.ResponsesMode != "" {
				fmt.Fprintf(&b, "responses_mode = %q   # responses provider: stateless|stateful\n", p.ResponsesMode)
			}
			if p.ResponsesStateful != nil {
				fmt.Fprintf(&b, "responses_stateful = %t   # legacy responses mode switch\n", *p.ResponsesStateful)
			}
			if p.BalanceURL != "" {
				fmt.Fprintf(&b, "balance_url = %q   # optional; wallet-balance endpoint shown in the status bar\n", p.BalanceURL)
			}
			if p.ContextWindow > 0 {
				fmt.Fprintf(&b, "context_window = %d   # tokens; compaction triggers near this limit\n", p.ContextWindow)
			}
			if p.MaxOutputTokens != 0 {
				fmt.Fprintf(&b, "max_output_tokens = %d   # per-turn total output; 0 = auto (~64K on DeepSeek high), 32768/65536/131072 explicit; never affects compact_ratio\n", p.MaxOutputTokens)
			} else {
				b.WriteString("# max_output_tokens = 0       # recommended: automatic (DeepSeek default high → ~64K; not unlimited)\n")
				b.WriteString("# max_output_tokens = 32768   # ordinary coding / cost control\n")
				b.WriteString("# max_output_tokens = 65536   # heavy reasoning / long tool loops\n")
				b.WriteString("# max_output_tokens = 131072  # only after repeated finish_reason=length\n")
			}
			if p.Price != nil {
				fmt.Fprintf(&b, "price       = %s   # provider-wide fallback, per 1M tokens\n", renderPricingInline(p.Price))
			}
			if len(p.Prices) > 0 {
				fmt.Fprintf(&b, "prices      = %s   # per-model prices, per 1M tokens\n", renderPricingMap(p.Prices))
			}
			if cur := strings.TrimSpace(p.BillingCurrency); cur != "" {
				fmt.Fprintf(&b, "billing_currency = %q   # frozen list-price currency; independent of display_currency\n", billing.NormalizeCurrency(cur))
			}
			if mode := strings.TrimSpace(p.BillingMode); mode != "" && mode != "payg" {
				fmt.Fprintf(&b, "billing_mode = %q   # payg|subscription_equivalent\n", mode)
			}
			if p.Thinking != "" {
				fmt.Fprintf(&b, "thinking    = %q\n", p.Thinking)
			}
			if p.Effort != "" {
				fmt.Fprintf(&b, "effort      = %q\n", p.Effort)
			}
			if p.Vision {
				b.WriteString("vision      = true   # provider accepts image input for all listed models\n")
			}
			if p.VisionModels != nil {
				fmt.Fprintf(&b, "vision_models = %s   # models in this provider that accept image input\n", renderStringArray(p.VisionModels))
			}
			if p.VisionDetail != "" {
				fmt.Fprintf(&b, "vision_detail = %q   # openai image detail hint: low|high; empty = auto\n", p.VisionDetail)
			}
			if p.WebSearch != nil {
				fmt.Fprintf(&b, "web_search  = %t   # provider-executed web_search tool; omitted defaults on for supported official DeepSeek APIs\n", *p.WebSearch)
			}
			if p.ReasoningProtocol != "" {
				fmt.Fprintf(&b, "reasoning_protocol = %q   # auto|deepseek|glm|kimi-k3|openai|none; overrides model/endpoint reasoning detection\n", p.ReasoningProtocol)
			}
			if len(p.SupportedEfforts) > 0 {
				fmt.Fprintf(&b, "supported_efforts = %s   # custom /effort levels exposed by this provider; overrides the built-in Kind/BaseURL default\n", renderStringArray(p.SupportedEfforts))
			}
			if p.DefaultEffort != "" {
				fmt.Fprintf(&b, "default_effort    = %q   # used when /effort is auto or unset; must be one of supported_efforts\n", p.DefaultEffort)
			}
			if len(p.ModelOverrides) > 0 {
				fmt.Fprintf(&b, "model_overrides   = %s   # per-model context/output/reasoning/vision overrides for mixed gateways\n", renderModelOverrides(p.ModelOverrides))
			}
			if p.NoProxy {
				b.WriteString("no_proxy    = true   # reach this base_url directly, never via the proxy\n")
			}
			b.WriteString("\n")
		}
	}

	b.WriteString("[tools]\n")
	if len(c.Tools.Enabled) == 0 {
		b.WriteString("enabled = []   # empty = all built-in tools\n")
	} else {
		b.WriteString("enabled = [")
		for i, t := range c.Tools.Enabled {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", t)
		}
		b.WriteString("]\n")
	}
	fmt.Fprintf(&b, "bash_timeout_seconds = %d   # foreground safety cap; set 0 for no tool-local cap\n", c.BashTimeoutSeconds())
	fmt.Fprintf(&b, "mcp_startup_timeout_seconds = %d   # background initialize + tools/list safety cap; per-plugin overrides may raise it\n", c.MCPStartupTimeoutSeconds())
	fmt.Fprintf(&b, "mcp_call_timeout_seconds = %d   # default MCP call safety cap; per-plugin/tool overrides may raise it\n\n", c.MCPCallTimeoutSeconds())

	b.WriteString("[tools.background_jobs]\n")
	fmt.Fprintf(&b, "stalled_warning_seconds = %d   # warn once per background job after this many quiet seconds; 0 disables\n\n", c.BackgroundJobStalledWarningSeconds())

	b.WriteString("[tools.shell]\n")
	if c.Tools.Shell.Prefer != "" {
		fmt.Fprintf(&b, "prefer = %q   # auto|bash|powershell|pwsh; empty/default = auto-detect\n", c.Tools.Shell.Prefer)
	} else {
		b.WriteString("# prefer = \"auto\"   # auto|bash|powershell|pwsh; empty/default = auto-detect\n")
	}
	if c.Tools.Shell.Path != "" {
		fmt.Fprintf(&b, "path   = %q   # absolute path to the shell executable; empty = PATH lookup\n\n", c.Tools.Shell.Path)
	} else {
		b.WriteString("# path   = \"/opt/homebrew/bin/bash\"   # absolute path to the shell executable; empty = PATH lookup\n\n")
	}

	renderLSPConfig(&b, c.LSP)

	b.WriteString("[skills]\n")
	if len(c.Skills.Paths) > 0 {
		fmt.Fprintf(&b, "paths = %s   # extra custom skill roots\n", renderStringArray(c.Skills.Paths))
	} else {
		b.WriteString("# paths = [\"~/my-skills\", \"../shared/skills\"]   # extra custom skill roots\n")
	}
	if len(c.Skills.ExcludedPaths) > 0 {
		fmt.Fprintf(&b, "excluded_paths = %s   # skill roots hidden from discovery\n", renderStringArray(c.Skills.ExcludedPaths))
	} else {
		b.WriteString("# excluded_paths = [\"~/.agents/skills\"]   # hide convention roots without deleting folders\n")
	}
	if c.Skills.DisableImplicitInvocation {
		b.WriteString("disable_implicit_invocation = true   # keep /skill explicit; hide skill discovery and tools from the model\n")
	} else {
		b.WriteString("# disable_implicit_invocation = false   # keep skills available for automatic model invocation\n")
	}
	if c.Skills.MaxDepth != 0 {
		fmt.Fprintf(&b, "max_depth = %d   # nested scan depth; default 3, set 1 for legacy root-only discovery\n", c.SkillMaxDepth())
	} else {
		b.WriteString("# max_depth = 3   # nested scan depth; set 1 for legacy root-only discovery\n")
	}
	if disabled := c.DisabledSkillNames(); len(disabled) > 0 {
		fmt.Fprintf(&b, "disabled_skills = %s   # hidden from the prompt, slash invocation, and skill tools\n\n", renderStringArray(disabled))
	} else {
		b.WriteString("# disabled_skills = [\"review\"]   # hide noisy or unwanted skills\n\n")
	}

	b.WriteString("[permissions]\n")
	b.WriteString("# Per-call gating. mode = writer fallback when no rule matches: ask|allow|deny.\n")
	b.WriteString("# Readers always default to allow. Precedence: deny > ask > allow > fallback.\n")
	b.WriteString("# Rules are \"Tool\" or \"Tool(specifier)\"; e.g. Bash(go test:*), Edit(src/**).\n")
	mode := c.Permissions.Mode
	if mode == "" {
		mode = "ask"
	}
	fmt.Fprintf(&b, "mode  = %q\n", mode)
	if c.Permissions.AllowDynamicBash {
		b.WriteString("allow_dynamic_bash = true   # advanced: let mode=allow cover command substitution and interpreter -c/-e\n")
	} else {
		b.WriteString("# allow_dynamic_bash = false   # advanced opt-in; deny/ask and exact rules still take precedence\n")
	}
	b.WriteString(renderRuleList("deny", c.Permissions.Deny, `["Bash(rm -rf*)", "Bash(git push*)"]   # hard-blocked in every mode`))
	b.WriteString(renderRuleList("allow", c.Permissions.Allow, `["Bash(go test:*)", "Bash(git status:*)"]   # never prompted`))
	b.WriteString(renderRuleList("ask", c.Permissions.Ask, `["Edit(src/**)"]   # force a prompt even if otherwise allowed`))
	b.WriteString("\n")

	b.WriteString("[sandbox]\n")
	b.WriteString("# Confine tool blast radius. File-writers (write_file/edit_file/multi_edit/move_file)\n")
	b.WriteString("# may only write under workspace_root (empty = current dir) and allow_write extras.\n")
	b.WriteString("# bash = \"enforce\" jails each command in an OS sandbox when available;\n")
	b.WriteString("# without one, bash execution is refused. Empty defaults to enforce on macOS/Linux.\n")
	b.WriteString("# Windows has no OS-level Bash sandbox and fixes bash = \"off\".\n")
	b.WriteString("# network allows sandboxed bash egress.\n")
	if c.Sandbox.WorkspaceRoot != "" {
		fmt.Fprintf(&b, "workspace_root = %q\n", c.Sandbox.WorkspaceRoot)
	} else {
		b.WriteString("# workspace_root = \"\"            # default: current working directory\n")
	}
	if len(c.Sandbox.AllowWrite) > 0 {
		fmt.Fprintf(&b, "allow_write = %s\n", renderStringArray(c.Sandbox.AllowWrite))
	} else {
		b.WriteString("# allow_write = [\"/tmp\"]          # extra dirs writers may also modify\n")
	}
	if len(c.Sandbox.ForbidRead) > 0 {
		fmt.Fprintf(&b, "forbid_read = %s\n", renderStringArray(c.Sandbox.ForbidRead))
	} else {
		b.WriteString("# forbid_read = []                  # dirs the agent cannot read or list\n")
	}
	fmt.Fprintf(&b, "bash    = %q\n", c.BashMode())
	fmt.Fprintf(&b, "network = %v\n", c.Sandbox.Network)
	b.WriteString("\n")

	b.WriteString("[statusline]\n")
	b.WriteString("# A custom status line: a command whose first stdout line replaces the built-in\n")
	b.WriteString("# data row. It receives {\"model\",\"contextUsed\",\"contextWindow\",\"cwd\"} as JSON on stdin.\n")
	if c.Statusline.Command != "" {
		fmt.Fprintf(&b, "command = %q\n", c.Statusline.Command)
	} else {
		b.WriteString("# command = \"my-statusline.sh\"\n")
	}
	b.WriteString("\n")

	// [secrets] is user/global only: LoadForRoot discards project values, so
	// the project scope never renders it. Rendering it here is what lets a
	// user's saved toggles survive config rewrites (WriteFile re-renders the
	// whole file from the struct).
	if scope != RenderScopeProject {
		b.WriteString("[secrets]   # credential protection; user/global only, ./reasonix.toml cannot override\n")
		if c.Secrets.FilterSubprocessEnv {
			b.WriteString("filter_subprocess_env = true   # strip credential-named env vars from tool/LSP/MCP subprocesses\n")
		} else {
			b.WriteString("# filter_subprocess_env = false   # opt-in; stripping tokens breaks gh, HTTPS git push, npm publish\n")
		}
		if c.Secrets.ProtectSensitiveFiles {
			b.WriteString("protect_sensitive_files = true   # hide .env/.git-credentials/key files/~/.ssh from read tools\n")
		} else {
			b.WriteString("# protect_sensitive_files = false   # opt-in; hiding credential files can break legitimate edit workflows\n")
		}
		b.WriteString("\n")
	}

	// [remote] was a user/global-only SSH host section; it has been removed.
	b.WriteString("# External MCP servers. type: \"stdio\" (default, a subprocess) | \"http\" | \"sse\".\n")
	b.WriteString("# ${VAR} / ${VAR:-default} are expanded from the environment in command/args/env/url/headers.\n")
	plugins := tomlPluginsForScope(c.Plugins, scope)
	if len(plugins) == 0 {
		b.WriteString("# [[plugins]]\n")
		b.WriteString("# name    = \"example\"\n")
		b.WriteString("# command = \"reasonix-plugin-example\"\n")
		b.WriteString("# startup_timeout_seconds = 60    # optional initialize + tools/list cap\n")
		b.WriteString("# call_timeout_seconds = 600       # optional per-server MCP call timeout\n")
		b.WriteString("# tool_timeout_seconds = { \"generate_video\" = 1800 }   # raw MCP tool names\n")
		b.WriteString("# [[plugins]]                                  # a remote server over Streamable HTTP\n")
		b.WriteString("# name    = \"stripe\"\n")
		b.WriteString("# type    = \"http\"\n")
		b.WriteString("# url     = \"https://mcp.stripe.com\"\n")
		b.WriteString("# headers = { Authorization = \"Bearer ${STRIPE_KEY}\" }\n")
	} else {
		for _, pl := range plugins {
			b.WriteString("\n[[plugins]]\n")
			fmt.Fprintf(&b, "name    = %q\n", pl.Name)
			if pl.Type != "" {
				fmt.Fprintf(&b, "type    = %q\n", pl.Type)
			}
			if pl.Command != "" {
				fmt.Fprintf(&b, "command = %q\n", pl.Command)
			}
			if len(pl.Args) > 0 {
				fmt.Fprintf(&b, "args    = %s\n", renderStringArray(pl.Args))
			}
			if pl.URL != "" {
				fmt.Fprintf(&b, "url     = %q\n", pl.URL)
			}
			if len(pl.Headers) > 0 {
				fmt.Fprintf(&b, "headers = %s\n", renderStringMap(pl.Headers))
			}
			if len(pl.Env) > 0 {
				fmt.Fprintf(&b, "env     = %s\n", renderStringMap(pl.Env))
			}
			if pl.StartupTimeoutSeconds > 0 {
				b.WriteString("# Per-server MCP initialize + tools/list timeout; 0 keeps the global/default cap.\n")
				fmt.Fprintf(&b, "startup_timeout_seconds = %d\n", pl.StartupTimeoutSeconds)
			}
			if pl.CallTimeoutSeconds > 0 {
				b.WriteString("# Per-server MCP call timeout; 0 keeps the global/default cap.\n")
				fmt.Fprintf(&b, "call_timeout_seconds = %d\n", pl.CallTimeoutSeconds)
			}
			if hasPositiveIntMap(pl.ToolTimeoutSeconds) {
				b.WriteString("# Raw MCP tool names with per-tool call timeouts.\n")
				fmt.Fprintf(&b, "tool_timeout_seconds = %s\n", renderIntMap(pl.ToolTimeoutSeconds))
			}
			renderPluginPolicy(&b, pl)
		}
	}

	return b.String()
}
