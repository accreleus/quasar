package db

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// testConnCfg returns a parsed *pgx.ConnConfig for classifyConnectErr's
// host/port/user/database formatting, without dialing anything.
func testConnCfg(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	cfg, err := pgx.ParseConfig("postgres://quasar:s3cret@db.example.internal:5432/quasar")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	return cfg
}

func TestPreflight_ParseFailure(t *testing.T) {
	// A URL whose port is not numeric fails pgx's own parse step before any
	// dial is attempted.
	err := Preflight(context.Background(), "postgres://user:must-not-leak@host:not-a-port/db")
	if err == nil {
		t.Fatal("expected a parse error, got nil")
	}
	if strings.Contains(err.Error(), "must-not-leak") {
		t.Fatal("parse error leaked database credentials")
	}
	var pfErr *PreflightError
	if !errors.As(err, &pfErr) {
		t.Fatalf("expected *PreflightError, got %T: %v", err, err)
	}
	if pfErr.Kind != PreflightParseFailure {
		t.Errorf("Kind = %v, want PreflightParseFailure", pfErr.Kind)
	}
	if !strings.Contains(pfErr.Message, "DATABASE_URL") {
		t.Errorf("message does not name DATABASE_URL: %q", pfErr.Message)
	}
	if pfErr.Cause == nil {
		t.Error("Cause is nil; raw driver error must be preserved for debugging")
	}
}

func TestClassifyConnectErr_AuthFailure(t *testing.T) {
	cfg := testConnCfg(t)
	for _, code := range []string{"28P01", "28000"} {
		pgErr := &pgconn.PgError{Code: code, Severity: "FATAL", Message: "password authentication failed"}
		err := classifyConnectErr(cfg, pgErr)
		var pfErr *PreflightError
		if !errors.As(err, &pfErr) {
			t.Fatalf("code %s: expected *PreflightError, got %T", code, err)
		}
		if pfErr.Kind != PreflightAuthFailure {
			t.Errorf("code %s: Kind = %v, want PreflightAuthFailure", code, pfErr.Kind)
		}
		if !strings.Contains(pfErr.Message, "DATABASE_URL") {
			t.Errorf("code %s: message does not name DATABASE_URL: %q", code, pfErr.Message)
		}
		if !strings.Contains(pfErr.Message, "quasar") { // the parsed user
			t.Errorf("code %s: message does not name credentials/user: %q", code, pfErr.Message)
		}
		if !errors.Is(err, pgErr) {
			t.Errorf("code %s: raw pgconn.PgError not reachable via errors.Is/Unwrap", code)
		}
	}
}

func TestClassifyConnectErr_DatabaseMissing(t *testing.T) {
	cfg := testConnCfg(t)
	pgErr := &pgconn.PgError{Code: "3D000", Severity: "FATAL", Message: `database "quasar" does not exist`}
	err := classifyConnectErr(cfg, pgErr)
	var pfErr *PreflightError
	if !errors.As(err, &pfErr) {
		t.Fatalf("expected *PreflightError, got %T", err)
	}
	if pfErr.Kind != PreflightDatabaseMissing {
		t.Errorf("Kind = %v, want PreflightDatabaseMissing", pfErr.Kind)
	}
	if !strings.Contains(pfErr.Message, "DATABASE_URL") {
		t.Errorf("message does not name DATABASE_URL: %q", pfErr.Message)
	}
}

func TestClassifyConnectErr_Unreachable_DNS(t *testing.T) {
	cfg := testConnCfg(t)
	dnsErr := &net.DNSError{Err: "no such host", Name: "db.example.internal", IsNotFound: true}
	err := classifyConnectErr(cfg, dnsErr)
	var pfErr *PreflightError
	if !errors.As(err, &pfErr) {
		t.Fatalf("expected *PreflightError, got %T", err)
	}
	if pfErr.Kind != PreflightUnreachable {
		t.Errorf("Kind = %v, want PreflightUnreachable", pfErr.Kind)
	}
	if !strings.Contains(pfErr.Message, "DATABASE_URL") {
		t.Errorf("message does not name DATABASE_URL: %q", pfErr.Message)
	}
}

func TestClassifyConnectErr_Unreachable_ConnRefused(t *testing.T) {
	cfg := testConnCfg(t)
	opErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	err := classifyConnectErr(cfg, opErr)
	var pfErr *PreflightError
	if !errors.As(err, &pfErr) {
		t.Fatalf("expected *PreflightError, got %T", err)
	}
	if pfErr.Kind != PreflightUnreachable {
		t.Errorf("Kind = %v, want PreflightUnreachable", pfErr.Kind)
	}
}

func TestClassifyConnectErr_Unreachable_Timeout(t *testing.T) {
	cfg := testConnCfg(t)
	err := classifyConnectErr(cfg, context.DeadlineExceeded)
	var pfErr *PreflightError
	if !errors.As(err, &pfErr) {
		t.Fatalf("expected *PreflightError, got %T", err)
	}
	if pfErr.Kind != PreflightUnreachable {
		t.Errorf("Kind = %v, want PreflightUnreachable", pfErr.Kind)
	}
}

func TestClassifyConnectErr_Other(t *testing.T) {
	cfg := testConnCfg(t)
	err := classifyConnectErr(cfg, errors.New("something unexpected"))
	var pfErr *PreflightError
	if !errors.As(err, &pfErr) {
		t.Fatalf("expected *PreflightError, got %T", err)
	}
	if pfErr.Kind != PreflightOther {
		t.Errorf("Kind = %v, want PreflightOther", pfErr.Kind)
	}
}

func TestPreflightError_ErrorStringKeepsRawCauseSecondary(t *testing.T) {
	cause := errors.New("dial tcp 127.0.0.1:5999: connect: connection refused")
	pfErr := &PreflightError{Kind: PreflightUnreachable, Message: "could not reach Postgres at 127.0.0.1:5999 named by DATABASE_URL", Cause: cause}
	s := pfErr.Error()
	if !strings.HasPrefix(s, pfErr.Message) {
		t.Errorf("actionable message is not the headline: %q", s)
	}
	if !strings.Contains(s, cause.Error()) {
		t.Errorf("raw driver error missing from Error(): %q", s)
	}
}

// TestPreflight_DBIntegration_WrongPassword exercises the real classification
// path (Preflight -> pgx.ConnectConfig -> a genuine 28P01 from Postgres)
// against an ephemeral database, per #518's acceptance criterion
// (`DATABASE_URL=postgres://quasar:wrong@127.0.0.1:5999/quasar`). Skipped
// unless TEST_DATABASE_URL is set, matching the rest of the DB-backed suite.
func TestPreflight_DBIntegration_WrongPassword(t *testing.T) {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	cfg, err := pgx.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("ParseConfig(TEST_DATABASE_URL): %v", err)
	}
	cfg.Password = "definitely-the-wrong-password"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, connErr := pgx.ConnectConfig(ctx, cfg)
	if connErr == nil {
		t.Skip("Postgres accepted a wrong password (likely trust auth in this test environment); cannot exercise the auth-failure path")
	}

	err = classifyConnectErr(cfg, connErr)
	var pfErr *PreflightError
	if !errors.As(err, &pfErr) {
		t.Fatalf("expected *PreflightError, got %T: %v", err, err)
	}
	if pfErr.Kind != PreflightAuthFailure {
		t.Fatalf("Kind = %v, want PreflightAuthFailure (raw error: %v)", pfErr.Kind, connErr)
	}
	if !strings.Contains(pfErr.Message, "DATABASE_URL") {
		t.Errorf("message does not name DATABASE_URL: %q", pfErr.Message)
	}
}

// TestPreflight_DBIntegration_Unreachable matches #518's acceptance
// criterion verification command directly: a DSN pointing at a closed local
// port must classify as unreachable, not a parse or migration error.
func TestPreflight_DBIntegration_Unreachable(t *testing.T) {
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := Preflight(ctx, "postgres://quasar:wrong@127.0.0.1:5999/quasar")
	if err == nil {
		t.Fatal("expected an error connecting to a closed port")
	}
	var pfErr *PreflightError
	if !errors.As(err, &pfErr) {
		t.Fatalf("expected *PreflightError, got %T: %v", err, err)
	}
	if pfErr.Kind != PreflightUnreachable {
		t.Fatalf("Kind = %v, want PreflightUnreachable (raw error: %v)", pfErr.Kind, err)
	}
	if strings.Contains(err.Error(), "migrations:") {
		t.Errorf("connection failure must not read as a migration failure: %q", err.Error())
	}
}
