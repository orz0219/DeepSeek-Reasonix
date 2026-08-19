// Package serve exposes a control.Controller over HTTP: the typed event stream
// as Server-Sent Events, and the commands as small JSON POST endpoints. It is a
// second frontend alongside the chat TUI — proof that the controller is
// transport-agnostic, and the basis for a browser/desktop client. One server
// drives one session; multiple browser tabs share it.
package serve

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/stats"
)

//go:embed index.html
var indexHTML []byte

//go:embed logo-wordmark.svg
var logoWordmarkSVG []byte

// Server wires a controller to its HTTP surface. The Broadcaster must be the
// same sink the controller was constructed with, so events reach SSE clients.
type Server struct {
	mu sync.RWMutex // guards ctrl, which rebuild paths swap at runtime
	// bindMu serializes every entry point that changes the active session
	// path or controller generation — /resume, /new, /fork, switchModel, and
	// extension reload. net/http runs handlers
	// concurrently and serve serves multiple browser tabs, so without this
	// two interleaved rebinds can leave the controller writing one session
	// while the lease keeper guards another (the exact split this feature
	// exists to prevent). It also keeps switchModel's Snapshot/Build/Close
	// off s.mu, as the narrower switchMu did before it was widened.
	bindMu sync.Mutex
	ctrl   control.SessionAPI
	bc     *Broadcaster
	// buildController builds the replacement controller during a model switch.
	// Nil in production (switchModel falls back to boot.Build); tests inject a
	// fake so switchModel can be exercised without real provider IO.
	buildController func(ctx context.Context, ref string) (*control.Controller, error)
	// rebuildController rebuilds the same model/runtime generation for an
	// extension reload. Tests inject it to exercise publication and failure
	// paths without starting real providers or sidecars.
	rebuildController func(ctx context.Context, old *control.Controller, ref string) (*control.Controller, error)
	titleProv         provider.Provider // lightweight flash provider for session titles
	titlePrice        *provider.Pricing
	titleModelRef     string
	titleUsageSink    event.Sink
	titles            *titleCache
	auth              *authGate // nil when auth is disabled
	providerSetupMu   sync.RWMutex
	providerSetup     providerSetupState
	// leases guards the active session file against other runtimes (a desktop
	// window, another CLI). Wired by the serve CLI command with the keeper that
	// already holds the startup session's lease; nil (tests, embedded use)
	// disables lease gating.
	leases *control.SessionLeaseKeeper
}

// New builds a Server. bc must be the controller's event sink.
// serveCfg controls authentication (none, token, or password).
func New(ctrl control.SessionAPI, bc *Broadcaster, serveCfg config.ServeConfig) *Server {
	if bc == nil {
		bc = NewBroadcaster()
	}
	s := &Server{
		ctrl:   ctrl,
		bc:     bc,
		titles: newTitleCache(ctrl.SessionDir()),
		auth:   newAuthGate(serveCfg),
	}
	if cfg, err := config.Load(); err == nil {
		bc.SetDisplayCurrency(cfg.ExplicitDisplayCurrency())
	}
	s.initTitleProvider()
	return s
}

// ctl returns the current controller. Handlers must read it through here, never
// the field directly, because switchModel replaces it under the write lock.
func (s *Server) ctl() control.SessionAPI {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ctrl
}

// resumeBindHookForTest, when set, runs inside /resume's critical sequence
// between the lease rebind and the controller Resume. Tests use it to force
// the interleaving bindMu exists to prevent; production never sets it.
var resumeBindHookForTest func()

// sessionInUseError renders a lease refusal for HTTP clients using the shared
// CLI wording, without the session file path.
func sessionInUseError(err error) string {
	return control.SessionInUseMessage(err) + "; " + control.SessionLeaseCloseHint
}

// AuthToken returns the pre-shared token when in token mode, or "" otherwise.
func (s *Server) AuthToken() string {
	if s.auth == nil {
		return ""
	}
	return s.auth.Token()
}

// AuthMode returns the authentication mode: "none", "token", or "password".
func (s *Server) AuthMode() string {
	if s.auth == nil {
		return "none"
	}
	return s.auth.Mode()
}

// initTitleProvider builds a lightweight flash-model provider used solely to
// generate short session titles. Errors are silently swallowed — title
// generation is best-effort, and the server works fine without it.
func (s *Server) initTitleProvider() {
	cfg, err := config.Load()
	if err != nil {
		return
	}
	entry, ok := cfg.ResolveModel("deepseek-flash")
	if !ok {
		return
	}
	prov, err := provider.New(entry.Kind, titleProviderConfig(entry))
	if err != nil {
		return
	}
	s.titleProv = prov
	s.titlePrice = entry.Price
	s.titleModelRef = entry.Name + "/" + entry.Model
	// Title generation is accounting-only; do not inject its usage event into
	// the shared chat SSE stream.
	s.titleUsageSink = stats.NewRecorder(event.Discard, config.StatsDir(), "serve")
}

func titleProviderConfig(entry *config.ProviderEntry) provider.Config {
	return provider.Config{
		Name:    entry.Name,
		BaseURL: entry.BaseURL,
		Model:   entry.Model,
		APIKey:  entry.APIKey(),
		// Title generation needs a short visible answer, not chain-of-thought.
		// "off" is a retired DeepSeek effort value and now falls back to high.
		Extra: map[string]any{"effort": "disabled"},
	}
}

// switchModel rebuilds the controller with a new model, carrying over the
// conversation history. This replicates the TUI/desktop model-switch path.
//
// The heavy steps — Snapshot (may touch disk), Build (provider init IO), and the
// old controller's Close (jobs.CloseWithGrace up to 15s + SessionEnd lifecycle event) — all
// run OFF s.mu. Holding the write lock across them would wedge every HTTP handler
// on s.ctl()'s RLock for the duration, stalling the whole serve frontend
// (mirrors the acp rebuildSession fix and PR #5920). bindMu serializes the
// switch against every other session-path-changing entry point (/resume,
// /new, /fork), preserving the old "second switch waits" semantics without
// pinning s.mu.
func (s *Server) switchModel(ctx context.Context, ref string) error {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()
	return s.switchModelLocked(ctx, ref)
}

// switchModelLocked performs switchModel while bindMu is held by the caller.
// Provider setup uses this form so credential persistence and the controller
// rebuild are one ordered operation relative to every session/model rebind.
func (s *Server) switchModelLocked(ctx context.Context, ref string) error {
	// Snapshot the current controller under a short read of s.mu only.
	cur := s.ctl()
	if controllerHasActiveRuntimeWork(cur) {
		return fmt.Errorf("cannot switch model while active work or background jobs are running")
	}

	// Off-lock: snapshot, carry history, and build the replacement. None of these
	// touch s.mu, so concurrent handlers keep reading the live controller.
	if err := cur.Snapshot(); err != nil {
		slog.Warn("serve: snapshot before model switch", "err", err)
	}
	// Capture the continue path and history only after Snapshot: a snapshot
	// conflict can retarget cur to a recovery branch (or adopt the newer disk
	// transcript), and a pre-snapshot capture would bind the rebuilt controller
	// back to the original file, re-conflicting on every later save.
	prevPath := cur.SessionPath()
	carried := cur.History()

	newCtrl, err := s.build(ctx, ref)
	if err != nil {
		return fmt.Errorf("switch model: %w", err)
	}
	// Run/RunGraceful only wire the initial controller. Every replacement must
	// receive the same frontend or the ask tool falls back to headless mode.
	newCtrl.EnableInteractiveApproval()
	// Keep the carried conversation in its existing file so the switch doesn't
	// orphan a duplicate (#2807).
	newPath := agent.ContinueSessionPath(prevPath, newCtrl.SessionDir(), newCtrl.Label())
	// The freshly built controller's own leading system message carries the
	// target profile's contract; AdoptHistory below replaces the whole
	// history with carried, so splice that message in first or the model
	// keeps seeing the outgoing profile's contract after every switch.
	if fresh := newCtrl.History(); len(fresh) > 0 && fresh[0].Role == provider.RoleSystem {
		if len(carried) > 0 && carried[0].Role == provider.RoleSystem {
			carried[0] = fresh[0]
		} else {
			carried = append([]provider.Message{fresh[0]}, carried...)
		}
	}
	newCtrl.AdoptHistory(carried, newPath)
	newCtrl.SetOnSessionRecovered(sessionLeaseRecoveryHandler(s.leases))
	// A rebuild must not force the user to re-approve tools already granted
	// this session, or re-trust Plan-mode read-only commands already trusted
	// this session.
	if prev, ok := cur.(*control.Controller); ok {
		newCtrl.RestoreSessionAuthorizations(prev.SessionAuthorizations())
	}
	// Persist before publishing the replacement. A failed write leaves cur and
	// the on-disk transcript coherent and lets the caller retry; publishing first
	// would report a successful switch whose refreshed system contract disappears
	// on restart. AdoptHistory retained the loaded CAS baseline for this rewrite.
	if err := s.rebindSessionLeaseFor(newPath, newCtrl); err != nil {
		newCtrl.Close()
		if errors.Is(err, agent.ErrSessionLeaseHeld) {
			return fmt.Errorf("switch model: %s", sessionInUseError(err))
		}
		return fmt.Errorf("switch model: unable to secure replacement session")
	}
	if newPath != "" {
		if err := newCtrl.Snapshot(); err != nil {
			if oldCtrl, ok := cur.(*control.Controller); ok {
				_ = s.rebindSessionLeaseFor(prevPath, oldCtrl)
			}
			newCtrl.Close()
			return fmt.Errorf("switch model: snapshot adopted history: %w", err)
		}
	}
	activePath := newCtrl.SessionPath()
	if err := s.rebindSessionLeaseFor(activePath, newCtrl); err != nil {
		newCtrl.Close()
		if errors.Is(err, agent.ErrSessionLeaseHeld) {
			return fmt.Errorf("switch model: %s", sessionInUseError(err))
		}
		slog.Error("serve: bind replacement session lease", "err", err)
		return fmt.Errorf("switch model: unable to secure replacement session")
	}

	// Publish the swap under a short write lock. bindMu already serializes
	// switches — today the only writer of s.ctrl — so the identity re-check is
	// defensive: it keeps a future controller-swapping path (or a test doing so)
	// from being silently clobbered after the off-lock build. On a mismatch,
	// discard the fresh controller off-lock instead of leaking it.
	s.mu.Lock()
	if s.ctrl != cur {
		s.mu.Unlock()
		oldCtrl, _ := cur.(*control.Controller)
		if restoreErr := s.rebindSessionLeaseFor(cur.SessionPath(), oldCtrl); restoreErr != nil {
			newCtrl.Close()
			slog.Error("serve: restore outgoing session lease after aborted model switch", "err", restoreErr)
			return fmt.Errorf("switch model: session changed during switch; unable to restore outgoing session ownership")
		}
		newCtrl.Close()
		return fmt.Errorf("switch model: session changed during switch")
	}
	s.ctrl = newCtrl
	s.mu.Unlock()
	s.refreshProviderSetup(currentModelRef(newCtrl))

	// Off-lock: tear down the old controller. Close can block up to 15s.
	cur.Close()
	return nil
}

// build returns the replacement controller for a model switch, using the
// injected builder in tests and boot.Build in production.
func (s *Server) build(ctx context.Context, ref string) (*control.Controller, error) {
	if s.buildController != nil {
		return s.buildController(ctx, ref)
	}
	opts := boot.Options{
		Model:       ref,
		Sink:        s.bc,
		Stderr:      os.Stderr,
		StatsSource: "serve",
	}
	// Keep the logical-session private temporary directory across model switches.
	if cur, ok := s.ctl().(*control.Controller); ok && cur != nil {
		opts.SessionTemp = cur.SessionTemp()
	}
	return boot.Build(ctx, opts)
}

// reloadExtensions fail-atomically rebuilds the active controller generation
// so extension package/config changes take effect. The old controller remains
// live until the replacement has inherited state, snapshotted successfully,
// secured the session lease, and won the short publication lock.
func (s *Server) reloadExtensions(ctx context.Context) error {
	s.bindMu.Lock()
	defer s.bindMu.Unlock()

	curAPI := s.ctl()
	if controllerHasActiveRuntimeWork(curAPI) {
		return fmt.Errorf("cannot reload extensions while active work or background jobs are running")
	}
	cur, ok := curAPI.(*control.Controller)
	if !ok {
		return fmt.Errorf("cannot reload extensions for this controller implementation")
	}
	if err := cur.Snapshot(); err != nil {
		slog.Warn("serve: snapshot before extension reload", "err", err)
	}

	ref := currentModelRef(cur)
	newCtrl, err := s.rebuild(ctx, cur, ref)
	if err != nil {
		return fmt.Errorf("reload extensions: %w", err)
	}
	newCtrl.EnableInteractiveApproval()
	newCtrl.SetOnSessionRecovered(sessionLeaseRecoveryHandler(s.leases))
	if err := s.rebindSessionLeaseFor(newCtrl.SessionPath(), newCtrl); err != nil {
		newCtrl.Close()
		if errors.Is(err, agent.ErrSessionLeaseHeld) {
			return fmt.Errorf("reload extensions: %s", sessionInUseError(err))
		}
		return fmt.Errorf("reload extensions: unable to secure replacement session")
	}
	if newCtrl.SessionPath() != "" {
		if err := newCtrl.Snapshot(); err != nil {
			_ = s.rebindSessionLeaseFor(cur.SessionPath(), cur)
			newCtrl.Close()
			return fmt.Errorf("reload extensions: snapshot migrated session: %w", err)
		}
	}
	if err := s.rebindSessionLeaseFor(newCtrl.SessionPath(), newCtrl); err != nil {
		newCtrl.Close()
		if errors.Is(err, agent.ErrSessionLeaseHeld) {
			return fmt.Errorf("reload extensions: %s", sessionInUseError(err))
		}
		return fmt.Errorf("reload extensions: unable to secure replacement session")
	}

	s.mu.Lock()
	if s.ctrl != curAPI {
		s.mu.Unlock()
		if restoreErr := s.rebindSessionLeaseFor(cur.SessionPath(), cur); restoreErr != nil {
			newCtrl.Close()
			slog.Error("serve: restore outgoing session lease after aborted extension reload", "err", restoreErr)
			return fmt.Errorf("reload extensions: session changed during reload; unable to restore outgoing session ownership")
		}
		newCtrl.Close()
		return fmt.Errorf("reload extensions: session changed during reload")
	}
	s.ctrl = newCtrl
	s.mu.Unlock()
	s.refreshProviderSetup(currentModelRef(newCtrl))

	cur.Close()
	return nil
}

func (s *Server) rebuild(ctx context.Context, old *control.Controller, ref string) (*control.Controller, error) {
	if s.rebuildController != nil {
		return s.rebuildController(ctx, old, ref)
	}
	res, err := boot.Rebuild(ctx, old, boot.Options{
		Model:       ref,
		Sink:        s.bc,
		Stderr:      os.Stderr,
		StatsSource: "serve",
	})
	if err != nil {
		return nil, err
	}
	return res.Controller, nil
}

// switchEffort persists a new reasoning-effort level for the active provider and
// rebuilds via switchModel (which serializes on bindMu).
func (s *Server) switchEffort(ctx context.Context, level string) error {
	cur := s.ctl()
	if controllerHasActiveRuntimeWork(cur) {
		return fmt.Errorf("cannot change effort while active work or background jobs are running")
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ref := currentModelRef(cur)
	entry, ok := cfg.ResolveModel(ref)
	if !ok {
		return fmt.Errorf("cannot resolve current provider %q", ref)
	}
	if !config.EffortCapabilityForEntry(entry).Supported {
		return fmt.Errorf("effort is not configurable for %s", entry.Name)
	}
	effort, err := config.NormalizeEffort(entry, level)
	if err != nil {
		return err
	}
	editPath := config.UserConfigPath()
	if editPath == "" {
		return fmt.Errorf("no config file found")
	}
	// Lock only the load-modify-save cycle; switchModel below rebuilds the
	// controller and must not hold the config edit lock.
	if err := func() error {
		unlock := config.LockUserConfigEdits()
		defer unlock()
		edit := config.LoadForEdit(editPath)
		if err := applyEffortEdit(edit, entry, effort); err != nil {
			return err
		}
		if err := edit.SaveTo(editPath); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		return nil
	}(); err != nil {
		return err
	}
	return s.switchModel(ctx, entry.Name+"/"+entry.Model)
}

func controllerHasActiveRuntimeWork(ctrl control.SessionAPI) bool {
	if ctrl == nil {
		return false
	}
	status := ctrl.RuntimeStatus()
	return status.Running || status.PendingPrompt || status.BackgroundJobs > 0
}

// applyEffortEdit writes effort onto entry within edit, mirroring CLI/desktop
// SetEffort: upsert the provider when the user config has no block for it yet, and
// enable adaptive thinking for Anthropic so the effort knob actually engages.
func applyEffortEdit(edit *config.Config, entry *config.ProviderEntry, effort string) error {
	if _, ok := edit.Provider(entry.Name); !ok {
		if err := edit.UpsertProvider(*entry); err != nil {
			return err
		}
	}
	if entry.Kind == "anthropic" && effort != "" && entry.Thinking == "" {
		if err := edit.SetProviderThinking(entry.Name, "adaptive"); err != nil {
			return err
		}
	}
	return edit.SetProviderEffort(entry.Name, effort)
}

// Handler returns the HTTP routes: GET / (a minimal browser client), GET /events
// (SSE), GET /history, GET /context, and POST command endpoints.
// CORS is NOT applied by default — same-origin policy protects the unauthenticated
// agent endpoints. Call HandlerWithCORS to opt in for local development.
func (s *Server) Handler() http.Handler {
	return s.handler()
}

// HandlerWithCORS returns the same routes as Handler but adds permissive CORS
// headers so a dev frontend on a different origin (e.g. Vite on :5173) can
// reach the server. Do NOT use in production — the server has no auth.
func (s *Server) HandlerWithCORS(origin string) http.Handler {
	return corsMiddleware(s.handler(), origin)
}
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.index)
	mux.HandleFunc("GET /sessions/{id}", s.index)
	mux.HandleFunc("GET /assets/logo-wordmark.svg", s.logoWordmark)
	mux.HandleFunc("GET /provider-setup", s.providerSetupStatus)
	mux.HandleFunc("POST /provider-setup", s.providerSetupSave)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /history", s.history)
	mux.HandleFunc("GET /context", s.context)
	mux.HandleFunc("POST /submit", s.submit)
	s.registerInboxRoutes(mux)
	mux.HandleFunc("POST /cancel", s.cancel)
	mux.HandleFunc("POST /approve", s.approve)
	mux.HandleFunc("POST /plan", s.plan)
	mux.HandleFunc("POST /compact", s.compact)
	mux.HandleFunc("POST /new", s.newSession)
	mux.HandleFunc("POST /rewind", s.rewind)
	mux.HandleFunc("POST /fork", s.fork)
	mux.HandleFunc("POST /summarize", s.summarize)
	mux.HandleFunc("POST /tool-approval-mode", s.toolApprovalMode)
	mux.HandleFunc("POST /auto-approve-tools", s.autoApproveTools)
	mux.HandleFunc("POST /bypass", s.bypass)
	mux.HandleFunc("POST /goal", s.goal)
	mux.HandleFunc("POST /answer", s.answer)
	mux.HandleFunc("POST /resume", s.resume)
	mux.HandleFunc("GET /checkpoints", s.checkpoints)
	mux.HandleFunc("GET /branches", s.branches)
	mux.HandleFunc("GET /models", s.models)
	mux.HandleFunc("POST /extensions/reload", s.reloadExtensionsHTTP)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("GET /sessions", s.sessions)
	mux.HandleFunc("GET /skills", s.skills)
	mux.HandleFunc("GET /todos", s.todos)
	mux.HandleFunc("POST /delete-session", s.deleteSession)
	return logMiddleware(s.auth.middleware(csrfGuard(mux)))
}

func (s *Server) reloadExtensionsHTTP(w http.ResponseWriter, r *http.Request) {
	if err := s.reloadExtensions(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// csrfGuard rejects state-changing requests that don't carry a JSON content type.
// The command endpoints have no auth and bind to localhost, so a page the user
// visits could otherwise drive them with a simple cross-origin POST (text/plain,
// no preflight) — submitting prompts or auto-approving tool calls. Requiring
// application/json forces a CORS preflight the unauthenticated server never
// answers, blocking cross-site requests; the same-origin frontend (which always
// sends JSON) is unaffected.
func csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			ct := r.Header.Get("Content-Type")
			if i := strings.IndexByte(ct, ';'); i >= 0 {
				ct = ct[:i]
			}
			if !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
				http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Run serves until the process is killed. Interactive approval is enabled so
// "ask" decisions surface as approval_request events answered via POST /approve.
func (s *Server) Run(addr string) error {
	s.ctl().EnableInteractiveApproval()
	return http.ListenAndServe(addr, s.Handler())
}

// RunGraceful serves with graceful shutdown. It listens for SIGINT/SIGTERM on
// the provided context and drains active connections for up to 10 seconds
// before returning.
func (s *Server) RunGraceful(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.RunGracefulListener(ctx, ln)
}

// RunGracefulListener is RunGraceful over a caller-supplied listener. Callers
// that need the real bound address (e.g. --addr 127.0.0.1:0 with --port-file)
// listen first, record ln.Addr(), then hand the listener here.
func (s *Server) RunGracefulListener(ctx context.Context, ln net.Listener) error {
	s.ctl().EnableInteractiveApproval()
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		slog.Info("serve: shutting down gracefully")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("serve: graceful shutdown failed", "err", err)
		}
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	if setup, ok := s.providerSetupSnapshot(); ok && setup.Required {
		s.providerSetupIndex(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = config.MigrateLegacyIfNeeded()
	lang := "auto"
	if cfg, err := config.Load(); err == nil {
		if dl := cfg.DesktopLanguage(); dl != "" {
			lang = dl
		}
	}
	html := string(indexHTML)
	html = strings.ReplaceAll(html, "__LANG__", lang)
	_, _ = w.Write([]byte(html))
}

func (s *Server) logoWordmark(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(logoWordmarkSVG)
}

// sseKeepaliveInterval is how often the /events handler emits a `: ping`
// SSE comment. Most reverse proxies (nginx, ALB, Cloudflare) close idle
// upstream connections after 30–60 s; a long quiet turn (the agent
// thinking, the model generating a single long response) easily hits
// that window. The comment is one byte on the wire and is dropped by
// the EventSource client, so it's a no-op for the consumer while it
// keeps the TCP socket warm for the proxy.
const sseKeepaliveInterval = 15 * time.Second

// events streams the controller's event flow as SSE until the client
// disconnects. Each event is one `data:` frame of the JSON wire form.

// Subscribe and replay as one handoff. Prompt producers are serialized with
// this operation, so no original event can land between the two steps.

// open the stream immediately

// SSE comment lines start with `:` and are ignored by the
// client. Emit one every sseKeepaliveInterval so the
// upstream socket stays warm; without this, a long quiet
// turn (e.g. a model thinking) lets a proxy like nginx
// or an ALB close the idle connection and the next
// event arrives on a half-closed stream.

// submit runs raw user input as a turn (slash commands and @-references
// resolved by the controller). Returns 202 — output arrives on the event stream.
// An optional "format":"json_object" asks the model for structured JSON output
// on this turn (text.format on the wire).

// Supported: empty = default text output, json_object = structured.

// Intercept /model <ref> for runtime model switching (the controller's
// Submit path only lists models — switching is frontend-specific).

// Intercept /effort <level> for reasoning effort switching.

// Serialize turn admission with controller-generation rebuilds. Admission
// marks an ordinary turn running synchronously, so a reload that follows
// observes the busy state; a submit that follows a reload targets only the
// published replacement. This closes the check/build/swap race where a
// request could otherwise start on cur after reload's initial busy check.

// Fix false 202 while a turn is active: SubmitHTTPFormat silently drops
// concurrent input. Clients must use POST /inbox/items for durable follow-up.

// After synchronous admission, a successful start sets Running. A silent
// drop (rotating/closed) leaves Running false — return 409 instead of 202.
// Finishing-window park also leaves Running false briefly; prefer 202 only
// when Running or a pending prompt is observed, else durable-queue guidance.

// Persist the compacted session to disk — ctrl.Compact() only mutates in-memory.

// Session-path-changing entry point: serialize with /resume, /fork, and
// switchModel so the controller and the lease keeper move together.

// Fresh path — the lease follows it; failure is theoretical but not silent.

// Steer messages are surfaced as a notice, not a user message.

// history returns the session's message log so a reconnecting client can
// repopulate its transcript, including historical tool cards. Supports ETag caching:
// if the client sends If-None-Match with the current ETag, the server returns
// 304 Not Modified with no body, saving bandwidth on reconnects.

// context returns the prompt-vs-window gauge numbers. Supports ETag caching
// so reconnecting clients avoid re-fetching unchanged context data.

// writeJSONCached encodes v as JSON, computes a weak ETag from the body, and
// returns 304 Not Modified if the client's If-None-Match matches. This avoids
// re-sending unchanged history/context payloads on every reconnect.

// corsMiddleware adds CORS headers for a specific allowed origin. Only use for
// local development — the server has no auth, so broad CORS would let any site
// drive the agent. origin is the exact origin to allow (e.g.
// "http://localhost:5173"); empty origin skips CORS entirely.

// logMiddleware logs each request's method, path, and status.

// responseWriter captures the status code for logging.

// Flush delegates to the underlying ResponseWriter if it supports flushing
// (required for SSE /events). Without this the type assertion in the events
// handler fails and the stream endpoint returns 500.

// rewind rewinds the session to a checkpoint.

// "code", "conversation", "both"

// fork creates a new branch at a checkpoint.

// Session-path-changing critical sequence: serialize with /resume, /new,
// and switchModel so the controller and the lease keeper move together.
// Taken after body decoding so a slow client cannot hold the binding lock.

// The controller switched to the fork (a fresh path); the lease follows it.

// summarize runs summarize-from or summarize-up-to on a turn.

// "from" or "upto"

// autoApproveTools toggles YOLO/full-access tool auto-approval.

// toolApprovalMode selects ask, auto, or yolo approval behavior for interactive
// frontends. Plan remains a separate workflow governed by the selected mode.

// bypass is the legacy HTTP endpoint for YOLO/full-access tool auto-approval.

// goal sets or clears the active goal. An empty goal string clears it.
// Setting a non-empty goal disables plan mode (matching the desktop behavior).

// Disable plan mode before setting the goal, mirroring the desktop.

// answer responds to an ask_request.

// resume loads a previous session from a JSONL file.

// Serialize with /new, /fork, and switchModel so the controller and lease
// cannot land on different sessions. Validate first to avoid slow holders.

// Snapshot the current session before switching away — while this process
// still holds its lease.

// Refuse to bind a session another runtime is writing (a desktop window,
// another CLI); on success the lease now guards the resume target.

// The lease already moved to the target; re-point it at the session the
// controller still owns (best-effort).

// forget deletes a saved memory by name.

// checkpoints returns the session's checkpoint list for the rewind picker.

// branches returns the branch list and tree text.

// models lists configured chat models for the browser model picker.

// ProviderCatalog is the controller-generation's authoritative merged view.
// Add descriptors not already represented by configured providers; this is
// where plugin/<plugin>/<provider>/<model> refs enter the Serve picker.

// ProviderCatalog also contains the config-backed base. Configured
// base refs were handled above; do not resurrect unconfigured ones.

// status returns a combined status snapshot.

// Runtime-only hint: a single wallet currency may select an existing
// valuation, but is never persisted as configuration or history.

// generateTitle calls a lightweight LLM to produce a short session title.
// Returns empty string on any error — callers should fall back to a preview.

// sessions lists saved session files from the session directory, enriched with
// LLM-generated titles and turn counts.

// Event-log aware: reading the .jsonl checkpoint directly would freeze
// turn counts and titles at the last checkpoint write.

// reverse so newest first

// deleteSession removes a saved session by the session name returned from /sessions.

// sessionTitle returns a title for a session: the cached flash-generated title
// when its first user message is unchanged, otherwise a freshly generated one
// (cached for next time), falling back to a truncated preview when generation
// is off.

// skills lists discoverable skills.

// todos returns the canonical task list (latest todo_write state merged with
// complete_step advances) so the frontend can render a live task panel.
