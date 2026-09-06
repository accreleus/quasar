package db

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// PreflightKind identifies why Preflight could not confirm DATABASE_URL is
// usable. It exists so callers (and tests) can branch on the failure shape
// without parsing the Message string.
type PreflightKind int

const (
	// PreflightParseFailure: DATABASE_URL is not a valid Postgres connection
	// string (e.g. malformed URL, unsupported scheme, unparsable query params).
	PreflightParseFailure PreflightKind = iota + 1
	// PreflightUnreachable: DATABASE_URL parsed, but nothing answered at the
	// named host/port — DNS failure, connection refused, or a dial timeout.
	// Typical cause: Postgres is not up yet, or the host/port is wrong.
	PreflightUnreachable
	// PreflightAuthFailure: Postgres was reached but rejected the
	// username/password in DATABASE_URL (SQLSTATE 28P01 / 28000).
	PreflightAuthFailure
	// PreflightDatabaseMissing: Postgres was reached and authenticated, but
	// the named database does not exist (SQLSTATE 3D000).
	PreflightDatabaseMissing
	// PreflightOther: a connection error occurred that does not match any of
	// the above — reported generically rather than guessed at.
	PreflightOther
)

// PreflightError is returned by Preflight when DATABASE_URL cannot be used to
// reach Postgres. Message is the operator-actionable headline (names
// DATABASE_URL and, where known, host/port/credentials); Cause is the raw
// driver error, kept for debugging but deliberately NOT the headline — see
// #518.
type PreflightError struct {
	Kind    PreflightKind
	Message string
	Cause   error
}

func (e *PreflightError) Error() string {
	// Parse errors can contain the entire credential-bearing DSN. Keep the
	// cause for errors.As/Unwrap, but never format it into startup logs.
	if e.Kind == PreflightParseFailure {
		return e.Message
	}
	return fmt.Sprintf("%s\n  driver error: %v", e.Message, e.Cause)
}

func (e *PreflightError) Unwrap() error { return e.Cause }

// Preflight checks that DATABASE_URL parses and that Postgres is reachable
// and accepts its credentials, BEFORE migrations run (#518). Without this, a
// bad or unreachable DATABASE_URL surfaces to an operator as a migration
// failure wrapping a raw pgx dial error, which points a self-hoster at the
// wrong layer entirely — migrations never got a chance to run.
//
// It opens and immediately closes its own connection rather than reusing the
// pool db.Open builds later: Open runs after this succeeds, so duplicating a
// one-shot connect here is the price of failing fast with the RIGHT message
// instead of failing later with the wrong one.
func Preflight(ctx context.Context, databaseURL string) error {
	connCfg, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return &PreflightError{
			Kind: PreflightParseFailure,
			Message: "DATABASE_URL could not be parsed as a Postgres connection string — " +
				"check the DATABASE_URL value passed to the control-plane service " +
				"(deploy/.env or the compose `control-plane` service's environment); " +
				"it must look like postgres://user:password@host:port/dbname",
			Cause: err,
		}
	}

	conn, err := pgx.ConnectConfig(ctx, connCfg)
	if err != nil {
		return classifyConnectErr(connCfg, err)
	}
	_ = conn.Close(context.Background())
	return nil
}

// classifyConnectErr turns a raw pgx connect error into an actionable
// PreflightError. cfg supplies the host/port/user/database that DATABASE_URL
// resolved to, so the message can point at specifics instead of quoting the
// (possibly credential-bearing) URL back at the operator.
func classifyConnectErr(cfg *pgx.ConnConfig, err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "28P01", "28000": // invalid_password / invalid_authorization_specification
			return &PreflightError{
				Kind: PreflightAuthFailure,
				Message: fmt.Sprintf(
					"Postgres at %s:%d rejected the credentials in DATABASE_URL for user %q — "+
						"check DATABASE_URL against the `postgres` compose service's actual "+
						"POSTGRES_USER/POSTGRES_PASSWORD",
					cfg.Host, cfg.Port, cfg.User),
				Cause: err,
			}
		case "3D000": // invalid_catalog_name (database does not exist)
			return &PreflightError{
				Kind: PreflightDatabaseMissing,
				Message: fmt.Sprintf(
					"database %q named by DATABASE_URL does not exist on the Postgres server at %s:%d — "+
						"check the database name in DATABASE_URL, or that the `postgres` compose "+
						"service finished creating it (POSTGRES_DB)",
					cfg.Database, cfg.Host, cfg.Port),
				Cause: err,
			}
		}
	}

	var dnsErr *net.DNSError
	var opErr *net.OpError
	if errors.As(err, &dnsErr) || errors.As(err, &opErr) || errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return &PreflightError{
			Kind: PreflightUnreachable,
			Message: fmt.Sprintf(
				"could not reach Postgres at %s:%d named by DATABASE_URL — check that the "+
					"`postgres` compose service is running and reachable from the control-plane "+
					"container, and that the host/port in DATABASE_URL are correct",
				cfg.Host, cfg.Port),
			Cause: err,
		}
	}

	return &PreflightError{
		Kind: PreflightOther,
		Message: fmt.Sprintf(
			"could not connect to Postgres at %s:%d named by DATABASE_URL — check DATABASE_URL "+
				"against the `postgres` compose service",
			cfg.Host, cfg.Port),
		Cause: err,
	}
}
