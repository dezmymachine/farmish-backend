package jobs_test

import (
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// riverSchemaVersion is the newest River migration included in our
// golang-migrate files (migrations/000003_river_queue.up.sql: versions 2-7).
const riverSchemaVersion = 7

// If this fails after upgrading River, the new library expects schema we
// haven't applied. Add a migration with only the new versions:
//
//	go run github.com/riverqueue/river/cmd/river@<version> migrate-get --version 8 --up   > migrations/NNNNNN_river_v8.up.sql
//	go run github.com/riverqueue/river/cmd/river@<version> migrate-get --version 8 --down > migrations/NNNNNN_river_v8.down.sql
//
// then bump riverSchemaVersion.
func TestRiverSchemaVersionMatchesMigrations(t *testing.T) {
	m, err := rivermigrate.New[pgx.Tx](riverpgxv5.New(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	all := m.AllVersions()
	if latest := all[len(all)-1].Version; latest != riverSchemaVersion {
		t.Fatalf("River library knows schema version %d, migrations include up to %d: add a migration (see comment)",
			latest, riverSchemaVersion)
	}
}
