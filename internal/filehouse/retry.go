package filehouse

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// retryDelay is the pause before a repeated attempt, so a flapping upstream is
// not hit twice in the same instant.
const retryDelay = 200 * time.Millisecond

// retryPolicy bounds the attempts of one upstream call. Presigning is a read
// of the permission graph — it mints a URL and stores nothing — so a transport
// failure and a 5xx are safe to repeat; a 4xx is a verdict (a missing grant
// does not appear on a second look) and is never repeated. The Ready probe
// never repeats at all.
type retryPolicy struct {
	attempts int // total attempts, the first included
	retry5xx bool
}

var (
	policyMint   = retryPolicy{attempts: 2, retry5xx: true} // one retry
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
