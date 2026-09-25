// Package migrations embeds the SQL migrations and applies them with
// golang-migrate. The same files feed sqlc (see sqlc.yaml).
package migrations

import (
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers the pgx5:// driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed *.sql
var files embed.FS

// New returns a migrator for the Postgres database at databaseURL
// (postgres:// or postgresql://). Callers must Close it.
func New(databaseURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(files, ".")
	if err != nil {
		return nil, fmt.Errorf("migrations source: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, driverURL(databaseURL))
	if err != nil {
		return nil, fmt.Errorf("migrations init: %w", err)
	}
	return m, nil
}

// Up applies all pending migrations. No pending migrations is not an error.
func Up(databaseURL string) (err error) {
	m, err := New(databaseURL)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeErr(m)) }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

func closeErr(m *migrate.Migrate) error {
	srcErr, dbErr := m.Close()
	return errors.Join(srcErr, dbErr)
}

// driverURL rewrites a libpq-style URL to golang-migrate's pgx5 scheme.
func driverURL(databaseURL string) string {
	for _, p := range []string{"postgresql://", "postgres://"} {
		if rest, ok := strings.CutPrefix(databaseURL, p); ok {
			return "pgx5://" + rest
		}
	}
	return databaseURL
}
