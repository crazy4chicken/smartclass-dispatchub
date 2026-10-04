package webcam

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxResponseBytes caps every response body read from webcam-server; it matches
// the upstream request cap.
const maxResponseBytes = 1 << 20

// maxErrorBodyBytes caps how much of a failed response is kept on
// UpstreamError.Body; upstream problems are small, the cap only guards against
// a proxy answering with an HTML page.
const maxErrorBodyBytes = 4 << 10

// Client is a typed client for one webcam-server base URL. It is safe for
// concurrent use.
type Client struct {
	baseURL string
	http    *http.Client
	tokens  TokenSource
	logger  *slog.Logger
}

// New builds a client for baseURL. timeout bounds one HTTP exchange
// (DISPATCH_WEBCAM_TIMEOUT) and the shared transport keeps connections alive.
// The token source supplies the Bearer credential for every call; a nil source
// rejects every request.
func New(baseURL string, tokens TokenSource, timeout time.Duration, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	if tokens == nil {
		tokens = TokenSourceFunc(func(context.Context) (string, error) {
			return "", errors.New("webcam: no token source configured")
		})
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
		tokens:  tokens,
		logger:  logger,
	}
}

// listResponse is the envelope of every webcam-server collection endpoint.
type listResponse[T any] struct {
	Items []T `json:"items"`
}

// cameraEnumRequest is the body of every device command.
type cameraEnumRequest struct {
	CameraEnum int `json:"camera_enum"`
}

// Device returns one device with the state of its live registration.
func (c *Client) Device(ctx context.Context, deviceID string) (Device, error) {
	var out Device
	err := c.call(ctx, apiRequest{
		action: "device",
		method: http.MethodGet,
		path:   "/api/devices/" + url.PathEscape(deviceID) + "/",
		out:    &out,
		policy: policyGET,
	})
	return out, err
}

// Start starts a recording and returns the created stream. A 409 means either
// the device is offline or the camera is already streaming.
func (c *Client) Start(ctx context.Context, deviceID string, cameraEnum int) (Stream, error) {
	var out Stream
	err := c.call(ctx, apiRequest{
		action: "start",
		method: http.MethodPost,
		path:   "/api/devices/" + url.PathEscape(deviceID) + "/recording/start",
		body:   cameraEnumRequest{CameraEnum: cameraEnum},
		out:    &out,
		policy: policyStart,
	})
	if err != nil {
		return Stream{}, err
	}
	if out.ID == "" {
		return Stream{}, errors.New("webcam: recording start answered without a stream id")
	}
	return out, nil
}

// Stop stops the camera's active recording and returns the finished stream. A
// 404 means the camera has no active stream.
func (c *Client) Stop(ctx context.Context, deviceID string, cameraEnum int) (Stream, error) {
	var out Stream
	err := c.call(ctx, apiRequest{
		action: "stop",
		method: http.MethodPost,
		path:   "/api/devices/" + url.PathEscape(deviceID) + "/recording/stop",
		body:   cameraEnumRequest{CameraEnum: cameraEnum},
		out:    &out,
		policy: policyStop,
	})
	if err != nil {
		return Stream{}, err
	}
	return out, nil
}

// Switch queues a camera switch and returns the queued command.
func (c *Client) Switch(ctx context.Context, deviceID string, cameraEnum int) (CommandAck, error) {
	var out CommandAck
	err := c.call(ctx, apiRequest{
		action: "switch",
		method: http.MethodPost,
		path:   "/api/devices/" + url.PathEscape(deviceID) + "/camera/switch",
		body:   cameraEnumRequest{CameraEnum: cameraEnum},
		out:    &out,
		policy: policySingle,
	})
	return out, err
}

// TakePhoto queues a snapshot and returns the queued command with the request
// id the resulting photo will carry.
func (c *Client) TakePhoto(ctx context.Context, deviceID string, cameraEnum int) (CommandAck, error) {
	var out CommandAck
	err := c.call(ctx, apiRequest{
		action: "photo",
		method: http.MethodPost,
		path:   "/api/devices/" + url.PathEscape(deviceID) + "/photo",
		body:   cameraEnumRequest{CameraEnum: cameraEnum},
		out:    &out,
		policy: policySingle,
	})
	return out, err
}

// Streams lists a device's streams, newest first. A limit of zero or less
// applies the upstream default.
func (c *Client) Streams(ctx context.Context, deviceID string, limit int) ([]Stream, error) {
	var out listResponse[Stream]
	err := c.call(ctx, apiRequest{
		action: "streams",
		method: http.MethodGet,
		path:   "/api/devices/" + url.PathEscape(deviceID) + "/streams",
		query:  limitQuery(limit),
		out:    &out,
		policy: policyGET,
	})
	return out.Items, err
}

// StreamDetail returns a stream together with its segments.
func (c *Client) StreamDetail(ctx context.Context, streamID string) (StreamDetail, error) {
	var out StreamDetail
	err := c.call(ctx, apiRequest{
		action: "stream",
		method: http.MethodGet,
		path:   "/api/streams/" + url.PathEscape(streamID) + "/",
		out:    &out,
		policy: policyGET,
	})
	return out, err
}

// Photos lists a device's photos, newest first. A limit of zero or less
// applies the upstream default.
func (c *Client) Photos(ctx context.Context, deviceID string, limit int) ([]Photo, error) {
	var out listResponse[Photo]
	err := c.call(ctx, apiRequest{
		action: "photos",
		method: http.MethodGet,
		path:   "/api/devices/" + url.PathEscape(deviceID) + "/photos",
		query:  limitQuery(limit),
		out:    &out,
		policy: policyGET,
	})
	return out.Items, err
}

// Photo returns one photo with a freshly minted download URL.
func (c *Client) Photo(ctx context.Context, photoID string) (Photo, error) {
	var out Photo
	err := c.call(ctx, apiRequest{
		action: "photo",
		method: http.MethodGet,
		path:   "/api/photos/" + url.PathEscape(photoID) + "/",
		out:    &out,
		policy: policyGET,
	})
	return out, err
}

// Ready probes GET /readyz. It never retries: a health probe must fail fast,
// the caller repeats it.
func (c *Client) Ready(ctx context.Context) error {
	return c.call(ctx, apiRequest{
		action: "ready",
		method: http.MethodGet,
		path:   "/readyz",
		policy: policySingle,
	})
}

// limitQuery renders the optional limit query parameter.
func limitQuery(limit int) url.Values {
	if limit <= 0 {
		return nil
	}
	return url.Values{"limit": []string{strconv.Itoa(limit)}}
}

// apiRequest describes one upstream call.
type apiRequest struct {
	action string // log label and retry policy selector
	method string
	path   string
	query  url.Values
	body   any
	out    any
	policy retryPolicy
}

// call performs an upstream call under its retry policy. A 401 response
// triggers exactly one forced token refresh and one retry, never more.
func (c *Client) call(ctx context.Context, req apiRequest) error {
	var body []byte
	if req.body != nil {
		encoded, err := json.Marshal(req.body)
		if err != nil {
			return fmt.Errorf("webcam: encode %s request: %w", req.action, err)
		}
		body = encoded
	}

	var lastErr error
	for attempt := 1; attempt <= req.policy.attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return lastErr
			}
			return err
		}
		started := time.Now()
		status, err := c.attempt(ctx, req, body)
		elapsed := time.Since(started)
		if err == nil {
			c.logger.Debug("webcam call",
				"action", req.action,
				"http_status", status,
				"duration_ms", elapsed.Milliseconds(),
				"attempt", attempt,
			)
			return nil
		}
		lastErr = err
		if attempt >= req.policy.attempts || !shouldRetry(ctx, err, req.policy) {
			attrs := []any{
				"action", req.action,
				"http_status", status,
				"duration_ms", elapsed.Milliseconds(),
				"attempt", attempt,
				"upstream_detail", DetailOf(err),
			}
			if status == 0 {
				attrs = append(attrs, "error", err)
			}
			c.logger.Warn("webcam call failed", attrs...)
			return err
		}
		c.logger.Debug("webcam call failed, retrying",
			"action", req.action,
			"http_status", status,
			"attempt", attempt,
			"upstream_detail", DetailOf(err),
		)
		if err := sleepCtx(ctx, retryDelay); err != nil {
			return lastErr
		}
	}
	return lastErr
}

// attempt performs one request/response exchange, including the single forced
// token refresh that follows a 401. It reports the HTTP status, or 0 when the
// exchange failed before a response was read.
func (c *Client) attempt(ctx context.Context, req apiRequest, body []byte) (int, error) {
	token, err := c.obtainToken(ctx, false)
	if err != nil {
		return 0, fmt.Errorf("webcam: %s: obtain service token: %w", req.action, err)
	}
	status, payload, err := c.exchange(ctx, req, body, token)
	if err != nil {
		return 0, err
	}
	if status == http.StatusUnauthorized {
		token, err = c.obtainToken(ctx, true)
		if err != nil {
			return status, fmt.Errorf("webcam: %s: refresh service token: %w", req.action, err)
		}
		status, payload, err = c.exchange(ctx, req, body, token)
		if err != nil {
			return 0, err
		}
	}
	return status, c.finish(req, status, payload)
}

// obtainToken mints the Bearer credential, forcing a refresh when the previous
// one was rejected and the source supports it.
func (c *Client) obtainToken(ctx context.Context, force bool) (string, error) {
	if force {
		if refresher, ok := c.tokens.(TokenRefresher); ok {
			return refresher.Refresh(ctx)
		}
	}
	return c.tokens.Token(ctx)
}

// exchange performs one HTTP round trip and reads the capped response body.
func (c *Client) exchange(ctx context.Context, req apiRequest, body []byte, token string) (int, []byte, error) {
	target := c.baseURL + req.path
	if len(req.query) > 0 {
		target += "?" + req.query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, target, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("webcam: %s: build request: %w", req.action, err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Accept", "application/json")
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return 0, nil, &transportError{err: fmt.Errorf("webcam: %s: %w", req.action, err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
		_ = resp.Body.Close()
	}()

	payload, err := readCapped(resp.Body)
	if err != nil {
		if errors.Is(err, errResponseTooLarge) {
			return 0, nil, fmt.Errorf("webcam: %s: %w", req.action, err)
		}
		return 0, nil, &transportError{err: fmt.Errorf("webcam: %s: read response: %w", req.action, err)}
	}
	return resp.StatusCode, payload, nil
}

// errResponseTooLarge marks a body that exceeded the 1 MiB response cap.
var errResponseTooLarge = errors.New("response exceeds the 1 MiB cap")

// readCapped reads at most maxResponseBytes+1 bytes so an over-sized body is
// detected instead of silently truncated.
func readCapped(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBytes {
		return nil, errResponseTooLarge
	}
	return data, nil
}

// finish decodes a 2xx body into the call's output or converts a non-2xx
// response into an *UpstreamError.
func (c *Client) finish(req apiRequest, status int, payload []byte) error {
	if status >= 200 && status < 300 {
		if req.out == nil || len(payload) == 0 {
			return nil
		}
		if err := json.Unmarshal(payload, req.out); err != nil {
			return fmt.Errorf("webcam: %s: decode response: %w", req.action, err)
		}
		return nil
	}
	return newUpstreamError(status, payload)
}

// newUpstreamError builds the typed error for a non-2xx response, decoding the
// RFC 9457 detail when the body is a problem document.
func newUpstreamError(status int, payload []byte) *UpstreamError {
	upstream := &UpstreamError{Status: status}
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(payload, &problem); err == nil {
		upstream.Detail = problem.Detail
	}
	body := strings.TrimSpace(string(payload))
	if len(body) > maxErrorBodyBytes {
		body = body[:maxErrorBodyBytes] + "…"
	}
	upstream.Body = body
	if upstream.Detail == "" {
		upstream.Detail = http.StatusText(status)
	}
	return upstream
}
