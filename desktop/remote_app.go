package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"reasonix/internal/remote"
)

// ── View structs mirrored in frontend/src/lib/types.ts ──

type RemoteHostView struct {
	ID               string `json:"id"`
	Label            string `json:"label"`
	Host             string `json:"host"`
	Port             int    `json:"port"`
	User             string `json:"user"`
	IdentityFile     string `json:"identityFile"`
	ProxyJump        string `json:"proxyJump"`
	DefaultWorkspace string `json:"defaultWorkspace"`
	ServeInstall     string `json:"serveInstall"`
	UseSSHConfig     bool   `json:"useSSHConfig"`
	PasswordSet      bool   `json:"passwordSet,omitempty"`
	KeyPassphraseSet bool   `json:"keyPassphraseSet,omitempty"`
}

type RemoteHostInput struct {
	Label                    string `json:"label"`
	Host                     string `json:"host"`
	Port                     int    `json:"port"`
	User                     string `json:"user"`
	IdentityFile             string `json:"identityFile"`
	ProxyJump                string `json:"proxyJump"`
	DefaultWorkspace         string `json:"defaultWorkspace"`
	ServeInstall             string `json:"serveInstall"`
	UseSSHConfig             bool   `json:"useSSHConfig"`
	Password                 string `json:"password,omitempty"`
	KeyPassphrase            string `json:"keyPassphrase,omitempty"`
	ClearPassword            bool   `json:"clearPassword,omitempty"`
	ClearPassphrase          bool   `json:"clearPassphrase,omitempty"`
	PreserveExistingSettings bool   `json:"preserveExistingSettings,omitempty"`
}

type RemoteFingerprintView struct {
	HostID  string `json:"hostId"`
	Address string `json:"address"`
	KeyType string `json:"keyType"`
	SHA256  string `json:"sha256"`
}

type RemoteConnectionStatusView struct {
	HostID       string                            `json:"hostId"`
	State        string                            `json:"state"`
	Error        string                            `json:"error,omitempty"`
	ErrorDetails *RemoteConnectionErrorDetailsView `json:"errorDetails,omitempty"`
	Fingerprint  *RemoteFingerprintView            `json:"fingerprint,omitempty"`
	SecretPrompt *RemoteSecretPromptView           `json:"secretPrompt,omitempty"`
	Attempt      int                               `json:"attempt,omitempty"`
}

// RemoteSecretPromptView contains prompt metadata only. Secret text travels
// one way through ConfirmRemoteSecret and is never emitted in status events.
type RemoteSecretPromptView struct {
	PromptID string `json:"promptId"`
	HostID   string `json:"hostId"`
	Host     string `json:"host"`
	Kind     string `json:"kind"` // password | passphrase
	Identity string `json:"identity,omitempty"`
}

type RemoteKnownHostLocationView struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}

type RemoteConnectionErrorDetailsView struct {
	Code             string                        `json:"code"`
	PresentedSHA256  string                        `json:"presentedSha256,omitempty"`
	KnownHostRecords []RemoteKnownHostLocationView `json:"knownHostRecords,omitempty"`
}

type RemoteDirEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	IsDir     bool   `json:"isDir"`
	Size      int64  `json:"size"`
	MtimeUnix int64  `json:"mtimeUnix"`
	Symlink   bool   `json:"symlink"`
}

type RemoteFilePreview struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Size      int64  `json:"size"`
	MtimeUnix int64  `json:"mtimeUnix"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
	Err       string `json:"err,omitempty"`
}

type RemoteWriteResult struct {
	OK           bool  `json:"ok"`
	Conflict     bool  `json:"conflict"`
	NewMtimeUnix int64 `json:"newMtimeUnix"`
}

type RemoteForwardInput struct {
	LocalPort  int    `json:"localPort"`
	RemoteHost string `json:"remoteHost"`
	RemotePort int    `json:"remotePort"`
	Label      string `json:"label"`
}

type RemoteForwardView struct {
	ID         string `json:"id"`
	HostID     string `json:"hostId"`
	LocalPort  int    `json:"localPort"`
	RemoteHost string `json:"remoteHost"`
	RemotePort int    `json:"remotePort"`
	Label      string `json:"label"`
	State      string `json:"state"`
	Error      string `json:"error,omitempty"`
}

type RemoteServerView struct {
	HostID    string `json:"hostId"`
	Workspace string `json:"workspace"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	LocalURL  string `json:"localUrl,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ── Kernel seam ──

// remoteKernel is the desktop's view of the remote subsystem. The concrete
// *desktopRemoteManager satisfies it; remote_app_test.go injects a fake.
type remoteKernel interface {
	Hosts() ([]RemoteHostView, error)
	AddHost(RemoteHostInput) (RemoteHostView, error)
	UpdateHost(id string, in RemoteHostInput) (RemoteHostView, error)
	RemoveHost(id string) error
	ScanSSHConfig() ([]RemoteHostInput, error)

	Connect(hostID string) error
	Disconnect(hostID string) error
	Statuses() []RemoteConnectionStatusView
	ResolveHostKey(hostID string, accept bool) error
	ResolveSecret(hostID, promptID, secret string, accept bool) error

	ListDir(ctx context.Context, hostID, path string) ([]RemoteDirEntry, error)
	ReadFile(ctx context.Context, hostID, path string) (RemoteFilePreview, error)
	WriteFile(ctx context.Context, hostID, path, body string, expectMtime int64) (RemoteWriteResult, error)
	Mkdir(ctx context.Context, hostID, path string) error
	Rename(ctx context.Context, hostID, oldPath, newPath string) error
	Delete(ctx context.Context, hostID, path string, recursive bool) error

	Forwards(hostID string) []RemoteForwardView
	AddForward(hostID string, in RemoteForwardInput) (RemoteForwardView, error)
	RemoveForward(hostID, forwardID string) error

	EnsureServer(ctx context.Context, hostID, workspace string) (RemoteServerView, string, error)
	StopServer(hostID string) error
	ServerStatus(hostID string) RemoteServerView
	ServerLogs(ctx context.Context, hostID string, tailLines int) (string, error)

	Close() error
}

// remoteEventSink receives kernel status transitions for bridging to the
// frontend. All methods may be called from kernel goroutines.
type remoteEventSink interface {
	onStatus(RemoteConnectionStatusView)
	onForwards(hostID string, forwards []RemoteForwardView)
	onServer(RemoteServerView)
}

// ── App wiring ──

func (a *App) remoteRT() (remoteKernel, error) {
	a.remoteMu.Lock()
	defer a.remoteMu.Unlock()
	if a.remoteRuntime != nil {
		return a.remoteRuntime, nil
	}
	mgr := newDesktopRemoteManager(a)
	a.remoteRuntime = mgr
	return mgr, nil
}

func (a *App) stopRemoteRuntime() {
	a.remoteMu.Lock()
	rt := a.remoteRuntime
	a.remoteRuntime = nil
	a.remoteMu.Unlock()
	if rt != nil {
		_ = rt.Close()
	}
}

// emitRemoteEvent bridges a kernel callback to the frontend through the async
// emitter so a slow webview never blocks the kernel.
func (a *App) emitRemoteEvent(name string, payload any) {
	ctx := a.bootContext()
	if ctx == nil {
		return
	}
	a.runtimeEvents.Emit(ctx, name, payload)
}

// remoteEventSink implementation on *App.
func (a *App) onStatus(s RemoteConnectionStatusView) {
	a.emitRemoteEvent("remote:status", s)
	// A terminal SSH failure (auth, host key, exhausted retries) kills the
	// tunnel: close the host's web window so the user is not left staring at a
	// dead Serve page. The frontend already shows the failure reason through
	// the remote:status event. Transient reconnects (reconnecting/degraded)
	// keep the window open.
	if s.State == "stopped" && s.Error != "" {
		// Status callbacks may run while desktopRemoteManager.mu is held, so never
		// wait on the host lifecycle mutex here. Capturing the generation before
		// queueing makes a later explicit reconnect/open supersede this close.
		op := a.beginRemoteWindowHostOperation(s.HostID)
		a.goSafe("remoteWindowTerminalClose", func() {
			_ = op.run(func(func() bool) error {
				a.closeRemoteWindowForHost(s.HostID)
				return nil
			})
		})
		return
	}
	// After a reconnect the loopback tunnel rebinds to a new port. Re-point an
	// open web window at the fresh Serve URL so it stays usable.
	if s.State == "connected" && a.hasRemoteWindow(s.HostID) {
		a.refreshRemoteWindowAfterReconnect(s.HostID)
	}
}

// refreshRemoteWindowAfterReconnect re-establishes the Serve forward after an
// SSH reconnect and navigates the host's web window to the new loopback URL.
// The remote Serve process is reused, so this is a cheap state probe when the
// tunnel already rebinding — the window is kept regardless of failure, and the
// user can reopen it if the Serve itself went away.
func (a *App) refreshRemoteWindowAfterReconnect(hostID string) {
	op := a.beginRemoteWindowHostOperation(hostID)
	a.goSafe("remoteWindowReconnect", func() {
		_ = op.run(func(current func() bool) error {
			rt, err := a.remoteRT()
			if err != nil {
				return nil
			}
			status := rt.ServerStatus(hostID)
			if status.State != "ready" || strings.TrimSpace(status.Workspace) == "" {
				return nil
			}
			view, token, err := rt.EnsureServer(a.bootContext(), hostID, status.Workspace)
			if err != nil || view.State != "ready" || view.LocalURL == "" || !current() {
				return nil
			}
			if !a.hasRemoteWindow(hostID) {
				return nil
			}
			_ = a.openRemoteWindowForHost(hostID, serveURLWithToken(view.LocalURL, token))
			return nil
		})
	})
}

func (a *App) onServer(s RemoteServerView) { a.emitRemoteEvent("remote:server", s) }
func (a *App) onForwards(hostID string, f []RemoteForwardView) {
	a.emitRemoteEvent("remote:forwards", map[string]any{"hostId": hostID, "forwards": f})
}

// ── Bound methods ──

func (a *App) RemoteHosts() ([]RemoteHostView, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return nil, err
	}
	return rt.Hosts()
}

func (a *App) AddRemoteHost(in RemoteHostInput) (RemoteHostView, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return RemoteHostView{}, err
	}
	return rt.AddHost(in)
}

func (a *App) UpdateRemoteHost(id string, in RemoteHostInput) (RemoteHostView, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return RemoteHostView{}, err
	}
	return rt.UpdateHost(id, in)
}

func (a *App) RemoveRemoteHost(id string) error {
	op := a.beginRemoteWindowHostOperation(id)
	return op.run(func(func() bool) error {
		rt, err := a.remoteRT()
		if err != nil {
			return err
		}
		if err := rt.RemoveHost(id); err != nil {
			return err
		}
		a.closeRemoteWindowForHost(id)
		return nil
	})
}

func (a *App) ConnectRemoteHost(id string) error {
	rt, err := a.remoteRT()
	if err != nil {
		return err
	}
	if err := rt.Connect(id); err != nil {
		view := RemoteConnectionStatusView{HostID: id, State: "stopped"}
		applyRemoteConnectionError(&view, err)
		a.onStatus(view)
		return err
	}
	return nil
}

func applyRemoteConnectionError(view *RemoteConnectionStatusView, err error) {
	if err == nil {
		return
	}
	view.Error = err.Error()
	if view.State == "degraded" {
		return
	}
	details := &RemoteConnectionErrorDetailsView{Code: "connection_failed"}
	switch {
	case errors.Is(err, remote.ErrHostKeyMismatch):
		details.Code = "host_key_mismatch"
		var mismatch *remote.HostKeyMismatchError
		if errors.As(err, &mismatch) {
			details.PresentedSHA256 = mismatch.PresentedFingerprint
			details.KnownHostRecords = make([]RemoteKnownHostLocationView, 0, len(mismatch.Locations))
			for _, location := range mismatch.Locations {
				details.KnownHostRecords = append(details.KnownHostRecords, RemoteKnownHostLocationView{
					Path: location.Filename,
					Line: location.Line,
				})
			}
		}
	case errors.Is(err, remote.ErrAuthFailed):
		details.Code = "auth_failed"
	case errors.Is(err, remote.ErrHostKeyRejected):
		details.Code = "host_key_rejected"
	}
	view.ErrorDetails = details
}

func (a *App) DisconnectRemoteHost(id string) error {
	op := a.beginRemoteWindowHostOperation(id)
	return op.run(func(func() bool) error {
		rt, err := a.remoteRT()
		if err != nil {
			return err
		}
		if err := rt.Disconnect(id); err != nil {
			return err
		}
		// An explicit disconnect kills the loopback tunnel; close the host's web
		// window so the user is not left staring at a dead Serve page. The remote
		// Serve itself stays resident.
		a.closeRemoteWindowForHost(id)
		return nil
	})
}

func (a *App) RemoteConnectionStatuses() []RemoteConnectionStatusView {
	rt, err := a.remoteRT()
	if err != nil {
		return nil
	}
	return rt.Statuses()
}

func (a *App) ConfirmRemoteHostKey(hostID string, accept bool) error {
	rt, err := a.remoteRT()
	if err != nil {
		return err
	}
	return rt.ResolveHostKey(hostID, accept)
}

// ConfirmRemoteSecret resolves a one-shot interactive SSH credential prompt.
// The secret is retained only in the connection's in-memory reconnect cache;
// callers must use the host settings form when they explicitly want storage.
func (a *App) ConfirmRemoteSecret(hostID, promptID, secret string, accept bool) error {
	rt, err := a.remoteRT()
	if err != nil {
		return err
	}
	return rt.ResolveSecret(hostID, promptID, secret, accept)
}

func (a *App) ListRemoteDir(hostID, path string) ([]RemoteDirEntry, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return nil, err
	}
	return rt.ListDir(a.bootContext(), hostID, path)
}

func (a *App) ReadRemoteFile(hostID, path string) (RemoteFilePreview, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return RemoteFilePreview{}, err
	}
	return rt.ReadFile(a.bootContext(), hostID, path)
}

func (a *App) WriteRemoteFile(hostID, path, body string, expectMtimeUnix int64) (RemoteWriteResult, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return RemoteWriteResult{}, err
	}
	return rt.WriteFile(a.bootContext(), hostID, path, body, expectMtimeUnix)
}

func (a *App) MkdirRemote(hostID, path string) error {
	rt, err := a.remoteRT()
	if err != nil {
		return err
	}
	return rt.Mkdir(a.bootContext(), hostID, path)
}

func (a *App) RenameRemotePath(hostID, oldPath, newPath string) error {
	rt, err := a.remoteRT()
	if err != nil {
		return err
	}
	return rt.Rename(a.bootContext(), hostID, oldPath, newPath)
}

func (a *App) DeleteRemotePath(hostID, path string, recursive bool) error {
	rt, err := a.remoteRT()
	if err != nil {
		return err
	}
	return rt.Delete(a.bootContext(), hostID, path, recursive)
}

func (a *App) RemoteForwards(hostID string) ([]RemoteForwardView, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return nil, err
	}
	return rt.Forwards(hostID), nil
}

func (a *App) AddRemoteForward(hostID string, in RemoteForwardInput) (RemoteForwardView, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return RemoteForwardView{}, err
	}
	return rt.AddForward(hostID, in)
}

func (a *App) RemoveRemoteForward(hostID, forwardID string) error {
	rt, err := a.remoteRT()
	if err != nil {
		return err
	}
	return rt.RemoveForward(hostID, forwardID)
}

// OpenRemoteWorkspace is the idempotent "open remote web" entry: it starts or
// reuses the target workspace's remote Serve, atomically replaces the loopback
// tunnel, then opens (or re-points) the host's web window.
//
// Two-phase switch contract:
//   - If Serve/tunnel establishment fails, nothing is touched: the previous
//     window and tunnel stay exactly as they were, and no workspace is saved.
//   - Once the new Serve and tunnel are committed, the switch is final. A
//     window-open failure (spawn error) surfaces to the caller while the Serve
//     stays ready for the new workspace, and the recorded last workspace
//     matches the running Serve so the next open reuses it. The previous
//     window, if any, is left in place; it is re-pointed by the next
//     successful open (or closed by an explicit disconnect/stop).
func (a *App) OpenRemoteWorkspace(hostID, workspace string) error {
	op := a.beginRemoteWindowHostOperation(hostID)
	return op.run(func(current func() bool) error {
		rt, err := a.remoteRT()
		if err != nil {
			return err
		}
		view, token, err := rt.EnsureServer(a.bootContext(), hostID, workspace)
		if err != nil {
			return err
		}
		// A disconnect, stop, removal, or terminal SSH failure that began while
		// EnsureServer was in flight owns the final state and must prevent a late
		// child window from being spawned against its dead tunnel.
		if !current() {
			return nil
		}
		if view.LocalURL == "" {
			return fmt.Errorf("remote serve did not report a local URL")
		}
		url := serveURLWithToken(view.LocalURL, token)
		a.saveLastRemoteWorkspace(hostID, workspace)
		return a.openRemoteWindowForHost(hostID, url)
	})
}

// serveURLWithToken appends the one-shot Serve token to the first-visit URL.
// The remote serve converts it to an HttpOnly cookie on the first request and
// redirects to a token-free URL.
func serveURLWithToken(localURL, token string) string {
	if token != "" && !strings.Contains(localURL, "token=") {
		return fmt.Sprintf("%s?token=%s", strings.TrimRight(localURL, "/"), token)
	}
	return localURL
}

func (a *App) StopRemoteServer(hostID string) error {
	op := a.beginRemoteWindowHostOperation(hostID)
	return op.run(func(func() bool) error {
		rt, err := a.remoteRT()
		if err != nil {
			return err
		}
		if err := rt.StopServer(hostID); err != nil {
			return err
		}
		// Stopping the service also tears down the loopback tunnel, so close the
		// host's web window.
		a.closeRemoteWindowForHost(hostID)
		return nil
	})
}

func (a *App) RemoteServerStatus(hostID string) (RemoteServerView, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return RemoteServerView{}, err
	}
	return rt.ServerStatus(hostID), nil
}

func (a *App) RemoteServerLogs(hostID string, tailLines int) (string, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return "", err
	}
	return rt.ServerLogs(a.bootContext(), hostID, tailLines)
}

// editUserConfig runs mutate against the user-global config under the edit lock
// and saves it there. Remote hosts are user-global (pinned in LoadForRoot).

// ── desktopRemoteManager: concrete remoteKernel ──

// TOFU resolution channel; non-nil while pending
// one-shot credential channel; non-nil while pending
// opaque ID prevents a stale dialog resolving a later prompt
// authenticated target key; retained after pending UI clears
// serializes EnsureServer/StopServer for this host

// Non-nil so Wails encodes an empty result as [] (not null), which the React
// import page iterates safely.

// Honor the user's proxy settings for the SSH dial, same as the CLI.

// a ProxyJump identity is not the target identity

// Insert a fully-populated generation atomically. A stopped generation is
// replaceable; active/connecting generations make Connect idempotent.

// already connecting/connected

// Keep the stopped generation and its user-visible error. The next
// Connect atomically replaces it with a fresh client.

// hostKeyPrompt returns a HostKeyPrompt that surfaces the fingerprint as a
// pending_hostkey status and blocks on the answer channel until the UI calls
// ConfirmRemoteHostKey.

// The frontend presents one global TOFU dialog. Serialize prompts so two
// simultaneous first-seen hosts cannot overwrite one another in the UI.

// secretPrompt surfaces a password/passphrase request as a global desktop
// dialog. Prompt metadata may be emitted, but the entered secret only crosses
// the one-shot answer channel and AuthOptions' in-memory reconnect cache.

// Preserve a pending modal that a separate prompt goroutine set.

// sftpfs.KindText == 0

// Optimistic-concurrency check: if the caller passed an expected mtime and
// the remote file moved, report a conflict instead of overwriting.

// Serialize per-host so two concurrent EnsureServer calls cannot both miss
// the state and launch duplicate/orphan serve processes.

// Start the replacement before retiring the old tunnel. If binding fails,
// the previous ready server stays usable instead of leaving a dead gap.

// Tear down the local serve tunnel so a stale forward can't linger.

// managed returns the managed host record for hostID, or nil.

// publishFailedServeStart keeps host server ownership on a previous ready
// Serve when a new Serve or its tunnel failed to establish. The previous
// Serve is still running with its tunnel (forward Replace is atomic), so
// Stop/Logs and reconnect refresh must keep operating on the workspace that
// actually runs; the failure is delivered through the EnsureServer return
// value and the caller's actionErr. When there is no previous ready Serve
// (first start), the error view is published so the UI can show it.

// ── helpers ──

// Importing an already-managed SSH alias refreshes only its OpenSSH lookup
// fields. Reasonix-specific workspace and bootstrap policy remain user-owned.

// applyRemoteCredentialInput maps plaintext received from the one-shot Wails
// call into Reasonix-owned credential slots. Blank fields preserve the current
// reference; explicit clear flags remove only slots that this desktop created.
