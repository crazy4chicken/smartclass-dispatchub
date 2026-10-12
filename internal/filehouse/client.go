package filehouse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// maxResponseBytes caps every response body read from nsc-filehouse; it
// matches the upstream request cap.
const maxResponseBytes = 1 << 20

// maxErrorBodyBytes caps how much of a failed response is kept on
// UpstreamError.Body; upstream problems are small, the cap only guards against
// a proxy answering with an HTML page.
const maxErrorBodyBytes = 4 << 10

// presignPath mints one presigned URL over an object.
const presignPath = "/api/v1/presign"

// readyPath is the public readiness route; a probe needs no credential.
const readyPath = "/readyz"

// Client is a typed client for one nsc-filehouse base URL. It is safe for
// concurrent use.
type Client struct {
	baseURL string
	http    *http.Client
	tokens  TokenSource
	logger  *slog.Logger
}

// New builds a client for baseURL. timeout bounds one HTTP exchange
// (DISPATCH_FILEHOUSE_TIMEOUT) and the shared transport keeps connections
// alive. The token source supplies the Bearer credential for every call; a nil
// source rejects every request.
func New(baseURL string, tokens TokenSource, timeout time.Duration, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	if tokens == nil {
		tokens = TokenSourceFunc(func(context.Context) (string, error) {
			return "", errors.New("filehouse: no token source configured")
		})
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: timeout},
		tokens:  tokens,
		logger:  logger,
	}
}

// mintRequest is the JSON body of POST /api/v1/presign. TTLSeconds is omitted
// when MintRequest.TTL is unset, leaving filehouse's own default (15m, capped
// at 24h) in place. content_type and max_bytes are never sent: dispatchub only
// ever mints a plain download URL.
type mintRequest struct {
	Bucket     string `json:"bucket"`
	Key        string `json:"key"`
	Method     string `json:"method"`
	TTLSeconds *int64 `json:"ttl_seconds,omitempty"`
}

// Mint asks filehouse for one presigned URL over req. Minting requires both
// filehouse:share:any|team and the grant the URL exercises
// (filehouse:read:any|team for GET), evaluated against the bucket resource.
// A 403 therefore means the service principal is missing one of the two.
func (c *Client) Mint(ctx context.Context, req MintRequest) (Presigned, error) {
	var out Presigned
	body := mintRequest{
		Bucket: req.Bucket,
		Key:    req.Key,
		Method: req.Method,
	}
	// ttl_seconds is whole seconds: a TTL that rounds down to zero is omitted
	// rather than sent as 0, so filehouse applies its own default.
	if ttl := int64(req.TTL / time.Second); ttl > 0 {
		body.TTLSeconds = &ttl
	}
	err := c.call(ctx, apiRequest{
		action: "mint",
		method: http.MethodPost,
		path:   presignPath,
		body:   body,
		out:    &out,
		policy: policyMint,
	})
	return out, err
}

// Ready probes GET /readyz. It never retries: a health probe must fail fast,
// the caller repeats it. /readyz is public, so the probe sends no credential
// and never consults the token source — a probe that needs a working
// credential cannot report whether the service is up.
func (c *Client) Ready(ctx context.Context) error {
	return c.call(ctx, apiRequest{
		action: "ready",
		method: http.MethodGet,
		path:   readyPath,
		public: true,
		policy: policySingle,
	})
}

// apiRequest describes one upstream call.
type apiRequest struct {
	action string // log label and retry policy selector
	method string
	path   string
	body   any
	public bool // no Authorization header, no token acquisition
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
			return fmt.Errorf("filehouse: encode %s request: %w", req.action, err)
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
			c.logger.Debug("filehouse call",
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
			c.logger.Warn("filehouse call failed", attrs...)
			return err
		}
		c.logger.Debug("filehouse call failed, retrying",
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
	token, err := c.credential(ctx, req, false)
	if err != nil {
		return 0, fmt.Errorf("filehouse: %s: obtain service token: %w", req.action, err)
	}
	status, payload, err := c.exchange(ctx, req, body, token)
	if err != nil {
		return 0, err
	}
	if status == http.StatusUnauthorized && !req.public {
		token, err = c.credential(ctx, req, true)
		if err != nil {
			return status, fmt.Errorf("filehouse: %s: refresh service token: %w", req.action, err)
		}
		status, payload, err = c.exchange(ctx, req, body, token)
		if err != nil {
			return 0, err
		}
	}
	return status, c.finish(req, status, payload)
}

// credential mints the Bearer credential for a call, or returns "" when the
// route is public and must carry none.
func (c *Client) credential(ctx context.Context, req apiRequest, force bool) (string, error) {
	if req.public {
		return "", nil
	}
	return c.obtainToken(ctx, force)
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
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.method, target, reader)
	if err != nil {
		return 0, nil, fmt.Errorf("filehouse: %s: build request: %w", req.action, err)
	}
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}
	httpReq.Header.Set("Accept", "application/json")
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return 0, nil, &transportError{err: fmt.Errorf("filehouse: %s: %w", req.action, err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
		_ = resp.Body.Close()
	}()

	payload, err := readCapped(resp.Body)
	if err != nil {
		if errors.Is(err, errResponseTooLarge) {
			return 0, nil, fmt.Errorf("filehouse: %s: %w", req.action, err)
		}
		return 0, nil, &transportError{err: fmt.Errorf("filehouse: %s: read response: %w", req.action, err)}
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
			return fmt.Errorf("filehouse: %s: decode response: %w", req.action, err)
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
