package plugin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"reasonix/internal/proc"
	"reasonix/internal/sandbox"
	"reasonix/internal/secrets"
	"reasonix/internal/tool"
)

const (
	closeWaitBudget         = 5 * time.Second
	gracefulCloseWaitBudget = 750 * time.Millisecond
)

// stdioTransport speaks newline-delimited JSON-RPC 2.0 over a subprocess's
// stdin/stdout — the MCP stdio convention (one JSON message per line, no
// embedded newlines). A dedicated reader goroutine owns stdout and demuxes each
// response to the waiting call by id, so a call can abandon a blocking read the
// moment its context is cancelled (the subprocess is bound to the session, not
// the turn, so a hung server would otherwise hang a cancelled turn forever).
// callMu serialises a request/response round-trip over the shared pipe.
type stdioTransport struct {
	name   string
	roots  []mcpRoot
	cmd    *exec.Cmd
	job    uintptr // Windows Job Object handle (0 elsewhere); reaps detached grandchildren on close
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *tailBuffer

	callMu  sync.Mutex // one in-flight request/response at a time over the shared pipe
	writeMu sync.Mutex // client calls and server-request replies share stdin

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcResponse
	readErr error // set once the reader goroutine exits; further calls fail fast

	waitOnce    sync.Once
	releaseSlot func() // returns a bounded instance slot (e.g. CodeGraph) on close; nil when unbounded
	progress    progressRouter
}

func newStdioTransport(ctx context.Context, s Spec) (*stdioTransport, error) {
	if strings.TrimSpace(s.Command) == "" {
		return nil, fmt.Errorf("stdio plugin %q: command is required", s.Name)
	}
	var releaseSlot func()
	if isCodeGraphSpecName(s.Name) {
		release, err := acquireCodeGraphSlot()
		if err != nil {
			return nil, err
		}
		releaseSlot = release
	}
	defer func() {
		// Release the reserved slot if construction fails before the transport
		// takes ownership of it (set to nil on the success path below).
		if releaseSlot != nil {
			releaseSlot()
		}
	}()
	env := mergeEnv(secrets.ProcessEnv(), s.Env)
	exe, env, err := resolveStdioExecutable(ctx, s, env)
	if err != nil {
		return nil, err
	}
	// Private state/cache/temp always apply so MCP processes do not pollute the
	// user's home caches. Command-sandbox wrapping is separate and only used for
	// confined mode; authorized user installs run as trusted host processes so
	// Chrome, Keychain, and local app services keep working.
	processSandbox := s.Sandbox
	processSandbox, env, err = prepareMCPPrivateState(s, processSandbox, env)
	if err != nil {
		return nil, err
	}
	launchArgs := append([]string{exe}, effectiveLaunchArgs(s)...)
	var argv []string
	if s.ResolvedProcessMode() == MCPProcessConfined {
		processSandbox.MinimalWrites = true
		argv, _ = sandbox.CommandArgs(processSandbox, launchArgs)
	} else {
		argv = launchArgs
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	proc.HideWindow(cmd)
	if s.LowPriority {
		proc.LowPriority(cmd)
	}
	cmd.Env = env
	cmd.Dir = stdioWorkingDir(s)
	stderr := &tailBuffer{limit: 16 * 1024}
	cmd.Stderr = stderr
	if s.Stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, s.Stderr)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	job, err := proc.StartTracked(cmd)
	if err != nil {
		return nil, err
	}
	if s.LowPriority {
		proc.LowPriorityStarted(cmd)
	}
	t := &stdioTransport{
		name:        s.Name,
		roots:       mcpRoots(s.WorkspaceRoot),
		cmd:         cmd,
		job:         job,
		stdin:       stdin,
		stdout:      bufio.NewReader(stdout),
		stderr:      stderr,
		pending:     map[int]chan rpcResponse{},
		releaseSlot: releaseSlot,
	}
	releaseSlot = nil // ownership transferred to t; close() releases it
	go t.readLoop()
	return t, nil
}

// Windows stdio processes are currently unsandboxed and must keep the host's
// short temporary directory. Nesting TEMP below Reasonix's workspace-scoped
// state path can exceed the 108-byte Unix-domain-socket limit used by MCP
// servers such as MATLAB before their initialize response is written.

// cachedShellPATH memoizes the first completed shell-PATH probe: the user's
// interactive PATH is stable for the process, and resolveStdioExecutable now
// probes for every stdio plugin, so caching avoids a login shell per server.
// The probe runs up to three login shells with a 2s timeout each, so it must
// not run under the lock; concurrent spawns share the in-flight probe instead
// of each running (or queueing behind) their own. Empty results are cached too
// — a host without a usable login shell must not re-probe on every spawn —
// except when the probe's context was cancelled, since that empty reflects the
// aborted caller rather than the host, and caching it would pin "" for the
// rest of the process.

// non-nil while a probe runs; closed when it settles

// re-check: the probe may not have cached (cancelled)

// Unconditionally enrich PATH with the user's shell PATH so every
// subprocess—including wrapper scripts that invoke npx, uvx, etc.—
// inherits the expected tool locations even under a GUI launch.

// stdioWorkingDir keeps WorkspaceRoot's roots/list role separate from process
// execution for user-installed servers. Only repository-declared servers need
// relative arguments to resolve against the project that supplied the config.

// enrichStdioShellPATH probes the user's interactive login shell for its PATH
// and prepends those directories to the current environment. The result is the
// subprocess environment with a PATH that matches what the user sees in their
// terminal, even when Reasonix was launched from the Finder / Dock / open(1).

// Explicit env so the login-shell probe honors [secrets]
// filter_subprocess_env instead of inheriting the full environment.

// stdioReplyQueueBound caps buffered server-request replies. The queue only
// backs up while the reply writer is stuck behind a jammed stdin pipe, so a
// small bound is plenty; overflow drops the reply instead of blocking readLoop.
const stdioReplyQueueBound = 16

// readLoop owns stdout for the transport's lifetime: it reads one JSON-RPC
// message per line, routes progress notifications, answers server requests, and
// hands each response to the call waiting on its id. On any read error it fails
// every pending call and exits.
func (t *stdioTransport) readLoop() {
	// Server-request replies go through replyLoop, never directly to stdin:
	// readLoop is the only goroutine draining stdout, and blocking it on
	// writeMu behind a client call whose own stdin write is jammed would
	// deadlock both pipes once the server also blocks writing stdout.
	replies := make(chan any, stdioReplyQueueBound)
	defer close(replies)
	go t.replyLoop(replies)
	for {
		line, readErr := t.stdout.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			t.handleInboundLine(line, replies)
		}
		if readErr != nil {
			t.failAll(readErr)
			return
		}
	}
}

// replyLoop serialises server-request replies onto the shared stdin pipe. A
// write failure is not terminal for the transport — the read side may still be
// healthy, and pipe errors surface through the next client call's own write —
// but it stops further replies and keeps draining so readLoop never blocks.
func (t *stdioTransport) replyLoop(replies <-chan any) {
	var dead bool
	for msg := range replies {
		if dead {
			continue
		}
		if t.write(msg) != nil {
			dead = true
		}
	}
}

func (t *stdioTransport) handleInboundLine(line []byte, replies chan<- any) {
	probe, ok := decodeInboundMessage(line)
	if !ok {
		return // unparseable line cannot be routed; keep the transport alive
	}
	if probe.Method != "" {
		if isNotificationID(probe.ID) {
			if probe.Method == "notifications/progress" {
				t.progress.dispatchProgress(probe.Params)
			}
			return
		}
		response := serverRequestReply(probe.ID, probe.Method, t.roots)
		select {
		case replies <- response:
		default:
			// The reply writer is stalled behind a full stdin pipe. An
			// unanswered request degrades to the server's own timeout; a
			// blocked readLoop could deadlock both pipes.
		}
		return
	}

	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return
	}
	t.mu.Lock()
	ch := t.pending[resp.ID]
	delete(t.pending, resp.ID)
	t.mu.Unlock()
	if ch != nil {
		ch <- resp // buffered(1): never blocks, even if the caller already left
	}
}

func (t *stdioTransport) registerProgress(token string, sink tool.ProgressFunc) func() {
	return t.progress.registerProgress(token, sink)
}

// failAll records the terminal read error and unblocks every pending call by
// closing its channel; a caller distinguishes this from a real response by the
// closed-channel receive.
func (t *stdioTransport) failAll(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.readErr == nil {
		t.readErr = err
	}
	for id, ch := range t.pending {
		close(ch)
		delete(t.pending, id)
	}
}

func (t *stdioTransport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	t.callMu.Lock()
	defer t.callMu.Unlock()

	t.mu.Lock()
	if t.readErr != nil {
		t.mu.Unlock()
		return nil, t.withStderr(fmt.Errorf("plugin %q: read: %w", t.name, t.readErr))
	}
	t.nextID++
	id := t.nextID
	ch := make(chan rpcResponse, 1)
	t.pending[id] = ch
	t.mu.Unlock()

	defer func() {
		t.mu.Lock()
		delete(t.pending, id)
		t.mu.Unlock()
	}()

	if err := t.write(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return nil, fmt.Errorf("plugin %q: write %s: %w", t.name, method, err)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return nil, t.withStderr(fmt.Errorf("plugin %q: read: %w", t.name, t.readErr))
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("plugin %q: %w", t.name, resp.Error)
		}
		return resp.Result, nil
	}
}

func (t *stdioTransport) notify(_ context.Context, method string, params any) error {
	return t.write(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

func (t *stdioTransport) write(v any) error {
	b, err := json.Marshal(v) // marshaled JSON never contains a literal newline
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err = t.stdin.Write(append(b, '\n')); err != nil {
		return t.withStderr(err)
	}
	return nil
}

func (t *stdioTransport) withStderr(err error) error {
	if t.stderr == nil {
		return err
	}
	// Reap the exited child so its stderr copy goroutine has flushed the tail.
	// Budgeted: a surviving grandchild keeps cmd.Wait blocked forever (see
	// close), and this path runs with callMu held — an unbounded wait here
	// would wedge every future call on this transport.
	waitWithBudget(t.wait, closeWaitBudget)
	// This error is returned directly to callers outside startup as well as
	// copied into diagnostics. Redact at the transport boundary so an early
	// child exit cannot bypass the startup-specific redaction layer.
	msg := secrets.RedactCredentials(t.stderr.String())
	if msg == "" {
		return err
	}
	return fmt.Errorf("%w: stderr: %s", err, msg)
}

func (t *stdioTransport) startupStderr() string {
	if t == nil || t.stderr == nil {
		return ""
	}
	return secrets.RedactCredentials(t.stderr.String())
}

// wait reaps the child exactly once; cmd.Wait blocks until the stderr-copy
// goroutine completes, so the tail buffer is settled before anyone reads it.
func (t *stdioTransport) wait() {
	t.waitOnce.Do(func() {
		if t.cmd != nil && t.cmd.Process != nil {
			_ = t.cmd.Wait()
		}
	})
}

// waitWithBudget runs wait in a goroutine and returns once it finishes or the
// budget elapses, whichever comes first. On timeout the goroutine is left to
// complete the reap in the background, so wait must be safe to abandon
// (stdioTransport.wait is single-shot via waitOnce).
func waitWithBudget(wait func(), budget time.Duration) {
	_ = waitFinishedWithinBudget(wait, budget)
}

func waitFinishedWithinBudget(wait func(), budget time.Duration) bool {
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(budget):
		return false
	}
}

// close first offers a short stdin-EOF grace period, then kills the whole
// process tree if needed (a launcher's surviving grandchild can otherwise keep
// inherited pipes open). Both paths are budgeted so one wedged server can never
// stall a boot or turn teardown.
func (t *stdioTransport) close() {
	if t.releaseSlot != nil {
		t.releaseSlot() // idempotent; frees the bounded CodeGraph instance slot
	}
	if t.stdin != nil {
		_ = t.stdin.Close()
	}
	if t.cmd == nil || t.cmd.Process == nil {
		return
	}
	// Give protocol-aware servers a short chance to observe stdin EOF and clean
	// up resources they launched outside the process group (Chrome isolated
	// profiles are the important case). Hard-kill after the bounded grace period
	// so an unresponsive MCP still cannot stall teardown.
	if waitFinishedWithinBudget(t.wait, gracefulCloseWaitBudget) {
		return
	}
	proc.KillTracked(t.cmd, t.job)
	waitWithBudget(t.wait, closeWaitBudget)
}

type tailBuffer struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if b.limit > 0 && len(b.buf) > b.limit {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.limit:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}
