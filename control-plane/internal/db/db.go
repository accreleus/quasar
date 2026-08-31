package db

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultStatementTimeout / DefaultLockTimeout are the RuntimeParams applied
// when the config layer's QUASAR_DB_STATEMENT_TIMEOUT / QUASAR_DB_LOCK_TIMEOUT
// are unset (#416). Every query issued over the pool inherits these from
// Postgres itself — belt to the many callers (agentws read loops, HTTP
// handlers riding a background context) that do not thread a per-call
// deadline. A slow query or a lock pile-up (raised odds after a FOR UPDATE
// hits a Seq Scan — see #414) now surfaces as a returned error within this
// bound instead of parking the caller, and eventually the whole connection
// pool, forever.
const (
	DefaultStatementTimeout = 30 * time.Second
	DefaultLockTimeout      = 10 * time.Second
)

// Open creates and validates a pgx connection pool using the given URL.
// The context is used for the initial ping; pass one with a deadline to limit
// startup time.
//
// statementTimeout and lockTimeout are applied as Postgres RuntimeParams
// (session-level defaults, in effect for every statement on every connection
// in the pool). Zero means "no override" — pgxpool.ParseConfig already leaves
// the param unset in that case, which is Postgres's own "no timeout" default.
// The config layer never produces zero (its env parse rejects a non-positive
// value and falls back to Default{Statement,Lock}Timeout instead); zero here
// is reachable only from a test that wants no timeout on purpose.
func Open(ctx context.Context, databaseURL string, statementTimeout, lockTimeout time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}

	// Connection pool sizing: conservative defaults suitable for a single-host
	// Phase 1 deploy. Phase 2+ can tune via DATABASE_URL params or promote these
	// to Config fields.
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute

	if statementTimeout > 0 {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(statementTimeout.Milliseconds(), 10)
	}
	if lockTimeout > 0 {
		cfg.ConnConfig.RuntimeParams["lock_timeout"] = strconv.FormatInt(lockTimeout.Milliseconds(), 10)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}
