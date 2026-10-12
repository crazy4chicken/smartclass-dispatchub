// Package filehouse is the typed client for nsc-filehouse, the object-storage
// gateway in front of the recordings bucket. It mirrors the one route
// dispatchub consumes — minting a presigned download URL — plus the /readyz
// probe, and decodes every upstream failure into *UpstreamError, the only error
// shape callers inspect.
package filehouse

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// TokenSource supplies the Bearer credential for every outbound call. The
// production implementation is the iamauth client-credentials source; it
// caches the token and refreshes it before expiry.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// TokenSourceFunc adapts a function to TokenSource.
type TokenSourceFunc func(ctx context.Context) (string, error)

// Token returns f(ctx).
func (f TokenSourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// TokenRefresher is an optional TokenSource extension. A source that caches a
// credential implements it so the client can force a refresh after a 401 and
// retry once; when a source does not implement it, the client still re-requests
// the token but a caching source may hand back the same one.
type TokenRefresher interface {
	Refresh(ctx context.Context) (string, error)
}

// MintRequest asks filehouse for one presigned URL over an object.
// Method is the HTTP method the URL grants, normally "GET". TTL is how long
// the URL stays redeemable: zero omits ttl_seconds and leaves filehouse's own
// default (15m, capped at 24h) in place.
type MintRequest struct {
	Bucket string
	Key    string
	Method string        // "GET"
	TTL    time.Duration // 0 → omit ttl_seconds (filehouse default 15m)
}

// Presigned is one minted URL as filehouse reports it. ExpiresAt is when the
// token stops redeeming; redemption re-checks permissions and is not
// single-use (an expired token answers 410).
type Presigned struct {
	URL       string    `json:"url"`
	Method    string    `json:"method"`
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	ExpiresAt time.Time `json:"expires_at"`
}

// UpstreamError is a non-2xx response from nsc-filehouse. Status is the HTTP
// status, Detail the RFC 9457 detail code (falling back to the HTTP status
// text when the body is not a problem document) and Body a bounded copy of the
// response body for logs.
type UpstreamError struct {
	Status int
	Detail string
	Body   string
}

// Error renders the upstream failure.
func (e *UpstreamError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("filehouse: upstream status %d: %s", e.Status, e.Detail)
	}
	if e.Body != "" {
		return fmt.Sprintf("filehouse: upstream status %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("filehouse: upstream status %d", e.Status)
}

// IsStatus reports whether err carries an nsc-filehouse response with the
// given HTTP status. It unwraps through caller-defined error types, so a
// handler error wrapping an *UpstreamError still answers.
func IsStatus(err error, status int) bool {
	var upstream *UpstreamError
	return errors.As(err, &upstream) && upstream.Status == status
}

// DetailOf returns the RFC 9457 detail of err, or "" when err carries no
// upstream problem.
func DetailOf(err error) string {
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Detail
	}
	return ""
}
