// SPDX-License-Identifier: AGPL-3.0-or-later

// Package postgres implements the store interfaces against Postgres via pgx.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"scm.poniatowski.dev/adam/lexarii/internal/store"
)

// Pool wraps a pgx connection pool.
type Pool struct {
	*pgxpool.Pool
}

// Open connects to Postgres and verifies the connection before returning, so a
// bad DATABASE_URL fails at startup rather than on the first request.
func Open(ctx context.Context, databaseURL string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing DATABASE_URL: %w", err)
	}

	// Self-hosted workloads are bursty and small: a handful of readers plus the
	// worker pool. Large pools cost Postgres memory and buy nothing here.
	cfg.MaxConns = 16
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("creating pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connecting to postgres: %w", err)
	}
	return &Pool{Pool: pool}, nil
}

// Ping satisfies the health checker interface.
func (p *Pool) Ping(ctx context.Context) error {
	if err := p.Pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	return nil
}

// Postgres error codes that carry meaning for callers.
// https://www.postgresql.org/docs/current/errcodes-appendix.html
const (
	codeUniqueViolation     = "23505"
	codeForeignKeyViolation = "23503"
	codeCheckViolation      = "23514"
	codeQueryCanceled       = "57014"
)

// mapError translates pgx and Postgres errors into store sentinels, so nothing
// above this package has to know pgx exists.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case codeUniqueViolation:
			// The common case is re-ingesting a file: book_files.content_hash is
			// UNIQUE precisely so this is a cheap no-op rather than a duplicate.
			return fmt.Errorf("%w: %s", store.ErrConflict, pgErr.ConstraintName)
		case codeForeignKeyViolation:
			return fmt.Errorf("%w: %s", store.ErrNotFound, pgErr.ConstraintName)
		case codeCheckViolation:
			return fmt.Errorf("check constraint %s violated: %w", pgErr.ConstraintName, err)
		}
	}
	return err
}

// VectorDimensions reports the width of the embedding column.
//
// The column is fixed at 768 by the migration that created it, and nothing
// reads LEXARII_EMBED_DIMENSIONS back out of the schema - so pointing this
// at a 1024-wide model passed validation, started cleanly, and then failed
// every single insert with "expected 768 dimensions, not 1024" hours into an
// embedding run. This exists so that becomes a startup message naming the
// variable.
//
// atttypmod carries the dimension for a pgvector column, the same way it
// carries the length for varchar.
func (p *Pool) VectorDimensions(ctx context.Context) (int, error) {
	const q = `
		SELECT atttypmod
		FROM pg_attribute
		WHERE attrelid = 'embeddings'::regclass
		  AND attname = 'vector'`

	var dims int
	if err := p.QueryRow(ctx, q).Scan(&dims); err != nil {
		return 0, mapError(err)
	}
	return dims, nil
}
