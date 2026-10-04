package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/crazy4chicken/smartclass-dispatchub/migrations"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// migrationLockKey is the PostgreSQL advisory lock key ("nsc-disp" in ASCII)
// serialising goose migrations across processes. It is distinct from the
// scheduler's leader-election lock.
const migrationLockKey int64 = 0x6e73632d64697370

// Migrate applies every pending goose migration from the embedded FS behind a
// session-level advisory lock, then logs the resulting schema version. It is
// idempotent: a second call applies nothing and logs "no pending migrations".
// The database itself is never created or dropped.
func (s *Store) Migrate(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
			logger.Warn("release migration lock", "error", err)
		}
	}()

	// Closing the sql.DB does not close the pool it was opened from.
	db := stdlib.OpenDBFromPool(s.Pool)
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithSlog(logger))
	if err != nil {
		return fmt.Errorf("init migrations: %w", err)
	}
	applied, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	version, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	if len(applied) == 0 {
		logger.Info("no pending migrations", "version", version)
		return nil
	}
	versions := make([]int64, 0, len(applied))
	for _, result := range applied {
		versions = append(versions, result.Source.Version)
	}
	logger.Info("migrations applied", "count", len(applied), "versions", versions, "version", version)
	return nil
}
