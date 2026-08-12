package cli

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"reasonix/internal/config"
)

func redactMCPConfigValue(key, value string) string {
	if looksSensitiveMCPKey(key) || looksSensitiveMCPValue(value) {
		return "<redacted>"
	}
	return value
}

func looksSensitiveMCPKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	for _, needle := range []string{"auth", "token", "secret", "credential", "api_key", "api-key", "apikey", "cookie"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func looksSensitiveMCPQueryKey(key string) bool {
	return strings.EqualFold(strings.TrimSpace(key), "key") || looksSensitiveMCPKey(key)
}

func looksSensitiveMCPValue(value string) bool {
	lower := strings.ToLower(value)
	for _, needle := range []string{"access_token", "id_token", "refresh_token", "api_key", "api-key", "apikey", "bearer "} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func redactMCPURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return raw
	}
	u, err := url.Parse(trimmed)
	if err != nil || u == nil {
		if looksSensitiveMCPValue(raw) {
			return "<redacted>"
		}
		return raw
	}
	q := u.Query()
	changed := false
	for key := range q {
		if looksSensitiveMCPQueryKey(key) {
			q.Set(key, "<redacted>")
			changed = true
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func mcpAddCLI(args []string) int {
	entry, err := parseMCPAdd(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, configured := range cfg.Plugins {
		if configured.Name == entry.Name {
			fmt.Fprintf(os.Stderr, "MCP server %q is already configured; remove it first or choose another name\n", entry.Name)
			return 1
		}
	}
	result, probeErr := mcpProbeForInstall(entry)
	if probeErr != nil && result.State != "action_required" {
		fmt.Fprintf(os.Stderr, "MCP server %q was not added: %s\n", entry.Name, result.Message)
		return 1
	}
	if err := persistCLIInstalledMCP(mcpCLIWorkspaceRoot(), entry); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if result.State == "action_required" {
		fmt.Printf("added MCP server %q — authentication required; finish authentication and retry\n", entry.Name)
		return 0
	}
	fmt.Printf("added MCP server %q — ready with %d tools\n", entry.Name, result.ToolCount)
	return 0
}

func persistCLIInstalledMCP(workspace string, entry config.PluginEntry) error {
	entry.Source = config.MCPSourceUserConfig
	_, err := config.InstallUserPluginForRoot(workspace, entry, entry.ShouldAutoStart())
	return err
}

func mcpRemoveCLI(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: reasonix mcp remove <name>")
		return 2
	}
	name := args[0]
	workspace := mcpCLIWorkspaceRoot()
	removed, ok, _, err := config.RemovePluginFromEffectiveSourceForRoot(workspace, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "no MCP server named %q in config\n", name)
		return 1
	}

	_ = config.DefaultMCPActivationStore().ClearServer(removed, workspace)
	if err := reconcileRemovedMCPOAuth(workspace, name); err != nil {
		fmt.Fprintf(os.Stderr, "removed MCP server %q, but failed to reconcile OAuth state: %v\n", name, err)
		return 1
	}
	fmt.Printf("removed MCP server %q\n", name)
	return 0
}

func mcpCLIWorkspaceRoot() string {
	if cwd, err := os.Getwd(); err == nil && strings.TrimSpace(cwd) != "" {
		return cwd
	}
	return "."
}

func mcpUsage() {
	fmt.Println(`Manage MCP servers (global installs use config.toml; project entries stay in project config).

Usage:
  reasonix mcp list
  reasonix mcp get <name>
  reasonix mcp install <registry-name> [--as <name>]
  reasonix mcp add -- <command> [args...]            stdio argv (no shell)
  reasonix mcp add <name> -- <command> [args...]
  reasonix mcp add <name> <command> [args...]        legacy stdio form
  reasonix mcp add https://example.com/mcp           remote HTTP
  reasonix mcp add <name> --http <url> [--header K=V]
  reasonix mcp add <name> --sse  <url>
  reasonix mcp enable <name>
  reasonix mcp disable <name>
  reasonix mcp retry <name>
  reasonix mcp auth <name>                          remote OAuth (opens browser)
  reasonix mcp update <name>
  reasonix mcp browse [query] [--limit N] [--json]
  reasonix mcp import
  reasonix mcp remove <name>

Flags for add:
  --http <url> | --sse <url>   remote transport (omit for a stdio command)
  --env K=V                    set an environment variable (repeatable, stdio)
  --header K=V                 set an HTTP header (repeatable, remote)

Examples:
  reasonix mcp add fs npx -y @modelcontextprotocol/server-filesystem .
  reasonix mcp add stripe --http https://mcp.stripe.com --header "Authorization=Bearer $STRIPE_KEY"

CLI config changes take effect on the next session. Inside a running chat, use
/mcp add to save and connect a server immediately. Installing a server is also
its launch authorization; there is no separate trust step. Remote OAuth, when
requested by the server, is completed with reasonix mcp auth <name> and stored
in Reasonix-private MCP state.

Servers declared by project reasonix.toml or .mcp.json are trusted configuration
and need no separate launch confirmation. Project entries override same-name
global entries; within a project, reasonix.toml overrides .mcp.json. Writer or
destructive annotations never trigger per-call approval. Explicit deny rules
still win; Plan Mode and strict read-only subagents may filter which tools are
available.`)
}
