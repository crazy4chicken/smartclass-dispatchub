package merge

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// retryDelay is the pause before a repeated attempt, so a flapping upstream is
// not hit twice in the same instant.
const retryDelay = 200 * time.Millisecond

// retryPolicy bounds the attempts of one upstream call. Both calls dispatchub
// makes are safe to repeat: Create carries an X-Idempotency-Key so a replay
// returns the original job, Get is a read. They therefore repeat a transport
// failure and any 5xx — which includes the 503 source_unavailable that asks the
// caller to retry once the segments are listable. A 4xx is a verdict, not a
// hiccup, and is never repeated. The Ready probe never repeats at all.
type retryPolicy struct {
	attempts int // total attempts, the first included
	retry5xx bool
}

var (
	policyCreate = retryPolicy{attempts: 3, retry5xx: true} // two retries
	policyGET    = retryPolicy{attempts: 3, retry5xx: true} // two retries
	policySingle = retryPolicy{attempts: 1}
)

// transportError marks a failure before a response was received, or while
// reading one; only these, and a 5xx under a retry5xx policy, are repeated.
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
		return policy.retry5xx && upstream.Status >= http.StatusInternalServerError
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
