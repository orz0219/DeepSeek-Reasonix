// Package extension is the Go SDK for Reasonix extension sidecars speaking
// Extension Protocol v2 over stdio. An extension is a separate process: the
// Reasonix host launches it, sends extension/initialize first, drives
// intercepts, events, provider streams, and UI calls, and finally asks it to
// stop with extension/shutdown.
//
// The transport is strict JSON-RPC 2.0 framed as NDJSON (one object per
// line, integer request ids, params as JSON objects, frames capped at
// FrameBytes). The SDK owns the wire, the handshake barrier, and the
// shutdown sequence; the extension implements Handler and, optionally,
// interceptors, a Provider, and UI callbacks via Options. After Initialize
// completes, the SDK may invoke up to 32 callbacks concurrently; extensions
// must synchronize any mutable state shared by those callbacks.
//
// After Serve returns nil from an orderly extension/shutdown the process
// should exit with code 0; the host reaps it by that exit status.
//
// Protocol reference: docs/EXTENSION_PROTOCOL.generated.md and
// internal/extension/protocol/schema.generated.json in the Reasonix
// repository.
package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
)

// Public callback types

// Handler is the one mandatory extension hook. Initialize is called once and
// completes before any other callback. Return the sidecar's declaration
// (name, version, subscriptions, replaces, providers, UI actions) — the host
// rejects anything beyond the installed manifest.
type Handler interface {
	Initialize(ctx context.Context, p InitializeParams) (*InitializeResult, error)
}

// InterceptorFunc rules on one intercepted event. payload is the event
// payload as raw JSON; content-ref externalized payloads are rehydrated
// before the call. Return one of Continue, Block, Replace, Allow, or Deny; a
// nil result is Continue. A non-nil error answers the intercept with the
// frozen internal error and the host proceeds with its default behavior.
type InterceptorFunc func(ctx context.Context, event string, payload json.RawMessage) (*InterceptResult, error)

// Provider brokers extension-hosted model providers. The extension holds the
// credentials; only the credential-free DTOs cross the wire.
type Provider interface {
	// Catalog returns the extension's full provider catalog. It may run
	// concurrently with other callbacks.
	Catalog(ctx context.Context) ([]ProviderDescriptor, error)
	// Stream opens one stream and returns its chunk channel. Stream must
	// return promptly; produce chunks in the background. The SDK numbers
	// chunks 1,2,3,… (from the host's SeqBase) and ends the stream with
	// exactly one stream/end: close the channel for a clean end, send an
	// ErrorChunk (or a chunk with Type ChunkError) to fail the stream with
	// end.error, and stop producing when ctx is cancelled (host cancel or
	// shutdown) — the SDK then ends the stream interrupted. Multiple Stream
	// calls may run concurrently.
	Stream(ctx context.Context, req StreamRequest) (<-chan StreamChunk, error)
}

// StreamRequest is one opened provider stream.
type StreamRequest struct {
	StreamID    string
	ProviderRef string
	Model       string
	Effort      string
	Request     ProviderRequest
}

// StreamChunk is one chunk a Provider produces; it is exactly the wire
// ProviderChunk. Build them with TextChunk, ReasoningChunk, UsageChunk,
// DoneChunk, and ErrorChunk.
type StreamChunk = ProviderChunk

// TextChunk is one assistant text delta.
func TextChunk(text string) StreamChunk { return StreamChunk{Type: ChunkText, Text: text} }

// ReasoningChunk is one reasoning delta with its optional signature.
func ReasoningChunk(text, signature string) StreamChunk {
	return StreamChunk{Type: ChunkReasoning, Text: text, Signature: signature}
}

// UsageChunk carries final token accounting.
func UsageChunk(usage ProviderUsage) StreamChunk {
	return StreamChunk{Type: ChunkUsage, Usage: &usage}
}

// DoneChunk marks the logical end of the assistant turn. The stream itself
// ends when the channel closes.
func DoneChunk() StreamChunk { return StreamChunk{Type: ChunkDone} }

// ErrorChunk fails the stream. The SDK ends it with stream/end.error set to
// the chunk's message instead of forwarding the chunk. Keep the message
// generic: it crosses the wire and must never contain credentials, endpoints,
// or response bodies.
func ErrorChunk(message string) StreamChunk {
	if strings.TrimSpace(message) == "" {
		message = frozenErrorSpecs[ErrProviderFailed].Message
	}
	return StreamChunk{Type: ChunkError, Error: &ProviderError{Code: ProviderFailed, Message: message}}
}

// UIHandler carries the extension's UI callbacks. A nil func makes the
// matching method answer unknown_method.
type UIHandler struct {
	// Action runs one handshake-declared action. A non-nil error answers
	// with {accepted:false, message}.
	Action func(ctx context.Context, actionID string, args map[string]string) error
	// Submit consumes one published form surface's values. A non-nil error
	// answers with {accepted:false} and is logged.
	Submit func(ctx context.Context, surfaceID string, values map[string]any) error
}

// Options configures Serve. Stdin/Stdout default to os.Stdin/os.Stdout. After
// Initialize, callback fields and Provider methods may be invoked concurrently
// (up to 32 inbound handlers); protect shared mutable maps, slices, counters,
// and clients with synchronization appropriate to the extension.
type Options struct {
	Stdin  io.Reader
	Stdout io.Writer
	// Name and Version fill InitializeResult when the Handler leaves them
	// empty.
	Name    string
	Version string
	// Interceptors maps an event name ("session.start", …) to its ruling
	// func; "*" is the wildcard fallback for events without an exact entry.
	Interceptors map[string]InterceptorFunc
	// Observer receives extension/event notifications. Events are
	// fire-and-forget; the observer cannot change host behavior.
	Observer func(ctx context.Context, event string, payload json.RawMessage)
	// ResourcesChanged receives extension/resources/changed notifications.
	ResourcesChanged func(ctx context.Context, paths []string)
	// Provider serves extension/provider/*; nil answers those methods with
	// unknown_method.
	Provider Provider
	// UI serves extension/ui/action and extension/ui/submit.
	UI UIHandler
	// Shutdown runs on extension/shutdown, bounded by the host's
	// TimeoutMillis. After it returns (or times out) the SDK answers
	// {accepted:true} and closes the transport; the process should then
	// exit(0).
	Shutdown func(ctx context.Context)
	// Logger receives stderr diagnostics (protocol violations, dropped
	// notifications, handler errors). Defaults to a stderr logger.
	Logger *log.Logger
}

// Sentinel errors

// ErrNotReady reports an Extension → Host call made before the handshake
// barrier opened: the sidecar must not send requests or notifications before
// the host's extension/initialized notification.
var ErrNotReady = errors.New("extension: host connection is not initialized (wait for extension/initialized)")

// ErrNoConnection reports a helper call (HostUI methods, ReadContentRef,
// ResolveExternalized) with a context that did not come from an SDK
// callback.
var ErrNoConnection = errors.New("extension: no host connection in context (use the context passed to an SDK callback)")

// ErrUICancelled reports a host prompt the user dismissed. UIRequestResult
// distinguishes dismissal from an empty value set; the SDK surfaces it as
// this sentinel.
var ErrUICancelled = errors.New("extension: the user dismissed the prompt")

// InterceptResult helpers

// Continue lets the event proceed unchanged.
func Continue() *InterceptResult { return &InterceptResult{Decision: DecisionContinue} }

// Block stops the event with a human-readable reason.
func Block(reason string) *InterceptResult {
	return &InterceptResult{Decision: DecisionBlock, Reason: reason}
}

// Replace substitutes the event payload. payload may be a json.RawMessage
// (used verbatim, must be valid JSON) or any marshalable value. The
// replacement travels inline; only the host can mint content refs, so a
// replacement must fit in one frame.
func Replace(payload any) (*InterceptResult, error) {
	var raw json.RawMessage
	switch value := payload.(type) {
	case json.RawMessage:
		raw = value
	case []byte:
		raw = value
	default:
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("extension: marshal replacement: %w", err)
		}
		raw = encoded
	}
	if !json.Valid(raw) {
		return nil, errors.New("extension: replacement is not valid JSON")
	}
	return &InterceptResult{Decision: DecisionReplace, Replacement: raw}, nil
}

// Allow grants a permission.decision intercept.
func Allow() *InterceptResult { return &InterceptResult{Decision: DecisionAllow} }

// Deny refuses a permission.decision intercept with a reason.
func Deny(reason string) *InterceptResult {
	return &InterceptResult{Decision: DecisionDeny, Reason: reason}
}

// Serve

type serverState uint8

const (
	stateNew serverState = iota
	// stateHandshake is entered when extension/initialize arrives and held
	// until the host's extension/initialized notification opens the barrier.
	stateHandshake
	stateReady
	stateShutdown
)

type server struct {
	conn    *conn
	handler Handler
	opts    Options
	log     *log.Logger

	mu           sync.Mutex
	state        serverState
	shutdownOnce sync.Once

	streamsMu sync.Mutex
	streams   map[string]*streamHandle
}

type streamHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type serverContextKey struct{}

func serverFrom(ctx context.Context) *server {
	s, _ := ctx.Value(serverContextKey{}).(*server)
	return s
}

// Serve runs the extension sidecar lifecycle on Options.Stdin/Stdout until
// the host closes the transport, asks for shutdown, or fatally violates the
// protocol. It returns nil on a clean end (host EOF or an answered
// extension/shutdown) and a non-nil error otherwise; canceling ctx tears
// everything down and returns the ctx error. After an orderly shutdown the
// process should exit(0).
func Serve(ctx context.Context, h Handler, opts Options) error {
	if h == nil {
		return errors.New("extension: Serve requires a non-nil Handler")
	}
	stdin := opts.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(os.Stderr, "reasonix-extension: ", log.LstdFlags)
	}
	s := &server{
		handler: h,
		opts:    opts,
		log:     logger,
		state:   stateNew,
		streams: make(map[string]*streamHandle),
	}
	c := newConn(stdin, stdout, logger)
	s.conn = c
	c.beforeRequest = s.gateRequest
	c.beforeNotification = s.gateNotification

	c.reqH[MethodExtensionInitialize] = s.withConnRequest(s.handleInitialize)
	c.reqH[MethodExtensionShutdown] = s.withConnRequest(s.handleShutdown)
	c.reqH[MethodExtensionIntercept] = s.withConnRequest(s.handleIntercept)
	c.reqH[MethodExtensionProviderCatalog] = s.withConnRequest(s.handleProviderCatalog)
	c.reqH[MethodExtensionProviderStreamOpen] = s.withConnRequest(s.handleStreamOpen)
	c.reqH[MethodExtensionProviderStreamCancel] = s.withConnRequest(s.handleStreamCancel)
	c.reqH[MethodExtensionUIAction] = s.withConnRequest(s.handleUIAction)
	c.reqH[MethodExtensionUISubmit] = s.withConnRequest(s.handleUISubmit)
	c.notH[MethodExtensionInitialized] = s.withConnNotification(s.handleInitialized)
	c.notH[MethodExtensionEvent] = s.withConnNotification(s.handleEvent)
	c.notH[MethodExtensionResourcesChanged] = s.withConnNotification(s.handleResourcesChanged)

	return c.serve(ctx)
}

// withConnRequest injects the server into handler contexts so HostUI,
// ReadContentRef, and ResolveExternalized can reach the transport.
func (s *server) withConnRequest(f requestHandler) requestHandler {
	return func(ctx context.Context, raw json.RawMessage) (any, error) {
		return f(context.WithValue(ctx, serverContextKey{}, s), raw)
	}
}

func (s *server) withConnNotification(f notificationHandler) notificationHandler {
	return func(ctx context.Context, raw json.RawMessage) {
		f(context.WithValue(ctx, serverContextKey{}, s), raw)
	}
}

// Handshake barrier

// gateRequest runs on the read loop before dispatch: the host must open with
// extension/initialize, and until its extension/initialized notification
// arrives only the lifecycle methods are served. Everything else is answered
// with the frozen protocol_error.
func (s *server) gateRequest(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case stateReady:
		return nil
	case stateNew:
		switch method {
		case MethodExtensionInitialize:
			s.state = stateHandshake
			return nil
		case MethodExtensionShutdown:
			return nil
		}
	case stateHandshake:
		if method == MethodExtensionShutdown {
			return nil
		}
	case stateShutdown:
		// fall through to the error below
	}
	return &ProtocolError{
		Reason:  ErrProtocolError,
		Message: fmt.Sprintf("extension protocol violation: host sent request %q before the handshake completed", method),
	}
}

// gateNotification applies the same barrier to notifications; violations are
// dropped (JSON-RPC notifications carry no response).
func (s *server) gateNotification(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case stateReady:
		return nil
	case stateHandshake:
		if method == MethodExtensionInitialized {
			s.state = stateReady
			return nil
		}
	}
	return fmt.Errorf("extension: dropping notification %q before the handshake completed", method)
}

// checkReady gates Extension → Host calls on the opened barrier.
func (s *server) checkReady() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != stateReady {
		return ErrNotReady
	}
	return nil
}

// Lifecycle handlers

// fatalError marks handler failures that must end the connection after the
// error response is written (a failed handshake leaves nothing to serve).
type fatalError struct{ err error }

func (e *fatalError) Error() string { return e.err.Error() }
func (e *fatalError) Unwrap() error { return e.err }

// compareProtocolVersion mirrors the host's handshake identity check.

// The barrier itself opened in gateNotification, synchronously on the
// read loop, so no later frame can overtake it.

// Orderly close: end in-flight calls, then close the read side so
// the read loop exits and the host sees EOF when the process
// exits. Serve returns nil.

// Intercept and observation

// The callback's advertised intercept budget expired. Return the
// frozen timeout reason rather than racing the host's identical timer
// with a generic internal error response.

// Provider broker

// The wire form requires an array; null fails the host's decoder.

// pumpStream forwards one provider channel onto the wire: chunks become
// stream/chunk notifications with contiguous 1-based seqs (from SeqBase),
// and exactly one stream/end closes the stream — clean on channel close,
// with error on an error chunk, interrupted on cancel. A cancel processed by
// the SDK is never trailed by another chunk.

// A cancel must never be trailed by one more chunk, so check before
// every receive and again before every send.

// UI handlers (Host → Extension)

// HostUI: Extension → Host UI client

// HostUI is the sidecar's client for the host's structured UI surfaces. The
// zero value is ready to use; every method takes the context of an SDK
// callback (interceptor, observer, provider, UI, or shutdown) and fails with
// ErrNoConnection otherwise, and with ErrNotReady before the handshake
// barrier opens. Surfaces are structured-only by design: there is no way to
// send HTML, CSS, JavaScript, or URLs.

// uiAnswerKey is the field key the host uses for single-field prompts.

// PublishStatus publishes or replaces a one-line status surface.

// PublishCard publishes or replaces a rich read-only card surface.

// PublishForm publishes or replaces an editable form surface; submissions
// return through the Options.UI.Submit callback.

// PublishNotification publishes a transient toast-style message.

// InputPrompt configures RequestInput.

// SelectPrompt configures RequestSelect.

// MultiSelectPrompt configures RequestMultiSelect.

// RequestConfirm blocks on a yes/no prompt; the bool is the user's answer.
// A dismissed prompt returns ErrUICancelled.

// RequestInput blocks on a free-text prompt and returns the entered text.

// RequestSelect blocks on a single-choice prompt and returns the picked
// option.

// RequestMultiSelect blocks on a multi-choice prompt and returns the picked
// options.

// RequestForm blocks on a fully custom form prompt and returns all values
// keyed by field key. It is the structured escape hatch behind the typed
// prompt helpers.

// Content refs (Extension → Host)

// ReadContentRef pages one whole content ref back from the host in
// ContentRefChunkBytes chunks, verifies the reassembled byte count and
// SHA-256 against the host's own report, and fails on any inconsistency. An
// expired or unknown ref returns a *ProtocolError with Reason
// ErrContentRefExpired.

// ResolveExternalized rehydrates one owner document's externalizable field.
// raw is the field's inline value and externalized the owner's envelope, at
// the schema-registered JSON pointer ("/payload" for intercept and event
// params, "/replacement" for intercept results). With an empty envelope the
// inline value passes through; otherwise the envelope must hold exactly the
// pointer's descriptor, and the ref is paged back and verified against the
// descriptor's byte count and SHA-256 before it is returned. An inline value
// alongside an envelope, a wrong pointer, or unverifiable content is a
// protocol error — never decode bytes the peer did not prove.
//
// Intercept and event payloads are resolved automatically before the
// interceptor/observer runs; this helper remains for manual use.

// shared helpers

// callHost issues one Extension → Host request behind the handshake barrier
// and maps a structured wire error back to a *ProtocolError.

// mapCallError converts a peer's JSON-RPC error into a *ProtocolError when it
// carries a frozen reason.

// strictDecode decodes one params/result document rejecting unknown fields
// and trailing JSON, mirroring the host's strict decoder envelope rules.

// jsonKeyPresent reports whether raw is an object containing key, for
// required-but-nullable fields such as the externalizable payload.
