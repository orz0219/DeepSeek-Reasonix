package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

func (s *server) handleInitialize(ctx context.Context, raw json.RawMessage) (any, error) {
	var p InitializeParams
	if err := strictDecode(raw, &p); err != nil {
		return nil, MustProtocolError(ErrInvalidParams)
	}
	if err := compareProtocolVersion(p.ProtocolID, p.ProtocolVersion); err != nil {
		return nil, &fatalError{err: err}
	}
	result, err := s.handler.Initialize(ctx, p)
	if err != nil {
		s.log.Printf("extension: initialize handler failed: %v", err)
		return nil, &fatalError{err: err}
	}
	if result == nil {
		return nil, &fatalError{err: errors.New("extension: Initialize returned a nil result")}
	}
	result.ProtocolVersion = ProtocolVersion
	if result.Name == "" {
		result.Name = s.opts.Name
	}
	if result.Version == "" {
		result.Version = s.opts.Version
	}
	if strings.TrimSpace(result.Name) == "" || strings.TrimSpace(result.Version) == "" {
		return nil, &fatalError{err: errors.New("extension: initialize result requires a name and version")}
	}
	if result.StateSchemaVersion < 0 {
		return nil, &fatalError{err: errors.New("extension: stateSchemaVersion must be non-negative")}
	}
	return result, nil
}

// compareProtocolVersion mirrors the host's handshake identity check.
func compareProtocolVersion(peerID, peerVersion string) error {
	if peerID != ProtocolID {
		return MustProtocolError(ErrUnsupportedVersion)
	}
	major, err := strconv.Atoi(peerVersion)
	if err != nil {
		return MustProtocolError(ErrProtocolError)
	}
	if major != ProtocolMajor {
		return MustProtocolError(ErrUnsupportedVersion)
	}
	return nil
}

func (s *server) handleInitialized(context.Context, json.RawMessage) {

}

func (s *server) handleShutdown(ctx context.Context, raw json.RawMessage) (any, error) {
	var p ShutdownParams
	if err := strictDecode(raw, &p); err != nil || p.TimeoutMillis < 0 {
		return nil, MustProtocolError(ErrInvalidParams)
	}
	s.shutdownOnce.Do(func() {
		s.mu.Lock()
		s.state = stateShutdown
		s.mu.Unlock()
		if s.opts.Shutdown != nil {
			fnCtx := ctx
			cancel := func() {}
			if p.TimeoutMillis > 0 {
				fnCtx, cancel = context.WithTimeout(ctx, time.Duration(p.TimeoutMillis)*time.Millisecond)
			}
			defer cancel()
			done := make(chan struct{})
			go func() {
				s.opts.Shutdown(fnCtx)
				close(done)
			}()
			select {
			case <-done:
			case <-fnCtx.Done():
				s.log.Printf("extension: shutdown function did not return within %dms", p.TimeoutMillis)
			}
		}
	})
	return deferredResult{
		result: ShutdownResult{Accepted: true},
		after: func() {

			s.conn.shutdown(nil)
			if closer, ok := s.conn.r.(io.Closer); ok {
				_ = closer.Close()
			}
		},
	}, nil
}

func (s *server) handleIntercept(ctx context.Context, raw json.RawMessage) (any, error) {
	var p InterceptParams
	if err := strictDecode(raw, &p); err != nil {
		return nil, MustProtocolError(ErrInvalidParams)
	}
	if !validInterceptEvent(p.Event) || p.Seq < 1 || p.TimeoutMillis < 0 || !jsonKeyPresent(raw, "payload") {
		return nil, MustProtocolError(ErrInvalidParams)
	}
	payload, err := s.rehydrate(ctx, p.Payload, p.Externalized, "/payload")
	if err != nil {
		return nil, err
	}
	fn := s.opts.Interceptors[string(p.Event)]
	if fn == nil {
		fn = s.opts.Interceptors["*"]
	}
	if fn == nil {
		return Continue(), nil
	}
	if p.TimeoutMillis > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(p.TimeoutMillis)*time.Millisecond)
		defer cancel()
	}
	result, err := fn(ctx, string(p.Event), payload)
	if err != nil {

		if errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, MustProtocolError(ErrInterceptTimeout)
		}
		return nil, err
	}
	if result == nil {
		return Continue(), nil
	}
	if !validInterceptDecision(result.Decision) {
		return nil, fmt.Errorf("extension: interceptor for %q returned invalid decision %q", p.Event, result.Decision)
	}
	return result, nil
}

func (s *server) handleEvent(ctx context.Context, raw json.RawMessage) {
	var p EventParams
	if err := strictDecode(raw, &p); err != nil || !validInterceptEvent(p.Event) || !jsonKeyPresent(raw, "payload") {
		s.log.Printf("extension: dropping malformed event notification")
		return
	}
	payload, err := s.rehydrate(ctx, p.Payload, p.Externalized, "/payload")
	if err != nil {
		s.log.Printf("extension: dropping event %q: %v", p.Event, err)
		return
	}
	if s.opts.Observer != nil {
		s.opts.Observer(ctx, string(p.Event), payload)
	}
}

func (s *server) handleResourcesChanged(ctx context.Context, raw json.RawMessage) {
	var p ResourcesChangedParams
	if err := strictDecode(raw, &p); err != nil || p.Paths == nil {
		s.log.Printf("extension: dropping malformed resources/changed notification")
		return
	}
	if s.opts.ResourcesChanged != nil {
		s.opts.ResourcesChanged(ctx, p.Paths)
	}
}

func (s *server) handleProviderCatalog(ctx context.Context, raw json.RawMessage) (any, error) {
	if s.opts.Provider == nil {
		return nil, MustProtocolError(ErrUnknownMethod)
	}
	if err := strictDecode(raw, &ProviderCatalogParams{}); err != nil {
		return nil, MustProtocolError(ErrInvalidParams)
	}
	providers, err := s.opts.Provider.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	if providers == nil {

		providers = []ProviderDescriptor{}
	}
	return ProviderCatalogResult{Providers: providers}, nil
}

func (s *server) handleStreamOpen(ctx context.Context, raw json.RawMessage) (any, error) {
	if s.opts.Provider == nil {
		return nil, MustProtocolError(ErrUnknownMethod)
	}
	var p StreamOpenParams
	if err := strictDecode(raw, &p); err != nil {
		return nil, MustProtocolError(ErrInvalidParams)
	}
	if p.SeqBase < 0 {
		return nil, MustProtocolError(ErrInvalidParams)
	}
	if err := p.Validate(); err != nil {
		return nil, MustProtocolError(ErrInvalidParams)
	}
	streamCtx, cancel := context.WithCancel(ctx)
	chunks, err := s.opts.Provider.Stream(streamCtx, StreamRequest{
		StreamID:    p.StreamID,
		ProviderRef: p.ProviderRef,
		Model:       p.Model,
		Effort:      p.Effort,
		Request:     p.Request,
	})
	if err != nil {
		cancel()
		s.log.Printf("extension: provider stream %q failed to open: %v", p.StreamID, err)
		return nil, MustProtocolError(ErrProviderFailed)
	}
	if chunks == nil {
		cancel()
		return nil, errors.New("extension: provider returned a nil chunk channel")
	}
	handle := &streamHandle{cancel: cancel, done: make(chan struct{})}
	s.streamsMu.Lock()
	if _, exists := s.streams[p.StreamID]; exists {
		s.streamsMu.Unlock()
		cancel()
		return nil, &ProtocolError{Reason: ErrProtocolError, Message: "duplicate stream id " + p.StreamID}
	}
	s.streams[p.StreamID] = handle
	s.streamsMu.Unlock()
	return deferredResult{
		result: StreamOpenResult{Accepted: true},
		after:  func() { go s.pumpStream(streamCtx, p.StreamID, p.SeqBase, chunks, handle) },
	}, nil
}

func (s *server) handleStreamCancel(_ context.Context, raw json.RawMessage) (any, error) {
	var p StreamCancelParams
	if err := strictDecode(raw, &p); err != nil || strings.TrimSpace(p.StreamID) == "" {
		return nil, MustProtocolError(ErrInvalidParams)
	}
	s.streamsMu.Lock()
	handle := s.streams[p.StreamID]
	s.streamsMu.Unlock()
	if handle == nil {
		return StreamCancelResult{Cancelled: false}, nil
	}
	handle.cancel()
	return StreamCancelResult{Cancelled: true}, nil
}

// pumpStream forwards one provider channel onto the wire: chunks become
// stream/chunk notifications with contiguous 1-based seqs (from SeqBase),
// and exactly one stream/end closes the stream — clean on channel close,
// with error on an error chunk, interrupted on cancel. A cancel processed by
// the SDK is never trailed by another chunk.
func (s *server) pumpStream(ctx context.Context, streamID string, seqBase int, chunks <-chan StreamChunk, handle *streamHandle) {
	defer close(handle.done)
	defer func() {
		s.streamsMu.Lock()
		delete(s.streams, streamID)
		s.streamsMu.Unlock()
	}()
	seq := int64(seqBase)
	if seq < 1 {
		seq = 1
	}
	var lastSeq int64
	end := StreamEndParams{StreamID: streamID}
	for {

		select {
		case <-ctx.Done():
			end.LastSeq, end.Interrupted = lastSeq, true
			s.sendStreamEnd(&end)
			return
		default:
		}
		select {
		case <-ctx.Done():
			end.LastSeq, end.Interrupted = lastSeq, true
			s.sendStreamEnd(&end)
			return
		case chunk, ok := <-chunks:
			if !ok {
				end.LastSeq = lastSeq
				s.sendStreamEnd(&end)
				return
			}
			if chunk.Type == ChunkError {
				end.LastSeq = lastSeq
				end.Error = frozenErrorSpecs[ErrProviderFailed].Message
				if chunk.Error != nil && strings.TrimSpace(chunk.Error.Message) != "" {
					end.Error = chunk.Error.Message
				}
				s.sendStreamEnd(&end)
				return
			}
			if err := chunk.Validate(); err != nil {
				s.log.Printf("extension: provider stream %q produced an invalid chunk: %v", streamID, err)
				end.LastSeq = lastSeq
				end.Error = "the extension provider produced an invalid chunk"
				s.sendStreamEnd(&end)
				return
			}
			if err := s.conn.notify(MethodExtensionProviderStreamChunk, StreamChunkParams{
				StreamID: streamID, Seq: seq, Chunk: chunk,
			}); err != nil {
				s.log.Printf("extension: provider stream %q could not deliver chunk %d: %v", streamID, seq, err)
				return
			}
			lastSeq = seq
			seq++
		}
	}
}

func (s *server) sendStreamEnd(end *StreamEndParams) {
	if err := s.conn.notify(MethodExtensionProviderStreamEnd, *end); err != nil {
		s.log.Printf("extension: provider stream %q could not deliver stream end: %v", end.StreamID, err)
	}
}

func (s *server) handleUIAction(ctx context.Context, raw json.RawMessage) (any, error) {
	if s.opts.UI.Action == nil {
		return nil, MustProtocolError(ErrUnknownMethod)
	}
	var p UIActionParams
	if err := strictDecode(raw, &p); err != nil || strings.TrimSpace(p.ActionID) == "" || strings.TrimSpace(p.SessionID) == "" {
		return nil, MustProtocolError(ErrInvalidParams)
	}
	if err := s.opts.UI.Action(ctx, p.ActionID, p.Args); err != nil {
		return UIActionResult{Accepted: false, Message: err.Error()}, nil
	}
	return UIActionResult{Accepted: true}, nil
}

func (s *server) handleUISubmit(ctx context.Context, raw json.RawMessage) (any, error) {
	if s.opts.UI.Submit == nil {
		return nil, MustProtocolError(ErrUnknownMethod)
	}
	var p UISubmitParams
	if err := strictDecode(raw, &p); err != nil || strings.TrimSpace(p.SurfaceID) == "" ||
		strings.TrimSpace(p.SessionID) == "" || p.Values == nil {
		return nil, MustProtocolError(ErrInvalidParams)
	}
	if err := s.opts.UI.Submit(ctx, p.SurfaceID, p.Values); err != nil {
		s.log.Printf("extension: UI submit for surface %q failed: %v", p.SurfaceID, err)
		return UISubmitResult{Accepted: false}, nil
	}
	return UISubmitResult{Accepted: true}, nil
}
