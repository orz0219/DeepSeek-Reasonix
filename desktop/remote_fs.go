package main

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/remote/bootstrap"
	"reasonix/internal/remote/forward"
)

func (m *desktopRemoteManager) fs(ctx context.Context, hostID string) (desktopSSHClient, error) {
	c := m.client(hostID)
	if c == nil {
		return nil, fmt.Errorf("host %q is not connected", hostID)
	}
	return c, nil
}

func (m *desktopRemoteManager) ListDir(ctx context.Context, hostID, path string) ([]RemoteDirEntry, error) {
	c, err := m.fs(ctx, hostID)
	if err != nil {
		return nil, err
	}
	fsys, err := c.SFTP()
	if err != nil {
		return nil, err
	}
	entries, err := fsys.List(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteDirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, RemoteDirEntry{
			Name: e.Name, Path: e.Path, IsDir: e.IsDir,
			Size: e.Size, MtimeUnix: e.ModTime, Symlink: e.Symlink,
		})
	}
	return out, nil
}

func (m *desktopRemoteManager) ReadFile(ctx context.Context, hostID, path string) (RemoteFilePreview, error) {
	c, err := m.fs(ctx, hostID)
	if err != nil {
		return RemoteFilePreview{}, err
	}
	fsys, err := c.SFTP()
	if err != nil {
		return RemoteFilePreview{}, err
	}
	st, err := fsys.Stat(ctx, path)
	if err != nil {
		return RemoteFilePreview{Path: path, Err: err.Error()}, nil
	}
	data, truncated, kind, err := fsys.ReadFile(ctx, path, 0)
	if err != nil {
		return RemoteFilePreview{Path: path, Err: err.Error()}, nil
	}
	binary := kind != 0
	prev := RemoteFilePreview{
		Path: path, Size: st.Size, MtimeUnix: st.ModTime,
		Truncated: truncated, Binary: binary,
	}
	if !binary {
		prev.Body = string(data)
	}
	return prev, nil
}

func (m *desktopRemoteManager) WriteFile(ctx context.Context, hostID, path, body string, expectMtime int64) (RemoteWriteResult, error) {
	c, err := m.fs(ctx, hostID)
	if err != nil {
		return RemoteWriteResult{}, err
	}
	fsys, err := c.SFTP()
	if err != nil {
		return RemoteWriteResult{}, err
	}

	if expectMtime > 0 {
		if st, serr := fsys.Stat(ctx, path); serr == nil && st.ModTime != expectMtime {
			return RemoteWriteResult{Conflict: true}, nil
		}
	}
	if err := fsys.WriteFileAtomic(ctx, path, []byte(body), 0o644); err != nil {
		return RemoteWriteResult{}, err
	}
	st, _ := fsys.Stat(ctx, path)
	return RemoteWriteResult{OK: true, NewMtimeUnix: st.ModTime}, nil
}

func (m *desktopRemoteManager) Mkdir(ctx context.Context, hostID, path string) error {
	c, err := m.fs(ctx, hostID)
	if err != nil {
		return err
	}
	fsys, err := c.SFTP()
	if err != nil {
		return err
	}
	return fsys.MkdirAll(ctx, path)
}

func (m *desktopRemoteManager) Rename(ctx context.Context, hostID, oldPath, newPath string) error {
	c, err := m.fs(ctx, hostID)
	if err != nil {
		return err
	}
	fsys, err := c.SFTP()
	if err != nil {
		return err
	}
	return fsys.Rename(ctx, oldPath, newPath)
}

func (m *desktopRemoteManager) Delete(ctx context.Context, hostID, path string, recursive bool) error {
	c, err := m.fs(ctx, hostID)
	if err != nil {
		return err
	}
	fsys, err := c.SFTP()
	if err != nil {
		return err
	}
	return fsys.Remove(ctx, path, recursive)
}

func (m *desktopRemoteManager) Forwards(hostID string) []RemoteForwardView {
	c := m.client(hostID)
	if c == nil {
		return nil
	}
	return forwardEntriesToViews(hostID, c.Forwards().List())
}

func (m *desktopRemoteManager) AddForward(hostID string, in RemoteForwardInput) (RemoteForwardView, error) {
	c := m.client(hostID)
	if c == nil {
		return RemoteForwardView{}, fmt.Errorf("host %q is not connected", hostID)
	}
	if in.LocalPort <= 0 || in.LocalPort > 65535 || in.RemotePort <= 0 || in.RemotePort > 65535 || strings.TrimSpace(in.RemoteHost) == "" {
		return RemoteForwardView{}, fmt.Errorf("forward requires a remote host and ports between 1 and 65535")
	}
	spec := forward.Spec{
		Name:       in.Label,
		Direction:  forward.Local,
		BindAddr:   net.JoinHostPort("127.0.0.1", fmt.Sprint(in.LocalPort)),
		TargetAddr: net.JoinHostPort(strings.TrimSpace(in.RemoteHost), fmt.Sprint(in.RemotePort)),
	}
	if _, err := c.Forwards().Add(spec); err != nil {
		return RemoteForwardView{}, err
	}
	m.emitForwards(hostID)
	view := RemoteForwardView{
		ID: spec.DefaultName(), HostID: hostID, LocalPort: in.LocalPort,
		RemoteHost: in.RemoteHost, RemotePort: in.RemotePort, Label: in.Label, State: "active",
	}
	return view, nil
}

func (m *desktopRemoteManager) RemoveForward(hostID, forwardID string) error {
	c := m.client(hostID)
	if c == nil {
		return fmt.Errorf("host %q is not connected", hostID)
	}
	if err := c.Forwards().Remove(forwardID); err != nil {
		return err
	}
	m.emitForwards(hostID)
	return nil
}

func (m *desktopRemoteManager) emitForwards(hostID string) {
	mh := m.managed(hostID)
	if mh != nil {
		m.emitForwardsFor(hostID, mh)
	}
}

func (m *desktopRemoteManager) emitForwardsFor(hostID string, generation *managedHost) {
	entries := generation.client.Forwards().List()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hosts[hostID] != generation {
		return
	}
	if m.sink != nil {
		m.sink.onForwards(hostID, forwardEntriesToViews(hostID, entries))
	}
}

const serveForwardName = "serve"

func (m *desktopRemoteManager) EnsureServer(ctx context.Context, hostID, workspace string) (RemoteServerView, string, error) {
	mh := m.managed(hostID)
	if mh == nil || mh.client == nil {
		return RemoteServerView{}, "", fmt.Errorf("host %q is not connected", hostID)
	}

	mh.serveMu.Lock()
	defer mh.serveMu.Unlock()
	m.mu.Lock()
	if m.hosts[hostID] != mh {
		m.mu.Unlock()
		return RemoteServerView{}, "", fmt.Errorf("host %q connection was replaced", hostID)
	}
	previousServer := mh.server
	previousToken := mh.token
	m.mu.Unlock()
	c := mh.client
	opCtx, cancel := managedOperationContext(ctx, mh)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		return RemoteServerView{}, "", err
	}
	entry, _ := cfg.RemoteHost(hostID)
	starting := RemoteServerView{HostID: hostID, Workspace: workspace, State: "starting"}
	if !m.publishServerIfCurrent(hostID, mh, starting, "") {
		return RemoteServerView{}, "", fmt.Errorf("host %q connection was replaced", hostID)
	}
	res, err := m.ensureServe(opCtx, c, bootstrap.Options{
		Workspace:      workspace,
		Install:        entry.ServeInstallMode(),
		LocalBinary:    m.localBinary(),
		LocalGOOS:      runtime.GOOS,
		LocalGOARCH:    runtime.GOARCH,
		ProductVersion: version,
		FetchBinary:    m.fetchRemoteBinary,
		MinVersion:     bootstrap.MinServeVersion,
		Progress: func(step, detail string) {
			view := RemoteServerView{HostID: hostID, Workspace: workspace, State: step, Message: detail}
			m.publishServerIfCurrent(hostID, mh, view, "")
		},
	})
	if err != nil {
		view := RemoteServerView{HostID: hostID, Workspace: workspace, State: "error", Error: err.Error()}
		m.publishFailedServeStart(hostID, mh, previousServer, previousToken, view)
		return view, "", err
	}
	if !m.isCurrent(hostID, mh) {
		return RemoteServerView{}, "", fmt.Errorf("host %q connection was replaced", hostID)
	}
	if res.Reused && previousServer.State == "ready" && previousServer.Workspace == workspace &&
		hasUsableServeForward(c.Forwards().List(), res.State.Addr, previousServer.LocalURL) {
		if !m.publishServerIfCurrent(hostID, mh, previousServer, res.Token) {
			return RemoteServerView{}, "", fmt.Errorf("host %q connection was replaced", hostID)
		}
		return previousServer, res.Token, nil
	}

	bound, ferr := c.Forwards().Replace(forward.Spec{
		Name: serveForwardName, Direction: forward.Local, BindAddr: "127.0.0.1:0", TargetAddr: res.State.Addr,
	})
	if ferr != nil {
		if !res.Reused {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = m.stopServe(cleanupCtx, c, workspace)
			cleanupCancel()
		}
		view := RemoteServerView{HostID: hostID, Workspace: workspace, State: "error", Error: ferr.Error()}
		m.publishFailedServeStart(hostID, mh, previousServer, previousToken, view)
		return view, "", ferr
	}
	localURL := fmt.Sprintf("http://%s/", bound)
	view := RemoteServerView{HostID: hostID, Workspace: workspace, State: "ready", LocalURL: localURL}
	if !m.publishServerIfCurrent(hostID, mh, view, res.Token) {
		_ = c.Forwards().Remove(serveForwardName)
		return RemoteServerView{}, "", fmt.Errorf("host %q connection was replaced", hostID)
	}
	return view, res.Token, nil
}

func (m *desktopRemoteManager) StopServer(hostID string) error {
	mh := m.managed(hostID)
	if mh == nil || mh.client == nil {
		return fmt.Errorf("host %q is not connected", hostID)
	}
	mh.serveMu.Lock()
	defer mh.serveMu.Unlock()
	if !m.isCurrent(hostID, mh) {
		return fmt.Errorf("host %q connection was replaced", hostID)
	}
	c := mh.client
	m.mu.Lock()
	ws := mh.server.Workspace
	m.mu.Unlock()
	if strings.TrimSpace(ws) == "" {
		return fmt.Errorf("host %q has no managed server workspace", hostID)
	}
	opCtx, cancel := managedOperationContext(context.Background(), mh)
	defer cancel()
	if err := m.stopServe(opCtx, c, ws); err != nil {
		return err
	}

	_ = c.Forwards().Remove(serveForwardName)
	view := RemoteServerView{HostID: hostID, Workspace: ws, State: "stopped"}
	m.publishServerIfCurrent(hostID, mh, view, "")
	return nil
}

// managed returns the managed host record for hostID, or nil.
func (m *desktopRemoteManager) managed(hostID string) *managedHost {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hosts[hostID]
}

func (m *desktopRemoteManager) ServerStatus(hostID string) RemoteServerView {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mh := m.hosts[hostID]; mh != nil {
		return mh.server
	}
	return RemoteServerView{HostID: hostID, State: "stopped"}
}

func (m *desktopRemoteManager) ServerLogs(ctx context.Context, hostID string, tailLines int) (string, error) {
	m.mu.Lock()
	mh := m.hosts[hostID]
	ws := ""
	if mh != nil {
		ws = mh.server.Workspace
	}
	m.mu.Unlock()
	if mh == nil || mh.client == nil {
		return "", fmt.Errorf("host %q is not connected", hostID)
	}
	if strings.TrimSpace(ws) == "" {
		return "", fmt.Errorf("host %q has no managed server workspace", hostID)
	}
	opCtx, cancel := managedOperationContext(ctx, mh)
	defer cancel()
	var sb strings.Builder
	if err := m.serveLogs(opCtx, mh.client, ws, tailLines, &sb); err != nil {
		return "", err
	}
	if !m.isCurrent(hostID, mh) {
		return "", fmt.Errorf("host %q connection was replaced", hostID)
	}
	return sb.String(), nil
}

func (m *desktopRemoteManager) Close() error {
	m.mu.Lock()
	hosts := m.hosts
	m.hosts = map[string]*managedHost{}
	answers := make([]chan bool, 0, len(hosts))
	secretAnswers := make([]chan remoteSecretAnswer, 0, len(hosts))
	for _, mh := range hosts {
		if mh.fpAnswer != nil {
			answers = append(answers, mh.fpAnswer)
			mh.fpAnswer = nil
		}
		if mh.secretAnswer != nil {
			secretAnswers = append(secretAnswers, mh.secretAnswer)
			mh.secretAnswer = nil
		}
	}
	m.mu.Unlock()
	for _, answer := range answers {
		select {
		case answer <- false:
		default:
		}
	}
	for _, answer := range secretAnswers {
		select {
		case answer <- remoteSecretAnswer{}:
		default:
		}
	}
	for _, mh := range hosts {
		closeManagedHost(mh)
	}
	return nil
}

func managedOperationContext(parent context.Context, mh *managedHost) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	stop := func() bool { return false }
	if mh != nil && mh.ctx != nil {
		stop = context.AfterFunc(mh.ctx, cancel)
	}
	return ctx, func() {
		stop()
		cancel()
	}
}

// publishFailedServeStart keeps host server ownership on a previous ready
// Serve when a new Serve or its tunnel failed to establish. The previous
// Serve is still running with its tunnel (forward Replace is atomic), so
// Stop/Logs and reconnect refresh must keep operating on the workspace that
// actually runs; the failure is delivered through the EnsureServer return
// value and the caller's actionErr. When there is no previous ready Serve
// (first start), the error view is published so the UI can show it.
func (m *desktopRemoteManager) publishFailedServeStart(hostID string, generation *managedHost, previous RemoteServerView, previousToken string, failed RemoteServerView) {
	if previous.State == "ready" {
		m.publishServerIfCurrent(hostID, generation, previous, previousToken)
		return
	}
	m.publishServerIfCurrent(hostID, generation, failed, "")
}

func (m *desktopRemoteManager) publishServerIfCurrent(hostID string, generation *managedHost, view RemoteServerView, token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hosts[hostID] != generation {
		return false
	}
	generation.server = view
	generation.token = token
	if m.sink != nil {
		m.sink.onServer(view)
	}
	return true
}

func hasUsableServeForward(entries []forward.Entry, targetAddr, localURL string) bool {
	for _, entry := range entries {
		if entry.Spec.Name == serveForwardName && entry.Up && entry.Spec.TargetAddr == targetAddr && entry.BoundAddr != "" {
			return localURL == fmt.Sprintf("http://%s/", entry.BoundAddr)
		}
	}
	return false
}
