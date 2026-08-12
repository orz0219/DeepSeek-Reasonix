package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/i18n"
	"reasonix/internal/telemetry"
)

func configCommand(args []string) int {
	if len(args) == 0 {
		configUsage()
		return 2
	}
	switch args[0] {
	case "auto-plan":
		return configAutoPlanCompatibilityCommand(args[1:])
	case "reasoning-language":
		return configReasoningLanguageCommand(args[1:])
	case "compact-ratio":
		return configCompactRatioCommand(args[1:])
	case "currency":
		return configCurrencyCommand(args[1:])
	case "telemetry":
		return configTelemetryCommand(args[1:])
	default:
		configUsage()
		return 2
	}
}

func configCurrencyCommand(args []string) int {
	fs := flag.NewFlagSet("config currency", flag.ContinueOnError)
	local := fs.Bool("local", false, "unsupported; pricing currency is user-level only")
	if code, ok := parseCommandFlags(fs, args); !ok {
		return code
	}
	if *local {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "currency is user-level only; --local is not supported")
		return 2
	}
	rest := fs.Args()
	if len(rest) > 1 {
		configCurrencyUsage()
		return 2
	}
	if len(rest) == 0 {
		cfg, err := config.LoadForRootReadOnly(".")
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		fmt.Printf("currency = %q (display: %s)\n", pricingCurrencyDisplay(cfg.DisplayCurrencyPref()), cfg.ResolveDisplayCurrency())
		return 0
	}
	mode, err := parseCLIPricingCurrency(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	path := config.UserConfigPath()
	if path == "" {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "cannot resolve user config path")
		return 1
	}
	unlock := config.LockUserConfigEdits()
	defer unlock()
	cfg := config.LoadForEdit(path)
	if err := cfg.SetDisplayCurrency(mode); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	resolved := cfg.ResolveDisplayCurrency()
	if err := cfg.SaveTo(path); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	fmt.Printf("currency = %q (display: %s, %s)\n", pricingCurrencyDisplay(mode), resolved, displayPath(path))
	return 0
}

func configTelemetryCommand(args []string) int {
	fs := flag.NewFlagSet("config telemetry", flag.ContinueOnError)
	if code, ok := parseCommandFlags(fs, args); !ok {
		return code
	}
	rest := fs.Args()
	if len(rest) > 1 {
		configTelemetryUsage()
		return 2
	}
	if len(rest) == 0 {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		fmt.Printf("cli_metrics = %q\n", cfg.CLITelemetryMode())
		return 0
	}
	path := config.UserConfigPath()
	if path == "" {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "cannot resolve config path")
		return 1
	}
	unlock := config.LockUserConfigEdits()
	defer unlock()
	cfg := config.LoadForEdit(path)
	if err := cfg.SetCLITelemetryMode(rest[0]); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	if err := cfg.SaveTo(path); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	if cfg.CLITelemetryMode() == "off" {
		if err := cleanupCLITelemetry(config.ReasonixHomeDir()); err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "telemetry disabled, but pending metrics could not be deleted:", err)
			return 1
		}
	}
	fmt.Printf("cli_metrics = %q (%s)\n", cfg.CLITelemetryMode(), displayPath(path))
	return 0
}

// configAutoPlanCompatibilityCommand preserves the released shell interface
// without restoring Automatic Plan Mode. Reading and writing "off" are safe
// no-ops; every attempt to enable the retired feature is rejected.
func configAutoPlanCompatibilityCommand(args []string) int {
	fs := flag.NewFlagSet("config auto-plan", flag.ContinueOnError)
	local := fs.Bool("local", false, "unsupported; automatic plan mode is retired")
	if code, ok := parseCommandFlags(fs, args); !ok {
		return code
	}
	if *local {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "auto-plan is user-level only; --local is not supported")
		return 2
	}
	rest := fs.Args()
	if len(rest) > 1 {
		configAutoPlanCompatibilityUsage()
		return 2
	}
	if len(rest) == 0 {
		fmt.Println(`auto_plan = "off"`)
		return 0
	}
	cfg := config.Default()
	if err := cfg.SetAutoPlan(rest[0]); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	fmt.Println(`auto_plan = "off"`)
	return 0
}

func configReasoningLanguageCommand(args []string) int {
	fs := flag.NewFlagSet("config reasoning-language", flag.ContinueOnError)
	local := fs.Bool("local", false, "write ./reasonix.toml instead of the user config")
	if code, ok := parseCommandFlags(fs, args); !ok {
		return code
	}
	rest := fs.Args()
	if len(rest) > 1 {
		configReasoningLanguageUsage()
		return 2
	}
	if len(rest) == 0 {
		cfg, err := config.Load()
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		fmt.Printf("reasoning_language = %q\n", cliReasoningLanguageMode(cfg.ReasoningLanguage()))
		return 0
	}
	mode, err := parseCLIReasoningLanguage(rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	path := config.UserConfigPath()
	if *local {
		path = "reasonix.toml"
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "cannot resolve config path")
		return 1
	}
	unlock, err := config.LockConfigFileEdits(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	defer unlock()
	if *local {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			lang, err := config.SaveMinimalProjectReasoningLanguage(path, mode)
			if err != nil {
				fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
				return 1
			}
			fmt.Printf("reasoning_language = %q (%s)\n", lang, displayPath(path))
			return 0
		} else if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
	}
	cfg, err := config.LoadForEditReadOnlyStrict(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	if err := cfg.SetReasoningLanguage(mode); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	if err := cfg.SaveTo(path); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	fmt.Printf("reasoning_language = %q (%s)\n", cfg.ReasoningLanguage(), displayPath(path))
	return 0
}

func configCompactRatioCommand(args []string) int {
	fs := flag.NewFlagSet("config compact-ratio", flag.ContinueOnError)
	local := fs.Bool("local", false, "write ./reasonix.toml instead of the user config")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) > 1 {
		configCompactRatioUsage()
		return 2
	}
	if len(rest) == 0 {
		cfg, err := config.LoadForRootReadOnly(".")
		if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
		fmt.Printf("compact_ratio = %s (%s)\n", formatCompactRatioPercent(cfg.Agent.CompactRatio), compactRatioSource())
		return 0
	}
	percent, err := strconv.ParseFloat(strings.TrimSpace(rest[0]), 64)
	if err != nil || math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 65 || percent > 85 {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "compact ratio must be a percentage between 65 and 85")
		return 2
	}
	ratio := percent / 100
	path := config.UserConfigPath()
	scope := "user"
	if *local {
		path = "reasonix.toml"
		scope = "project"
	}
	if path == "" {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, "cannot resolve config path")
		return 1
	}
	unlock, err := config.LockConfigFileEdits(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	defer unlock()
	if *local {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			saved, err := config.SaveMinimalProjectCompactRatio(path, ratio)
			if err != nil {
				fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
				return 1
			}
			fmt.Printf("compact_ratio = %s (%s: %s)\n", formatCompactRatioPercent(saved), scope, displayPath(path))
			return 0
		} else if err != nil {
			fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
			return 1
		}
	}
	cfg, err := config.LoadForEditReadOnlyStrict(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	if err := cfg.SetCompactRatio(ratio); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 2
	}
	if err := cfg.SaveTo(path); err != nil {
		fmt.Fprintln(os.Stderr, i18n.M.ErrorPrefix, err)
		return 1
	}
	fmt.Printf("compact_ratio = %s (%s: %s)\n", formatCompactRatioPercent(cfg.Agent.CompactRatio), scope, displayPath(path))
	return 0
}

func compactRatioSource() string {
	if config.ConfigFileDefinesCompactRatio("reasonix.toml") {
		return "project: " + displayPath("reasonix.toml")
	}
	if path := config.UserConfigPath(); path != "" && config.ConfigFileDefinesCompactRatio(path) {
		return "user: " + displayPath(path)
	}
	return "built-in default"
}

func formatCompactRatioPercent(ratio float64) string {
	value := strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", ratio*100), "0"), ".")
	return value + "%"
}

func configUsage() {
	fmt.Print(`Usage:
  reasonix config reasoning-language [--local] [auto|zh|en]
  reasonix config compact-ratio [--local] [65..85]
  reasonix config currency [auto|CNY|USD]
  reasonix config telemetry [auto|on|off]
`)
}

func configTelemetryUsage() {
	fmt.Print(`Usage:
  reasonix config telemetry [auto|on|off]
`)
}

func configCompactRatioUsage() {
	fmt.Print(`Usage:
  reasonix config compact-ratio [--local] [65..85]
`)
}

func startCLITelemetry(cfg *config.Config, opts telemetry.Options) *telemetry.Reporter {
	return startCLITelemetryWithIO(cfg, opts, os.Stdin, os.Stdout, os.Stderr)
}

func startCLITelemetryWithIO(cfg *config.Config, opts telemetry.Options, in io.Reader, out, errOut io.Writer) *telemetry.Reporter {
	if cfg == nil {
		cfg = config.Default()
	}
	opts.Mode = cfg.CLITelemetryMode()
	opts.HomeDir = config.ReasonixHomeDir()
	opts.Proxy = cfg.NetworkProxySpec()
	opts.Language = cfg.Language

	if cfg.CLITelemetryConfigured() || !telemetry.Enabled(opts.Mode, opts.Version, opts.Interactive) {
		return startCLITelemetryReporter(opts)
	}

	fmt.Fprintln(out, i18n.M.CLITelemetryConsentNotice)
	scanner := bufio.NewScanner(in)
	mode := ""
	for mode == "" {
		answer := strings.ToLower(strings.TrimSpace(ask(scanner, out, i18n.M.CLITelemetryConsentPrompt, "Y/n")))
		switch answer {
		case "y", "yes", "y/n":
			mode = "auto"
		case "n", "no":
			mode = "off"
		default:
			fmt.Fprintln(out, i18n.M.CLITelemetryConsentInvalid)
		}
	}

	if err := persistCLITelemetryConsent(mode); err != nil {
		fmt.Fprintf(errOut, i18n.M.CLITelemetryConsentSaveFailedFmt+"\n", err)
		return nil
	}
	cfg.Telemetry.CLIMetrics = mode
	opts.Mode = mode
	if mode == "off" {
		if err := cleanupCLITelemetry(opts.HomeDir); err != nil {
			fmt.Fprintf(errOut, i18n.M.CLITelemetryConsentCleanupFailedFmt+"\n", err)
		}
		return nil
	}
	return startCLITelemetryReporter(opts)
}

func cliTelemetrySessionMode(cont, resume, copySession bool) string {
	switch {
	case copySession:
		return "copy"
	case resume:
		return "resume"
	case cont:
		return "continue"
	default:
		return "fresh"
	}
}

func configAutoPlanCompatibilityUsage() {
	fmt.Print(`Usage:
  reasonix config auto-plan [off]
`)
}

func configReasoningLanguageUsage() {
	fmt.Print(`Usage:
  reasonix config reasoning-language [--local] [auto|zh|en]
`)
}

func configCurrencyUsage() {
	fmt.Print(`Usage:
  reasonix config currency [auto|CNY|USD]
`)
}
