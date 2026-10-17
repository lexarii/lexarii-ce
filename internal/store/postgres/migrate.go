// SPDX-License-Identifier: AGPL-3.0-or-later

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"scm.poniatowski.dev/adam/lexarii/migrations"
)

// Migrate brings the database up to the schema this binary was built against.
//
// Run at startup, which is the right default for software somebody runs on
// their own machine: the alternative is a second command that must be
// remembered at exactly the moment a person is least equipped to remember it.
// An operator who manages schema separately can turn it off.
//
// Goose takes an advisory lock for the duration, so two instances starting at
// once do not both try to migrate - one waits and finds nothing to do.
func Migrate(ctx context.Context, databaseURL string, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	// database/sql, only here. goose speaks it and pgx provides it, so this
	// costs one connection at startup rather than a second pool for the life
	// of the process.
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("opening the database for migration: %w", err)
	}
	defer func() { _ = db.Close() }()

	// One lock, held while migrating. Without it two containers starting
	// together can both run 00001, and one of them fails on a table the other
	// has just created.
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return fmt.Errorf("preparing the migration lock: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS,
		goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("preparing migrations: %w", err)
	}

	before, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("reading the schema version: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrating: %w", err)
	}

	if len(results) == 0 {
		log.Debug("schema is current", "version", before)
		return nil
	}
	for _, r := range results {
		log.Info("migration applied", "version", r.Source.Version, "name", r.Source.Path)
	}

	after, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("reading the schema version: %w", err)
	}
	log.Info("schema migrated", "from", before, "to", after, "applied", len(results))
	return nil
}
