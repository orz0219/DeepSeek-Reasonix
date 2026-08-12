package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"reasonix/internal/config"
	"reasonix/internal/remote"
	"reasonix/internal/remote/forward"
)

func desktopCLIBinaryPath() string {
	packagedName, commandName := desktopCLIBinaryNames(runtime.GOOS)
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates, filepath.Join(dir, packagedName))
	}
	if found, err := exec.LookPath(commandName); err == nil {
		candidates = append(candidates, found)
	}
	for _, candidate := range candidates {
		st, err := os.Stat(candidate)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if runtime.GOOS != "windows" && st.Mode().Perm()&0o111 == 0 {
			continue
		}
		return candidate
	}
	return ""
}

func desktopCLIBinaryNames(goos string) (packaged, command string) {
	if goos == "windows" {
		return "reasonix-cli.exe", "reasonix.exe"
	}
	return "reasonix", "reasonix"
}

func desktopNormalizeBind(bind string) string {
	bind = strings.TrimSpace(bind)
	if !strings.Contains(bind, ":") {
		return net.JoinHostPort("127.0.0.1", bind)
	}
	return bind
}

func preserveRemoteHostHiddenFields(entry *config.RemoteHostEntry, existing config.RemoteHostEntry) {
	entry.PassphraseEnv = existing.PassphraseEnv
	entry.PasswordEnv = existing.PasswordEnv
	entry.Forwards = append([]config.RemoteForwardEntry(nil), existing.Forwards...)
}

// Importing an already-managed SSH alias refreshes only its OpenSSH lookup
// fields. Reasonix-specific workspace and bootstrap policy remain user-owned.
func preserveRemoteHostImportSettings(entry *config.RemoteHostEntry, existing config.RemoteHostEntry) {
	entry.Workspace = existing.Workspace
	entry.ServeInstall = existing.ServeInstall
}

// applyRemoteCredentialInput maps plaintext received from the one-shot Wails
// call into Reasonix-owned credential slots. Blank fields preserve the current
// reference; explicit clear flags remove only slots that this desktop created.
func applyRemoteCredentialInput(entry *config.RemoteHostEntry, in RemoteHostInput) (changes []config.CredentialChange, removalCandidates []string) {
	if in.ClearPassword {
		if config.IsGeneratedRemoteCredential(entry.Name, entry.PasswordEnv) {
			removalCandidates = append(removalCandidates, entry.PasswordEnv)
		}
		entry.PasswordEnv = ""
	}
	if in.Password != "" {
		entry.PasswordEnv = config.RemotePasswordCredentialEnvName(entry.Name)
		changes = append(changes, config.CredentialChange{Key: entry.PasswordEnv, Value: in.Password})
	}

	if in.ClearPassphrase {
		if config.IsGeneratedRemoteCredential(entry.Name, entry.PassphraseEnv) {
			removalCandidates = append(removalCandidates, entry.PassphraseEnv)
		}
		entry.PassphraseEnv = ""
	}
	if in.KeyPassphrase != "" {
		entry.PassphraseEnv = config.RemotePassphraseCredentialEnvName(entry.Name)
		changes = append(changes, config.CredentialChange{Key: entry.PassphraseEnv, Value: in.KeyPassphrase})
	}
	return changes, removalCandidates
}

func hostEntryToView(h config.RemoteHostEntry) RemoteHostView {
	return RemoteHostView{
		ID: h.Name, Label: h.Name, Host: h.Host, Port: h.Port, User: h.User,
		IdentityFile: h.IdentityFile, ProxyJump: h.ProxyJump,
		DefaultWorkspace: h.Workspace, ServeInstall: h.ServeInstallMode(), UseSSHConfig: h.UseSSHConfig,
		PasswordSet:      config.ResolveCredential(h.PasswordEnv).Set,
		KeyPassphraseSet: config.ResolveCredential(h.PassphraseEnv).Set,
	}
}

func inputToHostEntry(in RemoteHostInput) config.RemoteHostEntry {
	name := strings.TrimSpace(in.Label)
	return config.RemoteHostEntry{
		Name: name, Host: in.Host, Port: in.Port, User: in.User,
		IdentityFile: in.IdentityFile, ProxyJump: in.ProxyJump,
		Workspace: in.DefaultWorkspace, ServeInstall: in.ServeInstall, UseSSHConfig: in.UseSSHConfig,
	}
}

func forwardEntriesToViews(hostID string, entries []forward.Entry) []RemoteForwardView {
	out := make([]RemoteForwardView, 0, len(entries))
	for _, e := range entries {
		state := "active"
		if !e.Up {
			state = "error"
		}
		v := RemoteForwardView{
			ID: e.Spec.Name, HostID: hostID, Label: e.Spec.Name, State: state,
		}
		if e.LastErr != nil {
			v.Error = e.LastErr.Error()
		}
		out = append(out, v)
	}
	return out
}

func statusString(s remote.Status) string {
	switch s {
	case remote.StatusConnecting:
		return "connecting"
	case remote.StatusConnected:
		return "connected"
	case remote.StatusReconnecting:
		return "reconnecting"
	case remote.StatusDegraded:
		return "degraded"
	case remote.StatusStopped:
		return "stopped"
	default:
		return "stopped"
	}
}
