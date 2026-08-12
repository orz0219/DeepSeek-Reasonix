package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// downloadAttempts caps how many times a transient transport failure (connection
// reset, read timeout, gateway 5xx) is retried before the update gives up. CN IPv6
// routes to Cloudflare reset mid-transfer often enough that a retry or two usually
// completes the download instead of surfacing a "forcibly closed" error.
const downloadAttempts = 3

// retryBackoff is the pause before the Nth retry; a package var so tests shrink it.
var retryBackoff = func(attempt int) time.Duration { return time.Duration(attempt) * 500 * time.Millisecond }

// retryTransient runs attempt 1..downloadAttempts of fetch, pausing between tries,
// until one succeeds. fetch receives the 1-based attempt number so a caller can
// switch transports on a retry. It stops early when ctx is cancelled (window closed
// / user cancelled). Only the transport is retried; the signature and sha256 checks
// run downstream in downloadVerify and are not retried.
func retryTransient(ctx context.Context, fetch func(attempt int) error) error {
	var err error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		if err = fetch(attempt); err == nil {
			return nil
		}
		if !isTransientFetchError(err) {
			break
		}
		if ctx.Err() != nil || attempt == downloadAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryBackoff(attempt)):
		}
	}
	return err
}

type httpStatusError struct {
	url    string
	status string
	code   int
}

func (e *httpStatusError) Error() string { return fmt.Sprintf("GET %s: %s", e.url, e.status) }

func isTransientFetchError(err error) bool {
	if errors.Is(err, errUpdateResponseTooLarge) {
		return false
	}
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) {
		return true
	}
	return statusErr.code == http.StatusRequestTimeout || statusErr.code == http.StatusTooManyRequests || statusErr.code >= 500
}

// fetchBytes GETs a URL fully into memory, retrying transient transport failures.
func fetchBytes(ctx context.Context, c *http.Client, url string) ([]byte, error) {
	return fetchBytesFallbackForChannel(ctx, c, nil, runningUpdateChannel(), url)
}

// fetchBytesFallback retries transport failures with the IPv4-pinned client.
// This covers small manifest/signature requests as well as the artifact body;
// previously only the large artifact download escaped a broken IPv6 route.
func fetchBytesFallback(ctx context.Context, c, fallback *http.Client, url string) ([]byte, error) {
	return fetchBytesFallbackForChannel(ctx, c, fallback, runningUpdateChannel(), url)
}

func fetchBytesFallbackForChannel(ctx context.Context, c, fallback *http.Client, selected, url string) ([]byte, error) {
	return fetchBytesFallbackForChannelSized(ctx, c, fallback, selected, url, maxDesktopManifestSize)
}

func fetchBytesFallbackForChannelSized(
	ctx context.Context,
	c, fallback *http.Client,
	selected, url string,
	maxBytes int64,
) ([]byte, error) {
	selected = normalizeUpdateChannel(selected)
	var data []byte
	err := retryTransient(ctx, func(attempt int) error {
		client := c
		if attempt > 1 && fallback != nil {
			client = fallback
		}
		var e error
		attemptCtx, cancel := context.WithTimeout(ctx, fetchAttemptTimeout)
		data, e = fetchBytesOnce(attemptCtx, client, selected, url, maxBytes)
		cancel()
		return e
	})
	return data, err
}

var errUpdateResponseTooLarge = errors.New("update: response exceeds allowed size")

func fetchBytesOnce(ctx context.Context, c *http.Client, selected, url string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("update: invalid response size limit %d", maxBytes)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", updaterUserAgent(selected))
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &httpStatusError{url: url, status: resp.Status, code: resp.StatusCode}
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("%w: GET %s declared %d bytes, maximum is %d", errUpdateResponseTooLarge, url, resp.ContentLength, maxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: GET %s exceeded %d bytes", errUpdateResponseTooLarge, url, maxBytes)
	}
	return data, nil
}

// download fetches url into memory, invoking onProgress as bytes arrive. A transient
// transport failure is retried; the retry resumes from the bytes already received
// via a Range request instead of restarting, and switches to the IPv4 fallback
// client (when provided) since a reset usually means the IPv6 route is the problem.
// total is the expected size for the progress denominator (refined from the response).
func download(ctx context.Context, c, fallback *http.Client, url string, total int64, onProgress func(received, total int64)) ([]byte, error) {
	return downloadForChannel(ctx, c, fallback, runningUpdateChannel(), url, total, onProgress)
}

func downloadForChannel(ctx context.Context, c, fallback *http.Client, selected, url string, total int64, onProgress func(received, total int64)) ([]byte, error) {
	selected = normalizeUpdateChannel(selected)
	if total < 0 || total > maxDesktopReleaseAssetSize {
		return nil, fmt.Errorf("update: invalid expected asset size %d", total)
	}
	expectedSize := total
	var buf bytes.Buffer
	err := retryTransient(ctx, func(attempt int) error {
		client := c
		if attempt > 1 && fallback != nil {
			client = fallback
		}
		return downloadInto(ctx, client, selected, url, expectedSize, &buf, &total, onProgress)
	})
	if err != nil {
		return nil, err
	}
	if expectedSize > 0 && int64(buf.Len()) != expectedSize {
		return nil, fmt.Errorf("update: downloaded size mismatch: got %d want %d", buf.Len(), expectedSize)
	}
	return buf.Bytes(), nil
}

// downloadInto appends url's body to buf, resuming from buf's current length via a
// Range request so a retry continues the partial download. A 206 carries the
// remaining bytes; a 200 means the server ignored Range, so buf is reset and the
// whole file re-downloaded. total is refined from the response for the progress
// denominator (Content-Length on 200, the size field of Content-Range on 206).
func downloadInto(ctx context.Context, c *http.Client, selected, url string, expectedSize int64, buf *bytes.Buffer, total *int64, onProgress func(received, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", updaterUserAgent(selected))
	if buf.Len() > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", buf.Len()))
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		buf.Reset()
		if resp.ContentLength > 0 {
			if resp.ContentLength > maxDesktopReleaseAssetSize {
				return fmt.Errorf("update: response size %d exceeds maximum %d", resp.ContentLength, maxDesktopReleaseAssetSize)
			}
			*total = resp.ContentLength
		}
	case http.StatusPartialContent:
		if t := totalFromContentRange(resp.Header.Get("Content-Range")); t > 0 {
			if t > maxDesktopReleaseAssetSize {
				return fmt.Errorf("update: response size %d exceeds maximum %d", t, maxDesktopReleaseAssetSize)
			}
			*total = t
		}
	default:
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	have := int64(buf.Len())
	if expectedSize > 0 && have > expectedSize {
		return fmt.Errorf("update: downloaded size exceeds manifest: got at least %d want %d", have, expectedSize)
	}
	limit := maxDesktopReleaseAssetSize - have + 1
	if expectedSize > 0 {
		limit = expectedSize - have + 1
	}
	body := io.LimitReader(resp.Body, limit)
	pr := &progressReader{r: body, received: have, lastEmit: have, total: *total, onProgress: onProgress}
	_, err = io.Copy(buf, pr)
	if err == nil && expectedSize > 0 && int64(buf.Len()) > expectedSize {
		return fmt.Errorf("update: downloaded size exceeds manifest: got at least %d want %d", buf.Len(), expectedSize)
	}
	if err == nil && int64(buf.Len()) > maxDesktopReleaseAssetSize {
		return fmt.Errorf("update: downloaded size exceeds maximum %d", maxDesktopReleaseAssetSize)
	}
	return err
}

// totalFromContentRange parses the total size out of a "bytes 200-999/1000" header,
// returning 0 when it's absent or "*" (unknown).
func totalFromContentRange(v string) int64 {
	i := strings.LastIndex(v, "/")
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v[i+1:]), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// progressReader reports cumulative bytes read, throttled so the event channel
// isn't flooded.
type progressReader struct {
	r          io.Reader
	received   int64
	total      int64
	lastEmit   int64
	onProgress func(received, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.received += int64(n)

	if p.onProgress != nil && (p.received-p.lastEmit >= 256<<10 || err == io.EOF) {
		p.lastEmit = p.received
		p.onProgress(p.received, p.total)
	}
	return n, err
}

// checkSHA256 verifies data's digest matches the lowercase-hex want.
func checkSHA256(data []byte, want string) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, want) {
		return fmt.Errorf("update: sha256 mismatch: got %s want %s", got, want)
	}
	return nil
}

// extractBinary pulls a single named regular file out of a .tar.gz blob.
func extractBinary(targz []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(targz))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && (h.Name == name || strings.HasSuffix(h.Name, "/"+name)) {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("update: %q not found in archive", name)
}
