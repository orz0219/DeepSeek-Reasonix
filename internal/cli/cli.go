// Package cli implements reasonix's command-line entry: subcommand routing, flag
// parsing, assembly from config, and exit codes. The core is config-driven —
// providers and tools are resolved from configuration, not hardcoded.
package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/i18n"
	"reasonix/internal/telemetry"
)

var (
	runInteractiveSession = chatREPL
	cliIsInteractive      = isInteractive
	runWebCommand         = runWeb
	openBrowserURL        = openInBrowser
)

// Run is the CLI entry point; it returns a process exit code.
// Prefer RunWithBuildInfo when git commit / build time are available from ldflags.
func Run(args []string, version string) int {
	return RunWithBuildInfo(args, BuildInfo{Version: version})
}

// RunWithBuildInfo is the full CLI entry with optional build metadata for
// `reasonix version --verbose` / `--json`.
func RunWithBuildInfo(args []string, info BuildInfo) int {
	info = info.withDefaults()
	version := info.Version
	// Usage recording is asynchronous so provider/UI paths never wait on disk.
	// Drain accepted records and fence the projection worker before returning.
	// An embedded Run may outlive one invocation and remove its CacheDir.
	defer closeCLIUsageCatalogs()
	// Pick the UI language up front so even pre-config paths (the first-run
	// welcome banner) come through localized. Env-only first; if a config
	// exists and pins a language, that wins.
	i18n.DetectLanguage("")
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	if cmd == "--acp" {
		cmd = "acp"
	}
	// -p/--print is one-shot print mode. reasonix has no interactive -p, so a
	// print flag anywhere in a leading flag run (no explicit subcommand) routes
	// the whole set to `run --print` — `reasonix --model X -p "task"` works, not
	// only `reasonix -p ...`.
	if cmd == "-p" || cmd == "--print" || (isDefaultInteractiveFlag(cmd) && hasLeadingPrintFlag(args)) {
		args = append([]string{"run", "--print"}, stripLeadingPrintFlag(args)...)
		cmd = "run"
	}
	if len(args) > 0 && isDefaultInteractiveFlag(cmd) {
		cmd = ""
	}
	doctorRepair := isDoctorRepairCommand(args)
	if shouldMigrateLegacyConfigForCLI(cmd) && !doctorRepair {
		migrateLegacyConfigForCLI()
	}
	if !doctorRepair {
		if cfg, err := config.Load(); err == nil {
			if cfg.Language != "" {
				i18n.DetectLanguage(cfg.Language)
			}
		}
	}

	if len(args) == 0 && cliIsInteractive() {
		return runInteractiveSession(nil, version)
	}
	if len(args) == 0 {
		configureCLIThemeFromConfigForTTYOutput()
		usage()
		return 0
	}
	if cmd == "" {
		return runInteractiveSession(args, version)
	}

	rest := args[1:]
	switch cmd {
	case "run":
		return runAgent(rest, version)
	case "chat", "code": // "code" is the v0.x name for the interactive session
		return runInteractiveSession(rest, version)
	case "serve":
		return runServe(rest)
	case "web":
		return runWebCommand(rest)
	case "setup":
		configureCLIThemeFromConfigForTTYOutput()
		return setupConfig(rest)
	case "config":
		configureCLIThemeFromConfig()
		return configCommand(rest)
	case "init":
		// Project memory (AGENTS.md) is model-generated in-session — `/init` runs
		// the codebase analysis. This CLI entry just points there (and to `setup`
		// for config), so `reasonix init` isn't a dead end.
		configureCLIThemeFromConfig()
		return initHint()
	case "acp":
		configureCLIThemeFromConfig()
		return acpCommand(rest, version)
	case "mcp":
		configureCLIThemeFromConfig()
		return mcpCommand(rest)
	case "remote":
		configureCLIThemeFromConfig()
		return remoteCommand(rest, version)
	case "plugin":
		configureCLIThemeFromConfig()
		return pluginCommand(rest)
	case "subagent":
		configureCLIThemeFromConfigForTTYOutput()
		return subagentCommand(rest)
	case "doctor":
		if !doctorRepair {
			configureCLIThemeFromConfig()
		}
		return doctorCommand(rest, version)
	case "report":
		configureCLIThemeFromConfig()
		return reportCommand(rest)
	case "session", "sessions", "catalogs":
		return runSessionOrCatalogCommand(cmd, rest)
	case "hook", "hooks":
		configureCLIThemeFromConfig()
		return hookCommand(rest)
	case "task":
		configureCLIThemeFromConfig()
		return taskCommand(rest)
	case "review":
		configureCLIThemeFromConfig()
		return reviewCommand(rest)
	case "bot":
		configureCLIThemeFromConfig()
		return botCommand(rest, version)
	case "upgrade", "update":
		configureCLIThemeFromConfig()
		return upgradeCommand(rest, version)
	case "version":
		// Detailed identity: version --verbose / --json. Top-level --version/-v
		// stay single-line for script compatibility (Integration D/E).
		return versionCommand(rest, info, true)
	case "--version", "-v":
		return versionCommand(nil, info, false)
	case "completion":
		return completionCommand(rest)
	case "docs-manifest":
		return docsManifestCommand(rest, version)
	case "help", "--help", "-h":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, i18n.M.UnknownCommandFmt+"\n\n", cmd)
		usage()
		return 2
	}
}

// setupProfile builds a ready-to-drive Controller from config via boot.Build.
// The assembly (model resolution, tool registry, permission gate, two-model
// Coordinator) lives in internal/boot, shared with the desktop frontend.
// requireKey forces the executor's API key to be present (used by run); chat
// passes false so the session UI is reachable before a key is set. sink receives
// the agent's typed event stream — runAgent passes a TextSink that renders to
// stdout, the TUI passes an event-channel sink so events become tea.Msgs.
// profile selects economy|balanced|delivery (empty = balanced/full).
// workspaceRoot pins the project root explicitly (from --dir); empty falls back
// to git-root detection.

// SessionTemp carries the previous Controller's private temporary directory
// manager across model/profile rebuilds so temporary files survive.

// sessionTempFromCLIController returns the logical-session private temporary
// directory manager for a same-session CLI controller rebuild. Nil keeps fresh
// builds on control.New's normal new-manager path.

// profile is dual-write TokenMode; also set AgentPreset for the new path.

// resolveCLISessionDir returns the session dir for CLI invocations. When the
// current working directory maps to a project session dir, the project dir is
// used so /resume shows project history. Falls back to the global session dir.

// setupQuietProfile is like setupProfile but guarantees plugin subprocess
// stderr stays off the terminal. Interactive callers provide the private TUI
// diagnostic writer; other callers fall back to io.Discard.

// Accept both --preset light|balanced|delivery and legacy --profile
// economy|full|delivery. Returns dual-write TokenMode values.

// chdirTo honours --dir: it switches the working directory before anything reads
// it, so config discovery, the sandbox root, and file tools all resolve from the
// chosen project root. Returns 2 (already reported) on failure, 0 otherwise.

// workspaceRootForDir returns the explicit project root to pin when --dir was
// given. It runs after chdirTo has already switched into dir, so the process
// working directory is the resolved root. An empty dir means no override (fall
// back to git-root detection). A Getwd failure is returned rather than swallowed:
// silently reverting to "" would re-trigger git-root/default resolution and break
// the explicit --dir guarantee, so the caller must fail loudly instead.

// withNotifications adds system notifications to CLI event streams when configured.

// registerContinueFlag registers --continue with its -c shorthand. The
// shorthand must go through BoolP (pflag shorthand), not BoolVar: BoolVar
// registers "c" as a long flag name, which leaves "-c" unparseable
// ("unknown shorthand flag: 'c' in -c") while accidentally accepting "--c".

// Resolve the resume target up front so --copy and the session lease can be
// handled before any heavy assembly. --resume takes precedence over
// --continue, matching the Resume call below. Accept file paths, branch
// IDs, preview text, and opaque machine session IDs (#7429).

// Keep structured (json/stream-json) and --print stdout a single
// machine-readable payload: the human copy notice goes to stderr there.
// Plain text runs keep it on stdout, where callers scrape the copied path.

// Own the session file for the lifetime of this run so a desktop window (or
// another CLI) writing the same session is refused up front instead of
// silently double-writing. Released after the controller closes.

// `reasonix run` is headless: there is no key loop to answer approval or ask
// prompts, and the approval timeout defaults to infinite. Installing the
// interactive approver/asker here would let an Ask rule, the `ask` tool, or a
// sandbox/config approval wedge the run forever. Map the mode onto a
// non-blocking headless gate instead — passed into boot.Build so every
// headless-only gate it constructs (task/read_only_task, writer-capable
// skill sub-agents, the planner runner) gets the same contract as the parent
// executor, not just the top-level one. Default/ask fails closed because no
// UI can answer; unattended writes require explicit --auto/-y,
// --permission-mode auto, or yolo.

// --resume: load a specific session file (non-interactive, meant for
// MCP/API callers that manage their own per-project session). Takes
// precedence over --continue.
// --continue: resume the most recent saved session.

// Fresh sessions take the lease too (defensive: the path is brand new); a
// resumed path is already held, making this a no-op.

// Snapshot under the sink's lock: a background job can still be emitting
// into it while this goroutine assembles the final record.

// --hash-password: generate a bcrypt hash and exit.

// Build serve config, merging CLI flags over config file.

// `reasonix web` is a local browser entry point and defaults to a freshly
// generated token. `reasonix serve` keeps its existing config-driven default,
// and an explicit --auth always wins for both commands.

// Hash the password at startup so the config never stores plaintext.
// If a PasswordHash is already set in config, the CLI password overrides it.

// Own the active session file for the server's lifetime; the serve
// handlers that rebind sessions (/resume, /new, /fork) move the lease
// through the same keeper. Released after the controller closes.

// Serve always resolves an implicit model from the user-global config,
// ignoring project-level default_model overrides. Explicit flags and
// resumable session models remain strict and are preserved verbatim.

// Keep the browser reachable when the selected provider has no saved key.
// The loopback-only provider setup surface stores the missing credential and
// rebuilds this controller in place before the normal web UI is exposed.

// Auto-save target: reuse the resumed file, else a fresh one — same as chat.

// Fresh sessions take the lease too (defensive: the path is brand new); a
// resumed path is already held, making this a no-op.

// same live keeper was bound above

// chatREPL is an interactive session: a single persistent agent/session and a
// prompt loop that keeps conversation context across turns. Exit with
// 'exit'/'quit' or Ctrl-D.

// Bubble Tea owns the terminal from the resume picker through controller
// shutdown. Start diagnostics before config/controller work so hangs leave a
// non-zero log with milestones (#7435, #7507).

// Decide whether we're starting fresh or resuming. --resume opens an
// interactive picker; --continue / -c jumps straight into the newest.

// Own the active session file for the TUI's lifetime; in-TUI switches
// (/resume, /switch, /new, ...) move the lease with the active path.
// Refusing a held resume target up front is what keeps a desktop window
// and this chat from silently double-writing one transcript.

// Plumb the controller's typed event stream through a channel so each event
// can become a tea.Msg inside the TUI's update loop. Buffered generously:
// streaming bursts (tool results, long answers) shouldn't backpressure the
// agent goroutine.

// True first run whose default model can't resolve: guide setup, then retry.
// With a config present, fall through to the descriptive error — re-running
// the wizard would overwrite the user's config (#2856).

// Decide where this conversation's auto-save lands. A resume reuses the
// file so closing/reopening keeps appending to the same history; a fresh
// session lands in a new file stamped with the model name.

// Fresh sessions take the lease too (defensive: the path is brand new); a
// resumed path is already held, making this a no-op.

// Surface a missing-key warning inside the TUI banner so the first message
// failing is at least pre-announced; the user can still enter chat.
// resolveModelForCLI transparently falls through a keyless default to the
// next configured provider (issue #6996). Validating the final ref is a
// no-op for that configured fallback and preserves the warning when every
// eligible chat provider is still keyless.

// Plugin-namespaced refs hold no config credential; boot's merged
// resolver already gated them, and there is no key env to warn about.

// Initial terminal width — the TUI re-flows on every WindowSizeMsg so
// this is just a starting estimate before the first resize event lands.

// Route "ask" decisions to the TUI: the controller emits an ApprovalRequest
// event and blocks until the user answers via ctrl.Approve. Sub-agents (the
// task tool) keep their headless gate from setup — no UI to prompt through.

// YOLO: skip ordinary tool approval requests for the session (deny rules and
// fresh reviews still apply; ask questions and plan approvals still wait).

// shown as the active entry in /output-style
// custom status-line command, "" = built-in row
// /verbose persistence: start with config default
// retain usage accounting even when transcript receipts are hidden

// /model support: a pure builder the TUI calls to rebuild on a different
// model (carrying the conversation). It must NOT touch the running model —
// runModelSubcommand performs the swap on the live copy. The same stable sink
// feeds the new controller, so events keep flowing to this TUI.

// Keep the logical-session private temporary directory across model /
// profile switches (Issue #7575).

// Keep the carried conversation in its existing file so the switch doesn't
// orphan a duplicate (#2807).

// /reload support: rebuild the runtime through boot.Rebuild so tools,
// skills, commands, hooks, MCP servers, and providers are discovered fresh
// while the boot layer migrates the session (history, approval grants,
// goal/recovery state, lifecycle). Same construction inputs as
// buildController so the replacement matches this session's launch wiring;
// the CLI holds no SharedHost, so each rebuild owns its plugin host.

// Non-Termux terminals use an alt-screen transcript viewport. Termux stays
// in the normal buffer so native touch scrollback and soft-keyboard focus
// keep working; finalized transcript lines are emitted via tea.Println.

// SSH drop (SIGHUP) or service stop (SIGTERM): persist the conversation
// before the terminal goes away, then unwind through the normal close path
// so resume picks up the interrupted session (#3772).

// Close the active controller plus any retired ones from /model switches.
// Retired controllers were stashed rather than closed at switch time
// because Controller.Close() runs SessionEnd hooks and kills plugin
// subprocesses — operations that corrupt bubbletea's terminal raw mode
// when executed while the TUI is alive.

// The Web runtime resumes a materialized TUI transcript or binds the exact
// reserved identity for a never-used session. Release the TUI lease before
// rebuilding the controller or the handoff would correctly reject its own
// session as already in use. The deferred Release remains as a harmless
// final guard for every other return path.

// adoptCarriedHistoryPreservingProfileAndGrants resumes c on the carried
// conversation the way buildController's callers expect: the freshly built
// c already has its own leading system message for the target profile, but
// AdoptHistory below would otherwise replace the
// whole history — including that message — with carry's outgoing one, so the
// switch splices the new leading message in first. It also carries forward
// oldCtrl's same-session "Allow for this session" tool grants and Plan-mode
// read-only command trust, which a rebuild would otherwise silently drop,
// forcing the user to re-approve things already granted this session.

// Persist the adopted history now: the splice above only refreshed the new
// controller's memory and nothing saves again until the next turn ends, so
// quitting right after the switch and resuming would otherwise revive the
// outgoing profile's contract from disk.

// Clear the terminal's scrollback history so a reopened chat starts
// with a clean slate (Termux stays in the normal buffer, so prior
// output would otherwise remain visible above the banner).

// setupTargets is where the wizard writes: the TOML config and the credential
// store. Keys always go to Reasonix's global .env so they
// never land in a project's own .env; only the config location is project-local
// under --local.

// defaultConfigTarget is the user-global config file, falling back to a
// project-local reasonix.toml only when the user config dir can't be resolved.

// defaultEnvTarget is the display target for the reasonix-owned global
// Reasonix global .env.

// resolveSetupTargets picks where `reasonix setup` writes. Keys always go to the
// global env. The config goes to the user-global dir by default, to ./reasonix.toml
// under --local, or to an explicit path argument when given.

// displayPath shortens a home-relative path to ~/… for readable wizard output.

// setupConfig runs the configuration wizard (the `reasonix setup` command),
// writing config.toml to the user-global dir (or ./reasonix.toml under --local)
// and API keys to Reasonix's global .env — never a project's own .env.
// Project memory is a separate concern — the in-session `/init` skill generates
// AGENTS.md (see initHint).

// Non-interactive must not clobber an existing config silently. On a TTY,
// setup is a non-destructive configuration manager, so opening an existing
// file no longer needs an overwrite confirmation.

// Interactive wizard on a TTY; fall back to the annotated default when piped.

// initHint handles `reasonix init`. Unlike a config scaffold, project memory is
// model-generated by analyzing the codebase, so it lives as the in-session
// `/init` skill rather than a CLI command. This entry just points the user there
// (and to `reasonix setup` for config) so the verb isn't a dead end.

// interactiveSetup opens the staged provider manager. Nothing is written until
// the user explicitly chooses Save and exit; q/Ctrl-C leaves both config and
// credentials untouched.

// Seed from the existing config when reconfiguring, so a re-run to fix a key
// preserves the user's providers / agent settings instead of resetting to
// defaults. First run (no file) falls back to the built-in defaults.

// Now that the catalogue matches the user's choice, show the welcome banner
// in their language before any substantive prompt.

// pickSessionToResume scans the session dir, takes the 10 most recent, and
// shows a single-choice menu with timestamp + turn count + first user
// message so the user can pick one. Returns the chosen path and a process
// exit code (non-zero when there's nothing to pick or the user cancelled).

// selectLanguage is the wizard's first prompt: it shows the two UI languages
// in their native form and pre-selects the env-detected one (so a single Enter
// confirms the auto-detection, a single arrow + Enter picks the other). The
// label is bilingual because we don't yet know which catalogue to trust.

// familyStaticModels unions the preset model lists of every entry in the family,
// preserving order and dropping duplicates. It is the fallback offered when the
// live /models probe fails, so a family with separate flash/pro preset entries
// still surfaces both rather than only the first member's model.

// fetchOrFallback tries the OpenAI-compatible GET /models endpoint
// (honoring the entry's ModelsURL when set) and returns the live model IDs.
// On any failure — no base URL, no key set yet (the key is collected in a
// later wizard step), network/auth error, or a vendor without /models — it
// silently returns the preset's static model list so the wizard can always
// present something. The fetch has a 10s timeout and is best-effort.

// fetchModelListCompat walks the full set of model-list URL candidates a given
// base URL can resolve to (root, /v1, known OpenAI/Anthropic compat suffixes)
// and returns the first successful fetch. This is the wizard-time probe for a
// *user-supplied* custom provider — its baseURL is whatever the user pasted,
// and "whatever they pasted" might be https://x.com (root, probe /v1/models)
// or https://x.com/v1 (versioned, probe /v1/models directly). Previously the
// wizard hardcoded `baseURL + "/models"`, which works for OpenAI-shape URLs
// but silently fails for Anthropic-shape roots and the reverse — so the
// wizard's idea of "what models exist" diverged from the chat client's actual
// endpoint. Returning the empty slice (not an error) on full miss lets the
// wizard fall through to a manual text input without an error message.

// buildFamilyEntry returns a single ProviderEntry exposing the user's
// selected models under one entry. It preserves the preset's API key env,
// base URL, kind, context window, pricing, and effort — the things that
// vary per vendor but not per model. The Default pointer is reset to the
// first selected model if it would otherwise reference a model the user
// didn't pick (or was empty).
// buildFamilyEntries splits the user's selection back across the family's preset
// members so each model keeps its own entry — and therefore its own pricing,
// context window, and balance URL. A family like DeepSeek ships flash and pro as
// separate presets with different prices; collapsing them into one entry would
// bill pro at flash's rate. Models the live /models list returned that match no
// preset (a new SKU) fall under the probe entry. Member order is preserved;
// within a member, selection order is preserved.

// filterStaleCustomEntries drops the wizard's own magic-name entries
// (Name="custom" with Kind="openai" or Name="anthropic" with Kind="anthropic")
// that older versions of the wizard wrote into reasonix.toml. They collide
// with the wizard's "custom" / "anthropic" menu items on re-run, showing up
// as duplicate broken entries. The new wizard writes host-derived slugs
// (e.g. "custom-token-sensenova-cn") so a hit on the magic name is
// unambiguously stale. The returned slice is the dropped set so the caller
// can warn the user to clean up reasonix.toml by hand.

// providerSlug derives a stable, human-readable entry name for a custom
// OpenAI / Anthropic-compatible provider from its base URL, e.g.
// "custom-token-sensenova-cn" or "anthropic-api-anthropic-com". We can't
// reuse the wizard's menu-item labels ("custom" / "anthropic") because
// those would collide with the menu item itself and end up rendered as
// duplicate provider entries on subsequent re-runs of `reasonix setup`.
// The host-based slug also gives users a meaningful name to grep for in
// reasonix.toml. Falls back to a short sha1 of the raw URL when the URL
// doesn't parse, so even malformed input still produces a unique name.

// providerFamily is a wizard-only grouping of provider SKUs by vendor; it does
// not exist in config because users editing reasonix.toml deal with SKU names
// directly.

// promptCustomProvider handles the custom provider entry flow.

// promptCustomProviderManual handles manual model entry.

// promptCustomProviderManualWith is the shared backend for manual entry.
// Pre-filled values (baseURL, keyEnv, apiKey) are reused as-is when non-empty
// so the URL-fetch flow can fall through to manual entry without re-asking
// the user for information they've already typed. An empty apiKey is allowed
// — the key step happens later in the wizard and Reasonix's global .env is updated then.

// promptCustomProviderFromURL tries the OpenAI-compatible GET /models
// endpoint and shows a checkbox of the returned models. If the call fails
// (network error, auth failure, or a vendor without /models) it falls
// through to manual entry, reusing the URL and key the user already typed.

// promptAnthropicProvider handles the Anthropic compatible provider entry flow.

// promptAnthropicProviderManual handles manual model entry.

// promptAnthropicProviderManualWith is the shared backend for manual entry
// of an Anthropic-compatible custom provider. Pre-filled values (baseURL,
// keyEnv, apiKey) are reused as-is when non-empty so the URL-fetch flow
// can fall through to manual entry without re-asking the user.

// promptAnthropicProviderFromURL tries the OpenAI-compatible GET /models
// endpoint (some Anthropic-compatible proxies do expose one). Most don't
// — Anthropic's own API has no public model list — so on any failure the
// flow falls through to manual entry with the URL/key already filled in,
// rather than aborting the wizard.

// withBuiltinFamilies guarantees the wizard always offers the built-in DeepSeek
// family even when the loaded config replaced the defaults.
// Built-in entries whose exact name already exists in the user's config are
// kept as-is (preserving customizations); missing built-in entries within an
// existing family are appended so the model picker always shows the full
// catalogue rather than only the previously selected subset.

// providersWithMissingKeys returns the providers the active configuration
// actually references (default/planner/subagent models) whose api_key_env is
// declared but not set. Merely-available providers stay silent; the chat banner
// still warns if users later switch to a model whose key is missing.
// configureKeys dedupes shared envs, so duplicates are fine to leave in.

// configureKeys reconciles each enabled provider's API key with the
// environment. For every distinct api_key_env: if the variable is already set,
// setup asks whether to re-enter it; Enter keeps and re-pins the existing value.
// Otherwise the user is asked once per env var (deduped across providers that
// share one, e.g. both DeepSeek models). Returns KEY=value lines for the
// Reasonix global .env. Re-pinning keeps hand-edited or previously saved values
// aligned with the user's latest setup choice.

// ask prints a prompt to w and returns the entered line, or def if input is empty.

// isInteractive reports whether we're attached to a real terminal on both
// stdin and stdout — required for prompting. Redirected or piped I/O is not
// interactive, so wizards never block or auto-default in scripts and CI.

// appendEnv merges KEY=value lines into a .env file. Existing assignments of
// any key that's about to be written are dropped first, then the new values
// are appended — so re-running `reasonix setup` with a corrected key replaces the
// stale one instead of stacking duplicates. The new values are also
// pinned into the current process env so a chat session started right after
// init picks up the fresh keys without a restart.

// strings.Split on a string ending with \n leaves a trailing empty
// element; trim it so we don't grow a blank line on every rewrite.

// readStdin reads piped input if present; an interactive terminal yields "".

var (
	cleanupCLITelemetry        = telemetry.Cleanup
	startCLITelemetryReporter  = telemetry.Start
	persistCLITelemetryConsent = func(mode string) error {
		path := config.UserConfigPath()
		if strings.TrimSpace(path) == "" {
			return errors.New("cannot resolve config path")
		}
		unlock := config.LockUserConfigEdits()
		defer unlock()
		cfg, err := config.LoadForEditReadOnlyStrict(path)
		if err != nil {
			return err
		}
		if err := cfg.SetCLITelemetryMode(mode); err != nil {
			return err
		}
		return cfg.SaveTo(path)
	}
)

// configAutoPlanCompatibilityCommand preserves the released shell interface
// without restoring Automatic Plan Mode. Reading and writing "off" are safe
// no-ops; every attempt to enable the retired feature is rejected.
