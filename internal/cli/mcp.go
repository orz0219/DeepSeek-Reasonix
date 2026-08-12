package cli

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"reasonix/internal/config"
)

// mcp.go holds the MCP server-management surface shared by the `reasonix mcp`
// subcommand (config-only; takes effect next session) and the in-chat `/mcp add`
// / `/mcp remove` slash commands (which hot-connect via the controller). Both
// parse arguments through parseMCPAdd so the grammar is identical everywhere.

// parseMCPAdd turns the arguments after "add" into a config.PluginEntry. Grammar:
//
//	<name> [--http URL | --sse URL] [--env K=V]... [--header K=V]... [command [args...]]
//
// A --http/--sse URL makes it a remote server; otherwise the first non-flag token
// (after the name and any --env/--header flags) begins the stdio command, and the
// rest are its args verbatim — so the command keeps its own -flags (e.g. `npx -y
// pkg`). Flag values accept both "--http URL" and "--http=URL" forms.
func parseMCPAdd(args []string) (config.PluginEntry, error) {
	var e config.PluginEntry
	if len(args) == 0 {
		return e, fmt.Errorf("mcp add: missing server name, command, or URL")
	}

	// Simplified forms:
	//   reasonix mcp add -- npx -y chrome-devtools-mcp@latest
	//   reasonix mcp add https://example.com/mcp
	// keep the historical "name command..." form as well.
	if args[0] == "--" {
		if len(args) < 2 {
			return e, fmt.Errorf("mcp add: -- requires a command argv")
		}
		e.Command = args[1]
		e.Args = append([]string(nil), args[2:]...)
		e.Name = defaultMCPNameFromArgv(e.Command, e.Args)
		if e.Name == "" {
			return e, fmt.Errorf("mcp add: could not derive a server name from the command; pass an explicit name")
		}
		return e, nil
	}
	if looksLikeRemoteMCPURL(args[0]) && (len(args) == 1 || strings.HasPrefix(args[1], "-")) {
		e.Name = defaultMCPNameFromURL(args[0])
		e.Type, e.URL = "http", args[0]
		// Allow trailing --header/--env after a bare URL.
		if len(args) > 1 {
			restEntry, err := parseMCPAdd(append([]string{e.Name, "--http", args[0]}, args[1:]...))
			if err != nil {
				return e, err
			}
			return restEntry, nil
		}
		return e, nil
	}

	e.Name = strings.TrimSpace(args[0])
	if e.Name == "" || strings.HasPrefix(e.Name, "-") {
		return e, fmt.Errorf("mcp add: first argument must be the server name, got %q", args[0])
	}
	rest := args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		// reasonix mcp add <name> -- <argv...>
		if len(rest) < 2 {
			return e, fmt.Errorf("mcp add: -- requires a command argv")
		}
		e.Command = rest[1]
		e.Args = append([]string(nil), rest[2:]...)
		return e, nil
	}

	i := 0
	// next consumes the following token as a flag's value (for the "--flag value"
	// form), reporting false when none remains.
	next := func(flag string) (string, error) {
		if i+1 >= len(rest) {
			return "", fmt.Errorf("mcp add: %s needs a value", flag)
		}
		i++
		return rest[i], nil
	}
	setEnv := func(dst *map[string]string, flag, pair string) error {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return fmt.Errorf("mcp add: %s expects KEY=VALUE, got %q", flag, pair)
		}
		if *dst == nil {
			*dst = map[string]string{}
		}
		(*dst)[k] = v
		return nil
	}

	for ; i < len(rest); i++ {
		a := rest[i]
		key, inline, hasInline := strings.Cut(a, "=")
		switch {
		case !strings.HasPrefix(a, "-"):
			// The stdio command and its remaining args, verbatim.
			e.Command = a
			e.Args = append([]string(nil), rest[i+1:]...)
			i = len(rest)
		case key == "--http" || key == "--streamable-http":
			v := inline
			if !hasInline {
				var err error
				if v, err = next(key); err != nil {
					return e, err
				}
			}
			e.Type, e.URL = "http", v
		case key == "--sse":
			v := inline
			if !hasInline {
				var err error
				if v, err = next(key); err != nil {
					return e, err
				}
			}
			e.Type, e.URL = "sse", v
		case key == "--env" || key == "--header":
			pair := inline
			if !hasInline {
				var err error
				if pair, err = next(key); err != nil {
					return e, err
				}
			}
			dst := &e.Env
			if key == "--header" {
				dst = &e.Headers
			}
			if err := setEnv(dst, key, pair); err != nil {
				return e, err
			}
		default:
			return e, fmt.Errorf("mcp add: unknown flag %q", a)
		}
	}

	switch {
	case e.URL != "" && e.Command != "":
		return e, fmt.Errorf("mcp add: specify a command OR a --http/--sse URL, not both")
	case e.URL == "" && e.Command == "":
		return e, fmt.Errorf("mcp add: need a command (stdio) or a --http/--sse URL")
	}
	return e, nil
}

func looksLikeRemoteMCPURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	return strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://")
}

func defaultMCPNameFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "remote-mcp"
	}
	host := strings.ToLower(u.Hostname())
	host = strings.TrimPrefix(host, "www.")
	host = strings.Split(host, ".")[0]
	host = sanitizeMCPName(host)
	if host == "" {
		return "remote-mcp"
	}
	return host
}

func defaultMCPNameFromArgv(command string, args []string) string {
	runner := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(filepath.Base(command), ".exe"), ".cmd"), ".bat"))
	candidate := command
	switch runner {
	case "npx", "bunx", "uvx":
		if operand := firstMCPCommandOperand(args); operand != "" {
			candidate = operand
		}
	case "python", "python3", "py":
		for i, arg := range args {
			if arg == "-m" && i+1 < len(args) {
				candidate = args[i+1]
				break
			}
		}
		if candidate == command {
			if operand := firstMCPCommandOperand(args); operand != "" {
				candidate = operand
			}
		}
	case "node":
		if operand := firstMCPCommandOperand(args); operand != "" {
			candidate = operand
		}
	case "uv":
		if len(args) > 0 && args[0] == "run" {
			if operand := firstMCPCommandOperand(args[1:]); operand != "" {
				candidate = operand
			}
		}
	}
	base := filepath.Base(candidate)
	if at := strings.Index(base, "@"); at > 0 {
		base = base[:at]
	}
	for _, ext := range []string{".js", ".exe", ".cmd", ".bat"} {
		base = strings.TrimSuffix(base, ext)
	}
	name := sanitizeMCPName(base)
	if name == "" {
		return "mcp-server"
	}
	if candidate == command {
		switch runner {
		case "npx", "bunx", "uvx", "uv", "node", "python", "python3", "py":
			return "mcp-server"
		}
	}
	return name
}

func firstMCPCommandOperand(args []string) string {
	valueFlags := map[string]bool{
		"-p": true, "--package": true, "-c": true, "--call": true,
		"--node-options": true, "--python": true,
	}
	options := true
	for i := 0; i < len(args); i++ {
		arg := strings.TrimSpace(args[i])
		if options && arg == "--" {
			options = false
			continue
		}
		if options && strings.HasPrefix(arg, "-") {
			if valueFlags[arg] {
				i++
			}
			continue
		}
		if arg != "" {
			return arg
		}
	}
	return ""
}

func sanitizeMCPName(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	for strings.Contains(name, "--") {
		name = strings.ReplaceAll(name, "--", "-")
	}
	return name
}

// tokenizeArgs splits a slash-command line into arguments, honouring "double" and
// 'single' quotes so values with spaces (e.g. --header "Authorization=Bearer x")
// survive. An unterminated quote takes the rest of the line as one token.
func tokenizeArgs(s string) []string {
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
			inWord = true
		case r == '"' || r == '\'':
			quote = r
			inWord = true
		case r == ' ' || r == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// mcpCommand implements persisted server management plus explicit browse/install
// access to the official MCP Registry. Config edits take effect on the next
// session start; for a live manual connection inside an open chat, use `/mcp add`.

// connect remains a compatibility alias for enable/retry.

// Standalone CLI cannot talk to a live Host; enabling is the durable
// equivalent of "retry next session". In-chat /mcp retry remains live.

// Uninstall clears activation overrides and Reasonix-owned OAuth state. A
// same-resource lower-priority declaration keeps owning the shared state.
