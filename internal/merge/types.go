// Package merge is the typed client for smartclass-merge, the service that
// turns a recording's segments into one video. It mirrors the two routes
// dispatchub consumes — submit a merge job and read one back — plus the
// /readyz probe, and decodes every upstream failure into *UpstreamError, the
// only error shape callers inspect.
package merge

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Job status values reported by smartclass-merge.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobCanceled  = "canceled"
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

// Dest is where a merge job writes its output. The bucket is filled by the job
// view (merge's MERGE_DEST_BUCKET); a create request never sends one.
type Dest struct {
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

// Job is one merge job as smartclass-merge reports it on create and on every
// poll. Status is one of the Job* constants; Error carries the upstream
// failure message once Status is JobFailed.
type Job struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"`
	Stage       string     `json:"stage"`
	Error       string     `json:"error"`
	Dest        Dest       `json:"dest"`
	Attempts    int        `json:"attempts"`
	MaxAttempts int        `json:"max_attempts"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// CreateRequest submits one recording for merging. FPS is sent whenever
// webcam-server reported one (0 omits it) and Codec to pin the encoder ("" lets
// merge sniff it). DestKey is always sent; the destination bucket is merge's
// own default. IdempotencyKey makes a repeated POST replay the original job.
type CreateRequest struct {
	DeviceID       string
	StreamID       string
	FPS            int    // 0 → omit
	Codec          string // "" → omit (merge sniffs)
	DestKey        string // always sent; DestBucket stays empty (merge's default bucket)
	IdempotencyKey string
}

// UpstreamError is a non-2xx response from smartclass-merge. Status is the HTTP
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
		return fmt.Sprintf("merge: upstream status %d: %s", e.Status, e.Detail)
	}
	if e.Body != "" {
		return fmt.Sprintf("merge: upstream status %d: %s", e.Status, e.Body)
	}
	return fmt.Sprintf("merge: upstream status %d", e.Status)
}

// IsStatus reports whether err carries a smartclass-merge response with the
// given HTTP status. It unwraps through caller-defined error types, so a
// scheduler error wrapping an *UpstreamError still answers.
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
