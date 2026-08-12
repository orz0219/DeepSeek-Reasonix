package rpcwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// Notify sends a fire-and-forget notification.
func (c *Conn) Notify(method string, params any) error {
	m, err := notification(method, params)
	if err != nil {
		return err
	}
	err = c.write(context.Background(), m)
	if err != nil {
		var tooLarge *FrameTooLargeError
		if !errors.As(err, &tooLarge) {
			c.fail(err)
		}
	}
	return err
}

// TryNotify enqueues a fire-and-forget notification without waiting for a
// physical write. A nil result means the bounded writer accepted the frame,
// not that the peer has processed it. When the queue is full it returns
// OutboundQueueFullError immediately, allowing observation-only callers to
// drop the event instead of adding sidecar backpressure to a host hot path.
func (c *Conn) TryNotify(method string, params any) error {
	m, err := notification(method, params)
	if err != nil {
		return err
	}
	job, err := c.prepareWrite(m, context.Background())
	if err != nil {
		return err
	}
	select {
	case c.tryNotifySlots <- struct{}{}:
		job.release = func() { <-c.tryNotifySlots }
	default:
		return &OutboundQueueFullError{Limit: cap(c.tryNotifySlots)}
	}
	if err := c.enqueueWrite(job, false); err != nil {
		job.release()
		return err
	}
	return nil
}

func notification(method string, params any) (outbound, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return outbound{}, err
	}
	return outbound{JSONRPC: "2.0", Method: method, Params: raw}, nil
}

// Request sends a request and waits for its response, cancellation, or closure.
func (c *Conn) Request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	id := c.nextID.Add(1)
	ch := make(chan rpcResult, 1)
	c.pmu.Lock()
	select {
	case <-c.closed:
		c.pmu.Unlock()
		closedErr := c.terminalError()
		if closedErr == nil {
			closedErr = fmt.Errorf("%s: connection closed", c.opts.Name)
		}
		return nil, closedErr
	default:
	}
	c.pending[id] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
	}()

	idRaw, _ := json.Marshal(id)
	if err := c.write(ctx, outbound{JSONRPC: "2.0", ID: idRaw, Method: method, Params: raw}); err != nil {
		var tooLarge *FrameTooLargeError

		if !errors.As(err, &tooLarge) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			c.fail(err)
		}
		return nil, err
	}
	select {
	case res := <-ch:
		return res.result, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Conn) write(ctx context.Context, m outbound) error {
	job, err := c.prepareWrite(m, ctx)
	if err != nil {
		return err
	}
	if err := c.enqueueWrite(job, true); err != nil {
		return err
	}
	select {
	case err := <-job.res:
		return err
	case <-job.ctx.Done():

		return job.ctx.Err()
	case <-c.closed:

		select {
		case err := <-job.res:
			return err
		default:
		}
		return c.terminalError()
	}
}

// enqueueWrite reserves bounded queue capacity before entering writeGate.
// The reservation makes the send non-blocking while the gate is held, so
// shutdown can close writeQ without racing a producer or waiting behind a
// producer blocked on a full queue. blocking is false for TryNotify.
func (c *Conn) enqueueWrite(job writeJob, blocking bool) error {
	c.ensureWriter()
	if blocking {
		select {
		case c.writeSlots <- struct{}{}:
		case <-job.ctx.Done():
			return job.ctx.Err()
		case <-c.closed:
			return c.terminalError()
		}
	} else {
		select {
		case c.writeSlots <- struct{}{}:
		default:
			return &OutboundQueueFullError{Limit: cap(c.tryNotifySlots)}
		}
	}

	c.writeGate.Lock()
	defer c.writeGate.Unlock()
	if c.writeClosed {
		<-c.writeSlots
		return c.terminalError()
	}

	c.writeQ <- job
	return nil
}

func (c *Conn) prepareWrite(m outbound, ctx context.Context) (writeJob, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return writeJob{}, err
	}
	if limit := c.opts.MaxOutboundBytes; limit > 0 && buf.Len() > limit {
		return writeJob{}, &FrameTooLargeError{Direction: "outbound", Size: buf.Len(), Limit: limit}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return writeJob{frame: buf.Bytes(), ctx: ctx, res: make(chan error, 1)}, nil
}

// writeQueueLimit bounds queued outbound frames per connection. A wedged
// peer fills the queue and then the stall watchdog fails the connection;
// senders never block unboundedly behind it.
const writeQueueLimit = 256

// bestEffortNotifyQueueLimit prevents observation events from filling the
// shared writer queue ahead of request/response traffic. Sixteen queued or
// in-flight events absorb healthy bursts while preserving capacity and
// latency for blocking intercept, provider, UI, and shutdown calls.
const bestEffortNotifyQueueLimit = 16

// writeJob is one outbound frame awaiting the single writer goroutine.
type writeJob struct {
	frame   []byte
	ctx     context.Context // pre-write cancellation only
	res     chan error      // buffered 1
	release func()          // releases optional best-effort notification capacity
}

func completeWriteJob(job writeJob, err error) {
	job.res <- err
	if job.release != nil {
		job.release()
	}
}

// writerLoop is the ONLY writer of c.w. It drains the queue in order, skips
// frames whose caller already gave up before the physical write began, and
// finishes any frame it started — frames are atomic and ordered by
// construction. On connection close it fails everything still queued.
func (c *Conn) writerLoop() {
	defer close(c.writerDone)
	for job := range c.writeQ {
		<-c.writeSlots
		c.writeGate.Lock()
		closed := c.writeClosed
		c.writeGate.Unlock()
		if closed {
			completeWriteJob(job, c.terminalError())
			continue
		}
		if job.ctx != nil {
			if err := job.ctx.Err(); err != nil {
				completeWriteJob(job, err)
				continue
			}
		}
		err := c.writeAll(job.frame)
		if err != nil {

			select {
			case <-c.closed:
				if terminal := c.terminalError(); terminal != nil {
					err = terminal
				}
			default:
			}
		}
		completeWriteJob(job, err)
		if err != nil {

			go c.fail(err)
			return
		}
	}
}

// writeAll serially writes one frame, marking activity for the stall
// watchdog before every blocking Write. It only returns on completion or a
// transport error — caller cancellation never tears a frame in half.
func (c *Conn) writeAll(b []byte) error {
	for len(b) > 0 {
		c.writeProgress.Store(time.Now().UnixNano())
		c.writeActive.Store(true)
		n, err := c.w.Write(b)
		c.writeActive.Store(false)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// stallWatchdog fails the connection when a physical write makes no progress
// for MaxWriteStall: the peer is alive enough to hold the pipe open but has
// stopped reading, and without a bound every later frame would queue behind
// it forever. The watchdog is deliberately independent of any caller
// context, so a short per-call timeout cannot preempt it.
func (c *Conn) stallWatchdog() {
	interval := c.opts.MaxWriteStall / 2
	if interval <= 0 {
		interval = time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !c.writeActive.Load() {
				continue
			}
			last := time.Unix(0, c.writeProgress.Load())
			if time.Since(last) > c.opts.MaxWriteStall {
				c.fail(&WriteStallError{Direction: "outbound", Stall: c.opts.MaxWriteStall})
				return
			}
		case <-c.closed:
			return
		}
	}
}

func (c *Conn) writeError(id json.RawMessage, code int, message string, data any) error {
	var raw json.RawMessage
	if data != nil {
		encoded, err := json.Marshal(data)
		if err != nil {
			code = ErrInternal
			message = "marshal error data: " + err.Error()
		} else if string(encoded) != "null" {
			raw = encoded
		}
	}
	return c.write(context.Background(), outbound{JSONRPC: "2.0", ID: id, Error: &ErrorObject{Code: code, Message: message, Data: raw}})
}

func (c *Conn) respondError(id json.RawMessage, code int, message string, data any) {
	err := c.writeError(id, code, message, data)
	var tooLarge *FrameTooLargeError
	if errors.As(err, &tooLarge) && (data != nil || code != ErrInternal || message != "error response exceeds frame size limit") {
		err = c.writeError(id, ErrInternal, "error response exceeds frame size limit", nil)
	}
	if err != nil {
		c.fail(err)
	}
}

func (c *Conn) fail(err error) {
	if err == nil {
		return
	}
	c.shutdown(err)
	if closer, ok := c.r.(io.Closer); ok {
		_ = closer.Close()
	}
}

// closeReason returns the stored terminal error as-is (nil on a clean EOF);
// Serve uses it so a graceful end still reports nil.
func (c *Conn) closeReason() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.closeErr
}

// terminalError is the error every caller observes after the connection
// ends. It is never nil — a graceful EOF must not report silently dropped
// writes as successes.
func (c *Conn) terminalError() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closeErr != nil {
		return c.closeErr
	}
	return fmt.Errorf("%s: connection closed", c.opts.Name)
}

// writerExitWaitBound caps how long shutdown lets an in-flight physical write
// finish. A wedged writer is failed by the stall watchdog; teardown itself must
// still be bounded.
const writerExitWaitBound = 100 * time.Millisecond

func (c *Conn) shutdown(err error) {
	c.closeOnce.Do(func() {
		c.closeMu.Lock()
		c.closeErr = err
		c.closeMu.Unlock()

		c.ensureWriter()
		c.writeGate.Lock()
		c.writeClosed = true
		close(c.writeQ)
		c.writeGate.Unlock()
		select {
		case <-c.writerDone:
		case <-time.After(writerExitWaitBound):
		}
		close(c.closed)
		c.pmu.Lock()
		for id, ch := range c.pending {
			closedErr := err
			if closedErr == nil {
				closedErr = fmt.Errorf("%s: connection closed", c.opts.Name)
			}
			ch <- rpcResult{err: closedErr}
			delete(c.pending, id)
		}
		c.pmu.Unlock()
	})
}
