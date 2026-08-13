package cli

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/i18n"
	"reasonix/internal/provider"
	"reasonix/internal/sandbox"
)

// showSandboxStatus displays the current sandbox configuration and whether
// the OS sandbox backend is available. It reads from the stored config so
// the user can inspect sandbox state without leaving the TUI (closes #3316).
func (m *chatTUI) showSandboxStatus() {
	if m.cfg == nil {
		m.notice("sandbox: config not loaded")
		return
	}
	bash := m.cfg.BashMode()
	network := m.cfg.Sandbox.Network
	available := sandbox.Available()
	roots := m.cfg.WriteRoots()

	var b strings.Builder
	b.WriteString("sandbox\n")
	b.WriteString("  phase 0  file-writer confinement\n")
	if len(roots) > 0 {
		fmt.Fprintf(&b, "    write_roots  %s\n", strings.Join(roots, ", "))
	}
	if m.cfg.Sandbox.WorkspaceRoot != "" {
		fmt.Fprintf(&b, "    workspace_root  %s\n", m.cfg.Sandbox.WorkspaceRoot)
	}
	if len(m.cfg.Sandbox.AllowWrite) > 0 {
		fmt.Fprintf(&b, "    allow_write  %s\n", strings.Join(m.cfg.Sandbox.AllowWrite, ", "))
	}
	b.WriteString("  phase 1  OS bash sandbox\n")
	fmt.Fprintf(&b, "    bash        %s", bash)
	if bash == "enforce" && !available {
		b.WriteString(" (unavailable: no OS sandbox on this host; bash execution is refused. " + sandbox.UnavailableRemediation() + ")")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "    network     %v\n", network)
	m.notice(b.String())
}

// runMCPSubcommand handles "/mcp" (status), "/mcp add …" (connect a server live
// and persist it), and "/mcp remove <name>" (disconnect + drop from config). Add
// connects synchronously — like /compact, an explicit command may briefly block
// the UI while the handshake runs.
func (m *chatTUI) runMCPSubcommand(input string) {
	args := tokenizeArgs(input)
	if len(args) < 2 {
		m.openMCPManager("")
		return
	}
	switch args[1] {
	case "list", "ls":

		m.showMCPStatus()
	case "show":
		if len(args) < 3 {
			m.notice("usage: /mcp show <name>")
			return
		}
		m.openMCPManager(args[2])
	case "tools":
		if len(args) < 3 {
			m.notice("usage: /mcp tools <name>")
			return
		}
		m.openMCPManager(args[2])
		if m.mcp != nil {
			m.mcp.stage = mcpStageTools
		}
	case "add":
		entry, err := parseMCPAdd(args[2:])
		if err != nil {
			m.notice(err.Error())
			return
		}
		n, err := m.ctrl.AddMCPServer(entry)
		if err != nil {
			m.notice("mcp add: " + err.Error())
			return
		}
		m.refreshHostAndInvalidateSlashCatalog()
		m.notice(fmt.Sprintf("connected %s — %d tools, saved to global config (available next message)", entry.Name, n))
	case "connect":
		if len(args) < 3 {
			m.notice("usage: /mcp connect <name>")
			return
		}
		n, err := m.ctrl.ConnectConfiguredMCPServer(args[2])
		if err != nil {
			m.notice("mcp connect: " + err.Error())
			return
		}
		m.refreshHostAndInvalidateSlashCatalog()
		m.notice(fmt.Sprintf("connected %s — %d tools (available next message)", args[2], n))
	case "remove", "rm":
		if len(args) < 3 {
			m.notice("usage: /mcp remove <name>")
			return
		}
		name := args[2]
		disconnected, err := m.ctrl.RemoveMCPServer(name)
		if err != nil {
			m.notice("mcp remove: " + err.Error())
			return
		}
		m.refreshHostAndInvalidateSlashCatalog()
		if disconnected {
			m.notice("disconnected " + name + " and removed it from config")
		} else {
			m.notice("removed " + name + " from config")
		}
	case "import":
		m.openMCPImportPicker()
	default:
		m.notice("unknown /mcp subcommand " + args[1] + " — try: /mcp, /mcp list, /mcp show, /mcp add, /mcp connect, /mcp import, /mcp remove")
	}
}

// showMCPStatus queues the connected MCP servers, their counts, and the prompt
// commands / resource refs they expose — the discovery surface for /mcp.
func (m *chatTUI) showMCPStatus() {
	if m.host == nil || (len(m.host.Servers()) == 0 && len(m.host.Failures()) == 0) {
		m.notice(i18n.M.SlashMCPNone)
		return
	}
	m.commitLine(renderMCPStatus(m.width, m.host.Servers(), m.host.Prompts(), m.host.Resources(), m.host.Failures()))
}

// notice queues a dim informational line to scrollback.
func (m *chatTUI) notice(note string) {
	m.commitLine(dim("  · " + note))
}

// showRemoteHosts renders a read-only summary of configured remote hosts. The
// remote session lives in a `reasonix serve` on the remote host, so connecting
// happens from a terminal (`reasonix remote connect`), not inside this chat.
func (m *chatTUI) showRemoteHosts() {
	cfg, err := config.Load()
	if err != nil {
		m.notice(err.Error())
		return
	}
	if len(cfg.Remote.Hosts) == 0 {
		m.notice(i18n.M.RemoteNoHostsHint)
		return
	}
	var b strings.Builder
	for _, h := range cfg.Remote.Hosts {
		target := h.Host
		if h.User != "" {
			target = h.User + "@" + target
		}
		if h.Port != 0 && h.Port != 22 {
			target = fmt.Sprintf("%s:%d", target, h.Port)
		}
		fmt.Fprintf(&b, "  · %s  %s\n", h.Name, target)
	}
	fmt.Fprintf(&b, "  run `reasonix remote connect <name>` in a terminal to open the remote workspace")
	m.commitLine(dim(b.String()))
}

// resolveRefs resolves a line's @references off the event loop via the
// controller, delivering a refsResolvedMsg with the tagged context block.
func (m *chatTUI) resolveRefs(sent, display, restore string) tea.Cmd {
	return func() tea.Msg {
		block, errs := m.ctrl.ResolveRefs(context.Background(), sent)
		return refsResolvedMsg{sent: sent, display: display, restore: restore, block: block, errs: errs}
	}
}

// runMCPPrompt resolves a /mcp__server__prompt command off the event loop via
// the controller, delivering a promptResolvedMsg with the rendered prompt.
func (m *chatTUI) runMCPPrompt(input string) tea.Cmd {
	return func() tea.Msg {
		sent, found, err := m.ctrl.MCPPrompt(context.Background(), input)
		if !found {
			name := strings.TrimPrefix(strings.Fields(input)[0], "/")
			return promptResolvedMsg{display: input, err: fmt.Errorf("%s: /%s", i18n.M.SlashUnknown, name)}
		}
		return promptResolvedMsg{display: input, sent: sent, err: err}
	}
}

// runExtensionAction invokes one extension UI action off the event loop (the
// call is a blocking sidecar round-trip), delivering an extensionActionMsg
// whose message surfaces as a transcript notice.
func (m *chatTUI) runExtensionAction(name string, args map[string]string) tea.Cmd {
	return func() tea.Msg {
		message, err := m.ctrl.InvokeExtensionAction(context.Background(), name, args)
		return extensionActionMsg{message: message, err: err}
	}
}

// replaySectionsFor turns a loaded session into scrollback blocks. Normal tool
// results remain quiet, while interrupted-turn reasoning and tool cards replay
// from provider-excluded LocalOnly records so restart matches the live view.

func replaySectionsForWithAssistantRenderer(
	history []provider.Message,
	width int,
	renderAssistant func(string, int) string,
) []string {
	var out []string
	for _, m := range history {
		if m.LocalOnly {
			if reasoning := strings.TrimSpace(m.ReasoningContent); reasoning != "" {
				out = append(out, dim("  ▎ "+i18n.M.ChatThinking)+"\n"+reasoningBlock(reasoning, width, 0)+"\n\n")
			}
			if body := strings.TrimSpace(m.Content); body != "" {
				out = append(out, renderAssistant(body, width)+"\n\n")
			}
			for _, call := range m.ToolCalls {
				out = append(out, toolCard(call.Name, "", width)+"\n\n")
			}
			if m.InterruptedTurn != nil {
				out = append(out, fmt.Sprintf("  · %s\n\n", interruptedTurnDisplayNotice()))
			}
			continue
		}
		switch m.Role {
		case provider.RoleUser:

			if steerText, isSteer := agent.SteerText(m.Content); isSteer {
				out = append(out, fmt.Sprintf("  ↪ %s\n\n", steerText))
				continue
			}
			content := control.StripComposePrefixes(m.Content)
			out = append(out, renderUserBubble(content, width, false)+"\n\n")
		case provider.RoleAssistant:
			if reasoning := strings.TrimSpace(m.ReasoningContent); reasoning != "" {
				out = append(out, dim("  ▎ "+i18n.M.ChatThinking)+"\n"+reasoningBlock(reasoning, width, 0)+"\n\n")
			}
			body := strings.TrimSpace(m.Content)
			if body != "" {
				out = append(out, renderAssistant(body, width)+"\n\n")
			}
			for _, call := range m.ToolCalls {
				out = append(out, toolCard(call.Name, call.Arguments, width)+"\n\n")
			}
		}
	}
	return out
}
