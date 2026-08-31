package migrate

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver for database/sql
)

// Run applies all pending up-migrations from migrationsFS to the database at
// databaseURL. It is idempotent: already-applied migrations are skipped.
func Run(migrationsFS fs.FS, databaseURL string) error {
	src, err := iofs.New(migrationsFS, ".")
	if err != nil {
		return fmt.Errorf("load migrations source: %w", err)
	}

	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open migrations db: %w", err)
	}
	defer sqlDB.Close()

	driver, err := pgxdriver.WithInstance(sqlDB, &pgxdriver.Config{
		MigrationsTable: "schema_migrations",
	})
	if err != nil {
		return fmt.Errorf("create migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "quasar", driver)
	if err != nil {
		return fmt.Errorf("create migrator: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		if rollbackErr := translateRollbackError(err); rollbackErr != nil {
			return rollbackErr
		}
		return fmt.Errorf("run migrations: %w", err)
	}

	return nil
}
