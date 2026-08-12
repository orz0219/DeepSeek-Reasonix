package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/mcpregistry"
)

// mcpCommand implements persisted server management plus explicit browse/install
// access to the official MCP Registry. Config edits take effect on the next
// session start; for a live manual connection inside an open chat, use `/mcp add`.
func mcpCommand(args []string) int {
	if len(args) == 0 {
		mcpUsage()
		return 2
	}
	switch args[0] {
	case "list", "ls":
		return mcpList()
	case "add":
		return mcpAddCLI(args[1:])
	case "get":
		return mcpGetCLI(args[1:])
	case "remove", "rm":
		return mcpRemoveCLI(args[1:])
	case "enable":
		return mcpEnableCLI(args[1:], true)
	case "disable":
		return mcpEnableCLI(args[1:], false)
	case "retry", "connect":

		return mcpRetryCLI(args[1:])
	case "auth", "authorize":
		return mcpAuthCLI(args[1:])
	case "update":
		return mcpUpdateCLI(args[1:])
	case "import":
		return mcpImportCLI()
	case "browse", "search":
		return mcpBrowseCLI(args[1:])
	case "install":
		return mcpInstallCLI(args[1:])
	case "help", "-h", "--help":
		mcpUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown mcp subcommand %q\n\n", args[0])
		mcpUsage()
		return 2
	}
}

func defaultMCPRegistryClient() *mcpregistry.Client {
	cachePath := ""
	if cacheDir := config.CacheDir(); cacheDir != "" {
		cachePath = filepath.Join(cacheDir, "mcp-registry-v0.1.json")
	}
	return mcpregistry.New(cachePath)
}

func mcpBrowseCLI(args []string) int {
	return mcpBrowseWithClient(args, defaultMCPRegistryClient())
}

func mcpBrowseWithClient(args []string, client *mcpregistry.Client) int {
	query := ""
	limit := 20
	jsonOutput := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOutput = true
		case "--limit":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "mcp browse: --limit needs a value")
				return 2
			}
			i++
			value, err := strconv.Atoi(args[i])
			if err != nil || value <= 0 || value > 100 {
				fmt.Fprintln(os.Stderr, "mcp browse: --limit must be between 1 and 100")
				return 2
			}
			limit = value
		default:
			if strings.HasPrefix(args[i], "-") {
				fmt.Fprintf(os.Stderr, "mcp browse: unknown flag %q\n", args[i])
				return 2
			}
			if query != "" {
				fmt.Fprintln(os.Stderr, "mcp browse: provide at most one search query")
				return 2
			}
			query = args[i]
		}
	}
	result, err := client.Search(context.Background(), query, limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if result.Warning != "" {
		fmt.Fprintf(os.Stderr, "MCP Registry unavailable; showing cached results: %s\n", result.Warning)
	}
	if jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result.Entries); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if len(result.Entries) == 0 {
		fmt.Println("no MCP Registry servers matched")
		return 0
	}
	for _, entry := range result.Entries {
		status := entry.Transport
		if !entry.Installable {
			status = "manual setup: " + entry.UnavailableReason
		}
		title := entry.Title
		if title == "" {
			title = entry.Name
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", entry.Name, entry.Version, status, title)
	}
	return 0
}

func mcpInstallCLI(args []string) int {
	return mcpInstallWithClient(args, defaultMCPRegistryClient())
}

func mcpInstallWithClient(args []string, client *mcpregistry.Client) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: reasonix mcp install <registry-name> [--as <local-name>]")
		return 2
	}
	registryName := strings.TrimSpace(args[0])
	if registryName == "" || strings.HasPrefix(registryName, "-") {
		fmt.Fprintln(os.Stderr, "mcp install: registry server name is required")
		return 2
	}
	localName := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--as":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				fmt.Fprintln(os.Stderr, "mcp install: --as needs a local name")
				return 2
			}
			i++
			localName = strings.TrimSpace(args[i])
		default:
			fmt.Fprintf(os.Stderr, "mcp install: unknown argument %q\n", args[i])
			return 2
		}
	}
	entry, result, err := client.Resolve(context.Background(), registryName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if result.Warning != "" {
		fmt.Fprintf(os.Stderr, "MCP Registry unavailable; using cached result: %s\n", result.Warning)
	}
	pluginEntry, err := entry.PluginEntry(localName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, configured := range cfg.Plugins {
		if configured.Name == pluginEntry.Name {
			fmt.Fprintf(os.Stderr, "MCP server %q is already configured; choose another name with --as or remove it first\n", pluginEntry.Name)
			return 1
		}
	}
	installResult, probeErr := mcpProbeForInstall(pluginEntry)
	if probeErr != nil && installResult.State != "action_required" {
		fmt.Fprintf(os.Stderr, "MCP server %q was not installed: %s\n", pluginEntry.Name, installResult.Message)
		return 1
	}
	if err := persistCLIInstalledMCP(mcpCLIWorkspaceRoot(), pluginEntry); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if installResult.State == "action_required" {
		fmt.Printf("installed MCP Registry server %q as %q — authentication required; finish authentication and run `reasonix mcp retry %s`\n", entry.Name, pluginEntry.Name, pluginEntry.Name)
		return 0
	}
	fmt.Printf("installed MCP Registry server %q as %q — ready with %d tools\n", entry.Name, pluginEntry.Name, installResult.ToolCount)
	return 0
}

func mcpEnableCLI(args []string, enabled bool) int {
	if len(args) == 0 {
		action := "enable"
		if !enabled {
			action = "disable"
		}
		fmt.Fprintf(os.Stderr, "usage: reasonix mcp %s <name>\n", action)
		return 2
	}
	name := strings.TrimSpace(args[0])
	workspace := mcpCLIWorkspaceRoot()
	cfg, err := config.LoadForRoot(workspace)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var entry config.PluginEntry
	found := false
	for _, p := range cfg.Plugins {
		if p.Name == name {
			entry = p
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "no MCP server named %q in config\n", name)
		return 1
	}
	store := config.DefaultMCPActivationStore()
	if err := store.SetServerEnabled(entry, workspace, enabled); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if enabled {
		fmt.Printf("enabled MCP server %q — tools restore from cache; process starts on first call\n", name)
	} else {
		fmt.Printf("disabled MCP server %q — tools removed from the catalog; authorization retained\n", name)
	}
	return 0
}

func mcpRetryCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: reasonix mcp retry <name>")
		return 2
	}

	return mcpEnableCLI(args, true)
}

func mcpUpdateCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: reasonix mcp update <name>")
		return 2
	}
	name := strings.TrimSpace(args[0])
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var entry config.PluginEntry
	found := false
	for _, configured := range cfg.Plugins {
		if configured.Name == name {
			entry, found = configured, true
			break
		}
	}
	if !found {
		fmt.Fprintf(os.Stderr, "no MCP server named %q in config\n", name)
		return 1
	}
	result, probeErr := mcpProbeForInstall(entry)
	if probeErr != nil {
		fmt.Fprintf(os.Stderr, "MCP update for %q was not applied: %s\n", name, result.Message)
		return 1
	}
	fmt.Printf("updated MCP server %q — candidate handshake passed with %d tools; cached schema switched atomically\n", name, result.ToolCount)
	return 0
}

func mcpImportCLI() int {
	total, added, updated, err := config.ImportCCSwitchMCP()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("imported %d MCP servers from cc-switch (%d added, %d updated) — servers load on the next session\n", total, added, updated)
	return 0
}

func mcpList() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	listed := 0
	for _, p := range cfg.Plugins {
		typ := p.Type
		if typ == "" {
			typ = "stdio"
		}
		auto := ""
		if !p.ShouldAutoStart() {
			auto = " [auto_start=false]"
		}
		if typ == "stdio" {
			line := strings.TrimSpace(p.Command + " " + strings.Join(p.Args, " "))
			fmt.Printf("%-16s (stdio)%s  %s\n", p.Name, auto, line)
		} else {
			fmt.Printf("%-16s (%s)%s  %s\n", p.Name, typ, auto, p.URL)
		}
		listed++
	}
	if listed == 0 {
		fmt.Println("no MCP servers configured")
	}
	return 0
}

func mcpGetCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: reasonix mcp get <name>")
		return 2
	}
	name := args[0]
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, p := range cfg.Plugins {
		if p.Name != name {
			continue
		}
		printMCPEntry(p)
		return 0
	}
	fmt.Fprintf(os.Stderr, "no MCP server named %q in config\n", name)
	return 1
}

func printMCPEntry(p config.PluginEntry) {
	typ := p.Type
	if typ == "" {
		typ = "stdio"
	}
	fmt.Printf("name: %s\n", p.Name)
	fmt.Printf("type: %s\n", typ)
	if typ == "stdio" {
		fmt.Printf("command: %s\n", p.Command)
		if len(p.Args) > 0 {
			fmt.Printf("args: %s\n", strings.Join(p.Args, "\n      "))
		}
		if len(p.Env) > 0 {
			fmt.Println("env:")
			for _, k := range sortedMapKeys(p.Env) {
				fmt.Printf("  %s=%s\n", k, redactMCPConfigValue(k, p.Env[k]))
			}
		}
	} else {
		fmt.Printf("url: %s\n", redactMCPURL(p.URL))
		if len(p.Headers) > 0 {
			fmt.Println("headers:")
			for _, k := range sortedMapKeys(p.Headers) {
				fmt.Printf("  %s=%s\n", k, redactMCPConfigValue(k, p.Headers[k]))
			}
		}
	}
	if !p.ShouldAutoStart() {
		fmt.Println("auto_start: false")
	}
}

func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
