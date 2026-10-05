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

// Schema is the PostgreSQL schema every dispatchub object lives in. The name
// is fixed to the project and is not configurable: the pool pins search_path to
// it, so the service never creates anything in the database's public schema,
// which PostgreSQL 15 and newer reserves behind an explicit grant. A DSN that
// pins search_path itself overrides it — the integration tests use that to run
// against a throwaway schema.
const Schema = "smartclass_dispatchub"

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

	// ownsSchema is true when the DSN left search_path alone, so Schema is this
	// service's to create: Migrate creates it before goose runs. A DSN that
	// pins its own search_path owns that schema instead.
	ownsSchema bool
}

// Open connects to PostgreSQL, verifies the connection and assembles the typed
// sub-stores. The connection reports application_name=smartclass-dispatchub and
// search_path=smartclass_dispatchub.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "smartclass-dispatchub"
	_, pinnedSchema := cfg.ConnConfig.RuntimeParams["search_path"]
	if !pinnedSchema {
		cfg.ConnConfig.RuntimeParams["search_path"] = Schema
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{
		Pool:       pool,
		Terms:      &Terms{db: pool},
		Rooms:      &Rooms{db: pool},
		Imports:    &Imports{db: pool},
		Entries:    &Entries{db: pool},
		Sessions:   &Sessions{db: pool},
		Photos:     &Photos{db: pool},
		Commands:   &Commands{db: pool},
		ownsSchema: !pinnedSchema,
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
