package webcam

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// retryDelay is the pause before a repeated attempt, so a flapping upstream is
// not hit twice in the same instant.
const retryDelay = 200 * time.Millisecond

// retryPolicy bounds the attempts of one upstream call. Only calls whose
// outcome is unknown-safe are repeated: idempotent GETs on transport errors,
// and start/stop on transport errors or a 502 (the command never reached the
// device). TakePhoto and Switch never repeat — a duplicate snapshot or camera
// change is an observable side effect.
type retryPolicy struct {
	attempts int  // total attempts, the first included
	retry502 bool // a 502 response is treated like a transport failure
}

var (
	policyGET    = retryPolicy{attempts: 3}                 // two retries
	policyStart  = retryPolicy{attempts: 2, retry502: true} // one retry
	policyStop   = retryPolicy{attempts: 2, retry502: true} // one retry
	policySingle = retryPolicy{attempts: 1}
)

// transportError marks a failure before a response was received, or while
// reading one; only these, and a 502 under a retry502 policy, are repeated.
type transportError struct{ err error }

// Error renders the wrapped transport failure.
func (e *transportError) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped transport failure.
func (e *transportError) Unwrap() error { return e.err }

// shouldRetry reports whether err is unknown-safe to repeat under policy.
func shouldRetry(ctx context.Context, err error, policy retryPolicy) bool {
	if ctx.Err() != nil {
		return false
	}
	var transport *transportError
	if errors.As(err, &transport) {
		return true
	}
	var upstream *UpstreamError
	if errors.As(err, &upstream) {
		return policy.retry502 && upstream.Status == http.StatusBadGateway
	}
	return false
}

// sleepCtx waits for d and reports ctx.Err() when the context ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
