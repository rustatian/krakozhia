// Package store persists documents, memory notes, benchmarks, vectors, and enrichment jobs in PostgreSQL.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Store wraps a pgxpool connection pool with pgvector support.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to PostgreSQL and registers pgvector types on each connection.
func Open(ctx context.Context, dsn string) (*Store, error) {
	return OpenWithSearchPath(ctx, dsn, "")
}

// OpenWithSearchPath connects to PostgreSQL using an explicit search_path when non-empty.
func OpenWithSearchPath(ctx context.Context, dsn, searchPath string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if searchPath != "" {
		if cfg.ConnConfig.RuntimeParams == nil {
			cfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		cfg.ConnConfig.RuntimeParams["search_path"] = searchPath
	}
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		var hasVector bool
		if err := conn.QueryRow(ctx, "SELECT to_regtype('vector') IS NOT NULL").Scan(&hasVector); err != nil {
			return err
		}
		if hasVector {
			return pgxvec.RegisterTypes(ctx, conn)
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases all pooled database connections.
func (s *Store) Close() {
	s.pool.Close()
}

// Exec runs a SQL statement directly on the pool.
func (s *Store) Exec(ctx context.Context, sql string, args ...any) error {
	_, err := s.pool.Exec(ctx, sql, args...)
	return err
}

// Ping verifies database connectivity.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Migrate applies any unapplied embedded SQL migrations in filename order.
func (s *Store) Migrate(ctx context.Context) error {
	var hasVector bool
	if err := s.pool.QueryRow(ctx, "SELECT to_regtype('vector') IS NOT NULL").Scan(&hasVector); err != nil {
		return fmt.Errorf("check vector type: %w", err)
	}
	if !hasVector {
		if _, err := s.pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
			return fmt.Errorf("create vector extension: %w", err)
		}
		s.pool.Reset()
	}

	if _, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)

	for _, name := range names {
		var exists bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", name).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists {
			continue
		}
		sqlBytes, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}
