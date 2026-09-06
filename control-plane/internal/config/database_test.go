package config

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestLoadDatabaseParametersPreservePassword(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("QUASAR_DATABASE_HOST", "postgres")
	t.Setenv("QUASAR_DATABASE_PASSWORD", "spaces /?#@:$' and %")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := pgx.ParseConfig(c.DatabaseURL)
	if err != nil {
		t.Fatal("generated database connection is invalid")
	}
	if db.Host != "postgres" || db.User != "quasar" || db.Database != "quasar" || db.Password != "spaces /?#@:$' and %" {
		t.Fatal("database parameters did not round trip")
	}
}

func TestDatabaseURLRemainsAuthoritative(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://existing/database")
	t.Setenv("QUASAR_DATABASE_HOST", "ignored")
	c, err := Load()
	if err != nil || c.DatabaseURL != "postgres://existing/database" {
		t.Fatal("existing DATABASE_URL override changed")
	}
}

func TestDatabaseShellTemplateRefusedWithoutLeakingSecret(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("QUASAR_DATABASE_HOST", "postgres")
	t.Setenv("QUASAR_DATABASE_PASSWORD", "$(openssl rand -hex 24)")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "generated value") || strings.Contains(err.Error(), "rand -hex") {
		t.Fatal("expected actionable, credential-free template error")
	}
}
