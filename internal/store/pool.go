// Package store owns every PostgreSQL interaction: connection pooling, goose
// migrations and the SQL behind terms, rooms, imports, entries, sessions,
// photos and camera commands.
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dbtx is the subset of pgx shared by *pgxpool.Pool and pgx.Tx. Every typed
// sub-store holds one, so the same SQL works inside and outside a transaction.
type dbtx interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Row is one scannable result row.
type Row = pgx.Row

// Store is the pgx-backed persistence layer. The typed sub-stores are
// assembled by Open and share its pool.
type Store struct {
	Pool *pgxpool.Pool

	Terms    *Terms
	Rooms    *Rooms
	Imports  *Imports
	Entries  *Entries
	Sessions *Sessions
	Photos   *Photos
	Commands *Commands
}

// Open connects to PostgreSQL, verifies the connection and assembles the typed
// sub-stores. The connection reports application_name=smartclass-dispatchub.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "smartclass-dispatchub"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{
		Pool:     pool,
		Terms:    &Terms{db: pool},
		Rooms:    &Rooms{db: pool},
		Imports:  &Imports{db: pool},
		Entries:  &Entries{db: pool},
		Sessions: &Sessions{db: pool},
		Photos:   &Photos{db: pool},
		Commands: &Commands{db: pool},
	}, nil
}

// DB exposes the pool through the shared query interface, for callers that need
// a dbtx rather than the concrete pool.
func (s *Store) DB() dbtx {
	return s.Pool
}

// Ping verifies that the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	return s.Pool.Ping(ctx)
}

// Close releases every pooled connection.
func (s *Store) Close() {
	s.Pool.Close()
}

// Tx runs fn inside one transaction and commits when it returns nil; any error
// rolls the transaction back. Driver errors are classified like every other
// store error.
func (s *Store) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return classify(err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if err := fn(tx); err != nil {
		return classify(err)
	}
	return classify(tx.Commit(ctx))
}
