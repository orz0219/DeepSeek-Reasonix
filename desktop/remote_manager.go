package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/netclient"
	"reasonix/internal/remote"
	"reasonix/internal/remote/bootstrap"
	"reasonix/internal/remote/forward"
)

func (a *App) ScanSSHConfig() ([]RemoteHostInput, error) {
	rt, err := a.remoteRT()
	if err != nil {
		return nil, err
	}
	return rt.ScanSSHConfig()
}

type managedHost struct {
	client         desktopSSHClient
	ctx            context.Context
	cancel         context.CancelFunc
	status         RemoteConnectionStatusView
	server         RemoteServerView
	token          string
	fpAnswer       chan bool               // TOFU resolution channel; non-nil while pending
	secretAnswer   chan remoteSecretAnswer // one-shot credential channel; non-nil while pending
	secretPromptID string                  // opaque ID prevents a stale dialog resolving a later prompt
	verifiedPeer   *RemoteFingerprintView  // authenticated target key; retained after pending UI clears
	serveMu        sync.Mutex              // serializes EnsureServer/StopServer for this host
}

type remoteSecretAnswer struct {
	secret string
	accept bool
}

type desktopSSHClient interface {
	bootstrap.Conn
	Start(context.Context) error
	Close() error
	Subscribe(func(remote.StatusEvent)) func()
	Forwards() *forward.Set
}

type desktopRemoteManager struct {
	sink remoteEventSink

	mu    sync.Mutex
	hosts map[string]*managedHost

	newClient         func(remote.Options) (desktopSSHClient, error)
	ensureServe       func(context.Context, bootstrap.Conn, bootstrap.Options) (bootstrap.Result, error)
	stopServe         func(context.Context, bootstrap.Conn, string) error
	serveLogs         func(context.Context, bootstrap.Conn, string, int, *strings.Builder) error
	localBinary       func() string
	fetchRemoteBinary func(context.Context, string, string, string) ([]byte, error)
	promptGate        chan struct{}
	promptSeq         uint64
}

func newDesktopRemoteManager(sink remoteEventSink) *desktopRemoteManager {
	return &desktopRemoteManager{
		sink:  sink,
		hosts: map[string]*managedHost{},
		newClient: func(opts remote.Options) (desktopSSHClient, error) {
			return remote.New(opts)
		},
		ensureServe: bootstrap.EnsureServe,
		stopServe:   bootstrap.Stop,
		serveLogs: func(ctx context.Context, conn bootstrap.Conn, workspace string, n int, out *strings.Builder) error {
			return bootstrap.Logs(ctx, conn, workspace, n, out)
		},
		localBinary:       desktopCLIBinaryPath,
		fetchRemoteBinary: downloadRemoteCLIBinary,
		promptGate:        make(chan struct{}, 1),
	}
}

func (m *desktopRemoteManager) Hosts() ([]RemoteHostView, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	out := make([]RemoteHostView, 0, len(cfg.Remote.Hosts))
	for _, h := range cfg.Remote.Hosts {
		out = append(out, hostEntryToView(h))
	}
	return out, nil
}

func (m *desktopRemoteManager) AddHost(in RemoteHostInput) (RemoteHostView, error) {
	var entry config.RemoteHostEntry
	if err := config.EditUserConfigWithCredentials(func(c *config.Config) ([]config.CredentialChange, error) {
		entry = inputToHostEntry(in)
		if existing, ok := c.RemoteHost(entry.Name); ok {
			preserveRemoteHostHiddenFields(&entry, existing)
			if in.PreserveExistingSettings {
				preserveRemoteHostImportSettings(&entry, existing)
			}
		}
		changes, removals := applyRemoteCredentialInput(&entry, in)
		if err := c.UpsertRemoteHost(entry); err != nil {
			return nil, err
		}
		return append(changes, config.UnusedGeneratedRemoteCredentialChanges(c, removals)...), nil
	}); err != nil {
		return RemoteHostView{}, err
	}
	return hostEntryToView(entry), nil
}

func (m *desktopRemoteManager) UpdateHost(id string, in RemoteHostInput) (RemoteHostView, error) {
	var merged config.RemoteHostEntry
	if err := config.EditUserConfigWithCredentials(func(c *config.Config) ([]config.CredentialChange, error) {
		entry := inputToHostEntry(in)
		entry.Name = id
		if existing, ok := c.RemoteHost(id); ok {
			preserveRemoteHostHiddenFields(&entry, existing)
		}
		changes, removals := applyRemoteCredentialInput(&entry, in)
		merged = entry
		if err := c.UpsertRemoteHost(entry); err != nil {
			return nil, err
		}
		return append(changes, config.UnusedGeneratedRemoteCredentialChanges(c, removals)...), nil
	}); err != nil {
		return RemoteHostView{}, err
	}
	return hostEntryToView(merged), nil
}

func (m *desktopRemoteManager) RemoveHost(id string) error {
	_ = m.Disconnect(id)
	removed := false
	if err := config.EditUserConfigWithCredentials(func(c *config.Config) ([]config.CredentialChange, error) {
		var removals []string
		if existing, ok := c.RemoteHost(id); ok {
			for _, key := range []string{existing.PasswordEnv, existing.PassphraseEnv} {
				if config.IsGeneratedRemoteCredential(id, key) {
					removals = append(removals, key)
				}
			}
		}
		removed = c.RemoveRemoteHost(id)
		return config.UnusedGeneratedRemoteCredentialChanges(c, removals), nil
	}); err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("no remote host named %q", id)
	}
	return nil
}

func (m *desktopRemoteManager) ScanSSHConfig() ([]RemoteHostInput, error) {
	src, err := remote.LoadUserSSHConfig()
	if err != nil {
		return nil, err
	}

	out := []RemoteHostInput{}
	for _, cand := range src.Aliases() {
		out = append(out, RemoteHostInput{
			Label:                    cand.Alias,
			Host:                     cand.Alias,
			Port:                     0,
			UseSSHConfig:             true,
			PreserveExistingSettings: true,
		})
	}
	return out, nil
}

func (m *desktopRemoteManager) Connect(hostID string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	sshCfg, err := remote.LoadUserSSHConfig()
	if err != nil {
		return fmt.Errorf("load SSH config: %w", err)
	}
	host, err := remote.ResolveHost(cfg, hostID, sshCfg)
	if err != nil {
		return err
	}

	resolvedJumps, err := remote.ResolveJumpHosts(cfg, host.ProxyJump, sshCfg)
	if err != nil {
		return err
	}

	dialer, derr := netclient.NewStreamDialer(cfg.NetworkProxySpec())
	if derr != nil {
		return fmt.Errorf("remote: network proxy is misconfigured: %w", derr)
	}

	hostCtx, cancel := context.WithCancel(context.Background())
	mh := &managedHost{
		ctx: hostCtx, cancel: cancel,
		status: RemoteConnectionStatusView{HostID: hostID, State: "connecting"},
	}
	secretPrompt := m.secretPrompt(hostID, mh)
	auth := desktopAuthForHost(host, secretPrompt)
	jumpHosts := make([]remote.JumpHostOptions, 0, len(resolvedJumps))
	for _, jump := range resolvedJumps {
		jumpHosts = append(jumpHosts, remote.JumpHostOptions{Host: jump, Auth: desktopAuthForHost(jump, secretPrompt)})
	}
	policy := &remote.HostKeyPolicy{
		Prompt: m.hostKeyPrompt(hostID, mh),
		Verified: func(q remote.HostKeyQuestion) {
			if q.Host != host.Label() {
				return
			}
			m.mu.Lock()
			if m.hosts[hostID] == mh {
				mh.verifiedPeer = &RemoteFingerprintView{
					HostID: hostID, Address: q.Address, KeyType: q.KeyType, SHA256: q.Fingerprint,
				}
			}
			m.mu.Unlock()
		},
	}
	client, err := m.newClient(remote.Options{
		Host: host, Auth: auth, JumpHosts: jumpHosts, HostKeys: policy, Dialer: dialer,
	})
	if err != nil {
		cancel()
		return err
	}
	mh.client = client

	// Insert a fully-populated generation atomically. A stopped generation is
	// replaceable; active/connecting generations make Connect idempotent.
	var replaced *managedHost
	m.mu.Lock()
	if existing := m.hosts[hostID]; existing != nil && existing.status.State != "stopped" {
		m.mu.Unlock()
		cancel()
		_ = client.Close()
		return nil
	}
	replaced = m.hosts[hostID]
	m.hosts[hostID] = mh
	m.mu.Unlock()
	closeManagedHost(replaced)

	client.Subscribe(func(ev remote.StatusEvent) { m.onClientStatus(hostID, mh, ev) })

	go func() {
		if err := client.Start(hostCtx); err != nil {

			cancel()
			_ = client.Close()
			return
		}
		m.applyConfiguredForwards(hostID, mh, cfg)
	}()
	return nil
}

func desktopAuthForHost(host remote.ResolvedHost, prompt remote.SecretPrompt) remote.AuthOptions {
	auth := remote.AuthOptions{SecretPrompt: prompt}
	if host.PassphraseEnv != "" {
		env := host.PassphraseEnv
		auth.Passphrase = func() (string, error) { return config.ResolveCredential(env).Value, nil }
	}
	if host.PasswordEnv != "" {
		env := host.PasswordEnv
		auth.Password = func() (string, error) { return config.ResolveCredential(env).Value, nil }
	}
	return auth
}

func (m *desktopRemoteManager) applyConfiguredForwards(hostID string, mh *managedHost, cfg *config.Config) {
	entry, ok := cfg.RemoteHost(hostID)
	if !ok || !m.isCurrent(hostID, mh) {
		return
	}
	for _, f := range entry.Forwards {
		dir := forward.Local
		if strings.EqualFold(f.Type, "remote") {
			dir = forward.Remote
		}
		_, _ = mh.client.Forwards().Add(forward.Spec{Direction: dir, BindAddr: desktopNormalizeBind(f.Bind), TargetAddr: f.Target})
	}
	m.emitForwardsFor(hostID, mh)
}

func (m *desktopRemoteManager) Disconnect(hostID string) error {
	m.mu.Lock()
	mh := m.hosts[hostID]
	delete(m.hosts, hostID)
	var answer chan bool
	var secretAnswer chan remoteSecretAnswer
	if mh != nil {
		answer = mh.fpAnswer
		mh.fpAnswer = nil
		secretAnswer = mh.secretAnswer
		mh.secretAnswer = nil
		mh.secretPromptID = ""
	}
	if mh != nil && m.sink != nil {
		m.sink.onStatus(RemoteConnectionStatusView{HostID: hostID, State: "stopped"})
	}
	m.mu.Unlock()
	if mh == nil {
		return nil
	}
	if answer != nil {
		select {
		case answer <- false:
		default:
		}
	}
	if secretAnswer != nil {
		select {
		case secretAnswer <- remoteSecretAnswer{}:
		default:
		}
	}
	closeManagedHost(mh)
	return nil
}

func closeManagedHost(mh *managedHost) {
	if mh == nil {
		return
	}
	if mh.cancel != nil {
		mh.cancel()
	}
	if mh.client != nil {
		_ = mh.client.Close()
	}
}

func (m *desktopRemoteManager) Statuses() []RemoteConnectionStatusView {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]RemoteConnectionStatusView, 0, len(m.hosts))
	for _, mh := range m.hosts {
		out = append(out, mh.status)
	}
	return out
}

func (m *desktopRemoteManager) ResolveHostKey(hostID string, accept bool) error {
	m.mu.Lock()
	mh := m.hosts[hostID]
	var ch chan bool
	if mh != nil {
		ch = mh.fpAnswer
	}
	m.mu.Unlock()
	if ch == nil {
		return fmt.Errorf("no pending host key confirmation for %q", hostID)
	}
	select {
	case ch <- accept:
		return nil
	default:
		return fmt.Errorf("host key confirmation already resolved for %q", hostID)
	}
}

func (m *desktopRemoteManager) ResolveSecret(hostID, promptID, secret string, accept bool) error {
	m.mu.Lock()
	mh := m.hosts[hostID]
	var ch chan remoteSecretAnswer
	if mh != nil && mh.secretPromptID == promptID {
		ch = mh.secretAnswer
	}
	m.mu.Unlock()
	if ch == nil {
		return fmt.Errorf("no pending SSH credential prompt for %q", hostID)
	}
	select {
	case ch <- remoteSecretAnswer{secret: secret, accept: accept}:
		return nil
	default:
		return fmt.Errorf("SSH credential prompt already resolved for %q", hostID)
	}
}

// hostKeyPrompt returns a HostKeyPrompt that surfaces the fingerprint as a
// pending_hostkey status and blocks on the answer channel until the UI calls
// ConfirmRemoteHostKey.
func (m *desktopRemoteManager) hostKeyPrompt(hostID string, generation *managedHost) remote.HostKeyPrompt {
	return func(ctx context.Context, q remote.HostKeyQuestion) (bool, error) {

		select {
		case m.promptGate <- struct{}{}:
			defer func() { <-m.promptGate }()
		case <-ctx.Done():
			return false, ctx.Err()
		}
		answer := make(chan bool, 1)
		m.mu.Lock()
		mh := m.hosts[hostID]
		if mh != generation {
			m.mu.Unlock()
			return false, fmt.Errorf("host %q connection was replaced", hostID)
		}
		mh.fpAnswer = answer
		fp := &RemoteFingerprintView{HostID: hostID, Address: q.Address, KeyType: q.KeyType, SHA256: q.Fingerprint}
		mh.status = RemoteConnectionStatusView{HostID: hostID, State: "pending_hostkey", Fingerprint: fp}
		status := mh.status
		if m.sink != nil {
			m.sink.onStatus(status)
		}
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			if m.hosts[hostID] == generation && generation.fpAnswer == answer {
				generation.fpAnswer = nil
			}
			m.mu.Unlock()
		}()

		select {
		case ok := <-answer:
			return ok, nil
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(2 * time.Minute):
			return false, fmt.Errorf("host key confirmation timed out")
		}
	}
}

// secretPrompt surfaces a password/passphrase request as a global desktop
// dialog. Prompt metadata may be emitted, but the entered secret only crosses
// the one-shot answer channel and AuthOptions' in-memory reconnect cache.
func (m *desktopRemoteManager) secretPrompt(hostID string, generation *managedHost) remote.SecretPrompt {
	return func(ctx context.Context, kind remote.SecretKind, host, identityFile string) (string, error) {
		select {
		case m.promptGate <- struct{}{}:
			defer func() { <-m.promptGate }()
		case <-ctx.Done():
			return "", ctx.Err()
		}

		answer := make(chan remoteSecretAnswer, 1)
		m.mu.Lock()
		mh := m.hosts[hostID]
		if mh != generation {
			m.mu.Unlock()
			return "", fmt.Errorf("host %q connection was replaced", hostID)
		}
		m.promptSeq++
		promptID := fmt.Sprintf("ssh-secret-%d", m.promptSeq)
		mh.secretAnswer = answer
		mh.secretPromptID = promptID
		identity := ""
		if strings.TrimSpace(identityFile) != "" {
			identity = filepath.Base(identityFile)
		}
		prompt := &RemoteSecretPromptView{PromptID: promptID, HostID: hostID, Host: host, Kind: kind.String(), Identity: identity}
		mh.status = RemoteConnectionStatusView{HostID: hostID, State: "pending_secret", SecretPrompt: prompt}
		status := mh.status
		if m.sink != nil {
			m.sink.onStatus(status)
		}
		m.mu.Unlock()
		defer func() {
			m.mu.Lock()
			if m.hosts[hostID] == generation && generation.secretAnswer == answer {
				generation.secretAnswer = nil
				generation.secretPromptID = ""
			}
			m.mu.Unlock()
		}()

		select {
		case response := <-answer:
			if !response.accept {
				return "", fmt.Errorf("remote: %s prompt canceled", kind)
			}
			return response.secret, nil
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Minute):
			return "", fmt.Errorf("remote: %s prompt timed out", kind)
		}
	}
}

func (m *desktopRemoteManager) onClientStatus(hostID string, generation *managedHost, ev remote.StatusEvent) {
	if ev.Status == remote.StatusIdle {
		return
	}
	view := RemoteConnectionStatusView{
		HostID:  hostID,
		State:   statusString(ev.Status),
		Attempt: ev.Attempt,
	}
	if ev.Err != nil {
		applyRemoteConnectionError(&view, ev.Err)
	}
	m.mu.Lock()
	mh := m.hosts[hostID]
	if mh != generation {
		m.mu.Unlock()
		return
	}

	if (mh.status.State == "pending_hostkey" || mh.status.State == "pending_secret") && view.State == "connecting" {
		m.mu.Unlock()
		return
	}
	mh.status = view
	if m.sink != nil {
		m.sink.onStatus(view)
	}
	m.mu.Unlock()
}

func (m *desktopRemoteManager) isCurrent(hostID string, generation *managedHost) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hosts[hostID] == generation
}

func (m *desktopRemoteManager) client(hostID string) desktopSSHClient {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mh := m.hosts[hostID]; mh != nil {
		return mh.client
	}
	return nil
}
