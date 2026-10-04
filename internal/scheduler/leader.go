package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// leaderLockKey is the dispatchub advisory-lock key: the ASCII bytes
// "dispatch" as a big-endian int64.
const leaderLockKey int64 = 0x6469737061746368

// Leader election pacing.
const (
	leaderRetryInterval = time.Second
	leaderUnlockTimeout = 5 * time.Second
)

// acquireLeadership takes the dispatchub session-level advisory lock on a
// dedicated pooled connection and blocks until the lock is held. The lock
// makes extra replicas idle instead of splitting the schedule; the returned
// connection must stay checked out for as long as leadership lasts and is
// released by releaseLeadership.
func (s *Scheduler) acquireLeadership(ctx context.Context) (*pgxpool.Conn, error) {
	conn, err := s.store.Pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("scheduler: acquire leadership connection: %w", err)
	}
	for {
		var locked bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leaderLockKey).Scan(&locked); err != nil {
			conn.Release()
			return nil, fmt.Errorf("scheduler: try advisory lock: %w", err)
		}
		if locked {
			return conn, nil
		}
		s.logger.Info("scheduler: waiting for leadership", "retry_in", leaderRetryInterval.String())
		select {
		case <-ctx.Done():
			conn.Release()
			return nil, ctx.Err()
		case <-time.After(leaderRetryInterval):
		}
	}
}

// releaseLeadership drops the advisory lock and returns the connection to the
// pool. It runs on a fresh context because the caller's context is normally
// already cancelled by the time Run returns.
func (s *Scheduler) releaseLeadership(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), leaderUnlockTimeout)
	defer cancel()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, leaderLockKey); err != nil {
		// A session-level lock that cannot be unlocked must not outlive Run on
		// a pooled connection, or the next replica could never become leader.
		s.logger.Warn("scheduler: release advisory lock, closing the connection", "lock_key", leaderLockKey, "error", err)
		hijacked := conn.Hijack()
		_ = hijacked.Close(context.Background())
		return
	}
	conn.Release()
}
